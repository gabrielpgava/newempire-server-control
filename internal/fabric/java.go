package fabric

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type JavaInstallation struct {
	Path    string `json:"path"`
	Version string `json:"version"`
	Major   int    `json:"major"`
	Error   string `json:"error,omitempty"`
}

var javaVersionLine = regexp.MustCompile(`(?m)^(?:openjdk|java) version "([^"\r\n]{1,128})"`)
var javaVersionNumber = regexp.MustCompile(`^([0-9]+)(?:[._+\-][0-9A-Za-z._+\-]+)?$`)

func ParseJavaVersion(output string) (version string, major int, err error) {
	if len(output) > 64<<10 {
		return "", 0, errors.New("Java version output exceeds size limit")
	}
	match := javaVersionLine.FindStringSubmatch(output)
	if match == nil {
		return "", 0, errors.New("Java did not return a recognized version line")
	}
	version = match[1]
	number := version
	if strings.HasPrefix(number, "1.") {
		number = strings.TrimPrefix(number, "1.")
	}
	parts := javaVersionNumber.FindStringSubmatch(number)
	if parts == nil {
		return "", 0, errors.New("Java returned an invalid version number")
	}
	major, err = strconv.Atoi(parts[1])
	if err != nil || major < 1 || major > 255 {
		return "", 0, errors.New("Java major version is out of bounds")
	}
	return version, major, nil
}

// InspectJava never searches PATH or executes a path outside the trusted
// administrator-configured allowlist. Paths and their targets must be admin-owned.
func InspectJava(ctx context.Context, executable string, allowlist []string) (JavaInstallation, error) {
	installation := JavaInstallation{Path: executable}
	allowed := false
	if !filepath.IsAbs(executable) || filepath.Clean(executable) != executable || strings.ContainsAny(executable, "\x00\r\n") {
		return installation, errors.New("Java executable must be a clean absolute path")
	}
	if len(allowlist) > 32 {
		return installation, errors.New("Java executable allowlist exceeds 32 entries")
	}
	for _, path := range allowlist {
		if path == executable {
			allowed = true
		}
	}
	if !allowed {
		return installation, errors.New("Java executable is not allowlisted")
	}
	info, err := os.Stat(executable)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 || info.Mode().Perm()&0022 != 0 {
		return installation, errors.New("Java executable must be a regular executable not writable by group or others")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-version")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"}
	var output javaOutput
	cmd.Stdout, cmd.Stderr = &output, &output
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return installation, ctx.Err()
		}
		return installation, errors.New("allowlisted Java version probe failed")
	}
	installation.Version, installation.Major, err = ParseJavaVersion(output.String())
	return installation, err
}

func InspectJavaInstallations(ctx context.Context, allowlist []string) ([]JavaInstallation, error) {
	if len(allowlist) == 0 {
		return nil, errors.New("no Java executables configured; configure an explicit executable allowlist")
	}
	if len(allowlist) > 32 {
		return nil, errors.New("Java executable allowlist exceeds 32 entries")
	}
	installations := make([]JavaInstallation, 0, len(allowlist))
	seen := make(map[string]bool)
	for _, executable := range allowlist {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if seen[executable] {
			continue
		}
		seen[executable] = true
		installation, err := InspectJava(ctx, executable, allowlist)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil {
			installation.Error = err.Error()
		}
		installations = append(installations, installation)
	}
	return installations, nil
}

// ChooseJava prefers the smallest working major meeting the metadata minimum.
// This is a JVM minimum check, not a promise of mod or future-JVM compatibility.
func ChooseJava(installations []JavaInstallation, minimum int) (JavaInstallation, error) {
	if minimum < 1 || minimum > 255 {
		return JavaInstallation{}, errors.New("invalid required Java major")
	}
	var chosen JavaInstallation
	for _, installation := range installations {
		if installation.Error != "" || installation.Path == "" || installation.Major < minimum || installation.Major > 255 {
			continue
		}
		if chosen.Path == "" || installation.Major < chosen.Major {
			chosen = installation
		}
	}
	if chosen.Path == "" {
		return chosen, fmt.Errorf("no allowlisted working Java installation meets minimum major %d", minimum)
	}
	return chosen, nil
}

type javaOutput struct{ bytes.Buffer }

func (b *javaOutput) Write(p []byte) (int, error) {
	if len(p) > (64<<10)-b.Len() {
		return 0, errors.New("Java output exceeds size limit")
	}
	return b.Buffer.Write(p)
}
