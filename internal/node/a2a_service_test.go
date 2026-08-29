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
	"context"
	"testing"

	"github.com/google/sam/api"
)

func TestA2AServiceInitRejectsCommand(t *testing.T) {
	svc := &A2AService{baseService: baseService{
		info:    &api.ServiceInfo{Type: api.ServiceType_SERVICE_TYPE_A2A, Name: "agent"},
		backend: &api.RegisterServiceRequest_Command{Command: &api.CommandBackend{Command: []string{"echo"}}},
	}}
	if err := svc.Init(context.Background()); err == nil {
		t.Fatal("command backend must be rejected for a2a services")
	}
}

func TestA2AServiceInitURLBackend(t *testing.T) {
	svc := &A2AService{baseService: baseService{
		info:    &api.ServiceInfo{Type: api.ServiceType_SERVICE_TYPE_A2A, Name: "agent"},
		backend: &api.RegisterServiceRequest_TargetUrl{TargetUrl: "http://127.0.0.1:9999"},
	}}
	if err := svc.Init(context.Background()); err != nil {
		t.Fatalf("url backend must be accepted: %v", err)
	}
	if svc.Handler() == nil {
		t.Fatal("nil handler after Init")
	}
}

func TestNewServiceFromRequestA2A(t *testing.T) {
	req := &api.RegisterServiceRequest{
		Service: &api.ServiceInfo{Type: api.ServiceType_SERVICE_TYPE_A2A, Name: "agent"},
		Backend: &api.RegisterServiceRequest_TargetUrl{TargetUrl: "http://127.0.0.1:9999"},
	}
	svc, err := NewServiceFromRequest(req)
	if err != nil {
		t.Fatalf("factory must accept a2a: %v", err)
	}
	if _, ok := svc.(*A2AService); !ok {
		t.Fatalf("factory returned %T, want *A2AService", svc)
	}
}
