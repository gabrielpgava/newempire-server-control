package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gabrielpgava/newempire-server-control/internal/api"
	"github.com/gabrielpgava/newempire-server-control/internal/auth"
	"github.com/gabrielpgava/newempire-server-control/internal/config"
	"github.com/gabrielpgava/newempire-server-control/internal/database"
	"github.com/gabrielpgava/newempire-server-control/internal/lock"
	"github.com/gabrielpgava/newempire-server-control/internal/model"
	serverruntime "github.com/gabrielpgava/newempire-server-control/internal/runtime"
	"github.com/gabrielpgava/newempire-server-control/internal/secrets"
	"github.com/gabrielpgava/newempire-server-control/web"
)

var version = "dev"
var commit = "unknown"
var releasePublicKey = "5jtZuBvFVWvMMHZo5VCuP98GEOt1uxmzvSF+rBzzQlM="

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if err := run(os.Args[1:]); err != nil {
		slog.Error("command failed", "error", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return help()
	}
	command := args[0]
	args = args[1:]
	if command == "help" || command == "--help" || command == "-h" {
		return help()
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	configPath := flags.String("config", os.Getenv("NEC_CONFIG"), "Configuration JSON file")
	jsonOutput := flags.Bool("json", false, "Print machine-readable output")
	username := flags.String("username", "", "Username for login")
	passwordStdin := flags.Bool("password-stdin", false, "Read login password from standard input")
	code := flags.String("code", "", "Six-digit login authentication code")
	codeStdin := flags.Bool("code-stdin", false, "Read login authentication code from the second standard-input line")
	offline := flags.Bool("offline", false, "Use stopped-panel local recovery without HTTP")
	confirmed := flags.Bool("confirm", false, "Confirm a destructive operation after reviewing its preview")
	independent := flags.Bool("independent", false, "Run as the restricted independent updater, not through HTTP")
	if err := flags.Parse(args); err != nil {
		return err
	}
	args = flags.Args()
	if err := validateCLIFlags(command, flags, args); err != nil {
		return err
	}
	if command == "version" {
		if *jsonOutput {
			return output(map[string]any{"version": version, "commit": commit, "schema": database.SchemaVersion, "release_repository": "gabrielpgava/newempire-server-control"})
		}
		fmt.Printf("New Empire Server Control %s (%s)\n", version, commit)
		return nil
	}
	c, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if c.ReleasePublicKey == "" {
		c.ReleasePublicKey = releasePublicKey
	}
	switch command {
	case "serve":
		return serve(c)
	case "doctor":
		return doctor(c)
	case "install-service":
		return installService(c, args)
	case "runtime-helper":
		return runtimeHelper(args)
	case "update-helper":
		if len(args) != 0 {
			return errors.New("update-helper accepts only the configured dispatch operation")
		}
		return independentUpdate(c, []string{"dispatch"})
	case "update":
		if *independent {
			return independentUpdate(c, args)
		}
		return remoteCommand(c, command, args, *confirmed)
	case "recover":
		return recoveryCommand(c, args, *confirmed, *passwordStdin, os.Stdin)
	case "login":
		if !*passwordStdin || *username == "" {
			return errors.New("login requires --username and --password-stdin; passwords are never accepted as command-line arguments")
		}
		password, second, err := readCLICredentials(os.Stdin, *code, *codeStdin)
		if err != nil {
			return err
		}
		return loginCLI(c, *username, password, second)
	case "backup":
		if *offline {
			return offlineBackupCommand(c, args, *confirmed)
		}
		return remoteCommand(c, command, args, *confirmed)
	case "instance":
		return remoteCommand(c, command, args, *confirmed)
	default:
		return fmt.Errorf("unknown command %q; run help", command)
	}
}

func help() error {
	fmt.Print(`New Empire Server Control

Commands:
  serve [--config FILE]                  Serve the embedded UI and API (never as root)
  version [--json]                       Print build identity
  doctor [--config FILE]                 Check configuration, database, and host requirements
  login --username NAME --password-stdin [--code CODE | --code-stdin]
                                         Store a protected CLI session
  install-service [--config FILE] [ID]    Provision an allowlisted systemd instance
  instance list|inspect|start|stop|restart [ID]
  backup [--offline] create|list|verify|preview|restore INSTANCE_ID [BACKUP_ID]
  update check|stage|apply|rollback       Manage signed panel releases
  recover [--config FILE]                Inspect stopped-panel integrity and interrupted jobs
  recover list INSTANCE_ID               List retained recovery records
  recover restore|restore-previous INSTANCE_ID
                                         Preview pending metadata-only reconciliation
  recover --confirm restore|restore-previous INSTANCE_ID
                                         Reconcile a recorded restore (never swaps worlds)
  recover --confirm filesystem INSTANCE_ID .nesc-recovery-ID
  recover --confirm content INSTANCE_ID TRANSACTION_ID
  recover --confirm file INSTANCE_ID TOKEN
  recover --confirm runtime INSTANCE_ID  Recover a retained runtime (never worlds)
  recover --confirm --password-stdin owner OWNER_USERNAME
                                         Reset owner password/factor and revoke owner sessions

Put flags before subcommands, for example:
  new-empire-control backup --confirm restore INSTANCE_ID BACKUP_ID
  new-empire-control instance --config /etc/new-empire-control/config.json list

Offline commands require the initialized panel state, its master key, the panel
account, and a stopped panel. Minecraft must also be stopped for offline mutations.
Destructive recovery requires --confirm after reviewing the selected record.
See docs/disaster-recovery.md before emergency binary or database recovery.
`)
	return nil
}

func serve(c config.Config) error {
	if os.Geteuid() == 0 {
		return errors.New("the HTTP service must not run as root; use the dedicated panel account")
	}
	if err := c.Prepare(); err != nil {
		return err
	}
	release, err := lock.Acquire(filepath.Join(c.DataDir, "panel.lock"))
	if err != nil {
		return err
	}
	defer release()
	db, err := database.Open(filepath.Join(c.DataDir, "panel.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	vault, err := secrets.Open(filepath.Join(c.DataDir, "master.key"))
	if err != nil {
		return err
	}
	authService := auth.New(db, vault)
	required, err := authService.Required(context.Background())
	if err != nil {
		return err
	}
	if required {
		path := filepath.Join(c.DataDir, "setup-token")
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err == nil {
			_, writeErr := io.WriteString(f, auth.Token()+"\n")
			syncErr := f.Sync()
			closeErr := f.Close()
			if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrExist) {
			return err
		}
		slog.Info("first-run setup requires the protected local token", "token_file", path)
	}
	app := api.New(c, db, vault, serverruntime.NewSystemd(c.RuntimeHelper), version, uiHandler(web.FS()))
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := app.Start(ctx); err != nil {
		return err
	}
	defer app.Wait()
	server := &http.Server{Addr: c.Listen, Handler: app.Router(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	stop := make(chan error, 1)
	go func() { stop <- server.ListenAndServe() }()
	slog.Info("panel listening", "address", c.Listen, "public_url", c.PublicURL, "version", version)
	select {
	case err := <-stop:
		cancel()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	shutdown, cancelShutdown := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelShutdown()
	return server.Shutdown(shutdown)
}

func uiHandler(assets fs.FS) http.Handler {
	files := http.FileServer(http.FS(assets))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && r.Method != "HEAD" {
			http.Error(w, "Method not allowed", 405)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name == "" {
			name = "index.html"
		}
		if !fs.ValidPath(name) {
			http.NotFound(w, r)
			return
		}
		if _, err := fs.Stat(assets, name); err != nil {
			if strings.Contains(filepath.Base(name), ".") {
				http.NotFound(w, r)
				return
			}
			r = r.Clone(r.Context())
			r.URL.Path = "/"
		}
		if strings.HasPrefix(name, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}

func output(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func doctor(c config.Config) error {
	checks := map[string]any{"public_url": c.PublicURL, "data_dir": c.DataDir, "instance_dir": c.InstanceDir, "backup_dir": c.BackupDir, "java_allowlist": c.JavaPaths, "runtime_helper": c.RuntimeHelper, "version": version}
	if _, err := os.Stat(filepath.Join(c.DataDir, "panel.db")); err == nil {
		release, err := lock.Acquire(filepath.Join(c.DataDir, "panel.lock"))
		if err != nil {
			return fmt.Errorf("stop the panel before offline database diagnostics; do not remove panel.lock: %w", err)
		}
		defer release()
		db, err := database.OpenExisting(filepath.Join(c.DataDir, "panel.db"))
		if err != nil {
			return err
		}
		defer db.Close()
		if err := db.Integrity(context.Background()); err != nil {
			return err
		}
		checks["database"] = "integrity and foreign keys verified"
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	} else {
		checks["database"] = "not initialized"
	}
	if _, err := os.Stat(c.RuntimeHelper); err != nil {
		checks["runtime"] = "restricted helper missing; lifecycle unavailable"
	} else {
		checks["runtime"] = "helper exists; systemd/firewall permissions still require a host test"
	}
	return output(checks)
}

type cliSession struct {
	Token string `json:"token"`
	CSRF  string `json:"csrf"`
}

func loginCLI(c config.Config, username, password, code string) error {
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Timeout: 15 * time.Second, Jar: jar, CheckRedirect: rejectCLIRedirect}
	var session auth.Session
	if err := apiRequest(c, client, cliSession{}, "POST", "/auth/login", map[string]string{"username": username, "password": password, "code": code}, &session); err != nil {
		return err
	}
	url, err := http.NewRequest("GET", c.PublicURL, nil)
	if err != nil {
		return err
	}
	var token string
	for _, cookie := range jar.Cookies(url.URL) {
		if cookie.Name == "nec_session" {
			token = cookie.Value
		}
	}
	if token == "" {
		return errors.New("server did not return a session cookie")
	}
	if err := writeCLISession(c, cliSession{Token: token, CSRF: session.CSRF}); err != nil {
		return err
	}
	fmt.Println("Signed in; CLI session stored with owner-only permissions.")
	return nil
}
func apiRequest(c config.Config, client *http.Client, s cliSession, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = strings.NewReader(string(b))
	}
	req, err := http.NewRequest(method, strings.TrimSuffix(c.PublicURL, "/")+"/api/v1"+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", strings.TrimSuffix(c.PublicURL, "/"))
	if s.Token != "" {
		req.AddCookie(&http.Cookie{Name: "nec_session", Value: s.Token})
		req.Header.Set("X-CSRF-Token", s.CSRF)
	}
	protected := *client
	protected.CheckRedirect = rejectCLIRedirect
	resp, err := protected.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var problem struct {
			Error string `json:"error"`
		}
		if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&problem) != nil {
			return fmt.Errorf("API returned HTTP %d", resp.StatusCode)
		}
		return errors.New(problem.Error)
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out)
	}
	return nil
}
func remoteCommand(c config.Config, command string, args []string, confirm bool) error {
	if len(args) == 0 {
		return errors.New("a subcommand is required")
	}
	action := args[0]
	args = args[1:]
	method, path := "GET", ""
	var body any
	switch command {
	case "instance":
		if action == "list" {
			if len(args) != 0 {
				return errors.New("instance list takes no instance ID")
			}
			path = "/instances"
		} else {
			if len(args) != 1 {
				return errors.New("provide exactly one instance ID")
			}
			if !model.ValidID(args[0]) {
				return errors.New("invalid instance identifier")
			}
			path = "/instances/" + args[0]
			switch action {
			case "inspect":
			case "start", "stop", "restart":
				path += "/actions/" + action
				method = "POST"
				body = map[string]any{}
			default:
				return errors.New("unsupported instance subcommand")
			}
		}
	case "backup":
		if err := validateBackupArgs(append([]string{action}, args...), confirm); err != nil {
			return err
		}
		path = "/instances/" + args[0] + "/backups"
		switch action {
		case "list":
		case "create":
			method = "POST"
			body = map[string]string{"note": "Created through CLI"}
		case "verify", "preview", "restore":
			if len(args) != 2 {
				return errors.New("provide instance ID and backup ID")
			}
			if action == "restore" && !confirm {
				return errors.New("review the restore preview, then repeat with --confirm before the subcommand")
			}
			path += "/" + args[1] + "/" + action
			if action != "preview" {
				method = "POST"
				body = map[string]bool{"confirm": confirm}
			}
		default:
			return errors.New("unsupported backup subcommand")
		}
	case "update":
		if len(args) != 0 {
			return errors.New("update takes exactly one subcommand")
		}
		switch action {
		case "check", "stage", "apply", "rollback":
			method = "POST"
			path = "/updates/" + action
			body = map[string]bool{"confirm": confirm}
		default:
			return errors.New("unsupported update subcommand")
		}
	}
	session, err := readCLISession(c)
	if err != nil {
		return fmt.Errorf("sign in with the login command first: %w", err)
	}
	var result any
	if err := apiRequest(c, &http.Client{Timeout: 30 * time.Second}, session, method, path, body, &result); err != nil {
		return err
	}
	return output(result)
}
