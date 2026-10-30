package host

// Minimal SOCKS5 outbound dialer (RFC 1928 + RFC 1929 user/pass) so the
// renderer's socks5 proxy options behave like Electron's (Chromium's
// network stack). No-auth and username/password auth are supported; the
// proxy is dialed with a plain TCP connection and the request flows
// through the CONNECT tunnel.

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"time"
)

// socks5DialContext returns a Transport.DialContext that tunnels every
// connection through the socks5 proxy at proxyURL (socks5://[user:pass@]host:port,
// default port 1080).
func socks5DialContext(proxyURL *url.URL) func(ctx context.Context, network, addr string) (net.Conn, error) {
	host := proxyURL.Hostname()
	port := proxyURL.Port()
	if port == "" {
		port = "1080"
	}
	proxyAddr := net.JoinHostPort(host, port)
	var user, pass string
	if proxyURL.User != nil {
		user = proxyURL.User.Username()
		pass, _ = proxyURL.User.Password()
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if network != "tcp" && network != "tcp4" && network != "tcp6" {
			return nil, fmt.Errorf("socks5: unsupported network %q", network)
		}
		var dialer net.Dialer
		conn, err := dialer.DialContext(ctx, "tcp", proxyAddr)
		if err != nil {
			return nil, fmt.Errorf("socks5: dial proxy %s: %w", proxyAddr, err)
		}
		if deadline, ok := ctx.Deadline(); ok {
			_ = conn.SetDeadline(deadline)
		} else {
			_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
		}
		if err := socks5Handshake(conn, addr, user, pass); err != nil {
			conn.Close()
			return nil, err
		}
		// The tunnel is established; the application data flow has no
		// deadline of its own.
		_ = conn.SetDeadline(time.Time{})
		return conn, nil
	}
}

// socks5Handshake negotiates the method, authenticates when credentials
// are configured and issues CONNECT for addr (host:port).
func socks5Handshake(conn net.Conn, addr, user, pass string) error {
	methods := []byte{0x00} // no authentication required
	if user != "" {
		methods = append(methods, 0x02) // username/password
	}
	greeting := append([]byte{0x05, byte(len(methods))}, methods...)
	if _, err := conn.Write(greeting); err != nil {
		return fmt.Errorf("socks5: send greeting: %w", err)
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return fmt.Errorf("socks5: read method: %w", err)
	}
	if reply[0] != 0x05 {
		return fmt.Errorf("socks5: unsupported proxy version %d", reply[0])
	}
	switch reply[1] {
	case 0x00:
	case 0x02:
		if user == "" {
			return errors.New("socks5: proxy requires username/password auth")
		}
		req := make([]byte, 0, 3+len(user)+len(pass))
		req = append(req, 0x01, byte(len(user)))
		req = append(req, user...)
		req = append(req, byte(len(pass)))
		req = append(req, pass...)
		if _, err := conn.Write(req); err != nil {
			return fmt.Errorf("socks5: send auth: %w", err)
		}
		auth := make([]byte, 2)
		if _, err := io.ReadFull(conn, auth); err != nil {
			return fmt.Errorf("socks5: read auth reply: %w", err)
		}
		if auth[1] != 0x00 {
			return errors.New("socks5: authentication failed")
		}
	default:
		return fmt.Errorf("socks5: proxy rejected every auth method (reply 0x%02x)", reply[1])
	}

	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("socks5: invalid target %q", addr)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 || port > 0xFFFF {
		return fmt.Errorf("socks5: invalid target port %q", portText)
	}
	req := make([]byte, 0, 7+len(host))
	req = append(req, 0x05, 0x01, 0x00, 0x03, byte(len(host)))
	req = append(req, host...)
	req = binary.BigEndian.AppendUint16(req, uint16(port))
	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("socks5: send CONNECT: %w", err)
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return fmt.Errorf("socks5: read CONNECT reply: %w", err)
	}
	if head[0] != 0x05 {
		return fmt.Errorf("socks5: unsupported reply version %d", head[0])
	}
	if head[1] != 0x00 {
		return fmt.Errorf("socks5: CONNECT failed (reply 0x%02x)", head[1])
	}
	// Skip the bound address: 4 bytes when IPv4 / 16 when IPv6, otherwise
	// a length byte plus that many host bytes, then the 2-byte port.
	var skip int
	switch head[3] {
	case 0x01:
		skip = 4
	case 0x04:
		skip = 16
	case 0x03:
		if _, err := io.ReadFull(conn, head[:1]); err != nil {
			return fmt.Errorf("socks5: read reply address: %w", err)
		}
		skip = int(head[0])
	default:
		return fmt.Errorf("socks5: unknown reply address type 0x%02x", head[3])
	}
	if _, err := io.CopyN(io.Discard, conn, int64(skip+2)); err != nil {
		return fmt.Errorf("socks5: read reply address: %w", err)
	}
	return nil
}
