package updates

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestCrashRecoveryAtEveryMutationPhaseIsOffline(t *testing.T) {
	for _, interrupted := range []State{Stopping, Snapshotting, Installing, CheckingHealth, RollingBack, ManualRecovery} {
		t.Run(string(interrupted), func(t *testing.T) {
			f := newFixture(t)
			config := read(t, filepath.Join(f.data, "config.json"))
			stage(t, f)
			r, err := f.m.lock(context.Background())
			must(t, err)
			j, err := readJournal(r.Root)
			must(t, err)
			tx := j.Transaction
			tx.PreviousSHA256, _, err = fileHash(context.Background(), r.Root, "versions/1.0.0/"+BinaryName)
			must(t, err)
			f.service.running = false
			if interrupted != Stopping && interrupted != Snapshotting {
				tx.SnapshotSHA256, err = f.m.snapshot(context.Background(), r.Root, tx)
				must(t, err)
			}
			if interrupted == CheckingHealth || interrupted == RollingBack || interrupted == ManualRecovery {
				must(t, r.Mkdir("versions/1.1.0", 0755))
				must(t, copyFile(filepath.Join(f.root, filepath.FromSlash(stageDir(tx)), "payload", BinaryName), filepath.Join(f.root, "versions", "1.1.0", BinaryName)))
				must(t, switchLink(r.Root, "current", "1.1.0"))
				writeDB(t, f.data, "interrupted update changed database")
				must(t, os.WriteFile(filepath.Join(f.data, "config.json"), []byte("interrupted update config"), 0600))
			}
			must(t, transition(r.Root, &j, interrupted, "Simulated process crash"))
			r.close()
			f.github.hook = func(*http.Request) (*http.Response, error) {
				t.Fatal("recovery attempted a network request")
				return nil, nil
			}
			// A new helper can recover solely from the fsynced journal and snapshots.
			f.m, err = New(f.opts)
			must(t, err)
			must(t, f.m.Recover(context.Background()))
			assertOriginal(t, f, config)
			want := RolledBack
			if interrupted == Stopping || interrupted == Snapshotting {
				want = Failed
			}
			if state(t, f.m).Transaction.State != want {
				t.Fatal("recovery state not durable")
			}
			calls := len(f.service.calls)
			must(t, f.m.Recover(context.Background()))
			if len(f.service.calls) != calls {
				t.Fatal("completed recovery was not idempotent")
			}
		})
	}
}

func TestInterruptedStagingRecoveryAndLocking(t *testing.T) {
	f := newFixture(t)
	stage(t, f)
	r, err := f.m.lock(context.Background())
	must(t, err)
	if _, err := f.m.Stage(context.Background(), "1.1.0", Stable); !errors.Is(err, ErrConflict) {
		t.Fatal("parallel stage ignored lock", err)
	}
	if err := f.m.Apply(context.Background()); !errors.Is(err, ErrConflict) {
		t.Fatal("parallel helper ignored lock", err)
	}
	j, err := readJournal(r.Root)
	must(t, err)
	must(t, transition(r.Root, &j, Downloading, "Simulated download interruption"))
	r.close()
	if _, err := f.m.Stage(context.Background(), "1.1.0", Stable); !errors.Is(err, ErrConflict) {
		t.Fatal("interrupted transaction was silently discarded", err)
	}
	must(t, f.m.Recover(context.Background()))
	if j := state(t, f.m); j.Transaction.State != Failed || len(f.service.calls) != 0 {
		t.Fatal("staging recovery touched panel", j)
	}
	_, err = f.m.Stage(context.Background(), "1.1.0", Stable)
	must(t, err)
	must(t, f.m.Recover(context.Background()))
	if state(t, f.m).Transaction.State != Staged || len(f.service.calls) != 0 {
		t.Fatal("valid staged release was modified by recovery")
	}
}

func TestRecoveryRetriesFailedPreSwitchRestart(t *testing.T) {
	f := newFixture(t)
	stage(t, f)
	f.opts.Snapshot = func(context.Context, string) error { return errors.New("snapshot unavailable") }
	f.service.onRestart = func(string) error { return errors.New("panel restart unavailable") }
	var err error
	f.m, err = New(f.opts)
	must(t, err)
	if err := f.m.Apply(context.Background()); !errors.Is(err, ErrRecovery) || state(t, f.m).Transaction.State != ManualRecovery {
		t.Fatal("failed pre-switch restart did not request recovery", err)
	}
	f.service.onRestart = nil
	must(t, f.m.Recover(context.Background()))
	if state(t, f.m).Transaction.State != Failed || f.service.current() != "1.0.0" || !f.service.running || f.restoreCalls != 0 {
		t.Fatal("pre-switch recovery did not safely restart the unchanged panel")
	}
}

func TestPreSwitchRecoveryRefusesSchemaDrift(t *testing.T) {
	f := newFixture(t)
	stage(t, f)
	r, err := f.m.lock(context.Background())
	must(t, err)
	j, err := readJournal(r.Root)
	must(t, err)
	j.Transaction.PreviousSHA256, _, err = fileHash(context.Background(), r.Root, "versions/1.0.0/"+BinaryName)
	must(t, err)
	must(t, transition(r.Root, &j, Stopping, "Simulated pre-switch interruption"))
	r.close()
	f.opts.Schema = 2
	f.m, err = New(f.opts)
	must(t, err)
	if err := f.m.Recover(context.Background()); !errors.Is(err, ErrConflict) || len(f.service.calls) != 0 || state(t, f.m).Transaction.State != ManualRecovery {
		t.Fatal("pre-switch recovery restarted a schema-incompatible panel", err)
	}
}

func TestRecoveryDoesNotHideJournalPersistenceFailure(t *testing.T) {
	f := newFixture(t)
	stage(t, f)
	r, err := f.m.lock(context.Background())
	must(t, err)
	j, err := readJournal(r.Root)
	must(t, err)
	j.Transaction.PreviousSHA256, _, err = fileHash(context.Background(), r.Root, "versions/1.0.0/"+BinaryName)
	must(t, err)
	must(t, transition(r.Root, &j, Stopping, "Simulated interrupted stop"))
	r.close()
	f.service.onRestart = func(string) error {
		journal := filepath.Join(f.root, "updates", "journal.json")
		must(t, os.Rename(journal, journal+".preserved"))
		return os.Mkdir(journal, 0700) // Force atomic journal replacement to fail.
	}
	if err := f.m.Recover(context.Background()); err == nil {
		t.Fatal("recovery claimed durable success after its journal write failed")
	}
}

func TestFilesystemEscapesAndCorruptJournalFailClosed(t *testing.T) {
	for _, attack := range []string{"absolute current", "traversal current", "updates symlink", "staging symlink", "corrupt journal", "journal traversal"} {
		t.Run(attack, func(t *testing.T) {
			f := newFixture(t)
			stage(t, f)
			switch attack {
			case "absolute current", "traversal current":
				must(t, os.Remove(filepath.Join(f.root, "current")))
				target := f.data
				if attack == "traversal current" {
					target = "versions/../../outside"
				}
				must(t, os.Symlink(target, filepath.Join(f.root, "current")))
			case "updates symlink":
				must(t, os.Rename(filepath.Join(f.root, "updates"), filepath.Join(f.root, "saved-updates")))
				must(t, os.Symlink(f.data, filepath.Join(f.root, "updates")))
			case "staging symlink":
				must(t, os.Rename(filepath.Join(f.root, "updates", "staging"), filepath.Join(f.root, "updates", "saved-staging")))
				must(t, os.Symlink(f.data, filepath.Join(f.root, "updates", "staging")))
			case "corrupt journal":
				must(t, os.WriteFile(filepath.Join(f.root, "updates", "journal.json"), []byte("{} garbage"), 0600))
			case "journal traversal":
				r, err := f.m.lock(context.Background())
				must(t, err)
				j, err := readJournal(r.Root)
				must(t, err)
				j.Transaction.ID = "../../outside"
				must(t, transition(r.Root, &j, Staged, "malformed path"))
				r.close()
			}
			if err := f.m.Apply(context.Background()); err == nil || len(f.service.calls) != 0 {
				t.Fatal("unsafe local state reached panel service", err)
			}
			if readDB(t, f.data) != "original" || string(read(t, f.world)) != "world must never change" {
				t.Fatal("unsafe local state escaped install root")
			}
		})
	}
}

func TestConfigurationAndHTTPErrorClassification(t *testing.T) {
	f := newFixture(t)
	for _, mutate := range []func(*Options){
		func(o *Options) { o.PublicKey = "" },
		func(o *Options) { o.PublicKey += "\n" },
		func(o *Options) { o.CurrentVersion = "dev" },
		func(o *Options) { o.InstallDir = "/" },
		func(o *Options) { o.InstallDir = "relative" },
		func(o *Options) { o.Schema = 0 },
		func(o *Options) { o.HealthTimeout = -1 },
	} {
		opts := f.opts
		mutate(&opts)
		if _, err := New(opts); err == nil {
			t.Fatal("invalid updater configuration accepted")
		}
	}
	f.opts.Service = nil
	m, err := New(f.opts)
	must(t, err)
	if err := m.Apply(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatal("apply accepted missing restricted helper adapters", err)
	}
	for _, tc := range []struct {
		err    error
		status int
	}{{nil, 200}, {ErrInvalid, 400}, {ErrConflict, 409}, {ErrVerification, 502}, {ErrUpstream, 502}, {ErrUnavailable, 503}, {ErrRecovery, 500}, {context.Canceled, 408}, {context.DeadlineExceeded, 408}, {os.ErrPermission, 500}} {
		if got := HTTPStatus(tc.err); got != tc.status {
			t.Fatalf("%v: status %d, want %d", tc.err, got, tc.status)
		}
	}
}
