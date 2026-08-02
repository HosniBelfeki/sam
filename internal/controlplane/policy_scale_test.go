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
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/biscuit-auth/biscuit-go/v2/datalog"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/sam/api"
	"github.com/google/sam/internal/identity"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

// TestPolicyScale is not a benchmark: it exercises the admin policy pipeline
// (config validation, role resolution, biscuit minting and authorization) at
// increasing numbers of roles/bindings and logs sizes and timings, so we have
// a record of where the current implementation starts to strain.
//
// The default tiers are kept under the biscuit-go authorizer's default world
// fact-count limit (1000 facts, see datalog.WithMaxFacts) so this test stays
// green in CI. Hitting that limit is an expected, informative outcome of
// pushing the scale further rather than a bug: once reached, the test logs it
// and stops escalating instead of failing. Any other error (validation, role
// resolution, or minting) still fails the test. Probe further with e.g.:
//
//	SAM_POLICY_SCALE_MAX=5000 go test ./internal/controlplane -run TestPolicyScale -v
func TestPolicyScale(t *testing.T) {
	tiers := []int{10, 100, 500}
	if extra := os.Getenv("SAM_POLICY_SCALE_MAX"); extra != "" {
		n, err := strconv.Atoi(extra)
		if err != nil {
			t.Fatalf("invalid SAM_POLICY_SCALE_MAX=%q: %v", extra, err)
		}
		tiers = append(tiers, n)
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	nodeKey, _, err := crypto.GenerateKeyPair(crypto.Ed25519, -1)
	if err != nil {
		t.Fatal(err)
	}
	nodePeer, err := peer.IDFromPrivateKey(nodeKey)
	if err != nil {
		t.Fatal(err)
	}

	for _, n := range tiers {
		roles := make([]*api.PolicyRole, 0, n)
		bindings := make([]*api.PolicyBinding, 0, n)
		roleNames := make([]string, 0, n)
		for i := 0; i < n; i++ {
			name := fmt.Sprintf("role-%d", i)
			roleNames = append(roleNames, name)
			roles = append(roles, &api.PolicyRole{
				Name: name,
				AllowedServices: []string{
					fmt.Sprintf("mcp://svc-%d-a.example", i),
					fmt.Sprintf("mcp://svc-%d-b.example", i),
				},
				AllowedTargets: []string{
					fmt.Sprintf("group:team-%d", i),
				},
			})
			bindings = append(bindings, &api.PolicyBinding{
				Role:    name,
				Members: []string{"node:" + nodePeer.String()},
			})
		}
		req := &api.PolicyConfigUpdateRequest{Roles: roles, Bindings: bindings}

		start := time.Now()
		if err := validatePolicyConfig(req); err != nil {
			t.Fatalf("validatePolicyConfig failed at n=%d: %v", n, err)
		}
		validateDur := time.Since(start)

		claims := jwt.MapClaims{}
		start = time.Now()
		resolved := resolveRoles(nodePeer.String(), claims, bindings)
		resolveDur := time.Since(start)
		if len(resolved) != n {
			t.Fatalf("expected %d resolved roles, got %d", n, len(resolved))
		}

		start = time.Now()
		biscuitBytes, _, err := identity.MintBiscuitToken(priv, claims, nil, nodePeer, time.Now().Add(time.Hour), roleNames, roles)
		mintDur := time.Since(start)
		if err != nil {
			t.Fatalf("mint failed at n=%d: %v", n, err)
		}

		start = time.Now()
		_, authErr := identity.VerifyBiscuit(biscuitBytes, nodePeer, []ed25519.PublicKey{pub}, 5*time.Second)
		authorizeDur := time.Since(start)

		if authErr != nil {
			if !strings.Contains(authErr.Error(), datalog.ErrWorldRunLimitMaxFacts.Error()) {
				t.Fatalf("authorize failed at n=%d for an unexpected reason: %v", n, authErr)
			}
			t.Logf("roles=%-6d validate=%-12s resolve=%-12s mint=%-12s biscuit_size=%-10dB authorize: hit current limit (%v) - stopping",
				n, validateDur, resolveDur, mintDur, len(biscuitBytes), authErr)
			break
		}

		total := validateDur + resolveDur + mintDur + authorizeDur
		t.Logf("roles=%-6d validate=%-12s resolve=%-12s mint=%-12s biscuit_size=%-10dB authorize=%-12s total=%s",
			n, validateDur, resolveDur, mintDur, len(biscuitBytes), authorizeDur, total)
	}
}
