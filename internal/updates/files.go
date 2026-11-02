package updates

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"golang.org/x/sys/unix"
)

type lockedRoot struct {
	*os.Root
	lock *os.File
}

func (r *lockedRoot) close() {
	r.lock.Close()
	r.Root.Close()
}

func (m *Manager) openInstall() (*os.Root, error) {
	info, err := os.Lstat(m.opts.InstallDir)
	if err != nil {
		return nil, fmt.Errorf("open panel install directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm()&0022 != 0 {
		return nil, problem(ErrUnavailable, "panel install directory must be a real directory without group/world write access")
	}
	return os.OpenRoot(m.opts.InstallDir)
}

func ensureDir(r *os.Root, name string, mode os.FileMode) error {
	created := r.Mkdir(name, mode)
	if created != nil && !errors.Is(created, os.ErrExist) {
		return created
	}
	info, err := r.Lstat(name)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0022 != 0 || mode == 0700 && info.Mode().Perm()&0077 != 0 {
		return problem(ErrUnavailable, "%s must be a protected real directory", name)
	}
	if created == nil {
		return syncDir(r, path.Dir(name))
	}
	return nil
}

func (m *Manager) lock(ctx context.Context) (*lockedRoot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r, err := m.openInstall()
	if err != nil {
		return nil, err
	}
	for _, dir := range []struct {
		name string
		mode os.FileMode
	}{{"versions", 0755}, {"updates", 0700}, {"updates/staging", 0700}, {"updates/snapshots", 0700}} {
		if err := ensureDir(r, dir.name, dir.mode); err != nil {
			r.Close()
			return nil, fmt.Errorf("prepare updater directories: %w", err)
		}
	}
	f, err := regular(r, "updates/lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		r.Close()
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		r.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, problem(ErrConflict, "another updater operation holds the install lock")
		}
		return nil, fmt.Errorf("lock updater: %w", err)
	}
	return &lockedRoot{Root: r, lock: f}, nil
}

func regular(r *os.Root, name string, flags int, mode os.FileMode) (*os.File, error) {
	f, err := r.OpenFile(name, flags|unix.O_NOFOLLOW|unix.O_NONBLOCK, mode)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%s is not an accessible regular file", name)
	}
	return f, nil
}

func syncDir(r *os.Root, dir string) error {
	f, err := r.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func newID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func atomicWrite(r *os.Root, name string, data []byte, mode os.FileMode) error {
	tmp := name + ".tmp-" + newID()
	f, err := regular(r, tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer r.Remove(tmp)
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := r.Rename(tmp, name); err != nil {
		return err
	}
	return syncDir(r, path.Dir(name))
}

func limitedRead(r *os.Root, name string, limit int64) ([]byte, error) {
	f, err := regular(r, name, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err == nil && int64(len(b)) > limit {
		err = fmt.Errorf("%s exceeds its size limit", name)
	}
	return b, err
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func fileHash(ctx context.Context, r *os.Root, name string) (string, int64, error) {
	f, err := regular(r, name, os.O_RDONLY, 0)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, contextReader{ctx, f})
	return hex.EncodeToString(h.Sum(nil)), n, err
}

func versionDir(v string) string { return "versions/" + v }

func currentVersion(r *os.Root) (string, error) {
	target, err := r.Readlink("current")
	if err != nil {
		return "", fmt.Errorf("read current panel version symlink: %w", err)
	}
	v := strings.TrimPrefix(target, "versions/")
	if _, err := parseVersion(v); err != nil || target != versionDir(v) || len(v) > 120 {
		return "", problem(ErrConflict, "current must link to a relative versions/<SemVer> directory")
	}
	info, err := r.Lstat(target)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0022 != 0 {
		return "", problem(ErrConflict, "current panel version directory is missing or unsafe")
	}
	return v, nil
}

func switchLink(r *os.Root, name, v string) error {
	tmp := "." + name + "-" + newID()
	if err := r.Symlink(versionDir(v), tmp); err != nil {
		return err
	}
	defer r.Remove(tmp)
	if info, err := r.Lstat(name); err == nil && info.Mode()&os.ModeSymlink == 0 {
		return problem(ErrConflict, "%s is not a symlink", name)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := r.Rename(tmp, name); err != nil {
		return err
	}
	return syncDir(r, ".")
}
