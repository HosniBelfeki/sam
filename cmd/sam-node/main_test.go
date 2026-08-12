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

import "testing"

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
