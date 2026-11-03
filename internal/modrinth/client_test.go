package modrinth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
func jsonResponse(status int, value any) *http.Response {
	data, _ := json.Marshal(value)
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(data))), ContentLength: int64(len(data))}
}

var testTarget = Target{MinecraftVersion: "test-game", Loader: "fabric", LoaderVersion: "test-loader"}

func TestOfficialAPIRequests(t *testing.T) {
	ctx := context.Background()
	hash := strings.Repeat("a", 128)
	var paths []string
	client := NewClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Scheme != "https" || req.URL.Host != "api.modrinth.com" || !strings.Contains(req.UserAgent(), "NewEmpireServerControl/") {
			t.Errorf("unsafe/missing request identity: %s %s", req.URL, req.UserAgent())
		}
		paths = append(paths, req.URL.Path)
		switch req.URL.Path {
		case "/v2/search":
			if req.URL.Query().Get("query") != "a&b" || !strings.Contains(req.URL.Query().Get("facets"), "categories:fabric") {
				t.Error("search did not encode/filter query")
			}
			return jsonResponse(200, SearchResult{Hits: []SearchHit{{ProjectID: "A"}}}), nil
		case "/v2/project/A":
			return jsonResponse(200, Project{ID: "A", ProjectType: "mod"}), nil
		case "/v2/projects":
			if req.URL.Query().Get("ids") != `["A"]` {
				t.Error("wrong project IDs")
			}
			return jsonResponse(200, []Project{{ID: "A"}}), nil
		case "/v2/version/v1":
			return jsonResponse(200, Version{ID: "v1", ProjectID: "A"}), nil
		case "/v2/project/A/version":
			if req.URL.Query().Get("loaders") != `["fabric"]` || req.URL.Query().Get("game_versions") != `["test-game"]` {
				t.Error("missing exact runtime filters")
			}
			return jsonResponse(200, []Version{{ID: "v1", ProjectID: "A", Loaders: []string{"fabric"}, GameVersions: []string{"test-game"}, VersionType: "release"}, {ID: "beta", ProjectID: "A", Loaders: []string{"fabric"}, GameVersions: []string{"test-game"}, VersionType: "beta"}}), nil
		case "/v2/version_files", "/v2/version_files/update":
			if req.Method != http.MethodPost || req.Header.Get("Content-Type") != "application/json" {
				t.Error("hash endpoint must use JSON POST")
			}
			var body map[string]json.RawMessage
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if string(body["algorithm"]) != `"sha512"` || string(body["hashes"]) != `["`+hash+`"]` {
				t.Error("wrong hash request")
			}
			if strings.HasSuffix(req.URL.Path, "/update") && (string(body["version_types"]) != `["release"]` || string(body["loaders"]) != `["fabric"]`) {
				t.Error("update is not stable/Fabric filtered")
			}
			return jsonResponse(200, map[string]Version{hash: {ID: "v1", ProjectID: "A", Files: []File{{Hashes: map[string]string{"sha512": hash}}}}}), nil
		}
		t.Fatalf("unexpected URL: %s", req.URL)
		return nil, nil
	})})
	if _, err := client.Search(ctx, "a&b", testTarget, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Project(ctx, "A"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Projects(ctx, []string{"A"}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Version(ctx, "v1"); err != nil {
		t.Fatal(err)
	}
	if versions, err := client.Versions(ctx, "A", testTarget, false); err != nil || len(versions) != 1 {
		t.Fatalf("versions=%v error=%v", versions, err)
	}
	if _, err := client.VersionsFromHashes(ctx, []string{hash}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.UpdatesFromHashes(ctx, []string{hash}, testTarget, false); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 7 {
		t.Fatal(paths)
	}
}

func TestRateLimitRetryCancellationAndBudget(t *testing.T) {
	t.Run("retry and headers", func(t *testing.T) {
		calls := 0
		client := NewClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			resp := jsonResponse(200, Project{ID: "A"})
			if calls == 1 {
				resp.StatusCode = 429
			}
			resp.Header.Set("X-Ratelimit-Limit", "300")
			resp.Header.Set("X-Ratelimit-Remaining", "0")
			resp.Header.Set("X-Ratelimit-Reset", "0.01")
			return resp, nil
		})})
		if _, err := client.Project(context.Background(), "A"); err != nil {
			t.Fatal(err)
		}
		if calls != 2 || client.RateLimit().Limit != 300 || client.RateLimit().Remaining != 0 {
			t.Fatal(calls, client.RateLimit())
		}
	})
	t.Run("cancel backoff", func(t *testing.T) {
		calls := 0
		ctx, cancel := context.WithCancel(context.Background())
		client := NewClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			cancel()
			return jsonResponse(429, map[string]string{}), nil
		})})
		if _, err := client.Project(ctx, "A"); !errors.Is(err, context.Canceled) {
			t.Fatalf("expected canceled, got %v", err)
		}
		if calls != 1 {
			t.Fatal(calls)
		}
	})
	t.Run("long retry-after fails without early retry", func(t *testing.T) {
		calls := 0
		client := NewClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			resp := jsonResponse(429, nil)
			resp.Header.Set("Retry-After", "3600")
			return resp, nil
		})})
		_, err := client.Project(context.Background(), "A")
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.RetryAfter < time.Hour || calls != 1 {
			t.Fatal(err, calls)
		}
	})
	t.Run("bounded retries", func(t *testing.T) {
		calls := 0
		client := NewClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) { calls++; return jsonResponse(503, nil), nil })})
		if _, err := client.Project(context.Background(), "A"); err == nil || calls != 3 {
			t.Fatal(err, calls)
		}
	})
}

func TestAPIResponseBoundsAndIdentity(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response func() *http.Response
	}{
		{"oversized", func() *http.Response {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(strings.Repeat(" ", maxResponse+1)))}
		}},
		{"invalid JSON", func() *http.Response {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":`))}
		}},
		{"unexpected version", func() *http.Response { return jsonResponse(200, Version{ID: "other"}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := NewClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return tc.response(), nil })})
			if _, err := client.Version(context.Background(), "v1"); err == nil {
				t.Fatal("accepted invalid response")
			}
		})
	}
	client := NewClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(200, map[string]Version{strings.Repeat("b", 128): {ID: "v1", ProjectID: "A"}}), nil
	})})
	if _, err := client.VersionsFromHashes(context.Background(), []string{strings.Repeat("a", 128)}); err == nil {
		t.Fatal("accepted unrequested hash")
	}
}

func TestDownloadURLAndRedirectGuards(t *testing.T) {
	for _, raw := range []string{"http://cdn.modrinth.com/data/A/a.jar", "https://cdn.modrinth.com.evil/data/A/a.jar", "https://cdn.modrinth.com:443/data/A/a.jar", "https://user@cdn.modrinth.com/data/A/a.jar", "https://cdn.modrinth.com/data/../a.jar", "https://127.0.0.1/data/a.jar", "https://cdn.modrinth.com/data/%2e%2e/a.jar"} {
		if ValidateDownload(raw) == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	if err := ValidateDownload("https://cdn.modrinth.com/data/A/versions/v1/a.jar"); err != nil {
		t.Fatal(err)
	}
	client := NewClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		resp := jsonResponse(302, nil)
		resp.Header.Set("Location", "https://127.0.0.1/private")
		return resp, nil
	})})
	if _, err := client.Project(context.Background(), "A"); err == nil {
		t.Fatal("accepted unsafe redirect")
	}
}

func TestVersionsAcceptsProjectSlugs(t *testing.T) {
	c := newCatalog()
	c.add("A", "v1", "jar")
	versions, err := c.client(t).Versions(context.Background(), "a", testTarget, false)
	if err != nil || len(versions) != 1 || versions[0].ProjectID != "A" {
		t.Fatal(versions, err)
	}
}
