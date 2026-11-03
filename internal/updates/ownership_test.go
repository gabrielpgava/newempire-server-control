package updates

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestUpdaterRejectsPanelOwnedStateBeforeTrust(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("two-UID ownership fixture requires isolated root")
	}
	for _, target := range []string{"updates", "updates/journal.json", "updates/lock", "updates/staging", "updates/snapshots", "snapshot directory", "snapshot file"} {
		t.Run(target, func(t *testing.T) {
			f := newFixture(t)
			tx := stage(t, f)
			must(t, f.m.Apply(context.Background()))
			calls := len(f.service.calls)
			name := target
			if target == "snapshot directory" {
				name = snapshotDir(&tx)
			}
			if target == "snapshot file" {
				name = snapshotDir(&tx) + "/data/config.json"
			}
			must(t, os.Chown(filepath.Join(f.root, filepath.FromSlash(name)), 1000, 1000))
			if err := f.m.Rollback(context.Background()); err == nil {
				t.Fatal("panel-owned recovery state was trusted")
			}
			if len(f.service.calls) != calls || f.restoreCalls != 0 || f.service.current() != "1.1.0" {
				t.Fatal("untrusted recovery state reached the service or restore adapter")
			}
			if target == "updates" || target == "updates/journal.json" {
				if _, err := f.m.Status(context.Background()); err == nil {
					t.Fatal("panel-owned journal was exposed as trusted status")
				}
			}
		})
	}
}

func TestUpdaterRejectsLinkedStateFiles(t *testing.T) {
	f := newFixture(t)
	stage(t, f)
	journal := filepath.Join(f.root, "updates", "journal.json")
	must(t, os.Link(journal, journal+".linked"))
	if _, err := f.m.Status(context.Background()); err == nil {
		t.Fatal("multiply linked journal was trusted")
	}
	if err := f.m.Apply(context.Background()); err == nil || len(f.service.calls) != 0 {
		t.Fatal("linked journal reached the service", err)
	}
}
