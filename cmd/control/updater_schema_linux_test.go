package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gabrielpgava/newempire-server-control/internal/config"
	"github.com/gabrielpgava/newempire-server-control/internal/database"
	"github.com/gabrielpgava/newempire-server-control/internal/updates"
	"golang.org/x/sys/unix"
)

func schemaDirectory(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	updaterMust(t, os.Chmod(dir, 0700))
	return dir
}

func schemaPanel(t *testing.T) config.Config {
	t.Helper()
	c := panelDataFixture(t)
	for name, value := range map[string]string{"panel.lock": "", "master.key": "dummy fixture key", "health-token": strings.Repeat("a", 43)} {
		path := filepath.Join(c.DataDir, name)
		updaterMust(t, os.WriteFile(path, []byte(value), 0600))
		updaterMust(t, os.Chown(path, 1000, 1000))
	}
	return c
}

// A quiescent raw copy models committed WAL left after a crash, without opening live state as root.
func schemaDatabase(t *testing.T, dir string, version int, wal bool, uid int) {
	t.Helper()
	build := schemaDirectory(t)
	u := &url.URL{Scheme: "file", Path: filepath.Join(build, "panel.db")}
	db, err := sql.Open("sqlite", u.String()+"?_pragma=journal_mode(WAL)&_pragma=wal_autocheckpoint(0)")
	updaterMust(t, err)
	defer db.Close()
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`CREATE TABLE schema_probe(value TEXT); INSERT INTO schema_probe VALUES('base')`)
	updaterMust(t, err)
	_, err = db.Exec(fmt.Sprintf("PRAGMA user_version=%d", database.SchemaVersion))
	updaterMust(t, err)
	_, err = db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	updaterMust(t, err)
	if !wal {
		_, err = db.Exec("PRAGMA journal_mode=DELETE")
		updaterMust(t, err)
	}
	_, err = db.Exec(fmt.Sprintf("PRAGMA user_version=%d", version))
	updaterMust(t, err)
	if wal {
		_, err = db.Exec(`UPDATE schema_probe SET value='committed WAL'`)
		updaterMust(t, err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		data, err := os.ReadFile(filepath.Join(build, "panel.db"+suffix))
		if suffix != "" && errors.Is(err, os.ErrNotExist) {
			continue
		}
		updaterMust(t, err)
		path := filepath.Join(dir, "panel.db"+suffix)
		updaterMust(t, os.WriteFile(path, data, 0600))
		updaterMust(t, os.Chown(path, uid, uid))
		if suffix == "" && wal && int(binary.BigEndian.Uint32(data[60:64])) != database.SchemaVersion {
			t.Fatal("WAL fixture changed the main-file schema instead of the committed WAL")
		}
	}
}

type schemaFileState struct {
	Mode     os.FileMode
	Size     int64
	Modified time.Time
	UID, GID uint32
	Links    uint64
	Data     string
}

func schemaTree(t *testing.T, dir string) map[string]schemaFileState {
	t.Helper()
	entries, err := os.ReadDir(dir)
	updaterMust(t, err)
	state := map[string]schemaFileState{}
	for _, entry := range entries {
		info, err := entry.Info()
		updaterMust(t, err)
		stat := info.Sys().(*syscall.Stat_t)
		file := schemaFileState{Mode: info.Mode(), Size: info.Size(), Modified: info.ModTime(), UID: stat.Uid, GID: stat.Gid, Links: uint64(stat.Nlink)}
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			updaterMust(t, err)
			file.Data = string(data)
		} else if info.Mode()&os.ModeSymlink != 0 {
			file.Data, err = os.Readlink(filepath.Join(dir, entry.Name()))
			updaterMust(t, err)
		}
		state[entry.Name()] = file
	}
	return state
}

func assertSchemaTree(t *testing.T, dir string, before map[string]schemaFileState) {
	t.Helper()
	if !reflect.DeepEqual(before, schemaTree(t, dir)) {
		t.Fatal("schema preflight mutated original files, sidecars, ownership, permissions or timestamps", dir)
	}
}

func TestPanelSnapshotChecksActualFileAndWALSchemaWithoutMigrations(t *testing.T) {
	for _, wal := range []bool{false, true} {
		for _, version := range []int{database.SchemaVersion - 1, database.SchemaVersion, database.SchemaVersion + 1} {
			t.Run(fmt.Sprintf("schema=%d/wal=%t", version, wal), func(t *testing.T) {
				c := schemaPanel(t)
				schemaDatabase(t, c.DataDir, version, wal, 1000)
				before := schemaTree(t, c.DataDir)
				dest := schemaDirectory(t)
				err := snapshotPanel(t.Context(), c, dest)
				assertSchemaTree(t, c.DataDir, before)
				if version != database.SchemaVersion {
					if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("existing database schema is %d", version)) {
						t.Fatal("unsupported actual schema was migrated or not checked", err)
					}
					for _, suffix := range []string{"", "-wal"} {
						if old, ok := before["panel.db"+suffix]; ok {
							data, err := os.ReadFile(filepath.Join(dest, "source.db"+suffix))
							updaterMust(t, err)
							if string(data) != old.Data {
								t.Fatal("schema rejection mutated the protected copy")
							}
						}
					}
					if _, err := os.Lstat(filepath.Join(dest, "panel.db")); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("schema rejection produced an installable recovery database", err)
					}
					return
				}
				updaterMust(t, err)
				db, err := database.OpenExisting(filepath.Join(dest, "panel.db"))
				updaterMust(t, err)
				defer db.Close()
				var value string
				updaterMust(t, db.QueryRow("SELECT value FROM schema_probe").Scan(&value))
				want := "base"
				if wal {
					want = "committed WAL"
				}
				if value != want {
					t.Fatal("snapshot lost committed database content", value)
				}
			})
		}
	}
}

func TestPanelRestartRejectsUnsafeActualSchemaAndReleasesLockBeforeStart(t *testing.T) {
	for _, kind := range []string{"older", "newer", "older WAL", "newer WAL", "empty", "missing", "matching", "matching WAL", "locked", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			c := schemaPanel(t)
			version := database.SchemaVersion
			if strings.HasPrefix(kind, "older") {
				version--
			} else if strings.HasPrefix(kind, "newer") {
				version++
			}
			if kind != "missing" {
				schemaDatabase(t, c.DataDir, version, strings.Contains(kind, "WAL"), 1000)
			}
			if kind == "empty" {
				updaterMust(t, os.WriteFile(filepath.Join(c.DataDir, "panel.db"), nil, 0600))
			}
			root, err := os.OpenRoot(c.DataDir)
			updaterMust(t, err)
			defer root.Close()
			if kind == "locked" {
				release, err := lockPanelData(root)
				updaterMust(t, err)
				defer release()
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if kind == "cancelled" {
				cancel()
			}
			before := schemaTree(t, c.DataDir)
			started := 0
			startErr := errors.New("fixture start error")
			err = restartCheckedPanel(ctx, c, func() error {
				started++
				release, err := lockPanelData(root)
				updaterMust(t, err)
				release()
				return startErr
			})
			if strings.HasPrefix(kind, "matching") {
				if started != 1 || !errors.Is(err, startErr) {
					t.Fatal("matching schema did not reach start or lost its error", err, started)
				}
			} else if started != 0 || err == nil || !strings.Contains(err.Error(), "manual recovery") {
				t.Fatal("unsafe state reached panel startup", err, started)
			}
			if kind == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatal("schema guard lost cancellation", err)
			}
			if kind == "cancelled" && strings.Contains(err.Error(), "source changed") {
				t.Fatal("cancellation was misreported as a source mutation", err)
			}
			assertSchemaTree(t, c.DataDir, before)
			entries, err := os.ReadDir(filepath.Join(c.InstallDir, "updates"))
			updaterMust(t, err)
			if len(entries) != 0 {
				t.Fatal("schema check retained temporary credential-bearing database copies", entries)
			}
		})
	}
}

func TestPanelSnapshotRejectsMissingAndEmptyDatabaseWithoutInitialization(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing=%t", missing), func(t *testing.T) {
			c := schemaPanel(t)
			if !missing {
				path := filepath.Join(c.DataDir, "panel.db")
				updaterMust(t, os.WriteFile(path, nil, 0600))
				updaterMust(t, os.Chown(path, 1000, 1000))
			}
			before := schemaTree(t, c.DataDir)
			dest := schemaDirectory(t)
			if err := snapshotPanel(t.Context(), c, dest); err == nil {
				t.Fatal("missing or empty state was initialized")
			}
			assertSchemaTree(t, c.DataDir, before)
			entries, err := os.ReadDir(dest)
			updaterMust(t, err)
			if len(entries) != 0 {
				t.Fatal("snapshot initialized a protected database copy", entries)
			}
		})
	}
}

func TestPanelSchemaCopyRequiresPrivateRootOwnedDestination(t *testing.T) {
	for _, kind := range []string{"owner", "permissions"} {
		t.Run(kind, func(t *testing.T) {
			c := schemaPanel(t)
			schemaDatabase(t, c.DataDir, database.SchemaVersion, false, 1000)
			dest := schemaDirectory(t)
			if kind == "owner" {
				updaterMust(t, os.Chown(dest, 1000, 1000))
			} else {
				updaterMust(t, os.Chmod(dest, 0755))
			}
			before := schemaTree(t, c.DataDir)
			if err := snapshotPanel(t.Context(), c, dest); err == nil || !strings.Contains(err.Error(), "private root-owned") {
				t.Fatal("unsafe copy destination was trusted", err)
			}
			assertSchemaTree(t, c.DataDir, before)
			entries, err := os.ReadDir(dest)
			updaterMust(t, err)
			if len(entries) != 0 {
				t.Fatal("database was copied into unsafe storage")
			}
		})
	}
}

func TestPanelSchemaCopyRejectsUnsafeDatabaseAndSidecars(t *testing.T) {
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		for _, kind := range []string{"symlink", "hardlink", "fifo", "wrong owner", "permissions"} {
			t.Run(suffix+"/"+kind, func(t *testing.T) {
				c := schemaPanel(t)
				schemaDatabase(t, c.DataDir, database.SchemaVersion, false, 1000)
				path := filepath.Join(c.DataDir, "panel.db"+suffix)
				if suffix != "" {
					updaterMust(t, os.WriteFile(path, []byte("unused sidecar fixture"), 0600))
					updaterMust(t, os.Chown(path, 1000, 1000))
				}
				switch kind {
				case "symlink":
					updaterMust(t, os.Rename(path, path+".target"))
					updaterMust(t, os.Symlink(filepath.Base(path)+".target", path))
				case "hardlink":
					updaterMust(t, os.Link(path, path+".alias"))
				case "fifo":
					updaterMust(t, os.Remove(path))
					updaterMust(t, unix.Mkfifo(path, 0600))
					updaterMust(t, os.Chown(path, 1000, 1000))
				case "wrong owner":
					updaterMust(t, os.Chown(path, 0, 0))
				case "permissions":
					updaterMust(t, os.Chmod(path, 0644))
				}
				before := schemaTree(t, c.DataDir)
				if err := snapshotPanel(t.Context(), c, schemaDirectory(t)); err == nil {
					t.Fatal("unsafe database or sidecar was accepted")
				}
				assertSchemaTree(t, c.DataDir, before)
			})
		}
	}
}

type schemaChangingContext struct {
	context.Context
	polls  int
	change func()
}

func (c *schemaChangingContext) Err() error {
	c.polls++
	// files.Copy checks cancellation again after reading, before writing that chunk.
	if c.polls == 2 {
		c.change()
	}
	return c.Context.Err()
}

func TestPanelSchemaCopyRejectsSourceReplacementDuringCopy(t *testing.T) {
	c := schemaPanel(t)
	schemaDatabase(t, c.DataDir, database.SchemaVersion, false, 1000)
	data, err := os.OpenRoot(c.DataDir)
	updaterMust(t, err)
	defer data.Close()
	release, err := lockPanelData(data)
	updaterMust(t, err)
	defer release()
	dest, err := os.OpenRoot(schemaDirectory(t))
	updaterMust(t, err)
	defer dest.Close()
	ctx := &schemaChangingContext{Context: t.Context(), change: func() {
		path := filepath.Join(c.DataDir, "panel.db")
		updaterMust(t, os.Rename(path, path+".preserved"))
		updaterMust(t, os.WriteFile(path, []byte("replacement fixture"), 0600))
		updaterMust(t, os.Chown(path, 1000, 1000))
	}}
	if db, err := copyPanelDatabase(ctx, data, dest); err == nil {
		db.Close()
		t.Fatal("changed source reached SQLite schema validation")
	} else if !strings.Contains(err.Error(), "source changed") {
		t.Fatal("lost the source replacement diagnostic", err)
	}
}

func TestPanelRestoreChecksInstalledAndSnapshotActualSchemaBeforeReplacingFiles(t *testing.T) {
	for _, target := range []string{"installed", "snapshot"} {
		for _, kind := range []string{"older", "newer", "older WAL", "newer WAL", "empty", "missing"} {
			t.Run(target+"/"+kind, func(t *testing.T) {
				c := schemaPanel(t)
				schemaDatabase(t, c.DataDir, database.SchemaVersion, false, 1000)
				snapshot := schemaDirectory(t)
				updaterMust(t, snapshotPanel(t.Context(), c, snapshot))
				dir, uid := c.DataDir, 1000
				if target == "snapshot" {
					dir, uid = snapshot, 0
				}
				version := database.SchemaVersion - 1
				if strings.HasPrefix(kind, "newer") {
					version = database.SchemaVersion + 1
				}
				schemaDatabase(t, dir, version, strings.Contains(kind, "WAL"), uid)
				if kind == "missing" {
					updaterMust(t, os.Remove(filepath.Join(dir, "panel.db")))
				} else if kind == "empty" {
					updaterMust(t, os.WriteFile(filepath.Join(dir, "panel.db"), nil, 0600))
				}
				beforeData, beforeSnapshot := schemaTree(t, c.DataDir), schemaTree(t, snapshot)
				configBefore, err := os.ReadFile(c.SourcePath)
				updaterMust(t, err)
				err = restorePanel(t.Context(), c, snapshot)
				if err == nil || !strings.Contains(err.Error(), target+" panel") && !strings.Contains(err.Error(), "panel snapshot") {
					t.Fatal("restore did not reject actual schema before replacing state", err)
				}
				assertSchemaTree(t, c.DataDir, beforeData)
				assertSchemaTree(t, snapshot, beforeSnapshot)
				configAfter, err := os.ReadFile(c.SourcePath)
				updaterMust(t, err)
				if !bytes.Equal(configBefore, configAfter) {
					t.Fatal("schema rejection changed configuration")
				}
			})
		}
	}
}

func TestPanelRestoreAcceptsMatchingInstalledWALButRequiresStandaloneSnapshot(t *testing.T) {
	for _, snapshotWAL := range []bool{false, true} {
		t.Run(fmt.Sprintf("snapshot-wal=%t", snapshotWAL), func(t *testing.T) {
			c := schemaPanel(t)
			schemaDatabase(t, c.DataDir, database.SchemaVersion, false, 1000)
			snapshot := schemaDirectory(t)
			updaterMust(t, snapshotPanel(t.Context(), c, snapshot))
			schemaDatabase(t, c.DataDir, database.SchemaVersion, true, 1000)
			if snapshotWAL {
				schemaDatabase(t, snapshot, database.SchemaVersion, true, 0)
			}
			before, saved := schemaTree(t, c.DataDir), schemaTree(t, snapshot)
			err := restorePanel(t.Context(), c, snapshot)
			assertSchemaTree(t, snapshot, saved)
			if snapshotWAL {
				if err == nil || !strings.Contains(err.Error(), "standalone database") {
					t.Fatal("restore would discard required snapshot WAL", err)
				}
				assertSchemaTree(t, c.DataDir, before)
				return
			}
			updaterMust(t, err)
			for _, suffix := range []string{"-wal", "-shm", "-journal"} {
				if _, err := os.Lstat(filepath.Join(c.DataDir, "panel.db"+suffix)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("restore retained stale SQLite sidecars", err)
				}
			}
			if data, err := os.ReadFile(filepath.Join(c.DataDir, "panel.db")); err != nil || string(data) != saved["panel.db"].Data {
				t.Fatal("restore did not select the verified standalone snapshot", err)
			}
		})
	}
}

type schemaRecoveryService struct {
	c        config.Config
	restarts int
}

func (s *schemaRecoveryService) StopPanel(context.Context) error { return nil }
func (s *schemaRecoveryService) Health(context.Context) error    { return nil }
func (s *schemaRecoveryService) RestartPanel(ctx context.Context) error {
	return restartCheckedPanel(ctx, s.c, func() error { s.restarts++; return nil })
}

func TestPanelSchemaFailureLeavesPreSwitchRecoveryStopped(t *testing.T) {
	for _, state := range []updates.State{updates.Stopping, updates.Snapshotting, updates.ManualRecovery} {
		for _, kind := range []string{"older", "newer", "older WAL", "newer WAL", "empty", "missing", "matching"} {
			t.Run(string(state)+"/"+kind, func(t *testing.T) {
				c := schemaPanel(t)
				version := database.SchemaVersion
				if strings.HasPrefix(kind, "older") {
					version--
				} else if strings.HasPrefix(kind, "newer") {
					version++
				}
				if kind != "missing" {
					schemaDatabase(t, c.DataDir, version, strings.Contains(kind, "WAL"), 1000)
				}
				if kind == "empty" {
					updaterMust(t, os.WriteFile(filepath.Join(c.DataDir, "panel.db"), nil, 0600))
				}
				updaterMust(t, os.MkdirAll(filepath.Join(c.InstallDir, "versions", "1.0.0"), 0755))
				binaryPath := filepath.Join(c.InstallDir, "versions", "1.0.0", updates.BinaryName)
				updaterMust(t, os.WriteFile(binaryPath, []byte("preserved panel fixture"), 0555))
				updaterMust(t, os.Symlink("versions/1.0.0", filepath.Join(c.InstallDir, "current")))
				tx := &updates.Transaction{ID: strings.Repeat("a", 32), Version: "1.1.0", PreviousVersion: "1.0.0", Schema: database.SchemaVersion, Channel: updates.Stable, State: state, BinarySHA256: strings.Repeat("b", 64), PreviousSHA256: strings.Repeat("c", 64)}
				journal, err := json.Marshal(updates.Journal{Transaction: tx, History: []updates.Event{}})
				updaterMust(t, err)
				updaterMust(t, os.WriteFile(filepath.Join(c.InstallDir, "updates", "journal.json"), journal, 0600))
				service := &schemaRecoveryService{c: c}
				manager, err := updates.New(updates.Options{InstallDir: c.InstallDir, CurrentVersion: "1.0.0", Schema: database.SchemaVersion, PublicKey: strings.Repeat("A", 43) + "=", Service: service, Snapshot: func(ctx context.Context, dir string) error { return snapshotPanel(ctx, c, dir) }, Restore: func(ctx context.Context, dir string) error { return restorePanel(ctx, c, dir) }})
				updaterMust(t, err)
				before := schemaTree(t, c.DataDir)
				err = manager.Recover(t.Context())
				want := updates.ManualRecovery
				if kind == "matching" {
					updaterMust(t, err)
					want = updates.Failed
					if service.restarts != 1 {
						t.Fatal("matching pre-switch recovery did not restart")
					}
				} else if err == nil || !errors.Is(err, updates.ErrRecovery) || !strings.Contains(err.Error(), "manual recovery") || service.restarts != 0 {
					t.Fatal("schema mismatch reached recovery startup or lost its diagnostic", err, service.restarts)
				}
				got, err := manager.Status(t.Context())
				updaterMust(t, err)
				if got.Transaction.State != want {
					t.Fatal("recovery did not persist its safe terminal state", got.Transaction.State)
				}
				assertSchemaTree(t, c.DataDir, before)
				current, err := os.Readlink(filepath.Join(c.InstallDir, "current"))
				updaterMust(t, err)
				binaryData, err := os.ReadFile(binaryPath)
				updaterMust(t, err)
				if current != "versions/1.0.0" || string(binaryData) != "preserved panel fixture" {
					t.Fatal("schema rejection changed the installed executable or symlink")
				}
			})
		}
	}
}
