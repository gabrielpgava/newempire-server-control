package api

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gabrielpgava/newempire-server-control/internal/backups"
	"github.com/gabrielpgava/newempire-server-control/internal/jobs"
	"github.com/gabrielpgava/newempire-server-control/internal/model"
)

func TestOfflineBackupRequiresStoppedRuntimeAndLeavesItStopped(t *testing.T) {
	h := newHarness(t, nil)
	for _, running := range []bool{true, false} {
		h.runtime.mu.Lock()
		h.runtime.ready = running
		h.runtime.mu.Unlock()
		queued, err := h.app.Jobs.Enqueue(t.Context(), h.instance.ID, "backup_offline", map[string]string{"note": "Offline recovery fixture"}, "", 0)
		if err != nil {
			t.Fatal(err)
		}
		j := h.wait(t, queued.ID)
		if running {
			if j.State != "failed" || !strings.Contains(j.Error, "stop Minecraft") {
				t.Fatalf("offline backup changed a running instance: %+v", j)
			}
			continue
		}
		mustSucceed(t, j)
		var result struct {
			Backup backups.Backup `json:"backup"`
		}
		if err := json.Unmarshal(j.Result, &result); err != nil {
			t.Fatal(err)
		}
		storage, err := h.app.backupRoot(h.instance.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer storage.Close()
		if _, err := backups.Verify(t.Context(), storage, result.Backup.ID, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := h.app.BackupInstance(t.Context(), storage, result.Backup.ID, h.instance); err != nil {
			t.Fatal(err)
		}
		status, err := h.runtime.Status(t.Context(), h.instance)
		if err != nil || status.Ready {
			t.Fatal("offline backup started Minecraft", err)
		}
	}
}

func TestRuntimeRecoveryVerifiesRetainedFilesAndNeverRestoresWorlds(t *testing.T) {
	h := newHarness(t, nil)
	launcher := filepath.Join(h.instance.Root, h.instance.Jar)
	if err := os.WriteFile(launcher, []byte("original-launcher"), 0660); err != nil {
		t.Fatal(err)
	}
	backup := backupID(t, h)
	directory := "runtime-recovery-" + model.NewID()
	retained := filepath.Join(h.app.Config.DataDir, "staging", directory)
	if err := os.Mkdir(retained, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(retained, "launcher.jar"), []byte("tampered-launcher"), 0600); err != nil {
		t.Fatal(err)
	}
	props, err := os.ReadFile(filepath.Join(h.instance.Root, "server.properties"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(retained, "server.properties"), props, 0600); err != nil {
		t.Fatal(err)
	}
	current := h.instance
	current.MinecraftVersion = "migrated-fixture"
	current.MemoryMB = 2048
	if err := h.app.Instances.Update(t.Context(), current); err != nil {
		t.Fatal(err)
	}
	world := filepath.Join(current.Root, "world", "level.dat")
	if err := os.WriteFile(world, []byte("valuable-post-migration-world"), 0660); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(launcher, []byte("new-launcher"), 0660); err != nil {
		t.Fatal(err)
	}
	recovery := runtimeRecovery{Directory: directory, Previous: h.instance, BackupID: backup, State: "applied"}
	if err := h.app.saveSetting(t.Context(), "runtime-pending:"+current.ID, recovery); err != nil {
		t.Fatal(err)
	}
	h.app.Jobs.Register("test_runtime_recovery", 0, func(ctx context.Context, task *jobs.Task, _ json.RawMessage) (any, error) {
		return h.app.RecoverRuntime(ctx, task, task.Job.InstanceID)
	})
	run := func() jobs.Job {
		queued, err := h.app.Jobs.Enqueue(t.Context(), current.ID, "test_runtime_recovery", map[string]bool{"confirm": true}, "", 0)
		if err != nil {
			t.Fatal(err)
		}
		return h.wait(t, queued.ID)
	}
	if j := run(); j.State != "failed" || !strings.Contains(j.Error, "differs from its verified") {
		t.Fatalf("tampered runtime accepted: %+v", j)
	}
	if actual, err := os.ReadFile(launcher); err != nil || string(actual) != "new-launcher" {
		t.Fatal("failed verification changed runtime files", err)
	}
	if err := os.WriteFile(filepath.Join(retained, "launcher.jar"), []byte("original-launcher"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.app.DB.Exec("CREATE TRIGGER retain_runtime_guard BEFORE DELETE ON settings WHEN OLD.key='runtime-pending:" + current.ID + "' BEGIN SELECT RAISE(ABORT,'controlled runtime completion failure'); END"); err != nil {
		t.Fatal(err)
	}
	if j := run(); j.State != "failed" || !strings.Contains(j.Error, "controlled runtime completion") {
		t.Fatalf("completion failure was not reported: %+v", j)
	}
	registered, err := h.app.Instances.Get(t.Context(), current.ID)
	if err != nil || registered.MinecraftVersion != h.instance.MinecraftVersion {
		t.Fatal("fixture did not reach metadata completion", err)
	}
	if err := h.app.prepareRuntimeStart(t.Context(), registered); err == nil || !strings.Contains(err.Error(), "upgraded world") {
		t.Fatal("partial completion lost the world-format startup guard", err)
	}
	if _, err := h.app.DB.Exec("DROP TRIGGER retain_runtime_guard"); err != nil {
		t.Fatal(err)
	}
	j := run()
	mustSucceed(t, j)
	var result struct {
		Inspection string `json:"inspection_backup_id"`
	}
	if err := json.Unmarshal(j.Result, &result); err != nil || !model.ValidID(result.Inspection) {
		t.Fatal("displaced runtime was not retained in a verified inspection archive", err)
	}
	if _, err := os.Stat(filepath.Join(h.app.Config.BackupDir, current.ID, result.Inspection+".instance.json")); !os.IsNotExist(err) {
		t.Fatal("mixed runtime inspection was advertised as a portable snapshot")
	}
	if actual, err := os.ReadFile(world); err != nil || string(actual) != "valuable-post-migration-world" {
		t.Fatal("runtime recovery replaced world progress", err)
	}
	if actual, err := os.ReadFile(launcher); err != nil || string(actual) != "original-launcher" {
		t.Fatal("verified original runtime was not recovered", err)
	}
	if err := h.app.prepareRuntimeStart(t.Context(), registered); err == nil {
		t.Fatal("old runtime was allowed to start against a potentially upgraded world")
	}
}
