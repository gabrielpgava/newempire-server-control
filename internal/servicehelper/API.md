# Native helper / CLI integration contract

The integration owner wires these functions into `cmd/control`:

```go
// control runtime-helper <operation> [arguments...]
servicehelper.Run(ctx, args, os.Stdin, os.Stdout) error

// control install-service (no arguments; one model.Instance JSON on stdin)
servicehelper.InstallService(ctx, os.Stdin) error

// Before opening any HTTP listener, including development configurations.
servicehelper.RefuseRootHTTP() error
```

`Run` requires Linux and real/effective UID 0. Sudo's `SUDO_UID`/`SUDO_USER`
must match the configured panel user; direct root administration is also allowed.
No production path or executable can be overridden through environment flags.
The root-owned wrapper `/usr/local/libexec/new-empire-runtime` executes the same
Go binary through `/opt/new-empire-control/control runtime-helper "$@"`.

## Panel-permitted sudo commands

```text
provision                         # one model.Instance JSON on stdin
reconfigure <32-lowercase-hex-ID>   # one model.Instance JSON on stdin
start|stop|restart|status|prepare <ID>
logs <ID> [1..1000 lines, default 200]
panel-update
```

No other arguments are accepted. JSON is limited to 64 KiB, unknown/duplicate
fields and trailing objects are rejected, and RCON credentials are never
serialized into a manifest. `InstallService` is the independent root-only
emergency equivalent of `provision`; a non-root `SUDO_UID` is refused there.

`provision` validates against root-owned `/etc/new-empire-control/runtime.json`
(`panel_user`, `managed_root`, `adopt_roots`, `java_paths`, `max_memory_mb`).
The config, approved executables and all privileged path ancestors must be
root-owned, canonical, without symlinks or group/world write permission. Java
paths are exact executable allowlist entries, not commands. The JSON must include
valid ID/name, explicit Minecraft/Fabric versions, creation time, EULA acceptance,
a safe relative `.jar` launcher, bounded memory and distinct ports 1024–65535.
`max_memory_mb` caps Java heap; the unit's cgroup MemoryMax additionally permits
1024 MiB native/JVM overhead and disables swap. Excess native usage can terminate
the JVM; compatibility/resource sizing still needs instance preflight and testing.
Registered instances may not overlap data roots or game/RCON ports.

Managed roots are exactly `<managed_root>/<ID>`; the parent is root-owned.
Provision BEFORE the create/import workflow populates this directory: the
helper creates it panel-owned with the dedicated instance group, creates the
root-owned manifest `/etc/new-empire-control/instances/<ID>.json`, writes the
fixed-argument Java unit, and completes `systemctl daemon-reload`. A zero exit
is the ready artifact, not a queued registration. No service is started/enabled.
Parent holds the durable instance job lock and only saves successful creation
after its remaining population/verification steps. A failed wizard may leave a
stopped registration for explicit recovery; no automatic destructive cleanup.

The unit uses the full ID: `nec-mc-<32 hex>.service`. Portable Linux account names
have a 31-character limit: the dedicated user/group is `nec-mc-<first 24 hex>`.
Prefix collisions fail rather than share users. The Minecraft user has no panel
group/supplementary groups. Provision adds the panel to the instance group.

`reconfigure <ID>` requires a registered unit with ActiveState inactive/failed
and MainPID 0. It cannot change ID, root, ports, adoption mode or creation time;
Java, launcher, memory and descriptive/version fields are revalidated against
the same root policy. The account/service identity remains fixed. Hold the
durable job lock and finish required backups/staging/graceful stop first. A
crash between the two root-file replacements fails closed on a unit/manifest
mismatch; see operations for explicit offline recovery. No world rollback is
performed by the helper.

## Ownership / permission safety

Managed preparation normalizes entries to the configured panel UID and registered
instance GID, and sets directories 02770/files 0660. Only panel-owned or this
instance-user-owned entries are accepted; the top directory must be panel-owned.
Descriptor traversal uses Linux openat2, refusing symlinks, regular-file
hardlinks, special files, mount crossings, root/unexpected owners, depths over
128 and more than one million entries. No backup, updater or arbitrary supplied
path is traversed. Named access/default POSIX ACLs grant the panel read/write
access to Minecraft-created files without restarting the panel for a new group.
This requires an ACL-capable filesystem; errors fail closed. Custom managed
permissions/ACLs are replaced by this policy. Data contents are not changed.

`prepare <ID>` is restricted to an already-registered, inactive/failed instance
with MainPID 0 and no processes still owned by the instance UID. After restore/root replacement it revalidates the managed tree
and refreshes only the manifest directory identity. The unit's root-only
`protect <ID>` ExecStartPre repeats preparation on every actual JVM start,
after systemd has stopped the previous JVM. Panel sudo cannot call `protect`.
Successful helper `stop` also repeats normalization after proving the unit and
all instance-UID processes stopped. Thus Minecraft-created managed files become
panel-owned before offline atomic editing; the editor need not silently drop
ownership or invoke an arbitrary privileged write.
Panel atomic replacements may remain 0600/0700 until preparation. A race/error
can leave partially repaired metadata; the unit remains stopped and an offline
retry is required.

Adoption NEVER changes existing ownership, modes or ACLs. It requires a strict
subdirectory of an adoption allowroot, trusted root-owned path ancestors, and a
tree already readable/writable by both accounts. Prepare external permissions
explicitly; use named panel ACLs or refresh panel supplementary groups. A changed
adopted root identity requires administrator review. The helper does not adopt
an arbitrary existing service user or grant it the panel group.
Adopted access checks interpret Linux POSIX ACL user/group entries and masks,
not just the mode's group-mask bits. Access ACLs over 4 KiB fail closed.
An atomic editor that cannot preserve the original adopted UID/GID must reject
the edit untouched; adoption is not a promise of unrestricted external editing.

## Start boundary / process contract

Before start/restart, and again in ExecStartPre, an nftables inet input hook
drops the registered RCON TCP port on non-loopback interfaces. Rule installation
and exact JSON verification are mandatory; no nft access means no JVM start.
The guard also requires explicit `server-port`, `rcon.port`, `enable-rcon=true`
and a nonempty `rcon.password` in a bounded, unlinked `server.properties`.
Ports must equal the root manifest. Escaped keys, continuations, duplicate keys
and escaped security values fail closed; other ordinary properties are retained.
The helper never prints/stores that password. All Java arguments are fixed.

`stop` uses SIGTERM with 120 seconds and `SendSIGKILL=no`. A stalled saving JVM
requires an operator; no forced kill. Parent should try authenticated RCON stop
first. `status` returns raw `systemctl show` for the six runtime adapter keys:
ActiveState, SubState, MainPID, MemoryCurrent, CPUUsageNSec,
ExecMainStartTimestampMonotonic. Missing units fail. `logs` returns bounded
journald JSON (1–1000 records, at most 4 MiB); diagnostics omit process output.
Commands use fixed absolute binaries and argv, never an interpolated shell.
Accepted privileged actions emit authpriv syslog; unavailable audit logging
fails closed.

`panel-update` only starts `new-empire-update.service` asynchronously. That
independent root-owned oneshot executes
`control update-helper --config /etc/new-empire-control/config.json`. Parent must
wire that CLI to independently verified updater apply/recovery, panel-only
systemctl actions and authenticated readiness. It cannot take a caller command.
The panel unit uses `control serve --config ...`; update these fixed CLI names
with the integration owner if needed.

The installer follows the signed updater archive/manifest contract, creates
`versions/<version>/new-empire-control` and `current -> versions/<version>`, and
pins the actual release public key in both PEM and base64 forms. Deployment
files are a separate reviewed deployment asset, never entries in native
archives. Linux/systemd/nftables live integration is still unverified.
