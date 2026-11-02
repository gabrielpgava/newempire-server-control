# Offsite backup storage

The `internal/storage` adapter uploads completed local backups to an S3-compatible HTTPS endpoint and downloads objects into verified local staging files. It uses AWS Signature Version 4 with the standard library; no provider SDK or external executable is required.

This adapter does not create backups or restore worlds. The application integration owns encrypted settings, authorization, jobs, local manifest verification, progress reporting, retention and administrator-confirmed restores. See [the package API](../internal/storage/API.md) for the exact integration contract.

## Configuration

The settings configuration has this shape:

```json
{
  "enabled": true,
  "endpoint": "https://account.r2.cloudflarestorage.com",
  "region": "auto",
  "bucket": "minecraft-backups",
  "access_key": "PROVIDE_THROUGH_OWNER_SETTINGS",
  "secret_key": "PROVIDE_THROUGH_OWNER_SETTINGS"
}
```

For Amazon S3, use the actual regional endpoint and matching signing region, for example `https://s3.us-east-1.amazonaws.com` and `us-east-1`. For Cloudflare R2, use the account's S3 API endpoint and signing region `auto`, not a public bucket/custom-domain download URL. The region is configuration, not a guessed default. An optional `session_token` supports temporary credentials; their renewal remains the caller's responsibility. An optional `prefix` scopes keys within the bucket.

- Only HTTPS port 443 is supported. Endpoints must be origins without user information, query, fragment or a path other than `/`.
- The endpoint must resolve exclusively to public addresses when connecting. Private, loopback, link-local, metadata-service and other non-public addresses are rejected. DNS is checked at the dial boundary and the connection uses the validated IP, preventing a second DNS lookup from changing the destination.
- All redirects are refused, including same-origin redirects. Fix a region or endpoint mismatch rather than allowing credential-bearing requests to redirect.
- Bucket names are 3–63 lowercase letters, digits, dots or hyphens, beginning and ending with a letter or digit; IP addresses, consecutive dots and adjacent dot/hyphen pairs are rejected.
- Keys and prefixes are safe relative paths of at most 1,024 combined bytes. Segments may contain ASCII letters, digits, `.`, `_` and `-`; empty segments, `.`/`..`, absolute paths, backslashes and percent escapes are rejected. A prefix has no trailing slash.

Validate enabled configurations with `Config.Validate()` before saving. Disabled configurations may be empty. Validation checks syntax and rejects literal non-public IPs; DNS, TLS and actual credentials are checked during a real transfer, not during configuration parsing. `Redacted()` returns only `{"configured":true|false}` and is not a connectivity or credential-validity test.

The parent must encrypt the **whole configuration** with its Vault, restrict updates to the owner, and never return the saved configuration to the browser or logs. Errors from the adapter exclude endpoint URLs, credentials, provider response bodies and local file paths. Never log signed request headers. `NewWithHTTPClient` is a trusted code-level test seam, not a settings option; production uses `New` and `internal/netguard`.

Use a dedicated, bucket/prefix-scoped credential with only object PUT and GET/HEAD permissions. Listing and deletion permissions are not needed. Keep the bucket private. The adapter provides TLS in transit, not client-side backup encryption or a guarantee of provider-side encryption at rest; configure provider encryption/access/retention policies separately.

## Upload and integrity guarantees

1. The parent completes and verifies the local backup against its local manifest under its backup/job locks.
2. Pass its immutable, absolute regular-file path to `Put(ctx, key, path)`. The adapter refuses symlink sources, pins the opened file, bounds size, hashes it without buffering the archive into memory, and streams it with a signed SHA-256 payload hash. It checks the streamed hash and detects size/mtime changes.
3. A successful PUT is followed by HEAD. The remote size and `x-amz-meta-sha256` must match the locally calculated values before the operation reports success.
4. Persist remote status separately from local backup status. Compare the returned size and SHA-256 to the existing trusted backup record; never replace that record's digest with a different upload result.

**A HEAD check validates metadata, not downloaded object bytes.** Both `Put` and `Head` return `verification: "metadata"`. User metadata can be copied or changed independently of the underlying object; an S3-compatible provider's successful signed-payload response is not an independent end-to-end download check. ETags are not treated as SHA-256 checksums. To verify remote bytes, download and hash them against the trusted local record.

Use instance ID and immutable backup ID in each key, such as `INSTANCE_ID/BACKUP_ID.zip`. PUT has normal S3 replacement semantics: reusing a key replaces its current object. The parent must not reuse a key for a different backup. Provider bucket versioning can offer additional protection but version IDs and historical-version retrieval are not implemented here.

An offsite failure does not delete, invalidate, restore or otherwise change the local backup. An interrupted/ambiguous PUT may already have reached storage; retries send the same pinned backup to the same key. The adapter never deletes an object automatically after a failed validation.

## Remote restore staging

Call `Download(ctx, key, newAbsoluteStagingPath, trustedSHA256, expectedSize)` using the SHA-256 and exact size from the application's trusted local backup record or a separately authenticated recovery manifest. Do not trust a digest obtained only from remote HEAD/user metadata.

The adapter:

- requires an existing protected staging directory and a nonexistent destination;
- downloads only from the configured origin, rejects unexpected size/encoding and bounds received bytes;
- streams into a mode-0600 temporary file, hashes every byte, synchronizes the verified file, and commits with an atomic no-replace hard link;
- removes its partial file on failure/cancellation and refuses to overwrite a destination that appears during the download.

The staging filesystem must support hard links. Caller-supplied source and staging directories must be protected application-managed directories; this package is not a host-wide path authorization layer. Staging never writes directly into an instance. After download verification, the parent still must run archive/import validation, restore preview, graceful stop, a verified pre-restore backup, instance locks and explicit administrator confirmation. No world rollback is automatic.

## Limits, retries and testing

- Single-part objects only, at most **5 GiB**. No multipart/resumable transfer, remote listing/deletion, automatic retention or presigned URLs.
- A 15-minute total operation deadline includes hashing, transfer, backoff and upload HEAD validation. A caller may impose a shorter deadline.
- Each PUT, HEAD or GET phase has at most three attempts, with cancellable 200 ms and 400 ms delays. An upload has a PUT phase and a HEAD phase. Only transient transport failures and HTTP 408/429/500/502/503/504 are retried; checksum and metadata mismatches are not. Interrupted download attempts reset their private staging file before retrying.
- The durable parent job owns later retries, diagnostics and truthful phase-based progress; this adapter does not invent a transfer percentage or convert configuration into successful delivery.

Local tests use public-looking origins routed through explicit fake TLS transports, not a production bypass flag. They cover AWS's four [published SigV4 vectors](https://docs.aws.amazon.com/AmazonS3/latest/developerguide/sig-v4-header-based-auth.html), temporary credentials, actual streamed payloads, HEAD mismatches, corrupt downloads, replay after transient failure, cancellation/cleanup, source symlinks, destination races, redirect refusal, DNS rebinding and error/config redaction.

```sh
go test -race -count=1 -timeout=90s ./internal/storage ./internal/netguard
```

Live AWS/R2 accounts are not exercised by these tests. Provider configuration, permissions and real-network throughput must be checked in a deployment before relying on offsite disaster recovery. Retain the trusted recovery manifests and Vault recovery material outside the Minecraft instance roots.
