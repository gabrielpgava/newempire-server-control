package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gabrielpgava/newempire-server-control/internal/api"
	"github.com/gabrielpgava/newempire-server-control/internal/auth"
	"github.com/gabrielpgava/newempire-server-control/internal/backups"
	"github.com/gabrielpgava/newempire-server-control/internal/config"
	"github.com/gabrielpgava/newempire-server-control/internal/database"
	"github.com/gabrielpgava/newempire-server-control/internal/files"
	"github.com/gabrielpgava/newempire-server-control/internal/instances"
	"github.com/gabrielpgava/newempire-server-control/internal/jobs"
	"github.com/gabrielpgava/newempire-server-control/internal/lock"
	"github.com/gabrielpgava/newempire-server-control/internal/model"
	"github.com/gabrielpgava/newempire-server-control/internal/modrinth"
	serverruntime "github.com/gabrielpgava/newempire-server-control/internal/runtime"
	"github.com/gabrielpgava/newempire-server-control/internal/secrets"
)

type offlinePanel struct {
	app      *api.App
	root     *os.Root
	release  func()
	actor    string
	operator map[string]any
}

func openOfflinePanel(c config.Config) (*offlinePanel, error) {
	root, err := privateDataRoot(c, false)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			root.Close()
		}
	}()
	for _, name := range []string{"panel.db", "master.key"} {
		f, err := openPrivateFile(root, name)
		if err != nil {
			return nil, fmt.Errorf("existing initialized %s is required; restore matching panel state and master key, do not bootstrap recovery: %w", name, err)
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
	}
	release, err := lock.Acquire(filepath.Join(c.DataDir, "panel.lock"))
	if err != nil {
		return nil, fmt.Errorf("stop the panel service before offline recovery; do not remove panel.lock: %w", err)
	}
	defer func() {
		if !ok {
			release()
		}
	}()
	db, err := database.OpenExisting(filepath.Join(c.DataDir, "panel.db"))
	if err != nil {
		return nil, err
	}
	defer func() {
		if !ok {
			db.Close()
		}
	}()
	vault, err := secrets.OpenExisting(filepath.Join(c.DataDir, "master.key"))
	if err != nil {
		return nil, err
	}
	if err := db.Integrity(context.Background()); err != nil {
		return nil, err
	}
	var owners int
	if err := db.QueryRow("SELECT count(*) FROM users WHERE role='owner'").Scan(&owners); err != nil {
		return nil, err
	}
	if owners == 0 {
		return nil, errors.New("panel state has no initialized owner; restore the existing panel database, do not initialize a new one during recovery")
	}
	operator := map[string]any{"uid": os.Getuid(), "effective_uid": os.Geteuid(), "gid": os.Getgid()}
	if u, err := user.Current(); err == nil && u.Uid == strconv.Itoa(os.Geteuid()) {
		operator["username"] = u.Username
	}
	p := &offlinePanel{app: api.New(c, db, vault, serverruntime.NewSystemd(c.RuntimeHelper), version, nil), root: root, release: release, actor: "local:uid:" + strconv.Itoa(os.Getuid()), operator: operator}
	p.app.RegisterJobs()
	p.registerRecoveryJobs()
	ok = true
	return p, nil
}

func withOfflinePanel(c config.Config, operation func(context.Context, *offlinePanel) error) (err error) {
	p, err := openOfflinePanel(c)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, p.app.DB.Close(), p.root.Close())
		p.release()
	}()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return operation(ctx, p)
}

func (p *offlinePanel) audit(ctx context.Context, action, instance string, detail any) error {
	b, err := json.Marshal(map[string]any{"operator": p.operator, "request": detail})
	if err != nil {
		return err
	}
	_, err = p.app.DB.ExecContext(ctx, "INSERT INTO audit(time,user_id,action,instance_id,detail) VALUES(?,?,?,?,?)", time.Now().Unix(), p.actor, action, instance, string(b))
	return err
}

func (p *offlinePanel) runJob(ctx context.Context, instance, kind string, payload any) (jobs.Job, error) {
	if err := p.audit(ctx, "offline."+kind+".requested", instance, payload); err != nil {
		return jobs.Job{}, err
	}
	// Zero workers only mark orphaned running jobs interrupted while we hold panel.lock.
	if err := p.app.Jobs.Start(ctx, 0); err != nil {
		return jobs.Job{}, err
	}
	j, err := p.app.Jobs.Enqueue(ctx, instance, kind, payload, "", 0)
	if err != nil {
		return j, err
	}
	if err := p.audit(ctx, "offline."+kind+".job", instance, map[string]string{"job_id": j.ID}); err != nil {
		return j, errors.Join(err, p.app.Jobs.Cancel(context.WithoutCancel(ctx), j.ID))
	}
	fmt.Fprintf(os.Stderr, "Running offline job %s (%s); unrelated queued jobs will not run.\n", j.ID, kind)
	jobID := j.ID
	progressCtx, stopProgress := context.WithCancel(ctx)
	var progress sync.WaitGroup
	progress.Add(1)
	go func() {
		defer progress.Done()
		last := ""
		for {
			select {
			case <-progressCtx.Done():
				return
			case <-p.app.Jobs.Events:
				current, err := p.app.Jobs.Get(progressCtx, jobID)
				if err == nil && current.Step != "" && current.Step != last {
					fmt.Fprintf(os.Stderr, "%s: %d%% %s\n", current.ID, current.Progress, current.Step)
					last = current.Step
				}
			}
		}
	}()
	j, err = p.app.Jobs.RunOne(ctx, j.ID)
	stopProgress()
	progress.Wait()
	if err != nil {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		current, lookupErr := p.app.Jobs.Get(cleanup, jobID)
		var cancelErr error
		if lookupErr == nil {
			j = current
			if j.State == "queued" {
				cancelErr = p.app.Jobs.Cancel(cleanup, jobID)
			}
		}
		return j, fmt.Errorf("offline job %s could not complete: %w", jobID, errors.Join(err, lookupErr, cancelErr))
	}
	if j.State != "succeeded" {
		return j, fmt.Errorf("offline job %s %s: %s; retained recovery data was not deleted", j.ID, j.State, j.Error)
	}
	return j, nil
}

func safeInstanceParent(c config.Config, i model.Instance) (*os.Root, string, error) {
	for _, base := range append([]string{c.InstanceDir}, c.AdoptRoots...) {
		rel, err := instances.Relative(base, i.Root)
		if err != nil {
			continue
		}
		root, err := os.OpenRoot(base)
		if err != nil {
			return nil, "", err
		}
		info, err := files.Check(root, filepath.ToSlash(filepath.Dir(rel)))
		if err != nil || !info.IsDir() {
			root.Close()
			return nil, "", errors.Join(err, files.ErrUnsafeType)
		}
		parent, err := root.OpenRoot(filepath.Dir(rel))
		root.Close()
		if err != nil {
			return nil, "", err
		}
		target := filepath.Base(rel)
		if info, err := files.Check(parent, target); err != nil && !errors.Is(err, os.ErrNotExist) || err == nil && !info.IsDir() {
			parent.Close()
			return nil, "", errors.Join(err, files.ErrUnsafeType)
		}
		return parent, target, nil
	}
	return nil, "", errors.New("instance directory is outside administrator-allowlisted roots")
}

func (p *offlinePanel) backupRoot(id string) (*os.Root, error) {
	base, err := privateDataRoot(config.Config{DataDir: p.app.Config.BackupDir}, false)
	if err != nil {
		return nil, err
	}
	defer base.Close()
	info, err := files.Check(base, id)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, files.ErrUnsafeType
	}
	return base.OpenRoot(id)
}

func offlineBackupCommand(c config.Config, args []string, confirm bool) error {
	if err := validateBackupArgs(args, confirm); err != nil {
		return err
	}
	return withOfflinePanel(c, func(ctx context.Context, p *offlinePanel) error {
		result, err := p.backup(ctx, args, confirm)
		if err != nil {
			return err
		}
		return output(result)
	})
}

func (p *offlinePanel) backup(ctx context.Context, args []string, confirm bool) (any, error) {
	if err := validateBackupArgs(args, confirm); err != nil {
		return nil, err
	}
	action, id := args[0], args[1]
	i, err := p.app.Instances.Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("instance not found or its credentials cannot be recovered: %w", err)
	}
	parent, _, err := safeInstanceParent(p.app.Config, i)
	if err != nil {
		return nil, err
	}
	parent.Close()
	if action == "create" || action == "restore" {
		if err := p.requireRestoreSettled(ctx, id); err != nil {
			return nil, err
		}
	}
	if action == "create" {
		base, err := privateDataRoot(config.Config{DataDir: p.app.Config.BackupDir}, false)
		if err != nil {
			return nil, err
		}
		info, err := files.Check(base, id)
		base.Close()
		if err != nil && !errors.Is(err, os.ErrNotExist) || err == nil && !info.IsDir() {
			return nil, errors.Join(err, files.ErrUnsafeType)
		}
		return p.runJob(ctx, id, "backup_offline", map[string]string{"note": "Created through stopped-panel offline CLI"})
	}
	storage, err := p.backupRoot(id)
	if errors.Is(err, os.ErrNotExist) && action == "list" {
		return []backups.Backup{}, p.audit(ctx, "offline.backup.list", id, nil)
	}
	if err != nil {
		return nil, err
	}
	defer storage.Close()
	if err := p.audit(ctx, "offline.backup."+action, id, args[2:]); err != nil {
		return nil, err
	}
	switch action {
	case "list":
		return backups.List(ctx, storage)
	case "verify":
		return p.runJob(ctx, id, "backup_verify", map[string]string{"backup_id": args[2]})
	case "preview", "restore":
		m, err := backups.Verify(ctx, storage, args[2], nil)
		if err != nil {
			return nil, err
		}
		if _, err := p.app.BackupInstance(ctx, storage, args[2], i); err != nil {
			return nil, err
		}
		if action == "preview" {
			return map[string]any{"files": m.Entries, "file_count": len(m.Entries), "total_bytes": m.Bytes, "warnings": []string{"Restore replaces world progress, runtime, configuration, and content. It creates a verified safety backup and retains the displaced tree. Minecraft remains stopped."}}, nil
		}
		if err := p.app.AssertStopped(ctx, i); err != nil {
			return nil, err
		}
		return p.runJob(ctx, id, "restore", map[string]string{"backup_id": args[2]})
	}
	return nil, errors.New("unsupported backup subcommand")
}

func validateRecoveryArgs(args []string, confirm, passwordStdin bool) error {
	if len(args) == 0 {
		if passwordStdin {
			return errors.New("--password-stdin is supported only by recover owner")
		}
		return nil
	}
	count := 3
	switch args[0] {
	case "owner":
		if len(args) != 2 || !passwordStdin || !confirm {
			return errors.New("recover owner requires --confirm --password-stdin and exactly one existing owner username; this resets its password, removes its factor, and revokes all owner sessions")
		}
		if len(args[1]) < 3 || len(args[1]) > 64 || strings.IndexFunc(args[1], func(c rune) bool {
			return !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.')
		}) >= 0 {
			return errors.New("invalid existing owner username")
		}
		return nil
	case "list", "runtime", "restore", "restore-previous":
		count = 2
	case "filesystem", "content", "file":
	default:
		return errors.New("recover supports list, restore, restore-previous, filesystem, content, file, runtime, or owner; use update --independent rollback for binary recovery")
	}
	if passwordStdin || len(args) != count {
		return errors.New("invalid recovery argument count; passwords are accepted only for recover owner")
	}
	if !model.ValidID(args[1]) {
		return errors.New("invalid instance identifier")
	}
	if count == 3 {
		id := args[2]
		if args[0] == "filesystem" {
			if !strings.HasPrefix(id, ".nesc-recovery-") {
				return errors.New("filesystem recovery requires the exact .nesc-recovery-ID from recover list")
			}
			id = strings.TrimPrefix(id, ".nesc-recovery-")
		}
		if !model.ValidID(id) {
			return errors.New("invalid recovery identifier")
		}
	}
	if args[0] != "list" && args[0] != "restore" && args[0] != "restore-previous" && !confirm {
		return errors.New("recovery can replace files or world progress; inspect recover list, then repeat with --confirm before the subcommand")
	}
	return nil
}

func recoveryCommand(c config.Config, args []string, confirm, passwordStdin bool, input io.Reader) error {
	if err := validateRecoveryArgs(args, confirm, passwordStdin); err != nil {
		return err
	}
	return withOfflinePanel(c, func(ctx context.Context, p *offlinePanel) error {
		if len(args) == 0 {
			return p.inspect(ctx)
		}
		if args[0] == "owner" {
			if err := p.audit(ctx, "offline.owner.requested", "", map[string]any{"username": args[1], "reset_factor": true, "revoke_all_owner_sessions": true}); err != nil {
				return err
			}
			password, _, err := readCLICredentials(input, "", false)
			if err != nil {
				return err
			}
			if err := p.recoverOwner(ctx, args[1], password); err != nil {
				return err
			}
			return output(map[string]string{"status": "owner credentials reset; factor removed; all owner sessions revoked", "username": args[1]})
		}
		result, err := p.recoverInstance(ctx, args, confirm)
		if err != nil {
			return err
		}
		return output(result)
	})
}

func (p *offlinePanel) inspect(ctx context.Context) error {
	if err := p.audit(ctx, "offline.recovery.inspect", "", nil); err != nil {
		return err
	}
	rows, err := p.app.DB.QueryContext(ctx, "SELECT id,instance_id,kind,state,step,error FROM jobs WHERE state IN ('queued','running','interrupted','failed') ORDER BY created_at DESC LIMIT 100")
	if err != nil {
		return err
	}
	defer rows.Close()
	all := []map[string]string{}
	for rows.Next() {
		var id, instance, kind, state, step, message string
		if err := rows.Scan(&id, &instance, &kind, &state, &step, &message); err != nil {
			return err
		}
		all = append(all, map[string]string{"id": id, "instance_id": instance, "kind": kind, "state": state, "step": step, "error": message})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return output(map[string]any{"database": "verified", "jobs_requiring_review": all, "note": "No queued job ran. No world, content, or credential was replaced. Use recover list INSTANCE_ID to inspect retained recovery records."})
}

func (p *offlinePanel) recoverOwner(ctx context.Context, username, password string) error {
	var id string
	var encrypted []byte
	if err := p.app.DB.QueryRowContext(ctx, "SELECT id,totp FROM users WHERE username=? AND role='owner'", username).Scan(&id, &encrypted); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("existing owner account not found; emergency recovery never creates or promotes accounts")
		}
		return err
	}
	if len(encrypted) > 0 {
		if _, err := p.app.Vault.Open(encrypted, "totp:"+id); err != nil {
			return errors.New("owner factor cannot be decrypted; restore the matching master key before account recovery")
		}
	}
	hash, err := auth.PasswordHash(password)
	if err != nil {
		return err
	}
	tx, err := p.app.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "UPDATE users SET password_hash=?,totp=NULL WHERE id=? AND role='owner'", hash, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE user_id IN (SELECT id FROM users WHERE role='owner')"); err != nil {
		return err
	}
	detail, err := json.Marshal(map[string]any{"operator": p.operator, "owner_id": id, "factor_removed": len(encrypted) > 0, "all_owner_sessions_revoked": true})
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO audit(time,user_id,action,detail) VALUES(?,?,?,?)", time.Now().Unix(), p.actor, "offline.owner.recovered", string(detail)); err != nil {
		return err
	}
	return tx.Commit()
}

func (p *offlinePanel) recoverInstance(ctx context.Context, args []string, confirm bool) (any, error) {
	if err := validateRecoveryArgs(args, confirm, false); err != nil {
		return nil, err
	}
	if len(args) < 2 {
		return nil, errors.New("instance recovery requires a subcommand and an instance ID")
	}
	i, err := p.app.Instances.Get(ctx, args[1])
	if err != nil {
		return nil, fmt.Errorf("instance not found or its credentials cannot be recovered: %w", err)
	}
	parent, _, err := safeInstanceParent(p.app.Config, i)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	if args[0] == "list" {
		if err := p.audit(ctx, "offline.recovery.list", i.ID, nil); err != nil {
			return nil, err
		}
		return p.recoveryRecords(ctx, i)
	}
	if args[0] == "restore" || args[0] == "restore-previous" {
		pending, err := p.pendingRestore(ctx, i.ID)
		if err != nil {
			return nil, err
		}
		if pending == nil {
			return nil, errors.New("no pending full-tree restore was recorded for this instance")
		}
		previous := args[0] == "restore-previous"
		if !confirm {
			if err := p.audit(ctx, "offline.restore.preview", i.ID, map[string]bool{"use_previous": previous}); err != nil {
				return nil, err
			}
			return map[string]any{"pending": pending, "use_previous": previous, "warnings": []string{"Reconciliation changes database/runtime-helper metadata only, never files or worlds. The current tree must exactly match the selected recorded verified backup. Minecraft remains stopped."}}, nil
		}
		if err := p.app.AssertStopped(ctx, i); err != nil {
			return nil, err
		}
		return p.runJob(ctx, i.ID, "restore_reconcile", map[string]bool{"use_previous": previous, "confirm": true})
	}
	if err := p.requireRestoreSettled(ctx, i.ID); err != nil {
		return nil, err
	}
	if err := p.app.AssertStopped(ctx, i); err != nil {
		return nil, err
	}
	switch args[0] {
	case "file":
		var record struct {
			Token string `json:"token"`
			Path  string `json:"path"`
		}
		var raw string
		if err := p.app.DB.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=?", "recovery:"+i.ID+":"+args[2]).Scan(&raw); err != nil {
			return nil, errors.New("file recovery record not found")
		}
		if json.Unmarshal([]byte(raw), &record) != nil || record.Token != args[2] || files.ValidatePath(record.Path) != nil {
			return nil, errors.New("invalid file recovery record")
		}
		info, err := files.Check(parent, ".nesc-recovery-"+i.ID+"/"+record.Token)
		if err != nil || !info.IsDir() && !info.Mode().IsRegular() {
			return nil, errors.Join(err, files.ErrUnsafeType)
		}
		return p.runJob(ctx, i.ID, "file", map[string]any{"operation": "recover", "token": args[2], "confirm": true})
	case "filesystem", "content", "runtime":
		payload := recoveryRequest{Confirm: true}
		if len(args) == 3 {
			payload.ID = args[2]
		}
		return p.runJob(ctx, i.ID, "recovery_"+args[0], payload)
	}
	return nil, errors.New("unsupported recovery operation")
}

type recoveryRequest struct {
	ID      string `json:"id"`
	Confirm bool   `json:"confirm"`
}

func (p *offlinePanel) registerRecoveryJobs() {
	p.app.Jobs.Register("recovery_content", 0, func(ctx context.Context, t *jobs.Task, raw json.RawMessage) (any, error) {
		var body recoveryRequest
		if err := json.Unmarshal(raw, &body); err != nil || !body.Confirm || !model.ValidID(body.ID) {
			return nil, errors.New("confirmed exact content transaction ID is required")
		}
		i, err := p.app.Instances.Get(ctx, t.Job.InstanceID)
		if err != nil {
			return nil, err
		}
		if err := p.requireRestoreSettled(ctx, i.ID); err != nil {
			return nil, err
		}
		parent, target, err := safeInstanceParent(p.app.Config, i)
		if err != nil {
			return nil, err
		}
		defer parent.Close()
		root, err := parent.OpenRoot(target)
		if err != nil {
			return nil, err
		}
		defer root.Close()
		tx, err := modrinth.LoadTransaction(root, body.ID)
		if err != nil {
			return nil, err
		}
		if err := p.app.AssertStopped(ctx, i); err != nil {
			return nil, err
		}
		backup, err := p.app.RecoveryBackup(ctx, t, i, "Before explicit offline content rollback "+body.ID)
		if err != nil {
			return nil, err
		}
		if err := t.CommitBarrier(ctx); err != nil {
			return nil, err
		}
		err = modrinth.Rollback(ctx, root, tx)
		return map[string]any{"transaction": tx, "pre_recovery_backup_id": backup.ID, "world_data": "unchanged", "state": "offline", "note": "Displaced content and its receipt are retained; no finalization or world restore was requested."}, err
	})
	p.app.Jobs.Register("recovery_filesystem", 0, p.recoverFilesystemJob)
	p.app.Jobs.Register("recovery_runtime", 0, func(ctx context.Context, t *jobs.Task, raw json.RawMessage) (any, error) {
		var body recoveryRequest
		if json.Unmarshal(raw, &body) != nil || !body.Confirm || body.ID != "" {
			return nil, errors.New("explicit runtime recovery confirmation is required")
		}
		recovery, ok := any(p.app).(interface {
			RecoverRuntime(context.Context, *jobs.Task, string) (any, error)
		})
		if !ok {
			return nil, errors.New("this binary does not implement verified offline runtime recovery; preserve runtime-pending state and staging, and use a recovery-capable official binary")
		}
		return recovery.RecoverRuntime(ctx, t, t.Job.InstanceID)
	})
}
