# Content domain API

`modrinth` and `modpacks` do not use the database, runtime, or HTTP handlers. The caller must run every mutation as an audited durable job, hold the instance mutation lock, create a verified external restore point, and stop Minecraft before `Apply` or `Rollback`. Never use content rollback to restore world data.

## Modrinth

```go
client := modrinth.NewClient(nil) // guarded official HTTPS API/CDN; inject *http.Client in tests
service := modrinth.NewService(client)
target := modrinth.Target{MinecraftVersion: instance.MinecraftVersion, Loader: "fabric", LoaderVersion: instance.LoaderVersion}
inventory, err := service.Inventory(ctx, instanceRoot) // *os.Root, read-only, SHA-512 identity
plan, err := service.Plan(ctx, instanceRoot, modrinth.Request{Target: target, Projects: []string{"fabric-api"}})
// err is an I/O/API failure; plan.Blockers must also be empty before proceeding.
err = service.Stage(ctx, stagingRoot, plan) // dedicated *os.Root outside managed instance
tx, err := modrinth.Apply(ctx, instanceRoot, stagingRoot, plan)
// Check every error. Restart and health-check in the caller; keep the transaction until healthy.
// If unhealthy: stop Minecraft, then Rollback(ctx, instanceRoot, tx).
err = modrinth.Finalize(instanceRoot, tx) // explicit cleanup after health or completed rollback
```

- Client: `Search(ctx, query, target, offset)`, `Project(ctx, idOrSlug)`, `Projects(ctx, ids)`, `Version(ctx, id)`, `Versions(ctx, project, target, allowPrerelease)`, `VersionsFromHashes(ctx, hashes)`, `UpdatesFromHashes(ctx, hashes, target, allowPrerelease)`, `RateLimit()`.
- `Request{Target, Projects, Remove, Versions, Policy}`: no Projects/Remove and nil Versions checks updates to all managed mods; otherwise unrequested projects retain their exact versions. Versions is project → exact version ID; a non-nil empty map keeps existing versions (config-only packs). Optional `*Policy` replaces the stored policy. Pins/excludes are canonical project IDs. Explicit requested versions still obey compatibility and channel policy.
- `Policy{Mode, AllowPrerelease, Pins, Excludes}`: Mode is `manual` (default), `notify`, or `automatic` (explicit opt-in). Automatic plans block unidentified JAR compatibility. Scheduling, maintenance windows, player postponement, and restart authorization belong to the caller. Stage/Apply never run automatically themselves.
- `Plan{ID, Target, Before, HadMods, LockHash, Files, Changes, Policy, Rationale, Blockers, Config, Pack}` is JSON-serializable and intended for durable job storage. ID is content-derived; replanning unchanged state produces identical JSON. `Files` is the complete desired managed lock. `Before` fingerprints all original mod files, including unmanaged files. Changes explain install/update/remove, original filename, and rationale. `Validate()` is also called by Stage/Apply. `Seal()` updates the content-derived ID after validated pack metadata/config attachment. Plans/stages must remain trusted job data, not accepted from a browser.
- `Inventory{Files, Lock}` includes filename, name (version name or filename fallback), version number, SHA-512, size, project/version identity when recognized, `managed`, and a diagnostic. A recognized JAR without an exact lock match remains unmanaged. Nested JARs are inventoried but never implicitly managed. Unknown files are never removed/adopted implicitly. Project titles and update availability can be mapped by the caller using Project/UpdatesFromHashes and a fully validated plan.
- `.newempire-content-lock.json` stores schema, target, policy, and exact filename/hash/project/version/download identity. `LoadLock(root)` and `SavePolicy(root, policy)` are provided; SavePolicy is a job mutation too.
- `LoadTransaction(root, id)` resumes recovery after process interruption. Apply journals original content before swapping directories. Rollback refuses changed content rather than overwriting later administrator changes. Reserve `.newempire-content-*` from the generic file manager. These short-lived recovery artifacts are not external backups.
- Apply can return a non-nil transaction together with an error: retain its ID and inspect/recover its persisted receipt before starting Minecraft. `Finalize` is idempotent; missing rollback receipts are an error, not an invented successful recovery.
- Only official `https://api.modrinth.com/v2` and `https://cdn.modrinth.com/data/` are accepted. API bodies and downloads are bounded; retries obey bounded/cancelable rate-limit backoff. No plugin management, arbitrary mirrors, shell calls, world writes, or archive execution.

## Server-only modpacks

```go
pack, err := modpacks.Read(readerAt, size, target)
plan, err := pack.Plan(ctx, service, instanceRoot)
err = pack.Stage(ctx, service, stagingRoot, plan)
tx, err := modrinth.Apply(ctx, instanceRoot, stagingRoot, plan)
err = modpacks.Export(ctx, instanceRoot, writer, modpacks.ExportOptions{Name: "My server", VersionID: "1", Target: target})
```

Read validates ZIP paths/types/bounds, the official index, exact Minecraft/Fabric requirements, server environments, and SHA-512/SHA-1 metadata. Client-only entries and `client-overrides` are excluded. Server mods must reconstruct to exact official Modrinth file/version identities and required dependency locks; unsupported or unknown projects block import. `overrides/config/` and `server-overrides/config/` are supported; existing local configuration wins, server overrides win over generic overrides. World/runtime paths, embedded JARs, and other override locations fail closed. Config changes use the same content transaction journal and rollback, with no world rollback.

Export writes a server-only reconstruction index referencing original CDN URLs, never redistributes JARs, and includes explicitly requested config files only. It refuses unmanaged or modified JARs rather than inventing redistributable metadata. No licensing or download-access restrictions are bypassed.

`Reconstruct(ctx, client, index, target)` returns `([]modrinth.LockedFile, []string, error)` for independent index validation. `Pack{Index, Warnings}` exposes parsed metadata. `ExportOptions{Name, VersionID, Target, ConfigPaths}` includes no config unless explicitly selected. Pack import is currently stable-only, even when the existing mod policy permits prereleases.

See [`docs/modrinth-and-modpacks.md`](../../docs/modrinth-and-modpacks.md) for transaction recovery, bounds, unsupported archive content, and integration obligations.
