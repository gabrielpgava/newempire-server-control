package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gabrielpgava/newempire-server-control/internal/config"
	"github.com/gabrielpgava/newempire-server-control/internal/database"
	"github.com/gabrielpgava/newempire-server-control/internal/jobs"
	"github.com/gabrielpgava/newempire-server-control/internal/model"
	"github.com/gabrielpgava/newempire-server-control/internal/panelupdate"
	"github.com/gabrielpgava/newempire-server-control/internal/updates"
)

func updateFixture(t *testing.T) *App {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("trusted report fixture requires isolated root")
	}
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	c := config.Default()
	c.DataDir = dir
	c.InstallDir = filepath.Join(dir, "install")
	c.ReleasePublicKey = strings.Repeat("A", 43) + "="
	return &App{Config: c, DB: db, Jobs: jobs.New(db), Version: "1.0.0"}
}

func publishUpdate(t *testing.T, a *App, report panelupdate.Report) {
	t.Helper()
	root, err := os.OpenRoot(a.Config.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if report.RequestID == "" {
		report.RequestID = model.NewID()
	}
	if report.JobID == "" {
		report.JobID = model.NewID()
	}
	if err := panelupdate.Publish(context.Background(), root, report); err != nil {
		t.Fatal(err)
	}
}

func automaticPolicy(t *testing.T, a *App) PanelPolicy {
	t.Helper()
	p := defaultPanelPolicy()
	p.Mode, p.WindowStart, p.WindowEnd = "automatic", "02:00", "04:00"
	if err := a.saveSetting(context.Background(), "updates-policy", p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAutomaticUpdateReusesStagedReleaseAndRechecksPolicy(t *testing.T) {
	for _, change := range []string{"none", "expired window", "disabled", "postponed", "channel changed"} {
		t.Run(change, func(t *testing.T) {
			a := updateFixture(t)
			p := automaticPolicy(t, a)
			now := time.Date(2026, 1, 1, 3, 59, 0, 0, time.UTC)
			calls := []string{}
			stage := func(action, version string) error {
				calls = append(calls, action)
				if version != "1.1.0" {
					t.Fatal("operation lost the selected release", version)
				}
				if action == "stage" {
					publishUpdate(t, a, panelupdate.Report{Action: "stage", Version: version, State: "succeeded", Journal: updates.Journal{Transaction: &updates.Transaction{Version: version, PreviousVersion: "1.0.0", Channel: updates.Stable, State: updates.Staged}}})
					switch change {
					case "expired window":
						now = now.Add(2 * time.Minute)
					case "disabled":
						p.Mode = "disabled"
					case "postponed":
						p.PostponeUntil = now.Add(time.Hour).Unix()
					case "channel changed":
						p.Channel = updates.Beta
					}
					return a.saveSetting(context.Background(), "updates-policy", p)
				}
				return nil
			}
			clock := func() time.Time { return now }
			if err := a.automaticPanelUpdate(context.Background(), "1.1.0", updates.Stable, clock, stage); err != nil {
				t.Fatal(err)
			}
			want := []string{"stage"}
			if change == "none" {
				want = append(want, "apply")
			}
			if !reflect.DeepEqual(calls, want) {
				t.Fatal("maintenance ignored the post-staging policy", calls, want)
			}
			if change != "none" {
				now = time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
				p = automaticPolicy(t, a)
				calls = nil
				if err := a.automaticPanelUpdate(context.Background(), "1.1.0", updates.Stable, clock, stage); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(calls, []string{"apply"}) {
					t.Fatal("matching staged artifact was downloaded again", calls)
				}
			}
		})
	}
}

func TestAutomaticUpdateLatchesFailuresIncludingAfterRestart(t *testing.T) {
	for _, failedAt := range []string{"stage", "apply", "reported rollback"} {
		t.Run(failedAt, func(t *testing.T) {
			a := updateFixture(t)
			automaticPolicy(t, a)
			clock := func() time.Time { return time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC) }
			if failedAt == "reported rollback" {
				publishUpdate(t, a, panelupdate.Report{Action: "apply", Version: "1.1.0", State: "failed", Error: "readiness failed and rolled back", Journal: updates.Journal{Transaction: &updates.Transaction{Version: "1.1.0", State: updates.RolledBack}}})
			}
			calls := 0
			run := func(action, _ string) error {
				calls++
				if action == failedAt {
					return errors.New("controlled updater failure")
				}
				return nil
			}
			if err := a.automaticPanelUpdate(context.Background(), "1.1.0", updates.Stable, clock, run); err == nil {
				t.Fatal("failure was hidden")
			}
			before := calls
			if err := a.automaticPanelUpdate(context.Background(), "1.1.0", updates.Stable, clock, run); err == nil || calls != before {
				t.Fatal("failed version retried automatically", calls, err)
			}
			if failedAt == "reported rollback" && calls != 0 {
				t.Fatal("restored database lost the failed-version latch")
			}
			if err := a.automaticPanelUpdate(context.Background(), "1.2.0", updates.Stable, clock, func(string, string) error { calls++; return nil }); err != nil || calls == before {
				t.Fatal("failure incorrectly blocked a different release", err)
			}
		})
	}
}

func TestOldFailureReportCannotUndoNewerFailureOrManualReset(t *testing.T) {
	a := updateFixture(t)
	publishUpdate(t, a, panelupdate.Report{Action: "apply", Version: "1.1.0", State: "failed", Error: "old failure"})
	report, err := a.updateReport(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := a.rememberUpdateFailure(context.Background(), report); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"1.2.0", ""} {
		if err := a.setFailedUpdateVersion(context.Background(), version); err != nil {
			t.Fatal(err)
		}
		if err := a.rememberUpdateFailure(context.Background(), report); err != nil {
			t.Fatal(err)
		}
		var failure panelUpdateFailure
		if err := a.setting(context.Background(), "updates-failure", &failure); err != nil || failure.Version != version {
			t.Fatal("old report replaced the newer failure/reset", failure, err)
		}
	}
}

func TestReconcileOnlyInterruptedFinalOperation(t *testing.T) {
	for _, entry := range []struct {
		kind, jobState, action, reportState, want string
	}{
		{"panel_check", "running", "stage", "succeeded", "running"},
		{"panel_check", "running", "apply", "succeeded", "running"},
		{"panel_check", "interrupted", "stage", "succeeded", "interrupted"},
		{"panel_check", "interrupted", "stage", "failed", "failed"},
		{"panel_check", "interrupted", "apply", "succeeded", "succeeded"},
		{"panel_apply", "interrupted", "apply", "failed", "failed"},
		{"panel_stage", "interrupted", "stage", "succeeded", "succeeded"},
		{"panel_apply", "interrupted", "stage", "succeeded", "interrupted"},
		{"panel_rollback", "interrupted", "apply", "succeeded", "interrupted"},
	} {
		t.Run(strings.Join([]string{entry.kind, entry.jobState, entry.action, entry.reportState}, "/"), func(t *testing.T) {
			a := updateFixture(t)
			a.Jobs.Register(entry.kind, 0, nil)
			job, err := a.Jobs.Enqueue(context.Background(), "", entry.kind, map[string]any{}, "", 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := a.DB.Exec("UPDATE jobs SET state=?,progress=10 WHERE id=?", entry.jobState, job.ID); err != nil {
				t.Fatal(err)
			}
			publishUpdate(t, a, panelupdate.Report{JobID: job.ID, Action: entry.action, Version: "1.1.0", State: entry.reportState, Error: "controlled report"})
			a.reconcileUpdates(context.Background())
			got, err := a.Jobs.Get(context.Background(), job.ID)
			if err != nil || got.State != entry.want || entry.want == "running" && got.Progress != 10 {
				t.Fatal("incorrect update reconciliation", got, err)
			}
		})
	}
}

func TestUpdateStatusUsesReportNotPrivateInstallJournal(t *testing.T) {
	a := updateFixture(t)
	publishUpdate(t, a, panelupdate.Report{Action: "stage", Version: "1.1.0", State: "succeeded", Journal: updates.Journal{Transaction: &updates.Transaction{Version: "1.1.0", State: updates.Staged}}})
	r := httptest.NewRequest(http.MethodGet, "/api/v1/updates", nil)
	w := httptest.NewRecorder()
	a.updatesStatus(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"staged"`) {
		t.Fatal("private or absent install journal masked the root report", w.Body.String())
	}
	publishUpdate(t, a, panelupdate.Report{Action: "stage", Version: "1.2.0", State: "failed", Error: "upstream failure before staging", Journal: updates.Journal{Transaction: &updates.Transaction{Version: "1.1.0", State: updates.Complete}}})
	w = httptest.NewRecorder()
	a.updatesStatus(w, r)
	if !strings.Contains(w.Body.String(), `"state":"failed"`) {
		t.Fatal("old completed transaction hid a new operation failure", w.Body.String())
	}
	if err := os.Chown(filepath.Join(a.Config.DataDir, panelupdate.ReportFile), 1000, 1000); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	a.updatesStatus(w, r)
	if !strings.Contains(w.Body.String(), `"state":"unavailable"`) {
		t.Fatal("untrusted report was displayed as idle/staged", w.Body.String())
	}
}

func TestHandoffIgnoresOldResultsAndReportsFailureOrTimeout(t *testing.T) {
	for _, state := range []string{"failed", "running", "missing"} {
		t.Run(state, func(t *testing.T) {
			a := updateFixture(t)
			id := model.NewID()
			if state != "missing" {
				publishUpdate(t, a, panelupdate.Report{RequestID: id, Action: "apply", State: state, Error: "controlled root failure"})
			}
			root, err := os.OpenRoot(a.Config.DataDir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			err = waitUpdate(ctx, root, id, func() error { return nil })
			if err == nil || state == "failed" && !strings.Contains(err.Error(), "controlled root failure") {
				t.Fatal("handoff lost its failure/timeout diagnostic", err)
			}
		})
	}
}

func TestHandoffRetriesStartWhenPreviousOneshotIsExiting(t *testing.T) {
	a := updateFixture(t)
	publishUpdate(t, a, panelupdate.Report{Action: "stage", State: "succeeded"})
	root, err := os.OpenRoot(a.Config.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	id := model.NewID()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	starts := 0
	err = waitUpdate(ctx, root, id, func() error {
		starts++
		if starts == 2 {
			publishUpdate(t, a, panelupdate.Report{RequestID: id, Action: "apply", State: "succeeded"})
		}
		return nil
	})
	if err != nil || starts != 2 {
		t.Fatal("next request was lost while the stage oneshot exited", starts, err)
	}
}
