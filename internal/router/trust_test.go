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

package router

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/biscuit-auth/biscuit-go/v2"
	"github.com/google/sam/api"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"google.golang.org/protobuf/proto"
)

// A router pointed at a plaintext control plane on another host has no way
// to know who it is talking to, so Options refuse it unless the operator
// says the network is trusted. Loopback is the standalone case.
func TestOptionsValidateControlPlaneTransport(t *testing.T) {
	tests := []struct {
		url      string
		insecure bool
		wantErr  bool
	}{
		{"http://127.0.0.1:8080", false, false},
		{"https://cp.example.com", false, false},
		{"http://sam-mesh-control-plane:8080", false, true},
		{"http://sam-mesh-control-plane:8080", true, false},
	}
	for _, tt := range tests {
		o := Options{ControlPlaneURL: tt.url, AllowInsecureControlPlane: tt.insecure}
		o.Default()
		err := o.Validate()
		if (err != nil) != tt.wantErr {
			t.Errorf("Validate(%q, insecure=%v) = %v, wantErr %v", tt.url, tt.insecure, err, tt.wantErr)
		}
		if tt.wantErr && !errors.Is(err, api.ErrInsecureControlPlaneURL) {
			t.Errorf("Validate(%q) = %v, want %v", tt.url, err, api.ErrInsecureControlPlaneURL)
		}
	}
}

// The transport re-checks the policy on every hop, so a stored or redirected
// plaintext URL is refused just like a flag-supplied one.
func TestControlPlaneClientRefusesPlaintextHop(t *testing.T) {
	r := &Router{config: Options{}}
	req, err := http.NewRequest(http.MethodGet, "http://sam-mesh-control-plane:8080/keys", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.controlPlaneClient(time.Second).Do(req)
	if !errors.Is(err, api.ErrInsecureControlPlaneURL) {
		t.Fatalf("Do = %v, want %v", err, api.ErrInsecureControlPlaneURL)
	}
}

// /keys may only replace the trust set when signed by a key already in it.
func TestSyncKeysRequiresTrustedSignature(t *testing.T) {
	oldPub, oldPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	newPub, newPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	attackerPub, attackerPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	newRouter := func() *Router {
		return &Router{config: Options{ControlPlaneURL: srv.URL}, trustedPublicKeys: []ed25519.PublicKey{oldPub}}
	}
	marshal := func(resp *api.KeysResponse) []byte {
		data, err := proto.Marshal(resp)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}

	t.Run("rotation signed by the retiring key is adopted", func(t *testing.T) {
		resp := &api.KeysResponse{PublicKeys: [][]byte{oldPub, newPub}}
		if err := api.SignKeysResponse(resp, []ed25519.PrivateKey{oldPriv, newPriv}, time.Now()); err != nil {
			t.Fatal(err)
		}
		body = marshal(resp)
		r := newRouter()
		if err := r.syncKeys(); err != nil {
			t.Fatalf("syncKeys: %v", err)
		}
		if keys := r.getTrustedPublicKeys(); len(keys) != 2 || !keys[1].Equal(newPub) {
			t.Errorf("trusted keys = %d, want old and new", len(keys))
		}
	})

	t.Run("unsigned set is refused", func(t *testing.T) {
		body = marshal(&api.KeysResponse{PublicKeys: [][]byte{oldPub, attackerPub}})
		r := newRouter()
		if err := r.syncKeys(); err == nil {
			t.Fatal("an unsigned /keys answer must not be adopted")
		}
		if keys := r.getTrustedPublicKeys(); len(keys) != 1 || !keys[0].Equal(oldPub) {
			t.Errorf("trust set changed on a refused answer: %d keys", len(keys))
		}
	})

	t.Run("set signed only by a stranger is refused", func(t *testing.T) {
		resp := &api.KeysResponse{PublicKeys: [][]byte{oldPub, attackerPub}}
		if err := api.SignKeysResponse(resp, []ed25519.PrivateKey{attackerPriv, attackerPriv}, time.Now()); err != nil {
			t.Fatal(err)
		}
		body = marshal(resp)
		r := newRouter()
		if err := r.syncKeys(); err == nil {
			t.Fatal("a /keys answer signed by an untrusted key must not be adopted")
		}
		if keys := r.getTrustedPublicKeys(); len(keys) != 1 || !keys[0].Equal(oldPub) {
			t.Errorf("trust set changed on a refused answer: %d keys", len(keys))
		}
	})
}

func mintRouterBiscuit(t *testing.T, priv ed25519.PrivateKey, peerID peer.ID, role string) []byte {
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

// A 403 from /refresh is reported, not obeyed with an exit; and a refreshed
// token is held to the enrollment bar (trusted signer, this peer, router role).
func TestRouterRefreshEnrollmentHardening(t *testing.T) {
	cpPub, cpPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, strangerPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	priv, _, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	peerID, err := peer.IDFromPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	current := mintRouterBiscuit(t, cpPriv, peerID, api.RoleRouter)

	var respond func(w http.ResponseWriter)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/refresh" {
			t.Errorf("unexpected request to %s", r.URL.Path)
			http.Error(w, "unexpected", http.StatusInternalServerError)
			return
		}
		respond(w)
	}))
	defer srv.Close()

	newRouter := func() *Router {
		return &Router{
			privKey:           priv,
			biscuitToken:      current,
			trustedPublicKeys: []ed25519.PublicKey{cpPub},
			config:            Options{ControlPlaneURL: srv.URL, RequiredRole: api.RoleRouter, BiscuitTimeout: time.Second},
		}
	}
	tokenResponse := func(token []byte) func(http.ResponseWriter) {
		return func(w http.ResponseWriter) {
			data, err := proto.Marshal(&api.TokenRefreshResponse{BiscuitToken: token, ExpiresAt: time.Now().Add(time.Hour).Unix()})
			if err != nil {
				t.Fatal(err)
			}
			_, _ = w.Write(data)
		}
	}

	t.Run("403 is an error, not an exit", func(t *testing.T) {
		respond = func(w http.ResponseWriter) { http.Error(w, "router banned", http.StatusForbidden) }
		r := newRouter()
		err := r.RefreshEnrollment(context.Background())
		if err == nil || !strings.Contains(err.Error(), "403") {
			t.Fatalf("RefreshEnrollment = %v, want a 403 error", err)
		}
		if !bytes.Equal(r.biscuitToken, current) {
			t.Error("the current biscuit must be kept on a refused refresh")
		}
	})

	for name, token := range map[string][]byte{
		"token signed by an untrusted key": mintRouterBiscuit(t, strangerPriv, peerID, api.RoleRouter),
		"token bound to another peer":      mintRouterBiscuit(t, cpPriv, newTestPeerID(t), api.RoleRouter),
		"token without the router role":    mintRouterBiscuit(t, cpPriv, peerID, api.RoleNode),
	} {
		t.Run(name+" is refused", func(t *testing.T) {
			respond = tokenResponse(token)
			r := newRouter()
			if err := r.RefreshEnrollment(context.Background()); err == nil {
				t.Fatal("RefreshEnrollment adopted a token it must have refused")
			}
			if !bytes.Equal(r.biscuitToken, current) {
				t.Error("the current biscuit must be kept when the refreshed one is refused")
			}
		})
	}

	t.Run("a good token is adopted", func(t *testing.T) {
		fresh := mintRouterBiscuit(t, cpPriv, peerID, api.RoleRouter)
		respond = tokenResponse(fresh)
		r := newRouter()
		if err := r.RefreshEnrollment(context.Background()); err != nil {
			t.Fatalf("RefreshEnrollment: %v", err)
		}
		if !bytes.Equal(r.biscuitToken, fresh) {
			t.Error("a valid refreshed token must replace the current one")
		}
	})
}
