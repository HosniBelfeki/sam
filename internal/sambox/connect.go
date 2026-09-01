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
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Named HTTP tunnels are the sandbox boundary protocol: authority-form CONNECT
// (RFC 9110) for TCP, connect-udp (RFC 9298) with capsules (RFC 9297) for UDP.
// A tunnel request carries the destination *name*, so egress policy is decided
// on "api.github.com" rather than on an address that says nothing about who is
// being talked to — the property the old SOCKS5 boundary was chosen for, kept.
//
// What CONNECT adds is symmetry and headroom. The reverse channel into a
// sandbox already speaks `CONNECT <port>`, so with this the boundary is one
// protocol in both directions; a refusal is a status code with a
// Boundary-Reason header rather than a bare reply byte; headers are the
// extension point identity and tracing arrive through; and connect-udp gives
// UDP a *named*, policy-checked shape, where SOCKS5's UDP ASSOCIATE never fit
// a one-socket boundary at all. The guest side is the tun2connect library,
// and `curl --proxy` speaks the TCP half of it natively.

// handshakeTimeout bounds reading the tunnel request only. Once a flow is
// established it may stream for as long as it likes, so no deadline survives
// into the relay.
const handshakeTimeout = 10 * time.Second

// Errors a Dialer returns to select a refusal status. Anything else becomes a
// general failure, which is the right default: an unrecognised failure must
// not be reported to a sandbox as a precise diagnostic.
var (
	// ErrNotAllowed is a policy denial: the destination is not permitted.
	ErrNotAllowed = errors.New("connection not allowed by ruleset")

	// ErrHostUnreachable means the destination could not be resolved or routed.
	ErrHostUnreachable = errors.New("host unreachable")

	// ErrConnectionRefused means the destination actively refused the flow.
	ErrConnectionRefused = errors.New("connection refused")
)

// Destination is a requested target exactly as it arrived on the sandbox
// boundary. Name is a domain when the client sent one, which is the case for
// every flow that came through tun2connect's virtual DNS; a literal address
// arrives when a client dialled an IP directly, and IsName says which.
type Destination struct {
	Name   string
	Port   uint16
	IsName bool

	// Network is "tcp" for a CONNECT tunnel and "udp" for a connect-udp
	// session. Empty means "tcp", so the zero value stays the common case.
	Network string
}

// Address renders the destination as a dial target.
func (d Destination) Address() string {
	return net.JoinHostPort(d.Name, strconv.Itoa(int(d.Port)))
}

func (d Destination) String() string { return d.Address() }

// network is Network with the zero value made explicit.
func (d Destination) network() string {
	if d.Network == "" {
		return "tcp"
	}
	return d.Network
}

// Credentials are the Proxy-Authorization Basic username and password. When
// one sam-box multiplexes several agents over a single socket, this is how a
// flow says which agent it belongs to; the password is never logged.
type Credentials struct {
	Username string
	Password string
}

// Dialer decides whether a requested destination may be reached and opens it.
// It is the single policy enforcement point on the sandbox boundary.
type Dialer interface {
	DialDestination(ctx context.Context, creds *Credentials, dst Destination) (net.Conn, error)
}

// DialerFunc adapts a function to Dialer.
type DialerFunc func(ctx context.Context, creds *Credentials, dst Destination) (net.Conn, error)

func (f DialerFunc) DialDestination(ctx context.Context, creds *Credentials, dst Destination) (net.Conn, error) {
	return f(ctx, creds, dst)
}

// ConnectServer serves the sandbox-facing side of the boundary.
type ConnectServer struct {
	// Dialer is required.
	Dialer Dialer

	// Authenticate, when set, makes Proxy-Authorization Basic credentials the
	// only acceptable greeting: a client that offers none is answered 407
	// rather than silently downgraded to an anonymous flow.
	Authenticate func(Credentials) error
}

// Serve accepts connections until the listener fails or ctx is cancelled.
func (s *ConnectServer) Serve(ctx context.Context, l net.Listener) error {
	if s.Dialer == nil {
		return errors.New("sambox: ConnectServer requires a Dialer")
	}

	go func() {
		<-ctx.Done()
		_ = l.Close()
	}()

	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Cancelling must drop flows, not wait them out: an established
			// relay only ends when one side closes, so an idle keep-alive
			// connection would otherwise hold shutdown open until some other
			// timeout fires.
			stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
			defer stop()
			s.handle(ctx, conn)
		}()
	}
}

func (s *ConnectServer) handle(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()

	if err := conn.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		return
	}

	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil {
		// Not HTTP at all: there is nothing well-formed to answer with.
		return
	}

	creds, err := s.credentials(req)
	if err != nil {
		writeRefusal(conn, http.StatusProxyAuthRequired, "credentials required",
			"Proxy-Authenticate: Basic realm=\"sam-box\"")
		return
	}

	var dst Destination
	switch {
	case req.Method == http.MethodConnect:
		dst, err = connectDestination(req.Host)
	case req.Method == http.MethodGet && strings.EqualFold(req.Header.Get("Upgrade"), "connect-udp"):
		// EscapedPath, not Path: the parser has already unescaped Path, so a
		// %2F inside a segment would change the segment count. The segments
		// are unescaped individually after splitting.
		dst, err = masqueDestination(req.URL.EscapedPath())
	default:
		writeRefusal(conn, http.StatusMethodNotAllowed, "the boundary speaks CONNECT and connect-udp only")
		return
	}
	if err != nil {
		writeRefusal(conn, http.StatusBadRequest, err.Error())
		return
	}

	// The request is read. Dialling a mesh destination can involve discovery,
	// so it must not inherit the handshake deadline; bounding it is the
	// Dialer's job, through the context it is given.
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return
	}

	upstream, err := s.Dialer.DialDestination(ctx, creds, dst)
	if err != nil {
		status, reason := refusalFor(err)
		writeRefusal(conn, status, reason)
		return
	}
	defer func() { _ = upstream.Close() }()

	if dst.network() == "udp" {
		if _, err := io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\n"+
			"Connection: Upgrade\r\nUpgrade: connect-udp\r\nCapsule-Protocol: ?1\r\n\r\n"); err != nil {
			return
		}
		pumpUDP(NewCapsuleStream(&bufConn{Conn: conn, br: br}), upstream)
		return
	}

	if _, err := io.WriteString(conn, "HTTP/1.1 200 OK\r\n\r\n"); err != nil {
		return
	}
	// br first: it may hold bytes the client pipelined behind the request.
	relay(&bufConn{Conn: conn, br: br}, upstream)
}

// credentials parses Proxy-Authorization and applies Authenticate when set.
func (s *ConnectServer) credentials(req *http.Request) (*Credentials, error) {
	creds := parseProxyBasicAuth(req.Header.Get("Proxy-Authorization"))
	if s.Authenticate == nil {
		return creds, nil
	}
	if creds == nil {
		return nil, errors.New("credentials required")
	}
	if err := s.Authenticate(*creds); err != nil {
		log.Printf("sambox: boundary authentication rejected for user %q", creds.Username)
		return nil, err
	}
	return creds, nil
}

// parseProxyBasicAuth decodes "Basic <base64(user:pass)>", or returns nil.
func parseProxyBasicAuth(header string) *Credentials {
	fields := strings.Fields(header)
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Basic") {
		return nil
	}
	decoded, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil {
		return nil
	}
	username, password, ok := strings.Cut(string(decoded), ":")
	if !ok {
		return nil
	}
	return &Credentials{Username: username, Password: password}
}

// connectDestination parses the authority-form CONNECT target.
func connectDestination(hostport string) (Destination, error) {
	host, rawPort, err := net.SplitHostPort(hostport)
	if err != nil || host == "" {
		return Destination{}, fmt.Errorf("malformed CONNECT target %q", hostport)
	}
	port, err := strconv.ParseUint(rawPort, 10, 16)
	if err != nil {
		return Destination{}, fmt.Errorf("malformed CONNECT port %q", rawPort)
	}
	_, isAddr := parseAddr(host)
	return Destination{Name: host, Port: uint16(port), IsName: !isAddr, Network: "tcp"}, nil
}

// masqueDestination parses the default connect-udp URI template
// /.well-known/masque/udp/{host}/{port}/ (RFC 9298 section 2).
func masqueDestination(path string) (Destination, error) {
	seg := strings.Split(strings.Trim(path, "/"), "/")
	if len(seg) != 5 || seg[0] != ".well-known" || seg[1] != "masque" || seg[2] != "udp" {
		return Destination{}, fmt.Errorf("malformed connect-udp template %q", path)
	}
	host, err := url.PathUnescape(seg[3])
	if err != nil || host == "" {
		return Destination{}, fmt.Errorf("malformed connect-udp target host")
	}
	port, err := strconv.ParseUint(seg[4], 10, 16)
	if err != nil {
		return Destination{}, fmt.Errorf("malformed connect-udp port %q", seg[4])
	}
	_, isAddr := parseAddr(host)
	return Destination{Name: host, Port: uint16(port), IsName: !isAddr, Network: "udp"}, nil
}

func parseAddr(host string) (netip.Addr, bool) {
	addr, err := netip.ParseAddr(host)
	return addr, err == nil
}

// writeRefusal answers a request that will not become a tunnel. The status is
// what a plain HTTP client sees; Boundary-Reason is what a log is read
// against, and "not allowed by policy" has to be legible as a decision rather
// than looking like the mesh being broken.
func writeRefusal(conn net.Conn, status int, reason string, extraHeaders ...string) {
	// The reason can quote request input, and a CR or LF in a header value
	// is response splitting (CWE-113); strip them at the sink.
	reason = strings.NewReplacer("\r", "", "\n", "").Replace(reason)
	msg := fmt.Sprintf("HTTP/1.1 %d %s\r\nBoundary-Reason: %s\r\n",
		status, http.StatusText(status), reason)
	for _, h := range extraHeaders {
		msg += h + "\r\n"
	}
	msg += "Content-Length: 0\r\n\r\n"
	_, _ = io.WriteString(conn, msg)
}

func refusalFor(err error) (int, string) {
	switch {
	case errors.Is(err, ErrNotAllowed):
		return http.StatusForbidden, "not allowed by policy"
	case errors.Is(err, ErrHostUnreachable):
		return http.StatusBadGateway, "host unreachable"
	case errors.Is(err, ErrConnectionRefused):
		return http.StatusBadGateway, "connection refused"
	default:
		return http.StatusInternalServerError, "general failure"
	}
}

// pumpUDP carries one connect-udp session: capsules from the client become
// datagrams upstream and back, until either side ends the session.
func pumpUDP(cs *CapsuleStream, upstream net.Conn) {
	go func() {
		defer func() { _ = upstream.Close() }()
		for {
			p, err := cs.ReadDatagram()
			if err != nil {
				return
			}
			if _, err := upstream.Write(p); err != nil {
				return
			}
		}
	}()
	buf := make([]byte, 65535)
	for {
		n, err := upstream.Read(buf)
		if err != nil {
			return
		}
		if cs.WriteDatagram(buf[:n]) != nil {
			return
		}
	}
}

// bufConn keeps bytes the request reader buffered past the header visible to
// the relay, and keeps the half-close the relay depends on reachable.
type bufConn struct {
	net.Conn
	br *bufio.Reader
}

func (c *bufConn) Read(p []byte) (int, error) { return c.br.Read(p) }

func (c *bufConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Close()
}

func relay(client, upstream net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(upstream, client)
		closeWrite(upstream)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(client, upstream)
		closeWrite(client)
	}()
	wg.Wait()
}

// closeWrite propagates a half-close so a peer waiting on EOF is not left
// hanging until a timeout.
func closeWrite(conn net.Conn) {
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = conn.Close()
}
