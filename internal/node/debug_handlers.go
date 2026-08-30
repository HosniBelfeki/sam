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
	"net/http"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
)

// Bound the diagnostic dials so a dead peer yields an error instead of a
// request that hangs for as long as the caller's patience.
const (
	connectivityPingTimeout = 15 * time.Second
	connectPeerTimeout      = 30 * time.Second
)

// newDebugHandler serves the operator diagnostics under /debug. These were MCP
// tools once; they moved here so agents never see them in their tool list (#318).
func newDebugHandler(n *SamNode) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /debug/mesh-info", func(w http.ResponseWriter, r *http.Request) {
		info, err := n.meshInfo()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeDebugJSON(w, info)
	})
	mux.HandleFunc("GET /debug/connectivity", func(w http.ResponseWriter, r *http.Request) {
		writeDebugJSON(w, n.connectivityStats(r.Context(), r.URL.Query().Get("peer_id")))
	})
	mux.HandleFunc("GET /debug/network-info", func(w http.ResponseWriter, r *http.Request) {
		writeDebugJSON(w, n.networkInfo())
	})
	mux.HandleFunc("GET /debug/token-info", func(w http.ResponseWriter, r *http.Request) {
		writeDebugJSON(w, n.tokenInfo())
	})
	mux.HandleFunc("GET /debug/logs", func(w http.ResponseWriter, r *http.Request) {
		writeDebugJSON(w, map[string]any{"logs": GetRecentLogs()})
	})
	mux.HandleFunc("POST /debug/connect-peer", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			PeerAddr string `json:"peer_addr"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}
		if req.PeerAddr == "" {
			http.Error(w, "peer_addr is required", http.StatusBadRequest)
			return
		}
		if err := n.connectPeer(r.Context(), req.PeerAddr); err != nil {
			http.Error(w, fmt.Sprintf("Failed to connect: %v", err), http.StatusInternalServerError)
			return
		}
		writeDebugJSON(w, map[string]string{"status": "connected"})
	})

	// One guard for every handler: refuse loudly on a half-built node instead
	// of panicking mid-request or fabricating zero-value diagnostics.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := n.debugReady(); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// debugReady reports whether the components the /debug handlers touch exist.
func (n *SamNode) debugReady() error {
	switch {
	case n == nil:
		return fmt.Errorf("node not initialized")
	case n.Host == nil:
		return fmt.Errorf("libp2p host not initialized")
	case n.DHT == nil:
		return fmt.Errorf("DHT not initialized")
	case n.Store == nil:
		return fmt.Errorf("node store not initialized")
	}
	return nil
}

func writeDebugJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		logger.Errorf("Failed to encode response: %v", err)
	}
}

// meshInfo backs both the get_mesh_info MCP tool and GET /debug/mesh-info.
// The MCP path skips the /debug boundary guard, so it re-checks here.
func (n *SamNode) meshInfo() (map[string]any, error) {
	if err := n.debugReady(); err != nil {
		return nil, err
	}

	peers := n.Host.Network().Peers()
	// Pre-sized so zero peers serializes as [] rather than null.
	connectedPeers := make([]string, 0, len(peers))
	for _, p := range peers {
		connectedPeers = append(connectedPeers, p.String())
	}
	dhtSize := n.DHT.RoutingTable().Size()

	resData := map[string]any{
		"peer_id":         n.Host.ID().String(),
		"connected_peers": connectedPeers,
		"dht_size":        dhtSize,
		"router_peer_id":  n.RouterPeerID.String(),
	}
	if n.BoundSocketPath != "" {
		resData["local_api_socket"] = n.BoundSocketPath
	}
	return resData, nil
}

// connectivityStats backs GET /debug/connectivity: with a peer ID it pings that
// peer, otherwise it pings the SAM router.
func (n *SamNode) connectivityStats(ctx context.Context, peerIDStr string) map[string]any {
	ctx, cancel := context.WithTimeout(ctx, connectivityPingTimeout)
	defer cancel()

	stats := map[string]any{
		"connected_peers":   len(n.Host.Network().Peers()),
		"total_known_peers": len(n.Host.Peerstore().Peers()),
	}

	if peerIDStr != "" {
		pid, err := peer.Decode(peerIDStr)
		if err == nil {
			n.preparePeerAddrs(ctx, pid)
			start := time.Now()
			err := n.Host.Connect(ctx, peer.AddrInfo{ID: pid})
			stats["ping_latency_ms"] = time.Since(start).Milliseconds()
			stats["ping_error"] = err != nil
			if err != nil {
				stats["ping_error_msg"] = err.Error()
			}
		} else {
			stats["ping_error"] = true
			stats["ping_error_msg"] = "invalid peer id"
		}
	} else if n.RouterPeerID != "" {
		start := time.Now()
		err := n.Host.Connect(ctx, peer.AddrInfo{ID: n.RouterPeerID})
		stats["router_latency_ms"] = time.Since(start).Milliseconds()
		stats["router_error"] = err != nil
		if err != nil {
			stats["router_error_msg"] = err.Error()
		}
	}

	return stats
}

// tokenInfo backs GET /debug/token-info.
func (n *SamNode) tokenInfo() map[string]any {
	info := map[string]any{
		"has_token": false,
	}

	token, err := n.Store.LoadIdentity()
	if err == nil && len(token) > 0 {
		info["has_token"] = true
		exp, err := n.Store.LoadIdentityExpiration()
		if err == nil {
			info["expires_in_seconds"] = time.Until(time.Unix(exp, 0)).Seconds()
			info["is_expired"] = time.Now().Unix() > exp
		}
	}
	return info
}

// networkInfo backs GET /debug/network-info.
func (n *SamNode) networkInfo() map[string]any {
	listenAddrs := []string{}
	for _, a := range n.Host.Network().ListenAddresses() {
		listenAddrs = append(listenAddrs, a.String())
	}

	observedAddrs := []string{}
	for _, a := range n.Host.Addrs() {
		observedAddrs = append(observedAddrs, a.String())
	}

	return map[string]any{
		"listen_addresses":   listenAddrs,
		"observed_addresses": observedAddrs,
	}
}

// connectPeer backs POST /debug/connect-peer.
func (n *SamNode) connectPeer(ctx context.Context, peerAddr string) error {
	ctx, cancel := context.WithTimeout(ctx, connectPeerTimeout)
	defer cancel()

	ma, err := multiaddr.NewMultiaddr(peerAddr)
	if err != nil {
		return err
	}
	addrInfo, err := peer.AddrInfoFromP2pAddr(ma)
	if err != nil {
		return err
	}
	if n.revokedPeers != nil && n.revokedPeers.Contains(addrInfo.ID.String()) {
		return fmt.Errorf("failed to dial: failed to dial %s: gater disallows connection to peer", addrInfo.ID)
	}
	if n.Store.IsBanned(addrInfo.ID) {
		return fmt.Errorf("failed to dial: failed to dial %s: gater disallows connection to peer", addrInfo.ID)
	}
	conns := n.Host.Network().ConnsToPeer(addrInfo.ID)
	connectedness := n.Host.Network().Connectedness(addrInfo.ID)
	logger.Debugf("[connect-peer] Target peer %s, connectedness: %v, active conns: %d", addrInfo.ID, connectedness, len(conns))

	err = n.Host.Connect(ctx, *addrInfo)
	logger.Debugf("[connect-peer] Host.Connect returned error: %v", err)
	return err
}
