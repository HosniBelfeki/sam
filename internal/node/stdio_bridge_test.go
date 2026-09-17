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
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// newPipeBridge returns a StdioBridge wired to two in-memory pipes so tests
// can drive stdin/stdout without a real subprocess.
func newPipeBridge() (*StdioBridge, *io.PipeWriter, *syncBuffer) {
	stdoutReader, stdoutWriter := io.Pipe()
	stdinBuf := &syncBuffer{}
	b := &StdioBridge{
		stdin:  stdinBuf,
		stdout: stdoutReader,
	}
	b.Start()
	return b, stdoutWriter, stdinBuf
}

// syncBuffer is the fake backend's stdin: written by request goroutines,
// read by the test.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) Close() error { return nil }

func (s *syncBuffer) lines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	text := strings.TrimSpace(s.buf.String())
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

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

// GET was the legacy SSE stream: every backend line to every reader, i.e.
// every caller's tool output to every other authorized caller. Refused.
func TestStdioBridge_ServeHTTP_GETIsRefused(t *testing.T) {
	b, stdoutWriter, _ := newPipeBridge()
	defer func() { _ = stdoutWriter.Close() }()

	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", rec.Code)
	}
}

// bridgeIDOf returns the id the bridge assigned to the most recent request it
// wrote to the backend's stdin, so a test can answer as the backend would.
// bridgeIDOf returns the bridge-assigned id of the line-th request (1-based)
// the backend received, waiting for it to arrive.
func bridgeIDOf(t *testing.T, stdinBuf *syncBuffer, line int) string {
	t.Helper()
	waitFor(t, "request to reach the backend", func() bool { return len(stdinBuf.lines()) >= line })
	var msg map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdinBuf.lines()[line-1]), &msg); err != nil {
		t.Fatalf("stdin line is not JSON: %v", err)
	}
	return string(msg["id"])
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
	if got := stdinBuf.lines(); len(got) != 1 || got[0] != body {
		t.Fatalf("stdin got %q, want [%q]", got, body)
	}
}

func TestStdioBridge_ServeHTTP_POSTCallWaitsForMatchingReply(t *testing.T) {
	b, stdoutWriter, stdinBuf := newPipeBridge()
	defer func() { _ = stdoutWriter.Close() }()

	body := `{"jsonrpc":"2.0","id":"caller-7","method":"ping"}`
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
	// The backend never sees the caller's id, only the bridge's.
	bridgeID := bridgeIDOf(t, stdinBuf, 1)
	if bridgeID == `"caller-7"` {
		t.Fatal("caller id reached the backend unrewritten")
	}
	_, _ = stdoutWriter.Write([]byte(`{"jsonrpc":"2.0","id":` + bridgeID + `,"result":{}}` + "\n"))

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeHTTP did not return after the matching reply arrived")
	}

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if string(got["id"]) != `"caller-7"` {
		t.Fatalf("reply id = %s, want the caller's own \"caller-7\"", got["id"])
	}
}

// A subprocess reads stdin and writes stdout on one thread. When it is
// blocked writing a large reply, it is not reading, so a caller's write to
// stdin blocks once the pipe is full. If that writer held the state lock,
// deliver could not drain stdout, the backend could never finish writing,
// and the two would wait on each other forever: a blocked stdin write must
// not stop other callers' replies from being delivered.
func TestStdioBridge_BlockedStdinWriteDoesNotStallDelivery(t *testing.T) {
	stdoutReader, stdoutWriter := io.Pipe()
	// A backend that is not reading stdin: the write blocks until someone
	// reads the other end, which nobody does until the end of the test.
	stdinReader, stdinWriter := io.Pipe()
	// entered fires as a write to stdin begins; the test must not touch b.mu
	// to learn that, or it would itself hang on the bug it is looking for.
	entered := make(chan struct{}, 8)
	b := &StdioBridge{stdin: signalingWriter{w: stdinWriter, entered: entered}, stdout: stdoutReader}
	b.Start()
	defer func() { _ = stdoutWriter.Close(); _ = stdinReader.Close() }()

	// Caller A: its request is written and it waits for a reply. Its stdin
	// write completes because we read exactly that line.
	recA := httptest.NewRecorder()
	doneA := make(chan struct{})
	go func() {
		b.ServeHTTP(recA, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"jsonrpc":"2.0","id":"a","method":"ping"}`)))
		close(doneA)
	}()
	<-entered
	lineA := make([]byte, 256)
	nA, err := stdinReader.Read(lineA)
	if err != nil {
		t.Fatal(err)
	}
	var msgA map[string]json.RawMessage
	if err := json.Unmarshal(lineA[:nA], &msgA); err != nil {
		t.Fatalf("stdin line is not JSON: %v", err)
	}
	bridgeIDA := string(msgA["id"])

	// Caller B: registered, then blocked in the stdin write because the
	// backend has stopped reading.
	recB := httptest.NewRecorder()
	go b.ServeHTTP(recB, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"jsonrpc":"2.0","id":"b","method":"ping"}`)))
	<-entered

	// The backend now answers A. With the state lock held by B's blocked
	// write, this delivery would never happen.
	if _, err := stdoutWriter.Write([]byte(`{"jsonrpc":"2.0","id":` + bridgeIDA + `,"result":{}}` + "\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-doneA:
	case <-time.After(2 * time.Second):
		t.Fatal("caller A's reply was not delivered while another caller was blocked writing to stdin")
	}
	if recA.Code != http.StatusOK {
		t.Fatalf("caller A status = %d, want 200", recA.Code)
	}
}

type signalingWriter struct {
	w       io.WriteCloser
	entered chan struct{}
}

func (s signalingWriter) Write(p []byte) (int, error) {
	s.entered <- struct{}{}
	return s.w.Write(p)
}

func (s signalingWriter) Close() error { return s.w.Close() }

// M18: with one backend process behind every authorized caller, two callers
// using the same JSON-RPC id used to collide in the bridge's routing table,
// and one caller's reply was handed to the other. Each reply goes to the
// request it answers, and a line with no owner goes to nobody.
func TestStdioBridge_ServeHTTP_SameIDFromTwoCallersDoesNotCrossWires(t *testing.T) {
	b, stdoutWriter, stdinBuf := newPipeBridge()
	defer func() { _ = stdoutWriter.Close() }()

	type result struct {
		rec  *httptest.ResponseRecorder
		done chan struct{}
	}
	start := func(method string) result {
		r := result{rec: httptest.NewRecorder(), done: make(chan struct{})}
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"`+method+`"}`))
		go func() {
			b.ServeHTTP(r.rec, req)
			close(r.done)
		}()
		return r
	}
	a := start("secret-for-a")
	waitFor(t, "first call registered", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return len(b.calls) == 1
	})
	idA := bridgeIDOf(t, stdinBuf, 1)
	bb := start("secret-for-b")
	waitFor(t, "second call registered", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return len(b.calls) == 2
	})
	idB := bridgeIDOf(t, stdinBuf, 2)
	if idA == idB {
		t.Fatalf("both callers got bridge id %s", idA)
	}

	// A backend notification has no owner: nobody receives it.
	_, _ = stdoutWriter.Write([]byte(`{"jsonrpc":"2.0","method":"notifications/progress"}` + "\n"))
	// Answer B first, then A.
	_, _ = stdoutWriter.Write([]byte(`{"jsonrpc":"2.0","id":` + idB + `,"result":"for-b"}` + "\n"))
	_, _ = stdoutWriter.Write([]byte(`{"jsonrpc":"2.0","id":` + idA + `,"result":"for-a"}` + "\n"))

	for name, r := range map[string]result{"a": a, "b": bb} {
		select {
		case <-r.done:
		case <-time.After(2 * time.Second):
			t.Fatalf("caller %s never got its reply", name)
		}
		if r.rec.Code != http.StatusOK {
			t.Fatalf("caller %s: status %d", name, r.rec.Code)
		}
		if !strings.Contains(r.rec.Body.String(), `"for-`+name+`"`) {
			t.Errorf("caller %s received %s", name, r.rec.Body.String())
		}
		if !strings.Contains(r.rec.Body.String(), `"id":1`) {
			t.Errorf("caller %s: id not restored: %s", name, r.rec.Body.String())
		}
	}
}

// A single backend line larger than bufio.Scanner's 64 KiB default used to
// stop the reader for good; results up to the request-body cap must flow.
func TestStdioBridge_ServeHTTP_LargeReplyIsDelivered(t *testing.T) {
	b, stdoutWriter, stdinBuf := newPipeBridge()
	defer func() { _ = stdoutWriter.Close() }()

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"jsonrpc":"2.0","id":7,"method":"big"}`))
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

	payload := strings.Repeat("x", 100<<10)
	_, _ = stdoutWriter.Write([]byte(`{"jsonrpc":"2.0","id":` + bridgeIDOf(t, stdinBuf, 1) + `,"result":"` + payload + `"}` + "\n"))

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeHTTP did not return after a >64 KiB reply")
	}
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), payload) || !strings.Contains(rec.Body.String(), `"id":7`) {
		t.Fatalf("status %d, body len %d; want 200 with the %d-byte payload and the caller's id", rec.Code, rec.Body.Len(), len(payload))
	}
}

// Once the backend's stdout is gone the bridge cannot answer anyone: callers
// get a 503 immediately rather than hanging until their own deadline.
func TestStdioBridge_ServeHTTP_RefusesAfterBackendExit(t *testing.T) {
	b, stdoutWriter, _ := newPipeBridge()

	// In-flight call sees the backend go away.
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
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
	_ = stdoutWriter.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight call hung after the backend closed stdout")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("in-flight status = %d, want 503", rec.Code)
	}

	// Later callers are refused up front.
	waitFor(t, "bridge to mark itself closed", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.closed
	})
	rec = httptest.NewRecorder()
	b.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"ping"}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("POST after exit: status = %d, want 503", rec.Code)
	}
}
