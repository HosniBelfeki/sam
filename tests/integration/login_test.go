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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSamNodeJoin(t *testing.T) {
	nodeBin := buildBinary(t, "./cmd/sam-node")
	tmpHome := t.TempDir()
	env := append(os.Environ(),
		"HOME="+tmpHome,
		"XDG_CONFIG_HOME="+filepath.Join(tmpHome, ".config"),
		"BROWSER=echo",
	)

	// Mock OIDC server for device flow
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]interface{}{
			"issuer":                 "http://" + r.Host,
			"token_endpoint":         "http://" + r.Host + "/token",
			"authorization_endpoint": "http://" + r.Host + "/auth",
		}); err != nil {
			t.Errorf("Failed to encode response: %v", err)
		}
	})
	mux.HandleFunc("/device/code", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]interface{}{
			"device_code":               "dev_code_123",
			"user_code":                 "ABCD-1234",
			"verification_uri":          "http://example.com/verify",
			"verification_uri_complete": "http://example.com/verify?code=ABCD-1234",
			"expires_in":                60,
			"interval":                  1,
		}); err != nil {
			t.Errorf("Failed to encode response: %v", err)
		}
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]string{
			"access_token": "test-jwt-token",
			"id_token":     "test-jwt-token",
		}); err != nil {
			t.Errorf("Failed to encode response: %v", err)
		}
	})
	oidcServer := httptest.NewServer(mux)
	defer oidcServer.Close()

	// Start mock libp2p router that knows about our mock OIDC server
	_, routerAddr := startMockRouterWithOIDC(t, oidcServer.URL)

	stdout, stderr, err := runCommandWithCallback(
		t,
		repoRoot(t),
		5*time.Second,
		env,
		"", // No stdin needed
		nodeBin,
		"join",
		routerAddr,
	)
	if err != nil {
		t.Fatalf("join command failed: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}

	out := stdout + stderr
	if !strings.Contains(out, "Successfully joined the Sovereign Agent Mesh!") {
		t.Fatalf("join did not succeed:\n%s", out)
	}

	// Verify that the identity is stored and node can run
	stdout, stderr, err = runCommand(
		t,
		repoRoot(t),
		3*time.Second,
		env,
		"",
		nodeBin,
		"run", "--control-plane", routerAddr,
		"--listen", "/ip4/127.0.0.1/udp/0/quic-v1",
		"--listen", "/ip4/127.0.0.1/tcp/0",
		"--bind-addr", "127.0.0.1:0",
		"--api-token-path", tokenPath(t, "dummy-token"),
	)
	if err != context.DeadlineExceeded {
		t.Fatalf("expected run command to keep running, got: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}

	out = stdout + stderr
	if !strings.Contains(out, "Using stored identity.") {
		t.Fatalf("node did not use stored identity:\n%s", out)
	}
}

// TestSamNodeRunJoinNonInteractive covers the safe fallback: since the test
// harness gives the child process no TTY (stdin defaults to /dev/null),
// "run --join" must not attempt (and block on) an interactive OIDC login; it
// should fall back to the same unauthenticated MCP sidecar as plain "run".
func TestSamNodeRunJoinNonInteractive(t *testing.T) {
	nodeBin := buildBinary(t, "./cmd/sam-node")
	tmpHome := t.TempDir()
	env := []string{
		"HOME=" + tmpHome,
		"XDG_CONFIG_HOME=" + filepath.Join(tmpHome, ".config"),
	}

	_, routerAddr := startMockRouter(t)

	stdout, stderr, err := runCommand(
		t,
		repoRoot(t),
		3*time.Second,
		env,
		"", // no stdin: child sees no TTY, same as a container without -it
		nodeBin,
		"run", "--join", "--control-plane", routerAddr,
		"--listen", "/ip4/127.0.0.1/udp/0/quic-v1",
		"--listen", "/ip4/127.0.0.1/tcp/0",
		"--bind-addr", "127.0.0.1:0",
	)
	if err != context.DeadlineExceeded {
		t.Fatalf("expected run --join to keep running via the unauthenticated sidecar, got: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}

	out := stdout + stderr
	if !strings.Contains(out, "no interactive terminal available") {
		t.Fatalf("expected --join to warn about falling back without a TTY:\n%s", out)
	}
	if !strings.Contains(out, "Starting unauthenticated sidecar for enrollment over MCP") {
		t.Fatalf("expected the unauthenticated sidecar to start:\n%s", out)
	}
}

// TestSamNodeRunControlPlaneMismatch covers a node enrolled with one control
// plane being pointed at a different one: it must fail loudly (never
// silently keep using the originally-enrolled mesh), with or without
// --join, and "reset" must clear the way for a fresh enrollment elsewhere.
func TestSamNodeRunControlPlaneMismatch(t *testing.T) {
	nodeBin := buildBinary(t, "./cmd/sam-node")
	tmpHome := t.TempDir()
	env := []string{
		"HOME=" + tmpHome,
		"XDG_CONFIG_HOME=" + filepath.Join(tmpHome, ".config"),
	}

	_, routerA := startMockRouter(t)
	_, routerB := startMockRouter(t)

	// Enroll against control plane A.
	stdout, stderr, err := runCommand(
		t, repoRoot(t), 3*time.Second, env, "",
		nodeBin, "run", "--control-plane", routerA, "--jwt", "test-jwt",
		"--listen", "/ip4/127.0.0.1/udp/0/quic-v1",
		"--listen", "/ip4/127.0.0.1/tcp/0",
		"--bind-addr", "127.0.0.1:0",
		"--api-token-path", tokenPath(t, "dummy-token"),
	)
	if err != context.DeadlineExceeded {
		t.Fatalf("expected initial enroll+run against A to keep running, got: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}

	// Pointing the same store at B, without --join, must fail rather than
	// silently keep talking to A.
	stdout, stderr, err = runCommand(
		t, repoRoot(t), 5*time.Second, env, "",
		nodeBin, "run", "--control-plane", routerB,
		"--listen", "/ip4/127.0.0.1/udp/0/quic-v1",
		"--listen", "/ip4/127.0.0.1/tcp/0",
		"--bind-addr", "127.0.0.1:0",
		"--api-token-path", tokenPath(t, "dummy-token"),
	)
	if err == nil || err == context.DeadlineExceeded {
		t.Fatalf("expected run against a mismatched control plane to fail fast, got: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	out := stdout + stderr
	if !strings.Contains(out, "does not match the mesh") {
		t.Fatalf("expected a control-plane mismatch error, got:\n%s", out)
	}

	// Same mismatch with --join, but no TTY to confirm the switch: must also
	// fail fast rather than silently ignore --join or block indefinitely.
	stdout, stderr, err = runCommand(
		t, repoRoot(t), 5*time.Second, env, "",
		nodeBin, "run", "--join", "--control-plane", routerB,
		"--listen", "/ip4/127.0.0.1/udp/0/quic-v1",
		"--listen", "/ip4/127.0.0.1/tcp/0",
		"--bind-addr", "127.0.0.1:0",
		"--api-token-path", tokenPath(t, "dummy-token"),
	)
	if err == nil || err == context.DeadlineExceeded {
		t.Fatalf("expected run --join against a mismatched control plane to fail fast without a TTY, got: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	out = stdout + stderr
	if !strings.Contains(out, "does not match the mesh") {
		t.Fatalf("expected a control-plane mismatch error, got:\n%s", out)
	}

	// "reset" clears the way for a fresh enrollment against B.
	stdout, stderr, err = runCommand(t, repoRoot(t), 3*time.Second, env, "", nodeBin, "reset")
	if err != nil {
		t.Fatalf("reset failed: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	if !strings.Contains(stdout+stderr, "Cleared stored mesh identity") {
		t.Fatalf("expected reset confirmation, got:\n%s", stdout+stderr)
	}

	stdout, stderr, err = runCommand(
		t, repoRoot(t), 3*time.Second, env, "",
		nodeBin, "run", "--control-plane", routerB, "--jwt", "test-jwt",
		"--listen", "/ip4/127.0.0.1/udp/0/quic-v1",
		"--listen", "/ip4/127.0.0.1/tcp/0",
		"--bind-addr", "127.0.0.1:0",
		"--api-token-path", tokenPath(t, "dummy-token"),
	)
	if err != context.DeadlineExceeded {
		t.Fatalf("expected re-enroll against B after reset to keep running, got: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
}

