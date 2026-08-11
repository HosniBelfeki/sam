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

import "testing"

func TestValidateRegion(t *testing.T) {
	valid := []string{
		"EU", "eu", " Eu ", // continent, any case
		"EU-DE", "NA-US", "AS-JP", "EU-RU", "AS-TR", // countries in their continent
		"EU-DE-BY", "NA-US-CA", "NA-US-123", // zone extension, syntax only
	}
	for _, s := range valid {
		if err := ValidateRegion(s); err != nil {
			t.Errorf("ValidateRegion(%q): unexpected error: %v", s, err)
		}
	}

	invalid := []string{
		"",                 // absent is the caller's decision, not a valid claim
		"MARS", "X", "EUR", // not a continent code
		"EU-XX",          // not an ISO 3166-1 country
		"EU-US", "NA-DE", // country outside its continent
		"US",             // country without its continent prefix
		"EU-DE-TOOLONG",  // zone syntax violation
		"EU-DE-BY-1",     // too many segments
		"EU_DE", "EU DE", // wrong separator
	}
	for _, s := range invalid {
		if err := ValidateRegion(s); err == nil {
			t.Errorf("ValidateRegion(%q): expected error, got nil", s)
		}
	}
}

func TestRegionMatches(t *testing.T) {
	tests := []struct {
		required, claimed string
		want              bool
	}{
		{"EU", "EU", true},
		{"EU", "EU-DE", true},       // finer claim satisfies coarser requirement
		{"EU", "EU-DE-BY", true},    // transitively
		{"EU-DE", "EU-DE-BY", true}, // zone within required country
		{"EU-DE", "EU", false},      // coarser claim never satisfies finer requirement
		{"EU-DE", "EU-FR", false},
		{"EU", "NA", false},
		{"EU", "", false}, // unlabeled never matches
	}
	for _, tt := range tests {
		if got := RegionMatches(tt.required, tt.claimed); got != tt.want {
			t.Errorf("RegionMatches(%q, %q) = %v, want %v", tt.required, tt.claimed, got, tt.want)
		}
	}
}

func TestNormalizeRegion(t *testing.T) {
	if got := NormalizeRegion(" eu-de "); got != "EU-DE" {
		t.Errorf("NormalizeRegion: got %q, want EU-DE", got)
	}
}

func TestRegionPrefixes(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"EU", []string{"EU"}},
		{"eu-de", []string{"EU", "EU-DE"}}, // normalized
		{"EU-DE-BY", []string{"EU", "EU-DE", "EU-DE-BY"}},
	}
	for _, tt := range tests {
		got := RegionPrefixes(tt.in)
		if len(got) != len(tt.want) {
			t.Errorf("RegionPrefixes(%q) = %v, want %v", tt.in, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("RegionPrefixes(%q)[%d] = %q, want %q", tt.in, i, got[i], tt.want[i])
			}
		}
	}
}
