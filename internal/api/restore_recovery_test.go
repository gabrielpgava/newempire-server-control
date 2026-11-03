package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gabrielpgava/newempire-server-control/internal/backups"
	"github.com/gabrielpgava/newempire-server-control/internal/jobs"
	"github.com/gabrielpgava/newempire-server-control/internal/model"
)

func interruptedRestore(t *testing.T) (*harness, restoreRecovery) {
	t.Helper()
	h := newHarness(t, nil)
	id := backupID(t, h)
	current := h.instance
	current.MemoryMB = 2048
	if err := h.app.Instances.Update(t.Context(), current); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(current.Root, "world", "level.dat"), []byte("newer world before restore"), 0660); err != nil {
		t.Fatal(err)
	}
	h.app.Provision = func(context.Context, model.Instance, bool) error {
		return errors.New("controlled helper acknowledgement failure")
	}
	j := h.queue(t, "/instances/"+current.ID+"/backups/"+id+"/restore", map[string]bool{"confirm": true})
	if j.State != "failed" || !strings.Contains(j.Error, "controlled helper") {
		t.Fatalf("restore did not reach the controlled post-swap failure: %+v", j)
	}
	var pending restoreRecovery
	if err := h.app.setting(t.Context(), "restore-pending:"+current.ID, &pending); err != nil {
		t.Fatal(err)
	}
	if pending.BackupID != id || !pending.Restore.Installed || pending.PreRestoreBackupID == "" {
		t.Fatalf("pending restore lost its recovery identity: %+v", pending)
	}
	return h, pending
}

func runRestoreReconciliation(t *testing.T, h *harness, previous, confirm bool) jobs.Job {
	t.Helper()
	queued, err := h.app.Jobs.Enqueue(t.Context(), h.instance.ID, "restore_reconcile", map[string]bool{"use_previous": previous, "confirm": confirm}, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	return h.wait(t, queued.ID)
}

func TestRestoreGuardFailsClosedForIncompleteRecords(t *testing.T) {
	h := newHarness(t, nil)
	for _, record := range []any{nil, map[string]string{}, map[string]string{"backup_id": ""}} {
		if err := h.app.saveSetting(t.Context(), "restore-pending:"+h.instance.ID, record); err != nil {
			t.Fatal(err)
		}
		if err := h.app.checkRestorePending(t.Context(), h.instance.ID); err == nil {
			t.Fatal("incomplete pending metadata bypassed the restore guard")
		}
	}
}

func TestInterruptedRestoreBlocksUnsafeStartAndVerifiesReconciliation(t *testing.T) {
	h, _ := interruptedRestore(t)
	base := "/instances/" + h.instance.ID
	if err := h.app.saveSetting(t.Context(), "backup-retention", retentionPolicy{Enabled: true, KeepLast: 1}); err != nil {
		t.Fatal(err)
	}
	retention, err := h.app.Jobs.Enqueue(t.Context(), h.instance.ID, "retention", map[string]any{}, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if j := h.wait(t, retention.ID); j.State != "failed" || !strings.Contains(j.Error, "interrupted restore") {
		t.Fatal("retention could delete a pending restore's snapshots:", j.Error)
	}
	for _, operation := range []struct {
		path string
		body any
	}{
		{base + "/actions/start", map[string]any{}},
		{base + "/files", map[string]string{"operation": "mkdir", "path": "unsafe"}},
		{base + "/backups", map[string]any{}},
	} {
		j := h.queue(t, operation.path, operation.body)
		if j.State != "failed" || !strings.Contains(j.Error, "interrupted restore") {
			t.Fatalf("pending restore did not guard %s: %+v", operation.path, j)
		}
	}
	if j := runRestoreReconciliation(t, h, false, false); j.State != "failed" {
		t.Fatal("reconciled a restore without explicit confirmation")
	}
	world := filepath.Join(h.instance.Root, "world", "level.dat")
	if err := os.WriteFile(world, []byte("unreviewed external changes"), 0660); err != nil {
		t.Fatal(err)
	}
	if j := runRestoreReconciliation(t, h, false, true); j.State != "failed" || !strings.Contains(j.Error, "does not exactly match") {
		t.Fatalf("reconciled unverified world bytes: %+v", j)
	}
	current, err := h.app.Instances.Get(t.Context(), h.instance.ID)
	if err != nil || current.MemoryMB != 2048 {
		t.Fatal("failed reconciliation changed runtime metadata", err)
	}
	if err := os.WriteFile(world, []byte("controlled-world-progress"), 0660); err != nil {
		t.Fatal(err)
	}
	h.app.Provision = func(context.Context, model.Instance, bool) error { return nil }
	mustSucceed(t, runRestoreReconciliation(t, h, false, true))
	current, err = h.app.Instances.Get(t.Context(), h.instance.ID)
	if err != nil || current.MemoryMB != 1024 {
		t.Fatal("verified reconciliation did not finish runtime metadata", err)
	}
	if err := h.app.checkRestorePending(t.Context(), h.instance.ID); err != nil {
		t.Fatal("successful reconciliation left the instance blocked:", err)
	}
	mustSucceed(t, h.queue(t, base+"/actions/start", map[string]any{}))
}

func TestRetentionKeepsUnresolvedRuntimeRecoverySnapshots(t *testing.T) {
	h := newHarness(t, nil)
	backupID(t, h)
	backupID(t, h)
	if err := h.app.saveSetting(t.Context(), "backup-retention", retentionPolicy{Enabled: true, KeepLast: 1}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"runtime-pending:", "runtime-world-review:"} {
		var value any = "required-snapshot"
		if key == "runtime-pending:" {
			value = runtimeRecovery{Directory: "retained-runtime"}
		}
		if err := h.app.saveSetting(t.Context(), key+h.instance.ID, value); err != nil {
			t.Fatal(err)
		}
		queued, err := h.app.Jobs.Enqueue(t.Context(), h.instance.ID, "retention", map[string]any{}, "", 0)
		if err != nil {
			t.Fatal(err)
		}
		j := h.wait(t, queued.ID)
		if j.State != "failed" || !strings.Contains(j.Error, "retention is paused") {
			t.Fatalf("retention ignored %s: %+v", key, j)
		}
		if _, err := h.app.DB.Exec("DELETE FROM settings WHERE key=?", key+h.instance.ID); err != nil {
			t.Fatal(err)
		}
	}
	storage, err := h.app.backupRoot(h.instance.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	all, err := backups.List(t.Context(), storage)
	if err != nil || len(all) != 2 {
		t.Fatal("retention deleted a recovery prerequisite", err)
	}
}

func TestExplicitPreviousTreeRecoveryReconcilesItsOwnRuntime(t *testing.T) {
	h, pending := interruptedRestore(t)
	parent, target, err := h.app.Instances.OpenParent(h.instance.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	if _, err := backups.Recover(t.Context(), parent, target, pending.Restore.Recovery); err != nil {
		t.Fatal(err)
	}
	configured := make(chan model.Instance, 1)
	h.app.Provision = func(_ context.Context, i model.Instance, _ bool) error { configured <- i; return nil }
	mustSucceed(t, runRestoreReconciliation(t, h, true, true))
	select {
	case actual := <-configured:
		if actual.MemoryMB != 2048 {
			t.Fatal("previous world was paired with the restored snapshot's runtime")
		}
	default:
		t.Fatal("reconciliation skipped the helper after an ambiguous prior acknowledgement")
	}
	world, err := os.ReadFile(filepath.Join(h.instance.Root, "world", "level.dat"))
	if err != nil || string(world) != "newer world before restore" {
		t.Fatal("reconciliation silently replaced the explicitly recovered world", err)
	}
}
