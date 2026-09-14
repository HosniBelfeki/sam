# SAM Code Review Style Guide

This file extends the standard Gemini Code Assist review prompt for this
repository. It lists checks that are specific to SAM and that generic review
does not catch. Apply them to every pull request that touches Go code. When a
rule is violated, quote the offending line and state the concrete fix; do not
only describe the principle.

`AGENTS.md` at the repository root holds the architecture and dependency
rules and is authoritative. This guide adds review-time checks on top of it.

## 1. Peer IDs must be canonicalized at the boundary

### Why

A libp2p peer ID has several valid string encodings. `peer.Decode` accepts
all of them (base58 multihash `12D3KooW...`, CIDv1 base32 `bafzaajai...`,
and others) and returns the same `peer.ID`, but `peer.ID.String()` always
emits the base58 form. Every cache, ban set, storage row, and `map[string]`
in SAM is keyed on that canonical form. A raw string copied off the wire and
used as a key is therefore a *different key for the same peer*, and a ban,
admission or dedup check silently misses. This is a security bug, not a
style issue.

### How to spot untrusted input

The repository has a naming convention that makes this reviewable:

- **`PeerId` / `GetPeerId()`** (protobuf spelling) is a wire field on an
  `api.*` message: `req.PeerId`, `event.PeerId`, `ann.GetPeerId()`. Treat it
  as untrusted, non-canonical input.
- **`PeerID`** (Go spelling) is an internal field or variable that is expected
  to already hold the canonical form.
- Other raw sources: URL query parameters (`r.URL.Query().Get("peer_id")`,
  `Get("id")`), path segments (`/sam/{peer}/...`), HTTP headers, MCP tool
  parameters.

### Rules

Flag any of these when the operand comes from a raw source:

1. **Discarded decode.** `if _, err := peer.Decode(s); err != nil { ... }`
   followed by continued use of `s`. The decoded `peer.ID` must be kept and
   used (or its `.String()`), never the input string.
2. **Raw string as a key.** `m[x.PeerId]`, `cache.Add(x.PeerId, ...)`,
   `cache.Contains(x.PeerId)`, `seen[p.PeerId]`.
3. **Raw string stored in a record.** `storage.EnrolledNode{PeerID: req.PeerId}`,
   `storage.RouterLease{PeerID: req.PeerId}`, `discovery.Provider{PeerID: ann.GetPeerId()}`.
4. **Raw string passed to a lookup keyed on the canonical form.**
   `store.GetNode(ctx, req.PeerId)`, `store.IsNodeBanned(ctx, req.PeerId)`,
   `store.GetEnrollmentRequest(ctx, peerIDFromQuery)`,
   `mesh.PublishEvent(ctx, api.MeshEvent_BANNED, req.PeerId, nil)`.
5. **Raw string compared for identity.** `x.PeerId == selfID`,
   `params.PeerID == n.Host.ID().String()`. Decode both sides and compare
   `peer.ID` values, or compare `.String()` of two decoded IDs. Emptiness
   checks (`!= ""`) are fine.
6. **Inconsistent handling inside one handler.** A function that decodes
   `req.PeerId` into `pID` and then uses `req.PeerId` for storage or
   publishing on a later line. Once `pID` exists, `req.PeerId` should only
   appear in log messages and error strings.

### Expected shape

```go
pID, err := peer.Decode(req.PeerId)
if err != nil {
    http.Error(w, "Invalid Peer ID", http.StatusBadRequest)
    return
}
canonical := pID.String()
// From here on: store, look up, publish and compare with pID / canonical.
```

Pubsub and libp2p callbacks already hand over a `peer.ID`
(`msg.GetFrom()`, `conn.RemotePeer()`); those are trusted and need no decode.

### Tests

A change that fixes or touches peer-ID keying must include a regression test
that feeds the alternative encoding, obtained with
`peer.ToCid(id).String()`, and asserts the lookup still hits under
`id.String()`. Ask for it if it is missing.

## 2. Time units must be explicit and consistent

### Why

Time values cross the wire as bare `int64`. The proto already mixes units:

- **Unix milliseconds:** `MeshEvent.timestamp`, the enrollment and refresh
  challenge `timestamp` fields, the `X-Sam-Challenge-Ts` header.
- **Unix seconds:** `ServiceAnnounce.timestamp`, every `expires_at`,
  `checked_at`, `credential_expires_at`, `biscuit_expires_at`.
- **Explicit-unit names:** `latency_ewma_ms`, `poll_interval_seconds`.

A mismatch between producer and consumer is a silent off-by-1000 that
freshness checks and TTLs will not catch in tests that use `time.Now()` on
both sides.

### Rules

1. **Every new `int64` time field in `api/sam.proto` must state its unit**,
   either in the field name (`_ms`, `_seconds`) or in a comment on the field.
   Prefer milliseconds for new instant fields; prefer the `_ms` suffix.
2. **Encoder and decoder must agree.** `time.Now().UnixMilli()` on the
   producing side pairs with `time.UnixMilli(x)` on the consuming side;
   `time.Now().Unix()` pairs with `time.Unix(x, 0)`. Flag any field written
   with one and read with the other, including in tests.
3. **One unit per container.** A map, cache or struct field must not receive
   seconds on one code path and milliseconds on another. Example to flag:
   seeding a cache with `time.Now().Unix()` in a constructor and later
   writing `event.Timestamp` (milliseconds) into the same cache from an event
   handler, even if the value is never read.
4. **Comparisons across units are wrong.** `event.Timestamp < lastSeen` is
   only valid if both were produced with the same `Unix*()` call. Check the
   origin of both operands.
5. **Durations stay `time.Duration`** inside Go. Convert to an integer only at
   the serialization boundary, and name the unit there.
6. **Freshness windows and TTLs in config** (`FreshnessThreshold`,
   `BiscuitTimeout`, `LeaseDuration`, `PolicySyncJitter`, etc.) must be
   `time.Duration`, not bare integers.

## 3. Test coverage must follow the pyramid

### Why

E2E tests (`tests/e2e/*.bats`) are slow and reserved for critical user
journeys. Integration tests (`tests/integration/`) are bounded to 10 seconds
per execution. Coverage belongs at the lowest layer that can express it.

### Rules

1. **Every behavior change ships with a test at the lowest adequate layer.**
   - Pure logic, parsing, canonicalization, unit conversion, policy
     evaluation: unit test in the same package (`*_test.go`).
   - Interaction between two components over the API in `api/sam.proto`
     (node ↔ control plane, router ↔ control plane): `tests/integration/`.
   - A full user journey across built binaries: `tests/e2e/*.bats`, and only
     if it is a documented CUJ.
2. **Flag coverage that is too high in the pyramid.** A new bats test for an
   edge case that a Go unit or integration test could pin is a request for
   change: ask for the lower-level test and removal of the e2e one.
3. **Flag missing regression tests for bug fixes.** A PR whose description
   says "fix", "bypass", "canonicalize", "race", "leak" or similar must add a
   test that fails on the parent commit. If no test is present, ask for one
   and suggest the layer.
4. **Flag duplicate coverage.** The same scenario asserted in both
   integration and e2e paths; keep the faster one.
5. **Integration tests must be time bounded.** Look for unbounded waits,
   `time.Sleep` longer than a few hundred milliseconds, or polling without a
   deadline. Prefer `context.WithTimeout` and `t.Deadline()`.
6. **Security fixes need a negative test.** For anything touching
   authentication, admission, bans, biscuit verification or label gating, the
   test must include the attacker's input (the forged token, the alternative
   encoding, the stale timestamp) and assert rejection, not only the happy
   path.
7. **Tests must not weaken production checks.** Flag test-only flags, `if
   testing` branches, or exported hooks whose only purpose is to skip a
   verification step.

## 4. Dependencies

Reinforcing `AGENTS.md` §2: a PR that adds a line to the root `go.mod`
`require` block must justify it in the description and must have been
explicitly agreed to. If the dependency only runs inside a sandbox image, it
belongs in that command's own module (`cmd/nano-init/go.mod` pattern). Flag
any new root dependency that lacks this justification.

## 5. Review output

- Group findings by the section numbers above so the author can see which
  rule applies.
- For rule 1 and rule 2 findings, always propose the corrected code, not
  just the diagnosis.
- Do not raise generic Go style nits (naming, comment punctuation, import
  ordering) that `gofmt` and `golangci-lint` already enforce via `make lint`.
