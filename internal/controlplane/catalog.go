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

package controlplane

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/sam/api"
	"github.com/google/sam/internal/identity"
	"github.com/google/sam/internal/storage"
	"github.com/libp2p/go-libp2p/core/peer"
	"google.golang.org/protobuf/proto"
)

// maxCatalogServices bounds one report so a single admitted node cannot grow
// the in-memory cache without limit.
const maxCatalogServices = 512

// nodeCatalogEntry is what HandleNodeCatalog caches per reporting peer.
type nodeCatalogEntry struct {
	Services   []*api.ServiceInfo
	ReportedAt time.Time
}

// catalogService is the console-facing shape of one reported service: a plain
// struct so the JSON the console reads does not depend on protoc-gen-go's
// struct layout or tags.
type catalogService struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Description string `json:"description"`
}

// catalogView is HandleAdminStatus's node_catalog value for one peer.
type catalogView struct {
	Services   []catalogService `json:"services"`
	ReportedAt time.Time        `json:"reported_at"`
}

// catalogSnapshot returns a stable copy of the current node service catalog
// cache, safe to range over without holding catalogMu.
func (s *Server) catalogSnapshot() map[string]nodeCatalogEntry {
	s.catalogMu.RLock()
	defer s.catalogMu.RUnlock()
	snap := make(map[string]nodeCatalogEntry, len(s.catalog))
	for k, v := range s.catalog {
		snap[k] = v
	}
	return snap
}

// catalogViewFor renders the cache for the console, restricted to nodes that
// are still admitted so a banned or expired node's last report disappears
// with its enrollment instead of lingering until the next restart.
func (s *Server) catalogViewFor(nodes []storage.EnrolledNode, now time.Time) map[string]catalogView {
	snap := s.catalogSnapshot()
	view := make(map[string]catalogView, len(snap))
	for i := range nodes {
		node := &nodes[i]
		// The cache is keyed by the canonical base58 form from the verified
		// biscuit; the stored record may carry another valid encoding of the
		// same peer (e.g. CIDv1), so decode before looking up.
		pID, err := peer.Decode(node.PeerID)
		if err != nil {
			logger.Warnw("Skipping enrolled node with undecodable peer ID in catalog view", "peer_id", node.PeerID, "error", err)
			continue
		}
		entry, ok := snap[pID.String()]
		if !ok || node.CheckAdmission(now) != nil {
			continue
		}
		services := make([]catalogService, 0, len(entry.Services))
		for _, svc := range entry.Services {
			if svc == nil {
				continue
			}
			typeName, err := api.ServiceTypeToString(svc.GetType())
			if err != nil {
				typeName = "unknown"
			}
			services = append(services, catalogService{
				Name:        svc.GetName(),
				Type:        typeName,
				Description: svc.GetDescription(),
			})
		}
		view[node.PeerID] = catalogView{Services: services, ReportedAt: entry.ReportedAt}
	}
	return view
}

// dropCatalogEntry forgets a peer's report; called when its enrollment ends.
// Accepts any valid encoding of the peer ID.
func (s *Server) dropCatalogEntry(peerID string) {
	pID, err := peer.Decode(peerID)
	if err != nil {
		// Cache keys always come from a verified biscuit, so an undecodable
		// ID cannot have an entry - nothing to evict, but say so.
		logger.Warnw("Not evicting catalog entry for undecodable peer ID", "peer_id", peerID, "error", err)
		return
	}
	s.catalogMu.Lock()
	delete(s.catalog, pID.String())
	s.catalogMu.Unlock()
}

// HandleNodeCatalog HTTP POST /nodes/catalog - a node self-reports the
// services it currently has registered locally (the same data
// list_local_services already answers on the node itself), so the control
// plane can show mesh-wide service topology without needing to be a DHT
// participant or open a P2P connection to every enrolled node itself.
//
// The body is an api.NodeCatalogReport. The reporting peer is the one bound
// in the presented Biscuit, so a node can only ever describe itself.
//
// This is a live-status cache, not authoritative state: a node that goes
// offline without ever reporting an empty catalog just leaves its last
// report in place until ReportedAt visibly goes stale or its enrollment
// ends. It is admin-facing display data only and never feeds authorization,
// which is also why a bare bearer Biscuit (no signed challenge, unlike
// /refresh) is accepted here: a replayed token can only repaint a table.
func (s *Server) HandleNodeCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	authHeader := r.Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, "Bearer ") {
		http.Error(w, "Missing node Biscuit token in Authorization header", http.StatusUnauthorized)
		return
	}
	biscuitBytes, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(authHeader, "Bearer "))
	if err != nil {
		http.Error(w, "Malformed base64 token", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	trustedKeys, err := s.store.GetAllValidPublicKeys(ctx)
	if err != nil {
		logger.Errorf("Failed to retrieve valid signing keys: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	peerID, err := identity.VerifyAndExtractPeerID(trustedKeys, biscuitBytes, s.config.BiscuitTimeout)
	if err != nil {
		logger.Warnw("Invalid biscuit presented to /nodes/catalog", "error", err)
		http.Error(w, "Invalid biscuit: "+err.Error(), http.StatusUnauthorized)
		return
	}

	nodeRecord, err := s.store.GetNode(ctx, peerID.String())
	if err == storage.ErrNotFound || (err == nil && nodeRecord == nil) {
		http.Error(w, "Node not enrolled", http.StatusUnauthorized)
		return
	} else if err != nil {
		logger.Errorf("Failed to retrieve node record: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	if err := nodeRecord.CheckAdmission(time.Now()); err != nil {
		http.Error(w, "Node not admitted: "+err.Error(), http.StatusUnauthorized)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read body", http.StatusBadRequest)
		return
	}

	var req api.NodeCatalogReport
	if err := proto.Unmarshal(body, &req); err != nil {
		http.Error(w, "Invalid request format", http.StatusBadRequest)
		return
	}
	if len(req.Services) > maxCatalogServices {
		http.Error(w, fmt.Sprintf("Too many services in report (max %d)", maxCatalogServices), http.StatusBadRequest)
		return
	}

	s.catalogMu.Lock()
	s.catalog[peerID.String()] = nodeCatalogEntry{
		Services:   req.Services,
		ReportedAt: time.Now(),
	}
	s.catalogMu.Unlock()

	w.WriteHeader(http.StatusNoContent)
}
