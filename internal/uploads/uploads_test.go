package uploads

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gabrielpgava/newempire-server-control/internal/model"
)

func uploadConfig(t *testing.T) Config {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return Config{Root: r, BasePath: "/api/v1/uploads/", MaxSize: 1024, Quota: 4096, MaxUploads: 10, TTL: time.Hour, Authorize: func(r *http.Request) (string, error) {
		if r.Header.Get("Owner") == "" {
			return "", errors.New("not authenticated")
		}
		if r.Method != http.MethodHead && r.Method != http.MethodOptions && r.Header.Get("X-CSRF-Token") != "valid" {
			return "", errors.New("invalid CSRF token")
		}
		return r.Header.Get("Owner"), nil
	}}
}

func newFixture(t *testing.T, config Config) *Service {
	t.Helper()
	s, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func request(s *Service, method, target, owner, body string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Header.Set("Owner", owner)
	r.Header.Set("X-CSRF-Token", "valid")
	r.Header.Set("Tus-Resumable", "1.0.0")
	if method == http.MethodPatch {
		r.Header.Set("Content-Type", "application/offset+octet-stream")
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func create(s *Service, owner, name string, size int64) *httptest.ResponseRecorder {
	return request(s, http.MethodPost, s.config.BasePath, owner, "", map[string]string{"Upload-Length": strconv.FormatInt(size, 10), "Upload-Metadata": "filename " + base64.StdEncoding.EncodeToString([]byte(name))})
}

func uploadLocation(t *testing.T, w *httptest.ResponseRecorder) (string, string) {
	t.Helper()
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	u, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	id := u.Path[strings.LastIndex(u.Path, "/")+1:]
	if !model.ValidID(id) {
		t.Fatalf("invalid upload location %s", u)
	}
	return u.Path, id
}

func TestTusResumeOffsetsOwnershipAndDelete(t *testing.T) {
	s := newFixture(t, uploadConfig(t))
	location, id := uploadLocation(t, create(s, "alice", "server.zip", 6))
	w := request(s, http.MethodHead, location, "alice", "", nil)
	if w.Code != http.StatusOK || w.Header().Get("Upload-Offset") != "0" {
		t.Fatalf("HEAD: %d %v", w.Code, w.Header())
	}
	w = request(s, http.MethodPatch, location, "alice", "ab", map[string]string{"Upload-Offset": "0"})
	if w.Code != http.StatusNoContent || w.Header().Get("Upload-Offset") != "2" {
		t.Fatalf("PATCH: %d %s", w.Code, w.Body.String())
	}
	if _, err := s.Lookup(context.Background(), "alice", id); err == nil {
		t.Fatal("incomplete upload returned")
	}
	w = request(s, http.MethodPatch, location, "alice", "bad", map[string]string{"Upload-Offset": "1"})
	if w.Code != http.StatusConflict {
		t.Fatalf("wrong offset: %d %s", w.Code, w.Body.String())
	}
	for _, method := range []string{http.MethodHead, http.MethodPatch, http.MethodDelete} {
		w = request(s, method, location, "mallory", "x", map[string]string{"Upload-Offset": "2"})
		if w.Code != http.StatusNotFound {
			t.Fatalf("other owner %s: %d", method, w.Code)
		}
	}
	w = request(s, http.MethodPatch, location, "alice", "cdef", map[string]string{"Upload-Offset": "2"})
	if w.Code != http.StatusNoContent || w.Header().Get("Upload-Offset") != "6" {
		t.Fatalf("resume: %d %s", w.Code, w.Body.String())
	}
	complete, err := s.Lookup(context.Background(), "alice", id)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(complete.File)
	complete.File.Close()
	if err != nil || string(data) != "abcdef" || complete.Filename != "server.zip" {
		t.Fatalf("completed bytes: %q %v", data, err)
	}
	if _, err = s.Lookup(context.Background(), "mallory", id); err == nil {
		t.Fatal("other owner's lookup allowed")
	}
	w = request(s, http.MethodGet, location, "alice", "", nil)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("tus download enabled: %d", w.Code)
	}
	w = request(s, http.MethodDelete, location, "alice", "", nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("DELETE: %d %s", w.Code, w.Body.String())
	}
	if _, err = s.Lookup(context.Background(), "alice", id); err == nil {
		t.Fatal("deleted upload returned")
	}
	if s.reserved != 0 || s.count != 0 {
		t.Fatal("reservation leaked")
	}
}

func TestTusAuthorizationMetadataLimitsAndQuota(t *testing.T) {
	config := uploadConfig(t)
	config.MaxSize, config.Quota = 8, 12
	s := newFixture(t, config)
	for _, name := range []string{"", "../escape", "/absolute", "dir/file", "a\\b", ".nesc-service.lock"} {
		if w := create(s, "alice", name, 1); w.Code < 400 {
			t.Fatalf("accepted filename %q", name)
		}
	}
	if w := create(s, "alice", "file", 9); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("max size: %d", w.Code)
	}
	if w := create(s, "", "file", 1); w.Code != http.StatusForbidden {
		t.Fatalf("anonymous upload: %d", w.Code)
	}
	w := request(s, http.MethodPost, config.BasePath, "alice", "", map[string]string{"X-CSRF-Token": "wrong", "Upload-Length": "1"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF: %d", w.Code)
	}
	w = request(s, http.MethodPost, config.BasePath, "alice", "", map[string]string{"Upload-Defer-Length": "1"})
	if w.Code < 400 {
		t.Fatal("unknown length accepted")
	}
	location, id := uploadLocation(t, create(s, "alice", "first.zip", 8))
	w = request(s, http.MethodPatch, location, "alice", "abc", map[string]string{"Upload-Offset": "0", "X-HTTP-Method-Override": "DELETE"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("method override accepted: %d", w.Code)
	}
	if w = create(s, "bob", "second.zip", 5); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("quota not reserved: %d", w.Code)
	}
	if err := s.Delete(context.Background(), "bob", id); err == nil {
		t.Fatal("other owner deletion allowed")
	}
	if err := s.Delete(context.Background(), "alice", id); err != nil {
		t.Fatal(err)
	}
	uploadLocation(t, create(s, "bob", "second.zip", 5))
	if w = request(s, http.MethodHead, config.BasePath+"../escape", "alice", "", nil); w.Code != http.StatusNotFound {
		t.Fatalf("path traversal: %d", w.Code)
	}
}

func TestTusDurableRestartAndProcessLease(t *testing.T) {
	config := uploadConfig(t)
	s := newFixture(t, config)
	location, id := uploadLocation(t, create(s, "alice", "file", 6))
	if w := request(s, http.MethodPatch, location, "alice", "abc", map[string]string{"Upload-Offset": "0"}); w.Code != http.StatusNoContent {
		t.Fatal(w.Body.String())
	}
	if other, err := New(config); err == nil {
		other.Close()
		t.Fatal("second quota authority allowed")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := newFixture(t, config)
	if restarted.reserved != 6 || restarted.count != 1 {
		t.Fatal("reservation not recovered")
	}
	w := request(restarted, http.MethodHead, location, "alice", "", nil)
	if w.Code != http.StatusOK || w.Header().Get("Upload-Offset") != "3" {
		t.Fatalf("offset not recovered: %d %v", w.Code, w.Header())
	}
	w = request(restarted, http.MethodPatch, location, "alice", "def", map[string]string{"Upload-Offset": "3"})
	if w.Code != http.StatusNoContent {
		t.Fatalf("restart resume: %d %s", w.Code, w.Body.String())
	}
	complete, err := restarted.Lookup(context.Background(), "alice", id)
	if err != nil {
		t.Fatal(err)
	}
	complete.File.Close()
}

func TestTusExpiryCountBoundCancellationAndLocks(t *testing.T) {
	config := uploadConfig(t)
	config.MaxUploads = 1
	s := newFixture(t, config)
	location, id := uploadLocation(t, create(s, "alice", "empty", 0))
	if w := create(s, "alice", "another", 0); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("zero-byte count unbounded: %d", w.Code)
	}
	lock, err := s.NewLock(id)
	if err != nil {
		t.Fatal(err)
	}
	if err = lock.Lock(context.Background(), func() {}); err != nil {
		t.Fatal(err)
	}
	blocked, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err = s.Lookup(blocked, "alice", id); err == nil {
		t.Fatal("lookup ignored file lock")
	}
	if err = lock.Unlock(); err != nil {
		t.Fatal(err)
	}
	st, err := s.readState(id)
	if err != nil {
		t.Fatal(err)
	}
	st.CreatedAt = time.Now().Add(-2 * time.Hour)
	st.ExpiresAt = time.Now().Add(-time.Hour)
	raw, _ := json.Marshal(st)
	if err = config.Root.WriteFile(id+".json", raw, 0600); err != nil {
		t.Fatal(err)
	}
	if w := request(s, http.MethodHead, location, "alice", "", nil); w.Code != http.StatusNotFound {
		t.Fatal("expired upload readable")
	}
	if err = s.Prune(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = config.Root.Stat(id + ".data"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired data retained: %v", err)
	}
	if s.reserved != 0 || s.count != 0 {
		t.Fatal("expired reservation retained")
	}
	uploadLocation(t, create(s, "alice", "replacement", 0))
	ctx, stop := context.WithCancel(context.Background())
	stop()
	if err = s.Prune(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("prune ignored cancellation: %v", err)
	}
}

func TestTusConcurrentReservationsAndOrphanRecovery(t *testing.T) {
	config := uploadConfig(t)
	config.MaxSize, config.Quota, config.MaxUploads = 8, 12, 100
	orphan := model.NewID()
	if err := config.Root.WriteFile(orphan+".data", []byte("crash orphan"), 0600); err != nil {
		t.Fatal(err)
	}
	s := newFixture(t, config)
	if _, err := config.Root.Stat(orphan + ".data"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("orphan not removed")
	}
	var successes atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if create(s, "alice", "file", 4).Code == http.StatusCreated {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 3 || s.reserved != 12 {
		t.Fatalf("quota raced: %d %d", successes.Load(), s.reserved)
	}
}

func TestTusConcurrentPatchesCannotReuseOffset(t *testing.T) {
	s := newFixture(t, uploadConfig(t))
	location, id := uploadLocation(t, create(s, "alice", "file", 6))
	var wg sync.WaitGroup
	results := make(chan int, 2)
	for _, body := range []string{"abc", "def"} {
		wg.Add(1)
		go func(body string) {
			defer wg.Done()
			results <- request(s, http.MethodPatch, location, "alice", body, map[string]string{"Upload-Offset": "0"}).Code
		}(body)
	}
	wg.Wait()
	close(results)
	counts := map[int]int{}
	for code := range results {
		counts[code]++
	}
	if counts[http.StatusNoContent] != 1 || counts[http.StatusConflict] != 1 {
		t.Fatalf("offset race: %+v", counts)
	}
	if _, err := s.Lookup(context.Background(), "alice", id); err == nil {
		t.Fatal("racing chunks combined despite identical offsets")
	}
	w := request(s, http.MethodHead, location, "alice", "", nil)
	if w.Header().Get("Upload-Offset") != "3" {
		t.Fatalf("bad offset after race: %v", w.Header())
	}
}

type interruptedReader struct {
	calls int
	stop  context.CancelFunc
}

func (r *interruptedReader) Read(p []byte) (int, error) {
	r.calls++
	if r.calls == 2 {
		r.stop()
	}
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func TestTusCancelledChunkPersistsResumableOffset(t *testing.T) {
	config := uploadConfig(t)
	config.MaxSize, config.Quota = 1<<20, 4<<20
	s := newFixture(t, config)
	location, id := uploadLocation(t, create(s, "alice", "large", 256<<10))
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), ownerKey{}, "alice"))
	defer cancel()
	u, err := s.GetUpload(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	n, err := u.WriteChunk(ctx, 0, &interruptedReader{stop: cancel})
	if !errors.Is(err, context.Canceled) || n != 128<<10 {
		t.Fatalf("interrupted chunk: %d %v", n, err)
	}
	w := request(s, http.MethodHead, location, "alice", "", nil)
	if w.Code != http.StatusOK || w.Header().Get("Upload-Offset") != strconv.FormatInt(n, 10) {
		t.Fatalf("partial offset: %d %v", w.Code, w.Header())
	}
	w = request(s, http.MethodPatch, location, "alice", strings.Repeat("y", 128<<10), map[string]string{"Upload-Offset": strconv.FormatInt(n, 10)})
	if w.Code != http.StatusNoContent {
		t.Fatalf("partial resume: %d %s", w.Code, w.Body.String())
	}
	completed, err := s.Lookup(context.Background(), "alice", id)
	if err != nil {
		t.Fatal(err)
	}
	completed.File.Close()
}
