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
	"container/ring"
	"strings"
	"testing"
)

// The ring keeps whole lines; without a per-line cap a remote peer that gets
// an 8 MiB string logged pins it in memory for 500 rounds.
func TestRingBufferSinkCapsLineBytes(t *testing.T) {
	sink := &RingBufferSink{buffer: ring.New(4)}
	huge := strings.Repeat("A", 3*maxLogLineBytes) + "\n"
	if _, err := sink.Write([]byte(huge)); err != nil {
		t.Fatal(err)
	}
	_, _ = sink.Write([]byte("short\n"))

	var lines []string
	sink.buffer.Do(func(p any) {
		if p != nil {
			lines = append(lines, p.(string))
		}
	})
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	if got := len(lines[0]); got > maxLogLineBytes+len("…[truncated]") {
		t.Errorf("stored line is %d bytes, want <= %d", got, maxLogLineBytes+len("…[truncated]"))
	}
	if !strings.HasSuffix(lines[0], "[truncated]") {
		t.Errorf("truncated line should be marked, got suffix %q", lines[0][len(lines[0])-16:])
	}
	if lines[1] != "short" {
		t.Errorf("second line = %q, want %q", lines[1], "short")
	}
}

func TestTruncateForLog(t *testing.T) {
	if got := truncateForLog("small"); got != "small" {
		t.Errorf("short strings must pass through, got %q", got)
	}
	long := strings.Repeat("x", maxRemoteLogBytes+1000)
	got := truncateForLog(long)
	if !strings.HasPrefix(got, long[:maxRemoteLogBytes]) || !strings.HasSuffix(got, "(+1000 bytes)") {
		t.Errorf("unexpected truncation: %q", got[len(got)-40:])
	}
	if len(got) > maxRemoteLogBytes+32 {
		t.Errorf("truncated string is %d bytes", len(got))
	}
}
