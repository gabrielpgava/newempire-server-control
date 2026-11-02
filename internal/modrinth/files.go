package modrinth

import (
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

	"github.com/gabrielpgava/newempire-server-control/internal/model"
)

type Policy struct {
	Mode            string            `json:"mode"`
	AllowPrerelease bool              `json:"allow_prerelease"`
	Pins            map[string]string `json:"pins"`
	Excludes        []string          `json:"excludes"`
}

func (p Policy) normalized() (Policy, error) {
	if len(p.Pins)+len(p.Excludes) > MaxProjects {
		return p, errors.New("content policy exceeds the project limit")
	}
	if p.Mode == "" {
		p.Mode = "manual"
	}
	if p.Mode != "manual" && p.Mode != "notify" && p.Mode != "automatic" {
		return p, errors.New("content policy mode must be manual, notify, or automatic")
	}
	pins := make(map[string]string, len(p.Pins))
	for project, version := range p.Pins {
		if !validID(project) || !validID(version) {
			return p, errors.New("pins must contain exact project and version IDs")
		}
		pins[project] = version
	}
	p.Pins = pins
	p.Excludes = slices.Clone(p.Excludes)
	for _, id := range p.Excludes {
		if !validID(id) {
			return p, errors.New("invalid excluded project ID")
		}
	}
	slices.Sort(p.Excludes)
	p.Excludes = slices.Compact(p.Excludes)
	return p, nil
}

type LockedFile struct {
	Filename    string `json:"filename"`
	SHA512      string `json:"sha512"`
	SHA1        string `json:"sha1,omitempty"`
	Size        int64  `json:"size"`
	ProjectID   string `json:"project_id"`
	VersionID   string `json:"version_id"`
	URL         string `json:"url"`
	Environment string `json:"environment,omitempty"`
}

func (f LockedFile) validate() error {
	if !validID(f.ProjectID) || !validID(f.VersionID) || f.SHA1 != "" && !ValidHash(f.SHA1, 40) {
		return errors.New("invalid locked project, version, or SHA-1")
	}
	return validateFile(File{Filename: f.Filename, Size: f.Size, URL: f.URL, Hashes: map[string]string{"sha512": f.SHA512}})
}

type PackMetadata struct {
	Name         string            `json:"name"`
	VersionID    string            `json:"version_id"`
	Dependencies map[string]string `json:"dependencies"`
}

type Lock struct {
	Schema int           `json:"schema"`
	Target Target        `json:"target"`
	Policy Policy        `json:"policy"`
	Files  []LockedFile  `json:"files"`
	Pack   *PackMetadata `json:"pack,omitempty"`
}

func LoadLock(root *os.Root) (Lock, error) {
	lock := Lock{Schema: 1, Policy: Policy{Mode: "manual", Pins: map[string]string{}, Excludes: []string{}}, Files: []LockedFile{}}
	data, err := readOptional(root, LockFilename, 4<<20)
	if err != nil {
		return lock, err
	}
	if data == nil {
		return lock, nil
	}
	if err := json.Unmarshal(data, &lock); err != nil {
		return lock, fmt.Errorf("invalid content lock: %w", err)
	}
	if lock.Schema != 1 {
		return lock, errors.New("unsupported content lock schema")
	}
	lock.Policy, err = lock.Policy.normalized()
	if err != nil {
		return lock, err
	}
	if err := validateLocked(lock.Files); err != nil {
		return lock, err
	}
	return lock, nil
}

// SavePolicy requires the same audited instance mutation job/lock as other content writes.
func SavePolicy(root *os.Root, policy Policy) error {
	policy, err := policy.normalized()
	if err != nil {
		return err
	}
	lock, err := LoadLock(root)
	if err != nil {
		return err
	}
	lock.Policy = policy
	data, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(root, LockFilename, data)
}

func validateLocked(files []LockedFile) error {
	if len(files) > MaxFiles {
		return errors.New("content lock has too many files")
	}
	names := map[string]bool{}
	projects := map[string]bool{}
	var total int64
	for _, f := range files {
		if err := f.validate(); err != nil {
			return err
		}
		name := strings.ToLower(f.Filename)
		if names[name] || projects[f.ProjectID] {
			return errors.New("content lock has duplicate filenames or projects")
		}
		names[name] = true
		projects[f.ProjectID] = true
		total += f.Size
	}
	if total > MaxTotalSize {
		return errors.New("content size exceeds the total limit")
	}
	return nil
}

type FileState struct {
	Path   string `json:"path"`
	SHA512 string `json:"sha512"`
	Size   int64  `json:"size"`
	Mode   uint32 `json:"mode"`
}

type LocalFile struct {
	Filename   string `json:"filename"`
	Name       string `json:"name"`
	Version    string `json:"version,omitempty"`
	SHA512     string `json:"sha512"`
	Size       int64  `json:"size"`
	ProjectID  string `json:"project_id,omitempty"`
	VersionID  string `json:"version_id,omitempty"`
	Managed    bool   `json:"managed"`
	Diagnostic string `json:"diagnostic,omitempty"`
}

type Inventory struct {
	Files   []LocalFile `json:"files"`
	Lock    Lock        `json:"lock"`
	before  []FileState
	hadMods bool
}

type Service struct{ Client *Client }

func NewService(client *Client) *Service {
	if client == nil {
		client = NewClient(nil)
	}
	return &Service{Client: client}
}

func (s *Service) Inventory(ctx context.Context, root *os.Root) (Inventory, error) {
	result := Inventory{Files: []LocalFile{}}
	lock, err := LoadLock(root)
	if err != nil {
		return result, err
	}
	result.Lock = lock
	states, hadMods, err := snapshot(ctx, root, "mods")
	if err != nil {
		return result, err
	}
	result.before = states
	result.hadMods = hadMods
	var hashes []string
	for _, f := range states {
		if jarPath(f.Path) {
			hashes = append(hashes, f.SHA512)
		}
	}
	versions, err := s.Client.VersionsFromHashes(ctx, hashes)
	if err != nil {
		return result, err
	}
	locked := map[string]LockedFile{}
	for _, f := range lock.Files {
		locked[f.Filename] = f
	}
	for _, f := range states {
		if !jarPath(f.Path) {
			continue
		}
		local := LocalFile{Filename: f.Path, Name: f.Path, SHA512: f.SHA512, Size: f.Size, Diagnostic: "Unknown JAR; preserved and unmanaged."}
		if version, ok := versions[f.SHA512]; ok {
			if version.Name != "" {
				local.Name = version.Name
			}
			local.Version = version.VersionNumber
			local.ProjectID = version.ProjectID
			local.VersionID = version.ID
			local.Diagnostic = "Recognized by hash, but externally managed; preserved."
			if entry, ok := locked[f.Path]; ok && entry.SHA512 == f.SHA512 && entry.Size == f.Size && entry.ProjectID == version.ProjectID && entry.VersionID == version.ID {
				local.Managed = true
				local.Diagnostic = ""
			}
		}
		result.Files = append(result.Files, local)
	}
	return result, nil
}

func snapshot(ctx context.Context, root *os.Root, dir string) ([]FileState, bool, error) {
	states := []FileState{}
	info, err := root.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return states, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, false, errors.New("content directory must be a real directory")
	}
	sub, err := openRealDir(root, dir)
	if err != nil {
		return nil, false, err
	}
	defer sub.Close()
	count := 0
	var total int64
	err = fs.WalkDir(sub.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		count++
		if count > MaxFiles || !SafePath(name) {
			return errors.New("content tree exceeds path/count limits")
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symbolic links are not allowed in managed content: %s", name)
		}
		if entry.IsDir() {
			return nil
		}
		state, err := fingerprint(ctx, sub, name)
		if err != nil {
			return err
		}
		total += state.Size
		if total > MaxTotalSize {
			return errors.New("content tree exceeds the size limit")
		}
		states = append(states, state)
		return nil
	})
	slices.SortFunc(states, func(a, b FileState) int { return strings.Compare(a.Path, b.Path) })
	return states, true, err
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func openRegular(root *os.Root, name string) (*os.File, fs.FileInfo, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("not a regular content file: %s", name)
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, nil, err
	}
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() {
		f.Close()
		return nil, nil, fmt.Errorf("content file changed while opening: %s", name)
	}
	return f, opened, nil
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

func fingerprint(ctx context.Context, root *os.Root, name string) (FileState, error) {
	state := FileState{Path: name}
	f, info, err := openRegular(root, name)
	if err != nil {
		return state, err
	}
	defer f.Close()
	if info.Size() > MaxFileSize {
		return state, errors.New("content file exceeds the size limit")
	}
	hash := sha512.New()
	n, err := io.Copy(hash, io.LimitReader(contextReader{ctx, f}, MaxFileSize+1))
	if err != nil {
		return state, err
	}
	if n != info.Size() || n > MaxFileSize {
		return state, errors.New("content file changed size while hashing")
	}
	state.SHA512 = hex.EncodeToString(hash.Sum(nil))
	state.Size = n
	state.Mode = uint32(info.Mode().Perm())
	return state, nil
}

func hashBytes(data []byte) string { sum := sha512.Sum512(data); return hex.EncodeToString(sum[:]) }

func readOptional(root *os.Root, name string, limit int64) ([]byte, error) {
	f, info, err := openRegular(root, name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if info.Size() > limit {
		return nil, errors.New("content metadata exceeds the size limit")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if int64(len(data)) > limit {
		return nil, errors.New("content metadata exceeds the size limit")
	}
	return data, err
}

func atomicWrite(root *os.Root, name string, data []byte) error {
	temp := path.Join(path.Dir(name), ".newempire-content-write-"+model.NewID())
	f, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(temp)
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	if err := root.Rename(temp, name); err != nil {
		return err
	}
	return syncDir(root, path.Dir(name))
}

func syncDir(root *os.Root, name string) error {
	f, err := root.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func ensureDirs(root *os.Root, dir string) error {
	if dir == "." {
		return nil
	}
	if !SafePath(dir) {
		return errors.New("unsafe content directory")
	}
	parts := strings.Split(dir, "/")
	for i := range parts {
		name := strings.Join(parts[:i+1], "/")
		err := root.Mkdir(name, 0750)
		if err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		if err == nil {
			if err := syncDir(root, path.Dir(name)); err != nil {
				return err
			}
		}
		info, err := root.Lstat(name)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("content parent must be a real directory")
		}
	}
	return nil
}
