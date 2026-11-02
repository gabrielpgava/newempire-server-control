# Archive import safety

Import inspection does not require an instance: a completed owner-checked tus file descriptor can be inspected during first-run creation. Supported formats are ZIP and gzip-compressed tar. The shared parser also supports Zstandard-compressed tar for backups. Format selection is explicit; do not infer trust from a filename extension.

Inspection reads **actual expanded data**, validates archive checksums and returns every entry with its size and SHA256. The preview contains a deterministic SHA256 `preview_hash` over the format, ordered entries and content hashes. Extraction into a fresh empty staging root recomputes that same preview and syncs extracted files/directories. Applying a stale preview must fail before any instance mutation.

Default limits are 32 GiB compressed input, 128 GiB expanded file content, 32 GiB per file, 100,000 entries (including implicit directories), and a 1,000:1 expansion ratio. Callers may select smaller limits. Negative and excessive limit settings are rejected. ZIP central-directory size is capped at 64 MiB and entry records are bounded before allocating the archive index. Tar header/extension overhead, decoder memory and trailing padding are separately bounded.

The importer rejects:

- Traversal, absolute/ambiguous paths, reserved `.nesc-` paths and unsafe filename characters.
- Duplicate entries, case-aliasing paths and file/directory parent collisions.
- Symlinks, hardlinks, sparse tar files, FIFOs, devices and other special entries.
- Encrypted/unsupported ZIP content, multi-volume archives, checksum errors, truncated compression streams and nonzero hidden data after a tar end marker.
- Expanded-size, entry-count or compression-ratio violations, even when archive metadata appears plausible.

ZIP64 is supported within the same limits. Self-extracting ZIP layouts are not supported. Extracted permissions are normalized to 0600 files and 0700 directories; archive ownership, executable bits and special permissions are not trusted. Non-Linux filesystems can impose additional filename normalization rules: extraction never overwrites existing files and fails closed on collisions the filesystem detects.

## Application transaction

1. Authorize the owner and obtain a completed upload descriptor.
2. Inspect and present warnings, launcher/world choices and a preview hash. Runtime detection, EULA and RCON policy belong to the application, not the archive parser.
3. On apply, inspect/extract again under the job's context and compare the confirmed hash. Never apply an unvalidated staging tree.
4. For an existing instance, hold its exclusive lock, verify shutdown and create a verified restore point before changing files. Preserve unknown content unless the administrator explicitly confirms replacement.
5. Install staging using a same-filesystem recoverable transaction. Never merge extracted entries directly into a live world. Keep an original recovery path until the job is durably complete and retention policy explicitly permits deletion.

Extraction errors or cancellation leave only partial staging; the caller removes its newly-created staging directory. The importer never deletes or rewrites instance data. Browser folder uploads should be packaged as ZIP with relative names and sent through tus. Arbitrary host-folder adoption remains a separate allowlisted-root workflow.
