#!/usr/bin/env python3
"""Build and verify the exposed-only offline relevance development dataset.

Labels are human-reviewed source records.  This tool joins them to the reconciled task inventory
and a content-addressed excerpt of the frozen fact corpus, deriving query/fact hashes, kinds, and
rolling-cutoff eligibility.  It never reads a task outside the explicit development assignment and
the allowed exposed inventory states.
"""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import math
import pathlib
import re
import sys
from collections import Counter
from typing import Any


SCHEMA_VERSION = 1
SAFE_STATES = ("prompt_inspected", "retrieval_probed", "agent_run", "optimization_used")
GRADES = ("solving", "relevant_alternative", "hard_topical_distractor", "irrelevant")
QUERY_SOURCES = ("user_prompt_derived", "oracle_upper_bound")
EVIDENCE_TYPES = ("legacy_agent_packet", "manual_frozen_corpus_review")
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
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=dt.timezone.utc)
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
    provenance = fact.get("provenance")
    if not isinstance(provenance, list) or not provenance:
        return False
    session_ids = [
        str(anchor.get("session_id") or "").strip()
        for anchor in provenance if isinstance(anchor, dict)
    ]
    if not session_ids or any(not session_id for session_id in session_ids):
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


def build_snapshot(labels: dict[str, Any], facts: list[dict[str, Any]], session_dates: dict[str, str],
                   facts_source_sha256: str, session_dates_source_sha256: str) -> dict[str, Any]:
    if labels.get("schema_version") != SCHEMA_VERSION:
        raise DatasetError("label source schema_version must be 1")
    expected_source = labels.get("source_corpus")
    if not isinstance(expected_source, dict):
        raise DatasetError("label source_corpus must be an object")
    if expected_source.get("facts_sha256") != facts_source_sha256:
        raise DatasetError("full fact corpus hash does not match label source")
    if expected_source.get("session_dates_sha256") != session_dates_source_sha256:
        raise DatasetError("session date map hash does not match label source")
    fact_index = _unique_index(facts, "id", "fact corpus")
    wanted: set[str] = set()
    for item in labels.get("items") or []:
        if not isinstance(item, dict):
            raise DatasetError("label item must be an object")
        for judgment in item.get("judgments") or []:
            if not isinstance(judgment, dict):
                raise DatasetError("judgment must be an object")
            wanted.add(str(judgment.get("fact_id") or ""))
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
    return {
        "schema_version": SCHEMA_VERSION,
        "source_facts_sha256": facts_source_sha256,
        "source_session_dates_sha256": session_dates_source_sha256,
        "fact_count": len(selected),
        "facts": selected,
        "session_dates": {session_id: session_dates[session_id] for session_id in provenance_ids if session_id in session_dates},
    }


def materialize(repo: pathlib.Path, labels_path: pathlib.Path, inventory_path: pathlib.Path,
                snapshot_path: pathlib.Path) -> dict[str, Any]:
    labels = load_json(labels_path)
    snapshot = load_json(snapshot_path)
    if not isinstance(labels, dict) or labels.get("schema_version") != SCHEMA_VERSION:
        raise DatasetError("label source schema_version must be 1")
    if not isinstance(snapshot, dict) or snapshot.get("schema_version") != SCHEMA_VERSION:
        raise DatasetError("fact snapshot schema_version must be 1")
    source = labels.get("source_corpus")
    if not isinstance(source, dict):
        raise DatasetError("label source_corpus must be an object")
    if snapshot.get("source_facts_sha256") != source.get("facts_sha256"):
        raise DatasetError("fact snapshot source hash differs from labels")
    if snapshot.get("source_session_dates_sha256") != source.get("session_dates_sha256"):
        raise DatasetError("session-date snapshot source hash differs from labels")
    facts = snapshot.get("facts")
    session_dates = snapshot.get("session_dates")
    if not isinstance(facts, list) or not isinstance(session_dates, dict):
        raise DatasetError("fact snapshot facts/session_dates are malformed")
    if snapshot.get("fact_count") != len(facts):
        raise DatasetError("fact snapshot fact_count is stale")
    fact_index = _unique_index(facts, "id", "fact snapshot")
    inventory = _load_inventory(inventory_path)
    allowed_states = labels.get("allowed_inventory_states")
    if allowed_states != list(SAFE_STATES):
        raise DatasetError(f"allowed_inventory_states must be {list(SAFE_STATES)!r}")

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
        if source_hash is not None and (
            not isinstance(source_hash, str)
            or re.fullmatch(r"[0-9a-f]{64}", source_hash) is None
        ):
            raise DatasetError(f"{query_id}: label_evidence source_sha256 is invalid")
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
                "fact_sha256": fact_sha256(fact),
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
        eligible_positive = [row for row in judgments if row["eligible"] and row["grade"] in {"solving", "relevant_alternative"}]
        if null_query and eligible_positive:
            raise DatasetError(f"{query_id}: null query has eligible positive judgments")
        if not null_query and not eligible_positive:
            raise DatasetError(f"{query_id}: answerable query has no eligible positive judgment")
        if not any(row["grade"] == "hard_topical_distractor" for row in judgments):
            raise DatasetError(f"{query_id}: a hard topical distractor judgment is required")
        items.append({
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
        })

    if not items:
        raise DatasetError("development labels cannot be empty")
    if not any(item["null_query"] for item in items):
        raise DatasetError("development labels require at least one null query")
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
    return {
        "schema_version": SCHEMA_VERSION,
        "dataset_id": dataset_id,
        "status": "draft_pending_labels_and_holdout_seal",
        "created_at": created_at,
        "label_policy": {
            "query_sources": list(QUERY_SOURCES),
            "required_judgments": list(GRADES),
            "null_queries_required": True,
            "temporal_eligibility_required": True,
            "near_duplicate_cluster_ids_required": True,
            "positive_definition": ["solving", "relevant_alternative"],
            "fact_hash_definition": "sha256(canonical JSON fact record; UTF-8, sorted keys, compact separators)",
        },
        "development": {
            "item_count": len(items),
            "unique_task_count": task_count,
            "judgment_count": sum(grade_counts.values()),
            "grade_counts": {grade: grade_counts[grade] for grade in GRADES},
            "evidence_counts": {evidence: evidence_counts[evidence] for evidence in EVIDENCE_TYPES},
            "source_facts_sha256": snapshot["source_facts_sha256"],
            "source_session_dates_sha256": snapshot["source_session_dates_sha256"],
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
    if not any(item.get("null_query") is True for item in items if isinstance(item, dict)):
        raise DatasetError("development dataset has no null query")
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

    snapshot_parser = subparsers.add_parser("snapshot", help="extract the labeled fact subset from a pinned full corpus")
    snapshot_parser.add_argument("--labels", type=pathlib.Path, required=True)
    snapshot_parser.add_argument("--facts", type=pathlib.Path, required=True)
    snapshot_parser.add_argument("--session-dates", type=pathlib.Path, required=True)
    snapshot_parser.add_argument("--output", type=pathlib.Path, required=True)
    snapshot_parser.add_argument("--check", action="store_true")

    materialize_parser = subparsers.add_parser("materialize", help="derive the checked-in dataset from labels and fact snapshot")
    materialize_parser.add_argument("--repo", type=pathlib.Path, default=pathlib.Path.cwd())
    materialize_parser.add_argument("--labels", type=pathlib.Path, required=True)
    materialize_parser.add_argument("--inventory", type=pathlib.Path, required=True)
    materialize_parser.add_argument("--snapshot", type=pathlib.Path, required=True)
    materialize_parser.add_argument("--output", type=pathlib.Path, required=True)
    materialize_parser.add_argument("--check", action="store_true")

    validate_parser = subparsers.add_parser("validate", help="fail if the checked-in dataset differs from its sources")
    validate_parser.add_argument("--repo", type=pathlib.Path, default=pathlib.Path.cwd())
    validate_parser.add_argument("--labels", type=pathlib.Path, required=True)
    validate_parser.add_argument("--inventory", type=pathlib.Path, required=True)
    validate_parser.add_argument("--snapshot", type=pathlib.Path, required=True)
    validate_parser.add_argument("--dataset", type=pathlib.Path, required=True)

    args = parser.parse_args()
    try:
        if args.command == "propose":
            print(_render(propose(args.repo.resolve(), args.inventory, args.facts, args.session_dates, args.limit)), end="")
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
        elif args.command == "materialize":
            value = materialize(args.repo.resolve(), args.labels, args.inventory, args.snapshot)
            _write_or_check(args.output, value, args.check)
        else:
            value = materialize(args.repo.resolve(), args.labels, args.inventory, args.snapshot)
            dataset = load_json(args.dataset)
            if not isinstance(dataset, dict):
                raise DatasetError("dataset must be an object")
            validate_dataset(dataset, value)
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
