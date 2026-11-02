# Storage adapter API

```go
cfg := storage.Config{
    Enabled: true, Endpoint: "https://account.r2.cloudflarestorage.com",
    Region: "auto", Bucket: "backups", AccessKey: accessKey, SecretKey: secretKey,
}
err := cfg.Validate() // Also accepts a disabled configuration.
client, err := storage.New(cfg)
summary := cfg.Redacted() // Only {configured: bool}; never return cfg to the browser.
object, err := client.Put(ctx, "instance/backup.zip", verifiedLocalBackupPath)
metadata, err := client.Head(ctx, object.Key)
restored, err := client.Download(ctx, object.Key, newStagingPath, object.SHA256, object.Size)
```

`Config` JSON fields: `enabled`, `endpoint`, `region`, `bucket`, `access_key`, `secret_key`, optional `session_token` and `prefix`. Encrypt the entire configuration with the parent Vault. `New` rejects disabled configurations. `Prefix` is optional and is applied internally; returned `Object.Key` remains relative to that prefix.

`Object` fields: `Key string`, `Size int64`, `SHA256 string`, `Verification string`. `Head` and successful `Put` return `Verification == "metadata"`: HEAD confirmed size and the stored SHA-256 metadata, **not an independently downloaded checksum**. Successful `Download` returns `Verification == "download_sha256"` after hashing all downloaded bytes against the caller's trusted local backup record. Never substitute remote metadata for that trusted record.

`Put` accepts an absolute path to a completed, verified, immutable regular backup file in the parent's protected backup area. The parent owns origin/instance authorization and local manifest verification. The adapter refuses symlink sources, hashes and streams the pinned file, signs its payload hash, checks streamed bytes, and validates HEAD afterwards. Local backup success and remote-copy success are separate states.

`Download` requires an absolute, nonexistent staging destination, a trusted 64-digit SHA-256 and an exact expected size. It never overwrites existing files. It writes a private temporary file and exposes the destination only after verification and synchronization. Import/restore jobs, previews, instance locks, preflight backups and administrator confirmation remain the parent's responsibility.

Transfers use HTTPS port 443, public DNS at connection time, path-style S3 SigV4, no redirects, at most three attempts per PUT/HEAD/GET phase and a 15-minute total operation timeout. Uploads have a PUT phase followed by a HEAD phase. Only single-part objects up to 5 GiB are supported. There is no delete, listing, multipart upload, retention or presigned-URL API. Errors omit URLs, credentials, provider bodies and local paths; cancellation remains recognizable with `errors.Is`.

`NewWithHTTPClient(cfg, *http.Client)` is an explicit trusted transport injection seam for tests. It retains configuration validation, redirect refusal and time bounds. Production must call `New`, which uses `internal/netguard.New(endpoint.Hostname())`; do not accept transports or bypass switches from settings.
