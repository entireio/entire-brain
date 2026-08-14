#!/usr/bin/env python3
"""Build and verify the pending multi-repository negative-control run plan.

This module is deliberately contract-only.  It reads development-only v2
inventory artifacts, freezes their exact candidate order and resource policy,
and emits a deterministic plan that cannot authorize execution.  It never
creates a worktree, invokes Git or Go, runs a candidate, opens a protected
population, or calls a model/provider.
"""

from __future__ import annotations

import argparse
import copy
import hashlib
import json
import os
import pathlib
import tempfile
from typing import Any, Sequence, cast

import task_eligibility_v2 as eligibility
import task_overlap_registry as overlap
import task_eligibility as cli_eligibility
import task_population as cli_population


PROFILE = "agent_brain_development_task_negative_control_run_plan_v2"
SCHEMA_VERSION = 2
STATUS = "pending_owner_authorization"
EXPOSURE = eligibility.EXPOSURE
CANONICAL_JSON_PROFILE = "sorted_keys_compact_utf8_no_float_v1"
ARTIFACT_RENDER_PROFILE = "sorted_keys_indent_2_utf8_lf_v1"
REPOSITORY_SCHEMA_DESCRIPTION = (
    "Exact generated projection of the three frozen ledgers and all 62 ordered candidates."
)
REPOSITORY_ORDER = ("entire-brain", "entire-db", "entire-graph")
EXPECTED_COUNTS = {"entire-brain": 18, "entire-db": 25, "entire-graph": 19}
TOTAL_CANDIDATES = 62

AUTHORITY = {
    "approval_receipt": "absent_not_implemented",
    "approval_receipt_sha256": None,
    "approval_trust_mechanism": "absent_not_implemented",
    "approval_trust_root_sha256": None,
    "benchmark_execution": "forbidden_pending_separate_owner_approval",
    "calibration_membership": "forbidden_permanent_development_exposure",
    "candidate_execution": "forbidden_plan_unexecutable",
    "confirmatory_holdout_membership": "forbidden_permanent_development_exposure",
    "model_provider_execution": "forbidden_not_authorized",
    "owner_key": "absent_not_fabricated",
    "paid_execution": "forbidden_not_authorized",
    "population_assignment": "absent_not_authorized",
    "source_session_identity": eligibility.SESSION_STATUS,
}

EXECUTION_PROTOCOL = {
    "arm_order": ["baseline", "first_parent_source_reversal"],
    "candidate_order": "repository_order_then_first_parent_position_serial_v1",
    "candidate_process_execution": "absent_not_implemented",
    "classifier": "absent_not_implemented",
    "go_test_internal_timeout": "disabled_outer_process_group_deadline_required",
    "max_concurrency": 1,
    "max_total_wall_seconds": 28_800,
    "outer_timeout_seconds_per_arm": 600,
    "private_log_writer": "absent_not_implemented",
    "repetitions_per_arm": 2,
    "repository_order": list(REPOSITORY_ORDER),
    "total_candidate_count": TOTAL_CANDIDATES,
}

RESOURCE_BUDGET = {
    "disk_preflight": "require_free_bytes_at_least_reserve_plus_total_staging_before_execution",
    "max_cache_bytes_per_arm": 2_147_483_648,
    "max_cache_seed_bytes": 2_147_483_648,
    "max_private_raw_log_bytes_per_arm": 16_777_216,
    "max_private_raw_log_bytes_total": 2_147_483_648,
    "max_public_receipt_bytes_total": 16_777_216,
    "max_total_staging_bytes": 8_589_934_592,
    "max_worktree_bytes_per_arm": 1_073_741_824,
    "minimum_free_disk_before_staging_bytes": 17_179_869_184,
    "minimum_free_disk_reserve_bytes": 8_589_934_592,
    "resource_preflight": "absent_not_implemented",
}

CACHE_SEED = {
    "archive_sha256": None,
    "host_shared_cache_reuse": "forbidden",
    "manifest_profile": "content_addressed_offline_go_cache_seed_manifest_v1",
    "manifest_sha256": None,
    "network_dependency_resolution": "forbidden",
    "per_arm_cache_policy": "fresh_private_copy_from_verified_seed_required",
    "source_locator": None,
    "status": "pending_manifest_not_supplied",
    "unpacked_byte_count": None,
}

ISOLATION = {
    "clean_detached_worktrees": "required_by_future_executor_not_implemented",
    "filesystem_os_enforcement": "not_enforced",
    "filesystem_sandbox_claim": "none",
    "fresh_arm_repetition_state": "required_by_future_executor_not_implemented",
    "network_os_enforcement": "not_enforced",
    "network_sandbox_claim": "none",
    "offline_dependency_policy": "required_verified_cache_seed_and_network_resolution_disabled",
}

PRIVATE_LOG_POLICY = {
    "content_addressing": "sha256_exact_raw_bytes_required",
    "private_file_mode": "0600",
    "private_root_locator": None,
    "private_root_mode": "0700",
    "public_receipt_fields": "raw_log_sha256_and_byte_count_only",
    "raw_log_retention_days": 7,
    "raw_output_in_public_plan_or_receipt": False,
    "secret_redaction_claim": "none_private_raw_logs_may_contain_test_output",
    "status": "policy_frozen_writer_and_checker_absent",
}

PENDING_GATES = [
    "owner_execution_approval_receipt_and_trust_mechanism",
    "offline_cache_seed_manifest_and_archive",
    "negative_control_executor_and_reversal_proof",
    "classification_truth_table_and_receipt_checker",
    "private_content_addressed_log_writer_and_checker",
    "resource_preflight_cleanup_and_interruption_attestation",
]


class RunPlanError(ValueError):
    """Raised when a pending plan or one of its exact inputs differs."""


def _require(condition: bool, message: str) -> None:
    if not condition:
        raise RunPlanError(message)


def _sha256(raw: bytes) -> str:
    return hashlib.sha256(raw).hexdigest()


def _reject_floats(value: Any) -> None:
    if isinstance(value, float):
        raise RunPlanError("canonical run-plan JSON forbids floating-point values")
    if isinstance(value, dict):
        for child in value.values():
            _reject_floats(child)
    elif isinstance(value, list):
        for child in value:
            _reject_floats(child)


def _canonical_json_bytes(value: Any) -> bytes:
    _reject_floats(value)
    return json.dumps(
        value,
        ensure_ascii=False,
        allow_nan=False,
        sort_keys=True,
        separators=(",", ":"),
    ).encode("utf-8")


def _canonical_hash(value: Any) -> str:
    return _sha256(_canonical_json_bytes(value))


def _render(value: dict[str, Any]) -> bytes:
    _reject_floats(value)
    return (
        json.dumps(
            value,
            ensure_ascii=False,
            allow_nan=False,
            indent=2,
            sort_keys=True,
        )
        + "\n"
    ).encode("utf-8")


def _self_hash(value: dict[str, Any]) -> str:
    projected = copy.deepcopy(value)
    _require("plan_sha256" in projected, "plan_sha256 is missing")
    projected["plan_sha256"] = None
    return _canonical_hash(projected)


def _reject_duplicate_pairs(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        _require(key not in result, "JSON contains a duplicate object key")
        result[key] = value
    return result


def _load_json(path: pathlib.Path) -> tuple[dict[str, Any], bytes]:
    try:
        raw = path.read_bytes()
        value = json.loads(raw, object_pairs_hook=_reject_duplicate_pairs)
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        raise RunPlanError(f"cannot load JSON: {exc}") from exc
    _require(isinstance(value, dict), "JSON root must be an object")
    return value, raw


def _valid_sha(value: Any) -> bool:
    return (
        isinstance(value, str)
        and eligibility.SHA256_RE.fullmatch(value) is not None
        and value != "0" * 64
    )


def _artifact_name(path: pathlib.Path) -> str:
    name = path.name
    _require(bool(name) and pathlib.PurePosixPath(name).name == name, "artifact name is invalid")
    return name


def _implementation_identity(schema_path: pathlib.Path) -> dict[str, Any]:
    try:
        builder_hash = _sha256(pathlib.Path(__file__).read_bytes())
        schema_hash = _sha256(schema_path.read_bytes())
    except OSError as exc:
        raise RunPlanError(f"cannot read run-plan implementation dependency: {exc}") from exc
    return {
        "artifact_render_profile": ARTIFACT_RENDER_PROFILE,
        "builder_sha256": builder_hash,
        "canonical_json_profile": CANONICAL_JSON_PROFILE,
        "schema_sha256": schema_hash,
    }


def _verify_schema_repository_projection(
    schema_path: pathlib.Path,
    repositories: list[dict[str, Any]],
) -> None:
    schema, _ = _load_json(schema_path)
    properties = schema.get("properties")
    _require(isinstance(properties, dict), "run-plan schema properties are missing")
    properties = cast(dict[str, Any], properties)
    projection = properties.get("repositories")
    _require(
        projection
        == {
            "const": repositories,
            "description": REPOSITORY_SCHEMA_DESCRIPTION,
        },
        "run-plan schema repository projection differs from exact ledger order",
    )


def _load_contract(
    contract_path: pathlib.Path,
) -> tuple[dict[str, dict[str, Any]], dict[str, Any]]:
    contract, raw = _load_json(contract_path)
    repositories: dict[str, dict[str, Any]] = {}
    for key in REPOSITORY_ORDER:
        try:
            entry, contract_hash = eligibility.load_repository_contract(contract_path, key)
        except eligibility.EligibilityV2Error as exc:
            raise RunPlanError(f"repository contract is invalid: {exc}") from exc
        _require(contract_hash == _sha256(raw), "repository contract raw hash changed while reading")
        repositories[key] = entry
    _require(
        len(contract.get("repositories", [])) == len(REPOSITORY_ORDER),
        "repository contract must contain exactly the three planned repositories",
    )
    dependency = {
        "artifact_file": _artifact_name(contract_path),
        "artifact_sha256": _sha256(raw),
        "profile": contract["profile"],
        "schema_version": contract["schema_version"],
    }
    return repositories, dependency


def _load_ledger(
    path: pathlib.Path,
    *,
    contract_entry: dict[str, Any],
    contract_sha256: str,
    scanner_sha256: str,
    eligibility_schema_sha256: str,
) -> tuple[dict[str, Any], bytes]:
    ledger, raw = _load_json(path)
    try:
        eligibility.validate_ledger(ledger)
    except eligibility.EligibilityV2Error as exc:
        raise RunPlanError(f"v2 ledger is invalid: {exc}") from exc
    implementation = ledger["implementation"]
    _require(implementation["repository_contract_sha256"] == contract_sha256, "ledger repository-contract hash differs")
    _require(implementation["scanner_sha256"] == scanner_sha256, "ledger scanner hash differs")
    _require(implementation["schema_sha256"] == eligibility_schema_sha256, "ledger schema hash differs")
    _require(ledger["repository"] == contract_entry, "ledger repository binding differs from contract")
    _require(ledger["execution_status"] == "not_executed", "ledger contains candidate execution")
    _require(
        ledger["summary"]["runner_ready_count"] == ledger["summary"]["candidate_count"],
        "ledger contains a structurally blocked candidate",
    )
    _require(
        all(
            item["runner_readiness"] == "structurally_ready_not_executed"
            and item["negative_control_status"] == "not_executed"
            for item in ledger["candidates"]
        ),
        "ledger candidate readiness or execution status differs",
    )
    return ledger, raw


def _registry_dependency(
    registry_path: pathlib.Path | None,
    registry_schema_path: pathlib.Path | None,
    ledgers: Sequence[tuple[dict[str, Any], bytes, pathlib.Path]],
    *,
    git_binding: dict[str, Any],
    scanner_sha256: str,
) -> dict[str, Any] | None:
    if registry_path is None or registry_schema_path is None:
        _require(registry_path is None and registry_schema_path is None, "registry and registry schema must be supplied together")
        return None
    registry, raw = _load_json(registry_path)
    try:
        overlap.validate_registry(registry)
    except overlap.OverlapRegistryError as exc:
        raise RunPlanError(f"overlap registry is invalid: {exc}") from exc
    try:
        builder_hash = _sha256(pathlib.Path(overlap.__file__).read_bytes())
        cli_scanner_hash = _sha256(pathlib.Path(cli_eligibility.__file__).read_bytes())
        cli_population_hash = _sha256(pathlib.Path(cli_population.__file__).read_bytes())
        registry_schema_hash = _sha256(registry_schema_path.read_bytes())
    except OSError as exc:
        raise RunPlanError(f"cannot read overlap-registry dependency: {exc}") from exc
    implementation = registry["implementation"]
    _require(implementation["cli_scanner_sha256"] == cli_scanner_hash, "overlap-registry CLI scanner hash differs")
    _require(implementation["cli_task_population_sha256"] == cli_population_hash, "overlap-registry CLI canonical-helper hash differs")
    _require(implementation["git_binary_sha256"] == git_binding["binary_sha256"], "overlap-registry Git binary hash differs")
    _require(implementation["git_version_output"] == git_binding["version_output"], "overlap-registry Git version differs")
    _require(implementation["registry_builder_sha256"] == builder_hash, "overlap-registry builder hash differs")
    _require(implementation["schema_sha256"] == registry_schema_hash, "overlap-registry schema hash differs")
    _require(implementation["v2_scanner_sha256"] == scanner_sha256, "overlap-registry v2 scanner hash differs")
    registry_inputs = {
        item["repository_id"]: item
        for item in registry["inputs"]
        if item["profile"] == eligibility.PROFILE
    }
    _require(len(registry_inputs) == 3, "overlap registry does not contain exactly three v2 inputs")
    planned_refs: set[str] = set()
    for ledger, ledger_raw, ledger_path in ledgers:
        repository_id = ledger["repository"]["repository_id"]
        item = registry_inputs.get(repository_id)
        _require(item is not None, "planned ledger is absent from overlap registry")
        item = cast(dict[str, Any], item)
        _require(item["artifact_file"] == _artifact_name(ledger_path), "overlap-registry ledger file differs")
        _require(item["artifact_sha256"] == _sha256(ledger_raw), "overlap-registry ledger raw hash differs")
        _require(item["ledger_sha256"] == ledger["ledger_sha256"], "overlap-registry ledger self hash differs")
        _require(item["candidate_count"] == len(ledger["candidates"]), "overlap-registry ledger count differs")
        planned_refs.update(candidate["candidate_ref"] for candidate in ledger["candidates"])
    registry_refs = {
        item["candidate_ref"]
        for item in registry["candidates"]
        if item["source_profile"] == eligibility.PROFILE
    }
    _require(registry_refs == planned_refs, "overlap-registry v2 candidate set differs")
    return {
        "artifact_file": _artifact_name(registry_path),
        "artifact_sha256": _sha256(raw),
        "builder_sha256": builder_hash,
        "cli_scanner_sha256": cli_scanner_hash,
        "cli_task_population_sha256": cli_population_hash,
        "git_binary_sha256": git_binding["binary_sha256"],
        "git_version_output": git_binding["version_output"],
        "profile": registry["profile"],
        "registry_sha256": registry["registry_sha256"],
        "schema_file": _artifact_name(registry_schema_path),
        "schema_sha256": registry_schema_hash,
        "schema_version": registry["schema_version"],
    }


def build_plan(
    *,
    contract_path: pathlib.Path,
    eligibility_schema_path: pathlib.Path,
    ledger_paths: Sequence[pathlib.Path],
    plan_schema_path: pathlib.Path,
    registry_path: pathlib.Path | None = None,
    registry_schema_path: pathlib.Path | None = None,
) -> dict[str, Any]:
    _require(len(ledger_paths) == len(REPOSITORY_ORDER), "exactly three v2 ledgers are required")
    try:
        scanner_sha256 = _sha256(pathlib.Path(eligibility.__file__).read_bytes())
        eligibility_schema_raw = eligibility_schema_path.read_bytes()
    except OSError as exc:
        raise RunPlanError(f"cannot read v2 inventory dependency: {exc}") from exc
    eligibility_schema_sha256 = _sha256(eligibility_schema_raw)
    contract_entries, contract_dependency = _load_contract(contract_path)
    git_bindings = [
        contract_entries[key]["toolchain"]["git"] for key in REPOSITORY_ORDER
    ]
    _require(
        all(binding == git_bindings[0] for binding in git_bindings),
        "repository contract does not bind one exact Git toolchain",
    )
    by_key: dict[str, tuple[dict[str, Any], bytes, pathlib.Path]] = {}
    for path in ledger_paths:
        ledger_preview, _ = _load_json(path)
        repository = ledger_preview.get("repository")
        _require(isinstance(repository, dict), "ledger repository binding is missing")
        repository = cast(dict[str, Any], repository)
        key = repository.get("key")
        _require(isinstance(key, str) and key in contract_entries, "ledger repository key is unplanned")
        key = cast(str, key)
        _require(key not in by_key, "duplicate planned repository ledger")
        ledger, raw = _load_ledger(
            path,
            contract_entry=contract_entries[key],
            contract_sha256=contract_dependency["artifact_sha256"],
            scanner_sha256=scanner_sha256,
            eligibility_schema_sha256=eligibility_schema_sha256,
        )
        by_key[key] = (ledger, raw, path)
    _require(set(by_key) == set(REPOSITORY_ORDER), "planned repository set differs")

    repositories: list[dict[str, Any]] = []
    ordered_ledgers: list[tuple[dict[str, Any], bytes, pathlib.Path]] = []
    all_refs: list[str] = []
    for key in REPOSITORY_ORDER:
        ledger, raw, path = by_key[key]
        ordered_ledgers.append((ledger, raw, path))
        candidates = ledger["candidates"]
        _require(len(candidates) == EXPECTED_COUNTS[key], f"{key} candidate count differs")
        order = [
            {
                "candidate_ref": candidate["candidate_ref"],
                "first_parent_position": candidate["first_parent_position"],
            }
            for candidate in candidates
        ]
        refs = [item["candidate_ref"] for item in order]
        all_refs.extend(refs)
        repository = ledger["repository"]
        repositories.append(
            {
                "base_oid": repository["window"]["base_oid"],
                "candidate_count": len(candidates),
                "candidate_order": order,
                "candidate_order_sha256": _canonical_hash(order),
                "head_oid": repository["window"]["head_oid"],
                "key": key,
                "ledger_file": _artifact_name(path),
                "ledger_file_sha256": _sha256(raw),
                "ledger_sha256": ledger["ledger_sha256"],
                "repository_id": repository["repository_id"],
                "toolchain": copy.deepcopy(repository["toolchain"]),
            }
        )
    _require(len(all_refs) == TOTAL_CANDIDATES and len(set(all_refs)) == TOTAL_CANDIDATES, "global candidate order is not exactly 62 unique identities")
    _verify_schema_repository_projection(plan_schema_path, repositories)

    registry_dependency = _registry_dependency(
        registry_path,
        registry_schema_path,
        ordered_ledgers,
        git_binding=git_bindings[0],
        scanner_sha256=scanner_sha256,
    )
    plan = {
        "authority": copy.deepcopy(AUTHORITY),
        "cache_seed": copy.deepcopy(CACHE_SEED),
        "dependencies": {
            "eligibility_scanner": {
                "artifact_file": _artifact_name(pathlib.Path(eligibility.__file__)),
                "artifact_sha256": scanner_sha256,
            },
            "eligibility_schema": {
                "artifact_file": _artifact_name(eligibility_schema_path),
                "artifact_sha256": eligibility_schema_sha256,
            },
            "overlap_registry": registry_dependency,
            "repository_contract": contract_dependency,
        },
        "execution_protocol": copy.deepcopy(EXECUTION_PROTOCOL),
        "exposure": EXPOSURE,
        "implementation": _implementation_identity(plan_schema_path),
        "isolation": copy.deepcopy(ISOLATION),
        "pending_gates": list(PENDING_GATES),
        "plan_sha256": None,
        "private_log_policy": copy.deepcopy(PRIVATE_LOG_POLICY),
        "profile": PROFILE,
        "repositories": repositories,
        "resource_budget": copy.deepcopy(RESOURCE_BUDGET),
        "schema_version": SCHEMA_VERSION,
        "status": STATUS,
    }
    plan["plan_sha256"] = _self_hash(plan)
    validate_plan(plan)
    return plan


def _validate_artifact_binding(value: Any, field: str) -> None:
    _require(isinstance(value, dict) and set(value) == {"artifact_file", "artifact_sha256"}, f"{field} fields differ")
    _require(isinstance(value["artifact_file"], str) and pathlib.PurePosixPath(value["artifact_file"]).name == value["artifact_file"], f"{field}.artifact_file is invalid")
    _require(_valid_sha(value["artifact_sha256"]), f"{field}.artifact_sha256 is invalid")


def validate_plan(value: dict[str, Any]) -> None:
    expected_root = {
        "authority", "cache_seed", "dependencies", "execution_protocol", "exposure",
        "implementation", "isolation", "pending_gates", "plan_sha256", "private_log_policy",
        "profile", "repositories", "resource_budget", "schema_version", "status",
    }
    _require(isinstance(value, dict) and set(value) == expected_root, "run-plan root fields differ")
    _require(value["schema_version"] == SCHEMA_VERSION and value["profile"] == PROFILE, "run-plan profile differs")
    _require(value["status"] == STATUS, "authorized run plans are rejected until an approval receipt and trust mechanism exist")
    _require(value["exposure"] == EXPOSURE, "run-plan exposure differs")
    _require(value["authority"] == AUTHORITY, "run-plan authority boundary differs")
    _require(value["execution_protocol"] == EXECUTION_PROTOCOL, "run-plan execution protocol differs")
    _require(value["resource_budget"] == RESOURCE_BUDGET, "run-plan resource budget differs")
    _require(
        value["resource_budget"]["minimum_free_disk_before_staging_bytes"]
        == value["resource_budget"]["minimum_free_disk_reserve_bytes"]
        + value["resource_budget"]["max_total_staging_bytes"],
        "run-plan free-disk preflight arithmetic differs",
    )
    _require(
        value["resource_budget"]["max_worktree_bytes_per_arm"]
        + value["resource_budget"]["max_cache_bytes_per_arm"]
        + value["resource_budget"]["max_private_raw_log_bytes_per_arm"]
        <= value["resource_budget"]["max_total_staging_bytes"],
        "run-plan per-arm resource ceilings exceed total staging",
    )
    _require(value["cache_seed"] == CACHE_SEED, "run-plan cache-seed gate differs")
    _require(value["isolation"] == ISOLATION, "run-plan isolation claims differ")
    _require(value["private_log_policy"] == PRIVATE_LOG_POLICY, "run-plan private-log policy differs")
    _require(value["pending_gates"] == PENDING_GATES, "run-plan pending gates differ")
    _require(_valid_sha(value["plan_sha256"]), "plan_sha256 is invalid")
    _require(value["plan_sha256"] == _self_hash(value), "run-plan self hash mismatch")

    implementation = value["implementation"]
    expected_implementation = {
        "artifact_render_profile", "builder_sha256", "canonical_json_profile", "schema_sha256",
    }
    _require(isinstance(implementation, dict) and set(implementation) == expected_implementation, "run-plan implementation fields differ")
    _require(implementation["artifact_render_profile"] == ARTIFACT_RENDER_PROFILE, "artifact render profile differs")
    _require(implementation["canonical_json_profile"] == CANONICAL_JSON_PROFILE, "canonical JSON profile differs")
    for field in ("builder_sha256", "schema_sha256"):
        _require(_valid_sha(implementation[field]), f"implementation.{field} is invalid")

    dependencies = value["dependencies"]
    _require(
        isinstance(dependencies, dict)
        and set(dependencies) == {"eligibility_scanner", "eligibility_schema", "overlap_registry", "repository_contract"},
        "run-plan dependency fields differ",
    )
    _validate_artifact_binding(dependencies["eligibility_scanner"], "eligibility_scanner")
    _validate_artifact_binding(dependencies["eligibility_schema"], "eligibility_schema")
    contract = dependencies["repository_contract"]
    _require(
        isinstance(contract, dict)
        and set(contract) == {"artifact_file", "artifact_sha256", "profile", "schema_version"},
        "repository-contract dependency fields differ",
    )
    _require(contract["profile"] == "agent_brain_development_repository_bindings_v2" and contract["schema_version"] == 1, "repository-contract dependency profile differs")
    _require(isinstance(contract["artifact_file"], str) and pathlib.PurePosixPath(contract["artifact_file"]).name == contract["artifact_file"], "repository-contract artifact file is invalid")
    _require(_valid_sha(contract["artifact_sha256"]), "repository-contract artifact hash is invalid")
    registry = dependencies["overlap_registry"]
    if registry is not None:
        expected_registry = {
            "artifact_file", "artifact_sha256", "builder_sha256", "cli_scanner_sha256",
            "cli_task_population_sha256", "git_binary_sha256", "git_version_output", "profile",
            "registry_sha256", "schema_file", "schema_sha256", "schema_version",
        }
        _require(isinstance(registry, dict) and set(registry) == expected_registry, "overlap-registry dependency fields differ")
        _require(registry["profile"] == overlap.PROFILE and registry["schema_version"] == 1, "overlap-registry dependency profile differs")
        for field in ("artifact_file", "schema_file"):
            _require(isinstance(registry[field], str) and pathlib.PurePosixPath(registry[field]).name == registry[field], f"overlap_registry.{field} is invalid")
        for field in (
            "artifact_sha256", "builder_sha256", "cli_scanner_sha256",
            "cli_task_population_sha256", "git_binary_sha256", "registry_sha256", "schema_sha256",
        ):
            _require(_valid_sha(registry[field]), f"overlap_registry.{field} is invalid")
        _require(
            isinstance(registry["git_version_output"], str)
            and registry["git_version_output"].startswith("git version "),
            "overlap_registry.git_version_output is invalid",
        )

    repositories = value["repositories"]
    _require(isinstance(repositories, list) and len(repositories) == 3, "run plan must contain exactly three repositories")
    _require([item.get("key") if isinstance(item, dict) else None for item in repositories] == list(REPOSITORY_ORDER), "repository order differs")
    all_refs: list[str] = []
    expected_repository_fields = {
        "base_oid", "candidate_count", "candidate_order", "candidate_order_sha256", "head_oid",
        "key", "ledger_file", "ledger_file_sha256", "ledger_sha256", "repository_id", "toolchain",
    }
    for index, repository in enumerate(repositories):
        key = REPOSITORY_ORDER[index]
        _require(isinstance(repository, dict) and set(repository) == expected_repository_fields, f"repository[{index}] fields differ")
        _require(repository["key"] == key and repository["candidate_count"] == EXPECTED_COUNTS[key], f"repository[{index}] identity or count differs")
        _require(isinstance(repository["repository_id"], str) and repository["repository_id"].startswith("github.com/"), f"repository[{index}].repository_id is invalid")
        _require(isinstance(repository["ledger_file"], str) and pathlib.PurePosixPath(repository["ledger_file"]).name == repository["ledger_file"], f"repository[{index}].ledger_file is invalid")
        for field in ("ledger_file_sha256", "ledger_sha256", "candidate_order_sha256"):
            _require(_valid_sha(repository[field]), f"repository[{index}].{field} is invalid")
        for field in ("base_oid", "head_oid"):
            item = repository[field]
            _require(isinstance(item, str) and eligibility.OID_RE.fullmatch(item) is not None and item != "0" * 40, f"repository[{index}].{field} is invalid")
        order = repository["candidate_order"]
        _require(isinstance(order, list) and len(order) == EXPECTED_COUNTS[key], f"repository[{index}] candidate order count differs")
        positions: list[int] = []
        refs: list[str] = []
        for candidate_index, candidate in enumerate(order):
            _require(isinstance(candidate, dict) and set(candidate) == {"candidate_ref", "first_parent_position"}, f"repository[{index}].candidate_order[{candidate_index}] fields differ")
            _require(_valid_sha(candidate["candidate_ref"]), f"repository[{index}] candidate reference is invalid")
            position = candidate["first_parent_position"]
            _require(type(position) is int and 1 <= position <= 31, f"repository[{index}] candidate position is invalid")
            positions.append(position)
            refs.append(candidate["candidate_ref"])
        _require(positions == sorted(set(positions)), f"repository[{index}] candidate positions are not canonical")
        _require(len(refs) == len(set(refs)), f"repository[{index}] candidate references collide")
        _require(repository["candidate_order_sha256"] == _canonical_hash(order), f"repository[{index}] candidate-order hash differs")
        all_refs.extend(refs)
        toolchain = repository["toolchain"]
        try:
            eligibility._validate_repository_entry(
                {
                    "key": repository["key"],
                    "remote": {"name": "synthetic", "ref": "refs/remotes/synthetic/main", "url": "https://github.com/placeholder/placeholder.git"},
                    "repository_id": repository["repository_id"],
                    "toolchain": toolchain,
                    "window": {"base_oid": repository["base_oid"], "first_parent_unit_count": 31, "head_oid": repository["head_oid"]},
                }
            )
        except eligibility.EligibilityV2Error as exc:
            raise RunPlanError(f"repository[{index}] toolchain binding is invalid: {exc}") from exc
    _require(len(all_refs) == TOTAL_CANDIDATES and len(set(all_refs)) == TOTAL_CANDIDATES, "global candidate order is not exactly 62 unique identities")


def verify_plan_dependencies(
    plan: dict[str, Any],
    *,
    contract_path: pathlib.Path,
    eligibility_schema_path: pathlib.Path,
    ledger_paths: Sequence[pathlib.Path],
    plan_schema_path: pathlib.Path,
    registry_path: pathlib.Path | None = None,
    registry_schema_path: pathlib.Path | None = None,
) -> None:
    validate_plan(plan)
    rebuilt = build_plan(
        contract_path=contract_path,
        eligibility_schema_path=eligibility_schema_path,
        ledger_paths=ledger_paths,
        plan_schema_path=plan_schema_path,
        registry_path=registry_path,
        registry_schema_path=registry_schema_path,
    )
    _require(plan == rebuilt, "run plan differs from exact dependency rebuild")


def _write_atomic(path: pathlib.Path, value: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    rendered = _render(value)
    descriptor, temporary_name = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent)
    temporary = pathlib.Path(temporary_name)
    try:
        os.fchmod(descriptor, 0o644)
        with os.fdopen(descriptor, "wb") as handle:
            handle.write(rendered)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
    finally:
        temporary.unlink(missing_ok=True)


def _common_arguments(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--contract", required=True, type=pathlib.Path)
    parser.add_argument("--eligibility-schema", required=True, type=pathlib.Path)
    parser.add_argument("--ledger", required=True, action="append", type=pathlib.Path)
    parser.add_argument("--plan-schema", required=True, type=pathlib.Path)
    parser.add_argument("--registry", type=pathlib.Path)
    parser.add_argument("--registry-schema", type=pathlib.Path)


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="command", required=True)
    build = subparsers.add_parser("build", help="build the deterministic pending plan")
    _common_arguments(build)
    build.add_argument("--output", required=True, type=pathlib.Path)
    check = subparsers.add_parser("check", help="check exact plan bytes and dependencies")
    check.add_argument("plan", type=pathlib.Path)
    _common_arguments(check)
    args = parser.parse_args(argv)
    try:
        kwargs = {
            "contract_path": args.contract,
            "eligibility_schema_path": args.eligibility_schema,
            "ledger_paths": args.ledger,
            "plan_schema_path": args.plan_schema,
            "registry_path": args.registry,
            "registry_schema_path": args.registry_schema,
        }
        if args.command == "build":
            plan = build_plan(**kwargs)
            _write_atomic(args.output, plan)
        else:
            plan, raw = _load_json(args.plan)
            _require(raw == _render(plan), "run-plan artifact bytes are not canonical")
            verify_plan_dependencies(plan, **kwargs)
    except RunPlanError as exc:
        parser.error(str(exc))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
