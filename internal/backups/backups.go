// Package backups stores verified archives outside managed instance trees.
package backups

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/gabrielpgava/newempire-server-control/internal/files"
	"github.com/gabrielpgava/newempire-server-control/internal/importer"
	"github.com/gabrielpgava/newempire-server-control/internal/model"
	"github.com/klauspost/compress/zstd"
	"golang.org/x/sys/unix"
)

type Format string

const (
	ZIP     Format = "zip"
	TARZSTD Format = "tar.zst"
)

const manifestLimit = 32 << 20

type Manifest struct {
	Version   int              `json:"version"`
	ID        string           `json:"id"`
	CreatedAt time.Time        `json:"created_at"`
	Bytes     int64            `json:"bytes"`
	Entries   []importer.Entry `json:"entries"`
}

type Backup struct {
	ID             string    `json:"id"`
	Format         Format    `json:"format"`
	CreatedAt      time.Time `json:"created_at"`
	Bytes          int64     `json:"bytes"`
	ArchiveSHA256  string    `json:"archive_sha256"`
	ManifestSHA256 string    `json:"manifest_sha256"`
}

func extension(format Format) (string, error) {
	switch format {
	case ZIP:
		return ".zip", nil
	case TARZSTD:
		return ".tar.zst", nil
	default:
		return "", errors.New("unsupported backup format")
	}
}

// Create requires a stopped instance, or an externally coordinated consistent
// snapshot. It never claims that a running directory scan is an online backup.
func Create(ctx context.Context, source, storage *os.Root, format Format, progress files.Progress) (Backup, error) {
	ext, err := extension(format)
	if err != nil {
		return Backup{}, err
	}
	if err = files.DisjointRoots(source, storage); err != nil {
		return Backup{}, err
	}
	b := Backup{ID: model.NewID(), Format: format, CreatedAt: time.Now().UTC()}
	tmp := ".nesc-backup-" + b.ID
	f, err := storage.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return Backup{}, err
	}
	defer storage.Remove(tmp)
	hash := sha256.New()
	out := io.MultiWriter(f, hash)
	var completed int64
	report := func(n int64) {
		if progress != nil {
			progress(completed + n)
		}
	}
	var add func(string, int64, bool, io.Reader) error
	var closeArchive func() error
	if format == ZIP {
		zw := zip.NewWriter(out)
		add = func(name string, size int64, dir bool, src io.Reader) error {
			h := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: b.CreatedAt}
			h.SetMode(0600)
			if dir {
				h.Name += "/"
				h.SetMode(os.ModeDir | 0700)
			}
			w, err := zw.CreateHeader(h)
			if err != nil || dir {
				return err
			}
			n, err := files.Copy(ctx, w, src, size, report)
			completed += n
			if err == nil && n != size {
				err = errors.New("source file changed during backup")
			}
			return err
		}
		closeArchive = zw.Close
	} else {
		zw, err := zstd.NewWriter(out, zstd.WithEncoderConcurrency(1), zstd.WithWindowSize(1<<20))
		if err != nil {
			f.Close()
			return Backup{}, err
		}
		tw := tar.NewWriter(zw)
		add = func(name string, size int64, dir bool, src io.Reader) error {
			h := &tar.Header{Name: name, Size: size, Mode: 0600, Typeflag: tar.TypeReg, ModTime: b.CreatedAt}
			if dir {
				h.Name += "/"
				h.Typeflag, h.Mode = tar.TypeDir, 0700
			}
			if err := tw.WriteHeader(h); err != nil || dir {
				return err
			}
			n, err := files.Copy(ctx, tw, src, size, report)
			completed += n
			if err == nil && n != size {
				err = errors.New("source file changed during backup")
			}
			return err
		}
		closeArchive = func() error { return errors.Join(tw.Close(), zw.Close()) }
	}
	m := Manifest{Version: 1, ID: b.ID, CreatedAt: b.CreatedAt, Entries: []importer.Entry{}}
	err = add("data", 0, true, nil)
	if err == nil {
		err = files.Walk(ctx, source, ".", func(name string, info fs.FileInfo) error {
			if name == "." {
				return nil
			}
			if len(m.Entries) >= 99998 || info.Size() > 32<<30 || (!info.IsDir() && info.Size() > (128<<30)-m.Bytes) {
				return files.ErrTooLarge
			}
			e := importer.Entry{Path: name, Directory: info.IsDir()}
			if info.IsDir() {
				if err := add("data/"+name, 0, true, nil); err != nil {
					return err
				}
			} else {
				in, err := files.OpenRegular(source, name)
				if err != nil {
					return err
				}
				h := sha256.New()
				err = add("data/"+name, info.Size(), false, io.TeeReader(in, h))
				after, statErr := in.Stat()
				err = errors.Join(err, statErr, in.Close())
				if err != nil {
					return err
				}
				if after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
					return errors.New("source file changed during backup")
				}
				e.Size, e.SHA256 = info.Size(), hex.EncodeToString(h.Sum(nil))
				m.Bytes += info.Size()
			}
			m.Entries = append(m.Entries, e)
			return nil
		})
	}
	var raw []byte
	if err == nil {
		raw, err = json.Marshal(m)
	}
	if err == nil && len(raw) > manifestLimit {
		err = files.ErrTooLarge
	}
	if err == nil {
		err = add("manifest.json", int64(len(raw)), false, bytes.NewReader(raw))
	}
	err = errors.Join(err, closeArchive())
	if err == nil {
		err = f.Sync()
	}
	info, statErr := f.Stat()
	err = errors.Join(err, statErr, f.Close())
	if err != nil {
		return Backup{}, err
	}
	if info.Size() > 32<<30 {
		return Backup{}, files.ErrTooLarge
	}
	if err = ctx.Err(); err != nil {
		return Backup{}, err
	}
	b.Bytes, b.ArchiveSHA256 = info.Size(), hex.EncodeToString(hash.Sum(nil))
	mhash := sha256.Sum256(raw)
	b.ManifestSHA256 = hex.EncodeToString(mhash[:])
	if err = files.RenameNoReplace(storage, tmp, b.ID+ext); err != nil {
		return Backup{}, err
	}
	archive, err := files.OpenRegular(storage, b.ID+ext)
	if err != nil {
		return Backup{}, err
	}
	_, verifyErr := verifyArchive(ctx, archive, b, nil)
	err = errors.Join(verifyErr, archive.Close())
	if err != nil {
		return Backup{}, fmt.Errorf("backup verification failed; archive retained as %s%s: %w", b.ID, ext, err)
	}
	if err = writeJSON(storage, b.ID+".json", b); err != nil {
		return Backup{}, fmt.Errorf("archive retained as %s%s without a complete catalog entry: %w", b.ID, ext, err)
	}
	return b, nil
}

func writeJSON(root *os.Root, name string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	tmp := ".nesc-json-" + model.NewID()
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(tmp)
	_, err = f.Write(raw)
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = files.RenameNoReplace(root, tmp, name); err != nil {
		return err
	}
	return syncRoot(root)
}

func syncRoot(root *os.Root) error {
	f, err := root.OpenFile(".", os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func decodeJSON(r io.Reader, maxBytes int64, out any) error {
	raw, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return err
	}
	if int64(len(raw)) > maxBytes {
		return files.ErrTooLarge
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err = d.Decode(out); err != nil {
		return err
	}
	if err = d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("unexpected data after JSON object")
	}
	return nil
}

func metadata(root *os.Root, id string) (Backup, error) {
	if !model.ValidID(id) {
		return Backup{}, files.ErrUnsafePath
	}
	f, err := files.OpenRegular(root, id+".json")
	if err != nil {
		return Backup{}, err
	}
	defer f.Close()
	var b Backup
	if err = decodeJSON(f, 4096, &b); err != nil {
		return Backup{}, err
	}
	_, err = extension(b.Format)
	if err != nil || b.ID != id || b.Bytes <= 0 || !digest(b.ArchiveSHA256) || !digest(b.ManifestSHA256) || b.CreatedAt.IsZero() {
		return Backup{}, errors.New("invalid backup catalog entry")
	}
	return b, nil
}

func digest(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == sha256.Size && s == strings.ToLower(s)
}

func List(ctx context.Context, root *os.Root) ([]Backup, error) {
	dir, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	result := []Backup{}
	for {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		names, readErr := dir.Readdirnames(256)
		for _, name := range names {
			id := strings.TrimSuffix(name, ".json")
			if name == id || !model.ValidID(id) {
				continue
			}
			b, err := metadata(root, id)
			if err != nil {
				return nil, err
			}
			result = append(result, b)
			if len(result) > 100000 {
				return nil, files.ErrTooLarge
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.After(result[j].CreatedAt) })
	return result, nil
}

// Open returns a seekable file suitable for http.ServeContent and range requests.
func Open(root *os.Root, id string) (*os.File, Backup, error) {
	b, err := metadata(root, id)
	if err != nil {
		return nil, Backup{}, err
	}
	ext, _ := extension(b.Format)
	f, err := files.OpenRegular(root, id+ext)
	if err != nil {
		return nil, Backup{}, err
	}
	info, err := f.Stat()
	if err != nil || info.Size() != b.Bytes {
		f.Close()
		return nil, Backup{}, errors.New("backup archive size differs from catalog")
	}
	return f, b, nil
}

func Verify(ctx context.Context, root *os.Root, id string, progress files.Progress) (Manifest, error) {
	f, b, err := Open(root, id)
	if err != nil {
		return Manifest{}, err
	}
	defer f.Close()
	return verifyArchive(ctx, f, b, progress)
}

func verifyArchive(ctx context.Context, f *os.File, b Backup, progress files.Progress) (Manifest, error) {
	h := sha256.New()
	n, err := files.Copy(ctx, h, f, b.Bytes, nil)
	if err != nil {
		return Manifest{}, err
	}
	if n != b.Bytes || hex.EncodeToString(h.Sum(nil)) != b.ArchiveSHA256 {
		return Manifest{}, errors.New("backup archive checksum mismatch")
	}
	p, err := importer.Inspect(ctx, f, importer.Format(b.Format), importer.Limits{}, progress)
	if err != nil {
		return Manifest{}, err
	}
	raw, err := readManifest(ctx, f, b)
	if err != nil {
		return Manifest{}, err
	}
	mh := sha256.Sum256(raw)
	if hex.EncodeToString(mh[:]) != b.ManifestSHA256 {
		return Manifest{}, errors.New("backup manifest checksum mismatch")
	}
	var m Manifest
	if err = decodeJSON(bytes.NewReader(raw), manifestLimit, &m); err != nil {
		return Manifest{}, err
	}
	if m.ID != b.ID || !m.CreatedAt.Equal(b.CreatedAt) {
		return Manifest{}, errors.New("backup manifest identity mismatch")
	}
	if err = match(m, p); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

func match(m Manifest, p importer.Preview) error {
	if m.Version != 1 || !model.ValidID(m.ID) || m.Bytes < 0 || len(m.Entries) > 99998 {
		return errors.New("invalid backup manifest")
	}
	expected := make(map[string]importer.Entry, len(m.Entries))
	var total int64
	for _, e := range m.Entries {
		if err := files.ValidatePath(e.Path); err != nil || e.Path == "." {
			return errors.New("invalid manifest path")
		}
		if _, exists := expected[e.Path]; exists || e.Size < 0 || e.Size > (128<<30)-total || (!e.Directory && !digest(e.SHA256)) || (e.Directory && (e.Size != 0 || e.SHA256 != "")) {
			return errors.New("invalid manifest entry")
		}
		expected[e.Path] = e
		total += e.Size
	}
	if total != m.Bytes {
		return errors.New("manifest byte total mismatch")
	}
	root, manifest := false, false
	for _, actual := range p.Entries {
		switch actual.Path {
		case "data":
			if !actual.Directory || root {
				return errors.New("invalid backup data root")
			}
			root = true
		case "manifest.json":
			if actual.Directory || manifest || actual.Size > manifestLimit {
				return errors.New("invalid backup manifest file")
			}
			manifest = true
		default:
			if !strings.HasPrefix(actual.Path, "data/") {
				return errors.New("unexpected file outside backup data root")
			}
			actual.Path = strings.TrimPrefix(actual.Path, "data/")
			entry, exists := expected[actual.Path]
			if !exists || entry != actual {
				return fmt.Errorf("backup data does not match manifest: %q", actual.Path)
			}
			delete(expected, actual.Path)
		}
	}
	if !root || !manifest || len(expected) != 0 {
		return errors.New("backup is missing required manifest entries")
	}
	return nil
}

func readManifest(ctx context.Context, f *os.File, b Backup) ([]byte, error) {
	var src io.Reader
	var closer io.Closer
	if b.Format == ZIP {
		zr, err := zip.NewReader(f, b.Bytes)
		if err != nil {
			return nil, err
		}
		for _, e := range zr.File {
			if e.Name == "manifest.json" {
				r, err := e.Open()
				if err != nil {
					return nil, err
				}
				src, closer = r, r
				break
			}
		}
	} else {
		z, err := zstd.NewReader(io.NewSectionReader(f, 0, b.Bytes), zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(64<<20))
		if err != nil {
			return nil, err
		}
		defer z.Close()
		tr := tar.NewReader(z)
		for {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			h, err := tr.Next()
			if err != nil {
				return nil, err
			}
			if h.Name == "manifest.json" {
				src = tr
				break
			}
			if _, err = files.Copy(ctx, io.Discard, tr, h.Size, nil); err != nil {
				return nil, err
			}
		}
	}
	if src == nil {
		return nil, errors.New("backup manifest not found")
	}
	if closer != nil {
		defer closer.Close()
	}
	var raw bytes.Buffer
	_, err := files.Copy(ctx, &raw, src, manifestLimit, nil)
	return raw.Bytes(), err
}
