package updates

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func assertOriginal(t *testing.T, f *fixture, config []byte) {
	t.Helper()
	if f.service.current() != "1.0.0" || !f.service.running || readDB(t, f.data) != "original" {
		t.Fatalf("original panel/database not restored: current=%s running=%t", f.service.current(), f.service.running)
	}
	if !bytes.Equal(read(t, filepath.Join(f.data, "config.json")), config) || string(read(t, filepath.Join(f.data, "encryption.key"))) != "fixture credential key" {
		t.Fatal("configuration/encryption material not restored")
	}
	if string(read(t, f.world)) != "world must never change" {
		t.Fatal("panel update touched Minecraft data")
	}
}

func mutatePanelOnNewRestart(t *testing.T, f *fixture) {
	t.Helper()
	f.service.onRestart = func(v string) error {
		if v == "1.1.0" {
			writeDB(t, f.data, "new panel wrote settings")
			must(t, os.WriteFile(filepath.Join(f.data, "config.json"), []byte("changed by new panel"), 0600))
			must(t, os.WriteFile(filepath.Join(f.data, "encryption.key"), []byte("changed key"), 0600))
		}
		return nil
	}
}

func TestSuccessfulApplyAndExplicitRollback(t *testing.T) {
	f := newFixture(t)
	before := read(t, filepath.Join(f.root, "versions", "1.0.0", BinaryName))
	config := read(t, filepath.Join(f.data, "config.json"))
	tx := stage(t, f)
	if tx.State != Staged || !validHash(tx.BinarySHA256) || len(f.service.calls) != 0 {
		t.Fatal("stage did not complete verification without touching live service", tx)
	}
	mutatePanelOnNewRestart(t, f)
	must(t, f.m.Apply(context.Background()))
	j := state(t, f.m)
	if j.Transaction.State != Complete || j.LastApplied == nil || j.LastApplied.ID != tx.ID || f.service.current() != "1.1.0" {
		t.Fatalf("apply did not commit: %+v", j)
	}
	if previous, err := os.Readlink(filepath.Join(f.root, "previous")); err != nil || previous != "versions/1.0.0" {
		t.Fatal("previous symlink not retained", previous, err)
	}
	if !bytes.Equal(before, read(t, filepath.Join(f.root, "versions", "1.0.0", BinaryName))) || !bytes.Equal(before, read(t, filepath.Join(f.root, filepath.FromSlash(snapshotDir(&tx)), "binary"))) {
		t.Fatal("previous binary was overwritten or not snapshotted")
	}
	if readDB(t, f.data) != "new panel wrote settings" || f.snapshotCalls != 1 || f.restoreCalls != 0 {
		t.Fatal("successful update restored state unexpectedly")
	}
	for name, want := range map[string]os.FileMode{
		filepath.Join(f.root, "versions", "1.1.0", BinaryName):                              0555,
		filepath.Join(f.root, "updates", "journal.json"):                                    0600,
		filepath.Join(f.root, filepath.FromSlash(snapshotDir(&tx)), "data", "panel.sqlite"): 0600,
	} {
		info, err := os.Stat(name)
		must(t, err)
		if info.Mode().Perm() != want {
			t.Fatalf("%s: permissions %o, want %o", name, info.Mode().Perm(), want)
		}
	}
	must(t, f.m.Rollback(context.Background()))
	assertOriginal(t, f, config)
	j = state(t, f.m)
	if j.Transaction.State != RolledBack || j.LastApplied != nil || f.restoreCalls != 1 {
		t.Fatal("rollback not durably recorded", j)
	}
	if _, err := os.Stat(filepath.Join(f.root, "versions", "1.1.0", BinaryName)); err != nil {
		t.Fatal("rollback deleted the failed/new binary", err)
	}
	if err := f.m.Rollback(context.Background()); !errors.Is(err, ErrConflict) {
		t.Fatal("repeat rollback claimed success", err)
	}
}

func TestFailedReadinessAutomaticallyRestoresBinarySQLiteAndConfig(t *testing.T) {
	for _, failure := range []string{"health", "restart", "health timeout"} {
		t.Run(failure, func(t *testing.T) {
			f := newFixture(t)
			config := read(t, filepath.Join(f.data, "config.json"))
			stage(t, f)
			mutatePanelOnNewRestart(t, f)
			if failure == "restart" {
				old := f.service.onRestart
				f.service.onRestart = func(v string) error {
					must(t, old(v))
					if v == "1.1.0" {
						return errors.New("systemd restart failed")
					}
					return nil
				}
			} else {
				f.service.onHealth = func(ctx context.Context, v string) error {
					if v == "1.1.0" {
						if failure == "health timeout" {
							<-ctx.Done()
							return ctx.Err()
						}
						return errors.New("authenticated readiness rejected new panel")
					}
					return nil
				}
			}
			if err := f.m.Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "rolled back") {
				t.Fatal("failed update reported success or missed rollback", err)
			}
			assertOriginal(t, f, config)
			j := state(t, f.m)
			if j.Transaction.State != RolledBack || j.Transaction.Error == "" || j.LastApplied != nil || f.restoreCalls != 1 {
				t.Fatal("failure diagnostics/rollback missing", j)
			}
		})
	}
}

func TestApplyReverifiesSignatureAndHashAndIgnoresMutablePayload(t *testing.T) {
	for _, tamper := range []string{"signature", "archive", "payload"} {
		t.Run(tamper, func(t *testing.T) {
			f := newFixture(t)
			tx := stage(t, f)
			dir := filepath.Join(f.root, filepath.FromSlash(stageDir(&tx)))
			switch tamper {
			case "signature":
				must(t, os.WriteFile(filepath.Join(dir, "release.json.sig"), bytes.Repeat([]byte("A"), 88), 0600))
			case "archive":
				must(t, os.WriteFile(filepath.Join(dir, "archive.tar.gz"), []byte("tampered"), 0600))
			case "payload":
				name := filepath.Join(dir, "payload", BinaryName)
				must(t, os.Chmod(name, 0600))
				must(t, os.WriteFile(name, []byte("never execute this"), 0600))
			}
			err := f.m.Apply(context.Background())
			if tamper == "payload" {
				must(t, err)
				if !bytes.Contains(read(t, filepath.Join(f.root, "current", BinaryName)), []byte("1.1.0")) {
					t.Fatal("mutable payload was trusted")
				}
			} else if !errors.Is(err, ErrVerification) || len(f.service.calls) != 0 || f.service.current() != "1.0.0" {
				t.Fatal("tampered stage reached live service", err)
			}
		})
	}
}

func TestPreSwitchFailuresRestartUnchangedPanel(t *testing.T) {
	for _, failure := range []string{"stop", "snapshot", "empty snapshot", "snapshot symlink", "snapshot cancellation"} {
		t.Run(failure, func(t *testing.T) {
			f := newFixture(t)
			config := read(t, filepath.Join(f.data, "config.json"))
			stage(t, f)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch failure {
			case "stop":
				f.service.onStop = func() error { return errors.New("stop permission denied") }
			case "snapshot":
				f.opts.Snapshot = func(context.Context, string) error { return errors.New("snapshot disk full") }
			case "empty snapshot":
				f.opts.Snapshot = func(context.Context, string) error { return nil }
			case "snapshot symlink":
				f.opts.Snapshot = func(ctx context.Context, dir string) error { return os.Symlink(f.world, filepath.Join(dir, "unsafe")) }
			case "snapshot cancellation":
				original := f.opts.Snapshot
				f.opts.Snapshot = func(ctx context.Context, dir string) error {
					must(t, original(ctx, dir))
					cancel()
					return ctx.Err()
				}
			}
			var err error
			f.m, err = New(f.opts)
			must(t, err)
			err = f.m.Apply(ctx)
			if err == nil || failure == "snapshot cancellation" && !errors.Is(err, context.Canceled) {
				t.Fatal("failed preflight/snapshot reported success or lost cancellation", err)
			}
			assertOriginal(t, f, config)
			if j := state(t, f.m); j.Transaction.State != Failed || j.Transaction.Error == "" || f.restoreCalls != 0 {
				t.Fatal("pre-switch failure did not preserve state", j)
			}
		})
	}
}

func TestCancellationAfterCommitFinishesSafely(t *testing.T) {
	f := newFixture(t)
	stage(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.service.onRestart = func(string) error { cancel(); return nil }
	must(t, f.m.Apply(ctx))
	if ctx.Err() == nil || f.service.current() != "1.1.0" || state(t, f.m).Transaction.State != Complete {
		t.Fatal("post-commit cancellation left incomplete update")
	}
}

func TestRestoreFailureIsRecoverableWithoutStartingPanel(t *testing.T) {
	f := newFixture(t)
	config := read(t, filepath.Join(f.data, "config.json"))
	stage(t, f)
	mutatePanelOnNewRestart(t, f)
	f.service.onHealth = func(ctx context.Context, v string) error {
		if v == "1.1.0" {
			return errors.New("unhealthy updated panel")
		}
		return nil
	}
	originalRestore := f.opts.Restore
	f.opts.Restore = func(context.Context, string) error { return errors.New("restore unavailable") }
	var err error
	f.m, err = New(f.opts)
	must(t, err)
	if err := f.m.Apply(context.Background()); !errors.Is(err, ErrRecovery) {
		t.Fatal("failed rollback was not surfaced", err)
	}
	if j := state(t, f.m); j.Transaction.State != ManualRecovery || f.service.running {
		t.Fatal("unsafe panel restarted after failed restore", j)
	}
	f.opts.Restore = originalRestore
	f.m, err = New(f.opts)
	must(t, err)
	must(t, f.m.Recover(context.Background()))
	assertOriginal(t, f, config)
	if state(t, f.m).Transaction.State != RolledBack {
		t.Fatal("offline helper recovery did not finish rollback")
	}
}

func TestRollbackRefusesCorruptSnapshotAndSchemaChange(t *testing.T) {
	for _, failure := range []string{"snapshot file", "snapshot manifest", "previous binary", "schema", "wrong host architecture"} {
		t.Run(failure, func(t *testing.T) {
			f := newFixture(t)
			tx := stage(t, f)
			must(t, f.m.Apply(context.Background()))
			calls := len(f.service.calls)
			switch failure {
			case "snapshot file":
				must(t, os.WriteFile(filepath.Join(f.root, filepath.FromSlash(snapshotDir(&tx)), "data", "config.json"), []byte("corrupt"), 0600))
			case "snapshot manifest":
				must(t, os.WriteFile(filepath.Join(f.root, filepath.FromSlash(snapshotDir(&tx)), "snapshot.json"), []byte("[]"), 0600))
			case "previous binary":
				old := filepath.Join(f.root, "versions", "1.0.0", BinaryName)
				must(t, os.Chmod(old, 0600))
				must(t, os.WriteFile(old, []byte("corrupt"), 0600))
			case "schema":
				f.opts.Schema = 2
				var err error
				f.m, err = New(f.opts)
				must(t, err)
			case "wrong host architecture":
				f.m.arch = "amd64"
				if runtime.GOARCH == "amd64" {
					f.m.arch = "arm64"
				}
			}
			if err := f.m.Rollback(context.Background()); err == nil {
				t.Fatal("unsafe rollback succeeded")
			}
			if len(f.service.calls) != calls || f.service.current() != "1.1.0" || f.restoreCalls != 0 || state(t, f.m).Transaction.State != ManualRecovery {
				t.Fatal("unsafe rollback stopped or modified live panel")
			}
		})
	}
}

func TestNoOverwriteOfExistingVersionAndNoStaleApply(t *testing.T) {
	for _, mode := range []string{"existing target", "changed current"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			stage(t, f)
			if mode == "existing target" {
				must(t, os.Mkdir(filepath.Join(f.root, "versions", "1.1.0"), 0755))
				must(t, os.WriteFile(filepath.Join(f.root, "versions", "1.1.0", "keep"), []byte("untouched"), 0600))
			} else {
				must(t, os.Mkdir(filepath.Join(f.root, "versions", "1.0.1"), 0755))
				must(t, os.Remove(filepath.Join(f.root, "current")))
				must(t, os.Symlink("versions/1.0.1", filepath.Join(f.root, "current")))
			}
			if err := f.m.Apply(context.Background()); !errors.Is(err, ErrConflict) || len(f.service.calls) != 0 {
				t.Fatalf("%s changed live service: %v", mode, err)
			}
		})
	}
}
