#!/bin/sh
# Single end-to-end installer for the Entire code-intelligence system.
#
# Builds and installs both plugins (the entire-sem semantic provider and the
# entire-brain store/MCP/hook layer), writes the default plugin configuration
# that registers the MCP server, agent hooks, and agent instructions, then
# verifies the environment. This replaces running each repo's install-local.sh
# by hand and wiring MCP/hooks separately.
#
# Usage:
#   scripts/install.sh
#   ENTIRE_SEM_DIR=/path/to/entire-sem scripts/install.sh   # custom sem location
set -eu

brain_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
sem_root=${ENTIRE_SEM_DIR:-"$brain_root/../entire-sem"}

if ! command -v entire >/dev/null 2>&1; then
	printf 'error: the parent `entire` CLI is required; install it first\n' >&2
	exit 1
fi

printf '==> Installing entire-sem semantic provider (%s)\n' "$sem_root"
if [ -x "$sem_root/scripts/install-local.sh" ]; then
	(cd "$sem_root" && sh scripts/install-local.sh)
else
	printf 'error: entire-sem not found at %s; set ENTIRE_SEM_DIR to its checkout\n' "$sem_root" >&2
	exit 1
fi

printf '==> Installing entire-brain (store, MCP tools, agent hooks)\n'
(cd "$brain_root" && sh scripts/install-local.sh)

printf '==> Writing default plugin configuration (MCP + hooks + agent instructions)\n'
if ! entire brain config init; then
	printf 'note: `entire brain config init` returned nonzero (config may already exist); continuing\n' >&2
fi

printf '==> Verifying the plugin environment\n'
entire brain doctor

cat <<'DONE'

Entire is installed end to end:
  - entire-sem provider and entire-brain plugin built and registered
  - default plugin config written (MCP server, hooks, agent instructions)

Next:
  - index a repo:   entire brain index --repo .
  - MCP (stdio):    entire brain mcp
  - re-run checks:  entire brain doctor
DONE
