---
title: "SAM Documentation"
linkTitle: "Documentation"
---
SAM (Sovereign Agent Mesh) is a smart, zero-config, zero-trust P2P network built for autonomous AI agents. Think of it as a modern, zero-trust overlay network (similar to a private VPN), but specifically designed and scoped for agent-to-agent tool sharing and communication.

## Why SAM?

Instead of exposing your agent's tools (like local scripts, LLM endpoints, or internal APIs) to the public internet, SAM allows you to create secure, private mesh networks. This is especially critical because modern AI agents operate across highly heterogeneous environments—spanning cloud servers, on-premises datacenters, personal laptops, Raspberry Pis, and Android devices. SAM seamlessly connects them all, regardless of complex network topologies or NATs.

**Security & Sovereignty Fundamentals**
* **Open Source Project:** SAM is an open-source project (Apache-2.0). It carries no vendor telemetry, has no proprietary lock-in, and can be deployed on any cloud, on-premises datacenter, or air-gapped environment.
* **Isolated by Default:** You do NOT join any mesh by default. You must explicitly configure the control plane you want to join.
* **Closed by Default:** Joining a mesh does not expose your tools. By default, your node does not allow any services to be reached by others. You must explicitly configure policies to share tools.
* **Public Testnets vs. Sovereign Deployments:** Public testnets (`bananas.sam-mesh.dev`, `hub.sam-mesh.dev`) are created using donated community resources solely as developer playgrounds for CI and experimentation. They hold zero production value, offer no guarantees or SLA, and carry no sovereignty. True sovereignty is achieved by deploying a dedicated control plane and routers with customer-managed cryptographic keys on your chosen infrastructure.
* 🏛️ **[Digital & Data Sovereignty Architecture](sovereignty/)**: Read our detailed breakdown on territorial attestation, jurisdictional label gates, local vetoes, and regulatory alignment.

---

## Where to Start

### "Developer Testnet Mode" (Ephemeral Testing Sandbox)
For the fastest way to get started and experiment with the open-source code, you can connect a node to our public developer testnet (`bananas.sam-mesh.dev`). *Note: This is a shared developer testbed created with community resources. There are no guarantees; it is strictly for testing. Do not expose sensitive or production tools in a public testing environment.*

- 🚀 **[User Quick Start Guide](quickstart/)**: Connect and run a SAM node using binaries or Docker, and query the local MCP server.
- 🤖 **[Agent Integration Guide](user/agent-usage/)**: Connect Google Gemini, Claude, and other AI agents to your SAM node to call tools across the mesh.
- 📡 **[Testnet Validation Tutorial](development/testnet-validation/)**: Real-time verification, remote tool invocation, and HTTP stream proxies.

### "Dedicated Sovereign Deployment Mode" (For Production & Private Meshes)
Deploy your own control plane, manage your own keys, and retain 100% cryptographic authority:
- 🏛️ **[Digital & Data Sovereignty Guide](sovereignty/)**: Deep-dive into jurisdictional label gates, local veto attenuation, uncooperative sandbox boundaries, and compliance.
- 🎛️ **[Production Kubernetes Deployment](user/kubernetes-deployment/)**: Deploy your own control plane and routers via Helm or Kubernetes manifests.
- 🛠️ **[Developer Guide](development/)**: Prereqs, compilation, local control plane setup, and Kind deployment.
- 🧪 **[Testing Guide](development/testing/)**: Go tests, E2E BATS, and containerized mesh execution.

---

## Architecture

*   **`sam-control-plane`**: The control plane for identity mapping, token issuing, and policy distribution.
*   **`sam-router`**: The GossipSub routing overlays and bootstrap points for the P2P mesh.
*   **`sam-node`**: The P2P nodes providing the mesh transport layer, self-healing connectivity, and local Model Context Protocol (MCP) HTTP interfaces.
