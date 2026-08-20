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
	"net"
	"net/http"
	"net/http/httputil"
	"strings"

	"github.com/google/sam/api"
)

// The gateway consumes the node; the agent consumes the mesh through the
// gateway. Those are different surfaces and this file is the boundary between
// them.
//
// A sam-node's sidecar API is local and operator-facing: it can register
// services under the node's identity, drive the raw /sam/<peer>/... egress
// proxy at any peer and service the operator chooses, and read node internals.
// Reaching its Unix socket is itself the credential — withAuth treats arriving
// there as proof of authorization, on the grounds that it is the same bar as
// reading the token file. Piping an agent's bytes to that socket would
// therefore hand every sandbox the node's full local authority, so the
// entrypoint terminates HTTP and forwards only what an agent is supposed to
// have.

// agentMayReach is the entire surface an agent gets. Inference and tools, and
// nothing else.
//
// Discovery is not on the list even though agents need it: it is already
// available through MCP as find_remote_tools and discover_remote_services, so
// exposing /sam/service/discover as well would widen the surface without adding
// a capability. Registration is not on the list at all — an agent that could
// register would advertise itself into the mesh under the node's identity, and
// choose the target_url the mesh then routes to. Ingress is declared by the
// platform through the connector interface, which is the only party that knows
// what an agent is supposed to serve.
func agentMayReach(path string) bool {
	switch path {
	case "/v1/models", "/v1/chat/completions", "/v1/completions":
		return true
	}
	return path == "/mcp" || strings.HasPrefix(path, "/mcp/")
}

// dialMeshEntrypoint returns a connection serving the agent-facing surface.
func (d *AgentDialer) dialMeshEntrypoint() (net.Conn, error) {
	if d.SidecarSocket == "" {
		return nil, ErrHostUnreachable
	}
	return serveOnPipe(d.entrypointHandler()), nil
}

func (d *AgentDialer) entrypointHandler() http.Handler {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL.Scheme = "http"
			r.Out.URL.Host = sidecarHost
			r.Out.Host = sidecarHost

			// Headers that assert an identity the agent does not have. The
			// sidecar would honour them: X-Sam-Biscuit is the mesh datapath
			// credential, and X-Sam-Authentication is the node's local gate.
			// Authorization is left alone, because there it means the
			// destination service's own credential and is the agent's to send.
			r.Out.Header.Del(api.HeaderSamBiscuit)
			r.Out.Header.Del(api.HeaderSamAuthentication)
		},
		Transport: d.sidecarTransport(),
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !agentMayReach(r.URL.Path) {
			http.Error(w, "the mesh entrypoint serves /v1 and /mcp only", http.StatusForbidden)
			return
		}
		proxy.ServeHTTP(w, r)
	})
}
