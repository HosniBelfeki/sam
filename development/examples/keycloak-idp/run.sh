#!/usr/bin/env bash
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Keycloak as the identity provider of an Agent Mesh. Brings compose.yaml up
# in order, then runs each case of README.md and checks its outcome.
#
#   ./run.sh        bring the mesh up and run every case
#   ./run.sh down   stop everything and delete the generated tokens

set -euo pipefail
# The components run as a non-root user and read the files written below.
umask 022
# Git Bash on Windows rewrites arguments that look like paths; the container
# paths below are meant literally.
export MSYS_NO_PATHCONV=1

cd "$(dirname "${BASH_SOURCE[0]}")"
RUN_DIR=.run
COMPOSE=(docker compose --progress=quiet)

if [[ "${1:-}" == "down" ]]; then
  "${COMPOSE[@]}" --profile cases down --volumes --remove-orphans
  rm -rf "${RUN_DIR}"
  exit 0
fi

failures=0

# expect NAME WANT GOT prints one result line and counts a mismatch.
expect() {
  if [[ "$3" == "$2" ]]; then
    printf '  PASS  %-66s %s\n' "$1" "$3"
  else
    printf '  FAIL  %-66s got %s, want %s\n' "$1" "$3" "$2"
    failures=$((failures + 1))
  fi
}

# http runs curl inside the compose network, where keycloak, control-plane
# and gateway resolve as the components themselves see them.
http() {
  "${COMPOSE[@]}" run --rm --no-deps -T tools -s "$@"
}

# admin calls the control plane's admin API with the generated admin token.
admin() {
  local method=$1 path=$2
  shift 2
  http -X "${method}" -H "@/example/${RUN_DIR}/admin.header" "$@" "http://control-plane:8080${path}"
}

random_token() {
  head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n'
}

# mint ROLE NAME writes a single-use bootstrap token for ROLE to .run/NAME.token.
mint() {
  admin POST /admin/bootstrap-tokens -H 'Content-Type: application/json' \
    -d "{\"role\":\"$1\",\"ttl_hours\":1,\"max_usages\":1,\"description\":\"keycloak-idp example $2\"}" |
    sed -n 's/.*"token": *"\([^"]*\)".*/\1/p' >"${RUN_DIR}/$2.token"
  [[ -s "${RUN_DIR}/$2.token" ]] || { echo "could not mint a bootstrap token for $2" >&2; exit 1; }
}

# id_token USER CLIENT REALM logs USER in with the password grant and writes
# the ID token to .run/USER.CLIENT.REALM.jwt and as a header to .header.
# The password grant only lets this script run unattended: a person enrolls
# with agentmesh-node join, which runs the browser or device flow.
id_token() {
  local base="${RUN_DIR}/$1.$2.$3"
  http -d grant_type=password -d "client_id=$2" -d "username=$1" -d "password=$1-test-only" -d scope=openid \
    "http://keycloak:8080/realms/$3/protocol/openid-connect/token" |
    sed -n 's/.*"id_token":"\([^"]*\)".*/\1/p' >"${base}.jwt"
  [[ -s "${base}.jwt" ]] || { echo "no ID token for $1 from realm $3, client $2" >&2; exit 1; }
  printf 'X-Mesh-Authentication: Bearer %s\n' "$(cat "${base}.jwt")" >"${base}.header"
}

# wait_log SERVICE TEXT waits for TEXT in the logs of SERVICE.
wait_log() {
  for _ in $(seq 1 90); do
    if "${COMPOSE[@]}" logs "$1" 2>/dev/null | grep -qF "$2"; then
      return 0
    fi
    sleep 1
  done
  echo "timed out waiting for \"$2\" from $1" >&2
  "${COMPOSE[@]}" logs --tail 20 "$1" >&2
  exit 1
}

# enroll CASE JWT starts a node that enrolls with the ID token in .run/JWT
# and prints "admitted", or the HTTP status the control plane refused it with.
enroll() {
  local name="keycloak-idp-member-$1"
  docker rm -f "${name}" >/dev/null 2>&1 || true
  "${COMPOSE[@]}" run -d --no-deps --name "${name}" member run \
    --control-plane=http://control-plane:8080 --insecure-control-plane \
    --jwt-path="/run/example/$2" --data-dir=/tmp/member \
    --api-token-path=/run/example/member-api.token >/dev/null
  local result=timeout
  for _ in $(seq 1 30); do
    if docker logs "${name}" 2>&1 | grep -qF 'Agent Mesh Node Online'; then
      result=admitted
      break
    fi
    if [[ "$(docker inspect -f '{{.State.Running}}' "${name}")" == false ]]; then
      result=$(docker logs "${name}" 2>&1 | sed -n 's/.*enrollment failed with status \([0-9]*\).*/\1/p' | head -n 1)
      break
    fi
    sleep 1
  done
  docker rm -f "${name}" >/dev/null 2>&1 || true
  echo "${result:-exited}"
}

INIT='{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"keycloak-idp-example","version":"0"}}}'

# call_calc HEADER sends an MCP initialize to mcp://calc through the shared
# node with the header file .run/HEADER and prints the HTTP status.
call_calc() {
  http -o /dev/null -w '%{http_code}' -X POST -H "@/example/${RUN_DIR}/$1" \
    -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
    --data "${INIT}" "http://gateway:8080/mesh/${PROVIDER}/mcp/calc"
}

echo "== Generating test-only tokens in ${RUN_DIR}/"
mkdir -p "${RUN_DIR}"
random_token >"${RUN_DIR}/admin.token"
printf 'Authorization: Bearer %s\n' "$(cat "${RUN_DIR}/admin.token")" >"${RUN_DIR}/admin.header"
for name in provider-api gateway-api member-api; do
  random_token >"${RUN_DIR}/${name}.token"
done
printf 'X-Mesh-Authentication: Bearer %s\n' "$(cat "${RUN_DIR}/gateway-api.token")" >"${RUN_DIR}/gateway-own.header"

echo "== Starting Keycloak, the control plane and the calc MCP server"
"${COMPOSE[@]}" up -d --build keycloak control-plane calc
control_plane_ready=false
for _ in $(seq 1 60); do
  if [[ "$(http -o /dev/null -w '%{http_code}' http://control-plane:8080/info || true)" == 200 ]]; then
    control_plane_ready=true
    break
  fi
  sleep 2
done
if [[ "${control_plane_ready}" == false ]]; then
  echo "the control plane did not answer /info within 120s" >&2
  "${COMPOSE[@]}" logs --tail 20 control-plane >&2
  exit 1
fi

echo "== Loading the mesh policy (calc-user bound to group:devs)"
expect "load policy.json" 200 "$(admin POST /policies -H 'Content-Type: application/json' \
  --data-binary @/example/policy.json -o /dev/null -w '%{http_code}')"

echo "== Starting the router, the calc provider and the shared node"
mint mesh:role:router router
mint mesh:role:node provider
mint mesh:role:node gateway
"${COMPOSE[@]}" up -d router provider gateway
wait_log provider 'Registered and announced SERVICE_TYPE_MCP/calc'
wait_log gateway 'Agent Mesh Node Online'
PROVIDER=$("${COMPOSE[@]}" logs provider | sed -n 's/.*PeerID: \(12D3KooW[1-9A-HJ-NP-Za-km-z]*\).*/\1/p' | head -n 1)
[[ -n "${PROVIDER}" ]] || { echo "could not read the provider's peer ID" >&2; exit 1; }

echo "== Tokens from Keycloak"
id_token alice agentmesh mesh
id_token bob agentmesh mesh
id_token alice other-app mesh
id_token alice agentmesh elsewhere

echo
echo "== Enrollment of a node with the user's own ID token"
expect "alice (group devs, bound to mesh:role:node)" admitted "$(enroll alice alice.agentmesh.mesh.jwt)"
expect "bob (group ops, not bound)" 403 "$(enroll bob bob.agentmesh.mesh.jwt)"
expect "alice, token for client other-app (audience)" 401 "$(enroll audience alice.other-app.mesh.jwt)"
expect "alice, token from realm elsewhere (issuer)" 401 "$(enroll issuer alice.agentmesh.elsewhere.jwt)"

echo
echo "== Calling mcp://calc through the shared node"
expect "alice (calc-user through group:devs)" 200 "$(call_calc alice.agentmesh.mesh.header)"
expect "bob (no calc-user)" 403 "$(call_calc bob.agentmesh.mesh.header)"
expect "the shared node's own API token" 403 "$(call_calc gateway-own.header)"

echo
echo "== Moving calc-user from group:devs to group:ops"
# A fresh token for alice, used once under the old policy so the shared node
# holds its exchange, and kept for after the change.
id_token alice agentmesh mesh
issued_at=$(date +%s)
cp "${RUN_DIR}/alice.agentmesh.mesh.header" "${RUN_DIR}/alice-before.header"
expect "alice, new token, before the change" 200 "$(call_calc alice-before.header)"
expect "load policy-calc-to-ops.json" 200 "$(admin POST /policies -H 'Content-Type: application/json' \
  --data-binary @/example/policy-calc-to-ops.json -o /dev/null -w '%{http_code}')"
# The nodes take the policy on the update event, or within their 10s sync.
sleep 15
id_token alice agentmesh mesh
id_token bob agentmesh mesh
expect "bob, new token (grant added)" 200 "$(call_calc bob.agentmesh.mesh.header)"
expect "alice, new token (grant removed)" 403 "$(call_calc alice.agentmesh.mesh.header)"
expect "alice, token exchanged before the change, not yet expired" 200 "$(call_calc alice-before.header)"

wait_for=$((issued_at + 125 - $(date +%s)))
if ((wait_for > 0)); then
  echo "   waiting ${wait_for}s for that token to pass its exp (120s lifetime in this realm)"
  sleep "${wait_for}"
fi
expect "alice, the same token after its exp" 403 "$(call_calc alice-before.header)"

echo
echo "== Where each refusal is logged"
echo "-- control plane: enrollment and token exchange"
"${COMPOSE[@]}" logs --no-log-prefix control-plane | grep -E 'JWT verification failed|"decision": "deny"' | tail -n 4 || true
echo "-- provider: requests its policy refused"
"${COMPOSE[@]}" logs --no-log-prefix provider | grep -F 'AuthZ Denied' | tail -n 3 || true

echo
if [[ "${failures}" -eq 0 ]]; then
  echo "All cases passed. The mesh is still running; ./run.sh down removes it."
else
  echo "${failures} case(s) failed. The mesh is still running for inspection; ./run.sh down removes it."
  exit 1
fi
