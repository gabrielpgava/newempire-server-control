package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func webhookConfig() Config {
	return Config{Enabled: true, Kind: "webhook", URL: "https://webhook.example.com/events?token=secret-token"}
}

func smtpConfig() Config {
	return Config{Enabled: true, Kind: "smtp", SMTPHost: "example.com", SMTPUsername: "relay-user", SMTPPassword: "smtp-secret-password", From: "panel@example.com", To: []string{"admin@example.com", "owner@example.com"}}
}

func event() Event {
	return Event{ID: strings.Repeat("a", 32), Kind: BackupSucceeded, InstanceID: strings.Repeat("b", 32), JobID: strings.Repeat("c", 32), OccurredAt: time.Date(2026, 11, 2, 12, 30, 0, 0, time.FixedZone("test", 3600))}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// No production configuration bypass: only this test transport rewrites the origin.
func fakeHTTP(t *testing.T, handler http.HandlerFunc) *http.Client {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	target, _ := url.Parse(server.URL)
	transport := server.Client().Transport
	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		copy := req.Clone(req.Context())
		copy.URL.Scheme, copy.URL.Host = target.Scheme, target.Host
		copy.Host = req.URL.Host
		return transport.RoundTrip(copy)
	})}
}

func mustSender(t *testing.T, cfg Config, client *http.Client) *Sender {
	t.Helper()
	sender, err := NewWithHTTPClient(cfg, client)
	if err != nil {
		t.Fatal(err)
	}
	return sender
}

func TestConfigValidation(t *testing.T) {
	for _, test := range []struct {
		name string
		cfg  Config
	}{
		{"http", Config{Enabled: true, Kind: "webhook", URL: "http://example.com/hook"}},
		{"private", Config{Enabled: true, Kind: "webhook", URL: "https://10.0.0.1/hook"}},
		{"loopback", Config{Enabled: true, Kind: "webhook", URL: "https://127.0.0.1/hook"}},
		{"metadata", Config{Enabled: true, Kind: "webhook", URL: "https://169.254.169.254/hook"}},
		{"ipv6", Config{Enabled: true, Kind: "webhook", URL: "https://[::1]/hook"}},
		{"mapped", Config{Enabled: true, Kind: "webhook", URL: "https://[::ffff:127.0.0.1]/hook"}},
		{"userinfo", Config{Enabled: true, Kind: "webhook", URL: "https://user:secret@example.com/hook"}},
		{"port", Config{Enabled: true, Kind: "webhook", URL: "https://example.com:8443/hook"}},
		{"empty port", Config{Enabled: true, Kind: "webhook", URL: "https://example.com:/hook"}},
		{"zoned IP", Config{Enabled: true, Kind: "webhook", URL: "https://[2606:4700:4700::1111%25en0]/hook"}},
		{"fragment", Config{Enabled: true, Kind: "webhook", URL: "https://example.com/hook#secret"}},
		{"empty fragment", Config{Enabled: true, Kind: "webhook", URL: "https://example.com/hook#"}},
		{"bad discord host", Config{Enabled: true, Kind: "discord", URL: "https://discord.com.attacker.example/api/webhooks/123/secret"}},
		{"bad discord path", Config{Enabled: true, Kind: "discord", URL: "https://discord.com/hook"}},
		{"discord query", Config{Enabled: true, Kind: "discord", URL: "https://discord.com/api/webhooks/123/secret?wait=true"}},
		{"unsupported", Config{Enabled: true, Kind: "sms"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.cfg.Validate(); err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatalf("validation or error redaction failed: %v", err)
			}
		})
	}
	for _, edit := range []func(*Config){
		func(c *Config) { c.SMTPHost = "127.0.0.1" },
		func(c *Config) { c.SMTPHost = "example.com:587" },
		func(c *Config) { c.SMTPUsername = "" },
		func(c *Config) { c.SMTPPassword = "password\x00injected" },
		func(c *Config) { c.From = "Panel <panel@example.com>" },
		func(c *Config) { c.From = "panel@example.com\r\nBcc: attacker@example.com" },
		func(c *Config) { c.To = nil },
		func(c *Config) { c.To = []string{"a@example.com", "A@example.com"} },
		func(c *Config) { c.To = []string{"a@example.com\nBcc: attacker@example.com"} },
		func(c *Config) { c.To = make([]string, 11) },
		func(c *Config) { c.URL = "https://example.com/hook" },
	} {
		cfg := smtpConfig()
		edit(&cfg)
		if cfg.Validate() == nil {
			t.Fatalf("invalid SMTP configuration accepted: %+v", cfg.Redacted())
		}
	}
	for _, cfg := range []Config{webhookConfig(), smtpConfig(), {Enabled: true, Kind: "discord", URL: "https://discord.com/api/webhooks/123456789/token_secret"}} {
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(cfg.Redacted())
		if string(encoded) != `{"configured":true}` {
			t.Fatalf("unsafe redaction: %s", encoded)
		}
	}
	if err := (Config{}).Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{}); err == nil {
		t.Fatal("disabled notifications accepted")
	}
	if _, err := NewWithHTTPClient(webhookConfig(), nil); err == nil {
		t.Fatal("nil HTTP client accepted")
	}
}

func TestWebhookAndDiscordPayloads(t *testing.T) {
	for _, kind := range []string{"webhook", "discord"} {
		t.Run(kind, func(t *testing.T) {
			cfg := webhookConfig()
			cfg.Kind = kind
			if kind == "discord" {
				cfg.URL = "https://discord.com/api/webhooks/123456789/secret-token"
			}
			captured := make(chan []byte, 1)
			sender := mustSender(t, cfg, fakeHTTP(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.Header.Get("Content-Type") != "application/json" {
					t.Error("wrong webhook method or media type")
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				captured <- body
				w.WriteHeader(204)
			}))
			if err := sender.Notify(context.Background(), event()); err != nil {
				t.Fatal(err)
			}
			body := <-captured
			if strings.Contains(string(body), "secret-token") || strings.Contains(string(body), "https://") || strings.Contains(string(body), "smtp") {
				t.Fatalf("configuration leaked into event: %s", body)
			}
			var payload map[string]json.RawMessage
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatal(err)
			}
			if kind == "discord" {
				var mentions struct{ Parse []string }
				json.Unmarshal(payload["allowed_mentions"], &mentions)
				if len(payload) != 2 || mentions.Parse == nil || len(mentions.Parse) != 0 || !strings.Contains(string(payload["content"]), event().ID) {
					t.Fatalf("Discord payload invalid: %s", body)
				}
			} else {
				var got struct {
					Event
					Message string
				}
				if err := json.Unmarshal(body, &got); err != nil {
					t.Fatal(err)
				}
				if got.ID != event().ID || got.Kind != BackupSucceeded || got.Message != "Local backup completed." || !got.OccurredAt.Equal(event().OccurredAt) || !strings.Contains(string(body), "11:30:00Z") {
					t.Fatalf("generic webhook payload invalid: %s", body)
				}
			}
		})
	}
}

func TestInvalidEventsNeverReachTransport(t *testing.T) {
	sender := mustSender(t, webhookConfig(), &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("invalid event reached transport")
		return nil, errors.New("unexpected")
	})})
	for _, edit := range []func(*Event){
		func(e *Event) { e.ID = "secret-rcon-password" },
		func(e *Event) { e.InstanceID = "@everyone" },
		func(e *Event) { e.JobID = "\r\nBcc: secret@example.com" },
		func(e *Event) { e.Kind = "secret RCON console output" },
		func(e *Event) { e.OccurredAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) },
	} {
		e := event()
		edit(&e)
		if err := sender.Notify(context.Background(), e); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("event validation or redaction: %v", err)
		}
	}
	for _, kind := range []string{Test, BackupSucceeded, BackupFailed, OffsiteSucceeded, OffsiteFailed, RestoreSucceeded, RestoreFailed, JobFailed, ServerStarted, ServerStopped, ServerCrashed, UpdateAvailable, UpdateSucceeded, UpdateFailed} {
		e := event()
		e.Kind = kind
		if err := e.Validate(); err != nil || eventMessage(kind) == "" {
			t.Fatalf("missing fixed message for %q: %v", kind, err)
		}
	}
}

func TestSingleAttemptStatusesAndRedactedErrors(t *testing.T) {
	for _, test := range []struct {
		status    int
		retryable bool
	}{
		{202, false}, {204, false}, {301, false}, {401, false}, {408, true}, {429, true}, {500, true},
	} {
		var calls atomic.Int32
		sender := mustSender(t, webhookConfig(), fakeHTTP(t, func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(test.status)
			io.WriteString(w, "secret-token at https://credential-destination.example.com")
		}))
		err := sender.Notify(context.Background(), event())
		if calls.Load() != 1 {
			t.Fatal("adapter retried outside the durable queue")
		}
		if test.status < 300 {
			if err != nil {
				t.Fatal(err)
			}
			continue
		}
		var failure *DeliveryError
		if !errors.As(err, &failure) || failure.Code != test.status || failure.Retryable != test.retryable || failure.RetryAfter != 2*time.Minute || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "https:") {
			t.Fatalf("incorrect delivery result for %d: %+v", test.status, err)
		}
	}
	sender := mustSender(t, webhookConfig(), &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("secret-token at https://credential-destination.example.com")
	})})
	err := sender.Notify(context.Background(), event())
	var failure *DeliveryError
	if !errors.As(err, &failure) || !failure.Retryable || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "https:") {
		t.Fatalf("transport error leaked: %v", err)
	}
}

func TestRedirectsRefusedAndCancellationPreserved(t *testing.T) {
	for _, destination := range []string{"https://127.0.0.1/private", "https://169.254.169.254/latest", "https://webhook.example.com/other", "http://webhook.example.com/other"} {
		var calls atomic.Int32
		sender := mustSender(t, webhookConfig(), fakeHTTP(t, func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.Header().Set("Location", destination)
			w.WriteHeader(307)
		}))
		if err := sender.Notify(context.Background(), event()); err == nil || calls.Load() != 1 {
			t.Fatalf("redirect followed: calls=%d err=%v", calls.Load(), err)
		}
	}
	sender := mustSender(t, webhookConfig(), fakeHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if err := sender.Notify(ctx, event()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("delivery deadline ignored: %v", err)
	}
}

func TestRetryAfterBounded(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, test := range []struct {
		raw  string
		want time.Duration
	}{
		{"2", 2 * time.Second}, {"999999999", time.Hour}, {"-1", 0}, {"invalid", 0},
		{now.Add(5 * time.Minute).Format(http.TimeFormat), 5 * time.Minute},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0},
	} {
		if got := retryAfter(test.raw, now); got != test.want {
			t.Fatalf("retry delay %q = %v, want %v", test.raw, got, test.want)
		}
	}
}
