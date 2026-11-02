# Architecture

New Empire Server Control is a Go modular monolith with a separately built React application embedded in the executable. Node.js is not required by the installed panel. SQLite is local, foreign-key enforced, WAL enabled, and uses FULL synchronization. Versioned migrations run under an exclusive SQLite transaction and reject unknown future schemas.

## Boundaries

- `internal/api`: same-origin session-authenticated versioned REST endpoints and live invalidation events.
- `internal/auth`, `internal/secrets`: Argon2id passwords, hashed session tokens, CSRF tokens, optional TOTP, AES-GCM credentials bound to a purpose-specific key.
- `internal/database`, `internal/model`, `internal/instances`: durable records, migrations, shared types, allowlisted instance roots, and isolation.
- `internal/jobs`, `internal/scheduler`: durable job state, step history, cancellation commit barriers, explicit interrupted state after crash, and timezone-aware schedules.
- `internal/runtime`, `internal/rcon`, `internal/servicehelper`: Linux/systemd supervision through a restricted helper and bounded localhost RCON. The HTTP process is not a host shell.
- `internal/files`, `internal/importer`, `internal/uploads`, `internal/backups`: rooted filesystem operations, staged archive processing, tus resumable uploads, and verified recovery points.
- `internal/fabric`, `internal/modrinth`, `internal/modpacks`: authoritative runtime metadata and hash-identified server-side Fabric content. Paper/Purpur plugins are not supported.
- `internal/storage`, `internal/notifications`: explicitly configured offsite replication and outbound notification adapters. Credentials are not returned by APIs.
- `internal/updates`: signed release discovery/staging and independent panel-only application with health-checked rollback.
- `web`: embedded UI; `deploy`: installation/service examples; `.github/workflows`: checks and orchestrated trusted-branch publishing.

## Transactions and recovery

Database and filesystem changes do not share an atomic transaction. Mutations therefore use durable plans/jobs, staged files, verified backups, filesystem-local commits when available, and retained recovery paths. Cross-filesystem copies must never be described as atomic. Interrupted jobs do not automatically replay destructive operations.

Two workers may process different instances, but a database claim transaction prevents simultaneous mutations for the same instance. Global panel jobs exclude other running jobs. No application update handler may restart Minecraft, replace world files, or invoke content rollback.

See [the wire contract](integration-contract.md), [OpenAPI](api/openapi.yaml), [security](security.md), and [migrations](migrations.md). Actual acceptance evidence is tracked separately from intended architecture.
