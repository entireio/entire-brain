#!/usr/bin/env python3
"""Verify the frozen regular-file-only cache bundle without staging it.

The checked v1 material declaration is pending, so production ``verify``
currently fails before opening either caller locator.  A future reviewed v1
material identity may enable two read-only passes over the same bundle file:
raw identity first, then bounded structure and content verification.  This
module cannot create a bundle or extract, stage, reserve, authorize, or execute
cache material.
"""

from __future__ import annotations

import argparse
import copy
import hashlib
import json
import os
import pathlib
import stat
import struct
import sys
import tempfile
import types
from collections.abc import Mapping, Sequence
from typing import Any, cast


ROOT = pathlib.Path(__file__).parent
V1_SOURCE_PATH = ROOT / "negative_control_cache_archive_v1.py"
FORMAT_PATH = ROOT / "negative-control-cache-bundle-format-v2.json"
VERIFIER_CONTRACT_PATH = ROOT / "negative-control-cache-bundle-verifier-contract-v2.json"
FORMAT_SCHEMA_PATH = ROOT / "schemas" / "negative-control-cache-bundle-format-v2.schema.json"
REPORT_SCHEMA_PATH = ROOT / "schemas" / "negative-control-cache-bundle-verification-report-v2.schema.json"
VERIFIER_SCHEMA_PATH = ROOT / "schemas" / "negative-control-cache-bundle-verifier-contract-v2.schema.json"

SCHEMA_VERSION = 2
FORMAT_PROFILE = "agent_brain_negative_control_cache_bundle_format_v2"
VERIFIER_PROFILE = "agent_brain_negative_control_cache_bundle_verifier_contract_v2"
REPORT_PROFILE = "agent_brain_negative_control_cache_bundle_verification_report_v2"
FORMAT_STATUS = "wire_format_frozen_no_material_no_authority"
PENDING_STATUS = "verifier_compiled_raw_material_source_review_pending_bundle_access_forbidden"
APPROVED_STATUS = "verifier_compiled_raw_material_source_reviewed_bundle_content_verification_only"
REPORT_STATUS = (
    "bundle_v2_same_descriptor_raw_identity_structure_manifest_bijection_and_"
    "all_file_contents_verified_extraction_staging_execution_forbidden"
)

CHECKED_V1_SOURCE_SHA256 = "2f21da09c28cf2de62f808a6362f74fbc7aead07a04cfef96c15c93584540b94"
CHECKED_V1_SOURCE_SIZE = 68_195

# Root fills these reviewed identities after the concurrently authored format
# and schemas settle, before creating the checked verifier contract.
CHECKED_FORMAT_FILE_SHA256 = "970669197065a563fa0a09c281f671f5a3cd63061d6ff8f52da64926fa5ac406"
CHECKED_FORMAT_SHA256 = "e2231e7fae7dfdad054c770d928b549bb996b4c452169cb22abea7f8f6f0ee59"
CHECKED_FORMAT_SCHEMA_SHA256 = "95712ba76c36107f583a195167e6dea9d90ba81729fba10fa211096d277617d8"
CHECKED_REPORT_SCHEMA_SHA256 = "3afd8fec9da54a8b7358b497031633bfeaf8c5df1f595347424769d4a30ec4ad"
CHECKED_VERIFIER_SCHEMA_SHA256 = "eca24ad3f0a50f4a9dcbd9e106715db8fee449d4735eaeee87912a95e7c564bd"

HEADER_BYTES = 232
RECORD_FIXED_BYTES = 52
TRAILER_BYTES = 104
HEADER_MAGIC = b"ENTIRECACHEBND2\0"
RECORD_MAGIC = b"FILE"
TRAILER_MAGIC = b"ENTIRECACHEEND2\0"
FORMAT_VERSION = 2
MAX_BUNDLE_BYTES = 2 * 1024 * 1024 * 1024
MAX_UNPACKED_BYTES = 2 * 1024 * 1024 * 1024
MAX_FILE_COUNT = 250_000
MAX_PATH_BYTES = 1_024
MIN_BUNDLE_BYTES = HEADER_BYTES + TRAILER_BYTES + 3 * (RECORD_FIXED_BYTES + 1 + 1)
READ_CHUNK_BYTES = 1_048_576
REPOSITORY_ORDER = ("entire-brain", "entire-db", "entire-graph")
REPOSITORY_IDS = {key: index for index, key in enumerate(REPOSITORY_ORDER)}

MANIFEST_PROJECTION_DOMAIN = b"entire-brain/cache-bundle-manifest-projection/v2\0"
RECORDS_DOMAIN = b"entire-brain/cache-bundle-records/v2\0"
PREFIX_DOMAIN = b"entire-brain/cache-bundle-prefix/v2\0"

V1_PENDING_MATERIAL_STATUS = "pending_source_review"
V1_APPROVED_MATERIAL_STATUS = "approved_source_review_identity_only"
V1_PENDING_VERIFIER_STATUS = "verifier_compiled_material_source_review_pending_execution_forbidden"
V1_APPROVED_VERIFIER_STATUS = (
    "verifier_compiled_material_source_reviewed_identity_only_execution_forbidden"
)
V1_VERIFIER_CONTRACT_PATH = ROOT / "negative-control-cache-archive-verifier-contract-v1.json"
MAX_FIXED_JSON_BYTES = 128 * 1024 * 1024
MAX_SOURCE_BYTES = 4 * 1024 * 1024
MAX_VERIFIER_CONTRACT_BYTES = 4 * 1024 * 1024

AUTHORITY = {
    "atomic_consumption": False,
    "benchmark_execution": "forbidden",
    "bundle_creation": "forbidden",
    "bundle_extraction": "forbidden",
    "bundle_staging": "forbidden",
    "candidate_execution": "forbidden",
    "e0_runtime_binding": False,
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
    "bundle_creation": False,
    "e0_runtime_binding": False,
    "extraction": False,
    "staged_tree_created": False,
    "staging": False,
}

REPORT_STRUCTURE = {
    "canonical_record_order": True,
    "compression": False,
    "exact_eof": True,
    "header_verified": True,
    "path_policy_verified": True,
    "record_kind": "nonempty_regular_file_only",
    "structurally_unrepresentable_verified": [
        "explicit_directories",
        "symlinks",
        "hardlinks_or_inode_identity",
        "fifos",
        "sockets",
        "block_devices",
        "character_devices",
        "other_devices",
        "whiteouts",
        "sparse_metadata",
        "alternate_data_streams",
        "resource_forks",
        "compression",
        "encryption",
        "signatures",
        "padding",
        "concatenated_members",
        "acls",
        "extended_attributes",
        "uid_gid",
        "timestamps",
        "mode_bits",
        "executable_setuid_setgid_sticky_bits",
    ],
    "trailer_verified": True,
}

FORMAT_BINDING = {
    "artifact_file": FORMAT_PATH.name,
    "artifact_sha256": CHECKED_FORMAT_FILE_SHA256,
    "format_contract_sha256": CHECKED_FORMAT_SHA256,
    "profile": FORMAT_PROFILE,
    "schema_file": FORMAT_SCHEMA_PATH.name,
    "schema_sha256": CHECKED_FORMAT_SCHEMA_SHA256,
    "schema_version": SCHEMA_VERSION,
    "status": FORMAT_STATUS,
}

MANIFEST_BINDING = {
    "manifest_profile": "content_addressed_offline_go_cache_seed_manifest_v1",
    "manifest_schema_sha256": "37d3839411b2e30a99fdd7e93784d56f5fd525c0472717a24cc7b92213b98999",
    "repository_key_semantics": "deterministic_ownership_and_provenance_only_not_destination_namespace",
    "run_plan_artifact_sha256": "f55b6e22a25daf8016305304adf3b7ac8d31675281d97cfc9bb78cc76b619790",
    "run_plan_sha256": "a47311fa1f4553f8085ebea6e8ddd003a4ea5d0b2d269d91bbcdd414134a248e",
    "run_plan_schema_sha256": "294f77f165676bec6053e9637578ca22f048a887d2a978bbf8b8e300f21a2dad",
    "shared_seed_semantics": "one_shared_union_seed_copied_unchanged_to_every_arm",
    "toolchain_bindings_sha256": "e8240c5af77530079643593a7484cb0062408829bab76690b7f55d12176fc87c",
}

RESIDUAL_GATES = [
    "successful_v2_bundle_content_verification_report",
    "separate_safe_stager_with_go_cache_behavior_validation",
    "trusted_apfs_observer_and_atomic_capacity_reservation",
    "clean_detached_worktree_executor_and_first_parent_reversal_proof",
    "attested_attempt_producer_and_classifier_integration",
    "private_log_executor_integration_retention_and_aggregate_accounting",
    "fail_closed_cleanup_interruption_attestation_and_no_receipt_guarantee",
    "atomic_single_use_approval_consumption_and_execution_binding",
]

RUNTIME_BINDINGS = {
    "actual_bundle_sha256": None,
    "actual_manifest_sha256": None,
    "bundle_verification_report_sha256": None,
    "consumption_run_sha256": None,
    "owner_approval_verification_report_sha256": None,
    "raw_identity_report_sha256": None,
    "staged_tree_sha256": None,
}

VERIFICATION_POLICY = {
    "bundle_byte_count_formula": "232 + 104 + 52*N + sum(path_utf8_byte_count) + unpacked_byte_count",
    "bundle_byte_count_max": MAX_BUNDLE_BYTES,
    "bundle_descriptor": "one_held_single_link_stable_regular_file_descriptor_never_reopened",
    "cli": {
        "build": "build --output CONTRACT",
        "check": "check",
        "verify": "verify MANIFEST BUNDLE",
    },
    "file_count_max": MAX_FILE_COUNT,
    "file_count_min": 3,
    "json_depth_max": 64,
    "json_nodes_max": 1_270_000,
    "manifest_byte_count_max": MAX_FIXED_JSON_BYTES,
    "no_payload_sized_allocations": True,
    "pass_1": "raw_bundle_size_and_sha256_against_source_reviewed_v1_material_before_interpretation_then_fstat",
    "pass_2": "rewind_same_descriptor_then_bounded_parse_stream_hash_manifest_bijection_exact_eof_then_fstat",
    "production_commands_absent": ["pack", "extract", "stage"],
    "report_persistence": "forbidden_stdout_only",
    "stream_chunk_byte_count": READ_CHUNK_BYTES,
    "successful_claim": "raw_identity_structure_manifest_bijection_and_every_regular_file_payload_sha256_only_not_staging_or_execution",
    "unpacked_byte_count_max": MAX_UNPACKED_BYTES,
}

_V1_MODULE: types.ModuleType | None = None


class CacheBundleVerificationError(RuntimeError):
    """Raised when a checked bundle identity or structural claim differs."""


def _require(condition: bool, message: str) -> None:
    if not condition:
        raise CacheBundleVerificationError(message)


def _sha256(raw: bytes) -> str:
    _require(type(raw) is bytes, "SHA-256 input must be exact bytes")
    return hashlib.sha256(raw).hexdigest()


def _open_source_descriptor(path: pathlib.Path, *, label: str) -> tuple[int, list[int]]:
    components = path.parts[1:] if path.is_absolute() else path.parts
    _require(bool(components), f"{label} path has no file component")
    _require(
        all(component not in {"", ".", ".."} and "\0" not in component for component in components),
        f"{label} path contains traversal",
    )
    _require(os.open in os.supports_dir_fd, "descriptor-relative open is unavailable")
    file_flags = os.O_RDONLY
    directory_flags = os.O_RDONLY
    for name in ("O_CLOEXEC", "O_NOFOLLOW", "O_NONBLOCK"):
        flag = getattr(os, name, None)
        _require(type(flag) is int and flag != 0, f"secure open flag {name} is unavailable")
        file_flags |= cast(int, flag)
        directory_flags |= cast(int, flag)
    directory = getattr(os, "O_DIRECTORY", None)
    _require(type(directory) is int and directory != 0, "O_DIRECTORY is unavailable")
    directory_flags |= cast(int, directory)
    directories: list[int] = []
    descriptor = -1
    try:
        current = os.open("/" if path.is_absolute() else ".", directory_flags)
        directories.append(current)
        for component in components[:-1]:
            current = os.open(component, directory_flags, dir_fd=current)
            directories.append(current)
            _require(stat.S_ISDIR(os.fstat(current).st_mode), f"{label} ancestor is not a directory")
        descriptor = os.open(components[-1], file_flags, dir_fd=current)
        _require(stat.S_ISREG(os.fstat(descriptor).st_mode), f"{label} is not a regular file")
        return descriptor, directories
    except CacheBundleVerificationError:
        if descriptor >= 0:
            os.close(descriptor)
        for current in reversed(directories):
            os.close(current)
        raise
    except OSError as exc:
        if descriptor >= 0:
            os.close(descriptor)
        for current in reversed(directories):
            os.close(current)
        raise CacheBundleVerificationError(f"cannot open {label}") from exc


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


def _read_checked_v1_source() -> bytes:
    descriptor, directories = _open_source_descriptor(V1_SOURCE_PATH, label="v1 raw verifier source")
    try:
        before = os.fstat(descriptor)
        _require(before.st_size == CHECKED_V1_SOURCE_SIZE, "v1 raw verifier source size differs")
        chunks: list[bytes] = []
        remaining = CHECKED_V1_SOURCE_SIZE + 1
        while remaining:
            chunk = os.read(descriptor, min(READ_CHUNK_BYTES, remaining))
            if not chunk:
                break
            chunks.append(chunk)
            remaining -= len(chunk)
        raw = b"".join(chunks)
        after = os.fstat(descriptor)
        _require(
            len(raw) == CHECKED_V1_SOURCE_SIZE
            and _stat_identity(before) == _stat_identity(after),
            "v1 raw verifier source changed while reading",
        )
        _require(_sha256(raw) == CHECKED_V1_SOURCE_SHA256, "v1 raw verifier source hash differs")
        return raw
    finally:
        os.close(descriptor)
        for current in reversed(directories):
            os.close(current)


def _checked_v1() -> types.ModuleType:
    global _V1_MODULE

    raw = _read_checked_v1_source()
    if _V1_MODULE is not None:
        return _V1_MODULE
    module_name = "_entire_brain_checked_negative_control_cache_archive_v1"
    module = types.ModuleType(module_name)
    module.__file__ = str(V1_SOURCE_PATH)
    previous = sys.modules.get(module_name)
    try:
        code = compile(raw, str(V1_SOURCE_PATH), "exec", dont_inherit=True)
        sys.modules[module_name] = module
        exec(code, module.__dict__)
    except Exception as exc:
        raise CacheBundleVerificationError("checked v1 raw verifier source cannot be loaded") from exc
    finally:
        if previous is None:
            sys.modules.pop(module_name, None)
        else:
            sys.modules[module_name] = previous
    required = (
        "_audit_schema",
        "_read_bounded",
        "_load_fixed_dependencies",
        "_load_schema",
        "_read_json",
        "_render",
        "_canonical_bytes",
        "_field_self_hash",
        "_validate_schema",
        "_validate_manifest_identity",
        "_open_regular_descriptor",
        "_stable_stat_identity",
        "check_verifier_contract",
        "CacheArchiveVerificationError",
    )
    _require(all(hasattr(module, name) for name in required), "checked v1 raw verifier API differs")
    _V1_MODULE = module
    return module


def _manifest_projection_sha256(manifest: Mapping[str, Any]) -> str:
    v1 = _checked_v1()
    projected = copy.deepcopy(dict(manifest))
    archive = projected.get("archive")
    _require(type(archive) is dict, "manifest archive projection differs")
    archive_map = cast(dict[str, Any], archive)
    archive_map["byte_count"] = None
    archive_map["sha256"] = None
    _require("manifest_sha256" in projected, "manifest self hash is absent")
    projected["manifest_sha256"] = None
    try:
        canonical = v1._canonical_bytes(projected)
    except v1.CacheArchiveVerificationError as exc:
        raise CacheBundleVerificationError(str(exc)) from exc
    return hashlib.sha256(MANIFEST_PROJECTION_DOMAIN + canonical).hexdigest()


def _validate_bundle_path_policy(manifest: Mapping[str, Any]) -> None:
    """Reject APFS named/resource-fork pseudo-components before byte parsing."""

    contents_value = manifest.get("contents")
    _require(type(contents_value) is dict, "manifest contents differ")
    contents = cast(dict[str, Any], contents_value)
    entries_value = contents.get("entries")
    _require(type(entries_value) is list, "manifest entries differ")
    for index, entry_value in enumerate(cast(list[Any], entries_value)):
        _require(type(entry_value) is dict, f"manifest entry[{index}] differs")
        entry = cast(dict[str, Any], entry_value)
        path_value = entry.get("path")
        _require(type(path_value) is str, f"manifest entry[{index}] path differs")
        path = cast(str, path_value)
        _require(
            all(component.casefold() != "..namedfork" for component in path.split("/")),
            f"manifest entry[{index}] APFS ..namedfork component is forbidden",
        )


class _BundleReader:
    """Checked-subtraction reader and domain-separated pass-two hash feeder."""

    def __init__(self, descriptor: int, bundle_bytes: int) -> None:
        _require(
            type(bundle_bytes) is int
            and MIN_BUNDLE_BYTES <= bundle_bytes <= MAX_BUNDLE_BYTES,
            "bundle byte count is outside the frozen ceiling",
        )
        self.descriptor = descriptor
        self.bundle_bytes = bundle_bytes
        self.remaining = bundle_bytes
        self.offset = 0
        self.trailer_offset = bundle_bytes - TRAILER_BYTES
        self.prefix_limit = bundle_bytes - 32
        self.records_digest = hashlib.sha256(RECORDS_DOMAIN)
        self.prefix_digest = hashlib.sha256(PREFIX_DOMAIN)

    def _feed(self, raw: bytes, start: int) -> None:
        end = start + len(raw)
        prefix_end = min(end, self.prefix_limit)
        if start < prefix_end:
            self.prefix_digest.update(raw[: prefix_end - start])
        records_start = max(start, HEADER_BYTES)
        records_end = min(end, self.trailer_offset)
        if records_start < records_end:
            left = records_start - start
            self.records_digest.update(raw[left : left + records_end - records_start])

    def read_exact(self, count: int, *, label: str, tail_reserve: int = 0) -> bytes:
        _require(type(count) is int and count >= 0, f"{label} byte count is invalid")
        _require(
            type(tail_reserve) is int
            and tail_reserve >= 0
            and tail_reserve <= self.remaining
            and count <= self.remaining - tail_reserve,
            f"{label} exceeds the remaining bundle bytes",
        )
        chunks: list[bytes] = []
        needed = count
        start = self.offset
        while needed:
            try:
                chunk = os.read(self.descriptor, min(READ_CHUNK_BYTES, needed))
            except OSError as exc:
                raise CacheBundleVerificationError(f"cannot read {label}") from exc
            _require(bool(chunk), f"{label} is truncated")
            chunks.append(chunk)
            needed -= len(chunk)
        raw = b"".join(chunks)
        self._feed(raw, start)
        self.offset += count
        self.remaining -= count
        return raw

    def hash_content(
        self,
        count: int,
        *,
        expected_sha256: str,
        label: str,
        tail_reserve: int,
    ) -> None:
        _require(
            type(count) is int
            and count >= 1
            and tail_reserve <= self.remaining
            and count <= self.remaining - tail_reserve,
            f"{label} exceeds the remaining bundle bytes",
        )
        digest = hashlib.sha256()
        needed = count
        while needed:
            chunk_count = min(READ_CHUNK_BYTES, needed)
            chunk = self.read_exact(
                chunk_count,
                label=label,
                tail_reserve=tail_reserve,
            )
            digest.update(chunk)
            needed -= len(chunk)
        _require(digest.hexdigest() == expected_sha256, f"{label} SHA-256 differs")


def _raw_identity_pass(
    descriptor: int,
    *,
    expected_byte_count: int,
    expected_sha256: str,
) -> tuple[dict[str, Any], os.stat_result]:
    before = os.fstat(descriptor)
    _require(stat.S_ISREG(before.st_mode), "cache bundle is not a regular file")
    _require(before.st_nlink == 1, "cache bundle must not be hard-linked")
    _require(
        before.st_size == expected_byte_count
        and MIN_BUNDLE_BYTES <= before.st_size <= MAX_BUNDLE_BYTES,
        "cache bundle byte count differs",
    )
    try:
        _require(os.lseek(descriptor, 0, os.SEEK_CUR) == 0, "cache bundle initial offset differs")
    except OSError as exc:
        raise CacheBundleVerificationError("cache bundle offset is unavailable") from exc
    digest = hashlib.sha256()
    byte_count = 0
    while True:
        try:
            chunk = os.read(descriptor, READ_CHUNK_BYTES)
        except OSError as exc:
            raise CacheBundleVerificationError("cannot read cache bundle raw identity") from exc
        if not chunk:
            break
        byte_count += len(chunk)
        _require(byte_count <= expected_byte_count, "cache bundle exceeds its reviewed byte count")
        digest.update(chunk)
    after = os.fstat(descriptor)
    _require(
        byte_count == expected_byte_count
        and _stat_identity(before) == _stat_identity(after),
        "cache bundle changed during raw identity verification",
    )
    identity = {"byte_count": byte_count, "sha256": digest.hexdigest()}
    _require(
        identity == {"byte_count": expected_byte_count, "sha256": expected_sha256},
        "cache bundle raw identity differs from reviewed v1 material",
    )
    try:
        position = os.lseek(descriptor, 0, os.SEEK_SET)
    except OSError as exc:
        raise CacheBundleVerificationError("cache bundle cannot be rewound") from exc
    _require(position == 0, "cache bundle rewind differs")
    return identity, before


def _parse_content_pass(
    descriptor: int,
    *,
    bundle_stat: os.stat_result,
    manifest: Mapping[str, Any],
    format_contract: Mapping[str, Any],
) -> dict[str, Any]:
    _validate_bundle_path_policy(manifest)
    contents = manifest.get("contents")
    _require(type(contents) is dict, "manifest contents differ")
    contents_map = cast(dict[str, Any], contents)
    entries_value = contents_map.get("entries")
    _require(type(entries_value) is list, "manifest entries differ")
    entries = cast(list[Any], entries_value)
    file_count_value = contents_map.get("file_count")
    unpacked_bytes_value = contents_map.get("unpacked_byte_count")
    repository_counts_value = contents_map.get("repository_file_counts")
    _require(
        type(file_count_value) is int
        and 3 <= file_count_value <= MAX_FILE_COUNT
        and file_count_value == len(entries),
        "manifest file count differs",
    )
    file_count = cast(int, file_count_value)
    _require(
        type(unpacked_bytes_value) is int
        and 1 <= unpacked_bytes_value <= MAX_UNPACKED_BYTES,
        "manifest unpacked byte count differs",
    )
    unpacked_bytes = cast(int, unpacked_bytes_value)
    _require(
        type(repository_counts_value) is dict
        and set(repository_counts_value) == set(REPOSITORY_ORDER),
        "manifest repository counts differ",
    )
    repository_counts_map = cast(dict[str, Any], repository_counts_value)
    _require(
        all(
            type(repository_counts_map[key]) is int
            and cast(int, repository_counts_map[key]) >= 1
            for key in REPOSITORY_ORDER
        ),
        "manifest repository counts are outside the frozen limits",
    )
    repository_counts = {
        key: cast(int, repository_counts_map[key]) for key in REPOSITORY_ORDER
    }
    _require(
        sum(repository_counts.values()) == file_count,
        "manifest repository counts do not sum to the file count",
    )
    path_total = 0
    for index, entry in enumerate(entries):
        _require(type(entry) is dict, f"manifest entry[{index}] differs")
        entry_map = cast(dict[str, Any], entry)
        path_value = entry_map.get("path")
        _require(type(path_value) is str, f"manifest entry[{index}] path differs")
        path_total += len(cast(str, path_value).encode("utf-8"))
        _require(path_total <= MAX_BUNDLE_BYTES, "manifest path bytes exceed the bundle ceiling")
    expected_bundle_bytes = (
        HEADER_BYTES
        + TRAILER_BYTES
        + RECORD_FIXED_BYTES * file_count
        + path_total
        + unpacked_bytes
    )
    _require(
        expected_bundle_bytes == bundle_stat.st_size
        and expected_bundle_bytes <= MAX_BUNDLE_BYTES,
        "cache bundle exact byte-count formula differs",
    )

    reader = _BundleReader(descriptor, bundle_stat.st_size)
    header_raw = reader.read_exact(HEADER_BYTES, label="cache bundle header", tail_reserve=TRAILER_BYTES)
    try:
        header = struct.unpack(">16sHHIQQQQQQ32s32s32s32s32s", header_raw)
    except struct.error as exc:
        raise CacheBundleVerificationError("cache bundle header layout differs") from exc
    (
        magic,
        version,
        header_bytes,
        flags,
        declared_bundle_bytes,
        declared_file_count,
        declared_unpacked_bytes,
        brain_count,
        db_count,
        graph_count,
        format_sha,
        manifest_projection_sha,
        inventory_sha,
        plan_sha,
        toolchain_sha,
    ) = header
    expected_format_sha = format_contract.get("format_contract_sha256")
    _require(
        magic == HEADER_MAGIC
        and version == FORMAT_VERSION
        and header_bytes == HEADER_BYTES
        and flags == 0,
        "cache bundle header identity differs",
    )
    _require(
        declared_bundle_bytes == bundle_stat.st_size
        and declared_file_count == file_count
        and declared_unpacked_bytes == unpacked_bytes
        and (brain_count, db_count, graph_count)
        == tuple(repository_counts[key] for key in REPOSITORY_ORDER),
        "cache bundle header counts differ from manifest",
    )
    run_plan_value = manifest.get("run_plan")
    _require(type(run_plan_value) is dict, "manifest run-plan binding differs")
    run_plan = cast(dict[str, Any], run_plan_value)
    _require(
        type(expected_format_sha) is str
        and format_sha.hex() == expected_format_sha
        and manifest_projection_sha.hex() == _manifest_projection_sha256(manifest)
        and inventory_sha.hex() == contents_map.get("inventory_sha256")
        and plan_sha.hex() == run_plan.get("plan_sha256")
        and toolchain_sha.hex()
        == run_plan.get("toolchain_bindings_sha256"),
        "cache bundle header bindings differ",
    )

    unpacked_total = 0
    observed_counts = {key: 0 for key in REPOSITORY_ORDER}
    for ordinal, entry in enumerate(entries):
        _require(type(entry) is dict, f"manifest entry[{ordinal}] differs")
        remaining_records = file_count - ordinal - 1
        minimum_tail = TRAILER_BYTES + remaining_records * (RECORD_FIXED_BYTES + 1 + 1)
        fixed = reader.read_exact(
            RECORD_FIXED_BYTES,
            label=f"cache bundle record[{ordinal}] header",
            tail_reserve=minimum_tail,
        )
        try:
            record_magic, repository_id, record_flags, path_bytes, record_ordinal, content_bytes, content_sha = struct.unpack(
                ">4sBBHIQ32s", fixed
            )
        except struct.error as exc:
            raise CacheBundleVerificationError(f"cache bundle record[{ordinal}] layout differs") from exc
        _require(
            record_magic == RECORD_MAGIC
            and record_flags == 0
            and record_ordinal == ordinal,
            f"cache bundle record[{ordinal}] identity differs",
        )
        repository_key = entry.get("repository_key")
        _require(
            repository_key in REPOSITORY_IDS
            and repository_id == REPOSITORY_IDS[cast(str, repository_key)],
            f"cache bundle record[{ordinal}] repository differs",
        )
        _require(1 <= path_bytes <= MAX_PATH_BYTES, f"cache bundle record[{ordinal}] path length differs")
        path_raw = reader.read_exact(
            path_bytes,
            label=f"cache bundle record[{ordinal}] path",
            tail_reserve=minimum_tail + content_bytes,
        )
        try:
            path = path_raw.decode("utf-8")
        except UnicodeError as exc:
            raise CacheBundleVerificationError(f"cache bundle record[{ordinal}] path is not UTF-8") from exc
        _require(
            path == entry.get("path")
            and path_raw == cast(str, entry.get("path")).encode("utf-8"),
            f"cache bundle record[{ordinal}] path differs from manifest",
        )
        _require(
            type(content_bytes) is int
            and content_bytes == entry.get("byte_count")
            and 1 <= content_bytes <= MAX_UNPACKED_BYTES
            and content_sha.hex() == entry.get("sha256"),
            f"cache bundle record[{ordinal}] content declaration differs",
        )
        unpacked_total += content_bytes
        _require(unpacked_total <= MAX_UNPACKED_BYTES, "cache bundle unpacked bytes exceed the ceiling")
        reader.hash_content(
            content_bytes,
            expected_sha256=content_sha.hex(),
            label=f"cache bundle record[{ordinal}] content",
            tail_reserve=minimum_tail,
        )
        observed_counts[cast(str, repository_key)] += 1

    trailer_raw = reader.read_exact(TRAILER_BYTES, label="cache bundle trailer")
    try:
        trailer = struct.unpack(">16sQQQ32s32s", trailer_raw)
    except struct.error as exc:
        raise CacheBundleVerificationError("cache bundle trailer layout differs") from exc
    trailer_magic, trailer_bundle_bytes, trailer_file_count, trailer_unpacked, records_sha, prefix_sha = trailer
    _require(
        trailer_magic == TRAILER_MAGIC
        and trailer_bundle_bytes == bundle_stat.st_size
        and trailer_file_count == file_count
        and trailer_unpacked == unpacked_bytes,
        "cache bundle trailer counts differ",
    )
    _require(reader.remaining == 0 and reader.offset == bundle_stat.st_size, "cache bundle has trailing bytes")
    _require(unpacked_total == unpacked_bytes, "cache bundle content byte total differs")
    _require(observed_counts == repository_counts, "cache bundle repository totals differ")
    _require(records_sha == reader.records_digest.digest(), "cache bundle records digest differs")
    _require(prefix_sha == reader.prefix_digest.digest(), "cache bundle prefix digest differs")
    try:
        _require(os.read(descriptor, 1) == b"", "cache bundle exact EOF differs")
    except OSError as exc:
        raise CacheBundleVerificationError("cannot verify cache bundle EOF") from exc
    after = os.fstat(descriptor)
    _require(
        _stat_identity(after) == _stat_identity(bundle_stat),
        "cache bundle changed during content verification",
    )
    return {
        "bundle_byte_count": bundle_stat.st_size,
        "file_count": file_count,
        "records_sha256": records_sha.hex(),
        "prefix_sha256": prefix_sha.hex(),
        "repository_file_counts": copy.deepcopy(repository_counts),
        "unpacked_byte_count": unpacked_bytes,
    }


def _file_binding(path: pathlib.Path, raw: bytes) -> dict[str, str]:
    return {"artifact_file": path.name, "artifact_sha256": _sha256(raw)}


def _validate_format_contract(
    value: Mapping[str, Any],
    *,
    schema: Mapping[str, Any],
    v1: types.ModuleType,
) -> dict[str, Any]:
    try:
        v1._validate_schema(
            value,
            schema,
            schema_name=FORMAT_SCHEMA_PATH.name,
            label="cache bundle format contract",
        )
    except v1.CacheArchiveVerificationError as exc:
        raise CacheBundleVerificationError(str(exc)) from exc
    expected_fields = {
        "authority",
        "canonical_json_profile",
        "digests",
        "format_contract_sha256",
        "header",
        "integer_encoding",
        "limits",
        "manifest_projection",
        "ordering",
        "profile",
        "record",
        "representability",
        "schema_version",
        "status",
        "trailer",
    }
    _require(type(value) is dict and set(value) == expected_fields, "format-contract fields differ")
    _require(
        value["profile"] == FORMAT_PROFILE
        and value["schema_version"] == SCHEMA_VERSION
        and value["status"] == FORMAT_STATUS,
        "format-contract identity differs",
    )
    try:
        self_hash = v1._field_self_hash(value, "format_contract_sha256")
    except v1.CacheArchiveVerificationError as exc:
        raise CacheBundleVerificationError(str(exc)) from exc
    _require(
        value["format_contract_sha256"] == CHECKED_FORMAT_SHA256
        and value["format_contract_sha256"] == self_hash,
        "format-contract self hash differs",
    )
    return copy.deepcopy(dict(value))


def _load_fixed_dependencies() -> dict[str, Any]:
    v1 = _checked_v1()
    try:
        v1_dependencies = v1._load_fixed_dependencies()
        v1_verifier_contract = v1.check_verifier_contract()
        v1_verifier_contract_read, v1_verifier_contract_raw = v1._read_json(
            V1_VERIFIER_CONTRACT_PATH,
            maximum=MAX_VERIFIER_CONTRACT_BYTES,
            label="v1 cache archive verifier contract",
            canonical=True,
            require_single_link=True,
        )
        _require(
            v1_verifier_contract_read == v1_verifier_contract
            and v1_verifier_contract_raw == v1._render(v1_verifier_contract),
            "v1 cache archive verifier contract differs",
        )
        format_schema, format_schema_raw = v1._load_schema(
            FORMAT_SCHEMA_PATH,
            expected_sha256=CHECKED_FORMAT_SCHEMA_SHA256,
            label="cache bundle format schema",
        )
        report_schema, report_schema_raw = v1._load_schema(
            REPORT_SCHEMA_PATH,
            expected_sha256=CHECKED_REPORT_SCHEMA_SHA256,
            label="cache bundle report schema",
        )
        verifier_schema, verifier_schema_raw = v1._load_schema(
            VERIFIER_SCHEMA_PATH,
            expected_sha256=CHECKED_VERIFIER_SCHEMA_SHA256,
            label="cache bundle verifier schema",
        )
        for schema, path, label in (
            (format_schema, FORMAT_SCHEMA_PATH, "cache bundle format"),
            (report_schema, REPORT_SCHEMA_PATH, "cache bundle verification report"),
            (verifier_schema, VERIFIER_SCHEMA_PATH, "cache bundle verifier contract"),
        ):
            v1._audit_schema(schema, schema_name=path.name, label=label)
        format_contract, format_raw = v1._read_json(
            FORMAT_PATH,
            maximum=v1.MAX_FIXED_JSON_BYTES,
            label="cache bundle format contract",
            canonical=True,
        )
    except v1.CacheArchiveVerificationError as exc:
        raise CacheBundleVerificationError(str(exc)) from exc
    _require(_sha256(format_raw) == CHECKED_FORMAT_FILE_SHA256, "format-contract raw hash differs")
    validated_format = _validate_format_contract(format_contract, schema=format_schema, v1=v1)
    return {
        "format": validated_format,
        "format_raw": format_raw,
        "format_schema": format_schema,
        "format_schema_raw": format_schema_raw,
        "report_schema": report_schema,
        "report_schema_raw": report_schema_raw,
        "v1": v1,
        "v1_dependencies": v1_dependencies,
        "v1_verifier_contract": v1_verifier_contract,
        "v1_verifier_contract_raw": v1_verifier_contract_raw,
        "verifier_schema": verifier_schema,
        "verifier_schema_raw": verifier_schema_raw,
    }


def _expected_implementation(dependencies: Mapping[str, Any]) -> dict[str, Any]:
    v1 = dependencies["v1"]
    try:
        source_raw = v1._read_bounded(
            pathlib.Path(__file__),
            maximum=MAX_SOURCE_BYTES,
            label="cache bundle verifier source",
            require_single_link=True,
        )
    except v1.CacheArchiveVerificationError as exc:
        raise CacheBundleVerificationError(str(exc)) from exc
    v1_dependencies = dependencies["v1_dependencies"]
    _require(type(v1_dependencies) is dict, "v1 fixed dependencies differ")
    return {
        "builder": _file_binding(pathlib.Path(__file__), source_raw),
        "draft202012_validator": copy.deepcopy(v1_dependencies["draft_validator"]),
        "format_artifact": _file_binding(FORMAT_PATH, dependencies["format_raw"]),
        "format_schema": _file_binding(FORMAT_SCHEMA_PATH, dependencies["format_schema_raw"]),
        "manifest_schema": _file_binding(
            pathlib.Path("offline-go-cache-seed-manifest-v1.schema.json"),
            v1_dependencies["manifest_schema_raw"],
        ),
        "report_schema": _file_binding(REPORT_SCHEMA_PATH, dependencies["report_schema_raw"]),
        "v1_raw_verifier_source": _file_binding(V1_SOURCE_PATH, _read_checked_v1_source()),
        "verifier_contract_schema": _file_binding(
            VERIFIER_SCHEMA_PATH,
            dependencies["verifier_schema_raw"],
        ),
    }


def _expected_raw_identity_binding(dependencies: Mapping[str, Any]) -> dict[str, Any]:
    v1_dependencies = dependencies["v1_dependencies"]
    material = v1_dependencies["material"]
    v1_contract = dependencies["v1_verifier_contract"]
    _require(
        type(material) is dict and type(v1_contract) is dict,
        "v1 raw identity dependencies differ",
    )
    return {
        "v1_material": {
            "artifact_file": "negative-control-cache-archive-material-v1.json",
            "artifact_sha256": _sha256(v1_dependencies["material_raw"]),
            "material_declaration_sha256": material["material_declaration_sha256"],
            "profile": material["profile"],
            "status": material["status"],
        },
        "v1_material_schema_sha256": "1424cd22b5eb057b5794347403780435f95c0428d4f81ff42441e71abc39d5d5",
        "v1_raw_verifier_source_sha256": CHECKED_V1_SOURCE_SHA256,
        "v1_report_schema_sha256": "5827c78df73b024df32bf2853d412df428e7362e869d5d202b33dac15bbe4e5d",
        "v1_verifier_contract": {
            "artifact_file": V1_VERIFIER_CONTRACT_PATH.name,
            "artifact_sha256": _sha256(dependencies["v1_verifier_contract_raw"]),
            "profile": v1_contract["profile"],
            "status": v1_contract["status"],
            "verifier_contract_sha256": v1_contract["verifier_contract_sha256"],
        },
        "v1_verifier_contract_schema_sha256": "c35c0de98dbdcec6744a3fe33ee363261507eb894bbddbb3af5af3642be7f914",
    }


def _expected_status(dependencies: Mapping[str, Any]) -> str:
    material = dependencies["v1_dependencies"]["material"]
    v1_contract = dependencies["v1_verifier_contract"]
    _require(
        type(material) is dict and type(v1_contract) is dict,
        "v1 approval dependencies differ",
    )
    material_status = material.get("status")
    contract_status = v1_contract.get("status")
    if material_status == V1_PENDING_MATERIAL_STATUS:
        _require(
            contract_status == V1_PENDING_VERIFIER_STATUS,
            "pending v1 material and verifier statuses differ",
        )
        return PENDING_STATUS
    _require(
        material_status == V1_APPROVED_MATERIAL_STATUS
        and contract_status == V1_APPROVED_VERIFIER_STATUS,
        "approved v1 material and verifier statuses differ",
    )
    return APPROVED_STATUS


def _build_verifier_contract(dependencies: Mapping[str, Any]) -> dict[str, Any]:
    v1 = dependencies["v1"]
    contract: dict[str, Any] = {
        "authority": copy.deepcopy(AUTHORITY),
        "format_binding": copy.deepcopy(FORMAT_BINDING),
        "implementation": _expected_implementation(dependencies),
        "manifest_binding": copy.deepcopy(MANIFEST_BINDING),
        "profile": VERIFIER_PROFILE,
        "raw_identity_binding": _expected_raw_identity_binding(dependencies),
        "residual_gates": list(RESIDUAL_GATES),
        "runtime_bindings": copy.deepcopy(RUNTIME_BINDINGS),
        "schema_version": SCHEMA_VERSION,
        "status": _expected_status(dependencies),
        "verification_policy": copy.deepcopy(VERIFICATION_POLICY),
        "verifier_contract_sha256": None,
    }
    contract["verifier_contract_sha256"] = v1._field_self_hash(
        contract,
        "verifier_contract_sha256",
    )
    return validate_verifier_contract(
        contract,
        schema=dependencies["verifier_schema"],
        dependencies=dependencies,
    )


def build_verifier_contract() -> dict[str, Any]:
    """Build a non-authorizing verifier contract from fixed local inputs."""

    return _build_verifier_contract(_load_fixed_dependencies())


def validate_verifier_contract(
    value: Mapping[str, Any],
    *,
    schema: Mapping[str, Any],
    dependencies: Mapping[str, Any],
) -> dict[str, Any]:
    v1 = dependencies["v1"]
    try:
        v1._validate_schema(
            value,
            schema,
            schema_name=VERIFIER_SCHEMA_PATH.name,
            label="cache bundle verifier contract",
        )
    except v1.CacheArchiveVerificationError as exc:
        raise CacheBundleVerificationError(str(exc)) from exc
    expected_fields = {
        "authority",
        "format_binding",
        "implementation",
        "manifest_binding",
        "profile",
        "raw_identity_binding",
        "residual_gates",
        "runtime_bindings",
        "schema_version",
        "status",
        "verification_policy",
        "verifier_contract_sha256",
    }
    _require(type(value) is dict and set(value) == expected_fields, "verifier-contract fields differ")
    _require(
        value["profile"] == VERIFIER_PROFILE
        and value["schema_version"] == SCHEMA_VERSION
        and value["status"] == _expected_status(dependencies),
        "verifier-contract identity or status differs",
    )
    _require(value["authority"] == AUTHORITY, "verifier-contract authority differs")
    _require(value["format_binding"] == FORMAT_BINDING, "verifier-contract format binding differs")
    _require(value["manifest_binding"] == MANIFEST_BINDING, "verifier-contract manifest binding differs")
    _require(
        value["raw_identity_binding"] == _expected_raw_identity_binding(dependencies),
        "verifier-contract raw identity binding differs",
    )
    _require(
        value["implementation"] == _expected_implementation(dependencies),
        "verifier-contract implementation binding differs",
    )
    _require(value["residual_gates"] == RESIDUAL_GATES, "verifier-contract residual gates differ")
    _require(value["runtime_bindings"] == RUNTIME_BINDINGS, "verifier-contract runtime bindings differ")
    _require(value["verification_policy"] == VERIFICATION_POLICY, "verifier-contract policy differs")
    self_hash = value["verifier_contract_sha256"]
    _require(
        type(self_hash) is str
        and len(self_hash) == 64
        and self_hash == v1._field_self_hash(value, "verifier_contract_sha256"),
        "verifier-contract self hash differs",
    )
    return copy.deepcopy(dict(value))


def _check_verifier_contract(dependencies: Mapping[str, Any]) -> dict[str, Any]:
    v1 = dependencies["v1"]
    try:
        value, raw = v1._read_json(
            VERIFIER_CONTRACT_PATH,
            maximum=MAX_VERIFIER_CONTRACT_BYTES,
            label="cache bundle verifier contract",
            canonical=True,
            require_single_link=True,
        )
    except v1.CacheArchiveVerificationError as exc:
        raise CacheBundleVerificationError(str(exc)) from exc
    validated = validate_verifier_contract(
        value,
        schema=dependencies["verifier_schema"],
        dependencies=dependencies,
    )
    expected = _build_verifier_contract(dependencies)
    _require(raw == v1._render(expected) and validated == expected, "checked verifier contract differs")
    return validated


def check_verifier_contract() -> dict[str, Any]:
    """Check only the fixed production verifier contract and dependencies."""

    return _check_verifier_contract(_load_fixed_dependencies())


def _build_verification_report(
    *,
    bundle_identity: Mapping[str, Any],
    content_summary: Mapping[str, Any],
    manifest: Mapping[str, Any],
    manifest_raw: bytes,
    dependencies: Mapping[str, Any],
    verifier_contract: Mapping[str, Any],
) -> dict[str, Any]:
    material = dependencies["v1_dependencies"]["material"]
    declared = material["material"]
    contents = manifest["contents"]
    run_plan = manifest["run_plan"]
    v1_contract = dependencies["v1_verifier_contract"]
    _require(
        all(type(value) is dict for value in (declared, contents, run_plan, v1_contract)),
        "verified report inputs differ",
    )
    report: dict[str, Any] = {
        "authority": copy.deepcopy(REPORT_AUTHORITY),
        "bindings": {
            "binding_status": "same_descriptor_raw_identity_recomputed_against_source_reviewed_v1_material",
            "format_artifact_sha256": CHECKED_FORMAT_FILE_SHA256,
            "format_contract_sha256": CHECKED_FORMAT_SHA256,
            "v1_material_artifact_sha256": _sha256(dependencies["v1_dependencies"]["material_raw"]),
            "v1_material_declaration_sha256": material["material_declaration_sha256"],
            "v1_verifier_contract_artifact_sha256": _sha256(
                dependencies["v1_verifier_contract_raw"]
            ),
            "v1_verifier_contract_sha256": v1_contract["verifier_contract_sha256"],
            "v2_verifier_contract_sha256": verifier_contract["verifier_contract_sha256"],
        },
        "bundle": {
            "artifact_file": declared["archive_file"],
            "byte_count": bundle_identity["byte_count"],
            "flags": 0,
            "format_version": FORMAT_VERSION,
            "header_byte_count": HEADER_BYTES,
            "prefix_sha256": content_summary["prefix_sha256"],
            "raw_sha256": bundle_identity["sha256"],
            "record_fixed_byte_count": RECORD_FIXED_BYTES,
            "records_sha256": content_summary["records_sha256"],
            "trailer_byte_count": TRAILER_BYTES,
        },
        "contents": {
            "all_payload_sha256_verified": True,
            "file_count": content_summary["file_count"],
            "manifest_bijection": True,
            "repository_file_counts": copy.deepcopy(
                content_summary["repository_file_counts"]
            ),
            "shared_seed_semantics": "one_shared_union_seed_copied_unchanged_to_every_arm",
            "unpacked_byte_count": content_summary["unpacked_byte_count"],
        },
        "manifest": {
            "artifact_file": declared["manifest_file"],
            "artifact_sha256": _sha256(manifest_raw),
            "inventory_sha256": contents["inventory_sha256"],
            "manifest_sha256": manifest["manifest_sha256"],
            "profile": manifest["profile"],
            "projection_sha256": _manifest_projection_sha256(manifest),
            "run_plan_sha256": run_plan["plan_sha256"],
            "schema_sha256": MANIFEST_BINDING["manifest_schema_sha256"],
            "toolchain_bindings_sha256": run_plan["toolchain_bindings_sha256"],
        },
        "profile": REPORT_PROFILE,
        "report_sha256": None,
        "safety": copy.deepcopy(REPORT_SAFETY),
        "schema_version": SCHEMA_VERSION,
        "status": REPORT_STATUS,
        "structure": copy.deepcopy(REPORT_STRUCTURE),
    }
    v1 = dependencies["v1"]
    report["report_sha256"] = v1._field_self_hash(report, "report_sha256")
    return _validate_verification_report(
        report,
        schema=dependencies["report_schema"],
        v1=v1,
    )


def _validate_verification_report(
    value: Mapping[str, Any],
    *,
    schema: Mapping[str, Any],
    v1: types.ModuleType,
) -> dict[str, Any]:
    try:
        v1._validate_schema(
            value,
            schema,
            schema_name=REPORT_SCHEMA_PATH.name,
            label="cache bundle verification report",
        )
    except v1.CacheArchiveVerificationError as exc:
        raise CacheBundleVerificationError(str(exc)) from exc
    expected_fields = {
        "authority",
        "bindings",
        "bundle",
        "contents",
        "manifest",
        "profile",
        "report_sha256",
        "safety",
        "schema_version",
        "status",
        "structure",
    }
    _require(type(value) is dict and set(value) == expected_fields, "verification-report fields differ")
    _require(
        value["profile"] == REPORT_PROFILE
        and value["schema_version"] == SCHEMA_VERSION
        and value["status"] == REPORT_STATUS,
        "verification-report identity differs",
    )
    _require(value["authority"] == REPORT_AUTHORITY, "verification-report authority differs")
    _require(value["safety"] == REPORT_SAFETY, "verification-report safety differs")
    _require(value["structure"] == REPORT_STRUCTURE, "verification-report structure differs")
    report_hash = value["report_sha256"]
    _require(
        type(report_hash) is str
        and len(report_hash) == 64
        and report_hash == v1._field_self_hash(value, "report_sha256"),
        "verification-report self hash differs",
    )
    return copy.deepcopy(dict(value))


def _verify_with_dependencies(
    manifest_path: pathlib.Path,
    bundle_path: pathlib.Path,
    *,
    dependencies: Mapping[str, Any],
    verifier_contract: Mapping[str, Any],
) -> dict[str, Any]:
    """Private synthetic seam; production callers use fixed dependencies."""

    v1 = dependencies["v1"]
    validated_contract = validate_verifier_contract(
        verifier_contract,
        schema=dependencies["verifier_schema"],
        dependencies=dependencies,
    )
    material = dependencies["v1_dependencies"]["material"]
    _require(type(material) is dict, "v1 material dependency differs")
    _require(
        material.get("status") == V1_APPROVED_MATERIAL_STATUS
        and validated_contract["status"] == APPROVED_STATUS,
        "cache bundle material source review is pending; locator access forbidden",
    )

    declared = material.get("material")
    _require(type(declared) is dict, "approved v1 material identities differ")
    declared_map = cast(dict[str, Any], declared)
    _require(
        manifest_path.name == declared_map["manifest_file"]
        and bundle_path.name == declared_map["archive_file"],
        "cache bundle locator filename differs from reviewed identity",
    )
    try:
        manifest, manifest_raw = v1._read_json(
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
        validated_manifest = v1._validate_manifest_identity(
            manifest,
            schema=dependencies["v1_dependencies"]["manifest_schema"],
            material=material,
        )
        _validate_bundle_path_policy(validated_manifest)
        descriptor, directories = v1._open_regular_descriptor(
            bundle_path,
            label="reviewed cache bundle",
        )
    except v1.CacheArchiveVerificationError as exc:
        raise CacheBundleVerificationError(str(exc)) from exc

    try:
        bundle_identity, bundle_stat = _raw_identity_pass(
            descriptor,
            expected_byte_count=declared_map["archive_byte_count"],
            expected_sha256=declared_map["archive_sha256"],
        )
        content_summary = _parse_content_pass(
            descriptor,
            bundle_stat=bundle_stat,
            manifest=validated_manifest,
            format_contract=dependencies["format"],
        )
    finally:
        os.close(descriptor)
        for current in reversed(directories):
            os.close(current)
    return _build_verification_report(
        bundle_identity=bundle_identity,
        content_summary=content_summary,
        manifest=validated_manifest,
        manifest_raw=manifest_raw,
        dependencies=dependencies,
        verifier_contract=validated_contract,
    )


def verify_bundle_files(
    manifest_path: pathlib.Path,
    bundle_path: pathlib.Path,
) -> dict[str, Any]:
    """Verify reviewed bundle contents without extracting, staging, or executing."""

    dependencies = _load_fixed_dependencies()
    verifier_contract = _check_verifier_contract(dependencies)
    material = dependencies["v1_dependencies"]["material"]
    _require(
        type(material) is dict and material.get("status") == V1_APPROVED_MATERIAL_STATUS,
        "cache bundle material source review is pending; locator access forbidden",
    )
    return _verify_with_dependencies(
        manifest_path,
        bundle_path,
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
            handle.write(_checked_v1()._render(value))
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
    verify = commands.add_parser("verify", help="verify reviewed bundle contents without staging")
    verify.add_argument("manifest", type=pathlib.Path)
    verify.add_argument("bundle", type=pathlib.Path)
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
            report = verify_bundle_files(args.manifest, args.bundle)
            print(_checked_v1()._canonical_bytes(report).decode("utf-8"))
    except CacheBundleVerificationError as exc:
        parser.error(str(exc))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
