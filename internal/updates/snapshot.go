package updates

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

type snapshotFile struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

func snapshotFiles(ctx context.Context, r *os.Root, seal bool) ([]snapshotFile, error) {
	files := []snapshotFile{}
	dirs := []string{}
	err := fs.WalkDir(r.FS(), ".", func(name string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if name == "snapshot.json" {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return problem(ErrRecovery, "snapshot contains a link or special file")
		}
		mode := os.FileMode(0600)
		if info.IsDir() {
			mode = 0700
			dirs = append(dirs, name)
		}
		if seal {
			f, err := r.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
			if err != nil {
				return err
			}
			opened, err := f.Stat()
			if err != nil || opened.IsDir() != info.IsDir() || !opened.IsDir() && !opened.Mode().IsRegular() {
				f.Close()
				return problem(ErrRecovery, "snapshot file type changed while sealing")
			}
			err = f.Chmod(mode)
			if err == nil {
				err = f.Sync()
			}
			closeErr := f.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
		} else if info.Mode().Perm() != mode {
			return problem(ErrRecovery, "snapshot permissions have changed")
		}
		if !info.IsDir() {
			hash, size, err := fileHash(ctx, r, name)
			if err != nil {
				return err
			}
			files = append(files, snapshotFile{name, size, hash})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for i := len(dirs) - 1; seal && i >= 0; i-- {
		if err := syncDir(r, dirs[i]); err != nil {
			return nil, err
		}
	}
	return files, nil
}

func (m *Manager) snapshot(ctx context.Context, r *os.Root, t *Transaction) (string, error) {
	dir := snapshotDir(t)
	if err := r.Mkdir(dir, 0700); err != nil {
		return "", err
	}
	sr, err := r.OpenRoot(dir)
	if err != nil {
		return "", err
	}
	defer sr.Close()
	if err := sr.Mkdir("data", 0700); err != nil {
		return "", err
	}
	old, err := regular(r, versionDir(t.PreviousVersion)+"/"+BinaryName, os.O_RDONLY, 0)
	if err != nil {
		return "", err
	}
	defer old.Close()
	backup, err := regular(sr, "binary", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	_, copyErr := io.Copy(backup, contextReader{ctx, io.LimitReader(old, maxBinaryBytes+1)})
	if copyErr == nil {
		copyErr = backup.Sync()
	}
	closeErr := backup.Close()
	if copyErr != nil {
		return "", copyErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	if err := m.opts.Snapshot(ctx, filepath.Join(r.Name(), filepath.FromSlash(dir), "data")); err != nil {
		return "", fmt.Errorf("snapshot panel database/configuration: %w", err)
	}
	files, err := snapshotFiles(ctx, sr, true)
	if err != nil {
		return "", err
	}
	dataFiles := 0
	for _, f := range files {
		if f.Name == "binary" && (f.SHA256 != t.PreviousSHA256 || f.Size > maxBinaryBytes || f.Size == 0) {
			return "", problem(ErrRecovery, "installed binary changed while taking its snapshot")
		}
		if strings.HasPrefix(f.Name, "data/") {
			dataFiles++
		}
	}
	if dataFiles == 0 {
		return "", problem(ErrRecovery, "snapshot adapter produced no database/configuration files")
	}
	b, err := json.Marshal(files)
	if err != nil {
		return "", err
	}
	if err := atomicWrite(sr, "snapshot.json", b, 0600); err != nil {
		return "", err
	}
	if err := syncDir(r, "updates/snapshots"); err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

func verifySnapshot(ctx context.Context, r *os.Root, t *Transaction) error {
	if !validHash(t.SnapshotSHA256) || !validHash(t.PreviousSHA256) {
		return problem(ErrRecovery, "transaction has no verified recovery snapshot")
	}
	sr, err := r.OpenRoot(snapshotDir(t))
	if err != nil {
		return err
	}
	defer sr.Close()
	b, err := limitedRead(sr, "snapshot.json", 8<<20)
	if err != nil {
		return err
	}
	h := sha256.Sum256(b)
	if hex.EncodeToString(h[:]) != t.SnapshotSHA256 {
		return problem(ErrRecovery, "snapshot integrity manifest does not match the transaction")
	}
	files, err := snapshotFiles(ctx, sr, false)
	if err != nil {
		return err
	}
	actual, _ := json.Marshal(files)
	if !bytes.Equal(b, actual) {
		return problem(ErrRecovery, "binary/database/configuration snapshot failed integrity verification")
	}
	hash, size, err := fileHash(ctx, r, versionDir(t.PreviousVersion)+"/"+BinaryName)
	if err != nil || size == 0 || size > maxBinaryBytes || hash != t.PreviousSHA256 {
		return problem(ErrRecovery, "preserved previous binary is missing or changed; restore it manually from the verified binary snapshot")
	}
	return nil
}
