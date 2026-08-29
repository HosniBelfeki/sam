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
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/sam/api"
)

func TestA2AServiceInitRejectsCommand(t *testing.T) {
	svc := &A2AService{baseService: baseService{
		info:    &api.ServiceInfo{Type: api.ServiceType_SERVICE_TYPE_A2A, Name: "agent"},
		backend: &api.RegisterServiceRequest_Command{Command: &api.CommandBackend{Command: []string{"echo"}}},
	}}
	if err := svc.Init(context.Background()); err == nil {
		t.Fatal("command backend must be rejected for a2a services")
	}
}

func TestA2AServiceInitURLBackend(t *testing.T) {
	svc := &A2AService{baseService: baseService{
		info:    &api.ServiceInfo{Type: api.ServiceType_SERVICE_TYPE_A2A, Name: "agent"},
		backend: &api.RegisterServiceRequest_TargetUrl{TargetUrl: "http://127.0.0.1:9999"},
	}}
	if err := svc.Init(context.Background()); err != nil {
		t.Fatalf("url backend must be accepted: %v", err)
	}
	if svc.Handler() == nil {
		t.Fatal("nil handler after Init")
	}
}

func TestNewServiceFromRequestA2A(t *testing.T) {
	req := &api.RegisterServiceRequest{
		Service: &api.ServiceInfo{Type: api.ServiceType_SERVICE_TYPE_A2A, Name: "agent"},
		Backend: &api.RegisterServiceRequest_TargetUrl{TargetUrl: "http://127.0.0.1:9999"},
	}
	svc, err := NewServiceFromRequest(req)
	if err != nil {
		t.Fatalf("factory must accept a2a: %v", err)
	}
	if _, ok := svc.(*A2AService); !ok {
		t.Fatalf("factory returned %T, want *A2AService", svc)
	}
}

func TestA2AEgressHookNonA2APassthrough(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/sam/12D3KooWpeer/mcp/svc/foo", nil)
	req.Header.Set(api.HeaderSamRequiredLabels, "region=eu")
	_, ok := a2aEgressHook(nil, rec, req)
	if !ok {
		t.Fatal("non-a2a path must pass through")
	}
	if req.Header.Get(api.HeaderSamRequiredLabels) == "" {
		t.Fatal("labels header on non-a2a path must be left untouched")
	}
}

func TestA2AEgressHookMalformedLabels(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/sam/12D3KooWpeer/a2a/agent/", nil)
	req.Header.Set(api.HeaderSamRequiredLabels, "not-a-label")
	_, ok := a2aEgressHook(nil, rec, req)
	if ok {
		t.Fatal("malformed labels must be refused")
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestA2AEgressHookTagsCardFetch(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/sam/12D3KooWpeer/a2a/agent/.well-known/agent-card.json", nil)
	req.Host = "127.0.0.1:8080"
	r2, ok := a2aEgressHook(nil, rec, req)
	if !ok {
		t.Fatal("card fetch must pass through")
	}
	base, _ := r2.Context().Value(a2aCardBaseURL{}).(string)
	want := "http://127.0.0.1:8080/sam/12D3KooWpeer/a2a/agent"
	if base != want {
		t.Fatalf("card base = %q, want %q", base, want)
	}
}

func TestRewriteA2AAgentCard(t *testing.T) {
	card := `{
	  "name": "T",
	  "url": "http://localhost:9999",
	  "preferredTransport": "GRPC",
	  "additionalInterfaces": [
	    {"url": "http://localhost:9999", "transport": "JSONRPC"},
	    {"url": "localhost:50051", "transport": "GRPC"}
	  ],
	  "supportedInterfaces": [
	    {"url": "http://localhost:9999", "protocolBinding": "JSONRPC"},
	    {"url": "localhost:50051", "protocolBinding": "GRPC"}
	  ],
	  "capabilities": {"streaming": true}
	}`
	base := "http://127.0.0.1:8080/sam/12D3KooWpeer/a2a/agent"
	req := httptest.NewRequest("GET", "/sam/12D3KooWpeer/a2a/agent/.well-known/agent-card.json", nil)
	req = req.WithContext(context.WithValue(req.Context(), a2aCardBaseURL{}, base))
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(card)),
		Request:    req,
	}
	if err := rewriteA2AAgentCard(resp); err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("rewritten card is not JSON: %v", err)
	}
	if got["url"] != base {
		t.Errorf("url = %v, want %s", got["url"], base)
	}
	if got["preferredTransport"] != "JSONRPC" {
		t.Errorf("preferredTransport = %v, want JSONRPC", got["preferredTransport"])
	}
	for _, key := range []string{"additionalInterfaces", "supportedInterfaces"} {
		ifaces, _ := got[key].([]any)
		if len(ifaces) != 1 {
			t.Fatalf("%s: want 1 HTTP interface after dropping gRPC, got %v", key, got[key])
		}
		if u := ifaces[0].(map[string]any)["url"]; u != base {
			t.Errorf("%s url = %v, want %s", key, u, base)
		}
	}
	if s := got["capabilities"].(map[string]any)["streaming"]; s != false {
		t.Errorf("streaming = %v, want false", s)
	}
}

func TestRewriteA2AAgentCardNoopWithoutTag(t *testing.T) {
	orig := `{"name":"T","url":"http://localhost:9999"}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(orig)),
		Request:    httptest.NewRequest("GET", "/anything", nil),
	}
	if err := rewriteA2AAgentCard(resp); err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != orig {
		t.Fatalf("untagged response was modified: %s", body)
	}
}
