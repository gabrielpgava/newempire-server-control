// Command release builds and verifies the exact format accepted by the panel updater.
package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"debug/elf"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gabrielpgava/newempire-server-control/internal/database"
	"github.com/gabrielpgava/newempire-server-control/internal/updates"
)

const limit = 512 << 20
const publicKeyFile = "deploy/release-public-key.txt"

func command(name string, args ...string) (string, error) {
	c := exec.Command(name, args...)
	c.Stderr = os.Stderr
	b, err := c.Output()
	return strings.TrimSpace(string(b)), err
}

func readRegular(path string, max int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > max {
		return nil, fmt.Errorf("expected a bounded regular file: %s", path)
	}
	return os.ReadFile(path)
}

func writeExclusive(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	return errors.Join(err, f.Close())
}

func decode(s string, size int) ([]byte, error) {
	b, err := base64.StdEncoding.Strict().DecodeString(s)
	if err != nil || len(b) != size || base64.StdEncoding.EncodeToString(b) != s {
		return nil, fmt.Errorf("expected canonical padded base64 of %d bytes", size)
	}
	return b, nil
}

func readPublicKey(path string) (ed25519.PublicKey, error) {
	b, err := readRegular(path, 1024)
	if err != nil {
		return nil, fmt.Errorf("pinned release public key is required: %w", err)
	}
	return decode(strings.TrimSpace(string(b)), ed25519.PublicKeySize)
}

func signingKey(value string, public ed25519.PublicKey) (ed25519.PrivateKey, error) {
	key, err := decode(value, ed25519.PrivateKeySize)
	if err != nil {
		return nil, errors.New("RELEASE_SIGNING_KEY must contain a canonical base64 64-byte Go Ed25519 private key")
	}
	canonical := ed25519.NewKeyFromSeed(key[:ed25519.SeedSize])
	if !bytes.Equal(canonical, key) || !bytes.Equal(canonical.Public().(ed25519.PublicKey), public) {
		return nil, errors.New("RELEASE_SIGNING_KEY does not match deploy/release-public-key.txt")
	}
	return canonical, nil
}

func archive(path string, epoch time.Time, contents func(*tar.Writer) error) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(f)
	gz.Header.ModTime = epoch
	tw := tar.NewWriter(gz)
	err = contents(tw)
	return errors.Join(err, tw.Close(), gz.Close(), f.Close())
}

func addFile(tw *tar.Writer, path, name string, mode int64, epoch time.Time) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > limit {
		return fmt.Errorf("refusing nonregular, empty, or oversized package file: %s", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: mode,
		Size: info.Size(), ModTime: epoch, Format: tar.FormatUSTAR}); err != nil {
		return err
	}
	n, err := io.Copy(tw, f)
	if err == nil && n != info.Size() {
		err = errors.New("package source changed during archive creation")
	}
	return err
}

func buildRelease(version, commit, out string, public ed25519.PublicKey) error {
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(commit) {
		return errors.New("release commit must be a full lowercase Git SHA")
	}
	head, err := command("git", "rev-parse", "HEAD")
	if err != nil || head != commit {
		return errors.New("release build requires exactly the checked-out commit")
	}
	if _, err := command("git", "diff", "--exit-code", "HEAD", "--"); err != nil {
		return errors.New("release build refuses modified tracked sources")
	}
	stamp, err := command("git", "show", "-s", "--format=%ct", commit)
	if err != nil {
		return err
	}
	seconds, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil {
		return err
	}
	epoch := time.Unix(seconds, 0).UTC()
	m := updates.Manifest{Version: version, Repository: updates.Repository, Schema: database.SchemaVersion}
	for _, arch := range []string{"amd64", "arm64"} {
		m.Assets = append(m.Assets, updates.Asset{Name: fmt.Sprintf("new-empire-control_%s_linux_%s.tar.gz", version, arch),
			OS: "linux", Arch: arch, Size: 1, SHA256: strings.Repeat("0", 64)})
	}
	if _, err := updates.CanonicalManifest(m); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(out), 0755); err != nil {
		return err
	}
	if err := os.Mkdir(out, 0755); err != nil {
		return fmt.Errorf("release output must not already exist: %w", err)
	}
	scratch, err := os.MkdirTemp("", "nec-release-build-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	for i, asset := range m.Assets {
		binary := filepath.Join(scratch, asset.Arch)
		ldflags := fmt.Sprintf("-s -w -buildid= -X main.version=%s -X main.commit=%s -X main.releasePublicKey=%s",
			version, commit, base64.StdEncoding.EncodeToString(public))
		c := exec.Command("go", "build", "-mod=readonly", "-trimpath", "-buildvcs=true", "-ldflags", ldflags, "-o", binary, "./cmd/control")
		c.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+asset.Arch, "GOAMD64=v1", "GOARM64=v8.0", "GOTOOLCHAIN=local")
		c.Stdout, c.Stderr = os.Stdout, os.Stderr
		if err := c.Run(); err != nil {
			return err
		}
		path := filepath.Join(out, asset.Name)
		if err := archive(path, epoch, func(tw *tar.Writer) error {
			return addFile(tw, binary, updates.BinaryName, 0755, epoch)
		}); err != nil {
			return err
		}
		raw, err := readRegular(path, limit)
		if err != nil {
			return err
		}
		hash := sha256.Sum256(raw)
		m.Assets[i].Size, m.Assets[i].SHA256 = int64(len(raw)), hex.EncodeToString(hash[:])
	}
	files, err := command("git", "ls-files", "-z", "--", "LICENSE", "README.md", "install.sh", "docs", "deploy")
	if err != nil {
		return err
	}
	if err := archive(filepath.Join(out, "documentation.tar.gz"), epoch, func(tw *tar.Writer) error {
		for _, name := range strings.Split(files, "\x00") {
			if name == "" {
				continue
			}
			info, err := os.Lstat(name)
			if err != nil {
				return err
			}
			if err := addFile(tw, name, name, int64(info.Mode().Perm()), epoch); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	raw, err := updates.CanonicalManifest(m)
	if err != nil {
		return err
	}
	return writeExclusive(filepath.Join(out, "release.json"), raw)
}

func readManifest(dir string) (updates.Manifest, []byte, error) {
	var m updates.Manifest
	raw, err := readRegular(filepath.Join(dir, "release.json"), 64<<10)
	if err != nil {
		return m, nil, err
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, nil, err
	}
	canonical, err := updates.CanonicalManifest(m)
	if err != nil || !bytes.Equal(canonical, raw) {
		return m, nil, errors.New("release.json is not the updater's canonical manifest")
	}
	if m.Schema != database.SchemaVersion {
		return m, nil, errors.New("release manifest schema does not match this source's SQLite schema")
	}
	return m, raw, nil
}

func verifyArchive(raw []byte, arch string) error {
	br := bufio.NewReader(bytes.NewReader(raw))
	gz, err := gzip.NewReader(br)
	if err != nil {
		return err
	}
	defer gz.Close()
	gz.Multistream(false)
	bounded := &io.LimitedReader{R: gz, N: limit + (1 << 20)}
	tr := tar.NewReader(bounded)
	h, err := tr.Next()
	if err != nil || h.Name != updates.BinaryName || h.Typeflag != tar.TypeReg || h.Mode != 0755 || h.Size <= 0 || h.Size > limit || len(h.PAXRecords) != 0 || len(h.Xattrs) != 0 {
		return errors.New("archive must contain exactly one ordinary executable without a directory prefix")
	}
	binary, err := io.ReadAll(tr)
	if err != nil || int64(len(binary)) != h.Size {
		return errors.New("archive binary is truncated")
	}
	if _, err := tr.Next(); !errors.Is(err, io.EOF) {
		return errors.New("archive contains extra entries or is truncated")
	}
	padding, err := io.ReadAll(bounded)
	if err != nil || bounded.N == 0 || len(bytes.Trim(padding, "\x00")) != 0 {
		return errors.New("archive trailer is corrupt or exceeds its expansion bound")
	}
	if _, err := br.Peek(1); !errors.Is(err, io.EOF) {
		return errors.New("archive contains trailing bytes or concatenated gzip members")
	}
	e, err := elf.NewFile(bytes.NewReader(binary))
	if err != nil {
		return fmt.Errorf("archive executable is not ELF: %w", err)
	}
	want := elf.EM_X86_64
	if arch == "arm64" {
		want = elf.EM_AARCH64
	}
	if e.Class != elf.ELFCLASS64 || e.Data != elf.ELFDATA2LSB || e.Machine != want || e.Type != elf.ET_EXEC && e.Type != elf.ET_DYN {
		return fmt.Errorf("archive executable does not match linux/%s", arch)
	}
	return nil
}

func payloadNames(m updates.Manifest) []string {
	names := []string{"documentation.tar.gz", "sbom.cdx.json", "release.json", "release.json.sig"}
	for _, asset := range m.Assets {
		names = append(names, asset.Name)
	}
	sort.Strings(names)
	return names
}

func checksumBytes(dir string, m updates.Manifest) ([]byte, error) {
	var output strings.Builder
	for _, name := range payloadNames(m) {
		raw, err := readRegular(filepath.Join(dir, name), limit)
		if err != nil {
			return nil, err
		}
		hash := sha256.Sum256(raw)
		fmt.Fprintf(&output, "%x  %s\n", hash, name)
	}
	return []byte(output.String()), nil
}

func verifyContent(dir string, m updates.Manifest) error {
	for _, asset := range m.Assets {
		raw, err := readRegular(filepath.Join(dir, asset.Name), limit)
		if err != nil {
			return err
		}
		hash := sha256.Sum256(raw)
		if int64(len(raw)) != asset.Size || hex.EncodeToString(hash[:]) != asset.SHA256 {
			return fmt.Errorf("archive size or SHA-256 differs from manifest: %s", asset.Name)
		}
		if err := verifyArchive(raw, asset.Arch); err != nil {
			return fmt.Errorf("%s: %w", asset.Name, err)
		}
	}
	if raw, err := readRegular(filepath.Join(dir, "documentation.tar.gz"), limit); err != nil || len(raw) == 0 {
		return errors.New("documentation archive is required")
	}
	raw, err := readRegular(filepath.Join(dir, "sbom.cdx.json"), 16<<20)
	if err != nil {
		return err
	}
	var bom struct {
		Format   string `json:"bomFormat"`
		Version  string `json:"specVersion"`
		Metadata struct {
			Component struct {
				Version string `json:"version"`
			} `json:"component"`
		} `json:"metadata"`
	}
	if json.Unmarshal(raw, &bom) != nil || bom.Format != "CycloneDX" || bom.Version != "1.6" || bom.Metadata.Component.Version != m.Version {
		return errors.New("SBOM must be CycloneDX 1.6 for this release version")
	}
	return nil
}

func sign(dir string, public ed25519.PublicKey, private ed25519.PrivateKey) error {
	m, raw, err := readManifest(dir)
	if err != nil {
		return err
	}
	if !bytes.Equal(private.Public().(ed25519.PublicKey), public) {
		return errors.New("release signing key does not match the pinned public key")
	}
	if err := verifyContent(dir, m); err != nil {
		return err
	}
	signature := base64.StdEncoding.EncodeToString(ed25519.Sign(private, raw))
	if err := writeExclusive(filepath.Join(dir, "release.json.sig"), []byte(signature)); err != nil {
		return err
	}
	checksums, err := checksumBytes(dir, m)
	if err != nil {
		return err
	}
	if err := writeExclusive(filepath.Join(dir, "checksums.txt"), checksums); err != nil {
		return err
	}
	return verify(dir, public)
}

func verify(dir string, public ed25519.PublicKey) error {
	m, raw, err := readManifest(dir)
	if err != nil {
		return err
	}
	sigRaw, err := readRegular(filepath.Join(dir, "release.json.sig"), 1024)
	if err != nil {
		return err
	}
	sig, err := decode(string(sigRaw), ed25519.SignatureSize)
	if err != nil || !ed25519.Verify(public, raw, sig) {
		return errors.New("release manifest signature does not match the pinned key")
	}
	if err := verifyContent(dir, m); err != nil {
		return err
	}
	checksums, err := checksumBytes(dir, m)
	if err != nil {
		return err
	}
	actual, err := readRegular(filepath.Join(dir, "checksums.txt"), 16<<10)
	if err != nil || !bytes.Equal(actual, checksums) {
		return errors.New("checksums.txt does not exactly match the release payload")
	}
	return nil
}

func localManifest(dir, version, commit string) (updates.Manifest, error) {
	m, _, err := readManifest(dir)
	if err != nil {
		return m, err
	}
	if m.Version != version || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(commit) {
		return m, errors.New("local provenance requires the manifest version and a full lowercase source SHA")
	}
	return m, nil
}

func provenanceBytes(dir string, m updates.Manifest, commit string) ([]byte, error) {
	type subject struct {
		Name   string `json:"name"`
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
	}
	value := struct {
		Schema     int       `json:"schema"`
		Repository string    `json:"repository"`
		Builder    string    `json:"builder"`
		Commit     string    `json:"commit"`
		Version    string    `json:"version"`
		Subjects   []subject `json:"subjects"`
	}{Schema: 1, Repository: updates.Repository, Builder: "local", Commit: commit, Version: m.Version}
	names := append(payloadNames(m), "checksums.txt")
	sort.Strings(names)
	for _, name := range names {
		raw, err := readRegular(filepath.Join(dir, name), limit)
		if err != nil {
			return nil, err
		}
		hash := sha256.Sum256(raw)
		value.Subjects = append(value.Subjects, subject{name, int64(len(raw)), hex.EncodeToString(hash[:])})
	}
	return json.Marshal(value)
}

func signLocal(dir, version, commit string, public ed25519.PublicKey, private ed25519.PrivateKey) error {
	m, err := localManifest(dir, version, commit)
	if err != nil {
		return err
	}
	if err := sign(dir, public, private); err != nil {
		return err
	}
	raw, err := provenanceBytes(dir, m, commit)
	if err != nil {
		return err
	}
	if err := writeExclusive(filepath.Join(dir, "provenance.json"), raw); err != nil {
		return err
	}
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(private, raw))
	if err := writeExclusive(filepath.Join(dir, "provenance.json.sig"), []byte(sig)); err != nil {
		return err
	}
	return verifyLocal(dir, version, commit, public)
}

func verifyLocal(dir, version, commit string, public ed25519.PublicKey) error {
	m, err := localManifest(dir, version, commit)
	if err != nil {
		return err
	}
	if err := verify(dir, public); err != nil {
		return err
	}
	expected, err := provenanceBytes(dir, m, commit)
	if err != nil {
		return err
	}
	raw, err := readRegular(filepath.Join(dir, "provenance.json"), 64<<10)
	if err != nil || !bytes.Equal(raw, expected) {
		return errors.New("local provenance does not match the source identity and exact release payload")
	}
	sigRaw, err := readRegular(filepath.Join(dir, "provenance.json.sig"), 1024)
	if err != nil {
		return err
	}
	sig, err := decode(string(sigRaw), ed25519.SignatureSize)
	if err != nil || !ed25519.Verify(public, raw, sig) {
		return errors.New("local provenance signature does not match the pinned key")
	}
	return nil
}

func mainErr() error {
	if len(os.Args) < 2 {
		return errors.New("usage: release build|sign|verify|sign-local|verify-local|dry-run [flags]")
	}
	mode := os.Args[1]
	flags := flag.NewFlagSet(mode, flag.ContinueOnError)
	dir := flags.String("dir", "dist/release", "release assets directory")
	out := flags.String("out", "dist/release", "new, nonexistent output directory")
	version := flags.String("version", "", "SemVer without v")
	commit := flags.String("commit", "", "exact source commit")
	keyPath := flags.String("public-key", publicKeyFile, "pinned base64 public key file")
	if err := flags.Parse(os.Args[2:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected release command arguments")
	}
	if mode == "dry-run" {
		public, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		if err := buildRelease(*version, *commit, *out, public); err != nil {
			return err
		}
		if _, err := command("python3", "-B", "scripts/release.py", "sbom", "--version", *version,
			"--commit", *commit, "--binary", filepath.Join(*out, fmt.Sprintf("new-empire-control_%s_linux_amd64.tar.gz", *version)),
			"--binary", filepath.Join(*out, fmt.Sprintf("new-empire-control_%s_linux_arm64.tar.gz", *version)),
			"--output", filepath.Join(*out, "sbom.cdx.json")); err != nil {
			return err
		}
		if err := signLocal(*out, *version, *commit, public, private); err != nil {
			return err
		}
		fmt.Println("Dry-run verified with an ephemeral key; these are NOT official update assets.")
		return nil
	}
	public, err := readPublicKey(*keyPath)
	if err != nil {
		return err
	}
	switch mode {
	case "build":
		return buildRelease(*version, *commit, *out, public)
	case "sign", "sign-local":
		private, err := signingKey(os.Getenv("RELEASE_SIGNING_KEY"), public)
		if err != nil {
			return err
		}
		if mode == "sign-local" {
			return signLocal(*dir, *version, *commit, public, private)
		}
		return sign(*dir, public, private)
	case "verify":
		return verify(*dir, public)
	case "verify-local":
		return verifyLocal(*dir, *version, *commit, public)
	default:
		return errors.New("unknown release command")
	}
}

func main() {
	if err := mainErr(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
