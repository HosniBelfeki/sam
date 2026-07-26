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
	"fmt"
	"sync"
	"time"

	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	"google.golang.org/protobuf/proto"

	"github.com/google/sam/api"
	"github.com/google/sam/internal/storage"
)

// ServiceAnnouncement represents service discovery details retrieved from the mesh.
type ServiceAnnouncement struct {
	ServiceName string
	ServiceType string
	PeerID      string
	Addresses   []string
}

// NodeStatus represents mesh status information for a peer.
type NodeStatus struct {
	PeerID      string
	IsReachable bool
	Addresses   []string
	LastSeen    time.Time
}

// MeshAdapter defines the generic interface for Control Plane operations interacting with the Sovereign Agent Mesh.
type MeshAdapter interface {
	// PublishEvent constructs, signs, and broadcasts a Control Plane MeshEvent (POLICY_UPDATE, BANNED, KEY_ROTATION).
	PublishEvent(ctx context.Context, eventType api.MeshEvent_Type, peerID string, payload []byte) error

	// DiscoverServices queries active mesh nodes/DHT for services matching a type or pattern.
	DiscoverServices(ctx context.Context, serviceType string) ([]*ServiceAnnouncement, error)

	// GetNodeStatus retrieves node reachability and status from the mesh.
	GetNodeStatus(ctx context.Context, peerID string) (*NodeStatus, error)

	// Close gracefully releases any P2P host resources, streams, and PubSub topics.
	Close() error
}

// NopMeshAdapter provides a no-op implementation used when P2P mesh integration is disabled or in unit tests.
type NopMeshAdapter struct{}

func NewNopMeshAdapter() *NopMeshAdapter {
	return &NopMeshAdapter{}
}

func (n *NopMeshAdapter) PublishEvent(ctx context.Context, eventType api.MeshEvent_Type, peerID string, payload []byte) error {
	logger.Debugf("[NopMeshAdapter] PublishEvent skipped (P2P disabled): type=%v, peerID=%s", eventType, peerID)
	return nil
}

func (n *NopMeshAdapter) DiscoverServices(ctx context.Context, serviceType string) ([]*ServiceAnnouncement, error) {
	return nil, nil
}

func (n *NopMeshAdapter) GetNodeStatus(ctx context.Context, peerID string) (*NodeStatus, error) {
	return nil, nil
}

func (n *NopMeshAdapter) Close() error {
	return nil
}

// P2PMeshAdapter implements MeshAdapter using a libp2p Host and GossipSub subscriber/publisher.
type P2PMeshAdapter struct {
	host  host.Host
	ps    *pubsub.PubSub
	topic *pubsub.Topic
	store storage.Store
	mu    sync.Mutex
}

func NewP2PMeshAdapter(h host.Host, ps *pubsub.PubSub, store storage.Store) (*P2PMeshAdapter, error) {
	if h == nil || ps == nil || store == nil {
		return nil, fmt.Errorf("host, pubsub, and store cannot be nil")
	}
	topic, err := ps.Join(api.GossipEvents)
	if err != nil {
		return nil, fmt.Errorf("failed to join gossip events topic %s: %w", api.GossipEvents, err)
	}

	return &P2PMeshAdapter{
		host:  h,
		ps:    ps,
		topic: topic,
		store: store,
	}, nil
}

func (p *P2PMeshAdapter) ConnectPeer(ctx context.Context, targetAddr string) error {
	info, err := peer.AddrInfoFromString(targetAddr)
	if err != nil {
		return fmt.Errorf("invalid peer address string %q: %w", targetAddr, err)
	}
	p.host.Peerstore().AddAddrs(info.ID, info.Addrs, peerstore.PermanentAddrTTL)
	if err := p.host.Connect(ctx, *info); err != nil {
		return fmt.Errorf("failed to connect to peer %s: %w", info.ID, err)
	}
	return nil
}

func (p *P2PMeshAdapter) PublishEvent(ctx context.Context, eventType api.MeshEvent_Type, peerID string, payload []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	var privKey ed25519.PrivateKey
	if p.store != nil {
		key, _, err := p.store.GetCurrentKey(ctx)
		if err != nil {
			return fmt.Errorf("failed to retrieve signing key for event publishing: %w", err)
		}
		privKey = key
	}

	event := &api.MeshEvent{
		Type:         eventType,
		PeerId:       peerID,
		Timestamp:    time.Now().UnixMilli(),
		NewPublicKey: payload,
	}

	if privKey != nil {
		eventData, err := proto.Marshal(event)
		if err != nil {
			return fmt.Errorf("failed to marshal event for signing: %w", err)
		}
		event.Signature = ed25519.Sign(privKey, eventData)
	}

	data, err := proto.Marshal(event)
	if err != nil {
		return fmt.Errorf("failed to marshal signed mesh event: %w", err)
	}

	if err := p.topic.Publish(ctx, data); err != nil {
		return fmt.Errorf("failed to publish mesh event to topic %s: %w", api.GossipEvents, err)
	}

	logger.Infof("[P2PMeshAdapter] Published MeshEvent %v to topic %s (peerID: %s)", eventType, api.GossipEvents, peerID)
	return nil
}

func (p *P2PMeshAdapter) DiscoverServices(ctx context.Context, serviceType string) ([]*ServiceAnnouncement, error) {
	return nil, nil
}

func (p *P2PMeshAdapter) GetNodeStatus(ctx context.Context, peerID string) (*NodeStatus, error) {
	return nil, nil
}

func (p *P2PMeshAdapter) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.topic != nil {
		_ = p.topic.Close()
	}
	if p.host != nil {
		return p.host.Close()
	}
	return nil
}
