# Safe files and resumable uploads

The files domain operates on already-open `*os.Root` handles. An HTTP path never chooses a host root. Relative names must be unambiguous slash-separated UTF-8 paths: traversal, absolute paths, backslashes, control characters, colons, empty components and `.`/`..` components are rejected. `.` alone selects the root for listing, searching or exporting.

Listing, case-insensitive filename search, bounded UTF-8 previews and seekable regular-file downloads are available. Links (including multiply-linked regular files), FIFOs, sockets and devices fail closed. Go's root confinement protects resolution against traversal races; final file descriptors are checked against their pre-open identity. Host bind mounts and privileged mount changes are outside this sandbox: provision allowlisted instance roots without untrusted mounts and deny mount privileges to runtime accounts.

Text replacement writes a private sibling temporary file, preserves an existing file's UID/GID and safe read/write bits (`mode & 0666`), syncs it, then atomically renames it. Executable and special permission bits are never copied. If the service cannot retain the original ownership, replacement fails without changing the original: group membership alone does not authorize changing a different UID. New files default to 0600; the application's managed-directory/runtime helper must explicitly establish any required per-instance group-access policy. Backups and uploads remain private regardless of runtime group policy. NUL-containing or invalid UTF-8 data and over-limit edits are rejected. `Move` never overwrites an existing destination, including one created concurrently. Directory creation and successful mutations sync their parent directories. The application must enforce stale-edit ETags, block credential-bearing files such as raw `server.properties`, create required restore points, stop the runtime and hold its durable per-instance mutation lock. These primitives do not replace those policies.

Trash moves to a separate administrator-owned recovery root and returns a random token. The API/job must retain the original path and token. Recovery refuses to overwrite anything. Trash/recovery use same-filesystem renames; crossing filesystems fails without copy/delete fallback. Recovery storage must be disjoint from instance roots and must not be exposed through the ordinary file manager. The `.nesc-` namespace is reserved for panel staging and is not accessible to ordinary file-manager calls.

ZIP exports stream data and can be cancelled. Export authorization and secret filtering remain mandatory in the HTTP layer, including exports of whole directories. Exports are not a consistency mechanism for a running world; use a coordinated backup instead. An interrupted streaming export is not a successful download.

## tus staging

`internal/uploads` implements tus 1.0 through tusd. Supply a private 0700 `*os.Root`, a full base URL path, an authorization callback, a maximum file size, a total reserved-byte quota, an upload-count cap and a TTL no longer than seven days. The callback runs for **every** request, including HEAD, OPTIONS and DELETE, and must enforce the application's session, role, Origin and mutation CSRF policies. Returning an owner ID binds uploads to that owner; client metadata cannot change ownership or disk paths.

- A single process-held OS lock prevents two services from independently reserving the same quota.
- Full declared sizes are reserved before accepting content, so concurrent partial uploads cannot overcommit quota. The count cap also bounds zero-length uploads.
- Per-upload OS locks serialize PATCH, lookup, deletion and expiry. Wrong offsets return HTTP 409. Chunk data is synced before acknowledging it; offset is recovered from the file length after restart.
- A simple filename is required. Unknown lengths, concatenation, method overrides and tus GET downloads are deliberately disabled.
- `Lookup` returns an already-open file **only** for a completed, unexpired upload owned by the caller. Close it after use. Import code reads this descriptor rather than trusting a metadata filename as a path.
- DELETE releases reservations. Run `Prune` periodically to remove expired content. Startup and new-upload requests also prune; abandoned data from a crash before metadata publication is removed at startup. Active writes have an expiry deadline as well; an already-blocked network read can take up to the configured 30-second network timeout to finish. Invalid stored metadata is an actionable error, not silently ignored.

Upload staging is not backup storage and must never be an instance directory. Stop request processing before closing the upload service, then close its borrowed root. Per-owner business quotas, request rate limits and durable job/audit integration belong to the HTTP/application layer.

See `internal/files/API.md` for exact Go signatures and `docs/server-import.md` for archive handling.
