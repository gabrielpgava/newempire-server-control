# Changelog

All entries are in English. Published releases will use Semantic Versioning; an entry under Unreleased is not a publication claim.

## Unreleased

### Added

- Go modular-monolith foundation with SQLite migrations, durable jobs, audit history, and a versioned HTTP API.
- Protected first-owner bootstrap, password sessions, CSRF/origin validation, roles, instance grants, and TOTP enrollment.
- Protected self-service password changes and TOTP disablement with all-session revocation, plus owner-only non-owner account revocation.
- Native systemd runtime integration, Java/Fabric metadata and compatibility planning, logs, readiness, and Minecraft console operations.
- Confined file operations, resumable staged uploads, import previews, recoverable deletion, verified backup archives, and explicit restore workflows.
- Modrinth hash inventory, dependency-aware content plans, managed-content policies, pins, and modpack domain support.
- Player moderation, world selection, safe property editing, schedules, metrics, and notification/offsite-storage integration modules.
- Embedded React/TypeScript interface, administrative CLI, deployment assets, and signed panel-release/update infrastructure.
- Locked offline backup, retained-data recovery and emergency owner access without a running HTTP panel; existing-state-only loading and targeted durable jobs prevent accidental bootstrap or unrelated queued execution.
- English public-product guides, OpenAPI 3.1 schemas, upstream references, and an acceptance-evidence matrix.
- Native installer access summary with the configured dashboard URL, informational private interface addresses, service-resolved data paths, and protected first-owner code display only on an interactive terminal; redirected output never reads the code.
- Short `curl | bash` Linux installer with latest-stable release selection and trusted installed-Java discovery. The initial GitHub script is explicitly trusted; its subsequent bootstrap and nine immutable-source deployment files are checksum-verified before signed-release/systemd installation.
- `make dist VERSION=... OUT=...` for unsigned embedded-UI Linux amd64/arm64 packages and a source-bound SBOM, plus read-only `make release-plan`.
- Entirely local release signing/publication with protected-main identity checks, deterministic signed source-bound provenance covering all payloads/checksums, and draft asset read-back before publication. No workflow or cloud runner is used.

### Fixed

- Validate portable backup instance metadata before stopping Minecraft or replacing world data; restore archived service memory settings as well as Java/launcher choices.
- Keep interrupted full-tree restores blocked until explicitly selected data and runtime metadata are reconciled against a verified snapshot; never automatically replace a world during reconciliation.
- Verify retained runtime recovery against the original snapshot, preserve displaced data, and latch world-format review before metadata replacement.
- Normalize reviewed imported server roots and preserve adoption identities across permission-preparation retries, with static launcher identity and authoritative Java preflight.
- Keep updater state root-private, publish umask-safe operation reports, preserve panel-lock ownership, reuse staged releases, and reconcile only final interrupted operations.
- Preserve local editor drafts through failed refreshes and require confirmation across navigation, instance changes, sign-out and supported browser unloads.
- Preserve notification deadline errors when a socket timeout precedes context-timer publication, without exposing SMTP diagnostics or changing active-context retry rules.
- Preserve panel-update postponements during ordinary/concurrent policy edits, and display refreshed server deadlines instead of stale action responses.
- Use pinned Go 1.27-compatible vulnerability tooling without disabling symbol/callgraph analysis after the initial Linux CI scanner crash.
- Validate actual SQLite schema on protected stopped-panel copies, including committed WAL, before updater snapshot/restore/restart; refuse initialization or migrations and keep unsafe state stopped for manual recovery.
- Block conflicting edits while content readiness/recovery is pending, reject duplicate mod adoption, and preserve unmanaged mods during automatic-safe maintenance.
- Redact known RCON credentials from console/player job output and errors.
- Include older conventional commit subjects in release planning even when the newest commit is documentation or a smaller repair.
- Consolidate TypeScript 7 and React plugin 6 updates with Vite client declarations and a compatible Vite 8 toolchain, without bypassing peer dependency checks.
- Synchronize the owner's explicitly selected initial Ed25519 trust root across the CLI, installer and release tooling. Unreleased old-key builds require a reviewed manual migration/reinstallation; automatic cross-key trust is not supported.

### Removed

- Automatic GitHub Actions CI/release triggers and their Dependabot update entry to avoid runner costs. Local validation/security commands and signed-update verification remain; only explicitly authorized manual publication is available.

### Project policy

- PolyForm Shield 1.0.0 source-available licensing; not described as OSI-approved open source.
- Linux/systemd is the primary server runtime. Compose is panel-only; no tested Docker or Paper/Purpur runtime provider is claimed.
- Significant completed features require a checked, versioned GitHub Release. No release version, publication, real-world deployment, or production-readiness is asserted in this entry.

### Verification still required

- Maintainer-reviewed local checks, Linux/systemd/firewall and real Minecraft acceptance, Proxmox LXC testing, provider delivery tests, and disaster-recovery drills. Local host/Linux checks and ephemeral-key package smokes are recorded in the acceptance ledger, not claimed as live deployment proof.
- Confirm release signing and trusted publication, both native architectures, updater discovery, panel-only restart, and post-update rollback on a real host. See [acceptance](docs/acceptance.md) for the evidence boundary.
