package files

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func testRoot(t *testing.T) *os.Root {
	t.Helper()
	r, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func TestPathsAndUnsafeTypes(t *testing.T) {
	r := testRoot(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "../secret", "/secret", "a/../secret", "a//b", "a/./b", "a\\b", "C:secret", "a\x00b", "a\nb"} {
		if _, err := Open(r, name); err == nil {
			t.Errorf("accepted %q", name)
		}
	}
	if err := r.Symlink(outside, "escape"); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(r, "escape/secret"); err == nil {
		t.Fatal("followed an external symlink")
	}
	if err := r.WriteFile("regular", []byte("safe"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := r.Symlink("regular", "alias"); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(r, "alias"); !errors.Is(err, ErrUnsafeType) {
		t.Fatalf("symlink: %v", err)
	}
	if err := r.Link("regular", "hardlink"); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(r, "regular"); !errors.Is(err, ErrUnsafeType) {
		t.Fatalf("hardlink: %v", err)
	}
	if err := unix.Mkfifo(filepath.Join(r.Name(), "pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(r, "pipe"); !errors.Is(err, ErrUnsafeType) {
		t.Fatalf("FIFO: %v", err)
	}
}

func TestAtomicTextAndCancellation(t *testing.T) {
	ctx := context.Background()
	r := testRoot(t)
	if err := WriteText(ctx, r, "notes.txt", "original", 100); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		content string
		limit   int64
	}{{"too long", 3}, {"binary\x00data", 100}, {"\xff", 100}} {
		if err := WriteText(ctx, r, "notes.txt", tc.content, tc.limit); err == nil {
			t.Fatal("invalid edit succeeded")
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := WriteText(cancelled, r, "notes.txt", "changed", 100); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled edit: %v", err)
	}
	text, err := ReadText(ctx, r, "notes.txt", 100)
	if err != nil || text != "original" {
		t.Fatalf("original lost: %q %v", text, err)
	}
	if _, err = ReadText(ctx, r, "notes.txt", 3); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("unbounded read: %v", err)
	}
	entries, err := List(ctx, r, ".")
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary file leaked: %+v %v", entries, err)
	}
	var out bytes.Buffer
	copyCtx, stop := context.WithCancel(ctx)
	_, err = Copy(copyCtx, &out, strings.NewReader(strings.Repeat("x", 300000)), 300000, func(int64) { stop() })
	if !errors.Is(err, context.Canceled) || out.Len() == 300000 {
		t.Fatalf("copy cancellation: %d %v", out.Len(), err)
	}
}

func TestTextReplacementPreservesOwnerGroupAndSafePermissions(t *testing.T) {
	r := testRoot(t)
	if err := r.WriteFile("settings", []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := r.Chmod("settings", 0770); err != nil {
		t.Fatal(err)
	}
	before, err := r.Stat("settings")
	if err != nil {
		t.Fatal(err)
	}
	if err = WriteText(context.Background(), r, "settings", "updated", 100); err != nil {
		t.Fatal(err)
	}
	after, err := r.Stat("settings")
	if err != nil {
		t.Fatal(err)
	}
	oldOwner, newOwner := before.Sys().(*syscall.Stat_t), after.Sys().(*syscall.Stat_t)
	if oldOwner.Uid != newOwner.Uid || oldOwner.Gid != newOwner.Gid || after.Mode().Perm() != 0660 || after.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		t.Fatalf("ownership or safe mode changed: %v => %v", before, after)
	}
	data, err := r.ReadFile("settings")
	if err != nil || string(data) != "updated" {
		t.Fatalf("edit failed: %q %v", data, err)
	}
	if err = WriteText(context.Background(), r, "new-file", "private", 100); err != nil {
		t.Fatal(err)
	}
	info, err := r.Stat("new-file")
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("new-file default: %v %v", info, err)
	}
}

func TestTextReplacementPreservesDifferentOwnerWhenPrivileged(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("ownership setup requires root in the Linux test container")
	}
	r := testRoot(t)
	if err := r.WriteFile("adopted", []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := r.Chown("adopted", 65534, 65534); err != nil {
		t.Fatal(err)
	}
	if err := r.Chmod("adopted", 0660); err != nil {
		t.Fatal(err)
	}
	if err := WriteText(context.Background(), r, "adopted", "edited", 100); err != nil {
		t.Fatal(err)
	}
	info, err := r.Stat("adopted")
	if err != nil {
		t.Fatal(err)
	}
	st := info.Sys().(*syscall.Stat_t)
	if st.Uid != 65534 || st.Gid != 65534 || info.Mode().Perm() != 0660 {
		t.Fatalf("adopted ownership lost: %+v %v", st, info.Mode())
	}
}

func TestTextReplacementRefusesOwnershipLoss(t *testing.T) {
	if os.Getenv("NESC_TEST_OWNERSHIP_CHILD") == "1" {
		r, err := os.OpenRoot(os.Getenv("NESC_TEST_OWNERSHIP_ROOT"))
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		err = WriteText(context.Background(), r, "adopted", "changed", 100)
		if err == nil || !strings.Contains(err.Error(), "cannot preserve existing file ownership") {
			t.Fatalf("expected ownership refusal: %v", err)
		}
		return
	}
	if os.Geteuid() != 0 {
		t.Skip("unprivileged child setup requires root in the Linux test container")
	}
	r := testRoot(t)
	if err := r.WriteFile("adopted", []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(r.Name()), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(r.Name(), 0777); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	binary, err := os.Open(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	defer binary.Close()
	accessible, err := r.OpenFile("ownership.test", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0700)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(accessible, binary)
	err = errors.Join(err, accessible.Chmod(0755), accessible.Close())
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, filepath.Join(r.Name(), "ownership.test"), "-test.run=^TestTextReplacementRefusesOwnershipLoss$")
	cmd.Env = append(os.Environ(), "NESC_TEST_OWNERSHIP_CHILD=1", "NESC_TEST_OWNERSHIP_ROOT="+r.Name())
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534}}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("unprivileged ownership test: %v\n%s", err, output)
	}
	data, err := r.ReadFile("adopted")
	if err != nil || string(data) != "original" {
		t.Fatalf("ownership failure changed original: %q %v", data, err)
	}
	info, err := r.Stat("adopted")
	if err != nil || info.Sys().(*syscall.Stat_t).Uid != 0 || info.Mode().Perm() != 0600 {
		t.Fatalf("original ownership changed: %v %v", info, err)
	}
}

func TestMoveTrashRecoverySearchAndZIP(t *testing.T) {
	ctx := context.Background()
	r, recovery := testRoot(t), testRoot(t)
	if err := Mkdir(ctx, r, "world"); err != nil {
		t.Fatal(err)
	}
	if err := WriteText(ctx, r, "world/notes.txt", "hello", 100); err != nil {
		t.Fatal(err)
	}
	if err := WriteText(ctx, r, "existing", "keep", 100); err != nil {
		t.Fatal(err)
	}
	if err := Move(ctx, r, "world/notes.txt", "existing"); !errors.Is(err, ErrExists) {
		t.Fatalf("overwrite: %v", err)
	}
	if err := Move(ctx, r, "world/notes.txt", "world/renamed.txt"); err != nil {
		t.Fatal(err)
	}
	items, err := Search(ctx, r, ".", "RENAMED", 10)
	if err != nil || len(items) != 1 || items[0].Path != "world/renamed.txt" {
		t.Fatalf("search: %+v %v", items, err)
	}
	token, err := Trash(ctx, r, recovery, "world")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.Lstat("world"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("trash did not move source: %v", err)
	}
	if err = Recover(ctx, r, recovery, token, "existing"); !errors.Is(err, ErrExists) {
		t.Fatalf("recovery overwrite: %v", err)
	}
	if err = Recover(ctx, r, recovery, token, "world"); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err = ExportZIP(ctx, r, "world", &archive, nil); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(archive.Bytes()), int64(archive.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if len(zr.File) != 2 || zr.File[1].Name != "world/renamed.txt" {
		t.Fatalf("unexpected archive: %+v", zr.File)
	}
	f, err := Open(r, "world/renamed.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	req := httptest.NewRequest(http.MethodGet, "/file", nil)
	req.Header.Set("Range", "bytes=1-3")
	w := httptest.NewRecorder()
	http.ServeContent(w, req, "renamed.txt", time.Unix(items[0].ModifiedAt, 0), f)
	if w.Code != http.StatusPartialContent || w.Body.String() != "ell" {
		t.Fatalf("range download: %d %q", w.Code, w.Body.String())
	}
}

func TestTrashCrossFilesystemFailsWithoutDeleting(t *testing.T) {
	if _, err := os.Stat("/dev/shm"); err != nil {
		t.Skip("Linux shared-memory filesystem is not available")
	}
	r := testRoot(t)
	name, err := os.MkdirTemp("/dev/shm", "nesc-recovery-")
	if err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { os.RemoveAll(name) })
	recovery, err := os.OpenRoot(name)
	if err != nil {
		t.Fatal(err)
	}
	defer recovery.Close()
	if err = r.WriteFile("keep", []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = Trash(context.Background(), r, recovery, "keep")
	if err == nil {
		t.Fatal("cross-filesystem trash succeeded unexpectedly")
	}
	f, openErr := Open(r, "keep")
	if openErr != nil {
		t.Fatal(openErr)
	}
	defer f.Close()
	data, _ := io.ReadAll(f)
	if string(data) != "original" {
		t.Fatal("original changed")
	}
}

func TestConcurrentSymlinkReplacementDoesNotEscape(t *testing.T) {
	r := testRoot(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := r.Mkdir("slot", 0700); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteFile("slot/secret", []byte("inside"), 0600); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			if r.Rename("slot", "park") != nil {
				return
			}
			if r.Symlink(outside, "slot") != nil {
				return
			}
			if r.Remove("slot") != nil {
				return
			}
			if r.Rename("park", "slot") != nil {
				return
			}
		}
	}()
	for i := 0; i < 1000; i++ {
		f, err := Open(r, "slot/secret")
		if err != nil {
			continue
		}
		data, err := io.ReadAll(f)
		f.Close()
		if err != nil || string(data) != "inside" {
			t.Errorf("sandbox escaped: %q %v", data, err)
			break
		}
	}
	wg.Wait()
}
