#!/usr/bin/env python3
"""Evaluate authenticated, text-free offline relevance ranked runs.

The evaluator consumes a separately verified development-query fixture plus
public dataset, source-membership, engine-matrix, and engine-pin bytes.  Its
ranked-run and report artifacts retain identifiers, categorical provenance,
and numeric telemetry only; query and fact text are never copied into either
artifact.

No retrieval engine or model is invoked here.  This module is a deterministic
scoring and selection lane for already-produced ranked IDs.
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
from statistics import fmean
from typing import Any, Iterable


SCHEMA_VERSION = 1
RANKED_RUN_SCHEMA = "agent-brain-offline-relevance-ranked-run/v1"
REPORT_SCHEMA = "agent-brain-offline-relevance-report/v1"
METRIC_SCHEMA = "agent-brain-offline-relevance-metrics/v1"
SELECTION_SCHEMA = "agent-brain-offline-relevance-selection/v1"

K_CANDIDATES = (5, 8, 10, 12)
MRR_CUTOFF = 12
NDCG_CUTOFF = 10
AGGREGATION_CANDIDATES = (
    "best_rank_then_hit_count_v1",
    "hit_count_then_best_rank_v1",
)
POSITIVE_GRADES = ("solving", "relevant_alternative")
GRADE_WEIGHTS = {
    "solving": 3,
    "relevant_alternative": 2,
    "hard_topical_distractor": 0,
    "irrelevant": 0,
}
QUERY_SOURCES = ("user_prompt_derived", "oracle_upper_bound")
THRESHOLDS = {
    "answerable_product_recall_at_5_min": 0.80,
    "null_query_false_positive_rate_max": 0.10,
    "temporal_leakage_max": 0,
    "selected_packet_cluster_occupancy_max": 1,
}
TIE_BREAK_ORDER = (
    "threshold_pass_desc",
    "answerable_product_recall_at_5_desc",
    "null_query_false_positive_rate_asc",
    "temporal_leakage_count_asc",
    "maximum_cluster_occupancy_asc",
    "answerable_product_mrr_desc",
    "answerable_product_ndcg_at_10_desc",
    "product_precision_at_k_desc",
    "packet_tokens_total_asc",
    "k_asc",
    "aggregation_contract_order",
    "run_id_lexicographic",
)

SHA256_RE = re.compile(r"[0-9a-f]{64}")
SAFE_ID_RE = re.compile(r"[A-Za-z0-9][A-Za-z0-9._:/@+\-]{0,255}")
RFC3339_RE = re.compile(
    r"(?P<date>\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})"
    r"(?:\.(?P<fraction>\d{1,9}))?"
    r"(?P<zone>Z|[+-]\d{2}:\d{2})"
)

RUN_FIELDS = {
    "schema_version",
    "schema",
    "run_id",
    "created_at",
    "input_bindings",
    "engine_identity",
    "aggregation_rule",
    "k_candidates",
    "queries",
    "run_sha256",
}
INPUT_BINDING_FIELDS = {
    "engine_matrix_sha256",
    "matrix_id",
    "engine_pins_sha256",
    "pin_set_id",
    "fixture_sha256",
    "fixture_id",
    "dataset_sha256",
    "dataset_id",
    "source_membership_sha256",
    "eligibility_membership_root_sha256",
    "k_candidates",
}
ENGINE_IDENTITY_FIELDS = {
    "arm_id",
    "effective_engine",
    "semantic",
    "binary_sha256",
    "binary_size_bytes",
    "vector_namespace",
    "vector_artifact_sha256",
    "vector_artifact_size_bytes",
    "embedding_dimension",
    "fallback_used",
}
QUERY_FIELDS = {
    "query_id",
    "task_id",
    "query_source",
    "null_query",
    "telemetry",
    "ordered_results",
}
TELEMETRY_FIELDS = {
    "active_fact_count",
    "eligible_fact_count",
    "temporal_exclusion_count",
    "delivered_count",
    "ranking_latency_ms",
}
RESULT_FIELDS = {"fact_id", "kind", "cluster_id", "eligible", "estimated_tokens"}


class EvaluationError(ValueError):
    """An input or derived offline-relevance contract failed closed."""


def canonical_json(value: Any) -> bytes:
    return json.dumps(
        value, sort_keys=True, separators=(",", ":"), ensure_ascii=False, allow_nan=False
    ).encode("utf-8")


def sha256_bytes(value: bytes) -> str:
    return hashlib.sha256(value).hexdigest()


def _self_hash(value: dict[str, Any], field: str) -> str:
    payload = dict(value)
    payload.pop(field, None)
    return sha256_bytes(canonical_json(payload))


def finalize_ranked_run(value: dict[str, Any]) -> dict[str, Any]:
    result = copy.deepcopy(value)
    result["run_sha256"] = _self_hash(result, "run_sha256")
    return result


def finalize_report(value: dict[str, Any]) -> dict[str, Any]:
    result = copy.deepcopy(value)
    result["report_sha256"] = _self_hash(result, "report_sha256")
    return result


def _reject_constant(value: str) -> None:
    raise EvaluationError(f"non-finite JSON number {value!r} is prohibited")


def _unique_object(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise EvaluationError("duplicate JSON object key")
        result[key] = value
    return result


def decode_json(raw: bytes, label: str) -> Any:
    try:
        value = json.loads(
            raw.decode("utf-8"),
            object_pairs_hook=_unique_object,
            parse_constant=_reject_constant,
        )
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise EvaluationError(f"{label} is not valid UTF-8 JSON: {exc}") from exc
    _reject_nonfinite(value, label)
    return value


def _reject_nonfinite(value: Any, label: str) -> None:
    if isinstance(value, float) and not math.isfinite(value):
        raise EvaluationError(f"{label} contains a non-finite number")
    if isinstance(value, dict):
        for child in value.values():
            _reject_nonfinite(child, label)
    elif isinstance(value, list):
        for child in value:
            _reject_nonfinite(child, label)


def _object(value: Any, label: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise EvaluationError(f"{label} must be an object")
    return value


def _array(value: Any, label: str) -> list[Any]:
    if not isinstance(value, list):
        raise EvaluationError(f"{label} must be an array")
    return value


def _exact_fields(value: dict[str, Any], expected: set[str], label: str) -> None:
    if set(value) != expected:
        raise EvaluationError(
            f"{label} fields differ (missing_count={len(expected - set(value))}, "
            f"extra_count={len(set(value) - expected)})"
        )


def _nonempty_string(value: Any, label: str) -> str:
    if not isinstance(value, str) or not value:
        raise EvaluationError(f"{label} must be a non-empty string")
    return value


def _safe_id(value: Any, label: str) -> str:
    text = _nonempty_string(value, label)
    if SAFE_ID_RE.fullmatch(text) is None:
        raise EvaluationError(f"{label} is not a bounded identifier")
    return text


def _sha256(value: Any, label: str) -> str:
    if not isinstance(value, str) or SHA256_RE.fullmatch(value) is None:
        raise EvaluationError(f"{label} must be a lowercase SHA-256")
    return value


def _nonnegative_int(value: Any, label: str) -> int:
    if not isinstance(value, int) or isinstance(value, bool) or value < 0:
        raise EvaluationError(f"{label} must be a non-negative integer")
    return value


def _positive_int(value: Any, label: str) -> int:
    result = _nonnegative_int(value, label)
    if result == 0:
        raise EvaluationError(f"{label} must be positive")
    return result


def _finite_nonnegative(value: Any, label: str) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise EvaluationError(f"{label} must be a finite non-negative number")
    result = float(value)
    if not math.isfinite(result) or result < 0:
        raise EvaluationError(f"{label} must be a finite non-negative number")
    return result


def _timestamp(value: Any, label: str) -> tuple[str, int]:
    text = _nonempty_string(value, label)
    match = RFC3339_RE.fullmatch(text)
    if match is None:
        raise EvaluationError(f"{label} must use the frozen RFC3339 profile")
    try:
        local = dt.datetime.strptime(match.group("date"), "%Y-%m-%dT%H:%M:%S")
        zone = match.group("zone")
        if zone == "Z":
            timezone = dt.timezone.utc
        else:
            sign = 1 if zone[0] == "+" else -1
            hours, minutes = (int(part) for part in zone[1:].split(":"))
            if hours > 23 or minutes > 59:
                raise ValueError("timezone offset is outside RFC3339 bounds")
            timezone = dt.timezone(sign * dt.timedelta(hours=hours, minutes=minutes))
        parsed = local.replace(tzinfo=timezone).astimezone(dt.timezone.utc)
    except ValueError as exc:
        raise EvaluationError(f"{label} is not a valid timestamp") from exc
    epoch = dt.datetime(1970, 1, 1, tzinfo=dt.timezone.utc)
    delta = parsed - epoch
    whole_seconds = delta.days * 86400 + delta.seconds
    fractional_digits = (match.group("fraction") or "").ljust(9, "0")
    nanoseconds = whole_seconds * 1_000_000_000 + int(fractional_digits or "0")
    return text, nanoseconds


def _mean(values: Iterable[float]) -> float:
    materialized = list(values)
    return round(fmean(materialized), 12) if materialized else 0.0


def _ratio(numerator: int, denominator: int) -> float:
    return round(numerator / denominator, 12) if denominator else 0.0


def _membership_root(records: list[dict[str, Any]]) -> str:
    normalized = sorted(
        (
            {
                "fact_id": record["fact_id"],
                "fact_sha256": record["fact_sha256"],
                "status": record["status"],
                "provenance_session_ids": record["provenance_session_ids"],
            }
            for record in records
        ),
        key=lambda record: record["fact_id"],
    )
    return sha256_bytes(canonical_json(normalized))


def _validate_membership(value: Any) -> dict[str, Any]:
    membership = _object(value, "source membership")
    facts_value = _array(membership.get("facts"), "source membership facts")
    sessions_value = _object(membership.get("session_dates"), "source membership session dates")
    facts: list[dict[str, Any]] = []
    seen_ids: set[str] = set()
    for index, raw in enumerate(facts_value):
        record = _object(raw, f"source membership fact[{index}]")
        fact_id = _safe_id(record.get("fact_id"), f"source membership fact[{index}].fact_id")
        if fact_id in seen_ids:
            raise EvaluationError("source membership contains duplicate fact IDs")
        seen_ids.add(fact_id)
        provenance = _array(
            record.get("provenance_session_ids"),
            f"source membership fact[{index}].provenance_session_ids",
        )
        session_ids = [
            _safe_id(item, f"source membership fact[{index}] provenance ID")
            for item in provenance
        ]
        if session_ids != sorted(set(session_ids)):
            raise EvaluationError("source membership provenance IDs are not canonical")
        facts.append(
            {
                "fact_id": fact_id,
                "fact_sha256": _sha256(
                    record.get("fact_sha256"), f"source membership fact[{index}].fact_sha256"
                ),
                "status": _safe_id(
                    record.get("status"), f"source membership fact[{index}].status"
                ),
                "provenance_session_ids": session_ids,
            }
        )
    session_dates: dict[str, int] = {}
    for session_id, raw_date in sessions_value.items():
        safe_session = _safe_id(session_id, "source membership session ID")
        _, parsed = _timestamp(raw_date, "source membership session date")
        session_dates[safe_session] = parsed
    expected_fact_count = _nonnegative_int(
        membership.get("fact_count"), "source membership fact_count"
    )
    expected_active_count = _nonnegative_int(
        membership.get("active_fact_count"), "source membership active_fact_count"
    )
    if expected_fact_count != len(facts):
        raise EvaluationError("source membership fact_count differs from records")
    active_count = sum(record["status"] == "active" for record in facts)
    if expected_active_count != active_count:
        raise EvaluationError("source membership active_fact_count differs from records")
    expected_root = _sha256(
        membership.get("eligibility_membership_root_sha256"),
        "source membership eligibility root",
    )
    if _membership_root(facts) != expected_root:
        raise EvaluationError("source membership eligibility root mismatch")
    return {
        "facts": facts,
        "fact_index": {record["fact_id"]: record for record in facts},
        "session_dates": session_dates,
        "active_fact_count": active_count,
        "eligibility_membership_root_sha256": expected_root,
    }


def _validate_fixture(value: Any) -> tuple[dict[str, Any], list[str]]:
    fixture = _object(value, "fixture")
    if fixture.get("schema_version") != 1:
        raise EvaluationError("fixture schema_version must be 1")
    _safe_id(fixture.get("fixture_id"), "fixture_id")
    expected_hash = _sha256(fixture.get("fixture_sha256"), "fixture_sha256")
    if _self_hash(fixture, "fixture_sha256") != expected_hash:
        raise EvaluationError("fixture self-hash mismatch")
    items = _array(fixture.get("items"), "fixture items")
    by_id: dict[str, dict[str, Any]] = {}
    order: list[str] = []
    for index, raw in enumerate(items):
        item = _object(raw, f"fixture item[{index}]")
        query_id = _safe_id(item.get("query_id"), f"fixture item[{index}].query_id")
        if query_id in by_id:
            raise EvaluationError("fixture contains duplicate query IDs")
        source = item.get("query_source")
        if source not in QUERY_SOURCES:
            raise EvaluationError("fixture query_source is unsupported")
        if not isinstance(item.get("null_query"), bool):
            raise EvaluationError("fixture null_query must be boolean")
        cutoff_text, cutoff = _timestamp(
            item.get("temporal_cutoff"), f"fixture item[{index}].temporal_cutoff"
        )
        excluded_raw = _array(
            item.get("exclude_session_ids"), f"fixture item[{index}].exclude_session_ids"
        )
        excluded = [
            _safe_id(value, f"fixture item[{index}] excluded session")
            for value in excluded_raw
        ]
        if excluded != sorted(set(excluded)):
            raise EvaluationError("fixture excluded session IDs are not canonical")
        by_id[query_id] = {
            "query_id": query_id,
            "task_id": _safe_id(item.get("task_id"), f"fixture item[{index}].task_id"),
            "query_source": source,
            "null_query": item["null_query"],
            "temporal_cutoff": cutoff_text,
            "cutoff": cutoff,
            "exclude_session_ids": excluded,
        }
        order.append(query_id)
    if not order:
        raise EvaluationError("fixture must contain queries")
    return by_id, order


def _validate_dataset(value: Any, fixture_index: dict[str, Any], fixture_order: list[str]) -> dict[str, Any]:
    dataset = _object(value, "dataset")
    if dataset.get("schema_version") != 2:
        raise EvaluationError("dataset schema_version must be 2")
    _safe_id(dataset.get("dataset_id"), "dataset_id")
    development = _object(dataset.get("development"), "dataset development")
    items = _array(development.get("items"), "dataset development items")
    by_id: dict[str, dict[str, Any]] = {}
    order: list[str] = []
    product_answerable = product_null = oracle = 0
    for index, raw in enumerate(items):
        item = _object(raw, f"dataset item[{index}]")
        query_id = _safe_id(item.get("query_id"), f"dataset item[{index}].query_id")
        if query_id in by_id:
            raise EvaluationError("dataset contains duplicate query IDs")
        fixture_item = fixture_index.get(query_id)
        if fixture_item is None:
            raise EvaluationError("dataset query set differs from fixture")
        task_id = _safe_id(item.get("task_id"), f"dataset item[{index}].task_id")
        source = item.get("query_source")
        null_query = item.get("null_query")
        if source not in QUERY_SOURCES or not isinstance(null_query, bool):
            raise EvaluationError("dataset query metadata is malformed")
        if (
            task_id != fixture_item["task_id"]
            or source != fixture_item["query_source"]
            or null_query != fixture_item["null_query"]
        ):
            raise EvaluationError("dataset query metadata differs from fixture")
        judgments_raw = _array(item.get("judgments"), f"dataset item[{index}].judgments")
        judgments: dict[str, dict[str, Any]] = {}
        positives: set[str] = set()
        for judgment_index, raw_judgment in enumerate(judgments_raw):
            judgment = _object(
                raw_judgment, f"dataset item[{index}].judgment[{judgment_index}]"
            )
            fact_id = _safe_id(
                judgment.get("fact_id"),
                f"dataset item[{index}].judgment[{judgment_index}].fact_id",
            )
            if fact_id in judgments:
                raise EvaluationError("dataset contains duplicate judgment fact IDs")
            grade = judgment.get("grade")
            if grade not in GRADE_WEIGHTS:
                raise EvaluationError("dataset contains an unknown relevance grade")
            normalized = {
                "fact_id": fact_id,
                "grade": grade,
                "kind": _safe_id(
                    judgment.get("kind"),
                    f"dataset item[{index}].judgment[{judgment_index}].kind",
                ),
                "cluster_id": _safe_id(
                    judgment.get("cluster_id"),
                    f"dataset item[{index}].judgment[{judgment_index}].cluster_id",
                ),
            }
            judgments[fact_id] = normalized
            if grade in POSITIVE_GRADES:
                positives.add(fact_id)
        if null_query and positives:
            raise EvaluationError("null query contains a positive judgment")
        if not null_query and not positives:
            raise EvaluationError("answerable query contains no positive judgment")
        if source == "user_prompt_derived":
            if null_query:
                product_null += 1
            else:
                product_answerable += 1
        else:
            oracle += 1
        by_id[query_id] = {
            "query_id": query_id,
            "task_id": task_id,
            "query_source": source,
            "null_query": null_query,
            "judgments": judgments,
            "positive_ids": positives,
        }
        order.append(query_id)
    if order != fixture_order:
        raise EvaluationError("dataset query order differs from fixture")
    if product_answerable == 0 or product_null == 0 or oracle == 0:
        raise EvaluationError(
            "dataset must contain answerable product, corpus-closed product-null, and oracle queries"
        )
    return {"items": by_id, "order": order}


def _eligible(
    fact: dict[str, Any], fixture_item: dict[str, Any], session_dates: dict[str, int]
) -> bool:
    if fact["status"] != "active" or not fact["provenance_session_ids"]:
        return False
    excluded = set(fixture_item["exclude_session_ids"])
    cutoff = fixture_item["cutoff"]
    for session_id in fact["provenance_session_ids"]:
        if session_id in excluded:
            return False
        created = session_dates.get(session_id)
        if created is None or not created < cutoff:
            return False
    return True


def _validate_judgment_eligibility(
    dataset: dict[str, Any],
    fixture_index: dict[str, Any],
    membership: dict[str, Any],
) -> None:
    for query_id in dataset["order"]:
        fixture_item = fixture_index[query_id]
        for fact_id in dataset["items"][query_id]["judgments"]:
            fact = membership["fact_index"].get(fact_id)
            if fact is None:
                raise EvaluationError("dataset judgment is absent from source membership")
            if not _eligible(fact, fixture_item, membership["session_dates"]):
                raise EvaluationError(
                    "dataset judgment is outside authenticated temporal eligibility"
                )


def _build_input_bindings(
    engine_matrix_raw: bytes,
    engine_matrix: dict[str, Any],
    engine_pins_raw: bytes,
    engine_pins: dict[str, Any],
    fixture: dict[str, Any],
    dataset_raw: bytes,
    dataset: dict[str, Any],
    membership_raw: bytes,
    membership: dict[str, Any],
) -> dict[str, Any]:
    return {
        "engine_matrix_sha256": sha256_bytes(engine_matrix_raw),
        "matrix_id": _safe_id(engine_matrix.get("matrix_id"), "matrix_id"),
        "engine_pins_sha256": sha256_bytes(engine_pins_raw),
        "pin_set_id": _safe_id(engine_pins.get("pin_set_id"), "pin_set_id"),
        "fixture_sha256": _sha256(fixture.get("fixture_sha256"), "fixture_sha256"),
        "fixture_id": _safe_id(fixture.get("fixture_id"), "fixture_id"),
        "dataset_sha256": sha256_bytes(dataset_raw),
        "dataset_id": _safe_id(dataset.get("dataset_id"), "dataset_id"),
        "source_membership_sha256": sha256_bytes(membership_raw),
        "eligibility_membership_root_sha256": _sha256(
            membership.get("eligibility_membership_root_sha256"),
            "eligibility membership root",
        ),
        "k_candidates": list(K_CANDIDATES),
    }


def _engine_arms(engine_matrix: dict[str, Any]) -> tuple[list[str], dict[str, dict[str, Any]]]:
    arms_value = _array(engine_matrix.get("arms"), "engine matrix arms")
    order: list[str] = []
    by_id: dict[str, dict[str, Any]] = {}
    for index, raw in enumerate(arms_value):
        arm = _object(raw, f"engine matrix arm[{index}]")
        arm_id = _safe_id(arm.get("id"), f"engine matrix arm[{index}].id")
        if arm_id in by_id:
            raise EvaluationError("engine matrix contains duplicate arm IDs")
        semantic = arm.get("semantic")
        if not isinstance(semantic, bool):
            raise EvaluationError("engine matrix arm semantic must be boolean")
        by_id[arm_id] = {
            "id": arm_id,
            "semantic": semantic,
            "effective_engine_required": _safe_id(
                arm.get("effective_engine_required"), "effective engine requirement"
            ),
            "namespace": _safe_id(arm.get("namespace"), "engine namespace"),
        }
        order.append(arm_id)
    if not order:
        raise EvaluationError("engine matrix must contain arms")
    return order, by_id


def _validate_engine_identity(
    value: Any,
    arm_contracts: dict[str, dict[str, Any]],
    engine_pins: dict[str, Any],
) -> dict[str, Any]:
    identity = _object(value, "engine identity")
    _exact_fields(identity, ENGINE_IDENTITY_FIELDS, "engine identity")
    arm_id = _safe_id(identity.get("arm_id"), "engine identity arm_id")
    contract = arm_contracts.get(arm_id)
    if contract is None:
        raise EvaluationError("engine identity arm is absent from the matrix")
    if identity.get("effective_engine") != contract["effective_engine_required"]:
        raise EvaluationError("effective engine differs from matrix")
    if identity.get("semantic") is not contract["semantic"]:
        raise EvaluationError("semantic identity differs from matrix")
    if identity.get("vector_namespace") != contract["namespace"]:
        raise EvaluationError("vector namespace differs from matrix")
    if identity.get("fallback_used") is not False:
        raise EvaluationError("engine fallback invalidates the ranked run")
    binary = _object(engine_pins.get("binary"), "engine pins binary")
    if identity.get("binary_sha256") != _sha256(binary.get("binary_sha256"), "pinned binary"):
        raise EvaluationError("binary identity differs from engine pins")
    if identity.get("binary_size_bytes") != _positive_int(
        binary.get("binary_size_bytes"), "pinned binary size"
    ):
        raise EvaluationError("binary size differs from engine pins")
    engines = _object(engine_pins.get("engines"), "engine pins engines")
    pinned_engine = _object(engines.get(arm_id), "pinned engine identity")
    dimension = pinned_engine.get("dimension")
    if contract["semantic"]:
        _sha256(identity.get("vector_artifact_sha256"), "vector artifact SHA-256")
        _positive_int(identity.get("vector_artifact_size_bytes"), "vector artifact size")
        if identity.get("embedding_dimension") != _positive_int(
            dimension, "pinned embedding dimension"
        ):
            raise EvaluationError("embedding dimension differs from engine pins")
    elif any(
        identity.get(field) is not None
        for field in (
            "vector_artifact_sha256",
            "vector_artifact_size_bytes",
            "embedding_dimension",
        )
    ):
        raise EvaluationError("lexical engine must not claim a vector artifact")
    return copy.deepcopy(identity)


def _validate_telemetry(
    value: Any,
    *,
    active_count: int,
    eligible_count: int,
    delivered_count: int,
) -> dict[str, Any]:
    telemetry = _object(value, "query telemetry")
    _exact_fields(telemetry, TELEMETRY_FIELDS, "query telemetry")
    if _nonnegative_int(telemetry.get("active_fact_count"), "active_fact_count") != active_count:
        raise EvaluationError("query active_fact_count differs from membership")
    if (
        _nonnegative_int(telemetry.get("eligible_fact_count"), "eligible_fact_count")
        != eligible_count
    ):
        raise EvaluationError("query eligible_fact_count differs from temporal policy")
    if (
        _nonnegative_int(
            telemetry.get("temporal_exclusion_count"), "temporal_exclusion_count"
        )
        != active_count - eligible_count
    ):
        raise EvaluationError("query temporal exclusion count differs")
    if (
        _nonnegative_int(telemetry.get("delivered_count"), "delivered_count")
        != delivered_count
    ):
        raise EvaluationError("query delivered_count differs from ordered results")
    latency = _finite_nonnegative(telemetry.get("ranking_latency_ms"), "ranking_latency_ms")
    result = copy.deepcopy(telemetry)
    result["ranking_latency_ms"] = latency
    return result


def _validate_run(
    value: Any,
    *,
    expected_bindings: dict[str, Any],
    engine_matrix: dict[str, Any],
    engine_pins: dict[str, Any],
    fixture_index: dict[str, Any],
    fixture_order: list[str],
    dataset_index: dict[str, Any],
    membership: dict[str, Any],
) -> dict[str, Any]:
    run = _object(value, "ranked run")
    _exact_fields(run, RUN_FIELDS, "ranked run")
    if run.get("schema_version") != SCHEMA_VERSION or run.get("schema") != RANKED_RUN_SCHEMA:
        raise EvaluationError("ranked run schema identity is invalid")
    run_id = _safe_id(run.get("run_id"), "run_id")
    _timestamp(run.get("created_at"), "ranked run created_at")
    supplied_hash = _sha256(run.get("run_sha256"), "run_sha256")
    if _self_hash(run, "run_sha256") != supplied_hash:
        raise EvaluationError("ranked run self-hash mismatch")
    bindings = _object(run.get("input_bindings"), "ranked run input bindings")
    _exact_fields(bindings, INPUT_BINDING_FIELDS, "ranked run input bindings")
    if bindings != expected_bindings:
        raise EvaluationError("ranked run input bindings differ from authenticated inputs")
    if run.get("k_candidates") != list(K_CANDIDATES):
        raise EvaluationError("ranked run K candidates differ from the frozen contract")
    aggregation = run.get("aggregation_rule")
    if aggregation not in AGGREGATION_CANDIDATES:
        raise EvaluationError("ranked run aggregation rule is unsupported")
    _, arm_contracts = _engine_arms(engine_matrix)
    identity = _validate_engine_identity(run.get("engine_identity"), arm_contracts, engine_pins)
    query_values = _array(run.get("queries"), "ranked run queries")
    if len(query_values) != len(fixture_order):
        raise EvaluationError("ranked run query count differs from fixture")
    queries: list[dict[str, Any]] = []
    seen_queries: set[str] = set()
    membership_index = membership["fact_index"]
    for index, raw in enumerate(query_values):
        query = _object(raw, f"ranked run query[{index}]")
        _exact_fields(query, QUERY_FIELDS, f"ranked run query[{index}]")
        query_id = _safe_id(query.get("query_id"), f"ranked run query[{index}].query_id")
        if query_id in seen_queries:
            raise EvaluationError("ranked run contains duplicate query IDs")
        seen_queries.add(query_id)
        if query_id != fixture_order[index]:
            raise EvaluationError("ranked run query order differs from fixture")
        fixture_item = fixture_index[query_id]
        dataset_item = dataset_index[query_id]
        if (
            query.get("task_id") != fixture_item["task_id"]
            or query.get("query_source") != fixture_item["query_source"]
            or query.get("null_query") is not fixture_item["null_query"]
        ):
            raise EvaluationError("ranked run query metadata differs from fixture")
        eligible_ids = {
            fact["fact_id"]
            for fact in membership["facts"]
            if _eligible(fact, fixture_item, membership["session_dates"])
        }
        results_raw = _array(query.get("ordered_results"), "ordered results")
        if len(results_raw) > max(K_CANDIDATES):
            raise EvaluationError("ranked run retains more results than the maximum K")
        results: list[dict[str, Any]] = []
        seen_facts: set[str] = set()
        for result_index, raw_result in enumerate(results_raw):
            result = _object(raw_result, f"ordered result[{result_index}]")
            _exact_fields(result, RESULT_FIELDS, f"ordered result[{result_index}]")
            fact_id = _safe_id(result.get("fact_id"), f"ordered result[{result_index}].fact_id")
            if fact_id in seen_facts:
                raise EvaluationError("ranked run contains duplicate result fact IDs")
            seen_facts.add(fact_id)
            fact = membership_index.get(fact_id)
            if fact is None:
                raise EvaluationError("ranked run result is absent from source membership")
            derived_eligible = fact_id in eligible_ids
            if result.get("eligible") is not derived_eligible:
                raise EvaluationError("ranked result eligibility differs from authenticated policy")
            if not derived_eligible:
                raise EvaluationError("ranked run contains temporal leakage")
            kind = _safe_id(result.get("kind"), f"ordered result[{result_index}].kind")
            cluster_id = _safe_id(
                result.get("cluster_id"), f"ordered result[{result_index}].cluster_id"
            )
            judgment = dataset_item["judgments"].get(fact_id)
            if judgment is not None and (
                judgment["kind"] != kind or judgment["cluster_id"] != cluster_id
            ):
                raise EvaluationError("ranked result metadata differs from reviewed judgment")
            results.append(
                {
                    "fact_id": fact_id,
                    "kind": kind,
                    "cluster_id": cluster_id,
                    "eligible": True,
                    "estimated_tokens": _nonnegative_int(
                        result.get("estimated_tokens"),
                        f"ordered result[{result_index}].estimated_tokens",
                    ),
                }
            )
        telemetry = _validate_telemetry(
            query.get("telemetry"),
            active_count=membership["active_fact_count"],
            eligible_count=len(eligible_ids),
            delivered_count=len(results),
        )
        queries.append(
            {
                "query_id": query_id,
                "task_id": fixture_item["task_id"],
                "query_source": fixture_item["query_source"],
                "null_query": fixture_item["null_query"],
                "telemetry": telemetry,
                "ordered_results": results,
            }
        )
    return {
        "schema_version": SCHEMA_VERSION,
        "schema": RANKED_RUN_SCHEMA,
        "run_id": run_id,
        "created_at": run["created_at"],
        "input_bindings": copy.deepcopy(bindings),
        "engine_identity": identity,
        "aggregation_rule": aggregation,
        "k_candidates": list(K_CANDIDATES),
        "queries": queries,
        "run_sha256": supplied_hash,
    }


def _recall(results: list[dict[str, Any]], positives: set[str], cutoff: int) -> float:
    if not positives:
        return 0.0
    hits = {result["fact_id"] for result in results[:cutoff]} & positives
    return _ratio(len(hits), len(positives))


def _mrr(results: list[dict[str, Any]], positives: set[str]) -> float:
    for rank, result in enumerate(results[:MRR_CUTOFF], 1):
        if result["fact_id"] in positives:
            return round(1.0 / rank, 12)
    return 0.0


def _ndcg(results: list[dict[str, Any]], judgments: dict[str, dict[str, Any]]) -> float:
    def gain(weight: int) -> float:
        return float((2**weight) - 1)

    dcg = 0.0
    for rank, result in enumerate(results[:NDCG_CUTOFF], 1):
        judgment = judgments.get(result["fact_id"])
        weight = GRADE_WEIGHTS[judgment["grade"]] if judgment is not None else 0
        dcg += gain(weight) / math.log2(rank + 1)
    ideal_weights = sorted(
        (GRADE_WEIGHTS[judgment["grade"]] for judgment in judgments.values()), reverse=True
    )[:NDCG_CUTOFF]
    idcg = sum(gain(weight) / math.log2(rank + 1) for rank, weight in enumerate(ideal_weights, 1))
    return round(dcg / idcg, 12) if idcg else 0.0


def _query_metrics(
    query: dict[str, Any], dataset_item: dict[str, Any], k: int
) -> dict[str, Any]:
    results = query["ordered_results"]
    top = results[:k]
    positives = dataset_item["positive_ids"]
    judgments = dataset_item["judgments"]
    grades = Counter(
        judgments[result["fact_id"]]["grade"]
        if result["fact_id"] in judgments
        else "unjudged"
        for result in top
    )
    kinds = Counter(result["kind"] for result in top)
    clusters = Counter(result["cluster_id"] for result in top)
    positive_hits = sum(result["fact_id"] in positives for result in top)
    return {
        "query_source": query["query_source"],
        "null_query": query["null_query"],
        "recall_at_1": _recall(results, positives, 1),
        "recall_at_5": _recall(results, positives, 5),
        "recall_at_10": _recall(results, positives, 10),
        "mrr": _mrr(results, positives),
        "ndcg_at_10": _ndcg(results, judgments),
        "precision_at_k": _ratio(positive_hits, k),
        "positive_hits": positive_hits,
        "hard_topical_distractor_hits": grades["hard_topical_distractor"],
        "irrelevant_hits": grades["irrelevant"],
        "unjudged_hits": grades["unjudged"],
        "result_count": len(top),
        "kind_counts": dict(sorted(kinds.items())),
        "maximum_cluster_occupancy": max(clusters.values(), default=0),
        "duplicate_cluster_count": sum(value > 1 for value in clusters.values()),
        "temporal_leakage_count": sum(result["eligible"] is not True for result in top),
        "packet_tokens": sum(result["estimated_tokens"] for result in top),
        "ranking_latency_ms": query["telemetry"]["ranking_latency_ms"],
    }


def _aggregate_metrics(metrics: list[dict[str, Any]], *, product: bool, k: int) -> dict[str, Any]:
    answerable = [metric for metric in metrics if not metric["null_query"]]
    nulls = [metric for metric in metrics if metric["null_query"]]
    kinds: Counter[str] = Counter()
    for metric in metrics:
        kinds.update(metric["kind_counts"])
    false_positive_nulls = sum(metric["result_count"] > 0 for metric in nulls)
    null_rate: float | None = (
        _ratio(false_positive_nulls, len(nulls)) if nulls else None
    )
    result = {
        "query_count": len(metrics),
        "answerable_query_count": len(answerable),
        "null_query_count": len(nulls),
        "mean_recall_at_1": _mean(metric["recall_at_1"] for metric in answerable),
        "mean_recall_at_5": _mean(metric["recall_at_5"] for metric in answerable),
        "mean_recall_at_10": _mean(metric["recall_at_10"] for metric in answerable),
        "mean_mrr": _mean(metric["mrr"] for metric in answerable),
        "mean_ndcg_at_10": _mean(metric["ndcg_at_10"] for metric in answerable),
        "mean_precision_at_k": _mean(metric["precision_at_k"] for metric in metrics),
        "positive_hits": sum(metric["positive_hits"] for metric in metrics),
        "hard_topical_distractor_hits": sum(
            metric["hard_topical_distractor_hits"] for metric in metrics
        ),
        "irrelevant_hits": sum(metric["irrelevant_hits"] for metric in metrics),
        "unjudged_hits": sum(metric["unjudged_hits"] for metric in metrics),
        "result_count": sum(metric["result_count"] for metric in metrics),
        "kind_distribution": dict(sorted(kinds.items())),
        "maximum_cluster_occupancy": max(
            (metric["maximum_cluster_occupancy"] for metric in metrics), default=0
        ),
        "duplicate_cluster_query_count": sum(
            metric["duplicate_cluster_count"] > 0 for metric in metrics
        ),
        "temporal_leakage_count": sum(
            metric["temporal_leakage_count"] for metric in metrics
        ),
        "null_false_positive_numerator": false_positive_nulls,
        "null_false_positive_denominator": len(nulls),
        "null_query_false_positive_rate": null_rate,
        "packet_tokens_total": sum(metric["packet_tokens"] for metric in metrics),
        "ranking_latency_ms_total": round(
            sum(metric["ranking_latency_ms"] for metric in metrics), 12
        ),
        "k": k,
    }
    if product and (not answerable or not nulls):
        raise EvaluationError("product metrics require answerable and null query strata")
    if not product and not answerable:
        raise EvaluationError("oracle metrics require an answerable query")
    return result


def _candidate_metrics(
    run: dict[str, Any], dataset_index: dict[str, Any], k: int
) -> dict[str, Any]:
    product_metrics: list[dict[str, Any]] = []
    oracle_metrics: list[dict[str, Any]] = []
    for query in run["queries"]:
        metric = _query_metrics(query, dataset_index[query["query_id"]], k)
        if query["query_source"] == "user_prompt_derived":
            product_metrics.append(metric)
        else:
            oracle_metrics.append(metric)
    product = _aggregate_metrics(product_metrics, product=True, k=k)
    oracle = _aggregate_metrics(oracle_metrics, product=False, k=k)
    null_rate = product["null_query_false_positive_rate"]
    assert isinstance(null_rate, float)
    threshold_values = {
        "answerable_product_recall_at_5": product["mean_recall_at_5"],
        "null_query_false_positive_rate": null_rate,
        "temporal_leakage_count": product["temporal_leakage_count"]
        + oracle["temporal_leakage_count"],
        "maximum_cluster_occupancy": max(
            product["maximum_cluster_occupancy"], oracle["maximum_cluster_occupancy"]
        ),
    }
    passed = bool(
        threshold_values["answerable_product_recall_at_5"]
        >= THRESHOLDS["answerable_product_recall_at_5_min"]
        and threshold_values["null_query_false_positive_rate"]
        <= THRESHOLDS["null_query_false_positive_rate_max"]
        and threshold_values["temporal_leakage_count"] <= THRESHOLDS["temporal_leakage_max"]
        and threshold_values["maximum_cluster_occupancy"]
        <= THRESHOLDS["selected_packet_cluster_occupancy_max"]
    )
    return {
        "k": k,
        "product": product,
        "oracle": oracle,
        "threshold_values": threshold_values,
        "thresholds_passed": passed,
    }


def metric_contract() -> dict[str, Any]:
    return {
        "schema": METRIC_SCHEMA,
        "positive_grades": list(POSITIVE_GRADES),
        "grade_weights": dict(GRADE_WEIGHTS),
        "unjudged_policy": "zero_gain_and_nonrelevant_development_lower_bound",
        "recall_denominator": "all_explicit_positive_judgments_per_answerable_query",
        "precision_denominator": "fixed_k_with_missing_slots_nonrelevant",
        "mrr_cutoff": MRR_CUTOFF,
        "ndcg_cutoff": NDCG_CUTOFF,
        "ndcg_gain": "two_to_grade_weight_minus_one",
        "null_false_positive_numerator": "product_null_queries_with_any_result_in_top_k",
        "null_false_positive_denominator": "product_null_query_count",
        "temporal_leakage": "any_ranked_fact_not_derived_active_and_eligible_fails_closed",
        "strata": ["user_prompt_derived", "oracle_upper_bound"],
    }


def selection_contract() -> dict[str, Any]:
    return {
        "schema": SELECTION_SCHEMA,
        "k_candidates": list(K_CANDIDATES),
        "aggregation_candidates": list(AGGREGATION_CANDIDATES),
        "thresholds": copy.deepcopy(THRESHOLDS),
        "tie_break_order": list(TIE_BREAK_ORDER),
        "selection_scope": "within_each_verified_engine_no_cross_engine_winner",
    }


def _selection_key(candidate: dict[str, Any]) -> tuple[Any, ...]:
    product = candidate["metrics"]["product"]
    values = candidate["metrics"]["threshold_values"]
    return (
        -int(candidate["metrics"]["thresholds_passed"]),
        -float(values["answerable_product_recall_at_5"]),
        float(values["null_query_false_positive_rate"]),
        int(values["temporal_leakage_count"]),
        int(values["maximum_cluster_occupancy"]),
        -float(product["mean_mrr"]),
        -float(product["mean_ndcg_at_10"]),
        -float(product["mean_precision_at_k"]),
        int(product["packet_tokens_total"]),
        int(candidate["k"]),
        AGGREGATION_CANDIDATES.index(candidate["aggregation_rule"]),
        candidate["run_id"],
    )


def evaluate(
    *,
    engine_matrix_raw: bytes,
    engine_pins_raw: bytes,
    fixture: dict[str, Any],
    dataset_raw: bytes,
    source_membership_raw: bytes,
    ranked_run_raws: list[bytes],
) -> dict[str, Any]:
    """Validate inputs, derive metrics, and return a self-hashed text-free report."""
    engine_matrix = _object(decode_json(engine_matrix_raw, "engine matrix"), "engine matrix")
    engine_pins = _object(decode_json(engine_pins_raw, "engine pins"), "engine pins")
    dataset = _object(decode_json(dataset_raw, "dataset"), "dataset")
    source_membership_value = _object(
        decode_json(source_membership_raw, "source membership"), "source membership"
    )
    fixture_index, fixture_order = _validate_fixture(fixture)
    dataset_contract = _validate_dataset(dataset, fixture_index, fixture_order)
    membership = _validate_membership(source_membership_value)
    _validate_judgment_eligibility(dataset_contract, fixture_index, membership)
    input_bindings = _build_input_bindings(
        engine_matrix_raw,
        engine_matrix,
        engine_pins_raw,
        engine_pins,
        fixture,
        dataset_raw,
        dataset,
        source_membership_raw,
        source_membership_value,
    )
    arm_order, _ = _engine_arms(engine_matrix)
    decoded_runs: list[tuple[bytes, dict[str, Any]]] = []
    run_ids: set[str] = set()
    run_keys: set[tuple[str, str]] = set()
    arm_identities: dict[str, dict[str, Any]] = {}
    for index, raw in enumerate(ranked_run_raws):
        decoded = decode_json(raw, f"ranked run[{index}]")
        run = _validate_run(
            decoded,
            expected_bindings=input_bindings,
            engine_matrix=engine_matrix,
            engine_pins=engine_pins,
            fixture_index=fixture_index,
            fixture_order=fixture_order,
            dataset_index=dataset_contract["items"],
            membership=membership,
        )
        if run["run_id"] in run_ids:
            raise EvaluationError("ranked run IDs are duplicated")
        run_ids.add(run["run_id"])
        key = (run["engine_identity"]["arm_id"], run["aggregation_rule"])
        if key in run_keys:
            raise EvaluationError("engine/aggregation ranked run is duplicated")
        run_keys.add(key)
        prior_identity = arm_identities.setdefault(
            run["engine_identity"]["arm_id"], run["engine_identity"]
        )
        if run["engine_identity"] != prior_identity:
            raise EvaluationError("engine identity differs across aggregation runs")
        decoded_runs.append((raw, run))
    expected_keys = {
        (arm_id, aggregation)
        for arm_id in arm_order
        for aggregation in AGGREGATION_CANDIDATES
    }
    if run_keys != expected_keys:
        raise EvaluationError("ranked runs do not cover the full engine/aggregation matrix")
    arm_position = {arm_id: index for index, arm_id in enumerate(arm_order)}
    decoded_runs.sort(
        key=lambda pair: (
            arm_position[pair[1]["engine_identity"]["arm_id"]],
            AGGREGATION_CANDIDATES.index(pair[1]["aggregation_rule"]),
            pair[1]["run_id"],
        )
    )
    evaluated: list[dict[str, Any]] = []
    selectable: dict[str, list[dict[str, Any]]] = {arm_id: [] for arm_id in arm_order}
    for raw, run in decoded_runs:
        candidates = [
            _candidate_metrics(run, dataset_contract["items"], k) for k in K_CANDIDATES
        ]
        evaluated.append(
            {
                "run_id": run["run_id"],
                "run_sha256": run["run_sha256"],
                "run_file_sha256": sha256_bytes(raw),
                "engine_identity": copy.deepcopy(run["engine_identity"]),
                "aggregation_rule": run["aggregation_rule"],
                "candidates": candidates,
            }
        )
        for metrics in candidates:
            selectable[run["engine_identity"]["arm_id"]].append(
                {
                    "run_id": run["run_id"],
                    "run_sha256": run["run_sha256"],
                    "aggregation_rule": run["aggregation_rule"],
                    "k": metrics["k"],
                    "metrics": metrics,
                }
            )
    selections: list[dict[str, Any]] = []
    for arm_id in arm_order:
        ordered = sorted(selectable[arm_id], key=_selection_key)
        passing = [candidate for candidate in ordered if candidate["metrics"]["thresholds_passed"]]
        if not passing:
            selections.append(
                {
                    "arm_id": arm_id,
                    "status": "no_passing_candidate",
                    "run_id": None,
                    "run_sha256": None,
                    "aggregation_rule": None,
                    "k": None,
                    "selection_sha256": None,
                }
            )
            continue
        selected = passing[0]
        projection = {
            "arm_id": arm_id,
            "run_id": selected["run_id"],
            "run_sha256": selected["run_sha256"],
            "aggregation_rule": selected["aggregation_rule"],
            "k": selected["k"],
        }
        selections.append(
            {
                "arm_id": arm_id,
                "status": "selected",
                **projection,
                "selection_sha256": sha256_bytes(canonical_json(projection)),
            }
        )
    generated_at = max(
        (run["created_at"] for _, run in decoded_runs),
        key=lambda value: (_timestamp(value, "ranked run created_at")[1], value),
    )
    report = {
        "schema_version": SCHEMA_VERSION,
        "schema": REPORT_SCHEMA,
        "report_id": "entire-brain-offline-relevance-development-v1",
        "generated_at": generated_at,
        "input_bindings": input_bindings,
        "metric_contract": metric_contract(),
        "selection_contract": selection_contract(),
        "evaluated_runs": evaluated,
        "selections": selections,
        "report_sha256": "",
    }
    return finalize_report(report)


def _load_public(path: pathlib.Path, label: str) -> bytes:
    try:
        return path.read_bytes()
    except OSError as exc:
        raise EvaluationError(f"cannot read {label}: {exc}") from exc


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo-root", type=pathlib.Path, default=pathlib.Path.cwd())
    parser.add_argument("--ranked-run", action="append", type=pathlib.Path, required=True)
    parser.add_argument("--output", type=pathlib.Path)
    args = parser.parse_args(argv)
    repo = args.repo_root.resolve()
    confirmatory = repo / "benchmarks" / "agent-brain" / "confirmatory"
    sys.path.insert(0, str(confirmatory))
    try:
        import development_relevance_queries as query_fixture  # type: ignore
    except ImportError as exc:
        print(f"ERROR: cannot import verified query fixture loader: {exc}", file=sys.stderr)
        return 1

    try:
        fixture = query_fixture.load_verified_fixture(repo)
        report = evaluate(
            engine_matrix_raw=_load_public(confirmatory / "engine-matrix.json", "engine matrix"),
            engine_pins_raw=_load_public(
                confirmatory / "engine-verification-pins.json", "engine pins"
            ),
            fixture=fixture,
            dataset_raw=_load_public(
                confirmatory / "offline-relevance-dataset.json", "dataset"
            ),
            source_membership_raw=_load_public(
                confirmatory / "offline-relevance-source-membership.json",
                "source membership",
            ),
            ranked_run_raws=[_load_public(path, "ranked run") for path in args.ranked_run],
        )
    except (EvaluationError, query_fixture.FixtureError) as exc:
        print(f"ERROR: {exc}", file=sys.stderr)
        return 1
    output = canonical_json(report) + b"\n"
    if args.output is None:
        sys.stdout.buffer.write(output)
    else:
        args.output.write_bytes(output)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
