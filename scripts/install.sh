#!/bin/sh
# Single end-to-end installer for the Entire code-intelligence system.
#
# Builds and installs both plugins (the entire-sem semantic provider and the
# entire-brain store/MCP layer), writes the default plugin configuration, then
# verifies the environment. This replaces running each repo's install-local.sh
# by hand.
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
  - entire-sem provider and entire-brain plugin built and registered
  - default entire-brain plugin config written

Next:
  - build a brain:  entire brain refresh --agent none
  - MCP (stdio):    entire brain mcp
  - re-run checks:  entire brain doctor
DONE
