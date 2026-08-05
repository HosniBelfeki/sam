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

package node

import (
	"context"

	"github.com/google/sam/api"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPService extends baseService to handle MCP protocol proxying.
type MCPService struct {
	baseService
}

// Init initializes the base service.
func (m *MCPService) Init(ctx context.Context) error {
	return m.baseService.Init(ctx)
}

// Teardown chains to baseService.Teardown.
func (m *MCPService) Teardown() error {
	return m.baseService.Teardown()
}

// methodServerDiscover is the SEP-2575 stateless capability probe that
// go-sdk clients (>= v1.7.0) send before "initialize", falling back to the
// legacy handshake if it's rejected. See:
// https://github.com/modelcontextprotocol/modelcontextprotocol/pull/2575
const methodServerDiscover = "server/discover"

// HandleStreamPassThrough connects to the backend and proxies JSON-RPC messages.
func (m *MCPService) HandleStreamPassThrough(s network.Stream) {
	defer func() {
		if err := s.Close(); err != nil {
			logger.Debugf("[MCPService] Failed to close MCP stream: %v", err)
		}
	}()

	var backendTransport mcp.Transport
	var closeTransport func()

	switch x := m.backend.(type) {
	case *api.RegisterServiceRequest_TargetUrl:
		backendTransport = &mcp.StreamableClientTransport{Endpoint: x.TargetUrl}
		closeTransport = func() {} // ClientTransport Close is handled by Connect's returned Connection
	case *api.RegisterServiceRequest_Command:
		bridge, ok := m.handler.(*StdioBridge)
		if !ok {
			logger.Errorf("[MCPService] %s: expected *StdioBridge handler for command-backed MCP service, got %T", m.info.Name, m.handler)
			return
		}
		backendTransport = newBridgeTransport(bridge)
		closeTransport = func() {} // Do not close the shared bridge
	default:
		logger.Errorf("[MCPService] %s: unsupported backend type %T", m.info.Name, m.backend)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() {
		if closeTransport != nil {
			closeTransport()
		}
	}()

	backendConn, err := backendTransport.Connect(ctx)
	if err != nil {
		logger.Errorf("[MCPService] %s: failed to connect to backend: %v", m.info.Name, err)
		return
	}
	defer func() { _ = backendConn.Close() }()

	clientTransport := NewStreamTransport(s)
	clientConn, err := clientTransport.Connect(ctx)
	if err != nil {
		logger.Errorf("[MCPService] %s: failed to connect to client: %v", m.info.Name, err)
		return
	}

	// Dumb pipe: Proxy JSON-RPC messages between client and backend
	errc := make(chan error, 2)

	go func() {
		for {
			msg, err := clientConn.Read(ctx)
			if err != nil {
				logger.Debugf("[MCPService] %s: client read error: %v", m.info.Name, err)
				errc <- err
				return
			}
			// This pipe opens a fresh, sessionless connection to the backend
			// per stream, so it can't answer the "server/discover" probe the
			// way a stateful server would. Reject it locally so the client's
			// own documented fallback to "initialize" runs on this same
			// connection, instead of forwarding a call the backend may not
			// understand and losing the stream entirely.
			if req, ok := msg.(*jsonrpc.Request); ok && req.Method == methodServerDiscover {
				resp := &jsonrpc.Response{ID: req.ID, Error: &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "server/discover is not supported by this pass-through proxy"}}
				if werr := clientConn.Write(ctx, resp); werr != nil {
					logger.Debugf("[MCPService] %s: failed to reject server/discover: %v", m.info.Name, werr)
					errc <- werr
					return
				}
				continue
			}
			if err := backendConn.Write(ctx, msg); err != nil {
				logger.Debugf("[MCPService] %s: backend write error: %v", m.info.Name, err)
				errc <- err
				return
			}
		}
	}()

	go func() {
		for {
			msg, err := backendConn.Read(ctx)
			if err != nil {
				logger.Debugf("[MCPService] %s: backend read error: %v", m.info.Name, err)
				errc <- err
				return
			}
			if err := clientConn.Write(ctx, msg); err != nil {
				logger.Debugf("[MCPService] %s: client write error: %v", m.info.Name, err)
				errc <- err
				return
			}
		}
	}()

	<-errc
}
