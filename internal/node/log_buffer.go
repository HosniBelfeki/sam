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

var globalLogBuffer = &RingBufferSink{
	buffer: ring.New(500), // Keep last 500 lines
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
	s.buffer.Value = line
	s.buffer = s.buffer.Next()

	return len(p), nil
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
