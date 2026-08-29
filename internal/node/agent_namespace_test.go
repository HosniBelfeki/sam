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

	"github.com/biscuit-auth/biscuit-go/v2"
	"github.com/google/sam/api"
	"github.com/libp2p/go-libp2p/core/peer"
)

// An agent claim travels beside the token as the calling node's word, so the
// only thing limiting it is the namespace grant in that node's own token.
// Without the limit, any authenticated peer could name any agent and pick up
// whatever role an agent: binding gives it, choosing its own principal. These
// tests cover the limit at the point it is applied.
func TestAuthorizeBoundsTheAgentClaimToTheGrantedNamespace(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	callerPeer := peer.ID("caller-peer")

	// grants is what the caller's own token attests it may speak for.
	mint := func(t *testing.T, grants []string) []byte {
		t.Helper()
		builder := biscuit.NewBuilder(priv)
		facts := []biscuit.Fact{
			{Predicate: biscuit.Predicate{Name: api.FactTargetUnrestricted}},
			{Predicate: biscuit.Predicate{Name: api.FactNode, IDs: []biscuit.Term{biscuit.String(callerPeer.String())}}},
			{Predicate: biscuit.Predicate{Name: api.FactClientPeerID, IDs: []biscuit.Term{biscuit.String(callerPeer.String())}}},
			{Predicate: biscuit.Predicate{Name: api.FactGrantedServiceExact, IDs: []biscuit.Term{biscuit.String(api.SystemNamespace), biscuit.String("/test/proto")}}},
			{Predicate: biscuit.Predicate{Name: api.FactExpiration, IDs: []biscuit.Term{biscuit.Date(time.Now().Add(time.Hour))}}},
		}
		facts = append(facts, api.BuildAgentDatalogFacts(grants)...)
		for _, f := range facts {
			if err := builder.AddAuthorityFact(f); err != nil {
				t.Fatal(err)
			}
		}
		b, err := builder.Build()
		if err != nil {
			t.Fatal(err)
		}
		data, err := b.Serialize()
		if err != nil {
			t.Fatal(err)
		}
		return data
	}

	node := &SamNode{
		trustedKeys:    []TrustedKey{{Key: pub, ReceivedAt: time.Now()}},
		BiscuitTimeout: 500 * time.Millisecond,
	}

	authorize := func(t *testing.T, grants []string, agent string) error {
		t.Helper()
		return node.Authorize(mint(t, grants), RequestContext{
			PeerID:   callerPeer,
			Protocol: "/test/proto",
			Agent:    agent,
		}, pub)
	}

	tests := []struct {
		name    string
		grants  []string
		agent   string
		wantErr bool
	}{
		{
			name:   "claim inside the granted suffix",
			grants: []string{"*.prod.acme.example"},
			agent:  "reviewer-7.prod.acme.example",
		},
		{
			name:    "claim outside the granted suffix",
			grants:  []string{"*.prod.acme.example"},
			agent:   "auditor-1.staging.acme.example",
			wantErr: true,
		},
		{
			// The reason agent ids are dot-anchored: a suffix grant keeps its
			// leading dot, so a lookalike authority is a different namespace.
			name:    "lookalike authority does not satisfy a suffix grant",
			grants:  []string{"*.prod.acme.example"},
			agent:   "intruder.evil-prod.acme.example",
			wantErr: true,
		},
		{
			name:    "no grant at all",
			grants:  nil,
			agent:   "reviewer-7.prod.acme.example",
			wantErr: true,
		},
		{
			name:   "exact grant matches exactly",
			grants: []string{"reviewer-7.prod.acme.example"},
			agent:  "reviewer-7.prod.acme.example",
		},
		{
			name:    "exact grant does not cover a sibling",
			grants:  []string{"reviewer-7.prod.acme.example"},
			agent:   "reviewer-8.prod.acme.example",
			wantErr: true,
		},
		{
			name:   "wildcard grant covers anything",
			grants: []string{"*"},
			agent:  "anyone.anywhere.example",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := authorize(t, tt.grants, tt.agent)
			if tt.wantErr && err == nil {
				t.Errorf("peer speaking for %q with grants %v was allowed; the claim is unbounded", tt.agent, tt.grants)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("peer speaking for %q with grants %v was refused: %v", tt.agent, tt.grants, err)
			}
		})
	}

	// A node's own housekeeping acts for no agent. Requiring a grant
	// unconditionally would refuse it, and its models would stop appearing in
	// peers' catalogues.
	t.Run("no agent claim needs no grant", func(t *testing.T) {
		if err := authorize(t, nil, ""); err != nil {
			t.Errorf("unattributed request refused: %v", err)
		}
	})
}
