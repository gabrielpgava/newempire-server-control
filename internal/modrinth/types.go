// Package modrinth plans and stages verified Fabric server content without owning runtime jobs.
package modrinth

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"
	"unicode"
)

const (
	LockFilename       = ".newempire-content-lock.json"
	MaxFileSize  int64 = 512 << 20
	MaxTotalSize int64 = 8 << 30
	MaxFiles           = 4096
	MaxProjects        = 256
)

type Target struct {
	MinecraftVersion string `json:"minecraft_version"`
	Loader           string `json:"loader"`
	LoaderVersion    string `json:"loader_version,omitempty"`
}

func (t Target) Validate() error {
	if t.Loader != "" && t.Loader != "fabric" {
		return errors.New("only Fabric mods are supported; plugins require a separate provider")
	}
	if t.MinecraftVersion == "" || len(t.MinecraftVersion) > 128 || strings.ContainsAny(t.MinecraftVersion, "\x00\r\n") {
		return errors.New("an exact Minecraft version is required")
	}
	return nil
}

type Project struct {
	ID          string   `json:"id"`
	Slug        string   `json:"slug"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	ProjectType string   `json:"project_type"`
	ServerSide  string   `json:"server_side"`
	Environment []string `json:"environment"`
	Loaders     []string `json:"loaders"`
	License     struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		URL  string `json:"url"`
	} `json:"license"`
}

type SearchHit struct {
	ProjectID   string   `json:"project_id"`
	Slug        string   `json:"slug"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	ProjectType string   `json:"project_type"`
	ServerSide  string   `json:"server_side"`
	Environment []string `json:"environment"`
	Categories  []string `json:"categories"`
}

type SearchResult struct {
	Hits      []SearchHit `json:"hits"`
	Offset    int         `json:"offset"`
	Limit     int         `json:"limit"`
	TotalHits int         `json:"total_hits"`
}

type Dependency struct {
	ProjectID string `json:"project_id"`
	VersionID string `json:"version_id"`
	FileName  string `json:"file_name"`
	Type      string `json:"dependency_type"`
}

type File struct {
	Hashes   map[string]string `json:"hashes"`
	URL      string            `json:"url"`
	Filename string            `json:"filename"`
	Primary  bool              `json:"primary"`
	Size     int64             `json:"size"`
}

type Version struct {
	ID            string       `json:"id"`
	ProjectID     string       `json:"project_id"`
	Name          string       `json:"name"`
	VersionNumber string       `json:"version_number"`
	VersionType   string       `json:"version_type"`
	DatePublished time.Time    `json:"date_published"`
	GameVersions  []string     `json:"game_versions"`
	Loaders       []string     `json:"loaders"`
	Environment   string       `json:"environment"`
	Dependencies  []Dependency `json:"dependencies"`
	Files         []File       `json:"files"`
}

// Compatible rejects ambiguous metadata instead of assuming that a client mod can run on a server.
func Compatible(p Project, v Version, target Target, allowPrerelease bool) error {
	if err := target.Validate(); err != nil {
		return err
	}
	if p.ID == "" || v.ProjectID != p.ID || v.ID == "" {
		return errors.New("project/version identity is missing or inconsistent")
	}
	if p.ProjectType != "mod" {
		return fmt.Errorf("project %s is not a Fabric mod", p.ID)
	}
	if !slices.Contains(v.Loaders, "fabric") || !slices.Contains(v.GameVersions, target.MinecraftVersion) {
		return fmt.Errorf("version %s is incompatible with Minecraft %s / Fabric", v.ID, target.MinecraftVersion)
	}
	if v.VersionType != "release" && (!allowPrerelease || (v.VersionType != "beta" && v.VersionType != "alpha")) {
		return fmt.Errorf("version %s is not permitted by the release-channel policy", v.ID)
	}
	if v.Environment != "" && v.Environment != "unknown" {
		if !serverEnvironment(v.Environment) {
			return fmt.Errorf("version %s does not support dedicated servers (%s)", v.ID, v.Environment)
		}
		return nil
	}
	if len(p.Environment) > 0 && !slices.Contains(p.Environment, "unknown") {
		for _, env := range p.Environment {
			if !serverEnvironment(env) {
				return fmt.Errorf("project %s has ambiguous or client-only environments; exact version metadata is required", p.ID)
			}
		}
		return nil
	}
	if p.ServerSide != "required" && p.ServerSide != "optional" {
		return fmt.Errorf("project %s has unsupported or unknown server compatibility", p.ID)
	}
	return nil
}

func serverEnvironment(env string) bool {
	switch env {
	case "client_and_server", "client_only_server_optional", "server_only", "server_only_client_optional", "dedicated_server_only", "client_or_server", "client_or_server_prefers_both":
		return true
	}
	return false
}

// SafePath accepts portable slash-separated relative paths, not ZIP/OS traversal or links.
func SafePath(name string) bool {
	if name == "" || len(name) > 1024 || path.Clean(name) != name || strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\\:\x00\r\n") {
		return false
	}
	for _, c := range name {
		if unicode.IsControl(c) {
			return false
		}
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." || strings.TrimSpace(part) != part || strings.HasSuffix(part, ".") {
			return false
		}
	}
	return true
}

func safeFilename(name string) bool {
	return jarPath(name) && !strings.Contains(name, "/")
}

func jarPath(name string) bool {
	return SafePath(name) && strings.HasSuffix(strings.ToLower(name), ".jar")
}

// ValidateDownload deliberately does not accept mirrors, even if their hashes look plausible.
func ValidateDownload(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != "cdn.modrinth.com" || u.User != nil || u.Fragment != "" || !strings.HasPrefix(u.Path, "/data/") || !SafePath(strings.TrimPrefix(u.Path, "/")) {
		return errors.New("download must use an exact official Modrinth CDN HTTPS URL")
	}
	return nil
}

func PrimaryFile(v Version) (File, error) {
	var candidates []File
	for _, f := range v.Files {
		if safeFilename(f.Filename) {
			candidates = append(candidates, f)
		}
	}
	var chosen *File
	for i := range candidates {
		if candidates[i].Primary {
			if chosen != nil {
				return File{}, errors.New("version has multiple primary JAR files")
			}
			chosen = &candidates[i]
		}
	}
	if chosen == nil && len(candidates) == 1 {
		chosen = &candidates[0]
	}
	if chosen == nil {
		return File{}, errors.New("version does not identify one unambiguous JAR file")
	}
	if err := validateFile(*chosen); err != nil {
		return File{}, err
	}
	return *chosen, nil
}

func validateFile(f File) error {
	if !safeFilename(f.Filename) || !ValidHash(f.Hashes["sha512"], 128) || f.Size <= 0 || f.Size > MaxFileSize {
		return errors.New("invalid JAR filename, size, or SHA-512 metadata")
	}
	return ValidateDownload(f.URL)
}

func ValidHash(hash string, length int) bool {
	if len(hash) != length {
		return false
	}
	for _, c := range hash {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
