// Package storage transfers verified local backups to public S3-compatible storage.
package storage

import (
	"errors"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"unicode"

	"github.com/gabrielpgava/newempire-server-control/internal/netguard"
)

// shortcut: single-part PUTs are capped at 5 GiB; add multipart for larger verified backups.
const MaxObjectSize int64 = 5 << 30

type Config struct {
	Enabled      bool   `json:"enabled"`
	Endpoint     string `json:"endpoint"`
	Region       string `json:"region"`
	Bucket       string `json:"bucket"`
	AccessKey    string `json:"access_key"`
	SecretKey    string `json:"secret_key"`
	SessionToken string `json:"session_token,omitempty"`
	Prefix       string `json:"prefix,omitempty"`
}

type Summary struct {
	Configured bool `json:"configured"`
}

func (c Config) Redacted() Summary {
	return Summary{Configured: c.Enabled && c.Validate() == nil}
}

var (
	bucketPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	regionPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	keyPattern    = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)
)

func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	if _, err := endpointURL(c.Endpoint); err != nil {
		return err
	}
	if !regionPattern.MatchString(c.Region) {
		return errors.New("offsite region must be a nonempty lowercase region identifier")
	}
	if !bucketPattern.MatchString(c.Bucket) || strings.Contains(c.Bucket, "..") || strings.Contains(c.Bucket, ".-") || strings.Contains(c.Bucket, "-.") || net.ParseIP(c.Bucket) != nil {
		return errors.New("offsite bucket must be a safe DNS-style bucket name")
	}
	if !credential(c.AccessKey, 256) || strings.ContainsAny(c.AccessKey, "/,= ") || !credential(c.SecretKey, 4096) || (c.SessionToken != "" && !credential(c.SessionToken, 16384)) {
		return errors.New("offsite credentials are missing or invalid")
	}
	if c.Prefix != "" {
		if err := validKey(c.Prefix); err != nil {
			return errors.New("offsite prefix must be a safe relative object path")
		}
	}
	return nil
}

func credential(s string, max int) bool {
	if len(s) == 0 || len(s) > max || strings.TrimSpace(s) != s {
		return false
	}
	for _, r := range s {
		if r > unicode.MaxASCII || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func endpointURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 2048 || strings.TrimSpace(raw) != raw || strings.Contains(raw, "#") || u.Scheme != "https" || u.Hostname() == "" || strings.HasSuffix(u.Host, ":") || u.User != nil || u.Opaque != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || (u.Port() != "" && u.Port() != "443") {
		return nil, errors.New("offsite endpoint must be an HTTPS origin on port 443 without credentials, a path, query or fragment")
	}
	host := u.Hostname()
	if ip, err := netip.ParseAddr(host); err == nil {
		if ip.Zone() != "" || !netguard.Public(ip) {
			return nil, errors.New("offsite endpoint must use a public host")
		}
	} else {
		if len(host) > 253 || !strings.Contains(host, ".") || strings.HasSuffix(host, ".") {
			return nil, errors.New("offsite endpoint must use a public DNS hostname")
		}
		for _, label := range strings.Split(host, ".") {
			if len(label) == 0 || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
				return nil, errors.New("offsite endpoint hostname is invalid")
			}
			for _, r := range label {
				if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-') {
					return nil, errors.New("offsite endpoint hostname is invalid")
				}
			}
		}
	}
	u.Path = ""
	return u, nil
}

func validKey(key string) error {
	if len(key) == 0 || len(key) > 1024 || !keyPattern.MatchString(key) || strings.HasPrefix(key, "/") {
		return errors.New("offsite key must be a safe relative object path")
	}
	for _, part := range strings.Split(key, "/") {
		if part == "" || part == "." || part == ".." {
			return errors.New("offsite key must be a safe relative object path")
		}
	}
	return nil
}
