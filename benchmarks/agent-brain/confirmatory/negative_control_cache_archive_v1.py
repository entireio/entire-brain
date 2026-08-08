#!/usr/bin/env python3
"""Build, check, and operate the non-authorizing cache-archive identity verifier.

The checked material declaration is deliberately pending.  Production
``verify`` therefore fails before opening either caller-supplied locator.  A
future source-reviewed declaration may bind one canonical cache manifest and
one opaque archive by exact raw identities.  Successful verification still
does not parse, extract, stage, authorize, or execute anything.

Only Python's standard library is imported normally.  The checked Draft
2020-12 validator is loaded from its exact source bytes so an import cache
cannot replace a reviewed local dependency.
"""

from __future__ import annotations

import argparse
import copy
import hashlib
import json
import os
import pathlib
import re
import stat
import sys
import tempfile
import types
import unicodedata
from collections import Counter
from collections.abc import Mapping, Sequence
from typing import Any, BinaryIO, cast


ROOT = pathlib.Path(__file__).parent
EXECUTION_CONTRACT_PATH = ROOT / "development-task-negative-control-execution-contract-v1.json"
RUN_PLAN_PATH = ROOT / "development-task-negative-control-run-plan-v2.json"
EXECUTION_CONTRACT_BUILDER_PATH = ROOT / "negative_control_execution_contract_v1.py"
RUN_PLAN_BUILDER_PATH = ROOT / "task_negative_control_plan_v2.py"
GATE_PRIMITIVE_PATH = ROOT / "task_negative_control_gate_v1.py"
MATERIAL_PATH = ROOT / "negative-control-cache-archive-material-v1.json"
VERIFIER_CONTRACT_PATH = ROOT / "negative-control-cache-archive-verifier-contract-v1.json"
DRAFT202012_PATH = ROOT / "draft202012.py"
EXECUTION_SCHEMA_PATH = ROOT / "schemas" / "negative-control-execution-contract-v1.schema.json"
RUN_PLAN_SCHEMA_PATH = ROOT / "schemas" / "development-task-negative-control-run-plan-v2.schema.json"
MANIFEST_SCHEMA_PATH = ROOT / "schemas" / "offline-go-cache-seed-manifest-v1.schema.json"
MATERIAL_SCHEMA_PATH = ROOT / "schemas" / "negative-control-cache-archive-material-v1.schema.json"
REPORT_SCHEMA_PATH = ROOT / "schemas" / "negative-control-cache-archive-verification-report-v1.schema.json"
VERIFIER_CONTRACT_SCHEMA_PATH = ROOT / "schemas" / "negative-control-cache-archive-verifier-contract-v1.schema.json"

SCHEMA_VERSION = 1
MATERIAL_PROFILE = "agent_brain_negative_control_cache_archive_material_v1"
VERIFIER_PROFILE = "agent_brain_negative_control_cache_archive_verifier_contract_v1"
REPORT_PROFILE = "agent_brain_negative_control_cache_archive_verification_report_v1"
MANIFEST_PROFILE = "content_addressed_offline_go_cache_seed_manifest_v1"
PENDING_MATERIAL_STATUS = "pending_source_review"
APPROVED_MATERIAL_STATUS = "approved_source_review_identity_only"
PENDING_VERIFIER_STATUS = "verifier_compiled_material_source_review_pending_execution_forbidden"
APPROVED_VERIFIER_STATUS = "verifier_compiled_material_source_reviewed_identity_only_execution_forbidden"
REPORT_STATUS = "archive_identity_verified_content_unparsed_staging_forbidden"
IDENTITY_BINDING_STATUS = "source_reviewed_actual_cache_manifest_and_archive_raw_identity"
E0_BINDING_STATUS = "identity_verified_not_bound_to_e0_runtime"
OPAQUE_ARCHIVE_INTERPRETATION = "opaque_bytes_content_safety_not_verified"
CANONICAL_ARTIFACT_PROFILE = "sorted_keys_indent_2_utf8_lf_v1"
CANONICAL_JSON_PROFILE = "sorted_keys_compact_utf8_no_float_v1"

CHECKED_EXECUTION_SOURCE_COMMIT = "f0552070921605cd9a0165ee29ca96e100675d58"
CHECKED_EXECUTION_ARTIFACT_SHA256 = "94422af8a5e24c6a855b66cf40868565ee3518d0c75eb5343b5d2450523b2c1e"
CHECKED_EXECUTION_CONTRACT_SHA256 = "1e290b91d5e0be42f750bf0e655edee48d297f86ad3e3867b8fe9177fd5125ef"
CHECKED_EXECUTION_SCHEMA_SHA256 = "dbcb95119feeda4c436e90d8162ac0498577454ca9ccaab7a8254d55d90359f9"
CHECKED_EXECUTION_BUILDER_SHA256 = "f3cfb82978b0ce98d17999ef4cb4f4089a425bb528ece4ff53b61a03a4344bcd"
CHECKED_RUN_PLAN_ARTIFACT_SHA256 = "f55b6e22a25daf8016305304adf3b7ac8d31675281d97cfc9bb78cc76b619790"
CHECKED_RUN_PLAN_SHA256 = "a47311fa1f4553f8085ebea6e8ddd003a4ea5d0b2d269d91bbcdd414134a248e"
CHECKED_RUN_PLAN_SCHEMA_SHA256 = "294f77f165676bec6053e9637578ca22f048a887d2a978bbf8b8e300f21a2dad"
CHECKED_RUN_PLAN_BUILDER_SHA256 = "23d08e9d3d23935cdc86b53780b609fb35f6b3020ede6b1360af288939e34715"
CHECKED_GATE_PRIMITIVE_SHA256 = "f04b8634caf619cc17bb495975d2b5358164be6eae2a124ade84efebd60c51dd"
CHECKED_MANIFEST_SCHEMA_SHA256 = "37d3839411b2e30a99fdd7e93784d56f5fd525c0472717a24cc7b92213b98999"
CHECKED_TOOLCHAIN_BINDINGS_SHA256 = "e8240c5af77530079643593a7484cb0062408829bab76690b7f55d12176fc87c"
CHECKED_DRAFT202012_SHA256 = "8688b67468b096427f758163174432e221d8197c6b13870a6427939f6ea281eb"
CHECKED_DRAFT202012_SIZE = 18_097

# Filled with the reviewed raw identities after the concurrently authored
# schemas and pending material declaration settle.  A mismatched dependency
# always fails closed; there is no runtime override.
CHECKED_MATERIAL_ARTIFACT_SHA256 = "7527cec7b2f1b47498845ae38710369997b7c82d2d0150761b23d2b594f68d93"
CHECKED_MATERIAL_SHA256 = "f03ae113a29585a9aba0c8ce57a41ab61eb0b2cd682474f1df2d214e299a4852"
CHECKED_MATERIAL_SCHEMA_SHA256 = "1424cd22b5eb057b5794347403780435f95c0428d4f81ff42441e71abc39d5d5"
CHECKED_REPORT_SCHEMA_SHA256 = "5827c78df73b024df32bf2853d412df428e7362e869d5d202b33dac15bbe4e5d"
CHECKED_VERIFIER_CONTRACT_SCHEMA_SHA256 = "c35c0de98dbdcec6744a3fe33ee363261507eb894bbddbb3af5af3642be7f914"

REPOSITORY_ORDER = ("entire-brain", "entire-db", "entire-graph")
CACHE_ROOTS = ("gocache", "gomodcache")
MAX_ARCHIVE_BYTES = 2 * 1024 * 1024 * 1024
MAX_UNPACKED_BYTES = 2 * 1024 * 1024 * 1024
MAX_MANIFEST_FILE_COUNT = 250_000
MAX_FIXED_JSON_BYTES = 128 * 1024 * 1024
MAX_SCHEMA_BYTES = 4 * 1024 * 1024
MAX_SOURCE_BYTES = 4 * 1024 * 1024
MAX_VERIFIER_CONTRACT_BYTES = 4 * 1024 * 1024
MAX_JSON_DEPTH = 64
MAX_JSON_NODES = MAX_MANIFEST_FILE_COUNT * 5 + 20_000
MAX_INTEGER_DIGITS = 64
MAX_INTEGER = 10**MAX_INTEGER_DIGITS - 1
SHA256_RE = re.compile(r"^(?!0{64}$)[0-9a-f]{64}$")

_CHECKED_DRAFT_VALIDATOR: types.ModuleType | None = None


class CacheArchiveVerificationError(RuntimeError):
    """Raised when any checked identity or non-authorizing claim differs."""


def _require(condition: bool, message: str) -> None:
    if not condition:
        raise CacheArchiveVerificationError(message)


def _sha256(raw: bytes) -> str:
    _require(type(raw) is bytes, "SHA-256 input must be exact built-in bytes")
    return hashlib.sha256(raw).hexdigest()


def _valid_sha(value: Any) -> bool:
    return type(value) is str and SHA256_RE.fullmatch(cast(str, value)) is not None


def _validate_json_profile(value: Any) -> None:
    stack: list[tuple[Any, int]] = [(value, 1)]
    seen: set[int] = set()
    nodes = 0
    while stack:
        current, depth = stack.pop()
        nodes += 1
        _require(nodes <= MAX_JSON_NODES, "JSON exceeds the node ceiling")
        _require(depth <= MAX_JSON_DEPTH, "JSON exceeds the nesting ceiling")
        current_type = type(current)
        if current_type in {type(None), bool, int, str}:
            if current_type is int:
                _require(
                    abs(cast(int, current)) <= MAX_INTEGER,
                    "JSON integer exceeds the digit ceiling",
                )
            if current_type is str:
                _require(
                    not any(0xD800 <= ord(character) <= 0xDFFF for character in cast(str, current)),
                    "JSON string contains a surrogate code point",
                )
            continue
        _require(current_type in {dict, list}, "JSON contains an unsupported value type")
        identity = id(current)
        _require(identity not in seen, "JSON contains a cycle or aliased container")
        seen.add(identity)
        if current_type is dict:
            mapping = cast(dict[Any, Any], current)
            _require(all(type(key) is str for key in mapping), "JSON object key is not a string")
            stack.extend((child, depth + 1) for child in mapping.values())
        else:
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
        raise CacheArchiveVerificationError("value is not canonical JSON") from exc


def _canonical_hash(value: Any) -> str:
    return _sha256(_canonical_bytes(value))


def _render(value: Any) -> bytes:
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
        raise CacheArchiveVerificationError("value is not renderable canonical JSON") from exc


def _field_self_hash(value: Mapping[str, Any], field: str) -> str:
    projected = copy.deepcopy(dict(value))
    _require(field in projected, f"{field} is missing")
    projected[field] = None
    return _canonical_hash(projected)


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
    raise CacheArchiveVerificationError("floating-point JSON is forbidden")


def _reject_constant(_text: str) -> None:
    raise CacheArchiveVerificationError("non-finite JSON is forbidden")


def _parse_json(raw: bytes, *, label: str) -> dict[str, Any]:
    try:
        value = json.loads(
            raw.decode("utf-8"),
            object_pairs_hook=_reject_duplicate_pairs,
            parse_constant=_reject_constant,
            parse_float=_reject_float,
            parse_int=_parse_integer,
        )
        _validate_json_profile(value)
    except CacheArchiveVerificationError:
        raise
    except (UnicodeError, json.JSONDecodeError, ValueError, RecursionError) as exc:
        raise CacheArchiveVerificationError(f"cannot parse {label}") from exc
    _require(type(value) is dict, f"{label} root is not an object")
    return cast(dict[str, Any], value)


def _open_regular_descriptor(path: pathlib.Path, *, label: str) -> tuple[int, list[int]]:
    _require(path.anchor in ("", "/"), f"{label} path anchor is unsupported")
    components = path.parts[1:] if path.is_absolute() else path.parts
    _require(bool(components), f"{label} path has no file component")
    _require(
        all(component not in ("", ".", "..") and "\0" not in component for component in components),
        f"{label} path contains a forbidden traversal component",
    )
    _require(os.open in os.supports_dir_fd, "descriptor-relative secure open is unavailable")

    file_flags = os.O_RDONLY
    directory_flags = os.O_RDONLY
    for flag_name in ("O_CLOEXEC", "O_NOFOLLOW", "O_NONBLOCK"):
        flag = getattr(os, flag_name, None)
        _require(type(flag) is int and flag != 0, f"secure open flag {flag_name} is unavailable")
        file_flags |= cast(int, flag)
        directory_flags |= cast(int, flag)
    directory_flag = getattr(os, "O_DIRECTORY", None)
    _require(type(directory_flag) is int and directory_flag != 0, "secure open flag O_DIRECTORY is unavailable")
    directory_flags |= cast(int, directory_flag)

    directories: list[int] = []
    descriptor: int | None = None
    try:
        directory_descriptor = os.open("/" if path.is_absolute() else ".", directory_flags)
        directories.append(directory_descriptor)
        for component in components[:-1]:
            directory_descriptor = os.open(component, directory_flags, dir_fd=directory_descriptor)
            directories.append(directory_descriptor)
            _require(
                stat.S_ISDIR(os.fstat(directory_descriptor).st_mode),
                f"{label} ancestor is not a directory",
            )
        descriptor = os.open(components[-1], file_flags, dir_fd=directory_descriptor)
        _require(stat.S_ISREG(os.fstat(descriptor).st_mode), f"{label} is not a regular file")
        return descriptor, directories
    except CacheArchiveVerificationError:
        if descriptor is not None:
            os.close(descriptor)
        for directory_descriptor in reversed(directories):
            os.close(directory_descriptor)
        raise
    except OSError as exc:
        if descriptor is not None:
            os.close(descriptor)
        for directory_descriptor in reversed(directories):
            os.close(directory_descriptor)
        raise CacheArchiveVerificationError(f"cannot open {label}") from exc


def _stable_stat_identity(value: os.stat_result) -> tuple[int, ...]:
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


def _read_bounded(
    path: pathlib.Path,
    *,
    maximum: int,
    label: str,
    require_single_link: bool = False,
) -> bytes:
    _require(type(maximum) is int and maximum >= 1, f"{label} byte ceiling is invalid")
    descriptor, directories = _open_regular_descriptor(path, label=label)
    try:
        before = os.fstat(descriptor)
        _require(before.st_size <= maximum, f"{label} exceeds the byte ceiling")
        if require_single_link:
            _require(before.st_nlink == 1, f"{label} link count differs")
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
        _require(
            len(raw) <= maximum
            and len(raw) == before.st_size
            and _stable_stat_identity(before) == _stable_stat_identity(after),
            f"{label} changed while being read",
        )
        return raw
    except CacheArchiveVerificationError:
        raise
    except OSError as exc:
        raise CacheArchiveVerificationError(f"cannot read {label}") from exc
    finally:
        os.close(descriptor)
        for directory_descriptor in reversed(directories):
            os.close(directory_descriptor)


def _stream_identity(path: pathlib.Path, *, maximum: int, label: str) -> dict[str, Any]:
    """Hash a regular file without buffering or interpreting its contents."""

    _require(type(maximum) is int and maximum >= 1, f"{label} byte ceiling is invalid")
    descriptor, directories = _open_regular_descriptor(path, label=label)
    try:
        before = os.fstat(descriptor)
        _require(1 <= before.st_size <= maximum, f"{label} size is outside the byte ceiling")
        _require(before.st_nlink == 1, f"{label} link count differs")
        digest = hashlib.sha256()
        byte_count = 0
        while True:
            chunk = os.read(descriptor, 1024 * 1024)
            if not chunk:
                break
            byte_count += len(chunk)
            _require(byte_count <= maximum, f"{label} exceeds the byte ceiling")
            digest.update(chunk)
        after = os.fstat(descriptor)
        _require(
            byte_count == before.st_size
            and _stable_stat_identity(before) == _stable_stat_identity(after),
            f"{label} changed while being read",
        )
        return {"byte_count": byte_count, "sha256": digest.hexdigest()}
    except CacheArchiveVerificationError:
        raise
    except OSError as exc:
        raise CacheArchiveVerificationError(f"cannot read {label}") from exc
    finally:
        os.close(descriptor)
        for directory_descriptor in reversed(directories):
            os.close(directory_descriptor)


def _read_json(
    path: pathlib.Path,
    *,
    maximum: int,
    label: str,
    canonical: bool = False,
    require_single_link: bool = False,
) -> tuple[dict[str, Any], bytes]:
    raw = _read_bounded(
        path,
        maximum=maximum,
        label=label,
        require_single_link=require_single_link,
    )
    value = _parse_json(raw, label=label)
    if canonical:
        _require(raw == _render(value), f"{label} bytes are not canonical")
    return value, raw


def _artifact_file(value: Any, *, label: str) -> str:
    _require(type(value) is str and bool(value), f"{label} is invalid")
    name = cast(str, value)
    _require(
        len(name.encode("utf-8")) <= 255
        and pathlib.PurePosixPath(name).name == name
        and name not in {".", ".."}
        and "\\" not in name
        and "\0" not in name
        and unicodedata.normalize("NFC", name) == name
        and not any(unicodedata.category(character).startswith("C") for character in name),
        f"{label} is not a canonical artifact filename",
    )
    return name


def _read_checked_draft_validator_source() -> tuple[bytes, dict[str, Any]]:
    raw = _read_bounded(
        DRAFT202012_PATH,
        maximum=MAX_SOURCE_BYTES,
        label="Draft 2020-12 validator source",
    )
    identity = {
        "artifact_file": DRAFT202012_PATH.name,
        "artifact_sha256": _sha256(raw),
        "size_bytes": len(raw),
    }
    _require(identity["artifact_sha256"] == CHECKED_DRAFT202012_SHA256, "Draft validator source hash differs")
    _require(identity["size_bytes"] == CHECKED_DRAFT202012_SIZE, "Draft validator source size differs")
    return raw, identity


def _draft_validator_identity() -> dict[str, Any]:
    _, identity = _read_checked_draft_validator_source()
    return identity


def _checked_draft_validator() -> types.ModuleType:
    global _CHECKED_DRAFT_VALIDATOR

    raw, _ = _read_checked_draft_validator_source()
    if _CHECKED_DRAFT_VALIDATOR is not None:
        return _CHECKED_DRAFT_VALIDATOR
    module_name = "_entire_brain_checked_cache_archive_draft202012_v1"
    module = types.ModuleType(module_name)
    module.__file__ = str(DRAFT202012_PATH)
    previous = sys.modules.get(module_name)
    try:
        code = compile(raw, str(DRAFT202012_PATH), "exec", dont_inherit=True)
        sys.modules[module_name] = module
        exec(code, module.__dict__)
    except Exception as exc:
        raise CacheArchiveVerificationError("checked Draft validator source cannot be loaded") from exc
    finally:
        if previous is None:
            sys.modules.pop(module_name, None)
        else:
            sys.modules[module_name] = previous
    _require(
        getattr(module, "DIALECT", None) == "https://json-schema.org/draft/2020-12/schema"
        and type(getattr(module, "SchemaDocument", None)) is type
        and type(getattr(module, "Validator", None)) is type
        and type(getattr(module, "SchemaError", None)) is type,
        "checked Draft validator API differs",
    )
    _CHECKED_DRAFT_VALIDATOR = module
    return module


def _validate_schema(
    instance: Mapping[str, Any],
    schema: Mapping[str, Any],
    *,
    schema_name: str,
    label: str,
) -> None:
    module = _checked_draft_validator()
    try:
        validator = module.Validator([module.SchemaDocument(name=schema_name, schema=schema)])
        validator.validate(instance, schema_name, label=label)
    except module.SchemaError as exc:
        raise CacheArchiveVerificationError(f"{label} fails its checked schema") from exc


def _audit_schema(schema: Mapping[str, Any], *, schema_name: str, label: str) -> None:
    module = _checked_draft_validator()
    try:
        module.Validator([module.SchemaDocument(name=schema_name, schema=schema)])
    except module.SchemaError as exc:
        raise CacheArchiveVerificationError(f"{label} schema is invalid") from exc


def _load_schema(
    path: pathlib.Path,
    *,
    expected_sha256: str,
    label: str,
) -> tuple[dict[str, Any], bytes]:
    schema, raw = _read_json(path, maximum=MAX_SCHEMA_BYTES, label=label)
    _require(_sha256(raw) == expected_sha256, f"{label} hash differs")
    return schema, raw


def _file_binding(path: pathlib.Path, raw: bytes) -> dict[str, str]:
    return {
        "artifact_file": _artifact_file(path.name, label="artifact filename"),
        "artifact_sha256": _sha256(raw),
    }


EXPECTED_EXECUTION_CONTRACT_BINDING = {
    "artifact_file": EXECUTION_CONTRACT_PATH.name,
    "artifact_sha256": CHECKED_EXECUTION_ARTIFACT_SHA256,
    "builder_file": EXECUTION_CONTRACT_BUILDER_PATH.name,
    "builder_sha256": CHECKED_EXECUTION_BUILDER_SHA256,
    "contract_sha256": CHECKED_EXECUTION_CONTRACT_SHA256,
    "execution_status": "forbidden_missing_all_residual_gates_and_audited_execution_adapter",
    "profile": "agent_brain_negative_control_execution_contract_v1",
    "schema_file": EXECUTION_SCHEMA_PATH.name,
    "schema_sha256": CHECKED_EXECUTION_SCHEMA_SHA256,
    "schema_version": 1,
    "source_commit": CHECKED_EXECUTION_SOURCE_COMMIT,
    "status": "contract_compiled_execution_forbidden",
}

EXPECTED_RUN_PLAN_BINDING = {
    "artifact_file": RUN_PLAN_PATH.name,
    "artifact_sha256": CHECKED_RUN_PLAN_ARTIFACT_SHA256,
    "plan_sha256": CHECKED_RUN_PLAN_SHA256,
    "profile": "agent_brain_development_task_negative_control_run_plan_v2",
    "schema_file": RUN_PLAN_SCHEMA_PATH.name,
    "schema_sha256": CHECKED_RUN_PLAN_SCHEMA_SHA256,
    "schema_version": 2,
    "status": "pending_owner_authorization",
}

EXPECTED_SCOPE = {
    "archive_byte_count_max": MAX_ARCHIVE_BYTES,
    "execution_contract": {
        key: value
        for key, value in EXPECTED_EXECUTION_CONTRACT_BINDING.items()
        if key not in {"builder_file", "builder_sha256"}
    },
    "file_count_max": MAX_MANIFEST_FILE_COUNT,
    "gate_primitive": {
        "source_file": GATE_PRIMITIVE_PATH.name,
        "source_sha256": CHECKED_GATE_PRIMITIVE_SHA256,
    },
    "manifest": {
        "profile": MANIFEST_PROFILE,
        "schema_file": MANIFEST_SCHEMA_PATH.name,
        "schema_sha256": CHECKED_MANIFEST_SCHEMA_SHA256,
    },
    "repository_order": list(REPOSITORY_ORDER),
    "roots": list(CACHE_ROOTS),
    "run_plan": EXPECTED_RUN_PLAN_BINDING,
    "toolchain_bindings_sha256": CHECKED_TOOLCHAIN_BINDINGS_SHA256,
    "unpacked_byte_count_max": MAX_UNPACKED_BYTES,
}

AUTHORITY = {
    "archive_extraction": "forbidden",
    "archive_staging": "forbidden",
    "atomic_consumption": False,
    "benchmark_execution": "forbidden",
    "candidate_execution": "forbidden",
    "content_safety_verified": False,
    "execution_authority": False,
    "model_provider_execution": "forbidden",
    "network_access": "forbidden",
    "owner_approval": False,
    "paid_execution": "forbidden",
}

REPORT_AUTHORITY = {
    "atomic_consumption": False,
    "benchmark_execution": "forbidden",
    "candidate_execution": "forbidden",
    "execution_authority": False,
    "model_provider_execution": "forbidden",
    "network_access": "forbidden",
    "owner_approval": False,
    "paid_execution": "forbidden",
}

REPORT_SAFETY = {
    "archive_content_parsed": False,
    "content_safety_verified": False,
    "e0_runtime_binding": False,
    "extraction": False,
    "staging": False,
}

RUNTIME_BINDINGS = {
    "actual_cache_archive_sha256": None,
    "actual_cache_manifest_sha256": None,
    "cache_verification_report_sha256": None,
    "owner_approval_verification_report_sha256": None,
    "safe_archive_verification_report_sha256": None,
}

RESIDUAL_GATES = [
    "actual_approved_offline_cache_seed_archive_and_authorized_binding",
    "safe_archive_traversal_type_link_device_and_content_verifier",
    "trusted_apfs_observer_and_atomic_capacity_reservation",
    "clean_detached_worktree_executor_and_first_parent_reversal_proof",
    "attested_attempt_producer_and_classifier_integration",
    "private_log_executor_integration_retention_and_aggregate_accounting",
    "fail_closed_cleanup_interruption_attestation_and_no_receipt_guarantee",
    "atomic_single_use_approval_consumption_and_execution_binding",
]

VERIFICATION_POLICY = {
    "archive_byte_count_max": MAX_ARCHIVE_BYTES,
    "archive_content_handling": "opaque_raw_bytes_only_never_parsed_or_extracted",
    "cli_commands": ["build", "check", "verify"],
    "e0_runtime_binding": E0_BINDING_STATUS,
    "file_count_max": MAX_MANIFEST_FILE_COUNT,
    "json_depth_max": MAX_JSON_DEPTH,
    "json_nodes_max": MAX_JSON_NODES,
    "manifest_byte_count_max": MAX_FIXED_JSON_BYTES,
    "production_path_overrides": False,
    "successful_verification_claim": IDENTITY_BINDING_STATUS,
    "unpacked_byte_count_max": MAX_UNPACKED_BYTES,
}


def _integer(value: Any, *, label: str, minimum: int, maximum: int) -> int:
    _require(type(value) is int and minimum <= value <= maximum, f"{label} is invalid")
    return cast(int, value)


def _execution_contract_binding(value: Mapping[str, Any], raw: bytes) -> dict[str, Any]:
    implementation = value.get("implementation")
    binding = {
        "artifact_file": EXECUTION_CONTRACT_PATH.name,
        "artifact_sha256": _sha256(raw),
        "builder_file": EXECUTION_CONTRACT_BUILDER_PATH.name,
        "builder_sha256": (
            implementation.get("builder_sha256") if type(implementation) is dict else None
        ),
        "contract_sha256": value.get("contract_sha256"),
        "execution_status": value.get("execution_status"),
        "profile": value.get("profile"),
        "schema_file": EXECUTION_SCHEMA_PATH.name,
        "schema_sha256": (
            implementation.get("schema_sha256") if type(implementation) is dict else None
        ),
        "schema_version": value.get("schema_version"),
        "source_commit": CHECKED_EXECUTION_SOURCE_COMMIT,
        "status": value.get("status"),
    }
    _require(binding == EXPECTED_EXECUTION_CONTRACT_BINDING, "execution contract differs from exact E0 binding")
    _require(
        value.get("contract_sha256") == _field_self_hash(value, "contract_sha256"),
        "execution-contract self hash differs",
    )
    authority = value.get("authority")
    runtime = value.get("runtime_bindings")
    residual = value.get("residual_gates")
    _require(
        type(authority) is dict
        and authority.get("candidate_execution") == "forbidden_contract_unexecutable"
        and authority.get("benchmark_execution") == "forbidden_contract_unexecutable"
        and authority.get("model_provider_execution") == "forbidden_not_authorized"
        and authority.get("paid_execution") == "forbidden_not_authorized",
        "execution-contract authority boundary differs",
    )
    _require(
        type(runtime) is dict
        and runtime.get("actual_cache_archive_sha256") is None
        and runtime.get("actual_cache_manifest_sha256") is None,
        "execution contract unexpectedly binds cache material",
    )
    _require(
        type(residual) is list
        and "actual_approved_offline_cache_seed_archive_and_authorized_binding" in residual
        and "safe_archive_traversal_type_link_device_and_content_verifier" in residual,
        "execution-contract cache residual gates differ",
    )
    return binding


def _run_plan_binding(value: Mapping[str, Any], raw: bytes) -> dict[str, Any]:
    binding = {
        "artifact_file": RUN_PLAN_PATH.name,
        "artifact_sha256": _sha256(raw),
        "plan_sha256": value.get("plan_sha256"),
        "profile": value.get("profile"),
        "schema_file": RUN_PLAN_SCHEMA_PATH.name,
        "schema_sha256": (
            value.get("implementation", {}).get("schema_sha256")
            if type(value.get("implementation")) is dict
            else None
        ),
        "schema_version": value.get("schema_version"),
        "status": value.get("status"),
    }
    _require(binding == EXPECTED_RUN_PLAN_BINDING, "run plan differs from exact binding")
    _require(
        value.get("plan_sha256") == _field_self_hash(value, "plan_sha256"),
        "run-plan self hash differs",
    )
    authority = value.get("authority")
    cache_seed = value.get("cache_seed")
    _require(
        type(authority) is dict
        and authority.get("candidate_execution") == "forbidden_plan_unexecutable"
        and authority.get("benchmark_execution") == "forbidden_pending_separate_owner_approval"
        and authority.get("model_provider_execution") == "forbidden_not_authorized"
        and authority.get("paid_execution") == "forbidden_not_authorized",
        "run-plan authority boundary differs",
    )
    _require(
        type(cache_seed) is dict
        and cache_seed.get("archive_sha256") is None
        and cache_seed.get("manifest_sha256") is None
        and cache_seed.get("source_locator") is None
        and cache_seed.get("unpacked_byte_count") is None,
        "run plan unexpectedly binds cache material",
    )
    repositories = value.get("repositories")
    _require(type(repositories) is list, "run-plan repositories differ")
    repository_values = cast(list[Any], repositories)
    projection: list[dict[str, Any]] = []
    for repository in repository_values:
        _require(type(repository) is dict, "run-plan repository differs")
        projection.append(
            {
                "repository_key": repository.get("key"),
                "toolchain": copy.deepcopy(repository.get("toolchain")),
            }
        )
    _require(
        [entry["repository_key"] for entry in projection] == list(REPOSITORY_ORDER),
        "run-plan repository order differs",
    )
    _require(
        _canonical_hash(projection) == CHECKED_TOOLCHAIN_BINDINGS_SHA256,
        "run-plan toolchain projection differs",
    )
    resource = value.get("resource_budget")
    _require(
        type(resource) is dict
        and resource.get("max_cache_seed_bytes") == MAX_ARCHIVE_BYTES,
        "run-plan cache byte ceiling differs",
    )
    return binding


def _expected_review(status: str) -> dict[str, Any]:
    _require(status in {PENDING_MATERIAL_STATUS, APPROVED_MATERIAL_STATUS}, "material status differs")
    approved = status == APPROVED_MATERIAL_STATUS
    return {
        "archive_created_by_this_slice": False,
        "archive_extraction": False,
        "benchmark_execution": "forbidden",
        "candidate_execution": "forbidden",
        "content_safety_verified": False,
        "disposition": status,
        "e0_runtime_binding": False,
        "execution_authority": False,
        "model_provider_execution": "forbidden",
        "network_access": "forbidden",
        "owner_approval_binding": False,
        "paid_execution": "forbidden",
        "source_review_effect": (
            "raw_identity_only_not_e0_runtime_authority" if approved else "none_pending"
        ),
    }


def _validate_repository_counts(value: Any, *, file_count: int, label: str) -> dict[str, int]:
    _require(type(value) is dict and set(value) == set(REPOSITORY_ORDER), f"{label} fields differ")
    counts: dict[str, int] = {}
    for key in REPOSITORY_ORDER:
        counts[key] = _integer(
            value[key],
            label=f"{label}.{key}",
            minimum=1,
            maximum=MAX_MANIFEST_FILE_COUNT,
        )
    _require(sum(counts.values()) == file_count, f"{label} sum differs from file count")
    return counts


def validate_material_declaration(
    value: Mapping[str, Any],
    *,
    schema: Mapping[str, Any],
) -> dict[str, Any]:
    _validate_schema(
        value,
        schema,
        schema_name=MATERIAL_SCHEMA_PATH.name,
        label="cache archive material declaration",
    )
    expected_fields = {
        "canonical_json_profile",
        "material",
        "material_declaration_sha256",
        "profile",
        "review",
        "schema_version",
        "scope",
        "status",
    }
    _require(type(value) is dict and set(value) == expected_fields, "material-declaration fields differ")
    _require(
        value["canonical_json_profile"] == CANONICAL_ARTIFACT_PROFILE
        and value["profile"] == MATERIAL_PROFILE
        and value["schema_version"] == SCHEMA_VERSION,
        "material-declaration identity differs",
    )
    status = value["status"]
    _require(status in {PENDING_MATERIAL_STATUS, APPROVED_MATERIAL_STATUS}, "material-declaration status differs")
    _require(value["scope"] == EXPECTED_SCOPE, "material-declaration scope differs")
    _require(value["review"] == _expected_review(cast(str, status)), "material-declaration review boundary differs")
    _require(_valid_sha(value["material_declaration_sha256"]), "material-declaration self hash is invalid")
    _require(
        value["material_declaration_sha256"]
        == _field_self_hash(value, "material_declaration_sha256"),
        "material-declaration self hash differs",
    )

    material = value["material"]
    material_fields = {
        "archive_byte_count",
        "archive_file",
        "archive_interpretation",
        "archive_sha256",
        "content_inventory_sha256",
        "file_count",
        "manifest_file",
        "manifest_file_sha256",
        "manifest_sha256",
        "repository_file_counts",
        "unpacked_byte_count",
    }
    _require(type(material) is dict and set(material) == material_fields, "material identity fields differ")
    material_map = cast(dict[str, Any], material)
    _require(
        material_map["archive_interpretation"] == OPAQUE_ARCHIVE_INTERPRETATION,
        "archive interpretation differs",
    )
    identity_fields = material_fields - {"archive_interpretation"}
    if status == PENDING_MATERIAL_STATUS:
        _require(
            all(material_map[field] is None for field in identity_fields),
            "pending material declaration contains an identity",
        )
    else:
        archive_file = _artifact_file(material_map["archive_file"], label="material.archive_file")
        manifest_file = _artifact_file(material_map["manifest_file"], label="material.manifest_file")
        _require(archive_file != manifest_file, "archive and manifest filenames collide")
        for field in (
            "archive_sha256",
            "content_inventory_sha256",
            "manifest_file_sha256",
            "manifest_sha256",
        ):
            _require(_valid_sha(material_map[field]), f"material.{field} is invalid")
        archive_bytes = _integer(
            material_map["archive_byte_count"],
            label="material.archive_byte_count",
            minimum=1,
            maximum=MAX_ARCHIVE_BYTES,
        )
        _require(archive_bytes >= 1, "material archive byte count differs")
        file_count = _integer(
            material_map["file_count"],
            label="material.file_count",
            minimum=len(REPOSITORY_ORDER),
            maximum=MAX_MANIFEST_FILE_COUNT,
        )
        _integer(
            material_map["unpacked_byte_count"],
            label="material.unpacked_byte_count",
            minimum=len(REPOSITORY_ORDER),
            maximum=MAX_UNPACKED_BYTES,
        )
        _validate_repository_counts(
            material_map["repository_file_counts"],
            file_count=file_count,
            label="material.repository_file_counts",
        )
    return copy.deepcopy(dict(value))


def _material_binding(
    value: Mapping[str, Any],
    *,
    material_raw: bytes,
) -> dict[str, Any]:
    material = value["material"]
    _require(type(material) is dict, "material-declaration material differs")
    material_map = cast(dict[str, Any], material)
    scope = value["scope"]
    _require(type(scope) is dict, "material-declaration scope differs")
    return {
        "archive_byte_count": material_map["archive_byte_count"],
        "archive_file": material_map["archive_file"],
        "archive_sha256": material_map["archive_sha256"],
        "artifact_file": MATERIAL_PATH.name,
        "artifact_sha256": _sha256(material_raw),
        "content_inventory_sha256": material_map["content_inventory_sha256"],
        "file_count": material_map["file_count"],
        "manifest_file": material_map["manifest_file"],
        "manifest_file_sha256": material_map["manifest_file_sha256"],
        "manifest_sha256": material_map["manifest_sha256"],
        "material_declaration_sha256": value["material_declaration_sha256"],
        "profile": value["profile"],
        "repository_file_counts": copy.deepcopy(material_map["repository_file_counts"]),
        "schema_file": MATERIAL_SCHEMA_PATH.name,
        "schema_sha256": CHECKED_MATERIAL_SCHEMA_SHA256,
        "status": value["status"],
        "toolchain_bindings_sha256": scope["toolchain_bindings_sha256"],
        "unpacked_byte_count": material_map["unpacked_byte_count"],
    }


def _load_fixed_dependencies() -> dict[str, Any]:
    execution_schema, execution_schema_raw = _load_schema(
        EXECUTION_SCHEMA_PATH,
        expected_sha256=CHECKED_EXECUTION_SCHEMA_SHA256,
        label="execution-contract schema",
    )
    run_plan_schema, run_plan_schema_raw = _load_schema(
        RUN_PLAN_SCHEMA_PATH,
        expected_sha256=CHECKED_RUN_PLAN_SCHEMA_SHA256,
        label="run-plan schema",
    )
    manifest_schema, manifest_schema_raw = _load_schema(
        MANIFEST_SCHEMA_PATH,
        expected_sha256=CHECKED_MANIFEST_SCHEMA_SHA256,
        label="cache-manifest schema",
    )
    material_schema, material_schema_raw = _load_schema(
        MATERIAL_SCHEMA_PATH,
        expected_sha256=CHECKED_MATERIAL_SCHEMA_SHA256,
        label="cache-archive material schema",
    )
    report_schema, report_schema_raw = _load_schema(
        REPORT_SCHEMA_PATH,
        expected_sha256=CHECKED_REPORT_SCHEMA_SHA256,
        label="cache-archive verification-report schema",
    )
    verifier_schema, verifier_schema_raw = _load_schema(
        VERIFIER_CONTRACT_SCHEMA_PATH,
        expected_sha256=CHECKED_VERIFIER_CONTRACT_SCHEMA_SHA256,
        label="cache-archive verifier-contract schema",
    )
    for schema, path, label in (
        (execution_schema, EXECUTION_SCHEMA_PATH, "execution-contract"),
        (run_plan_schema, RUN_PLAN_SCHEMA_PATH, "run-plan"),
        (manifest_schema, MANIFEST_SCHEMA_PATH, "cache-manifest"),
        (material_schema, MATERIAL_SCHEMA_PATH, "cache-archive material"),
        (report_schema, REPORT_SCHEMA_PATH, "cache-archive verification-report"),
        (verifier_schema, VERIFIER_CONTRACT_SCHEMA_PATH, "cache-archive verifier-contract"),
    ):
        _audit_schema(schema, schema_name=path.name, label=label)
    execution_contract, execution_raw = _read_json(
        EXECUTION_CONTRACT_PATH,
        maximum=MAX_FIXED_JSON_BYTES,
        label="execution contract",
        canonical=True,
    )
    _require(_sha256(execution_raw) == CHECKED_EXECUTION_ARTIFACT_SHA256, "execution-contract raw hash differs")
    _validate_schema(
        execution_contract,
        execution_schema,
        schema_name=EXECUTION_SCHEMA_PATH.name,
        label="execution contract",
    )
    execution_binding = _execution_contract_binding(execution_contract, execution_raw)
    run_plan, run_plan_raw = _read_json(
        RUN_PLAN_PATH,
        maximum=MAX_FIXED_JSON_BYTES,
        label="run plan",
        canonical=True,
    )
    _require(_sha256(run_plan_raw) == CHECKED_RUN_PLAN_ARTIFACT_SHA256, "run-plan raw hash differs")
    _validate_schema(
        run_plan,
        run_plan_schema,
        schema_name=RUN_PLAN_SCHEMA_PATH.name,
        label="run plan",
    )
    run_plan_binding = _run_plan_binding(run_plan, run_plan_raw)

    fixed_sources: dict[str, tuple[pathlib.Path, str]] = {
        "execution_contract_builder": (
            EXECUTION_CONTRACT_BUILDER_PATH,
            CHECKED_EXECUTION_BUILDER_SHA256,
        ),
        "run_plan_builder": (RUN_PLAN_BUILDER_PATH, CHECKED_RUN_PLAN_BUILDER_SHA256),
        "gate_primitive": (GATE_PRIMITIVE_PATH, CHECKED_GATE_PRIMITIVE_SHA256),
    }
    source_raw: dict[str, bytes] = {}
    for key, (path, expected_hash) in fixed_sources.items():
        raw = _read_bounded(path, maximum=MAX_SOURCE_BYTES, label=key.replace("_", " "))
        _require(_sha256(raw) == expected_hash, f"{key.replace('_', ' ')} hash differs")
        source_raw[key] = raw

    material, material_raw = _read_json(
        MATERIAL_PATH,
        maximum=MAX_FIXED_JSON_BYTES,
        label="cache archive material declaration",
        canonical=True,
    )
    _require(_sha256(material_raw) == CHECKED_MATERIAL_ARTIFACT_SHA256, "material-declaration raw hash differs")
    validated_material = validate_material_declaration(material, schema=material_schema)
    _require(
        validated_material["material_declaration_sha256"] == CHECKED_MATERIAL_SHA256,
        "material-declaration checked self hash differs",
    )
    return {
        "draft_validator": _draft_validator_identity(),
        "execution_binding": execution_binding,
        "execution_contract": execution_contract,
        "execution_schema_raw": execution_schema_raw,
        "manifest_schema": manifest_schema,
        "manifest_schema_raw": manifest_schema_raw,
        "material": validated_material,
        "material_raw": material_raw,
        "material_schema": material_schema,
        "material_schema_raw": material_schema_raw,
        "report_schema": report_schema,
        "report_schema_raw": report_schema_raw,
        "run_plan": run_plan,
        "run_plan_binding": run_plan_binding,
        "run_plan_schema_raw": run_plan_schema_raw,
        "source_raw": source_raw,
        "verifier_schema": verifier_schema,
        "verifier_schema_raw": verifier_schema_raw,
    }


def _expected_implementation(dependencies: Mapping[str, Any]) -> dict[str, Any]:
    source_raw = _read_bounded(
        pathlib.Path(__file__),
        maximum=MAX_SOURCE_BYTES,
        label="cache archive verifier source",
    )
    fixed_source_raw = dependencies["source_raw"]
    _require(type(fixed_source_raw) is dict, "fixed source dependency map differs")
    return {
        "builder": _file_binding(pathlib.Path(__file__), source_raw),
        "draft202012_validator": copy.deepcopy(dependencies["draft_validator"]),
        "execution_contract_builder": _file_binding(
            EXECUTION_CONTRACT_BUILDER_PATH,
            fixed_source_raw["execution_contract_builder"],
        ),
        "execution_contract_schema": _file_binding(
            EXECUTION_SCHEMA_PATH,
            dependencies["execution_schema_raw"],
        ),
        "gate_primitive": _file_binding(
            GATE_PRIMITIVE_PATH,
            fixed_source_raw["gate_primitive"],
        ),
        "manifest_schema": _file_binding(
            MANIFEST_SCHEMA_PATH,
            dependencies["manifest_schema_raw"],
        ),
        "material_schema": _file_binding(
            MATERIAL_SCHEMA_PATH,
            dependencies["material_schema_raw"],
        ),
        "report_schema": _file_binding(
            REPORT_SCHEMA_PATH,
            dependencies["report_schema_raw"],
        ),
        "run_plan_builder": _file_binding(
            RUN_PLAN_BUILDER_PATH,
            fixed_source_raw["run_plan_builder"],
        ),
        "run_plan_schema": _file_binding(
            RUN_PLAN_SCHEMA_PATH,
            dependencies["run_plan_schema_raw"],
        ),
        "verifier_contract_schema": _file_binding(
            VERIFIER_CONTRACT_SCHEMA_PATH,
            dependencies["verifier_schema_raw"],
        ),
    }


def build_verifier_contract() -> dict[str, Any]:
    dependencies = _load_fixed_dependencies()
    material = dependencies["material"]
    material_status = material["status"]
    status = (
        APPROVED_VERIFIER_STATUS
        if material_status == APPROVED_MATERIAL_STATUS
        else PENDING_VERIFIER_STATUS
    )
    contract: dict[str, Any] = {
        "authority": copy.deepcopy(AUTHORITY),
        "execution_contract_binding": copy.deepcopy(dependencies["execution_binding"]),
        "implementation": _expected_implementation(dependencies),
        "material_binding": _material_binding(
            material,
            material_raw=dependencies["material_raw"],
        ),
        "profile": VERIFIER_PROFILE,
        "residual_gates": list(RESIDUAL_GATES),
        "runtime_bindings": copy.deepcopy(RUNTIME_BINDINGS),
        "schema_version": SCHEMA_VERSION,
        "status": status,
        "verification_policy": copy.deepcopy(VERIFICATION_POLICY),
        "verifier_contract_sha256": None,
    }
    contract["verifier_contract_sha256"] = _field_self_hash(
        contract,
        "verifier_contract_sha256",
    )
    validate_verifier_contract(
        contract,
        schema=dependencies["verifier_schema"],
        dependencies=dependencies,
    )
    return contract


def validate_verifier_contract(
    value: Mapping[str, Any],
    *,
    schema: Mapping[str, Any],
    dependencies: Mapping[str, Any],
) -> dict[str, Any]:
    _validate_schema(
        value,
        schema,
        schema_name=VERIFIER_CONTRACT_SCHEMA_PATH.name,
        label="cache archive verifier contract",
    )
    expected_fields = {
        "authority",
        "execution_contract_binding",
        "implementation",
        "material_binding",
        "profile",
        "residual_gates",
        "runtime_bindings",
        "schema_version",
        "status",
        "verification_policy",
        "verifier_contract_sha256",
    }
    _require(type(value) is dict and set(value) == expected_fields, "verifier-contract fields differ")
    material = dependencies["material"]
    expected_status = (
        APPROVED_VERIFIER_STATUS
        if material["status"] == APPROVED_MATERIAL_STATUS
        else PENDING_VERIFIER_STATUS
    )
    _require(
        value["profile"] == VERIFIER_PROFILE
        and value["schema_version"] == SCHEMA_VERSION
        and value["status"] == expected_status,
        "verifier-contract identity or status differs",
    )
    _require(value["authority"] == AUTHORITY, "verifier-contract authority differs")
    _require(
        value["execution_contract_binding"] == EXPECTED_EXECUTION_CONTRACT_BINDING,
        "verifier-contract E0 binding differs",
    )
    _require(value["residual_gates"] == RESIDUAL_GATES, "verifier-contract residual gates differ")
    _require(value["runtime_bindings"] == RUNTIME_BINDINGS, "verifier-contract runtime bindings differ")
    _require(value["verification_policy"] == VERIFICATION_POLICY, "verifier-contract policy differs")
    _require(
        value["material_binding"]
        == _material_binding(material, material_raw=dependencies["material_raw"]),
        "verifier-contract material binding differs",
    )
    _require(
        value["implementation"] == _expected_implementation(dependencies),
        "verifier-contract implementation binding differs",
    )
    _require(_valid_sha(value["verifier_contract_sha256"]), "verifier-contract self hash is invalid")
    _require(
        value["verifier_contract_sha256"]
        == _field_self_hash(value, "verifier_contract_sha256"),
        "verifier-contract self hash differs",
    )
    return copy.deepcopy(dict(value))


def check_verifier_contract() -> dict[str, Any]:
    """Check only the fixed production verifier contract and dependencies."""

    dependencies = _load_fixed_dependencies()
    value, raw = _read_json(
        VERIFIER_CONTRACT_PATH,
        maximum=MAX_VERIFIER_CONTRACT_BYTES,
        label="cache archive verifier contract",
        canonical=True,
    )
    validated = validate_verifier_contract(
        value,
        schema=dependencies["verifier_schema"],
        dependencies=dependencies,
    )
    expected = build_verifier_contract()
    _require(raw == _render(expected) and validated == expected, "checked verifier contract differs")
    return validated


def _cache_path(value: Any, *, label: str) -> str:
    _require(type(value) is str and bool(value), f"{label} is invalid")
    path = cast(str, value)
    pure = pathlib.PurePosixPath(path)
    _require(
        not pure.is_absolute()
        and str(pure) == path
        and len(pure.parts) >= 2
        and pure.parts[0] in CACHE_ROOTS
        and "\\" not in path
        and "\0" not in path
        and all(component not in {"", ".", ".."} for component in pure.parts),
        f"{label} is not a canonical cache-relative path",
    )
    _require(
        len(path.encode("utf-8")) <= 1024
        and unicodedata.normalize("NFC", path) == path
        and all(len(component.encode("utf-8")) <= 255 for component in pure.parts)
        and not any(unicodedata.category(character).startswith("C") for character in path),
        f"{label} encoding is not canonical",
    )
    return path


def _portable_path_key(value: str) -> tuple[str, ...]:
    return tuple(
        unicodedata.normalize(
            "NFC",
            unicodedata.normalize("NFC", component).casefold(),
        )
        for component in pathlib.PurePosixPath(value).parts
    )


def _validate_manifest_implementation(value: Any) -> None:
    expected_fields = {
        "artifact_render_profile",
        "builder_sha256",
        "canonical_json_profile",
        "manifest_schema_sha256",
        "python_executable_sha256",
        "python_implementation",
        "python_version",
        "run_plan_builder_sha256",
        "schema_validator_sha256",
        "unicode_data_version",
        "verifier_sources",
    }
    _require(type(value) is dict and set(value) == expected_fields, "manifest implementation fields differ")
    implementation = cast(dict[str, Any], value)
    _require(
        implementation["artifact_render_profile"] == CANONICAL_ARTIFACT_PROFILE
        and implementation["canonical_json_profile"] == CANONICAL_JSON_PROFILE
        and implementation["builder_sha256"] == CHECKED_GATE_PRIMITIVE_SHA256
        and implementation["manifest_schema_sha256"] == CHECKED_MANIFEST_SCHEMA_SHA256
        and implementation["run_plan_builder_sha256"] == CHECKED_RUN_PLAN_BUILDER_SHA256
        and implementation["schema_validator_sha256"] == CHECKED_DRAFT202012_SHA256,
        "manifest implementation pins differ",
    )
    _require(
        implementation["unicode_data_version"] == unicodedata.unidata_version,
        "manifest Unicode data version differs from verifier runtime",
    )
    _require(
        type(implementation["python_implementation"]) is str
        and bool(implementation["python_implementation"])
        and type(implementation["python_version"]) is str
        and bool(implementation["python_version"])
        and _valid_sha(implementation["python_executable_sha256"]),
        "manifest Python implementation identity is invalid",
    )
    verifier_sources = implementation["verifier_sources"]
    expected_sources = {
        "draft202012.py",
        "relevance_dataset.py",
        "task_eligibility.py",
        "task_eligibility_v2.py",
        "task_negative_control_plan_v2.py",
        "task_overlap_registry.py",
        "task_population.py",
    }
    _require(
        type(verifier_sources) is dict
        and set(verifier_sources) == expected_sources
        and all(_valid_sha(item) for item in verifier_sources.values())
        and verifier_sources["draft202012.py"] == CHECKED_DRAFT202012_SHA256
        and verifier_sources["task_negative_control_plan_v2.py"]
        == CHECKED_RUN_PLAN_BUILDER_SHA256,
        "manifest verifier-source bindings differ",
    )


def _validate_manifest_identity(
    value: Mapping[str, Any],
    *,
    schema: Mapping[str, Any],
    material: Mapping[str, Any],
) -> dict[str, Any]:
    """Validate the reviewed manifest without opening or parsing the archive."""

    _validate_schema(
        value,
        schema,
        schema_name=MANIFEST_SCHEMA_PATH.name,
        label="cache archive manifest",
    )
    expected_fields = {
        "archive",
        "authority",
        "contents",
        "host_shared_cache_reuse",
        "implementation",
        "manifest_sha256",
        "network_dependency_resolution",
        "profile",
        "run_plan",
        "schema_version",
        "status",
    }
    _require(type(value) is dict and set(value) == expected_fields, "cache manifest fields differ")
    _require(
        value["profile"] == MANIFEST_PROFILE
        and value["schema_version"] == SCHEMA_VERSION
        and value["status"] == "external_input_identity_only_execution_forbidden",
        "cache manifest identity differs",
    )
    _require(
        value["authority"]
        == {
            "benchmark_execution": "forbidden_pending_separate_owner_approval",
            "candidate_execution": "forbidden_plan_unexecutable",
            "model_provider_execution": "forbidden_not_authorized",
            "paid_execution": "forbidden_not_authorized",
            "plan_status": "pending_owner_authorization",
        }
        and value["host_shared_cache_reuse"] == "forbidden"
        and value["network_dependency_resolution"] == "forbidden",
        "cache manifest authority boundary differs",
    )
    _require(
        value["run_plan"]
        == {
            "plan_sha256": CHECKED_RUN_PLAN_SHA256,
            "profile": "agent_brain_development_task_negative_control_run_plan_v2",
            "schema_version": 2,
            "toolchain_bindings_sha256": CHECKED_TOOLCHAIN_BINDINGS_SHA256,
        },
        "cache manifest run-plan binding differs",
    )
    _validate_manifest_implementation(value["implementation"])
    _require(
        _valid_sha(value["manifest_sha256"])
        and value["manifest_sha256"] == _field_self_hash(value, "manifest_sha256"),
        "cache manifest self hash differs",
    )

    declared = material.get("material")
    _require(type(declared) is dict, "reviewed material identities differ")
    declared_map = cast(dict[str, Any], declared)
    archive = value["archive"]
    _require(
        type(archive) is dict
        and set(archive) == {"artifact_file", "byte_count", "sha256"},
        "cache manifest archive fields differ",
    )
    archive_map = cast(dict[str, Any], archive)
    _artifact_file(archive_map["artifact_file"], label="manifest archive filename")
    archive_bytes = _integer(
        archive_map["byte_count"],
        label="manifest archive byte count",
        minimum=1,
        maximum=MAX_ARCHIVE_BYTES,
    )
    _require(_valid_sha(archive_map["sha256"]), "manifest archive hash is invalid")
    _require(
        archive_map
        == {
            "artifact_file": declared_map["archive_file"],
            "byte_count": declared_map["archive_byte_count"],
            "sha256": declared_map["archive_sha256"],
        }
        and archive_bytes == declared_map["archive_byte_count"],
        "cache manifest archive identity differs from reviewed material",
    )

    contents = value["contents"]
    expected_content_fields = {
        "entries",
        "file_count",
        "inventory_sha256",
        "repository_file_counts",
        "unpacked_byte_count",
    }
    _require(type(contents) is dict and set(contents) == expected_content_fields, "manifest contents fields differ")
    contents_map = cast(dict[str, Any], contents)
    entries = contents_map["entries"]
    _require(type(entries) is list, "manifest entries are not a list")
    file_count = _integer(
        contents_map["file_count"],
        label="manifest file count",
        minimum=len(REPOSITORY_ORDER),
        maximum=MAX_MANIFEST_FILE_COUNT,
    )
    _require(file_count == len(entries), "manifest file count differs")
    unpacked = _integer(
        contents_map["unpacked_byte_count"],
        label="manifest unpacked byte count",
        minimum=len(REPOSITORY_ORDER),
        maximum=MAX_UNPACKED_BYTES,
    )
    _require(_valid_sha(contents_map["inventory_sha256"]), "manifest inventory hash is invalid")

    expected_entry_fields = {"byte_count", "path", "repository_key", "sha256"}
    identities: list[tuple[str, str]] = []
    exact_paths: list[str] = []
    portable_paths: list[tuple[str, ...]] = []
    repository_counts: Counter[str] = Counter()
    unpacked_total = 0
    scalar_bytes = 0
    for index, entry in enumerate(entries):
        _require(
            type(entry) is dict and set(entry) == expected_entry_fields,
            f"manifest entry[{index}] fields differ",
        )
        entry_map = cast(dict[str, Any], entry)
        repository_key = entry_map["repository_key"]
        _require(
            type(repository_key) is str and repository_key in REPOSITORY_ORDER,
            f"manifest entry[{index}] repository differs",
        )
        key = cast(str, repository_key)
        path = _cache_path(entry_map["path"], label=f"manifest entry[{index}] path")
        _require(_valid_sha(entry_map["sha256"]), f"manifest entry[{index}] hash is invalid")
        byte_count = _integer(
            entry_map["byte_count"],
            label=f"manifest entry[{index}] byte count",
            minimum=1,
            maximum=MAX_UNPACKED_BYTES,
        )
        unpacked_total += byte_count
        _require(unpacked_total <= MAX_UNPACKED_BYTES, "manifest unpacked bytes exceed the ceiling")
        scalar_bytes += len(key.encode("utf-8")) + len(path.encode("utf-8")) + 64
        _require(scalar_bytes <= MAX_FIXED_JSON_BYTES, "manifest scalar bytes exceed the ceiling")
        identities.append((key, path))
        exact_paths.append(path)
        portable_paths.append(_portable_path_key(path))
        repository_counts[key] += 1

    _require(identities == sorted(identities), "manifest entries are not canonical")
    _require(len(exact_paths) == len(set(exact_paths)), "manifest paths collide globally")
    portable_set = set(portable_paths)
    _require(len(portable_paths) == len(portable_set), "manifest paths collide portably")
    _require(
        not any(
            path[:component_count] in portable_set
            for path in portable_set
            for component_count in range(1, len(path))
        ),
        "manifest paths contain a portable ancestor conflict",
    )
    expected_counts = {key: repository_counts[key] for key in REPOSITORY_ORDER}
    _require(all(expected_counts.values()), "manifest does not cover every repository")
    _require(
        contents_map["repository_file_counts"] == expected_counts
        and _validate_repository_counts(
            contents_map["repository_file_counts"],
            file_count=file_count,
            label="manifest repository counts",
        )
        == expected_counts,
        "manifest repository counts differ",
    )
    _require(unpacked == unpacked_total, "manifest unpacked byte count differs")
    _require(contents_map["inventory_sha256"] == _canonical_hash(entries), "manifest inventory hash differs")
    _require(
        file_count == declared_map["file_count"]
        and contents_map["inventory_sha256"] == declared_map["content_inventory_sha256"]
        and expected_counts == declared_map["repository_file_counts"]
        and unpacked == declared_map["unpacked_byte_count"]
        and value["manifest_sha256"] == declared_map["manifest_sha256"],
        "cache manifest contents differ from reviewed material",
    )
    return copy.deepcopy(dict(value))


def _build_verification_report(
    *,
    archive_identity: Mapping[str, Any],
    manifest: Mapping[str, Any],
    manifest_raw: bytes,
    material: Mapping[str, Any],
    verifier_contract: Mapping[str, Any],
) -> dict[str, Any]:
    declared = material["material"]
    contents = manifest["contents"]
    _require(type(declared) is dict and type(contents) is dict, "verified material projection differs")
    report: dict[str, Any] = {
        "archive": {
            "artifact_file": declared["archive_file"],
            "byte_count": archive_identity["byte_count"],
            "sha256": archive_identity["sha256"],
            "verification": "exact_bounded_regular_file_raw_identity",
        },
        "authority": copy.deepcopy(REPORT_AUTHORITY),
        "contents": {
            "file_count": contents["file_count"],
            "inventory_sha256": contents["inventory_sha256"],
            "repository_file_counts": copy.deepcopy(contents["repository_file_counts"]),
            "unpacked_byte_count": contents["unpacked_byte_count"],
        },
        "identity_binding_status": IDENTITY_BINDING_STATUS,
        "manifest": {
            "artifact_file": declared["manifest_file"],
            "file_sha256": _sha256(manifest_raw),
            "manifest_sha256": manifest["manifest_sha256"],
            "profile": MANIFEST_PROFILE,
            "schema_sha256": CHECKED_MANIFEST_SCHEMA_SHA256,
            "verification": "canonical_schema_self_scope_and_material_identity",
        },
        "material_declaration_sha256": material["material_declaration_sha256"],
        "profile": REPORT_PROFILE,
        "report_sha256": None,
        "safety": copy.deepcopy(REPORT_SAFETY),
        "schema_version": SCHEMA_VERSION,
        "status": REPORT_STATUS,
        "verifier_contract_sha256": verifier_contract["verifier_contract_sha256"],
    }
    report["report_sha256"] = _field_self_hash(report, "report_sha256")
    return report


def _validate_verification_report(
    value: Mapping[str, Any],
    *,
    schema: Mapping[str, Any],
) -> dict[str, Any]:
    _validate_schema(
        value,
        schema,
        schema_name=REPORT_SCHEMA_PATH.name,
        label="cache archive verification report",
    )
    expected_fields = {
        "archive",
        "authority",
        "contents",
        "identity_binding_status",
        "manifest",
        "material_declaration_sha256",
        "profile",
        "report_sha256",
        "safety",
        "schema_version",
        "status",
        "verifier_contract_sha256",
    }
    _require(type(value) is dict and set(value) == expected_fields, "verification-report fields differ")
    _require(
        value["profile"] == REPORT_PROFILE
        and value["schema_version"] == SCHEMA_VERSION
        and value["status"] == REPORT_STATUS
        and value["identity_binding_status"] == IDENTITY_BINDING_STATUS,
        "verification-report identity differs",
    )
    _require(value["authority"] == REPORT_AUTHORITY, "verification-report authority differs")
    _require(value["safety"] == REPORT_SAFETY, "verification-report safety boundary differs")
    _require(
        _valid_sha(value["report_sha256"])
        and value["report_sha256"] == _field_self_hash(value, "report_sha256"),
        "verification-report self hash differs",
    )
    return copy.deepcopy(dict(value))


def _verify_with_dependencies(
    manifest_path: pathlib.Path,
    archive_path: pathlib.Path,
    *,
    dependencies: Mapping[str, Any],
    verifier_contract: Mapping[str, Any],
) -> dict[str, Any]:
    """Private synthetic seam; production callers use fixed dependencies only."""

    material = dependencies["material"]
    _require(type(material) is dict, "material dependency differs")
    validated_contract = validate_verifier_contract(
        verifier_contract,
        schema=dependencies["verifier_schema"],
        dependencies=dependencies,
    )
    _require(
        material["status"] == APPROVED_MATERIAL_STATUS
        and validated_contract["status"] == APPROVED_VERIFIER_STATUS,
        "cache archive material source review is pending; locator access forbidden",
    )

    declared = material["material"]
    _require(type(declared) is dict, "approved material identities differ")
    declared_map = cast(dict[str, Any], declared)
    _require(
        manifest_path.name == declared_map["manifest_file"]
        and archive_path.name == declared_map["archive_file"],
        "cache material locator filename differs from reviewed identity",
    )

    manifest, manifest_raw = _read_json(
        manifest_path,
        maximum=MAX_FIXED_JSON_BYTES,
        label="reviewed cache manifest",
        canonical=True,
        require_single_link=True,
    )
    _require(
        _sha256(manifest_raw) == declared_map["manifest_file_sha256"],
        "cache manifest raw hash differs from reviewed material",
    )
    validated_manifest = _validate_manifest_identity(
        manifest,
        schema=dependencies["manifest_schema"],
        material=material,
    )

    archive_identity = _stream_identity(
        archive_path,
        maximum=MAX_ARCHIVE_BYTES,
        label="reviewed opaque cache archive",
    )
    _require(
        archive_identity
        == {
            "byte_count": declared_map["archive_byte_count"],
            "sha256": declared_map["archive_sha256"],
        },
        "opaque cache archive raw identity differs from reviewed material",
    )
    report = _build_verification_report(
        archive_identity=archive_identity,
        manifest=validated_manifest,
        manifest_raw=manifest_raw,
        material=material,
        verifier_contract=validated_contract,
    )
    return _validate_verification_report(
        report,
        schema=dependencies["report_schema"],
    )


def verify_material_files(
    manifest_path: pathlib.Path,
    archive_path: pathlib.Path,
) -> dict[str, Any]:
    """Verify fixed, source-reviewed raw identities without parsing the archive."""

    verifier_contract = check_verifier_contract()
    dependencies = _load_fixed_dependencies()
    material = dependencies["material"]
    _require(
        material["status"] == APPROVED_MATERIAL_STATUS,
        "cache archive material source review is pending; locator access forbidden",
    )
    return _verify_with_dependencies(
        manifest_path,
        archive_path,
        dependencies=dependencies,
        verifier_contract=verifier_contract,
    )


def _write_atomic(path: pathlib.Path, value: Mapping[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temporary_name = tempfile.mkstemp(
        prefix=f".{path.name}.",
        dir=path.parent,
    )
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


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    build = commands.add_parser("build", help="build the non-authorizing verifier contract")
    build.add_argument("--output", required=True, type=pathlib.Path)
    commands.add_parser("check", help="check the fixed verifier contract and dependencies")
    verify = commands.add_parser("verify", help="verify opaque cache raw identities only")
    verify.add_argument("manifest", type=pathlib.Path)
    verify.add_argument("archive", type=pathlib.Path)
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    parser = _parser()
    args = parser.parse_args(argv)
    try:
        if args.command == "build":
            _write_atomic(args.output, build_verifier_contract())
        elif args.command == "check":
            contract = check_verifier_contract()
            print(
                json.dumps(
                    {
                        "execution_authority": False,
                        "profile": contract["profile"],
                        "status": contract["status"],
                    },
                    separators=(",", ":"),
                    sort_keys=True,
                )
            )
        else:
            report = verify_material_files(args.manifest, args.archive)
            print(_canonical_bytes(report).decode("utf-8"))
    except CacheArchiveVerificationError as exc:
        parser.error(str(exc))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
