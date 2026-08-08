#!/usr/bin/env python3
"""Classify injected multi-repository negative-control attempt observations.

This module is deliberately non-executing. It validates a complete public
schedule of injected, unattested attempt observations and derives a canonical
classification receipt. It cannot invoke Git, Go, a candidate, a worktree, a
model/provider, private storage, or benchmark execution.
"""

from __future__ import annotations

import argparse
import copy
import hashlib
import os
import pathlib
import platform
import sys
import tempfile
import unicodedata
from collections import Counter
from collections.abc import Mapping, Sequence
from typing import Any, cast

import negative_control_private_log as private_log
import task_negative_control_gate_v1 as gate
import task_negative_control_plan_v2 as run_plan


ATTEMPT_PROFILE = "agent_brain_negative_control_attempt_observations_v1"
RECEIPT_PROFILE = "agent_brain_negative_control_classification_receipt_v1"
SCHEMA_VERSION = 1
ATTEMPT_STATUS = "injected_attempt_observations_unattested_execution_forbidden"
RECEIPT_STATUS = "classification_applied_to_injected_unattested_attempts"
EXECUTION_STATUS = "forbidden_missing_attested_executor_and_remaining_gates"
EXPOSURE = "permanent_development_only_identity_inspected"
OBSERVATION_KIND = "injected_attempt_result_unattested"
CANONICAL_JSON_PROFILE = gate.CANONICAL_JSON_PROFILE
ARTIFACT_RENDER_PROFILE = gate.ARTIFACT_RENDER_PROFILE
ARM_ORDER = ("baseline", "first_parent_source_reversal")
REPETITIONS = (1, 2)
TOTAL_CANDIDATES = run_plan.TOTAL_CANDIDATES
TOTAL_ATTEMPTS = TOTAL_CANDIDATES * len(ARM_ORDER) * len(REPETITIONS)
MAX_ARTIFACT_RAW_BYTES = 4 * 1024 * 1024
MAX_SCHEMA_RAW_BYTES = 4 * 1024 * 1024
MAX_IMPLEMENTATION_SOURCE_BYTES = 4 * 1024 * 1024
MAX_PYTHON_EXECUTABLE_BYTES = 256 * 1024 * 1024
MAX_EXIT_CODE = 255
MAX_ATTEMPT_RAW_LOG_BYTES = (
    TOTAL_ATTEMPTS * private_log.MAX_PRIVATE_RAW_LOG_BYTES_PER_ARM
)
ROOT = pathlib.Path(__file__).parent
CHECKED_GATE_PRIMITIVE_SHA256 = "f04b8634caf619cc17bb495975d2b5358164be6eae2a124ade84efebd60c51dd"
CHECKED_RUN_PLAN_BUILDER_SHA256 = "23d08e9d3d23935cdc86b53780b609fb35f6b3020ede6b1360af288939e34715"
CHECKED_PRIVATE_LOG_WRITER_SHA256 = "2a88ef48cc52d52e477b145c06bd0a55d66cc487570aa766cb4da23897121216"
CHECKED_PRIVATE_LOG_SCHEMA_SHA256 = "6b3378bb3a2bf8a687459aafb3ceefac963995897bca1d4f97cb961fd7b9abf8"
CHECKED_ATTEMPT_SCHEMA_SHA256 = "d5a036e22ded76cc485fc62742a4f85d93ff6d228063ffc2c014fa13725b53ca"
CHECKED_RECEIPT_SCHEMA_SHA256 = "c025b1fde9dba21b1b139aa7f9e5ae5f9bc1912aa61bf33921fe62c07aefcca0"

CLASSIFICATIONS = (
    "eligible_for_symptom_review",
    "negative_control_survived",
    "baseline_invalid_failure",
    "baseline_invalid_timeout",
    "baseline_inconsistent",
    "reversed_invalid_timeout",
    "reversed_inconsistent",
)

RESIDUAL_GATES = [
    "owner_execution_approval_receipt_and_trust_mechanism",
    "authorized_plan_binding_for_actual_cache_seed",
    "safe_archive_traversal_type_link_device_and_content_verifier",
    "trusted_filesystem_observer_and_atomic_resource_reservation",
    "negative_control_executor_and_reversal_proof",
    "attested_attempt_producer_and_classifier_integration",
    "private_log_executor_integration_retention_and_aggregate_accounting",
    "fail_closed_cleanup_and_no_receipt_on_interruption",
]


class ClassificationError(ValueError):
    """Raised when injected observations cannot yield a trustworthy receipt."""


def _require(condition: bool, message: str) -> None:
    if not condition:
        raise ClassificationError(message)


def _sha256(raw: bytes) -> str:
    _require(type(raw) is bytes, "SHA-256 input must be exact built-in bytes")
    return hashlib.sha256(raw).hexdigest()


def _valid_sha(value: Any, *, allow_zero: bool = False) -> bool:
    return (
        type(value) is str
        and len(cast(str, value)) == 64
        and run_plan.eligibility.SHA256_RE.fullmatch(cast(str, value)) is not None
        and (allow_zero or value != "0" * 64)
    )


def _canonical_json_bytes(value: Any) -> bytes:
    try:
        gate._validate_json_profile(value)
        return gate._canonical_json_bytes(value)
    except gate.GatePrimitiveError as exc:
        raise ClassificationError(str(exc)) from exc


def _canonical_hash(value: Any) -> str:
    return _sha256(_canonical_json_bytes(value))


def _exact_json_equal(actual: Any, expected: Any) -> bool:
    """Compare JSON values without Python's bool/int equality coercion."""

    return _canonical_json_bytes(actual) == _canonical_json_bytes(expected)


def _render(value: dict[str, Any]) -> bytes:
    try:
        raw = gate._render(value)
    except gate.GatePrimitiveError as exc:
        raise ClassificationError(str(exc)) from exc
    _require(
        len(raw) <= MAX_ARTIFACT_RAW_BYTES,
        "classification artifact exceeds the raw-byte ceiling",
    )
    return raw


def _self_hash(value: dict[str, Any], field: str) -> str:
    projected = copy.deepcopy(value)
    _require(field in projected, f"{field} is missing")
    projected[field] = None
    return _canonical_hash(projected)


def _read_json(
    path: pathlib.Path,
    *,
    max_raw_bytes: int = MAX_ARTIFACT_RAW_BYTES,
    label: str,
) -> tuple[dict[str, Any], bytes]:
    try:
        return gate._load_json(path, max_raw_bytes=max_raw_bytes, label=label)
    except gate.GatePrimitiveError as exc:
        raise ClassificationError(str(exc)) from exc


def _read_bounded(path: pathlib.Path, *, maximum: int, label: str) -> bytes:
    try:
        return gate._read_bounded(path, max_raw_bytes=maximum, label=label)
    except gate.GatePrimitiveError as exc:
        raise ClassificationError(str(exc)) from exc


def _source_hash(module: Any, label: str) -> str:
    source = getattr(module, "__file__", None)
    _require(type(source) is str and bool(source), f"{label} source is unavailable")
    return _sha256(
        _read_bounded(
            pathlib.Path(cast(str, source)),
            maximum=MAX_IMPLEMENTATION_SOURCE_BYTES,
            label=f"{label} source",
        )
    )


def _implementation_identity(
    *,
    attempt_schema_path: pathlib.Path,
    receipt_schema_path: pathlib.Path,
    private_log_schema_path: pathlib.Path,
    gate_receipt_schema_path: pathlib.Path,
) -> dict[str, Any]:
    attempt_schema_sha256 = _sha256(
        _read_bounded(
            attempt_schema_path,
            maximum=MAX_SCHEMA_RAW_BYTES,
            label="attempt schema",
        )
    )
    receipt_schema_sha256 = _sha256(
        _read_bounded(
            receipt_schema_path,
            maximum=MAX_SCHEMA_RAW_BYTES,
            label="classification receipt schema",
        )
    )
    private_log_schema_sha256 = _sha256(
        _read_bounded(
            private_log_schema_path,
            maximum=MAX_SCHEMA_RAW_BYTES,
            label="private-log receipt schema",
        )
    )
    gate_receipt_schema_sha256 = _sha256(
        _read_bounded(
            gate_receipt_schema_path,
            maximum=MAX_SCHEMA_RAW_BYTES,
            label="gate receipt schema",
        )
    )
    _require(
        attempt_schema_sha256 == CHECKED_ATTEMPT_SCHEMA_SHA256,
        "attempt schema differs from the checked v1 schema",
    )
    _require(
        receipt_schema_sha256 == CHECKED_RECEIPT_SCHEMA_SHA256,
        "classification receipt schema differs from the checked v1 schema",
    )
    _require(
        private_log_schema_sha256 == CHECKED_PRIVATE_LOG_SCHEMA_SHA256,
        "private-log receipt schema differs from the checked v1 schema",
    )
    _require(
        gate_receipt_schema_sha256 == gate.CHECKED_RECEIPT_SCHEMA_FILE_SHA256,
        "gate receipt schema differs from the checked v1 schema",
    )
    gate_hash = _source_hash(gate, "gate primitive")
    run_plan_hash = _source_hash(run_plan, "run-plan builder")
    private_log_hash = _source_hash(private_log, "private-log writer")
    _require(
        gate_hash == CHECKED_GATE_PRIMITIVE_SHA256,
        "gate primitive source differs from the checked implementation",
    )
    _require(
        run_plan_hash == CHECKED_RUN_PLAN_BUILDER_SHA256,
        "run-plan builder source differs from the checked implementation",
    )
    _require(
        private_log_hash == CHECKED_PRIVATE_LOG_WRITER_SHA256,
        "private-log writer source differs from the checked implementation",
    )
    try:
        python_executable = pathlib.Path(sys.executable).resolve(strict=True)
    except OSError as exc:
        raise ClassificationError("Python executable is unavailable") from exc
    return {
        "artifact_render_profile": ARTIFACT_RENDER_PROFILE,
        "attempt_schema_sha256": attempt_schema_sha256,
        "builder_sha256": _source_hash(sys.modules[__name__], "classification builder"),
        "canonical_json_profile": CANONICAL_JSON_PROFILE,
        "gate_primitive_sha256": gate_hash,
        "gate_receipt_schema_sha256": gate_receipt_schema_sha256,
        "private_log_receipt_schema_sha256": private_log_schema_sha256,
        "private_log_writer_sha256": private_log_hash,
        "python_executable_sha256": _sha256(
            _read_bounded(
                python_executable,
                maximum=MAX_PYTHON_EXECUTABLE_BYTES,
                label="Python executable",
            )
        ),
        "python_implementation": platform.python_implementation(),
        "python_version": platform.python_version(),
        "receipt_schema_sha256": receipt_schema_sha256,
        "run_plan_builder_sha256": run_plan_hash,
        "unicode_data_version": unicodedata.unidata_version,
    }


def _authority(plan: dict[str, Any]) -> dict[str, Any]:
    try:
        authority = gate._pending_authority(plan)
    except (gate.GatePrimitiveError, run_plan.RunPlanError) as exc:
        raise ClassificationError(str(exc)) from exc
    expected = {
        "benchmark_execution": "forbidden_pending_separate_owner_approval",
        "candidate_execution": "forbidden_plan_unexecutable",
        "model_provider_execution": "forbidden_not_authorized",
        "paid_execution": "forbidden_not_authorized",
        "plan_status": "pending_owner_authorization",
    }
    _require(
        _exact_json_equal(authority, expected),
        "classification authority boundary differs",
    )
    return expected


def _protocol(plan: dict[str, Any]) -> dict[str, Any]:
    protocol = plan["execution_protocol"]
    expected = {
        "arm_order": list(ARM_ORDER),
        "candidate_order": "repository_order_then_first_parent_position_serial_v1",
        "max_concurrency": 1,
        "observation_kind": OBSERVATION_KIND,
        "outer_timeout_seconds_per_arm": 600,
        "repetitions_per_arm": 2,
        "repository_order": list(run_plan.REPOSITORY_ORDER),
        "total_attempt_count": TOTAL_ATTEMPTS,
        "total_candidate_count": TOTAL_CANDIDATES,
    }
    _require(
        _exact_json_equal(protocol["arm_order"], expected["arm_order"]),
        "plan arm order differs",
    )
    _require(
        protocol["candidate_order"] == expected["candidate_order"],
        "plan candidate order differs",
    )
    _require(
        type(protocol["max_concurrency"]) is int
        and protocol["max_concurrency"] == 1,
        "plan concurrency differs",
    )
    _require(
        type(protocol["outer_timeout_seconds_per_arm"]) is int
        and protocol["outer_timeout_seconds_per_arm"] == 600,
        "plan outer timeout differs",
    )
    _require(
        type(protocol["repetitions_per_arm"]) is int
        and protocol["repetitions_per_arm"] == 2,
        "plan repetition count differs",
    )
    _require(
        _exact_json_equal(
            protocol["repository_order"], expected["repository_order"]
        ),
        "plan repository order differs",
    )
    _require(
        type(protocol["total_candidate_count"]) is int
        and protocol["total_candidate_count"] == TOTAL_CANDIDATES,
        "plan candidate total differs",
    )
    return expected


def _repository_bindings(plan: dict[str, Any]) -> list[dict[str, Any]]:
    bindings: list[dict[str, Any]] = []
    for repository in plan["repositories"]:
        bindings.append(
            {
                "base_oid": repository["base_oid"],
                "candidate_count": repository["candidate_count"],
                "candidate_order_sha256": repository["candidate_order_sha256"],
                "head_oid": repository["head_oid"],
                "key": repository["key"],
                "ledger_file_sha256": repository["ledger_file_sha256"],
                "ledger_sha256": repository["ledger_sha256"],
                "repository_id": repository["repository_id"],
                "toolchain_sha256": _canonical_hash(repository["toolchain"]),
            }
        )
    _require(
        _exact_json_equal(
            [item["key"] for item in bindings], list(run_plan.REPOSITORY_ORDER)
        ),
        "repository binding order differs",
    )
    return bindings


def _validate_gate_dependencies(
    *,
    plan: dict[str, Any],
    plan_raw: bytes,
    gate_manifest: dict[str, Any],
    gate_manifest_raw: bytes,
    gate_receipt: dict[str, Any],
    gate_receipt_raw: bytes,
    gate_manifest_schema_path: pathlib.Path,
    gate_receipt_schema_path: pathlib.Path,
) -> dict[str, Any]:
    _require(type(plan) is dict, "run plan must be an exact built-in object")
    _require(type(plan_raw) is bytes, "run-plan bytes must be exact built-in bytes")
    _require(
        type(gate_manifest) is dict,
        "gate manifest must be an exact built-in object",
    )
    _require(
        type(gate_manifest_raw) is bytes,
        "gate-manifest bytes must be exact built-in bytes",
    )
    _require(
        type(gate_receipt) is dict,
        "gate receipt must be an exact built-in object",
    )
    _require(
        type(gate_receipt_raw) is bytes,
        "gate-receipt bytes must be exact built-in bytes",
    )
    authority = _authority(plan)
    _require(
        len(plan_raw) <= gate.MAX_PLAN_RAW_BYTES
        and _sha256(plan_raw) == gate.CHECKED_PLAN_FILE_SHA256,
        "run-plan raw binding differs",
    )
    _require(plan_raw == run_plan._render(plan), "run-plan bytes are not canonical")
    _require(
        len(gate_manifest_raw) <= gate.MAX_MANIFEST_RAW_BYTES,
        "gate manifest exceeds the raw-byte ceiling",
    )
    _require(
        gate_manifest_raw == gate._render(gate_manifest),
        "gate-manifest bytes are not canonical",
    )
    _require(
        len(gate_receipt_raw) <= gate.MAX_RECEIPT_RAW_BYTES,
        "gate receipt exceeds the raw-byte ceiling",
    )
    _require(
        gate_receipt_raw == gate._render(gate_receipt),
        "gate-receipt bytes are not canonical",
    )
    try:
        gate.validate_preflight_receipt(
            gate_receipt,
            plan=plan,
            plan_raw=plan_raw,
            manifest=gate_manifest,
            manifest_raw=gate_manifest_raw,
            manifest_schema_path=gate_manifest_schema_path,
            receipt_schema_path=gate_receipt_schema_path,
        )
    except (gate.GatePrimitiveError, run_plan.RunPlanError) as exc:
        raise ClassificationError(f"gate dependency is invalid: {exc}") from exc
    _require(
        _exact_json_equal(gate_receipt["authority"], authority)
        and gate_receipt["execution_status"] == "forbidden_missing_remaining_gates"
        and gate_receipt["status"] == gate.RECEIPT_STATUS,
        "gate receipt overclaims execution authority",
    )
    return authority


def _base_inputs(
    *,
    plan: dict[str, Any],
    plan_raw: bytes,
    gate_manifest: dict[str, Any],
    gate_manifest_raw: bytes,
    gate_receipt: dict[str, Any],
    gate_receipt_raw: bytes,
    implementation: dict[str, Any],
) -> dict[str, Any]:
    return {
        "gate_manifest_file_sha256": _sha256(gate_manifest_raw),
        "gate_manifest_sha256": gate_manifest["manifest_sha256"],
        "gate_receipt_file_sha256": _sha256(gate_receipt_raw),
        "gate_receipt_sha256": gate_receipt["receipt_sha256"],
        "plan_file_sha256": _sha256(plan_raw),
        "plan_sha256": plan["plan_sha256"],
        "private_log_receipt_schema_file_sha256": implementation[
            "private_log_receipt_schema_sha256"
        ],
        "private_log_writer_sha256": implementation["private_log_writer_sha256"],
        "toolchain_bindings_sha256": _canonical_hash(
            gate._toolchain_projection(plan)
        ),
    }


def _expected_schedule(plan: dict[str, Any]) -> list[dict[str, Any]]:
    schedule: list[dict[str, Any]] = []
    ordinal = 1
    for repository in plan["repositories"]:
        for candidate in repository["candidate_order"]:
            for arm in ARM_ORDER:
                for repetition in REPETITIONS:
                    schedule.append(
                        {
                            "arm": arm,
                            "attempt_ordinal": ordinal,
                            "candidate_ref": candidate["candidate_ref"],
                            "first_parent_position": candidate["first_parent_position"],
                            "repetition": repetition,
                            "repository_id": repository["repository_id"],
                            "repository_key": repository["key"],
                        }
                    )
                    ordinal += 1
    _require(len(schedule) == TOTAL_ATTEMPTS, "attempt schedule count differs")
    return schedule


def _empty_outcome_counts() -> dict[str, dict[str, int]]:
    return {
        arm: {"failed": 0, "passed": 0, "timeout": 0}
        for arm in ARM_ORDER
    }


def _validate_attempt_observations(
    attempts: Any,
    *,
    plan: dict[str, Any],
) -> tuple[list[dict[str, Any]], dict[str, Any]]:
    _require(type(attempts) is list, "attempts must be an exact built-in array")
    attempt_values = cast(list[Any], attempts)
    try:
        gate._validate_json_profile(attempt_values)
    except gate.GatePrimitiveError as exc:
        raise ClassificationError(str(exc)) from exc
    _require(len(attempt_values) == TOTAL_ATTEMPTS, "attempt count is not exactly 248")
    schedule = _expected_schedule(plan)
    expected_fields = {
        "arm",
        "attempt_ordinal",
        "candidate_ref",
        "exit_code",
        "first_parent_position",
        "observation_kind",
        "raw_log",
        "repetition",
        "repository_id",
        "repository_key",
        "status",
        "test_command_sha256",
    }
    expected_raw_log_fields = {
        "raw_log_byte_count",
        "raw_log_sha256",
        "receipt_file_sha256",
    }
    normalized: list[dict[str, Any]] = []
    outcome_counts = _empty_outcome_counts()
    unique_logs: dict[str, int] = {}
    attempt_log_bytes = 0
    identity_fields = set(schedule[0])
    for index, (attempt_value, expected) in enumerate(
        zip(attempt_values, schedule, strict=True)
    ):
        _require(
            type(attempt_value) is dict and set(attempt_value) == expected_fields,
            f"attempt[{index}] fields differ",
        )
        attempt = cast(dict[str, Any], attempt_value)
        identity = {field: attempt[field] for field in identity_fields}
        _require(
            _exact_json_equal(identity, expected),
            f"attempt[{index}] schedule identity differs",
        )
        _require(
            attempt["observation_kind"] == OBSERVATION_KIND,
            f"attempt[{index}] observation kind differs",
        )
        status = attempt["status"]
        exit_code = attempt["exit_code"]
        _require(
            type(status) is str and status in {"completed", "timeout"},
            f"attempt[{index}] status is invalid",
        )
        if status == "completed":
            _require(
                type(exit_code) is int and 0 <= exit_code <= MAX_EXIT_CODE,
                f"attempt[{index}] completed exit code is invalid",
            )
        else:
            _require(
                exit_code is None,
                f"attempt[{index}] timeout exit code must be null",
            )
        _require(
            _valid_sha(attempt["test_command_sha256"]),
            f"attempt[{index}] test-command hash is invalid",
        )
        raw_log_value = attempt["raw_log"]
        _require(
            type(raw_log_value) is dict and set(raw_log_value) == expected_raw_log_fields,
            f"attempt[{index}] raw-log fields differ",
        )
        raw_log = cast(dict[str, Any], raw_log_value)
        raw_digest_value = raw_log["raw_log_sha256"]
        raw_byte_count_value = raw_log["raw_log_byte_count"]
        _require(
            _valid_sha(raw_digest_value, allow_zero=True),
            f"attempt[{index}] raw-log SHA-256 is invalid",
        )
        _require(
            type(raw_byte_count_value) is int
            and 0
            <= raw_byte_count_value
            <= private_log.MAX_PRIVATE_RAW_LOG_BYTES_PER_ARM,
            f"attempt[{index}] raw-log byte count is invalid",
        )
        raw_digest = cast(str, raw_digest_value)
        raw_byte_count = cast(int, raw_byte_count_value)
        canonical_receipt = private_log.RawLogReceipt(
            raw_log_sha256=raw_digest,
            raw_log_byte_count=raw_byte_count,
        ).canonical_bytes()
        try:
            private_log.parse_receipt(canonical_receipt)
        except private_log.PrivateLogError as exc:
            raise ClassificationError(
                f"attempt[{index}] raw-log receipt is invalid"
            ) from exc
        _require(
            _valid_sha(raw_log["receipt_file_sha256"])
            and raw_log["receipt_file_sha256"] == _sha256(canonical_receipt),
            f"attempt[{index}] raw-log receipt-file hash differs",
        )
        prior_count = unique_logs.get(raw_digest)
        _require(
            prior_count is None or prior_count == raw_byte_count,
            f"attempt[{index}] repeated raw-log digest has a different size",
        )
        unique_logs[raw_digest] = raw_byte_count
        attempt_log_bytes += raw_byte_count
        _require(
            attempt_log_bytes <= MAX_ATTEMPT_RAW_LOG_BYTES,
            "attempt raw-log byte sum exceeds its arithmetic ceiling",
        )
        arm = cast(str, attempt["arm"])
        outcome_counts[arm][_outcome(attempt)] += 1
        normalized.append(copy.deepcopy(attempt))
    for offset in range(0, TOTAL_ATTEMPTS, 4):
        command_hashes = {
            normalized[index]["test_command_sha256"]
            for index in range(offset, offset + 4)
        }
        _require(
            len(command_hashes) == 1,
            f"candidate[{offset // 4}] test-command hashes differ",
        )
    unique_log_bytes = sum(unique_logs.values())
    _require(
        unique_log_bytes <= plan["resource_budget"]["max_private_raw_log_bytes_total"],
        "unique private raw-log bytes exceed the frozen total ceiling",
    )
    _require(
        len(unique_logs) <= private_log.MAX_PRIVATE_RAW_LOG_FINAL_FILE_COUNT,
        "unique private raw-log count exceeds the frozen file ceiling",
    )
    for arm in ARM_ORDER:
        _require(
            sum(outcome_counts[arm].values()) == TOTAL_CANDIDATES * 2,
            f"{arm} outcome count differs",
        )
    summary = {
        "attempt_count": TOTAL_ATTEMPTS,
        "candidate_count": TOTAL_CANDIDATES,
        "outcome_counts": outcome_counts,
        "raw_log_attempt_byte_count": attempt_log_bytes,
        "raw_log_unique_byte_count": unique_log_bytes,
        "raw_log_unique_count": len(unique_logs),
        "repository_count": len(run_plan.REPOSITORY_ORDER),
    }
    return normalized, summary


def _derive_classifications(
    attempts: list[dict[str, Any]],
) -> tuple[list[dict[str, Any]], dict[str, int]]:
    _require(
        type(attempts) is list and len(attempts) == TOTAL_ATTEMPTS,
        "complete attempts are required for classification",
    )
    classifications: list[dict[str, Any]] = []
    counts: Counter[str] = Counter()
    for candidate_index, offset in enumerate(range(0, TOTAL_ATTEMPTS, 4)):
        group = attempts[offset : offset + 4]
        _require(len(group) == 4, "candidate attempt group is incomplete")
        baseline = [_outcome(item) for item in group[:2]]
        reversed_source = [_outcome(item) for item in group[2:]]
        classification = classify_candidate(baseline, reversed_source)
        first = group[0]
        row = {
            "baseline_outcomes": baseline,
            "candidate_ordinal": candidate_index + 1,
            "candidate_ref": first["candidate_ref"],
            "classification": classification,
            "first_parent_position": first["first_parent_position"],
            "repository_id": first["repository_id"],
            "repository_key": first["repository_key"],
            "reversed_source_outcomes": reversed_source,
        }
        classifications.append(row)
        counts[classification] += 1
    _require(
        len(classifications) == TOTAL_CANDIDATES,
        "classification count is not exactly 62",
    )
    return classifications, {name: counts[name] for name in CLASSIFICATIONS}


def _validate_schema_projections(
    *,
    plan: dict[str, Any],
    attempt_schema_path: pathlib.Path,
    receipt_schema_path: pathlib.Path,
) -> None:
    attempt_schema, _ = _read_json(
        attempt_schema_path,
        max_raw_bytes=MAX_SCHEMA_RAW_BYTES,
        label="attempt schema",
    )
    receipt_schema, _ = _read_json(
        receipt_schema_path,
        max_raw_bytes=MAX_SCHEMA_RAW_BYTES,
        label="classification receipt schema",
    )
    expected_bindings = _repository_bindings(plan)
    schedule = _expected_schedule(plan)
    candidates = [schedule[offset] for offset in range(0, TOTAL_ATTEMPTS, 4)]
    expected_attempt_prefix = []
    for index, expected in enumerate(schedule):
        expected_attempt_prefix.append(
            {
                "allOf": [
                    {"$ref": "#/$defs/attempt"},
                    {"$ref": f"#/$defs/candidate_{index // 4 + 1:03d}"},
                    {
                        "properties": {
                            "arm": {"const": expected["arm"]},
                            "attempt_ordinal": {
                                "const": expected["attempt_ordinal"]
                            },
                            "repetition": {"const": expected["repetition"]},
                        }
                    },
                ]
            }
        )
    expected_classification_prefix = [
        {
            "allOf": [
                {"$ref": "#/$defs/classification"},
                {"$ref": f"#/$defs/candidate_{index + 1:03d}"},
                {
                    "properties": {
                        "candidate_ordinal": {"const": index + 1}
                    }
                },
            ]
        }
        for index in range(TOTAL_CANDIDATES)
    ]
    for label, schema in (
        ("attempt", attempt_schema),
        ("classification receipt", receipt_schema),
    ):
        _require(
            schema.get("$schema") == "https://json-schema.org/draft/2020-12/schema",
            f"{label} schema dialect differs",
        )
        definitions_value = schema.get("$defs")
        _require(
            type(definitions_value) is dict,
            f"{label} schema definitions are invalid",
        )
        definitions = cast(dict[str, Any], definitions_value)
        repository_projection_value = definitions.get("repository_bindings")
        _require(
            type(repository_projection_value) is dict
            and type(repository_projection_value.get("minItems")) is int
            and repository_projection_value.get("minItems") == 3
            and type(repository_projection_value.get("maxItems")) is int
            and repository_projection_value.get("maxItems") == 3
            and repository_projection_value.get("items") is False,
            f"{label} repository schema bounds differ",
        )
        repository_projection = cast(dict[str, Any], repository_projection_value)
        projected_bindings_value = repository_projection.get("prefixItems")
        _require(
            type(projected_bindings_value) is list,
            f"{label} repository schema projection differs",
        )
        projected_bindings = cast(list[dict[str, Any]], projected_bindings_value)
        _require(
            _exact_json_equal(
                [item["allOf"][1]["const"] for item in projected_bindings],
                expected_bindings,
            ),
            f"{label} repository schema projection differs",
        )
        for index, candidate in enumerate(candidates):
            candidate_definition_value = definitions.get(f"candidate_{index + 1:03d}")
            expected_properties = {
                "candidate_ref": {"const": candidate["candidate_ref"]},
                "first_parent_position": {
                    "const": candidate["first_parent_position"]
                },
                "repository_id": {"const": candidate["repository_id"]},
                "repository_key": {"const": candidate["repository_key"]},
            }
            _require(
                type(candidate_definition_value) is dict
                and _exact_json_equal(
                    candidate_definition_value.get("properties"),
                    expected_properties,
                ),
                f"{label} candidate schema projection differs",
            )
        attempt_projection_value = definitions.get("attempts")
        _require(
            type(attempt_projection_value) is dict
            and type(attempt_projection_value.get("minItems")) is int
            and attempt_projection_value.get("minItems") == TOTAL_ATTEMPTS
            and type(attempt_projection_value.get("maxItems")) is int
            and attempt_projection_value.get("maxItems") == TOTAL_ATTEMPTS
            and attempt_projection_value.get("items") is False
            and _exact_json_equal(
                attempt_projection_value.get("prefixItems"),
                expected_attempt_prefix,
            ),
            f"{label} attempt schema projection differs",
        )
    receipt_definitions_value = receipt_schema["$defs"]
    _require(
        type(receipt_definitions_value) is dict,
        "classification receipt schema definitions are invalid",
    )
    receipt_definitions = cast(dict[str, Any], receipt_definitions_value)
    classification_projection_value = receipt_definitions.get("classifications")
    _require(
        type(classification_projection_value) is dict
        and type(classification_projection_value.get("minItems")) is int
        and classification_projection_value.get("minItems") == TOTAL_CANDIDATES
        and type(classification_projection_value.get("maxItems")) is int
        and classification_projection_value.get("maxItems") == TOTAL_CANDIDATES
        and classification_projection_value.get("items") is False
        and _exact_json_equal(
            classification_projection_value.get("prefixItems"),
            expected_classification_prefix,
        ),
        "classification result schema projection differs",
    )


def _outcome(attempt: Mapping[str, Any]) -> str:
    if attempt["status"] == "timeout":
        return "timeout"
    return "passed" if attempt["exit_code"] == 0 else "failed"


def classify_candidate(
    baseline_outcomes: Sequence[str],
    reversed_source_outcomes: Sequence[str],
) -> str:
    """Apply the frozen two-repetition truth table."""

    _require(type(baseline_outcomes) in {list, tuple}, "baseline outcomes are invalid")
    _require(
        type(reversed_source_outcomes) in {list, tuple},
        "reversed-source outcomes are invalid",
    )
    baseline = list(baseline_outcomes)
    reversed_source = list(reversed_source_outcomes)
    valid = {"passed", "failed", "timeout"}
    _require(
        len(baseline) == 2
        and len(reversed_source) == 2
        and all(type(item) is str and item in valid for item in baseline + reversed_source),
        "candidate outcome pair is invalid",
    )
    if "timeout" in baseline:
        return "baseline_invalid_timeout"
    baseline_counts = Counter(baseline)
    if baseline_counts == Counter({"failed": 2}):
        return "baseline_invalid_failure"
    if baseline_counts == Counter({"passed": 1, "failed": 1}):
        return "baseline_inconsistent"
    _require(
        baseline_counts == Counter({"passed": 2}),
        "baseline outcome pair is unclassifiable",
    )
    if "timeout" in reversed_source:
        return "reversed_invalid_timeout"
    reversed_counts = Counter(reversed_source)
    if reversed_counts == Counter({"failed": 2}):
        return "eligible_for_symptom_review"
    if reversed_counts == Counter({"passed": 1, "failed": 1}):
        return "reversed_inconsistent"
    _require(
        reversed_counts == Counter({"passed": 2}),
        "reversed-source outcome pair is unclassifiable",
    )
    return "negative_control_survived"


def _common_expected(
    *,
    plan: dict[str, Any],
    plan_raw: bytes,
    gate_manifest: dict[str, Any],
    gate_manifest_raw: bytes,
    gate_receipt: dict[str, Any],
    gate_receipt_raw: bytes,
    gate_manifest_schema_path: pathlib.Path,
    gate_receipt_schema_path: pathlib.Path,
    attempt_schema_path: pathlib.Path,
    receipt_schema_path: pathlib.Path,
    private_log_schema_path: pathlib.Path,
) -> tuple[dict[str, Any], dict[str, Any], dict[str, Any]]:
    authority = _validate_gate_dependencies(
        plan=plan,
        plan_raw=plan_raw,
        gate_manifest=gate_manifest,
        gate_manifest_raw=gate_manifest_raw,
        gate_receipt=gate_receipt,
        gate_receipt_raw=gate_receipt_raw,
        gate_manifest_schema_path=gate_manifest_schema_path,
        gate_receipt_schema_path=gate_receipt_schema_path,
    )
    implementation = _implementation_identity(
        attempt_schema_path=attempt_schema_path,
        receipt_schema_path=receipt_schema_path,
        private_log_schema_path=private_log_schema_path,
        gate_receipt_schema_path=gate_receipt_schema_path,
    )
    _validate_schema_projections(
        plan=plan,
        attempt_schema_path=attempt_schema_path,
        receipt_schema_path=receipt_schema_path,
    )
    inputs = _base_inputs(
        plan=plan,
        plan_raw=plan_raw,
        gate_manifest=gate_manifest,
        gate_manifest_raw=gate_manifest_raw,
        gate_receipt=gate_receipt,
        gate_receipt_raw=gate_receipt_raw,
        implementation=implementation,
    )
    return authority, implementation, inputs


def build_attempt_manifest(
    attempts: list[dict[str, Any]],
    *,
    plan: dict[str, Any],
    plan_raw: bytes,
    gate_manifest: dict[str, Any],
    gate_manifest_raw: bytes,
    gate_receipt: dict[str, Any],
    gate_receipt_raw: bytes,
    gate_manifest_schema_path: pathlib.Path,
    gate_receipt_schema_path: pathlib.Path,
    attempt_schema_path: pathlib.Path,
    receipt_schema_path: pathlib.Path,
    private_log_schema_path: pathlib.Path,
) -> dict[str, Any]:
    """Build a non-authoritative manifest from injected public observations."""

    authority, implementation, inputs = _common_expected(
        plan=plan,
        plan_raw=plan_raw,
        gate_manifest=gate_manifest,
        gate_manifest_raw=gate_manifest_raw,
        gate_receipt=gate_receipt,
        gate_receipt_raw=gate_receipt_raw,
        gate_manifest_schema_path=gate_manifest_schema_path,
        gate_receipt_schema_path=gate_receipt_schema_path,
        attempt_schema_path=attempt_schema_path,
        receipt_schema_path=receipt_schema_path,
        private_log_schema_path=private_log_schema_path,
    )
    normalized, summary = _validate_attempt_observations(attempts, plan=plan)
    manifest = {
        "attempts": normalized,
        "attempts_sha256": _canonical_hash(normalized),
        "authority": authority,
        "exposure": EXPOSURE,
        "implementation": implementation,
        "inputs": inputs,
        "manifest_sha256": None,
        "profile": ATTEMPT_PROFILE,
        "protocol": _protocol(plan),
        "repository_bindings": _repository_bindings(plan),
        "schema_version": SCHEMA_VERSION,
        "status": ATTEMPT_STATUS,
        "summary": summary,
    }
    manifest["manifest_sha256"] = _self_hash(manifest, "manifest_sha256")
    validate_attempt_manifest(
        manifest,
        plan=plan,
        plan_raw=plan_raw,
        gate_manifest=gate_manifest,
        gate_manifest_raw=gate_manifest_raw,
        gate_receipt=gate_receipt,
        gate_receipt_raw=gate_receipt_raw,
        gate_manifest_schema_path=gate_manifest_schema_path,
        gate_receipt_schema_path=gate_receipt_schema_path,
        attempt_schema_path=attempt_schema_path,
        receipt_schema_path=receipt_schema_path,
        private_log_schema_path=private_log_schema_path,
    )
    return manifest


def validate_attempt_manifest(
    value: dict[str, Any],
    *,
    plan: dict[str, Any],
    plan_raw: bytes,
    gate_manifest: dict[str, Any],
    gate_manifest_raw: bytes,
    gate_receipt: dict[str, Any],
    gate_receipt_raw: bytes,
    gate_manifest_schema_path: pathlib.Path,
    gate_receipt_schema_path: pathlib.Path,
    attempt_schema_path: pathlib.Path,
    receipt_schema_path: pathlib.Path,
    private_log_schema_path: pathlib.Path,
) -> None:
    _require(type(value) is dict, "attempt manifest must be an exact built-in object")
    try:
        gate._validate_json_profile(value)
    except gate.GatePrimitiveError as exc:
        raise ClassificationError(str(exc)) from exc
    expected_root = {
        "attempts",
        "attempts_sha256",
        "authority",
        "exposure",
        "implementation",
        "inputs",
        "manifest_sha256",
        "profile",
        "protocol",
        "repository_bindings",
        "schema_version",
        "status",
        "summary",
    }
    _require(set(value) == expected_root, "attempt manifest root fields differ")
    authority, implementation, inputs = _common_expected(
        plan=plan,
        plan_raw=plan_raw,
        gate_manifest=gate_manifest,
        gate_manifest_raw=gate_manifest_raw,
        gate_receipt=gate_receipt,
        gate_receipt_raw=gate_receipt_raw,
        gate_manifest_schema_path=gate_manifest_schema_path,
        gate_receipt_schema_path=gate_receipt_schema_path,
        attempt_schema_path=attempt_schema_path,
        receipt_schema_path=receipt_schema_path,
        private_log_schema_path=private_log_schema_path,
    )
    _require(
        type(value["schema_version"]) is int
        and value["schema_version"] == SCHEMA_VERSION
        and value["profile"] == ATTEMPT_PROFILE
        and value["status"] == ATTEMPT_STATUS
        and value["exposure"] == EXPOSURE,
        "attempt manifest authority profile differs",
    )
    _require(
        _exact_json_equal(value["authority"], authority),
        "attempt manifest authority differs",
    )
    _require(
        _exact_json_equal(value["inputs"], inputs),
        "attempt manifest input bindings differ",
    )
    _require(
        _exact_json_equal(value["implementation"], implementation),
        "attempt manifest implementation differs",
    )
    _require(
        _exact_json_equal(value["protocol"], _protocol(plan)),
        "attempt protocol differs",
    )
    _require(
        _exact_json_equal(
            value["repository_bindings"], _repository_bindings(plan)
        ),
        "attempt repository bindings differ",
    )
    normalized, summary = _validate_attempt_observations(value["attempts"], plan=plan)
    _require(
        _exact_json_equal(value["attempts"], normalized),
        "attempt observations differ",
    )
    _require(
        _exact_json_equal(value["summary"], summary),
        "attempt summary differs",
    )
    _require(
        _valid_sha(value["attempts_sha256"])
        and value["attempts_sha256"] == _canonical_hash(normalized),
        "attempt array hash differs",
    )
    _require(
        _valid_sha(value["manifest_sha256"])
        and value["manifest_sha256"] == _self_hash(value, "manifest_sha256"),
        "attempt manifest self hash differs",
    )
    _render(value)


def build_classification_receipt(
    attempt_manifest: dict[str, Any],
    *,
    attempt_manifest_raw: bytes,
    plan: dict[str, Any],
    plan_raw: bytes,
    gate_manifest: dict[str, Any],
    gate_manifest_raw: bytes,
    gate_receipt: dict[str, Any],
    gate_receipt_raw: bytes,
    gate_manifest_schema_path: pathlib.Path,
    gate_receipt_schema_path: pathlib.Path,
    attempt_schema_path: pathlib.Path,
    receipt_schema_path: pathlib.Path,
    private_log_schema_path: pathlib.Path,
) -> dict[str, Any]:
    """Classify one complete canonical injected-attempt manifest."""

    validate_attempt_manifest(
        attempt_manifest,
        plan=plan,
        plan_raw=plan_raw,
        gate_manifest=gate_manifest,
        gate_manifest_raw=gate_manifest_raw,
        gate_receipt=gate_receipt,
        gate_receipt_raw=gate_receipt_raw,
        gate_manifest_schema_path=gate_manifest_schema_path,
        gate_receipt_schema_path=gate_receipt_schema_path,
        attempt_schema_path=attempt_schema_path,
        receipt_schema_path=receipt_schema_path,
        private_log_schema_path=private_log_schema_path,
    )
    _require(
        type(attempt_manifest_raw) is bytes
        and len(attempt_manifest_raw) <= MAX_ARTIFACT_RAW_BYTES,
        "attempt-manifest bytes exceed the raw-byte ceiling",
    )
    _require(
        attempt_manifest_raw == _render(attempt_manifest),
        "attempt-manifest bytes are not canonical",
    )
    authority, implementation, base_inputs = _common_expected(
        plan=plan,
        plan_raw=plan_raw,
        gate_manifest=gate_manifest,
        gate_manifest_raw=gate_manifest_raw,
        gate_receipt=gate_receipt,
        gate_receipt_raw=gate_receipt_raw,
        gate_manifest_schema_path=gate_manifest_schema_path,
        gate_receipt_schema_path=gate_receipt_schema_path,
        attempt_schema_path=attempt_schema_path,
        receipt_schema_path=receipt_schema_path,
        private_log_schema_path=private_log_schema_path,
    )
    classifications, classification_counts = _derive_classifications(
        attempt_manifest["attempts"]
    )
    inputs = {
        **base_inputs,
        "attempt_manifest_file_sha256": _sha256(attempt_manifest_raw),
        "attempt_manifest_sha256": attempt_manifest["manifest_sha256"],
        "attempts_sha256": attempt_manifest["attempts_sha256"],
    }
    receipt = {
        "attempts": copy.deepcopy(attempt_manifest["attempts"]),
        "authority": authority,
        "classifications": classifications,
        "execution_status": EXECUTION_STATUS,
        "exposure": EXPOSURE,
        "implementation": implementation,
        "inputs": inputs,
        "profile": RECEIPT_PROFILE,
        "protocol": _protocol(plan),
        "receipt_sha256": None,
        "repository_bindings": _repository_bindings(plan),
        "residual_gates": list(RESIDUAL_GATES),
        "schema_version": SCHEMA_VERSION,
        "status": RECEIPT_STATUS,
        "summary": {
            **copy.deepcopy(attempt_manifest["summary"]),
            "classification_counts": classification_counts,
        },
    }
    receipt["receipt_sha256"] = _self_hash(receipt, "receipt_sha256")
    validate_classification_receipt(
        receipt,
        attempt_manifest=attempt_manifest,
        attempt_manifest_raw=attempt_manifest_raw,
        plan=plan,
        plan_raw=plan_raw,
        gate_manifest=gate_manifest,
        gate_manifest_raw=gate_manifest_raw,
        gate_receipt=gate_receipt,
        gate_receipt_raw=gate_receipt_raw,
        gate_manifest_schema_path=gate_manifest_schema_path,
        gate_receipt_schema_path=gate_receipt_schema_path,
        attempt_schema_path=attempt_schema_path,
        receipt_schema_path=receipt_schema_path,
        private_log_schema_path=private_log_schema_path,
    )
    return receipt


def validate_classification_receipt(
    value: dict[str, Any],
    *,
    attempt_manifest: dict[str, Any],
    attempt_manifest_raw: bytes,
    plan: dict[str, Any],
    plan_raw: bytes,
    gate_manifest: dict[str, Any],
    gate_manifest_raw: bytes,
    gate_receipt: dict[str, Any],
    gate_receipt_raw: bytes,
    gate_manifest_schema_path: pathlib.Path,
    gate_receipt_schema_path: pathlib.Path,
    attempt_schema_path: pathlib.Path,
    receipt_schema_path: pathlib.Path,
    private_log_schema_path: pathlib.Path,
) -> None:
    _require(
        type(value) is dict,
        "classification receipt must be an exact built-in object",
    )
    try:
        gate._validate_json_profile(value)
    except gate.GatePrimitiveError as exc:
        raise ClassificationError(str(exc)) from exc
    expected_root = {
        "attempts",
        "authority",
        "classifications",
        "execution_status",
        "exposure",
        "implementation",
        "inputs",
        "profile",
        "protocol",
        "receipt_sha256",
        "repository_bindings",
        "residual_gates",
        "schema_version",
        "status",
        "summary",
    }
    _require(set(value) == expected_root, "classification receipt root fields differ")
    validate_attempt_manifest(
        attempt_manifest,
        plan=plan,
        plan_raw=plan_raw,
        gate_manifest=gate_manifest,
        gate_manifest_raw=gate_manifest_raw,
        gate_receipt=gate_receipt,
        gate_receipt_raw=gate_receipt_raw,
        gate_manifest_schema_path=gate_manifest_schema_path,
        gate_receipt_schema_path=gate_receipt_schema_path,
        attempt_schema_path=attempt_schema_path,
        receipt_schema_path=receipt_schema_path,
        private_log_schema_path=private_log_schema_path,
    )
    _require(
        type(attempt_manifest_raw) is bytes
        and len(attempt_manifest_raw) <= MAX_ARTIFACT_RAW_BYTES
        and attempt_manifest_raw == _render(attempt_manifest),
        "attempt-manifest raw binding differs",
    )
    authority, implementation, base_inputs = _common_expected(
        plan=plan,
        plan_raw=plan_raw,
        gate_manifest=gate_manifest,
        gate_manifest_raw=gate_manifest_raw,
        gate_receipt=gate_receipt,
        gate_receipt_raw=gate_receipt_raw,
        gate_manifest_schema_path=gate_manifest_schema_path,
        gate_receipt_schema_path=gate_receipt_schema_path,
        attempt_schema_path=attempt_schema_path,
        receipt_schema_path=receipt_schema_path,
        private_log_schema_path=private_log_schema_path,
    )
    expected_inputs = {
        **base_inputs,
        "attempt_manifest_file_sha256": _sha256(attempt_manifest_raw),
        "attempt_manifest_sha256": attempt_manifest["manifest_sha256"],
        "attempts_sha256": attempt_manifest["attempts_sha256"],
    }
    _require(
        type(value["schema_version"]) is int
        and value["schema_version"] == SCHEMA_VERSION
        and value["profile"] == RECEIPT_PROFILE
        and value["status"] == RECEIPT_STATUS
        and value["execution_status"] == EXECUTION_STATUS
        and value["exposure"] == EXPOSURE,
        "classification receipt authority profile differs",
    )
    _require(
        _exact_json_equal(value["authority"], authority),
        "classification authority differs",
    )
    _require(
        _exact_json_equal(value["inputs"], expected_inputs),
        "classification input bindings differ",
    )
    _require(
        _exact_json_equal(value["implementation"], implementation),
        "classification implementation differs",
    )
    _require(
        _exact_json_equal(value["protocol"], _protocol(plan)),
        "classification protocol differs",
    )
    _require(
        _exact_json_equal(
            value["repository_bindings"], _repository_bindings(plan)
        ),
        "classification repository bindings differ",
    )
    _require(
        _exact_json_equal(value["attempts"], attempt_manifest["attempts"]),
        "classification attempts differ from the bound manifest",
    )
    classifications, counts = _derive_classifications(attempt_manifest["attempts"])
    _require(
        _exact_json_equal(value["classifications"], classifications),
        "classification rows differ from the truth table",
    )
    expected_summary = {
        **attempt_manifest["summary"],
        "classification_counts": counts,
    }
    _require(
        _exact_json_equal(value["summary"], expected_summary),
        "classification summary differs",
    )
    _require(
        _exact_json_equal(value["residual_gates"], RESIDUAL_GATES),
        "classification residual gates differ",
    )
    _require(
        _valid_sha(value["receipt_sha256"])
        and value["receipt_sha256"] == _self_hash(value, "receipt_sha256"),
        "classification receipt self hash differs",
    )
    _render(value)


def _write_atomic(path: pathlib.Path, value: dict[str, Any]) -> None:
    descriptor: int | None = None
    temporary: pathlib.Path | None = None
    try:
        path.parent.mkdir(parents=True, exist_ok=True)
        rendered = _render(value)
        descriptor, temporary_name = tempfile.mkstemp(
            prefix=f".{path.name}.", dir=path.parent
        )
        temporary = pathlib.Path(temporary_name)
        os.fchmod(descriptor, 0o644)
        handle = os.fdopen(descriptor, "wb")
        descriptor = None
        with handle:
            handle.write(rendered)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
    except OSError as exc:
        raise ClassificationError("classification receipt publication failed") from exc
    finally:
        if descriptor is not None:
            try:
                os.close(descriptor)
            except OSError:
                pass
        if temporary is not None:
            try:
                temporary.unlink(missing_ok=True)
            except OSError:
                pass


def _common_cli_arguments(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--plan", required=True, type=pathlib.Path)
    parser.add_argument("--gate-manifest", required=True, type=pathlib.Path)
    parser.add_argument("--gate-receipt", required=True, type=pathlib.Path)
    parser.add_argument("--gate-manifest-schema", required=True, type=pathlib.Path)
    parser.add_argument("--gate-receipt-schema", required=True, type=pathlib.Path)
    parser.add_argument("--attempt-schema", required=True, type=pathlib.Path)
    parser.add_argument("--receipt-schema", required=True, type=pathlib.Path)
    parser.add_argument("--private-log-schema", required=True, type=pathlib.Path)
    parser.add_argument("--attempt-manifest", required=True, type=pathlib.Path)


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="command", required=True)
    classify = subparsers.add_parser(
        "classify",
        help="classify a complete injected, unattested attempt manifest",
    )
    _common_cli_arguments(classify)
    classify.add_argument("--output", required=True, type=pathlib.Path)
    check = subparsers.add_parser(
        "check-receipt",
        help="check a canonical classification receipt and all exact public inputs",
    )
    check.add_argument("receipt", type=pathlib.Path)
    _common_cli_arguments(check)
    args = parser.parse_args(argv)
    try:
        plan, plan_raw = _read_json(
            args.plan,
            max_raw_bytes=gate.MAX_PLAN_RAW_BYTES,
            label="run plan",
        )
        gate_manifest, gate_manifest_raw = _read_json(
            args.gate_manifest,
            max_raw_bytes=gate.MAX_MANIFEST_RAW_BYTES,
            label="gate manifest",
        )
        gate_receipt, gate_receipt_raw = _read_json(
            args.gate_receipt,
            max_raw_bytes=gate.MAX_RECEIPT_RAW_BYTES,
            label="gate receipt",
        )
        attempt_manifest, attempt_manifest_raw = _read_json(
            args.attempt_manifest,
            max_raw_bytes=MAX_ARTIFACT_RAW_BYTES,
            label="attempt manifest",
        )
        common = {
            "attempt_manifest": attempt_manifest,
            "attempt_manifest_raw": attempt_manifest_raw,
            "plan": plan,
            "plan_raw": plan_raw,
            "gate_manifest": gate_manifest,
            "gate_manifest_raw": gate_manifest_raw,
            "gate_receipt": gate_receipt,
            "gate_receipt_raw": gate_receipt_raw,
            "gate_manifest_schema_path": args.gate_manifest_schema,
            "gate_receipt_schema_path": args.gate_receipt_schema,
            "attempt_schema_path": args.attempt_schema,
            "receipt_schema_path": args.receipt_schema,
            "private_log_schema_path": args.private_log_schema,
        }
        if args.command == "classify":
            receipt = build_classification_receipt(**common)
            _write_atomic(args.output, receipt)
        else:
            receipt, receipt_raw = _read_json(
                args.receipt,
                max_raw_bytes=MAX_ARTIFACT_RAW_BYTES,
                label="classification receipt",
            )
            _require(
                receipt_raw == _render(receipt),
                "classification receipt bytes are not canonical",
            )
            validate_classification_receipt(receipt, **common)
    except ClassificationError:
        parser.error("negative-control classification validation failed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
