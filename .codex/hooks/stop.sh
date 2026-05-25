#!/bin/sh
set -eu

# Keep non-start hooks silent when Entire is unavailable; SessionStart
# already surfaces the actionable installation message to the user.
if ! command -v entire >/dev/null 2>&1; then
  exit 0
fi

exec entire hooks codex stop
