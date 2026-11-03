package secrets

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestOpenExistingMissingDoesNotCreate(t *testing.T) {
	for _, name := range []string{"master.key", "missing/master.key"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if _, err := OpenExisting(filepath.Join(dir, name)); err == nil {
				t.Fatal("accepted missing key")
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 0 {
				t.Fatalf("created missing key or directory: %v", err)
			}
		})
	}
}

func TestOpenExistingKeyRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.key")
	original, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := original.Seal([]byte("existing-credential"), "purpose")
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0400); err != nil {
		t.Fatal(err)
	}
	v, err := OpenExisting(path)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := v.Open(sealed, "purpose")
	if err != nil || string(plain) != "existing-credential" {
		t.Fatalf("existing key could not decrypt existing ciphertext: %v", err)
	}
	if _, err := v.Open(sealed, "different-purpose"); err == nil {
		t.Fatal("existing key ignored credential purpose")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("changed existing key: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0400 {
		t.Fatalf("changed read-only key permissions: %v", err)
	}
}

func TestOpenExistingRejectsUnsafeKeyWithoutMutation(t *testing.T) {
	for _, kind := range []string{"empty", "short", "long", "symlink", "hardlink", "permissions", "directory", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "master.key")
			data := bytes.Repeat([]byte{42}, 32)
			switch kind {
			case "empty":
				data = nil
			case "short":
				data = data[:31]
			case "long":
				data = append(data, 42)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			target := path
			var err error
			switch kind {
			case "symlink":
				target = path + ".target"
				err = os.Rename(path, target)
				if err == nil {
					err = os.Symlink(filepath.Base(target), path)
				}
			case "hardlink":
				err = os.Link(path, path+".alias")
			case "permissions":
				err = os.Chmod(path, 0644)
			case "directory", "fifo":
				err = os.Remove(path)
				if err == nil {
					if kind == "directory" {
						err = os.Mkdir(path, 0700)
					} else {
						err = unix.Mkfifo(path, 0600)
					}
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(target)
			if err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := OpenExisting(path); err == nil {
				t.Fatal("accepted unsafe existing key")
			}
			after, err := os.Lstat(target)
			if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) || before.Size() != after.Size() {
				t.Fatalf("changed rejected key: %v", err)
			}
			if after.Mode().IsRegular() {
				got, err := os.ReadFile(target)
				if err != nil || !bytes.Equal(data, got) {
					t.Fatalf("changed rejected key bytes: %v", err)
				}
			}
			current, err := os.ReadDir(dir)
			if err != nil || len(current) != len(entries) {
				t.Fatalf("changed directory entries: %v", err)
			}
		})
	}
}

func TestExistingKeyRejectsForeignOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(path, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	foreign := foreignKeyInfo{FileInfo: info, stat: *info.Sys().(*syscall.Stat_t)}
	foreign.stat.Uid++
	if existingKey(foreign) {
		t.Fatal("accepted another account's master key")
	}
}

type foreignKeyInfo struct {
	os.FileInfo
	stat syscall.Stat_t
}

func (i foreignKeyInfo) Sys() any { return &i.stat }
