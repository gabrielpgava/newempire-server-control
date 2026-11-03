package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

func TestOpenExistingMissingDoesNotCreate(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(fmt.Sprint(nested), func(t *testing.T) {
			dir := existingDir(t)
			path := filepath.Join(dir, "panel.db")
			if nested {
				path = filepath.Join(dir, "missing", "panel.db")
			}
			before := existingTree(t, dir)
			if s, err := OpenExisting(path); err == nil {
				s.Close()
				t.Fatal("opened missing state")
			}
			assertExistingTree(t, dir, before)
		})
	}
}

func TestOpenExistingRejectsUninitializedOrUnsupportedSchemaWithoutMutation(t *testing.T) {
	for _, version := range []int{-1, SchemaVersion - 1, SchemaVersion + 1} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			dir := existingDir(t)
			path := filepath.Join(dir, "panel.db")
			if version == -1 {
				if err := os.WriteFile(path, nil, 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				s, err := Open(path)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.Exec("PRAGMA journal_mode=DELETE"); err != nil {
					t.Fatal(err)
				}
				if _, err := s.Exec(fmt.Sprintf("PRAGMA user_version=%d", version)); err != nil {
					t.Fatal(err)
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path+"-journal", make([]byte, 512), 0600); err != nil {
					t.Fatal(err)
				}
			}
			before := existingTree(t, dir)
			if s, err := OpenExisting(path); err == nil {
				s.Close()
				t.Fatal("accepted empty or unsupported database")
			} else if version >= 0 && !strings.Contains(err.Error(), "existing database schema is ") {
				t.Fatalf("schema preflight could not read existing database: %v", err)
			}
			assertExistingTree(t, dir, before)
		})
	}
}

func TestOpenExistingChecksWALSchemaWithoutWritingSidecars(t *testing.T) {
	for _, version := range []int{SchemaVersion - 1, SchemaVersion + 1} {
		for _, index := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/index=%t", version, index), func(t *testing.T) {
				path := existingWALFixture(t, version)
				if index {
					if err := os.WriteFile(path+"-shm", make([]byte, 32768), 0600); err != nil {
						t.Fatal(err)
					}
				}
				dir := filepath.Dir(path)
				before := existingTree(t, dir)
				if s, err := OpenExisting(path); err == nil {
					s.Close()
					t.Fatal("accepted unsupported schema in committed WAL")
				} else if !strings.Contains(err.Error(), "existing database schema is ") {
					t.Fatalf("schema preflight could not read committed WAL: %v", err)
				}
				assertExistingTree(t, dir, before)
			})
		}
	}
}

func TestOpenExistingEnablesWALForCurrentSchema(t *testing.T) {
	path := filepath.Join(existingDir(t), "panel.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Exec("PRAGMA journal_mode=DELETE"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	s, err = OpenExisting(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var mode string
	if err := s.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("existing database did not enable WAL: %v", err)
	}
	if _, err := s.Exec(`INSERT INTO settings(key,value) VALUES('recovery','true')`); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
		t.Fatalf("replaced existing database or changed its permissions: %v", err)
	}
}

func TestOpenExistingReadsCommittedWALAndRetainsDurability(t *testing.T) {
	path := existingWALFixture(t, SchemaVersion)
	s, err := OpenExisting(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var value string
	if err := s.QueryRow("SELECT value FROM settings WHERE key='wal-sample'").Scan(&value); err != nil || value != `"committed"` {
		t.Fatalf("committed WAL data was not visible: %v", err)
	}
	for pragma, want := range map[string]string{"foreign_keys": "1", "journal_mode": "wal", "busy_timeout": "10000", "synchronous": "2", "locking_mode": "normal", "user_version": fmt.Sprint(SchemaVersion)} {
		var got string
		if err := s.QueryRow("PRAGMA " + pragma).Scan(&got); err != nil || got != want {
			t.Fatalf("%s=%s, want %s: %v", pragma, got, want, err)
		}
	}
	if err := s.Integrity(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Exec("INSERT INTO sessions VALUES('x','missing','csrf',1)"); err == nil {
		t.Fatal("foreign key was not enforced")
	}
	tx, err := s.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	u := &url.URL{Scheme: "file", Path: path}
	other, err := sql.Open("sqlite", u.String()+"?mode=rw&_pragma=busy_timeout(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	_, err = other.Exec(`INSERT INTO settings(key,value) VALUES('blocked','"value"')`)
	var locked *sqlite.Error
	if !errors.As(err, &locked) || locked.Code() != sqlite3.SQLITE_BUSY {
		t.Fatalf("transaction did not acquire its immediate write lock: %v", err)
	}
}

func TestOpenExistingRejectsUnsafeFilesWithoutMutation(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "permissions", "directory", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			dir := existingDir(t)
			path := filepath.Join(dir, "panel.db")
			s, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			unsafeExistingFixture(t, path, kind)
			before := existingTree(t, dir)
			if s, err := OpenExisting(path); err == nil {
				s.Close()
				t.Fatal("accepted unsafe database file")
			}
			assertExistingTree(t, dir, before)
		})
	}
}

func TestOpenExistingRejectsUnsafeSidecarsWithoutMutation(t *testing.T) {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		for _, kind := range []string{"symlink", "hardlink", "permissions", "directory", "fifo"} {
			t.Run(suffix+"/"+kind, func(t *testing.T) {
				dir := existingDir(t)
				path := filepath.Join(dir, "panel.db")
				s, err := Open(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path+suffix, []byte("untouched"), 0600); err != nil {
					t.Fatal(err)
				}
				unsafeExistingFixture(t, path+suffix, kind)
				before := existingTree(t, dir)
				if s, err := OpenExisting(path); err == nil {
					s.Close()
					t.Fatal("accepted unsafe SQLite sidecar")
				}
				assertExistingTree(t, dir, before)
			})
		}
	}
}

func TestOpenExistingRejectsUnsafeDirectory(t *testing.T) {
	for _, kind := range []string{"symlink", "permissions"} {
		t.Run(kind, func(t *testing.T) {
			dir := existingDir(t)
			s, err := Open(filepath.Join(dir, "panel.db"))
			if err != nil {
				t.Fatal(err)
			}
			s.Close()
			before := existingTree(t, dir)
			path := filepath.Join(dir, "panel.db")
			if kind == "permissions" {
				if err := os.Chmod(dir, 0755); err != nil {
					t.Fatal(err)
				}
			} else {
				link := filepath.Join(existingDir(t), "link")
				if err := os.Symlink(dir, link); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(link, "panel.db")
			}
			if s, err := OpenExisting(path); err == nil {
				s.Close()
				t.Fatal("accepted unsafe database directory")
			}
			assertExistingTree(t, dir, before)
			if kind == "permissions" {
				info, err := os.Stat(dir)
				if err != nil || info.Mode().Perm() != 0755 {
					t.Fatalf("changed directory permissions: %v", err)
				}
			}
		})
	}
}

func TestExistingFileBoundsAndOwner(t *testing.T) {
	path := filepath.Join(existingDir(t), "panel.db")
	if err := os.WriteFile(path, make([]byte, 100), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	foreign := existingFileInfo{FileInfo: info, stat: *info.Sys().(*syscall.Stat_t), size: info.Size()}
	foreign.stat.Uid++
	if existingRegular(foreign, 100) {
		t.Fatal("accepted another account's file")
	}
	// Exercise the production limit without exceeding the sandbox's file-size limit.
	for _, size := range []int64{-1, 99, 100, maxExistingFileSize, maxExistingFileSize + 1} {
		bounded := existingFileInfo{FileInfo: info, stat: *info.Sys().(*syscall.Stat_t), size: size}
		if got, want := existingRegular(bounded, 100), size >= 100 && size <= maxExistingFileSize; got != want {
			t.Errorf("existingRegular size %d = %v; want %v", size, got, want)
		}
	}
}

type existingFileInfo struct {
	os.FileInfo
	stat syscall.Stat_t
	size int64
}

func (i existingFileInfo) Sys() any    { return &i.stat }
func (i existingFileInfo) Size() int64 { return i.size }

func existingWALFixture(t *testing.T, version int) string {
	t.Helper()
	path := filepath.Join(existingDir(t), "panel.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Exec(`INSERT INTO settings(key,value) VALUES('wal-sample','"committed"')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Exec(fmt.Sprintf("PRAGMA user_version=%d", version)); err != nil {
		t.Fatal(err)
	}
	copy := filepath.Join(existingDir(t), "panel.db")
	// A quiescent test-only copy models a crash with committed WAL and no shared-memory index.
	for _, suffix := range []string{"", "-wal"} {
		data, err := os.ReadFile(path + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(copy+suffix, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return copy
}

func unsafeExistingFixture(t *testing.T, path, kind string) {
	t.Helper()
	var err error
	switch kind {
	case "symlink":
		err = os.Rename(path, path+".target")
		if err == nil {
			err = os.Symlink(filepath.Base(path)+".target", path)
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
}

func existingDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

type existingFileState struct {
	Mode     os.FileMode
	Size     int64
	Modified time.Time
	Data     string
}

func existingTree(t *testing.T, dir string) map[string]existingFileState {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	state := map[string]existingFileState{}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		file := existingFileState{Mode: info.Mode(), Size: info.Size(), Modified: info.ModTime()}
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			file.Data = string(data)
		} else if info.Mode()&os.ModeSymlink != 0 {
			file.Data, err = os.Readlink(filepath.Join(dir, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
		}
		state[entry.Name()] = file
	}
	return state
}

func assertExistingTree(t *testing.T, dir string, before map[string]existingFileState) {
	t.Helper()
	if !reflect.DeepEqual(before, existingTree(t, dir)) {
		t.Fatal("existing-only open changed directory entries, file bytes, sizes, timestamps, or permissions")
	}
}
