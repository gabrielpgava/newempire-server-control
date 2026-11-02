package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gabrielpgava/newempire-server-control/internal/auth"
	"github.com/gabrielpgava/newempire-server-control/internal/backups"
	"github.com/gabrielpgava/newempire-server-control/internal/jobs"
	"github.com/gabrielpgava/newempire-server-control/internal/model"
	"github.com/gabrielpgava/newempire-server-control/internal/notifications"
	"github.com/gabrielpgava/newempire-server-control/internal/storage"
)

type retentionPolicy struct {
	Enabled  bool `json:"enabled"`
	KeepLast int  `json:"keep_last"`
}

func (a *App) loadSecret(ctx context.Context, key string, out any) error {
	var encrypted []byte
	err := a.DB.QueryRowContext(ctx, "SELECT value FROM secrets WHERE key=?", key).Scan(&encrypted)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	plain, err := a.Vault.Open(encrypted, "settings:"+key)
	if err != nil {
		return errors.New("settings could not be decrypted; restore the credential master key")
	}
	return json.Unmarshal(plain, out)
}
func (a *App) settings(w http.ResponseWriter, r *http.Request) {
	var remote storage.Config
	var notify notifications.Config
	retention := retentionPolicy{KeepLast: 7}
	if err := errors.Join(a.loadSecret(r.Context(), "storage", &remote), a.loadSecret(r.Context(), "notifications", &notify), a.setting(r.Context(), "backup-retention", &retention)); err != nil {
		fail(w, 500, err)
		return
	}
	out := map[string]any{"public_url": a.Config.PublicURL, "version": a.Version, "runtime_provider": "systemd", "notifications": map[string]any{"configured": notify.Redacted().Configured, "enabled": notify.Enabled}, "storage": map[string]any{"configured": remote.Redacted().Configured, "enabled": remote.Enabled}, "backup_retention": retention}
	out["adopt_roots"] = []string{}
	out["java_paths"] = []string{}
	if auth.Permitted(session(r).User.Role, "admin") {
		out["adopt_roots"] = append([]string{}, a.Config.AdoptRoots...)
		out["java_paths"] = append([]string{}, a.Config.JavaPaths...)
	}
	respond(w, 200, out)
}
func (a *App) mergedConfig(ctx context.Context, key string, patch json.RawMessage, out any) error {
	old := map[string]json.RawMessage{}
	if err := a.loadSecret(ctx, key, &old); err != nil {
		return err
	}
	var changes map[string]json.RawMessage
	if err := json.Unmarshal(patch, &changes); err != nil || changes == nil {
		return errors.New("settings section must be a JSON object")
	}
	for name, value := range changes {
		if string(value) == "null" {
			return errors.New("settings fields cannot be null")
		}
		old[name] = value
	}
	merged, err := json.Marshal(old)
	if err != nil {
		return err
	}
	d := json.NewDecoder(strings.NewReader(string(merged)))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return errors.New("settings contain unsupported fields or invalid values")
	}
	return nil
}
func (a *App) saveSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Storage       json.RawMessage  `json:"storage"`
		Notifications json.RawMessage  `json:"notifications"`
		Retention     *retentionPolicy `json:"backup_retention"`
	}
	if err := decode(r, &body); err != nil {
		fail(w, 400, err)
		return
	}
	if (len(body.Storage) > 0 || len(body.Notifications) > 0) && session(r).User.Role != "owner" {
		fail(w, 403, errors.New("only the owner may configure outbound destinations and credentials"))
		return
	}
	encrypted := map[string][]byte{}
	for key, raw := range map[string]json.RawMessage{"storage": body.Storage, "notifications": body.Notifications} {
		if len(raw) == 0 {
			continue
		}
		var value any
		if key == "storage" {
			var cfg storage.Config
			if err := a.mergedConfig(r.Context(), key, raw, &cfg); err != nil {
				fail(w, 400, err)
				return
			}
			if err := cfg.Validate(); err != nil {
				fail(w, 400, err)
				return
			}
			value = cfg
		} else {
			var cfg notifications.Config
			if err := a.mergedConfig(r.Context(), key, raw, &cfg); err != nil {
				fail(w, 400, err)
				return
			}
			if err := cfg.Validate(); err != nil {
				fail(w, 400, err)
				return
			}
			value = cfg
		}
		plain, err := json.Marshal(value)
		if err != nil {
			fail(w, 400, err)
			return
		}
		ciphertext, err := a.Vault.Seal(plain, "settings:"+key)
		if err != nil {
			fail(w, 500, err)
			return
		}
		encrypted[key] = ciphertext
	}
	if body.Retention != nil && (body.Retention.KeepLast < 1 || body.Retention.KeepLast > 10000) {
		fail(w, 400, errors.New("retention must keep between 1 and 10000 verified backups"))
		return
	}
	if len(encrypted) == 0 && body.Retention == nil {
		fail(w, 400, errors.New("no supported settings were supplied"))
		return
	}
	tx, err := a.DB.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, 500, err)
		return
	}
	defer tx.Rollback()
	for key, value := range encrypted {
		if _, err := tx.ExecContext(r.Context(), "INSERT INTO secrets(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, value); err != nil {
			fail(w, 500, err)
			return
		}
	}
	if body.Retention != nil {
		raw, _ := json.Marshal(body.Retention)
		if _, err := tx.ExecContext(r.Context(), "INSERT INTO settings(key,value) VALUES('backup-retention',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", string(raw)); err != nil {
			fail(w, 500, err)
			return
		}
	}
	if _, err := tx.ExecContext(r.Context(), "INSERT INTO audit(time,user_id,action,instance_id,detail) VALUES(?,?,'settings.save','','Nonsecret preferences and encrypted credential configuration updated')", time.Now().Unix(), session(r).User.ID); err != nil {
		fail(w, 500, err)
		return
	}
	if err := tx.Commit(); err != nil {
		fail(w, 500, err)
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}

type offsiteRecord struct {
	State     string         `json:"state"`
	Object    storage.Object `json:"object"`
	Backup    backups.Backup `json:"backup"`
	Instance  model.Instance `json:"instance"`
	UpdatedAt int64          `json:"updated_at"`
}

func (a *App) replicateBackup(ctx context.Context, instanceID string, b backups.Backup) (any, error) {
	var cfg storage.Config
	if err := a.loadSecret(ctx, "storage", &cfg); err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return map[string]string{"state": "disabled"}, nil
	}
	client, err := storage.New(cfg)
	if err != nil {
		return nil, err
	}
	name := b.ID + backupExtension(b)
	source := filepath.Join(a.Config.BackupDir, instanceID, name)
	object, err := client.Put(ctx, instanceID+"/"+name, source)
	if err != nil {
		return map[string]string{"state": "failed"}, err
	}
	i, err := a.Instances.Get(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	if object.SHA256 != b.ArchiveSHA256 || object.Size != b.Bytes {
		return nil, errors.New("offsite object does not match the verified local backup")
	}
	record := offsiteRecord{State: "uploaded", Object: object, Backup: b, Instance: i, UpdatedAt: time.Now().Unix()}
	if err := a.saveSetting(ctx, "backup-remote:"+instanceID+":"+b.ID, record); err != nil {
		return record, errors.New("remote copy accepted but the trusted local restoration record could not be saved")
	}
	return record, nil
}

func (a *App) notify(ctx context.Context, kind, instanceID, jobID string) {
	if kind == "" {
		return
	}
	event := notifications.Event{ID: model.NewID(), Kind: kind, InstanceID: instanceID, JobID: jobID, OccurredAt: time.Now().UTC()}
	raw, err := json.Marshal(event)
	if err != nil {
		return
	}
	_, err = a.DB.ExecContext(ctx, "INSERT INTO notifications(id,time,level,message,next_attempt) VALUES(?,?,?,?,?)", event.ID, event.OccurredAt.Unix(), kind, string(raw), time.Now().Unix())
	if err != nil {
		a.audit(ctx, "system", "notification.queue_failed", instanceID, "Could not persist a notification")
	}
}
func (a *App) deliverNotifications(ctx context.Context, now time.Time) error {
	var cfg notifications.Config
	if err := a.loadSecret(ctx, "notifications", &cfg); err != nil {
		return err
	}
	if !cfg.Enabled {
		return nil
	}
	var id, raw string
	var attempts int
	err := a.DB.QueryRowContext(ctx, "SELECT id,message,attempts FROM notifications WHERE delivered=0 AND attempts<5 AND next_attempt<=? ORDER BY time LIMIT 1", now.Unix()).Scan(&id, &raw, &attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var event notifications.Event
	if err := json.Unmarshal([]byte(raw), &event); err != nil {
		return err
	}
	sender, err := notifications.New(cfg)
	if err != nil {
		return err
	}
	deliveryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err = sender.Notify(deliveryCtx, event)
	next := now.Add(time.Duration(1<<attempts) * time.Minute).Unix()
	delivered := err == nil
	message := ""
	if err != nil {
		message = "Notification delivery was not accepted; retry is bounded to five attempts"
		var failure *notifications.DeliveryError
		if errors.As(err, &failure) {
			if !failure.Retryable {
				attempts = 4
			}
			if failure.RetryAfter > 0 {
				next = now.Add(failure.RetryAfter).Unix()
			}
		}
	}
	_, saveErr := a.DB.ExecContext(ctx, "UPDATE notifications SET delivered=?,attempts=?,next_attempt=?,last_error=? WHERE id=?", delivered, attempts+1, next, message, id)
	a.DB.ExecContext(ctx, "DELETE FROM notifications WHERE time<? AND (delivered=1 OR attempts>=5)", now.Add(-30*24*time.Hour).Unix())
	return saveErr
}

func (a *App) registerSettingsJobs() {
	a.Jobs.Register("retention", 0, func(ctx context.Context, t *jobs.Task, _ json.RawMessage) (any, error) {
		p := retentionPolicy{KeepLast: 7}
		if err := a.setting(ctx, "backup-retention", &p); err != nil {
			return nil, err
		}
		if !p.Enabled {
			return map[string]int{"removed": 0}, nil
		}
		if p.KeepLast < 1 || p.KeepLast > 10000 {
			return nil, errors.New("invalid retention policy; no backups were removed")
		}
		if err := a.checkRestorePending(ctx, t.Job.InstanceID); err != nil {
			return map[string]int{"removed": 0}, err
		}
		var pending runtimeRecovery
		var review string
		if err := a.setting(ctx, "runtime-pending:"+t.Job.InstanceID, &pending); err != nil {
			return nil, err
		}
		if err := a.setting(ctx, "runtime-world-review:"+t.Job.InstanceID, &review); err != nil {
			return nil, err
		}
		if pending.Directory != "" || review != "" {
			return map[string]int{"removed": 0}, errors.New("backup retention is paused while runtime or world recovery requires its restore points")
		}
		root, err := a.backupRoot(t.Job.InstanceID)
		if err != nil {
			return nil, err
		}
		defer root.Close()
		all, err := backups.List(ctx, root)
		if err != nil {
			return nil, err
		}
		slices.SortFunc(all, func(a, b backups.Backup) int { return b.CreatedAt.Compare(a.CreatedAt) })
		if len(all) <= p.KeepLast {
			return map[string]int{"removed": 0}, nil
		}
		for _, b := range all[:p.KeepLast] {
			if _, err := backups.Verify(ctx, root, b.ID, nil); err != nil {
				return nil, errors.New("a retained backup failed verification; no retention deletion was performed")
			}
		}
		if err := t.CommitBarrier(ctx); err != nil {
			return nil, err
		}
		removed := 0
		for _, b := range all[p.KeepLast:] {
			if !model.ValidID(b.ID) {
				return nil, errors.New("invalid backup catalog identifier")
			}
			if err := root.Remove(b.ID + backupExtension(b)); err != nil {
				return map[string]int{"removed": removed}, err
			}
			if err := root.Remove(b.ID + ".json"); err != nil {
				return map[string]int{"removed": removed}, err
			}
			if err := root.Remove(b.ID + ".instance.json"); err != nil && !errors.Is(err, os.ErrNotExist) {
				return map[string]int{"removed": removed}, err
			}
			removed++
		}
		return map[string]int{"removed": removed}, nil
	})
}
