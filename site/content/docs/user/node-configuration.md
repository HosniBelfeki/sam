---
title: "Node Configuration Guide"
linkTitle: "Node Configuration"
weight: 15
---

The `sam-node` acts as a local security gateway and tool proxy for AI agents. While the control plane is the central authority, each Node independently defines its own local tool catalogue and enforces its own local security identity.

---

## 1. Node Configuration File (`sam-node.yaml`)

By default, `sam-node` runs without exposing any local tools to the mesh. To expose local tools or strictly enforce your node's network identity, you must create a Node configuration file and pass it to the daemon using the `--config` flag:

```bash
SAM_API_TOKEN="secret" sam-node run --config ./sam-node.yaml
```

### Configuration Schema

The `sam-node.yaml` file supports defining local **Services** and local **Attenuation** security rules.

```yaml
version: "v1alpha1"

# 1. Define Local Services
services:
  # Example: Expose a local CLI MCP server to the mesh
  - type: mcp
    name: local-shell-tools
    description: "Execute bash commands safely in a local container"
    command: ["npx", "-y", "@modelcontextprotocol/server-everything"]

  # Example: Expose a local inference endpoint
  - type: inference
    name: local-ollama
    description: "DeepSeek local inference proxy"
    target_url: "http://localhost:11434"

# 2. Define Local Security Identity (Zero Trust)
attenuation:
  rules:
    # Example: Inject custom Datalog facts asserting local node state
    - 'time(2026-06-30T00:00:00Z) <- true;'
  policies:
    # Example: Custom local deny rule restricting access from untrusted users
    - 'deny if user("untrusted_sub_id");'
```

---

## 2. Defining Local Services

The `services` array allows you to register endpoints that remote peers in the SAM Network can discover and execute (provided they possess the proper `granted_service_*` credentials issued by the control plane).

| Property | Description |
| :--- | :--- |
| `type` | The protocol protocol type. Supported values are `mcp` (Model Context Protocol), `inference`, or `a2a`. |
| `name` | The unique name of the service (e.g., `git-helper`). This must exactly match the name authorized by the control plane's mesh policy (e.g., `mcp://git-helper`). |
| `description` | A human-readable description published to the mesh discovery catalogue. |
| `command` | *(For MCP)* The executable command array to spawn as a local subprocess (e.g. `["node", "index.js"]`). |
| `env` | *(For MCP)* Key-value environment variables passed to the subprocess. |
| `target_url` | *(For HTTP/Inference)* The upstream local URL to proxy traffic to. |

### Inference Service Path Standards & Proxy Routing

When configuring `target_url` for `type: inference` services (e.g. Ollama, vLLM, OpenAI-compatible backends):
* **Root Target URL Standard**: Always register `target_url` using the base root URL (e.g. `http://localhost:11434` or `http://localhost:8000`), strictly omitting `/v1`.
* **OpenAI Facade Access**: Clients connecting via the node's local OpenAI Facade (`http://localhost:8080/v1`) request paths like `/v1/chat/completions`. SAM automatically proxies these to the backend's root URL.
* **Raw Proxy Access**: If bypassing the Facade and making requests directly via the local egress proxy (`/sam/{peer}/inference/{service}`), the request path must include the explicit `/v1` namespace suffix (e.g. `http://localhost:8080/sam/{peer}/inference/{service}/v1/chat/completions`).

---

## 3. Defining Local Security (Target Attenuation)

In a Zero Trust architecture, the destination node is entirely responsible for verifying that it is the intended recipient of an incoming request.

While the control plane limits token capabilities based on target restrictions (e.g., `target_restricted()` or `target_unrestricted()`), the destination node evaluates these dynamically. The node automatically resolves its local identity context based on its configuration, generating facts internally (such as `allow_network_target($fact, $value)`).

If the caller's token has target restrictions, the connection will only be allowed if the token's `granted_target_*` facts match the dynamically injected identity of the node. You do **not** need to write manual Datalog rules to enforce this mechanism; it is baked directly into the node middleware via baseline policies.

### Local Custom Policies
You can further restrict access using the `attenuation` block. Local policies defined here are evaluated **before** the baseline rules. This means local administrators can write custom rules that explicitly `deny` access based on custom logic, overriding broad access granted by the control plane.

1. **`rules`**: Inject custom Datalog facts asserting local node state (e.g., `time($time)`).
2. **`policies`**: Add local restrictions (e.g., `deny if user("banned_user");`).

---

## 4. Labels & Data Sovereignty

SAM supports attested key=value labels (e.g. `region`, `team`) so a request never leaves a required scope. Cloud providers, on-prem operators, and countries all name regions differently, so SAM imposes no built-in taxonomy or hierarchy: composition (e.g. also attesting a coarser value) is entirely up to the operator.

### Declaring labels (provider)

Start the node with its operator-declared labels, a comma-separated `key=value` list:

```bash
sam-node run --labels region=us-east-1,team=platform ...
```

Labels are declared at enrollment and **attested by the control plane**, but only the ones a role permits. Set `allowed_labels` on the node's role (see [control plane configuration](../control-plane-configuration/)); a role granting none means the node can declare none, and enrollment is refused if it tries. This applies to all three enrollment paths, including bootstrap requests an administrator approves by hand: approving says the identity may join, so the role grant is what says which labels it may carry. The control plane then mints one signed `label(key, value)` fact per declared label into the node's Biscuit. Matching is exact and case-sensitive; an empty value means no claim for that key.

### Requiring labels (consumer)

On the sidecar's OpenAI-compatible endpoints, constrain a request with the `X-Sam-Required-Labels` header (comma-separated `key=value` pairs, any-of):

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $SAM_API_TOKEN" \
  -H "X-Sam-Required-Labels: region=us-east-1" \
  -d '{"model":"test-model","messages":[{"role":"user","content":"hi"}]}'
```

The `call_remote_tool` MCP tool accepts the same requirement via its `required_labels` parameter.

Enforcement is fail-closed and cryptographic: gossiped labels only rank candidate providers, and before any request data leaves your node the sidecar verifies the provider's control-plane-signed Biscuit and checks its attested `label()` facts. Providers that return no identity or lack a matching fact are rejected.

### Restricting callers by label (provider)

Because every enrolled node's token carries its attested label facts, a provider can require callers to hold a label with a single local check in its `attenuation` block:

```yaml
attenuation:
  checks:
    - 'check if label("region", "us-east-1");'
    - 'check if label("jurisdiction", "eu");'
```

For full details on territorial enforcement, regulatory compliance (GDPR Art 44-49, EU Cloud Sovereignty Framework), and cryptographic evidence verification, see the **[Digital & Data Sovereignty Guide](../sovereignty/)**.
