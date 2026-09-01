---
title: "A2A Chat"
linkTitle: "A2A Chat"
weight: 30
---

Hold a multi-turn conversation with an [A2A](https://a2a-protocol.org/)
(Agent2Agent) agent hosted on a remote mesh node — using a **stock,
unmodified `a2a-sdk` client**. No SAM-specific client code: the mesh's
agent-card rewrite makes the standard SDK work as-is.

Source: [`development/examples/chat-a2a/`](https://github.com/google/sam/tree/main/development/examples/chat-a2a).

## The idea

A2A is the mesh's southbound agent-to-agent wire: an agent process speaking
A2A over HTTP registers on its node as a `type: a2a` service, and remote
peers reach it through the raw egress path
`/sam/{peer}/a2a/{service}/...` (see
[A2A Service Routing](../../user/node-configuration/#a2a-service-routing)).

The problem with proxying A2A naively is the **agent card**. A2A clients
bootstrap from `/.well-known/agent-card.json`, and the card contains the
agent's own interface URLs — addresses that are only reachable on the
provider's machine. A stock client would fetch the card through the mesh,
then try to talk to `http://127.0.0.1:7777/` and fail.

So the caller's node rewrites the card **in transit**: interface URLs are
pointed back at the mesh path the client fetched from, transports the mesh
cannot carry (gRPC needs its own end-to-end connection) are dropped, and
streaming is advertised off. The client follows the rewritten URLs like it
would for any A2A server — discovery, JSON-RPC `message/send`, and
`contextId` round-tripping all work unmodified, while the traffic actually
flows over libp2p between the nodes.

This example proves both halves:

- **Card rewrite** — the bundled REPL is a plain `a2a-sdk` client; it works
  only because the rewritten card sends it back through the mesh.
- **Conversation continuity** — the agent keeps one Gemini chat session per
  A2A `contextId`. Tell it your name, ask for it back two turns later: the
  answer shows the context survived the mesh hop. Each turn is still its own
  short-lived A2A **task** (`taskId` changes every turn, created and
  completed by each `message/send`); `contextId` is what persists and groups
  them into one conversation.

## The pieces

- **`chat` service** (`agent.py`) — a Gemini-backed A2A agent built on the
  Python `a2a-sdk`: serves its agent card, answers `message/send`, and holds
  history server-side, one Gemini chat session per `contextId`. The node
  proxies to it via `target_url`; the agent itself knows nothing about SAM.
- **REPL client** (`chat.py`) — a ~40-line stock `a2a-sdk` client: resolves
  the card through the mesh, then loops `input()` → `message/send`, echoing
  the `contextId` the server minted on the first reply.

## What you can do with it

- **Bring any A2A agent onto the mesh unchanged** — the example agent is
  deliberately vanilla `a2a-sdk`; anything speaking A2A over HTTP (ADK,
  LangGraph, a hosted agent) plugs in the same way, one `target_url` line in
  the node config.
- **Use any stock A2A client** — `chat.py` is just the smallest one. The
  rewritten card means SDKs, CLIs, and other agents resolve and call the
  service without knowing SAM exists.
- **Let the agent own the conversation** — the same pattern as
  [Gemini Buddy](../gemini-buddy/), but on the standard A2A wire instead of a
  custom MCP tool: state lives with the agent, keyed by `contextId`, and
  callers only ever send the next message.

## Try it on kind

The repository ships a [kind](https://kind.sigs.k8s.io/)-based local mesh
that brings the agent up with one command.

### 1. Set a Gemini key for the agent image

`agent.py` calls Gemini, so set your API key on the `ENV GEMINI_API_KEY`
line in `development/examples/chat-a2a/Dockerfile` before building (a free
Google AI Studio key is fine for the demo).

### 2. Mesh layout

Host the agent on one node in `development/kind/mesh-config.yaml`:

```yaml
node-a:            # bare node
node-b: chat-a2a   # the A2A agent
```

### 3. Bring the mesh up and enroll a local caller node

```bash
make build            # builds ./bin/sam-node (once)
make kind-up          # control plane + router + agent (node-b)
make kind-local-node  # local sam-node enrolled in the mesh — LEAVE RUNNING
```

The local node is your entry point: its sidecar API listens on
**`http://127.0.0.1:9099`** (bearer token `devtoken`), MCP tools at `/mcp`.

### 4. Find the provider peer

```bash
./bin/mcp-client -url http://127.0.0.1:9099/mcp -token devtoken \
  -tool discover_remote_services -args '{"type":"a2a","name":"chat"}'
```

Note the peer ID and export it as `PEER`. Discovery is gossip-fed; retry for
a few seconds after startup if the list comes back empty.

### 5. Watch the card rewrite happen

Fetch the agent card through the mesh with nothing but `curl`:

```bash
curl -s -H 'X-Sam-Authentication: Bearer devtoken' \
  "http://127.0.0.1:9099/sam/$PEER/a2a/chat/.well-known/agent-card.json" | jq
```

The interface URLs in the response point back at this
`/sam/{peer}/a2a/chat` path — not at the agent's own `127.0.0.1:7777` — and
`capabilities.streaming` is `false`. That rewrite is the whole trick.

### 6. Chat

Requires [`uv`](https://docs.astral.sh/uv/).

```bash
cd development/examples/chat-a2a
uv run --with-requirements requirements.txt chat.py \
  "http://127.0.0.1:9099/sam/$PEER/a2a/chat"
```

Prove the continuity: introduce yourself in one turn, chat about something
else, then ask the agent what your name is. It answers from its own
server-side session — the client never re-sent the history, only the
`contextId`.

## Configuration

| var | default | used by |
|-----|---------|---------|
| `GEMINI_API_KEY` | *(placeholder — set in the Dockerfile)* | agent |
| `GEMINI_MODEL` | `models/gemini-3.5-flash-lite` *(set in the Dockerfile)* | agent |

The listen port (`7777`) is a constant at the top of `agent.py`.
