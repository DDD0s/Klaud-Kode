#!/usr/bin/env bash
# Runs ON the brain host via `deploy.sh rollback`. Arg 1: the komputer-api directory.
# Returns the checkout to the commit that ran before the most recent deploy.
set -euo pipefail
cd "$1"
REPO="$(git rev-parse --show-toplevel)"
PROJECT="$(cd .. && pwd)"
PREFIX="$(git -C "$PROJECT" rev-parse --show-prefix)"
MCP_REL="${PREFIX}komputer-session/.mcp.json"
START_REL="${PREFIX}komputer-api/start.sh"
MCP="$PROJECT/komputer-session/.mcp.json"
prev=$(cat "$PROJECT/.deploy-prev" 2>/dev/null || true)
if [[ -z "$prev" ]]; then
  echo "no .deploy-prev recorded; nothing to roll back to" >&2
  exit 1
fi
if ! git -C "$REPO" cat-file -e "$prev:$START_REL" 2>/dev/null \
  || ! git -C "$REPO" cat-file -e "$prev:$MCP_REL" 2>/dev/null; then
  echo "revision $prev is not compatible with the komputer layout; use the migration guide" >&2
  exit 1
fi
keep=$(mktemp)
cp -p "$MCP" "$keep"
git -C "$REPO" update-index --no-skip-worktree "$MCP_REL"
git -C "$REPO" reset -q --hard "$prev"
cp -p "$keep" "$MCP"
rm -f "$keep"
git -C "$REPO" update-index --skip-worktree "$MCP_REL"
./start.sh --build-only
./stop.sh || true
./start.sh </dev/null
echo "rolled back to $(git -C "$REPO" rev-parse HEAD)"
