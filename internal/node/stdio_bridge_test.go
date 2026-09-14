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
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newPipeBridge returns a StdioBridge wired to two in-memory pipes so tests
// can drive stdin/stdout without a real subprocess.
func newPipeBridge() (*StdioBridge, *io.PipeWriter, *bytes.Buffer) {
	stdoutReader, stdoutWriter := io.Pipe()
	stdinBuf := &bytes.Buffer{}
	b := &StdioBridge{
		stdin:  nopWriteCloser{stdinBuf},
		stdout: stdoutReader,
	}
	b.Start()
	return b, stdoutWriter, stdinBuf
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// waitFor polls cond until it holds; fails the test after 2s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// writeSignalRecorder is a ResponseRecorder that reports each body Write on
// wrote, so a test can tell the handler has emitted a line without reading
// rec.Body while the handler goroutine is still writing to it.
type writeSignalRecorder struct {
	*httptest.ResponseRecorder
	wrote chan struct{}
}

func (r *writeSignalRecorder) Write(p []byte) (int, error) {
	n, err := r.ResponseRecorder.Write(p)
	select {
	case r.wrote <- struct{}{}:
	default:
	}
	return n, err
}

func TestStdioBridge_ServeHTTP_GETStreamsBroadcastLines(t *testing.T) {
	b, stdoutWriter, _ := newPipeBridge()
	defer func() { _ = stdoutWriter.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	rec := &writeSignalRecorder{ResponseRecorder: httptest.NewRecorder(), wrote: make(chan struct{}, 1)}
	done := make(chan struct{})
	go func() {
		b.ServeHTTP(rec, req)
		close(done)
	}()

	waitFor(t, "SSE client registration", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return len(b.clients) > 0
	})
	_, _ = stdoutWriter.Write([]byte("hello\n"))
	select {
	case <-rec.wrote:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the SSE line to be written")
	}
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeHTTP did not return after ctx was cancelled")
	}

	if got := rec.Body.String(); !strings.Contains(got, "data: hello\n\n") {
		t.Fatalf("SSE body = %q, want it to contain %q", got, "data: hello\n\n")
	}
}

func TestStdioBridge_ServeHTTP_POSTNotificationReturnsAccepted(t *testing.T) {
	b, stdoutWriter, stdinBuf := newPipeBridge()
	defer func() { _ = stdoutWriter.Close() }()

	body := `{"jsonrpc":"2.0","method":"notify"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	if got := stdinBuf.String(); got != body+"\n" {
		t.Fatalf("stdin got %q, want %q", got, body+"\n")
	}
}

func TestStdioBridge_ServeHTTP_POSTCallWaitsForMatchingReply(t *testing.T) {
	b, stdoutWriter, _ := newPipeBridge()
	defer func() { _ = stdoutWriter.Close() }()

	body := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		b.ServeHTTP(rec, req)
		close(done)
	}()

	waitFor(t, "call registration", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return len(b.calls) > 0
	})
	reply := `{"jsonrpc":"2.0","id":1,"result":{}}`
	_, _ = stdoutWriter.Write([]byte(reply + "\n"))

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeHTTP did not return after the matching reply arrived")
	}

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Body.String(); got != reply {
		t.Fatalf("body = %q, want %q", got, reply)
	}
}
