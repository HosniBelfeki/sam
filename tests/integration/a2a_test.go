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

package integration_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/google/sam/api"
)

// TestA2ACUJ covers the "A2A agent behind the mesh" CUJ: node A (attested
// region=eu) hosts an a2a service; node B's raw egress proxy serves it with
// a rewritten agent card, admits a region=eu-labelled request, and refuses a
// region=us-east-1 request fail-closed before any payload leaves node B.
func TestA2ACUJ(t *testing.T) {
	nodeBin := buildBinary(t, "./cmd/sam-node")
	_, hubAddr := startMockRouter(t)

	homeA := t.TempDir()
	homeB := t.TempDir()
	apiToken := "test-token"

	t.Log("Starting Node A (provider, region=eu)...")
	_ = startBackgroundNode(t, nodeBin, hubAddr, homeA,
		"--listen", "/ip4/127.0.0.1/udp/0/quic-v1",
		"--listen", "/ip4/127.0.0.1/tcp/0",
		"--discovery-interval", "100ms",
		"--labels", "region=eu",
	)
	t.Log("Starting Node B (consumer)...")
	_ = startBackgroundNode(t, nodeBin, hubAddr, homeB,
		"--listen", "/ip4/127.0.0.1/udp/0/quic-v1",
		"--listen", "/ip4/127.0.0.1/tcp/0",
		"--discovery-interval", "100ms",
	)

	apiAddrA := waitForMCPAddr(t, filepath.Join(homeA, "node.log"))
	apiAddrB := waitForMCPAddr(t, filepath.Join(homeB, "node.log"))
	waitForAPI(t, apiAddrA)
	waitForAPI(t, apiAddrB)

	addrA := waitForPeerInfoInLog(t, filepath.Join(homeA, "node.log"))
	connectPeer(t, apiAddrB, addrA)
	waitForDHTPeers(t, apiAddrA)

	idx := strings.LastIndex(addrA, "/p2p/")
	if idx < 0 {
		t.Fatalf("no /p2p/ component in peer addr %q", addrA)
	}
	peerA := addrA[idx+len("/p2p/"):]

	// Fake A2A agent on node A's side: serves its card and echoes message/send.
	var sendCount atomic.Int32
	var sawLabelsHeader atomic.Bool
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(api.HeaderSamRequiredLabels) != "" {
			sawLabelsHeader.Store(true)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/.well-known/agent-card.json":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"echo-agent",` +
				`"supportedInterfaces":[` +
				`{"url":"http://localhost:9999","protocolBinding":"JSONRPC","protocolVersion":"1.0"},` +
				`{"url":"localhost:50051","protocolBinding":"GRPC","protocolVersion":"1.0"}],` +
				`"capabilities":{"streaming":true}}`))
		case r.Method == http.MethodPost:
			sendCount.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"kind":"message",` +
				`"messageId":"m1","role":"agent","parts":[{"kind":"text","text":"echo from eu"}]}}`))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer agent.Close()

	registerA2AService(t, apiAddrA, apiToken, "echo-agent", agent.URL)

	meshBase := "http://" + apiAddrB + "/sam/" + peerA + "/a2a/echo-agent"

	// CUJ step 1: the agent card comes back rewritten for mesh use. Poll:
	// the first fetch can race connectivity establishment.
	deadline := time.Now().Add(30 * time.Second)
	var cardBody []byte
	for {
		req, _ := http.NewRequest("GET", meshBase+"/.well-known/agent-card.json", nil)
		req.Header.Set(api.HeaderSamAuthentication, "Bearer "+apiToken)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			cardBody, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout fetching agent card, last body: %s", string(cardBody))
		}
		time.Sleep(200 * time.Millisecond)
	}
	var card struct {
		SupportedInterfaces []struct {
			URL             string `json:"url"`
			ProtocolBinding string `json:"protocolBinding"`
		} `json:"supportedInterfaces"`
		Capabilities struct {
			Streaming bool `json:"streaming"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(cardBody, &card); err != nil {
		t.Fatalf("invalid card: %v, body: %s", err, string(cardBody))
	}
	if len(card.SupportedInterfaces) != 1 || card.SupportedInterfaces[0].URL != meshBase {
		t.Errorf("interfaces not regenerated / gRPC not dropped: %s", string(cardBody))
	}
	if card.Capabilities.Streaming {
		t.Error("streaming must be advertised off through the mesh")
	}

	// CUJ step 2: message/send constrained to region=eu is admitted.
	sendBody := `{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"message":` +
		`{"kind":"message","messageId":"c1","role":"user","parts":[{"kind":"text","text":"hi"}]}}}`
	deadline = time.Now().Add(30 * time.Second)
	for {
		req, _ := http.NewRequest("POST", meshBase+"/", strings.NewReader(sendBody))
		req.Header.Set(api.HeaderSamAuthentication, "Bearer "+apiToken)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(api.HeaderSamRequiredLabels, "region=eu")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("labelled message/send failed: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			if !strings.Contains(string(body), "echo from eu") {
				t.Fatalf("unexpected message/send response: %s", string(body))
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("labelled message/send status: %d, body: %s", resp.StatusCode, string(body))
		}
		time.Sleep(200 * time.Millisecond)
	}

	// CUJ step 3: a mismatched label refuses fail-closed BEFORE egress —
	// the agent backend must never see the request.
	before := sendCount.Load()
	req, _ := http.NewRequest("POST", meshBase+"/", strings.NewReader(sendBody))
	req.Header.Set(api.HeaderSamAuthentication, "Bearer "+apiToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(api.HeaderSamRequiredLabels, "region=us-east-1")
	respUS, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("mismatched-label message/send failed: %v", err)
	}
	usBody, _ := io.ReadAll(respUS.Body)
	_ = respUS.Body.Close()
	if respUS.StatusCode != http.StatusForbidden {
		t.Fatalf("mismatched label must fail closed with 403: got %d, body: %s", respUS.StatusCode, string(usBody))
	}
	if sendCount.Load() != before {
		t.Fatal("payload reached the agent despite label refusal")
	}

	// Zero-trust invariant: the labels header never crosses the mesh.
	if sawLabelsHeader.Load() {
		t.Errorf("%s header leaked to the agent backend", api.HeaderSamRequiredLabels)
	}

	t.Log("A2A CUJ test passed.")
}

func registerA2AService(t *testing.T, apiAddr, token, serviceName, targetURL string) {
	t.Helper()
	reqData := &api.RegisterServiceRequest{
		Service: &api.ServiceInfo{
			Type:        api.ServiceType_SERVICE_TYPE_A2A,
			Name:        serviceName,
			Description: "test a2a agent",
		},
		Backend: &api.RegisterServiceRequest_TargetUrl{TargetUrl: targetURL},
	}
	body, err := protojson.Marshal(reqData)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("POST", "http://"+apiAddr+"/sam/service/register", bytes.NewBuffer(body))
	req.Header.Set(api.HeaderSamAuthentication, "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Failed to register a2a service: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		t.Fatalf("Register a2a service failed with status: %d, body: %s", resp.StatusCode, string(bodyBytes))
	}
}
