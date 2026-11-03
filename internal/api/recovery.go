package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/gabrielpgava/newempire-server-control/internal/backups"
	"github.com/gabrielpgava/newempire-server-control/internal/jobs"
	"github.com/gabrielpgava/newempire-server-control/internal/model"
)

// Offline recovery callers hold the panel process lock and use durable jobs for mutations.
func (a *App) AssertStopped(ctx context.Context, i model.Instance) error {
	return a.assertStopped(ctx, i)
}

func (a *App) BackupInstance(ctx context.Context, storage *os.Root, id string, current model.Instance) (model.Instance, error) {
	return a.backupInstance(ctx, storage, id, current)
}

func (a *App) BackupInstanceForRecovery(ctx context.Context, storage *os.Root, id string, current model.Instance) (model.Instance, error) {
	return a.backupInstanceMetadata(ctx, storage, id, current, true)
}

func (a *App) RecoveryBackup(ctx context.Context, t *jobs.Task, i model.Instance, note string) (backups.Backup, error) {
	return a.safetyBackup(ctx, t, i, note)
}

func (a *App) RecoverRuntime(ctx context.Context, t *jobs.Task, instanceID string) (any, error) {
	i, err := a.Instances.Get(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	if t.Job.InstanceID != instanceID {
		return nil, errors.New("runtime recovery job belongs to a different instance")
	}
	if err := a.checkRestorePending(ctx, i.ID); err != nil {
		return nil, err
	}
	if err := a.assertStopped(ctx, i); err != nil {
		return nil, err
	}
	var recovery runtimeRecovery
	if err := a.setting(ctx, "runtime-pending:"+i.ID, &recovery); err != nil {
		return nil, err
	}
	if recovery.State != "prepared" && recovery.State != "applied" {
		return nil, errors.New("a recorded pending runtime migration is required for recovery")
	}
	if err := t.Progress(ctx, 10, "Verifying retained runtime files against the pre-migration backup; worlds will not be restored"); err != nil {
		return nil, err
	}
	storage, err := a.backupRoot(i.ID)
	if err != nil {
		return nil, err
	}
	defer storage.Close()
	expected, err := backups.Verify(ctx, storage, recovery.BackupID, nil)
	if err != nil {
		return nil, err
	}
	previous, err := a.backupInstance(ctx, storage, recovery.BackupID, i)
	if err != nil {
		return nil, err
	}
	if etagJSON(previous) != etagJSON(recovery.Previous) {
		return nil, errors.New("pending runtime identity differs from its verified portable snapshot")
	}
	stage, err := os.OpenRoot(filepath.Join(a.Config.DataDir, "staging"))
	if err != nil {
		return nil, err
	}
	defer stage.Close()
	if err := validRuntimeRecoveryDirectory(recovery.Directory); err != nil {
		return nil, err
	}
	old, err := stage.OpenRoot(recovery.Directory)
	if err != nil {
		return nil, err
	}
	defer old.Close()
	retained, err := treeManifest(ctx, old, "")
	if err != nil {
		return nil, err
	}
	if len(retained.Entries) != 2 {
		return nil, errors.New("retained runtime must contain only the recorded launcher and properties")
	}
	for name, original := range map[string]string{"launcher.jar": previous.Jar, "server.properties": "server.properties"} {
		matched := false
		for _, actual := range retained.Entries {
			for _, entry := range expected.Entries {
				if actual.Path == name && entry.Path == original && !actual.Directory && !entry.Directory && actual.Size == entry.Size && actual.SHA256 == entry.SHA256 {
					matched = true
				}
			}
		}
		if !matched {
			return nil, errors.New("retained runtime differs from its verified pre-migration snapshot")
		}
	}
	root, err := a.Instances.OpenRoot(i.Root)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	// An interrupted migration may have mixed runtime files; do not attach misleading portable metadata.
	inspection, err := backups.Create(ctx, root, storage, backups.ZIP, nil)
	if err != nil {
		return nil, err
	}
	result := map[string]any{"inspection_backup_id": inspection.ID, "pre_upgrade_backup_id": recovery.BackupID, "state": "offline", "world_data": "unchanged"}
	if err := t.CommitBarrier(ctx); err != nil {
		return result, err
	}
	recovery.State = "prepared"
	if err := a.saveSetting(ctx, "runtime-pending:"+i.ID, recovery); err != nil {
		return result, err
	}
	return result, a.restoreRuntime(ctx, i, recovery)
}
