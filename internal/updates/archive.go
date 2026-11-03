package updates

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
)

func (m *Manager) download(ctx context.Context, r *os.Root, name, version string, asset Asset) error {
	resp, err := m.request(ctx, downloadURL(version, asset.Name))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.ContentLength >= 0 && resp.ContentLength != asset.Size {
		return problem(ErrVerification, "archive response size does not match the signed manifest")
	}
	f, err := regular(r, name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, h), contextReader{ctx, io.LimitReader(resp.Body, asset.Size+1)})
	if copyErr == nil {
		copyErr = f.Sync()
	}
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if n != asset.Size || hex.EncodeToString(h.Sum(nil)) != asset.SHA256 {
		return problem(ErrVerification, "archive size or SHA-256 does not match the signed manifest")
	}
	return nil
}

type zeroPadding struct{}

func (zeroPadding) Write(p []byte) (int, error) {
	for _, b := range p {
		if b != 0 {
			return 0, problem(ErrVerification, "archive contains data after its tar terminator")
		}
	}
	return len(p), nil
}

func (m *Manager) extract(ctx context.Context, r *os.Root, archive, dest string, expected Asset) (_ string, retErr error) {
	defer func() {
		if ctx.Err() != nil {
			retErr = ctx.Err()
		}
	}()
	if err := r.Mkdir(dest, 0755); err != nil {
		return "", err
	}
	f, err := regular(r, archive, os.O_RDONLY, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	compressed := sha256.New()
	rawLimit := &io.LimitedReader{R: contextReader{ctx, f}, N: expected.Size + 1}
	br := bufio.NewReader(io.TeeReader(rawLimit, compressed))
	gz, err := gzip.NewReader(br)
	if err != nil {
		return "", problem(ErrVerification, "archive is not valid gzip")
	}
	defer gz.Close()
	gz.Multistream(false)
	limit := &io.LimitedReader{R: gz, N: maxBinaryBytes + (1 << 20)}
	tr := tar.NewReader(limit)
	header, err := tr.Next()
	if err != nil || header.Name != BinaryName || header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA || header.Size <= 0 || header.Size > maxBinaryBytes || header.Mode&07000 != 0 || len(header.PAXRecords) != 0 || len(header.Xattrs) != 0 {
		return "", problem(ErrVerification, "archive must contain one ordinary panel executable without paths, links, or special permissions")
	}
	out, err := regular(r, dest+"/"+BinaryName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0500)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(out, h), contextReader{ctx, tr})
	if copyErr == nil {
		copyErr = out.Chmod(0555)
	}
	if copyErr == nil {
		copyErr = out.Sync()
	}
	closeErr := out.Close()
	if copyErr != nil {
		return "", archiveError(ctx, copyErr)
	}
	if closeErr != nil {
		return "", closeErr
	}
	if _, err := tr.Next(); !errors.Is(err, io.EOF) {
		return "", problem(ErrVerification, "archive contains additional entries or is truncated")
	}
	if _, err := io.Copy(zeroPadding{}, contextReader{ctx, limit}); err != nil {
		return "", fmt.Errorf("validate archive trailer: %w", archiveError(ctx, err))
	}
	if limit.N == 0 {
		return "", problem(ErrVerification, "expanded archive exceeds its size limit")
	}
	if _, err := br.Peek(1); !errors.Is(err, io.EOF) {
		return "", problem(ErrVerification, "archive has trailing data or concatenated gzip members")
	}
	if expected.Size+1-rawLimit.N != expected.Size || hex.EncodeToString(compressed.Sum(nil)) != expected.SHA256 {
		return "", problem(ErrVerification, "extracted archive bytes do not match the signed hash/size")
	}
	if err := validateELF(r, dest+"/"+BinaryName, m.arch); err != nil {
		return "", err
	}
	if err := syncDir(r, dest); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func archiveError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, gzip.ErrChecksum) || errors.Is(err, gzip.ErrHeader) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, tar.ErrHeader) {
		return problem(ErrVerification, "archive is corrupt or truncated")
	}
	return err
}

func validateELF(r *os.Root, name, arch string) error {
	f, err := regular(r, name, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	e, err := elf.NewFile(f)
	if err != nil {
		return problem(ErrVerification, "panel executable is not valid ELF")
	}
	want := elf.EM_X86_64
	if arch == "arm64" {
		want = elf.EM_AARCH64
	}
	if e.Class != elf.ELFCLASS64 || e.Data != elf.ELFDATA2LSB || e.Machine != want || e.Type != elf.ET_EXEC && e.Type != elf.ET_DYN || e.OSABI != elf.ELFOSABI_NONE && e.OSABI != elf.ELFOSABI_LINUX {
		return problem(ErrVerification, "panel executable does not match linux/%s", arch)
	}
	return nil
}
