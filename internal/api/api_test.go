package api

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gabrielpgava/newempire-server-control/internal/auth"
	"github.com/gabrielpgava/newempire-server-control/internal/config"
	"github.com/gabrielpgava/newempire-server-control/internal/database"
	"github.com/gabrielpgava/newempire-server-control/internal/jobs"
	"github.com/gabrielpgava/newempire-server-control/internal/model"
	"github.com/gabrielpgava/newempire-server-control/internal/modrinth"
	"github.com/gabrielpgava/newempire-server-control/internal/secrets"
)

type fakeRuntime struct {
	mu         sync.Mutex
	ready      bool
	failStart  bool
	commandErr error
	commands   []string
}

func (f *fakeRuntime) Status(context.Context, model.Instance) (model.RuntimeStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	state := "offline"
	pid := 0
	if f.ready {
		state = "running"
		pid = 123
	}
	return model.RuntimeStatus{State: state, Ready: f.ready, PID: pid, Players: []string{}}, nil
}
func (f *fakeRuntime) Start(context.Context, model.Instance) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failStart {
		return errors.New("controlled startup failure")
	}
	f.ready = true
	return nil
}
func (f *fakeRuntime) Stop(context.Context, model.Instance) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ready = false
	return nil
}
func (f *fakeRuntime) Restart(ctx context.Context, i model.Instance) error {
	if err := f.Stop(ctx, i); err != nil {
		return err
	}
	return f.Start(ctx, i)
}
func (f *fakeRuntime) Logs(context.Context, model.Instance, int) ([]model.LogLine, error) {
	return []model.LogLine{{Time: time.Now(), Message: "fixture dummy-rcon-secret"}}, nil
}
func (f *fakeRuntime) Command(_ context.Context, i model.Instance, command string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, command)
	if command == "stop" {
		f.ready = false
	}
	return "Controlled command accepted " + i.RCONSecret, f.commandErr
}

type harness struct {
	app      *App
	handler  http.Handler
	runtime  *fakeRuntime
	session  auth.Session
	instance model.Instance
}

func newHarness(t *testing.T, content *modrinth.Service) *harness {
	t.Helper()
	dir := t.TempDir()
	c := config.Default()
	c.DataDir = dir
	c.InstanceDir = filepath.Join(dir, "instances")
	c.BackupDir = filepath.Join(dir, "backups")
	c.JavaPaths = []string{"/usr/bin/java"}
	c.MaxUploadBytes = 8 << 20
	if err := c.Prepare(); err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(filepath.Join(dir, "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	vault, err := secrets.Open(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	rt := &fakeRuntime{}
	a := New(c, db, vault, rt, "0.1.0", nil)
	a.Content = content
	a.Provision = func(_ context.Context, i model.Instance, _ bool) error { return os.MkdirAll(i.Root, 0770) }
	a.Prepare = func(context.Context, model.Instance) error { return nil }
	ctx, cancel := context.WithCancel(context.Background())
	if err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); a.Wait(); db.Close() })
	if _, err := a.Auth.CreateUser(ctx, "owner", "controlled-password", "owner", true); err != nil {
		t.Fatal(err)
	}
	session, err := a.Auth.Login(ctx, "owner", "controlled-password", "")
	if err != nil {
		t.Fatal(err)
	}
	i := model.Instance{ID: model.NewID(), Name: "Integration fixture", JavaPath: "/usr/bin/java", Jar: "launcher.jar", MinecraftVersion: "fixture", LoaderVersion: "fixture", MemoryMB: 1024, Port: 25565, RCONPort: 25575, RCONSecret: "dummy-rcon-secret", EULA: true, CreatedAt: time.Now().UTC()}
	i.Root = filepath.Join(c.InstanceDir, i.ID)
	if err := os.Mkdir(i.Root, 0770); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(i.Root, "world"), 0770); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(i.Root, "world", "level.dat"), []byte("controlled-world-progress"), 0660)
	os.WriteFile(filepath.Join(i.Root, "server.properties"), []byte("# Keep this comment\nunknown-key=preserved\nrcon.password=dummy-rcon-secret\nlevel-name=world\n"), 0660)
	if err := a.Instances.Save(ctx, i); err != nil {
		t.Fatal(err)
	}
	return &harness{app: a, handler: a.Router(), runtime: rt, session: session, instance: i}
}
func (h *harness) request(t *testing.T, method, path string, body any, session *auth.Session, csrf bool) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	req := httptest.NewRequest(method, "http://127.0.0.1:8080/api/v1"+path, reader)
	req.RemoteAddr = "127.0.0.1:2345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", h.app.Config.PublicURL)
	if session != nil {
		req.AddCookie(&http.Cookie{Name: "nec_session", Value: session.Token})
		if csrf {
			req.Header.Set("X-CSRF-Token", session.CSRF)
		}
	}
	response := httptest.NewRecorder()
	h.handler.ServeHTTP(response, req)
	return response
}
func decodeResponse[T any](t *testing.T, response *httptest.ResponseRecorder, status int) T {
	t.Helper()
	if response.Code != status {
		t.Fatalf("HTTP %d, want %d: %s", response.Code, status, response.Body.String())
	}
	var out T
	if err := json.Unmarshal(response.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func (h *harness) wait(t *testing.T, id string) jobs.Job {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		j, err := h.app.Jobs.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if j.State != "queued" && j.State != "running" {
			return j
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("job did not reach a terminal state")
	return jobs.Job{}
}
func (h *harness) queue(t *testing.T, path string, body any) jobs.Job {
	t.Helper()
	response := h.request(t, "POST", path, body, &h.session, true)
	queued := decodeResponse[jobs.Job](t, response, 202)
	return h.wait(t, queued.ID)
}
func mustSucceed(t *testing.T, j jobs.Job) {
	t.Helper()
	if j.State != "succeeded" {
		t.Fatalf("job %s failed (%s): %s", j.Kind, j.State, j.Error)
	}
}

func TestAuthenticationCSRFIsolationAndSecretRedaction(t *testing.T) {
	h := newHarness(t, nil)
	base := "/instances/" + h.instance.ID
	if r := h.request(t, "GET", "/instances", nil, nil, false); r.Code != 401 {
		t.Fatalf("unauthenticated list returned %d", r.Code)
	}
	if r := h.request(t, "POST", base+"/actions/start", map[string]any{}, &h.session, false); r.Code != 403 {
		t.Fatal("missing CSRF was accepted")
	}
	viewer, err := h.app.Auth.CreateUser(context.Background(), "viewer", "controlled-password", "viewer", false)
	if err != nil {
		t.Fatal(err)
	}
	viewSession, err := h.app.Auth.Login(context.Background(), "viewer", "controlled-password", "")
	if err != nil {
		t.Fatal(err)
	}
	visible := decodeResponse[[]model.Instance](t, h.request(t, "GET", "/instances", nil, &viewSession, false), 200)
	if len(visible) != 0 {
		t.Fatal("viewer saw an ungranted instance")
	}
	if _, err := h.app.DB.Exec("INSERT INTO instance_permissions VALUES(?,?,?)", viewer.ID, h.instance.ID, "viewer"); err != nil {
		t.Fatal(err)
	}
	if r := h.request(t, "POST", base+"/actions/start", map[string]any{}, &viewSession, true); r.Code != 403 {
		t.Fatal("viewer could start the instance")
	}
	for _, endpoint := range []string{base, base + "/logs", base + "/properties"} {
		response := h.request(t, "GET", endpoint, nil, &h.session, false)
		if response.Code != 200 {
			t.Fatalf("%s: %d %s", endpoint, response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), h.instance.RCONSecret) {
			t.Fatalf("credential leaked through %s", endpoint)
		}
	}
	if r := h.request(t, "GET", base+"/file?path=server.properties", nil, &h.session, false); r.Code != 400 {
		t.Fatal("raw RCON configuration was downloadable")
	}
	if r := h.request(t, "GET", base+"/file?path=..%2Fmaster.key", nil, &h.session, false); r.Code != 400 {
		t.Fatal("path traversal accepted")
	}
	if r := h.request(t, "POST", "/setup", map[string]string{"token": "wrong", "username": "another", "password": "controlled-password"}, nil, false); r.Code == 201 {
		t.Fatal("bootstrap reopened after owner creation")
	}
}

func TestFileEditRecoveryBackupRangeAndRestore(t *testing.T) {
	h := newHarness(t, nil)
	base := "/instances/" + h.instance.ID
	os.WriteFile(filepath.Join(h.instance.Root, "config.txt"), []byte("original"), 0660)
	preview := decodeResponse[map[string]string](t, h.request(t, "GET", base+"/file?path=config.txt&preview=true", nil, &h.session, false), 200)
	stale := h.queue(t, base+"/files", map[string]any{"operation": "write", "path": "config.txt", "content": "bad", "etag": "stale", "confirm": true})
	if stale.State != "failed" {
		t.Fatal("stale configuration edit was accepted")
	}
	edit := h.queue(t, base+"/files", map[string]any{"operation": "write", "path": "config.txt", "content": "changed", "etag": preview["etag"], "confirm": true})
	mustSucceed(t, edit)
	trash := h.queue(t, base+"/files", map[string]any{"operation": "trash", "path": "config.txt", "confirm": true})
	mustSucceed(t, trash)
	var entry recoveryEntry
	if err := json.Unmarshal(trash.Result, &entry); err != nil {
		t.Fatal(err)
	}
	recover := h.queue(t, base+"/files", map[string]any{"operation": "recover", "token": entry.Token, "confirm": true})
	mustSucceed(t, recover)
	backup := h.queue(t, base+"/backups", map[string]any{"note": "Controlled restore fixture"})
	mustSucceed(t, backup)
	var result struct {
		Backup struct {
			ID string `json:"id"`
		} `json:"backup"`
	}
	if err := json.Unmarshal(backup.Result, &result); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", "http://127.0.0.1:8080/api/v1"+base+"/backups/"+result.Backup.ID+"/download", nil)
	request.AddCookie(&http.Cookie{Name: "nec_session", Value: h.session.Token})
	request.Header.Set("Range", "bytes=0-9")
	response := httptest.NewRecorder()
	h.handler.ServeHTTP(response, request)
	if response.Code != 206 || response.Body.Len() != 10 {
		t.Fatalf("range download failed: %d", response.Code)
	}
	os.WriteFile(filepath.Join(h.instance.Root, "world", "level.dat"), []byte("newer-world-progress"), 0660)
	restore := h.queue(t, base+"/backups/"+result.Backup.ID+"/restore", map[string]bool{"confirm": true})
	mustSucceed(t, restore)
	world, err := os.ReadFile(filepath.Join(h.instance.Root, "world", "level.dat"))
	if err != nil || string(world) != "controlled-world-progress" {
		t.Fatalf("explicit restore failed: %s %v", world, err)
	}
	var records int
	if err := h.app.DB.QueryRow("SELECT count(*) FROM audit WHERE instance_id=?", h.instance.ID).Scan(&records); err != nil || records < 6 {
		t.Fatal("mutations lack audit history")
	}
}

func TestOnlineBackupAlwaysReenablesSavingAndOfflineMutationsRejectRunning(t *testing.T) {
	h := newHarness(t, nil)
	h.runtime.mu.Lock()
	h.runtime.ready = true
	h.runtime.mu.Unlock()
	base := "/instances/" + h.instance.ID
	j := h.queue(t, base+"/files", map[string]any{"operation": "mkdir", "path": "unsafe-while-running"})
	if j.State != "failed" {
		t.Fatal("changed a running server directory")
	}
	os.Symlink(t.TempDir(), filepath.Join(h.instance.Root, "unsafe-link"))
	j = h.queue(t, base+"/backups", map[string]any{"online": true})
	if j.State != "failed" {
		t.Fatal("unsafe online source was backed up")
	}
	h.runtime.mu.Lock()
	commands := append([]string{}, h.runtime.commands...)
	h.runtime.mu.Unlock()
	if !slices.Contains(commands, "save-off") || !slices.Contains(commands, "save-all flush") || commands[len(commands)-1] != "save-on" {
		t.Fatalf("save sequence did not recover after failure: %v", commands)
	}
}

func TestPropertiesWorldsPlayersAndEncryptedSettings(t *testing.T) {
	h := newHarness(t, nil)
	base := "/instances/" + h.instance.ID
	response := h.request(t, "PUT", base+"/properties", map[string]any{"properties": map[string]string{"difficulty": "hard"}, "confirm": true}, &h.session, true)
	job := decodeResponse[jobs.Job](t, response, 202)
	mustSucceed(t, h.wait(t, job.ID))
	raw, _ := os.ReadFile(filepath.Join(h.instance.Root, "server.properties"))
	if !bytes.Contains(raw, []byte("# Keep this comment")) || !bytes.Contains(raw, []byte("unknown-key=preserved")) {
		t.Fatal("properties lost unknown keys or comments")
	}
	if r := h.request(t, "PUT", base+"/properties", map[string]any{"properties": map[string]string{"rcon.password": "leak"}, "confirm": true}, &h.session, true); r.Code != 400 {
		t.Fatal("credential change bypassed safe configuration")
	}
	mustSucceed(t, h.queue(t, base+"/worlds", map[string]any{"operation": "create", "name": "new-world", "confirm": true}))
	mustSucceed(t, h.queue(t, base+"/worlds", map[string]any{"operation": "activate", "name": "new-world", "confirm": true}))
	if _, err := playerCommand(playerRequest{Action: "op", Player: "Tester"}, false); err == nil {
		t.Fatal("operator could grant OP")
	}
	if _, err := playerCommand(playerRequest{Action: "kick", Player: "Tester\nstop"}, true); err == nil {
		t.Fatal("multiline player target accepted")
	}
	settings := map[string]any{"storage": map[string]any{"enabled": true, "endpoint": "https://account.r2.cloudflarestorage.com", "region": "auto", "bucket": "fixture-bucket", "access_key": "dummy-access-key", "secret_key": "dummy-secret-key"}}
	decodeResponse[map[string]bool](t, h.request(t, "PUT", "/settings", settings, &h.session, true), 200)
	decodeResponse[map[string]bool](t, h.request(t, "PUT", "/settings", map[string]any{"storage": map[string]bool{"enabled": false}}, &h.session, true), 200)
	var stored []byte
	if err := h.app.DB.QueryRow("SELECT value FROM secrets WHERE key='storage'").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored, []byte("dummy-secret-key")) {
		t.Fatal("credential was not encrypted at rest")
	}
	var cfg map[string]any
	if err := h.app.loadSecret(context.Background(), "storage", &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["secret_key"] != "dummy-secret-key" || cfg["enabled"] != false {
		t.Fatal("partial settings update destroyed credentials")
	}
	r := h.request(t, "GET", "/settings", nil, &h.session, false)
	if strings.Contains(r.Body.String(), "dummy-secret-key") || strings.Contains(r.Body.String(), "dummy-access-key") {
		t.Fatal("settings leaked credentials")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func fixtureContent(t *testing.T) *modrinth.Service {
	t.Helper()
	jar := []byte("controlled fake JAR bytes")
	sum := sha512.Sum512(jar)
	hash := hex.EncodeToString(sum[:])
	version := map[string]any{"id": "version1", "project_id": "project1", "version_number": "1.0.0", "version_type": "release", "date_published": "2026-01-01T00:00:00Z", "game_versions": []string{"fixture"}, "loaders": []string{"fabric"}, "environment": "server_only", "dependencies": []any{}, "files": []any{map[string]any{"filename": "fixture.jar", "size": len(jar), "primary": true, "url": "https://cdn.modrinth.com/data/project1/versions/version1/fixture.jar", "hashes": map[string]string{"sha512": hash}}}}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var payload any
		switch {
		case r.URL.Host == "cdn.modrinth.com":
			return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(jar)), Header: make(http.Header), ContentLength: int64(len(jar)), Request: r}, nil
		case r.URL.Path == "/v2/version_files" || r.URL.Path == "/v2/version_files/update":
			payload = map[string]any{}
		case r.URL.Path == "/v2/project/project1":
			payload = map[string]any{"id": "project1", "slug": "fixture", "title": "Controlled fixture mod", "project_type": "mod", "server_side": "required", "environment": []string{"server_only"}, "loaders": []string{"fabric"}}
		case strings.HasSuffix(r.URL.Path, "/version"):
			payload = []any{version}
		case r.URL.Path == "/v2/version/version1":
			payload = version
		default:
			return nil, fmt.Errorf("unexpected fixture upstream request: %s", r.URL.Path)
		}
		b, _ := json.Marshal(payload)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(b)), Header: make(http.Header), ContentLength: int64(len(b)), Request: r}, nil
	})}
	return modrinth.NewService(modrinth.NewClient(client))
}

func TestReviewedContentFailureRollsBackModsNeverWorlds(t *testing.T) {
	h := newHarness(t, fixtureContent(t))
	base := "/instances/" + h.instance.ID
	os.Mkdir(filepath.Join(h.instance.Root, "mods"), 0770)
	os.WriteFile(filepath.Join(h.instance.Root, "mods", "external.jar"), []byte("unknown external bytes"), 0660)
	plan := decodeResponse[struct {
		Hash    string   `json:"plan_hash"`
		Blocked []string `json:"blocked"`
	}](t, h.request(t, "POST", base+"/mods/plan", map[string]any{"projects": []string{"project1"}}, &h.session, true), 200)
	if len(plan.Blocked) > 0 {
		t.Fatal(plan.Blocked)
	}
	j := h.queue(t, base+"/mods/apply", map[string]any{"projects": []string{"project1"}, "plan_hash": plan.Hash, "confirm": true})
	mustSucceed(t, j)
	if _, err := os.Stat(filepath.Join(h.instance.Root, "mods", "fixture.jar")); err != nil {
		t.Fatal(err)
	}
	h.runtime.mu.Lock()
	h.runtime.failStart = true
	h.runtime.mu.Unlock()
	start := h.queue(t, base+"/actions/start", map[string]any{})
	if start.State != "failed" {
		t.Fatal("controlled startup failure reported success")
	}
	if _, err := os.Stat(filepath.Join(h.instance.Root, "mods", "fixture.jar")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed content was not rolled back")
	}
	unknown, _ := os.ReadFile(filepath.Join(h.instance.Root, "mods", "external.jar"))
	world, _ := os.ReadFile(filepath.Join(h.instance.Root, "world", "level.dat"))
	if string(unknown) != "unknown external bytes" || string(world) != "controlled-world-progress" {
		t.Fatal("rollback touched unknown content or world data")
	}
}

func TestArchiveImportPreviewAndCommitRejectStaleState(t *testing.T) {
	h := newHarness(t, nil)
	base := "/instances/" + h.instance.ID
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	file, _ := writer.Create("config/new.txt")
	file.Write([]byte("imported config"))
	writer.Close()
	req := httptest.NewRequest("POST", "http://127.0.0.1:8080/api/v1/uploads/", nil)
	req.Header.Set("Origin", h.app.Config.PublicURL)
	req.Header.Set("X-CSRF-Token", h.session.CSRF)
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Upload-Length", strconv.Itoa(archive.Len()))
	req.Header.Set("Upload-Metadata", "filename "+base64.StdEncoding.EncodeToString([]byte("fixture.zip")))
	req.AddCookie(&http.Cookie{Name: "nec_session", Value: h.session.Token})
	response := httptest.NewRecorder()
	h.handler.ServeHTTP(response, req)
	if response.Code != 201 {
		t.Fatalf("tus create: %d %s", response.Code, response.Body.String())
	}
	location := response.Header().Get("Location")
	req = httptest.NewRequest("PATCH", location, bytes.NewReader(archive.Bytes()))
	req.Header.Set("Origin", h.app.Config.PublicURL)
	req.Header.Set("X-CSRF-Token", h.session.CSRF)
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Upload-Offset", "0")
	req.Header.Set("Content-Type", "application/offset+octet-stream")
	req.AddCookie(&http.Cookie{Name: "nec_session", Value: h.session.Token})
	response = httptest.NewRecorder()
	h.handler.ServeHTTP(response, req)
	if response.Code != 204 {
		t.Fatalf("tus patch: %d %s", response.Code, response.Body.String())
	}
	id := path.Base(location)
	preview := decodeResponse[importPreview](t, h.request(t, "POST", base+"/imports/preview", map[string]string{"upload_id": id}, &h.session, true), 200)
	os.WriteFile(filepath.Join(h.instance.Root, "changed.txt"), []byte("changed after preview"), 0660)
	j := h.queue(t, base+"/imports/apply", map[string]any{"upload_id": id, "preview_hash": preview.PreviewHash, "confirm": true})
	if j.State != "failed" {
		t.Fatal("stale import was committed")
	}
	preview = decodeResponse[importPreview](t, h.request(t, "POST", base+"/imports/preview", map[string]string{"upload_id": id}, &h.session, true), 200)
	mustSucceed(t, h.queue(t, base+"/imports/apply", map[string]any{"upload_id": id, "preview_hash": preview.PreviewHash, "confirm": true}))
	content, err := os.ReadFile(filepath.Join(h.instance.Root, "config", "new.txt"))
	if err != nil || string(content) != "imported config" {
		t.Fatalf("import not committed: %s %v", content, err)
	}
	world, _ := os.ReadFile(filepath.Join(h.instance.Root, "world", "level.dat"))
	if string(world) != "controlled-world-progress" {
		t.Fatal("merge import lost original world")
	}
}
