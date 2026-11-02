package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gabrielpgava/newempire-server-control/internal/backups"
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
