package jobs

import (
	"context"
	"encoding/json"
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
	stop()
	if err := e.Start(restartCtx, 0); err != nil {
		t.Fatal(err)
	}
	j, _ = e.Get(context.Background(), first.ID)
	if j.State != "interrupted" {
		t.Fatal("unsafe automatic replay after crash")
	}
}
