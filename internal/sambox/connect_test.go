// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sambox

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// startBoundary serves s on a Unix socket, the transport the sandbox boundary
// actually uses, and returns its path.
func startBoundary(t *testing.T, s *ConnectServer) string {
	t.Helper()

	// Not t.TempDir(): test names make paths long enough to hit the ~108 byte
	// sockaddr_un limit.
	dir, err := os.MkdirTemp("", "sambox")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	path := filepath.Join(dir, "agent.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := s.Serve(ctx, l); err != nil {
			t.Errorf("Serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return path
}

// startEcho returns the address of a server that echoes what it is sent.
func startEcho(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return l.Addr().String()
}

// startUDPEcho returns the address of a datagram server that echoes.
func startUDPEcho(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })

	go func() {
		buf := make([]byte, 65535)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteTo(buf[:n], from)
		}
	}()
	return pc.LocalAddr().String()
}

// dialRaw opens a plain connection to the boundary, for the cases a
// well-behaved client library will never produce.
func dialRaw(t *testing.T, path string) net.Conn {
	t.Helper()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	return conn
}

func basicAuth(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

// boundaryDialContext returns a DialContext that opens each flow as a CONNECT
// tunnel through the boundary at path, the way tun2connect does in a sandbox.
func boundaryDialContext(path string) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, _, addr string) (net.Conn, error) {
		var d net.Dialer
		conn, err := d.DialContext(ctx, "unix", path)
		if err != nil {
			return nil, err
		}
		req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Host: addr}, Host: addr, Header: make(http.Header)}
		if err := req.Write(conn); err != nil {
			_ = conn.Close()
			return nil, err
		}
		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, req)
		if err != nil {
			_ = conn.Close()
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			_ = conn.Close()
			return nil, errors.New("boundary refused: " + resp.Status)
		}
		return &bufConn{Conn: conn, br: br}, nil
	}
}

// connectRoundTrip writes an authority-form CONNECT for hostport and returns
// the connection, the buffered reader holding anything past the response, and
// the response itself.
func connectRoundTrip(t *testing.T, path, hostport string, header http.Header) (net.Conn, *bufio.Reader, *http.Response) {
	t.Helper()
	conn := dialRaw(t, path)
	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Host: hostport},
		Host:   hostport,
		Header: header,
	}
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	if err := req.Write(conn); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	return conn, br, resp
}

// connectThrough opens a CONNECT tunnel and fails the test on refusal.
func connectThrough(t *testing.T, path, hostport string) net.Conn {
	t.Helper()
	conn, br, resp := connectRoundTrip(t, path, hostport, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT %s = %s (reason %q), want 200", hostport, resp.Status, resp.Header.Get("Boundary-Reason"))
	}
	return &bufConn{Conn: conn, br: br}
}

// TestConnectPreservesDestinationName is the property the whole boundary rests
// on: policy must see the name the agent asked for, never a resolved address.
func TestConnectPreservesDestinationName(t *testing.T) {
	echo := startEcho(t)

	seen := make(chan Destination, 1)
	path := startBoundary(t, &ConnectServer{
		Dialer: DialerFunc(func(ctx context.Context, creds *Credentials, dst Destination) (net.Conn, error) {
			seen <- dst
			return net.Dial("tcp", echo)
		}),
	})

	conn := connectThrough(t, path, "api.github.com:443")
	defer func() { _ = conn.Close() }()

	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "ping" {
		t.Errorf("echo = %q, want %q", got, "ping")
	}

	dst := <-seen
	if !dst.IsName {
		t.Errorf("destination %+v was not reported as a name", dst)
	}
	if dst.Name != "api.github.com" || dst.Port != 443 || dst.network() != "tcp" {
		t.Errorf("destination = %s over %s, want api.github.com:443 over tcp", dst, dst.network())
	}
}

func TestConnectDeniedByPolicy(t *testing.T) {
	path := startBoundary(t, &ConnectServer{
		Dialer: DialerFunc(func(ctx context.Context, creds *Credentials, dst Destination) (net.Conn, error) {
			return nil, ErrNotAllowed
		}),
	})

	_, _, resp := connectRoundTrip(t, path, "evil.example:80", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
	if reason := resp.Header.Get("Boundary-Reason"); reason != "not allowed by policy" {
		t.Errorf("Boundary-Reason = %q, want a legible policy denial", reason)
	}
}

func TestNonTunnelMethodsAreRefused(t *testing.T) {
	path := startBoundary(t, &ConnectServer{
		Dialer: DialerFunc(func(ctx context.Context, creds *Credentials, dst Destination) (net.Conn, error) {
			t.Error("dialer must not be reached for a non-tunnel request")
			return nil, errors.New("unreachable")
		}),
	})

	// A GET without the connect-udp upgrade is a client that thinks this is a
	// web server; the boundary is not one.
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		conn := dialRaw(t, path)
		req, err := http.NewRequest(method, "http://boundary/anything", nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		if err := req.Write(conn); err != nil {
			t.Fatalf("write request: %v", err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(conn), req)
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s: status = %d, want %d", method, resp.StatusCode, http.StatusMethodNotAllowed)
		}
	}
}

func TestMalformedConnectTargetIsRefused(t *testing.T) {
	path := startBoundary(t, &ConnectServer{
		Dialer: DialerFunc(func(ctx context.Context, creds *Credentials, dst Destination) (net.Conn, error) {
			t.Error("dialer must not be reached for a malformed target")
			return nil, errors.New("unreachable")
		}),
	})

	// Authority form requires host:port; a bare host must be refused rather
	// than guessed at.
	_, _, resp := connectRoundTrip(t, path, "api.github.com", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestConnectByIPLiteralIsNotReportedAsAName(t *testing.T) {
	echo := startEcho(t)

	seen := make(chan Destination, 1)
	path := startBoundary(t, &ConnectServer{
		Dialer: DialerFunc(func(ctx context.Context, creds *Credentials, dst Destination) (net.Conn, error) {
			seen <- dst
			return net.Dial("tcp", echo)
		}),
	})

	conn := connectThrough(t, path, "192.0.2.10:443")
	defer func() { _ = conn.Close() }()

	dst := <-seen
	if dst.IsName {
		t.Errorf("destination %+v was reported as a name", dst)
	}
	if dst.Name != "192.0.2.10" || dst.Port != 443 {
		t.Errorf("destination = %s, want 192.0.2.10:443", dst)
	}
}

// TestAuthenticationIsNotDowngraded pins that a server expecting credentials
// refuses an anonymous client instead of serving it unidentified.
func TestAuthenticationIsNotDowngraded(t *testing.T) {
	path := startBoundary(t, &ConnectServer{
		Dialer: DialerFunc(func(ctx context.Context, creds *Credentials, dst Destination) (net.Conn, error) {
			t.Error("dialer must not be reached for an unauthenticated client")
			return nil, errors.New("unreachable")
		}),
		Authenticate: func(Credentials) error { return nil },
	})

	_, _, resp := connectRoundTrip(t, path, "api.github.com:443", nil)
	if resp.StatusCode != http.StatusProxyAuthRequired {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusProxyAuthRequired)
	}
	if resp.Header.Get("Proxy-Authenticate") == "" {
		t.Error("a 407 must say how to authenticate")
	}
}

func TestAuthenticatedFlowCarriesCredentials(t *testing.T) {
	echo := startEcho(t)

	seen := make(chan *Credentials, 1)
	path := startBoundary(t, &ConnectServer{
		Dialer: DialerFunc(func(ctx context.Context, creds *Credentials, dst Destination) (net.Conn, error) {
			seen <- creds
			return net.Dial("tcp", echo)
		}),
		Authenticate: func(c Credentials) error {
			if c.Username != "reviewer-7.prod.acme.example" {
				return errors.New("unknown agent")
			}
			return nil
		},
	})

	header := make(http.Header)
	header.Set("Proxy-Authorization", basicAuth("reviewer-7.prod.acme.example", "admission-token"))
	_, _, resp := connectRoundTrip(t, path, "code-reviewer.mcp.sam.alt:80", header)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	creds := <-seen
	if creds == nil {
		t.Fatal("dialer received no credentials")
	}
	if creds.Username != "reviewer-7.prod.acme.example" || creds.Password != "admission-token" {
		t.Errorf("credentials = %+v, want the agent id and its admission token", creds)
	}
}

func TestRejectedCredentialsFailTheHandshake(t *testing.T) {
	path := startBoundary(t, &ConnectServer{
		Dialer: DialerFunc(func(ctx context.Context, creds *Credentials, dst Destination) (net.Conn, error) {
			t.Error("dialer must not be reached when authentication fails")
			return nil, errors.New("unreachable")
		}),
		Authenticate: func(Credentials) error { return errors.New("unknown agent") },
	})

	header := make(http.Header)
	header.Set("Proxy-Authorization", basicAuth("bar", "nope"))
	_, _, resp := connectRoundTrip(t, path, "api.github.com:443", header)
	if resp.StatusCode != http.StatusProxyAuthRequired {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusProxyAuthRequired)
	}
}

func TestNonHTTPGreetingIsDropped(t *testing.T) {
	path := startBoundary(t, &ConnectServer{
		Dialer: DialerFunc(func(ctx context.Context, creds *Credentials, dst Destination) (net.Conn, error) {
			t.Error("dialer must not be reached for a non-HTTP client")
			return nil, errors.New("unreachable")
		}),
	})

	conn := dialRaw(t, path)
	// A SOCKS5 greeting, which is what the previous boundary spoke: there is
	// nothing well-formed to answer it with. The newline lets the boundary
	// judge the line now rather than waiting out the handshake deadline.
	if _, err := conn.Write([]byte("\x05\x01\x00\r\n")); err != nil {
		t.Fatalf("write greeting: %v", err)
	}
	if _, err := io.ReadFull(conn, make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Errorf("read after non-HTTP greeting = %v, want EOF", err)
	}
}

// TestConnectUDPRoundTrip pins the boundary's UDP shape: a connect-udp upgrade
// naming the destination, then DATAGRAM capsules both ways.
func TestConnectUDPRoundTrip(t *testing.T) {
	echo := startUDPEcho(t)

	seen := make(chan Destination, 1)
	path := startBoundary(t, &ConnectServer{
		Dialer: DialerFunc(func(ctx context.Context, creds *Credentials, dst Destination) (net.Conn, error) {
			seen <- dst
			return net.Dial("udp", echo)
		}),
	})

	conn := dialRaw(t, path)
	req := &http.Request{
		Method: http.MethodGet,
		URL:    &url.URL{Scheme: "http", Host: "boundary", Path: "/.well-known/masque/udp/dns.example/53/"},
		Host:   "boundary",
		Header: make(http.Header),
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "connect-udp")
	req.Header.Set("Capsule-Protocol", "?1")
	if err := req.Write(conn); err != nil {
		t.Fatalf("write connect-udp: %v", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols || resp.Header.Get("Upgrade") != "connect-udp" {
		t.Fatalf("response = %s (Upgrade %q), want 101 connect-udp", resp.Status, resp.Header.Get("Upgrade"))
	}

	dst := <-seen
	if dst.Name != "dns.example" || dst.Port != 53 || dst.network() != "udp" || !dst.IsName {
		t.Errorf("destination = %+v, want dns.example:53 over udp as a name", dst)
	}

	cs := NewCapsuleStream(&bufConn{Conn: conn, br: br})
	if err := cs.WriteDatagram([]byte("ping")); err != nil {
		t.Fatalf("WriteDatagram: %v", err)
	}
	got, err := cs.ReadDatagram()
	if err != nil {
		t.Fatalf("ReadDatagram: %v", err)
	}
	if string(got) != "ping" {
		t.Errorf("echo = %q, want %q", got, "ping")
	}
}

// TestMasqueDestinationSurvivesEscapedSlashes pins the parsing order: split
// the escaped path first, unescape each segment after, so a %2F inside the
// host cannot change the segment count.
func TestMasqueDestinationSurvivesEscapedSlashes(t *testing.T) {
	dst, err := masqueDestination("/.well-known/masque/udp/odd%2Fname/53/")
	if err != nil {
		t.Fatalf("masqueDestination: %v", err)
	}
	if dst.Name != "odd/name" || dst.Port != 53 {
		t.Errorf("destination = %+v, want odd/name:53", dst)
	}
}

func TestParseProxyBasicAuth(t *testing.T) {
	valid := basicAuth("user", "pass")
	tests := []struct {
		name   string
		header string
		want   *Credentials
	}{
		{"well formed", valid, &Credentials{Username: "user", Password: "pass"}},
		{"case-insensitive scheme", "basic " + strings.TrimPrefix(valid, "Basic "), &Credentials{Username: "user", Password: "pass"}},
		{"extra whitespace", "Basic   " + strings.TrimPrefix(valid, "Basic "), &Credentials{Username: "user", Password: "pass"}},
		{"empty", "", nil},
		{"wrong scheme", "Bearer token", nil},
		{"not base64", "Basic !!!", nil},
		{"no colon", "Basic " + base64.StdEncoding.EncodeToString([]byte("nocolon")), nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseProxyBasicAuth(tc.header)
			if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
				t.Errorf("parseProxyBasicAuth(%q) = %+v, want %+v", tc.header, got, tc.want)
			}
		})
	}
}

// TestRefusalReasonCannotSplitTheResponse pins the CWE-113 fix: a reason
// carrying CRLF must not become a second header or response.
func TestRefusalReasonCannotSplitTheResponse(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	go func() {
		writeRefusal(server, http.StatusForbidden, "bad\r\nInjected: header\r\n\r\nHTTP/1.1 200 OK")
		_ = server.Close()
	}()

	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatalf("ReadResponse: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
	if got := resp.Header.Get("Injected"); got != "" {
		t.Errorf("Injected header = %q, the reason split the response", got)
	}
}

func TestConnectUDPDeniedByPolicy(t *testing.T) {
	path := startBoundary(t, &ConnectServer{
		Dialer: DialerFunc(func(ctx context.Context, creds *Credentials, dst Destination) (net.Conn, error) {
			return nil, ErrNotAllowed
		}),
	})

	conn := dialRaw(t, path)
	req := &http.Request{
		Method: http.MethodGet,
		URL:    &url.URL{Scheme: "http", Host: "boundary", Path: "/.well-known/masque/udp/evil.example/53/"},
		Host:   "boundary",
		Header: make(http.Header),
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "connect-udp")
	req.Header.Set("Capsule-Protocol", "?1")
	if err := req.Write(conn); err != nil {
		t.Fatalf("write connect-udp: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
}

// TestServeDropsFlowsOnCancel pins that shutdown is prompt. An established
// relay only ends when one side closes, so without this an idle keep-alive
// connection holds the gateway open until some unrelated timeout fires.
func TestServeDropsFlowsOnCancel(t *testing.T) {
	echo := startEcho(t)

	dir, err := os.MkdirTemp("", "sambox")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "agent.sock")

	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	server := &ConnectServer{
		Dialer: DialerFunc(func(ctx context.Context, creds *Credentials, dst Destination) (net.Conn, error) {
			return net.Dial("tcp", echo)
		}),
	}

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- server.Serve(ctx, l) }()

	// Serve is racing this dial; retry briefly until the listener answers.
	var conn net.Conn
	for range 50 {
		conn, _, _ = func() (net.Conn, *bufio.Reader, *http.Response) {
			c, err := net.Dial("unix", socket)
			if err != nil {
				time.Sleep(10 * time.Millisecond)
				return nil, nil, nil
			}
			req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Host: "api.github.com:443"}, Host: "api.github.com:443", Header: make(http.Header)}
			if err := req.Write(c); err != nil {
				_ = c.Close()
				return nil, nil, nil
			}
			br := bufio.NewReader(c)
			if _, err := http.ReadResponse(br, req); err != nil {
				_ = c.Close()
				return nil, nil, nil
			}
			return c, br, nil
		}()
		if conn != nil {
			break
		}
	}
	if conn == nil {
		t.Fatal("could not establish a flow through the boundary")
	}
	defer func() { _ = conn.Close() }()

	// The flow is established and idle, which is the case that used to hang.
	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after cancel; an idle flow is holding shutdown open")
	}
}

func TestRefusalFor(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
	}{
		{"policy denial", ErrNotAllowed, http.StatusForbidden},
		{"unreachable", ErrHostUnreachable, http.StatusBadGateway},
		{"refused", ErrConnectionRefused, http.StatusBadGateway},
		{"wrapped denial", errors.Join(errors.New("context"), ErrNotAllowed), http.StatusForbidden},
		{"anything else stays generic", errors.New("boom"), http.StatusInternalServerError},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := refusalFor(tc.err); got != tc.wantStatus {
				t.Errorf("refusalFor(%v) = %d, want %d", tc.err, got, tc.wantStatus)
			}
		})
	}
}
