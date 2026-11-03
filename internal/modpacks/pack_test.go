package modpacks

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
	"testing"

	"github.com/gabrielpgava/newempire-server-control/internal/modrinth"
)

var testTarget = modrinth.Target{MinecraftVersion: "test-game", Loader: "fabric", LoaderVersion: "test-loader"}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type fixture struct {
	versions map[string]modrinth.Version
	projects map[string]modrinth.Project
	data     map[string]string
}

func newFixture() *fixture {
	return &fixture{versions: map[string]modrinth.Version{}, projects: map[string]modrinth.Project{}, data: map[string]string{}}
}
func (f *fixture) add(project, version, data string) File {
	h1 := sha1.Sum([]byte(data))
	url := "https://cdn.modrinth.com/data/" + project + "/versions/" + version + "/" + project + ".jar"
	file := modrinth.File{Filename: project + ".jar", Size: int64(len(data)), URL: url, Primary: true, Hashes: map[string]string{"sha512": hashBytes([]byte(data)), "sha1": hex.EncodeToString(h1[:])}}
	f.versions[version] = modrinth.Version{ID: version, ProjectID: project, VersionType: "release", Environment: "server_only", GameVersions: []string{testTarget.MinecraftVersion}, Loaders: []string{"fabric"}, Files: []modrinth.File{file}}
	f.projects[project] = modrinth.Project{ID: project, ProjectType: "mod", ServerSide: "required"}
	f.data[url] = data
	return File{Path: "mods/" + file.Filename, Hashes: file.Hashes, Env: &Environment{Client: "optional", Server: "required"}, Downloads: []string{url}, FileSize: file.Size}
}
func (f *fixture) client(t *testing.T) *modrinth.Client {
	t.Helper()
	response := func(status int, v any) *http.Response {
		data, _ := json.Marshal(v)
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(data)), ContentLength: int64(len(data))}
	}
	return modrinth.NewClient(&http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "cdn.modrinth.com" {
			data, ok := f.data[r.URL.String()]
			if !ok {
				return response(404, nil), nil
			}
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(data)), ContentLength: int64(len(data))}, nil
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v2/"), "/")
		if r.Method == "POST" && r.URL.Path == "/v2/version_files" {
			var body struct {
				Hashes []string `json:"hashes"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			result := map[string]modrinth.Version{}
			for _, h := range body.Hashes {
				for _, v := range f.versions {
					for _, file := range v.Files {
						if file.Hashes["sha512"] == h {
							result[h] = v
						}
					}
				}
			}
			return response(200, result), nil
		}
		if len(parts) == 2 && parts[0] == "project" {
			if p, ok := f.projects[parts[1]]; ok {
				return response(200, p), nil
			}
			return response(404, nil), nil
		}
		if len(parts) == 2 && parts[0] == "version" {
			if v, ok := f.versions[parts[1]]; ok {
				return response(200, v), nil
			}
			return response(404, nil), nil
		}
		if len(parts) == 3 && parts[0] == "project" && parts[2] == "version" {
			versions := []modrinth.Version{}
			for _, v := range f.versions {
				if v.ProjectID == parts[1] {
					versions = append(versions, v)
				}
			}
			return response(200, versions), nil
		}
		t.Errorf("unexpected endpoint %s", r.URL)
		return response(404, nil), nil
	})})
}
func testIndex(files ...File) Index {
	return Index{FormatVersion: 1, Game: "minecraft", VersionID: "pack-1", Name: "Test server pack", Files: files, Dependencies: map[string]string{"minecraft": testTarget.MinecraftVersion, "fabric-loader": testTarget.LoaderVersion}}
}
func archiveBytes(t *testing.T, index Index, entries map[string]string) []byte {
	t.Helper()
	var data bytes.Buffer
	z := zip.NewWriter(&data)
	b, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	file, err := z.Create("modrinth.index.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.Write(b); err != nil {
		t.Fatal(err)
	}
	for _, name := range sortedKeys(entries) {
		w, err := z.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = w.Write([]byte(entries[name])); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}
func readPack(t *testing.T, data []byte) *Pack {
	t.Helper()
	p, err := Read(bytes.NewReader(data), int64(len(data)), testTarget)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func testRoot(t *testing.T) *os.Root {
	t.Helper()
	r, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}
func put(t *testing.T, r *os.Root, name, data string) {
	t.Helper()
	if err := r.MkdirAll(path.Dir(name), 0750); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteFile(name, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}
}
func readText(t *testing.T, r *os.Root, name string) string {
	t.Helper()
	data, err := r.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
func absent(t *testing.T, r *os.Root, name string) {
	t.Helper()
	if _, err := r.Lstat(name); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("expected %s absent; got %v", name, err)
	}
}

func TestServerOnlyPackImportPreservesLocalConfigAndRoundTrips(t *testing.T) {
	f := newFixture()
	a := f.add("A", "v1", "jar A")
	b := f.add("B", "v2", "jar B")
	clientOnly := f.add("C", "v3", "client-only JAR")
	clientOnly.Env = &Environment{Client: "required", Server: "unsupported"}
	v := f.versions["v1"]
	v.Dependencies = []modrinth.Dependency{{ProjectID: "B", VersionID: "v2", Type: "required"}}
	f.versions[v.ID] = v
	p := readPack(t, archiveBytes(t, testIndex(a, b, clientOnly), map[string]string{"overrides/config/new.toml": "generic", "server-overrides/config/new.toml": "server", "server-overrides/config/local.toml": "pack default", "client-overrides/options.txt": "client options"}))
	root := testRoot(t)
	put(t, root, "config/local.toml", "local settings")
	put(t, root, "world/level.dat", "world data")
	service := modrinth.NewService(f.client(t))
	plan, err := p.Plan(context.Background(), service, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Blockers) > 0 || len(plan.Files) != 2 || len(plan.Config) != 1 {
		t.Fatalf("plan=%+v", plan)
	}
	second, err := p.Plan(context.Background(), service, root)
	if err != nil {
		t.Fatal(err)
	}
	aJSON, _ := json.Marshal(plan)
	bJSON, _ := json.Marshal(second)
	if string(aJSON) != string(bJSON) {
		t.Fatal("pack plans not stable")
	}
	stage := testRoot(t)
	if err := p.Stage(context.Background(), service, stage, plan); err != nil {
		t.Fatal(err)
	}
	tx, err := modrinth.Apply(context.Background(), root, stage, plan)
	if err != nil {
		t.Fatal(err)
	}
	if readText(t, root, "mods/A.jar") != "jar A" || readText(t, root, "mods/B.jar") != "jar B" || readText(t, root, "config/new.toml") != "server" || readText(t, root, "config/local.toml") != "local settings" {
		t.Fatal("wrong server-only import or override precedence")
	}
	absent(t, root, "mods/C.jar")
	absent(t, root, "options.txt")
	var exported bytes.Buffer
	if err := Export(context.Background(), root, &exported, ExportOptions{Target: testTarget, Name: "Exported server", VersionID: "export-1", ConfigPaths: []string{"config/new.toml"}}); err != nil {
		t.Fatal(err)
	}
	z, err := zip.NewReader(bytes.NewReader(exported.Bytes()), int64(exported.Len()))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range z.File {
		if strings.HasSuffix(entry.Name, ".jar") || strings.Contains(entry.Name, "local.toml") {
			t.Fatal("export redistributed a JAR or unselected config")
		}
	}
	rebuilt := readPack(t, exported.Bytes())
	if len(rebuilt.Index.Files) != 2 {
		t.Fatal(rebuilt.Index)
	}
	for _, file := range rebuilt.Index.Files {
		if file.Env.Server != "required" || file.Env.Client != "unsupported" {
			t.Fatal("not server-only")
		}
	}
	newRoot := testRoot(t)
	rebuildPlan, err := rebuilt.Plan(context.Background(), service, newRoot)
	if err != nil || len(rebuildPlan.Blockers) > 0 {
		t.Fatal(rebuildPlan, err)
	}
	newStage := testRoot(t)
	if err := rebuilt.Stage(context.Background(), service, newStage, rebuildPlan); err != nil {
		t.Fatal(err)
	}
	rebuildTx, err := modrinth.Apply(context.Background(), newRoot, newStage, rebuildPlan)
	if err != nil {
		t.Fatal(err)
	}
	if readText(t, newRoot, "config/new.toml") != "server" {
		t.Fatal("round-trip lost override")
	}
	if err := modrinth.Finalize(newRoot, rebuildTx); err != nil {
		t.Fatal(err)
	}
	put(t, root, "world/level.dat", "new world state")
	if err := modrinth.Rollback(context.Background(), root, tx); err != nil {
		t.Fatal(err)
	}
	absent(t, root, "mods")
	absent(t, root, "config/new.toml")
	if readText(t, root, "config/local.toml") != "local settings" || readText(t, root, "world/level.dat") != "new world state" {
		t.Fatal("rollback touched local config/world")
	}
}

func TestPackRejectsUnsafeArchivesAndRuntimeRequirements(t *testing.T) {
	f := newFixture()
	file := f.add("A", "v1", "jar")
	for _, name := range []string{"../escape", "overrides/config/../../world/a", "server-overrides/config\\evil", "overrides/mods/embedded.jar", "overrides/world/level.dat", "overrides/options.txt", "server-overrides/config/a:stream"} {
		t.Run(name, func(t *testing.T) {
			data := archiveBytes(t, testIndex(file), map[string]string{name: "evil"})
			if _, err := Read(bytes.NewReader(data), int64(len(data)), testTarget); err == nil {
				t.Fatal("unsafe archive accepted")
			}
		})
	}
	for _, kind := range []string{"minecraft", "fabric", "forge", "missing runtime", "environment", "hash", "path", "mirror", "negative size"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture()
			file := f.add("A", "v1", "jar")
			index := testIndex(file)
			switch kind {
			case "minecraft":
				index.Dependencies["minecraft"] = "other"
			case "fabric":
				index.Dependencies["fabric-loader"] = "other"
			case "forge":
				index.Dependencies["forge"] = "x"
			case "missing runtime":
				delete(index.Dependencies, "fabric-loader")
			case "environment":
				index.Files[0].Env.Server = "unknown"
			case "hash":
				index.Files[0].Hashes["sha512"] = "bad"
			case "path":
				index.Files[0].Path = "mods/../world/a.jar"
			case "mirror":
				index.Files[0].Downloads = []string{"https://example.com/A.jar"}
			case "negative size":
				index.Files[0].FileSize = -1
			}
			data := archiveBytes(t, index, nil)
			if _, err := Read(bytes.NewReader(data), int64(len(data)), testTarget); err == nil {
				t.Fatal("invalid index accepted")
			}
		})
	}
	t.Run("symlink", func(t *testing.T) {
		var data bytes.Buffer
		z := zip.NewWriter(&data)
		h := &zip.FileHeader{Name: "overrides/config/link"}
		h.SetMode(os.ModeSymlink | 0777)
		w, err := z.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte("../../../world"))
		if err := z.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := Read(bytes.NewReader(data.Bytes()), int64(data.Len()), testTarget); err == nil {
			t.Fatal("archive symlink accepted")
		}
	})
	t.Run("zip bomb", func(t *testing.T) {
		data := archiveBytes(t, testIndex(), map[string]string{"overrides/config/large": strings.Repeat("A", 2<<20)})
		if _, err := Read(bytes.NewReader(data), int64(len(data)), testTarget); err == nil {
			t.Fatal("excessive compression ratio accepted")
		}
	})
	t.Run("duplicate", func(t *testing.T) {
		data := archiveBytes(t, testIndex(), map[string]string{"modrinth.index.json": "{}"})
		if _, err := Read(bytes.NewReader(data), int64(len(data)), testTarget); err == nil {
			t.Fatal("duplicate index accepted")
		}
	})
	t.Run("case-colliding overrides", func(t *testing.T) {
		data := archiveBytes(t, testIndex(), map[string]string{"overrides/config/Example.toml": "generic", "server-overrides/config/example.toml": "server"})
		if _, err := Read(bytes.NewReader(data), int64(len(data)), testTarget); err == nil {
			t.Fatal("case-colliding override paths accepted")
		}
	})
	t.Run("file and directory collision", func(t *testing.T) {
		data := archiveBytes(t, testIndex(), map[string]string{"overrides/config/a": "file", "server-overrides/config/a/b": "child"})
		if _, err := Read(bytes.NewReader(data), int64(len(data)), testTarget); err == nil {
			t.Fatal("config path hierarchy collision accepted")
		}
	})
	t.Run("count", func(t *testing.T) {
		entries := map[string]string{}
		for i := 0; i < modrinth.MaxFiles; i++ {
			entries["client-overrides/"+string(rune('a'+i))+".txt"] = ""
		}
		data := archiveBytes(t, testIndex(), entries)
		if _, err := Read(bytes.NewReader(data), int64(len(data)), testTarget); err == nil {
			t.Fatal("entry count limit ignored")
		}
	})
}

func TestReconstructionBlocksUnknownClientOnlyConflictsAndMissingLocks(t *testing.T) {
	for _, kind := range []string{"unknown", "client-only", "legacy unknown", "missing dependency", "wrong dependency", "incompatible", "changed hashes", "changed URL", "duplicate project"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture()
			a := f.add("A", "v1", "jar A")
			b := f.add("B", "v2", "jar B")
			index := testIndex(a, b)
			v := f.versions["v1"]
			switch kind {
			case "unknown":
				delete(f.versions, "v1")
			case "client-only":
				v.Environment = "client_only"
			case "legacy unknown":
				v.Environment = ""
				p := f.projects["A"]
				p.ServerSide = "unknown"
				f.projects["A"] = p
			case "missing dependency":
				v.Dependencies = []modrinth.Dependency{{ProjectID: "B", VersionID: "v2", Type: "required"}}
				index.Files = index.Files[:1]
			case "wrong dependency":
				f.add("B", "v3", "other B")
				v.Dependencies = []modrinth.Dependency{{ProjectID: "B", VersionID: "v3", Type: "required"}}
			case "incompatible":
				v.Dependencies = []modrinth.Dependency{{ProjectID: "B", Type: "incompatible"}}
			case "changed hashes":
				copyHashes := map[string]string{}
				for key, value := range index.Files[0].Hashes {
					copyHashes[key] = value
				}
				copyHashes["sha1"] = strings.Repeat("a", 40)
				index.Files[0].Hashes = copyHashes
			case "changed URL":
				index.Files[0].Downloads = []string{"https://cdn.modrinth.com/data/A/different.jar"}
			case "duplicate project":
				a2 := f.add("A", "v3", "new A")
				a2.Path = "mods/other.jar"
				index.Files = append(index.Files, a2)
			}
			if kind != "unknown" {
				f.versions[v.ID] = v
			}
			pack := readPack(t, archiveBytes(t, index, nil))
			service := modrinth.NewService(f.client(t))
			plan, err := pack.Plan(context.Background(), service, testRoot(t))
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.Blockers) == 0 {
				t.Fatal("unsafe reconstruction accepted", plan)
			}
			if err := pack.Stage(context.Background(), service, testRoot(t), plan); err == nil {
				t.Fatal("blocked pack staged")
			}
		})
	}
}

func TestExportRefusesUnmanagedModifiedAndUnselectedSensitiveFiles(t *testing.T) {
	f := newFixture()
	file := f.add("A", "v1", "jar")
	pack := readPack(t, archiveBytes(t, testIndex(file), nil))
	service := modrinth.NewService(f.client(t))
	root := testRoot(t)
	plan, err := pack.Plan(context.Background(), service, root)
	if err != nil {
		t.Fatal(err)
	}
	stage := testRoot(t)
	if err := pack.Stage(context.Background(), service, stage, plan); err != nil {
		t.Fatal(err)
	}
	tx, err := modrinth.Apply(context.Background(), root, stage, plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := modrinth.Finalize(root, tx); err != nil {
		t.Fatal(err)
	}
	opts := ExportOptions{Target: testTarget, Name: "Server", VersionID: "v1"}
	put(t, root, "mods/unknown.jar", "unknown")
	if err := Export(context.Background(), root, io.Discard, opts); err == nil {
		t.Fatal("unmanaged binary exported")
	}
	if err := root.Remove("mods/unknown.jar"); err != nil {
		t.Fatal(err)
	}
	put(t, root, "mods/A.jar", "bad")
	if err := Export(context.Background(), root, io.Discard, opts); err == nil {
		t.Fatal("modified binary exported")
	}
	put(t, root, "mods/A.jar", "jar")
	opts.ConfigPaths = []string{"world/level.dat"}
	if err := Export(context.Background(), root, io.Discard, opts); err == nil {
		t.Fatal("world data exported as overrides")
	}
	opts.ConfigPaths = nil
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Export(ctx, root, io.Discard, opts); !errors.Is(err, context.Canceled) {
		t.Fatal("export ignored cancellation", err)
	}
}

func TestLocalConfigAppearanceAndSymlinksFailClosed(t *testing.T) {
	pack := readPack(t, archiveBytes(t, testIndex(), map[string]string{"server-overrides/config/new.toml": "default"}))
	f := newFixture()
	service := modrinth.NewService(f.client(t))
	root := testRoot(t)
	plan, err := pack.Plan(context.Background(), service, root)
	if err != nil {
		t.Fatal(err)
	}
	stage := testRoot(t)
	if err := pack.Stage(context.Background(), service, stage, plan); err != nil {
		t.Fatal(err)
	}
	put(t, root, "config/new.toml", "local")
	if _, err := modrinth.Apply(context.Background(), root, stage, plan); err == nil {
		t.Fatal("new local config overwritten")
	}
	if readText(t, root, "config/new.toml") != "local" {
		t.Fatal("local config lost")
	}
	root2 := testRoot(t)
	if err := root2.Mkdir("world", 0750); err != nil {
		t.Fatal(err)
	}
	if err := root2.Symlink("world", "config"); err != nil {
		t.Fatal(err)
	}
	if _, err := pack.Plan(context.Background(), service, root2); err == nil {
		t.Fatal("config directory symlink accepted")
	}
}
