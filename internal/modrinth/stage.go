package modrinth

import (
	"context"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/gabrielpgava/newempire-server-control/internal/model"
)

const MaxConfigSize int64 = 16 << 20
const stageManifest = "content-stage.json"

type stagedManifest struct {
	Schema   int    `json:"schema"`
	PlanHash string `json:"plan_hash"`
}

func planHash(plan *Plan) string { data, _ := json.Marshal(plan); return hashBytes(data) }

func (plan *Plan) Validate() error {
	if plan == nil || !model.ValidID(plan.ID) {
		return errors.New("invalid content plan identity")
	}
	if len(plan.Blockers) > 0 {
		return errors.New("content plan has unresolved blockers")
	}
	sealed := *plan
	sealed.Seal()
	if sealed.ID != plan.ID {
		return errors.New("content plan identity does not match its payload")
	}
	if err := plan.Target.Validate(); err != nil {
		return err
	}
	if _, err := plan.Policy.normalized(); err != nil {
		return err
	}
	if err := validateLocked(plan.Files); err != nil {
		return err
	}
	if plan.LockHash != "" && !ValidHash(plan.LockHash, 128) {
		return errors.New("invalid content lock fingerprint")
	}
	if len(plan.Before) > MaxFiles || len(plan.Config) > MaxFiles || len(plan.Before)+len(plan.Config)+len(plan.Files) > MaxFiles {
		return errors.New("content plan has too many files")
	}
	paths := map[string]bool{}
	var total int64
	for _, f := range plan.Before {
		if !SafePath(f.Path) || !ValidHash(f.SHA512, 128) || f.Size < 0 || f.Size > MaxFileSize || f.Mode > 0777 || paths[f.Path] {
			return errors.New("invalid original content fingerprint")
		}
		paths[f.Path] = true
		total += f.Size
	}
	paths = map[string]bool{}
	for _, f := range plan.Config {
		if !SafePath(f.Path) || !strings.HasPrefix(f.Path, "config/") || !ValidHash(f.SHA512, 128) || f.Size < 0 || f.Size > MaxConfigSize || paths[f.Path] {
			return errors.New("invalid config-only override")
		}
		paths[f.Path] = true
		total += f.Size
	}
	for _, f := range plan.Files {
		total += f.Size
	}
	if total > MaxTotalSize {
		return errors.New("content plan exceeds the total size limit")
	}
	return nil
}

func needsDownload(plan *Plan, file LockedFile) bool {
	for _, before := range plan.Before {
		if before.Path == file.Filename && before.SHA512 == file.SHA512 && before.Size == file.Size {
			return false
		}
	}
	return true
}

func blobPath(file LockedFile) string { return "downloads/" + file.SHA512 + ".jar" }

// Stage downloads only exact official CDN artifacts and never touches instance/world files.
func (s *Service) Stage(ctx context.Context, stage *os.Root, plan *Plan) error {
	if err := plan.Validate(); err != nil {
		return err
	}
	if err := ensureDirs(stage, "downloads"); err != nil {
		return err
	}
	for _, f := range plan.Files {
		if !needsDownload(plan, f) {
			continue
		}
		name := blobPath(f)
		if _, err := stage.Lstat(name); err == nil {
			if err := verifyFile(ctx, stage, name, f.SHA512, f.Size); err != nil {
				return err
			}
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := s.download(ctx, stage, name, f); err != nil {
			return fmt.Errorf("stage %s: %w", f.Filename, err)
		}
	}
	for _, f := range plan.Config {
		if err := verifyFile(ctx, stage, "overrides/"+f.Path, f.SHA512, f.Size); err != nil {
			return err
		}
	}
	data, err := json.Marshal(stagedManifest{Schema: 1, PlanHash: planHash(plan)})
	if err != nil {
		return err
	}
	return atomicWrite(stage, stageManifest, data)
}

func (s *Service) download(ctx context.Context, stage *os.Root, name string, file LockedFile) error {
	if err := file.validate(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, file.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := s.Client.send(req, "cdn.modrinth.com")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("CDN returned HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength >= 0 && resp.ContentLength != file.Size {
		return errors.New("CDN size differs from locked metadata")
	}
	temp := "downloads/.partial-" + model.NewID()
	if err := writeVerified(ctx, stage, temp, resp.Body, file.Size, file.SHA512, file.SHA1, 0600); err != nil {
		return err
	}
	defer stage.Remove(temp)
	if err := stage.Rename(temp, name); err != nil {
		return err
	}
	return syncDir(stage, "downloads")
}

func verifyFile(ctx context.Context, root *os.Root, name, hash string, size int64) error {
	f, err := fingerprint(ctx, root, name)
	if err != nil {
		return err
	}
	if f.Size != size || f.SHA512 != hash {
		return fmt.Errorf("content hash/size mismatch: %s", name)
	}
	return nil
}

func writeVerified(ctx context.Context, root *os.Root, name string, src io.Reader, size int64, expected, expectedSHA1 string, mode fs.FileMode) (err error) {
	if !SafePath(name) || size < 0 || size > MaxFileSize {
		return errors.New("invalid staged file path or size")
	}
	if err := ensureDirs(root, path.Dir(name)); err != nil {
		return err
	}
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			root.Remove(name)
		}
	}()
	h512 := sha512.New()
	h1 := sha1.New()
	n, copyErr := io.Copy(io.MultiWriter(f, h512, h1), io.LimitReader(contextReader{ctx, src}, size+1))
	if copyErr == nil && (n != size || hex.EncodeToString(h512.Sum(nil)) != expected || expectedSHA1 != "" && hex.EncodeToString(h1.Sum(nil)) != expectedSHA1) {
		copyErr = errors.New("download/copy hash or size mismatch")
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(copyErr, syncErr, closeErr); err != nil {
		return err
	}
	return syncDir(root, path.Dir(name))
}

func verifyStage(ctx context.Context, stage *os.Root, plan *Plan) error {
	data, err := readOptional(stage, stageManifest, 4096)
	if err != nil {
		return err
	}
	var manifest stagedManifest
	if err := json.Unmarshal(data, &manifest); err != nil || manifest.Schema != 1 || manifest.PlanHash != planHash(plan) {
		return errors.New("staged content does not match the confirmed plan")
	}
	for _, f := range plan.Files {
		if needsDownload(plan, f) {
			if err := verifyFile(ctx, stage, blobPath(f), f.SHA512, f.Size); err != nil {
				return err
			}
		}
	}
	for _, f := range plan.Config {
		if err := verifyFile(ctx, stage, "overrides/"+f.Path, f.SHA512, f.Size); err != nil {
			return err
		}
	}
	return nil
}

func sameStates(a, b []FileState) bool { return slices.Equal(a, b) }
