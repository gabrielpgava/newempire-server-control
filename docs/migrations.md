# Database migrations

`internal/database/migrations/001_initial.sql` initializes users/sessions, isolated instances and grants, jobs/steps, settings/secrets, schedules, metrics, notification queue, and audit history. `PRAGMA user_version` is the authoritative schema version.

Startup applies each missing numbered SQL migration in one `BEGIN EXCLUSIVE` transaction. An unavailable lock or migration failure stops startup; a newer schema is rejected. Foreign keys, WAL, busy timeout, and FULL synchronization are set on the database connection.

Offline recovery uses `database.OpenExisting`, which requires the exact current schema and never creates a database or applies migrations. It checks the committed schema (including WAL) with a read-only, exclusive `unix-none` probe before enabling normal WAL writes. The probe keeps its WAL index in memory and relies on `panel.lock`, not SQLite's file locks, to exclude writers. Database and sidecar files must be private, single-link regular files owned by the executing account and no larger than 1 TiB. The caller must validate trusted private path ancestors and hold `panel.lock`; path-based SQLite access does not defend against a hostile same-account process replacing files. `secrets.OpenExisting` likewise reads only an existing private, single-link, 32-byte master key and never generates one or changes permissions.

Before a panel update, use SQLite's consistent `VACUUM INTO` snapshot through the application, not a plain copy of an active database file. A raw main-file copy can miss committed WAL pages. Preserve the credential master key separately with restrictive permissions. Verify snapshots with `PRAGMA integrity_check` and `PRAGMA foreign_key_check`.

Automatic panel updates must refuse schema changes unless rollback compatibility is explicitly supported. Restoring the old binary alone is not a database downgrade strategy. Never restore panel metadata by replacing a running database or by deleting WAL files.

Add migrations, tests, schema compatibility policy, release notes, and disaster-recovery instructions in the same change. Never edit an already released migration; add the next version instead.
