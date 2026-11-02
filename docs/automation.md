# Jobs, schedules, and update policies

Automation uses the same durable jobs and instance mutation locks as manual operations. A saved schedule is not evidence that its last job succeeded.

## Follow jobs

Authenticated `GET /api/v1/jobs` lists visible jobs, newest first, up to 200. `GET /api/v1/jobs/{jobID}` returns `{job,steps}`. Each step has Unix-second `time`, `progress`, and `message` fields. Job states are:

| State | Meaning |
| --- | --- |
| `queued` | Saved, waiting for execution/locks |
| `running` | Handler executing; inspect its current step |
| `succeeded` | Completed; read the operation-specific result |
| `failed` | Failed; inspect `error` and retained recovery data |
| `cancelled` | Cancelled while safe to do so |
| `interrupted` | Panel stopped during execution; manual review required |

HTTP **202 means accepted**, not success. Job progress reports handler checkpoints, not a continuously measured time estimate. Console output appears in a successful job's `result.output`; a newly created instance appears in `result.instance`.

An admin may request `POST /api/v1/jobs/{jobID}/cancel`. `{ "ok": true }` means the request was accepted; refresh the job for its final state. A commit barrier sets `cancellable:false` before irreversible work. Cancellation after that barrier, or of a terminal job, returns HTTP 409. Never promise an immediate stop or kill a process to simulate cancellation.

After panel restart, jobs that were running are marked interrupted instead of blindly replaying a potentially destructive operation. Inspect them and their retained staging/restore points before retrying. Only handlers explicitly registered as safely retryable receive automatic retries.

The enqueueing API supports `Idempotency-Key` for identical repeated requests within a user's namespace. Reusing a key for different work is rejected. Persist the job ID and use a distinct key for distinct operations; idempotency is not a reason to skip reviewing a restore or a stale plan.

## Schedules

Admins/owners create or replace schedules with `POST /api/v1/schedules`. Omit `id` to create one; include a saved ID to update it. A concrete daily backup schedule is:

```json
{
  "instance_id":"0123456789abcdef0123456789abcdef",
  "kind":"backup",
  "hour":4,
  "minute":0,
  "weekdays":[0,1,2,3,4,5,6],
  "timezone":"UTC",
  "enabled":true
}
```

- Kinds are **`backup`**, **`restart`**, and **`mod_check`**, not arbitrary commands or cron expressions.
- `weekdays` uses Sunday `0` through Saturday `6`; choose at least one.
- Use an IANA timezone, such as `UTC` or `America/New_York`, not a browser-local implicit offset.
- `next_run` is an absolute Unix-second timestamp. `last_job_id` links to the actual execution record.
- Scheduling scans real instants. A nonexistent DST wall time is skipped; a repeated wall time can match twice. Review timezone choices for disruptive work.
- On downtime, older missed occurrences are coalesced into one catch-up run per due schedule. This is not a full historical replay.
- The scheduler checks roughly every 30 seconds; queued work may wait longer for instance locks. It is not a real-time deadline service.

`GET /api/v1/schedules` is filtered by instance access. `DELETE /api/v1/schedules/{scheduleID}` stops future runs; it does not delete previous jobs or backups and is not a cancellation request for an already queued job. A scheduled restart can disconnect players. Schedule checks do not automatically opt an instance into applying mod updates.

## Mod update policy

Read or save `/api/v1/instances/{instanceID}/mods/policy`. The fields are `mode`, `interval_hours` (1–168), `window_start`, `window_end`, `timezone`, and `postpone_players`.

- `manual` is the default: no automatic checks/applications.
- `notify` checks for compatible changes without silently applying them.
- `automatic-safe` is explicit opt-in to compatible managed-content updates, constrained by a maintenance window and the safety plan.

The default policy uses a 24-hour interval, `02:00`–`04:00` in `UTC`, with player postponement enabled. Windows use `HH:MM`, inclusive start and exclusive end. A window crossing midnight is supported; equal endpoints mean the entire day. Player postponement does not replace a verified backup or prove that a mod is safe for your world. Pins and unknown/external content must remain respected. Inspect blocked plans and job failures instead of bypassing them.

## Panel update policy

Owners save `PUT /api/v1/updates/policy` with `mode` (`disabled`, `notify`, or `automatic`), `channel` (`stable` or `beta`), `interval_hours`, `window_start`, `window_end`, and `timezone`. Panel policy is independent of an instance's mod policy.

Check/stage/apply/rollback operations return jobs. Automatic mode is not an authorization to accept unsigned binaries, bypass the configured official identity, restart Minecraft, or restore worlds. A missing signing key/helper must fail clearly. No published version or end-to-end updater acceptance is confirmed; keep automatic application disabled until signed publication and real-host rollback have been verified.

An owner can defer automatic application with `POST /api/v1/updates/postpone` and `{"hours":24}` (1–168 hours). This returns the saved policy synchronously, including `postpone_until` in Unix seconds; it is not a queued update or a cancellation of already-started work.

## Notifications, retention, and storage

Owners configure integrations through `PUT /api/v1/settings`:

- `notifications`: `enabled`, `kind` (`discord`, `webhook`, `smtp`), and the matching destination/credential fields. Webhook URLs can contain secrets. SMTP uses `smtp_host`, `smtp_username`, `smtp_password`, `from`, and `to`.
- `storage`: `enabled`, `endpoint`, `region`, `bucket`, `access_key`, and `secret_key` for S3-compatible offsite copies.
- `backup_retention`: `enabled` and `keep_last` for explicitly enabled retention.

Settings reads return only integration `configured` flags, never saved credentials. Supply complete credential configuration when replacing an integration; do not round-trip a redacted GET as a credential update. Public HTTPS destinations and provider restrictions are enforced; a private SMTP or object-storage endpoint is not silently allowed. Configuration success is not proof of actual delivery or recoverability. Retention is destructive: verify independent/offsite recovery before enabling it, and inspect its jobs/errors.

The authenticated `/api/v1/events` SSE stream emits periodic `change` invalidations. It is not a durable event queue or a notification-delivery receipt; reconnect and fetch current jobs/state after a disconnection. See [acceptance](acceptance.md) for live provider and crash-recovery verification still required.
