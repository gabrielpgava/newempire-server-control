package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/sha256"
	"debug/elf"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gabrielpgava/newempire-server-control/internal/updates"
)

func testELF(arch string) []byte {
	b := make([]byte, 64)
	copy(b, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(b[16:], uint16(elf.ET_EXEC))
	machine := elf.EM_X86_64
	if arch == "arm64" {
		machine = elf.EM_AARCH64
	}
	binary.LittleEndian.PutUint16(b[18:], uint16(machine))
	binary.LittleEndian.PutUint32(b[20:], 1)
	binary.LittleEndian.PutUint16(b[52:], 64)
	binary.LittleEndian.PutUint16(b[54:], 56)
	binary.LittleEndian.PutUint16(b[58:], 64)
	return b
}

func testArchive(t *testing.T, name, arch string, extra bool) []byte {
	t.Helper()
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	b := testELF(arch)
	for n := 0; n < 1 || n == 1 && extra; n++ {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Size: int64(len(b)), Mode: 0755}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(b); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func put(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), data, 0644); err != nil {
		t.Fatal(err)
	}
}

func fixture(t *testing.T) (string, ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	dir := t.TempDir()
	private := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))
	public := private.Public().(ed25519.PublicKey)
	m := updates.Manifest{Version: "1.2.3", Repository: updates.Repository, Schema: 1}
	for _, arch := range []string{"amd64", "arm64"} {
		name := "new-empire-control_1.2.3_linux_" + arch + ".tar.gz"
		raw := testArchive(t, updates.BinaryName, arch, false)
		hash := sha256.Sum256(raw)
		m.Assets = append(m.Assets, updates.Asset{Name: name, OS: "linux", Arch: arch,
			Size: int64(len(raw)), SHA256: hex.EncodeToString(hash[:])})
		put(t, dir, name, raw)
	}
	raw, err := updates.CanonicalManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	put(t, dir, "release.json", raw)
	put(t, dir, "documentation.tar.gz", []byte("fixture documentation"))
	put(t, dir, "sbom.cdx.json", []byte(`{"bomFormat":"CycloneDX","specVersion":"1.6","metadata":{"component":{"version":"1.2.3"}},"components":[]}`))
	return dir, public, private
}

func TestSignedReleaseMatchesCanonicalUpdaterContract(t *testing.T) {
	dir, public, private := fixture(t)
	if err := sign(dir, public, private); err != nil {
		t.Fatal(err)
	}
	if err := verify(dir, public); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"release.json", "release.json.sig"} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || bytes.ContainsAny(raw, "\r\n") {
			t.Fatalf("%s must have no newline: %v", name, err)
		}
	}
	if err := sign(dir, public, private); err == nil {
		t.Fatal("signing overwrote an existing artifact")
	}
}

func TestCorruptReleaseFailsClosed(t *testing.T) {
	for _, name := range []string{"release.json", "release.json.sig", "checksums.txt", "sbom.cdx.json",
		"new-empire-control_1.2.3_linux_amd64.tar.gz", "documentation.tar.gz"} {
		t.Run(name, func(t *testing.T) {
			dir, public, private := fixture(t)
			if err := sign(dir, public, private); err != nil {
				t.Fatal(err)
			}
			put(t, dir, name, []byte("corrupt"))
			if err := verify(dir, public); err == nil {
				t.Fatal("accepted corrupt release asset")
			}
		})
	}
}

func TestRejectNoncanonicalManifest(t *testing.T) {
	for _, mutate := range []func([]byte) []byte{
		func(b []byte) []byte { return append(b, '\n') },
		func(b []byte) []byte {
			return bytes.Replace(b, []byte(`"version":`), []byte(`"unknown":1,"version":`), 1)
		},
		func(b []byte) []byte {
			return bytes.Replace(b, []byte(`"schema":1`), []byte(`"schema":1,"schema":1`), 1)
		},
	} {
		dir, public, private := fixture(t)
		raw, err := os.ReadFile(filepath.Join(dir, "release.json"))
		if err != nil {
			t.Fatal(err)
		}
		put(t, dir, "release.json", mutate(raw))
		if err := sign(dir, public, private); err == nil {
			t.Fatal("accepted noncanonical manifest")
		}
	}
}

func TestSigningKeyMustMatchPinnedPublicKey(t *testing.T) {
	_, public, private := fixture(t)
	encoded := base64.StdEncoding.EncodeToString(private)
	if _, err := signingKey(encoded, public); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "not-base64", base64.StdEncoding.EncodeToString(private.Seed()), encoded + "\n"} {
		if _, err := signingKey(bad, public); err == nil {
			t.Fatal("accepted invalid or missing signing key")
		}
	}
	other := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, ed25519.SeedSize))
	if _, err := signingKey(encoded, other.Public().(ed25519.PublicKey)); err == nil {
		t.Fatal("accepted wrong pinned public key")
	}
	inconsistent := append([]byte(nil), private...)
	inconsistent[63] ^= 1
	if _, err := signingKey(base64.StdEncoding.EncodeToString(inconsistent), public); err == nil {
		t.Fatal("accepted inconsistent Go private key")
	}
}

func TestArchiveRejectsWrongArchitecturePathsExtraFilesAndTrailingData(t *testing.T) {
	valid := testArchive(t, updates.BinaryName, "amd64", false)
	if err := verifyArchive(valid, "amd64"); err != nil {
		t.Fatal(err)
	}
	cases := [][]byte{
		testArchive(t, "../new-empire-control", "amd64", false),
		testArchive(t, "package/new-empire-control", "amd64", false),
		testArchive(t, updates.BinaryName, "arm64", false),
		testArchive(t, updates.BinaryName, "amd64", true),
		append(append([]byte(nil), valid...), valid...),
		append(append([]byte(nil), valid...), []byte("trailing")...),
		valid[:len(valid)-5],
		[]byte("not gzip"),
	}
	for n, raw := range cases {
		if err := verifyArchive(raw, "amd64"); err == nil {
			t.Fatalf("accepted unsafe/corrupt archive case %d", n)
		}
	}
}

func TestArchiveIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	put(t, dir, "binary", testELF("amd64"))
	epoch := time.Unix(1234, 0).UTC()
	for _, name := range []string{"a.tar.gz", "b.tar.gz"} {
		if err := archive(filepath.Join(dir, name), epoch, func(tw *tar.Writer) error {
			return addFile(tw, filepath.Join(dir, "binary"), updates.BinaryName, 0755, epoch)
		}); err != nil {
			t.Fatal(err)
		}
	}
	a, _ := os.ReadFile(filepath.Join(dir, "a.tar.gz"))
	b, _ := os.ReadFile(filepath.Join(dir, "b.tar.gz"))
	if !bytes.Equal(a, b) {
		t.Fatal("same source and epoch produced different archive bytes")
	}
}

func TestManifestRequiresBothArchitecturesAndCurrentSchema(t *testing.T) {
	dir, _, _ := fixture(t)
	m, _, err := readManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	m.Schema++
	raw, err := updates.CanonicalManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	put(t, dir, "release.json", raw)
	if _, _, err := readManifest(dir); err == nil {
		t.Fatal("manifest schema did not match this source's current SQLite schema")
	}
	m.Assets = m.Assets[:1]
	if _, err := updates.CanonicalManifest(m); err == nil {
		t.Fatal("manifest accepted a missing architecture")
	}
	if !strings.HasPrefix(m.Assets[0].Name, "new-empire-control_1.2.3_linux_") {
		t.Fatal("unexpected archive naming contract")
	}
}

func TestLocalProvenanceBindsSourceAndEveryPayload(t *testing.T) {
	dir, public, private := fixture(t)
	sha := strings.Repeat("a", 40)
	if err := signLocal(dir, "1.2.3", sha, public, private); err != nil {
		t.Fatal(err)
	}
	if err := verifyLocal(dir, "1.2.3", sha, public); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "provenance.json"))
	if err != nil {
		t.Fatal(err)
	}
	var value struct {
		Repository string `json:"repository"`
		Builder    string `json:"builder"`
		Commit     string `json:"commit"`
		Version    string `json:"version"`
		Subjects   []struct {
			Name string `json:"name"`
		} `json:"subjects"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	if value.Repository != updates.Repository || value.Builder != "local" || value.Commit != sha || value.Version != "1.2.3" || len(value.Subjects) != 7 {
		t.Fatalf("incorrect local provenance: %s", raw)
	}
	found := false
	for _, subject := range value.Subjects {
		found = found || subject.Name == "checksums.txt"
	}
	if !found {
		t.Fatal("checksums are not bound to the local signature")
	}
	if err := verifyLocal(dir, "1.2.3", strings.Repeat("b", 40), public); err == nil {
		t.Fatal("accepted another source commit")
	}
	if err := verifyLocal(dir, "1.2.4", sha, public); err == nil {
		t.Fatal("accepted another release version")
	}
	if err := signLocal(dir, "1.2.3", sha, public, private); err == nil {
		t.Fatal("overwrote signed local artifacts")
	}
	// Recomputing unsigned checksums must not authenticate substituted documentation.
	put(t, dir, "documentation.tar.gz", []byte("substituted documentation"))
	m, _, err := readManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	checksums, err := checksumBytes(dir, m)
	if err != nil {
		t.Fatal(err)
	}
	put(t, dir, "checksums.txt", checksums)
	if err := verify(dir, public); err != nil {
		t.Fatal(err)
	}
	if err := verifyLocal(dir, "1.2.3", sha, public); err == nil {
		t.Fatal("accepted replaced documentation and recomputed checksums")
	}
}

func TestLocalSigningRejectsInvalidSourceBeforeWriting(t *testing.T) {
	for _, sha := range []string{"", "main", strings.Repeat("A", 40), strings.Repeat("a", 39)} {
		dir, public, private := fixture(t)
		if err := signLocal(dir, "1.2.3", sha, public, private); err == nil {
			t.Fatal("accepted an invalid source commit")
		}
		if _, err := os.Stat(filepath.Join(dir, "release.json.sig")); !os.IsNotExist(err) {
			t.Fatal("signed before validating the source")
		}
	}
}

func TestLocalProvenanceRejectsCorruption(t *testing.T) {
	for _, name := range []string{"provenance.json", "provenance.json.sig", "checksums.txt", "release.json.sig", "sbom.cdx.json", "new-empire-control_1.2.3_linux_arm64.tar.gz"} {
		t.Run(name, func(t *testing.T) {
			dir, public, private := fixture(t)
			sha := strings.Repeat("a", 40)
			if err := signLocal(dir, "1.2.3", sha, public, private); err != nil {
				t.Fatal(err)
			}
			put(t, dir, name, []byte("corrupt"))
			if err := verifyLocal(dir, "1.2.3", sha, public); err == nil {
				t.Fatal("accepted a corrupt locally signed asset")
			}
		})
	}
}
