package updates

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	Repository      = "gabrielpgava/newempire-server-control"
	BinaryName      = "new-empire-control"
	maxArchiveBytes = 512 << 20
	maxBinaryBytes  = 512 << 20
	maxManifestSize = 64 << 10
)

// Field order is part of the signing contract; see API.md.
type Manifest struct {
	Version    string  `json:"version"`
	Repository string  `json:"repository"`
	Schema     int     `json:"schema"`
	Assets     []Asset `json:"assets"`
}

type Asset struct {
	Name   string `json:"name"`
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

func archiveName(v, arch string) string {
	return fmt.Sprintf("%s_%s_linux_%s.tar.gz", BinaryName, v, arch)
}

// CanonicalManifest returns the only accepted byte representation, for signing.
// Assets must already be sorted so the signing caller cannot accidentally reorder them.
func CanonicalManifest(m Manifest) ([]byte, error) {
	if _, err := parseVersion(m.Version); err != nil {
		return nil, err
	}
	if len(m.Version) > 120 || m.Repository != Repository || m.Schema < 1 || len(m.Assets) != 2 {
		return nil, problem(ErrInvalid, "manifest requires the official repository, a positive schema, a version of at most 120 bytes, and both Linux architectures")
	}
	for i, a := range m.Assets {
		arch := []string{"amd64", "arm64"}[i]
		if a.OS != "linux" || a.Arch != arch || a.Name != archiveName(m.Version, arch) || a.Size <= 0 || a.Size > maxArchiveBytes || !validHash(a.SHA256) {
			return nil, problem(ErrInvalid, "invalid or unsorted manifest asset for linux/%s", arch)
		}
	}
	return json.Marshal(m)
}

func validHash(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 32 && s == strings.ToLower(s)
}

func decodeBase64(s string, size int) ([]byte, error) {
	b, err := base64.StdEncoding.Strict().DecodeString(s)
	if err != nil || len(b) != size || base64.StdEncoding.EncodeToString(b) != s {
		return nil, fmt.Errorf("expected canonical base64 encoding of %d bytes", size)
	}
	return b, nil
}

func verifyManifest(raw, signature []byte, key ed25519.PublicKey) (Manifest, error) {
	var m Manifest
	if len(raw) > maxManifestSize {
		return m, problem(ErrVerification, "manifest is too large")
	}
	sig, err := decodeBase64(string(signature), ed25519.SignatureSize)
	if err != nil || !ed25519.Verify(key, raw, sig) {
		return m, problem(ErrVerification, "manifest signature does not match the pinned public key")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&m); err != nil {
		return m, problem(ErrVerification, "invalid manifest JSON")
	}
	canonical, err := CanonicalManifest(m)
	if err != nil || !bytes.Equal(raw, canonical) {
		return m, problem(ErrVerification, "manifest is not in the required canonical format")
	}
	return m, nil
}

func (m Manifest) asset(arch string) (Asset, error) {
	for _, a := range m.Assets {
		if a.OS == "linux" && a.Arch == arch {
			return a, nil
		}
	}
	return Asset{}, problem(ErrVerification, "release has no linux/%s archive", arch)
}
