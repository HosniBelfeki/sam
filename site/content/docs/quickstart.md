---
title: "Quick Start"
linkTitle: "Quick Start"
weight: 1
---

# Quick Start

This guide gets you up and running with a SAM node connected to the public `bananas.sam-mesh.dev` mesh. You can run SAM either directly via a binary or using Docker.

## 1. Install SAM

### Option A: Install Script (macOS / Linux)
The easiest way to install the latest binaries directly:
```bash
curl -sL https://sam-mesh.dev/install.sh | bash
```

### Option B: Go Install (macOS / Linux / Windows)
If you have Go installed, you can compile and install directly from the repository:
```bash
go install github.com/google/sam/cmd/sam-node@latest

# Optional: if you want to run your own control plane and router:
go install github.com/google/sam/cmd/sam-control-plane@latest
go install github.com/google/sam/cmd/sam-router@latest
```

### Option C: PowerShell (Windows)
For Windows users without WSL, you can download the latest release using PowerShell:
```powershell
$Release = Invoke-RestMethod -Uri "https://api.github.com/repos/google/sam/releases/latest"
$Version = $Release.tag_name
$Url = "https://github.com/google/sam/releases/download/$Version/sam_Windows_x86_64.zip"
Invoke-WebRequest -Uri $Url -OutFile "sam.zip"
Expand-Archive -Path "sam.zip" -DestinationPath "$env:ProgramFiles\sam"
```

---

## 2. Connect Your Node to the Mesh

**Shortcut: let your agent do it.** If you use an agent that supports skills (Claude Code, Google Antigravity, …), install the [agent skill](#4-teach-your-ai-agent-to-use-the-mesh) with `sam-node skill install`, restart the agent, and ask it to connect to the mesh. The skill walks it through starting the node and registering the MCP endpoint on its own — the only thing it hands back to you is the one-time enrollment login below.

Getting a node onto the mesh takes two steps: **joining** — registering the node and obtaining its cryptographic identity (a Biscuit token) via an OIDC login — and **running** it. We recommend doing them as two explicit commands: enrollment is a one-time step per machine whose prompts (browser login, admin approval) are easier to follow on their own, and it is the same flow the [agent skill](#4-teach-your-ai-agent-to-use-the-mesh) guides your agent through. If you prefer a single command, see the [alternative below](#alternative-one-command).

### Recommended: Join, Then Run

Join once to enroll the node, then run it. The identity is stored in the node's data directory and reused on every later start.

#### Step 1: Join the Mesh

To register your node with the mesh and obtain a cryptographic identity token (Biscuit), you can use either the interactive OIDC authorization flow or the non-interactive bootstrap token flow.

##### Option A: Interactive OIDC Flow (Default)

The interactive flow uses your browser to authenticate your identity against Dex (OIDC):

###### Using the Binary
```bash
sam-node join https://bananas.sam-mesh.dev
```

###### Using Docker
```bash
mkdir -p $(pwd)/sam-data
docker run -it \
  --user "$(id -u):$(id -g)" \
  -v $(pwd)/sam-data:/data \
  ghcr.io/google/sam-node:latest \
  join --data-dir /data https://bananas.sam-mesh.dev
```

The CLI will output a Device Authorization URL (if headless/Docker) or open your browser natively. Once authenticated, the node registers and saves the identity database.

##### Option B: Non-Interactive Bootstrap Flow (Headless)

If you are deploying a headless server or router and have a generated bootstrap token from the Control Plane API:

###### Using the Binary
```bash
sam-node join --bootstrap-token <your-token> https://bananas.sam-mesh.dev
```

###### Using Docker
```bash
mkdir -p $(pwd)/sam-data
docker run -it \
  --user "$(id -u):$(id -g)" \
  -v $(pwd)/sam-data:/data \
  ghcr.io/google/sam-node:latest \
  join --data-dir /data --bootstrap-token <your-token> https://bananas.sam-mesh.dev
```

*Note: In non-interactive mode, unless the control plane runs with `--auto-approve-enrollment`, the enrollment request remains **PENDING** until approved manually by a network administrator.*

#### Step 2: Run the Node

Start your node in the background. We set a security API token (via the `SAM_API_TOKEN` environment variable or `--api-token-path` file) to protect access to the local control plane API. Tokens are never accepted as command-line values: they would be visible in process listings.

##### Using the Binary
```bash
SAM_API_TOKEN=my-secret-token sam-node run --bind-addr 127.0.0.1:8080
```
You should see in the logs:
```text
INFO  sam-node  [AuthN] Successfully authenticated with router via libp2p: ...
SAM Node Online.
PeerID: 12D3KooW...
```

##### Using Docker
Map the required ports (`5001/udp`, `5002/tcp` for libp2p, and `8080/tcp` for the local API):
```bash
mkdir -p $(pwd)/sam-data
docker run -d \
  --name sam-node \
  --user "$(id -u):$(id -g)" \
  -v $(pwd)/sam-data:/data \
  -p 5001:5001/udp \
  -p 5002:5002 \
  -p 8080:8080 \
  -e SAM_API_TOKEN=my-secret-token \
  ghcr.io/google/sam-node:latest \
  run --data-dir /data --bind-addr 0.0.0.0:8080
```
Verify the node is running with `docker logs sam-node`.

#### Running It in the Background

`sam-node run` stays in the foreground. Add `--daemonize` to start it detached and return as soon as its local API answers — useful when an AI agent is driving the setup, or when you don't want a terminal dedicated to the node:

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

If no API token is configured (`SAM_API_TOKEN` or `--api-token-path`), `--daemonize` generates one under the data directory and reuses it on later starts. The command is idempotent: re-run it to confirm a node is up. Enrollment still needs a one-time login, so on a node with no identity it tells you to run `sam-node join --headless <control-plane-url>` first. In headless mode, SAM prefers OAuth device flow automatically when the provider supports it, and falls back to OOB code-paste only when needed.

#### Starting Over

A node reuses whatever is already in its data directory, which is what you want day to day but not when you are testing setup flows. Two levels of reset:

```bash
sam-node reset             # forget the mesh identity only, keep the PeerID
sam-node reset --all       # delete every file the node keeps, including its key
```

`--all` asks for confirmation, and needs `--yes` when there is no terminal to ask on. Both refuse while a node is still running, so stop it first (`kill <pid>` from the `--daemonize` output). After `--all` the node generates a new PeerID and has to enroll again.

### Alternative: One Command

The `--join` flag on `run` does both steps in one command: it enrolls the first time (when the node has no identity yet), then starts serving; on every later restart it's a no-op since the identity is already stored. By default `--join` enrolls with the public testnet (`bananas.sam-mesh.dev`); pass `--control-plane <url>` to enroll with a different mesh.

#### Using the Binary
```bash
SAM_API_TOKEN=my-secret-token sam-node run --join --bind-addr 127.0.0.1:8080
```
The CLI will open your browser for login (or print a device code if headless). Once authenticated:
```text
Successfully joined the Sovereign Agent Mesh!
INFO  sam-node  [AuthN] Successfully authenticated with router via libp2p: ...
SAM Node Online.
PeerID: 12D3KooW...
```

#### Using Docker
```bash
mkdir -p $(pwd)/sam-data
docker run -it \
  --user "$(id -u):$(id -g)" \
  -v $(pwd)/sam-data:/data \
  -p 5001:5001/udp \
  -p 5002:5002 \
  -p 8080:8080 \
  -e SAM_API_TOKEN=my-secret-token \
  ghcr.io/google/sam-node:latest \
  run --join --data-dir /data --bind-addr 0.0.0.0:8080
```
Use `-it` for this first run so you can complete the browser/device-code login; once enrolled, restart it detached with `-d` instead (`--join` is a no-op at that point, so it's safe to leave in your start command). If there's no interactive terminal attached (e.g. `-d` on the very first run), the node instead comes up as an unauthenticated sidecar waiting for out-of-band enrollment over MCP.

## 3. Query the Local MCP API

Your SAM node exposes a standard Model Context Protocol (MCP) server. The easiest way to interact with it is using the `mcp-client` CLI tool (which is installed alongside `sam-node`):

### List Local Control Plane Tools
Query the list of tools available on your local node (e.g. peer discovery, message broadcast, and remote tool execution):

```bash
mcp-client -url http://localhost:8080/mcp -token my-secret-token -list
```

### Discover Remote Services in the Mesh
List active MCP services currently registered across the public mesh network:

```bash
mcp-client -url http://localhost:8080/mcp \
  -tool discover_remote_services \
  -args '{"type":"mcp"}'
```

### Find Remote Tools on a Peer
Using a `peer_id` returned from the service discovery, find the tools available on that peer:

```bash
mcp-client -url http://localhost:8080/mcp \
  -tool find_remote_tools \
  -args '{"peer_id":"<target-peer-id>"}'
```

### Call a Remote Tool
Call a tool hosted on a remote peer through your local node's P2P stream reverse proxy:

```bash
mcp-client -url http://localhost:8080/mcp \
  -tool call_remote_tool \
  -args '{"peer_id":"<target-peer-id>","tool_name":"everything.get-sum","arguments":{"a":12.5,"b":7.5}}'
```

## 4. Teach Your AI Agent to Use the Mesh

Knowing the tools exist is not the same as knowing when and how to use them. `sam-node` ships an [agent skill](https://agentskills.io) that tells your agent how to bring a node online, discover mesh services, call remote tools, and reach mesh inference models. Install it once:

```bash
sam-node skill install
```

This writes `SKILL.md` into the per-user skill directories your agents scan:

| Agent | Path |
| :--- | :--- |
| Claude Code, Claude Desktop | `~/.claude/skills/sam-mesh/SKILL.md` |
| Google Antigravity | `~/.gemini/config/skills/sam-mesh/SKILL.md` |

Useful variants:

```bash
sam-node skill install --project   # install into this project (./.claude and ./.agents)
sam-node skill install --dir DIR   # install into a specific skills directory
sam-node skill list                # show where it is installed and whether it is current
sam-node skill show                # print the document, for agents with a different layout
```

Re-run `sam-node skill install` after upgrading `sam-node` to refresh the document. Then connect your agent to the node's MCP endpoint — see the [integration guides](../integrations/) — and restart it so both the skill and the tools load.

With the skill installed you can also skip the manual setup entirely and ask the agent to bring itself online (for example: *"connect to the sam mesh and show me what tools are available"*). The skill teaches it to start the node with `sam-node run --daemonize`, read the API token, and register the MCP endpoint itself; it only stops to hand you the one-time `sam-node join` login, which stays with a human by design.
