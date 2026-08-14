#!/usr/bin/env python3
"""Build and check the non-executing negative-control run-storage contract.

This module freezes dependency identities and the storage/recovery protocol
that a future audited adapter must implement.  It deliberately exposes no
approval-consumption, filesystem-root, journal, reservation, staging,
cleanup, worktree, candidate, subprocess, network, model, or paid-execution
operation.  ``build`` may atomically write only the caller-selected contract
output; ``check`` reads only fixed repository artifacts.
"""

from __future__ import annotations

import argparse
import copy
import hashlib
import json
import os
import pathlib
import stat
import sys
import tempfile
import types
from collections.abc import Mapping, Sequence
from typing import Any, cast


ROOT = pathlib.Path(__file__).parent
SCHEMA_ROOT = ROOT / "schemas"
PROFILE = "agent_brain_negative_control_run_storage_contract_v1"
SCHEMA_VERSION = 1
STATUS = "run_storage_contract_compiled_runtime_unbound_execution_forbidden"
EXECUTION_STATUS = "forbidden_contract_only_no_atomic_consumption_no_storage_adapter"
EXPOSURE = "permanent_development_only_identity_inspected"
CANONICAL_JSON_PROFILE = "sorted_keys_compact_utf8_no_float_v1"
ARTIFACT_RENDER_PROFILE = "sorted_keys_indent_2_utf8_lf_v1"

MAX_SOURCE_BYTES = 4 * 1024 * 1024
MAX_JSON_BYTES = 16 * 1024 * 1024
MAX_SCHEMA_BYTES = 4 * 1024 * 1024
READ_CHUNK_BYTES = 1024 * 1024

CONTRACT_PATH = ROOT / "negative-control-run-storage-contract-v1.json"
CONTRACT_SCHEMA_PATH = SCHEMA_ROOT / "negative-control-run-storage-contract-v1.schema.json"
JOURNAL_SCHEMA_PATH = SCHEMA_ROOT / "negative-control-run-journal-event-v1.schema.json"
RESERVATION_SCHEMA_PATH = SCHEMA_ROOT / "negative-control-capacity-reservation-receipt-v1.schema.json"
CLEANUP_SCHEMA_PATH = SCHEMA_ROOT / "negative-control-cleanup-attestation-v1.schema.json"

CACHE_SOURCE_PATH = ROOT / "negative_control_cache_bundle_v2.py"
E0_PATH = ROOT / "development-task-negative-control-execution-contract-v1.json"
E0_SCHEMA_PATH = SCHEMA_ROOT / "negative-control-execution-contract-v1.schema.json"
E0_SOURCE_PATH = ROOT / "negative_control_execution_contract_v1.py"
PLAN_PATH = ROOT / "development-task-negative-control-run-plan-v2.json"
PLAN_SCHEMA_PATH = SCHEMA_ROOT / "development-task-negative-control-run-plan-v2.schema.json"
PLAN_SOURCE_PATH = ROOT / "task_negative_control_plan_v2.py"
OWNER_PATH = ROOT / "negative-control-owner-approval-verifier-contract-v1.json"
OWNER_SCHEMA_PATH = SCHEMA_ROOT / "negative-control-owner-approval-verifier-contract-v1.schema.json"
OWNER_SOURCE_PATH = ROOT / "negative_control_owner_approval_v1.py"
TRUST_ROOTS_PATH = ROOT / "negative-control-owner-approval-trust-roots-v1.json"
TRUST_ROOTS_SCHEMA_PATH = SCHEMA_ROOT / "negative-control-owner-approval-trust-roots-v1.schema.json"
APPROVAL_SCHEMA_PATH = SCHEMA_ROOT / "negative-control-owner-approval-envelope-v1.schema.json"
APPROVAL_REPORT_SCHEMA_PATH = SCHEMA_ROOT / "negative-control-owner-approval-verification-report-v1.schema.json"

CAPACITY_SOURCE_PATH = ROOT / "negative_control_darwin_capacity_v1.swift"
CAPACITY_SCHEMA_PATH = SCHEMA_ROOT / "negative-control-darwin-capacity-observation-v1.schema.json"
GATE_SOURCE_PATH = ROOT / "task_negative_control_gate_v1.py"
GATE_SCHEMA_PATH = SCHEMA_ROOT / "negative-control-gate-primitive-receipt-v1.schema.json"
PRIVATE_LOG_SOURCE_PATH = ROOT / "negative_control_private_log.py"
PRIVATE_LOG_SCHEMA_PATH = SCHEMA_ROOT / "negative-control-private-log-receipt-v1.schema.json"
CLASSIFIER_SOURCE_PATH = ROOT / "task_negative_control_classification_v1.py"
ATTEMPT_SCHEMA_PATH = SCHEMA_ROOT / "negative-control-attempt-observations-v1.schema.json"
CLASSIFICATION_SCHEMA_PATH = SCHEMA_ROOT / "negative-control-classification-receipt-v1.schema.json"
DRAFT_SOURCE_PATH = ROOT / "draft202012.py"

CHECKED_CACHE_SOURCE_SHA256 = "1236f3244be5d25cb72b7937aab159903a20cb97c2ce057023e4c4694f248bfc"
CHECKED_CACHE_SOURCE_SIZE = 54_632
CHECKED_E0_RAW_SHA256 = "94422af8a5e24c6a855b66cf40868565ee3518d0c75eb5343b5d2450523b2c1e"
CHECKED_E0_SHA256 = "1e290b91d5e0be42f750bf0e655edee48d297f86ad3e3867b8fe9177fd5125ef"
CHECKED_E0_SOURCE_SHA256 = "f3cfb82978b0ce98d17999ef4cb4f4089a425bb528ece4ff53b61a03a4344bcd"
CHECKED_E0_SCHEMA_SHA256 = "dbcb95119feeda4c436e90d8162ac0498577454ca9ccaab7a8254d55d90359f9"
CHECKED_E0_SOURCE_COMMIT = "f0552070921605cd9a0165ee29ca96e100675d58"
CHECKED_E0_CANDIDATES_SHA256 = "4fbde9876d4c91e9cedcf994de9cecd5442beee928c1cadc505116a25c2bef60"
CHECKED_E0_SCHEDULE_SHA256 = "c818e8ad87f4d6ff5fac8839f4296fd86da13eaf95dadf8b2c789c7254bfdbcd"
CHECKED_E0_AUTHORITY_SHA256 = "422d6742432af499f9aec5e6b7ecc9d86ac0cf901e2accd0fedb611c580176de"
CHECKED_E0_PROTOCOL_SHA256 = "3e5cef5c19836b61f5739347f4d0db25564b859cd26fc6c5761f81072b2f3f01"
CHECKED_E0_STATE_MACHINE_SHA256 = "31015d9150afaf6c73cbea84dc647039449cc5bfc498ac0efedddfabad271207"
CHECKED_E0_ENFORCEMENT_SHA256 = "c23f32b102077254ef4eae322faa12101e3e24d688808ebf6bca31cf128185e2"
CHECKED_E0_RESIDUAL_SHA256 = "650f570c5fe096626381171dd6d9d2f0ca77c4a6c13e6994c783ee3a82d5cb91"
CHECKED_E0_RUNTIME_SHA256 = "938f7a76faf1d0a3490db06fbb5ac5b1100f2967a24b0bc846ce40c3c762b634"

CHECKED_PLAN_RAW_SHA256 = "f55b6e22a25daf8016305304adf3b7ac8d31675281d97cfc9bb78cc76b619790"
CHECKED_PLAN_SHA256 = "a47311fa1f4553f8085ebea6e8ddd003a4ea5d0b2d269d91bbcdd414134a248e"
CHECKED_PLAN_SOURCE_SHA256 = "23d08e9d3d23935cdc86b53780b609fb35f6b3020ede6b1360af288939e34715"
CHECKED_PLAN_SCHEMA_SHA256 = "294f77f165676bec6053e9637578ca22f048a887d2a978bbf8b8e300f21a2dad"
CHECKED_RESOURCE_BUDGET_SHA256 = "ebd6d63bc9334270dbf0052e9c7477d1206973c678e7355ffa0b400d2d8ff512"
CHECKED_TOOLCHAINS_SHA256 = "e8240c5af77530079643593a7484cb0062408829bab76690b7f55d12176fc87c"

CHECKED_OWNER_RAW_SHA256 = "d19ec3df382070c743e2b466175c661d91d03001d15c0de561d9e700df6c5569"
CHECKED_OWNER_SHA256 = "ed22928cfe42e29ba57f3c58fab62eecebe9fd1bee010a2708fe02773ec35093"
CHECKED_OWNER_SOURCE_SHA256 = "569045e1c15366f93500fa030c8fcb1214c7c6a6e912a8b5499a32690e484119"
CHECKED_OWNER_SCHEMA_SHA256 = "e3b8e95e62cda252434e11bf190e4f248526486ba3c1f63415ce78d78f2b5707"
CHECKED_TRUST_RAW_SHA256 = "07b515dddf74c53872c01f69cf2b1076820a10782dd9102cf235cf9691d89f1b"
CHECKED_TRUST_SHA256 = "b3374aff490518f3f1eb142428810bb7791aeaf5a6a05cf16f63455d02908b37"
CHECKED_TRUST_SCHEMA_SHA256 = "54dd9ccd634863cfbc050221d2394b54326e03a6e465ebf807e358113f37158d"
CHECKED_APPROVAL_SCHEMA_SHA256 = "1300806efed4cedfccbbd68291676f71eabfc1296dbbf054a721e18a20a4d675"
CHECKED_APPROVAL_REPORT_SCHEMA_SHA256 = "352ceeb16bc398c4241c0c3347b021a08c4ea16948f28d9d34e28965257e1594"

CHECKED_BUNDLE_RAW_SHA256 = "29d66a79f4c3083e006c823b44a5aeacb50538d426f0186e05fffa77226760ee"
CHECKED_BUNDLE_SHA256 = "98de7291aceb5a84ff03d1862e66550fb4d67a55796634eacc4e610856829bdd"
CHECKED_BUNDLE_SCHEMA_SHA256 = "eca24ad3f0a50f4a9dcbd9e106715db8fee449d4735eaeee87912a95e7c564bd"
CHECKED_BUNDLE_REPORT_SCHEMA_SHA256 = "3afd8fec9da54a8b7358b497031633bfeaf8c5df1f595347424769d4a30ec4ad"
CHECKED_FORMAT_RAW_SHA256 = "970669197065a563fa0a09c281f671f5a3cd63061d6ff8f52da64926fa5ac406"
CHECKED_FORMAT_SHA256 = "e2231e7fae7dfdad054c770d928b549bb996b4c452169cb22abea7f8f6f0ee59"
CHECKED_FORMAT_SCHEMA_SHA256 = "95712ba76c36107f583a195167e6dea9d90ba81729fba10fa211096d277617d8"
CHECKED_MATERIAL_RAW_SHA256 = "7527cec7b2f1b47498845ae38710369997b7c82d2d0150761b23d2b594f68d93"
CHECKED_MATERIAL_SHA256 = "f03ae113a29585a9aba0c8ce57a41ab61eb0b2cd682474f1df2d214e299a4852"
CHECKED_MATERIAL_SCHEMA_SHA256 = "1424cd22b5eb057b5794347403780435f95c0428d4f81ff42441e71abc39d5d5"

CHECKED_CAPACITY_SOURCE_SHA256 = "e1402fa9ef2b25d13dc125af6581f33c21a163c138accaee78fcbe6b51f619b6"
CHECKED_CAPACITY_SCHEMA_SHA256 = "1a2a529336258c9cb5c6da512d65981c03c2a9407934d5e699ce60a1b2a70086"
CHECKED_GATE_SOURCE_SHA256 = "f04b8634caf619cc17bb495975d2b5358164be6eae2a124ade84efebd60c51dd"
CHECKED_GATE_SCHEMA_SHA256 = "7603f755dd8429ff8c0a17114a50cda0aa23ae99c07cf2cfff9856df00c0a8e0"
CHECKED_PRIVATE_LOG_SOURCE_SHA256 = "2a88ef48cc52d52e477b145c06bd0a55d66cc487570aa766cb4da23897121216"
CHECKED_PRIVATE_LOG_SCHEMA_SHA256 = "6b3378bb3a2bf8a687459aafb3ceefac963995897bca1d4f97cb961fd7b9abf8"
CHECKED_CLASSIFIER_SOURCE_SHA256 = "5b8cad62c72e06e1cfae99630e98d06b1fe78b0660825f495085c6f89c5fd147"
CHECKED_ATTEMPT_SCHEMA_SHA256 = "d5a036e22ded76cc485fc62742a4f85d93ff6d228063ffc2c014fa13725b53ca"
CHECKED_CLASSIFICATION_SCHEMA_SHA256 = "c025b1fde9dba21b1b139aa7f9e5ae5f9bc1912aa61bf33921fe62c07aefcca0"
CHECKED_DRAFT_SOURCE_SHA256 = "8688b67468b096427f758163174432e221d8197c6b13870a6427939f6ea281eb"
CHECKED_DRAFT_SOURCE_SIZE = 18_097

# These three future evidence schemas are frozen in this slice.  The contract
# schema hash is filled after its concurrently-authored shape is finalized.
CHECKED_JOURNAL_SCHEMA_SHA256 = "8eceb818bb1bb6fedb7a3d0bc8f9911d1305c5f43dad5b02ea3b18f6d7f64d5f"
CHECKED_RESERVATION_SCHEMA_SHA256 = "fb0c5cba1f72184f1192bf17b115636259940fe573e02676b4d2e4598e6b9645"
CHECKED_CLEANUP_SCHEMA_SHA256 = "81ae0a8aa8ceb91d664c25117507150442bc4f9288bf17419286d49ac58f812a"
CHECKED_CONTRACT_SCHEMA_SHA256 = "0036e470d0dcf5563877db9bed68a7d679e88dcc9169c016692b1f1d756f975e"

AUTHORITY = {
    "atomic_consumption": False,
    "benchmark_execution": "forbidden",
    "cache_staging": "forbidden",
    "candidate_execution": "forbidden",
    "capacity_reservation": "forbidden",
    "cleanup": "forbidden",
    "execution_authority": False,
    "filesystem_mutation": "forbidden",
    "journal_persistence": "forbidden",
    "model_provider_execution": "forbidden",
    "network_access": "forbidden",
    "owner_approval": False,
    "paid_execution": "forbidden",
    "recovery": "forbidden",
    "run_storage_mutation": "forbidden",
    "worktree_creation": "forbidden",
}

RUNTIME_BINDINGS = {
    "actual_bundle_sha256": None,
    "actual_manifest_sha256": None,
    "aggregate_receipt_sha256": None,
    "approval_envelope_sha256": None,
    "approval_verification_report_sha256": None,
    "atomic_consumption_genesis_sha256": None,
    "attempt_receipts_sha256": None,
    "bundle_content_verification_report_sha256": None,
    "bundle_raw_identity_report_sha256": None,
    "capacity_observation_sha256": None,
    "capacity_observer_attestation_sha256": None,
    "capacity_reservation_receipt_sha256": None,
    "classification_receipt_sha256": None,
    "cleanup_attestation_sha256": None,
    "control_root_device_id": None,
    "control_root_fsid": None,
    "control_root_inode": None,
    "execution_host_identity_sha256": None,
    "journal_head_sha256": None,
    "private_log_inventory_sha256": None,
    "private_log_root_inode": None,
    "producer_attestation_key_sha256": None,
    "recovery_report_sha256": None,
    "run_identity_sha256": None,
    "run_root_device_id": None,
    "run_root_fsid": None,
    "run_root_inode": None,
    "signed_payload_sha256": None,
    "staged_cache_tree_sha256": None,
    "stager_behavior_report_sha256": None,
    "worktree_inventory_sha256": None,
}

RESIDUAL_GATES = [
    "successor_owner_approval_and_trust_roots_binding_exact_storage_contract",
    "actual_source_reviewed_bundle_and_successful_v2_content_report",
    "atomic_single_use_consumption_record_that_is_also_journal_genesis",
    "durable_bounded_journal_recovery_and_descriptor_safe_cleanup_adapter",
    "trusted_apfs_capacity_reservation_release_and_accounting_mechanism",
    "safe_third_pass_cache_stager_and_go_cache_behavior_validation",
    "clean_detached_worktree_executor_and_first_parent_reversal_proof",
    "attested_attempt_producer_and_classifier_integration",
    "private_log_retention_inventory_and_aggregate_accounting",
    "fail_closed_interruption_cleanup_release_and_no_receipt_attestation",
    "checked_prefix_items_schema_auditor_for_attempt_and_classification_schemas",
]


class RunStorageContractError(ValueError):
    """Raised when the frozen run-storage contract or a dependency differs."""


_CACHE_MODULE: types.ModuleType | None = None


def _require(condition: bool, message: str) -> None:
    if not condition:
        raise RunStorageContractError(message)


def _sha256(raw: bytes) -> str:
    _require(type(raw) is bytes, "SHA-256 input must be exact bytes")
    return hashlib.sha256(raw).hexdigest()


def _stat_identity(value: os.stat_result) -> tuple[int, ...]:
    return (
        value.st_dev,
        value.st_ino,
        value.st_mode,
        value.st_uid,
        value.st_gid,
        value.st_nlink,
        value.st_size,
        value.st_mtime_ns,
        value.st_ctime_ns,
    )


def _read_exact_source(path: pathlib.Path, *, expected_sha256: str, expected_size: int, label: str) -> bytes:
    _require(path.parent == ROOT and path.name not in {"", ".", ".."}, f"{label} path differs")
    flags = os.O_RDONLY
    for name in ("O_CLOEXEC", "O_NOFOLLOW", "O_NONBLOCK"):
        flag = getattr(os, name, None)
        _require(type(flag) is int and flag != 0, f"secure flag {name} is unavailable")
        flags |= cast(int, flag)
    root_flags = os.O_RDONLY
    for name in ("O_CLOEXEC", "O_NOFOLLOW", "O_DIRECTORY"):
        flag = getattr(os, name, None)
        _require(type(flag) is int and flag != 0, f"secure flag {name} is unavailable")
        root_flags |= cast(int, flag)
    root_fd: int | None = None
    fd: int | None = None
    try:
        root_fd = os.open(ROOT, root_flags)
        fd = os.open(path.name, flags, dir_fd=root_fd)
        before = os.fstat(fd)
        _require(stat.S_ISREG(before.st_mode) and before.st_nlink == 1, f"{label} type differs")
        _require(before.st_size == expected_size <= MAX_SOURCE_BYTES, f"{label} size differs")
        chunks: list[bytes] = []
        remaining = expected_size + 1
        while remaining:
            chunk = os.read(fd, min(READ_CHUNK_BYTES, remaining))
            if not chunk:
                break
            chunks.append(chunk)
            remaining -= len(chunk)
        raw = b"".join(chunks)
        after = os.fstat(fd)
        _require(len(raw) == expected_size and _stat_identity(before) == _stat_identity(after), f"{label} changed while reading")
        _require(_sha256(raw) == expected_sha256, f"{label} hash differs")
        return raw
    except RunStorageContractError:
        raise
    except OSError as exc:
        raise RunStorageContractError(f"cannot read {label}") from exc
    finally:
        if fd is not None:
            os.close(fd)
        if root_fd is not None:
            os.close(root_fd)


def _checked_cache_module() -> types.ModuleType:
    global _CACHE_MODULE
    raw = _read_exact_source(
        CACHE_SOURCE_PATH,
        expected_sha256=CHECKED_CACHE_SOURCE_SHA256,
        expected_size=CHECKED_CACHE_SOURCE_SIZE,
        label="cache bundle verifier source",
    )
    if _CACHE_MODULE is not None:
        return _CACHE_MODULE
    name = "_entire_brain_checked_negative_control_cache_bundle_v2"
    module = types.ModuleType(name)
    module.__file__ = str(CACHE_SOURCE_PATH)
    previous = sys.modules.get(name)
    try:
        sys.modules[name] = module
        exec(compile(raw, str(CACHE_SOURCE_PATH), "exec", dont_inherit=True), module.__dict__)
    except Exception as exc:
        raise RunStorageContractError("checked cache bundle verifier cannot be loaded") from exc
    finally:
        if previous is None:
            sys.modules.pop(name, None)
        else:
            sys.modules[name] = previous
    required = ("_load_fixed_dependencies", "_check_verifier_contract", "_checked_v1", "CacheBundleVerificationError")
    _require(all(hasattr(module, item) for item in required), "checked cache bundle verifier API differs")
    _CACHE_MODULE = module
    return module


COMPONENT_BINDING_SPECS: tuple[tuple[str, pathlib.Path, str], ...] = (
    ("execution_contract", E0_PATH, CHECKED_E0_RAW_SHA256),
    ("execution_contract_builder", E0_SOURCE_PATH, CHECKED_E0_SOURCE_SHA256),
    ("execution_contract_schema", E0_SCHEMA_PATH, CHECKED_E0_SCHEMA_SHA256),
    ("run_plan", PLAN_PATH, CHECKED_PLAN_RAW_SHA256),
    ("run_plan_builder", PLAN_SOURCE_PATH, CHECKED_PLAN_SOURCE_SHA256),
    ("run_plan_schema", PLAN_SCHEMA_PATH, CHECKED_PLAN_SCHEMA_SHA256),
    ("cache_bundle_verifier_contract", ROOT / "negative-control-cache-bundle-verifier-contract-v2.json", CHECKED_BUNDLE_RAW_SHA256),
    ("cache_bundle_verifier_source", CACHE_SOURCE_PATH, CHECKED_CACHE_SOURCE_SHA256),
    ("cache_bundle_verifier_schema", SCHEMA_ROOT / "negative-control-cache-bundle-verifier-contract-v2.schema.json", CHECKED_BUNDLE_SCHEMA_SHA256),
    ("cache_bundle_report_schema", SCHEMA_ROOT / "negative-control-cache-bundle-verification-report-v2.schema.json", CHECKED_BUNDLE_REPORT_SCHEMA_SHA256),
    ("cache_bundle_format", ROOT / "negative-control-cache-bundle-format-v2.json", CHECKED_FORMAT_RAW_SHA256),
    ("cache_bundle_format_schema", SCHEMA_ROOT / "negative-control-cache-bundle-format-v2.schema.json", CHECKED_FORMAT_SCHEMA_SHA256),
    ("cache_material_declaration", ROOT / "negative-control-cache-archive-material-v1.json", CHECKED_MATERIAL_RAW_SHA256),
    ("cache_material_schema", SCHEMA_ROOT / "negative-control-cache-archive-material-v1.schema.json", CHECKED_MATERIAL_SCHEMA_SHA256),
    ("owner_approval_verifier_contract", OWNER_PATH, CHECKED_OWNER_RAW_SHA256),
    ("owner_approval_verifier_source", OWNER_SOURCE_PATH, CHECKED_OWNER_SOURCE_SHA256),
    ("owner_approval_verifier_schema", OWNER_SCHEMA_PATH, CHECKED_OWNER_SCHEMA_SHA256),
    ("owner_trust_roots", TRUST_ROOTS_PATH, CHECKED_TRUST_RAW_SHA256),
    ("owner_trust_roots_schema", TRUST_ROOTS_SCHEMA_PATH, CHECKED_TRUST_SCHEMA_SHA256),
    ("owner_approval_envelope_schema", APPROVAL_SCHEMA_PATH, CHECKED_APPROVAL_SCHEMA_SHA256),
    ("owner_approval_report_schema", APPROVAL_REPORT_SCHEMA_PATH, CHECKED_APPROVAL_REPORT_SCHEMA_SHA256),
    ("darwin_capacity_observer", CAPACITY_SOURCE_PATH, CHECKED_CAPACITY_SOURCE_SHA256),
    ("darwin_capacity_observation_schema", CAPACITY_SCHEMA_PATH, CHECKED_CAPACITY_SCHEMA_SHA256),
    ("gate_primitive", GATE_SOURCE_PATH, CHECKED_GATE_SOURCE_SHA256),
    ("gate_receipt_schema", GATE_SCHEMA_PATH, CHECKED_GATE_SCHEMA_SHA256),
    ("private_log_primitive", PRIVATE_LOG_SOURCE_PATH, CHECKED_PRIVATE_LOG_SOURCE_SHA256),
    ("private_log_receipt_schema", PRIVATE_LOG_SCHEMA_PATH, CHECKED_PRIVATE_LOG_SCHEMA_SHA256),
    ("attempt_classifier", CLASSIFIER_SOURCE_PATH, CHECKED_CLASSIFIER_SOURCE_SHA256),
    ("attempt_observation_schema", ATTEMPT_SCHEMA_PATH, CHECKED_ATTEMPT_SCHEMA_SHA256),
    ("classification_receipt_schema", CLASSIFICATION_SCHEMA_PATH, CHECKED_CLASSIFICATION_SCHEMA_SHA256),
    ("draft202012_validator", DRAFT_SOURCE_PATH, CHECKED_DRAFT_SOURCE_SHA256),
    ("run_journal_event_schema", JOURNAL_SCHEMA_PATH, CHECKED_JOURNAL_SCHEMA_SHA256),
    ("capacity_reservation_receipt_schema", RESERVATION_SCHEMA_PATH, CHECKED_RESERVATION_SCHEMA_SHA256),
    ("cleanup_attestation_schema", CLEANUP_SCHEMA_PATH, CHECKED_CLEANUP_SCHEMA_SHA256),
)


def _canonical_bytes(value: Any) -> bytes:
    try:
        return json.dumps(
            value,
            allow_nan=False,
            ensure_ascii=False,
            separators=(",", ":"),
            sort_keys=True,
        ).encode("utf-8")
    except (TypeError, ValueError, UnicodeError) as exc:
        raise RunStorageContractError("value is not canonical JSON") from exc


def _canonical_hash(value: Any) -> str:
    return _sha256(_canonical_bytes(value))


def _render(value: Mapping[str, Any]) -> bytes:
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
        raise RunStorageContractError("value is not renderable canonical JSON") from exc
    _require(len(raw) <= MAX_JSON_BYTES, "contract exceeds the raw-byte ceiling")
    return raw


def _self_hash(value: Mapping[str, Any]) -> str:
    projected = copy.deepcopy(dict(value))
    _require("contract_sha256" in projected, "contract_sha256 is missing")
    projected["contract_sha256"] = None
    return _canonical_hash(projected)


def _read_bound_bytes(v1: types.ModuleType, path: pathlib.Path, expected_sha256: str, label: str) -> bytes:
    try:
        raw = v1._read_bounded(
            path,
            maximum=MAX_JSON_BYTES if path.suffix == ".json" else MAX_SOURCE_BYTES,
            label=label,
            require_single_link=True,
        )
    except v1.CacheArchiveVerificationError as exc:
        raise RunStorageContractError(str(exc)) from exc
    _require(_sha256(raw) == expected_sha256, f"{label} hash differs")
    return raw


def _load_bound_schema(
    v1: types.ModuleType,
    path: pathlib.Path,
    expected_sha256: str,
    label: str,
) -> tuple[dict[str, Any], bytes]:
    try:
        schema, raw = v1._load_schema(path, expected_sha256=expected_sha256, label=label)
        v1._audit_schema(schema, schema_name=path.name, label=label)
    except v1.CacheArchiveVerificationError as exc:
        raise RunStorageContractError(str(exc)) from exc
    return cast(dict[str, Any], schema), cast(bytes, raw)


def _load_bound_json(
    v1: types.ModuleType,
    path: pathlib.Path,
    expected_raw_sha256: str,
    label: str,
    *,
    schema: Mapping[str, Any],
    self_field: str,
    expected_self_sha256: str,
) -> tuple[dict[str, Any], bytes]:
    try:
        value, raw = v1._read_json(
            path,
            maximum=MAX_JSON_BYTES,
            label=label,
            canonical=True,
            require_single_link=True,
        )
        v1._validate_schema(value, schema, schema_name=path.name, label=label)
        self_hash = v1._field_self_hash(value, self_field)
    except v1.CacheArchiveVerificationError as exc:
        raise RunStorageContractError(str(exc)) from exc
    _require(_sha256(raw) == expected_raw_sha256, f"{label} raw hash differs")
    _require(value.get(self_field) == expected_self_sha256 == self_hash, f"{label} self hash differs")
    return cast(dict[str, Any], value), cast(bytes, raw)


def _component_bindings(v1: types.ModuleType) -> list[dict[str, str]]:
    result: list[dict[str, str]] = []
    roles: set[str] = set()
    for role, path, expected in COMPONENT_BINDING_SPECS:
        _require(role not in roles, "component binding roles collide")
        roles.add(role)
        raw = _read_bound_bytes(v1, path, expected, role.replace("_", " "))
        result.append(
            {
                "artifact_file": path.name,
                "artifact_sha256": _sha256(raw),
                "role": role,
            }
        )
    return result


def _cross_component_invariants() -> dict[str, Any]:
    return {
        "approval_consumption": {
            "cas_claim_is_journal_genesis": True,
            "first_persistent_new_run_effect": "atomic_single_use_consumption_and_genesis_record",
            "no_separate_cas_to_genesis_gap": True,
            "owner_v1_binding_effect": "cannot_authorize_successor_storage_contract",
            "pre_consumption_state": "logical_read_only_contract_checked_execution_forbidden",
            "recovery_effect": "cleanup_release_and_no_receipt_only_never_resume_or_rerun",
            "verified_owner_report_atomic_consumption": False,
        },
        "cache": {
            "bundle_contract_sha256": CHECKED_BUNDLE_SHA256,
            "bundle_status": "verifier_compiled_raw_material_source_review_pending_bundle_access_forbidden",
            "format_contract_sha256": CHECKED_FORMAT_SHA256,
            "format_status": "wire_format_frozen_no_material_no_authority",
            "material_declaration_sha256": CHECKED_MATERIAL_SHA256,
            "material_status": "pending_source_review",
            "shared_seed_semantics": "one_shared_union_seed_copied_unchanged_to_every_arm",
        },
        "capacity": {
            "independent_hold_through_staging_claim": "forbidden_unsound_at_16_gib_preflight",
            "max_total_staging_bytes": 8_589_934_592,
            "minimum_free_disk_before_staging_bytes": 17_179_869_184,
            "minimum_residual_free_bytes": 8_589_934_592,
            "physical_reservation_mechanism": None,
            "proof_alternatives": [
                "spendable_conversion_reservation_remaining_plus_staging_actual_equals_8_gib",
                "independent_8_gib_hold_requires_24_gib_preflight",
            ],
        },
        "execution_contract": {
            "authority_sha256": CHECKED_E0_AUTHORITY_SHA256,
            "candidate_bindings_sha256": CHECKED_E0_CANDIDATES_SHA256,
            "contract_sha256": CHECKED_E0_SHA256,
            "enforcement_requirements_sha256": CHECKED_E0_ENFORCEMENT_SHA256,
            "protocol_sha256": CHECKED_E0_PROTOCOL_SHA256,
            "residual_gates_sha256": CHECKED_E0_RESIDUAL_SHA256,
            "runtime_bindings_sha256": CHECKED_E0_RUNTIME_SHA256,
            "schedule_sha256": CHECKED_E0_SCHEDULE_SHA256,
            "state_machine_sha256": CHECKED_E0_STATE_MACHINE_SHA256,
            "status": "contract_compiled_execution_forbidden",
            "total_attempt_count": 248,
            "total_candidate_count": 62,
        },
        "legacy_schema_audit_boundary": {
            "checked_draft_validator_unsupported_keyword": "prefixItems",
            "exact_raw_bound_schemas": [
                "negative-control-attempt-observations-v1.schema.json",
                "negative-control-classification-receipt-v1.schema.json",
            ],
            "execution_effect": "forbidden_until_checked_prefix_items_auditor_exists",
            "silent_meta_audit_skip": False,
        },
        "owner_approval": {
            "root_count": 0,
            "trust_roots_sha256": CHECKED_TRUST_SHA256,
            "trust_status": "pending_owner_authorization",
            "verifier_contract_sha256": CHECKED_OWNER_SHA256,
            "verifier_status": "verifier_compiled_trust_root_pending_execution_forbidden",
        },
        "run_plan": {
            "plan_sha256": CHECKED_PLAN_SHA256,
            "repository_order": ["entire-brain", "entire-db", "entire-graph"],
            "resource_budget_sha256": CHECKED_RESOURCE_BUDGET_SHA256,
            "status": "pending_owner_authorization",
            "toolchain_bindings_sha256": CHECKED_TOOLCHAINS_SHA256,
        },
        "runtime_evidence": {
            "all_bindings_null": True,
            "future_schemas_non_authorizing": True,
            "schemas_do_not_prove_implementation": True,
        },
    }


def _storage_protocol(e0_state_machine: Mapping[str, Any]) -> dict[str, Any]:
    return {
        "approval_and_genesis": {
            "atomic_record": "single_use_approval_cas_claim_is_same_durable_record_as_journal_genesis",
            "binding": "approval_raw_self_payload_run_identity_exact_e0_and_exact_storage_contract",
            "failure_rule": "adapter_forbidden_if_claim_and_genesis_payload_cannot_be_persisted_atomically",
            "first_external_effect": "atomic_consumption_genesis_commit",
            "pre_consumption_work": "read_only_in_memory_only_no_started_run",
        },
        "capacity": {
            "important_usage_capacity": "observation_only_never_reservation",
            "independent_hold_preflight_bytes": 25_769_803_776,
            "max_staging_bytes": 8_589_934_592,
            "minimum_preflight_bytes": 17_179_869_184,
            "minimum_residual_bytes": 8_589_934_592,
            "reservation_mechanism": None,
            "status": "research_pending_no_reservation_claim",
        },
        "cleanup": {
            "control_root_deleted_with_run_root": False,
            "order": "descriptor_relative_postorder_descendants_only",
            "reclaimed_apfs_blocks_claim": False,
            "same_device_reverified_at_every_descent_and_before_delete": True,
            "target": "disposable_run_root_descendants_never_control_root",
        },
        "durability": {
            "effect_protocol": "prepared_record_durable_then_effect_then_verify_then_committed_record_durable",
            "full_chain_recovery_validation": True,
            "implementation_status": "absent_contract_only",
            "journal_hash_chain": "immutable_canonical_o_excl_sha256",
            "malformed_gap_tail_or_unknown_rule": "cleanup_failed_latched_never_receipt",
            "max_record_bytes": 65_536,
            "max_record_count": 20_000,
            "max_total_journal_bytes": 67_108_864,
            "publication_rule": "immutable_exclusive_create_no_replace",
            "record_encoding": "canonical_bytes_only",
            "record_ordinal": "contiguous_zero_based",
            "stable_control_root": "0700_current_uid_nonsymlink_pinned_descriptor_outside_disposable_run_root",
            "transition_rule": "durable_before_and_after_every_external_effect",
        },
        "e0_state_machine": copy.deepcopy(dict(e0_state_machine)),
        "e0_state_machine_sha256": CHECKED_E0_STATE_MACHINE_SHA256,
        "lifecycle_overlay": {
            "aggregate_receipt_guard": "cursor_248_ceilings_passed_run_cleanup_committed_reservation_release_committed_latch_false",
            "cursor_rule": "plus_one_only_after_matching_attempt_cleanup_committed",
            "initial_state_persistence": "logical_only_until_atomic_consumption_genesis",
            "receipt_forbidden_latch": "persistent_monotonic_false_to_true",
            "recovery_rule": "started_nonterminal_run_latches_no_receipt_before_cleanup_and_never_resumes",
            "release_before_receipt": True,
        },
        "path_policy": {
            "boundaries": [
                "approval_cas",
                "cache",
                "control_root",
                "journal",
                "lock",
                "run_root",
                "staging",
                "cleanup",
                "worktree",
                "reversal",
                "every_derived_path",
            ],
            "descriptor_relative_only": True,
            "directory_mode": "0700",
            "file_mode": "0600",
            "forbidden": ["absolute", "empty", "dot", "dotdot", "nul", "backslash", "unicode_control", "non_nfc", "casefold_dotdot_namedfork"],
            "forbidden_component_casefold": "..namedfork",
            "serialized_absolute_locator": False,
        },
        "schema_artifacts": {
            "cleanup_attestation": "shape_only_non_authorizing_no_reclaimed_block_claim",
            "journal_event": "shape_only_non_authorizing_no_persistence_claim",
            "reservation_receipt": "shape_only_non_authorizing_mechanism_pending",
        },
    }


def _schema_bindings() -> tuple[tuple[pathlib.Path, str, str], ...]:
    return (
        (E0_SCHEMA_PATH, CHECKED_E0_SCHEMA_SHA256, "execution contract schema"),
        (PLAN_SCHEMA_PATH, CHECKED_PLAN_SCHEMA_SHA256, "run plan schema"),
        (OWNER_SCHEMA_PATH, CHECKED_OWNER_SCHEMA_SHA256, "owner verifier schema"),
        (TRUST_ROOTS_SCHEMA_PATH, CHECKED_TRUST_SCHEMA_SHA256, "owner trust roots schema"),
        (APPROVAL_SCHEMA_PATH, CHECKED_APPROVAL_SCHEMA_SHA256, "owner approval envelope schema"),
        (APPROVAL_REPORT_SCHEMA_PATH, CHECKED_APPROVAL_REPORT_SCHEMA_SHA256, "owner approval report schema"),
        (CAPACITY_SCHEMA_PATH, CHECKED_CAPACITY_SCHEMA_SHA256, "capacity observation schema"),
        (GATE_SCHEMA_PATH, CHECKED_GATE_SCHEMA_SHA256, "gate receipt schema"),
        (PRIVATE_LOG_SCHEMA_PATH, CHECKED_PRIVATE_LOG_SCHEMA_SHA256, "private log receipt schema"),
        (JOURNAL_SCHEMA_PATH, CHECKED_JOURNAL_SCHEMA_SHA256, "run journal event schema"),
        (RESERVATION_SCHEMA_PATH, CHECKED_RESERVATION_SCHEMA_SHA256, "capacity reservation receipt schema"),
        (CLEANUP_SCHEMA_PATH, CHECKED_CLEANUP_SCHEMA_SHA256, "cleanup attestation schema"),
        (CONTRACT_SCHEMA_PATH, CHECKED_CONTRACT_SCHEMA_SHA256, "run storage contract schema"),
    )


def _load_dependencies() -> dict[str, Any]:
    cache = _checked_cache_module()
    try:
        cache_dependencies = cache._load_fixed_dependencies()
        bundle_contract = cache._check_verifier_contract(cache_dependencies)
    except cache.CacheBundleVerificationError as exc:
        raise RunStorageContractError(str(exc)) from exc
    v1 = cache_dependencies["v1"]
    _require(type(v1) is types.ModuleType, "checked v1 verifier module differs")

    schemas: dict[pathlib.Path, tuple[dict[str, Any], bytes]] = {}
    for path, expected, label in _schema_bindings():
        schemas[path] = _load_bound_schema(v1, path, expected, label)

    e0, e0_raw = _load_bound_json(
        v1,
        E0_PATH,
        CHECKED_E0_RAW_SHA256,
        "execution contract",
        schema=schemas[E0_SCHEMA_PATH][0],
        self_field="contract_sha256",
        expected_self_sha256=CHECKED_E0_SHA256,
    )
    plan, plan_raw = _load_bound_json(
        v1,
        PLAN_PATH,
        CHECKED_PLAN_RAW_SHA256,
        "run plan",
        schema=schemas[PLAN_SCHEMA_PATH][0],
        self_field="plan_sha256",
        expected_self_sha256=CHECKED_PLAN_SHA256,
    )
    owner, owner_raw = _load_bound_json(
        v1,
        OWNER_PATH,
        CHECKED_OWNER_RAW_SHA256,
        "owner approval verifier contract",
        schema=schemas[OWNER_SCHEMA_PATH][0],
        self_field="verifier_contract_sha256",
        expected_self_sha256=CHECKED_OWNER_SHA256,
    )
    trust, trust_raw = _load_bound_json(
        v1,
        TRUST_ROOTS_PATH,
        CHECKED_TRUST_RAW_SHA256,
        "owner trust roots",
        schema=schemas[TRUST_ROOTS_SCHEMA_PATH][0],
        self_field="trust_roots_sha256",
        expected_self_sha256=CHECKED_TRUST_SHA256,
    )

    _require(
        e0.get("profile") == "agent_brain_negative_control_execution_contract_v1"
        and e0.get("status") == "contract_compiled_execution_forbidden"
        and e0.get("execution_status") == "forbidden_missing_all_residual_gates_and_audited_execution_adapter",
        "execution contract status differs",
    )
    _require(
        e0.get("candidate_bindings_sha256") == CHECKED_E0_CANDIDATES_SHA256
        and e0.get("schedule_sha256") == CHECKED_E0_SCHEDULE_SHA256
        and len(cast(list[Any], e0.get("candidates"))) == 62
        and len(cast(list[Any], e0.get("schedule"))) == 248,
        "execution contract population differs",
    )
    for field, expected in (
        ("authority", CHECKED_E0_AUTHORITY_SHA256),
        ("protocol", CHECKED_E0_PROTOCOL_SHA256),
        ("state_machine", CHECKED_E0_STATE_MACHINE_SHA256),
        ("enforcement_requirements", CHECKED_E0_ENFORCEMENT_SHA256),
        ("residual_gates", CHECKED_E0_RESIDUAL_SHA256),
        ("runtime_bindings", CHECKED_E0_RUNTIME_SHA256),
    ):
        _require(_canonical_hash(e0.get(field)) == expected, f"execution contract {field} projection differs")
    _require(
        type(e0.get("runtime_bindings")) is dict
        and all(value is None for value in cast(dict[str, Any], e0["runtime_bindings"]).values()),
        "execution contract runtime bindings are not null",
    )

    _require(
        plan.get("profile") == "agent_brain_development_task_negative_control_run_plan_v2"
        and plan.get("status") == "pending_owner_authorization"
        and _canonical_hash(plan.get("resource_budget")) == CHECKED_RESOURCE_BUDGET_SHA256,
        "run plan identity or resource budget differs",
    )
    repositories = cast(list[dict[str, Any]], plan.get("repositories"))
    _require(
        type(repositories) is list
        and [item.get("key") for item in repositories] == ["entire-brain", "entire-db", "entire-graph"],
        "run plan repository order differs",
    )

    _require(
        owner.get("profile") == "agent_brain_negative_control_owner_approval_verifier_contract_v1"
        and owner.get("status") == "verifier_compiled_trust_root_pending_execution_forbidden"
        and owner.get("execution_contract_binding", {}).get("contract_sha256") == CHECKED_E0_SHA256
        and owner.get("execution_contract_binding", {}).get("state_machine_sha256") == CHECKED_E0_STATE_MACHINE_SHA256
        and owner.get("authority", {}).get("atomic_consumption") is False
        and all(value is None for value in owner.get("runtime_bindings", {}).values()),
        "owner verifier remains pending or binds a different E0",
    )
    _require(
        trust.get("status") == "pending_owner_authorization"
        and trust.get("roots") == []
        and owner.get("trust_roots_binding", {}).get("root_count") == 0
        and owner.get("trust_roots_binding", {}).get("trust_roots_sha256") == CHECKED_TRUST_SHA256,
        "owner trust roots are not the checked pending empty set",
    )

    _require(
        bundle_contract.get("verifier_contract_sha256") == CHECKED_BUNDLE_SHA256
        and bundle_contract.get("status") == "verifier_compiled_raw_material_source_review_pending_bundle_access_forbidden"
        and bundle_contract.get("manifest_binding", {}).get("run_plan_sha256") == CHECKED_PLAN_SHA256
        and bundle_contract.get("manifest_binding", {}).get("toolchain_bindings_sha256") == CHECKED_TOOLCHAINS_SHA256
        and all(value is None for value in bundle_contract.get("runtime_bindings", {}).values()),
        "cache bundle verifier differs or has runtime authority",
    )
    material = cache_dependencies["v1_dependencies"]["material"]
    material_raw = cache_dependencies["v1_dependencies"]["material_raw"]
    material_fields = material.get("material", {})
    _require(
        _sha256(material_raw) == CHECKED_MATERIAL_RAW_SHA256
        and material.get("material_declaration_sha256") == CHECKED_MATERIAL_SHA256
        and material.get("status") == "pending_source_review"
        and material_fields.get("archive_interpretation") == "opaque_bytes_content_safety_not_verified"
        and all(
            value is None
            for key, value in material_fields.items()
            if key != "archive_interpretation"
        ),
        "cache material is not the checked pending declaration",
    )

    component_bindings = _component_bindings(v1)
    return {
        "bundle_contract": copy.deepcopy(bundle_contract),
        "component_bindings": component_bindings,
        "contract_schema": schemas[CONTRACT_SCHEMA_PATH][0],
        "contract_schema_raw": schemas[CONTRACT_SCHEMA_PATH][1],
        "e0": e0,
        "e0_raw": e0_raw,
        "owner": owner,
        "owner_raw": owner_raw,
        "plan": plan,
        "plan_raw": plan_raw,
        "schemas": schemas,
        "trust": trust,
        "trust_raw": trust_raw,
        "v1": v1,
    }


def _file_binding(path: pathlib.Path, raw: bytes) -> dict[str, Any]:
    return {
        "artifact_file": path.name,
        "artifact_sha256": _sha256(raw),
    }


def _implementation(dependencies: Mapping[str, Any]) -> dict[str, Any]:
    v1 = dependencies["v1"]
    source_raw = _read_bound_bytes(
        v1,
        pathlib.Path(__file__),
        _sha256(v1._read_bounded(pathlib.Path(__file__), maximum=MAX_SOURCE_BYTES, label="run storage contract builder", require_single_link=True)),
        "run storage contract builder",
    )
    schemas = dependencies["schemas"]
    return {
        "artifact_render_profile": ARTIFACT_RENDER_PROFILE,
        "builder": _file_binding(pathlib.Path(__file__), source_raw),
        "canonical_json_profile": CANONICAL_JSON_PROFILE,
        "contract_schema": _file_binding(CONTRACT_SCHEMA_PATH, dependencies["contract_schema_raw"]),
        "draft202012_validator": {
            "artifact_file": DRAFT_SOURCE_PATH.name,
            "artifact_sha256": CHECKED_DRAFT_SOURCE_SHA256,
        },
        "future_evidence_schemas": {
            "capacity_reservation_receipt": _file_binding(
                RESERVATION_SCHEMA_PATH, schemas[RESERVATION_SCHEMA_PATH][1]
            ),
            "cleanup_attestation": _file_binding(
                CLEANUP_SCHEMA_PATH, schemas[CLEANUP_SCHEMA_PATH][1]
            ),
            "run_journal_event": _file_binding(
                JOURNAL_SCHEMA_PATH, schemas[JOURNAL_SCHEMA_PATH][1]
            ),
        },
    }


def build_contract() -> dict[str, Any]:
    dependencies = _load_dependencies()
    contract: dict[str, Any] = {
        "authority": copy.deepcopy(AUTHORITY),
        "component_bindings": copy.deepcopy(dependencies["component_bindings"]),
        "contract_sha256": None,
        "cross_component_invariants": _cross_component_invariants(),
        "execution_status": EXECUTION_STATUS,
        "implementation": _implementation(dependencies),
        "profile": PROFILE,
        "residual_gates": list(RESIDUAL_GATES),
        "runtime_bindings": copy.deepcopy(RUNTIME_BINDINGS),
        "schema_version": SCHEMA_VERSION,
        "status": STATUS,
        "storage_protocol": _storage_protocol(dependencies["e0"]["state_machine"]),
    }
    contract["contract_sha256"] = _self_hash(contract)
    _validate_contract(contract, dependencies=dependencies)
    return contract


def _validate_contract(value: Mapping[str, Any], *, dependencies: Mapping[str, Any]) -> None:
    expected_fields = {
        "authority",
        "component_bindings",
        "contract_sha256",
        "cross_component_invariants",
        "execution_status",
        "implementation",
        "profile",
        "residual_gates",
        "runtime_bindings",
        "schema_version",
        "status",
        "storage_protocol",
    }
    _require(type(value) is dict and set(value) == expected_fields, "contract root fields differ")
    _require(value.get("profile") == PROFILE and value.get("schema_version") == SCHEMA_VERSION, "contract profile differs")
    _require(value.get("status") == STATUS and value.get("execution_status") == EXECUTION_STATUS, "contract status differs")
    _require(value.get("authority") == AUTHORITY, "contract authority boundary differs")
    _require(value.get("runtime_bindings") == RUNTIME_BINDINGS, "contract runtime bindings must remain null")
    _require(value.get("residual_gates") == RESIDUAL_GATES, "contract residual gates differ")
    _require(value.get("component_bindings") == dependencies["component_bindings"], "component bindings differ")
    _require(value.get("cross_component_invariants") == _cross_component_invariants(), "cross-component invariants differ")
    _require(value.get("storage_protocol") == _storage_protocol(dependencies["e0"]["state_machine"]), "storage protocol or E0 state machine differs")
    _require(value.get("implementation") == _implementation(dependencies), "implementation bindings differ")
    contract_sha256 = value.get("contract_sha256")
    _require(type(contract_sha256) is str and len(contract_sha256) == 64 and contract_sha256 != "0" * 64, "contract SHA-256 is invalid")
    _require(contract_sha256 == _self_hash(value), "contract self hash differs")
    try:
        dependencies["v1"]._validate_schema(
            value,
            dependencies["contract_schema"],
            schema_name=CONTRACT_SCHEMA_PATH.name,
            label="run storage contract",
        )
    except dependencies["v1"].CacheArchiveVerificationError as exc:
        raise RunStorageContractError(str(exc)) from exc


def check_contract() -> dict[str, Any]:
    dependencies = _load_dependencies()
    v1 = dependencies["v1"]
    try:
        value, raw = v1._read_json(
            CONTRACT_PATH,
            maximum=MAX_JSON_BYTES,
            label="checked run storage contract",
            canonical=True,
            require_single_link=True,
        )
    except v1.CacheArchiveVerificationError as exc:
        raise RunStorageContractError(str(exc)) from exc
    expected = build_contract()
    _require(value == expected and raw == _render(expected), "checked run storage contract differs from exact rebuild")
    return copy.deepcopy(expected)


def _write_atomic(path: pathlib.Path, value: Mapping[str, Any]) -> None:
    _require(path.parent.is_dir(), "output parent must already exist")
    descriptor, temporary_name = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent)
    temporary = pathlib.Path(temporary_name)
    try:
        os.fchmod(descriptor, 0o644)
        with os.fdopen(descriptor, "wb") as handle:
            handle.write(_render(value))
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
    except OSError as exc:
        raise RunStorageContractError("cannot atomically write requested contract output") from exc
    finally:
        temporary.unlink(missing_ok=True)


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="command", required=True)
    build = subparsers.add_parser("build", help="build the non-executing storage contract")
    build.add_argument("--output", required=True, type=pathlib.Path)
    subparsers.add_parser("check", help="check the fixed storage contract and dependencies")
    args = parser.parse_args(argv)
    try:
        if args.command == "build":
            _write_atomic(args.output, build_contract())
        else:
            checked = check_contract()
            print(
                json.dumps(
                    {
                        "execution_authority": checked["authority"]["execution_authority"],
                        "execution_status": checked["execution_status"],
                        "profile": checked["profile"],
                        "status": checked["status"],
                    },
                    separators=(",", ":"),
                    sort_keys=True,
                )
            )
    except RunStorageContractError as exc:
        parser.error(str(exc))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
