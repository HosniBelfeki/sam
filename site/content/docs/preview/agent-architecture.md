---
title: "Agent architecture"
linkTitle: "Agent architecture"
weight: 2
aliases:
  - /docs/agent-architecture/
---

{{% alert title="Preview" color="warning" %}}
This is the design behind [sandboxed agents](../sandboxed-agents/). The
decisions here are settled; the interfaces that implement them are still
being shaped, and the last section lists what is not built yet.
{{% /alert %}}

This page explains how an unmodified agent, running inside a sandbox with no
network of its own, is connected to the mesh. It covers the layers, the
reasons for each layer, the contracts between them, and the limits of what
the design guarantees. The [scale report](../scale-report/) measures what
the result costs.

## Goals

1. **The harness is unmodified and does not know about the mesh.** It opens
   ordinary sockets, resolves ordinary names and speaks ordinary HTTP. It
   needs no SDK, no `LD_PRELOAD`, and no proxy variables.
2. **One enforcement point.** Every flow that the sandbox emits is authorized
   in exactly one place, by name, against one policy.
3. **The sandbox has no network.** This means no network device other than
   the one the design gives it, and not a filtered network. Isolation is a
   property of the runtime and not of a firewall rule.
4. **The mesh looks like the internet.** Reaching a mesh model or tool is the
   same operation as reaching `api.github.com`: connect to a name. Which
   names are internal to the mesh is a policy decision on the host, not a
   code path in the agent.
5. **No new dependencies on the host.** The guest-side network stack lives in
   its own Go module.

## Why one enforcement point

There are three common ways to intercept a sandbox's traffic at the
application layer. Each of them requires the agent to cooperate with its own
confinement:

- **Proxy environment variables** are honoured only by HTTP-aware clients. A
  raw socket, a library with `trust_env=False`, gRPC or Postgres bypass
  them.
- **DNS spoofing** forces every flow through an interception point and
  answers with loopback addresses. This destroys the destination name, which
  is what the policy engine needs to reason about.
- **An `LD_PRELOAD` shim** must be built for each architecture and libc, and
  a statically linked binary bypasses it.

An agent that has to agree to be confined is not confined. Routing does not
have this problem, because nothing has to agree to a route. So the boundary
is a route and a socket, and there is exactly one of them.

## The layering

```text
┌─ sandbox (microVM, or container with network=none) ────────────────┐
│  harness (unmodified: sockets, DNS, HTTP)                          │
│      │ IP                                                          │
│    tun0 ── default route, the only device besides lo               │
│      │                                                             │
│  nano-init / tun2connect ── IP flows → CONNECT (TCP), connect-udp  │
│      │                     destination kept as a NAME (virtual DNS)│
└──────┼─────────────────────────────────────────────────────────────┘
       │ one byte stream per flow
       │ microVM:   AF_VSOCK → firecracker → host UDS <path>_<port>
       │ container: bind-mounted UDS
┌──────┼─────────────────────────────────────────────────────────────┐
│ host ▼                                                             │
│  sam-box (one per sandbox; no libp2p host, no enrollment)          │
│    = CONNECT server = the policy enforcement point                 │
│      ├── mesh.sam.alt           → /v1/* and /mcp only ──┐          │
│      ├── <svc>.<type>.sam.alt   → discover, then        ├─▶ sam-node
│      │                            /sam/<peer>/<type>/…  │   (UDS)  │
│      └── anything else          → deny-by-default egress → internet│
└────────────────────────────────────────────────────────────────────┘
```

### Named HTTP tunnels are the boundary protocol

The boundary speaks authority-form `CONNECT` (RFC 9110) for TCP and
connect-udp (RFC 9298, with capsules as in RFC 9297) for UDP, over a Unix
socket. Four properties decided this choice over the alternatives (an earlier
design used SOCKS5):

- **It carries the destination name.** `CONNECT api.github.com:443`
  authorizes `api.github.com`, and not an address. connect-udp carries the
  name in its URI template. No DNS interception is needed, and there are no
  IP allowlists that go stale.
- **It is protocol-agnostic.** After `200 OK` the tunnel is a byte pipe.
  HTTP is the handshake, and the payload can be any protocol.
- **It has a vocabulary for refusals.** A denial is `403` with a
  `Boundary-Reason` header, which the guest stack turns into `ECONNREFUSED`.
  The agent sees a refused connection, and the operator reads the reason in
  words.
- **Headers are the extension point, and one grammar serves both
  directions.** Ingress into a sandbox is `CONNECT <port>`. Sandbox identity
  for a boundary that serves many agents rides in `Proxy-Authorization`. UDP
  fits on the same stream as capsules. And `curl --proxy` speaks the TCP
  half, so the boundary can be tested with a tool that every operator has.

### Names are resolved on the host, never in the guest

The guest's virtual DNS answers every lookup with a synthetic address from
pools that it invents: IPv4 from `100.64.0.0/10` (CGNAT space; link-local
addresses would be safer against leaks, but SSRF guards in HTTP clients
commonly block `169.254/16`), and IPv6 from `100::/64` (the RFC 6666 discard
prefix, so a packet that escapes is dropped). The engine maps the address
back to the name when the flow opens, and the boundary receives the name. A
flow to an address that was never resolved has no name and is refused in the
guest. There is no resolver path out of the sandbox, so there is no DNS
exfiltration either.

### The host never speaks AF_VSOCK

Firecracker's vsock device terminates `AF_VSOCK` and gives the host a Unix
socket per guest port, named `<uds_path>_<port>`. The boundary therefore
always listens on a Unix socket, and a microVM and a `network=none`
container are the same code path:

| Sandbox | Guest side | Host side |
|---|---|---|
| Firecracker microVM | vsock CID 2, port *P* | listener on `<uds>_P` |
| container, `network=none` | `/run/sam/agent.sock` | listener on the bind-mounted path |

This keeps vsock out of the Go code. More importantly, it makes the whole
datapath testable in a Go integration test against a socket in a temporary
directory, without a VM.

### A mesh name is the service namespace in DNS shape

The mesh's namespace is `mcp://<service>`, `inference://<service>`,
`a2a://<service>` and `system://sam.catalog`. Sandbox names are a projection
of it, declared once in `api/names.go` so that the boundary and the node
cannot drift apart:

```text
inference://openrouter   <->   openrouter.inference.sam.alt
mcp://code-reviewer      <->   code-reviewer.mcp.sam.alt
(the mesh, curated)      <->   mesh.sam.alt
```

`.alt` is the pseudo-TLD that RFC 9476 reserves for names that are not
resolved through the DNS. It cannot collide with a delegated TLD, and a name
that escapes the sandbox fails closed. `.local` belongs to mDNS and was
rejected.

Provider selection is not part of the name. `openrouter.inference.sam.alt`
says which service, never which peer. Pinning a peer would need a DNS-safe
encoding of peer IDs (base58 is case-sensitive, DNS labels are not) and is
deferred.

### The agent is the principal, and the node is the channel

Policy has to be able to say "agent `foo` may use `inference://X`, agent
`bar` may not". The whole mesh has to be able to evaluate that statement, and
it has to survive the agent being paused on one host and resumed on another.
So agent identity travels with the agent and never with the host.

A credential is bound to a peer ID, and a verifier checks that binding on
every connection. That is right for the channel and wrong for the question
"who is asking". The mesh already takes in a foreign identity once, at node
enrollment, by verifying an OIDC token and translating claims into facts.
Agents reuse that pattern one level down:

| | Asserted by | Verified against | When | On the datapath as |
|---|---|---|---|---|
| node identity | control plane | the OIDC issuer | enrollment | a credential |
| agent identity | the `sam-box` that admitted the agent | the platform's workload issuer | admission | a claim next to the credential |

The claim travels next to the credential (`X-Sam-Agent` on HTTP, `agent` in
the stream authentication frame), and not inside it. Biscuit lets a holder
append blocks to a token, but a verifier evaluates the facts of an appended
block only against that block's own checks, never against the authorizer's
policy. This is intentional, because otherwise attenuation could grant
instead of only narrow. `TestAttenuationBlockFactsAreInvisibleToTheAuthorizer`
in `internal/identity` pins this behaviour. Carrying the claim in a header
costs nothing cryptographically. Whoever can append a block can append any
block, so a node's claim about which agent it speaks for is worth exactly
what the node is worth, in either form.

The remaining trust is this: an enrolled node asserts its own agents. Mesh
policy limits this with the role's `allowed_agents`, the namespaces that a
node may name. A node with no grant can name no agent. The grant is keyed on
the node's role, which is resolved from verified claims against bindings
that an administrator wrote. It is not keyed on the node's labels, which the
node declares itself. Proof of possession by the agent would remove the
remaining trust. It needs a signer inside the sandbox and third-party blocks
in the Biscuit library, and it can be added later without a rewrite.

## Where the boundary lives

Two binaries with separate jobs. **`sam-node`** owns the libp2p host,
enrollment, identity, discovery and the local API. **`sam-box`**, one per
sandbox, owns none of those. It serves the tunnel boundary, enforces egress
policy, and reaches the mesh only by dialling the node's API socket as a
client.

This split has several benefits. *N* sandboxes on a host share one libp2p
host, so there are not *N* DHT clients, identify loops or router
connections, which would otherwise limit density. `sam-box` is small: a
CONNECT server and an HTTP client. A compromised sandbox affects only its
own boundary, which can be killed and started again on its own. And the
agent never touches the node's API socket. That matters because reaching
the socket is itself a credential: the socket can proxy to any peer and
service that the node's grants allow. `sam-box` is a client of the node. The
agent is a client of the mesh, through `sam-box`.

## Contracts

### What the harness sees

```text
OPENAI_BASE_URL=http://mesh.sam.alt/v1
MCP_URL=http://mesh.sam.alt/mcp
```

There is no proxy variable, no CA bundle and no token. A harness that wants
one specific service instead of policy-chosen routing uses the service's
mesh name: `http://code-reviewer.mcp.sam.alt/`. Mesh names use `http://` on
purpose. The transport underneath is libp2p, which is already authenticated
and encrypted, and plain HTTP avoids installing a CA in the guest.

### Dispatch

The boundary applies these rules to the tunnel's `(host, port)`, where
`host` is the preserved name, in this order:

1. **`mesh.sam.alt`**: the boundary's own surface, served in process:
   `/v1/models`, `/v1/chat/completions`, `/v1/completions`, `/mcp`. Any
   other path is `403` and never reaches the node. Discovery is not exposed
   here because MCP already provides it as tools.
2. **`<service>.<type>.sam.alt`**: the boundary terminates HTTP, asks the
   node's `/sam/service/discover` for a provider, and reverse-proxies to
   `/sam/<peer>/<type>/<service>/<path>`.
3. **Anything else**: the egress allowance from the bundle (or from
   `--egress-allow` for an unidentified sandbox). Allowed flows are dialled
   directly, by the name that was authorized. For TLS, the client's SNI must
   match that name.
4. **No match**: `403 Boundary-Reason: not allowed by policy`.

Only case 2 inspects the payload, and only because provider selection is an
HTTP-level decision.

### What a sandbox must provide

`nano-init` builds the route out of a sandbox. It does not build the sandbox
itself. Three runtimes are supported, and each must satisfy the same five
preconditions:

| Precondition | `docker --network none` | Firecracker | Kubernetes pod |
|---|---|---|---|
| a network namespace with no way out | the runtime | the guest kernel, given no NIC | **nothing provides it**; `nano-init --create-namespaces` builds it |
| a private `resolv.conf` | the container's mount namespace | the guest rootfs | **nothing provides it**; mount one per container |
| a Unix socket to the boundary | bind mount | vsock, exposed on the host as `<uds>_<port>` | shared `emptyDir` |
| a way to create a TUN | `--device /dev/net/tun --cap-add NET_ADMIN` | `CONFIG_TUN` in the guest kernel | `hostPath` of type `CharDevice`, no capability |
| PID 1 | the entrypoint | `/sbin/init` | the entrypoint |

A pod shares one network namespace and one `resolv.conf` across its
containers, so the sandbox has to create both for itself. It first creates a
user namespace, in which it holds `CAP_SYS_ADMIN` and `CAP_NET_ADMIN` over
what it creates next, so the container needs no capability. Granting
`CAP_SYS_ADMIN` alone is worse than granting nothing: the namespace is
created, and then the tun fails.

This failure produces no error (`tun0` next to `eth0` is a working network),
so `nano-init` refuses to start in a namespace that has any interface other
than loopback and its own tun, whichever runtime is in use.

## Agent identity

### Requirements

1. **Portable.** An agent paused on node A and resumed on node B is the same
   principal.
2. **Verifiable across the mesh.** Any peer can decide "agent `foo` may,
   agent `bar` may not" without asking anyone.
3. **Scalable.** There is no central per-agent record, no per-agent mint on
   the hot path, no list of agents in any policy, and no revocation list
   that grows with the agent population.

### Why it scales

**Admission is local.** The control plane never mints a credential per
agent. It grants a node's role authority over an agent namespace once. The
boundary then asserts a per-agent identity under that namespace, with no
round trip and no central state.

**Policy is written over namespaces.** The same machinery that compiles
`allowed_services` wildcards compiles `allowed_agents` into
`granted_agent_prefix`, `_suffix`, `_set` and `_exact`, and the destination
derives `agent_authorized()` from them. "Foo yes, bar no" is a namespace, a
set or a prefix, never a list of a billion names.

**Revocation has three levels.**

| What happened | Mechanism | Cost |
|---|---|---|
| the agent is done or misbehaving | kill the sandbox | instant, local |
| its credential must stop working elsewhere | the platform stops renewing the short-lived workload credential | a bounded window, no list |
| a node is compromised | ban the node | one entry ends every agent that it spoke for |

### Where identity comes from

Identity does not come from the agent, which is unmodified and could only
lie about itself. It comes from the credential that the platform already
issues to the workload: a Kubernetes projected service account token, a pod
certificate, a SPIFFE SVID. `sam-box` verifies the credential against the
issuer named by `--credential-issuer` and `--credential-audience` at
admission, which is cheap and stays inside the cluster. It accepts the bundle
only if the subject of the credential matches the bundle's `external_id`.
The subject check is the important one. Every sandbox on a platform holds a
valid credential, so a signature alone would let any sandbox claim to be any
other. The issuer is a flag and not a bundle field, because the bundle
travels with the agent.

Verification is the default. `--insecure-unverified-bundle` runs without it,
and the flag is visible in a process listing.

The identity is then bound to the channel, so nothing is asserted in-band.
By default there is one socket per agent, fixed when the sandbox is created.
A boundary that serves many agents over one socket takes the agent id per
connection in `Proxy-Authorization` Basic credentials.

### The bundle

What the platform provides per agent. It lives in the agent's own state
directory, so it migrates with the agent:

```yaml
version: v1
agent:
  id: reviewer-7.prod.acme.example                    # canonical mesh identifier
  external_id: spiffe://acme.example/prod/reviewer-7  # verbatim, for audit and the subject check
  credential: /var/run/secrets/platform/token         # the platform's workload credential
egress:
  allow: ["api.github.com", "*.pypi.org"]
serves:                                               # optional: the agent's own A2A service
  name: code-reviewer
  port: 8080
```

Parsing is strict. An unknown field is an error, because a typo that parsed
as "absent" would give an agent broader access than intended.

### The identifier

The identifier is dot-separated and DNS-shaped, with the most specific label
first: `reviewer-7.prod.acme.example`, matched by `*.prod.acme.example`. The
policy engine decided this shape. Wildcards compile to facts that keep their
anchoring dot, so `evil-acme.example` cannot match `*.acme.example`, and
every other name in the system (services, mesh hosts) has the same shape.

The identifier is a mesh convention, in the same way that `user`, `email`
and `group` are. External systems do not adopt it. A connector translates
into it, and the translation must be:

1. **Total**: every foreign identity maps to exactly one identifier.
2. **Injective**: no two foreign identities collide. The rightmost labels
   must be an authority that the platform controls.
3. **Hierarchy-preserving**: boundaries in the foreign hierarchy land on
   dots, otherwise wildcard policy cannot express them.
4. **Auditable**: the original travels unchanged as `external_id`.
5. **Bounded**: the name includes a label for the boundary where the workload
   credential stops being valid, usually the cluster. Node grants are
   written against that label. A mapping without it forces tenant-wide
   grants.

| Foreign identity | Mesh identifier |
|---|---|
| `spiffe://acme.example/prod/reviewer-7` | `reviewer-7.prod.acme.example` |
| Kubernetes SA `prod/reviewer` in cluster `c1.acme.example` | `reviewer.prod.c1.acme.example` |
| OIDC `iss=https://acme.example`, `sub=reviewer-7` | `reviewer-7.acme.example` |

Attributes that are not hierarchical do not belong in the name. Attested
labels exist for those.

### What the claim covers

The claim covers attribution and policy on both datapaths. A provider can
authorize and audit "agent `reviewer-7` called me" over HTTP for inference
and over the libp2p stream for tools, using the ordinary `agent()` fact. On
the stream path the claim is bound to the MCP session instead of the
request. This fits, because one boundary serves one agent, so a session
belongs to one agent for its whole life.

The claim is not a proof (it is the node's word, limited by
`allowed_agents`), and it does not cover anything that never leaves the
node. One consequence to design around: a node's own housekeeping carries no
agent. A provider whose policy demands an agent unconditionally therefore
also refuses that node's model-catalog probe, and its models disappear from
the peers' `/v1/models` even though agents can still call them. Policy that
intends to gate agent traffic should say so explicitly, instead of demanding
an agent everywhere.

## Agents that serve

An agent serves exactly one thing: itself, over A2A. Tools and models are
operator workloads declared in a node's configuration. An agent is not a
generic backend.

Nothing is registered at run time. A runtime declaration carries a name, so
an agent could advertise itself as `code-reviewer` under the node's identity
and take over someone else's traffic. If it could also give a URL, it could
point the mesh anywhere. So the route is contracted before the agent runs:

- The bundle's `serves` names the service and the port (like `$PORT` on a
  serverless runtime).
- The node's configuration declares the `a2a` service with the boundary's
  ingress listener as its backend.
- The node probes the agent's card through the boundary before it
  advertises the service, and keeps probing.

Readiness is observed, never assumed. When the sandbox goes away, the probe
fails and discovery converges. On resume on another host, the same bundle
produces the same contract, and the probe points discovery at the new
location.

Inbound traffic is the one place where the datapath is not shaped like
egress. The boundary cannot dial the agent, because the agent's loopback is
inside its own namespace. So the way in mirrors the way out: a second Unix
socket, served from inside by `nano-init --ingress-socket` and dialled by
`sam-box --agent-ingress-socket`, with one stream per request
(`CONNECT <port>`). This is the same handshake that Firecracker's vsock
uses, so a microVM offers it without any change in the boundary.

| Channel | Direction | Protocol | Who listens |
|---|---|---|---|
| egress | guest → host | CONNECT, connect-udp | host (`sam-box`) |
| ingress | host → guest | `CONNECT <port>`, one stream per request | guest (`nano-init`) |

Authorization needs nothing new. The caller's node authorized the caller,
with its agent claim, before the stream reached the boundary. "Agent `foo`
may call agent `bar`" is one policy statement, evaluated at the node of
`bar`, with both ends named by portable identities.

## The platform integration interface

This is the interface that a scheduler builds against, and the only place
where identity enters. It is small and versioned. It consists of the bundle
above (YAML, with its canonical copy as a file in the agent's state) and
four operations (`Attach`, `Detach`, `Refresh`, `Status`) whose request and
response messages are defined in `api/sam.proto`.

The interface gives a connector these guarantees:

- `Attach` is idempotent by agent id, so resume after a crash or migration
  is another `Attach`.
- Identity never arrives in-band from the sandbox.
- The agent id is stable across `Attach` calls on different hosts.
- `Detach` is complete, and leaves no advertisement behind.
- The schema is versioned.

## Not built yet

- The control socket that serves the four operations. Today the bundle is a
  file read at start, and the lifecycle of the boundary is the lifecycle of
  the process.
- Credential rotation (`Refresh`). The credential is verified once at start.
- Secret injection for external destinations. The ephemeral CA and header
  rewriting exist in `internal/sambox` but are not on this datapath.
- Peer-pinned mesh names, which need a DNS-safe peer ID encoding.
- Bulk pre-creation of agents. `Attach` is idempotent per id, so an
  `AttachBatch` can be added later.

## Testing

Coverage sits as low in the test pyramid as possible. Unit tests cover the
tunnel grammar, the capsule codec, name projection, dispatch classification
and allowlist matching. Integration tests (`tests/integration/`, under ten
seconds each) run the whole datapath without containers: nodes with fake
backends, a `sam-box` on a socket in a temporary directory, real `CONNECT`
tunnels, and assertions that mesh names reach the right peer, that allowed
names reach a local server, that refused names get `403`, and that no path
outside `/v1/*` and `/mcp` reaches the node. Two end-to-end tests exist: a
`--network none` container with the real harness, and the same rootfs under
Firecracker. The second is skipped without `/dev/kvm`, and it proves only
the vsock mapping.
