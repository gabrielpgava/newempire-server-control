# Acceptance and verification evidence

This is an evidence ledger, not a completion badge. **No production-readiness, real Minecraft/systemd deployment, Proxmox test, or published release is confirmed.** Update this file with command output/artifact links and exact revisions when the integration owner completes additional checks.

## Evidence recorded so far

As reported by the integration owner on **2026-11-02**, the first Linux-container pass with networking disabled (`docker --network none`) and Go's race detector passed for `internal/auth`, `internal/database`, `internal/jobs`, `internal/scheduler`, and `internal/netguard`. This is unit/local-fixture evidence, not host-runtime or upstream connectivity evidence. The documentation agent did not independently rerun those tests.

New `internal/config`, `internal/instances`, and `internal/secrets` tests were pending in that report. Tests merely present in source, checks run in other unmerged branches, and planned CI jobs are not recorded as passing here. The final integrated suite, embedded frontend build, OpenAPI validation, security checks, and release checks must be recorded after integration.

Documentation-worktree checks on **2026-11-02** passed with `openapi-spec-validator` 0.9.0 and JSON Schema Draft 2020-12 validation: the OpenAPI 3.1 document, all local references, duplicate-key rejection, unique operation IDs, every path parameter, required mutation Origin/CSRF headers, and coverage of the parent router. The reference also documents tus operations, explicit owner-controlled update postponement, and the separate loopback-only updater health probe. Eleven positive/negative schema assertions covered recovery confirmation, required move/adoption fields, player broadcasts, import previews, and rejection of secret-bearing user/settings responses. This is static contract evidence, not an HTTP/runtime test; rerun after the remaining handlers are integrated.

## Twelve required acceptance checks

| # | Acceptance criterion | Unit/mock/local evidence | Infrastructure/release evidence still required |
| --- | --- | --- | --- |
| 1 | Native Go application serves the actual embedded UI; version/migrations/build identity are correct on supported architectures; deployed runtime needs no Node.js. | Database migration/snapshot/future-schema rejection tests reported passed. Integrated native/frontend build not yet confirmed here. | Fresh Linux amd64/arm64 packages, browser load from the packaged binary, exact build identity, clean install and upgrade. |
| 2 | Protected bootstrap, sessions, CSRF/origin checks, roles, instance isolation, and secret recovery enforce least privilege. | Auth bootstrap/session/role, TOTP/throttling tests reported passed. Config/instance/vault additions pending; HTTP authorization matrix not yet confirmed. | HTTPS proxy and cookie checks; denied cross-instance access; token consumed; credentials absent from API/logs; recovery using matching database/master key. |
| 3 | Create, uploaded archive/folder import, and allowlisted adoption use explicit EULA/runtime choices and reject stale previews without rewriting external data. | Contracts and domain implementation alone are not end-to-end proof. No integrated result recorded. | Exercise all three paths on disposable server copies, conflicts/ports/RCON errors, stale source data, and interrupted first-run jobs. |
| 4 | Lifecycle jobs use restricted systemd calls, graceful stop, truthful readiness/logs, and protected local RCON; no shell/root web process. | Runtime/helper/RCON fixture coverage must be recorded after merge; no passed real-host test recorded. | Actual systemd units, service identities, firewall denial from another host, real Minecraft readiness/start/stop/restart/logs/console and failure diagnostics. |
| 5 | Files/uploads/imports enforce confinement, limits, ownership, resumability, stale-edit protection, recoverable deletion, and safe exports. | Integrated filesystem/upload test results not recorded. | Interrupted/restarted upload, traversal/link/ZIP-bomb rejection, browser folder ZIP, cross-user denial, same-filesystem trash/recovery, and large-volume/disk-full drills. |
| 6 | Offline/coordinated-online backups verify integrity, download with authorization, preview restore, preserve a recovery point, and handle retention/offsite failures honestly. | Backup/archive/restore fixture results not recorded. | Real Minecraft online save coordination, corrupted archive refusal, complete restore to a disposable instance, retention behavior, S3/R2 upload/download/recovery, full disaster-recovery rehearsal. |
| 7 | Fabric/Java choices derive from authoritative metadata, validate compatibility, stage runtime updates, and never implicitly roll back a world. | Integrated metadata/Java/staging results not recorded. Public documentation availability is not runtime validation. | Live selected-version staging and first-launch downloads; actual migration and failed-readiness recovery with a valuable-world copy. Record origin-only Fabric launcher trust separately from independently published hashes. |
| 8 | Modrinth hash inventory/plans preserve unknown mods and pins, solve dependencies, default to stable server-compatible versions, reject stale plans, and gate automation. | Content/modpack test results not recorded here. | Live upstream inventory/search/install/update/remove/disable/pin, dependency failures, restart readiness, `.mrpack` workflow exposed by the integrated API/UI, and failure recovery without world replacement. |
| 9 | Players/worlds/properties enforce per-action roles, safe keys/names, preservation, confirmation, and truthful output. | No integrated player/world/property job result recorded. | Real whitelist/bans/OP/IP-privacy checks, world activation/generation/import/export, custom dimensions, property/comment preservation and restart behavior. |
| 10 | Durable jobs, per-instance exclusivity, safe cancellation, idempotency, retry boundaries, and timezone-aware schedules survive interruption. | Jobs exclusivity/idempotency/interruption and scheduler DST/window tests reported passed. | Kill/restart at staging/commit barriers, host power-loss simulation on disposable data, scheduled catch-up, lock contention, blocked/failed scheduled tasks, player postponement. |
| 11 | Monitoring/notifications and mobile/keyboard UI show actionable, accurate states without leaking data. | No integrated browser accessibility/performance or live-delivery evidence recorded. | Real resource/accounting observations, Discord/webhook/SMTP delivery/failure/retry, keyboard/mobile browser checks, empty/error states and actual application screenshots. |
| 12 | Trusted CI produces authentic Linux releases, and the independent updater verifies official identity/assets/version policy, restarts only the panel, health-checks and rolls back safely. | Release/updater fixture tests and local packaging results must be recorded after merge. No published release confirmed. | Successful protected-branch workflow, tag/release/assets/manifest/checksum/signature/provenance links, updater discovery and both architectures, tamper/wrong-identity rejection, failed-health rollback, unchanged Minecraft PID/world/content. |

**A row is not complete because a control exists or a mock test passed.** A failure path without a test or a live prerequisite without evidence remains a gap.

## Release-specific acceptance

For check 12, independently record each mandatory release condition:

1. CI rejects broken builds, failing tests, invalid metadata, and missing mandatory files.
2. A complete significant conventional `feat:` merge creates the expected new stable SemVer release after checks pass.
3. Both Linux architectures have authenticated assets whose names, sizes, hashes, versions, and manifest entries match.
4. The panel discovers the official release, stages/verifies it, invokes the independent updater, and recovers after failed health checks.
5. Panel updates do not restart Minecraft or change world/mod files; record PIDs and filesystem evidence before and after.
6. If permissions or signing configuration block publication, record the exact failure and retain releasable build code. Do not invent a release or substitute unsigned binaries.

Current release version/status: **Unreleased; publication not confirmed.** No release URL or CI-success badge is asserted.

## Remaining release gates

- Merge the independently owned modules and documentation, then run repository-defined format/lint/security/migration/frontend/unit/integration checks on the integrated revision.
- Validate OpenAPI paths, parameters, request bodies, role checks, response shapes, and operation-specific job results against the final HTTP handlers and frontend client.
- Complete all applicable disposable-host drills above on Linux/systemd; separately test Proxmox LXC before claiming that deployment is verified.
- Confirm the installer and recovery guides against actual files and commands. A Compose panel is not a tested Minecraft Docker adapter.
- Provide real external-provider and browser evidence, including screenshots only from the implemented UI.
- Configure protected release permissions and signing, publish only after required checks, verify the actual release, and run the updater/rollback acceptance.

When recording a result, include date, commit, OS/architecture, exact command, pass/fail/skip, artifact/log location, and limits. Redact tokens, passwords, webhook URLs, private player records, and production filesystem paths. Use only disposable instances for destructive tests.
