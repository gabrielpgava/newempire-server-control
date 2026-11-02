package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gabrielpgava/newempire-server-control/internal/database"
	"github.com/gabrielpgava/newempire-server-control/internal/model"
)

func TestExclusiveJobsIdempotencyAndCrashRecovery(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	e := New(db)
	var active atomic.Int32
	var overlap atomic.Bool
	e.Register("write", 0, func(ctx context.Context, task *Task, p json.RawMessage) (any, error) {
		if active.Add(1) > 1 {
			overlap.Store(true)
		}
		defer active.Add(-1)
		if err := task.Progress(ctx, 10, "Validated"); err != nil {
			return nil, err
		}
		time.Sleep(20 * time.Millisecond)
		return nil, task.CommitBarrier(ctx)
	})
	id := model.NewID()
	first, err := e.Enqueue(context.Background(), id, "write", map[string]int{"v": 1}, "request", 0)
	if err != nil {
		t.Fatal(err)
	}
	again, err := e.Enqueue(context.Background(), id, "write", map[string]int{"v": 1}, "request", 0)
	if err != nil || first.ID != again.ID {
		t.Fatal("idempotency failed")
	}
	if _, err := e.Enqueue(context.Background(), id, "write", map[string]int{"v": 2}, "request", 0); err == nil {
		t.Fatal("conflicting idempotency accepted")
	}
	second, err := e.Enqueue(context.Background(), id, "write", nil, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := e.Start(ctx, 2); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		j, _ := e.Get(ctx, second.ID)
		if j.State == "succeeded" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	j, err := e.Get(ctx, second.ID)
	if err != nil || j.State != "succeeded" || overlap.Load() {
		t.Fatalf("state=%s overlap=%t err=%v", j.State, overlap.Load(), err)
	}
	cancel()
	e.Wait()
	if _, err := db.Exec("UPDATE jobs SET state='running' WHERE id=?", first.ID); err != nil {
		t.Fatal(err)
	}
	restartCtx, stop := context.WithCancel(context.Background())
	defer stop()
	if err := e.Start(restartCtx, 0); err != nil {
		t.Fatal(err)
	}
	j, _ = e.Get(context.Background(), first.ID)
	if j.State != "interrupted" {
		t.Fatal("unsafe automatic replay after crash")
	}
}

func TestRunOneNeverExecutesUnrelatedQueuedWork(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	e := New(db)
	var calls int
	e.Register("selected", 0, func(ctx context.Context, task *Task, _ json.RawMessage) (any, error) {
		calls++
		if err := task.Progress(ctx, 20, "Only the explicitly selected task"); err != nil {
			return nil, err
		}
		return map[string]bool{"selected": true}, task.CommitBarrier(ctx)
	})
	other, err := e.Enqueue(t.Context(), "", "selected", nil, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := e.Enqueue(t.Context(), model.NewID(), "selected", nil, "", -100)
	if err != nil {
		t.Fatal(err)
	}
	result, err := e.RunOne(t.Context(), selected.ID)
	if err != nil || result.State != "succeeded" || calls != 1 || result.Progress != 100 {
		t.Fatalf("selected job: %+v calls=%d error=%v", result, calls, err)
	}
	untouched, err := e.Get(t.Context(), other.ID)
	if err != nil || untouched.State != "queued" || untouched.Attempts != 0 {
		t.Fatalf("unrelated job was touched: %+v %v", untouched, err)
	}
	if _, err := e.RunOne(t.Context(), selected.ID); err == nil {
		t.Fatal("terminal job was replayed")
	}
}

func TestRunOnePreservesExclusionAndHandlerFailure(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	e := New(db)
	e.Register("failed", 0, func(context.Context, *Task, json.RawMessage) (any, error) {
		return map[string]string{"recovery": "retained"}, errors.New("controlled failure")
	})
	instance := model.NewID()
	blocker, err := e.Enqueue(t.Context(), instance, "failed", nil, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := e.Enqueue(t.Context(), instance, "failed", nil, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE jobs SET state='running' WHERE id=?", blocker.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RunOne(t.Context(), selected.ID); err == nil {
		t.Fatal("selected job bypassed instance exclusion")
	}
	if _, err := db.Exec("UPDATE jobs SET instance_id='' WHERE id=?", blocker.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RunOne(t.Context(), selected.ID); err == nil {
		t.Fatal("selected job bypassed global exclusion")
	}
	if _, err := db.Exec("UPDATE jobs SET state='interrupted' WHERE id=?", blocker.ID); err != nil {
		t.Fatal(err)
	}
	failed, err := e.RunOne(t.Context(), selected.ID)
	if err != nil || failed.State != "failed" || failed.Error != "controlled failure" || string(failed.Result) != `{"recovery":"retained"}` {
		t.Fatalf("handler failure was not durable: %+v %v", failed, err)
	}
	for _, id := range []string{"", "../unsafe", model.NewID()} {
		if _, err := e.RunOne(t.Context(), id); err == nil {
			t.Fatal("accepted an invalid or missing explicit job")
		}
	}
}
