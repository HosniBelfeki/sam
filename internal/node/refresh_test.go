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
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/biscuit-auth/biscuit-go/v2"
	"github.com/google/sam/api"
	"github.com/libp2p/go-libp2p/core/peer"
	"google.golang.org/protobuf/proto"
)

// refreshHarness is a node with a stored identity and a mock control plane
// whose /refresh answer the test chooses.
type refreshHarness struct {
	node     *SamNode
	cpPub    ed25519.PublicKey
	cpPriv   ed25519.PrivateKey
	peerID   peer.ID
	identity []byte
}

func mintRoleBiscuit(t *testing.T, priv ed25519.PrivateKey, peerID peer.ID, role string) []byte {
	t.Helper()
	builder := biscuit.NewBuilder(priv)
	for _, f := range []biscuit.Fact{
		{Predicate: biscuit.Predicate{Name: api.FactNode, IDs: []biscuit.Term{biscuit.String(peerID.String())}}},
		{Predicate: biscuit.Predicate{Name: api.FactRole, IDs: []biscuit.Term{biscuit.String(role)}}},
		{Predicate: biscuit.Predicate{Name: api.FactExpiration, IDs: []biscuit.Term{biscuit.Date(time.Now().Add(24 * time.Hour))}}},
	} {
		if err := builder.AddAuthorityFact(f); err != nil {
			t.Fatal(err)
		}
	}
	tok, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	b, err := tok.Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func newRefreshHarness(t *testing.T, refresh http.HandlerFunc) *refreshHarness {
	t.Helper()
	cpPub, cpPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	privKey := GetOrGenerateKey(store)
	peerID, err := peer.IDFromPublicKey(privKey.GetPublic())
	if err != nil {
		t.Fatal(err)
	}
	current := mintRoleBiscuit(t, cpPriv, peerID, api.RoleNode)
	if err := store.SaveIdentity(current); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/refresh", refresh)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	if err := store.SaveControlPlaneURL(srv.URL); err != nil {
		t.Fatal(err)
	}

	node := &SamNode{
		Store:          store,
		trustedKeys:    []TrustedKey{{Key: cpPub, ReceivedAt: time.Now()}},
		BiscuitTimeout: 500 * time.Millisecond,
		config:         Options{RequiredRole: api.RoleNode},
	}
	node.SetIdentityCache(current)
	return &refreshHarness{node: node, cpPub: cpPub, cpPriv: cpPriv, peerID: peerID, identity: current}
}

func writeRefreshResponse(t *testing.T, w http.ResponseWriter, token []byte) {
	t.Helper()
	data, err := proto.Marshal(&api.TokenRefreshResponse{BiscuitToken: token, ExpiresAt: time.Now().Add(24 * time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	_, _ = w.Write(data)
}

func (h *refreshHarness) storedIdentityUnchanged(t *testing.T) {
	t.Helper()
	stored, err := h.node.Store.LoadIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, h.identity) {
		t.Error("stored identity was replaced by a response that must not have been accepted")
	}
	if !bytes.Equal(h.node.GetIdentity(), h.identity) {
		t.Error("cached identity was replaced by a response that must not have been accepted")
	}
}

// A 403 from whoever answers /refresh is not the control plane's word that
// this node is banned; that is a verified MeshEvent_BANNED. The node reports
// the refusal and keeps its current identity instead of exiting.
func TestRefreshEnrollmentForbiddenDoesNotExit(t *testing.T) {
	h := newRefreshHarness(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "node banned", http.StatusForbidden)
	})

	err := h.node.RefreshEnrollment(context.Background())
	var rerr *RefreshError
	if !errors.As(err, &rerr) || rerr.StatusCode != http.StatusForbidden {
		t.Fatalf("RefreshEnrollment = %v, want *RefreshError with 403", err)
	}
	h.storedIdentityUnchanged(t)
}

// The refreshed token is held to the same bar as the enrolled one: signed by
// a key this node already trusts, bound to this peer, carrying its role.
func TestRefreshEnrollmentRejectsUntrustworthyToken(t *testing.T) {
	_, strangerPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	otherPeer := randomPeerID(t)

	tests := []struct {
		name string
		mint func(h *refreshHarness) []byte
	}{
		{"signed by an untrusted key", func(h *refreshHarness) []byte {
			return mintRoleBiscuit(t, strangerPriv, h.peerID, api.RoleNode)
		}},
		{"bound to another peer", func(h *refreshHarness) []byte {
			return mintRoleBiscuit(t, h.cpPriv, otherPeer, api.RoleNode)
		}},
		{"wrong role", func(h *refreshHarness) []byte {
			return mintRoleBiscuit(t, h.cpPriv, h.peerID, api.RoleRouter)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var h *refreshHarness
			h = newRefreshHarness(t, func(w http.ResponseWriter, r *http.Request) {
				writeRefreshResponse(t, w, tt.mint(h))
			})
			if err := h.node.RefreshEnrollment(context.Background()); err == nil {
				t.Fatal("RefreshEnrollment accepted a token it must have rejected")
			}
			h.storedIdentityUnchanged(t)
		})
	}

	t.Run("a good token is adopted", func(t *testing.T) {
		var h *refreshHarness
		h = newRefreshHarness(t, func(w http.ResponseWriter, r *http.Request) {
			writeRefreshResponse(t, w, mintRoleBiscuit(t, h.cpPriv, h.peerID, api.RoleNode))
		})
		if err := h.node.RefreshEnrollment(context.Background()); err != nil {
			t.Fatalf("RefreshEnrollment: %v", err)
		}
		if bytes.Equal(h.node.GetIdentity(), h.identity) {
			t.Error("a valid refreshed token must replace the current identity")
		}
	})
}
