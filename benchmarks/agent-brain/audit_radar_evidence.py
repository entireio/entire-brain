#!/usr/bin/env python3
"""Audit Regression Radar benchmark summaries for proof quality.

This is intentionally narrower than audit_codex.py.  It answers the release
question Thomas actually raised for Radar-shaped evidence: did the brain/Radar
arm prove something the no-brain arm could not already solve, or did the task
saturate and merely look busy?
"""
from __future__ import annotations

import argparse
import fnmatch
import json
import pathlib
import sys
from collections import Counter
from typing import Any

BENCH = pathlib.Path(__file__).resolve().parent
RESULTS = BENCH / "results"
RADAR_SCOPES = {"mcp_radar_location_only", "mcp_workspace_radar_location_only"}


def as_float(value: Any) -> float | None:
    if isinstance(value, (int, float)):
        return float(value)
    return None


def suite_matches(name: str, globs: list[str]) -> bool:
    return any(fnmatch.fnmatch(name, pattern) for pattern in globs)


def load_summary(path: pathlib.Path) -> dict[str, Any]:
    try:
        data = json.loads(path.read_text())
    except (OSError, json.JSONDecodeError):
        return {}
    return data if isinstance(data, dict) else {}


def iter_radar_comparisons(results: pathlib.Path, suite_globs: list[str]) -> list[dict[str, Any]]:
    rows: list[dict[str, Any]] = []
    if not results.exists():
        return rows
    for summary_path in sorted(results.glob("*/summary.json")):
        suite = summary_path.parent.name
        if not suite_matches(suite, suite_globs):
            continue
        summary = load_summary(summary_path)
        for comp in summary.get("comparisons") or []:
            if not isinstance(comp, dict):
                continue
            if comp.get("delivery_scope") not in RADAR_SCOPES:
                continue
            row = dict(comp)
            row["suite"] = suite
            rows.append(row)
    return rows


def radar_status(comp: dict[str, Any]) -> dict[str, Any]:
    baseline_pass = as_float(comp.get("pass_rate_baseline"))
    condition_pass = as_float(comp.get("pass_rate_condition"))
    if baseline_pass is None:
        baseline_pass = as_float(comp.get("success_rate_baseline"))
    if condition_pass is None:
        condition_pass = as_float(comp.get("success_rate_condition"))
    n_condition = int(comp.get("n_condition") or 0)
    n_baseline = int(comp.get("n_baseline") or 0)
    stability = comp.get("stability") if isinstance(comp.get("stability"), dict) else {}
    stability_tag = stability.get("tag")
    proof_ready = bool(comp.get("proof_ready")) and stability_tag == "brain_positive_stable"

    reasons: list[str] = []
    baseline_headroom = baseline_pass is not None and baseline_pass < 1.0
    brain_clean = condition_pass == 1.0
    repeated = n_condition >= 4 and n_baseline >= 4
    saturated = baseline_pass == 1.0 and condition_pass == 1.0

    if proof_ready:
        status = "proof-ready"
        reasons.append("stable repeated brain-positive Radar comparison")
    elif saturated:
        status = "saturated"
        reasons.append("no-brain and Radar arms both passed, so there is no correctness headroom")
    elif baseline_pass is None or condition_pass is None:
        status = "missing-pass-rate"
        reasons.append("summary is missing pass_rate/success_rate fields")
    elif not brain_clean:
        status = "brain-not-clean"
        reasons.append("Radar arm did not pass cleanly")
    elif baseline_headroom and not repeated:
        status = "promotable-pilot"
        reasons.append("no-brain left headroom and Radar passed; promote only with repeated audit-clean runs")
    elif baseline_headroom:
        status = "has-headroom"
        reasons.append("baseline has headroom, but the comparison is not proof-ready")
    else:
        status = "no-headroom"
        reasons.append("baseline leaves no measured headroom")

    recommendation = {
        "proof-ready": "retain as release-candidate evidence after audit_codex passes",
        "promotable-pilot": "run the committed panel at n>=4 and retain only if audit_codex marks it proof-ready",
        "has-headroom": "inspect the failed proof gate before spending more runs",
        "saturated": "do not spend more repetitions on this task; screen a harder target/source history",
        "brain-not-clean": "fix Radar delivery or task setup before promotion",
        "missing-pass-rate": "regenerate the suite with the current benchmark summarizer",
        "no-headroom": "screen a harder target/source history",
    }[status]

    return {
        "status": status,
        "proof_ready": proof_ready,
        "promotable": status in {"proof-ready", "promotable-pilot"},
        "baseline_headroom": baseline_headroom,
        "brain_clean": brain_clean,
        "repeated": repeated,
        "saturated": saturated,
        "pass_rate_baseline": baseline_pass,
        "pass_rate_condition": condition_pass,
        "stability_tag": stability_tag,
        "reasons": reasons,
        "recommendation": recommendation,
    }


def build_report(results: pathlib.Path, suite_globs: list[str]) -> dict[str, Any]:
    comparisons: list[dict[str, Any]] = []
    status_counts: Counter[str] = Counter()
    proof_ready = 0
    promotable = 0
    for comp in iter_radar_comparisons(results, suite_globs):
        status = radar_status(comp)
        row = {
            "suite": comp.get("suite"),
            "task_id": comp.get("task_id"),
            "runner": comp.get("runner"),
            "condition": comp.get("condition"),
            "delivery_scope": comp.get("delivery_scope"),
            "n_condition": comp.get("n_condition"),
            "n_baseline": comp.get("n_baseline"),
            "verdict": comp.get("verdict"),
            "score_delta": comp.get("delta"),
            "mean_total_tokens_condition": comp.get("mean_total_tokens_condition"),
            "mean_total_tokens_baseline": comp.get("mean_total_tokens_baseline"),
            "mean_search_calls_condition": comp.get("mean_search_calls_condition"),
            "mean_search_calls_baseline": comp.get("mean_search_calls_baseline"),
            "radar_gate": status,
        }
        comparisons.append(row)
        status_counts[status["status"]] += 1
        if status["proof_ready"]:
            proof_ready += 1
        if status["promotable"]:
            promotable += 1
    return {
        "schema": 1,
        "results": str(results),
        "suite_globs": suite_globs,
        "totals": {
            "radar_comparisons": len(comparisons),
            "proof_ready": proof_ready,
            "promotable_or_proof": promotable,
            "status_counts": dict(sorted(status_counts.items())),
        },
        "comparisons": comparisons,
    }


def render_markdown(report: dict[str, Any]) -> str:
    totals = report["totals"]
    lines = [
        "# Regression Radar Evidence Audit",
        "",
        f"- Radar comparisons: **{totals['radar_comparisons']}**",
        f"- Proof-ready Radar comparisons: **{totals['proof_ready']}**",
        f"- Promotable pilots or proof: **{totals['promotable_or_proof']}**",
    ]
    if totals["status_counts"]:
        status_text = ", ".join(f"`{k}`={v}" for k, v in totals["status_counts"].items())
        lines.append(f"- Status counts: {status_text}")
    lines.extend(["", "| Suite | Task | Runner | Scope | Pass no-brain -> Radar | Status | Recommendation |", "|---|---|---|---|---:|---|---|"])
    for comp in report["comparisons"]:
        gate = comp["radar_gate"]
        base = gate.get("pass_rate_baseline")
        cond = gate.get("pass_rate_condition")
        pass_text = "n/a"
        if isinstance(base, (int, float)) and isinstance(cond, (int, float)):
            pass_text = f"{base:.2f} -> {cond:.2f}"
        lines.append(
            "| "
            + " | ".join(
                [
                    str(comp.get("suite") or ""),
                    str(comp.get("task_id") or ""),
                    str(comp.get("runner") or ""),
                    str(comp.get("delivery_scope") or ""),
                    pass_text,
                    str(gate["status"]),
                    str(gate["recommendation"]),
                ]
            )
            + " |"
        )
    if not report["comparisons"]:
        lines.append("| _none_ | | | | | | No Radar summaries matched the selected suites. |")
    return "\n".join(lines) + "\n"


def write_report(report: dict[str, Any], out_dir: pathlib.Path) -> pathlib.Path:
    out_dir.mkdir(parents=True, exist_ok=True)
    json_path = out_dir / "radar-candidate-report.json"
    json_path.write_text(json.dumps(report, indent=2, sort_keys=True))
    (out_dir / "radar-candidate-report.md").write_text(render_markdown(report))
    return json_path


def parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Audit Regression Radar benchmark evidence for saturation/headroom.")
    parser.add_argument("--results", type=pathlib.Path, default=RESULTS, help="Directory containing benchmark suites")
    parser.add_argument("--suite-glob", action="append", default=None, help="Only audit suites whose directory name matches this glob; repeatable")
    parser.add_argument("--out-dir", type=pathlib.Path, default=None, help="Directory for radar-candidate-report.{json,md}; defaults to --results")
    parser.add_argument("--fail-when-no-promotable", action="store_true", help="Exit nonzero unless at least one Radar comparison is proof-ready or promotable")
    parser.add_argument("--fail-when-no-proof", action="store_true", help="Exit nonzero unless at least one Radar comparison is proof-ready")
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(sys.argv[1:] if argv is None else argv)
    suite_globs = args.suite_glob or ["pilot-radar-*", "release-candidate-*"]
    results = args.results.resolve()
    out_dir = (args.out_dir or results).resolve()
    report = build_report(results, suite_globs)
    json_path = write_report(report, out_dir)
    print(render_markdown(report).split("\n\n", 1)[0])
    print(f"\nWrote {json_path} and radar-candidate-report.md")
    totals = report["totals"]
    if args.fail_when_no_proof and int(totals["proof_ready"]) <= 0:
        print("Radar evidence has no proof-ready comparison.", file=sys.stderr)
        return 1
    if args.fail_when_no_promotable and int(totals["promotable_or_proof"]) <= 0:
        print("Radar evidence has no promotable pilot or proof-ready comparison.", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
