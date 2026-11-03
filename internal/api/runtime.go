package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/gabrielpgava/newempire-server-control/internal/fabric"
	"github.com/gabrielpgava/newempire-server-control/internal/files"
	"github.com/gabrielpgava/newempire-server-control/internal/jobs"
	"github.com/gabrielpgava/newempire-server-control/internal/model"
	"github.com/gabrielpgava/newempire-server-control/internal/modrinth"
)

type runtimeRequest struct {
	MinecraftVersion string `json:"minecraft_version"`
	LoaderVersion    string `json:"loader_version"`
	InstallerVersion string `json:"installer_version"`
	JavaPath         string `json:"java_path,omitempty"`
	PlanHash         string `json:"plan_hash,omitempty"`
	Confirm          bool   `json:"confirm,omitempty"`
}
type runtimeApproval struct {
	Runtime        fabric.InstallPlan `json:"runtime"`
	Content        *modrinth.Plan     `json:"content"`
	JavaPath       string             `json:"java_path"`
	PropertiesHash string             `json:"properties_hash"`
	Current        model.Instance     `json:"current"`
}
type runtimeRecovery struct {
	Directory          string         `json:"directory"`
	Previous           model.Instance `json:"previous"`
	State              string         `json:"state"`
	BackupID           string         `json:"backup_id"`
	ContentTransaction string         `json:"content_transaction"`
}

func (a *App) runtimePlan(ctx context.Context, i model.Instance, body runtimeRequest) (runtimeApproval, error) {
	selection, err := a.selection(ctx, body.MinecraftVersion, body.LoaderVersion, body.InstallerVersion)
	if err != nil {
		return runtimeApproval{}, err
	}
	plan, err := a.Fabric.Plan(ctx, selection)
	if err != nil {
		return runtimeApproval{}, err
	}
	javaPath := body.JavaPath
	if javaPath == "" {
		javaPath = i.JavaPath
	}
	if !slices.Contains(a.Config.JavaPaths, javaPath) {
		return runtimeApproval{}, errors.New("selected Java executable is not administrator-allowlisted")
	}
	java, err := fabric.InspectJava(ctx, javaPath, a.Config.JavaPaths)
	if err != nil {
		return runtimeApproval{}, err
	}
	if java.Major < plan.JavaMajor {
		return runtimeApproval{}, fmt.Errorf("this migration requires Java %d or newer", plan.JavaMajor)
	}
	root, err := a.Instances.OpenRoot(i.Root)
	if err != nil {
		return runtimeApproval{}, err
	}
	defer root.Close()
	props, err := readOptionalText(ctx, root, "server.properties")
	if err != nil {
		return runtimeApproval{}, err
	}
	inventory, err := a.Content.Inventory(ctx, root)
	if err != nil {
		return runtimeApproval{}, err
	}
	if i.MinecraftVersion != selection.MinecraftVersion {
		for _, f := range inventory.Files {
			if !f.Managed {
				return runtimeApproval{}, errors.New("Minecraft migration is blocked by externally managed mods; reconcile exact compatible versions before proceeding")
			}
		}
	}
	p, err := a.Content.Plan(ctx, root, modrinth.Request{Target: modrinth.Target{MinecraftVersion: selection.MinecraftVersion, Loader: "fabric", LoaderVersion: selection.LoaderVersion}})
	if err != nil {
		return runtimeApproval{}, err
	}
	return runtimeApproval{Runtime: plan, Content: p, JavaPath: javaPath, PropertiesHash: etag(props), Current: i}, nil
}
func runtimeWire(p runtimeApproval) map[string]any {
	warnings := append([]string{}, p.Runtime.Warnings...)
	warnings = append(warnings, p.Content.Rationale...)
	warnings = append(warnings, p.Content.Blockers...)
	if p.Current.MinecraftVersion != p.Runtime.MinecraftVersion {
		warnings = append(warnings, "Minecraft upgrades can change world formats. Failed readiness restores runtime/content only and blocks the old runtime until an administrator explicitly restores the pre-upgrade world backup.")
	}
	return map[string]any{"plan_hash": etagJSON(p), "warnings": warnings, "blocked": p.Content.Blockers, "java_required": p.Runtime.JavaMajor, "changes": []string{"Minecraft: " + p.Current.MinecraftVersion + " → " + p.Runtime.MinecraftVersion, "Fabric Loader: " + p.Current.LoaderVersion + " → " + p.Runtime.LoaderVersion, "Java executable: " + p.JavaPath}}
}
func (a *App) planRuntime(w http.ResponseWriter, r *http.Request) {
	var body runtimeRequest
	if err := decode(r, &body); err != nil {
		fail(w, 400, err)
		return
	}
	p, err := a.runtimePlan(r.Context(), instance(r), body)
	if err != nil {
		fail(w, 400, err)
		return
	}
	respond(w, 200, runtimeWire(p))
}
func (a *App) applyRuntime(w http.ResponseWriter, r *http.Request) {
	var body runtimeRequest
	if err := decode(r, &body); err != nil {
		fail(w, 400, err)
		return
	}
	if !body.Confirm || body.PlanHash == "" {
		fail(w, 400, errors.New("review and explicitly approve the exact runtime migration plan"))
		return
	}
	a.enqueue(w, r, instance(r).ID, "runtime", body)
}

func atomicBinary(ctx context.Context, source *os.Root, from string, dest *os.Root, to string, max int64) error {
	if err := files.ValidatePath(to); err != nil {
		return err
	}
	in, err := files.OpenRegular(source, from)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := dest.MkdirAll(path.Dir(to), 0770); err != nil {
		return err
	}
	old, statErr := files.Check(dest, to)
	if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
		return statErr
	}
	if statErr == nil && !old.Mode().IsRegular() {
		return files.ErrUnsafeType
	}
	tmp := path.Join(path.Dir(to), ".nesc-runtime-"+model.NewID())
	out, err := dest.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer dest.Remove(tmp)
	_, err = files.Copy(ctx, out, in, max, nil)
	if err == nil && statErr == nil {
		if stat, ok := old.Sys().(*syscall.Stat_t); ok {
			current, statErr := out.Stat()
			if statErr != nil {
				out.Close()
				return statErr
			}
			now, ok := current.Sys().(*syscall.Stat_t)
			if !ok {
				out.Close()
				return errors.New("runtime file ownership is unavailable")
			}
			if now.Uid != stat.Uid || now.Gid != stat.Gid {
				err = out.Chown(int(stat.Uid), int(stat.Gid))
			}
			if err == nil {
				err = out.Chmod(old.Mode().Perm() & 0666)
			}
		} else {
			err = errors.New("cannot preserve runtime-file ownership on this platform")
		}
	}
	err = errors.Join(err, out.Sync(), out.Close())
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := dest.Rename(tmp, to); err != nil {
		return err
	}
	dir, err := dest.Open(path.Dir(to))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func (a *App) stopMinecraft(ctx context.Context, i model.Instance) error {
	status, _ := a.runtimeStatus(ctx, i)
	if status.Ready {
		graceful, cancel := context.WithTimeout(ctx, 5*time.Second)
		a.runtimeCommand(graceful, i, "stop")
		cancel()
	}
	return a.Runtime.Stop(ctx, i)
}
func (a *App) restoreRuntime(ctx context.Context, i model.Instance, recovery runtimeRecovery) error {
	if err := a.assertStopped(ctx, i); err != nil {
		return err
	}
	root, err := a.Instances.OpenRoot(i.Root)
	if err != nil {
		return err
	}
	defer root.Close()
	stageBase, err := os.OpenRoot(filepath.Join(a.Config.DataDir, "staging"))
	if err != nil {
		return err
	}
	defer stageBase.Close()
	if err := validRuntimeRecoveryDirectory(recovery.Directory); err != nil {
		return err
	}
	old, err := stageBase.OpenRoot(recovery.Directory)
	if err != nil {
		return err
	}
	defer old.Close()
	if recovery.Previous.MinecraftVersion != i.MinecraftVersion {
		if err := a.saveSetting(ctx, "runtime-world-review:"+i.ID, recovery.BackupID); err != nil {
			return err
		}
	}
	if err := atomicBinary(ctx, old, "launcher.jar", root, recovery.Previous.Jar, fabric.MaxLauncherBytes); err != nil {
		return err
	}
	props, err := files.ReadText(ctx, old, "server.properties", 1<<20)
	if err != nil {
		return err
	}
	if err := files.WriteText(ctx, root, "server.properties", props, 1<<20); err != nil {
		return err
	}
	if recovery.ContentTransaction != "" {
		tx, err := modrinth.LoadTransaction(root, recovery.ContentTransaction)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if tx != nil && tx.State != "rolled_back" {
			if err := modrinth.Rollback(ctx, root, tx); err != nil {
				return err
			}
		}
		if tx != nil {
			if err := modrinth.Finalize(root, tx); err != nil {
				return err
			}
		}
	}
	previous := recovery.Previous
	previous.RCONSecret = i.RCONSecret
	if err := a.provision(ctx, previous, true); err != nil {
		return err
	}
	if err := a.Instances.Update(ctx, previous); err != nil {
		return err
	}
	_, err = a.DB.ExecContext(ctx, "DELETE FROM settings WHERE key=?", "runtime-pending:"+i.ID)
	return err
}

func validRuntimeRecoveryDirectory(name string) error {
	if !strings.HasPrefix(name, "runtime-recovery-") || !model.ValidID(strings.TrimPrefix(name, "runtime-recovery-")) {
		return errors.New("invalid retained runtime recovery directory")
	}
	return nil
}
func (a *App) prepareRuntimeStart(ctx context.Context, i model.Instance) error {
	if err := a.checkRestorePending(ctx, i.ID); err != nil {
		return err
	}
	var review string
	if err := a.setting(ctx, "runtime-world-review:"+i.ID, &review); err != nil {
		return err
	}
	if review != "" {
		return fmt.Errorf("the prior Minecraft runtime may not understand the upgraded world; explicitly restore pre-upgrade backup %s before starting the restored runtime", review)
	}
	var recovery runtimeRecovery
	if err := a.setting(ctx, "runtime-pending:"+i.ID, &recovery); err != nil {
		return err
	}
	if recovery.Directory != "" && recovery.State == "prepared" {
		return errors.Join(errors.New("an interrupted runtime commit was recovered; inspect the instance and retry start"), a.restoreRuntime(ctx, i, recovery))
	}
	return nil
}
func (a *App) completeRuntimeStart(ctx context.Context, i model.Instance, healthy bool) error {
	var recovery runtimeRecovery
	if err := a.setting(ctx, "runtime-pending:"+i.ID, &recovery); err != nil {
		return err
	}
	if recovery.Directory == "" {
		return nil
	}
	if !healthy {
		if err := a.stopMinecraft(ctx, i); err != nil {
			return err
		}
		return a.restoreRuntime(ctx, i, recovery)
	}
	_, err := a.DB.ExecContext(ctx, "DELETE FROM settings WHERE key IN (?,?)", "runtime-pending:"+i.ID, "runtime-world-review:"+i.ID)
	return err
}
func (a *App) registerRuntimeJobs() {
	a.Jobs.Register("runtime", 0, func(ctx context.Context, t *jobs.Task, raw json.RawMessage) (any, error) {
		var body runtimeRequest
		if err := json.Unmarshal(raw, &body); err != nil {
			return nil, err
		}
		i, err := a.Instances.Get(ctx, t.Job.InstanceID)
		if err != nil {
			return nil, err
		}
		p, err := a.runtimePlan(ctx, i, body)
		if err != nil {
			return nil, err
		}
		if etagJSON(p) != body.PlanHash {
			return nil, errors.New("runtime migration plan changed; review it again")
		}
		if len(p.Content.Blockers) > 0 {
			return runtimeWire(p), errors.New("runtime migration is blocked by content compatibility")
		}
		root, err := a.Instances.OpenRoot(i.Root)
		if err != nil {
			return nil, err
		}
		defer root.Close()
		if err := contentSettled(root); err != nil {
			return nil, err
		}
		props, err := files.ReadText(ctx, root, "server.properties", 1<<20)
		if err != nil {
			return nil, err
		}
		if err := t.Progress(ctx, 5, "Staging authoritative runtime and compatible mod downloads"); err != nil {
			return nil, err
		}
		staged, err := a.Fabric.Stage(ctx, p.Runtime.Selection, filepath.Join(a.Config.DataDir, "staging"), []byte(props))
		if err != nil {
			return nil, err
		}
		if etagJSON(staged.Plan) != etagJSON(p.Runtime) {
			return nil, errors.New("runtime upstream metadata changed during staging")
		}
		newRuntime, err := os.OpenRoot(staged.Directory)
		if err != nil {
			return nil, err
		}
		defer newRuntime.Close()
		_, contentStage, err := a.contentStage()
		if err != nil {
			return nil, err
		}
		defer contentStage.Close()
		if err := a.Content.Stage(ctx, contentStage, p.Content); err != nil {
			return nil, err
		}
		status, err := a.runtimeStatus(ctx, i)
		if err != nil {
			return nil, err
		}
		wasRunning := status.PID > 0 || status.Ready
		if wasRunning {
			if err := a.stopMinecraft(ctx, i); err != nil {
				return nil, err
			}
		}
		backup, err := a.safetyBackup(ctx, t, i, "Before Minecraft/Fabric runtime migration")
		if err != nil {
			return nil, err
		}
		parent, err := os.OpenRoot(filepath.Join(a.Config.DataDir, "staging"))
		if err != nil {
			return nil, err
		}
		defer parent.Close()
		oldName := "runtime-recovery-" + model.NewID()
		if err := parent.Mkdir(oldName, 0700); err != nil {
			return nil, err
		}
		old, err := parent.OpenRoot(oldName)
		if err != nil {
			return nil, err
		}
		defer old.Close()
		if err := atomicBinary(ctx, root, i.Jar, old, "launcher.jar", fabric.MaxLauncherBytes); err != nil {
			return nil, err
		}
		if err := files.WriteText(ctx, old, "server.properties", props, 1<<20); err != nil {
			return nil, err
		}
		previous := i
		updated := i
		updated.MinecraftVersion = p.Runtime.MinecraftVersion
		updated.LoaderVersion = p.Runtime.LoaderVersion
		updated.JavaPath = p.JavaPath
		recovery := runtimeRecovery{Directory: oldName, Previous: previous, State: "prepared", BackupID: backup.ID, ContentTransaction: p.Content.ID}
		if err := a.saveSetting(ctx, "runtime-pending:"+i.ID, recovery); err != nil {
			return nil, err
		}
		if err := t.CommitBarrier(ctx); err != nil {
			return nil, err
		}
		if err := atomicBinary(ctx, newRuntime, "launcher.jar", root, i.Jar, fabric.MaxLauncherBytes); err != nil {
			return nil, err
		}
		tx, err := modrinth.Apply(ctx, root, contentStage, p.Content)
		if err != nil {
			return map[string]any{"recovery": recovery, "content_transaction": tx}, err
		}
		if err := a.provision(ctx, updated, true); err != nil {
			return nil, err
		}
		if err := a.Instances.Update(ctx, updated); err != nil {
			return nil, err
		}
		recovery.State = "applied"
		if err := a.saveSetting(ctx, "runtime-pending:"+i.ID, recovery); err != nil {
			return nil, err
		}
		result := map[string]any{"instance": updated, "pre_upgrade_backup_id": backup.ID, "recovery": recovery, "world_data": "never automatically rolled back"}
		if !wasRunning {
			result["health"] = "not checked until the next explicit start"
			return result, nil
		}
		healthErr := a.Runtime.Start(ctx, updated)
		if healthErr == nil {
			_, healthErr = a.awaitReady(ctx, i.ID, 3*time.Minute)
		}
		if healthErr != nil {
			recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
			defer cancel()
			rollbackErr := a.completeRuntimeStart(recoveryCtx, updated, false)
			return result, errors.Join(errors.New("new runtime failed readiness; inspect retained recovery data and world compatibility before retrying"), healthErr, rollbackErr)
		}
		if err := modrinth.Finalize(root, tx); err != nil {
			return result, err
		}
		if err := a.completeRuntimeStart(ctx, updated, true); err != nil {
			return result, err
		}
		result["health"] = "ready"
		return result, nil
	})
}
