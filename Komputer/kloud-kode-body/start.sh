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

ensure_binary() {
  if [[ -n "${KLOUD_KODE_BIN:-}" ]]; then BIN="$KLOUD_KODE_BIN"; [[ -x "$BIN" ]] || { echo "ERROR: KLOUD_KODE_BIN is not executable: $BIN" >&2; exit 1; }; return 0; fi
  local requested="${KLOUD_KODE_VERSION:-}" os arch bin_name asset tmp repo base marker
  if [[ -z "$requested" && -f "$MODULE/VERSION" ]]; then requested="$(tr -d '[:space:]' < "$MODULE/VERSION")"; fi
  [[ -n "$requested" ]] || requested="latest"
  case "$(uname -s)" in Linux*) os=linux ;; Darwin*) os=darwin ;; MINGW*|MSYS*|CYGWIN*) os=windows ;; *) echo "unsupported operating system: $(uname -s)" >&2; exit 1 ;; esac
  case "$(uname -m)" in x86_64|amd64) arch=amd64 ;; arm64|aarch64) arch=arm64 ;; *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;; esac
  bin_name="kloud-kode"; if [[ "$os" == windows ]]; then bin_name="kloud-kode.exe"; fi
  BIN="$DIR/$bin_name"; marker="$DIR/.kloud-kode-version"; repo="${KLOUD_KODE_REPO:-DDD0s/Kloud-Kode}"
  if [[ "$requested" == latest ]]; then requested="$(curl -fsSL "https://api.github.com/repos/$repo/releases/latest" | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)"; fi
  [[ -n "$requested" ]] || { echo "ERROR: could not resolve the Kloud Kode release" >&2; exit 1; }
  if [[ -x "$BIN" && -f "$marker" ]] && [[ "$(tr -d '[:space:]' < "$marker")" == "$requested" ]]; then return 0; fi
  if [[ "${KLOUD_KODE_BUILD:-}" == 1 ]] && command -v go >/dev/null 2>&1; then (cd "$MODULE" && go build -trimpath -o "$BIN" ./cmd/kloud-kode); printf "%s\n" "$requested" > "$marker"; return 0; fi
  base="${KLOUD_KODE_RELEASE_BASE:-https://github.com/$repo/releases}"; asset="kloud-kode-$requested-$os-$arch"; if [[ "$os" == windows ]]; then asset="$asset.zip"; else asset="$asset.tar.gz"; fi
  tmp="$(mktemp -d)"; echo "downloading Kloud Kode $requested ($os/$arch)"
  curl -fL --retry 3 -o "$tmp/$asset" "$base/download/$requested/$asset"
  curl -fL --retry 3 -o "$tmp/SHA256SUMS" "$base/download/$requested/SHA256SUMS"
  (cd "$tmp" && grep " $asset$" SHA256SUMS > asset.sha256 && sha256sum -c asset.sha256)
  mkdir -p "$tmp/extract"; if [[ "$os" == windows ]]; then unzip -o -q "$tmp/$asset" -d "$tmp/extract"; else tar -xzf "$tmp/$asset" -C "$tmp/extract"; fi
  install -m 0755 "$tmp/extract/$bin_name" "$BIN"; rm -rf "$tmp"; printf "%s\n" "$requested" > "$marker"
}
ensure_binary
cd "$DIR"
exec "$BIN" body "$MODE"
