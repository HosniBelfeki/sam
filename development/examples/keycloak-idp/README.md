# Keycloak as the identity provider

Two users log in to Keycloak and reach one MCP service of the mesh, each
with their own ID token. Membership in a Keycloak group decides what each
may call; the node they call through does not lend them its own
permissions. The example also shows what a refused token looks like, and how
long a permission change takes to reach a caller.

Tracked in [#751](https://github.com/google/agentmesh/issues/751), part of
[#734](https://github.com/google/agentmesh/issues/734).

## Topology

Everything runs in Docker, on one compose network:

| Service | What it is |
|---|---|
| `keycloak` | Keycloak 26.4.0, realm `mesh` with client `agentmesh`, a `groups` claim, and users `alice` (group `devs`) and `bob` (group `ops`). A second realm, `elsewhere`, is a different issuer. |
| `control-plane` | `--issuer http://keycloak:8080/realms/mesh`, `--allowed-audiences agentmesh`. |
| `router` | One router. |
| `calc` | The MCP server of [`../calc-mcp`](../calc-mcp) (`add`, `multiply`). |
| `provider` | A node that publishes it as `mcp://calc` ([`node-provider.yaml`](node-provider.yaml)). |
| `gateway` | A shared node both users call through, with their ID token in `X-Mesh-Authentication`. |

The policy ([`policy.json`](policy.json)) lets nodes publish but grants no
service to `mesh:role:node`, and binds `calc-user` (`allowed_services:
["mcp://calc"]`) to `group:devs` only:

```json
{ "role": "calc-user", "members": ["group:devs"] }
```

`group:devs` matches an entry of the token's `groups` claim. The realm's
`groups` mapper sets `full.path` to `false`, so the claim holds `devs`
rather than `/devs`.

## Run it

Requires Docker with Compose v2 and bash (Linux, macOS, or Git Bash on
Windows).

```sh
./run.sh
```

The agentmesh images are pinned to the commit the example was tested with.
`AGENTMESH_TAG=<commit or release>` runs another published version, and
`AGENTMESH_REGISTRY` another registry.

`run.sh` brings the services up in order, then runs each case and checks
its result. Expected output:

```
== Enrollment of a node with the user's own ID token
  PASS  alice (group devs, bound to mesh:role:node)                        admitted
  PASS  bob (group ops, not bound)                                         403
  PASS  alice, token for client other-app (audience)                       401
  PASS  alice, token from realm elsewhere (issuer)                         401

== Calling mcp://calc through the shared node
  PASS  alice (calc-user through group:devs)                               200
  PASS  bob (no calc-user)                                                 403
  PASS  the shared node's own API token                                    403

== Moving calc-user from group:devs to group:ops
  PASS  alice, new token, before the change                                200
  PASS  load policy-calc-to-ops.json                                       200
  PASS  bob, new token (grant added)                                       200
  PASS  alice, new token (grant removed)                                   403
  PASS  alice, token exchanged before the change, not yet expired          200
  PASS  alice, the same token after its exp                                403
```

It exits non-zero if any case does not match. The mesh keeps running
afterwards; `./run.sh down` removes the containers and the generated
tokens.

## What each case shows

**Enrollment.** A node enrolls with its user's ID token
(`agentmesh-node run --jwt-path`). The control plane checks the issuer and
the audience, then requires the requested role to resolve from a binding.
`bob` is authenticated but no binding gives `group:ops` the node role, so he
gets `403`. A token for another client fails the audience check, and a token
from another realm fails the issuer check, both with `401`.

**Calls through a shared node.** `gateway` exchanges each caller's ID token
at the control plane (`POST /token/exchange`) for a credential of that
caller, and forwards the request with it. `alice` reaches `calc`; `bob` and
the gateway's own API token are refused by the provider before the request
reaches `calc`. The gateway's own credential grants no service, so a caller
cannot borrow it.

**A permission change.** After `calc-user` moves to `group:ops`:

| | When it takes effect |
|---|---|
| A grant added (`bob`) | When the nodes take the new policy: at once on the policy update event, otherwise within `--control-plane-sync-interval`. |
| A grant removed, new token (`alice`) | At once: the exchange mints the credential from the current bindings. |
| A grant removed, token already exchanged (`alice`) | When that ID token expires. The node caches the exchange until the token's `exp`, and the credential carries the roles it was minted with, as [authorization.md](../../../site/content/docs/concepts/authorization.md) describes. |

The ID token lifetime is therefore the longest a removed permission stays
usable by a caller who already holds a token. The realms here set it to 60
seconds (`accessTokenLifespan`, which the ID token follows) so the script
finishes quickly; Keycloak's default is 5 minutes.

## Where each refusal is logged

| Refusal | Logged by | Line |
|---|---|---|
| Wrong audience or issuer | control plane | `JWT verification failed` with the reason |
| Expired token at the exchange | control plane | `Border Crossing` with `"decision": "deny"` |
| A call the caller's credential does not grant | provider node | `[Ingress] AuthZ Denied` with the failed check |

`run.sh` prints the last of these lines at the end.

## Shortcuts this example takes

- Keycloak runs `start-dev`: plain `http` inside the compose network and an
  in-container database. Production Keycloak needs TLS, a database and a
  fixed hostname
  ([Configuring Keycloak for production](https://www.keycloak.org/server/configuration-production)).
  The control plane and the nodes use `--insecure-control-plane` for the
  same reason.
- `run.sh` gets tokens with the password grant so it runs unattended. A
  person enrolls a laptop with `agentmesh-node join <control-plane-url>`,
  which runs the browser or device flow against the same client.
- The users' passwords (`alice-test-only`, `bob-test-only`) are in the realm
  files and are for this example only. The admin, API and bootstrap tokens
  are generated at random into `.run/` on each run, passed to the
  components as files and to `curl` as header files, never on a command
  line.
- The control plane runs with `--auto-approve-enrollment`, so the router and
  the two nodes enroll with the single-use bootstrap tokens `run.sh` mints
  without an approval step. User enrollment through Keycloak is decided by
  the policy bindings alone and does not depend on it.
