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

	"github.com/multiformats/go-multiaddr"
)

type routerConnectionManager interface {
	IsConnected() bool
	LoadMeshConfig() ([]byte, []string, error)
	ConnectAndAuthWithRouter(ctx context.Context, addr multiaddr.Multiaddr) error
	LoadControlPlaneURL() (string, error)

	SaveMeshConfig(pubKey []byte, addrs []string) error
	UpdateRelays(addrs []multiaddr.Multiaddr)
}

// checkRouterConnection monitors the connection to the routers and attempts to recover it if disconnected.
// It returns two booleans indicating the current state of the connection:
// - stable: true if the connection was already established and is healthy.
// - reconnected: true if the connection was lost but successfully recovered during this check.
//
// The recovery process follows these steps:
//  1. Connection Check: If already connected, return (stable=true, reconnected=false).
//  2. P2P Retry: If disconnected, attempt to reconnect using the known P2P multiaddresses stored locally.
//  3. HTTP Fallback: If P2P retries fail, fall back to HTTP discovery using the stored Control plane URL to fetch
//     the new router addresses and Peer IDs, update the local config, and attempt to reconnect.
//  4. Total Failure: If all reconnection attempts fail, return (stable=false, reconnected=false).
func checkRouterConnection(ctx context.Context, mgr routerConnectionManager) (stable bool, reconnected bool) {
	if mgr.IsConnected() {
		return true, false
	}

	logger.Warn("[Monitor] Disconnected from router. Attempting to reconnect...")

	var p2pAddrs []multiaddr.Multiaddr
	if _, storedAddrs, err := mgr.LoadMeshConfig(); err == nil {
		for _, addrStr := range storedAddrs {
			if ma, err := multiaddr.NewMultiaddr(addrStr); err == nil {
				p2pAddrs = append(p2pAddrs, ma)
			}
		}
	}

	var connectedAddrs []multiaddr.Multiaddr
	for _, addr := range p2pAddrs {
		if err := mgr.ConnectAndAuthWithRouter(ctx, addr); err == nil {
			logger.Infof("[Monitor] Successfully reconnected to router via P2P: %s", addr)
			connectedAddrs = append(connectedAddrs, addr)
		}
	}
	if len(connectedAddrs) > 0 {
		mgr.UpdateRelays(connectedAddrs)
		return false, true
	}

	controlPlaneURL, err := mgr.LoadControlPlaneURL()
	if err != nil || controlPlaneURL == "" {
		return false, false
	}

	logger.Infof("[Monitor] Reconnect P2P failed. Discovering control plane info from %s...", controlPlaneURL)
	info, err := FetchControlPlaneInfo(ctx, controlPlaneURL)
	if err != nil || len(info.RouterAddresses) == 0 {
		return false, false
	}

	var newRouterAddrs []multiaddr.Multiaddr
	for _, addrStr := range info.RouterAddresses {
		if ma, err := multiaddr.NewMultiaddr(addrStr); err == nil {
			newRouterAddrs = append(newRouterAddrs, ma)
		}
	}

	if len(newRouterAddrs) == 0 {
		return false, false
	}

	if pubKeyBytes, _, err := mgr.LoadMeshConfig(); err == nil {
		if saveErr := mgr.SaveMeshConfig(pubKeyBytes, info.RouterAddresses); saveErr != nil {
			logger.Errorf("[Monitor] Failed to save updated mesh config: %v", saveErr)
		}
	}

	var connectedAddrsFallback []multiaddr.Multiaddr
	for _, addr := range newRouterAddrs {
		if err := mgr.ConnectAndAuthWithRouter(ctx, addr); err == nil {
			logger.Infof("[Monitor] Successfully reconnected to router via HTTP fallback: %s", addr)
			connectedAddrsFallback = append(connectedAddrsFallback, addr)
		}
	}
	if len(connectedAddrsFallback) > 0 {
		mgr.UpdateRelays(connectedAddrsFallback)
		return false, true
	}

	return false, false
}
