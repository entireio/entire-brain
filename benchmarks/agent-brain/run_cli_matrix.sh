#!/bin/bash
# Idempotent driver for the entireio/cli brain-vs-no-brain matrix.
# Usage: run_cli_matrix.sh <codex|claude>
# Runs one suite per (runner,task): conditions no_brain/full_cli_compact/mcp_history, n=3.
# A (runner,task) suite is SKIPPED if it already has >= EXPECTED records with returncode 0
# (i.e. ran without infra failure). Incomplete suites are rebuilt from scratch (no dup inflation).
set -u
cd "$(dirname "$0")"

AGENT="${1:?usage: run_cli_matrix.sh <codex|claude>}"
CONDS="no_brain,full_cli_compact,mcp_history"
REPS=3
CKPT=200
CBUDGET=25
EXPECTED=9   # 3 conditions * 3 reps per (runner,task)

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

for runner in "${RUNNERS[@]}"; do
  for task in "${TASKS[@]}"; do
    sn="cliproof-$(shorttask "$task")-$(shortrunner "$runner")"
    suite="results/$sn"
    clean=0
    if [ -f "$suite/records.ndjson" ]; then
      clean=$(jq -s 'map(select(.agent_info.returncode==0)) | length' "$suite/records.ndjson" 2>/dev/null || echo 0)
    fi
    if [ "${clean:-0}" -ge "$EXPECTED" ]; then
      echo "[$(date '+%H:%M:%S')] SKIP $sn (clean=$clean)"
      continue
    fi
    echo "[$(date '+%H:%M:%S')] RUN  $sn (had clean=$clean) runner=$runner task=$task"
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
      >> "/tmp/cliproof-${AGENT}.log" 2>&1
  done
done
echo "[$(date '+%H:%M:%S')] MATRIX PASS COMPLETE for $AGENT"
