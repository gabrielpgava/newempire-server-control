//go:build linux

package servicehelper

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gabrielpgava/newempire-server-control/internal/model"
	"golang.org/x/sys/unix"
)

func TestPermissionValidation(t *testing.T) {
	a := account{uid: 1001, gid: 1002, groups: map[uint32]bool{1002: true}}
	for _, tc := range []struct {
		mode, uid, gid, want uint32
		ok                   bool
	}{
		{0660, 2000, 1002, 6, true}, {0600, 2000, 1002, 4, false}, {02770, 1001, 1002, 7, true}, {0640, 1001, 1002, 6, true}, {0640, 2000, 1002, 6, false}, {0666, 2000, 2001, 6, true}, {0000, 1001, 1002, 4, false},
	} {
		if got := canAccess(tc.mode, tc.uid, tc.gid, a, tc.want); got != tc.ok {
			t.Errorf("permissions %#o: got %v want %v", tc.mode, got, tc.ok)
		}
	}
}

func TestManagedEntryConstraints(t *testing.T) {
	for _, tc := range []struct {
		mode, uid uint32
		links     uint64
		ok        bool
	}{
		{unix.S_IFREG | 0600, 1001, 1, true}, {unix.S_IFREG | 0600, 1002, 1, true}, {unix.S_IFREG | 0600, 0, 1, false}, {unix.S_IFREG | 0600, 1003, 1, false}, {unix.S_IFREG | 0600, 1001, 2, false}, {unix.S_IFLNK | 0777, 1001, 1, false}, {unix.S_IFIFO | 0600, 1001, 1, false}, {unix.S_IFDIR | 0700, 1001, 2, true},
	} {
		st := unix.Stat_t{Mode: tc.mode, Uid: tc.uid, Nlink: 1}
		if tc.links == 2 {
			st.Nlink = 2
		}
		if got := validManagedEntry(&st, 1001, 1002); got != tc.ok {
			t.Errorf("unsafe owner/type/link constraint: %+v got %v", tc, got)
		}
	}
}

func TestManagedACLAllowsLivePanelWithoutSharingPanelGroup(t *testing.T) {
	for _, dir := range []bool{false, true} {
		acl := managedACL(1001, dir)
		if len(acl) != 44 || binary.LittleEndian.Uint32(acl) != 2 || binary.LittleEndian.Uint16(acl[12:]) != 2 || binary.LittleEndian.Uint32(acl[16:]) != 1001 {
			t.Fatal("invalid named panel ACL")
		}
		if binary.LittleEndian.Uint16(acl[38:]) != 0 {
			t.Fatal("ACL grants access to other users")
		}
	}
}

func TestAdoptedACLDoesNotMistakeMaskForGroupPermission(t *testing.T) {
	st := unix.Stat_t{Mode: 0660, Uid: 1001, Gid: 1003}
	acl := managedACL(1001, false)
	// The mask is rw, but the owning group's actual entry is read-only.
	binary.LittleEndian.PutUint16(acl[22:], 4)
	mc := account{uid: 1002, gid: 1003, groups: map[uint32]bool{1003: true}}
	if canAccessACL(&st, acl, mc, 6) {
		t.Fatal("ACL mask was treated as write permission")
	}
	if !canAccessACL(&st, acl, mc, 4) {
		t.Fatal("valid group read access rejected")
	}
	panel := account{uid: 1001, gid: 2000, groups: map[uint32]bool{2000: true}}
	if !canAccessACL(&st, acl, panel, 6) {
		t.Fatal("owner lost its actual permissions")
	}
}

func TestServerPropertiesMatchFirewall(t *testing.T) {
	good := "server-port=25565\nrcon.port=25575\nenable-rcon=true\nrcon.password=test-only-password\nmotd=Hello\\u0020World\n"
	if err := validateProperties([]byte(good), 25565, 25575); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		strings.Replace(good, "rcon.port=25575", "rcon.port=25576", 1),
		strings.Replace(good, "enable-rcon=true", "enable-rcon=false", 1),
		strings.Replace(good, "rcon.password=test-only-password", "rcon.password=", 1),
		strings.Replace(good, "rcon.port=25575", "rcon\\u002Eport=25575", 1),
		strings.Replace(good, "rcon.port=25575", "\vrcon.port=25575", 1),
		good + "rcon.port=25576\n", good + "escaped=continued\\\n",
		strings.Replace(good, "rcon.password=test-only-password", "rcon.password=test-only-password\rrcon.port=25576", 1),
	} {
		if err := validateProperties([]byte(bad), 25565, 25575); err == nil {
			t.Fatal("accepted properties that bypass registered RCON filtering")
		}
	}
}

func TestAuditDiagnosticEscapesAndBoundsUntrustedPaths(t *testing.T) {
	text := safeDiagnostic(errors.New("untrusted\nforged-audit " + strings.Repeat("x", 2048)))
	if strings.Contains(text, "\n") || len(text) > 1100 {
		t.Fatal("audit diagnostic permits multiline or unbounded output")
	}
}

func TestReconfigurationNeverChangesIdentity(t *testing.T) {
	before := testInstance()
	for _, mutate := range []func(*model.Instance){
		func(i *model.Instance) { i.ID = "11111111111111111111111111111111" }, func(i *model.Instance) { i.Root = "/srv/adopt/other" }, func(i *model.Instance) { i.Port++ }, func(i *model.Instance) { i.RCONPort++ }, func(i *model.Instance) { i.Adopted = true },
	} {
		after := before
		mutate(&after)
		if unchangedIdentity(before, after) {
			t.Fatal("reconfiguration changed registered identity")
		}
	}
	after := before
	after.JavaPath = "/usr/lib/jvm/other-approved/bin/java"
	after.Jar = "updated.jar"
	after.MemoryMB = 4096
	if !unchangedIdentity(before, after) {
		t.Fatal("allowed runtime change rejected")
	}
}

func TestPrepareManagedNormalizesStoppedTree(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires an isolated root fixture with CHOWN/DAC_OVERRIDE/FOWNER/FSETID capabilities")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "minecraft-created.properties")
	if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(path, 1002, 1003); err != nil {
		if errors.Is(err, unix.EPERM) {
			t.Skip("container intentionally drops CHOWN capability")
		}
		t.Fatal(err)
	}
	if err := os.Chown(dir, 1001, 1003); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 256; i++ {
		extra := filepath.Join(dir, fmt.Sprintf("extra-%03d.dat", i))
		if err := os.WriteFile(extra, []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(extra, 1002, 1003); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := prepareManaged(context.Background(), root, 1001, 1002, 1003, false); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	if err := prepareManaged(context.Background(), root, 1001, 1002, 1003, true); err != nil {
		t.Fatal(err)
	}
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		t.Fatal(err)
	}
	if st.Uid != 1001 || st.Gid != 1003 || st.Mode&07777 != 0660 {
		t.Fatalf("managed file not normalized safely: %+v", st)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "fixture" {
		t.Fatal("permission preparation changed file contents")
	}
	if err := unix.Fstat(int(root.Fd()), &st); err != nil {
		t.Fatal(err)
	}
	if st.Uid != 1001 || st.Gid != 1003 || st.Mode&07777 != 02770 {
		t.Fatalf("managed root not normalized safely: %+v", st)
	}
	acl := make([]byte, 128)
	n, err := unix.Fgetxattr(int(root.Fd()), "system.posix_acl_default", acl)
	if err != nil || n != 44 {
		t.Fatalf("default ACL missing: %d %v", n, err)
	}
	if err := os.Link(path, filepath.Join(dir, "hardlink")); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	if err := prepareManaged(context.Background(), root, 1001, 1002, 1003, false); err == nil {
		t.Fatal("hardlink accepted for root ownership normalization")
	}
	if err := os.Remove(filepath.Join(dir, "hardlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "symlink")); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	if err := prepareManaged(context.Background(), root, 1001, 1002, 1003, false); err == nil {
		t.Fatal("symlink accepted for root traversal")
	}
}

func TestStoppedProofRefusesActiveOrOrphanProcesses(t *testing.T) {
	for _, state := range []string{"active\n", "activating\n", "deactivating\n"} {
		r := &fakeRunner{handle: func(c runCall) ([]byte, error) {
			if c.args[1] == "--property=MainPID" {
				return []byte("0\n"), nil
			}
			return []byte(state), nil
		}}
		if err := proveStopped(context.Background(), r, testID, uint32(os.Geteuid()), false); err == nil {
			t.Fatal("non-stopped unit accepted")
		}
	}
	if err := noInstanceProcesses(uint32(os.Geteuid())); err == nil {
		t.Fatal("current process was not detected as an orphan process")
	}
}

func TestLoadedUnitMustUseRegisteredJavaArguments(t *testing.T) {
	i := testInstance()
	good := "{ path=" + i.JavaPath + " ; argv[]=" + i.JavaPath + " -Xms256M -Xmx2048M -jar ./" + i.Jar + " nogui ; ignore_errors=no ; pid=0 ; }\n"
	for _, text := range []string{good, strings.Replace(good, "-Xmx2048M", "-Xmx4096M", 1), strings.Replace(good, i.JavaPath, "/bin/sh", 1)} {
		r := &fakeRunner{handle: func(runCall) ([]byte, error) { return []byte(text), nil }}
		err := verifyLoadedCommand(context.Background(), r, manifest{Instance: i})
		if (text == good) != (err == nil) {
			t.Fatalf("unexpected loaded-command acceptance: %v", err)
		}
	}
}

func TestTrustedFileRejectsWritableFile(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "untrusted")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Chmod(0666); err != nil {
		t.Fatal(err)
	}
	if err := checkTrusted(f, false); err == nil {
		t.Fatal("group/world writable privileged file accepted")
	}
}

func TestTrustedDirectoryRejectsSymlinkAndWritableAncestors(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink("/etc", dir+"/linked"); err != nil {
		t.Fatal(err)
	}
	if f, err := trustedDir(dir + "/linked"); err == nil {
		f.Close()
		t.Fatal("symlink/untrusted ancestor accepted")
	}
}

func TestRootFileWritesAreIdempotentAndRejectEscapes(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires isolated root-owned fixture")
	}
	dir := t.TempDir()
	trusted, err := trustedDir(dir)
	if err != nil {
		t.Skip("fixture needs root-controlled temporary ancestors")
	}
	trusted.Close()
	path := filepath.Join(dir, "registration.json")
	if err := writeExclusive(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeExclusive(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeExclusive(path, []byte("different"), 0600); err == nil {
		t.Fatal("changed registration silently replaced")
	}
	if err := atomicWriteTrusted(path, []byte("prepared"), 0600); err != nil {
		t.Fatal(err)
	}
	data, err := readTrusted(path)
	if err != nil || !bytes.Equal(data, []byte("prepared")) {
		t.Fatalf("atomic registration update: %q %v", data, err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteTrusted(link, []byte("escape"), 0600); err == nil {
		t.Fatal("root write followed a symlink")
	}
	data, err = readTrusted(path)
	if err != nil || string(data) != "prepared" {
		t.Fatal("symlink target changed")
	}
}
