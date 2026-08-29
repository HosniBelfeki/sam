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

package main

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestIngressSocketIsOwnerOnly pins the permissions on the one hole punched
// through the sandbox boundary. The ingress socket relays "CONNECT <port>" to
// any port inside the namespace with no token and no capability of its own, so
// its mode is the only thing standing between a neighbouring process and the
// agent. Both sibling sockets in this repo are 0600 for the same reason; the
// umask that would otherwise decide this is not ours to assume.
func TestIngressSocketIsOwnerOnly(t *testing.T) {
	// Loosen the umask so a missing chmod really would leave the socket group-
	// and world-accessible, rather than being masked into passing.
	old := syscall.Umask(0)
	defer syscall.Umask(old)

	socketPath := filepath.Join(t.TempDir(), "ingress.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	served := make(chan error, 1)
	go func() { served <- serveIngress(ctx, socketPath) }()

	deadline := time.Now().Add(2 * time.Second)
	var info os.FileInfo
	for {
		var err error
		if info, err = os.Stat(socketPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ingress socket %s never appeared: %v", socketPath, err)
		}
		time.Sleep(5 * time.Millisecond)
	}

	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("ingress socket permissions are %#o, want 0600", perm)
	}

	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("serveIngress returned %v, want nil on cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("serveIngress did not return after cancellation")
	}
}
