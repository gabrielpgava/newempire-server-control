# Panel update disaster recovery

This procedure concerns the **panel's executable, SQLite database, configuration,
and credential-encryption material**. Minecraft services, worlds, mods, and world
backups are separate. Never restore a world merely because a panel update failed.

## Preserve evidence first

1. Prevent concurrent panel-update jobs. Do not remove `updates/lock`; its advisory
   lock is released when the owning process exits. Wait for a running helper to
   finish instead of launching another mutation.
2. Preserve `updates/journal.json`, the `current`/`previous` symlink targets,
   relevant `versions/` directories, and `updates/snapshots/<transaction-id>/`.
   Keep copies private: snapshots contain configuration and credentials.
3. Inspect the recorded state and English diagnostic, disk availability,
   administrator-controlled path permissions, and panel-only service logs. Do
   not edit the journal to force an apparently successful state.

## Run recovery outside the panel

The integrated restricted CLI helper must invoke `updates.Manager.Recover(ctx)`
with the configured install directory, pinned public key, observed database
schema, panel service adapter, and consistent snapshot/restore callbacks. It does
not need GitHub access or a running panel. The CLI's concrete command is owned by
the application integration; this domain does not install or claim an additional
standalone recovery command.

| Durable state | Recovery behavior |
| --- | --- |
| No transaction, `staged`, `complete`, `failed`, `rolled_back` | No automatic mutation. A staged release remains staged. |
| `downloading` | Records failed interrupted staging; explicitly stage again. |
| `stopping`, `snapshotting` | Restarts/checks the unchanged original panel; no database restore. |
| `installing`, `checking_health`, `rolling_back` | Verifies the preserved binary and snapshot, stops only the panel, restores SQLite/config, switches to the old binary, restarts/checks it. |
| `manual_recovery` | Retries the recorded rollback, or restarts the unchanged panel if application never reached a completed snapshot. Errors remain visible when recovery is unsafe. |

Recovery is idempotent after it records its terminal state. A process can crash
between a symlink swap and a readiness check; the journal is persisted before the
swap, so the next helper conservatively rolls back rather than assuming success.
Restoration callbacks must themselves be idempotent because interrupted restore
is retried. They must honor their context and must not run migrations.

For a completed update that later needs an administrator-requested rollback, the
independent helper invokes `Manager.Rollback(ctx)`. That restores the last
successful update's same-schema snapshot and previous binary, after stopping the
panel. **Panel settings/users/sessions changed since that snapshot are reverted.**
Confirm that loss with the administrator before requesting rollback. No world
files are reverted.

## When automatic recovery refuses

- **Schema differs:** do not run an older binary against the newer SQLite schema.
  Preserve the current database and snapshot. An administrator must choose and
  execute a version-specific manual migration/recovery plan; automatic recovery
  fails closed.
- **Snapshot hash, file set, permissions, or manifest changed:** preserve the
  damaged copy and recover from a separately verified panel backup. Do not alter
  hashes in the journal to make a damaged snapshot pass.
- **Previous binary missing/changed:** the private snapshot includes `binary`.
  An administrator may restore it only after independently checking the snapshot
  manifest digest against the recorded transaction and its file hashes. Preserve
  suspect binaries first, keep the panel stopped, restore only the expected old
  version executable with correct ownership/permissions, and retry the helper.
  Do not overwrite any currently executing helper binary.
- **Current symlink references an unexpected version, an absolute path, or
  traversal:** do not guess the intended target. Resolve the transaction and
  installed versions manually. The helper deliberately refuses unknown state.
- **Stop/restore/restart/readiness fails:** repair the fixed panel service,
  authentication/readiness configuration, filesystem access, or disk capacity,
  then retry recovery. A failed restore must leave the panel stopped until
  consistency is restored. Do not restart Minecraft to repair the panel.
- **Journal damaged or absent after a suspected interrupted application:** keep
  all versions/snapshots. The helper cannot reconstruct an authenticated
  transaction from filenames and deliberately does not guess.

## SQLite/configuration restore responsibilities

The panel must be stopped before replacing its database or configuration. Restore
the consistent SQLite backup together with its matching configuration and
encryption/recovery material. Handle WAL/SHM files explicitly so stale sidecars
cannot replay unrelated state, use atomic file replacement and directory fsync,
restore least-privilege ownership, and verify SQLite integrity before startup.
These operations belong to the integrated restore callback; do not replace a
live SQLite file with an ordinary file copy.

After recovery, verify local authenticated readiness, the actual running panel
version/schema, administrator access, credential decryption, and the unchanged
Minecraft services/data. Retain the incident journal and snapshot until recovery
has been verified and an administrator approves their secure removal.

Keep an independent, tested backup of panel state and encryption material outside
the install directory. Updater snapshots on the same disk are rollback points,
not protection against disk loss, filesystem corruption, or compromise.
