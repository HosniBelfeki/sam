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
	"time"

	"github.com/google/sam/api"
	"github.com/google/sam/internal/node/discovery"
)

// discoverySourceProbeTimeout bounds each backend model probe per tick.
const discoverySourceProbeTimeout = 2 * time.Second

// activeTracker is implemented by services that report in-flight load.
type activeTracker interface {
	ActiveRequests() uint32
}

// discoverySource builds gossip announcements from the registered services.
// Only inference services announce for now; MCP adoption follows.
func (n *SamNode) discoverySource() []discovery.Announcement {
	var labels map[string]string
	if n.config.Region != "" {
		labels = map[string]string{api.LabelRegion: n.config.Region}
	}
	var out []discovery.Announcement
	for _, info := range n.services.List(api.ServiceType_SERVICE_TYPE_INFERENCE) {
		svc, ok := n.services.Get(info.GetName())
		if !ok {
			continue
		}
		lister, ok := svc.(modelLister)
		if !ok {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), discoverySourceProbeTimeout)
		models, err := lister.Models(ctx)
		cancel()
		if err != nil || len(models) == 0 {
			continue
		}
		ann := discovery.Announcement{
			Type:   api.ServiceType_SERVICE_TYPE_INFERENCE,
			Name:   info.GetName(),
			Keys:   models,
			Labels: labels,
		}
		if tracker, ok := svc.(activeTracker); ok {
			ann.Load.ActiveRequests = tracker.ActiveRequests()
		}
		out = append(out, ann)
	}
	return out
}
