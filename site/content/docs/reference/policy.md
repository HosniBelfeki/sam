---
title: "Mesh policy"
linkTitle: "Mesh policy"
weight: 6
aliases:
  - /docs/development/policy/
---

The mesh policy is one document held by the control plane: a list of roles
and a list of bindings. It is posted as JSON (protojson of
`PolicyConfigUpdateRequest` in `api/sam.proto`) to `POST /policies`, edited
in the console, or given to `sam-one --policy-file` for first boot.

```json
{
  "roles": [
    {
      "name": "sam:role:node",
      "allowed_services": ["system://sam.catalog"],
      "allowed_labels": ["region=*", "team=platform"]
    },
    {
      "name": "developer",
      "allowed_services": ["mcp://code-reviewer", "mcp://build-runner.*", "inference://*"],
      "allowed_targets": ["group:dev-nodes", "node:12D3KooWSpecialNode"],
      "allowed_agents": ["*.dev.acme.example"],
      "custom_datalog": ["tier(\"standard\");"]
    }
  ],
  "bindings": [
    { "role": "sam:role:node", "members": ["group:engineering", "user:system:serviceaccount:sam-nodes:calc-mcp-sam-node"] },
    { "role": "developer",     "members": ["group:engineering"] }
  ]
}
```

The whole document is replaced on every post. Unknown fields are rejected,
so a misspelt key fails the request instead of dropping a grant without
notice.

## Roles

| Field | Type | Meaning |
|---|---|---|
| `name` | string, required, unique | The role name. `sam:role:node`, `sam:role:router` and `sam:role:sambox` are the roles that the three binaries request at enrollment. Any other name is an ordinary role. |
| `allowed_services` | list of service patterns | Services that holders may call. |
| `allowed_targets` | list of target patterns | Nodes that holders may call. If absent, any node may be called. |
| `allowed_labels` | list of label patterns | Labels that a node holding this role may declare at enrollment. If absent, no labels may be declared. |
| `allowed_agents` | list of agent patterns | Agent identifiers that a node holding this role may claim to act for. If absent, no agent may be named. |
| `custom_datalog` | list of Datalog statements | Facts are minted into the credentials of holders. Rules are distributed to nodes and applied when a holder is verified. |

### Service patterns

`type://name`, where `type` is `mcp`, `inference`, `a2a` or `system`, and
`name` consists of dot-separated DNS-style labels.

| Pattern | Compiles to | Matches |
|---|---|---|
| `mcp://calculator` | `granted_service_exact("mcp", "calculator")`. Several exact entries of one type are merged into one `granted_service_set`. | that service |
| `mcp://*.internal` | `granted_service_suffix("mcp", ".internal")` | `a.internal` and `b.c.internal`, but not `xinternal` |
| `mcp://build.*` | `granted_service_prefix("mcp", "build.")` | `build.runner`, but not `builder` |
| `mcp://*` | `granted_service_all("mcp")` | every MCP service |
| `*` | `granted_service_all_types()` | every service |

`system://sam.catalog` is the built-in discovery service that every node
runs. A role that should be able to list the tools of a node needs it.

### Target patterns

`fact:value`, where `fact` is one of the identity facts that a node's
credential can carry: `node` (peer ID), `user`, `email`, `group`,
`idp_role`. The values come from the destination node's own credential, so
`group:dev-nodes` means "nodes whose enrolling identity is in group
`dev-nodes`".

| Pattern | Matches |
|---|---|
| `node:12D3KooW...` | one node |
| `group:dev-nodes` | nodes enrolled by a member of that group |
| `email:*.acme.example` | suffix match on the email of the enrolling identity |
| `group:*` | any node with a `group` fact |
| `*` | any node |

Exact entries of one fact are merged into one `granted_target_set` fact.
`agent:` is not a valid target, because a node's identity does not say which
agents it hosts.

### Label patterns

| Pattern | Permits |
|---|---|
| `key=value` | exactly that pair |
| `key=*` | any value for that key |
| `*` | any label |

Keys match `[a-zA-Z0-9_.-]{1,63}`. Values are up to 255 characters with no
`,`, `=` or control characters. Every label that a node declares must be
permitted by a role it holds, or enrollment fails. Manual approval of a
bootstrap enrollment does not bypass this check.

### Agent patterns

An agent identifier is lowercase and dot-separated, with at least two labels
(`reviewer-7.prod.acme.example`). A pattern is an identifier, `*.<suffix>`,
`<prefix>.*`, or `*`. Wildcards are anchored on label boundaries. `*` lets
every holder name any agent, and nodes log a warning when they receive such
a grant. See the [sandboxed agents preview](../../preview/sandboxed-agents/).

### `custom_datalog`

Each entry is one Biscuit Datalog fact or rule. A **fact**
(`tier("standard");`) is minted into the credential of every holder, where
`attenuation` statements on any node can refer to it. A **rule**
(`head($x) <- body($x);`) is compiled into the node-side rule set and
applied when a holder is verified. `allow` and `deny` policies are not
accepted here. They belong in a node's `attenuation.policies`. An entry that
parses as neither a fact nor a rule fails validation.

## Bindings

| Field | Meaning |
|---|---|
| `role` | A role name from `roles`. A binding to an undefined role is rejected. |
| `members` | Identities that receive the role. At least one is required. |

A member is one of:

| Member | Matches |
|---|---|
| `user:<sub>` | the OIDC subject. For a Kubernetes service account: `user:system:serviceaccount:<namespace>:<name>`. |
| `email:<address>` | a verified email claim |
| `group:<name>` | an entry of the `groups` claim |
| `idp_role:<name>` | an entry of the issuer's `roles` claim |
| `node:<peer-id>` | one node, by key |
| `agent:<id>` | an agent identifier that a node names when acting for the agent. Only as trustworthy as the node's `allowed_agents` grant. |
| `sam:system:authenticated` | every identity that the identity provider authenticates |

`role:` is not a member, because a role cannot grant a role. A bootstrap
token enrollment carries no OIDC claims. Such a node receives exactly the
token's role and nothing that a binding on claims would add, so the grants
must be on the role itself.

## Evaluation

At enrollment and at refresh, the control plane resolves the identity's roles
from the bindings and mints into the credential one `role()` fact per role
and the compiled `granted_*` facts of every role. Nodes also fetch the policy
(`--policy-sync-interval`, 1 hour) and compile it into rules such as
`role("developer") <- group("engineering")` and
`granted_service_exact("mcp","code-reviewer") <- role("developer")`. These
rules run at verification time. Additions therefore reach nodes within the
sync interval, and removals within the credential TTL.

At the destination node, in this order:

1. Facts for the request: `service($type, $name)`, `connection_peer_id($id)`,
   `time($now)`, and `agent($id)` if a claim was made.
2. Checks that always apply: `client_peer_id($id), connection_peer_id($id)`,
   `time($t), expiration($e), $t <= $e`, and `agent_authorized()` when an
   agent was named.
3. The node's own identity as `target_fact($name, $value)` facts.
4. The node's `attenuation` rules, checks and policies.
5. Baseline policies: `allow if service($t,$n), granted_service_exact($t,$n)`
   and the set, prefix, suffix, per-type and global variants; the check
   `allow_network_target($f,$v) or target_unrestricted()`.
6. The synced mesh policy rules.

All checks must hold, and the first matching policy decides. Without a
matching `allow`, the request is denied.

## Limits

The control plane rejects a policy that could give one identity, through
overlapping bindings, more Datalog facts than the Biscuit authorizer can
evaluate. The limit is far above any ordinary policy. A policy that reaches
it should use wildcards or sets instead of listing every entry.

## Datalog vocabulary

Every fact name used by SAM, for writing `custom_datalog` or `attenuation`
statements.

| Fact | Terms | Minted by |
|---|---|---|
| `node` | peer ID | control plane |
| `client_peer_id` | peer ID | control plane |
| `expiration` | date | control plane |
| `role` | name | control plane |
| `user`, `email`, `group`, `idp_role` | string | control plane, from OIDC claims |
| `label` | key, value | control plane |
| `right` | `relay` | control plane, for routers |
| `target_unrestricted` | | control plane, for roles with `allowed_targets: ["*"]` or none |
| `granted_service_exact`, `_set`, `_prefix`, `_suffix`, `_all`, `_all_types` | type, name/set/pattern | control plane and node rules |
| `granted_target_exact`, `_set`, `_prefix`, `_suffix`, `_all`, `_all_facts` | fact, value/set/pattern | control plane and node rules |
| `granted_agent_exact`, `_set`, `_prefix`, `_suffix`, `_all` | pattern | control plane and node rules |
| `service` | type, name | destination node, per request |
| `connection_peer_id` | peer ID | destination node, per request |
| `time` | date | destination node, per request |
| `agent` | identifier | destination node, from the caller's claim |
| `target_fact` | fact, value | destination node, from its own credential |
| `allow_network_target` | fact, value | derived by baseline rules |
| `agent_authorized` | | derived by baseline rules |
