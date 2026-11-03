package notifications

import (
	"context"
	"encoding/binary"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProductionSendersRejectDNSRebinding(t *testing.T) {
	server, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	var private atomic.Bool
	go func() {
		buffer := make([]byte, 1500)
		for {
			n, peer, err := server.ReadFrom(buffer)
			if err != nil {
				return
			}
			questionEnd := 12
			for questionEnd < n && buffer[questionEnd] != 0 {
				questionEnd += int(buffer[questionEnd]) + 1
			}
			questionEnd++
			if questionEnd+4 > n {
				continue
			}
			kind := binary.BigEndian.Uint16(buffer[questionEnd:])
			reply := append([]byte(nil), buffer[:questionEnd+4]...)
			binary.BigEndian.PutUint16(reply[2:], 0x8180)
			binary.BigEndian.PutUint16(reply[6:], 0)
			binary.BigEndian.PutUint16(reply[8:], 0)
			binary.BigEndian.PutUint16(reply[10:], 0)
			if kind == 1 {
				binary.BigEndian.PutUint16(reply[6:], 1)
				reply = append(reply, 0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 0, 0, 4)
				if private.Load() {
					reply = append(reply, 10, 0, 0, 1)
				} else {
					reply = append(reply, 1, 1, 1, 1)
				}
			}
			server.WriteTo(reply, peer)
		}
	}()
	original := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "udp", server.LocalAddr().String())
	}}
	t.Cleanup(func() { net.DefaultResolver = original })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", "example.com")
	if err != nil || len(ips) != 1 || ips[0].String() != "1.1.1.1" {
		t.Fatalf("public DNS fixture: %v, %v", ips, err)
	}
	sender, err := New(webhookConfig())
	if err != nil {
		t.Fatal(err)
	}
	private.Store(true)
	transport := sender.http.Transport.(*http.Transport)
	conn, err := transport.DialContext(ctx, "tcp", "webhook.example.com:443")
	if conn != nil {
		conn.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "non-public") {
		t.Fatalf("HTTP DNS rebinding allowed: %v", err)
	}
	if err := sender.Notify(ctx, event()); err == nil || strings.Contains(err.Error(), "example.com") || strings.Contains(err.Error(), "10.0.0.1") {
		t.Fatalf("HTTP DNS error not redacted: %v", err)
	}
	conn, err = dialSMTP(ctx, "example.com")
	if conn != nil {
		conn.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "non-public") {
		t.Fatalf("SMTP DNS rebinding allowed: %v", err)
	}
	sender, err = New(smtpConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := sender.Notify(ctx, event()); err == nil || strings.Contains(err.Error(), "example.com") || strings.Contains(err.Error(), "10.0.0.1") {
		t.Fatalf("SMTP DNS error not redacted: %v", err)
	}
}
