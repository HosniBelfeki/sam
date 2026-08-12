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
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/sam/api"
)

// fakeModelService is a local Service that also reports served models.
type fakeModelService struct {
	testService
	models []string
	err    error
}

func (f *fakeModelService) Models(_ context.Context) ([]string, error) {
	return f.models, f.err
}

func newFakeModelService(name string, handler http.Handler, models ...string) *fakeModelService {
	return &fakeModelService{
		testService: testService{
			info:    &api.ServiceInfo{Type: api.ServiceType_SERVICE_TYPE_INFERENCE, Name: name},
			handler: handler,
		},
		models: models,
	}
}

// newTestFacade builds a facade with inert seams; tests override as needed.
func newTestFacade() *openAIFacade {
	return &openAIFacade{
		ttl:           time.Minute,
		localPeerID:   func() string { return "" },
		localServices: func() []Service { return nil },
		discover: func(_ context.Context) ([]*api.DiscoveredProvider, error) {
			return nil, nil
		},
		remoteModels: func(_ context.Context, _, _ string) ([]string, error) {
			return nil, nil
		},
	}
}

func decodeModelsResponse(t *testing.T, body io.Reader) map[string]string {
	t.Helper()
	var resp struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.NewDecoder(body).Decode(&resp); err != nil {
		t.Fatalf("decode models response: %v", err)
	}
	if resp.Object != "list" {
		t.Errorf("object: got %q, want %q", resp.Object, "list")
	}
	out := map[string]string{}
	for _, m := range resp.Data {
		out[m.ID] = m.OwnedBy
	}
	return out
}

func TestFacade_HandleModels_AggregatesLocalAndRemote(t *testing.T) {
	f := newTestFacade()
	f.localServices = func() []Service {
		return []Service{newFakeModelService("local-llm", nil, "local-model", "shared-model")}
	}
	f.discover = func(_ context.Context) ([]*api.DiscoveredProvider, error) {
		return []*api.DiscoveredProvider{
			{PeerId: "peerA", SrvName: "srvA"},
			{PeerId: "peerB", SrvName: "srvB"},
		}, nil
	}
	f.remoteModels = func(_ context.Context, peerID, _ string) ([]string, error) {
		if peerID == "peerA" {
			return []string{"remote-model", "shared-model"}, nil
		}
		return nil, fmt.Errorf("unreachable")
	}

	rec := httptest.NewRecorder()
	f.handleModels(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusOK)
	}
	got := decodeModelsResponse(t, rec.Body)
	if len(got) != 3 {
		t.Fatalf("models: got %v, want 3 entries", got)
	}
	if got["local-model"] != "local" {
		t.Errorf("local-model owned_by: got %q, want %q", got["local-model"], "local")
	}
	// Local providers sort first, so a model served both locally and remotely
	// is reported as local.
	if got["shared-model"] != "local" {
		t.Errorf("shared-model owned_by: got %q, want %q", got["shared-model"], "local")
	}
	if got["remote-model"] != "peerA" {
		t.Errorf("remote-model owned_by: got %q, want %q", got["remote-model"], "peerA")
	}
}

func TestFacade_HandleModels_MethodNotAllowed(t *testing.T) {
	f := newTestFacade()
	rec := httptest.NewRecorder()
	f.handleModels(rec, httptest.NewRequest(http.MethodPost, "/v1/models", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestFacade_Completions_ForwardsToRemote(t *testing.T) {
	f := newTestFacade()
	f.discover = func(_ context.Context) ([]*api.DiscoveredProvider, error) {
		return []*api.DiscoveredProvider{{PeerId: "peerA", SrvName: "srvA"}}, nil
	}
	f.remoteModels = func(_ context.Context, _, _ string) ([]string, error) {
		return []string{"m1"}, nil
	}
	var gotPath, gotBody, gotAuth, gotSamAuth string
	f.forward = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		gotAuth = r.Header.Get("Authorization")
		gotSamAuth = r.Header.Get(api.HeaderSamAuthentication)
		w.WriteHeader(http.StatusOK)
	})

	body := `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	// Post-gate state: the gate stripped the credential it consumed; a
	// surviving Authorization header is the backend's own credential.
	req.Header.Set("Authorization", "Bearer backend-credential")
	req.Header.Set(api.HeaderSamAuthentication, "Bearer sidecar-token")
	rec := httptest.NewRecorder()
	f.handleCompletions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusOK)
	}
	if want := "/sam/peerA/inference/srvA/v1/chat/completions"; gotPath != want {
		t.Errorf("forwarded path: got %q, want %q", gotPath, want)
	}
	if gotBody != body {
		t.Errorf("forwarded body: got %q, want %q", gotBody, body)
	}
	if gotSamAuth != "" {
		t.Errorf("%s must never travel off-node, got %q", api.HeaderSamAuthentication, gotSamAuth)
	}
	if gotAuth != "Bearer backend-credential" {
		t.Errorf("backend Authorization must pass through, got %q", gotAuth)
	}
}

func TestFacade_Completions_LegacyCompletionsPath(t *testing.T) {
	f := newTestFacade()
	f.discover = func(_ context.Context) ([]*api.DiscoveredProvider, error) {
		return []*api.DiscoveredProvider{{PeerId: "peerA", SrvName: "srvA"}}, nil
	}
	f.remoteModels = func(_ context.Context, _, _ string) ([]string, error) {
		return []string{"m1"}, nil
	}
	var gotPath string
	f.forward = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/completions", strings.NewReader(`{"model":"m1","prompt":"hi"}`))
	f.handleCompletions(httptest.NewRecorder(), req)

	if want := "/sam/peerA/inference/srvA/v1/completions"; gotPath != want {
		t.Errorf("forwarded path: got %q, want %q", gotPath, want)
	}
}

func TestFacade_Completions_PrefersLocal(t *testing.T) {
	f := newTestFacade()
	localHit := false
	var gotPeerHeader string
	local := newFakeModelService("local-llm", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		localHit = true
		gotPeerHeader = r.Header.Get("X-Peer-Id")
		w.WriteHeader(http.StatusOK)
	}), "m1")
	f.localPeerID = func() string { return "selfPeer" }
	f.localServices = func() []Service { return []Service{local} }
	f.discover = func(_ context.Context) ([]*api.DiscoveredProvider, error) {
		return []*api.DiscoveredProvider{{PeerId: "peerA", SrvName: "srvA"}}, nil
	}
	f.remoteModels = func(_ context.Context, _, _ string) ([]string, error) {
		return []string{"m1"}, nil
	}
	f.forward = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("remote forward must not be used when a local provider exists")
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m1"}`))
	rec := httptest.NewRecorder()
	f.handleCompletions(rec, req)

	if !localHit {
		t.Fatal("local service was not invoked")
	}
	if gotPeerHeader != "selfPeer" {
		t.Errorf("X-Peer-Id: got %q, want %q", gotPeerHeader, "selfPeer")
	}
}

func TestFacade_Completions_UnknownModel(t *testing.T) {
	f := newTestFacade()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"nope"}`))
	rec := httptest.NewRecorder()
	f.handleCompletions(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusNotFound)
	}
	if !strings.Contains(rec.Body.String(), "model_not_found") {
		t.Errorf("body missing model_not_found code: %s", rec.Body.String())
	}
}

func TestFacade_Completions_BadRequests(t *testing.T) {
	tests := []struct {
		name   string
		method string
		body   string
		want   int
	}{
		{"missing model", http.MethodPost, `{"messages":[]}`, http.StatusBadRequest},
		{"invalid JSON", http.MethodPost, `{`, http.StatusBadRequest},
		{"wrong method", http.MethodGet, ``, http.StatusMethodNotAllowed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newTestFacade()
			req := httptest.NewRequest(tt.method, "/v1/chat/completions", strings.NewReader(tt.body))
			rec := httptest.NewRecorder()
			f.handleCompletions(rec, req)
			if rec.Code != tt.want {
				t.Fatalf("status: got %d, want %d", rec.Code, tt.want)
			}
		})
	}
}

func TestFacade_RegistryCacheAndForcedRefresh(t *testing.T) {
	f := newTestFacade()
	discoverCalls := 0
	f.discover = func(_ context.Context) ([]*api.DiscoveredProvider, error) {
		discoverCalls++
		return []*api.DiscoveredProvider{{PeerId: "peerA", SrvName: "srvA"}}, nil
	}
	f.remoteModels = func(_ context.Context, _, _ string) ([]string, error) {
		return []string{"m1"}, nil
	}

	ctx := context.Background()
	// First resolution populates the cache.
	if got := f.providersFor(ctx, "m1"); len(got) != 1 {
		t.Fatalf("providersFor(m1): got %v, want 1 provider", got)
	}
	if discoverCalls != 1 {
		t.Fatalf("discover calls after first resolve: got %d, want 1", discoverCalls)
	}
	// Cached hit within TTL: no new discovery.
	f.providersFor(ctx, "m1")
	if discoverCalls != 1 {
		t.Fatalf("discover calls after cached resolve: got %d, want 1", discoverCalls)
	}
	// Unknown model on a fresh cache forces one refresh.
	f.providersFor(ctx, "m2")
	if discoverCalls != 2 {
		t.Fatalf("discover calls after unknown-model resolve: got %d, want 2", discoverCalls)
	}
}

func TestFacade_GossipViewServesRegistryMiss(t *testing.T) {
	f := newTestFacade()
	interestCalls := 0
	f.ensureInterest = func(model string) { interestCalls++ }
	f.viewProviders = func(model string) []modelProvider {
		if model == "m-gossip" {
			return []modelProvider{{peerID: "peerG", service: "srvG"}}
		}
		return nil
	}
	var gotPath string
	f.forward = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m-gossip"}`))
	f.handleCompletions(httptest.NewRecorder(), req)

	if want := "/sam/peerG/inference/srvG/v1/chat/completions"; gotPath != want {
		t.Errorf("forwarded path: got %q, want %q", gotPath, want)
	}
	if interestCalls != 1 {
		t.Errorf("ensureInterest calls: got %d, want 1", interestCalls)
	}
}

func TestFacade_RegistryHitBeatsGossipView(t *testing.T) {
	f := newTestFacade()
	f.discover = func(_ context.Context) ([]*api.DiscoveredProvider, error) {
		return []*api.DiscoveredProvider{{PeerId: "peerA", SrvName: "srvA"}}, nil
	}
	f.remoteModels = func(_ context.Context, _, _ string) ([]string, error) {
		return []string{"m1"}, nil
	}
	f.viewProviders = func(model string) []modelProvider {
		t.Error("gossip view must not be consulted on a registry hit")
		return nil
	}
	var gotPath string
	f.forward = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m1"}`))
	f.handleCompletions(httptest.NewRecorder(), req)

	if want := "/sam/peerA/inference/srvA/v1/chat/completions"; gotPath != want {
		t.Errorf("forwarded path: got %q, want %q", gotPath, want)
	}
}

func TestFacade_EmptyRegistryIsNotCached(t *testing.T) {
	f := newTestFacade()
	discoverCalls := 0
	available := false
	f.discover = func(_ context.Context) ([]*api.DiscoveredProvider, error) {
		discoverCalls++
		if !available {
			return nil, nil
		}
		return []*api.DiscoveredProvider{{PeerId: "peerA", SrvName: "srvA"}}, nil
	}
	f.remoteModels = func(_ context.Context, _, _ string) ([]string, error) {
		return []string{"m1"}, nil
	}

	ctx := context.Background()
	if got := f.modelsView(ctx); len(got) != 0 {
		t.Fatalf("modelsView on empty mesh: got %v, want empty", got)
	}
	// A provider appears: the empty view must not shadow it until TTL.
	available = true
	if got := f.modelsView(ctx); len(got) != 1 {
		t.Fatalf("modelsView after provider appeared: got %v, want 1 model", got)
	}
	if discoverCalls != 2 {
		t.Fatalf("discover calls: got %d, want 2", discoverCalls)
	}
}

func TestRankProviders(t *testing.T) {
	local := modelProvider{service: "local-llm"}
	remoteEU := modelProvider{peerID: "peerEU", service: "srv", labels: map[string]string{"region": "eu-de"}, active: 5}
	remoteUS := modelProvider{peerID: "peerUS", service: "srv", labels: map[string]string{"region": "na-us"}, active: 0}
	remoteNoLabel := modelProvider{peerID: "peerX", service: "srv", active: 1}

	t.Run("locals first, remotes by ascending load", func(t *testing.T) {
		f := newTestFacade()
		got := f.rankProviders([]modelProvider{remoteEU, remoteUS, local}, nil)
		if len(got) != 3 || got[0].service != "local-llm" || got[1].peerID != "peerUS" || got[2].peerID != "peerEU" {
			t.Fatalf("unexpected ranking: %+v", got)
		}
	})

	t.Run("label requirement is fail-closed and exact", func(t *testing.T) {
		f := newTestFacade()
		// Requiring region=eu-de matches the exact claim and drops the
		// known-mismatched remote. The unlabeled remote survives ranking
		// (the label gate attests it before any bytes are sent); the
		// unlabeled local has no gate and stays excluded.
		got := f.rankProviders([]modelProvider{remoteEU, remoteUS, remoteNoLabel, local}, map[string]string{"region": "eu-de"})
		if len(got) != 2 || got[0].peerID != "peerX" || got[1].peerID != "peerEU" {
			t.Fatalf("unexpected ranking under label requirement: %+v", got)
		}
		// No built-in hierarchy: a coarser claim never matches a finer requirement.
		coarse := modelProvider{peerID: "peerEU2", service: "srv", labels: map[string]string{"region": "eu"}}
		if got := f.rankProviders([]modelProvider{coarse}, map[string]string{"region": "eu-de"}); len(got) != 0 {
			t.Fatalf("coarse claim must not satisfy a more specific requirement: %+v", got)
		}
	})

	t.Run("local matches its declared label", func(t *testing.T) {
		f := newTestFacade()
		f.localLabels = func() map[string]string { return map[string]string{"region": "eu"} }
		got := f.rankProviders([]modelProvider{local, remoteUS}, map[string]string{"region": "eu"})
		if len(got) != 1 || got[0].service != "local-llm" {
			t.Fatalf("local with matching label should survive: %+v", got)
		}
	})

	t.Run("revoked peers are excluded", func(t *testing.T) {
		f := newTestFacade()
		f.isRevoked = func(peerID string) bool { return peerID == "peerUS" }
		got := f.rankProviders([]modelProvider{remoteEU, remoteUS}, nil)
		if len(got) != 1 || got[0].peerID != "peerEU" {
			t.Fatalf("revoked peer should be excluded: %+v", got)
		}
	})

	t.Run("backoff excludes and expires", func(t *testing.T) {
		f := newTestFacade()
		f.recordBackoff(remoteUS)
		if got := f.rankProviders([]modelProvider{remoteUS}, nil); len(got) != 0 {
			t.Fatalf("backed-off provider should be excluded: %+v", got)
		}
		f.backoffMu.Lock()
		f.backoff[backoffKey(remoteUS)] = time.Now().Add(-time.Second)
		f.backoffMu.Unlock()
		if got := f.rankProviders([]modelProvider{remoteUS}, nil); len(got) != 1 {
			t.Fatal("expired backoff should be cleared")
		}
	})

	t.Run("equal load rotates", func(t *testing.T) {
		f := newTestFacade()
		a := modelProvider{peerID: "peerA", service: "srv"}
		b := modelProvider{peerID: "peerB", service: "srv"}
		seen := map[string]bool{}
		for range 10 {
			seen[f.rankProviders([]modelProvider{a, b}, nil)[0].peerID] = true
		}
		if !seen["peerA"] || !seen["peerB"] {
			t.Errorf("rotation should spread across equals, got %v", seen)
		}
	})
}

func TestParseRequiredLabels(t *testing.T) {
	if got, err := parseRequiredLabels(""); got != nil || err != nil {
		t.Errorf("empty header: got %v, %v; want nil, nil", got, err)
	}
	got, err := parseRequiredLabels(" region=eu , team=platform ,,")
	if err != nil || len(got) != 2 || got["region"] != "eu" || got["team"] != "platform" {
		t.Errorf("parse should split key=value pairs: got %v, %v", got, err)
	}
	if _, err := parseRequiredLabels("noequals"); err == nil {
		t.Error("entry without '=' must be rejected")
	}
	if _, err := parseRequiredLabels("bad key!=v"); err == nil {
		t.Error("invalid label key must be rejected")
	}
	if _, err := parseRequiredLabels("region=us-east-1,region=us-west-1"); err == nil {
		t.Error("duplicate label key must be rejected")
	}
}

func TestRankProviders_LabelMatchIsExactAndCaseSensitive(t *testing.T) {
	f := newTestFacade()
	provider := modelProvider{peerID: "peerEU", service: "srv", labels: map[string]string{"region": "eu-de"}}
	required, err := parseRequiredLabels("region=eu-de")
	if err != nil {
		t.Fatalf("parseRequiredLabels: %v", err)
	}
	if got := f.rankProviders([]modelProvider{provider}, required); len(got) != 1 {
		t.Errorf("exact label match should survive: got %+v", got)
	}
	requiredWrongCase, err := parseRequiredLabels("region=EU-DE")
	if err != nil {
		t.Fatalf("parseRequiredLabels: %v", err)
	}
	if got := f.rankProviders([]modelProvider{provider}, requiredWrongCase); len(got) != 0 {
		t.Errorf("label match is case-sensitive: got %+v", got)
	}
}

func TestFacade_Completions_RetriesNextProvider(t *testing.T) {
	f := newTestFacade()
	f.discover = func(_ context.Context) ([]*api.DiscoveredProvider, error) {
		return []*api.DiscoveredProvider{
			{PeerId: "peerA", SrvName: "srvA"},
			{PeerId: "peerB", SrvName: "srvB"},
		}, nil
	}
	f.remoteModels = func(_ context.Context, _, _ string) ([]string, error) {
		return []string{"m1"}, nil
	}
	var attempts []string
	f.forward = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts = append(attempts, r.URL.Path)
		if len(attempts) == 1 {
			http.Error(w, "overloaded", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m1"}`))
	rec := httptest.NewRecorder()
	f.handleCompletions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d (attempts: %v)", rec.Code, http.StatusOK, attempts)
	}
	if len(attempts) != 2 {
		t.Fatalf("attempts: got %v, want 2", attempts)
	}
	// The failed provider must be in backoff now.
	f.backoffMu.Lock()
	n := len(f.backoff)
	f.backoffMu.Unlock()
	if n != 1 {
		t.Errorf("backoff entries: got %d, want 1", n)
	}
}

func TestFacade_Completions_AllProvidersFail(t *testing.T) {
	f := newTestFacade()
	f.discover = func(_ context.Context) ([]*api.DiscoveredProvider, error) {
		return []*api.DiscoveredProvider{{PeerId: "peerA", SrvName: "srvA"}}, nil
	}
	f.remoteModels = func(_ context.Context, _, _ string) ([]string, error) {
		return []string{"m1"}, nil
	}
	f.forward = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m1"}`))
	rec := httptest.NewRecorder()
	f.handleCompletions(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if !strings.Contains(rec.Body.String(), "no_available_provider") {
		t.Errorf("body missing no_available_provider: %s", rec.Body.String())
	}
}

func TestFacade_Completions_LabelRequirement(t *testing.T) {
	f := newTestFacade()
	f.viewProviders = func(model string) []modelProvider {
		return []modelProvider{
			{peerID: "peerEU", service: "srvEU", labels: map[string]string{"region": "eu-de"}},
			{peerID: "peerUS", service: "srvUS", labels: map[string]string{"region": "na-us"}},
		}
	}
	var attested [][2]string
	f.verifyPeerLabels = func(_ context.Context, peerID string, required map[string]string) error {
		attested = append(attested, [2]string{peerID, required["region"]})
		return nil
	}
	var gotPath, gotLabelsHeader string
	f.forward = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotLabelsHeader = r.Header.Get(api.HeaderSamRequiredLabels)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m1"}`))
	req.Header.Set(api.HeaderSamRequiredLabels, "region=eu-de")
	rec := httptest.NewRecorder()
	f.handleCompletions(rec, req)

	if want := "/sam/peerEU/inference/srvEU/v1/chat/completions"; gotPath != want {
		t.Errorf("forwarded path: got %q, want %q", gotPath, want)
	}
	if gotLabelsHeader != "" {
		t.Errorf("%s must not travel to the provider, got %q", api.HeaderSamRequiredLabels, gotLabelsHeader)
	}
	if len(attested) != 1 || attested[0] != [2]string{"peerEU", "eu-de"} {
		t.Errorf("expected one attestation for peerEU/eu-de, got %v", attested)
	}

	// No provider satisfies the requirement (valid entry, no claimant).
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m1"}`))
	req.Header.Set(api.HeaderSamRequiredLabels, "region=oc")
	rec = httptest.NewRecorder()
	f.handleCompletions(rec, req)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "no_eligible_provider") {
		t.Errorf("unsatisfiable label: got status %d, body %s", rec.Code, rec.Body.String())
	}

	// Malformed label entries are rejected outright.
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m1"}`))
	req.Header.Set(api.HeaderSamRequiredLabels, "noequals")
	rec = httptest.NewRecorder()
	f.handleCompletions(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_request") {
		t.Errorf("invalid label: got status %d, body %s", rec.Code, rec.Body.String())
	}
}

func TestFacade_Completions_LabelAttestation(t *testing.T) {
	newLabelFacade := func() *openAIFacade {
		f := newTestFacade()
		f.viewProviders = func(model string) []modelProvider {
			return []modelProvider{
				{peerID: "peerEU1", service: "srv1", labels: map[string]string{"region": "eu-de"}, active: 1},
				{peerID: "peerEU2", service: "srv2", labels: map[string]string{"region": "eu-de"}, active: 2},
			}
		}
		return f
	}

	t.Run("unattested provider is skipped, attested one serves", func(t *testing.T) {
		f := newLabelFacade()
		f.verifyPeerLabels = func(_ context.Context, peerID string, _ map[string]string) error {
			if peerID == "peerEU1" {
				return fmt.Errorf("no attested label")
			}
			return nil
		}
		var gotPath string
		f.forward = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
		})

		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m1"}`))
		req.Header.Set(api.HeaderSamRequiredLabels, "region=eu-de")
		rec := httptest.NewRecorder()
		f.handleCompletions(rec, req)

		if want := "/sam/peerEU2/inference/srv2/v1/chat/completions"; gotPath != want {
			t.Errorf("forwarded path: got %q, want %q", gotPath, want)
		}
	})

	t.Run("requirement with enforcement unavailable fails closed", func(t *testing.T) {
		f := newLabelFacade() // verifyPeerLabels deliberately nil
		f.forward = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("request must not be forwarded without attestation")
		})

		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m1"}`))
		req.Header.Set(api.HeaderSamRequiredLabels, "region=eu-de")
		rec := httptest.NewRecorder()
		f.handleCompletions(rec, req)

		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "label_unattested") {
			t.Errorf("got status %d, body %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("no requirement never attests", func(t *testing.T) {
		f := newLabelFacade()
		f.verifyPeerLabels = func(_ context.Context, _ string, _ map[string]string) error {
			t.Error("attestation must not run without a requirement")
			return nil
		}
		f.forward = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})

		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m1"}`))
		f.handleCompletions(httptest.NewRecorder(), req)
	})
}

func TestAttemptWriter(t *testing.T) {
	t.Run("retryable status swallowed", func(t *testing.T) {
		rec := httptest.NewRecorder()
		aw := newAttemptWriter(rec)
		aw.Header().Set("Content-Type", "application/json")
		aw.WriteHeader(http.StatusServiceUnavailable)
		_, _ = aw.Write([]byte("error body"))
		if !aw.retryable {
			t.Fatal("expected retryable")
		}
		if rec.Body.Len() != 0 || rec.Header().Get("Content-Type") != "" {
			t.Errorf("nothing must reach the client on a retryable status, got body %q", rec.Body.String())
		}
	})

	t.Run("success streams through with headers", func(t *testing.T) {
		rec := httptest.NewRecorder()
		aw := newAttemptWriter(rec)
		aw.Header().Set("Content-Type", "text/event-stream")
		aw.WriteHeader(http.StatusOK)
		_, _ = aw.Write([]byte("data: x\n\n"))
		aw.Flush()
		if aw.retryable {
			t.Fatal("must not be retryable")
		}
		if rec.Code != http.StatusOK || rec.Body.String() != "data: x\n\n" {
			t.Errorf("unexpected passthrough: code %d, body %q", rec.Code, rec.Body.String())
		}
		if rec.Header().Get("Content-Type") != "text/event-stream" {
			t.Errorf("headers must be copied, got %v", rec.Header())
		}
		if !rec.Flushed {
			t.Error("flush must reach the destination")
		}
	})

	t.Run("implicit 200 on first write", func(t *testing.T) {
		rec := httptest.NewRecorder()
		aw := newAttemptWriter(rec)
		_, _ = aw.Write([]byte("hello"))
		if rec.Code != http.StatusOK || rec.Body.String() != "hello" {
			t.Errorf("implicit 200: code %d, body %q", rec.Code, rec.Body.String())
		}
	})

	t.Run("non-retryable error passes through", func(t *testing.T) {
		rec := httptest.NewRecorder()
		aw := newAttemptWriter(rec)
		aw.WriteHeader(http.StatusNotFound)
		_, _ = aw.Write([]byte("nope"))
		if aw.retryable {
			t.Fatal("404 must not be retryable")
		}
		if rec.Code != http.StatusNotFound || rec.Body.String() != "nope" {
			t.Errorf("passthrough: code %d, body %q", rec.Code, rec.Body.String())
		}
	})
}
