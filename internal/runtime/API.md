# Runtime integration contract

`NewSystemd(helper string) *Systemd` implements `ServerRuntime`. The helper must
be an absolute executable path supplied by trusted panel configuration. No shell
is used. Non-Linux hosts return `ErrUnsupported` without invoking a process.

Every invocation is exactly:

```text
sudo -n <helper> <start|stop|restart|status|logs> <32-lowercase-hex-instance-ID> [lines]
```

`lines` occurs only for `logs` and is clamped to 1–1000 (default 200).
The privileged helper must independently validate arguments, derive the unit
from the ID (never accept a unit/path from the caller), and reject extra args.
Successful start/stop/restart commands produce no required output. The helper
must use a graceful systemd stop and must not force-kill a saving server.
Before start/restart, the helper must enforce root-owned ingress filtering for
that instance's configured RCON port: only loopback (or an explicitly secured
internal network) may reach it. Vanilla may bind RCON on all interfaces when
`server-ip` is blank. Loopback-only client dialing is not sufficient isolation.

`status` returns raw `systemctl show` output, one `key=value` per line, for:

```text
ActiveState=active
SubState=running
MainPID=1234
ExecMainStartTimestampMonotonic=123456789
MemoryCurrent=536870912
CPUUsageNSec=1000000000
```

All six keys must be present. `MemoryCurrent`/`CPUUsageNSec` may be
`[not set]` or `18446744073709551615` when systemd accounting is unavailable.
Other values are validated. `ExecMainStartTimestampMonotonic` is microseconds
since boot; the adapter reads Linux `CLOCK_MONOTONIC` for elapsed time. An absent unit or
failed command must exit nonzero, not masquerade as an inactive server.

`logs` returns at most the requested number of newline-delimited journald JSON
records (`journalctl --unit=<derived-unit> --lines=<lines> --output=json
--no-pager --quiet`). Each record has `__REALTIME_TIMESTAMP` (Unix microseconds,
string) and `MESSAGE` (string, or journald's integer byte array). Output is
bounded to 4 MiB; an oversized/malformed response is an error, not truncated
success. Diagnostics never include untrusted helper output.

`Status` reports `running` only for an active/running unit with a positive PID;
`Ready` additionally requires successful authenticated loopback RCON `list` and
a valid Minecraft player-list response. An existing JVM is not readiness.
RCON always connects to numeric `127.0.0.1:<instance.RCONPort>`, never a caller
host. `Command` is Minecraft console input, not a shell; multiline/NUL input is
rejected and commands are limited to 1446 UTF-8 bytes. Start/restart require
`instance.EULA`; the adapter never writes EULA.

The caller must hold the durable per-instance job lock for mutations and enforce
roles, audit, backups, and stop-before-file-apply. Lifecycle calls return when
the helper completes; callers should poll `Status` for readiness. `Stop` does
not require a healthy RCON connection.
