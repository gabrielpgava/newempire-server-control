package updates

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type fakeGithub struct {
	key      ed25519.PrivateKey
	public   ed25519.PublicKey
	releases []githubRelease
	bodies   map[string][]byte
	hook     transportFunc
	requests int
}

func newGithub(t *testing.T) *fakeGithub {
	t.Helper()
	pub, private, err := ed25519.GenerateKey(rand.Reader)
	must(t, err)
	return &fakeGithub{key: private, public: pub, bodies: map[string][]byte{}}
}

func response(r *http.Request, status int, b []byte) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(b)), ContentLength: int64(len(b)), Request: r}
}

func (f *fakeGithub) roundTrip(r *http.Request) (*http.Response, error) {
	f.requests++
	if f.hook != nil {
		resp, err := f.hook(r)
		if resp != nil || err != nil {
			return resp, err
		}
	}
	address := r.URL.String()
	if strings.HasPrefix(address, releasesAPI+"?") {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		start := (page - 1) * 100
		if start < 0 || start >= len(f.releases) {
			return response(r, 200, []byte("[]")), nil
		}
		end := min(start+100, len(f.releases))
		b, _ := json.Marshal(f.releases[start:end])
		return response(r, 200, b), nil
	}
	if strings.HasPrefix(address, releasesAPI+"/tags/") {
		tag := strings.TrimPrefix(r.URL.Path, "/repos/"+Repository+"/releases/tags/")
		for _, release := range f.releases {
			if release.Tag == tag {
				b, _ := json.Marshal(release)
				return response(r, 200, b), nil
			}
		}
	}
	if b, ok := f.bodies[address]; ok {
		return response(r, 200, b), nil
	}
	return response(r, 404, nil), nil
}

func fakeELF(arch, marker string) []byte {
	b := make([]byte, 64)
	copy(b, "\x7fELF")
	b[4], b[5], b[6], b[7] = 2, 1, 1, 3
	binary.LittleEndian.PutUint16(b[16:18], 2)
	machine := uint16(62)
	if arch == "arm64" {
		machine = 183
	}
	binary.LittleEndian.PutUint16(b[18:20], machine)
	binary.LittleEndian.PutUint32(b[20:24], 1)
	binary.LittleEndian.PutUint16(b[52:54], 64)
	return append(b, marker...)
}

func tarball(t *testing.T, header *tar.Header, payload []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	if header == nil {
		header = &tar.Header{Name: BinaryName, Mode: 0755, Size: int64(len(payload)), Typeflag: tar.TypeReg}
	}
	must(t, tw.WriteHeader(header))
	if header.Size > 0 {
		_, err := tw.Write(payload)
		must(t, err)
	}
	must(t, tw.Close())
	must(t, gz.Close())
	return b.Bytes()
}

func digest(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (f *fakeGithub) release(v string) *githubRelease {
	for i := range f.releases {
		if f.releases[i].Tag == "v"+v {
			return &f.releases[i]
		}
	}
	panic("missing fixture release " + v)
}

func (f *fakeGithub) setManifest(t *testing.T, v string, raw []byte) {
	t.Helper()
	sig := []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(f.key, raw)))
	f.bodies[downloadURL(v, "release.json")] = raw
	f.bodies[downloadURL(v, "release.json.sig")] = sig
	r := f.release(v)
	for i := range r.Assets {
		if r.Assets[i].Name == "release.json" {
			r.Assets[i].Size = int64(len(raw))
		}
		if r.Assets[i].Name == "release.json.sig" {
			r.Assets[i].Size = int64(len(sig))
		}
	}
}

func (f *fakeGithub) add(t *testing.T, v string, schema int) Manifest {
	t.Helper()
	m := Manifest{Version: v, Repository: Repository, Schema: schema}
	r := githubRelease{Tag: "v" + v, Prerelease: strings.Contains(strings.Split(v, "+")[0], "-")}
	for _, arch := range []string{"amd64", "arm64"} {
		b := tarball(t, nil, fakeELF(arch, v))
		a := Asset{Name: archiveName(v, arch), OS: "linux", Arch: arch, Size: int64(len(b)), SHA256: digest(b)}
		m.Assets = append(m.Assets, a)
		r.Assets = append(r.Assets, githubAsset{a.Name, a.Size, downloadURL(v, a.Name)})
		f.bodies[downloadURL(v, a.Name)] = b
	}
	r.Assets = append(r.Assets, githubAsset{"release.json", 1, downloadURL(v, "release.json")}, githubAsset{"release.json.sig", 88, downloadURL(v, "release.json.sig")})
	f.releases = append(f.releases, r)
	raw, err := CanonicalManifest(m)
	must(t, err)
	f.setManifest(t, v, raw)
	return m
}

func (f *fakeGithub) replaceArchive(t *testing.T, v, arch string, body []byte) {
	t.Helper()
	var m Manifest
	must(t, json.Unmarshal(f.bodies[downloadURL(v, "release.json")], &m))
	for i := range m.Assets {
		if m.Assets[i].Arch == arch {
			m.Assets[i].Size = int64(len(body))
			m.Assets[i].SHA256 = digest(body)
		}
	}
	name := archiveName(v, arch)
	f.bodies[downloadURL(v, name)] = body
	for i := range f.release(v).Assets {
		if f.release(v).Assets[i].Name == name {
			f.release(v).Assets[i].Size = int64(len(body))
		}
	}
	raw, err := CanonicalManifest(m)
	must(t, err)
	f.setManifest(t, v, raw)
}

type fakeService struct {
	root      string
	running   bool
	calls     []string
	onStop    func() error
	onRestart func(string) error
	onHealth  func(context.Context, string) error
}

func (s *fakeService) current() string {
	v, _ := os.Readlink(filepath.Join(s.root, "current"))
	return strings.TrimPrefix(v, "versions/")
}

func (s *fakeService) StopPanel(ctx context.Context) error {
	s.calls = append(s.calls, "stop")
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.onStop != nil {
		if err := s.onStop(); err != nil {
			return err
		}
	}
	s.running = false
	return nil
}

func (s *fakeService) RestartPanel(ctx context.Context) error {
	s.calls = append(s.calls, "restart:"+s.current())
	if err := ctx.Err(); err != nil {
		return err
	}
	s.running = true
	if s.onRestart != nil {
		return s.onRestart(s.current())
	}
	return nil
}

func (s *fakeService) Health(ctx context.Context) error {
	s.calls = append(s.calls, "health:"+s.current())
	if !s.running {
		return fmt.Errorf("panel is stopped")
	}
	if s.onHealth != nil {
		return s.onHealth(ctx, s.current())
	}
	return ctx.Err()
}

type fixture struct {
	m             *Manager
	github        *fakeGithub
	service       *fakeService
	root          string
	data          string
	world         string
	opts          Options
	snapshotCalls int
	restoreCalls  int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{root: filepath.Join(t.TempDir(), "install"), data: t.TempDir(), world: filepath.Join(t.TempDir(), "world")}
	must(t, os.MkdirAll(filepath.Join(f.root, "versions", "1.0.0"), 0755))
	must(t, os.WriteFile(filepath.Join(f.root, "versions", "1.0.0", BinaryName), fakeELF(runtime.GOARCH, "original"), 0555))
	must(t, os.Symlink("versions/1.0.0", filepath.Join(f.root, "current")))
	must(t, os.WriteFile(filepath.Join(f.data, "config.json"), []byte(`{"listen":"127.0.0.1:8080"}`), 0600))
	must(t, os.WriteFile(filepath.Join(f.data, "encryption.key"), []byte("fixture credential key"), 0600))
	must(t, os.WriteFile(f.world, []byte("world must never change"), 0600))
	writeDB(t, f.data, "original")
	f.github = newGithub(t)
	f.service = &fakeService{root: f.root, running: true}
	f.opts = Options{InstallDir: f.root, CurrentVersion: "1.0.0", Schema: 1, PublicKey: base64.StdEncoding.EncodeToString(f.github.public), Client: &http.Client{Transport: transportFunc(f.github.roundTrip)}, Service: f.service, HealthTimeout: 50 * time.Millisecond}
	f.opts.Snapshot = func(ctx context.Context, dir string) error {
		f.snapshotCalls++
		if f.service.running {
			return fmt.Errorf("snapshot attempted while panel running")
		}
		db, err := sql.Open("sqlite", filepath.Join(f.data, "panel.sqlite"))
		if err != nil {
			return err
		}
		_, err = db.ExecContext(ctx, "VACUUM INTO ?", filepath.Join(dir, "panel.sqlite"))
		db.Close()
		if err != nil {
			return err
		}
		for _, name := range []string{"config.json", "encryption.key"} {
			if err := copyFile(filepath.Join(f.data, name), filepath.Join(dir, name)); err != nil {
				return err
			}
		}
		return ctx.Err()
	}
	f.opts.Restore = func(ctx context.Context, dir string) error {
		f.restoreCalls++
		if f.service.running {
			return fmt.Errorf("restore attempted while panel running")
		}
		for _, name := range []string{"panel.sqlite", "config.json", "encryption.key"} {
			if err := copyFile(filepath.Join(dir, name), filepath.Join(f.data, name)); err != nil {
				return err
			}
		}
		return ctx.Err()
	}
	var err error
	f.m, err = New(f.opts)
	must(t, err)
	return f
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0600)
}

func writeDB(t *testing.T, dir, value string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "panel.sqlite"))
	must(t, err)
	defer db.Close()
	_, err = db.Exec("CREATE TABLE IF NOT EXISTS settings (value TEXT NOT NULL)")
	must(t, err)
	_, err = db.Exec("DELETE FROM settings")
	must(t, err)
	_, err = db.Exec("INSERT INTO settings (value) VALUES (?)", value)
	must(t, err)
}

func readDB(t *testing.T, dir string) string {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "panel.sqlite"))
	must(t, err)
	defer db.Close()
	var value string
	must(t, db.QueryRow("SELECT value FROM settings").Scan(&value))
	return value
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(name)
	must(t, err)
	return b
}

func state(t *testing.T, m *Manager) Journal {
	t.Helper()
	j, err := m.Status(context.Background())
	must(t, err)
	return j
}

func stage(t *testing.T, f *fixture) Transaction {
	t.Helper()
	f.github.add(t, "1.1.0", 1)
	tx, err := f.m.Stage(context.Background(), "1.1.0", Stable)
	must(t, err)
	return tx
}
