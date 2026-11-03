# New Empire Server Control

A browser-based control panel for headless Minecraft Java/Fabric servers. Go serves a versioned API and an embedded React interface; SQLite keeps configuration, audit history, schedules, and durable jobs. Linux with systemd is the primary runtime.

**Status: evaluation-stage software.** Install only an authenticatable build listed in the [official GitHub Releases](https://github.com/gabrielpgava/newempire-server-control/releases). Release availability is separate from the [acceptance checks](docs/acceptance.md) required before use with valuable worlds; a public repository or published asset is not evidence of a tested deployment.

## What is in the project

- Token-protected first-owner setup, password sessions, TOTP, roles, and explicit instance grants.
- Separate instances with their own directories, ports, Java selections, runtime services, and maintenance jobs.
- Runtime status, readiness checks, logs, Minecraft console commands, and start/stop/restart jobs.
- Staged uploads and imports, bounded text editing, file recovery, and authenticated exports.
- Verified backups, restore previews, world selection, server properties, and player access controls.
- Fabric/Java metadata and Modrinth inventory, dependency plans, pins, and opt-in update policies.
- Schedules, metrics, notification configuration, and signed panel-update workflows.
- Locked offline backups/recovery and emergency owner access when the HTTP panel is unavailable.

These describe the source modules and API, **not a claim that every workflow has passed real-host acceptance**. See [known verification gaps](docs/acceptance.md#remaining-release-gates). Fabric mods are not Paper/Purpur plugins; there is no tested plugin provider or Docker server-runtime adapter.

## Run a local source build

Use the toolchain selected by `.go-version` and `go.mod`, and Node.js/npm compatible with `web/package.json`. Node.js is needed to build the interface, not to run the resulting native binary. Run from an integrated source checkout as an ordinary, non-root user:

```sh
npm --prefix web ci
npm --prefix web run build
go run ./cmd/control serve
```

Open **http://127.0.0.1:8080**. Building the frontend first is required: its `web/dist` output is embedded at Go compile time. Rebuild the Go binary after changing that output.

On first start, read the protected token locally, using the account that runs the panel:

```sh
cat data/setup-token
```

Use it in the setup screen to choose the first owner's username and password. Passwords must contain 12–1024 bytes. There is **no default administrator or password**, no unauthenticated administrator API, and no public registration after setup. The token is not returned by HTTP and is removed after successful setup.

The local defaults use `./data` for panel state, `./data/instances` for instances, and `./data/backups` for backups. This starts the panel; it does **not** install Java, provision a privileged helper, configure a firewall, or make Minecraft lifecycle controls work on macOS or Windows. Configure an explicit Java executable allowlist and the restricted systemd integration before creating or starting an instance.

Build a reusable binary and inspect the host configuration:

```sh
go build -o new-empire-control ./cmd/control
./new-empire-control version --json
./new-empire-control doctor
```

`doctor` reports configuration, database integrity when initialized, and whether the helper exists. For initialized state it requires the panel to be stopped and takes `panel.lock`; it never initializes an empty database or runs migrations. Helper existence is not proof that systemd permissions, firewall isolation, or Minecraft readiness work.

## Native Linux deployment

### Verified one-line installation

**Requires an actual published signed release:** if the official Releases page
has none, this command cannot install the application. Do not substitute an
unsigned local build. Select a published stable `VERSION` (without `v`) and set `JAVA_EXECUTABLE` to an
administrator-approved absolute Java executable path before running this line:

```sh
( : "${VERSION:?Set VERSION to a published stable version without v}"; : "${JAVA_EXECUTABLE:?Set JAVA_EXECUTABLE to an approved absolute Java path}"; sudo /bin/sh -c 'set -eu; PATH=/usr/sbin:/usr/bin:/sbin:/bin; export PATH; unset PYTHONHOME PYTHONPATH LD_PRELOAD LD_LIBRARY_PATH; umask 077; work=$(mktemp -d /root/new-empire-bootstrap.XXXXXXXX); trap "rm -f -- $work/bootstrap.py; rmdir -- $work" 0; ip=$(timeout 10 /usr/bin/python3 -I -c "import ipaddress,socket,sys; a=socket.getaddrinfo(\"raw.githubusercontent.com\",443,type=socket.SOCK_STREAM); a and all(ipaddress.ip_address(x[4][0]).is_global for x in a) or sys.exit(\"Non-public bootstrap destination\"); p=ipaddress.ip_address(a[0][4][0]); print(\"[\"+str(p)+\"]\" if p.version==6 else str(p))"); status=$(curl --disable --fail --silent --show-error --proto "=https" --tlsv1.2 --noproxy "*" --resolve "raw.githubusercontent.com:443:$ip" --connect-timeout 10 --max-time 30 --max-filesize 65536 --write-out "%{http_code}" https://raw.githubusercontent.com/gabrielpgava/newempire-server-control/main/deploy/bootstrap.py -o "$work/bootstrap.py"); [ "$status" = 200 ]; printf "%s  %s\n" 423d95deb5bde6041a127a8ef441cac61639ef5aae64b607e1f290c869e91c91 "$work/bootstrap.py" | sha256sum --check --status; /usr/bin/python3 -I "$work/bootstrap.py" --version "$1" --java "$2"' sh "$VERSION" "$JAVA_EXECUTABLE" )
```

Requires the [Linux/systemd prerequisites](docs/install/linux-systemd.md), curl,
`sha256sum`, `mktemp`, coreutils `timeout`, and root administration through sudo.
No Go or Node.js is needed on the destination. The command verifies the bootstrap's SHA-256 **before
execution**; that bootstrap verifies nine deployment files pinned to an immutable
official source revision, then uses the existing pinned-key signed-release
installer. The initial curl pins a validated public address, bypasses environment
proxies and requires HTTP 200; redirects are rejected. Copy the command/checksum
from reviewed official documentation.

Installation enables `new-empire-control.service` and reports the configured
dashboard URL, default **http://127.0.0.1:8080** / **http://localhost:8080**, and
detected private interface IPs. Those IPs are informational, **not remotely
reachable dashboard listeners**; the default remains loopback-only. Use an SSH
tunnel or a configured HTTPS reverse proxy for remote access. On an interactive
terminal it also shows the protected **first-owner setup code** for creating your
first login. Redirected output never reads or prints that code; it gives a local
read command instead. No Minecraft instance is started.

### Layout and exposure

Use the deployment files under `deploy/`; inspect the installation guide and installer before invoking them. The native layout uses `/etc/new-empire-control/config.json` for configuration, `/var/lib/new-empire-control` for panel state, `/opt/new-empire-control` for the application, and `/usr/local/libexec/new-empire-runtime` for the restricted helper. Confirm the final installed paths in your configuration and service units.

- Run the web process as the dedicated unprivileged panel account. `serve` refuses to run as root.
- Keep instance roots, backups, upload staging, and file recovery separate. Preserve `panel.db` **and** its matching `master.key` in protected panel recovery backups.
- Set `public_url` to the exact browser origin. Non-loopback access requires HTTPS; follow the [reverse-proxy guide](docs/install/reverse-proxy.md) and preserve the original `Origin` header.
- Never expose RCON publicly. A loopback RCON client alone does not restrict the Minecraft listener; the native helper/firewall must enforce isolation.
- The Compose deployment hosts the **panel only**. It does not supply a Docker runtime adapter or control host systemd services. Do not mount the Docker socket or give the container unrestricted host privileges as a workaround.

Proxmox LXC/systemd, firewall enforcement, real Minecraft operation, and a full recovery drill still require host-level verification. Do not treat a containerized unit test as that verification.

## CLI

The CLI calls the authenticated panel API for instance, backup, and ordinary update operations. Flags go **before** the subcommand. Use `--config FILE` or `NEC_CONFIG` to select the same configuration as the service. Only `NEC_LISTEN`, `NEC_PUBLIC_URL`, and `NEC_DATA_DIR` override the corresponding configuration values through the environment.

Sign in without placing a password in command-line arguments or shell history. In Bash:

```bash
read -r -s -p 'Panel password: ' PANEL_PASSWORD
printf '\n'
printf '%s\n' "$PANEL_PASSWORD" | ./new-empire-control login --username owner --password-stdin
unset PANEL_PASSWORD
./new-empire-control instance list
./new-empire-control instance inspect INSTANCE_ID
./new-empire-control instance start INSTANCE_ID
./new-empire-control backup create INSTANCE_ID
./new-empire-control backup list INSTANCE_ID
./new-empire-control backup verify INSTANCE_ID BACKUP_ID
```

The protected CLI session is stored in the configured data directory. TOTP accounts can add `--code CODE`, or `--code-stdin` to read a password line followed by a six-digit code line. For shells without `read -s -p`, supply a password-manager pipe to `--password-stdin` instead. Requests never follow redirects: use the exact configured origin.

Before restoring, inspect the authenticated backup preview in the browser or API. Only then run:

```sh
./new-empire-control backup --confirm restore INSTANCE_ID BACKUP_ID
```

Mutation commands returning a job do not wait for success. Follow that job in the panel or `GET /api/v1/jobs/{jobID}`; HTTP 202 means **accepted**, not completed. Do not retry destructive work merely because a browser or CLI connection was interrupted.

For offline operation, stop only the panel and run as its unprivileged service account with the same configuration. These commands require existing initialized state and matching encryption material; they hold `panel.lock`, never initialize/migrate state, execute only their selected job and leave Minecraft stopped:

```sh
./new-empire-control backup --offline create INSTANCE_ID
./new-empire-control backup --offline preview INSTANCE_ID BACKUP_ID
./new-empire-control backup --offline --confirm restore INSTANCE_ID BACKUP_ID
./new-empire-control recover list INSTANCE_ID
```

Minecraft must already be proven stopped for offline mutations; panel downtime does not prove that. The [disaster-recovery guide](docs/disaster-recovery.md) explains explicit retained-data recovery, pending-restore reconciliation, runtime recovery without world rollback, and confirmed owner credential recovery. Never delete a held lock or clear recovery guards by editing SQLite.

## API and documentation

The [OpenAPI 3.1 reference](docs/api/openapi.yaml) defines request and response shapes. API paths use `/api/v1`; `/healthz` is minimal public liveness. Authentication uses the `nec_session` HTTP-only cookie. Authenticated mutations require both a matching `Origin` and the `X-CSRF-Token` obtained from `/api/v1/auth/session`. Setup and login also require the matching origin, but not an existing session.

| Topic | Reference |
| --- | --- |
| Architecture and integration boundaries | [Architecture](docs/architecture.md), [wire contract](docs/integration-contract.md) |
| Security and credentials | [Security](docs/security.md) |
| Imports, files, and backups | [Server import](docs/server-import.md), [files](docs/files.md), [backups and restores](docs/backups-and-restores.md) |
| Runtime compatibility | [Fabric and Java](docs/fabric-and-java.md) |
| Routine administration | [Players](docs/players.md), [worlds](docs/worlds.md), [automation](docs/automation.md) |
| Verification and upstream assumptions | [Acceptance](docs/acceptance.md), [upstream references](docs/upstream-references.md) |
| Development and publishing | [Development](docs/development.md), [testing](docs/testing.md), [releases](docs/releases.md) |
| Change history | [Changelog](CHANGELOG.md) |

Actual implemented UI: [desktop](docs/evidence/ui/desktop-account.png) · [mobile](docs/evidence/ui/mobile-account.png). These are captures of a disposable empty panel served by the embedded Go binary, not mockups or production/Minecraft acceptance. See the [browser evidence and limits](docs/evidence/ui/README.md).

## Contributing and releases

Read [AGENTS.md](AGENTS.md) before contributing. Keep code, tests, English documentation, migrations, and OpenAPI synchronized. Use Conventional Commits. A significant feature requires a versioned release after trusted-branch checks pass; do not publish incomplete or untested code just to create a tag.

Run `make check` and `make security` locally. `make build` creates the host binary; `make dist VERSION=0.0.0-dev.1 OUT=dist/local-evaluation` creates unsigned Linux amd64/arm64 packages with embedded UI and an SBOM from clean committed source. These are evaluation candidates, not official installation/update assets. `make release-plan` previews the next version without publishing.

Automatic CI and releases remain disabled. The [manual release workflow](docs/releases.md) requires explicit owner authorization for each run, protected-main source, environment approval, signing and provenance; GitHub Actions must be disabled again when it finishes, including on failure.

The required release output includes Linux amd64/arm64 binaries with the embedded UI, machine-readable metadata, checksums, and authenticated signatures/provenance. Signing configuration, publication, self-update discovery, and a real-host rollback drill are release gates, not assumed successes. Until a release is actually published, use source builds for evaluation only.

## License

**Source-available under [PolyForm Shield 1.0.0](LICENSE).** This is not an OSI-approved open-source license. Read the full license before using, modifying, or distributing the application. Minecraft, Fabric, Modrinth, and other upstream projects retain their own licenses and terms; accepting Minecraft's EULA is a separate explicit step.
