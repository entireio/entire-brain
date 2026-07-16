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
import pathlib
import re
import sys
from typing import Any


SCHEMA_VERSION = 1
PURPOSE = "development_retrieval_evaluation_only"
EXCLUDED_PATH_COMPONENT = "holdout"
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
SHA256_RE = re.compile(r"[0-9a-f]{64}")
SAFE_PATH_RE = re.compile(r"[A-Za-z0-9._/-]+")
RFC3339_RE = re.compile(
    r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})"
)


class FixtureError(ValueError):
    """The development query fixture or one of its bindings failed closed."""


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


def _reject_constant(value: str) -> None:
    raise FixtureError(f"non-finite JSON number {value!r} is prohibited")


def _unique_object(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise FixtureError(f"duplicate JSON object key {key!r}")
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
        missing = sorted(expected - actual)
        extra = sorted(actual - expected)
        raise FixtureError(f"{label} fields differ (missing={missing}, extra={extra})")


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


def _validate_shape(fixture: Any) -> dict[str, pathlib.PurePosixPath]:
    root = _require_object(fixture, "fixture")
    _require_exact_fields(root, TOP_LEVEL_FIELDS, "fixture")
    if root["schema_version"] != SCHEMA_VERSION or isinstance(root["schema_version"], bool):
        raise FixtureError(f"fixture schema_version must be {SCHEMA_VERSION}")
    _require_nonempty_string(root["fixture_id"], "fixture.fixture_id")
    if root["purpose"] != PURPOSE:
        raise FixtureError(f"fixture purpose must be {PURPOSE!r}")
    _require_timestamp(root["created_at"], "fixture.created_at")
    _require_sha256(root["fixture_sha256"], "fixture.fixture_sha256")

    bindings = _require_object(root["source_bindings"], "fixture.source_bindings")
    _require_exact_fields(bindings, SOURCE_BINDING_FIELDS, "fixture.source_bindings")
    _require_nonempty_string(bindings["label_set_id"], "fixture.source_bindings.label_set_id")

    # Complete this lexical pass for every source before any source is resolved, statted, or read.
    paths: dict[str, pathlib.PurePosixPath] = {}
    for role in SOURCE_ROLES:
        record = _require_object(bindings[role], f"fixture.source_bindings.{role}")
        _require_exact_fields(record, SOURCE_RECORD_FIELDS, f"fixture.source_bindings.{role}")
        paths[role] = _safe_source_path(record["path"], f"fixture.source_bindings.{role}.path")
        _require_sha256(record["sha256"], f"fixture.source_bindings.{role}.sha256")
    if len(set(paths.values())) != len(paths):
        raise FixtureError("source binding paths must be distinct")

    roots = _require_object(bindings["snapshot_roots"], "fixture.source_bindings.snapshot_roots")
    _require_exact_fields(roots, SNAPSHOT_ROOT_FIELDS, "fixture.source_bindings.snapshot_roots")
    for field in SNAPSHOT_ROOT_FIELDS:
        if field in {"source_active_fact_count", "fact_count", "session_date_count"}:
            _require_positive_int(roots[field], f"fixture.source_bindings.snapshot_roots.{field}")
        else:
            _require_sha256(roots[field], f"fixture.source_bindings.snapshot_roots.{field}")

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


def _resolve_bound_sources(
    fixture: dict[str, Any], repo_root: pathlib.Path, relative_paths: dict[str, pathlib.PurePosixPath]
) -> dict[str, dict[str, Any]]:
    try:
        resolved_root = repo_root.resolve(strict=True)
    except OSError as exc:
        raise FixtureError(f"cannot resolve repository root {repo_root}: {exc}") from exc
    if not resolved_root.is_dir():
        raise FixtureError(f"repository root is not a directory: {repo_root}")

    resolved: dict[str, pathlib.Path] = {}
    for role, relative_path in relative_paths.items():
        unresolved = resolved_root.joinpath(*relative_path.parts)
        try:
            candidate = unresolved.resolve(strict=True)
            candidate.relative_to(resolved_root)
        except (OSError, ValueError) as exc:
            raise FixtureError(f"source binding {role} does not resolve inside the repository") from exc
        if not candidate.is_file():
            raise FixtureError(f"source binding {role} is not a regular file")
        resolved[role] = candidate

    sources: dict[str, dict[str, Any]] = {}
    bindings = fixture["source_bindings"]
    for role in SOURCE_ROLES:
        path = resolved[role]
        try:
            raw = path.read_bytes()
        except OSError as exc:
            raise FixtureError(f"cannot read source binding {role}: {exc}") from exc
        expected = bindings[role]["sha256"]
        actual = sha256_bytes(raw)
        if actual != expected:
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
            raise FixtureError(f"{label} contains duplicate {key} {identifier!r}")
        indexed[identifier] = record
    return indexed


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
    for query_id, label in label_items.items():
        review = review_items[query_id]
        for field in ("task_id", "query_source"):
            if review.get(field) != label.get(field):
                raise FixtureError(f"review ledger {query_id!r} {field} differs from labels")

    null_items = _index_records(null_review_ledger.get("items"), "query_id", "null_review_ledger.items")
    fixture_null = {item["query_id"]: item for item in fixture_items if item["null_query"]}
    if set(null_items) != set(fixture_null):
        raise FixtureError("null-review ledger query IDs do not exactly cover fixture null queries")
    for query_id, item in fixture_null.items():
        ledger = null_items[query_id]
        for field in ("task_id", "query_sha256", "temporal_policy_sha256"):
            if ledger.get(field) != item[field]:
                raise FixtureError(f"null-review commitment {query_id!r} {field} mismatch")


def verify_fixture(fixture: dict[str, Any], repo_root: pathlib.Path) -> dict[str, Any]:
    """Verify an already-decoded fixture and return it only after every binding passes."""
    relative_paths = _validate_shape(fixture)
    expected_fixture_hash = fixture_sha256(fixture)
    if fixture["fixture_sha256"] != expected_fixture_hash:
        raise FixtureError("fixture self-hash mismatch")

    sources = _resolve_bound_sources(fixture, repo_root, relative_paths)
    labels = sources["labels"]
    if labels.get("label_set_id") != fixture["source_bindings"]["label_set_id"]:
        raise FixtureError("labels label_set_id differs from source binding")
    _validate_snapshot_roots(fixture, labels, sources["fact_snapshot"])

    label_items = _index_records(labels.get("items"), "query_id", "labels.items")
    fixture_items = fixture["items"]
    _validate_coverage(fixture_items, label_items)
    _validate_review_bindings(
        fixture_items, label_items, sources["review_ledger"], sources["null_review_ledger"]
    )

    inventory = _index_records(sources["task_inventory"].get("tasks"), "task_id", "task_inventory.tasks")
    for item in fixture_items:
        query_id = item["query_id"]
        label = label_items[query_id]
        for field in ("task_id", "query_source", "null_query"):
            if item[field] != label.get(field):
                raise FixtureError(f"fixture item {query_id!r} {field} differs from labels")

        actual_query_hash = sha256_bytes(item["query_text"].encode("utf-8"))
        if item["query_sha256"] != actual_query_hash:
            raise FixtureError(f"fixture item {query_id!r} query text/hash mismatch")
        expected_policy_hash = temporal_policy_sha256(item["temporal_cutoff"], item["exclude_session_ids"])
        if item["temporal_policy_sha256"] != expected_policy_hash:
            raise FixtureError(f"fixture item {query_id!r} temporal policy hash mismatch")

        task = inventory.get(item["task_id"])
        if task is None:
            raise FixtureError(f"fixture item {query_id!r} task is missing from inventory")
        if task.get("assigned_split") != "development":
            raise FixtureError(f"fixture item {query_id!r} is not assigned to development")
        artifacts = _require_object(task.get("artifacts"), f"inventory task {item['task_id']!r}.artifacts")
        if item["source_config_sha256"] != artifacts.get("config_sha256"):
            raise FixtureError(f"fixture item {query_id!r} source config hash mismatch")
        if item["task_prompt_sha256"] != artifacts.get("prompt_sha256"):
            raise FixtureError(f"fixture item {query_id!r} task prompt hash mismatch")

        if item["query_source"] == "user_prompt_derived":
            if item["query_sha256"] != item["task_prompt_sha256"]:
                raise FixtureError(f"product query {query_id!r} is not bound to the inventory prompt hash")
        else:
            oracle_text = label.get("query_text")
            if not isinstance(oracle_text, str) or not oracle_text:
                raise FixtureError(f"oracle label {query_id!r} does not contain reviewed query text")
            if item["query_text"] != oracle_text or item["query_sha256"] != sha256_bytes(oracle_text.encode("utf-8")):
                raise FixtureError(f"oracle query {query_id!r} differs from the reviewed label")

    # These artifacts are deliberately source-bound even though query semantics do not depend on a
    # particular engine implementation.  Requiring JSON objects prevents a hash-bound opaque blob.
    _require_object(sources["engine_pins"], "engine_pins")
    return fixture


def load_verified_fixture(fixture_path: pathlib.Path, repo_root: pathlib.Path) -> dict[str, Any]:
    """Read one fixture, then verify all source bindings without reading any task configs."""
    try:
        raw = fixture_path.read_bytes()
    except OSError as exc:
        raise FixtureError(f"cannot read fixture {fixture_path}: {exc}") from exc
    fixture = _require_object(decode_json(raw, "fixture"), "fixture")
    return verify_fixture(fixture, repo_root)


def safe_summary(fixture: dict[str, Any]) -> dict[str, Any]:
    """Return metadata safe for logs; query text and task/session identifiers are omitted."""
    return {
        "status": "verified",
        "fixture_id": fixture["fixture_id"],
        "fixture_sha256": fixture["fixture_sha256"],
        "purpose": fixture["purpose"],
        "coverage": fixture["coverage"],
    }


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("fixture", type=pathlib.Path, help="query fixture to verify")
    parser.add_argument("--repo-root", type=pathlib.Path, default=pathlib.Path.cwd())
    args = parser.parse_args(argv)
    try:
        fixture = load_verified_fixture(args.fixture, args.repo_root)
    except FixtureError as exc:
        print(f"ERROR: {exc}", file=sys.stderr)
        return 1
    print(json.dumps(safe_summary(fixture), sort_keys=True, separators=(",", ":")))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
