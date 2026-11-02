package rcon

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func mockServer(t *testing.T, handler func(net.Conn) error) (string, <-chan error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		done <- handler(conn)
	}()
	return listener.Addr().String(), done
}

func authenticate(conn net.Conn) (packet, error) {
	auth, err := readPacket(conn)
	if err != nil {
		return auth, err
	}
	if auth.kind != 3 || auth.body != "secret-never-logged" || auth.id <= 0 {
		return auth, errors.New("incorrect auth request")
	}
	if err := writePacket(conn, packet{auth.id, 0, ""}); err != nil {
		return auth, err
	}
	return auth, writePacket(conn, packet{auth.id, 2, ""})
}

func TestExecuteAuthenticatesAndJoinsAllResponsePackets(t *testing.T) {
	first := strings.Repeat("a", 4095) + "\xe2"
	second := "\x82\xac" + strings.Repeat("b", 4096)
	address, done := mockServer(t, func(conn net.Conn) error {
		auth, err := authenticate(conn)
		if err != nil {
			return err
		}
		command, err := readPacket(conn)
		if err != nil {
			return err
		}
		end, err := readPacket(conn)
		if err != nil {
			return err
		}
		if command.body != "list" || command.kind != 2 || end.body != "" || end.kind != 2 || end.id == command.id || end.id == auth.id || command.id == auth.id {
			return errors.New("invalid command boundary")
		}
		for _, p := range []packet{{command.id, 0, first}, {command.id, 0, second}, {end.id, 0, "Unknown command"}} {
			if err := writePacket(oneByteWriter{conn}, p); err != nil {
				return err
			}
		}
		return nil
	})
	output, err := Execute(t.Context(), address, "secret-never-logged", "list")
	if err != nil {
		t.Fatal(err)
	}
	if output != first+second {
		t.Fatal("response was truncated or contained the boundary response")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type oneByteWriter struct{ io.Writer }

func (w oneByteWriter) Write(p []byte) (int, error) { return w.Writer.Write(p[:min(len(p), 1)]) }

func TestExecuteRejectsAuthenticationFailure(t *testing.T) {
	address, done := mockServer(t, func(conn net.Conn) error {
		if _, err := readPacket(conn); err != nil {
			return err
		}
		return writePacket(conn, packet{-1, 2, ""})
	})
	_, err := Execute(t.Context(), address, "secret-never-logged", "list")
	if !errors.Is(err, ErrAuthentication) {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(err.Error(), "secret-never-logged") {
		t.Fatal("password leaked")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestExecuteRejectsUnexpectedIDsAndTruncation(t *testing.T) {
	for _, mode := range []string{"auth-id", "auth-type", "command-id", "no-boundary", "boundary-before-response", "oversized-response"} {
		t.Run(mode, func(t *testing.T) {
			address, done := mockServer(t, func(conn net.Conn) error {
				if mode == "auth-id" || mode == "auth-type" {
					auth, err := readPacket(conn)
					if err != nil {
						return err
					}
					if mode == "auth-id" {
						return writePacket(conn, packet{auth.id + 1, 2, ""})
					}
					return writePacket(conn, packet{auth.id, 42, ""})
				}
				if _, err := authenticate(conn); err != nil {
					return err
				}
				command, err := readPacket(conn)
				if err != nil {
					return err
				}
				end, err := readPacket(conn)
				if err != nil {
					return err
				}
				switch mode {
				case "command-id":
					return writePacket(conn, packet{command.id + 100, 0, "untrusted"})
				case "boundary-before-response":
					return writePacket(conn, packet{end.id, 0, ""})
				case "oversized-response":
					for i := 0; i < 66; i++ {
						if err := writePacket(conn, packet{command.id, 0, strings.Repeat("x", 65000)}); err != nil {
							return nil
						}
					}
					return nil
				default:
					return writePacket(conn, packet{command.id, 0, "partial"})
				}
			})
			output, err := Execute(t.Context(), address, "secret-never-logged", "list")
			if err == nil || output != "" {
				t.Fatalf("partial or invalid response succeeded: %q, %v", output, err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestExecuteCancellationClosesConnection(t *testing.T) {
	address, done := mockServer(t, func(conn net.Conn) error {
		if _, err := readPacket(conn); err != nil {
			return err
		}
		var b [1]byte
		_, err := conn.Read(b[:])
		if err != io.EOF {
			return fmt.Errorf("expected client close, got %v", err)
		}
		return nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err := Execute(ctx, address, "secret-never-logged", "list")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestExecuteRejectsNonLoopbackAndUnsafeInput(t *testing.T) {
	for _, address := range []string{"localhost:25575", "example.com:25575", "192.168.1.1:25575", "0.0.0.0:25575", "127.0.0.1:0", "[::1%lo]:25575", "[::]:25575", "127.0.0.1:65536", "127.0.0.1"} {
		if _, err := Execute(t.Context(), address, "secret", "list"); err == nil {
			t.Errorf("accepted %s", address)
		}
	}
	for _, command := range []string{"", " ", "list\nstop", "list\rstop", "say\x00hidden", strings.Repeat("x", MaxCommandBytes+1), "\xff"} {
		if err := ValidateCommand(command); err == nil {
			t.Errorf("accepted unsafe command %q", command)
		}
	}
	if _, err := Execute(t.Context(), "127.0.0.1:25575", "", "list"); err == nil {
		t.Fatal("accepted empty secret")
	}
}

func TestReadPacketRejectsMalformedFrames(t *testing.T) {
	var good bytes.Buffer
	if err := writePacket(&good, packet{12, 0, "okay"}); err != nil {
		t.Fatal(err)
	}
	badTerminator := bytes.Clone(good.Bytes())
	badTerminator[len(badTerminator)-2] = 1
	badBody := bytes.Clone(good.Bytes())
	badBody[12] = 0
	short, huge, negative := make([]byte, 4), make([]byte, 4), make([]byte, 4)
	binary.LittleEndian.PutUint32(short, 9)
	binary.LittleEndian.PutUint32(huge, maxPacketBytes+1)
	binary.LittleEndian.PutUint32(negative, ^uint32(0))
	for _, frame := range [][]byte{nil, {1, 2}, short, huge, negative, good.Bytes()[:10], badTerminator, badBody} {
		if _, err := readPacket(bytes.NewReader(frame)); err == nil {
			t.Errorf("accepted malformed frame %v", frame)
		}
	}
}

func FuzzReadPacket(f *testing.F) {
	f.Add([]byte{10, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	f.Add([]byte{255, 255, 255, 255})
	f.Fuzz(func(t *testing.T, data []byte) { _, _ = readPacket(bytes.NewReader(data)) })
}
