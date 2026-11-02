package updates

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestSemVer(t *testing.T) {
	ordered := []string{"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta", "1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0", "1.0.1", "1.9.0", "1.10.0", "2.0.0", "999999999999999999999999.0.0"}
	for i, s := range ordered {
		v, err := parseVersion(s)
		must(t, err)
		if v.compare(v) != 0 {
			t.Fatal("version differs from itself", s)
		}
		for _, other := range ordered[i+1:] {
			w, err := parseVersion(other)
			must(t, err)
			if v.compare(w) >= 0 || w.compare(v) <= 0 {
				t.Fatalf("incorrect precedence: %s < %s", s, other)
			}
		}
	}
	a, _ := parseVersion("1.2.3+first")
	b, _ := parseVersion("1.2.3+different.000")
	if a.compare(b) != 0 {
		t.Fatal("build metadata changed precedence")
	}
	for _, s := range []string{"", "v1.2.3", "1.2", "01.2.3", "1.02.3", "1.2.03", "1.2.3-01", "1.2.3-beta.01", "1.2.3-", "1.2.3+", "1.2.3-a..b", "1.2.3+a..b", "1.2.3-a_1", "1.2.3\n", "1.2.3+foo+bar", "1.2.3-β"} {
		if _, err := parseVersion(s); err == nil {
			t.Errorf("accepted invalid SemVer %q", s)
		}
	}
}

func TestSignedManifestCanonicalContract(t *testing.T) {
	f := newGithub(t)
	m := f.add(t, "1.2.0", 1)
	raw, err := CanonicalManifest(m)
	must(t, err)
	wantPrefix := `{"version":"1.2.0","repository":"` + Repository + `","schema":1,"assets":[{"name":"new-empire-control_1.2.0_linux_amd64.tar.gz","os":"linux","arch":"amd64","size":`
	if !bytes.HasPrefix(raw, []byte(wantPrefix)) || bytes.HasSuffix(raw, []byte("\n")) {
		t.Fatal("signing field order or whitespace contract changed", string(raw))
	}
	sign := func(b []byte) []byte { return []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(f.key, b))) }
	_, err = verifyManifest(raw, sign(raw), f.public)
	must(t, err)
	invalid := map[string][]byte{
		"newline":    append(bytes.Clone(raw), '\n'),
		"whitespace": []byte(" " + string(raw)),
		"duplicate":  []byte(strings.Replace(string(raw), `"version":"1.2.0"`, `"version":"1.2.0","version":"1.2.0"`, 1)),
		"unknown":    []byte(strings.Replace(string(raw), `"schema":1`, `"schema":1,"extra":true`, 1)),
		"escaped":    []byte(strings.Replace(string(raw), "1.2.0", `1.\u0032.0`, 1)),
		"exponent":   []byte(strings.Replace(string(raw), `"schema":1`, `"schema":1e0`, 1)),
		"trailing":   append(bytes.Clone(raw), []byte("{}")...),
	}
	for name, body := range invalid {
		t.Run(name, func(t *testing.T) {
			if _, err := verifyManifest(body, sign(body), f.public); !errors.Is(err, ErrVerification) {
				t.Fatalf("signed ambiguous manifest accepted: %v", err)
			}
		})
	}
	for name, mutate := range map[string]func(*Manifest){
		"repository":             func(m *Manifest) { m.Repository = "attacker/fork" },
		"duplicate architecture": func(m *Manifest) { m.Assets[1] = m.Assets[0] },
		"wrong OS":               func(m *Manifest) { m.Assets[0].OS = "darwin" },
		"wrong arch":             func(m *Manifest) { m.Assets[0].Arch = "386" },
		"uppercase hash":         func(m *Manifest) { m.Assets[0].SHA256 = strings.ToUpper(m.Assets[0].SHA256) },
		"size":                   func(m *Manifest) { m.Assets[0].Size = 0 },
		"oversize":               func(m *Manifest) { m.Assets[0].Size = maxArchiveBytes + 1 },
		"schema":                 func(m *Manifest) { m.Schema = 0 },
		"order":                  func(m *Manifest) { m.Assets[0], m.Assets[1] = m.Assets[1], m.Assets[0] },
		"missing architecture":   func(m *Manifest) { m.Assets = m.Assets[:1] },
	} {
		t.Run(name, func(t *testing.T) {
			copy := m
			copy.Assets = append([]Asset{}, m.Assets...)
			mutate(&copy)
			body, err := json.Marshal(copy)
			must(t, err)
			if _, err := verifyManifest(body, sign(body), f.public); !errors.Is(err, ErrVerification) {
				t.Fatalf("invalid signed manifest accepted: %v", err)
			}
		})
	}
	bad := bytes.Clone(sign(raw))
	bad[0] ^= 1
	for _, sig := range [][]byte{nil, bad, append(sign(raw), '\n'), []byte(strings.TrimRight(string(sign(raw)), "="))} {
		if _, err := verifyManifest(raw, sig, f.public); !errors.Is(err, ErrVerification) {
			t.Fatalf("bad signature encoding accepted: %v", err)
		}
	}
	other := newGithub(t)
	if _, err := verifyManifest(raw, sign(raw), other.public); !errors.Is(err, ErrVerification) {
		t.Fatal("accepted an unpinned signing key", err)
	}
}
