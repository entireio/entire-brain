#!/usr/bin/env python3
"""Compile and check the unexecutable negative-control execution contract.

This module projects the exact frozen v2 plan and its three eligibility
ledgers into a deterministic future-executor contract.  It is deliberately
build/check-only: it has no subprocess, Git, Go, worktree, candidate, network,
model/provider, private-log-write, host-observation, reservation, or approval
surface.  A valid contract still forbids execution and leaves every runtime
binding null.
"""

from __future__ import annotations

import argparse
import copy
import hashlib
import json
import os
import pathlib
import stat
import tempfile
from collections.abc import Sequence
from typing import Any, cast

import draft202012


PROFILE = "agent_brain_negative_control_execution_contract_v1"
SCHEMA_VERSION = 1
STATUS = "contract_compiled_execution_forbidden"
EXECUTION_STATUS = (
    "forbidden_missing_all_residual_gates_and_audited_execution_adapter"
)
EXPOSURE = "permanent_development_only_identity_inspected"
ROOT = pathlib.Path(__file__).parent
REPOSITORY_ORDER = ("entire-brain", "entire-db", "entire-graph")
EXPECTED_COUNTS = {"entire-brain": 18, "entire-db": 25, "entire-graph": 19}
TOTAL_CANDIDATES = 62
ARM_ORDER = ("baseline", "first_parent_source_reversal")
REPETITIONS = (1, 2)
TOTAL_ATTEMPTS = TOTAL_CANDIDATES * len(ARM_ORDER) * len(REPETITIONS)
MAX_ARTIFACT_RAW_BYTES = 8 * 1024 * 1024
MAX_COMPONENT_RAW_BYTES = 4 * 1024 * 1024
MAX_JSON_DEPTH = 64
MAX_JSON_NODES = 2_000_000
MAX_INTEGER_DIGITS = 64
MAX_INTEGER = 10**MAX_INTEGER_DIGITS - 1
CANONICAL_JSON_PROFILE = "sorted_keys_compact_utf8_no_float_v1"
ARTIFACT_RENDER_PROFILE = "sorted_keys_indent_2_utf8_lf_v1"
PLAN_PROFILE = "agent_brain_development_task_negative_control_run_plan_v2"
PLAN_STATUS = "pending_owner_authorization"
ELIGIBILITY_PROFILE = "agent_brain_development_task_eligibility_scan_v2"
CHECKED_PLAN_SHA256 = "a47311fa1f4553f8085ebea6e8ddd003a4ea5d0b2d269d91bbcdd414134a248e"
CHECKED_PLAN_FILE_SHA256 = "f55b6e22a25daf8016305304adf3b7ac8d31675281d97cfc9bb78cc76b619790"
CHECKED_CONTRACT_SCHEMA_SHA256 = "dbcb95119feeda4c436e90d8162ac0498577454ca9ccaab7a8254d55d90359f9"
CHECKED_CANDIDATE_BINDINGS_SHA256 = "4fbde9876d4c91e9cedcf994de9cecd5442beee928c1cadc505116a25c2bef60"
CHECKED_SCHEDULE_SHA256 = "c818e8ad87f4d6ff5fac8839f4296fd86da13eaf95dadf8b2c789c7254bfdbcd"

AUTHORITY = {
    "approval_receipt_sha256": None,
    "approval_trust_mechanism": "absent_not_implemented",
    "approval_trust_root_sha256": None,
    "benchmark_execution": "forbidden_contract_unexecutable",
    "candidate_execution": "forbidden_contract_unexecutable",
    "model_provider_execution": "forbidden_not_authorized",
    "owner_key": "absent_not_fabricated",
    "paid_execution": "forbidden_not_authorized",
    "population_assignment": "absent_not_authorized",
    "source_plan_status": PLAN_STATUS,
}

RUNTIME_BINDINGS = {
    "actual_cache_archive_sha256": None,
    "actual_cache_manifest_sha256": None,
    "approval_receipt_sha256": None,
    "approval_trust_root_sha256": None,
    "capacity_observation_file_sha256": None,
    "capacity_reservation_receipt_sha256": None,
    "cleanup_attestation_sha256": None,
    "execution_host_identity_sha256": None,
    "private_log_root_locator": None,
    "producer_attestation_key_sha256": None,
}

PROTOCOL = {
    "arm_order": list(ARM_ORDER),
    "candidate_order": "repository_order_then_first_parent_position_serial_v1",
    "go_test_internal_timeout": "disabled_outer_process_group_deadline_required",
    "max_concurrency": 1,
    "max_total_wall_seconds": 28_800,
    "outer_timeout_seconds_per_arm": 600,
    "repetitions_per_arm": 2,
    "repository_order": list(REPOSITORY_ORDER),
    "reversal": {
        "full_commit_revert": "forbidden",
        "merge_parent_selection": "first_parent_only",
        "non_production_proof": (
            "exact_candidate_tree_entries_unchanged_and_no_untracked_paths"
        ),
        "parent_binding": "candidate_parent_oid",
        "production_path_proof": (
            "exact_parent_and_candidate_tree_mode_type_blob_or_absence_per_bound_path"
        ),
        "source_scope": "production_go_paths_from_exact_ledger_identity_only",
        "test_command_parity": (
            "identical_test_target_and_direct_argv_binding_across_both_arms"
        ),
        "test_evidence_mutation": "forbidden",
    },
    "total_attempt_count": TOTAL_ATTEMPTS,
    "total_candidate_count": TOTAL_CANDIDATES,
}

STATE_MACHINE = {
    "aggregate_accounting": {
        "counters": [
            "elapsed_wall_nanoseconds",
            "staging_bytes",
            "cache_bytes",
            "private_log_bytes",
            "public_receipt_bytes",
        ],
        "rule": "durably_update_before_advancing_schedule_cursor",
    },
    "attempt_journal": {
        "allowed_transitions": [
            {"from": "attempt_bound", "to": ["attempt_staged", "attempt_aborting"]},
            {"from": "attempt_staged", "to": ["attempt_running", "attempt_aborting"]},
            {"from": "attempt_running", "to": ["attempt_result_private", "attempt_aborting"]},
            {"from": "attempt_result_private", "to": ["attempt_cleaning", "attempt_aborting"]},
            {"from": "attempt_cleaning", "to": ["attempt_cleanup_committed", "cleanup_failed_latched"]},
            {"from": "attempt_aborting", "to": ["attempt_cleaning", "cleanup_failed_latched"]},
        ],
        "binding_rule": "bind_exact_schedule_row_at_run_cursor_plus_one",
        "initial_state": "attempt_bound",
        "interruption_rule": (
            "attempt_cleanup_or_post_cleanup_pre_cursor_interruption_recovers_"
            "under_the_irreversible_run_receipt_forbidden_latch_without_rerun_or_receipt"
        ),
        "receipt_rule": "attempt_terminal_never_publishes_aggregate_receipt",
        "success_terminal_state": "attempt_cleanup_committed",
    },
    "journal_rule": "durable_transition_before_and_after_each_external_effect",
    "latched_cleanup_failure_rule": (
        "blocks_every_future_run_until_independently_verified_remediation_and_"
        "reservation_release_evidence_without_ever_permitting_failed_run_receipt"
    ),
    "restart_recovery_rule": (
        "under_one_exclusive_run_lock_recover_nonterminal_journal_cleanup_and_"
        "reservation_release_before_new_run_or_receipt"
    ),
    "run_journal": {
        "abort_latch": (
            "executor_staging_attestation_log_journal_cleanup_failure_or_external_"
            "interruption_detected_before_aggregate_receipt_commit_durably_sets_"
            "receipt_forbidden_latch_but_captured_test_exit_or_timeout_is_data"
        ),
        "allowed_transitions": [
            {
                "from": "contract_checked_execution_forbidden",
                "to": ["authority_and_inputs_verified", "run_aborting"],
            },
            {"from": "authority_and_inputs_verified", "to": ["capacity_reserved", "run_aborting"]},
            {"from": "capacity_reserved", "to": ["attempts_active", "run_aborting"]},
            {"from": "attempts_active", "to": ["attempts_active", "run_cleaning", "run_aborting"]},
            {
                "from": "run_cleaning",
                "to": [
                    "reservation_released",
                    "run_cleaning_no_receipt",
                    "cleanup_failed_latched",
                ],
            },
            {
                "from": "reservation_released",
                "to": [
                    "aggregate_receipt_committed",
                    "reservation_released_no_receipt",
                ],
            },
            {"from": "run_aborting", "to": ["run_cleaning_no_receipt", "cleanup_failed_latched"]},
            {"from": "run_cleaning_no_receipt", "to": ["reservation_released_no_receipt", "cleanup_failed_latched"]},
            {"from": "reservation_released_no_receipt", "to": ["cleaned_no_receipt"]},
        ],
        "initial_state": "contract_checked_execution_forbidden",
        "interruption_targets": {
            "authority_and_inputs_verified": "run_aborting",
            "capacity_reserved": "run_aborting",
            "contract_checked_execution_forbidden": "run_aborting",
            "attempts_active": "run_aborting",
            "reservation_released": "reservation_released_no_receipt",
            "run_cleaning": "run_cleaning_no_receipt",
        },
        "receipt_commit_boundary": (
            "atomic_durable_aggregate_receipt_commit_is_the_success_boundary_and_"
            "has_no_outgoing_transition"
        ),
        "receipt_forbidden_latch": (
            "persistent_monotonic_false_to_true_on_any_pre_receipt_abort_or_"
            "interruption_and_true_forbids_aggregate_receipt_commit"
        ),
        "restart_interruption_rule": (
            "any_recovery_of_a_nonterminal_started_run_is_an_interruption_and_"
            "must_durably_set_receipt_forbidden_latch_before_recovery_effects"
        ),
        "schedule_cursor_initial": 0,
        "schedule_cursor_rule": (
            "monotonic_plus_one_only_after_matching_attempt_cleanup_committed"
        ),
        "success_guard": (
            "run_cleaning_requires_schedule_cursor_248_all_aggregate_ceilings_"
            "passed_and_receipt_forbidden_latch_false"
        ),
        "success_terminal_state": "aggregate_receipt_committed",
        "terminal_states": [
            "aggregate_receipt_committed",
            "cleaned_no_receipt",
            "cleanup_failed_latched",
        ],
    },
    "scope": "run_journal_with_nested_per_schedule_row_attempt_journal_v1",
}

ENFORCEMENT_REQUIREMENTS = [
    "authenticated_owner_approval_bound_to_exact_contract_before_any_external_effect",
    "actual_cache_archive_safely_traversed_and_content_verified_before_staging",
    "trusted_apfs_observation_and_atomic_bounded_capacity_reservation",
    "clean_detached_worktree_per_arm_repetition_from_exact_candidate_commit",
    "exact_commit_parent_tree_and_first_parent_relationship_reverified",
    "first_parent_production_source_only_reversal_with_test_evidence_unchanged",
    "exact_git_tree_mode_type_blob_or_absence_proof_for_every_reversed_source_path",
    "all_non_production_tree_entries_unchanged_no_untracked_paths_and_identical_test_argv",
    "fresh_private_verified_offline_cache_copy_per_arm_repetition",
    "ambient_go_cache_and_network_dependency_resolution_forbidden",
    "direct_argv_exact_pinned_binary_execution_without_shell_interpretation",
    "dedicated_process_group_with_outer_deadline_and_descendant_termination",
    "private_content_addressed_log_publication_before_public_receipt_projection",
    "attempt_producer_binds_exact_schedule_row_reversal_proof_and_test_command",
    "aggregate_time_staging_cache_log_and_receipt_ceilings_enforced",
    "durable_cleanup_journal_and_capacity_release_on_success_failure_or_interrupt",
    "irreversible_pre_commit_no_receipt_latch_and_atomic_receipt_commit_boundary",
    "aggregate_receipt_only_after_all_248_attempt_cleanups_and_final_reservation_release",
    "no_public_receipt_when_cleanup_or_attestation_is_incomplete",
]

RESIDUAL_GATES = [
    "owner_execution_approval_receipt_and_trust_mechanism",
    "actual_approved_offline_cache_seed_archive_and_authorized_binding",
    "safe_archive_traversal_type_link_device_and_content_verifier",
    "trusted_apfs_observer_and_atomic_capacity_reservation",
    "clean_detached_worktree_executor_and_first_parent_reversal_proof",
    "attested_attempt_producer_and_classifier_integration",
    "private_log_executor_integration_retention_and_aggregate_accounting",
    "fail_closed_cleanup_interruption_attestation_and_no_receipt_guarantee",
]

COMPONENT_FILES = (
    ("gate_primitive", "task_negative_control_gate_v1.py"),
    ("gate_cache_manifest_schema", "schemas/offline-go-cache-seed-manifest-v1.schema.json"),
    ("gate_receipt_schema", "schemas/negative-control-gate-primitive-receipt-v1.schema.json"),
    ("private_log_writer", "negative_control_private_log.py"),
    ("private_log_receipt_schema", "schemas/negative-control-private-log-receipt-v1.schema.json"),
    ("attempt_classifier", "task_negative_control_classification_v1.py"),
    ("attempt_observation_schema", "schemas/negative-control-attempt-observations-v1.schema.json"),
    ("classification_receipt_schema", "schemas/negative-control-classification-receipt-v1.schema.json"),
    ("darwin_capacity_observer", "negative_control_darwin_capacity_v1.swift"),
    ("darwin_capacity_schema", "schemas/negative-control-darwin-capacity-observation-v1.schema.json"),
    ("offline_schema_validator", "draft202012.py"),
)


class ExecutionContractError(ValueError):
    """Raised when the execution contract or one of its exact inputs differs."""


def _require(condition: bool, message: str) -> None:
    if not condition:
        raise ExecutionContractError(message)


def _sha256(raw: bytes) -> str:
    _require(type(raw) is bytes, "SHA-256 input must be exact built-in bytes")
    return hashlib.sha256(raw).hexdigest()


def _validate_json_profile(value: Any) -> None:
    stack: list[tuple[Any, int]] = [(value, 1)]
    containers: set[int] = set()
    nodes = 0
    while stack:
        current, depth = stack.pop()
        nodes += 1
        _require(nodes <= MAX_JSON_NODES, "JSON exceeds the node ceiling")
        _require(depth <= MAX_JSON_DEPTH, "JSON exceeds the depth ceiling")
        current_type = type(current)
        _require(
            current_type in {dict, list, str, int, bool, type(None)},
            "canonical contract JSON requires exact built-in JSON value types",
        )
        if current_type is int:
            _require(
                -MAX_INTEGER <= cast(int, current) <= MAX_INTEGER,
                "JSON integer exceeds the digit ceiling",
            )
        if current_type in {dict, list}:
            identity = id(current)
            _require(
                identity not in containers,
                "JSON contains a cycle or repeated container alias",
            )
            containers.add(identity)
        if current_type is dict:
            mapping = cast(dict[Any, Any], current)
            _require(
                all(type(key) is str for key in mapping),
                "canonical contract JSON requires exact built-in string keys",
            )
            stack.extend((child, depth + 1) for child in mapping.values())
        elif current_type is list:
            stack.extend((child, depth + 1) for child in cast(list[Any], current))


def _canonical_bytes(value: Any) -> bytes:
    _validate_json_profile(value)
    try:
        return json.dumps(
            value,
            allow_nan=False,
            ensure_ascii=False,
            separators=(",", ":"),
            sort_keys=True,
        ).encode("utf-8")
    except (TypeError, ValueError, UnicodeError) as exc:
        raise ExecutionContractError(f"value is not canonical JSON: {exc}") from exc


def _canonical_hash(value: Any) -> str:
    return _sha256(_canonical_bytes(value))


def _render(value: dict[str, Any]) -> bytes:
    _validate_json_profile(value)
    try:
        raw = (
            json.dumps(
                value,
                allow_nan=False,
                ensure_ascii=False,
                indent=2,
                sort_keys=True,
            )
            + "\n"
        ).encode("utf-8")
    except (TypeError, ValueError, UnicodeError) as exc:
        raise ExecutionContractError(f"value is not renderable JSON: {exc}") from exc
    _require(
        len(raw) <= MAX_ARTIFACT_RAW_BYTES,
        "execution contract exceeds the raw-byte ceiling",
    )
    return raw


def _self_hash(value: dict[str, Any]) -> str:
    projected = copy.deepcopy(value)
    _require("contract_sha256" in projected, "contract_sha256 is missing")
    projected["contract_sha256"] = None
    return _canonical_hash(projected)


def _field_self_hash(value: dict[str, Any], field: str) -> str:
    projected = copy.deepcopy(value)
    _require(field in projected, f"{field} is missing")
    projected[field] = None
    return _canonical_hash(projected)


def _read_bounded(
    path: pathlib.Path,
    *,
    maximum: int,
    label: str,
) -> bytes:
    _require(type(maximum) is int and maximum >= 1, f"{label} byte ceiling is invalid")
    _require(path.anchor in ("", "/"), f"{label} path anchor is unsupported")
    components = path.parts[1:] if path.is_absolute() else path.parts
    _require(bool(components), f"{label} path has no file component")
    _require(
        all(
            component not in ("", ".", "..") and "\0" not in component
            for component in components
        ),
        f"{label} path contains a forbidden traversal component",
    )
    _require(
        os.open in os.supports_dir_fd,
        "descriptor-relative secure open is unavailable",
    )

    file_flags = os.O_RDONLY
    directory_flags = os.O_RDONLY
    for flag_name in ("O_CLOEXEC", "O_NOFOLLOW", "O_NONBLOCK"):
        flag = getattr(os, flag_name, None)
        _require(type(flag) is int and flag != 0, f"secure open flag {flag_name} is unavailable")
        file_flags |= cast(int, flag)
        directory_flags |= cast(int, flag)
    directory_flag = getattr(os, "O_DIRECTORY", None)
    _require(
        type(directory_flag) is int and directory_flag != 0,
        "secure open flag O_DIRECTORY is unavailable",
    )
    directory_flags |= cast(int, directory_flag)

    directory_descriptors: list[int] = []
    descriptor: int | None = None
    try:
        root = "/" if path.is_absolute() else "."
        directory_descriptor = os.open(root, directory_flags)
        directory_descriptors.append(directory_descriptor)
        for component in components[:-1]:
            directory_descriptor = os.open(
                component,
                directory_flags,
                dir_fd=directory_descriptor,
            )
            directory_descriptors.append(directory_descriptor)
            _require(
                stat.S_ISDIR(os.fstat(directory_descriptor).st_mode),
                f"{label} ancestor is not a directory",
            )
        descriptor = os.open(
            components[-1],
            file_flags,
            dir_fd=directory_descriptor,
        )
        before = os.fstat(descriptor)
        _require(stat.S_ISREG(before.st_mode), f"{label} is not a regular file")
        _require(before.st_size <= maximum, f"{label} exceeds the byte ceiling")
        chunks: list[bytes] = []
        remaining = maximum + 1
        while remaining:
            chunk = os.read(descriptor, min(1024 * 1024, remaining))
            if not chunk:
                break
            chunks.append(chunk)
            remaining -= len(chunk)
        raw = b"".join(chunks)
        after = os.fstat(descriptor)
        identity_before = (
            before.st_dev,
            before.st_ino,
            before.st_mode,
            before.st_uid,
            before.st_gid,
            before.st_nlink,
            before.st_size,
            before.st_mtime_ns,
            before.st_ctime_ns,
        )
        identity_after = (
            after.st_dev,
            after.st_ino,
            after.st_mode,
            after.st_uid,
            after.st_gid,
            after.st_nlink,
            after.st_size,
            after.st_mtime_ns,
            after.st_ctime_ns,
        )
        _require(
            len(raw) <= maximum
            and len(raw) == before.st_size
            and identity_before == identity_after,
            f"{label} changed while being read",
        )
        return raw
    except ExecutionContractError:
        raise
    except OSError as exc:
        raise ExecutionContractError(f"cannot read {label}: {exc}") from exc
    finally:
        if descriptor is not None:
            os.close(descriptor)
        for directory_descriptor in reversed(directory_descriptors):
            os.close(directory_descriptor)


def _reject_duplicate_pairs(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        _require(key not in result, "JSON contains a duplicate object key")
        result[key] = value
    return result


def _parse_integer(text: str) -> int:
    _require(
        len(text.removeprefix("-")) <= MAX_INTEGER_DIGITS,
        "JSON integer exceeds the digit ceiling",
    )
    return int(text)


def _reject_float(_text: str) -> float:
    raise ExecutionContractError("canonical contract JSON forbids floating-point values")


def _reject_constant(_text: str) -> None:
    raise ExecutionContractError("canonical contract JSON forbids non-finite values")


def _read_json(
    path: pathlib.Path,
    *,
    maximum: int = MAX_ARTIFACT_RAW_BYTES,
    label: str,
) -> tuple[dict[str, Any], bytes]:
    raw = _read_bounded(path, maximum=maximum, label=label)
    try:
        value = json.loads(
            raw,
            object_pairs_hook=_reject_duplicate_pairs,
            parse_constant=_reject_constant,
            parse_float=_reject_float,
            parse_int=_parse_integer,
        )
        _validate_json_profile(value)
    except ExecutionContractError:
        raise
    except (UnicodeError, json.JSONDecodeError, ValueError, RecursionError) as exc:
        raise ExecutionContractError(f"cannot parse {label}: {exc}") from exc
    _require(type(value) is dict, "JSON root must be an object")
    return cast(dict[str, Any], value), raw


def _read_bytes(path: pathlib.Path, *, label: str) -> bytes:
    return _read_bounded(path, maximum=MAX_COMPONENT_RAW_BYTES, label=label)


def _valid_sha(value: Any, *, length: int = 64) -> bool:
    return (
        type(value) is str
        and len(cast(str, value)) == length
        and all(character in "0123456789abcdef" for character in cast(str, value))
        and value != "0" * length
    )


def _artifact_file(path: pathlib.Path) -> str:
    name = path.name
    _require(
        bool(name)
        and pathlib.PurePosixPath(name).name == name
        and name not in {".", ".."},
        "artifact file name is invalid",
    )
    return name


def _component_bindings() -> list[dict[str, Any]]:
    bindings: list[dict[str, Any]] = []
    for role, relative in COMPONENT_FILES:
        path = ROOT / relative
        bindings.append(
            {
                "artifact_file": path.name,
                "artifact_sha256": _sha256(
                    _read_bytes(path, label=f"{role} component")
                ),
                "role": role,
            }
        )
    return bindings


def _implementation(schema_raw: bytes) -> dict[str, Any]:
    builder = _read_bytes(pathlib.Path(__file__), label="execution-contract builder")
    return {
        "artifact_render_profile": ARTIFACT_RENDER_PROFILE,
        "builder_sha256": _sha256(builder),
        "canonical_json_profile": CANONICAL_JSON_PROFILE,
        "schema_sha256": _sha256(schema_raw),
    }


def _validate_schema_snapshot(
    value: dict[str, Any],
    *,
    schema: dict[str, Any],
    schema_name: str,
) -> None:
    try:
        validator = draft202012.Validator(
            [draft202012.SchemaDocument(schema_name, schema)]
        )
        validator.validate(value, schema_name, label="execution contract")
    except draft202012.SchemaError as exc:
        raise ExecutionContractError(
            f"execution contract schema validation failed: {exc}"
        ) from exc


def _load_contract_schema(
    path: pathlib.Path,
) -> tuple[dict[str, Any], bytes]:
    _require(
        _artifact_file(path) == "negative-control-execution-contract-v1.schema.json",
        "execution-contract schema file name differs",
    )
    schema, raw = _read_json(
        path,
        maximum=MAX_COMPONENT_RAW_BYTES,
        label="execution-contract schema",
    )
    _require(
        _sha256(raw) == CHECKED_CONTRACT_SCHEMA_SHA256,
        "execution-contract schema differs from the checked v1 schema",
    )
    return schema, raw


def _candidate_binding(
    candidate: dict[str, Any],
    *,
    ordinal: int,
    repository_key: str,
    repository_id: str,
) -> dict[str, Any]:
    targets = candidate["test_targets"]
    return {
        "candidate_ordinal": ordinal,
        "candidate_ref": candidate["candidate_ref"],
        "commit_oid": candidate["commit_oid"],
        "first_parent_position": candidate["first_parent_position"],
        "full_diff_sha256": candidate["full_diff_sha256"],
        "full_stable_patch_id": candidate["full_stable_patch_id"],
        "merge_parent_count": candidate["merge_parent_count"],
        "module_bindings_sha256": _canonical_hash(candidate["module_bindings"]),
        "parent_oid": candidate["parent_oid"],
        "private_execution_projection_sha256": None,
        "production_go_paths_sha256": candidate["production_go_paths_sha256"],
        "repository_id": repository_id,
        "repository_key": repository_key,
        "source_diff_sha256": candidate["source_diff_sha256"],
        "source_stable_patch_id": candidate["source_stable_patch_id"],
        "test_command_sha256": None,
        "test_diff_sha256": candidate["test_diff_sha256"],
        "test_evidence_paths_sha256": candidate["test_evidence_paths_sha256"],
        "test_stable_patch_id": candidate["test_stable_patch_id"],
        "test_target_bindings_sha256": _canonical_hash(targets),
        "test_target_count": len(targets),
        "tree_oid": candidate["tree_oid"],
        "unit_kind": candidate["unit_kind"],
    }


def _candidate_ref(repository_id: str, commit_oid: str) -> str:
    return _sha256(
        b"entire-brain/task-eligibility-v2/candidate\0"
        + repository_id.encode("utf-8")
        + b"\0"
        + commit_oid.encode("ascii")
    )


def _schedule(candidates: Sequence[dict[str, Any]]) -> list[dict[str, Any]]:
    result: list[dict[str, Any]] = []
    attempt_ordinal = 1
    for candidate in candidates:
        for arm in ARM_ORDER:
            for repetition in REPETITIONS:
                result.append(
                    {
                        "arm": arm,
                        "attempt_ordinal": attempt_ordinal,
                        "candidate_ordinal": candidate["candidate_ordinal"],
                        "candidate_ref": candidate["candidate_ref"],
                        "first_parent_position": candidate["first_parent_position"],
                        "repetition": repetition,
                        "repository_id": candidate["repository_id"],
                        "repository_key": candidate["repository_key"],
                    }
                )
                attempt_ordinal += 1
    _require(len(result) == TOTAL_ATTEMPTS, "attempt schedule count differs")
    return result


def _verify_bound_file(
    path: pathlib.Path,
    *,
    expected_file: str,
    expected_sha256: str,
    label: str,
    maximum: int = MAX_COMPONENT_RAW_BYTES,
) -> bytes:
    _require(_artifact_file(path) == expected_file, f"{label} file name differs")
    raw = _read_bounded(path, maximum=maximum, label=label)
    _require(_sha256(raw) == expected_sha256, f"{label} raw hash differs")
    return raw


def _load_checked_plan(path: pathlib.Path) -> tuple[dict[str, Any], bytes]:
    plan, raw = _read_json(
        path,
        maximum=MAX_ARTIFACT_RAW_BYTES,
        label="run plan",
    )
    _require(raw == _render(plan), "run-plan bytes are not canonical")
    _require(
        _sha256(raw) == CHECKED_PLAN_FILE_SHA256
        and plan.get("plan_sha256") == CHECKED_PLAN_SHA256
        and plan.get("plan_sha256") == _field_self_hash(plan, "plan_sha256")
        and plan.get("profile") == PLAN_PROFILE
        and plan.get("schema_version") == 2
        and plan.get("status") == PLAN_STATUS
        and plan.get("exposure") == EXPOSURE,
        "run plan differs from the checked frozen v2 plan",
    )
    return plan, raw


def _load_inputs(
    *,
    contract_path: pathlib.Path,
    eligibility_schema_path: pathlib.Path,
    ledger_paths: Sequence[pathlib.Path],
    plan_path: pathlib.Path,
    plan_schema_path: pathlib.Path,
    registry_path: pathlib.Path,
    registry_schema_path: pathlib.Path,
) -> tuple[dict[str, Any], bytes, list[tuple[dict[str, Any], bytes, pathlib.Path]]]:
    _require(len(ledger_paths) == 3, "exactly three eligibility ledgers are required")
    plan, plan_raw = _load_checked_plan(plan_path)
    dependencies = plan["dependencies"]
    repository_contract = dependencies["repository_contract"]
    eligibility_schema = dependencies["eligibility_schema"]
    eligibility_scanner = dependencies["eligibility_scanner"]
    overlap_registry = dependencies["overlap_registry"]
    _require(type(overlap_registry) is dict, "run-plan overlap registry is absent")

    _verify_bound_file(
        contract_path,
        expected_file=repository_contract["artifact_file"],
        expected_sha256=repository_contract["artifact_sha256"],
        label="repository contract",
        maximum=MAX_ARTIFACT_RAW_BYTES,
    )
    _verify_bound_file(
        eligibility_schema_path,
        expected_file=eligibility_schema["artifact_file"],
        expected_sha256=eligibility_schema["artifact_sha256"],
        label="eligibility schema",
        maximum=MAX_ARTIFACT_RAW_BYTES,
    )
    _verify_bound_file(
        ROOT / eligibility_scanner["artifact_file"],
        expected_file=eligibility_scanner["artifact_file"],
        expected_sha256=eligibility_scanner["artifact_sha256"],
        label="eligibility scanner",
    )
    _verify_bound_file(
        plan_schema_path,
        expected_file="development-task-negative-control-run-plan-v2.schema.json",
        expected_sha256=plan["implementation"]["schema_sha256"],
        label="run-plan schema",
        maximum=MAX_ARTIFACT_RAW_BYTES,
    )
    _verify_bound_file(
        ROOT / "task_negative_control_plan_v2.py",
        expected_file="task_negative_control_plan_v2.py",
        expected_sha256=plan["implementation"]["builder_sha256"],
        label="run-plan builder",
    )
    _verify_bound_file(
        registry_path,
        expected_file=overlap_registry["artifact_file"],
        expected_sha256=overlap_registry["artifact_sha256"],
        label="overlap registry",
        maximum=MAX_ARTIFACT_RAW_BYTES,
    )
    _verify_bound_file(
        registry_schema_path,
        expected_file=overlap_registry["schema_file"],
        expected_sha256=overlap_registry["schema_sha256"],
        label="overlap-registry schema",
        maximum=MAX_ARTIFACT_RAW_BYTES,
    )
    for source_file, hash_field, label in (
        ("task_overlap_registry.py", "builder_sha256", "overlap-registry builder"),
        ("task_eligibility.py", "cli_scanner_sha256", "CLI eligibility scanner"),
        ("task_population.py", "cli_task_population_sha256", "CLI population helper"),
    ):
        _verify_bound_file(
            ROOT / source_file,
            expected_file=source_file,
            expected_sha256=overlap_registry[hash_field],
            label=label,
        )

    plan_repositories_value = plan["repositories"]
    _require(
        type(plan_repositories_value) is list
        and len(plan_repositories_value) == 3
        and all(type(item) is dict for item in plan_repositories_value),
        "run-plan repository projection is malformed",
    )
    plan_repositories = cast(list[dict[str, Any]], plan_repositories_value)
    _require(
        [item["key"] for item in plan_repositories] == list(REPOSITORY_ORDER)
        and [item["candidate_count"] for item in plan_repositories]
        == [EXPECTED_COUNTS[key] for key in REPOSITORY_ORDER],
        "run-plan repository projection differs",
    )
    repository_by_ledger_hash = {
        item["ledger_file_sha256"]: item for item in plan_repositories
    }
    _require(
        len(repository_by_ledger_hash) == 3,
        "run-plan ledger hashes collide",
    )

    ledgers_by_key: dict[str, tuple[dict[str, Any], bytes, pathlib.Path]] = {}
    for path in ledger_paths:
        ledger, raw = _read_json(path, label="eligibility ledger")
        _require(raw == _render(ledger), "eligibility ledger bytes are not canonical")
        plan_repository = repository_by_ledger_hash.get(_sha256(raw))
        _require(plan_repository is not None, "eligibility ledger raw hash differs from plan")
        plan_repository = cast(dict[str, Any], plan_repository)
        _require(
            _artifact_file(path) == plan_repository["ledger_file"],
            "eligibility ledger file name differs from plan",
        )
        _require(
            ledger.get("ledger_sha256") == plan_repository["ledger_sha256"]
            and ledger.get("ledger_sha256") == _field_self_hash(ledger, "ledger_sha256")
            and ledger.get("profile") == ELIGIBILITY_PROFILE
            and ledger.get("schema_version") == 2
            and ledger.get("exposure") == EXPOSURE,
            "eligibility ledger identity differs",
        )
        repository = ledger["repository"]
        key = repository["key"]
        _require(
            type(key) is str
            and key in REPOSITORY_ORDER
            and key == plan_repository["key"]
            and repository["repository_id"] == plan_repository["repository_id"]
            and repository["window"]["base_oid"] == plan_repository["base_oid"]
            and repository["window"]["head_oid"] == plan_repository["head_oid"],
            "eligibility ledger repository is unplanned",
        )
        _require(key not in ledgers_by_key, "duplicate eligibility ledger repository")
        candidates_value = ledger["candidates"]
        _require(
            ledger["execution_status"] == "not_executed"
            and type(candidates_value) is list
            and len(candidates_value) == EXPECTED_COUNTS[cast(str, key)]
            and all(type(candidate) is dict for candidate in candidates_value),
            "eligibility ledger contains malformed candidates",
        )
        candidates = cast(list[dict[str, Any]], candidates_value)
        _require(
            all(
                candidate["negative_control_status"] == "not_executed"
                and candidate["runner_readiness"] == "structurally_ready_not_executed"
                for candidate in candidates
            ),
            "eligibility ledger contains execution or a blocked candidate",
        )
        ledgers_by_key[cast(str, key)] = (ledger, raw, path)
    _require(
        set(ledgers_by_key) == set(REPOSITORY_ORDER),
        "eligibility ledger repository set differs",
    )
    ordered = [ledgers_by_key[key] for key in REPOSITORY_ORDER]
    return plan, plan_raw, ordered


def build_contract(
    *,
    contract_path: pathlib.Path,
    contract_schema_path: pathlib.Path,
    eligibility_schema_path: pathlib.Path,
    ledger_paths: Sequence[pathlib.Path],
    plan_path: pathlib.Path,
    plan_schema_path: pathlib.Path,
    registry_path: pathlib.Path,
    registry_schema_path: pathlib.Path,
) -> dict[str, Any]:
    contract_schema, contract_schema_raw = _load_contract_schema(
        contract_schema_path
    )
    plan, plan_raw, ledgers = _load_inputs(
        contract_path=contract_path,
        eligibility_schema_path=eligibility_schema_path,
        ledger_paths=ledger_paths,
        plan_path=plan_path,
        plan_schema_path=plan_schema_path,
        registry_path=registry_path,
        registry_schema_path=registry_schema_path,
    )
    candidates: list[dict[str, Any]] = []
    repository_bindings: list[dict[str, Any]] = []
    ledger_inputs: list[dict[str, Any]] = []
    ordinal = 1
    for plan_repository, (ledger, raw, path) in zip(
        plan["repositories"], ledgers, strict=True
    ):
        repository = ledger["repository"]
        key = repository["key"]
        _require(
            key == plan_repository["key"]
            and repository["repository_id"] == plan_repository["repository_id"],
            "ledger and run-plan repository bindings differ",
        )
        _require(
            _sha256(raw) == plan_repository["ledger_file_sha256"]
            and ledger["ledger_sha256"] == plan_repository["ledger_sha256"],
            "ledger raw or self hash differs from the checked run plan",
        )
        order = [
            {
                "candidate_ref": candidate["candidate_ref"],
                "first_parent_position": candidate["first_parent_position"],
            }
            for candidate in ledger["candidates"]
        ]
        _require(
            order == plan_repository["candidate_order"],
            "ledger and run-plan candidate order differ",
        )
        repository_bindings.append(
            {
                "base_oid": plan_repository["base_oid"],
                "candidate_count": plan_repository["candidate_count"],
                "candidate_order_sha256": plan_repository["candidate_order_sha256"],
                "head_oid": plan_repository["head_oid"],
                "key": key,
                "ledger_file_sha256": _sha256(raw),
                "ledger_sha256": ledger["ledger_sha256"],
                "repository_id": repository["repository_id"],
                "toolchain_sha256": _canonical_hash(repository["toolchain"]),
            }
        )
        ledger_inputs.append(
            {
                "artifact_file": _artifact_file(path),
                "artifact_sha256": _sha256(raw),
                "candidate_count": len(ledger["candidates"]),
                "ledger_sha256": ledger["ledger_sha256"],
                "profile": ledger["profile"],
                "repository_id": repository["repository_id"],
                "repository_key": key,
                "schema_version": ledger["schema_version"],
            }
        )
        for candidate in ledger["candidates"]:
            candidates.append(
                _candidate_binding(
                    candidate,
                    ordinal=ordinal,
                    repository_key=key,
                    repository_id=repository["repository_id"],
                )
            )
            ordinal += 1
    _require(ordinal == TOTAL_CANDIDATES + 1, "candidate binding count differs")
    schedule = _schedule(candidates)
    contract = {
        "authority": copy.deepcopy(AUTHORITY),
        "candidate_bindings_sha256": _canonical_hash(candidates),
        "candidates": candidates,
        "component_bindings": _component_bindings(),
        "contract_sha256": None,
        "enforcement_requirements": list(ENFORCEMENT_REQUIREMENTS),
        "execution_status": EXECUTION_STATUS,
        "exposure": EXPOSURE,
        "implementation": _implementation(contract_schema_raw),
        "inputs": {
            "eligibility_schema": copy.deepcopy(plan["dependencies"]["eligibility_schema"]),
            "eligibility_scanner": copy.deepcopy(plan["dependencies"]["eligibility_scanner"]),
            "ledgers": ledger_inputs,
            "overlap_registry": copy.deepcopy(plan["dependencies"]["overlap_registry"]),
            "repository_contract": copy.deepcopy(plan["dependencies"]["repository_contract"]),
            "run_plan": {
                "artifact_file": _artifact_file(plan_path),
                "artifact_sha256": _sha256(plan_raw),
                "builder_sha256": plan["implementation"]["builder_sha256"],
                "plan_sha256": plan["plan_sha256"],
                "profile": plan["profile"],
                "schema_sha256": plan["implementation"]["schema_sha256"],
                "schema_version": plan["schema_version"],
            },
        },
        "profile": PROFILE,
        "protocol": copy.deepcopy(PROTOCOL),
        "repository_bindings": repository_bindings,
        "residual_gates": list(RESIDUAL_GATES),
        "runtime_bindings": copy.deepcopy(RUNTIME_BINDINGS),
        "schedule": schedule,
        "schedule_sha256": _canonical_hash(schedule),
        "schema_version": SCHEMA_VERSION,
        "state_machine": copy.deepcopy(STATE_MACHINE),
        "status": STATUS,
    }
    contract["contract_sha256"] = _self_hash(contract)
    _validate_contract_snapshot(
        contract,
        schema=contract_schema,
        schema_raw=contract_schema_raw,
        schema_name=_artifact_file(contract_schema_path),
    )
    return contract


def _validate_contract_snapshot(
    value: dict[str, Any],
    *,
    schema: dict[str, Any],
    schema_raw: bytes,
    schema_name: str,
) -> None:
    expected_root = {
        "authority",
        "candidate_bindings_sha256",
        "candidates",
        "component_bindings",
        "contract_sha256",
        "enforcement_requirements",
        "execution_status",
        "exposure",
        "implementation",
        "inputs",
        "profile",
        "protocol",
        "repository_bindings",
        "residual_gates",
        "runtime_bindings",
        "schedule",
        "schedule_sha256",
        "schema_version",
        "state_machine",
        "status",
    }
    _require(type(value) is dict and set(value) == expected_root, "contract root fields differ")
    _require(
        value["schema_version"] == SCHEMA_VERSION and value["profile"] == PROFILE,
        "contract profile differs",
    )
    _require(value["status"] == STATUS, "contract status differs")
    _require(value["execution_status"] == EXECUTION_STATUS, "execution status differs")
    _require(value["exposure"] == EXPOSURE, "contract exposure differs")
    _require(value["authority"] == AUTHORITY, "contract authority boundary differs")
    _require(value["runtime_bindings"] == RUNTIME_BINDINGS, "runtime bindings must remain null")
    _require(value["protocol"] == PROTOCOL, "contract protocol differs")
    _require(value["state_machine"] == STATE_MACHINE, "contract state machine differs")
    _require(
        value["enforcement_requirements"] == ENFORCEMENT_REQUIREMENTS,
        "enforcement requirements differ",
    )
    _require(value["residual_gates"] == RESIDUAL_GATES, "residual gates differ")
    _require(_valid_sha(value["contract_sha256"]), "contract_sha256 is invalid")
    _require(value["contract_sha256"] == _self_hash(value), "contract self hash mismatch")

    candidates_value = value["candidates"]
    _require(type(candidates_value) is list, "candidates must be an exact built-in array")
    candidates = cast(list[dict[str, Any]], candidates_value)
    _require(len(candidates) == TOTAL_CANDIDATES, "candidate count is not exactly 62")
    expected_candidate_fields = {
        "candidate_ordinal",
        "candidate_ref",
        "commit_oid",
        "first_parent_position",
        "full_diff_sha256",
        "full_stable_patch_id",
        "merge_parent_count",
        "module_bindings_sha256",
        "parent_oid",
        "private_execution_projection_sha256",
        "production_go_paths_sha256",
        "repository_id",
        "repository_key",
        "source_diff_sha256",
        "source_stable_patch_id",
        "test_command_sha256",
        "test_diff_sha256",
        "test_evidence_paths_sha256",
        "test_stable_patch_id",
        "test_target_bindings_sha256",
        "test_target_count",
        "tree_oid",
        "unit_kind",
    }
    refs: set[str] = set()
    for index, candidate in enumerate(candidates, start=1):
        _require(
            type(candidate) is dict and set(candidate) == expected_candidate_fields,
            f"candidate[{index - 1}] fields differ",
        )
        _require(
            type(candidate["candidate_ordinal"]) is int
            and candidate["candidate_ordinal"] == index,
            f"candidate[{index - 1}] ordinal differs",
        )
        _require(_valid_sha(candidate["candidate_ref"]), f"candidate[{index - 1}] reference is invalid")
        _require(candidate["candidate_ref"] not in refs, "candidate references collide")
        refs.add(candidate["candidate_ref"])
        for field in ("commit_oid", "parent_oid", "tree_oid", "full_stable_patch_id", "source_stable_patch_id", "test_stable_patch_id"):
            _require(_valid_sha(candidate[field], length=40), f"candidate[{index - 1}].{field} is invalid")
        for field in (
            "full_diff_sha256",
            "module_bindings_sha256",
            "production_go_paths_sha256",
            "source_diff_sha256",
            "test_diff_sha256",
            "test_evidence_paths_sha256",
            "test_target_bindings_sha256",
        ):
            _require(_valid_sha(candidate[field]), f"candidate[{index - 1}].{field} is invalid")
        _require(
            candidate["private_execution_projection_sha256"] is None
            and candidate["test_command_sha256"] is None,
            f"candidate[{index - 1}] contains a runtime execution binding",
        )
        _require(
            type(candidate["merge_parent_count"]) is int
            and candidate["merge_parent_count"] in {1, 2}
            and type(candidate["first_parent_position"]) is int
            and 1 <= candidate["first_parent_position"] <= 31
            and type(candidate["test_target_count"]) is int
            and candidate["test_target_count"] >= 1,
            f"candidate[{index - 1}] counts are invalid",
        )
        expected_unit_kind = (
            "single_parent_integration_unit"
            if candidate["merge_parent_count"] == 1
            else "two_parent_feature_branch"
        )
        _require(
            candidate["unit_kind"] == expected_unit_kind,
            f"candidate[{index - 1}] unit kind differs from parent count",
        )
        _require(
            type(candidate["repository_id"]) is str
            and type(candidate["repository_key"]) is str
            and candidate["repository_key"] in REPOSITORY_ORDER
            and candidate["candidate_ref"]
            == _candidate_ref(candidate["repository_id"], candidate["commit_oid"]),
            f"candidate[{index - 1}] repository or derived reference differs",
        )
    _require(
        value["candidate_bindings_sha256"] == CHECKED_CANDIDATE_BINDINGS_SHA256
        and value["candidate_bindings_sha256"] == _canonical_hash(candidates),
        "candidate binding hash differs",
    )

    repositories = value["repository_bindings"]
    _require(type(repositories) is list and len(repositories) == 3, "repository bindings differ")
    repository_fields = {
        "base_oid",
        "candidate_count",
        "candidate_order_sha256",
        "head_oid",
        "key",
        "ledger_file_sha256",
        "ledger_sha256",
        "repository_id",
        "toolchain_sha256",
    }
    candidate_offset = 0
    for repository_index, item in enumerate(repositories):
        _require(
            type(item) is dict and set(item) == repository_fields,
            f"repository[{repository_index}] fields differ",
        )
        repository = cast(dict[str, Any], item)
        key = REPOSITORY_ORDER[repository_index]
        expected_count = EXPECTED_COUNTS[key]
        _require(
            repository["key"] == key
            and type(repository["candidate_count"]) is int
            and repository["candidate_count"] == expected_count
            and type(repository["repository_id"]) is str
            and repository["repository_id"].startswith("github.com/"),
            f"repository[{repository_index}] identity or count differs",
        )
        for field in ("base_oid", "head_oid"):
            _require(
                _valid_sha(repository[field], length=40),
                f"repository[{repository_index}].{field} is invalid",
            )
        for field in (
            "candidate_order_sha256",
            "ledger_file_sha256",
            "ledger_sha256",
            "toolchain_sha256",
        ):
            _require(
                _valid_sha(repository[field]),
                f"repository[{repository_index}].{field} is invalid",
            )
        repository_candidates = candidates[
            candidate_offset : candidate_offset + expected_count
        ]
        _require(
            len(repository_candidates) == expected_count
            and all(
                candidate["repository_key"] == key
                and candidate["repository_id"] == repository["repository_id"]
                for candidate in repository_candidates
            ),
            f"repository[{repository_index}] candidate membership differs",
        )
        positions = [
            candidate["first_parent_position"]
            for candidate in repository_candidates
        ]
        _require(
            positions == sorted(set(positions)),
            f"repository[{repository_index}] candidate positions differ",
        )
        order = [
            {
                "candidate_ref": candidate["candidate_ref"],
                "first_parent_position": candidate["first_parent_position"],
            }
            for candidate in repository_candidates
        ]
        _require(
            repository["candidate_order_sha256"] == _canonical_hash(order),
            f"repository[{repository_index}] candidate order hash differs",
        )
        candidate_offset += expected_count
    _require(candidate_offset == TOTAL_CANDIDATES, "repository candidate total differs")

    inputs = value["inputs"]
    expected_input_fields = {
        "eligibility_schema",
        "eligibility_scanner",
        "ledgers",
        "overlap_registry",
        "repository_contract",
        "run_plan",
    }
    _require(
        type(inputs) is dict and set(inputs) == expected_input_fields,
        "contract input fields differ",
    )
    run_plan_input = inputs["run_plan"]
    _require(
        type(run_plan_input) is dict
        and set(run_plan_input)
        == {
            "artifact_file",
            "artifact_sha256",
            "builder_sha256",
            "plan_sha256",
            "profile",
            "schema_sha256",
            "schema_version",
        }
        and run_plan_input["artifact_file"]
        == "development-task-negative-control-run-plan-v2.json"
        and run_plan_input["artifact_sha256"] == CHECKED_PLAN_FILE_SHA256
        and run_plan_input["plan_sha256"] == CHECKED_PLAN_SHA256
        and run_plan_input["profile"] == PLAN_PROFILE
        and run_plan_input["schema_version"] == 2
        and _valid_sha(run_plan_input["builder_sha256"])
        and _valid_sha(run_plan_input["schema_sha256"]),
        "run-plan input binding differs",
    )
    ledger_inputs_value = inputs["ledgers"]
    _require(
        type(ledger_inputs_value) is list and len(ledger_inputs_value) == 3,
        "ledger input bindings differ",
    )
    ledger_inputs = cast(list[Any], ledger_inputs_value)
    ledger_fields = {
        "artifact_file",
        "artifact_sha256",
        "candidate_count",
        "ledger_sha256",
        "profile",
        "repository_id",
        "repository_key",
        "schema_version",
    }
    for index, ledger_value in enumerate(ledger_inputs):
        _require(
            type(ledger_value) is dict and set(ledger_value) == ledger_fields,
            f"ledger input[{index}] fields differ",
        )
        ledger = cast(dict[str, Any], ledger_value)
        repository = cast(dict[str, Any], repositories[index])
        _require(
            ledger["repository_key"] == repository["key"]
            and ledger["repository_id"] == repository["repository_id"]
            and ledger["candidate_count"] == repository["candidate_count"]
            and ledger["artifact_sha256"] == repository["ledger_file_sha256"]
            and ledger["ledger_sha256"] == repository["ledger_sha256"]
            and ledger["profile"] == ELIGIBILITY_PROFILE
            and ledger["schema_version"] == 2
            and type(ledger["artifact_file"]) is str
            and pathlib.PurePosixPath(ledger["artifact_file"]).name
            == ledger["artifact_file"],
            f"ledger input[{index}] differs from repository binding",
        )

    checked_plan, checked_plan_raw = _load_checked_plan(
        ROOT / "development-task-negative-control-run-plan-v2.json"
    )
    _require(
        inputs["eligibility_schema"]
        == checked_plan["dependencies"]["eligibility_schema"]
        and inputs["eligibility_scanner"]
        == checked_plan["dependencies"]["eligibility_scanner"]
        and inputs["overlap_registry"]
        == checked_plan["dependencies"]["overlap_registry"]
        and inputs["repository_contract"]
        == checked_plan["dependencies"]["repository_contract"],
        "checked run-plan dependency projection differs",
    )
    _require(
        run_plan_input
        == {
            "artifact_file": "development-task-negative-control-run-plan-v2.json",
            "artifact_sha256": _sha256(checked_plan_raw),
            "builder_sha256": checked_plan["implementation"]["builder_sha256"],
            "plan_sha256": checked_plan["plan_sha256"],
            "profile": checked_plan["profile"],
            "schema_sha256": checked_plan["implementation"]["schema_sha256"],
            "schema_version": checked_plan["schema_version"],
        },
        "checked run-plan implementation projection differs",
    )
    checked_repositories = cast(
        list[dict[str, Any]], checked_plan["repositories"]
    )
    expected_repositories = [
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
        for repository in checked_repositories
    ]
    _require(
        repositories == expected_repositories,
        "repository bindings differ from the checked run plan",
    )
    _require(
        all(
            ledger_inputs[index]["artifact_file"]
            == checked_repositories[index]["ledger_file"]
            for index in range(3)
        ),
        "ledger input file names differ from the checked run plan",
    )

    schedule_value = value["schedule"]
    _require(type(schedule_value) is list, "schedule must be an exact built-in array")
    schedule = cast(list[dict[str, Any]], schedule_value)
    _require(schedule == _schedule(candidates), "schedule differs from candidate bindings")
    _require(
        value["schedule_sha256"] == CHECKED_SCHEDULE_SHA256
        and value["schedule_sha256"] == _canonical_hash(schedule),
        "schedule hash differs",
    )

    components = value["component_bindings"]
    _require(type(components) is list and len(components) == len(COMPONENT_FILES), "component bindings differ")
    _require(
        [item.get("role") if type(item) is dict else None for item in components]
        == [role for role, _ in COMPONENT_FILES],
        "component binding order differs",
    )
    for index, component in enumerate(components):
        expected_role, expected_relative = COMPONENT_FILES[index]
        _require(
            type(component) is dict
            and set(component) == {"artifact_file", "artifact_sha256", "role"}
            and component["role"] == expected_role
            and component["artifact_file"] == pathlib.Path(expected_relative).name
            and _valid_sha(component["artifact_sha256"]),
            f"component[{index}] binding is invalid",
        )
    _require(
        components == _component_bindings(),
        "component bindings differ from current exact files",
    )

    implementation = value["implementation"]
    _require(
        type(implementation) is dict
        and set(implementation)
        == {"artifact_render_profile", "builder_sha256", "canonical_json_profile", "schema_sha256"},
        "implementation fields differ",
    )
    _require(
        implementation["artifact_render_profile"] == ARTIFACT_RENDER_PROFILE
        and implementation["canonical_json_profile"] == CANONICAL_JSON_PROFILE
        and implementation["builder_sha256"]
        == _sha256(_read_bytes(pathlib.Path(__file__), label="execution-contract builder"))
        and implementation["schema_sha256"] == _sha256(schema_raw),
        "implementation identity is invalid",
    )
    _validate_schema_snapshot(value, schema=schema, schema_name=schema_name)


def validate_contract(
    value: dict[str, Any],
    *,
    contract_schema_path: pathlib.Path,
) -> None:
    schema, schema_raw = _load_contract_schema(contract_schema_path)
    _validate_contract_snapshot(
        value,
        schema=schema,
        schema_raw=schema_raw,
        schema_name=_artifact_file(contract_schema_path),
    )


def verify_contract_dependencies(
    value: dict[str, Any],
    *,
    contract_path: pathlib.Path,
    contract_schema_path: pathlib.Path,
    eligibility_schema_path: pathlib.Path,
    ledger_paths: Sequence[pathlib.Path],
    plan_path: pathlib.Path,
    plan_schema_path: pathlib.Path,
    registry_path: pathlib.Path,
    registry_schema_path: pathlib.Path,
) -> None:
    validate_contract(value, contract_schema_path=contract_schema_path)
    rebuilt = build_contract(
        contract_path=contract_path,
        contract_schema_path=contract_schema_path,
        eligibility_schema_path=eligibility_schema_path,
        ledger_paths=ledger_paths,
        plan_path=plan_path,
        plan_schema_path=plan_schema_path,
        registry_path=registry_path,
        registry_schema_path=registry_schema_path,
    )
    _require(value == rebuilt, "execution contract differs from exact dependency rebuild")


def _write_atomic(path: pathlib.Path, value: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temporary_name = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent)
    temporary = pathlib.Path(temporary_name)
    try:
        os.fchmod(descriptor, 0o644)
        with os.fdopen(descriptor, "wb") as handle:
            handle.write(_render(value))
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
    finally:
        temporary.unlink(missing_ok=True)


def _common_arguments(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--contract", required=True, type=pathlib.Path)
    parser.add_argument("--contract-schema", required=True, type=pathlib.Path)
    parser.add_argument("--eligibility-schema", required=True, type=pathlib.Path)
    parser.add_argument("--ledger", required=True, action="append", type=pathlib.Path)
    parser.add_argument("--plan", required=True, type=pathlib.Path)
    parser.add_argument("--plan-schema", required=True, type=pathlib.Path)
    parser.add_argument("--registry", required=True, type=pathlib.Path)
    parser.add_argument("--registry-schema", required=True, type=pathlib.Path)


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="command", required=True)
    build = subparsers.add_parser("build", help="build the unexecutable contract")
    _common_arguments(build)
    build.add_argument("--output", required=True, type=pathlib.Path)
    check = subparsers.add_parser("check", help="check exact contract bytes and dependencies")
    check.add_argument("artifact", type=pathlib.Path)
    _common_arguments(check)
    args = parser.parse_args(argv)
    kwargs = {
        "contract_path": args.contract,
        "contract_schema_path": args.contract_schema,
        "eligibility_schema_path": args.eligibility_schema,
        "ledger_paths": args.ledger,
        "plan_path": args.plan,
        "plan_schema_path": args.plan_schema,
        "registry_path": args.registry,
        "registry_schema_path": args.registry_schema,
    }
    try:
        if args.command == "build":
            _write_atomic(args.output, build_contract(**kwargs))
        else:
            artifact, raw = _read_json(args.artifact, label="execution contract")
            _require(raw == _render(artifact), "execution-contract bytes are not canonical")
            verify_contract_dependencies(artifact, **kwargs)
    except ExecutionContractError as exc:
        parser.error(str(exc))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
