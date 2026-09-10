#!/usr/bin/env python3
"""Fail-closed scorer for the candidate-distillation quantitative contract.

The input is one sealed JSON evidence artifact.  It contains a predeclared
contract and development/confirmation partitions.  This tool never creates
labels or runs providers: it validates that complete labels and paired outputs
already exist, then emits a deterministic report.  A passing report is
evaluation evidence only; it is not a Phase gate decision.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import math
import pathlib
import random
import re
from statistics import NormalDist
import sys
from collections import defaultdict
from typing import Any, Iterable


SCHEMA = "candidate-distillation-quantitative-evidence/v1"
METRIC_VERSION = "candidate-distillation-paired-metrics/v1"
RESAMPLES = 10_000
ALPHA = 0.05
SHA256 = re.compile(r"^[0-9a-f]{64}$")


class EvidenceError(ValueError):
    """Raised when evidence cannot support a score."""


def require(value: bool, message: str) -> None:
    if not value:
        raise EvidenceError(message)


def load(path: pathlib.Path) -> dict[str, Any]:
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise EvidenceError(f"cannot read JSON evidence: {exc}") from exc
    require(isinstance(value, dict), "evidence root must be an object")
    return value


def canonical_sha256(value: Any) -> str:
    """Digest an artifact value with a stable, non-finite-number-free encoding."""
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(",", ":"), allow_nan=False).encode("utf-8")).hexdigest()


def partition_roster(partition: dict[str, Any]) -> dict[str, Any]:
    """The pre-judgment commitment: corpus identity only, never labels/results."""
    return {"source_sessions": [{key: session.get(key) for key in ("session_id", "family_id", "stratum", "source_content_sha256")}
                                for session in partition.get("source_sessions", [])]}


def _ids(values: Any, label: str) -> set[str]:
    require(isinstance(values, list), f"{label} must be a list")
    result: set[str] = set()
    for value in values:
        require(isinstance(value, str) and value, f"{label} entries must be non-empty strings")
        require(value not in result, f"{label} contains duplicate id {value!r}")
        result.add(value)
    return result


def _finite(value: Any, label: str) -> float:
    require(isinstance(value, (int, float)) and not isinstance(value, bool), f"{label} must be numeric")
    result = float(value)
    require(math.isfinite(result), f"{label} must be finite")
    return result


def validate_contract(value: dict[str, Any]) -> dict[str, Any]:
    require(value.get("schema") == SCHEMA, f"schema must be {SCHEMA!r}")
    contract = value.get("contract")
    require(isinstance(contract, dict), "contract must be an object")
    required = {
        "split_seed", "source_cutoff", "strata", "corpus_sizes", "provider_repetitions",
        "metric_version", "bootstrap", "power",
    }
    missing = sorted(required - set(contract))
    require(not missing, f"contract missing predeclared inputs: {', '.join(missing)}")
    require(isinstance(contract["split_seed"], (str, int)) and str(contract["split_seed"]), "split_seed is required")
    require(isinstance(contract["source_cutoff"], str) and contract["source_cutoff"], "source_cutoff is required")
    require(isinstance(contract["strata"], list) and contract["strata"], "strata must be non-empty")
    require(isinstance(contract["corpus_sizes"], dict), "corpus_sizes must be an object")
    for role in ("development", "confirmation"):
        require(isinstance(contract["corpus_sizes"].get(role), int) and contract["corpus_sizes"][role] > 0,
                f"corpus_sizes.{role} must be a positive integer")
    require(contract["metric_version"] == METRIC_VERSION, "metric_version is not the pinned evaluator version")
    bootstrap = contract["bootstrap"]
    require(isinstance(bootstrap, dict), "bootstrap must be an object")
    require(bootstrap.get("method") == "paired_stratified_cluster_bootstrap/v1", "unsupported bootstrap method")
    require(bootstrap.get("resamples") == RESAMPLES, "bootstrap must predeclare 10000 resamples")
    require(isinstance(bootstrap.get("seed"), int), "bootstrap seed must be an integer")
    power = contract["power"]
    require(isinstance(power, dict), "power must be an object")
    require(power.get("development_only") is True, "power calculation must be development-only")
    require(power.get("source_partition") == "development", "power source_partition must be development")
    require(power.get("one_sided_alpha") == ALPHA, "power must use one-sided alpha 0.05")
    require(power.get("target_power") == 0.80, "power must target 80 percent")
    require(power.get("recall_noninferiority_margin") == 0.02, "power must use 0.02 recall margin")
    require(isinstance(power.get("required_independent_families"), int)
            and power["required_independent_families"] > 0,
            "power.required_independent_families must be positive")
    require(isinstance(power.get("calculation"), dict), "power.calculation must retain the numerical development calculation")
    reps = contract["provider_repetitions"]
    require(isinstance(reps, dict) and reps, "provider_repetitions must be non-empty")
    for provider, count in reps.items():
        require(isinstance(provider, str) and provider, "provider repetition key must be non-empty")
        require(isinstance(count, int) and count > 0, "provider repetition counts must be positive")
    return contract


def validate_partitions(value: dict[str, Any], contract: dict[str, Any]) -> dict[str, dict[str, Any]]:
    partitions = value.get("partitions")
    require(isinstance(partitions, dict), "partitions must be an object")
    require(set(partitions) == {"development", "confirmation"}, "partitions must contain exactly development and confirmation")
    seal = value.get("preconfirmation_seal")
    require(isinstance(seal, dict), "preconfirmation_seal must bind the frozen contract and partitions")
    require(seal.get("schema") == "candidate-distillation-preconfirmation-seal/v1", "unsupported preconfirmation seal schema")
    require(seal.get("contract_sha256") == canonical_sha256(contract), "preconfirmation seal contract digest mismatch")
    seen_sessions: set[str] = set()
    seen_source_content: set[str] = set()
    families_by_role: dict[str, set[str]] = {}
    for role, partition in partitions.items():
        require(isinstance(partition, dict), f"{role} partition must be an object")
        sessions = partition.get("source_sessions")
        require(isinstance(sessions, list) and sessions, f"{role}.source_sessions must be non-empty")
        require(len(sessions) == contract["corpus_sizes"][role], f"{role} source session count differs from predeclared corpus size")
        families: set[str] = set()
        require(seal.get(f"{role}_roster_sha256") == canonical_sha256(partition_roster(partition)),
                f"preconfirmation seal {role} roster digest mismatch")
        for session in sessions:
            require(isinstance(session, dict), f"{role} source session must be an object")
            session_id, family_id = session.get("session_id"), session.get("family_id")
            require(isinstance(session_id, str) and session_id, f"{role} session_id is required")
            require(session_id not in seen_sessions, f"source session {session_id!r} appears in both partitions")
            seen_sessions.add(session_id)
            source_content = session.get("source_content")
            source_digest = session.get("source_content_sha256")
            require(isinstance(source_content, str) and source_content, f"{role} source_content is required")
            require(isinstance(source_digest, str) and SHA256.fullmatch(source_digest), f"{role} source_content_sha256 is required")
            require(source_digest == hashlib.sha256(source_content.encode("utf-8")).hexdigest(),
                    f"{role} source_content_sha256 does not match source content")
            require(source_digest not in seen_source_content,
                    "duplicate source content appears in both partitions or under a re-export")
            seen_source_content.add(source_digest)
            require(isinstance(family_id, str) and family_id, f"{role} family_id is required")
            families.add(family_id)
        families_by_role[role] = families
    require(not families_by_role["development"] & families_by_role["confirmation"],
            "session family appears in both development and confirmation")
    return partitions


def _output_metrics(session: dict[str, Any], arm: str, label: str, declared_repetitions: dict[str, int]) -> tuple[float, float, float, float, float, float, float, float, int, int]:
    refs = session.get("reference_facts")
    require(isinstance(refs, list), f"{label}.reference_facts must be a list")
    ref_ids: set[str] = set()
    authority: set[str] = set()
    for ref in refs:
        require(isinstance(ref, dict), f"{label}.reference_facts entries must be objects")
        fid = ref.get("id")
        require(isinstance(fid, str) and fid and fid not in ref_ids, f"{label} reference facts need unique ids")
        ref_ids.add(fid)
        if ref.get("authority_class") is True:
            authority.add(fid)
    outputs = session.get("outputs", {}).get(arm)
    require(isinstance(outputs, list) and outputs, f"{label}.{arm} output coverage is missing")
    supported: list[float] = []
    spans: list[float] = []
    recalls: list[float] = []
    precision: list[float] = []
    recall_counts: list[float] = []
    emitted_counts: list[float] = []
    span_counts: list[float] = []
    authority_span_counts: list[float] = []
    expected = session.get("expected_repetitions")
    require(isinstance(expected, dict) and expected, f"{label}.expected_repetitions is required")
    require(expected == declared_repetitions, f"{label}.expected_repetitions differs from the sealed contract")
    keys: set[tuple[str, int]] = set()
    for output in outputs:
        require(isinstance(output, dict), f"{label}.{arm} outputs must be objects")
        provider, repetition = output.get("provider"), output.get("repetition")
        require(provider in expected and isinstance(repetition, int) and repetition >= 1, f"{label}.{arm} invalid provider repetition")
        key = (provider, repetition)
        require(key not in keys, f"{label}.{arm} duplicate provider repetition {key}")
        keys.add(key)
        preserved = _ids(output.get("admitted_span_fact_ids"), f"{label}.{arm}.admitted_span_fact_ids")
        require(preserved <= ref_ids, f"{label}.{arm} admitted span refers to an unknown reference")
        emitted = output.get("emitted_facts")
        require(isinstance(emitted, list), f"{label}.{arm}.emitted_facts must be a list")
        supported_ids: set[str] = set()
        for emitted_fact in emitted:
            require(isinstance(emitted_fact, dict), f"{label}.{arm}.emitted facts must be objects")
            ref_id = emitted_fact.get("reference_id")
            if ref_id in ref_ids and emitted_fact.get("faithful") is True:
                supported_ids.add(ref_id)
        recalls.append(len(supported_ids) / len(ref_ids) if ref_ids else 0.0)
        precision.append(len(supported_ids) / len(emitted) if emitted else 0.0)
        spans.append(len(preserved & ref_ids) / len(ref_ids) if ref_ids else 0.0)
        supported.append(len(preserved & authority) / len(authority) if authority else 0.0)
        recall_counts.append(float(len(supported_ids)))
        emitted_counts.append(float(len(emitted)))
        span_counts.append(float(len(preserved & ref_ids)))
        authority_span_counts.append(float(len(preserved & authority)))
    expected_keys = {(provider, rep) for provider, count in expected.items() for rep in range(1, count + 1)}
    require(keys == expected_keys, f"{label}.{arm} provider coverage is incomplete")
    first_spans = None
    for output in outputs:
        spans_for_output = _ids(output["admitted_span_fact_ids"], f"{label}.{arm}.admitted_span_fact_ids")
        if first_spans is None:
            first_spans = spans_for_output
        else:
            require(spans_for_output == first_spans, f"{label}.{arm} selector drifted across provider repetitions")
    return (sum(recalls) / len(recalls), sum(precision) / len(precision), sum(spans) / len(spans), sum(supported) / len(supported),
            sum(recall_counts) / len(recall_counts), sum(emitted_counts) / len(emitted_counts),
            sum(span_counts) / len(span_counts), sum(authority_span_counts) / len(authority_span_counts), len(authority), len(ref_ids))


def _quantile(values: list[float], p: float) -> float:
    values = sorted(values)
    return values[max(0, min(len(values) - 1, math.ceil(p * len(values)) - 1))]


def _lower_bound(values: dict[str, tuple[str, float]], seed: int) -> float:
    require(values, "no independent families for bootstrap")
    groups: dict[str, list[float]] = defaultdict(list)
    for stratum, point in values.values():
        groups[stratum].append(point)
    rng = random.Random(seed)
    draws = [sum(sum(rng.choice(rows) for _ in rows) for rows in groups.values()) / sum(len(rows) for rows in groups.values()) for _ in range(RESAMPLES)]
    return _quantile(draws, ALPHA)


def _ratio_difference_lower_bound(
    components: dict[str, tuple[str, float, float, float, float]], seed: int
) -> float:
    """Cluster bootstrap a difference of aggregate ratios, preserving denominators."""
    require(components, "no independent families for bootstrap")
    groups: dict[str, list[tuple[float, float, float, float]]] = defaultdict(list)
    for stratum, candidate_num, candidate_den, legacy_num, legacy_den in components.values():
        groups[stratum].append((candidate_num, candidate_den, legacy_num, legacy_den))
    rows = [row for group in groups.values() for row in group]
    rng = random.Random(seed)
    draws: list[float] = []
    for _ in range(RESAMPLES):
        chosen = [rng.choice(group) for group in groups.values() for _ in group]
        candidate_num = sum(row[0] for row in chosen)
        candidate_den = sum(row[1] for row in chosen)
        legacy_num = sum(row[2] for row in chosen)
        legacy_den = sum(row[3] for row in chosen)
        # A resample made solely of labeled negative families has no recall
        # ratio. Treat it as adverse rather than silently dropping it.
        draws.append(candidate_num / candidate_den - legacy_num / legacy_den
                     if candidate_den > 0 and legacy_den > 0 else float("-inf"))
    return _quantile(draws, ALPHA)


def score(evidence: dict[str, Any]) -> dict[str, Any]:
    contract = validate_contract(evidence)
    partitions = validate_partitions(evidence, contract)
    result_digests = evidence.get("scored_partition_digests")
    require(isinstance(result_digests, dict), "scored_partition_digests must bind post-judgment inputs")
    for role in ("development", "confirmation"):
        require(result_digests.get(role) == canonical_sha256(partitions[role]),
                f"scored {role} partition digest mismatch")
    confirmation = partitions["confirmation"]
    # Development is not used to score confirmation, but its complete paired
    # rows are required because the sealed power calculation claims to derive
    # from this partition.
    development_components: dict[str, dict[str, float]] = defaultdict(lambda: defaultdict(float))
    for session in partitions["development"]["source_sessions"]:
        label = f"development/{session['session_id']}"
        candidate = _output_metrics(session, "candidate", label, contract["provider_repetitions"])
        legacy = _output_metrics(session, "legacy", label, contract["provider_repetitions"])
        development_components[session["family_id"]]["numerator"] += candidate[4] - legacy[4]
        development_components[session["family_id"]]["denominator"] += candidate[9]
    required_families = contract["power"]["required_independent_families"]
    require(len(development_components) >= 2, "power needs at least two development families")
    total_development_denominator = sum(component["denominator"] for component in development_components.values())
    require(total_development_denominator > 0, "development recall denominator is zero")
    mean = sum(component["numerator"] for component in development_components.values()) / total_development_denominator
    # Family influence values are the delta-method linearization of the
    # aggregate ratio, so different family/session sizes cannot turn this into
    # an unweighted session-rate estimate. Labeled negative families contribute
    # zero denominator/numerator without being discarded.
    family_count = len(development_components)
    influences = [family_count * (component["numerator"] - mean * component["denominator"]) / total_development_denominator
                  for component in development_components.values()]
    variance = sum(point ** 2 for point in influences) / (family_count - 1)
    sd = math.sqrt(variance)
    power = contract["power"]
    calculation = power["calculation"]
    require(calculation.get("method") == "normal_approximation_paired_recall/v1", "unsupported power calculation method")
    require(abs(_finite(calculation.get("observed_mean_difference"), "power observed mean") - mean) <= 1e-12,
            "power observed mean does not match development pairs")
    require(abs(_finite(calculation.get("observed_sd"), "power observed sd") - sd) <= 1e-12,
            "power observed sd does not match development pairs")
    margin_headroom = mean + power["recall_noninferiority_margin"]
    require(margin_headroom > 0, "development estimate cannot power the non-inferiority margin")
    z = NormalDist().inv_cdf(1 - ALPHA) + NormalDist().inv_cdf(power["target_power"])
    computed_required = max(2, math.ceil((z * sd / margin_headroom) ** 2))
    require(calculation.get("computed_required_independent_families") == computed_required,
            "power calculation required family count does not match development pairs")
    require(required_families == computed_required,
            "power.required_independent_families does not match recomputed development power")
    families: dict[str, dict[str, list[float]]] = defaultdict(lambda: defaultdict(list))
    span_hits = authority_total = fact_hits = fact_total = 0.0
    for session in confirmation["source_sessions"]:
        label = f"confirmation/{session['session_id']}"
        candidate = _output_metrics(session, "candidate", label, contract["provider_repetitions"])
        legacy = _output_metrics(session, "legacy", label, contract["provider_repetitions"])
        family = session["family_id"]
        stratum = session.get("stratum")
        require(isinstance(stratum, str) and stratum in contract["strata"], f"{label} must declare a predeclared stratum")
        if "stratum" in families[family]:
            require(families[family]["stratum"] == [stratum], f"family {family} spans strata")
        families[family]["stratum"] = [stratum]
        families[family]["candidate_recall_num"].append(candidate[4])
        families[family]["legacy_recall_num"].append(legacy[4])
        families[family]["reference_den"].append(candidate[9])
        families[family]["candidate_precision_num"].append(candidate[4])
        families[family]["candidate_precision_den"].append(candidate[5])
        families[family]["legacy_precision_num"].append(legacy[4])
        families[family]["legacy_precision_den"].append(legacy[5])
        span_hits += candidate[6]
        fact_hits += candidate[7]
        fact_total += candidate[9]
        authority_total += candidate[8]
    require(len(families) >= required_families,
            f"confirmation has {len(families)} independent families; power requires {required_families}")
    require(fact_total > 0, "all-fact span denominator is zero")
    require(authority_total > 0, "authority-class span denominator is zero")
    family_metrics: dict[str, dict[str, Any]] = {
        family: {name: points[0] if name == "stratum" else sum(points) for name, points in metrics.items()}
        for family, metrics in families.items()
    }
    retrieval = confirmation.get("retrieval_tasks")
    require(isinstance(retrieval, list) and retrieval, "confirmation retrieval task coverage is missing")
    retrieval_families: dict[str, dict[str, list[float]]] = defaultdict(lambda: defaultdict(list))
    for task in retrieval:
        require(isinstance(task, dict), "retrieval task must be an object")
        family = task.get("family_id")
        require(isinstance(family, str) and family, "retrieval task family_id is required")
        stratum = task.get("stratum")
        require(isinstance(stratum, str) and stratum in contract["strata"], "retrieval task must declare a predeclared stratum")
        if "stratum" in retrieval_families[family]:
            require(retrieval_families[family]["stratum"] == [stratum], f"retrieval family {family} spans strata")
        retrieval_families[family]["stratum"] = [stratum]
        for metric in ("recall", "precision", "useful_per_1k"):
            candidate = _finite(task.get("candidate", {}).get(metric), f"retrieval candidate.{metric}")
            legacy = _finite(task.get("legacy", {}).get(metric), f"retrieval legacy.{metric}")
            if metric != "useful_per_1k":
                require(0 <= candidate <= 1 and 0 <= legacy <= 1, f"retrieval {metric} must be in [0, 1]")
            else:
                require(candidate >= 0 and legacy >= 0, "retrieval useful_per_1k must be non-negative")
            retrieval_families[family][metric].append(candidate - legacy)
    require(len(retrieval_families) >= required_families,
            f"retrieval has {len(retrieval_families)} independent task families; power requires {required_families}")
    require(all(len(metrics["recall"]) == 1 for metrics in retrieval_families.values()),
            "this evaluator requires one pre-aggregated paired retrieval row per task family; variable family rows are insufficient evidence")
    diffs: dict[str, dict[str, tuple[str, float]]] = {"retrieval_recall": {}, "useful_per_1k": {}}
    for family, metrics in retrieval_families.items():
        for metric, output_name in (("recall", "retrieval_recall"), ("useful_per_1k", "useful_per_1k")):
            diffs[output_name][family] = (metrics["stratum"][0], sum(metrics[metric]) / len(metrics[metric]))
    seed = contract["bootstrap"]["seed"]
    fact_recall_components = {family: (metrics["stratum"], metrics["candidate_recall_num"], metrics["reference_den"], metrics["legacy_recall_num"], metrics["reference_den"])
                              for family, metrics in family_metrics.items()}
    precision_components = {family: (metrics["stratum"], metrics["candidate_precision_num"], metrics["candidate_precision_den"], metrics["legacy_precision_num"], metrics["legacy_precision_den"])
                            for family, metrics in family_metrics.items()}
    lower = {
        "fact_recall": _ratio_difference_lower_bound(fact_recall_components, seed),
        "durable_precision": _ratio_difference_lower_bound(precision_components, seed + 1),
        **{metric: _lower_bound(values, seed + index + 2) for index, (metric, values) in enumerate(diffs.items())},
    }
    span_all, span_authority = span_hits / fact_total, fact_hits / authority_total
    gates = {
        "all_fact_span_recall": span_all >= .95,
        "authority_span_recall": span_authority >= .98,
        "fact_recall_noninferiority": lower["fact_recall"] >= -.02,
        "durable_precision_nonregression": lower["durable_precision"] >= 0.0,
        "retrieval_recall_noninferiority": lower["retrieval_recall"] >= -.02,
        "useful_per_1k_nonregression": lower["useful_per_1k"] >= 0.0,
    }
    return {"schema": "candidate-distillation-quantitative-report/v1", "status": "pass" if all(gates.values()) else "fail",
            "acceptance_decision": "evaluation_only_not_phase_gate", "independent_families": {"source": len(families), "retrieval": len(retrieval_families)},
            "span_recall": {"all_facts": span_all, "authority_class": span_authority},
            "paired_lower_bounds": lower, "gates": gates}


def main(argv: Iterable[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("evidence", type=pathlib.Path, help="sealed JSON evidence artifact; see candidate_distillation_eval.md")
    parser.add_argument("--output", type=pathlib.Path, help="write JSON report (stdout otherwise)")
    args = parser.parse_args(argv)
    try:
        report = score(load(args.evidence))
    except EvidenceError as exc:
        report = {"schema": "candidate-distillation-quantitative-report/v1", "status": "insufficient_evidence", "error": str(exc), "acceptance_decision": "evaluation_only_not_phase_gate"}
    encoded = json.dumps(report, indent=2, sort_keys=True) + "\n"
    if args.output:
        args.output.write_text(encoded, encoding="utf-8")
    else:
        sys.stdout.write(encoded)
    return 0 if report["status"] == "pass" else 2


if __name__ == "__main__":
    raise SystemExit(main())
