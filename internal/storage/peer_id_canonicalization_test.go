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

package storage

import (
	"context"
	"crypto/rand"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/sam/api"
	libcrypto "github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

func newPeerID(t *testing.T) peer.ID {
	t.Helper()
	priv, _, err := libcrypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	id, err := peer.IDFromPrivateKey(priv)
	if err != nil {
		t.Fatalf("failed to derive peer id: %v", err)
	}
	return id
}

func aliasOf(t *testing.T, id peer.ID) string {
	t.Helper()
	alias := peer.ToCid(id).String()
	if alias == id.String() {
		t.Fatalf("peer.ToCid(%s) did not produce an alias", id)
	}
	decoded, err := peer.Decode(alias)
	if err != nil {
		t.Fatalf("alias %q does not decode: %v", alias, err)
	}
	if decoded != id {
		t.Fatalf("alias %q decodes to %s, want %s", alias, decoded, id)
	}
	return alias
}

func openMigratedStore(t *testing.T, path string) *SQLStore {
	t.Helper()
	store, err := NewSQLStore("sqlite", path)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	if _, err := store.db.Exec(`DELETE FROM schema_migrations WHERE version = 9`); err != nil {
		t.Fatalf("failed to reset migration marker: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("failed to close store: %v", err)
	}

	reopened, err := NewSQLStore("sqlite", path)
	if err != nil {
		t.Fatalf("failed to reopen store: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	return reopened
}

func newMigratableStore(t *testing.T) (*SQLStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	store, err := NewSQLStore("sqlite", path)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	return store, path
}

func insertNodeRow(t *testing.T, s *SQLStore, peerID string, banned bool, enrolledAt int64) {
	t.Helper()
	_, err := s.db.Exec(
		`INSERT INTO nodes (peer_id, public_key, biscuit_token, role, enrollment_type, claims_json, owner_id, labels_json, enrolled_at, expires_at, banned)
		 VALUES (?, ?, ?, ?, ?, ?, NULL, ?, ?, ?, ?)`,
		peerID, []byte("public-key"), []byte("biscuit"), "node", "BOOTSTRAP", "", "{}", enrolledAt, enrolledAt, banned)
	if err != nil {
		t.Fatalf("failed to insert node %q: %v", peerID, err)
	}
}

func insertEnrollmentRequestRow(t *testing.T, s *SQLStore, id, peerID string, createdAt int64) {
	t.Helper()
	_, err := s.db.Exec(
		`INSERT INTO enrollment_requests (id, peer_id, public_key, token_id, status, labels_json, biscuit_token, created_at, resolved_at, resolved_by)
		 VALUES (?, ?, ?, '', ?, '{}', NULL, ?, NULL, '')`,
		id, peerID, []byte("public-key"), 0, createdAt)
	if err != nil {
		t.Fatalf("failed to insert enrollment request %q: %v", id, err)
	}
}

func insertRouterRow(t *testing.T, s *SQLStore, peerID string, lastRenewal int64) {
	t.Helper()
	_, err := s.db.Exec(
		`INSERT INTO routers (peer_id, multiaddresses, last_lease_renewal, expires_at, connected_peers, dht_size)
		 VALUES (?, '[]', ?, ?, '[]', 0)`,
		peerID, lastRenewal, time.Now().Add(time.Hour).UnixMilli())
	if err != nil {
		t.Fatalf("failed to insert router %q: %v", peerID, err)
	}
}

func TestMigrationRewritesAliasedPeerIDs(t *testing.T) {
	store, path := newMigratableStore(t)
	id := newPeerID(t)
	alias := aliasOf(t, id)

	insertNodeRow(t, store, alias, false, time.Now().UnixMilli())
	insertEnrollmentRequestRow(t, store, "req-1", alias, time.Now().UnixMilli())
	insertRouterRow(t, store, alias, time.Now().UnixMilli())
	if err := store.Close(); err != nil {
		t.Fatalf("failed to close store: %v", err)
	}

	migrated := openMigratedStore(t, path)
	ctx := context.Background()

	node, err := migrated.GetNode(ctx, id.String())
	if err != nil {
		t.Fatalf("node not reachable under its canonical id: %v", err)
	}
	if node.PeerID != id.String() {
		t.Errorf("node peer id = %q, want %q", node.PeerID, id.String())
	}
	if _, err := migrated.GetNode(ctx, alias); err != ErrNotFound {
		t.Errorf("aliased node row survived: err = %v", err)
	}

	if _, err := migrated.GetEnrollmentRequest(ctx, id.String()); err != nil {
		t.Errorf("enrollment request not reachable under its canonical id: %v", err)
	}

	leases, err := migrated.GetActiveRouters(ctx)
	if err != nil {
		t.Fatalf("failed to list routers: %v", err)
	}
	if len(leases) != 1 || leases[0].PeerID != id.String() {
		t.Errorf("router leases = %+v, want a single lease for %s", leases, id.String())
	}
}

func TestMigrationKeepsBansAcrossSpellings(t *testing.T) {
	cases := []struct {
		name            string
		aliasedBanned   bool
		canonicalBanned bool
		wantBanned      bool
	}{
		{name: "ban on the aliased row", aliasedBanned: true, canonicalBanned: false, wantBanned: true},
		{name: "ban on the canonical row", aliasedBanned: false, canonicalBanned: true, wantBanned: true},
		{name: "ban on both rows", aliasedBanned: true, canonicalBanned: true, wantBanned: true},
		{name: "no ban on either row", aliasedBanned: false, canonicalBanned: false, wantBanned: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, path := newMigratableStore(t)
			id := newPeerID(t)
			alias := aliasOf(t, id)

			insertNodeRow(t, store, alias, tc.aliasedBanned, time.Now().UnixMilli())
			insertNodeRow(t, store, id.String(), tc.canonicalBanned, time.Now().UnixMilli())
			if err := store.Close(); err != nil {
				t.Fatalf("failed to close store: %v", err)
			}

			migrated := openMigratedStore(t, path)
			ctx := context.Background()

			nodes, err := migrated.ListNodes(ctx)
			if err != nil {
				t.Fatalf("failed to list nodes: %v", err)
			}
			if len(nodes) != 1 {
				t.Fatalf("got %d node rows, want the pair folded into one: %+v", len(nodes), nodes)
			}
			if nodes[0].PeerID != id.String() {
				t.Errorf("surviving row is keyed %q, want %q", nodes[0].PeerID, id.String())
			}

			banned, err := migrated.IsNodeBanned(ctx, id.String())
			if err != nil {
				t.Fatalf("failed to read ban: %v", err)
			}
			if banned != tc.wantBanned {
				t.Errorf("banned = %v, want %v", banned, tc.wantBanned)
			}
		})
	}
}

func TestMigrationKeepsNewestEnrollmentRequest(t *testing.T) {
	cases := []struct {
		name             string
		aliasedCreated   int64
		canonicalCreated int64
		wantID           string
	}{
		{name: "aliased request is newer", aliasedCreated: 2000, canonicalCreated: 1000, wantID: "aliased"},
		{name: "canonical request is newer", aliasedCreated: 1000, canonicalCreated: 2000, wantID: "canonical"},
		{name: "same age favours the canonical row", aliasedCreated: 1000, canonicalCreated: 1000, wantID: "canonical"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, path := newMigratableStore(t)
			id := newPeerID(t)
			alias := aliasOf(t, id)

			insertEnrollmentRequestRow(t, store, "aliased", alias, tc.aliasedCreated)
			insertEnrollmentRequestRow(t, store, "canonical", id.String(), tc.canonicalCreated)
			if err := store.Close(); err != nil {
				t.Fatalf("failed to close store: %v", err)
			}

			migrated := openMigratedStore(t, path)
			ctx := context.Background()

			requests, err := migrated.ListEnrollmentRequests(ctx)
			if err != nil {
				t.Fatalf("failed to list enrollment requests: %v", err)
			}
			if len(requests) != 1 {
				t.Fatalf("got %d enrollment requests, want one: %+v", len(requests), requests)
			}
			if requests[0].ID != tc.wantID {
				t.Errorf("surviving request = %q, want %q", requests[0].ID, tc.wantID)
			}
			if requests[0].PeerID != id.String() {
				t.Errorf("surviving request peer id = %q, want %q", requests[0].PeerID, id.String())
			}
		})
	}
}

func TestMigrationKeepsNewestRouterLease(t *testing.T) {
	cases := []struct {
		name             string
		aliasedRenewal   int64
		canonicalRenewal int64
		wantRenewal      int64
	}{
		{name: "aliased lease is newer", aliasedRenewal: 2000, canonicalRenewal: 1000, wantRenewal: 2000},
		{name: "canonical lease is newer", aliasedRenewal: 1000, canonicalRenewal: 2000, wantRenewal: 2000},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, path := newMigratableStore(t)
			id := newPeerID(t)
			alias := aliasOf(t, id)

			insertRouterRow(t, store, alias, tc.aliasedRenewal)
			insertRouterRow(t, store, id.String(), tc.canonicalRenewal)
			if err := store.Close(); err != nil {
				t.Fatalf("failed to close store: %v", err)
			}

			migrated := openMigratedStore(t, path)

			leases, err := migrated.GetActiveRouters(context.Background())
			if err != nil {
				t.Fatalf("failed to list routers: %v", err)
			}
			if len(leases) != 1 {
				t.Fatalf("got %d router leases, want one: %+v", len(leases), leases)
			}
			if got := leases[0].LastRenewal.UnixMilli(); got != tc.wantRenewal {
				t.Errorf("surviving lease renewed at %d, want %d", got, tc.wantRenewal)
			}
		})
	}
}

func TestMigrationLeavesUndecodablePeerIDsAlone(t *testing.T) {
	store, path := newMigratableStore(t)
	for _, id := range []string{"peer-id-1", "peer-node-a", "not-a-peer-id"} {
		insertNodeRow(t, store, id, false, time.Now().UnixMilli())
	}
	if err := store.Close(); err != nil {
		t.Fatalf("failed to close store: %v", err)
	}

	migrated := openMigratedStore(t, path)
	nodes, err := migrated.ListNodes(context.Background())
	if err != nil {
		t.Fatalf("failed to list nodes: %v", err)
	}
	if len(nodes) != 3 {
		t.Fatalf("got %d nodes, want the 3 undecodable rows untouched: %+v", len(nodes), nodes)
	}
}

func TestMigrationIsIdempotent(t *testing.T) {
	store, path := newMigratableStore(t)
	id := newPeerID(t)
	alias := aliasOf(t, id)
	insertNodeRow(t, store, alias, true, time.Now().UnixMilli())
	if err := store.Close(); err != nil {
		t.Fatalf("failed to close store: %v", err)
	}

	first := openMigratedStore(t, path)
	if err := first.Close(); err != nil {
		t.Fatalf("failed to close store: %v", err)
	}
	second := openMigratedStore(t, path)

	ctx := context.Background()
	nodes, err := second.ListNodes(ctx)
	if err != nil {
		t.Fatalf("failed to list nodes: %v", err)
	}
	if len(nodes) != 1 || nodes[0].PeerID != id.String() {
		t.Fatalf("second run changed the rows: %+v", nodes)
	}
	banned, err := second.IsNodeBanned(ctx, id.String())
	if err != nil {
		t.Fatalf("failed to read ban: %v", err)
	}
	if !banned {
		t.Error("ban was lost on the second run")
	}
}

func TestMigrationRunsInTheCallersTransaction(t *testing.T) {
	store, _ := newMigratableStore(t)
	defer func() { _ = store.Close() }()

	id := newPeerID(t)
	alias := aliasOf(t, id)
	insertNodeRow(t, store, alias, true, time.Now().UnixMilli())

	tx, err := store.db.Begin()
	if err != nil {
		t.Fatalf("failed to begin transaction: %v", err)
	}
	if err := migrateCanonicalPeerIDs(context.Background(), store, tx); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("failed to roll back: %v", err)
	}

	if _, err := store.GetNode(context.Background(), alias); err != nil {
		t.Fatalf("rollback did not restore the aliased row: %v", err)
	}
	if _, err := store.GetNode(context.Background(), id.String()); err != ErrNotFound {
		t.Errorf("canonical row exists after rollback: err = %v", err)
	}
}

func TestCanonicalPeerID(t *testing.T) {
	id := newPeerID(t)
	alias := aliasOf(t, id)

	canonical, err := api.CanonicalPeerID(alias)
	if err != nil {
		t.Fatalf("failed to canonicalize alias: %v", err)
	}
	if canonical != id.String() {
		t.Errorf("canonical = %q, want %q", canonical, id.String())
	}

	canonical, err = api.CanonicalPeerID(id.String())
	if err != nil {
		t.Fatalf("failed to canonicalize canonical id: %v", err)
	}
	if canonical != id.String() {
		t.Errorf("canonical form is not stable: %q", canonical)
	}

	if _, err := api.CanonicalPeerID("not-a-peer-id"); err == nil {
		t.Error("expected an error for an undecodable peer id")
	}
}
