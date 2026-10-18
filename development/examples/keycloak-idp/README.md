# Keycloak as the identity provider

Everything the guide at `site/content/docs/guides/keycloak.md` walks through,
as one `docker compose` project and a script that checks each step:

- Keycloak 26.4.0 with the realm `mesh` (client `agentmesh`, a `groups`
  claim, `alice` in group `devs`, `bob` in group `ops`) and a second realm,
  `elsewhere`, that the control plane does not trust;
- `agentmesh-control-plane` with that realm as its issuer, one router, the
  calculator of [`../calc-mcp`](../calc-mcp) published as `mcp://calc`, and
  a shared `agentmesh-node` both users call through with their own ID token.

```sh
./run.sh        # bring it up, run every case, exit non-zero on a mismatch
./run.sh down   # remove the containers and the generated tokens
```

`run.sh` prints one line per case:

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

The agentmesh images are pinned to the commit the example was tested with.
`AGENTMESH_TAG` selects another published commit or release, and
`AGENTMESH_REGISTRY` another registry.

`.run/` holds the admin, API and bootstrap tokens `run.sh` generates on each
run, and is not committed. The users' passwords in the realm files are for
this example only. Keycloak runs `start-dev` over plain `http` inside the
Compose network; the guide lists what to change for a real deployment.
