package api

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha1"
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
	"sync/atomic"
	"testing"

	"github.com/gabrielpgava/newempire-server-control/internal/fabric"
	"github.com/gabrielpgava/newempire-server-control/internal/model"
)

type creationTransport func(*http.Request) (*http.Response, error)

func (f creationTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func sourceBootstrap(t *testing.T, game, loader string) []byte {
	t.Helper()
	return sourceZIP(t, map[string][]byte{
		"META-INF/MANIFEST.MF":                        []byte("Manifest-Version: 1.0\r\nMain-Class: net.fabricmc.installer.ServerLauncher\r\n\r\n"),
		"install.properties":                          []byte("game-version=" + game + "\nfabric-loader-version=" + loader + "\n"),
		"net/fabricmc/installer/ServerLauncher.class": {0xca, 0xfe, 0xba, 0xbe, 0, 0, 0, 52},
	})
}

func sourceZIP(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var data bytes.Buffer
	writer := zip.NewWriter(&data)
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		file, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(entries[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func creationJava(t *testing.T, major int) string {
	t.Helper()
	name := filepath.Join(t.TempDir(), "java")
	program := fmt.Sprintf("#!/bin/sh\n[ \"$#\" -eq 1 ] && [ \"$1\" = '-version' ] || exit 1\nprintf 'openjdk version \"%d\"\\n' >&2\n", major)
	if err := os.WriteFile(name, []byte(program), 0700); err != nil {
		t.Fatal(err)
	}
	return name
}

func creationHarness(t *testing.T) (*harness, string, string) {
	t.Helper()
	h := newHarness(t, nil)
	high, low := creationJava(t, 99), creationJava(t, 17)
	h.app.Config.JavaPaths = []string{high, low}
	h.app.Instances.Config.JavaPaths = h.app.Config.JavaPaths
	metadata := []byte(`{"id":"source-game","javaVersion":{"majorVersion":23},"downloads":{"server":{"url":"https://piston-data.mojang.com/v1/objects/fixture/server.jar","sha1":"0123456789abcdef0123456789abcdef01234567","size":42}}}`)
	digest := sha1.Sum(metadata)
	routes := map[string][]byte{
		"https://meta.fabricmc.net/v2/versions/game":                                       []byte(`[{"version":"source-game","stable":true}]`),
		"https://meta.fabricmc.net/v2/versions/loader":                                     []byte(`[{"version":"0.99.0","stable":true,"maven":"net.fabricmc:fabric-loader:0.99.0"}]`),
		"https://meta.fabricmc.net/v2/versions/installer":                                  []byte(`[{"version":"9.9.0","stable":true,"maven":"net.fabricmc:fabric-installer:9.9.0","url":"https://maven.fabricmc.net/net/fabricmc/fabric-installer/9.9.0/fabric-installer-9.9.0.jar"}]`),
		"https://meta.fabricmc.net/v2/versions/loader/source-game":                         []byte(`[{"loader":{"version":"0.99.0","stable":true,"maven":"net.fabricmc:fabric-loader:0.99.0"},"intermediary":{"version":"source-game","maven":"net.fabricmc:intermediary:source-game"},"launcherMeta":{"min_java_version":21}}]`),
		"https://piston-meta.mojang.com/mc/game/version_manifest_v2.json":                  []byte(fmt.Sprintf(`{"versions":[{"id":"source-game","url":"https://piston-meta.mojang.com/v1/packages/fixture/game.json","sha1":"%s"}]}`, hex.EncodeToString(digest[:]))),
		"https://piston-meta.mojang.com/v1/packages/fixture/game.json":                     metadata,
		"https://meta.fabricmc.net/v2/versions/loader/source-game/0.99.0/9.9.0/server/jar": sourceBootstrap(t, "source-game", "0.99.0"),
	}
	h.app.Fabric = fabric.NewClient(&http.Client{Transport: creationTransport(func(r *http.Request) (*http.Response, error) {
		body, ok := routes[r.URL.String()]
		if !ok {
			return nil, errors.New("unexpected source metadata request")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body)), Header: http.Header{}, Request: r}, nil
	})})
	return h, high, low
}

func sourceFiles(t *testing.T) map[string][]byte {
	t.Helper()
	return map[string][]byte{
		"launcher.jar":                     sourceBootstrap(t, "source-game", "0.99.0"),
		"server.properties":                []byte("# Preserve this configuration\nunknown-key=preserved\nlevel-name=Realm\nwhite-list=true\nonline-mode=true\nserver-port=30001\nrcon.port=30002\nenable-rcon=true\nrcon.password=dummy-source-rcon-password\n"),
		"eula.txt":                         []byte("eula=true\n"),
		"Realm/level.dat":                  []byte("original world progress"),
		"whitelist.json":                   []byte("[]\n"),
		"mods/external.jar":                []byte("preserve unknown mod bytes"),
		"libraries/example/dependency.jar": []byte("preserve library bytes"),
	}
}

func stageSourceUpload(t *testing.T, h *harness, data []byte) string {
	t.Helper()
	req := httptest.NewRequest("POST", "http://127.0.0.1:8080/api/v1/uploads/", nil)
	req.Header.Set("Origin", h.app.Config.PublicURL)
	req.Header.Set("X-CSRF-Token", h.session.CSRF)
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Upload-Length", strconv.Itoa(len(data)))
	req.Header.Set("Upload-Metadata", "filename "+base64.StdEncoding.EncodeToString([]byte("source.zip")))
	req.AddCookie(&http.Cookie{Name: "nec_session", Value: h.session.Token})
	response := httptest.NewRecorder()
	h.handler.ServeHTTP(response, req)
	if response.Code != 201 {
		t.Fatalf("stage source: %d %s", response.Code, response.Body.String())
	}
	location := response.Header().Get("Location")
	req = httptest.NewRequest("PATCH", location, bytes.NewReader(data))
	req.Header.Set("Origin", h.app.Config.PublicURL)
	req.Header.Set("X-CSRF-Token", h.session.CSRF)
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Upload-Offset", "0")
	req.Header.Set("Content-Type", "application/offset+octet-stream")
	req.AddCookie(&http.Cookie{Name: "nec_session", Value: h.session.Token})
	response = httptest.NewRecorder()
	h.handler.ServeHTTP(response, req)
	if response.Code != 204 {
		t.Fatalf("write source: %d %s", response.Code, response.Body.String())
	}
	return path.Base(location)
}

func sourceRequest(java string, preview importPreview) createRequest {
	return createRequest{Mode: "import", Name: "Imported survival", JavaPath: java, Jar: preview.Launchers[0], MinecraftVersion: preview.MinecraftVersion, LoaderVersion: preview.LoaderVersion, MemoryMB: 2048, Port: 30001, RCONPort: 30002, EULA: true, PreviewHash: preview.PreviewHash, ServerRoot: preview.ServerRoot, ReservationID: preview.ReservationID}
}

func writeAdoptionSource(t *testing.T, h *harness, entries map[string][]byte) string {
	t.Helper()
	base := filepath.Join(h.app.Config.DataDir, "external")
	root := filepath.Join(base, "server")
	for name, data := range entries {
		filename := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(filename), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	h.app.Config.AdoptRoots = []string{base}
	h.app.Instances.Config.AdoptRoots = []string{base}
	return root
}

func assertSourceBytes(t *testing.T, root string, entries map[string][]byte) {
	t.Helper()
	for name, expected := range entries {
		actual, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil || !bytes.Equal(actual, expected) {
			t.Fatalf("source file changed: %s: %v", name, err)
		}
	}
}

func TestCreationFreshFabricAndRequestValidationBeforeProvision(t *testing.T) {
	h, high, low := creationHarness(t)
	var calls atomic.Int32
	h.app.Provision = func(_ context.Context, i model.Instance, _ bool) error {
		calls.Add(1)
		if _, err := os.Stat(i.Root); !errors.Is(err, os.ErrNotExist) {
			return errors.New("provision must create the initially empty managed root")
		}
		return os.Mkdir(i.Root, 0770)
	}
	body := createRequest{Mode: "create", Name: "Fresh Fabric", JavaPath: high, MinecraftVersion: "source-game", LoaderVersion: "0.99.0", InstallerVersion: "9.9.0", MemoryMB: 2048, Port: 30001, RCONPort: 30002, EULA: true}
	for _, name := range []string{" Survival", "Survival ", "\tSurvival", "Survival\nserver"} {
		bad := body
		bad.Name = name
		decodeResponse[map[string]string](t, h.request(t, "POST", "/instances", bad, &h.session, true), 400)
	}
	bad := body
	bad.Jar = "..launcher.jar"
	decodeResponse[map[string]string](t, h.request(t, "POST", "/instances", bad, &h.session, true), 400)
	bad = body
	bad.Name, bad.Jar = "Old queued name ", "launcher.jar"
	queued, err := h.app.Jobs.Enqueue(t.Context(), model.NewID(), "instance_create", bad, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if job := h.wait(t, queued.ID); job.State != "failed" || calls.Load() != 0 {
		t.Fatalf("an older invalid queued request reached provisioning: %+v", job)
	}
	bad = body
	bad.JavaPath = low
	job := h.queue(t, "/instances", bad)
	if job.State != "failed" || !strings.Contains(job.Error, "Java 23") || calls.Load() != 0 {
		t.Fatalf("incompatible create reached provisioning: %+v calls=%d", job, calls.Load())
	}
	job = h.queue(t, "/instances", body)
	mustSucceed(t, job)
	i, err := h.app.Instances.Get(t.Context(), job.InstanceID)
	if err != nil || calls.Load() != 1 {
		t.Fatalf("create registration: %+v %v calls=%d", i, err, calls.Load())
	}
	for _, name := range []string{"fabric-server-launch.jar", "server.properties", "eula.txt"} {
		if _, err := os.Stat(filepath.Join(i.Root, name)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInitialImportUsesReviewedServerSubtree(t *testing.T) {
	for _, wrapper := range []string{".", "backup/server"} {
		t.Run(wrapper, func(t *testing.T) {
			h, high, _ := creationHarness(t)
			original := sourceFiles(t)
			entries := make(map[string][]byte)
			var expectedBytes int64
			for name, data := range original {
				entries[path.Join(wrapper, name)] = data
				expectedBytes += int64(len(data))
			}
			if wrapper != "." {
				entries["outside.txt"] = []byte("do not install outside the selected subtree")
			}
			archive := sourceZIP(t, entries)
			upload := stageSourceUpload(t, h, archive)
			p := decodeResponse[importPreview](t, h.request(t, "POST", "/imports/preview", importRequest{UploadID: upload}, &h.session, true), 200)
			if p.ServerRoot != wrapper || p.MinecraftVersion != "source-game" || p.LoaderVersion != "0.99.0" || p.JavaRequired != 23 || p.TotalBytes != expectedBytes || !slices.Equal(p.Launchers, []string{"launcher.jar"}) || !slices.Equal(p.Worlds, []string{"Realm"}) {
				t.Fatalf("wrong source preview: %+v", p)
			}
			for _, e := range p.Files {
				if e.Path == "outside.txt" || strings.HasPrefix(e.Path, "backup/") {
					t.Fatal("preview includes files outside the rebased subtree")
				}
			}
			body := sourceRequest(high, p)
			body.UploadID = upload
			job := h.queue(t, "/instances", body)
			mustSucceed(t, job)
			i, err := h.app.Instances.Get(t.Context(), job.InstanceID)
			if err != nil || i.Jar != "launcher.jar" {
				t.Fatalf("wrong imported registration: %+v %v", i, err)
			}
			for _, name := range []string{"Realm/level.dat", "mods/external.jar", "libraries/example/dependency.jar", "whitelist.json", "launcher.jar"} {
				data, err := os.ReadFile(filepath.Join(i.Root, filepath.FromSlash(name)))
				if err != nil || !bytes.Equal(data, original[name]) {
					t.Fatalf("import changed %s: %v", name, err)
				}
			}
			props, err := os.ReadFile(filepath.Join(i.Root, "server.properties"))
			if err != nil || !strings.Contains(string(props), "# Preserve this configuration") {
				t.Fatalf("lost properties: %s %v", props, err)
			}
			values, err := fabric.ReadProperties(props)
			if err != nil || values["level-name"] != "Realm" || values["white-list"] != "true" || values["unknown-key"] != "preserved" || values["rcon.password"] != i.RCONSecret {
				t.Fatalf("wrong active configuration: %+v %v", values, err)
			}
			for _, name := range []string{"outside.txt", "backup", "world"} {
				if _, err := os.Stat(filepath.Join(i.Root, name)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("unexpected import path: %s: %v", name, err)
				}
			}
			staged, err := h.app.Uploads.Lookup(t.Context(), h.session.User.ID, upload)
			if err != nil {
				t.Fatal(err)
			}
			defer staged.File.Close()
			unchanged, err := io.ReadAll(staged.File)
			if err != nil || !bytes.Equal(unchanged, archive) {
				t.Fatal("import changed its uploaded source")
			}
		})
	}
}

func TestInitialImportRejectsAmbiguousAndUnverifiableSources(t *testing.T) {
	for _, mode := range []string{"two-roots", "two-launchers", "legacy", "unknown-runtime", "unsafe-world"} {
		t.Run(mode, func(t *testing.T) {
			h, _, _ := creationHarness(t)
			var calls atomic.Int32
			h.app.Provision = func(context.Context, model.Instance, bool) error { calls.Add(1); return nil }
			entries := sourceFiles(t)
			switch mode {
			case "two-roots":
				for name, data := range sourceFiles(t) {
					entries["another/"+name] = data
				}
			case "two-launchers":
				entries["second.jar"] = entries["launcher.jar"]
			case "legacy":
				entries["launcher.jar"] = []byte("legacy/custom launcher")
			case "unknown-runtime":
				entries["launcher.jar"] = sourceBootstrap(t, "unlisted-game", "0.99.0")
			case "unsafe-world":
				entries["server.properties"] = []byte("level-name=../other-world\n")
			}
			upload := stageSourceUpload(t, h, sourceZIP(t, entries))
			decodeResponse[map[string]string](t, h.request(t, "POST", "/imports/preview", importRequest{UploadID: upload}, &h.session, true), 400)
			if calls.Load() != 0 {
				t.Fatal("invalid source reached provisioning")
			}
		})
	}
}

func TestInitialImportAndAdoptionRejectInvalidChoicesBeforeProvision(t *testing.T) {
	for _, mode := range []string{"import", "adopt"} {
		for _, failure := range []string{"java", "game", "loader", "launcher", "server-root", "preview", "changed-source", "missing-source"} {
			t.Run(mode+"/"+failure, func(t *testing.T) {
				h, high, low := creationHarness(t)
				var calls atomic.Int32
				h.app.Provision = func(context.Context, model.Instance, bool) error {
					calls.Add(1)
					return errors.New("unexpected provisioning")
				}
				entries := sourceFiles(t)
				var p importPreview
				var sourcePath, upload string
				if mode == "adopt" {
					sourcePath = writeAdoptionSource(t, h, entries)
					p = decodeResponse[importPreview](t, h.request(t, "POST", "/adoptions/preview", map[string]string{"path": sourcePath}, &h.session, true), 200)
				} else {
					upload = stageSourceUpload(t, h, sourceZIP(t, entries))
					p = decodeResponse[importPreview](t, h.request(t, "POST", "/imports/preview", importRequest{UploadID: upload}, &h.session, true), 200)
				}
				body := sourceRequest(high, p)
				body.Mode, body.Path, body.UploadID = mode, sourcePath, upload
				switch failure {
				case "java":
					body.JavaPath = low
				case "game":
					body.MinecraftVersion = "wrong-claim"
				case "loader":
					body.LoaderVersion = "wrong-claim"
				case "launcher":
					body.Jar = "mods/external.jar"
				case "server-root":
					body.ServerRoot = "wrong-root"
				case "preview":
					body.PreviewHash = "changed-preview"
				case "changed-source":
					entries["Realm/level.dat"] = []byte("changed source world")
					if mode == "adopt" {
						if err := os.WriteFile(filepath.Join(sourcePath, "Realm", "level.dat"), entries["Realm/level.dat"], 0600); err != nil {
							t.Fatal(err)
						}
					} else {
						body.UploadID = stageSourceUpload(t, h, sourceZIP(t, entries))
					}
				case "missing-source":
					if mode == "adopt" {
						retained := sourcePath + "-retained"
						if err := os.Rename(sourcePath, retained); err != nil {
							t.Fatal(err)
						}
						sourcePath = retained
					} else {
						body.UploadID = model.NewID()
					}
				}
				response := h.request(t, "POST", "/instances", body, &h.session, true)
				if response.Code == 202 {
					var queued struct {
						ID string `json:"id"`
					}
					if err := json.Unmarshal(response.Body.Bytes(), &queued); err != nil {
						t.Fatal(err)
					}
					job := h.wait(t, queued.ID)
					if job.State != "failed" {
						t.Fatalf("accepted invalid source: %+v", job)
					}
					if failure == "java" && !strings.Contains(job.Error, "Java 23") {
						t.Fatalf("wrong required Java check: %s", job.Error)
					}
				} else if response.Code != 400 {
					t.Fatalf("unexpected preflight status: %d %s", response.Code, response.Body.String())
				}
				if calls.Load() != 0 {
					t.Fatal("invalid source reached provisioning")
				}
				if mode == "adopt" {
					assertSourceBytes(t, sourcePath, entries)
				}
			})
		}
	}
}

func TestAdoptionReservationPersistsAndRetriesTheSameRegistration(t *testing.T) {
	h, high, _ := creationHarness(t)
	entries := sourceFiles(t)
	root := writeAdoptionSource(t, h, entries)
	preview := func(id string) importPreview {
		return decodeResponse[importPreview](t, h.request(t, "POST", "/adoptions/preview", map[string]string{"path": root, "reservation_id": id}, &h.session, true), 200)
	}
	p := preview("")
	if !model.ValidID(p.ReservationID) || p.ServiceAccount != "nec-mc-"+p.ReservationID[:24] || preview("").ReservationID != p.ReservationID || preview(p.ReservationID).ReservationID != p.ReservationID {
		t.Fatalf("adoption identity was not reserved: %+v", p)
	}
	body := sourceRequest(high, p)
	body.Mode, body.Path = "adopt", root
	var mu sync.Mutex
	var attempts []model.Instance
	h.app.Provision = func(_ context.Context, i model.Instance, reconfigure bool) error {
		mu.Lock()
		defer mu.Unlock()
		if reconfigure {
			return errors.New("adoption retry must replay provision, not reconfigure")
		}
		attempts = append(attempts, i)
		switch len(attempts) {
		case 1:
			return errors.New("fixture: dedicated account access not prepared")
		case 2:
			return errors.New("fixture: lost reply after root registration")
		}
		return nil
	}
	for attempt := 1; attempt <= 3; attempt++ {
		job := h.queue(t, "/instances", body)
		if job.InstanceID != p.ReservationID {
			t.Fatal("retry allocated another dedicated identity")
		}
		if attempt < 3 && job.State != "failed" || attempt == 3 && job.State != "succeeded" {
			t.Fatalf("wrong retry outcome: %+v", job)
		}
		if attempt == 1 {
			var raw string
			if err := h.app.DB.QueryRowContext(t.Context(), "SELECT value FROM settings WHERE key=?", "adoption-reservation:"+p.ReservationID).Scan(&raw); err != nil || strings.Contains(raw, "dummy-source-rcon-password") {
				t.Fatalf("reservation was not durably stored without credentials: %v", err)
			}
			if err := os.Chmod(filepath.Join(root, "server.properties"), 0640); err != nil {
				t.Fatal(err)
			}
			if next := preview(""); next.ReservationID != p.ReservationID || next.PreviewHash != p.PreviewHash {
				t.Fatal("permission preparation lost the persisted reservation")
			}
			changed := body
			changed.MemoryMB += 512
			decodeResponse[map[string]string](t, h.request(t, "POST", "/instances", changed, &h.session, true), 400)
			changed = body
			changed.Port += 10
			decodeResponse[map[string]string](t, h.request(t, "POST", "/instances", changed, &h.session, true), 400)
		}
	}
	mustSucceed(t, h.queue(t, "/instances", body))
	mu.Lock()
	defer mu.Unlock()
	if len(attempts) != 3 {
		t.Fatalf("completed adoption was provisioned again: %d", len(attempts))
	}
	for _, i := range attempts[1:] {
		if etagJSON(i) != etagJSON(attempts[0]) || i.RCONSecret != attempts[0].RCONSecret {
			t.Fatal("retry changed registration identity, creation time, ports, or credential")
		}
	}
	assertSourceBytes(t, root, entries)
	info, err := os.Stat(filepath.Join(root, "server.properties"))
	if err != nil || info.Mode().Perm() != 0640 {
		t.Fatal("adoption changed externally prepared permissions")
	}
}

func TestAdoptionPreflightCanCorrectJavaWithoutReallocatingIdentity(t *testing.T) {
	h, high, low := creationHarness(t)
	entries := sourceFiles(t)
	root := writeAdoptionSource(t, h, entries)
	p := decodeResponse[importPreview](t, h.request(t, "POST", "/adoptions/preview", map[string]string{"path": root}, &h.session, true), 200)
	body := sourceRequest(low, p)
	body.Mode, body.Path = "adopt", root
	var calls atomic.Int32
	h.app.Provision = func(context.Context, model.Instance, bool) error { calls.Add(1); return nil }
	job := h.queue(t, "/instances", body)
	if job.State != "failed" || calls.Load() != 0 {
		t.Fatalf("incompatible Java reached provisioning: %+v", job)
	}
	r, err := h.app.adoptionReservation(t.Context(), h.session.User.ID, root, p.PreviewHash, p.ReservationID)
	if err != nil || r.Instance != nil {
		t.Fatalf("failed preflight froze the incompatible configuration: %+v %v", r, err)
	}
	body.JavaPath = high
	job = h.queue(t, "/instances", body)
	mustSucceed(t, job)
	if job.InstanceID != p.ReservationID || calls.Load() != 1 {
		t.Fatal("corrected Java did not use the same reserved instance")
	}
	assertSourceBytes(t, root, entries)
}

func TestAdoptionReservationRejectsAnotherOwnerPathAndHash(t *testing.T) {
	h, high, _ := creationHarness(t)
	root := writeAdoptionSource(t, h, sourceFiles(t))
	p := decodeResponse[importPreview](t, h.request(t, "POST", "/adoptions/preview", map[string]string{"path": root}, &h.session, true), 200)
	body := sourceRequest(high, p)
	body.Mode, body.Path = "adopt", root
	otherUser, err := h.app.Auth.CreateUser(t.Context(), "other-admin", "controlled-password", "admin", false)
	if err != nil {
		t.Fatal(err)
	}
	other, err := h.app.Auth.Login(t.Context(), otherUser.Username, "controlled-password", "")
	if err != nil {
		t.Fatal(err)
	}
	decodeResponse[map[string]string](t, h.request(t, "POST", "/instances", body, &other, true), 400)
	for _, mutate := range []func(*createRequest){
		func(b *createRequest) { b.Path += "-other" },
		func(b *createRequest) { b.PreviewHash = "changed" },
		func(b *createRequest) { b.ReservationID = model.NewID() },
		func(b *createRequest) { b.ReservationID = "" },
	} {
		bad := body
		mutate(&bad)
		decodeResponse[map[string]string](t, h.request(t, "POST", "/instances", bad, &h.session, true), 400)
	}
	if _, err := h.app.adoptionReservation(t.Context(), h.session.User.ID, root, p.PreviewHash, p.ReservationID); err != nil {
		t.Fatal("invalid requests damaged the original reservation", err)
	}
}
