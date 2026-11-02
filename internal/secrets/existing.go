package secrets

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// OpenExisting reads an existing private master key, never generating or repairing one.
// The caller must ensure the path's ancestors are trusted.
func OpenExisting(path string) (*Vault, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	name := filepath.Base(path)
	before, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !existingKey(before) {
		return nil, errors.New("existing master key must be a private 32-byte regular file with one link owned by the executing account")
	}
	f, err := root.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !existingKey(opened) || !os.SameFile(before, opened) {
		return nil, errors.New("master key changed while opening")
	}
	key, err := io.ReadAll(io.LimitReader(f, 33))
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	current, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if len(key) != 32 || !existingKey(after) || !existingKey(current) || !os.SameFile(after, current) ||
		!opened.ModTime().Equal(after.ModTime()) {
		return nil, errors.New("master key changed while reading")
	}
	return newVault(key)
}

func existingKey(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return info.Mode().IsRegular() && info.Mode().Perm()&0077 == 0 &&
		info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) == 0 && info.Size() == 32 &&
		ok && stat.Uid == uint32(os.Geteuid()) && stat.Nlink == 1
}
