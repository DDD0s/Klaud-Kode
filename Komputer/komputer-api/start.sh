#!/usr/bin/env bash
# Start the Kloud Kode API on the brain host. Paths come from where this script
# lives, so the checkout can sit anywhere.
#
# The service runs a checksum-verified precompiled Kloud Kode release.
# The launcher downloads the version named by Komputer/VERSION (or
# KLOUD_KODE_VERSION / the latest release) beside this script on first start.
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

ensure_binary() {
  if [[ -n "${KLOUD_KODE_BIN:-}" ]]; then
    [[ -x "$KLOUD_KODE_BIN" ]] || { echo "ERROR: KLOUD_KODE_BIN is not executable: $KLOUD_KODE_BIN" >&2; exit 1; }
    BIN="$KLOUD_KODE_BIN"
    return 0
  fi
  local requested="${KLOUD_KODE_VERSION:-}" os arch ext bin_name asset tmp repo base marker
  if [[ -z "$requested" && -f "$MODULE/VERSION" ]]; then requested="$(tr -d '[:space:]' < "$MODULE/VERSION")"; fi
  [[ -n "$requested" ]] || requested="latest"
  case "$(uname -s)" in Linux*) os=linux ;; Darwin*) os=darwin ;; MINGW*|MSYS*|CYGWIN*) os=windows ;; *) echo "unsupported operating system: $(uname -s)" >&2; exit 1 ;; esac
  case "$(uname -m)" in x86_64|amd64) arch=amd64 ;; arm64|aarch64) arch=arm64 ;; *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;; esac
  ext=""; bin_name="kloud-kode"; if [[ "$os" == windows ]]; then ext=".exe"; bin_name="kloud-kode.exe"; fi
  BIN="$DIR/$bin_name"; marker="$DIR/.kloud-kode-version"
  repo="${KLOUD_KODE_REPO:-DDD0s/Kloud-Kode}"
  if [[ "$requested" == latest ]]; then requested="$(curl -fsSL "https://api.github.com/repos/$repo/releases/latest" | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)"; fi
  [[ -n "$requested" ]] || { echo "ERROR: could not resolve the Kloud Kode release" >&2; exit 1; }
  if [[ -x "$BIN" && -f "$marker" ]] && [[ "$(tr -d '[:space:]' < "$marker")" == "$requested" ]]; then return 0; fi
  if [[ "${KLOUD_KODE_BUILD:-}" == 1 ]] && command -v go >/dev/null 2>&1; then echo "building $BIN from source (KLOUD_KODE_BUILD=1)"; (cd "$MODULE" && go build -trimpath -o "$BIN" ./cmd/kloud-kode); printf "%s\n" "$requested" > "$marker"; return 0; fi
  base="${KLOUD_KODE_RELEASE_BASE:-https://github.com/$repo/releases}"
  asset="kloud-kode-$requested-$os-$arch"; if [[ "$os" == windows ]]; then asset="$asset.zip"; else asset="$asset.tar.gz"; fi
  tmp="$(mktemp -d)"; echo "downloading Kloud Kode $requested ($os/$arch)"
  curl -fL --retry 3 -o "$tmp/$asset" "$base/download/$requested/$asset"
  curl -fL --retry 3 -o "$tmp/SHA256SUMS" "$base/download/$requested/SHA256SUMS"
  (cd "$tmp" && grep " $asset$" SHA256SUMS > asset.sha256 && sha256sum -c asset.sha256)
  mkdir -p "$tmp/extract"; if [[ "$os" == windows ]]; then unzip -o -q "$tmp/$asset" -d "$tmp/extract"; else tar -xzf "$tmp/$asset" -C "$tmp/extract"; fi
  install -m 0755 "$tmp/extract/$bin_name" "$BIN"; rm -rf "$tmp"
  printf "%s\n" "$requested" > "$marker"; "$BIN" version >/dev/null
}

if [[ "${KOMPUTER_IMPL:-go}" == "node" ]]; then
  NODE="${NODE_BIN:-$(command -v node || true)}"
  if [[ -z "$NODE" ]]; then
    echo "ERROR: node not found. Install Node 20+ or set NODE_BIN in $DIR/env" >&2
    exit 1
  fi
  RUN=("$NODE" "$DIR/server.mjs")
else
  ensure_binary
  RUN=("$BIN" api)
fi

if [[ "${1:-}" == "--build-only" ]]; then
  "$BIN" version
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
