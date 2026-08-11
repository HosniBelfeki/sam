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
	"crypto/ed25519"
	"fmt"
	"strings"
	"time"

	"github.com/biscuit-auth/biscuit-go/v2"
	"github.com/biscuit-auth/biscuit-go/v2/datalog"
	"github.com/google/sam/api"
	"github.com/google/sam/internal/identity"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-msgio"
	"google.golang.org/protobuf/proto"
)

// The region gate is the consumer-side enforcement point for region
// requirements: gossip labels only rank providers, this gate verifies the
// provider's control-plane-attested region() facts (api.FactRegion) before
// any request data leaves this node. Fail-closed: a provider that returns no
// biscuit or lacks a matching fact is rejected.
const (
	regionGateCacheSize = 1024
	// regionGateTTL bounds how long a positive verification verdict is
	// reused before the provider's biscuit is fetched and checked again.
	regionGateTTL = 5 * time.Minute
	// regionGateDialTimeout bounds the biscuit-fetch handshake.
	regionGateDialTimeout = 10 * time.Second
)

// VerifyPeerRegions ensures the peer holds a control-plane-attested region
// satisfying any of the required regions (canonical, pre-validated). A nil
// requirement passes without network traffic. Positive verdicts are cached.
func (n *SamNode) VerifyPeerRegions(ctx context.Context, peerID peer.ID, required []string) error {
	if len(required) == 0 {
		return nil
	}
	key := peerID.String() + "|" + strings.Join(required, ",")
	if until, ok := n.peerRegionGate.Get(key); ok && time.Now().Before(until) {
		return nil
	}

	providerBiscuit, err := n.fetchPeerBiscuit(ctx, peerID)
	if err != nil {
		return fmt.Errorf("provider %s region unverifiable: %w", peerID, err)
	}
	if err := n.checkPeerRegions(providerBiscuit, peerID, required); err != nil {
		return err
	}

	n.peerRegionGate.Add(key, time.Now().Add(regionGateTTL))
	return nil
}

// checkPeerRegions verifies the provider's biscuit (control-plane signature,
// expiry, binding to peerID) and evaluates the compiled region requirement
// against its attested facts.
func (n *SamNode) checkPeerRegions(providerBiscuit []byte, peerID peer.ID, required []string) error {
	if len(providerBiscuit) == 0 {
		return fmt.Errorf("provider %s returned no identity biscuit; cannot attest required region %v", peerID, required)
	}

	n.keysMu.RLock()
	trustedKeys := make([]ed25519.PublicKey, 0, len(n.trustedKeys))
	for _, tk := range n.trustedKeys {
		trustedKeys = append(trustedKeys, tk.Key)
	}
	n.keysMu.RUnlock()
	if len(trustedKeys) == 0 {
		return fmt.Errorf("no trusted control plane keys loaded")
	}

	b, key, err := identity.VerifyBiscuitAndGetKey(providerBiscuit, peerID, trustedKeys, n.BiscuitTimeout)
	if err != nil {
		return fmt.Errorf("provider %s biscuit verification failed: %w", peerID, err)
	}

	var authOpts []biscuit.AuthorizerOption
	if n.BiscuitTimeout > 0 {
		authOpts = append(authOpts, biscuit.WithWorldOptions(datalog.WithMaxDuration(n.BiscuitTimeout)))
	}
	authorizer, err := b.Authorizer(key, authOpts...)
	if err != nil {
		return fmt.Errorf("provider %s authorizer instantiation failed: %w", peerID, err)
	}
	check, err := api.RegionCheck(required)
	if err != nil {
		return err
	}
	authorizer.AddCheck(check)
	authorizer.AddPolicy(api.AllowIfTruePolicy)
	if err := authorizer.Authorize(); err != nil {
		return fmt.Errorf("provider %s has no attested region matching %v: %w", peerID, required, err)
	}
	return nil
}

// fetchPeerBiscuit obtains the peer's control-plane-minted identity via the
// mutual auth handshake on AuthProtocolID, authenticating with our own.
func (n *SamNode) fetchPeerBiscuit(ctx context.Context, peerID peer.ID) ([]byte, error) {
	ourBiscuit := n.GetIdentity()
	if ourBiscuit == nil {
		return nil, fmt.Errorf("missing node identity")
	}

	dialCtx, cancel := context.WithTimeout(ctx, regionGateDialTimeout)
	defer cancel()
	s, err := n.Host.NewStream(dialCtx, peerID, api.AuthProtocolID)
	if err != nil {
		return nil, fmt.Errorf("failed to open auth stream: %w", err)
	}
	defer func() { _ = s.Close() }()
	_ = s.SetDeadline(time.Now().Add(regionGateDialTimeout))

	authBytes, err := proto.Marshal(&api.AuthFrame{Biscuit: ourBiscuit})
	if err != nil {
		return nil, fmt.Errorf("marshal auth frame: %w", err)
	}
	writer := msgio.NewVarintWriter(s)
	if err := writer.WriteMsg(authBytes); err != nil {
		return nil, fmt.Errorf("write auth frame: %w", err)
	}

	reader := msgio.NewVarintReaderSize(s, 1024*64)
	msg, err := reader.ReadMsg()
	if err != nil {
		return nil, fmt.Errorf("read auth response (peer may predate mutual auth): %w", err)
	}
	defer reader.ReleaseMsg(msg)

	var resp api.AuthResponse
	if err := proto.Unmarshal(msg, &resp); err != nil {
		return nil, fmt.Errorf("invalid auth response: %w", err)
	}
	if !resp.Success {
		return nil, fmt.Errorf("auth rejected: %s", resp.Error)
	}
	return resp.Biscuit, nil
}
