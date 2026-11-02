package api

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gabrielpgava/newempire-server-control/internal/backups"
	"github.com/gabrielpgava/newempire-server-control/internal/model"
)

func backupID(t *testing.T, h *harness) string {
	t.Helper()
	j := h.queue(t, "/instances/"+h.instance.ID+"/backups", map[string]any{})
	mustSucceed(t, j)
	var result struct {
		Backup struct{ ID string } `json:"backup"`
	}
	if err := json.Unmarshal(j.Result, &result); err != nil || result.Backup.ID == "" {
		t.Fatalf("backup job result: %s %v", j.Result, err)
	}
	return result.Backup.ID
}

func TestRestoreMetadataPreflightLeavesRunningServerUntouched(t *testing.T) {
	h := newHarness(t, nil)
	id := backupID(t, h)
	for _, invalid := range []string{"missing", "malformed", "identity", "ports", "ownership", "java", "memory", "launcher"} {
		t.Run(invalid, func(t *testing.T) {
			metadataPath := filepath.Join(h.app.Config.BackupDir, h.instance.ID, id+".instance.json")
			metadata := h.instance
			switch invalid {
			case "missing":
				if err := os.Remove(metadataPath); err != nil {
					t.Fatal(err)
				}
			case "malformed":
				if err := os.WriteFile(metadataPath, []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			case "identity":
				metadata.Root += "-other"
			case "ports":
				metadata.Port++
			case "ownership":
				metadata.Adopted = !metadata.Adopted
			case "java":
				metadata.JavaPath = "/unapproved/java"
			case "memory":
				metadata.MemoryMB = 0
			case "launcher":
				metadata.Jar = "../unapproved.jar"
			}
			if invalid != "missing" && invalid != "malformed" {
				raw, err := json.Marshal(metadata)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(metadataPath, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			worldPath := filepath.Join(h.instance.Root, "world", "level.dat")
			if err := os.WriteFile(worldPath, []byte("valuable newer world"), 0660); err != nil {
				t.Fatal(err)
			}
			h.runtime.mu.Lock()
			h.runtime.ready = true
			h.runtime.mu.Unlock()
			base := "/instances/" + h.instance.ID + "/backups/" + id
			if r := h.request(t, "GET", base+"/preview", nil, &h.session, false); r.Code != 400 {
				t.Fatalf("invalid metadata preview returned %d: %s", r.Code, r.Body.String())
			}
			j := h.queue(t, base+"/restore", map[string]bool{"confirm": true})
			if j.State != "failed" {
				t.Fatal("restore accepted invalid portable metadata")
			}
			world, err := os.ReadFile(worldPath)
			if err != nil || string(world) != "valuable newer world" {
				t.Fatalf("preflight changed world data: %q %v", world, err)
			}
			status, err := h.runtime.Status(t.Context(), h.instance)
			if err != nil || !status.Ready {
				t.Fatal("preflight stopped Minecraft")
			}
			root, err := h.app.backupRoot(h.instance.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			all, err := backups.List(t.Context(), root)
			if err != nil || len(all) != 1 {
				t.Fatalf("invalid restore did work before preflight: %d backups, %v", len(all), err)
			}
		})
	}
}

func TestRestoreReconfiguresArchivedMemoryLimit(t *testing.T) {
	h := newHarness(t, nil)
	id := backupID(t, h)
	current := h.instance
	current.MemoryMB = 2048
	if err := h.app.Instances.Update(t.Context(), current); err != nil {
		t.Fatal(err)
	}
	configured := make(chan model.Instance, 1)
	h.app.Provision = func(_ context.Context, i model.Instance, reconfigure bool) error {
		if !reconfigure {
			t.Error("restore attempted to create a new service")
		}
		configured <- i
		return nil
	}
	mustSucceed(t, h.queue(t, "/instances/"+h.instance.ID+"/backups/"+id+"/restore", map[string]bool{"confirm": true}))
	select {
	case restored := <-configured:
		if restored.MemoryMB != h.instance.MemoryMB || restored.RCONSecret != h.instance.RCONSecret {
			t.Fatalf("wrong restored service configuration: %+v", restored)
		}
	default:
		t.Fatal("archived memory limit did not reach the restricted service helper")
	}
}
