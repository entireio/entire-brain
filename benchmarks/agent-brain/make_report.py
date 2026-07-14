#!/usr/bin/env python3
"""Build the Entire Brain proof report: master table (md+csv) + per-model charts.

Charts are PER SPECIFIC MODEL (no cross-model averaging): each runner
(model+effort) is its own x-axis group, with one bar per condition. Claude
(n=3, all 3 conditions) and Codex (per effort) are drawn on separate charts so
n=3 results are never mixed with n=1 pilots.

Reads integrity-clean proof suites (cross-references codex-audit-report.json to
drop any run with a hard integrity flag). Read-only w.r.t. records; writes only
under results/report/.
"""
from __future__ import annotations

import csv
import json
import pathlib
import statistics as stats
from collections import defaultdict
from typing import Any

import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
from mpl_toolkits.mplot3d import Axes3D  # noqa: F401

BENCH = pathlib.Path(__file__).resolve().parent
RESULTS = BENCH / "results"
OUT = RESULTS / "report"
OUT.mkdir(parents=True, exist_ok=True)

# Integrity-clean proof suites, discovered by glob so no hard-coded suite names.
# Hard-flagged runs are dropped per-run via codex-audit-report.json regardless of suite.
CODEX_SUITE_GLOBS = [
    # MCP per-effort (no_brain + mcp_history)
    "*mcp-history-codex-low-med-tight-checklist*",
    "*mcp-history-codex-high-xhigh-tight-checklist*",
    "*mcp-history-codex-xhigh-oversearch-guard*",
    # CLI per-effort (no_brain + full_cli_compact) — provides the CLI bars at med/high/xhigh
    "ultron-self-contained-mini-efforts-hardened-actions-*",
    "ultron-self-contained-gpt55-efforts-hardened-actions-*",
    # low-effort n=3 (CLI proof)
    "ultron-self-contained-mini-low-n3-*",
    "ultron-self-contained-gpt55-low-n3-*",
    "ultron-metadata-mini-low-n3-*",
    "ultron-metadata-gpt55-low-n3-*",
]
CLAUDE_SUITE_GLOBS = ["claude-s1-n3-*", "claude-s2-n3-*"]
# entireio/cli proof, done properly: fresh clone + real sessions (entire brain refresh),
# brain_brief/action_checklist delivery, full model x effort matrix, n=3.
# (The stale semantic-only cli-monorepo-ab-* / cli-tr-* suites are intentionally excluded:
# they had no real sessions and 0 valid runs.)
CLI_SUITE_GLOBS = ["cliproof-tr-*", "cliproof-rv-*"]

SCENARIO = {
    "ultron-history-agentic-self-contained-turns": "self-contained-turns",
    "ultron-history-openai-metadata-step-string": "metadata",
    "ultron-history-storage-limit-normalization": "storage-limit",
    "ultron-history-youtube-media-verification": "youtube-media",
    "entireio-cli-review-base-flag-scope": "cli-review-flag-scope",
    "entireio-cli-transcript-reresolve-updates-state": "cli-transcript-reresolve",
    "entireio-cli-transcript-reresolve": "cli-transcript-reresolve",
}

COND_ORDER = ["no_brain", "semantic_brain", "full_cli_compact", "mcp_semantic", "mcp_history"]
COND_LABEL = {"no_brain": "no_brain (grep)", "semantic_brain": "Brain via CLI (graph)",
              "full_cli_compact": "Brain via CLI", "mcp_semantic": "Brain via MCP (graph)",
              "mcp_history": "Brain via MCP"}
COND_COLOR = {"no_brain": "#8d99ae", "semantic_brain": "#3aa6a0", "full_cli_compact": "#2a9d8f",
              "mcp_semantic": "#21897e", "mcp_history": "#1d7874"}


def load_hard_flagged_runs() -> set[tuple[str, str]]:
    # Keyed by (suite, run_id): run_ids are NOT unique across suites, so a flag in
    # one suite must not drop an identically-named clean run in another suite.
    p = RESULTS / "codex-audit-report.json"
    flagged: set[tuple[str, str]] = set()
    if p.exists():
        data = json.loads(p.read_text())
        for sname, sdata in data.get("suites", {}).items():
            for r in sdata.get("records", []):
                if r.get("flags"):
                    flagged.add((sname, r["run_id"]))
    return flagged


def g(d: Any, *path, default=None):
    cur = d
    for k in path:
        if not isinstance(cur, dict):
            return default
        cur = cur.get(k)
    return cur if cur is not None else default


def model_short(model: str) -> str:
    return {
        "claude-haiku-4-5": "Haiku", "claude-sonnet-4-6": "Sonnet", "claude-opus-4-8": "Opus",
        "gpt-5.4-mini": "5.4-mini", "gpt-5.5": "5.5",
    }.get(model, model)


def collect() -> list[dict[str, Any]]:
    flagged = load_hard_flagged_runs()
    suites: list[str] = []
    for pat in CODEX_SUITE_GLOBS + CLAUDE_SUITE_GLOBS + CLI_SUITE_GLOBS:
        suites += [d.name for d in RESULTS.glob(pat) if d.is_dir() and (d / "records.ndjson").exists()]
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
            # Drop infra failures (non-zero exit, provider rate-limit/429 bail,
            # or empty/zero output) so charts reflect real agent behavior, not quota.
            rc = g(rec, "agent_info", "returncode")
            tail = (g(rec, "agent_info", "stdout_tail", default="") or "") + (g(rec, "agent_info", "stderr_tail", default="") or "")
            secs = g(rec, "agent_info", "seconds")
            toks = g(rec, "agent_info", "usage", "total_tokens")
            sc = g(rec, "score", "total")
            if rc not in (0, None):
                continue
            if "session limit" in tail or '"api_error_status":429' in tail:
                continue
            # "didn't actually run" guard: no time AND no tokens (provider error / empty output)
            if (not secs or secs < 1) and not toks:
                continue
            if (sc in (0, None)) and not toks:
                continue
            act = g(rec, "agent_info", "activity", default={}) or {}
            rows.append({
                "suite": suite,
                "scenario": SCENARIO.get(rec.get("task_id", ""), rec.get("task_id", "")),
                "agent": rec.get("agent"),
                "model": g(rec, "runner", "model") or rec.get("agent"),
                "effort": g(rec, "runner", "effort") or "na",
                "condition": rec.get("condition"),
                "valid": bool(g(rec, "validation", "ok")),
                "score": g(rec, "score", "total"),
                "seconds": g(rec, "agent_info", "seconds"),
                "tokens": g(rec, "agent_info", "usage", "total_tokens"),
                "cost": g(rec, "agent_info", "usage", "cost_usd"),
                "mcp": int(act.get("mcp_tool_calls") or 0),
                "search": int(act.get("search_calls") or 0),
            })
    return rows


def mean(xs):
    xs = [x for x in xs if isinstance(x, (int, float))]
    return stats.mean(xs) if xs else 0.0


def agg(rows):
    """(scenario,agent,model,effort,condition) -> stats dict."""
    groups: dict[tuple, list[dict]] = defaultdict(list)
    for r in rows:
        groups[(r["scenario"], r["agent"], r["model"], r["effort"], r["condition"])].append(r)
    out = {}
    for k, rs in groups.items():
        out[k] = {
            "n": len(rs),
            "valid": sum(1 for x in rs if x["valid"]),
            "score": mean([x["score"] for x in rs]),
            "seconds": mean([x["seconds"] for x in rs]),
            "tokens": mean([x["tokens"] for x in rs]),
            "cost": mean([x["cost"] for x in rs]),
            "mcp": mean([x["mcp"] for x in rs]),
            "search": mean([x["search"] for x in rs]),
        }
    return out


# ---- per-model grouped bar charts ---------------------------------------------

def grouped_bars(ax, runners, runner_label, A, scenario, agent, key, scale, fmt):
    """One panel: x=runners, grouped bars by condition for one metric."""
    conds = [c for c in COND_ORDER
             if any((scenario, agent, m, e, c) in A for (m, e) in runners)]
    width = 0.8 / max(1, len(conds))
    x = list(range(len(runners)))
    for ci, cond in enumerate(conds):
        vals, labels = [], []
        for (m, e) in runners:
            st = A.get((scenario, agent, m, e, cond))
            v = (st[key] / scale) if st else 0.0
            vals.append(v)
            labels.append(st)
        offs = [xi + (ci - (len(conds) - 1) / 2) * width for xi in x]
        bars = ax.bar(offs, vals, width=width, color=COND_COLOR[cond], label=COND_LABEL[cond])
        for xi, v, st in zip(offs, vals, labels):
            if st is None:
                continue
            ax.text(xi, v, fmt(v, st), ha="center", va="bottom", fontsize=7)
    ax.set_xticks(x)
    ax.set_xticklabels([runner_label(m, e) for (m, e) in runners], rotation=20, ha="right", fontsize=8)


def chart_family_scenario(rows, A, scenario, agent, runners, runner_label, fname, title):
    if not any((scenario, agent, m, e, c) in A for (m, e) in runners for c in COND_ORDER):
        return
    fig, axes = plt.subplots(1, 3, figsize=(max(11, 3 * len(runners)), 5.2))
    grouped_bars(axes[0], runners, runner_label, A, scenario, agent, "score", 1,
                 lambda v, st: f"{v:.0f}\n{st['valid']}/{st['n']}")
    axes[0].set_title("Score (↑ better) + valid runs")
    axes[0].set_ylim(0, 105)
    grouped_bars(axes[1], runners, runner_label, A, scenario, agent, "seconds", 1,
                 lambda v, st: f"{v:.0f}s")
    axes[1].set_title("Agent time, s (↓ better)")
    grouped_bars(axes[2], runners, runner_label, A, scenario, agent, "tokens", 1000,
                 lambda v, st: f"{v:.0f}k")
    axes[2].set_title("Tokens, k (↓ better)")
    axes[1].legend(loc="upper right", fontsize=8)
    fig.suptitle(title, fontsize=13)
    fig.tight_layout(rect=[0, 0, 1, 0.95])
    fig.savefig(OUT / fname, dpi=140)
    plt.close(fig)


def chart_scatter(rows, scenario):
    srows = [r for r in rows if r["scenario"] == scenario and isinstance(r["tokens"], (int, float))
             and isinstance(r["score"], (int, float))]
    if not srows:
        return
    fig, ax = plt.subplots(figsize=(9, 6))
    markers = {"claude-haiku-4-5": "o", "claude-sonnet-4-6": "s", "claude-opus-4-8": "^",
               "gpt-5.4-mini": "D", "gpt-5.5": "P"}
    for cond in COND_ORDER:
        for r in [x for x in srows if x["condition"] == cond]:
            ax.scatter(r["tokens"] / 1000, r["score"], color=COND_COLOR[cond],
                       marker=markers.get(r["model"], "o"),
                       s=80, alpha=0.5 if not r["valid"] else 0.95,
                       edgecolors="black" if not r["valid"] else "none", linewidths=0.8)
    # legends
    cond_handles = [plt.Line2D([0], [0], marker="o", color="w", markerfacecolor=COND_COLOR[c],
                    markersize=10, label=COND_LABEL[c]) for c in COND_ORDER]
    model_handles = [plt.Line2D([0], [0], marker=mk, color="w", markerfacecolor="#444",
                     markersize=9, label=model_short(m)) for m, mk in markers.items()
                     if any(x["model"] == m for x in srows)]
    leg1 = ax.legend(handles=cond_handles, loc="upper right", fontsize=8, title="condition")
    ax.add_artist(leg1)
    ax.legend(handles=model_handles, loc="lower right", fontsize=8, title="model")
    ax.set_xlabel("tokens (k, ↓ better)")
    ax.set_ylabel("score (↑ better)")
    ax.set_title(f"{scenario}: tokens vs score  (faded+ring = invalid)")
    fig.tight_layout()
    fig.savefig(OUT / f"scatter_{scenario}.png", dpi=140)
    plt.close(fig)


def chart_3d(rows):
    pts = [r for r in rows if all(isinstance(r[k], (int, float)) for k in ("tokens", "score", "seconds"))]
    if not pts:
        return
    fig = plt.figure(figsize=(10, 8))
    ax = fig.add_subplot(111, projection="3d")
    for cond in COND_ORDER:
        cr = [r for r in pts if r["condition"] == cond]
        if not cr:
            continue
        ax.scatter([r["tokens"] / 1000 for r in cr], [r["score"] for r in cr],
                   [r["seconds"] for r in cr], color=COND_COLOR[cond], label=COND_LABEL[cond], s=45, alpha=0.85)
    ax.set_xlabel("tokens (k)")
    ax.set_ylabel("score")
    ax.set_zlabel("time (s)")
    ax.set_title("All proof runs: tokens × score × speed")
    ax.legend()
    fig.tight_layout()
    fig.savefig(OUT / "scatter3d_tokens_score_speed.png", dpi=140)
    plt.close(fig)


def build_master_table(rows, A):
    base = {(s, ag, m, e): st["score"] for (s, ag, m, e, c), st in A.items() if c == "no_brain"}
    cols = ["scenario", "agent", "model", "effort", "condition", "n", "valid",
            "score", "delta", "time_s", "tokens", "cost_usd", "mcp", "search"]
    table = []
    for (s, ag, m, e, c), st in sorted(A.items()):
        b = base.get((s, ag, m, e))
        delta = round(st["score"] - b, 1) if (b is not None and c != "no_brain") else ""
        table.append({"scenario": s, "agent": ag, "model": m, "effort": e, "condition": c,
                      "n": st["n"], "valid": f"{st['valid']}/{st['n']}", "score": round(st["score"], 1),
                      "delta": delta, "time_s": round(st["seconds"], 1), "tokens": int(st["tokens"]),
                      "cost_usd": round(st["cost"], 4) if st["cost"] else "", "mcp": round(st["mcp"], 1),
                      "search": round(st["search"], 1)})
    with (OUT / "master_table.csv").open("w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=cols); w.writeheader(); w.writerows(table)
    md = ["# Master results table (integrity-clean proof set)", "",
          "| " + " | ".join(cols) + " |", "|" + "---|" * len(cols)]
    for t in table:
        md.append("| " + " | ".join(str(t[c]) for c in cols) + " |")
    (OUT / "master_table.md").write_text("\n".join(md) + "\n")
    return table


def main():
    rows = collect()
    A = agg(rows)
    print(f"collected {len(rows)} integrity-clean records across {len(set(r['suite'] for r in rows))} suites")
    build_master_table(rows, A)

    claude_label = lambda m, e: model_short(m)
    codex_label = lambda m, e: f"{model_short(m)}-{e}"
    eff_rank = lambda e: ["low", "medium", "high", "xhigh"].index(e) if e in ["low", "medium", "high", "xhigh"] else 9

    for scn in sorted(set(r["scenario"] for r in rows)):
        # Claude: runners present for this scenario, ordered Haiku/Sonnet/Opus
        claude_runners = sorted({(r["model"], r["effort"]) for r in rows
                                 if r["agent"] == "claude" and r["scenario"] == scn},
                                key=lambda me: ["claude-haiku-4-5", "claude-sonnet-4-6", "claude-opus-4-8"].index(me[0])
                                if me[0] in ["claude-haiku-4-5", "claude-sonnet-4-6", "claude-opus-4-8"] else 9)
        if claude_runners:
            chart_family_scenario(rows, A, scn, "claude", claude_runners, claude_label,
                                  f"claude_{scn}.png", f"Claude — {scn}")
        # Codex: runners (model+effort) present for this scenario
        codex_runners = sorted({(r["model"], r["effort"]) for r in rows
                               if r["agent"] == "codex" and r["scenario"] == scn},
                              key=lambda me: (me[0], eff_rank(me[1])))
        if codex_runners:
            chart_family_scenario(rows, A, scn, "codex", codex_runners, codex_label,
                                  f"codex_{scn}.png", f"Codex — {scn}")
        chart_scatter(rows, scn)
    chart_3d(rows)
    print(f"charts + master_table written to {OUT}")
    # console accuracy check
    for (s, ag, m, e, c), st in sorted(A.items()):
        if c != "no_brain":
            b = next((v["score"] for (s2, a2, m2, e2, c2), v in A.items()
                      if (s2, a2, m2, e2, c2) == (s, ag, m, e, "no_brain")), None)
            d = f"{st['score']-b:+.1f}" if b is not None else "n/a"
            print(f"  {s:20} {ag:5} {model_short(m):9} {e:6} {c:16} n={st['n']} valid={st['valid']}/{st['n']} "
                  f"score={st['score']:.1f} Δ={d} time={st['seconds']:.0f}s tok={st['tokens']/1000:.0f}k mcp={st['mcp']:.1f}")


if __name__ == "__main__":
    main()
