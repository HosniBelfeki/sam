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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/sam/api"
	"github.com/libp2p/go-libp2p/core/crypto"
	"google.golang.org/protobuf/proto"
)

func TestGetOrGenerateKey(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	// First call should generate a key
	key1 := GetOrGenerateKey(store)
	if key1 == nil {
		t.Fatal("Expected key to be generated")
	}

	// Second call should retrieve the same key
	key2 := GetOrGenerateKey(store)
	if key2 == nil {
		t.Fatal("Expected key to be retrieved")
	}

	// Verify they are the same key
	raw1, _ := crypto.MarshalPrivateKey(key1)
	raw2, _ := crypto.MarshalPrivateKey(key2)
	if !bytes.Equal(raw1, raw2) {
		t.Error("Expected retrieved key to match generated key")
	}
}

func TestEnroll_InvalidHubPublicKeySize(t *testing.T) {
	// 1. Start a fake hub enrollment server
	invalidKey := []byte("too-short") // Not ed25519.PublicKeySize (32 bytes)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/register" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		resp := &api.EnrollResponse{
			BiscuitToken: []byte("mock-token"),
			Expiration:   time.Now().Add(1 * time.Hour).Unix(),
			HubPublicKey: invalidKey,
			HubAddresses: []string{"/ip4/127.0.0.1/tcp/4001"},
		}
		data, _ := proto.Marshal(resp)
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}))
	defer srv.Close()

	// 2. Setup mock node options
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	priv, _, _ := crypto.GenerateKeyPair(crypto.Ed25519, -1)
	node, err := NewSamNode(Options{
		PrivKey:     priv,
		Store:       store,
		ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := node.Start(ctx); err != nil {
		t.Fatal(err)
	}

	// 3. Call enroll which should fail due to public key validation
	err = node.Enroll(context.Background(), srv.URL, "dummy-jwt")
	if err == nil {
		t.Fatal("Expected Enroll to fail with invalid public key size, but it succeeded")
	}
	if !strings.Contains(err.Error(), "received invalid hub public key size") {
		t.Fatalf("Expected invalid public key size error, got: %v", err)
	}
}

func TestProcessEnrollResponse_Errors(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	priv, _, _ := crypto.GenerateKeyPair(crypto.Ed25519, -1)
	node, err := NewSamNode(Options{
		PrivKey:     priv,
		Store:       store,
		ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"},
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("Non200Status", func(t *testing.T) {
		resp := &http.Response{
			StatusCode: http.StatusBadRequest,
			Status:     "400 Bad Request",
			Body:       io.NopCloser(strings.NewReader("bad request details")),
		}
		_, err := node.processEnrollResponse(resp)
		if err == nil || !strings.Contains(err.Error(), "enrollment failed with status 400") {
			t.Fatalf("Expected status 400 error, got: %v", err)
		}
	})

	t.Run("ErrorMessageInResponse", func(t *testing.T) {
		enrollResp := &api.EnrollResponse{ErrorMessage: "unauthorized user"}
		data, _ := proto.Marshal(enrollResp)
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(data)),
		}
		_, err := node.processEnrollResponse(resp)
		if err == nil || !strings.Contains(err.Error(), "enrollment failed: unauthorized user") {
			t.Fatalf("Expected enrollment failed error message, got: %v", err)
		}
	})

	t.Run("EmptyBiscuitToken", func(t *testing.T) {
		enrollResp := &api.EnrollResponse{
			BiscuitToken: nil,
			HubPublicKey: make([]byte, 32),
		}
		data, _ := proto.Marshal(enrollResp)
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(data)),
		}
		_, err := node.processEnrollResponse(resp)
		if err == nil || !strings.Contains(err.Error(), "received empty biscuit token") {
			t.Fatalf("Expected empty biscuit token error, got: %v", err)
		}
	})
}

func TestConnectToHubAddresses_EmptyAddrs(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	priv, _, _ := crypto.GenerateKeyPair(crypto.Ed25519, -1)
	node, err := NewSamNode(Options{
		PrivKey:     priv,
		Store:       store,
		ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"},
	})
	if err != nil {
		t.Fatal(err)
	}

	err = node.connectToHubAddresses(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "returned no hub addresses") {
		t.Fatalf("Expected no hub addresses error, got: %v", err)
	}
}
