package api

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/gabrielpgava/newempire-server-control/internal/backups"
	"github.com/gabrielpgava/newempire-server-control/internal/jobs"
	"github.com/gabrielpgava/newempire-server-control/internal/model"
)

type restoreRecovery struct {
	BackupID           string                `json:"backup_id"`
	PreRestoreBackupID string                `json:"pre_restore_backup_id"`
	Previous           model.Instance        `json:"previous"`
	Restored           model.Instance        `json:"restored"`
	Stage              string                `json:"stage"`
	Restore            backups.RestoreResult `json:"restore"`
}

func (a *App) checkRestorePending(ctx context.Context, id string) error {
	var pending bool
	if err := a.DB.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM settings WHERE key=?)", "restore-pending:"+id).Scan(&pending); err != nil {
		return err
	}
	if pending {
		return errors.New("an interrupted restore requires explicit offline reconciliation before starting Minecraft or changing files; inspect its retained recovery report")
	}
	return nil
}

func (a *App) finishBackupRestore(ctx context.Context, current, restored model.Instance, backupID string) error {
	// A prior helper call may have committed before its acknowledgement or DB update failed.
	if err := a.provision(ctx, restored, true); err != nil {
		return err
	}
	if err := a.Instances.Update(ctx, restored); err != nil {
		return err
	}
	var review string
	if err := a.setting(ctx, "runtime-world-review:"+current.ID, &review); err != nil {
		return err
	}
	if review == backupID {
		if _, err := a.DB.ExecContext(ctx, "DELETE FROM settings WHERE key=?", "runtime-world-review:"+current.ID); err != nil {
			return err
		}
	}
	_, err := a.DB.ExecContext(ctx, "DELETE FROM settings WHERE key IN (?,?)", "runtime-pending:"+current.ID, "restore-pending:"+current.ID)
	return err
}

// Reconciliation never selects or swaps a world; it verifies the administrator's retained tree first.
func (a *App) reconcileRestoreJob(ctx context.Context, t *jobs.Task, raw json.RawMessage) (any, error) {
	var body struct {
		UsePrevious bool `json:"use_previous"`
		Confirm     bool `json:"confirm"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	if !body.Confirm {
		return nil, errors.New("restore reconciliation requires explicit administrator confirmation")
	}
	i, err := a.Instances.Get(ctx, t.Job.InstanceID)
	if err != nil {
		return nil, err
	}
	if err := a.assertStopped(ctx, i); err != nil {
		return nil, err
	}
	var pending restoreRecovery
	if err := a.setting(ctx, "restore-pending:"+i.ID, &pending); err != nil {
		return nil, err
	}
	selected := pending.BackupID
	if body.UsePrevious {
		selected = pending.PreRestoreBackupID
	}
	if !model.ValidID(selected) || pending.Previous.ID != i.ID || pending.Restored.ID != i.ID {
		return nil, errors.New("a valid pending restore record is required for reconciliation")
	}
	storage, err := a.backupRoot(i.ID)
	if err != nil {
		return nil, err
	}
	defer storage.Close()
	if err := t.Progress(ctx, 10, "Verifying the recovery snapshot and stopped instance without replacing world data"); err != nil {
		return nil, err
	}
	expected, err := backups.Verify(ctx, storage, selected, nil)
	if err != nil {
		return nil, err
	}
	restored, err := a.backupInstance(ctx, storage, selected, i)
	if err != nil {
		return nil, err
	}
	root, err := a.Instances.OpenRoot(i.Root)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	actual, err := treeManifest(ctx, root, "")
	if err != nil {
		return nil, err
	}
	if manifestHash(actual) != manifestHash(expected) {
		return nil, errors.New("instance data does not exactly match the selected verified recovery snapshot; no runtime metadata was changed")
	}
	if err := t.CommitBarrier(ctx); err != nil {
		return nil, err
	}
	if err := a.finishBackupRestore(ctx, i, restored, selected); err != nil {
		return nil, err
	}
	return map[string]any{"instance": restored, "state": "offline", "world_data": "verified in place; not replaced by reconciliation"}, nil
}
