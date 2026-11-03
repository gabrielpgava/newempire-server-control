# Native Linux / systemd installation

This is the intended native deployment. The signed-release installer and complete
Minecraft/updater integration have **not yet passed real-host acceptance**.
[Limited development-fixture service/schema checks](../evidence/linux/README.md)
passed on a disposable ARM64 VM; they do not certify this installation workflow,
a distribution or production readiness. The deployed Go binary embeds the
frontend and does not require Node.js.

## Prerequisites and trust

- Linux amd64/arm64, kernel 5.6+ (openat2), systemd 247+ and journald `/dev/log`.
- An ACL-capable filesystem (for example appropriately configured ext4/XFS),
  nftables with permission to install inet input chains, sudo 1.9.10+,
  shadow account tools, Python 3.9+, and OpenSSL with Ed25519 `pkeyutl -rawin`.
- A compatible, administrator-installed Java selected using current upstream
  Minecraft/Fabric metadata, not a guessed Java version.
- A root administrator for initial setup. The HTTP panel always runs as the
  non-login `new-empire-control` account, never root.
- For the one-line bootstrap: curl, `sha256sum`, `mktemp`, `timeout`, sudo access, and a
  root-owned `/root` directory for private staging.

Download/review the deployment asset from the official
`gabrielpgava/newempire-server-control` release using its authenticated provenance.
Extract it under a root-owned directory such as `/root/new-empire-deploy`.
Do not execute `curl | sh`. A deployment script is executable code: verifying the
later native archive does not authenticate a maliciously replaced installer.
Do not use an installer/key from a fork or an unauthenticated third-party host.

The embedded Ed25519 raw-public-key SHA-256 fingerprint is:

```text
dd6804fd279cc3935f9dc3669dc900d81f7e1d0a1382b67f007568a615e5bd7f
```

`deploy/release-public-key.pem` and `deploy/release-public-key.txt` contain that
public key. The installer pins the same key/fingerprint; a replacement key is
rejected even if its manifest signature is internally valid. No private key
belongs on the Minecraft host. Trust keys are never fetched beside the assets.

## Verified one-line bootstrap

The [README command](../../README.md#verified-one-line-installation) downloads a
small bootstrap, verifies the documented SHA-256, then runs it as root. It does
not execute an unauthenticated `curl | sh` response. It requires the same explicit
stable `VERSION` and approved `JAVA_EXECUTABLE` as the installer below. **No
official signed release is published yet; release installation remains blocked.**

The initial command bounds DNS lookup to ten seconds, refuses empty or non-public
results, pins a validated address for curl, disables proxy selection, and requires
HTTP 200 within thirty seconds before checking the bootstrap checksum. A redirect
response is refused even if its body happens to match the expected checksum.

The bootstrap downloads exactly nine deployment files from the official
repository at immutable commit `0bc59f9cb537982048c4d1cbef355ccf982d072c`.
Each file must match its embedded SHA-256 before any installer code runs. Downloads
use normal TLS certificate/hostname verification, reject non-public DNS results
and redirects, and are bounded to 64 KiB per file. A separate worker bounds the
complete transfer, including DNS, TLS, headers and chunk framing, to thirty seconds
plus at most two seconds of worker cleanup; socket timeouts are ten seconds.
No proxy or alternative origin is selected from environment variables. Private
staging is removed on normal completion/failure.
The existing native installer still independently verifies the release manifest,
production key, archive hash and architecture; the bootstrap is not a release
publisher or an unsigned-install fallback.

Maintainers: after changing any pinned deployment file, commit/review that change
first, then update `SOURCE_COMMIT` and all nine hashes in `deploy/bootstrap.py`.
Update the README bootstrap checksum after every bootstrap change. Run the
deployment tests and local gates before pushing; the tests check the README
checksum and pinned file contents. Do not silently rotate the production key.

## Install an explicit stable release

From the reviewed, root-owned deployment directory:

```sh
sudo sh ./install.sh --version "$VERSION" --java "$JAVA_EXECUTABLE"
```

`VERSION` is the selected stable SemVer **without** `v`; `JAVA_EXECUTABLE` is the
administrator-approved path. Repeat `--java` for additional approved runtimes.
Java symlinks supplied here are resolved to their canonical root-owned targets;
the helper subsequently requires those exact paths. No current versions are
hard-coded or downloaded for Java/Minecraft/Fabric.

The installer downloads from the fixed official release URL, follows only
allowlisted public HTTPS GitHub/CDN destinations and bounds inputs. Alternatively:

```sh
sudo sh ./install.sh --version "$VERSION" --java "$JAVA_EXECUTABLE" \
  --archive "/root/releases/new-empire-control_${VERSION}_linux_${ARCH}.tar.gz" \
  --manifest /root/releases/release.json \
  --signature /root/releases/release.json.sig
```

Both modes use OpenSSL to verify the canonical `release.json` bytes with the
pinned Ed25519 key and padded-base64 detached signature, then enforce official
repository/version/schema, archive SHA-256/size and ELF64 architecture. Native
archives must contain exactly one ordinary `new-empire-control` file; links,
extra entries, malformed headers and excessive expansion fail. Install/deploy
docs are shipped separately. Local inputs must be regular files, not symlinks.

Re-running the same release is idempotent. Existing custom configs are preserved;
different installed executables, wrappers or units are not silently overwritten.
The installer refuses to switch an existing installation to a different version:
use the verified updater and a backup/maintenance plan instead. Failures may
leave stopped host setup artifacts for an explicit retry; no instance contents
are deleted, imported or restored by the installer.

## Installed layout

| Path | Ownership / purpose |
| --- | --- |
| `/opt/new-empire-control/versions/<version>/new-empire-control` | root-owned immutable release binary |
| `/opt/new-empire-control/current` | root-controlled relative version symlink |
| `/opt/new-empire-control/control` | root-owned fixed wrapper for the selected binary |
| `/opt/new-empire-control/updates` | root:root 0700, private updater journal, staging, lock and recovery snapshots |
| `/etc/new-empire-control/config.json` | root:panel 0640, panel application settings |
| `/etc/new-empire-control/runtime.json` | root 0600, privileged path/Java/resource policy |
| `/etc/new-empire-control/instances/<id>.json` | root 0600, validated registration, no RCON secret |
| `/var/lib/new-empire-control` | panel-owned 0700, DB, credentials, uploads, external backups/recovery |
| `/var/lib/new-empire-control/panel-update-report.json` | root-owned 0644 bounded operation result, readable by the panel without private journal access |
| `/srv/new-empire/instances` | root-owned parent, fixed per-ID managed directories |
| `/usr/local/libexec/new-empire-runtime` | root-owned restricted helper wrapper |

Initial HTTP binding is `127.0.0.1:8080`. Read the bootstrap token only from the
protected panel state directory. Use an SSH tunnel for initial setup or configure
an HTTPS reverse proxy and matching `public_url`/secure cookies before remote
access. Do not expose the loopback development origin or RCON publicly.

After enabling `new-empire-control.service`, the installer prints the configured
dashboard URL, the default localhost address, and detected RFC1918/IPv6 ULA
interface addresses. Those interface addresses are **not dashboard listeners**:
the installer does not change the loopback binding, existing custom `public_url`,
or firewall policy. Missing address discovery is non-fatal.

On an interactive terminal it waits up to ten seconds for the existing protected
`setup-token` and shows the **first-owner setup code**. Enter that code in the
dashboard to create your first login; there is no default username/password.
Keep the code private and do not record/share that terminal. The file must be an
ordinary single-link, panel-owned mode-0600 file with the expected token format;
links, devices, unsafe permissions and malformed/oversized contents are refused.

Redirected/non-interactive output never opens or prints the code. Instead it
shows a local administrator read command, normally:

```sh
sudo cat -- /var/lib/new-empire-control/setup-token
```

An unavailable code can mean startup is pending or first-owner setup is complete;
it is not proof of either. Inspect service status and protected file permissions
locally if necessary. Installation never regenerates the token or retrieves it
over HTTP. Existing custom data-directory settings are resolved relative to the
service's working directory, `/var/lib/new-empire-control`, not the administrator's
shell. An omitted/null `data_dir` uses its `data` child, matching the application;
an empty value uses the working directory itself. Absolute paths are preserved.
Nested panel-owned directories are supported for reporting, but every component
must be a real directory, owned by root or the panel and not group/world writable.
The stricter root-only ancestor rules for installation files remain unchanged.

Existing panel-owned `updates/` directories are refused. Do not recursively chown
them into trust: preserve and inspect legacy state offline, then provision a
fresh root-owned private directory. The panel never needs direct staging/journal
write permission. Reports are explicitly chmodded independently of the updater
umask; snapshot credentials remain root-private. See [self-updates](../self-updates.md)
for staged-release reuse, failed-version retry latches and job reconciliation.

## Managed create/import and permissions

The panel's durable creation job calls sudo `provision` with one validated
`model.Instance` JSON document **before** populating its managed directory. The
root-owned policy must permit its exact Java path; the managed root is always
`managed_root/<32-lowercase-hex-ID>`. The helper creates the root manifest/unit
and a dedicated Minecraft account. Full IDs identify units; portable account
names use `nec-mc-<first 24 hex>` and collisions fail closed. The HTTP process is
never granted general sudo, arbitrary systemctl, a shell, or Docker socket access.

Managed preparation normalizes entries to the panel UID, assigns the dedicated instance group and
sets directories `02770` and regular files `0660`. Named access/default ACLs
give the panel live access without restarting it for newly assigned groups.
Only panel/this-instance-owned entries are accepted. Symlinks, hardlinks,
special files, mount crossings, root-owned entries and excessive trees fail.
Custom managed executable bits/ACLs are intentionally not preserved; Java runs
an ordinary `.jar`, not uploaded launcher scripts. No world contents are changed.
The root policy bounds heap size. Each unit's cgroup memory ceiling is its heap
plus 1024 MiB native/JVM allowance, with swap disabled; size/test actual modpacks
before live use because exceeding the ceiling can terminate Java.

The root-only unit guard repeats preparation before each actual JVM start, so
panel writes/atomic replacements with `0600`/`0700` are repaired safely offline.
Successful helper stop also proves the unit and all dedicated-UID processes have
exited, then normalizes Minecraft-created files for ownership-preserving atomic
editing. It never repairs permissions while a saving Minecraft process remains.
After restore/root replacement, the panel may call restricted `prepare <ID>`
while the unit is proven stopped. Reconfiguration of Java/launcher/memory uses
restricted `reconfigure <ID>` and cannot change identity/root/ports. Do not edit
manifests through the panel or expose independent root CLI functions over HTTP.

## Adopt an external directory

Adoption never recursively chowns, chmods, or changes ACLs. Registering an
unprepared external tree fails with preparation guidance. The root administrator
must first stop its old service, make/verify a backup and explicitly prepare it.

1. Add a narrow root-owned `adopt_roots` entry to `runtime.json` and the matching
   panel config. Keep every ancestor of the final data directory root-owned and
   not group/world writable; do not use `/home`, system directories, symlinks or
   a broad allowroot. The final instance must be a strict subdirectory.
2. Add a root-owned systemd panel drop-in with an explicit
   `ReadWritePaths=/srv/adopted/<directory>` for that permitted tree and reload/
   restart **only the panel**. The default sandbox does not permit external writes.
3. Reserve the instance ID, prepare/create its dedicated account/group using a
   root-reviewed account plan, then grant only that user and the panel the needed
   tree access. Named per-user access/default ACLs are preferable when preserving
   external ownership. Do not make data world-writable or add Minecraft to the
   panel group. If relying on new panel supplementary group membership rather
   than named ACLs, restart only the panel to refresh its process groups.
4. Ensure all directories are traversable/readable/writable and all regular
   files readable/writable by both identities. Remove/review unsafe links and
   special files separately; the helper will not silently rewrite them. Verify
   `server.properties` and EULA/launcher/runtime compatibility before adoption.

Adoption of a directory owned by some other external service account does not
make the panel impersonate that account. A root administrator may use
`control install-service < /root/validated-instance.json` as an emergency
registration path. This command consumes the same bounded model schema and
policy, accepts no arbitrary commands, and still does not mutate adopted data.
Use an independent root login/shell for this emergency CLI; inherited non-root
`SUDO_UID` is intentionally rejected (an authorized root administrator can clear
sudo-origin environment variables in that root shell). Routine wizard jobs use
the restricted sudo `provision` path instead.
Keep the instance stopped until preparation succeeds.
For adopted files, an atomic editor must preserve the original UID/GID or fail
without changing the file; it does not silently take ownership through root.

## RCON and lifecycle

Minecraft cannot reliably bind RCON independently from the game listener.
Connecting the panel to `127.0.0.1` alone is not protection. Every instance start
must install/verify a root-owned nftables inet input hook dropping its registered
RCON port on non-loopback interfaces (IPv4 and IPv6); the game port remains
separate. The guard checks explicit matching game/RCON ports, enabled RCON and
a nonempty password in `server.properties`. Escaped keys/continuations/duplicates
or escaped security values fail closed. Do not flush nftables while servers run;
re-establish filtering before restarting an instance. Review other host firewall
rules without removing this guard.

Stops use parent RCON when available, then systemd SIGTERM with 120 seconds and
no forced SIGKILL. A stalled saver needs operator intervention. Start/stop/restart
affect only the selected instance unit; application updates affect only the panel.
See [operations](../operations.md) and [troubleshooting](../troubleshooting.md).
