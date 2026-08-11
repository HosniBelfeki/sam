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

func TestCheckPeerRegions(t *testing.T) {
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

	mint := func(key ed25519.PrivateKey, p peer.ID, region string) []byte {
		t.Helper()
		b, err := identity.MintBootstrapBiscuitToken(key, p, api.RoleNode, expiry, nil, region)
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
		required  []string
		expectErr bool
	}{
		{"finer claim satisfies coarser requirement", mint(cpPriv, providerPeer, "EU-DE"), []string{"EU"}, false},
		{"exact match", mint(cpPriv, providerPeer, "EU-DE"), []string{"EU-DE"}, false},
		{"any-of requirement", mint(cpPriv, providerPeer, "NA-US"), []string{"EU", "NA-US"}, false},
		{"coarser claim fails finer requirement", mint(cpPriv, providerPeer, "EU"), []string{"EU-DE"}, true},
		{"disjoint region fails", mint(cpPriv, providerPeer, "NA-US"), []string{"EU"}, true},
		{"unattested token fails closed", mint(cpPriv, providerPeer, ""), []string{"EU"}, true},
		{"empty biscuit fails closed", nil, []string{"EU"}, true},
		{"untrusted signer fails", mint(otherPriv, providerPeer, "EU-DE"), []string{"EU"}, true},
		{"token bound to another peer fails", mint(cpPriv, peer.ID("other-peer"), "EU-DE"), []string{"EU"}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := node.checkPeerRegions(tt.biscuit, providerPeer, tt.required)
			if tt.expectErr && err == nil {
				t.Error("expected error, got nil")
			} else if !tt.expectErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}

	t.Run("no trusted keys fails closed", func(t *testing.T) {
		bare := &SamNode{BiscuitTimeout: 500 * time.Millisecond}
		if err := bare.checkPeerRegions(mint(cpPriv, providerPeer, "EU"), providerPeer, []string{"EU"}); err == nil {
			t.Error("expected error, got nil")
		}
	})
}
