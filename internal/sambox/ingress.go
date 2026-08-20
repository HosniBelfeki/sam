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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/google/sam/api"
)

// An agent that serves is still not allowed to talk to the node. Registering a
// service means naming a target_url the mesh will route to and a name it will
// route under, and an agent holding either would be able to point the mesh at
// anything and to advertise itself as somebody else's service. So the gateway
// offers the capability without the API: the agent says it is ready, and the
// gateway does the registering.
//
// Two things make that safe. The target_url is never the agent's to give: it is
// always this gateway's own ingress address. And the name must be one the
// platform already granted in the bundle, so claiming a name nobody granted is
// not expressible rather than merely refused.
//
// Splitting it this way is also what makes the timing work. The platform knows
// what an agent may serve; only the agent knows when it is listening. A route
// declared up front advertises a service that is not up yet, and the mesh
// routes to it and fails.

// maxIngressBody bounds the declaration an agent posts.
const maxIngressBody = 4 << 10

// IngressManager registers what an agent serves, and forwards what arrives.
type IngressManager struct {
	// SidecarSocket is the node's API socket, where registrations are made.
	SidecarSocket string

	// Allowed is what the bundle permits this agent to serve. Empty means the
	// agent may serve nothing.
	Allowed []BundleIngress

	// AgentAddr resolves where the agent listens inside its sandbox. It exists
	// because reaching in is topology-dependent: a sandbox sharing a network
	// namespace with the gateway is reachable on loopback, while an isolated
	// one needs a reverse channel that does not exist yet.
	AgentAddr func(port int) string

	mu       sync.Mutex
	listener net.Listener
	routes   map[string]int // service name -> port inside the sandbox
}

// ingressDeclaration is what an agent posts to say it is ready.
type ingressDeclaration struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Port int    `json:"port"`
}

// Handler serves the gateway's own ingress endpoint. It is never forwarded to
// the node.
func (m *IngressManager) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST a declaration to announce a service", http.StatusMethodNotAllowed)
			return
		}

		var decl ingressDeclaration
		if err := json.NewDecoder(io.LimitReader(r.Body, maxIngressBody)).Decode(&decl); err != nil {
			http.Error(w, "expected {\"name\":..., \"type\":..., \"port\":...}", http.StatusBadRequest)
			return
		}

		if err := m.Announce(r.Context(), decl); err != nil {
			// The agent is told what it did wrong, which is always something it
			// declared, never anything about the node behind the gateway.
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

// Announce validates a declaration, starts routing to the agent, and registers
// the service with the node.
func (m *IngressManager) Announce(ctx context.Context, decl ingressDeclaration) error {
	if decl.Port < 1 || decl.Port > 65535 {
		return fmt.Errorf("port %d is not a port", decl.Port)
	}
	if !m.permits(decl) {
		return fmt.Errorf("this agent was not granted %s://%s", decl.Type, decl.Name)
	}

	addr, err := m.ensureListening()
	if err != nil {
		return fmt.Errorf("serving ingress: %w", err)
	}

	m.mu.Lock()
	m.routes[decl.Name] = decl.Port
	m.mu.Unlock()

	// The agent named a service; the gateway names where the mesh reaches it.
	target := "http://" + addr + "/" + decl.Name
	if err := m.register(ctx, decl, target); err != nil {
		m.mu.Lock()
		delete(m.routes, decl.Name)
		m.mu.Unlock()
		return fmt.Errorf("registering %s://%s: %w", decl.Type, decl.Name, err)
	}
	log.Printf("sambox: serving %s://%s from the sandbox's port %d", decl.Type, decl.Name, decl.Port)
	return nil
}

func (m *IngressManager) permits(decl ingressDeclaration) bool {
	for _, allowed := range m.Allowed {
		if allowed.Name == decl.Name && allowed.Type == decl.Type {
			return true
		}
	}
	return false
}

// ensureListening starts the ingress listener on first use, so an agent that
// serves nothing costs nothing.
func (m *IngressManager) ensureListening() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.listener != nil {
		return m.listener.Addr().String(), nil
	}
	if m.routes == nil {
		m.routes = make(map[string]int)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	m.listener = listener

	server := &http.Server{Handler: m.forwarder(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		_ = server.Serve(listener)
	}()
	return listener.Addr().String(), nil
}

// forwarder carries what the node delivers into the sandbox, stripping the
// service name the gateway added so the agent sees the path it published.
func (m *IngressManager) forwarder() http.Handler {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			name, rest := splitServicePath(r.In.URL.Path)

			m.mu.Lock()
			port, known := m.routes[name]
			m.mu.Unlock()
			if !known {
				// Nothing to route to; the proxy reports a failure rather than
				// dialling something arbitrary.
				r.Out.URL = &url.URL{Scheme: "http", Host: "ingress.invalid"}
				return
			}

			r.Out.URL.Scheme = "http"
			r.Out.URL.Host = m.agentAddr(port)
			r.Out.Host = r.Out.URL.Host
			r.Out.URL.Path = rest
			r.Out.URL.RawPath = ""
		},
	}
	return proxy
}

func (m *IngressManager) agentAddr(port int) string {
	if m.AgentAddr != nil {
		return m.AgentAddr(port)
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
}

// register tells the node to advertise the service, with a target only the
// gateway could have chosen.
func (m *IngressManager) register(ctx context.Context, decl ingressDeclaration, target string) error {
	serviceType, err := api.ParseServiceType(decl.Type)
	if err != nil {
		return err
	}

	body, err := protojson.Marshal(&api.RegisterServiceRequest{
		Service: &api.ServiceInfo{
			Type:        serviceType,
			Name:        decl.Name,
			Description: "served by an agent behind this gateway",
		},
		Backend: &api.RegisterServiceRequest_TargetUrl{TargetUrl: target},
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+sidecarHost+"/sam/service/register", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Transport: sidecarTransport(m.SidecarSocket)}).Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, maxIngressBody))
		return fmt.Errorf("the node refused it: %s: %s", resp.Status, bytes.TrimSpace(detail))
	}
	return nil
}

// Close stops serving and withdraws everything this gateway advertised, so a
// detached sandbox stops being routed to instead of lingering in discovery.
func (m *IngressManager) Close(ctx context.Context) {
	m.mu.Lock()
	listener := m.listener
	m.listener = nil
	routes := m.routes
	m.routes = nil
	m.mu.Unlock()

	if listener != nil {
		_ = listener.Close()
	}
	for name := range routes {
		if err := m.unregister(ctx, name); err != nil {
			log.Printf("sambox: withdrawing %s: %v", name, err)
		}
	}
}

func (m *IngressManager) unregister(ctx context.Context, name string) error {
	body, err := json.Marshal(map[string]string{"name": name})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+sidecarHost+"/sam/service/unregister", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Transport: sidecarTransport(m.SidecarSocket)}).Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the node returned %s", resp.Status)
	}
	return nil
}

// splitServicePath separates the leading service name from the rest of the path.
func splitServicePath(path string) (name, rest string) {
	trimmed := path
	if len(trimmed) > 0 && trimmed[0] == '/' {
		trimmed = trimmed[1:]
	}
	for i := 0; i < len(trimmed); i++ {
		if trimmed[i] == '/' {
			return trimmed[:i], trimmed[i:]
		}
	}
	return trimmed, "/"
}
