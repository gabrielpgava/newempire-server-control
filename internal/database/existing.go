package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const maxExistingFileSize = 1 << 40 // Recovery must not open database or sidecar files larger than 1 TiB.

// OpenExisting opens private, initialized panel state without creating or migrating it.
// The caller must trust the path's ancestors and exclude other writers with panel.lock.
func OpenExisting(path string) (*Store, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dir, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	if !dir.IsDir() || !existingOwned(dir) {
		return nil, errors.New("database directory must be a real private directory owned by the executing account")
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	openedDir, err := root.Stat(".")
	if err != nil || !os.SameFile(dir, openedDir) {
		return nil, errors.Join(err, errors.New("database directory changed while opening"))
	}
	name := filepath.Base(path)
	info, err := existingFile(root, name, 100)
	if err != nil {
		return nil, err
	}
	if err := existingSidecars(root, name); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	u := &url.URL{Scheme: "file", Path: path}
	// panel.lock excludes writers; unix-none permits read-only exclusive access with an in-memory WAL index.
	reader, err := sql.Open("sqlite", u.String()+"?mode=ro&vfs=unix-none&_pragma=locking_mode(EXCLUSIVE)")
	if err != nil {
		return nil, err
	}
	reader.SetMaxOpenConns(1)
	var version int
	err = reader.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version)
	if err := errors.Join(err, reader.Close()); err != nil {
		return nil, fmt.Errorf("read existing database schema: %w", err)
	}
	if version != SchemaVersion {
		return nil, fmt.Errorf("existing database schema is %d; this binary requires %d and recovery never runs migrations", version, SchemaVersion)
	}
	if err := existingIdentity(root, path, dir, info); err != nil {
		return nil, err
	}
	if err := existingSidecars(root, name); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", u.String()+"?mode=rw&"+connectionOptions)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if err := existingIdentity(root, path, dir, info); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db, path}, nil
}

func existingOwned(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid()) && info.Mode().Perm()&0077 == 0 &&
		info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) == 0
}

func existingRegular(info os.FileInfo, min int64) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return info.Mode().IsRegular() && existingOwned(info) && ok && stat.Nlink == 1 &&
		info.Size() >= min && info.Size() <= maxExistingFileSize
}

func existingFile(root *os.Root, name string, min int64) (os.FileInfo, error) {
	before, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !existingRegular(before, min) {
		return nil, fmt.Errorf("%s must be a bounded private regular file with one link owned by the executing account", name)
	}
	f, err := root.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	opened, err := f.Stat()
	after, statErr := root.Lstat(name)
	closeErr := f.Close()
	if err := errors.Join(err, statErr, closeErr); err != nil {
		return nil, err
	}
	if !existingRegular(opened, min) || !existingRegular(after, min) || !os.SameFile(before, opened) || !os.SameFile(opened, after) {
		return nil, fmt.Errorf("%s changed while opening", name)
	}
	return opened, nil
}

func existingSidecars(root *os.Root, name string) error {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := existingFile(root, name+suffix, 0); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func existingIdentity(root *os.Root, path string, dir, file os.FileInfo) error {
	currentDir, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return err
	}
	current, err := root.Lstat(filepath.Base(path))
	if err != nil {
		return err
	}
	// Do not open/close the main file while SQLite is connected: closing another FD drops POSIX locks.
	if !currentDir.IsDir() || !existingOwned(currentDir) || !os.SameFile(dir, currentDir) ||
		!existingRegular(current, 100) || !os.SameFile(file, current) {
		return errors.New("existing database path changed while opening")
	}
	return nil
}
