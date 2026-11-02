package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/gabrielpgava/newempire-server-control/internal/database"
	"github.com/gabrielpgava/newempire-server-control/internal/jobs"
	"github.com/gabrielpgava/newempire-server-control/internal/model"
	"github.com/gabrielpgava/newempire-server-control/internal/panelupdate"
	"github.com/gabrielpgava/newempire-server-control/internal/scheduler"
	"github.com/gabrielpgava/newempire-server-control/internal/updates"
	"github.com/go-chi/chi/v5"
)

type PanelPolicy struct {
	Mode          string          `json:"mode"`
	Channel       updates.Channel `json:"channel"`
	IntervalHours int             `json:"interval_hours"`
	WindowStart   string          `json:"window_start"`
	WindowEnd     string          `json:"window_end"`
	Timezone      string          `json:"timezone"`
	PostponeUntil int64           `json:"postpone_until,omitempty"`
}

type panelUpdateFailure struct {
	Version    string `json:"version"`
	ReportTime int64  `json:"report_time"`
}

func defaultPanelPolicy() PanelPolicy {
	return PanelPolicy{Mode: "notify", Channel: updates.Stable, IntervalHours: 24, WindowStart: "02:00", WindowEnd: "04:00", Timezone: "UTC"}
}
func (p PanelPolicy) Validate() error {
	if p.Mode != "disabled" && p.Mode != "notify" && p.Mode != "automatic" {
		return errors.New("invalid application update policy")
	}
	if p.Channel != updates.Stable && p.Channel != updates.Beta {
		return errors.New("invalid release channel")
	}
	if p.IntervalHours < 1 || p.IntervalHours > 168 {
		return errors.New("update interval must be between 1 and 168 hours")
	}
	_, err := scheduler.InWindow(time.Now(), p.Timezone, p.WindowStart, p.WindowEnd)
	return err
}
func (a *App) updateManager() (*updates.Manager, error) {
	if a.Config.ReleaseRepository != updates.Repository {
		return nil, errors.New("release repository does not match this build's official identity")
	}
	return updates.New(updates.Options{InstallDir: a.Config.InstallDir, CurrentVersion: strings.TrimPrefix(a.Version, "v"), Schema: database.SchemaVersion, PublicKey: a.Config.ReleasePublicKey})
}
func (a *App) updatesStatus(w http.ResponseWriter, r *http.Request) {
	p := defaultPanelPolicy()
	if err := a.setting(r.Context(), "updates-policy", &p); err != nil {
		fail(w, 500, err)
		return
	}
	var available updates.CheckResult
	if err := a.setting(r.Context(), "updates-available", &available); err != nil {
		fail(w, 500, err)
		return
	}
	out := map[string]any{"current_version": a.Version, "repository": updates.Repository, "available": nil, "state": "idle", "history": []updates.Event{}, "policy": p}
	if available.Available != nil {
		size := int64(0)
		for _, asset := range available.Available.Assets {
			if asset.Arch == runtime.GOARCH {
				size = asset.Size
			}
		}
		out["available"] = map[string]any{"version": available.Available.Version, "notes": available.Notes, "size": size, "prerelease": available.Prerelease}
	}
	report, err := a.updateReport(r.Context())
	if err == nil {
		out["history"] = report.Journal.History
		out["state"] = report.State
		if report.Journal.Transaction != nil && report.State != "running" {
			out["state"] = report.Journal.Transaction.State
		}
		if report.State == "failed" {
			if t := report.Journal.Transaction; t == nil || t.State != updates.Failed && t.State != updates.RolledBack && t.State != updates.ManualRecovery {
				out["state"] = "failed"
			}
			out["detail"] = report.Error
		}
		if report.State == "running" {
			out["detail"] = "Independent updater is executing the requested operation"
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		out["state"] = "unavailable"
		out["detail"] = err.Error()
	}
	if _, err := a.updateManager(); err != nil {
		out["state"] = "unavailable"
		out["detail"] = err.Error()
	}
	respond(w, 200, out)
}
func (a *App) updatePolicy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PanelPolicy
		PostponeUntil *int64 `json:"postpone_until"`
	}
	if err := decode(r, &body); err != nil {
		fail(w, 400, err)
		return
	}
	p := body.PanelPolicy
	if body.PostponeUntil != nil {
		p.PostponeUntil = *body.PostponeUntil
	}
	if err := p.Validate(); err != nil {
		fail(w, 400, err)
		return
	}
	raw, err := json.Marshal(p)
	if err == nil {
		// Preserve concurrent postponements unless the owner explicitly replaces the deadline.
		err = a.DB.QueryRowContext(r.Context(), `INSERT INTO settings(key,value) VALUES('updates-policy',?)
			ON CONFLICT(key) DO UPDATE SET value=CASE WHEN ? THEN excluded.value
			ELSE json_set(excluded.value,'$.postpone_until',coalesce(json_extract(settings.value,'$.postpone_until'),0)) END
			RETURNING value`, string(raw), body.PostponeUntil != nil).Scan(&raw)
	}
	if err == nil {
		err = json.Unmarshal(raw, &p)
	}
	if err != nil {
		fail(w, 500, err)
		return
	}
	a.audit(r.Context(), session(r).User.ID, "panel.update_policy", "", p.Mode+"/"+string(p.Channel))
	respond(w, 200, p)
}
func (a *App) updateAction(w http.ResponseWriter, r *http.Request) {
	action := chi.URLParam(r, "action")
	var body struct {
		Confirm bool `json:"confirm"`
		Hours   int  `json:"hours,omitempty"`
	}
	if err := decode(r, &body); err != nil {
		fail(w, 400, err)
		return
	}
	if action == "postpone" {
		if body.Hours < 1 || body.Hours > 168 {
			fail(w, 400, errors.New("postpone duration must be 1 to 168 hours"))
			return
		}
		p := defaultPanelPolicy()
		p.PostponeUntil = time.Now().Add(time.Duration(body.Hours) * time.Hour).Unix()
		raw, err := json.Marshal(p)
		if err == nil {
			err = a.DB.QueryRowContext(r.Context(), `INSERT INTO settings(key,value) VALUES('updates-policy',?)
				ON CONFLICT(key) DO UPDATE SET value=json_set(settings.value,'$.postpone_until',?)
				RETURNING value`, string(raw), p.PostponeUntil).Scan(&raw)
		}
		if err == nil {
			err = json.Unmarshal(raw, &p)
		}
		if err != nil {
			fail(w, 500, err)
			return
		}
		respond(w, 200, p)
		return
	}
	if action != "check" && action != "stage" && action != "apply" && action != "rollback" {
		fail(w, 400, errors.New("unsupported panel update action"))
		return
	}
	if (action == "apply" || action == "rollback") && !body.Confirm {
		fail(w, 400, errors.New("panel binary/state changes require explicit confirmation"))
		return
	}
	a.enqueue(w, r, "", "panel_"+action, map[string]any{"requested_by": session(r).User.ID})
}
func (a *App) handoffUpdate(ctx context.Context, t *jobs.Task, action, version string, channel updates.Channel) error {
	if runtime.GOOS != "linux" {
		return errors.New("independent panel updates require the native Linux installation")
	}
	root, err := os.OpenRoot(a.Config.DataDir)
	if err != nil {
		return err
	}
	defer root.Close()
	req := panelupdate.Request{ID: model.NewID(), JobID: t.Job.ID, Action: action, Version: version, Channel: channel, RequestedBy: "authenticated panel job"}
	if err := panelupdate.WriteRequest(ctx, root, req); err != nil {
		return err
	}
	if err := t.CommitBarrier(ctx); err != nil {
		return err
	}
	waitCtx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	return waitUpdate(waitCtx, root, req.ID, func() error {
		command := exec.CommandContext(waitCtx, "/usr/bin/sudo", "-n", a.Config.RuntimeHelper, "panel-update")
		command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C"}
		if err := command.Run(); err != nil {
			return errors.New("could not start the restricted independent updater; check the helper and systemd oneshot")
		}
		return nil
	})
}
func waitUpdate(ctx context.Context, root *os.Root, requestID string, start func() error) error {
	if err := start(); err != nil {
		return err
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		report, err := panelupdate.ReadReport(ctx, root)
		if err == nil && report.RequestID == requestID {
			if report.State == "succeeded" {
				return nil
			}
			if report.State == "failed" {
				return errors.New(report.Error)
			}
		}
		if err == nil && report.RequestID != requestID && report.State != "running" {
			// Starting an active oneshot does not queue the next request while it exits.
			if err := start(); err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return errors.New("independent updater did not report completion; inspect its retained journal")
		case <-ticker.C:
		}
	}
}
func (a *App) reconcileUpdates(ctx context.Context) {
	report, err := a.updateReport(ctx)
	if err != nil || report.State == "running" {
		return
	}
	if err := a.rememberUpdateFailure(ctx, report); err != nil {
		return
	}
	if report.Action != "stage" && report.Action != "apply" && report.Action != "rollback" {
		return
	}
	state := "succeeded"
	if report.State == "failed" {
		state = "failed"
	}
	b, _ := json.Marshal(report.Journal)
	finalCheck := report.Action == "apply" || report.Action == "stage" && report.State == "failed"
	a.DB.ExecContext(ctx, "UPDATE jobs SET state=?,error=?,result=?,progress=CASE WHEN ?='succeeded' THEN 100 ELSE progress END,finished_at=? WHERE id=? AND state='interrupted' AND (kind=? OR (kind='panel_check' AND ?))", state, report.Error, string(b), state, time.Now().Unix(), report.JobID, "panel_"+report.Action, finalCheck)
}
func (a *App) updateReport(ctx context.Context) (panelupdate.Report, error) {
	root, err := os.OpenRoot(a.Config.DataDir)
	if err != nil {
		return panelupdate.Report{}, err
	}
	defer root.Close()
	return panelupdate.ReadReport(ctx, root)
}
func (a *App) rememberUpdateFailure(ctx context.Context, report panelupdate.Report) error {
	if report.State == "failed" && report.Version != "" && (report.Action == "stage" || report.Action == "apply" || report.Action == "recover") {
		at := report.UpdatedAt.UnixNano()
		b, err := json.Marshal(panelUpdateFailure{Version: report.Version, ReportTime: at})
		if err != nil {
			return err
		}
		// Re-reading an old root report must not undo a manual reset or a newer local failure.
		_, err = a.DB.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES('updates-failure',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value WHERE coalesce(json_extract(settings.value,'$.report_time'),0)<?", string(b), at)
		return err
	}
	return nil
}
func (a *App) setFailedUpdateVersion(ctx context.Context, version string) error {
	b, err := json.Marshal(panelUpdateFailure{Version: version})
	if err != nil {
		return err
	}
	_, err = a.DB.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES('updates-failure',?) ON CONFLICT(key) DO UPDATE SET value=json_set(settings.value,'$.version',?)", string(b), version)
	return err
}
func (a *App) automaticUpdateAllowed(ctx context.Context, channel updates.Channel, now time.Time) (bool, error) {
	p := defaultPanelPolicy()
	if err := a.setting(ctx, "updates-policy", &p); err != nil {
		return false, err
	}
	if err := p.Validate(); err != nil {
		return false, err
	}
	inside, err := scheduler.InWindow(now, p.Timezone, p.WindowStart, p.WindowEnd)
	return p.Mode == "automatic" && p.Channel == channel && inside && now.Unix() >= p.PostponeUntil, err
}
func (a *App) automaticPanelUpdate(ctx context.Context, version string, channel updates.Channel, now func() time.Time, handoff func(string, string) error) error {
	allowed, err := a.automaticUpdateAllowed(ctx, channel, now())
	if err != nil || !allowed {
		return err
	}
	report, err := a.updateReport(ctx)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err == nil && report.State == "running" {
		return errors.New("independent updater is still running; inspect its report before another automatic operation")
	}
	if err := a.rememberUpdateFailure(ctx, report); err != nil {
		return err
	}
	var failed panelUpdateFailure
	if err := a.setting(ctx, "updates-failure", &failed); err != nil {
		return err
	}
	if failed.Version == version {
		return errors.New("this release previously failed automatic maintenance; administrator review and a successful manual stage are required")
	}
	run := func(action string) error {
		err := handoff(action, version)
		if err != nil && ctx.Err() == nil {
			err = errors.Join(err, a.setFailedUpdateVersion(ctx, version))
		}
		return err
	}
	t := report.Journal.Transaction
	if report.State != "succeeded" || t == nil || t.State != updates.Staged || t.Version != version || t.Channel != channel || t.PreviousVersion != strings.TrimPrefix(a.Version, "v") {
		if err := run("stage"); err != nil {
			return err
		}
	}
	allowed, err = a.automaticUpdateAllowed(ctx, channel, now())
	if err != nil || !allowed {
		return err
	}
	return run("apply")
}
func (a *App) registerUpdateJobs() {
	a.Jobs.Register("panel_check", 0, func(ctx context.Context, t *jobs.Task, _ json.RawMessage) (any, error) {
		p := defaultPanelPolicy()
		if err := a.setting(ctx, "updates-policy", &p); err != nil {
			return nil, err
		}
		manager, err := a.updateManager()
		if err != nil {
			return nil, err
		}
		if err := t.Progress(ctx, 10, "Checking official GitHub Releases and authenticated metadata"); err != nil {
			return nil, err
		}
		result, err := manager.Check(ctx, p.Channel)
		if err != nil {
			return nil, err
		}
		if err := a.saveSetting(ctx, "updates-available", result); err != nil {
			return nil, err
		}
		if result.Available != nil {
			a.notify(ctx, "update_available", "", t.Job.ID)
			return result, a.automaticPanelUpdate(ctx, result.Available.Version, p.Channel, time.Now, func(action, version string) error {
				return a.handoffUpdate(ctx, t, action, version, p.Channel)
			})
		}
		return result, nil
	})
	for _, action := range []string{"stage", "apply", "rollback"} {
		action := action
		a.Jobs.Register("panel_"+action, 0, func(ctx context.Context, t *jobs.Task, _ json.RawMessage) (any, error) {
			p := defaultPanelPolicy()
			if err := a.setting(ctx, "updates-policy", &p); err != nil {
				return nil, err
			}
			selected := ""
			if action == "stage" {
				manager, err := a.updateManager()
				if err != nil {
					return nil, err
				}
				result, err := manager.Check(ctx, p.Channel)
				if err != nil {
					return nil, err
				}
				if result.Available == nil {
					return nil, errors.New("no newer verified release is available")
				}
				selected = result.Available.Version
				if err := a.saveSetting(ctx, "updates-available", result); err != nil {
					return nil, err
				}
			}
			if err := t.Progress(ctx, 10, "Handing the validated request to the independent panel-only updater"); err != nil {
				return nil, err
			}
			err := a.handoffUpdate(ctx, t, action, selected, p.Channel)
			if err == nil && action == "stage" {
				err = a.setFailedUpdateVersion(ctx, "")
			}
			if err != nil {
				a.notify(ctx, "update_failed", "", t.Job.ID)
			} else {
				a.notify(ctx, "update_succeeded", "", t.Job.ID)
			}
			return map[string]string{"operation": action, "version": selected}, err
		})
	}
}
func (a *App) localHealth(w http.ResponseWriter, r *http.Request) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(host)
	if err != nil || ip == nil || !ip.IsLoopback() {
		fail(w, 403, errors.New("local authenticated health check required"))
		return
	}
	token, err := os.ReadFile(a.Config.DataDir + "/health-token")
	want := strings.TrimSpace(string(token))
	if err != nil || len(want) != 43 || subtle.ConstantTimeCompare([]byte(want), []byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))) != 1 {
		fail(w, 403, errors.New("local authenticated health check required"))
		return
	}
	if err := a.DB.PingContext(r.Context()); err != nil {
		fail(w, 503, errors.New("database is not ready"))
		return
	}
	respond(w, 200, map[string]any{"status": "ok", "version": strings.TrimPrefix(a.Version, "v"), "schema": database.SchemaVersion})
}
