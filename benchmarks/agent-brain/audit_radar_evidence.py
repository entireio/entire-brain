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


def load_records(path: pathlib.Path) -> list[dict[str, Any]]:
    records: list[dict[str, Any]] = []
    try:
        lines = path.read_text().splitlines()
    except OSError:
        return records
    for line in lines:
        if not line.strip():
            continue
        try:
            record = json.loads(line)
        except json.JSONDecodeError:
            continue
        if isinstance(record, dict):
            records.append(record)
    return records


def iter_matching_suite_dirs(results: pathlib.Path, suite_globs: list[str]) -> list[pathlib.Path]:
    if not results.exists():
        return []
    out: list[pathlib.Path] = []
    for suite_dir in sorted(path for path in results.iterdir() if path.is_dir()):
        if suite_matches(suite_dir.name, suite_globs):
            if (suite_dir / "summary.json").exists() or (suite_dir / "records.ndjson").exists():
                out.append(suite_dir)
    return out


def load_codex_audit(path: pathlib.Path | None) -> dict[str, Any] | None:
    if path is None:
        return None
    try:
        data = json.loads(path.read_text())
    except (OSError, json.JSONDecodeError):
        return {"suites": {}}
    return data if isinstance(data, dict) else {"suites": {}}


def display_path(path: pathlib.Path) -> str:
    resolved = path.resolve()
    try:
        return str(resolved.relative_to(pathlib.Path.cwd().resolve()))
    except ValueError:
        return str(resolved)


def codex_audit_radar_keys(report: dict[str, Any] | None) -> set[tuple[str, str, str, str, str]] | None:
    if report is None:
        return None
    return set(codex_audit_radar_backing(report))


def validation_pass_rate(records: list[dict[str, Any]]) -> float | None:
    if not records:
        return None
    return sum(1 for record in records if record.get("valid") is True) / len(records)


def close_float(left: Any, right: Any, *, tolerance: float = 0.000001) -> bool:
    left_float = as_float(left)
    right_float = as_float(right)
    if left_float is None or right_float is None:
        return left_float is None and right_float is None
    return abs(left_float - right_float) <= tolerance


def codex_audit_radar_backing(report: dict[str, Any] | None) -> dict[tuple[str, str, str, str, str], dict[str, Any]]:
    if report is None:
        return {}
    out: dict[tuple[str, str, str, str, str], dict[str, Any]] = {}
    suites = report.get("suites") if isinstance(report.get("suites"), dict) else {}
    for suite, suite_data in suites.items():
        comparisons = suite_data.get("comparisons") if isinstance(suite_data, dict) else []
        records = suite_data.get("records") if isinstance(suite_data, dict) else []
        records = records if isinstance(records, list) else []
        for comp in comparisons or []:
            if not isinstance(comp, dict):
                continue
            if comp.get("proof_scope") not in RADAR_SCOPES:
                continue
            backing = comp.get("record_backing") if isinstance(comp.get("record_backing"), dict) else {}
            if not backing:
                continue
            key = (
                str(suite),
                str(comp.get("task") or ""),
                str(comp.get("runner") or ""),
                str(comp.get("condition") or ""),
                str(comp.get("delivery_scope") or ""),
            )
            condition_all_records = [
                record for record in records
                if isinstance(record, dict)
                and record.get("task_id") == comp.get("task")
                and record.get("runner") == comp.get("runner")
                and record.get("condition") == comp.get("condition")
                and record.get("delivery_scope") == comp.get("delivery_scope")
            ]
            condition_records = [
                record for record in condition_all_records
                if isinstance(record, dict)
                and record.get("pass") is True
                and record.get("mcp_verified") is True
            ]
            condition_named_tool_records = [
                record for record in condition_records
                if record.get("mcp_named_tool_verified") is True
            ]
            condition_completed_named_tool_records = [
                record for record in condition_records
                if record.get("mcp_named_tool_completed") is True
            ]
            condition_attempted_named_tool_records = [
                record for record in condition_all_records
                if record.get("mcp_named_tool_verified") is True
            ]
            condition_attempted_completed_named_tool_records = [
                record for record in condition_all_records
                if record.get("mcp_named_tool_completed") is True
            ]
            condition_flag_counts = Counter(
                flag
                for record in condition_all_records
                for flag in (record.get("flags") if isinstance(record.get("flags"), list) else [])
                if isinstance(flag, str)
            )
            baseline_records = [
                record for record in records
                if isinstance(record, dict)
                and record.get("pass") is True
                and record.get("task_id") == comp.get("task")
                and record.get("runner") == comp.get("runner")
                and record.get("condition") == "no_brain"
            ]
            baseline_all_records = [
                record for record in records
                if isinstance(record, dict)
                and record.get("task_id") == comp.get("task")
                and record.get("runner") == comp.get("runner")
                and record.get("condition") == "no_brain"
            ]
            out[key] = {
                "codex_audit_backed": True,
                "codex_audit_comparison_pass": bool(comp.get("pass")),
                "codex_audit_comparison_proof_ready": bool(comp.get("proof_ready")),
                "condition_mcp_verified_ok": backing.get("condition_mcp_verified_ok"),
                "condition_mcp_named_tool_verified_ok": backing.get("condition_mcp_named_tool_verified_ok"),
                "condition_mcp_named_tool_completed_ok": backing.get("condition_mcp_named_tool_completed_ok"),
                "condition_records": len(condition_records),
                "condition_named_tool_records": len(condition_named_tool_records),
                "condition_completed_named_tool_records": len(condition_completed_named_tool_records),
                "condition_attempted_records": len(condition_all_records),
                "condition_attempted_named_tool_records": len(condition_attempted_named_tool_records),
                "condition_attempted_completed_named_tool_records": len(condition_attempted_completed_named_tool_records),
                "condition_flag_counts": dict(sorted(condition_flag_counts.items())),
                "baseline_records": len(baseline_records),
                "baseline_attempted_records": len(baseline_all_records),
                "condition_pass_rate": validation_pass_rate(condition_records),
                "condition_attempted_pass_rate": validation_pass_rate(condition_all_records),
                "baseline_pass_rate": validation_pass_rate(baseline_records),
                "baseline_attempted_pass_rate": validation_pass_rate(baseline_all_records),
            }
    return out


def audit_summary_consistency(comp: dict[str, Any], backing: dict[str, Any]) -> list[str]:
    mismatches: list[str] = []
    condition_count = backing.get("condition_attempted_records", backing.get("condition_records"))
    baseline_count = backing.get("baseline_attempted_records", backing.get("baseline_records"))
    condition_rate = backing.get("condition_attempted_pass_rate", backing.get("condition_pass_rate"))
    baseline_rate = backing.get("baseline_attempted_pass_rate", backing.get("baseline_pass_rate"))
    if int(comp.get("n_condition") or 0) != int(condition_count or 0):
        mismatches.append(
            f"n_condition summary={comp.get('n_condition')} records={condition_count}"
        )
    if int(comp.get("n_baseline") or 0) != int(baseline_count or 0):
        mismatches.append(
            f"n_baseline summary={comp.get('n_baseline')} records={baseline_count}"
        )
    if not close_float(comp.get("pass_rate_condition"), condition_rate):
        mismatches.append(
            f"pass_rate_condition summary={comp.get('pass_rate_condition')} records={condition_rate}"
        )
    if not close_float(comp.get("pass_rate_baseline"), baseline_rate):
        mismatches.append(
            f"pass_rate_baseline summary={comp.get('pass_rate_baseline')} records={baseline_rate}"
        )
    return mismatches


def iter_radar_comparisons(results: pathlib.Path, suite_globs: list[str]) -> list[dict[str, Any]]:
    rows: list[dict[str, Any]] = []
    for suite_dir in iter_matching_suite_dirs(results, suite_globs):
        suite = suite_dir.name
        summary_path = suite_dir / "summary.json"
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


def early_stopped_no_brain_rows(results: pathlib.Path, suite_globs: list[str], suites_with_radar: set[str]) -> list[dict[str, Any]]:
    rows: list[dict[str, Any]] = []
    for suite_dir in iter_matching_suite_dirs(results, suite_globs):
        suite = suite_dir.name
        if suite in suites_with_radar:
            continue
        for record in load_records(suite_dir / "records.ndjson"):
            if record.get("condition") != "no_brain":
                continue
            score_obj = record.get("score") if isinstance(record.get("score"), dict) else {}
            score = as_float(score_obj.get("total"))
            provenance = record.get("provenance") if isinstance(record.get("provenance"), dict) else {}
            run_config = provenance.get("run_config") if isinstance(provenance.get("run_config"), dict) else {}
            threshold = as_float(run_config.get("stop_after_no_brain_score"))
            if score is None or threshold is None or score <= threshold:
                continue
            runner = record.get("runner")
            if isinstance(runner, dict):
                runner = runner.get("id")
            gate = {
                "status": "no-brain-too-easy",
                "proof_ready": False,
                "promotable": False,
                "baseline_headroom": False,
                "brain_clean": None,
                "repeated": False,
                "saturated": True,
                "pass_rate_baseline": 1.0 if record.get("valid") is True else None,
                "pass_rate_condition": None,
                "baseline_score": score,
                "stop_after_no_brain_score": threshold,
                "stability_tag": "early_stopped",
                "reasons": [f"no-brain pilot score {score:g} exceeded early-stop threshold {threshold:g}"],
                "recommendation": "do not spend Radar repetitions on this task; screen a harder target/source history",
            }
            rows.append({
                "suite": suite,
                "task_id": record.get("task_id"),
                "runner": runner,
                "condition": "no_brain",
                "delivery_scope": "early_stop",
                "n_condition": 0,
                "n_baseline": 1,
                "verdict": "early_stopped",
                "score_delta": None,
                "mean_total_tokens_condition": None,
                "mean_total_tokens_baseline": None,
                "mean_search_calls_condition": None,
                "mean_search_calls_baseline": None,
                "radar_gate": gate,
            })
            break
    return rows


def incomplete_radar_suite_rows(results: pathlib.Path, suite_globs: list[str], suites_with_radar: set[str]) -> list[dict[str, Any]]:
    rows: list[dict[str, Any]] = []
    for suite_dir in iter_matching_suite_dirs(results, suite_globs):
        suite = suite_dir.name
        if suite in suites_with_radar or (suite_dir / "summary.json").exists():
            continue
        records = load_records(suite_dir / "records.ndjson")
        if not records:
            continue
        record = next((r for r in records if r.get("condition") != "prep"), records[0])
        runner = record.get("runner")
        if isinstance(runner, dict):
            runner = runner.get("id")
        conditions = sorted({str(r.get("condition")) for r in records if r.get("condition")})
        gate = {
            "status": "incomplete-suite",
            "proof_ready": False,
            "promotable": False,
            "baseline_headroom": None,
            "brain_clean": None,
            "repeated": False,
            "saturated": False,
            "pass_rate_baseline": None,
            "pass_rate_condition": None,
            "stability_tag": "incomplete",
            "reasons": [f"suite has {len(records)} record(s) but no summary.json"],
            "recommendation": "rerun or regenerate the suite summary before citing Radar evidence",
        }
        rows.append({
            "suite": suite,
            "task_id": record.get("task_id"),
            "runner": runner,
            "condition": ",".join(conditions),
            "delivery_scope": "incomplete",
            "n_condition": sum(1 for r in records if r.get("condition") != "no_brain"),
            "n_baseline": sum(1 for r in records if r.get("condition") == "no_brain"),
            "verdict": "incomplete",
            "score_delta": None,
            "mean_total_tokens_condition": None,
            "mean_total_tokens_baseline": None,
            "mean_search_calls_condition": None,
            "mean_search_calls_baseline": None,
            "radar_gate": gate,
        })
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
    reported_proof_ready = bool(comp.get("proof_ready")) and stability_tag == "brain_positive_stable"

    reasons: list[str] = []
    baseline_headroom = baseline_pass is not None and baseline_pass < 1.0
    brain_clean = condition_pass == 1.0
    repeated = n_condition >= 4 and n_baseline >= 4
    saturated = baseline_pass == 1.0 and condition_pass == 1.0
    proof_ready = reported_proof_ready and repeated and brain_clean and baseline_headroom

    if saturated:
        status = "saturated"
        reasons.append("no-brain and Radar arms both passed, so there is no correctness headroom")
    elif proof_ready:
        status = "proof-ready"
        reasons.append("stable repeated brain-positive Radar comparison with baseline headroom")
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


def build_report(results: pathlib.Path, suite_globs: list[str], codex_audit: dict[str, Any] | None = None) -> dict[str, Any]:
    comparisons: list[dict[str, Any]] = []
    status_counts: Counter[str] = Counter()
    proof_ready = 0
    promotable = 0
    audit_backing = codex_audit_radar_backing(codex_audit) if codex_audit is not None else None
    suites_with_radar: set[str] = set()
    for comp in iter_radar_comparisons(results, suite_globs):
        suites_with_radar.add(str(comp.get("suite") or ""))
        status = radar_status(comp)
        if audit_backing is not None:
            key = (
                str(comp.get("suite") or ""),
                str(comp.get("task_id") or ""),
                str(comp.get("runner") or ""),
                str(comp.get("condition") or ""),
                str(comp.get("delivery_scope") or ""),
            )
            backing = audit_backing.get(key)
            backed = backing is not None
            status["codex_audit_backed"] = backed
            proof_backed = (
                backing is not None
                and backing.get("codex_audit_comparison_pass") is True
                and backing.get("condition_mcp_verified_ok") is True
                and backing.get("condition_mcp_named_tool_verified_ok") is True
                and backing.get("condition_mcp_named_tool_completed_ok") is True
            )
            if backing is not None:
                mismatches = audit_summary_consistency(comp, backing)
                backing = dict(backing)
                backing["summary_consistency_ok"] = not mismatches
                backing["summary_consistency_mismatches"] = mismatches
                status["codex_audit_record_backing"] = backing
                if status["proof_ready"] and mismatches:
                    status["status"] = "audit-mismatch"
                    status["proof_ready"] = False
                    status["promotable"] = False
                    status["reasons"].append("Radar summary pass/count fields do not match audited record rows")
                    status["reasons"].extend(mismatches)
                    status["recommendation"] = "regenerate summary.json from retained records before citing Radar proof"
            if status["proof_ready"] and not proof_backed:
                status["status"] = "audit-backing-gap" if backed else "audit-missing"
                status["proof_ready"] = False
                status["promotable"] = False
                if backed:
                    status["reasons"].append("matching Radar records exist, but they are not audit-clean with required named-tool MCP backing")
                else:
                    status["reasons"].append("matching Radar comparison is missing from audit_codex output")
                status["recommendation"] = "retain audit-clean records with MCP named-tool backing before citing Radar proof"
            elif status["promotable"] and not proof_backed:
                status["status"] = "promotable-audit-gap" if backed else "promotable-audit-missing"
                status["proof_ready"] = False
                status["promotable"] = False
                if backed:
                    status["reasons"].append("pilot has Radar headroom, but matching records are not audit-clean with required named-tool MCP backing")
                else:
                    status["reasons"].append("pilot has Radar headroom, but the comparison is missing from audit_codex output")
                status["recommendation"] = "rerun the pilot with audit-clean MCP backing before promotion"
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
    for row in early_stopped_no_brain_rows(results, suite_globs, suites_with_radar):
        comparisons.append(row)
        status_counts[row["radar_gate"]["status"]] += 1
    for row in incomplete_radar_suite_rows(results, suite_globs, suites_with_radar):
        comparisons.append(row)
        status_counts[row["radar_gate"]["status"]] += 1
    return {
        "schema": 1,
        "results": display_path(results),
        "suite_globs": suite_globs,
        "codex_audit_required": audit_backing is not None,
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
    lines.extend(["", "| Suite | Task | Runner | Scope | Pass no-brain -> Radar | MCP named/completed | Status | Recommendation |", "|---|---|---|---|---:|---:|---|---|"])
    for comp in report["comparisons"]:
        gate = comp["radar_gate"]
        base = gate.get("pass_rate_baseline")
        cond = gate.get("pass_rate_condition")
        pass_text = "n/a"
        if gate.get("status") == "no-brain-too-easy":
            pass_text = f"score {float(gate.get('baseline_score') or 0):.0f} > {float(gate.get('stop_after_no_brain_score') or 0):.0f}"
        elif isinstance(base, (int, float)) and isinstance(cond, (int, float)):
            pass_text = f"{base:.2f} -> {cond:.2f}"
        backing = gate.get("codex_audit_record_backing") if isinstance(gate.get("codex_audit_record_backing"), dict) else {}
        mcp_backing = "n/a"
        if backing:
            clean_named = int(backing.get("condition_named_tool_records") or 0)
            clean_completed = int(backing.get("condition_completed_named_tool_records") or 0)
            attempted_named = int(backing.get("condition_attempted_named_tool_records") or 0)
            attempted_completed = int(backing.get("condition_attempted_completed_named_tool_records") or 0)
            mcp_backing = f"{clean_named}/{clean_completed}"
            if attempted_named != clean_named or attempted_completed != clean_completed:
                mcp_backing += f" clean; {attempted_named}/{attempted_completed} attempted"
                flag_counts = backing.get("condition_flag_counts") if isinstance(backing.get("condition_flag_counts"), dict) else {}
                if any("missing_include_deletions" in str(flag) for flag in flag_counts):
                    mcp_backing += " (wrong args)"
                elif any("missing_radar_include_deletions_policy" in str(flag) for flag in flag_counts):
                    mcp_backing += " (missing policy)"
        lines.append(
            "| "
            + " | ".join(
                [
                    str(comp.get("suite") or ""),
                    str(comp.get("task_id") or ""),
                    str(comp.get("runner") or ""),
                    str(comp.get("delivery_scope") or ""),
                    pass_text,
                    mcp_backing,
                    str(gate["status"]),
                    str(gate["recommendation"]),
                ]
            )
            + " |"
        )
    if not report["comparisons"]:
        lines.append("| _none_ | | | | | | | No Radar summaries matched the selected suites. |")
    return "\n".join(lines) + "\n"


def has_release_candidate_comparison(report: dict[str, Any]) -> bool:
    return any(
        str(comp.get("suite") or "").startswith("release-candidate-")
        for comp in report.get("comparisons") or []
        if isinstance(comp, dict)
    )


def write_report(report: dict[str, Any], out_dir: pathlib.Path) -> pathlib.Path:
    out_dir.mkdir(parents=True, exist_ok=True)
    json_path = out_dir / "radar-candidate-report.json"
    json_path.write_text(json.dumps(report, indent=2, sort_keys=True))
    (out_dir / "radar-candidate-report.md").write_text(render_markdown(report))
    return json_path


def codex_audit_is_no_claim_pass(report: dict[str, Any] | None) -> bool:
    if not isinstance(report, dict):
        return False
    gate = report.get("gate_status") if isinstance(report.get("gate_status"), dict) else {}
    totals = report.get("totals") if isinstance(report.get("totals"), dict) else {}
    return (
        gate.get("claim_policy") == "no_release_claim"
        and gate.get("status") == "pass"
        and gate.get("release_evidence") is False
        and int(totals.get("proof_ready_comparisons") or 0) == 0
    )


def parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Audit Regression Radar benchmark evidence for saturation/headroom.")
    parser.add_argument("--results", type=pathlib.Path, default=RESULTS, help="Directory containing benchmark suites")
    parser.add_argument("--suite-glob", action="append", default=None, help="Only audit suites whose directory name matches this glob; repeatable")
    parser.add_argument("--out-dir", type=pathlib.Path, default=None, help="Directory for radar-candidate-report.{json,md}; defaults to --results")
    parser.add_argument("--fail-when-no-promotable", action="store_true", help="Exit nonzero unless at least one Radar comparison is proof-ready or promotable")
    parser.add_argument("--fail-when-no-proof", action="store_true", help="Exit nonzero unless at least one Radar comparison is proof-ready")
    parser.add_argument("--codex-audit-report", type=pathlib.Path, default=None, help="Require proof-ready Radar comparisons to be backed by an audit_codex report")
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(sys.argv[1:] if argv is None else argv)
    suite_globs = args.suite_glob or ["pilot-radar-*", "release-candidate-*"]
    results = args.results.resolve()
    out_dir = (args.out_dir or results).resolve()
    codex_audit = load_codex_audit(args.codex_audit_report.resolve() if args.codex_audit_report else None)
    report = build_report(results, suite_globs, codex_audit)
    json_path = write_report(report, out_dir)
    print(render_markdown(report).split("\n\n", 1)[0])
    print(f"\nWrote {json_path} and radar-candidate-report.md")
    totals = report["totals"]
    if args.fail_when_no_proof and args.codex_audit_report is None and has_release_candidate_comparison(report):
        print("Release-candidate Radar proof requires --codex-audit-report.", file=sys.stderr)
        return 1
    if args.fail_when_no_proof and int(totals["proof_ready"]) <= 0:
        # A verified no-claim posture satisfies the proof requirement only; it
        # must not early-return past --fail-when-no-promotable below, which is
        # an independent requirement when both flags are passed.
        if codex_audit_is_no_claim_pass(codex_audit):
            print("Radar evidence is no-claim; no proof-ready comparison required.")
        else:
            print("Radar evidence has no proof-ready comparison.", file=sys.stderr)
            return 1
    if args.fail_when_no_promotable and int(totals["promotable_or_proof"]) <= 0:
        print("Radar evidence has no promotable pilot or proof-ready comparison.", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
