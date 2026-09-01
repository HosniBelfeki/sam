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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Name preservation — the property that mesh.sam.alt reaches the boundary as
// a NAME, because the boundary chooses a provider from it — lives in the
// tun2connect library now, and is pinned by that library's own tests. What
// remains here is what nano-init still owns: the two ways of naming a
// boundary, the agent's untouched environment, and the copy mode.

func TestTheAgentEnvironmentIsNotDoctored(t *testing.T) {
	// The point of the rewrite: nano-init no longer reaches into the agent. If
	// these come back, confinement has quietly become a request for the
	// agent's cooperation again and every argument for the design stops
	// holding.
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	for _, forbidden := range []string{
		"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY",
		"LD_PRELOAD", "SSL_CERT_FILE", "REQUESTS_CA_BUNDLE",
	} {
		if strings.Contains(string(source), `"`+forbidden+`"`) {
			t.Errorf("%s is being set for the agent again", forbidden)
		}
	}
}

func TestTheBoundaryCanBeNamedEitherWay(t *testing.T) {
	// A container dials a path and a microVM dials vsock. One binary serves
	// both, and nothing else in the sandbox knows which kind it is, so this
	// string is the entire difference between them.
	if _, _, err := parseVsock("2:1080"); err != nil {
		t.Errorf("parseVsock(2:1080): %v", err)
	}
	for _, bad := range []string{"2", "host:1080", "2:not-a-port", ""} {
		if _, _, err := parseVsock(bad); err == nil {
			t.Errorf("parseVsock(%q) was accepted", bad)
		}
	}

	// A missing socket has to be reported at startup. A sandbox that starts
	// without a way out looks like a mesh outage on the agent's first call.
	if err := checkBoundary(filepath.Join(t.TempDir(), "absent.sock")); err == nil {
		t.Error("a boundary socket that does not exist was accepted")
	}
	if err := checkBoundary("vsock://2:1080"); err != nil {
		t.Errorf("a well-formed vsock boundary was rejected: %v", err)
	}
}

func TestCopyFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dest := filepath.Join(dir, "dest")

	if err := os.WriteFile(src, []byte("binary"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := copyFile(src, dest); err != nil {
		t.Fatalf("copyFile: %v", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "binary" {
		t.Errorf("contents = %q, want %q", got, "binary")
	}

	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	// The copy has to be runnable; the exact bits are the umask's business.
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("mode = %v, want the owner execute bit set", info.Mode().Perm())
	}
}
