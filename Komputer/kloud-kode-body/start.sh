#!/usr/bin/env bash
# Start the Kloud Kode Body on the computer it should operate.
# Runs a checksum-verified precompiled release; no Go toolchain is needed.
# KLOUD_KODE_BUILD=1 builds from this checkout instead. KOMPUTER_IMPL=node
# runs the previous JavaScript implementation as the rollback path.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MODULE="$(dirname "$DIR")"
MODE="${1:-http}"

if [[ "${KOMPUTER_IMPL:-go}" == "node" ]]; then
  NODE="${NODE_BIN:-$(command -v node || true)}"
  [[ -n "$NODE" ]] || { echo "ERROR: node not found (set NODE_BIN)" >&2; exit 1; }
  exec "$NODE" "$DIR/server.mjs" "$MODE"
fi

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
  if [[ -n "${KLOUD_KODE_BIN:-}" ]]; then
    [[ -x "$KLOUD_KODE_BIN" ]] || fail "KLOUD_KODE_BIN is not executable: $KLOUD_KODE_BIN"
    BIN="$KLOUD_KODE_BIN"
    return 0
  fi
  local requested="${KLOUD_KODE_VERSION:-}" os arch ext asset tmp repo base
  if [[ -z "$requested" && -f "$MODULE/VERSION" ]]; then requested="$(tr -d "[:space:]" < "$MODULE/VERSION")"; fi
  [[ -n "$requested" ]] || requested="latest"
  case "$(uname -s)" in Linux*) os=linux ;; Darwin*) os=darwin ;; MINGW*|MSYS*|CYGWIN*) os=windows ;; *) fail "unsupported operating system: $(uname -s)" ;; esac
  case "$(uname -m)" in x86_64|amd64) arch=amd64 ;; arm64|aarch64) arch=arm64 ;; *) fail "unsupported architecture: $(uname -m)" ;; esac
  ext=""; if [[ "$os" == windows ]]; then ext=".exe"; fi
  repo="${KLOUD_KODE_REPO:-DDD0s/Kloud-Kode}"
  if [[ "$requested" == latest ]]; then
    command -v curl >/dev/null 2>&1 || fail "curl is needed to resolve the latest release"
    requested="$(curl -fsSL "https://api.github.com/repos/$repo/releases/latest" | sed -n "s/.*\"tag_name\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p" | head -1)"
    [[ -n "$requested" ]] || fail "could not resolve the latest Kloud Kode release"
  fi
  cd "$DIR"
  BIN="$DIR/bin/kloud-kode-$requested$ext"
  if [[ -x "$BIN" ]]; then return 0; fi
  if [[ "${KLOUD_KODE_BUILD:-}" == 1 ]]; then
    command -v go >/dev/null 2>&1 || fail "KLOUD_KODE_BUILD=1 needs go on PATH"
    echo "building $BIN from source"
    (cd "$MODULE" && go build -trimpath -ldflags "-X github.com/DDD0s/Kloud-Kode/Komputer/internal/version.Value=$requested" -o "$BIN" ./cmd/kloud-kode) || fail "go build failed"
    return 0
  fi
  command -v curl >/dev/null 2>&1 || fail "curl is needed to download the release"
  base="${KLOUD_KODE_RELEASE_BASE:-https://github.com/$repo/releases}"
  asset="kloud-kode-$requested-$os-$arch"; if [[ "$os" == windows ]]; then asset="$asset.zip"; else asset="$asset.tar.gz"; fi
  tmp="$(mktemp -d)"
  trap "rm -rf \"$tmp\"" EXIT
  echo "downloading Kloud Kode $requested ($os/$arch)"
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
  cp -f "$tmp/extract/kloud-kode$ext" "$BIN"
  chmod 0755 "$BIN"
  "$BIN" version >/dev/null || fail "the downloaded binary does not run"
}

prepare
cd "$DIR"
exec "$BIN" body "$MODE"
