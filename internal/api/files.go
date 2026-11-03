package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/gabrielpgava/newempire-server-control/internal/backups"
	"github.com/gabrielpgava/newempire-server-control/internal/fabric"
	"github.com/gabrielpgava/newempire-server-control/internal/files"
	"github.com/gabrielpgava/newempire-server-control/internal/importer"
	"github.com/gabrielpgava/newempire-server-control/internal/jobs"
	"github.com/gabrielpgava/newempire-server-control/internal/model"
)

func visiblePath(name string) error {
	if err := files.ValidatePath(name); err != nil {
		return err
	}
	for _, part := range strings.Split(name, "/") {
		lower := strings.ToLower(part)
		if strings.HasPrefix(lower, ".newempire-") || strings.HasPrefix(lower, ".nesc-") {
			return errors.New("internal transaction and lock files are protected")
		}
	}
	if strings.EqualFold(path.Base(name), "server.properties") {
		return errors.New("use Settings to edit server.properties without exposing RCON credentials")
	}
	return nil
}
func requestedPath(r *http.Request) string {
	name := r.URL.Query().Get("path")
	if name == "" {
		return "."
	}
	return name
}
func etag(content string) string {
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:])
}

func (a *App) listFiles(w http.ResponseWriter, r *http.Request) {
	name := requestedPath(r)
	if err := visiblePath(name); err != nil {
		fail(w, 400, err)
		return
	}
	root, err := a.Instances.OpenRoot(instance(r).Root)
	if err != nil {
		fail(w, 400, err)
		return
	}
	defer root.Close()
	var entries []files.Entry
	if query := r.URL.Query().Get("q"); query != "" {
		entries, err = files.Search(r.Context(), root, name, query, 200)
	} else {
		entries, err = files.List(r.Context(), root, name)
	}
	if err != nil {
		fail(w, 400, err)
		return
	}
	out := []map[string]any{}
	for _, e := range entries {
		if visiblePath(e.Path) != nil {
			continue
		}
		out = append(out, map[string]any{"name": e.Name, "path": e.Path, "is_dir": e.Directory, "size": e.Size, "modified_at": time.Unix(e.ModifiedAt, 0).UTC().Format(time.RFC3339)})
	}
	respond(w, 200, out)
}
func (a *App) downloadFile(w http.ResponseWriter, r *http.Request) {
	name := requestedPath(r)
	if err := visiblePath(name); err != nil {
		fail(w, 400, err)
		return
	}
	root, err := a.Instances.OpenRoot(instance(r).Root)
	if err != nil {
		fail(w, 400, err)
		return
	}
	defer root.Close()
	if r.URL.Query().Get("preview") == "true" {
		content, err := files.ReadText(r.Context(), root, name, 1<<20)
		if err != nil {
			fail(w, 400, err)
			return
		}
		respond(w, 200, map[string]string{"path": name, "content": content, "etag": etag(content)})
		return
	}
	f, err := files.Open(root, name)
	if err != nil {
		fail(w, 404, errors.New("regular file not found or unsafe file type"))
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		fail(w, 500, err)
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", path.Base(name)))
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, path.Base(name), info.ModTime(), f)
}
func (a *App) exportFiles(w http.ResponseWriter, r *http.Request) {
	name := requestedPath(r)
	if err := visiblePath(name); err != nil {
		fail(w, 400, err)
		return
	}
	root, err := a.Instances.OpenRoot(instance(r).Root)
	if err != nil {
		fail(w, 400, err)
		return
	}
	defer root.Close()
	if err := files.Walk(r.Context(), root, name, func(p string, _ fs.FileInfo) error { return visiblePath(p) }); err != nil {
		fail(w, 400, errors.New("this export contains protected configuration; use a privileged full backup instead"))
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", "attachment; filename=\"export.zip\"")
	if err := files.ExportZIP(r.Context(), root, name, w, nil); err != nil {
		panic(http.ErrAbortHandler)
	}
}

type fileMutation struct {
	Operation   string `json:"operation"`
	Path        string `json:"path"`
	Destination string `json:"destination"`
	Content     string `json:"content"`
	ETag        string `json:"etag"`
	Token       string `json:"token"`
	Confirm     bool   `json:"confirm"`
}
type recoveryEntry struct {
	Token     string `json:"token"`
	Path      string `json:"path"`
	CreatedAt int64  `json:"created_at"`
}

func (a *App) mutateFiles(w http.ResponseWriter, r *http.Request) {
	var body fileMutation
	if err := decode(r, &body); err != nil {
		fail(w, 400, err)
		return
	}
	if body.Operation != "mkdir" && body.Operation != "write" && body.Operation != "move" && body.Operation != "trash" && body.Operation != "recover" {
		fail(w, 400, errors.New("unsupported file operation"))
		return
	}
	if body.Operation != "recover" {
		if err := visiblePath(body.Path); err != nil {
			fail(w, 400, err)
			return
		}
	} else if !model.ValidID(body.Token) {
		fail(w, 400, errors.New("invalid recovery token"))
		return
	}
	if body.Operation == "move" {
		if err := visiblePath(body.Destination); err != nil {
			fail(w, 400, err)
			return
		}
	}
	if body.Operation != "mkdir" && !body.Confirm {
		fail(w, 400, errors.New("explicit confirmation is required"))
		return
	}
	a.enqueue(w, r, instance(r).ID, "file", body)
}
func (a *App) listRecovery(w http.ResponseWriter, r *http.Request) {
	rows, err := a.DB.QueryContext(r.Context(), "SELECT value FROM settings WHERE key LIKE ? ORDER BY key", "recovery:"+instance(r).ID+":%")
	if err != nil {
		fail(w, 500, err)
		return
	}
	defer rows.Close()
	all := []recoveryEntry{}
	for rows.Next() {
		var raw string
		var entry recoveryEntry
		if err := rows.Scan(&raw); err != nil {
			fail(w, 500, err)
			return
		}
		if err := json.Unmarshal([]byte(raw), &entry); err != nil {
			fail(w, 500, err)
			return
		}
		all = append(all, entry)
	}
	respond(w, 200, all)
}
func (a *App) recoveryRoot(i model.Instance) (*os.Root, error) {
	parent, _, err := a.Instances.OpenParent(i.Root)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	name := ".nesc-recovery-" + i.ID
	if err := parent.Mkdir(name, 0700); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	return parent.OpenRoot(name)
}
func (a *App) assertStopped(ctx context.Context, i model.Instance) error {
	status, err := a.runtimeStatus(ctx, i)
	if err != nil {
		return fmt.Errorf("cannot prove the runtime is stopped: %w", err)
	}
	if status.PID > 0 || status.State != "offline" && status.State != "crashed" {
		return errors.New("stop Minecraft before changing instance files")
	}
	return nil
}
func (a *App) registerFileJobs() {
	a.Jobs.Register("file", 0, func(ctx context.Context, t *jobs.Task, raw json.RawMessage) (any, error) {
		var body fileMutation
		if err := json.Unmarshal(raw, &body); err != nil {
			return nil, err
		}
		i, err := a.Instances.Get(ctx, t.Job.InstanceID)
		if err != nil {
			return nil, err
		}
		if err := a.checkRestorePending(ctx, i.ID); err != nil {
			return nil, err
		}
		if err := a.assertStopped(ctx, i); err != nil {
			return nil, err
		}
		root, err := a.Instances.OpenRoot(i.Root)
		if err != nil {
			return nil, err
		}
		defer root.Close()
		if err := contentSettled(root); err != nil {
			return nil, err
		}
		if err := t.Progress(ctx, 5, "Checking paths and creating a verified restore point"); err != nil {
			return nil, err
		}
		if body.Operation != "mkdir" {
			if _, err := a.safetyBackup(ctx, t, i, "Before file "+body.Operation); err != nil {
				return nil, err
			}
		}
		if body.Operation == "write" {
			if current, err := files.ReadText(ctx, root, body.Path, 1<<20); err == nil {
				if body.ETag == "" || etag(current) != body.ETag {
					return nil, errors.New("file changed since the preview; reopen it before saving")
				}
			} else if !errors.Is(err, fs.ErrNotExist) {
				return nil, err
			}
			if strings.HasSuffix(strings.ToLower(body.Path), ".json") && !json.Valid([]byte(body.Content)) {
				return nil, errors.New("configuration is not valid JSON")
			}
			if strings.HasSuffix(strings.ToLower(body.Path), ".properties") {
				if _, err := fabric.ReadProperties([]byte(body.Content)); err != nil {
					return nil, err
				}
			}
		}
		if err := t.CommitBarrier(ctx); err != nil {
			return nil, err
		}
		switch body.Operation {
		case "mkdir":
			err = files.Mkdir(ctx, root, body.Path)
		case "write":
			err = files.WriteText(ctx, root, body.Path, body.Content, 1<<20)
		case "move":
			err = files.Move(ctx, root, body.Path, body.Destination)
		case "trash":
			recovery, openErr := a.recoveryRoot(i)
			if openErr != nil {
				return nil, openErr
			}
			defer recovery.Close()
			token, trashErr := files.Trash(ctx, root, recovery, body.Path)
			if token != "" {
				entry := recoveryEntry{Token: token, Path: body.Path, CreatedAt: time.Now().Unix()}
				saveErr := a.saveSetting(ctx, "recovery:"+i.ID+":"+token, entry)
				return entry, errors.Join(trashErr, saveErr)
			}
			err = trashErr
		case "recover":
			var entry recoveryEntry
			if err := a.setting(ctx, "recovery:"+i.ID+":"+body.Token, &entry); err != nil {
				return nil, err
			}
			if entry.Token == "" {
				return nil, errors.New("recovery record not found")
			}
			recovery, openErr := a.recoveryRoot(i)
			if openErr != nil {
				return nil, openErr
			}
			defer recovery.Close()
			err = files.Recover(ctx, root, recovery, body.Token, entry.Path)
			if err == nil {
				_, err = a.DB.ExecContext(ctx, "DELETE FROM settings WHERE key=?", "recovery:"+i.ID+":"+body.Token)
			}
		}
		return map[string]bool{"ok": err == nil}, err
	})
}

type importRequest struct {
	UploadID    string `json:"upload_id"`
	PreviewHash string `json:"preview_hash"`
	Confirm     bool   `json:"confirm"`
	Owner       string `json:"owner,omitempty"`
}
type importPreview struct {
	PreviewHash      string           `json:"preview_hash"`
	Files            []importer.Entry `json:"files"`
	FileCount        int              `json:"file_count"`
	TotalBytes       int64            `json:"total_bytes"`
	Warnings         []string         `json:"warnings"`
	Launchers        []string         `json:"launchers"`
	Worlds           []string         `json:"worlds"`
	Conflicts        []string         `json:"conflicts,omitempty"`
	ServerRoot       string           `json:"server_root,omitempty"`
	MinecraftVersion string           `json:"minecraft_version,omitempty"`
	LoaderVersion    string           `json:"loader_version,omitempty"`
	JavaRequired     int              `json:"java_required,omitempty"`
	ReservationID    string           `json:"reservation_id,omitempty"`
	ServiceAccount   string           `json:"service_account,omitempty"`
}

func describePreview(p importer.Preview) importPreview {
	out := importPreview{PreviewHash: p.Hash, Files: p.Entries, FileCount: p.Files, TotalBytes: p.Bytes, Warnings: []string{}, Launchers: []string{}, Worlds: []string{}}
	for _, e := range p.Entries {
		if !e.Directory && strings.HasSuffix(strings.ToLower(e.Path), ".jar") && !strings.HasPrefix(e.Path, "mods/") && !strings.HasPrefix(e.Path, "libraries/") {
			out.Launchers = append(out.Launchers, e.Path)
		}
		if strings.HasSuffix(e.Path, "/level.dat") {
			out.Worlds = append(out.Worlds, path.Dir(e.Path))
		}
	}
	return out
}
func archiveFormat(filename string) importer.Format {
	lower := strings.ToLower(filename)
	switch {
	case strings.HasSuffix(lower, ".zip"), strings.HasSuffix(lower, ".mrpack"):
		return importer.ZIP
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		return importer.TARGZ
	case strings.HasSuffix(lower, ".tar.zst"):
		return importer.TARZSTD
	}
	return ""
}
func (a *App) inspectUpload(ctx context.Context, owner, id string) (importer.Preview, error) {
	u, err := a.Uploads.Lookup(ctx, owner, id)
	if err != nil {
		return importer.Preview{}, err
	}
	defer u.File.Close()
	if format := archiveFormat(u.Filename); format != "" {
		return importer.Inspect(ctx, u.File, format, importer.Limits{MaxArchiveBytes: a.Config.MaxUploadBytes}, nil)
	}
	if err := files.ValidatePath(u.Filename); err != nil || path.Base(u.Filename) != u.Filename {
		return importer.Preview{}, errors.New("upload filename is not a safe relative file")
	}
	h := sha256.New()
	if _, err := files.Copy(ctx, h, u.File, u.Size, nil); err != nil {
		return importer.Preview{}, err
	}
	entry := importer.Entry{Path: u.Filename, Size: u.Size, SHA256: hex.EncodeToString(h.Sum(nil))}
	raw, _ := json.Marshal(entry)
	sum := sha256.Sum256(raw)
	return importer.Preview{Hash: hex.EncodeToString(sum[:]), Bytes: u.Size, Files: 1, Entries: []importer.Entry{entry}}, nil
}
func (a *App) previewUpload(w http.ResponseWriter, r *http.Request) {
	var body importRequest
	if err := decode(r, &body); err != nil {
		fail(w, 400, err)
		return
	}
	owner := session(r).User.ID
	p, err := a.inspectUpload(r.Context(), owner, body.UploadID)
	if err != nil {
		fail(w, 400, err)
		return
	}
	parent, err := os.OpenRoot(filepath.Join(a.Config.DataDir, "staging"))
	if err != nil {
		fail(w, 400, err)
		return
	}
	defer parent.Close()
	name := "source-preview-" + model.NewID()
	if err := parent.Mkdir(name, 0700); err != nil {
		fail(w, 400, err)
		return
	}
	defer parent.RemoveAll(name)
	root, err := parent.OpenRoot(name)
	if err != nil {
		fail(w, 400, err)
		return
	}
	defer root.Close()
	if err := ensureSpace(root, p.Bytes); err != nil {
		fail(w, 400, err)
		return
	}
	extracted, err := a.extractUpload(r.Context(), owner, body.UploadID, root)
	if err != nil || extracted.Hash != p.Hash {
		fail(w, 400, errors.New("upload changed or could not be safely extracted; inspect it again"))
		return
	}
	preview, err := a.instanceSourcePreview(r.Context(), root, p.Entries, p.Hash)
	if err != nil {
		fail(w, 400, err)
		return
	}
	respond(w, 200, preview)
}

// Initial instance imports choose a server subtree; ordinary file imports keep archive-relative paths.
func (a *App) instanceSourcePreview(ctx context.Context, root *os.Root, entries []importer.Entry, sourceHash string) (importPreview, error) {
	var selected, serverRoot string
	var source fabric.SourceRuntime
	names := make(map[string]bool, len(entries))
	for _, e := range entries {
		if !e.Directory {
			names[e.Path] = true
		}
	}
	candidates := 0
	for _, e := range entries {
		if e.Directory || !strings.HasSuffix(e.Path, ".jar") || !names[path.Join(path.Dir(e.Path), "server.properties")] {
			continue
		}
		content := false
		for _, part := range strings.Split(e.Path, "/") {
			if part == "mods" || part == "libraries" || part == ".fabric" {
				content = true
			}
		}
		if content {
			continue
		}
		candidates++
		if candidates > 32 {
			return importPreview{}, errors.New("too many source launcher candidates; supply one server directory")
		}
		f, err := files.OpenRegular(root, e.Path)
		if err != nil {
			return importPreview{}, err
		}
		inspected, inspectErr := fabric.InspectSourceLauncher(ctx, f)
		closeErr := f.Close()
		if err := ctx.Err(); err != nil {
			return importPreview{}, err
		}
		if closeErr != nil {
			return importPreview{}, closeErr
		}
		if inspectErr != nil {
			continue
		}
		if selected != "" {
			return importPreview{}, errors.New("source has multiple supported Fabric launchers or server roots; supply exactly one server directory")
		}
		selected, serverRoot, source = e.Path, path.Dir(e.Path), inspected
	}
	if selected == "" {
		return importPreview{}, errors.New("source must contain one supported unattended Fabric bootstrap beside server.properties; legacy, custom, and non-Fabric launchers cannot be verified")
	}
	selection, err := a.selection(ctx, source.MinecraftVersion, source.LoaderVersion, "")
	if err != nil {
		return importPreview{}, err
	}
	plan, err := a.Fabric.Plan(ctx, selection)
	if err != nil {
		return importPreview{}, err
	}
	properties, err := files.ReadText(ctx, root, path.Join(serverRoot, "server.properties"), 1<<20)
	if err != nil {
		return importPreview{}, err
	}
	values, err := fabric.ReadProperties([]byte(properties))
	if err != nil {
		return importPreview{}, err
	}
	if level, ok := values["level-name"]; ok {
		if err := fabric.ValidateProperty("level-name", level); err != nil {
			return importPreview{}, err
		}
	}
	preview := importer.Preview{Entries: []importer.Entry{}}
	for _, e := range entries {
		if serverRoot != "." {
			if !strings.HasPrefix(e.Path, serverRoot+"/") {
				continue
			}
			e.Path = strings.TrimPrefix(e.Path, serverRoot+"/")
		}
		preview.Entries = append(preview.Entries, e)
		if !e.Directory {
			preview.Files++
			preview.Bytes += e.Size
		}
	}
	out := describePreview(preview)
	out.ServerRoot, out.MinecraftVersion, out.LoaderVersion = serverRoot, source.MinecraftVersion, source.LoaderVersion
	out.JavaRequired = max(plan.JavaMajor, source.JavaMajor)
	out.Launchers = []string{path.Base(selected)}
	out.PreviewHash = etagJSON(struct {
		Source, Root, Launcher, Game, Loader string
		Java                                 int
	}{sourceHash, serverRoot, out.Launchers[0], source.MinecraftVersion, source.LoaderVersion, out.JavaRequired})
	out.Warnings = append(out.Warnings, "Only the reported server root is installed. Launcher metadata is checked statically, not authenticated; import only code you trust. The Java minimum is not proof of mod compatibility.")
	return out, nil
}

func treeManifest(ctx context.Context, root *os.Root, id string) (backups.Manifest, error) {
	m := backups.Manifest{Version: 1, ID: id, CreatedAt: time.Now().UTC(), Entries: []importer.Entry{}}
	err := files.Walk(ctx, root, ".", func(name string, info fs.FileInfo) error {
		if name == "." {
			return nil
		}
		if len(m.Entries) >= 100000 {
			return files.ErrTooLarge
		}
		e := importer.Entry{Path: name, Directory: info.IsDir()}
		if !info.IsDir() {
			f, err := files.OpenRegular(root, name)
			if err != nil {
				return err
			}
			h := sha256.New()
			n, copyErr := files.Copy(ctx, h, f, 32<<30, nil)
			after, statErr := f.Stat()
			err = errors.Join(copyErr, statErr, f.Close())
			if err != nil {
				return err
			}
			if n != info.Size() || after.Size() != n || !after.ModTime().Equal(info.ModTime()) {
				return errors.New("source changed during inspection")
			}
			e.Size = n
			e.SHA256 = hex.EncodeToString(h.Sum(nil))
			m.Bytes += n
			if m.Bytes > 128<<30 {
				return files.ErrTooLarge
			}
		}
		m.Entries = append(m.Entries, e)
		return nil
	})
	return m, err
}
func manifestHash(m backups.Manifest) string {
	raw, _ := json.Marshal(m.Entries)
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}
func (a *App) previewAdoption(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path          string `json:"path"`
		ReservationID string `json:"reservation_id,omitempty"`
	}
	if err := decode(r, &body); err != nil {
		fail(w, 400, err)
		return
	}
	if !filepath.IsAbs(body.Path) || filepath.Clean(body.Path) != body.Path {
		fail(w, 400, errors.New("adoption requires the exact canonical absolute server directory"))
		return
	}
	root, err := a.Instances.OpenRoot(body.Path)
	if err != nil {
		fail(w, 400, err)
		return
	}
	defer root.Close()
	m, err := treeManifest(r.Context(), root, "")
	if err != nil {
		fail(w, 400, err)
		return
	}
	p, err := a.instanceSourcePreview(r.Context(), root, m.Entries, manifestHash(m))
	if err != nil {
		fail(w, 400, err)
		return
	}
	if p.ServerRoot != "." {
		fail(w, 400, errors.New("select the actual server directory for adoption, not a wrapper directory"))
		return
	}
	p.ReservationID, err = a.reserveAdoption(r.Context(), session(r).User.ID, body.Path, p.PreviewHash, body.ReservationID)
	if err != nil {
		fail(w, 400, err)
		return
	}
	p.ServiceAccount = "nec-mc-" + p.ReservationID[:24]
	p.Warnings = append(p.Warnings, "Adoption preserves files and ownership. The existing server must be stopped, EULA accepted, and RCON configured; service permissions are checked before registration.")
	respond(w, 200, p)
}
func (a *App) previewInstanceImport(w http.ResponseWriter, r *http.Request) {
	if err := a.assertStopped(r.Context(), instance(r)); err != nil {
		fail(w, 409, err)
		return
	}
	var body importRequest
	if err := decode(r, &body); err != nil {
		fail(w, 400, err)
		return
	}
	p, err := a.inspectUpload(r.Context(), session(r).User.ID, body.UploadID)
	if err != nil {
		fail(w, 400, err)
		return
	}
	root, err := a.Instances.OpenRoot(instance(r).Root)
	if err != nil {
		fail(w, 400, err)
		return
	}
	defer root.Close()
	m, err := treeManifest(r.Context(), root, "")
	if err != nil {
		fail(w, 400, err)
		return
	}
	out := describePreview(p)
	out.PreviewHash = etag(p.Hash + manifestHash(m))
	out.Conflicts = []string{}
	for _, e := range p.Entries {
		if visiblePath(e.Path) != nil {
			fail(w, 400, errors.New("file-manager imports cannot replace protected runtime configuration; use the new-instance import flow"))
			return
		}
		if _, err := root.Lstat(e.Path); err == nil {
			out.Conflicts = append(out.Conflicts, e.Path)
		} else if !errors.Is(err, fs.ErrNotExist) {
			fail(w, 400, err)
			return
		}
	}
	out.Warnings = append(out.Warnings, "Conflicting files are replaced only after a verified backup; original instance data is retained in a recovery directory.")
	respond(w, 200, out)
}
func (a *App) applyInstanceImport(w http.ResponseWriter, r *http.Request) {
	var body importRequest
	if err := decode(r, &body); err != nil {
		fail(w, 400, err)
		return
	}
	if !body.Confirm || body.PreviewHash == "" {
		fail(w, 400, errors.New("a preview hash and explicit confirmation are required"))
		return
	}
	body.Owner = session(r).User.ID
	a.enqueue(w, r, instance(r).ID, "import", body)
}

func copyTree(ctx context.Context, source, destination *os.Root, allowReplace bool) error {
	return files.Walk(ctx, source, ".", func(name string, info fs.FileInfo) error {
		if name == "." {
			return nil
		}
		if info.IsDir() {
			if err := destination.Mkdir(name, 0700); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
			other, err := files.Check(destination, name)
			if err != nil {
				return err
			}
			if !other.IsDir() {
				return errors.New("import directory collides with a file")
			}
			return nil
		}
		if _, err := destination.Lstat(name); err == nil && !allowReplace {
			return files.ErrExists
		} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := destination.MkdirAll(path.Dir(name), 0700); err != nil {
			return err
		}
		in, err := files.OpenRegular(source, name)
		if err != nil {
			return err
		}
		defer in.Close()
		tmp := path.Join(path.Dir(name), ".nesc-copy-"+model.NewID())
		out, err := destination.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		defer destination.Remove(tmp)
		n, copyErr := files.Copy(ctx, out, in, 32<<30, nil)
		syncErr := out.Sync()
		closeErr := out.Close()
		if err := errors.Join(copyErr, syncErr, closeErr); err != nil {
			return err
		}
		after, err := in.Stat()
		if err != nil {
			return err
		}
		if n != info.Size() || after.Size() != n || !after.ModTime().Equal(info.ModTime()) {
			return errors.New("source changed during staged copy")
		}
		if !allowReplace {
			return files.RenameNoReplace(destination, tmp, name)
		}
		if old, err := files.Check(destination, name); err == nil && !old.Mode().IsRegular() {
			return errors.New("import file collides with a directory")
		}
		return destination.Rename(tmp, name)
	})
}
func writeManifest(ctx context.Context, parent *os.Root, outer string, data *os.Root) error {
	m, err := treeManifest(ctx, data, model.NewID())
	if err != nil {
		return err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	f, err := parent.OpenFile(outer+"/manifest.json", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	return errors.Join(err, f.Sync(), f.Close())
}
func (a *App) extractUpload(ctx context.Context, owner, id string, stage *os.Root) (importer.Preview, error) {
	u, err := a.Uploads.Lookup(ctx, owner, id)
	if err != nil {
		return importer.Preview{}, err
	}
	defer u.File.Close()
	format := archiveFormat(u.Filename)
	if format != "" {
		return importer.Extract(ctx, u.File, format, stage, importer.Limits{MaxArchiveBytes: a.Config.MaxUploadBytes}, nil)
	}
	p, err := a.inspectUpload(ctx, owner, id)
	if err != nil {
		return p, err
	}
	f, err := stage.OpenFile(u.Filename, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return p, err
	}
	_, err = files.Copy(ctx, f, u.File, u.Size, nil)
	return p, errors.Join(err, f.Sync(), f.Close())
}
func (a *App) registerImportJobs() {
	a.Jobs.Register("import", 0, func(ctx context.Context, t *jobs.Task, raw json.RawMessage) (any, error) {
		var body importRequest
		if err := json.Unmarshal(raw, &body); err != nil {
			return nil, err
		}
		i, err := a.Instances.Get(ctx, t.Job.InstanceID)
		if err != nil {
			return nil, err
		}
		if err := a.assertStopped(ctx, i); err != nil {
			return nil, err
		}
		root, err := a.Instances.OpenRoot(i.Root)
		if err != nil {
			return nil, err
		}
		defer root.Close()
		if err := contentSettled(root); err != nil {
			return nil, err
		}
		p, err := a.inspectUpload(ctx, body.Owner, body.UploadID)
		if err != nil {
			return nil, err
		}
		m, err := treeManifest(ctx, root, "")
		if err != nil {
			return nil, err
		}
		if etag(p.Hash+manifestHash(m)) != body.PreviewHash {
			return nil, errors.New("import preview is stale; inspect it again")
		}
		for _, e := range p.Entries {
			if err := visiblePath(e.Path); err != nil {
				return nil, err
			}
		}
		if err := t.Progress(ctx, 5, "Creating and verifying the pre-import backup"); err != nil {
			return nil, err
		}
		backup, err := a.safetyBackup(ctx, t, i, "Before file import")
		if err != nil {
			return nil, err
		}
		parent, target, err := a.Instances.OpenParent(i.Root)
		if err != nil {
			return nil, err
		}
		defer parent.Close()
		storage, err := a.backupRoot(i.ID)
		if err != nil {
			return nil, err
		}
		defer storage.Close()
		stage, err := backups.StageRestore(ctx, storage, backup.ID, parent, nil)
		if err != nil {
			return nil, err
		}
		data, err := parent.OpenRoot(stage.Name)
		if err != nil {
			return nil, err
		}
		defer data.Close()
		incomingName := ".nesc-incoming-" + model.NewID()
		if err := parent.Mkdir(incomingName, 0700); err != nil {
			return nil, err
		}
		incoming, err := parent.OpenRoot(incomingName)
		if err != nil {
			return nil, err
		}
		defer incoming.Close()
		extracted, err := a.extractUpload(ctx, body.Owner, body.UploadID, incoming)
		if err != nil {
			return nil, err
		}
		if extracted.Hash != p.Hash {
			return nil, errors.New("upload changed after inspection")
		}
		if err := copyTree(ctx, incoming, data, true); err != nil {
			return nil, err
		}
		if err := writeManifest(ctx, parent, path.Dir(stage.Name), data); err != nil {
			return nil, err
		}
		if err := t.Progress(ctx, 85, "Staged import validated; committing with retained recovery data"); err != nil {
			return nil, err
		}
		if err := a.assertStopped(ctx, i); err != nil {
			return nil, err
		}
		if err := t.CommitBarrier(ctx); err != nil {
			return nil, err
		}
		result, err := backups.CommitRestore(ctx, parent, target, stage.Name)
		return result, err
	})
}
