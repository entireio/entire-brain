#!/bin/bash
# Snapshot of the cliproof matrix progress.
cd "$(dirname "$0")"
echo "=== drivers running ==="
pgrep -fl "run_cli_matrix.sh" || echo "  (no drivers running)"
pgrep -fl "run.py prep" >/dev/null 2>&1 && echo "  (review prep still running)"
echo
echo "=== per-suite progress (clean/total records; valid count) ==="
total_clean=0
for d in results/cliproof-tr-* results/cliproof-rv-*; do
  [ -d "$d" ] || continue
  [ -f "$d/records.ndjson" ] || { echo "  $(basename "$d"): (no records yet)"; continue; }
  n=$(jq -s 'length' "$d/records.ndjson" 2>/dev/null || echo 0)
  clean=$(jq -s 'map(select(.agent_info.returncode==0))|length' "$d/records.ndjson" 2>/dev/null || echo 0)
  valid=$(jq -s 'map(select(.validation.ok==true))|length' "$d/records.ndjson" 2>/dev/null || echo 0)
  total_clean=$((total_clean + clean))
  flag=""; [ "$clean" -ge 9 ] && flag="DONE"
  printf "  %-34s %2s rec  %2s clean  %2s valid  %s\n" "$(basename "$d")" "$n" "$clean" "$valid" "$flag"
done
echo
echo "  TOTAL clean records: $total_clean / 360"
echo
echo "=== agent infra failures (rate-limit / 429 / nonzero agent rc) ==="
cat results/cliproof-*/records.ndjson 2>/dev/null | jq -s '
  map(select((.agent_info.returncode != 0 and .agent_info.returncode != null)
             or ((.agent_info.stderr_tail // "") | test("session limit|api_error_status.:429"))))
  | length' 2>/dev/null | xargs echo "  agent runs that hit infra/rate-limit failure:"
