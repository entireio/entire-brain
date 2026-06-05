#!/usr/bin/env python3
"""Opus compact-mode result: re-derive audit `ok` with the committed (tested)
model-aware mcp_condition_audit, then report compact-Opus vs no_brain per cell.

Re-derivation is deterministic and applies the committed audit bugfix to records
that were collected by in-flight processes loading the pre-fix module — it does
NOT change any measured metric (validation, tokens, time, cost, score), only the
audit-derived `ok` field. Lead metrics are TIME + COST (raw total_tokens are
cache-read-inflated), plus validation pass-rate and composite score.
"""
import glob, importlib.util, json, sys, pathlib

RUN = pathlib.Path(__file__).with_name("run.py")
spec = importlib.util.spec_from_file_location("agent_brain_run", RUN)
run = importlib.util.module_from_spec(spec); sys.modules[spec.name] = run; spec.loader.exec_module(run)


def g(d, *p, dv=None):
    c = d
    for k in p:
        c = c.get(k) if isinstance(c, dict) else None
    return c if c is not None else dv


def rederive(rec):
    runner = None
    rd = rec.get("runner")
    if isinstance(rd, dict):
        runner = run.RunnerSpec(id=rd.get("id", ""), agent=rd.get("agent", ""), model=rd.get("model"), effort=rd.get("effort"))
    ai = rec.get("agent_info", {}) or {}
    audit = run.mcp_condition_audit(rec.get("condition", ""), ai, runner)
    rec["mcp_condition_audit"] = audit
    rec["ok"] = bool(g(rec, "validation", "ok")) and (ai.get("returncode") == 0) \
        and bool(g(rec, "leak_audit", "ok", dv=True)) and bool(audit.get("ok"))
    return rec


def main():
    # 1. re-derive ok for every opusfix record, rewrite in place
    n_flipped = 0
    for f in glob.glob("results/opusfix-*/records.ndjson"):
        out = []
        for ln in open(f).read().splitlines():
            if not ln.strip():
                continue
            rec = json.loads(ln); before = rec.get("ok")
            rederive(rec)
            if rec.get("ok") != before:
                n_flipped += 1
            out.append(json.dumps(rec))
        pathlib.Path(f).write_text("\n".join(out) + "\n")
    print(f"re-derived ok on opusfix records; flipped {n_flipped} (audit bugfix applied)\n")

    # 2. aggregate compact-Opus vs no_brain per (task, effort)
    cells = {}
    for f in glob.glob("results/opusfix-*/records.ndjson"):
        for ln in open(f).read().splitlines():
            if not ln.strip():
                continue
            rec = json.loads(ln)
            if g(rec, "agent_info", "returncode") not in (0, None):
                continue
            t = rec["run_id"].split("__")[0].replace("entireio-cli-", "")
            e = g(rec, "runner", "effort")
            cells.setdefault((t, e, rec["condition"]), []).append(rec)

    def agg(recs):
        n = len(recs)
        return {
            "n": n,
            "valid": sum(1 for r in recs if g(r, "validation", "ok")),
            "score": round(sum(g(r, "score", "total", dv=0) for r in recs) / n, 1),
            "time": round(sum(g(r, "agent_info", "seconds", dv=0) for r in recs) / n),
            "cost": round(sum(g(r, "agent_info", "usage", "cost_usd", dv=0) for r in recs) / n, 3),
            "tok": round(sum(g(r, "agent_info", "usage", "total_tokens", dv=0) for r in recs) / n),
            "out": round(sum(g(r, "agent_info", "usage", "output_tokens", dv=0) for r in recs) / n),
        }

    tasks = sorted({k[0] for k in cells}); efforts = ["low", "medium", "high", "xhigh"]
    hdr = f"{'task':12}{'effort':8}{'cond':18}{'valid':7}{'score':7}{'time':7}{'cost':8}{'Δtime':8}{'Δcost':8}{'Δscore':7}"
    print(hdr); print("-" * len(hdr))
    wins = {"mcp_history": {"all3": 0, "tot": 0}, "full_cli_compact": {"both": 0, "tot": 0}}
    for t in tasks:
        for e in efforts:
            nb = cells.get((t, e, "no_brain"))
            if not nb:
                continue
            b = agg(nb)
            for c in ["no_brain", "full_cli_compact", "mcp_history"]:
                rs = cells.get((t, e, c))
                if not rs:
                    continue
                a = agg(rs)
                dt = dc = ds = ""
                if c != "no_brain":
                    dt = f"{(a['time']-b['time'])/b['time']*100:+.0f}%"
                    dc = f"{(a['cost']-b['cost'])/b['cost']*100:+.0f}%" if b['cost'] else "n/a"
                    ds = f"{a['score']-b['score']:+.1f}"
                    if c == "mcp_history":
                        wins["mcp_history"]["tot"] += 1
                        if a['time'] < b['time'] and a['cost'] < b['cost'] and a['score'] > b['score']:
                            wins["mcp_history"]["all3"] += 1
                    else:
                        wins["full_cli_compact"]["tot"] += 1
                        if a['time'] < b['time'] and a['cost'] < b['cost']:
                            wins["full_cli_compact"]["both"] += 1
                print(f"{t:12}{e:8}{c:18}{str(a['valid'])+'/'+str(a['n']):7}{a['score']:<7}{a['time']:<7}{a['cost']:<8}{dt:8}{dc:8}{ds:7}")
    print()
    print(f"MCP cells where Opus is faster AND cheaper AND higher-score: {wins['mcp_history']['all3']}/{wins['mcp_history']['tot']}")
    print(f"CLI cells where Opus is faster AND cheaper:                  {wins['full_cli_compact']['both']}/{wins['full_cli_compact']['tot']}")

    # 3. pooled
    print("\n=== POOLED (all efforts + both tasks) compact-Opus vs no_brain ===")
    pool = {}
    for (t, e, c), rs in cells.items():
        pool.setdefault(c, []).extend(rs)
    b = agg(pool["no_brain"])
    print(f"{'cond':18}{'valid':9}{'score':8}{'time':7}{'cost':8}{'tok':9}{'out':8}")
    for c in ["no_brain", "full_cli_compact", "mcp_history"]:
        a = agg(pool[c])
        d = ""
        if c != "no_brain":
            d = f"  Δtime {(a['time']-b['time'])/b['time']*100:+.0f}%  Δcost {(a['cost']-b['cost'])/b['cost']*100:+.0f}%  Δscore {a['score']-b['score']:+.1f}"
        print(f"{c:18}{str(a['valid'])+'/'+str(a['n']):9}{a['score']:<8}{a['time']:<7}{a['cost']:<8}{a['tok']:<9}{a['out']:<8}{d}")


if __name__ == "__main__":
    main()
