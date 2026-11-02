package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gabrielpgava/newempire-server-control/internal/api"
	"github.com/gabrielpgava/newempire-server-control/internal/auth"
	"github.com/gabrielpgava/newempire-server-control/internal/backups"
	"github.com/gabrielpgava/newempire-server-control/internal/config"
	"github.com/gabrielpgava/newempire-server-control/internal/database"
	"github.com/gabrielpgava/newempire-server-control/internal/jobs"
	"github.com/gabrielpgava/newempire-server-control/internal/lock"
	"github.com/gabrielpgava/newempire-server-control/internal/model"
	"github.com/gabrielpgava/newempire-server-control/internal/modrinth"
	"github.com/gabrielpgava/newempire-server-control/internal/secrets"
)

func unprivilegedRecoveryTest(t *testing.T) bool {
	t.Helper()
	if os.Geteuid() != 0 {
		return false
	}
	dir, err := os.MkdirTemp(os.TempDir(), "nesc-cli-unpriv-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(program)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	name := filepath.Join(dir, "control.test")
	out, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0755)
	if err != nil {
		t.Fatal(err)
	}
	_, copyErr := io.Copy(out, in)
	if err := errors.Join(copyErr, out.Close()); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(dir, "tmp")
	if err := os.Mkdir(tmp, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(tmp, 65534, 65534); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(name, "-test.v", "-test.run=^"+regexp.QuoteMeta(t.Name())+"$")
	cmd.Env = append(os.Environ(), "TMPDIR="+tmp)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534}}
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("unprivileged recovery tests failed: %v\n%s", err, b)
	}
	t.Log(string(b))
	return true
}

type recoveryRuntime struct {
	running bool
	starts  int
}

func (r *recoveryRuntime) Status(context.Context, model.Instance) (model.RuntimeStatus, error) {
	if r.running {
		return model.RuntimeStatus{State: "running", PID: 123, Ready: true}, nil
	}
	return model.RuntimeStatus{State: "stopped"}, nil
}
func (r *recoveryRuntime) Start(context.Context, model.Instance) error {
	r.starts++
	r.running = true
	return nil
}
func (r *recoveryRuntime) Stop(context.Context, model.Instance) error { r.running = false; return nil }
func (r *recoveryRuntime) Restart(ctx context.Context, i model.Instance) error {
	return r.Start(ctx, i)
}
func (r *recoveryRuntime) Logs(context.Context, model.Instance, int) ([]model.LogLine, error) {
	return nil, nil
}
func (r *recoveryRuntime) Command(context.Context, model.Instance, string) (string, error) {
	return "", errors.New("unexpected RCON call")
}

const fixturePassword = "fixture-owner-password"

func recoveryFixture(t *testing.T) (config.Config, model.Instance) {
	t.Helper()
	c := config.Default()
	c.DataDir = t.TempDir()
	if err := os.Chmod(c.DataDir, 0700); err != nil {
		t.Fatal(err)
	}
	c.InstanceDir = filepath.Join(c.DataDir, "instances")
	c.BackupDir = filepath.Join(c.DataDir, "backups")
	c.PublicURL = "http://127.0.0.1:1"
	c.JavaPaths = []string{"/fixture/java"}
	if err := c.Prepare(); err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(filepath.Join(c.DataDir, "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	vault, err := secrets.Open(filepath.Join(c.DataDir, "master.key"))
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	a := api.New(c, db, vault, &recoveryRuntime{}, "fixture", nil)
	if _, err := a.Auth.CreateUser(t.Context(), "owner", fixturePassword, "owner", true); err != nil {
		db.Close()
		t.Fatal(err)
	}
	i := model.Instance{ID: model.NewID(), Name: "Offline recovery fixture", JavaPath: c.JavaPaths[0], Jar: "launcher.jar", MinecraftVersion: "fixture", LoaderVersion: "fixture", MemoryMB: 1024, Port: 25565, RCONPort: 25575, RCONSecret: "fixture-rcon-secret", EULA: true, CreatedAt: time.Now().UTC()}
	i.Root = filepath.Join(c.InstanceDir, i.ID)
	putRecoveryFile(t, filepath.Join(i.Root, "world", "level.dat"), "original-world")
	putRecoveryFile(t, filepath.Join(i.Root, i.Jar), "fixture-launcher-never-executed")
	putRecoveryFile(t, filepath.Join(i.Root, "server.properties"), "level-name=world\nrcon.password=fixture-rcon-secret\n")
	if err := a.Instances.Save(t.Context(), i); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return c, i
}

func openRecoveryFixture(t *testing.T, c config.Config) (*offlinePanel, *recoveryRuntime) {
	t.Helper()
	p, err := openOfflinePanel(c)
	if err != nil {
		t.Fatal(err)
	}
	rt := &recoveryRuntime{}
	p.app.Runtime = rt
	p.app.Prepare = func(context.Context, model.Instance) error { return nil }
	p.app.Provision = func(context.Context, model.Instance, bool) error { return nil }
	t.Cleanup(func() { p.app.DB.Close(); p.root.Close(); p.release() })
	return p, rt
}

func putRecoveryFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func recoveryText(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func recoveryBackupID(t *testing.T, result any) string {
	t.Helper()
	j, ok := result.(jobs.Job)
	if !ok || j.State != "succeeded" {
		t.Fatalf("unexpected backup result: %#v", result)
	}
	var body struct {
		Backup struct {
			ID string `json:"id"`
		} `json:"backup"`
	}
	if err := json.Unmarshal(j.Result, &body); err != nil || !model.ValidID(body.Backup.ID) {
		t.Fatalf("invalid backup result: %s %v", j.Result, err)
	}
	return body.Backup.ID
}

func TestRecoveryValidation(t *testing.T) {
	id, backup := model.NewID(), model.NewID()
	for _, args := range [][]string{{}, {"list"}, {"list", id, "extra"}, {"create", "../escape"}, {"verify", id}, {"preview", id, strings.Repeat("A", 32)}, {"restore", id, backup}} {
		if err := validateBackupArgs(args, false); err == nil {
			t.Fatalf("accepted invalid or unconfirmed backup: %q", args)
		}
	}
	for _, args := range [][]string{{"filesystem", id, ".nesc-recovery-" + backup}, {"content", id, backup}, {"runtime", id}, {"file", id, backup}, {"owner", "owner"}} {
		if err := validateRecoveryArgs(args, false, args[0] == "owner"); err == nil {
			t.Fatalf("accepted unconfirmed recovery: %q", args)
		}
	}
	for _, args := range [][]string{{"list", id, "extra"}, {"content", id, "../escape"}, {"filesystem", id, backup}, {"filesystem", id, ".nesc-recovery-../escape"}, {"runtime", id, backup}, {"owner", "new user"}} {
		if err := validateRecoveryArgs(args, true, args[0] == "owner"); err == nil {
			t.Fatalf("accepted invalid recovery: %q", args)
		}
	}
	for _, args := range [][]string{{"version", "extra"}, {"version", "--code", "123456"}, {"serve", "--offline"}, {"login", "--confirm"}, {"login", "--password-stdin", "unexpected"}} {
		if err := run(args); err == nil {
			t.Fatalf("accepted invalid flags/count: %q", args)
		}
	}
	c := config.Default()
	c.DataDir = filepath.Join(t.TempDir(), "must-not-create")
	input := strings.NewReader("never-read-this-password\n")
	length := input.Len()
	if err := recoveryCommand(c, []string{"owner", "owner"}, false, true, input); err == nil || input.Len() != length {
		t.Fatal("unconfirmed owner recovery accepted or read credentials")
	}
	if err := offlineBackupCommand(c, []string{"restore", id, backup}, false); err == nil {
		t.Fatal("unconfirmed offline command accepted")
	}
	if _, err := os.Stat(c.DataDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("rejected recovery initialized state", err)
	}
}

func TestCLICredentialInput(t *testing.T) {
	for _, test := range []struct {
		input, code   string
		second, valid bool
	}{
		{" secret password \n", "", false, true}, {"password\r\n012345\r\n", "", true, true}, {"password\n", "012345", false, true},
		{"password\n012345\nextra\n", "", true, false}, {"password\n012345\n", "123456", true, false}, {"password\n", "", true, false},
		{"password\n", "+12345", false, false}, {"password\n", " 12345", false, false}, {strings.Repeat("p", 1025), "", false, false}, {"", "", false, false},
		{"password\xff\n", "", false, false},
	} {
		password, code, err := readCLICredentials(strings.NewReader(test.input), test.code, test.second)
		if (err == nil) != test.valid {
			t.Fatalf("credential validation mismatch for second=%t: %v", test.second, err)
		}
		if test.valid && (password == "" || test.second && code != "012345") {
			t.Fatal("credential input lost password/code")
		}
	}
	password, _, err := readCLICredentials(strings.NewReader(" secret password \n"), "", false)
	if err != nil || password != " secret password " {
		t.Fatal("password whitespace was changed")
	}
}

func TestCLIRequestsNeverRedirectCredentials(t *testing.T) {
	hits := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++; t.Error("redirect target received a request") }))
	defer target.Close()
	for _, status := range []int{301, 302, 303, 307, 308} {
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, status) }))
		c := config.Default()
		c.PublicURL = origin.URL
		err := apiRequest(c, &http.Client{}, cliSession{Token: auth.Token(), CSRF: auth.Token()}, "POST", "/backups", map[string]string{"password": "sensitive-fixture"}, nil)
		origin.Close()
		if err == nil || !strings.Contains(err.Error(), "redirect") {
			t.Fatalf("redirect %d was accepted: %v", status, err)
		}
	}
	if hits != 0 {
		t.Fatal("cookie/CSRF redirect leak")
	}
}

func fixtureCode(secret string) string {
	key, _ := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	var count [8]byte
	binary.BigEndian.PutUint64(count[:], uint64(time.Now().Unix()/30))
	h := hmac.New(sha1.New, key)
	h.Write(count[:])
	sum := h.Sum(nil)
	offset := sum[len(sum)-1] & 15
	return fmt.Sprintf("%06d", (binary.BigEndian.Uint32(sum[offset:offset+4])&0x7fffffff)%1000000)
}

func TestCLITOTPLoginAndProtectedSessionFiles(t *testing.T) {
	if unprivilegedRecoveryTest(t) {
		return
	}
	c, _ := recoveryFixture(t)
	p, _ := openRecoveryFixture(t, c)
	var owner string
	if err := p.app.DB.QueryRow("SELECT id FROM users WHERE username='owner'").Scan(&owner); err != nil {
		t.Fatal(err)
	}
	secret := auth.NewTOTP()
	if err := p.app.Auth.EnableTOTP(t.Context(), owner, fixturePassword, secret, fixtureCode(secret)); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(p.app.Router())
	defer server.Close()
	c.PublicURL = server.URL
	p.app.Config.PublicURL = server.URL
	if err := loginCLI(c, "owner", fixturePassword, ""); err == nil {
		t.Fatal("TOTP login succeeded without a code")
	}
	if err := loginCLI(c, "owner", fixturePassword, fixtureCode(secret)); err != nil {
		t.Fatal(err)
	}
	session, err := readCLISession(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.app.Auth.Session(t.Context(), session.Token); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(c.DataDir, "cli-session.json")
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("session mode is not private", err)
	}
	for _, unsafe := range []string{"symlink", "hardlink", "permissions", "oversized", "fifo"} {
		t.Run(unsafe, func(t *testing.T) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(c.DataDir, "outside-"+unsafe)
			putRecoveryFile(t, outside, "do not overwrite")
			switch unsafe {
			case "symlink":
				err = os.Symlink(outside, path)
			case "hardlink":
				err = os.Link(outside, path)
			case "permissions":
				err = os.WriteFile(path, []byte(`{}`), 0644)
			case "oversized":
				err = os.WriteFile(path, []byte(strings.Repeat("x", 4097)), 0600)
			case "fifo":
				err = syscall.Mkfifo(path, 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := readCLISession(c); err == nil {
				t.Fatal("unsafe session was read")
			}
			if unsafe != "oversized" {
				if err := writeCLISession(c, session); err == nil {
					t.Fatal("unsafe session was replaced")
				}
			}
			if recoveryText(t, outside) != "do not overwrite" {
				t.Fatal("outside data was overwritten")
			}
		})
	}
}

func TestOfflineBackupRestoreAndRetainedRecoveryWithoutHTTP(t *testing.T) {
	if unprivilegedRecoveryTest(t) {
		return
	}
	c, i := recoveryFixture(t)
	p, rt := openRecoveryFixture(t, c)
	unrelated, err := p.app.Jobs.Enqueue(t.Context(), i.ID, "start", map[string]any{}, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	created, err := p.backup(t.Context(), []string{"create", i.ID}, false)
	if err != nil {
		t.Fatal(err)
	}
	id := recoveryBackupID(t, created)
	listed, err := p.backup(t.Context(), []string{"list", i.ID}, false)
	if err != nil || len(listed.([]backups.Backup)) != 1 {
		t.Fatalf("offline list failed: %#v %v", listed, err)
	}
	for _, action := range []string{"verify", "preview"} {
		if _, err := p.backup(t.Context(), []string{action, i.ID, id}, false); err != nil {
			t.Fatal(action, err)
		}
	}
	world := filepath.Join(i.Root, "world", "level.dat")
	putRecoveryFile(t, world, "valuable-newer-world")
	if _, err := p.backup(t.Context(), []string{"restore", i.ID, id}, false); err == nil {
		t.Fatal("unconfirmed restore accepted")
	}
	if recoveryText(t, world) != "valuable-newer-world" {
		t.Fatal("unconfirmed restore changed data")
	}
	rt.running = true
	if _, err := p.backup(t.Context(), []string{"restore", i.ID, id}, true); err == nil {
		t.Fatal("offline restore accepted a running server")
	}
	rt.running = false
	restored, err := p.backup(t.Context(), []string{"restore", i.ID, id}, true)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Restore backups.RestoreResult `json:"restore"`
		Before  string                `json:"pre_restore_backup_id"`
	}
	if err := json.Unmarshal(restored.(jobs.Job).Result, &result); err != nil || !result.Restore.Installed || !model.ValidID(result.Before) {
		t.Fatalf("invalid restore result: %#v %v", result, err)
	}
	if recoveryText(t, world) != "original-world" {
		t.Fatal("offline restore did not restore selected data")
	}
	if recoveryText(t, filepath.Join(c.InstanceDir, result.Restore.Recovery, "world", "level.dat")) != "valuable-newer-world" {
		t.Fatal("restore discarded original data")
	}
	records, err := p.recoverInstance(t.Context(), []string{"list", i.ID}, false)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, record := range records.([]map[string]any) {
		found = found || record["kind"] == "filesystem" && record["id"] == result.Restore.Recovery
	}
	if !found {
		t.Fatal("retained filesystem record was not listed")
	}
	if _, err := p.backup(t.Context(), []string{"verify", i.ID, result.Before}, false); err != nil {
		t.Fatal("safety backup not verified", err)
	}
	if _, err := p.recoverInstance(t.Context(), []string{"filesystem", i.ID, result.Restore.Recovery}, false); err == nil {
		t.Fatal("unconfirmed filesystem recovery accepted")
	}
	changed := i
	changed.MemoryMB = 2048
	if err := p.app.Instances.Update(t.Context(), changed); err != nil {
		t.Fatal(err)
	}
	if _, err := p.recoverInstance(t.Context(), []string{"filesystem", i.ID, result.Restore.Recovery}, true); err == nil {
		t.Fatal("filesystem recovery bypassed different database/runtime identity")
	}
	if err := p.app.Instances.Update(t.Context(), i); err != nil {
		t.Fatal(err)
	}
	retainedWorld := filepath.Join(c.InstanceDir, result.Restore.Recovery, "world", "level.dat")
	putRecoveryFile(t, retainedWorld, "tampered-retained-world")
	if _, err := p.recoverInstance(t.Context(), []string{"filesystem", i.ID, result.Restore.Recovery}, true); err == nil {
		t.Fatal("changed retained tree accepted")
	}
	if recoveryText(t, world) != "original-world" || recoveryText(t, retainedWorld) != "tampered-retained-world" {
		t.Fatal("failed recovery changed either tree")
	}
	putRecoveryFile(t, retainedWorld, "valuable-newer-world")
	recovered, err := p.recoverInstance(t.Context(), []string{"filesystem", i.ID, result.Restore.Recovery}, true)
	if err != nil {
		t.Fatal(err)
	}
	if recoveryText(t, world) != "valuable-newer-world" {
		t.Fatal("retained filesystem tree was not recovered")
	}
	var recovery struct {
		Restore backups.RestoreResult `json:"restore"`
		Before  string                `json:"pre_recovery_backup_id"`
	}
	if err := json.Unmarshal(recovered.(jobs.Job).Result, &recovery); err != nil || !recovery.Restore.Installed || !model.ValidID(recovery.Before) {
		t.Fatal("invalid retained recovery result", err)
	}
	if recoveryText(t, filepath.Join(c.InstanceDir, recovery.Restore.Recovery, "world", "level.dat")) != "original-world" {
		t.Fatal("recovery discarded displaced data")
	}
	queued, err := p.app.Jobs.Get(t.Context(), unrelated.ID)
	if err != nil || queued.State != "queued" || rt.starts != 0 {
		t.Fatalf("offline commands drained unrelated work: %#v %v", queued, err)
	}
	var audit int
	if err := p.app.DB.QueryRow("SELECT count(*) FROM audit WHERE user_id=? AND action LIKE 'offline.%'", p.actor).Scan(&audit); err != nil || audit < 6 {
		t.Fatal("local operator actions were not audited", err)
	}
}

func TestOfflineRecoveryRequiresExistingPrivateStateAndPanelLock(t *testing.T) {
	if unprivilegedRecoveryTest(t) {
		return
	}
	for _, failure := range []string{"directory", "database", "blank-database", "key", "key-size", "key-mode", "database-link", "database-hardlink", "key-link", "key-hardlink", "uninitialized", "concurrent"} {
		t.Run(failure, func(t *testing.T) {
			c, _ := recoveryFixture(t)
			dbpath, keypath := filepath.Join(c.DataDir, "panel.db"), filepath.Join(c.DataDir, "master.key")
			switch failure {
			case "directory":
				c.DataDir += "-missing"
			case "database":
				if err := os.Remove(dbpath); err != nil {
					t.Fatal(err)
				}
			case "blank-database":
				if err := os.WriteFile(dbpath, nil, 0600); err != nil {
					t.Fatal(err)
				}
			case "key":
				if err := os.Remove(keypath); err != nil {
					t.Fatal(err)
				}
			case "key-size":
				putRecoveryFile(t, keypath, "short")
			case "key-mode":
				if err := os.Chmod(keypath, 0644); err != nil {
					t.Fatal(err)
				}
			case "database-link", "database-hardlink", "key-link", "key-hardlink":
				path := dbpath
				if strings.HasPrefix(failure, "key-") {
					path = keypath
				}
				outside := path + ".original"
				if err := os.Rename(path, outside); err != nil {
					t.Fatal(err)
				}
				var err error
				if strings.HasSuffix(failure, "hardlink") {
					err = os.Link(outside, path)
				} else {
					err = os.Symlink(outside, path)
				}
				if err != nil {
					t.Fatal(err)
				}
			case "uninitialized":
				if err := os.Remove(dbpath); err != nil {
					t.Fatal(err)
				}
				db, err := database.Open(dbpath)
				if err != nil {
					t.Fatal(err)
				}
				db.Close()
			case "concurrent":
				release, err := lock.Acquire(filepath.Join(c.DataDir, "panel.lock"))
				if err != nil {
					t.Fatal(err)
				}
				defer release()
			}
			p, err := openOfflinePanel(c)
			if err == nil {
				p.app.DB.Close()
				p.root.Close()
				p.release()
				t.Fatal("unsafe recovery state accepted", failure)
			}
			if failure == "database" || failure == "key" {
				path := dbpath
				if failure == "key" {
					path = keypath
				}
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("recovery created missing state", err)
				}
			}
			if failure == "blank-database" {
				if info, err := os.Stat(dbpath); err != nil || info.Size() != 0 {
					t.Fatal("recovery initialized an empty database", err)
				}
			}
		})
	}
}

func TestOfflineRecoveryRejectsUnknownIDsAndLinks(t *testing.T) {
	if unprivilegedRecoveryTest(t) {
		return
	}
	c, i := recoveryFixture(t)
	p, _ := openRecoveryFixture(t, c)
	for _, args := range [][]string{{"list", model.NewID()}, {"verify", i.ID, model.NewID()}, {"preview", i.ID, model.NewID()}, {"restore", i.ID, model.NewID()}} {
		if _, err := p.backup(t.Context(), args, true); err == nil {
			t.Fatal("unknown backup/instance accepted", args)
		}
	}
	for _, kind := range []string{"content", "file", "filesystem"} {
		id := model.NewID()
		if kind == "filesystem" {
			id = ".nesc-recovery-" + id
		}
		if _, err := p.recoverInstance(t.Context(), []string{kind, i.ID, id}, true); err == nil {
			t.Fatal("unknown recovery accepted", kind)
		}
	}
	outside := filepath.Join(c.DataDir, "untouched")
	putRecoveryFile(t, filepath.Join(outside, "keep"), "untouched")
	backupPath := filepath.Join(c.BackupDir, i.ID)
	if err := os.Symlink(outside, backupPath); err != nil {
		t.Fatal(err)
	}
	if _, err := p.backup(t.Context(), []string{"list", i.ID}, false); err == nil {
		t.Fatal("backup-root symlink accepted")
	}
	if _, err := p.backup(t.Context(), []string{"create", i.ID}, false); err == nil {
		t.Fatal("backup-root symlink accepted for writes")
	}
	if recoveryText(t, filepath.Join(outside, "keep")) != "untouched" {
		t.Fatal("outside data changed")
	}
	if err := os.Rename(i.Root, i.Root+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(i.Root+"-original", i.Root); err != nil {
		t.Fatal(err)
	}
	if _, err := p.backup(t.Context(), []string{"create", i.ID}, false); err == nil {
		t.Fatal("instance-root symlink accepted")
	}
}

func TestOfflineContentRollbackNeverRestoresWorlds(t *testing.T) {
	if unprivilegedRecoveryTest(t) {
		return
	}
	c, i := recoveryFixture(t)
	p, _ := openRecoveryFixture(t, c)
	id := model.NewID()
	original := ".newempire-content-" + id + "-original"
	for _, dir := range []string{"mods", original} {
		putRecoveryFile(t, filepath.Join(i.Root, dir, "manual.jar"), "unknown-manual-mod")
	}
	putRecoveryFile(t, filepath.Join(i.Root, "mods", "changed.jar"), "new-mod")
	putRecoveryFile(t, filepath.Join(i.Root, original, "changed.jar"), "old-mod")
	state := func(path, content string) modrinth.FileState {
		sum := sha512.Sum512([]byte(content))
		return modrinth.FileState{Path: path, Size: int64(len(content)), SHA512: hex.EncodeToString(sum[:]), Mode: 0600}
	}
	before := []modrinth.FileState{state("changed.jar", "old-mod"), state("manual.jar", "unknown-manual-mod")}
	after := []modrinth.FileState{state("changed.jar", "new-mod"), state("manual.jar", "unknown-manual-mod")}
	lock := []byte(`{"schema":1}`)
	receipt := map[string]any{"id": id, "state": "applied", "plan_hash": strings.Repeat("a", 128), "before": before, "after": after, "had_mods": true, "before_lock": nil, "after_lock": lock}
	b, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	putRecoveryFile(t, filepath.Join(i.Root, ".newempire-content-"+id+"-transaction.json"), string(b))
	putRecoveryFile(t, filepath.Join(i.Root, modrinth.LockFilename), string(lock))
	world := filepath.Join(i.Root, "world", "level.dat")
	putRecoveryFile(t, world, "new-world-progress-must-remain")
	if _, err := p.recoverInstance(t.Context(), []string{"content", i.ID, id}, false); err == nil {
		t.Fatal("unconfirmed content rollback accepted")
	}
	recovered, err := p.recoverInstance(t.Context(), []string{"content", i.ID, id}, true)
	if err != nil {
		t.Fatal(err)
	}
	if recoveryText(t, world) != "new-world-progress-must-remain" || recoveryText(t, filepath.Join(i.Root, "mods", "manual.jar")) != "unknown-manual-mod" || recoveryText(t, filepath.Join(i.Root, "mods", "changed.jar")) != "old-mod" {
		t.Fatal("content recovery rolled back unrelated data or failed to restore mods")
	}
	if recoveryText(t, filepath.Join(i.Root, ".newempire-content-"+id+"-discard", "changed.jar")) != "new-mod" {
		t.Fatal("displaced content was discarded")
	}
	root, err := os.OpenRoot(i.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if tx, err := modrinth.LoadTransaction(root, id); err != nil || tx.State != "rolled_back" {
		t.Fatal("receipt was not retained", err)
	}
	var result struct {
		Backup string `json:"pre_recovery_backup_id"`
	}
	if err := json.Unmarshal(recovered.(jobs.Job).Result, &result); err != nil || !model.ValidID(result.Backup) {
		t.Fatal("missing safety backup", err)
	}
	storage, err := p.backupRoot(i.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	if _, err := backups.Verify(t.Context(), storage, result.Backup, nil); err != nil {
		t.Fatal("content recovery safety backup is not verified", err)
	}
}

func TestOfflineTrashRecoveryDoesNotOverwriteNewerFiles(t *testing.T) {
	if unprivilegedRecoveryTest(t) {
		return
	}
	c, i := recoveryFixture(t)
	p, _ := openRecoveryFixture(t, c)
	path := filepath.Join(i.Root, "notes.txt")
	putRecoveryFile(t, path, "original-notes")
	j, err := p.runJob(t.Context(), i.ID, "file", map[string]any{"operation": "trash", "path": "notes.txt", "confirm": true})
	if err != nil {
		t.Fatal(err)
	}
	var entry struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(j.Result, &entry); err != nil || !model.ValidID(entry.Token) {
		t.Fatal("invalid trash token", err)
	}
	listed, err := p.recoverInstance(t.Context(), []string{"list", i.ID}, false)
	if err != nil || len(listed.([]map[string]any)) != 1 || listed.([]map[string]any)[0]["id"] != entry.Token {
		t.Fatalf("file recovery record was not listed: %#v %v", listed, err)
	}
	putRecoveryFile(t, path, "newer-notes")
	if _, err := p.recoverInstance(t.Context(), []string{"file", i.ID, entry.Token}, true); err == nil {
		t.Fatal("file recovery overwrote a newer file")
	}
	if recoveryText(t, path) != "newer-notes" {
		t.Fatal("failed file recovery changed newer data")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := p.recoverInstance(t.Context(), []string{"file", i.ID, entry.Token}, true); err != nil {
		t.Fatal(err)
	}
	if recoveryText(t, path) != "original-notes" || recoveryText(t, filepath.Join(i.Root, "world", "level.dat")) != "original-world" {
		t.Fatal("file recovery changed unrelated data or failed to recover the original")
	}
}

func TestOfflineRestoreMetadataReconciliationNeverSwapsWorlds(t *testing.T) {
	if unprivilegedRecoveryTest(t) {
		return
	}
	c, i := recoveryFixture(t)
	p, _ := openRecoveryFixture(t, c)
	created, err := p.backup(t.Context(), []string{"create", i.ID}, false)
	if err != nil {
		t.Fatal(err)
	}
	id := recoveryBackupID(t, created)
	current := i
	current.MemoryMB = 2048
	if err := p.app.Instances.Update(t.Context(), current); err != nil {
		t.Fatal(err)
	}
	world := filepath.Join(i.Root, "world", "level.dat")
	putRecoveryFile(t, world, "newer-world-before-failed-restore")
	p.app.Provision = func(context.Context, model.Instance, bool) error {
		return errors.New("controlled metadata helper failure")
	}
	if _, err := p.backup(t.Context(), []string{"restore", i.ID, id}, true); err == nil {
		t.Fatal("restore ignored helper reconfiguration failure")
	}
	if recoveryText(t, world) != "original-world" {
		t.Fatal("failed metadata step did not leave the already installed tree intact")
	}
	pending, err := p.pendingRestore(t.Context(), i.ID)
	if err != nil || pending == nil || pending.BackupID != id {
		t.Fatal("failed restore did not retain pending identity", err)
	}
	if _, err := p.backup(t.Context(), []string{"create", i.ID}, false); err == nil {
		t.Fatal("created misleading metadata backup before reconciliation")
	}
	if _, err := p.recoverInstance(t.Context(), []string{"filesystem", i.ID, pending.Restore.Recovery}, true); err == nil {
		t.Fatal("filesystem recovery bypassed pending metadata guard")
	}
	preview, err := p.recoverInstance(t.Context(), []string{"restore", i.ID}, false)
	if err != nil || preview.(map[string]any)["use_previous"] != false {
		t.Fatal("pending reconciliation preview failed", err)
	}
	if recoveryText(t, world) != "original-world" {
		t.Fatal("reconciliation preview changed world data")
	}
	p.app.Provision = func(context.Context, model.Instance, bool) error { return nil }
	if _, err := p.recoverInstance(t.Context(), []string{"restore-previous", i.ID}, true); err == nil {
		t.Fatal("reconciled previous metadata against the wrong current tree")
	}
	if _, err := p.recoverInstance(t.Context(), []string{"restore", i.ID}, true); err != nil {
		t.Fatal(err)
	}
	if pending, err := p.pendingRestore(t.Context(), i.ID); err != nil || pending != nil {
		t.Fatal("metadata guard was not cleared", err)
	}
	registered, err := p.app.Instances.Get(t.Context(), i.ID)
	if err != nil || registered.MemoryMB != i.MemoryMB {
		t.Fatal("database/runtime identity was not reconciled", err)
	}
	if recoveryText(t, world) != "original-world" {
		t.Fatal("metadata-only reconciliation swapped a world")
	}
}

func TestOfflineFilesystemSwapKeepsGuardIfCompletionCannotPersist(t *testing.T) {
	if unprivilegedRecoveryTest(t) {
		return
	}
	c, i := recoveryFixture(t)
	p, _ := openRecoveryFixture(t, c)
	created, err := p.backup(t.Context(), []string{"create", i.ID}, false)
	if err != nil {
		t.Fatal(err)
	}
	id := recoveryBackupID(t, created)
	world := filepath.Join(i.Root, "world", "level.dat")
	putRecoveryFile(t, world, "valuable-newer-world")
	restored, err := p.backup(t.Context(), []string{"restore", i.ID, id}, true)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Restore backups.RestoreResult `json:"restore"`
	}
	if err := json.Unmarshal(restored.(jobs.Job).Result, &result); err != nil || !result.Restore.Installed {
		t.Fatal("missing retained restore result", err)
	}
	if _, err := p.app.DB.Exec("CREATE TRIGGER retain_pending_guard BEFORE DELETE ON settings WHEN OLD.key='restore-pending:" + i.ID + "' BEGIN SELECT RAISE(ABORT,'controlled completion persistence failure'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.recoverInstance(t.Context(), []string{"filesystem", i.ID, result.Restore.Recovery}, true); err == nil {
		t.Fatal("failed guard completion was reported as success")
	}
	if recoveryText(t, world) != "valuable-newer-world" {
		t.Fatal("already installed recovery tree was rolled back automatically")
	}
	if pending, err := p.pendingRestore(t.Context(), i.ID); err != nil || pending == nil || !pending.Restore.Installed {
		t.Fatal("ambiguous completion lost its startup guard", err)
	}
	if _, err := p.app.DB.Exec("DROP TRIGGER retain_pending_guard"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.recoverInstance(t.Context(), []string{"restore", i.ID}, true); err != nil {
		t.Fatal(err)
	}
	if pending, err := p.pendingRestore(t.Context(), i.ID); err != nil || pending != nil {
		t.Fatal("verified explicit reconciliation did not clear the guard", err)
	}
	if recoveryText(t, world) != "valuable-newer-world" {
		t.Fatal("metadata-only reconciliation changed world progress")
	}
}

func TestOfflineOwnerRecoveryIsAtomicAndRevokesOwnerSessions(t *testing.T) {
	if unprivilegedRecoveryTest(t) {
		return
	}
	c, _ := recoveryFixture(t)
	p, _ := openRecoveryFixture(t, c)
	var owner string
	if err := p.app.DB.QueryRow("SELECT id FROM users WHERE username='owner'").Scan(&owner); err != nil {
		t.Fatal(err)
	}
	secret := auth.NewTOTP()
	encrypted, err := p.app.Vault.Seal([]byte(secret), "totp:"+owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.app.DB.Exec("UPDATE users SET totp=? WHERE id=?", encrypted, owner); err != nil {
		t.Fatal(err)
	}
	viewer, err := p.app.Auth.CreateUser(t.Context(), "viewer", fixturePassword, "viewer", false)
	if err != nil {
		t.Fatal(err)
	}
	other, err := p.app.Auth.CreateUser(t.Context(), "other-owner", fixturePassword, "owner", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, user := range []string{owner, viewer.ID, other.ID} {
		if _, err := p.app.DB.Exec("INSERT INTO sessions(token_hash,user_id,csrf,expires_at) VALUES(?,?,?,?)", auth.HashToken(auth.Token()), user, auth.Token(), time.Now().Add(time.Hour).Unix()); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct{ user, password string }{{"owner", "short"}, {"missing", fixturePassword}, {"viewer", fixturePassword}} {
		if err := p.recoverOwner(t.Context(), test.user, test.password); err == nil {
			t.Fatal("invalid owner recovery accepted", test.user)
		}
	}
	newPassword := "changed-strong-fixture-password"
	originalVault := p.app.Vault
	p.app.Vault, err = secrets.Open(filepath.Join(t.TempDir(), "different-master.key"))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.recoverOwner(t.Context(), "owner", newPassword); err == nil {
		t.Fatal("owner recovery accepted a mismatched vault")
	}
	p.app.Vault = originalVault
	if _, err := p.app.DB.Exec("CREATE TRIGGER reject_recovery_audit BEFORE INSERT ON audit WHEN NEW.action='offline.owner.recovered' BEGIN SELECT RAISE(ABORT,'fixture audit failure'); END"); err != nil {
		t.Fatal(err)
	}
	if err := p.recoverOwner(t.Context(), "owner", newPassword); err == nil {
		t.Fatal("owner recovery committed without its audit")
	}
	var originalHash string
	var stillEncrypted []byte
	if err := p.app.DB.QueryRow("SELECT password_hash,totp FROM users WHERE id=?", owner).Scan(&originalHash, &stillEncrypted); err != nil || !auth.VerifyPassword(originalHash, fixturePassword) || string(stillEncrypted) != string(encrypted) {
		t.Fatal("audit failure left partial owner credentials", err)
	}
	var survivingSessions int
	if err := p.app.DB.QueryRow("SELECT count(*) FROM sessions WHERE user_id IN (SELECT id FROM users WHERE role='owner')").Scan(&survivingSessions); err != nil || survivingSessions != 2 {
		t.Fatal("audit failure revoked sessions without committing credentials", err)
	}
	if _, err := p.app.DB.Exec("DROP TRIGGER reject_recovery_audit"); err != nil {
		t.Fatal(err)
	}
	if err := p.recoverOwner(t.Context(), "owner", newPassword); err != nil {
		t.Fatal(err)
	}
	var sessions, factors int
	if err := p.app.DB.QueryRow("SELECT count(*) FROM sessions WHERE user_id IN (SELECT id FROM users WHERE role='owner')").Scan(&sessions); err != nil || sessions != 0 {
		t.Fatal("owner sessions not revoked", err)
	}
	if err := p.app.DB.QueryRow("SELECT count(*) FROM sessions WHERE user_id=?", viewer.ID).Scan(&sessions); err != nil || sessions != 1 {
		t.Fatal("unrelated viewer session revoked", err)
	}
	if err := p.app.DB.QueryRow("SELECT count(*) FROM users WHERE id=? AND totp IS NOT NULL", owner).Scan(&factors); err != nil || factors != 0 {
		t.Fatal("factor not removed", err)
	}
	if _, err := p.app.Auth.Login(t.Context(), "owner", newPassword, ""); err != nil {
		t.Fatal("recovered owner cannot sign in", err)
	}
	var actor, detail string
	if err := p.app.DB.QueryRow("SELECT user_id,detail FROM audit WHERE action='offline.owner.recovered'").Scan(&actor, &detail); err != nil {
		t.Fatal(err)
	}
	if actor != p.actor || !strings.Contains(detail, "effective_uid") || strings.Contains(detail, newPassword) || strings.Contains(detail, secret) {
		t.Fatal("owner audit is missing operator identity or leaked credentials")
	}
}

func TestOfflineCancelledJobKeepsDurableIdentity(t *testing.T) {
	if unprivilegedRecoveryTest(t) {
		return
	}
	c, i := recoveryFixture(t)
	p, _ := openRecoveryFixture(t, c)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	p.app.Jobs.Register("recovery_cancellation_fixture", 0, func(ctx context.Context, task *jobs.Task, _ json.RawMessage) (any, error) {
		if err := task.Progress(ctx, 10, "Cancellation before mutation"); err != nil {
			return nil, err
		}
		cancel()
		return nil, ctx.Err()
	})
	j, err := p.runJob(ctx, i.ID, "recovery_cancellation_fixture", map[string]any{})
	if err == nil || !model.ValidID(j.ID) || j.State != "interrupted" {
		t.Fatalf("cancelled command lost durable job state: %#v %v", j, err)
	}
	if recoveryText(t, filepath.Join(i.Root, "world", "level.dat")) != "original-world" {
		t.Fatal("cancelled recovery mutated the world")
	}
}

func TestRootRecoveryRefusesBeforeCreatingState(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root-rejection fixture runs in the Linux sandbox")
	}
	c := config.Default()
	c.DataDir = filepath.Join(t.TempDir(), "missing")
	if _, err := openOfflinePanel(c); err == nil || !strings.Contains(err.Error(), "sudo -u new-empire-control") {
		t.Fatal("root recovery did not give run-as-panel-account guidance", err)
	}
	if _, err := os.Stat(c.DataDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("root recovery created state", err)
	}
}
