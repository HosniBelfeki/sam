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
	"fmt"
	"net/url"
	"strings"
	"sync"

	"go.uber.org/zap"
)

// RingBufferSink implements zap.Sink to capture logs in memory.
type RingBufferSink struct {
	mu     sync.Mutex
	buffer *ring.Ring
}

const (
	logBufferLines = 500
	// maxLogLineBytes bounds each retained line so the ring's memory is
	// bounded too (logBufferLines * maxLogLineBytes), whatever a remote peer
	// manages to get logged.
	maxLogLineBytes = 4 << 10
	// maxRemoteLogBytes is how much of a remote-controlled string a log
	// message may carry; see truncateForLog.
	maxRemoteLogBytes = 256
)

var globalLogBuffer = &RingBufferSink{
	buffer: ring.New(logBufferLines),
}

func init() {
	_ = zap.RegisterSink("ringbuffer", func(u *url.URL) (zap.Sink, error) {
		return globalLogBuffer, nil
	})
}

// Write implements io.Writer
func (s *RingBufferSink) Write(p []byte) (n int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// zap writes complete log lines per Write call
	line := strings.TrimSuffix(string(p), "\n")
	if len(line) > maxLogLineBytes {
		line = line[:maxLogLineBytes] + "…[truncated]"
	}
	s.buffer.Value = line
	s.buffer = s.buffer.Next()

	return len(p), nil
}

// truncateForLog bounds a string that a remote peer chose (a request path, a
// tool result, a target name) before it reaches the logs.
func truncateForLog(s string) string {
	if len(s) <= maxRemoteLogBytes {
		return s
	}
	return fmt.Sprintf("%s…(+%d bytes)", s[:maxRemoteLogBytes], len(s)-maxRemoteLogBytes)
}

// Sync implements zap.Sink
func (s *RingBufferSink) Sync() error {
	return nil
}

// Close implements zap.Sink
func (s *RingBufferSink) Close() error {
	return nil
}

// GetRecentLogs returns the most recent log lines.
func GetRecentLogs() []string {
	globalLogBuffer.mu.Lock()
	defer globalLogBuffer.mu.Unlock()

	logs := []string{}
	globalLogBuffer.buffer.Do(func(p any) {
		if p != nil {
			logs = append(logs, p.(string))
		}
	})
	return logs
}
