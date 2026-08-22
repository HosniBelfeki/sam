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
	"strings"
	"testing"
)

func TestIsolationError(t *testing.T) {
	for _, tc := range []struct {
		name    string
		links   []string
		wantErr bool
		// names is what the message must mention, so an operator is told which
		// interface is the problem rather than only that there is one.
		names []string
	}{
		{
			name:  "a microVM with no network device",
			links: []string{"lo"},
		},
		{
			name:  "a container run with --network none",
			links: []string{"lo"},
		},
		{
			name:  "our own tun, from an earlier run in this namespace",
			links: []string{"lo", tunName},
		},
		{
			name:    "a Kubernetes pod, where the namespace is shared",
			links:   []string{"lo", "eth0"},
			wantErr: true,
			names:   []string{"eth0"},
		},
		{
			name:    "a docker run that forgot --network none",
			links:   []string{"lo", "eth0", tunName},
			wantErr: true,
			names:   []string{"eth0"},
		},
		{
			name:    "several ways out are all reported",
			links:   []string{"lo", "eth0", "vlan7"},
			wantErr: true,
			names:   []string{"eth0", "vlan7"},
		},
		{
			// A namespace with nothing at all is not one we built, but it has
			// no way out either, which is the only property being asserted.
			name:  "an empty namespace",
			links: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := isolationError(tc.links)
			if tc.wantErr && err == nil {
				t.Fatalf("isolationError(%q) = nil, want an error: an agent here is not confined", tc.links)
			}
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("isolationError(%q) = %v, want nil", tc.links, err)
				}
				return
			}
			for _, name := range tc.names {
				if !strings.Contains(err.Error(), name) {
					t.Errorf("error does not name %q, so it does not say what to fix: %v", name, err)
				}
			}
		})
	}
}
