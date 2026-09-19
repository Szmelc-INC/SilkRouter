package proxy

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
)

// splitHostPort resolves an "host:port" target into the pieces the SOCKS
// handshakes need. Port errors are surfaced early with a clear message.
func splitHostPort(addr string) (host string, port uint16, err error) {
	h, pStr, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, fmt.Errorf("bad target address %q: %w", addr, err)
	}
	p, err := strconv.Atoi(pStr)
	if err != nil || p < 1 || p > 65535 {
		return "", 0, fmt.Errorf("bad target port %q", pStr)
	}
	return h, uint16(p), nil
}

// socks5Handshake performs a SOCKS5 CONNECT to target over an established conn.
// It supports the no-auth and username/password (RFC 1929) methods.
func (p Proxy) socks5Handshake(conn net.Conn, target string) error {
	host, port, err := splitHostPort(target)
	if err != nil {
		return err
	}

	// Greeting: advertise no-auth, plus user/pass when we have credentials.
	methods := []byte{0x00}
	if p.User != "" {
		methods = []byte{0x00, 0x02}
	}
	greet := append([]byte{0x05, byte(len(methods))}, methods...)
	if _, err := conn.Write(greet); err != nil {
		return fmt.Errorf("socks5 greeting: %w", err)
	}

	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return fmt.Errorf("socks5 method reply: %w", err)
	}
	if reply[0] != 0x05 {
		return fmt.Errorf("socks5: bad version 0x%02x", reply[0])
	}
	switch reply[1] {
	case 0x00: // no auth
	case 0x02: // username/password
		if err := p.socks5Auth(conn); err != nil {
			return err
		}
	case 0xff:
		return fmt.Errorf("socks5: no acceptable auth method (creds required?)")
	default:
		return fmt.Errorf("socks5: unsupported auth method 0x%02x", reply[1])
	}

	// CONNECT request.
	req := []byte{0x05, 0x01, 0x00}
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			req = append(req, 0x01)
			req = append(req, v4...)
		} else {
			req = append(req, 0x04)
			req = append(req, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return fmt.Errorf("socks5: hostname too long")
		}
		req = append(req, 0x03, byte(len(host)))
		req = append(req, host...)
	}
	req = binary.BigEndian.AppendUint16(req, port)
	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("socks5 connect request: %w", err)
	}

	// Reply header: VER REP RSV ATYP ...
	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return fmt.Errorf("socks5 connect reply: %w", err)
	}
	if head[1] != 0x00 {
		return fmt.Errorf("socks5 connect failed: %s", socks5Error(head[1]))
	}
	// Consume the bound address so the stream is positioned at real data.
	var addrLen int
	switch head[3] {
	case 0x01:
		addrLen = 4
	case 0x04:
		addrLen = 16
	case 0x03:
		l := make([]byte, 1)
		if _, err := io.ReadFull(conn, l); err != nil {
			return fmt.Errorf("socks5 bound addr len: %w", err)
		}
		addrLen = int(l[0])
	default:
		return fmt.Errorf("socks5: bad bound atyp 0x%02x", head[3])
	}
	if _, err := io.ReadFull(conn, make([]byte, addrLen+2)); err != nil {
		return fmt.Errorf("socks5 bound addr: %w", err)
	}
	return nil
}

// socks5Auth runs the username/password sub-negotiation (RFC 1929).
func (p Proxy) socks5Auth(conn net.Conn) error {
	if len(p.User) > 255 || len(p.Pass) > 255 {
		return fmt.Errorf("socks5 auth: credential too long")
	}
	msg := []byte{0x01, byte(len(p.User))}
	msg = append(msg, p.User...)
	msg = append(msg, byte(len(p.Pass)))
	msg = append(msg, p.Pass...)
	if _, err := conn.Write(msg); err != nil {
		return fmt.Errorf("socks5 auth send: %w", err)
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return fmt.Errorf("socks5 auth reply: %w", err)
	}
	if resp[1] != 0x00 {
		return fmt.Errorf("socks5 auth: rejected (status 0x%02x)", resp[1])
	}
	return nil
}

// socks4Handshake performs a SOCKS4/4a CONNECT. When the scheme is socks4a (or
// the target is a hostname that cannot be resolved locally) the hostname is
// sent to the proxy for remote resolution.
func (p Proxy) socks4Handshake(ctx context.Context, conn net.Conn, target string) error {
	host, port, err := splitHostPort(target)
	if err != nil {
		return err
	}

	req := []byte{0x04, 0x01}
	req = binary.BigEndian.AppendUint16(req, port)

	var hostname string
	if ip := net.ParseIP(host); ip != nil && ip.To4() != nil {
		req = append(req, ip.To4()...)
	} else if p.Scheme == SOCKS4A {
		// SOCKS4a sentinel address 0.0.0.x — hostname follows the userid.
		req = append(req, 0x00, 0x00, 0x00, 0x01)
		hostname = host
	} else {
		// Plain SOCKS4 cannot carry a hostname; resolve it ourselves.
		ips, err := net.DefaultResolver.LookupIP(ctx, "ip4", host)
		if err != nil || len(ips) == 0 {
			return fmt.Errorf("socks4: cannot resolve %q (use socks4a): %v", host, err)
		}
		req = append(req, ips[0].To4()...)
	}
	req = append(req, p.User...) // userid (optional)
	req = append(req, 0x00)
	if hostname != "" {
		req = append(req, hostname...)
		req = append(req, 0x00)
	}
	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("socks4 connect request: %w", err)
	}

	reply := make([]byte, 8)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return fmt.Errorf("socks4 connect reply: %w", err)
	}
	if reply[0] != 0x00 {
		return fmt.Errorf("socks4: bad reply version 0x%02x", reply[0])
	}
	if reply[1] != 0x5a {
		return fmt.Errorf("socks4 connect failed: %s", socks4Error(reply[1]))
	}
	return nil
}

func socks5Error(code byte) string {
	switch code {
	case 0x01:
		return "general SOCKS server failure"
	case 0x02:
		return "connection not allowed by ruleset"
	case 0x03:
		return "network unreachable"
	case 0x04:
		return "host unreachable"
	case 0x05:
		return "connection refused"
	case 0x06:
		return "TTL expired"
	case 0x07:
		return "command not supported"
	case 0x08:
		return "address type not supported"
	default:
		return fmt.Sprintf("unknown error 0x%02x", code)
	}
}

func socks4Error(code byte) string {
	switch code {
	case 0x5b:
		return "request rejected or failed"
	case 0x5c:
		return "request rejected: identd unreachable"
	case 0x5d:
		return "request rejected: identd user mismatch"
	default:
		return fmt.Sprintf("unknown error 0x%02x", code)
	}
}
