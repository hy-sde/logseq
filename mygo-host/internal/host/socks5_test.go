package host

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// fakeSocks5Server implements just enough of RFC 1928 to accept the
// client's handshake and CONNECT, then forwards raw bytes to the real
// target (an httptest server on loopback).
func fakeSocks5Server(t *testing.T, wantUser, wantPass string) (addr string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveSocks5(conn, wantUser, wantPass)
		}
	}()
	return ln.Addr().String()
}

func serveSocks5(conn net.Conn, wantUser, wantPass string) {
	defer conn.Close()
	head := make([]byte, 2)
	if _, err := readFull(conn, head); err != nil || head[0] != 0x05 {
		return
	}
	methods := make([]byte, head[1])
	if _, err := readFull(conn, methods); err != nil {
		return
	}
	hasAuth := false
	for _, m := range methods {
		if m == 0x02 {
			hasAuth = true
		}
	}
	if wantUser != "" {
		if !hasAuth {
			return
		}
		conn.Write([]byte{0x05, 0x02})
		ver := make([]byte, 1)
		ulen := make([]byte, 1)
		if _, err := readFull(conn, ver); err != nil || ver[0] != 0x01 {
			return
		}
		if _, err := readFull(conn, ulen); err != nil {
			return
		}
		user := make([]byte, ulen[0])
		if _, err := readFull(conn, user); err != nil {
			return
		}
		plen := make([]byte, 1)
		if _, err := readFull(conn, plen); err != nil {
			return
		}
		pass := make([]byte, plen[0])
		if _, err := readFull(conn, pass); err != nil {
			return
		}
		if string(user) != wantUser || string(pass) != wantPass {
			conn.Write([]byte{0x01, 0x01})
			return
		}
		conn.Write([]byte{0x01, 0x00})
	} else {
		conn.Write([]byte{0x05, 0x00})
	}

	req := make([]byte, 4)
	if _, err := readFull(conn, req); err != nil || req[1] != 0x01 {
		return
	}
	var host string
	switch req[3] {
	case 0x01:
		b := make([]byte, 4)
		if _, err := readFull(conn, b); err != nil {
			return
		}
		host = net.IP(b).String()
	case 0x03:
		l := make([]byte, 1)
		if _, err := readFull(conn, l); err != nil {
			return
		}
		b := make([]byte, l[0])
		if _, err := readFull(conn, b); err != nil {
			return
		}
		host = string(b)
	default:
		return
	}
	p := make([]byte, 2)
	if _, err := readFull(conn, p); err != nil {
		return
	}
	target, err := net.Dial("tcp", net.JoinHostPort(host, fmt.Sprintf("%d", int(p[0])<<8|int(p[1]))))
	if err != nil {
		conn.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	defer target.Close()
	conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	go copyClose(target, conn)
	copyClose(conn, target)
}

func readFull(conn net.Conn, b []byte) (int, error) {
	total := 0
	for total < len(b) {
		n, err := conn.Read(b[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func copyClose(dst, src net.Conn) {
	buf := make([]byte, 4096)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
}

func TestSocks5ProxyRoundTrip(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "through-socks5")
	}))
	defer backend.Close()
	proxyAddr := fakeSocks5Server(t, "", "")

	transport := &http.Transport{
		DialContext: socks5DialContext(&url.URL{Scheme: "socks5", Host: proxyAddr}),
	}
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	resp, err := client.Get(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 64)
	n, _ := resp.Body.Read(buf)
	if strings.TrimSpace(string(buf[:n])) != "through-socks5" {
		t.Fatalf("body = %q, want %q", string(buf[:n]), "through-socks5")
	}
}

func TestSocks5ProxyAuth(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer backend.Close()
	proxyAddr := fakeSocks5Server(t, "user", "pass")

	// Correct credentials work.
	transport := &http.Transport{
		DialContext: socks5DialContext(&url.URL{Scheme: "socks5", User: url.UserPassword("user", "pass"), Host: proxyAddr}),
	}
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	resp, err := client.Get(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// Wrong credentials fail the auth subnegotiation.
	badTransport := &http.Transport{
		DialContext: socks5DialContext(&url.URL{Scheme: "socks5", User: url.UserPassword("user", "wrong"), Host: proxyAddr}),
	}
	badClient := &http.Client{Transport: badTransport, Timeout: 5 * time.Second}
	if _, err := badClient.Get(backend.URL); err == nil {
		t.Fatal("expected auth failure with wrong password")
	} else if !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("err = %v, want authentication failure", err)
	}
}

func TestSocks5DialerConnectFailure(t *testing.T) {
	// A socks5 proxy that cannot reach its target surfaces the CONNECT
	// reply code.
	proxyAddr := fakeSocks5Server(t, "", "")
	transport := &http.Transport{
		DialContext: socks5DialContext(&url.URL{Scheme: "socks5", Host: proxyAddr}),
	}
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	_, err := client.Get("http://127.0.0.1:1/x")
	if err == nil || !strings.Contains(err.Error(), "CONNECT failed") {
		t.Fatalf("err = %v, want CONNECT failure", err)
	}
}

func TestSocks5DialerContextCancel(t *testing.T) {
	proxyAddr := fakeSocks5Server(t, "", "")
	dial := socks5DialContext(&url.URL{Scheme: "socks5", Host: proxyAddr})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := dial(ctx, "tcp", "example.com:80"); err == nil {
		t.Fatal("expected canceled context to fail the dial")
	}
}
