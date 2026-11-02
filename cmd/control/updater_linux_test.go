package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/gabrielpgava/newempire-server-control/internal/config"
	"github.com/gabrielpgava/newempire-server-control/internal/database"
	"github.com/gabrielpgava/newempire-server-control/internal/updates"
	"golang.org/x/sys/unix"
)

func updaterMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func panelDataFixture(t *testing.T) config.Config {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("two-UID updater fixture requires isolated root")
	}
	c := config.Default()
	c.DataDir = t.TempDir()
	updaterMust(t, os.Chown(c.DataDir, 1000, 1000))
	c.SourcePath = filepath.Join(t.TempDir(), "config.json")
	updaterMust(t, os.WriteFile(c.SourcePath, []byte(`{"listen":"127.0.0.1:8080"}`), 0640))
	return c
}

func TestPanelSnapshotLockNeverCreatesRootOwnedStartupBlocker(t *testing.T) {
	c := panelDataFixture(t)
	dest := t.TempDir()
	for _, operation := range []func(context.Context, config.Config, string) error{snapshotPanel, restorePanel} {
		if err := operation(context.Background(), c, dest); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("missing existing lock was not rejected", err)
		}
		if _, err := os.Lstat(filepath.Join(c.DataDir, "panel.lock")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("root updater created a lock", err)
		}
	}
	for p := filepath.Dir(c.DataDir); p != filepath.Clean(os.TempDir()); p = filepath.Dir(p) {
		updaterMust(t, os.Chmod(p, 0755))
	}
	command := exec.Command("/usr/bin/touch", filepath.Join(c.DataDir, "panel.lock"))
	command.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + c.DataDir, "TMPDIR=" + os.TempDir()}
	command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 1000, Gid: 1000}}
	updaterMust(t, command.Run())
	root, err := os.OpenRoot(c.DataDir)
	updaterMust(t, err)
	defer root.Close()
	release, err := lockPanelData(root)
	updaterMust(t, err)
	if _, err := lockPanelData(root); err == nil {
		t.Fatal("updater ignored the held data lock")
	}
	release()
	info, err := root.Stat("panel.lock")
	updaterMust(t, err)
	if info.Sys().(*syscall.Stat_t).Uid != 1000 {
		t.Fatal("root updater changed ownership of the panel's lock")
	}
}

func TestPanelSnapshotLockRejectsLinksSpecialFilesAndWrongOwner(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "fifo", "wrong owner"} {
		t.Run(kind, func(t *testing.T) {
			c := panelDataFixture(t)
			path := filepath.Join(c.DataDir, "panel.lock")
			switch kind {
			case "symlink":
				updaterMust(t, os.WriteFile(filepath.Join(c.DataDir, "other"), nil, 0600))
				updaterMust(t, os.Symlink("other", path))
			case "fifo":
				updaterMust(t, unix.Mkfifo(path, 0600))
			default:
				updaterMust(t, os.WriteFile(path, nil, 0600))
				if kind == "hardlink" {
					updaterMust(t, os.Chown(path, 1000, 1000))
					updaterMust(t, os.Link(path, path+".linked"))
				}
			}
			root, err := os.OpenRoot(c.DataDir)
			updaterMust(t, err)
			defer root.Close()
			if release, err := lockPanelData(root); err == nil {
				release()
				t.Fatal("unsafe lock was accepted")
			}
		})
	}
}

func TestPanelSnapshotRestoreKeepsUnprivilegedFileOwners(t *testing.T) {
	c := panelDataFixture(t)
	dbPath := filepath.Join(c.DataDir, "panel.db")
	db, err := database.Open(dbPath)
	updaterMust(t, err)
	_, err = db.Exec(`INSERT INTO settings(key,value) VALUES('fixture','"original"')`)
	updaterMust(t, err)
	updaterMust(t, db.Close())
	for name, value := range map[string]string{"master.key": "dummy encryption fixture", "health-token": strings.Repeat("a", 43), "panel.lock": ""} {
		updaterMust(t, os.WriteFile(filepath.Join(c.DataDir, name), []byte(value), 0600))
	}
	for _, name := range []string{"panel.db", "master.key", "health-token", "panel.lock"} {
		updaterMust(t, os.Chown(filepath.Join(c.DataDir, name), 1000, 1000))
	}
	dest := t.TempDir()
	updaterMust(t, snapshotPanel(context.Background(), c, dest))
	_, err = os.Stat(filepath.Join(dest, "panel.db"))
	updaterMust(t, err)
	db, err = database.Open(dbPath)
	updaterMust(t, err)
	_, err = db.Exec(`UPDATE settings SET value='"changed"' WHERE key='fixture'`)
	updaterMust(t, err)
	updaterMust(t, db.Close())
	updaterMust(t, os.WriteFile(filepath.Join(c.DataDir, "master.key"), []byte("changed fixture"), 0600))
	updaterMust(t, os.WriteFile(c.SourcePath, []byte(`{"listen":"127.0.0.1:9090"}`), 0640))
	updaterMust(t, restorePanel(context.Background(), c, dest))
	for _, name := range []string{"panel.db", "master.key", "health-token", "panel.lock"} {
		info, err := os.Stat(filepath.Join(c.DataDir, name))
		updaterMust(t, err)
		if info.Sys().(*syscall.Stat_t).Uid != 1000 || info.Mode().Perm() != 0600 {
			t.Fatal("restored panel file lost its owner or permissions", name, info)
		}
	}
	db, err = database.Open(dbPath)
	updaterMust(t, err)
	defer db.Close()
	var value string
	updaterMust(t, db.QueryRow("SELECT value FROM settings WHERE key='fixture'").Scan(&value))
	if value != `"original"` {
		t.Fatal("snapshot did not restore panel state", value)
	}
	configBytes, err := os.ReadFile(c.SourcePath)
	updaterMust(t, err)
	if string(configBytes) != `{"listen":"127.0.0.1:8080"}` {
		t.Fatal("root configuration was not restored")
	}
}

func TestIndependentStageReusesMatchingTrustedTransactionWithoutNetwork(t *testing.T) {
	dir := t.TempDir()
	updaterMust(t, os.Mkdir(filepath.Join(dir, "updates"), 0700))
	tx := &updates.Transaction{ID: strings.Repeat("a", 32), Version: "1.1.0", PreviousVersion: "1.0.0", Schema: database.SchemaVersion, Channel: updates.Stable, State: updates.Staged, BinarySHA256: strings.Repeat("b", 64)}
	b, err := json.Marshal(updates.Journal{Transaction: tx, History: []updates.Event{}})
	updaterMust(t, err)
	updaterMust(t, os.WriteFile(filepath.Join(dir, "updates", "journal.json"), b, 0600))
	m, err := updates.New(updates.Options{InstallDir: dir, CurrentVersion: "1.0.0", Schema: database.SchemaVersion, PublicKey: strings.Repeat("A", 43) + "="})
	updaterMust(t, err)
	for range 2 {
		got, err := stagePanelUpdate(context.Background(), m, "1.0.0", "1.1.0", updates.Stable)
		updaterMust(t, err)
		if got.ID != tx.ID || got.State != updates.Staged {
			t.Fatal("matching stage was replaced", got)
		}
	}
}
