---
title: "Agent Architecture"
weight: 15
---

# Agent Sandbox Architecture

This document defines how an **agent** — an unmodified harness such as
`cmd/chaos-agent`, a LangChain script, or Claude Code — is connected to the
Sovereign Agent Mesh when it runs inside a sandbox (a Firecracker microVM or a
`--network none` container).

It is a design document: it states the layering, the contracts each layer
exposes, and the tests that hold those contracts in place. Scale testing depends
on this being settled, not the other way round.

## 1. Goals

1. **The harness is unmodified and mesh-unaware.** It opens ordinary sockets, it
   resolves ordinary names, it speaks ordinary HTTP. No SDK, no `LD_PRELOAD`, no
   proxy environment variables it has to honour.
2. **One enforcement point.** Every packet the sandbox emits is authorized in
   exactly one place, by name, against one policy.
3. **The sandbox has no network.** Not a filtered network — *no* network device
   other than the one we hand it. Isolation is a property of the runtime
   (`network=none`, a microVM without a tap device), not of a firewall rule.
4. **The mesh looks like the internet.** Reaching a remote model or a remote MCP
   server is the same act as reaching `api.github.com`: connect to a name. Which
   names are mesh-internal and which are public is a policy decision made on the
   host, not a code path in the agent.
5. **Deps stay put.** The design below adds no `go.mod` entry (see §9).

## 2. What is wrong with the current layering

Today there are three independent interception mechanisms and two overlapping
local dataplanes.

`cmd/nano-init` intercepts at the application layer, three ways at once:

* a **DNS spoofer** on `127.0.0.1:53` plus a rewritten `/etc/resolv.conf`,
* **`HTTP_PROXY`/`HTTPS_PROXY` injection** into the child's environment,
* an optional **`LD_PRELOAD` socket interceptor**, shipped per architecture and
  per libc from the gateway's `/internal/bootstrap/libinterceptor.so`.

Each one leaks. Proxy variables are honoured only by HTTP-aware clients — a
harness that opens a raw socket, a Python library using `httpx` with
`trust_env=False`, or anything speaking gRPC or Postgres simply escapes. DNS
spoofing forces every flow through a MITM even when no secret needs injecting,
and it answers with loopback addresses, which destroys the destination name the
policy engine actually wants to reason about. The `LD_PRELOAD` shim has to be
built and served for every arch/libc pair, and is defeated by a static binary.
The `nano-init` README is explicit that this is deliberate ("HTTP is an
opinionated waist"), and that reasoning was sound when the only capability was
HTTP. It stops being sound the moment an agent needs to `git clone`, or the
moment a sandbox needs a deny-by-default network.

Meanwhile there are two host-side dataplanes with disjoint capabilities:

| | `sam-node` sidecar | `sam-box` gateway |
|---|---|---|
| Local MCP (`/mcp`) | yes | no |
| OpenAI facade (`/v1/*`) | yes | no |
| Mesh egress to a peer (`/sam/<peer>/…`) | yes | no |
| Internet egress | no | yes |
| Secret injection (MITM CA) | no | yes |
| Domain filtering | no | no |
| Own libp2p host + enrollment | yes | yes (duplicated) |

An agent that wants inference *and* `api.github.com` cannot be served by either
one. `scripts/microvm-init.sh` shows the seam directly: it starts `tun2proxy`
pointed at the `sam-box` UDS **and** hands the harness
`--mcp-url http://127.0.0.1:8080/mcp`, two mutually exclusive assumptions about
what is listening on that port. It is also wired incorrectly — Firecracker
multiplexes guest→host vsock connections onto `<uds_path>_<port>`, so the guest's
`VSOCK-CONNECT:2:8080` surfaces on the host as `sam-box-1.sock_8080` while
`sam-box` listens on `sam-box-1.sock`.

Neither dataplane can filter by domain, which is the one network capability the
use case actually asks for.

## 3. The layering

One socket, one protocol, one policy point. SOCKS5 is the waist.

```text
┌─ sandbox (microVM, or container with network=none) ───────────────┐
│                                                                   │
│   harness (unmodified: sockets + DNS + HTTP)                      │
│        │ IP                                                       │
│      tun0  ── default route, the only device besides lo           │
│        │                                                          │
│   tun2socks ── IP flows → SOCKS5, destination kept as a NAME      │
│        │       (virtual-DNS / fake-IP, see Decision 2)            │
└────────┼──────────────────────────────────────────────────────────┘
         │  one byte stream
         │  microVM  : AF_VSOCK → firecracker → host UDS <uds>_<port>
         │  container: bind-mounted UDS
┌────────┼──────────────────────────────────────────────────────────┐
│ host   ▼                                                          │
│  sam-box  (one per sandbox, NO libp2p host, NO enrollment)        │
│    = SOCKS5 server = THE policy enforcement point                 │
│        │                                                          │
│        ├── mesh.sam.alt          ── /v1 + /mcp only ─────┐        │
│        ├── <svc>.<type>.sam.alt  ── HTTP: discover, then ┤        │
│        │                            /sam/<peer>/<type>/… │        │
│        └── anything else ── deny-by-default domain       │        │
│                             policy, opt-in MITM +        │        │
│                             secret injection             │        │
│                                    │                     ▼        │
│                                    │           sam-node sidecar   │
│                                    ▼           UDS: /mcp /v1/*    │
│                              the internet           /sam/*        │
└─────────────────────────────────────────────────────┼─────────────┘
                                                      │ libp2p
                                                    mesh
```

### Decision 1 — SOCKS5, not HTTP CONNECT, is the sandbox boundary protocol

SOCKS5 is chosen for three properties HTTP proxying does not have:

* **It carries the destination name.** `ATYP=0x03` is a domain name. The gateway
  authorizes `api.github.com`, not `140.82.121.5`. Domain filtering therefore
  needs no DNS interception, no MITM, and no IP allowlists that rot.
* **It is protocol-agnostic.** TCP is TCP. `git`, Postgres, gRPC and a raw socket
  are all first-class; HTTP becomes a payload rather than a requirement.
* **It has a refusal vocabulary.** Reply `0x02 connection not allowed by
  ruleset` is a policy denial the guest kernel turns into a clean
  `ECONNREFUSED` — exactly what an agent should observe when it reaches for
  something it may not have. UDP (`UDP ASSOCIATE`) is specified but stays **out
  of scope** for v1: deny it, so the only way out is a named TCP flow.

HTTP MITM does not disappear; it moves *above* the waist. When a domain is
configured for secret injection, the gateway terminates TLS for that flow with
the existing `internal/sambox` ephemeral CA and injects the header. Every other
flow is a byte pipe with no interception at all — a meaningful improvement over
the status quo, where every flow is MITM'd.

### Decision 2 — names are resolved on the host, never in the guest

`tun2socks` runs with a virtual-DNS pool (fake IPs out of `198.18.0.0/15`). The
guest resolver gets a synthetic address, `tun2socks` maps that address back to
the FQDN when the flow opens, and the gateway receives the FQDN in the SOCKS5
request. Nothing in the guest ever performs a real DNS lookup, so:

* `nano-init`'s DNS spoofer, `/etc/resolv.conf` rewrite, `HTTP_PROXY` injection
  and `libinterceptor.so` bootstrap are **deleted**, not reimplemented;
* the guest cannot exfiltrate over DNS, because there is no resolver path out;
* the policy engine gets the name for free, on every flow, for every protocol.

### Decision 3 — the host never speaks AF_VSOCK

Firecracker's vsock device already terminates AF_VSOCK and hands the host a Unix
socket per guest port (`<uds_path>_<port>`). The gateway therefore accepts on a
`net.Listener` that is *always* a UDS. A microVM and a `network=none` container
become the same code path, and the difference is one line of sandbox setup:

| sandbox | guest side | host side |
|---|---|---|
| Firecracker microVM | `tun2socks` → vsock CID 2, port *P* | listener on `<uds>_P` |
| container, `network=none` | `tun2socks` → `/run/sam/agent.sock` | listener on the bind-mounted path |

This keeps AF_VSOCK out of the Go code entirely (no new dependency) and — more
importantly for the test pyramid — makes the whole datapath exercisable in a Go
integration test against a UDS in `t.TempDir()`, with no VM and no KVM.

### Decision 4 — a mesh name is the service namespace written in DNS shape

The mesh already has a canonical namespace: `mcp://<service>`,
`inference://<service>`, `system://sam.catalog`, and internally
`libp2p://<peer>/<type>/<service>/…`. That namespace is what the control plane
authorizes in `allowed_services` and what lands in the Biscuit `service()` fact.
Sandbox names are a **projection of it**, not a second namespace:

```text
inference://openrouter   <->   openrouter.inference.sam.alt
mcp://code-reviewer      <->   code-reviewer.mcp.sam.alt
(the mesh, curated)      <->   mesh.sam.alt
```

The mapping is declared once, in [`api/names.go`](https://github.com/google/sam/blob/main/api/names.go)
(`MeshZone`, `MeshEntrypointHost`, `MeshHost`, `ParseMeshHost`), so the gateway
and the node cannot drift. The rule that keeps it honest: **never introduce a
routing decision expressible in one form but not the other.**

`.alt` is the pseudo-TLD reserved by RFC 9476 for namespaces that are
deliberately *not* resolved through the DNS — which is exactly what these are.
It cannot collide with a delegated gTLD, and a name that escapes a sandbox fails
closed instead of resolving to somebody else's host. `.local` was rejected: it
is mDNS's (RFC 6762) and resolvers treat it specially.

Provider selection is *not* in the name. `openrouter.inference.sam.alt` says
which service, never which peer; that stays a discovery decision, scored by the
existing provider scorer. Pinning a peer would mirror `libp2p://<peer>/…` as a
longer name, but needs a DNS-safe peer encoding first — base58 peer IDs are
case-sensitive and DNS labels are not. IPFS hit the same wall and solved it with
lowercase base36 CIDs in subdomain gateways; that is the path if we ever need
it, and it is noted in `api/names.go` rather than half-built.

### Decision 5 — the agent is the principal; the node is only the channel

Traceability and granular policy are the product. "Agent `foo` may use
`inference://X`, agent `bar` may not" has to be a statement the *whole mesh* can
evaluate, and it has to survive an agent being paused on one node and resumed on
another. So agent identity travels with the agent, never with the host.

A Biscuit is bound to a peer ID: `SamNode.Authorize` requires a `node(<peer>)`
fact matching the connecting peer and enforces `BaselineReplayCheck`
(`client_peer_id == connection_peer_id`). That binding is right — for the
*channel*. It is the wrong place to express *who is asking*.

The mesh already knows how to take in a foreign identity, and agents reuse that
pattern one level down. A node presents an OIDC JWT **once**, at enrollment; the
control plane verifies it against a configured issuer and mints a Biscuit;
`translateClaimsToFacts` turns claims into facts, and the JWT never appears on
the datapath again. The two domains are joined at exactly one point and never
mixed:

| | asserted by | verified against | when | on the datapath as |
|---|---|---|---|---|
| node identity | control plane | the OIDC issuer | enrollment | a Biscuit |
| agent identity | the enrolled SAM component that admitted the agent | the platform's workload issuer (K8s SA token, pod certificate, SVID) | admission | an appended Biscuit block |

The agent's facts ride as an **attenuation block appended to the node token**,
not as a second credential. `biscuit-go` cannot verify a block signed by a third
party (§10.2), so splitting them buys no extra cryptographic attribution while
costing a second verification on every request. The identifier is still the
agent's and still portable: teleport the actor to another worker and the SAM
component there verifies the same workload credential and asserts the same
`agent:` name. Nothing downstream notices the move.

The residual trust, stated plainly: an enrolled node asserts its own agents.
Mesh policy bounds that by binding namespaces to node attestation — only nodes
attested `cluster=c1.acme` may assert `agent:*.c1.acme` — which the existing
target-fact machinery already expresses. Non-repudiation *by the agent itself*
needs proof of possession, which stays an upgrade path (§10.6).

## 4. Where the gateway lives — DECIDED: `sam-box`, without a libp2p host

`sam-node` and `sam-box` currently answer this twice: both run a libp2p host and
both enroll. The decision is to keep two binaries but give them disjoint jobs.

**`sam-node` — the mesh member.** Unchanged. It owns the libp2p host,
enrollment, identity, discovery and the sidecar API (`/mcp`, `/v1/*`,
`/sam/service/*`, `/sam/<peer>/…`) over TCP and its Unix socket.

**`sam-box` — the sandbox dataplane.** One per sandbox. It holds **no libp2p
host, no enrollment, no mesh identity**. It serves SOCKS5 on the sandbox-facing
socket, enforces the egress policy, does MITM/secret injection where configured,
and reaches the mesh solely by dialling the local `sam-node` sidecar Unix socket
as a client.

What this buys:

* *N* sandboxes per host still share **one** libp2p host, so no *N*× DHT
  clients, identify/ping loops or router connections — the thing that would
  otherwise cap density in the scale experiment.
* `sam-box` becomes small and boring: a SOCKS5 server plus an HTTP client. No
  `join`, no OIDC, no Biscuit handling, no bbolt store. Most of
  [cmd/sam-box/main.go](cmd/sam-box/main.go) is deleted.
* The blast radius of a compromised sandbox stops at its `sam-box`, which holds
  one agent credential and can be killed and reissued in isolation.
* Failure isolation and lifecycle match the sandbox's, not the host's.

`sam-box` is a **multiplexer of agents**: it may serve one agent per socket, or
many agents over one socket, and each flow it forwards carries that agent's own
identity (Decision 5, §10). Losing the libp2p host costs it nothing here — the
agent's authority never came from the host in the first place.

`internal/sambox` keeps the ephemeral CA and secret injection and gains the
SOCKS5 server; `internal/node` keeps everything mesh.

## 5. Contracts

### 5.1 What the harness sees

```bash
OPENAI_BASE_URL=http://mesh.sam.alt/v1   # inference, provider chosen by policy
MCP_URL=http://mesh.sam.alt/mcp          # tools: local catalog + remote services
```

No `HTTP_PROXY`. No CA bundle, unless a domain is explicitly configured for
secret injection. No mesh concepts, and no token of any kind.
`cmd/chaos-agent` already takes `--mcp-url` and `--inference-url`, so it needs
no change beyond the values it is given.

A harness that wants one specific service instead of policy-chosen routing uses
its mesh name directly: `http://code-reviewer.mcp.sam.alt/`.

### 5.2 Gateway dispatch

Applied to the SOCKS5 request's `(host, port)` — `host` is the name
`tun2socks` preserved, never an IP — in order:

1. **`mesh.sam.alt`** (`api.IsMeshEntrypointHost`) → the gateway's own
   agent-facing surface, served in process: `/v1/models`,
   `/v1/chat/completions`, `/v1/completions`, and `/mcp`. Nothing else. Any
   other path is refused with 403 and never reaches the node.

   **The agent never touches the node.** A `sam-node` sidecar is a local,
   operator-facing API: it can register services under the node's identity, and
   its `/sam/<peer>/<type>/<name>` proxy will carry a request to any peer and
   service the caller names. Worse, arriving on its Unix socket *is* the
   credential — `withAuth` treats reaching the socket as proof of authorization,
   on the grounds that it is the same bar as reading the token file. So piping
   an agent's bytes to that socket would hand every sandbox the node's full
   local authority. `sam-box` is the node's consumer; the agent is the mesh's
   consumer, through `sam-box`, and the two are different surfaces.

   Discovery is deliberately absent from the list even though agents need it:
   MCP already exposes `find_remote_tools` and `discover_remote_services` as
   tools, so opening `/sam/service/discover` as well would widen the surface
   without adding a capability.
2. **`<service>.<type>.sam.alt`** (`api.ParseMeshHost`) → `sam-box` terminates
   HTTP and rewrites onto the sidecar's existing surface:
   `GET /sam/service/discover?type=<type>&name=<service>` picks a provider, then
   the request is reverse-proxied to `/sam/<peer>/<type>/<service>/<path>`. No
   new sidecar endpoint, no libp2p in `sam-box`.
3. **anything else** → egress policy (§5.3). Allowed flows are dialled directly;
   flows on a secret-injection domain are terminated with the ephemeral CA.
4. **no match, or a name in the zone that resolves to nothing** → SOCKS5 reply
   `0x02`.

Case 2 is the only one that inspects payload, and only because provider
selection is an HTTP-level decision. `https://` to a mesh name therefore needs
the ephemeral CA trusted in the guest; v1 documents mesh names as `http://`
(the transport underneath is libp2p, already authenticated and encrypted) and
leaves guest CA installation to sandbox image build rather than a bootstrap
endpoint.

### 5.3 Egress policy

Deny-by-default, evaluated on the name, expressed in the node config next to the
existing `attenuation` block:

```yaml
version: v1
sandbox:
  egress:
    allow:
      - "api.github.com"
      - "*.pypi.org"
    secrets:
      "api.github.com":
        kind: bearer
        value_from: /etc/sam/secrets/github
```

Filtering by name without MITM is honest here: the gateway dials the name it
authorized and, for TLS, requires the client's SNI to match it.

## 6. Test plan

Coverage is pushed as far down the pyramid as it will go. Firecracker appears
exactly once, at the top.

**Unit (`internal/…`, `api/`).** SOCKS5 request/reply codec; the name↔URI
projection (`api/names_test.go`, already landed); dispatch classification
(`mesh.sam.alt` / mesh name / public / denied); the agent-facing path allowlist;
wildcard matching in the egress allowlist; SNI-vs-authorized-name mismatch.
Pure functions, microseconds.

**Integration (`tests/integration/`, budget 10s each).** The whole datapath, no
containers:

* node A registers a fake OpenAI-compatible backend — reuse the
  `registerInferenceService` + `httptest` pattern already in
  `openai_facade_test.go`;
* node B registers a fake MCP service — reuse `newFakeMCPHandler`;
* node C runs with a sidecar Unix socket, and a `sam-box` beside it serves
  SOCKS5 on a second socket in `t.TempDir()`;
* the test drives a real SOCKS5 client (`golang.org/x/net/proxy`, already a
  direct dependency) over that socket and asserts:
  1. `mesh.sam.alt:80` → `/v1/chat/completions` reaches A's fake LLM across the mesh,
  2. `mesh.sam.alt:80` → `/mcp` `call_remote_tool` reaches B's fake MCP,
  3. `<svc>.mcp.sam.alt:80` resolves through discovery to B and reaches it,
  4. an allowlisted public name reaches a local `httptest` server,
  5. a non-allowlisted name is refused with SOCKS5 `0x02`,
  6. no path outside `/v1/*` and `/mcp` reaches node C at all.

**E2E (`tests/e2e/*.bats`).** Two CUJs, no more:

* *Container sandbox*: `docker run --network none` with the real harness plus
  `tun2socks`, a bind-mounted UDS to a host `sam-node`, against the existing bats
  mesh (`lib/container_mesh.bash`). Asserts a real inference call, a real tool
  call, and one blocked domain.
* *microVM sandbox*: the same rootfs under Firecracker, `skip` unless `/dev/kvm`
  is present. It exists to prove the vsock↔UDS mapping and nothing else; every
  behavioural assertion is already covered above.

The draft `tests/e2e/agent_scenarios.bats` is superseded: its scenario 1 asserts
`/v1/models` over the sidecar UDS (an integration-level fact) and its scenario 2
depends on live `api.github.com` reachability (a flake).

## 7. What gets deleted

* `cmd/nano-init`'s DNS spoofer, `/etc/resolv.conf` rewrite, `HTTP_PROXY`
  injection and interceptor bootstrap. What remains worth keeping is PID 1
  hygiene — zombie reaping, signal propagation, exit-code passthrough — plus, if
  we own the guest side in Go, `tun0` setup.
* `internal/sambox`'s `/internal/bootstrap/libinterceptor.so` endpoint and the
  `--interceptors-dir` flag.
* `sam-box`'s entire mesh half: `join`, OIDC/device-code enrollment, the bbolt
  store, `node.SamNode` construction, relay/bootstrap/router flags. It keeps
  `run`, the sandbox socket, the egress policy and the ephemeral CA, and gains
  `--sidecar-socket` + a token.
* The contradictory URL/proxy wiring in `scripts/microvm-init.sh`.

## 8. Migration order

1. Land the SOCKS5 server and dispatch in `internal/sambox`, with unit +
   integration coverage, behind the existing `sam-box run -u`. Nothing else
   changes yet.
2. Strip `sam-box`'s mesh half; it becomes a client of the `sam-node` sidecar
   socket.
3. Point `scripts/microvm-init.sh` and the container path at it; fix the
   `<uds>_<port>` mapping. Add the two bats CUJs.
4. Strip `nano-init` to PID 1 duties + `tun0`.
5. *Then* resume scale testing, on a datapath whose failure modes are known.

## 9. Dependencies

Nothing on the host requires a `go.mod` change:

* SOCKS5 **server** — a few hundred lines against the stdlib; we implement only
  `CONNECT` with `NO AUTH` (RFC 1928) on a Unix socket.
* SOCKS5 **client** (tests) — `golang.org/x/net/proxy`, already a direct dep.
* AF_VSOCK — never touched by Go code (Decision 3).
* MITM CA and secret injection — `internal/sambox`, already ours.
* Mesh name projection — `api/names.go`, stdlib only.

### 9.1 The guest-side tun2socks — decided, with an exit

**Decision: keep the external binary.** `scripts/build-rootfs.sh` downloads a
release of the Rust `tun2proxy` into the guest rootfs. It is a guest-image
artifact, not a Go dependency, so `go.mod` stays clean and the host binaries stay
unaffected.

What we accept by doing so:

* a third-party release URL is pinned into the image build. It **must** be pinned
  by SHA-256 and verified after download — today the script does neither, which
  is a supply-chain hole in an artifact that sees every byte the agent sends;
* a second language toolchain in the sandbox image;
* the guest datapath is not covered by `make test`, only by the bats CUJ.

What would trigger the switch to an in-guest Go implementation:

* needing to make policy or telemetry decisions *inside* the guest (per-process
  attribution, for instance) — `tun2proxy` cannot be extended from here;
* needing architectures the project does not publish binaries for;
* the release URL becoming unavailable or unverifiable.

That switch means a userspace TCP/IP stack — `gvisor.dev/gvisor/pkg/tcpip` is
the realistic option — which is a **new dependency requiring explicit
approval**, and a large one. It is not blocked by anything in this design: the
sandbox boundary is SOCKS5 over a byte stream either way, so replacing the guest
implementation changes nothing on the host and no test above needs rewriting.
The guest-side code carries this note inline so the trade-off is visible where
the decision would be made.

## 10. Agent identity

### 10.1 Requirements

1. **Portable.** An agent paused on node A and resumed on node B is the same
   principal. Identity moves with the agent's state, never with the host.
2. **Verifiable mesh-wide.** Any peer can decide "agent `foo` may use
   `inference://X`, agent `bar` may not" without asking anyone.
3. **Scales to 10⁹ agents and services.** No central per-agent record, no
   per-agent mint on the hot path, no enumeration in any policy document, no
   revocation list proportional to the agent population.

### 10.2 The library constraint that decides the shape

`biscuit-go/v2 v2.2.0` has **no third-party blocks**: the library's own sample
suite explicitly skips `test024_third_party.bc`, and the API exposes no
public-key (`trusting`) checks. So an agent-signed block cannot be embedded
inside the node's token.

What the library *does* give us is exactly enough:

* `New` — mint under a root key;
* `CreateBlock` + `Append` — **offline attenuation**, no root key, no network;
* multi-root verification (`VerifyBiscuit(..., trustedPublicKeys, ...)`);
* `RevocationIds()` — per-block revocation identifiers;
* `Seal` — stop further attenuation.

Hence the agent's facts travel as an ordinary appended block on the node token
(Decision 5). This is not a workaround — it is why **`Authorize` needs no change
at all**: appended blocks are already part of the token and already evaluated,
so only the component that admits the agent has to learn to append. If
`biscuit-go` later gains third-party blocks, the agent can sign its own block
and non-repudiation arrives without a wire break.

> **Correction (falsified by test).** The paragraph above was wrong, and
> `TestAttenuationBlockFactsAreInvisibleToTheAuthorizer` in `internal/identity`
> now pins why. `Authorize()` merges **only the authority block's** facts and
> rules into the authorizer's world; every appended block is evaluated in a
> world of its own, so its facts can satisfy that block's own checks and
> nothing else. A fact appended by a holder is invisible to the far end's
> policy.
>
> That is deliberate and correct: if appending could add facts the authorizer
> sees, attenuation would be able to *grant* authority instead of only
> narrowing it, and any bearer could promote itself.
>
> The consequence is that "append a block naming the agent" cannot work, so the
> claim has to travel beside the token instead — and it is worth being precise
> that this costs nothing cryptographically. Whoever can append a block can
> append *any* block, so a node's claim about which agent it is speaking for is
> worth exactly what the node is worth either way (Decision 5's residual
> trust, bounded by binding namespaces to node attestation). Only a block
> signed by the agent itself would change that, and that needs third-party
> blocks.
>
> Options, to be settled before the node side is built:
>
> 1. **Propagate `X-Sam-Agent` peer to peer** and have the receiving node inject
>    `agent(...)` into its authorizer, exactly as `injectIdentityFacts` already
>    injects facts the node derived itself. Simple, and honest about the trust
>    involved.
> 2. **Append the block anyway and read it back out** of the token at the far
>    end, then inject the same fact. Identical trust, plus datalog text parsing,
>    since `biscuit-go` exposes no structured per-block fact accessor.
> 3. **Wait for third-party blocks**, which would make the agent's own signature
>    the thing being verified, and is the only option that adds real
>    non-repudiation.
>
> **Decided: option 1**, implemented in `internal/node/agent.go`.

### 10.2.1 What the agent claim covers, and what it does not

**Covers.** Attribution and policy on **both** datapaths. A peer can authorize
and audit "agent `reviewer-7` called me" rather than only "some node did", using
the existing vocabulary, because the claim is injected as an ordinary `agent()`
fact. `TestAgentPolicyCUJ` shows two agents behind two gateways on one node
being told apart by the provider's policy — over HTTP for inference and over the
libp2p stream for tool calls — including a lookalike authority being refused and
an unidentified sandbox being refused.

The two paths carry it differently because they authenticate differently: HTTP
requests carry `X-Sam-Agent`, and streams carry `AuthFrame.agent`. On the stream
path the claim is bound to the MCP *session* rather than the request, since the
SDK hands a tool handler the session's context and not the request's. That fits
how sandboxes work anyway: one gateway serves one agent, so a session belongs to
one agent for its whole life.

**Does not cover.**

* **Proof.** The claim is the calling node's word. A node that lies can name any
  agent, so a mesh that cares must also constrain which peers may speak for
  which agent namespaces. Carrying it in a header rather than a block costs
  nothing here: an appended block is exactly as forgeable by the same party.
* **Anything that never leaves the node**, which the boundary already gates
  locally.

**A consequence to design around.** A node's own housekeeping carries no agent,
because no agent asked for it. A provider whose policy demands an agent
unconditionally therefore also refuses that node's model-catalog probe, and its
models stop appearing in peers' `/v1/models` listings even though agents can
still call them. This was found by the test, not by reasoning. Policy that means
to gate agent traffic should say so rather than demanding an agent everywhere.

### 10.3 Why it scales: delegation, not enumeration

Three rules, each of which removes a per-agent bottleneck:

**Minting is offline and local.** The control plane never mints per agent. It
grants a *namespace holder* — in practice the enrolled `sam-node`/`sam-box` in a
cluster, scoped by its attestation (§10.6) — authority over an agent namespace,
once. That holder asserts a per-agent identity by appending a block to its own
token with `Append`: no root key, no round trip, no central state. Admitting an
agent is O(1) local work, which is the only way 10⁹ of them exist.

**Policy is written over namespaces, not identities.** This needs almost no new
machinery, because the existing vocabulary is already shaped for it:
`BuildTargetDatalogFacts` compiles targets into `granted_target_prefix`,
`granted_target_suffix`, `granted_target_set` and `granted_target_exact`. Making
`agent` a first-class principal is:

* `FactAgent = "agent"` in `api/datalog.go`;
* `FactAgent` added to `ValidMemberPrefixes` in `api/policy.go`;
* the agent block's `agent(...)` fact injected as a `target_fact` at
  authorization time.

After that, `allowed_targets: ["agent:acme/prod/*"]` works with **zero new
evaluation logic**, and "foo yes, bar no" is expressed as a namespace, a set or
a prefix — never as a list of a billion names.

**Revocation is tiered.** A revocation list sized like the agent population is
not a design, so nothing depends on one:

| what happened | mechanism | cost |
|---|---|---|
| agent is done or misbehaving | kill the sandbox | instant, local, free |
| credential must stop being usable elsewhere | short-lived workload credential; the platform stops renewing it | bounded window, no list |
| a namespace holder is compromised | `RevocationIds()` on its delegation block | one entry kills every agent under it |

### 10.4 How the identity travels

The agent's credential bundle lives **in the agent's own state**, next to
whatever else the platform migrates — not in `sam-box`'s configuration and not in
the node's store. Pause, snapshot, move, resume: `sam-box` on node B reads the
bundle from the migrated state and starts asserting it. **No control-plane call
on resume.** That is what makes migration cheap, and it is the same property
that makes 10⁹ agents possible.

### 10.5 Where the agent's identity comes from

Not from the agent. The harness stays unmodified (Goal 1), so it never asserts
anything about itself — it could only lie. It comes from the credential the
platform **already** issues to the workload:

* a **Kubernetes projected service-account token**, audience-scoped and
  short-lived;
* a **pod certificate** (KEP-4317) — which Substrate already provides today via
  its `podcertcontroller` polyfill;
* a **SPIFFE SVID**, where SPIRE is in play.

`sam-box` verifies that credential against the platform's issuer at admission —
in-cluster, cheap, no mesh round trip — and translates it into `agent:` facts by
the rules in §10.8. **The scheduler needs no mesh enrollment and no mesh
credential of its own.** It keeps doing exactly what it already does for every
workload: project an identity document. This is the OIDC relationship, not a
merger of the two domains.

`sam-box` then binds that identity to the channel, so nothing has to be asserted
in-band afterwards:

1. **One socket per agent (default).** Identity is a property of the socket,
   fixed when the sandbox is created: one vsock UDS per microVM, one bind-mounted
   socket per container. Nothing in-band, nothing to spoof.
2. **SOCKS5 username (multiplexing).** When one `sam-box` fronts many agents over
   one socket, the RFC 1929 username/password sub-negotiation carries the agent
   id and its admission token. It is standard, in-band, supported by every SOCKS
   client and by `tun2proxy` (`socks5://user:pass@host`), and it makes identity
   **per-connection** — which is exactly what a multiplexer needs.

### 10.6 The honest limit

An enrolled node asserts its own agents, so a compromised node can assert any
agent identity **within the namespaces its own attestation permits**. That bound
is the mitigation, and it is enforced by the same policy machinery as everything
else; without the namespace binding, a single compromised node could impersonate
any agent anywhere, which is why it is not optional.

Removing the residue entirely requires proof of possession: the agent holds a
key and signs a per-request binding. That needs either a mesh-aware harness
(violates Goal 1) or a signer inside the sandbox, and it needs third-party
blocks in `biscuit-go` to be attributable. It is a clean upgrade, not a rewrite.

### 10.7 Consequences for the plan

* `api/` — `FactAgent`, `ValidMemberPrefixes`, the §10.8 translation helpers, and
  the §12 bundle schema.
* `internal/node` — **nothing** for authorization: appended blocks are already
  verified and evaluated. Only the namespace-binding policy is new.
* `internal/sambox` — holds the bundle, attaches it per flow, per socket or per
  SOCKS5 connection; serves the §12 control socket and the §11 ingress endpoint.
* `cmd/nano-init` — owns the guest side of the ingress channel (§11.2).
* Sidecar — propagates the agent header on egress alongside `X-Sam-Biscuit`.
* Tests — the CUJ that demonstrates the selling point belongs at integration
  level: two agents behind one `sam-box`, same node, same mesh; `foo` reaches
  `inference://X` and `bar` is refused, purely on their own credentials.

### 10.8 The agent identifier

**Decided: dot-separated, DNS-shaped, most-specific-first.**
`agent:reviewer-7.prod.acme.example`, matched by `agent:*.prod.acme.example`.

The policy engine chooses this for us. `BuildTargetDatalogFact` compiles
`*.acme.example` into a suffix fact that **keeps the leading dot**
(`.acme.example`) and `acme.*` into a prefix fact that **keeps the trailing dot**
(`acme.`). Wildcards are therefore already anchored on dot boundaries:
`evil-acme.example` cannot match `*.acme.example`. A slash-separated
`agent:acme/prod/name` would need new fact types and new matching code, and
would reintroduce the boundary bug the dot anchoring already avoids. It also
matches every other name in the system: service names are validated by
`dnsNameRegex`, and mesh hosts are DNS-shaped (§Decision 4).

**Is it a mesh-only convention? Yes — and that is fine, for exactly the reason
you gave.** It is the same relationship the mesh already has with OIDC:
`translateClaimsToFacts` turns `sub`, `email` and `groups` into `user`, `email`
and `group` facts, and policy is written against *those*, never against the raw
JWT. Third parties do not adopt `agent:`; connectors translate into it.

A connector's translation must satisfy four properties, and this is the part
worth writing down because "without losing capabilities" lives here:

1. **Total** — every foreign identity maps to exactly one mesh identifier.
2. **Injective** — two foreign identities never collide, or policy leaks across
   tenants. The rightmost labels must be an authority the platform actually
   controls.
3. **Hierarchy-preserving** — foreign hierarchy boundaries must land on dot
   boundaries. This is the capability-preserving requirement: if the connector
   flattens the hierarchy, wildcard policies stop being expressible and the
   operator is forced back to enumeration.
4. **Auditable** — the original identifier travels verbatim alongside the
   translated one. Where translation cannot round-trip, the verbatim value is
   what an auditor reads.

| foreign identity | mesh identifier |
|---|---|
| `spiffe://acme.example/prod/reviewer-7` | `agent:reviewer-7.prod.acme.example` |
| K8s SA `prod/reviewer` in cluster `c1.acme.example` | `agent:reviewer.prod.c1.acme.example` |
| OIDC `iss=https://acme.example`, `sub=reviewer-7` | `agent:reviewer-7.acme.example` |

Each segment must be a valid DNS label; a connector encodes anything else.
Non-hierarchical attributes do **not** belong in the name — that is what the
attested label machinery (`api/labels.go`) is for.

## 11. Ingress: agents that serve

An agent must be able to say "route traffic for this name to me". Two things
make this smaller than it looks.

**It is not a new namespace.** `code-reviewer.mcp.sam.alt` is already the name
(Decision 4); serving it just means being its provider. And **the registration
API already exists**: `RegisterServiceRequest{service{type,name,description},
target_url}` on `/sam/service/register`.

### 11.1 Declaring: autoregistration through the gateway

The agent does **not** call the node. `/sam/service/register` is part of the
operator surface an agent never reaches (§5.2), for two independent reasons: the
request carries a `target_url`, so an agent could point the mesh at anything and
turn its node into an open relay; and it carries a service *name*, so an agent
could advertise itself as `code-reviewer` under the node's identity and take
over somebody else's traffic.

Both are properties of *that* API, not of the capability. So the gateway offers
the capability without the API:

```http
POST http://mesh.sam.alt/ingress
{"type": "mcp", "name": "code-reviewer", "port": 8080}
```

`sam-box` serves this itself — it is never forwarded — and then registers with
the node on the agent's behalf. Two things make that safe:

* **`target_url` is not the agent's to give.** `sam-box` always substitutes its
  own per-agent ingress endpoint, so the field an agent could abuse is one it
  never supplies.
* **The name must be one the agent already had.** Either the bundle enumerated
  it (§12.1 `ingress`), or it falls inside the agent's own namespace — derived
  from its identifier, which is authority-anchored and collision-free by
  construction (§10.8). Squatting a name the platform did not grant is therefore
  not expressible, rather than merely rejected.

### 11.1.1 Why this is also the better UX

This is not only the safe shape, it is the one that matches how agents actually
start:

* **The platform knows *what* an agent may serve; only the agent knows *when*.**
  A statically declared route advertises a service that is not listening yet, so
  the mesh routes to it and fails. Splitting the declaration from the readiness
  signal removes that window without a health-check protocol.
* **The port is usually the agent's own business**, chosen at runtime by the
  framework it happens to use.
* **Lifetime is tied to the sandbox.** `sam-box` unregisters when the agent's
  channel drops or the platform detaches it, so a suspended agent stops being
  advertised without anyone reconciling anything.
* **Resume is automatic.** On another host, the new `sam-box` re-registers the
  same name against a different peer, and discovery re-points. That is exactly
  the property §10 exists to provide, and it would be lost if routes lived in
  static platform config.

The bundle stays the authority (what may be served), and the agent supplies only
liveness and a port. An agent that declares nothing serves nothing.

Still to fix before this is built: whether an agent may re-register under a
changed port (flapping needs a rate limit), and whether `sam-box` should probe
the port before advertising rather than trusting the agent's claim.

### 11.2 The reverse channel

Inbound is the one place the datapath is not egress-shaped:

```text
remote peer → libp2p → sam-node → sam-box ingress endpoint
                                        │
                                        ▼  one stream per inbound request
                                   sandbox boundary
                                        │
                                   nano-init accept loop
                                        │
                                   127.0.0.1:<port>  (the agent's listener)
```

So a sandbox has exactly two channels, one per direction:

| channel | direction | protocol | who listens |
|---|---|---|---|
| egress | guest → host | SOCKS5 | host (`sam-box`) |
| ingress | host → guest | one stream per request | guest (`nano-init`) |

Firecracker's vsock is bidirectional, so host→guest needs no new transport: the
host connects to the firecracker UDS and writes `CONNECT <port>`, with the guest
listening on that vsock port. A container gets the symmetric arrangement with a
second bind-mounted socket. This is also what finally justifies `nano-init`
beyond PID 1 hygiene: it owns the guest side of the ingress channel.

### 11.3 Authorization closes the loop

Nothing new is needed. `sam-node` authorizes the **caller** — their node token
carrying their agent block (Decision 5) — before the stream ever reaches
`sam-box`. "Agent `foo` may call agent `bar`" is therefore one policy statement,
evaluated at `bar`'s node, with the existing machinery, and both ends are named
by portable identities rather than by hosts.

### 11.4 Migration

The ingress declaration is part of what travels (§10.4). On resume, `sam-box` on
node B registers the same name and discovery re-points to node B. The name is
stable because it belongs to the agent, not to the node — which is the whole
point of §10.

## 12. The platform integration interface

This is the seam a scheduler or harness connector builds against. It must be
small, versioned, and the only place identity enters the system.

### 12.1 The agent bundle

What the platform provides per agent. It lives in the agent's state directory, so
it migrates with the agent by construction:

```yaml
version: v1
agent:
  id: reviewer-7.prod.acme.example          # canonical mesh identifier (§10.8)
  external_id: spiffe://acme.example/prod/reviewer-7   # verbatim, for audit
  credential: /var/run/secrets/substrate/token  # the platform's own workload
                                                # credential, verified at
                                                # admission (§10.5)
egress:
  allow: ["api.github.com", "*.pypi.org"]
  secrets:
    "api.github.com": {kind: bearer, value_from: /etc/sam/secrets/github}
ingress:
  - {name: code-reviewer, type: mcp, port: 8080}
```

### 12.2 Operations

Exposed by `sam-box` on a control socket that is **not** reachable from inside
the sandbox:

| operation | meaning |
|---|---|
| `Attach(bundle)` | admit an agent; returns the egress and ingress endpoints to wire into the sandbox |
| `Detach(agent_id)` | stop it: unregister ingress, close channels, drop credentials |
| `Refresh(agent_id, credential)` | hand in a rotated workload credential (§10.3) |
| `Status(agent_id)` | what is registered and connected, for the scheduler's reconcile loop |

### 12.3 Guarantees the interface owes a connector

1. **`Attach` is idempotent, keyed by agent id.** Resume after a crash or a
   migration is just another `Attach` — no special-case path.
2. **Identity never arrives in-band from the sandbox.** The platform is the sole
   identity source (§10.5).
3. **The agent id is stable across `Attach` on different hosts.** This is what
   makes migration invisible to the rest of the mesh.
4. **`Detach` is complete.** No residual advertisement; discovery converges.
5. **Versioned.** `version: v1`, and the schema lives in `api/`, so it is a real
   contract and not a convention.

### 12.4 The first connector: Agent Substrate

[Agent Substrate](https://github.com/agent-substrate/substrate) is the
integration target, and it lines up unusually well:

| Substrate | SAM |
|---|---|
| **Actor** (the agent) | the principal: `agent:<actor>.<atespace>.<cluster domain>` |
| **Atespace** (namespace) | the hierarchy label policy wildcards on |
| routing by Host `my-counter-1.demo.actors.resources.substrate.ate.dev` | already dot-separated and most-specific-first — the same shape as §10.8 |
| `podcertcontroller` (KEP-4317 pod certificates), projected SA tokens | the workload credential `sam-box` verifies at admission (§10.5) |
| `atelet` per-node DaemonSet; `ateom-gvisor` / `ateom-microvm` | where `sam-box` attaches: one sandbox, two channels |
| suspend/resume with full-state snapshot (RAM + filesystem) | carries the agent bundle with no extra work (§10.4) |
| `atenet` (DNS, Envoy routing, proxy sidecars, CONNECT) | the seam to negotiate: who owns egress |

The name mapping is a pure string translation with no capability loss, which is
the §10.8 test: the Host `my-counter-1.demo.actors.resources.substrate.ate.dev`
becomes `agent:my-counter-1.demo.actors.resources.substrate.ate.dev`, and
`agent:*.demo.actors.resources.substrate.ate.dev` means "every actor in atespace
`demo`" — injective, hierarchy-preserving, reversible.

Two things to settle **with** the Substrate side rather than assume:

* **Egress ownership.** `atenet` already supplies DNS, routing, proxy sidecars
  and CONNECT. The SOCKS5 boundary has to slot in as the sandbox's egress rather
  than compete with it. `sam-box` behind atenet's sidecar seam is the likely
  fit, but that is a conversation, not a decision to take unilaterally.
* **Ingress.** Substrate already routes inbound to actors by Host header. §11
  must reuse that path rather than build a parallel one: a SAM ingress
  declaration should end up as a Substrate route.

### 12.5 Still to settle

* **Wire format — decided.** Proto in `api/sam.proto` for the operations
  (`AgentAttach`/`AgentDetach`/`AgentRefresh`/`AgentStatus` request and response
  messages), YAML for the bundle. Substrate is Go, so proto costs its connector
  nothing; the bundle stays YAML because its canonical copy is a file in the
  agent's state directory, where it has to be readable and diffable by whoever
  operates the platform. `AgentBundle` in the proto is the transport mirror of
  that file, not a second source of truth.
* **Bulk pre-creation is deliberately not in v1** — premature until the semantics
  are proven. What v1 owes it is *room*: `Attach` takes a bundle and is
  idempotent per agent id, so `AttachBatch` is a purely additive operation
  later, and the offline attenuation in §10.3 already makes it possible with no
  protocol change.

## 13. Remaining smaller open items

1. **UDP.** Denied in v1. If an agent ever needs QUIC or real DNS, it returns as
   `UDP ASSOCIATE` under the same name-based policy.
2. **Guest trust of the ephemeral CA.** Baked into the sandbox image at build
   time, or re-introduce a bootstrap endpoint? v1 avoids the question by keeping
   mesh names on `http://` (§5.2).
3. **Peer-pinned mesh names.** Deferred until a DNS-safe peer encoding is chosen
   (Decision 4).
