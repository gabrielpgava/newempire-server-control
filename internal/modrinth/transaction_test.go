package modrinth

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"
)

func readText(t *testing.T, root *os.Root, name string) string {
	t.Helper()
	data, err := root.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
func assertMissing(t *testing.T, root *os.Root, name string) {
	t.Helper()
	if _, err := root.Lstat(name); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("expected missing %s, got %v", name, err)
	}
}

func TestStageApplyRollbackPreservesUnknownLocalConfigAndWorlds(t *testing.T) {
	c := newCatalog()
	old := c.add("A", "old", "original JAR")
	root := testRoot(t)
	installFixture(t, root, c, old)
	put(t, root, "mods/unknown.jar", "unknown JAR")
	put(t, root, "mods/readme.txt", "external notes")
	put(t, root, "mods/subdir/note.txt", "nested notes")
	put(t, root, "config/local.toml", "local custom config")
	put(t, root, "world/region/test.mca", "world must never be rolled back")
	originalLock := readText(t, root, LockFilename)
	newVersion := c.add("A", "new", "updated JAR")
	s := NewService(c.client(t))
	p := mustPlan(t, s, root, Request{Target: testTarget})
	if len(p.Blockers) > 0 {
		t.Fatal(p.Blockers)
	}
	config := "new config"
	p.Config = []ConfigFile{{Path: "config/new.toml", SHA512: hashBytes([]byte(config)), Size: int64(len(config))}}
	p.Seal()
	stage := testRoot(t)
	put(t, stage, "overrides/config/new.toml", config)
	if err := s.Stage(context.Background(), stage, p); err != nil {
		t.Fatal(err)
	}
	if got := readText(t, root, "mods/"+old.Files[0].Filename); got != "original JAR" {
		t.Fatal("staging changed original")
	}
	tx, err := Apply(context.Background(), root, stage, p)
	if err != nil {
		t.Fatal(err)
	}
	if tx.State != "applied" {
		t.Fatal(tx)
	}
	assertMissing(t, root, "mods/"+old.Files[0].Filename)
	if readText(t, root, "mods/"+newVersion.Files[0].Filename) != "updated JAR" || readText(t, root, "mods/unknown.jar") != "unknown JAR" || readText(t, root, "mods/subdir/note.txt") != "nested notes" {
		t.Fatal("content apply corrupted files")
	}
	if readText(t, root, "config/local.toml") != "local custom config" || readText(t, root, "config/new.toml") != config {
		t.Fatal("config not preserved/applied")
	}
	put(t, root, "world/region/test.mca", "new world progress")
	loaded, err := LoadTransaction(root, tx.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := Rollback(context.Background(), root, loaded); err != nil {
		t.Fatal(err)
	}
	if readText(t, root, "mods/"+old.Files[0].Filename) != "original JAR" || readText(t, root, LockFilename) != originalLock {
		t.Fatal("original content/lock not recovered exactly")
	}
	assertMissing(t, root, "mods/"+newVersion.Files[0].Filename)
	assertMissing(t, root, "config/new.toml")
	if readText(t, root, "mods/readme.txt") != "external notes" || readText(t, root, "config/local.toml") != "local custom config" || readText(t, root, "world/region/test.mca") != "new world progress" {
		t.Fatal("unrelated content was rolled back")
	}
	if err := Rollback(context.Background(), root, loaded); err != nil {
		t.Fatal("rollback not idempotent", err)
	}
	if err := Finalize(root, loaded); err != nil {
		t.Fatal(err)
	}
	if err := Finalize(root, loaded); err != nil {
		t.Fatal("finalize not idempotent", err)
	}
	assertMissing(t, root, receiptName(tx.ID))
	assertMissing(t, root, recoveryName(tx.ID, "discard"))
}

func TestHashMismatchAndTamperedStageNeverMutateOriginals(t *testing.T) {
	for _, kind := range []string{"CDN hash mismatch", "tampered stage", "changed plan", "changed mods", "changed policy"} {
		t.Run(kind, func(t *testing.T) {
			c := newCatalog()
			old := c.add("A", "old", "old")
			root := testRoot(t)
			installFixture(t, root, c, old)
			next := c.add("A", "new", "new")
			s := NewService(c.client(t))
			p := mustPlan(t, s, root, Request{Target: testTarget})
			stage := testRoot(t)
			if kind == "CDN hash mismatch" {
				c.jars[next.Files[0].URL] = "bad"
				if err := s.Stage(context.Background(), stage, p); err == nil {
					t.Fatal("accepted corrupt download")
				}
				assertMissing(t, stage, stageManifest)
				if readText(t, root, "mods/"+old.Files[0].Filename) != "old" {
					t.Fatal("original changed")
				}
				return
			}
			if err := s.Stage(context.Background(), stage, p); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "tampered stage":
				put(t, stage, blobPath(locked(next)), "bad")
			case "changed plan":
				p.Files[0].Filename = "other.jar"
			case "changed mods":
				put(t, root, "mods/external.jar", "external")
			case "changed policy":
				if err := SavePolicy(root, Policy{Mode: "notify"}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := Apply(context.Background(), root, stage, p); err == nil {
				t.Fatal("accepted stale/tampered content")
			}
			if readText(t, root, "mods/"+old.Files[0].Filename) != "old" {
				t.Fatal("original changed")
			}
			assertMissing(t, root, "mods/"+next.Files[0].Filename)
		})
	}
}

func TestRuntimePermissionNormalizationDoesNotBreakContentRollback(t *testing.T) {
	c := newCatalog()
	old := c.add("A", "old", "old bytes")
	root := testRoot(t)
	installFixture(t, root, c, old)
	next := c.add("A", "new", "new bytes")
	service := NewService(c.client(t))
	plan := mustPlan(t, service, root, Request{Target: testTarget})
	stage := testRoot(t)
	if err := service.Stage(context.Background(), stage, plan); err != nil {
		t.Fatal(err)
	}
	if err := root.Chmod("mods/"+old.Files[0].Filename, 0660); err != nil {
		t.Fatal(err)
	}
	tx, err := Apply(context.Background(), root, stage, plan)
	if err != nil {
		t.Fatal("safe pre-apply permission normalization invalidated content identity:", err)
	}
	if err := root.Chmod("mods/"+next.Files[0].Filename, 0660); err != nil {
		t.Fatal(err)
	}
	if err := Rollback(context.Background(), root, tx); err != nil {
		t.Fatal("safe startup permission normalization prevented rollback:", err)
	}
	if readText(t, root, "mods/"+old.Files[0].Filename) != "old bytes" {
		t.Fatal("old content was not restored")
	}
}

func TestCommitFailureRecoversOriginalContent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission-failure fixture requires an unprivileged user")
	}
	c := newCatalog()
	old := c.add("A", "old", "old")
	root := testRoot(t)
	installFixture(t, root, c, old)
	_ = c.add("A", "new", "new")
	s := NewService(c.client(t))
	p := mustPlan(t, s, root, Request{Target: testTarget})
	stage := testRoot(t)
	data := "config"
	p.Config = []ConfigFile{{Path: "config/test.toml", SHA512: hashBytes([]byte(data)), Size: int64(len(data))}}
	p.Seal()
	put(t, stage, "overrides/config/test.toml", data)
	if err := s.Stage(context.Background(), stage, p); err != nil {
		t.Fatal(err)
	}
	if err := root.Mkdir("config", 0500); err != nil {
		t.Fatal(err)
	}
	defer root.Chmod("config", 0750)
	tx, err := Apply(context.Background(), root, stage, p)
	if err == nil || tx == nil || tx.State != "rolled_back" {
		t.Fatalf("transaction=%v error=%v", tx, err)
	}
	if readText(t, root, "mods/"+old.Files[0].Filename) != "old" {
		t.Fatal("commit error did not restore original JAR")
	}
	if err := Finalize(root, tx); err != nil {
		t.Fatal(err)
	}
}

func TestRollbackAfterInterruptedRenameBarriers(t *testing.T) {
	for _, barrier := range []string{"prepared", "original renamed", "new active", "lock written"} {
		t.Run(barrier, func(t *testing.T) {
			c := newCatalog()
			old := c.add("A", "old", "old")
			root := testRoot(t)
			installFixture(t, root, c, old)
			oldLock := []byte(readText(t, root, LockFilename))
			s := NewService(c.client(t))
			p := mustPlan(t, s, root, Request{Target: testTarget})
			before, _, err := snapshot(context.Background(), root, "mods")
			if err != nil {
				t.Fatal(err)
			}
			next := recoveryName(p.ID, "next")
			put(t, root, next+"/new.jar", "new")
			after, _, err := snapshot(context.Background(), root, next)
			if err != nil {
				t.Fatal(err)
			}
			r := &receipt{Transaction: Transaction{ID: p.ID, State: "prepared"}, PlanHash: planHash(p), Before: before, After: after, HadMods: true, BeforeLock: oldLock, AfterLock: []byte(`{"schema":1}`)}
			if err := saveReceipt(root, r); err != nil {
				t.Fatal(err)
			}
			if barrier != "prepared" {
				if err := root.Rename("mods", recoveryName(p.ID, "original")); err != nil {
					t.Fatal(err)
				}
			}
			if barrier == "new active" || barrier == "lock written" {
				if err := root.Rename(next, "mods"); err != nil {
					t.Fatal(err)
				}
			}
			if barrier == "lock written" {
				if err := atomicWrite(root, LockFilename, r.AfterLock); err != nil {
					t.Fatal(err)
				}
			}
			tx, err := LoadTransaction(root, p.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := Rollback(context.Background(), root, tx); err != nil {
				t.Fatal(err)
			}
			if readText(t, root, "mods/"+old.Files[0].Filename) != "old" || readText(t, root, LockFilename) != string(oldLock) {
				t.Fatal("interruption recovery failed")
			}
			if err := Finalize(root, tx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFirstInstallRollbackAndHealthFinalize(t *testing.T) {
	for _, rollback := range []bool{false, true} {
		t.Run(map[bool]string{true: "rollback", false: "healthy"}[rollback], func(t *testing.T) {
			c := newCatalog()
			v := c.add("A", "v1", "jar")
			s := NewService(c.client(t))
			root := testRoot(t)
			stage := testRoot(t)
			p := mustPlan(t, s, root, Request{Target: testTarget, Projects: []string{"A"}})
			if err := s.Stage(context.Background(), stage, p); err != nil {
				t.Fatal(err)
			}
			tx, err := Apply(context.Background(), root, stage, p)
			if err != nil {
				t.Fatal(err)
			}
			if again, err := Apply(context.Background(), root, stage, p); err != nil || again.ID != tx.ID {
				t.Fatal("idempotent apply failed", again, err)
			}
			if rollback {
				if err := Rollback(context.Background(), root, tx); err != nil {
					t.Fatal(err)
				}
				assertMissing(t, root, "mods")
				assertMissing(t, root, LockFilename)
			} else {
				if readText(t, root, "mods/"+v.Files[0].Filename) != "jar" {
					t.Fatal("not installed")
				}
			}
			if err := Finalize(root, tx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRollbackRefusesLaterAdministratorChanges(t *testing.T) {
	c := newCatalog()
	old := c.add("A", "old", "old")
	root := testRoot(t)
	installFixture(t, root, c, old)
	_ = c.add("A", "new", "new")
	s := NewService(c.client(t))
	stage := testRoot(t)
	p := mustPlan(t, s, root, Request{Target: testTarget})
	if err := s.Stage(context.Background(), stage, p); err != nil {
		t.Fatal(err)
	}
	tx, err := Apply(context.Background(), root, stage, p)
	if err != nil {
		t.Fatal(err)
	}
	put(t, root, "mods/external.jar", "administrator addition")
	if err := Rollback(context.Background(), root, tx); err == nil {
		t.Fatal("destructive rollback accepted")
	}
	if readText(t, root, "mods/external.jar") != "administrator addition" {
		t.Fatal("user work lost")
	}
	if err := Finalize(root, tx); err == nil {
		t.Fatal("recovery deleted despite content drift")
	}
}

func TestSymlinksAndUnsafePlansRejected(t *testing.T) {
	c := newCatalog()
	s := NewService(c.client(t))
	root := testRoot(t)
	put(t, root, "world/secret.jar", "private")
	if err := root.Mkdir("mods", 0750); err != nil {
		t.Fatal(err)
	}
	if err := root.Symlink("../world/secret.jar", "mods/escape.jar"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Inventory(context.Background(), root); err == nil {
		t.Fatal("symlink inventory accepted")
	}
	p := &Plan{ID: strings.Repeat("a", 32), Target: testTarget, Files: []LockedFile{}, Config: []ConfigFile{{Path: "world/region/file", SHA512: strings.Repeat("a", 128), Size: 1}}}
	if err := s.Stage(context.Background(), testRoot(t), p); err == nil {
		t.Fatal("world override accepted")
	}
	r := receipt{Transaction: Transaction{ID: p.ID, State: "prepared"}, PlanHash: strings.Repeat("a", 128), Config: p.Config}
	data, _ := json.Marshal(r)
	if err := root.WriteFile(receiptName(p.ID), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := Rollback(context.Background(), root, &r.Transaction); err == nil {
		t.Fatal("unsafe journal accepted")
	}
}

func TestCanceledApplyDoesNotCommit(t *testing.T) {
	c := newCatalog()
	c.add("A", "v1", "jar")
	s := NewService(c.client(t))
	root := testRoot(t)
	stage := testRoot(t)
	p := mustPlan(t, s, root, Request{Target: testTarget, Projects: []string{"A"}})
	if err := s.Stage(context.Background(), stage, p); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Apply(ctx, root, stage, p); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	assertMissing(t, root, "mods")
	assertMissing(t, root, receiptName(p.ID))
}

func TestOversizedReceiptFailsBeforePublishing(t *testing.T) {
	root := testRoot(t)
	r := &receipt{Transaction: Transaction{ID: strings.Repeat("a", 32), State: "prepared"}, BeforeLock: make([]byte, 13<<20)}
	if err := saveReceipt(root, r); err == nil {
		t.Fatal("unreadable oversized recovery receipt was published")
	}
	assertMissing(t, root, receiptName(r.ID))
}
