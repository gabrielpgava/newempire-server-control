package api

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"path/filepath"

	"github.com/gabrielpgava/newempire-server-control/internal/backups"
	"github.com/gabrielpgava/newempire-server-control/internal/files"
	"github.com/gabrielpgava/newempire-server-control/internal/jobs"
	"github.com/gabrielpgava/newempire-server-control/internal/model"
	"github.com/gabrielpgava/newempire-server-control/internal/storage"
	"github.com/go-chi/chi/v5"
)

func (a *App) remoteBackups(ctx context.Context, id string) ([]offsiteRecord, error) {
	rows, err := a.DB.QueryContext(ctx, "SELECT value FROM settings WHERE key LIKE ?", "backup-remote:"+id+":%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	all := []offsiteRecord{}
	for rows.Next() {
		var raw string
		var record offsiteRecord
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &record); err != nil {
			return nil, err
		}
		all = append(all, record)
	}
	return all, rows.Err()
}
func (a *App) fetchOffsite(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "backupID")
	if !model.ValidID(id) {
		fail(w, 400, errors.New("invalid backup identifier"))
		return
	}
	a.enqueue(w, r, instance(r).ID, "offsite_fetch", map[string]string{"backup_id": id})
}
func (a *App) registerOffsiteJobs() {
	a.Jobs.Register("offsite_fetch", 0, func(ctx context.Context, t *jobs.Task, raw json.RawMessage) (any, error) {
		var body struct {
			BackupID string `json:"backup_id"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			return nil, err
		}
		var record offsiteRecord
		if err := a.setting(ctx, "backup-remote:"+t.Job.InstanceID+":"+body.BackupID, &record); err != nil {
			return nil, err
		}
		if record.State != "uploaded" || record.Backup.ID != body.BackupID || record.Instance.ID != t.Job.InstanceID || record.Object.SHA256 != record.Backup.ArchiveSHA256 || record.Object.Size != record.Backup.Bytes {
			return nil, errors.New("trusted offsite restoration record is missing or inconsistent")
		}
		var cfg storage.Config
		if err := a.loadSecret(ctx, "storage", &cfg); err != nil {
			return nil, err
		}
		client, err := storage.New(cfg)
		if err != nil {
			return nil, err
		}
		root, err := a.backupRoot(t.Job.InstanceID)
		if err != nil {
			return nil, err
		}
		defer root.Close()
		name := record.Backup.ID + backupExtension(record.Backup)
		if _, err := root.Lstat(name); err == nil {
			return nil, errors.New("local backup already exists; verify it rather than replacing it")
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		if err := ensureSpace(root, record.Object.Size); err != nil {
			return nil, err
		}
		if err := t.Progress(ctx, 10, "Downloading offsite archive and verifying its trusted SHA-256"); err != nil {
			return nil, err
		}
		object, err := client.Download(ctx, record.Object.Key, filepath.Join(a.Config.BackupDir, t.Job.InstanceID, name), record.Backup.ArchiveSHA256, record.Backup.Bytes)
		if err != nil {
			return nil, err
		}
		metadata, _ := json.Marshal(record.Backup)
		if err := files.WriteText(ctx, root, record.Backup.ID+".json", string(metadata), 1<<20); err != nil {
			return nil, err
		}
		metadata, _ = json.Marshal(record.Instance)
		if err := files.WriteText(ctx, root, record.Backup.ID+".instance.json", string(metadata), 1<<20); err != nil {
			return nil, err
		}
		if _, err := backups.Verify(ctx, root, record.Backup.ID, nil); err != nil {
			return nil, err
		}
		return map[string]any{"local": "verified", "offsite": object}, nil
	})
}
