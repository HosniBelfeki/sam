---
title: "Integrating SAM with VS Code and GitHub Copilot"
linkTitle: "Integrating SAM with VS Code"
---
You can connect your `sam-node` to VS Code as an MCP server, so GitHub Copilot's
agent mode can discover and call tools from anywhere on the SAM mesh.

## Overview

`sam-node` serves a standard Model Context Protocol endpoint over Streamable
HTTP. VS Code is a generic MCP client, so once the server is registered its
tools — `get_mesh_info`, `list_local_services`, `discover_remote_services`,
`find_remote_tools`, `describe_remote_tool` and `call_remote_tool` — appear
alongside Copilot's own.

Worth being clear about what this arrangement is. Copilot connects as an
ordinary MCP client over the node's API, not as a sandboxed agent behind a
`sam-box`. It gets the node's tools; it does not get a per-agent policy, an
egress allowance or a boundary. That is the right shape for a coding assistant
on your own machine, and the wrong one for untrusted code — for that, see
[Running agents on SAM]({{< relref "/docs/user/running-agents" >}}).

## Prerequisites

- A running `sam-node` serving its API (default `http://localhost:8080`). See
  the [Quick Start]({{< relref "/docs/quickstart" >}}).
- The node's API token — the `SAM_API_TOKEN` you started it with, or the
  contents of `--api-token-path`.
- VS Code with GitHub Copilot, in agent mode.

## Configuration

Create `.vscode/mcp.json` in your workspace:

```json
{
  "inputs": [
    {
      "type": "promptString",
      "id": "sam-api-token",
      "description": "SAM node API token",
      "password": true
    }
  ],
  "servers": {
    "sam-mesh": {
      "type": "http",
      "url": "http://localhost:8080/mcp",
      "headers": {
        "X-Sam-Authentication": "Bearer ${input:sam-api-token}"
      }
    }
  }
}
```

The token is deliberately not in the file. `promptString` with
`"password": true` makes VS Code ask for it once and keep it in its own secret
store, which means the file is safe to commit and safe to share — useful,
because it is also the shortest description of how to attach an agent to a node.

For every workspace rather than one, put the same JSON in your user-level
`mcp.json` instead: run **MCP: Open User Configuration** from the command
palette.

`type` must be `http`. Without it the entry is read as a stdio server and will
not load.

## Start the server

Creating the file is not enough, and this is the step that most often looks like
a broken node. VS Code does not automatically run a server it has just been told
about: it waits to be asked.

Open `.vscode/mcp.json` and click the **Start** action VS Code
shows directly above the `"sam-mesh"` entry. It will prompt for the token the
first time.

If nothing seems to happen, run **MCP: List Servers** from the command palette.
That distinguishes the two failures cleanly:

- `sam-mesh` is missing entirely — the file is not being read. Check it is at
  `.vscode/mcp.json` in the workspace root and is valid JSON.
- `sam-mesh` is listed but stopped — it was read and is simply waiting. Start it.

A useful property of the "waiting" case is that it leaves no trace: if the
server was never started, the MCP log has no entry for it at all, successful or
failed. An empty log is evidence of a server that was never asked to run, not of
one that ran and failed.

## Verification

Ask Copilot directly:

> What does `get_mesh_info` say about this node?

A healthy node reports a `router_peer_id`, a non-empty `connected_peers`, and a
`local_api_socket`.

New tools do not always appear in an in-progress chat. If Copilot claims it has
no mesh tools while **MCP: List Servers** shows the server running, start a new
chat, and check `sam-mesh` is ticked in the tools picker on the chat input.

## Install the SAM skill

The MCP tools let Copilot reach the mesh. The skill tells it when reaching for
the mesh is the right move, and how to go about it — discover before describing,
describe before calling.

```bash
sam-node skill install
```

This writes `~/.claude/skills/sam-mesh/SKILL.md`, which VS Code also reads.
`sam-node skill install --project` writes it into the current repository
instead, and `sam-node skill list` shows whether an installed copy is current:

```console
$ sam-node skill list
SAM skill (sam-mesh):
  outdated  ~/.claude/skills/sam-mesh/SKILL.md         Claude
```

Reinstall when it says `outdated`, and reload the window afterwards.

## Examples

Copilot decides when to reach for these. What follows is what to ask, and the
real responses from a node attached to the public testnet — abridged where
marked, but not otherwise tidied up.

### Look at the mesh

> What's the state of the mesh from this node?

Calls `get_mesh_info` with `{}`:

```json
{
  "connected_peers": [
    "12D3KooWNuHQXgEu3fbaBPKnkPCfSsEVjNvpD5Rh3hcfGeJsDzm7",
    "12D3KooWAjWy4GrQDq27AjXgBbKPrNgWhmmxN221mC9oAYNRcZcs"
  ],
  "dht_size": 3,
  "local_api_socket": "/home/you/.config/sam-mesh/sam.sock",
  "router_peer_id": "12D3KooWQQHDRrSfZp2S4RRTYz3pd3Fremsb5ZaP3XXg4CNZGDQj"
}
```

Two fields are easy to misread. `connected_peers` was 18 entries long in this
capture while `dht_size` was 3 — they count different things, so a small
`dht_size` next to a long peer list is not a fault. And `local_api_socket` is a
Unix socket that answers the same HTTP API without a token, because filesystem
permissions already decide who may open it. It is the easier way to script
against your own node.

### Find out what is out there

> What MCP services can this node reach?

Calls `discover_remote_services` with `{"type":"mcp"}`:

```json
[
  {"peer_id": "12D3KooWQ1hk…veSLS", "srv_name": "dummy-http",
   "srv_description": "Canary HTTP tool (k8s agnhost)"},
  {"peer_id": "12D3KooWFQrX…9Uwe1V", "srv_name": "everything",
   "srv_description": "MCP everything test server (tools, resources, prompts)"},
  {"peer_id": "12D3KooWAjWy…RcZcs", "srv_name": "everything",
   "srv_description": "MCP everything test server (tools, resources, prompts)"}
]
```

Note `everything` appearing twice under different peers, and `dummy-http` three
times. Service names are not unique across the mesh and were never meant to be —
the `peer_id` is the identity. Any step that remembers "the everything service"
without remembering which peer will eventually talk to the wrong one.

### Find tools, and read the failures

> List the tools available on the mesh.

Calls `find_remote_tools` with `{}`. The real answer mixes successes and
failures in one array:

```json
[
  {"peer_id": "12D3KooWQ1hk…veSLS", "tool_name": "mcp://dummy-http",
   "error": "failed to connect: failed to connect client: calling \"initialize\": EOF"},
  {"peer_id": "12D3KooWAjWy…RcZcs", "tool_name": "mcp://everything/get-sum",
   "description": "Returns the sum of two numbers"},
  {"peer_id": "12D3KooWAjWy…RcZcs", "tool_name": "mcp://everything/echo",
   "description": "Echoes back the input string"}
]
```

Discovery is best-effort per peer. Three peers advertising `dummy-http` were
reachable enough to be listed but failed at `initialize`, and that is reported
as an `error` field on the entry rather than failing the whole call. A partly
broken mesh returns a partly populated array, so it is worth checking whether
the tool you wanted came back with a `description` or an `error`.

Narrow the search with `service_name` or `tool_name` when you already know the
target; `tool_name` is served from gossip announcements and is the fastest path.

### Call a tool on someone else's machine

> Use the everything service on the mesh to add 2 and 3.

`describe_remote_tool` first, with the peer and the namespaced name exactly as
discovery returned them:

```json
{
  "tool_name": "mcp://everything/get-sum",
  "description": "Returns the sum of two numbers",
  "input_schema": {
    "type": "object",
    "properties": {
      "a": {"type": "number", "description": "First number"},
      "b": {"type": "number", "description": "Second number"}
    },
    "required": ["a", "b"]
  }
}
```

Then `call_remote_tool` with `arguments` matching that schema — `{"a": 2, "b": 3}`
— which returned:

```text
The sum of 2 and 3 is 5.
```

The describe step is not ceremony. Names are namespaced `scheme://service/tool`,
not `service.tool`, and the arguments here are `a` and `b` rather than the
`numbers` array a guess might have produced. Both are the provider's to change.

### Use a model the mesh provides

Inference is not an MCP tool and is never called with `call_remote_tool`.
`discover_remote_services` with `{"type":"inference"}` inventories the providers
— on this mesh, a vLLM TPU service and an OpenRouter proxy — but to actually use
one you talk HTTP to the node's OpenAI-compatible facade at
`http://localhost:8080/v1`.

Over the tokenless local socket:

```console
$ curl -s --unix-socket ~/.config/sam-mesh/sam.sock http://localhost/v1/models
{"object":"list","data":[
  {"id":"openrouter/auto","object":"model","created":0,
   "owned_by":"12D3KooWHfiw…8T5YP"}
]}
```

`owned_by` is the peer serving the model. Note that two inference services were
discovered but only one model is listed: discovery inventories services, while
`/v1/models` lists models that answered the catalog walk. Ask for a model that
is not in this list and it resolves to no provider and comes back 404 — so read
the catalog rather than assuming a name.

A completion is the ordinary OpenAI shape, and took 3.3s here:

```bash
curl -s --unix-socket ~/.config/sam-mesh/sam.sock \
  http://localhost/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"openrouter/auto",
       "messages":[{"role":"user","content":"Reply with exactly: mesh inference works"}]}'
```

Over TCP instead of the socket, add
`-H "X-Sam-Authentication: Bearer $SAM_API_TOKEN"`. Any OpenAI SDK works too:
point `base_url` at `http://localhost:8080/v1`.

## Developing an agent against the mesh

Everything above treats Copilot as a client of your node. The other half of the
loop is writing an agent that runs *behind* a boundary, and VS Code is a
comfortable place to do it: edit the harness in the editor, run it in a
container with no network at all, and watch the boundary decide what it may
reach.

The example lives in `development/examples/agent-harness`. Build it with the
repository root as the build context, because the image compiles `nano-init`
from source:

```bash
docker build -t agent-harness -f development/examples/agent-harness/Dockerfile .
```

Start a boundary for it. `--sidecar-socket` is the node's local API socket, the
same tokenless socket `get_mesh_info` reported, so a node you are already
running is a node you can already develop against:

```bash
sam-box run \
  --socket /tmp/sam-demo/agent.sock \
  --sidecar-socket ~/.config/sam-mesh/sam.sock \
  --bundle development/examples/agent-harness/bundle.yaml \
  --insecure-unverified-bundle \
  --metrics-addr 127.0.0.1:9600
```

It says what that insecure flag costs, and it is right to:

```text
WARN  --insecure-unverified-bundle: this bundle is taken at its word, so
      whoever can write it decides which agent this sandbox is
INFO  Serving agent researcher-1.prod.acme.example
INFO  Agents reach the mesh at http://mesh.sam.alt
```

That is fine on your laptop and not fine anywhere else. In production the
bundle's claim is checked against a platform-issued credential.

### Watch it bootstrap itself

Now run the agent with no network:

```bash
docker run --rm \
  --network none \
  --cap-add NET_ADMIN \
  --device /dev/net/tun \
  -v /tmp/sam-demo/agent.sock:/run/agent.sock \
  agent-harness "Use your mesh tools to find out how many peers this node is connected to."
```

`--network none` is the assertion, not the arrangement. The container has no
interface, no resolver, no route and no credentials; `NET_ADMIN` and
`/dev/net/tun` exist only so `nano-init` can build the single `tun0` that leads
to the mounted socket. If this worked because the container could route
somewhere, it would prove nothing.

The real output:

```text
mesh offered model: openrouter/auto
mesh granted 10 tools: call_remote_tool, describe_remote_tool,
  discover_remote_services, find_remote_tools, get_mesh_info,
  list_local_services, mesh_pubsub_broadcast, poll_messages, send_message,
  subscribe_topic
step 1: get_mesh_info({})
This node is connected to **23 peers**.
```

Nothing was configured. The harness asked `mesh.sam.alt` for a model, asked it
for tools, was granted fifteen, chose one, called it, and answered — from inside
a container that cannot reach anything else. The catalog is answered per agent,
so the list is not a property of the node but of who is asking.

### Watch policy refuse something

The bundle allows `api.github.com` and `*.githubusercontent.com`, and nothing
else. Run a probe in the same sandbox:

```bash
docker run --rm --network none --cap-add NET_ADMIN --device /dev/net/tun \
  -v /tmp/sam-demo/agent.sock:/run/agent.sock \
  --entrypoint nano-init agent-harness \
  run /run/agent.sock python -c '
import urllib.request as u
for url in ["https://api.github.com/zen", "https://example.com"]:
    try:
        r = u.urlopen(url, timeout=25)
        print(url, "->", r.status, r.read(60))
    except Exception as e:
        print(url, "->", type(e).__name__, e)
'
```

```text
[TCP] dial 100.64.0.2:443: boundary refused: 403 Forbidden (not allowed by policy)
https://api.github.com/zen  -> 200 b'Responsive is better than fast.'
https://example.com         -> URLError: [Errno 104] Connection reset by peer
```

The agent sees a reset connection, which is all a denied agent should learn. The
reason lives outside the sandbox, on the boundary, in metrics you can read while
you develop:

```console
$ curl -s http://127.0.0.1:9600/metrics | grep sam_box_flows_total
sam_box_flows_total{outcome="allowed",route="external"} 1
sam_box_flows_total{outcome="allowed",route="mesh-entrypoint"} 19
sam_box_flows_total{outcome="denied",route="unresolved"} 1
```

Nineteen mesh flows for one short run — model catalog, tool listing, the tool
call — one allowed external flow, one denial. Editing `bundle.yaml` and
restarting `sam-box` changes those numbers without touching the agent at all,
which is the property worth developing against: policy is not in the code.

### A trap worth knowing

The harness asks the mesh for a model rather than hardcoding one, and takes the
first the catalog offers. On a mesh offering `google/gemma-2-2b-it` first, that
run fails:

```text
mesh offered model: google/gemma-2-2b-it
openai.BadRequestError: Error code: 400 -
  {'error': {'message': 'System role not supported', ...}}
```

Not a mesh fault and not a policy denial — Gemma has no system role, and the
harness sends a system prompt. Pin a model with `-e SAM_MODEL=openrouter/auto`
while developing, and remember that "whatever the mesh offers first" is a
different model on different days.

## Troubleshooting

**401 from the server.** The token does not match the node's. It is the
`SAM_API_TOKEN` the node was started with. VS Code caches what you typed, so
clear it with **MCP: Reset Cached Tokens** and reconnect.

**Server unreachable.** Check the node is listening:

```bash
curl -s -o /dev/null -w '%{http_code}\n' http://localhost:8080/mcp
```

`401` is a healthy answer — it means the endpoint is there and wants the token.
Connection refused means the node is not running.

**Tools do not appear.** Confirm the server is actually running with **MCP:
List Servers** — see [Start the server](#start-the-server). If it is running,
start a new chat: a chat already in progress may not pick up newly registered
tools.

**A tool is discovered but will not run.** Look for an `error` field on the
entry `find_remote_tools` returned. `failed to connect … calling "initialize":
EOF` means the peer is advertising a service whose backend is not answering —
that is the provider's problem, not yours, and another peer offering the same
service name may well work.

**No remote tools found at all.** Check `get_mesh_info` first. A `dht_size` of
zero means the node has not found the mesh, and no amount of discovery will help
until it has. An empty `connected_peers` means it has not even reached a router.

**Everything works but nothing is sandboxed.** That is by design here — see the
note at the top. Copilot is a client of your node, not an agent behind a
boundary.
