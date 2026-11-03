# Archive import safety

Import inspection does not require an instance: a completed owner-checked tus file descriptor can be inspected during first-run creation. Supported formats are ZIP and gzip-compressed tar. The shared parser also supports Zstandard-compressed tar for backups. Format selection is explicit; do not infer trust from a filename extension.

The archive parser reads **actual expanded data**, validates archive checksums and returns every entry with its size and SHA256. Its deterministic hash covers the format, ordered entries and content hashes. Extraction into a fresh empty staging root recomputes that hash and syncs extracted files/directories. The application binds the initial-instance preview to the selected server subtree and inspected runtime as described below. Applying a stale preview fails before provisioning or instance mutation.

Default limits are 32 GiB compressed input, 128 GiB expanded file content, 32 GiB per file, 100,000 entries (including implicit directories), and a 1,000:1 expansion ratio. Callers may select smaller limits. Negative and excessive limit settings are rejected. ZIP central-directory size is capped at 64 MiB and entry records are bounded before allocating the archive index. Tar header/extension overhead, decoder memory and trailing padding are separately bounded.

The importer rejects:

- Traversal, absolute/ambiguous paths, reserved `.nesc-` paths and unsafe filename characters.
- Duplicate entries, case-aliasing paths and file/directory parent collisions.
- Symlinks, hardlinks, sparse tar files, FIFOs, devices and other special entries.
- Encrypted/unsupported ZIP content, multi-volume archives, checksum errors, truncated compression streams and nonzero hidden data after a tar end marker.
- Expanded-size, entry-count or compression-ratio violations, even when archive metadata appears plausible.

ZIP64 is supported within the same limits. Self-extracting ZIP layouts are not supported. Extracted permissions are normalized to 0600 files and 0700 directories; archive ownership, executable bits and special permissions are not trusted. Non-Linux filesystems can impose additional filename normalization rules: extraction never overwrites existing files and fails closed on collisions the filesystem detects.

## New-instance source selection

`POST /api/v1/imports/preview` takes a completed `upload_id`. It validates and temporarily extracts the archive in private panel staging, then identifies **exactly one supported unattended Fabric bootstrap beside `server.properties`**. Multiple supported launchers, multiple server roots, unverifiable legacy/custom/non-Fabric launchers, and unsafe `level-name` values are rejected. Up to 32 candidate JARs are inspected; `mods`, `libraries`, and `.fabric` directories are not launcher candidates. No uploaded JAR is executed.

Initial import and adoption also fetch current official Fabric/Mojang metadata to verify the detected versions and effective Java requirement. They are **not fully offline**, even when the archive already contains server/library caches. Working DNS and certificate-verified HTTPS to the allowed metadata origins are required; a metadata/network error is not permission to bypass version, TLS or public-address checks. See [Fabric and Java](fabric-and-java.md) for upstream validation and limits.

The initial-instance preview adds:

- `server_root`: `.` for an unwrapped source, or a safe archive-relative directory such as `backup/server`.
- `launchers`: the supported launcher path **relative to that root**, not to the outer archive.
- `files`, `worlds`, `file_count`, and `total_bytes`: only the contents of that subtree, with rebased paths.
- `minecraft_version`, `loader_version`, and `java_required`: statically inspected game/loader identity and the effective Java minimum verified against upstream metadata. These are not migration choices.
- `preview_hash`: a hash binding the entire inspected source, server root, rebased launcher, detected versions, and required Java major.

`POST /api/v1/instances` with `mode:"import"` must echo `server_root`, `preview_hash`, the rebased `jar`, and the detected Minecraft/Fabric versions, alongside the ordinary name, Java, memory, ports, EULA, and upload fields. Apply re-extracts and revalidates the source and authoritative Java metadata before provisioning. Only the reported subtree is copied into the new instance root. For example, `backup/server/Realm/level.dat` becomes `Realm/level.dat`; the original `level-name=Realm`, whitelist, unknown mods, and ordinary configuration are retained. Files outside the chosen subtree are not installed.

This does **not** change existing-instance file-manager imports. Their previews/applications retain archive-relative paths and their existing backup, collision, and recovery rules. Browser folder packaging already removes its selected outer folder; server-side root selection also handles uploaded archives with wrappers.

Instance names with surrounding whitespace or control characters are rejected before a job is queued, rather than failing registration after provisioning.

## Reserved local adoption

Stop the external service, make and verify an external backup, and grant the panel inspection access before adoption. `POST /api/v1/adoptions/preview` takes the canonical absolute `path` of the **actual server directory**, not a wrapper. Its source preview has `server_root:"."` and the same runtime fields above, plus:

- `reservation_id`: a persisted 32-character lowercase hexadecimal instance ID, bound to the authenticated owner, exact path, and source preview hash.
- `service_account`: the dedicated `nec-mc-<first 24 ID characters>` account/group identity to prepare through reviewed host administration.

Repeated previews for the same owner/path/hash reuse the reservation, including after a panel restart. An optional `reservation_id` in the preview request explicitly reopens that reservation; a different owner, path, or hash is rejected. Permission/ACL preparation does not change the content hash. Do not make the source world-writable or grant Minecraft the panel group to bypass preparation.

Creation with `mode:"adopt"` must include `reservation_id`, `server_root:"."`, and the matching preview/runtime fields. After preflight succeeds and immediately before the first provisioning attempt, the reservation fixes its name, Java/launcher, versions, memory, ports, EULA acceptance, and original creation time. Retry with those same settings after preparing access. Failed preflight, such as insufficient Java, does not freeze an incompatible runtime choice. The wizard allows explicit retry after a failed/interrupted/cancelled adoption job and reuses the reserved identity; a lost helper reply can therefore replay the same registration rather than allocate another account. Repeating a completed adoption returns its existing registration without starting Minecraft.

Adoption never rewrites source contents, ownership, modes, or ACLs. It still requires matching existing game/RCON ports, a strong existing RCON credential, existing EULA acceptance, unchanged source content, and helper permission checks. Runtime claims must match the inspected launcher; adoption is not a runtime migration. A content change needs a new inspection, not an override of the old reservation. Failed managed create/import jobs may still leave stopped registrations and retained staging for administrator-assisted recovery; reserved adoption is not a general managed-creation resume facility.

## Application transaction

1. Authorize the owner and obtain a completed upload descriptor.
2. Inspect and present warnings, launcher/world choices and a preview hash. Runtime detection, EULA and RCON policy belong to the application, not the archive parser.
3. On apply, inspect/extract again under the job's context and compare the confirmed hash. Never apply an unvalidated staging tree.
4. For an existing instance, hold its exclusive lock, verify shutdown and create a verified restore point before changing files. Preserve unknown content unless the administrator explicitly confirms replacement.
5. Install staging using a same-filesystem recoverable transaction. Never merge extracted entries directly into a live world. Keep an original recovery path until the job is durably complete and retention policy explicitly permits deletion.

Extraction errors or cancellation leave only partial staging; the caller removes its newly-created staging directory. The importer never deletes or rewrites instance data. Browser folder uploads should be packaged as ZIP with relative names and sent through tus. Arbitrary host-folder adoption remains a separate allowlisted-root workflow.
