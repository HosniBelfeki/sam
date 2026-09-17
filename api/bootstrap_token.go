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

// BootstrapTokenRequest is the JSON body that mints a bootstrap token, on
// POST /admin/bootstrap-tokens (admin bearer) and POST /users/me/tokens
// (OIDC user). It is the one definition both the control plane and its
// clients (sam-one's CLI, the console) marshal, so a field name exists in
// exactly one place.
type BootstrapTokenRequest struct {
	// Role the token enrolls into, e.g. RoleNode. Required on the admin
	// endpoint; the user endpoint defaults it to RoleNode.
	Role string `json:"role"`
	// OwnerID is the user the token is issued on behalf of. Honored by the
	// user endpoint only, and only for admins; defaults to the caller.
	OwnerID string `json:"owner_id,omitempty"`
	// TTLHours bounds the token's validity; the control plane defaults a
	// non-positive value to 24.
	TTLHours int `json:"ttl_hours"`
	// MaxUsages is how many enrollments the token admits; the control plane
	// defaults a non-positive value to 1.
	MaxUsages int `json:"max_usages"`
	// Description is a free-form operator note stored with the token.
	Description string `json:"description,omitempty"`
	// AutonomousRecovery is copied onto every node the token enrolls: such a
	// node may still refresh its credential after the control plane's
	// signing key rotated past its grace period, on proof of possession of
	// its own key alone. Admin-only, because a node that can always recover
	// holds a credential that never expires (see
	// storage.EnrolledNode.AutonomousRecovery).
	AutonomousRecovery bool `json:"autonomous_recovery"`
}

// BootstrapTokenResponse is returned (201) when a bootstrap token is minted.
// Token is the plaintext and is shown exactly once; the control plane keeps
// only its hash, which is also the ID.
type BootstrapTokenResponse struct {
	ID        string `json:"id"`
	Token     string `json:"token"`
	Role      string `json:"role"`
	OwnerID   string `json:"owner_id,omitempty"`
	ExpiresAt string `json:"expires_at"`
}
