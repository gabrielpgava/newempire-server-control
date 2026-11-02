// Package importer validates untrusted archives before applying staged data.
package importer

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"strings"

	"github.com/gabrielpgava/newempire-server-control/internal/files"
	"github.com/klauspost/compress/zstd"
	"golang.org/x/sys/unix"
)

type Format string

const (
	ZIP     Format = "zip"
	TARGZ   Format = "tar.gz"
	TARZSTD Format = "tar.zst"
)

type Limits struct {
	MaxArchiveBytes int64
	MaxBytes        int64
	MaxFileBytes    int64
	MaxEntries      int
	MaxRatio        int64
}

func (l Limits) normalized() (Limits, error) {
	if l.MaxArchiveBytes < 0 || l.MaxBytes < 0 || l.MaxFileBytes < 0 || l.MaxEntries < 0 || l.MaxRatio < 0 {
		return l, errors.New("archive limits must not be negative")
	}
	if l.MaxArchiveBytes == 0 {
		l.MaxArchiveBytes = 32 << 30
	}
	if l.MaxBytes == 0 {
		l.MaxBytes = 128 << 30
	}
	if l.MaxFileBytes == 0 {
		l.MaxFileBytes = 32 << 30
	}
	if l.MaxEntries == 0 {
		l.MaxEntries = 100000
	}
	if l.MaxRatio == 0 {
		l.MaxRatio = 1000
	}
	if l.MaxBytes > math.MaxInt64/2 || l.MaxEntries > 1000000 || l.MaxRatio > 1000000 {
		return l, errors.New("archive limits exceed supported bounds")
	}
	return l, nil
}

type Entry struct {
	Path      string `json:"path"`
	Directory bool   `json:"directory"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256,omitempty"`
}

type Preview struct {
	Hash    string  `json:"preview_hash"`
	Format  Format  `json:"format"`
	Bytes   int64   `json:"bytes"`
	Files   int     `json:"files"`
	Entries []Entry `json:"entries"`
}

type scanner struct {
	ctx         context.Context
	limits      Limits
	preview     Preview
	seen        map[string]bool
	explicit    map[string]bool
	spellings   map[string]string
	stage       *os.Root
	progress    files.Progress
	archiveSize int64
}

func Inspect(ctx context.Context, archive *os.File, format Format, limits Limits, progress files.Progress) (Preview, error) {
	return scan(ctx, archive, format, nil, limits, progress)
}

// Extract requires private, empty staging owned exclusively by this job.
// The caller discards staging on any error; an instance is never modified here.
func Extract(ctx context.Context, archive *os.File, format Format, stage *os.Root, limits Limits, progress files.Progress) (Preview, error) {
	f, err := stage.Open(".")
	if err != nil {
		return Preview{}, err
	}
	names, readErr := f.Readdirnames(1)
	f.Close()
	if len(names) != 0 || (readErr != nil && !errors.Is(readErr, io.EOF)) {
		return Preview{}, errors.New("staging directory must be empty")
	}
	return scan(ctx, archive, format, stage, limits, progress)
}

func scan(ctx context.Context, archive *os.File, format Format, stage *os.Root, limits Limits, progress files.Progress) (Preview, error) {
	l, err := limits.normalized()
	if err != nil {
		return Preview{}, err
	}
	if err = ctx.Err(); err != nil {
		return Preview{}, err
	}
	info, err := archive.Stat()
	if err != nil {
		return Preview{}, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > l.MaxArchiveBytes {
		return Preview{}, errors.New("archive is not a bounded regular file")
	}
	s := scanner{ctx: ctx, limits: l, preview: Preview{Format: format, Entries: []Entry{}}, seen: make(map[string]bool), explicit: make(map[string]bool), spellings: make(map[string]string), stage: stage, progress: progress, archiveSize: info.Size()}
	switch format {
	case ZIP:
		err = s.zip(archive, info.Size())
	case TARGZ, TARZSTD:
		err = s.tar(archive, info.Size(), format)
	default:
		err = errors.New("unsupported archive format")
	}
	if err != nil {
		return Preview{}, err
	}
	if stage != nil {
		if err = files.Walk(ctx, stage, ".", func(name string, info os.FileInfo) error {
			if !info.IsDir() {
				return nil
			}
			f, err := stage.OpenFile(name, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
			if err != nil {
				return err
			}
			return errors.Join(f.Sync(), f.Close())
		}); err != nil {
			return Preview{}, err
		}
	}
	raw, err := json.Marshal(s.preview)
	if err != nil {
		return Preview{}, err
	}
	hash := sha256.Sum256(raw)
	s.preview.Hash = hex.EncodeToString(hash[:])
	return s.preview, nil
}

func (s *scanner) register(name string, dir bool) error {
	if err := files.ValidatePath(name); err != nil || name == "." {
		return files.ErrUnsafePath
	}
	parts := strings.Split(name, "/")
	for _, part := range parts {
		if strings.HasPrefix(strings.ToLower(part), ".nesc-") {
			return files.ErrUnsafePath
		}
	}
	key := strings.ToLower(name)
	for i := 1; i <= len(parts); i++ {
		spelling := strings.Join(parts[:i], "/")
		fold := strings.ToLower(spelling)
		if old, exists := s.spellings[fold]; exists && old != spelling {
			return fmt.Errorf("archive case collision: %q", name)
		}
		s.spellings[fold] = spelling
	}
	if oldDir, exists := s.seen[key]; exists && (s.explicit[key] || !dir || !oldDir) {
		return fmt.Errorf("archive entry collision: %q", name)
	}
	for i := 1; i < len(parts); i++ {
		parent := strings.ToLower(strings.Join(parts[:i], "/"))
		if isDir, exists := s.seen[parent]; exists && !isDir {
			return fmt.Errorf("archive parent collision: %q", name)
		}
		s.seen[parent] = true
	}
	s.seen[key] = dir
	s.explicit[key] = true
	if len(s.explicit) > s.limits.MaxEntries || len(s.seen) > s.limits.MaxEntries {
		return files.ErrTooLarge
	}
	return nil
}

func (s *scanner) entry(name string, dir bool, size int64, src io.Reader) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	if err := s.register(name, dir); err != nil {
		return err
	}
	if size < 0 || size > s.limits.MaxFileBytes || size > s.limits.MaxBytes-s.preview.Bytes || (dir && size != 0) {
		return files.ErrTooLarge
	}
	if (s.preview.Bytes+size)/s.limits.MaxRatio > s.archiveSize {
		return errors.New("archive expansion ratio exceeds the limit")
	}
	e := Entry{Path: name, Directory: dir, Size: size}
	if dir {
		if s.stage != nil {
			if err := s.stage.MkdirAll(name, 0700); err != nil {
				return err
			}
		}
		s.preview.Entries = append(s.preview.Entries, e)
		return nil
	}
	hash := sha256.New()
	var dst io.Writer = hash
	var out *os.File
	if s.stage != nil {
		if err := s.stage.MkdirAll(path.Dir(name), 0700); err != nil {
			return err
		}
		var err error
		out, err = s.stage.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0600)
		if err != nil {
			return err
		}
		dst = io.MultiWriter(out, hash)
	}
	n, err := files.Copy(s.ctx, dst, src, size, func(n int64) {
		if s.progress != nil {
			s.progress(s.preview.Bytes + n)
		}
	})
	if out != nil {
		if err == nil {
			err = out.Sync()
		}
		err = errors.Join(err, out.Close())
	}
	if err != nil {
		return err
	}
	if n != size {
		return errors.New("archive entry size does not match its data")
	}
	e.SHA256 = hex.EncodeToString(hash.Sum(nil))
	s.preview.Bytes += n
	s.preview.Files++
	s.preview.Entries = append(s.preview.Entries, e)
	return nil
}

func (s *scanner) zip(src *os.File, size int64) error {
	if err := zipDirectoryBounds(s.ctx, src, size, s.limits.MaxEntries); err != nil {
		return err
	}
	zr, err := zip.NewReader(src, size)
	if err != nil {
		return err
	}
	for _, f := range zr.File {
		if err := s.ctx.Err(); err != nil {
			return err
		}
		mode := f.Mode()
		if (!mode.IsRegular() && !mode.IsDir()) || f.Flags&1 != 0 || f.UncompressedSize64 > math.MaxInt64 {
			return files.ErrUnsafeType
		}
		if f.CompressedSize64 == 0 && f.UncompressedSize64 != 0 || f.UncompressedSize64/uint64(s.limits.MaxRatio) > f.CompressedSize64 {
			return errors.New("archive entry expansion ratio exceeds the limit")
		}
		name := f.Name
		if mode.IsDir() {
			name = strings.TrimSuffix(name, "/")
		}
		r, err := f.Open()
		if err != nil {
			return err
		}
		err = s.entry(name, mode.IsDir(), int64(f.UncompressedSize64), r)
		err = errors.Join(err, r.Close())
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *scanner) tar(src *os.File, size int64, format Format) error {
	section := io.NewSectionReader(src, 0, size)
	var stream io.Reader
	var closeStream func() error
	if format == TARGZ {
		gz, err := gzip.NewReader(section)
		if err != nil {
			return err
		}
		stream, closeStream = gz, gz.Close
	} else {
		z, err := zstd.NewReader(section, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(64<<20))
		if err != nil {
			return err
		}
		stream, closeStream = z, func() error { z.Close(); return nil }
	}
	defer closeStream()
	// Include a bounded allowance for tar headers, extensions and block padding.
	bounded := &io.LimitedReader{R: stream, N: s.limits.MaxBytes + int64(s.limits.MaxEntries)*8192 + 1024}
	tr := tar.NewReader(bounded)
	for {
		if err := s.ctx.Err(); err != nil {
			return err
		}
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			// Read through the compression footer: truncated/checksum-invalid streams fail.
			_, err = files.Copy(s.ctx, zeroPadding{}, bounded, 1<<20, nil)
			if bounded.N == 0 {
				return files.ErrTooLarge
			}
			return err
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeDir && h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA {
			return files.ErrUnsafeType
		}
		for key := range h.PAXRecords {
			if strings.HasPrefix(key, "GNU.sparse") {
				return files.ErrUnsafeType
			}
		}
		name := h.Name
		if h.Typeflag == tar.TypeDir {
			name = strings.TrimSuffix(name, "/")
		}
		if err := s.entry(name, h.Typeflag == tar.TypeDir, h.Size, tr); err != nil {
			return err
		}
	}
}

type zeroPadding struct{}

func (zeroPadding) Write(p []byte) (int, error) {
	for _, b := range p {
		if b != 0 {
			return 0, errors.New("nonzero data after tar end marker")
		}
	}
	return len(p), nil
}

// Bound central-directory allocations before archive/zip constructs its index.
func zipDirectoryBounds(ctx context.Context, src io.ReaderAt, size int64, maxEntries int) error {
	if size < 22 {
		return zip.ErrFormat
	}
	tailSize := min(size, int64(65535+22))
	tail := make([]byte, tailSize)
	if _, err := src.ReadAt(tail, size-tailSize); err != nil {
		return err
	}
	idx := -1
	for i := len(tail) - 22; i >= 0; i-- {
		if binary.LittleEndian.Uint32(tail[i:]) == 0x06054b50 && i+22+int(binary.LittleEndian.Uint16(tail[i+20:])) == len(tail) {
			idx = i
			break
		}
	}
	if idx < 0 {
		return zip.ErrFormat
	}
	end := tail[idx:]
	if binary.LittleEndian.Uint32(end[4:]) != 0 {
		return errors.New("multi-volume ZIP archives are not supported")
	}
	count := uint64(binary.LittleEndian.Uint16(end[10:]))
	dirSize := uint64(binary.LittleEndian.Uint32(end[12:]))
	offset := uint64(binary.LittleEndian.Uint32(end[16:]))
	if count == 0xffff || dirSize == 0xffffffff || offset == 0xffffffff {
		endOffset := size - tailSize + int64(idx)
		var locator [20]byte
		if endOffset < 20 {
			return zip.ErrFormat
		}
		if _, err := src.ReadAt(locator[:], endOffset-20); err != nil {
			return err
		}
		if binary.LittleEndian.Uint32(locator[:]) != 0x07064b50 || binary.LittleEndian.Uint32(locator[4:]) != 0 || binary.LittleEndian.Uint32(locator[16:]) != 1 {
			return zip.ErrFormat
		}
		zoff := binary.LittleEndian.Uint64(locator[8:])
		if zoff > uint64(size-56) {
			return zip.ErrFormat
		}
		var z [56]byte
		if _, err := src.ReadAt(z[:], int64(zoff)); err != nil {
			return err
		}
		if binary.LittleEndian.Uint32(z[:]) != 0x06064b50 || binary.LittleEndian.Uint64(z[16:]) != 0 {
			return zip.ErrFormat
		}
		count, dirSize, offset = binary.LittleEndian.Uint64(z[32:]), binary.LittleEndian.Uint64(z[40:]), binary.LittleEndian.Uint64(z[48:])
	}
	if count > uint64(maxEntries) || dirSize > 64<<20 {
		return files.ErrTooLarge
	}
	if offset > uint64(size) || dirSize > uint64(size)-offset {
		return zip.ErrFormat
	}
	var found uint64
	for pos, end := offset, offset+dirSize; pos < end; {
		if err := ctx.Err(); err != nil {
			return err
		}
		var header [46]byte
		if end-pos < 46 {
			return zip.ErrFormat
		}
		if _, err := src.ReadAt(header[:], int64(pos)); err != nil {
			return err
		}
		if binary.LittleEndian.Uint32(header[:]) != 0x02014b50 {
			return zip.ErrFormat
		}
		recordSize := uint64(46) + uint64(binary.LittleEndian.Uint16(header[28:])) + uint64(binary.LittleEndian.Uint16(header[30:])) + uint64(binary.LittleEndian.Uint16(header[32:]))
		if recordSize > end-pos {
			return zip.ErrFormat
		}
		found++
		if found > uint64(maxEntries) {
			return files.ErrTooLarge
		}
		pos += recordSize
	}
	if found != count {
		return zip.ErrFormat
	}
	return nil
}
