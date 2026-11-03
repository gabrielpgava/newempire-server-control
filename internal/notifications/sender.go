package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/textproto"
	"strconv"
	"time"

	"github.com/gabrielpgava/newempire-server-control/internal/netguard"
)

const deliveryTimeout = 30 * time.Second

// SMTPDialer returns a connection that has already completed verified implicit TLS.
type SMTPDialer func(context.Context, string) (net.Conn, error)

type Sender struct {
	config Config
	http   *http.Client
	dial   SMTPDialer
}

// DeliveryError contains only redacted diagnostics and scheduling hints.
type DeliveryError struct {
	Code       int
	Retryable  bool
	RetryAfter time.Duration
	stage      string
}

func (e *DeliveryError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("notification %s failed (status %d)", e.stage, e.Code)
	}
	return "notification " + e.stage + " failed"
}

func New(cfg Config) (*Sender, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, errors.New("notifications are disabled")
	}
	if cfg.Kind == "smtp" {
		return NewWithSMTPDialer(cfg, dialSMTP)
	}
	u, _ := webhookURL(cfg.URL)
	return NewWithHTTPClient(cfg, netguard.New(u.Hostname()))
}

// NewWithHTTPClient trusts the supplied transport; production callers must use New.
func NewWithHTTPClient(cfg Config, client *http.Client) (*Sender, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if !cfg.Enabled || cfg.Kind == "smtp" || client == nil {
		return nil, errors.New("an enabled HTTP notification configuration and client are required")
	}
	copy := *client
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if copy.Timeout <= 0 || copy.Timeout > deliveryTimeout {
		copy.Timeout = deliveryTimeout
	}
	return &Sender{config: cfg, http: &copy}, nil
}

// NewWithSMTPDialer trusts the supplied TLS dialer; production callers must use New.
func NewWithSMTPDialer(cfg Config, dial SMTPDialer) (*Sender, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if !cfg.Enabled || cfg.Kind != "smtp" || dial == nil {
		return nil, errors.New("an enabled SMTP notification configuration and TLS dialer are required")
	}
	cfg.To = append([]string(nil), cfg.To...)
	return &Sender{config: cfg, dial: dial}, nil
}

func (s *Sender) Notify(ctx context.Context, event Event) error {
	if err := event.Validate(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, deliveryTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now().UTC()
	} else {
		event.OccurredAt = event.OccurredAt.UTC()
	}
	if s.config.Kind == "smtp" {
		return s.sendSMTP(ctx, event)
	}
	var payload any
	if s.config.Kind == "discord" {
		payload = map[string]any{"content": event.text(), "allowed_mentions": map[string]any{"parse": []string{}}}
	} else {
		payload = struct {
			Event
			Message string `json:"message"`
		}{Event: event, Message: eventMessage(event.Kind)}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return errors.New("notification payload could not be encoded")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.config.URL, bytes.NewReader(encoded))
	if err != nil {
		return errors.New("notification request could not be constructed")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "NewEmpireServerControl")
	resp, err := s.http.Do(req)
	if err != nil {
		return deliveryFailure(ctx, "HTTP delivery", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return &DeliveryError{
		Code: resp.StatusCode, Retryable: resp.StatusCode == 408 || resp.StatusCode == 429 || resp.StatusCode >= 500,
		RetryAfter: retryAfter(resp.Header.Get("Retry-After"), time.Now()), stage: "HTTP delivery",
	}
}

func retryAfter(raw string, now time.Time) time.Duration {
	var delay time.Duration
	if seconds, err := strconv.ParseInt(raw, 10, 32); err == nil {
		if seconds > 0 {
			delay = time.Duration(seconds) * time.Second
		}
	} else if at, err := http.ParseTime(raw); err == nil {
		delay = at.Sub(now)
	}
	if delay < 0 {
		return 0
	}
	if delay > time.Hour {
		return time.Hour
	}
	return delay
}

func deliveryFailure(ctx context.Context, stage string, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// A socket deadline can expire before the context timer publishes its error.
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	failure := &DeliveryError{stage: stage, Retryable: true}
	var reply *textproto.Error
	if errors.As(err, &reply) {
		failure.Code = reply.Code
		failure.Retryable = reply.Code >= 400 && reply.Code < 500
	}
	return failure
}
