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
	"strings"
	"testing"
)

func TestValidateLabelKey(t *testing.T) {
	valid := []string{"region", "team", "cloud-region", "on_prem.zone", "A1"}
	for _, k := range valid {
		if err := ValidateLabelKey(k); err != nil {
			t.Errorf("ValidateLabelKey(%q): unexpected error: %v", k, err)
		}
	}

	invalid := []string{"", "has space", "has,comma", "has=equals", strings.Repeat("a", 64)}
	for _, k := range invalid {
		if err := ValidateLabelKey(k); err == nil {
			t.Errorf("ValidateLabelKey(%q): expected error, got nil", k)
		}
	}
}

func TestValidateLabelValue(t *testing.T) {
	valid := []string{"us-east-1", "EU-DE", "office-berlin", "rack.3", "123"}
	for _, v := range valid {
		if err := ValidateLabelValue(v); err != nil {
			t.Errorf("ValidateLabelValue(%q): unexpected error: %v", v, err)
		}
	}

	invalid := []string{"", "has,comma", "has=equals", "has\ttab", strings.Repeat("a", maxLabelValueLen+1)}
	for _, v := range invalid {
		if err := ValidateLabelValue(v); err == nil {
			t.Errorf("ValidateLabelValue(%q): expected error, got nil", v)
		}
	}
}

func TestValidateLabels(t *testing.T) {
	if err := ValidateLabels(nil); err != nil {
		t.Errorf("ValidateLabels(nil): unexpected error: %v", err)
	}
	if err := ValidateLabels(map[string]string{"region": "us-east-1", "team": "platform"}); err != nil {
		t.Errorf("ValidateLabels(valid): unexpected error: %v", err)
	}
	if err := ValidateLabels(map[string]string{"region": ""}); err == nil {
		t.Error("ValidateLabels(empty value): expected error, got nil")
	}
	if err := ValidateLabels(map[string]string{"bad key!": "v"}); err == nil {
		t.Error("ValidateLabels(bad key): expected error, got nil")
	}
}
