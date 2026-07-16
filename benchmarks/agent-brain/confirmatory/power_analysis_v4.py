#!/usr/bin/env python3
"""Final development calibration and four-arm power analysis v4.

V4 is a standalone development lane.  It consumes a completed product-cycle
v1 manifest plus a hash-bound projection of the canonical task-population v2
contract.  It never changes the locked three-arm preregistration or power-v3
artifact, authorizes no spend, and makes no provider or model call.

Power is estimated with deterministic shared resampling of independent task
clusters.  Repetition means are formed inside task/arm before contrasts.  Time
uses paired task log ratios, normalized cost preserves the frozen ratio of
equal-task-weighted arithmetic arm means, and quality uses paired task mean
differences.  Joint power is the same-draw frequency of all three endpoints
clearing their one-sided bounds; marginal powers are never multiplied.
"""

from __future__ import annotations

import argparse
from collections import defaultdict
from decimal import Decimal
import hashlib
import importlib.util
import json
import math
import pathlib
import sys
from typing import Any, Mapping, Sequence


HERE = pathlib.Path(__file__).resolve().parent
PRODUCT_SPEC = importlib.util.spec_from_file_location(
    "power_v4_product_cycle", HERE / "product_cycle.py"
)
assert PRODUCT_SPEC and PRODUCT_SPEC.loader
PRODUCT = importlib.util.module_from_spec(PRODUCT_SPEC)
PRODUCT_SPEC.loader.exec_module(PRODUCT)

CALIBRATION_SCHEMA = "agent-brain-final-calibration/v1"
REPORT_SCHEMA = "agent-brain-power-analysis/v4"
TASK_POPULATION_PROFILE = "agent_brain_task_population_v2"
PRODUCT_CYCLE_SCHEMA_SHA256 = (
    "3e029222e76091740bb1e218a7b1e23fe464f98aa2c83797d3fe71ad0b97c288"
)
TASK_POPULATION_SCHEMA_SHA256 = (
    "2846906e0caa450e6c4dbc206648346ababbb91fed4675c0c7eca03cf506a8b9"
)
MIN_CALIBRATION_CLUSTERS = 12
TARGET_POWER = 0.80
CONFIDENCE = 0.95
MIN_RESAMPLES = 100
MAX_RESAMPLES = 10_000
MAX_CANDIDATE_CLUSTERS = 512
RESAMPLING_METHOD = "paired_cluster_residual_bootstrap_shared_draws_v1"
RESAMPLING_DOMAIN = b"entire-brain/power-analysis-v4/shared-cluster-resample\0"
PRIMARY_COMPARISON = "retrieved_memory_vs_no_memory"
DIAGNOSTIC_COMPARISON = "retrieved_memory_vs_preoptimization_memory"
ENDPOINTS = ("elapsed_time", "normalized_cost", "code_quality")
MAX_JSON_BYTES = 64 * 1024 * 1024


class PowerV4Error(ValueError):
    """Raised when calibration or power evidence cannot safely be used."""


def _require(condition: bool, message: str) -> None:
    if not condition:
        raise PowerV4Error(message)


def _duplicate_key_guard(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    value: dict[str, Any] = {}
    for key, item in pairs:
        _require(key not in value, "JSON contains a duplicate object key")
        value[key] = item
    return value


def load_calibration(path: pathlib.Path) -> dict[str, Any]:
    try:
        _require(path.is_file() and not path.is_symlink(), "calibration must be a regular non-symlink file")
        _require(path.stat().st_size <= MAX_JSON_BYTES, "calibration exceeds the size bound")
        value = json.loads(
            path.read_text(encoding="utf-8"), object_pairs_hook=_duplicate_key_guard
        )
    except PowerV4Error:
        raise
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        raise PowerV4Error(f"calibration cannot be loaded: {exc}") from exc
    _require(isinstance(value, dict), "calibration root must be an object")
    return value


def load_power_report(path: pathlib.Path) -> dict[str, Any]:
    try:
        _require(path.is_file() and not path.is_symlink(), "power report must be a regular non-symlink file")
        _require(path.stat().st_size <= MAX_JSON_BYTES, "power report exceeds the size bound")
        value = json.loads(
            path.read_text(encoding="utf-8"), object_pairs_hook=_duplicate_key_guard
        )
    except PowerV4Error:
        raise
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        raise PowerV4Error(f"power report cannot be loaded: {exc}") from exc
    _require(isinstance(value, dict), "power report root must be an object")
    return value


def _exact_keys(value: Any, expected: set[str], label: str) -> dict[str, Any]:
    _require(isinstance(value, dict), f"{label} must be an object")
    actual = set(value)
    missing = sorted(expected - actual)
    extra = sorted(actual - expected)
    _require(not missing and not extra, f"{label} fields mismatch: missing={missing}, extra={extra}")
    return value


def _sha256(value: Any, label: str) -> str:
    try:
        return PRODUCT._sha256(value, label)
    except PRODUCT.ProductCycleError as exc:
        raise PowerV4Error(str(exc)) from exc


def _self_sha256(value: Mapping[str, Any], field: str = "identity_sha256") -> str:
    return PRODUCT.self_sha256(value, field)


def _nonempty(value: Any, label: str) -> str:
    _require(isinstance(value, str) and bool(value), f"{label} must be a non-empty string")
    return value


def _positive_int(value: Any, label: str, *, maximum: int | None = None) -> int:
    _require(type(value) is int and value > 0, f"{label} must be a positive integer")
    if maximum is not None:
        _require(value <= maximum, f"{label} exceeds {maximum}")
    return value


def _finite(value: Any, label: str) -> float:
    _require(type(value) in {int, float}, f"{label} must be a finite number")
    rendered = float(value)
    _require(math.isfinite(rendered), f"{label} must be finite")
    return rendered


def _stable_float(value: float | Decimal) -> float:
    rendered = float(value)
    _require(math.isfinite(rendered), "analysis produced a non-finite result")
    return float(format(rendered, ".12g"))


def _task_identity_sha256(task_id: str) -> str:
    return hashlib.sha256(
        b"entire-brain/task-population-v2/task-id\0" + task_id.encode("utf-8")
    ).hexdigest()


def _validate_population_binding(value: Any) -> tuple[list[str], dict[str, dict[str, Any]]]:
    binding = _exact_keys(
        value,
        {
            "schema_version",
            "profile",
            "status",
            "schema_sha256",
            "population_sha256",
            "source_file_sha256",
            "review_ledger_sha256",
            "selection_verification_status",
            "selection_receipt_sha256",
            "assignment_verification_status",
            "assignment_receipt_sha256",
            "overlap_commitment_verification_status",
            "overlap_commitment_receipt_sha256",
            "calibration_min_independent_tasks",
            "holdout_plaintext",
            "calibration_members",
            "identity_sha256",
        },
        "task_population_binding",
    )
    _require(
        type(binding["schema_version"]) is int and binding["schema_version"] == 2,
        "task-population schema version changed",
    )
    _require(binding["profile"] == TASK_POPULATION_PROFILE, "task-population profile changed")
    _require(
        binding["status"] in {"candidate_unopened", "frozen_unopened"},
        "final calibration requires a candidate-locked task population",
    )
    _require(
        _sha256(binding["schema_sha256"], "task_population_binding.schema_sha256")
        == TASK_POPULATION_SCHEMA_SHA256,
        "task-population v2 schema identity changed",
    )
    _sha256(binding["population_sha256"], "task_population_binding.population_sha256")
    _sha256(
        binding["source_file_sha256"],
        "task_population_binding.source_file_sha256",
    )
    _sha256(binding["review_ledger_sha256"], "task_population_binding.review_ledger_sha256")
    for field in (
        "selection_verification_status",
        "assignment_verification_status",
        "overlap_commitment_verification_status",
    ):
        _require(binding[field] == "verified", f"task_population_binding.{field} must be verified")
    for field in (
        "selection_receipt_sha256",
        "assignment_receipt_sha256",
        "overlap_commitment_receipt_sha256",
    ):
        _sha256(binding[field], f"task_population_binding.{field}")
    _require(
        type(binding["calibration_min_independent_tasks"]) is int
        and binding["calibration_min_independent_tasks"]
        == MIN_CALIBRATION_CLUSTERS,
        "task-population calibration floor changed",
    )
    _require(binding["holdout_plaintext"] == "forbidden", "holdout plaintext policy changed")
    members = binding["calibration_members"]
    _require(isinstance(members, list), "task_population_binding.calibration_members must be a list")
    _require(
        len(members) >= MIN_CALIBRATION_CLUSTERS,
        "final calibration requires at least 12 independent active task clusters",
    )
    task_ids: list[str] = []
    by_task: dict[str, dict[str, Any]] = {}
    member_refs: set[str] = set()
    task_identity_commitments: set[str] = set()
    overlap_commitments: set[str] = set()
    for index, raw in enumerate(members):
        label = f"task_population_binding.calibration_members[{index}]"
        member = _exact_keys(
            raw,
            {
                "member_ref",
                "membership",
                "task_id",
                "task_id_sha256",
                "product_task_sha256",
                "task_overlap_commitment_sha256",
                "contamination_state",
                "review_commitment_sha256",
                "identity_sha256",
            },
            label,
        )
        member_ref = _sha256(member["member_ref"], f"{label}.member_ref")
        _require(member_ref not in member_refs, "calibration member_ref is duplicated")
        member_refs.add(member_ref)
        _require(
            member["membership"] == "development_calibration",
            "optimization or holdout members are prohibited from final calibration",
        )
        _require(
            member["contamination_state"] == "reviewed_clear",
            "final calibration accepts only active reviewed-clear members",
        )
        task_id = _nonempty(member["task_id"], f"{label}.task_id")
        _require(task_id not in by_task, f"duplicate calibration task_id {task_id!r}")
        task_identity = _sha256(member["task_id_sha256"], f"{label}.task_id_sha256")
        _require(
            task_identity == _task_identity_sha256(task_id),
            f"{label}.task_id_sha256 is not bound to task_id",
        )
        _require(task_identity not in task_identity_commitments, "calibration task identity is duplicated")
        task_identity_commitments.add(task_identity)
        _sha256(member["product_task_sha256"], f"{label}.product_task_sha256")
        overlap = _sha256(
            member["task_overlap_commitment_sha256"],
            f"{label}.task_overlap_commitment_sha256",
        )
        _require(overlap not in overlap_commitments, "calibration task-overlap commitment is duplicated")
        overlap_commitments.add(overlap)
        _sha256(member["review_commitment_sha256"], f"{label}.review_commitment_sha256")
        digest = _sha256(member["identity_sha256"], f"{label}.identity_sha256")
        _require(digest == _self_sha256(member), f"{label} identity hash mismatch")
        task_ids.append(task_id)
        by_task[task_id] = member
    _require(task_ids == sorted(task_ids), "calibration members must be ordered by task_id")
    digest = _sha256(binding["identity_sha256"], "task_population_binding.identity_sha256")
    _require(digest == _self_sha256(binding), "task-population binding identity hash mismatch")
    return task_ids, by_task


def _precalibration_plan_sha256(product_root: Mapping[str, Any]) -> str:
    """Hash only inputs that exist before any calibration outcome is opened."""
    design = product_root["design"]
    return PRODUCT.value_sha256(
        {
            "schema": "agent-brain-product-cycle-precalibration-plan/v1",
            "purpose": product_root["purpose"],
            "evidence_class": product_root["evidence_class"],
            "claim_scope": product_root["claim_scope"],
            "product_identity_binding_sha256": product_root["product_identities"][
                "binding_sha256"
            ],
            "task_inventory_sha256": product_root["task_inventory_sha256"],
            "shared_execution_identity_sha256": product_root["shared_execution"][
                "identity_sha256"
            ],
            "arms": design["arms"],
            "arm_product_roles": design["arm_product_roles"],
            "primary_contrast": design["primary_contrast"],
            "diagnostic_contrast": design["diagnostic_contrast"],
            "task_clusters": design["task_clusters"],
            "repetitions_per_arm": design["repetitions_per_arm"],
            "requested_cells": design["requested_cells"],
            "agent_retry_limit": design["agent_retry_limit"],
            "replacement_cell_limit": design["replacement_cell_limit"],
            "reserve_cell_limit": design["reserve_cell_limit"],
            "maximum_agent_invocations": design["maximum_agent_invocations"],
            "budget": design["budget"],
            "schedule_sha256": product_root["shared_execution"]["schedule_sha256"],
        }
    )


def _validate_candidate_lock(
    value: Any,
    *,
    product_root: Mapping[str, Any],
    population_binding: Mapping[str, Any],
) -> dict[str, Any]:
    lock = _exact_keys(
        value,
        {
            "status",
            "candidate_product_identity_sha256",
            "precalibration_plan_sha256",
            "task_population_contract_sha256",
            "lock_receipt_sha256",
            "identity_sha256",
        },
        "candidate_lock",
    )
    _require(
        lock["status"] == "verified_locked_before_development_calibration",
        "candidate lock must be verified before development calibration",
    )
    candidate_sha = _sha256(
        lock["candidate_product_identity_sha256"],
        "candidate_lock.candidate_product_identity_sha256",
    )
    _require(
        candidate_sha
        == product_root["product_identities"]["candidate"]["identity_sha256"],
        "candidate lock product identity drift",
    )
    plan_sha = _sha256(
        lock["precalibration_plan_sha256"],
        "candidate_lock.precalibration_plan_sha256",
    )
    _require(
        plan_sha == _precalibration_plan_sha256(product_root),
        "candidate lock pre-calibration plan drift",
    )
    population_sha = _sha256(
        lock["task_population_contract_sha256"],
        "candidate_lock.task_population_contract_sha256",
    )
    _require(
        population_sha == population_binding["population_sha256"],
        "candidate lock task-population contract drift",
    )
    _sha256(lock["lock_receipt_sha256"], "candidate_lock.lock_receipt_sha256")
    digest = _sha256(lock["identity_sha256"], "candidate_lock.identity_sha256")
    _require(digest == _self_sha256(lock), "candidate lock identity hash mismatch")
    return lock


def _validate_source_byte_bindings(
    value: Any,
    *,
    product_root: Mapping[str, Any],
    population_binding: Mapping[str, Any],
) -> dict[str, Any]:
    bindings = _exact_keys(
        value,
        {
            "product_cycle_schema_file_sha256",
            "product_cycle_evidence_canonical_bytes_sha256",
            "task_population_schema_file_sha256",
            "task_population_contract_file_sha256",
            "task_population_contract_sha256",
            "task_population_binding_canonical_bytes_sha256",
            "identity_sha256",
        },
        "source_byte_bindings",
    )
    expected = {
        "product_cycle_schema_file_sha256": PRODUCT_CYCLE_SCHEMA_SHA256,
        "product_cycle_evidence_canonical_bytes_sha256": PRODUCT.value_sha256(
            product_root
        ),
        "task_population_schema_file_sha256": TASK_POPULATION_SCHEMA_SHA256,
        "task_population_contract_file_sha256": population_binding[
            "source_file_sha256"
        ],
        "task_population_contract_sha256": population_binding["population_sha256"],
        "task_population_binding_canonical_bytes_sha256": PRODUCT.value_sha256(
            population_binding
        ),
    }
    for field, expected_value in expected.items():
        actual = _sha256(bindings[field], f"source_byte_bindings.{field}")
        _require(actual == expected_value, f"source byte binding drift: {field}")
    digest = _sha256(
        bindings["identity_sha256"], "source_byte_bindings.identity_sha256"
    )
    _require(
        digest == _self_sha256(bindings),
        "source byte bindings identity hash mismatch",
    )
    return bindings


def _validate_locked_identities(
    value: Any,
    *,
    product_root: Mapping[str, Any],
    population_binding: Mapping[str, Any],
) -> dict[str, Any]:
    locked = _exact_keys(
        value,
        {
            "product_cycle_schema_sha256",
            "product_cycle_contract_sha256",
            "product_cycle_evidence_canonical_bytes_sha256",
            "task_population_schema_sha256",
            "task_population_sha256",
            "task_population_file_sha256",
            "candidate_product_identity_sha256",
            "candidate_packet_format_sha256",
            "corpus_sha256",
            "engine_sha256",
            "prompt_template_sha256",
            "prompt_parity_algorithm",
            "cache_policy_sha256",
            "runner_sha256",
            "model_id",
            "effort",
            "schedule_sha256",
            "price_quote_sha256",
            "pricing_policy_sha256",
            "identity_sha256",
        },
        "locked_identities",
    )
    candidate = product_root["product_identities"]["candidate"]
    shared = product_root["shared_execution"]
    expected = {
        "product_cycle_schema_sha256": PRODUCT_CYCLE_SCHEMA_SHA256,
        "product_cycle_contract_sha256": product_root["manifest_sha256"],
        "product_cycle_evidence_canonical_bytes_sha256": PRODUCT.value_sha256(
            product_root
        ),
        "task_population_schema_sha256": TASK_POPULATION_SCHEMA_SHA256,
        "task_population_sha256": population_binding["population_sha256"],
        "task_population_file_sha256": population_binding["source_file_sha256"],
        "candidate_product_identity_sha256": candidate["identity_sha256"],
        "candidate_packet_format_sha256": candidate["packet_format"]["sha256"],
        "corpus_sha256": shared["corpus_sha256"],
        "engine_sha256": shared["engine_sha256"],
        "prompt_template_sha256": shared["prompt_template_sha256"],
        "prompt_parity_algorithm": shared["prompt_parity_algorithm"],
        "cache_policy_sha256": shared["cache_policy_sha256"],
        "runner_sha256": shared["runner_sha256"],
        "model_id": shared["model_id"],
        "effort": shared["effort"],
        "schedule_sha256": shared["schedule_sha256"],
        "price_quote_sha256": shared["price_quote_sha256"],
        "pricing_policy_sha256": shared["pricing_policy_sha256"],
    }
    for field, expected_value in expected.items():
        if field.endswith("_sha256"):
            _sha256(locked[field], f"locked_identities.{field}")
        else:
            _nonempty(locked[field], f"locked_identities.{field}")
        _require(locked[field] == expected_value, f"locked identity drift: {field}")
    digest = _sha256(locked["identity_sha256"], "locked_identities.identity_sha256")
    _require(digest == _self_sha256(locked), "locked identities hash mismatch")
    return locked


def _validate_ratio_alternative(value: Any, label: str) -> dict[str, float]:
    endpoint = _exact_keys(
        value, {"required_ratio_max", "planning_alternative_ratio"}, label
    )
    required = _finite(endpoint["required_ratio_max"], f"{label}.required_ratio_max")
    alternative = _finite(
        endpoint["planning_alternative_ratio"], f"{label}.planning_alternative_ratio"
    )
    _require(0 < alternative < required < 1, f"{label} alternative must be strictly better than a ratio floor in (0,1)")
    return {"required_ratio_max": required, "planning_alternative_ratio": alternative}


def _validate_quality_alternative(value: Any, label: str) -> dict[str, float]:
    endpoint = _exact_keys(
        value, {"required_difference_min", "planning_alternative_difference"}, label
    )
    required = _finite(
        endpoint["required_difference_min"], f"{label}.required_difference_min"
    )
    alternative = _finite(
        endpoint["planning_alternative_difference"],
        f"{label}.planning_alternative_difference",
    )
    _require(0 < required < alternative <= 1, f"{label} alternative must be strictly better than a difference floor in (0,1]")
    return {
        "required_difference_min": required,
        "planning_alternative_difference": alternative,
    }


def _validate_contrast_alternatives(
    value: Any,
    *,
    label: str,
    comparison: str,
    role: str,
) -> dict[str, Any]:
    contrast = _exact_keys(
        value,
        {"comparison", "role", "elapsed_time", "normalized_cost", "code_quality"},
        label,
    )
    _require(contrast["comparison"] == comparison, f"{label}.comparison changed")
    _require(contrast["role"] == role, f"{label}.role changed")
    return {
        "comparison": comparison,
        "role": role,
        "elapsed_time": _validate_ratio_alternative(
            contrast["elapsed_time"], f"{label}.elapsed_time"
        ),
        "normalized_cost": _validate_ratio_alternative(
            contrast["normalized_cost"], f"{label}.normalized_cost"
        ),
        "code_quality": _validate_quality_alternative(
            contrast["code_quality"], f"{label}.code_quality"
        ),
    }


def _validate_planning(
    value: Any, *, calibration_repetitions: int, state: str
) -> tuple[dict[str, Any], dict[str, Any] | None]:
    planning = _exact_keys(
        value,
        {
            "method",
            "target_power",
            "confidence",
            "resamples",
            "seed",
            "candidate_task_clusters",
            "arms",
            "repetitions_per_arm",
            "agent_retry_limit",
            "replacement_cell_limit",
            "reserve_cell_limit",
            "planning_alternatives",
        },
        "planning",
    )
    _require(planning["method"] == RESAMPLING_METHOD, "power-v4 resampling method changed")
    _require(_finite(planning["target_power"], "planning.target_power") == TARGET_POWER, "target power must remain 0.80")
    _require(_finite(planning["confidence"], "planning.confidence") == CONFIDENCE, "confidence must remain 0.95")
    resamples = _positive_int(
        planning["resamples"], "planning.resamples", maximum=MAX_RESAMPLES
    )
    _require(resamples >= MIN_RESAMPLES, f"planning.resamples must be at least {MIN_RESAMPLES}")
    seed = planning["seed"]
    _require(type(seed) is int and 0 <= seed < 2**63, "planning.seed must be a nonnegative signed 63-bit integer")
    candidates = planning["candidate_task_clusters"]
    _require(isinstance(candidates, list) and bool(candidates), "candidate task counts must be a non-empty list")
    _require(
        all(type(item) is int and MIN_CALIBRATION_CLUSTERS <= item <= MAX_CANDIDATE_CLUSTERS for item in candidates),
        "candidate task counts must be integers in [12,512]",
    )
    _require(candidates == sorted(set(candidates)), "candidate task counts must be unique and increasing")
    _require(planning["arms"] == list(PRODUCT.ARMS), "power-v4 four-arm design changed")
    repetitions = _positive_int(planning["repetitions_per_arm"], "planning.repetitions_per_arm")
    _require(
        repetitions == calibration_repetitions,
        "planned repetitions must equal the calibration repetition contract",
    )
    for field in ("agent_retry_limit", "replacement_cell_limit", "reserve_cell_limit"):
        _require(
            type(planning[field]) is int and planning[field] == 0,
            f"planning.{field} must remain integer zero",
        )
    alternatives = planning["planning_alternatives"]
    if state == "pending":
        _require(alternatives is None, "pending calibration must retain null planning alternatives")
        return planning, None
    _require(isinstance(alternatives, dict), "candidate/frozen calibration requires planning alternatives")
    alternatives = _exact_keys(alternatives, {"primary", "product_diagnostic"}, "planning.planning_alternatives")
    validated = {
        "primary": _validate_contrast_alternatives(
            alternatives["primary"],
            label="planning.planning_alternatives.primary",
            comparison=PRIMARY_COMPARISON,
            role="primary_development_contrast",
        ),
        "product_diagnostic": _validate_contrast_alternatives(
            alternatives["product_diagnostic"],
            label="planning.planning_alternatives.product_diagnostic",
            comparison=DIAGNOSTIC_COMPARISON,
            role="diagnostic_product_progress_only",
        ),
    }
    return planning, validated


def _validated_calibration(
    calibration: Any,
) -> tuple[dict[str, Any], list[dict[str, Any]], dict[str, Any]]:
    root = _exact_keys(
        calibration,
        {
            "schema",
            "evidence_class",
            "state",
            "owner_approval_sha256",
            "candidate_lock",
            "source_byte_bindings",
            "locked_identities",
            "task_population_binding",
            "product_cycle_evidence",
            "planning",
            "identity_sha256",
        },
        "calibration",
    )
    _require(root["schema"] == CALIBRATION_SCHEMA, "final-calibration schema changed")
    evidence_class = root["evidence_class"]
    _require(
        evidence_class in {"synthetic_fixture", "development_measurement"},
        "calibration evidence_class is invalid",
    )
    state = root["state"]
    _require(state in {"pending", "candidate", "frozen"}, "calibration state is invalid")
    approval = root["owner_approval_sha256"]
    if state == "frozen":
        _require(evidence_class == "development_measurement", "synthetic calibration cannot be frozen")
        _sha256(approval, "calibration.owner_approval_sha256")
    else:
        _require(approval is None, "only a frozen calibration may carry owner approval")

    try:
        product_root, measurements, product_meta = PRODUCT._validated_manifest(
            root["product_cycle_evidence"]
        )
    except PRODUCT.ProductCycleError as exc:
        raise PowerV4Error(f"product-cycle evidence is invalid: {exc}") from exc
    _require(
        product_root["evidence_class"] == evidence_class,
        "calibration and product-cycle evidence classes differ",
    )
    population = root["task_population_binding"]
    task_ids, members = _validate_population_binding(population)
    _require(
        product_meta["task_ids"] == task_ids,
        "product-cycle tasks do not exactly match the calibration population projection",
    )
    product_tasks = {item["task_id"]: item for item in product_root["tasks"]}
    for task_id in task_ids:
        _require(
            members[task_id]["product_task_sha256"]
            == product_tasks[task_id]["task_sha256"],
            f"product task identity drift for {task_id}",
        )
    candidate_lock = _validate_candidate_lock(
        root["candidate_lock"],
        product_root=product_root,
        population_binding=population,
    )
    source_bindings = _validate_source_byte_bindings(
        root["source_byte_bindings"],
        product_root=product_root,
        population_binding=population,
    )
    locked = _validate_locked_identities(
        root["locked_identities"],
        product_root=product_root,
        population_binding=population,
    )
    planning, alternatives = _validate_planning(
        root["planning"],
        calibration_repetitions=product_meta["repetitions"],
        state=state,
    )
    if state == "frozen":
        _require(population["status"] == "frozen_unopened", "frozen calibration requires frozen_unopened population")
    digest = _sha256(root["identity_sha256"], "calibration.identity_sha256")
    _require(digest == _self_sha256(root), "final-calibration identity hash mismatch")
    metadata = {
        "state": state,
        "evidence_class": evidence_class,
        "task_ids": task_ids,
        "task_clusters": len(task_ids),
        "calibration_repetitions": product_meta["repetitions"],
        "calibration_cells": product_meta["expected_cells"],
        "planning": planning,
        "alternatives": alternatives,
        "product_cycle_contract_sha256": product_root["manifest_sha256"],
        "task_population_sha256": population["population_sha256"],
        "candidate_lock_sha256": candidate_lock["identity_sha256"],
        "source_byte_bindings_sha256": source_bindings["identity_sha256"],
        "product_cycle_evidence_canonical_bytes_sha256": source_bindings[
            "product_cycle_evidence_canonical_bytes_sha256"
        ],
        "task_population_contract_file_sha256": source_bindings[
            "task_population_contract_file_sha256"
        ],
        "locked_identities_sha256": locked["identity_sha256"],
        "product_cycle_schema_file_sha256": source_bindings[
            "product_cycle_schema_file_sha256"
        ],
        "task_population_schema_file_sha256": source_bindings[
            "task_population_schema_file_sha256"
        ],
    }
    return root, measurements, metadata


def preflight_calibration(calibration: Any) -> dict[str, Any]:
    """Fail closed on membership, identity, design, and pending-state drift."""
    root, measurements, metadata = _validated_calibration(calibration)
    return {
        "schema": "agent-brain-final-calibration-preflight/v1",
        "status": "valid",
        "state": metadata["state"],
        "evidence_class": metadata["evidence_class"],
        "calibration_sha256": root["identity_sha256"],
        "independent_active_task_clusters": metadata["task_clusters"],
        "calibration_repetitions_per_arm": metadata["calibration_repetitions"],
        "calibration_requested_cells": metadata["calibration_cells"],
        "calibration_validated_cells": len(measurements),
        "product_cycle_contract_sha256": metadata["product_cycle_contract_sha256"],
        "task_population_sha256": metadata["task_population_sha256"],
        "candidate_lock_sha256": metadata["candidate_lock_sha256"],
        "source_byte_bindings_sha256": metadata["source_byte_bindings_sha256"],
        "planning_ready": metadata["alternatives"] is not None,
        "budget_authorized": False,
    }


def _task_arm_means(
    measurements: Sequence[Mapping[str, Any]], task_ids: Sequence[str], field: str
) -> dict[str, dict[str, float]]:
    grouped: dict[tuple[str, str], list[Decimal]] = defaultdict(list)
    for cell in measurements:
        grouped[(cell["task_id"], cell["arm"])].append(cell[field])
    return {
        task_id: {
            arm: float(
                sum(grouped[(task_id, arm)], Decimal(0))
                / Decimal(len(grouped[(task_id, arm)]))
            )
            for arm in PRODUCT.ARMS
        }
        for task_id in task_ids
    }


def _contrast_vectors(
    measurements: Sequence[Mapping[str, Any]],
    task_ids: Sequence[str],
    *,
    numerator: str,
    denominator: str,
) -> list[dict[str, Any]]:
    elapsed = _task_arm_means(measurements, task_ids, "elapsed_time")
    costs = _task_arm_means(measurements, task_ids, "normalized_cost")
    quality = _task_arm_means(measurements, task_ids, "code_quality")
    vectors: list[dict[str, Any]] = []
    for task_id in task_ids:
        elapsed_num = elapsed[task_id][numerator]
        elapsed_den = elapsed[task_id][denominator]
        cost_num = costs[task_id][numerator]
        cost_den = costs[task_id][denominator]
        _require(elapsed_num > 0 and elapsed_den > 0, f"elapsed log ratio is undefined for {task_id}")
        _require(
            cost_num >= 0 and cost_den > 0,
            f"normalized-cost ratio requires a nonnegative numerator and positive denominator for {task_id}",
        )
        vector = {
            "task_id": task_id,
            "elapsed_log_ratio": math.log(elapsed_num / elapsed_den),
            "cost_numerator_mean": cost_num,
            "cost_denominator_mean": cost_den,
            "quality_difference": quality[task_id][numerator] - quality[task_id][denominator],
        }
        _require(
            all(math.isfinite(float(item)) for key, item in vector.items() if key != "task_id"),
            f"contrast vector contains non-finite values for {task_id}",
        )
        vectors.append(vector)
    return vectors


def shared_cluster_resamples(
    *,
    source_clusters: int,
    target_clusters: int,
    resamples: int,
    seed: int,
) -> tuple[list[tuple[int, ...]], str]:
    """Return one deterministic cluster-index stream shared by every endpoint."""
    _positive_int(source_clusters, "source_clusters")
    _positive_int(target_clusters, "target_clusters", maximum=MAX_CANDIDATE_CLUSTERS)
    _positive_int(resamples, "resamples", maximum=MAX_RESAMPLES)
    _require(type(seed) is int and 0 <= seed < 2**63, "seed is invalid")
    draws: list[tuple[int, ...]] = []
    digest = hashlib.sha256()
    digest.update(RESAMPLING_DOMAIN)
    digest.update(seed.to_bytes(8, "big"))
    digest.update(source_clusters.to_bytes(4, "big"))
    digest.update(target_clusters.to_bytes(4, "big"))
    digest.update(resamples.to_bytes(4, "big"))
    for draw_index in range(resamples):
        draw: list[int] = []
        for position in range(target_clusters):
            payload = (
                RESAMPLING_DOMAIN
                + seed.to_bytes(8, "big")
                + source_clusters.to_bytes(4, "big")
                + target_clusters.to_bytes(4, "big")
                + draw_index.to_bytes(4, "big")
                + position.to_bytes(4, "big")
            )
            index = int.from_bytes(hashlib.sha256(payload).digest()[:8], "big") % source_clusters
            draw.append(index)
            digest.update(index.to_bytes(4, "big"))
        draws.append(tuple(draw))
        digest.update(b"\n")
    return draws, digest.hexdigest()


def _mean(values: Sequence[float]) -> float:
    _require(bool(values), "cannot average an empty sequence")
    value = math.fsum(values) / len(values)
    _require(math.isfinite(value), "mean is non-finite")
    return value


def _quantile(sorted_values: Sequence[float], probability: float) -> float:
    _require(bool(sorted_values), "cannot take a quantile of an empty sequence")
    _require(0 <= probability <= 1, "quantile probability is invalid")
    position = (len(sorted_values) - 1) * probability
    lower = int(math.floor(position))
    upper = int(math.ceil(position))
    if lower == upper:
        return float(sorted_values[lower])
    weight = position - lower
    return float(sorted_values[lower] * (1 - weight) + sorted_values[upper] * weight)


def _vector_parameters(vectors: Sequence[Mapping[str, Any]]) -> dict[str, float]:
    return {
        "elapsed_log_mean": _mean([float(row["elapsed_log_ratio"]) for row in vectors]),
        "cost_numerator_mean": _mean([float(row["cost_numerator_mean"]) for row in vectors]),
        "cost_denominator_mean": _mean([float(row["cost_denominator_mean"]) for row in vectors]),
        "quality_difference_mean": _mean([float(row["quality_difference"]) for row in vectors]),
    }


def _draw_statistics(
    vectors: Sequence[Mapping[str, Any]],
    draws: Sequence[Sequence[int]],
    *,
    alternative: Mapping[str, Any] | None,
) -> tuple[list[dict[str, float]], dict[str, float]]:
    parameters = _vector_parameters(vectors)
    _require(
        parameters["cost_numerator_mean"] >= 0
        and parameters["cost_denominator_mean"] > 0,
        "observed normalized-cost ratio is undefined",
    )
    observed_cost_ratio = (
        parameters["cost_numerator_mean"] / parameters["cost_denominator_mean"]
    )
    statistics: list[dict[str, float]] = []
    for draw in draws:
        elapsed_residual = _mean(
            [
                float(vectors[index]["elapsed_log_ratio"])
                - parameters["elapsed_log_mean"]
                for index in draw
            ]
        )
        quality_residual = _mean(
            [
                float(vectors[index]["quality_difference"])
                - parameters["quality_difference_mean"]
                for index in draw
            ]
        )
        drawn_cost_num = _mean(
            [float(vectors[index]["cost_numerator_mean"]) for index in draw]
        )
        drawn_cost_den = _mean(
            [float(vectors[index]["cost_denominator_mean"]) for index in draw]
        )
        _require(
            drawn_cost_num >= 0 and drawn_cost_den > 0,
            "bootstrap normalized-cost ratio is undefined",
        )
        drawn_cost_ratio = drawn_cost_num / drawn_cost_den
        cost_deviation = drawn_cost_ratio - observed_cost_ratio
        if alternative is None:
            elapsed_value = parameters["elapsed_log_mean"] + elapsed_residual
            cost_value = observed_cost_ratio + cost_deviation
            quality_value = parameters["quality_difference_mean"] + quality_residual
        else:
            elapsed_value = math.log(
                alternative["elapsed_time"]["planning_alternative_ratio"]
            ) + elapsed_residual
            cost_alt = alternative["normalized_cost"]["planning_alternative_ratio"]
            cost_value = (
                cost_alt
                if observed_cost_ratio == 0
                else drawn_cost_ratio * cost_alt / observed_cost_ratio
            )
            quality_value = alternative["code_quality"][
                "planning_alternative_difference"
            ] + quality_residual
        row = {
            "elapsed_time": elapsed_value,
            "normalized_cost": cost_value,
            "code_quality": quality_value,
        }
        _require(all(math.isfinite(item) for item in row.values()), "resampled endpoint is non-finite")
        statistics.append(row)
    return statistics, parameters


def _ci_offsets(
    vectors: Sequence[Mapping[str, Any]], draws: Sequence[Sequence[int]]
) -> dict[str, float]:
    bootstrap, parameters = _draw_statistics(vectors, draws, alternative=None)
    observed = {
        "elapsed_time": parameters["elapsed_log_mean"],
        "normalized_cost": parameters["cost_numerator_mean"]
        / parameters["cost_denominator_mean"],
        "code_quality": parameters["quality_difference_mean"],
    }
    deviations = {
        endpoint: sorted(row[endpoint] - observed[endpoint] for row in bootstrap)
        for endpoint in ENDPOINTS
    }
    offsets = {
        "elapsed_time": _quantile(deviations["elapsed_time"], CONFIDENCE),
        "normalized_cost": _quantile(deviations["normalized_cost"], CONFIDENCE),
        "code_quality": _quantile(deviations["code_quality"], 1 - CONFIDENCE),
    }
    _require(all(math.isfinite(item) for item in offsets.values()), "CI offset is non-finite")
    return offsets


def _power_gate(
    marginal_power: Mapping[str, Any], joint_power: Any, *, target: float = TARGET_POWER
) -> bool:
    if set(marginal_power) != set(ENDPOINTS):
        return False
    values: list[float] = []
    for endpoint in ENDPOINTS:
        value = marginal_power[endpoint]
        if type(value) not in {int, float} or not math.isfinite(float(value)):
            return False
        values.append(float(value))
    if type(joint_power) not in {int, float} or not math.isfinite(float(joint_power)):
        return False
    return all(item >= target for item in values) and float(joint_power) >= target


def _power_for_contrast(
    vectors: Sequence[Mapping[str, Any]],
    alternatives: Mapping[str, Any],
    draws: Sequence[Sequence[int]],
    *,
    task_clusters: int,
    resampling_sha256: str,
) -> dict[str, Any]:
    offsets = _ci_offsets(vectors, draws)
    simulated, _ = _draw_statistics(vectors, draws, alternative=alternatives)
    thresholds = {
        "elapsed_time": math.log(alternatives["elapsed_time"]["required_ratio_max"]),
        "normalized_cost": alternatives["normalized_cost"]["required_ratio_max"],
        "code_quality": alternatives["code_quality"]["required_difference_min"],
    }
    pass_counts = {endpoint: 0 for endpoint in ENDPOINTS}
    joint_count = 0
    for row in simulated:
        passed = {
            "elapsed_time": row["elapsed_time"] + offsets["elapsed_time"]
            <= thresholds["elapsed_time"],
            "normalized_cost": row["normalized_cost"] + offsets["normalized_cost"]
            <= thresholds["normalized_cost"],
            "code_quality": row["code_quality"] + offsets["code_quality"]
            >= thresholds["code_quality"],
        }
        for endpoint in ENDPOINTS:
            pass_counts[endpoint] += int(passed[endpoint])
        joint_count += int(all(passed.values()))
    total = len(simulated)
    marginal = {
        endpoint: _stable_float(pass_counts[endpoint] / total) for endpoint in ENDPOINTS
    }
    joint = _stable_float(joint_count / total)
    gate = _power_gate(marginal, joint)
    return {
        "comparison": alternatives["comparison"],
        "role": alternatives["role"],
        "task_clusters": task_clusters,
        "resamples": total,
        "target_power": TARGET_POWER,
        "marginal_power": marginal,
        "joint_all_endpoint_power": joint,
        "all_marginal_at_least_target": all(
            marginal[endpoint] >= TARGET_POWER for endpoint in ENDPOINTS
        ),
        "joint_at_least_target": joint >= TARGET_POWER,
        "finite_estimates_and_ci_inputs": True,
        "shared_resampling_sha256": resampling_sha256,
        "independence_shortcut_used": False,
        "statistical_gate_met": gate,
    }


def _calibration_estimates(
    vectors: Sequence[Mapping[str, Any]], draws: Sequence[Sequence[int]], *, comparison: str
) -> dict[str, Any]:
    parameters = _vector_parameters(vectors)
    offsets = _ci_offsets(vectors, draws)
    elapsed_log = parameters["elapsed_log_mean"]
    cost_ratio = (
        parameters["cost_numerator_mean"] / parameters["cost_denominator_mean"]
    )
    quality = parameters["quality_difference_mean"]
    vectors_sha = PRODUCT.value_sha256(
        [
            {
                "task_id": row["task_id"],
                "elapsed_log_ratio": _stable_float(row["elapsed_log_ratio"]),
                "cost_numerator_mean": _stable_float(row["cost_numerator_mean"]),
                "cost_denominator_mean": _stable_float(row["cost_denominator_mean"]),
                "quality_difference": _stable_float(row["quality_difference"]),
            }
            for row in vectors
        ]
    )
    return {
        "comparison": comparison,
        "n_task_clusters": len(vectors),
        "repetition_aggregation": "arithmetic_mean_within_task_arm_before_contrast",
        "paired_task_vectors_sha256": vectors_sha,
        "elapsed_time": {
            "estimand": "paired_task_geometric_mean_ratio",
            "paired_task_geometric_mean_ratio": _stable_float(math.exp(elapsed_log)),
            "one_sided_upper_bound": _stable_float(
                math.exp(elapsed_log + offsets["elapsed_time"])
            ),
        },
        "normalized_cost": {
            "estimand": "ratio_of_equal_task_weighted_task_arm_mean_costs",
            "equal_task_weighted_mean_ratio": _stable_float(cost_ratio),
            "one_sided_upper_bound": _stable_float(
                cost_ratio + offsets["normalized_cost"]
            ),
            "geometric_mean_task_ratios_used": False,
            "structural_zero_treatment_cost_supported": True,
        },
        "code_quality": {
            "estimand": "paired_task_mean_difference",
            "paired_task_mean_difference": _stable_float(quality),
            "one_sided_lower_bound": _stable_float(
                quality + offsets["code_quality"]
            ),
        },
        "finite_estimates_and_ci_inputs": True,
    }


def _decision(
    *,
    rows: Sequence[Mapping[str, Any]],
    result_key: str,
    comparison: str,
    role: str,
    state: str,
    evidence_class: str,
    alters_benchmark_primary_verdict: bool | None = None,
) -> dict[str, Any]:
    selected = next(
        (
            row["task_clusters"]
            for row in rows
            if isinstance(row[result_key], dict)
            and row[result_key]["statistical_gate_met"] is True
        ),
        None,
    )
    statistical_gate = selected is not None
    eligible = state == "frozen" and evidence_class == "development_measurement"
    passed = bool(statistical_gate and eligible)
    result = {
        "comparison": comparison,
        "role": role,
        "selected_task_clusters": selected,
        "statistical_gate_met": statistical_gate,
        "every_marginal_and_joint_power_at_least_0_80": statistical_gate,
        "eligible_evidence": eligible,
        "passed": passed,
        "reason": (
            "passed"
            if passed
            else "synthetic_fixture_not_decision_eligible"
            if evidence_class == "synthetic_fixture"
            else "pending_calibration"
            if state == "pending"
            else "candidate_not_owner_approved"
            if state == "candidate"
            else "no_candidate_task_count_clears_every_power_gate"
        ),
    }
    if alters_benchmark_primary_verdict is not None:
        result["alters_benchmark_primary_verdict"] = (
            alters_benchmark_primary_verdict
        )
    return result


def analyze_calibration(calibration: Any) -> dict[str, Any]:
    """Compute deterministic marginal and direct joint power for both contrasts."""
    root, measurements, metadata = _validated_calibration(calibration)
    task_ids = metadata["task_ids"]
    primary_vectors = _contrast_vectors(
        measurements,
        task_ids,
        numerator="retrieved_memory",
        denominator="no_memory",
    )
    diagnostic_vectors = _contrast_vectors(
        measurements,
        task_ids,
        numerator="retrieved_memory",
        denominator="preoptimization_memory",
    )
    planning = metadata["planning"]
    calibration_draws, calibration_draws_sha = shared_cluster_resamples(
        source_clusters=len(task_ids),
        target_clusters=len(task_ids),
        resamples=planning["resamples"],
        seed=planning["seed"],
    )
    estimates = {
        "primary": _calibration_estimates(
            primary_vectors, calibration_draws, comparison=PRIMARY_COMPARISON
        ),
        "product_diagnostic": _calibration_estimates(
            diagnostic_vectors,
            calibration_draws,
            comparison=DIAGNOSTIC_COMPARISON,
        ),
    }

    power_rows: list[dict[str, Any]] = []
    for task_count in planning["candidate_task_clusters"]:
        requested_cells = task_count * planning["repetitions_per_arm"] * len(PRODUCT.ARMS)
        draws, draws_sha = shared_cluster_resamples(
            source_clusters=len(task_ids),
            target_clusters=task_count,
            resamples=planning["resamples"],
            seed=planning["seed"],
        )
        if metadata["alternatives"] is None:
            primary_power = None
            diagnostic_power = None
        else:
            primary_power = _power_for_contrast(
                primary_vectors,
                metadata["alternatives"]["primary"],
                draws,
                task_clusters=task_count,
                resampling_sha256=draws_sha,
            )
            diagnostic_power = _power_for_contrast(
                diagnostic_vectors,
                metadata["alternatives"]["product_diagnostic"],
                draws,
                task_clusters=task_count,
                resampling_sha256=draws_sha,
            )
        power_rows.append(
            {
                "task_clusters": task_count,
                "repetitions_per_arm": planning["repetitions_per_arm"],
                "arms": len(PRODUCT.ARMS),
                "requested_cells": requested_cells,
                "maximum_agent_invocations": requested_cells,
                "agent_retry_limit": 0,
                "replacement_cell_limit": 0,
                "reserve_cell_limit": 0,
                "shared_resampling_sha256": draws_sha,
                "primary": primary_power,
                "product_diagnostic": diagnostic_power,
            }
        )

    benchmark_decision = _decision(
        rows=power_rows,
        result_key="primary",
        comparison=PRIMARY_COMPARISON,
        role="benchmark_primary_power_gate",
        state=metadata["state"],
        evidence_class=metadata["evidence_class"],
    )
    product_decision = _decision(
        rows=power_rows,
        result_key="product_diagnostic",
        comparison=DIAGNOSTIC_COMPARISON,
        role="separate_product_improvement_gate",
        state=metadata["state"],
        evidence_class=metadata["evidence_class"],
        alters_benchmark_primary_verdict=False,
    )
    report: dict[str, Any] = {
        "schema": REPORT_SCHEMA,
        "state": metadata["state"],
        "evidence_class": metadata["evidence_class"],
        "status": "pending" if metadata["alternatives"] is None else "evaluated",
        "final_calibration_sha256": root["identity_sha256"],
        "owner_approval_sha256": root["owner_approval_sha256"],
        "planning_sha256": PRODUCT.value_sha256(planning),
        "planning_alternatives": metadata["alternatives"],
        "method": {
            "resampling": RESAMPLING_METHOD,
            "seed": planning["seed"],
            "resamples": planning["resamples"],
            "cluster_unit": "independent_task",
            "shared_draws_across_endpoints_and_contrasts": True,
            "cross_endpoint_dependence_preserved": True,
            "joint_probability_method": "same_draw_all_endpoint_pass_frequency",
            "parametric_independence_shortcut_used": False,
            "confidence": CONFIDENCE,
            "target_power": TARGET_POWER,
        },
        "design": {
            "arms": list(PRODUCT.ARMS),
            "calibration_task_clusters": len(task_ids),
            "calibration_repetitions_per_arm": metadata["calibration_repetitions"],
            "calibration_requested_cells": metadata["calibration_cells"],
            "calibration_maximum_agent_invocations": metadata["calibration_cells"],
            "candidate_task_clusters": planning["candidate_task_clusters"],
        },
        "calibration_estimates": estimates,
        "calibration_shared_resampling_sha256": calibration_draws_sha,
        "power_candidates": power_rows,
        "benchmark_power_decision": benchmark_decision,
        "product_improvement_gate": product_decision,
        "integrity": {
            "repetitions_averaged_inside_task_arm": True,
            "independent_task_clusters": len(task_ids),
            "all_calibration_members_development_calibration": True,
            "optimization_members_used": 0,
            "holdout_members_used": 0,
            "exact_four_arm_cells_and_invocations": True,
            "product_cycle_contract_sha256": metadata[
                "product_cycle_contract_sha256"
            ],
            "product_cycle_schema_file_sha256": metadata[
                "product_cycle_schema_file_sha256"
            ],
            "product_cycle_evidence_canonical_bytes_sha256": metadata[
                "product_cycle_evidence_canonical_bytes_sha256"
            ],
            "task_population_sha256": metadata["task_population_sha256"],
            "task_population_schema_file_sha256": metadata[
                "task_population_schema_file_sha256"
            ],
            "task_population_contract_file_sha256": metadata[
                "task_population_contract_file_sha256"
            ],
            "candidate_lock_sha256": metadata["candidate_lock_sha256"],
            "source_byte_bindings_sha256": metadata[
                "source_byte_bindings_sha256"
            ],
            "locked_identities_sha256": metadata["locked_identities_sha256"],
            "budget_authorized": False,
            "provider_or_model_calls_performed": False,
        },
    }
    report["report_sha256"] = _self_sha256(report, "report_sha256")
    validate_power_report(report)
    return report


def _validate_power_result(
    value: Any,
    label: str,
    *,
    expected_comparison: str,
    expected_role: str,
    expected_task_clusters: int,
    expected_resamples: int,
    expected_resampling_sha256: str,
) -> None:
    result = _exact_keys(
        value,
        {
            "comparison",
            "role",
            "task_clusters",
            "resamples",
            "target_power",
            "marginal_power",
            "joint_all_endpoint_power",
            "all_marginal_at_least_target",
            "joint_at_least_target",
            "finite_estimates_and_ci_inputs",
            "shared_resampling_sha256",
            "independence_shortcut_used",
            "statistical_gate_met",
        },
        label,
    )
    _require(result["comparison"] == expected_comparison, f"{label}.comparison changed")
    _require(result["role"] == expected_role, f"{label}.role changed")
    _require(
        result["task_clusters"] == expected_task_clusters,
        f"{label}.task_clusters changed",
    )
    _require(
        result["resamples"] == expected_resamples,
        f"{label}.resamples changed",
    )
    _require(_finite(result["target_power"], f"{label}.target_power") == TARGET_POWER, f"{label}.target_power changed")
    marginal = _exact_keys(result["marginal_power"], set(ENDPOINTS), f"{label}.marginal_power")
    for endpoint in ENDPOINTS:
        value = _finite(marginal[endpoint], f"{label}.marginal_power.{endpoint}")
        _require(0 <= value <= 1, f"{label}.marginal_power.{endpoint} is outside [0,1]")
    joint = _finite(result["joint_all_endpoint_power"], f"{label}.joint_all_endpoint_power")
    _require(0 <= joint <= 1, f"{label}.joint_all_endpoint_power is outside [0,1]")
    expected_marginal = all(marginal[endpoint] >= TARGET_POWER for endpoint in ENDPOINTS)
    expected_joint = joint >= TARGET_POWER
    _require(result["all_marginal_at_least_target"] is expected_marginal, f"{label} marginal gate drift")
    _require(result["joint_at_least_target"] is expected_joint, f"{label} joint gate drift")
    _require(result["finite_estimates_and_ci_inputs"] is True, f"{label} finite-input gate is false")
    _require(
        _sha256(
            result["shared_resampling_sha256"],
            f"{label}.shared_resampling_sha256",
        )
        == expected_resampling_sha256,
        f"{label} resampling identity drift",
    )
    _require(result["independence_shortcut_used"] is False, f"{label} independence shortcut is prohibited")
    _require(
        result["statistical_gate_met"] is _power_gate(marginal, joint),
        f"{label} statistical gate drift",
    )


def _validate_calibration_estimate(
    value: Any, label: str, *, expected_comparison: str, expected_clusters: int
) -> None:
    estimate = _exact_keys(
        value,
        {
            "comparison",
            "n_task_clusters",
            "repetition_aggregation",
            "paired_task_vectors_sha256",
            "elapsed_time",
            "normalized_cost",
            "code_quality",
            "finite_estimates_and_ci_inputs",
        },
        label,
    )
    _require(
        estimate["comparison"] == expected_comparison,
        f"{label}.comparison changed",
    )
    estimate_clusters = _positive_int(
        estimate["n_task_clusters"], f"{label}.n_task_clusters"
    )
    _require(estimate_clusters == expected_clusters, f"{label}.n_task_clusters changed")
    _require(
        estimate["repetition_aggregation"]
        == "arithmetic_mean_within_task_arm_before_contrast",
        f"{label}.repetition_aggregation changed",
    )
    _sha256(estimate["paired_task_vectors_sha256"], f"{label}.paired_task_vectors_sha256")

    elapsed = _exact_keys(
        estimate["elapsed_time"],
        {
            "estimand",
            "paired_task_geometric_mean_ratio",
            "one_sided_upper_bound",
        },
        f"{label}.elapsed_time",
    )
    _require(
        elapsed["estimand"] == "paired_task_geometric_mean_ratio",
        f"{label}.elapsed_time estimand changed",
    )
    for field in ("paired_task_geometric_mean_ratio", "one_sided_upper_bound"):
        _require(
            _finite(elapsed[field], f"{label}.elapsed_time.{field}") > 0,
            f"{label}.elapsed_time.{field} must be positive",
        )

    cost = _exact_keys(
        estimate["normalized_cost"],
        {
            "estimand",
            "equal_task_weighted_mean_ratio",
            "one_sided_upper_bound",
            "geometric_mean_task_ratios_used",
            "structural_zero_treatment_cost_supported",
        },
        f"{label}.normalized_cost",
    )
    _require(
        cost["estimand"] == "ratio_of_equal_task_weighted_task_arm_mean_costs",
        f"{label}.normalized_cost estimand changed",
    )
    for field in ("equal_task_weighted_mean_ratio", "one_sided_upper_bound"):
        _require(
            _finite(cost[field], f"{label}.normalized_cost.{field}") >= 0,
            f"{label}.normalized_cost.{field} must be nonnegative",
        )
    _require(
        cost["geometric_mean_task_ratios_used"] is False,
        f"{label}.normalized_cost may not use geometric task-ratio averaging",
    )
    _require(
        cost["structural_zero_treatment_cost_supported"] is True,
        f"{label}.normalized_cost must support authenticated treatment structural zeros",
    )

    quality = _exact_keys(
        estimate["code_quality"],
        {"estimand", "paired_task_mean_difference", "one_sided_lower_bound"},
        f"{label}.code_quality",
    )
    _require(
        quality["estimand"] == "paired_task_mean_difference",
        f"{label}.code_quality estimand changed",
    )
    _finite(
        quality["paired_task_mean_difference"],
        f"{label}.code_quality.paired_task_mean_difference",
    )
    _finite(
        quality["one_sided_lower_bound"],
        f"{label}.code_quality.one_sided_lower_bound",
    )
    _require(
        estimate["finite_estimates_and_ci_inputs"] is True,
        f"{label} finite-input gate is false",
    )


def validate_power_report(report: Any) -> None:
    """Recheck report identities, arithmetic, null states, and both power gates."""
    root = _exact_keys(
        report,
        {
            "schema",
            "state",
            "evidence_class",
            "status",
            "final_calibration_sha256",
            "owner_approval_sha256",
            "planning_sha256",
            "planning_alternatives",
            "method",
            "design",
            "calibration_estimates",
            "calibration_shared_resampling_sha256",
            "power_candidates",
            "benchmark_power_decision",
            "product_improvement_gate",
            "integrity",
            "report_sha256",
        },
        "power_report",
    )
    _require(root["schema"] == REPORT_SCHEMA, "power report schema changed")
    state = root["state"]
    _require(state in {"pending", "candidate", "frozen"}, "power report state is invalid")
    evidence = root["evidence_class"]
    _require(evidence in {"synthetic_fixture", "development_measurement"}, "power report evidence_class is invalid")
    _require(
        state != "frozen" or evidence == "development_measurement",
        "a frozen power report requires development measurement evidence",
    )
    _sha256(root["final_calibration_sha256"], "power_report.final_calibration_sha256")
    _sha256(root["planning_sha256"], "power_report.planning_sha256")
    if state == "frozen":
        _sha256(root["owner_approval_sha256"], "power_report.owner_approval_sha256")
    else:
        _require(
            root["owner_approval_sha256"] is None,
            "only a frozen power report may carry owner approval",
        )
    _sha256(
        root["calibration_shared_resampling_sha256"],
        "power_report.calibration_shared_resampling_sha256",
    )
    method = _exact_keys(
        root["method"],
        {
            "resampling",
            "seed",
            "resamples",
            "cluster_unit",
            "shared_draws_across_endpoints_and_contrasts",
            "cross_endpoint_dependence_preserved",
            "joint_probability_method",
            "parametric_independence_shortcut_used",
            "confidence",
            "target_power",
        },
        "power_report.method",
    )
    _require(
        method["resampling"] == RESAMPLING_METHOD,
        "power_report resampling method changed",
    )
    _require(
        type(method["seed"]) is int and 0 <= method["seed"] < 2**63,
        "power_report seed is invalid",
    )
    resamples = _positive_int(
        method["resamples"], "power_report.method.resamples", maximum=MAX_RESAMPLES
    )
    _require(resamples >= MIN_RESAMPLES, "power_report resample floor changed")
    _require(
        method["cluster_unit"] == "independent_task",
        "power_report cluster unit changed",
    )
    _require(
        method["shared_draws_across_endpoints_and_contrasts"] is True,
        "power_report must share draws across endpoints and contrasts",
    )
    _require(
        method["cross_endpoint_dependence_preserved"] is True,
        "power_report must preserve cross-endpoint dependence",
    )
    _require(
        method["joint_probability_method"]
        == "same_draw_all_endpoint_pass_frequency",
        "power_report joint probability method changed",
    )
    _require(
        method["parametric_independence_shortcut_used"] is False,
        "power_report independence shortcut is prohibited",
    )
    _require(
        _finite(method["confidence"], "power_report.method.confidence")
        == CONFIDENCE,
        "power_report confidence changed",
    )
    _require(
        _finite(method["target_power"], "power_report.method.target_power")
        == TARGET_POWER,
        "power_report target power changed",
    )

    design = _exact_keys(
        root["design"],
        {
            "arms",
            "calibration_task_clusters",
            "calibration_repetitions_per_arm",
            "calibration_requested_cells",
            "calibration_maximum_agent_invocations",
            "candidate_task_clusters",
        },
        "power_report.design",
    )
    _require(design["arms"] == list(PRODUCT.ARMS), "power_report arms changed")
    calibration_clusters = _positive_int(
        design["calibration_task_clusters"],
        "power_report.design.calibration_task_clusters",
    )
    _require(
        calibration_clusters >= MIN_CALIBRATION_CLUSTERS,
        "power_report has too few calibration task clusters",
    )
    repetitions = _positive_int(
        design["calibration_repetitions_per_arm"],
        "power_report.design.calibration_repetitions_per_arm",
    )
    calibration_cells = calibration_clusters * repetitions * len(PRODUCT.ARMS)
    _require(
        _positive_int(
            design["calibration_requested_cells"],
            "power_report.design.calibration_requested_cells",
        )
        == calibration_cells,
        "power_report calibration requested-cell arithmetic drift",
    )
    _require(
        _positive_int(
            design["calibration_maximum_agent_invocations"],
            "power_report.design.calibration_maximum_agent_invocations",
        )
        == calibration_cells,
        "power_report calibration invocation arithmetic drift",
    )
    candidate_counts = design["candidate_task_clusters"]
    _require(
        isinstance(candidate_counts, list)
        and bool(candidate_counts)
        and all(
            type(item) is int
            and MIN_CALIBRATION_CLUSTERS <= item <= MAX_CANDIDATE_CLUSTERS
            for item in candidate_counts
        ),
        "power_report candidate task counts are invalid",
    )
    _require(
        candidate_counts == sorted(set(candidate_counts)),
        "power_report candidate task counts must be unique and increasing",
    )
    if state == "pending":
        _require(
            root["planning_alternatives"] is None,
            "pending power report must retain null planning alternatives",
        )
        alternatives = None
    else:
        raw_alternatives = _exact_keys(
            root["planning_alternatives"],
            {"primary", "product_diagnostic"},
            "power_report.planning_alternatives",
        )
        alternatives = {
            "primary": _validate_contrast_alternatives(
                raw_alternatives["primary"],
                label="power_report.planning_alternatives.primary",
                comparison=PRIMARY_COMPARISON,
                role="primary_development_contrast",
            ),
            "product_diagnostic": _validate_contrast_alternatives(
                raw_alternatives["product_diagnostic"],
                label="power_report.planning_alternatives.product_diagnostic",
                comparison=DIAGNOSTIC_COMPARISON,
                role="diagnostic_product_progress_only",
            ),
        }
    planning_projection = {
        "method": method["resampling"],
        "target_power": method["target_power"],
        "confidence": method["confidence"],
        "resamples": method["resamples"],
        "seed": method["seed"],
        "candidate_task_clusters": candidate_counts,
        "arms": design["arms"],
        "repetitions_per_arm": repetitions,
        "agent_retry_limit": 0,
        "replacement_cell_limit": 0,
        "reserve_cell_limit": 0,
        "planning_alternatives": alternatives,
    }
    _require(
        root["planning_sha256"] == PRODUCT.value_sha256(planning_projection),
        "power_report planning identity drift",
    )
    _, expected_calibration_resampling_sha256 = shared_cluster_resamples(
        source_clusters=calibration_clusters,
        target_clusters=calibration_clusters,
        resamples=resamples,
        seed=method["seed"],
    )
    _require(
        root["calibration_shared_resampling_sha256"]
        == expected_calibration_resampling_sha256,
        "power_report calibration resampling identity drift",
    )

    estimates = _exact_keys(
        root["calibration_estimates"],
        {"primary", "product_diagnostic"},
        "power_report.calibration_estimates",
    )
    _validate_calibration_estimate(
        estimates["primary"],
        "power_report.calibration_estimates.primary",
        expected_comparison=PRIMARY_COMPARISON,
        expected_clusters=calibration_clusters,
    )
    _validate_calibration_estimate(
        estimates["product_diagnostic"],
        "power_report.calibration_estimates.product_diagnostic",
        expected_comparison=DIAGNOSTIC_COMPARISON,
        expected_clusters=calibration_clusters,
    )
    candidates = root["power_candidates"]
    _require(isinstance(candidates, list) and bool(candidates), "power report candidates are missing")
    _require(
        [row.get("task_clusters") if isinstance(row, dict) else None for row in candidates]
        == candidate_counts,
        "power report candidate rows do not match the design",
    )
    pending = state == "pending"
    _require(root["status"] == ("pending" if pending else "evaluated"), "power report status/state mismatch")
    for index, row in enumerate(candidates):
        label = f"power_report.power_candidates[{index}]"
        row = _exact_keys(
            row,
            {
                "task_clusters",
                "repetitions_per_arm",
                "arms",
                "requested_cells",
                "maximum_agent_invocations",
                "agent_retry_limit",
                "replacement_cell_limit",
                "reserve_cell_limit",
                "shared_resampling_sha256",
                "primary",
                "product_diagnostic",
            },
            label,
        )
        task_count = _positive_int(row["task_clusters"], f"{label}.task_clusters")
        _require(
            _positive_int(row["repetitions_per_arm"], f"{label}.repetitions_per_arm")
            == repetitions,
            f"{label} repetition arithmetic drift",
        )
        _require(
            _positive_int(row["arms"], f"{label}.arms") == len(PRODUCT.ARMS),
            f"{label} arm arithmetic drift",
        )
        expected_cells = task_count * repetitions * len(PRODUCT.ARMS)
        _require(
            _positive_int(row["requested_cells"], f"{label}.requested_cells")
            == expected_cells,
            f"{label} requested-cell arithmetic drift",
        )
        _require(
            _positive_int(
                row["maximum_agent_invocations"],
                f"{label}.maximum_agent_invocations",
            )
            == expected_cells,
            f"{label} invocation arithmetic drift",
        )
        for field in ("agent_retry_limit", "replacement_cell_limit", "reserve_cell_limit"):
            _require(
                type(row[field]) is int and row[field] == 0,
                f"{label}.{field} must remain integer zero",
            )
        shared_hash = _sha256(row["shared_resampling_sha256"], f"{label}.shared_resampling_sha256")
        _, expected_shared_hash = shared_cluster_resamples(
            source_clusters=calibration_clusters,
            target_clusters=task_count,
            resamples=resamples,
            seed=method["seed"],
        )
        _require(shared_hash == expected_shared_hash, f"{label} resampling identity drift")
        if pending:
            _require(row["primary"] is None and row["product_diagnostic"] is None, "pending power rows must retain null results")
        else:
            _validate_power_result(
                row["primary"],
                f"{label}.primary",
                expected_comparison=PRIMARY_COMPARISON,
                expected_role="primary_development_contrast",
                expected_task_clusters=task_count,
                expected_resamples=resamples,
                expected_resampling_sha256=shared_hash,
            )
            _validate_power_result(
                row["product_diagnostic"],
                f"{label}.product_diagnostic",
                expected_comparison=DIAGNOSTIC_COMPARISON,
                expected_role="diagnostic_product_progress_only",
                expected_task_clusters=task_count,
                expected_resamples=resamples,
                expected_resampling_sha256=shared_hash,
            )

    for key, result_key, comparison, role, diagnostic in (
        (
            "benchmark_power_decision",
            "primary",
            PRIMARY_COMPARISON,
            "benchmark_primary_power_gate",
            False,
        ),
        (
            "product_improvement_gate",
            "product_diagnostic",
            DIAGNOSTIC_COMPARISON,
            "separate_product_improvement_gate",
            True,
        ),
    ):
        decision_fields = {
            "comparison",
            "role",
            "selected_task_clusters",
            "statistical_gate_met",
            "every_marginal_and_joint_power_at_least_0_80",
            "eligible_evidence",
            "passed",
            "reason",
        }
        if diagnostic:
            decision_fields.add("alters_benchmark_primary_verdict")
        decision = _exact_keys(root[key], decision_fields, f"power_report.{key}")
        selected = next(
            (
                row["task_clusters"]
                for row in candidates
                if isinstance(row.get(result_key), dict)
                and row[result_key].get("statistical_gate_met") is True
            ),
            None,
        )
        _require(decision.get("comparison") == comparison, f"power_report.{key} comparison drift")
        _require(decision.get("role") == role, f"power_report.{key} role drift")
        decision_selected = decision["selected_task_clusters"]
        _require(
            (decision_selected is None and selected is None)
            or (
                type(decision_selected) is int
                and decision_selected == selected
            ),
            f"power_report.{key} selected count drift",
        )
        statistical = selected is not None
        _require(decision.get("statistical_gate_met") is statistical, f"power_report.{key} statistical gate drift")
        _require(
            decision.get("every_marginal_and_joint_power_at_least_0_80") is statistical,
            f"power_report.{key} all-power gate drift",
        )
        eligible = state == "frozen" and evidence == "development_measurement"
        _require(decision.get("eligible_evidence") is eligible, f"power_report.{key} eligibility drift")
        passed = bool(statistical and eligible)
        _require(decision.get("passed") is passed, f"power_report.{key} pass drift")
        reason = (
            "passed"
            if passed
            else "synthetic_fixture_not_decision_eligible"
            if evidence == "synthetic_fixture"
            else "pending_calibration"
            if state == "pending"
            else "candidate_not_owner_approved"
            if state == "candidate"
            else "no_candidate_task_count_clears_every_power_gate"
        )
        _require(decision.get("reason") == reason, f"power_report.{key} reason drift")
        if diagnostic:
            _require(
                decision["alters_benchmark_primary_verdict"] is False,
                "product diagnostic may not alter the benchmark primary verdict",
            )

    integrity = _exact_keys(
        root["integrity"],
        {
            "repetitions_averaged_inside_task_arm",
            "independent_task_clusters",
            "all_calibration_members_development_calibration",
            "optimization_members_used",
            "holdout_members_used",
            "exact_four_arm_cells_and_invocations",
            "product_cycle_contract_sha256",
            "product_cycle_schema_file_sha256",
            "product_cycle_evidence_canonical_bytes_sha256",
            "task_population_sha256",
            "task_population_schema_file_sha256",
            "task_population_contract_file_sha256",
            "candidate_lock_sha256",
            "source_byte_bindings_sha256",
            "locked_identities_sha256",
            "budget_authorized",
            "provider_or_model_calls_performed",
        },
        "power_report.integrity",
    )
    for field in (
        "repetitions_averaged_inside_task_arm",
        "all_calibration_members_development_calibration",
        "exact_four_arm_cells_and_invocations",
    ):
        _require(integrity[field] is True, f"power_report.integrity.{field} must be true")
    _require(
        _positive_int(
            integrity["independent_task_clusters"],
            "power_report.integrity.independent_task_clusters",
        )
        == calibration_clusters,
        "power_report integrity task-cluster count drift",
    )
    _require(
        type(integrity["optimization_members_used"]) is int
        and integrity["optimization_members_used"] == 0,
        "optimization evidence is prohibited",
    )
    _require(
        type(integrity["holdout_members_used"]) is int
        and integrity["holdout_members_used"] == 0,
        "holdout evidence is prohibited",
    )
    for field in (
        "product_cycle_contract_sha256",
        "product_cycle_evidence_canonical_bytes_sha256",
        "task_population_sha256",
        "task_population_contract_file_sha256",
        "candidate_lock_sha256",
        "source_byte_bindings_sha256",
        "locked_identities_sha256",
    ):
        _sha256(integrity[field], f"power_report.integrity.{field}")
    _require(
        _sha256(
            integrity["product_cycle_schema_file_sha256"],
            "power_report.integrity.product_cycle_schema_file_sha256",
        )
        == PRODUCT_CYCLE_SCHEMA_SHA256,
        "power_report product-cycle schema bytes drift",
    )
    _require(
        _sha256(
            integrity["task_population_schema_file_sha256"],
            "power_report.integrity.task_population_schema_file_sha256",
        )
        == TASK_POPULATION_SCHEMA_SHA256,
        "power_report task-population schema bytes drift",
    )
    _require(integrity["budget_authorized"] is False, "power v4 cannot authorize budget")
    _require(
        integrity["provider_or_model_calls_performed"] is False,
        "power v4 cannot perform provider or model calls",
    )
    digest = _sha256(root["report_sha256"], "power_report.report_sha256")
    _require(digest == _self_sha256(root, "report_sha256"), "power report identity hash mismatch")


def check_power_report(calibration: Any, report: Any) -> dict[str, Any]:
    """Recompute a report from its bound calibration and require exact equality."""
    validate_power_report(report)
    expected = analyze_calibration(calibration)
    _require(
        report == expected,
        "power report does not exactly match deterministic recomputation from calibration",
    )
    return {
        "schema": "agent-brain-power-analysis-check/v1",
        "status": "valid",
        "final_calibration_sha256": expected["final_calibration_sha256"],
        "report_sha256": expected["report_sha256"],
        "budget_authorized": False,
        "provider_or_model_calls_performed": False,
    }


def _write(value: Any) -> None:
    sys.stdout.write(json.dumps(value, indent=2, sort_keys=True, allow_nan=False) + "\n")


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="command", required=True)
    for command in ("preflight", "analyze"):
        child = subparsers.add_parser(command)
        child.add_argument("calibration", type=pathlib.Path)
    checker = subparsers.add_parser("check")
    checker.add_argument("calibration", type=pathlib.Path)
    checker.add_argument("report", type=pathlib.Path)
    args = parser.parse_args(argv)
    try:
        calibration = load_calibration(args.calibration)
        if args.command == "preflight":
            result = preflight_calibration(calibration)
        elif args.command == "analyze":
            result = analyze_calibration(calibration)
        else:
            result = check_power_report(calibration, load_power_report(args.report))
    except PowerV4Error as exc:
        sys.stderr.write(f"power-v4 {args.command} failed: {exc}\n")
        return 2
    _write(result)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
