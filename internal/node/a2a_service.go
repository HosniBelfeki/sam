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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/sam/api"
	"github.com/libp2p/go-libp2p/core/peer"
)

// A2AService proxies Agent2Agent (A2A) JSON-RPC/REST traffic to a local
// agent process. URL backends only: a command backend would wire the A2A
// route to an MCP stdio bridge no A2A client can talk to.
type A2AService struct{ baseService }

func (s *A2AService) Init(ctx context.Context) error {
	switch x := s.backend.(type) {
	case *api.RegisterServiceRequest_TargetUrl:
		h, err := newReverseProxyHandler(x.TargetUrl)
		if err != nil {
			return err
		}
		s.handler = h
	case *api.RegisterServiceRequest_Command:
		return fmt.Errorf("command-based backends are not supported for A2AService")
	default:
		return fmt.Errorf("unsupported backend type %T for A2AService", s.backend)
	}
	return nil
}

func init() {
	registerEgressMiddleware(api.ServiceTypeStringA2A, egressMiddleware{
		gateRequest:    a2aEgressGate,
		modifyResponse: rewriteA2AAgentCard,
	})
}

// a2aCardBaseURL is the context key carrying the caller-facing mesh base URL
// of an agent-card fetch, set by a2aEgressGate and consumed by the rewrite.
type a2aCardBaseURL struct{}

// a2aEgressGate runs the caller-side A2A checks on a raw egress request:
// the fail-closed labels gate and tagging agent-card fetches for rewrite.
// On refusal it writes the HTTP error itself and returns ok=false.
func a2aEgressGate(node *SamNode, w http.ResponseWriter, r *http.Request, route egressRoute) (*http.Request, bool) {
	if labelsHeader := r.Header.Get(api.HeaderSamRequiredLabels); labelsHeader != "" {
		r.Header.Del(api.HeaderSamRequiredLabels)
		required, err := parseRequiredLabels(labelsHeader)
		if err != nil {
			http.Error(w, fmt.Sprintf("Invalid %s header: %v", api.HeaderSamRequiredLabels, err), http.StatusBadRequest)
			return r, false
		}
		pid, err := peer.Decode(route.peerID)
		if err != nil {
			http.Error(w, "Invalid peer ID", http.StatusBadRequest)
			return r, false
		}
		if err := node.VerifyPeerLabels(r.Context(), pid, required); err != nil {
			logger.Warnf("[A2A] label gate refused egress to %s: %v", route.peerID, err)
			http.Error(w, "Required labels not attested by provider", http.StatusForbidden)
			return r, false
		}
	}
	if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/.well-known/agent-card.json") {
		base := fmt.Sprintf("http://%s/sam/%s/%s/%s", r.Host, route.peerID, route.serviceType, route.serviceName)
		r = r.WithContext(context.WithValue(r.Context(), a2aCardBaseURL{}, base))
	}
	return r, true
}

// rewriteA2AAgentCard makes a proxied agent card usable by stock A2A clients:
// interface URLs point back at the mesh path, transports the mesh cannot
// carry are dropped, and streaming is advertised off until verified.
func rewriteA2AAgentCard(resp *http.Response) error {
	base, ok := resp.Request.Context().Value(a2aCardBaseURL{}).(string)
	if !ok || resp.StatusCode != http.StatusOK {
		return nil
	}
	if resp.Header.Get("Content-Encoding") != "" {
		logger.Warnf("[A2A] agent card response is content-encoded; skipping rewrite")
		return nil
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return err
	}
	var card map[string]any
	if err := json.Unmarshal(body, &card); err != nil {
		return fmt.Errorf("agent card is not valid JSON: %w", err)
	}
	if _, ok := card["url"]; ok {
		card["url"] = base
	}
	if pt, ok := card["preferredTransport"].(string); ok && !a2aTransportOverHTTP(pt) {
		card["preferredTransport"] = "JSONRPC"
	}
	for _, key := range []string{"additionalInterfaces", "supportedInterfaces"} {
		ifaces, ok := card[key].([]any)
		if !ok {
			continue
		}
		kept := make([]any, 0, len(ifaces))
		for _, entry := range ifaces {
			iface, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			transport, _ := iface["transport"].(string)
			if transport == "" {
				transport, _ = iface["protocolBinding"].(string)
			}
			if !a2aTransportOverHTTP(transport) {
				continue
			}
			iface["url"] = base
			kept = append(kept, iface)
		}
		card[key] = kept
	}
	if caps, ok := card["capabilities"].(map[string]any); ok {
		caps["streaming"] = false
	}
	out, err := json.Marshal(card)
	if err != nil {
		return err
	}
	resp.Body = io.NopCloser(bytes.NewReader(out))
	resp.ContentLength = int64(len(out))
	resp.Header.Set("Content-Length", strconv.Itoa(len(out)))
	return nil
}

// a2aTransportOverHTTP reports whether an A2A transport can traverse the
// mesh's HTTP-over-libp2p path; gRPC needs its own end-to-end connection.
func a2aTransportOverHTTP(transport string) bool {
	return transport == "JSONRPC" || transport == "HTTP+JSON"
}
