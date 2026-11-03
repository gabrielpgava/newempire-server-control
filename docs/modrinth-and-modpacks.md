# Modrinth mods and server modpacks

This domain supports **Fabric server mods**, not Paper/Purpur plugins. It uses the official Modrinth v2 API and exact official CDN files. Runtime installation and migration remain separate operations. The domain does not start or stop Minecraft, run shell commands, write world files, schedule jobs, or own the database.

The Go integration surface and examples are in [`internal/modrinth/API.md`](../internal/modrinth/API.md).

## Inventory and management ownership

Inventory hashes JAR bytes with SHA-512 and resolves them through `POST /version_files`. A filename alone never establishes identity or management ownership.

- A file is managed only when filename, size, SHA-512, project ID, and version ID match `.newempire-content-lock.json` and the API's hash result.
- Known but externally installed JARs remain unmanaged. Unknown JARs remain unmanaged. Both are preserved, including files not listed in a modpack.
- A missing, changed, or unresolvable locked JAR blocks mutation until an administrator reconciles it explicitly. There is no automatic adoption or deletion of external files.
- A request that would collide with unmanaged content or install a second copy of an identified unmanaged project is blocked.
- Other regular files and subdirectories under `mods/` are copied unchanged during a transaction. Links, special files, filesystem escapes, excessive trees, and unsafe paths are rejected.

The lock records exact filenames, hashes, sizes, original CDN URLs, project/version IDs, runtime target, policy, and optional pack reconstruction metadata. It is internal control data, not an editable file-manager document. The parent must protect `.newempire-content-*` paths from ordinary file-manager operations.

## Preview, compatibility, pins, and dependencies

Planning is read-only. Its JSON includes actions, selected locked files, rationale, blockers, the original mod-tree fingerprint, and the content-lock fingerprint. Its content-derived ID makes an unchanged preview deterministic. The API/job owner can hash `json.Marshal(plan)` and reject a changed preview before applying. Do not accept a complete plan supplied by a browser.

The default channel is stable (`release`), and the default policy mode is `manual`. Alpha/beta require explicit `AllowPrerelease`. Automatic execution requires explicit caller opt-in and caller-enforced maintenance windows, player postponement, backup, shutdown, restart, and health checks. Policy mode alone never starts background mutations in this package.

Automatic-mode plans block unidentified JARs because their dependency compatibility cannot be established. Manual plans explain and preserve them, but do not claim that unknown code is compatible.

Selections require the exact requested Minecraft version, the Fabric loader, and a mod project type. New version-level `environment` metadata is authoritative. Client-only, singleplayer-only, unknown, or unsupported environments are not treated as dedicated-server compatible. Server-capable environments include optional server installations such as `client_only_server_optional`. When version environment metadata is absent/unknown, uniformly server-capable project environments or legacy `server_side: required|optional` provide backward compatibility; ambiguous project aggregates fail closed.

Required dependencies are selected recursively. Exact required versions, explicit version requests, and project/version pins must agree. Incompatible relationships, optional-version conflicts when the optional mod is present, required cycles, unidentified file-only dependencies, and attempts to remove a required project block the plan. Excluded projects remain at their exact installed version and cannot be removed or newly installed implicitly. Pins and excludes use canonical project IDs, not slugs.

Resolution is deterministic and bounded (256 projects, depth 64, 16 exact-version reselections). It does not perform exhaustive SAT/backtracking over alternative releases. If the newest compatible candidates cannot form a consistent set, select an explicit compatible version set rather than guessing or dropping dependency checks.

`POST /version_files/update` is available for bulk update checks, with exact game/Fabric filters and `version_types: ["release"]` by default. A returned candidate is not permission to install it: a complete domain plan still validates environment, dependencies, policy, collisions, and original content.

## Safe staging, apply, and recovery

The parent must:

1. Authorize the administrator and persist an audited job and accepted preview hash.
2. Hold the exclusive instance mutation lock, replan, and reject stale previews.
3. Stage into a dedicated allowlisted `*os.Root` outside the managed instance. Staging is safe while Minecraft runs because it does not modify the instance.
4. Create and verify an appropriate external backup/restore point; stop Minecraft gracefully before apply.
5. Call `Apply`, restart through the runtime provider, and check real health.
6. On failure, stop Minecraft before calling `Rollback`. On success or completed recovery, call `Finalize` explicitly.

Staging downloads exact allowlisted HTTPS CDN URLs, enforces size, verifies SHA-512 (and SHA-1 when available), and binds a staged manifest to the entire confirmed plan. Apply checks the manifest and hashes again, rejects changed mod/policy state, constructs a complete replacement directory, and writes a durable transaction receipt before entering the commit barrier. Cancellation is honored before that barrier; commit or recovery then completes without cancellation.

Apply retains the original mod directory, swaps in the complete replacement, and updates the content lock. These are multiple journaled atomic renames, **not** a claim of a single cross-directory atomic filesystem operation. `LoadTransaction(root, plan.ID)` plus `Rollback` recovers the tested intermediate layouts after interruption. The parent must recover interrupted jobs before starting Minecraft. Recovery artifacts are not substitutes for the required external verified backup.

Rollback restores the original mods and exact prior content lock, and removes only unchanged config files newly created by that transaction. It never restores world data or runtime binaries. It refuses destructive rollback if mod files, the lock, imported config, or the original recovery copy changed later. That refusal requires administrator-directed recovery, not an automatic world restore. Empty config directories may remain after rollback.

`Finalize` deletes validated recovery artifacts only after explicit caller confirmation. It refuses unfinished transactions and unexpected content drift.

All instance and staging access is anchored through supplied `*os.Root` handles. Config writes/removals are additionally anchored to a real `config/` sub-root. Do not reopen arbitrary adopted paths: the parent should derive instance roots from allowlisted parent roots.

## Server-only `.mrpack` import

Supported archives use `modrinth.index.json`, `formatVersion: 1`, `game: minecraft`, and exact `minecraft` plus `fabric-loader` dependencies matching the selected runtime. The domain does not silently migrate the runtime. Forge, Quilt, arbitrary runtime dependencies, non-mod indexed downloads, and embedded mod binaries are unsupported and rejected.

- Both SHA-512 and SHA-1 file hashes, size, filename, and an exact original CDN URL must reconstruct to official primary-file metadata.
- Entries marked `env.server: unsupported` are skipped. Required and optional server entries are included. Missing `env` uses the format's default; actual Modrinth server compatibility is still checked independently.
- `client-overrides/` files are never installed. A JAR mislabeled as server-capable by the index is still blocked when official version metadata is client-only.
- Every required dependency must be present in the pack's exact reconstruction set. Unknown projects, omitted dependency locks, conflicting versions, client-only dependencies, or pins that disagree block import.
- `overrides/config/` and `server-overrides/config/` are supported. Server overrides take precedence over generic overrides, and **existing local config always wins**. A config file appearing after preview forces replanning instead of being overwritten.
- World files, launchers, plugins, scripts outside config, options files, resource packs, and all other override locations fail closed. Review imported configuration: even a configuration-only file can change a mod's behavior.

Pack import currently accepts stable Modrinth releases only. It does not download unrecognized third-party files, bypass access restrictions, accept arbitrary mirrors, infer missing hashes, or manufacture redistribution rights.

## Export and reconstruction

Export verifies that every installed JAR is managed and unchanged. It writes a server-only index containing exact hashes, sizes, runtime dependencies, and original CDN URLs; **no JAR bytes are redistributed**. The index can be reconstructed and imported through the same validation and dependency plan.

No config is exported by default. Only explicitly selected regular `config/` paths are included under `server-overrides/`; review them for secrets and redistribution rights before sharing. Unknown, modified, missing, or nested unmanaged JARs make export fail instead of producing a misleading reconstruction pack. Write exports to a temporary artifact and publish only after success.

## Bounds and network behavior

- API response: 16 MiB; hash requests are batched in groups of 100.
- At most three API attempts; retry/reset headers are respected with cancelable backoff. A wait longer than five seconds is returned to the caller, never shortened into an early retry. Each request/retry group has a 45-second timeout; bulk calls can issue multiple batches and inherit the caller's overall deadline.
- API rate-limit limit/remaining/reset information is exposed to the caller. Server error bodies are not propagated into user errors or logs.
- Official HTTPS API/CDN only, exact hosts, guarded public-address resolution, and redirect checks. Injected HTTP clients are for trusted tests; production defaults use `internal/netguard`.
- JAR or copied mod-tree file: 512 MiB; config override: 16 MiB; pack index: 4 MiB.
- File/ZIP entry count: 4096; content work and expanded archives: 8 GiB. Compressed entries above 1 MiB with a ratio greater than 200 are rejected. Exported selected configs have a 64 MiB aggregate limit.
- ZIP traversal, duplicate/case-colliding paths, encrypted entries, links, and special files are rejected. No archive entries execute.

## Verification and integration status

Domain tests cover official endpoint shapes, rate-limit headers/retries/cancellation, bounded responses, compatibility and legacy environments, unmanaged inventory, pins/conflicts/cycles, hash mismatch, stale stages/previews, content rollback, interruption barriers, failed config commit, traversal/links/ZIP bombs, client-only exclusion, exact dependency locks, local-config preservation, and reconstruction round trips. A read-only live API smoke test is available with `MODRINTH_LIVE_TEST=1` and derives its game version from upstream metadata.

This package is not the complete application feature on its own: the parent owns authenticated routes, durable jobs, policy scheduling metadata, external backups, runtime shutdown/readiness, UI, protected file-manager paths, integration tests, and release publication. No release is created by this domain branch.

Upstream references: [hash lookup](https://docs.modrinth.com/api/operations/versionsfromhashes/), [hash updates](https://docs.modrinth.com/api/operations/getlatestversionsfromhashes/), [project metadata](https://docs.modrinth.com/api/operations/getproject/), and [environment semantics](https://modrinth.com/news/article/new-environments/#new-system).
