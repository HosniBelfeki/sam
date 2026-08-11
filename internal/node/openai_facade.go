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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/sam/api"
	libp2phttp "github.com/libp2p/go-libp2p-http"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
)

const (
	// modelRegistryTTL bounds how stale the mesh-wide model view can be.
	modelRegistryTTL = 30 * time.Second
	// remoteModelProbeTimeout bounds a single provider /v1/models probe.
	remoteModelProbeTimeout = 10 * time.Second
	// remoteProbeConcurrency bounds parallel provider probes during refresh.
	remoteProbeConcurrency = 10
	// maxInferenceBodyBytes caps buffered completion request bodies.
	maxInferenceBodyBytes = 16 << 20 // 16 MiB
)

// modelLister is implemented by local services that can report served models.
type modelLister interface {
	Models(ctx context.Context) ([]string, error)
}

// modelProvider locates one service able to serve a model.
// An empty peerID means the service runs on this node.
type modelProvider struct {
	peerID  string
	service string
}

// openAIFacade exposes an OpenAI-compatible surface on the sidecar
// (/v1/models, /v1/chat/completions, /v1/completions): point any OpenAI SDK
// at the sidecar and the facade resolves the requested model to a local or
// mesh provider, preferring local. Function fields are seams so unit tests
// can fake discovery and probing.
type openAIFacade struct {
	forward       http.Handler // egress proxy handling /sam/<peer>/<type>/<name>/...
	localPeerID   func() string
	localServices func() []Service
	discover      func(ctx context.Context) ([]*api.DiscoveredProvider, error)
	remoteModels  func(ctx context.Context, peerID, srvName string) ([]string, error)

	ttl     time.Duration
	mu      sync.Mutex
	models  map[string][]modelProvider
	expires time.Time
}

func newOpenAIFacade(node *SamNode, egress http.Handler) *openAIFacade {
	client := &http.Client{Transport: libp2phttp.NewTransport(node.Host)}
	return &openAIFacade{
		forward:     egress,
		ttl:         modelRegistryTTL,
		localPeerID: func() string { return node.Host.ID().String() },
		localServices: func() []Service {
			var out []Service
			for _, info := range node.ListLocalServices(api.ServiceType_SERVICE_TYPE_INFERENCE) {
				if svc, ok := node.services.Get(info.GetName()); ok {
					out = append(out, svc)
				}
			}
			return out
		},
		discover: func(ctx context.Context) ([]*api.DiscoveredProvider, error) {
			return node.DiscoverRemoteServices(ctx, api.ServiceType_SERVICE_TYPE_INFERENCE, "")
		},
		remoteModels: func(ctx context.Context, peerID, srvName string) ([]string, error) {
			return fetchRemoteModels(ctx, node, client, peerID, srvName)
		},
	}
}

// fetchRemoteModels probes a remote provider's backend /v1/models through its
// libp2p ingress, authenticating with this node's biscuit.
func fetchRemoteModels(ctx context.Context, node *SamNode, client *http.Client, peerID, srvName string) ([]string, error) {
	pid, err := peer.Decode(peerID)
	if err != nil {
		return nil, fmt.Errorf("invalid peer ID %q: %w", peerID, err)
	}
	identity := node.GetIdentity()
	if identity == nil {
		return nil, fmt.Errorf("missing node identity")
	}
	ctx, cancel := context.WithTimeout(network.WithAllowLimitedConn(ctx, "openai-facade"), remoteModelProbeTimeout)
	defer cancel()
	if cond := node.Host.Network().Connectedness(pid); cond != network.Connected && cond != network.Limited {
		node.preparePeerAddrs(ctx, pid)
	}
	u := fmt.Sprintf("libp2p://%s/%s/%s/v1/models", peerID, api.ServiceTypeStringInference, srvName)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(api.HeaderSamBiscuit, base64.StdEncoding.EncodeToString(identity))
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("provider %s/%s returned status %d for /v1/models", peerID, srvName, resp.StatusCode)
	}
	return decodeModelIDs(resp.Body)
}

// providersFor resolves a model to its providers, force-refreshing once on a
// miss so newly appeared providers are usable before the TTL lapses.
func (f *openAIFacade) providersFor(ctx context.Context, model string) []modelProvider {
	f.mu.Lock()
	defer f.mu.Unlock()
	refreshed := false
	if f.models == nil || time.Now().After(f.expires) {
		f.refreshLocked(ctx)
		refreshed = true
	}
	if providers := f.models[model]; len(providers) > 0 || refreshed {
		return providers
	}
	f.refreshLocked(ctx)
	return f.models[model]
}

// modelsView returns the current model registry, refreshing when stale. The
// returned map is a snapshot: refreshes swap the pointer, never mutate.
func (f *openAIFacade) modelsView(ctx context.Context) map[string][]modelProvider {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.models == nil || time.Now().After(f.expires) {
		f.refreshLocked(ctx)
	}
	return f.models
}

func (f *openAIFacade) refreshLocked(ctx context.Context) {
	f.models = f.collectModels(ctx)
	f.expires = time.Now().Add(f.ttl)
}

// collectModels aggregates model IDs from local services (first, so they are
// preferred) and mesh providers. Failed probes drop the provider with a log
// line rather than failing the whole view.
func (f *openAIFacade) collectModels(ctx context.Context) map[string][]modelProvider {
	models := map[string][]modelProvider{}
	for _, svc := range f.localServices() {
		lister, ok := svc.(modelLister)
		if !ok {
			continue
		}
		ids, err := lister.Models(ctx)
		if err != nil {
			logger.Warnf("[OpenAIFacade] local model probe for %q failed: %v", svc.Info().GetName(), err)
			continue
		}
		for _, id := range ids {
			models[id] = append(models[id], modelProvider{service: svc.Info().GetName()})
		}
	}

	providers, err := f.discover(ctx)
	if err != nil {
		logger.Warnf("[OpenAIFacade] mesh discovery failed: %v", err)
		return models
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, remoteProbeConcurrency)
	for _, p := range providers {
		wg.Add(1)
		go func(peerID, srvName string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			ids, err := f.remoteModels(ctx, peerID, srvName)
			if err != nil {
				logger.Warnf("[OpenAIFacade] model probe for %s/%s failed: %v", peerID, srvName, err)
				return
			}
			mu.Lock()
			for _, id := range ids {
				models[id] = append(models[id], modelProvider{peerID: peerID, service: srvName})
			}
			mu.Unlock()
		}(p.GetPeerId(), p.GetSrvName())
	}
	wg.Wait()
	return models
}

// facadeRR spreads load across remote providers of the same model.
var facadeRR atomic.Uint64

// pickProvider prefers a local provider (locals sort first in collectModels),
// otherwise round-robins across remotes.
func pickProvider(providers []modelProvider) modelProvider {
	if providers[0].peerID == "" {
		return providers[0]
	}
	return providers[facadeRR.Add(1)%uint64(len(providers))]
}

func (f *openAIFacade) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only GET is supported")
		return
	}
	type openAIModel struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	view := f.modelsView(r.Context())
	data := make([]openAIModel, 0, len(view))
	for id, providers := range view {
		ownedBy := "local"
		if providers[0].peerID != "" {
			ownedBy = providers[0].peerID
		}
		data = append(data, openAIModel{ID: id, Object: "model", OwnedBy: ownedBy})
	}
	sort.Slice(data, func(i, j int) bool { return data[i].ID < data[j].ID })

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data}); err != nil {
		logger.Errorf("[OpenAIFacade] failed to encode models response: %v", err)
	}
}

func (f *openAIFacade) handleCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only POST is supported")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxInferenceBodyBytes))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds limit")
			return
		}
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "failed to read request body")
		return
	}
	var req struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "request body is not valid JSON")
		return
	}
	if req.Model == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "missing required field: model")
		return
	}

	providers := f.providersFor(r.Context(), req.Model)
	if len(providers) == 0 {
		writeOpenAIError(w, http.StatusNotFound, "model_not_found",
			fmt.Sprintf("model %q is not served by any provider on the mesh", req.Model))
		return
	}
	p := pickProvider(providers)
	logger.Debugf("[OpenAIFacade] model %q -> provider peer=%q service=%q (%d candidates)",
		req.Model, p.peerID, p.service, len(providers))

	// The gate already stripped the credential it consumed; a surviving
	// Authorization header is the destination backend's own credential and
	// passes through, mirroring the egress path. X-Sam-Authentication is
	// local-only and must never travel off-node.
	r.Header.Del(api.HeaderSamAuthentication)
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))

	if p.peerID == "" {
		f.serveLocal(w, r, p.service)
		return
	}
	r.URL.Path = fmt.Sprintf("/sam/%s/%s/%s%s", p.peerID, api.ServiceTypeStringInference, p.service, r.URL.Path)
	r.URL.RawPath = ""
	f.forward.ServeHTTP(w, r)
}

func (f *openAIFacade) serveLocal(w http.ResponseWriter, r *http.Request, serviceName string) {
	for _, svc := range f.localServices() {
		if svc.Info().GetName() != serviceName || svc.Handler() == nil {
			continue
		}
		// Attribute locally served tokens to this node in usage metrics.
		if id := f.localPeerID(); id != "" {
			r.Header.Set("X-Peer-Id", id)
		}
		svc.Handler().ServeHTTP(w, r)
		return
	}
	writeOpenAIError(w, http.StatusServiceUnavailable, "service_unavailable",
		fmt.Sprintf("local service %q is no longer available", serviceName))
}

// writeOpenAIError emits an OpenAI-style error body so SDKs surface it cleanly.
func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	err := json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": "invalid_request_error", "code": code},
	})
	if err != nil {
		logger.Errorf("[OpenAIFacade] failed to encode error response: %v", err)
	}
}
