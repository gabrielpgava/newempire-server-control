package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gabrielpgava/newempire-server-control/internal/backups"
	"github.com/gabrielpgava/newempire-server-control/internal/files"
	"github.com/gabrielpgava/newempire-server-control/internal/jobs"
	"github.com/gabrielpgava/newempire-server-control/internal/model"
	"github.com/go-chi/chi/v5"
)

func (a *App) backupRoot(id string) (*os.Root, error) {
	if !model.ValidID(id) {
		return nil, errors.New("invalid instance identifier")
	}
	parent, err := os.OpenRoot(a.Config.BackupDir)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	if err := parent.Mkdir(id, 0700); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	return parent.OpenRoot(id)
}
func (a *App) backupWire(ctx context.Context, id string, b backups.Backup) map[string]any {
	var note string
	a.setting(ctx, "backup-note:"+id+":"+b.ID, &note)
	var remote any
	a.setting(ctx, "backup-remote:"+id+":"+b.ID, &remote)
	return map[string]any{"id": b.ID, "instance_id": id, "created_at": b.CreatedAt.Unix(), "size": b.Bytes, "sha256": b.ArchiveSHA256, "note": note, "verified": true, "offsite": remote, "local": true}
}
func (a *App) listBackups(w http.ResponseWriter, r *http.Request) {
	root, err := a.backupRoot(instance(r).ID)
	if err != nil {
		fail(w, 500, err)
		return
	}
	defer root.Close()
	all, err := backups.List(r.Context(), root)
	if err != nil {
		fail(w, 500, err)
		return
	}
	out := []map[string]any{}
	for _, b := range all {
		out = append(out, a.backupWire(r.Context(), instance(r).ID, b))
	}
	remote, err := a.remoteBackups(r.Context(), instance(r).ID)
	if err != nil {
		fail(w, 500, err)
		return
	}
	for _, record := range remote {
		found := false
		for _, b := range all {
			if b.ID == record.Backup.ID {
				found = true
				break
			}
		}
		if !found {
			wire := a.backupWire(r.Context(), instance(r).ID, record.Backup)
			wire["local"] = false
			out = append(out, wire)
		}
	}
	respond(w, 200, out)
}

type backupRequest struct {
	Note    string `json:"note"`
	Online  bool   `json:"online"`
	Format  string `json:"format,omitempty"`
	Confirm bool   `json:"confirm,omitempty"`
}

func (a *App) createBackup(w http.ResponseWriter, r *http.Request) {
	var body backupRequest
	if err := decode(r, &body); err != nil {
		fail(w, 400, err)
		return
	}
	if len(body.Note) > 1000 {
		fail(w, 400, errors.New("backup note must not exceed 1000 bytes"))
		return
	}
	if body.Format != "" && body.Format != "zip" && body.Format != "tar.zst" {
		fail(w, 400, errors.New("backup format must be zip or tar.zst"))
		return
	}
	a.enqueue(w, r, instance(r).ID, "backup", body)
}
func (a *App) downloadBackup(w http.ResponseWriter, r *http.Request) {
	root, err := a.backupRoot(instance(r).ID)
	if err != nil {
		fail(w, 500, err)
		return
	}
	defer root.Close()
	f, b, err := backups.Open(root, chi.URLParam(r, "backupID"))
	if err != nil {
		fail(w, 404, errors.New("verified backup record not found"))
		return
	}
	defer f.Close()
	name := b.ID + "." + string(b.Format)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, name, b.CreatedAt, f)
}
func (a *App) previewBackup(w http.ResponseWriter, r *http.Request) {
	root, err := a.backupRoot(instance(r).ID)
	if err != nil {
		fail(w, 500, err)
		return
	}
	defer root.Close()
	manifest, err := backups.Verify(r.Context(), root, chi.URLParam(r, "backupID"), nil)
	if err != nil {
		fail(w, 400, err)
		return
	}
	if _, err := a.backupInstance(r.Context(), root, chi.URLParam(r, "backupID"), instance(r)); err != nil {
		fail(w, 400, err)
		return
	}
	respond(w, 200, map[string]any{"files": manifest.Entries, "file_count": len(manifest.Entries), "total_bytes": manifest.Bytes, "warnings": []string{"Restore replaces instance data only after stopping Minecraft and backing up the current state. World progress will be replaced by your explicit request."}})
}

func (a *App) backupInstance(ctx context.Context, storage *os.Root, id string, current model.Instance) (model.Instance, error) {
	if !model.ValidID(id) {
		return model.Instance{}, errors.New("invalid backup identifier")
	}
	metadata, err := files.ReadText(ctx, storage, id+".instance.json", 1<<20)
	if err != nil {
		return model.Instance{}, fmt.Errorf("portable instance metadata is unavailable; no restore was applied: %w", err)
	}
	var restored model.Instance
	if err := json.Unmarshal([]byte(metadata), &restored); err != nil {
		return model.Instance{}, fmt.Errorf("invalid portable instance metadata: %w", err)
	}
	if restored.ID != current.ID || restored.Root != current.Root || restored.Port != current.Port || restored.RCONPort != current.RCONPort || restored.Adopted != current.Adopted {
		return model.Instance{}, errors.New("snapshot identity, directory, ports, or ownership mode differs from this instance; no restore was applied")
	}
	if err := a.Instances.Validate(restored); err != nil {
		return model.Instance{}, fmt.Errorf("snapshot instance configuration failed preflight: %w", err)
	}
	restored.RCONSecret = current.RCONSecret
	return restored, nil
}
func (a *App) verifyBackup(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "backupID")
	if !model.ValidID(id) {
		fail(w, 400, errors.New("invalid backup identifier"))
		return
	}
	a.enqueue(w, r, instance(r).ID, "backup_verify", map[string]string{"backup_id": id})
}
func (a *App) restoreBackup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Confirm bool `json:"confirm"`
	}
	if err := decode(r, &body); err != nil {
		fail(w, 400, err)
		return
	}
	if !body.Confirm {
		fail(w, 400, errors.New("restoring world progress requires explicit administrator confirmation"))
		return
	}
	id := chi.URLParam(r, "backupID")
	if !model.ValidID(id) {
		fail(w, 400, errors.New("invalid backup identifier"))
		return
	}
	a.enqueue(w, r, instance(r).ID, "restore", map[string]string{"backup_id": id})
}

func (a *App) safetyBackup(ctx context.Context, t *jobs.Task, i model.Instance, note string) (backups.Backup, error) {
	if err := a.prepareOffline(ctx, i); err != nil {
		return backups.Backup{}, err
	}
	return a.createLocalBackup(ctx, t, i, note, backups.ZIP)
}
func (a *App) createLocalBackup(ctx context.Context, t *jobs.Task, i model.Instance, note string, format backups.Format) (backups.Backup, error) {
	root, err := a.Instances.OpenRoot(i.Root)
	if err != nil {
		return backups.Backup{}, err
	}
	defer root.Close()
	storage, err := a.backupRoot(i.ID)
	if err != nil {
		return backups.Backup{}, err
	}
	defer storage.Close()
	manifest, err := treeManifest(ctx, root, "")
	if err != nil {
		return backups.Backup{}, err
	}
	if err := ensureSpace(storage, manifest.Bytes+int64(len(manifest.Entries))*1024); err != nil {
		return backups.Backup{}, err
	}
	backupCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	last := time.Time{}
	progress := func(bytes int64) {
		if time.Since(last) < time.Second {
			return
		}
		last = time.Now()
		percent := 10
		if manifest.Bytes > 0 {
			percent = 10 + int(min(bytes*65/manifest.Bytes, 65))
		}
		if err := t.Progress(backupCtx, percent, fmt.Sprintf("Archived %d of %d bytes; verification follows", bytes, manifest.Bytes)); err != nil {
			cancel()
		}
	}
	b, err := backups.Create(backupCtx, root, storage, format, progress)
	if err != nil {
		return b, err
	}
	if err := a.saveSetting(ctx, "backup-note:"+i.ID+":"+b.ID, note); err != nil {
		return b, fmt.Errorf("backup is verified locally but its note could not be saved: %w", err)
	}
	metadata, err := json.Marshal(i)
	if err != nil {
		return b, err
	}
	if err := files.WriteText(ctx, storage, b.ID+".instance.json", string(metadata), 1<<20); err != nil {
		return b, fmt.Errorf("archive is verified but portable instance metadata could not be saved: %w", err)
	}
	return b, nil
}
func (a *App) registerBackupJobs() {
	a.Jobs.Register("backup_offline", 0, func(ctx context.Context, t *jobs.Task, raw json.RawMessage) (any, error) {
		var body struct {
			Note string `json:"note"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			return nil, err
		}
		if len(body.Note) > 1000 {
			return nil, errors.New("backup note must not exceed 1000 bytes")
		}
		i, err := a.Instances.Get(ctx, t.Job.InstanceID)
		if err != nil {
			return nil, err
		}
		b, err := a.safetyBackup(ctx, t, i, body.Note)
		return map[string]any{"backup": b, "state": "offline"}, err
	})
	a.Jobs.Register("backup", 0, func(ctx context.Context, t *jobs.Task, raw json.RawMessage) (result any, err error) {
		var body backupRequest
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
		status, err := a.runtimeStatus(ctx, i)
		if err != nil {
			return nil, err
		}
		if err := t.Progress(ctx, 2, "Checking runtime and backup policy"); err != nil {
			return nil, err
		}
		wasRunning := status.PID > 0 || status.Ready
		if body.Online {
			if !status.Ready {
				return nil, errors.New("online backup requires a ready Minecraft server with authenticated RCON")
			}
			defer func() {
				cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
				defer cancel()
				_, saveErr := a.runtimeCommand(cleanup, i, "save-on")
				if saveErr != nil {
					err = errors.Join(err, errors.New("could not re-enable Minecraft saving; run save-on through the console immediately"))
					a.notify(cleanup, "job_failed", i.ID, t.Job.ID)
				}
			}()
			if _, err := a.runtimeCommand(ctx, i, "save-off"); err != nil {
				return nil, err
			}
			if _, err := a.runtimeCommand(ctx, i, "save-all flush"); err != nil {
				return nil, err
			}
		} else if wasRunning {
			if err := a.stopMinecraft(ctx, i); err != nil {
				return nil, err
			}
			defer func() {
				restartCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
				defer cancel()
				restartErr := a.Runtime.Start(restartCtx, i)
				if restartErr == nil {
					_, restartErr = a.awaitReady(restartCtx, i.ID, 3*time.Minute)
				}
				if restartErr != nil {
					err = errors.Join(err, errors.New("backup finished but Minecraft did not restart cleanly; inspect the console"))
				}
			}()
		}
		if !body.Online {
			if err := a.prepareOffline(ctx, i); err != nil {
				return nil, err
			}
		}
		format := backups.ZIP
		if body.Format == "tar.zst" {
			format = backups.TARZSTD
		}
		b, err := a.createLocalBackup(ctx, t, i, body.Note, format)
		if err != nil {
			a.notify(ctx, "backup_failed", i.ID, t.Job.ID)
			return nil, err
		}
		remote, remoteErr := a.replicateBackup(ctx, i.ID, b)
		result = map[string]any{"backup": a.backupWire(ctx, i.ID, b), "local": "verified", "offsite": remote}
		if remoteErr != nil {
			a.notify(ctx, "offsite_failed", i.ID, t.Job.ID)
			return result, fmt.Errorf("local backup %s is verified; offsite replication failed: %w", b.ID, remoteErr)
		}
		a.notify(ctx, "backup_succeeded", i.ID, t.Job.ID)
		return result, nil
	})
	a.Jobs.Register("backup_verify", 0, func(ctx context.Context, t *jobs.Task, raw json.RawMessage) (any, error) {
		var body struct {
			BackupID string `json:"backup_id"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			return nil, err
		}
		root, err := a.backupRoot(t.Job.InstanceID)
		if err != nil {
			return nil, err
		}
		defer root.Close()
		if err := t.Progress(ctx, 10, "Verifying the archive and every manifest entry"); err != nil {
			return nil, err
		}
		return backups.Verify(ctx, root, body.BackupID, nil)
	})
	a.Jobs.Register("restore", 0, func(ctx context.Context, t *jobs.Task, raw json.RawMessage) (any, error) {
		var body struct {
			BackupID string `json:"backup_id"`
		}
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
		storage, err := a.backupRoot(i.ID)
		if err != nil {
			return nil, err
		}
		defer storage.Close()
		if err := t.Progress(ctx, 5, "Verifying the selected snapshot before stopping Minecraft"); err != nil {
			return nil, err
		}
		manifest, err := backups.Verify(ctx, storage, body.BackupID, nil)
		if err != nil {
			return nil, err
		}
		restoredInstance, err := a.backupInstance(ctx, storage, body.BackupID, i)
		if err != nil {
			return nil, err
		}
		parent, target, err := a.Instances.OpenParent(i.Root)
		if err != nil {
			return nil, err
		}
		defer parent.Close()
		if err := ensureSpace(parent, manifest.Bytes*2); err != nil {
			return nil, err
		}
		status, err := a.runtimeStatus(ctx, i)
		if err != nil {
			return nil, err
		}
		if status.PID > 0 || status.Ready {
			if err := a.stopMinecraft(ctx, i); err != nil {
				return nil, err
			}
		}
		before, err := a.safetyBackup(ctx, t, i, "Before explicitly confirmed restore")
		if err != nil {
			return nil, err
		}
		if err := t.Progress(ctx, 70, "Extracting and validating restore data in same-filesystem staging"); err != nil {
			return nil, err
		}
		stage, err := backups.StageRestore(ctx, storage, body.BackupID, parent, nil)
		if err != nil {
			return nil, err
		}
		if err := a.assertStopped(ctx, i); err != nil {
			return nil, err
		}
		recovery := restoreRecovery{BackupID: body.BackupID, PreRestoreBackupID: before.ID, Previous: i, Restored: restoredInstance, Stage: stage.Name}
		if err := a.saveSetting(ctx, "restore-pending:"+i.ID, recovery); err != nil {
			return nil, err
		}
		if err := t.CommitBarrier(ctx); err != nil {
			return nil, err
		}
		restored, restoreErr := backups.CommitRestore(ctx, parent, target, stage.Name)
		recovery.Restore = restored
		saveErr := a.saveSetting(context.WithoutCancel(ctx), "restore-pending:"+i.ID, recovery)
		result := map[string]any{"restore": restored, "pre_restore_backup_id": before.ID, "state": "offline", "note": "Minecraft remains stopped so you can review restored runtime/content before starting."}
		if restoreErr != nil || saveErr != nil {
			a.notify(ctx, "restore_failed", i.ID, t.Job.ID)
			return result, errors.Join(restoreErr, saveErr)
		}
		if err := a.finishBackupRestore(ctx, i, restoredInstance, body.BackupID); err != nil {
			return result, err
		}
		a.notify(ctx, "restore_succeeded", i.ID, t.Job.ID)
		return result, nil
	})
	a.Jobs.Register("restore_reconcile", 0, a.reconcileRestoreJob)
}

func backupExtension(b backups.Backup) string {
	if strings.Contains(string(b.Format), "/") {
		return ""
	}
	return "." + string(b.Format)
}
