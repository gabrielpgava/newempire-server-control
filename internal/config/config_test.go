package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRejectUnsafeOriginsAndOverlappingBackupRoots(t *testing.T) {
	for _, origin := range []string{"http://example.com", "https://owner:password@example.com", "https://example.com/api", "https://example.com?secret=x"} {
		path := filepath.Join(t.TempDir(), "config.json")
		b, _ := json.Marshal(map[string]string{"public_url": origin})
		if err := os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Errorf("accepted origin %q", origin)
		}
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	b, _ := json.Marshal(map[string]string{"instance_dir": filepath.Join(dir, "instances"), "backup_dir": filepath.Join(dir, "instances", "hidden-backups")})
	os.WriteFile(path, b, 0600)
	if _, err := Load(path); err == nil {
		t.Fatal("accepted backups inside instance directory")
	}
}
