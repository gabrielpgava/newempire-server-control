package runtime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/gabrielpgava/newempire-server-control/internal/model"
)

const activeStatus = "ActiveState=active\nSubState=running\nMainPID=1234\nExecMainStartTimestampMonotonic=10000000\nMemoryCurrent=2048\nCPUUsageNSec=3000000000\n"

func testInstance() model.Instance {
	return model.Instance{ID: "0123456789abcdef0123456789abcdef", EULA: true, RCONPort: 25575, RCONSecret: "not-logged-secret"}
}

func testSystemd() *Systemd {
	s := NewSystemd("/usr/libexec/newempire-runtime-helper")
	s.platform = "linux"
	s.clock = func() (float64, error) { return 30, nil }
	s.run = func(context.Context, string, ...string) ([]byte, error) { return []byte(activeStatus), nil }
	s.console = func(context.Context, string, string, string) (string, error) {
		return "There are 2 of a max of 20 players online: Alex, Steve", nil
	}
	return s
}

func TestReadinessRequiresProcessAndAuthenticatedValidPlayerList(t *testing.T) {
	for _, tc := range []struct {
		name, output, response, state string
		consoleError                  error
		ready                         bool
		calls                         int
	}{
		{"ready", activeStatus, "There are 2 of a max of 20 players online: Alex, Steve", "running", nil, true, 1},
		{"empty", activeStatus, "There are 0 of a max of 20 players online: ", "running", nil, true, 1},
		{"failed-rcon", activeStatus, "", "running", errors.New("secret must not leak"), false, 1},
		{"wrong-response", activeStatus, "Unknown command", "running", nil, false, 1},
		{"count-mismatch", activeStatus, "There are 2 of a max of 20 players online: Alex", "running", nil, false, 1},
		{"duplicate-player", activeStatus, "There are 2 of a max of 20 players online: Alex, Alex", "running", nil, false, 1},
		{"no-pid", strings.Replace(activeStatus, "MainPID=1234", "MainPID=0", 1), "", "unhealthy", nil, false, 0},
		{"not-running", strings.Replace(activeStatus, "SubState=running", "SubState=exited", 1), "", "unhealthy", nil, false, 0},
		{"activating", strings.Replace(activeStatus, "ActiveState=active", "ActiveState=activating", 1), "", "starting", nil, false, 0},
		{"deactivating", strings.Replace(activeStatus, "ActiveState=active", "ActiveState=deactivating", 1), "", "stopping", nil, false, 0},
		{"failed", strings.Replace(activeStatus, "ActiveState=active", "ActiveState=failed", 1), "", "failed", nil, false, 0},
		{"inactive", strings.Replace(activeStatus, "ActiveState=active", "ActiveState=inactive", 1), "", "stopped", nil, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testSystemd()
			s.run = func(_ context.Context, path string, args ...string) ([]byte, error) {
				if path != "/usr/bin/sudo" || !reflect.DeepEqual(args, []string{"-n", s.helper, "status", testInstance().ID}) {
					t.Fatalf("unsafe helper invocation: %s %v", path, args)
				}
				return []byte(tc.output), nil
			}
			calls := 0
			s.console = func(_ context.Context, address, password, command string) (string, error) {
				calls++
				if address != "127.0.0.1:25575" || password != testInstance().RCONSecret || command != "list" {
					t.Fatal("incorrect readiness command")
				}
				return tc.response, tc.consoleError
			}
			status, err := s.Status(t.Context(), testInstance())
			if err != nil {
				t.Fatal(err)
			}
			if status.State != tc.state || status.Ready != tc.ready || calls != tc.calls {
				t.Fatalf("got %+v, calls %d", status, calls)
			}
			if !tc.ready && status.State == "running" && status.Detail == "" {
				t.Fatal("missing readiness diagnostic")
			}
			if strings.Contains(status.Detail, "secret must not leak") {
				t.Fatal("readiness diagnostic leaked credentials")
			}
			if status.MemoryBytes != 2048 || status.CPUSeconds != 3 || (status.PID > 0 && status.UptimeSeconds != 20) {
				t.Fatalf("incorrect process accounting: %+v", status)
			}
		})
	}
}

func TestStatusRejectsMalformedAndUnavailableHelper(t *testing.T) {
	for _, data := range []string{"", "MainPID=123", activeStatus + "MainPID=2\n", strings.Replace(activeStatus, "MainPID=1234", "MainPID=-1", 1), strings.Replace(activeStatus, "MemoryCurrent=2048", "MemoryCurrent=nope", 1), strings.Replace(activeStatus, "CPUUsageNSec=3000000000", "CPUUsageNSec=NaN", 1), strings.Replace(activeStatus, "ExecMainStartTimestampMonotonic=10000000", "ExecMainStartTimestampMonotonic=99999999999999", 1)} {
		s := testSystemd()
		s.run = func(context.Context, string, ...string) ([]byte, error) { return []byte(data), nil }
		if status, err := s.Status(t.Context(), testInstance()); err == nil || status.Ready {
			t.Fatalf("accepted malformed status %q", data)
		}
	}
	s := testSystemd()
	s.run = func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("permission denied") }
	if status, err := s.Status(t.Context(), testInstance()); err == nil || status.State != "unknown" || status.Ready {
		t.Fatalf("unavailable helper masqueraded as status: %+v %v", status, err)
	}
	s = testSystemd()
	s.run = func(context.Context, string, ...string) ([]byte, error) {
		return []byte(strings.ReplaceAll(strings.ReplaceAll(activeStatus, "MemoryCurrent=2048", "MemoryCurrent=[not set]"), "CPUUsageNSec=3000000000", "CPUUsageNSec=18446744073709551615")), nil
	}
	if status, err := s.Status(t.Context(), testInstance()); err != nil || !status.Ready || status.MemoryBytes != 0 || status.CPUSeconds != 0 {
		t.Fatalf("unavailable accounting: %+v %v", status, err)
	}
}

func TestLifecycleContractAndValidation(t *testing.T) {
	s := testSystemd()
	var calls []string
	s.run = func(ctx context.Context, path string, args ...string) ([]byte, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("helper has no deadline")
		}
		if path != "/usr/bin/sudo" || len(args) != 4 || args[0] != "-n" || args[1] != s.helper || args[3] != testInstance().ID {
			t.Fatalf("unsafe invocation %s %v", path, args)
		}
		calls = append(calls, args[2])
		return nil, nil
	}
	inst := testInstance()
	if err := s.Start(t.Context(), inst); err != nil {
		t.Fatal(err)
	}
	if err := s.Restart(t.Context(), inst); err != nil {
		t.Fatal(err)
	}
	inst.EULA = false
	if err := s.Stop(t.Context(), inst); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(t.Context(), inst); err == nil {
		t.Fatal("started without EULA acceptance")
	}
	if err := s.Restart(t.Context(), inst); err == nil {
		t.Fatal("restarted without EULA acceptance")
	}
	inst.ID = "../../other-unit"
	if err := s.Stop(t.Context(), inst); err == nil {
		t.Fatal("accepted unsafe ID")
	}
	s.helper = "helper-from-PATH"
	if err := s.Stop(t.Context(), testInstance()); err == nil {
		t.Fatal("accepted relative helper")
	}
	if !reflect.DeepEqual(calls, []string{"start", "restart", "stop"}) {
		t.Fatal(calls)
	}
}

func TestNonLinuxRefusesRealRuntime(t *testing.T) {
	s := testSystemd()
	s.platform = "darwin"
	s.run = func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("invoked helper on unsupported platform")
		return nil, nil
	}
	if _, err := s.Status(t.Context(), testInstance()); !errors.Is(err, ErrUnsupported) {
		t.Fatal(err)
	}
	if err := s.Start(t.Context(), testInstance()); !errors.Is(err, ErrUnsupported) {
		t.Fatal(err)
	}
	if _, err := s.Command(t.Context(), testInstance(), "list"); !errors.Is(err, ErrUnsupported) {
		t.Fatal(err)
	}
	if _, err := s.Logs(t.Context(), testInstance(), 2); !errors.Is(err, ErrUnsupported) {
		t.Fatal(err)
	}
	if goruntime.GOOS != "linux" {
		if _, err := NewSystemd("/helper").Status(t.Context(), testInstance()); !errors.Is(err, ErrUnsupported) {
			t.Fatal(err)
		}
	}
}

func TestLogsAreBoundedJournaldJSON(t *testing.T) {
	s := testSystemd()
	for _, requested := range []int{-1, 0, 2, 1001} {
		s.run = func(_ context.Context, path string, args ...string) ([]byte, error) {
			want := map[int]string{-1: "1", 0: "200", 2: "2", 1001: "1000"}[requested]
			if len(args) != 5 || args[2] != "logs" || args[4] != want {
				t.Fatal(args)
			}
			return []byte("{\"__REALTIME_TIMESTAMP\":\"1700000000000000\",\"MESSAGE\":\"Server started\"}\n"), nil
		}
		logs, err := s.Logs(t.Context(), testInstance(), requested)
		if err != nil || len(logs) != 1 || logs[0].Message != "Server started" || logs[0].Time.Unix() != 1700000000 {
			t.Fatalf("%+v %v", logs, err)
		}
	}
	for _, data := range []string{"not json", `{}`, `{"__REALTIME_TIMESTAMP":"-1","MESSAGE":"x"}`, `{"__REALTIME_TIMESTAMP":"9223372036854775807","MESSAGE":"x"}`, `{"__REALTIME_TIMESTAMP":"1","MESSAGE":null}`, "{\"__REALTIME_TIMESTAMP\":\"1\",\"MESSAGE\":\"\xff\"}", `{"__REALTIME_TIMESTAMP":"1","MESSAGE":[256]}`, strings.Repeat(`{"__REALTIME_TIMESTAMP":"1","MESSAGE":"x"}`+"\n", 3), strings.Repeat("x", 4<<20+1)} {
		s.run = func(context.Context, string, ...string) ([]byte, error) { return []byte(data), nil }
		if _, err := s.Logs(t.Context(), testInstance(), 2); err == nil {
			t.Fatalf("accepted invalid logs %.100s", data)
		}
	}
	s.run = func(context.Context, string, ...string) ([]byte, error) {
		return []byte(`{"__REALTIME_TIMESTAMP":"1","MESSAGE":[65,255,66]}`), nil
	}
	if logs, err := s.Logs(t.Context(), testInstance(), 1); err != nil || logs[0].Message != "A\uFFFDB" {
		t.Fatalf("%+v %v", logs, err)
	}
}

func TestRunBoundedDoesNotExposeStderrAndLimitsOutput(t *testing.T) {
	var buffer boundedBuffer
	buffer.limit = 3
	if _, err := buffer.Write([]byte("okay")); err == nil || buffer.Len() != 0 {
		t.Fatal("output limit failed")
	}
	data, err := runBounded(t.Context(), os.Args[0], "-test.run=TestRuntimeHelperProcess", "--", "okay")
	if err != nil || !bytes.Equal(data, []byte("helper-output")) {
		t.Fatalf("%q %v", data, err)
	}
	_, err = runBounded(t.Context(), os.Args[0], "-test.run=TestRuntimeHelperProcess", "--", "failure")
	if err == nil || strings.Contains(err.Error(), "secret-on-stderr") {
		t.Fatal(err)
	}
}

func TestRuntimeHelperProcess(t *testing.T) {
	for i, argument := range os.Args {
		if argument != "--" || i+1 >= len(os.Args) {
			continue
		}
		if os.Args[i+1] == "failure" {
			_, _ = os.Stderr.WriteString("secret-on-stderr")
			os.Exit(5)
		}
		_, _ = os.Stdout.WriteString("helper-output")
		os.Exit(0)
	}
}
