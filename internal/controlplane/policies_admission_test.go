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
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/sam/api"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"google.golang.org/protobuf/proto"
)

// TestPoliciesRequiresAnAdmissibleNode pins that reading mesh policy applies the
// same admission rules as refreshing a token. The GET handler used to accept any
// node record that was not explicitly banned, so a node whose OIDC session had
// lapsed kept reading roles, bindings and allowed targets until an operator
// banned it by hand. Passive expiry is the backstop that has to work on its own.
func TestPoliciesRequiresAnAdmissibleNode(t *testing.T) {
	issuer, mintToken := startCustomMockOIDC(t)
	srv, store, baseURL := setupTestServer(t, issuer)
	defer func() {
		_ = srv.Close()
		_ = store.Close()
	}()

	ctx := context.Background()
	client := &http.Client{Timeout: 5 * time.Second}

	privNode, _, err := crypto.GenerateKeyPair(crypto.Ed25519, -1)
	if err != nil {
		t.Fatal(err)
	}
	nodePeer, err := peer.IDFromPrivateKey(privNode)
	if err != nil {
		t.Fatal(err)
	}
	nodePubKeyBytes, err := crypto.MarshalPublicKey(privNode.GetPublic())
	if err != nil {
		t.Fatal(err)
	}

	enrollReq := &api.EnrollRequest{
		Jwt:           mintToken(map[string]interface{}{"sub": "node-alice", "groups": []string{"users"}}),
		PeerId:        nodePeer.String(),
		PublicKey:     nodePubKeyBytes,
		RequestedRole: api.RoleNode,
	}
	reqData, err := proto.Marshal(enrollReq)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := client.Post(baseURL+"/register", "application/x-protobuf", bytes.NewReader(reqData))
	if err != nil {
		t.Fatalf("node /register failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("node /register status %s (body: %s)", resp.Status, body)
	}

	var enrollResp api.EnrollResponse
	if err := proto.Unmarshal(body, &enrollResp); err != nil {
		t.Fatalf("failed to unmarshal EnrollResponse: %v", err)
	}

	getPolicies := func(t *testing.T) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, baseURL+"/policies", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+base64.StdEncoding.EncodeToString(enrollResp.BiscuitToken))
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET /policies failed: %v", err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	// The token is the same throughout; only the node record changes, so any
	// difference below comes from the admission check and not from the biscuit.
	if got := getPolicies(t); got != http.StatusOK {
		t.Fatalf("freshly enrolled node got status %d, want %d", got, http.StatusOK)
	}

	expireSession := func(t *testing.T, at time.Time) {
		t.Helper()
		record, err := store.GetNode(ctx, nodePeer.String())
		if err != nil {
			t.Fatalf("GetNode: %v", err)
		}
		record.ExpiresAt = at
		if err := store.EnrollNode(ctx, record); err != nil {
			t.Fatalf("EnrollNode: %v", err)
		}
	}

	expireSession(t, time.Now().Add(-time.Hour))
	if got := getPolicies(t); got != http.StatusUnauthorized {
		t.Errorf("node with a lapsed session got status %d, want %d", got, http.StatusUnauthorized)
	}

	expireSession(t, time.Now().Add(time.Hour))
	if got := getPolicies(t); got != http.StatusOK {
		t.Errorf("node with a renewed session got status %d, want %d", got, http.StatusOK)
	}

	if err := store.SetNodeBanned(ctx, nodePeer.String(), true); err != nil {
		t.Fatalf("SetNodeBanned: %v", err)
	}
	if got := getPolicies(t); got != http.StatusUnauthorized {
		t.Errorf("banned node got status %d, want %d", got, http.StatusUnauthorized)
	}
}
