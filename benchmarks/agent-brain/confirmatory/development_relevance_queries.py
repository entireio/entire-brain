#!/usr/bin/env python3
"""Load and fail-closed verify the development relevance query fixture.

The real fixture is deliberately not generated here.  This module only authenticates an explicitly
supplied fixture against already-reviewed development metadata.  It never opens task config paths;
their inventory hashes are the binding surface.
"""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import os
import pathlib
import re
import stat
import sys
from typing import Any, NamedTuple


SCHEMA_VERSION = 1
FIXTURE_ID = "entire-brain-development-relevance-queries-v1"
PURPOSE = "development_retrieval_evaluation_only"
EXCLUDED_PATH_COMPONENT = "holdout"
FIXTURE_RELATIVE_PATH = pathlib.PurePosixPath(
    "benchmarks/agent-brain/confirmatory/development-relevance-queries-v1.json"
)
TEMPORAL_RECEIPT_RELATIVE_PATH = pathlib.PurePosixPath(
    "benchmarks/agent-brain/confirmatory/development-relevance-temporal-policy-receipt-v1.json"
)
TEMPORAL_RECEIPT_ID = "entire-brain-development-relevance-temporal-policy-v1"
TEMPORAL_RECEIPT_PURPOSE = "authorized_development_temporal_policy_commitment_only"
# This stays unset until an authorized exporter creates the receipt and a reviewer pins its raw
# bytes in code.  Production verification therefore fails before opening the plaintext fixture.
DEFAULT_TEMPORAL_RECEIPT_SHA256: str | None = None
SOURCE_ROLES = (
    "labels",
    "review_ledger",
    "null_review_ledger",
    "fact_snapshot",
    "engine_pins",
    "task_inventory",
)
EXPECTED_COVERAGE = {
    "query_count": 14,
    "unique_task_count": 13,
    "answerable_product_query_count": 12,
    "product_null_query_count": 1,
    "oracle_query_count": 1,
}
TOP_LEVEL_FIELDS = {
    "schema_version",
    "fixture_id",
    "purpose",
    "created_at",
    "source_bindings",
    "coverage",
    "items",
    "fixture_sha256",
}
SOURCE_BINDING_FIELDS = set(SOURCE_ROLES) | {"label_set_id", "snapshot_roots"}
SOURCE_RECORD_FIELDS = {"path", "sha256"}
SNAPSHOT_ROOT_FIELDS = {
    "source_facts_sha256",
    "source_session_dates_sha256",
    "source_active_fact_count",
    "source_active_fact_catalog_sha256",
    "fact_count",
    "facts_sha256",
    "session_date_count",
    "session_dates_sha256",
}
ITEM_FIELDS = {
    "query_id",
    "task_id",
    "query_source",
    "null_query",
    "query_text",
    "query_sha256",
    "task_prompt_sha256",
    "source_config_sha256",
    "temporal_cutoff",
    "exclude_session_ids",
    "temporal_policy_sha256",
}
TEMPORAL_RECEIPT_FIELDS = {
    "schema_version",
    "receipt_id",
    "purpose",
    "created_at",
    "label_set_id",
    "query_count",
    "items",
    "receipt_sha256",
}
TEMPORAL_RECEIPT_ITEM_FIELDS = {"query_id", "task_id", "temporal_policy_sha256"}
SHA256_RE = re.compile(r"[0-9a-f]{64}")
SAFE_PATH_RE = re.compile(r"[A-Za-z0-9._/-]+")
RFC3339_RE = re.compile(
    r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})"
)


class FixtureError(ValueError):
    """The development query fixture or one of its bindings failed closed."""


class PreparedFile(NamedTuple):
    root: pathlib.Path
    relative_path: pathlib.PurePosixPath
    device: int
    inode: int
    private: bool


def default_trusted_source_bindings() -> dict[str, Any]:
    """Return the machine trust root; fixture bytes never select these values."""
    base = "benchmarks/agent-brain/confirmatory/"
    return {
        "labels": {
            "path": base + "offline-relevance-development-labels.json",
            "sha256": "1702cd625e71a9f5ad69ef41b5f5f1ef314bfa6df160d9aa002380f7a4ee77fe",
        },
        "review_ledger": {
            "path": base + "offline-relevance-review-ledger.json",
            "sha256": "67124c453fdfef5b8fe2602c6095e45078b093acb1b8a48dfaa73b0adb0c1f55",
        },
        "null_review_ledger": {
            "path": base + "offline-relevance-null-review-ledger.json",
            "sha256": "d9e7d71334a88a60cdbf4f71c8808c5383d616f54bc7fcda59ef7460e5d01e0b",
        },
        "fact_snapshot": {
            "path": base + "offline-relevance-fact-snapshot.json",
            "sha256": "a4c2061d2eaf8acde60594e7db7339c6ea6fa81304925f6d257341580eb07d79",
        },
        "engine_pins": {
            "path": base + "engine-verification-pins.json",
            "sha256": "c4e4989ffe22cefa2e71211ba2baa4027634daf07d19ff5e92a8b1289243db11",
        },
        "task_inventory": {
            "path": base + "task-inventory.json",
            "sha256": "1f16cb079f5aa7a5fc023011a1f3c717bc060cf86a59fe385a27aa07ba28b5d5",
        },
        "label_set_id": "exposed-c0701-manual-review-2026-07-15-v3",
        "snapshot_roots": {
            "source_facts_sha256": "084f5170c07a8b843e6ffc7eac1939df0fa9c13d45ade9f5061d7c185195a793",
            "source_session_dates_sha256": "0ccd0a2ec938b354fa512fea090f2f68ca0e7b958f17ffa50784fb13108ed18a",
            "source_active_fact_count": 2531,
            "source_active_fact_catalog_sha256": (
                "22ded28af2c42f9f49bcb518a21b41381f247e7d0c3d35896b914230e1c6023c"
            ),
            "fact_count": 44,
            "facts_sha256": "4f2f5269770444a912d53b759f86ae43bc17b6e4a16096eeb115206fae748410",
            "session_date_count": 39,
            "session_dates_sha256": "5293a15ee69018a5c1ba639f30cb2f7c67e325dca4c674ea54b7eee17f74456a",
        },
    }


def sha256_bytes(value: bytes) -> str:
    return hashlib.sha256(value).hexdigest()


def canonical_json(value: Any) -> bytes:
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8")


def temporal_policy_sha256(temporal_cutoff: str, exclude_session_ids: list[str]) -> str:
    return sha256_bytes(canonical_json({
        "temporal_cutoff": temporal_cutoff,
        "exclude_session_ids": exclude_session_ids,
    }))


def fixture_sha256(fixture: dict[str, Any]) -> str:
    payload = dict(fixture)
    payload.pop("fixture_sha256", None)
    return sha256_bytes(canonical_json(payload))


def temporal_receipt_sha256(receipt: dict[str, Any]) -> str:
    payload = dict(receipt)
    payload.pop("receipt_sha256", None)
    return sha256_bytes(canonical_json(payload))


def _reject_constant(value: str) -> None:
    raise FixtureError(f"non-finite JSON number {value!r} is prohibited")


def _unique_object(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise FixtureError("duplicate JSON object key")
        result[key] = value
    return result


def decode_json(raw: bytes, label: str) -> Any:
    try:
        text = raw.decode("utf-8")
        return json.loads(text, object_pairs_hook=_unique_object, parse_constant=_reject_constant)
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise FixtureError(f"{label} is not valid UTF-8 JSON: {exc}") from exc


def _require_object(value: Any, label: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise FixtureError(f"{label} must be an object")
    return value


def _require_exact_fields(value: dict[str, Any], expected: set[str], label: str) -> None:
    actual = set(value)
    if actual != expected:
        raise FixtureError(
            f"{label} fields differ (missing_count={len(expected - actual)}, "
            f"extra_count={len(actual - expected)})"
        )


def _require_nonempty_string(value: Any, label: str) -> str:
    if not isinstance(value, str) or not value:
        raise FixtureError(f"{label} must be a non-empty string")
    return value


def _require_sha256(value: Any, label: str) -> str:
    if not isinstance(value, str) or SHA256_RE.fullmatch(value) is None:
        raise FixtureError(f"{label} must be a lowercase SHA-256")
    return value


def _require_positive_int(value: Any, label: str) -> int:
    if not isinstance(value, int) or isinstance(value, bool) or value < 1:
        raise FixtureError(f"{label} must be a positive integer")
    return value


def _require_timestamp(value: Any, label: str) -> str:
    text = _require_nonempty_string(value, label)
    if RFC3339_RE.fullmatch(text) is None:
        raise FixtureError(f"{label} must be an RFC3339 timestamp")
    parsed_text = text[:-1] + "+00:00" if text.endswith(("Z", "z")) else text
    try:
        parsed = dt.datetime.fromisoformat(parsed_text)
    except ValueError as exc:
        raise FixtureError(f"{label} must be an RFC3339 timestamp") from exc
    if parsed.tzinfo is None or parsed.utcoffset() is None:
        raise FixtureError(f"{label} must include a timezone")
    return text


def _safe_source_path(value: Any, label: str) -> pathlib.PurePosixPath:
    """Validate a bound source path lexically, before any source filesystem access."""
    text = _require_nonempty_string(value, label)
    if SAFE_PATH_RE.fullmatch(text) is None or "\x00" in text or "\\" in text:
        raise FixtureError(f"{label} must be a canonical repo-relative POSIX path")
    path = pathlib.PurePosixPath(text)
    if path.is_absolute() or any(part == ".." for part in path.parts):
        raise FixtureError(f"{label} must not be absolute or contain '..'")
    if any(part.casefold() == EXCLUDED_PATH_COMPONENT.casefold() for part in path.parts):
        raise FixtureError(f"{label} contains excluded path component {EXCLUDED_PATH_COMPONENT!r}")
    if str(path) != text or text in ("", "."):
        raise FixtureError(f"{label} must be a canonical repo-relative POSIX path")
    return path


def _validate_source_bindings(bindings_value: Any, label: str) -> dict[str, pathlib.PurePosixPath]:
    bindings = _require_object(bindings_value, label)
    _require_exact_fields(bindings, SOURCE_BINDING_FIELDS, label)
    _require_nonempty_string(bindings["label_set_id"], f"{label}.label_set_id")

    paths: dict[str, pathlib.PurePosixPath] = {}
    for role in SOURCE_ROLES:
        record = _require_object(bindings[role], f"{label}.{role}")
        _require_exact_fields(record, SOURCE_RECORD_FIELDS, f"{label}.{role}")
        paths[role] = _safe_source_path(record["path"], f"{label}.{role}.path")
        _require_sha256(record["sha256"], f"{label}.{role}.sha256")
    if len(set(paths.values())) != len(paths):
        raise FixtureError(f"{label} paths must be distinct")

    roots = _require_object(bindings["snapshot_roots"], f"{label}.snapshot_roots")
    _require_exact_fields(roots, SNAPSHOT_ROOT_FIELDS, f"{label}.snapshot_roots")
    for field in SNAPSHOT_ROOT_FIELDS:
        if field in {"source_active_fact_count", "fact_count", "session_date_count"}:
            _require_positive_int(roots[field], f"{label}.snapshot_roots.{field}")
        else:
            _require_sha256(roots[field], f"{label}.snapshot_roots.{field}")
    return paths


def _validate_shape(fixture: Any) -> dict[str, pathlib.PurePosixPath]:
    root = _require_object(fixture, "fixture")
    _require_exact_fields(root, TOP_LEVEL_FIELDS, "fixture")
    if root["schema_version"] != SCHEMA_VERSION or isinstance(root["schema_version"], bool):
        raise FixtureError(f"fixture schema_version must be {SCHEMA_VERSION}")
    if root["fixture_id"] != FIXTURE_ID:
        raise FixtureError("fixture.fixture_id differs from the pinned identifier")
    if root["purpose"] != PURPOSE:
        raise FixtureError(f"fixture purpose must be {PURPOSE!r}")
    _require_timestamp(root["created_at"], "fixture.created_at")
    _require_sha256(root["fixture_sha256"], "fixture.fixture_sha256")

    # Complete this lexical pass for every source before any source is resolved, statted, or read.
    paths = _validate_source_bindings(root["source_bindings"], "fixture.source_bindings")

    coverage = _require_object(root["coverage"], "fixture.coverage")
    _require_exact_fields(coverage, set(EXPECTED_COVERAGE), "fixture.coverage")
    for field, value in coverage.items():
        _require_positive_int(value, f"fixture.coverage.{field}")
    if coverage != EXPECTED_COVERAGE:
        raise FixtureError(f"fixture coverage must equal {EXPECTED_COVERAGE}")

    items = root["items"]
    if not isinstance(items, list) or len(items) != EXPECTED_COVERAGE["query_count"]:
        raise FixtureError("fixture.items must contain exactly 14 records")
    for index, raw_item in enumerate(items):
        item = _require_object(raw_item, f"fixture.items[{index}]")
        _require_exact_fields(item, ITEM_FIELDS, f"fixture.items[{index}]")
        for field in ("query_id", "task_id", "query_text"):
            _require_nonempty_string(item[field], f"fixture.items[{index}].{field}")
        if item["query_source"] not in ("user_prompt_derived", "oracle_upper_bound"):
            raise FixtureError(f"fixture.items[{index}].query_source is invalid")
        if not isinstance(item["null_query"], bool):
            raise FixtureError(f"fixture.items[{index}].null_query must be boolean")
        for field in (
            "query_sha256", "task_prompt_sha256", "source_config_sha256", "temporal_policy_sha256"
        ):
            _require_sha256(item[field], f"fixture.items[{index}].{field}")
        _require_timestamp(item["temporal_cutoff"], f"fixture.items[{index}].temporal_cutoff")
        exclusions = item["exclude_session_ids"]
        if not isinstance(exclusions, list) or not all(isinstance(value, str) and value for value in exclusions):
            raise FixtureError(f"fixture.items[{index}].exclude_session_ids must be non-empty strings")
        if len(exclusions) != len(set(exclusions)):
            raise FixtureError(f"fixture.items[{index}].exclude_session_ids must be unique")
    return paths


def _prepare_repo_file(
    repo_root: pathlib.Path,
    relative_path: pathlib.PurePosixPath,
    label: str,
    *,
    private: bool,
) -> PreparedFile:
    """Reject symlinks component-by-component, then recheck the resolved repo-relative path."""
    root = pathlib.Path(os.path.abspath(repo_root))
    try:
        root_stat = root.lstat()
    except OSError as exc:
        raise FixtureError("repository root metadata check failed") from exc
    if stat.S_ISLNK(root_stat.st_mode) or not stat.S_ISDIR(root_stat.st_mode):
        raise FixtureError("repository root must be a non-symlink directory")

    current = root
    final_stat: os.stat_result | None = None
    for index, part in enumerate(relative_path.parts):
        current = current / part
        try:
            current_stat = current.lstat()
        except OSError as exc:
            raise FixtureError(f"{label} metadata check failed") from exc
        if stat.S_ISLNK(current_stat.st_mode):
            raise FixtureError(f"{label} path must not contain symlinks")
        if index < len(relative_path.parts) - 1 and not stat.S_ISDIR(current_stat.st_mode):
            raise FixtureError(f"{label} parent component is not a directory")
        final_stat = current_stat
    if final_stat is None or not stat.S_ISREG(final_stat.st_mode):
        raise FixtureError(f"{label} must be a regular file")

    try:
        resolved_root = root.resolve(strict=True)
        resolved_file = current.resolve(strict=True)
        resolved_relative = resolved_file.relative_to(resolved_root)
    except (OSError, ValueError) as exc:
        raise FixtureError(f"{label} resolved path is outside the repository") from exc
    if any(part.casefold() == EXCLUDED_PATH_COMPONENT.casefold() for part in resolved_relative.parts):
        raise FixtureError(f"{label} resolved path contains the excluded component")
    if resolved_relative.as_posix() != relative_path.as_posix():
        raise FixtureError(f"{label} resolved path is not canonical")

    if private:
        if final_stat.st_uid != os.getuid():
            raise FixtureError(f"{label} must be owned by the current user")
        if stat.S_IMODE(final_stat.st_mode) != 0o600:
            raise FixtureError(f"{label} mode must be 0600")
    return PreparedFile(root, relative_path, final_stat.st_dev, final_stat.st_ino, private)


def _read_prepared_file(prepared: PreparedFile, label: str) -> bytes:
    common_flags = getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)
    directory_flags = os.O_RDONLY | common_flags | getattr(os, "O_DIRECTORY", 0)
    open_directories: list[int] = []
    descriptor = -1
    try:
        parent_descriptor = os.open(prepared.root, directory_flags)
        open_directories.append(parent_descriptor)
        for part in prepared.relative_path.parts[:-1]:
            parent_descriptor = os.open(part, directory_flags, dir_fd=parent_descriptor)
            open_directories.append(parent_descriptor)
        descriptor = os.open(
            prepared.relative_path.parts[-1],
            os.O_RDONLY | common_flags,
            dir_fd=parent_descriptor,
        )
    except OSError as exc:
        for directory_descriptor in reversed(open_directories):
            os.close(directory_descriptor)
        raise FixtureError(f"{label} secure open failed") from exc
    try:
        current_stat = os.fstat(descriptor)
        if (
            current_stat.st_dev != prepared.device
            or current_stat.st_ino != prepared.inode
            or not stat.S_ISREG(current_stat.st_mode)
        ):
            raise FixtureError(f"{label} changed after metadata validation")
        if prepared.private:
            if current_stat.st_uid != os.getuid() or stat.S_IMODE(current_stat.st_mode) != 0o600:
                raise FixtureError(f"{label} private-file metadata changed before read")
        with os.fdopen(descriptor, "rb", closefd=True) as stream:
            descriptor = -1
            return stream.read()
    except OSError as exc:
        raise FixtureError(f"{label} secure read failed") from exc
    finally:
        if descriptor >= 0:
            os.close(descriptor)
        for directory_descriptor in reversed(open_directories):
            os.close(directory_descriptor)


def _resolve_bound_sources(
    fixture: dict[str, Any],
    repo_root: pathlib.Path,
    relative_paths: dict[str, pathlib.PurePosixPath],
) -> dict[str, dict[str, Any]]:
    # Prepare every path first.  No source content is opened until the complete set is symlink- and
    # exclusion-safe, including the post-resolution component check.
    prepared = {
        role: _prepare_repo_file(repo_root, relative_paths[role], f"source binding {role}", private=False)
        for role in SOURCE_ROLES
    }

    sources: dict[str, dict[str, Any]] = {}
    bindings = fixture["source_bindings"]
    for role in SOURCE_ROLES:
        # Recheck components immediately before secure open; an inode check closes the final-file
        # replacement window.
        rechecked = _prepare_repo_file(
            repo_root, relative_paths[role], f"source binding {role}", private=False
        )
        if rechecked.device != prepared[role].device or rechecked.inode != prepared[role].inode:
            raise FixtureError(f"source binding {role} changed after preflight")
        raw = _read_prepared_file(rechecked, f"source binding {role}")
        if sha256_bytes(raw) != bindings[role]["sha256"]:
            raise FixtureError(f"source binding {role} SHA-256 mismatch")
        sources[role] = _require_object(decode_json(raw, f"source binding {role}"), f"source binding {role}")
    return sources


def _index_records(records: Any, key: str, label: str) -> dict[str, dict[str, Any]]:
    if not isinstance(records, list):
        raise FixtureError(f"{label} must be an array")
    indexed: dict[str, dict[str, Any]] = {}
    for index, raw_record in enumerate(records):
        record = _require_object(raw_record, f"{label}[{index}]")
        identifier = _require_nonempty_string(record.get(key), f"{label}[{index}].{key}")
        if identifier in indexed:
            raise FixtureError(f"{label} contains a duplicate {key}")
        indexed[identifier] = record
    return indexed


def _validate_temporal_receipt(receipt_value: Any) -> dict[str, dict[str, Any]]:
    receipt = _require_object(receipt_value, "temporal receipt")
    _require_exact_fields(receipt, TEMPORAL_RECEIPT_FIELDS, "temporal receipt")
    if receipt["schema_version"] != 1 or isinstance(receipt["schema_version"], bool):
        raise FixtureError("temporal receipt schema_version must be 1")
    if receipt["receipt_id"] != TEMPORAL_RECEIPT_ID:
        raise FixtureError("temporal receipt identifier differs from the pinned identifier")
    if receipt["purpose"] != TEMPORAL_RECEIPT_PURPOSE:
        raise FixtureError("temporal receipt purpose differs from the pinned purpose")
    _require_timestamp(receipt["created_at"], "temporal receipt.created_at")
    _require_nonempty_string(receipt["label_set_id"], "temporal receipt.label_set_id")
    if (
        not isinstance(receipt["query_count"], int)
        or isinstance(receipt["query_count"], bool)
        or receipt["query_count"] != EXPECTED_COVERAGE["query_count"]
    ):
        raise FixtureError("temporal receipt query_count must be 14")
    _require_sha256(receipt["receipt_sha256"], "temporal receipt.receipt_sha256")
    if receipt["receipt_sha256"] != temporal_receipt_sha256(receipt):
        raise FixtureError("temporal receipt self-hash mismatch")

    items = receipt["items"]
    if not isinstance(items, list) or len(items) != EXPECTED_COVERAGE["query_count"]:
        raise FixtureError("temporal receipt.items must contain exactly 14 records")
    for index, raw_item in enumerate(items):
        item = _require_object(raw_item, f"temporal receipt.items[{index}]")
        _require_exact_fields(item, TEMPORAL_RECEIPT_ITEM_FIELDS, f"temporal receipt.items[{index}]")
        _require_nonempty_string(item["query_id"], f"temporal receipt.items[{index}].query_id")
        _require_nonempty_string(item["task_id"], f"temporal receipt.items[{index}].task_id")
        _require_sha256(
            item["temporal_policy_sha256"],
            f"temporal receipt.items[{index}].temporal_policy_sha256",
        )
    return _index_records(items, "query_id", "temporal receipt.items")


def _validate_snapshot_roots(fixture: dict[str, Any], labels: dict[str, Any], snapshot: dict[str, Any]) -> None:
    roots = fixture["source_bindings"]["snapshot_roots"]
    for field in SNAPSHOT_ROOT_FIELDS:
        if snapshot.get(field) != roots[field]:
            raise FixtureError(f"fact snapshot {field} differs from bound snapshot root")
    facts = snapshot.get("facts")
    session_dates = snapshot.get("session_dates")
    if not isinstance(facts, list) or len(facts) != roots["fact_count"]:
        raise FixtureError("fact snapshot fact_count is not canonical")
    if sha256_bytes(canonical_json(facts)) != roots["facts_sha256"]:
        raise FixtureError("fact snapshot facts_sha256 is not canonical")
    if not isinstance(session_dates, dict) or len(session_dates) != roots["session_date_count"]:
        raise FixtureError("fact snapshot session_date_count is not canonical")
    if sha256_bytes(canonical_json(session_dates)) != roots["session_dates_sha256"]:
        raise FixtureError("fact snapshot session_dates_sha256 is not canonical")

    source_corpus = _require_object(labels.get("source_corpus"), "labels.source_corpus")
    snapshot_commitment = _require_object(labels.get("snapshot_commitment"), "labels.snapshot_commitment")
    label_root_map = {
        "source_facts_sha256": source_corpus.get("facts_sha256"),
        "source_session_dates_sha256": source_corpus.get("session_dates_sha256"),
        "source_active_fact_count": source_corpus.get("active_fact_count"),
        "source_active_fact_catalog_sha256": source_corpus.get("active_fact_catalog_sha256"),
        "fact_count": snapshot_commitment.get("fact_count"),
        "facts_sha256": snapshot_commitment.get("facts_sha256"),
        "session_date_count": snapshot_commitment.get("session_date_count"),
        "session_dates_sha256": snapshot_commitment.get("session_dates_sha256"),
    }
    if label_root_map != roots:
        raise FixtureError("label source/snapshot commitments differ from bound snapshot roots")


def _validate_coverage(fixture_items: list[dict[str, Any]], label_items: dict[str, dict[str, Any]]) -> None:
    fixture_ids = [item["query_id"] for item in fixture_items]
    if len(fixture_ids) != len(set(fixture_ids)):
        raise FixtureError("fixture contains duplicate query_id values")
    if set(fixture_ids) != set(label_items):
        raise FixtureError("fixture query IDs do not exactly cover the reviewed labels")

    product = [item for item in fixture_items if item["query_source"] == "user_prompt_derived"]
    oracle = [item for item in fixture_items if item["query_source"] == "oracle_upper_bound"]
    product_tasks = [item["task_id"] for item in product]
    derived = {
        "query_count": len(fixture_items),
        "unique_task_count": len({item["task_id"] for item in fixture_items}),
        "answerable_product_query_count": sum(not item["null_query"] for item in product),
        "product_null_query_count": sum(item["null_query"] for item in product),
        "oracle_query_count": len(oracle),
    }
    if derived != EXPECTED_COVERAGE:
        raise FixtureError(f"fixture records derive unexpected coverage {derived}")
    if len(product_tasks) != len(set(product_tasks)):
        raise FixtureError("user-prompt-derived task IDs must be unique")
    if len(oracle) != 1 or oracle[0]["null_query"]:
        raise FixtureError("the oracle query must be the single non-null oracle record")
    if oracle[0]["task_id"] not in set(product_tasks):
        raise FixtureError("the oracle query must share an existing product task")


def _validate_review_bindings(
    fixture_items: list[dict[str, Any]],
    label_items: dict[str, dict[str, Any]],
    review_ledger: dict[str, Any],
    null_review_ledger: dict[str, Any],
) -> None:
    review_items = _index_records(review_ledger.get("items"), "query_id", "review_ledger.items")
    if set(review_items) != set(label_items):
        raise FixtureError("review ledger query IDs do not exactly cover the labels")
    for label_index, (query_id, label) in enumerate(label_items.items()):
        review = review_items[query_id]
        for field in ("task_id", "query_source"):
            if review.get(field) != label.get(field):
                raise FixtureError(
                    f"review ledger item {label_index} metadata differs from labels"
                )

    null_items = _index_records(null_review_ledger.get("items"), "query_id", "null_review_ledger.items")
    fixture_null = {item["query_id"]: item for item in fixture_items if item["null_query"]}
    if set(null_items) != set(fixture_null):
        raise FixtureError("null-review ledger query IDs do not exactly cover fixture null queries")
    for null_index, (query_id, item) in enumerate(fixture_null.items()):
        ledger = null_items[query_id]
        for field in ("task_id", "query_sha256", "temporal_policy_sha256"):
            if ledger.get(field) != item[field]:
                raise FixtureError(f"null-review commitment {null_index} mismatch")


def _verify_fixture_core(
    fixture: dict[str, Any],
    repo_root: pathlib.Path,
    trusted_source_bindings: dict[str, Any],
    temporal_receipt: dict[str, Any],
) -> dict[str, Any]:
    relative_paths = _validate_shape(fixture)
    trusted_paths = _validate_source_bindings(trusted_source_bindings, "trusted source receipt")
    if fixture["source_bindings"] != trusted_source_bindings or relative_paths != trusted_paths:
        raise FixtureError("fixture source bindings differ from the machine trust root")
    if fixture["fixture_sha256"] != fixture_sha256(fixture):
        raise FixtureError("fixture self-hash mismatch")
    temporal_items = _validate_temporal_receipt(temporal_receipt)
    if temporal_receipt["label_set_id"] != trusted_source_bindings["label_set_id"]:
        raise FixtureError("temporal receipt label set differs from the machine trust root")

    sources = _resolve_bound_sources(fixture, repo_root, relative_paths)
    labels = sources["labels"]
    if labels.get("label_set_id") != trusted_source_bindings["label_set_id"]:
        raise FixtureError("labels label_set_id differs from the machine trust root")
    _validate_snapshot_roots(fixture, labels, sources["fact_snapshot"])

    label_items = _index_records(labels.get("items"), "query_id", "labels.items")
    fixture_items = fixture["items"]
    _validate_coverage(fixture_items, label_items)
    if set(temporal_items) != set(label_items):
        raise FixtureError("temporal receipt query IDs do not exactly cover the trusted labels")
    _validate_review_bindings(
        fixture_items, label_items, sources["review_ledger"], sources["null_review_ledger"]
    )

    inventory = _index_records(sources["task_inventory"].get("tasks"), "task_id", "task_inventory.tasks")
    for item_index, item in enumerate(fixture_items):
        query_id = item["query_id"]
        label = label_items[query_id]
        for field in ("task_id", "query_source", "null_query"):
            if item[field] != label.get(field):
                raise FixtureError(f"fixture.items[{item_index}] metadata differs from labels")

        temporal_commitment = temporal_items[query_id]
        if (
            temporal_commitment["task_id"] != item["task_id"]
            or temporal_commitment["temporal_policy_sha256"] != item["temporal_policy_sha256"]
        ):
            raise FixtureError(
                f"fixture.items[{item_index}] temporal policy differs from the trusted receipt"
            )

        actual_query_hash = sha256_bytes(item["query_text"].encode("utf-8"))
        if item["query_sha256"] != actual_query_hash:
            raise FixtureError(f"fixture.items[{item_index}] query text/hash mismatch")
        expected_policy_hash = temporal_policy_sha256(item["temporal_cutoff"], item["exclude_session_ids"])
        if item["temporal_policy_sha256"] != expected_policy_hash:
            raise FixtureError(f"fixture.items[{item_index}] temporal policy hash mismatch")

        task = inventory.get(item["task_id"])
        if task is None:
            raise FixtureError(f"fixture.items[{item_index}] task is missing from inventory")
        if task.get("assigned_split") != "development":
            raise FixtureError(f"fixture.items[{item_index}] task is not assigned to development")
        artifacts = _require_object(task.get("artifacts"), "task_inventory task artifacts")
        if item["source_config_sha256"] != artifacts.get("config_sha256"):
            raise FixtureError(f"fixture.items[{item_index}] source config hash mismatch")
        if item["task_prompt_sha256"] != artifacts.get("prompt_sha256"):
            raise FixtureError(f"fixture.items[{item_index}] task prompt hash mismatch")

        if item["query_source"] == "user_prompt_derived":
            if item["query_sha256"] != item["task_prompt_sha256"]:
                raise FixtureError(
                    f"fixture.items[{item_index}] product query is not bound to the inventory prompt hash"
                )
        else:
            oracle_text = label.get("query_text")
            if not isinstance(oracle_text, str) or not oracle_text:
                raise FixtureError("trusted oracle label does not contain reviewed query text")
            if item["query_text"] != oracle_text or item["query_sha256"] != sha256_bytes(oracle_text.encode("utf-8")):
                raise FixtureError(f"fixture.items[{item_index}] oracle query differs from the reviewed label")

    # These artifacts are deliberately source-bound even though query semantics do not depend on a
    # particular engine implementation.  Requiring JSON objects prevents a hash-bound opaque blob.
    _require_object(sources["engine_pins"], "engine_pins")
    return fixture


def _verify_fixture_with_trusted_receipts_for_testing(
    fixture: dict[str, Any],
    repo_root: pathlib.Path,
    trusted_source_bindings: dict[str, Any],
    temporal_receipt: dict[str, Any],
) -> dict[str, Any]:
    """Receipt injection surface reserved for synthetic unit tests."""
    return _verify_fixture_core(fixture, repo_root, trusted_source_bindings, temporal_receipt)


def _read_fixed_private_json(
    repo_root: pathlib.Path, relative_path: pathlib.PurePosixPath, label: str
) -> tuple[bytes, dict[str, Any]]:
    prepared = _prepare_repo_file(repo_root, relative_path, label, private=True)
    raw = _read_prepared_file(prepared, label)
    value = _require_object(decode_json(raw, label), label)
    return raw, value


def _load_default_temporal_receipt(repo_root: pathlib.Path) -> dict[str, Any]:
    if DEFAULT_TEMPORAL_RECEIPT_SHA256 is None:
        raise FixtureError("trusted temporal-policy receipt hash is not pinned")
    raw, receipt = _read_fixed_private_json(
        repo_root, TEMPORAL_RECEIPT_RELATIVE_PATH, "temporal receipt"
    )
    if sha256_bytes(raw) != DEFAULT_TEMPORAL_RECEIPT_SHA256:
        raise FixtureError("temporal receipt raw SHA-256 differs from the machine trust root")
    _validate_temporal_receipt(receipt)
    return receipt


def _load_verified_fixture_with_trusted_receipts_for_testing(
    repo_root: pathlib.Path,
    trusted_source_bindings: dict[str, Any],
    temporal_receipt: dict[str, Any],
) -> dict[str, Any]:
    """Fixed-path loader with receipt injection reserved for synthetic unit tests."""
    _, fixture = _read_fixed_private_json(repo_root, FIXTURE_RELATIVE_PATH, "fixture")
    return _verify_fixture_with_trusted_receipts_for_testing(
        fixture, repo_root, trusted_source_bindings, temporal_receipt
    )


def load_verified_fixture(repo_root: pathlib.Path) -> dict[str, Any]:
    """Load only the fixed private fixture under machine-pinned production trust roots."""
    temporal_receipt = _load_default_temporal_receipt(repo_root)
    _, fixture = _read_fixed_private_json(repo_root, FIXTURE_RELATIVE_PATH, "fixture")
    return _verify_fixture_core(
        fixture, repo_root, default_trusted_source_bindings(), temporal_receipt
    )


def safe_summary(fixture: dict[str, Any]) -> dict[str, Any]:
    """Return only an authenticated hash and aggregate counts; all fixture text/IDs stay private."""
    return {
        "status": "verified",
        "fixture_sha256": _require_sha256(fixture.get("fixture_sha256"), "fixture hash"),
        "coverage": dict(EXPECTED_COVERAGE),
    }


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo-root", type=pathlib.Path, default=pathlib.Path.cwd())
    args = parser.parse_args(argv)
    try:
        fixture = load_verified_fixture(args.repo_root)
    except FixtureError as exc:
        print(f"ERROR: {exc}", file=sys.stderr)
        return 1
    print(json.dumps(safe_summary(fixture), sort_keys=True, separators=(",", ":")))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
