// Package runtime provides the unprivileged systemd and Minecraft console adapter.
package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gabrielpgava/newempire-server-control/internal/model"
	"github.com/gabrielpgava/newempire-server-control/internal/rcon"
)

var ErrUnsupported = errors.New("systemd runtime is supported only on Linux")

type ServerRuntime interface {
	Status(context.Context, model.Instance) (model.RuntimeStatus, error)
	Start(context.Context, model.Instance) error
	Stop(context.Context, model.Instance) error
	Restart(context.Context, model.Instance) error
	Logs(context.Context, model.Instance, int) ([]model.LogLine, error)
	Command(context.Context, model.Instance, string) (string, error)
}

type Systemd struct {
	helper   string
	platform string
	run      func(context.Context, string, ...string) ([]byte, error)
	clock    func() (float64, error)
	console  func(context.Context, string, string, string) (string, error)
}

var _ ServerRuntime = (*Systemd)(nil)

func NewSystemd(helper string) *Systemd {
	return &Systemd{helper: helper, platform: goruntime.GOOS, run: runBounded, clock: monotonicSeconds, console: rcon.Execute}
}

func (s *Systemd) invoke(ctx context.Context, inst model.Instance, action string, extra ...string) ([]byte, error) {
	if s.platform != "linux" {
		return nil, ErrUnsupported
	}
	if !model.ValidID(inst.ID) {
		return nil, errors.New("invalid runtime instance ID")
	}
	if !filepath.IsAbs(s.helper) || filepath.Clean(s.helper) != s.helper || strings.ContainsAny(s.helper, "\x00\r\n") {
		return nil, errors.New("runtime helper must be a clean absolute path")
	}
	timeout := 2 * time.Minute
	if action == "status" || action == "logs" {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	args := append([]string{"-n", s.helper, action, inst.ID}, extra...)
	output, err := s.run(ctx, "/usr/bin/sudo", args...)
	if err != nil {
		return nil, fmt.Errorf("runtime helper %s failed; check service diagnostics and current status: %w", action, err)
	}
	return output, nil
}

func (s *Systemd) Start(ctx context.Context, inst model.Instance) error {
	if s.platform != "linux" {
		return ErrUnsupported
	}
	if !inst.EULA {
		return errors.New("Minecraft EULA must be explicitly accepted before starting")
	}
	_, err := s.invoke(ctx, inst, "start")
	return err
}

func (s *Systemd) Stop(ctx context.Context, inst model.Instance) error {
	_, err := s.invoke(ctx, inst, "stop")
	return err
}

func (s *Systemd) Restart(ctx context.Context, inst model.Instance) error {
	if s.platform != "linux" {
		return ErrUnsupported
	}
	if !inst.EULA {
		return errors.New("Minecraft EULA must be explicitly accepted before restarting")
	}
	_, err := s.invoke(ctx, inst, "restart")
	return err
}

func (s *Systemd) Command(ctx context.Context, inst model.Instance, command string) (string, error) {
	if s.platform != "linux" {
		return "", ErrUnsupported
	}
	if !model.ValidID(inst.ID) || inst.RCONPort < 1 || inst.RCONPort > 65535 {
		return "", errors.New("invalid RCON instance configuration")
	}
	if err := rcon.ValidateCommand(command); err != nil {
		return "", err
	}
	return s.console(ctx, "127.0.0.1:"+strconv.Itoa(inst.RCONPort), inst.RCONSecret, command)
}

func (s *Systemd) Status(ctx context.Context, inst model.Instance) (model.RuntimeStatus, error) {
	status := model.RuntimeStatus{State: "unknown", Players: []string{}}
	data, err := s.invoke(ctx, inst, "status")
	if err != nil {
		return status, err
	}
	status, start, err := parseStatus(data)
	if err != nil {
		return status, err
	}
	if status.PID > 0 && start > 0 {
		now, err := s.clock()
		if err != nil {
			return status, fmt.Errorf("read runtime monotonic clock: %w", err)
		}
		if now < start || math.IsNaN(now) || math.IsInf(now, 0) {
			return status, errors.New("invalid systemd process start time")
		}
		status.UptimeSeconds = now - start
	}
	if status.State != "running" {
		return status, nil
	}
	response, err := s.Command(ctx, inst, "list")
	if err != nil {
		if ctx.Err() != nil {
			return status, ctx.Err()
		}
		status.Detail = "Process is running; authenticated RCON readiness check failed"
		return status, nil
	}
	status.Players, err = parsePlayers(response)
	if err != nil {
		status.Players = []string{}
		status.Detail = "Process is running; RCON did not return a valid Minecraft player list"
		return status, nil
	}
	status.Ready = true
	return status, nil
}

func parseStatus(data []byte) (model.RuntimeStatus, float64, error) {
	status := model.RuntimeStatus{State: "unknown", Players: []string{}}
	fields := make(map[string]string)
	if len(data) > 64<<10 || !utf8.Valid(data) {
		return status, 0, errors.New("invalid systemd status output")
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || key == "" {
			return status, 0, errors.New("malformed systemd status output")
		}
		if _, duplicate := fields[key]; duplicate {
			return status, 0, errors.New("duplicate systemd status field")
		}
		fields[key] = value
	}
	for _, key := range []string{"ActiveState", "SubState", "MainPID", "ExecMainStartTimestampMonotonic", "MemoryCurrent", "CPUUsageNSec"} {
		if _, exists := fields[key]; !exists {
			return status, 0, fmt.Errorf("missing systemd status field %s", key)
		}
	}
	pid, err := strconv.ParseUint(fields["MainPID"], 10, 31)
	if err != nil {
		return status, 0, errors.New("invalid systemd PID")
	}
	start, err := strconv.ParseUint(fields["ExecMainStartTimestampMonotonic"], 10, 64)
	if err != nil {
		return status, 0, errors.New("invalid systemd process start timestamp")
	}
	metrics := make([]uint64, 2)
	for i, key := range []string{"MemoryCurrent", "CPUUsageNSec"} {
		if fields[key] == "[not set]" || fields[key] == "18446744073709551615" {
			continue
		}
		metrics[i], err = strconv.ParseUint(fields[key], 10, 64)
		if err != nil {
			return status, 0, fmt.Errorf("invalid systemd %s accounting", key)
		}
	}
	status.PID = int(pid)
	status.MemoryBytes = metrics[0]
	status.CPUSeconds = float64(metrics[1]) / 1e9
	switch fields["ActiveState"] {
	case "active":
		status.State = "unhealthy"
		status.Detail = "Unit is active without a running server process"
		if fields["SubState"] == "running" && pid > 0 && start > 0 {
			status.State, status.Detail = "running", ""
		}
	case "inactive":
		status.State = "stopped"
	case "failed":
		status.State = "failed"
	case "activating", "reloading":
		status.State = "starting"
	case "deactivating":
		status.State = "stopping"
	default:
		status.Detail = "Unrecognized systemd unit state"
	}
	return status, float64(start) / 1e6, nil
}

var playerListPattern = regexp.MustCompile(`^There are ([0-9]+) of a max of ([0-9]+) players online:[ ]?(.*)$`)
var playerNamePattern = regexp.MustCompile(`^[A-Za-z0-9_]{1,16}$`)

func parsePlayers(response string) ([]string, error) {
	match := playerListPattern.FindStringSubmatch(strings.TrimSpace(response))
	if match == nil {
		return nil, errors.New("invalid Minecraft player list")
	}
	count, err := strconv.ParseUint(match[1], 10, 32)
	maximum, maxErr := strconv.ParseUint(match[2], 10, 32)
	if err != nil || maxErr != nil || maximum == 0 || count > maximum || count > 100000 {
		return nil, errors.New("invalid Minecraft player count")
	}
	players := []string{}
	if match[3] != "" {
		players = strings.Split(match[3], ", ")
	}
	if uint64(len(players)) != count {
		return nil, errors.New("Minecraft player count does not match names")
	}
	seen := make(map[string]bool, len(players))
	for _, player := range players {
		if !playerNamePattern.MatchString(player) || seen[player] {
			return nil, errors.New("invalid or duplicate Minecraft player name")
		}
		seen[player] = true
	}
	return players, nil
}

func (s *Systemd) Logs(ctx context.Context, inst model.Instance, lines int) ([]model.LogLine, error) {
	if lines == 0 {
		lines = 200
	}
	lines = max(1, min(lines, 1000))
	data, err := s.invoke(ctx, inst, "logs", strconv.Itoa(lines))
	if err != nil {
		return nil, err
	}
	if len(data) > 4<<20 {
		return nil, errors.New("runtime logs exceed output limit")
	}
	logs := []model.LogLine{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		if len(logs) >= lines {
			return nil, errors.New("runtime helper exceeded requested log count")
		}
		var record struct {
			Timestamp string          `json:"__REALTIME_TIMESTAMP"`
			Message   json.RawMessage `json:"MESSAGE"`
		}
		if !utf8.Valid(scanner.Bytes()) {
			return nil, errors.New("invalid journald JSON encoding")
		}
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return nil, errors.New("invalid journald JSON record")
		}
		stamp, err := strconv.ParseInt(record.Timestamp, 10, 64)
		if err != nil || stamp <= 0 || len(record.Message) == 0 {
			return nil, errors.New("invalid journald timestamp or message")
		}
		timestamp := time.UnixMicro(stamp).UTC()
		if timestamp.Year() > 9999 {
			return nil, errors.New("journald timestamp exceeds RFC3339 range")
		}
		var message string
		if bytes.Equal(record.Message, []byte("null")) {
			return nil, errors.New("invalid journald message")
		}
		if err := json.Unmarshal(record.Message, &message); err != nil {
			var binaryMessage []uint8
			// []uint8's JSON decoder expects base64 strings, not journald arrays.
			var values []int
			if err := json.Unmarshal(record.Message, &values); err != nil || values == nil {
				return nil, errors.New("invalid journald message")
			}
			for _, value := range values {
				if value < 0 || value > 255 {
					return nil, errors.New("invalid journald binary message")
				}
				binaryMessage = append(binaryMessage, byte(value))
			}
			message = strings.ToValidUTF8(string(binaryMessage), "\uFFFD")
		}
		logs = append(logs, model.LogLine{Time: timestamp, Message: message})
	}
	if err := scanner.Err(); err != nil {
		return nil, errors.New("journald record exceeds size limit")
	}
	return logs, nil
}

type boundedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, errors.New("process output exceeds size limit")
	}
	return b.Buffer.Write(p)
}

func runBounded(ctx context.Context, path string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
	stdout, stderr := &boundedBuffer{limit: 4 << 20}, &boundedBuffer{limit: 16 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		// Neither helper output nor environment is safe to expose to HTTP/logs.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, exec.ErrWaitDelay) {
			return nil, errors.New("runtime helper did not close its output")
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, fmt.Errorf("helper exited with status %d", exit.ExitCode())
		}
		return nil, errors.New("runtime helper could not execute or exceeded output limits")
	}
	return stdout.Bytes(), nil
}
