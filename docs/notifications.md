# Operational notifications

The `internal/notifications` adapter sends real HTTP webhook or authenticated SMTP deliveries. It has no database dependency: the application owns the encrypted configuration and durable notification queue, including `attempts`, `next_attempt`, per-event rate limits and backoff. See [the package API](../internal/notifications/API.md) for exported constructors and event fields.

## Destinations and configuration

Supported configurations use these flat fields:

```json
{"enabled":true,"kind":"discord","url":"https://discord.com/api/webhooks/WEBHOOK_ID/WEBHOOK_TOKEN"}
```

```json
{"enabled":true,"kind":"webhook","url":"https://notifications.example.com/events?token=DESTINATION_TOKEN"}
```

```json
{
  "enabled": true,
  "kind": "smtp",
  "smtp_host": "smtp.example.com",
  "smtp_username": "panel-account",
  "smtp_password": "PROVIDE_THROUGH_OWNER_SETTINGS",
  "from": "panel@example.com",
  "to": ["admin@example.com"]
}
```

These are examples, not working credentials. Discord requires an official `discord.com/api/webhooks/ID/TOKEN` URL without a query or fragment. Generic webhooks may use a token in the HTTPS URL but custom authentication headers, request signatures and arbitrary payload templates are not implemented.

Enabled configuration must pass `Config.Validate()` before saving. Disabled configuration may be empty. Parsing validates fields and rejects non-public IP literals; DNS, certificate verification and credentials are checked at delivery time. `Redacted()` exposes only `{"configured":true|false}`. It means valid enabled configuration, not successful delivery.

The parent must encrypt the **entire configuration** with the Vault, restrict credential changes to the owner, and never echo saved credentials, webhook paths, SMTP hosts/recipients or provider response bodies in settings JSON, audit details, queue diagnostics or browser errors. The adapter's errors contain a fixed delivery stage and optional numeric status only.

### Network restrictions

- Webhooks use HTTPS **443 only**. SMTP uses implicit TLS/SMTPS **465 only**; do not append a port to `smtp_host`.
- All resolved addresses must be public at connection time. Loopback, private, link-local, metadata-service and other non-public networks are rejected. Dialers connect to the checked IP rather than resolving the hostname a second time. A hostname changing from a public address to a private one is rejected on the next connection.
- Every HTTP redirect is refused, including same-host redirects. Supply the final HTTPS webhook URL.
- SMTP requires a trusted certificate for the configured hostname and TLS 1.2 or newer. The host needs the appropriate public CA trust store. AUTH PLAIN is used **inside verified TLS**; plaintext authentication is never used.
- There is no internal-network approval/bypass setting. Internal relays, STARTTLS/587, unauthenticated SMTP, custom CA settings and custom ports are not implemented.
- SMTP supports one sender and 1–10 distinct recipients, using plain ASCII mailbox addresses without display names. Credentials must be nonempty ASCII without control characters or surrounding whitespace.

Production must call `New`. Explicit `NewWithHTTPClient` and `NewWithSMTPDialer` constructors are trusted code-level test seams and must never be exposed through settings. Injected SMTP connections are also checked for completed verified TLS and the configured peer hostname.

## Event contents

Events carry only a queue ID, a supported kind, optional instance/job IDs and the occurrence time. IDs must be 32 lowercase hexadecimal characters. The adapter renders fixed English messages; it does not accept free-form text, logs, names, exception strings, RCON output, file paths, credentials or destination links. Persist the original occurrence time in the queue; a missing time defaults to delivery time.

Supported kinds:

- `test`
- `backup_succeeded`, `backup_failed`
- `offsite_succeeded`, `offsite_failed`
- `restore_succeeded`, `restore_failed`
- `job_failed`
- `server_started`, `server_stopped`, `server_crashed`
- `update_available`, `update_succeeded`, `update_failed`

The parent must emit events only after the corresponding actual state change. In particular, local backup completion is not offsite transfer completion, and an offsite success message describes remote metadata matching, not an independent download check. Failed jobs direct the user to authenticated panel diagnostics rather than sending raw failure text to a third party.

Generic webhooks receive this JSON structure:

```json
{
  "id": "11111111111111111111111111111111",
  "kind": "backup_succeeded",
  "instance_id": "22222222222222222222222222222222",
  "job_id": "33333333333333333333333333333333",
  "occurred_at": "2026-11-02T12:30:00Z",
  "message": "Local backup completed."
}
```

Optional IDs are omitted when absent. Discord receives a concise `content` string containing the same fixed information and `allowed_mentions: {"parse":[]}`. SMTP sends a plain-text MIME message, with validated envelope/header addresses, a fixed subject based on event kind, a UTC date, `Auto-Submitted: auto-generated`, and a stable Message-ID derived from the event ID. Neither body format includes credential destinations or secrets.

## Delivery, retries and rate limiting

`Notify(ctx, event)` performs **one attempt**, bounded by 30 seconds including DNS, TLS and SMTP exchange. Context cancellation closes an active SMTP connection and is recognizable with `errors.Is(err, context.Canceled)` or `context.DeadlineExceeded`.

- Any HTTP 2xx is acceptance by the configured endpoint, not proof a human saw the message.
- SMTP success means the relay accepted DATA, not final mailbox delivery. A failed QUIT after DATA acceptance is not retried as a failed notification.
- `*DeliveryError` exposes `Code` (HTTP or SMTP status), `Retryable`, and `RetryAfter`. HTTP 408/429/5xx and transport failures are retry candidates. HTTP redirects and ordinary 4xx are not. SMTP 4xx replies are transient; SMTP 5xx replies are permanent. Unknown connection/protocol failures remain retry candidates subject to the parent's finite retry limit.
- HTTP `Retry-After` seconds or dates are parsed and clamped to at most one hour; invalid/past values become zero. This is a scheduling hint, never an internal sleep/retry loop.
- The parent queue must impose a finite attempt limit, exponential backoff, per-event/destination rate limits, durable state changes and an actionable terminal failure. Failed configuration or missing Vault material must not be marked sent.

Retries after an ambiguous network acknowledgement may duplicate a delivery. The stable generic-webhook event ID and SMTP Message-ID can help downstream deduplication but do not provide exactly-once delivery; Discord does not promise deduplication. Do not claim delivery merely because a configuration is saved or a queue row is inserted.

## Tests and deployment checks

```sh
go test -race -count=1 -timeout=90s ./internal/notifications ./internal/netguard
```

Tests exercise real local TLS HTTP and SMTP protocol exchanges via explicit injected clients/dialers. Coverage includes payload/ID/header validation, SMTP authentication and envelope failures, DATA rejection, failed QUIT after acceptance, unverified SMTP connections, timeout cancellation, retry classification, destination/error redaction, redirect refusal, private-address rejection and DNS rebinding checks for the production constructors.

No live Discord endpoint or external SMTP account is contacted. Verify a real test notification, spam/relay policy, credentials, CA trust and recipient delivery in the deployment. The application integration must additionally test encrypted settings, queue persistence/crash recovery, rate limiting and event generation; those responsibilities are outside this standalone adapter.
