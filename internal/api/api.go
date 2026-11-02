package api

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gabrielpgava/newempire-server-control/internal/auth"
	"github.com/gabrielpgava/newempire-server-control/internal/config"
	"github.com/gabrielpgava/newempire-server-control/internal/database"
	"github.com/gabrielpgava/newempire-server-control/internal/fabric"
	"github.com/gabrielpgava/newempire-server-control/internal/instances"
	"github.com/gabrielpgava/newempire-server-control/internal/jobs"
	"github.com/gabrielpgava/newempire-server-control/internal/model"
	"github.com/gabrielpgava/newempire-server-control/internal/modrinth"
	"github.com/gabrielpgava/newempire-server-control/internal/scheduler"
	"github.com/gabrielpgava/newempire-server-control/internal/secrets"
	"github.com/gabrielpgava/newempire-server-control/internal/uploads"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

type Runtime interface {
	Status(context.Context, model.Instance) (model.RuntimeStatus, error)
	Start(context.Context, model.Instance) error
	Stop(context.Context, model.Instance) error
	Restart(context.Context, model.Instance) error
	Logs(context.Context, model.Instance, int) ([]model.LogLine, error)
	Command(context.Context, model.Instance, string) (string, error)
}

type App struct {
	Config        config.Config
	DB            *database.Store
	Vault         *secrets.Vault
	Auth          *auth.Service
	Jobs          *jobs.Engine
	Instances     *instances.Service
	Schedules     *scheduler.Service
	Runtime       Runtime
	Version       string
	UI            http.Handler
	UploadHandler http.Handler
	Uploads       *uploads.Service
	Fabric        *fabric.Client
	Content       *modrinth.Service
	Provision     func(context.Context, model.Instance, bool) error
	Prepare       func(context.Context, model.Instance) error
	uploadRoot    *os.Root
	cancel        context.CancelFunc
	wg            sync.WaitGroup
}
type contextKey string

const sessionKey contextKey = "session"
const instanceKey contextKey = "instance"

func New(c config.Config, db *database.Store, vault *secrets.Vault, rt Runtime, version string, ui http.Handler) *App {
	e := jobs.New(db)
	a := &App{Config: c, DB: db, Vault: vault, Auth: auth.New(db, vault), Jobs: e, Instances: &instances.Service{DB: db, Vault: vault, Config: c}, Runtime: rt, Version: version, UI: ui}
	a.Schedules = &scheduler.Service{DB: db, Jobs: e}
	return a
}

func (a *App) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)
	r.Use(a.headers)
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) { respond(w, 200, map[string]string{"status": "ok"}) })
	r.Get("/internal/health", a.localHealth)
	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/setup", a.setupStatus)
		r.Post("/setup", a.setup)
		r.Post("/auth/login", a.login)
		r.Group(func(r chi.Router) {
			r.Use(a.authenticate)
			r.Get("/auth/session", func(w http.ResponseWriter, r *http.Request) { respond(w, 200, session(r)) })
			r.Post("/auth/logout", a.logout)
			r.Get("/health", a.health)
			r.Get("/events", a.events)
			r.Get("/instances", a.listInstances)
			r.With(a.require("admin")).Post("/instances", a.createInstance)
			r.With(a.require("admin")).Get("/adoptions", func(w http.ResponseWriter, r *http.Request) {
				respond(w, 200, map[string]any{"roots": a.Config.AdoptRoots})
			})
			r.With(a.require("admin")).Post("/adoptions/preview", a.previewAdoption)
			r.With(a.require("admin")).Post("/imports/preview", a.previewUpload)
			r.With(a.require("admin")).Get("/java", a.javaInstallations)
			r.With(a.require("admin")).Get("/fabric/versions", a.fabricVersions)
			r.Get("/jobs", a.listJobs)
			r.Get("/jobs/{jobID}", a.getJob)
			r.Post("/jobs/{jobID}/cancel", a.cancelJob)
			r.Get("/schedules", a.listSchedules)
			r.With(a.require("admin")).Post("/schedules", a.saveSchedule)
			r.With(a.require("admin")).Delete("/schedules/{scheduleID}", a.deleteSchedule)
			r.With(a.require("admin")).Get("/audit", a.auditList)
			r.With(a.require("owner")).Get("/users", a.users)
			r.With(a.require("owner")).Post("/users", a.createUser)
			r.With(a.require("owner")).Delete("/users/{userID}", a.revokeUser)
			r.Post("/auth/totp", a.enableTOTP)
			r.Delete("/auth/totp", a.disableTOTP)
			r.Post("/auth/password", a.changePassword)
			r.Get("/settings", a.settings)
			r.With(a.require("admin")).Put("/settings", a.saveSettings)
			r.With(a.require("admin")).Get("/mods/search", a.searchMods)
			r.With(a.require("admin")).Get("/updates", a.updatesStatus)
			r.With(a.require("owner")).Put("/updates/policy", a.updatePolicy)
			r.With(a.require("owner")).Post("/updates/{action}", a.updateAction)
			if a.UploadHandler != nil {
				r.With(a.require("admin")).Mount("/uploads", a.UploadHandler)
			}
			r.Route("/instances/{instanceID}", func(r chi.Router) {
				r.Use(a.loadInstance)
				r.Get("/", a.getInstance)
				r.Get("/status", a.status)
				r.Get("/logs", a.logs)
				r.Get("/metrics", a.metrics)
				r.With(a.require("operator")).Post("/actions/{action}", a.lifecycle)
				r.With(a.require("operator")).Post("/console", a.console)
				r.With(a.require("admin")).Get("/files", a.listFiles)
				r.With(a.require("admin")).Get("/file", a.downloadFile)
				r.With(a.require("admin")).Post("/files", a.mutateFiles)
				r.With(a.require("admin")).Get("/export", a.exportFiles)
				r.With(a.require("admin")).Post("/imports/preview", a.previewInstanceImport)
				r.With(a.require("admin")).Post("/imports/apply", a.applyInstanceImport)
				r.Get("/backups", a.listBackups)
				r.With(a.require("admin")).Get("/recovery", a.listRecovery)
				r.With(a.require("operator")).Post("/backups", a.createBackup)
				r.With(a.require("admin")).Get("/backups/{backupID}/download", a.downloadBackup)
				r.With(a.require("admin")).Get("/backups/{backupID}/preview", a.previewBackup)
				r.With(a.require("operator")).Post("/backups/{backupID}/verify", a.verifyBackup)
				r.With(a.require("admin")).Post("/backups/{backupID}/fetch-offsite", a.fetchOffsite)
				r.With(a.require("admin")).Post("/backups/{backupID}/restore", a.restoreBackup)
				r.Get("/properties", a.properties)
				r.With(a.require("admin")).Put("/properties", a.saveProperties)
				r.Get("/worlds", a.worlds)
				r.With(a.require("admin")).Post("/worlds", a.mutateWorlds)
				r.Get("/players", a.players)
				r.With(a.require("operator")).Post("/players", a.playerAction)
				r.Get("/mods", a.mods)
				r.With(a.require("admin")).Post("/mods/plan", a.planMods)
				r.With(a.require("admin")).Post("/mods/apply", a.applyMods)
				r.With(a.require("admin")).Post("/mods/manage", a.manageMods)
				r.With(a.require("admin")).Post("/modpacks/preview", a.previewModpack)
				r.With(a.require("admin")).Post("/modpacks/apply", a.applyModpack)
				r.With(a.require("admin")).Get("/modpacks/export", a.exportModpack)
				r.Get("/mods/policy", a.modPolicy)
				r.With(a.require("admin")).Put("/mods/policy", a.saveModPolicy)
				r.With(a.require("admin")).Post("/runtime/plan", a.planRuntime)
				r.With(a.require("admin")).Post("/runtime/apply", a.applyRuntime)
			})
		})
	})
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			fail(w, 404, errors.New("API endpoint not found"))
			return
		}
		if a.UI == nil {
			fail(w, 503, errors.New("frontend is not built"))
			return
		}
		a.UI.ServeHTTP(w, r)
	})
	return r
}

func (a *App) headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
		if a.Config.SecureCookies {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func (a *App) origin(r *http.Request) bool {
	return strings.TrimSuffix(r.Header.Get("Origin"), "/") == strings.TrimSuffix(a.Config.PublicURL, "/") && r.Header.Get("Origin") != ""
}
func (a *App) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("nec_session")
		if err != nil {
			fail(w, 401, errors.New("authentication required"))
			return
		}
		s, err := a.Auth.Session(r.Context(), cookie.Value)
		if err != nil {
			fail(w, 401, errors.New("session expired; sign in again"))
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" && r.Method != "OPTIONS" {
			if !a.origin(r) || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(s.CSRF)) != 1 {
				fail(w, 403, errors.New("request origin or CSRF token is invalid"))
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sessionKey, s)))
	})
}
func session(r *http.Request) auth.Session {
	s, _ := r.Context().Value(sessionKey).(auth.Session)
	return s
}
func (a *App) require(role string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !auth.Permitted(session(r).User.Role, role) {
				fail(w, 403, errors.New("your role does not permit this operation"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
func (a *App) canInstance(ctx context.Context, u auth.User, id string) bool {
	if auth.Permitted(u.Role, "admin") {
		return true
	}
	var role string
	err := a.DB.QueryRowContext(ctx, "SELECT role FROM instance_permissions WHERE user_id=? AND instance_id=?", u.ID, id).Scan(&role)
	return err == nil && auth.Permitted(role, "viewer")
}
func (a *App) loadInstance(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "instanceID")
		if !a.canInstance(r.Context(), session(r).User, id) {
			fail(w, 404, errors.New("instance not found"))
			return
		}
		i, err := a.Instances.Get(r.Context(), id)
		if err != nil {
			fail(w, 404, errors.New("instance not found or its credentials cannot be recovered"))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), instanceKey, i)))
	})
}
func instance(r *http.Request) model.Instance {
	i, _ := r.Context().Value(instanceKey).(model.Instance)
	return i
}
func decode(r *http.Request, v any) error {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		return errors.New("Content-Type must be application/json")
	}
	d := json.NewDecoder(io.LimitReader(r.Body, (1<<20)+1))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return errors.New("invalid JSON request: " + err.Error())
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("request must contain exactly one JSON object")
	}
	return nil
}
func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		slog.Debug("write response", "error", err)
	}
}
func fail(w http.ResponseWriter, status int, err error) {
	respond(w, status, map[string]string{"error": err.Error()})
}
func (a *App) audit(ctx context.Context, user, action, id, detail string) error {
	if len(detail) > 2048 {
		detail = detail[:2048]
	}
	_, err := a.DB.ExecContext(ctx, "INSERT INTO audit(time,user_id,action,instance_id,detail) VALUES(?,?,?,?,?)", time.Now().Unix(), user, action, id, detail)
	if err == nil {
		a.Jobs.Revision.Add(1)
	}
	return err
}
func (a *App) enqueue(w http.ResponseWriter, r *http.Request, id, kind string, payload any) {
	if err := a.audit(r.Context(), session(r).User.ID, "job."+kind, id, "Requested through the authenticated API"); err != nil {
		fail(w, 500, errors.New("could not write audit record; operation was not queued"))
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key != "" {
		key = session(r).User.ID + ":" + key
	}
	j, err := a.Jobs.Enqueue(r.Context(), id, kind, payload, key, 0)
	if err != nil {
		fail(w, 400, err)
		return
	}
	respond(w, http.StatusAccepted, j)
}

func (a *App) setupStatus(w http.ResponseWriter, r *http.Request) {
	required, err := a.Auth.Required(r.Context())
	if err != nil {
		fail(w, 503, errors.New("database is unavailable"))
		return
	}
	respond(w, 200, map[string]bool{"required": required})
}
func (a *App) setup(w http.ResponseWriter, r *http.Request) {
	if !a.origin(r) {
		fail(w, 403, errors.New("request origin is invalid"))
		return
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if !a.Auth.Allow("setup:" + host) {
		fail(w, 429, errors.New("too many setup attempts; try again in 15 minutes"))
		return
	}
	var body struct {
		Token    string `json:"token"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decode(r, &body); err != nil {
		fail(w, 400, err)
		return
	}
	token, err := os.ReadFile(filepath.Join(a.Config.DataDir, "setup-token"))
	if err != nil || subtle.ConstantTimeCompare([]byte(strings.TrimSpace(string(token))), []byte(body.Token)) != 1 {
		fail(w, 403, errors.New("invalid setup token"))
		return
	}
	u, err := a.Auth.CreateUser(r.Context(), body.Username, body.Password, "owner", true)
	if err != nil {
		fail(w, 409, err)
		return
	}
	if err := a.audit(r.Context(), u.ID, "auth.bootstrap", "", "First owner created; public registration disabled"); err != nil {
		slog.Error("bootstrap audit", "error", err)
	}
	if err := os.Remove(filepath.Join(a.Config.DataDir, "setup-token")); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("remove used setup token", "error", err)
	}
	s, err := a.Auth.Login(r.Context(), body.Username, body.Password, "")
	if err != nil {
		fail(w, 500, errors.New("owner created; sign in to continue"))
		return
	}
	a.cookie(w, s.Token)
	respond(w, 201, s)
}
func (a *App) cookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{Name: "nec_session", Value: token, Path: "/", HttpOnly: true, Secure: a.Config.SecureCookies, SameSite: http.SameSiteStrictMode, MaxAge: 12 * 60 * 60})
}
func (a *App) login(w http.ResponseWriter, r *http.Request) {
	if !a.origin(r) {
		fail(w, 403, errors.New("request origin is invalid"))
		return
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if !a.Auth.Allow("login:" + host) {
		fail(w, 429, errors.New("too many sign-in attempts; try again in 15 minutes"))
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if err := decode(r, &body); err != nil {
		fail(w, 400, err)
		return
	}
	s, err := a.Auth.Login(r.Context(), body.Username, body.Password, body.Code)
	if err != nil {
		a.audit(r.Context(), "anonymous", "auth.failed", "", "Sign-in rejected")
		fail(w, 401, errors.New("invalid credentials or authentication code"))
		return
	}
	a.cookie(w, s.Token)
	a.audit(r.Context(), s.User.ID, "auth.login", "", "Signed in")
	respond(w, 200, s)
}
func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	cookie, _ := r.Cookie("nec_session")
	if err := a.Auth.Logout(r.Context(), cookie.Value); err != nil {
		fail(w, 500, errors.New("could not invalidate session"))
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "nec_session", Path: "/", Value: "", MaxAge: -1, HttpOnly: true, Secure: a.Config.SecureCookies, SameSite: http.SameSiteStrictMode})
	respond(w, 200, map[string]bool{"ok": true})
}
func (a *App) health(w http.ResponseWriter, r *http.Request) {
	if err := a.DB.PingContext(r.Context()); err != nil {
		fail(w, 503, errors.New("database is unavailable"))
		return
	}
	respond(w, 200, map[string]any{"status": "ok", "version": a.Version, "schema": database.SchemaVersion})
}
func (a *App) events(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	rc := http.NewResponseController(w)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	last := ^uint64(0)
	for {
		rc.SetWriteDeadline(time.Now().Add(10 * time.Second))
		revision := a.Jobs.Revision.Load()
		message := ": heartbeat\n\n"
		if revision != last {
			message = fmt.Sprintf("event: invalidate\ndata: %d\n\n", revision)
			last = revision
		}
		if _, err := fmt.Fprint(w, message); err != nil {
			return
		}
		if err := rc.Flush(); err != nil {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *App) listInstances(w http.ResponseWriter, r *http.Request) {
	all, err := a.Instances.List(r.Context())
	if err != nil {
		fail(w, 500, errors.New("could not list instances"))
		return
	}
	visible := []model.Instance{}
	for _, i := range all {
		if a.canInstance(r.Context(), session(r).User, i.ID) {
			visible = append(visible, i)
		}
	}
	respond(w, 200, visible)
}
func (a *App) getInstance(w http.ResponseWriter, r *http.Request) { respond(w, 200, instance(r)) }
func (a *App) status(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	status, err := a.runtimeStatus(ctx, instance(r))
	if err != nil {
		status.State = "unknown"
		status.Ready = false
		status.Detail = err.Error()
	}
	if status.Players == nil {
		status.Players = []string{}
	}
	if root, rootErr := a.Instances.OpenRoot(instance(r).Root); rootErr == nil {
		if available, diskErr := freeDisk(root); diskErr == nil {
			status.DiskFreeBytes = &available
		}
		root.Close()
	}
	respond(w, 200, status)
}
func (a *App) logs(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	lines, err := a.Runtime.Logs(ctx, instance(r), 200)
	if err != nil {
		fail(w, 503, err)
		return
	}
	if lines == nil {
		lines = []model.LogLine{}
	}
	if secret := instance(r).RCONSecret; secret != "" {
		for n := range lines {
			lines[n].Message = strings.ReplaceAll(lines[n].Message, secret, "[REDACTED]")
		}
	}
	respond(w, 200, lines)
}

func (a *App) runtimeCommand(ctx context.Context, i model.Instance, command string) (string, error) {
	output, err := a.Runtime.Command(ctx, i, command)
	if i.RCONSecret != "" {
		output = strings.ReplaceAll(output, i.RCONSecret, "[redacted]")
		if err != nil && strings.Contains(err.Error(), i.RCONSecret) {
			err = errors.New(strings.ReplaceAll(err.Error(), i.RCONSecret, "[redacted]"))
		}
	}
	return output, err
}

func (a *App) runtimeStatus(ctx context.Context, i model.Instance) (model.RuntimeStatus, error) {
	status, err := a.Runtime.Status(ctx, i)
	switch status.State {
	case "stopped":
		status.State = "offline"
	case "failed":
		status.State = "crashed"
	case "running":
		if status.Ready {
			status.State = "ready"
		} else {
			status.State = "starting"
		}
	case "unhealthy":
		status.State = "unknown"
	}
	if status.Players == nil {
		status.Players = []string{}
	}
	return status, err
}
func (a *App) lifecycle(w http.ResponseWriter, r *http.Request) {
	action := chi.URLParam(r, "action")
	if action != "start" && action != "stop" && action != "restart" && action != "backup" {
		fail(w, 400, errors.New("unsupported lifecycle action"))
		return
	}
	a.enqueue(w, r, instance(r).ID, action, map[string]any{})
}
func (a *App) console(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Command string `json:"command"`
	}
	if err := decode(r, &body); err != nil {
		fail(w, 400, err)
		return
	}
	if err := validateCommand(body.Command, session(r).User.Role); err != nil {
		fail(w, 403, err)
		return
	}
	a.enqueue(w, r, instance(r).ID, "console", body)
}

func validateCommand(command, role string) error {
	if len(command) == 0 || len(command) > 4096 || strings.ContainsAny(command, "\r\n\x00") {
		return errors.New("command must be one bounded Minecraft command")
	}
	verb := strings.Fields(strings.TrimPrefix(command, "/"))
	if len(verb) == 0 {
		return errors.New("command is empty")
	}
	allowed := map[string]bool{"list": true, "say": true, "kick": true, "whitelist": true, "ban": true, "pardon": true, "save-all": true}
	if !auth.Permitted(role, "admin") && !allowed[verb[0]] {
		return errors.New("this console command requires administrator permission")
	}
	if !auth.Permitted(role, "admin") && verb[0] == "whitelist" && (len(verb) < 2 || verb[1] != "add" && verb[1] != "remove" && verb[1] != "list") {
		return errors.New("changing whitelist enforcement requires administrator permission")
	}
	if verb[0] == "stop" || verb[0] == "save-off" || verb[0] == "save-on" {
		return errors.New("use lifecycle or coordinated backup controls for this command")
	}
	return nil
}

func (a *App) listJobs(w http.ResponseWriter, r *http.Request) {
	all, err := a.Jobs.List(r.Context())
	if err != nil {
		fail(w, 500, err)
		return
	}
	visible := []jobs.Job{}
	for _, j := range all {
		if j.InstanceID == "" && auth.Permitted(session(r).User.Role, "admin") || a.canInstance(r.Context(), session(r).User, j.InstanceID) {
			j.Payload = nil
			visible = append(visible, j)
		}
	}
	respond(w, 200, visible)
}
func (a *App) accessibleJob(w http.ResponseWriter, r *http.Request) (jobs.Job, bool) {
	j, err := a.Jobs.Get(r.Context(), chi.URLParam(r, "jobID"))
	if err != nil || j.InstanceID == "" && !auth.Permitted(session(r).User.Role, "admin") || j.InstanceID != "" && !a.canInstance(r.Context(), session(r).User, j.InstanceID) {
		fail(w, 404, errors.New("job not found"))
		return j, false
	}
	return j, true
}
func (a *App) getJob(w http.ResponseWriter, r *http.Request) {
	j, ok := a.accessibleJob(w, r)
	if !ok {
		return
	}
	steps, err := a.Jobs.Steps(r.Context(), j.ID)
	if err != nil {
		fail(w, 500, err)
		return
	}
	j.Payload = nil
	respond(w, 200, map[string]any{"job": j, "steps": steps})
}
func (a *App) cancelJob(w http.ResponseWriter, r *http.Request) {
	j, ok := a.accessibleJob(w, r)
	if !ok {
		return
	}
	if !auth.Permitted(session(r).User.Role, "admin") {
		fail(w, 403, errors.New("cancelling maintenance requires administrator permission"))
		return
	}
	if err := a.Jobs.Cancel(r.Context(), j.ID); err != nil {
		fail(w, 409, err)
		return
	}
	a.audit(r.Context(), session(r).User.ID, "job.cancel", j.InstanceID, j.ID)
	respond(w, 200, map[string]bool{"ok": true})
}
func (a *App) listSchedules(w http.ResponseWriter, r *http.Request) {
	all, err := a.Schedules.List(r.Context())
	if err != nil {
		fail(w, 500, err)
		return
	}
	visible := []scheduler.Schedule{}
	for _, s := range all {
		if a.canInstance(r.Context(), session(r).User, s.InstanceID) {
			visible = append(visible, s)
		}
	}
	respond(w, 200, visible)
}
func (a *App) saveSchedule(w http.ResponseWriter, r *http.Request) {
	var entry scheduler.Schedule
	if err := decode(r, &entry); err != nil {
		fail(w, 400, err)
		return
	}
	saved, err := a.Schedules.Save(r.Context(), entry)
	if err != nil {
		fail(w, 400, err)
		return
	}
	a.audit(r.Context(), session(r).User.ID, "schedule.save", entry.InstanceID, entry.Kind)
	respond(w, 201, saved)
}
func (a *App) deleteSchedule(w http.ResponseWriter, r *http.Request) {
	result, err := a.DB.ExecContext(r.Context(), "DELETE FROM schedules WHERE id=?", chi.URLParam(r, "scheduleID"))
	if err != nil {
		fail(w, 500, err)
		return
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		fail(w, 404, errors.New("schedule not found"))
		return
	}
	a.audit(r.Context(), session(r).User.ID, "schedule.delete", "", chi.URLParam(r, "scheduleID"))
	respond(w, 200, map[string]bool{"ok": true})
}
func (a *App) users(w http.ResponseWriter, r *http.Request) {
	all, err := a.Auth.Users(r.Context())
	if err != nil {
		fail(w, 500, err)
		return
	}
	respond(w, 200, all)
}
func (a *App) createUser(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username    string   `json:"username"`
		Password    string   `json:"password"`
		Role        string   `json:"role"`
		InstanceIDs []string `json:"instance_ids"`
	}
	if err := decode(r, &body); err != nil {
		fail(w, 400, err)
		return
	}
	for _, id := range body.InstanceIDs {
		if _, err := a.Instances.Get(r.Context(), id); err != nil {
			fail(w, 400, errors.New("instance grant refers to an unknown instance"))
			return
		}
	}
	u, err := a.Auth.CreateUser(r.Context(), body.Username, body.Password, body.Role, false)
	if err != nil {
		fail(w, 400, err)
		return
	}
	if body.Role == "viewer" || body.Role == "operator" {
		for _, id := range body.InstanceIDs {
			if _, err := a.DB.ExecContext(r.Context(), "INSERT INTO instance_permissions(user_id,instance_id,role) VALUES(?,?,?)", u.ID, id, body.Role); err != nil {
				fail(w, 500, errors.New("user created but instance permission could not be saved"))
				return
			}
		}
	}
	a.audit(r.Context(), session(r).User.ID, "user.create", "", u.Username+" ("+u.Role+")")
	respond(w, 201, u)
}
func (a *App) enableTOTP(w http.ResponseWriter, r *http.Request) {
	if !a.Auth.Allow("account:" + session(r).User.ID) {
		fail(w, 429, errors.New("too many account-security changes; try again in 15 minutes"))
		return
	}
	var body struct {
		Password string `json:"password"`
		Secret   string `json:"secret"`
		Code     string `json:"code"`
	}
	if err := decode(r, &body); err != nil {
		fail(w, 400, err)
		return
	}
	if body.Secret == "" {
		respond(w, 200, map[string]string{"secret": auth.NewTOTP()})
		return
	}
	if err := a.Auth.EnableTOTP(r.Context(), session(r).User.ID, body.Password, body.Secret, body.Code); err != nil {
		fail(w, 400, err)
		return
	}
	a.audit(r.Context(), session(r).User.ID, "auth.totp", "", "Two-factor authentication enabled; sessions revoked")
	respond(w, 200, map[string]bool{"ok": true})
}
func (a *App) auditList(w http.ResponseWriter, r *http.Request) {
	rows, err := a.DB.QueryContext(r.Context(), "SELECT id,time,user_id,action,instance_id,detail FROM audit ORDER BY id DESC LIMIT 500")
	if err != nil {
		fail(w, 500, err)
		return
	}
	defer rows.Close()
	all := []map[string]any{}
	for rows.Next() {
		var id, at int64
		var user, action, instance, detail string
		if err := rows.Scan(&id, &at, &user, &action, &instance, &detail); err != nil {
			fail(w, 500, err)
			return
		}
		all = append(all, map[string]any{"id": id, "time": at, "user_id": user, "action": action, "instance_id": instance, "detail": detail})
	}
	respond(w, 200, all)
}
func (a *App) metrics(w http.ResponseWriter, r *http.Request) {
	rows, err := a.DB.QueryContext(r.Context(), "SELECT time,state,memory_bytes,cpu_seconds FROM metrics WHERE instance_id=? ORDER BY time DESC LIMIT 288", instance(r).ID)
	if err != nil {
		fail(w, 500, err)
		return
	}
	defer rows.Close()
	all := []map[string]any{}
	for rows.Next() {
		var at, memory int64
		var cpu float64
		var state string
		if err := rows.Scan(&at, &state, &memory, &cpu); err != nil {
			fail(w, 500, err)
			return
		}
		all = append(all, map[string]any{"time": at, "state": state, "memory_bytes": memory, "cpu_seconds": cpu})
	}
	respond(w, 200, all)
}

func (a *App) setting(ctx context.Context, key string, out any) error {
	var raw string
	err := a.DB.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=?", key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(raw), out)
}
func (a *App) saveSetting(ctx context.Context, key string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = a.DB.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, string(b))
	return err
}
