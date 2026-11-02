# Backups and restores

`internal/backups` creates streamed ZIP or TAR.ZST archives beneath a trusted external backup root. Instance and backup roots must be disjoint; source files are accessed only through the supplied `*os.Root`, including adopted instances. Backup and recovery roots must be protected from the ordinary file manager and untrusted local writes. Do not configure bind-mount aliases of an instance as backup storage.

The domain does not stop Minecraft or issue save commands. The default application workflow must stop the server and verify shutdown. A coordinated online workflow may call this domain only against an externally guaranteed consistent snapshot; scanning a live directory is **not** advertised as a consistent online backup. Cancellation is checked during file copies and validation. Unknown files/mods are preserved. Internal `.nesc-` staging files are excluded.

## Format and verification

Each archive contains a `data/` tree and a versioned `manifest.json` with every directory/file, actual file size and per-file SHA256. A separate small catalog record stores the archive SHA256, manifest SHA256, compressed size, format, ID and timestamp. Archives and catalog entries use random IDs and private permissions. The archive is synced and fully verified **before** an atomic catalog record makes it visible to `List`.

Verification checks the complete archive checksum, the actual expanded bytes and archive checksums, the raw manifest checksum, manifest identity/totals, and an exact match between every manifest entry and archived data. Missing, extra, duplicated or altered files fail. Checking a manifest alone is insufficient. These local checks detect corruption; they are not a cryptographic signature against an attacker who can rewrite both archives and catalog records. Keep storage permissions and any remote integrity/authentication policy separate.

Archives can be opened as seekable `*os.File` values for authenticated `http.ServeContent` range downloads. Backups include RCON credentials and other private instance content: restrict backup downloads to appropriately privileged users. Do not expose the backup root through generic file endpoints.

Default parser limits are documented in `docs/server-import.md`. Very large/highly compressible backups can fail verification under those limits rather than being falsely marked usable. A failed verification/catalog operation retains the unlisted archive for diagnosis and reports its filename; it does not expose a successful restore point. ZIP/TAR.ZST permissions normalize to 0600 files and 0700 directories; ownership, executable bits, xattrs and ACLs are not preserved.

## Restore and recovery

1. Verify the archive and manifest before extracting.
2. `StageRestore` creates a private `.nesc-restore-<id>` directory under a trusted instance-parent root, extracts into it, verifies the actual staged content, and returns its `data` path plus manifest for a preview. Cancellation never changes the current instance.
3. The application obtains administrator confirmation, verifies the server is stopped, holds its durable exclusive lock, and ensures appropriate pre-restore protection.
4. `CommitRestore` rehashes staging immediately before commit. Target, staging and parent must be on the same filesystem. It writes and syncs a `.nesc-transaction-<id>.json` intent journal before renaming anything.
5. The original directory is renamed to `.nesc-recovery-<id>` and synced; staging is then installed by a no-overwrite rename. The original is **never deleted**. The short commit finishes despite cancellation once the original has been retained.

Persist `RestoreResult` even on error: `recovery`, `journal` and `installed` identify the actual recovery state. A failed second rename attempts to put the untouched original back without overwriting any new destination. Failure to sync or recover is reported, with retained paths. An interrupted process can leave a missing target between the two renames; the intent journal and retained original remain available. Startup must inspect these journals and report an interrupted job, not guess that a restore succeeded.

`Recover` is a separate administrator-confirmed operation. It can restore the original to a missing target after interruption, or swap it with a current instance while retaining that displaced tree under another recovery name. Nothing automatically rolls back world data after a mod/content update failure. Runtime adapters must close/reopen handles and continue to address the same configured target path after a swap.

Retention, journal reconciliation in the durable job layer, remote R2/S3 storage, coordinated live snapshots, encryption/key recovery and automated cleanup are not implemented by this domain. They must not be represented as enabled features. Recovery directories/journals stay until explicit application policy removes them; never treat a generic file cleanup as restore retention.
