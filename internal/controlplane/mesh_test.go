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
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/peer"

	"github.com/libp2p/go-libp2p"
	"google.golang.org/protobuf/proto"

	"github.com/google/sam/api"
	"github.com/google/sam/internal/storage"
)

func TestNopMeshAdapter(t *testing.T) {
	adapter := NewNopMeshAdapter()
	ctx := context.Background()

	if err := adapter.PublishEvent(ctx, api.MeshEvent_POLICY_UPDATE, "", nil); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}

	services, err := adapter.DiscoverServices(ctx, "test")
	if err != nil || len(services) != 0 {
		t.Fatalf("unexpected DiscoverServices output: %v, %v", services, err)
	}

	status, err := adapter.GetNodeStatus(ctx, "peer1")
	if err != nil || status != nil {
		t.Fatalf("unexpected GetNodeStatus output: %v, %v", status, err)
	}

	if err := adapter.Close(); err != nil {
		t.Fatalf("expected nil error on Close, got %v", err)
	}
}

func TestP2PMeshAdapter_PublishAndSubscribe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 1. Create Control Plane store & initial keyring
	store, err := storage.NewSQLStore("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer func() { _ = store.Close() }()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	if err := store.SaveInitialKey(ctx, priv, pub); err != nil {
		t.Fatalf("failed to save key: %v", err)
	}

	// 2. Create libp2p host and PubSub for Control Plane P2PMeshAdapter
	cpHost, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatalf("failed to create cp host: %v", err)
	}
	defer func() { _ = cpHost.Close() }()

	cpPS, err := pubsub.NewGossipSub(ctx, cpHost)
	if err != nil {
		t.Fatalf("failed to create cp pubsub: %v", err)
	}

	adapter, err := NewP2PMeshAdapter(cpHost, cpPS, store)
	if err != nil {
		t.Fatalf("failed to create P2PMeshAdapter: %v", err)
	}
	defer func() { _ = adapter.Close() }()

	// 3. Create subscriber node host & PubSub
	subHost, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatalf("failed to create sub host: %v", err)
	}
	defer func() { _ = subHost.Close() }()

	subPS, err := pubsub.NewGossipSub(ctx, subHost)
	if err != nil {
		t.Fatalf("failed to create sub pubsub: %v", err)
	}

	subTopic, err := subPS.Join(api.GossipEvents)
	if err != nil {
		t.Fatalf("failed to join topic: %v", err)
	}
	sub, err := subTopic.Subscribe()
	if err != nil {
		t.Fatalf("failed to subscribe to topic: %v", err)
	}

	// 4. Connect subscriber node to Control Plane host
	subHost.Peerstore().AddAddrs(cpHost.ID(), cpHost.Addrs(), 10*time.Second)
	if err := subHost.Connect(ctx, peer.AddrInfo{ID: cpHost.ID(), Addrs: cpHost.Addrs()}); err != nil {
		t.Fatalf("failed to connect subHost to cpHost: %v", err)
	}

	// Allow GossipSub mesh overlay connection to settle
	time.Sleep(500 * time.Millisecond)

	// 5. Test publishing BANNED event
	targetPeerID := "12D3KooWBannedPeerIDForTesting1234567890"
	if err := adapter.PublishEvent(ctx, api.MeshEvent_BANNED, targetPeerID, nil); err != nil {
		t.Fatalf("failed to publish BANNED event: %v", err)
	}

	msg, err := sub.Next(ctx)
	if err != nil {
		t.Fatalf("failed to receive event on subscriber: %v", err)
	}

	var event api.MeshEvent
	if err := proto.Unmarshal(msg.Data, &event); err != nil {
		t.Fatalf("failed to unmarshal received MeshEvent: %v", err)
	}

	if event.Type != api.MeshEvent_BANNED {
		t.Fatalf("expected event type %v, got %v", api.MeshEvent_BANNED, event.Type)
	}
	if event.PeerId != targetPeerID {
		t.Fatalf("expected peerID %q, got %q", targetPeerID, event.PeerId)
	}
	if len(event.Signature) == 0 {
		t.Fatalf("expected signed event signature, got empty signature")
	}

	// Verify Ed25519 signature against Control Plane public key
	sig := event.Signature
	event.Signature = nil
	eventData, err := proto.Marshal(&event)
	if err != nil {
		t.Fatalf("failed to marshal event for sig verification: %v", err)
	}
	if !ed25519.Verify(pub, eventData, sig) {
		t.Fatalf("ed25519 signature verification failed for published MeshEvent")
	}
}
