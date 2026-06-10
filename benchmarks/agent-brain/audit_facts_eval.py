#!/usr/bin/env python3
"""Audit retained facts-vs-raw eval artifacts for release-claimable proof.

This is intentionally read-only over eval outputs. It does not recompute metrics
or judge relevance; it checks that the retained `facts eval --json` and
`facts eval-compare --json` artifacts are paired, hash-matched, proof-labeled,
and explicitly marked release-claimable by the CLI.
"""
from __future__ import annotations

import argparse
import json
import pathlib
import re
import sys
from typing import Any

DEFAULT_REQUIRED_RETRIEVERS = ["facts", "history", "query", "raw-sessions"]
SHA256_VALUE_RE = re.compile(r"^sha256:[0-9a-f]{64}$")


def get(d: Any, *path: str, default: Any = None) -> Any:
    cur = d
    for part in path:
        if not isinstance(cur, dict):
            return default
        cur = cur.get(part)
    return cur if cur is not None else default


def load_json(path: pathlib.Path) -> Any:
    try:
        return json.loads(path.read_text())
    except FileNotFoundError as exc:
        raise SystemExit(f"artifact not found: {path}") from exc
    except json.JSONDecodeError as exc:
        raise SystemExit(f"artifact is not valid JSON: {path}: {exc}") from exc


def is_sha256_value(value: Any) -> bool:
    return isinstance(value, str) and bool(SHA256_VALUE_RE.fullmatch(value))


def manifest_artifact(root: pathlib.Path, value: Any, field: str) -> pathlib.Path:
    if not isinstance(value, str) or not value:
        raise SystemExit(f"facts eval manifest requires non-empty {field}")
    path = pathlib.Path(value)
    if path.is_absolute():
        raise SystemExit(f"facts eval manifest {field} must be relative, got {value}")
    return (root / path).resolve()


def validate_manifest(data: Any) -> None:
    if not isinstance(data, dict):
        raise SystemExit("facts eval manifest must be a JSON object")
    if data.get("schema") != 1:
        raise SystemExit("facts eval manifest schema must be 1")
    summaries = data.get("summaries")
    if not isinstance(summaries, dict):
        raise SystemExit("facts eval manifest requires summaries")
    comparisons = data.get("comparisons")
    if not isinstance(comparisons, dict):
        raise SystemExit("facts eval manifest requires comparisons")
    claims = data.get("required_claims")
    if not isinstance(claims, list) or not claims:
        raise SystemExit("facts eval manifest requires non-empty required_claims")


def compare_hash_fields(comp: dict[str, Any], tasks_sha: str, brain_sha: str, flags: list[str], name: str) -> None:
    for side in ("a", "b"):
        task_value = comp.get(f"{side}_tasks_sha256")
        brain_value = comp.get(f"{side}_brain_manifest_sha256")
        if task_value != tasks_sha:
            flags.append(f"{name}: {side}_tasks_sha256 mismatch")
        if brain_value != brain_sha:
            flags.append(f"{name}: {side}_brain_manifest_sha256 mismatch")


def audit_facts_eval_manifest(manifest_path: pathlib.Path) -> dict[str, Any]:
    manifest_path = manifest_path.resolve()
    root = manifest_path.parent
    manifest = load_json(manifest_path)
    validate_manifest(manifest)

    flags: list[str] = []
    notes: list[str] = []
    required_retrievers = manifest.get("required_retrievers", DEFAULT_REQUIRED_RETRIEVERS)
    if not isinstance(required_retrievers, list) or not all(isinstance(r, str) and r for r in required_retrievers):
        raise SystemExit("facts eval manifest required_retrievers must be a string list")

    summaries: dict[str, dict[str, Any]] = {}
    summary_paths: dict[str, str] = {}
    tasks_hashes: set[str] = set()
    brain_hashes: set[str] = set()
    for retriever in required_retrievers:
        rel = get(manifest, "summaries", retriever)
        if rel is None:
            flags.append(f"missing summary for retriever {retriever}")
            continue
        path = manifest_artifact(root, rel, f"summaries.{retriever}")
        summary = load_json(path)
        if not isinstance(summary, dict):
            flags.append(f"{retriever}: summary must be a JSON object")
            continue
        summaries[retriever] = summary
        summary_paths[retriever] = str(path.relative_to(root))
        if summary.get("retriever") != retriever:
            flags.append(f"{retriever}: summary retriever is {summary.get('retriever')!r}")
        tasks_sha = get(summary, "run_config", "tasks_sha256")
        brain_sha = get(summary, "run_config", "brain_manifest_sha256")
        if not is_sha256_value(tasks_sha):
            flags.append(f"{retriever}: missing or invalid run_config.tasks_sha256")
        else:
            tasks_hashes.add(tasks_sha)
        if not is_sha256_value(brain_sha):
            flags.append(f"{retriever}: missing or invalid run_config.brain_manifest_sha256")
        else:
            brain_hashes.add(brain_sha)
        if not isinstance(summary.get("results"), list) or not summary.get("results"):
            flags.append(f"{retriever}: summary results must be non-empty")

    if len(tasks_hashes) > 1:
        flags.append("eval summaries have differing tasks_sha256 values")
    if len(brain_hashes) > 1:
        flags.append("eval summaries have differing brain_manifest_sha256 values")
    tasks_sha = next(iter(tasks_hashes), "")
    brain_sha = next(iter(brain_hashes), "")

    comparisons: dict[str, dict[str, Any]] = {}
    comparison_paths: dict[str, str] = {}
    for name, rel in (manifest.get("comparisons") or {}).items():
        if not isinstance(name, str) or not name:
            flags.append("comparison names must be non-empty strings")
            continue
        path = manifest_artifact(root, rel, f"comparisons.{name}")
        comp = load_json(path)
        if not isinstance(comp, dict):
            flags.append(f"{name}: comparison must be a JSON object")
            continue
        comparisons[name] = comp
        comparison_paths[name] = str(path.relative_to(root))
        compare_hash_fields(comp, tasks_sha, brain_sha, flags, name)
        if comp.get("allow_proxy_comparison") is True:
            flags.append(f"{name}: allow_proxy_comparison must be false for release proof")
        for field in ("allow_missing_tasks", "allow_task_hash_mismatch", "allow_brain_manifest_mismatch"):
            if comp.get(field) is True:
                flags.append(f"{name}: {field} must be false for release proof")
        if comp.get("missing_from_a") not in ([], None):
            flags.append(f"{name}: missing_from_a must be empty")
        if comp.get("missing_from_b") not in ([], None):
            flags.append(f"{name}: missing_from_b must be empty")
        if not isinstance(comp.get("metrics"), list) or not comp.get("metrics"):
            flags.append(f"{name}: metrics must be non-empty")

    claim_reports: list[dict[str, Any]] = []
    for claim in manifest.get("required_claims") or []:
        if not isinstance(claim, dict):
            flags.append("required_claims entries must be objects")
            continue
        name = claim.get("comparison")
        metric_name = claim.get("metric")
        if not isinstance(name, str) or name not in comparisons:
            flags.append(f"required claim references missing comparison {name!r}")
            continue
        if not isinstance(metric_name, str) or not metric_name:
            flags.append(f"{name}: required claim needs metric")
            continue
        comp = comparisons[name]
        metrics = [m for m in comp.get("metrics", []) if isinstance(m, dict) and m.get("metric") == metric_name]
        if not metrics:
            flags.append(f"{name}: metric {metric_name!r} not found")
            continue
        metric = metrics[0]
        expected_winner = claim.get("winner")
        expected_basis = claim.get("evidence_basis", "proof_labels")
        expected_a = claim.get("a_retriever")
        expected_b = claim.get("b_retriever")
        if expected_a and comp.get("a_retriever") != expected_a:
            flags.append(f"{name}: a_retriever is {comp.get('a_retriever')!r}, want {expected_a!r}")
        if expected_b and comp.get("b_retriever") != expected_b:
            flags.append(f"{name}: b_retriever is {comp.get('b_retriever')!r}, want {expected_b!r}")
        if metric.get("evidence_basis") != expected_basis:
            flags.append(f"{name}: {metric_name} evidence_basis is {metric.get('evidence_basis')!r}, want {expected_basis!r}")
        if expected_winner and metric.get("winner") != expected_winner:
            flags.append(f"{name}: {metric_name} winner is {metric.get('winner')!r}, want {expected_winner!r}")
        if metric.get("release_claimable") is not True:
            flags.append(f"{name}: {metric_name} is not release_claimable")
        if metric.get("significant") is not True:
            flags.append(f"{name}: {metric_name} is not significant")
        if not isinstance(metric.get("n"), int) or metric.get("n") <= 0:
            flags.append(f"{name}: {metric_name} has no paired rows")
        claim_reports.append({
            "comparison": name,
            "metric": metric_name,
            "a_retriever": comp.get("a_retriever"),
            "b_retriever": comp.get("b_retriever"),
            "winner": metric.get("winner"),
            "evidence_basis": metric.get("evidence_basis"),
            "release_claimable": metric.get("release_claimable"),
            "significant": metric.get("significant"),
            "n": metric.get("n"),
        })

    if not comparisons:
        notes.append("no comparison artifacts loaded")

    return {
        "schema": 1,
        "manifest": str(manifest_path),
        "status": "fail" if flags else "pass",
        "release_evidence": not flags,
        "tasks_sha256": tasks_sha,
        "brain_manifest_sha256": brain_sha,
        "required_retrievers": required_retrievers,
        "summary_paths": summary_paths,
        "comparison_paths": comparison_paths,
        "required_claims": claim_reports,
        "flags": flags,
        "notes": notes,
    }


def render_markdown(report: dict[str, Any]) -> str:
    lines = [
        "# Facts Eval Evidence Audit",
        "",
        f"- Status: **{report['status'].upper()}**",
        f"- Release evidence: **{str(report['release_evidence']).lower()}**",
        f"- Tasks hash: `{report.get('tasks_sha256') or 'unset'}`",
        f"- Brain manifest hash: `{report.get('brain_manifest_sha256') or 'unset'}`",
        f"- Required retrievers: {', '.join(f'`{r}`' for r in report.get('required_retrievers', []))}",
        "",
    ]
    if report.get("required_claims"):
        lines.extend(["## Required Claims", "", "| Comparison | Metric | A | B | Winner | Evidence | Claimable |", "|---|---|---|---|---|---|---|"])
        for claim in report["required_claims"]:
            lines.append(
                f"| {claim.get('comparison')} | {claim.get('metric')} | {claim.get('a_retriever')} | "
                f"{claim.get('b_retriever')} | {claim.get('winner')} | {claim.get('evidence_basis')} | "
                f"{claim.get('release_claimable')} |"
            )
        lines.append("")
    if report.get("flags"):
        lines.extend(["## Flags", ""])
        for flag in report["flags"]:
            lines.append(f"- {flag}")
    else:
        lines.extend(["## Flags", "", "None."])
    return "\n".join(lines) + "\n"


def write_report(report: dict[str, Any], out_dir: pathlib.Path) -> pathlib.Path:
    out_dir.mkdir(parents=True, exist_ok=True)
    json_path = out_dir / "facts-eval-audit-report.json"
    json_path.write_text(json.dumps(report, indent=2))
    (out_dir / "facts-eval-audit-report.md").write_text(render_markdown(report))
    return json_path


def parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Audit retained Entire Brain facts eval evidence artifacts.")
    parser.add_argument("--manifest", type=pathlib.Path, required=True, help="Path to facts-eval evidence manifest.json")
    parser.add_argument("--out-dir", type=pathlib.Path, default=None, help="Output directory for facts-eval-audit-report.{json,md}")
    parser.add_argument("--fail-on-flags", action="store_true", help="Exit nonzero when the retained artifacts are not release evidence")
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(sys.argv[1:] if argv is None else argv)
    report = audit_facts_eval_manifest(args.manifest)
    out_dir = (args.out_dir or args.manifest.parent).resolve()
    json_path = write_report(report, out_dir)
    print(render_markdown(report).split("\n## Flags", 1)[0].rstrip())
    print(f"\nWrote {json_path} and facts-eval-audit-report.md")
    if args.fail_on_flags and report["status"] != "pass":
        print("\nFacts eval artifacts are not release evidence:", file=sys.stderr)
        for flag in report["flags"]:
            print(f"- {flag}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
