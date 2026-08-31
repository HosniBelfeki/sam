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
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/biscuit-auth/biscuit-go/v2"
	"github.com/google/sam/api"
	"github.com/google/sam/internal/identity"
	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
)

func TestAnnounceFilter(t *testing.T) {
	const (
		loopback  = "/ip4/127.0.0.1/tcp/5002"
		linkLocal = "/ip4/169.254.10.1/tcp/5002"
		private   = "/ip4/192.168.94.13/tcp/5002"
		public    = "/ip4/34.91.220.211/udp/8192/quic-v1"
		// Relay path whose router sits on a private address, as in an on-premises mesh.
		circuit = "/ip4/10.0.0.7/tcp/4501/p2p/12D3KooWG1pA6goegCncqwbZLSr8pnjUZ6JMAAe6SmnHTgUNCk88/p2p-circuit"
	)
	input := []multiaddr.Multiaddr{}
	for _, s := range []string{loopback, linkLocal, private, public, circuit} {
		ma, err := multiaddr.NewMultiaddr(s)
		if err != nil {
			t.Fatalf("failed to parse %s: %v", s, err)
		}
		input = append(input, ma)
	}

	announce := true
	suppress := false

	tests := []struct {
		name            string
		allowLoopback   bool
		announcePrivate *bool
		want            []string
	}{
		{
			name: "default keeps private addresses so LAN meshes stay reachable",
			want: []string{private, public, circuit},
		},
		{
			name:            "explicit announce private matches the default",
			announcePrivate: &announce,
			want:            []string{private, public, circuit},
		},
		{
			name:            "suppressing private addresses still announces relay paths",
			announcePrivate: &suppress,
			want:            []string{public, circuit},
		},
		{
			name:          "allow loopback publishes everything",
			allowLoopback: true,
			want:          []string{loopback, linkLocal, private, public, circuit},
		},
		{
			name:            "allow loopback combined with suppressed private addresses",
			allowLoopback:   true,
			announcePrivate: &suppress,
			want:            []string{loopback, linkLocal, public, circuit},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := &SamNode{config: Options{
				AllowLoopback:        tt.allowLoopback,
				AnnouncePrivateAddrs: tt.announcePrivate,
			}}
			got := n.announceFilter(input)
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i, addr := range got {
				if addr.String() != tt.want[i] {
					t.Errorf("addr %d: got %s, want %s", i, addr, tt.want[i])
				}
			}
		})
	}
}

func TestHandleBannedEvent(t *testing.T) {
	revokedCache, _ := lru.New[string, int64](10)
	node := &SamNode{
		revokedPeers:   revokedCache,
		BiscuitTimeout: 500 * time.Millisecond,
	}

	event := &api.MeshEvent{
		Type:      api.MeshEvent_BANNED,
		PeerId:    "12D3KooWAFv4iJst5G6MjwXhZ66K5zS1tP7A9vSg4vK8f1T7X8t9",
		Timestamp: time.Now().UnixMilli(),
	}

	node.handleBannedEvent(event)

	if !node.revokedPeers.Contains(event.PeerId) {
		t.Error("Expected peer to be added to revokedPeers")
	}
}

func TestHandleKeyRotationEvent(t *testing.T) {
	node := &SamNode{BiscuitTimeout: 500 * time.Millisecond}

	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("Failed to generate key: %v", err)
	}

	event := &api.MeshEvent{
		Type:         api.MeshEvent_KEY_ROTATION,
		NewPublicKey: pub,
		Timestamp:    time.Now().UnixMilli(),
	}

	node.handleKeyRotationEvent(event)

	if len(node.trustedKeys) != 1 {
		t.Errorf("Expected 1 trusted key, got %d", len(node.trustedKeys))
	}
}

// TestKeyRotationEventSurvivesRestart covers the offline/restart half of
// #325: a key learned from a rotation event must outlive the process, or a
// restarted node falls back to its stale enrollment-time key.
func TestKeyRotationEventSurvivesRestart(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	enrollKey, _, _ := ed25519.GenerateKey(nil)
	rotatedKey, _, _ := ed25519.GenerateKey(nil)

	node := &SamNode{Store: store, trustedKeys: []TrustedKey{{Key: enrollKey, ReceivedAt: time.Now()}}}
	node.handleKeyRotationEvent(&api.MeshEvent{
		Type:         api.MeshEvent_KEY_ROTATION,
		NewPublicKey: rotatedKey,
		Timestamp:    time.Now().UnixMilli(),
	})
	// Duplicate event must not grow the persisted set
	node.handleKeyRotationEvent(&api.MeshEvent{
		Type:         api.MeshEvent_KEY_ROTATION,
		NewPublicKey: rotatedKey,
		Timestamp:    time.Now().UnixMilli(),
	})

	// "Restart": a fresh node built from the same store must trust the rotated key
	priv, _, _ := crypto.GenerateKeyPair(crypto.Ed25519, -1)
	restarted, err := NewSamNode(Options{PrivKey: priv, Store: store, ControlPlanePubKey: enrollKey})
	if err != nil {
		t.Fatal(err)
	}
	if len(restarted.trustedKeys) != 2 {
		t.Fatalf("expected 2 trusted keys after restart, got %d", len(restarted.trustedKeys))
	}
	if !containsTrustedKey(restarted.trustedKeys, rotatedKey) {
		t.Error("rotated key lost across restart")
	}
	if !containsTrustedKey(restarted.trustedKeys, enrollKey) {
		t.Error("enrollment key missing after restart")
	}
}

// TestPruneTrustedKeys covers the trust-set floor: a node that runs past the
// grace period without witnessing a rotation must not age out its only key.
func TestPruneTrustedKeys(t *testing.T) {
	now := time.Now()
	grace := 24 * time.Hour
	k := func(age time.Duration) TrustedKey {
		pub, _, err := ed25519.GenerateKey(nil)
		if err != nil {
			t.Fatal(err)
		}
		return TrustedKey{Key: pub, ReceivedAt: now.Add(-age)}
	}

	stale := k(48 * time.Hour)
	staler := k(72 * time.Hour)
	fresh := k(time.Hour)

	tests := []struct {
		name string
		in   []TrustedKey
		want []TrustedKey
	}{
		{"sole stale key survives", []TrustedKey{stale}, []TrustedKey{stale}},
		{"stale dropped when fresher exists", []TrustedKey{staler, fresh}, []TrustedKey{fresh}},
		{"newest of two stale keys survives", []TrustedKey{staler, stale}, []TrustedKey{stale}},
		{"fresh keys all kept", []TrustedKey{fresh, k(2 * time.Hour)}, nil}, // want computed below
		{"empty", nil, nil},
	}
	tests[3].want = tests[3].in

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pruneTrustedKeys(tt.in, now, grace)
			if len(got) != len(tt.want) {
				t.Fatalf("got %d keys, want %d", len(got), len(tt.want))
			}
			for i := range got {
				if !got[i].Key.Equal(tt.want[i].Key) {
					t.Errorf("key %d: got %x, want %x", i, got[i].Key, tt.want[i].Key)
				}
			}
		})
	}
}

// TestVerifyBiscuitRejectsExpiredToken covers the peer-admission half of #296:
// HandleAuthHandshake admits a peer into authPeers, which the relay ACL then
// trusts, so it must reject an expired token exactly like the dataplane does.
func TestVerifyBiscuitRejectsExpiredToken(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	remotePeer := peer.ID("dummy-peer")

	mint := func(expiration time.Time) []byte {
		builder := biscuit.NewBuilder(priv)
		for _, f := range []biscuit.Fact{
			{Predicate: biscuit.Predicate{Name: api.FactNode, IDs: []biscuit.Term{biscuit.String(remotePeer.String())}}},
			{Predicate: biscuit.Predicate{Name: api.FactExpiration, IDs: []biscuit.Term{biscuit.Date(expiration)}}},
		} {
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

	want := time.Now().Add(time.Hour)
	_, expiry, err := node.verifyBiscuit(mint(want), remotePeer)
	if err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	// The admission is cached against this instant, so it has to be the token's.
	if skew := expiry.Sub(want); skew < -time.Second || skew > time.Second {
		t.Errorf("reported expiry %v, want ~%v", expiry, want)
	}

	if _, _, err := node.verifyBiscuit(mint(time.Now().Add(-time.Hour)), remotePeer); err == nil {
		t.Fatal("expired token admitted on the peer-authentication path")
	}
}

func TestStartRenewalLoop_ExpiredAndFails(t *testing.T) {
	if os.Getenv("BE_CRASHER") == "1" {
		store, _ := NewStore(t.TempDir())
		// Set expiration to the past
		_ = store.SaveIdentityExpiration(time.Now().Add(-1 * time.Hour).Unix())

		node := &SamNode{
			BiscuitTimeout: 500 * time.Millisecond,
			Store:          store,
		}

		// Run the renewal loop. Since there's no JWT/Issuer provided, it fails to renew.
		// It will see that it's expired and it failed to renew, so it will log.Fatalf
		node.StartRenewalLoop(context.Background(), "", "", "", "")
		time.Sleep(2 * time.Second)
		os.Exit(0) // should not be reached
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestStartRenewalLoop_ExpiredAndFails")
	cmd.Env = append(os.Environ(), "BE_CRASHER=1")
	err := cmd.Run()
	if e, ok := err.(*exec.ExitError); ok && !e.Success() {
		return // Successful fatal exit
	}
	t.Fatalf("process ran with err %v, want exit status 1 (fatal crash)", err)
}

func TestConnectionMonitor_CrashesAfterFailures(t *testing.T) {
	if os.Getenv("BE_CRASHER_MONITOR") == "1" {
		priv, _, _ := crypto.GenerateKeyPair(crypto.Ed25519, -1)
		store, _ := NewStore(t.TempDir())
		node, err := NewSamNode(Options{
			PrivKey:           priv,
			RouterAddrs:       nil,
			Store:             store,
			MeshID:            "test",
			DiscoveryInterval: "10s",
			ListenAddrs:       []string{"/ip4/127.0.0.1/tcp/0"},
			EnableRelay:       false,
			NodeConfig:        nil,
			KeyGracePeriod:    0,
			AllowLoopback:     false,
			MonitorBootstrap:  2 * time.Minute,
			MonitorInterval:   1 * time.Minute,
		})
		if err != nil {
			os.Exit(0) // Ignore NewSamNode errors for this crasher
		}
		if err := node.Start(context.Background()); err != nil {
			os.Exit(0)
		}

		// Use very short durations
		node.startConnectionMonitor(context.Background(), 10*time.Millisecond, 10*time.Millisecond, 3)
		time.Sleep(1 * time.Second)
		os.Exit(0) // should not be reached
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestConnectionMonitor_CrashesAfterFailures")
	cmd.Env = append(os.Environ(), "BE_CRASHER_MONITOR=1")
	err := cmd.Run()
	if e, ok := err.(*exec.ExitError); ok && !e.Success() {
		return // Successful fatal exit
	}
	t.Fatalf("process ran with err %v, want exit status 1 (fatal crash)", err)
}

func TestNewSamNode_Validation(t *testing.T) {
	priv, _, _ := crypto.GenerateKeyPair(crypto.Ed25519, -1)
	store, _ := NewStore(t.TempDir())
	defer func() { _ = store.Close() }()

	t.Run("nil PrivKey", func(t *testing.T) {
		_, err := NewSamNode(Options{
			PrivKey: nil,
			Store:   store,
		})
		if err == nil || err.Error() != "private key is required" {
			t.Errorf("expected 'private key is required' error, got: %v", err)
		}
	})

	t.Run("nil Store", func(t *testing.T) {
		_, err := NewSamNode(Options{
			PrivKey: priv,
			Store:   nil,
		})
		if err == nil || err.Error() != "store is required" {
			t.Errorf("expected 'store is required' error, got: %v", err)
		}
	})
}

func TestNewSamNode_BiscuitTimeout(t *testing.T) {
	priv, _, _ := crypto.GenerateKeyPair(crypto.Ed25519, -1)
	store, _ := NewStore(t.TempDir())
	defer func() { _ = store.Close() }()

	t.Run("defaults when unset", func(t *testing.T) {
		node, err := NewSamNode(Options{
			PrivKey: priv,
			Store:   store,
		})
		if err != nil {
			t.Fatalf("NewSamNode: %v", err)
		}
		if node.BiscuitTimeout != identity.DefaultAuthorizerTimeout {
			t.Errorf("BiscuitTimeout = %v, want default %v", node.BiscuitTimeout, identity.DefaultAuthorizerTimeout)
		}
	})

	t.Run("explicit value respected", func(t *testing.T) {
		node, err := NewSamNode(Options{
			PrivKey:        priv,
			Store:          store,
			BiscuitTimeout: 10 * time.Second,
		})
		if err != nil {
			t.Fatalf("NewSamNode: %v", err)
		}
		if node.BiscuitTimeout != 10*time.Second {
			t.Errorf("BiscuitTimeout = %v, want 10s", node.BiscuitTimeout)
		}
	})
}

func TestNewSamNode_DHTOptions(t *testing.T) {
	priv, _, _ := crypto.GenerateKeyPair(crypto.Ed25519, -1)
	store, _ := NewStore(t.TempDir())
	defer func() { _ = store.Close() }()

	opts := Options{
		PrivKey:              priv,
		Store:                store,
		ListenAddrs:          []string{"/ip4/127.0.0.1/tcp/0"},
		DHTProviderAddrTTL:   10 * time.Second,
		DHTMaxRecordAge:      15 * time.Second,
		DHTLookupLimit:       50,
		DiscoveryConcurrency: 5,
	}

	node, err := NewSamNode(opts)
	if err != nil {
		t.Fatalf("failed to create node with DHT options: %v", err)
	}

	if node.config.DHTProviderAddrTTL != 10*time.Second {
		t.Errorf("expected DHTProviderAddrTTL to be 10s, got %v", node.config.DHTProviderAddrTTL)
	}
	if node.config.DHTMaxRecordAge != 15*time.Second {
		t.Errorf("expected DHTMaxRecordAge to be 15s, got %v", node.config.DHTMaxRecordAge)
	}
	if node.config.DHTLookupLimit != 50 {
		t.Errorf("expected DHTLookupLimit to be 50, got %d", node.config.DHTLookupLimit)
	}
	if node.config.DiscoveryConcurrency != 5 {
		t.Errorf("expected DiscoveryConcurrency to be 5, got %d", node.config.DiscoveryConcurrency)
	}
}
