"""Summarise runs for one question: per-run metrics, rubric hits, and per-condition means.

    python3 grade.py [question=global-activation] [--json]

Rubric regexes come from questions/<question>.rubric.json; 'recovered' is a first pass
and should be hand-checked against the answers in runs/<question>/*/summary.json.
"""
import glob, json, os, re, statistics, sys

here = os.path.dirname(os.path.abspath(__file__))
q = next((a for a in sys.argv[1:] if not a.startswith("--")), "global-activation")
if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]*", q):
    sys.exit(f"question name {q!r} must match [A-Za-z0-9][A-Za-z0-9._-]* (it names files under questions/)")
out_dir = os.environ.get("LADDER_OUT", os.path.join(here, "runs"))
rubric_path = os.path.join(here, "questions", f"{q}.rubric.json")
if not os.path.isfile(rubric_path):
    sys.exit(f"no rubric for question {q!r}: {rubric_path}")
rubric = {k: v for k, v in json.load(open(rubric_path)).items() if not k.startswith("_")}
rows = []
for sj in sorted(glob.glob(os.path.join(out_dir, q, "c*-r*", "summary.json"))):
    d = json.load(open(sj))
    text = d.get("result") or ""
    hits = {k: bool(re.search(p, text, re.I | re.S)) for k, p in rubric.items()}
    if "completed" in d:
        completed = bool(d["completed"])
    else:  # summary written by an older parse.py: derive the same rule
        completed = not d.get("is_error") and d.get("subtype") == "success" and bool(d.get("result"))
    if not completed:
        hits = {k: None for k in rubric}  # an unfinished run has no answer to grade
    rows.append({"run": d["cond"], "completed": completed, **{k: d.get(k) for k in ("duration_s", "cost_usd", "num_turns", "tool_calls", "tokens_in", "tokens_out", "is_error", "subtype", "entire_subcommands")}, **hits})
if "--json" in sys.argv:
    print(json.dumps(rows, indent=1)); sys.exit()
cols = ["completed", "duration_s", "num_turns", "tool_calls", "tokens_in"] + list(rubric)
print("run".ljust(8) + "".join(c[:11].ljust(12) for c in cols))
for r in rows:
    print(r["run"].ljust(8) + "".join(str(r.get(c))[:11].ljust(12) for c in cols))
by = {}
for r in rows:
    by.setdefault(r["run"].split("-")[0], []).append(r)
print("\nMeans and the recovered denominator use completed runs only; failed runs are counted separately.")
print("cond  completed/total  mean_dur_s  mean_turns  mean_tok_in  recovered")
for c, rs in sorted(by.items()):
    ok = [x for x in rs if x["completed"]]
    m = lambda k: statistics.mean(x[k] or 0 for x in ok) if ok else float("nan")
    rec = sum(bool(x.get("recovered")) for x in ok)
    print(f"{c}    {len(ok):>9}/{len(rs):<6}  {m('duration_s'):9.1f}  {m('num_turns'):10.1f}  {m('tokens_in'):11.0f}  {rec}/{len(ok)}")
failed = [r["run"] for r in rows if not r["completed"]]
if failed:
    print("\nfailed or incomplete runs (excluded from means):", ", ".join(failed))
