// Package modpacks imports and exports conservative server-only Modrinth packs.
package modpacks

import (
	"archive/zip"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/gabrielpgava/newempire-server-control/internal/modrinth"
)

type Environment struct {
	Client string `json:"client"`
	Server string `json:"server"`
}
type File struct {
	Path      string            `json:"path"`
	Hashes    map[string]string `json:"hashes"`
	Env       *Environment      `json:"env,omitempty"`
	Downloads []string          `json:"downloads"`
	FileSize  int64             `json:"fileSize"`
}
type Index struct {
	FormatVersion int               `json:"formatVersion"`
	Game          string            `json:"game"`
	VersionID     string            `json:"versionId"`
	Name          string            `json:"name"`
	Summary       string            `json:"summary,omitempty"`
	Files         []File            `json:"files"`
	Dependencies  map[string]string `json:"dependencies"`
}
type override struct {
	file *zip.File
	hash string
}
type Pack struct {
	Index     Index
	Warnings  []string
	target    modrinth.Target
	overrides map[string]override
}

// Read never extracts ZIP entries. Keep readerAt open until Stage finishes.
func Read(readerAt io.ReaderAt, size int64, target modrinth.Target) (*Pack, error) {
	if err := target.Validate(); err != nil {
		return nil, err
	}
	if size <= 0 || size > modrinth.MaxTotalSize {
		return nil, errors.New("modpack archive exceeds the size limit")
	}
	archive, err := zip.NewReader(readerAt, size)
	if err != nil {
		return nil, err
	}
	if len(archive.File) > modrinth.MaxFiles {
		return nil, errors.New("modpack archive has too many entries")
	}
	pack := &Pack{target: target, Warnings: []string{}, overrides: map[string]override{}}
	seen := map[string]bool{}
	generic := map[string]*zip.File{}
	server := map[string]*zip.File{}
	var total uint64
	var indexFile *zip.File
	for _, entry := range archive.File {
		name := strings.TrimSuffix(entry.Name, "/")
		key := strings.ToLower(name)
		if !modrinth.SafePath(name) || seen[key] || entry.Mode()&os.ModeSymlink != 0 || entry.Mode()&os.ModeType != 0 && !entry.Mode().IsDir() || entry.Flags&1 != 0 {
			return nil, fmt.Errorf("unsafe, encrypted, or duplicate ZIP entry: %s", entry.Name)
		}
		seen[key] = true
		if entry.UncompressedSize64 > uint64(modrinth.MaxFileSize) || entry.UncompressedSize64 > 1<<20 && (entry.CompressedSize64 == 0 || entry.UncompressedSize64/entry.CompressedSize64 > 200) {
			return nil, errors.New("modpack entry exceeds size/compression limits")
		}
		total += entry.UncompressedSize64
		if total > uint64(modrinth.MaxTotalSize) {
			return nil, errors.New("modpack archive exceeds the expanded size limit")
		}
		if entry.Mode().IsDir() {
			continue
		}
		switch {
		case name == "modrinth.index.json":
			indexFile = entry
		case strings.HasPrefix(name, "client-overrides/"):
			pack.Warnings = append(pack.Warnings, "Skipped client-only override: "+name)
		case strings.HasPrefix(name, "server-overrides/config/"):
			if entry.UncompressedSize64 > uint64(modrinth.MaxConfigSize) {
				return nil, errors.New("config override exceeds the size limit")
			}
			server[strings.TrimPrefix(name, "server-overrides/")] = entry
		case strings.HasPrefix(name, "overrides/config/"):
			if entry.UncompressedSize64 > uint64(modrinth.MaxConfigSize) {
				return nil, errors.New("config override exceeds the size limit")
			}
			generic[strings.TrimPrefix(name, "overrides/")] = entry
		default:
			return nil, fmt.Errorf("unsupported override location; only config files are accepted: %s", name)
		}
	}
	if indexFile == nil {
		return nil, errors.New("modpack is missing modrinth.index.json")
	}
	data, err := readZip(indexFile, 4<<20)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &pack.Index); err != nil {
		return nil, fmt.Errorf("invalid modpack index: %w", err)
	}
	if err := validateIndex(pack.Index, target); err != nil {
		return nil, err
	}
	for _, f := range pack.Index.Files {
		if f.Env != nil && f.Env.Server == "unsupported" {
			pack.Warnings = append(pack.Warnings, "Skipped client-only file: "+f.Path)
		}
	}
	for name, entry := range server {
		for genericName := range generic {
			if genericName != name && strings.EqualFold(genericName, name) {
				return nil, errors.New("case-colliding generic/server config override paths")
			}
		}
		generic[name] = entry
	}
	keys := sortedKeys(generic)
	filePaths := map[string]bool{}
	for _, name := range keys {
		filePaths[strings.ToLower(name)] = true
	}
	for _, name := range keys {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if filePaths[strings.ToLower(parent)] {
				return nil, errors.New("config override path is both a directory and a file")
			}
		}
		data, err := readZip(generic[name], modrinth.MaxConfigSize)
		if err != nil {
			return nil, err
		}
		pack.overrides[name] = override{file: generic[name], hash: hashBytes(data)}
	}
	return pack, nil
}

func validateIndex(index Index, target modrinth.Target) error {
	if index.FormatVersion != 1 || index.Game != "minecraft" || !label(index.Name, 256) || !label(index.VersionID, 128) {
		return errors.New("unsupported or invalid Modrinth pack index")
	}
	if target.LoaderVersion == "" || index.Dependencies["minecraft"] != target.MinecraftVersion || index.Dependencies["fabric-loader"] != target.LoaderVersion {
		return errors.New("pack requires exact matching Minecraft and Fabric loader versions; migrate runtime separately first")
	}
	for dependency, version := range index.Dependencies {
		if (dependency != "minecraft" && dependency != "fabric-loader") || !label(version, 128) {
			return errors.New("unsupported pack runtime dependency")
		}
	}
	if len(index.Files) > modrinth.MaxFiles {
		return errors.New("modpack index has too many files")
	}
	names := map[string]bool{}
	var total int64
	for _, file := range index.Files {
		name := strings.TrimPrefix(file.Path, "mods/")
		if !modrinth.SafePath(file.Path) || !strings.HasPrefix(file.Path, "mods/") || strings.Contains(name, "/") || !strings.HasSuffix(strings.ToLower(name), ".jar") || names[strings.ToLower(file.Path)] {
			return errors.New("pack files must be distinct JARs directly under mods/")
		}
		names[strings.ToLower(file.Path)] = true
		if !modrinth.ValidHash(file.Hashes["sha512"], 128) || !modrinth.ValidHash(file.Hashes["sha1"], 40) || file.FileSize <= 0 || file.FileSize > modrinth.MaxFileSize {
			return errors.New("pack file has invalid size or SHA-512/SHA-1 hashes")
		}
		if len(file.Downloads) == 0 || len(file.Downloads) > 8 {
			return errors.New("pack file needs a bounded list of official downloads")
		}
		for _, download := range file.Downloads {
			if err := modrinth.ValidateDownload(download); err != nil {
				return err
			}
		}
		if file.Env != nil {
			for _, env := range []string{file.Env.Client, file.Env.Server} {
				if env != "required" && env != "optional" && env != "unsupported" {
					return errors.New("pack file has an unknown environment requirement")
				}
			}
		}
		total += file.FileSize
	}
	if total > modrinth.MaxTotalSize {
		return errors.New("pack file downloads exceed the total size limit")
	}
	return nil
}

// Reconstruct binds every server file to official exact project/version/file metadata.
// Unknown projects, client-only versions, and missing required dependency locks block the pack.
func Reconstruct(ctx context.Context, client *modrinth.Client, index Index, target modrinth.Target) ([]modrinth.LockedFile, []string, error) {
	if err := target.Validate(); err != nil {
		return nil, nil, err
	}
	if err := validateIndex(index, target); err != nil {
		return nil, nil, err
	}
	var hashes []string
	for _, f := range index.Files {
		if f.Env == nil || f.Env.Server != "unsupported" {
			hashes = append(hashes, f.Hashes["sha512"])
		}
	}
	if len(hashes) > modrinth.MaxProjects {
		return nil, nil, errors.New("pack exceeds the supported server mod project limit")
	}
	versions, err := client.VersionsFromHashes(ctx, hashes)
	if err != nil {
		return nil, nil, err
	}
	files := []modrinth.LockedFile{}
	blockers := []string{}
	byProject := map[string]modrinth.Version{}
	for _, file := range index.Files {
		if file.Env != nil && file.Env.Server == "unsupported" {
			continue
		}
		v, found := versions[file.Hashes["sha512"]]
		if !found {
			blockers = append(blockers, "Unknown Modrinth file cannot be reconstructed: "+file.Path)
			continue
		}
		p, err := client.Project(ctx, v.ProjectID)
		if err != nil {
			return nil, nil, err
		}
		// Import still defaults to stable-only; explicit prerelease import needs a separate policy/preview.
		if err := modrinth.Compatible(p, v, target, false); err != nil {
			blockers = append(blockers, err.Error())
			continue
		}
		primary, err := modrinth.PrimaryFile(v)
		if err != nil {
			blockers = append(blockers, err.Error())
			continue
		}
		if primary.Hashes["sha512"] != file.Hashes["sha512"] || primary.Hashes["sha1"] != file.Hashes["sha1"] || primary.Size != file.FileSize || primary.Filename != strings.TrimPrefix(file.Path, "mods/") || !slices.Contains(file.Downloads, primary.URL) {
			blockers = append(blockers, "Pack file differs from official primary file metadata: "+file.Path)
			continue
		}
		if _, exists := byProject[v.ProjectID]; exists {
			blockers = append(blockers, "Pack contains duplicate project versions: "+v.ProjectID)
			continue
		}
		byProject[v.ProjectID] = v
		files = append(files, modrinth.LockedFile{Filename: primary.Filename, SHA512: primary.Hashes["sha512"], SHA1: primary.Hashes["sha1"], Size: primary.Size, ProjectID: v.ProjectID, VersionID: v.ID, URL: primary.URL, Environment: v.Environment})
	}
	for _, id := range sortedKeys(byProject) {
		for _, dep := range byProject[id].Dependencies {
			if dep.Type != "required" {
				continue
			}
			project := dep.ProjectID
			if dep.VersionID != "" {
				v, err := client.Version(ctx, dep.VersionID)
				if err != nil {
					return nil, nil, err
				}
				if project != "" && project != v.ProjectID {
					blockers = append(blockers, "Dependency project/version mismatch in "+id)
					continue
				}
				project = v.ProjectID
			}
			v, ok := byProject[project]
			if !ok || dep.VersionID != "" && dep.VersionID != v.ID {
				blockers = append(blockers, "Pack omits or conflicts with required exact dependency for "+id+": "+project+" / "+dep.VersionID)
			}
		}
	}
	slices.SortFunc(files, func(a, b modrinth.LockedFile) int { return strings.Compare(a.ProjectID, b.ProjectID) })
	return files, blockers, nil
}

func (p *Pack) Plan(ctx context.Context, service *modrinth.Service, root *os.Root) (*modrinth.Plan, error) {
	files, blockers, err := Reconstruct(ctx, service.Client, p.Index, p.target)
	if err != nil {
		return nil, err
	}
	versions := map[string]string{}
	for _, f := range files {
		versions[f.ProjectID] = f.VersionID
	}
	plan, err := service.Plan(ctx, root, modrinth.Request{Target: p.target, Versions: versions})
	if err != nil {
		return nil, err
	}
	plan.Blockers = append(plan.Blockers, blockers...)
	plan.Rationale = append(plan.Rationale, p.Warnings...)
	plan.Pack = &modrinth.PackMetadata{Name: p.Index.Name, VersionID: p.Index.VersionID, Dependencies: p.Index.Dependencies}
	for _, name := range sortedKeys(p.overrides) {
		if err := safeParents(root, path.Dir(name)); err != nil {
			return nil, err
		}
		if info, err := root.Lstat(name); err == nil {
			if !info.Mode().IsRegular() {
				return nil, errors.New("local config is not a regular file")
			}
			plan.Rationale = append(plan.Rationale, "Preserved existing local config: "+name)
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		override := p.overrides[name]
		plan.Config = append(plan.Config, modrinth.ConfigFile{Path: name, SHA512: override.hash, Size: int64(override.file.UncompressedSize64)})
	}
	plan.Seal()
	return plan, nil
}

func (p *Pack) Stage(ctx context.Context, service *modrinth.Service, stage *os.Root, plan *modrinth.Plan) error {
	if plan == nil || len(plan.Blockers) > 0 {
		return errors.New("pack plan has unresolved blockers")
	}
	if err := plan.Validate(); err != nil {
		return err
	}
	for _, file := range plan.Config {
		override, found := p.overrides[file.Path]
		if !found || override.hash != file.SHA512 || int64(override.file.UncompressedSize64) != file.Size {
			return errors.New("pack overrides differ from the confirmed plan")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		name := "overrides/" + file.Path
		if err := makeDirs(stage, path.Dir(name)); err != nil {
			return err
		}
		if _, err := stage.Lstat(name); err == nil {
			data, err := readRootFile(stage, name, modrinth.MaxConfigSize)
			if err != nil {
				return err
			}
			if hashBytes(data) != file.SHA512 {
				return errors.New("staged override differs from the confirmed hash")
			}
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		data, err := readZip(override.file, modrinth.MaxConfigSize)
		if err != nil {
			return err
		}
		if hashBytes(data) != file.SHA512 {
			return errors.New("pack changed since preview")
		}
		f, err := stage.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, writeErr := f.Write(data)
		syncErr := f.Sync()
		closeErr := f.Close()
		if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
			_ = stage.Remove(name)
			return err
		}
	}
	return service.Stage(ctx, stage, plan)
}

func label(s string, max int) bool {
	return s != "" && len(s) <= max && !strings.ContainsAny(s, "\x00\r\n")
}
func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
func hashBytes(data []byte) string { h := sha512.Sum512(data); return hex.EncodeToString(h[:]) }
func readZip(file *zip.File, limit int64) ([]byte, error) {
	if file.UncompressedSize64 > uint64(limit) {
		return nil, errors.New("ZIP entry exceeds the size limit")
	}
	r, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit || uint64(len(data)) != file.UncompressedSize64 {
		return nil, errors.New("ZIP entry size mismatch")
	}
	return data, nil
}
func safeParents(root *os.Root, dir string) error {
	if dir == "." {
		return nil
	}
	if !modrinth.SafePath(dir) {
		return errors.New("unsafe config parent")
	}
	parts := strings.Split(dir, "/")
	for i := range parts {
		info, err := root.Lstat(strings.Join(parts[:i+1], "/"))
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("config parent must be a real directory")
		}
	}
	return nil
}
func makeDirs(root *os.Root, dir string) error {
	if !modrinth.SafePath(dir) {
		return errors.New("unsafe staging path")
	}
	parts := strings.Split(dir, "/")
	for i := range parts {
		name := strings.Join(parts[:i+1], "/")
		if err := root.Mkdir(name, 0750); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		info, err := root.Lstat(name)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("staging parent must be a real directory")
		}
	}
	return nil
}
func readRootFile(root *os.Root, name string, limit int64) ([]byte, error) {
	if !modrinth.SafePath(name) {
		return nil, errors.New("unsafe file path")
	}
	if err := safeParents(root, path.Dir(name)); err != nil {
		return nil, err
	}
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("file is not regular or exceeds the size limit")
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, opened) {
		return nil, errors.New("file changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit || int64(len(data)) != info.Size() {
		return nil, errors.New("file size changed while reading")
	}
	return data, nil
}

func openRealDir(root *os.Root, name string) (*os.Root, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("content directory must not be a link")
	}
	sub, err := root.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, err := sub.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		sub.Close()
		return nil, errors.New("content directory changed while opening")
	}
	return sub, nil
}
