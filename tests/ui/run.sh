#!/bin/bash
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
#
# Boots a control plane and console against a throwaway sqlite DB, then runs the
# Playwright console smoke test against them. No docker or kind required.

set -o errexit
set -o nounset
set -o pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "${REPO_ROOT}"

export SAM_ADMIN_TOKEN="ui-test-admin-token"
export SAM_CONSOLE_URL="http://127.0.0.1:8091"
OIDC_ISSUER="http://127.0.0.1:18090"

WORK_DIR=$(mktemp -d)
PIDS=()

cleanup() {
  for pid in "${PIDS[@]:-}"; do
    kill_tree "${pid}"
    wait "${pid}" 2>/dev/null || true
  done
  rm -rf "${WORK_DIR}"
}

# $! can name a wrapper rather than the server itself (python3 is a shim on some
# systems), so walk the descendants instead of trusting the recorded pid alone.
kill_tree() {
  local pid="$1" child
  for child in $(pgrep -P "${pid}" 2>/dev/null || true); do
    kill_tree "${child}"
  done
  kill "${pid}" 2>/dev/null || true
}
trap cleanup EXIT INT TERM

wait_for() {
  local url="$1" name="$2"
  for _ in $(seq 1 50); do
    if curl -fsS -o /dev/null "${url}" 2>/dev/null; then
      return 0
    fi
    sleep 0.2
  done
  echo "timed out waiting for ${name} at ${url}" >&2
  cat "${WORK_DIR}/${name}.log" >&2 || true
  return 1
}

echo "Starting mock OIDC discovery endpoint..."
# The console smoke test authenticates with the admin token, so the issuer only
# has to satisfy the control plane's startup discovery. A static document is enough.
mkdir -p "${WORK_DIR}/oidc/.well-known"
cat >"${WORK_DIR}/oidc/.well-known/openid-configuration" <<EOF
{
  "issuer": "${OIDC_ISSUER}",
  "authorization_endpoint": "${OIDC_ISSUER}/auth",
  "token_endpoint": "${OIDC_ISSUER}/token",
  "jwks_uri": "${OIDC_ISSUER}/keys"
}
EOF
echo '{"keys":[]}' >"${WORK_DIR}/oidc/keys"
python3 -m http.server 18090 --bind 127.0.0.1 --directory "${WORK_DIR}/oidc" \
  >"${WORK_DIR}/oidc.log" 2>&1 &
PIDS+=($!)
wait_for "${OIDC_ISSUER}/.well-known/openid-configuration" "oidc"

echo "Starting control plane..."
"${REPO_ROOT}/bin/sam-control-plane" \
  --bind-address "127.0.0.1:8090" \
  --db-dsn "${WORK_DIR}/console-ui-test.db" \
  --issuer "${OIDC_ISSUER}" \
  --auto-approve-enrollment \
  >"${WORK_DIR}/control-plane.log" 2>&1 &
PIDS+=($!)
wait_for "http://127.0.0.1:8090/info" "control-plane"

echo "Starting console..."
"${REPO_ROOT}/bin/sam-console" \
  --control-plane "http://127.0.0.1:8090" \
  --bind-addr "127.0.0.1:8091" \
  --static-dir "${REPO_ROOT}/cmd/sam-console/public" \
  >"${WORK_DIR}/console.log" 2>&1 &
PIDS+=($!)
wait_for "${SAM_CONSOLE_URL}/" "console"

cd "${REPO_ROOT}/tests/ui"
npm ci --no-audit --no-fund
# --with-deps needs root to install OS libraries; only CI runners allow that.
if [[ -n "${CI:-}" ]]; then
  npx playwright install --with-deps chromium
else
  npx playwright install chromium
fi
npx playwright test "$@"
