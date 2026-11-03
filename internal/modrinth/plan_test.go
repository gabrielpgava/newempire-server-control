package modrinth

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path"
	"slices"
	"strings"
	"testing"
	"time"
)

type catalog struct {
	projects map[string]Project
	versions map[string]Version
	jars     map[string]string
}

func newCatalog() *catalog {
	return &catalog{projects: map[string]Project{}, versions: map[string]Version{}, jars: map[string]string{}}
}
func (c *catalog) add(project, version, contents string) Version {
	c.projects[project] = Project{ID: project, Slug: strings.ToLower(project), ProjectType: "mod", ServerSide: "required"}
	h1 := sha1.Sum([]byte(contents))
	v := Version{ID: version, ProjectID: project, VersionNumber: version, VersionType: "release", DatePublished: time.Unix(int64(len(c.versions)), 0).UTC(), GameVersions: []string{testTarget.MinecraftVersion}, Loaders: []string{"fabric"}, Environment: "server_only", Files: []File{{Filename: project + "-" + version + ".jar", Size: int64(len(contents)), Hashes: map[string]string{"sha512": hashBytes([]byte(contents)), "sha1": hex.EncodeToString(h1[:])}, Primary: true, URL: "https://cdn.modrinth.com/data/" + project + "/versions/" + version + "/" + project + "-" + version + ".jar"}}}
	c.versions[version] = v
	c.jars[v.Files[0].URL] = contents
	return v
}
func (c *catalog) client(t *testing.T) *Client {
	t.Helper()
	return NewClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "cdn.modrinth.com" {
			if data, ok := c.jars[req.URL.String()]; ok {
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(data)), ContentLength: int64(len(data))}, nil
			}
			return jsonResponse(404, nil), nil
		}
		if req.Method == "POST" && req.URL.Path == "/v2/version_files" {
			var body struct {
				Hashes []string `json:"hashes"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			result := map[string]Version{}
			for _, hash := range body.Hashes {
				for _, v := range c.versions {
					for _, file := range v.Files {
						if file.Hashes["sha512"] == hash {
							result[hash] = v
						}
					}
				}
			}
			return jsonResponse(200, result), nil
		}
		parts := strings.Split(strings.TrimPrefix(req.URL.Path, "/v2/"), "/")
		if len(parts) == 2 && parts[0] == "version" {
			if v, ok := c.versions[parts[1]]; ok {
				return jsonResponse(200, v), nil
			}
			return jsonResponse(404, nil), nil
		}
		if len(parts) >= 2 && parts[0] == "project" {
			project, ok := c.projects[parts[1]]
			if !ok {
				for _, p := range c.projects {
					if p.Slug == parts[1] {
						project = p
						ok = true
					}
				}
			}
			if !ok {
				return jsonResponse(404, nil), nil
			}
			if len(parts) == 2 {
				return jsonResponse(200, project), nil
			}
			if len(parts) == 3 && parts[2] == "version" {
				list := []Version{}
				for _, v := range c.versions {
					if v.ProjectID == project.ID {
						list = append(list, v)
					}
				}
				return jsonResponse(200, list), nil
			}
		}
		t.Errorf("unhandled request %s %s", req.Method, req.URL)
		return jsonResponse(404, nil), nil
	})})
}
func testRoot(t *testing.T) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return root
}
func put(t *testing.T, root *os.Root, name, data string) {
	t.Helper()
	if err := root.MkdirAll(path.Dir(name), 0750); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile(name, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}
}
func locked(v Version) LockedFile {
	f := v.Files[0]
	return LockedFile{Filename: f.Filename, SHA512: f.Hashes["sha512"], SHA1: f.Hashes["sha1"], Size: f.Size, ProjectID: v.ProjectID, VersionID: v.ID, URL: f.URL, Environment: v.Environment}
}
func installFixture(t *testing.T, root *os.Root, c *catalog, versions ...Version) {
	t.Helper()
	lock := Lock{Schema: 1, Target: testTarget, Policy: Policy{Mode: "manual", Pins: map[string]string{}, Excludes: []string{}}, Files: []LockedFile{}}
	for _, v := range versions {
		f := locked(v)
		lock.Files = append(lock.Files, f)
		put(t, root, "mods/"+f.Filename, c.jars[f.URL])
	}
	data, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile(LockFilename, data, 0600); err != nil {
		t.Fatal(err)
	}
}
func mustPlan(t *testing.T, s *Service, root *os.Root, req Request) *Plan {
	t.Helper()
	p, err := s.Plan(context.Background(), root, req)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCompatibilityEnvironmentsAndLegacy(t *testing.T) {
	c := newCatalog()
	v := c.add("A", "v1", "jar")
	p := c.projects["A"]
	for _, env := range []string{"server_only", "dedicated_server_only", "client_and_server", "server_only_client_optional", "client_only_server_optional", "client_or_server", "client_or_server_prefers_both"} {
		v.Environment = env
		if err := Compatible(p, v, testTarget, false); err != nil {
			t.Error(env, err)
		}
	}
	for _, env := range []string{"client_only", "singleplayer_only", "future_environment"} {
		v.Environment = env
		if err := Compatible(p, v, testTarget, false); err == nil {
			t.Error("accepted", env)
		}
	}
	v.Environment = ""
	if err := Compatible(p, v, testTarget, false); err != nil {
		t.Fatal(err)
	}
	p.ServerSide = "unknown"
	if err := Compatible(p, v, testTarget, false); err == nil {
		t.Error("accepted unknown legacy project")
	}
	p.Environment = []string{"server_only"}
	if err := Compatible(p, v, testTarget, false); err != nil {
		t.Fatal(err)
	}
	p.Environment = []string{"server_only", "client_only"}
	if err := Compatible(p, v, testTarget, false); err == nil {
		t.Error("accepted ambiguous project aggregate")
	}
	p.ProjectType = "plugin"
	v.Environment = "server_only"
	if err := Compatible(p, v, testTarget, false); err == nil {
		t.Error("accepted plugin")
	}
}

func TestInventoryHashesAndUnmanagedPreservation(t *testing.T) {
	c := newCatalog()
	v := c.add("A", "v1", "known")
	root := testRoot(t)
	installFixture(t, root, c, v)
	put(t, root, "mods/unknown.jar", "unknown")
	put(t, root, "mods/external.jar", "known")
	s := NewService(c.client(t))
	inv, err := s.Inventory(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Files) != 3 {
		t.Fatal(inv.Files)
	}
	for _, f := range inv.Files {
		if f.Filename == v.Files[0].Filename && !f.Managed {
			t.Error("locked file unmanaged")
		}
		if f.Filename == "unknown.jar" && (f.Managed || f.ProjectID != "") {
			t.Error("unknown file adopted")
		}
		if f.Filename == "external.jar" && (f.Managed || f.ProjectID != "A") {
			t.Error("external file not recognized/preserved")
		}
	}
	if p := mustPlan(t, s, root, Request{Target: testTarget, Projects: []string{"A"}}); len(p.Blockers) == 0 {
		t.Fatal("implicit adoption/duplicate project not blocked")
	}
	put(t, root, "mods/"+v.Files[0].Filename, "modified")
	inv, err = s.Inventory(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range inv.Files {
		if f.Filename == v.Files[0].Filename && f.Managed {
			t.Fatal("modified lock still managed")
		}
	}
	if p := mustPlan(t, s, root, Request{Target: testTarget}); len(p.Blockers) == 0 {
		t.Fatal("lock drift was not blocked")
	}
}

func TestPlanStableDefaultPinsDependenciesAndConflicts(t *testing.T) {
	c := newCatalog()
	a := c.add("Z", "z1", "Z")
	b1 := c.add("A", "a1", "A-old")
	_ = c.add("A", "a2", "A-new")
	a.Dependencies = []Dependency{{ProjectID: "A", VersionID: b1.ID, Type: "required"}}
	c.versions[a.ID] = a
	root := testRoot(t)
	s := NewService(c.client(t))
	req := Request{Target: testTarget, Projects: []string{"A", "Z"}}
	p := mustPlan(t, s, root, req)
	if len(p.Blockers) > 0 || len(p.Files) != 2 {
		t.Fatalf("files=%v blockers=%v", p.Files, p.Blockers)
	}
	for _, f := range p.Files {
		if f.ProjectID == "A" && f.VersionID != "a1" {
			t.Fatal("required exact version was not selected")
		}
	}
	p2 := mustPlan(t, s, root, req)
	j1, _ := json.Marshal(p)
	j2, _ := json.Marshal(p2)
	if string(j1) != string(j2) {
		t.Fatal("unchanged plan was not deterministic")
	}
	policy := Policy{Pins: map[string]string{"A": "a2"}}
	req.Policy = &policy
	if p := mustPlan(t, s, root, req); len(p.Blockers) == 0 {
		t.Fatal("dependency/pin conflict accepted")
	}
	policy = Policy{Excludes: []string{"A"}}
	if p := mustPlan(t, s, root, req); len(p.Blockers) == 0 {
		t.Fatal("excluded dependency accepted")
	}
	policy = Policy{}
	a.Dependencies = append(a.Dependencies, Dependency{ProjectID: "A", Type: "incompatible"})
	c.versions[a.ID] = a
	if p := mustPlan(t, s, root, req); len(p.Blockers) == 0 {
		t.Fatal("incompatibility accepted")
	}
}

func TestPlanRejectsCyclesUnknownClientOnlyAndPlugins(t *testing.T) {
	for _, kind := range []string{"cycle", "unknown", "client-only", "plugin", "game", "loader", "file-only", "required conflict"} {
		t.Run(kind, func(t *testing.T) {
			c := newCatalog()
			a := c.add("A", "v1", "A")
			b := c.add("B", "v2", "B")
			req := Request{Target: testTarget, Projects: []string{"A"}}
			switch kind {
			case "cycle":
				a.Dependencies = []Dependency{{ProjectID: "B", Type: "required"}}
				b.Dependencies = []Dependency{{ProjectID: "A", Type: "required"}}
				c.versions[b.ID] = b
			case "unknown":
				req.Projects = []string{"unknown"}
			case "client-only":
				a.Environment = "client_only"
			case "plugin":
				p := c.projects["A"]
				p.ProjectType = "plugin"
				c.projects["A"] = p
			case "game":
				a.GameVersions = []string{"other"}
			case "loader":
				a.Loaders = []string{"paper"}
			case "file-only":
				a.Dependencies = []Dependency{{FileName: "unverified.jar", Type: "required"}}
			case "required conflict":
				b2 := c.add("B", "v3", "B2")
				d := c.add("D", "v4", "D")
				a.Dependencies = []Dependency{{ProjectID: "B", VersionID: b.ID, Type: "required"}}
				d.Dependencies = []Dependency{{ProjectID: "B", VersionID: b2.ID, Type: "required"}}
				c.versions[d.ID] = d
				req.Projects = append(req.Projects, "D")
			}
			c.versions[a.ID] = a
			if p := mustPlan(t, NewService(c.client(t)), testRoot(t), req); len(p.Blockers) == 0 {
				t.Fatal("unsafe plan accepted", p)
			}
		})
	}
}

func TestStablePolicyAndSafeRemoval(t *testing.T) {
	c := newCatalog()
	old := c.add("A", "old", "old")
	beta := c.add("A", "beta", "beta")
	beta.VersionType = "beta"
	c.versions[beta.ID] = beta
	root := testRoot(t)
	installFixture(t, root, c, old)
	s := NewService(c.client(t))
	if p := mustPlan(t, s, root, Request{Target: testTarget}); len(p.Blockers) > 0 || p.Files[0].VersionID != "old" {
		t.Fatal(p)
	}
	policy := Policy{AllowPrerelease: true}
	if p := mustPlan(t, s, root, Request{Target: testTarget, Policy: &policy}); len(p.Blockers) > 0 || p.Files[0].VersionID != "beta" {
		t.Fatal(p)
	}
	policy = Policy{Pins: map[string]string{"A": "old"}}
	if err := SavePolicy(root, policy); err != nil {
		t.Fatal(err)
	}
	if p := mustPlan(t, s, root, Request{Target: testTarget, Remove: []string{"A"}}); len(p.Blockers) == 0 {
		t.Fatal("pinned removal accepted")
	}
	policy = Policy{}
	p := mustPlan(t, s, root, Request{Target: testTarget, Remove: []string{"A"}, Policy: &policy})
	if len(p.Blockers) > 0 || len(p.Files) != 0 || len(p.Changes) != 1 || p.Changes[0].Kind != "remove" {
		t.Fatal(p)
	}
	if p := mustPlan(t, s, root, Request{Target: testTarget, Versions: map[string]string{}}); !slices.ContainsFunc(p.Files, func(f LockedFile) bool { return f.VersionID == "old" }) {
		t.Fatal("empty explicit pack changed retained versions")
	}
}

func TestNestedJARsRemainUnmanagedAndDirectoryCollisionsBlockPlans(t *testing.T) {
	c := newCatalog()
	v := c.add("A", "v1", "jar")
	root := testRoot(t)
	s := NewService(c.client(t))
	put(t, root, "mods/nested/A.jar", "jar")
	inv, err := s.Inventory(context.Background(), root)
	if err != nil || len(inv.Files) != 1 || inv.Files[0].Managed || inv.Files[0].ProjectID != "A" {
		t.Fatal(inv, err)
	}
	if p := mustPlan(t, s, root, Request{Target: testTarget, Projects: []string{"A"}}); len(p.Blockers) == 0 {
		t.Fatal("nested unmanaged project was silently adopted")
	}
	root2 := testRoot(t)
	if err := root2.MkdirAll("mods/"+v.Files[0].Filename, 0750); err != nil {
		t.Fatal(err)
	}
	if p := mustPlan(t, s, root2, Request{Target: testTarget, Projects: []string{"A"}}); len(p.Blockers) == 0 {
		t.Fatal("directory destination collision missing from preview")
	}
}

func TestPolicyRequiresCanonicalProjectIDs(t *testing.T) {
	c := newCatalog()
	c.add("A", "v1", "jar")
	s := NewService(c.client(t))
	root := testRoot(t)
	policy := Policy{Pins: map[string]string{"a": "v1"}}
	p := mustPlan(t, s, root, Request{Target: testTarget, Projects: []string{"A"}, Policy: &policy})
	if len(p.Blockers) == 0 {
		t.Fatal("slug pin was silently ignored")
	}
}

func TestRequestAndPolicyWorkLimits(t *testing.T) {
	s := NewService(NewClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("oversized request reached the network")
		return nil, nil
	})}))
	p := mustPlan(t, s, testRoot(t), Request{Target: testTarget, Projects: make([]string, MaxProjects+1)})
	if len(p.Blockers) == 0 {
		t.Fatal("unbounded project request accepted")
	}
	if err := SavePolicy(testRoot(t), Policy{Excludes: make([]string, MaxProjects+1)}); err == nil {
		t.Fatal("unbounded policy accepted")
	}
	for _, name := range []string{"config/a\tb", "mods/a\x1bb.jar", "config/a\x7fb"} {
		if SafePath(name) {
			t.Fatal("control character path accepted", name)
		}
	}
}

func TestExplicitEmptyVersionSelectionSurvivesJSON(t *testing.T) {
	request := Request{Target: testTarget, Versions: map[string]string{}}
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var restored Request
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Versions == nil {
		t.Fatal("config-only pack became an all-managed update request after durable serialization")
	}
}

func TestAutomaticPolicyBlocksUnknownCompatibility(t *testing.T) {
	c := newCatalog()
	old := c.add("A", "old", "old")
	root := testRoot(t)
	installFixture(t, root, c, old)
	c.add("A", "new", "new")
	put(t, root, "mods/unknown.jar", "unknown")
	s := NewService(c.client(t))
	policy := Policy{Mode: "automatic"}
	if p := mustPlan(t, s, root, Request{Target: testTarget, Policy: &policy}); len(p.Blockers) == 0 {
		t.Fatal("automatic update claimed compatibility with unknown JAR")
	}
	policy.Mode = "manual"
	if p := mustPlan(t, s, root, Request{Target: testTarget, Policy: &policy}); len(p.Blockers) != 0 || len(p.Rationale) < 2 {
		t.Fatal("manual plan did not preserve/explain unknown content", p)
	}
}
