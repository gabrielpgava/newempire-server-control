package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"

	"github.com/gabrielpgava/newempire-server-control/internal/backups"
	"github.com/gabrielpgava/newempire-server-control/internal/files"
	"github.com/gabrielpgava/newempire-server-control/internal/jobs"
	"github.com/gabrielpgava/newempire-server-control/internal/model"
	"github.com/gabrielpgava/newempire-server-control/internal/modrinth"
)

type filesystemRecord struct {
	Target   string `json:"target"`
	Stage    string `json:"stage"`
	Recovery string `json:"recovery"`
	Journal  string `json:"-"`
}

type pendingRestore struct {
	BackupID string                `json:"backup_id"`
	BeforeID string                `json:"pre_restore_backup_id"`
	Previous model.Instance        `json:"previous"`
	Restored model.Instance        `json:"restored"`
	Stage    string                `json:"stage"`
	Restore  backups.RestoreResult `json:"restore"`
}

func (p *offlinePanel) pendingRestore(ctx context.Context, id string) (*pendingRestore, error) {
	var raw string
	if err := p.app.DB.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=?", "restore-pending:"+id).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	var pending pendingRestore
	if json.Unmarshal([]byte(raw), &pending) != nil || !model.ValidID(pending.BackupID) || !model.ValidID(pending.BeforeID) || pending.Previous.ID != id || pending.Restored.ID != id {
		return nil, errors.New("pending restore record is invalid; preserve panel state and retained directories for a version-specific recovery plan")
	}
	return &pending, nil
}

func (p *offlinePanel) requireRestoreSettled(ctx context.Context, id string) error {
	pending, err := p.pendingRestore(ctx, id)
	if err != nil {
		return err
	}
	if pending != nil {
		return errors.New("a full-tree restore requires metadata reconciliation; inspect recover restore INSTANCE_ID (or restore-previous if the current tree is the recorded previous data), then explicitly confirm reconciliation before another mutation")
	}
	return nil
}

func readRecoveryJSON(root *os.Root, name string, value any) error {
	f, err := files.OpenRegular(root, name)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return err
	}
	if len(b) > 4096 {
		return files.ErrTooLarge
	}
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("unexpected data after recovery journal")
	}
	return nil
}

func rootNames(root *os.Root) ([]string, error) {
	f, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	names, err := f.Readdirnames(100001)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(names) > 100000 {
		return nil, files.ErrTooLarge
	}
	return names, nil
}

func filesystemRecords(parent *os.Root, target string) ([]filesystemRecord, error) {
	names, err := rootNames(parent)
	if err != nil {
		return nil, err
	}
	records := []filesystemRecord{}
	for _, name := range names {
		if !strings.HasPrefix(name, ".nesc-transaction-") || !strings.HasSuffix(name, ".json") || !model.ValidID(strings.TrimSuffix(strings.TrimPrefix(name, ".nesc-transaction-"), ".json")) {
			continue
		}
		var record filesystemRecord
		if err := readRecoveryJSON(parent, name, &record); err != nil {
			return nil, fmt.Errorf("unsafe filesystem recovery journal %s: %w", name, err)
		}
		if record.Target != target || record.Recovery == "" {
			continue
		}
		if !strings.HasPrefix(record.Recovery, ".nesc-recovery-") || !model.ValidID(strings.TrimPrefix(record.Recovery, ".nesc-recovery-")) {
			return nil, errors.New("invalid retained filesystem recovery identifier")
		}
		info, err := files.Check(parent, record.Recovery)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.IsDir() {
			return nil, fmt.Errorf("unsafe retained recovery directory %s: %w", record.Recovery, errors.Join(err, files.ErrUnsafeType))
		}
		record.Journal = name
		records = append(records, record)
	}
	return records, nil
}

func (p *offlinePanel) originalBackup(ctx context.Context, instanceID, recoveryID string) (string, error) {
	var raw string
	err := p.app.DB.QueryRowContext(ctx, "SELECT result FROM jobs WHERE instance_id=? AND kind IN ('restore','recovery_filesystem') AND json_valid(result) AND json_extract(result,'$.restore.recovery')=? ORDER BY created_at DESC,rowid DESC LIMIT 1", instanceID, recoveryID).Scan(&raw)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", errors.New("retained directory has no completed portable recovery record; preserve it and explicitly restore a verified local backup instead")
		}
		return "", err
	}
	var result struct {
		BeforeRestore  string `json:"pre_restore_backup_id"`
		BeforeRecovery string `json:"pre_recovery_backup_id"`
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return "", err
	}
	id := result.BeforeRestore
	if id == "" {
		id = result.BeforeRecovery
	}
	if !model.ValidID(id) {
		return "", errors.New("retained directory lacks a verified original backup identifier; do not guess a recovery source")
	}
	return id, nil
}

func (p *offlinePanel) recoveryRecords(ctx context.Context, i model.Instance) ([]map[string]any, error) {
	parent, target, err := safeInstanceParent(p.app.Config, i)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	retained, err := filesystemRecords(parent, target)
	if err != nil {
		return nil, err
	}
	all := []map[string]any{}
	pending, err := p.pendingRestore(ctx, i.ID)
	if err != nil {
		return nil, err
	}
	if pending != nil {
		all = append(all, map[string]any{"kind": "restore", "pending": pending, "world_data": "metadata reconciliation never swaps worlds"})
	}
	for _, record := range retained {
		id, err := p.originalBackup(ctx, i.ID, record.Recovery)
		entry := map[string]any{"kind": "filesystem", "id": record.Recovery, "journal": record.Journal, "original_backup_id": id}
		if err != nil {
			entry["blocked"] = err.Error()
		}
		all = append(all, entry)
	}
	root, err := parent.OpenRoot(target)
	if err == nil {
		defer root.Close()
		names, err := rootNames(root)
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			if !strings.HasPrefix(name, ".newempire-content-") || !strings.HasSuffix(name, "-transaction.json") {
				continue
			}
			id := strings.TrimSuffix(strings.TrimPrefix(name, ".newempire-content-"), "-transaction.json")
			tx, err := modrinth.LoadTransaction(root, id)
			if err != nil {
				return nil, err
			}
			all = append(all, map[string]any{"kind": "content", "id": tx.ID, "state": tx.State, "world_data": "never restored by content recovery"})
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	rows, err := p.app.DB.QueryContext(ctx, "SELECT key,value FROM settings WHERE key LIKE ? OR key=? ORDER BY key", "recovery:"+i.ID+":%", "runtime-pending:"+i.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key, raw string
		if err := rows.Scan(&key, &raw); err != nil {
			return nil, err
		}
		if strings.HasPrefix(key, "runtime-pending:") {
			var record struct {
				Directory string `json:"directory"`
				BackupID  string `json:"backup_id"`
				State     string `json:"state"`
			}
			if err := json.Unmarshal([]byte(raw), &record); err != nil {
				return nil, err
			}
			all = append(all, map[string]any{"kind": "runtime", "directory": record.Directory, "backup_id": record.BackupID, "state": record.State, "world_data": "never automatically restored"})
		} else {
			var record struct {
				Token string `json:"token"`
				Path  string `json:"path"`
			}
			if err := json.Unmarshal([]byte(raw), &record); err != nil {
				return nil, err
			}
			if !model.ValidID(record.Token) || key != "recovery:"+i.ID+":"+record.Token || files.ValidatePath(record.Path) != nil {
				return nil, errors.New("invalid retained file recovery record")
			}
			all = append(all, map[string]any{"kind": "file", "id": record.Token, "path": record.Path})
		}
	}
	return all, rows.Err()
}

func (p *offlinePanel) recoverFilesystemJob(ctx context.Context, t *jobs.Task, raw json.RawMessage) (any, error) {
	var body recoveryRequest
	if json.Unmarshal(raw, &body) != nil || !body.Confirm || !strings.HasPrefix(body.ID, ".nesc-recovery-") || !model.ValidID(strings.TrimPrefix(body.ID, ".nesc-recovery-")) {
		return nil, errors.New("confirmed exact .nesc-recovery-ID is required")
	}
	i, err := p.app.Instances.Get(ctx, t.Job.InstanceID)
	if err != nil {
		return nil, err
	}
	if err := p.requireRestoreSettled(ctx, i.ID); err != nil {
		return nil, err
	}
	parent, target, err := safeInstanceParent(p.app.Config, i)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	records, err := filesystemRecords(parent, target)
	if err != nil {
		return nil, err
	}
	found := false
	for _, record := range records {
		found = found || record.Recovery == body.ID
	}
	if !found {
		return nil, errors.New("retained filesystem recovery record not found for this instance")
	}
	id, err := p.originalBackup(ctx, i.ID, body.ID)
	if err != nil {
		return nil, err
	}
	storage, err := p.backupRoot(i.ID)
	if err != nil {
		return nil, err
	}
	defer storage.Close()
	expected, err := backups.Verify(ctx, storage, id, nil)
	if err != nil {
		return nil, err
	}
	previous, err := p.app.BackupInstance(ctx, storage, id, i)
	if err != nil {
		return nil, err
	}
	currentJSON, _ := json.Marshal(i)
	previousJSON, _ := json.Marshal(previous)
	if string(currentJSON) != string(previousJSON) {
		return nil, fmt.Errorf("retained tree needs different instance configuration; explicitly restore verified backup %s so runtime/helper configuration is restored together", id)
	}
	if err := p.app.AssertStopped(ctx, i); err != nil {
		return nil, err
	}
	source, err := parent.OpenRoot(body.ID)
	if err != nil {
		return nil, err
	}
	defer source.Close()
	retainedBackup, err := backups.Create(ctx, source, storage, backups.ZIP, nil)
	if err != nil {
		return nil, err
	}
	actual, err := backups.Verify(ctx, storage, retainedBackup.ID, nil)
	if err != nil {
		return nil, err
	}
	if actual.Bytes != expected.Bytes || !reflect.DeepEqual(actual.Entries, expected.Entries) {
		return map[string]string{"inspection_backup_id": retainedBackup.ID, "recovery_id": body.ID}, fmt.Errorf("retained tree differs from its verified original backup; inspection backup %s is retained, but no data was swapped", retainedBackup.ID)
	}
	if err := files.WriteText(ctx, storage, retainedBackup.ID+".instance.json", string(previousJSON), 1<<20); err != nil {
		return nil, err
	}
	before, err := p.app.RecoveryBackup(ctx, t, i, "Before explicit retained filesystem recovery "+body.ID)
	if err != nil {
		return nil, err
	}
	pending := pendingRestore{BackupID: retainedBackup.ID, BeforeID: before.ID, Previous: i, Restored: previous, Stage: body.ID}
	pendingJSON, err := json.Marshal(pending)
	if err != nil {
		return nil, err
	}
	if _, err := p.app.DB.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES(?,?)", "restore-pending:"+i.ID, string(pendingJSON)); err != nil {
		return nil, err
	}
	if err := t.CommitBarrier(ctx); err != nil {
		return nil, err
	}
	recovered, recoverErr := backups.Recover(ctx, parent, target, body.ID)
	pending.Restore = recovered
	pendingJSON, err = json.Marshal(pending)
	if err == nil {
		_, err = p.app.DB.ExecContext(ctx, "UPDATE settings SET value=? WHERE key=?", string(pendingJSON), "restore-pending:"+i.ID)
	}
	if recoverErr == nil && err == nil && recovered.Installed {
		_, err = p.app.DB.ExecContext(ctx, "DELETE FROM settings WHERE key=?", "restore-pending:"+i.ID)
	}
	return map[string]any{"restore": recovered, "pre_recovery_backup_id": before.ID, "verified_recovery_backup_id": retainedBackup.ID, "state": "offline", "note": "The displaced tree, swap journal, and both verified snapshots are retained. Minecraft was not started. Any remaining restore-pending guard requires explicit metadata reconciliation."}, errors.Join(recoverErr, err)
}
