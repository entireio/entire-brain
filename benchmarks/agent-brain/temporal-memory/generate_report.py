#!/usr/bin/env python3
"""Generate a privacy-preserving Phase 0A development report from retained records."""

from __future__ import annotations

import argparse
import hashlib
import importlib.util
import json
import os
import pathlib
import subprocess
import sys
from typing import Any


HERE = pathlib.Path(__file__).resolve().parent
BENCH = HERE.parent
ROOT = BENCH.parents[1]
CACHE = BENCH / "cache"
HARNESS_PATH = BENCH / "run.py"
HARNESS_SPEC = importlib.util.spec_from_file_location("temporal_memory_harness", HARNESS_PATH)
if HARNESS_SPEC is None or HARNESS_SPEC.loader is None:
    raise RuntimeError(f"cannot load benchmark harness: {HARNESS_PATH}")
HARNESS = importlib.util.module_from_spec(HARNESS_SPEC)
sys.modules[HARNESS_SPEC.name] = HARNESS
HARNESS_SPEC.loader.exec_module(HARNESS)


def sha256_text(value: str) -> str:
    return hashlib.sha256(value.encode()).hexdigest()


def read_json(path: pathlib.Path) -> Any:
    return json.loads(path.read_text())


def condition_record_path(suite: pathlib.Path, task_id: str, condition: str, prep: bool) -> pathlib.Path:
    runner = "prep" if prep else None
    if prep:
        name = f"{task_id}__{runner}__{condition}"
    else:
        candidates = sorted(suite.glob(f"{task_id}__*__{condition}__r1/record.json"))
        if len(candidates) != 1:
            raise RuntimeError(f"expected one {condition} task record under {suite}, found {len(candidates)}")
        return candidates[0]
    return suite / name / "record.json"


def condition_record(suite: pathlib.Path, task_id: str, condition: str, prep: bool) -> dict[str, Any]:
    return read_json(condition_record_path(suite, task_id, condition, prep))


def task_query(task: dict[str, Any]) -> str:
    base = str(task["prompt"]).strip()
    query = f"{task['id']}: {base[:120]}"
    terms = ", ".join(task.get("brain_queries", []))
    return " ".join((f"{query} | {terms}" if terms else query).split())


def retrieval_probe(
    record: dict[str, Any], binary: pathlib.Path, task: dict[str, Any], expected_text: str
) -> dict[str, Any]:
    key = record["brain_prep"]["cache"]["key"]
    plugin = CACHE / key / "plugin"
    env = os.environ.copy()
    env.update(
        {
            "ENTIRE_REPO_ROOT": str(ROOT),
            "ENTIRE_PLUGIN_CONFIG_DIR": str(plugin / "config"),
            "ENTIRE_PLUGIN_DATA_DIR": str(plugin / "data"),
            "ENTIRE_PLUGIN_STATE_DIR": str(plugin / "state"),
            "ENTIRE_PLUGIN_CACHE_DIR": str(plugin / "cache"),
        }
    )
    query = task_query(task)
    branch = task["memory_bundle"]["retrieval_branch"]
    proc = subprocess.run(
        [str(binary), "search", query, "--json", "--limit", "10", "--branch", branch],
        cwd=ROOT,
        env=env,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        timeout=120,
        check=False,
    )
    if proc.returncode != 0:
        raise RuntimeError(f"retrieval probe failed for {record['condition']}: {proc.stderr}")
    payload = json.loads(proc.stdout)
    rows = payload.get("results", [])
    expected_ranks = [index + 1 for index, row in enumerate(rows) if expected_text in str(row.get("text") or "")]
    return {
        "condition": record["condition"],
        "query_sha256": sha256_text(query),
        "result_count": len(rows),
        "expected_text_sha256": sha256_text(expected_text),
        "expected_ranks": expected_ranks,
        "top_results": [
            {
                "rank": index + 1,
                "source": row.get("source"),
                "id": row.get("id"),
                "text_sha256": sha256_text(str(row.get("text") or "")),
            }
            for index, row in enumerate(rows)
        ],
    }


def agent_metrics(record_path: pathlib.Path) -> dict[str, Any]:
    record = read_json(record_path)
    usage = (record.get("agent_info") or {}).get("usage") or {}
    validation = record.get("validation") or {}
    score = record.get("score") or {}
    stdout = record_path.with_name("agent.stdout").read_text(encoding="utf-8", errors="ignore")
    stderr = record_path.with_name("agent.stderr").read_text(encoding="utf-8", errors="ignore")
    activity = HARNESS.extract_agent_activity(stdout, stderr)
    protocol_audit = HARNESS.temporal_memory_condition_audit(
        str(record.get("condition") or ""), {"activity": activity}
    )
    runner = record.get("runner") or {}
    return {
        "agent": record.get("agent"),
        "runner_id": runner.get("id"),
        "resolved_model": HARNESS.extract_resolved_model(stdout),
        "condition": record.get("condition"),
        "ok": record.get("ok"),
        "validation_ok": validation.get("ok"),
        "score": score.get("total"),
        "total_tokens": usage.get("total_tokens"),
        "input_tokens": usage.get("input_tokens"),
        "output_tokens": usage.get("output_tokens"),
        "turns": usage.get("turns"),
        "seconds": (record.get("agent_info") or {}).get("seconds"),
        "brain_commands": activity.get("brain_commands", []),
        "direct_brain_cli_calls": activity.get("direct_brain_cli_calls"),
        "first_tool_is_memory_search": activity.get("first_tool_is_memory_search"),
        "protocol_audit_ok": protocol_audit.get("ok"),
        "protocol_findings": protocol_audit.get("findings", []),
        "changed_files": record.get("changed_files", []),
        "leak_audit_ok": (record.get("agent_leak_audit") or {}).get("ok"),
    }


def agent_smoke_interpretation(metrics: list[dict[str, Any]]) -> str:
    if not metrics:
        return ""
    by_runner: dict[str, dict[str, dict[str, Any]]] = {}
    for row in metrics:
        by_runner.setdefault(str(row["runner_id"]), {})[str(row["condition"])] = row
    history_successes = 0
    fact_failures = 0
    efficiency: list[str] = []
    for runner, rows in sorted(by_runner.items()):
        baseline = rows.get("no_brain", {})
        raw = rows.get("raw_history", {})
        combined = rows.get("history_facts", {})
        facts = rows.get("facts_only", {})
        if raw.get("validation_ok") is True and combined.get("validation_ok") is True:
            history_successes += 1
        if facts.get("validation_ok") is False:
            fact_failures += 1
        baseline_tokens = baseline.get("total_tokens")
        raw_tokens = raw.get("total_tokens")
        combined_tokens = combined.get("total_tokens")
        if isinstance(baseline_tokens, (int, float)) and baseline_tokens > 0:
            raw_reduction = 100.0 * (1.0 - float(raw_tokens) / float(baseline_tokens))
            combined_reduction = 100.0 * (1.0 - float(combined_tokens) / float(baseline_tokens))
            efficiency.append(
                f"`{runner}` used {raw_reduction:.1f}% fewer tokens with history and "
                f"{combined_reduction:.1f}% fewer with history plus facts than its no-Brain arm"
            )
    deviations = [
        f"`{row['runner_id']}/{row['condition']}`"
        for row in metrics
        if row.get("protocol_audit_ok") is not True
    ]
    statements = [
        f"Both history-bearing arms passed validation for {history_successes}/{len(by_runner)} backends; "
        f"facts-only failed for {fact_failures}/{len(by_runner)} backends.",
        "; ".join(efficiency) + "." if efficiency else "",
    ]
    if deviations:
        statements.append(
            "Protocol deviations occurred in " + ", ".join(deviations)
            + "; comparisons using those rows are diagnostic only even when validation passed."
        )
    statements.append(
        "With one authored task and one repetition per backend, these are mechanism and failure-mode observations, not treatment-effect estimates."
    )
    return " ".join(statement for statement in statements if statement)


def render_markdown(report: dict[str, Any]) -> str:
    lines = [
        "# Temporal Memory Phase 0A Development Report",
        "",
        f"- Status: **{report['status']}**",
        f"- Task: `{report['task_id']}`",
        f"- Source cache: `{report['source_cache_key']}`",
        f"- Session transcript SHA-256: `{report['transcript_sha256']}`",
        f"- History index SHA-256: `{report['history_sha256']}`",
        f"- Fact artifact SHA-256: `{report['facts_sha256']}`",
        f"- Distilled facts: **{report['facts_count']}**",
        "",
        "## Retrieval Probe",
        "",
        "| Condition | Results | Expected ranks |",
        "|---|---:|---|",
    ]
    for row in report["retrieval_probes"]:
        ranks = ", ".join(str(rank) for rank in row["expected_ranks"]) or "none"
        lines.append(f"| `{row['condition']}` | {row['result_count']} | {ranks} |")
    if report.get("agent_metrics"):
        lines.extend(
            [
                "",
                "## Agent Smoke",
                "",
                "| Runner | Resolved model | Condition | Validation | Protocol | Score | Tokens | Seconds | Brain calls |",
                "|---|---|---|---:|---:|---:|---:|---:|---:|",
            ]
        )
        for row in report["agent_metrics"]:
            lines.append(
                f"| `{row['runner_id']}` | `{row['resolved_model'] or 'not output-confirmed'}` | `{row['condition']}` | {row['validation_ok']} | "
                f"{row['protocol_audit_ok']} | {row['score']} | {row['total_tokens']} | "
                f"{row['seconds']:.1f} | {row['direct_brain_cli_calls']} |"
            )
    lines.extend(
        [
            "",
            "## Interpretation",
            "",
            report["interpretation"],
            "",
            report.get("agent_smoke_interpretation", ""),
            "",
            "This development task is diagnostic and not publication-level evidence.",
        ]
    )
    return "\n".join(lines) + "\n"


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--source-suite", type=pathlib.Path, required=True)
    parser.add_argument("--derived-suite", type=pathlib.Path, required=True)
    parser.add_argument("--agent-suite", type=pathlib.Path, action="append", default=[])
    parser.add_argument("--task", type=pathlib.Path, required=True)
    parser.add_argument("--expected-text", required=True)
    parser.add_argument("--out-dir", type=pathlib.Path, required=True)
    args = parser.parse_args()

    task = read_json(args.task)
    task_id = task["id"]
    source = condition_record(args.source_suite, task_id, "raw_history", prep=True)
    facts = condition_record(args.derived_suite, task_id, "facts_only", prep=True)
    combined = condition_record(args.derived_suite, task_id, "history_facts", prep=True)
    source_key = source["brain_prep"]["source_cache"]["key"]
    if {facts["brain_prep"]["source_cache"]["key"], combined["brain_prep"]["source_cache"]["key"]} != {source_key}:
        raise RuntimeError("derived conditions do not share the frozen temporal source cache")

    source_bundle = source["brain_prep"]["memory_bundle"]
    facts_sha = source_bundle["facts"]["artifacts"][0]["sha256"]
    for record in (facts, combined):
        if record["brain_prep"]["memory_bundle"]["facts"]["artifacts"][0]["sha256"] != facts_sha:
            raise RuntimeError("fact artifact differs across temporal-memory conditions")

    binary = (args.source_suite / "bin" / "entire-brain").resolve()
    probes = [retrieval_probe(record, binary, task, args.expected_text) for record in (source, facts, combined)]
    metrics = []
    for suite in args.agent_suite:
        metrics.extend(
            agent_metrics(condition_record_path(suite, task_id, condition, prep=False))
            for condition in ("no_brain", "raw_history", "facts_only", "history_facts")
        )

    history_has = bool(probes[0]["expected_ranks"])
    facts_has = bool(probes[1]["expected_ranks"])
    interpretation = (
        "Indexed history retained and retrieved the decisive pre-cutoff value, while the distilled fact store omitted it. "
        "This task therefore supports history-channel feasibility and records a fact-compression miss; it must not be used as a positive durable-facts task."
        if history_has and not facts_has
        else "The retrieval pattern did not match the preregistered history-present/facts-absent development expectation; inspect the frozen artifacts before proceeding."
    )
    report = {
        "schema": 1,
        "status": (
            "complete_with_protocol_deviation"
            if metrics and any(row.get("protocol_audit_ok") is not True for row in metrics)
            else "complete"
            if metrics
            else "retrieval_complete_agent_pending"
        ),
        "task_id": task_id,
        "source_cache_key": source_key,
        "checkpoint_ref_commit": source_bundle["checkpoint_ref_commit"],
        "cutoff_at": source_bundle["cutoff_at"],
        "transcript_sha256": source_bundle["selected_sessions"][0]["transcript_sha256"],
        "history_sha256": source_bundle["history_index"]["sha256"],
        "facts_sha256": facts_sha,
        "facts_count": source_bundle["facts"]["count"],
        "retrieval_probes": probes,
        "agent_metrics": metrics,
        "interpretation": interpretation,
        "agent_smoke_interpretation": agent_smoke_interpretation(metrics),
    }
    args.out_dir.mkdir(parents=True, exist_ok=True)
    (args.out_dir / "development-report.json").write_text(json.dumps(report, indent=2, sort_keys=True) + "\n")
    (args.out_dir / "development-report.md").write_text(render_markdown(report))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
