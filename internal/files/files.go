// Package files provides confined file operations. Callers serialize mutations
// and stop the runtime before changing instance data.
package files

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"

	"github.com/gabrielpgava/newempire-server-control/internal/model"
	"golang.org/x/sys/unix"
)

var (
	ErrUnsafePath = errors.New("unsafe relative path")
	ErrUnsafeType = errors.New("links and special files are not allowed")
	ErrTooLarge   = errors.New("file or directory exceeds the configured limit")
	ErrExists     = errors.New("destination already exists")
)

const reserved = ".nesc-"

type Progress func(completedBytes int64)

type Entry struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	Directory  bool   `json:"directory"`
	ModifiedAt int64  `json:"modified_at"`
}

// DisjointRoots validates trusted, stable root names without reopening file data.
// Storage roots must not be aliases, ancestors or descendants of instance roots.
func DisjointRoots(first, second *os.Root) error {
	firstInfo, err := first.Stat(".")
	if err != nil {
		return err
	}
	secondInfo, err := second.Stat(".")
	if err != nil {
		return err
	}
	if os.SameFile(firstInfo, secondInfo) {
		return errors.New("storage and instance roots must be distinct")
	}
	a, err := filepath.EvalSymlinks(first.Name())
	if err != nil {
		return err
	}
	b, err := filepath.EvalSymlinks(second.Name())
	if err != nil {
		return err
	}
	a, err = filepath.Abs(a)
	if err != nil {
		return err
	}
	b, err = filepath.Abs(b)
	if err != nil {
		return err
	}
	for _, pair := range [][2]string{{a, b}, {b, a}} {
		rel, err := filepath.Rel(pair[0], pair[1])
		if err != nil {
			return err
		}
		if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
			return errors.New("storage and instance roots must be disjoint")
		}
	}
	return nil
}

// ValidatePath rejects rather than cleans ambiguous input. A dot denotes the root.
func ValidatePath(name string) error {
	if name == "." {
		return nil
	}
	if name == "" || len(name) > 4096 || !utf8.ValidString(name) || strings.ContainsAny(name, "\\:") || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return ErrUnsafePath
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." || len(part) > 255 {
			return ErrUnsafePath
		}
	}
	return nil
}

func managedPath(name string, rootAllowed bool) error {
	if err := ValidatePath(name); err != nil {
		return err
	}
	if !rootAllowed && name == "." {
		return ErrUnsafePath
	}
	for _, part := range strings.Split(name, "/") {
		if strings.HasPrefix(strings.ToLower(part), reserved) {
			return ErrUnsafePath
		}
	}
	return nil
}

func regular(info fs.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return info.Mode().IsRegular() && ok && st.Nlink == 1
}

// Check rejects existing symlink components, devices, and multiply-linked files.
// os.Root additionally confines resolution when an external process races us.
func Check(root *os.Root, name string) (fs.FileInfo, error) {
	if err := ValidatePath(name); err != nil {
		return nil, err
	}
	var info fs.FileInfo
	var err error
	parts := strings.Split(name, "/")
	for i := range parts {
		info, err = root.Lstat(strings.Join(parts[:i+1], "/"))
		if err != nil {
			return nil, err
		}
		if !info.IsDir() && !regular(info) {
			return nil, ErrUnsafeType
		}
		if i < len(parts)-1 && !info.IsDir() {
			return nil, ErrUnsafeType
		}
	}
	return info, nil
}

// OpenRegular returns a seekable, confined descriptor, never a FIFO or device.
func OpenRegular(root *os.Root, name string) (*os.File, error) {
	before, err := Check(root, name)
	if err != nil {
		return nil, err
	}
	if !regular(before) {
		return nil, ErrUnsafeType
	}
	f, err := root.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil || !regular(after) || !os.SameFile(before, after) {
		f.Close()
		if err != nil {
			return nil, err
		}
		return nil, ErrUnsafeType
	}
	return f, nil
}

func openDir(root *os.Root, name string) (*os.File, error) {
	info, err := Check(root, name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, ErrUnsafeType
	}
	f, err := root.OpenFile(name, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil || !os.SameFile(info, after) {
		f.Close()
		if err != nil {
			return nil, err
		}
		return nil, ErrUnsafeType
	}
	return f, nil
}

func Open(root *os.Root, name string) (*os.File, error) {
	if err := managedPath(name, false); err != nil {
		return nil, err
	}
	return OpenRegular(root, name)
}

// Copy bounds expanded bytes and checks cancellation between reads and writes.
func Copy(ctx context.Context, dst io.Writer, src io.Reader, maxBytes int64, progress Progress) (int64, error) {
	if maxBytes < 0 {
		return 0, ErrTooLarge
	}
	var total int64
	buf := make([]byte, 128*1024)
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, readErr := src.Read(buf)
		if int64(n) > maxBytes-total {
			return total, ErrTooLarge
		}
		if n > 0 {
			if err := ctx.Err(); err != nil {
				return total, err
			}
			written, err := dst.Write(buf[:n])
			total += int64(written)
			if progress != nil {
				progress(total)
			}
			if err != nil {
				return total, err
			}
			if written != n {
				return total, io.ErrShortWrite
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return total, nil
			}
			return total, readErr
		}
		if n == 0 {
			return total, io.ErrNoProgress
		}
	}
}

func ReadText(ctx context.Context, root *os.Root, name string, maxBytes int64) (string, error) {
	f, err := Open(root, name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var out strings.Builder
	if _, err = Copy(ctx, &out, f, maxBytes, nil); err != nil {
		return "", err
	}
	text := out.String()
	if !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
		return "", errors.New("file is not UTF-8 text")
	}
	return text, nil
}

func List(ctx context.Context, root *os.Root, name string) ([]Entry, error) {
	if err := managedPath(name, true); err != nil {
		return nil, err
	}
	dir, err := openDir(root, name)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	result := []Entry{}
	for {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		items, readErr := dir.Readdir(256)
		for _, info := range items {
			if strings.HasPrefix(strings.ToLower(info.Name()), reserved) {
				continue
			}
			p := path.Join(name, info.Name())
			if err := ValidatePath(p); err != nil || (!info.IsDir() && !regular(info)) {
				return nil, fmt.Errorf("unsafe directory entry %q: %w", info.Name(), ErrUnsafeType)
			}
			result = append(result, Entry{Name: info.Name(), Path: p, Size: info.Size(), Directory: info.IsDir(), ModifiedAt: info.ModTime().Unix()})
			if len(result) > 100000 {
				return nil, ErrTooLarge
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result, nil
}

// Walk includes the starting path. Reserved internal files are skipped.
func Walk(ctx context.Context, root *os.Root, name string, visit func(string, fs.FileInfo) error) error {
	if err := managedPath(name, true); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := Check(root, name)
	if err != nil {
		return err
	}
	if err = visit(name, info); err != nil {
		return err
	}
	if !info.IsDir() {
		return nil
	}
	entries, err := List(ctx, root, name)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := Walk(ctx, root, e.Path, visit); err != nil {
			return err
		}
	}
	return nil
}

func Search(ctx context.Context, root *os.Root, name, query string, limit int) ([]Entry, error) {
	if limit < 1 || limit > 1000 {
		return nil, errors.New("search limit must be between 1 and 1000")
	}
	result := []Entry{}
	done := errors.New("search complete")
	err := Walk(ctx, root, name, func(p string, info fs.FileInfo) error {
		if p != name && strings.Contains(strings.ToLower(info.Name()), strings.ToLower(query)) {
			result = append(result, Entry{Name: info.Name(), Path: p, Size: info.Size(), Directory: info.IsDir(), ModifiedAt: info.ModTime().Unix()})
			if len(result) == limit {
				return done
			}
		}
		return nil
	})
	if errors.Is(err, done) {
		err = nil
	}
	return result, err
}

func syncDir(root *os.Root, name string) error {
	dir, err := openDir(root, name)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func WriteText(ctx context.Context, root *os.Root, name, content string, maxBytes int64) error {
	if err := managedPath(name, false); err != nil {
		return err
	}
	if !utf8.ValidString(content) || strings.ContainsRune(content, 0) {
		return errors.New("content is not UTF-8 text")
	}
	if int64(len(content)) > maxBytes || maxBytes < 0 {
		return ErrTooLarge
	}
	parent := path.Dir(name)
	if _, err := Check(root, parent); err != nil {
		return err
	}
	previous, err := Check(root, name)
	if err == nil && !regular(previous) {
		return ErrUnsafeType
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	tmp := path.Join(parent, reserved+"write-"+model.NewID())
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(tmp)
	_, copyErr := Copy(ctx, f, strings.NewReader(content), maxBytes, nil)
	if copyErr == nil && previous != nil {
		owner := previous.Sys().(*syscall.Stat_t)
		if err := f.Chown(int(owner.Uid), int(owner.Gid)); err != nil {
			copyErr = fmt.Errorf("cannot preserve existing file ownership; original retained: %w", err)
		} else {
			copyErr = f.Chmod(previous.Mode().Perm() & 0666)
		}
		if copyErr == nil {
			staged, statErr := f.Stat()
			if statErr != nil {
				copyErr = statErr
			} else if st := staged.Sys().(*syscall.Stat_t); st.Uid != owner.Uid || st.Gid != owner.Gid || staged.Mode().Perm() != previous.Mode().Perm()&0666 || !regular(staged) {
				copyErr = errors.New("staged file ownership or permissions changed; original retained")
			}
		}
	}
	if copyErr == nil {
		copyErr = f.Sync()
	}
	err = f.Close()
	if copyErr != nil {
		return copyErr
	}
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = root.Rename(tmp, name); err != nil {
		return err
	}
	return syncDir(root, parent)
}

func Mkdir(ctx context.Context, root *os.Root, name string) error {
	if err := managedPath(name, false); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := Check(root, path.Dir(name)); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := root.Mkdir(name, 0700); err != nil {
		return err
	}
	return syncDir(root, path.Dir(name))
}

func move(ctx context.Context, root *os.Root, from, to string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := Check(root, from); err != nil {
		return err
	}
	if _, err := Check(root, path.Dir(to)); err != nil {
		return err
	}
	if _, err := root.Lstat(to); err == nil {
		return ErrExists
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := RenameNoReplace(root, from, to); err != nil {
		return err
	}
	return errors.Join(syncDir(root, path.Dir(from)), syncDir(root, path.Dir(to)))
}

// RenameNoReplace atomically refuses replacement, including racing destinations.
// Both parent descriptors are obtained through the confined root.
func RenameNoReplace(root *os.Root, from, to string) error {
	return RenameBetween(root, from, root, to)
}

// RenameBetween performs a no-replace rename across two trusted roots. Cross-
// filesystem moves fail without copying or deleting the source.
func RenameBetween(source *os.Root, from string, destination *os.Root, to string) error {
	if err := ValidatePath(from); err != nil || from == "." {
		return ErrUnsafePath
	}
	if err := ValidatePath(to); err != nil || to == "." {
		return ErrUnsafePath
	}
	src, err := openDir(source, path.Dir(from))
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := openDir(destination, path.Dir(to))
	if err != nil {
		return err
	}
	defer dst.Close()
	if err = renameExclusive(int(src.Fd()), path.Base(from), int(dst.Fd()), path.Base(to)); errors.Is(err, fs.ErrExist) {
		return ErrExists
	}
	return err
}

func Move(ctx context.Context, root *os.Root, from, to string) error {
	if err := managedPath(from, false); err != nil {
		return err
	}
	if err := managedPath(to, false); err != nil {
		return err
	}
	return move(ctx, root, from, to)
}

// Trash uses an external private root on the same filesystem and retains data.
// Persist the returned token and original path in the caller's audit/job record.
func Trash(ctx context.Context, root, recovery *os.Root, name string) (string, error) {
	if err := managedPath(name, false); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := DisjointRoots(root, recovery); err != nil {
		return "", err
	}
	if _, err := Check(root, name); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	token := model.NewID()
	if err := RenameBetween(root, name, recovery, token); err != nil {
		return token, err
	}
	return token, errors.Join(syncDir(root, path.Dir(name)), syncDir(recovery, "."))
}

func Recover(ctx context.Context, root, recovery *os.Root, token, destination string) error {
	if !model.ValidID(token) {
		return ErrUnsafePath
	}
	if err := managedPath(destination, false); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := DisjointRoots(root, recovery); err != nil {
		return err
	}
	if _, err := Check(recovery, token); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := RenameBetween(recovery, token, root, destination); err != nil {
		return err
	}
	return errors.Join(syncDir(root, path.Dir(destination)), syncDir(recovery, "."))
}

func ExportZIP(ctx context.Context, root *os.Root, name string, dst io.Writer, progress Progress) error {
	zw := zip.NewWriter(dst)
	var total int64
	err := Walk(ctx, root, name, func(p string, info fs.FileInfo) error {
		if p == "." {
			return nil
		}
		h, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		h.Name = p
		h.Method = zip.Deflate
		if info.IsDir() {
			h.Name += "/"
		}
		out, err := zw.CreateHeader(h)
		if err != nil || info.IsDir() {
			return err
		}
		f, err := OpenRegular(root, p)
		if err != nil {
			return err
		}
		defer f.Close()
		n, err := Copy(ctx, out, f, info.Size(), func(n int64) {
			if progress != nil {
				progress(total + n)
			}
		})
		total += n
		if err == nil && n != info.Size() {
			err = errors.New("file changed during export")
		}
		return err
	})
	return errors.Join(err, zw.Close())
}
