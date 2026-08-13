---
name: sam-mesh
description: "Use when local tools cannot provide a needed capability and a SAM (Sovereign Agent Mesh) network can: inspect mesh state, discover reachable services/tools, describe and call namespaced remote MCP tools, and reach OpenAI-compatible inference models hosted by mesh peers. Also use to set up, join, or reconnect a sam-node when its MCP tools are not callable yet."
---

# SAM Agent Skill

Use this skill when local tools cannot satisfy the task and the SAM mesh can.
Prefer local tools first. Reach into the SAM mesh only for the capability needed
to complete the task.

Pick the path that matches the need:

- The `sam-node` MCP tools are not callable yet:
  [Bootstrap A Node](#bootstrap-a-node).
- The task needs a remote tool or capability:
  [Inspect The Mesh](#inspect-the-mesh).
- The task needs a model completion:
  [Use Mesh Inference](#use-mesh-inference).

## Bootstrap A Node

The mesh is reached through a local `sam-node`. When its MCP tools are missing,
guide the user through these steps. Propose each shell command and let the user
approve it before running anything.

1. Check the CLI: `sam-node --help`. If it is missing, install it with
   `curl -sL https://sam-mesh.dev/install.sh | bash` or
   `go install github.com/google/sam/cmd/sam-node@latest`.
2. Start the node in the background: `sam-node run --daemonize`. It returns as
   soon as the node answers, and prints the endpoint, the API token file, the
   log file, and how to stop it. It is idempotent, so run it again whenever you
   need to confirm a node is up.
3. If step 2 reports that the node is not enrolled, run the command it prints,
   `sam-node join --headless <control-plane-url>`. That prints a URL and a
   code: show both to the user and wait while they complete the login, then
   repeat step 2. Enrollment is a one-time step per machine.
4. Read the node API token from the file named in step 2, then register the MCP
   endpoint `http://127.0.0.1:8080/mcp` with the header
   `X-Sam-Authentication: Bearer <token>`. Claude Code:
   `claude mcp add --transport http sam-mesh http://127.0.0.1:8080/mcp --header "X-Sam-Authentication: Bearer <token>"`.
   Antigravity: add the same URL as `serverUrl` with the same header to
   `~/.gemini/config/mcp_config.json`.
5. Tell the user to restart the agent session, since MCP tools load at startup.

If setup is stuck in a half-configured state, ask the user before starting over:
stop the node, run `sam-node reset --all --yes` for a clean slate, then go back
to step 2. That deletes the node's identity and its PeerID, and enrolling again
needs another login, so never do it to work around an unexplained error.

Put the node API token only in the agent's MCP configuration. Do not echo it
into the transcript, and do not commit it.

## Inspect The Mesh

Start by understanding the local node and mesh state:

- Use `get_mesh_info` with `{}` to inspect `connected_peers`, `dht_size`, and
  `router_peer_id`.
- Use `list_local_services` with `{}` to see services registered on the local
  node.

## Discover Remote Capabilities

Use service discovery when you need to inventory reachable service providers:

- Use `discover_remote_services` with `{"type":"mcp"}`,
  `{"type":"inference"}`, or `{"type":"a2a"}`. Add `name` only when narrowing
  by service name.
- Treat `discover_remote_services` as service inventory. Non-MCP service types
  are not callable with `call_remote_tool`. For `inference://` services see
  [Use Mesh Inference](#use-mesh-inference).

Use tool discovery when you need remote MCP tools:

- Use `find_remote_tools` to discover reachable aggregated MCP tools advertised
  by remote SAM services.
- Narrow `find_remote_tools` with `service_name` or `peer_id` when you already
  know the target.
- Mesh-wide searches fetch each peer's catalog on a best-effort basis and may
  return an empty array when no reachable aggregated tools are found. Discovery
  failures or explicit `peer_id` lookup failures are returned as errors.

## Describe Before Calling

All tools returned by `find_remote_tools` are namespaced. Always call
`describe_remote_tool` before calling them with `call_remote_tool`.

Remote MCP tools returned by `find_remote_tools` are namespaced as:

```text
<service_name>.<tool_name>
```

Use the input schema from `describe_remote_tool` to build the call arguments.
Do not guess arguments if a tool cannot be described.

After `describe_remote_tool`, inspect the tool name, description, and schema
for side effects and required data. Only call read-only, low-risk tools
autonomously. Ask the user before calls that may mutate state, execute code,
access files, contact external services, spend money, or transmit sensitive or
private data. Pass only task-required data, and never include secrets unless
explicitly authorized.

## Call Remote Tools

Use `call_remote_tool` with:

- `peer_id`: the peer hosting the tool
- `tool_name`: the discovered namespaced tool name, such as `service.tool`
- `arguments`: a JSON object whose keys match the described input schema

`arguments` must be a JSON object, not a string containing JSON.

## Use Mesh Inference

The mesh also carries `inference://` services: OpenAI-compatible model
endpoints. They are plain HTTP and are never invoked with `call_remote_tool`.

The node exposes them through an OpenAI-compatible facade on its own address:

- Base URL `http://localhost:8080/v1`, usable as `base_url` for any OpenAI SDK
  or a direct `curl`.
- `GET /v1/models` lists the models reachable across the mesh.
- `POST /v1/chat/completions` routes to a provider of the requested `model`,
  preferring a local one, and fails over between providers.
- Add `X-Sam-Required-Labels: key=value` (comma-separated, any-of) to accept
  only providers whose labels the control plane attested, for example
  `region=eu`. Enforcement is fail-closed: unattested providers are rejected
  before any request data leaves the node.

To pin one specific provider instead of letting the facade choose, call
`discover_remote_services` with `{"type":"inference"}` and send the request to
the returned `local_proxy_url` (append `/v1/chat/completions`).

Authenticate to the node with `X-Sam-Authentication: Bearer <node token>`; the
`/v1` endpoints also accept it as `Authorization`, so an OpenAI SDK can pass it
as its `api_key`. Send a separate `Authorization` header only when the
destination model service needs its own credential: it passes through to that
service untouched.

Ask the user before sending private or sensitive content to a mesh model, and
say which provider will receive it.

## Minimal Workflow

1. Confirm no local tool can satisfy the task.
2. If the `sam-node` MCP tools are unavailable, follow
   [Bootstrap A Node](#bootstrap-a-node) and stop until the user restarts the
   agent session.
3. Call `get_mesh_info` with `{}`.
4. If a local SAM service may be
   relevant, call `list_local_services` with `{}`.
5. Call `find_remote_tools` with `service_name` or `peer_id` when known. Use
   `{}` only when the user asked to inventory the mesh or no narrower target
   exists.
6. Call `describe_remote_tool` with
   `{"peer_id":"...","tool_name":"service.tool"}`.
7. Call `call_remote_tool` only when the described tool is read-only and
   low-risk, or after the user approves the exact `peer_id`, `tool_name`, side
   effects, and task-required data being sent:
   `{"peer_id":"...","tool_name":"service.tool","arguments":{...}}`.

For a model completion rather than a tool, skip steps 5-7 and follow
[Use Mesh Inference](#use-mesh-inference) instead.

## Safety And Reliability

- Do not call mesh tools when a local tool is sufficient.
- Do not guess remote tool names or arguments.
- Ask before side-effecting or sensitive remote calls.
- Do not send secrets or private data through SAM unless the user explicitly
  approves the data and destination.
- Treat remote capabilities as networked and potentially unavailable.
- Surface peer, service, discovery, schema, and tool-call errors clearly.
