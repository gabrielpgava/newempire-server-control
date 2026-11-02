# Offline disaster recovery

The recovery CLI does not need the panel HTTP service. It uses the configured,
**already initialized** SQLite database and matching `master.key`; it never
bootstraps a replacement database or encryption key. Stop the panel and use its
dedicated unprivileged account. Root recovery is rejected before writing state,
so an emergency command cannot leave root-owned panel files behind.

```sh
sudo systemctl stop new-empire-control.service
sudo -u new-empire-control /opt/new-empire-control/control recover --config /etc/new-empire-control/config.json
sudo -u new-empire-control /opt/new-empire-control/control recover --config /etc/new-empire-control/config.json list INSTANCE_ID
```

Replace uppercase placeholders with the exact recorded identifiers. Instance,
backup, content-transaction and file-recovery tokens are 32 lowercase hexadecimal
characters. Filesystem recovery takes the full `.nesc-recovery-<32-hex-ID>` name.
Flags must precede subcommands. Unknown arguments, unsafe links and missing
confirmation fail closed.

Every offline command acquires `panel.lock` exclusively. Do not delete that file
to bypass a running panel or another recovery operation. Inspecting state never
executes queued jobs. Mutations run only the explicitly selected durable job;
orphaned running jobs are marked interrupted, and unrelated queued jobs remain
queued. Job IDs and progress go to stderr; successful results are JSON on stdout.
On failure, preserve the reported job ID and inspect it with `recover`. A signal
requests cancellation before safe commit barriers; do not forcibly kill a
filesystem commit or remove staging/journals to make it appear complete.

Local audit entries record the real/effective UID, GID and resolved execution
account, not an untrusted `SUDO_USER` value. Retain host sudo/audit logs to identify
the human who switched to the panel account. The local account and configured
storage roots are trusted administrative boundaries.

## Local backups with no panel HTTP service

Minecraft must already be stopped for offline mutations. Use the normal service
manager or the deployed allowlisted runtime helper to stop only the selected
Minecraft service; stopping the panel does **not** stop Minecraft. Recovery fails
if runtime status or safe offline preparation cannot be proved. Fix helper
permissions, instance processes, filesystem ACL support or disk space rather than
bypassing those checks.

```sh
sudo -u new-empire-control /opt/new-empire-control/control backup --config /etc/new-empire-control/config.json --offline list INSTANCE_ID
sudo -u new-empire-control /opt/new-empire-control/control backup --config /etc/new-empire-control/config.json --offline create INSTANCE_ID
sudo -u new-empire-control /opt/new-empire-control/control backup --config /etc/new-empire-control/config.json --offline verify INSTANCE_ID BACKUP_ID
sudo -u new-empire-control /opt/new-empire-control/control backup --config /etc/new-empire-control/config.json --offline preview INSTANCE_ID BACKUP_ID
sudo -u new-empire-control /opt/new-empire-control/control backup --config /etc/new-empire-control/config.json --offline --confirm restore INSTANCE_ID BACKUP_ID
```

This is local recovery: it does not download from GitHub/Modrinth or fetch or
replicate offsite backups. Fetch remote-only data through the normal authenticated
workflow when available, or restore independently verified local backup files and
their matching catalog and `.instance.json` records into protected backup storage.
`list` reads catalog records; use `verify` for current archive/manifest integrity.
An archive without portable instance metadata cannot be restored. Preview and
restore apply the same identity, allowlist, EULA, memory, name and runtime
configuration preflight used by the API before data replacement.

Full restore explicitly replaces **world progress, content and runtime files**.
It creates a verified pre-restore backup, retains the displaced instance tree and
swap journal, restores matching database/helper configuration, and leaves
Minecraft stopped. Check the result and runtime/content before starting it.

### Interrupted restore metadata

A successful filesystem swap can be followed by a failed helper/database update.
The durable `restore-pending` record blocks startup and further mutations so the
old runtime cannot start against mismatched files. Inspect the recorded state:

```sh
sudo -u new-empire-control /opt/new-empire-control/control recover --config /etc/new-empire-control/config.json restore INSTANCE_ID
sudo -u new-empire-control /opt/new-empire-control/control recover --config /etc/new-empire-control/config.json --confirm restore INSTANCE_ID
```

This reconciles the **current restored tree's metadata only**, after requiring
its file hashes to match the recorded restored backup. It never swaps files or
worlds. If the interrupted commit left the original tree installed, preview and
explicitly reconcile the recorded previous identity instead:

```sh
sudo -u new-empire-control /opt/new-empire-control/control recover --config /etc/new-empire-control/config.json restore-previous INSTANCE_ID
sudo -u new-empire-control /opt/new-empire-control/control recover --config /etc/new-empire-control/config.json --confirm restore-previous INSTANCE_ID
```

The previous selection also requires an exact match against its recorded verified
backup; it is not a request to roll a world back. While this guard exists,
content/file/runtime recovery and new backups/restores refuse rather than create
a backup with misleading runtime metadata. If a current tree still exists,
filesystem recovery also refuses: reconcile that exact tree's recorded identity
first. Never clear the guard by editing SQLite.

If the original tree was retained but the process stopped before installing the
staged tree, the live target may be missing. `recover list` can bind the retained
directory's actual-target journal to the pending restore's stage and any recorded
recovery/journal identifiers, even when the job never persisted its result:

```sh
sudo -u new-empire-control /opt/new-empire-control/control recover --config /etc/new-empire-control/config.json list INSTANCE_ID
sudo -u new-empire-control /opt/new-empire-control/control recover --config /etc/new-empire-control/config.json --confirm filesystem INSTANCE_ID .nesc-recovery-RECOVERY_ID
sudo -u new-empire-control /opt/new-empire-control/control recover --config /etc/new-empire-control/config.json --confirm restore-previous INSTANCE_ID
```

The filesystem step requires stopped Minecraft, verifies both recorded backup
archives, validates the pre-restore portable identity, and checks every backed-up
path/hash in the selected retained tree against that pre-restore snapshot before
moving it into the missing target. It does not guess from a directory name or
change an adopted tree's ownership. No safety backup of a nonexistent target is
possible: the two original verified snapshots remain restore points, and the
verified retained-source archive and original journals are preserved as well.
The original pending record remains unchanged after the move; startup is still
blocked until the separately confirmed `restore-previous` job verifies the tree
in place and reconciles its helper/database identity. If no trusted journal and
snapshot match, preserve all data for a version-specific recovery plan.

## Retained filesystem, file and content recovery

Run `recover list INSTANCE_ID` first. Never guess a recovery ID or reuse another
instance's sibling directory.

```sh
sudo -u new-empire-control /opt/new-empire-control/control recover --config /etc/new-empire-control/config.json --confirm filesystem INSTANCE_ID .nesc-recovery-RECOVERY_ID
sudo -u new-empire-control /opt/new-empire-control/control recover --config /etc/new-empire-control/config.json --confirm file INSTANCE_ID FILE_RECOVERY_TOKEN
sudo -u new-empire-control /opt/new-empire-control/control recover --config /etc/new-empire-control/config.json --confirm content INSTANCE_ID CONTENT_TRANSACTION_ID
```

- **Filesystem:** explicitly swaps back a recorded full instance tree. It binds
  the selected directory to that instance's swap journal and original verified
  backup and revalidates its contents. Without an existing pending restore it
  requires unchanged metadata and backs up both selected and displaced data;
  it retains the displaced tree and syncs the swap journal/directories. Run
  `recover --confirm restore INSTANCE_ID` afterward to reconcile the selected
  backup's helper/database identity: filesystem completion alone never clears
  the startup guard. Pending/missing-target recovery uses the separate previous
  identity procedure above. Changed retained trees, unbound journals, missing
  portable records, or differing metadata without a pending plan cause refusal.
  Differing metadata in that last case needs an explicitly reviewed full backup
  restore so runtime/helper identity changes together. Do not rename directories
  by hand and leave database/runtime identity inconsistent.
- **File:** restores an existing trash token to its recorded destination without
  overwriting a newer file, using the existing file-recovery job and verified
  safety backup. Conflicting pending content must be recovered first.
- **Content:** rolls back only the selected recorded mod transaction and its
  approved configuration. It makes a verified safety backup and retains the
  receipt/displaced mods. Unknown/manual mods are preserved. **It never restores
  worlds**, including world progress made after a failed mod update. Normal
  later startup can finalize a rolled-back content transaction; the external
  safety backup remains available. Changed mods/locks/configuration or damaged
  recovery records cause refusal, not a guessed rollback.

`recover --confirm runtime INSTANCE_ID` recovers only a recorded pending runtime
migration. It verifies the pre-upgrade archive and portable identity, then matches
the retained launcher and properties against that snapshot before restoring
runtime/content. A verified raw `inspection_backup_id` preserves the possibly
mixed displaced state without a misleading `.instance.json` sidecar. Worlds are
never restored by this operation, and Minecraft remains stopped. A changed
Minecraft version latches a world-format review guard before metadata replacement;
it persists across completion failures and can require a separately confirmed
pre-upgrade world restore before the old runtime is safe to start. Missing,
changed, or unbound recovery material fails closed rather than guessing from
staging directory names.

## Emergency owner access

Keep the panel stopped. The existing database and matching vault are required;
this command cannot create/promote an account or recover a lost encryption key.
It resets one existing owner's password, removes that owner's encrypted TOTP
factor, revokes **all owner sessions**, and commits the security audit in the same
SQLite transaction. Other accounts are not modified. A factor that cannot be
decrypted causes refusal until matching encryption material is restored.

Provide one new UTF-8 password line (12–1024 bytes) on standard input. Never put the
password in a command argument, shell history, environment variable or log. For
example, feed it from a protected password-manager command:

```sh
password-manager-command | sudo -u new-empire-control /opt/new-empire-control/control recover --config /etc/new-empire-control/config.json --confirm --password-stdin owner OWNER_USERNAME
```

Replace `password-manager-command` with your actual secure producer. No password,
factor secret or session credential is printed. Restart the panel, sign in with
the new password, and re-enroll TOTP. Ordinary CLI login supports existing TOTP
accounts with `login --username NAME --password-stdin --code CODE`, or use
`--code-stdin` to supply the password on the first input line and the six-digit
authentication code on the second. CLI session files are atomically replaced
with owner-only permissions; symlinks, hardlinks, special files and unsafe modes
are rejected. CLI HTTP requests never follow redirects, so configure the exact
panel origin rather than a redirecting URL.

## Panel update disaster recovery

This procedure concerns the **panel's executable, SQLite database, configuration,
and credential-encryption material**. Minecraft services, worlds, mods, and world
backups are separate. Never restore a world merely because a panel update failed.

### Preserve evidence first

1. Prevent concurrent panel-update jobs. Do not remove `updates/lock`; its advisory
   lock is released when the owning process exits. Wait for a running helper to
   finish instead of launching another mutation.
2. Preserve `updates/journal.json`, the `current`/`previous` symlink targets,
   relevant `versions/` directories, and `updates/snapshots/<transaction-id>/`.
   Keep copies private: snapshots contain configuration and credentials.
3. Inspect the recorded state and English diagnostic, disk availability,
   administrator-controlled path permissions, and panel-only service logs. Do
   not edit the journal to force an apparently successful state.

### Run recovery outside the panel

The integrated restricted CLI helper must invoke `updates.Manager.Recover(ctx)`
with the configured install directory, pinned public key, supported database
schema, panel service adapter, and consistent snapshot/restore callbacks. It does
not need GitHub access or a running panel. The CLI's concrete command is owned by
the application integration; this domain does not install or claim an additional
standalone recovery command.

| Durable state | Recovery behavior |
| --- | --- |
| No transaction, `staged`, `complete`, `failed`, `rolled_back` | No automatic mutation. A staged release remains staged. |
| `downloading` | Records failed interrupted staging; explicitly stage again. |
| `stopping`, `snapshotting` | Checks actual stopped-state schema before restarting/checking the unchanged panel; no database restore. Unsafe state stays stopped. |
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

### When automatic recovery refuses

- **Actual schema differs, or the database is missing/empty/unsafe:** the helper
  checks protected copies with committed WAL and never initializes or migrates
  them. It refuses both automatic restore and startup, including restarting an
  unchanged panel after an aborted update. Keep the panel stopped; preserve the
  current database, sidecars and snapshot. An administrator must choose and
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

### SQLite/configuration restore responsibilities

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
