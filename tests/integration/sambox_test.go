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

package integration_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/sam/internal/sambox"
)

// TestSandboxReachesOnlyWhatPolicyAllows joins the two halves of the sandbox:
// the real nano-init process building the only route out, and the real sam-box
// boundary deciding what that route reaches.
//
// The seam this covers is the name. nano-init answers the agent's lookup with a
// placeholder address, turns the flow back into the name that address stood for,
// and the boundary rules on the name -- so an unmodified curl, with no proxy
// configuration and no cooperation of its own, reaches an allowed host and is
// refused a blocked one. Each side is unit-tested alone; only together do they
// show the name surviving the trip.
//
// nano-init is a container PID 1: it builds a tun, routes everything through it
// and rewrites /etc/resolv.conf. Doing that on the host would break the
// machine's DNS, so the test re-executes itself inside a fresh user, mount and
// network namespace with /etc/resolv.conf bind-mounted over. Where an
// unprivileged user namespace is unavailable, the test skips.
func TestSandboxReachesOnlyWhatPolicyAllows(t *testing.T) {
	const (
		allowedHost = "allowed.example"
		blockedHost = "blocked.example"
		serverBody  = "reached the far side"
	)

	if os.Getenv("SAM_TEST_IS_ISOLATED") != "1" {
		for _, tool := range []string{"unshare", "bash", "curl", "ip"} {
			if _, err := exec.LookPath(tool); err != nil {
				t.Skipf("requires %q to isolate nano-init from the host", tool)
			}
		}

		// Built out here: nano-init is a separate module, and inside the
		// namespace there is no network to fetch one.
		nanoInitBin := buildBinary(t, "./cmd/nano-init")

		self, err := os.Executable()
		if err != nil {
			t.Fatalf("locating the test binary: %v", err)
		}

		resolvConf := filepath.Join(t.TempDir(), "resolv.conf")
		if err := os.WriteFile(resolvConf, nil, 0o644); err != nil {
			t.Fatalf("creating a stand-in resolv.conf: %v", err)
		}

		cmd := exec.Command("unshare", "-m", "-n", "-r", "bash", "-c", fmt.Sprintf(
			"ip link set lo up && mount --bind %s /etc/resolv.conf && exec %s -test.run='^%s$' -test.v",
			resolvConf, self, t.Name(),
		))
		cmd.Env = append(os.Environ(),
			"SAM_TEST_IS_ISOLATED=1",
			"SAM_NANO_INIT_BIN="+nanoInitBin,
		)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("the isolated run failed: %v", err)
		}
		return
	}

	nanoInitBin := os.Getenv("SAM_NANO_INIT_BIN")
	if nanoInitBin == "" {
		t.Fatal("SAM_NANO_INIT_BIN is unset: this process was not re-executed by the parent")
	}

	// What the sandbox is trying to reach. It is on loopback inside the
	// namespace, which the boundary can dial and the sandbox cannot: the
	// sandbox has no route except the tun.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, serverBody)
	}))
	defer server.Close()

	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parsing the server URL: %v", err)
	}

	// Socket paths have a ~104 byte kernel budget, which a test name spends
	// quickly, so this is not t.TempDir().
	sockDir, err := os.MkdirTemp("", "sandbox")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer func() { _ = os.RemoveAll(sockDir) }()
	agentSocket := filepath.Join(sockDir, "agent.sock")

	var dialed struct {
		sync.Mutex
		names []string
	}

	egress, err := sambox.NewEgressPolicy([]string{allowedHost})
	if err != nil {
		t.Fatalf("NewEgressPolicy: %v", err)
	}
	listener, err := sambox.ListenSandboxSocket(agentSocket)
	if err != nil {
		t.Fatalf("ListenSandboxSocket: %v", err)
	}

	boundary := &sambox.SOCKS5Server{
		Dialer: &sambox.AgentDialer{
			Router: &sambox.Router{Egress: egress},
			// Stands in for the resolution the boundary would do itself. What
			// is being recorded is that a name arrived here at all.
			DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				host, _, err := net.SplitHostPort(address)
				if err != nil {
					host = address
				}
				dialed.Lock()
				dialed.names = append(dialed.names, host)
				dialed.Unlock()
				return (&net.Dialer{}).DialContext(ctx, network, serverURL.Host)
			},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() {
		defer close(served)
		if err := boundary.Serve(ctx, listener); err != nil {
			t.Errorf("boundary: %v", err)
		}
	}()
	defer func() {
		cancel()
		<-served
	}()

	// One nano-init run covers both cases: the tun it creates outlives the
	// process, so a second run in this namespace would find it already there.
	agent := fmt.Sprintf(
		`curl -sS --max-time 20 http://%s/; echo "allowed-exit=$?"; `+
			`curl -sS --max-time 20 http://%s/; echo "blocked-exit=$?"`,
		allowedHost, blockedHost,
	)

	runCtx, runCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer runCancel()

	// The sandbox's exit status is the agent's, and the blocked curl fails on
	// purpose, so a non-zero status is expected and the output is the verdict.
	out, err := exec.CommandContext(runCtx, nanoInitBin, "run", agentSocket, "bash", "-c", agent).CombinedOutput()
	t.Logf("nano-init exited with %v, sandbox output:\n%s", err, out)

	if !strings.Contains(string(out), serverBody) {
		t.Errorf("the sandbox did not reach %s", allowedHost)
	}
	if !strings.Contains(string(out), "allowed-exit=0") {
		t.Errorf("reaching %s failed, want policy to allow it", allowedHost)
	}
	if strings.Contains(string(out), "blocked-exit=0") {
		t.Errorf("reaching %s succeeded, want the boundary to refuse it", blockedHost)
	}

	dialed.Lock()
	defer dialed.Unlock()
	if len(dialed.names) != 1 || dialed.names[0] != allowedHost {
		t.Errorf("the boundary dialled %q, want exactly [%q]: an address here means the name did not survive the tun",
			dialed.names, allowedHost)
	}
}
