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

package tunnel

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"sync"
	"syscall"
	"time"

	golog "github.com/ipfs/go-log/v2"
)

var logger = golog.Logger("tunnel")

// Cloudflare opens a TryCloudflare quick tunnel: `cloudflared tunnel --url
// <target>` publishes the target on a random https://*.trycloudflare.com
// subdomain with no account or login. Quick tunnels are a development
// convenience with no SLA; cloudflared prints the assigned URL on its log
// output, which is the only thing this provider parses.
//
// The connector binary is resolved in order: Binary if set, `cloudflared` on
// PATH, a previously installed copy under InstallDir whose digest still
// matches the pin, and finally a fresh download of the pinned release, which
// happens only if Consent agrees. A cached copy that fails verification is
// never run.
type Cloudflare struct {
	// Binary is an explicit cloudflared executable; when set, nothing else
	// is tried.
	Binary string
	// Timeout bounds how long Open waits for the URL; defaults to 30s.
	Timeout time.Duration
	// InstallDir is where a downloaded cloudflared is kept (sam-one uses
	// <data-dir>/bin). Empty disables both the cache and downloads.
	InstallDir string
	// Consent is asked before downloading; installing cloudflared means
	// accepting Cloudflare's license, so this must be an explicit choice.
	// Nil never downloads.
	Consent func(version, url string) bool
	// ReleaseURL overrides the GitHub release download prefix (mirrors,
	// tests). The pinned digests still apply.
	ReleaseURL string
	// HTTPClient performs the download; nil uses a 10 minute timeout.
	HTTPClient *http.Client
}

// quickTunnelURL matches the assigned hostname in cloudflared's banner. The
// same pattern also matches cloudflared's own API endpoint, which shows up
// in retry logs before the banner; assignedQuickTunnelURL filters it out.
var quickTunnelURL = regexp.MustCompile(`https://([a-z0-9-]+)\.trycloudflare\.com`)

// assignedQuickTunnelURL returns the quick-tunnel URL in line, or "" when
// the line has none or only names api.trycloudflare.com.
func assignedQuickTunnelURL(line string) string {
	for _, m := range quickTunnelURL.FindAllStringSubmatch(line, -1) {
		if m[1] != "api" {
			return m[0]
		}
	}
	return ""
}

// Name implements Provider.
func (c *Cloudflare) Name() string { return "cloudflare" }

// Open implements Provider.
func (c *Cloudflare) Open(ctx context.Context, target string) (Tunnel, error) {
	binary, err := c.resolveBinary(ctx)
	if err != nil {
		return nil, err
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	// The process outlives Open: its lifetime is the tunnel's, ended by Close.
	procCtx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(procCtx, binary, "tunnel", "--url", target, "--no-autoupdate")
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("failed to start cloudflared: %w", err)
	}
	t := &process{cancel: cancel, done: make(chan struct{})}
	urlCh := make(chan string, 1)
	go t.scan(pr, urlCh)
	go func() {
		err := cmd.Wait()
		_ = pw.Close()
		t.finish(err)
	}()

	select {
	case u := <-urlCh:
		t.url = u
		return t, nil
	case <-t.done:
		return nil, fmt.Errorf("cloudflared exited before publishing a URL: %w", t.Err())
	case <-time.After(timeout):
		_ = t.Close()
		return nil, fmt.Errorf("cloudflared did not publish a URL within %s", timeout)
	case <-ctx.Done():
		_ = t.Close()
		return nil, ctx.Err()
	}
}

// resolveBinary picks the cloudflared to run; see the type doc for the order.
func (c *Cloudflare) resolveBinary(ctx context.Context) (string, error) {
	if c.Binary != "" {
		path, err := exec.LookPath(c.Binary)
		if err != nil {
			return "", fmt.Errorf("%w at %q: %v", ErrCloudflaredUnavailable, c.Binary, err)
		}
		return path, nil
	}
	if path, err := exec.LookPath("cloudflared"); err == nil {
		return path, nil
	}
	if c.InstallDir == "" {
		return "", fmt.Errorf("%w on PATH (install it from %s)", ErrCloudflaredUnavailable, CloudflaredLicenseURL)
	}
	asset, ok := pinnedAsset()
	if !ok {
		return "", fmt.Errorf("%w on PATH and no pinned build for %s/%s (install it from %s)", ErrCloudflaredUnavailable, runtime.GOOS, runtime.GOARCH, CloudflaredLicenseURL)
	}
	cached := installedCloudflared(c.InstallDir)
	if verifyBinary(cached, asset.BinarySHA256) {
		return cached, nil
	}
	if _, err := os.Stat(cached); err == nil {
		logger.Warnf("Ignoring %s: digest does not match the pinned cloudflared %s", cached, CloudflaredVersion)
	}
	base := c.ReleaseURL
	if base == "" {
		base = DefaultCloudflaredReleaseURL
	}
	url := base + CloudflaredVersion + "/" + asset.Name
	if c.Consent == nil || !c.Consent(CloudflaredVersion, url) {
		return "", fmt.Errorf("%w on PATH and download not authorized (install it from %s, or allow sam-one to download the pinned release)", ErrCloudflaredUnavailable, CloudflaredLicenseURL)
	}
	client := c.HTTPClient
	if client == nil {
		client = defaultDownloadClient()
	}
	return downloadCloudflared(ctx, client, base, c.InstallDir)
}

// process is a Tunnel backed by a child connector process.
type process struct {
	url    string
	cancel context.CancelFunc
	done   chan struct{}

	mu     sync.Mutex
	closed bool
	err    error
}

func (p *process) URL() string           { return p.url }
func (p *process) Done() <-chan struct{} { return p.done }

func (p *process) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

func (p *process) Close() error {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	p.cancel()
	<-p.done
	return nil
}

// finish records the exit and releases Done; an exit caused by Close is
// not an error.
func (p *process) finish(err error) {
	p.mu.Lock()
	if !p.closed && err != nil {
		p.err = err
	}
	p.mu.Unlock()
	close(p.done)
}

// scan drains the connector's output for its lifetime (a full pipe would
// stall the child) and reports the first quick-tunnel URL it sees.
func (p *process) scan(r io.Reader, urlCh chan<- string) {
	sc := bufio.NewScanner(r)
	found := false
	for sc.Scan() {
		line := sc.Text()
		logger.Debugf("cloudflared: %s", line)
		if !found {
			if m := assignedQuickTunnelURL(line); m != "" {
				found = true
				urlCh <- m
			}
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.ErrClosedPipe) {
		logger.Debugf("cloudflared output closed: %v", err)
	}
}
