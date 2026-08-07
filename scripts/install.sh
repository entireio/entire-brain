#!/bin/sh
# Single end-to-end installer for the Entire code-intelligence system.
#
# Builds and installs both plugins (the entire-graph semantic provider and the
# entire-brain store/MCP layer), writes the default plugin configuration, then
# verifies the environment. This replaces running each repo's install-local.sh
# by hand.
#
# Usage:
#   scripts/install.sh
#   ENTIRE_GRAPH_DIR=/path/to/entire-graph scripts/install.sh   # custom graph location
set -eu

brain_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
graph_root=${ENTIRE_GRAPH_DIR:-"$brain_root/../entire-graph"}

if ! command -v entire >/dev/null 2>&1; then
	printf 'error: the parent `entire` CLI is required; install it first\n' >&2
	exit 1
fi

printf '==> Installing entire-graph semantic provider (%s)\n' "$graph_root"
if [ -x "$graph_root/scripts/install-local.sh" ]; then
	(cd "$graph_root" && sh scripts/install-local.sh)
else
	printf 'error: entire-graph not found at %s; set ENTIRE_GRAPH_DIR to its checkout\n' "$graph_root" >&2
	exit 1
fi

printf '==> Installing entire-brain (store and MCP tools)\n'
(cd "$brain_root" && sh scripts/install-local.sh)

printf '==> Writing default plugin configuration\n'
if ! entire brain config init; then
	printf 'note: `entire brain config init` returned nonzero (config may already exist); continuing\n' >&2
fi

printf '==> Verifying the plugin environment\n'
entire brain doctor

cat <<'DONE'

Entire is installed end to end:
  - entire-graph provider and entire-brain plugin built and registered
  - default entire-brain plugin config written

Next:
  - build a brain:  entire brain refresh --agent none
  - MCP (stdio):    entire brain mcp
  - re-run checks:  entire brain doctor
DONE
