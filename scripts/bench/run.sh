#!/usr/bin/env bash
# Multi-repo embedder benchmark. For each repo: build facts, generate a
# provenance-labeled task set, and eval fact retrieval with Model2Vec (the
# bundled default) vs EmbeddingGemma (the Stage 1b transformer) — same lexical
# arm, same tasks. Emits per-repo JSON under $OUT for agg.py to pool.
#
#   scripts/bench/run.sh <repo-path> [<repo-path> ...]
#   scripts/bench/agg.py            # print the per-repo + pooled table
#
# Requires the EmbeddingGemma embed server on $EMBED_URL (see embed-server.mjs).
# Env: EB (entire-brain binary), OUT (results dir), EMBED_URL, MODEL (distill).
set -uo pipefail
SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
REPO_ROOT=$(cd "$SCRIPT_DIR/../.." && pwd)
EB="${EB:-$REPO_ROOT/entire-brain}"
OUT="${OUT:-$SCRIPT_DIR/results}"
EMBED_URL="${EMBED_URL:-http://localhost:11500}"
MODEL="${MODEL:-gpt-5.4-mini}"
mkdir -p "$OUT"

[ -x "$EB" ] || { echo "build the binary first: (cd $REPO_ROOT && go build -o entire-brain ./cmd/entire-brain)"; exit 1; }

if [ "$#" -eq 0 ]; then
  echo "usage: scripts/bench/run.sh <repo-path> [<repo-path> ...]" >&2
  echo "  (no repos given — nothing to benchmark)" >&2
  exit 2
fi

for repo in "$@"; do
  name=$(basename "$repo")
  echo "### $name ($repo)"

  echo "  [1/5] refresh (deterministic)"
  (cd "$repo" && "$EB" refresh --agent none) >"$OUT/$name.refresh.log" 2>&1

  echo "  [2/5] distill ($MODEL, cheap)"
  (cd "$repo" && "$EB" distill --branch main --model "$MODEL" --effort low) >"$OUT/$name.distill.log" 2>&1
  if ! (cd "$repo" && "$EB" status 2>/dev/null | grep -q "facts=true"); then
    echo "  SKIP: no facts after distill (see $OUT/$name.distill.log)"; continue
  fi

  echo "  [3/5] eval-gen"
  (cd "$repo" && "$EB" facts eval-gen --branch main --out "$OUT/$name.tasks.json") >"$OUT/$name.evalgen.log" 2>&1
  n=$(python3 -c "import json;print(len(json.load(open('$OUT/$name.tasks.json'))))" 2>/dev/null || echo 0)
  if [ "${n:-0}" -lt 3 ]; then echo "  SKIP: only ${n:-0} tasks"; continue; fi

  echo "  [4/5] eval Model2Vec"
  if ! (cd "$repo" && "$EB" facts eval --tasks "$OUT/$name.tasks.json" --branch main --k 10 --semantic --json) >"$OUT/$name.m2v.json" 2>"$OUT/$name.m2v.log"; then
    echo "  SKIP: Model2Vec eval failed (see $OUT/$name.m2v.log)"; continue
  fi

  echo "  [5/5] eval EmbeddingGemma"
  if ! (cd "$repo" && ENTIRE_BRAIN_EMBEDDER=ollama ENTIRE_BRAIN_EMBED_URL="$EMBED_URL" \
    "$EB" facts eval --tasks "$OUT/$name.tasks.json" --branch main --k 10 --semantic --json) >"$OUT/$name.gemma.json" 2>"$OUT/$name.gemma.log"; then
    echo "  SKIP: EmbeddingGemma eval failed (see $OUT/$name.gemma.log)"; continue
  fi

  echo "  done: $n tasks -> $OUT/$name.{m2v,gemma}.json"
done
echo "ALL DONE"
