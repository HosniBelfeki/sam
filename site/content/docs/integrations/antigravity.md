---
title: "Integrating SAM with Google Antigravity"
linkTitle: "Integrating SAM with Google Antigravity"
---
You can connect your `sam-node` to Google Antigravity as an MCP server. By exposing the SAM Model Context Protocol (MCP) server to Antigravity, the agent can dynamically discover tools hosted by other peers in the mesh, describe them, and execute them to solve tasks.

## Overview

Antigravity natively supports Streamable HTTP MCP servers via the `serverUrl` configuration. Since `sam-node` implements the Streamable HTTP transport, you can connect it directly without any bridge.

## Prerequisites

- `sam-node` installed and able to join a mesh (see the [Quick Start](../quickstart/)).
- The node API token (`SAM_API_TOKEN` env or `--api-token-path`) you'll launch it with.

## Running the Node Alongside Antigravity

Antigravity talks to `sam-node` as a plain HTTP server rather than launching it itself (unlike a `stdio` MCP server, which the harness spawns and manages). That means `sam-node run` has to already be listening on the configured port *before* Antigravity starts, and keep running for the whole session.

### Recommended: `--daemonize`

`--daemonize` starts the node in the background and returns once its local API answers, so there is no second terminal to keep open. It also generates an API token under the data directory if you have not configured one:

```bash
sam-node run --daemonize
```

```text
sam-node is running in the background.
  PID       48213
  Endpoint  http://127.0.0.1:8080/mcp
  Token     /home/you/.config/sam-mesh/api-token
  Logs      /home/you/.config/sam-mesh/sam-node.log
  Stop      kill 48213
```

The command is idempotent: re-run it any time to make sure a node is up. Enrollment still needs a one-time login, so if the node has no identity yet the command tells you to run `sam-node join --headless <control-plane-url>` first, which prints a URL and a code to complete in your browser.

### Alternative: a dedicated terminal

On the very first run, `--join` needs an interactive terminal to complete the browser/device-code login; after that, the identity is stored and `--join` is a no-op, so the node can be started detached/hidden freely.

### macOS / Linux
Simplest: leave the node running in its own terminal tab, then open a second tab for Antigravity:
```bash
# Terminal 1 — leave running
SAM_API_TOKEN=my-secret-token sam-node run --join --bind-addr 127.0.0.1:8080
```
```bash
# Terminal 2
agy
```

### Windows
PowerShell and cmd.exe don't have a direct equivalent of Unix job control (`&`), so either use `sam-node.exe run --daemonize` or run `sam-node.exe run --join` in one Windows Terminal/PowerShell tab and leave it running, then open a second tab for Antigravity.

### Any OS: Docker
The Docker image already runs detached, so it doesn't need a second terminal at all — pass `-it` instead of `-d` only for the first run, to complete the interactive login:
```bash
docker run -d --name sam-node \
  --user "$(id -u):$(id -g)" \
  -v $(pwd)/sam-data:/data \
  -p 5001:5001/udp -p 5002:5002 -p 8080:8080 \
  -e SAM_API_TOKEN=my-secret-token \
  ghcr.io/google/sam-node:latest \
  run --join --data-dir /data --bind-addr 0.0.0.0:8080
```

## Configuration

Edit your Antigravity MCP configuration file:

- Path: `~/.gemini/config/mcp_config.json`

Add the node directly using its HTTP endpoint (replace `<YOUR_TOKEN>` with your the node API token (`SAM_API_TOKEN` env or `--api-token-path`)):

```json
{
  "mcpServers": {
    "sam-mesh": {
      "serverUrl": "http://localhost:8080/mcp",
      "headers": {
        "X-Sam-Authentication": "Bearer <YOUR_TOKEN>"
      }
    }
  }
}
```

Antigravity will automatically discover the change. The `sam-node` tools — `discover_remote_services`, `find_remote_tools`, `describe_remote_tool`, and `call_remote_tool` — will then be available.

## Install the SAM Skill

The MCP tools give Antigravity the ability to reach the mesh; the [SAM skill](https://antigravity.google/docs/skills) tells it when and how to use them, including how to reach mesh inference models. Install it once:

```bash
sam-node skill install
```

This writes `~/.gemini/config/skills/sam-mesh/SKILL.md`, Antigravity's global skills location, so it applies to every workspace. Use `sam-node skill install --project` instead to check it into the current workspace at `.agents/skills/sam-mesh/SKILL.md`, and `sam-node skill list` to see whether an installed copy is current. The agent picks the skill up on its own when a task needs the mesh.

## Discovering and Invoking Remote Tools

The tool flow for Antigravity is as follows:

1. `discover_remote_services` → list active services and obtain their `peer_id`s.
2. `find_remote_tools` (`peer_id`) → list the tools a peer hosts.
3. `describe_remote_tool` (`peer_id`, `tool_name`) → fetch the tool's `input_schema`.
4. `call_remote_tool` (`peer_id`, `tool_name`, `arguments`) → invoke it across the mesh.

## Troubleshooting

* **Connection errors**: verify `sam-node` is reachable at the configured URL and that the bearer token matches the node API token (`SAM_API_TOKEN` env or `--api-token-path`).
* **Running `sam-node` in WSL or a container**: the `mcp-remote` bridge runs on the host, so that host must be able to reach the node's bind address. Bind the node to an address the host can reach (e.g. `0.0.0.0`).
