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

package api

import (
	"testing"

	"github.com/biscuit-auth/biscuit-go/v2"
)

func TestValidateAgentPattern(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		wantErr bool
	}{
		{name: "exact id", pattern: "reviewer-7.prod.acme.example"},
		{name: "with the agent prefix", pattern: "agent:reviewer-7.prod.acme.example"},
		{name: "suffix wildcard", pattern: "*.prod.acme.example"},
		{name: "prefix wildcard", pattern: "acme.*"},
		{name: "bare wildcard grants everything", pattern: "*"},

		{name: "empty", pattern: "", wantErr: true},
		{name: "wildcard alone beside a dot", pattern: "*.", wantErr: true},
		{name: "wildcard in the middle", pattern: "reviewer.*.acme.example", wantErr: true},
		{name: "two wildcards", pattern: "*.acme.*", wantErr: true},
		{name: "uppercase", pattern: "*.PROD.acme.example", wantErr: true},
		{name: "empty label", pattern: "*..acme.example", wantErr: true},
		// An unqualified id has no authority, so it would collide across tenants.
		{name: "exact id with no authority", pattern: "reviewer-7", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateAgentPattern(tt.pattern)
			if tt.wantErr && err == nil {
				t.Errorf("ValidateAgentPattern(%q) = nil, want an error", tt.pattern)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("ValidateAgentPattern(%q) = %v, want nil", tt.pattern, err)
			}
		})
	}
}

// TestBuildAgentDatalogFactKeepsTheWildcardAnchored pins the property the whole
// namespace bound rests on: a suffix grant keeps its leading dot, so
// "evil-prod.acme.example" is a different namespace from "*.prod.acme.example"
// rather than a match for it.
func TestBuildAgentDatalogFactKeepsTheWildcardAnchored(t *testing.T) {
	tests := []struct {
		pattern  string
		wantName string
		wantVal  string
	}{
		{"*.prod.acme.example", FactGrantedAgentSuffix, ".prod.acme.example"},
		{"acme.*", FactGrantedAgentPrefix, "acme."},
		{"reviewer-7.prod.acme.example", FactGrantedAgentExact, "reviewer-7.prod.acme.example"},
	}

	for _, tt := range tests {
		t.Run(tt.pattern, func(t *testing.T) {
			fact := BuildAgentDatalogFact(tt.pattern)
			if fact.Name != tt.wantName {
				t.Fatalf("BuildAgentDatalogFact(%q) name = %q, want %q", tt.pattern, fact.Name, tt.wantName)
			}
			got, ok := fact.IDs[0].(biscuit.String)
			if !ok || string(got) != tt.wantVal {
				t.Errorf("BuildAgentDatalogFact(%q) value = %v, want %q", tt.pattern, fact.IDs[0], tt.wantVal)
			}
		})
	}

	if got := BuildAgentDatalogFact("*").Name; got != FactGrantedAgentAll {
		t.Errorf(`BuildAgentDatalogFact("*") name = %q, want %q`, got, FactGrantedAgentAll)
	}
}

// TestBuildAgentDatalogFactsMergesExactGrants keeps a role naming many agents
// from costing one world fact each, the same way service and target grants are
// merged: the authorizer rejects worlds beyond ~1000 facts.
func TestBuildAgentDatalogFactsMergesExactGrants(t *testing.T) {
	facts := BuildAgentDatalogFacts([]string{
		"a.acme.example",
		"b.acme.example",
		"c.acme.example",
		"*.prod.acme.example",
	})

	var sets, suffixes int
	for _, f := range facts {
		switch f.Name {
		case FactGrantedAgentSet:
			sets++
			set, ok := f.IDs[0].(biscuit.Set)
			if !ok {
				t.Fatalf("granted_agent_set term is %T, want biscuit.Set", f.IDs[0])
			}
			if len(set) != 3 {
				t.Errorf("granted_agent_set holds %d ids, want 3", len(set))
			}
		case FactGrantedAgentSuffix:
			suffixes++
		default:
			t.Errorf("unexpected fact %q", f.Name)
		}
	}
	if sets != 1 || suffixes != 1 {
		t.Errorf("got %d set facts and %d suffix facts, want 1 and 1", sets, suffixes)
	}
}
