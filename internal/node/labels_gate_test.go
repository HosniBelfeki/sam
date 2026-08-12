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
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/google/sam/api"
	"github.com/google/sam/internal/identity"
	"github.com/libp2p/go-libp2p/core/peer"
)

func TestCheckPeerLabels(t *testing.T) {
	cpPub, cpPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	otherPub, otherPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = otherPub

	providerPeer := peer.ID("provider-peer-id")
	expiry := time.Now().Add(time.Hour)

	mint := func(key ed25519.PrivateKey, p peer.ID, labels map[string]string) []byte {
		t.Helper()
		b, err := identity.MintBootstrapBiscuitToken(key, p, api.RoleNode, expiry, nil, labels)
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		return b
	}

	node := &SamNode{
		trustedKeys:    []TrustedKey{{Key: cpPub, ReceivedAt: time.Now()}},
		BiscuitTimeout: 500 * time.Millisecond,
	}

	tests := []struct {
		name      string
		biscuit   []byte
		required  map[string]string
		expectErr bool
	}{
		{"exact match", mint(cpPriv, providerPeer, map[string]string{"region": "us-east-1"}), map[string]string{"region": "us-east-1"}, false},
		{"any-of requirement matches one key", mint(cpPriv, providerPeer, map[string]string{"region": "na-us", "team": "platform"}), map[string]string{"region": "eu", "team": "platform"}, false},
		{"no built-in hierarchy: coarser requirement fails a finer claim", mint(cpPriv, providerPeer, map[string]string{"region": "us-east-1"}), map[string]string{"region": "us"}, true},
		{"disjoint labels fail", mint(cpPriv, providerPeer, map[string]string{"region": "na-us"}), map[string]string{"region": "eu"}, true},
		{"unattested token fails closed", mint(cpPriv, providerPeer, nil), map[string]string{"region": "eu"}, true},
		{"empty biscuit fails closed", nil, map[string]string{"region": "eu"}, true},
		{"untrusted signer fails", mint(otherPriv, providerPeer, map[string]string{"region": "eu"}), map[string]string{"region": "eu"}, true},
		{"token bound to another peer fails", mint(cpPriv, peer.ID("other-peer"), map[string]string{"region": "eu"}), map[string]string{"region": "eu"}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := node.checkPeerLabels(tt.biscuit, providerPeer, tt.required)
			if tt.expectErr && err == nil {
				t.Error("expected error, got nil")
			} else if !tt.expectErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}

	t.Run("no trusted keys fails closed", func(t *testing.T) {
		bare := &SamNode{BiscuitTimeout: 500 * time.Millisecond}
		if err := bare.checkPeerLabels(mint(cpPriv, providerPeer, map[string]string{"region": "eu"}), providerPeer, map[string]string{"region": "eu"}); err == nil {
			t.Error("expected error, got nil")
		}
	})
}
