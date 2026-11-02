package fabric

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type fixtureResponse struct {
	body   []byte
	status int
	length int64
}

func metadataFixture(t *testing.T) (*Client, Selection, map[string]fixtureResponse) {
	t.Helper()
	selection := Selection{MinecraftVersion: "fixture-game", LoaderVersion: "0.99.0", InstallerVersion: "9.9.0"}
	minecraft := []byte(`{"id":"fixture-game","javaVersion":{"majorVersion":23},"downloads":{"server":{"url":"https://piston-data.mojang.com/v1/objects/abc/server.jar","sha1":"0123456789abcdef0123456789abcdef01234567","size":42}}}`)
	hash := sha1.Sum(minecraft)
	routes := map[string]fixtureResponse{
		metaOrigin + "/v2/versions/game":                {body: []byte(`[{"version":"fixture-game","stable":true},{"version":"1.14 Pre-Release 5","stable":false}]`)},
		metaOrigin + "/v2/versions/loader":              {body: []byte(`[{"version":"0.99.0","stable":true,"maven":"net.fabricmc:fabric-loader:0.99.0"}]`)},
		metaOrigin + "/v2/versions/installer":           {body: []byte(`[{"version":"9.9.0","stable":true,"maven":"net.fabricmc:fabric-installer:9.9.0","url":"https://maven.fabricmc.net/net/fabricmc/fabric-installer/9.9.0/fabric-installer-9.9.0.jar"}]`)},
		metaOrigin + "/v2/versions/loader/fixture-game": {body: []byte(`[{"loader":{"version":"0.99.0","stable":true,"maven":"net.fabricmc:fabric-loader:0.99.0"},"intermediary":{"version":"mapping-alias","maven":"net.fabricmc:intermediary:mapping-alias"},"launcherMeta":{"min_java_version":21}}]`)},
		mojangManifest: {body: []byte(fmt.Sprintf(`{"versions":[{"id":"fixture-game","url":"https://piston-meta.mojang.com/v1/packages/fixture/game.json","sha1":"%s"}]}`, hex.EncodeToString(hash[:])))},
		"https://piston-meta.mojang.com/v1/packages/fixture/game.json":          {body: minecraft},
		metaOrigin + "/v2/versions/loader/fixture-game/0.99.0/9.9.0/server/jar": {body: launcherFixture(t, selection)},
	}
	client := NewClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet {
			return nil, errors.New("not GET")
		}
		if _, ok := req.Context().Deadline(); !ok {
			return nil, errors.New("missing request deadline")
		}
		response, exists := routes[req.URL.String()]
		if !exists {
			return nil, fmt.Errorf("unexpected upstream request %s", req.URL)
		}
		status := response.status
		if status == 0 {
			status = http.StatusOK
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(response.body)), ContentLength: response.length, Header: make(http.Header), Request: req}, nil
	})})
	return client, selection, routes
}

func launcherFixture(t *testing.T, selection Selection) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	for _, file := range []struct{ name, content string }{
		{"META-INF/MANIFEST.MF", "Manifest-Version: 1.0\r\nMain-Class: net.fabricmc.installer.ServerLauncher\r\n\r\n"},
		{"net/fabricmc/installer/ServerLauncher.class", "\xca\xfe\xba\xbe\x00\x00\x00\x34"},
		{"install.properties", "fabric-loader-version=" + selection.LoaderVersion + "\ngame-version=" + selection.MinecraftVersion + "\n"},
	} {
		entry, err := writer.Create(file.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(entry, file.content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestPlanUsesVerifiedMojangJavaMinimumAndFabricCompatibility(t *testing.T) {
	client, selection, _ := metadataFixture(t)
	plan, err := client.Plan(t.Context(), selection)
	if err != nil {
		t.Fatal(err)
	}
	if plan.JavaMajor != 23 || plan.IntermediaryVersion != "mapping-alias" || plan.LauncherTrust != LauncherTrust || !strings.HasPrefix(plan.LauncherURL, metaOrigin+"/") || plan.MinecraftServer.Size != 42 || len(plan.Warnings) < 3 {
		t.Fatalf("incorrect plan %+v", plan)
	}
	versions, err := client.Versions(t.Context())
	if err != nil || len(versions.Games) != 2 || versions.Games[1].Version != "1.14 Pre-Release 5" {
		t.Fatalf("lost upstream versions: %+v %v", versions, err)
	}
	loaders, err := client.Loaders(t.Context(), selection.MinecraftVersion)
	if err != nil || len(loaders) != 1 || loaders[0].Version != selection.LoaderVersion {
		t.Fatalf("%+v %v", loaders, err)
	}
}

func TestPlanHonorsHigherLoaderJavaMinimum(t *testing.T) {
	client, selection, routes := metadataFixture(t)
	key := metaOrigin + "/v2/versions/loader/fixture-game"
	response := routes[key]
	response.body = bytes.ReplaceAll(response.body, []byte(`"min_java_version":21`), []byte(`"min_java_version":25`))
	routes[key] = response
	plan, err := client.Plan(t.Context(), selection)
	if err != nil || plan.JavaMajor != 25 {
		t.Fatalf("%+v %v", plan, err)
	}
}

func TestPlanFailsClosedForUnknownVersionsAndMalformedMetadata(t *testing.T) {
	for _, tc := range []struct{ name, route, replacement string }{
		{"bad-json", "/v2/versions/game", `[{`},
		{"trailing-json", "/v2/versions/game", `[{"version":"fixture-game"}] {}`},
		{"empty-games", "/v2/versions/game", `[]`},
		{"null-games", "/v2/versions/game", `null`},
		{"duplicate-games", "/v2/versions/game", `[{"version":"fixture-game"},{"version":"fixture-game"}]`},
		{"unsafe-game", "/v2/versions/game", `[{"version":"../../secret"}]`},
		{"loader-origin", "/v2/versions/loader", `[{"version":"0.99.0","maven":"evil:loader:0.99.0"}]`},
		{"installer-origin", "/v2/versions/installer", `[{"version":"9.9.0","maven":"net.fabricmc:fabric-installer:9.9.0","url":"https://127.0.0.1/installer.jar"}]`},
		{"invalid-intermediary", "/v2/versions/loader/fixture-game", `[{"loader":{"version":"0.99.0","maven":"net.fabricmc:fabric-loader:0.99.0"},"intermediary":{"version":"mapping-alias","maven":"evil:intermediary:mapping-alias"}}]`},
		{"no-game-loader", "/v2/versions/loader/fixture-game", `[]`},
		{"bad-loader-java", "/v2/versions/loader/fixture-game", `[{"loader":{"version":"0.99.0","maven":"net.fabricmc:fabric-loader:0.99.0"},"intermediary":{"version":"mapping-alias","maven":"net.fabricmc:intermediary:mapping-alias"},"launcherMeta":{"min_java_version":-1}}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, selection, routes := metadataFixture(t)
			routes[metaOrigin+tc.route] = fixtureResponse{body: []byte(tc.replacement)}
			if _, err := client.Plan(t.Context(), selection); err == nil {
				t.Fatal("accepted malformed upstream metadata")
			}
		})
	}
	for _, selection := range []Selection{{}, {MinecraftVersion: "fixture-game", LoaderVersion: "0.99.0", InstallerVersion: "unknown"}, {MinecraftVersion: "../escape", LoaderVersion: "0.99.0", InstallerVersion: "9.9.0"}, {MinecraftVersion: "fixture-game", LoaderVersion: "latest", InstallerVersion: "9.9.0"}} {
		client, _, _ := metadataFixture(t)
		if _, err := client.Plan(t.Context(), selection); err == nil {
			t.Fatalf("accepted %+v", selection)
		}
	}
}

func TestPlanVerifiesMojangHashOriginIdentityAndDeclaredJava(t *testing.T) {
	for _, mode := range []string{"hash", "origin", "private", "missing", "duplicate", "identity", "missing-java", "bad-java", "server-origin", "server-hash", "server-size", "malformed-json"} {
		t.Run(mode, func(t *testing.T) {
			client, selection, routes := metadataFixture(t)
			metadataKey := "https://piston-meta.mojang.com/v1/packages/fixture/game.json"
			manifest, metadata := routes[mojangManifest], routes[metadataKey]
			switch mode {
			case "hash":
				metadata.body = append(metadata.body, ' ')
			case "origin":
				manifest.body = bytes.ReplaceAll(manifest.body, []byte("piston-meta.mojang.com"), []byte("evil.example"))
			case "private":
				manifest.body = bytes.ReplaceAll(manifest.body, []byte("piston-meta.mojang.com"), []byte("127.0.0.1"))
			case "missing":
				manifest.body = []byte(`{"versions":[]}`)
			case "duplicate":
				var m struct {
					Versions []json.RawMessage `json:"versions"`
				}
				if err := json.Unmarshal(manifest.body, &m); err != nil {
					t.Fatal(err)
				}
				m.Versions = append(m.Versions, m.Versions[0])
				manifest.body, _ = json.Marshal(m)
			case "identity":
				metadata.body = bytes.ReplaceAll(metadata.body, []byte(`"id":"fixture-game"`), []byte(`"id":"wrong-game"`))
			case "missing-java":
				metadata.body = bytes.ReplaceAll(metadata.body, []byte(`"majorVersion":23`), []byte(`"notMajorVersion":23`))
			case "bad-java":
				metadata.body = bytes.ReplaceAll(metadata.body, []byte(`"majorVersion":23`), []byte(`"majorVersion":99999`))
			case "server-origin":
				metadata.body = bytes.ReplaceAll(metadata.body, []byte("piston-data.mojang.com"), []byte("attacker.example"))
			case "server-hash":
				metadata.body = bytes.ReplaceAll(metadata.body, []byte("0123456789abcdef0123456789abcdef01234567"), []byte("bad"))
			case "server-size":
				metadata.body = bytes.ReplaceAll(metadata.body, []byte(`"size":42`), []byte(`"size":0`))
			case "malformed-json":
				metadata.body = []byte(`{"id":`)
			}
			if mode != "hash" {
				original := sha1.Sum(routes[metadataKey].body)
				changed := sha1.Sum(metadata.body)
				manifest.body = bytes.ReplaceAll(manifest.body, []byte(hex.EncodeToString(original[:])), []byte(hex.EncodeToString(changed[:])))
			}
			routes[mojangManifest], routes[metadataKey] = manifest, metadata
			if _, err := client.Plan(t.Context(), selection); err == nil {
				t.Fatal("accepted unverified/incomplete Mojang metadata")
			}
		})
	}
}

func TestHTTPBoundariesAndRedirects(t *testing.T) {
	for _, address := range []string{"http://meta.fabricmc.net/v2/versions/game", "https://127.0.0.1/a", "https://meta.fabricmc.net.evil.example/a", "https://meta.fabricmc.net:8443/a", "https://user:pass@meta.fabricmc.net/a", "https://meta.fabricmc.net/a?q=1", "https://meta.fabricmc.net/a#x", "file:///etc/passwd"} {
		if officialURL(address) {
			t.Errorf("accepted %s", address)
		}
	}
	client, _, routes := metadataFixture(t)
	key := metaOrigin + "/v2/versions/game"
	for _, response := range []fixtureResponse{{status: 500}, {body: []byte("12345")}, {body: []byte("x"), length: 5}} {
		routes[key] = response
		if _, err := client.get(t.Context(), key, 4); err == nil {
			t.Fatal("accepted oversized/error response")
		}
	}
	calls := 0
	client = NewClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"https://127.0.0.1/secret"}}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
	})})
	if _, err := client.get(t.Context(), key, 100); err == nil || calls != 1 {
		t.Fatalf("unsafe redirect was followed: %d %v", calls, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	client = NewClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) { return nil, req.Context().Err() })})
	if _, err := client.get(ctx, key, 100); !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
}

func TestStageWritesOnlyNewPrivateVerifiedArtifacts(t *testing.T) {
	client, selection, _ := metadataFixture(t)
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	properties := []byte("# Keep this comment\nlevel-name=world\n")
	result, err := client.Stage(t.Context(), selection, root, properties)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(result.Directory) != root || len(result.LauncherSHA256) != 64 || len(result.PropertiesSHA256) != 64 {
		t.Fatalf("invalid stage result %+v", result)
	}
	for _, name := range []string{"launcher.jar", "server.properties", "manifest.json"} {
		info, err := os.Stat(filepath.Join(result.Directory, name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("unsafe staged %s: %v %v", name, info, err)
		}
	}
	data, err := os.ReadFile(filepath.Join(result.Directory, "server.properties"))
	if err != nil || !bytes.Equal(data, properties) {
		t.Fatalf("properties changed: %q %v", data, err)
	}
	entries, err := os.ReadDir(result.Directory)
	if err != nil || len(entries) != 3 {
		t.Fatalf("unexpected stage contents: %v %v", entries, err)
	}
	if _, err := os.Stat(filepath.Join(result.Directory, "eula.txt")); !os.IsNotExist(err) {
		t.Fatal("staging accepted EULA")
	}
	second, err := client.Stage(t.Context(), selection, root, properties)
	if err != nil || second.Directory == result.Directory {
		t.Fatalf("stage reused an existing directory: %+v %v", second, err)
	}
}

func TestStageFailsWithoutWritingOnUnsafeOrMalformedInput(t *testing.T) {
	for _, mode := range []string{"wrong-versions", "not-jar", "missing-manifest", "public-root", "bad-properties", "oversized-download", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			client, selection, routes := metadataFixture(t)
			root := t.TempDir()
			if err := os.Chmod(root, 0700); err != nil {
				t.Fatal(err)
			}
			key := metaOrigin + "/v2/versions/loader/fixture-game/0.99.0/9.9.0/server/jar"
			response := routes[key]
			properties := []byte("level-name=world\n")
			ctx := t.Context()
			switch mode {
			case "wrong-versions":
				response.body = launcherFixture(t, Selection{MinecraftVersion: "wrong", LoaderVersion: selection.LoaderVersion})
			case "not-jar":
				response.body = []byte("not a jar")
			case "missing-manifest":
				var data bytes.Buffer
				writer := zip.NewWriter(&data)
				file, err := writer.Create("install.properties")
				if err != nil {
					t.Fatal(err)
				}
				_, _ = io.WriteString(file, "game-version=fixture-game\nfabric-loader-version=0.99.0")
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
				response.body = data.Bytes()
			case "public-root":
				if err := os.Chmod(root, 0755); err != nil {
					t.Fatal(err)
				}
			case "bad-properties":
				properties = []byte("level-name=../outside\n")
			case "oversized-download":
				response.length = MaxLauncherBytes + 1
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			routes[key] = response
			if _, err := client.Stage(ctx, selection, root, properties); err == nil {
				t.Fatal("unsafe staging succeeded")
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 0 {
				t.Fatalf("failed stage left artifacts: %v %v", entries, err)
			}
		})
	}
}

func TestWriteStagedRejectsSymlinksAndExistingFiles(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "protected")
	if err := os.WriteFile(outside, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.Symlink(outside, "launcher.jar"); err != nil {
		t.Fatal(err)
	}
	if err := writeStaged(root, "launcher.jar", []byte("unsafe")); err == nil {
		t.Fatal("followed symlink")
	}
	if err := writeStaged(root, "server.properties", []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := writeStaged(root, "server.properties", []byte("second")); err == nil {
		t.Fatal("overwrote existing stage")
	}
	data, err := os.ReadFile(outside)
	if err != nil || string(data) != "unchanged" {
		t.Fatal("escaped staging root")
	}
}

func TestLiveUpstreamStage(t *testing.T) {
	if os.Getenv("NEWEMPIRE_UPSTREAM_TEST") != "1" {
		t.Skip("set NEWEMPIRE_UPSTREAM_TEST=1 for official HTTPS metadata and launcher smoke test")
	}
	client := NewClient(nil)
	versions, err := client.Versions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var selection Selection
	for _, game := range versions.Games {
		if game.Stable {
			selection.MinecraftVersion = game.Version
			break
		}
	}
	loaders, err := client.Loaders(t.Context(), selection.MinecraftVersion)
	if err != nil {
		t.Fatal(err)
	}
	for _, loader := range loaders {
		if loader.Stable {
			selection.LoaderVersion = loader.Version
			break
		}
	}
	for _, installer := range versions.Installers {
		if installer.Stable {
			selection.InstallerVersion = installer.Version
			break
		}
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	stage, err := client.Stage(t.Context(), selection, root, []byte("level-name=world\n"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("staged official Minecraft %s / loader %s / installer %s; metadata Java minimum %d; launcher SHA-256 %s", selection.MinecraftVersion, selection.LoaderVersion, selection.InstallerVersion, stage.Plan.JavaMajor, stage.LauncherSHA256)
}
