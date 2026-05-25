#!/bin/sh
set -eu

if ! command -v entire >/dev/null 2>&1; then
  printf '%s\n' '{"systemMessage":"Entire CLI is enabled but not installed or not on PATH. Installation guide: https://docs.entire.io/cli/installation#installation-methods"}'
  exit 0
fi

exec entire hooks codex session-start
