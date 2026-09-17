#!/usr/bin/env bats

load "lib/container_mesh.bash"

CALC_MCP_IMAGE="sam-calc-mcp:local"

build_calc_mcp_image() {
  if ! docker image inspect "${CALC_MCP_IMAGE}" >/dev/null 2>&1; then
    docker build -t "${CALC_MCP_IMAGE}" \
      -f tests/e2e/docker/calc-mcp/Dockerfile \
      tests/e2e/docker/calc-mcp >/dev/null
  fi
}

start_calc_mcp() {
  local name="${MESH_PREFIX}-calc-mcp"
  docker run -d \
    --name "${name}" \
    --network "${MESH_NETWORK}" \
    --network-alias calc-mcp \
    "${CALC_MCP_IMAGE}" >/dev/null
  MESH_CONTAINERS+=("${name}")
  mesh_wait_for_log "${name}" "Uvicorn running on" 20
}

# Custom mock OIDC server that returns 'data-scientist' role
mesh_start_mock_oidc_custom() {
  local name="${MESH_PREFIX}-oidc"
  local cmd
  read -r -d '' cmd <<'EOF' || true
python3 - <<'PY'
import json
import time
import jwt
from http.server import BaseHTTPRequestHandler, HTTPServer

# The signing key is generated fresh on every start and the JWKS derived from
# it, so no private key lives in the repository: an issuer whose key is public
# would let anyone mint a token for any identity a control plane trusts.
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric import rsa

def _b64url_uint(n):
    import base64
    raw = n.to_bytes((n.bit_length() + 7) // 8, 'big')
    return base64.urlsafe_b64encode(raw).rstrip(b'=').decode('ascii')

_KEY = rsa.generate_private_key(public_exponent=65537, key_size=2048)
PRIVATE_KEY = _KEY.private_bytes(
    serialization.Encoding.PEM,
    serialization.PrivateFormat.PKCS8,
    serialization.NoEncryption(),
)
_PUB = _KEY.public_key().public_numbers()
JWKS = {
  "keys": [
    {
      "kty": "RSA",
      "alg": "RS256",
      "use": "sig",
      "kid": "test-key-id",
      "n": _b64url_uint(_PUB.n),
      "e": _b64url_uint(_PUB.e),
    }
  ]
}

class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == '/.well-known/openid-configuration':
            body = {
                'issuer': 'http://mock-oidc:18080',
                'authorization_endpoint': 'http://mock-oidc:18080/auth',
                'token_endpoint': 'http://mock-oidc:18080/token',
                'jwks_uri': 'http://mock-oidc:18080/keys'
            }
            data = json.dumps(body).encode('utf-8')
            self.send_response(200)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(data)))
            self.end_headers()
            self.wfile.write(data)
            return
        if self.path == '/keys':
            data = json.dumps(JWKS).encode('utf-8')
            self.send_response(200)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(data)))
            self.end_headers()
            self.wfile.write(data)
            return
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b'ok')

    def do_POST(self):
        if self.path == '/token':
            payload = {
                'iss': 'http://mock-oidc:18080',
                'aud': 'sam-e2e',
                'sub': 'test-user',
                'exp': int(time.time()) + 3600,
                'roles': ['data-scientist'] # Custom role
            }
            token = jwt.encode(payload, PRIVATE_KEY, algorithm='RS256', headers={'kid': 'test-key-id'})
            body = {
                'access_token': token,
                'token_type': 'Bearer',
                'expires_in': 3600
            }
            data = json.dumps(body).encode('utf-8')
            self.send_response(200)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(data)))
            self.end_headers()
            self.wfile.write(data)
            return
        self.send_response(404)
        self.end_headers()

print("Mock OIDC server ready", flush=True)
HTTPServer(('0.0.0.0', 18080), Handler).serve_forever()
PY
EOF

    docker run -d \
      --name "${name}" \
      --network "${MESH_NETWORK}" \
      --network-alias mock-oidc \
      sam-mock-oidc:local \
      sh -c "${cmd}" >/dev/null

    MESH_CONTAINERS+=("${name}")
    mesh_wait_for_log "${name}" "Mock OIDC server ready" 30
}



setup() {
  mesh_setup_env
  build_calc_mcp_image
  mkdir -p tests/e2e/logs

  local node_policy="version: \"v1alpha1\"
services:
  - type: \"mcp\"
    name: \"calculator\"
    description: \"Simple math operations\"
    target_url: \"http://calc-mcp:7777/mcp\"
  - type: \"mcp\"
    name: \"db-agent\"
    description: \"Database operations\"
    target_url: \"http://calc-mcp:7777/mcp\"
attenuation:
  policies:
    - 'deny if service(\"mcp\", \"db-agent\");'"

  local config_file="/tmp/${MESH_PREFIX}-local_policy.yaml"
  echo "${node_policy}" > "${config_file}"

  # Start services
  start_calc_mcp

  # Initialize router PeerID from suite-level file
  mesh_start_router

  # Start Node 1 (Target) with local policy file
  mesh_start_node 1 "" "${config_file}"
  mesh_wait_for_log "${MESH_PREFIX}-node-1" "Successfully enrolled" 20

  # Start Node 2 (Caller)
  mesh_start_node 2
  mesh_wait_for_log "${MESH_PREFIX}-node-2" "SAM Node Online" 20
  mesh_wait_for_mcp_ready 2

  local node2_id
  node2_id=$(docker logs "${MESH_PREFIX}-node-2" 2>&1 | grep "PeerID:" | grep -oE '12D3Koo[a-zA-Z0-9]+' | head -n 1)

  # Wait for discovery (Node 2 should see Node 1)
  local i
  local router_id
  router_id="$(cat "/tmp/${MESH_PREFIX}-router-peer-id")"
  export TARGET_PEER_ID=""
  
  for ((i=0; i<40; i++)); do
    local output
    output="$(docker run --rm --network "${MESH_NETWORK}" "${MESH_RUNTIME_IMAGE}" mcp-client -url "http://${MESH_PREFIX}-node-2:8080/mcp" -tool "get_mesh_info")"
    TARGET_PEER_ID=$(echo "${output}" | grep -oE '12D3Koo[a-zA-Z0-9]+' | grep -v "${router_id}" | grep -v "${node2_id}" | head -n 1)
    if [[ -n "${TARGET_PEER_ID}" ]]; then
      break
    fi
    sleep 1
  done

  echo "Node 2 logs after discovery loop:" >&3
  docker logs "${MESH_PREFIX}-node-2" >&3
  
  if [[ -z "${TARGET_PEER_ID}" ]]; then
    echo "Timeout waiting for discovery of Node 1"
    return 1
  fi

  # Explicitly connect Node 2 to Node 1 to avoid "no addresses" error
  local node1_addr="/dns4/${MESH_PREFIX}-node-1/tcp/5002/p2p/${TARGET_PEER_ID}"
  mesh_connect_peer 2 "${node1_addr}" >/dev/null
}

teardown() {
  if [[ "${BATS_TEST_COMPLETED:-0}" -ne 1 ]]; then
    mkdir -p tests/e2e/logs
    local ids
    ids="$(docker ps -aq --filter "name=mesh-")"
    for id in ${ids}; do
      local name
      name="$(docker inspect -f '{{.Name}}' "${id}" | tr -d '/')"
      docker logs "${id}" > "tests/e2e/logs/${name}.log" 2>&1 || true
    done
  fi
  mesh_cleanup_env
  rm -f "/tmp/${MESH_PREFIX}-local_policy.yaml" || true
}

@test "Policy E2E: Positive Path (Allowed by control plane, Not blocked by Node)" {
  local call_args="{\"peer_id\":\"${TARGET_PEER_ID}\",\"tool_name\":\"mcp://calculator/add\",\"arguments\":{\"a\":2,\"b\":3}}"
  run docker run --rm --network "${MESH_NETWORK}" "${MESH_RUNTIME_IMAGE}" mcp-client -url "http://${MESH_PREFIX}-node-2:8080/mcp" -tool "call_remote_tool" -args "${call_args}"
  echo "Output: $output"
  [ "$status" -eq 0 ]
  [[ "$output" == *"5"* ]]
}

@test "Policy E2E: Negative Path (Denied by control plane)" {
  local call_args="{\"peer_id\":\"${TARGET_PEER_ID}\",\"tool_name\":\"mcp://unauthorized-service/reboot_server\",\"arguments\":{}}"
  run docker run --rm --network "${MESH_NETWORK}" "${MESH_RUNTIME_IMAGE}" mcp-client -url "http://${MESH_PREFIX}-node-2:8080/mcp" -tool "call_remote_tool" -args "${call_args}"
  echo "Output: $output"
  [[ "$output" == *"denied"* ]]
}

@test "Policy E2E: Attenuation Path (Allowed by control plane, Blocked by Node)" {
  local call_args="{\"peer_id\":\"${TARGET_PEER_ID}\",\"tool_name\":\"mcp://db-agent/delete_tables\",\"arguments\":{}}"
  run docker run --rm --network "${MESH_NETWORK}" "${MESH_RUNTIME_IMAGE}" mcp-client -url "http://${MESH_PREFIX}-node-2:8080/mcp" -tool "call_remote_tool" -args "${call_args}"
  echo "Output: $output"
  [[ "$output" == *"denied"* ]]
}
