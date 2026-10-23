#!/usr/bin/env bash
# Start the Kloud Kode API on the brain host. Paths come from where this script
# lives, so the checkout can sit anywhere.
#
# The service is the Go binary built from Komputer/cmd/kloud-kode. It is built
# here on first start and whenever a source file is newer than the binary.
# KOMPUTER_IMPL=node runs the previous JavaScript implementation instead, which
# stays in the tree as the rollback path.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MODULE="$(dirname "$DIR")"
BIN="$DIR/kloud-kode"
PIDF="$DIR/komputer-api.pid"
LOG="$DIR/logs/komputer-api.log"

# Optional per-host settings: proxy, listen address, body name, toolchain path.
# Copy env.example to env and edit it; it is git-ignored.
if [[ -f "$DIR/env" ]]; then
  set -a
  # shellcheck disable=SC1091
  source "$DIR/env"
  set +a
fi

GO="${GO_BIN:-$(command -v go || true)}"

build_binary() {
  local stale=0
  if [[ ! -x "$BIN" ]]; then
    stale=1
  elif [[ -n "$(find "$MODULE/cmd" "$MODULE/internal" -name '*.go' -newer "$BIN" -print -quit 2>/dev/null)" ]]; then
    stale=1
  elif [[ "$MODULE/go.mod" -nt "$BIN" ]] || [[ "$MODULE/go.sum" -nt "$BIN" ]]; then
    stale=1
  fi
  if [[ "$stale" == "0" ]]; then
    return 0
  fi
  if [[ -z "$GO" ]]; then
    echo "ERROR: go not found and $BIN is missing or stale. Install Go 1.27+ or set GO_BIN in $DIR/env" >&2
    echo "       (KOMPUTER_IMPL=node runs the JavaScript implementation instead)" >&2
    exit 1
  fi
  echo "building $BIN"
  # The binary sits in this directory, so the service resolves KEYS.txt,
  # sessions.json and the session directory exactly where the JS version did.
  (cd "$MODULE" && "$GO" build -trimpath -o "$BIN" ./cmd/kloud-kode)
}

if [[ "${KOMPUTER_IMPL:-go}" == "node" ]]; then
  NODE="${NODE_BIN:-$(command -v node || true)}"
  if [[ -z "$NODE" ]]; then
    echo "ERROR: node not found. Install Node 20+ or set NODE_BIN in $DIR/env" >&2
    exit 1
  fi
  RUN=("$NODE" "$DIR/server.mjs")
else
  build_binary
  RUN=("$BIN" api)
fi

if [[ "${1:-}" == "--build-only" ]]; then
  echo "built $BIN"
  exit 0
fi

mkdir -p "$DIR/logs"

export KOMPUTER_API_HOST="${KOMPUTER_API_HOST:-127.0.0.1}"
export KOMPUTER_API_PORT="${KOMPUTER_API_PORT:-18888}"
export KOMPUTER_MAX_SESSIONS="${KOMPUTER_MAX_SESSIONS:-0}"
export KOMPUTER_MAX_IN_FLIGHT="${KOMPUTER_MAX_IN_FLIGHT:-4}"
export KOMPUTER_IDLE_TIMEOUT_MS="${KOMPUTER_IDLE_TIMEOUT_MS:-2700000}"

if [[ -f "$PIDF" ]]; then
  old=$(cat "$PIDF" || true)
  if [[ -n "${old:-}" ]] && kill -0 "$old" 2>/dev/null; then
    echo "already running pid=$old"
    exit 0
  fi
fi

# Best-effort tunnel so the first request is faster.
PROJECT="$(cd "$MODULE/.." && pwd)"
TUNNEL="${KOMPUTER_TUNNEL_UP:-$PROJECT/komputer/tunnel-up.sh}"
if [[ -x "$TUNNEL" ]]; then
  "$TUNNEL" </dev/null >/dev/null 2>&1 || true
fi

# Run from the api directory so relative defaults land here even if the binary
# is launched from somewhere else.
cd "$DIR"
launch() {
  nohup "${RUN[@]}" </dev/null >>"$LOG" 2>&1 &
  echo $! >"$PIDF"
  sleep 0.6
  kill -0 "$(cat "$PIDF")" 2>/dev/null
}

# A brand-new binary can have its first writes denied while the host's
# antivirus scans it; one immediate retry covers that without hiding real
# startup errors, which still leave the process dead.
if ! launch; then
  sleep 1
  if ! launch; then
    echo "ERROR: failed to start — see $LOG" >&2
    exit 1
  fi
fi
echo "started pid=$(cat "$PIDF") http://${KOMPUTER_API_HOST}:${KOMPUTER_API_PORT} max_sessions=${KOMPUTER_MAX_SESSIONS} max_in_flight=${KOMPUTER_MAX_IN_FLIGHT}"
