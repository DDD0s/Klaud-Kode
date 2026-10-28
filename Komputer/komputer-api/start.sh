#!/usr/bin/env bash
# Start the Klaud Kode API on the brain host. Paths come from where this script
# lives, so the checkout can sit anywhere.
#
# The service runs a checksum-verified precompiled Klaud Kode release. The
# launcher downloads the version named by Komputer/VERSION (or
# KLAUD_KODE_VERSION) into ./bin on first start. Binaries are kept per version,
# so an upgrade never overwrites the file a running service still holds.
# KOMPUTER_IMPL=node runs the previous JavaScript implementation instead, which
# stays in the tree as the rollback path.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MODULE="$(dirname "$DIR")"
PIDF="$DIR/komputer-api.pid"
LOG="$DIR/logs/komputer-api.log"
BIN=""

# Optional per-host settings: proxy, listen address, body name, release origin.
# Copy env.example to env and edit it; it is git-ignored.
if [[ -f "$DIR/env" ]]; then
  set -a
  # shellcheck disable=SC1091
  source "$DIR/env"
  set +a
fi

# Pre-rename variable names still work, so an untouched env file keeps working.
: "${KLAUD_KODE_BIN:=${KLOUD_KODE_BIN:-}}"
: "${KLAUD_KODE_VERSION:=${KLOUD_KODE_VERSION:-}}"
: "${KLAUD_KODE_REPO:=${KLOUD_KODE_REPO:-}}"
: "${KLAUD_KODE_RELEASE_BASE:=${KLOUD_KODE_RELEASE_BASE:-}}"
: "${KLAUD_KODE_BUILD:=${KLOUD_KODE_BUILD:-}}"
fail() { echo "ERROR: $*" >&2; exit 1; }

verify_sum() {
  local dir="$1" asset="$2" want got
  want="$(grep -E "^[0-9a-fA-F]{64} [ *]$asset$" "$dir/SHA256SUMS" | head -1 | cut -d" " -f1)"
  [[ -n "$want" ]] || fail "SHA256SUMS has no entry for $asset"
  if command -v sha256sum >/dev/null 2>&1; then got="$(sha256sum "$dir/$asset" | cut -d" " -f1)"
  elif command -v shasum >/dev/null 2>&1; then got="$(shasum -a 256 "$dir/$asset" | cut -d" " -f1)"
  elif command -v openssl >/dev/null 2>&1; then got="$(openssl dgst -sha256 -r "$dir/$asset" | cut -d" " -f1)"
  else fail "no SHA-256 tool found (need sha256sum, shasum or openssl)"; fi
  [[ "${got,,}" == "${want,,}" ]] || fail "checksum mismatch for $asset"
  echo "$asset: OK"
}

prepare() {
  if [[ -n "${KLAUD_KODE_BIN:-}" ]]; then
    [[ -x "$KLAUD_KODE_BIN" ]] || fail "KLAUD_KODE_BIN is not executable: $KLAUD_KODE_BIN"
    BIN="$KLAUD_KODE_BIN"
    return 0
  fi
  local requested="${KLAUD_KODE_VERSION:-}" os arch ext asset tmp repo base
  if [[ -z "$requested" && -f "$MODULE/VERSION" ]]; then requested="$(tr -d "[:space:]" < "$MODULE/VERSION")"; fi
  [[ -n "$requested" ]] || requested="latest"
  case "$(uname -s)" in Linux*) os=linux ;; Darwin*) os=darwin ;; MINGW*|MSYS*|CYGWIN*) os=windows ;; *) fail "unsupported operating system: $(uname -s)" ;; esac
  case "$(uname -m)" in x86_64|amd64) arch=amd64 ;; arm64|aarch64) arch=arm64 ;; *) fail "unsupported architecture: $(uname -m)" ;; esac
  ext=""; if [[ "$os" == windows ]]; then ext=".exe"; fi
  repo="${KLAUD_KODE_REPO:-DDD0s/Klaud-Kode}"
  if [[ "$requested" == latest ]]; then
    command -v curl >/dev/null 2>&1 || fail "curl is needed to resolve the latest release"
    requested="$(curl -fsSL "https://api.github.com/repos/$repo/releases/latest" | sed -n "s/.*\"tag_name\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p" | head -1)"
    [[ -n "$requested" ]] || fail "could not resolve the latest Klaud Kode release"
  fi
  cd "$DIR"
  BIN="$DIR/bin/klaud-kode-$requested$ext"
  if [[ -x "$BIN" ]]; then printf "%s\n" "$requested" > "$DIR/.klaud-kode-version"; return 0; fi
  if [[ "${KLAUD_KODE_BUILD:-}" == 1 ]]; then
    command -v go >/dev/null 2>&1 || fail "KLAUD_KODE_BUILD=1 needs go on PATH"
    echo "building $BIN from source"
    (cd "$MODULE" && go build -trimpath -ldflags "-X github.com/DDD0s/Klaud-Kode/Komputer/internal/version.Value=$requested" -o "$BIN" ./cmd/klaud-kode) || fail "go build failed"
    printf "%s\n" "$requested" > "$DIR/.klaud-kode-version"
    return 0
  fi
  command -v curl >/dev/null 2>&1 || fail "curl is needed to download the release"
  base="${KLAUD_KODE_RELEASE_BASE:-https://github.com/$repo/releases}"
  asset="klaud-kode-$requested-$os-$arch"; if [[ "$os" == windows ]]; then asset="$asset.zip"; else asset="$asset.tar.gz"; fi
  tmp="$(mktemp -d)"
  trap "rm -rf \"$tmp\"" EXIT
  echo "downloading Klaud Kode $requested ($os/$arch)"
  curl -fL --retry 3 -o "$tmp/$asset" "$base/download/$requested/$asset" || fail "download failed: $base/download/$requested/$asset"
  curl -fL --retry 3 -o "$tmp/SHA256SUMS" "$base/download/$requested/SHA256SUMS" || fail "could not download SHA256SUMS"
  ( cd "$tmp" && verify_sum "$tmp" "$asset" )
  mkdir -p "$tmp/extract" "$DIR/bin"
  if [[ "$os" == windows ]]; then
    command -v unzip >/dev/null 2>&1 || fail "unzip is needed on Windows"
    unzip -o -q "$tmp/$asset" -d "$tmp/extract"
  else
    tar -xzf "$tmp/$asset" -C "$tmp/extract"
  fi
  # Install under the versioned name so a running service keeps its own file.
  cp -f "$tmp/extract/klaud-kode$ext" "$BIN"
  chmod 0755 "$BIN"
  "$BIN" version >/dev/null || fail "the downloaded binary does not run"
  printf "%s\n" "$requested" > "$DIR/.klaud-kode-version"
}

if [[ "${KOMPUTER_IMPL:-go}" == "node" ]]; then
  NODE="${NODE_BIN:-$(command -v node || true)}"
  [[ -n "$NODE" ]] || fail "node not found. Install Node 20+ or set NODE_BIN in $DIR/env"
  RUN=("$NODE" "$DIR/server.mjs")
else
  prepare
  RUN=("$BIN" api)
fi

case "${1:-}" in
  --prepare|--build-only)
    if [[ -n "$BIN" ]]; then "$BIN" version; else echo "implementation: node ($NODE)"; fi
    exit 0
    ;;
esac

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

# A brand-new binary can have its first writes denied while the host antivirus
# scans it; one retry covers that without hiding real startup errors, which
# still leave the process dead.
if ! launch; then
  sleep 1
  if ! launch; then
    echo "ERROR: failed to start — see $LOG" >&2
    exit 1
  fi
fi
echo "started pid=$(cat "$PIDF") http://${KOMPUTER_API_HOST}:${KOMPUTER_API_PORT} max_sessions=${KOMPUTER_MAX_SESSIONS} max_in_flight=${KOMPUTER_MAX_IN_FLIGHT}"
