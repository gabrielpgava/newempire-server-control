package backups

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"
	"syscall"

	"github.com/gabrielpgava/newempire-server-control/internal/files"
	"github.com/gabrielpgava/newempire-server-control/internal/importer"
	"github.com/gabrielpgava/newempire-server-control/internal/model"
)

type Stage struct {
	Name     string   `json:"name"`
	BackupID string   `json:"backup_id"`
	Manifest Manifest `json:"manifest"`
}

// StageRestore validates both the stored archive and the extracted staging data.
// Parent is a trusted instance-parent directory, not the instance itself.
func StageRestore(ctx context.Context, storage *os.Root, id string, parent *os.Root, progress files.Progress) (Stage, error) {
	m, err := Verify(ctx, storage, id, nil)
	if err != nil {
		return Stage{}, err
	}
	f, b, err := Open(storage, id)
	if err != nil {
		return Stage{}, err
	}
	defer f.Close()
	name := ".nesc-restore-" + model.NewID()
	if err = parent.Mkdir(name, 0700); err != nil {
		return Stage{}, err
	}
	complete := false
	defer func() {
		if !complete {
			parent.RemoveAll(name)
		}
	}()
	stage, err := parent.OpenRoot(name)
	if err != nil {
		return Stage{}, err
	}
	defer stage.Close()
	p, err := importer.Extract(ctx, f, importer.Format(b.Format), stage, importer.Limits{}, progress)
	if err != nil {
		return Stage{}, err
	}
	if err = match(m, p); err != nil {
		return Stage{}, err
	}
	manifest, err := files.OpenRegular(stage, "manifest.json")
	if err != nil {
		return Stage{}, err
	}
	h := sha256.New()
	_, err = files.Copy(ctx, h, manifest, manifestLimit, nil)
	err = errors.Join(err, manifest.Close())
	if err != nil {
		return Stage{}, err
	}
	if hex.EncodeToString(h.Sum(nil)) != b.ManifestSHA256 {
		return Stage{}, errors.New("staged backup manifest changed")
	}
	if err = syncRoot(stage); err != nil {
		return Stage{}, err
	}
	if err = syncRoot(parent); err != nil {
		return Stage{}, err
	}
	complete = true
	return Stage{Name: name + "/data", BackupID: id, Manifest: m}, nil
}

type RestoreResult struct {
	Recovery  string `json:"recovery"`
	Journal   string `json:"journal"`
	Installed bool   `json:"installed"`
}

type restoreJournal struct {
	Target   string `json:"target"`
	Stage    string `json:"stage"`
	Recovery string `json:"recovery"`
}

func directory(root *os.Root, name string) (fs.FileInfo, error) {
	info, err := files.Check(root, name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, files.ErrUnsafeType
	}
	return info, nil
}

// CommitRestore does not delete any original data. Persist the result even on
// error; a prepared journal is written and synced before the first rename.
func CommitRestore(ctx context.Context, parent *os.Root, target, stage string) (RestoreResult, error) {
	if !strings.HasPrefix(stage, ".nesc-restore-") || path.Base(stage) != "data" || strings.Count(stage, "/") != 1 {
		return RestoreResult{}, files.ErrUnsafePath
	}
	outer := path.Dir(stage)
	if !model.ValidID(strings.TrimPrefix(outer, ".nesc-restore-")) {
		return RestoreResult{}, files.ErrUnsafePath
	}
	r, err := parent.OpenRoot(outer)
	if err != nil {
		return RestoreResult{}, err
	}
	defer r.Close()
	f, err := files.OpenRegular(r, "manifest.json")
	if err != nil {
		return RestoreResult{}, err
	}
	var m Manifest
	err = decodeJSON(f, manifestLimit, &m)
	err = errors.Join(err, f.Close())
	if err != nil {
		return RestoreResult{}, err
	}
	if err = verifyTree(ctx, r, m); err != nil {
		return RestoreResult{}, err
	}
	return swap(ctx, parent, target, stage)
}

func verifyTree(ctx context.Context, root *os.Root, m Manifest) error {
	p := importer.Preview{Entries: []importer.Entry{{Path: "manifest.json"}}}
	err := fs.WalkDir(root.FS(), "data", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := files.Check(root, name)
		if err != nil {
			return err
		}
		e := importer.Entry{Path: name, Directory: info.IsDir()}
		if !info.IsDir() {
			f, err := files.OpenRegular(root, name)
			if err != nil {
				return err
			}
			h := sha256.New()
			n, err := files.Copy(ctx, h, f, info.Size(), nil)
			err = errors.Join(err, f.Close())
			if err != nil {
				return err
			}
			e.Size, e.SHA256 = n, hex.EncodeToString(h.Sum(nil))
		}
		p.Entries = append(p.Entries, e)
		return nil
	})
	if err != nil {
		return err
	}
	return match(m, p)
}

func swap(ctx context.Context, parent *os.Root, target, stage string) (RestoreResult, error) {
	if err := files.ValidatePath(target); err != nil || strings.Contains(target, "/") || target == "." || strings.HasPrefix(strings.ToLower(target), ".nesc-") {
		return RestoreResult{}, files.ErrUnsafePath
	}
	if err := files.ValidatePath(stage); err != nil {
		return RestoreResult{}, err
	}
	targetInfo, err := directory(parent, target)
	if err != nil {
		return RestoreResult{}, err
	}
	stageInfo, err := directory(parent, stage)
	if err != nil {
		return RestoreResult{}, err
	}
	parentInfo, err := directory(parent, ".")
	if err != nil {
		return RestoreResult{}, err
	}
	dev := parentInfo.Sys().(*syscall.Stat_t).Dev
	if targetInfo.Sys().(*syscall.Stat_t).Dev != dev || stageInfo.Sys().(*syscall.Stat_t).Dev != dev || os.SameFile(targetInfo, stageInfo) {
		return RestoreResult{}, errors.New("restore requires distinct directories on the same filesystem as their parent")
	}
	if err = ctx.Err(); err != nil {
		return RestoreResult{}, err
	}
	id := model.NewID()
	result := RestoreResult{Recovery: ".nesc-recovery-" + id, Journal: ".nesc-transaction-" + id + ".json"}
	if err = writeJSON(parent, result.Journal, restoreJournal{Target: target, Stage: stage, Recovery: result.Recovery}); err != nil {
		return result, err
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	if err = files.RenameNoReplace(parent, target, result.Recovery); err != nil {
		return result, err
	}
	if err = syncRoot(parent); err != nil {
		return result, fmt.Errorf("original retained at %s; parent sync failed: %w", result.Recovery, err)
	}
	// Once the original is retained, finish the short transaction despite cancellation.
	if err = files.RenameNoReplace(parent, stage, target); err != nil {
		recoveryErr := files.RenameNoReplace(parent, result.Recovery, target)
		return result, fmt.Errorf("restore install failed; original recovery path %s: %w", result.Recovery, errors.Join(err, recoveryErr, syncRoot(parent)))
	}
	result.Installed = true
	err = syncRoot(parent)
	if err == nil && path.Dir(stage) != "." {
		dir, openErr := parent.OpenRoot(path.Dir(stage))
		if openErr != nil {
			err = openErr
		} else {
			err = errors.Join(syncRoot(dir), dir.Close())
		}
	}
	return result, err
}

// Recover is a separately administrator-confirmed swap. It preserves the
// displaced current tree as another recovery directory, never as an implicit rollback.
func Recover(ctx context.Context, parent *os.Root, target, recovery string) (RestoreResult, error) {
	if !strings.HasPrefix(recovery, ".nesc-recovery-") || !model.ValidID(strings.TrimPrefix(recovery, ".nesc-recovery-")) {
		return RestoreResult{}, files.ErrUnsafePath
	}
	if err := files.ValidatePath(target); err != nil || strings.Contains(target, "/") || target == "." || strings.HasPrefix(strings.ToLower(target), ".nesc-") {
		return RestoreResult{}, files.ErrUnsafePath
	}
	if _, err := parent.Lstat(target); errors.Is(err, fs.ErrNotExist) {
		info, err := directory(parent, recovery)
		if err != nil {
			return RestoreResult{}, err
		}
		parentInfo, err := directory(parent, ".")
		if err != nil {
			return RestoreResult{}, err
		}
		if info.Sys().(*syscall.Stat_t).Dev != parentInfo.Sys().(*syscall.Stat_t).Dev {
			return RestoreResult{}, errors.New("recovery requires the same filesystem")
		}
		if err = ctx.Err(); err != nil {
			return RestoreResult{}, err
		}
		result := RestoreResult{Journal: ".nesc-transaction-" + model.NewID() + ".json"}
		if err = writeJSON(parent, result.Journal, restoreJournal{Target: target, Stage: recovery}); err != nil {
			return result, err
		}
		if err = ctx.Err(); err != nil {
			return result, err
		}
		if err = files.RenameNoReplace(parent, recovery, target); err != nil {
			return result, err
		}
		result.Installed = true
		return result, syncRoot(parent)
	}
	return swap(ctx, parent, target, recovery)
}
