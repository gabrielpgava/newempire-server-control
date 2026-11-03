package notifications

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"net/textproto"
	"os"
	"strings"
	"testing"
	"time"
)

type smtpCapture struct {
	commands []string
	message  string
	auth     string
	err      error
}

func smtpFixture(t *testing.T, mode string) (SMTPDialer, <-chan smtpCapture) {
	t.Helper()
	certServer := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	serverTLS := certServer.TLS.Clone()
	clientTLS := certServer.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	clientTLS.ServerName = "example.com"
	certServer.Close()
	listener, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	captured := make(chan smtpCapture, 1)
	go func() {
		var capture smtpCapture
		defer func() { captured <- capture }()
		conn, err := listener.Accept()
		if err != nil {
			capture.err = err
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		if mode == "stall" {
			if err := conn.(*tls.Conn).Handshake(); err != nil {
				capture.err = err
				return
			}
			io.Copy(io.Discard, conn)
			return
		}
		protocol := textproto.NewConn(conn)
		protocol.PrintfLine("220 example.com ESMTP")
		for {
			line, err := protocol.ReadLine()
			if err != nil {
				if !errors.Is(err, io.EOF) {
					capture.err = err
				}
				return
			}
			capture.commands = append(capture.commands, line)
			switch {
			case strings.HasPrefix(line, "EHLO "):
				protocol.PrintfLine("250-example.com\r\n250-AUTH PLAIN\r\n250 SIZE 100000")
			case strings.HasPrefix(line, "AUTH PLAIN "):
				decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(line, "AUTH PLAIN "))
				if err != nil {
					capture.err = err
					return
				}
				capture.auth = string(decoded)
				if mode == "auth_failure" {
					protocol.PrintfLine("535 smtp-secret-password authentication failed")
				} else {
					protocol.PrintfLine("235 authenticated")
				}
			case strings.HasPrefix(line, "MAIL FROM:"):
				protocol.PrintfLine("250 sender accepted")
			case strings.HasPrefix(line, "RCPT TO:"):
				if mode == "recipient_failure" {
					protocol.PrintfLine("550 secret destination recipient rejected")
				} else {
					protocol.PrintfLine("250 recipient accepted")
				}
			case line == "DATA":
				protocol.PrintfLine("354 send message")
				message, err := io.ReadAll(protocol.DotReader())
				if err != nil {
					capture.err = err
					return
				}
				capture.message = string(message)
				if mode == "data_failure" {
					protocol.PrintfLine("451 smtp-secret-password temporary failure")
				} else {
					protocol.PrintfLine("250 queued")
				}
			case line == "QUIT":
				if mode != "quit_failure" {
					protocol.PrintfLine("221 closing")
				}
				return
			case line == "*":
				protocol.PrintfLine("501 cancelled")
			default:
				protocol.PrintfLine("500 unsupported")
			}
		}
	}()
	dial := func(ctx context.Context, host string) (net.Conn, error) {
		if host != "example.com" {
			return nil, errors.New("unexpected SMTP hostname")
		}
		dialer := &tls.Dialer{Config: clientTLS}
		return dialer.DialContext(ctx, "tcp", listener.Addr().String())
	}
	return dial, captured
}

func captureSMTP(t *testing.T, captured <-chan smtpCapture) smtpCapture {
	t.Helper()
	select {
	case capture := <-captured:
		if capture.err != nil {
			t.Fatal(capture.err)
		}
		return capture
	case <-time.After(4 * time.Second):
		t.Fatal("SMTP fixture did not finish")
		return smtpCapture{}
	}
}

func TestSMTPTLSPayloadAndRelayAcceptance(t *testing.T) {
	for _, mode := range []string{"accept", "quit_failure"} {
		t.Run(mode, func(t *testing.T) {
			dial, captured := smtpFixture(t, mode)
			cfg := smtpConfig()
			sender, err := NewWithSMTPDialer(cfg, dial)
			if err != nil {
				t.Fatal(err)
			}
			cfg.To[0] = "mutated@example.com"
			if err := sender.Notify(context.Background(), event()); err != nil {
				t.Fatal(err)
			}
			capture := captureSMTP(t, captured)
			if capture.auth != "\x00relay-user\x00smtp-secret-password" {
				t.Fatal("SMTP credentials were not authenticated using PLAIN inside TLS")
			}
			commands := strings.Join(capture.commands, "\n")
			for _, want := range []string{"EHLO localhost", "MAIL FROM:<panel@example.com>", "RCPT TO:<admin@example.com>", "RCPT TO:<owner@example.com>", "DATA", "QUIT"} {
				if !strings.Contains(commands, want) {
					t.Errorf("missing SMTP command %q", want)
				}
			}
			message, err := mail.ReadMessage(strings.NewReader(capture.message))
			if err != nil {
				t.Fatal(err)
			}
			if message.Header.Get("Subject") != "New Empire Server Control: backup_succeeded" || message.Header.Get("Message-ID") != "<"+event().ID+"@newempire.invalid>" || message.Header.Get("Auto-Submitted") != "auto-generated" {
				t.Fatalf("invalid mail headers: %v", message.Header)
			}
			body, _ := io.ReadAll(message.Body)
			if !strings.Contains(string(body), "Local backup completed.") || !strings.Contains(string(body), event().InstanceID) || strings.Contains(capture.message, "smtp-secret-password") || strings.Contains(capture.message, "relay-user") || strings.Contains(capture.message, "mutated@example.com") {
				t.Fatalf("unsafe or incomplete email: %s", body)
			}
		})
	}
}

func TestSMTPFailuresAreRedactedAndClassified(t *testing.T) {
	for _, test := range []struct {
		mode      string
		code      int
		retryable bool
	}{
		{"auth_failure", 535, false}, {"recipient_failure", 550, false}, {"data_failure", 451, true},
	} {
		t.Run(test.mode, func(t *testing.T) {
			dial, captured := smtpFixture(t, test.mode)
			sender, err := NewWithSMTPDialer(smtpConfig(), dial)
			if err != nil {
				t.Fatal(err)
			}
			err = sender.Notify(context.Background(), event())
			var failure *DeliveryError
			if !errors.As(err, &failure) || failure.Code != test.code || failure.Retryable != test.retryable || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "example.com") {
				t.Fatalf("unsafe SMTP failure: %v", err)
			}
			capture := captureSMTP(t, captured)
			if test.mode != "data_failure" && capture.message != "" {
				t.Fatal("message was sent after a failed envelope/authentication")
			}
		})
	}
}

func TestSMTPCancellationAndUnverifiedConnections(t *testing.T) {
	dial, captured := smtpFixture(t, "stall")
	sender, err := NewWithSMTPDialer(smtpConfig(), dial)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	if err := sender.Notify(ctx, event()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("SMTP cancellation ignored: %v", err)
	}
	captureSMTP(t, captured)

	client, server := net.Pipe()
	defer server.Close()
	sender, err = NewWithSMTPDialer(smtpConfig(), func(context.Context, string) (net.Conn, error) { return client, nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := sender.Notify(context.Background(), event()); err == nil {
		t.Fatal("plaintext SMTP connection accepted")
	}
	if _, err := NewWithSMTPDialer(smtpConfig(), nil); err == nil {
		t.Fatal("nil SMTP dialer accepted")
	}
}

type unpublishedDeadlineContext struct {
	context.Context
	deadline time.Time
}

func (c unpublishedDeadlineContext) Deadline() (time.Time, bool) { return c.deadline, true }

func TestSMTPDeadlineBeforeContextCancellation(t *testing.T) {
	timeout := &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}
	ctx := unpublishedDeadlineContext{Context: context.Background(), deadline: time.Now().Add(-time.Second)}
	if ctx.Err() != nil {
		t.Fatal("fixture already published context cancellation")
	}
	if err := deliveryFailure(ctx, "SMTP greeting", timeout); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("socket deadline lost context deadline: %v", err)
	}

	for _, ctx := range []context.Context{
		context.Background(),
		unpublishedDeadlineContext{Context: context.Background(), deadline: time.Now().Add(time.Hour)},
	} {
		for _, test := range []struct {
			err       error
			code      int
			retryable bool
		}{
			{timeout, 0, true},
			{&textproto.Error{Code: 451, Msg: "smtp-secret-password temporary failure"}, 451, true},
			{&textproto.Error{Code: 535, Msg: "smtp-secret-password authentication failed"}, 535, false},
		} {
			err := deliveryFailure(ctx, "SMTP greeting", test.err)
			var failure *DeliveryError
			if !errors.As(err, &failure) || failure.Code != test.code || failure.Retryable != test.retryable || strings.Contains(err.Error(), "secret") {
				t.Fatalf("live context changed delivery classification or redaction: %v", err)
			}
		}
	}

	ctxCanceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := deliveryFailure(ctxCanceled, "SMTP greeting", timeout); !errors.Is(err, context.Canceled) {
		t.Fatalf("explicit context cancellation lost: %v", err)
	}
}

func TestSMTPProductionDialRejectsPrivateAddresses(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "::1", "::ffff:127.0.0.1", "100.64.0.1"} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		conn, err := dialSMTP(ctx, host)
		cancel()
		if err == nil {
			conn.Close()
			t.Fatalf("private SMTP address allowed: %s", host)
		}
	}
}
