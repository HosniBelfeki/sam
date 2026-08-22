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
	"fmt"
	"strings"

	"github.com/vishvananda/netlink"
)

// This binary builds the only route out of a sandbox. It does not build the
// sandbox: `docker run --network none` and a microVM's own kernel each hand it
// a network namespace with nowhere to go, and it fills in the way out.
//
// That assumption is worth checking rather than trusting, because when it does
// not hold nothing complains. Started in a namespace that already has an
// interface -- a Kubernetes pod, where every container shares one, or a plain
// `docker run` where somebody forgot the flag -- it would add tun0 alongside
// the existing device, add a second default route, and hand the agent a
// sandbox that is not one. The agent would be confined to a network it can
// route around, and the run would look like every successful run.
//
// So the precondition is enforced here, once, whatever created the namespace.

// assertIsolated reports whether this network namespace is a sandbox.
func assertIsolated() error {
	links, err := netlink.LinkList()
	if err != nil {
		return fmt.Errorf("list interfaces: %w", err)
	}
	names := make([]string, 0, len(links))
	for _, l := range links {
		names = append(names, l.Attrs().Name)
	}
	return isolationError(names)
}

// isolationError names the interfaces that mean this is not a sandbox.
//
// Loopback is expected and carries no traffic off the namespace. tun0 is our
// own, which matters because the device outlives the process that made it, so
// a second run in the same namespace must read as "already set up" rather than
// as "not isolated".
func isolationError(links []string) error {
	var foreign []string
	for _, name := range links {
		if name == "lo" || name == tunName {
			continue
		}
		foreign = append(foreign, name)
	}
	if len(foreign) == 0 {
		return nil
	}
	return fmt.Errorf(
		"this network namespace has %s, so it is not a sandbox: the agent could route around the boundary. "+
			"Give it a namespace of its own -- `docker run --network none`, or a microVM with no network device",
		strings.Join(foreign, ", "),
	)
}
