"""Summarise runs for one question: per-run metrics, rubric hits, and per-condition means.

    python3 grade.py [question=global-activation] [--json]

Rubric regexes come from questions/<question>.rubric.json; 'recovered' is a first pass
and should be hand-checked against the answers in runs/<question>/*/summary.json.
"""
import glob, json, os, re, statistics, sys

here = os.path.dirname(os.path.abspath(__file__))
q = next((a for a in sys.argv[1:] if not a.startswith("--")), "global-activation")
out_dir = os.environ.get("LADDER_OUT", os.path.join(here, "runs"))
rubric = {k: v for k, v in json.load(open(os.path.join(here, "questions", f"{q}.rubric.json"))).items() if not k.startswith("_")}
rows = []
for sj in sorted(glob.glob(os.path.join(out_dir, q, "c*-r*", "summary.json"))):
    d = json.load(open(sj))
    text = d.get("result") or ""
    hits = {k: bool(re.search(p, text, re.I | re.S)) for k, p in rubric.items()}
    rows.append({"run": d["cond"], **{k: d.get(k) for k in ("duration_s", "cost_usd", "num_turns", "tool_calls", "tokens_in", "tokens_out", "is_error", "entire_subcommands")}, **hits})
if "--json" in sys.argv:
    print(json.dumps(rows, indent=1)); sys.exit()
cols = ["duration_s", "num_turns", "tool_calls", "tokens_in"] + list(rubric)
print("run".ljust(8) + "".join(c[:11].ljust(12) for c in cols))
for r in rows:
    print(r["run"].ljust(8) + "".join(str(r.get(c))[:11].ljust(12) for c in cols))
by = {}
for r in rows:
    by.setdefault(r["run"].split("-")[0], []).append(r)
print("\ncond  n  mean_dur_s  mean_turns  mean_tok_in  recovered")
for c, rs in sorted(by.items()):
    m = lambda k: statistics.mean(x[k] or 0 for x in rs)
    print(f"{c}    {len(rs)}  {m('duration_s'):9.1f}  {m('num_turns'):10.1f}  {m('tokens_in'):11.0f}  {sum(bool(x.get('recovered')) for x in rs)}/{len(rs)}")
