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

// Command nano-init is PID 1 in an agent sandbox.
//
// It gives the sandbox one route, which leads to the boundary, and then gets
// out of the agent's way.
//
// It used to do the opposite. It rewrote /etc/resolv.conf to point at a DNS
// server it ran itself, answered lookups with addresses it invented, injected
// HTTP_PROXY and friends into the agent's environment, and preloaded a shared
// object into the agent's address space to catch the connections that got past
// all that. Every one of those asks the agent to cooperate, and an agent that
// has to cooperate with its own confinement is not confined: the next library
// that ignores the proxy variables, the next subprocess that clears its
// environment, the next static binary with no loader to preload into, each one
// was outside the boundary.
//
// Routing does not ask. There is no interface in this sandbox except the tun,
// and the tun goes to the boundary, so an agent that ignores every convention
// here still reaches only what policy allowed. The resolver that remains is a
// convenience for clients that look a name up before connecting, not a control:
// an agent that resolves some other way is routed through the tun regardless.
//
// The datapath is the tun2connect library: gVisor's TCP stack terminating the
// sandbox's flows in userspace, each one leaving for the boundary as a named
// HTTP CONNECT (RFC 9110) or connect-udp (RFC 9298) tunnel, with a virtual DNS
// preserving the name the agent asked for. Writing a TCP stack here would mean
// writing retransmission, windowing and teardown, and getting those subtly
// wrong shows up as tail latency under load, which is exactly where this has
// to be trusted.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/aojea/agents.net/tun2connect/pkg/tun2connect"
)

const (
	tunName = "tun0"
	tunMTU  = 1500

	// The guest addresses sit at the TOP of tun2connect's synthetic pools:
	// the virtual DNS invents answers from the bottom up, so they can never
	// collide with one. The /10 and /64 prefix lengths make the kernel
	// install connected routes covering every synthetic address, so no
	// explicit route entries are needed.
	//
	// v4 is CGNAT space (RFC 6598) rather than the link-local range this used
	// to number from: link-local would be leak-proof at the first router, but
	// SSRF guards in HTTP clients commonly block 169.254/16, which broke
	// legitimate egress. v6 is the RFC 6666 discard-only prefix, so a packet
	// that ever escapes through a stray interface is blackholed rather than
	// delivered.
	tunAddr4 = "100.127.255.254/10"
	tunAddr6 = "100::ffff:ffff:ffff:fffe/64"

	// The resolver's address is any pool address routed through the tun: the
	// engine answers UDP port 53 locally wherever the query is sent, so it
	// needs no route or listener of its own.
	resolverIP = "100.127.255.253"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}

	switch os.Args[1] {
	case "copy":
		if len(os.Args) != 3 {
			log.Fatalf("usage: %s copy <dest>", os.Args[0])
		}
		src, err := os.Executable()
		if err != nil {
			src = "/nano-init"
		}
		if err := copyFile(src, os.Args[2]); err != nil {
			log.Fatalf("copy binary: %v", err)
		}

	case "run":
		createNS, ingressSocket, args := parseRunFlags(os.Args[2:])
		if len(args) < 2 {
			usage()
		}
		run(createNS, ingressSocket, args[0], args[1], args[2:])

	default:
		usage()
	}
}

// parseRunFlags reads our own flags and stops at the first argument that is not
// one, because everything after that belongs to the agent and must reach it
// untouched.
func parseRunFlags(args []string) (createNS bool, ingressSocket string, rest []string) {
	for len(args) > 0 {
		switch {
		case args[0] == "--create-namespaces":
			createNS, args = true, args[1:]
		case args[0] == "--ingress-socket":
			if len(args) < 2 {
				log.Fatalf("--ingress-socket needs a path")
			}
			ingressSocket, args = args[1], args[2:]
		case strings.HasPrefix(args[0], "--ingress-socket="):
			ingressSocket, args = strings.TrimPrefix(args[0], "--ingress-socket="), args[1:]
		default:
			return createNS, ingressSocket, args
		}
	}
	return createNS, ingressSocket, nil
}

// runFlags rebuilds the arguments for the re-executed half, so it is given
// what this one was given.
func runFlags(ingressSocket, boundarySocket, cmdName string, cmdArgs []string) []string {
	args := []string{"run", "--create-namespaces"}
	if ingressSocket != "" {
		args = append(args, "--ingress-socket", ingressSocket)
	}
	args = append(args, boundarySocket, cmdName)
	return append(args, cmdArgs...)
}

func usage() {
	log.Fatalf("usage:\n  %s copy <dest>\n  %s run [--create-namespaces] [--ingress-socket <path>] <boundary-socket> <cmd> [args...]",
		os.Args[0], os.Args[0])
}

// run wires the sandbox up and hands it to the agent.
func run(createNS bool, ingressSocket, boundarySocket, cmdName string, cmdArgs []string) {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	defer cancel()

	// The namespaces have to exist before anything is checked in them, and a
	// whole Go program can only enter a new network namespace by being started
	// in one. So this half makes them and becomes a supervisor; the half that
	// comes back through here does the work.
	if createNS && !insideCreatedNamespaces() {
		userNS, err := needUserNamespace()
		if err != nil {
			log.Fatalf("refusing to start: %v", err)
		}
		// Decided out here, where /etc/resolv.conf is the one the runtime gave
		// this container: if it already names our resolver there is nothing to
		// mount, and asking for a mount namespace would only invite a denial.
		mountNS := !resolvConfAlreadyOurs()
		self, err := os.Executable()
		if err != nil {
			log.Fatalf("locate this binary to re-execute it: %v", err)
		}
		args := runFlags(ingressSocket, boundarySocket, cmdName, cmdArgs)
		code, err := runAgent(ctx, cancel, self, args, withNamespaces(userNS, mountNS))
		if err != nil {
			log.Fatalf("create the sandbox namespaces: %v\n%s", err, namespaceHint(err))
		}
		os.Exit(code)
	}

	if createNS {
		if err := privateResolvConf(); err != nil {
			log.Fatalf("refusing to start: %v", err)
		}
	}

	// First, and before anything is built: if this namespace is not a sandbox
	// then the boundary is beside the point, and saying so in that order is
	// the difference between "you forgot --network none" and a puzzling
	// complaint about a socket.
	if err := assertIsolated(); err != nil {
		log.Fatalf("refusing to start: %v", err)
	}

	if err := checkBoundary(boundarySocket); err != nil {
		log.Fatalf("this sandbox has no way out: %v", err)
	}

	if err := setupNetwork(ctx, boundarySocket); err != nil {
		log.Fatalf("set up sandbox network: %v", err)
	}

	// Started here rather than earlier because it only makes sense once the
	// sandbox exists: this is the one process that can reach the agent at the
	// address the gateway will name.
	if ingressSocket != "" {
		go func() {
			if err := serveIngress(ctx, ingressSocket); err != nil {
				log.Printf("ingress: %v", err)
			}
		}()
	}

	code, err := runAgent(ctx, cancel, cmdName, cmdArgs)
	if err != nil {
		log.Fatalf("start agent: %v", err)
	}
	os.Exit(code)
}

// setupNetwork builds the only route out of the sandbox.
//
// This talks netlink rather than shelling out to `ip`, and carries its own TCP
// stack rather than running a separate binary, so a sandbox image can be the
// agent and nothing else. That is not tidiness: image size is what decides how
// many agents fit on a host.
func setupNetwork(ctx context.Context, boundarySocket string) error {
	// As PID 1 in a microVM nothing else has done this, and a sandbox without
	// loopback breaks things that have no business caring about the network.
	if lo, err := netlink.LinkByName("lo"); err == nil {
		_ = netlink.LinkSetUp(lo)
	}

	fd, err := openTUN(tunName)
	if err != nil {
		return fmt.Errorf("create %s: %w\n%s", tunName, err, describeTunFailure(err))
	}

	link, err := netlink.LinkByName(tunName)
	if err != nil {
		return fmt.Errorf("find %s after creating it: %w", tunName, err)
	}
	for _, cidr := range []string{tunAddr4, tunAddr6} {
		addr, err := netlink.ParseAddr(cidr)
		if err != nil {
			return fmt.Errorf("parse %s: %w", cidr, err)
		}
		if err := netlink.AddrAdd(link, addr); err != nil {
			return fmt.Errorf("address %s with %s: %w", tunName, cidr, err)
		}
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("bring up %s: %w", tunName, err)
	}

	// Default routes with no gateway: nothing on the far side of this link has
	// an address worth naming, and everything goes the same way regardless.
	// The connected /10 and /64 routes already cover every synthetic address,
	// but the default is what keeps the promise that routing does not ask: an
	// agent that hardcodes its own resolver still has the query answered by
	// the engine, and a stray dial to a literal address terminates at the
	// boundary as a visible refusal rather than a kernel errno. The
	// destination has to be spelled out rather than left nil, which netlink
	// reads as "no route specified at all".
	for _, dst := range []*net.IPNet{
		{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)},
		{IP: net.IPv6zero, Mask: net.CIDRMask(0, 128)},
	} {
		if err := netlink.RouteAdd(&netlink.Route{
			LinkIndex: link.Attrs().Index,
			Scope:     netlink.SCOPE_LINK,
			Dst:       dst,
		}); err != nil {
			return fmt.Errorf("default route for %s via %s: %w", dst, tunName, err)
		}
	}

	// A pod can mount the file over instead, in which case it is read-only and
	// already says this.
	if !resolvConfAlreadyOurs() {
		if err := os.WriteFile("/etc/resolv.conf", []byte("nameserver "+resolverIP+"\n"), 0o644); err != nil {
			// Not fatal: resolution is a convenience here, not the control.
			log.Printf("could not write /etc/resolv.conf, name resolution may fail: %v", err)
		}
	}

	dev, err := tun2connect.NewTUNDevice(fd, tunMTU)
	if err != nil {
		return fmt.Errorf("link endpoint on %s: %w", tunName, err)
	}
	engine, err := tun2connect.New(tun2connect.Config{
		Device: dev,
		Dialer: &tun2connect.BoundaryClient{
			DialBoundary: func(ctx context.Context) (net.Conn, error) {
				return dialBoundary(ctx, boundarySocket)
			},
		},
		DNS:       tun2connect.NewVirtualDNS(),
		EnableUDP: true,
	})
	if err != nil {
		return fmt.Errorf("start the userspace TCP stack: %w", err)
	}
	go func() {
		<-ctx.Done()
		engine.Close()
	}()
	return nil
}

// openTUN opens the clone device and names the interface. The fd is what the
// engine reads and writes; the interface is what the kernel routes into.
func openTUN(name string) (int, error) {
	fd, err := unix.Open(tunDevice, unix.O_RDWR, 0)
	if err != nil {
		return -1, fmt.Errorf("open %s: %w", tunDevice, err)
	}
	ifr, err := unix.NewIfreq(name)
	if err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	ifr.SetUint16(unix.IFF_TUN | unix.IFF_NO_PI)
	if err := unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr); err != nil {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("TUNSETIFF %s: %w", name, err)
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

// runAgent starts the agent and reports the exit status it should be judged by.
//
// The same supervision serves the namespace trampoline, whose child is this
// binary again: orphans still reparent here and still have to be reaped, and
// the exit code still has to be the one the caller sees.
func runAgent(ctx context.Context, cancel context.CancelFunc, cmdName string, cmdArgs []string, opts ...func(*exec.Cmd)) (int, error) {
	cmd := exec.CommandContext(ctx, cmdName, cmdArgs...)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
	cmd.Env = os.Environ() // Nothing injected: the agent is not configured, it is routed.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	for _, opt := range opts {
		opt(cmd)
	}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	cmd.WaitDelay = 5 * time.Second

	if err := cmd.Start(); err != nil {
		// Returned rather than fatal: the namespace trampoline starts this same
		// binary, and a refusal there means something quite different.
		return 0, err
	}

	// As PID 1 this process inherits every orphan in the sandbox, so it has to
	// reap them or the guest fills with zombies. Reaping also means Wait can
	// lose the race for the agent's own status, hence the channel.
	agentExit := make(chan syscall.WaitStatus, 1)
	reapChildren(cmd.Process.Pid, agentExit)

	waitErr := cmd.Wait()
	cancel()

	if waitErr != nil && errors.Is(waitErr, syscall.ECHILD) {
		status := <-agentExit
		if status.Signaled() {
			return 128 + int(status.Signal()), nil
		}
		return status.ExitStatus(), nil
	}
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			return exitErr.ExitCode(), nil
		}
		return 1, nil
	}
	return 0, nil
}

// reapChildren collects orphans and remembers the agent's own status.
func reapChildren(agentPid int, exitChan chan<- syscall.WaitStatus) {
	sigCh := make(chan os.Signal, 10)
	signal.Notify(sigCh, syscall.SIGCHLD)

	go func() {
		reap := func() {
			for {
				var status syscall.WaitStatus
				pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
				if pid <= 0 || err != nil {
					return
				}
				if pid == agentPid {
					select {
					case exitChan <- status:
					default:
					}
				}
			}
		}
		// Once before waiting on signals, to catch anything that exited
		// between Start and Notify.
		reap()
		for range sigCh {
			reap()
		}
	}()
}
