package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/gabrielpgava/newempire-server-control/internal/database"
	"github.com/gabrielpgava/newempire-server-control/internal/model"
)

type Job struct {
	ID              string          `json:"id"`
	InstanceID      string          `json:"instance_id"`
	Kind            string          `json:"kind"`
	Payload         json.RawMessage `json:"payload"`
	State           string          `json:"state"`
	Progress        int             `json:"progress"`
	Step            string          `json:"step"`
	Error           string          `json:"error"`
	Result          json.RawMessage `json:"result"`
	CancelRequested bool            `json:"cancel_requested"`
	Cancellable     bool            `json:"cancellable"`
	Attempts        int             `json:"attempts"`
	CreatedAt       int64           `json:"created_at"`
	StartedAt       sql.NullInt64   `json:"-"`
	FinishedAt      sql.NullInt64   `json:"-"`
}
type Step struct {
	Time     int64  `json:"time"`
	Progress int    `json:"progress"`
	Message  string `json:"message"`
}
type Handler func(context.Context, *Task, json.RawMessage) (any, error)
type registration struct {
	run     Handler
	retries int
}
type Engine struct {
	DB       *database.Store
	handlers map[string]registration
	wake     chan struct{}
	Events   chan struct{}
	wg       sync.WaitGroup
}
type Task struct {
	Job    Job
	engine *Engine
}

func New(db *database.Store) *Engine {
	return &Engine{DB: db, handlers: map[string]registration{}, wake: make(chan struct{}, 1), Events: make(chan struct{}, 1)}
}

// Register retries must be zero for operations that are not proven idempotent.
func (e *Engine) Register(kind string, retries int, h Handler) {
	e.handlers[kind] = registration{h, retries}
}
func (e *Engine) signal() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
	select {
	case e.Events <- struct{}{}:
	default:
	}
}

func (e *Engine) Enqueue(ctx context.Context, instance, kind string, payload any, key string, priority int) (Job, error) {
	if _, ok := e.handlers[kind]; !ok {
		return Job{}, errors.New("unsupported job type")
	}
	if instance != "" && !model.ValidID(instance) {
		return Job{}, errors.New("invalid instance identifier")
	}
	if len(key) > 200 {
		return Job{}, errors.New("idempotency key is too long")
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return Job{}, err
	}
	j := Job{ID: model.NewID(), InstanceID: instance, Kind: kind, Payload: b, State: "queued", Cancellable: true, Result: json.RawMessage(`{}`), CreatedAt: time.Now().Unix()}
	var idempotency any
	if key != "" {
		idempotency = key
	}
	_, err = e.DB.ExecContext(ctx, "INSERT INTO jobs(id,instance_id,kind,payload,state,created_at,idempotency_key,priority) VALUES(?,?,?,?,?,?,?,?)", j.ID, instance, kind, string(b), j.State, j.CreatedAt, idempotency, priority)
	if err != nil && key != "" {
		var id, existingKind, existingInstance, existingPayload string
		if qerr := e.DB.QueryRowContext(ctx, "SELECT id,kind,instance_id,payload FROM jobs WHERE idempotency_key=?", key).Scan(&id, &existingKind, &existingInstance, &existingPayload); qerr == nil {
			if kind != existingKind || instance != existingInstance || string(b) != existingPayload {
				return Job{}, errors.New("idempotency key was already used for a different request")
			}
			return e.Get(ctx, id)
		}
	}
	if err != nil {
		return Job{}, err
	}
	e.signal()
	return j, nil
}

const jobColumns = "id,instance_id,kind,payload,state,progress,step,error,result,cancel_requested,cancellable,attempts,created_at,started_at,finished_at"

type scanner interface{ Scan(...any) error }

func scan(row scanner) (Job, error) {
	var j Job
	var payload, result string
	err := row.Scan(&j.ID, &j.InstanceID, &j.Kind, &payload, &j.State, &j.Progress, &j.Step, &j.Error, &result, &j.CancelRequested, &j.Cancellable, &j.Attempts, &j.CreatedAt, &j.StartedAt, &j.FinishedAt)
	j.Payload = json.RawMessage(payload)
	j.Result = json.RawMessage(result)
	return j, err
}
func (e *Engine) Get(ctx context.Context, id string) (Job, error) {
	return scan(e.DB.QueryRowContext(ctx, "SELECT "+jobColumns+" FROM jobs WHERE id=?", id))
}
func (e *Engine) List(ctx context.Context) ([]Job, error) {
	rows, err := e.DB.QueryContext(ctx, "SELECT "+jobColumns+" FROM jobs ORDER BY created_at DESC,rowid DESC LIMIT 200")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	all := []Job{}
	for rows.Next() {
		j, err := scan(rows)
		if err != nil {
			return nil, err
		}
		all = append(all, j)
	}
	return all, rows.Err()
}
func (e *Engine) Steps(ctx context.Context, id string) ([]Step, error) {
	rows, err := e.DB.QueryContext(ctx, "SELECT time,progress,message FROM job_steps WHERE job_id=? ORDER BY seq", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	all := []Step{}
	for rows.Next() {
		var s Step
		if err := rows.Scan(&s.Time, &s.Progress, &s.Message); err != nil {
			return nil, err
		}
		all = append(all, s)
	}
	return all, rows.Err()
}

func (e *Engine) Cancel(ctx context.Context, id string) error {
	result, err := e.DB.ExecContext(ctx, "UPDATE jobs SET cancel_requested=1,state=CASE WHEN state='queued' THEN 'cancelled' ELSE state END,finished_at=CASE WHEN state='queued' THEN ? ELSE finished_at END WHERE id=? AND cancellable=1 AND state IN ('queued','running')", time.Now().Unix(), id)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return errors.New("job cannot be cancelled at this step")
	}
	e.signal()
	return nil
}

func (t *Task) Progress(ctx context.Context, percent int, message string) error {
	if percent < 0 || percent > 100 || len(message) > 2048 {
		return errors.New("invalid job progress")
	}
	tx, err := t.engine.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var cancel bool
	if err := tx.QueryRowContext(ctx, "SELECT cancel_requested FROM jobs WHERE id=?", t.Job.ID).Scan(&cancel); err != nil {
		return err
	}
	if cancel {
		return context.Canceled
	}
	if _, err := tx.ExecContext(ctx, "UPDATE jobs SET progress=?,step=? WHERE id=?", percent, message, t.Job.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO job_steps(job_id,time,progress,message) VALUES(?,?,?,?)", t.Job.ID, time.Now().Unix(), percent, message); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	t.engine.signal()
	return nil
}

// CommitBarrier atomically refuses late cancellation before an irreversible commit.
func (t *Task) CommitBarrier(ctx context.Context) error {
	r, err := t.engine.DB.ExecContext(ctx, "UPDATE jobs SET cancellable=0 WHERE id=? AND cancel_requested=0", t.Job.ID)
	if err != nil {
		return err
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return context.Canceled
	}
	return nil
}

func (e *Engine) Start(ctx context.Context, workers int) error {
	_, err := e.DB.ExecContext(ctx, "UPDATE jobs SET state='interrupted',error='Panel stopped during execution. Inspect the retained staging data and recovery report before retrying.',finished_at=? WHERE state='running'", time.Now().Unix())
	if err != nil {
		return err
	}
	for range workers {
		e.wg.Add(1)
		go func() { defer e.wg.Done(); e.worker(ctx) }()
	}
	return nil
}
func (e *Engine) Wait() { e.wg.Wait() }

func (e *Engine) claim(ctx context.Context) (Job, error) {
	tx, err := e.DB.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, err
	}
	defer tx.Rollback()
	j, err := scan(tx.QueryRowContext(ctx, "SELECT "+jobColumns+" FROM jobs j WHERE state='queued' AND available_at<=? AND NOT EXISTS(SELECT 1 FROM jobs r WHERE r.state='running' AND (r.instance_id=j.instance_id OR r.instance_id='' OR j.instance_id='')) ORDER BY priority DESC,created_at,rowid LIMIT 1", time.Now().Unix()))
	if err != nil {
		return j, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE jobs SET state='running',started_at=?,attempts=attempts+1 WHERE id=?", time.Now().Unix(), j.ID); err != nil {
		return j, err
	}
	j.State = "running"
	j.Attempts++
	return j, tx.Commit()
}

func (e *Engine) worker(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		j, err := e.claim(ctx)
		if err != nil {
			if !errors.Is(err, sql.ErrNoRows) && ctx.Err() == nil {
				slog.Error("claim job", "error", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				continue
			case <-e.wake:
				continue
			}
		}
		e.run(ctx, j)
	}
}

func (e *Engine) run(ctx context.Context, j Job) {
	registration, ok := e.handlers[j.Kind]
	var result any
	var err error
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				err = fmt.Errorf("job failed unexpectedly; inspect panel logs")
				slog.Error("job panic", "job", j.ID, "panic", recovered)
			}
		}()
		if !ok {
			err = errors.New("job handler is unavailable")
			return
		}
		result, err = registration.run(ctx, &Task{Job: j, engine: e}, j.Payload)
	}()
	finishCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	state, message := "succeeded", ""
	progress := 100
	if err != nil {
		state = "failed"
		message = err.Error()
		progress = j.Progress
		if errors.Is(err, context.Canceled) {
			state = "cancelled"
		}
		if ctx.Err() != nil {
			state = "interrupted"
		}
	}
	if len(message) > 4096 {
		message = message[:4096]
	}
	if err != nil && ctx.Err() == nil && !errors.Is(err, context.Canceled) && j.Attempts <= registration.retries {
		delay := min(1<<j.Attempts, 300)
		_, dbErr := e.DB.ExecContext(finishCtx, "UPDATE jobs SET state='queued',error=?,available_at=?,cancellable=1 WHERE id=? AND cancel_requested=0", message, time.Now().Add(time.Duration(delay)*time.Second).Unix(), j.ID)
		if dbErr != nil {
			slog.Error("retry job", "error", dbErr)
		}
		e.signal()
		return
	}
	b, encodeErr := json.Marshal(result)
	if encodeErr != nil {
		state = "failed"
		message = "could not encode job result"
		b = []byte(`null`)
	}
	_, dbErr := e.DB.ExecContext(finishCtx, "UPDATE jobs SET state=?,error=?,result=?,progress=CASE WHEN ?='succeeded' THEN 100 ELSE progress END,finished_at=? WHERE id=?", state, message, string(b), state, time.Now().Unix(), j.ID)
	_ = progress
	if dbErr != nil {
		slog.Error("finish job", "job", j.ID, "error", dbErr)
	}
	e.signal()
}
