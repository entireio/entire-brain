#!/bin/sh
# Run one condition, one repetition, against a fresh clone of this repository.
#   run.sh <cond 1-5> <rep> [question-name]        (default question: global-activation)
# Environment:
#   LADDER_OUT=<dir>       where runs land (default: benchmarks/memory-ladder/runs, gitignored)
#   LADDER_MODEL=<model>   model for the nested agent (default: sonnet)
#   LADDER_ORIGIN=<url>    origin URL to set on the clone (default: this repo's origin). Brain and Graph
#                          derive the repository key from it, so a filesystem path origin breaks them.
#   LADDER_MAX_TURNS=<n>   nested agent turn cap (default: 80)
#   LADDER_BRANCH=<name>   branch to clone (default: main). Brain scopes facts to the checked-out branch,
#                          so this must be the branch the brain was built for, not the feature branch
#                          you happen to be developing the harness on.
# The nested agent runs read-only: explicit tool allowlist, no permission bypass, no session persistence.
# Graph prewarm (conditions 3+) is excluded from timing and written to prewarm.txt.
# The clone's HEAD is one commit past the source (the harness directory is stripped), so Brain
# reports its index as one commit behind; its content is unchanged by that commit.
set -eu
COND=$1; REP=$2; Q=${3:-global-activation}
here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
SRC=$(git -C "$here" rev-parse --show-toplevel)
OUT=${LADDER_OUT:-$here/runs}
ORIGIN=${LADDER_ORIGIN:-$(git -C "$SRC" remote get-url origin)}
BRANCH=${LADDER_BRANCH:-main}
RUN=$OUT/$Q/c${COND}-r${REP}
[ -f "$here/guides/cond${COND}.md" ] || { echo "guides missing; run setup-guides.sh first" >&2; exit 1; }
[ -f "$here/questions/$Q.txt" ] || { echo "unknown question $Q" >&2; exit 1; }
rm -rf "$RUN"; mkdir -p "$RUN"
git clone -q --branch "$BRANCH" "$SRC" "$RUN/repo"
cd "$RUN/repo"
git remote set-url origin "$ORIGIN"
# The agent under test must not be able to read its own grading rubric, the prompt, or the README
# that narrates earlier results. Remove the harness from the worktree and from HEAD's tree, so
# neither the filesystem nor `git show HEAD:...` can reach it. Older history is still reachable
# through git; the strip commit is recorded in summary.json so a reader can check for leakage.
if [ -d benchmarks/memory-ladder ]; then
  git rm -r -q --cached benchmarks/memory-ladder && rm -rf benchmarks/memory-ladder
  git -c user.name=ladder -c user.email=ladder@invalid commit -q -m "memory-ladder: strip harness from the clone under test"
fi
git rev-parse HEAD > "$RUN/head.txt"
cp "$here/guides/cond${COND}.md" CLAUDE.md
rm -f .entire/agent-guide.md
[ "$COND" = 1 ] && rm -f .claude/agents/entire-search.md
export PATH="$here/bin:$PATH" LADDER_COND=$COND
if [ "$COND" -ge 3 ]; then
  s=$(date +%s); entire graph index --repo . --profile full >"$RUN/prewarm.log" 2>&1 || true
  echo "prewarm_s=$(( $(date +%s) - s ))" > "$RUN/prewarm.txt"
fi
ALLOW="Read Grep Glob Agent Bash(git:*) Bash(entire:*) Bash(grep:*) Bash(rg:*) Bash(ls:*) Bash(cat:*) Bash(head:*) Bash(tail:*) Bash(sed -n:*) Bash(find:*) Bash(wc:*) Bash(go doc:*)"
start=$(date +%s)
claude -p "$(cat "$here/questions/$Q.txt")" --model "${LADDER_MODEL:-sonnet}" --output-format stream-json --verbose \
  --setting-sources project --no-session-persistence --max-turns "${LADDER_MAX_TURNS:-80}" \
  --allowedTools $ALLOW --disallowedTools "Edit Write MultiEdit NotebookEdit WebFetch WebSearch" \
  > "$RUN/stream.jsonl" 2> "$RUN/stderr.log" || echo "exit=$?" > "$RUN/failed.txt"
echo "wall_s=$(( $(date +%s) - start ))" > "$RUN/wall.txt"
python3 "$here/parse.py" "$RUN" > "$RUN/summary.json"
echo "done $Q c${COND}-r${REP}"
