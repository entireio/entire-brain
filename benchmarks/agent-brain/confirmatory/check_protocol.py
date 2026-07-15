#!/usr/bin/env python3
"""Offline consistency and freeze-gate checks for the WS6 protocol artifacts."""

from __future__ import annotations

import argparse
import copy
from datetime import datetime, timedelta, timezone
from decimal import Decimal
import hashlib
import json
import pathlib
import re
import sys
from typing import Any


HERE = pathlib.Path(__file__).resolve().parent
REPO = HERE.parents[2]
C0701 = REPO / "benchmarks" / "agent-brain" / "mined-c0701"
if str(HERE) not in sys.path:
    sys.path.insert(0, str(HERE))

import power_analysis  # noqa: E402  (local deterministic companion module)
import pricing_budget  # noqa: E402  (local deterministic companion module)


SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
COMMIT_RE = re.compile(r"^[0-9a-f]{40}$")
STATES = ["unseen", "prompt_inspected", "retrieval_probed", "agent_run", "optimization_used"]
ARMS = ["lexical_handrolled", "model2vec_rrf", "embeddinggemma_rrf"]
BASE_COMMIT = "bbe1bdf5be2e4fac2f81dc9156a3118a2733c360"
SOURCE_COMMITS = {
    "ws2": "0c04088d8a9d70ed15c21a90979caebc5a70d2dc",
    "ws3": "0fc3a7d891a3b7106cdb4dd88023b8606886c4db",
    "ws4": "05708ca7e5692f4aae0c066d6f96aa822f675e5c",
    "ws5": "5112d72e95180dc166bcfeab4a1ce48ee6f67641",
}
DEPENDENCY_CHECKS = {
    "ws2_treatment_and_task_validity": "ws2_treatment_contract_integrated",
    "ws3_temporal_completeness": "ws3_temporal_preranking_contract",
    "ws4_schedule_and_cache": "ws4_schedule_cache_controls",
    "ws5_evidence_verification": "ws5_evidence_verification",
}
GO_NO_GO_IDS = [
    "ws2_treatment_contract_integrated",
    "fresh_task_validity_review",
    "ws3_temporal_preranking_contract",
    "ws4_schedule_cache_controls",
    "ws5_evidence_verification",
    "unique_inventory_reconciled",
    "fresh_holdout_hash_frozen",
    "holdout_unopened",
    "analyzer_hash_frozen",
    "all_engines_machine_verified",
    "offline_development_threshold",
    "power_target_met",
    "model_runner_price_pinned",
    "paid_budget_cap_approved",
    "protocol_content_hash_frozen",
]
ANALYZER_LOCK_ALGORITHM = "sha256_ordered_path_nul_sha256_newline_v1"


def load(path: pathlib.Path) -> Any:
    with path.open(encoding="utf-8") as handle:
        return json.load(handle)


def digest(path: pathlib.Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def _error(errors: list[str], condition: bool, message: str) -> None:
    if not condition:
        errors.append(message)


def _is_sha256(value: Any) -> bool:
    return isinstance(value, str) and SHA256_RE.fullmatch(value) is not None


def _is_commit(value: Any) -> bool:
    return isinstance(value, str) and COMMIT_RE.fullmatch(value) is not None


def _is_nonnegative_int(value: Any) -> bool:
    return isinstance(value, int) and not isinstance(value, bool) and value >= 0


def _unique_strings(values: list[Any]) -> bool:
    return all(isinstance(value, str) for value in values) and len(values) == len(set(values))


def _all_equal(values: list[Any]) -> bool:
    return not values or all(value == values[0] for value in values[1:])


def canonical_json_sha256(value: Any) -> str:
    """Hash the UTF-8 RFC-8259-style canonical encoding used by this protocol."""
    encoded = json.dumps(
        value,
        ensure_ascii=False,
        allow_nan=False,
        sort_keys=True,
        separators=(",", ":"),
    ).encode("utf-8")
    return hashlib.sha256(encoded).hexdigest()


def protocol_content_sha256(protocol: dict[str, Any]) -> str:
    """Hash a protocol without making its content hash self-referential."""
    canonical = copy.deepcopy(protocol)
    freeze_info = canonical.setdefault("freeze", {})
    freeze_info["protocol_sha256"] = None
    return canonical_json_sha256(canonical)


def analyzer_aggregate_sha256(files: list[tuple[str, str]]) -> str:
    """Hash ordered ``path NUL content-sha newline`` lock records."""
    aggregate = hashlib.sha256()
    for path, sha256 in files:
        aggregate.update(path.encode("utf-8"))
        aggregate.update(b"\0")
        aggregate.update(sha256.encode("ascii"))
        aggregate.update(b"\n")
    return aggregate.hexdigest()


def _safe_relative_path(raw: Any, base: pathlib.Path) -> pathlib.Path | None:
    if not isinstance(raw, str) or not raw:
        return None
    relative = pathlib.Path(raw)
    if relative.is_absolute() or ".." in relative.parts:
        return None
    candidate = (base / relative).resolve()
    try:
        candidate.relative_to(base.resolve())
    except ValueError:
        return None
    return candidate


def _evidence_path(evidence: Any, here: pathlib.Path = HERE, repo: pathlib.Path = REPO) -> pathlib.Path | None:
    if not isinstance(evidence, str) or not evidence:
        return None
    raw_path = evidence.split("#", 1)[0]
    relative = pathlib.Path(raw_path)
    base = repo if relative.parts and relative.parts[0] == "benchmarks" else here
    return _safe_relative_path(raw_path, base)


def _load_artifact(path: pathlib.Path, errors: list[str], label: str) -> Any | None:
    try:
        return load(path)
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        errors.append(f"{label} is not valid readable JSON: {exc}")
        return None


def _required_object_fields(errors: list[str], value: Any, fields: tuple[str, ...], label: str) -> bool:
    if not isinstance(value, dict):
        errors.append(f"{label} must be an object")
        return False
    for field in fields:
        _error(errors, field in value, f"{label}: missing required field {field}")
    return all(field in value for field in fields)


def validate_gate_evidence(
    gate: dict[str, Any],
    *,
    here: pathlib.Path = HERE,
    repo: pathlib.Path = REPO,
) -> tuple[list[str], dict[str, dict[str, Any]]]:
    errors: list[str] = []
    checks = gate.get("checks", [])
    if not isinstance(checks, list):
        return ["go/no-go checks must be an array"], {}
    ids = [item.get("id") if isinstance(item, dict) else None for item in checks]
    _error(errors, ids == GO_NO_GO_IDS, "go/no-go check ids changed, reordered, or are incomplete")
    _error(errors, _unique_strings(ids), "go/no-go check ids are not unique strings")
    by_id: dict[str, dict[str, Any]] = {}
    for item in checks:
        if not isinstance(item, dict):
            errors.append("go/no-go check must be an object")
            continue
        check_id = item.get("id")
        if isinstance(check_id, str):
            by_id[check_id] = item
        status = item.get("status")
        _error(errors, status in {"pending", "pass", "fail"}, f"{check_id}: invalid go/no-go status")
        evidence = item.get("evidence")
        if status in {"pass", "fail"}:
            _error(errors, isinstance(evidence, str) and bool(evidence), f"{check_id}: decided check has no evidence")
        if evidence is not None:
            target = _evidence_path(evidence, here, repo)
            _error(errors, target is not None, f"{check_id}: evidence path is unsafe or invalid")
            if target is not None:
                _error(errors, target.exists(), f"{check_id}: evidence path does not exist: {evidence.split('#', 1)[0]}")
    expected_decision = "go" if checks and all(
        isinstance(item, dict) and item.get("status") == "pass" for item in checks
    ) else "no_go"
    _error(errors, gate.get("decision") == expected_decision, "go/no-go decision does not match checklist")
    return errors, by_id


def _verify_hashed_file(
    errors: list[str],
    record: Any,
    label: str,
    *,
    repo: pathlib.Path = REPO,
) -> str | None:
    if not _required_object_fields(errors, record, ("path", "sha256"), label):
        return None
    raw_path = record.get("path")
    target = _safe_relative_path(raw_path, repo)
    _error(errors, target is not None, f"{label}: path must be safe and repo-relative")
    _error(errors, _is_sha256(record.get("sha256")), f"{label}: sha256 is invalid")
    if target is None:
        return None
    _error(errors, target.is_file(), f"{label}: file does not exist: {raw_path}")
    if target.is_file() and _is_sha256(record.get("sha256")):
        _error(errors, digest(target) == record["sha256"], f"{label}: content hash mismatch: {raw_path}")
    return raw_path if isinstance(raw_path, str) else None


def validate_integration_verification(
    protocol: dict[str, Any],
    checks: dict[str, dict[str, Any]],
    *,
    here: pathlib.Path = HERE,
    repo: pathlib.Path = REPO,
) -> list[str]:
    errors: list[str] = []
    dependencies = protocol.get("dependencies", {})
    for dependency, check_id in DEPENDENCY_CHECKS.items():
        dependency_status = dependencies.get(dependency)
        check_status = checks.get(check_id, {}).get("status")
        _error(errors, dependency_status in {"pending", "pass", "fail"}, f"{dependency}: invalid dependency status")
        _error(
            errors,
            (dependency_status == "pass") == (check_status == "pass"),
            f"{dependency}: protocol and go/no-go pass status differ",
        )

    claimed = [name for name in DEPENDENCY_CHECKS if dependencies.get(name) == "pass"]
    artifact_path = here / "integration-verification.json"
    if not artifact_path.exists() and not claimed:
        return errors
    _error(errors, artifact_path.is_file(), "integration-verification.json is required when a dependency passes")
    if not artifact_path.is_file():
        return errors
    artifact = _load_artifact(artifact_path, errors, "integration-verification.json")
    if not isinstance(artifact, dict):
        return errors

    required = (
        "schema_version",
        "base_commit",
        "verified_commit",
        "source_commits",
        "source_artifacts",
        "test_runs",
        "dependency_evidence",
        "entire_graph_modified",
        "paid_runs_performed",
    )
    _required_object_fields(errors, artifact, required, "integration-verification.json")
    _error(errors, artifact.get("schema_version") == 1, "integration verification schema_version must be 1")
    _error(errors, artifact.get("base_commit") == BASE_COMMIT, "integration verification base commit changed")
    _error(errors, _is_commit(artifact.get("verified_commit")), "integration verified_commit must be a 40-hex commit")
    _error(errors, artifact.get("entire_graph_modified") is False, "integration verification must record entire_graph_modified=false")
    _error(errors, artifact.get("paid_runs_performed") is False, "integration verification must record paid_runs_performed=false")

    source_commits = artifact.get("source_commits")
    if not isinstance(source_commits, dict):
        errors.append("integration source_commits must be an object")
        source_commits = {}
    for key, expected in SOURCE_COMMITS.items():
        _error(errors, source_commits.get(key) == expected, f"integration source commit mismatch: {key}")
    _error(errors, set(source_commits).issubset({*SOURCE_COMMITS, "ws6", "ws7"}), "integration source_commits has an unknown workstream")
    for key in (set(source_commits) - set(SOURCE_COMMITS)):
        _error(errors, _is_commit(source_commits.get(key)), f"integration source commit is invalid: {key}")

    source_records = artifact.get("source_artifacts")
    if not isinstance(source_records, list):
        errors.append("integration source_artifacts must be an array")
        source_records = []
    source_paths: list[str] = []
    for index, record in enumerate(source_records):
        path = _verify_hashed_file(errors, record, f"source_artifacts[{index}]", repo=repo)
        if path is not None:
            source_paths.append(path)
    _error(errors, _unique_strings(source_paths), "integration source artifact paths are not unique")

    test_records = artifact.get("test_runs")
    if not isinstance(test_records, list):
        errors.append("integration test_runs must be an array")
        test_records = []
    test_ids: list[str] = []
    for index, record in enumerate(test_records):
        label = f"test_runs[{index}]"
        if not _required_object_fields(errors, record, ("id", "command", "status", "log_path", "log_sha256"), label):
            continue
        test_id = record.get("id")
        _error(errors, isinstance(test_id, str) and bool(test_id), f"{label}: id is missing")
        _error(errors, isinstance(record.get("command"), str) and bool(record.get("command")), f"{label}: command is missing")
        _error(errors, record.get("status") == "pass", f"{label}: test status must be pass")
        log_record = {"path": record.get("log_path"), "sha256": record.get("log_sha256")}
        _verify_hashed_file(errors, log_record, f"{label} log", repo=repo)
        if isinstance(test_id, str):
            test_ids.append(test_id)
    _error(errors, _unique_strings(test_ids), "integration test run ids are not unique")

    dependency_evidence = artifact.get("dependency_evidence")
    if not isinstance(dependency_evidence, dict):
        errors.append("integration dependency_evidence must be an object")
        dependency_evidence = {}
    _error(errors, set(dependency_evidence) == set(DEPENDENCY_CHECKS), "integration dependency_evidence keys must exactly match protocol dependencies")
    for dependency in DEPENDENCY_CHECKS:
        evidence = dependency_evidence.get(dependency)
        label = f"dependency_evidence.{dependency}"
        if not _required_object_fields(errors, evidence, ("source_artifact_paths", "test_run_ids"), label):
            continue
        referenced_sources = evidence.get("source_artifact_paths")
        referenced_tests = evidence.get("test_run_ids")
        _error(errors, isinstance(referenced_sources, list) and bool(referenced_sources), f"{label}: source_artifact_paths must be non-empty")
        _error(errors, isinstance(referenced_tests, list) and bool(referenced_tests), f"{label}: test_run_ids must be non-empty")
        if isinstance(referenced_sources, list):
            _error(errors, _unique_strings(referenced_sources), f"{label}: source artifact references must be unique strings")
            for path in referenced_sources:
                _error(errors, path in source_paths, f"{label}: unknown source artifact path: {path}")
        if isinstance(referenced_tests, list):
            _error(errors, _unique_strings(referenced_tests), f"{label}: test run references must be unique strings")
            for test_id in referenced_tests:
                _error(errors, test_id in test_ids, f"{label}: unknown test run id: {test_id}")

    for dependency in claimed:
        check = checks.get(DEPENDENCY_CHECKS[dependency], {})
        target = _evidence_path(check.get("evidence"), here, repo)
        _error(errors, target == artifact_path.resolve(), f"{dependency}: pass evidence must reference integration-verification.json")
    return errors


def validate_analyzer_lock(
    protocol: dict[str, Any],
    check: dict[str, Any],
    *,
    here: pathlib.Path = HERE,
    repo: pathlib.Path = REPO,
) -> list[str]:
    errors: list[str] = []
    artifact_path = here / "analyzer-lock.json"
    claimed = check.get("status") == "pass"
    protocol_hash = protocol.get("analyzer_sha256")
    if not artifact_path.exists() and not claimed and protocol_hash is None:
        return errors
    _error(errors, artifact_path.is_file(), "analyzer-lock.json is required when analyzer hash is frozen")
    if not artifact_path.is_file():
        return errors
    artifact = _load_artifact(artifact_path, errors, "analyzer-lock.json")
    if not isinstance(artifact, dict):
        return errors
    _required_object_fields(
        errors,
        artifact,
        ("schema_version", "algorithm", "files", "aggregate_sha256"),
        "analyzer-lock.json",
    )
    _error(errors, artifact.get("schema_version") == 1, "analyzer lock schema_version must be 1")
    _error(errors, artifact.get("algorithm") == ANALYZER_LOCK_ALGORITHM, "analyzer lock algorithm changed")
    records = artifact.get("files")
    if not isinstance(records, list):
        errors.append("analyzer lock files must be an array")
        records = []
    _error(errors, bool(records), "analyzer lock must contain at least one file")
    locked: list[tuple[str, str]] = []
    for index, record in enumerate(records):
        path = _verify_hashed_file(errors, record, f"analyzer files[{index}]", repo=repo)
        if path is not None and isinstance(record, dict) and _is_sha256(record.get("sha256")):
            locked.append((path, record["sha256"]))
    paths = [path for path, _ in locked]
    _error(errors, paths == sorted(paths), "analyzer lock paths must be in lexical order")
    _error(errors, len(paths) == len(set(paths)), "analyzer lock paths are not unique")
    aggregate = analyzer_aggregate_sha256(locked)
    _error(errors, _is_sha256(artifact.get("aggregate_sha256")), "analyzer aggregate_sha256 is invalid")
    _error(errors, artifact.get("aggregate_sha256") == aggregate, "analyzer aggregate hash mismatch")
    if claimed or protocol_hash is not None:
        _error(errors, protocol_hash == aggregate, "preregistration analyzer_sha256 does not match analyzer lock")
    if claimed:
        target = _evidence_path(check.get("evidence"), here, repo)
        _error(errors, target == artifact_path.resolve(), "analyzer pass evidence must reference analyzer-lock.json")
    return errors


def validate_power_analysis(
    protocol: dict[str, Any],
    check: dict[str, Any],
    *,
    here: pathlib.Path = HERE,
    repo: pathlib.Path = REPO,
) -> list[str]:
    errors: list[str] = []
    artifact_path = here / "power-analysis.json"
    completed = protocol.get("agent_design", {}).get("power", {}).get("completed") is True
    decided = check.get("status") in {"pass", "fail"}
    if not artifact_path.exists() and not completed and not decided:
        return errors
    _error(errors, artifact_path.is_file(), "power-analysis.json is required when power is complete")
    if not artifact_path.is_file():
        return errors
    artifact = _load_artifact(artifact_path, errors, "power-analysis.json")
    if not isinstance(artifact, dict):
        return errors
    try:
        expected = power_analysis.build_report()
    except (OSError, UnicodeDecodeError, json.JSONDecodeError, ValueError) as exc:
        errors.append(f"cannot derive power-analysis.json: {exc}")
        return errors
    _error(errors, artifact == expected, "power-analysis.json is stale or does not match power_analysis.build_report()")
    decision_passed = artifact.get("decision", {}).get("passed") is True
    status_passed = artifact.get("status") == "pass"
    _error(errors, decision_passed == status_passed, "power artifact status and decision disagree")
    power = protocol.get("agent_design", {}).get("power", {})
    _error(errors, completed, "checked-in power analysis requires protocol power.completed=true")
    protocol_evidence = _evidence_path(power.get("evidence"), here, repo)
    _error(errors, protocol_evidence == artifact_path.resolve(), "protocol power evidence must reference power-analysis.json")
    _error(errors, power.get("analysis_kind") == artifact.get("analysis_kind"), "protocol power analysis_kind does not match artifact")
    if artifact.get("schema_version") == 2:
        _error(
            errors,
            power.get("design_options_evidence") == "power-analysis.json#design_options",
            "protocol design_options_evidence must reference power-analysis.json#design_options",
        )
        artifact_calibration = artifact.get("exploratory_calibration", {})
        protocol_calibration = power.get("exploratory_calibration", {})
        expected_calibration = {
            "manifest": pathlib.Path(str(artifact_calibration.get("manifest_path") or "")).name,
            "eligibility": "exploratory_only",
            "confirmatory_assumption_source": False,
            "unique_task_ids": artifact_calibration.get("unique_task_ids_across_sources"),
            "paired_task_cluster_instances": artifact_calibration.get(
                "paired_task_cluster_instances"
            ),
            "pooled_estimate_prohibited": artifact_calibration.get(
                "pooled_estimate_prohibited"
            ),
        }
        _error(
            errors,
            protocol_calibration == expected_calibration,
            "protocol exploratory calibration summary does not match power artifact",
        )
    protocol_status = power.get("status")
    _error(
        errors,
        isinstance(protocol_status, str)
        and (protocol_status == "pass" if decision_passed else protocol_status.startswith("fail")),
        "protocol power status does not match deterministic decision",
    )
    _error(
        errors,
        power.get("design_decision_required") is (not decision_passed),
        "protocol design_decision_required does not match deterministic decision",
    )
    expected_gate_status = "pass" if decision_passed else "fail"
    _error(errors, check.get("status") == expected_gate_status, "power_target_met status does not match deterministic decision")
    if check.get("status") in {"pass", "fail"}:
        target = _evidence_path(check.get("evidence"), here, repo)
        _error(errors, target == artifact_path.resolve(), "power decision evidence must reference power-analysis.json")
    return errors


def _parse_timestamp(errors: list[str], value: Any, label: str) -> datetime | None:
    if not isinstance(value, str) or not value:
        errors.append(f"{label} is missing")
        return None
    try:
        parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        errors.append(f"{label} is not a valid ISO-8601 timestamp")
        return None
    if parsed.tzinfo is None or parsed.utcoffset() is None:
        errors.append(f"{label} must include a UTC offset")
        return None
    return parsed.astimezone(timezone.utc)


def _require_nonempty_strings(errors: list[str], value: Any, fields: tuple[str, ...], label: str) -> bool:
    if not isinstance(value, dict):
        errors.append(f"{label} must be an object")
        return False
    complete = True
    for field in fields:
        present = isinstance(value.get(field), str) and bool(value[field])
        _error(errors, present, f"{label}.{field} is missing")
        complete = complete and present
    return complete


def _require_exact_fields(errors: list[str], value: Any, fields: tuple[str, ...], label: str) -> bool:
    if not isinstance(value, dict):
        errors.append(f"{label} must be an object")
        return False
    exact = set(value) == set(fields)
    _error(errors, exact, f"{label} fields changed or are incomplete")
    return exact


def validate_pricing_budget(
    protocol: dict[str, Any],
    checks: dict[str, dict[str, Any]],
    *,
    here: pathlib.Path = HERE,
    repo: pathlib.Path = REPO,
    now: datetime | None = None,
) -> list[str]:
    """Validate a provider-neutral quote, token envelope, calculation, and approval."""
    errors: list[str] = []
    artifact_path = here / "pricing-budget.json"
    paid_budget = protocol.get("paid_budget", {})
    _error(errors, paid_budget.get("contract") == "pricing-budget.json", "paid budget contract must reference pricing-budget.json")
    _error(errors, paid_budget.get("currency") == "USD", "paid budget currency must be USD")
    _error(errors, paid_budget.get("formula_id") == pricing_budget.FORMULA_ID, "paid budget formula_id changed")
    _error(errors, artifact_path.is_file(), "pricing-budget.json is missing")
    if not artifact_path.is_file():
        return errors
    artifact = _load_artifact(artifact_path, errors, "pricing-budget.json")
    if not isinstance(artifact, dict):
        return errors
    required = (
        "schema_version",
        "runner",
        "pricing_quote",
        "design",
        "token_assumptions",
        "calculation",
        "approval",
    )
    if not _required_object_fields(errors, artifact, required, "pricing-budget.json"):
        return errors
    _require_exact_fields(errors, artifact, required, "pricing-budget.json")
    _error(errors, artifact.get("schema_version") == 1, "pricing budget schema_version must be 1")

    runner = artifact.get("runner")
    if not isinstance(runner, dict):
        errors.append("pricing runner must be an object")
        runner = {}
    runner_fields = ("provider", "runner_id", "runner_version", "model_id", "effort")
    _require_exact_fields(errors, runner, runner_fields, "pricing runner")
    for field in runner_fields:
        value = runner.get(field)
        _error(errors, value is None or (isinstance(value, str) and bool(value)), f"pricing runner.{field} must be null or a non-empty string")

    quote = artifact.get("pricing_quote")
    if not isinstance(quote, dict):
        errors.append("pricing_quote must be an object")
        quote = {}
    quote_fields = (
        "status",
        "source_uri",
        "source_artifact_path",
        "source_artifact_sha256",
        "as_of",
        "retrieved_at",
        "expires_at",
        "maximum_age_days",
        "currency",
        "tokens_per_price_unit",
        "prices_usd_per_unit",
    )
    _require_exact_fields(errors, quote, quote_fields, "pricing_quote")
    _error(errors, quote.get("status") in {"pending", "pinned"}, "pricing quote status is invalid")
    _error(errors, quote.get("currency") == "USD", "pricing quote currency must be USD")
    _error(errors, quote.get("tokens_per_price_unit") == 1_000_000, "pricing quote unit must be USD per 1,000,000 tokens")
    prices = quote.get("prices_usd_per_unit")
    if not isinstance(prices, dict):
        errors.append("prices_usd_per_unit must be an object")
        prices = {}
    _error(errors, set(prices) == set(pricing_budget.TOKEN_KEYS), "pricing quote token categories changed")
    for key in pricing_budget.TOKEN_KEYS:
        value = prices.get(key)
        if value is not None:
            try:
                pricing_budget.parse_decimal(value, f"prices_usd_per_unit.{key}")
            except ValueError as exc:
                errors.append(str(exc))

    agent_design = protocol.get("agent_design", {})
    power = agent_design.get("power", {})
    design = artifact.get("design")
    if not isinstance(design, dict):
        errors.append("pricing design must be an object")
        design = {}
    design_fields = (
        "protocol_id",
        "power_status_at_binding",
        "approved_for_budgeting",
        "tasks",
        "treatments",
        "repetitions_per_treatment",
        "requested_calls",
        "reserve_calls",
        "maximum_calls_with_reserve",
    )
    _require_exact_fields(errors, design, design_fields, "pricing design")
    treatments = agent_design.get("primary_treatments", [])
    tasks = agent_design.get("tasks")
    repetitions = agent_design.get("repetitions_per_treatment")
    expected_requested = (
        tasks * len(treatments) * repetitions
        if isinstance(tasks, int)
        and not isinstance(tasks, bool)
        and isinstance(treatments, list)
        and isinstance(repetitions, int)
        and not isinstance(repetitions, bool)
        else None
    )
    expected_maximum = agent_design.get("maximum_calls_with_reserve")
    expected_reserve = (
        expected_maximum - expected_requested
        if isinstance(expected_maximum, int)
        and not isinstance(expected_maximum, bool)
        and isinstance(expected_requested, int)
        else None
    )
    expected_design = {
        "protocol_id": protocol.get("protocol_id"),
        "power_status_at_binding": power.get("status"),
        "tasks": tasks,
        "treatments": treatments,
        "repetitions_per_treatment": repetitions,
        "requested_calls": agent_design.get("requested_cells"),
        "reserve_calls": expected_reserve,
        "maximum_calls_with_reserve": expected_maximum,
    }
    for field, expected in expected_design.items():
        _error(errors, design.get(field) == expected, f"pricing design does not match preregistration: {field}")
    _error(errors, design.get("requested_calls") == expected_requested, "pricing design requested_calls arithmetic is inconsistent")
    _error(errors, isinstance(design.get("approved_for_budgeting"), bool), "pricing design approved_for_budgeting must be boolean")

    price_check = checks.get("model_runner_price_pinned", {})
    budget_check = checks.get("paid_budget_cap_approved", {})
    quote_pinned = quote.get("status") == "pinned"
    quote_times: dict[str, datetime | None] = {"as_of": None, "retrieved_at": None, "expires_at": None}
    if quote_pinned:
        _require_nonempty_strings(errors, runner, runner_fields, "pricing runner")
        _require_nonempty_strings(errors, quote, ("source_uri",), "pricing_quote")
        source_record = {
            "path": quote.get("source_artifact_path"),
            "sha256": quote.get("source_artifact_sha256"),
        }
        _verify_hashed_file(errors, source_record, "pricing quote source artifact", repo=repo)
        for key in pricing_budget.TOKEN_KEYS:
            try:
                pricing_budget.parse_decimal(prices.get(key), f"prices_usd_per_unit.{key}")
            except ValueError as exc:
                errors.append(str(exc))
        for field in quote_times:
            quote_times[field] = _parse_timestamp(errors, quote.get(field), f"pricing_quote.{field}")
        maximum_age_days = quote.get("maximum_age_days")
        _error(
            errors,
            isinstance(maximum_age_days, int) and not isinstance(maximum_age_days, bool) and maximum_age_days > 0,
            "pricing_quote.maximum_age_days must be a positive integer",
        )
        as_of = quote_times["as_of"]
        retrieved_at = quote_times["retrieved_at"]
        expires_at = quote_times["expires_at"]
        current = (now or datetime.now(timezone.utc)).astimezone(timezone.utc)
        if as_of is not None and retrieved_at is not None:
            _error(errors, retrieved_at >= as_of, "pricing quote was retrieved before its as-of timestamp")
        if as_of is not None and expires_at is not None:
            _error(errors, expires_at > as_of, "pricing quote expiry must be after its as-of timestamp")
        if retrieved_at is not None:
            _error(errors, retrieved_at <= current, "pricing quote retrieval timestamp is in the future")
        if expires_at is not None:
            _error(errors, current <= expires_at, "pricing quote has expired")
        if as_of is not None and isinstance(maximum_age_days, int) and not isinstance(maximum_age_days, bool) and maximum_age_days > 0:
            _error(errors, current <= as_of + timedelta(days=maximum_age_days), "pricing quote exceeds its maximum age")

    if price_check.get("status") == "pass":
        _error(errors, quote_pinned, "model/runner/price gate cannot pass until the quote is pinned")
        target = _evidence_path(price_check.get("evidence"), here, repo)
        _error(errors, target == artifact_path.resolve(), "model/runner/price pass evidence must reference pricing-budget.json")

    assumptions = artifact.get("token_assumptions")
    if not isinstance(assumptions, dict):
        errors.append("token_assumptions must be an object")
        assumptions = {}
    _require_exact_fields(
        errors,
        assumptions,
        ("mode", "explicit_per_call_caps", "empirical_bound"),
        "token_assumptions",
    )
    caps = assumptions.get("explicit_per_call_caps")
    if not isinstance(caps, dict):
        errors.append("explicit_per_call_caps must be an object")
        caps = {}
    _require_exact_fields(
        errors,
        caps,
        ("uncached_input", "cached_input", "output", "rationale"),
        "explicit_per_call_caps",
    )
    empirical = assumptions.get("empirical_bound")
    if not isinstance(empirical, dict):
        errors.append("empirical_bound must be an object")
        empirical = {}
    _require_exact_fields(
        errors,
        empirical,
        (
            "runner",
            "evidence_path",
            "evidence_sha256",
            "statistic",
            "quantile",
            "safety_multiplier",
            "observed_tokens_per_call",
        ),
        "empirical_bound",
    )
    observed = empirical.get("observed_tokens_per_call")
    if not isinstance(observed, dict):
        errors.append("empirical observed_tokens_per_call must be an object")
        observed = {}
    _require_exact_fields(
        errors,
        observed,
        pricing_budget.TOKEN_KEYS,
        "empirical observed_tokens_per_call",
    )
    empirical_runner = empirical.get("runner")
    if empirical_runner is not None:
        _require_exact_fields(errors, empirical_runner, runner_fields, "empirical runner")
    mode = assumptions.get("mode")
    _error(errors, mode in {None, "explicit_per_call_caps", "empirical_bound"}, "token assumption mode is invalid")
    if mode == "explicit_per_call_caps":
        _error(errors, isinstance(caps.get("rationale"), str) and bool(caps["rationale"]), "explicit per-call caps require a rationale")
        try:
            pricing_budget.effective_tokens_per_call(artifact)
        except ValueError as exc:
            errors.append(str(exc))
    elif mode == "empirical_bound":
        _error(errors, empirical.get("runner") == runner, "empirical token evidence runner does not match the pinned runner")
        _verify_hashed_file(
            errors,
            {"path": empirical.get("evidence_path"), "sha256": empirical.get("evidence_sha256")},
            "empirical token evidence",
            repo=repo,
        )
        _error(errors, isinstance(empirical.get("statistic"), str) and bool(empirical["statistic"]), "empirical token statistic is missing")
        quantile = empirical.get("quantile")
        _error(
            errors,
            isinstance(quantile, (int, float)) and not isinstance(quantile, bool) and 0 < quantile <= 1,
            "empirical token quantile must be in (0, 1]",
        )
        try:
            pricing_budget.effective_tokens_per_call(artifact)
        except ValueError as exc:
            errors.append(str(exc))

    expected_calculation: dict[str, Any] | None = None
    if quote_pinned and mode in {"explicit_per_call_caps", "empirical_bound"}:
        try:
            expected_calculation = pricing_budget.calculate(artifact)
        except ValueError as exc:
            errors.append(f"cannot calculate paid budget: {exc}")
        if expected_calculation is not None:
            _error(errors, artifact.get("calculation") == expected_calculation, "pricing budget calculation is stale or incorrect")
    elif artifact.get("calculation") is not None:
        errors.append("pricing budget calculation must remain null until quote and token assumptions are complete")

    approval = artifact.get("approval")
    if not isinstance(approval, dict):
        errors.append("pricing budget approval must be an object")
        approval = {}
    _require_exact_fields(
        errors,
        approval,
        (
            "status",
            "approved_cap_usd",
            "approved_by",
            "approver_role",
            "approved_at",
            "expires_at",
            "notes",
        ),
        "pricing budget approval",
    )
    _error(errors, approval.get("status") in {"pending", "approved"}, "pricing budget approval status is invalid")
    approval_is_final = approval.get("status") == "approved"
    if approval_is_final:
        _error(errors, quote_pinned, "budget approval requires a pinned pricing quote")
        _error(errors, expected_calculation is not None, "budget approval requires a computed maximum")
        _error(errors, design.get("approved_for_budgeting") is True, "budget approval cannot use an unapproved experimental design")
        _error(errors, power.get("status") == "pass" and power.get("design_decision_required") is False, "budget approval requires an approved powered design")
        _require_nonempty_strings(errors, approval, ("approved_by", "approver_role"), "approval")
        approved_at = _parse_timestamp(errors, approval.get("approved_at"), "approval.approved_at")
        approval_expires = _parse_timestamp(errors, approval.get("expires_at"), "approval.expires_at")
        current = (now or datetime.now(timezone.utc)).astimezone(timezone.utc)
        if approved_at is not None:
            _error(errors, approved_at <= current, "budget approval timestamp is in the future")
            retrieved_at = quote_times.get("retrieved_at")
            if retrieved_at is not None:
                _error(errors, approved_at >= retrieved_at, "budget was approved before the pricing quote was retrieved")
        if approval_expires is not None:
            _error(errors, current <= approval_expires, "budget approval has expired")
            quote_expires = quote_times.get("expires_at")
            if quote_expires is not None:
                _error(errors, approval_expires <= quote_expires, "budget approval cannot outlive the pricing quote")
        approved_cap: Decimal | None = None
        try:
            approved_cap = pricing_budget.parse_decimal(approval.get("approved_cap_usd"), "approval.approved_cap_usd")
        except ValueError as exc:
            errors.append(str(exc))
        if approved_cap is not None and expected_calculation is not None:
            calculated_maximum = pricing_budget.parse_decimal(expected_calculation["maximum_usd"], "calculation.maximum_usd")
            _error(errors, approved_cap >= calculated_maximum, "approved USD cap is below the calculated maximum")

    if budget_check.get("status") == "pass":
        _error(errors, price_check.get("status") == "pass", "budget gate cannot pass before model/runner/price gate")
        _error(errors, approval_is_final, "paid budget gate cannot pass without explicit approval")
        _error(errors, expected_calculation is not None, "paid budget gate cannot pass without a computed maximum")
        _error(errors, design.get("approved_for_budgeting") is True, "paid budget gate cannot use an unapproved experimental design")
        _error(errors, power.get("status") == "pass" and power.get("design_decision_required") is False, "paid budget gate requires an approved powered design")
        target = _evidence_path(budget_check.get("evidence"), here, repo)
        _error(errors, target == artifact_path.resolve(), "paid budget pass evidence must reference pricing-budget.json")
        _error(errors, paid_budget.get("status") == "approved", "preregistration paid budget status must be approved")
        _error(errors, paid_budget.get("maximum_usd") == approval.get("approved_cap_usd"), "preregistration maximum_usd must match the approved cap")
    else:
        _error(errors, paid_budget.get("status") == "pending", "preregistration paid budget must remain pending until the gate passes")
        _error(errors, paid_budget.get("maximum_usd") is None, "preregistration maximum_usd must remain null until approval")
    return errors


def _validate_query_items(items: Any, label: str) -> tuple[list[str], list[dict[str, Any]]]:
    errors: list[str] = []
    if not isinstance(items, list):
        return [f"{label} items must be an array"], []
    valid_items: list[dict[str, Any]] = []
    query_ids: list[Any] = []
    query_hashes: list[Any] = []
    for index, item in enumerate(items):
        item_label = f"{label}.items[{index}]"
        required = (
            "query_id",
            "task_id",
            "query_source",
            "query_text",
            "query_sha256",
            "temporal_cutoff",
            "null_query",
            "judgments",
        )
        if not _required_object_fields(errors, item, required, item_label):
            continue
        valid_items.append(item)
        query_id = item.get("query_id")
        task_id = item.get("task_id")
        query_text = item.get("query_text")
        query_hash = item.get("query_sha256")
        query_ids.append(query_id)
        query_hashes.append(query_hash)
        _error(errors, isinstance(query_id, str) and bool(query_id), f"{item_label}: query_id is missing")
        _error(errors, isinstance(task_id, str) and bool(task_id), f"{item_label}: task_id is missing")
        _error(errors, item.get("query_source") in {"user_prompt_derived", "oracle_upper_bound"}, f"{item_label}: query_source is invalid")
        _error(errors, isinstance(query_text, str), f"{item_label}: query_text must be a string")
        _error(errors, _is_sha256(query_hash), f"{item_label}: query_sha256 is invalid")
        if isinstance(query_text, str) and _is_sha256(query_hash):
            expected_hash = hashlib.sha256(query_text.encode("utf-8")).hexdigest()
            _error(errors, query_hash == expected_hash, f"{item_label}: query text hash mismatch")
        _error(errors, isinstance(item.get("null_query"), bool), f"{item_label}: null_query must be boolean")
        judgments = item.get("judgments")
        if not isinstance(judgments, list):
            errors.append(f"{item_label}: judgments must be an array")
            continue
        fact_ids = [judgment.get("fact_id") for judgment in judgments if isinstance(judgment, dict)]
        _error(errors, len(fact_ids) == len(judgments), f"{item_label}: judgment must be an object")
        _error(errors, _unique_strings(fact_ids), f"{item_label}: judgment fact_ids are not unique strings")
    _error(errors, _unique_strings(query_ids), f"{label} query_ids are not unique strings")
    _error(errors, _unique_strings(query_hashes), f"{label} query hashes are not unique strings")
    return errors, valid_items


def validate_dataset(dataset: dict[str, Any], protocol: dict[str, Any], freeze: bool) -> list[str]:
    errors: list[str] = []
    development = dataset.get("development", {})
    if not isinstance(development, dict):
        return ["development dataset section must be an object"]
    dev_errors, dev_items = _validate_query_items(development.get("items"), "development")
    errors.extend(dev_errors)
    dev_task_ids = [item.get("task_id") for item in dev_items if isinstance(item.get("task_id"), str)]
    dev_task_count = len(set(dev_task_ids))
    if "item_count" in development:
        _error(errors, development.get("item_count") == len(dev_items), "development item_count is stale")
    if "unique_task_count" in development:
        _error(errors, development.get("unique_task_count") == dev_task_count, "development unique_task_count is stale")

    sealed = dataset.get("sealed_holdout", {})
    if not isinstance(sealed, dict):
        return errors + ["sealed holdout section must be an object"]
    allowed_sealed_fields = {
        "item_count",
        "unique_task_count",
        "commitment_sha256",
        "sealed_at",
        "opened_at",
        "items",
    }
    _error(
        errors,
        isinstance(sealed, dict) and set(sealed).issubset(allowed_sealed_fields),
        "sealed holdout contains fields outside the committed metadata/items contract",
    )
    sealed_items = sealed.get("items")
    if sealed.get("opened_at") is None:
        _error(
            errors,
            isinstance(sealed_items, list) and not sealed_items,
            "plaintext sealed holdout labels are prohibited while unopened",
        )
    elif isinstance(sealed_items, list):
        sealed_errors, opened_items = _validate_query_items(sealed_items, "sealed_holdout")
        errors.extend(sealed_errors)
        _error(errors, sealed.get("item_count") == len(opened_items), "sealed holdout item_count is stale")
        _error(
            errors,
            sealed.get("unique_task_count")
            == len({item.get("task_id") for item in opened_items if isinstance(item.get("task_id"), str)}),
            "sealed holdout unique_task_count is stale",
        )
    _error(errors, _is_nonnegative_int(sealed.get("item_count")), "sealed holdout item_count is invalid")
    _error(errors, _is_nonnegative_int(sealed.get("unique_task_count")), "sealed holdout unique_task_count is invalid")
    if _is_nonnegative_int(sealed.get("item_count")) and _is_nonnegative_int(sealed.get("unique_task_count")):
        _error(errors, sealed["unique_task_count"] <= sealed["item_count"], "sealed holdout unique task count exceeds item count")

    if freeze:
        offline = protocol.get("offline_dataset", {})
        _error(errors, len(dev_items) >= offline.get("minimum_development_queries", 0), "too few development relevance queries")
        _error(errors, dev_task_count >= offline.get("minimum_development_tasks", 0), "too few development relevance tasks")
        _error(errors, sealed.get("item_count", 0) >= offline.get("minimum_sealed_holdout_queries", 0), "too few sealed relevance queries")
        _error(errors, sealed.get("unique_task_count", 0) >= offline.get("minimum_sealed_holdout_tasks", 0), "too few sealed relevance tasks")
        _error(errors, _is_sha256(sealed.get("commitment_sha256")), "relevance holdout commitment is missing")
        fresh_commitment = protocol.get("fresh_holdout", {}).get("commitment_sha256")
        _error(errors, sealed.get("commitment_sha256") == fresh_commitment, "fresh and relevance holdout commitments differ")
    return errors


def _load_engine_records(path: pathlib.Path, errors: list[str], repo: pathlib.Path) -> list[Any]:
    if path.is_dir():
        records: list[Any] = []
        for record_path in sorted(path.glob("*.json")):
            record = _load_artifact(record_path, errors, f"engine verification record {record_path.name}")
            if record is not None:
                records.append(record)
        return records
    artifact = _load_artifact(path, errors, "engine verification evidence")
    if isinstance(artifact, list):
        return artifact
    if not isinstance(artifact, dict):
        return []
    if "records" in artifact:
        records = artifact.get("records")
        if not isinstance(records, list):
            errors.append("engine verification manifest records must be an array")
            return []
        return records
    if "record_paths" in artifact:
        record_paths = artifact.get("record_paths")
        if not isinstance(record_paths, list):
            errors.append("engine verification record_paths must be an array")
            return []
        records = []
        for index, raw_path in enumerate(record_paths):
            target = _safe_relative_path(raw_path, repo)
            _error(errors, target is not None, f"engine record_paths[{index}] is unsafe or invalid")
            if target is None:
                continue
            _error(errors, target.is_file(), f"engine record does not exist: {raw_path}")
            if target.is_file():
                record = _load_artifact(target, errors, f"engine verification record {raw_path}")
                if record is not None:
                    records.append(record)
        return records
    return [artifact]


def validate_engine_verification(
    matrix: dict[str, Any],
    check: dict[str, Any],
    *,
    here: pathlib.Path = HERE,
    repo: pathlib.Path = REPO,
) -> list[str]:
    errors: list[str] = []
    if check.get("status") != "pass":
        return errors
    evidence_path = _evidence_path(check.get("evidence"), here, repo)
    _error(errors, evidence_path is not None and evidence_path.exists(), "engine pass evidence is missing")
    if evidence_path is None or not evidence_path.exists():
        return errors
    records = _load_engine_records(evidence_path, errors, repo)
    record_arms = [record.get("arm") if isinstance(record, dict) else None for record in records]
    _error(errors, len(record_arms) == len(ARMS), "engine verification must contain exactly one record per primary arm")
    _error(errors, _unique_strings(record_arms), "engine verification arms are not unique strings")
    _error(errors, all(isinstance(arm, str) for arm in record_arms) and set(record_arms) == set(ARMS), "engine verification does not cover the exact primary arms")
    matrix_by_id = {
        arm.get("id"): arm for arm in matrix.get("arms", []) if isinstance(arm, dict) and isinstance(arm.get("id"), str)
    }
    namespaces: list[Any] = []
    semantic_vector_paths: list[Any] = []
    corpus_signatures: list[tuple[Any, Any, Any]] = []
    query_ids: list[Any] = []
    for index, record in enumerate(records):
        label = f"engine records[{index}]"
        if not _required_object_fields(
            errors,
            record,
            ("schema_version", "arm", "requested", "effective", "artifacts", "corpus", "result"),
            label,
        ):
            continue
        arm_id = record.get("arm")
        arm = matrix_by_id.get(arm_id, {}) if isinstance(arm_id, str) else {}
        _error(errors, record.get("schema_version") == 1, f"{label}: schema_version must be 1")
        requested = record.get("requested")
        effective = record.get("effective")
        artifacts = record.get("artifacts")
        corpus = record.get("corpus")
        result = record.get("result")
        requested_ok = _required_object_fields(errors, requested, ("command", "environment", "namespace"), f"{label}.requested")
        effective_ok = _required_object_fields(
            errors,
            effective,
            (
                "engine",
                "semantic_available",
                "bm25_enabled",
                "fallback_used",
                "embedder_id",
                "embedding_dimension",
                "vector_count",
                "vector_namespace",
            ),
            f"{label}.effective",
        )
        artifacts_ok = _required_object_fields(
            errors,
            artifacts,
            (
                "binary_path",
                "binary_sha256",
                "stdout_path",
                "stdout_sha256",
                "stderr_path",
                "stderr_sha256",
                "vector_artifact_path",
                "vector_artifact_sha256",
                "embedding_model_path",
                "embedding_model_sha256",
            ),
            f"{label}.artifacts",
        )
        corpus_ok = _required_object_fields(
            errors,
            corpus,
            ("facts_sha256", "prefilter_count", "eligible_count", "excluded_by_reason", "delivered_count"),
            f"{label}.corpus",
        )
        result_ok = _required_object_fields(errors, result, ("query_id", "fact_ids_in_order", "output_valid"), f"{label}.result")

        if requested_ok:
            command = requested.get("command")
            _error(
                errors,
                (isinstance(command, str) and bool(command))
                or (isinstance(command, list) and bool(command) and all(isinstance(part, str) for part in command)),
                f"{label}: requested command is invalid",
            )
            environment = requested.get("environment")
            _error(errors, isinstance(environment, dict), f"{label}: requested environment must be an object")
            if isinstance(environment, dict):
                for key, value in arm.get("environment", {}).items():
                    _error(errors, environment.get(key) == value, f"{label}: requested environment mismatch: {key}")
            _error(errors, requested.get("namespace") == arm.get("namespace"), f"{label}: requested namespace mismatch")

        if effective_ok:
            namespace = effective.get("vector_namespace")
            namespaces.append(namespace)
            _error(errors, effective.get("engine") == arm_id == arm.get("effective_engine_required"), f"{label}: effective engine mismatch")
            _error(errors, effective.get("semantic_available") is arm.get("semantic"), f"{label}: semantic availability mismatch")
            _error(errors, effective.get("bm25_enabled") is False, f"{label}: BM25 must be disabled")
            _error(errors, effective.get("fallback_used") is False, f"{label}: fallback is prohibited")
            _error(errors, namespace == arm.get("namespace"), f"{label}: effective namespace mismatch")
            _error(errors, isinstance(namespace, str) and bool(namespace), f"{label}: vector namespace is missing")
            _error(errors, _is_nonnegative_int(effective.get("vector_count")), f"{label}: vector_count is invalid")
            if arm.get("semantic") is True:
                _error(errors, isinstance(effective.get("embedder_id"), str) and bool(effective.get("embedder_id")), f"{label}: semantic embedder_id is missing")
                dimension = effective.get("embedding_dimension")
                _error(errors, _is_nonnegative_int(dimension) and dimension > 0, f"{label}: semantic embedding_dimension is invalid")
            else:
                _error(errors, effective.get("embedder_id") is None, f"{label}: lexical embedder_id must be null")
                _error(errors, effective.get("embedding_dimension") is None, f"{label}: lexical embedding_dimension must be null")

        if artifacts_ok:
            for stem in ("binary", "stdout", "stderr"):
                _verify_hashed_file(
                    errors,
                    {"path": artifacts.get(f"{stem}_path"), "sha256": artifacts.get(f"{stem}_sha256")},
                    f"{label}.{stem} artifact",
                    repo=repo,
                )
            vector_hash = artifacts.get("vector_artifact_sha256")
            vector_path = artifacts.get("vector_artifact_path")
            model_hash = artifacts.get("embedding_model_sha256")
            model_path = artifacts.get("embedding_model_path")
            if arm.get("semantic") is True:
                semantic_vector_paths.append(vector_path)
                _verify_hashed_file(
                    errors,
                    {"path": vector_path, "sha256": vector_hash},
                    f"{label}.vector artifact",
                    repo=repo,
                )
            else:
                _error(errors, (vector_path is None) == (vector_hash is None), f"{label}: vector artifact path/hash presence differs")
                if vector_path is not None or vector_hash is not None:
                    _verify_hashed_file(
                        errors,
                        {"path": vector_path, "sha256": vector_hash},
                        f"{label}.vector artifact",
                        repo=repo,
                    )
            if arm_id == "embeddinggemma_rrf":
                _verify_hashed_file(
                    errors,
                    {"path": model_path, "sha256": model_hash},
                    f"{label}.embedding model artifact",
                    repo=repo,
                )
            else:
                _error(errors, (model_path is None) == (model_hash is None), f"{label}: embedding model path/hash presence differs")
                if model_path is not None or model_hash is not None:
                    _verify_hashed_file(
                        errors,
                        {"path": model_path, "sha256": model_hash},
                        f"{label}.embedding model artifact",
                        repo=repo,
                    )

        if corpus_ok:
            facts_hash = corpus.get("facts_sha256")
            prefilter_count = corpus.get("prefilter_count")
            eligible_count = corpus.get("eligible_count")
            delivered_count = corpus.get("delivered_count")
            _error(errors, _is_sha256(facts_hash), f"{label}: facts_sha256 is invalid")
            for key, value in (
                ("prefilter_count", prefilter_count),
                ("eligible_count", eligible_count),
                ("delivered_count", delivered_count),
            ):
                _error(errors, _is_nonnegative_int(value), f"{label}: {key} is invalid")
            _error(errors, isinstance(corpus.get("excluded_by_reason"), dict), f"{label}: excluded_by_reason must be an object")
            if _is_nonnegative_int(prefilter_count) and _is_nonnegative_int(eligible_count):
                _error(errors, eligible_count <= prefilter_count, f"{label}: eligible_count exceeds prefilter_count")
            if _is_nonnegative_int(eligible_count) and _is_nonnegative_int(delivered_count):
                _error(errors, delivered_count <= eligible_count, f"{label}: delivered_count exceeds eligible_count")
            corpus_signatures.append((facts_hash, prefilter_count, eligible_count))

        if result_ok:
            query_id = result.get("query_id")
            query_ids.append(query_id)
            _error(errors, isinstance(query_id, str) and bool(query_id), f"{label}: query_id is missing")
            fact_ids = result.get("fact_ids_in_order")
            _error(errors, isinstance(fact_ids, list) and all(isinstance(fact_id, str) for fact_id in fact_ids), f"{label}: fact_ids_in_order is invalid")
            if isinstance(fact_ids, list) and all(isinstance(fact_id, str) for fact_id in fact_ids):
                _error(errors, _unique_strings(fact_ids), f"{label}: ranked fact ids are not unique")
            _error(errors, result.get("output_valid") is True, f"{label}: output_valid must be true")

    _error(errors, _unique_strings(namespaces), "engine verification namespaces are not unique strings")
    _error(
        errors,
        _unique_strings(semantic_vector_paths),
        "semantic engine vector artifact paths are not unique strings",
    )
    _error(errors, _all_equal(corpus_signatures), "engine verification corpus hash/prefilter/eligible counts differ")
    _error(errors, _all_equal(query_ids), "engine verification query ids differ")
    return errors


def validate_inventory(inventory: dict[str, Any]) -> list[str]:
    errors: list[str] = []
    configs = sorted((C0701 / "dev").glob("*.json")) + sorted((C0701 / "holdout").glob("*.json"))
    patches = sorted((C0701 / "patches").glob("*-test.patch"))
    ledger = load(C0701 / "selection-ledger.json")
    ledger_by_short = {row["short_sha"]: row for row in ledger["tasks"]}
    actual_by_id: dict[str, tuple[pathlib.Path, dict[str, Any]]] = {}
    actual_by_short: dict[str, tuple[pathlib.Path, dict[str, Any]]] = {}
    for path in configs:
        task = load(path)
        task_id = task["id"]
        short = task["_mined_from_commit"][:9]
        _error(errors, task_id not in actual_by_id, f"duplicate task id in configs: {task_id}")
        _error(errors, short not in actual_by_short, f"duplicate fix commit in configs: {short}")
        actual_by_id[task_id] = (path, task)
        actual_by_short[short] = (path, task)

    inventory_tasks = inventory.get("tasks", [])
    inventory_by_id = {row.get("task_id"): row for row in inventory_tasks}
    _error(errors, inventory.get("schema_version") == 1, "inventory schema_version must be 1")
    _error(errors, inventory.get("state_order") == STATES, "inventory state order changed")
    _error(errors, len(inventory_by_id) == len(inventory_tasks), "inventory task ids are not unique")
    _error(errors, set(inventory_by_id) == set(actual_by_id), "inventory/config task-id sets differ")
    patch_shorts = {path.name.removeprefix("entire-cli-c0701-").removesuffix("-test.patch") for path in patches}
    _error(errors, patch_shorts == set(actual_by_short), "patch/config fix-commit sets differ")
    _error(errors, set(ledger_by_short) == set(actual_by_short), "ledger/config fix-commit sets differ")
    _error(errors, ledger.get("total") == len(actual_by_id), "ledger total is stale")
    _error(errors, ledger.get("dev_count") == sum(path.parent.name == "dev" for path in configs), "ledger dev_count is stale")
    _error(errors, ledger.get("holdout_count") == sum(path.parent.name == "holdout" for path in configs), "ledger holdout_count is stale")

    for task_id, row in inventory_by_id.items():
        if task_id not in actual_by_id:
            continue
        config_path, config = actual_by_id[task_id]
        artifacts = row.get("artifacts", {})
        patch_path = REPO / artifacts.get("patch_path", "missing")
        _error(errors, REPO / artifacts.get("config_path", "missing") == config_path, f"{task_id}: config path mismatch")
        _error(errors, artifacts.get("config_sha256") == digest(config_path), f"{task_id}: config hash mismatch")
        prompt_hash = hashlib.sha256(config["prompt"].encode()).hexdigest()
        _error(errors, artifacts.get("prompt_sha256") == prompt_hash, f"{task_id}: prompt hash mismatch")
        _error(errors, patch_path.is_file(), f"{task_id}: patch is missing")
        if patch_path.is_file():
            _error(errors, artifacts.get("patch_sha256") == digest(patch_path), f"{task_id}: patch hash mismatch")
        _error(errors, row.get("state") in STATES, f"{task_id}: invalid task state")
        _error(errors, bool(row.get("state_recorded_at")), f"{task_id}: state timestamp missing")
        _error(errors, bool(row.get("suite_references")), f"{task_id}: suite/source reference missing")
        _error(errors, row.get("ledger", {}).get("present") is True, f"{task_id}: ledger presence not recorded")
        if row.get("confirmatory_eligible"):
            _error(errors, row.get("state") == "unseen", f"{task_id}: exposed task marked confirmatory eligible")

    summary = inventory.get("summary", {})
    expected = {
        "config_files": len(configs),
        "unique_task_ids": len(actual_by_id),
        "patch_files": len(patches),
        "ledger_rows": len(ledger_by_short),
        "source_dev": sum(path.parent.name == "dev" for path in configs),
        "source_holdout": sum(path.parent.name == "holdout" for path in configs),
        "confirmatory_eligible": sum(bool(row.get("confirmatory_eligible")) for row in inventory_tasks),
    }
    _error(errors, summary == expected, "inventory summary does not match artifacts")
    _error(errors, not (C0701 / "dev" / "entire-cli-c0701-4dd458656.json").exists(), "duplicate 4dd458656 dev config remains")
    _error(errors, "d9df8fcca" in ledger_by_short, "d9df8fcca is absent from the ledger")
    return errors


def validate(freeze: bool = False) -> list[str]:
    errors: list[str] = []
    inventory = load(HERE / "task-inventory.json")
    protocol = load(HERE / "preregistration.json")
    matrix = load(HERE / "engine-matrix.json")
    dataset = load(HERE / "offline-relevance-dataset.json")
    gate = load(HERE / "go-no-go.json")
    errors.extend(validate_inventory(inventory))

    for schema_path in sorted((HERE / "schemas").glob("*.json")):
        schema = load(schema_path)
        _error(errors, schema.get("$schema") == "https://json-schema.org/draft/2020-12/schema", f"{schema_path.name}: wrong JSON Schema dialect")

    arms = matrix.get("arms", [])
    _error(errors, [arm.get("id") for arm in arms] == ARMS, "primary engine arms changed or reordered")
    namespaces = [arm.get("namespace") for arm in arms]
    _error(errors, len(set(namespaces)) == len(namespaces), "engine namespaces are not isolated")
    for arm in arms:
        _error(errors, arm.get("environment", {}).get("ENTIRE_BRAIN_FACTS_BM25") == "0", f"{arm.get('id')}: BM25 must be off")
        _error(errors, arm.get("effective_engine_required") == arm.get("id"), f"{arm.get('id')}: effective engine contract mismatch")
    if len(arms) == len(ARMS):
        _error(errors, "--no-semantic" in arms[0].get("cli_flags", []), "lexical arm must explicitly disable semantic ranking")
        _error(errors, arms[1].get("environment", {}).get("ENTIRE_BRAIN_EMBEDDER") == "", "Model2Vec arm must explicitly select bundled default")
        _error(errors, arms[2].get("environment", {}).get("ENTIRE_BRAIN_EMBEDDER") == "ollama", "EmbeddingGemma arm must explicitly select loopback embedder")

    _error(errors, protocol.get("holdout_opened") is False, "fresh holdout was opened during preparation")
    _error(errors, dataset.get("sealed_holdout", {}).get("opened_at") is None, "relevance holdout was opened during preparation")
    gate_errors, checks = validate_gate_evidence(gate)
    errors.extend(gate_errors)
    errors.extend(validate_dataset(dataset, protocol, freeze))
    errors.extend(validate_integration_verification(protocol, checks))
    errors.extend(validate_analyzer_lock(protocol, checks.get("analyzer_hash_frozen", {})))
    errors.extend(validate_power_analysis(protocol, checks.get("power_target_met", {})))
    errors.extend(validate_engine_verification(matrix, checks.get("all_engines_machine_verified", {})))
    errors.extend(validate_pricing_budget(protocol, checks))

    freeze_info = protocol.get("freeze", {})
    recorded_protocol_hash = freeze_info.get("protocol_sha256")
    protocol_hash_claimed = freeze or recorded_protocol_hash is not None or checks.get("protocol_content_hash_frozen", {}).get("status") == "pass"
    if protocol_hash_claimed:
        expected_protocol_hash = protocol_content_sha256(protocol)
        _error(errors, _is_sha256(recorded_protocol_hash), "protocol freeze hash is missing or invalid")
        _error(errors, recorded_protocol_hash == expected_protocol_hash, "protocol freeze hash does not match canonical protocol content")

    if freeze:
        _error(errors, protocol.get("status") == "frozen_unopened", "protocol is not frozen_unopened")
        _error(errors, all(value == "pass" for value in protocol.get("dependencies", {}).values()), "WS2-WS5 dependencies are pending")
        selection = protocol.get("development_selection", {})
        _error(errors, selection.get("threshold_passed") is True, "offline development threshold has not passed")
        _error(errors, selection.get("selected_k") is not None, "packet K is not frozen")
        _error(errors, selection.get("selected_aggregation_rule") is not None, "aggregation rule is not frozen")
        _error(errors, protocol.get("agent_design", {}).get("power", {}).get("completed") is True, "power calculation is pending")
        _error(errors, _is_sha256(protocol.get("analyzer_sha256")), "analyzer hash is not frozen")
        holdout = protocol.get("fresh_holdout", {})
        _error(errors, _is_sha256(holdout.get("commitment_sha256")), "fresh holdout commitment is not frozen")
        _error(errors, dataset.get("status") == "frozen_unopened", "offline relevance dataset is not frozen_unopened")
        _error(errors, all(check.get("status") == "pass" for check in checks.values()), "paid-run checklist is not all pass")
        _error(errors, gate.get("decision") == "go", "paid-run decision is not go")
        _error(errors, bool(freeze_info.get("frozen_at")), "freeze timestamp is missing")
    return errors


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--freeze", action="store_true", help="enforce final-freeze and paid-run gates")
    args = parser.parse_args()
    errors = validate(freeze=args.freeze)
    if errors:
        for error in errors:
            print(f"ERROR: {error}", file=sys.stderr)
        return 1
    mode = "freeze" if args.freeze else "preparation"
    print(f"WS6 {mode} checks passed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
