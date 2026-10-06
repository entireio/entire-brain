#!/bin/sh
# Run every condition sequentially for N repetitions (arms are not run in parallel so they do not
# contend for the hosted search service, the Brain store or the Graph cache).
#   run-all.sh [reps=3] [question=global-activation] [conditions="1 2 3 4 5"]
set -eu
here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
REPS=${1:-3}; Q=${2:-global-activation}; CONDS=${3:-"1 2 3 4 5"}
sh "$here/setup-guides.sh"
r=1
while [ "$r" -le "$REPS" ]; do
  for c in $CONDS; do sh "$here/run.sh" "$c" "$r" "$Q"; done
  r=$((r + 1))
done
python3 "$here/grade.py" "$Q"
