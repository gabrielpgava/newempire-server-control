package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/gabrielpgava/newempire-server-control/internal/jobs"
	"github.com/gabrielpgava/newempire-server-control/internal/model"
	"github.com/gabrielpgava/newempire-server-control/internal/scheduler"
	"github.com/gabrielpgava/newempire-server-control/internal/updates"
)

type ContentPolicy struct {
	Mode            string `json:"mode"`
	IntervalHours   int    `json:"interval_hours"`
	WindowStart     string `json:"window_start"`
	WindowEnd       string `json:"window_end"`
	Timezone        string `json:"timezone"`
	PostponePlayers bool   `json:"postpone_players"`
}

func defaultContentPolicy() ContentPolicy {
	return ContentPolicy{Mode: "manual", IntervalHours: 24, WindowStart: "02:00", WindowEnd: "04:00", Timezone: "UTC", PostponePlayers: true}
}
func (p ContentPolicy) Validate() error {
	if p.Mode != "manual" && p.Mode != "notify" && p.Mode != "automatic-safe" {
		return errors.New("invalid content update policy")
	}
	if p.IntervalHours < 1 || p.IntervalHours > 168 {
		return errors.New("check interval must be between 1 and 168 hours")
	}
	_, err := scheduler.InWindow(time.Now(), p.Timezone, p.WindowStart, p.WindowEnd)
	return err
}

func (a *App) background(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	lastMetrics := time.Time{}
	for {
		now := time.Now()
		a.reconcileUpdates(ctx)
		if err := a.Schedules.Tick(ctx, now); err != nil && ctx.Err() == nil {
			slog.Error("scheduler tick", "error", err)
		}
		if now.Sub(lastMetrics) >= time.Duration(max(a.Config.MetricsIntervalSeconds, 30))*time.Second {
			a.sampleMetrics(ctx, now)
			lastMetrics = now
		}
		if err := a.pollPolicies(ctx, now); err != nil && ctx.Err() == nil {
			slog.Error("automation policy check", "error", err)
		}
		if err := a.deliverNotifications(ctx, now); err != nil && ctx.Err() == nil {
			slog.Warn("notification delivery deferred", "error", err)
		}
		if a.Uploads != nil {
			if err := a.Uploads.Prune(ctx); err != nil && ctx.Err() == nil {
				slog.Warn("upload expiry cleanup deferred", "error", err)
			}
		}
		if _, err := a.DB.ExecContext(ctx, "DELETE FROM sessions WHERE expires_at<?", now.Unix()); err != nil && ctx.Err() == nil {
			slog.Warn("expire sessions", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *App) sampleMetrics(ctx context.Context, now time.Time) {
	all, err := a.Instances.List(ctx)
	if err != nil {
		return
	}
	for _, i := range all {
		instance, err := a.Instances.Get(ctx, i.ID)
		if err != nil {
			continue
		}
		sampleCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		status, err := a.runtimeStatus(sampleCtx, instance)
		cancel()
		if err != nil {
			continue
		}
		var previous string
		a.DB.QueryRowContext(ctx, "SELECT state FROM metrics WHERE instance_id=? ORDER BY time DESC LIMIT 1", i.ID).Scan(&previous)
		if _, err := a.DB.ExecContext(ctx, "INSERT OR REPLACE INTO metrics(instance_id,time,state,memory_bytes,cpu_seconds) VALUES(?,?,?,?,?)", i.ID, now.Unix(), status.State, status.MemoryBytes, status.CPUSeconds); err != nil {
			continue
		}
		if root, err := a.Instances.OpenRoot(i.Root); err == nil {
			available, diskErr := freeDisk(root)
			root.Close()
			if diskErr == nil {
				var warned bool
				a.setting(ctx, "disk-alert:"+i.ID, &warned)
				low := available < a.Config.DiskWarningBytes
				if low && !warned {
					a.notify(ctx, "storage_threshold", i.ID, "")
				}
				a.saveSetting(ctx, "disk-alert:"+i.ID, low)
			}
		}
		if previous == "ready" && (status.State == "crashed" || status.State == "offline") {
			var running int
			a.DB.QueryRowContext(ctx, "SELECT count(*) FROM jobs WHERE instance_id=? AND state='running'", i.ID).Scan(&running)
			if running == 0 {
				a.notify(ctx, "server_crashed", i.ID, "")
			}
		}
	}
	a.DB.ExecContext(ctx, "DELETE FROM metrics WHERE time<?", now.Add(-time.Duration(max(a.Config.MetricsRetentionDays, 1))*24*time.Hour).Unix())
}

func (a *App) pollPolicies(ctx context.Context, now time.Time) error {
	all, err := a.Instances.List(ctx)
	if err != nil {
		return err
	}
	for _, i := range all {
		retention := retentionPolicy{KeepLast: 7}
		if err := a.setting(ctx, "backup-retention", &retention); err != nil {
			return err
		}
		if retention.Enabled {
			var next int64
			if err := a.setting(ctx, "retention-next:"+i.ID, &next); err != nil {
				return err
			}
			if next <= now.Unix() {
				if _, err := a.Jobs.Enqueue(ctx, i.ID, "retention", map[string]any{}, fmt.Sprintf("retention:%s:%d", i.ID, now.Unix()/86400), -40); err != nil {
					return err
				}
				if err := a.saveSetting(ctx, "retention-next:"+i.ID, now.Add(24*time.Hour).Unix()); err != nil {
					return err
				}
			}
		}
		p := defaultContentPolicy()
		if err := a.setting(ctx, "mods-policy:"+i.ID, &p); err != nil {
			return err
		}
		if err := p.Validate(); err != nil {
			return err
		}
		if p.Mode == "manual" {
			continue
		}
		var checked int64
		if err := a.setting(ctx, "mods-next:"+i.ID, &checked); err != nil {
			return err
		}
		inside, err := scheduler.InWindow(now, p.Timezone, p.WindowStart, p.WindowEnd)
		if err != nil {
			return err
		}
		var pending bool
		if err := a.setting(ctx, "mods-pending:"+i.ID, &pending); err != nil {
			return err
		}
		maintenance := p.Mode == "automatic-safe" && inside && pending
		if checked > now.Unix() && !maintenance {
			continue
		}
		key := fmt.Sprintf("policy-mods:%s:%d", i.ID, now.Unix()/int64(p.IntervalHours*3600))
		if maintenance {
			key = fmt.Sprintf("maintenance-mods:%s:%d", i.ID, now.Unix()/900)
		}
		if _, err := a.Jobs.Enqueue(ctx, i.ID, "mod_check", map[string]any{"scheduled": true}, key, -20); err != nil {
			return err
		}
		if err := a.saveSetting(ctx, "mods-next:"+i.ID, now.Add(time.Duration(p.IntervalHours)*time.Hour).Unix()); err != nil {
			return err
		}
	}
	updatePolicy := defaultPanelPolicy()
	if err := a.setting(ctx, "updates-policy", &updatePolicy); err != nil {
		return err
	}
	if err := updatePolicy.Validate(); err != nil {
		return err
	}
	if updatePolicy.Mode != "" && updatePolicy.Mode != "disabled" {
		if _, err := a.updateManager(); err != nil {
			return nil
		}
		var next int64
		if err := a.setting(ctx, "updates-next", &next); err != nil {
			return err
		}
		var available updates.CheckResult
		if err := a.setting(ctx, "updates-available", &available); err != nil {
			return err
		}
		inside, err := scheduler.InWindow(now, updatePolicy.Timezone, updatePolicy.WindowStart, updatePolicy.WindowEnd)
		if err != nil {
			return err
		}
		maintenance := updatePolicy.Mode == "automatic" && inside && now.Unix() >= updatePolicy.PostponeUntil && available.Available != nil
		if next <= now.Unix() || maintenance {
			key := fmt.Sprintf("panel-check:%d", now.Unix()/int64(max(updatePolicy.IntervalHours, 1)*3600))
			if maintenance {
				key = fmt.Sprintf("panel-maintenance:%d", now.Unix()/900)
			}
			if _, err := a.Jobs.Enqueue(ctx, "", "panel_check", map[string]any{"scheduled": true}, key, -30); err != nil {
				return err
			}
			if err := a.saveSetting(ctx, "updates-next", now.Add(time.Duration(max(updatePolicy.IntervalHours, 1))*time.Hour).Unix()); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *App) registerLifecycleJobs() {
	for _, kind := range []string{"start", "stop", "restart"} {
		kind := kind
		a.Jobs.Register(kind, 0, func(ctx context.Context, t *jobs.Task, _ json.RawMessage) (any, error) {
			i, err := a.Instances.Get(ctx, t.Job.InstanceID)
			if err != nil {
				return nil, err
			}
			if err := t.Progress(ctx, 10, "Validating the instance runtime"); err != nil {
				return nil, err
			}
			if err := t.CommitBarrier(ctx); err != nil {
				return nil, err
			}
			if kind == "stop" || kind == "restart" {
				if err := a.stopMinecraft(ctx, i); err != nil {
					return nil, err
				}
			}
			if kind != "stop" {
				if err := a.prepareRuntimeStart(ctx, i); err != nil {
					return nil, err
				}
				if err := a.recoverPreparedContent(ctx, i); err != nil {
					return nil, err
				}
				err = a.Runtime.Start(ctx, i)
			}
			if err != nil {
				a.notify(ctx, "job_failed", i.ID, t.Job.ID)
				return nil, a.recoverFailedStart(ctx, i, err)
			}
			if kind == "stop" {
				a.notify(ctx, "server_stopped", i.ID, t.Job.ID)
				return map[string]any{"state": "offline"}, nil
			}
			if err := t.Progress(ctx, 40, "Process launched; waiting for Minecraft readiness"); err != nil {
				return nil, err
			}
			status, err := a.awaitReady(ctx, i.ID, 3*time.Minute)
			if err != nil {
				return status, a.recoverFailedStart(ctx, i, err)
			}
			if err := a.pendingContent(ctx, i, false); err != nil {
				return status, err
			}
			if err := a.completeRuntimeStart(ctx, i, true); err != nil {
				return status, err
			}
			if err := a.saveSetting(ctx, "properties-dirty:"+i.ID, false); err != nil {
				return status, err
			}
			a.notify(ctx, "server_started", i.ID, t.Job.ID)
			return status, err
		})
	}
	a.Jobs.Register("console", 0, func(ctx context.Context, t *jobs.Task, payload json.RawMessage) (any, error) {
		var body struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(payload, &body); err != nil {
			return nil, err
		}
		i, err := a.Instances.Get(ctx, t.Job.InstanceID)
		if err != nil {
			return nil, err
		}
		if err := t.CommitBarrier(ctx); err != nil {
			return nil, err
		}
		output, err := a.runtimeCommand(ctx, i, body.Command)
		return map[string]string{"output": output}, err
	})
}

func (a *App) recoverFailedStart(ctx context.Context, i model.Instance, startErr error) error {
	recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer cancel()
	runtimeErr := a.completeRuntimeStart(recoveryCtx, i, false)
	root, openErr := a.Instances.OpenRoot(i.Root)
	if openErr != nil {
		return errors.Join(startErr, runtimeErr, openErr)
	}
	transactions, scanErr := contentTransactions(root)
	root.Close()
	if scanErr != nil {
		return errors.Join(startErr, runtimeErr, scanErr)
	}
	if len(transactions) > 0 {
		if err := a.stopMinecraft(recoveryCtx, i); err != nil {
			return errors.Join(startErr, runtimeErr, err)
		}
		return errors.Join(startErr, runtimeErr, a.pendingContent(recoveryCtx, i, true))
	}
	return errors.Join(startErr, runtimeErr)
}

func (a *App) awaitReady(ctx context.Context, id string, timeout time.Duration) (any, error) {
	i, err := a.Instances.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	timer := time.NewTicker(2 * time.Second)
	defer timer.Stop()
	for {
		status, err := a.runtimeStatus(ctx, i)
		if err == nil && status.Ready {
			return status, nil
		}
		if err == nil && status.State == "crashed" {
			return status, errors.New("Minecraft exited before readiness; inspect the console and restore point")
		}
		select {
		case <-ctx.Done():
			return nil, errors.New("Minecraft did not become ready within the health-check window; inspect its logs")
		case <-timer.C:
		}
	}
}
