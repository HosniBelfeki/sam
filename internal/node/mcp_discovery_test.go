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
	"fmt"
	"net/http/httptest"
	"reflect"
	"sort"
	"testing"

	"github.com/google/sam/api"
	samdiscovery "github.com/google/sam/internal/node/discovery"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPService_Tools(t *testing.T) {
	backend := httptest.NewServer(newFakeMCPHandler(t, []*mcp.Tool{
		{Name: "zeta", Description: "z", InputSchema: map[string]any{"type": "object"}},
		{Name: "alpha", Description: "a", InputSchema: map[string]any{"type": "object"}},
	}))
	defer backend.Close()

	svc := &MCPService{baseService: baseService{
		info:    &api.ServiceInfo{Type: api.ServiceType_SERVICE_TYPE_MCP, Name: "tools-svc"},
		backend: &api.RegisterServiceRequest_TargetUrl{TargetUrl: backend.URL},
	}}
	if err := svc.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}

	got, err := svc.Tools(context.Background())
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if want := []string{"alpha", "zeta"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Tools: got %v, want %v (sorted)", got, want)
	}

	// Cached: survives the backend going away.
	backend.Close()
	got, err = svc.Tools(context.Background())
	if err != nil || len(got) != 2 {
		t.Errorf("cached Tools: got %v, err %v", got, err)
	}
}

// fakeToolService is a local MCP Service reporting served tool names.
type fakeToolService struct {
	testService
	tools []string
	err   error
}

func (f *fakeToolService) Tools(_ context.Context) ([]string, error) {
	return f.tools, f.err
}

func TestDiscoverySource(t *testing.T) {
	node := &SamNode{
		services: NewServiceRegistry(&fakeDHT{}),
		// Region arrives normalized and validated from Options.Default/Validate.
		config: Options{Region: "EU"},
	}
	ctx := context.Background()

	llm := newFakeModelService("llm", nil, "model-b", "model-a")
	tools := &fakeToolService{
		testService: testService{info: &api.ServiceInfo{Type: api.ServiceType_SERVICE_TYPE_MCP, Name: "reviewer"}},
		tools:       []string{"review_pr", "lint"},
	}
	broken := &fakeToolService{
		testService: testService{info: &api.ServiceInfo{Type: api.ServiceType_SERVICE_TYPE_MCP, Name: "broken"}},
		err:         fmt.Errorf("backend down"),
	}
	for _, svc := range []Service{llm, tools, broken} {
		if err := node.services.Register(ctx, svc); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}

	anns := node.discoverySource()
	if len(anns) != 2 {
		t.Fatalf("announcements: got %d (%+v), want 2", len(anns), anns)
	}
	byName := map[string]samdiscovery.Announcement{}
	for _, a := range anns {
		byName[a.Name] = a
	}
	if a := byName["llm"]; a.Type != api.ServiceType_SERVICE_TYPE_INFERENCE || len(a.Keys) != 2 {
		t.Errorf("llm announcement: %+v", a)
	}
	if a := byName["reviewer"]; a.Type != api.ServiceType_SERVICE_TYPE_MCP || len(a.Keys) != 2 {
		t.Errorf("reviewer announcement: %+v", a)
	}
	for name, a := range byName {
		if a.Labels[api.LabelRegion] != "EU" {
			t.Errorf("%s labels: got %v, want region=EU", name, a.Labels)
		}
	}
}

func TestCapKeys(t *testing.T) {
	keys := make([]string, api.MaxAnnounceKeys+5)
	for i := range keys {
		keys[i] = fmt.Sprintf("k%03d", i)
	}
	sort.Strings(keys)
	if got := capKeys(keys); len(got) != api.MaxAnnounceKeys {
		t.Errorf("capKeys: got %d, want %d", len(got), api.MaxAnnounceKeys)
	}
	if got := capKeys(keys[:3]); len(got) != 3 {
		t.Errorf("capKeys under cap: got %d, want 3", len(got))
	}
}

func TestGossipToolRows(t *testing.T) {
	provs := []samdiscovery.Provider{
		{PeerID: "peerA", Service: "reviewer", Labels: map[string]string{api.LabelRegion: "eu"}},
		{PeerID: "peerB", Service: "other"},
	}
	rows := gossipToolRows(provs, "review_pr", "")
	if len(rows) != 2 {
		t.Fatalf("rows: got %+v, want 2", rows)
	}
	if rows[0].ToolName != "mcp://reviewer/review_pr" || rows[0].Region != "eu" {
		t.Errorf("row 0: %+v", rows[0])
	}
	if rows[1].Region != "" {
		t.Errorf("row 1 should have no region claim: %+v", rows[1])
	}

	filtered := gossipToolRows(provs, "review_pr", "reviewer")
	if len(filtered) != 1 || filtered[0].PeerID != "peerA" {
		t.Errorf("service filter: got %+v", filtered)
	}
}

func TestFilterRowsByToolName(t *testing.T) {
	rows := []remoteToolRow{
		{PeerID: "p1", ToolName: "mcp://svc/review_pr"},
		{PeerID: "p2", ToolName: "mcp://svc/lint"},
		{PeerID: "p3", Error: "unreachable"},
	}
	got := filterRowsByToolName(rows, "review_pr")
	if len(got) != 1 || got[0].PeerID != "p1" {
		t.Errorf("filter: got %+v", got)
	}
}
