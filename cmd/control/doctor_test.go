package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gabrielpgava/newempire-server-control/internal/config"
)

func TestDoctorDoesNotInitializeAnEmptyDatabase(t *testing.T) {
	c := config.Default()
	c.DataDir = t.TempDir()
	path := filepath.Join(c.DataDir, "panel.db")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := doctor(c); err == nil {
		t.Fatal("doctor accepted uninitialized state")
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() != 0 {
		t.Fatal("doctor initialized or migrated the database", err)
	}
}
