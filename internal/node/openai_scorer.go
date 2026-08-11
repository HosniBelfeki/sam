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
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/sam/api"
)

// providerBackoff is how long a provider is skipped after a retryable failure.
const providerBackoff = 15 * time.Second

// Rejection reasons for facade provider filtering (metric label values).
const (
	reasonPeerRevoked      = "peer_revoked"
	reasonProviderBackoff  = "provider_backoff"
	reasonRegionMismatch   = "region_mismatch"
	reasonNoEligible       = "no_eligible_provider"
	reasonAttemptsExceeded = "attempts_exceeded"
)

// parseRequiredRegions splits the X-Sam-Required-Region header value into
// canonical continent codes; any invalid code rejects the whole request.
func parseRequiredRegions(h string) ([]string, error) {
	if h == "" {
		return nil, nil
	}
	var out []string
	for _, part := range strings.Split(h, ",") {
		p := api.NormalizeRegion(part)
		if p == "" {
			continue
		}
		if err := api.ValidateRegion(p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// regionAllowed reports whether a provider's claim satisfies any required
// region (hierarchical, see api.RegionMatches).
func regionAllowed(required []string, claimed string) bool {
	claimed = api.NormalizeRegion(claimed)
	for _, req := range required {
		if api.RegionMatches(req, claimed) {
			return true
		}
	}
	return false
}

// rankProviders applies hard constraints then orders the survivors: eligible
// locals first, then remotes by ascending advertised load. Providers with no
// load information look idle — without telemetry this degrades to rotation
// among equals. Every exclusion is accounted per reason.
func (f *openAIFacade) rankProviders(providers []modelProvider, requiredRegions []string) []modelProvider {
	var locals, remotes []modelProvider
	for _, p := range providers {
		region := p.region
		if p.peerID == "" && f.localRegion != nil {
			region = f.localRegion()
		}
		// Region is fail-closed: a requirement never matches an unlabeled or
		// coarser-scoped provider. Labels are routing hints until
		// biscuit-attested.
		if len(requiredRegions) > 0 && !regionAllowed(requiredRegions, region) {
			recordFacadeRejection(reasonRegionMismatch)
			continue
		}
		if p.peerID == "" {
			locals = append(locals, p)
			continue
		}
		if f.isRevoked != nil && f.isRevoked(p.peerID) {
			recordFacadeRejection(reasonPeerRevoked)
			continue
		}
		if f.inBackoff(p) {
			recordFacadeRejection(reasonProviderBackoff)
			continue
		}
		remotes = append(remotes, p)
	}

	sort.SliceStable(remotes, func(i, j int) bool { return remotes[i].active < remotes[j].active })
	rotateEqualHead(remotes)
	return append(locals, remotes...)
}

// rotateEqualHead rotates the group of equally loaded head providers so
// repeated requests spread across them instead of hammering the first.
func rotateEqualHead(remotes []modelProvider) {
	if len(remotes) < 2 {
		return
	}
	group := 1
	for group < len(remotes) && remotes[group].active == remotes[0].active {
		group++
	}
	if group < 2 {
		return
	}
	offset := int(facadeRR.Add(1) % uint64(group))
	head := append([]modelProvider{}, remotes[:group]...)
	for i := range group {
		remotes[i] = head[(i+offset)%group]
	}
}

func backoffKey(p modelProvider) string { return p.peerID + "|" + p.service }

func (f *openAIFacade) inBackoff(p modelProvider) bool {
	f.backoffMu.Lock()
	defer f.backoffMu.Unlock()
	until, ok := f.backoff[backoffKey(p)]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(f.backoff, backoffKey(p))
		return false
	}
	return true
}

func (f *openAIFacade) recordBackoff(p modelProvider) {
	if p.peerID == "" {
		return // locals are not backed off; the service registry owns their lifecycle
	}
	f.backoffMu.Lock()
	defer f.backoffMu.Unlock()
	if f.backoff == nil {
		f.backoff = map[string]time.Time{}
	}
	f.backoff[backoffKey(p)] = time.Now().Add(providerBackoff)
}

// retryableStatus reports provider responses that are safe to retry
// elsewhere: the provider produced no model output.
func retryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// attemptWriter defers committing to the client until the provider's status
// is known: retryable statuses are swallowed (body discarded) so the request
// can be retried on the next-ranked provider; anything else streams through.
type attemptWriter struct {
	dst       http.ResponseWriter
	header    http.Header
	status    int
	retryable bool
	started   bool
}

func newAttemptWriter(dst http.ResponseWriter) *attemptWriter {
	return &attemptWriter{dst: dst, header: http.Header{}}
}

func (a *attemptWriter) Header() http.Header { return a.header }

func (a *attemptWriter) WriteHeader(code int) {
	if a.started || a.retryable {
		return
	}
	a.status = code
	if retryableStatus(code) {
		a.retryable = true
		return
	}
	dst := a.dst.Header()
	for k, vv := range a.header {
		dst[k] = vv
	}
	a.dst.WriteHeader(code)
	a.started = true
}

func (a *attemptWriter) Write(p []byte) (int, error) {
	if a.retryable {
		return len(p), nil // discard failed attempt's error body
	}
	if !a.started {
		a.WriteHeader(http.StatusOK)
	}
	return a.dst.Write(p)
}

func (a *attemptWriter) Flush() {
	if !a.started {
		return
	}
	if fl, ok := a.dst.(http.Flusher); ok {
		fl.Flush()
	}
}
