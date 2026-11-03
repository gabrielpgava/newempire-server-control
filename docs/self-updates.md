# Verified panel updates

The `internal/updates` domain supports signed, same-schema updates of the panel
only. HTTP authorization/jobs, the independent CLI helper, systemd integration,
SQLite/configuration adapters, deployment permissions, and release publication
are integration responsibilities. Availability in an installed panel depends on
those adapters being wired and tested; this document does not assert a published
release exists.

## Trust and release policy

- Repository: **gabrielpgava/newempire-server-control**, fixed in code.
- Default channel: **stable**, excluding draft and prerelease releases. Selecting
  **beta** explicitly includes SemVer prereleases as well as stable releases.
- The updater compares SemVer precedence, not publication time or lexical order.
  Build metadata never makes an equal version newer. Downgrades are rejected;
  explicit rollback is a separate operation.
- A pinned Ed25519 public key is mandatory. Missing/invalid keys disable updates.
  Keys are canonical padded standard base64 of the raw 32-byte public key. Keep
  the key/configuration administrator-controlled. There is no unsigned fallback.
- Each release has signed `release.json`, base64 `release.json.sig`, and archives
  named `new-empire-control_<version>_linux_amd64.tar.gz` and
  `new-empire-control_<version>_linux_arm64.tar.gz`.
- The signed manifest binds repository, version, database schema, architecture,
  compressed size, and SHA-256. Canonical JSON, exact raw-byte signature checks,
  official asset URLs, bounded download/extraction, and ELF architecture checks
  are mandatory. Unknown or ambiguous manifest fields are rejected.
- Only HTTPS GitHub API/release hosts and their explicit asset CDN hosts are
  allowed. The production client rejects private/non-public destination addresses
  and unsafe redirects. Never inject an unrestricted production HTTP transport.

The complete signing wire format and Go integration API are in
[`internal/updates/API.md`](../internal/updates/API.md). SBOM and provenance are
separate release assets; the updater authenticates the manifest through its pinned
Ed25519 key, not through an unsigned checksum or unauthenticated provenance file.

## Layout and prerequisites

```text
<install>/
  current -> versions/1.0.0
  previous -> versions/0.9.0
  versions/<version>/new-empire-control
  updates/
    lock
    journal.json
    staging/<transaction-id>/release.json
    staging/<transaction-id>/release.json.sig
    staging/<transaction-id>/archive.tar.gz
    staging/<transaction-id>/payload/new-empire-control
    snapshots/<transaction-id>/binary
    snapshots/<transaction-id>/snapshot.json
    snapshots/<transaction-id>/data/...
```

The example versions are illustrative. Initial installation must create the
versioned executable and relative `current` symlink. The install directory,
ancestors, and `versions/` must be administrator-controlled real directories,
never group/world writable or writable by the unprivileged web process. Use
`0755` version directories and `0555` installed executables. The web process must
not run as root. Only the restricted helper may mutate `versions/`, `current`,
`previous`, or updater snapshots/journal/lock. Route staging and private status
reads through that helper rather than granting the web account write access to
updater state. Release checks themselves need no filesystem write access.

Updater state/snapshots use private `0700` directories and `0600` files, outside
instance roots and excluded from the instance file manager. Snapshots contain
credentials and encryption material: do not expose them as ordinary downloads.
All updater processes must share the same filesystem lock and journal. Local
filesystems must support advisory locking, atomic rename, directory fsync, and
relative symlinks; do not assume those semantics on network filesystems.
The native installer creates `updates/` as **root:root 0700**, not panel-owned.
The root updater rejects a journal, lock, staging directory, snapshot directory,
or file that is not controlled by its account before trusting that path. Existing
panel-owned updater state is not migrated by chown: preserve it outside the
active install, review it offline, and provision a fresh root-owned updater
directory. Never promote an untrusted old journal or snapshot into trusted state.

The panel reads only the bounded, root-owned `panel-update-report.json` in its
own private data directory; it does not open the private updater journal. Reports
are explicitly chmodded to `0644` even under the oneshot's `0077` umask. They bind
the request/job, operation and selected version to the journal result. Missing
first-run reports mean idle; invalid/unreadable reports mean unavailable, not a
successful or unknown update. Snapshot secrets remain in root-private storage.
After restoration, reconcile durable panel-update jobs with the updater journal;
the restored SQLite snapshot may contain an interrupted apply job that must not
be blindly resubmitted.
Reconciliation uses the root report, only finishes interrupted jobs, and never
finishes an automatic check from a successful intermediate staging report.

## Check, stage, apply

The default policy is **notify only**, stable channel, a 24-hour check interval,
and a `02:00`–`04:00 UTC` maintenance window. Only the owner may opt into
`automatic` mode. In that mode a due or manually requested check can proceed to
verified staging/application when the policy, window and postponement permit it;
otherwise it only checks/notifies. Saving ordinary policy fields preserves an
existing postponement atomically. A direct policy API caller can explicitly
replace its timestamp or clear it with zero. Explicit manual stage/apply are not
deferred by the automatic maintenance window or postponement, and manual
apply/rollback still require confirmation.

1. **Check** reads eligible releases through the official GitHub API, chooses the
   highest newer SemVer, and authenticates its manifest. No newer release returns
   `available: null`; an unsigned latest release is an error, not a silent fallback.
2. **Stage** takes an explicit version/channel, re-fetches its official release,
   verifies signature/schema/archive hash and architecture, and records a durable
   `staged` transaction. A running Minecraft server is not stopped or modified.
3. A **separate restricted helper process** applies the staged transaction. The
   web process must only submit the durable job/helper request, never apply in
   place. The helper verifies the signed manifest again and hashes the exact
   compressed stream it extracts into administrator-controlled temporary storage
   under `versions/`, avoiding a staging-file hash/use race.
4. The helper stops **only the panel**, snapshots its preserved executable,
   consistent SQLite database, configuration, and encryption material, syncs and
   hashes the snapshot, installs a new version directory, and atomically updates
   `previous` and `current`. An existing version directory is never overwritten.
5. The helper restarts only the panel and invokes local **authenticated readiness**
   within the configured health timeout (60 seconds by default, at most five
   minutes). The adapter must wait/retry as needed and verify the running version,
   database schema, and readiness; public `/healthz` liveness is not sufficient.
6. Failed restart/readiness automatically restores the previous version symlink
   and verified database/configuration snapshot, then restarts/checks the old
   panel. The operation still returns an error explaining that it rolled back.

Automatic maintenance reuses a matching staged version/channel for the current
installation, including after postponement. The root staging adapter also reuses
that transaction if a report was lost; it does not download another identical
artifact. Immediately before apply, the panel reloads the policy and checks the
channel, maintenance window and postponement deadline again. Disabling or
postponing maintenance during staging leaves the verified stage unapplied.

A failed automatic stage/apply latches that version until administrator review
and a successful explicit manual stage. A root failure report restores the latch
after a panel database rollback/restart. A different newer release is not blocked
by the old version's latch. Retained failed installation directories still
require the offline review described below; retry never overwrites them.

Apply/recovery never invokes shell commands or a Minecraft runtime adapter. The
service adapter must target a fixed panel unit and may not accept arbitrary unit
names, paths, or commands from a request.

## Schema, cancellation, and progress

Online checks/staging compare the signed release schema with the running binary's
supported schema; they do not stop the panel or open its live database as root.
After stopping the panel, snapshot/install, restore, and every helper-controlled
restart require the actual SQLite **schema to be identical** to that supported
schema. While holding the existing `panel.lock`, the helper validates bounded,
private database/sidecar files and opens only a root-private copy with
`database.OpenExisting`, preserving committed WAL without initialization or
migrations. Restore checks both installed state and the verified standalone
snapshot before replacing files. The lock is released before service startup.

Missing, empty, unsafe, or mismatched state prevents automatic restore/restart and
leaves the panel stopped with `manual_recovery` diagnostics rather than letting
normal startup initialize or migrate it. Schema-changing upgrades need an
administrator-planned manual migration and recovery procedure; changing a flag or
lying about the schema is not a supported bypass.

Cancellation is honored during network reads, verification, and pre-switch
snapshotting. If the panel was already stopped, it is restarted before reporting
cancellation only when the stopped-state schema guard permits it; otherwise it
remains stopped for manual recovery. Once a verified snapshot is ready and installation
begins, the helper finishes installation or recovery using a separate bounded
context; killing that process is not safe cancellation.
Snapshot/restore adapters require the existing regular, single-link `panel.lock`
owned by the panel data-directory account. They never create it as root. A missing
or unsafe lock aborts before copying/restoring state; a normal panel start creates
a missing lock as the panel account. Review and repair a legacy root-owned lock
offline rather than making it broadly writable.

`Status` returns the durable transaction, last successful update, and transition
history. States include `downloading`, `staged`, `stopping`, `snapshotting`,
`installing`, `checking_health`, `complete`, `rolling_back`, `rolled_back`,
`failed`, and `manual_recovery`. Progress is phase-based, not an invented percent.
Errors distinguish invalid requests (400), conflicts/schema/downgrade (409),
untrusted/upstream releases (502), disabled/missing adapters (503), canceled or
timed-out work (408), and local/recovery failures (500).

## Retention and limitations

Previous binaries, staging, failed versions, history, and snapshots are retained;
the domain does not automatically delete recovery evidence. Monitor free space.
After a failed install, do not overwrite its existing version directory to retry:
an administrator must inspect the journal/current symlink, retain the needed
snapshot and previous binary, and deliberately remove only an unused failed
version. This updater supports Linux amd64/arm64 archives only, not Docker image
updates or cross-schema automatic migrations.

Use the offline helper recovery procedure in
[`disaster-recovery.md`](disaster-recovery.md) when the panel cannot start.
