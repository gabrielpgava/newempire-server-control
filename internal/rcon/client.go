// Package rcon implements bounded, authenticated Minecraft RCON over loopback TCP.
package rcon

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	MaxCommandBytes  = 1446
	MaxResponseBytes = 4 << 20
	maxPacketBytes   = 64 << 10
	maxPackets       = 4096
	operationTimeout = 5 * time.Second
)

var (
	ErrAuthentication = errors.New("RCON authentication failed")
	ErrProtocol       = errors.New("invalid RCON response")
)

// Execute opens a fresh connection for one command. Only numeric loopback
// addresses are accepted, so DNS and redirects cannot widen the trust boundary.
func Execute(ctx context.Context, address, password, command string) (string, error) {
	if err := ValidateCommand(command); err != nil {
		return "", err
	}
	if len(password) == 0 || len(password) > MaxCommandBytes || strings.ContainsRune(password, 0) {
		return "", errors.New("RCON password is missing or invalid")
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", errors.New("invalid RCON address")
	}
	ip, err := netip.ParseAddr(host)
	p, portErr := strconv.Atoi(port)
	if err != nil || ip.Zone() != "" || !ip.Unmap().IsLoopback() || portErr != nil || p < 1 || p > 65535 {
		return "", errors.New("RCON requires a numeric loopback address and valid port")
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), strconv.Itoa(p)))
	if err != nil {
		return "", fmt.Errorf("connect RCON: %w", err)
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return "", fmt.Errorf("set RCON deadline: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	result, err := exchange(conn, password, command)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	var timeout net.Error
	if err != nil && errors.As(err, &timeout) && timeout.Timeout() && !time.Now().Before(deadline) {
		return "", context.DeadlineExceeded
	}
	return result, err
}

func ValidateCommand(command string) error {
	if len(strings.TrimSpace(command)) == 0 || len(command) > MaxCommandBytes || !utf8.ValidString(command) || strings.ContainsAny(command, "\x00\r\n") {
		return errors.New("RCON command must be one nonempty UTF-8 line of at most 1446 bytes")
	}
	return nil
}

type packet struct {
	id   int32
	kind int32
	body string
}

func exchange(conn net.Conn, password, command string) (string, error) {
	var random [4]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("create RCON request ID: %w", err)
	}
	// Keep three distinct positive IDs, excluding the protocol's -1 auth failure.
	authID := int32(binary.LittleEndian.Uint32(random[:])&0x3fffffff) + 1
	commandID, endID := authID+1, authID+2
	if err := writePacket(conn, packet{authID, 3, password}); err != nil {
		return "", fmt.Errorf("write RCON authentication: %w", err)
	}
	authenticated := false
	for i := 0; i < 2; i++ {
		p, err := readPacket(conn)
		if err != nil {
			return "", fmt.Errorf("read RCON authentication: %w", err)
		}
		if p.id == -1 && p.kind == 2 {
			return "", ErrAuthentication
		}
		if p.id != authID || p.body != "" {
			return "", ErrProtocol
		}
		if p.kind == 2 {
			authenticated = true
			break
		}
		if p.kind != 0 {
			return "", ErrProtocol
		}
	}
	if !authenticated {
		return "", ErrProtocol
	}
	if err := writePacket(conn, packet{commandID, 2, command}); err != nil {
		return "", fmt.Errorf("write RCON command: %w", err)
	}
	// Minecraft replies to an empty command. Its distinct ID marks the end of
	// all command chunks without treating a timeout as a successful response.
	if err := writePacket(conn, packet{endID, 2, ""}); err != nil {
		return "", fmt.Errorf("write RCON response boundary: %w", err)
	}
	var result strings.Builder
	seen := false
	for i := 0; i < maxPackets; i++ {
		p, err := readPacket(conn)
		if err != nil {
			return "", fmt.Errorf("read RCON command response: %w", err)
		}
		if p.kind != 0 {
			return "", ErrProtocol
		}
		if p.id == endID && seen {
			if !utf8.ValidString(result.String()) {
				return "", ErrProtocol
			}
			return result.String(), nil
		}
		if p.id != commandID || result.Len()+len(p.body) > MaxResponseBytes {
			return "", ErrProtocol
		}
		seen = true
		result.WriteString(p.body)
	}
	return "", errors.New("RCON response exceeds packet limit")
}

func readPacket(r io.Reader) (packet, error) {
	var length [4]byte
	if _, err := io.ReadFull(r, length[:]); err != nil {
		return packet{}, err
	}
	n := binary.LittleEndian.Uint32(length[:])
	if n < 10 || n > maxPacketBytes {
		return packet{}, ErrProtocol
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return packet{}, err
	}
	if buf[n-1] != 0 || buf[n-2] != 0 || strings.ContainsRune(string(buf[8:n-2]), 0) {
		return packet{}, ErrProtocol
	}
	return packet{int32(binary.LittleEndian.Uint32(buf)), int32(binary.LittleEndian.Uint32(buf[4:])), string(buf[8 : n-2])}, nil
}

func writePacket(w io.Writer, p packet) error {
	buf := make([]byte, 14+len(p.body))
	binary.LittleEndian.PutUint32(buf, uint32(10+len(p.body)))
	binary.LittleEndian.PutUint32(buf[4:], uint32(p.id))
	binary.LittleEndian.PutUint32(buf[8:], uint32(p.kind))
	copy(buf[12:], p.body)
	for len(buf) > 0 {
		n, err := w.Write(buf)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		buf = buf[n:]
	}
	return nil
}
