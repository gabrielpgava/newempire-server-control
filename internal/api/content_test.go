package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gabrielpgava/newempire-server-control/internal/jobs"
	"github.com/gabrielpgava/newempire-server-control/internal/modrinth"
)

func installContent(t *testing.T, h *harness) {
	t.Helper()
	base := "/instances/" + h.instance.ID
	plan := decodeResponse[struct {
		Hash string `json:"plan_hash"`
	}](t, h.request(t, "POST", base+"/mods/plan", map[string]any{"projects": []string{"project1"}}, &h.session, true), 200)
	mustSucceed(t, h.queue(t, base+"/mods/apply", map[string]any{"projects": []string{"project1"}, "plan_hash": plan.Hash, "confirm": true}))
}

func TestPendingContentBlocksConflictingFileAndConfigurationJobs(t *testing.T) {
	h := newHarness(t, fixtureContent(t))
	installContent(t, h)
	base := "/instances/" + h.instance.ID
	for _, operation := range []struct {
		method, path string
		body         any
	}{
		{"POST", base + "/files", map[string]any{"operation": "mkdir", "path": "conflict"}},
		{"PUT", base + "/properties", map[string]any{"properties": map[string]string{"difficulty": "hard"}, "confirm": true}},
		{"POST", base + "/worlds", map[string]any{"operation": "create", "name": "conflict", "confirm": true}},
		{"POST", base + "/players", map[string]any{"action": "op_level", "player": "Tester", "value": "4"}},
		{"POST", base + "/imports/apply", map[string]any{"upload_id": "unused", "preview_hash": "unused", "confirm": true}},
		{"POST", base + "/mods/manage", map[string]any{"operation": "pin", "project_id": "project1", "confirm": true}},
	} {
		queued := decodeResponse[jobs.Job](t, h.request(t, operation.method, operation.path, operation.body, &h.session, true), 202)
		j := h.wait(t, queued.ID)
		if j.State != "failed" || !strings.Contains(j.Error, "content transaction") {
			t.Fatalf("%s was not blocked by pending content: %s %s", operation.path, j.State, j.Error)
		}
	}
	if _, err := os.Stat(filepath.Join(h.instance.Root, "conflict")); !os.IsNotExist(err) {
		t.Fatal("a blocked job changed the instance")
	}
	mustSucceed(t, h.queue(t, base+"/actions/start", map[string]any{}))
	mustSucceed(t, h.queue(t, base+"/actions/stop", map[string]any{}))
	mustSucceed(t, h.queue(t, base+"/files", map[string]any{"operation": "mkdir", "path": "after-readiness"}))
}

func TestAdoptionCannotDuplicateAnAlreadyManagedProject(t *testing.T) {
	h := newHarness(t, fixtureContent(t))
	installContent(t, h)
	base := "/instances/" + h.instance.ID
	mustSucceed(t, h.queue(t, base+"/actions/start", map[string]any{}))
	mustSucceed(t, h.queue(t, base+"/actions/stop", map[string]any{}))
	j := h.queue(t, base+"/mods/manage", map[string]any{"operation": "adopt", "project_id": "project1", "confirm": true})
	if j.State != "failed" || !strings.Contains(j.Error, "already recorded") {
		t.Fatalf("duplicate adoption: %s %s", j.State, j.Error)
	}
	root, err := h.app.Instances.OpenRoot(h.instance.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	lock, err := modrinth.LoadLock(root)
	if err != nil || len(lock.Files) != 1 {
		t.Fatalf("duplicate adoption corrupted the lock: %+v %v", lock, err)
	}
}

func TestAutomaticSafePreservesUnidentifiedModsAndDeduplicatesNotice(t *testing.T) {
	h := newHarness(t, fixtureContent(t))
	installContent(t, h)
	base := "/instances/" + h.instance.ID
	mustSucceed(t, h.queue(t, base+"/actions/start", map[string]any{}))
	mustSucceed(t, h.queue(t, base+"/actions/stop", map[string]any{}))
	root, err := h.app.Instances.OpenRoot(h.instance.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	lock, err := modrinth.LoadLock(root)
	if err != nil {
		t.Fatal(err)
	}
	lock.Policy.Pins["project1"] = "version2"
	if err := modrinth.SavePolicy(root, lock.Policy); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile("mods/unidentified.jar", []byte("external mod bytes"), 0660); err != nil {
		t.Fatal(err)
	}
	policy := defaultContentPolicy()
	policy.Mode = "automatic-safe"
	if err := h.app.saveSetting(t.Context(), "mods-policy:"+h.instance.ID, policy); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		queued, err := h.app.Jobs.Enqueue(t.Context(), h.instance.ID, "mod_check", map[string]any{}, "", 0)
		if err != nil {
			t.Fatal(err)
		}
		j := h.wait(t, queued.ID)
		mustSucceed(t, j)
		var result struct {
			Postponed bool   `json:"postponed"`
			Reason    string `json:"reason"`
		}
		if err := json.Unmarshal(j.Result, &result); err != nil || !result.Postponed || !strings.Contains(result.Reason, "unidentified") {
			t.Fatalf("unsafe automatic plan was not postponed: %s %v", j.Result, err)
		}
	}
	unchanged, err := root.ReadFile("mods/unidentified.jar")
	if err != nil || string(unchanged) != "external mod bytes" {
		t.Fatal("automatic maintenance changed unknown content")
	}
	lock, err = modrinth.LoadLock(root)
	if err != nil || len(lock.Files) != 1 || lock.Files[0].VersionID != "version1" {
		t.Fatal("automatic maintenance applied an unreviewed change")
	}
	var notices int
	if err := h.app.DB.QueryRow("SELECT count(*) FROM notifications WHERE message LIKE '%content_updates_available%'").Scan(&notices); err != nil || notices != 1 {
		t.Fatalf("available plan notices were not deduplicated: %d %v", notices, err)
	}
}
