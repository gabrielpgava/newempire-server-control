package updates

import (
	"context"
	"crypto/ed25519"
	"net/http"
	"path/filepath"
	"runtime"
	"time"

	"github.com/gabrielpgava/newempire-server-control/internal/netguard"
)

type Channel string

const (
	Stable Channel = "stable"
	Beta   Channel = "beta"
)

type Service interface {
	StopPanel(context.Context) error
	RestartPanel(context.Context) error
	Health(context.Context) error
}

type Options struct {
	InstallDir     string
	CurrentVersion string
	Schema         int
	PublicKey      string
	Client         *http.Client
	Service        Service
	Snapshot       func(context.Context, string) error
	Restore        func(context.Context, string) error
	HealthTimeout  time.Duration
}

type Manager struct {
	opts   Options
	key    ed25519.PublicKey
	client *http.Client
	arch   string
}

type CheckResult struct {
	CurrentVersion string    `json:"current_version"`
	Channel        Channel   `json:"channel"`
	Available      *Manifest `json:"available"`
}

func New(opts Options) (*Manager, error) {
	key, err := decodeBase64(opts.PublicKey, ed25519.PublicKeySize)
	if err != nil {
		return nil, problem(ErrUnavailable, "a pinned Ed25519 release public key is required")
	}
	if _, err := parseVersion(opts.CurrentVersion); err != nil {
		return nil, err
	}
	if len(opts.CurrentVersion) > 120 {
		return nil, problem(ErrInvalid, "installed version exceeds the 120-byte packaging limit")
	}
	if !filepath.IsAbs(opts.InstallDir) || filepath.Clean(opts.InstallDir) == string(filepath.Separator) || opts.Schema < 1 {
		return nil, problem(ErrInvalid, "an absolute non-root install directory and positive database schema are required")
	}
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		return nil, problem(ErrUnavailable, "only linux/amd64 and linux/arm64 panel archives are supported")
	}
	if opts.HealthTimeout < 0 || opts.HealthTimeout > 5*time.Minute {
		return nil, problem(ErrInvalid, "health timeout must be between zero and five minutes")
	}
	if opts.HealthTimeout == 0 {
		opts.HealthTimeout = time.Minute
	}
	safe := netguard.New("api.github.com", "github.com", "release-assets.githubusercontent.com", "objects.githubusercontent.com")
	client := safe
	if opts.Client != nil {
		c := *opts.Client
		// Only transport injection is intended for tests; redirect restrictions are never relaxed.
		c.CheckRedirect = safe.CheckRedirect
		if c.Timeout <= 0 || c.Timeout > safe.Timeout {
			c.Timeout = safe.Timeout
		}
		client = &c
	}
	return &Manager{opts: opts, key: key, client: client, arch: runtime.GOARCH}, nil
}

func channel(c Channel) (Channel, error) {
	if c == "" {
		c = Stable
	}
	if c != Stable && c != Beta {
		return c, problem(ErrInvalid, "channel must be stable or beta")
	}
	return c, nil
}

func (m *Manager) helperReady() error {
	if m.opts.Service == nil || m.opts.Snapshot == nil || m.opts.Restore == nil {
		return problem(ErrUnavailable, "independent updater requires panel service and consistent snapshot/restore adapters")
	}
	return nil
}
