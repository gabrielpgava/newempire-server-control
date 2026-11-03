// Package notifications delivers fixed, credential-free operational events.
package notifications

import (
	"errors"
	"net/mail"
	"net/netip"
	"net/url"
	"strings"
	"unicode"

	"github.com/gabrielpgava/newempire-server-control/internal/netguard"
)

type Config struct {
	Enabled      bool     `json:"enabled"`
	Kind         string   `json:"kind"`
	URL          string   `json:"url,omitempty"`
	SMTPHost     string   `json:"smtp_host,omitempty"`
	SMTPUsername string   `json:"smtp_username,omitempty"`
	SMTPPassword string   `json:"smtp_password,omitempty"`
	From         string   `json:"from,omitempty"`
	To           []string `json:"to,omitempty"`
}

type Summary struct {
	Configured bool `json:"configured"`
}

func (c Config) Redacted() Summary {
	return Summary{Configured: c.Enabled && c.Validate() == nil}
}

func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	switch c.Kind {
	case "discord", "webhook":
		u, err := webhookURL(c.URL)
		if err != nil {
			return err
		}
		if c.SMTPHost != "" || c.SMTPUsername != "" || c.SMTPPassword != "" || c.From != "" || len(c.To) != 0 {
			return errors.New("HTTP notifications must not contain SMTP configuration")
		}
		if c.Kind == "discord" {
			parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
			if !strings.EqualFold(u.Hostname(), "discord.com") || len(parts) != 4 || parts[0] != "api" || parts[1] != "webhooks" || !digits(parts[2]) || !token(parts[3]) || u.RawQuery != "" || u.ForceQuery {
				return errors.New("Discord notifications require an official discord.com API webhook URL")
			}
		}
	case "smtp":
		if c.URL != "" {
			return errors.New("SMTP notifications must not contain a webhook URL")
		}
		if err := publicHost(c.SMTPHost); err != nil {
			return err
		}
		if !safeValue(c.SMTPUsername, 256) || !safeValue(c.SMTPPassword, 4096) {
			return errors.New("SMTP username and password are required and must not contain control characters")
		}
		if !mailbox(c.From) || len(c.To) == 0 || len(c.To) > 10 {
			return errors.New("SMTP requires a plain sender address and one to ten recipient addresses")
		}
		seen := make(map[string]bool)
		for _, recipient := range c.To {
			if !mailbox(recipient) || seen[strings.ToLower(recipient)] {
				return errors.New("SMTP recipients must be distinct plain email addresses")
			}
			seen[strings.ToLower(recipient)] = true
		}
	default:
		return errors.New("notification kind must be discord, webhook or smtp")
	}
	return nil
}

func webhookURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 4096 || strings.TrimSpace(raw) != raw || strings.Contains(raw, "#") || u.Scheme != "https" || u.Hostname() == "" || strings.HasSuffix(u.Host, ":") || u.User != nil || u.Opaque != "" || (u.Port() != "" && u.Port() != "443") {
		return nil, errors.New("notification URL must use HTTPS port 443 without user information or a fragment")
	}
	if err := publicHost(u.Hostname()); err != nil {
		return nil, err
	}
	if strings.ContainsAny(u.Path, "\r\n\x00\\") {
		return nil, errors.New("notification URL path is invalid")
	}
	return u, nil
}

func publicHost(host string) error {
	if ip, err := netip.ParseAddr(host); err == nil {
		if ip.Zone() == "" && netguard.Public(ip) {
			return nil
		}
		return errors.New("notification destination must be a public host")
	}
	if len(host) > 253 || !strings.Contains(host, ".") || strings.HasSuffix(host, ".") {
		return errors.New("notification destination must be a public DNS hostname")
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return errors.New("notification destination hostname is invalid")
		}
		for _, r := range label {
			if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-') {
				return errors.New("notification destination hostname is invalid")
			}
		}
	}
	return nil
}

func safeValue(s string, max int) bool {
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

func mailbox(s string) bool {
	if !safeValue(s, 254) {
		return false
	}
	a, err := mail.ParseAddress(s)
	return err == nil && a.Name == "" && a.Address == s && strings.Contains(s, "@")
}

func digits(s string) bool {
	if len(s) < 1 || len(s) > 32 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func token(s string) bool {
	if len(s) < 1 || len(s) > 256 {
		return false
	}
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-') {
			return false
		}
	}
	return true
}
