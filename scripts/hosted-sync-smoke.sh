#!/usr/bin/env bash
# Hosted sync two-machine smoke (COR-2032): a fact remembered on checkout A
# must appear in `recall` on checkout B with no manual commands beyond
# `entire brain connect`.
#
# Each "machine" is a fresh clone with its own plugin data/state/config/cache
# dirs, sharing only the hosted brainstore head. Auth is the host CLI's own:
# the plugin shells `entire auth token`, which prints ENTIRE_TOKEN verbatim
# when set, else the active context's login. To target a non-active context
# (e.g. staging), set AUTH_CONTEXT and the script exports ENTIRE_TOKEN from it.
#
# Usage:
#   REPO_ID=01M2Q9WATHXVM46CD8Y4D3AX8W \
#   API_URL=https://aws-us-east-2.api.partial.to/api/v1 \
#   AUTH_CONTEXT=us.auth.partial.to \
#   scripts/hosted-sync-smoke.sh
set -euo pipefail

REPO_ID=${REPO_ID:?set REPO_ID (region-local repo ULID)}
API_URL=${API_URL:?set API_URL (cell base URL, e.g. https://aws-us-east-2.api.partial.to/api/v1)}
if [ -n "${AUTH_CONTEXT:-}" ]; then
  ENTIRE_TOKEN=$(entire auth token --context "$AUTH_CONTEXT")
  export ENTIRE_TOKEN
fi

root=$(git rev-parse --show-toplevel)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

echo "==> building entire-brain"
bin="$work/entire-brain"
(cd "$root" && go build -o "$bin" ./cmd/entire-brain)

run() { # run <machine> <checkout> <args...>
  local m=$1 dir=$2
  shift 2
  ENTIRE_PLUGIN_DATA_DIR="$work/$m/data" \
    ENTIRE_PLUGIN_STATE_DIR="$work/$m/state" \
    ENTIRE_PLUGIN_CONFIG_DIR="$work/$m/config" \
    ENTIRE_PLUGIN_CACHE_DIR="$work/$m/cache" \
    ENTIRE_REPO_ROOT="$dir" \
    "$bin" "$@"
}

echo "==> cloning checkouts A and B"
git clone -q "$root" "$work/a"
git clone -q "$root" "$work/b"

fact="hosted sync smoke $(date +%s)-$RANDOM: the two-machine loop works"

connect_args=(--repo-id "$REPO_ID" --api-url "$API_URL")

echo "==> machine A: connect"
run a "$work/a" connect "${connect_args[@]}"
echo "==> machine A: remember \"$fact\""
run a "$work/a" remember "$fact" --path project.testing.smoke --agent none
echo "==> machine A: publish (connect re-runs the sync)"
run a "$work/a" connect "${connect_args[@]}"

echo "==> machine B: connect (fresh clone; connect doubles as the first pull)"
run b "$work/b" connect "${connect_args[@]}"

echo "==> machine B: recall"
if run b "$work/b" recall "hosted sync smoke two-machine loop" --json | grep -qF "$fact"; then
  echo "PASS: fact remembered on A appears in recall on B"
else
  echo "FAIL: fact not found on machine B" >&2
  exit 1
fi
