#!/usr/bin/env bash
set -Eeuo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CONFIG_FILE="${CONTEXT_LENS_CONFIG:-$ROOT_DIR/config.local.json}"
RUN_DIR="${CONTEXT_LENS_RUN_DIR:-$ROOT_DIR/.context-lens-run}"
BACKEND_ADDR="${CONTEXT_LENS_ADDR:-127.0.0.1:8080}"
FRONTEND_HOST="${CONTEXT_LENS_FRONTEND_HOST:-127.0.0.1}"
FRONTEND_PORT="${CONTEXT_LENS_FRONTEND_PORT:-5173}"
OPEN_BROWSER="${CONTEXT_LENS_OPEN_BROWSER:-1}"
START_MOCK="${CONTEXT_LENS_START_MOCK:-1}"

MOCK_PID=""
BACKEND_PID=""
FRONTEND_PID=""

cleanup() {
  trap - EXIT INT TERM
  for pid in "$FRONTEND_PID" "$BACKEND_PID" "$MOCK_PID"; do
    if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
      kill "$pid" 2>/dev/null || true
    fi
  done
  wait "$FRONTEND_PID" "$BACKEND_PID" "$MOCK_PID" 2>/dev/null || true
}
trap cleanup EXIT INT TERM

if [[ ! -f "$CONFIG_FILE" ]]; then
  cp "$ROOT_DIR/config.example.json" "$CONFIG_FILE"
  chmod 600 "$CONFIG_FILE"
  echo "Created local config: $CONFIG_FILE"
  echo "Edit base_url and api_key there if you are not using the bundled mock."
else
  chmod 600 "$CONFIG_FILE"
fi

mkdir -p "$RUN_DIR"

if [[ "$START_MOCK" == "1" ]]; then
  CONFIG_BASE_URL="$(python3 - "$CONFIG_FILE" <<'PY'
import json
import sys
try:
    with open(sys.argv[1], encoding="utf-8") as handle:
        print(json.load(handle).get("base_url", "").rstrip("/"))
except Exception:
    print("")
PY
)"
  if [[ "$CONFIG_BASE_URL" == "http://127.0.0.1:19091" ]]; then
    MOCK_SSE_DELAY_MS="${MOCK_SSE_DELAY_MS:-30}" python3 -u "$ROOT_DIR/scripts/mock-upstream.py" >"$RUN_DIR/mock.log" 2>&1 &
    MOCK_PID=$!
  else
    echo "Skipping bundled mock: config base_url is not the local fixture upstream."
  fi
fi

(
  cd "$ROOT_DIR"
  CONTEXT_LENS_ADDR="$BACKEND_ADDR" go run ./cmd/context-lens -config "$CONFIG_FILE"
) >"$RUN_DIR/backend.log" 2>&1 &
BACKEND_PID=$!

if [[ ! -x "$ROOT_DIR/frontend/node_modules/.bin/vite" ]]; then
  echo "Installing frontend dependencies..."
  (cd "$ROOT_DIR/frontend" && npm ci --no-audit --no-fund)
fi

(
  cd "$ROOT_DIR/frontend"
  CONTEXT_LENS_BACKEND_URL="http://$BACKEND_ADDR" npm run dev -- --host "$FRONTEND_HOST" --port "$FRONTEND_PORT"
) >"$RUN_DIR/frontend.log" 2>&1 &
FRONTEND_PID=$!

wait_for_url() {
  local url="$1"
  local name="$2"
  for _ in $(seq 1 80); do
    if curl --fail --silent --show-error "$url" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.25
  done
  echo "$name did not start. See $RUN_DIR/*.log" >&2
  return 1
}

wait_for_url "http://$BACKEND_ADDR/healthz" "backend"
wait_for_url "http://$FRONTEND_HOST:$FRONTEND_PORT/" "frontend"

cat <<EOF

context-lens is running:
  Workbench: http://$FRONTEND_HOST:$FRONTEND_PORT/
  Backend:   http://$BACKEND_ADDR/
  Workspace: http://$BACKEND_ADDR/api/exchanges
  Config:    $CONFIG_FILE
  Logs:      $RUN_DIR/

Use Ctrl-C to stop all local processes.
EOF

if [[ "$OPEN_BROWSER" == "1" ]] && command -v open >/dev/null 2>&1; then
  open "http://$FRONTEND_HOST:$FRONTEND_PORT/" >/dev/null 2>&1 || true
fi

while true; do
  if [[ -n "$BACKEND_PID" ]] && ! kill -0 "$BACKEND_PID" 2>/dev/null; then
    echo "backend exited; see $RUN_DIR/backend.log" >&2
    exit 1
  fi
  if [[ -n "$FRONTEND_PID" ]] && ! kill -0 "$FRONTEND_PID" 2>/dev/null; then
    echo "frontend exited; see $RUN_DIR/frontend.log" >&2
    exit 1
  fi
  sleep 1
done
