//go:build linux

package servicehelper

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// Each ancestor is checked on the descriptor used for the next lookup.
func trustedDir(path string) (*os.File, error) {
	if path != "/" && !cleanPath(path) {
		return nil, errors.New("unsafe privileged directory path")
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "/")
	if err := checkTrusted(f, true); err != nil {
		f.Close()
		return nil, err
	}
	if path == "/" {
		return f, nil
	}
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		next, err := unix.Openat(int(f.Fd()), part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		f.Close()
		if err != nil {
			return nil, errors.New("privileged path contains an unavailable or unsafe directory")
		}
		f = os.NewFile(uintptr(next), part)
		if err := checkTrusted(f, true); err != nil {
			f.Close()
			return nil, err
		}
	}
	return f, nil
}

func checkTrusted(f *os.File, dir bool) error {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return err
	}
	typeBits := uint32(unix.S_IFREG)
	if dir {
		typeBits = unix.S_IFDIR
	}
	if st.Uid != 0 || st.Mode&0022 != 0 || st.Mode&unix.S_IFMT != typeBits {
		return errors.New("privileged paths must be root-owned and not group/world writable")
	}
	return nil
}

func trustedFile(path string) (*os.File, error) {
	if !cleanPath(path) {
		return nil, errors.New("unsafe privileged file path")
	}
	dir, err := trustedDir(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	fd, err := unix.Openat(int(dir.Fd()), filepath.Base(path), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	if err := checkTrusted(f, false); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func readTrusted(path string) ([]byte, error) {
	f, err := trustedFile(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxJSON+1))
	if err == nil && len(b) > maxJSON {
		return nil, errors.New("privileged file exceeds 64 KiB")
	}
	return b, err
}

func checkExecutable(path string) error {
	f, err := trustedFile(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0111 == 0 {
		return errors.New("approved executable is not executable")
	}
	return nil
}

// Existing registrations are never replaced, even when the caller is root.
func writeExclusive(path string, data []byte, mode uint32) error {
	dir, err := trustedDir(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	fd, err := unix.Openat(int(dir.Fd()), filepath.Base(path), unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, mode)
	if errors.Is(err, unix.EEXIST) {
		existing, err := readTrusted(path)
		if err == nil && bytes.Equal(existing, data) {
			return nil
		}
		return errors.New("registration already exists with different contents; stop and review it offline")
	}
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), path)
	ok := false
	defer func() {
		f.Close()
		if !ok {
			_ = unix.Unlinkat(int(dir.Fd()), filepath.Base(path), 0)
		}
	}()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Chmod(os.FileMode(mode)); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		return err
	}
	ok = true
	return nil
}

func lockRegistry() (*os.File, error) {
	dir, err := trustedDir(filepath.Dir(configPath))
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	fd, err := unix.Openat(int(dir.Fd()), ".runtime.lock", unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "runtime lock")
	if err := checkTrusted(f, false); err != nil {
		f.Close()
		return nil, err
	}
	if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func dataDir(path string) (*os.File, error) {
	dir, err := trustedDir(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	fd, err := unix.Openat(int(dir.Fd()), filepath.Base(path), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errors.New("instance root is unavailable, linked, or not a directory")
	}
	return os.NewFile(uintptr(fd), path), nil
}

func identity(f *os.File) (uint64, uint64, error) {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return 0, 0, err
	}
	return uint64(st.Dev), st.Ino, nil
}

func launcherFile(root *os.File, path string) (*os.File, error) {
	fd, err := unix.Dup(int(root.Fd()))
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "launcher root")
	parts := strings.Split(path, "/")
	for n, part := range parts {
		flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
		if n < len(parts)-1 {
			flags |= unix.O_DIRECTORY
		}
		next, err := unix.Openat(int(f.Fd()), part, flags, 0)
		f.Close()
		if err != nil {
			return nil, errors.New("launcher is unavailable or contains a symlink")
		}
		f = os.NewFile(uintptr(next), part)
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, errors.New("launcher must be a regular file")
	}
	return f, nil
}

func ensureManagedRoot(path string, uid, gid uint32) (*os.File, error) {
	dir, err := trustedDir(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	err = unix.Mkdirat(int(dir.Fd()), filepath.Base(path), 0700)
	created := err == nil
	if err != nil && !errors.Is(err, unix.EEXIST) {
		return nil, err
	}
	f, err := dataDir(path)
	if err != nil {
		return nil, err
	}
	if created {
		if err := f.Chown(int(uid), int(gid)); err != nil {
			f.Close()
			return nil, err
		}
		if err := unix.Fchmod(int(f.Fd()), 02770); err != nil {
			f.Close()
			return nil, err
		}
		if err := f.Sync(); err != nil {
			f.Close()
			return nil, err
		}
		if err := dir.Sync(); err != nil {
			f.Close()
			return nil, err
		}
	}
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		f.Close()
		return nil, err
	}
	if st.Uid != uid {
		f.Close()
		return nil, fmt.Errorf("existing managed root requires panel UID %d; no ownership was changed", uid)
	}
	return f, nil
}
