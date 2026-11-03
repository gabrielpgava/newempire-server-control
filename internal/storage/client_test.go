package storage

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func config() Config {
	return Config{Enabled: true, Endpoint: "https://storage.example.com", Region: "auto", Bucket: "backups", AccessKey: "access", SecretKey: "secret-value", SessionToken: "session-value"}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// Only this explicit test transport maps a valid public origin to httptest.
func fakeHTTP(t *testing.T, handler http.HandlerFunc) *http.Client {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	target, _ := url.Parse(server.URL)
	transport := server.Client().Transport
	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		copy := req.Clone(req.Context())
		copy.URL.Scheme, copy.URL.Host = target.Scheme, target.Host
		copy.Host = req.URL.Host
		return transport.RoundTrip(copy)
	})}
}

func mustClient(t *testing.T, cfg Config, transport *http.Client) *Client {
	t.Helper()
	client, err := NewWithHTTPClient(cfg, transport)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestConfigValidationAndRedaction(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*Config)
	}{
		{"http", func(c *Config) { c.Endpoint = "http://storage.example.com" }},
		{"private", func(c *Config) { c.Endpoint = "https://10.0.0.1" }},
		{"loopback", func(c *Config) { c.Endpoint = "https://127.0.0.1" }},
		{"ipv6", func(c *Config) { c.Endpoint = "https://[::1]" }},
		{"mapped", func(c *Config) { c.Endpoint = "https://[::ffff:127.0.0.1]" }},
		{"metadata", func(c *Config) { c.Endpoint = "https://169.254.169.254" }},
		{"userinfo", func(c *Config) { c.Endpoint = "https://access:secret@storage.example.com" }},
		{"port", func(c *Config) { c.Endpoint = "https://storage.example.com:8443" }},
		{"empty port", func(c *Config) { c.Endpoint = "https://storage.example.com:" }},
		{"zoned IP", func(c *Config) { c.Endpoint = "https://[2606:4700:4700::1111%25en0]" }},
		{"query", func(c *Config) { c.Endpoint += "?secret=value" }},
		{"path", func(c *Config) { c.Endpoint += "/bucket" }},
		{"fragment", func(c *Config) { c.Endpoint += "#secret" }},
		{"empty fragment", func(c *Config) { c.Endpoint += "#" }},
		{"hostname", func(c *Config) { c.Endpoint = "https://bad_host.example.com" }},
		{"region", func(c *Config) { c.Region = "auto/s3" }},
		{"bucket path", func(c *Config) { c.Bucket = "../backups" }},
		{"bucket ip", func(c *Config) { c.Bucket = "1.2.3.4" }},
		{"bucket dots", func(c *Config) { c.Bucket = "my..backups" }},
		{"prefix", func(c *Config) { c.Prefix = "../backups" }},
		{"credential", func(c *Config) { c.SecretKey = "secret\r\ninjected" }},
		{"missing credential", func(c *Config) { c.AccessKey = "" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := config()
			test.edit(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("invalid configuration accepted")
			}
			if cfg.Redacted().Configured {
				t.Fatal("invalid configuration reported configured")
			}
		})
	}
	cfg := config()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(cfg.Redacted())
	if string(encoded) != `{"configured":true}` {
		t.Fatalf("unexpected redacted output: %s", encoded)
	}
	if err := (Config{}).Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{}); err == nil {
		t.Fatal("disabled configuration accepted")
	}
	if _, err := NewWithHTTPClient(cfg, nil); err == nil {
		t.Fatal("nil HTTP client accepted")
	}
}

func TestVerifiedTransferRoundTrip(t *testing.T) {
	cfg := config()
	cfg.Prefix = "nec"
	content := strings.Repeat("verified local backup\n", 5000)
	var mu sync.Mutex
	var data, checksum string
	transport := fakeHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backups/nec/instance/backup.zip" {
			t.Errorf("unexpected object path %q", r.URL.Path)
		}
		if !strings.Contains(r.Header.Get("Authorization"), "Credential=access/") || r.Header.Get("X-Amz-Security-Token") != cfg.SessionToken {
			t.Error("request not signed with temporary credentials")
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case "PUT":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
			}
			data = string(body)
			checksum = r.Header.Get("X-Amz-Meta-Sha256")
			if checksum != hashHex(data) || checksum != r.Header.Get("X-Amz-Content-Sha256") || r.ContentLength != int64(len(data)) {
				t.Error("payload hash or length did not match transmitted bytes")
			}
		case "HEAD":
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			w.Header().Set("X-Amz-Meta-Sha256", checksum)
		case "GET":
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			io.WriteString(w, data)
		default:
			w.WriteHeader(405)
		}
	})
	client := mustClient(t, cfg, transport)
	dir := t.TempDir()
	source := filepath.Join(dir, "source.zip")
	if err := os.WriteFile(source, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	object, err := client.Put(context.Background(), "instance/backup.zip", source)
	if err != nil {
		t.Fatal(err)
	}
	if object.Key != "instance/backup.zip" || object.Verification != "metadata" || object.SHA256 != hashHex(content) || object.Size != int64(len(content)) {
		t.Fatalf("incorrect upload result: %+v", object)
	}
	destination := filepath.Join(dir, "restored.zip")
	verified, err := client.Download(context.Background(), object.Key, destination, object.SHA256, object.Size)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Verification != "download_sha256" {
		t.Fatal("download did not distinguish actual-byte verification")
	}
	got, err := os.ReadFile(destination)
	if err != nil || string(got) != content {
		t.Fatal("downloaded bytes differ", err)
	}
	info, _ := os.Stat(destination)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("staging permissions: %o", info.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("partial file was left behind: %v", entries)
	}
}

func TestMetadataIsNotActualByteVerification(t *testing.T) {
	client := mustClient(t, config(), fakeHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "4")
		w.Header().Set("X-Amz-Meta-Sha256", hashHex("good"))
		if r.Method == "GET" {
			io.WriteString(w, "evil")
		}
	}))
	object, err := client.Head(context.Background(), "backup.zip")
	if err != nil || object.Verification != "metadata" {
		t.Fatalf("HEAD: %+v, %v", object, err)
	}
	dir := t.TempDir()
	destination := filepath.Join(dir, "download.zip")
	if _, err := client.Download(context.Background(), "backup.zip", destination, hashHex("good"), 4); err == nil {
		t.Fatal("corrupted remote bytes were trusted")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("failed download exposed a file: %v", entries)
	}
}

func TestUnsafeKeysNeverReachTransport(t *testing.T) {
	client := mustClient(t, config(), &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("unsafe path reached transport")
		return nil, errors.New("unexpected")
	})})
	for _, key := range []string{"", "/absolute", "../backup", "a/../backup", "a/./backup", "a//backup", "a/", `a\backup`, "a/%2e%2e/secret", "key?query", "key#fragment", "a\nheader", strings.Repeat("a", 1025)} {
		if _, err := client.Head(context.Background(), key); err == nil {
			t.Errorf("accepted unsafe key %q", key)
		}
	}
}

func TestDownloadRejectsUntrustedSizeAndEncoding(t *testing.T) {
	for _, test := range []struct{ name, size, encoding string }{
		{"length", "5", ""}, {"encoding", "4", "gzip"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := mustClient(t, config(), fakeHTTP(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", test.size)
				w.Header().Set("Content-Encoding", test.encoding)
				io.WriteString(w, "evil")
			}))
			dir := t.TempDir()
			if _, err := client.Download(context.Background(), "backup.zip", filepath.Join(dir, "download"), hashHex("evil"), 4); err == nil {
				t.Fatal("invalid remote representation accepted")
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 0 {
				t.Fatal("partial download left behind")
			}
		})
	}
}

func TestDownloadDoesNotReplaceExistingOrRacedDestination(t *testing.T) {
	for _, race := range []bool{false, true} {
		t.Run(strconv.FormatBool(race), func(t *testing.T) {
			dir := t.TempDir()
			destination := filepath.Join(dir, "existing")
			var calls atomic.Int32
			client := mustClient(t, config(), fakeHTTP(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if err := os.WriteFile(destination, []byte("keep"), 0600); err != nil {
					t.Error(err)
				}
				w.Header().Set("Content-Length", "4")
				io.WriteString(w, "good")
			}))
			if !race {
				if err := os.WriteFile(destination, []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := client.Download(context.Background(), "backup", destination, hashHex("good"), 4); err == nil {
				t.Fatal("overwrote existing destination")
			}
			got, _ := os.ReadFile(destination)
			if string(got) != "keep" || (!race && calls.Load() != 0) {
				t.Fatal("existing file changed or was unnecessarily fetched")
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 1 {
				t.Fatal("partial file left behind")
			}
		})
	}
}

func TestSourceSymlinksAndInvalidFilesAreRejected(t *testing.T) {
	client := mustClient(t, config(), &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("invalid local file reached transport")
		return nil, errors.New("unexpected")
	})})
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	os.WriteFile(source, []byte("good"), 0600)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(source, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"relative", dir, link, filepath.Join(dir, "missing")} {
		if _, err := client.Put(context.Background(), "backup", path); err == nil {
			t.Errorf("accepted invalid source %q", path)
		}
	}
	if _, err := client.Download(context.Background(), "backup", link, hashHex("good"), 4); err == nil {
		t.Fatal("accepted symlink destination")
	}
}

func TestPutRejectsChangedSourceAndHeadMismatch(t *testing.T) {
	for _, mode := range []string{"source", "head_size", "head_hash", "head_missing_hash"} {
		t.Run(mode, func(t *testing.T) {
			source := filepath.Join(t.TempDir(), "source")
			os.WriteFile(source, []byte("good"), 0600)
			transport := fakeHTTP(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "PUT" {
					io.Copy(io.Discard, r.Body)
					return
				}
				w.Header().Set("Content-Length", "4")
				w.Header().Set("X-Amz-Meta-Sha256", hashHex("good"))
				switch mode {
				case "head_size":
					w.Header().Set("Content-Length", "5")
				case "head_hash":
					w.Header().Set("X-Amz-Meta-Sha256", hashHex("evil"))
				case "head_missing_hash":
					w.Header().Del("X-Amz-Meta-Sha256")
				}
			})
			if mode == "source" {
				next := transport.Transport
				transport.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
					if req.Method == "PUT" {
						if err := os.WriteFile(source, []byte("evil"), 0600); err != nil {
							t.Error(err)
						}
					}
					return next.RoundTrip(req)
				})
			}
			client := mustClient(t, config(), transport)
			if _, err := client.Put(context.Background(), "backup", source); err == nil {
				t.Fatal("unsafe upload reported successful")
			}
		})
	}
}

func TestBoundedRetriesCancellationAndRedactedErrors(t *testing.T) {
	var calls atomic.Int32
	client := mustClient(t, config(), fakeHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(503)
		io.WriteString(w, "secret-value session-value https://credential-destination.example.com")
	}))
	_, err := client.Head(context.Background(), "backup")
	if err == nil || calls.Load() != 3 || strings.Contains(err.Error(), "secret-value") || strings.Contains(err.Error(), "https://") {
		t.Fatalf("retries or redaction: calls=%d err=%v", calls.Load(), err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = client.Head(ctx, "backup")
	if !errors.Is(err, context.Canceled) || calls.Load() != 3 {
		t.Fatalf("cancellation ignored: %v", err)
	}

	client = mustClient(t, config(), fakeHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	ctx, cancel = context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	_, err = client.Head(ctx, "backup")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline not preserved: %v", err)
	}

	client = mustClient(t, config(), &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("credential: secret-value at https://private-destination.example.com")
	})})
	_, err = client.Head(context.Background(), "backup")
	if err == nil || strings.Contains(err.Error(), "secret-value") || strings.Contains(err.Error(), "destination") {
		t.Fatalf("transport error not redacted: %v", err)
	}
}

func TestRedirectsNeverForwardSignedCredentials(t *testing.T) {
	for _, location := range []string{"https://169.254.169.254/latest", "https://storage.example.com/other", "http://storage.example.com/other"} {
		t.Run(location, func(t *testing.T) {
			var calls atomic.Int32
			transport := fakeHTTP(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Location", location)
				w.WriteHeader(307)
			})
			client := mustClient(t, config(), transport)
			if _, err := client.Head(context.Background(), "backup"); err == nil || calls.Load() != 1 {
				t.Fatalf("redirect followed: calls=%d err=%v", calls.Load(), err)
			}
		})
	}
}

func TestEmptyBackupAndCancelledDownload(t *testing.T) {
	var calls atomic.Int32
	client := mustClient(t, config(), fakeHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method == "HEAD" {
			w.Header().Set("X-Amz-Meta-Sha256", emptySHA256)
		}
		w.Header().Set("Content-Length", "0")
	}))
	dir := t.TempDir()
	source := filepath.Join(dir, "empty")
	os.WriteFile(source, nil, 0600)
	if _, err := client.Put(context.Background(), "empty", source); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	destination := filepath.Join(dir, "cancelled")
	if _, err := client.Download(ctx, "empty", destination, emptySHA256, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("download cancellation: %v", err)
	}
	if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancelled destination exists")
	}
	if calls.Load() != 2 {
		t.Fatal("cancelled download reached transport")
	}
}

func TestTransferRetriesReplayOnlyCompleteBytes(t *testing.T) {
	var uploads, downloads atomic.Int32
	content := strings.Repeat("verified", 500)
	client := mustClient(t, config(), fakeHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "PUT":
			body, err := io.ReadAll(r.Body)
			if err != nil || string(body) != content {
				t.Error("upload retry did not replay the complete source", err)
			}
			if uploads.Add(1) == 1 {
				w.WriteHeader(503)
			}
		case "HEAD":
			w.Header().Set("Content-Length", strconv.Itoa(len(content)))
			w.Header().Set("X-Amz-Meta-Sha256", hashHex(content))
		case "GET":
			w.Header().Set("Content-Length", strconv.Itoa(len(content)))
			if downloads.Add(1) == 1 {
				io.WriteString(w, "truncated")
				return
			}
			io.WriteString(w, content)
		}
	}))
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	os.WriteFile(source, []byte(content), 0600)
	object, err := client.Put(context.Background(), "backup", source)
	if err != nil || uploads.Load() != 2 {
		t.Fatalf("upload replay: attempts=%d err=%v", uploads.Load(), err)
	}
	destination := filepath.Join(dir, "download")
	if _, err := client.Download(context.Background(), "backup", destination, object.SHA256, object.Size); err != nil || downloads.Load() != 2 {
		t.Fatalf("download retry: attempts=%d err=%v", downloads.Load(), err)
	}
	data, _ := os.ReadFile(destination)
	if string(data) != content {
		t.Fatal("download retry retained partial bytes")
	}
}

func TestCancellationCleansPartiallyDownloadedBytes(t *testing.T) {
	client := mustClient(t, config(), fakeHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000000")
		io.WriteString(w, "partial bytes")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := client.Download(ctx, "backup", filepath.Join(dir, "download"), hashHex(strings.Repeat("x", 1000000)), 1000000)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("in-flight download deadline not preserved: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("cancelled download left partial files: %v", entries)
	}
}
