---
title: "Sandbox Gateway Guide"
linkTitle: "Sandbox Gateway"
weight: 30
---

`sam-box` is the sandbox gateway: one per agent sandbox, serving the boundary an
agent's traffic leaves through. An unmodified agent reaches mesh inference and
tools by name, reaches allowlisted destinations on the internet, and reaches
nothing else.

For the reasoning behind the design, see
[Agent Architecture]({{< relref "/docs/agent-architecture" >}}).

---

## 1. What it is, and is not

`sam-box` holds **no libp2p host, no enrolment and no mesh identity**. It
consumes a local `sam-node` over that node's API socket, and offers the sandbox
a curated surface built on top of it.

```mermaid
sequenceDiagram
    participant Agent as Agent (inside the sandbox)
    participant Box as sam-box (sandbox boundary)
    participant Node as sam-node (mesh member)
    participant Mesh as The mesh / the internet

    Agent->>Box: SOCKS5 CONNECT mesh.sam.alt:80
    Box->>Box: Classify by name, apply policy
    Box->>Node: /v1/chat/completions over the node's API socket
    Node->>Mesh: Route to a provider, authorized by the node's Biscuit
    Mesh-->>Agent: Response
```

The split matters. A `sam-node`'s API can register services under the node's
identity and proxy to any peer, and reaching its socket is itself the
credential. So an agent never touches it: `sam-box` is the node's consumer, the
agent is the mesh's consumer through `sam-box`.

---

## 2. Running it

```bash
sam-box run \
  --socket=/var/run/sam/agent.sock \
  --sidecar-socket=/var/run/sam/node.sock \
  --egress-allow=api.github.com \
  --egress-allow='*.pypi.org'
```

| Flag | Meaning |
|---|---|
| `--socket` | The sandbox-facing socket. Created 0600: its permissions are the credential. |
| `--sidecar-socket` | The local `sam-node`'s API socket (`sam-node run --socket-path`). |
| `--egress-allow` | A destination outside the mesh the agent may reach. Repeatable. **Empty means nothing is reachable.** |

Egress entries are matched on the name the agent asked for, never on a resolved
address. A wildcard covers subdomains only: `*.pypi.org` matches
`files.pypi.org` but not `pypi.org`, and never `evilpypi.org`.

---

## 3. What the agent sees

```bash
OPENAI_BASE_URL=http://mesh.sam.alt/v1
MCP_URL=http://mesh.sam.alt/mcp
```

No proxy variables, no CA bundle, no token: the agent holds no credential at
all. `mesh.sam.alt` serves exactly four paths — `/v1/models`,
`/v1/chat/completions`, `/v1/completions` and `/mcp`. Anything else is refused
with 403 and never reaches the node.

A specific service can be addressed by its own name instead of letting policy
choose: `http://code-reviewer.mcp.sam.alt/`.

Destinations that policy refuses fail the SOCKS5 handshake, which the sandbox's
kernel turns into an ordinary connection refusal — so an agent sees "refused"
rather than a hang.

---

## 4. Connecting a sandbox to the socket

The sandbox has no network of its own. It runs `tun2socks` against a `tun`
device and points it at the boundary socket, so every flow leaves as SOCKS5
carrying the destination *name*:

| sandbox | how it reaches the socket |
|---|---|
| Firecracker microVM | vsock; firecracker terminates it as `<uds_path>_<port>` on the host |
| container with `network=none` | the socket is bind-mounted in |

For a quick test without a sandbox, bridge a TCP port to the socket and use any
SOCKS5 client:

```bash
socat TCP-LISTEN:1080,fork,reuseaddr UNIX-CONNECT:/var/run/sam/agent.sock &
curl --socks5-hostname 127.0.0.1:1080 http://mesh.sam.alt/v1/models
```

---

## 5. Not yet implemented

Deliberately absent, and tracked in the architecture document:

* **Secret injection** for external destinations. The ephemeral CA and the
  injection machinery exist, but are not wired into this datapath.
* **Ingress**: an agent serving a mesh service of its own.
* **Per-agent identity** on the wire. Today flows carry the node's identity;
  the agent's own identity is the next piece of work.
