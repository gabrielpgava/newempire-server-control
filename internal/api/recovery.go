package api

import (
	"context"
	"os"

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

func (a *App) RecoveryBackup(ctx context.Context, t *jobs.Task, i model.Instance, note string) (backups.Backup, error) {
	return a.safetyBackup(ctx, t, i, note)
}
