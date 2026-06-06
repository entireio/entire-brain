#!/usr/bin/env python3
"""Generate honest evidence graphs for the diff-less reviewer (Regression Radar) PR.

Reads the radar A/B suites and emits PNGs + a markdown table. "brain_called" is RE-DERIVED from each
run's agent.stdout (counting mcp__entire_brain__brain_regressions tool_use), so it is correct even for
the batches recorded before the run.py parser fix (which had scored those calls as 0).

No network. Reads only local result records.
"""
import glob
import importlib.util
import json
import os
import statistics
import sys

BASE = os.path.join(os.path.dirname(__file__), "results")
OUT = os.path.join(BASE, "radar-report")
os.makedirs(OUT, exist_ok=True)

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt

# Reuse run.py's authoritative activity parser so "brain_called" is correct for BOTH claude (tool_use
# events) and codex (mcp_tool_call events) — a naive string match misses codex's representation.
_spec = importlib.util.spec_from_file_location("rb_graphs", os.path.join(os.path.dirname(__file__), "run.py"))
_rb = importlib.util.module_from_spec(_spec)
sys.modules["rb_graphs"] = _rb
try:
    _spec.loader.exec_module(_rb)
except SystemExit:
    pass


def brain_called(run_dir: str) -> bool:
    so = os.path.join(run_dir, "agent.stdout")
    if not os.path.exists(so):
        return False
    try:
        stdout = open(so, errors="ignore").read()
    except OSError:
        return False
    act = _rb.extract_agent_activity(stdout, "")
    return any(str(n).endswith("brain_regressions") for n in act.get("mcp_tool_names", []))


def load_suite(pattern: str):
    """Return list of dicts {model, arm, ok, tokens, brain_called} for matching run dirs."""
    rows = []
    for rec in glob.glob(os.path.join(BASE, pattern, "*", "record.json")):
        d = json.load(open(rec))
        if d.get("agent_info", {}).get("returncode", 0) != 0:
            continue
        det = d.get("score", {}).get("details", {})
        rows.append(
            {
                "suite": os.path.basename(os.path.dirname(os.path.dirname(rec))),
                "run": os.path.basename(os.path.dirname(rec)),
                "cond": d.get("condition"),
                "ok": bool(d.get("validation", {}).get("ok")),
                "tokens": det.get("total_tokens") or 0,
                "brain": brain_called(os.path.dirname(rec)),
            }
        )
    return rows


def median(vals):
    vals = [v for v in vals if v]
    return int(statistics.median(vals)) if vals else 0


# ---- Graph 1: fair location-only radar vs no_brain, tokens, 5 models (radarfix, n=1) ----
def graph_tokens_fair():
    models = ["opus", "sonnet", "haiku", "g55", "mini"]
    labels = ["opus", "sonnet", "haiku", "gpt-5.5", "gpt-5.4-mini"]
    nb, rd = [], []
    for m in models:
        base = load_suite(f"radarfix-{m}-base")
        loc = load_suite(f"radarfix-{m}-loc")
        nb.append(median([r["tokens"] for r in base if r["cond"] == "no_brain"]) / 1000)
        rd.append(median([r["tokens"] for r in loc]) / 1000)
    x = range(len(models))
    w = 0.38
    fig, ax = plt.subplots(figsize=(9, 4.5))
    ax.bar([i - w / 2 for i in x], nb, w, label="no_brain", color="#b0b0b0")
    ax.bar([i + w / 2 for i in x], rd, w, label="radar (location-only, fair)", color="#2b7bba")
    ax.set_xticks(list(x))
    ax.set_xticklabels(labels)
    ax.set_ylabel("median tokens (thousands)")
    ax.set_title("Diff-less radar vs no_brain — tokens (review task, fair location-only, n=1, directional)")
    ax.legend()
    for i in x:
        if nb[i] and rd[i]:
            ax.text(i + w / 2, rd[i], f"-{round(100*(nb[i]-rd[i])/nb[i])}%", ha="center", va="bottom", fontsize=8)
    fig.tight_layout()
    fig.savefig(os.path.join(OUT, "radar_tokens_fair.png"), dpi=130)
    plt.close(fig)
    return list(zip(labels, nb, rd))


# ---- Graph 2: fresh validation (radar-thomas, n=3): tokens + brain-called ----
def graph_fresh():
    rows = load_suite("radar-thomas")
    cells = {}
    for r in rows:
        run = r["run"]
        model = "gpt-5.5" if "gpt-5.5" in run else ("opus" if "opus" in run else "mini")
        arm = "radar" if r["cond"] == "mcp_history" else "no_brain"
        cells.setdefault((model, arm), []).append(r)
    models = [m for m in ("opus", "gpt-5.5") if (m, "radar") in cells and (m, "no_brain") in cells]
    nb = [median([r["tokens"] for r in cells[(m, "no_brain")]]) / 1000 for m in models]
    rd = [median([r["tokens"] for r in cells[(m, "radar")]]) / 1000 for m in models]
    bc = [(sum(1 for r in cells[(m, "radar")] if r["brain"]), len(cells[(m, "radar")])) for m in models]
    x = range(len(models))
    w = 0.38
    fig, ax = plt.subplots(figsize=(8, 4.5))
    ax.bar([i - w / 2 for i in x], nb, w, label="no_brain (brain 0/n)", color="#b0b0b0")
    ax.bar([i + w / 2 for i in x], rd, w, label="radar (location-only)", color="#2b7bba")
    ax.set_xticks(list(x))
    ax.set_xticklabels(models)
    ax.set_ylabel("median tokens (thousands)")
    ax.set_title("Fresh validation (n=3, post parser-fix): tokens, with brain-call verification")
    ax.legend()
    for i, m in enumerate(models):
        ax.text(i + w / 2, rd[i], f"-{round(100*(nb[i]-rd[i])/nb[i])}%\nbrain {bc[i][0]}/{bc[i][1]}", ha="center", va="bottom", fontsize=8)
    fig.tight_layout()
    fig.savefig(os.path.join(OUT, "radar_fresh_validation.png"), dpi=130)
    plt.close(fig)
    return list(zip(models, nb, rd, bc))


# ---- Graph 3: n=8 pass-rate + tokens (radarab, answer-assisted upper bound) ----
def graph_n8():
    out = []
    fig, (ax1, ax2) = plt.subplots(1, 2, figsize=(11, 4.5))
    models = ["g55", "mini"]
    labels = ["gpt-5.5", "gpt-5.4-mini"]
    nb_pass, rd_pass, nb_tok, rd_tok = [], [], [], []
    for m in models:
        base = [r for r in load_suite(f"radarab-{m}-base") if r["cond"] == "no_brain"]
        radar = load_suite(f"radarab-{m}-radar")
        nb_pass.append(100 * sum(1 for r in base if r["ok"]) / max(1, len(base)))
        rd_pass.append(100 * sum(1 for r in radar if r["ok"]) / max(1, len(radar)))
        nb_tok.append(median([r["tokens"] for r in base]) / 1000)
        rd_tok.append(median([r["tokens"] for r in radar]) / 1000)
        out.append((m, len(base), len(radar)))
    x = range(len(models))
    w = 0.38
    ax1.bar([i - w / 2 for i in x], nb_pass, w, label="no_brain", color="#b0b0b0")
    ax1.bar([i + w / 2 for i in x], rd_pass, w, label="radar", color="#2b7bba")
    ax1.set_xticks(list(x)); ax1.set_xticklabels(labels); ax1.set_ylabel("pass-rate (%)"); ax1.set_ylim(0, 105)
    ax1.set_title("pass-rate (n=8)"); ax1.legend()
    ax2.bar([i - w / 2 for i in x], nb_tok, w, label="no_brain", color="#b0b0b0")
    ax2.bar([i + w / 2 for i in x], rd_tok, w, label="radar", color="#2b7bba")
    ax2.set_xticks(list(x)); ax2.set_xticklabels(labels); ax2.set_ylabel("median tokens (thousands)")
    ax2.set_title("tokens (n=8)"); ax2.legend()
    fig.suptitle("n=8 radar vs no_brain — answer-assisted (UPPER BOUND, not a fair detection measure)")
    fig.tight_layout()
    fig.savefig(os.path.join(OUT, "radar_n8_passrate_tokens.png"), dpi=130)
    plt.close(fig)
    return out


if __name__ == "__main__":
    g1 = graph_tokens_fair()
    g2 = graph_fresh()
    g3 = graph_n8()
    lines = ["# Diff-less reviewer — evidence (auto-generated)\n"]
    lines.append("## Fair location-only radar vs no_brain, tokens (radarfix, n=1, directional)\n")
    lines.append("| model | no_brain (k tok) | radar-loc (k tok) | delta |")
    lines.append("|---|---|---|---|")
    for label, nb, rd in g1:
        d = f"{round(100*(rd-nb)/nb):+d}%" if nb else "n/a"  # negative = fewer tokens
        lines.append(f"| {label} | {nb:.0f} | {rd:.0f} | {d} |")
    lines.append("\n## Fresh validation (radar-thomas, n=3, post parser-fix)\n")
    lines.append("| model | no_brain (k) | radar (k) | delta | brain called |")
    lines.append("|---|---|---|---|---|")
    for m, nb, rd, bc in g2:
        d = f"{round(100*(rd-nb)/nb):+d}%" if nb else "n/a"  # negative = fewer tokens
        lines.append(f"| {m} | {nb:.0f} | {rd:.0f} | {d} | {bc[0]}/{bc[1]} |")
    lines.append("\nGraphs: radar_tokens_fair.png, radar_fresh_validation.png, radar_n8_passrate_tokens.png")
    open(os.path.join(OUT, "EVIDENCE.md"), "w").write("\n".join(lines) + "\n")
    print("wrote graphs + EVIDENCE.md to", OUT)
    for f in sorted(os.listdir(OUT)):
        print(" ", f)
