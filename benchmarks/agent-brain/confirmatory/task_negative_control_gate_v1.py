#!/usr/bin/env python3
"""Pure, fail-closed gate primitives for the pending negative-control plan.

This module can describe and verify an external offline Go cache seed and can
evaluate injected filesystem capacity.  It deliberately has no Git, Go,
network, worktree, candidate, model, or benchmark execution surface.  A
successful primitive receipt remains non-authoritative and cannot change the
pending run plan's execution boundary.
"""

from __future__ import annotations

import argparse
import copy
import hashlib
import json
import os
import pathlib
import platform
import stat
import sys
import unicodedata
from collections import Counter
from typing import Any, Mapping, Sequence, cast

import draft202012
import task_negative_control_plan_v2 as run_plan_v2


CACHE_MANIFEST_PROFILE = "content_addressed_offline_go_cache_seed_manifest_v1"
PREFLIGHT_RECEIPT_PROFILE = "agent_brain_negative_control_gate_primitive_receipt_v1"
SCHEMA_VERSION = 1
CANONICAL_JSON_PROFILE = "sorted_keys_compact_utf8_no_float_v1"
ARTIFACT_RENDER_PROFILE = "sorted_keys_indent_2_utf8_lf_v1"
MANIFEST_STATUS = "external_input_identity_only_execution_forbidden"
RECEIPT_STATUS = "primitive_checks_passed_execution_forbidden"
FILESYSTEM_OBSERVATION_KIND = "injected_filesystem_stats_unattested"
CACHE_ARCHIVE_OBSERVATION_KIND = "injected_archive_identity_unattested"
CACHE_BINDING_STATUS = "external_manifest_not_bound_by_pending_plan"
CACHE_ROOTS = ("gocache", "gomodcache")
REPOSITORY_KEYS = tuple(run_plan_v2.REPOSITORY_ORDER)
ROOT = pathlib.Path(__file__).parent
CHECKED_PLAN_SHA256 = "a47311fa1f4553f8085ebea6e8ddd003a4ea5d0b2d269d91bbcdd414134a248e"
CHECKED_PLAN_FILE_SHA256 = "f55b6e22a25daf8016305304adf3b7ac8d31675281d97cfc9bb78cc76b619790"
CHECKED_MANIFEST_SCHEMA_FILE_SHA256 = "37d3839411b2e30a99fdd7e93784d56f5fd525c0472717a24cc7b92213b98999"
CHECKED_RECEIPT_SCHEMA_FILE_SHA256 = "7603f755dd8429ff8c0a17114a50cda0aa23ae99c07cf2cfff9856df00c0a8e0"
MAX_PLAN_RAW_BYTES = 1 * 1024 * 1024
MAX_SCHEMA_RAW_BYTES = 4 * 1024 * 1024
MAX_RECEIPT_RAW_BYTES = 4 * 1024 * 1024
MAX_MANIFEST_RAW_BYTES = 128 * 1024 * 1024
MAX_MANIFEST_FILE_COUNT = 250_000
MAX_MANIFEST_ENTRY_JSON_NODES = 5
MAX_JSON_FIXED_ENVELOPE_NODES = 10_000
MAX_JSON_VISITED_NODES = (
    MAX_MANIFEST_FILE_COUNT * MAX_MANIFEST_ENTRY_JSON_NODES
    + MAX_JSON_FIXED_ENVELOPE_NODES
)
MAX_JSON_DEPTH = 64
MAX_JSON_INTEGER_DIGITS = 64
MAX_JSON_INTEGER_ABSOLUTE = 10**MAX_JSON_INTEGER_DIGITS - 1
_NONE_TYPE = type(None)
EXACT_JSON_TYPES_ERROR = "canonical gate JSON requires exact built-in JSON value types"
EXACT_JSON_KEYS_ERROR = "canonical gate JSON requires exact built-in string object keys"
MAX_RESOURCE_AVAILABLE_BLOCKS = MAX_JSON_INTEGER_ABSOLUTE
MAX_RESOURCE_FRAGMENT_SIZE_BYTES = MAX_JSON_INTEGER_ABSOLUTE
MAX_RESOURCE_DERIVED_BYTES = MAX_JSON_INTEGER_ABSOLUTE
MAX_IMPLEMENTATION_SOURCE_BYTES = 4 * 1024 * 1024
MAX_PYTHON_EXECUTABLE_BYTES = 256 * 1024 * 1024
CHECKS = [
    "pending_authority_boundary_verified",
    "cache_manifest_identity_verified",
    "injected_cache_archive_identity_observation_matches_manifest",
    "resource_budget_arithmetic_verified",
    "injected_free_space_threshold_verified",
]
RESIDUAL_GATES = [
    "owner_execution_approval_receipt_and_trust_mechanism",
    "authorized_plan_binding_for_actual_cache_seed",
    "safe_archive_traversal_type_link_device_and_content_verifier",
    "trusted_filesystem_observer_and_atomic_resource_reservation",
    "negative_control_executor_and_reversal_proof",
    "classification_truth_table_and_receipt_checker",
    "private_content_addressed_log_writer_and_checker",
    "fail_closed_cleanup_and_no_receipt_on_interruption",
]


class GatePrimitiveError(ValueError):
    """Raised when a manifest, observation, or primitive receipt differs."""


def _require(condition: bool, message: str) -> None:
    if not condition:
        raise GatePrimitiveError(message)


def _sha256(raw: bytes) -> str:
    _require(type(raw) is bytes, "SHA-256 input must be exact built-in bytes")
    return hashlib.sha256(raw).hexdigest()


def _validate_json_profile(value: Any) -> int:
    stack: list[tuple[Any, int]] = [(value, 1)]
    seen_container_ids: set[int] = set()
    visited_nodes = 0
    while stack:
        current, depth = stack.pop()
        visited_nodes += 1
        _require(
            visited_nodes <= MAX_JSON_VISITED_NODES,
            "JSON exceeds the visited-node ceiling",
        )
        _require(depth <= MAX_JSON_DEPTH, "JSON exceeds the nesting-depth ceiling")
        current_type = type(current)
        if current_type is float:
            raise GatePrimitiveError("canonical gate JSON forbids floating-point values")
        is_container = (
            current_type is dict
            or current_type is list
            or current_type is tuple
        )
        is_scalar = (
            current_type is _NONE_TYPE
            or current_type is str
            or current_type is int
            or current_type is bool
        )
        _require(is_container or is_scalar, EXACT_JSON_TYPES_ERROR)
        if current_type is int:
            _require(
                -MAX_JSON_INTEGER_ABSOLUTE
                <= cast(int, current)
                <= MAX_JSON_INTEGER_ABSOLUTE,
                "JSON integer exceeds the digit ceiling",
            )
        if is_container:
            identity = id(current)
            _require(
                identity not in seen_container_ids,
                "JSON contains a cycle or repeated container alias",
            )
            seen_container_ids.add(identity)
        if current_type is dict:
            _require(
                all(type(key) is str for key in current),
                EXACT_JSON_KEYS_ERROR,
            )
            _require(
                visited_nodes + len(stack) + len(current)
                <= MAX_JSON_VISITED_NODES,
                "JSON exceeds the visited-node ceiling",
            )
            stack.extend((child, depth + 1) for child in current.values())
        elif current_type is list or current_type is tuple:
            _require(
                visited_nodes + len(stack) + len(current)
                <= MAX_JSON_VISITED_NODES,
                "JSON exceeds the visited-node ceiling",
            )
            stack.extend((child, depth + 1) for child in current)
    return visited_nodes


def _canonical_json_bytes(value: Any) -> bytes:
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
        raise GatePrimitiveError(f"value is not canonical JSON: {exc}") from exc


def _canonical_hash(value: Any) -> str:
    return _sha256(_canonical_json_bytes(value))


def _render(value: dict[str, Any]) -> bytes:
    _validate_json_profile(value)
    try:
        return (
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
        raise GatePrimitiveError(f"value is not renderable canonical JSON: {exc}") from exc


def _self_hash(value: dict[str, Any], field: str) -> str:
    _validate_json_profile(value)
    _require(type(value) is dict, EXACT_JSON_TYPES_ERROR)
    _require(type(field) is str, EXACT_JSON_TYPES_ERROR)
    _require(field in value, f"{field} is missing")
    projected = value.copy()
    projected[field] = None
    return _canonical_hash(projected)


def _reject_duplicate_pairs(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        _require(key not in result, "JSON contains a duplicate object key")
        result[key] = value
    return result


def _read_bounded(
    path: pathlib.Path,
    *,
    max_raw_bytes: int,
    label: str,
) -> bytes:
    _require(max_raw_bytes >= 1, f"{label} byte ceiling is invalid")
    flags = os.O_RDONLY
    for flag_name in ("O_CLOEXEC", "O_NOFOLLOW", "O_NONBLOCK"):
        flag = getattr(os, flag_name, None)
        _require(
            type(flag) is int and flag != 0,
            f"secure file-open flag {flag_name} is unavailable",
        )
        flags |= cast(int, flag)
    descriptor: int | None = None
    try:
        descriptor = os.open(path, flags)
        before = os.fstat(descriptor)
        _require(stat.S_ISREG(before.st_mode), f"{label} is not a regular file")
        _require(before.st_size <= max_raw_bytes, f"{label} exceeds the raw-byte ceiling")
        remaining = max_raw_bytes + 1
        chunks: list[bytes] = []
        while remaining:
            chunk = os.read(descriptor, min(1024 * 1024, remaining))
            if not chunk:
                break
            chunks.append(chunk)
            remaining -= len(chunk)
        raw = b"".join(chunks)
        after = os.fstat(descriptor)
        _require(len(raw) <= max_raw_bytes, f"{label} exceeds the raw-byte ceiling")
        _require(
            (
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
            == (
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
            and len(raw) == before.st_size,
            f"{label} changed while being read",
        )
        return raw
    except GatePrimitiveError:
        raise
    except OSError as exc:
        raise GatePrimitiveError(f"cannot read {label}: {exc}") from exc
    finally:
        if descriptor is not None:
            os.close(descriptor)


def _parse_bounded_integer(text: str) -> int:
    digits = text.removeprefix("-")
    _require(
        len(digits) <= MAX_JSON_INTEGER_DIGITS,
        "JSON integer exceeds the digit ceiling",
    )
    return int(text)


def _reject_json_float(_text: str) -> float:
    raise GatePrimitiveError("canonical gate JSON forbids floating-point values")


def _reject_json_constant(_text: str) -> None:
    raise GatePrimitiveError("canonical gate JSON forbids non-finite values")


def _load_json(
    path: pathlib.Path,
    *,
    max_raw_bytes: int = MAX_SCHEMA_RAW_BYTES,
    label: str = "JSON input",
) -> tuple[dict[str, Any], bytes]:
    raw = _read_bounded(path, max_raw_bytes=max_raw_bytes, label=label)
    try:
        value = json.loads(
            raw,
            object_pairs_hook=_reject_duplicate_pairs,
            parse_constant=_reject_json_constant,
            parse_float=_reject_json_float,
            parse_int=_parse_bounded_integer,
        )
        _validate_json_profile(value)
    except GatePrimitiveError:
        raise
    except (UnicodeError, json.JSONDecodeError, ValueError, RecursionError) as exc:
        raise GatePrimitiveError(f"cannot parse {label}: {exc}") from exc
    _require(type(value) is dict, "JSON root must be an object")
    return value, raw


def _valid_sha(value: Any) -> bool:
    return (
        type(value) is str
        and len(value) == 64
        and run_plan_v2.eligibility.SHA256_RE.fullmatch(value) is not None
        and value != "0" * 64
    )


def _integer(
    value: Any,
    field: str,
    *,
    minimum: int = 0,
    maximum: int = MAX_JSON_INTEGER_ABSOLUTE,
) -> int:
    _require(
        type(value) is int and minimum <= value <= maximum,
        f"{field} is invalid",
    )
    return cast(int, value)


def _exact_bytes(value: Any, label: str) -> bytes:
    _require(type(value) is bytes, f"{label} must be exact built-in bytes")
    return cast(bytes, value)


def _artifact_file(value: Any, field: str) -> str:
    _require(type(value) is str and bool(value), f"{field} is invalid")
    value = cast(str, value)
    _require(len(value) <= 255, f"{field} is too long")
    _require(
        pathlib.PurePosixPath(value).name == value
        and value not in {".", ".."}
        and "\\" not in value
        and "\x00" not in value
        and not any(unicodedata.category(character).startswith("C") for character in value),
        f"{field} is not a canonical file name",
    )
    _require(
        len(value.encode("utf-8")) <= 255
        and unicodedata.normalize("NFC", value) == value,
        f"{field} encoding is not canonical",
    )
    return value


def _cache_path(value: Any, field: str) -> str:
    _require(type(value) is str and bool(value), f"{field} is invalid")
    value = cast(str, value)
    _require(len(value) <= 1024, f"{field} is too long")
    pure = pathlib.PurePosixPath(value)
    root = pure.parts[0] if pure.parts else None
    _require(
        not pure.is_absolute()
        and str(pure) == value
        and len(pure.parts) >= 2
        and "\\" not in value
        and "\x00" not in value
        and all(part not in {"", ".", ".."} for part in pure.parts),
        f"{field} is not a canonical relative path",
    )
    _require(root in CACHE_ROOTS, f"{field} is outside the isolated cache roots")
    _require(
        not any(unicodedata.category(character).startswith("C") for character in value),
        f"{field} component encoding is not canonical",
    )
    _require(
        len(value.encode("utf-8")) <= 1024
        and unicodedata.normalize("NFC", value) == value
        and all(len(part.encode("utf-8")) <= 255 for part in pure.parts),
        f"{field} encoding is not canonical",
    )
    return value


def _portable_path_key(value: str) -> tuple[str, ...]:
    return tuple(
        unicodedata.normalize(
            "NFC",
            unicodedata.normalize("NFC", component).casefold(),
        )
        for component in pathlib.PurePosixPath(value).parts
    )


def _validate_manifest_profile_limits(
    *,
    file_count: int,
    rendered_byte_count: int,
) -> None:
    _integer(file_count, "manifest file count", minimum=3)
    _integer(rendered_byte_count, "manifest rendered byte count", minimum=1)
    _require(
        3 <= file_count <= MAX_MANIFEST_FILE_COUNT,
        "manifest file count exceeds the frozen profile ceiling",
    )
    _require(
        1 <= rendered_byte_count <= MAX_MANIFEST_RAW_BYTES,
        "manifest rendered bytes exceed the frozen profile ceiling",
    )


def _pending_authority(plan: dict[str, Any]) -> dict[str, Any]:
    _validate_json_profile(plan)
    try:
        run_plan_v2.validate_plan(plan)
    except run_plan_v2.RunPlanError as exc:
        raise GatePrimitiveError(f"run plan is invalid: {exc}") from exc
    _require(
        plan["plan_sha256"] == CHECKED_PLAN_SHA256,
        "run plan differs from the checked plan self hash",
    )
    _require(
        _sha256(run_plan_v2._render(plan)) == CHECKED_PLAN_FILE_SHA256,
        "run plan differs from the checked plan raw hash",
    )
    try:
        run_plan_v2.verify_plan_dependencies(
            plan,
            contract_path=ROOT / "development-task-repositories-v2.json",
            eligibility_schema_path=(
                ROOT / "schemas" / "development-task-eligibility-scan-v2.schema.json"
            ),
            ledger_paths=[
                ROOT / "development-task-eligibility-entire-brain-v2.json",
                ROOT / "development-task-eligibility-entire-db-v2.json",
                ROOT / "development-task-eligibility-entire-graph-v2.json",
            ],
            plan_schema_path=(
                ROOT / "schemas" / "development-task-negative-control-run-plan-v2.schema.json"
            ),
            registry_path=ROOT / "development-task-overlap-registry-v1.json",
            registry_schema_path=(
                ROOT / "schemas" / "development-task-overlap-registry-v1.schema.json"
            ),
        )
    except run_plan_v2.RunPlanError as exc:
        raise GatePrimitiveError(
            f"run plan exact dependency verification failed: {exc}"
        ) from exc
    _require(plan["status"] == "pending_owner_authorization", "run plan is not pending")
    authority = plan["authority"]
    _require(
        authority["candidate_execution"] == "forbidden_plan_unexecutable"
        and authority["benchmark_execution"] == "forbidden_pending_separate_owner_approval"
        and authority["model_provider_execution"] == "forbidden_not_authorized"
        and authority["paid_execution"] == "forbidden_not_authorized",
        "run plan execution authority differs",
    )
    cache_seed = plan["cache_seed"]
    _require(
        cache_seed["manifest_sha256"] is None
        and cache_seed["archive_sha256"] is None
        and cache_seed["source_locator"] is None
        and cache_seed["unpacked_byte_count"] is None,
        "pending plan unexpectedly binds a cache seed",
    )
    return {
        "benchmark_execution": authority["benchmark_execution"],
        "candidate_execution": authority["candidate_execution"],
        "model_provider_execution": authority["model_provider_execution"],
        "paid_execution": authority["paid_execution"],
        "plan_status": plan["status"],
    }


def _toolchain_projection(plan: dict[str, Any]) -> list[dict[str, Any]]:
    return [
        {
            "repository_key": repository["key"],
            "toolchain": copy.deepcopy(repository["toolchain"]),
        }
        for repository in plan["repositories"]
    ]


def _module_source_sha256(module: Any, label: str) -> str:
    source = getattr(module, "__file__", None)
    _require(type(source) is str and bool(source), f"{label} source path is unavailable")
    raw = _read_bounded(
        pathlib.Path(cast(str, source)),
        max_raw_bytes=MAX_IMPLEMENTATION_SOURCE_BYTES,
        label=f"{label} source",
    )
    return _sha256(raw)


def _verifier_source_hashes() -> dict[str, str]:
    return {
        "draft202012.py": _module_source_sha256(draft202012, "draft202012"),
        "relevance_dataset.py": _module_source_sha256(
            run_plan_v2.cli_population.relevance_dataset,
            "relevance_dataset",
        ),
        "task_eligibility.py": _module_source_sha256(
            run_plan_v2.cli_eligibility,
            "task_eligibility",
        ),
        "task_eligibility_v2.py": _module_source_sha256(
            run_plan_v2.eligibility,
            "task_eligibility_v2",
        ),
        "task_negative_control_plan_v2.py": _module_source_sha256(
            run_plan_v2,
            "task_negative_control_plan_v2",
        ),
        "task_overlap_registry.py": _module_source_sha256(
            run_plan_v2.overlap,
            "task_overlap_registry",
        ),
        "task_population.py": _module_source_sha256(
            run_plan_v2.cli_population,
            "task_population",
        ),
    }


def _manifest_implementation(schema_path: pathlib.Path) -> dict[str, Any]:
    try:
        python_executable = pathlib.Path(sys.executable).resolve(strict=True)
    except OSError as exc:
        raise GatePrimitiveError(f"cannot read manifest implementation dependency: {exc}") from exc
    builder_hash = _module_source_sha256(sys.modules[__name__], "gate primitive")
    sources = _verifier_source_hashes()
    schema_hash = _sha256(
        _read_bounded(
            schema_path,
            max_raw_bytes=MAX_SCHEMA_RAW_BYTES,
            label="manifest schema",
        )
    )
    _require(
        schema_hash == CHECKED_MANIFEST_SCHEMA_FILE_SHA256,
        "manifest schema raw hash differs from the checked-in v1 schema",
    )
    python_executable_hash = _sha256(
        _read_bounded(
            python_executable,
            max_raw_bytes=MAX_PYTHON_EXECUTABLE_BYTES,
            label="Python executable",
        )
    )
    return {
        "artifact_render_profile": ARTIFACT_RENDER_PROFILE,
        "builder_sha256": builder_hash,
        "canonical_json_profile": CANONICAL_JSON_PROFILE,
        "manifest_schema_sha256": schema_hash,
        "python_executable_sha256": python_executable_hash,
        "python_implementation": platform.python_implementation(),
        "python_version": platform.python_version(),
        "run_plan_builder_sha256": sources["task_negative_control_plan_v2.py"],
        "schema_validator_sha256": sources["draft202012.py"],
        "unicode_data_version": unicodedata.unidata_version,
        "verifier_sources": sources,
    }


def _receipt_implementation(
    manifest_schema_path: pathlib.Path,
    receipt_schema_path: pathlib.Path,
) -> dict[str, Any]:
    implementation = _manifest_implementation(manifest_schema_path)
    receipt_schema_hash = _sha256(
        _read_bounded(
            receipt_schema_path,
            max_raw_bytes=MAX_SCHEMA_RAW_BYTES,
            label="receipt schema",
        )
    )
    _require(
        receipt_schema_hash == CHECKED_RECEIPT_SCHEMA_FILE_SHA256,
        "receipt schema raw hash differs from the checked-in v1 schema",
    )
    return {**implementation, "receipt_schema_sha256": receipt_schema_hash}


def _validate_against_schema(
    value: dict[str, Any],
    schema_path: pathlib.Path,
    label: str,
) -> None:
    schema, _ = _load_json(
        schema_path,
        max_raw_bytes=MAX_SCHEMA_RAW_BYTES,
        label=f"{label} schema",
    )
    try:
        validator = draft202012.Validator(
            [draft202012.SchemaDocument(schema_path.name, schema)]
        )
        validator.validate(value, schema_path.name, label=label)
    except draft202012.SchemaError as exc:
        raise GatePrimitiveError(f"{label} schema validation failed: {exc}") from exc


def build_cache_seed_manifest(
    *,
    plan: dict[str, Any],
    manifest_schema_path: pathlib.Path,
    archive_file: str,
    archive_sha256: str,
    archive_byte_count: int,
    entries: Sequence[Mapping[str, Any]],
) -> dict[str, Any]:
    entries_type = type(entries)
    _require(
        entries_type is list or entries_type is tuple,
        EXACT_JSON_TYPES_ERROR,
    )
    _require(
        3 <= len(entries) <= MAX_MANIFEST_FILE_COUNT,
        "manifest file count exceeds the frozen profile ceiling",
    )
    _validate_json_profile(entries)
    _validate_json_profile(archive_file)
    _validate_json_profile(archive_sha256)
    _validate_json_profile(archive_byte_count)
    authority = _pending_authority(plan)
    maximum_seed_bytes = _integer(
        plan["resource_budget"]["max_cache_seed_bytes"],
        "max_cache_seed_bytes",
        minimum=1,
    )
    _artifact_file(archive_file, "archive_file")
    _require(_valid_sha(archive_sha256), "archive_sha256 is invalid")
    _require(
        1 <= _integer(archive_byte_count, "archive_byte_count", minimum=1)
        <= maximum_seed_bytes,
        "cache archive exceeds the frozen byte ceiling",
    )
    expected_entry_fields = {"byte_count", "path", "repository_key", "sha256"}
    normalized_entries: list[dict[str, Any]] = []
    scalar_byte_count = 0
    unpacked_byte_count = 0
    for index, entry in enumerate(entries):
        _require(
            type(entry) is dict and set(entry) == expected_entry_fields,
            f"entry[{index}] fields differ",
        )
        key = entry["repository_key"]
        _require(key in REPOSITORY_KEYS, f"entry[{index}].repository_key is invalid")
        path = _cache_path(entry["path"], f"entry[{index}].path")
        _require(_valid_sha(entry["sha256"]), f"entry[{index}].sha256 is invalid")
        byte_count = _integer(
            entry["byte_count"],
            f"entry[{index}].byte_count",
            minimum=1,
        )
        _require(
            byte_count <= maximum_seed_bytes,
            f"entry[{index}].byte_count exceeds the frozen byte ceiling",
        )
        unpacked_byte_count += byte_count
        _require(
            unpacked_byte_count <= maximum_seed_bytes,
            "unpacked cache seed exceeds the frozen byte ceiling",
        )
        scalar_byte_count += len(path.encode("utf-8")) + len(cast(str, key).encode("utf-8")) + 64
        _require(
            scalar_byte_count <= MAX_MANIFEST_RAW_BYTES,
            "manifest scalar bytes exceed the frozen profile ceiling",
        )
        normalized_entries.append(
            {
                "byte_count": byte_count,
                "path": path,
                "repository_key": key,
                "sha256": entry["sha256"],
            }
        )
    counts = Counter(
        key if type(key) is str else None
        for entry in normalized_entries
        for key in (entry.get("repository_key"),)
    )
    contents = {
        "entries": normalized_entries,
        "file_count": len(normalized_entries),
        "inventory_sha256": _canonical_hash(normalized_entries),
        "repository_file_counts": {key: counts[key] for key in REPOSITORY_KEYS},
        "unpacked_byte_count": unpacked_byte_count,
    }
    manifest = {
        "archive": {
            "artifact_file": archive_file,
            "byte_count": archive_byte_count,
            "sha256": archive_sha256,
        },
        "authority": authority,
        "contents": contents,
        "host_shared_cache_reuse": "forbidden",
        "implementation": _manifest_implementation(manifest_schema_path),
        "manifest_sha256": None,
        "network_dependency_resolution": "forbidden",
        "profile": CACHE_MANIFEST_PROFILE,
        "run_plan": {
            "plan_sha256": plan["plan_sha256"],
            "profile": plan["profile"],
            "schema_version": plan["schema_version"],
            "toolchain_bindings_sha256": _canonical_hash(_toolchain_projection(plan)),
        },
        "schema_version": SCHEMA_VERSION,
        "status": MANIFEST_STATUS,
    }
    _validate_json_profile(manifest)
    manifest["manifest_sha256"] = _self_hash(manifest, "manifest_sha256")
    validate_cache_seed_manifest(
        manifest,
        plan=plan,
        manifest_schema_path=manifest_schema_path,
    )
    return manifest


def validate_cache_seed_manifest(
    value: dict[str, Any],
    *,
    plan: dict[str, Any],
    manifest_schema_path: pathlib.Path,
) -> None:
    expected_root = {
        "archive", "authority", "contents", "host_shared_cache_reuse", "implementation",
        "manifest_sha256", "network_dependency_resolution", "profile", "run_plan",
        "schema_version", "status",
    }
    _require(type(value) is dict, EXACT_JSON_TYPES_ERROR)
    _require(all(type(key) is str for key in value), EXACT_JSON_KEYS_ERROR)
    _require(set(value) == expected_root, "manifest root fields differ")
    contents = value["contents"]
    _require(type(contents) is dict, EXACT_JSON_TYPES_ERROR)
    _require(all(type(key) is str for key in contents), EXACT_JSON_KEYS_ERROR)
    _require(
        set(contents)
        == {"entries", "file_count", "inventory_sha256", "repository_file_counts", "unpacked_byte_count"},
        "manifest contents fields differ",
    )
    entries = contents["entries"]
    _require(type(entries) is list, EXACT_JSON_TYPES_ERROR)
    _require(bool(entries), "manifest entries must be a non-empty list")
    _require(
        3 <= len(entries) <= MAX_MANIFEST_FILE_COUNT,
        "manifest file count exceeds the frozen profile ceiling",
    )
    _validate_json_profile(value)
    authority = _pending_authority(plan)
    _require(value["profile"] == CACHE_MANIFEST_PROFILE and value["schema_version"] == 1, "manifest profile differs")
    _require(value["status"] == MANIFEST_STATUS, "manifest status differs")
    _require(value["authority"] == authority, "manifest authority boundary differs")
    _require(value["host_shared_cache_reuse"] == "forbidden", "host cache reuse is not forbidden")
    _require(value["network_dependency_resolution"] == "forbidden", "network resolution is not forbidden")
    _require(_valid_sha(value["manifest_sha256"]), "manifest_sha256 is invalid")

    _require(value["implementation"] == _manifest_implementation(manifest_schema_path), "manifest implementation binding differs")
    expected_run_plan = {
        "plan_sha256": plan["plan_sha256"],
        "profile": plan["profile"],
        "schema_version": plan["schema_version"],
        "toolchain_bindings_sha256": _canonical_hash(_toolchain_projection(plan)),
    }
    _require(value["run_plan"] == expected_run_plan, "manifest run-plan binding differs")

    archive = value["archive"]
    _require(type(archive) is dict and set(archive) == {"artifact_file", "byte_count", "sha256"}, "manifest archive fields differ")
    _artifact_file(archive["artifact_file"], "archive.artifact_file")
    _require(_valid_sha(archive["sha256"]), "archive.sha256 is invalid")
    archive_bytes = _integer(archive["byte_count"], "archive.byte_count", minimum=1)

    maximum_seed_bytes = _integer(plan["resource_budget"]["max_cache_seed_bytes"], "max_cache_seed_bytes", minimum=1)
    _require(archive_bytes <= maximum_seed_bytes, "cache archive exceeds the frozen byte ceiling")

    declared_file_count = _integer(contents["file_count"], "contents.file_count", minimum=3)
    _require(declared_file_count <= MAX_MANIFEST_FILE_COUNT, "manifest file count exceeds the frozen profile ceiling")
    _require(declared_file_count == len(entries), "manifest file count differs")
    _require(_valid_sha(contents["inventory_sha256"]), "manifest inventory hash is invalid")
    declared_unpacked_bytes = _integer(
        contents["unpacked_byte_count"],
        "contents.unpacked_byte_count",
        minimum=3,
    )
    _require(
        declared_unpacked_bytes <= maximum_seed_bytes,
        "unpacked cache seed exceeds the frozen byte ceiling",
    )
    expected_entry_fields = {"byte_count", "path", "repository_key", "sha256"}
    identities: list[tuple[str, str]] = []
    archive_paths: list[str] = []
    portable_paths: list[tuple[str, ...]] = []
    unpacked_bytes = 0
    scalar_byte_count = 0
    repository_counts: Counter[str] = Counter()
    for index, entry in enumerate(entries):
        _require(type(entry) is dict and set(entry) == expected_entry_fields, f"entry[{index}] fields differ")
        exact_entry = cast(dict[str, Any], entry)
        key_value = exact_entry["repository_key"]
        _require(
            type(key_value) is str and key_value in REPOSITORY_KEYS,
            f"entry[{index}].repository_key is invalid",
        )
        key = cast(str, key_value)
        path = _cache_path(exact_entry["path"], f"entry[{index}].path")
        _require(_valid_sha(exact_entry["sha256"]), f"entry[{index}].sha256 is invalid")
        entry_byte_count = _integer(
            exact_entry["byte_count"],
            f"entry[{index}].byte_count",
            minimum=1,
        )
        _require(
            entry_byte_count <= maximum_seed_bytes,
            f"entry[{index}].byte_count exceeds the frozen byte ceiling",
        )
        unpacked_bytes += entry_byte_count
        scalar_byte_count += len(path.encode("utf-8")) + len(key.encode("utf-8")) + 64
        _require(
            scalar_byte_count <= MAX_MANIFEST_RAW_BYTES,
            "manifest scalar bytes exceed the frozen profile ceiling",
        )
        identities.append((key, path))
        archive_paths.append(path)
        portable_paths.append(_portable_path_key(path))
        repository_counts[key] += 1
    _require(identities == sorted(identities), "manifest entries are not canonical")
    _require(len(archive_paths) == len(set(archive_paths)), "manifest archive paths collide globally")
    portable_path_set = set(portable_paths)
    _require(
        len(portable_paths) == len(portable_path_set),
        "manifest archive paths collide under the portable path key",
    )
    _require(
        not any(
            portable_path[:component_count] in portable_path_set
            for portable_path in portable_path_set
            for component_count in range(1, len(portable_path))
        ),
        "manifest archive paths contain a portable ancestor conflict",
    )
    _require(all(repository_counts[key] > 0 for key in REPOSITORY_KEYS), "manifest does not cover every repository cache")
    expected_counts = {key: repository_counts[key] for key in REPOSITORY_KEYS}
    _require(contents["repository_file_counts"] == expected_counts, "manifest repository counts differ")
    _require(declared_unpacked_bytes == unpacked_bytes, "manifest unpacked byte count differs")
    _require(contents["inventory_sha256"] == _canonical_hash(entries), "manifest inventory hash differs")

    _require(unpacked_bytes <= maximum_seed_bytes, "unpacked cache seed exceeds the frozen byte ceiling")
    _validate_manifest_profile_limits(
        file_count=len(entries),
        rendered_byte_count=len(_render(value)),
    )
    _require(value["manifest_sha256"] == _self_hash(value, "manifest_sha256"), "manifest self hash mismatch")
    _validate_against_schema(value, manifest_schema_path, "cache manifest")


def _validate_archive_observation(
    observation: Mapping[str, Any],
    manifest: dict[str, Any],
) -> dict[str, Any]:
    _validate_json_profile(observation)
    _require(type(observation) is dict, EXACT_JSON_TYPES_ERROR)
    exact_observation = cast(dict[str, Any], observation)
    expected_fields = {
        "archive_sha256", "archive_byte_count", "content_inventory_sha256",
        "file_count", "observation_kind", "unpacked_byte_count",
    }
    _require(
        set(exact_observation) == expected_fields,
        "cache archive observation fields differ",
    )
    archive = manifest["archive"]
    contents = manifest["contents"]
    expected = {
        "archive_byte_count": archive["byte_count"],
        "archive_sha256": archive["sha256"],
        "content_inventory_sha256": contents["inventory_sha256"],
        "file_count": contents["file_count"],
        "observation_kind": CACHE_ARCHIVE_OBSERVATION_KIND,
        "unpacked_byte_count": contents["unpacked_byte_count"],
    }
    observed = exact_observation.copy()
    _require(observed == expected, "cache archive observation differs from manifest")
    return expected


def evaluate_injected_resource_stats(
    plan: dict[str, Any],
    filesystem_stats: Mapping[str, Any],
) -> dict[str, Any]:
    _validate_json_profile(filesystem_stats)
    _require(type(filesystem_stats) is dict, EXACT_JSON_TYPES_ERROR)
    exact_filesystem_stats = cast(dict[str, Any], filesystem_stats)
    _pending_authority(plan)
    _require(
        set(exact_filesystem_stats) == {"available_blocks", "fragment_size_bytes"},
        "filesystem stats fields differ",
    )
    fragment_size = _integer(
        exact_filesystem_stats["fragment_size_bytes"],
        "fragment_size_bytes",
        minimum=1,
        maximum=MAX_RESOURCE_FRAGMENT_SIZE_BYTES,
    )
    available_blocks = _integer(
        exact_filesystem_stats["available_blocks"],
        "available_blocks",
        maximum=MAX_RESOURCE_AVAILABLE_BLOCKS,
    )
    _require(
        available_blocks <= MAX_RESOURCE_DERIVED_BYTES // fragment_size,
        "filesystem free-byte multiplication exceeds the JSON integer ceiling",
    )
    free_bytes = fragment_size * available_blocks
    budget = plan["resource_budget"]
    reserve = _integer(budget["minimum_free_disk_reserve_bytes"], "minimum_free_disk_reserve_bytes", minimum=1)
    staging = _integer(budget["max_total_staging_bytes"], "max_total_staging_bytes", minimum=1)
    required = _integer(budget["minimum_free_disk_before_staging_bytes"], "minimum_free_disk_before_staging_bytes", minimum=1)
    _require(
        reserve <= MAX_RESOURCE_DERIVED_BYTES - staging,
        "frozen free-space arithmetic exceeds the JSON integer ceiling",
    )
    _require(required == reserve + staging, "frozen free-space arithmetic differs")
    _require(
        required <= MAX_RESOURCE_DERIVED_BYTES,
        "frozen free-space threshold exceeds the JSON integer ceiling",
    )
    _require(free_bytes >= required, "free space is below the frozen 16 GiB preflight threshold")
    _require(free_bytes - staging >= reserve, "staging would consume the frozen free-space reserve")
    resource = {
        "available_blocks": available_blocks,
        "free_bytes": free_bytes,
        "fragment_size_bytes": fragment_size,
        "headroom_bytes": free_bytes - required,
        "minimum_required_bytes": required,
        "observation_kind": FILESYSTEM_OBSERVATION_KIND,
        "reserve_bytes": reserve,
        "staging_ceiling_bytes": staging,
    }
    _validate_json_profile(resource)
    return resource


def build_preflight_receipt(
    *,
    plan: dict[str, Any],
    plan_raw: bytes,
    manifest: dict[str, Any],
    manifest_raw: bytes,
    manifest_schema_path: pathlib.Path,
    receipt_schema_path: pathlib.Path,
    archive_observation: Mapping[str, Any],
    filesystem_stats: Mapping[str, Any],
) -> dict[str, Any]:
    plan_raw = _exact_bytes(plan_raw, "run-plan raw input")
    manifest_raw = _exact_bytes(manifest_raw, "manifest raw input")
    _validate_json_profile(archive_observation)
    _require(type(archive_observation) is dict, EXACT_JSON_TYPES_ERROR)
    _validate_json_profile(filesystem_stats)
    _require(type(filesystem_stats) is dict, EXACT_JSON_TYPES_ERROR)
    authority = _pending_authority(plan)
    _require(len(plan_raw) <= MAX_PLAN_RAW_BYTES, "run-plan bytes exceed the raw-byte ceiling")
    _require(_sha256(plan_raw) == CHECKED_PLAN_FILE_SHA256, "run-plan raw hash differs")
    _require(plan_raw == run_plan_v2._render(plan), "run-plan bytes are not canonical")
    validate_cache_seed_manifest(manifest, plan=plan, manifest_schema_path=manifest_schema_path)
    _require(len(manifest_raw) <= MAX_MANIFEST_RAW_BYTES, "cache manifest exceeds the raw-byte ceiling")
    _require(manifest_raw == _render(manifest), "cache manifest bytes are not canonical")
    cache = _validate_archive_observation(archive_observation, manifest)
    resource = evaluate_injected_resource_stats(plan, filesystem_stats)
    receipt = {
        "authority": authority,
        "cache_seed": {
            **cache,
            "binding_status": CACHE_BINDING_STATUS,
            "manifest_sha256": manifest["manifest_sha256"],
        },
        "checks": list(CHECKS),
        "execution_status": "forbidden_missing_remaining_gates",
        "implementation": _receipt_implementation(manifest_schema_path, receipt_schema_path),
        "inputs": {
            "manifest_file_sha256": _sha256(manifest_raw),
            "plan_file_sha256": _sha256(plan_raw),
            "plan_sha256": plan["plan_sha256"],
        },
        "profile": PREFLIGHT_RECEIPT_PROFILE,
        "receipt_sha256": None,
        "residual_gates": list(RESIDUAL_GATES),
        "resource": resource,
        "schema_version": SCHEMA_VERSION,
        "status": RECEIPT_STATUS,
    }
    _validate_json_profile(receipt)
    receipt["receipt_sha256"] = _self_hash(receipt, "receipt_sha256")
    validate_preflight_receipt(
        receipt,
        plan=plan,
        plan_raw=plan_raw,
        manifest=manifest,
        manifest_raw=manifest_raw,
        manifest_schema_path=manifest_schema_path,
        receipt_schema_path=receipt_schema_path,
    )
    return receipt


def validate_preflight_receipt(
    value: dict[str, Any],
    *,
    plan: dict[str, Any],
    plan_raw: bytes,
    manifest: dict[str, Any],
    manifest_raw: bytes,
    manifest_schema_path: pathlib.Path,
    receipt_schema_path: pathlib.Path,
) -> None:
    _validate_json_profile(value)
    plan_raw = _exact_bytes(plan_raw, "run-plan raw input")
    manifest_raw = _exact_bytes(manifest_raw, "manifest raw input")
    authority = _pending_authority(plan)
    _require(len(plan_raw) <= MAX_PLAN_RAW_BYTES, "run-plan bytes exceed the raw-byte ceiling")
    _require(_sha256(plan_raw) == CHECKED_PLAN_FILE_SHA256, "run-plan raw hash differs")
    _require(plan_raw == run_plan_v2._render(plan), "run-plan bytes are not canonical")
    validate_cache_seed_manifest(manifest, plan=plan, manifest_schema_path=manifest_schema_path)
    _require(len(manifest_raw) <= MAX_MANIFEST_RAW_BYTES, "cache manifest exceeds the raw-byte ceiling")
    _require(manifest_raw == _render(manifest), "cache manifest bytes are not canonical")
    expected_root = {
        "authority", "cache_seed", "checks", "execution_status", "implementation", "inputs",
        "profile", "receipt_sha256", "residual_gates", "resource", "schema_version", "status",
    }
    _require(type(value) is dict and set(value) == expected_root, "preflight receipt root fields differ")
    _require(value["profile"] == PREFLIGHT_RECEIPT_PROFILE and value["schema_version"] == 1, "preflight receipt profile differs")
    _require(value["status"] == RECEIPT_STATUS, "preflight receipt status differs")
    _require(value["authority"] == authority, "preflight receipt authority differs")
    _require(value["checks"] == CHECKS, "preflight receipt checks differ")
    _require(value["execution_status"] == "forbidden_missing_remaining_gates", "preflight receipt overclaims execution authority")
    _require(value["residual_gates"] == RESIDUAL_GATES, "preflight receipt residual gates differ")
    _require(value["implementation"] == _receipt_implementation(manifest_schema_path, receipt_schema_path), "preflight receipt implementation differs")
    _require(value["inputs"] == {
        "manifest_file_sha256": _sha256(manifest_raw),
        "plan_file_sha256": _sha256(plan_raw),
        "plan_sha256": plan["plan_sha256"],
    }, "preflight receipt input bindings differ")
    cache_seed = value["cache_seed"]
    _require(type(cache_seed) is dict, "preflight receipt cache binding is invalid")
    expected_cache = {
        **_validate_archive_observation(
            {
                "archive_byte_count": cache_seed.get("archive_byte_count"),
                "archive_sha256": cache_seed.get("archive_sha256"),
                "content_inventory_sha256": cache_seed.get("content_inventory_sha256"),
                "file_count": cache_seed.get("file_count"),
                "observation_kind": cache_seed.get("observation_kind"),
                "unpacked_byte_count": cache_seed.get("unpacked_byte_count"),
            },
            manifest,
        ),
        "binding_status": CACHE_BINDING_STATUS,
        "manifest_sha256": manifest["manifest_sha256"],
    }
    _require(value["cache_seed"] == expected_cache, "preflight receipt cache binding differs")
    resource = value["resource"]
    _require(type(resource) is dict, "preflight receipt resource is invalid")
    observed_resource = evaluate_injected_resource_stats(
        plan,
        {
            "available_blocks": resource.get("available_blocks"),
            "fragment_size_bytes": resource.get("fragment_size_bytes"),
        },
    )
    _require(resource == observed_resource, "preflight receipt resource arithmetic differs")
    _require(_valid_sha(value["receipt_sha256"]), "receipt_sha256 is invalid")
    _require(
        len(_render(value)) <= MAX_RECEIPT_RAW_BYTES,
        "preflight receipt rendered bytes exceed the raw-byte ceiling",
    )
    _require(value["receipt_sha256"] == _self_hash(value, "receipt_sha256"), "preflight receipt self hash mismatch")
    _validate_against_schema(value, receipt_schema_path, "preflight receipt")


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="command", required=True)
    manifest_check = subparsers.add_parser(
        "check-manifest",
        help="check canonical cache-manifest bytes and exact dependencies",
    )
    manifest_check.add_argument("manifest", type=pathlib.Path)
    manifest_check.add_argument("--plan", required=True, type=pathlib.Path)
    manifest_check.add_argument("--manifest-schema", required=True, type=pathlib.Path)
    receipt_check = subparsers.add_parser(
        "check-receipt",
        help="check canonical primitive-receipt bytes and exact inputs",
    )
    receipt_check.add_argument("receipt", type=pathlib.Path)
    receipt_check.add_argument("--plan", required=True, type=pathlib.Path)
    receipt_check.add_argument("--manifest", required=True, type=pathlib.Path)
    receipt_check.add_argument("--manifest-schema", required=True, type=pathlib.Path)
    receipt_check.add_argument("--receipt-schema", required=True, type=pathlib.Path)
    args = parser.parse_args(argv)
    try:
        plan, plan_raw = _load_json(
            args.plan,
            max_raw_bytes=MAX_PLAN_RAW_BYTES,
            label="run plan",
        )
        _require(_sha256(plan_raw) == CHECKED_PLAN_FILE_SHA256, "run-plan raw hash differs")
        _require(plan_raw == run_plan_v2._render(plan), "run-plan bytes are not canonical")
        manifest, manifest_raw = _load_json(
            args.manifest,
            max_raw_bytes=MAX_MANIFEST_RAW_BYTES,
            label="cache manifest",
        )
        _require(manifest_raw == _render(manifest), "cache manifest bytes are not canonical")
        if args.command == "check-manifest":
            validate_cache_seed_manifest(
                manifest,
                plan=plan,
                manifest_schema_path=args.manifest_schema,
            )
        else:
            receipt, receipt_raw = _load_json(
                args.receipt,
                max_raw_bytes=MAX_RECEIPT_RAW_BYTES,
                label="preflight receipt",
            )
            _require(receipt_raw == _render(receipt), "preflight receipt bytes are not canonical")
            validate_preflight_receipt(
                receipt,
                plan=plan,
                plan_raw=plan_raw,
                manifest=manifest,
                manifest_raw=manifest_raw,
                manifest_schema_path=args.manifest_schema,
                receipt_schema_path=args.receipt_schema,
            )
    except (GatePrimitiveError, run_plan_v2.RunPlanError) as exc:
        parser.error(str(exc))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
