package updates

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

const releasesAPI = "https://api.github.com/repos/" + Repository + "/releases"

type githubAsset struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	URL  string `json:"browser_download_url"`
}

type githubRelease struct {
	Tag        string        `json:"tag_name"`
	Draft      bool          `json:"draft"`
	Prerelease bool          `json:"prerelease"`
	Notes      string        `json:"body"`
	Assets     []githubAsset `json:"assets"`
}

func (m *Manager) request(ctx context.Context, address string) (*http.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, problem(ErrInvalid, "invalid release URL")
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	req.Header.Set("User-Agent", "new-empire-control-verified-updater")
	resp, err := m.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, problem(ErrUpstream, "GitHub request failed")
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, problem(ErrUpstream, "GitHub returned HTTP %d", resp.StatusCode)
	}
	return resp, nil
}

func (m *Manager) fetch(ctx context.Context, address string, limit int64) ([]byte, error) {
	resp, err := m.request(ctx, address)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, problem(ErrUpstream, "cannot read release response")
	}
	if int64(len(b)) > limit {
		return nil, problem(ErrVerification, "release response exceeds its size limit")
	}
	return b, nil
}

func eligible(r githubRelease, c Channel) (version, bool) {
	if r.Draft || !strings.HasPrefix(r.Tag, "v") {
		return version{}, false
	}
	v, err := parseVersion(strings.TrimPrefix(r.Tag, "v"))
	if err != nil || r.Prerelease != (len(v.pre) > 0) || c == Stable && r.Prerelease {
		return version{}, false
	}
	return v, true
}

// Check verifies the newest eligible release, rather than trusting GitHub's latest ordering.
func (m *Manager) Check(ctx context.Context, c Channel) (CheckResult, error) {
	c, err := channel(c)
	result := CheckResult{CurrentVersion: m.opts.CurrentVersion, Channel: c}
	if err != nil {
		return result, err
	}
	current, _ := parseVersion(m.opts.CurrentVersion)
	type candidate struct {
		release githubRelease
		version version
	}
	var candidates []candidate
	for page := 1; ; page++ {
		if page > 10 {
			return result, problem(ErrUpstream, "release listing exceeds the 1,000-release safety limit")
		}
		b, err := m.fetch(ctx, fmt.Sprintf("%s?per_page=100&page=%d", releasesAPI, page), 4<<20)
		if err != nil {
			return result, err
		}
		var releases []githubRelease
		if err := json.Unmarshal(b, &releases); err != nil || releases == nil {
			return result, problem(ErrUpstream, "invalid GitHub releases response")
		}
		for _, r := range releases {
			if v, ok := eligible(r, c); ok && v.compare(current) > 0 {
				candidates = append(candidates, candidate{r, v})
			}
		}
		if len(releases) < 100 {
			break
		}
	}
	if len(candidates) == 0 {
		return result, nil
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].version.compare(candidates[j].version) > 0 })
	if len(candidates) > 1 && candidates[0].version.compare(candidates[1].version) == 0 {
		return result, problem(ErrVerification, "multiple releases have the same SemVer precedence")
	}
	mf, _, _, err := m.releaseManifest(ctx, candidates[0].release)
	if err != nil {
		return result, err
	}
	result.Available = &mf
	result.Notes = candidates[0].release.Notes
	result.Prerelease = candidates[0].release.Prerelease
	return result, nil
}

func downloadURL(v, name string) string {
	return "https://github.com/" + Repository + "/releases/download/" + url.PathEscape("v"+v) + "/" + url.PathEscape(name)
}

func officialDownload(address, v, name string) bool {
	u, err := url.Parse(address)
	want, _ := url.Parse(downloadURL(v, name))
	return err == nil && u.Scheme == "https" && u.Host == "github.com" && u.User == nil && u.Path == want.Path && u.RawQuery == "" && !u.ForceQuery && u.Fragment == ""
}

func releaseAsset(r githubRelease, name string) (githubAsset, error) {
	var found *githubAsset
	for _, a := range r.Assets {
		if a.Name == name {
			if found != nil {
				return githubAsset{}, problem(ErrVerification, "duplicate release asset %s", name)
			}
			copy := a
			found = &copy
		}
	}
	if found == nil || !officialDownload(found.URL, strings.TrimPrefix(r.Tag, "v"), name) || found.Size <= 0 {
		return githubAsset{}, problem(ErrVerification, "missing or nonofficial release asset %s", name)
	}
	return *found, nil
}

func (m *Manager) releaseManifest(ctx context.Context, r githubRelease) (Manifest, []byte, []byte, error) {
	var mf Manifest
	a, err := releaseAsset(r, "release.json")
	if err != nil {
		return mf, nil, nil, err
	}
	s, err := releaseAsset(r, "release.json.sig")
	if err != nil {
		return mf, nil, nil, err
	}
	if a.Size > maxManifestSize || s.Size != 88 {
		return mf, nil, nil, problem(ErrVerification, "invalid signed manifest asset sizes")
	}
	raw, err := m.fetch(ctx, a.URL, maxManifestSize)
	if err != nil {
		return mf, nil, nil, err
	}
	sig, err := m.fetch(ctx, s.URL, 88)
	if err != nil {
		return mf, nil, nil, err
	}
	if int64(len(raw)) != a.Size || int64(len(sig)) != s.Size {
		return mf, nil, nil, problem(ErrVerification, "manifest asset sizes do not match GitHub metadata")
	}
	mf, err = verifyManifest(raw, sig, m.key)
	if err != nil {
		return mf, nil, nil, err
	}
	if "v"+mf.Version != r.Tag {
		return mf, nil, nil, problem(ErrVerification, "signed manifest version does not match the release tag")
	}
	for _, asset := range mf.Assets {
		remote, err := releaseAsset(r, asset.Name)
		if err != nil {
			return mf, nil, nil, err
		}
		if remote.Size != asset.Size {
			return mf, nil, nil, problem(ErrVerification, "archive size does not match the signed manifest")
		}
	}
	return mf, raw, sig, nil
}
