#!/usr/bin/env python3
# Aggregate the multi-repo embedder benchmark: per-repo and pooled Model2Vec vs
# EmbeddingGemma useful/1k, with the concept-stratum delta and win/loss counts.
#   scripts/bench/agg.py [results-dir]
import json, glob, os, sys

OUT = sys.argv[1] if len(sys.argv) > 1 else os.path.join(os.path.dirname(__file__), "results")

def load(p):
    with open(p) as f:
        return json.load(f)

repos = sorted({os.path.basename(p).rsplit(".m2v.json", 1)[0]
                for p in glob.glob(os.path.join(OUT, "*.m2v.json"))})
if not repos:
    sys.exit(f"no *.m2v.json results in {OUT}")

pool = {"m2v": [], "gemma": []}
print(f"{'repo':<16} {'tasks':>5} {'M2V':>7} {'Gemma':>7} {'delta':>8} {'concept Δ':>10}")
print("-" * 60)
for r in repos:
    m = load(os.path.join(OUT, f"{r}.m2v.json"))
    g = load(os.path.join(OUT, f"{r}.gemma.json"))
    mb, gb = m["mean_useful_per_1k"], g["mean_useful_per_1k"]
    cm = m.get("by_stratum", {}).get("concept", {}).get("mean_useful_per_1k")
    cg = g.get("by_stratum", {}).get("concept", {}).get("mean_useful_per_1k")
    cd = f"{(cg - cm):+.2f}" if cm is not None and cg is not None else "  n/a"
    dpct = f"{((gb-mb)/mb*100):+.0f}%" if mb else "  n/a"
    print(f"{r:<16} {m['tasks']:>5} {mb:>7.3f} {gb:>7.3f} {dpct:>8} {cd:>10}")
    mres = {t["id"]: t["useful_per_1k"] for t in m["results"]}
    gres = {t["id"]: t["useful_per_1k"] for t in g["results"]}
    for tid in mres:
        pool["m2v"].append(mres[tid])
        pool["gemma"].append(gres.get(tid, 0.0))

n = len(pool["m2v"])
mb = sum(pool["m2v"]) / n
gb = sum(pool["gemma"]) / n
wins = sum(1 for a, b in zip(pool["m2v"], pool["gemma"]) if b - a > 0.01)
losses = sum(1 for a, b in zip(pool["m2v"], pool["gemma"]) if b - a < -0.01)
ties = n - wins - losses
rescues = sum(1 for a, b in zip(pool["m2v"], pool["gemma"]) if a < 0.01 and b > 0.5)
print("-" * 60)
print(f"{'POOLED':<16} {n:>5} {mb:>7.3f} {gb:>7.3f} {((gb-mb)/mb*100):>+7.0f}%")
print(f"  {n} tasks | wins {wins} losses {losses} ties {ties} | zero-rescues {rescues}")
