#!/usr/bin/env bash
# Runs ON the brain host via deploy.sh. Arg 1: the komputer-api directory inside the checkout.
# Pulls origin/main, builds the Go binary, restarts the API, and goes back to the
# previous commit if it does not come up.
set -euo pipefail
DIR="$1"
cd "$DIR"
REPO="$(git rev-parse --show-toplevel)"
PROJECT="$(cd .. && pwd)"
PREFIX="$(git -C "$PROJECT" rev-parse --show-prefix)"
MCP_REL="${PREFIX}komputer-session/.mcp.json"
START_REL="${PREFIX}komputer-api/start.sh"
MOD_REL="${PREFIX}go.mod"
if [[ -f ./env ]]; then set -a; source ./env; set +a; fi
PORT="${KOMPUTER_API_PORT:-18888}"
if [[ -n "${KOMPUTER_TLS_CERT_FILE:-}" && -z "${KOMPUTER_DEPLOY_HEALTH_URL:-}" ]]; then
  echo "TLS deployments need KOMPUTER_DEPLOY_HEALTH_URL using a certificate-valid HTTPS hostname" >&2
  exit 1
fi
HEALTH="${KOMPUTER_DEPLOY_HEALTH_URL:-http://127.0.0.1:$PORT/healthz}"
KEY=$(awk -F': ' '/^api-key:/{print $2; exit}' KEYS.txt)
# The session .mcp.json carries this host's real body token; the repo only has a placeholder.
MCP="$PROJECT/komputer-session/.mcp.json"

prev=$(git -C "$REPO" rev-parse HEAD)
git -C "$REPO" fetch -q origin
next=$(git -C "$REPO" rev-parse origin/main)
echo "running $prev"
echo "target  $next"

# Never switch to a revision that removes the running application's layout.
for rev in "$prev" "$next"; do
  if ! git -C "$REPO" cat-file -e "$rev:$START_REL" 2>/dev/null \
    || ! git -C "$REPO" cat-file -e "$rev:$MOD_REL" 2>/dev/null \
    || ! git -C "$REPO" cat-file -e "$rev:$MCP_REL" 2>/dev/null; then
    echo "revision $rev is not compatible with the komputer layout; use the migration guide" >&2
    exit 1
  fi
done

# The launcher downloads the release named by Komputer/VERSION. Check that the
# release actually exists before stopping anything, so a merged VERSION bump
# without its published tag fails the deploy instead of the restart.
version_file="$(cd .. && pwd)/VERSION"
if [[ -f "$version_file" ]]; then
  wanted="$(tr -d '[:space:]' < "$version_file")"
  if ! curl -fsSL --max-time 10 -o /dev/null "https://github.com/${KLAUD_KODE_REPO:-DDD0s/Klaud-Kode}/releases/download/$wanted/SHA256SUMS"; then
    echo "release $wanted has no SHA256SUMS yet; publish the tag before deploying" >&2
    git -C "$REPO" reset -q --hard "$prev"
    exit 1
  fi
  echo "release $wanted is published"
fi

# Do not cut off a turn that is running right now (wait up to 5 minutes).
for _ in $(seq 1 60); do
  busy=$(curl -sf --max-time 3 -H "Authorization: Bearer $KEY" "$HEALTH" \
    | sed -n 's/.*"in_flight":\([0-9]*\).*/\1/p' || true)
  busy="${busy:-0}"
  [[ "$busy" == "0" ]] && break
  echo "waiting: $busy turn(s) in flight"
  sleep 5
done

checkout() {
  local rev="$1" keep
  keep=$(mktemp)
  cp -p "$MCP" "$keep"
  git -C "$REPO" update-index --no-skip-worktree "$MCP_REL"
  git -C "$REPO" reset -q --hard "$rev"
  cp -p "$keep" "$MCP"
  rm -f "$keep"
  git -C "$REPO" update-index --skip-worktree "$MCP_REL"
}

build_binary() {
  ./start.sh --build-only
}

restart() {
  ./stop.sh || true
  ./start.sh </dev/null
  sleep 1.5
}

healthy() {
  curl -sf --max-time 5 -H "Authorization: Bearer $KEY" "$HEALTH" | grep -q '"builtin_tools"'
}

echo "$prev" > "$PROJECT/.deploy-prev"
checkout "$next"

if build_binary && restart && healthy; then
  echo "deploy OK"
  curl -s -H "Authorization: Bearer $KEY" "$HEALTH"
  echo
else
  echo "deploy FAILED, going back to $prev" >&2
  tail -n 30 logs/komputer-api.log >&2 || true
  checkout "$prev"
  build_binary || true
  restart || true
  exit 1
fi
