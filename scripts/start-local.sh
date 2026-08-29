#!/usr/bin/env bash
set -Eeuo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CONFIG_FILE="${CONTEXT_LENS_CONFIG:-$ROOT_DIR/config.local.json}"
RUN_DIR="${CONTEXT_LENS_RUN_DIR:-$ROOT_DIR/.context-lens-run}"
BACKEND_ADDR="${CONTEXT_LENS_ADDR:-127.0.0.1:3001}"
FRONTEND_HOST="${CONTEXT_LENS_FRONTEND_HOST:-127.0.0.1}"
FRONTEND_PORT="${CONTEXT_LENS_FRONTEND_PORT:-5172}"
OPEN_BROWSER="${CONTEXT_LENS_OPEN_BROWSER:-0}"
KILL_EXISTING="${CONTEXT_LENS_KILL_EXISTING:-0}"
START_MOCK="${CONTEXT_LENS_START_MOCK:-1}"

MOCK_PID=""
BACKEND_PID=""
FRONTEND_PID=""

port_in_use() {
  local port="$1"
  command -v lsof >/dev/null 2>&1 && lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1
}

# Listeners on a port (pid + command), one per line. Empty when free.
port_listeners() {
  local port="$1"
  lsof -nP -iTCP:"$port" -sTCP:LISTEN -Fpc 2>/dev/null | paste -d' ' - -
}

kill_port_listeners() {
  local port="$1"
  local pids
  pids="$(lsof -tiTCP:"$port" -sTCP:LISTEN 2>/dev/null || true)"
  if [[ -z "$pids" ]]; then
    return 0
  fi
  echo "Stopping listeners on port $port:"
  while read -r pid; do
    [[ -n "$pid" ]] || continue
    echo "  pid $pid ($(ps -p "$pid" -o command= 2>/dev/null | cut -c1-80))"
  done <<<"$pids"
  # shellcheck disable=SC2086
  kill $pids 2>/dev/null || true
  for _ in $(seq 1 20); do
    if ! port_in_use "$port"; then
      return 0
    fi
    sleep 0.25
  done
  echo "Port $port still in use after SIGTERM; forcing." >&2
  # shellcheck disable=SC2086
  kill -9 $pids 2>/dev/null || true
  for _ in $(seq 1 8); do
    if ! port_in_use "$port"; then
      return 0
    fi
    sleep 0.25
  done
  echo "Port $port is still in use; stop it manually and retry." >&2
  return 1
}

# Handle a busy port: kill when CONTEXT_LENS_KILL_EXISTING=1, ask when
# interactive (the prompt preselects "yes" since rerunning the launcher is the
# usual way to restart), fail otherwise. Never kills without consent.
resolve_port_conflict() {
  local port="$1"
  local label="$2"
  local env_hint="$3"
  if ! port_in_use "$port"; then
    return 0
  fi
  if [[ "$KILL_EXISTING" == "1" ]]; then
    echo "$label port $port is already in use; stopping the existing process (CONTEXT_LENS_KILL_EXISTING=1)."
    kill_port_listeners "$port"
    return
  fi
  if [[ -t 0 ]]; then
    echo "$label port $port is already in use by:"
    port_listeners "$port" | sed 's/^/  /'
    local answer="y"
    read -r -p "Stop it and continue? [Y/n] " answer || true
    if [[ "$answer" == "" || "$answer" == "y" || "$answer" == "Y" ]]; then
      kill_port_listeners "$port"
      return
    fi
    echo "Keeping the existing process. $env_hint" >&2
    exit 1
  fi
  echo "$label port $port is already in use; stop the existing process, or rerun with CONTEXT_LENS_KILL_EXISTING=1. $env_hint" >&2
  exit 1
}

backend_port="${BACKEND_ADDR##*:}"
resolve_port_conflict "$backend_port" "Backend" "Set CONTEXT_LENS_ADDR to use another loopback port."
resolve_port_conflict "$FRONTEND_PORT" "Frontend" "Set CONTEXT_LENS_FRONTEND_PORT to use another port."

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
