# Database migrations

`internal/database/migrations/001_initial.sql` initializes users/sessions, isolated instances and grants, jobs/steps, settings/secrets, schedules, metrics, notification queue, and audit history. `PRAGMA user_version` is the authoritative schema version.

Startup applies each missing numbered SQL migration in one `BEGIN EXCLUSIVE` transaction. An unavailable lock or migration failure stops startup; a newer schema is rejected. Foreign keys, WAL, busy timeout, and FULL synchronization are set on the database connection.

Before a panel update, use SQLite's consistent `VACUUM INTO` snapshot through the application, not a plain copy of an active database file. A raw main-file copy can miss committed WAL pages. Preserve the credential master key separately with restrictive permissions. Verify snapshots with `PRAGMA integrity_check` and `PRAGMA foreign_key_check`.

Automatic panel updates must refuse schema changes unless rollback compatibility is explicitly supported. Restoring the old binary alone is not a database downgrade strategy. Never restore panel metadata by replacing a running database or by deleting WAL files.

Add migrations, tests, schema compatibility policy, release notes, and disaster-recovery instructions in the same change. Never edit an already released migration; add the next version instead.
