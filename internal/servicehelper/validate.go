package servicehelper

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gabrielpgava/newempire-server-control/internal/model"
)

const (
	configPath   = "/etc/new-empire-control/runtime.json"
	manifestDir  = "/etc/new-empire-control/instances"
	unitDir      = "/etc/systemd/system"
	controlPath  = "/opt/new-empire-control/control"
	systemctl    = "/usr/bin/systemctl"
	journalctl   = "/usr/bin/journalctl"
	nft          = "/usr/sbin/nft"
	maxJSON      = 64 << 10
	maxOutput    = 4 << 20
	statusFields = "ActiveState,SubState,MainPID,MemoryCurrent,CPUUsageNSec,ExecMainStartTimestampMonotonic"
)

var (
	safePath    = regexp.MustCompile(`^/[A-Za-z0-9_./-]+$`)
	safeJar     = regexp.MustCompile(`^[A-Za-z0-9_./-]+\.jar$`)
	safeUser    = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,30}$`)
	safeVersion = regexp.MustCompile(`^[A-Za-z0-9_.+-]{1,128}$`)
)

type policy struct {
	PanelUser   string   `json:"panel_user"`
	ManagedRoot string   `json:"managed_root"`
	AdoptRoots  []string `json:"adopt_roots"`
	JavaPaths   []string `json:"java_paths"`
	MaxMemoryMB int      `json:"max_memory_mb"`
}

type manifest struct {
	Instance model.Instance `json:"instance"`
	Device   uint64         `json:"device"`
	Inode    uint64         `json:"inode"`
	UID      uint32         `json:"uid"`
	GID      uint32         `json:"gid"`
}

type request struct {
	op    string
	id    string
	lines int
}

func parseRequest(args []string) (request, error) {
	if len(args) == 1 && (args[0] == "panel-update" || args[0] == "provision") {
		return request{op: args[0]}, nil
	}
	if len(args) < 2 || !model.ValidID(args[1]) {
		return request{}, errors.New("expected an operation and a 32-character lowercase hexadecimal instance ID")
	}
	r := request{op: args[0], id: args[1], lines: 200}
	switch r.op {
	case "start", "stop", "restart", "status", "protect", "prepare", "reconfigure":
		if len(args) != 2 {
			return request{}, errors.New("operation does not accept additional arguments")
		}
	case "logs":
		if len(args) > 3 {
			return request{}, errors.New("logs accepts only an optional line count")
		}
		if len(args) == 3 {
			n, err := strconv.Atoi(args[2])
			if err != nil || n < 1 || n > 1000 || strconv.Itoa(n) != args[2] {
				return request{}, errors.New("log line count must be an integer from 1 to 1000")
			}
			r.lines = n
		}
	default:
		return request{}, errors.New("unsupported runtime operation")
	}
	return r, nil
}

func decode(r io.Reader, dst any) error {
	b, err := io.ReadAll(io.LimitReader(r, maxJSON+1))
	if err != nil {
		return err
	}
	if len(b) > maxJSON {
		return errors.New("JSON exceeds 64 KiB")
	}
	keys := json.NewDecoder(strings.NewReader(string(b)))
	if err := uniqueJSONKeys(keys, 0); err != nil {
		return errors.New("invalid, duplicate, or excessively nested JSON fields")
	}
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return errors.New("invalid or unknown JSON fields")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("expected exactly one JSON object")
	}
	return nil
}

func uniqueJSONKeys(d *json.Decoder, depth int) error {
	if depth > 64 {
		return errors.New("JSON depth exceeded")
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	for d.More() {
		if delim == '{' {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok {
				return errors.New("invalid key")
			}
			name = strings.ToLower(name)
			if seen[name] {
				return errors.New("duplicate key")
			}
			seen[name] = true
		}
		if err := uniqueJSONKeys(d, depth+1); err != nil {
			return err
		}
	}
	_, err = d.Token()
	return err
}

func cleanPath(path string) bool {
	return len(path) <= 4096 && path != "/" && safePath.MatchString(path) && filepath.Clean(path) == path
}

func within(root, path string) bool {
	return path == root || strings.HasPrefix(path, root+"/")
}

func (p policy) validate() error {
	if !safeUser.MatchString(p.PanelUser) || p.PanelUser == "root" {
		return errors.New("panel_user must name a non-root service account")
	}
	if p.MaxMemoryMB < 256 || p.MaxMemoryMB > 1<<20 {
		return errors.New("max_memory_mb must be between 256 and 1048576")
	}
	if len(p.JavaPaths) == 0 || len(p.JavaPaths) > 32 || len(p.AdoptRoots) > 32 {
		return errors.New("configure 1–32 exact java_paths and at most 32 adopt_roots")
	}
	roots := append([]string{p.ManagedRoot}, p.AdoptRoots...)
	for i, root := range roots {
		if !cleanPath(root) {
			return errors.New("data roots must be canonical absolute paths without special characters")
		}
		for _, reserved := range []string{"/etc", "/usr", "/opt", "/bin", "/sbin", "/proc", "/sys", "/dev", "/run", "/tmp", "/var/tmp", "/root", "/home", "/var/lib/new-empire-control"} {
			if within(reserved, root) || within(root, reserved) {
				return errors.New("data roots must not overlap protected system or panel directories")
			}
		}
		for _, other := range roots[:i] {
			if within(root, other) || within(other, root) {
				return errors.New("managed and adoption roots must not overlap")
			}
		}
	}
	seen := make(map[string]bool)
	for _, path := range p.JavaPaths {
		if !cleanPath(path) || seen[path] {
			return errors.New("java_paths must contain distinct canonical absolute executable paths")
		}
		seen[path] = true
	}
	return nil
}

func validateInstance(p policy, i model.Instance) error {
	if !model.ValidID(i.ID) || !i.EULA {
		return errors.New("instance requires a valid ID and explicit EULA acceptance")
	}
	if !utf8.ValidString(i.Name) || len([]rune(i.Name)) == 0 || len([]rune(i.Name)) > 128 || strings.IndexFunc(i.Name, unicode.IsControl) >= 0 || strings.TrimSpace(i.Name) == "" {
		return errors.New("instance name must be 1–128 printable characters")
	}
	if !safeVersion.MatchString(i.MinecraftVersion) || !safeVersion.MatchString(i.LoaderVersion) || i.CreatedAt.IsZero() {
		return errors.New("instance requires explicit Minecraft/Fabric versions and creation time")
	}
	if !cleanPath(i.Root) {
		return errors.New("instance root must be a canonical absolute path without special characters")
	}
	if i.Adopted {
		allowed := false
		for _, root := range p.AdoptRoots {
			allowed = allowed || (i.Root != root && within(root, i.Root))
		}
		if !allowed {
			return errors.New("adopted instance is outside configured adoption roots")
		}
	} else if i.Root != filepath.Join(p.ManagedRoot, i.ID) {
		return errors.New("managed instance root must be managed_root/ID")
	}
	allowed := false
	for _, path := range p.JavaPaths {
		allowed = allowed || i.JavaPath == path
	}
	if !allowed {
		return errors.New("Java executable is not explicitly allowlisted")
	}
	if len(i.Jar) > 1024 || !safeJar.MatchString(i.Jar) || filepath.IsAbs(i.Jar) || filepath.Clean(i.Jar) != i.Jar || strings.HasPrefix(i.Jar, "../") {
		return errors.New("launcher must be a safe relative .jar path")
	}
	if i.MemoryMB < 256 || i.MemoryMB > p.MaxMemoryMB {
		return errors.New("instance memory is outside root-approved limits")
	}
	if i.Port < 1024 || i.Port > 65535 || i.RCONPort < 1024 || i.RCONPort > 65535 || i.Port == i.RCONPort {
		return errors.New("game and RCON ports must be distinct unprivileged TCP ports")
	}
	return nil
}

func unitName(id string) string { return "nec-mc-" + id + ".service" }

// Linux account names are limited to 31 portable characters; registrations reject prefix collisions.
func accountName(id string) string { return "nec-mc-" + id[:24] }

func renderUnit(i model.Instance) []byte {
	return []byte(fmt.Sprintf(`[Unit]
Description=New Empire Minecraft instance %s
After=network.target

[Service]
Type=simple
User=%s
Group=%s
WorkingDirectory=%s
ExecStartPre=+%s runtime-helper protect %s
ExecStart=%s -Xms256M -Xmx%dM -jar ./%s nogui
KillSignal=SIGTERM
KillMode=control-group
TimeoutStopSec=120s
SendSIGKILL=no
Restart=no
UMask=0007
NoNewPrivileges=yes
CapabilityBoundingSet=
AmbientCapabilities=
ProtectSystem=strict
ProtectHome=yes
ReadWritePaths=%s
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
ProtectProc=invisible
RestrictSUIDSGID=yes
RestrictRealtime=yes
RestrictNamespaces=yes
LockPersonality=yes
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK
SystemCallArchitectures=native
MemoryAccounting=yes
MemoryMax=%dM
MemorySwapMax=0
CPUAccounting=yes
TasksMax=512
LimitNOFILE=65536
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
`, i.ID, accountName(i.ID), accountName(i.ID), i.Root, controlPath, i.ID, i.JavaPath, i.MemoryMB, i.Jar, i.Root, i.MemoryMB+1024))
}
