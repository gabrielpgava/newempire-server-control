package updates

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func (m *Manager) safetyContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 2*m.opts.HealthTimeout+time.Minute)
}

func (m *Manager) health(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, m.opts.HealthTimeout)
	defer cancel()
	if err := m.opts.Service.Health(ctx); err != nil {
		return fmt.Errorf("authenticated panel readiness check failed: %w", err)
	}
	return ctx.Err()
}

func (m *Manager) prepare(ctx context.Context, r *os.Root, t *Transaction) (string, error) {
	dir := stageDir(t)
	if info, err := r.Lstat(dir); err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return "", problem(ErrVerification, "staged release directory is missing or unsafe")
	}
	raw, err := limitedRead(r, dir+"/release.json", maxManifestSize)
	if err != nil {
		return "", err
	}
	sig, err := limitedRead(r, dir+"/release.json.sig", 88)
	if err != nil {
		return "", err
	}
	mf, err := verifyManifest(raw, sig, m.key)
	if err != nil {
		return "", err
	}
	if mf.Version != t.Version || mf.Schema != t.Schema || mf.Schema != m.opts.Schema {
		return "", problem(ErrConflict, "staged version/schema does not match the transaction and installed database")
	}
	v, _ := parseVersion(mf.Version)
	if t.Channel == Stable && len(v.pre) != 0 {
		return "", problem(ErrVerification, "stable transaction cannot install a prerelease")
	}
	asset, err := mf.asset(m.arch)
	if err != nil {
		return "", err
	}
	// Hash the same stream we extract, into administrator-controlled storage, not mutable staging.
	prepared := "versions/.prepared-" + t.ID
	if err := r.RemoveAll(prepared); err != nil {
		return "", err
	}
	hash, err := m.extract(ctx, r, dir+"/archive.tar.gz", prepared, asset)
	if err != nil {
		return "", err
	}
	t.BinarySHA256 = hash
	return prepared, nil
}

// Apply must run in the independent restricted updater process, never the web process.
func (m *Manager) Apply(ctx context.Context) error {
	if err := m.helperReady(); err != nil {
		return err
	}
	r, err := m.lock(ctx)
	if err != nil {
		return err
	}
	defer r.close()
	j, err := readJournal(r.Root)
	if err != nil {
		return err
	}
	t := j.Transaction
	if t == nil || t.State != Staged {
		return problem(ErrConflict, "a verified staged release is required before apply")
	}
	current, err := currentVersion(r.Root)
	if err != nil {
		return err
	}
	if current != t.PreviousVersion {
		return problem(ErrConflict, "installed version changed after staging; stage again")
	}
	newVersion, _ := parseVersion(t.Version)
	oldVersion, _ := parseVersion(current)
	if newVersion.compare(oldVersion) <= 0 {
		return problem(ErrConflict, "update must advance SemVer precedence")
	}
	if _, err := r.Lstat(versionDir(t.Version)); err == nil {
		return problem(ErrConflict, "target version directory already exists; existing binaries are never overwritten")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	prepared, err := m.prepare(ctx, r.Root, t)
	if err != nil {
		return fail(r.Root, &j, err)
	}
	if err := validateELF(r.Root, versionDir(current)+"/"+BinaryName, m.arch); err != nil {
		return fail(r.Root, &j, err)
	}
	old, err := regular(r.Root, versionDir(current)+"/"+BinaryName, os.O_RDONLY, 0)
	if err != nil {
		return fail(r.Root, &j, err)
	}
	info, err := old.Stat()
	old.Close()
	if err != nil {
		return fail(r.Root, &j, err)
	}
	if info.Mode().Perm()&0022 != 0 || info.Size() <= 0 || info.Size() > maxBinaryBytes {
		return fail(r.Root, &j, problem(ErrVerification, "installed binary has unsafe permissions or size"))
	}
	t.PreviousSHA256, _, err = fileHash(ctx, r.Root, versionDir(current)+"/"+BinaryName)
	if err != nil {
		return fail(r.Root, &j, err)
	}
	if err := transition(r.Root, &j, Stopping, "Stopping only the panel before a verified database/configuration snapshot"); err != nil {
		return err
	}
	if err := m.opts.Service.StopPanel(ctx); err != nil {
		return m.restartPrevious(ctx, r.Root, &j, fmt.Errorf("stop panel: %w", err))
	}
	if err := transition(r.Root, &j, Snapshotting, "Panel stopped; taking consistent panel-only recovery snapshot"); err != nil {
		return m.restartPrevious(ctx, r.Root, &j, err)
	}
	t.SnapshotSHA256, err = m.snapshot(ctx, r.Root, t)
	if err != nil {
		return m.restartPrevious(ctx, r.Root, &j, err)
	}
	if err := ctx.Err(); err != nil {
		return m.restartPrevious(ctx, r.Root, &j, err)
	}
	commitCtx, cancel := m.safetyContext(ctx)
	defer cancel()
	if err := transition(r.Root, &j, Installing, "Verified snapshot complete; installing a new immutable panel version"); err != nil {
		return m.restartPrevious(commitCtx, r.Root, &j, err)
	}
	if err := r.Mkdir(versionDir(t.Version), 0755); err != nil {
		return m.applyFailed(commitCtx, r.Root, &j, err)
	}
	versionRoot, err := r.Open(versionDir(t.Version))
	if err != nil {
		return m.applyFailed(commitCtx, r.Root, &j, err)
	}
	err = versionRoot.Chmod(0755)
	versionRoot.Close()
	if err != nil {
		return m.applyFailed(commitCtx, r.Root, &j, err)
	}
	if err := r.Rename(prepared+"/"+BinaryName, versionDir(t.Version)+"/"+BinaryName); err != nil {
		return m.applyFailed(commitCtx, r.Root, &j, err)
	}
	if err := syncDir(r.Root, versionDir(t.Version)); err != nil {
		return m.applyFailed(commitCtx, r.Root, &j, err)
	}
	if err := syncDir(r.Root, "versions"); err != nil {
		return m.applyFailed(commitCtx, r.Root, &j, err)
	}
	if err := switchLink(r.Root, "previous", t.PreviousVersion); err != nil {
		return m.applyFailed(commitCtx, r.Root, &j, err)
	}
	if err := switchLink(r.Root, "current", t.Version); err != nil {
		return m.applyFailed(commitCtx, r.Root, &j, err)
	}
	if err := transition(r.Root, &j, CheckingHealth, "Restarting only the panel and checking authenticated readiness"); err != nil {
		return m.applyFailed(commitCtx, r.Root, &j, err)
	}
	if err := m.opts.Service.RestartPanel(commitCtx); err != nil {
		return m.applyFailed(commitCtx, r.Root, &j, fmt.Errorf("restart updated panel: %w", err))
	}
	if err := m.health(commitCtx); err != nil {
		return m.applyFailed(commitCtx, r.Root, &j, err)
	}
	t.Error = ""
	t.State = Complete
	j.LastApplied = cloneTransaction(t)
	if err := transition(r.Root, &j, Complete, "Updated panel passed authenticated readiness; previous version and snapshot retained"); err != nil {
		return m.applyFailed(commitCtx, r.Root, &j, err)
	}
	return nil
}

func (m *Manager) restartPrevious(ctx context.Context, r *os.Root, j *Journal, cause error) error {
	safeCtx, cancel := m.safetyContext(ctx)
	defer cancel()
	t := j.Transaction
	t.Error = cause.Error()
	if t.Schema != m.opts.Schema {
		err := problem(ErrConflict, "transaction schema %d differs from configured schema %d; refusing to restart the previous panel", t.Schema, m.opts.Schema)
		t.Error = err.Error()
		return errors.Join(err, transition(r, j, ManualRecovery, err.Error()))
	}
	current, err := currentVersion(r)
	if err == nil && current != t.PreviousVersion {
		err = problem(ErrRecovery, "current changed before installation; refusing to restart an unrecognized version")
	}
	if err == nil {
		err = m.opts.Service.RestartPanel(safeCtx)
	}
	if err == nil {
		err = m.health(safeCtx)
	}
	if err != nil {
		combined := problem(ErrRecovery, "%s; restarting unchanged panel failed: %s", cause, err)
		t.Error = combined.Error()
		return errors.Join(combined, transition(r, j, ManualRecovery, combined.Error()))
	}
	if err := transition(r, j, Failed, "Update aborted before switch; original panel restarted: "+cause.Error()); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

func (m *Manager) applyFailed(ctx context.Context, r *os.Root, j *Journal, cause error) error {
	j.Transaction.Error = cause.Error()
	safeCtx, cancel := m.safetyContext(ctx)
	defer cancel()
	if err := m.rollback(safeCtx, r, j, "Automatic rollback after update failure: "+cause.Error()); err != nil {
		return problem(ErrRecovery, "update failed: %s; automatic rollback failed: %s", cause, err)
	}
	return fmt.Errorf("panel update failed and was rolled back: %w", cause)
}

func (m *Manager) rollback(ctx context.Context, r *os.Root, j *Journal, reason string) (err error) {
	t := j.Transaction
	defer func() {
		if err != nil {
			t.Error = err.Error()
			err = errors.Join(err, transition(r, j, ManualRecovery, err.Error()))
		}
	}()
	if t.Schema != m.opts.Schema {
		return problem(ErrConflict, "snapshot schema %d differs from configured schema %d; automatic rollback is unsafe", t.Schema, m.opts.Schema)
	}
	if err := verifySnapshot(ctx, r, t); err != nil {
		return err
	}
	if err := validateELF(r, versionDir(t.PreviousVersion)+"/"+BinaryName, m.arch); err != nil {
		return err
	}
	current, err := currentVersion(r)
	if err != nil {
		return err
	}
	if current != t.Version && current != t.PreviousVersion {
		return problem(ErrRecovery, "current version differs from both transaction versions; refusing to modify it")
	}
	if err := transition(r, j, RollingBack, reason); err != nil {
		return err
	}
	if err := m.opts.Service.StopPanel(ctx); err != nil {
		return fmt.Errorf("stop panel before recovery: %w", err)
	}
	if err := m.opts.Restore(ctx, filepath.Join(r.Name(), filepath.FromSlash(snapshotDir(t)), "data")); err != nil {
		return fmt.Errorf("restore panel database/configuration: %w", err)
	}
	if err := switchLink(r, "current", t.PreviousVersion); err != nil {
		return err
	}
	if err := m.opts.Service.RestartPanel(ctx); err != nil {
		return fmt.Errorf("restart recovered panel: %w", err)
	}
	if err := m.health(ctx); err != nil {
		return err
	}
	if j.LastApplied != nil && j.LastApplied.ID == t.ID {
		j.LastApplied = nil
	}
	return transition(r, j, RolledBack, "Previous panel binary and verified database/configuration restored; authenticated readiness passed")
}

// Rollback restores the last successful same-schema panel update, not Minecraft data.
func (m *Manager) Rollback(ctx context.Context) error {
	if err := m.helperReady(); err != nil {
		return err
	}
	r, err := m.lock(ctx)
	if err != nil {
		return err
	}
	defer r.close()
	j, err := readJournal(r.Root)
	if err != nil {
		return err
	}
	if j.Transaction != nil && inProgress(j.Transaction.State) {
		return problem(ErrConflict, "recover the interrupted transaction before requesting rollback")
	}
	if j.LastApplied == nil {
		return problem(ErrConflict, "there is no successful panel update to roll back")
	}
	j.Transaction = cloneTransaction(j.LastApplied)
	safeCtx, cancel := m.safetyContext(ctx)
	defer cancel()
	return m.rollback(safeCtx, r.Root, &j, "Administrator requested panel-only rollback")
}

// Recover is usable from the independent helper even when the panel cannot start.
func (m *Manager) Recover(ctx context.Context) error {
	if err := m.helperReady(); err != nil {
		return err
	}
	r, err := m.lock(ctx)
	if err != nil {
		return err
	}
	defer r.close()
	j, err := readJournal(r.Root)
	if err != nil {
		return err
	}
	t := j.Transaction
	if t == nil {
		return nil
	}
	safeCtx, cancel := m.safetyContext(ctx)
	defer cancel()
	switch t.State {
	case Downloading:
		t.Error = "Staging was interrupted; stage the release again"
		return transition(r.Root, &j, Failed, t.Error)
	case Stopping, Snapshotting:
		cause := errors.New("Update interrupted before version switch")
		err := m.restartPrevious(safeCtx, r.Root, &j, cause)
		if err == cause {
			return nil
		}
		return err
	case Installing, CheckingHealth, RollingBack, ManualRecovery:
		if t.SnapshotSHA256 == "" {
			cause := errors.New("Retrying unchanged panel after interrupted pre-switch recovery")
			err := m.restartPrevious(safeCtx, r.Root, &j, cause)
			if err == cause {
				return nil
			}
			return err
		}
		return m.rollback(safeCtx, r.Root, &j, "Recovering interrupted panel update")
	default:
		return nil
	}
}
