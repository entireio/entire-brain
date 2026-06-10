#!/usr/bin/env python3
"""Audit retained facts-vs-raw eval artifacts for release-claimable proof.

This is intentionally read-only over eval outputs. It does not judge relevance;
it checks that the retained `facts eval --json` and `facts eval-compare --json`
artifacts are paired, hash-matched, proof-labeled, internally consistent, and
explicitly marked release-claimable by the CLI.
"""
from __future__ import annotations

import argparse
import json
import math
import pathlib
import re
import sys
from typing import Any

DEFAULT_REQUIRED_RETRIEVERS = ["facts", "history", "query", "raw-sessions"]
SHA256_VALUE_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
PROOF_LABEL_SOURCES = {"human", "judge_refined"}
RELEVANCE_PROOF_METRICS = {"precision", "recall", "useful_per_1k"}
ALL_PAIRED_METRIC_N = {"precision", "useful_per_1k", "tokens", "latency_ms"}
LABELED_PAIR_METRIC_N = {"recall"}
METRIC_RESULT_FIELDS = {
    "precision": "precision",
    "recall": "recall",
    "useful_per_1k": "useful_per_1k",
    "tokens": "tokens",
    "latency_ms": "latency_ms",
}
METRIC_COMPARE_TOLERANCE = 1e-9
CLAIM_POLICY_PROOF = "proof_required"
CLAIM_POLICY_NO_CLAIM = "no_release_claim"


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


def display_path(path: pathlib.Path) -> str:
    resolved = path.resolve()
    try:
        return str(resolved.relative_to(pathlib.Path.cwd().resolve()))
    except ValueError:
        return str(resolved)


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
    policy = data.get("claim_policy", CLAIM_POLICY_PROOF)
    if policy not in (CLAIM_POLICY_PROOF, CLAIM_POLICY_NO_CLAIM):
        raise SystemExit("facts eval manifest claim_policy must be proof_required or no_release_claim")
    summaries = data.get("summaries")
    if policy == CLAIM_POLICY_NO_CLAIM:
        if summaries not in ({}, None):
            raise SystemExit("facts eval no_release_claim manifest must not list summaries")
        comparisons = data.get("comparisons")
        if comparisons not in ({}, None):
            raise SystemExit("facts eval no_release_claim manifest must not list comparisons")
        claims = data.get("required_claims")
        if claims not in ([], None):
            raise SystemExit("facts eval no_release_claim manifest must not list required_claims")
        return
    if not isinstance(summaries, dict):
        raise SystemExit("facts eval manifest requires summaries")
    comparisons = data.get("comparisons")
    if not isinstance(comparisons, dict):
        raise SystemExit("facts eval manifest requires comparisons")
    claims = data.get("required_claims")
    if not isinstance(claims, list) or not claims:
        raise SystemExit("facts eval manifest requires non-empty required_claims")


def audit_no_claim_manifest(manifest_path: pathlib.Path, manifest: dict[str, Any], root: pathlib.Path) -> dict[str, Any]:
    flags: list[str] = []
    notes = [
        "no facts-vs-raw release claim is retained; paired proof artifacts are still required before claiming facts beat raw sessions",
    ]
    status_rel = manifest.get("facts_status")
    status_report = None
    if status_rel is not None:
        status_path = manifest_artifact(root, status_rel, "facts_status")
        loaded = load_json(status_path)
        if not isinstance(loaded, dict):
            flags.append("facts_status must be a JSON object")
        else:
            status_report = {
                "path": str(status_path.relative_to(root)),
                "facts_arm_ready": loaded.get("facts_arm_ready"),
                "totals": loaded.get("totals"),
                "warnings": loaded.get("warnings") or [],
            }
            if loaded.get("facts_arm_ready") is not False:
                flags.append("facts_status.facts_arm_ready must be false for no_release_claim evidence")
            totals = loaded.get("totals")
            if not isinstance(totals, dict):
                flags.append("facts_status.totals must be an object")
            elif int(totals.get("active") or 0) != 0:
                flags.append("facts_status.totals.active must be 0 for no_release_claim evidence")
    else:
        flags.append("no_release_claim manifest requires facts_status")
    return {
        "schema": 1,
        "manifest": display_path(manifest_path),
        "status": "fail" if flags else "pass",
        "release_evidence": not flags,
        "claim_policy": CLAIM_POLICY_NO_CLAIM,
        "claimable_facts_vs_raw": False,
        "tasks_sha256": "",
        "brain_manifest_sha256": "",
        "required_retrievers": [],
        "summary_paths": {},
        "comparison_paths": {},
        "required_claims": [],
        "facts_status": status_report,
        "flags": flags,
        "notes": notes,
    }


def compare_hash_fields(comp: dict[str, Any], tasks_sha: str, brain_sha: str, flags: list[str], name: str) -> None:
    for side in ("a", "b"):
        task_value = comp.get(f"{side}_tasks_sha256")
        brain_value = comp.get(f"{side}_brain_manifest_sha256")
        if task_value != tasks_sha:
            flags.append(f"{name}: {side}_tasks_sha256 mismatch")
        if brain_value != brain_sha:
            flags.append(f"{name}: {side}_brain_manifest_sha256 mismatch")


def comparison_side_for_retriever(comp: dict[str, Any], retriever: str) -> str:
    if comp.get("a_retriever") == retriever:
        return "a"
    if comp.get("b_retriever") == retriever:
        return "b"
    return ""


def proof_label_summary_flags(retriever: str, summary: dict[str, Any]) -> list[str]:
    results = summary.get("results")
    if not isinstance(results, list) or not results:
        return []
    bad: list[str] = []
    for index, result in enumerate(results):
        if not isinstance(result, dict):
            bad.append(f"#{index + 1}(not-object)")
            continue
        if (
            result.get("labeled") is True
            and result.get("relevance_source") == "explicit_label"
            and result.get("label_source") in PROOF_LABEL_SOURCES
        ):
            continue
        ident = str(result.get("id") or f"#{index + 1}")
        bad.append(
            f"{ident}(labeled={result.get('labeled')!r}, "
            f"relevance_source={result.get('relevance_source')!r}, "
            f"label_source={result.get('label_source')!r})"
        )
    if not bad:
        return []
    sample = ", ".join(bad[:5])
    if len(bad) > 5:
        sample += f", ... {len(bad) - 5} more"
    return [f"{retriever}: {len(bad)} result(s) are not human/judge_refined explicit proof labels: {sample}"]


def sample_ids(ids: list[str]) -> str:
    sample = ", ".join(ids[:5])
    if len(ids) > 5:
        sample += f", ... {len(ids) - 5} more"
    return sample


def result_index_for_summary(retriever: str, summary: dict[str, Any], flags: list[str]) -> dict[str, dict[str, Any]]:
    results = summary.get("results")
    if not isinstance(results, list):
        return {}
    by_id: dict[str, dict[str, Any]] = {}
    for index, result in enumerate(results):
        if not isinstance(result, dict):
            flags.append(f"{retriever}: result #{index + 1} must be an object")
            continue
        task_id = result.get("id")
        if not isinstance(task_id, str) or not task_id:
            flags.append(f"{retriever}: result #{index + 1} must have non-empty id")
            continue
        if task_id in by_id:
            flags.append(f"{retriever}: duplicate result id {task_id!r}")
            continue
        by_id[task_id] = result
    return by_id


def metric_n_from_retained_summaries(
    metric_name: str,
    paired_ids: list[str],
    a_results: dict[str, dict[str, Any]],
    b_results: dict[str, dict[str, Any]],
) -> int | None:
    if metric_name in ALL_PAIRED_METRIC_N:
        return len(paired_ids)
    if metric_name in LABELED_PAIR_METRIC_N:
        return sum(
            1
            for task_id in paired_ids
            if a_results[task_id].get("labeled") is True and b_results[task_id].get("labeled") is True
        )
    return None


def finite_number(value: Any) -> float | None:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return None
    out = float(value)
    if not math.isfinite(out):
        return None
    return out


def metric_winner(metric_name: str, mean_a: float, mean_b: float) -> str:
    if abs(mean_a - mean_b) <= METRIC_COMPARE_TOLERANCE:
        return "tie"
    if metric_name in {"tokens", "latency_ms"}:
        return "a" if mean_a < mean_b else "b"
    return "a" if mean_a > mean_b else "b"


def retained_metric_stats(
    name: str,
    metric_name: str,
    paired_ids: list[str],
    a_results: dict[str, dict[str, Any]],
    b_results: dict[str, dict[str, Any]],
    flags: list[str],
) -> dict[str, Any] | None:
    field = METRIC_RESULT_FIELDS.get(metric_name)
    if field is None:
        return None
    values_a: list[float] = []
    values_b: list[float] = []
    for task_id in paired_ids:
        a_row = a_results[task_id]
        b_row = b_results[task_id]
        if metric_name in LABELED_PAIR_METRIC_N and not (a_row.get("labeled") is True and b_row.get("labeled") is True):
            continue
        a_value = finite_number(a_row.get(field))
        b_value = finite_number(b_row.get(field))
        if a_value is None:
            flags.append(f"{name}: {metric_name} retained row {task_id!r} missing numeric A.{field}")
            continue
        if b_value is None:
            flags.append(f"{name}: {metric_name} retained row {task_id!r} missing numeric B.{field}")
            continue
        values_a.append(a_value)
        values_b.append(b_value)
    n = len(values_a)
    if n == 0:
        return {"n": 0, "mean_a": 0.0, "mean_b": 0.0, "delta": 0.0, "winner": "tie"}
    mean_a = sum(values_a) / n
    mean_b = sum(values_b) / n
    return {
        "n": n,
        "mean_a": mean_a,
        "mean_b": mean_b,
        "delta": mean_b - mean_a,
        "winner": metric_winner(metric_name, mean_a, mean_b),
    }


def compare_numeric_metric_field(
    name: str,
    metric_name: str,
    metric: dict[str, Any],
    field: str,
    retained_value: float,
    flags: list[str],
) -> None:
    declared_value = finite_number(metric.get(field))
    if declared_value is None:
        flags.append(f"{name}: {metric_name} missing numeric {field}")
        return
    if abs(declared_value - retained_value) > METRIC_COMPARE_TOLERANCE:
        flags.append(f"{name}: {metric_name} {field} is {declared_value:g}, retained rows recompute to {retained_value:g}")


def recompute_comparison_pairing(
    name: str,
    comp: dict[str, Any],
    summary_results_by_id: dict[str, dict[str, dict[str, Any]]],
    flags: list[str],
) -> dict[str, Any] | None:
    a_retriever = comp.get("a_retriever")
    b_retriever = comp.get("b_retriever")
    if not isinstance(a_retriever, str) or not isinstance(b_retriever, str):
        return None
    a_results = summary_results_by_id.get(a_retriever)
    b_results = summary_results_by_id.get(b_retriever)
    if a_results is None or b_results is None:
        return None

    a_ids = set(a_results)
    b_ids = set(b_results)
    missing_from_a = sorted(b_ids - a_ids)
    missing_from_b = sorted(a_ids - b_ids)
    paired_ids = sorted(a_ids & b_ids)

    if missing_from_a:
        flags.append(f"{name}: retained summaries have {len(missing_from_a)} id(s) missing from A: {sample_ids(missing_from_a)}")
    if missing_from_b:
        flags.append(f"{name}: retained summaries have {len(missing_from_b)} id(s) missing from B: {sample_ids(missing_from_b)}")
    if isinstance(comp.get("n"), int) and comp.get("n") != len(paired_ids):
        flags.append(f"{name}: comparison n is {comp.get('n')}, retained paired rows recompute to {len(paired_ids)}")

    for task_id in paired_ids:
        a_row = a_results[task_id]
        b_row = b_results[task_id]
        a_task = a_row.get("task")
        b_task = b_row.get("task")
        if isinstance(a_task, str) and isinstance(b_task, str) and a_task and b_task and a_task != b_task:
            flags.append(f"{name}: task {task_id!r} differs between retained summaries")
        a_query_type = a_row.get("query_type")
        b_query_type = b_row.get("query_type")
        if (
            isinstance(a_query_type, str)
            and isinstance(b_query_type, str)
            and a_query_type
            and b_query_type
            and a_query_type != b_query_type
        ):
            flags.append(f"{name}: task {task_id!r} query_type differs between retained summaries")

    return {
        "paired_ids": paired_ids,
        "a_results": a_results,
        "b_results": b_results,
    }


def audit_facts_eval_manifest(manifest_path: pathlib.Path) -> dict[str, Any]:
    manifest_path = manifest_path.resolve()
    root = manifest_path.parent
    manifest = load_json(manifest_path)
    validate_manifest(manifest)
    claim_policy = manifest.get("claim_policy", CLAIM_POLICY_PROOF)
    if claim_policy == CLAIM_POLICY_NO_CLAIM:
        return audit_no_claim_manifest(manifest_path, manifest, root)

    flags: list[str] = []
    notes: list[str] = []
    required_retrievers = manifest.get("required_retrievers", DEFAULT_REQUIRED_RETRIEVERS)
    if not isinstance(required_retrievers, list) or not all(isinstance(r, str) and r for r in required_retrievers):
        raise SystemExit("facts eval manifest required_retrievers must be a string list")
    missing_canonical = [r for r in DEFAULT_REQUIRED_RETRIEVERS if r not in required_retrievers]
    if missing_canonical:
        flags.append("required_retrievers missing canonical retrievers: " + ", ".join(missing_canonical))

    summaries: dict[str, dict[str, Any]] = {}
    summary_results_by_id: dict[str, dict[str, dict[str, Any]]] = {}
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
        summary_results_by_id[retriever] = result_index_for_summary(retriever, summary, flags)

    if len(tasks_hashes) > 1:
        flags.append("eval summaries have differing tasks_sha256 values")
    if len(brain_hashes) > 1:
        flags.append("eval summaries have differing brain_manifest_sha256 values")
    tasks_sha = next(iter(tasks_hashes), "")
    brain_sha = next(iter(brain_hashes), "")

    comparisons: dict[str, dict[str, Any]] = {}
    comparison_metric_ns: dict[tuple[str, str], int] = {}
    comparison_metric_stats: dict[tuple[str, str], dict[str, Any]] = {}
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
        for side in ("a", "b"):
            retriever = comp.get(f"{side}_retriever")
            if not isinstance(retriever, str) or not retriever:
                flags.append(f"{name}: {side}_retriever must be non-empty")
            elif retriever not in summaries:
                flags.append(f"{name}: {side}_retriever {retriever!r} has no retained summary")
        compare_hash_fields(comp, tasks_sha, brain_sha, flags, name)
        if comp.get("release_pairing_ready") is not True:
            flags.append(f"{name}: release_pairing_ready must be true")
        if comp.get("release_claimable") is not True:
            flags.append(f"{name}: release_claimable must be true")
        if not isinstance(comp.get("n"), int) or comp.get("n") <= 0:
            flags.append(f"{name}: comparison has no paired rows")
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
        pairing = recompute_comparison_pairing(name, comp, summary_results_by_id, flags)
        if pairing is not None:
            for metric in comp.get("metrics") or []:
                if not isinstance(metric, dict):
                    continue
                metric_name = metric.get("metric")
                if not isinstance(metric_name, str) or not metric_name:
                    continue
                retained_n = metric_n_from_retained_summaries(
                    metric_name,
                    pairing["paired_ids"],
                    pairing["a_results"],
                    pairing["b_results"],
                )
                if retained_n is None:
                    continue
                comparison_metric_ns[(name, metric_name)] = retained_n
                if isinstance(metric.get("n"), int) and metric.get("n") != retained_n:
                    flags.append(f"{name}: {metric_name} n is {metric.get('n')}, retained rows recompute to {retained_n}")
                stats = retained_metric_stats(
                    name,
                    metric_name,
                    pairing["paired_ids"],
                    pairing["a_results"],
                    pairing["b_results"],
                    flags,
                )
                if stats is None:
                    continue
                comparison_metric_stats[(name, metric_name)] = stats
                compare_numeric_metric_field(name, metric_name, metric, "mean_a", float(stats["mean_a"]), flags)
                compare_numeric_metric_field(name, metric_name, metric, "mean_b", float(stats["mean_b"]), flags)
                compare_numeric_metric_field(name, metric_name, metric, "delta", float(stats["delta"]), flags)
                declared_winner = metric.get("winner")
                if declared_winner != stats["winner"]:
                    flags.append(f"{name}: {metric_name} winner is {declared_winner!r}, retained rows recompute to {stats['winner']!r}")

    claim_reports: list[dict[str, Any]] = []
    proof_checked_retrievers: set[str] = set()
    facts_vs_raw_claim = False
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
        if not isinstance(expected_a, str) or not expected_a:
            flags.append(f"{name}: required claim must include a_retriever")
        if not isinstance(expected_b, str) or not expected_b:
            flags.append(f"{name}: required claim must include b_retriever")
        if expected_a and comp.get("a_retriever") != expected_a:
            flags.append(f"{name}: a_retriever is {comp.get('a_retriever')!r}, want {expected_a!r}")
        if expected_b and comp.get("b_retriever") != expected_b:
            flags.append(f"{name}: b_retriever is {comp.get('b_retriever')!r}, want {expected_b!r}")
        if metric_name in RELEVANCE_PROOF_METRICS and expected_basis != "proof_labels":
            flags.append(f"{name}: {metric_name} required claim evidence_basis must be 'proof_labels'")
        if expected_basis == "proof_labels" and metric_name in RELEVANCE_PROOF_METRICS:
            for retriever in (comp.get("a_retriever"), comp.get("b_retriever")):
                if not isinstance(retriever, str) or not retriever:
                    continue
                if retriever in proof_checked_retrievers:
                    continue
                proof_checked_retrievers.add(retriever)
                summary = summaries.get(retriever)
                if summary is None:
                    flags.append(f"{name}: proof-label claim references missing summary for retriever {retriever}")
                    continue
                flags.extend(proof_label_summary_flags(retriever, summary))
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
        retained_metric_n = comparison_metric_ns.get((name, metric_name))
        retained_stats = comparison_metric_stats.get((name, metric_name))
        if metric_name in RELEVANCE_PROOF_METRICS and retained_metric_n is None:
            flags.append(f"{name}: {metric_name} retained metric n could not be recomputed")
        if metric_name in RELEVANCE_PROOF_METRICS and retained_stats is None:
            flags.append(f"{name}: {metric_name} retained metric values could not be recomputed")
        metric_n_verified = (
            retained_metric_n is not None
            and isinstance(metric.get("n"), int)
            and metric.get("n") == retained_metric_n
            and retained_metric_n > 0
        )
        metric_values_verified = (
            retained_stats is not None
            and metric.get("winner") == retained_stats.get("winner")
            and finite_number(metric.get("mean_a")) is not None
            and finite_number(metric.get("mean_b")) is not None
            and finite_number(metric.get("delta")) is not None
        )
        facts_side = comparison_side_for_retriever(comp, "facts")
        raw_side = comparison_side_for_retriever(comp, "raw-sessions")
        if (
            metric_name in RELEVANCE_PROOF_METRICS
            and expected_basis == "proof_labels"
            and facts_side
            and raw_side
            and facts_side != raw_side
            and metric.get("winner") == facts_side
            and metric.get("release_claimable") is True
            and metric.get("significant") is True
            and metric_n_verified
            and metric_values_verified
        ):
            facts_vs_raw_claim = True
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
            "retained_n": retained_metric_n,
            "retained_mean_a": retained_stats.get("mean_a") if retained_stats else None,
            "retained_mean_b": retained_stats.get("mean_b") if retained_stats else None,
            "retained_delta": retained_stats.get("delta") if retained_stats else None,
            "retained_winner": retained_stats.get("winner") if retained_stats else None,
        })

    if not comparisons:
        notes.append("no comparison artifacts loaded")
    if not facts_vs_raw_claim:
        flags.append("required_claims must include a proof-label claim where facts beats raw-sessions")

    return {
        "schema": 1,
        "manifest": display_path(manifest_path),
        "status": "fail" if flags else "pass",
        "release_evidence": not flags,
        "claim_policy": CLAIM_POLICY_PROOF,
        "claimable_facts_vs_raw": not flags and facts_vs_raw_claim,
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
    required_retrievers = ", ".join(f"`{r}`" for r in report.get("required_retrievers", [])) or "none"
    lines = [
        "# Facts Eval Evidence Audit",
        "",
        f"- Status: **{report['status'].upper()}**",
        f"- Release evidence: **{str(report['release_evidence']).lower()}**",
        f"- Claim policy: **{report.get('claim_policy', CLAIM_POLICY_PROOF)}**",
        f"- Facts-vs-raw claimable: **{str(report.get('claimable_facts_vs_raw', False)).lower()}**",
        f"- Tasks hash: `{report.get('tasks_sha256') or 'unset'}`",
        f"- Brain manifest hash: `{report.get('brain_manifest_sha256') or 'unset'}`",
        f"- Required retrievers: {required_retrievers}",
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
    if report.get("notes"):
        lines.extend(["", "## Notes", ""])
        for note in report["notes"]:
            lines.append(f"- {note}")
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
