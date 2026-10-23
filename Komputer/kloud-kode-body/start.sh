#!/usr/bin/env bash
# Start the Kloud Kode Body on the computer it should operate.
#
# The service is the Go binary built from Komputer/cmd/kloud-kode. It is built
# here on first start and whenever a source file is newer than the binary.
# KOMPUTER_IMPL=node runs the previous JavaScript implementation (rollback).
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MODULE="$(dirname "$DIR")"
BIN="$DIR/kloud-kode"
GO="${GO_BIN:-$(command -v go || true)}"
MODE="${1:-http}"

if [[ "${KOMPUTER_IMPL:-go}" == "node" ]]; then
  NODE="${NODE_BIN:-$(command -v node || true)}"
  [[ -n "$NODE" ]] || { echo "ERROR: node not found (set NODE_BIN)" >&2; exit 1; }
  exec "$NODE" "$DIR/server.mjs" "$MODE"
fi

stale=0
if [[ ! -x "$BIN" ]]; then
  stale=1
elif [[ -n "$(find "$MODULE/cmd" "$MODULE/internal" -name '*.go' -newer "$BIN" -print -quit 2>/dev/null)" ]]; then
  stale=1
elif [[ "$MODULE/go.mod" -nt "$BIN" ]]; then
  stale=1
fi
if [[ -n "$GO" ]] && [[ "$stale" == "1" ]]; then
  echo "building $BIN"
  (cd "$MODULE" && "$GO" build -trimpath -o "$BIN" ./cmd/kloud-kode)
elif [[ ! -x "$BIN" ]]; then
  echo "ERROR: go not found and $BIN is missing. Install Go 1.27+ or set GO_BIN" >&2
  exit 1
fi
cd "$DIR"
exec "$BIN" body "$MODE"
