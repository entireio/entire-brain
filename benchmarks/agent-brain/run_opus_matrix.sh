#!/bin/bash
# Opus-4.8 compact-mode validation matrix: both cli tasks x all 4 efforts x
# {no_brain, full_cli_compact(CLI), mcp_history(MCP)} x n=3. Opus only.
# Idempotent (skip suites already at >=9 clean records); parallel (maxjobs).
set -u
cd "$(dirname "$0")"
MAXJOBS="${1:-5}"
CONDS="no_brain,full_cli_compact,mcp_history"; REPS=3; CKPT=200; CBUDGET=25; EXPECTED=9
EFFORTS=(low medium high xhigh)
TASKS=(entireio-cli-transcript-reresolve entireio-cli-review-base-flag-scope)
shorttask(){ case "$1" in *transcript*) echo tr;; *review*) echo rv;; esac; }
run_one(){
  local eff="$1" task="$2"
  local sn="opusfix-$(shorttask "$task")-opus-$eff"
  local suite="results/$sn"; local clean=0
  [ -f "$suite/records.ndjson" ] && clean=$(jq -s 'map(select(.agent_info.returncode==0))|length' "$suite/records.ndjson" 2>/dev/null||echo 0)
  if [ "${clean:-0}" -ge "$EXPECTED" ]; then echo "[$(date '+%H:%M:%S')] SKIP $sn (clean=$clean)"; return 0; fi
  echo "[$(date '+%H:%M:%S')] START $sn"; rm -rf "$suite"
  python3 run.py run --tasks "${task}*" --runners "claude:opus:$eff" --conditions "$CONDS" \
    --repetitions "$REPS" --checkpoint-limit "$CKPT" --claude-budget "$CBUDGET" \
    --pricing-file pricing.json --suite-name "$sn" >> "/tmp/opusfix-$sn.log" 2>&1
  echo "[$(date '+%H:%M:%S')] END   $sn rc=$?"
}
running(){ jobs -r -p | wc -l | tr -d ' '; }
for eff in "${EFFORTS[@]}"; do for task in "${TASKS[@]}"; do
  while [ "$(running)" -ge "$MAXJOBS" ]; do sleep 5; done
  run_one "$eff" "$task" &
done; done
wait
echo "[$(date '+%H:%M:%S')] OPUS MATRIX COMPLETE"
