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

Getting a node onto the mesh takes two things: **joining** — registering the node and obtaining its cryptographic identity (a Biscuit token) via an OIDC login — and **running** it. The `--join` flag on `run` does both in one command: it enrolls the first time (when the node has no identity yet), then starts serving; on every later restart it's a no-op since the identity is already stored.

### Recommended: One Command

#### Using the Binary
The binary is the simplest way to run a node locally — no volumes or port mapping to think about:
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
Docker works the same way, but needs a persistent volume for the identity and explicit port mapping (`5001/udp`, `5002/tcp` for libp2p, `8080/tcp` for the local API):
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

By default `--join` enrolls with the public testnet (`bananas.sam-mesh.dev`); pass `--control-plane <url>` to join a different mesh.

### Alternative: Join and Run Separately

If you're deploying headlessly with a pre-issued bootstrap token, or just prefer explicit steps, you can join and run as two commands instead.

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
