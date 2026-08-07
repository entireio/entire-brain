#!/usr/bin/env python3
"""Comprehensive entireio/cli brain-vs-no-brain report: full tables + many 2D charts + 3D charts.

Reads the integrity-clean cliproof-* suites (drops audit hard-flagged runs and infra failures),
then emits to results/full-report/:
  - tables: master (every cell), per-model "no-effort" rollup, per-agent overall, per-condition
    overall, effort sweep  (each as .md and .csv)
  - 2D charts: per-model x condition (score/time/tokens/search/valid), brain deltas, effort sweeps,
    per-model "no-effort" rollup, per-agent overall
  - 3D charts: tokens x score x time colored by condition / model / agent, plus per-task
Run: python3 full_report.py
"""
import json, glob, statistics as stats
from collections import defaultdict
from pathlib import Path

import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
from mpl_toolkits.mplot3d import Axes3D  # noqa: F401
import numpy as np

HERE = Path(__file__).resolve().parent
RESULTS = HERE / "results"
OUT = RESULTS / "full-report"
OUT.mkdir(parents=True, exist_ok=True)

SUITE_GLOBS = ["cliproof-tr-*", "cliproof-rv-*"]
SCENARIO = {
    "entireio-cli-review-base-flag-scope": "review-flag-scope",
    "entireio-cli-transcript-reresolve": "transcript-reresolve",
}
COND_ORDER = ["no_brain", "semantic_history_cli_compact", "mcp_history"]
COND_LABEL = {"no_brain": "no_brain (grep)", "semantic_history_cli_compact": "Brain via CLI", "mcp_history": "Brain via MCP"}
COND_COLOR = {"no_brain": "#8d99ae", "semantic_history_cli_compact": "#2a9d8f", "mcp_history": "#1d4e89"}
COND_MARK = {"no_brain": "o", "semantic_history_cli_compact": "s", "mcp_history": "^"}
MODEL_ORDER = ["claude-opus-4-8", "claude-sonnet-4-6", "claude-haiku-4-5", "gpt-5.5", "gpt-5.4-mini"]
MODEL_SHORT = {"claude-opus-4-8": "Opus", "claude-sonnet-4-6": "Sonnet", "claude-haiku-4-5": "Haiku",
               "gpt-5.5": "GPT-5.5", "gpt-5.4-mini": "GPT-5.4-mini"}
MODEL_COLOR = {"claude-opus-4-8": "#6a4c93", "claude-sonnet-4-6": "#1982c4", "claude-haiku-4-5": "#8ac926",
               "gpt-5.5": "#ff595e", "gpt-5.4-mini": "#ffca3a"}
AGENT_LABEL = {"claude": "Claude", "codex": "Codex (GPT)"}
EFFORT_ORDER = ["low", "medium", "high", "xhigh"]


def g(d, *path, default=None):
    cur = d
    for k in path:
        if not isinstance(cur, dict):
            return default
        cur = cur.get(k)
    return cur if cur is not None else default


def load_flagged():
    p = RESULTS / "codex-audit-report.json"
    flagged = set()
    if p.exists():
        data = json.loads(p.read_text())
        for sname, sdata in data.get("suites", {}).items():
            for r in sdata.get("records", []):
                if r.get("flags"):
                    flagged.add((sname, r["run_id"]))
    return flagged


def load_rows():
    flagged = load_flagged()
    suites = []
    for pat in SUITE_GLOBS:
        suites += [Path(d).name for d in glob.glob(str(RESULTS / pat)) if (Path(d) / "records.ndjson").exists()]
    rows = []
    for suite in sorted(set(suites)):
        for line in (RESULTS / suite / "records.ndjson").read_text().splitlines():
            line = line.strip()
            if not line:
                continue
            try:
                rec = json.loads(line)
            except json.JSONDecodeError:
                continue
            run_id = rec.get("run_id", "")
            if "__prep__" in run_id or (suite, run_id) in flagged:
                continue
            rc = g(rec, "agent_info", "returncode")
            if rc not in (0, None):
                continue
            tail = (g(rec, "agent_info", "stdout_tail", default="") or "") + (g(rec, "agent_info", "stderr_tail", default="") or "")
            if "session limit" in tail or '"api_error_status":429' in tail:
                continue
            toks = g(rec, "agent_info", "usage", "total_tokens")
            secs = g(rec, "agent_info", "seconds")
            sc = g(rec, "score", "total")
            if (not secs or secs < 1) and not toks:
                continue
            if (sc in (0, None)) and not toks:
                continue
            act = g(rec, "agent_info", "activity", default={}) or {}
            rows.append({
                "task": SCENARIO.get(rec.get("task_id", ""), rec.get("task_id", "")),
                "agent": rec.get("agent"),
                "model": g(rec, "runner", "model") or rec.get("agent"),
                "effort": g(rec, "runner", "effort") or "na",
                "condition": rec.get("condition"),
                "valid": bool(g(rec, "validation", "ok")),
                "score": sc,
                "seconds": secs,
                "tokens": toks,
                "cost": g(rec, "agent_info", "usage", "cost_usd"),
                "mcp": int(act.get("mcp_tool_calls") or 0),
                "search": int(act.get("search_calls") or 0),
            })
    return rows


def mean(xs):
    xs = [x for x in xs if isinstance(x, (int, float))]
    return stats.mean(xs) if xs else 0.0


def summarize(rs):
    return {
        "n": len(rs),
        "valid": sum(1 for x in rs if x["valid"]),
        "score": mean([x["score"] for x in rs]),
        "seconds": mean([x["seconds"] for x in rs]),
        "tokens": mean([x["tokens"] for x in rs]),
        "cost": mean([x["cost"] for x in rs]),
        "mcp": mean([x["mcp"] for x in rs]),
        "search": mean([x["search"] for x in rs]),
    }


def group(rows, keyfn):
    d = defaultdict(list)
    for r in rows:
        d[keyfn(r)].append(r)
    return {k: summarize(v) for k, v in d.items()}


def pct(new, base):
    if not base:
        return 0.0
    return (new - base) / base * 100.0


# ---------------------------------------------------------------- tables --------
def write_table(path_md, path_csv, headers, table_rows):
    md = ["| " + " | ".join(headers) + " |", "|" + "|".join(["---"] * len(headers)) + "|"]
    for r in table_rows:
        md.append("| " + " | ".join(str(c) for c in r) + " |")
    path_md.write_text("\n".join(md) + "\n")
    csv = [",".join(headers)] + [",".join(str(c).replace(",", ";") for c in r) for r in table_rows]
    path_csv.write_text("\n".join(csv) + "\n")


def fmt_k(x):
    return f"{x/1000:.0f}k"


def model_sorted(keys):
    return sorted(keys, key=lambda m: MODEL_ORDER.index(m) if m in MODEL_ORDER else 99)


def build_tables(rows):
    # T1 — full master, every (task, model, effort, condition) with deltas vs no_brain
    nb = [r for r in rows if r["condition"] == "no_brain"]
    base = group(nb, lambda r: (r["task"], r["model"], r["effort"]))
    A = group(rows, lambda r: (r["task"], r["model"], r["effort"], r["condition"]))
    t1 = []
    for task in ["transcript-reresolve", "review-flag-scope"]:
        for m in model_sorted({k[1] for k in A if k[0] == task}):
            for e in EFFORT_ORDER:
                for c in COND_ORDER:
                    st = A.get((task, m, e, c))
                    if not st:
                        continue
                    b = base.get((task, m, e))
                    ds = f"{st['score']-b['score']:+.1f}" if (b and c != "no_brain") else ""
                    dtok = f"{pct(st['tokens'], b['tokens']):+.0f}%" if (b and c != "no_brain") else ""
                    dtime = f"{pct(st['seconds'], b['seconds']):+.0f}%" if (b and c != "no_brain") else ""
                    t1.append([task, MODEL_SHORT.get(m, m), e, COND_LABEL[c], st["n"], f"{st['valid']}/{st['n']}",
                               f"{st['score']:.1f}", ds, fmt_k(st["tokens"]), dtok, f"{st['seconds']:.0f}s", dtime,
                               f"{st['search']:.1f}", f"{st['mcp']:.0f}", f"${st['cost']:.2f}"])
    write_table(OUT / "table1_full_master.md", OUT / "table1_full_master.csv",
                ["task", "model", "effort", "condition", "n", "valid", "score", "Δscore", "tokens", "Δtok%",
                 "time", "Δtime%", "search", "mcp", "cost"], t1)

    # T2 — per-model "no effort" rollup (pooled across efforts AND tasks)
    Mb = group(nb, lambda r: (r["model"],))
    M = group(rows, lambda r: (r["model"], r["condition"]))
    t2 = []
    for m in model_sorted({k[0] for k in M}):
        for c in COND_ORDER:
            st = M.get((m, c))
            if not st:
                continue
            b = Mb.get((m,))
            ds = f"{st['score']-b['score']:+.1f}" if (b and c != "no_brain") else ""
            dtok = f"{pct(st['tokens'], b['tokens']):+.0f}%" if (b and c != "no_brain") else ""
            dtime = f"{pct(st['seconds'], b['seconds']):+.0f}%" if (b and c != "no_brain") else ""
            t2.append([MODEL_SHORT.get(m, m), COND_LABEL[c], st["n"], f"{100*st['valid']/st['n']:.0f}%",
                       f"{st['score']:.1f}", ds, fmt_k(st["tokens"]), dtok, f"{st['seconds']:.0f}s", dtime, f"{st['search']:.1f}"])
    write_table(OUT / "table2_per_model_noeffort.md", OUT / "table2_per_model_noeffort.csv",
                ["model", "condition", "n", "valid%", "score", "Δscore", "tokens", "Δtok%", "time", "Δtime%", "search"], t2)

    # T3 — per-agent overall (Claude vs Codex), pooled across everything
    Ab = group(nb, lambda r: (r["agent"],))
    AG = group(rows, lambda r: (r["agent"], r["condition"]))
    t3 = []
    for a in ["claude", "codex"]:
        for c in COND_ORDER:
            st = AG.get((a, c))
            if not st:
                continue
            b = Ab.get((a,))
            ds = f"{st['score']-b['score']:+.1f}" if (b and c != "no_brain") else ""
            dtok = f"{pct(st['tokens'], b['tokens']):+.0f}%" if (b and c != "no_brain") else ""
            dtime = f"{pct(st['seconds'], b['seconds']):+.0f}%" if (b and c != "no_brain") else ""
            t3.append([AGENT_LABEL[a], COND_LABEL[c], st["n"], f"{100*st['valid']/st['n']:.0f}%",
                       f"{st['score']:.1f}", ds, fmt_k(st["tokens"]), dtok, f"{st['seconds']:.0f}s", dtime, f"{st['search']:.1f}"])
    write_table(OUT / "table3_per_agent_overall.md", OUT / "table3_per_agent_overall.csv",
                ["agent", "condition", "n", "valid%", "score", "Δscore", "tokens", "Δtok%", "time", "Δtime%", "search"], t3)

    # T4 — overall by condition (everything pooled)
    Ob = group(nb, lambda r: ("all",))
    O = group(rows, lambda r: ("all", r["condition"]))
    t4 = []
    for c in COND_ORDER:
        st = O.get(("all", c)); b = Ob.get(("all",))
        ds = f"{st['score']-b['score']:+.1f}" if c != "no_brain" else ""
        dtok = f"{pct(st['tokens'], b['tokens']):+.0f}%" if c != "no_brain" else ""
        dtime = f"{pct(st['seconds'], b['seconds']):+.0f}%" if c != "no_brain" else ""
        t4.append([COND_LABEL[c], st["n"], f"{100*st['valid']/st['n']:.0f}%", f"{st['score']:.1f}", ds,
                   fmt_k(st["tokens"]), dtok, f"{st['seconds']:.0f}s", dtime, f"{st['search']:.1f}", f"${st['cost']:.2f}"])
    write_table(OUT / "table4_overall_by_condition.md", OUT / "table4_overall_by_condition.csv",
                ["condition", "n", "valid%", "score", "Δscore", "tokens", "Δtok%", "time", "Δtime%", "search", "cost"], t4)

    # T5 — effort sweep (per model x effort, pooled tasks), brain-MCP delta vs no_brain
    Eb = group(rows, lambda r: (r["model"], r["effort"], "no_brain"))
    E = group(rows, lambda r: (r["model"], r["effort"], r["condition"]))
    t5 = []
    for m in model_sorted({k[0] for k in E}):
        for e in EFFORT_ORDER:
            row = [MODEL_SHORT.get(m, m), e]
            b = Eb.get((m, e, "no_brain"))
            for c in COND_ORDER:
                st = E.get((m, e, c))
                if not st:
                    row += ["-", "-"]; continue
                row.append(f"{st['score']:.0f}")
                row.append(fmt_k(st["tokens"]))
            t5.append(row)
    write_table(OUT / "table5_effort_sweep.md", OUT / "table5_effort_sweep.csv",
                ["model", "effort", "nb score", "nb tok", "CLI score", "CLI tok", "MCP score", "MCP tok"], t5)
    return A, M, AG, E


# ---------------------------------------------------------------- 2D charts -----
def bars_by_model(rows, keyfn_models, metric, scale, title, fname, fmt="{:.0f}", ylim=None, pooledtasks=True):
    """Grouped bars: x=models, groups=conditions, one metric."""
    G = group(rows, lambda r: (r["model"], r["condition"]))
    models = model_sorted({k[0] for k in G})
    fig, ax = plt.subplots(figsize=(max(9, 1.6 * len(models)), 5.2))
    width = 0.8 / len(COND_ORDER)
    x = np.arange(len(models))
    for ci, c in enumerate(COND_ORDER):
        vals = [(G.get((m, c), {}).get(metric, 0) / scale) for m in models]
        offs = x + (ci - (len(COND_ORDER) - 1) / 2) * width
        ax.bar(offs, vals, width=width, color=COND_COLOR[c], label=COND_LABEL[c])
        for xi, v in zip(offs, vals):
            ax.text(xi, v, fmt.format(v), ha="center", va="bottom", fontsize=7)
    ax.set_xticks(x); ax.set_xticklabels([MODEL_SHORT.get(m, m) for m in models], fontsize=9)
    ax.set_title(title, fontsize=12); ax.legend(fontsize=8)
    if ylim:
        ax.set_ylim(*ylim)
    fig.tight_layout(); fig.savefig(OUT / fname, dpi=140); plt.close(fig)


def chart_valid_rate(rows):
    G = group(rows, lambda r: (r["model"], r["condition"]))
    models = model_sorted({k[0] for k in G})
    fig, ax = plt.subplots(figsize=(max(9, 1.6 * len(models)), 5))
    width = 0.8 / len(COND_ORDER); x = np.arange(len(models))
    for ci, c in enumerate(COND_ORDER):
        vals = [(100 * G[(m, c)]["valid"] / G[(m, c)]["n"]) if (m, c) in G else 0 for m in models]
        offs = x + (ci - (len(COND_ORDER) - 1) / 2) * width
        ax.bar(offs, vals, width=width, color=COND_COLOR[c], label=COND_LABEL[c])
        for xi, v in zip(offs, vals):
            ax.text(xi, v, f"{v:.0f}%", ha="center", va="bottom", fontsize=7)
    ax.set_xticks(x); ax.set_xticklabels([MODEL_SHORT.get(m, m) for m in models], fontsize=9)
    ax.set_ylim(0, 109); ax.set_title("Validity rate by model × condition (↑ better)", fontsize=12); ax.legend(fontsize=8)
    fig.tight_layout(); fig.savefig(OUT / "c6_valid_rate_by_model.png", dpi=140); plt.close(fig)


def chart_brain_deltas(rows):
    """Brain (MCP & CLI) deltas vs no_brain by model: Δscore, Δtokens%, Δtime%."""
    Mb = group([r for r in rows if r["condition"] == "no_brain"], lambda r: (r["model"],))
    M = group(rows, lambda r: (r["model"], r["condition"]))
    models = model_sorted({k[0] for k in M})
    fig, axes = plt.subplots(1, 3, figsize=(15, 5))
    metrics = [("score", "Δ score (pts, ↑ better)", lambda st, b: st["score"] - b["score"]),
               ("tokens", "Δ tokens (%, ↓ better)", lambda st, b: pct(st["tokens"], b["tokens"])),
               ("seconds", "Δ time (%, ↓ better)", lambda st, b: pct(st["seconds"], b["seconds"]))]
    brain_conds = ["semantic_history_cli_compact", "mcp_history"]
    for ax, (mk, title, fn) in zip(axes, metrics):
        width = 0.8 / len(brain_conds); x = np.arange(len(models))
        for ci, c in enumerate(brain_conds):
            vals = [fn(M[(m, c)], Mb[(m,)]) if (m, c) in M and (m,) in Mb else 0 for m in models]
            offs = x + (ci - (len(brain_conds) - 1) / 2) * width
            ax.bar(offs, vals, width=width, color=COND_COLOR[c], label=COND_LABEL[c])
            for xi, v in zip(offs, vals):
                ax.text(xi, v, f"{v:+.0f}", ha="center", va="bottom" if v >= 0 else "top", fontsize=7)
        ax.axhline(0, color="#333", lw=0.8)
        ax.set_xticks(x); ax.set_xticklabels([MODEL_SHORT.get(m, m) for m in models], rotation=15, fontsize=8)
        ax.set_title(title, fontsize=11)
    axes[0].legend(fontsize=8)
    fig.suptitle("Brain impact vs no_brain, by model (pooled tasks & efforts)", fontsize=13)
    fig.tight_layout(rect=[0, 0, 1, 0.95]); fig.savefig(OUT / "c4_brain_deltas_by_model.png", dpi=140); plt.close(fig)


def chart_effort_sweep(rows):
    """Per model: score vs effort and tokens vs effort, one line per condition."""
    E = group(rows, lambda r: (r["model"], r["effort"], r["condition"]))
    models = model_sorted({k[0] for k in E})
    fig, axes = plt.subplots(2, len(models), figsize=(3.2 * len(models), 8), sharex=True)
    for j, m in enumerate(models):
        for c in COND_ORDER:
            ys = [E[(m, e, c)]["score"] if (m, e, c) in E else np.nan for e in EFFORT_ORDER]
            axes[0][j].plot(EFFORT_ORDER, ys, marker=COND_MARK[c], color=COND_COLOR[c], label=COND_LABEL[c])
            yt = [E[(m, e, c)]["tokens"] / 1000 if (m, e, c) in E else np.nan for e in EFFORT_ORDER]
            axes[1][j].plot(EFFORT_ORDER, yt, marker=COND_MARK[c], color=COND_COLOR[c])
        axes[0][j].set_title(MODEL_SHORT.get(m, m), fontsize=10)
        axes[0][j].set_ylim(55, 102)
        for ax in (axes[0][j], axes[1][j]):
            ax.tick_params(axis="x", labelrotation=30, labelsize=7)
    axes[0][0].set_ylabel("score (↑)"); axes[1][0].set_ylabel("tokens k (↓)")
    axes[0][-1].legend(fontsize=7, loc="lower right")
    fig.suptitle("Effort sweep — score & tokens vs reasoning effort, per model × condition", fontsize=13)
    fig.tight_layout(rect=[0, 0, 1, 0.96]); fig.savefig(OUT / "c5_effort_sweep.png", dpi=140); plt.close(fig)


def chart_agent_overall(rows):
    """Claude vs Codex overall, 4 metrics by condition."""
    AG = group(rows, lambda r: (r["agent"], r["condition"]))
    agents = ["claude", "codex"]
    panels = [("score", "Score (↑)", 1, "{:.0f}", (0, 105)), ("tokens", "Tokens k (↓)", 1000, "{:.0f}k", None),
              ("seconds", "Time s (↓)", 1, "{:.0f}s", None), ("search", "Search calls (↓)", 1, "{:.1f}", None)]
    fig, axes = plt.subplots(1, 4, figsize=(18, 4.8))
    for ax, (mk, title, scale, fmt, ylim) in zip(axes, panels):
        width = 0.8 / len(COND_ORDER); x = np.arange(len(agents))
        for ci, c in enumerate(COND_ORDER):
            vals = [AG[(a, c)][mk] / scale if (a, c) in AG else 0 for a in agents]
            offs = x + (ci - (len(COND_ORDER) - 1) / 2) * width
            ax.bar(offs, vals, width=width, color=COND_COLOR[c], label=COND_LABEL[c])
            for xi, v in zip(offs, vals):
                ax.text(xi, v, fmt.format(v), ha="center", va="bottom", fontsize=7)
        ax.set_xticks(x); ax.set_xticklabels([AGENT_LABEL[a] for a in agents], fontsize=9); ax.set_title(title, fontsize=11)
        if ylim:
            ax.set_ylim(*ylim)
    axes[0].legend(fontsize=8)
    fig.suptitle("Overall: Claude vs Codex (GPT) by condition — pooled across models, efforts, tasks", fontsize=13)
    fig.tight_layout(rect=[0, 0, 1, 0.94]); fig.savefig(OUT / "c3_agent_overall.png", dpi=140); plt.close(fig)


def chart_model_noeffort(rows):
    """Per-model rollup (no effort): score / tokens / time / search by condition."""
    bars_by_model(rows, None, "score", 1, "Score by model × condition — pooled efforts & tasks (↑ better)",
                  "c2a_model_noeffort_score.png", "{:.0f}", (0, 105))
    bars_by_model(rows, None, "tokens", 1000, "Tokens by model × condition — pooled (↓ better)",
                  "c2b_model_noeffort_tokens.png", "{:.0f}k")
    bars_by_model(rows, None, "seconds", 1, "Time by model × condition — pooled (↓ better)",
                  "c2c_model_noeffort_time.png", "{:.0f}s")
    bars_by_model(rows, None, "search", 1, "Search calls by model × condition — pooled (↓ better)",
                  "c2d_model_noeffort_search.png", "{:.1f}")


# ---------------------------------------------------------------- 3D charts -----
COND_COLOR_3D = {"no_brain": "#e63946", "semantic_history_cli_compact": "#2a9d8f", "mcp_history": "#1d4e89"}


def scatter3d(rows, colorby, fname, title):
    # two viewing angles side by side for readability
    fig = plt.figure(figsize=(18, 8))
    if colorby == "condition":
        color_of = lambda r: COND_COLOR_3D[r["condition"]]; marker_of = lambda r: COND_MARK[r["condition"]]
        legend = [(COND_LABEL[c], COND_COLOR_3D[c], COND_MARK[c]) for c in COND_ORDER]
    elif colorby == "model":
        legend = [(MODEL_SHORT[m], MODEL_COLOR[m], "o") for m in MODEL_ORDER]
        color_of = lambda r: MODEL_COLOR.get(r["model"], "#999"); marker_of = lambda r: "o"
    else:  # agent
        amap = {"claude": "#1982c4", "codex": "#ff595e"}
        legend = [(AGENT_LABEL[a], amap[a], "o") for a in ["claude", "codex"]]
        color_of = lambda r: amap.get(r["agent"], "#999"); marker_of = lambda r: "o"
    pts = [r for r in rows if (r["tokens"] and r["seconds"] and r["score"])]
    for vi, (elev, azim) in enumerate([(20, -60), (28, 25)]):
        ax = fig.add_subplot(1, 2, vi + 1, projection="3d")
        for r in pts:
            ax.scatter(r["tokens"] / 1000, r["score"], r["seconds"],
                       c=color_of(r), marker=marker_of(r), s=42, alpha=0.78, edgecolors="white", linewidths=0.3)
        ax.set_xlabel("tokens k (↓ better)", fontsize=9); ax.set_ylabel("score (↑ better)", fontsize=9)
        ax.set_zlabel("time s (↓ better)", fontsize=9)
        ax.view_init(elev=elev, azim=azim)
    handles = [plt.Line2D([0], [0], marker=mk, color="w", markerfacecolor=col, markersize=11, label=lab)
               for lab, col, mk in legend]
    fig.legend(handles=handles, fontsize=10, loc="upper center", ncol=len(legend))
    fig.suptitle(title, fontsize=13, y=0.98)
    fig.tight_layout(rect=[0, 0, 1, 0.93]); fig.savefig(OUT / fname, dpi=140); plt.close(fig)


def chart_quality_vs_efficiency(rows):
    """The honest headline: QUALITY (validation pass-rate, hard data) on the left,
    EFFICIENCY (mean tokens, measured) on the right — no composite score, which
    mixes in brain_use/runtime-efficiency components and can mislead."""
    G = group(rows, lambda r: (r["model"], r["condition"]))
    models = model_sorted({k[0] for k in G})
    fig, axes = plt.subplots(1, 2, figsize=(17, 5.4))
    width = 0.8 / len(COND_ORDER)
    x = np.arange(len(models))
    # left: pass-rate (quality)
    for ci, c in enumerate(COND_ORDER):
        vals = [(100 * G[(m, c)]["valid"] / G[(m, c)]["n"]) if (m, c) in G else 0 for m in models]
        offs = x + (ci - (len(COND_ORDER) - 1) / 2) * width
        axes[0].bar(offs, vals, width=width, color=COND_COLOR[c], label=COND_LABEL[c])
        for xi, v in zip(offs, vals):
            axes[0].text(xi, v, f"{v:.0f}", ha="center", va="bottom", fontsize=7)
    axes[0].set_ylim(0, 109); axes[0].set_title("QUALITY — validation pass-rate % (↑ better)", fontsize=12)
    axes[0].legend(fontsize=8)
    # right: tokens (efficiency/cost)
    for ci, c in enumerate(COND_ORDER):
        vals = [(G[(m, c)]["tokens"] / 1000) if (m, c) in G else 0 for m in models]
        offs = x + (ci - (len(COND_ORDER) - 1) / 2) * width
        axes[1].bar(offs, vals, width=width, color=COND_COLOR[c], label=COND_LABEL[c])
        for xi, v in zip(offs, vals):
            axes[1].text(xi, v, f"{v:.0f}k", ha="center", va="bottom", fontsize=7)
    axes[1].set_title("EFFICIENCY — mean tokens, thousands (↓ better)", fontsize=12)
    for ax in axes:
        ax.set_xticks(x); ax.set_xticklabels([MODEL_SHORT.get(m, m) for m in models], fontsize=9)
    fig.suptitle("Brain vs grep — measured quality and efficiency (no composite score)", fontsize=13)
    fig.tight_layout(rect=[0, 0, 1, 0.95]); fig.savefig(OUT / "c0_quality_vs_efficiency.png", dpi=140); plt.close(fig)


def main():
    rows = load_rows()
    print(f"loaded {len(rows)} integrity-clean cliproof records")
    build_tables(rows)
    # 2D
    chart_quality_vs_efficiency(rows)
    chart_model_noeffort(rows)
    chart_agent_overall(rows)
    chart_brain_deltas(rows)
    chart_effort_sweep(rows)
    chart_valid_rate(rows)
    # per-task model×condition score/tokens/time
    for task in ["transcript-reresolve", "review-flag-scope"]:
        tr = [r for r in rows if r["task"] == task]
        bars_by_model(tr, None, "score", 1, f"[{task}] Score by model × condition (↑)",
                      f"c1_{task}_score.png", "{:.0f}", (0, 105))
        bars_by_model(tr, None, "tokens", 1000, f"[{task}] Tokens by model × condition (↓)",
                      f"c1_{task}_tokens.png", "{:.0f}k")
    # 3D
    scatter3d(rows, "condition", "d1_3d_by_condition.png", "All 360 runs — tokens × score × time, colored by CONDITION")
    scatter3d(rows, "model", "d2_3d_by_model.png", "All 360 runs — tokens × score × time, colored by MODEL")
    scatter3d(rows, "agent", "d3_3d_by_agent.png", "All 360 runs — tokens × score × time, colored by AGENT")
    for task in ["transcript-reresolve", "review-flag-scope"]:
        scatter3d([r for r in rows if r["task"] == task], "condition",
                  f"d4_3d_{task}_by_condition.png", f"[{task}] tokens × score × time by condition")
    print("wrote tables + charts to", OUT)


if __name__ == "__main__":
    main()
