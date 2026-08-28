#!/usr/bin/env bash
set -Eeuo pipefail

PROXY_URL="${CONTEXT_LENS_PROXY_URL:-http://127.0.0.1:8080}"
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

post_fixture() {
  local route="$1"
  local fixture="$2"
  curl --fail --silent --show-error \
    -H 'Content-Type: application/json' \
    --data-binary "@${ROOT_DIR}/tests/fixtures/${fixture}" \
    "${PROXY_URL}${route}" >/dev/null
  printf 'seeded %-24s %s\n' "${fixture}" "${route}"
}

post_fixture /v1/responses responses/json/request.json
post_fixture /v1/chat/completions chat_completions/json/request.json
post_fixture /v1/messages anthropic_messages/json/request.json
post_fixture /v1/responses responses/sse/request.json
post_fixture /v1/chat/completions chat_completions/sse/request.json
post_fixture /v1/messages anthropic_messages/sse/request.json

printf 'workspace: %s/api/exchanges\n' "${PROXY_URL}"
