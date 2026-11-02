package api

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gabrielpgava/newempire-server-control/internal/files"
	"github.com/gabrielpgava/newempire-server-control/internal/jobs"
	"github.com/gabrielpgava/newempire-server-control/internal/model"
	"github.com/gabrielpgava/newempire-server-control/internal/modpacks"
	"github.com/gabrielpgava/newempire-server-control/internal/modrinth"
	"github.com/gabrielpgava/newempire-server-control/internal/scheduler"
)

func contentTarget(i model.Instance) modrinth.Target {
	return modrinth.Target{MinecraftVersion: i.MinecraftVersion, Loader: "fabric", LoaderVersion: i.LoaderVersion}
}
func (a *App) searchMods(w http.ResponseWriter, r *http.Request) {
	game := r.URL.Query().Get("minecraft_version")
	if game == "" {
		fail(w, 400, errors.New("select an exact Minecraft version for compatible search"))
		return
	}
	result, err := a.Content.Client.Search(r.Context(), r.URL.Query().Get("q"), modrinth.Target{MinecraftVersion: game, Loader: "fabric"}, 0)
	if err != nil {
		fail(w, 502, err)
		return
	}
	respond(w, 200, result.Hits)
}
func (a *App) mods(w http.ResponseWriter, r *http.Request) {
	i := instance(r)
	root, err := a.Instances.OpenRoot(i.Root)
	if err != nil {
		fail(w, 400, err)
		return
	}
	defer root.Close()
	inventory, err := a.Content.Inventory(r.Context(), root)
	if err != nil {
		fail(w, 502, err)
		return
	}
	var cached struct {
		Versions map[string]string `json:"versions"`
	}
	a.setting(r.Context(), "mod-updates:"+i.ID, &cached)
	out := []map[string]any{}
	for _, f := range inventory.Files {
		name, version := f.Filename, f.VersionID
		out = append(out, map[string]any{"filename": f.Filename, "sha512": f.SHA512, "project_id": f.ProjectID, "name": name, "version": version, "version_id": f.VersionID, "managed": f.Managed, "pinned": inventory.Lock.Policy.Pins[f.ProjectID] != "", "update_available": cached.Versions[f.ProjectID] != "" && cached.Versions[f.ProjectID] != f.VersionID, "reason": f.Diagnostic})
	}
	respond(w, 200, out)
}

type modRequest struct {
	Projects []string `json:"projects,omitempty"`
	PlanHash string   `json:"plan_hash,omitempty"`
	Confirm  bool     `json:"confirm,omitempty"`
	UploadID string   `json:"upload_id,omitempty"`
	Owner    string   `json:"owner,omitempty"`
}

func planWire(p *modrinth.Plan) map[string]any {
	actions := []map[string]string{}
	for _, change := range p.Changes {
		filename := ""
		for _, file := range p.Files {
			if file.ProjectID == change.ProjectID {
				filename = file.Filename
				break
			}
		}
		actions = append(actions, map[string]string{"project_id": change.ProjectID, "name": change.ProjectID, "from_version": change.From, "to_version": change.To, "filename": filename, "reason": change.Rationale})
	}
	return map[string]any{"plan_hash": p.ID, "actions": actions, "blocked": p.Blockers, "warnings": p.Rationale}
}
func (a *App) planMods(w http.ResponseWriter, r *http.Request) {
	var body modRequest
	if err := decode(r, &body); err != nil {
		fail(w, 400, err)
		return
	}
	root, err := a.Instances.OpenRoot(instance(r).Root)
	if err != nil {
		fail(w, 400, err)
		return
	}
	defer root.Close()
	p, err := a.Content.Plan(r.Context(), root, modrinth.Request{Target: contentTarget(instance(r)), Projects: body.Projects})
	if err != nil {
		fail(w, 502, err)
		return
	}
	respond(w, 200, planWire(p))
}
func (a *App) applyMods(w http.ResponseWriter, r *http.Request) {
	var body modRequest
	if err := decode(r, &body); err != nil {
		fail(w, 400, err)
		return
	}
	if !body.Confirm || body.PlanHash == "" {
		fail(w, 400, errors.New("review the exact mod plan before confirming"))
		return
	}
	a.enqueue(w, r, instance(r).ID, "mod_apply", body)
}

type modManagement struct {
	Operation string `json:"operation"`
	ProjectID string `json:"project_id"`
	Confirm   bool   `json:"confirm"`
}

func (a *App) manageMods(w http.ResponseWriter, r *http.Request) {
	var body modManagement
	if err := decode(r, &body); err != nil {
		fail(w, 400, err)
		return
	}
	if !body.Confirm || !slices.Contains([]string{"pin", "unpin", "remove", "disable", "adopt"}, body.Operation) || len(body.ProjectID) < 1 || len(body.ProjectID) > 64 {
		fail(w, 400, errors.New("invalid confirmed mod operation"))
		return
	}
	a.enqueue(w, r, instance(r).ID, "mod_manage", body)
}
func (a *App) modPolicy(w http.ResponseWriter, r *http.Request) {
	p := defaultContentPolicy()
	if err := a.setting(r.Context(), "mods-policy:"+instance(r).ID, &p); err != nil {
		fail(w, 500, err)
		return
	}
	respond(w, 200, p)
}
func (a *App) saveModPolicy(w http.ResponseWriter, r *http.Request) {
	var p ContentPolicy
	if err := decode(r, &p); err != nil {
		fail(w, 400, err)
		return
	}
	if err := p.Validate(); err != nil {
		fail(w, 400, err)
		return
	}
	if err := a.saveSetting(r.Context(), "mods-policy:"+instance(r).ID, p); err != nil {
		fail(w, 500, err)
		return
	}
	a.audit(r.Context(), session(r).User.ID, "mods.policy", instance(r).ID, p.Mode)
	respond(w, 200, p)
}

func (a *App) previewModpack(w http.ResponseWriter, r *http.Request) {
	var body modRequest
	if err := decode(r, &body); err != nil {
		fail(w, 400, err)
		return
	}
	u, err := a.Uploads.Lookup(r.Context(), session(r).User.ID, body.UploadID)
	if err != nil {
		fail(w, 400, err)
		return
	}
	defer u.File.Close()
	if !strings.HasSuffix(strings.ToLower(u.Filename), ".mrpack") {
		fail(w, 400, errors.New("select a completed .mrpack upload"))
		return
	}
	i := instance(r)
	pack, err := modpacks.Read(u.File, u.Size, contentTarget(i))
	if err != nil {
		fail(w, 400, err)
		return
	}
	root, err := a.Instances.OpenRoot(i.Root)
	if err != nil {
		fail(w, 400, err)
		return
	}
	defer root.Close()
	p, err := pack.Plan(r.Context(), a.Content, root)
	if err != nil {
		fail(w, 502, err)
		return
	}
	respond(w, 200, planWire(p))
}
func (a *App) applyModpack(w http.ResponseWriter, r *http.Request) {
	var body modRequest
	if err := decode(r, &body); err != nil {
		fail(w, 400, err)
		return
	}
	if !body.Confirm || body.PlanHash == "" || body.UploadID == "" {
		fail(w, 400, errors.New("review the .mrpack plan before confirming"))
		return
	}
	body.Owner = session(r).User.ID
	a.enqueue(w, r, instance(r).ID, "modpack", body)
}
func (a *App) exportModpack(w http.ResponseWriter, r *http.Request) {
	i := instance(r)
	root, err := a.Instances.OpenRoot(i.Root)
	if err != nil {
		fail(w, 400, err)
		return
	}
	defer root.Close()
	f, err := os.CreateTemp(filepath.Join(a.Config.DataDir, "staging"), "pack-*.mrpack")
	if err != nil {
		fail(w, 500, err)
		return
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := modpacks.Export(r.Context(), root, f, modpacks.ExportOptions{Name: i.Name, VersionID: time.Now().UTC().Format("20060102T150405Z"), Target: contentTarget(i)}); err != nil {
		fail(w, 400, err)
		return
	}
	info, err := f.Stat()
	if err != nil {
		fail(w, 500, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-modrinth-modpack+zip")
	w.Header().Set("Content-Disposition", "attachment; filename=\"server.mrpack\"")
	http.ServeContent(w, r, "server.mrpack", info.ModTime(), f)
}
func (a *App) contentStage() (string, *os.Root, error) {
	parent, err := os.OpenRoot(filepath.Join(a.Config.DataDir, "staging"))
	if err != nil {
		return "", nil, err
	}
	defer parent.Close()
	name := "content-" + model.NewID()
	if err := parent.Mkdir(name, 0700); err != nil {
		return "", nil, err
	}
	root, err := parent.OpenRoot(name)
	return name, root, err
}
func contentTransactions(root *os.Root) ([]*modrinth.Transaction, error) {
	dir, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	all := []*modrinth.Transaction{}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".newempire-content-") && strings.HasSuffix(name, "-transaction.json") {
			id := strings.TrimSuffix(strings.TrimPrefix(name, ".newempire-content-"), "-transaction.json")
			tx, err := modrinth.LoadTransaction(root, id)
			if err != nil {
				return nil, err
			}
			all = append(all, tx)
		}
	}
	return all, nil
}

func contentSettled(root *os.Root) error {
	transactions, err := contentTransactions(root)
	if err != nil {
		return err
	}
	if len(transactions) != 0 {
		return errors.New("a prior content transaction needs readiness verification or explicit recovery before another file or configuration change")
	}
	return nil
}
func (a *App) pendingContent(ctx context.Context, i model.Instance, rollback bool) error {
	root, err := a.Instances.OpenRoot(i.Root)
	if err != nil {
		return err
	}
	defer root.Close()
	transactions, err := contentTransactions(root)
	if err != nil {
		return err
	}
	for _, tx := range transactions {
		if rollback || tx.State == "prepared" {
			if err := a.assertStopped(ctx, i); err != nil {
				return err
			}
			if err := modrinth.Rollback(ctx, root, tx); err != nil {
				return err
			}
		}
		if tx.State == "rolled_back" || !rollback && tx.State == "applied" {
			if err := modrinth.Finalize(root, tx); err != nil {
				return err
			}
		}
	}
	return nil
}
func (a *App) recoverPreparedContent(ctx context.Context, i model.Instance) error {
	root, err := a.Instances.OpenRoot(i.Root)
	if err != nil {
		return err
	}
	defer root.Close()
	transactions, err := contentTransactions(root)
	if err != nil {
		return err
	}
	for _, tx := range transactions {
		if tx.State == "prepared" {
			if err := a.assertStopped(ctx, i); err != nil {
				return err
			}
			if err := modrinth.Rollback(ctx, root, tx); err != nil {
				return err
			}
			if err := modrinth.Finalize(root, tx); err != nil {
				return err
			}
		} else if tx.State == "rolled_back" {
			if err := modrinth.Finalize(root, tx); err != nil {
				return err
			}
		}
	}
	return nil
}
func (a *App) applyContent(ctx context.Context, t *jobs.Task, i model.Instance, p *modrinth.Plan, stage *os.Root) (response any, resultErr error) {
	defer func() {
		kind := "content_update_succeeded"
		if resultErr != nil {
			kind = "content_update_failed"
		}
		a.notify(context.WithoutCancel(ctx), kind, i.ID, t.Job.ID)
	}()
	if len(p.Blockers) > 0 {
		return planWire(p), errors.New("content plan is blocked: " + strings.Join(p.Blockers, "; "))
	}
	root, err := a.Instances.OpenRoot(i.Root)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if err := contentSettled(root); err != nil {
		return nil, err
	}
	status, err := a.runtimeStatus(ctx, i)
	if err != nil {
		return nil, err
	}
	wasRunning := status.PID > 0 || status.Ready
	if wasRunning {
		if err := a.stopMinecraft(ctx, i); err != nil {
			return nil, err
		}
	}
	if err := a.assertStopped(ctx, i); err != nil {
		return nil, err
	}
	if _, err := a.safetyBackup(ctx, t, i, "Before compatible content transaction "+p.ID); err != nil {
		if wasRunning {
			a.Runtime.Start(context.WithoutCancel(ctx), i)
		}
		return nil, err
	}
	if err := t.Progress(ctx, 85, "Verified restore point complete; committing only content and approved configuration"); err != nil {
		return nil, err
	}
	if err := t.CommitBarrier(ctx); err != nil {
		return nil, err
	}
	tx, err := modrinth.Apply(ctx, root, stage, p)
	result := map[string]any{"plan": planWire(p), "transaction": tx, "world_data": "not rolled back"}
	if err != nil {
		return result, err
	}
	if !wasRunning {
		result["health"] = "not checked: Minecraft remains offline; the next start will verify or roll back pending content"
		return result, nil
	}
	healthErr := a.Runtime.Start(ctx, i)
	if healthErr == nil {
		_, healthErr = a.awaitReady(ctx, i.ID, 3*time.Minute)
	}
	if healthErr != nil {
		recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
		defer cancel()
		if err := a.stopMinecraft(recoveryCtx, i); err != nil {
			return result, errors.Join(healthErr, err)
		}
		if err := modrinth.Rollback(recoveryCtx, root, tx); err != nil {
			return result, errors.Join(healthErr, err)
		}
		if err := modrinth.Finalize(root, tx); err != nil {
			return result, errors.Join(healthErr, err)
		}
		restartErr := a.Runtime.Start(recoveryCtx, i)
		if restartErr == nil {
			_, restartErr = a.awaitReady(recoveryCtx, i.ID, 3*time.Minute)
		}
		return result, errors.Join(errors.New("new content failed readiness; prior content was restored, but world progress was not rolled back"), healthErr, restartErr)
	}
	if err := modrinth.Finalize(root, tx); err != nil {
		return result, err
	}
	result["health"] = "ready"
	return result, nil
}

func (a *App) registerContentJobs() {
	a.Jobs.Register("mod_apply", 0, func(ctx context.Context, t *jobs.Task, raw json.RawMessage) (any, error) {
		var body modRequest
		if err := json.Unmarshal(raw, &body); err != nil {
			return nil, err
		}
		i, err := a.Instances.Get(ctx, t.Job.InstanceID)
		if err != nil {
			return nil, err
		}
		root, err := a.Instances.OpenRoot(i.Root)
		if err != nil {
			return nil, err
		}
		defer root.Close()
		p, err := a.Content.Plan(ctx, root, modrinth.Request{Target: contentTarget(i), Projects: body.Projects})
		if err != nil {
			return nil, err
		}
		if p.ID != body.PlanHash {
			return nil, errors.New("mod plan changed; review it again")
		}
		if len(p.Blockers) > 0 {
			return planWire(p), errors.New("mod plan is blocked")
		}
		if err := t.Progress(ctx, 10, "Downloading compatible content into verified staging"); err != nil {
			return nil, err
		}
		_, stage, err := a.contentStage()
		if err != nil {
			return nil, err
		}
		defer stage.Close()
		if err := a.Content.Stage(ctx, stage, p); err != nil {
			return nil, err
		}
		return a.applyContent(ctx, t, i, p, stage)
	})
	a.Jobs.Register("modpack", 0, func(ctx context.Context, t *jobs.Task, raw json.RawMessage) (any, error) {
		var body modRequest
		if err := json.Unmarshal(raw, &body); err != nil {
			return nil, err
		}
		i, err := a.Instances.Get(ctx, t.Job.InstanceID)
		if err != nil {
			return nil, err
		}
		u, err := a.Uploads.Lookup(ctx, body.Owner, body.UploadID)
		if err != nil {
			return nil, err
		}
		defer u.File.Close()
		pack, err := modpacks.Read(u.File, u.Size, contentTarget(i))
		if err != nil {
			return nil, err
		}
		root, err := a.Instances.OpenRoot(i.Root)
		if err != nil {
			return nil, err
		}
		defer root.Close()
		p, err := pack.Plan(ctx, a.Content, root)
		if err != nil {
			return nil, err
		}
		if p.ID != body.PlanHash {
			return nil, errors.New("modpack plan changed; review it again")
		}
		if len(p.Blockers) > 0 {
			return planWire(p), errors.New("modpack plan is blocked")
		}
		_, stage, err := a.contentStage()
		if err != nil {
			return nil, err
		}
		defer stage.Close()
		if err := pack.Stage(ctx, a.Content, stage, p); err != nil {
			return nil, err
		}
		return a.applyContent(ctx, t, i, p, stage)
	})
	a.Jobs.Register("mod_check", 0, func(ctx context.Context, t *jobs.Task, _ json.RawMessage) (any, error) {
		i, err := a.Instances.Get(ctx, t.Job.InstanceID)
		if err != nil {
			return nil, err
		}
		root, err := a.Instances.OpenRoot(i.Root)
		if err != nil {
			return nil, err
		}
		defer root.Close()
		p, err := a.Content.Plan(ctx, root, modrinth.Request{Target: contentTarget(i)})
		if err != nil {
			return nil, err
		}
		versions := map[string]string{}
		for _, f := range p.Files {
			versions[f.ProjectID] = f.VersionID
		}
		if err := a.saveSetting(ctx, "mod-updates:"+i.ID, map[string]any{"versions": versions}); err != nil {
			return nil, err
		}
		if err := a.saveSetting(ctx, "mods-pending:"+i.ID, len(p.Changes) > 0 && len(p.Blockers) == 0); err != nil {
			return nil, err
		}
		if len(p.Changes) > 0 {
			var notified string
			if err := a.setting(ctx, "mods-notified-plan:"+i.ID, &notified); err != nil {
				return nil, err
			}
			if notified != p.ID {
				a.notify(ctx, "content_updates_available", i.ID, t.Job.ID)
				if err := a.saveSetting(ctx, "mods-notified-plan:"+i.ID, p.ID); err != nil {
					return nil, err
				}
			}
		}
		policy := defaultContentPolicy()
		if err := a.setting(ctx, "mods-policy:"+i.ID, &policy); err != nil {
			return nil, err
		}
		if policy.Mode != "automatic-safe" || len(p.Changes) == 0 || len(p.Blockers) > 0 {
			return planWire(p), nil
		}
		var failedPlan string
		if err := a.setting(ctx, "mods-failed-plan:"+i.ID, &failedPlan); err != nil {
			return nil, err
		}
		if failedPlan == p.ID {
			return planWire(p), errors.New("this exact plan previously failed automatic maintenance; administrator review is required")
		}
		inventory, err := a.Content.Inventory(ctx, root)
		if err != nil {
			return nil, err
		}
		for _, file := range inventory.Files {
			if !file.Managed {
				return map[string]any{"plan": planWire(p), "postponed": true, "reason": "Automatic-safe maintenance requires every installed mod to be explicitly managed; unidentified and external mods were preserved."}, nil
			}
		}
		inside, err := scheduler.InWindow(time.Now(), policy.Timezone, policy.WindowStart, policy.WindowEnd)
		if err != nil {
			return nil, err
		}
		status, err := a.runtimeStatus(ctx, i)
		if err != nil {
			return nil, err
		}
		if !inside || !status.Ready || policy.PostponePlayers && len(status.Players) > 0 {
			return map[string]any{"plan": planWire(p), "postponed": true, "reason": "Maintenance window, online readiness, or connected-player policy deferred the update"}, nil
		}
		_, stage, err := a.contentStage()
		if err != nil {
			return nil, err
		}
		defer stage.Close()
		if err := a.Content.Stage(ctx, stage, p); err != nil {
			return nil, err
		}
		status, err = a.runtimeStatus(ctx, i)
		if err != nil {
			return nil, err
		}
		inside, err = scheduler.InWindow(time.Now(), policy.Timezone, policy.WindowStart, policy.WindowEnd)
		if err != nil {
			return nil, err
		}
		if !inside || !status.Ready || policy.PostponePlayers && len(status.Players) > 0 {
			return planWire(p), errors.New("automatic maintenance postponed after staging because its window or player state changed")
		}
		result, applyErr := a.applyContent(ctx, t, i, p, stage)
		if applyErr != nil {
			return result, errors.Join(applyErr, a.saveSetting(ctx, "mods-failed-plan:"+i.ID, p.ID))
		}
		return result, a.saveSetting(ctx, "mods-pending:"+i.ID, false)
	})
	a.Jobs.Register("mod_manage", 0, a.manageContentJob)
}

func (a *App) manageContentJob(ctx context.Context, t *jobs.Task, raw json.RawMessage) (any, error) {
	var body modManagement
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	i, err := a.Instances.Get(ctx, t.Job.InstanceID)
	if err != nil {
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
	inventory, err := a.Content.Inventory(ctx, root)
	if err != nil {
		return nil, err
	}
	var selected *modrinth.LocalFile
	for n := range inventory.Files {
		if inventory.Files[n].ProjectID == body.ProjectID {
			if selected != nil {
				return nil, errors.New("multiple installed files identify this project; reconcile duplicate mods explicitly before managing it")
			}
			selected = &inventory.Files[n]
		}
	}
	if selected == nil {
		return nil, errors.New("project is not installed")
	}
	if !selected.Managed && body.Operation != "adopt" {
		return nil, errors.New("external content is preserved; explicitly adopt its exact hash before managing it")
	}
	if body.Operation == "adopt" {
		for _, file := range inventory.Lock.Files {
			if file.ProjectID == selected.ProjectID || strings.EqualFold(file.Filename, selected.Filename) {
				return nil, errors.New("this project or filename is already recorded in the content lock; reconcile its existing entry instead of adopting it twice")
			}
		}
	}
	if body.Operation == "pin" || body.Operation == "unpin" || body.Operation == "adopt" {
		if err := a.assertStopped(ctx, i); err != nil {
			return nil, err
		}
		if _, err := a.safetyBackup(ctx, t, i, "Before explicit content policy/ownership change"); err != nil {
			return nil, err
		}
		lock := inventory.Lock
		if body.Operation == "adopt" {
			v, err := a.Content.Client.Version(ctx, selected.VersionID)
			if err != nil {
				return nil, err
			}
			project, err := a.Content.Client.Project(ctx, selected.ProjectID)
			if err != nil {
				return nil, err
			}
			if err := modrinth.Compatible(project, v, contentTarget(i), false); err != nil {
				return nil, err
			}
			var found *modrinth.File
			for n := range v.Files {
				if v.Files[n].Hashes["sha512"] == selected.SHA512 {
					found = &v.Files[n]
					break
				}
			}
			if found == nil {
				return nil, errors.New("installed hash does not match the exact official version")
			}
			lock.Files = append(lock.Files, modrinth.LockedFile{Filename: selected.Filename, SHA512: selected.SHA512, SHA1: found.Hashes["sha1"], Size: selected.Size, ProjectID: selected.ProjectID, VersionID: selected.VersionID, URL: found.URL, Environment: v.Environment})
			lock.Target = contentTarget(i)
		} else if body.Operation == "pin" {
			lock.Policy.Pins[selected.ProjectID] = selected.VersionID
		} else {
			delete(lock.Policy.Pins, selected.ProjectID)
		}
		if err := t.CommitBarrier(ctx); err != nil {
			return nil, err
		}
		if body.Operation == "adopt" {
			b, _ := json.Marshal(lock)
			err = files.WriteText(ctx, root, modrinth.LockFilename, string(b), 4<<20)
		} else {
			err = modrinth.SavePolicy(root, lock.Policy)
		}
		return map[string]string{"operation": body.Operation, "project_id": selected.ProjectID}, err
	}
	exact := map[string]string{}
	for _, f := range inventory.Lock.Files {
		if f.ProjectID != selected.ProjectID {
			exact[f.ProjectID] = f.VersionID
		}
	}
	p, err := a.Content.Plan(ctx, root, modrinth.Request{Target: contentTarget(i), Remove: []string{selected.ProjectID}, Versions: exact})
	if err != nil {
		return nil, err
	}
	if len(p.Blockers) > 0 {
		return planWire(p), errors.New("removal is blocked by dependencies or pins")
	}
	_, stage, err := a.contentStage()
	if err != nil {
		return nil, err
	}
	defer stage.Close()
	if err := a.Content.Stage(ctx, stage, p); err != nil {
		return nil, err
	}
	if body.Operation == "disable" {
		if _, err := a.safetyBackup(ctx, t, i, "Before disabling a managed mod"); err != nil {
			return nil, err
		}
		if err := a.assertStopped(ctx, i); err != nil {
			return nil, err
		}
		if err := root.Mkdir("disabled-mods", 0770); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		source, err := files.OpenRegular(root, path.Join("mods", selected.Filename))
		if err != nil {
			return nil, err
		}
		defer source.Close()
		destination := path.Join("disabled-mods", selected.Filename+".disabled")
		f, err := root.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return nil, err
		}
		_, err = files.Copy(ctx, f, source, modrinth.MaxFileSize, nil)
		err = errors.Join(err, f.Sync(), f.Close())
		if err != nil {
			return nil, err
		}
	}
	return a.applyContent(ctx, t, i, p, stage)
}
