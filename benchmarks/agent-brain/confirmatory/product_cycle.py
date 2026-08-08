#!/usr/bin/env python3
"""Fail-closed development product-cycle analysis for agent-brain.

This module is deliberately separate from the locked three-arm confirmatory
analyzer.  It validates and analyzes a same-schedule four-arm development
comparison between a pre-optimization product identity and the current
optimized identity.  It does not authorize spend, freeze a design, or produce
a confirmatory verdict.
"""

from __future__ import annotations

import argparse
from collections import defaultdict
from decimal import Decimal, InvalidOperation, localcontext
import hashlib
import json
import math
import pathlib
import re
import sys
from typing import Any, Mapping, Sequence


MANIFEST_SCHEMA = "agent-brain-development-product-cycle/v1"
SCHEDULE_SCHEMA = "agent-brain-development-product-cycle-schedule/v1"
PREFLIGHT_SCHEMA = "agent-brain-development-product-cycle-preflight/v1"
REPORT_SCHEMA = "agent-brain-development-product-cycle-analysis/v1"
NORMALIZED_COST_SCHEMA = "agent-brain-normalized-cost/v1"
QUALITY_SCHEMA = "agent-brain-code-quality/v2"
QUALITY_RUBRIC = "task_relative_output_outcome_patch_focus_v2"
PROMPT_PARITY_ALGORITHM = "frozen_memory_packet_payload_placeholder_v1"

ARMS = (
    "no_memory",
    "placebo_packet",
    "preoptimization_memory",
    "retrieved_memory",
)
ARM_PRODUCT_ROLES = {
    "no_memory": "candidate",
    "placebo_packet": "candidate",
    "preoptimization_memory": "baseline",
    "retrieved_memory": "candidate",
}
PRIMARY_CONTRAST = {
    "id": "retrieved_memory_vs_no_memory",
    "numerator": "retrieved_memory",
    "denominator": "no_memory",
    "role": "primary_development_contrast",
}
DIAGNOSTIC_CONTRAST = {
    "id": "retrieved_memory_vs_preoptimization_memory",
    "numerator": "retrieved_memory",
    "denominator": "preoptimization_memory",
    "role": "diagnostic_product_progress_only",
}
PRICE_CATEGORIES = (
    "uncached_input",
    "cache_read_input",
    "cache_write_input",
    "visible_output",
    "reasoning_output",
)
QUALITY_EXCLUSIONS = (
    "validation_discipline",
    "runtime_efficiency",
    "brain_use",
)
SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
OID_RE = re.compile(r"^[0-9a-f]{40}$")
DECIMAL_RE = re.compile(r"^(0|[1-9][0-9]*)(\.[0-9]+)?$")
MAX_MANIFEST_BYTES = 32 * 1024 * 1024


class ProductCycleError(ValueError):
    """Raised when product-cycle evidence cannot safely be analyzed."""


def _require(condition: bool, message: str) -> None:
    if not condition:
        raise ProductCycleError(message)


def _object_without_duplicate_keys(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    value: dict[str, Any] = {}
    for key, item in pairs:
        _require(key not in value, f"JSON contains duplicate key {key!r}")
        value[key] = item
    return value


def canonical_json_bytes(value: Any) -> bytes:
    """Return stable UTF-8 JSON bytes used by every product-cycle binding."""
    try:
        return json.dumps(
            value,
            ensure_ascii=False,
            allow_nan=False,
            sort_keys=True,
            separators=(",", ":"),
        ).encode("utf-8")
    except (TypeError, ValueError) as exc:
        raise ProductCycleError(f"value is not canonical JSON: {exc}") from exc


def value_sha256(value: Any) -> str:
    return hashlib.sha256(canonical_json_bytes(value)).hexdigest()


def self_sha256(value: Mapping[str, Any], field: str = "identity_sha256") -> str:
    return value_sha256({key: item for key, item in value.items() if key != field})


def _exact_keys(value: Any, expected: set[str], label: str) -> dict[str, Any]:
    _require(isinstance(value, dict), f"{label} must be an object")
    actual = set(value)
    missing = sorted(expected - actual)
    extra = sorted(actual - expected)
    _require(not missing and not extra, f"{label} fields mismatch: missing={missing}, extra={extra}")
    return value


def _nonempty_string(value: Any, label: str) -> str:
    _require(isinstance(value, str) and bool(value.strip()), f"{label} must be a non-empty string")
    return value


def _sha256(value: Any, label: str) -> str:
    _require(
        isinstance(value, str) and SHA256_RE.fullmatch(value) is not None,
        f"{label} must be a lowercase SHA-256",
    )
    _require(value != "0" * 64, f"{label} cannot be an all-zero placeholder")
    return value


def _oid(value: Any, label: str) -> str:
    _require(
        isinstance(value, str) and OID_RE.fullmatch(value) is not None,
        f"{label} must be a lowercase 40-character Git object ID",
    )
    _require(value != "0" * 40, f"{label} cannot be an all-zero placeholder")
    return value


def _positive_int(value: Any, label: str) -> int:
    _require(type(value) is int and value > 0, f"{label} must be a positive integer")
    return value


def _decimal(value: Any, label: str) -> Decimal:
    _require(
        isinstance(value, str) and DECIMAL_RE.fullmatch(value) is not None,
        f"{label} must be a nonnegative non-exponent decimal string",
    )
    try:
        parsed = Decimal(value)
    except InvalidOperation as exc:  # pragma: no cover - guarded by the regex
        raise ProductCycleError(f"{label} is not a decimal") from exc
    _require(parsed.is_finite() and parsed >= 0, f"{label} must be finite and nonnegative")
    return parsed


def _finite_decimal(value: Any, label: str, *, positive: bool = False) -> Decimal:
    _require(type(value) in {int, float}, f"{label} must be a finite number")
    number = float(value)
    _require(math.isfinite(number), f"{label} must be finite")
    if positive:
        _require(number > 0, f"{label} must be positive")
    return Decimal(str(value))


def _string_list(value: Any, label: str, *, nonempty: bool = False) -> list[str]:
    _require(isinstance(value, list), f"{label} must be a list")
    _require(all(isinstance(item, str) and item for item in value), f"{label} must contain non-empty strings")
    _require(len(value) == len(set(value)), f"{label} must not contain duplicates")
    if nonempty:
        _require(bool(value), f"{label} must not be empty")
    return value


def load_manifest(path: pathlib.Path) -> dict[str, Any]:
    """Strictly load one bounded JSON manifest without accepting duplicate keys."""
    try:
        size = path.stat().st_size
        _require(path.is_file() and not path.is_symlink(), "manifest must be one regular non-symlink file")
        _require(size <= MAX_MANIFEST_BYTES, "manifest exceeds the size limit")
        raw = path.read_text(encoding="utf-8")
        value = json.loads(raw, object_pairs_hook=_object_without_duplicate_keys)
    except ProductCycleError:
        raise
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        raise ProductCycleError(f"manifest cannot be loaded: {exc}") from exc
    _require(isinstance(value, dict), "manifest root must be an object")
    return value


def _validate_product_identity(value: Any, label: str, expected_role: str) -> str:
    identity = _exact_keys(
        value,
        {
            "role",
            "commit_oid",
            "tree_oid",
            "binary_sha256",
            "config_sha256",
            "packet_format",
            "identity_sha256",
        },
        label,
    )
    _require(identity["role"] == expected_role, f"{label}.role changed")
    _oid(identity["commit_oid"], f"{label}.commit_oid")
    _oid(identity["tree_oid"], f"{label}.tree_oid")
    _sha256(identity["binary_sha256"], f"{label}.binary_sha256")
    _sha256(identity["config_sha256"], f"{label}.config_sha256")
    packet = _exact_keys(identity["packet_format"], {"id", "sha256"}, f"{label}.packet_format")
    _nonempty_string(packet["id"], f"{label}.packet_format.id")
    _sha256(packet["sha256"], f"{label}.packet_format.sha256")
    digest = _sha256(identity["identity_sha256"], f"{label}.identity_sha256")
    _require(digest == self_sha256(identity), f"{label} identity hash mismatch")
    return digest


def _validate_product_identities(value: Any) -> dict[str, str]:
    identities = _exact_keys(value, {"baseline", "candidate", "binding_sha256"}, "product_identities")
    baseline = _validate_product_identity(
        identities["baseline"], "product_identities.baseline", "preoptimization_memory"
    )
    candidate = _validate_product_identity(
        identities["candidate"], "product_identities.candidate", "retrieved_memory_optimized_current"
    )
    _require(baseline != candidate, "baseline and candidate product identities must differ")
    binding = _sha256(identities["binding_sha256"], "product_identities.binding_sha256")
    expected = value_sha256({"baseline": baseline, "candidate": candidate})
    _require(binding == expected, "baseline/candidate product binding hash mismatch")
    return {"baseline": baseline, "candidate": candidate, "binding": binding}


def _validate_tasks(value: Any, recorded_hash: Any) -> tuple[list[str], dict[str, dict[str, Any]], str]:
    _require(isinstance(value, list) and bool(value), "tasks must be a non-empty list")
    tasks: dict[str, dict[str, Any]] = {}
    task_ids: list[str] = []
    for index, raw in enumerate(value):
        task = _exact_keys(
            raw,
            {"task_id", "task_sha256", "prompt_parity_sha256"},
            f"tasks[{index}]",
        )
        task_id = _nonempty_string(task["task_id"], f"tasks[{index}].task_id")
        _require(task_id not in tasks, f"duplicate task_id {task_id!r}")
        _sha256(task["task_sha256"], f"tasks[{index}].task_sha256")
        _sha256(
            task["prompt_parity_sha256"],
            f"tasks[{index}].prompt_parity_sha256",
        )
        tasks[task_id] = task
        task_ids.append(task_id)
    _require(task_ids == sorted(task_ids), "tasks must be ordered by task_id")
    digest = _sha256(recorded_hash, "task_inventory_sha256")
    _require(digest == value_sha256(value), "task inventory hash mismatch")
    return task_ids, tasks, digest


def _validate_schedule(
    value: Any,
    *,
    task_ids: Sequence[str],
    repetitions: int,
) -> tuple[dict[int, tuple[str, int, str]], str]:
    schedule = _exact_keys(
        value,
        {"schema", "order_policy", "cells", "identity_sha256"},
        "design.schedule",
    )
    _require(schedule["schema"] == SCHEDULE_SCHEMA, "schedule schema changed")
    _require(
        schedule["order_policy"] == "explicit_same_schedule_four_arm_v1",
        "schedule order policy changed",
    )
    raw_cells = schedule["cells"]
    expected_count = len(task_ids) * repetitions * len(ARMS)
    _require(isinstance(raw_cells, list), "design.schedule.cells must be a list")
    _require(len(raw_cells) == expected_count, f"schedule must contain {expected_count} cells")
    by_position: dict[int, tuple[str, int, str]] = {}
    observed: set[tuple[str, int, str]] = set()
    expected_tasks = set(task_ids)
    for index, raw in enumerate(raw_cells):
        cell = _exact_keys(raw, {"position", "task_id", "repetition", "arm"}, f"design.schedule.cells[{index}]")
        position = _positive_int(cell["position"], f"design.schedule.cells[{index}].position")
        _require(position == index + 1, "schedule positions must be consecutive and in declared order")
        task_id = _nonempty_string(cell["task_id"], f"design.schedule.cells[{index}].task_id")
        _require(task_id in expected_tasks, f"schedule contains unexpected task {task_id!r}")
        repetition = _positive_int(cell["repetition"], f"design.schedule.cells[{index}].repetition")
        _require(repetition <= repetitions, "schedule repetition exceeds the design")
        arm = cell["arm"]
        _require(arm in ARMS, f"schedule contains invalid arm {arm!r}")
        key = (task_id, repetition, arm)
        _require(key not in observed, f"duplicate schedule cell {key!r}")
        observed.add(key)
        by_position[position] = key
    expected = {
        (task_id, repetition, arm)
        for task_id in task_ids
        for repetition in range(1, repetitions + 1)
        for arm in ARMS
    }
    _require(observed == expected, "schedule does not contain the exact four-arm task/repetition grid")
    digest = _sha256(schedule["identity_sha256"], "design.schedule.identity_sha256")
    _require(digest == self_sha256(schedule), "schedule identity hash mismatch")
    return by_position, digest


def _validate_design(value: Any, task_ids: Sequence[str]) -> tuple[dict[int, tuple[str, int, str]], str, int, int]:
    design = _exact_keys(
        value,
        {
            "arms",
            "arm_product_roles",
            "primary_contrast",
            "diagnostic_contrast",
            "task_clusters",
            "repetitions_per_arm",
            "requested_cells",
            "agent_retry_limit",
            "replacement_cell_limit",
            "reserve_cell_limit",
            "maximum_agent_invocations",
            "budget",
            "schedule",
        },
        "design",
    )
    _require(design["arms"] == list(ARMS), "four-arm vocabulary or order changed")
    _require(design["arm_product_roles"] == ARM_PRODUCT_ROLES, "arm/product identity mapping changed")
    _require(design["primary_contrast"] == PRIMARY_CONTRAST, "primary contrast changed")
    _require(design["diagnostic_contrast"] == DIAGNOSTIC_CONTRAST, "diagnostic contrast changed")
    task_clusters = _positive_int(design["task_clusters"], "design.task_clusters")
    _require(task_clusters == len(task_ids), "task cluster arithmetic drift")
    repetitions = _positive_int(design["repetitions_per_arm"], "design.repetitions_per_arm")
    expected_cells = task_clusters * repetitions * len(ARMS)
    _require(design["requested_cells"] == expected_cells, "requested-cell arithmetic drift")
    for field in ("agent_retry_limit", "replacement_cell_limit", "reserve_cell_limit"):
        _require(design[field] == 0, f"design.{field} must remain zero")
    _require(
        design["maximum_agent_invocations"] == expected_cells,
        "maximum-invocation arithmetic drift",
    )
    budget = _exact_keys(design["budget"], {"status", "authorized_usd"}, "design.budget")
    _require(
        budget == {"status": "not_frozen", "authorized_usd": None},
        "development product-cycle v1 cannot freeze or authorize a budget",
    )
    schedule, schedule_hash = _validate_schedule(
        design["schedule"], task_ids=task_ids, repetitions=repetitions
    )
    return schedule, schedule_hash, repetitions, expected_cells


def _validate_shared_execution(value: Any, *, task_hash: str, schedule_hash: str) -> dict[str, Any]:
    shared = _exact_keys(
        value,
        {
            "task_inventory_sha256",
            "corpus_sha256",
            "engine_sha256",
            "prompt_template_sha256",
            "prompt_parity_algorithm",
            "cache_policy_sha256",
            "runner_sha256",
            "price_quote_sha256",
            "pricing_policy_sha256",
            "model_id",
            "effort",
            "schedule_sha256",
            "identity_sha256",
        },
        "shared_execution",
    )
    _require(shared["task_inventory_sha256"] == task_hash, "shared task identity differs from the manifest")
    for field in (
        "corpus_sha256",
        "engine_sha256",
        "prompt_template_sha256",
        "cache_policy_sha256",
        "runner_sha256",
        "price_quote_sha256",
        "pricing_policy_sha256",
        "schedule_sha256",
    ):
        _sha256(shared[field], f"shared_execution.{field}")
    _nonempty_string(shared["model_id"], "shared_execution.model_id")
    _nonempty_string(shared["effort"], "shared_execution.effort")
    _require(
        shared["prompt_parity_algorithm"] == PROMPT_PARITY_ALGORITHM,
        "shared prompt parity algorithm changed",
    )
    _require(shared["schedule_sha256"] == schedule_hash, "shared schedule identity differs from the design")
    digest = _sha256(shared["identity_sha256"], "shared_execution.identity_sha256")
    _require(digest == self_sha256(shared), "shared execution identity hash mismatch")
    return shared


def _validate_execution_identity(
    value: Any,
    *,
    label: str,
    task: Mapping[str, Any],
    task_hash: str,
    shared: Mapping[str, Any],
    product_hash: str,
) -> None:
    identity = _exact_keys(
        value,
        {
            "task_sha256",
            "prompt_parity_sha256",
            "task_inventory_sha256",
            "corpus_sha256",
            "engine_sha256",
            "prompt_template_sha256",
            "prompt_parity_algorithm",
            "cache_policy_sha256",
            "runner_sha256",
            "price_quote_sha256",
            "pricing_policy_sha256",
            "model_id",
            "effort",
            "schedule_sha256",
            "product_identity_sha256",
            "identity_sha256",
        },
        label,
    )
    expected = {
        "task_sha256": task["task_sha256"],
        "prompt_parity_sha256": task["prompt_parity_sha256"],
        "task_inventory_sha256": task_hash,
        "corpus_sha256": shared["corpus_sha256"],
        "engine_sha256": shared["engine_sha256"],
        "prompt_template_sha256": shared["prompt_template_sha256"],
        "prompt_parity_algorithm": shared["prompt_parity_algorithm"],
        "cache_policy_sha256": shared["cache_policy_sha256"],
        "runner_sha256": shared["runner_sha256"],
        "price_quote_sha256": shared["price_quote_sha256"],
        "pricing_policy_sha256": shared["pricing_policy_sha256"],
        "model_id": shared["model_id"],
        "effort": shared["effort"],
        "schedule_sha256": shared["schedule_sha256"],
        "product_identity_sha256": product_hash,
    }
    for field, expected_value in expected.items():
        _require(identity[field] == expected_value, f"{label}.{field} parity mismatch")
    digest = _sha256(identity["identity_sha256"], f"{label}.identity_sha256")
    _require(digest == self_sha256(identity), f"{label} identity hash mismatch")


def _validate_timing(value: Any, label: str) -> Decimal:
    timing = _exact_keys(
        value,
        {
            "primary",
            "end_to_end_user_visible_wall_seconds",
            "timeout_occurred",
            "timeout_stage",
            "agent_timeout_limit_seconds",
            "timeout_component_limit_seconds",
            "pre_treatment_setup_included_in_primary",
            "treatment_retrieval_included_in_primary",
            "hidden_validation_included_in_primary",
        },
        label,
    )
    _require(timing["primary"] == "end_to_end_user_visible_wall_seconds", f"{label}.primary changed")
    elapsed = _finite_decimal(
        timing["end_to_end_user_visible_wall_seconds"],
        f"{label}.end_to_end_user_visible_wall_seconds",
        positive=True,
    )
    _finite_decimal(timing["agent_timeout_limit_seconds"], f"{label}.agent_timeout_limit_seconds", positive=True)
    _require(type(timing["timeout_occurred"]) is bool, f"{label}.timeout_occurred must be boolean")
    if timing["timeout_occurred"]:
        _require(
            timing["timeout_stage"] in {"treatment_retrieval_or_delivery", "agent_execution"},
            f"{label}.timeout_stage is invalid",
        )
        _finite_decimal(
            timing["timeout_component_limit_seconds"],
            f"{label}.timeout_component_limit_seconds",
            positive=True,
        )
    else:
        _require(timing["timeout_stage"] is None, f"{label}.timeout_stage must be null")
        _require(
            timing["timeout_component_limit_seconds"] is None,
            f"{label}.timeout_component_limit_seconds must be null",
        )
    _require(timing["pre_treatment_setup_included_in_primary"] is False, f"{label} timing boundary changed")
    _require(timing["treatment_retrieval_included_in_primary"] is True, f"{label} timing boundary changed")
    _require(timing["hidden_validation_included_in_primary"] is False, f"{label} timing boundary changed")
    return elapsed


def _validate_cost(value: Any, label: str) -> Decimal:
    cost = _exact_keys(
        value,
        {"schema", "currency", "complete", "categories", "total_usd"},
        label,
    )
    _require(cost["schema"] == NORMALIZED_COST_SCHEMA, f"{label}.schema changed")
    _require(cost["currency"] == "USD", f"{label}.currency must be USD")
    _require(cost["complete"] is True, f"{label} must attest complete category accounting")
    categories = _exact_keys(cost["categories"], set(PRICE_CATEGORIES), f"{label}.categories")
    parts = [_decimal(categories[name], f"{label}.categories.{name}") for name in PRICE_CATEGORIES]
    total = _decimal(cost["total_usd"], f"{label}.total_usd")
    _require(sum(parts, Decimal(0)) == total, f"{label} total/category arithmetic drift")
    return total


def _validate_quality(value: Any, label: str) -> tuple[Decimal, bool]:
    quality = _exact_keys(
        value,
        {
            "schema",
            "rubric",
            "task_normalized_score",
            "critical_failure",
            "critical_failure_reasons",
            "excluded_components",
        },
        label,
    )
    _require(quality["schema"] == QUALITY_SCHEMA, f"{label}.schema changed")
    _require(quality["rubric"] == QUALITY_RUBRIC, f"{label}.rubric changed")
    score = _finite_decimal(quality["task_normalized_score"], f"{label}.task_normalized_score")
    _require(Decimal(0) <= score <= Decimal(1), f"{label}.task_normalized_score must be in [0,1]")
    critical = quality["critical_failure"]
    _require(type(critical) is bool, f"{label}.critical_failure must be boolean")
    reasons = _string_list(quality["critical_failure_reasons"], f"{label}.critical_failure_reasons")
    _require(critical == bool(reasons), f"{label} critical flag/reasons disagree")
    _require(quality["excluded_components"] == list(QUALITY_EXCLUSIONS), f"{label}.excluded_components changed")
    if critical:
        _require(score == 0, f"{label} critical failure must force quality to zero")
    return score, critical


def _validate_failure(value: Any, label: str) -> bool:
    failure = _exact_keys(value, {"occurred", "kind", "return_code", "reason_codes"}, label)
    occurred = failure["occurred"]
    _require(type(occurred) is bool, f"{label}.occurred must be boolean")
    reasons = _string_list(failure["reason_codes"], f"{label}.reason_codes")
    return_code = failure["return_code"]
    _require(return_code is None or type(return_code) is int, f"{label}.return_code must be integer or null")
    if occurred:
        _nonempty_string(failure["kind"], f"{label}.kind")
        _require(bool(reasons), f"{label}.reason_codes must describe the failure")
    else:
        _require(failure["kind"] is None, f"{label}.kind must be null without a failure")
        _require(return_code == 0, f"{label}.return_code must be zero without a failure")
        _require(not reasons, f"{label}.reason_codes must be empty without a failure")
    return occurred


def _validate_validation(value: Any, label: str) -> bool:
    validation = _exact_keys(value, {"ok", "reason_codes"}, label)
    ok = validation["ok"]
    _require(type(ok) is bool, f"{label}.ok must be boolean")
    reasons = _string_list(validation["reason_codes"], f"{label}.reason_codes")
    _require(bool(reasons) != ok, f"{label}.reason_codes must be empty exactly when validation passes")
    return ok


def _validate_outcome(value: Any, label: str) -> dict[str, Any]:
    outcome = _exact_keys(
        value,
        {
            "executed",
            "timing",
            "normalized_cost",
            "code_quality",
            "failure",
            "validation",
            "identity_sha256",
        },
        label,
    )
    _require(outcome["executed"] is True, f"{label} must be an executed requested cell")
    elapsed = _validate_timing(outcome["timing"], f"{label}.timing")
    cost = _validate_cost(outcome["normalized_cost"], f"{label}.normalized_cost")
    quality, critical = _validate_quality(outcome["code_quality"], f"{label}.code_quality")
    failure = _validate_failure(outcome["failure"], f"{label}.failure")
    validation_ok = _validate_validation(outcome["validation"], f"{label}.validation")
    timed_out = outcome["timing"]["timeout_occurred"]
    if timed_out:
        _require(failure, f"{label} timeout must be recorded as a failure")
    if failure or not validation_ok or timed_out:
        _require(
            critical and quality == 0,
            f"{label} failure, timeout, or failed validation must force critical zero quality",
        )
    digest = _sha256(outcome["identity_sha256"], f"{label}.identity_sha256")
    _require(digest == self_sha256(outcome), f"{label} identity hash mismatch")
    return {
        "elapsed_time": elapsed,
        "normalized_cost": cost,
        "code_quality": quality,
        "failed": failure,
        "validation_ok": validation_ok,
        "timed_out": timed_out,
    }


def _validate_cells(
    value: Any,
    *,
    schedule: Mapping[int, tuple[str, int, str]],
    tasks: Mapping[str, Mapping[str, Any]],
    task_hash: str,
    shared: Mapping[str, Any],
    product_hashes: Mapping[str, str],
) -> list[dict[str, Any]]:
    _require(isinstance(value, list), "cells must be a list")
    _require(len(value) == len(schedule), f"cells must contain exactly {len(schedule)} requested measurements")
    seen_runs: set[str] = set()
    seen_positions: set[int] = set()
    measurements: list[dict[str, Any]] = []
    for index, raw in enumerate(value):
        label = f"cells[{index}]"
        cell = _exact_keys(
            raw,
            {
                "run_id",
                "schedule_position",
                "task_id",
                "repetition",
                "arm",
                "product_identity_sha256",
                "execution_identity",
                "outcome",
                "identity_sha256",
            },
            label,
        )
        run_id = _nonempty_string(cell["run_id"], f"{label}.run_id")
        _require(run_id not in seen_runs, f"duplicate run_id {run_id!r}")
        seen_runs.add(run_id)
        position = _positive_int(cell["schedule_position"], f"{label}.schedule_position")
        _require(position in schedule, f"{label} has an unexpected schedule position")
        _require(position not in seen_positions, f"duplicate measurement for schedule position {position}")
        seen_positions.add(position)
        task_id = _nonempty_string(cell["task_id"], f"{label}.task_id")
        repetition = _positive_int(cell["repetition"], f"{label}.repetition")
        arm = cell["arm"]
        _require((task_id, repetition, arm) == schedule[position], f"{label} does not match its scheduled cell")
        role = ARM_PRODUCT_ROLES[arm]
        expected_product_hash = product_hashes[role]
        _require(
            cell["product_identity_sha256"] == expected_product_hash,
            f"{label} product identity does not match arm mapping",
        )
        _validate_execution_identity(
            cell["execution_identity"],
            label=f"{label}.execution_identity",
            task=tasks[task_id],
            task_hash=task_hash,
            shared=shared,
            product_hash=expected_product_hash,
        )
        outcome = _validate_outcome(cell["outcome"], f"{label}.outcome")
        digest = _sha256(cell["identity_sha256"], f"{label}.identity_sha256")
        _require(digest == self_sha256(cell), f"{label} identity hash mismatch")
        measurements.append(
            {
                "run_id": run_id,
                "schedule_position": position,
                "task_id": task_id,
                "repetition": repetition,
                "arm": arm,
                **outcome,
            }
        )
    _require(seen_positions == set(schedule), "measurements do not cover the exact schedule")
    measurements.sort(key=lambda item: item["schedule_position"])
    return measurements


def _validated_manifest(manifest: Any) -> tuple[dict[str, Any], list[dict[str, Any]], dict[str, Any]]:
    root = _exact_keys(
        manifest,
        {
            "schema",
            "purpose",
            "evidence_class",
            "claim_scope",
            "product_identities",
            "tasks",
            "task_inventory_sha256",
            "shared_execution",
            "design",
            "cells",
            "manifest_sha256",
        },
        "manifest",
    )
    _require(root["schema"] == MANIFEST_SCHEMA, "product-cycle manifest schema changed")
    _require(root["purpose"] == "development_product_cycle_only", "product-cycle purpose changed")
    _require(
        root["evidence_class"] in {"synthetic_fixture", "development_measurement"},
        "evidence_class is invalid",
    )
    _require(
        root["claim_scope"] == "descriptive_not_confirmatory_no_budget_authorization",
        "product-cycle claim scope changed",
    )
    product_hashes = _validate_product_identities(root["product_identities"])
    task_ids, tasks, task_hash = _validate_tasks(root["tasks"], root["task_inventory_sha256"])
    schedule, schedule_hash, repetitions, expected_cells = _validate_design(root["design"], task_ids)
    shared = _validate_shared_execution(root["shared_execution"], task_hash=task_hash, schedule_hash=schedule_hash)
    measurements = _validate_cells(
        root["cells"],
        schedule=schedule,
        tasks=tasks,
        task_hash=task_hash,
        shared=shared,
        product_hashes=product_hashes,
    )
    digest = _sha256(root["manifest_sha256"], "manifest.manifest_sha256")
    _require(digest == self_sha256(root, "manifest_sha256"), "manifest identity hash mismatch")
    metadata = {
        "task_ids": task_ids,
        "task_inventory_sha256": task_hash,
        "schedule_sha256": schedule_hash,
        "shared_execution_sha256": shared["identity_sha256"],
        "product_binding_sha256": product_hashes["binding"],
        "repetitions": repetitions,
        "expected_cells": expected_cells,
    }
    return root, measurements, metadata


def preflight_manifest(manifest: Any) -> dict[str, Any]:
    """Validate all identities, parity fields, schedule cells, and arithmetic."""
    root, measurements, metadata = _validated_manifest(manifest)
    return {
        "schema": PREFLIGHT_SCHEMA,
        "status": "valid",
        "manifest_sha256": root["manifest_sha256"],
        "claim_scope": root["claim_scope"],
        "budget_status": "not_frozen",
        "task_clusters": len(metadata["task_ids"]),
        "repetitions_per_arm": metadata["repetitions"],
        "arms": list(ARMS),
        "requested_cells": metadata["expected_cells"],
        "validated_cells": len(measurements),
        "maximum_agent_invocations": metadata["expected_cells"],
        "task_inventory_sha256": metadata["task_inventory_sha256"],
        "schedule_sha256": metadata["schedule_sha256"],
        "shared_execution_sha256": metadata["shared_execution_sha256"],
        "product_binding_sha256": metadata["product_binding_sha256"],
    }


def _task_arm_means(
    measurements: Sequence[Mapping[str, Any]], task_ids: Sequence[str], field: str
) -> dict[str, dict[str, Decimal]]:
    grouped: dict[tuple[str, str], list[Decimal]] = defaultdict(list)
    for cell in measurements:
        grouped[(cell["task_id"], cell["arm"])].append(cell[field])
    return {
        task_id: {
            arm: sum(grouped[(task_id, arm)], Decimal(0))
            / Decimal(len(grouped[(task_id, arm)]))
            for arm in ARMS
        }
        for task_id in task_ids
    }


def _stable_float(value: Decimal | float) -> float:
    number = float(value)
    _require(math.isfinite(number), "analysis produced a non-finite result")
    return float(format(number, ".12g"))


def _ratio_endpoint(
    means: Mapping[str, Mapping[str, Decimal]],
    task_ids: Sequence[str],
    *,
    numerator: str,
    denominator: str,
    source_field: str,
) -> dict[str, Any]:
    ratios: list[float] = []
    for task_id in task_ids:
        denominator_value = means[task_id][denominator]
        _require(denominator_value > 0, f"{source_field} denominator must be positive for task {task_id}")
        ratios.append(float(means[task_id][numerator] / denominator_value))
    log_mean = sum(math.log(value) for value in ratios) / len(ratios)
    return {
        "source_field": source_field,
        "estimand": "paired_task_geometric_mean_ratio",
        "repetition_aggregation": "arithmetic_mean_within_task_arm_before_contrast",
        "cluster_unit": "task",
        "n_task_clusters": len(task_ids),
        "paired_task_geometric_mean_ratio": _stable_float(math.exp(log_mean)),
    }


def _cost_endpoint(
    means: Mapping[str, Mapping[str, Decimal]],
    task_ids: Sequence[str],
    *,
    numerator: str,
    denominator: str,
) -> dict[str, Any]:
    numerator_mean = sum((means[task][numerator] for task in task_ids), Decimal(0)) / Decimal(len(task_ids))
    denominator_mean = sum((means[task][denominator] for task in task_ids), Decimal(0)) / Decimal(len(task_ids))
    _require(denominator_mean > 0, "normalized-cost equal-task denominator must be positive")
    with localcontext() as context:
        context.prec = 40
        ratio = numerator_mean / denominator_mean
    return {
        "source_field": "outcome.normalized_cost.total_usd",
        "estimand": "ratio_of_equal_task_weighted_task_arm_mean_costs",
        "repetition_aggregation": "arithmetic_mean_within_task_arm_before_contrast",
        "task_aggregation": "equal_weight_arithmetic_mean_before_ratio",
        "cluster_unit": "task",
        "n_task_clusters": len(task_ids),
        "numerator_equal_task_mean_usd": _stable_float(numerator_mean),
        "denominator_equal_task_mean_usd": _stable_float(denominator_mean),
        "equal_task_weighted_mean_ratio": _stable_float(ratio),
    }


def _quality_endpoint(
    means: Mapping[str, Mapping[str, Decimal]],
    task_ids: Sequence[str],
    *,
    numerator: str,
    denominator: str,
) -> dict[str, Any]:
    differences = [means[task][numerator] - means[task][denominator] for task in task_ids]
    difference = sum(differences, Decimal(0)) / Decimal(len(differences))
    return {
        "source_field": "outcome.code_quality.task_normalized_score_with_critical_failure_zero",
        "estimand": "paired_task_mean_difference",
        "repetition_aggregation": "arithmetic_mean_within_task_arm_before_contrast",
        "cluster_unit": "task",
        "n_task_clusters": len(task_ids),
        "paired_task_mean_difference": _stable_float(difference),
    }


def _contrast_endpoints(
    measurements: Sequence[Mapping[str, Any]],
    task_ids: Sequence[str],
    *,
    numerator: str,
    denominator: str,
) -> dict[str, Any]:
    elapsed = _task_arm_means(measurements, task_ids, "elapsed_time")
    costs = _task_arm_means(measurements, task_ids, "normalized_cost")
    quality = _task_arm_means(measurements, task_ids, "code_quality")
    return {
        "elapsed_time": _ratio_endpoint(
            elapsed,
            task_ids,
            numerator=numerator,
            denominator=denominator,
            source_field="outcome.timing.end_to_end_user_visible_wall_seconds",
        ),
        "normalized_cost": _cost_endpoint(
            costs,
            task_ids,
            numerator=numerator,
            denominator=denominator,
        ),
        "code_quality": _quality_endpoint(
            quality,
            task_ids,
            numerator=numerator,
            denominator=denominator,
        ),
    }


def analyze_manifest(manifest: Any) -> dict[str, Any]:
    """Return deterministic descriptive results for both frozen contrasts."""
    root, measurements, metadata = _validated_manifest(manifest)
    task_ids = metadata["task_ids"]
    arm_summary: dict[str, Any] = {}
    endpoint_fields = {
        "mean_elapsed_seconds": "elapsed_time",
        "mean_normalized_cost_usd": "normalized_cost",
        "mean_task_normalized_quality": "code_quality",
    }
    for arm in ARMS:
        arm_cells = [cell for cell in measurements if cell["arm"] == arm]
        arm_summary[arm] = {
            "executed_cells": len(arm_cells),
            **{
                name: _stable_float(
                    sum((cell[field] for cell in arm_cells), Decimal(0)) / Decimal(len(arm_cells))
                )
                for name, field in endpoint_fields.items()
            },
            "failures": sum(bool(cell["failed"]) for cell in arm_cells),
            "validation_passes": sum(bool(cell["validation_ok"]) for cell in arm_cells),
            "timeouts": sum(bool(cell["timed_out"]) for cell in arm_cells),
        }

    primary_endpoints = _contrast_endpoints(
        measurements,
        task_ids,
        numerator=PRIMARY_CONTRAST["numerator"],
        denominator=PRIMARY_CONTRAST["denominator"],
    )
    diagnostic_endpoints = _contrast_endpoints(
        measurements,
        task_ids,
        numerator=DIAGNOSTIC_CONTRAST["numerator"],
        denominator=DIAGNOSTIC_CONTRAST["denominator"],
    )
    report: dict[str, Any] = {
        "schema": REPORT_SCHEMA,
        "status": "evaluated_descriptive_only",
        "manifest_sha256": root["manifest_sha256"],
        "claim_scope": root["claim_scope"],
        "design": {
            "arms": list(ARMS),
            "task_clusters": len(task_ids),
            "repetitions_per_arm": metadata["repetitions"],
            "requested_cells": metadata["expected_cells"],
            "analyzed_cells": len(measurements),
            "cluster_unit": "task",
        },
        "primary_result": {
            "contrast": PRIMARY_CONTRAST["id"],
            "role": PRIMARY_CONTRAST["role"],
            "verdict": "descriptive_only_no_frozen_threshold",
            "eligible_for_confirmatory_claim": False,
            "endpoints": primary_endpoints,
        },
        "diagnostics": {
            DIAGNOSTIC_CONTRAST["id"]: {
                "role": DIAGNOSTIC_CONTRAST["role"],
                "eligible_for_primary_verdict": False,
                "endpoints": diagnostic_endpoints,
            }
        },
        "arm_summary": arm_summary,
        "integrity": {
            "all_requested_cells_analyzed": len(measurements) == metadata["expected_cells"],
            "all_cost_categories_complete": True,
            "repetitions_aggregated_inside_task": True,
            "task_inventory_sha256": metadata["task_inventory_sha256"],
            "schedule_sha256": metadata["schedule_sha256"],
            "shared_execution_sha256": metadata["shared_execution_sha256"],
            "product_binding_sha256": metadata["product_binding_sha256"],
        },
    }
    report["report_sha256"] = self_sha256(report, "report_sha256")
    return report


def _write_json(value: Any) -> None:
    sys.stdout.write(json.dumps(value, ensure_ascii=False, allow_nan=False, indent=2, sort_keys=True) + "\n")


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="command", required=True)
    for command in ("preflight", "analyze"):
        child = subparsers.add_parser(command)
        child.add_argument("manifest", type=pathlib.Path)
    args = parser.parse_args(argv)
    try:
        manifest = load_manifest(args.manifest)
        result = preflight_manifest(manifest) if args.command == "preflight" else analyze_manifest(manifest)
    except ProductCycleError as exc:
        sys.stderr.write(f"product-cycle {args.command} failed: {exc}\n")
        return 2
    _write_json(result)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
