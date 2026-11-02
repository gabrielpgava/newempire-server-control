# Backups and restores

The application connects the Backups page, authenticated HTTP routes, durable jobs, runtime coordination, local archives, optional S3-compatible replication, and local retention. `internal/backups` supplies archive verification and staged filesystem swaps; it does not itself authorize users, stop Minecraft, schedule work, or contact offsite storage. Native offline preparation and service reconfiguration require the configured Linux/systemd helper. See the [OpenAPI reference](api/openapi.yaml) for exact request/response shapes.

Backup storage is beneath `backup_dir/INSTANCE_ID`, outside managed instance trees. Instance and backup roots must be disjoint; source files are accessed through allowlisted, descriptor-rooted handles, including adopted instances. Protect backup/recovery roots against untrusted local writes and ordinary file-manager access. Do not configure bind-mount aliases of an instance as backup storage. Unknown regular files/mods are included; internal `.nesc-` staging files are excluded.

## Application workflow and permissions

- Viewers with instance access can read the catalog. Operators can create backups and queue local archive verification on permitted instances.
- Administrators/owners can download, fetch offsite copies, preview, and explicitly restore. Downloads are privileged because archives can contain RCON credentials and other private instance data.
- Administrators/owners can configure local retention. Only the owner may change offsite storage or notification destinations/credentials, including enable-only changes.
- Backup/fetch/verify/restore requests return HTTP 202 with a durable job, not completed work. Inspect its terminal state, error, result, and persisted steps. Jobs serialize per instance; cancellation is safe-point based and cannot undo a committed restore.

`POST /api/v1/instances/{instanceID}/backups` accepts an optional note, `online` flag, and `format` (`zip` by default, or `tar.zst`). The current UI supplies the note and online choice.

**Offline is the default.** If Minecraft is running, the job stops it, verifies shutdown, and runs restricted offline preparation before archiving. It attempts to restart a previously running instance and wait for readiness afterward, including after a backup failure. Restart failure is reported rather than hidden. An already stopped instance stays stopped.

**Coordinated online is explicit.** The server must be ready with authenticated RCON. The job issues `save-off`, then `save-all flush`, and attempts `save-on` in cancellation-independent cleanup even if copying fails. This coordinates Minecraft saves; it is **not** a filesystem snapshot or a transactional guarantee for every mod's private storage. Use offline backups when that distinction matters. Failure to resume saving is a job error requiring immediate administrator investigation.

## Format and verification

Each archive contains a `data/` tree and versioned `manifest.json` with every directory/file, actual file size, and per-file SHA-256. Protected local storage contains:

- `BACKUP_ID.zip` or `BACKUP_ID.tar.zst`: the archive;
- `BACKUP_ID.json`: archive/manifest SHA-256, compressed size, format, ID, and timestamp;
- `BACKUP_ID.instance.json`: separate application metadata describing the instance configuration without its RCON secret.

Archives and catalog entries use random IDs and private permissions. The domain syncs and fully verifies the archive **before** publishing the catalog entry. The application then writes the portable instance sidecar. A later sidecar failure can therefore leave a verified archive visible in the catalog but unusable for an application restore; the job reports that failure. The archive download endpoint returns only the archive, not either sidecar. Preserve sidecars and the panel's trusted database/key recovery material separately; an archive alone is not sufficient for this application's restore preflight.

Verification checks the complete archive checksum, the actual expanded bytes and archive checksums, the raw manifest checksum, manifest identity/totals, and an exact match between every manifest entry and archived data. Missing, extra, duplicated or altered files fail. Checking a manifest alone is insufficient. These local checks detect corruption; they are not a cryptographic signature against an attacker who can rewrite both archives and catalog records. Keep storage permissions and any remote integrity/authentication policy separate.

The catalog's `verified` flag records creation-time integrity, not a new check of local or remote bytes. The optional `local` field reports local catalog presence; `false` means fetch the offsite archive before local use. Listing does not prove that an archive still exists or is unchanged. Explicit verification and restore preview read the archive fully. Downloads use authenticated HTTP byte ranges and check the catalog/file size when opening, but do not rehash the entire archive for each download. Do not expose backup storage through generic file endpoints.

Default parser limits are documented in `docs/server-import.md`. Very large/highly compressible backups can fail verification under those limits rather than being falsely marked usable. A failed verification/catalog operation retains the unlisted archive for diagnosis and reports its filename; it does not expose a successful restore point. ZIP/TAR.ZST permissions normalize to 0600 files and 0700 directories; ownership, executable bits, xattrs and ACLs are not preserved.

## Restore and recovery

1. Fetch an offsite-only copy first, then request the administrator restore preview. Preview verifies the archive and validates `BACKUP_ID.instance.json`; it returns paths, sizes, and warnings without stopping Minecraft or replacing files.
2. Review the warning that world progress will be replaced, then explicitly confirm the restore. The API requires `{"confirm":true}` and queues a separate restore job; the UI does not enable confirmation without a successful preview.
3. Under the instance lock, the job repeats archive verification and instance-metadata preflight **before stopping Minecraft or replacing data**. Missing or malformed metadata, a different instance ID, root, game/RCON ports, or adoption mode, or invalid instance configuration fails without stopping the server or changing its world. Configuration validation includes the Java allowlist, safe relative launcher JAR, memory bounds, EULA, and protected/allowlisted root checks. Restore is not an implicit cross-instance migration.
4. After preflight and a free-space check, the job stops Minecraft if necessary and creates a verified backup of the current state. `StageRestore` extracts into a private `.nesc-restore-<id>` directory under the trusted instance parent and verifies the extracted bytes. The job checks shutdown again before its commit barrier.
5. `CommitRestore` rehashes staging immediately before commit. Target, staging, and parent must be on the same filesystem. It writes and syncs a `.nesc-transaction-<id>.json` intent journal, renames the original directory to `.nesc-recovery-<id>`, syncs, and installs staging with a no-overwrite rename. The original is **never deleted**. The short swap finishes despite cancellation once the original has been retained.
6. If archived Java path, launcher JAR, or memory differs, the application asks the restricted helper to reconfigure the existing service before updating the instance record. Minecraft stays stopped so an administrator can review runtime/content and explicitly start it. A helper/database failure after the swap is reported; it does not automatically undo restored world data.

Inspect the job result even on failure: `restore.recovery`, `restore.journal`, `restore.installed`, and `pre_restore_backup_id` identify retained data and the actual commit state when available. A failed second rename attempts to put the untouched original back without overwriting a new destination. Sync/recovery failures retain diagnostic paths. A process crash can leave a missing target between renames; the journal and retained original must be inspected before retrying. The job engine marks interrupted work as interrupted, but that is not automatic reconciliation of every backup swap journal. See [disaster recovery](disaster-recovery.md) for host recovery procedures.

The domain's `Recover` function can restore a retained original to a missing target or swap it with a current tree while preserving the displaced data. It is not the file-manager trash-recovery endpoint, and there is no generic backup-directory recovery button in the current HTTP API. Recovery is an explicit administrator decision. Nothing automatically rolls back world data after a mod/content update failure. Runtime/file operations must reopen the configured target after a swap rather than keep addressing the retained original.

## Offsite copies and local retention

When owner-configured storage is enabled, ordinary backup jobs replicate the verified local archive to the configured S3-compatible endpoint. The application stores the trusted object key, size, archive/manifest digests, and nonsecret instance configuration in its database. A failed offsite upload does not invalidate or delete the local backup; the job can fail while its result still identifies a verified local restore point.

The catalog's `offsite` state is saved replication metadata, **not an independent check of current remote bytes**. Upload verification checks provider size/SHA-256 metadata after PUT. `POST /api/v1/instances/{instanceID}/backups/{backupID}/fetch-offsite` instead downloads bytes against the trusted saved digest/size, refuses to overwrite an existing local archive, recreates catalog/instance sidecars, and performs full archive/manifest verification. It does not stop Minecraft or apply a restore. Preview and explicit restore confirmation remain separate afterward.

There is no remote bucket discovery or arbitrary-object restore endpoint. Preserve the trusted database and credential master key outside the instance roots; remote user metadata alone cannot replace those trust records. Storage supports single-part objects up to 5 GiB, not resumable/multipart backup transfers. TLS transport is not client-side archive encryption. See [offsite storage](offsite-storage.md) for provider constraints.

Local retention is opt-in and disabled by default. Once enabled, the application queues a per-instance retention job approximately daily, keeping the newest configured number (1–10,000; default preference 7). It fully verifies the backups to retain before deleting any older local archives and their catalog/instance sidecars. Remote records/objects and retained restore/recovery directories are not deleted by this policy. Failure can leave partial retention progress; inspect the job result.

The archive domain alone implements none of that policy scheduling or replication. Filesystem-wide online snapshots, client-side backup encryption, remote listing/deletion/version retrieval, and automatic cleanup/reconciliation of retained restore journals remain outside the current backup HTTP workflow. Never treat generic file cleanup as backup/recovery retention. These documented mechanisms are not evidence of a successful live provider or disaster-recovery drill; consult [testing](testing.md) for actual checks and gaps.
