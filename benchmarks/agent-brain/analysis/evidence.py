"""Portable, content-addressed evidence bundle capture and verification."""

from __future__ import annotations

import datetime as dt
import hashlib
import json
import pathlib
import shutil
from typing import Any, Iterable

from .common import EXECUTED_RUN_PREDICATE_VERSION, is_executed_run, load_records
from .metrics import headline_table

SUITE_SCHEMA = "agent-brain-evidence-suite/v1"
RUN_SCHEMA = "agent-brain-evidence-run/v1"
REPORT_SCHEMA = "agent-brain-evidence-report/v1"
ANALYZER_AGGREGATE_ALGORITHM = "sha256_ordered_path_nul_sha256_newline_v1"
ANALYZER_RUNTIME_SOURCE_PATHS = (
    "benchmarks/agent-brain/analysis/__init__.py",
    "benchmarks/agent-brain/analysis/common.py",
    "benchmarks/agent-brain/analysis/confirmatory.py",
    "benchmarks/agent-brain/analysis/evidence.py",
    "benchmarks/agent-brain/analysis/metrics.py",
    "benchmarks/agent-brain/analysis/schemas/confirmatory-analysis-v1.schema.json",
    "benchmarks/agent-brain/analysis/schemas/run-v1.schema.json",
    "benchmarks/agent-brain/analysis/schemas/suite-v1.schema.json",
)
_ANALYSIS_SOURCE_PREFIX = pathlib.PurePosixPath("benchmarks/agent-brain/analysis")


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_file(path: pathlib.Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def write_json(path: pathlib.Path, value: Any) -> None:
    path.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n")


def manifest_identity(value: dict[str, Any]) -> str:
    payload = {key: item for key, item in value.items() if key != "identity_sha256"}
    return sha256_bytes(json.dumps(payload, sort_keys=True, separators=(",", ":")).encode())


def artifact(path: pathlib.Path, bundle_root: pathlib.Path, *, role: str) -> dict[str, Any]:
    relative = path.relative_to(bundle_root).as_posix()
    return {"role": role, "path": relative, "bytes": path.stat().st_size, "sha256": sha256_file(path)}


def retain_input(source: pathlib.Path, suite_dir: pathlib.Path, *, role: str) -> dict[str, Any]:
    data = source.read_bytes()
    digest = sha256_bytes(data)
    suffix = source.suffix if source.suffix else ".bin"
    destination = suite_dir / "evidence" / "inputs" / f"{digest}{suffix}"
    destination.parent.mkdir(parents=True, exist_ok=True)
    if not destination.exists():
        destination.write_bytes(data)
    return {**artifact(destination, suite_dir, role=role), "logical_name": source.name}


def analyzer_runtime_files(analysis_dir: pathlib.Path) -> list[tuple[str, pathlib.Path]]:
    """Resolve the explicit, ordered confirmatory runtime source/schema set."""
    files: list[tuple[str, pathlib.Path]] = []
    for source_path in ANALYZER_RUNTIME_SOURCE_PATHS:
        relative = pathlib.PurePosixPath(source_path).relative_to(_ANALYSIS_SOURCE_PREFIX)
        path = analysis_dir.joinpath(*relative.parts)
        if not path.is_file():
            raise FileNotFoundError(f"required analyzer runtime source is missing: {path}")
        files.append((source_path, path))
    return files


def analyzer_aggregate_sha256(records: Iterable[dict[str, Any]]) -> str:
    """Hash ordered ``source_path NUL content-sha newline`` records."""
    ordered: list[tuple[str, str]] = []
    for record in records:
        source_path = record.get("source_path") if isinstance(record, dict) else None
        digest = record.get("sha256") if isinstance(record, dict) else None
        if not isinstance(source_path, str) or not isinstance(digest, str):
            raise ValueError("analyzer aggregate records require source_path and sha256 strings")
        ordered.append((source_path, digest))
    ordered.sort()
    aggregate = hashlib.sha256()
    for source_path, digest in ordered:
        aggregate.update(source_path.encode("utf-8"))
        aggregate.update(b"\0")
        aggregate.update(digest.encode("ascii"))
        aggregate.update(b"\n")
    return aggregate.hexdigest()


def current_analyzer_records(analysis_dir: pathlib.Path) -> list[dict[str, str]]:
    return [
        {"source_path": source_path, "sha256": sha256_file(path)}
        for source_path, path in analyzer_runtime_files(analysis_dir)
    ]


def analyzer_artifacts(analysis_dir: pathlib.Path, suite_dir: pathlib.Path) -> list[dict[str, Any]]:
    artifacts: list[dict[str, Any]] = []
    for source_path, path in analyzer_runtime_files(analysis_dir):
        retained = retain_input(path, suite_dir, role="analyzer_source")
        retained["source_path"] = source_path
        artifacts.append(retained)
    return artifacts


def validate_analyzer_manifest(value: Any) -> list[str]:
    errors: list[str] = []
    if not isinstance(value, dict):
        return ["suite manifest analyzer must be an object"]
    if value.get("algorithm") != ANALYZER_AGGREGATE_ALGORITHM:
        errors.append("suite manifest analyzer algorithm is unsupported")
    artifacts = value.get("artifacts")
    if not isinstance(artifacts, list):
        return [*errors, "suite manifest analyzer artifacts must be a list"]
    paths = [item.get("source_path") if isinstance(item, dict) else None for item in artifacts]
    if paths != list(ANALYZER_RUNTIME_SOURCE_PATHS):
        errors.append("suite manifest analyzer runtime source set changed or is out of order")
    if any(
        not isinstance(item, dict)
        or not isinstance(item.get("sha256"), str)
        or len(item["sha256"]) != 64
        or any(character not in "0123456789abcdef" for character in item["sha256"])
        for item in artifacts
    ):
        errors.append("suite manifest analyzer artifact hash is invalid")
        return errors
    try:
        aggregate = analyzer_aggregate_sha256(artifacts)
    except (UnicodeEncodeError, ValueError):
        errors.append("suite manifest analyzer aggregate inputs are invalid")
        return errors
    if value.get("aggregate_sha256") != aggregate:
        errors.append("suite manifest analyzer aggregate hash mismatch")
    return errors


def validate_suite_manifest(value: dict[str, Any]) -> list[str]:
    errors: list[str] = []
    if value.get("schema") != SUITE_SCHEMA:
        errors.append(f"unsupported suite schema: {value.get('schema')!r}")
    for key in ("suite_id", "identity_sha256", "command", "requested_cells", "harness", "inputs", "analyzer", "runs"):
        if key not in value:
            errors.append(f"suite manifest missing {key}")
    if not isinstance(value.get("runs"), list):
        errors.append("suite manifest runs must be a list")
    errors.extend(validate_analyzer_manifest(value.get("analyzer")))
    return errors


def validate_run_manifest(value: dict[str, Any]) -> list[str]:
    errors: list[str] = []
    if value.get("schema") != RUN_SCHEMA:
        errors.append(f"unsupported run schema: {value.get('schema')!r}")
    for key in ("run_id", "identity_sha256", "executed", "artifacts", "execution_gate", "raw_metrics"):
        if key not in value:
            errors.append(f"run manifest missing {key}")
    if not isinstance(value.get("artifacts"), list):
        errors.append("run manifest artifacts must be a list")
    packet = value.get("packet")
    if isinstance(packet, dict) and packet.get("present") and packet.get("recorded_matches_file") is False:
        errors.append("run manifest packet does not match recorded packet provenance")
    return errors


def packet_details(packet_path: pathlib.Path | None, record: dict[str, Any]) -> dict[str, Any]:
    if packet_path is None or not packet_path.exists():
        return {"present": False, "bytes": 0, "sha256": None, "fact_ids": [], "kinds": []}
    data = packet_path.read_bytes()
    fact_ids: list[str] = []
    kinds: list[str] = []
    try:
        payload = json.loads(data)
        for result in payload.get("results", []) if isinstance(payload, dict) else []:
            if not isinstance(result, dict):
                continue
            identifier = result.get("id") or result.get("fact_id")
            if identifier is not None:
                fact_ids.append(str(identifier))
            if result.get("kind") is not None:
                kinds.append(str(result["kind"]))
    except (UnicodeDecodeError, json.JSONDecodeError):
        pass
    delivery = record.get("memory_delivery") if isinstance(record.get("memory_delivery"), dict) else {}
    recorded = record.get("packet_artifact") if isinstance(record.get("packet_artifact"), dict) else {}
    treatment = record.get("treatment") if isinstance(record.get("treatment"), dict) else {}
    digest = sha256_bytes(data)
    recorded_ids = recorded.get("fact_ids") if isinstance(recorded.get("fact_ids"), list) else None
    recorded_matches = (
        recorded.get("sha256") in (None, digest)
        and recorded.get("bytes") in (None, len(data))
        and (recorded_ids is None or [str(item) for item in recorded_ids] == fact_ids)
    )
    return {
        "present": True,
        "bytes": len(data),
        "sha256": digest,
        "fact_ids": fact_ids,
        "kinds": kinds,
        "query_source_class": (
            record.get("retrieval_query_source")
            or delivery.get("query_source_class")
            or "legacy_brain_queries"
        ),
        "treatment_arm": treatment.get("arm"),
        "recorded_matches_file": recorded_matches,
    }


def temporal_eligibility_evidence(record: dict[str, Any]) -> dict[str, Any] | None:
    delivery = record.get("memory_delivery") if isinstance(record.get("memory_delivery"), dict) else {}
    packet = record.get("packet_artifact") if isinstance(record.get("packet_artifact"), dict) else {}
    prep = record.get("brain_prep") if isinstance(record.get("brain_prep"), dict) else {}
    for candidate in (
        packet.get("temporal_eligibility"),
        delivery.get("temporal_eligibility"),
        prep.get("temporal_eligibility"),
    ):
        if isinstance(candidate, dict):
            return candidate
    return None


def retrieval_arm_evidence(record: dict[str, Any]) -> dict[str, Any] | None:
    delivery = record.get("memory_delivery") if isinstance(record.get("memory_delivery"), dict) else {}
    packet = record.get("packet_artifact") if isinstance(record.get("packet_artifact"), dict) else {}
    treatment = record.get("treatment") if isinstance(record.get("treatment"), dict) else {}
    if not delivery and not packet and not treatment:
        return None
    sources = delivery.get("sources") if isinstance(delivery.get("sources"), dict) else {}
    retrieval = delivery.get("retrieval") if isinstance(delivery.get("retrieval"), dict) else {}
    engine = retrieval.get("engine") if isinstance(retrieval.get("engine"), dict) else {}
    eligibility = temporal_eligibility_evidence(record) or {}
    evidence = {
        "condition": record.get("condition"),
        "treatment_arm": treatment.get("arm"),
        "query_source_class": record.get("retrieval_query_source") or delivery.get("query_source_class"),
        "packet_sha256": packet.get("sha256") or (retrieval.get("packet") or {}).get("sha256"),
        "packet_fact_ids": packet.get("fact_ids") or [],
        "packet_fact_count": packet.get("fact_count"),
        "frozen_root_logical_id": sources.get("source_cache_key") or sources.get("prep_cache_key"),
        "facts_file_sha256": sources.get("facts_file_sha256"),
        "fact_artifact_sha256": sources.get("fact_artifact_sha256") or [],
        "active_fact_count": sources.get("active_fact_count"),
        "superseded_fact_count": sources.get("superseded_fact_count"),
        "session_date_map_sha256": sources.get("session_date_map_sha256"),
        "embedder_id": engine.get("embedder_id") or retrieval.get("embedder_id"),
        "embedding_dimension": engine.get("dimension") or retrieval.get("embedding_dimension"),
        "vector_count": engine.get("vector_count") or retrieval.get("vector_count"),
        "vector_artifact_sha256": engine.get("vector_artifact_sha256") or retrieval.get("vector_artifact_sha256"),
        "prefilter_corpus_count": eligibility.get("prefilter_corpus_count"),
        "eligible_count": eligibility.get("eligible_count"),
        "excluded_counts": eligibility.get("excluded_counts"),
        "delivered_count": eligibility.get("delivered_count") or (retrieval.get("packet") or {}).get("delivered_result_count"),
    }
    raw = {
        "delivery": delivery,
        "packet_artifact": packet,
        "retrieval_query_source": record.get("retrieval_query_source"),
        "treatment": treatment,
    }
    evidence["raw_delivery_sha256"] = sha256_bytes(
        json.dumps(raw, sort_keys=True, separators=(",", ":")).encode()
    )
    return evidence


def build_run_manifest(record: dict[str, Any], run_dir: pathlib.Path, suite_dir: pathlib.Path) -> dict[str, Any]:
    artifacts: list[dict[str, Any]] = []
    for name, role in (
        ("prompt.txt", "agent_prompt"),
        ("packet.txt", "delivered_packet"),
        ("agent.patch", "agent_patch"),
        ("agent.stdout", "agent_stdout"),
        ("agent.stderr", "agent_stderr"),
        ("memory-delivery.json", "memory_delivery"),
    ):
        path = run_dir / name
        if path.exists():
            artifacts.append(artifact(path, suite_dir, role=role))
    prompt = run_dir / "prompt.txt"
    packet_artifact = record.get("packet_artifact") if isinstance(record.get("packet_artifact"), dict) else {}
    packet_name = packet_artifact.get("path") or "packet.txt"
    packet = run_dir / str(packet_name)
    try:
        packet.resolve().relative_to(run_dir.resolve())
    except ValueError as exc:
        raise ValueError(f"packet artifact escapes run directory: {packet_name}") from exc
    agent_info = record.get("agent_info") if isinstance(record.get("agent_info"), dict) else {}
    usage = agent_info.get("usage") if isinstance(agent_info.get("usage"), dict) else {}
    validation = record.get("validation") if isinstance(record.get("validation"), dict) else {}
    provenance = record.get("provenance") if isinstance(record.get("provenance"), dict) else {}
    tools = provenance.get("tools") if isinstance(provenance.get("tools"), dict) else {}
    timing = record.get("timing") if isinstance(record.get("timing"), dict) else {}
    primary_duration = timing.get("harness_agent_interval_wall_seconds")
    if not isinstance(primary_duration, (int, float)):
        primary_duration = agent_info.get("seconds")
    retrieval_evidence = retrieval_arm_evidence(record)
    return {
        "schema": RUN_SCHEMA,
        "run_id": record.get("run_id"),
        "task_id": record.get("task_id"),
        "condition": record.get("condition"),
        "runner": record.get("runner"),
        "resolved_model": agent_info.get("resolved_model"),
        "reasoning_effort": (record.get("runner") or {}).get("effort"),
        "service_tier": usage.get("service_tier"),
        "isolation": agent_info.get("isolation"),
        "executed": is_executed_run(record),
        "prompt_sha256": sha256_file(prompt) if prompt.exists() else None,
        "packet": packet_details(packet if packet.exists() else None, record),
        "temporal_eligibility": temporal_eligibility_evidence(record),
        "retrieval_evidence": retrieval_evidence,
        "binary_hashes": {name: item.get("sha256") for name, item in sorted(tools.items()) if isinstance(item, dict)},
        "execution_gate": {
            "agent_ran": record.get("agent_ran"),
            "duration_seconds": primary_duration,
            "agent_reported_seconds": agent_info.get("seconds"),
            "total_tokens": usage.get("total_tokens"),
            "error": record.get("error"),
            "return_code": agent_info.get("returncode"),
            "validation_ok": validation.get("ok"),
            "record_ok": record.get("ok"),
        },
        "patch_sha256": (record.get("patch_artifact") or {}).get("sha256"),
        "result_sha256": hashlib.sha256(json.dumps(record, sort_keys=True, separators=(",", ":")).encode()).hexdigest(),
        "raw_metrics": {
            "score": record.get("score"),
            "usage": usage,
            "duration_seconds": primary_duration,
            "timing": timing,
        },
        "artifacts": artifacts,
    }


def write_run_manifest(record: dict[str, Any], run_dir: pathlib.Path, suite_dir: pathlib.Path) -> dict[str, Any]:
    value = build_run_manifest(record, run_dir, suite_dir)
    value["identity_sha256"] = manifest_identity(value)
    write_json(run_dir / "evidence-manifest.json", value)
    return value


def iter_artifacts(value: Any) -> Iterable[dict[str, Any]]:
    if isinstance(value, dict):
        if isinstance(value.get("path"), str) and isinstance(value.get("sha256"), str):
            yield value
        for child in value.values():
            yield from iter_artifacts(child)
    elif isinstance(value, list):
        for child in value:
            yield from iter_artifacts(child)


def _safe_bundle_path(root: pathlib.Path, relative: str) -> pathlib.Path:
    path = (root / relative).resolve()
    try:
        path.relative_to(root.resolve())
    except ValueError as exc:
        raise ValueError(f"artifact escapes evidence bundle: {relative}") from exc
    return path


def verify_bundle(suite_dir: pathlib.Path) -> dict[str, Any]:
    suite_dir = suite_dir.resolve()
    manifest_path = suite_dir / "evidence-manifest.json"
    suite = json.loads(manifest_path.read_text())
    errors = validate_suite_manifest(suite)
    if suite.get("identity_sha256") != manifest_identity(suite):
        errors.append("suite manifest identity mismatch")
    checked = 0
    for item in iter_artifacts(suite):
        try:
            path = _safe_bundle_path(suite_dir, item["path"])
        except ValueError as exc:
            errors.append(str(exc))
            continue
        if not path.is_file():
            errors.append(f"missing artifact: {item['path']}")
            continue
        checked += 1
        if path.stat().st_size != item.get("bytes"):
            errors.append(f"size mismatch: {item['path']}")
        if sha256_file(path) != item["sha256"]:
            errors.append(f"hash mismatch: {item['path']}")
    run_manifests: list[dict[str, Any]] = []
    for run_ref in suite.get("runs", []):
        relative = run_ref.get("manifest") if isinstance(run_ref, dict) else None
        if not isinstance(relative, str):
            errors.append("run reference missing manifest")
            continue
        try:
            path = _safe_bundle_path(suite_dir, relative)
            value = json.loads(path.read_text())
        except (OSError, json.JSONDecodeError, ValueError) as exc:
            errors.append(f"invalid run manifest {relative}: {exc}")
            continue
        errors.extend(f"{relative}: {error}" for error in validate_run_manifest(value))
        if value.get("identity_sha256") != manifest_identity(value):
            errors.append(f"{relative}: manifest identity mismatch")
        run_manifests.append(value)
        for item in value.get("artifacts", []):
            try:
                artifact_path = _safe_bundle_path(suite_dir, item["path"])
                if not artifact_path.is_file() or sha256_file(artifact_path) != item.get("sha256"):
                    errors.append(f"run artifact mismatch: {item.get('path')}")
                else:
                    checked += 1
            except (KeyError, ValueError) as exc:
                errors.append(f"invalid run artifact: {exc}")
    try:
        records = load_records(suite_dir)
    except (OSError, ValueError, json.JSONDecodeError) as exc:
        errors.append(f"invalid raw records: {exc}")
        records = []
    record_ids = [record.get("run_id") for record in records]
    manifest_ids = [manifest.get("run_id") for manifest in run_manifests]
    if record_ids != manifest_ids:
        errors.append("run manifest order/IDs do not match records.ndjson")
    for record, manifest in zip(records, run_manifests):
        digest = hashlib.sha256(
            json.dumps(record, sort_keys=True, separators=(",", ":")).encode()
        ).hexdigest()
        if digest != manifest.get("result_sha256"):
            errors.append(f"record hash mismatch: {record.get('run_id')}")
    report = headline_table(records)
    return {"ok": not errors, "schema": REPORT_SCHEMA, "suite_id": suite.get("suite_id"), "artifacts_checked": checked, "errors": errors, "headline": report}


def render_markdown(verification: dict[str, Any]) -> str:
    headline = verification["headline"]
    lines = [
        f"# Evidence report: {verification.get('suite_id')}",
        "",
        f"Verification: **{'PASS' if verification.get('ok') else 'FAIL'}**",
        "",
        f"Estimand: `{headline['estimand']}`",
        "",
        "| Task | Runner | Delivery | Condition | Executed | Passed | Mean tokens | Mean seconds |",
        "|---|---|---|---|---:|---:|---:|---:|",
    ]
    for row in headline["arms"]:
        tokens = "—" if row["mean_total_tokens_all_executed_with_measurement"] is None else f"{row['mean_total_tokens_all_executed_with_measurement']:.1f}"
        seconds = "—" if row["mean_agent_seconds_all_executed_with_measurement"] is None else f"{row['mean_agent_seconds_all_executed_with_measurement']:.1f}"
        lines.append(f"| {row['task_id']} | {row['runner']} | {row['delivery_mode']} | {row['condition']} | {row['executed_attempts']} | {row['validation_passes']} | {tokens} | {seconds} |")
    if verification.get("errors"):
        lines.extend(["", "## Verification errors", ""] + [f"- {error}" for error in verification["errors"]])
    return "\n".join(lines) + "\n"


def finalize_suite_manifest(
    suite_dir: pathlib.Path,
    *,
    suite_id: str,
    command: list[str],
    requested_cells: dict[str, Any],
    harness: dict[str, Any],
    tasks: list[dict[str, Any]],
    analysis_dir: pathlib.Path,
    tools: dict[str, pathlib.Path] | None = None,
    validation_fixture_dir: pathlib.Path | None = None,
) -> dict[str, Any]:
    inputs: list[dict[str, Any]] = []
    seen: set[tuple[str, str]] = set()
    for task in tasks:
        task_path = pathlib.Path(str(task.get("_path") or ""))
        candidates = [(task_path, "task_config")]
        parent_ledger = task_path.parent.parent / "selection-ledger.json"
        if parent_ledger.exists():
            candidates.append((parent_ledger, "selection_ledger"))
        dates = task.get("frozen_session_dates_path")
        if dates:
            candidates.append((pathlib.Path(str(dates)), "session_date_map"))
        if validation_fixture_dir is not None:
            for entry in task.get("validation_files", []):
                if isinstance(entry, dict) and entry.get("fixture"):
                    candidates.append((validation_fixture_dir / str(entry["fixture"]), "hidden_validation_fixture"))
        for source, role in candidates:
            if source.is_file() and (role, str(source.resolve())) not in seen:
                inputs.append(retain_input(source, suite_dir, role=role))
                seen.add((role, str(source.resolve())))
    for name, path in sorted((tools or {}).items()):
        if path.is_file():
            inputs.append(retain_input(path, suite_dir, role=f"binary:{name}"))
    records = load_records(suite_dir)
    runs = []
    for record in records:
        relative = pathlib.Path(str(record["run_id"])) / "evidence-manifest.json"
        runs.append({"run_id": record["run_id"], "manifest": relative.as_posix()})
    analyzers = analyzer_artifacts(analysis_dir, suite_dir)
    suite_artifacts = []
    for name, role in (
        ("schedule.json", "planned_schedule"),
        ("actual-order.ndjson", "actual_execution_order"),
        ("schedule-state.json", "schedule_state"),
        ("runtime-controls.json", "runtime_controls"),
        ("prompt-snapshots.json", "treatment_prompt_snapshots"),
        ("harness-evidence.json", "harness_identity"),
        ("harness.patch", "harness_patch"),
    ):
        path = suite_dir / name
        if path.is_file():
            suite_artifacts.append(artifact(path, suite_dir, role=role))
    runtime = []
    retrieval = []
    sources = []
    for record in records:
        agent_info = record.get("agent_info") if isinstance(record.get("agent_info"), dict) else {}
        runtime.append(
            {
                "runner": record.get("runner"),
                "resolved_model": agent_info.get("resolved_model"),
                "isolation": agent_info.get("isolation"),
                "timing": record.get("timing"),
                "runtime_controls": record.get("runtime_controls"),
            }
        )
        arm_evidence = retrieval_arm_evidence(record)
        if arm_evidence is not None:
            retrieval.append(arm_evidence)
        provenance = record.get("provenance") if isinstance(record.get("provenance"), dict) else {}
        if isinstance(provenance.get("source"), dict):
            source = provenance["source"]
            sources.append(
                {
                    "repo": source.get("repo"),
                    "base_commit": (source.get("base") or {}).get("commit"),
                    "head_commit": (source.get("head") or {}).get("commit"),
                }
            )
    unique = lambda values: [json.loads(item) for item in sorted({json.dumps(value, sort_keys=True, default=str) for value in values})]
    value = {
        "schema": SUITE_SCHEMA,
        "suite_id": suite_id,
        "created_at": dt.datetime.now(dt.UTC).isoformat(),
        "command": command,
        "requested_cells": requested_cells,
        "schedule": requested_cells.get("schedule") or {"seed": None, "order_policy": "legacy_task_runner_condition"},
        "cache_policy": requested_cells.get("cache_policy") or "legacy_shared_or_per-run_flags; see requested_cells.isolation_flags",
        "harness": harness,
        "source_repositories": unique(sources),
        "runtime": unique(runtime),
        "retrieval_arms": unique(retrieval),
        "suite_artifacts": suite_artifacts,
        "inputs": inputs,
        "analyzer": {
            "predicate_version": EXECUTED_RUN_PREDICATE_VERSION,
            "estimand": "all executed attempts; correctness and efficiency separate",
            "algorithm": ANALYZER_AGGREGATE_ALGORITHM,
            "source_set": "explicit_confirmatory_runtime_v1",
            "artifacts": analyzers,
            "aggregate_sha256": analyzer_aggregate_sha256(analyzers),
        },
        "runs": runs,
        "records": artifact(suite_dir / "records.ndjson", suite_dir, role="raw_records"),
    }
    value["identity_sha256"] = manifest_identity(value)
    write_json(suite_dir / "evidence-manifest.json", value)
    return value
