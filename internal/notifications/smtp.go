package notifications

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/smtp"
	"strings"
	"time"

	"github.com/gabrielpgava/newempire-server-control/internal/netguard"
)

// shortcut: only implicit TLS/465 is supported; add STARTTLS when a deployment needs 587.
func dialSMTP(ctx context.Context, host string) (net.Conn, error) {
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(ips) == 0 {
		return nil, errors.New("SMTP public DNS resolution failed")
	}
	for _, ip := range ips {
		if !netguard.Public(ip) {
			return nil, errors.New("SMTP destination resolves to a non-public address")
		}
	}
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	for _, ip := range ips {
		conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), "465"))
		if err != nil {
			continue
		}
		secure := tls.Client(conn, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
		if err := secure.HandshakeContext(ctx); err != nil {
			secure.Close()
			return nil, errors.New("SMTP TLS verification failed")
		}
		return secure, nil
	}
	return nil, errors.New("SMTP connection failed")
}

func (s *Sender) sendSMTP(ctx context.Context, event Event) error {
	conn, err := s.dial(ctx, s.config.SMTPHost)
	if err != nil {
		return deliveryFailure(ctx, "SMTP connection", err)
	}
	defer conn.Close()
	secure, ok := conn.(*tls.Conn)
	if !ok {
		return &DeliveryError{stage: "SMTP verified TLS"}
	}
	state := secure.ConnectionState()
	if !state.HandshakeComplete || state.Version < tls.VersionTLS12 || len(state.VerifiedChains) == 0 {
		return &DeliveryError{stage: "SMTP verified TLS"}
	}
	if len(state.PeerCertificates) == 0 || state.PeerCertificates[0].VerifyHostname(s.config.SMTPHost) != nil {
		return &DeliveryError{stage: "SMTP verified TLS"}
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return deliveryFailure(ctx, "SMTP deadline", err)
		}
	}
	client, err := smtp.NewClient(conn, s.config.SMTPHost)
	if err != nil {
		return deliveryFailure(ctx, "SMTP greeting", err)
	}
	defer client.Close()
	if err := client.Hello("localhost"); err != nil {
		return deliveryFailure(ctx, "SMTP greeting", err)
	}
	if err := client.Auth(smtp.PlainAuth("", s.config.SMTPUsername, s.config.SMTPPassword, s.config.SMTPHost)); err != nil {
		return deliveryFailure(ctx, "SMTP authentication", err)
	}
	if err := client.Mail(s.config.From); err != nil {
		return deliveryFailure(ctx, "SMTP sender", err)
	}
	for _, recipient := range s.config.To {
		if err := client.Rcpt(recipient); err != nil {
			return deliveryFailure(ctx, "SMTP recipient", err)
		}
	}
	data, err := client.Data()
	if err != nil {
		return deliveryFailure(ctx, "SMTP data", err)
	}
	message := "From: " + s.config.From + "\r\nTo: " + strings.Join(s.config.To, ",\r\n ") +
		"\r\nDate: " + event.OccurredAt.UTC().Format(time.RFC1123Z) +
		"\r\nMessage-ID: <" + event.ID + "@newempire.invalid>\r\n" +
		"Subject: New Empire Server Control: " + event.Kind +
		"\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\n" +
		"Auto-Submitted: auto-generated\r\n\r\n" + strings.ReplaceAll(event.text(), "\n", "\r\n")
	if _, err := io.WriteString(data, message); err != nil {
		return deliveryFailure(ctx, "SMTP data", err)
	}
	if err := data.Close(); err != nil {
		return deliveryFailure(ctx, "SMTP acceptance", err)
	}
	// DATA was accepted; a failed QUIT must not schedule duplicate delivery.
	client.Quit()
	return nil
}
