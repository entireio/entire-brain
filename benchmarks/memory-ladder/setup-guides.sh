#!/bin/sh
# Generate the per-condition CLAUDE.md guides from the product's own output so
# checked-in copies cannot go stale. Writes guides/cond1.md .. cond5.md.
#   cond1  plain repository note, no Entire tooling
#   cond2  plain note + session-history section
#   cond3  `entire graph agent-guide` outside a repository (standalone Graph) + sessions
#   cond4  `entire graph agent-guide` inside this repository (combined, normal) + sessions
#   cond5  this repository's .entire/agent-guide.md as committed (strict if init-agents --strict was run) + sessions
set -eu
here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
repo=$(git -C "$here" rev-parse --show-toplevel)
out=$here/guides; mkdir -p "$out"
sessions='

## Session history

Previous agent sessions, checkpoints and commits for this repository are
searchable. Use them when a question concerns previous work, decisions, or
history. Always pass --json to search; without it the command opens a TUI.

    entire search "<query>" --json              # checkpoints, commits and sessions
    entire search "<query>" --json --compact    # trimmed output for agents
    entire checkpoint list                      # checkpoints on this branch
    entire checkpoint explain <id|sha>          # explain a checkpoint or commit
    entire checkpoint explain <id|sha> --full   # include the session transcript

Run `entire agent-help <command>` for exact flags.
'
plain='# Repository guide

This is the entire-brain Go repository. Use the source tree and git history
(git log, git blame, git show) to answer questions.'
printf '%s No Entire tooling (session\nhistory, code graph, or brain) is available in this environment.\n' "$plain" > "$out/cond1.md"
printf '%s%s' "$plain" "$sessions" > "$out/cond2.md"
{ (cd / && entire graph agent-guide); printf '%s' "$sessions"; } > "$out/cond3.md"
{ (cd "$repo" && entire graph agent-guide); printf '%s' "$sessions"; } > "$out/cond4.md"
if [ -f "$repo/.entire/agent-guide.md" ]; then
  { cat "$repo/.entire/agent-guide.md"; printf '%s' "$sessions"; } > "$out/cond5.md"
else
  echo "warning: $repo/.entire/agent-guide.md missing; cond5 falls back to cond4" >&2; cp "$out/cond4.md" "$out/cond5.md"
fi
for c in 3 4; do grep -q "^# Entire repository agent guide" "$out/cond$c.md" || { echo "cond$c guide did not render; is entire graph installed?" >&2; exit 1; }; done
wc -l "$out"/cond*.md
