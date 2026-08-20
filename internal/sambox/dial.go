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
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
	"time"
)

// dialTimeout bounds opening a destination. The SOCKS5 layer deliberately drops
// its handshake deadline before dialling, so this is the only bound and it has
// to exist here.
const dialTimeout = 30 * time.Second

// AgentDialer opens whatever a Route calls for. It is the only place in the
// sandbox boundary that touches the network, which keeps the routing decision
// (route.go) and the protocol (socks5.go) free of I/O.
type AgentDialer struct {
	// Router classifies destinations. Required.
	Router *Router

	// SidecarSocket is the Unix socket of the sam-node this sandbox is attached
	// to. Flows to node.sam.alt are piped to it verbatim: sam-box parses
	// nothing, so /v1/*, /mcp and /sam/* behave exactly as they do over TCP,
	// including how they treat their own authentication headers.
	SidecarSocket string

	// DialContext opens external destinations. Nil uses a plain net.Dialer;
	// tests and future egress interception replace it.
	DialContext func(ctx context.Context, network, address string) (net.Conn, error)
}

// DialDestination implements Dialer.
func (d *AgentDialer) DialDestination(ctx context.Context, _ *Credentials, dst Destination) (net.Conn, error) {
	if d.Router == nil {
		return nil, errors.New("sambox: AgentDialer requires a Router")
	}

	route, err := d.Router.Route(dst)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	switch route.Kind {
	case RouteLocalNode:
		return d.dial(ctx, "unix", d.SidecarSocket)
	case RouteExternal:
		return d.dial(ctx, "tcp", dst.Address())
	case RouteMeshService:
		return d.dialMeshService(ctx, route)
	default:
		return nil, fmt.Errorf("sambox: unhandled route %v", route.Kind)
	}
}

func (d *AgentDialer) dial(ctx context.Context, network, address string) (net.Conn, error) {
	if address == "" {
		return nil, fmt.Errorf("sambox: no %s address configured", network)
	}

	dial := d.DialContext
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}

	conn, err := dial(ctx, network, address)
	if err != nil {
		return nil, classifyDialError(err)
	}
	return conn, nil
}

// classifyDialError maps a dial failure onto the vocabulary the SOCKS5 layer
// can report, so an agent sees "refused" or "unreachable" rather than a
// generic failure it cannot act on.
func classifyDialError(err error) error {
	var dnsErr *net.DNSError
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return fmt.Errorf("%w: %v", ErrConnectionRefused, err)
	case errors.As(err, &dnsErr),
		errors.Is(err, syscall.EHOSTUNREACH),
		errors.Is(err, syscall.ENETUNREACH):
		return fmt.Errorf("%w: %v", ErrHostUnreachable, err)
	default:
		return err
	}
}
