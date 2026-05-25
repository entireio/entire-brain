#!/bin/sh
set -eu

if ! command -v entire >/dev/null 2>&1; then
  exit 0
fi

exec entire hooks codex post-tool-use
