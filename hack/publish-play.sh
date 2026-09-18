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

# Uploads an Android App Bundle to a Google Play track with the Play
# Developer Publishing API: open an edit, upload the bundle, point the track
# at the new versionCode, commit. Four calls, no SDK.
#
# Auth is a bearer token in PLAY_ACCESS_TOKEN, minted for the
# https://www.googleapis.com/auth/androidpublisher scope by a service account
# that Play Console lists under "Users and permissions" with release rights.
# The token reaches curl through a header file, never through argv.
#
# usage: publish-play.sh <package-name> <track> <bundle.aab>

set -o errexit
set -o nounset
set -o pipefail

if [[ $# -ne 3 ]]; then
  echo "usage: $0 <package-name> <track> <bundle.aab>" >&2
  exit 2
fi
package=$1
track=$2
bundle=$3

: "${PLAY_ACCESS_TOKEN:?PLAY_ACCESS_TOKEN must hold an androidpublisher access token}"
[[ -f "${bundle}" ]] || { echo "bundle not found: ${bundle}" >&2; exit 1; }
case "${track}" in
  internal|alpha|beta|production) ;;
  *) echo "track must be internal, alpha, beta or production (got '${track}')" >&2; exit 1 ;;
esac

api="https://androidpublisher.googleapis.com/androidpublisher/v3/applications/${package}"
upload="https://androidpublisher.googleapis.com/upload/androidpublisher/v3/applications/${package}"

auth_header=$(mktemp)
trap 'rm -f "${auth_header}"' EXIT
chmod 0600 "${auth_header}"
printf 'Authorization: Bearer %s\n' "${PLAY_ACCESS_TOKEN}" > "${auth_header}"

# Prints the response body on stdout. On a non-2xx the body is Play's own
# explanation (a 403 says whether the service account is missing from the
# console or the app does not exist yet), so it goes to stderr instead of
# being swallowed by the jq that would otherwise consume it.
play() {
  local body status
  body=$(curl --silent --show-error --write-out '\n%{http_code}' --header "@${auth_header}" "$@")
  status=${body##*$'\n'}
  body=${body%$'\n'*}
  if [[ "${status}" -lt 200 || "${status}" -ge 300 ]]; then
    echo "Play API returned HTTP ${status}:" >&2
    echo "${body}" >&2
    exit 1
  fi
  printf '%s' "${body}"
}

# A 2xx with the field missing would otherwise send us on to edits/null.
require() {
  [[ -n "$2" && "$2" != "null" ]] || { echo "$1" >&2; exit 1; }
}

edit_id=$(play -X POST "${api}/edits" -H 'Content-Type: application/json' -d '{}' | jq -r .id)
require "failed to open an edit: no id in the response" "${edit_id}"
echo "opened edit ${edit_id}"

version_code=$(play -X POST "${upload}/edits/${edit_id}/bundles?uploadType=media" \
  -H 'Content-Type: application/octet-stream' --data-binary "@${bundle}" | jq -r .versionCode)
require "bundle upload returned no versionCode" "${version_code}"
echo "uploaded ${bundle} as versionCode ${version_code}"

release=$(jq -nc --arg track "${track}" --arg vc "${version_code}" \
  '{track: $track, releases: [{versionCodes: [$vc], status: "completed"}]}')
play -X PUT "${api}/edits/${edit_id}/tracks/${track}" -H 'Content-Type: application/json' -d "${release}" >/dev/null

play -X POST "${api}/edits/${edit_id}:commit" >/dev/null
echo "published ${package} versionCode ${version_code} to the ${track} track"
