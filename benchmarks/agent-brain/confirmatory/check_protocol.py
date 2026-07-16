#!/usr/bin/env python3
"""Offline consistency and freeze-gate checks for the WS6 protocol artifacts."""

from __future__ import annotations

import argparse
import copy
import datetime as dt
from datetime import datetime, timedelta, timezone
from decimal import Decimal
import hashlib
import json
import math
import os
import pathlib
import re
import stat
import struct
import subprocess
import sys
from typing import Any


HERE = pathlib.Path(__file__).resolve().parent
REPO = HERE.parents[2]
C0701 = REPO / "benchmarks" / "agent-brain" / "mined-c0701"
if str(HERE) not in sys.path:
    sys.path.insert(0, str(HERE))

import power_analysis  # noqa: E402  (local deterministic companion module)
import pricing_budget  # noqa: E402  (local deterministic companion module)
import public_engine_evidence  # noqa: E402  (privacy-safe public engine evidence)
import relevance_dataset  # noqa: E402  (local deterministic companion module)


SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
COMMIT_RE = re.compile(r"^[0-9a-f]{40}$")
FROZEN_POWER_TARGET = 0.80
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
RELEVANCE_ARTIFACTS = {
    "labels": ("offline-relevance-development-labels.json", "schemas/relevance-label-source.schema.json"),
    "snapshot": ("offline-relevance-fact-snapshot.json", "schemas/relevance-fact-snapshot.schema.json"),
    "dataset": ("offline-relevance-dataset.json", "schemas/relevance-dataset.schema.json"),
    "review_ledger": ("offline-relevance-review-ledger.json", "schemas/relevance-review-ledger.schema.json"),
}
RELEVANCE_SOURCE_CONTRACT_FILE = "relevance-source-contract.json"
RELEVANCE_SOURCE_MEMBERSHIP_FILE = "offline-relevance-source-membership.json"
RELEVANCE_SOURCE_MEMBERSHIP_REPO_PATH = (
    "benchmarks/agent-brain/confirmatory/offline-relevance-source-membership.json"
)
RELEVANCE_REVIEW_LEDGER_REPO_PATH = (
    "benchmarks/agent-brain/confirmatory/offline-relevance-review-ledger.json"
)
RELEVANCE_NULL_REVIEW_LEDGER_FILE = "offline-relevance-null-review-ledger.json"
RELEVANCE_NULL_REVIEW_CONTRACT_FILE = "relevance-null-review-contract.json"
RELEVANCE_NULL_REVIEW_LEDGER_REPO_PATH = (
    "benchmarks/agent-brain/confirmatory/offline-relevance-null-review-ledger.json"
)
# Reviewed independently of preregistration.json's routinely regenerated artifact hashes.
RELEVANCE_SOURCE_CONTRACT_SHA256 = "5708b8f6f0ade1cedf4e1f7d0b4ff499d707e2c9cd93034d9e38a0aebb9836e1"
RELEVANCE_NULL_REVIEW_CONTRACT_SHA256 = "e606192db0f30cb091338db578cb88c7accb61e391a3ec21f06d83f6c40ecf0b"
ENGINE_PINS_REPO_PATH = "benchmarks/agent-brain/confirmatory/engine-verification-pins.json"
ENGINE_PINS = REPO / ENGINE_PINS_REPO_PATH
ENGINE_EVIDENCE_STORAGE_REPO_PATH = "benchmarks/agent-brain/confirmatory/engine-evidence-storage.json"
ENGINE_EVIDENCE_HYDRATION_PARENT = "benchmarks/agent-brain/confirmatory/.engine-evidence"
DEPENDENCY_INVENTORY_ALGORITHM = "sha256_ordered_relative_path_nul_sha256_newline_v1"
CANDIDATE_IDS_ALGORITHM = "sha256_canonical_sorted_id_array_v1"


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


def _evidence_path_contains_symlink(evidence: Any, here: pathlib.Path, repo: pathlib.Path) -> bool:
    if not isinstance(evidence, str) or not evidence:
        return False
    raw_path = evidence.split("#", 1)[0]
    relative = pathlib.Path(raw_path)
    if relative.is_absolute() or ".." in relative.parts:
        return False
    base = repo if relative.parts and relative.parts[0] == "benchmarks" else here
    current = base
    for part in relative.parts:
        current = current / part
        if current.is_symlink():
            return True
    return False


def _relative_path_contains_symlink(raw: Any, base: pathlib.Path) -> bool:
    if not isinstance(raw, str) or not raw:
        return False
    relative = pathlib.Path(raw)
    if relative.is_absolute() or ".." in relative.parts:
        return False
    current = base
    for part in relative.parts:
        current = current / part
        if current.is_symlink():
            return True
    return False


def _load_artifact(path: pathlib.Path, errors: list[str], label: str) -> Any | None:
    try:
        return load(path)
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        errors.append(f"{label} is not valid readable JSON: {exc}")
        return None


def _json_type_matches(value: Any, expected: str) -> bool:
    if expected == "null":
        return value is None
    if expected == "boolean":
        return isinstance(value, bool)
    if expected == "object":
        return isinstance(value, dict)
    if expected == "array":
        return isinstance(value, list)
    if expected == "string":
        return isinstance(value, str)
    if expected == "integer":
        return isinstance(value, int) and not isinstance(value, bool)
    if expected == "number":
        return isinstance(value, (int, float)) and not isinstance(value, bool)
    return False


def _json_equal(left: Any, right: Any) -> bool:
    if isinstance(left, bool) or isinstance(right, bool):
        return type(left) is type(right) and left == right
    if isinstance(left, (int, float)) and isinstance(right, (int, float)):
        return left == right
    return type(left) is type(right) and left == right


def _resolve_schema_pointer(root: dict[str, Any], fragment: str) -> dict[str, Any] | None:
    current: Any = root
    if fragment in {"", "/"}:
        return current if isinstance(current, dict) else None
    for encoded in fragment.removeprefix("/").split("/"):
        token = encoded.replace("~1", "/").replace("~0", "~")
        if not isinstance(current, dict) or token not in current:
            return None
        current = current[token]
    return current if isinstance(current, dict) else None


def _validate_schema_node(
    value: Any,
    schema: dict[str, Any],
    errors: list[str],
    label: str,
    *,
    root_schema: dict[str, Any],
    schema_dir: pathlib.Path,
) -> None:
    reference = schema.get("$ref")
    if isinstance(reference, str):
        if reference.startswith("#"):
            target_root = root_schema
            target = _resolve_schema_pointer(target_root, reference[1:])
            target_dir = schema_dir
        else:
            raw_path, separator, fragment = reference.partition("#")
            target_path = schema_dir / raw_path
            try:
                target_root = load(target_path)
            except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
                errors.append(f"{label}: cannot load referenced schema {reference}: {exc}")
                return
            target = _resolve_schema_pointer(target_root, fragment if separator else "")
            target_dir = target_path.parent
        if target is None:
            errors.append(f"{label}: unresolved schema reference {reference}")
            return
        _validate_schema_node(
            value,
            target,
            errors,
            label,
            root_schema=target_root,
            schema_dir=target_dir,
        )
        return

    expected_type = schema.get("type")
    if isinstance(expected_type, str):
        allowed_types = [expected_type]
    elif isinstance(expected_type, list) and all(isinstance(item, str) for item in expected_type):
        allowed_types = expected_type
    else:
        allowed_types = []
    if allowed_types and not any(_json_type_matches(value, item) for item in allowed_types):
        errors.append(f"{label}: schema type must be {' or '.join(allowed_types)}")
        return
    if "const" in schema and not _json_equal(value, schema["const"]):
        errors.append(f"{label}: value differs from schema const")
    enum = schema.get("enum")
    if isinstance(enum, list) and not any(_json_equal(value, item) for item in enum):
        errors.append(f"{label}: value is outside schema enum")

    if isinstance(value, dict):
        required = schema.get("required", [])
        if isinstance(required, list):
            for key in required:
                if isinstance(key, str) and key not in value:
                    errors.append(f"{label}: schema requires property {key}")
        properties = schema.get("properties", {})
        if not isinstance(properties, dict):
            properties = {}
        for key, child_schema in properties.items():
            if key in value and isinstance(child_schema, dict):
                _validate_schema_node(
                    value[key],
                    child_schema,
                    errors,
                    f"{label}.{key}",
                    root_schema=root_schema,
                    schema_dir=schema_dir,
                )
        extras = set(value) - set(properties)
        additional = schema.get("additionalProperties", True)
        if additional is False:
            for key in sorted(extras):
                errors.append(f"{label}: schema prohibits additional property {key}")
        elif isinstance(additional, dict):
            for key in sorted(extras):
                _validate_schema_node(
                    value[key],
                    additional,
                    errors,
                    f"{label}.{key}",
                    root_schema=root_schema,
                    schema_dir=schema_dir,
                )
    elif isinstance(value, list):
        minimum_items = schema.get("minItems")
        maximum_items = schema.get("maxItems")
        if isinstance(minimum_items, int) and len(value) < minimum_items:
            errors.append(f"{label}: array has fewer than {minimum_items} items")
        if isinstance(maximum_items, int) and len(value) > maximum_items:
            errors.append(f"{label}: array has more than {maximum_items} items")
        items = schema.get("items")
        if isinstance(items, dict):
            for index, item in enumerate(value):
                _validate_schema_node(
                    item,
                    items,
                    errors,
                    f"{label}[{index}]",
                    root_schema=root_schema,
                    schema_dir=schema_dir,
                )
    elif isinstance(value, str):
        minimum_length = schema.get("minLength")
        if isinstance(minimum_length, int) and len(value) < minimum_length:
            errors.append(f"{label}: string is shorter than {minimum_length}")
        pattern = schema.get("pattern")
        if isinstance(pattern, str) and re.search(pattern, value) is None:
            errors.append(f"{label}: string does not match schema pattern")
    elif isinstance(value, (int, float)) and not isinstance(value, bool):
        minimum = schema.get("minimum")
        if isinstance(minimum, (int, float)) and value < minimum:
            errors.append(f"{label}: number is below schema minimum {minimum}")


def _validate_engine_manifest_schema(path: pathlib.Path, errors: list[str]) -> None:
    artifact = _load_artifact(path, errors, "engine verification schema input")
    if artifact is None:
        return
    schema_path = HERE / "schemas" / "engine-verification-manifest.schema.json"
    try:
        schema = load(schema_path)
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        errors.append(f"engine verification manifest schema is unreadable: {exc}")
        return
    _validate_schema_node(
        artifact,
        schema,
        errors,
        "engine manifest",
        root_schema=schema,
        schema_dir=schema_path.parent,
    )


def _validate_public_engine_manifest_schema(path: pathlib.Path, errors: list[str]) -> None:
    artifact = _load_artifact(path, errors, "public engine verification schema input")
    if artifact is None:
        return
    schema_path = HERE / "schemas" / "engine-verification-public-v4.schema.json"
    try:
        schema = load(schema_path)
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        errors.append(f"public engine verification manifest schema is unreadable: {exc}")
        return
    _validate_schema_node(
        artifact,
        schema,
        errors,
        "public engine manifest",
        root_schema=schema,
        schema_dir=schema_path.parent,
    )


def _validate_engine_storage_schema(path: pathlib.Path, errors: list[str]) -> dict[str, Any] | None:
    artifact = _load_artifact(path, errors, "engine evidence storage contract")
    if artifact is None:
        return None
    schema_path = HERE / "schemas" / "engine-evidence-storage.schema.json"
    try:
        schema = load(schema_path)
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        errors.append(f"engine evidence storage schema is unreadable: {exc}")
        return None
    _validate_schema_node(
        artifact,
        schema,
        errors,
        "engine evidence storage contract",
        root_schema=schema,
        schema_dir=schema_path.parent,
    )
    if not isinstance(artifact, dict):
        errors.append("engine evidence storage contract must be an object")
        return None
    return artifact


def _canonical_release_asset_url(repository: Any, tag: Any, asset_name: Any) -> str | None:
    if not all(isinstance(value, str) and value and "/" not in value for value in (tag, asset_name)):
        return None
    if not isinstance(repository, str) or repository.count("/") != 1:
        return None
    return f"https://github.com/{repository}/releases/download/{tag}/{asset_name}"


def _validate_hydrated_tree(
    root: pathlib.Path,
    archive: dict[str, Any],
    errors: list[str],
) -> None:
    _error(errors, root.exists(), "engine evidence hydrated archive root is missing")
    _error(errors, root.is_dir() and not root.is_symlink(), "engine evidence hydrated archive root must be a real directory")
    if not root.is_dir() or root.is_symlink():
        return
    regular_count = 0
    logical_bytes = 0
    symlink_count = 0
    special_count = 0
    try:
        for current, dirnames, filenames in os.walk(root, followlinks=False):
            current_path = pathlib.Path(current)
            for name in [*dirnames, *filenames]:
                entry = current_path / name
                mode = entry.lstat().st_mode
                if stat.S_ISLNK(mode):
                    symlink_count += 1
                elif stat.S_ISREG(mode):
                    regular_count += 1
                    logical_bytes += entry.stat().st_size
                elif not stat.S_ISDIR(mode):
                    special_count += 1
    except OSError as exc:
        errors.append(f"engine evidence hydrated archive tree is unreadable: {exc}")
        return
    _error(errors, special_count == 0, "engine evidence hydrated archive contains a special filesystem entry")
    _error(errors, symlink_count == archive.get("symlink_count") == 0, "engine evidence hydrated archive symlink count differs")
    _error(errors, regular_count == archive.get("regular_file_count"), "engine evidence hydrated archive regular-file count differs")
    _error(errors, logical_bytes == archive.get("logical_bytes"), "engine evidence hydrated archive logical byte count differs")


def _validate_engine_storage_contract(
    path: pathlib.Path,
    errors: list[str],
    *,
    repo: pathlib.Path,
    require_published: bool,
) -> dict[str, Any] | None:
    contract = _validate_engine_storage_schema(path, errors)
    if contract is None:
        return None
    storage = contract.get("storage") if isinstance(contract.get("storage"), dict) else {}
    archive = contract.get("archive") if isinstance(contract.get("archive"), dict) else {}
    evidence = contract.get("evidence") if isinstance(contract.get("evidence"), dict) else {}
    external = contract.get("recorded_external_inputs") if isinstance(contract.get("recorded_external_inputs"), dict) else {}
    hydration = contract.get("hydration") if isinstance(contract.get("hydration"), dict) else {}

    expected_url = _canonical_release_asset_url(storage.get("repository"), storage.get("tag"), storage.get("asset_name"))
    _error(errors, storage.get("asset_url") == expected_url, "engine evidence release asset URL is not canonical")
    _error(errors, storage.get("kind") == "github_immutable_release_asset", "engine evidence storage kind is invalid")
    _error(errors, archive.get("format") == "tar_zstd", "engine evidence archive format is invalid")
    _error(errors, archive.get("symlink_count") == 0, "engine evidence archive must prohibit symlinks")
    _error(errors, _is_sha256(storage.get("asset_sha256")), "engine evidence archive SHA-256 is invalid")
    _error(errors, _is_nonnegative_int(storage.get("asset_size_bytes")) and storage.get("asset_size_bytes", 0) > 0, "engine evidence archive size is invalid")
    _error(errors, _is_sha256(evidence.get("manifest_sha256")), "engine evidence manifest SHA-256 is invalid")

    if require_published:
        _error(errors, storage.get("publication_disposition") == "approved", "engine evidence publication disposition is not approved")
        _error(errors, storage.get("privacy_review") == "publishable", "engine evidence privacy review did not pass")
        _error(errors, storage.get("published") is True, "engine evidence release asset is not published")
        _error(errors, _is_nonnegative_int(storage.get("release_id")) and storage.get("release_id", 0) > 0, "engine evidence release ID is missing")
        _error(errors, _is_nonnegative_int(storage.get("asset_id")) and storage.get("asset_id", 0) > 0, "engine evidence asset ID is missing")
        _error(errors, storage.get("release_immutable") is True, "engine evidence release is not immutable")
        _error(errors, _is_commit(storage.get("release_target_commitish")), "engine evidence release target commit is not pinned")
        _error(errors, storage.get("asset_api_digest") == f"sha256:{storage.get('asset_sha256')}", "engine evidence API digest differs from the archive hash")
        _error(errors, _parse_rfc3339(storage.get("verified_at")) is not None, "engine evidence release verification timestamp is invalid")

    hydration_relative = hydration.get("repo_relative_parent")
    _error(errors, hydration_relative == ENGINE_EVIDENCE_HYDRATION_PARENT, "engine evidence hydration parent was redirected")
    hydration_parent = _safe_relative_path(hydration_relative, repo)
    _error(errors, hydration_parent is not None, "engine evidence hydration parent is unsafe")
    _error(errors, not _relative_path_contains_symlink(hydration_relative, repo), "engine evidence hydration parent must not traverse a symlink")
    if hydration_parent is None:
        return None

    archive_root_name = archive.get("root")
    archive_root = _safe_relative_path(archive_root_name, hydration_parent)
    _error(errors, archive_root is not None, "engine evidence archive root is unsafe")
    if archive_root is None:
        return None
    _error(errors, not _relative_path_contains_symlink(archive_root_name, hydration_parent), "engine evidence archive root must not traverse a symlink")
    _validate_hydrated_tree(archive_root, archive, errors)

    manifest_relative = evidence.get("manifest_path")
    manifest_path = _safe_relative_path(manifest_relative, hydration_parent)
    _error(errors, manifest_path is not None, "engine evidence manifest path is unsafe")
    if manifest_path is None:
        return None
    try:
        manifest_path.relative_to(archive_root)
    except ValueError:
        errors.append("engine evidence manifest path is outside the archive root")
    _error(errors, not _relative_path_contains_symlink(manifest_relative, hydration_parent), "engine evidence manifest must not traverse a symlink")
    _error(errors, manifest_path.is_file() and not manifest_path.is_symlink(), "engine evidence hydrated manifest is missing")
    if manifest_path.is_file() and _is_sha256(evidence.get("manifest_sha256")):
        _error(errors, digest(manifest_path) == evidence["manifest_sha256"], "engine evidence hydrated manifest hash differs")

    recorded_root = pathlib.Path(evidence.get("recorded_artifact_root", ""))
    _error(errors, recorded_root.is_absolute() and ".." not in recorded_root.parts, "engine evidence recorded artifact root is invalid")
    external_root = pathlib.Path(external.get("repo_root", ""))
    _error(errors, external_root.is_absolute() and ".." not in external_root.parts, "engine evidence recorded external repo root is invalid")
    repo_key = external.get("repo_key")
    _error(errors, isinstance(repo_key, str) and _safe_relative_path(repo_key, pathlib.Path("/")) is not None, "engine evidence recorded repo key is invalid")
    return {
        "contract": contract,
        "artifact_repo": hydration_parent,
        "archive_root": archive_root,
        "manifest_path": manifest_path,
        "manifest_root": manifest_path.parent,
        "recorded_artifact_root": recorded_root,
        "recorded_external_repo_root": external.get("repo_root"),
        "recorded_external_repo_key": repo_key,
    }


def _map_recorded_artifact_path(
    raw: Any,
    context: dict[str, Any],
    errors: list[str],
    label: str,
    *,
    expected: pathlib.Path | None = None,
    kind: str = "file",
) -> pathlib.Path | None:
    if not isinstance(raw, str) or not raw:
        errors.append(f"{label}: recorded absolute path is missing")
        return None
    source = pathlib.Path(raw)
    recorded_root = context["recorded_artifact_root"]
    if not source.is_absolute() or ".." in source.parts:
        errors.append(f"{label}: recorded path is not a normalized absolute path")
        return None
    source = source.resolve(strict=False)
    recorded_root = recorded_root.resolve(strict=False)
    try:
        relative = source.relative_to(recorded_root)
    except ValueError:
        errors.append(f"{label}: recorded absolute path is outside recorded_artifact_root")
        return None
    target = _safe_relative_path(relative.as_posix(), context["artifact_repo"])
    if target is None:
        errors.append(f"{label}: recorded path cannot be mapped into the hydrated artifact root")
        return None
    try:
        target.relative_to(context["archive_root"])
    except ValueError:
        errors.append(f"{label}: mapped path is outside the hydrated archive root")
    if expected is not None:
        _error(errors, target == expected.resolve(), f"{label}: recorded path maps to the wrong hydrated artifact")
    if kind == "file":
        _error(errors, target.is_file() and not target.is_symlink(), f"{label}: mapped hydrated file is missing")
    elif kind == "directory":
        _error(errors, target.is_dir() and not target.is_symlink(), f"{label}: mapped hydrated directory is missing")
    return target


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


def validate_engine_pin_binding(
    protocol: dict[str, Any],
    *,
    here: pathlib.Path = HERE,
    repo: pathlib.Path = REPO,
) -> list[str]:
    """Bind the preregistration to the canonical production engine-pin bytes."""
    errors: list[str] = []
    binding = protocol.get("engine_verification_pins")
    label = "preregistration engine_verification_pins"
    path = _verify_hashed_file(errors, binding, label, repo=repo)
    if not isinstance(binding, dict):
        return errors
    _error(
        errors,
        set(binding) == {"path", "sha256"},
        f"{label}: fields must be exactly path and sha256",
    )
    _error(
        errors,
        path == ENGINE_PINS_REPO_PATH,
        f"{label}: path must be {ENGINE_PINS_REPO_PATH}",
    )
    if path == ENGINE_PINS_REPO_PATH:
        _error(
            errors,
            not _evidence_path_contains_symlink(path, here, repo),
            f"{label}: canonical path must not contain symlinks",
        )
    return errors


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
    power = protocol.get("agent_design", {}).get("power", {})
    completed = power.get("completed") is True
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
    decision_errors = power_analysis.validate_power_report(artifact)
    errors.extend(f"power artifact: {error}" for error in decision_errors)
    try:
        derived_decision = power_analysis.recompute_power_decision(artifact)
    except (TypeError, ValueError) as exc:
        errors.append(f"cannot recompute power decision: {exc}")
        derived_decision = {"passed": False, "status": "invalid"}
    decision_passed = derived_decision["passed"] is True
    _error(errors, artifact.get("schema_version") == 3, "power artifact schema_version must be 3")
    agent_design = protocol.get("agent_design", {})
    inputs = artifact.get("protocol_inputs")
    if not isinstance(inputs, dict):
        errors.append("power artifact protocol_inputs must be an object")
        inputs = {}
    treatments = agent_design.get("primary_treatments")
    tasks = agent_design.get("tasks")
    repetitions = agent_design.get("repetitions_per_treatment")
    expected_treatment_count = len(treatments) if isinstance(treatments, list) else None
    expected_requested = (
        tasks * expected_treatment_count * repetitions
        if isinstance(tasks, int)
        and not isinstance(tasks, bool)
        and isinstance(expected_treatment_count, int)
        and isinstance(repetitions, int)
        and not isinstance(repetitions, bool)
        else None
    )
    _error(errors, inputs.get("tasks") == tasks, "power artifact task count does not match preregistration")
    _error(
        errors,
        inputs.get("power_target") == FROZEN_POWER_TARGET
        and power.get("target") == FROZEN_POWER_TARGET,
        "power target must remain frozen at 0.80 in artifact and preregistration",
    )
    _error(
        errors,
        inputs.get("repetitions_per_treatment") == repetitions,
        "power artifact repetition count does not match preregistration",
    )
    _error(
        errors,
        inputs.get("primary_treatments") == expected_treatment_count,
        "power artifact treatment count does not match preregistration",
    )
    _error(
        errors,
        not isinstance(agent_design.get("requested_cells"), bool)
        and agent_design.get("requested_cells") == expected_requested
        and not isinstance(inputs.get("requested_cells"), bool)
        and inputs.get("requested_cells") == expected_requested,
        "power artifact requested-cell arithmetic does not match preregistration",
    )
    _error(
        errors,
        not isinstance(inputs.get("agent_retry_limit"), bool)
        and inputs.get("agent_retry_limit") == 0
        and not isinstance(inputs.get("replacement_cell_limit"), bool)
        and inputs.get("replacement_cell_limit") == 0
        and not isinstance(inputs.get("maximum_agent_invocations"), bool)
        and inputs.get("maximum_agent_invocations") == expected_requested
        and not isinstance(agent_design.get("maximum_agent_invocations"), bool)
        and agent_design.get("maximum_agent_invocations") == expected_requested,
        "confirmatory agent-invocation ceiling must equal requested cells with zero retries and replacements",
    )
    _error(
        errors,
        artifact.get("status") == derived_decision["status"],
        "power artifact status does not match recomputed decision",
    )
    _error(
        errors,
        completed is decision_passed,
        "protocol power.completed must remain false until all three endpoints are calibrated and powered",
    )
    protocol_evidence = _evidence_path(power.get("evidence"), here, repo)
    _error(errors, protocol_evidence == artifact_path.resolve(), "protocol power evidence must reference power-analysis.json")
    _error(errors, power.get("analysis_kind") == artifact.get("analysis_kind"), "protocol power analysis_kind does not match artifact")
    artifact_floors = artifact.get("co_primary_endpoints", {})
    floor_statuses = {
        endpoint.get("claim_floor", {}).get("status")
        for endpoint in artifact_floors.values()
        if isinstance(endpoint, dict)
    }
    _error(
        errors,
        floor_statuses in ({"provisional"}, {"frozen_approved"}),
        "all co-primary claim-floor statuses must move together",
    )
    common_floor_status = next(iter(floor_statuses)) if len(floor_statuses) == 1 else None
    expected_floors = {
        "elapsed_time_ratio_max": artifact_floors.get("elapsed_time", {}).get("claim_floor", {}).get("ratio_max"),
        "normalized_cost_ratio_max": artifact_floors.get("normalized_cost", {}).get("claim_floor", {}).get("ratio_max"),
        "code_quality_difference_min": artifact_floors.get("code_quality", {}).get("claim_floor", {}).get("difference_min"),
        "status": common_floor_status,
    }
    _error(
        errors,
        power.get("co_primary_claim_floors") == expected_floors,
        "protocol co-primary claim floors do not match power artifact",
    )
    expected_alternatives = {
        "elapsed_time_ratio_true": artifact_floors.get("elapsed_time", {}).get("planning_alternative", {}).get("ratio_true"),
        "normalized_cost_ratio_true": artifact_floors.get("normalized_cost", {}).get("planning_alternative", {}).get("ratio_true"),
        "code_quality_difference_true": artifact_floors.get("code_quality", {}).get("planning_alternative", {}).get("difference_true"),
        "status": artifact_floors.get("elapsed_time", {}).get("planning_alternative", {}).get("status"),
    }
    _error(
        errors,
        power.get("planning_alternatives") == expected_alternatives,
        "protocol planning alternatives do not match power artifact",
    )
    calibration_requirements = artifact.get("calibration_requirements", {})
    readiness = artifact.get("design_readiness", {})
    _error(errors, power.get("target") == inputs.get("power_target"), "protocol power target does not match power artifact")
    _error(
        errors,
        not isinstance(readiness.get("provisional_tasks"), bool)
        and readiness.get("provisional_tasks") == tasks
        and not isinstance(
            readiness.get("provisional_repetitions_per_treatment"), bool
        )
        and readiness.get("provisional_repetitions_per_treatment") == repetitions
        and not isinstance(readiness.get("provisional_requested_cells"), bool)
        and readiness.get("provisional_requested_cells") == expected_requested
        and not isinstance(readiness.get("maximum_agent_invocations"), bool)
        and readiness.get("maximum_agent_invocations") == expected_requested
        and not isinstance(readiness.get("agent_retry_limit"), bool)
        and readiness.get("agent_retry_limit") == 0
        and not isinstance(readiness.get("replacement_cell_limit"), bool)
        and readiness.get("replacement_cell_limit") == 0,
        "power readiness count/cell arithmetic does not match the protocol design",
    )
    _error(
        errors,
        power.get("target_scope")
        == "each_marginal_and_overall_intersection_union_joint_success",
        "protocol power target scope must cover every marginal and overall joint success",
    )
    _error(
        errors,
        power.get("minimum_calibration_task_clusters")
        == calibration_requirements.get("minimum_independent_task_clusters")
        and power.get("minimum_calibration_is_power_sized_design") is False
        and calibration_requirements.get(
            "minimum_is_calibration_floor_not_power_sized_design"
        )
        is True,
        "protocol calibration floor must not be represented as a powered design",
    )
    _error(
        errors,
        power.get("power_sized_development_task_count")
        == readiness.get("power_sized_development_task_count")
        and power.get("power_sized_confirmatory_task_count")
        == readiness.get("power_sized_confirmatory_task_count"),
        "protocol power-sized task counts do not match power readiness artifact",
    )
    sized_development = readiness.get("power_sized_development_task_count")
    sized_confirmatory = readiness.get("power_sized_confirmatory_task_count")
    minimum_calibration = calibration_requirements.get("minimum_independent_task_clusters")
    if sized_development is not None or sized_confirmatory is not None:
        _error(
            errors,
            isinstance(sized_development, int)
            and not isinstance(sized_development, bool)
            and isinstance(minimum_calibration, int)
            and not isinstance(minimum_calibration, bool)
            and sized_development >= minimum_calibration,
            "power-sized development task count is below the calibration minimum",
        )
        _error(
            errors,
            isinstance(sized_confirmatory, int)
            and not isinstance(sized_confirmatory, bool)
            and sized_confirmatory == tasks,
            "power-sized confirmatory task count must equal the protocol task count",
        )
    artifact_calibration = artifact.get("exploratory_calibration", {})
    expected_calibration = {
        "manifest": pathlib.Path(str(artifact_calibration.get("manifest_path") or "")).name,
        "eligibility": "exploratory_only",
        "confirmatory_assumption_source": False,
        "unique_task_ids": artifact_calibration.get("unique_task_ids_across_sources"),
        "paired_task_cluster_instances": artifact_calibration.get("paired_task_cluster_instances"),
        "pooled_estimate_prohibited": artifact_calibration.get("pooled_estimate_prohibited"),
    }
    _error(
        errors,
        power.get("exploratory_calibration") == expected_calibration,
        "protocol exploratory calibration summary does not match power artifact",
    )
    protocol_status = power.get("status")
    _error(
        errors,
        protocol_status == derived_decision["status"],
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


def validate_joint_success_contract(protocol: dict[str, Any], *, freeze: bool) -> list[str]:
    """Validate the self-hashed v2 intersection-union success contract."""
    errors: list[str] = []
    contract = protocol.get("agent_design", {}).get("success_contract")
    if not isinstance(contract, dict):
        return ["joint superiority success contract is missing"]
    _error(
        errors,
        contract.get("schema") == "agent-brain-joint-superiority-contract/v2",
        "joint superiority success contract schema changed",
    )
    _error(
        errors,
        contract.get("primary_contrast") == "retrieved_memory_vs_no_memory",
        "joint superiority primary contrast changed",
    )
    _error(errors, contract.get("intersection_union_alpha") == 0.05, "joint superiority alpha changed")
    endpoints = contract.get("endpoints")
    expected_names = {"elapsed_time", "normalized_cost", "code_quality"}
    if not isinstance(endpoints, dict):
        errors.append("joint superiority endpoints must be an object")
        endpoints = {}
    _error(errors, set(endpoints) == expected_names, "joint superiority endpoint set changed")
    expected_shapes = {
        "elapsed_time": ("lower", "paired_task_geometric_mean_ratio", "practical_ratio_max", 0.9),
        "normalized_cost": (
            "lower",
            "ratio_of_equal_task_weighted_task_arm_mean_costs",
            "practical_ratio_max",
            0.88,
        ),
        "code_quality": ("higher", "paired_task_mean_difference", "practical_difference_min", 0.05),
    }
    for name, (direction, estimand, floor_key, provisional_floor) in expected_shapes.items():
        endpoint = endpoints.get(name)
        if not isinstance(endpoint, dict):
            errors.append(f"joint superiority {name} endpoint is missing")
            continue
        _error(errors, endpoint.get("direction") == direction, f"joint superiority {name} direction changed")
        _error(errors, endpoint.get("estimand") == estimand, f"joint superiority {name} estimand changed")
        floor = endpoint.get(floor_key)
        _error(
            errors,
            isinstance(floor, (int, float)) and not isinstance(floor, bool) and floor == provisional_floor,
            f"joint superiority {name} practical floor changed without methodology update",
        )
        _error(
            errors,
            endpoint.get("floor_status") in {"provisional", "frozen_approved"},
            f"joint superiority {name} floor status is invalid",
        )
    _error(
        errors,
        contract.get("code_quality_measurement")
        == {
            "schema": "agent-brain-code-quality/v2",
            "rubric": "task_relative_output_outcome_patch_focus_v2",
            "included_components": ["outcome", "patch_focus"],
            "normalization_denominator_points": 75,
            "excluded_components": [
                "validation_discipline",
                "runtime_efficiency",
                "brain_use",
            ],
            "critical_failure_forces_zero": True,
        },
        "joint superiority code-quality measurement changed",
    )
    timeout = contract.get("timeout_policy")
    if not isinstance(timeout, dict):
        errors.append("joint superiority timeout policy is missing")
        timeout = {}
    _error(
        errors,
        timeout.get("policy")
        == "retain_measured_end_to_end_elapsed_no_component_cap_substitution",
        "joint superiority timeout policy changed",
    )
    _error(
        errors,
        timeout.get("elapsed_field") == "timing.end_to_end_user_visible_wall_seconds"
        and timeout.get("substitute_component_timeout_limit") is False
        and timeout.get("provider_retry_limit") == 0,
        "joint superiority timeout/retry observation contract changed",
    )
    limit = timeout.get("agent_timeout_limit_seconds")
    _error(
        errors,
        isinstance(limit, (int, float)) and not isinstance(limit, bool) and math.isfinite(float(limit)) and limit > 0,
        "joint superiority agent timeout limit must be positive",
    )
    _error(
        errors,
        timeout.get("status") in {"provisional", "frozen_approved"},
        "joint superiority timeout status is invalid",
    )
    canonical = copy.deepcopy(contract)
    recorded_hash = canonical.pop("contract_sha256", None)
    _error(errors, _is_sha256(recorded_hash), "joint superiority contract hash is invalid")
    _error(
        errors,
        recorded_hash == canonical_json_sha256(canonical),
        "joint superiority contract self-hash mismatch",
    )
    if freeze:
        _error(errors, contract.get("status") == "frozen_approved", "joint superiority contract is not frozen and approved")
        _error(
            errors,
            all(item.get("floor_status") == "frozen_approved" for item in endpoints.values() if isinstance(item, dict)),
            "all three practical floors are not frozen and approved",
        )
        _error(errors, timeout.get("status") == "frozen_approved", "timeout policy is not frozen and approved")
    else:
        _error(
            errors,
            contract.get("status") in {"provisional", "frozen_approved"},
            "joint superiority contract status is invalid",
        )
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
    _error(errors, artifact.get("schema_version") == 3, "pricing budget schema_version must be 3")

    runner = artifact.get("runner")
    if not isinstance(runner, dict):
        errors.append("pricing runner must be an object")
        runner = {}
    runner_fields = (
        "schema",
        "provider",
        "runner_id",
        "runner_version",
        "agent_id",
        "agent_cli",
        "agent_cli_version",
        "requested_model_id",
        "resolved_model_id",
        "effort",
        "schedule_sha256",
        "identity_sha256",
    )
    _require_exact_fields(errors, runner, runner_fields, "pricing runner")
    _error(
        errors,
        runner.get("schema") == "agent-brain-frozen-runner-identity/v1",
        "pricing runner identity schema changed",
    )
    for field in runner_fields[1:]:
        value = runner.get(field)
        _error(errors, value is None or (isinstance(value, str) and bool(value)), f"pricing runner.{field} must be null or a non-empty string")

    quote = artifact.get("pricing_quote")
    if not isinstance(quote, dict):
        errors.append("pricing_quote must be an object")
        quote = {}
    quote_fields = (
        "schema",
        "status",
        "quote_sha256",
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
        "price_aliases",
        "usage_semantics",
    )
    _require_exact_fields(errors, quote, quote_fields, "pricing_quote")
    _error(errors, quote.get("status") in {"pending", "pinned"}, "pricing quote status is invalid")
    _error(errors, quote.get("schema") == "agent-brain-price-quote/v2", "pricing quote schema changed")
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
    aliases = quote.get("price_aliases")
    if not isinstance(aliases, dict):
        errors.append("pricing quote price_aliases must be an object")
        aliases = {}
    _error(errors, set(aliases) == set(pricing_budget.TOKEN_KEYS), "pricing quote alias categories changed")
    semantics = quote.get("usage_semantics")
    if not isinstance(semantics, dict):
        errors.append("pricing quote usage_semantics must be an object")
        semantics = {}
    _error(
        errors,
        set(semantics)
        == {
            "input_tokens_includes",
            "output_tokens_includes",
            "counter_absence_means_zero",
        },
        "pricing quote usage semantics fields changed",
    )
    input_includes = semantics.get("input_tokens_includes")
    output_includes = semantics.get("output_tokens_includes")
    absence = semantics.get("counter_absence_means_zero")
    _error(
        errors,
        isinstance(input_includes, list)
        and _unique_strings(input_includes)
        and set(input_includes).issubset({"cache_read_input", "cache_write_input"}),
        "pricing quote input inclusion semantics are invalid",
    )
    _error(
        errors,
        isinstance(output_includes, list)
        and _unique_strings(output_includes)
        and set(output_includes).issubset({"reasoning_output"}),
        "pricing quote output inclusion semantics are invalid",
    )
    _error(
        errors,
        isinstance(absence, dict)
        and set(absence)
        == {"cache_read_input", "cache_write_input", "reasoning_output"}
        and all(isinstance(value, bool) for value in absence.values()),
        "pricing quote counter-absence semantics are invalid",
    )

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
        "requested_cells",
        "retry_agent_invocations",
        "replacement_cell_attempts",
        "reserve_cell_attempts",
        "maximum_agent_invocations",
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
    expected_maximum = agent_design.get("maximum_agent_invocations")
    expected_design = {
        "protocol_id": protocol.get("protocol_id"),
        "power_status_at_binding": power.get("status"),
        "tasks": tasks,
        "treatments": treatments,
        "repetitions_per_treatment": repetitions,
        "requested_cells": agent_design.get("requested_cells"),
        "retry_agent_invocations": 0,
        "replacement_cell_attempts": 0,
        "reserve_cell_attempts": 0,
        "maximum_agent_invocations": expected_maximum,
    }
    for field, expected in expected_design.items():
        _error(errors, design.get(field) == expected, f"pricing design does not match preregistration: {field}")
    _error(errors, design.get("requested_cells") == expected_requested, "pricing design requested_cells arithmetic is inconsistent")
    _error(
        errors,
        design.get("retry_agent_invocations") == 0
        and design.get("replacement_cell_attempts") == 0
        and design.get("reserve_cell_attempts") == 0
        and expected_maximum == expected_requested,
        "confirmatory retries, replacements, and reserves must remain zero and maximum agent invocations must equal requested cells",
    )
    _error(errors, isinstance(design.get("approved_for_budgeting"), bool), "pricing design approved_for_budgeting must be boolean")

    price_check = checks.get("model_runner_price_pinned", {})
    budget_check = checks.get("paid_budget_cap_approved", {})
    quote_pinned = quote.get("status") == "pinned"
    quote_times: dict[str, datetime | None] = {"as_of": None, "retrieved_at": None, "expires_at": None}
    if quote_pinned:
        _require_nonempty_strings(errors, runner, runner_fields, "pricing runner")
        _error(errors, _is_sha256(runner.get("schedule_sha256")), "pricing runner schedule_sha256 is invalid")
        _error(errors, _is_sha256(runner.get("identity_sha256")), "pricing runner identity_sha256 is invalid")
        runner_without_hash = dict(runner)
        runner_without_hash.pop("identity_sha256", None)
        _error(
            errors,
            runner.get("identity_sha256") == canonical_json_sha256(runner_without_hash),
            "pricing runner identity self-hash mismatch",
        )
        _error(
            errors,
            runner.get("agent_id") == runner.get("agent_cli"),
            "pricing runner agent_id and agent_cli must identify the same frozen adapter",
        )
        _require_nonempty_strings(errors, quote, ("source_uri",), "pricing_quote")
        source_record = {
            "path": quote.get("source_artifact_path"),
            "sha256": quote.get("source_artifact_sha256"),
        }
        _verify_hashed_file(errors, source_record, "pricing quote source artifact", repo=repo)
        for key in pricing_budget.TOKEN_KEYS:
            direct = prices.get(key)
            alias = aliases.get(key)
            _error(
                errors,
                (direct is None) != (alias is None),
                f"pricing quote {key} must set exactly one direct price or alias",
            )
            if direct is not None:
                try:
                    pricing_budget.parse_decimal(direct, f"prices_usd_per_unit.{key}")
                except ValueError as exc:
                    errors.append(str(exc))
            if alias is not None:
                _error(
                    errors,
                    alias in pricing_budget.TOKEN_KEYS
                    and alias != key
                    and prices.get(alias) is not None,
                    f"pricing quote {key} alias must target a direct category price",
                )
        canonical_quote = copy.deepcopy(quote)
        recorded_quote_hash = canonical_quote.pop("quote_sha256", None)
        _error(errors, _is_sha256(recorded_quote_hash), "pricing quote self-hash is invalid")
        _error(
            errors,
            recorded_quote_hash == canonical_json_sha256(canonical_quote),
            "pricing quote self-hash mismatch",
        )
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
    else:
        _error(errors, quote.get("quote_sha256") is None, "pending pricing quote hash must remain null")

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
        ("mode", "explicit_per_agent_invocation_caps", "empirical_bound"),
        "token_assumptions",
    )
    caps = assumptions.get("explicit_per_agent_invocation_caps")
    if not isinstance(caps, dict):
        errors.append("explicit_per_agent_invocation_caps must be an object")
        caps = {}
    _require_exact_fields(
        errors,
        caps,
        (*pricing_budget.TOKEN_KEYS, "rationale"),
        "explicit_per_agent_invocation_caps",
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
            "observed_tokens_per_agent_invocation",
        ),
        "empirical_bound",
    )
    observed = empirical.get("observed_tokens_per_agent_invocation")
    if not isinstance(observed, dict):
        errors.append(
            "empirical observed_tokens_per_agent_invocation must be an object"
        )
        observed = {}
    _require_exact_fields(
        errors,
        observed,
        pricing_budget.TOKEN_KEYS,
        "empirical observed_tokens_per_agent_invocation",
    )
    empirical_runner = empirical.get("runner")
    if empirical_runner is not None:
        _require_exact_fields(errors, empirical_runner, runner_fields, "empirical runner")
    mode = assumptions.get("mode")
    _error(
        errors,
        mode in {None, "explicit_per_agent_invocation_caps", "empirical_bound"},
        "token assumption mode is invalid",
    )
    if mode == "explicit_per_agent_invocation_caps":
        _error(
            errors,
            isinstance(caps.get("rationale"), str) and bool(caps["rationale"]),
            "explicit per-agent-invocation caps require a rationale",
        )
        try:
            pricing_budget.effective_tokens_per_agent_invocation(artifact)
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
            pricing_budget.effective_tokens_per_agent_invocation(artifact)
        except ValueError as exc:
            errors.append(str(exc))

    expected_calculation: dict[str, Any] | None = None
    if quote_pinned and mode in {
        "explicit_per_agent_invocation_caps",
        "empirical_bound",
    }:
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


def validate_relevance_bundle(
    protocol: dict[str, Any],
    *,
    here: pathlib.Path = HERE,
    repo: pathlib.Path = REPO,
) -> list[str]:
    """Validate committed relevance sources, schemas, generated output, and versioned hashes."""
    errors: list[str] = []
    reviewed_contract_path = here / RELEVANCE_SOURCE_CONTRACT_FILE
    membership_path = here / RELEVANCE_SOURCE_MEMBERSHIP_FILE
    null_review_contract_path = here / RELEVANCE_NULL_REVIEW_CONTRACT_FILE
    null_review_ledger_path = here / RELEVANCE_NULL_REVIEW_LEDGER_FILE
    reviewed_contract: dict[str, Any] | None = None
    membership: dict[str, Any] | None = None
    null_review_contract: dict[str, Any] | None = None
    null_review_ledger: dict[str, Any] | None = None
    if not reviewed_contract_path.is_file():
        errors.append("reviewed relevance source contract is missing")
    else:
        _error(
            errors,
            digest(reviewed_contract_path) == RELEVANCE_SOURCE_CONTRACT_SHA256,
            "reviewed relevance source contract digest differs from hardcoded trust root",
        )
        loaded_contract = _load_artifact(reviewed_contract_path, errors, "reviewed relevance source contract")
        if isinstance(loaded_contract, dict):
            reviewed_contract = loaded_contract
        else:
            errors.append("reviewed relevance source contract must be an object")
    if not membership_path.is_file():
        errors.append("complete relevance source membership catalog is missing")
    else:
        loaded_membership = _load_artifact(membership_path, errors, "complete relevance source membership catalog")
        if isinstance(loaded_membership, dict):
            membership = loaded_membership
        else:
            errors.append("complete relevance source membership catalog must be an object")
    if reviewed_contract is not None:
        _error(
            errors,
            reviewed_contract.get("membership_path") == RELEVANCE_SOURCE_MEMBERSHIP_REPO_PATH,
            "reviewed relevance source membership path changed",
        )
    if not null_review_contract_path.is_file():
        errors.append("reviewed relevance null-review contract is missing")
    else:
        _error(
            errors,
            digest(null_review_contract_path) == RELEVANCE_NULL_REVIEW_CONTRACT_SHA256,
            "reviewed relevance null-review contract digest differs from hardcoded trust root",
        )
        loaded = _load_artifact(null_review_contract_path, errors, "reviewed relevance null-review contract")
        if isinstance(loaded, dict):
            null_review_contract = loaded
    if not null_review_ledger_path.is_file():
        errors.append("relevance null-review ledger is missing")
    else:
        loaded = _load_artifact(null_review_ledger_path, errors, "relevance null-review ledger")
        if isinstance(loaded, dict):
            null_review_ledger = loaded
    for value, schema_name, label in (
        (reviewed_contract, "relevance-source-contract.schema.json", "source contract"),
        (membership, "relevance-source-membership.schema.json", "source membership"),
        (null_review_contract, "relevance-null-review-contract.schema.json", "null-review contract"),
        (null_review_ledger, "relevance-null-review-ledger.schema.json", "null-review ledger"),
    ):
        schema_path = here / "schemas" / schema_name
        if not schema_path.is_file():
            errors.append(f"relevance {label} schema is missing")
            continue
        schema = _load_artifact(schema_path, errors, f"relevance {label} schema")
        if isinstance(value, dict) and isinstance(schema, dict):
            errors.extend(relevance_dataset.validate_schema_instance(value, schema, label))
    if null_review_contract is not None and null_review_ledger is not None and reviewed_contract is not None:
        try:
            relevance_dataset.validate_null_review_contract(
                null_review_contract,
                null_review_ledger,
                reviewed_contract,
                ledger_path=RELEVANCE_NULL_REVIEW_LEDGER_REPO_PATH,
                ledger_sha256=digest(null_review_ledger_path),
            )
        except relevance_dataset.DatasetError as exc:
            errors.append(f"relevance null-review contract validation failed: {exc}")
    offline = protocol.get("offline_dataset")
    if not isinstance(offline, dict):
        return ["offline_dataset protocol section must be an object"]
    contract = offline.get("development_artifact_contract")
    if not isinstance(contract, dict):
        return ["offline relevance development artifact contract is missing"]
    _error(errors, contract.get("schema_version") == 1, "relevance artifact contract schema_version must be 1")
    _error(
        errors,
        set(contract) == {"schema_version", "source_corpus", "artifacts"},
        "relevance artifact contract fields changed",
    )
    source_contract = contract.get("source_corpus")
    expected_source_fields = {
        "facts_sha256",
        "session_dates_sha256",
        "active_fact_count",
        "active_fact_catalog_sha256",
    }
    if not isinstance(source_contract, dict):
        errors.append("relevance artifact source_corpus must be an object")
        source_contract = {}
    else:
        _error(errors, set(source_contract) == expected_source_fields, "relevance artifact source fields changed")
    _error(errors, _is_sha256(source_contract.get("facts_sha256")), "relevance source facts hash is invalid")
    _error(errors, _is_sha256(source_contract.get("session_dates_sha256")), "relevance source session-date hash is invalid")
    _error(errors, _is_nonnegative_int(source_contract.get("active_fact_count")), "relevance active fact count is invalid")
    _error(
        errors,
        _is_sha256(source_contract.get("active_fact_catalog_sha256")),
        "relevance active fact catalog hash is invalid",
    )

    specs = contract.get("artifacts")
    if not isinstance(specs, dict):
        errors.append("relevance artifact descriptors must be an object")
        specs = {}
    _error(errors, set(specs) == set(RELEVANCE_ARTIFACTS), "relevance artifact roles changed")
    loaded: dict[str, dict[str, Any]] = {}
    paths: dict[str, pathlib.Path] = {}
    for role, (expected_path, expected_schema_path) in RELEVANCE_ARTIFACTS.items():
        spec = specs.get(role)
        label = f"relevance {role} artifact"
        if not isinstance(spec, dict):
            errors.append(f"{label} descriptor is missing")
            continue
        _error(
            errors,
            set(spec) == {"path", "sha256", "schema_path", "schema_sha256"},
            f"{label} descriptor fields changed",
        )
        _error(errors, spec.get("path") == expected_path, f"{label} path changed")
        _error(errors, spec.get("schema_path") == expected_schema_path, f"{label} schema path changed")
        artifact_path = _safe_relative_path(spec.get("path"), here)
        schema_path = _safe_relative_path(spec.get("schema_path"), here)
        if artifact_path is None or not artifact_path.is_file():
            errors.append(f"{label} path is unsafe or missing")
        else:
            paths[role] = artifact_path
            _error(errors, _is_sha256(spec.get("sha256")), f"{label} hash is invalid")
            if _is_sha256(spec.get("sha256")):
                _error(errors, digest(artifact_path) == spec["sha256"], f"{label} content hash mismatch")
            artifact = _load_artifact(artifact_path, errors, label)
            if isinstance(artifact, dict):
                loaded[role] = artifact
            elif artifact is not None:
                errors.append(f"{label} must be an object")
        if schema_path is None or not schema_path.is_file():
            errors.append(f"{label} schema path is unsafe or missing")
        else:
            _error(errors, _is_sha256(spec.get("schema_sha256")), f"{label} schema hash is invalid")
            if _is_sha256(spec.get("schema_sha256")):
                _error(errors, digest(schema_path) == spec["schema_sha256"], f"{label} schema content hash mismatch")

    _error(
        errors,
        offline.get("path") == RELEVANCE_ARTIFACTS["dataset"][0],
        "offline relevance dataset path differs from artifact contract",
    )
    if set(loaded) == set(RELEVANCE_ARTIFACTS):
        labels = loaded["labels"]
        snapshot = loaded["snapshot"]
        dataset = loaded["dataset"]
        review_ledger = loaded["review_ledger"]
        try:
            errors.extend(
                relevance_dataset.validate_relevance_schemas(
                    labels, snapshot, dataset, here / "schemas", review_ledger
                )
            )
        except relevance_dataset.DatasetError as exc:
            errors.append(f"relevance schema validation failed: {exc}")
        try:
            if reviewed_contract is None or membership is None:
                raise relevance_dataset.DatasetError("reviewed source contract/membership is unavailable")
            relevance_dataset.validate_source_membership(
                labels,
                snapshot,
                reviewed_contract,
                membership,
                membership_sha256=digest(membership_path),
            )
        except relevance_dataset.DatasetError as exc:
            errors.append(f"relevance source membership validation failed: {exc}")
        try:
            if reviewed_contract is None:
                raise relevance_dataset.DatasetError("reviewed source contract is unavailable")
            relevance_dataset.validate_review_ledger(
                labels,
                review_ledger,
                reviewed_contract,
                ledger_path=RELEVANCE_REVIEW_LEDGER_REPO_PATH,
                ledger_sha256=digest(paths["review_ledger"]),
            )
        except relevance_dataset.DatasetError as exc:
            errors.append(f"relevance review ledger validation failed: {exc}")
        try:
            expected = relevance_dataset.materialize(
                repo.resolve(),
                paths["labels"],
                here / "task-inventory.json",
                paths["snapshot"],
                source_membership=membership,
                null_review_ledger=null_review_ledger,
                null_review_ledger_path=RELEVANCE_NULL_REVIEW_LEDGER_REPO_PATH,
                null_review_ledger_sha256=digest(null_review_ledger_path),
            )
            relevance_dataset.validate_dataset(dataset, expected)
        except relevance_dataset.DatasetError as exc:
            errors.append(f"relevance source validation failed: {exc}")

        label_source = labels.get("source_corpus") if isinstance(labels.get("source_corpus"), dict) else {}
        snapshot_source = {
            "facts_sha256": snapshot.get("source_facts_sha256"),
            "session_dates_sha256": snapshot.get("source_session_dates_sha256"),
            "active_fact_count": snapshot.get("source_active_fact_count"),
            "active_fact_catalog_sha256": snapshot.get("source_active_fact_catalog_sha256"),
        }
        development = dataset.get("development") if isinstance(dataset.get("development"), dict) else {}
        dataset_source = {
            "facts_sha256": development.get("source_facts_sha256"),
            "session_dates_sha256": development.get("source_session_dates_sha256"),
            "active_fact_count": development.get("source_active_fact_count"),
            "active_fact_catalog_sha256": development.get("source_active_fact_catalog_sha256"),
        }
        _error(errors, label_source == source_contract, "relevance labels differ from pinned full-source contract")
        _error(errors, snapshot_source == source_contract, "relevance snapshot differs from pinned full-source contract")
        _error(errors, dataset_source == source_contract, "relevance dataset differs from pinned full-source contract")
        _error(
            errors,
            development.get("labels_sha256") == digest(paths["labels"]),
            "relevance dataset labels hash is stale",
        )
        _error(
            errors,
            development.get("fact_snapshot_sha256") == digest(paths["snapshot"]),
            "relevance dataset snapshot hash is stale",
        )
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
        _error(
            errors,
            not (item.get("query_source") == "oracle_upper_bound" and item.get("null_query") is True),
            f"{item_label}: oracle_upper_bound queries cannot be null",
        )
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
    dev_null_count = sum(item.get("null_query") is True for item in dev_items)
    answerable_product_items = [
        item for item in dev_items
        if item.get("query_source") == "user_prompt_derived" and item.get("null_query") is False
    ]
    answerable_product_task_count = len({item.get("task_id") for item in answerable_product_items})
    product_null_count = sum(
        item.get("query_source") == "user_prompt_derived" and item.get("null_query") is True
        for item in dev_items
    )
    if "item_count" in development:
        _error(errors, development.get("item_count") == len(dev_items), "development item_count is stale")
    if "unique_task_count" in development:
        _error(errors, development.get("unique_task_count") == dev_task_count, "development unique_task_count is stale")
    if "null_query_count" in development:
        _error(errors, development.get("null_query_count") == dev_null_count, "development null_query_count is stale")
    if "answerable_product_item_count" in development:
        _error(
            errors,
            development.get("answerable_product_item_count") == len(answerable_product_items),
            "development answerable_product_item_count is stale",
        )
    if "unique_answerable_product_task_count" in development:
        _error(
            errors,
            development.get("unique_answerable_product_task_count") == answerable_product_task_count,
            "development unique_answerable_product_task_count is stale",
        )
    if "product_null_query_count" in development:
        _error(
            errors,
            development.get("product_null_query_count") == product_null_count,
            "development product_null_query_count is stale",
        )

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
        _error(
            errors,
            answerable_product_task_count >= offline.get("minimum_development_tasks", 0),
            "too few answerable product-derived development relevance tasks",
        )
        _error(
            errors,
            product_null_count >= offline.get("minimum_development_null_queries", 0),
            "too few corpus-closed product-derived development null queries",
        )
        _error(errors, sealed.get("item_count", 0) >= offline.get("minimum_sealed_holdout_queries", 0), "too few sealed relevance queries")
        _error(errors, sealed.get("unique_task_count", 0) >= offline.get("minimum_sealed_holdout_tasks", 0), "too few sealed relevance tasks")
        _error(errors, _is_sha256(sealed.get("commitment_sha256")), "relevance holdout commitment is missing")
        fresh_commitment = protocol.get("fresh_holdout", {}).get("commitment_sha256")
        _error(errors, sealed.get("commitment_sha256") == fresh_commitment, "fresh and relevance holdout commitments differ")
    return errors


def _load_engine_records(path: pathlib.Path, errors: list[str], repo: pathlib.Path) -> list[Any]:
    _error(errors, path.is_file(), "engine verification evidence must be exactly one regular JSON file")
    if not path.is_file():
        return []
    artifact = _load_artifact(path, errors, "engine verification evidence")
    if not isinstance(artifact, dict):
        errors.append("engine verification evidence must be a manifest object")
        return []
    _error(
        errors,
        set(artifact) == {"schema_version", "records"},
        "engine verification manifest must contain exactly schema_version and records",
    )
    _error(errors, artifact.get("schema_version") == 2, "engine verification manifest schema_version must be 2")
    records = artifact.get("records")
    if not isinstance(records, list):
        errors.append("engine verification manifest records must be an array")
        return []
    return records


def _parse_rfc3339(value: Any) -> dt.datetime | None:
    if not isinstance(value, str) or not value:
        return None
    try:
        parsed = dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        return None
    if parsed.tzinfo is None or parsed.utcoffset() is None:
        return None
    return parsed


def _parse_vector_artifact(path: pathlib.Path, errors: list[str], label: str) -> dict[str, Any] | None:
    try:
        raw = path.read_bytes()
        if raw[:4] != b"EBV1":
            raise ValueError("magic is not EBV1")
        offset = 4

        def take(fmt: str) -> tuple[int, ...]:
            nonlocal offset
            size = struct.calcsize(fmt)
            if offset + size > len(raw):
                raise ValueError("truncated header")
            values = struct.unpack_from(fmt, raw, offset)
            offset += size
            return values

        (model_len,) = take("<H")
        if offset + model_len > len(raw):
            raise ValueError("truncated model id")
        model_id = raw[offset : offset + model_len].decode("utf-8")
        offset += model_len
        (dimension,) = take("<I")
        (count,) = take("<I")
        if not model_id or dimension <= 0:
            raise ValueError("empty model id or zero dimension")
        fact_ids: list[str] = []
        seen: set[str] = set()
        for _ in range(count):
            (fact_id_len,) = take("<H")
            if offset + fact_id_len > len(raw):
                raise ValueError("truncated fact id")
            fact_id = raw[offset : offset + fact_id_len].decode("utf-8")
            offset += fact_id_len
            if not fact_id or fact_id in seen:
                raise ValueError("empty or duplicate fact id")
            seen.add(fact_id)
            fact_ids.append(fact_id)
            vector_bytes = dimension * 4
            if offset + vector_bytes > len(raw):
                raise ValueError(f"truncated vector for {fact_id}")
            for component in range(dimension):
                (value,) = struct.unpack_from("<f", raw, offset + component * 4)
                if not math.isfinite(value):
                    raise ValueError(f"non-finite float for {fact_id} component {component}")
            offset += vector_bytes
        if offset != len(raw):
            raise ValueError("trailing bytes")
    except (OSError, UnicodeDecodeError, ValueError, struct.error) as exc:
        errors.append(f"{label}: invalid EBV1 vector artifact: {exc}")
        return None
    return {"model_id": model_id, "dimension": dimension, "count": count, "fact_ids": fact_ids}


def _parse_eligibility_time(value: Any) -> dt.datetime | None:
    if not isinstance(value, str) or not value.strip():
        return None
    normalized = value.strip()
    if normalized.endswith("z") or normalized.endswith("Z"):
        normalized = normalized[:-1] + "+00:00"
    try:
        parsed = dt.datetime.fromisoformat(normalized)
    except ValueError:
        return None
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=dt.UTC)
    return parsed


def _derive_fact_eligibility(
    facts_path: pathlib.Path,
    sessions_path: pathlib.Path,
    pins: dict[str, Any],
    errors: list[str],
    label: str,
) -> dict[str, Any] | None:
    try:
        session_dates = load(sessions_path)
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        errors.append(f"{label}: retained session dates are unreadable: {exc}")
        return None
    if not isinstance(session_dates, dict) or not all(
        isinstance(key, str) and isinstance(value, str) for key, value in session_dates.items()
    ):
        errors.append(f"{label}: retained session dates must be a string-to-string object")
        return None
    task = pins.get("development_task", {})
    cutoff = _parse_eligibility_time(task.get("eligible_before"))
    if cutoff is None:
        errors.append(f"{label}: pinned temporal cutoff is invalid")
        return None
    excluded = {
        value.strip()
        for value in task.get("exclude_session_ids", [])
        if isinstance(value, str) and value.strip()
    }
    excluded_counts = {
        "empty_provenance": 0,
        "excluded_session": 0,
        "unknown_session": 0,
        "at_or_after_cutoff": 0,
    }
    all_ids: set[str] = set()
    eligible_ids: set[str] = set()
    active_eligible_ids: set[str] = set()
    try:
        with facts_path.open(encoding="utf-8") as handle:
            for line_number, line in enumerate(handle, 1):
                if not line.strip():
                    continue
                fact = json.loads(line)
                if not isinstance(fact, dict):
                    raise ValueError(f"line {line_number} is not an object")
                fact_id = fact.get("id")
                if not isinstance(fact_id, str) or not fact_id or fact_id in all_ids:
                    raise ValueError(f"line {line_number} has an invalid or duplicate id")
                status = fact.get("status")
                if status not in {"active", "superseded", "retracted"}:
                    raise ValueError(f"line {line_number} has invalid status")
                all_ids.add(fact_id)
                provenance = fact.get("provenance")
                has_empty_anchor = not isinstance(provenance, list) or len(provenance) == 0
                has_excluded_session = False
                has_unknown_session = False
                has_at_or_after_cutoff = False
                if isinstance(provenance, list):
                    for anchor in provenance:
                        if not isinstance(anchor, dict):
                            has_empty_anchor = True
                            continue
                        session_id = anchor.get("session_id")
                        if not isinstance(session_id, str) or not session_id.strip():
                            has_empty_anchor = True
                            continue
                        session_id = session_id.strip()
                        if session_id in excluded:
                            has_excluded_session = True
                            continue
                        created = _parse_eligibility_time(session_dates.get(session_id))
                        if created is None:
                            has_unknown_session = True
                            continue
                        if created >= cutoff:
                            has_at_or_after_cutoff = True
                reason = ""
                if has_empty_anchor:
                    reason = "empty_provenance"
                elif has_excluded_session:
                    reason = "excluded_session"
                elif has_unknown_session:
                    reason = "unknown_session"
                elif has_at_or_after_cutoff:
                    reason = "at_or_after_cutoff"
                if reason:
                    excluded_counts[reason] += 1
                    continue
                eligible_ids.add(fact_id)
                if status == "active":
                    active_eligible_ids.add(fact_id)
    except (OSError, UnicodeDecodeError, json.JSONDecodeError, ValueError) as exc:
        errors.append(f"{label}: retained facts cannot establish temporal/status eligibility: {exc}")
        return None
    return {
        "all_ids": all_ids,
        "eligible_ids": eligible_ids,
        "active_eligible_ids": active_eligible_ids,
        "excluded_counts": excluded_counts,
    }


def _dependency_inventory_sha256(rows: list[tuple[str, str]]) -> str:
    aggregate = hashlib.sha256()
    for relative, sha256 in rows:
        aggregate.update(relative.encode("utf-8"))
        aggregate.update(b"\0")
        aggregate.update(sha256.encode("ascii"))
        aggregate.update(b"\n")
    return aggregate.hexdigest()


def _engine_pin_descriptor(pins: dict[str, Any], *, require_production: bool) -> dict[str, str]:
    if require_production:
        digest_value = digest(ENGINE_PINS)
        authority = "production"
    else:
        digest_value = canonical_json_sha256(pins)
        authority = "test_fixture"
    return {"id": pins.get("pin_set_id"), "sha256": digest_value, "authority": authority}


def _git_value(repo: pathlib.Path, *args: str) -> str | None:
    try:
        completed = subprocess.run(
            ["git", *args],
            cwd=repo,
            capture_output=True,
            check=False,
            text=True,
        )
    except OSError:
        return None
    if completed.returncode != 0:
        return None
    return completed.stdout.strip()


def _validate_engine_pins(
    pins: Any,
    errors: list[str],
    *,
    repo: pathlib.Path,
    require_production: bool,
) -> dict[str, Any]:
    if not isinstance(pins, dict):
        errors.append("engine verification pins must be an object")
        return {}
    if require_production:
        canonical = load(ENGINE_PINS)
        _error(errors, pins == canonical, "engine verification did not use the canonical production pin set")
        _error(errors, pins.get("authority") == "production", "engine production pin authority is invalid")
    else:
        _error(errors, pins.get("authority") == "test_fixture", "injected engine pins must have test_fixture authority")
    _error(errors, pins.get("schema_version") == 1, "engine pin schema_version must be 1")
    _error(errors, isinstance(pins.get("pin_set_id"), str) and bool(pins.get("pin_set_id")), "engine pin_set_id is missing")

    binary = pins.get("binary", {})
    _error(errors, binary.get("provenance_mode") == "reproducible_build_v1", "engine binary provenance mode is invalid")
    _error(errors, _is_sha256(binary.get("binary_sha256")), "engine binary hash pin is invalid")
    _error(
        errors,
        _is_nonnegative_int(binary.get("binary_size_bytes")) and binary.get("binary_size_bytes", 0) > 0,
        "engine binary size pin is invalid",
    )
    _error(errors, _is_commit(binary.get("source_commit")), "engine binary source commit is invalid")
    _error(errors, _is_commit(binary.get("source_tree")), "engine binary source tree is invalid")
    build_command = binary.get("build_command")
    _error(
        errors,
        isinstance(build_command, list)
        and len(build_command) == 7
        and build_command[:5] == ["go", "build", "-trimpath", "-buildvcs=false", "-o"]
        and isinstance(build_command[5], str)
        and bool(build_command[5])
        and build_command[6] == "./cmd/entire-brain",
        "engine binary build command is not the deterministic contract",
    )
    _error(
        errors,
        isinstance(binary.get("go_version"), str) and binary.get("go_version", "").startswith("go version go"),
        "engine binary Go version is invalid",
    )
    if require_production and _is_commit(binary.get("source_commit")):
        commit = binary["source_commit"]
        _error(errors, _git_value(repo, "rev-parse", f"{commit}^{{commit}}") == commit, "engine binary source commit is unavailable")
        _error(errors, _git_value(repo, "show", "-s", "--format=%T", commit) == binary.get("source_tree"), "engine binary source tree differs from commit")

    task = pins.get("development_task", {})
    queries = task.get("queries", []) if isinstance(task, dict) else []
    query_ids: list[Any] = []
    query_hashes: list[Any] = []
    for index, query in enumerate(queries if isinstance(queries, list) else []):
        if not isinstance(query, dict):
            errors.append(f"engine pins development queries[{index}] must be an object")
            continue
        query_ids.append(query.get("query_id"))
        query_hashes.append(query.get("query_sha256"))
        text = query.get("query_text")
        _error(errors, isinstance(text, str) and bool(text), f"engine pins development queries[{index}] text is missing")
        if isinstance(text, str):
            _error(
                errors,
                query.get("query_sha256") == hashlib.sha256(text.encode("utf-8")).hexdigest(),
                f"engine pins development queries[{index}] hash is stale",
            )
    _error(errors, bool(query_ids) and _unique_strings(query_ids), "engine pin query ids are missing or duplicated")
    _error(errors, _unique_strings(query_hashes), "engine pin query hashes are not unique strings")
    exclusions = task.get("exclude_session_ids") if isinstance(task, dict) else None
    _error(errors, isinstance(exclusions, list) and _unique_strings(exclusions), "engine pin excluded sessions are invalid")
    _error(errors, isinstance(task.get("eligible_before"), str) and bool(task.get("eligible_before")), "engine pin cutoff is missing")
    _error(errors, isinstance(task.get("branch"), str) and bool(task.get("branch")), "engine pin branch is missing")
    _error(errors, _is_nonnegative_int(task.get("k")) and task.get("k", 0) > 0, "engine pin k is invalid")

    corpus = pins.get("corpus", {})
    for key in ("facts_sha256", "session_dates_sha256"):
        _error(errors, _is_sha256(corpus.get(key)), f"engine pin {key} is invalid")
    for key in ("eligible_ids_sha256", "semantic_candidate_ids_sha256"):
        _error(errors, _is_sha256(corpus.get(key)), f"engine pin {key} is invalid")
    _error(
        errors,
        corpus.get("candidate_ids_algorithm") == CANDIDATE_IDS_ALGORITHM,
        "engine pin candidate-id algorithm is unsupported",
    )
    for key in (
        "facts_size_bytes",
        "session_dates_size_bytes",
        "prefilter_count",
        "eligible_count",
        "semantic_candidate_count",
    ):
        _error(errors, _is_nonnegative_int(corpus.get(key)), f"engine pin {key} is invalid")
    if _is_nonnegative_int(corpus.get("eligible_count")) and _is_nonnegative_int(corpus.get("prefilter_count")):
        _error(errors, corpus["eligible_count"] <= corpus["prefilter_count"], "engine pin eligibility counts are impossible")
    if _is_nonnegative_int(corpus.get("semantic_candidate_count")) and _is_nonnegative_int(corpus.get("eligible_count")):
        _error(
            errors,
            0 < corpus["semantic_candidate_count"] <= corpus["eligible_count"],
            "engine pin semantic candidate count is impossible",
        )

    model = pins.get("embedding_model", {})
    _error(errors, _is_sha256(model.get("sha256")), "engine model pin hash is invalid")
    _error(errors, _is_nonnegative_int(model.get("size_bytes")) and model.get("size_bytes", 0) > 0, "engine model size pin is invalid")
    _error(errors, _is_nonnegative_int(model.get("dimension")) and model.get("dimension", 0) > 0, "engine model dimension pin is invalid")
    engines = pins.get("engines", {})
    _error(errors, isinstance(engines, dict) and set(engines) == set(ARMS), "engine pins do not cover the exact arms")

    runtime = pins.get("runtime", {})
    for key in ("platform", "node_version", "node_llama_cpp_version", "platform_package", "platform_package_version"):
        _error(errors, isinstance(runtime.get(key), str) and bool(runtime.get(key)), f"engine runtime pin {key} is invalid")
    for key in (
        "node_sha256",
        "server_script_sha256",
        "package_manifest_sha256",
        "package_lock_sha256",
        "dependency_inventory_sha256",
    ):
        _error(errors, _is_sha256(runtime.get(key)), f"engine runtime pin {key} is invalid")
    _error(
        errors,
        runtime.get("dependency_inventory_algorithm") == DEPENDENCY_INVENTORY_ALGORITHM,
        "engine dependency inventory algorithm is unsupported",
    )
    for key in ("dependency_file_count", "dependency_total_bytes"):
        _error(errors, _is_nonnegative_int(runtime.get(key)), f"engine runtime pin {key} is invalid")
    interval = runtime.get("server_health_interval_seconds")
    _error(
        errors,
        isinstance(interval, (int, float))
        and not isinstance(interval, bool)
        and 0 < interval <= 10,
        "engine runtime health interval is invalid or unbounded",
    )
    for stem in ("server_script", "package_manifest", "package_lock"):
        raw_path = runtime.get(f"{stem}_repo_path")
        target = _safe_relative_path(raw_path, repo)
        _error(errors, target is not None and target.is_file(), f"engine runtime pin {stem} path is missing")
        if target is not None and target.is_file() and _is_sha256(runtime.get(f"{stem}_sha256")):
            _error(errors, digest(target) == runtime[f"{stem}_sha256"], f"engine runtime pin {stem} hash is stale")
    return pins


def _validate_dependency_manifest(
    raw_path: Any,
    pins: dict[str, Any],
    errors: list[str],
    label: str,
    *,
    repo: pathlib.Path,
    expected_root: pathlib.Path | None = None,
) -> None:
    target = _safe_relative_path(raw_path, repo)
    if target is None or not target.is_file():
        return
    manifest = _load_artifact(target, errors, f"{label} dependency manifest")
    if not isinstance(manifest, dict):
        return
    runtime = pins.get("runtime", {})
    _error(errors, manifest.get("schema_version") == 1, f"{label}: dependency manifest schema_version must be 1")
    _error(errors, manifest.get("algorithm") == DEPENDENCY_INVENTORY_ALGORITHM, f"{label}: dependency algorithm mismatch")
    root = _safe_relative_path(manifest.get("root_path"), repo)
    _error(errors, root is not None and root.is_dir(), f"{label}: retained dependency root is missing")
    if root is not None and expected_root is not None:
        _error(errors, root == expected_root.resolve(), f"{label}: retained dependency root is not beside server script")
    files = manifest.get("files")
    _error(errors, isinstance(files, list) and bool(files), f"{label}: dependency files are missing")
    rows: list[tuple[str, str]] = []
    total_bytes = 0
    seen_relative: set[str] = set()
    if isinstance(files, list):
        for index, item in enumerate(files):
            item_label = f"{label}.dependency files[{index}]"
            if not _required_object_fields(errors, item, ("relative_path", "path", "sha256", "size_bytes"), item_label):
                continue
            relative = item.get("relative_path")
            _error(errors, isinstance(relative, str) and bool(relative) and ".." not in pathlib.Path(relative).parts, f"{item_label}: relative path is invalid")
            if isinstance(relative, str):
                _error(errors, relative not in seen_relative, f"{item_label}: duplicate relative path")
                seen_relative.add(relative)
            verified_path = _verify_hashed_file(errors, item, item_label, repo=repo)
            target_file = _safe_relative_path(verified_path, repo)
            if root is not None and target_file is not None and isinstance(relative, str):
                _error(errors, target_file == (root / relative).resolve(), f"{item_label}: path is outside dependency root")
            size = item.get("size_bytes")
            _error(errors, _is_nonnegative_int(size), f"{item_label}: size is invalid")
            if target_file is not None and target_file.is_file() and _is_nonnegative_int(size):
                _error(errors, target_file.stat().st_size == size, f"{item_label}: size mismatch")
                total_bytes += size
            if isinstance(relative, str) and _is_sha256(item.get("sha256")):
                rows.append((relative, item["sha256"]))
    rows.sort()
    aggregate = _dependency_inventory_sha256(rows)
    _error(errors, manifest.get("aggregate_sha256") == aggregate, f"{label}: dependency aggregate is stale")
    _error(errors, manifest.get("aggregate_sha256") == runtime.get("dependency_inventory_sha256"), f"{label}: dependency aggregate differs from pin")
    _error(errors, manifest.get("file_count") == len(rows) == runtime.get("dependency_file_count"), f"{label}: dependency file count differs from pin")
    _error(errors, manifest.get("total_bytes") == total_bytes == runtime.get("dependency_total_bytes"), f"{label}: dependency byte count differs from pin")
    for key in ("node_version", "node_llama_cpp_version", "platform_package", "platform_package_version"):
        _error(errors, manifest.get(key) == runtime.get(key), f"{label}: dependency {key} differs from pin")
    if root is not None and root.is_dir():
        actual: set[str] = set()
        for path in root.rglob("*"):
            _error(errors, not path.is_symlink(), f"{label}: retained dependency root contains a symlink")
            if path.is_file() and not path.is_symlink():
                actual.add(path.relative_to(root).as_posix())
        _error(errors, actual == seen_relative, f"{label}: retained dependency file set differs from manifest")


def _validate_binary_attestation(
    raw_path: Any,
    retained_binary_path: Any,
    pins: dict[str, Any],
    errors: list[str],
    label: str,
    *,
    repo: pathlib.Path,
) -> None:
    target = _safe_relative_path(raw_path, repo)
    if target is None or not target.is_file():
        return
    artifact = _load_artifact(target, errors, f"{label} binary attestation")
    if not isinstance(artifact, dict):
        return
    binary = pins.get("binary", {})
    expected = {
        "schema_version": 1,
        "pin_set_id": pins.get("pin_set_id"),
        "provenance_mode": binary.get("provenance_mode"),
        "binary_path": retained_binary_path,
        "binary_sha256": binary.get("binary_sha256"),
        "binary_size_bytes": binary.get("binary_size_bytes"),
        "source_commit": binary.get("source_commit"),
        "source_tree": binary.get("source_tree"),
        "build_command": binary.get("build_command"),
        "go_version": binary.get("go_version"),
    }
    _error(errors, artifact == expected, f"{label}: binary attestation differs from the pinned build contract")


def _load_runtime_stdout(raw_path: Any, errors: list[str], label: str, *, repo: pathlib.Path) -> dict[str, Any] | None:
    target = _safe_relative_path(raw_path, repo)
    if target is None or not target.is_file():
        return None
    artifact = _load_artifact(target, errors, f"{label} retained stdout")
    if not isinstance(artifact, dict):
        errors.append(f"{label}: retained stdout must be one JSON object")
        return None
    return artifact


def _validate_runtime_stdout_reconciliation(
    output: dict[str, Any],
    arm_id: Any,
    engine_pin: dict[str, Any],
    effective: dict[str, Any],
    corpus: dict[str, Any],
    result: dict[str, Any],
    errors: list[str],
    label: str,
) -> None:
    engine = output.get("retrieval_engine")
    eligibility = output.get("eligibility")
    facts = output.get("facts")
    _error(errors, output.get("effective_engine") == arm_id, f"{label}: stdout top-level engine differs from record")
    if not isinstance(engine, dict):
        errors.append(f"{label}: stdout retrieval_engine is missing")
    else:
        semantic = engine_pin.get("semantic") is True
        comparisons = (
            ("effective_engine", arm_id),
            ("semantic_requested", semantic),
            ("semantic_applied", semantic),
            ("semantic_available", effective.get("semantic_available")),
            ("bm25_enabled", effective.get("bm25_enabled")),
            ("fallback_used", effective.get("fallback_used")),
        )
        for field, expected in comparisons:
            _error(errors, engine.get(field) == expected, f"{label}: stdout {field} differs from record/pin")
        _error(errors, engine.get("identity_verified") is True, f"{label}: stdout engine identity is not verified")
        _error(
            errors,
            (engine.get("embedder_id") if semantic else None) == effective.get("embedder_id"),
            f"{label}: stdout embedder differs from record",
        )
        _error(
            errors,
            (engine.get("embedding_dimension") if semantic else None) == effective.get("embedding_dimension"),
            f"{label}: stdout embedding dimension differs from record",
        )
        _error(
            errors,
            (engine.get("vector_count", 0) if semantic else 0) == effective.get("vector_count"),
            f"{label}: stdout vector_count differs from record",
        )
        _error(
            errors,
            (engine.get("vector_candidate_count", 0) if semantic else 0)
            == effective.get("vector_candidate_count"),
            f"{label}: stdout vector_candidate_count differs from record",
        )
        _error(
            errors,
            (engine.get("loaded_vector_count", 0) if semantic else 0) == effective.get("loaded_vector_count"),
            f"{label}: stdout loaded_vector_count differs from record",
        )
        _error(
            errors,
            (engine.get("resident_vector_count", 0) if semantic else 0) == effective.get("resident_vector_count"),
            f"{label}: stdout resident_vector_count differs from record",
        )
        if semantic:
            _error(errors, engine.get("vector_cache_backend") == "flat_file", f"{label}: stdout vector backend is not flat_file")
            _error(errors, engine.get("vector_cache_read_only") is False, f"{label}: stdout vector cache is read-only")
    if not isinstance(eligibility, dict):
        errors.append(f"{label}: stdout eligibility is missing")
    else:
        _error(errors, eligibility.get("prefilter_corpus_count") == corpus.get("prefilter_count"), f"{label}: stdout prefilter count differs from record")
        _error(errors, eligibility.get("eligible_count") == corpus.get("eligible_count"), f"{label}: stdout eligible count differs from record")
        _error(errors, eligibility.get("excluded_counts") == corpus.get("excluded_by_reason"), f"{label}: stdout excluded counts differ from record")
        _error(errors, eligibility.get("delivered_count") == corpus.get("delivered_count"), f"{label}: stdout delivered count differs from record")
    fact_ids = [fact.get("id") if isinstance(fact, dict) else None for fact in facts] if isinstance(facts, list) else None
    _error(errors, fact_ids == result.get("fact_ids_in_order"), f"{label}: stdout ranked fact ids differ from record")
    if isinstance(fact_ids, list) and isinstance(result.get("fact_ids_in_order"), list):
        result_count = len(result["fact_ids_in_order"])
        _error(errors, len(fact_ids) == result_count, f"{label}: stdout fact count differs from ranked fact count")
        _error(
            errors,
            corpus.get("delivered_count") == result_count,
            f"{label}: corpus delivered_count differs from ranked fact count",
        )
        if isinstance(eligibility, dict):
            _error(
                errors,
                eligibility.get("delivered_count") == result_count,
                f"{label}: stdout delivered_count differs from ranked fact count",
            )


def _validate_server_attestation(
    raw_path: Any,
    requested: dict[str, Any],
    pins: dict[str, Any],
    errors: list[str],
    label: str,
    *,
    repo: pathlib.Path,
    relocation: dict[str, Any] | None = None,
    expected_model_path: pathlib.Path | None = None,
) -> None:
    target = _safe_relative_path(raw_path, repo)
    if target is None or not target.is_file():
        return
    artifact = _load_artifact(target, errors, f"{label} server attestation")
    if not isinstance(artifact, dict):
        return
    model = pins.get("embedding_model", {})
    runtime = pins.get("runtime", {})
    _error(
        errors,
        set(artifact)
        == {
            "schema_version",
            "pin_set_id",
            "process_pid",
            "ownership_token_sha256",
            "model_path",
            "model_sha256",
            "embedding_dimension",
            "node_version",
            "health_interval_seconds",
            "recall_window",
            "observations",
        },
        f"{label}: server attestation fields differ from the v2 contract",
    )
    _error(errors, artifact.get("schema_version") == 2, f"{label}: server attestation schema_version must be 2")
    _error(errors, artifact.get("pin_set_id") == pins.get("pin_set_id"), f"{label}: server attestation pin set differs")
    pid = artifact.get("process_pid")
    _error(errors, _is_nonnegative_int(pid) and pid > 0, f"{label}: server attestation PID is invalid")
    token_hash = artifact.get("ownership_token_sha256")
    _error(errors, _is_sha256(token_hash), f"{label}: server ownership token hash is invalid")
    model_path = artifact.get("model_path")
    _error(errors, isinstance(model_path, str) and bool(model_path), f"{label}: attested model path is missing")
    if relocation is not None:
        _map_recorded_artifact_path(
            model_path,
            relocation,
            errors,
            f"{label}: attested model path",
            expected=expected_model_path,
        )
    _error(errors, artifact.get("model_sha256") == model.get("sha256"), f"{label}: attested model hash differs from pin")
    _error(errors, artifact.get("embedding_dimension") == model.get("dimension"), f"{label}: attested dimension differs from pin")
    _error(errors, artifact.get("node_version") == runtime.get("node_version"), f"{label}: attested Node version differs from pin")
    _error(
        errors,
        artifact.get("health_interval_seconds") == runtime.get("server_health_interval_seconds"),
        f"{label}: attested health interval differs from pin",
    )
    environment = requested.get("embedding_server_environment")
    _error(errors, isinstance(environment, dict), f"{label}: embedding server environment is missing")
    if isinstance(environment, dict) and isinstance(environment.get("ENGINE_VERIFICATION_TOKEN"), str):
        _error(
            errors,
            hashlib.sha256(environment["ENGINE_VERIFICATION_TOKEN"].encode("utf-8")).hexdigest() == token_hash,
            f"{label}: server environment ownership token differs from attestation",
        )
        gguf = environment.get("GGUF")
        if relocation is None:
            _error(
                errors,
                isinstance(gguf, str)
                and isinstance(model_path, str)
                and pathlib.Path(gguf).resolve() == pathlib.Path(model_path).resolve(),
                f"{label}: attested model path differs from server environment",
            )
        else:
            mapped_gguf = _map_recorded_artifact_path(
                gguf,
                relocation,
                errors,
                f"{label}: server environment GGUF",
                expected=expected_model_path,
            )
            mapped_model = _map_recorded_artifact_path(
                model_path,
                relocation,
                errors,
                f"{label}: server attestation model",
                expected=expected_model_path,
            )
            _error(errors, mapped_gguf is not None and mapped_gguf == mapped_model, f"{label}: attested model path differs from server environment")
    else:
        errors.append(f"{label}: server environment ownership token is missing")
    recall_window = artifact.get("recall_window")
    _error(
        errors,
        isinstance(recall_window, dict) and set(recall_window) == {"started_at", "finished_at"},
        f"{label}: recall window is invalid",
    )
    recall_started = _parse_rfc3339(recall_window.get("started_at")) if isinstance(recall_window, dict) else None
    recall_finished = _parse_rfc3339(recall_window.get("finished_at")) if isinstance(recall_window, dict) else None
    _error(errors, recall_started is not None, f"{label}: recall start is not timezone-aware RFC3339")
    _error(errors, recall_finished is not None, f"{label}: recall finish is not timezone-aware RFC3339")
    if recall_started is not None and recall_finished is not None:
        _error(errors, recall_started < recall_finished, f"{label}: recall window is empty or reversed")

    observations = artifact.get("observations")
    _error(errors, isinstance(observations, list) and len(observations) >= 3, f"{label}: server health observations are incomplete")
    phases: list[Any] = []
    observed_times: list[dt.datetime] = []
    request_counts: list[Any] = []
    nonce_hashes: list[Any] = []
    if isinstance(observations, list):
        for index, observation in enumerate(observations):
            observation_label = f"{label}.server observations[{index}]"
            if not _required_object_fields(
                errors,
                observation,
                (
                    "sequence",
                    "phase",
                    "observed_at",
                    "pid",
                    "ownership_token_sha256",
                    "request_nonce_sha256",
                    "health_request_count",
                    "model_path",
                    "model_sha256",
                    "embedding_dimension",
                    "node_version",
                    "healthy",
                ),
                observation_label,
            ):
                continue
            phases.append(observation.get("phase"))
            observed_at = _parse_rfc3339(observation.get("observed_at"))
            if observed_at is not None:
                observed_times.append(observed_at)
            request_counts.append(observation.get("health_request_count"))
            nonce_hashes.append(observation.get("request_nonce_sha256"))
            _error(errors, observation.get("sequence") == index, f"{observation_label}: sequence is not contiguous")
            _error(
                errors,
                observation.get("phase") in {"pre_recall", "heartbeat", "during_recall", "post_recall"},
                f"{observation_label}: phase is invalid",
            )
            _error(errors, observation.get("healthy") is True, f"{observation_label}: health is not true")
            _error(errors, observation.get("pid") == pid, f"{observation_label}: PID changed")
            _error(errors, observation.get("ownership_token_sha256") == token_hash, f"{observation_label}: ownership token changed")
            _error(errors, observation.get("model_path") == model_path, f"{observation_label}: model path changed")
            _error(errors, observation.get("model_sha256") == model.get("sha256"), f"{observation_label}: model hash changed")
            _error(errors, observation.get("embedding_dimension") == model.get("dimension"), f"{observation_label}: dimension changed")
            _error(errors, observation.get("node_version") == runtime.get("node_version"), f"{observation_label}: Node version changed")
            _error(errors, observed_at is not None, f"{observation_label}: timestamp is not timezone-aware RFC3339")
            _error(errors, _is_sha256(observation.get("request_nonce_sha256")), f"{observation_label}: nonce hash is invalid")
            _error(
                errors,
                _is_nonnegative_int(observation.get("health_request_count"))
                and observation.get("health_request_count", 0) > 0,
                f"{observation_label}: health request counter is invalid",
            )
    _error(errors, phases and phases[0] == "pre_recall", f"{label}: first health attestation is not pre-recall")
    _error(errors, phases.count("pre_recall") == 1, f"{label}: pre-recall health attestation must occur exactly once")
    _error(errors, phases.count("during_recall") == 1, f"{label}: during-recall health attestation must occur exactly once")
    _error(errors, phases.count("post_recall") == 1, f"{label}: post-recall health attestation must occur exactly once")
    _error(errors, phases and phases[-1] == "post_recall", f"{label}: final health attestation is not post-recall")
    _error(
        errors,
        len(observed_times) == len(phases)
        and all(left < right for left, right in zip(observed_times, observed_times[1:])),
        f"{label}: health observation timestamps are not strictly increasing",
    )
    _error(
        errors,
        all(_is_nonnegative_int(value) for value in request_counts)
        and all(left < right for left, right in zip(request_counts, request_counts[1:])),
        f"{label}: health request counters are not strictly increasing",
    )
    _error(errors, _unique_strings(nonce_hashes), f"{label}: health request nonces are invalid or reused")
    if (
        recall_started is not None
        and recall_finished is not None
        and len(observed_times) == len(phases)
        and phases.count("during_recall") == 1
        and phases
    ):
        during_time = observed_times[phases.index("during_recall")]
        _error(
            errors,
            observed_times[0] < recall_started < during_time < recall_finished < observed_times[-1],
            f"{label}: pre/during/post health evidence does not strictly bound active recall",
        )


def validate_engine_verification(
    matrix: dict[str, Any],
    check: dict[str, Any],
    *,
    here: pathlib.Path = HERE,
    repo: pathlib.Path = REPO,
    pins: dict[str, Any] | None = None,
    require_production: bool = True,
    pin_repo: pathlib.Path | None = None,
    require_storage_contract: bool = False,
) -> list[str]:
    errors: list[str] = []
    if check.get("status") != "pass":
        return errors
    pin_data = load(ENGINE_PINS) if pins is None else pins
    pin_data = _validate_engine_pins(
        pin_data,
        errors,
        repo=repo if pin_repo is None else pin_repo,
        require_production=require_production,
    )
    expected_descriptor = _engine_pin_descriptor(pin_data, require_production=require_production)
    evidence_path = _evidence_path(check.get("evidence"), here, repo)
    _error(errors, evidence_path is not None and evidence_path.exists(), "engine pass evidence is missing")
    _error(
        errors,
        not _evidence_path_contains_symlink(check.get("evidence"), here, repo),
        "engine verification evidence must not traverse a symlink",
    )
    if evidence_path is None or not evidence_path.exists():
        return errors
    relocation: dict[str, Any] | None = None
    artifact_repo = repo
    if require_storage_contract:
        _error(
            errors,
            check.get("evidence") == ENGINE_EVIDENCE_STORAGE_REPO_PATH,
            "engine pass evidence must reference the canonical storage contract",
        )
        relocation = _validate_engine_storage_contract(
            evidence_path,
            errors,
            repo=repo,
            require_published=True,
        )
        if relocation is None:
            return errors
        evidence_path = relocation["manifest_path"]
        artifact_repo = relocation["artifact_repo"]
    manifest_identity = _load_artifact(evidence_path, errors, "engine verification manifest identity")
    if isinstance(manifest_identity, dict) and manifest_identity.get("schema_version") == 4:
        _validate_public_engine_manifest_schema(evidence_path, errors)
        errors.extend(
            public_engine_evidence.validate_public_bundle(
                evidence_path,
                matrix,
                pin_data,
                expected_descriptor,
            )
        )
        if require_production:
            errors.append(
                "public v4 evidence lacks an authenticated restricted replay attestation bound to the v4 manifest, pin set, and checker"
            )
        return errors
    if require_storage_contract and require_production:
        errors.append(
            "legacy engine evidence schema v2 is diagnostic-only; authoritative stored evidence requires public schema v4"
        )
    _validate_engine_manifest_schema(evidence_path, errors)
    records = _load_engine_records(evidence_path, errors, artifact_repo)
    record_arms = [record.get("arm") if isinstance(record, dict) else None for record in records]
    _error(errors, len(record_arms) == len(ARMS), "engine verification must contain exactly one record per primary arm")
    _error(errors, _unique_strings(record_arms), "engine verification arms are not unique strings")
    _error(errors, record_arms == ARMS, "engine verification does not cover the primary arms in canonical order")
    matrix_by_id = {
        arm.get("id"): arm for arm in matrix.get("arms", []) if isinstance(arm, dict) and isinstance(arm.get("id"), str)
    }
    pinned_queries = {
        item.get("query_id"): item
        for item in pin_data.get("development_task", {}).get("queries", [])
        if isinstance(item, dict) and isinstance(item.get("query_id"), str)
    }
    development_k = pin_data.get("development_task", {}).get("k")
    corpus_pin = pin_data.get("corpus", {})
    runtime_pin = pin_data.get("runtime", {})
    model_pin = pin_data.get("embedding_model", {})
    engine_pins = pin_data.get("engines", {})
    artifact_fields = tuple(
        f"{stem}_{suffix}"
        for stem in (
            "binary",
            "binary_attestation",
            "stdout",
            "stderr",
            "facts_source",
            "session_dates_source",
            "derived_facts",
            "vector_artifact",
            "embedding_model",
            "embedding_server_stdout",
            "embedding_server_stderr",
            "embedding_server_attestation",
            "server_script",
            "node_runtime",
            "package_manifest",
            "package_lock",
            "runtime_dependency_manifest",
        )
        for suffix in ("path", "sha256")
    )
    server_stems = (
        "embedding_server_stdout",
        "embedding_server_stderr",
        "embedding_server_attestation",
        "server_script",
        "node_runtime",
        "package_manifest",
        "package_lock",
        "runtime_dependency_manifest",
    )
    namespaces: list[Any] = []
    semantic_vector_paths: list[Any] = []
    corpus_signatures: list[tuple[Any, Any, Any]] = []
    query_ids: list[Any] = []
    pin_descriptors: list[Any] = []
    facts_source_paths: list[Any] = []
    session_source_paths: list[Any] = []
    derived_facts_paths: list[Any] = []
    binary_paths: list[Any] = []
    binary_hashes: list[Any] = []
    binary_attestation_paths: list[Any] = []
    for index, record in enumerate(records):
        label = f"engine records[{index}]"
        if not _required_object_fields(
            errors,
            record,
            ("schema_version", "pin_set", "arm", "requested", "effective", "artifacts", "corpus", "result"),
            label,
        ):
            continue
        arm_id = record.get("arm")
        arm = matrix_by_id.get(arm_id, {}) if isinstance(arm_id, str) else {}
        engine_pin = engine_pins.get(arm_id, {}) if isinstance(engine_pins, dict) else {}
        _error(errors, record.get("schema_version") == 2, f"{label}: schema_version must be 2")
        pin_set = record.get("pin_set")
        if _required_object_fields(errors, pin_set, ("id", "sha256", "authority"), f"{label}.pin_set"):
            pin_descriptors.append(pin_set)
            _error(errors, pin_set == expected_descriptor, f"{label}: pin descriptor is not authoritative")
        requested = record.get("requested")
        effective = record.get("effective")
        artifacts = record.get("artifacts")
        corpus = record.get("corpus")
        result = record.get("result")
        derived_fact_ids: set[str] | None = None
        active_eligible_ids: set[str] | None = None
        derived_eligibility: dict[str, Any] | None = None
        requested_ok = _required_object_fields(errors, requested, ("command", "environment", "namespace"), f"{label}.requested")
        effective_ok = _required_object_fields(
            errors,
            effective,
            ("engine", "semantic_available", "bm25_enabled", "fallback_used", "embedder_id", "embedding_dimension", "vector_count", "vector_candidate_count", "loaded_vector_count", "resident_vector_count", "vector_namespace"),
            f"{label}.effective",
        )
        artifacts_ok = _required_object_fields(errors, artifacts, artifact_fields, f"{label}.artifacts")
        corpus_ok = _required_object_fields(
            errors,
            corpus,
            ("facts_sha256", "prefilter_count", "eligible_count", "excluded_by_reason", "delivered_count"),
            f"{label}.corpus",
        )
        result_ok = _required_object_fields(errors, result, ("query_id", "fact_ids_in_order", "output_valid"), f"{label}.result")

        if requested_ok:
            command = requested.get("command")
            command_ok = isinstance(command, list) and bool(command) and all(isinstance(part, str) and part for part in command)
            _error(errors, command_ok, f"{label}: requested command is invalid")
            environment = requested.get("environment")
            _error(errors, isinstance(environment, dict), f"{label}: requested environment must be an object")
            if isinstance(environment, dict):
                for key, value in arm.get("environment", {}).items():
                    _error(errors, environment.get(key) == value, f"{label}: requested environment mismatch: {key}")
                _error(errors, environment.get("ENGINE_VERIFICATION_PIN_SET_ID") == pin_data.get("pin_set_id"), f"{label}: runtime pin-set environment differs")
            _error(errors, requested.get("namespace") == arm.get("namespace"), f"{label}: requested namespace mismatch")

        if effective_ok:
            namespace = effective.get("vector_namespace")
            namespaces.append(namespace)
            _error(errors, effective.get("engine") == arm_id == arm.get("effective_engine_required"), f"{label}: effective engine mismatch")
            _error(errors, effective.get("semantic_available") is arm.get("semantic") is engine_pin.get("semantic"), f"{label}: semantic availability mismatch")
            _error(errors, effective.get("bm25_enabled") is False, f"{label}: BM25 must be disabled")
            _error(errors, effective.get("fallback_used") is False, f"{label}: fallback is prohibited")
            _error(errors, namespace == arm.get("namespace"), f"{label}: effective namespace mismatch")
            _error(errors, isinstance(namespace, str) and bool(namespace), f"{label}: vector namespace is missing")
            _error(errors, _is_nonnegative_int(effective.get("vector_count")), f"{label}: vector_count is invalid")
            _error(errors, _is_nonnegative_int(effective.get("vector_candidate_count")), f"{label}: vector_candidate_count is invalid")
            _error(errors, _is_nonnegative_int(effective.get("loaded_vector_count")), f"{label}: loaded_vector_count is invalid")
            _error(errors, _is_nonnegative_int(effective.get("resident_vector_count")), f"{label}: resident_vector_count is invalid")
            if arm.get("semantic") is True:
                _error(
                    errors,
                    _is_nonnegative_int(effective.get("vector_count")) and effective.get("vector_count", 0) > 0,
                    f"{label}: semantic vector_count must be positive",
                )
                _error(
                    errors,
                    _is_nonnegative_int(effective.get("resident_vector_count"))
                    and effective.get("resident_vector_count", 0) > 0,
                    f"{label}: semantic resident_vector_count must be positive",
                )
            _error(errors, effective.get("embedder_id") == engine_pin.get("embedder_id"), f"{label}: embedder differs from canonical pin")
            _error(errors, effective.get("embedding_dimension") == engine_pin.get("dimension"), f"{label}: dimension differs from canonical pin")

        if artifacts_ok:
            if relocation is not None:
                for field in artifact_fields:
                    if not field.endswith("_path") or artifacts.get(field) is None:
                        continue
                    target = _safe_relative_path(artifacts.get(field), artifact_repo)
                    try:
                        inside_archive = target is not None and target.relative_to(relocation["archive_root"]) is not None
                    except ValueError:
                        inside_archive = False
                    _error(errors, inside_archive, f"{label}.{field}: retained artifact path is outside the hydrated archive root")
            for stem in ("binary", "binary_attestation", "stdout", "stderr", "facts_source", "session_dates_source", "derived_facts"):
                _verify_hashed_file(
                    errors,
                    {"path": artifacts.get(f"{stem}_path"), "sha256": artifacts.get(f"{stem}_sha256")},
                    f"{label}.{stem} artifact",
                    repo=artifact_repo,
                )
            binary_paths.append(artifacts.get("binary_path"))
            binary_hashes.append(artifacts.get("binary_sha256"))
            binary_attestation_paths.append(artifacts.get("binary_attestation_path"))
            facts_source_paths.append(artifacts.get("facts_source_path"))
            session_source_paths.append(artifacts.get("session_dates_source_path"))
            derived_facts_paths.append(artifacts.get("derived_facts_path"))
            _error(errors, artifacts.get("facts_source_sha256") == corpus_pin.get("facts_sha256"), f"{label}: retained facts source differs from pin")
            _error(errors, artifacts.get("session_dates_source_sha256") == corpus_pin.get("session_dates_sha256"), f"{label}: retained session dates differ from pin")
            _error(errors, artifacts.get("derived_facts_sha256") == corpus_pin.get("facts_sha256"), f"{label}: derived facts changed during recall")
            binary_target = _safe_relative_path(artifacts.get("binary_path"), artifact_repo)
            sessions_target = _safe_relative_path(artifacts.get("session_dates_source_path"), artifact_repo)
            derived_target = _safe_relative_path(artifacts.get("derived_facts_path"), artifact_repo)
            if (
                derived_target is not None
                and derived_target.is_file()
                and sessions_target is not None
                and sessions_target.is_file()
            ):
                derived_eligibility = _derive_fact_eligibility(
                    derived_target,
                    sessions_target,
                    pin_data,
                    errors,
                    f"{label}.derived eligibility",
                )
                if derived_eligibility is not None:
                    derived_fact_ids = derived_eligibility["all_ids"]
                    active_eligible_ids = derived_eligibility["active_eligible_ids"]
                    eligible_ids = derived_eligibility["eligible_ids"]
                    _error(errors, len(derived_fact_ids) == corpus_pin.get("prefilter_count"), f"{label}: derived prefilter count differs from pin")
                    _error(errors, len(eligible_ids) == corpus_pin.get("eligible_count"), f"{label}: derived eligible count differs from pin")
                    _error(
                        errors,
                        canonical_json_sha256(sorted(eligible_ids)) == corpus_pin.get("eligible_ids_sha256"),
                        f"{label}: derived eligible fact IDs differ from pin",
                    )
                    _error(
                        errors,
                        len(active_eligible_ids) == corpus_pin.get("semantic_candidate_count"),
                        f"{label}: derived semantic candidate count differs from pin",
                    )
                    _error(
                        errors,
                        canonical_json_sha256(sorted(active_eligible_ids))
                        == corpus_pin.get("semantic_candidate_ids_sha256"),
                        f"{label}: derived semantic candidate IDs differ from pin",
                    )
                    if isinstance(effective, dict):
                        if arm.get("semantic") is True:
                            expected_count = len(active_eligible_ids)
                            for field in ("vector_count", "vector_candidate_count", "resident_vector_count"):
                                _error(
                                    errors,
                                    effective.get(field) == expected_count,
                                    f"{label}: {field} does not cover exact active+eligible candidates",
                                )
                            _error(
                                errors,
                                effective.get("loaded_vector_count") == 0,
                                f"{label}: clean derived namespace inherited vectors",
                            )
                        else:
                            for field in (
                                "vector_count",
                                "vector_candidate_count",
                                "loaded_vector_count",
                                "resident_vector_count",
                            ):
                                _error(errors, effective.get(field) == 0, f"{label}: lexical {field} must be zero")
            _error(errors, artifacts.get("binary_sha256") == pin_data.get("binary", {}).get("binary_sha256"), f"{label}: retained binary differs from canonical pin")
            if binary_target is not None and binary_target.is_file():
                _error(
                    errors,
                    binary_target.stat().st_size == pin_data.get("binary", {}).get("binary_size_bytes"),
                    f"{label}: retained binary size differs from canonical pin",
                )
            _validate_binary_attestation(
                artifacts.get("binary_attestation_path"),
                artifacts.get("binary_path"),
                pin_data,
                errors,
                label,
                repo=artifact_repo,
            )
            command = requested.get("command") if isinstance(requested, dict) else None
            query_id = result.get("query_id") if isinstance(result, dict) else None
            query_pin = pinned_queries.get(query_id)
            if (
                isinstance(command, list)
                and binary_target is not None
                and sessions_target is not None
                and isinstance(query_pin, dict)
            ):
                task_pin = pin_data.get("development_task", {})
                expected_command = [
                    str(binary_target),
                    "recall",
                    query_pin.get("query_text"),
                    "--branch",
                    task_pin.get("branch"),
                    "--k",
                    str(task_pin.get("k")),
                    "--eligible-before",
                    task_pin.get("eligible_before"),
                    "--session-dates",
                    str(sessions_target),
                ]
                for session_id in task_pin.get("exclude_session_ids", []):
                    expected_command.extend(("--exclude-session-id", session_id))
                if arm_id == "lexical_handrolled":
                    expected_command.append("--no-semantic")
                expected_command.append("--json")
                if relocation is not None:
                    expected_command[0] = str(relocation["recorded_artifact_root"] / artifacts["binary_path"])
                    expected_command[10] = str(relocation["recorded_artifact_root"] / artifacts["session_dates_source_path"])
                    _map_recorded_artifact_path(
                        command[0] if command else None,
                        relocation,
                        errors,
                        f"{label}: recall command binary",
                        expected=binary_target,
                    )
                    _map_recorded_artifact_path(
                        command[10] if len(command) > 10 else None,
                        relocation,
                        errors,
                        f"{label}: recall command session dates",
                        expected=sessions_target,
                    )
                    for command_index, part in enumerate(command):
                        if isinstance(part, str) and pathlib.Path(part).is_absolute() and command_index not in {0, 10}:
                            errors.append(f"{label}: recall command contains an unmapped absolute field at index {command_index}")
                _error(errors, command == expected_command, f"{label}: requested recall command differs from the pinned retained-binary invocation")

            if relocation is not None and isinstance(requested, dict):
                environment = requested.get("environment")
                mapped_environment = {
                    "ENTIRE_PLUGIN_CONFIG_DIR": "config",
                    "ENTIRE_PLUGIN_DATA_DIR": "data",
                    "ENTIRE_PLUGIN_STATE_DIR": "state",
                    "ENTIRE_PLUGIN_CACHE_DIR": "cache",
                }
                if isinstance(environment, dict):
                    for key, directory in mapped_environment.items():
                        _map_recorded_artifact_path(
                            environment.get(key),
                            relocation,
                            errors,
                            f"{label}: {key}",
                            expected=relocation["manifest_root"] / "runtime" / str(arm_id) / directory,
                            kind="directory",
                        )
                    _error(
                        errors,
                        environment.get("ENTIRE_REPO_ROOT") == relocation["recorded_external_repo_root"],
                        f"{label}: ENTIRE_REPO_ROOT differs from the recorded external input",
                    )
                    inert_absolute_keys = {"HOME", "PATH", "TMPDIR"}
                    allowed_absolute_keys = set(mapped_environment) | {"ENTIRE_REPO_ROOT"} | inert_absolute_keys
                    for key, value in environment.items():
                        if isinstance(value, str) and pathlib.Path(value).is_absolute() and key not in allowed_absolute_keys:
                            errors.append(f"{label}: requested environment contains unmapped absolute field {key}")
                repo_key = relocation["recorded_external_repo_key"]
                if isinstance(repo_key, str) and derived_target is not None:
                    expected_fragment = pathlib.Path("data") / "repos" / repo_key / "facts"
                    _error(
                        errors,
                        expected_fragment.as_posix() in derived_target.as_posix(),
                        f"{label}: derived facts path does not bind the recorded external repo key",
                    )

            stdout = _load_runtime_stdout(artifacts.get("stdout_path"), errors, label, repo=artifact_repo)
            if (
                stdout is not None
                and isinstance(effective, dict)
                and isinstance(corpus, dict)
                and isinstance(result, dict)
            ):
                _validate_runtime_stdout_reconciliation(
                    stdout,
                    arm_id,
                    engine_pin,
                    effective,
                    corpus,
                    result,
                    errors,
                    label,
                )
                if relocation is not None and arm.get("semantic") is True:
                    runtime_engine = stdout.get("retrieval_engine")
                    runtime_vector = runtime_engine.get("vector_cache_path") if isinstance(runtime_engine, dict) else None
                    expected_runtime_vector = derived_target.parent / "embeddings" / "vectors.bin" if derived_target is not None else None
                    _map_recorded_artifact_path(
                        runtime_vector,
                        relocation,
                        errors,
                        f"{label}: runtime vector cache",
                        expected=expected_runtime_vector,
                    )

            vector_path = artifacts.get("vector_artifact_path")
            vector_hash = artifacts.get("vector_artifact_sha256")
            if arm.get("semantic") is True:
                semantic_vector_paths.append(vector_path)
                _verify_hashed_file(errors, {"path": vector_path, "sha256": vector_hash}, f"{label}.vector artifact", repo=artifact_repo)
                vector_target = _safe_relative_path(vector_path, artifact_repo)
                parsed_vector = (
                    _parse_vector_artifact(vector_target, errors, f"{label}.vector artifact")
                    if vector_target is not None and vector_target.is_file()
                    else None
                )
                if parsed_vector is not None and isinstance(effective, dict):
                    _error(errors, parsed_vector["model_id"] == effective.get("embedder_id"), f"{label}: EBV1 model id differs from record/runtime")
                    _error(errors, parsed_vector["dimension"] == effective.get("embedding_dimension"), f"{label}: EBV1 dimension differs from record/runtime")
                    _error(errors, parsed_vector["count"] == effective.get("resident_vector_count"), f"{label}: EBV1 count differs from record/runtime")
                    if active_eligible_ids is not None:
                        _error(
                            errors,
                            set(parsed_vector["fact_ids"]) == active_eligible_ids,
                            f"{label}: EBV1 fact IDs do not exactly cover active+eligible candidates",
                        )
            else:
                _error(errors, vector_path is None and vector_hash is None, f"{label}: lexical vector artifact must be null")

            model_path = artifacts.get("embedding_model_path")
            model_hash = artifacts.get("embedding_model_sha256")
            if arm_id == "embeddinggemma_rrf":
                _verify_hashed_file(errors, {"path": model_path, "sha256": model_hash}, f"{label}.embedding model artifact", repo=artifact_repo)
                _error(errors, model_hash == model_pin.get("sha256"), f"{label}: embedding model differs from pin")
                for stem in server_stems:
                    _verify_hashed_file(
                        errors,
                        {"path": artifacts.get(f"{stem}_path"), "sha256": artifacts.get(f"{stem}_sha256")},
                        f"{label}.{stem} artifact",
                        repo=artifact_repo,
                    )
                for stem, expected in (
                    ("server_script", runtime_pin.get("server_script_sha256")),
                    ("node_runtime", runtime_pin.get("node_sha256")),
                    ("package_manifest", runtime_pin.get("package_manifest_sha256")),
                    ("package_lock", runtime_pin.get("package_lock_sha256")),
                ):
                    _error(errors, artifacts.get(f"{stem}_sha256") == expected, f"{label}: retained {stem} differs from pin")
                server_script_target = _safe_relative_path(artifacts.get("server_script_path"), artifact_repo)
                node_target = _safe_relative_path(artifacts.get("node_runtime_path"), artifact_repo)
                model_target = _safe_relative_path(model_path, artifact_repo)
                package_target = _safe_relative_path(artifacts.get("package_manifest_path"), artifact_repo)
                lock_target = _safe_relative_path(artifacts.get("package_lock_path"), artifact_repo)
                if server_script_target is not None:
                    _error(errors, package_target == server_script_target.parent / "package.json", f"{label}: package manifest is not beside server script")
                    _error(errors, lock_target == server_script_target.parent / "package-lock.json", f"{label}: package lock is not beside server script")
                _validate_dependency_manifest(
                    artifacts.get("runtime_dependency_manifest_path"),
                    pin_data,
                    errors,
                    label,
                    repo=artifact_repo,
                    expected_root=server_script_target.parent / "node_modules" if server_script_target is not None else None,
                )
                _validate_server_attestation(
                    artifacts.get("embedding_server_attestation_path"),
                    requested if isinstance(requested, dict) else {},
                    pin_data,
                    errors,
                    label,
                    repo=artifact_repo,
                    relocation=relocation,
                    expected_model_path=model_target,
                )
                server_command = requested.get("embedding_server_command") if isinstance(requested, dict) else None
                _error(errors, isinstance(server_command, list) and len(server_command) == 2 and all(isinstance(part, str) and part for part in server_command), f"{label}: controlled server command is invalid")
                if (
                    isinstance(server_command, list)
                    and len(server_command) == 2
                    and all(isinstance(part, str) and part for part in server_command)
                ):
                    if relocation is None:
                        _error(errors, pathlib.Path(server_command[0]).resolve() == node_target, f"{label}: server command did not use retained Node")
                        _error(errors, pathlib.Path(server_command[1]).resolve() == server_script_target, f"{label}: server command did not use retained script")
                    else:
                        _map_recorded_artifact_path(server_command[0], relocation, errors, f"{label}: server command Node", expected=node_target)
                        _map_recorded_artifact_path(server_command[1], relocation, errors, f"{label}: server command script", expected=server_script_target)
                server_environment = requested.get("embedding_server_environment") if isinstance(requested, dict) else None
                _error(errors, isinstance(server_environment, dict), f"{label}: controlled server environment is invalid")
                if isinstance(server_environment, dict):
                    _error(errors, server_environment.get("HOST") == "127.0.0.1", f"{label}: controlled server host is not loopback-pinned")
                    _error(errors, server_environment.get("PORT") == "11500", f"{label}: controlled server port differs from matrix")
                    gguf = server_environment.get("GGUF")
                    if relocation is None:
                        _error(errors, isinstance(gguf, str) and pathlib.Path(gguf).resolve() == model_target, f"{label}: controlled server did not use retained GGUF")
                    else:
                        _map_recorded_artifact_path(gguf, relocation, errors, f"{label}: controlled server GGUF", expected=model_target)
                        for key, value in server_environment.items():
                            if (
                                isinstance(value, str)
                                and pathlib.Path(value).is_absolute()
                                and key not in {"GGUF", "HOME", "PATH", "TMPDIR"}
                            ):
                                errors.append(f"{label}: server environment contains unmapped absolute field {key}")
            else:
                _error(errors, model_path is None and model_hash is None, f"{label}: non-EmbeddingGemma model artifact must be null")
                for stem in server_stems:
                    _error(
                        errors,
                        artifacts.get(f"{stem}_path") is None and artifacts.get(f"{stem}_sha256") is None,
                        f"{label}: non-EmbeddingGemma {stem} artifact must be null",
                    )
                if isinstance(requested, dict):
                    _error(errors, "embedding_server_command" not in requested, f"{label}: non-EmbeddingGemma server command is prohibited")
                    _error(errors, "embedding_server_environment" not in requested, f"{label}: non-EmbeddingGemma server environment is prohibited")

        if corpus_ok:
            facts_hash = corpus.get("facts_sha256")
            prefilter_count = corpus.get("prefilter_count")
            eligible_count = corpus.get("eligible_count")
            delivered_count = corpus.get("delivered_count")
            _error(errors, facts_hash == corpus_pin.get("facts_sha256"), f"{label}: corpus facts hash differs from pin")
            _error(errors, prefilter_count == corpus_pin.get("prefilter_count"), f"{label}: prefilter count differs from pin")
            _error(errors, eligible_count == corpus_pin.get("eligible_count"), f"{label}: eligible count differs from pin")
            _error(errors, _is_nonnegative_int(delivered_count), f"{label}: delivered_count is invalid")
            excluded = corpus.get("excluded_by_reason")
            _error(errors, isinstance(excluded, dict), f"{label}: excluded_by_reason must be an object")
            if isinstance(excluded, dict) and _is_nonnegative_int(prefilter_count) and _is_nonnegative_int(eligible_count):
                _error(errors, all(_is_nonnegative_int(value) for value in excluded.values()), f"{label}: excluded count is invalid")
                _error(errors, sum(excluded.values()) == prefilter_count - eligible_count, f"{label}: excluded counts do not reconcile")
                if derived_eligibility is not None:
                    _error(
                        errors,
                        excluded == derived_eligibility["excluded_counts"],
                        f"{label}: excluded counts differ from fact-level temporal eligibility",
                    )
            if _is_nonnegative_int(eligible_count) and _is_nonnegative_int(delivered_count):
                _error(errors, delivered_count <= eligible_count, f"{label}: delivered_count exceeds eligible_count")
            corpus_signatures.append((facts_hash, prefilter_count, eligible_count))

        if result_ok:
            query_id = result.get("query_id")
            query_ids.append(query_id)
            _error(errors, query_id in pinned_queries, f"{label}: query_id is not in the canonical development pin set")
            fact_ids = result.get("fact_ids_in_order")
            _error(errors, isinstance(fact_ids, list) and all(isinstance(fact_id, str) for fact_id in fact_ids), f"{label}: fact_ids_in_order is invalid")
            if isinstance(fact_ids, list) and all(isinstance(fact_id, str) for fact_id in fact_ids):
                _error(errors, _unique_strings(fact_ids), f"{label}: ranked fact ids are not unique")
                _error(
                    errors,
                    _is_nonnegative_int(development_k) and len(fact_ids) <= development_k,
                    f"{label}: ranked fact count exceeds pinned development task k",
                )
                if active_eligible_ids is not None:
                    _error(
                        errors,
                        set(fact_ids).issubset(active_eligible_ids),
                        f"{label}: delivered fact ids are inactive or temporally ineligible",
                    )
                if isinstance(corpus, dict):
                    _error(
                        errors,
                        corpus.get("delivered_count") == len(fact_ids),
                        f"{label}: delivered_count does not equal ranked fact count",
                    )
            _error(errors, result.get("output_valid") is True, f"{label}: output_valid must be true")

    _error(errors, _all_equal(pin_descriptors), "engine verification pin descriptors differ")
    _error(errors, _unique_strings(namespaces), "engine verification namespaces are not unique strings")
    _error(errors, _unique_strings(semantic_vector_paths), "semantic engine vector artifact paths are not unique strings")
    _error(errors, _all_equal(corpus_signatures), "engine verification corpus hash/prefilter/eligible counts differ")
    _error(errors, _all_equal(query_ids), "engine verification query ids differ")
    _error(errors, _all_equal(facts_source_paths), "engine verification retained facts sources differ")
    _error(errors, _all_equal(session_source_paths), "engine verification retained session-date sources differ")
    _error(errors, _all_equal(binary_paths), "engine verification retained binary paths differ")
    _error(errors, _all_equal(binary_hashes), "engine verification retained binary hashes differ")
    _error(errors, _all_equal(binary_attestation_paths), "engine verification binary attestations differ")
    _error(errors, _unique_strings(derived_facts_paths), "engine verification derived facts paths are not unique")
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
    _error(errors, protocol.get("schema_version") == 2, "preregistration schema_version must be 2")
    errors.extend(validate_joint_success_contract(protocol, freeze=freeze))
    errors.extend(validate_engine_pin_binding(protocol))
    pins = _load_artifact(ENGINE_PINS, errors, "engine-verification-pins.json")
    if pins is not None:
        _validate_engine_pins(pins, errors, repo=REPO, require_production=True)

    for schema_path in sorted((HERE / "schemas").glob("*.json")):
        schema = load(schema_path)
        _error(errors, schema.get("$schema") == "https://json-schema.org/draft/2020-12/schema", f"{schema_path.name}: wrong JSON Schema dialect")

    arms = matrix.get("arms", [])
    _error(
        errors,
        matrix.get("verification_schema") == "schemas/engine-verification-public-v4.schema.json",
        "authoritative engine verification schema must be public v4",
    )
    _error(
        errors,
        matrix.get("diagnostic_verification_schema") == "schemas/engine-verification.schema.json",
        "legacy engine diagnostic schema binding changed",
    )
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
    errors.extend(validate_relevance_bundle(protocol))
    errors.extend(validate_dataset(dataset, protocol, freeze))
    errors.extend(validate_integration_verification(protocol, checks))
    errors.extend(validate_analyzer_lock(protocol, checks.get("analyzer_hash_frozen", {})))
    errors.extend(validate_power_analysis(protocol, checks.get("power_target_met", {})))
    errors.extend(
        validate_engine_verification(
            matrix,
            checks.get("all_engines_machine_verified", {}),
            require_storage_contract=True,
        )
    )
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
