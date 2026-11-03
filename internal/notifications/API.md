# Notification adapter API

```go
cfg := notifications.Config{Enabled: true, Kind: "discord", URL: webhookURL}
err := cfg.Validate() // Also accepts a disabled configuration.
sender, err := notifications.New(cfg)
summary := cfg.Redacted() // Only {configured: bool}.
err = sender.Notify(ctx, notifications.Event{
    ID: notificationID, Kind: notifications.BackupSucceeded,
    InstanceID: instanceID, JobID: jobID, OccurredAt: time.Now().UTC(),
})
```

Flat configuration JSON matches settings: `enabled`, `kind` (`discord`, `webhook`, `smtp`), `url`, `smtp_host`, `smtp_username`, `smtp_password`, `from`, `to` (array). Encrypt the entire configuration with the parent Vault; return only `Redacted()` to clients. `New` rejects disabled configurations.

`Event` contains `ID`, `Kind`, optional `InstanceID`, optional `JobID`, and `OccurredAt`. IDs must be repository 32-character lowercase hexadecimal IDs. A zero occurrence time is filled at delivery; prefer persisting the original time in the queue. Kinds are `Test`, `BackupSucceeded`, `BackupFailed`, `OffsiteSucceeded`, `OffsiteFailed`, `RestoreSucceeded`, `RestoreFailed`, `JobFailed`, `ServerStarted`, `ServerStopped`, `ServerCrashed`, `UpdateAvailable`, `UpdateSucceeded`, `UpdateFailed`. Their values use snake_case (test is `test`). Payloads use fixed English messages and IDs only: do not enqueue logs, exception strings, RCON output, names, credentials or destinations.

`Notify` performs exactly one bounded delivery attempt. The parent owns the durable queue, attempts/next_attempt, per-event rate limits and retry backoff. `*DeliveryError` exposes `Code int`, `Retryable bool` and bounded `RetryAfter time.Duration`; its error string is redacted. `errors.Is` recognizes context cancellation/deadlines. Generic webhooks receive the event plus its fixed `message`; Discord receives `content` with mentions disabled. Any 2xx means HTTP acceptance, not human receipt.

SMTP supports implicit TLS on **465 only**, authenticated PLAIN over verified TLS 1.2+, one sender and 1–10 recipients. Successful delivery means relay acceptance after DATA, not mailbox receipt. STARTTLS/587, internal relays and unauthenticated relays are not implemented.

Production `New` uses `internal/netguard.New` for HTTPS 443 and a public-DNS-checked TLS SMTP dialer. Redirects are always refused. Internal/private/link-local destinations and DNS rebinding to them are rejected at connection time. No internal-network approval or bypass setting exists.

Explicit trusted injection constructors are `NewWithHTTPClient(cfg, *http.Client)` for HTTP fake clients and `NewWithSMTPDialer(cfg, SMTPDialer)` for an already authenticated TLS connection. `SMTPDialer` has signature `func(context.Context, string) (net.Conn, error)` (the string is the configured hostname, port is always 465). Production code must use `New`; injected transports/dialers must never come from user settings.
