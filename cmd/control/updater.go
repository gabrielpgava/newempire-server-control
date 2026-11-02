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
	"github.com/gabrielpgava/newempire-server-control/internal/model"
	"github.com/gabrielpgava/newempire-server-control/internal/panelupdate"
	"github.com/gabrielpgava/newempire-server-control/internal/updates"
	"golang.org/x/sys/unix"
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
	return restartCheckedPanel(ctx, s.config, func() error {
		return systemctl(ctx, "restart", "new-empire-control.service")
	})
}
func restartCheckedPanel(ctx context.Context, c config.Config, restart func() error) error {
	err := func() error {
		data, err := os.OpenRoot(c.DataDir)
		if err != nil {
			return err
		}
		defer data.Close()
		release, err := lockPanelData(data)
		if err != nil {
			return err
		}
		defer release()
		return checkPanelSchemaCopy(ctx, c, data)
	}()
	if err != nil {
		return fmt.Errorf("panel restart refused; keep the panel stopped for manual recovery: %w", err)
	}
	// Startup must acquire panel.lock itself, after the schema check releases it.
	return restart()
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
		if err := panelupdate.Publish(ctx, root, panelupdate.Report{RequestID: request.ID, JobID: request.JobID, Action: action, Version: selected, State: "running"}); err != nil {
			return err
		}
		defer func() {
			journal, journalErr := manager.Status(context.Background())
			report := panelupdate.Report{RequestID: request.ID, JobID: request.JobID, Action: action, Version: selected, State: "succeeded", Journal: journal}
			if report.Version == "" && journal.Transaction != nil {
				report.Version = journal.Transaction.Version
			}
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
				action = "recover"
				selected = journal.Transaction.Version
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
		transaction, err := stagePanelUpdate(ctx, manager, current, selected, channel)
		if err != nil {
			return err
		}
		return output(transaction)
	case "apply":
		if selected != "" {
			journal, err := manager.Status(ctx)
			if err != nil {
				return err
			}
			if t := journal.Transaction; t == nil || t.Version != selected || t.Channel != channel {
				return errors.New("staged release changed after the automatic update request")
			}
		}
		return manager.Apply(ctx)
	case "rollback":
		return manager.Rollback(ctx)
	case "recover":
		return manager.Recover(ctx)
	default:
		return errors.New("unsupported independent updater action")
	}
}

func stagePanelUpdate(ctx context.Context, manager *updates.Manager, current, selected string, channel updates.Channel) (updates.Transaction, error) {
	if selected == "" {
		return updates.Transaction{}, errors.New("independent staging requires a reviewed release version")
	}
	journal, err := manager.Status(ctx)
	if err != nil {
		return updates.Transaction{}, err
	}
	if t := journal.Transaction; t != nil && t.State == updates.Staged && t.Version == selected && t.Channel == channel && t.PreviousVersion == current {
		return *t, nil
	}
	return manager.Stage(ctx, selected, channel)
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
	before, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := dest.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	n, copyErr := files.Copy(ctx, out, in, 8<<30, nil)
	after, statErr := source.Lstat(name)
	if statErr == nil && (!os.SameFile(before, after) || copyErr == nil && n != before.Size() || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || before.Mode() != after.Mode()) {
		statErr = errors.New("snapshot source changed while copying")
	}
	return errors.Join(copyErr, statErr, out.Sync(), out.Close())
}

// The caller holds panel.lock; SQLite only opens the private updater-owned copy.
func copyPanelDatabase(ctx context.Context, source, dest *os.Root) (*database.Store, error) {
	private, err := dest.Stat(".")
	if err != nil {
		return nil, err
	}
	privateStat, ok := private.Sys().(*syscall.Stat_t)
	if !ok || !private.IsDir() || privateStat.Uid != 0 || private.Mode().Perm()&0077 != 0 {
		return nil, errors.New("panel database copies require private root-owned updater storage")
	}
	owner, err := source.Stat(".")
	if err != nil {
		return nil, err
	}
	ownerStat, ok := owner.Sys().(*syscall.Stat_t)
	if !ok || !owner.IsDir() || owner.Mode().Perm()&0077 != 0 {
		return nil, errors.New("panel database source must be a private directory")
	}
	suffixes := []string{"", "-wal", "-shm", "-journal"}
	before := make([]os.FileInfo, len(suffixes))
	for n, suffix := range suffixes {
		info, err := files.Check(source, "panel.db"+suffix)
		if suffix != "" && errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.Mode().IsRegular() || stat.Uid != ownerStat.Uid || info.Mode().Perm()&0077 != 0 || info.Size() > 8<<30 || suffix == "" && info.Size() < 100 {
			return nil, fmt.Errorf("panel.db%s must be a bounded private regular file owned by its directory account", suffix)
		}
		before[n] = info
		// Rebuild the WAL index in private storage instead of trusting shared memory.
		if suffix != "-shm" {
			if err := copySnapshotFile(ctx, source, "panel.db"+suffix, dest, "source.db"+suffix); err != nil {
				return nil, err
			}
		}
	}
	for n, suffix := range suffixes {
		after, err := source.Lstat("panel.db" + suffix)
		if before[n] == nil && errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		stat, ok := after.Sys().(*syscall.Stat_t)
		if before[n] == nil || !ok || stat.Uid != ownerStat.Uid || stat.Nlink != 1 || !os.SameFile(before[n], after) || before[n].Size() != after.Size() || !before[n].ModTime().Equal(after.ModTime()) || before[n].Mode() != after.Mode() {
			return nil, errors.New("panel database or sidecars changed while copying")
		}
	}
	return database.OpenExisting(filepath.Join(dest.Name(), "source.db"))
}

func checkPanelSchemaCopy(ctx context.Context, c config.Config, source *os.Root) (resultErr error) {
	path := filepath.Join(c.InstallDir, "updates")
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || stat.Uid != 0 || info.Mode().Perm()&0077 != 0 {
		return errors.New("schema checks require private root-owned updater storage")
	}
	parent, err := os.OpenRoot(path)
	if err != nil {
		return err
	}
	defer parent.Close()
	opened, err := parent.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		return errors.Join(err, errors.New("updater storage changed while opening"))
	}
	name := ".schema-" + model.NewID()
	if err := parent.Mkdir(name, 0700); err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, parent.RemoveAll(name)) }()
	dest, err := parent.OpenRoot(name)
	if err != nil {
		return err
	}
	defer dest.Close()
	db, err := copyPanelDatabase(ctx, source, dest)
	if err != nil {
		return err
	}
	return db.Close()
}
func snapshotPanel(ctx context.Context, c config.Config, dir string) error {
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
	release, err := lockPanelData(data)
	if err != nil {
		return err
	}
	defer release()
	db, err := copyPanelDatabase(ctx, data, dest)
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
	for _, name := range []string{"source.db", "source.db-wal", "source.db-shm", "source.db-journal"} {
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
	release, err := lockPanelData(data)
	if err != nil {
		return err
	}
	defer release()
	if err := checkPanelSchemaCopy(ctx, c, data); err != nil {
		return fmt.Errorf("installed panel database prevents automatic restore: %w", err)
	}
	if err := checkPanelSchemaCopy(ctx, c, source); err != nil {
		return fmt.Errorf("panel snapshot database prevents automatic restore: %w", err)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := source.Lstat("panel.db" + suffix); err == nil {
			return errors.New("panel recovery snapshot must contain a standalone database without SQLite sidecars")
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	for _, name := range []string{"panel.db-wal", "panel.db-shm", "panel.db-journal"} {
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

func lockPanelData(data *os.Root) (func(), error) {
	f, err := files.OpenRegular(data, "panel.lock")
	if err != nil {
		return nil, fmt.Errorf("open existing panel lock: %w", err)
	}
	info, err := f.Stat()
	owner, ownerErr := data.Stat(".")
	if err != nil || ownerErr != nil {
		f.Close()
		return nil, errors.Join(err, ownerErr)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	dataStat, dataOK := owner.Sys().(*syscall.Stat_t)
	if !ok || !dataOK || stat.Uid != dataStat.Uid || info.Mode().Perm()&0022 != 0 {
		f.Close()
		return nil, errors.New("existing panel lock must belong to the panel data-directory owner and not be group/world writable")
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("another panel or recovery operation holds the data-directory lock")
	}
	return func() { f.Close() }, nil
}
