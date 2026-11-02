package modrinth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gabrielpgava/newempire-server-control/internal/netguard"
)

const apiBase = "https://api.modrinth.com/v2"
const userAgent = "NewEmpireServerControl/1 (https://github.com/gabrielpgava/newempire-server-control)"
const maxResponse = 16 << 20
const maxBackoff = 5 * time.Second

type RateLimit struct {
	Limit      int           `json:"limit"`
	Remaining  int           `json:"remaining"`
	ResetAfter time.Duration `json:"reset_after"`
}

type APIError struct {
	Status     int
	RetryAfter time.Duration
}

func (e *APIError) Error() string { return fmt.Sprintf("Modrinth API returned HTTP %d", e.Status) }

type Client struct {
	http *http.Client
	mu   sync.Mutex
	rate RateLimit
}

func NewClient(client *http.Client) *Client {
	if client == nil {
		client = netguard.New("api.modrinth.com", "cdn.modrinth.com")
	}
	return &Client{http: client}
}

func (c *Client) RateLimit() RateLimit { c.mu.Lock(); defer c.mu.Unlock(); return c.rate }

func (c *Client) do(ctx context.Context, method, endpoint string, body any, out any) error {
	var payload []byte
	var err error
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, apiBase+endpoint, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.send(req, "api.modrinth.com")
		if err != nil {
			return err
		}
		rate := RateLimit{Limit: headerInt(resp.Header, "X-Ratelimit-Limit"), Remaining: headerInt(resp.Header, "X-Ratelimit-Remaining"), ResetAfter: headerSeconds(resp.Header.Get("X-Ratelimit-Reset"))}
		c.mu.Lock()
		c.rate = rate
		c.mu.Unlock()
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
		closeErr := resp.Body.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		if len(data) > maxResponse {
			return errors.New("Modrinth API response exceeds the size limit")
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if err := json.Unmarshal(data, out); err != nil {
				return fmt.Errorf("invalid Modrinth API JSON: %w", err)
			}
			return nil
		}
		delay := retryDelay(resp.Header, attempt, resp.StatusCode)
		apiErr := &APIError{Status: resp.StatusCode, RetryAfter: delay}
		if attempt == 2 || (resp.StatusCode != 429 && resp.StatusCode != 502 && resp.StatusCode != 503 && resp.StatusCode != 504) || delay > maxBackoff {
			return apiErr
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return errors.New("Modrinth retry budget exhausted")
}

func (c *Client) send(req *http.Request, host string) (*http.Response, error) {
	client := *c.http
	original := client.CheckRedirect
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if len(via) >= 5 || next.URL.Scheme != "https" || next.URL.Host != host || next.URL.User != nil {
			return errors.New("unsafe Modrinth redirect")
		}
		if original != nil {
			return original(next, via)
		}
		return nil
	}
	return client.Do(req)
}

func headerInt(h http.Header, name string) int {
	n, err := strconv.Atoi(h.Get(name))
	if err != nil || n < 0 {
		return -1
	}
	return n
}
func headerSeconds(value string) time.Duration {
	n, err := strconv.ParseFloat(value, 64)
	if err != nil || n <= 0 || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0
	}
	if n > 86400 {
		return 24 * time.Hour
	}
	return time.Duration(n * float64(time.Second))
}
func retryDelay(h http.Header, attempt, status int) time.Duration {
	delay := time.Duration(1<<attempt) * 250 * time.Millisecond
	if d := headerSeconds(h.Get("X-Ratelimit-Reset")); d > delay && (status == http.StatusTooManyRequests || h.Get("X-Ratelimit-Remaining") == "0") {
		delay = d
	}
	if d := headerSeconds(h.Get("Retry-After")); d > delay {
		delay = d
	}
	if at, err := http.ParseTime(h.Get("Retry-After")); err == nil && time.Until(at) > delay {
		delay = time.Until(at)
	}
	return delay
}

func jsonQuery(v any) string { b, _ := json.Marshal(v); return string(b) }
func validID(id string) bool {
	if id == "" || len(id) > 128 || id == "." || id == ".." {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func (c *Client) Search(ctx context.Context, query string, target Target, offset int) (SearchResult, error) {
	result := SearchResult{Hits: []SearchHit{}}
	if err := target.Validate(); err != nil {
		return result, err
	}
	if len(query) > 512 || offset < 0 || offset > 100000 {
		return result, errors.New("invalid search query or offset")
	}
	facets := [][]string{{"project_type:mod"}, {"categories:fabric"}, {"versions:" + target.MinecraftVersion}, {"server_side:required", "server_side:optional"}}
	q := url.Values{"query": {query}, "facets": {jsonQuery(facets)}, "limit": {"20"}, "offset": {strconv.Itoa(offset)}}
	err := c.do(ctx, http.MethodGet, "/search?"+q.Encode(), nil, &result)
	return result, err
}

func (c *Client) Project(ctx context.Context, id string) (Project, error) {
	var result Project
	if !validID(id) {
		return result, errors.New("invalid Modrinth project identifier")
	}
	err := c.do(ctx, http.MethodGet, "/project/"+url.PathEscape(id), nil, &result)
	if err != nil {
		return result, fmt.Errorf("Modrinth project %s: %w", id, err)
	}
	if err == nil && !validID(result.ID) {
		err = errors.New("Modrinth project response has an invalid identity")
	}
	return result, err
}

func (c *Client) Projects(ctx context.Context, ids []string) ([]Project, error) {
	result := []Project{}
	if len(ids) > 100 {
		return nil, errors.New("at most 100 projects can be requested together")
	}
	for _, id := range ids {
		if !validID(id) {
			return nil, errors.New("invalid Modrinth project identifier")
		}
	}
	if len(ids) == 0 {
		return result, nil
	}
	err := c.do(ctx, http.MethodGet, "/projects?"+url.Values{"ids": {jsonQuery(ids)}}.Encode(), nil, &result)
	return result, err
}

func (c *Client) Version(ctx context.Context, id string) (Version, error) {
	var result Version
	if !validID(id) {
		return result, errors.New("invalid Modrinth version identifier")
	}
	err := c.do(ctx, http.MethodGet, "/version/"+url.PathEscape(id), nil, &result)
	if err != nil {
		return result, fmt.Errorf("Modrinth version %s: %w", id, err)
	}
	if err == nil && result.ID != id {
		err = errors.New("Modrinth version response identity mismatch")
	}
	return result, err
}

func (c *Client) Versions(ctx context.Context, project string, target Target, allowPrerelease bool) ([]Version, error) {
	result := []Version{}
	if !validID(project) {
		return nil, errors.New("invalid Modrinth project identifier")
	}
	if err := target.Validate(); err != nil {
		return nil, err
	}
	q := url.Values{"loaders": {`["fabric"]`}, "game_versions": {jsonQuery([]string{target.MinecraftVersion})}}
	if err := c.do(ctx, http.MethodGet, "/project/"+url.PathEscape(project)+"/version?"+q.Encode(), nil, &result); err != nil {
		return nil, err
	}
	result = slices.DeleteFunc(result, func(v Version) bool {
		return !validID(v.ProjectID) || !validID(v.ID) || !slices.Contains(v.Loaders, "fabric") || !slices.Contains(v.GameVersions, target.MinecraftVersion) || (v.VersionType != "release" && (!allowPrerelease || v.VersionType != "beta" && v.VersionType != "alpha"))
	})
	slices.SortFunc(result, func(a, b Version) int {
		if d := b.DatePublished.Compare(a.DatePublished); d != 0 {
			return d
		}
		return strings.Compare(a.ID, b.ID)
	})
	return result, nil
}

func (c *Client) VersionsFromHashes(ctx context.Context, hashes []string) (map[string]Version, error) {
	return c.hashes(ctx, "/version_files", hashes, nil, false)
}
func (c *Client) UpdatesFromHashes(ctx context.Context, hashes []string, target Target, allowPrerelease bool) (map[string]Version, error) {
	if err := target.Validate(); err != nil {
		return nil, err
	}
	return c.hashes(ctx, "/version_files/update", hashes, &target, allowPrerelease)
}
func (c *Client) hashes(ctx context.Context, endpoint string, hashes []string, target *Target, allowPrerelease bool) (map[string]Version, error) {
	result := make(map[string]Version)
	if len(hashes) > MaxFiles {
		return nil, errors.New("too many file hashes")
	}
	for _, hash := range hashes {
		if !ValidHash(hash, 128) {
			return nil, errors.New("invalid SHA-512 hash")
		}
	}
	for start := 0; start < len(hashes); start += 100 {
		batch := hashes[start:min(start+100, len(hashes))]
		body := map[string]any{"hashes": batch, "algorithm": "sha512"}
		if target != nil {
			body["loaders"] = []string{"fabric"}
			body["game_versions"] = []string{target.MinecraftVersion}
			body["version_types"] = []string{"release"}
			if allowPrerelease {
				body["version_types"] = []string{"release", "beta", "alpha"}
			}
		}
		var found map[string]Version
		if err := c.do(ctx, http.MethodPost, endpoint, body, &found); err != nil {
			return nil, err
		}
		for hash, version := range found {
			if !slices.Contains(batch, hash) || !validID(version.ID) || !validID(version.ProjectID) {
				return nil, errors.New("Modrinth hash response has an unexpected identity")
			}
			if target == nil {
				matched := false
				for _, f := range version.Files {
					if f.Hashes["sha512"] == hash {
						matched = true
					}
				}
				if !matched {
					return nil, errors.New("Modrinth hash response does not contain the requested file")
				}
			}
			result[hash] = version
		}
	}
	return result, nil
}
