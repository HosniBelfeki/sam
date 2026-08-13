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
	"testing"

	"github.com/google/sam/internal/node"
)

func TestParseLabelsFlag(t *testing.T) {
	if got, err := parseLabelsFlag(""); got != nil || err != nil {
		t.Errorf("empty flag: got %v, %v; want nil, nil", got, err)
	}

	got, err := parseLabelsFlag(" region=eu , team=platform ,,")
	if err != nil || len(got) != 2 || got["region"] != "eu" || got["team"] != "platform" {
		t.Errorf("parse should split key=value pairs: got %v, %v", got, err)
	}

	if _, err := parseLabelsFlag("noequals"); err == nil {
		t.Error("entry without '=' must be rejected")
	}

	if _, err := parseLabelsFlag("region=us-east-1,region=us-west-1"); err == nil {
		t.Error("duplicate label key must be rejected")
	}
}

func TestNormalizeControlPlaneURL(t *testing.T) {
	cases := map[string]string{
		"bananas.sam-mesh.dev":          "https://bananas.sam-mesh.dev",
		"http://localhost:8080":         "http://localhost:8080",
		"https://bananas.sam-mesh.dev/": "https://bananas.sam-mesh.dev",
	}
	for in, want := range cases {
		if got := normalizeControlPlaneURL(in); got != want {
			t.Errorf("normalizeControlPlaneURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDefaultControlPlane(t *testing.T) {
	store, err := node.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	if got := defaultControlPlane(store, "https://example.com"); got != "https://example.com" {
		t.Errorf("explicit control plane should win: got %q", got)
	}
	if got := defaultControlPlane(store, ""); got != "https://bananas.sam-mesh.dev" {
		t.Errorf("no explicit and no stored URL should default to the testnet: got %q", got)
	}

	if err := store.SaveControlPlaneURL("https://stored.example.com"); err != nil {
		t.Fatalf("SaveControlPlaneURL: %v", err)
	}
	if got := defaultControlPlane(store, ""); got != "https://stored.example.com" {
		t.Errorf("no explicit should fall back to the stored URL: got %q", got)
	}
}
