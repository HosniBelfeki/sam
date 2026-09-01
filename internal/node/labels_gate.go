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
	"sort"
	"strings"
	"time"

	"github.com/google/sam/api"
	"github.com/google/sam/internal/identity"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-msgio"
	"google.golang.org/protobuf/proto"
)

// parseRequiredLabels splits the X-Sam-Required-Labels header value
// (comma-separated "key=value" pairs) into a label map; any malformed entry
// rejects the whole request.
//
// Only a blank specification means "no requirement". A specification that
// carries content but names no label — ",," or a lone separator — is rejected
// instead of being read as unconstrained: an empty requirement set switches
// the label gate off entirely (see VerifyPeerLabels and rankProviders), so
// silently deriving one from a caller's non-blank input would turn a
// fail-closed control into a fail-open one. Empty entries *alongside* real
// ones stay tolerated, so a trailing comma is still harmless.
func parseRequiredLabels(h string) (map[string]string, error) {
	if strings.TrimSpace(h) == "" {
		return nil, nil
	}
	var out map[string]string
	for _, part := range strings.Split(h, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("invalid label %q: expected key=value", part)
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if err := api.ValidateLabelKey(k); err != nil {
			return nil, err
		}
		if err := api.ValidateLabelValue(v); err != nil {
			return nil, err
		}
		if out == nil {
			out = make(map[string]string)
		}
		if _, exists := out[k]; exists {
			return nil, fmt.Errorf("duplicate label key %q", k)
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("invalid required labels %q: expected at least one key=value pair", h)
	}
	return out, nil
}

// The label gate is the consumer-side enforcement point for label
// requirements: gossip labels only rank providers, this gate verifies the
// provider's control-plane-attested label() facts (api.FactLabel) before
// any request data leaves this node. Fail-closed: a provider that returns no
// biscuit or lacks a matching fact is rejected.
const (
	labelGateCacheSize = 1024
	// labelGateTTL bounds how long a positive verification verdict is
	// reused before the provider's biscuit is fetched and checked again.
	labelGateTTL = 5 * time.Minute
	// labelGateDialTimeout bounds the biscuit-fetch handshake.
	labelGateDialTimeout = 10 * time.Second
)

// labelGateKey builds a deterministic cache key from a required label set.
func labelGateKey(peerID peer.ID, required map[string]string) string {
	keys := make([]string, 0, len(required))
	for k := range required {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	key := peerID.String()
	for _, k := range keys {
		key += "|" + k + "=" + required[k]
	}
	return key
}

// VerifyPeerLabels ensures the peer holds control-plane-attested labels
// satisfying any of the required key=value pairs (canonical, pre-validated).
// A nil/empty requirement passes without network traffic. Positive verdicts
// are cached.
func (n *SamNode) VerifyPeerLabels(ctx context.Context, peerID peer.ID, required map[string]string) error {
	if len(required) == 0 {
		return nil
	}
	key := labelGateKey(peerID, required)
	if until, ok := n.peerLabelGate.Get(key); ok && time.Now().Before(until) {
		return nil
	}

	providerBiscuit, err := n.fetchPeerBiscuit(ctx, peerID)
	if err != nil {
		return fmt.Errorf("provider %s labels unverifiable: %w", peerID, err)
	}
	if err := n.checkPeerLabels(providerBiscuit, peerID, required); err != nil {
		return err
	}

	n.peerLabelGate.Add(key, time.Now().Add(labelGateTTL))
	return nil
}

// checkPeerLabels verifies the provider's biscuit (control-plane signature,
// expiry, binding to peerID) and evaluates the compiled label requirement
// against its attested facts.
func (n *SamNode) checkPeerLabels(providerBiscuit []byte, peerID peer.ID, required map[string]string) error {
	if len(providerBiscuit) == 0 {
		return fmt.Errorf("provider %s returned no identity biscuit; cannot attest required labels %v", peerID, required)
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

	authorizer, err := b.Authorizer(key, identity.AuthorizerOptions(n.BiscuitTimeout)...)
	if err != nil {
		return fmt.Errorf("provider %s authorizer instantiation failed: %w", peerID, err)
	}
	check, err := api.LabelCheck(required)
	if err != nil {
		return err
	}
	authorizer.AddCheck(check)
	authorizer.AddPolicy(api.AllowIfTruePolicy)
	if err := authorizer.Authorize(); err != nil {
		return fmt.Errorf("provider %s has no attested label matching %v: %w", peerID, required, err)
	}
	return nil
}

// fetchPeerBiscuit obtains the peer's control-plane-minted identity via the
// mutual auth handshake on AuthProtocolID, authenticating with our own.
func (n *SamNode) fetchPeerBiscuit(ctx context.Context, peerID peer.ID) ([]byte, error) {
	observation, err := n.fetchPeerBiscuitEvidence(ctx, peerID)
	if err != nil {
		return nil, err
	}
	return observation.Biscuit, nil
}

type peerBiscuitObservation struct {
	Biscuit        []byte
	ConnectionPeer peer.ID
}

// fetchPeerBiscuitEvidence is the uncached form used by the local evidence API.
// It preserves the PeerID authenticated by the libp2p stream separately from
// the requested target so callers can fail closed on any binding mismatch.
func (n *SamNode) fetchPeerBiscuitEvidence(ctx context.Context, peerID peer.ID) (peerBiscuitObservation, error) {
	ourBiscuit := n.GetIdentity()
	if ourBiscuit == nil {
		return peerBiscuitObservation{}, fmt.Errorf("missing node identity")
	}

	dialCtx, cancel := context.WithTimeout(ctx, labelGateDialTimeout)
	defer cancel()
	s, err := n.Host.NewStream(dialCtx, peerID, api.AuthProtocolID)
	if err != nil {
		return peerBiscuitObservation{}, fmt.Errorf("failed to open auth stream: %w", err)
	}
	defer func() { _ = s.Close() }()
	_ = s.SetDeadline(time.Now().Add(labelGateDialTimeout))
	connectionPeer := s.Conn().RemotePeer()
	if connectionPeer != peerID {
		return peerBiscuitObservation{}, fmt.Errorf("authenticated connection peer %s does not match requested peer %s", connectionPeer, peerID)
	}

	authBytes, err := proto.Marshal(&api.AuthFrame{Biscuit: ourBiscuit})
	if err != nil {
		return peerBiscuitObservation{}, fmt.Errorf("marshal auth frame: %w", err)
	}
	writer := msgio.NewVarintWriter(s)
	if err := writer.WriteMsg(authBytes); err != nil {
		return peerBiscuitObservation{}, fmt.Errorf("write auth frame: %w", err)
	}

	reader := msgio.NewVarintReaderSize(s, 1024*64)
	msg, err := reader.ReadMsg()
	if err != nil {
		return peerBiscuitObservation{}, fmt.Errorf("read auth response (peer may predate mutual auth): %w", err)
	}
	defer reader.ReleaseMsg(msg)

	var resp api.AuthResponse
	if err := proto.Unmarshal(msg, &resp); err != nil {
		return peerBiscuitObservation{}, fmt.Errorf("invalid auth response: %w", err)
	}
	if !resp.Success {
		return peerBiscuitObservation{}, fmt.Errorf("auth rejected: %s", resp.Error)
	}
	return peerBiscuitObservation{
		Biscuit:        resp.Biscuit,
		ConnectionPeer: connectionPeer,
	}, nil
}
