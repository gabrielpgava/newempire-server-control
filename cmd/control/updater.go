package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gabrielpgava/newempire-server-control/internal/config"
	"github.com/gabrielpgava/newempire-server-control/internal/database"
	"github.com/gabrielpgava/newempire-server-control/internal/files"
	"github.com/gabrielpgava/newempire-server-control/internal/lock"
	"github.com/gabrielpgava/newempire-server-control/internal/model"
	"github.com/gabrielpgava/newempire-server-control/internal/panelupdate"
	"github.com/gabrielpgava/newempire-server-control/internal/updates"
)

type panelService struct{ config config.Config }

func systemctl(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "/usr/bin/systemctl", args...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C"}
	return cmd.Run()
}
func (s panelService) StopPanel(ctx context.Context) error {
	return systemctl(ctx, "stop", "new-empire-control.service")
}
func (s panelService) RestartPanel(ctx context.Context) error {
	return systemctl(ctx, "restart", "new-empire-control.service")
}
func installedVersion(c config.Config) (string, error) {
	target, err := os.Readlink(filepath.Join(c.InstallDir, "current"))
	if err != nil {
		return "", err
	}
	parts := strings.Split(filepath.ToSlash(target), "/")
	if len(parts) != 2 || parts[0] != "versions" || parts[1] == "" || parts[1] == "." || parts[1] == ".." {
		return "", errors.New("current installation is not a validated version symlink")
	}
	return parts[1], nil
}
func (s panelService) Health(ctx context.Context) error {
	_, port, err := net.SplitHostPort(s.config.Listen)
	if err != nil {
		return errors.New("panel listen address cannot be used for a local health check")
	}
	expected, err := installedVersion(s.config)
	if err != nil {
		return err
	}
	token, err := os.ReadFile(filepath.Join(s.config.DataDir, "health-token"))
	if err != nil {
		return errors.New("authenticated local health token is unavailable")
	}
	tokenString := strings.TrimSpace(string(token))
	if len(tokenString) != 43 {
		return errors.New("local health token has an invalid format")
	}
	client := http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("local health redirects are forbidden") }}
	timer := time.NewTicker(time.Second)
	defer timer.Stop()
	for {
		req, err := http.NewRequestWithContext(ctx, "GET", "http://127.0.0.1:"+port+"/internal/health", nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tokenString)
		response, requestErr := client.Do(req)
		if requestErr == nil {
			var health struct {
				Status  string `json:"status"`
				Version string `json:"version"`
				Schema  int    `json:"schema"`
			}
			decodeErr := json.NewDecoder(http.MaxBytesReader(nil, response.Body, 4096)).Decode(&health)
			response.Body.Close()
			if decodeErr == nil && response.StatusCode == 200 && health.Status == "ok" && health.Version == expected && health.Schema == database.SchemaVersion {
				if err := s.verifyProcess(ctx); err == nil {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return errors.New("panel failed its authenticated local version/schema health check")
		case <-timer.C:
		}
	}
}
func (s panelService) verifyProcess(ctx context.Context) error {
	command := exec.CommandContext(ctx, "/usr/bin/systemctl", "show", "new-empire-control.service", "--property=MainPID", "--value")
	command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C"}
	out, err := command.Output()
	if err != nil || len(out) > 32 {
		return errors.New("panel service PID is unavailable")
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || pid < 1 {
		return errors.New("panel service has no running process")
	}
	process, err := os.Stat(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return err
	}
	binary, err := os.Stat(filepath.Join(s.config.InstallDir, "current", "new-empire-control"))
	if err != nil || !os.SameFile(process, binary) {
		return errors.New("health response does not belong to the selected panel executable")
	}
	return nil
}

func independentUpdate(c config.Config, args []string) (resultErr error) {
	if runtime.GOOS != "linux" || os.Getuid() != 0 || os.Geteuid() != 0 {
		return errors.New("independent updates require the restricted root-owned Linux oneshot or a root recovery shell")
	}
	if c.ReleaseRepository != updates.Repository {
		return errors.New("configured release repository is not this build's official identity")
	}
	if err := validateUpdaterPaths(c); err != nil {
		return err
	}
	if len(args) < 1 {
		return errors.New("independent updater action is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	current, err := installedVersion(c)
	if err != nil {
		return err
	}
	manager, err := updates.New(updates.Options{InstallDir: c.InstallDir, CurrentVersion: current, Schema: database.SchemaVersion, PublicKey: c.ReleasePublicKey, Service: panelService{c}, Snapshot: func(ctx context.Context, dir string) error { return snapshotPanel(ctx, c, dir) }, Restore: func(ctx context.Context, dir string) error { return restorePanel(ctx, c, dir) }})
	if err != nil {
		return err
	}
	action := args[0]
	channel := updates.Stable
	selected := ""
	var request panelupdate.Request
	var root *os.Root
	if action == "dispatch" {
		root, err = os.OpenRoot(c.DataDir)
		if err != nil {
			return err
		}
		defer root.Close()
		request, err = panelupdate.ReadRequest(ctx, root)
		if err != nil {
			return err
		}
		action = request.Action
		selected = request.Version
		channel = request.Channel
		if err := panelupdate.Publish(ctx, root, panelupdate.Report{RequestID: request.ID, JobID: request.JobID, State: "running"}); err != nil {
			return err
		}
		defer func() {
			journal, journalErr := manager.Status(context.Background())
			report := panelupdate.Report{RequestID: request.ID, JobID: request.JobID, State: "succeeded", Journal: journal}
			if resultErr != nil {
				report.State = "failed"
				report.Error = resultErr.Error()
			}
			if journalErr != nil && resultErr == nil {
				report.State = "failed"
				report.Error = "updater completed but its durable journal could not be read"
			}
			if err := panelupdate.Publish(context.Background(), root, report); err != nil {
				resultErr = errors.Join(resultErr, err)
			}
		}()
		journal, err := manager.Status(ctx)
		if err != nil {
			return err
		}
		if journal.Transaction != nil {
			switch journal.Transaction.State {
			case updates.Downloading, updates.Stopping, updates.Snapshotting, updates.Installing, updates.CheckingHealth, updates.RollingBack, updates.ManualRecovery:
				if err := manager.Recover(ctx); err != nil {
					return err
				}
				return errors.New("interrupted panel update recovered; review its journal before requesting another operation")
			}
		}
	} else if len(args) > 1 {
		selected = args[1]
	}
	switch action {
	case "check":
		checked, err := manager.Check(ctx, channel)
		if err != nil {
			return err
		}
		return output(checked)
	case "stage":
		if selected == "" {
			return errors.New("independent staging requires a reviewed release version")
		}
		transaction, err := manager.Stage(ctx, selected, channel)
		if err != nil {
			return err
		}
		return output(transaction)
	case "apply":
		return manager.Apply(ctx)
	case "rollback":
		return manager.Rollback(ctx)
	case "recover":
		return manager.Recover(ctx)
	default:
		return errors.New("unsupported independent updater action")
	}
}

func validateUpdaterPaths(c config.Config) error {
	if c.SourcePath == "" {
		return errors.New("independent updates require an explicit root-owned configuration file")
	}
	for _, entry := range []struct {
		path string
		data bool
	}{{c.SourcePath, false}, {c.InstallDir, false}, {c.DataDir, true}} {
		if !filepath.IsAbs(entry.path) || filepath.Clean(entry.path) != entry.path {
			return errors.New("updater paths must be canonical absolute paths")
		}
		current := string(filepath.Separator)
		parts := strings.Split(strings.TrimPrefix(entry.path, current), string(filepath.Separator))
		for n, part := range parts {
			current = filepath.Join(current, part)
			info, err := os.Lstat(current)
			if err != nil {
				return errors.New("updater configuration, install, or data path is unavailable")
			}
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 {
				return errors.New("updater paths must not contain links or group/world-writable ancestors")
			}
			last := n == len(parts)-1
			if !last || !entry.data {
				if stat.Uid != 0 {
					return errors.New("updater install/configuration paths and data ancestors must be root-owned")
				}
			} else if stat.Uid == 0 || !info.IsDir() {
				return errors.New("panel data directory must belong to the unprivileged panel account")
			}
			if !last && !info.IsDir() {
				return errors.New("updater path ancestor is not a directory")
			}
		}
	}
	return nil
}

func copySnapshotFile(ctx context.Context, source *os.Root, name string, dest *os.Root, to string) error {
	in, err := files.OpenRegular(source, name)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := dest.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = files.Copy(ctx, out, in, 8<<30, nil)
	return errors.Join(err, out.Sync(), out.Close())
}
func snapshotPanel(ctx context.Context, c config.Config, dir string) error {
	release, err := lock.Acquire(filepath.Join(c.DataDir, "panel.lock"))
	if err != nil {
		return err
	}
	defer release()
	dest, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer dest.Close()
	data, err := os.OpenRoot(c.DataDir)
	if err != nil {
		return err
	}
	defer data.Close()
	// Recover committed WAL pages in a private copy, never open a panel-controlled SQLite path as root.
	if err := copySnapshotFile(ctx, data, "panel.db", dest, "source.db"); err != nil {
		return err
	}
	if _, err := data.Lstat("panel.db-wal"); err == nil {
		if err := copySnapshotFile(ctx, data, "panel.db-wal", dest, "source.db-wal"); err != nil {
			return err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	db, err := database.Open(filepath.Join(dir, "source.db"))
	if err != nil {
		return err
	}
	integrityErr := db.Integrity(ctx)
	if integrityErr == nil {
		integrityErr = db.Snapshot(ctx, filepath.Join(dir, "panel.db"))
	}
	closeErr := db.Close()
	if err := errors.Join(integrityErr, closeErr); err != nil {
		return err
	}
	for _, name := range []string{"source.db", "source.db-wal", "source.db-shm"} {
		if err := dest.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	for _, name := range []string{"master.key", "health-token"} {
		if err := copySnapshotFile(ctx, data, name, dest, name); err != nil {
			return err
		}
	}
	if c.SourcePath != "" {
		configRoot, err := os.OpenRoot(filepath.Dir(c.SourcePath))
		if err != nil {
			return err
		}
		defer configRoot.Close()
		if err := copySnapshotFile(ctx, configRoot, filepath.Base(c.SourcePath), dest, "config.json"); err != nil {
			return err
		}
	}
	return nil
}
func replaceSnapshotFile(ctx context.Context, source *os.Root, name string, dest *os.Root, to string) error {
	old, err := files.Check(dest, to)
	if err != nil {
		return err
	}
	stat, ok := old.Sys().(*syscall.Stat_t)
	if !ok || !old.Mode().IsRegular() {
		return errors.New("snapshot destination is not a regular Unix file")
	}
	tmp := ".panel-restore-" + model.NewID()
	if err := copySnapshotFile(ctx, source, name, dest, tmp); err != nil {
		return err
	}
	defer dest.Remove(tmp)
	f, err := files.OpenRegular(dest, tmp)
	if err != nil {
		return err
	}
	err = errors.Join(f.Chown(int(stat.Uid), int(stat.Gid)), f.Chmod(old.Mode().Perm()), f.Sync(), f.Close())
	if err != nil {
		return err
	}
	if err := dest.Rename(tmp, to); err != nil {
		return err
	}
	dir, err := dest.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func restorePanel(ctx context.Context, c config.Config, dir string) error {
	release, err := lock.Acquire(filepath.Join(c.DataDir, "panel.lock"))
	if err != nil {
		return err
	}
	defer release()
	source, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer source.Close()
	data, err := os.OpenRoot(c.DataDir)
	if err != nil {
		return err
	}
	defer data.Close()
	for _, name := range []string{"panel.db-wal", "panel.db-shm"} {
		if err := data.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	for _, name := range []string{"panel.db", "master.key", "health-token"} {
		if err := replaceSnapshotFile(ctx, source, name, data, name); err != nil {
			return err
		}
	}
	if c.SourcePath != "" {
		root, err := os.OpenRoot(filepath.Dir(c.SourcePath))
		if err != nil {
			return err
		}
		defer root.Close()
		if err := replaceSnapshotFile(ctx, source, "config.json", root, filepath.Base(c.SourcePath)); err != nil {
			return err
		}
	}
	return nil
}
