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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/sam/api"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// newProbeBackend serves a real MCP server that exposes no tools at all.
func newProbeBackend(t *testing.T) *httptest.Server {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "probe-backend", Version: "0.0.1"}, nil)
	ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	t.Cleanup(ts.Close)
	return ts
}

func newProbeService(url string) *MCPService {
	return &MCPService{baseService: baseService{
		info:    &api.ServiceInfo{Name: "probe-me", Type: api.ServiceType_SERVICE_TYPE_MCP},
		backend: &api.RegisterServiceRequest_TargetUrl{TargetUrl: url},
	}}
}

// Serving no tools is not a fault: a backend may serve only resources or
// prompts and is still an MCP server, so it must stay advertisable.
func TestMCPService_ProbeAcceptsToollessBackend(t *testing.T) {
	svc := newProbeService(newProbeBackend(t).URL)
	if err := svc.Probe(context.Background()); err != nil {
		t.Errorf("Probe on a tool-less MCP backend: %v", err)
	}
}

// The canary case: answers HTTP, is not an MCP server.
func TestMCPService_ProbeRejectsNonMCPBackend(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(ts.Close)

	svc := newProbeService(ts.URL)
	if err := svc.Probe(context.Background()); err == nil {
		t.Fatal("Probe accepted a backend that is not an MCP server")
	}
	if svc.probeExpires.IsZero() {
		t.Error("a real negative result should be cached")
	}
}

// A cancelled context is a fact about the caller, not the backend. Caching it
// would withhold a healthy service for the whole TTL.
func TestMCPService_ProbeDoesNotCacheContextErrors(t *testing.T) {
	svc := newProbeService(newProbeBackend(t).URL)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := svc.Probe(cancelled); err == nil {
		t.Fatal("Probe returned nil for a cancelled context")
	}
	if !svc.probeExpires.IsZero() {
		t.Fatal("a context error was cached")
	}

	if err := svc.Probe(context.Background()); err != nil {
		t.Errorf("Probe after a cancelled one: %v", err)
	}
}

// The same, for a context that is alive on entry and expires mid-dial: this is
// the slow backend the gating is explicitly meant not to punish.
func TestMCPService_ProbeDoesNotCacheTimeouts(t *testing.T) {
	// Answers, but never within the probe's deadline.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(150 * time.Millisecond)
		http.Error(w, "too slow", http.StatusServiceUnavailable)
	}))
	t.Cleanup(ts.Close)

	svc := newProbeService(ts.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	if err := svc.Probe(ctx); err == nil {
		t.Fatal("Probe returned nil against a backend slower than the deadline")
	}
	if !svc.probeExpires.IsZero() {
		t.Error("a timeout was cached: a slow backend would stay withheld for the whole TTL")
	}
}
