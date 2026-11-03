# Verified panel updater integration

## Release signing contract (version 1)

Repository is fixed to `gabrielpgava/newempire-server-control`. GitHub tags are
`v<SemVer>`; manifest versions omit `v`. Stable excludes drafts and prereleases.
The explicitly selected `beta` channel also admits SemVer prereleases.

Upload `release.json` and `release.json.sig` beside both Linux archives. The
manifest is **exactly** compact UTF-8 JSON, in this field order, without a BOM,
spaces, or trailing newline. Assets are sorted by name; hashes are lowercase.
`schema` is the panel SQLite schema version, not a JSON format version.

```json
{"version":"1.2.0","repository":"gabrielpgava/newempire-server-control","schema":1,"assets":[{"name":"new-empire-control_1.2.0_linux_amd64.tar.gz","os":"linux","arch":"amd64","size":12345,"sha256":"<64 lowercase hex characters>"},{"name":"new-empire-control_1.2.0_linux_arm64.tar.gz","os":"linux","arch":"arm64","size":12345,"sha256":"<64 lowercase hex characters>"}]}
```

Both archive entries are required. Each archive contains exactly one regular
file, `new-empire-control`, a Linux ELF64 executable for the declared architecture.
No directory prefix, links, extra files, or special entries are accepted. Package
documentation, SBOM, and provenance are separate GitHub release assets, outside
the two archive entries. Archive and expanded binary limits are each 512 MiB.

`release.json.sig` is standard padded base64 of an Ed25519 signature of the
**exact manifest bytes**, without a newline. The configured public key is standard
padded base64 of the 32-byte Ed25519 public key. Unknown fields, duplicate keys,
noncanonical JSON/base64, duplicate assets, and unsigned releases fail closed.
Use Go `updates.CanonicalManifest` to produce signing bytes.

## Go API

```go
type Channel string // Stable = "stable" (default), Beta = "beta"
type Options struct {
    InstallDir string
    CurrentVersion string // compiled SemVer, without v
    Schema int // current SQLite schema
    PublicKey string // pinned base64 Ed25519 key; absence disables updates
    Client *http.Client // nil: production netguard client; transport injection for tests only
    Service Service // only required by independent helper methods
    Snapshot func(context.Context, string) error
    Restore func(context.Context, string) error
    HealthTimeout time.Duration // default 60s
}
type Service interface {
    StopPanel(context.Context) error
    RestartPanel(context.Context) error
    Health(context.Context) error
}
func New(Options) (*Manager, error)
func (*Manager) Check(context.Context, Channel) (CheckResult, error)
func (*Manager) Stage(context.Context, string, Channel) (Transaction, error)
func (*Manager) Status(context.Context) (Journal, error)
func (*Manager) Apply(context.Context) error
func (*Manager) Rollback(context.Context) error
func (*Manager) Recover(context.Context) error
func CanonicalManifest(Manifest) ([]byte, error)
func HTTPStatus(error) int
```

`CheckResult` contains `current_version`, `channel`, and nullable `available`
(verified `Manifest`). `Stage` takes an explicit newer version, independently
fetches its official release, verifies its signed manifest and archive, and
persists a staged transaction. It refuses schema changes; those require a manual,
administrator-planned migration. SemVer precedence ignores build metadata.

Only a separately invoked, restricted CLI helper may call `Apply`, `Rollback`,
or `Recover`; never call them in the web process. The helper must use the same
install directory/key/schema as the panel. These methods take a cross-process
lock and never overwrite an existing version binary. There is no shell executor.
`StopPanel` is required before restoring SQLite/config; it must stop **only the
panel**, not Minecraft. `Health` must perform local authenticated readiness
checks, compare the running version/schema with the current symlink/configured
schema, and wait/retry within its context, not trust public liveness.

`Snapshot(ctx, dir)` writes a consistent SQLite backup and all configuration and
credential-encryption material required for restoration into the supplied empty
private directory. It must not mutate live state. `Restore(ctx, dir)` restores
that exact snapshot, replacing SQLite safely (including WAL/SHM handling) and
configuration; it runs only while the panel is stopped. The domain additionally
hash-verifies and syncs every regular snapshot file and snapshots the old binary.
Callbacks must honor context cancellation. They may never access instance/world
data. A snapshot must contain at least one regular file.

The initial install must have `versions/<CurrentVersion>/new-empire-control` and
`current -> versions/<CurrentVersion>`. The install directory, its ancestors, and
`versions/` must be administrator-controlled and not writable by the web process;
only the restricted helper may write versions/symlinks. `updates/` (especially its
journal, lock, and snapshots) is private, helper-controlled, not web-writable, and
excluded from the instance file manager. Route `Stage`/`Status` through the
restricted helper when those permissions require it. `Check` needs no filesystem
write access. Application re-extracts and hashes
the same authenticated stream into protected `versions/.prepared-<id>` storage,
never executes the mutable staging payload. `previous` is atomically maintained. Each transaction,
state transition, diagnostic, and last successful update is durably journaled.
`Status` exposes that journal; HTTP handlers must still enforce administrator
authorization, CSRF, audit logging, and durable job execution.

After the switch, a failed restart/readiness check automatically restores the
previous symlink and verified database/config snapshot, then restarts/checks the
old panel. Cancellation before the switch is safe; recovery after stopping the
panel uses its own bounded context. `Recover` is safe to invoke from the helper
when the panel cannot start: interrupted staging is marked failed, interrupted
application rolls back, and interrupted rollback is retried. Schema-mismatched
snapshots fail closed for manual recovery. No world data is restored.

Online check/stage validates the release against the binary's supported schema
without stopping the panel. At stopped-panel snapshot/install, restore and restart
boundaries, the native adapter holds the existing `panel.lock`, validates the
bounded private main/sidecar files, and opens only root-private copies with
`database.OpenExisting`. It never initializes or migrates state and includes
committed WAL pages. Restore validates installed and snapshot schemas before
replacing files; recovery snapshots must be standalone databases. Restart releases
the lock before invoking systemd. Unsafe, missing, empty or mismatched state
refuses restore/start and retains `manual_recovery` diagnostics.

Reconcile panel update jobs against this filesystem journal after a
database restore: the snapshot may contain a formerly running apply job; do not
blindly resubmit it. Manual rollback reverts panel settings/users/sessions changed
since the snapshot and requires administrator confirmation at the caller.

Errors are English and wrap typed kinds. `HTTPStatus` maps invalid input to 400,
conflicts/downgrades/schema mismatches to 409, verification/upstream errors to
502, cancellation to 408, unavailable configuration/adapters to 503, and local
I/O/recovery failures to 500. Check with no newer release is a successful response
with `available:null`; it is not a fake update success.
