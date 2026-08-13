#!/usr/bin/env python3
"""Non-mutating diagnostics for the gated ranking workstream.

This analyzer deliberately does not retrieve facts or alter production scoring.  It consumes
captured per-query ranks plus pre-kind lexical scores, then makes two otherwise easy-to-confuse
effects explicit:

* the rank/top-k effect of the current closed-negative +30 lexical boost; and
* the delivered packet rank produced by best per-query rank, then query hit count.

Input and output are JSON so a future WS6 relevance set can use the same deterministic analysis.
"""

from __future__ import annotations

import argparse
import json
import re
from pathlib import Path
from typing import Any

INPUT_SCHEMA = "entire-brain-ranking-diagnostics-input-v1"
OUTPUT_SCHEMA = "entire-brain-ranking-diagnostics-v1"
DEFAULT_CLOSED_NEGATIVE_BOOST = 30
DEFAULT_CLUSTER_THRESHOLD = 0.8


def _fact_id(fact: dict[str, Any]) -> str:
    fact_id = str(fact.get("id") or "").strip()
    if not fact_id:
        raise ValueError("every diagnostic fact requires a non-empty id")
    return fact_id


def _rank_closed_negative_facts(
    facts: list[dict[str, Any]], boost: int
) -> list[dict[str, Any]]:
    ranked: list[tuple[int, str, int, dict[str, Any]]] = []
    for stable_index, fact in enumerate(facts):
        _fact_id(fact)
        base_score = fact.get("lexical_score_before_kind_boost")
        if not isinstance(base_score, int) or isinstance(base_score, bool):
            raise ValueError("lexical_score_before_kind_boost must be an integer")
        score = base_score
        if base_score > 0 and fact.get("kind") == "closed-negative":
            score += boost
        if score <= 0:
            continue
        updated_at = str(fact.get("updated_at") or "")
        ranked.append((-score, updated_at, stable_index, fact))
    # Production uses score descending, UpdatedAt descending, then stable input order.
    ranked.sort(key=lambda item: (item[0], _descending_text_key(item[1]), item[2]))
    return [fact for _, _, _, fact in ranked]


def _descending_text_key(value: str) -> tuple[int, ...]:
    # RFC3339 timestamps are lexically ordered when normalized. Negated code points give a
    # deterministic descending key without platform locale behavior.
    return tuple(-ord(char) for char in value)


def closed_negative_ablation(
    queries: list[dict[str, Any]], boost: int, top_k: int
) -> dict[str, Any]:
    output_queries: list[dict[str, Any]] = []
    moved = 0
    top_k_changed = 0
    closed_negative_count = 0
    for query in queries:
        query_id = str(query.get("query_id") or "").strip()
        facts = query.get("facts")
        if not query_id or not isinstance(facts, list):
            raise ValueError("each closed_negative_ablation query needs query_id and facts")
        without = _rank_closed_negative_facts(facts, 0)
        with_boost = _rank_closed_negative_facts(facts, boost)
        rank_without = {_fact_id(fact): rank for rank, fact in enumerate(without, 1)}
        rank_with = {_fact_id(fact): rank for rank, fact in enumerate(with_boost, 1)}
        effects: list[dict[str, Any]] = []
        for fact in facts:
            if fact.get("kind") != "closed-negative":
                continue
            closed_negative_count += 1
            fact_id = _fact_id(fact)
            before = rank_without.get(fact_id)
            after = rank_with.get(fact_id)
            delta = None if before is None or after is None else before - after
            changed = delta not in (None, 0)
            membership_changed = (before is not None and before <= top_k) != (
                after is not None and after <= top_k
            )
            moved += int(changed)
            top_k_changed += int(membership_changed)
            effects.append(
                {
                    "fact_id": fact_id,
                    "base_score": fact["lexical_score_before_kind_boost"],
                    "boosted_score": fact["lexical_score_before_kind_boost"] + boost
                    if fact["lexical_score_before_kind_boost"] > 0
                    else fact["lexical_score_before_kind_boost"],
                    "rank_without_boost_1based": before,
                    "rank_with_boost_1based": after,
                    "rank_improvement": delta,
                    "top_k_membership_changed": membership_changed,
                }
            )
        output_queries.append(
            {
                "query_id": query_id,
                "ranked_ids_without_boost": [_fact_id(fact) for fact in without],
                "ranked_ids_with_boost": [_fact_id(fact) for fact in with_boost],
                "closed_negative_effects": effects,
            }
        )
    return {
        "configured_boost": boost,
        "top_k": top_k,
        "query_count": len(output_queries),
        "closed_negative_count": closed_negative_count,
        "closed_negatives_with_rank_change": moved,
        "closed_negatives_with_top_k_membership_change": top_k_changed,
        "queries": output_queries,
    }


def _tokens(text: str) -> set[str]:
    return set(re.findall(r"[a-z0-9]+", text.casefold()))


def _jaccard(left: str, right: str) -> float:
    left_tokens, right_tokens = _tokens(left), _tokens(right)
    union = left_tokens | right_tokens
    return len(left_tokens & right_tokens) / len(union) if union else 1.0


def _clusters(facts: list[dict[str, Any]], threshold: float) -> list[list[str]]:
    ids = [_fact_id(fact) for fact in facts]
    parent = list(range(len(facts)))

    def find(index: int) -> int:
        while parent[index] != index:
            parent[index] = parent[parent[index]]
            index = parent[index]
        return index

    def union(left: int, right: int) -> None:
        left_root, right_root = find(left), find(right)
        if left_root != right_root:
            parent[right_root] = left_root

    for left in range(len(facts)):
        for right in range(left + 1, len(facts)):
            if _jaccard(str(facts[left].get("text") or ""), str(facts[right].get("text") or "")) >= threshold:
                union(left, right)
    grouped: dict[int, list[str]] = {}
    for index, fact_id in enumerate(ids):
        grouped.setdefault(find(index), []).append(fact_id)
    return list(grouped.values())


def aggregate_packet(
    queries: list[dict[str, Any]], cap: int, cluster_threshold: float
) -> dict[str, Any]:
    by_id: dict[str, dict[str, Any]] = {}
    stable_index = 0
    per_query: list[dict[str, Any]] = []
    for query in queries:
        query_id = str(query.get("query_id") or "").strip()
        facts = query.get("facts")
        if not query_id or not isinstance(facts, list):
            raise ValueError("each packet_aggregation query needs query_id and facts")
        ids: list[str] = []
        for rank, fact in enumerate(facts, 1):
            fact_id = _fact_id(fact)
            ids.append(fact_id)
            entry = by_id.get(fact_id)
            if entry is None:
                entry = {
                    "fact": fact,
                    "best_rank_1based": rank,
                    "hit_count": 1,
                    "per_query_ranks_1based": {query_id: rank},
                    "stable_index": stable_index,
                }
                stable_index += 1
                by_id[fact_id] = entry
            else:
                entry["best_rank_1based"] = min(entry["best_rank_1based"], rank)
                entry["hit_count"] += 1
                entry["per_query_ranks_1based"][query_id] = rank
        per_query.append({"query_id": query_id, "ranked_fact_ids": ids})
    ordered = sorted(
        by_id.values(),
        key=lambda entry: (entry["best_rank_1based"], -entry["hit_count"], entry["stable_index"]),
    )
    delivered = ordered[:cap]
    delivered_facts = [entry["fact"] for entry in delivered]
    clusters = _clusters(delivered_facts, cluster_threshold)
    cluster_rows = [
        {"fact_ids": cluster, "slot_count": len(cluster)} for cluster in clusters if len(cluster) > 1
    ]
    rows = []
    for delivered_rank, entry in enumerate(delivered, 1):
        rows.append(
            {
                "fact_id": _fact_id(entry["fact"]),
                "delivered_rank_1based": delivered_rank,
                "best_rank_1based": entry["best_rank_1based"],
                "hit_count": entry["hit_count"],
                "per_query_ranks_1based": entry["per_query_ranks_1based"],
            }
        )
    occupied = sum(row["slot_count"] for row in cluster_rows)
    return {
        "cap": cap,
        "query_count": len(per_query),
        "unique_fact_count": len(by_id),
        "delivered_count": len(delivered),
        "ordering_rule": ["best_rank_ascending", "hit_count_descending", "stable_first_seen"],
        "per_query": per_query,
        "delivered": rows,
        "near_duplicate_clusters": {
            "method": "transitive_token_jaccard",
            "threshold": cluster_threshold,
            "cluster_count": len(cluster_rows),
            "occupied_slots": occupied,
            "occupancy_fraction": occupied / len(delivered) if delivered else 0.0,
            "clusters": cluster_rows,
        },
    }


def analyze(payload: dict[str, Any]) -> dict[str, Any]:
    if payload.get("schema_version") != INPUT_SCHEMA:
        raise ValueError(f"schema_version must be {INPUT_SCHEMA!r}")
    boost_input = payload.get("closed_negative_ablation") or {}
    packet_input = payload.get("packet_aggregation") or {}
    boost = boost_input.get("boost", DEFAULT_CLOSED_NEGATIVE_BOOST)
    top_k = boost_input.get("top_k", 5)
    cap = packet_input.get("cap", 12)
    threshold = packet_input.get("near_duplicate_threshold", DEFAULT_CLUSTER_THRESHOLD)
    if not isinstance(boost, int) or boost < 0 or not isinstance(top_k, int) or top_k <= 0:
        raise ValueError("boost must be a non-negative integer and top_k must be positive")
    if not isinstance(cap, int) or cap <= 0 or not isinstance(threshold, (int, float)) or not 0 <= threshold <= 1:
        raise ValueError("cap must be positive and near_duplicate_threshold must be in [0, 1]")
    return {
        "schema_version": OUTPUT_SCHEMA,
        "production_behavior_changed": False,
        "closed_negative_ablation": closed_negative_ablation(
            boost_input.get("queries") or [], boost, top_k
        ),
        "packet_aggregation": aggregate_packet(
            packet_input.get("queries") or [], cap, float(threshold)
        ),
    }


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("input", type=Path)
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    result = analyze(json.loads(args.input.read_text()))
    rendered = json.dumps(result, indent=2, sort_keys=True) + "\n"
    if args.output:
        args.output.write_text(rendered)
    else:
        print(rendered, end="")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
