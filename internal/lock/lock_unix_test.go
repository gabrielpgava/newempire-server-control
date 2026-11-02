//go:build unix

package lock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLockRejectsLinksAndKeepsExclusiveOwnership(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(dir, "must-not-be-created")
	path := filepath.Join(dir, "panel.lock")
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if release, err := Acquire(path); err == nil {
		release()
		t.Fatal("lock followed a symlink")
	}
	if _, err := os.Stat(outside); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("lock created a file through its symlink")
	}
	os.Remove(path)
	release, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := Acquire(path); err == nil {
		second()
		t.Fatal("concurrent owner acquired the lock")
	}
	release()
	if err := os.Link(path, filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	if release, err := Acquire(path); err == nil {
		release()
		t.Fatal("hard-linked lock was accepted")
	}
}
