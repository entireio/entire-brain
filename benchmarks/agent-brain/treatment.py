"""Treatment isolation and task-validity contracts for the agent benchmark.

This module is deliberately deterministic and has no model/runtime dependency so it can be
used by preflight, CI, and evidence verification without running a benchmark agent.
"""

from __future__ import annotations

import datetime as dt
import hashlib
import json
import re
from typing import Any, Callable


TREATMENT_ARMS = {"no_memory", "placebo_packet", "retrieved_memory", "oracle_retrieval"}
QUERY_SOURCES = {"user_query", "oracle_queries"}
REVIEW_DISPOSITIONS = {"approved_symptom_only", "revise", "exclude_oracle_assisted"}
PACKET_START = "<frozen-memory-packet>"
PACKET_END = "</frozen-memory-packet>"


def user_query(task: dict[str, Any]) -> str:
    """Return the agent-visible problem statement, accepting `prompt` only for legacy tasks."""
    value = task.get("user_query", task.get("prompt"))
    if not isinstance(value, str) or not value.strip():
        raise ValueError(f"task {task.get('id', '<unknown>')} requires a non-empty user_query")
    return value.strip()


def query_source(task: dict[str, Any]) -> str:
    """Resolve the declared retrieval source; legacy brain_queries never become implicit input."""
    source = task.get("retrieval_query_source", "user_query")
    if source not in QUERY_SOURCES:
        raise ValueError(f"retrieval_query_source must be one of {sorted(QUERY_SOURCES)}, got {source!r}")
    if source == "oracle_queries":
        queries = task.get("oracle_queries")
        if not isinstance(queries, list) or not queries or any(not isinstance(q, str) or not q.strip() for q in queries):
            raise ValueError("retrieval_query_source=oracle_queries requires non-empty oracle_queries")
    return source


def retrieval_query(task: dict[str, Any], source: str | None = None) -> str:
    """Return harness-owned retrieval input. `brain_queries` are intentionally ignored."""
    source = source or query_source(task)
    if source not in QUERY_SOURCES:
        raise ValueError(f"retrieval query source must be one of {sorted(QUERY_SOURCES)}")
    if source == "oracle_queries":
        queries = task.get("oracle_queries")
        if not isinstance(queries, list) or not queries:
            raise ValueError("oracle query source requires oracle_queries")
        return " | ".join(str(q).strip() for q in task["oracle_queries"])
    return user_query(task)


def treatment_for_condition(task: dict[str, Any], condition: str) -> dict[str, Any]:
    """Validate and return one condition's explicit treatment declaration.

    Tasks without `treatments` remain runnable for historical reproduction, but are marked legacy
    and cannot pass confirmatory preflight.
    """
    treatments = task.get("treatments")
    if treatments is None:
        arm = "no_memory" if condition == "no_brain" else "retrieved_memory"
        return {"arm": arm, "query_source": "legacy", "confirmatory_eligible": False}
    if not isinstance(treatments, dict) or not treatments:
        raise ValueError("treatments must be a non-empty condition-to-treatment object")
    raw = treatments.get(condition)
    if not isinstance(raw, dict):
        raise ValueError(f"condition {condition!r} has no explicit treatment declaration")
    unknown = sorted(set(raw) - {
        "arm", "query_source", "seed", "candidate_facts", "solving_fact_ids", "near_duplicate_texts"
    })
    if unknown:
        raise ValueError(f"treatment for {condition!r} has unknown fields: {unknown}")
    arm = raw.get("arm")
    if arm not in TREATMENT_ARMS:
        raise ValueError(f"treatment arm must be one of {sorted(TREATMENT_ARMS)}, got {arm!r}")
    source = raw.get("query_source")
    if source not in QUERY_SOURCES:
        raise ValueError(f"treatment query_source must be one of {sorted(QUERY_SOURCES)}")
    if arm == "oracle_retrieval" and source != "oracle_queries":
        raise ValueError("oracle_retrieval requires query_source=oracle_queries")
    if arm != "oracle_retrieval" and source == "oracle_queries":
        raise ValueError("oracle_queries may only feed the oracle_retrieval arm")
    if source == "oracle_queries":
        retrieval_query(task, source)
    if arm == "placebo_packet":
        if not isinstance(raw.get("seed"), (str, int)) or isinstance(raw.get("seed"), bool):
            raise ValueError("placebo_packet requires a deterministic string or integer seed")
        facts = raw.get("candidate_facts")
        if not isinstance(facts, list) or not facts:
            raise ValueError("placebo_packet requires non-empty candidate_facts")
    return {**raw, "confirmatory_eligible": arm != "oracle_retrieval"}


def treatment_schema_errors(task: dict[str, Any], conditions: list[str] | set[str]) -> list[str]:
    errors: list[str] = []
    if task.get("memory_delivery") != "frozen_brief":
        errors.append("explicit comparable treatments require memory_delivery=frozen_brief")
    if "user_query" not in task:
        errors.append("explicit schema requires user_query (legacy prompt is exploratory-only)")
    if "retrieval_query_source" not in task:
        errors.append("explicit schema requires retrieval_query_source")
    if "treatments" not in task:
        errors.append("explicit schema requires treatments")
    for condition in sorted(conditions):
        try:
            treatment = treatment_for_condition(task, condition)
            if treatment["arm"] == "oracle_retrieval":
                errors.append(f"condition {condition!r} is oracle_retrieval and is excluded from confirmatory suites")
        except ValueError as exc:
            errors.append(str(exc))
    return errors


def normalize_prompt_for_parity(prompt: str) -> str:
    """Remove only the packet payload block, leaving its stable presence marker."""
    pattern = re.compile(
        rf"{re.escape(PACKET_START)}.*?{re.escape(PACKET_END)}",
        re.DOTALL,
    )
    return pattern.sub(f"{PACKET_START}\n<packet-payload>\n{PACKET_END}", prompt)


def prompt_parity(prompts: dict[str, str]) -> dict[str, Any]:
    normalized = {condition: normalize_prompt_for_parity(prompt) for condition, prompt in prompts.items()}
    hashes = {
        condition: hashlib.sha256(value.encode("utf-8")).hexdigest()
        for condition, value in normalized.items()
    }
    return {"ok": len(set(normalized.values())) <= 1, "normalized_sha256": hashes}


def _fact_id(fact: dict[str, Any]) -> str:
    return str(fact.get("id") or fact.get("fact_id") or "")


def _fact_text(fact: dict[str, Any]) -> str:
    return str(fact.get("text") or fact.get("content") or "")


def packet_fact_ids(packet: str | dict[str, Any]) -> list[str]:
    """Extract stable fact/result IDs from supported search packet shapes."""
    payload = json.loads(packet) if isinstance(packet, str) else packet
    results = payload.get("results", []) if isinstance(payload, dict) else []
    ids: list[str] = []
    for result in results:
        if not isinstance(result, dict):
            continue
        nested_fact = result.get("fact") if isinstance(result.get("fact"), dict) else {}
        provenance = result.get("provenance") if isinstance(result.get("provenance"), dict) else {}
        value = result.get("fact_id") or nested_fact.get("id") or provenance.get("fact_id") or result.get("id")
        if value:
            ids.append(str(value))
    return ids


def _normalized_terms(text: str) -> set[str]:
    return {token for token in re.findall(r"[a-z0-9]+", text.casefold()) if len(token) >= 3}


def _near_duplicate(left: str, right: str) -> bool:
    a, b = _normalized_terms(left), _normalized_terms(right)
    if not a or not b:
        return False
    return len(a & b) / len(a | b) >= 0.72


def _fact_timestamp(fact: dict[str, Any]) -> dt.datetime | None:
    value = fact.get("created_at", fact.get("timestamp"))
    if not value:
        return None
    try:
        parsed = dt.datetime.fromisoformat(str(value).replace("Z", "+00:00"))
    except ValueError:
        return None
    if parsed.tzinfo is None:
        return None
    return parsed.astimezone(dt.UTC)


def generate_placebo_packet(
    candidate_facts: list[dict[str, Any]],
    reference_packet: str,
    *,
    seed: str | int,
    cutoff_at: str,
    solving_fact_ids: list[str] | set[str] = (),
    near_duplicate_texts: list[str] = (),
) -> tuple[str, dict[str, Any]]:
    """Select a deterministic, temporally valid, task-irrelevant size-matched placebo packet."""
    reference = json.loads(reference_packet)
    if not isinstance(reference, dict) or not isinstance(reference.get("results"), list):
        raise ValueError("reference packet must be a JSON object with a results array")
    cutoff = dt.datetime.fromisoformat(cutoff_at.replace("Z", "+00:00"))
    if cutoff.tzinfo is None:
        raise ValueError("placebo cutoff_at must include a timezone")
    cutoff = cutoff.astimezone(dt.UTC)
    blocked = {str(value) for value in solving_fact_ids}
    blocked_texts = [_fact_text(fact) for fact in candidate_facts if _fact_id(fact) in blocked and _fact_text(fact)]
    rejected = {"missing_id_or_text": 0, "solving_fact": 0, "temporally_ineligible": 0, "near_duplicate": 0}
    eligible: list[dict[str, Any]] = []
    for fact in candidate_facts:
        fact_id, text = _fact_id(fact), _fact_text(fact)
        if not fact_id or not text:
            rejected["missing_id_or_text"] += 1
            continue
        if fact_id in blocked:
            rejected["solving_fact"] += 1
            continue
        timestamp = _fact_timestamp(fact)
        if timestamp is None or timestamp >= cutoff:
            rejected["temporally_ineligible"] += 1
            continue
        if any(_near_duplicate(text, other) for other in [*near_duplicate_texts, *blocked_texts]):
            rejected["near_duplicate"] += 1
            continue
        eligible.append(fact)
    eligible.sort(key=lambda fact: hashlib.sha256(f"{seed}\0{_fact_id(fact)}".encode()).hexdigest())
    target_count = len(reference["results"])
    if len(eligible) < target_count:
        raise ValueError(f"placebo pool has {len(eligible)} eligible facts, needs {target_count}")
    selected = eligible[:target_count]
    payload = {key: value for key, value in reference.items() if key != "results"}
    payload["results"] = [dict(fact) for fact in selected]
    rendered = json.dumps(payload, ensure_ascii=False, separators=(",", ":"), sort_keys=True)
    target_bytes = len(reference_packet.encode("utf-8"))
    delta = target_bytes - len(rendered.encode("utf-8"))
    if delta > 0 and payload["results"]:
        # Whitespace padding changes no fact meaning and makes the packet byte-matched without
        # adding a treatment-revealing metadata field to the agent-visible format.
        payload["results"][-1]["text"] = _fact_text(payload["results"][-1]) + (" " * delta)
        rendered = json.dumps(payload, ensure_ascii=False, separators=(",", ":"), sort_keys=True)
    elif delta < 0 and payload["results"]:
        # Truncate only placebo text, from the last result backwards. IDs and fact count remain
        # stable; the provenance record retains the unmodified selected IDs.
        excess = -delta
        for result in reversed(payload["results"]):
            text = _fact_text(result)
            if not text:
                continue
            remove = min(len(text), excess)
            result["text"] = text[:-remove] if remove else text
            excess -= remove
            if excess <= 0:
                break
        rendered = json.dumps(payload, ensure_ascii=False, separators=(",", ":"), sort_keys=True)
    raw = rendered.encode("utf-8")
    tolerance = max(128, target_bytes // 10)
    if abs(len(raw) - target_bytes) > tolerance:
        raise ValueError(
            f"placebo packet cannot be size matched within {tolerance} bytes "
            f"(reference={target_bytes}, placebo={len(raw)})"
        )
    meta = {
        "seed_sha256": hashlib.sha256(str(seed).encode()).hexdigest(),
        "candidate_count": len(candidate_facts),
        "eligible_count": len(eligible),
        "rejected": rejected,
        "fact_ids": [_fact_id(fact) for fact in selected],
        "fact_count": len(selected),
        "bytes": len(raw),
        "reference_bytes": target_bytes,
        "byte_delta": len(raw) - target_bytes,
        "size_match_tolerance_bytes": tolerance,
        "sha256": hashlib.sha256(raw).hexdigest(),
    }
    return rendered, meta


TASK_VALIDITY_PATTERNS: tuple[tuple[str, re.Pattern[str]], ...] = (
    ("edit_location", re.compile(r"(?:[A-Za-z0-9_.-]+/)+[A-Za-z0-9_.-]+|\b(?:function|method|file|class)\s+`?[A-Za-z_]", re.I)),
    ("exact_ordering", re.compile(r"\b(?:before|after)\b.{0,80}\b(?:before|after|resolution|fallback|then)\b|\bonly after\b", re.I)),
    ("replacement_strategy", re.compile(r"\b(?:replace|remove|append|insert|swap)\b.{0,100}\b(?:with|instead|entry|fallback|wrapper)\b", re.I)),
    ("literal_expression_or_value", re.compile(r"`[^`]+`|\b(?:true|false|null|nil)\b|(?<![A-Za-z])\d+(?:\.\d+)?(?![A-Za-z])", re.I)),
    ("hidden_test_invariant", re.compile(r"\b(?:exactly one|must go|must be|regardless of|without disturbing|preserving non-)\b", re.I)),
)


def task_validity_lint(task: dict[str, Any], overlap_audit: Callable[[dict[str, Any]], dict[str, Any]] | None = None) -> dict[str, Any]:
    """Triage answer-bearing user wording. Human review remains mandatory for approval."""
    query = user_query(task)
    findings: list[dict[str, str]] = []
    for kind, pattern in TASK_VALIDITY_PATTERNS:
        match = pattern.search(query)
        if match:
            findings.append({"kind": kind, "excerpt_sha256": hashlib.sha256(match.group(0).encode()).hexdigest()[:16]})
    if overlap_audit is not None:
        probe = dict(task, brain_queries=[query])
        for finding in overlap_audit(probe).get("findings", []):
            findings.append({"kind": "hidden_material_overlap", "source": str(finding.get("where", "unknown"))})
    treatment_oracle = any(
        isinstance(value, dict) and value.get("arm") == "oracle_retrieval"
        for value in (task.get("treatments") or {}).values()
    ) if isinstance(task.get("treatments"), dict) else False
    oracle = query_source(task) == "oracle_queries" or treatment_oracle or bool(findings)
    return {
        "schema": 1,
        "task_id": task.get("id"),
        "prompt_sha256": hashlib.sha256(query.encode()).hexdigest(),
        "automated_disposition": "oracle_assisted" if oracle else "needs_human_review",
        "confirmatory_eligible": False,
        "findings": findings,
        "note": "automated lint is triage; approved_symptom_only requires a human review-ledger entry",
    }


def validate_review_ledger(ledger: dict[str, Any], tasks: dict[str, dict[str, Any]]) -> list[str]:
    errors: list[str] = []
    if ledger.get("schema_version") != 1 or not isinstance(ledger.get("reviews"), list):
        return ["review ledger requires schema_version=1 and a reviews array"]
    seen: set[str] = set()
    for index, review in enumerate(ledger["reviews"]):
        prefix = f"reviews[{index}]"
        if not isinstance(review, dict):
            errors.append(f"{prefix} must be an object")
            continue
        task_id = review.get("task_id")
        if not isinstance(task_id, str) or not task_id:
            errors.append(f"{prefix}.task_id is required")
            continue
        if task_id in seen:
            errors.append(f"duplicate review for {task_id}")
        seen.add(task_id)
        if not isinstance(review.get("reviewer"), str) or not review["reviewer"].strip():
            errors.append(f"{prefix}.reviewer is required")
        if review.get("disposition") not in REVIEW_DISPOSITIONS:
            errors.append(f"{prefix}.disposition must be one of {sorted(REVIEW_DISPOSITIONS)}")
        if not isinstance(review.get("rationale"), str) or not review["rationale"].strip():
            errors.append(f"{prefix}.rationale is required")
        try:
            reviewed_at = dt.datetime.fromisoformat(str(review.get("reviewed_at", "")).replace("Z", "+00:00"))
            if reviewed_at.tzinfo is None:
                raise ValueError
        except ValueError:
            errors.append(f"{prefix}.reviewed_at must be timezone-aware ISO-8601")
        task = tasks.get(task_id)
        if task is not None:
            expected = hashlib.sha256(user_query(task).encode()).hexdigest()
            if review.get("prompt_sha256") != expected:
                errors.append(f"{prefix}.prompt_sha256 does not match task text")
    return errors
