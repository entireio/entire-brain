#!/usr/bin/env python3
"""Build and verify the exposed-only offline relevance development dataset.

Labels are human-reviewed source records.  This tool joins them to the reconciled task inventory
and a content-addressed excerpt of the frozen fact corpus, deriving query/fact hashes, kinds, and
rolling-cutoff eligibility.  It never reads a task outside the explicit development assignment and
the allowed exposed inventory states.
"""

from __future__ import annotations

import argparse
import copy
import datetime as dt
import hashlib
import json
import math
import pathlib
import re
import sys
from collections import Counter
from typing import Any


SCHEMA_VERSION = 2
SOURCE_MEMBERSHIP_SCHEMA_VERSION = 2
SOURCE_CONTRACT_SCHEMA_VERSION = 2
SAFE_STATES = ("prompt_inspected", "retrieval_probed", "agent_run", "optimization_used")
GRADES = ("solving", "relevant_alternative", "hard_topical_distractor", "irrelevant")
QUERY_SOURCES = ("user_prompt_derived", "oracle_upper_bound")
EVIDENCE_TYPES = ("legacy_agent_packet", "manual_frozen_corpus_review")
NULL_CLOSURE_METHOD = "exhaustive_active_corpus_review_v2"
REVIEW_METHOD = "manual_judgment_from_exposed_prompt_and_pinned_membership_v1"
STOPWORDS = {
    "a", "an", "and", "are", "as", "at", "be", "by", "for", "from", "has", "in", "is",
    "it", "of", "on", "or", "that", "the", "their", "this", "to", "was", "when", "with",
}


class DatasetError(ValueError):
    """A fail-closed dataset contract violation."""


def sha256_bytes(value: bytes) -> str:
    return hashlib.sha256(value).hexdigest()


def sha256_text(value: str) -> str:
    return sha256_bytes(value.encode("utf-8"))


def canonical_json(value: Any) -> bytes:
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8")


def fact_sha256(fact: dict[str, Any]) -> str:
    return sha256_bytes(canonical_json(fact))


def fact_catalog_sha256(facts: list[dict[str, Any]]) -> str:
    records = sorted(
        ({"fact_id": str(fact.get("id") or ""), "fact_sha256": fact_sha256(fact)} for fact in facts),
        key=lambda record: record["fact_id"],
    )
    if any(not record["fact_id"] for record in records):
        raise DatasetError("fact catalog contains an empty fact id")
    if len({record["fact_id"] for record in records}) != len(records):
        raise DatasetError("fact catalog contains duplicate fact ids")
    return sha256_bytes(canonical_json(records))


def _provenance_session_ids(fact: dict[str, Any]) -> list[str]:
    """Return the canonical provenance IDs, or an empty list for any malformed provenance."""
    provenance = fact.get("provenance")
    if not isinstance(provenance, list) or not provenance:
        return []
    session_ids: list[str] = []
    for anchor in provenance:
        if not isinstance(anchor, dict):
            return []
        session_id = str(anchor.get("session_id") or "").strip()
        if not session_id:
            return []
        session_ids.append(session_id)
    return sorted(set(session_ids))


def _membership_fact_records(facts: list[dict[str, Any]]) -> list[dict[str, Any]]:
    records = sorted(({
        "fact_id": str(fact.get("id") or ""),
        "fact_sha256": fact_sha256(fact),
        "status": str(fact.get("status") or ""),
        "provenance_session_ids": _provenance_session_ids(fact),
    } for fact in facts), key=lambda record: record["fact_id"])
    if any(not record["fact_id"] or not record["status"] for record in records):
        raise DatasetError("source membership contains an empty fact id or status")
    if len({record["fact_id"] for record in records}) != len(records):
        raise DatasetError("source membership contains duplicate fact ids")
    return records


def fact_membership_root_sha256(records: list[dict[str, Any]]) -> str:
    normalized = sorted(({
        "fact_id": str(record.get("fact_id") or ""),
        "fact_sha256": str(record.get("fact_sha256") or ""),
        "status": str(record.get("status") or ""),
    } for record in records), key=lambda record: record["fact_id"])
    if any(
        not record["fact_id"] or not _is_sha256(record["fact_sha256"]) or not record["status"]
        for record in normalized
    ):
        raise DatasetError("source membership fact record is malformed")
    if len({record["fact_id"] for record in normalized}) != len(normalized):
        raise DatasetError("source membership contains duplicate fact ids")
    return sha256_bytes(canonical_json(normalized))


def eligibility_membership_root_sha256(records: list[dict[str, Any]]) -> str:
    normalized: list[dict[str, Any]] = []
    for record in records:
        session_ids = record.get("provenance_session_ids")
        if not isinstance(session_ids, list) or not all(
            isinstance(session_id, str) and bool(session_id) for session_id in session_ids
        ):
            raise DatasetError("source eligibility membership provenance is malformed")
        if session_ids != sorted(set(session_ids)):
            raise DatasetError("source eligibility membership provenance is not canonical")
        normalized.append({
            "fact_id": str(record.get("fact_id") or ""),
            "fact_sha256": str(record.get("fact_sha256") or ""),
            "status": str(record.get("status") or ""),
            "provenance_session_ids": session_ids,
        })
    normalized.sort(key=lambda record: record["fact_id"])
    if any(
        not record["fact_id"] or not _is_sha256(record["fact_sha256"]) or not record["status"]
        for record in normalized
    ):
        raise DatasetError("source eligibility membership fact record is malformed")
    if len({record["fact_id"] for record in normalized}) != len(normalized):
        raise DatasetError("source eligibility membership contains duplicate fact ids")
    return sha256_bytes(canonical_json(normalized))


def fact_record_catalog_sha256(records: list[dict[str, Any]], *, status: str | None = None) -> str:
    selected = [record for record in records if status is None or record.get("status") == status]
    normalized = sorted(({
        "fact_id": str(record.get("fact_id") or ""),
        "fact_sha256": str(record.get("fact_sha256") or ""),
    } for record in selected), key=lambda record: record["fact_id"])
    if any(not record["fact_id"] or not _is_sha256(record["fact_sha256"]) for record in normalized):
        raise DatasetError("source membership fact catalog record is malformed")
    if len({record["fact_id"] for record in normalized}) != len(normalized):
        raise DatasetError("source membership fact catalog contains duplicate ids")
    return sha256_bytes(canonical_json(normalized))


def session_date_membership_root_sha256(session_dates: dict[str, Any]) -> str:
    normalized: dict[str, str] = {}
    for session_id, value in session_dates.items():
        if not isinstance(session_id, str) or not session_id or not isinstance(value, str):
            raise DatasetError("source membership session-date record is malformed")
        _parse_time(value, f"source membership session date {session_id}")
        normalized[session_id] = value
    return sha256_bytes(canonical_json(normalized))


def build_source_membership(
    facts: list[dict[str, Any]],
    session_dates: dict[str, str],
    facts_source_sha256: str,
    session_dates_source_sha256: str,
) -> dict[str, Any]:
    """Build a complete authenticated ID/hash/status and session/date catalog."""
    records = _membership_fact_records(facts)
    active = [record for record in records if record["status"] == "active"]
    return {
        "schema_version": SOURCE_MEMBERSHIP_SCHEMA_VERSION,
        "source_facts_sha256": facts_source_sha256,
        "source_session_dates_sha256": session_dates_source_sha256,
        "fact_count": len(records),
        "active_fact_count": len(active),
        "fact_membership_root_sha256": fact_membership_root_sha256(records),
        "eligibility_membership_root_sha256": eligibility_membership_root_sha256(records),
        "active_fact_catalog_sha256": fact_record_catalog_sha256(records, status="active"),
        "facts": records,
        "session_date_count": len(session_dates),
        "session_date_membership_root_sha256": session_date_membership_root_sha256(session_dates),
        "session_dates": dict(sorted(session_dates.items())),
    }


def build_source_contract(membership_path: str, membership: dict[str, Any], membership_sha256: str) -> dict[str, Any]:
    """Build the small reviewed trust-root document for a complete membership catalog."""
    return {
        "schema_version": SOURCE_CONTRACT_SCHEMA_VERSION,
        "contract_id": "agent-brain-offline-relevance-source-2026-07-15",
        "membership_path": membership_path,
        "membership_sha256": membership_sha256,
        "source_facts_sha256": membership["source_facts_sha256"],
        "source_session_dates_sha256": membership["source_session_dates_sha256"],
        "fact_count": membership["fact_count"],
        "active_fact_count": membership["active_fact_count"],
        "fact_membership_root_sha256": membership["fact_membership_root_sha256"],
        "eligibility_membership_root_sha256": membership["eligibility_membership_root_sha256"],
        "active_fact_catalog_sha256": membership["active_fact_catalog_sha256"],
        "session_date_count": membership["session_date_count"],
        "session_date_membership_root_sha256": membership["session_date_membership_root_sha256"],
    }


def temporal_policy_sha256(cutoff: str, excluded_sessions: list[str]) -> str:
    return sha256_bytes(canonical_json({
        "temporal_cutoff": cutoff,
        "exclude_session_ids": excluded_sessions,
    }))


def _is_sha256(value: Any) -> bool:
    return isinstance(value, str) and re.fullmatch(r"[0-9a-f]{64}", value) is not None


def _is_nonnegative_int(value: Any) -> bool:
    return isinstance(value, int) and not isinstance(value, bool) and value >= 0


def load_json(path: pathlib.Path) -> Any:
    try:
        return json.loads(path.read_text())
    except (OSError, json.JSONDecodeError) as exc:
        raise DatasetError(f"cannot read JSON {path}: {exc}") from exc


def load_facts(path: pathlib.Path) -> list[dict[str, Any]]:
    facts: list[dict[str, Any]] = []
    try:
        lines = path.read_text().splitlines()
    except OSError as exc:
        raise DatasetError(f"cannot read facts {path}: {exc}") from exc
    for line_number, line in enumerate(lines, 1):
        if not line.strip():
            continue
        try:
            fact = json.loads(line)
        except json.JSONDecodeError as exc:
            raise DatasetError(f"invalid fact JSON at {path}:{line_number}: {exc}") from exc
        if not isinstance(fact, dict):
            raise DatasetError(f"fact at {path}:{line_number} is not an object")
        facts.append(fact)
    return facts


def _parse_time(value: Any, label: str) -> dt.datetime:
    if not isinstance(value, str) or not value.strip():
        raise DatasetError(f"{label} must be a non-empty RFC3339 timestamp")
    text = value.strip()
    if text.endswith(("Z", "z")):
        text = text[:-1] + "+00:00"
    try:
        parsed = dt.datetime.fromisoformat(text)
    except ValueError as exc:
        raise DatasetError(f"{label} is not an RFC3339 timestamp: {value!r}") from exc
    if parsed.tzinfo is None or parsed.utcoffset() is None:
        raise DatasetError(f"{label} must include an RFC3339 UTC offset: {value!r}")
    return parsed


def _unique_index(rows: list[dict[str, Any]], key: str, label: str) -> dict[str, dict[str, Any]]:
    output: dict[str, dict[str, Any]] = {}
    for row in rows:
        value = str(row.get(key) or "").strip()
        if not value:
            raise DatasetError(f"{label} row has no {key}")
        if value in output:
            raise DatasetError(f"duplicate {label} {key}: {value}")
        output[value] = row
    return output


def _eligible(fact: dict[str, Any], session_dates: dict[str, str], cutoff: str,
              excluded_sessions: set[str]) -> bool:
    cutoff_at = _parse_time(cutoff, "temporal cutoff")
    session_ids = _provenance_session_ids(fact)
    if not session_ids:
        return False
    if any(session_id in excluded_sessions for session_id in session_ids):
        return False
    for session_id in session_ids:
        created = session_dates.get(session_id)
        if created is None:
            return False
        if not _parse_time(created, f"session date {session_id}") < cutoff_at:
            return False
    return True


def active_eligible_fact_catalog(
    membership: dict[str, Any], cutoff: str, excluded_sessions: list[str]
) -> list[dict[str, str]]:
    """Derive the exact active+eligible ID/hash catalog from authenticated membership metadata."""
    cutoff_at = _parse_time(cutoff, "temporal cutoff")
    excluded = set(excluded_sessions)
    facts = membership.get("facts")
    session_dates = membership.get("session_dates")
    if membership.get("schema_version") != SOURCE_MEMBERSHIP_SCHEMA_VERSION:
        raise DatasetError("null review requires provenance-aware source membership schema")
    if not isinstance(facts, list) or not isinstance(session_dates, dict):
        raise DatasetError("null review source membership is malformed")
    output: list[dict[str, str]] = []
    for record in facts:
        if not isinstance(record, dict) or record.get("status") != "active":
            continue
        session_ids = record.get("provenance_session_ids")
        if not isinstance(session_ids, list) or not session_ids:
            continue
        if session_ids != sorted(set(session_ids)) or not all(
            isinstance(session_id, str) and bool(session_id) for session_id in session_ids
        ):
            raise DatasetError("null review source membership provenance is malformed")
        if any(session_id in excluded for session_id in session_ids):
            continue
        eligible = True
        for session_id in session_ids:
            created = session_dates.get(session_id)
            if not isinstance(created, str):
                eligible = False
                break
            try:
                created_at = _parse_time(created, f"source membership session date {session_id}")
            except DatasetError:
                eligible = False
                break
            if not created_at < cutoff_at:
                eligible = False
                break
        if eligible:
            output.append({
                "fact_id": str(record.get("fact_id") or ""),
                "fact_sha256": str(record.get("fact_sha256") or ""),
            })
    output.sort(key=lambda record: record["fact_id"])
    fact_record_catalog_sha256(output)
    return output


def _load_inventory(inventory_path: pathlib.Path) -> dict[str, dict[str, Any]]:
    inventory = load_json(inventory_path)
    if not isinstance(inventory, dict) or not isinstance(inventory.get("tasks"), list):
        raise DatasetError("task inventory must contain a tasks array")
    rows = _unique_index(inventory["tasks"], "task_id", "inventory")
    for task_id, row in rows.items():
        artifact = row.get("artifacts")
        if not isinstance(artifact, dict):
            raise DatasetError(f"{task_id}: inventory artifacts missing")
        config_path = artifact.get("config_path")
        if not isinstance(config_path, str) or pathlib.PurePosixPath(config_path).is_absolute() or ".." in pathlib.PurePosixPath(config_path).parts:
            raise DatasetError(f"{task_id}: unsafe config_path")
    return rows


def _task_config(repo: pathlib.Path, inventory_row: dict[str, Any]) -> dict[str, Any]:
    path = repo / inventory_row["artifacts"]["config_path"]
    task_id = str(inventory_row.get("task_id") or "<unknown>")
    if not path.is_file():
        raise DatasetError(f"{task_id}: task config does not exist")
    if sha256_bytes(path.read_bytes()) != inventory_row["artifacts"].get("config_sha256"):
        raise DatasetError(f"{task_id}: task config hash differs from inventory")
    config = load_json(path)
    if not isinstance(config, dict):
        raise DatasetError(f"task config {path} is not an object")
    return config


def _repo_relative_file(repo: pathlib.Path, value: Any, label: str) -> pathlib.Path:
    if not isinstance(value, str) or not value:
        raise DatasetError(f"{label} path is missing")
    relative = pathlib.PurePosixPath(value)
    if relative.is_absolute() or ".." in relative.parts:
        raise DatasetError(f"{label} path is unsafe")
    target = (repo / pathlib.Path(*relative.parts)).resolve()
    try:
        target.relative_to(repo.resolve())
    except ValueError as exc:
        raise DatasetError(f"{label} path escapes the repository") from exc
    if not target.is_file():
        raise DatasetError(f"{label} path does not exist")
    return target


def _snapshot_payload(facts: list[dict[str, Any]], session_dates: dict[str, str], wanted: set[str],
                      facts_source_sha256: str, session_dates_source_sha256: str) -> dict[str, Any]:
    fact_index = _unique_index(facts, "id", "fact corpus")
    missing = sorted(wanted - set(fact_index))
    if missing:
        raise DatasetError(f"labeled fact ids absent from frozen corpus: {missing}")
    selected = [fact_index[fact_id] for fact_id in sorted(wanted)]
    provenance_ids = sorted({
        str(anchor.get("session_id") or "")
        for fact in selected
        for anchor in (fact.get("provenance") or [])
        if isinstance(anchor, dict) and str(anchor.get("session_id") or "")
    })
    missing_sessions = sorted(set(provenance_ids) - set(session_dates))
    if missing_sessions:
        raise DatasetError(f"labeled facts have provenance absent from the full session-date map: {missing_sessions}")
    selected_dates = {session_id: session_dates[session_id] for session_id in provenance_ids}
    active = [fact for fact in facts if fact.get("status") == "active"]
    return {
        "schema_version": SCHEMA_VERSION,
        "source_facts_sha256": facts_source_sha256,
        "source_session_dates_sha256": session_dates_source_sha256,
        "source_active_fact_count": len(active),
        "source_active_fact_catalog_sha256": fact_catalog_sha256(active),
        "fact_count": len(selected),
        "facts_sha256": sha256_bytes(canonical_json(selected)),
        "facts": selected,
        "session_date_count": len(selected_dates),
        "session_dates_sha256": sha256_bytes(canonical_json(selected_dates)),
        "session_dates": selected_dates,
    }


def _wanted_fact_ids(labels: dict[str, Any]) -> set[str]:
    wanted: set[str] = set()
    for item in labels.get("items") or []:
        if not isinstance(item, dict):
            raise DatasetError("label item must be an object")
        for judgment in item.get("judgments") or []:
            if not isinstance(judgment, dict):
                raise DatasetError("judgment must be an object")
            wanted.add(str(judgment.get("fact_id") or ""))
    if "" in wanted:
        raise DatasetError("judgment fact_id cannot be empty")
    return wanted


def _snapshot_commitment(snapshot: dict[str, Any]) -> dict[str, Any]:
    return {
        "fact_count": snapshot["fact_count"],
        "facts_sha256": snapshot["facts_sha256"],
        "session_date_count": snapshot["session_date_count"],
        "session_dates_sha256": snapshot["session_dates_sha256"],
    }


def bind_labels(labels: dict[str, Any], facts: list[dict[str, Any]], session_dates: dict[str, str],
                facts_source_sha256: str, session_dates_source_sha256: str) -> dict[str, Any]:
    """Add full-source fact hashes and excerpt commitments without changing judgments."""
    if labels.get("schema_version") != SCHEMA_VERSION:
        raise DatasetError(f"label source schema_version must be {SCHEMA_VERSION}")
    bound = copy.deepcopy(labels)
    source = bound.get("source_corpus")
    if not isinstance(source, dict):
        raise DatasetError("label source_corpus must be an object")
    if source.get("facts_sha256") != facts_source_sha256:
        raise DatasetError("full fact corpus hash does not match label source")
    if source.get("session_dates_sha256") != session_dates_source_sha256:
        raise DatasetError("session date map hash does not match label source")
    fact_index = _unique_index(facts, "id", "fact corpus")
    for item in bound.get("items") or []:
        if not isinstance(item, dict):
            raise DatasetError("label item must be an object")
        for judgment in item.get("judgments") or []:
            if not isinstance(judgment, dict):
                raise DatasetError("judgment must be an object")
            fact_id = str(judgment.get("fact_id") or "")
            fact = fact_index.get(fact_id)
            if fact is None:
                raise DatasetError(f"labeled fact id absent from frozen corpus: {fact_id!r}")
            judgment["fact_sha256"] = fact_sha256(fact)
    active = [fact for fact in facts if fact.get("status") == "active"]
    source["active_fact_count"] = len(active)
    source["active_fact_catalog_sha256"] = fact_catalog_sha256(active)
    snapshot = _snapshot_payload(
        facts,
        session_dates,
        _wanted_fact_ids(bound),
        facts_source_sha256,
        session_dates_source_sha256,
    )
    bound["snapshot_commitment"] = _snapshot_commitment(snapshot)
    return bound


def build_snapshot(labels: dict[str, Any], facts: list[dict[str, Any]], session_dates: dict[str, str],
                   facts_source_sha256: str, session_dates_source_sha256: str) -> dict[str, Any]:
    if labels.get("schema_version") != SCHEMA_VERSION:
        raise DatasetError(f"label source schema_version must be {SCHEMA_VERSION}")
    expected_source = labels.get("source_corpus")
    if not isinstance(expected_source, dict):
        raise DatasetError("label source_corpus must be an object")
    if expected_source.get("facts_sha256") != facts_source_sha256:
        raise DatasetError("full fact corpus hash does not match label source")
    if expected_source.get("session_dates_sha256") != session_dates_source_sha256:
        raise DatasetError("session date map hash does not match label source")
    snapshot = _snapshot_payload(
        facts,
        session_dates,
        _wanted_fact_ids(labels),
        facts_source_sha256,
        session_dates_source_sha256,
    )
    if expected_source.get("active_fact_count") != snapshot["source_active_fact_count"]:
        raise DatasetError("active full-corpus fact count does not match label source")
    if expected_source.get("active_fact_catalog_sha256") != snapshot["source_active_fact_catalog_sha256"]:
        raise DatasetError("active full-corpus fact catalog hash does not match label source")
    fact_index = _unique_index(facts, "id", "fact corpus")
    for item in labels.get("items") or []:
        query_id = str(item.get("query_id") or "")
        for judgment in item.get("judgments") or []:
            fact_id = str(judgment.get("fact_id") or "")
            expected_hash = judgment.get("fact_sha256")
            if not isinstance(expected_hash, str) or re.fullmatch(r"[0-9a-f]{64}", expected_hash) is None:
                raise DatasetError(f"{query_id}/{fact_id}: reviewed fact_sha256 is missing or invalid")
            if fact_sha256(fact_index[fact_id]) != expected_hash:
                raise DatasetError(f"{query_id}/{fact_id}: reviewed fact hash differs from full corpus")
    if labels.get("snapshot_commitment") != _snapshot_commitment(snapshot):
        raise DatasetError("label snapshot commitment differs from the pinned full-source excerpt")
    return snapshot


def validate_source_membership(
    labels: dict[str, Any],
    snapshot: dict[str, Any],
    contract: dict[str, Any],
    membership: dict[str, Any],
    *,
    membership_sha256: str,
) -> None:
    """Prove every selected fact and date belongs to the independently pinned full source."""
    contract_fields = {
        "schema_version", "contract_id", "membership_path", "membership_sha256",
        "source_facts_sha256", "source_session_dates_sha256", "fact_count",
        "active_fact_count", "fact_membership_root_sha256", "eligibility_membership_root_sha256",
        "active_fact_catalog_sha256",
        "session_date_count", "session_date_membership_root_sha256",
    }
    if (
        not isinstance(contract, dict)
        or set(contract) != contract_fields
        or contract.get("schema_version") != SOURCE_CONTRACT_SCHEMA_VERSION
    ):
        raise DatasetError("source contract fields or schema version changed")
    if contract.get("membership_sha256") != membership_sha256:
        raise DatasetError("source membership content hash differs from reviewed contract")
    membership_fields = {
        "schema_version", "source_facts_sha256", "source_session_dates_sha256", "fact_count",
        "active_fact_count", "fact_membership_root_sha256", "eligibility_membership_root_sha256",
        "active_fact_catalog_sha256", "facts",
        "session_date_count", "session_date_membership_root_sha256", "session_dates",
    }
    if (
        not isinstance(membership, dict)
        or set(membership) != membership_fields
        or membership.get("schema_version") != SOURCE_MEMBERSHIP_SCHEMA_VERSION
    ):
        raise DatasetError("source membership fields or schema version changed")
    facts = membership.get("facts")
    session_dates = membership.get("session_dates")
    if not isinstance(facts, list) or not all(isinstance(record, dict) for record in facts):
        raise DatasetError("source membership facts are malformed")
    if not isinstance(session_dates, dict):
        raise DatasetError("source membership session dates are malformed")
    expected_fact_record_fields = {"fact_id", "fact_sha256", "status", "provenance_session_ids"}
    if any(set(record) != expected_fact_record_fields for record in facts):
        raise DatasetError("source membership fact record fields changed")
    if membership.get("fact_count") != len(facts):
        raise DatasetError("source membership fact count is stale")
    if membership.get("active_fact_count") != sum(record.get("status") == "active" for record in facts):
        raise DatasetError("source membership active fact count is stale")
    if membership.get("fact_membership_root_sha256") != fact_membership_root_sha256(facts):
        raise DatasetError("source membership fact root is stale")
    if membership.get("eligibility_membership_root_sha256") != eligibility_membership_root_sha256(facts):
        raise DatasetError("source membership eligibility root is stale")
    if membership.get("active_fact_catalog_sha256") != fact_record_catalog_sha256(facts, status="active"):
        raise DatasetError("source membership active fact root is stale")
    if membership.get("session_date_count") != len(session_dates):
        raise DatasetError("source membership session-date count is stale")
    if membership.get("session_date_membership_root_sha256") != session_date_membership_root_sha256(session_dates):
        raise DatasetError("source membership session-date root is stale")
    for field in (
        "source_facts_sha256", "source_session_dates_sha256", "fact_count", "active_fact_count",
        "fact_membership_root_sha256", "eligibility_membership_root_sha256",
        "active_fact_catalog_sha256", "session_date_count",
        "session_date_membership_root_sha256",
    ):
        if contract.get(field) != membership.get(field):
            raise DatasetError(f"source membership {field} differs from reviewed contract")

    source = labels.get("source_corpus") if isinstance(labels.get("source_corpus"), dict) else {}
    expected_source = {
        "facts_sha256": contract["source_facts_sha256"],
        "session_dates_sha256": contract["source_session_dates_sha256"],
        "active_fact_count": contract["active_fact_count"],
        "active_fact_catalog_sha256": contract["active_fact_catalog_sha256"],
    }
    if source != expected_source:
        raise DatasetError("labels full-source fields differ from reviewed source contract")
    snapshot_source = {
        "facts_sha256": snapshot.get("source_facts_sha256"),
        "session_dates_sha256": snapshot.get("source_session_dates_sha256"),
        "active_fact_count": snapshot.get("source_active_fact_count"),
        "active_fact_catalog_sha256": snapshot.get("source_active_fact_catalog_sha256"),
    }
    if snapshot_source != expected_source:
        raise DatasetError("snapshot full-source fields differ from reviewed source contract")

    member_index = _unique_index(facts, "fact_id", "source membership")
    selected_facts = snapshot.get("facts")
    selected_dates = snapshot.get("session_dates")
    if not isinstance(selected_facts, list) or not isinstance(selected_dates, dict):
        raise DatasetError("snapshot facts/session dates are malformed")
    selected_index = _unique_index(selected_facts, "id", "fact snapshot")
    for fact_id, fact in selected_index.items():
        member = member_index.get(fact_id)
        if member is None:
            raise DatasetError(f"snapshot fact is absent from reviewed full-source membership: {fact_id}")
        if member.get("fact_sha256") != fact_sha256(fact):
            raise DatasetError(f"snapshot fact differs from reviewed full-source membership: {fact_id}")
        if member.get("status") != "active":
            raise DatasetError(f"snapshot fact is not active in reviewed full-source membership: {fact_id}")
    expected_session_ids = {
        str(anchor.get("session_id") or "")
        for fact in selected_facts
        for anchor in (fact.get("provenance") or [])
        if isinstance(anchor, dict) and str(anchor.get("session_id") or "")
    }
    if set(selected_dates) != expected_session_ids:
        raise DatasetError("snapshot session-date keys do not exactly cover selected fact provenance")
    for session_id, value in selected_dates.items():
        if session_dates.get(session_id) != value:
            raise DatasetError(f"snapshot session date differs from reviewed full-source membership: {session_id}")
    for item in labels.get("items") or []:
        query_id = str(item.get("query_id") or "")
        for judgment in item.get("judgments") or []:
            fact_id = str(judgment.get("fact_id") or "")
            member = member_index.get(fact_id)
            if member is None or member.get("fact_sha256") != judgment.get("fact_sha256"):
                raise DatasetError(f"{query_id}/{fact_id}: judgment differs from reviewed full-source membership")


def validate_null_review_contract(
    contract: dict[str, Any],
    ledger: dict[str, Any],
    source_contract: dict[str, Any],
    *,
    ledger_path: str,
    ledger_sha256: str,
) -> None:
    required = {
        "schema_version", "contract_id", "ledger_path", "ledger_sha256",
        "source_facts_sha256", "source_session_dates_sha256",
        "eligibility_membership_root_sha256",
    }
    if not isinstance(contract, dict) or set(contract) != required or contract.get("schema_version") != 1:
        raise DatasetError("null review contract fields or schema version changed")
    if contract.get("ledger_path") != ledger_path or contract.get("ledger_sha256") != ledger_sha256:
        raise DatasetError("null review ledger path or content hash differs from reviewed contract")
    for field in (
        "source_facts_sha256", "source_session_dates_sha256", "eligibility_membership_root_sha256"
    ):
        if contract.get(field) != source_contract.get(field):
            raise DatasetError(f"null review contract {field} differs from source contract")
    _null_review_index(ledger)


def _ledger_judgments(item: dict[str, Any]) -> list[dict[str, Any]]:
    return [{
        "fact_id": judgment.get("fact_id"),
        "fact_sha256": judgment.get("fact_sha256"),
        "grade": judgment.get("grade"),
        "cluster_id": judgment.get("cluster_id"),
        "rationale": judgment.get("rationale"),
    } for judgment in item.get("judgments") or []]


def _ledger_item(item: dict[str, Any], roots: dict[str, Any], reviewed_at: str,
                 reviewer: str) -> dict[str, Any]:
    entry = {
        "query_id": item.get("query_id"),
        "task_id": item.get("task_id"),
        "query_source": item.get("query_source"),
        "reviewed_at": reviewed_at,
        "reviewer": reviewer,
        "review_method": REVIEW_METHOD,
        **roots,
        "decision_summary": (item.get("label_evidence") or {}).get("rationale"),
        "judgments": _ledger_judgments(item),
    }
    if item.get("null_query") is True:
        entry["null_review"] = copy.deepcopy(item.get("null_review"))
    return entry


def build_review_ledger(
    labels: dict[str, Any],
    contract: dict[str, Any],
    *,
    reviewed_at: str,
    reviewer: str,
) -> dict[str, Any]:
    _parse_time(reviewed_at, "reviewed_at")
    if not reviewer.strip():
        raise DatasetError("reviewer must be non-empty")
    roots = {
        "source_facts_sha256": contract["source_facts_sha256"],
        "source_session_dates_sha256": contract["source_session_dates_sha256"],
        "fact_membership_root_sha256": contract["fact_membership_root_sha256"],
        "eligibility_membership_root_sha256": contract["eligibility_membership_root_sha256"],
        "active_fact_catalog_sha256": contract["active_fact_catalog_sha256"],
        "session_date_membership_root_sha256": contract["session_date_membership_root_sha256"],
    }
    return {
        "schema_version": 1,
        "ledger_id": "agent-brain-offline-relevance-review-2026-07-15",
        "items": [
            _ledger_item(item, roots, reviewed_at, reviewer)
            for item in labels.get("items") or []
        ],
    }


def validate_review_ledger(
    labels: dict[str, Any],
    ledger: dict[str, Any],
    contract: dict[str, Any],
    *,
    ledger_path: str,
    ledger_sha256: str,
) -> None:
    if not isinstance(ledger, dict) or set(ledger) != {"schema_version", "ledger_id", "items"}:
        raise DatasetError("review ledger fields changed")
    if ledger.get("schema_version") != 1 or not isinstance(ledger.get("ledger_id"), str):
        raise DatasetError("review ledger schema version or id is invalid")
    entries = ledger.get("items")
    if not isinstance(entries, list) or not all(isinstance(entry, dict) for entry in entries):
        raise DatasetError("review ledger items are malformed")
    entry_index = _unique_index(entries, "query_id", "review ledger")
    label_items = labels.get("items")
    if not isinstance(label_items, list):
        raise DatasetError("labels items are malformed")
    if set(entry_index) != {str(item.get("query_id") or "") for item in label_items}:
        raise DatasetError("review ledger query coverage differs from labels")
    root_fields = (
        "source_facts_sha256", "source_session_dates_sha256", "fact_membership_root_sha256",
        "eligibility_membership_root_sha256", "active_fact_catalog_sha256",
        "session_date_membership_root_sha256",
    )
    required = {
        "query_id", "task_id", "query_source", "reviewed_at", "reviewer", "review_method",
        *root_fields, "decision_summary", "judgments",
    }
    for item in label_items:
        query_id = str(item.get("query_id") or "")
        evidence = item.get("label_evidence")
        if not isinstance(evidence, dict):
            raise DatasetError(f"{query_id}: label evidence is malformed")
        if evidence.get("type") != "manual_frozen_corpus_review":
            raise DatasetError(f"{query_id}: only retained manual review ledger evidence is accepted")
        if evidence.get("source_path") != ledger_path:
            raise DatasetError(f"{query_id}: evidence source_path does not identify the retained review ledger")
        if evidence.get("source_sha256") != ledger_sha256:
            raise DatasetError(f"{query_id}: evidence source hash differs from retained review ledger")
        if evidence.get("source_id") != f"{ledger_path}#{query_id}":
            raise DatasetError(f"{query_id}: evidence source_id does not identify its precise ledger entry")
        entry = entry_index[query_id]
        expected_fields = required | ({"null_review"} if item.get("null_query") is True else set())
        if set(entry) != expected_fields:
            raise DatasetError(f"{query_id}: review ledger entry fields changed")
        if entry.get("task_id") != item.get("task_id") or entry.get("query_source") != item.get("query_source"):
            raise DatasetError(f"{query_id}: review ledger task/query source differs from labels")
        if entry.get("review_method") != REVIEW_METHOD:
            raise DatasetError(f"{query_id}: review ledger method is invalid")
        _parse_time(entry.get("reviewed_at"), f"{query_id} reviewed_at")
        if not isinstance(entry.get("reviewer"), str) or not entry["reviewer"].strip():
            raise DatasetError(f"{query_id}: review ledger reviewer is missing")
        if entry.get("decision_summary") != evidence.get("rationale"):
            raise DatasetError(f"{query_id}: review ledger decision summary differs from labels")
        if entry.get("judgments") != _ledger_judgments(item):
            raise DatasetError(f"{query_id}: review ledger judgments differ from labels")
        if item.get("null_query") is True and entry.get("null_review") != item.get("null_review"):
            raise DatasetError(f"{query_id}: review ledger null review differs from labels")
        for field in root_fields:
            if entry.get(field) != contract.get(field):
                raise DatasetError(f"{query_id}: review ledger {field} differs from source contract")


def _null_review_index(ledger: Any) -> dict[str, dict[str, Any]]:
    if not isinstance(ledger, dict) or set(ledger) != {"schema_version", "ledger_id", "items"}:
        raise DatasetError("null review ledger fields changed")
    if ledger.get("schema_version") != 1 or not isinstance(ledger.get("ledger_id"), str):
        raise DatasetError("null review ledger schema version or id is invalid")
    items = ledger.get("items")
    if not isinstance(items, list) or not all(isinstance(item, dict) for item in items):
        raise DatasetError("null review ledger items are malformed")
    return _unique_index(items, "query_id", "null review ledger")


def _derive_null_closure(
    query_id: str,
    task_id: str,
    reference: Any,
    review_entry: dict[str, Any],
    source: dict[str, Any],
    membership: dict[str, Any],
    query_hash: str,
    cutoff: str,
    excluded_sessions: list[str],
    *,
    ledger_path: str,
    ledger_sha256: str,
) -> tuple[dict[str, Any], dict[str, str]]:
    reference_fields = {"source_id", "source_path", "source_sha256"}
    if not isinstance(reference, dict) or set(reference) != reference_fields:
        raise DatasetError(f"{query_id}: null query requires exact retained review evidence")
    if reference.get("source_id") != f"{ledger_path}#{query_id}":
        raise DatasetError(f"{query_id}: null review source_id differs")
    if reference.get("source_path") != ledger_path or reference.get("source_sha256") != ledger_sha256:
        raise DatasetError(f"{query_id}: null review source path or hash differs")
    required = {
        "query_id", "task_id", "query_sha256", "temporal_policy_sha256", "reviewed_at",
        "reviewer", "method", "source_facts_sha256", "source_session_dates_sha256",
        "eligibility_membership_root_sha256", "reviewer_assertion", "decisions",
    }
    if set(review_entry) != required:
        raise DatasetError(f"{query_id}: null review ledger entry fields changed")
    if review_entry.get("task_id") != task_id or review_entry.get("query_sha256") != query_hash:
        raise DatasetError(f"{query_id}: null review task or query hash differs")
    if review_entry.get("method") != NULL_CLOSURE_METHOD:
        raise DatasetError(f"{query_id}: null review method is invalid")
    if review_entry.get("temporal_policy_sha256") != temporal_policy_sha256(cutoff, excluded_sessions):
        raise DatasetError(f"{query_id}: null review temporal policy hash differs")
    if review_entry.get("source_facts_sha256") != source.get("facts_sha256"):
        raise DatasetError(f"{query_id}: null review full fact source hash differs")
    if review_entry.get("source_session_dates_sha256") != source.get("session_dates_sha256"):
        raise DatasetError(f"{query_id}: null review session-date source hash differs")
    if review_entry.get("eligibility_membership_root_sha256") != membership.get(
        "eligibility_membership_root_sha256"
    ):
        raise DatasetError(f"{query_id}: null review eligibility membership root differs")
    _parse_time(review_entry.get("reviewed_at"), f"{query_id} null reviewed_at")
    if not isinstance(review_entry.get("reviewer"), str) or not review_entry["reviewer"].strip():
        raise DatasetError(f"{query_id}: null review reviewer is missing")
    if not isinstance(review_entry.get("reviewer_assertion"), str) or not review_entry["reviewer_assertion"].strip():
        raise DatasetError(f"{query_id}: null review reviewer assertion is missing")

    eligible = active_eligible_fact_catalog(membership, cutoff, excluded_sessions)
    eligible_index = {record["fact_id"]: record for record in eligible}
    decisions = review_entry.get("decisions")
    if not isinstance(decisions, list) or not all(isinstance(decision, dict) for decision in decisions):
        raise DatasetError(f"{query_id}: null review decisions are malformed")
    decision_index: dict[str, dict[str, Any]] = {}
    normalized_decisions: list[dict[str, str]] = []
    for decision in decisions:
        if set(decision) != {"fact_id", "fact_sha256", "grade"}:
            raise DatasetError(f"{query_id}: null review decision fields changed")
        fact_id = decision.get("fact_id")
        fact_hash = decision.get("fact_sha256")
        grade = decision.get("grade")
        if not isinstance(fact_id, str) or not fact_id or fact_id in decision_index:
            raise DatasetError(f"{query_id}: null review fact ids are missing or duplicated")
        if not _is_sha256(fact_hash) or grade not in GRADES:
            raise DatasetError(f"{query_id}/{fact_id}: null review hash or grade is invalid")
        decision_index[fact_id] = decision
        normalized_decisions.append({"fact_id": fact_id, "fact_sha256": fact_hash, "grade": grade})
    if set(decision_index) != set(eligible_index):
        raise DatasetError(f"{query_id}: null review does not exactly cover the derived active+eligible catalog")
    for fact_id, record in eligible_index.items():
        if decision_index[fact_id].get("fact_sha256") != record["fact_sha256"]:
            raise DatasetError(f"{query_id}/{fact_id}: null review fact hash differs from membership")
    normalized_decisions.sort(key=lambda decision: decision["fact_id"])
    positive_count = sum(
        decision["grade"] in {"solving", "relevant_alternative"}
        for decision in normalized_decisions
    )
    if positive_count:
        raise DatasetError(f"{query_id}: null review contains positive facts")
    catalog_hash = fact_record_catalog_sha256(eligible)
    closure = {
        "method": NULL_CLOSURE_METHOD,
        "query_sha256": query_hash,
        "temporal_policy_sha256": temporal_policy_sha256(cutoff, excluded_sessions),
        "source_facts_sha256": source["facts_sha256"],
        "source_session_dates_sha256": source["session_dates_sha256"],
        "eligibility_membership_root_sha256": membership["eligibility_membership_root_sha256"],
        "active_fact_count": membership["active_fact_count"],
        "active_fact_catalog_sha256": membership["active_fact_catalog_sha256"],
        "eligible_fact_count": len(eligible),
        "eligible_fact_catalog_sha256": catalog_hash,
        "reviewed_fact_count": len(normalized_decisions),
        "reviewed_fact_catalog_sha256": fact_record_catalog_sha256(normalized_decisions),
        "review_decisions_sha256": sha256_bytes(canonical_json(normalized_decisions)),
        "positive_fact_count": positive_count,
        "reviewer_assertion": review_entry["reviewer_assertion"],
    }
    return closure, {fact_id: str(decision["grade"]) for fact_id, decision in decision_index.items()}


def materialize(
    repo: pathlib.Path,
    labels_path: pathlib.Path,
    inventory_path: pathlib.Path,
    snapshot_path: pathlib.Path,
    *,
    source_membership: dict[str, Any] | None = None,
    null_review_ledger: dict[str, Any] | None = None,
    null_review_ledger_path: str = "",
    null_review_ledger_sha256: str = "",
) -> dict[str, Any]:
    labels = load_json(labels_path)
    snapshot = load_json(snapshot_path)
    if not isinstance(labels, dict) or labels.get("schema_version") != SCHEMA_VERSION:
        raise DatasetError(f"label source schema_version must be {SCHEMA_VERSION}")
    if not isinstance(snapshot, dict) or snapshot.get("schema_version") != SCHEMA_VERSION:
        raise DatasetError(f"fact snapshot schema_version must be {SCHEMA_VERSION}")
    if not isinstance(labels.get("label_set_id"), str) or not labels["label_set_id"].strip():
        raise DatasetError("label_set_id must be a non-empty string")
    source = labels.get("source_corpus")
    if not isinstance(source, dict):
        raise DatasetError("label source_corpus must be an object")
    if snapshot.get("source_facts_sha256") != source.get("facts_sha256"):
        raise DatasetError("fact snapshot source hash differs from labels")
    if snapshot.get("source_session_dates_sha256") != source.get("session_dates_sha256"):
        raise DatasetError("session-date snapshot source hash differs from labels")
    if snapshot.get("source_active_fact_count") != source.get("active_fact_count"):
        raise DatasetError("active full-corpus fact count differs between snapshot and labels")
    if snapshot.get("source_active_fact_catalog_sha256") != source.get("active_fact_catalog_sha256"):
        raise DatasetError("active full-corpus fact catalog hash differs between snapshot and labels")
    facts = snapshot.get("facts")
    session_dates = snapshot.get("session_dates")
    if not isinstance(facts, list) or not isinstance(session_dates, dict):
        raise DatasetError("fact snapshot facts/session_dates are malformed")
    if snapshot.get("fact_count") != len(facts):
        raise DatasetError("fact snapshot fact_count is stale")
    if snapshot.get("session_date_count") != len(session_dates):
        raise DatasetError("fact snapshot session_date_count is stale")
    if snapshot.get("facts_sha256") != sha256_bytes(canonical_json(facts)):
        raise DatasetError("fact snapshot facts commitment is stale")
    if snapshot.get("session_dates_sha256") != sha256_bytes(canonical_json(session_dates)):
        raise DatasetError("fact snapshot session_dates commitment is stale")
    if labels.get("snapshot_commitment") != _snapshot_commitment(snapshot):
        raise DatasetError("fact snapshot content differs from the reviewed label commitment")
    fact_index = _unique_index(facts, "id", "fact snapshot")
    inventory = _load_inventory(inventory_path)
    allowed_states = labels.get("allowed_inventory_states")
    if allowed_states != list(SAFE_STATES):
        raise DatasetError(f"allowed_inventory_states must be {list(SAFE_STATES)!r}")
    null_review_index = _null_review_index(null_review_ledger) if null_review_ledger is not None else {}
    consumed_null_reviews: set[str] = set()

    items: list[dict[str, Any]] = []
    query_ids: set[str] = set()
    query_hashes: set[str] = set()
    grade_counts: Counter[str] = Counter()
    evidence_counts: Counter[str] = Counter()
    for source_item in labels.get("items") or []:
        if not isinstance(source_item, dict):
            raise DatasetError("label item must be an object")
        query_id = str(source_item.get("query_id") or "").strip()
        task_id = str(source_item.get("task_id") or "").strip()
        if not query_id or query_id in query_ids:
            raise DatasetError(f"missing or duplicate query_id: {query_id!r}")
        query_ids.add(query_id)
        row = inventory.get(task_id)
        if row is None:
            raise DatasetError(f"{query_id}: task absent from inventory: {task_id}")
        state = row.get("state")
        if state not in SAFE_STATES or row.get("assigned_split") != "development":
            raise DatasetError(f"{query_id}: task is not exposed development material")
        config = _task_config(repo, row)
        prompt = config.get("prompt")
        if not isinstance(prompt, str) or not prompt:
            raise DatasetError(f"{query_id}: task prompt is missing")
        prompt_hash = sha256_text(prompt)
        if prompt_hash != row["artifacts"].get("prompt_sha256"):
            raise DatasetError(f"{query_id}: prompt hash differs from inventory")
        query_source = source_item.get("query_source")
        if query_source not in QUERY_SOURCES:
            raise DatasetError(f"{query_id}: invalid query_source")
        if query_source == "user_prompt_derived":
            if "query_text" in source_item:
                raise DatasetError(f"{query_id}: product query text must derive exactly from task prompt")
            query_text = prompt
        else:
            query_text = source_item.get("query_text")
            if not isinstance(query_text, str) or not query_text:
                raise DatasetError(f"{query_id}: oracle query_text is missing")
        query_hash = sha256_text(query_text)
        if query_hash in query_hashes:
            raise DatasetError(f"{query_id}: duplicate query text hash")
        query_hashes.add(query_hash)
        cutoff = config.get("rolling_cutoff_rfc3339")
        _parse_time(cutoff, f"{query_id} temporal cutoff")
        excluded = config.get("exclude_session_ids")
        if not isinstance(excluded, list) or not all(isinstance(value, str) for value in excluded):
            raise DatasetError(f"{query_id}: task exclude_session_ids is malformed")
        evidence = source_item.get("label_evidence")
        if not isinstance(evidence, dict) or evidence.get("type") not in EVIDENCE_TYPES:
            raise DatasetError(f"{query_id}: label_evidence is missing or invalid")
        if not isinstance(evidence.get("source_id"), str) or not evidence["source_id"].strip():
            raise DatasetError(f"{query_id}: label_evidence source_id is missing")
        source_hash = evidence.get("source_sha256")
        if not _is_sha256(source_hash):
            raise DatasetError(f"{query_id}: label_evidence source_sha256 is missing or invalid")
        source_path = evidence.get("source_path")
        retained_source = _repo_relative_file(repo, source_path, f"{query_id}: label_evidence source")
        if sha256_bytes(retained_source.read_bytes()) != source_hash:
            raise DatasetError(f"{query_id}: label_evidence retained source hash differs")
        if not isinstance(evidence.get("rationale"), str) or not evidence["rationale"].strip():
            raise DatasetError(f"{query_id}: label_evidence rationale is missing")
        evidence_counts[evidence["type"]] += 1
        judgments: list[dict[str, Any]] = []
        seen_fact_ids: set[str] = set()
        for source_judgment in source_item.get("judgments") or []:
            if not isinstance(source_judgment, dict):
                raise DatasetError(f"{query_id}: judgment must be an object")
            fact_id = str(source_judgment.get("fact_id") or "").strip()
            if not fact_id or fact_id in seen_fact_ids:
                raise DatasetError(f"{query_id}: missing or duplicate fact_id: {fact_id!r}")
            seen_fact_ids.add(fact_id)
            fact = fact_index.get(fact_id)
            if fact is None:
                raise DatasetError(f"{query_id}: fact absent from content-addressed snapshot: {fact_id}")
            if fact.get("status") != "active":
                raise DatasetError(f"{query_id}/{fact_id}: only active facts may be judged")
            reviewed_fact_hash = source_judgment.get("fact_sha256")
            if not _is_sha256(reviewed_fact_hash):
                raise DatasetError(f"{query_id}/{fact_id}: reviewed fact_sha256 is missing or invalid")
            if reviewed_fact_hash != fact_sha256(fact):
                raise DatasetError(f"{query_id}/{fact_id}: snapshot fact differs from reviewed fact hash")
            grade = source_judgment.get("grade")
            if grade not in GRADES:
                raise DatasetError(f"{query_id}/{fact_id}: invalid grade")
            cluster_id = source_judgment.get("cluster_id")
            rationale = source_judgment.get("rationale")
            if not isinstance(cluster_id, str) or not cluster_id or not isinstance(rationale, str) or not rationale.strip():
                raise DatasetError(f"{query_id}/{fact_id}: cluster_id/rationale is missing")
            kind = fact.get("kind")
            if not isinstance(kind, str) or not kind:
                raise DatasetError(f"{query_id}/{fact_id}: fact kind is missing")
            judgments.append({
                "fact_id": fact_id,
                "fact_sha256": reviewed_fact_hash,
                "grade": grade,
                "eligible": _eligible(fact, {str(k): str(v) for k, v in session_dates.items()}, cutoff, set(excluded)),
                "kind": kind,
                "cluster_id": cluster_id,
                "rationale": rationale,
            })
            grade_counts[grade] += 1
        if not judgments:
            raise DatasetError(f"{query_id}: judgments cannot be empty")
        null_query = source_item.get("null_query")
        if not isinstance(null_query, bool):
            raise DatasetError(f"{query_id}: null_query must be boolean")
        closure: dict[str, Any] | None = None
        null_decision_grades: dict[str, str] = {}
        if null_query:
            if query_source != "user_prompt_derived":
                raise DatasetError(f"{query_id}: null queries must be product-derived")
            if source_membership is None or null_review_ledger is None:
                raise DatasetError(f"{query_id}: null query requires authenticated membership and review ledger")
            review_entry = null_review_index.get(query_id)
            if review_entry is None:
                raise DatasetError(f"{query_id}: null review ledger entry is missing")
            closure, null_decision_grades = _derive_null_closure(
                query_id,
                task_id,
                source_item.get("null_review"),
                review_entry,
                source,
                source_membership,
                query_hash,
                cutoff,
                excluded,
                ledger_path=null_review_ledger_path,
                ledger_sha256=null_review_ledger_sha256,
            )
            consumed_null_reviews.add(query_id)
        elif "null_review" in source_item:
            raise DatasetError(f"{query_id}: non-null query cannot carry null review evidence")
        eligible_positive = [row for row in judgments if row["eligible"] and row["grade"] in {"solving", "relevant_alternative"}]
        if null_query and eligible_positive:
            raise DatasetError(f"{query_id}: null query has eligible positive judgments")
        if not null_query and not eligible_positive:
            raise DatasetError(f"{query_id}: answerable query has no eligible positive judgment")
        if not any(row["grade"] == "hard_topical_distractor" for row in judgments):
            raise DatasetError(f"{query_id}: a hard topical distractor judgment is required")
        if null_query:
            for judgment in judgments:
                if null_decision_grades.get(judgment["fact_id"]) != judgment["grade"]:
                    raise DatasetError(
                        f"{query_id}/{judgment['fact_id']}: judgment differs from exhaustive null review"
                    )
        materialized_item = {
            "query_id": query_id,
            "task_id": task_id,
            "query_source": query_source,
            "query_text": query_text,
            "query_sha256": query_hash,
            "task_prompt_sha256": prompt_hash,
            "temporal_cutoff": cutoff,
            "null_query": null_query,
            "task_inventory_state": state,
            "label_evidence": evidence,
            "judgments": judgments,
        }
        if null_query:
            materialized_item["null_closure"] = closure
        items.append(materialized_item)

    if not items:
        raise DatasetError("development labels cannot be empty")
    if set(null_review_index) != consumed_null_reviews:
        raise DatasetError("null review ledger coverage differs from null labels")
    observed_query_sources = {item["query_source"] for item in items}
    if observed_query_sources != set(QUERY_SOURCES):
        raise DatasetError(f"development labels must cover both query sources: {list(QUERY_SOURCES)!r}")
    missing_grades = [grade for grade in GRADES if not grade_counts[grade]]
    if missing_grades:
        raise DatasetError(f"development labels do not cover required grades: {missing_grades}")
    dataset_id = labels.get("dataset_id")
    if not isinstance(dataset_id, str) or not dataset_id:
        raise DatasetError("label dataset_id is missing")
    created_at = labels.get("created_at")
    _parse_time(created_at, "label created_at")
    task_count = len({item["task_id"] for item in items})
    null_query_count = sum(item["null_query"] for item in items)
    answerable_product_items = [
        item for item in items
        if item["query_source"] == "user_prompt_derived" and not item["null_query"]
    ]
    product_null_items = [
        item for item in items
        if item["query_source"] == "user_prompt_derived" and item["null_query"]
    ]
    return {
        "schema_version": SCHEMA_VERSION,
        "dataset_id": dataset_id,
        "status": "draft_pending_labels_and_holdout_seal",
        "created_at": created_at,
        "label_policy": {
            "query_sources": list(QUERY_SOURCES),
            "required_judgments": list(GRADES),
            "null_query_closure_required": True,
            "null_query_requirement_for_threshold": True,
            "null_query_coverage_status": (
                "present_corpus_closed" if null_query_count else "pending_no_corpus_closed_queries"
            ),
            "temporal_eligibility_required": True,
            "near_duplicate_cluster_ids_required": True,
            "positive_definition": ["solving", "relevant_alternative"],
            "fact_hash_definition": "sha256(canonical JSON fact record; UTF-8, sorted keys, compact separators)",
        },
        "development": {
            "item_count": len(items),
            "unique_task_count": task_count,
            "null_query_count": null_query_count,
            "answerable_product_item_count": len(answerable_product_items),
            "unique_answerable_product_task_count": len({item["task_id"] for item in answerable_product_items}),
            "product_null_query_count": len(product_null_items),
            "judgment_count": sum(grade_counts.values()),
            "grade_counts": {grade: grade_counts[grade] for grade in GRADES},
            "evidence_counts": {evidence: evidence_counts[evidence] for evidence in EVIDENCE_TYPES},
            "source_facts_sha256": snapshot["source_facts_sha256"],
            "source_session_dates_sha256": snapshot["source_session_dates_sha256"],
            "source_active_fact_count": snapshot["source_active_fact_count"],
            "source_active_fact_catalog_sha256": snapshot["source_active_fact_catalog_sha256"],
            "snapshot_facts_sha256": snapshot["facts_sha256"],
            "snapshot_session_dates_sha256": snapshot["session_dates_sha256"],
            "labels_sha256": sha256_bytes(labels_path.read_bytes()),
            "fact_snapshot_sha256": sha256_bytes(snapshot_path.read_bytes()),
            "items": items,
        },
        "sealed_holdout": {
            "item_count": 0,
            "unique_task_count": 0,
            "commitment_sha256": None,
            "sealed_at": None,
            "opened_at": None,
            "items": [],
        },
    }


def validate_dataset(dataset: dict[str, Any], expected: dict[str, Any]) -> None:
    if canonical_json(dataset) != canonical_json(expected):
        raise DatasetError("checked-in offline relevance dataset is stale; re-run materialize")
    development = dataset.get("development")
    if not isinstance(development, dict):
        raise DatasetError("development section is missing")
    items = development.get("items")
    if not isinstance(items, list):
        raise DatasetError("development items are missing")
    sealed = dataset.get("sealed_holdout")
    if sealed != {
        "item_count": 0,
        "unique_task_count": 0,
        "commitment_sha256": None,
        "sealed_at": None,
        "opened_at": None,
        "items": [],
    }:
        raise DatasetError("sealed holdout must remain unopened and empty during development labeling")


def _schema_type_matches(value: Any, expected_type: str) -> bool:
    return {
        "object": isinstance(value, dict),
        "array": isinstance(value, list),
        "string": isinstance(value, str),
        "integer": isinstance(value, int) and not isinstance(value, bool),
        "number": isinstance(value, (int, float)) and not isinstance(value, bool),
        "boolean": isinstance(value, bool),
        "null": value is None,
    }.get(expected_type, False)


def validate_schema_instance(value: Any, schema: dict[str, Any], label: str) -> list[str]:
    """Validate the JSON-Schema subset used by the relevance artifacts."""
    errors: list[str] = []

    def resolve(reference: str) -> dict[str, Any] | None:
        if not reference.startswith("#/"):
            return None
        current: Any = schema
        for part in reference[2:].split("/"):
            if not isinstance(current, dict) or part not in current:
                return None
            current = current[part]
        return current if isinstance(current, dict) else None

    def walk(instance: Any, rule: dict[str, Any], path: str) -> None:
        reference = rule.get("$ref")
        if reference is not None:
            target = resolve(reference) if isinstance(reference, str) else None
            if target is None:
                errors.append(f"{label}{path}: unresolved schema reference {reference!r}")
                return
            walk(instance, target, path)
            return
        if "const" in rule and instance != rule["const"]:
            errors.append(f"{label}{path}: value differs from schema const")
        if "enum" in rule and instance not in rule["enum"]:
            errors.append(f"{label}{path}: value is outside schema enum")
        expected = rule.get("type")
        if expected is not None:
            expected_types = expected if isinstance(expected, list) else [expected]
            if not all(isinstance(item, str) for item in expected_types) or not any(
                _schema_type_matches(instance, item) for item in expected_types
            ):
                errors.append(f"{label}{path}: value has wrong schema type")
                return
        if isinstance(instance, dict):
            required = rule.get("required", [])
            if isinstance(required, list):
                for field in required:
                    if field not in instance:
                        errors.append(f"{label}{path}: missing schema-required field {field}")
            properties = rule.get("properties", {})
            if not isinstance(properties, dict):
                properties = {}
            for field, child in properties.items():
                if field in instance and isinstance(child, dict):
                    walk(instance[field], child, f"{path}.{field}")
            extras = set(instance) - set(properties)
            additional = rule.get("additionalProperties", True)
            if additional is False:
                for field in sorted(extras):
                    errors.append(f"{label}{path}: schema prohibits field {field}")
            elif isinstance(additional, dict):
                for field in sorted(extras):
                    walk(instance[field], additional, f"{path}.{field}")
        if isinstance(instance, list):
            minimum_items = rule.get("minItems")
            if isinstance(minimum_items, int) and len(instance) < minimum_items:
                errors.append(f"{label}{path}: array is shorter than schema minItems")
            if rule.get("uniqueItems") is True:
                encoded = [canonical_json(item) for item in instance]
                if len(encoded) != len(set(encoded)):
                    errors.append(f"{label}{path}: array violates schema uniqueItems")
            item_rule = rule.get("items")
            if isinstance(item_rule, dict):
                for index, item in enumerate(instance):
                    walk(item, item_rule, f"{path}[{index}]")
        if isinstance(instance, str):
            minimum_length = rule.get("minLength")
            if isinstance(minimum_length, int) and len(instance) < minimum_length:
                errors.append(f"{label}{path}: string is shorter than schema minLength")
            pattern = rule.get("pattern")
            if isinstance(pattern, str) and re.search(pattern, instance) is None:
                errors.append(f"{label}{path}: string does not match schema pattern")
            if rule.get("format") == "date-time":
                try:
                    _parse_time(instance, f"{label}{path}")
                except DatasetError as exc:
                    errors.append(str(exc))
        minimum = rule.get("minimum")
        if isinstance(minimum, (int, float)) and isinstance(instance, (int, float)) and not isinstance(instance, bool):
            if instance < minimum:
                errors.append(f"{label}{path}: number is below schema minimum")

    walk(value, schema, "$")
    return errors


def validate_relevance_schemas(labels: dict[str, Any], snapshot: dict[str, Any], dataset: dict[str, Any],
                               schema_dir: pathlib.Path, ledger: dict[str, Any] | None = None) -> list[str]:
    errors: list[str] = []
    artifacts: list[tuple[dict[str, Any], str, str]] = [
        (labels, "relevance-label-source.schema.json", "labels"),
        (snapshot, "relevance-fact-snapshot.schema.json", "snapshot"),
        (dataset, "relevance-dataset.schema.json", "dataset"),
    ]
    if ledger is not None:
        artifacts.append((ledger, "relevance-review-ledger.schema.json", "review ledger"))
    for artifact, filename, label in artifacts:
        schema = load_json(schema_dir / filename)
        if not isinstance(schema, dict):
            errors.append(f"{filename}: schema must be an object")
            continue
        if schema.get("$schema") != "https://json-schema.org/draft/2020-12/schema":
            errors.append(f"{filename}: wrong JSON Schema dialect")
        errors.extend(validate_schema_instance(artifact, schema, label))
    return errors


def _tokens(text: str) -> set[str]:
    return {token for token in re.findall(r"[a-z0-9]+", text.casefold()) if token not in STOPWORDS and len(token) > 1}


def propose(repo: pathlib.Path, inventory_path: pathlib.Path, facts_path: pathlib.Path,
            session_dates_path: pathlib.Path, limit: int) -> list[dict[str, Any]]:
    inventory = _load_inventory(inventory_path)
    facts = load_facts(facts_path)
    sessions = load_json(session_dates_path)
    if not isinstance(sessions, dict):
        raise DatasetError("session date map must be an object")
    active = [fact for fact in facts if fact.get("status") == "active"]
    document_frequency: Counter[str] = Counter()
    tokenized: dict[str, set[str]] = {}
    for fact in active:
        fact_id = str(fact.get("id") or "")
        tokens = _tokens(" ".join([str(fact.get("text") or ""), " ".join(fact.get("locus") or [])]))
        tokenized[fact_id] = tokens
        document_frequency.update(tokens)
    output: list[dict[str, Any]] = []
    for task_id, row in sorted(inventory.items()):
        if row.get("assigned_split") != "development" or row.get("state") not in SAFE_STATES:
            continue
        config = _task_config(repo, row)
        query_text = " ".join([str(config.get("prompt") or ""), *[str(value) for value in config.get("brain_queries") or []]])
        query_tokens = _tokens(query_text)
        cutoff = config.get("rolling_cutoff_rfc3339")
        excluded = set(config.get("exclude_session_ids") or [])
        candidates: list[tuple[float, dict[str, Any]]] = []
        for fact in active:
            if not _eligible(fact, {str(k): str(v) for k, v in sessions.items()}, cutoff, excluded):
                continue
            overlap = query_tokens & tokenized[str(fact.get("id") or "")]
            if not overlap:
                continue
            score = sum(math.log((len(active) + 1) / (document_frequency[token] + 1)) + 1 for token in overlap)
            candidates.append((score, fact))
        candidates.sort(key=lambda pair: (-pair[0], str(pair[1].get("id") or "")))
        output.append({
            "task_id": task_id,
            "inventory_state": row["state"],
            "query_text": config.get("prompt"),
            "candidates": [{
                "fact_id": fact.get("id"),
                "kind": fact.get("kind"),
                "score": round(score, 6),
                "text": fact.get("text"),
            } for score, fact in candidates[:limit]],
        })
    return output


def _render(value: Any) -> str:
    return json.dumps(value, indent=2, ensure_ascii=False) + "\n"


def _write_or_check(path: pathlib.Path, value: Any, check: bool) -> None:
    rendered = _render(value)
    if check:
        if not path.is_file() or path.read_text() != rendered:
            raise DatasetError(f"generated artifact is stale: {path}")
    else:
        path.write_text(rendered)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="command", required=True)

    propose_parser = subparsers.add_parser("propose", help="rank eligible frozen facts for exposed development prompts")
    propose_parser.add_argument("--repo", type=pathlib.Path, default=pathlib.Path.cwd())
    propose_parser.add_argument("--inventory", type=pathlib.Path, required=True)
    propose_parser.add_argument("--facts", type=pathlib.Path, required=True)
    propose_parser.add_argument("--session-dates", type=pathlib.Path, required=True)
    propose_parser.add_argument("--limit", type=int, default=10)

    bind_parser = subparsers.add_parser("bind", help="bind reviewed labels to the full pinned corpus")
    bind_parser.add_argument("--labels", type=pathlib.Path, required=True)
    bind_parser.add_argument("--facts", type=pathlib.Path, required=True)
    bind_parser.add_argument("--session-dates", type=pathlib.Path, required=True)
    bind_parser.add_argument("--output", type=pathlib.Path, required=True)
    bind_parser.add_argument("--check", action="store_true")

    snapshot_parser = subparsers.add_parser("snapshot", help="extract the labeled fact subset from a pinned full corpus")
    snapshot_parser.add_argument("--labels", type=pathlib.Path, required=True)
    snapshot_parser.add_argument("--facts", type=pathlib.Path, required=True)
    snapshot_parser.add_argument("--session-dates", type=pathlib.Path, required=True)
    snapshot_parser.add_argument("--output", type=pathlib.Path, required=True)
    snapshot_parser.add_argument("--check", action="store_true")

    membership_parser = subparsers.add_parser(
        "source-membership", help="build the complete authenticated fact/date membership catalog"
    )
    membership_parser.add_argument("--facts", type=pathlib.Path, required=True)
    membership_parser.add_argument("--session-dates", type=pathlib.Path, required=True)
    membership_parser.add_argument("--output", type=pathlib.Path, required=True)
    membership_parser.add_argument("--check", action="store_true")

    contract_parser = subparsers.add_parser(
        "source-contract", help="build the small reviewed contract that pins a membership catalog"
    )
    contract_parser.add_argument("--membership", type=pathlib.Path, required=True)
    contract_parser.add_argument("--membership-path", required=True)
    contract_parser.add_argument("--output", type=pathlib.Path, required=True)
    contract_parser.add_argument("--check", action="store_true")

    ledger_parser = subparsers.add_parser(
        "review-ledger", help="build retained per-query/per-judgment manual review evidence"
    )
    ledger_parser.add_argument("--labels", type=pathlib.Path, required=True)
    ledger_parser.add_argument("--source-contract", type=pathlib.Path, required=True)
    ledger_parser.add_argument("--reviewed-at", required=True)
    ledger_parser.add_argument("--reviewer", required=True)
    ledger_parser.add_argument("--output", type=pathlib.Path, required=True)
    ledger_parser.add_argument("--check", action="store_true")

    materialize_parser = subparsers.add_parser("materialize", help="derive the checked-in dataset from labels and fact snapshot")
    materialize_parser.add_argument("--repo", type=pathlib.Path, default=pathlib.Path.cwd())
    materialize_parser.add_argument("--labels", type=pathlib.Path, required=True)
    materialize_parser.add_argument("--inventory", type=pathlib.Path, required=True)
    materialize_parser.add_argument("--snapshot", type=pathlib.Path, required=True)
    materialize_parser.add_argument("--source-membership", type=pathlib.Path)
    materialize_parser.add_argument("--null-review-ledger", type=pathlib.Path)
    materialize_parser.add_argument("--output", type=pathlib.Path, required=True)
    materialize_parser.add_argument("--check", action="store_true")

    validate_parser = subparsers.add_parser("validate", help="fail if the checked-in dataset differs from its sources")
    validate_parser.add_argument("--repo", type=pathlib.Path, default=pathlib.Path.cwd())
    validate_parser.add_argument("--labels", type=pathlib.Path, required=True)
    validate_parser.add_argument("--inventory", type=pathlib.Path, required=True)
    validate_parser.add_argument("--snapshot", type=pathlib.Path, required=True)
    validate_parser.add_argument("--dataset", type=pathlib.Path, required=True)
    validate_parser.add_argument("--source-contract", type=pathlib.Path, required=True)
    validate_parser.add_argument("--source-membership", type=pathlib.Path, required=True)
    validate_parser.add_argument("--review-ledger", type=pathlib.Path, required=True)
    validate_parser.add_argument("--null-review-ledger", type=pathlib.Path, required=True)
    validate_parser.add_argument("--null-review-contract", type=pathlib.Path, required=True)
    validate_parser.add_argument(
        "--schemas",
        type=pathlib.Path,
        default=pathlib.Path(__file__).resolve().with_name("schemas"),
    )

    args = parser.parse_args()
    try:
        if args.command == "propose":
            print(_render(propose(args.repo.resolve(), args.inventory, args.facts, args.session_dates, args.limit)), end="")
        elif args.command == "bind":
            labels = load_json(args.labels)
            sessions = load_json(args.session_dates)
            if not isinstance(labels, dict) or not isinstance(sessions, dict):
                raise DatasetError("labels/session dates must be JSON objects")
            value = bind_labels(
                labels,
                load_facts(args.facts),
                {str(k): str(v) for k, v in sessions.items()},
                sha256_bytes(args.facts.read_bytes()),
                sha256_bytes(args.session_dates.read_bytes()),
            )
            _write_or_check(args.output, value, args.check)
        elif args.command == "snapshot":
            labels = load_json(args.labels)
            sessions = load_json(args.session_dates)
            if not isinstance(labels, dict) or not isinstance(sessions, dict):
                raise DatasetError("labels/session dates must be JSON objects")
            value = build_snapshot(
                labels,
                load_facts(args.facts),
                {str(k): str(v) for k, v in sessions.items()},
                sha256_bytes(args.facts.read_bytes()),
                sha256_bytes(args.session_dates.read_bytes()),
            )
            _write_or_check(args.output, value, args.check)
        elif args.command == "source-membership":
            sessions = load_json(args.session_dates)
            if not isinstance(sessions, dict):
                raise DatasetError("session dates must be a JSON object")
            value = build_source_membership(
                load_facts(args.facts),
                {str(k): str(v) for k, v in sessions.items()},
                sha256_bytes(args.facts.read_bytes()),
                sha256_bytes(args.session_dates.read_bytes()),
            )
            _write_or_check(args.output, value, args.check)
        elif args.command == "source-contract":
            membership = load_json(args.membership)
            if not isinstance(membership, dict):
                raise DatasetError("source membership must be a JSON object")
            value = build_source_contract(
                args.membership_path,
                membership,
                sha256_bytes(args.membership.read_bytes()),
            )
            _write_or_check(args.output, value, args.check)
        elif args.command == "review-ledger":
            labels = load_json(args.labels)
            contract = load_json(args.source_contract)
            if not isinstance(labels, dict) or not isinstance(contract, dict):
                raise DatasetError("labels/source contract must be JSON objects")
            value = build_review_ledger(
                labels,
                contract,
                reviewed_at=args.reviewed_at,
                reviewer=args.reviewer,
            )
            _write_or_check(args.output, value, args.check)
        elif args.command == "materialize":
            membership = load_json(args.source_membership) if args.source_membership else None
            null_ledger = load_json(args.null_review_ledger) if args.null_review_ledger else None
            ledger_path = (
                args.null_review_ledger.resolve().relative_to(args.repo.resolve()).as_posix()
                if args.null_review_ledger else ""
            )
            value = materialize(
                args.repo.resolve(), args.labels, args.inventory, args.snapshot,
                source_membership=membership if isinstance(membership, dict) else None,
                null_review_ledger=null_ledger if isinstance(null_ledger, dict) else None,
                null_review_ledger_path=ledger_path,
                null_review_ledger_sha256=(
                    sha256_bytes(args.null_review_ledger.read_bytes()) if args.null_review_ledger else ""
                ),
            )
            _write_or_check(args.output, value, args.check)
        else:
            labels = load_json(args.labels)
            snapshot = load_json(args.snapshot)
            contract = load_json(args.source_contract)
            membership = load_json(args.source_membership)
            ledger = load_json(args.review_ledger)
            null_ledger = load_json(args.null_review_ledger)
            null_contract = load_json(args.null_review_contract)
            if not all(isinstance(value, dict) for value in (
                labels, snapshot, contract, membership, ledger, null_ledger, null_contract
            )):
                raise DatasetError("relevance validation artifacts must be objects")
            null_ledger_path = args.null_review_ledger.resolve().relative_to(args.repo.resolve()).as_posix()
            null_ledger_sha = sha256_bytes(args.null_review_ledger.read_bytes())
            value = materialize(
                args.repo.resolve(), args.labels, args.inventory, args.snapshot,
                source_membership=membership,
                null_review_ledger=null_ledger,
                null_review_ledger_path=null_ledger_path,
                null_review_ledger_sha256=null_ledger_sha,
            )
            dataset = load_json(args.dataset)
            if not isinstance(dataset, dict):
                raise DatasetError("dataset must be an object")
            validate_dataset(dataset, value)
            validate_source_membership(
                labels,
                snapshot,
                contract,
                membership,
                membership_sha256=sha256_bytes(args.source_membership.read_bytes()),
            )
            validate_review_ledger(
                labels,
                ledger,
                contract,
                ledger_path=args.review_ledger.resolve().relative_to(args.repo.resolve()).as_posix(),
                ledger_sha256=sha256_bytes(args.review_ledger.read_bytes()),
            )
            validate_null_review_contract(
                null_contract,
                null_ledger,
                contract,
                ledger_path=null_ledger_path,
                ledger_sha256=null_ledger_sha,
            )
            schema_errors = validate_relevance_schemas(labels, snapshot, dataset, args.schemas, ledger)
            if schema_errors:
                raise DatasetError("schema validation failed: " + "; ".join(schema_errors))
            print(
                f"relevance development dataset valid: {value['development']['item_count']} queries, "
                f"{value['development']['unique_task_count']} tasks, "
                f"{value['development']['judgment_count']} judgments; sealed holdout unopened"
            )
    except DatasetError as exc:
        print(f"ERROR: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
