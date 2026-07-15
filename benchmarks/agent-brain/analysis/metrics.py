"""Manifest-driven metrics migrated from the former scratch analyzers.

Primary correctness and efficiency values include every executed attempt.
Successful-attempt-only efficiency is intentionally not computed here.
"""

from __future__ import annotations

import statistics
from collections import defaultdict
from typing import Any

from .common import is_executed_run


def _number(record: dict[str, Any], *path: str) -> float | None:
    value: Any = record
    for key in path:
        if not isinstance(value, dict):
            return None
        value = value.get(key)
    return float(value) if isinstance(value, (int, float)) else None


def _primary_duration(record: dict[str, Any]) -> float | None:
    measured = _number(record, "timing", "harness_agent_interval_wall_seconds")
    return measured if measured is not None else _number(record, "agent_info", "seconds")


def headline_table(records: list[dict[str, Any]]) -> dict[str, Any]:
    """Aggregate all executed attempts by task/runner/delivery/condition."""
    groups: dict[tuple[str, str, str, str], list[dict[str, Any]]] = defaultdict(list)
    non_executed = 0
    for record in records:
        if not is_executed_run(record):
            non_executed += 1
            continue
        runner = record.get("runner") if isinstance(record.get("runner"), dict) else {}
        key = (
            str(record.get("task_id") or ""),
            str(runner.get("id") or record.get("agent") or ""),
            str(record.get("delivery_mode") or "agent_tool"),
            str(record.get("condition") or ""),
        )
        groups[key].append(record)

    rows: list[dict[str, Any]] = []
    for (task_id, runner_id, delivery_mode, condition), arm in sorted(groups.items()):
        tokens = [value for record in arm if (value := _number(record, "agent_info", "usage", "total_tokens")) is not None]
        seconds = [value for record in arm if (value := _primary_duration(record)) is not None]
        passed = sum(bool((record.get("validation") or {}).get("ok")) for record in arm)
        rows.append(
            {
                "task_id": task_id,
                "runner": runner_id,
                "delivery_mode": delivery_mode,
                "condition": condition,
                "executed_attempts": len(arm),
                "validation_passes": passed,
                "validation_pass_rate": passed / len(arm),
                "token_denominator": len(tokens),
                "mean_total_tokens_all_executed_with_measurement": statistics.fmean(tokens) if tokens else None,
                "duration_denominator": len(seconds),
                "duration_metric": "harness_agent_interval_wall_seconds_with_legacy_agent_info_fallback",
                "mean_agent_seconds_all_executed_with_measurement": statistics.fmean(seconds) if seconds else None,
            }
        )
    return {
        "estimand": "all_executed_attempts; correctness and efficiency reported separately",
        "records": len(records),
        "executed_records": sum(row["executed_attempts"] for row in rows),
        "non_executed_records": non_executed,
        "arms": rows,
    }


def render_exploratory_markdown(records: list[dict[str, Any]], sources: list[dict[str, Any]]) -> str:
    """Render a legacy-suite migration report without upgrading its evidence status."""
    table = headline_table(records)
    lines = [
        "# Existing-suite analysis (exploratory)",
        "",
        "> This recomputation uses retained raw records and the shared executed-run predicate. "
        "These legacy suites do not contain schema-v1 prompt/packet evidence manifests and are not confirmatory evidence.",
        "",
        f"Estimand: `{table['estimand']}`",
        "",
        f"Raw records: {table['records']} total; {table['executed_records']} executed; {table['non_executed_records']} non-executed.",
        "",
        "## Inputs",
        "",
    ]
    lines.extend(f"- `{item['logical_id']}` — `{item['sha256']}` ({item['records']} records)" for item in sources)
    lines.extend(
        [
            "",
            "## All-executed arm metrics",
            "",
            "| Task | Runner | Delivery | Condition | N | Passes | Pass rate | Token n | Mean tokens | Time n | Mean seconds |",
            "|---|---|---|---|---:|---:|---:|---:|---:|---:|---:|",
        ]
    )
    for row in table["arms"]:
        tokens = "—" if row["mean_total_tokens_all_executed_with_measurement"] is None else f"{row['mean_total_tokens_all_executed_with_measurement']:.1f}"
        seconds = "—" if row["mean_agent_seconds_all_executed_with_measurement"] is None else f"{row['mean_agent_seconds_all_executed_with_measurement']:.1f}"
        lines.append(
            f"| {row['task_id']} | {row['runner']} | {row['delivery_mode']} | {row['condition']} | "
            f"{row['executed_attempts']} | {row['validation_passes']} | {row['validation_pass_rate']:.3f} | "
            f"{row['token_denominator']} | {tokens} | {row['duration_denominator']} | {seconds} |"
        )
    lines.extend(
        [
            "",
            "No successful-attempt-only token or duration headline is computed. Failures remain in the correctness denominator, and measured efficiency values use all executed attempts with that measurement.",
            "",
        ]
    )
    return "\n".join(lines)
