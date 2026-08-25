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
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"

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

// tunDevice is the clone device every tun is created through. Its absence and
// its permissions are two different problems with two different fixes, which
// is the whole reason for the diagnosis below.
const tunDevice = "/dev/net/tun"

// userNSRestriction is Ubuntu's switch for what an unprivileged user namespace
// may do.
const userNSRestriction = "/proc/sys/kernel/apparmor_restrict_unprivileged_userns"

// userNSCapabilitiesRestricted reports whether this host takes back the
// capabilities a user namespace would otherwise grant.
//
// Where it is on, a process with no AppArmor profile that creates a user
// namespace is confined to the unprivileged_userns profile, and that profile
// denies every capability. Nothing fails at the time: the namespace is created,
// and the capability inside it is refused later.
func userNSCapabilitiesRestricted() bool {
	value, err := os.ReadFile(userNSRestriction)
	return err == nil && strings.TrimSpace(string(value)) != "0"
}

// describeTunFailure turns a netlink error into the thing to change.
func describeTunFailure(err error) string {
	_, statErr := os.Stat(tunDevice)
	return tunHint(err, statErr, userNSCapabilitiesRestricted())
}

// tunHint explains a failed tun creation.
//
// The old message asked whether the kernel had CONFIG_TUN, which is the right
// question in a microVM and useless in a container, where the same failure
// means the device was not passed in or the capability was not granted. The
// profiles fail differently, so they are told apart here rather than left to
// whoever is reading a log at the time.
func tunHint(err error, statErr error, restrictedUserNS bool) string {
	switch {
	case os.IsNotExist(statErr):
		return "there is no " + tunDevice + ". In a microVM that means a guest kernel built without CONFIG_TUN" +
			" (the stock Firecracker CI kernels carry vsock but no tun driver; 6.18.41 has it)." +
			" In a container it means the device was not passed in: `--device /dev/net/tun` for docker," +
			" or a hostPath volume of type CharDevice for a Kubernetes pod"

	case os.IsPermission(statErr):
		return tunDevice + " exists but cannot be opened. Check the device's own permissions, and any" +
			" device cgroup or seccomp policy the runtime applies; note that a user namespace does not" +
			" help here, because opening the device is checked against the host and not the namespace"

	case errors.Is(err, syscall.EPERM), errors.Is(err, syscall.EACCES),
		// netlink formats this one rather than wrapping the errno, so there is
		// nothing for errors.Is to match on.
		err != nil && strings.Contains(err.Error(), "TUNSETIFF"):
		// Named first because the advice below is wrong here: the capability
		// was granted and then taken away again, so granting it harder is no
		// answer.
		if restrictedUserNS {
			return "creating a tun was refused, and this host restricts what an unprivileged user namespace may do" +
				" (" + userNSRestriction + " is not 0): a process with no AppArmor profile that creates one is" +
				" confined to the unprivileged_userns profile, which denies every capability -- so the namespace" +
				" was created and CAP_NET_ADMIN in it was refused anyway. Give the sandbox an AppArmor profile" +
				" that permits capabilities, or set that sysctl to 0"
		}
		return "creating a tun was refused. It needs CAP_NET_ADMIN in the user namespace that owns this network" +
			" namespace: a user namespace of your own grants it over the namespaces it owns, which is what" +
			" --create-namespaces relies on; otherwise `--cap-add NET_ADMIN` for docker, or" +
			" securityContext.capabilities.add: [NET_ADMIN] for a pod"

	case errors.Is(err, syscall.ENODEV):
		return "the kernel has no tun driver. Rebuild the guest kernel with CONFIG_TUN=y"
	}
	return "the tun device could not be created, and the cause is not one this knows how to explain"
}
