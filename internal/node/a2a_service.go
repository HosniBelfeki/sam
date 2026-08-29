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
	"fmt"

	"github.com/google/sam/api"
)

// A2AService proxies Agent2Agent (A2A) JSON-RPC/REST traffic to a local
// agent process. URL backends only: a command backend would wire the A2A
// route to an MCP stdio bridge no A2A client can talk to.
type A2AService struct{ baseService }

func (s *A2AService) Init(ctx context.Context) error {
	switch x := s.backend.(type) {
	case *api.RegisterServiceRequest_TargetUrl:
		h, err := newReverseProxyHandler(x.TargetUrl)
		if err != nil {
			return err
		}
		s.handler = h
	case *api.RegisterServiceRequest_Command:
		return fmt.Errorf("command-based backends are not supported for A2AService")
	default:
		return fmt.Errorf("unsupported backend type %T for A2AService", s.backend)
	}
	return nil
}
