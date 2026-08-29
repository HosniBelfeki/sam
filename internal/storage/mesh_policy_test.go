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

package storage

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/google/sam/api"
)

// TestMeshPolicyRoundTripsEveryRoleField stores a role with every repeated
// field populated and reads it back.
//
// role_permissions rows are discriminated by a resource_type string, so a field
// added to PolicyRole without a matching case here is written nowhere and read
// back empty. Nothing fails: the policy saves, the API returns it, and the
// grant silently does not exist. This walks the message with reflection so a
// future field is caught here rather than in production.
func TestMeshPolicyRoundTripsEveryRoleField(t *testing.T) {
	store, err := NewSQLStore("sqlite", filepath.Join(t.TempDir(), "policy.db"))
	if err != nil {
		t.Fatalf("NewSQLStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	ctx := context.Background()
	want := &api.PolicyRole{
		Name:            "everything",
		AllowedTargets:  []string{"group:backend"},
		AllowedServices: []string{"mcp://tool"},
		CustomDatalog:   []string{`region("emea")`},
		AllowedAgents:   []string{"*.prod.acme.example"},
		AllowedLabels:   []string{"region=*"},
	}

	if err := store.SaveMeshPolicy(ctx, []*api.PolicyRole{want}, nil); err != nil {
		t.Fatalf("SaveMeshPolicy: %v", err)
	}

	roles, _, err := store.GetMeshPolicy(ctx)
	if err != nil {
		t.Fatalf("GetMeshPolicy: %v", err)
	}
	if len(roles) != 1 {
		t.Fatalf("got %d roles, want 1", len(roles))
	}
	got := roles[0]

	// Reflection rather than a field list: a new repeated field left out of
	// SaveMeshPolicy comes back empty and is reported here by name.
	wantVal := reflect.ValueOf(want).Elem()
	gotVal := reflect.ValueOf(got).Elem()
	for i := 0; i < wantVal.NumField(); i++ {
		field := wantVal.Type().Field(i)
		if !field.IsExported() || field.Type.Kind() != reflect.Slice {
			continue
		}
		w := wantVal.Field(i).Interface()
		g := gotVal.Field(i).Interface()
		if !reflect.DeepEqual(w, g) {
			t.Errorf("field %s did not round trip: saved %v, loaded %v", field.Name, w, g)
		}
	}
}
