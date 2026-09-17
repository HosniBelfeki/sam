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
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/sam/api"
	"google.golang.org/protobuf/proto"
)

func TestMergeTrustedKeys(t *testing.T) {
	now := time.Now()
	earlier := now.Add(-2 * time.Hour)
	keyA, _, _ := ed25519.GenerateKey(nil)
	keyB, _, _ := ed25519.GenerateKey(nil)
	keyC, _, _ := ed25519.GenerateKey(nil)

	existing := []TrustedKey{
		{Key: keyA, ReceivedAt: earlier},
		{Key: keyC, ReceivedAt: earlier}, // expired at the CP: absent from /keys
	}
	got := mergeTrustedKeys(existing, []ed25519.PublicKey{keyA, keyB}, now)

	if len(got) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(got))
	}
	if !got[0].Key.Equal(keyA) || !got[0].ReceivedAt.Equal(earlier) {
		t.Errorf("known key must keep its ReceivedAt: got %+v", got[0])
	}
	if !got[1].Key.Equal(keyB) || !got[1].ReceivedAt.Equal(now) {
		t.Errorf("new key must get the sync time: got %+v", got[1])
	}
	for _, tk := range got {
		if tk.Key.Equal(keyC) {
			t.Error("key expired at the control plane must be dropped")
		}
	}
}

func TestFetchControlPlaneInfo(t *testing.T) {
	expectedInfo := &api.ControlPlaneInfoResponse{
		RouterAddresses: []string{"/ip4/127.0.0.1/tcp/4001"},
		OidcIssuer:      "https://issuer.example.com",
		ClientId:        "client-id",
	}

	body, err := proto.Marshal(expectedInfo)
	if err != nil {
		t.Fatalf("Failed to marshal info: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/info" {
			t.Errorf("Expected path /info, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer server.Close()

	info, err := FetchControlPlaneInfo(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("FetchControlPlaneInfo failed: %v", err)
	}

	if !reflect.DeepEqual(info.RouterAddresses, expectedInfo.RouterAddresses) {
		t.Errorf("Expected RouterAddresses %v, got %v", expectedInfo.RouterAddresses, info.RouterAddresses)
	}
	if info.OidcIssuer != expectedInfo.OidcIssuer {
		t.Errorf("Expected OidcIssuer %s, got %s", expectedInfo.OidcIssuer, info.OidcIssuer)
	}
	if info.ClientId != expectedInfo.ClientId {
		t.Errorf("Expected ClientId %s, got %s", expectedInfo.ClientId, info.ClientId)
	}
}

func TestFetchControlPlaneInfo_HTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	_, err := FetchControlPlaneInfo(context.Background(), server.URL)
	if err == nil {
		t.Fatal("Expected error, got nil")
	}
	if !strings.Contains(err.Error(), "control plane returned status 500 Internal Server Error") {
		t.Errorf("Expected error to contain '500 Internal Server Error', got %v", err)
	}
}

func TestFetchControlPlaneInfo_InvalidProto(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("invalid data"))
	}))
	defer server.Close()

	_, err := FetchControlPlaneInfo(context.Background(), server.URL)
	if err == nil {
		t.Fatal("Expected error, got nil")
	}
	if !strings.Contains(err.Error(), "failed to decode /info response") {
		t.Errorf("Expected error to contain 'failed to decode /info response', got %v", err)
	}
}

func TestSyncMeshConfig(t *testing.T) {
	expectedInfo := &api.ControlPlaneInfoResponse{
		RouterAddresses: []string{"/ip4/127.0.0.1/tcp/4001"},
		OidcIssuer:      "https://issuer.example.com",
		ClientId:        "client-id",
	}

	body, err := proto.Marshal(expectedInfo)
	if err != nil {
		t.Fatalf("Failed to marshal info: %v", err)
	}

	cpPub, cpPriv, _ := ed25519.GenerateKey(nil)
	gracePub, gracePriv, _ := ed25519.GenerateKey(nil)
	keysResp := &api.KeysResponse{PublicKeys: [][]byte{cpPub, gracePub}}
	if err := api.SignKeysResponse(keysResp, []ed25519.PrivateKey{cpPriv, gracePriv}, time.Now()); err != nil {
		t.Fatal(err)
	}
	keysBody, err := proto.Marshal(keysResp)
	if err != nil {
		t.Fatalf("Failed to marshal keys: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if r.URL.Path == "/keys" {
			_, _ = w.Write(keysBody)
			return
		}
		_, _ = w.Write(body)
	}))
	defer server.Close()

	tempDir := t.TempDir()
	store, err := NewStore(tempDir)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close() //nolint:errcheck

	// Initial store is empty, so SyncMeshConfig should just return empty
	pubKey, addrs, bannedPeerIDs, err := SyncMeshConfig(context.Background(), store)
	if err != nil {
		t.Fatalf("SyncMeshConfig failed: %v", err)
	}
	if len(bannedPeerIDs) != 0 {
		t.Errorf("Expected no banned peers for empty store, got %v", bannedPeerIDs)
	}
	if len(pubKey) != 0 || len(addrs) != 0 {
		t.Errorf("Expected empty result for empty store, got pubKey=%v, addrs=%v", pubKey, addrs)
	}

	// Save initial config with explicit control plane URL. The node trusts
	// only cpPub, as after enrollment; the grace key must be learned through
	// cpPub's signature on the set.
	testPubKey := []byte("test-pub-key")
	if err := store.SaveMeshConfig(testPubKey, []string{"/ip4/1.2.3.4/tcp/1234"}); err != nil {
		t.Fatalf("Failed to save mesh config: %v", err)
	}
	if err := store.SaveControlPlaneURL(server.URL); err != nil {
		t.Fatalf("Failed to save control plane URL: %v", err)
	}
	if err := store.SaveTrustedKeys([]TrustedKey{{Key: cpPub, ReceivedAt: time.Now()}}); err != nil {
		t.Fatalf("SaveTrustedKeys: %v", err)
	}

	// Call SyncMeshConfig, it should fetch new addrs from server
	pubKey, addrs, _, err = SyncMeshConfig(context.Background(), store)
	if err != nil {
		t.Fatalf("SyncMeshConfig failed: %v", err)
	}

	if string(pubKey) != string(testPubKey) {
		t.Errorf("Expected pubKey %s, got %s", testPubKey, pubKey)
	}

	if len(addrs) != 1 || addrs[0].String() != expectedInfo.RouterAddresses[0] {
		t.Errorf("Expected addrs %v, got %v", expectedInfo.RouterAddresses, addrs)
	}

	// Verify the new addrs were saved to the store
	savedPubKey, savedAddrsStr, err := store.LoadMeshConfig()
	if err != nil {
		t.Fatalf("Failed to load mesh config: %v", err)
	}
	if string(savedPubKey) != string(testPubKey) {
		t.Errorf("Expected saved pubKey %s, got %s", testPubKey, savedPubKey)
	}

	// The full valid key set from /keys must have been persisted
	trusted, err := store.LoadTrustedKeys()
	if err != nil {
		t.Fatalf("LoadTrustedKeys: %v", err)
	}
	if len(trusted) != 2 {
		t.Fatalf("expected 2 trusted keys from /keys, got %d", len(trusted))
	}
	if !trusted[0].Key.Equal(cpPub) || !trusted[1].Key.Equal(gracePub) {
		t.Errorf("persisted keys do not match /keys response")
	}
	if len(savedAddrsStr) != 1 || savedAddrsStr[0] != expectedInfo.RouterAddresses[0] {
		t.Errorf("Expected saved addrs %v, got %v", expectedInfo.RouterAddresses, savedAddrsStr)
	}
}

// Whoever answers /keys must already be the control plane: a set that is not
// signed by a key the node trusts leaves the trust set untouched.
func TestSyncMeshConfigRefusesUntrustedKeySet(t *testing.T) {
	cpPub, _, _ := ed25519.GenerateKey(nil)
	attackerPub, attackerPriv, _ := ed25519.GenerateKey(nil)

	infoBody, err := proto.Marshal(&api.ControlPlaneInfoResponse{RouterAddresses: []string{"/ip4/127.0.0.1/tcp/4001"}})
	if err != nil {
		t.Fatal(err)
	}

	for name, keysResp := range map[string]*api.KeysResponse{
		"unsigned set": {PublicKeys: [][]byte{cpPub, attackerPub}},
		"set signed only by the attacker": func() *api.KeysResponse {
			r := &api.KeysResponse{PublicKeys: [][]byte{cpPub, attackerPub}}
			if err := api.SignKeysResponse(r, []ed25519.PrivateKey{attackerPriv, attackerPriv}, time.Now()); err != nil {
				t.Fatal(err)
			}
			return r
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			keysBody, err := proto.Marshal(keysResp)
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/keys" {
					_, _ = w.Write(keysBody)
					return
				}
				_, _ = w.Write(infoBody)
			}))
			defer server.Close()

			store, err := NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close() //nolint:errcheck
			if err := store.SaveMeshConfig(cpPub, nil); err != nil {
				t.Fatal(err)
			}
			if err := store.SaveControlPlaneURL(server.URL); err != nil {
				t.Fatal(err)
			}
			if err := store.SaveTrustedKeys([]TrustedKey{{Key: cpPub, ReceivedAt: time.Now()}}); err != nil {
				t.Fatal(err)
			}

			if _, _, _, err := SyncMeshConfig(context.Background(), store); err != nil {
				t.Fatalf("SyncMeshConfig: %v", err)
			}

			trusted, err := store.LoadTrustedKeys()
			if err != nil {
				t.Fatal(err)
			}
			if len(trusted) != 1 || !trusted[0].Key.Equal(cpPub) {
				t.Fatalf("trust set was replaced by an unverified /keys answer: %d keys", len(trusted))
			}
		})
	}
}

// The control plane is the trust root, so a plaintext hop to it is refused
// unless the operator opted in; loopback is the standalone case and is fine.
func TestControlPlaneClientRefusesPlaintextToNonLoopback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := proto.Marshal(&api.ControlPlaneInfoResponse{})
		_, _ = w.Write(body)
	}))
	defer server.Close()
	// httptest binds 127.0.0.1; spell it as a non-loopback name that the
	// transport must refuse before any connection is attempted.
	nonLoopbackURL := strings.Replace(server.URL, "127.0.0.1", "sam-control-plane.invalid", 1)

	t.Cleanup(func() { SetAllowInsecureControlPlane(false) })

	if _, err := FetchControlPlaneInfo(context.Background(), server.URL); err != nil {
		t.Fatalf("loopback plaintext must be accepted: %v", err)
	}

	_, err := FetchControlPlaneInfo(context.Background(), nonLoopbackURL)
	if !errors.Is(err, api.ErrInsecureControlPlaneURL) {
		t.Fatalf("plaintext to a non-loopback host: err = %v, want %v", err, api.ErrInsecureControlPlaneURL)
	}

	// With the opt-in the request is attempted; the name does not resolve,
	// which is a dial error, not the policy error.
	SetAllowInsecureControlPlane(true)
	_, err = FetchControlPlaneInfo(context.Background(), nonLoopbackURL)
	if err == nil || errors.Is(err, api.ErrInsecureControlPlaneURL) {
		t.Fatalf("with --insecure-control-plane the policy must not be what fails: %v", err)
	}
}

func TestReportNodeCatalog(t *testing.T) {
	services := []*api.ServiceInfo{
		{Type: api.ServiceType_SERVICE_TYPE_MCP, Name: "stvv-compliance-docs", Description: "doc lookup"},
	}

	var gotAuth, gotContentType string
	var gotReq api.NodeCatalogReport
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("Expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/nodes/catalog" {
			t.Errorf("Expected path /nodes/catalog, got %s", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("failed to read request body: %v", err)
		}
		if err := proto.Unmarshal(body, &gotReq); err != nil {
			t.Errorf("failed to decode request body: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	biscuitToken := []byte("fake-biscuit-bytes")
	if err := ReportNodeCatalog(context.Background(), server.URL, biscuitToken, services); err != nil {
		t.Fatalf("ReportNodeCatalog failed: %v", err)
	}

	wantAuth := "Bearer " + base64.StdEncoding.EncodeToString(biscuitToken)
	if gotAuth != wantAuth {
		t.Errorf("Expected Authorization header %q, got %q", wantAuth, gotAuth)
	}
	if gotContentType != "application/x-protobuf" {
		t.Errorf("Expected Content-Type application/x-protobuf, got %q", gotContentType)
	}
	if len(gotReq.Services) != 1 || gotReq.Services[0].Name != "stvv-compliance-docs" || gotReq.Services[0].Type != api.ServiceType_SERVICE_TYPE_MCP {
		t.Errorf("Expected relayed services %v, got %v", services, gotReq.Services)
	}
}

func TestReportNodeCatalog_HTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "node not enrolled or not admitted", http.StatusUnauthorized)
	}))
	defer server.Close()

	err := ReportNodeCatalog(context.Background(), server.URL, []byte("fake-biscuit-bytes"), nil)
	if err == nil {
		t.Fatal("Expected error, got nil")
	}
	if !strings.Contains(err.Error(), "control plane returned status 401") {
		t.Errorf("Expected error to mention status 401, got %v", err)
	}
}

// newCatalogTestNode is a SamNode with just enough state for the catalog
// loop: a store holding the control-plane URL, a cached identity, and a
// registry with one service.
func newCatalogTestNode(t *testing.T, controlPlaneURL string) *SamNode {
	t.Helper()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if controlPlaneURL != "" {
		if err := store.SaveControlPlaneURL(controlPlaneURL); err != nil {
			t.Fatalf("SaveControlPlaneURL: %v", err)
		}
	}
	node := &SamNode{Store: store, services: newServiceRegistryForTest(&fakeDHT{})}
	node.services.insertService(newFakeSvc("calc", api.ServiceType_SERVICE_TYPE_MCP))
	return node
}

func TestReportNodeCatalog_Preconditions(t *testing.T) {
	noURL := newCatalogTestNode(t, "")
	noURL.SetIdentityCache([]byte("biscuit"))
	if err := noURL.reportNodeCatalog(context.Background()); err == nil || !strings.Contains(err.Error(), "control plane URL") {
		t.Errorf("without a control-plane URL: got %v, want a URL error", err)
	}

	noIdentity := newCatalogTestNode(t, "http://127.0.0.1:1")
	if err := noIdentity.reportNodeCatalog(context.Background()); err == nil || !strings.Contains(err.Error(), "identity") {
		t.Errorf("without an identity: got %v, want an identity error", err)
	}
}

// The loop must report once after the initial delay and then keep reporting
// every interval, carrying the live service list each time.
func TestStartCatalogReportLoop(t *testing.T) {
	reports := make(chan *api.NodeCatalogReport, 16)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/nodes/catalog" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var report api.NodeCatalogReport
		if err := proto.Unmarshal(body, &report); err != nil {
			t.Errorf("decode report: %v", err)
		}
		reports <- &report
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	node := newCatalogTestNode(t, server.URL)
	node.SetIdentityCache([]byte("biscuit"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	node.startCatalogReportLoop(ctx, 10*time.Millisecond, 20*time.Millisecond)

	for i := 0; i < 3; i++ {
		select {
		case report := <-reports:
			if len(report.Services) != 1 || report.Services[0].Name != "calc" {
				t.Fatalf("report %d: got services %v, want [calc]", i, report.Services)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for report %d", i)
		}
	}

	// Cancelling stops the loop: no report after the in-flight one settles.
	cancel()
	time.Sleep(100 * time.Millisecond)
	for len(reports) > 0 {
		<-reports
	}
	select {
	case <-reports:
		t.Fatal("loop kept reporting after context cancellation")
	case <-time.After(150 * time.Millisecond):
	}
}

func TestOptionsDefault_CatalogReport(t *testing.T) {
	var o Options
	o.Default()
	if o.CatalogReportInterval != time.Minute {
		t.Errorf("CatalogReportInterval = %v, want 1m", o.CatalogReportInterval)
	}
	if o.CatalogReportInitialDelay != 5*time.Second {
		t.Errorf("CatalogReportInitialDelay = %v, want 5s", o.CatalogReportInitialDelay)
	}

	custom := Options{CatalogReportInterval: 3 * time.Second, CatalogReportInitialDelay: time.Second}
	custom.Default()
	if custom.CatalogReportInterval != 3*time.Second || custom.CatalogReportInitialDelay != time.Second {
		t.Errorf("Default overwrote explicit values: %+v", custom)
	}
}
