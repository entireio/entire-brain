#!/bin/bash
# Parallel, idempotent driver for the entireio/cli brain-vs-no-brain matrix.
# Usage: run_cli_parallel.sh <codex|claude> <maxjobs>
# Runs up to <maxjobs> (runner,task) suites concurrently. Each suite = 3 conditions x 3 reps = 9 runs.
# Concurrency-safe: each run.py builds its own <suite>/bin (Go cache is locked), and worktrees are
# independent `git archive`+`git init` copies (no shared git-worktree locks). Brain caches are warm,
# so runs hit cache (no build races). Idempotent: complete suites (>=9 clean records) are skipped;
# incomplete suites are rebuilt from scratch (no duplicate inflation).
set -u
cd "$(dirname "$0")"

AGENT="${1:?usage: run_cli_parallel.sh <codex|claude> <maxjobs>}"
MAXJOBS="${2:?maxjobs}"
CONDS="no_brain,full_cli_compact,mcp_history"
REPS=3
CKPT=200
CBUDGET=25
EXPECTED=9

CODEX_RUNNERS=(
  codex:gpt-5.4-mini:low codex:gpt-5.4-mini:medium codex:gpt-5.4-mini:high codex:gpt-5.4-mini:xhigh
  codex:gpt-5.5:low codex:gpt-5.5:medium codex:gpt-5.5:high codex:gpt-5.5:xhigh
)
CLAUDE_RUNNERS=(
  claude:haiku:low claude:haiku:medium claude:haiku:high claude:haiku:xhigh
  claude:sonnet:low claude:sonnet:medium claude:sonnet:high claude:sonnet:xhigh
  claude:opus:low claude:opus:medium claude:opus:high claude:opus:xhigh
)
TASKS=(entireio-cli-transcript-reresolve entireio-cli-review-base-flag-scope)

case "$AGENT" in
  codex)  RUNNERS=("${CODEX_RUNNERS[@]}") ;;
  claude) RUNNERS=("${CLAUDE_RUNNERS[@]}") ;;
  *) echo "unknown agent: $AGENT"; exit 2 ;;
esac

shortrunner() { echo "$1" | sed -e 's/codex:gpt-5.4-mini:/codex-mini-/; s/codex:gpt-5.5:/codex-55-/; s/claude:/claude-/; s/:/-/g'; }
shorttask()   { case "$1" in *transcript*) echo tr ;; *review*) echo rv ;; esac; }

run_one() {
  local runner="$1" task="$2"
  local sn="cliproof-$(shorttask "$task")-$(shortrunner "$runner")"
  local suite="results/$sn"
  local clean=0
  if [ -f "$suite/records.ndjson" ]; then
    clean=$(jq -s 'map(select(.agent_info.returncode==0))|length' "$suite/records.ndjson" 2>/dev/null || echo 0)
  fi
  if [ "${clean:-0}" -ge "$EXPECTED" ]; then
    echo "[$(date '+%H:%M:%S')] SKIP $sn (clean=$clean)"
    return 0
  fi
  echo "[$(date '+%H:%M:%S')] START $sn (had clean=$clean)"
  rm -rf "$suite"
  python3 run.py run \
    --tasks "${task}*" \
    --runners "$runner" \
    --conditions "$CONDS" \
    --repetitions "$REPS" \
    --checkpoint-limit "$CKPT" \
    --claude-budget "$CBUDGET" \
    --pricing-file pricing.json \
    --suite-name "$sn" \
    >> "/tmp/cliproof-job-${sn}.log" 2>&1
  echo "[$(date '+%H:%M:%S')] END   $sn rc=$?"
}

running() { jobs -r -p | wc -l | tr -d ' '; }

for runner in "${RUNNERS[@]}"; do
  for task in "${TASKS[@]}"; do
    while [ "$(running)" -ge "$MAXJOBS" ]; do sleep 5; done
    run_one "$runner" "$task" &
  done
done
wait
echo "[$(date '+%H:%M:%S')] PARALLEL PASS COMPLETE for $AGENT (jobs=$MAXJOBS)"
