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
	"sync/atomic"
	"testing"
	"time"

	"github.com/biscuit-auth/biscuit-go/v2"
	"github.com/google/sam/api"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-msgio"
	"github.com/multiformats/go-multiaddr"
	"google.golang.org/protobuf/proto"
)

// A router without an external URL advertises one multiaddr per interface,
// all for the same peer. Authenticating once per address re-runs the
// handshake on the already-open connection, which trips the router's
// per-peer handshake limiter and logs a failure for every extra address.
// One live authenticated session per router must be enough.
func TestStartAuthenticatesEachRouterOnce(t *testing.T) {
	cpPub, cpPriv, _ := ed25519.GenerateKey(nil)

	// Two listen addresses on the same host stand in for the interfaces a
	// real router advertises; the handshake counter is per router, not per
	// address.
	h, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0", "/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })

	mint := func(peerID, role string) []byte {
		b := biscuit.NewBuilder(cpPriv)
		for _, f := range []biscuit.Fact{
			{Predicate: biscuit.Predicate{Name: api.FactNode, IDs: []biscuit.Term{biscuit.String(peerID)}}},
			{Predicate: biscuit.Predicate{Name: api.FactRole, IDs: []biscuit.Term{biscuit.String(role)}}},
			{Predicate: biscuit.Predicate{Name: api.FactExpiration, IDs: []biscuit.Term{biscuit.Date(time.Now().Add(24 * time.Hour))}}},
			{Predicate: biscuit.Predicate{Name: api.FactTargetUnrestricted}},
		} {
			if err := b.AddAuthorityFact(f); err != nil {
				t.Fatal(err)
			}
		}
		tok, err := b.Build()
		if err != nil {
			t.Fatal(err)
		}
		out, err := tok.Serialize()
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	routerBiscuit := mint(h.ID().String(), api.RoleRouter)

	var handshakes atomic.Int32
	h.SetStreamHandler(api.AuthProtocolID, func(s network.Stream) {
		defer func() { _ = s.Close() }()
		handshakes.Add(1)
		reader := msgio.NewVarintReaderSize(s, 1024*64)
		msg, err := reader.ReadMsg()
		if err != nil {
			return
		}
		reader.ReleaseMsg(msg)
		data, _ := proto.Marshal(&api.AuthResponse{Success: true, Biscuit: routerBiscuit})
		_ = msgio.NewVarintWriter(s).WriteMsg(data)
	})

	var routerAddrs []multiaddr.Multiaddr
	for _, a := range h.Addrs() {
		ma, err := multiaddr.NewMultiaddr(a.String() + "/p2p/" + h.ID().String())
		if err != nil {
			t.Fatal(err)
		}
		routerAddrs = append(routerAddrs, ma)
	}
	// The same address listed twice, as a control plane that stores the
	// router's lease verbatim can hand out.
	routerAddrs = append(routerAddrs, routerAddrs[0])
	if len(routerAddrs) < 3 {
		t.Fatalf("expected at least 3 router multiaddrs, got %d", len(routerAddrs))
	}

	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	privKey := GetOrGenerateKey(store)
	pid, err := peer.IDFromPrivateKey(privKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveIdentity(mint(pid.String(), api.RoleNode)); err != nil {
		t.Fatal(err)
	}

	node, err := NewSamNode(Options{
		PrivKey:            privKey,
		Store:              store,
		ControlPlanePubKey: cpPub,
		RouterAddrs:        routerAddrs,
		ListenAddrs:        []string{"/ip4/127.0.0.1/tcp/0"},
		AllowLoopback:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := node.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = node.Teardown() })

	if got := handshakes.Load(); got != 1 {
		t.Fatalf("router saw %d handshakes for %d advertised addresses, want 1", got, len(routerAddrs))
	}
	if !node.IsConnected() {
		t.Fatal("node does not report an authenticated router connection")
	}

	// A re-run with the session still open (what the connection monitor and
	// a re-enrollment do) must not handshake again either.
	for _, a := range routerAddrs {
		if err := node.ConnectAndAuthWithRouter(ctx, a); err != nil {
			t.Fatalf("ConnectAndAuthWithRouter(%s): %v", a, err)
		}
	}
	if got := handshakes.Load(); got != 1 {
		t.Fatalf("re-auth over a live session handshook again: %d total, want 1", got)
	}

	// Once the session is gone the router has forgotten the peer, so the
	// next attempt must handshake afresh rather than trust the stale entry.
	if err := node.Host.Network().ClosePeer(h.ID()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for node.Host.Network().Connectedness(h.ID()) == network.Connected && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if err := node.ConnectAndAuthWithRouter(ctx, routerAddrs[0]); err != nil {
		t.Fatalf("re-auth after disconnect: %v", err)
	}
	if got := handshakes.Load(); got != 2 {
		t.Fatalf("re-auth after disconnect: %d handshakes, want 2", got)
	}
}
