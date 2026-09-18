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
	"sync/atomic"
	"testing"
	"time"
)

// The router lands in the routing table after an asynchronous check that may
// finish after Start returns; discovery must not advertise into an empty table.
func TestAwaitRoutingTableReturnsOncePopulated(t *testing.T) {
	var size atomic.Int32
	time.AfterFunc(250*time.Millisecond, func() { size.Store(1) })

	start := time.Now()
	awaitRoutingTable(context.Background(), func() int { return int(size.Load()) }, 5*time.Second)
	if waited := time.Since(start); waited < 250*time.Millisecond || waited > 2*time.Second {
		t.Fatalf("returned after %s, want shortly after the table got its peer at 250ms", waited)
	}
}

func TestAwaitRoutingTableDoesNotWaitWhenPopulated(t *testing.T) {
	start := time.Now()
	awaitRoutingTable(context.Background(), func() int { return 1 }, 5*time.Second)
	if waited := time.Since(start); waited > 50*time.Millisecond {
		t.Fatalf("waited %s on an already populated table", waited)
	}
}

// With no router ever reachable, discovery must still start: the bound keeps
// the previous behaviour instead of blocking forever.
func TestAwaitRoutingTableGivesUpAtBound(t *testing.T) {
	start := time.Now()
	awaitRoutingTable(context.Background(), func() int { return 0 }, 200*time.Millisecond)
	if waited := time.Since(start); waited < 200*time.Millisecond || waited > 2*time.Second {
		t.Fatalf("returned after %s, want about the 200ms bound", waited)
	}
}

func TestAwaitRoutingTableStopsWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	done := make(chan struct{})
	go func() {
		awaitRoutingTable(ctx, func() int { return 0 }, time.Hour)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("did not return after the context was cancelled")
	}
}
