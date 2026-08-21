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
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// A sandbox reaches its boundary one of two ways, and the difference is the
// only thing that distinguishes a container from a microVM here.
//
// A container gets the socket bind-mounted in, so it dials a path. A microVM
// has no shared filesystem and no network device, so it dials vsock, and
// Firecracker delivers that to a Unix socket on the host named
// "<uds_path>_<port>". Both arrive at the same sam-box, which is why one
// binary serves both and neither sandbox contains anything that knows which
// kind it is beyond this string.

const vsockScheme = "vsock://"

// dialBoundary opens a connection to the boundary named by spec, which is
// either "vsock://<cid>:<port>" or a Unix socket path.
func dialBoundary(ctx context.Context, spec string) (net.Conn, error) {
	if strings.HasPrefix(spec, vsockScheme) {
		return dialVsock(ctx, strings.TrimPrefix(spec, vsockScheme))
	}
	return (&net.Dialer{Timeout: boundaryDialTimeout}).DialContext(ctx, "unix", spec)
}

// checkBoundary reports whether the boundary named by spec could plausibly be
// reached, so a sandbox with no way out says so at startup rather than on the
// agent's first request.
func checkBoundary(spec string) error {
	if strings.HasPrefix(spec, vsockScheme) {
		if _, _, err := parseVsock(strings.TrimPrefix(spec, vsockScheme)); err != nil {
			return err
		}
		// Whether the host is listening cannot be known until a connection is
		// attempted, and attempting one here would consume a flow the agent
		// has not asked for yet.
		return nil
	}
	if _, err := os.Stat(spec); err != nil {
		return fmt.Errorf("no boundary at %s: %w", spec, err)
	}
	return nil
}

func parseVsock(hostPort string) (cid, port uint32, err error) {
	rawCID, rawPort, found := strings.Cut(hostPort, ":")
	if !found {
		return 0, 0, fmt.Errorf("vsock boundary %q is not <cid>:<port>", hostPort)
	}

	parsedCID, err := strconv.ParseUint(rawCID, 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("vsock cid %q: %w", rawCID, err)
	}
	parsedPort, err := strconv.ParseUint(rawPort, 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("vsock port %q: %w", rawPort, err)
	}
	return uint32(parsedCID), uint32(parsedPort), nil
}

// dialVsock opens an AF_VSOCK connection.
//
// This is done by hand rather than with a library because it is one socket, one
// connect, and a library would be a dependency carried into every sandbox for
// forty lines.
func dialVsock(ctx context.Context, hostPort string) (net.Conn, error) {
	cid, port, err := parseVsock(hostPort)
	if err != nil {
		return nil, err
	}

	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("vsock socket: %w", err)
	}

	if err := unix.Connect(fd, &unix.SockaddrVM{CID: cid, Port: port}); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("vsock connect %d:%d: %w", cid, port, err)
	}

	// net.FileConn dups the descriptor and hands it to the runtime poller, so
	// the original has to be closed or every flow leaks one.
	file := os.NewFile(uintptr(fd), "vsock")
	defer func() { _ = file.Close() }()

	conn, err := net.FileConn(file)
	if err != nil {
		return nil, fmt.Errorf("vsock conn: %w", err)
	}
	return conn, nil
}
