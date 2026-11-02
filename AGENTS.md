# AGENTS.md - New Empire Server Control

## Project mission

New Empire Server Control is a browser-based, headless Linux management application for Minecraft server instances. Implement complete, safe lifecycle management of a Minecraft server with Go, an embedded React frontend, durable job execution, backups, content updates, and GitHub Release-based application updates.

This file is binding guidance for every coding agent contributing to this repository. Read it before making changes, and update it in the same pull request when project architecture or engineering rules change. The exact paths and commands below must be adjusted to match verified repository contents as the implementation evolves.

## Language and public-project policy

- **English only** for all user-visible copy, documentation, code comments, errors, CLI messages, issue/PR templates, changelog, and release notes.
- Never introduce Portuguese as the default language. Future localization is allowed through a proper internationalization layer.
- Make no false claims about supported features, security guarantees, tests, version compatibility, publishing, or release status.
- Preserve existing repository licensing. Public source availability is not the same as an OSI-approved open-source license. If PolyForm Shield is used, describe it as source-available.

## Architecture and engineering conventions

- Prefer a modular Go monolith: chi router, versioned SQLite migrations, typed SQL, explicit dependency injection, reusable domain services and interfaces.
- Build the React/TypeScript frontend separately, then embed its distribution into a single Go application binary. The deployed native package must not require Node.js.
- Linux/systemd is the primary runtime provider; any Docker support must be a distinct runtime adapter with accurately documented limitations.
- Keep Minecraft server instances isolated by explicit per-instance IDs, data roots, ports, service names, locks, configuration, and role-based permissions.
- Use RCON only on localhost or a secured internal network. Do not add arbitrary shell execution features.
- All operations that may mutate world files, instance configurations, mod content, or runtime files must use a validated job and auditable transaction plan.
- Prefer repository-local patterns and existing working code over wholesale rewrites.
- Do not hard-code current Minecraft, Fabric, Java, or Go versions. Read authoritative upstream metadata and verify compatibility.
- Module path is `github.com/gabrielpgava/newempire-server-control`. Shared domain types live in `internal/model`; domain packages must not import HTTP handlers. See `docs/integration-contract.md` for integration boundaries.
- The executable entrypoint is `cmd/control`; `internal/api` integrates domain services, durable jobs, and authenticated HTTP routes. Native helpers use `internal/servicehelper`; the independent updater exchanges bounded request/report files through `internal/panelupdate`.
- Runtime version choices come from Fabric and Mojang metadata. Go 1.27 is the verified build baseline; `.go-version` and `go.mod` are the build-toolchain authority.
- The repository is licensed under PolyForm Shield 1.0.0 by the owner's explicit selection. Describe it as source-available, not OSI-approved open source.

## Data safety: absolute requirements

1. Never modify a running server's files in a way that can leave a partial world, runtime, or content update.
2. Before imports, destructive edits, restores, Minecraft/Fabric migrations, or significant content updates, run preflight checks and create an appropriate verified backup or restore point.
3. Stage downloads and file mutations, verify their hash and origin, then apply only after required checks and graceful shutdown.
4. Provide meaningful progress, cancellation when safe, durable task state, failure diagnostics, and a recovery path.
5. Preserve unknown or externally managed mods. Respect explicit version pins and lockfiles.
6. Never roll back world data automatically just because a content update failed; make world recovery a separate, administrator-confirmed operation.
7. Keep backups outside managed instance directories and protect them against ordinary file-manager deletion.
8. Enforce safe relative paths and race-resistant filesystem access. Reject traversal, unsafe archive links, ZIP bombs, filesystem escapes, and access outside allowlisted roots.

## Security requirements

- Use least-privilege accounts and service permissions. The Go web process must not run with general root privileges.
- Restrict privileged operations to explicit, validated, audited systemd/runtime helper calls.
- Protect sessions, passwords, API permissions, setup flow, file downloads, uploads, and RCON credentials.
- Defend third-party downloads and URLs against SSRF and redirection into private networks; inspect source metadata and hashes.
- Keep secrets out of logs, HTTP responses, and repository history; configure credential encryption/recovery.
- Never allow unsigned or unverified panel auto-updates. Verify the expected official release identity, signature/attestation, checksum, architecture, and version policy.
- Never grant a fork pull-request workflow write access to production credentials or GitHub release permissions.

## Required modules and definition of functional completeness

- Secure bootstrap, admin settings, sessions, roles, and audit history.
- First-run wizard: create instance, import archive/folder, or adopt existing local directory.
- Lifecycle: start/stop/restart, service integration, logs, console, status, and health.
- File manager: resumable uploads, folder/archive imports, previews, safe text editing, downloads, cleanup, and recovery.
- Backups: offline and coordinated online backups, integrity manifests, downloading, restore previews, retention, optional R2/S3, and disaster recovery.
- Fabric/Java: compatible runtime detection, version choices, migration plans, and safe upgrades.
- Modrinth: hash-based inventory, browsing, install/update/remove, dependency solving, version pins, `.mrpack`, stable-only default, and unattended compatible updates after opt-in.
- Distinguish Fabric **mods** from Paper/Purpur **plugins**. Only expose plugin management for implemented and tested plugin-based providers.
- Players/worlds/settings: whitelist, bans, OPs, safe configuration editing, imports, exports, and runtime controls.
- Jobs and scheduling: durable state, exclusive per-instance mutation locks, retries, maintenance windows, crash recovery, and truthful progress.
- Monitoring, diagnostics, actionable notifications, and mobile-friendly UI.
- Application GitHub Release self-updater with verified download, independent updater process, service restart limited to panel, and health-checked rollback.
- CI, security verification, automated releases, reproducible installable assets, and full English documentation.

No module is complete if its user-facing controls are nonfunctional or its failure path is untested.

## Development and code review workflow

- Before coding: inspect the repository, its branch status, current docs and tests. Do not discard user work.
- Describe scope and integration boundaries for a large feature. Implement in complete vertical slices.
- Keep changes small enough to review, but do not split a feature into unusable partial releases.
- Use Conventional Commits. Prefer squash merges with conventional PR titles.
- Add or update tests for changed behavior. Test failure modes, not only successful paths.
- Update OpenAPI and end-user documentation alongside any API or workflow change.
- Run repository-defined formatting, linting, security checks, migrations, frontend build, unit tests, and applicable integration tests before declaring work complete.
- Record known risks and evidence of actual test execution. Never invent results.
- `make check` and `make security` are the integrated automated gates; `make release-dry-run` builds unsigned-publication test artifacts using an ephemeral signing key and makes no GitHub writes. `bash scripts/test-sandbox.sh` runs Go race tests with read-only source/module mounts and no network; its private Docker volume is only for the helper's ACL fixture because Docker tmpfs may not support POSIX ACLs. The deployed application still requires ACL-capable storage.

## Parallel agents and worktrees

- Delegate independent substantial modules when the execution environment supports parallel work.
- Assign each agent clear module ownership, tests, API contracts, and docs responsibilities.
- Use separate branches or worktrees to avoid competing changes to the same files.
- Centralize shared migrations, integration contracts, release automation, and `AGENTS.md` maintenance under one integration owner.
- Integrate branches, resolve conflicts, and run the complete suite after merge. Do not stop with unmerged worktrees.

## Mandatory release policy

**Every completed significant feature MUST result in a new versioned GitHub Release once merged into `main`, after required checks pass. This requirement is part of the definition of done, not a suggestion.**

A significant feature includes a new complete module, a substantial user-visible capability, a major workflow, a newly supported runtime/storage provider, safety-critical functionality, or a major API/behavior change. Tiny copy changes and internal refactors need not each produce a release.

- `feat:` for a backward-compatible user-facing feature must lead to a MINOR SemVer bump.
- `fix:` for a defect must lead to a PATCH SemVer bump.
- `feat!:` or `BREAKING CHANGE:` must lead to a MAJOR SemVer bump.
- A significant feature PR must have a conventional release-producing title/commit and an appropriate `release:feature` label or equivalent CI-validated marker.
- Before merging: pass CI, update tests, complete English documentation, add changelog entries, and verify required migrations.
- After merging: the automated release workflow must publish its version tag, Linux amd64 and arm64 assets with embedded UI, machine-readable metadata, checksums, authenticated signatures/provenance, and English release notes.
- Verify the new release can be discovered and consumed by the application self-updater. If release automation fails, the feature is NOT fully delivered until the pipeline is repaired or the blocker is accurately reported.
- Never publish incomplete/untested code just to satisfy a schedule. Never assert that a GitHub Release exists unless it actually exists.
- Releases are built from trusted protected branches, with minimal permission scopes and safeguards against concurrency, duplicate tags, and release asset mismatch.
- Avoid a split tag-triggered workflow that relies on tags generated by `GITHUB_TOKEN` to start another workflow: those events ordinarily do not trigger new Actions jobs. Use a single orchestrated publish process or an intentional permitted dispatch mechanism.

## Release and self-update acceptance

1. CI rejects broken builds, failing tests, invalid release metadata, and missing mandatory files.
2. A complete significant `feat:` merge leads to a new valid GitHub release on the stable channel.
3. Every release contains authenticatable assets for supported host architectures, and the update manifest matches them.
4. The panel can check an official GitHub release, stage it, verify it, update only itself, and recover automatically from failed post-update health checks.
5. A panel update must not restart Minecraft or modify world and content files.
6. If publication is blocked by permissions/secrets, report the exact blocker and keep all build and release code ready; never fabricate publication.

## Documentation checklist

Keep README, installation guides (Linux/systemd, Proxmox LXC, Docker), API references, development/testing, security, import/files, backup/restore, Modrinth/Fabric, operations, self-update, release process, and disaster recovery documentation synchronized with the actual implementation.

## Final reporting requirements

For any substantial change, report what was implemented, tests that actually ran, any skipped or failing tests and why, documentation changes, release version/status and links when confirmed, limitations, and the exact remaining work. Claims such as "production-ready" or "100% complete" require verified evidence across all applicable acceptance criteria.
