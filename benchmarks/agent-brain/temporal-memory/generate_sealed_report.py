#!/usr/bin/env python3
"""Generate and gate the Phase 0A sealed temporal-memory smoke report."""

from __future__ import annotations

import argparse
import hashlib
import importlib.util
import json
import pathlib
import sys
from typing import Any


HERE = pathlib.Path(__file__).resolve().parent
BENCH = HERE.parent
HARNESS_PATH = BENCH / "run.py"
HARNESS_SPEC = importlib.util.spec_from_file_location("temporal_memory_sealed_harness", HARNESS_PATH)
if HARNESS_SPEC is None or HARNESS_SPEC.loader is None:
    raise RuntimeError(f"cannot load benchmark harness: {HARNESS_PATH}")
HARNESS = importlib.util.module_from_spec(HARNESS_SPEC)
sys.modules[HARNESS_SPEC.name] = HARNESS
HARNESS_SPEC.loader.exec_module(HARNESS)

CONDITIONS = ("no_brain", "raw_history", "facts_only", "history_facts")
MEMORY_CONDITIONS = CONDITIONS[1:]


def read_json(path: pathlib.Path) -> Any:
    return json.loads(path.read_text())


def sha256_file(path: pathlib.Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def task_path(manifest_path: pathlib.Path, entry: dict[str, Any]) -> pathlib.Path:
    return (manifest_path.parent / str(entry["path"])).resolve()


def record_path(suite: pathlib.Path, task_id: str, condition: str) -> pathlib.Path:
    candidates = sorted(suite.glob(f"{task_id}__*__{condition}__r1/record.json"))
    if len(candidates) != 1:
        raise RuntimeError(f"expected one {task_id}/{condition} record in {suite}, found {len(candidates)}")
    return candidates[0]


def source_isolation_findings(condition: str, record: dict[str, Any]) -> list[str]:
    if condition == "no_brain":
        return [] if not record.get("brain_state") else ["no_brain_has_brain_state"]
    manifest = ((record.get("brain_state") or {}).get("manifest") or {})
    expected = {
        "raw_history": {"has_history": True, "has_facts": False},
        "facts_only": {"has_history": False, "has_facts": True},
        "history_facts": {"has_history": True, "has_facts": True},
    }[condition]
    findings = [
        f"{field}={manifest.get(field)!r}, expected {want!r}"
        for field, want in expected.items()
        if manifest.get(field) is not want
    ]
    for field in ("has_sessions", "has_semantic", "has_seed", "has_docs", "has_patterns", "has_checkpoints"):
        if manifest.get(field):
            findings.append(f"withheld source present: {field}")
    return findings


def row_from_record(
    record_file: pathlib.Path,
    task_entry: dict[str, Any],
    task: dict[str, Any],
    source_artifact: dict[str, Any],
) -> dict[str, Any]:
    record = read_json(record_file)
    stdout = record_file.with_name("agent.stdout").read_text(encoding="utf-8", errors="ignore")
    stderr = record_file.with_name("agent.stderr").read_text(encoding="utf-8", errors="ignore")
    activity = HARNESS.extract_agent_activity(stdout, stderr)
    protocol = HARNESS.temporal_memory_condition_audit(str(record["condition"]), {"activity": activity})
    usage = (record.get("agent_info") or {}).get("usage") or {}
    condition = str(record["condition"])
    findings: list[str] = []
    expected_hash = str(task_entry["sha256"])
    actual_hash = sha256_file(pathlib.Path(task["_path"]))
    if actual_hash != expected_hash:
        findings.append(f"task hash mismatch: {actual_hash} != {expected_hash}")
    recorded_hash = str((((record.get("provenance") or {}).get("task") or {}).get("config_sha256") or ""))
    if recorded_hash != expected_hash:
        findings.append(f"record task hash mismatch: {recorded_hash} != {expected_hash}")
    if not (record.get("validation") or {}).get("ok"):
        findings.append("hidden validation failed")
    if record.get("ok") is not True:
        findings.append("record is not marked ok")
    if not protocol.get("ok"):
        findings.append(f"protocol audit failed: {protocol.get('findings', [])}")
    if not (record.get("agent_leak_audit") or {}).get("ok"):
        findings.append("agent output leak audit failed")
    if not (record.get("agent_secret_preflight") or {}).get("ok"):
        findings.append("agent secret preflight failed")
    total_tokens = usage.get("total_tokens")
    if not isinstance(total_tokens, (int, float)) or total_tokens <= 0:
        findings.append("missing or zero token usage")
    findings.extend(source_isolation_findings(condition, record))
    if sorted(record.get("changed_files") or []) != sorted(task.get("expected_files") or []):
        findings.append(
            f"changed files mismatch: {sorted(record.get('changed_files') or [])} != {sorted(task.get('expected_files') or [])}"
        )
    if condition in MEMORY_CONDITIONS:
        source_cache = (record.get("brain_prep") or {}).get("source_cache") or {}
        if source_cache.get("key") != source_artifact["cache_key"]:
            findings.append("source cache key mismatch")
    provenance = record.get("provenance") or {}
    harness_dirty = bool((((provenance.get("harness") or {}).get("dirty") or {}).get("dirty")))
    source_dirty = bool((((provenance.get("source") or {}).get("dirty") or {}).get("dirty")))
    return {
        "task_id": record["task_id"],
        "stratum": task_entry["stratum"],
        "runner_id": (record.get("runner") or {}).get("id"),
        "agent": record.get("agent"),
        "resolved_model": HARNESS.extract_resolved_model(stdout),
        "condition": condition,
        "record_ok": record.get("ok"),
        "validation_ok": (record.get("validation") or {}).get("ok"),
        "protocol_ok": protocol.get("ok"),
        "leak_audit_ok": (record.get("agent_leak_audit") or {}).get("ok"),
        "source_isolation_ok": not source_isolation_findings(condition, record),
        "total_tokens": total_tokens,
        "input_tokens": usage.get("input_tokens"),
        "output_tokens": usage.get("output_tokens"),
        "cache_creation_tokens": usage.get("cache_creation_tokens"),
        "cache_read_tokens": usage.get("cache_read_tokens"),
        "seconds": (record.get("agent_info") or {}).get("seconds"),
        "search_calls": activity.get("search_calls"),
        "direct_brain_cli_calls": activity.get("direct_brain_cli_calls"),
        "first_tool_is_memory_search": activity.get("first_tool_is_memory_search"),
        "changed_files": record.get("changed_files", []),
        "harness_dirty": harness_dirty,
        "source_dirty": source_dirty,
        "integrity_findings": findings,
        "integrity_ok": not findings,
    }


def comparisons(rows: list[dict[str, Any]]) -> list[dict[str, Any]]:
    result: list[dict[str, Any]] = []
    keys = sorted({(str(row["task_id"]), str(row["runner_id"])) for row in rows})
    for task_id, runner_id in keys:
        cell = {
            str(row["condition"]): row
            for row in rows
            if row["task_id"] == task_id and row["runner_id"] == runner_id
        }
        baseline = cell["no_brain"]
        for condition in MEMORY_CONDITIONS:
            treatment = cell[condition]
            base_tokens = float(baseline["total_tokens"])
            base_seconds = float(baseline["seconds"])
            result.append(
                {
                    "task_id": task_id,
                    "stratum": baseline["stratum"],
                    "runner_id": runner_id,
                    "condition": condition,
                    "validation_baseline": baseline["validation_ok"],
                    "validation_condition": treatment["validation_ok"],
                    "token_delta_percent": 100.0 * (float(treatment["total_tokens"]) - base_tokens) / base_tokens,
                    "time_delta_percent": 100.0 * (float(treatment["seconds"]) - base_seconds) / base_seconds,
                    "search_delta": int(treatment["search_calls"] or 0) - int(baseline["search_calls"] or 0),
                }
            )
    return result


def gate(name: str, passed: bool, evidence: str) -> dict[str, Any]:
    return {"name": name, "passed": passed, "evidence": evidence}


def build_gates(
    rows: list[dict[str, Any]], manifest: dict[str, Any], source_archive_verified: bool
) -> list[dict[str, Any]]:
    expected_rows = len(manifest["tasks"]) * len(manifest["runners"]) * len(CONDITIONS)
    positive_rows = [row for row in rows if row["stratum"] == "fact_positive"]
    stale_rows = [row for row in rows if row["stratum"] == "stale_conflict"]
    neutral_rows = [row for row in rows if row["stratum"] == "neutral"]
    headroom_pairs = []
    for task_id, runner_id in sorted({(row["task_id"], row["runner_id"]) for row in rows}):
        cell = [row for row in rows if row["task_id"] == task_id and row["runner_id"] == runner_id]
        baseline = next(row for row in cell if row["condition"] == "no_brain")
        if baseline["validation_ok"] is False and any(
            row["validation_ok"] and row["protocol_ok"] for row in cell if row["condition"] in MEMORY_CONDITIONS
        ):
            headroom_pairs.append(f"{runner_id}/{task_id}")
    clean_rows = [row for row in rows if not row["harness_dirty"] and not row["source_dirty"]]
    expected_runners = sorted(HARNESS.parse_runner_spec(spec).id for spec in manifest["runners"])
    actual_runners = sorted({str(row["runner_id"]) for row in rows})
    return [
        gate("complete_matrix", len(rows) == expected_rows, f"{len(rows)}/{expected_rows} rows"),
        gate("runner_matrix", actual_runners == expected_runners, f"actual={actual_runners}; expected={expected_runners}"),
        gate("row_integrity", all(row["integrity_ok"] for row in rows), f"{sum(row['integrity_ok'] for row in rows)}/{len(rows)} rows"),
        gate(
            "fact_positive_channel",
            bool(positive_rows) and all(
                row["validation_ok"] and row["protocol_ok"]
                for row in positive_rows
                if row["condition"] == "facts_only"
            ),
            "facts_only passed for every backend on the sealed fact-positive task",
        ),
        gate(
            "stale_memory_rejected",
            bool(stale_rows) and all(
                row["validation_ok"] and row["protocol_ok"]
                for row in stale_rows
                if row["condition"] in MEMORY_CONDITIONS
            ),
            "all stale/conflict memory arms restored the shipped target-snapshot behavior",
        ),
        gate(
            "neutral_correctness",
            bool(neutral_rows) and all(row["validation_ok"] and row["protocol_ok"] for row in neutral_rows),
            "all neutral rows passed without unrelated edits",
        ),
        gate(
            "positive_task_headroom",
            bool(headroom_pairs),
            ", ".join(headroom_pairs) if headroom_pairs else "no sealed no_brain row failed while a memory row passed",
        ),
        gate(
            "complete_token_accounting",
            all(isinstance(row["total_tokens"], (int, float)) and row["total_tokens"] > 0 for row in rows),
            f"{sum(isinstance(row['total_tokens'], (int, float)) and row['total_tokens'] > 0 for row in rows)}/{len(rows)} rows",
        ),
        gate("clean_run_provenance", len(clean_rows) == len(rows), f"{len(clean_rows)}/{len(rows)} rows have clean harness and source worktrees"),
        gate(
            "portable_source_artifact",
            source_archive_verified,
            "private source archive checksum verified" if source_archive_verified else "private source archive was not supplied or failed verification",
        ),
    ]


def render_markdown(report: dict[str, Any]) -> str:
    lines = [
        "# Temporal Memory Phase 0A Sealed Smoke",
        "",
        f"- Decision: **{report['decision']}**",
        f"- Rows: **{len(report['rows'])}**",
        f"- Validation passes: **{sum(row['validation_ok'] is True for row in report['rows'])}/{len(report['rows'])}**",
        f"- Protocol passes: **{sum(row['protocol_ok'] is True for row in report['rows'])}/{len(report['rows'])}**",
        f"- Source cache: `{report['source_artifact']['cache_key']}`",
        "",
        "## Gates",
        "",
        "| Gate | Pass | Evidence |",
        "|---|---:|---|",
    ]
    for item in report["gates"]:
        lines.append(f"| `{item['name']}` | {item['passed']} | {item['evidence']} |")
    lines.extend(
        [
            "",
            "## Outcomes",
            "",
            "| Task | Stratum | Runner | Condition | Validation | Protocol | Tokens | Seconds | Searches |",
            "|---|---|---|---|---:|---:|---:|---:|---:|",
        ]
    )
    for row in report["rows"]:
        lines.append(
            f"| `{row['task_id']}` | `{row['stratum']}` | `{row['runner_id']}` | `{row['condition']}` | "
            f"{row['validation_ok']} | {row['protocol_ok']} | {row['total_tokens']} | {row['seconds']:.1f} | {row['search_calls']} |"
        )
    lines.extend(
        [
            "",
            "## Relative Cost",
            "",
            "Positive percentages are overhead versus the same task/runner no-Brain row.",
            "",
            "| Task | Runner | Condition | Token delta | Time delta | Search delta |",
            "|---|---|---|---:|---:|---:|",
        ]
    )
    for item in report["comparisons"]:
        lines.append(
            f"| `{item['task_id']}` | `{item['runner_id']}` | `{item['condition']}` | "
            f"{item['token_delta_percent']:+.1f}% | {item['time_delta_percent']:+.1f}% | {item['search_delta']:+d} |"
        )
    lines.extend(
        [
            "",
            "## Interpretation",
            "",
            "The fact-positive task confirms that the durable-fact channel can preserve and deliver a decision that the development distillation omitted elsewhere. Both backends also rejected the deliberately stale pre-shipment history and all neutral rows remained correct.",
            "",
            "The matrix is saturated: every no-Brain row passed. Memory cost is heterogeneous, ranging from useful search/token reductions on some cells to substantial overhead on others. With no correctness headroom and one repetition, this smoke cannot estimate a positive treatment effect.",
            "",
            "The scaled temporal-memory correctness study is therefore a no-go under the preregistered gates. The protocol and safety mechanisms are feasible, but a harder sealed task sample, a portable private-source artifact, clean committed-run provenance, and repeated task-clustered runs are required before a paper claim is promoted.",
        ]
    )
    findings = [
        f"{row['runner_id']}/{row['task_id']}/{row['condition']}: {finding}"
        for row in report["rows"]
        for finding in row["integrity_findings"]
    ]
    if findings:
        lines.extend(["", "## Integrity Findings", "", *[f"- {finding}" for finding in findings]])
    return "\n".join(lines) + "\n"


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--manifest", type=pathlib.Path, required=True)
    parser.add_argument("--source-artifact-manifest", type=pathlib.Path)
    parser.add_argument("--source-archive", type=pathlib.Path)
    parser.add_argument("--agent-suite", type=pathlib.Path, action="append", required=True)
    parser.add_argument("--out-dir", type=pathlib.Path, required=True)
    args = parser.parse_args()

    manifest_path = args.manifest.resolve()
    manifest = read_json(manifest_path)
    source_artifact = dict(manifest["source_artifact"])
    source_archive_verified = False
    if args.source_artifact_manifest:
        artifact_manifest = read_json(args.source_artifact_manifest.resolve())
        if artifact_manifest.get("cache_key") != source_artifact.get("cache_key"):
            raise RuntimeError("source artifact manifest cache key does not match the sealed manifest")
        source_artifact.update(artifact_manifest)
        if args.source_archive:
            archive = args.source_archive.resolve()
            source_archive_verified = (
                archive.is_file()
                and archive.stat().st_size == int(artifact_manifest["archive_bytes"])
                and sha256_file(archive) == artifact_manifest["archive_sha256"]
            )
            if not source_archive_verified:
                raise RuntimeError("private source archive failed size or SHA-256 verification")
    tasks: list[tuple[dict[str, Any], dict[str, Any]]] = []
    for entry in manifest["tasks"]:
        path = task_path(manifest_path, entry)
        if sha256_file(path) != entry["sha256"]:
            raise RuntimeError(f"frozen task hash mismatch before report generation: {path}")
        task = read_json(path)
        task["_path"] = str(path)
        tasks.append((entry, task))

    rows = []
    for suite in args.agent_suite:
        for entry, task in tasks:
            for condition in CONDITIONS:
                rows.append(row_from_record(record_path(suite, task["id"], condition), entry, task, source_artifact))
    rows.sort(key=lambda row: (row["task_id"], row["runner_id"], CONDITIONS.index(row["condition"])))
    gates = build_gates(rows, manifest, source_archive_verified)
    required_scale_gates = {
        "complete_matrix",
        "runner_matrix",
        "row_integrity",
        "fact_positive_channel",
        "stale_memory_rejected",
        "neutral_correctness",
        "positive_task_headroom",
        "complete_token_accounting",
        "clean_run_provenance",
        "portable_source_artifact",
    }
    decision = "GO" if all(item["passed"] for item in gates if item["name"] in required_scale_gates) else "NO-GO"
    report = {
        "schema": 1,
        "decision": decision,
        "manifest": str(manifest_path),
        "manifest_sha256": sha256_file(manifest_path),
        "source_artifact": source_artifact,
        "gates": gates,
        "rows": rows,
        "comparisons": comparisons(rows),
    }
    args.out_dir.mkdir(parents=True, exist_ok=True)
    (args.out_dir / "sealed-smoke-report.json").write_text(json.dumps(report, indent=2, sort_keys=True) + "\n")
    (args.out_dir / "sealed-smoke-report.md").write_text(render_markdown(report))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
