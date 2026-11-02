package modrinth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/gabrielpgava/newempire-server-control/internal/model"
)

type Transaction struct {
	ID    string `json:"id"`
	State string `json:"state"`
}
type receipt struct {
	Transaction
	PlanHash   string       `json:"plan_hash"`
	Before     []FileState  `json:"before"`
	After      []FileState  `json:"after"`
	HadMods    bool         `json:"had_mods"`
	BeforeLock []byte       `json:"before_lock"`
	AfterLock  []byte       `json:"after_lock"`
	Config     []ConfigFile `json:"config,omitempty"`
}

func recoveryName(id, suffix string) string { return ".newempire-content-" + id + "-" + suffix }
func receiptName(id string) string          { return recoveryName(id, "transaction.json") }
func saveReceipt(root *os.Root, r *receipt) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if len(data) > 16<<20 {
		return errors.New("content transaction metadata exceeds the size limit")
	}
	return atomicWrite(root, receiptName(r.ID), data)
}
func readReceipt(root *os.Root, id string) (*receipt, error) {
	if !model.ValidID(id) {
		return nil, errors.New("invalid content transaction ID")
	}
	data, err := readOptional(root, receiptName(id), 16<<20)
	if err != nil {
		return nil, err
	}
	var r receipt
	if data == nil {
		return nil, fs.ErrNotExist
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	if r.ID != id || (r.State != "prepared" && r.State != "applied" && r.State != "rolled_back") {
		return nil, errors.New("invalid content transaction receipt")
	}
	if !ValidHash(r.PlanHash, 128) {
		return nil, errors.New("invalid content transaction fingerprint")
	}
	for _, f := range r.Config {
		if !SafePath(f.Path) || !strings.HasPrefix(f.Path, "config/") || !ValidHash(f.SHA512, 128) || f.Size < 0 || f.Size > MaxConfigSize {
			return nil, errors.New("unsafe config path in content transaction")
		}
	}
	return &r, nil
}

func LoadTransaction(root *os.Root, id string) (*Transaction, error) {
	r, err := readReceipt(root, id)
	if err != nil {
		return nil, err
	}
	return &r.Transaction, nil
}

// Apply requires a stopped server, an exclusive instance job lock, and an external verified backup.
// Cancellation is honored until the durable prepared journal; rename/rollback then finish uncancelably.
func Apply(ctx context.Context, root, stage *os.Root, plan *Plan) (*Transaction, error) {
	if err := plan.Validate(); err != nil {
		return nil, err
	}
	if err := verifyStage(ctx, stage, plan); err != nil {
		return nil, err
	}
	if old, err := readReceipt(root, plan.ID); err == nil {
		if old.State == "applied" && old.PlanHash == planHash(plan) {
			if err := verifyCurrent(ctx, root, old.After, old.AfterLock, true); err != nil {
				return nil, err
			}
			return &old.Transaction, nil
		}
		return nil, errors.New("transaction already exists; recover/finalize it before retrying")
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	before, hadMods, err := snapshot(ctx, root, "mods")
	if err != nil {
		return nil, err
	}
	if hadMods != plan.HadMods || !sameStates(before, plan.Before) {
		return nil, errors.New("mod files changed since the preview; create a new plan")
	}
	lockData, err := readOptional(root, LockFilename, 4<<20)
	if err != nil {
		return nil, err
	}
	lockHash := ""
	if lockData != nil {
		lockHash = hashBytes(lockData)
	}
	if lockHash != plan.LockHash {
		return nil, errors.New("content policy/lock changed since the preview")
	}
	lock, err := LoadLock(root)
	if err != nil {
		return nil, err
	}
	for _, f := range plan.Config {
		if _, err := root.Lstat(f.Path); err == nil {
			return nil, fmt.Errorf("local config appeared since preview; replan to preserve it: %s", f.Path)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		if err := checkParents(root, path.Dir(f.Path)); err != nil {
			return nil, err
		}
	}
	next := recoveryName(plan.ID, "next")
	original := recoveryName(plan.ID, "original")
	discard := recoveryName(plan.ID, "discard")
	for _, name := range []string{next, original, discard} {
		if _, err := root.Lstat(name); err == nil {
			return nil, fmt.Errorf("content recovery path already exists: %s", name)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	if err := root.Mkdir(next, 0750); err != nil {
		return nil, err
	}
	journaled := false
	defer func() {
		if !journaled {
			root.RemoveAll(next)
		}
	}()
	if hadMods {
		if err := copyTree(ctx, root, "mods", next, before); err != nil {
			return nil, err
		}
	}
	if err := reconcileCandidate(ctx, root, stage, next, lock, plan); err != nil {
		return nil, err
	}
	after, _, err := snapshot(ctx, root, next)
	if err != nil {
		return nil, err
	}
	newLock := Lock{Schema: 1, Target: plan.Target, Policy: plan.Policy, Files: plan.Files, Pack: plan.Pack}
	afterLock, err := json.MarshalIndent(newLock, "", "  ")
	if err != nil {
		return nil, err
	}
	r := &receipt{Transaction: Transaction{ID: plan.ID, State: "prepared"}, PlanHash: planHash(plan), Before: before, After: after, HadMods: hadMods, BeforeLock: lockData, AfterLock: afterLock, Config: plan.Config}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := saveReceipt(root, r); err != nil {
		return nil, err
	}
	journaled = true
	commit := func() error {
		if hadMods {
			if err := root.Rename("mods", original); err != nil {
				return err
			}
			if err := syncDir(root, "."); err != nil {
				return err
			}
		}
		if err := root.Rename(next, "mods"); err != nil {
			return err
		}
		if err := syncDir(root, "."); err != nil {
			return err
		}
		if err := atomicWrite(root, LockFilename, afterLock); err != nil {
			return err
		}
		var configRoot *os.Root
		if len(plan.Config) > 0 {
			if err := ensureDirs(root, "config"); err != nil {
				return err
			}
			var err error
			configRoot, err = openRealDir(root, "config")
			if err != nil {
				return err
			}
			defer configRoot.Close()
		}
		for _, f := range plan.Config {
			src, _, err := openRegular(stage, "overrides/"+f.Path)
			if err != nil {
				return err
			}
			err = writeVerified(context.Background(), configRoot, strings.TrimPrefix(f.Path, "config/"), src, f.Size, f.SHA512, "", 0640)
			closeErr := src.Close()
			if err := errors.Join(err, closeErr); err != nil {
				return err
			}
		}
		r.State = "applied"
		return saveReceipt(root, r)
	}
	if err := commit(); err != nil {
		rollbackErr := Rollback(context.Background(), root, &r.Transaction)
		return &r.Transaction, errors.Join(fmt.Errorf("content commit failed: %w", err), rollbackErr)
	}
	return &r.Transaction, nil
}

func checkParents(root *os.Root, dir string) error {
	if dir == "." {
		return nil
	}
	parts := strings.Split(dir, "/")
	for i := range parts {
		info, err := root.Lstat(strings.Join(parts[:i+1], "/"))
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("config parent must be a real directory")
		}
	}
	return nil
}

func copyTree(ctx context.Context, root *os.Root, from, to string, states []FileState) error {
	src, err := openRealDir(root, from)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := openRealDir(root, to)
	if err != nil {
		return err
	}
	defer dst.Close()
	count := 0
	if err := fs.WalkDir(src.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		count++
		if count > MaxFiles {
			return errors.New("content changed beyond the file count limit while copying")
		}
		if !SafePath(name) || entry.Type()&os.ModeSymlink != 0 {
			return errors.New("unsafe content path while copying")
		}
		if entry.IsDir() {
			return ensureDirs(dst, name)
		}
		return nil
	}); err != nil {
		return err
	}
	for _, state := range states {
		f, _, err := openRegular(src, state.Path)
		if err != nil {
			return err
		}
		err = writeVerified(ctx, dst, state.Path, f, state.Size, state.SHA512, "", fs.FileMode(state.Mode))
		closeErr := f.Close()
		if err := errors.Join(err, closeErr); err != nil {
			return err
		}
	}
	return syncDir(dst, ".")
}

func reconcileCandidate(ctx context.Context, root, stage *os.Root, next string, old Lock, plan *Plan) error {
	candidate, err := openRealDir(root, next)
	if err != nil {
		return err
	}
	defer candidate.Close()
	oldNames := map[string]LockedFile{}
	for _, f := range old.Files {
		if err := verifyFile(ctx, candidate, f.Filename, f.SHA512, f.Size); err != nil {
			return errors.New("managed content changed or is missing; refusing to overwrite it")
		}
		oldNames[f.Filename] = f
		keep := false
		for _, desired := range plan.Files {
			if desired.ProjectID == f.ProjectID && desired.Filename == f.Filename && desired.SHA512 == f.SHA512 {
				keep = true
			}
		}
		if !keep {
			if err := candidate.Remove(f.Filename); err != nil {
				return err
			}
		}
	}
	for _, f := range plan.Files {
		if _, err := candidate.Lstat(f.Filename); err == nil {
			old, managed := oldNames[f.Filename]
			if !managed || old.ProjectID != f.ProjectID || old.SHA512 != f.SHA512 {
				return fmt.Errorf("refusing to adopt/overwrite unmanaged content: %s", f.Filename)
			}
			if err := verifyFile(ctx, candidate, f.Filename, f.SHA512, f.Size); err != nil {
				return err
			}
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		src, _, err := openRegular(stage, blobPath(f))
		if err != nil {
			return err
		}
		err = writeVerified(ctx, candidate, f.Filename, src, f.Size, f.SHA512, f.SHA1, 0644)
		closeErr := src.Close()
		if err := errors.Join(err, closeErr); err != nil {
			return err
		}
	}
	return nil
}

func verifyCurrent(ctx context.Context, root *os.Root, want []FileState, lock []byte, hadMods bool) error {
	states, exists, err := snapshot(ctx, root, "mods")
	if err != nil {
		return err
	}
	if exists != hadMods || !sameStates(states, want) {
		return errors.New("content changed after transaction; manual recovery is required")
	}
	current, err := readOptional(root, LockFilename, 4<<20)
	if err != nil {
		return err
	}
	if !slices.Equal(current, lock) {
		return errors.New("content lock changed after transaction; manual recovery is required")
	}
	return nil
}

// Rollback restores original mods and the exact previous content lock, never worlds or runtime files.
func Rollback(ctx context.Context, root *os.Root, tx *Transaction) error {
	if tx == nil {
		return errors.New("content transaction is required")
	}
	r, err := readReceipt(root, tx.ID)
	if err != nil {
		return err
	}
	if r.State == "rolled_back" {
		tx.State = r.State
		return verifyCurrent(ctx, root, r.Before, r.BeforeLock, r.HadMods)
	}
	current, active, err := snapshot(ctx, root, "mods")
	if err != nil {
		return err
	}
	original := recoveryName(r.ID, "original")
	discard := recoveryName(r.ID, "discard")
	old, hasOriginal, err := snapshot(ctx, root, original)
	if err != nil {
		return err
	}
	if hasOriginal && !sameStates(old, r.Before) {
		return errors.New("original content recovery copy changed; refusing rollback")
	}
	if active && !sameStates(current, r.After) && !sameStates(current, r.Before) {
		return errors.New("mod files changed after commit; refusing destructive rollback")
	}
	if r.HadMods && !hasOriginal && (!active || !sameStates(current, r.Before)) {
		return errors.New("original content is unavailable; restore the verified external backup explicitly")
	}
	lock, err := readOptional(root, LockFilename, 4<<20)
	if err != nil {
		return err
	}
	if !slices.Equal(lock, r.BeforeLock) && !slices.Equal(lock, r.AfterLock) {
		return errors.New("content policy/lock changed after commit; refusing rollback")
	}
	var configRoot *os.Root
	if len(r.Config) > 0 {
		configRoot, err = openRealDir(root, "config")
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if configRoot != nil {
			defer configRoot.Close()
		}
	}
	for _, f := range r.Config {
		if configRoot == nil {
			break
		}
		name := strings.TrimPrefix(f.Path, "config/")
		if err := checkParents(configRoot, path.Dir(name)); err != nil {
			return err
		}
		if _, err := configRoot.Lstat(name); errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		if err := verifyFile(ctx, configRoot, name, f.SHA512, f.Size); err != nil {
			return errors.New("imported config was changed locally; refusing to remove it during rollback")
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if (hasOriginal || !r.HadMods) && active {
		if _, err := root.Lstat(discard); err == nil {
			return errors.New("rollback discard path already exists; manual recovery required")
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := root.Rename("mods", discard); err != nil {
			return err
		}
		if err := syncDir(root, "."); err != nil {
			return err
		}
	}
	if hasOriginal {
		if err := root.Rename(original, "mods"); err != nil {
			if active {
				_ = root.Rename(discard, "mods")
			}
			return err
		}
		if err := syncDir(root, "."); err != nil {
			return err
		}
	}
	if r.BeforeLock == nil {
		if err := root.Remove(LockFilename); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := syncDir(root, "."); err != nil {
			return err
		}
	} else if err := atomicWrite(root, LockFilename, r.BeforeLock); err != nil {
		return err
	}
	for _, f := range r.Config {
		if configRoot == nil {
			break
		}
		name := strings.TrimPrefix(f.Path, "config/")
		if err := configRoot.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := syncDir(configRoot, path.Dir(name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	r.State = "rolled_back"
	if err := saveReceipt(root, r); err != nil {
		return err
	}
	tx.State = r.State
	return nil
}

// Finalize explicitly discards recovery artifacts only after the caller confirms health or recovery.
func Finalize(root *os.Root, tx *Transaction) error {
	if tx == nil {
		return errors.New("content transaction is required")
	}
	r, err := readReceipt(root, tx.ID)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if r.State == "prepared" {
		return errors.New("unfinished content transaction must be recovered first")
	}
	want, lock, hadMods := r.After, r.AfterLock, true
	if r.State == "rolled_back" {
		want, lock, hadMods = r.Before, r.BeforeLock, r.HadMods
	}
	if err := verifyCurrent(context.Background(), root, want, lock, hadMods); err != nil {
		return err
	}
	for _, suffix := range []string{"original", "next", "discard"} {
		name := recoveryName(r.ID, suffix)
		states, exists, err := snapshot(context.Background(), root, name)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		expected := r.After
		if suffix == "original" {
			expected = r.Before
		}
		if !sameStates(states, expected) {
			return errors.New("recovery artifacts changed; refusing to delete them")
		}
		if err := root.RemoveAll(name); err != nil {
			return err
		}
	}
	if err := root.Remove(receiptName(r.ID)); err != nil {
		return err
	}
	return syncDir(root, ".")
}
