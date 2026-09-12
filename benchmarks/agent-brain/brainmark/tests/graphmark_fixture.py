"""Local GraphMark protocol double; no external checkout or network needed."""
from pathlib import Path

def install(root: Path) -> None:
    tools = root / "tools"
    (tools / "netjail").mkdir(parents=True, exist_ok=True)
    (tools / "collect_patch.sh").write_text('''#!/bin/sh
set -eu
git -C "$1" add -N .
git -C "$1" diff --binary HEAD > "$2"
''')
