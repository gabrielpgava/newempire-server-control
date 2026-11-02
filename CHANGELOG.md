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
- English public-product guides, OpenAPI 3.1 schemas, upstream references, and an acceptance-evidence matrix.

### Fixed

- Validate portable backup instance metadata before stopping Minecraft or replacing world data; restore archived service memory settings as well as Java/launcher choices.
- Block conflicting edits while content readiness/recovery is pending, reject duplicate mod adoption, and preserve unmanaged mods during automatic-safe maintenance.
- Redact known RCON credentials from console/player job output and errors.
- Include older conventional commit subjects in release planning even when the newest commit is documentation or a smaller repair.

### Project policy

- PolyForm Shield 1.0.0 source-available licensing; not described as OSI-approved open source.
- Linux/systemd is the primary server runtime. Compose is panel-only; no tested Docker or Paper/Purpur runtime provider is claimed.
- Significant completed features require a checked, versioned GitHub Release. No release version, publication, real-world deployment, or production-readiness is asserted in this entry.

### Verification still required

- Complete integrated checks, Linux/systemd/firewall and real Minecraft acceptance, Proxmox LXC testing, provider delivery tests, and disaster-recovery drills.
- Confirm release signing and trusted publication, both native architectures, updater discovery, panel-only restart, and post-update rollback on a real host. See [acceptance](docs/acceptance.md) for the evidence boundary.
