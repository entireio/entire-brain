"""Frozen-shape confirmatory analysis for verified agent-brain suites.

The analyzer intentionally consumes only the evidence bundle, schedule, and raw
execution records.  It never loads task configurations, holdout assignments, or
relevance labels.  All primary treatment attempts must be present and balanced
before an endpoint is computed.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import math
import pathlib
import re
import sys
from collections import defaultdict
from typing import Any, Iterable, Sequence

if __package__ in (None, ""):
    # Support ``python3 benchmarks/agent-brain/analysis/confirmatory.py``.
    sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1]))
    from analysis.common import is_executed_run, load_records
    from analysis.evidence import (
        ANALYZER_AGGREGATE_ALGORITHM,
        ANALYZER_RUNTIME_SOURCE_PATHS,
        analyzer_aggregate_sha256,
        current_analyzer_records,
        verify_bundle,
    )
else:
    from .common import is_executed_run, load_records
    from .evidence import (
        ANALYZER_AGGREGATE_ALGORITHM,
        ANALYZER_RUNTIME_SOURCE_PATHS,
        analyzer_aggregate_sha256,
        current_analyzer_records,
        verify_bundle,
    )


REPORT_SCHEMA = "agent-brain-confirmatory-analysis/v1"
PRIMARY_ARMS = ("no_memory", "placebo_packet", "retrieved_memory")
BASELINE_ARM = "no_memory"
COMPARISON_ARMS = ("retrieved_memory", "placebo_packet")
DEFAULT_TASKS = 24
DEFAULT_REPETITIONS = 4
DEFAULT_RESAMPLES = 10_000
DEFAULT_SEED = 607_152_026
CONFIDENCE = 0.95
ALPHA = 0.05
NONINFERIORITY_MARGIN = -0.10
SHA256_RE = re.compile(r"^[0-9a-f]{64}$")


class AnalysisInputError(ValueError):
    """Raised when a suite cannot safely enter confirmatory analysis."""


def _stable_json_sha256(value: Any) -> str:
    payload = json.dumps(value, sort_keys=True, separators=(",", ":"))
    return hashlib.sha256(payload.encode("utf-8")).hexdigest()


def _nested(record: dict[str, Any], *path: str) -> Any:
    value: Any = record
    for key in path:
        if not isinstance(value, dict):
            return None
        value = value.get(key)
    return value


def _positive_number(value: Any, label: str) -> float:
    if (
        isinstance(value, bool)
        or not isinstance(value, (int, float))
        or not math.isfinite(float(value))
        or float(value) <= 0.0
    ):
        raise AnalysisInputError(f"{label} must be a finite positive number, got {value!r}")
    return float(value)


def _quantile(sorted_values: Sequence[float], probability: float) -> float:
    """Linearly interpolated sample quantile (the common type-7 definition)."""
    if not sorted_values:
        raise ValueError("cannot take a quantile of an empty sample")
    if not 0.0 <= probability <= 1.0:
        raise ValueError("quantile probability must be in [0, 1]")
    position = (len(sorted_values) - 1) * probability
    lower = math.floor(position)
    upper = math.ceil(position)
    if lower == upper:
        return float(sorted_values[lower])
    fraction = position - lower
    return float(sorted_values[lower] * (1.0 - fraction) + sorted_values[upper] * fraction)


def _bootstrap_indices(task_count: int, resamples: int, seed: int) -> list[tuple[int, ...]]:
    if task_count < 1:
        raise AnalysisInputError("at least one task cluster is required")
    if isinstance(resamples, bool) or not isinstance(resamples, int) or resamples < 1:
        raise AnalysisInputError("bootstrap resamples must be a positive integer")
    if isinstance(seed, bool) or not isinstance(seed, int):
        raise AnalysisInputError("bootstrap seed must be an integer")
    # A specified SHA-256 counter stream avoids depending on implementation
    # details of ``random.randrange`` across Python releases. Rejection removes
    # modulo bias, making each retained index exactly uniform over task clusters.
    ceiling = 1 << 256
    limit = ceiling - (ceiling % task_count)
    domain = b"agent-brain-task-cluster-bootstrap-v1\0" + str(seed).encode("ascii") + b"\0"
    counter = 0
    draws: list[int] = []
    required = resamples * task_count
    while len(draws) < required:
        digest = hashlib.sha256(domain + counter.to_bytes(16, "big")).digest()
        counter += 1
        value = int.from_bytes(digest, "big")
        if value < limit:
            draws.append(value % task_count)
    return [
        tuple(draws[offset : offset + task_count])
        for offset in range(0, required, task_count)
    ]


def _bootstrap_means(values: Sequence[float], samples: Sequence[Sequence[int]]) -> list[float]:
    denominator = len(values)
    return [sum(values[index] for index in draw) / denominator for draw in samples]


def _one_sided_greater_p(
    bootstrap: Sequence[float], point: float, null_value: float
) -> float:
    """Centered-bootstrap p-value for H0: theta <= null versus theta > null."""
    distance = point - null_value
    exceedances = sum((sample - point) >= distance for sample in bootstrap)
    return (exceedances + 1.0) / (len(bootstrap) + 1.0)


def _two_sided_p(bootstrap: Sequence[float], point: float, null_value: float) -> float:
    """Centered-bootstrap two-sided p-value with a finite-resample correction."""
    distance = abs(point - null_value)
    exceedances = sum(abs(sample - point) >= distance for sample in bootstrap)
    return (exceedances + 1.0) / (len(bootstrap) + 1.0)


def holm_adjust(raw_p_values: dict[str, float], alpha: float = ALPHA) -> dict[str, dict[str, Any]]:
    """Return deterministic Holm step-down adjustments for one endpoint family."""
    if not raw_p_values:
        return {}
    for name, value in raw_p_values.items():
        if not isinstance(value, (int, float)) or isinstance(value, bool) or not 0.0 <= value <= 1.0:
            raise ValueError(f"invalid p-value for {name}: {value!r}")
    ordered = sorted(raw_p_values.items(), key=lambda item: (item[1], item[0]))
    family_size = len(ordered)
    running_adjusted = 0.0
    rejection_open = True
    result: dict[str, dict[str, Any]] = {}
    for index, (name, raw) in enumerate(ordered):
        multiplier = family_size - index
        threshold = alpha / multiplier
        running_adjusted = max(running_adjusted, multiplier * raw)
        rejected = rejection_open and raw <= threshold
        if not rejected:
            rejection_open = False
        result[name] = {
            "raw_p_value": raw,
            "holm_adjusted_p_value": min(1.0, running_adjusted),
            "holm_step": index + 1,
            "holm_alpha_threshold": threshold,
            "holm_rejected_at_family_alpha": rejected,
        }
    return result


def _runner_id(record: dict[str, Any]) -> str | None:
    runner = record.get("runner")
    value = runner.get("id") if isinstance(runner, dict) else None
    return value if isinstance(value, str) and value else None


def _schedule_cell_key(value: dict[str, Any]) -> tuple[str, str, str, int] | None:
    runner = value.get("runner")
    runner_id = runner.get("id") if isinstance(runner, dict) else None
    repetition = value.get("repetition")
    if (
        not isinstance(value.get("task_id"), str)
        or not isinstance(runner_id, str)
        or not isinstance(value.get("condition"), str)
        or isinstance(repetition, bool)
        or not isinstance(repetition, int)
    ):
        return None
    return (value["task_id"], runner_id, value["condition"], repetition)


def _validate_expected_schedule(
    records: Sequence[dict[str, Any]], expected_schedule_cells: Sequence[dict[str, Any]]
) -> None:
    expected_by_run: dict[str, tuple[str, str, str, int]] = {}
    for index, cell in enumerate(expected_schedule_cells):
        if not isinstance(cell, dict):
            raise AnalysisInputError(f"schedule cell {index} is not an object")
        run_id = cell.get("run_id")
        key = _schedule_cell_key(cell)
        if not isinstance(run_id, str) or not run_id or key is None:
            raise AnalysisInputError(f"schedule cell {index} has an invalid identity")
        if run_id in expected_by_run:
            raise AnalysisInputError(f"duplicate schedule run_id: {run_id}")
        expected_by_run[run_id] = key

    actual_by_run: dict[str, tuple[str, str, str, int]] = {}
    for index, record in enumerate(records):
        run_id = record.get("run_id")
        key = _schedule_cell_key(record)
        if not isinstance(run_id, str) or not run_id or key is None:
            raise AnalysisInputError(f"record {index} has an invalid schedule identity")
        if run_id in actual_by_run:
            raise AnalysisInputError(f"duplicate run_id: {run_id}")
        actual_by_run[run_id] = key

    missing = sorted(set(expected_by_run) - set(actual_by_run))
    extra = sorted(set(actual_by_run) - set(expected_by_run))
    if missing or extra:
        raise AnalysisInputError(
            f"records do not match verified schedule: missing={missing[:5]}, extra={extra[:5]}"
        )
    mismatched = sorted(
        run_id for run_id in expected_by_run if expected_by_run[run_id] != actual_by_run[run_id]
    )
    if mismatched:
        raise AnalysisInputError(f"record identities disagree with schedule: {mismatched[:5]}")


def _prepare_cells(
    records: Sequence[dict[str, Any]],
    *,
    expected_task_ids: Sequence[str],
    expected_runner_id: str,
    repetitions: int,
    expected_schedule_cells: Sequence[dict[str, Any]] | None,
) -> tuple[list[dict[str, Any]], dict[str, Any]]:
    if not expected_task_ids or any(not isinstance(item, str) or not item for item in expected_task_ids):
        raise AnalysisInputError("expected_task_ids must be a non-empty sequence of task IDs")
    task_ids = list(expected_task_ids)
    if len(task_ids) != len(set(task_ids)):
        raise AnalysisInputError("expected_task_ids contains duplicates")
    if not isinstance(expected_runner_id, str) or not expected_runner_id:
        raise AnalysisInputError("expected_runner_id is required")
    if isinstance(repetitions, bool) or not isinstance(repetitions, int) or repetitions < 1:
        raise AnalysisInputError("repetitions must be a positive integer")
    if expected_schedule_cells is not None:
        _validate_expected_schedule(records, expected_schedule_cells)

    expected_count = len(task_ids) * len(PRIMARY_ARMS) * repetitions
    if len(records) != expected_count:
        raise AnalysisInputError(
            f"expected {expected_count} primary cells, found {len(records)}"
        )

    expected_tasks = set(task_ids)
    seen_run_ids: set[str] = set()
    seen_cells: set[tuple[str, str, int]] = set()
    observed_tasks: set[str] = set()
    observed_arms: set[str] = set()
    condition_to_arm: dict[str, str] = {}
    arm_to_condition: dict[str, str] = {}
    missing_validation_scored_incorrect = 0
    missing_agent_api_measurements = 0
    cells: list[dict[str, Any]] = []

    for index, record in enumerate(records):
        prefix = f"record[{index}]"
        if not isinstance(record, dict):
            raise AnalysisInputError(f"{prefix} must be an object")
        run_id = record.get("run_id")
        if not isinstance(run_id, str) or not run_id:
            raise AnalysisInputError(f"{prefix}.run_id is required")
        if run_id in seen_run_ids:
            raise AnalysisInputError(f"duplicate run_id: {run_id}")
        seen_run_ids.add(run_id)

        if record.get("agent_ran") is not True or not is_executed_run(record):
            raise AnalysisInputError(f"{run_id}: cell is not an explicitly executed valid attempt")
        if record.get("analysis_excluded"):
            raise AnalysisInputError(f"{run_id}: analysis-excluded attempts cannot fill a requested cell")

        task_id = record.get("task_id")
        if not isinstance(task_id, str) or task_id not in expected_tasks:
            raise AnalysisInputError(f"{run_id}: unexpected or missing task_id {task_id!r}")
        runner_id = _runner_id(record)
        if runner_id != expected_runner_id:
            raise AnalysisInputError(
                f"{run_id}: runner {runner_id!r} does not match {expected_runner_id!r}"
            )
        condition = record.get("condition")
        if not isinstance(condition, str) or not condition:
            raise AnalysisInputError(f"{run_id}: condition is required")
        repetition = record.get("repetition")
        if (
            isinstance(repetition, bool)
            or not isinstance(repetition, int)
            or not 1 <= repetition <= repetitions
        ):
            raise AnalysisInputError(f"{run_id}: invalid repetition {repetition!r}")

        treatment = record.get("treatment")
        if not isinstance(treatment, dict):
            raise AnalysisInputError(f"{run_id}: explicit treatment metadata is required")
        arm = treatment.get("arm")
        if arm not in PRIMARY_ARMS:
            raise AnalysisInputError(f"{run_id}: non-primary or invalid treatment arm {arm!r}")
        if treatment.get("confirmatory_eligible") is not True:
            raise AnalysisInputError(f"{run_id}: treatment is not confirmatory eligible")
        if treatment.get("query_source") != "user_query" or record.get("retrieval_query_source") != "user_query":
            raise AnalysisInputError(f"{run_id}: primary treatments must use the user_query source")

        previous_arm = condition_to_arm.setdefault(condition, arm)
        if previous_arm != arm:
            raise AnalysisInputError(f"condition {condition!r} maps to multiple treatment arms")
        previous_condition = arm_to_condition.setdefault(arm, condition)
        if previous_condition != condition:
            raise AnalysisInputError(f"treatment arm {arm!r} maps to multiple conditions")

        cell_key = (task_id, arm, repetition)
        if cell_key in seen_cells:
            raise AnalysisInputError(f"duplicate task/arm/repetition cell: {cell_key}")
        seen_cells.add(cell_key)
        observed_tasks.add(task_id)
        observed_arms.add(arm)

        validation = record.get("validation")
        validation_ok = validation.get("ok") if isinstance(validation, dict) else None
        if not isinstance(validation_ok, bool):
            # A post-agent integrity or validation exception is a real, incorrect
            # outcome.  An otherwise successful-looking record with no validation
            # result is malformed and cannot enter the analysis.
            if not record.get("error"):
                raise AnalysisInputError(f"{run_id}: validation.ok must be boolean")
            validation_ok = False
            missing_validation_scored_incorrect += 1

        total_tokens = _positive_number(
            _nested(record, "agent_info", "usage", "total_tokens"),
            f"{run_id}: total_tokens",
        )
        harness_seconds = _positive_number(
            _nested(record, "timing", "harness_agent_interval_wall_seconds"),
            f"{run_id}: harness_agent_interval_wall_seconds",
        )
        api_value = _nested(record, "timing", "agent_reported_api_seconds")
        if api_value is None:
            # Provider-reported API duration is explicitly nullable in the run
            # contract.  Missing values withhold that complete secondary endpoint;
            # they never borrow the independently measured harness duration.
            api_seconds = None
            missing_agent_api_measurements += 1
        else:
            api_seconds = _positive_number(
                api_value,
                f"{run_id}: agent_reported_api_seconds",
            )
        cells.append(
            {
                "task_id": task_id,
                "arm": arm,
                "repetition": repetition,
                "correct": 1.0 if validation_ok else 0.0,
                "total_tokens": total_tokens,
                "harness_wall_seconds": harness_seconds,
                "agent_api_seconds": api_seconds,
            }
        )

    if observed_tasks != expected_tasks:
        raise AnalysisInputError(
            f"task set is incomplete: missing={sorted(expected_tasks - observed_tasks)}"
        )
    if observed_arms != set(PRIMARY_ARMS):
        raise AnalysisInputError(
            f"primary treatment set mismatch: got={sorted(observed_arms)}, want={list(PRIMARY_ARMS)}"
        )
    if len(condition_to_arm) != len(PRIMARY_ARMS):
        raise AnalysisInputError("conditions and primary treatment arms must have a one-to-one mapping")

    expected_repetitions = set(range(1, repetitions + 1))
    grouped_repetitions: dict[tuple[str, str], set[int]] = defaultdict(set)
    for cell in cells:
        grouped_repetitions[(cell["task_id"], cell["arm"])].add(cell["repetition"])
    for task_id in sorted(expected_tasks):
        for arm in PRIMARY_ARMS:
            observed = grouped_repetitions.get((task_id, arm), set())
            if observed != expected_repetitions:
                raise AnalysisInputError(
                    f"unbalanced cell {task_id}/{arm}: repetitions={sorted(observed)}, "
                    f"want={sorted(expected_repetitions)}"
                )

    return cells, {
        "valid_executed_attempts": len(cells),
        "expected_attempts": expected_count,
        "missing_validation_scored_incorrect": missing_validation_scored_incorrect,
        "missing_agent_api_measurements": missing_agent_api_measurements,
        "condition_to_treatment_arm": dict(sorted(condition_to_arm.items())),
    }


def _task_arm_means(
    cells: Sequence[dict[str, Any]], task_ids: Sequence[str], field: str
) -> dict[str, dict[str, float]]:
    values: dict[tuple[str, str], list[float]] = defaultdict(list)
    for cell in cells:
        values[(cell["task_id"], cell["arm"])].append(float(cell[field]))
    return {
        task_id: {
            arm: sum(values[(task_id, arm)]) / len(values[(task_id, arm)])
            for arm in PRIMARY_ARMS
        }
        for task_id in task_ids
    }


def _correctness_family(
    means: dict[str, dict[str, float]],
    task_ids: Sequence[str],
    samples: Sequence[Sequence[int]],
) -> dict[str, Any]:
    comparisons: dict[str, dict[str, Any]] = {}
    raw_p: dict[str, float] = {}
    for arm in COMPARISON_ARMS:
        differences = [means[task][arm] - means[task][BASELINE_ARM] for task in task_ids]
        point = sum(differences) / len(differences)
        bootstrap = sorted(_bootstrap_means(differences, samples))
        p_value = _one_sided_greater_p(bootstrap, point, NONINFERIORITY_MARGIN)
        raw_p[arm] = p_value
        comparisons[arm] = {
            "comparison": f"{arm}_vs_{BASELINE_ARM}",
            "n_task_clusters": len(task_ids),
            "task_level_paired_pass_rate_difference": point,
            "one_sided_confidence": CONFIDENCE,
            "one_sided_percentile_lower_bound": _quantile(bootstrap, ALPHA),
            "noninferiority_margin_absolute": NONINFERIORITY_MARGIN,
            "bootstrap_p_value_one_sided": p_value,
        }
    adjusted = holm_adjust(raw_p)
    for arm, comparison in comparisons.items():
        comparison.update(adjusted[arm])
        comparison["noninferiority_passed"] = bool(
            comparison["one_sided_percentile_lower_bound"] > NONINFERIORITY_MARGIN
            and comparison["holm_rejected_at_family_alpha"]
        )
    return {
        "status": "evaluated",
        "estimand": "mean_across_tasks(arm_all_valid_attempt_pass_rate - no_memory_all_valid_attempt_pass_rate)",
        "bootstrap": "task_clustered_percentile",
        "multiplicity": "holm_within_correctness_endpoint_family",
        "comparisons": comparisons,
        "primary_gate": {
            "comparison": "retrieved_memory_vs_no_memory",
            "passed": comparisons["retrieved_memory"]["noninferiority_passed"],
        },
    }


def _ratio_family(
    means: dict[str, dict[str, float]],
    task_ids: Sequence[str],
    samples: Sequence[Sequence[int]],
    *,
    estimand: str,
    multiplicity: str,
) -> dict[str, Any]:
    comparisons: dict[str, dict[str, Any]] = {}
    raw_p: dict[str, float] = {}
    for arm in COMPARISON_ARMS:
        log_ratios = [math.log(means[task][arm] / means[task][BASELINE_ARM]) for task in task_ids]
        point_log = sum(log_ratios) / len(log_ratios)
        bootstrap_log = sorted(_bootstrap_means(log_ratios, samples))
        p_value = _two_sided_p(bootstrap_log, point_log, 0.0)
        raw_p[arm] = p_value
        lower_log = _quantile(bootstrap_log, ALPHA / 2.0)
        upper_log = _quantile(bootstrap_log, 1.0 - ALPHA / 2.0)
        comparisons[arm] = {
            "comparison": f"{arm}_vs_{BASELINE_ARM}",
            "n_task_clusters": len(task_ids),
            "paired_task_geometric_mean_ratio": math.exp(point_log),
            "two_sided_confidence": CONFIDENCE,
            "two_sided_percentile_ci": [math.exp(lower_log), math.exp(upper_log)],
            "bootstrap_p_value_two_sided_log_ratio_zero": p_value,
        }
    adjusted = holm_adjust(raw_p)
    for arm, comparison in comparisons.items():
        comparison.update(adjusted[arm])
        comparison["ratio_below_one_supported"] = bool(
            comparison["two_sided_percentile_ci"][1] < 1.0
            and comparison["holm_rejected_at_family_alpha"]
        )
    return {
        "status": "evaluated",
        "estimand": estimand,
        "bootstrap": "task_clustered_percentile_on_log_ratio",
        "multiplicity": multiplicity,
        "comparisons": comparisons,
    }


def analyze_records(
    records: Sequence[dict[str, Any]],
    *,
    expected_task_ids: Sequence[str],
    expected_runner_id: str,
    repetitions: int = DEFAULT_REPETITIONS,
    resamples: int = DEFAULT_RESAMPLES,
    seed: int = DEFAULT_SEED,
    expected_schedule_cells: Sequence[dict[str, Any]] | None = None,
    suite_integrity: dict[str, Any] | None = None,
) -> dict[str, Any]:
    """Analyze already-verified records under the preregistered paired design.

    ``expected_task_ids`` is mandatory: deriving it from observed records would
    silently accept an entirely missing task.  Callers analyzing a bundle should
    prefer :func:`analyze_verified_suite`, which obtains the task IDs and cells
    from the content-verified schedule without reading holdout task metadata.
    """
    task_ids = sorted(expected_task_ids)
    cells, cell_integrity = _prepare_cells(
        records,
        expected_task_ids=task_ids,
        expected_runner_id=expected_runner_id,
        repetitions=repetitions,
        expected_schedule_cells=expected_schedule_cells,
    )
    samples = _bootstrap_indices(len(task_ids), resamples, seed)
    correctness_means = _task_arm_means(cells, task_ids, "correct")
    correctness = _correctness_family(correctness_means, task_ids, samples)

    arm_summary: dict[str, dict[str, Any]] = {}
    for arm in PRIMARY_ARMS:
        arm_cells = [cell for cell in cells if cell["arm"] == arm]
        api_values = [
            cell["agent_api_seconds"]
            for cell in arm_cells
            if cell["agent_api_seconds"] is not None
        ]
        arm_summary[arm] = {
            "valid_executed_attempts": len(arm_cells),
            "validation_pass_rate": sum(cell["correct"] for cell in arm_cells) / len(arm_cells),
            "arithmetic_mean_total_tokens": sum(cell["total_tokens"] for cell in arm_cells) / len(arm_cells),
            "arithmetic_mean_harness_wall_seconds": (
                sum(cell["harness_wall_seconds"] for cell in arm_cells) / len(arm_cells)
            ),
            "agent_api_measurements": len(api_values),
            "arithmetic_mean_agent_api_seconds": (
                sum(api_values) / len(api_values) if len(api_values) == len(arm_cells) else None
            ),
        }

    if correctness["primary_gate"]["passed"]:
        token_means = _task_arm_means(cells, task_ids, "total_tokens")
        tokens: dict[str, Any] = _ratio_family(
            token_means,
            task_ids,
            samples,
            estimand=(
                "geometric_mean_across_tasks(arm_mean_total_tokens_all_valid_attempts / "
                "no_memory_mean_total_tokens_all_valid_attempts)"
            ),
            multiplicity="holm_within_total_token_endpoint_family",
        )
        wall_endpoints: dict[str, Any] = {}
        harness_means = _task_arm_means(cells, task_ids, "harness_wall_seconds")
        wall_endpoints["harness_wall_seconds"] = {
            "source_field": "harness_agent_interval_wall_seconds",
            **_ratio_family(
                harness_means,
                task_ids,
                samples,
                estimand=(
                    "geometric_mean_across_tasks(arm_mean_harness_wall_seconds_all_valid_attempts / "
                    "no_memory_mean_harness_wall_seconds_all_valid_attempts)"
                ),
                multiplicity="holm_within_harness_wall_seconds_endpoint_family",
            ),
        }
        missing_api = cell_integrity["missing_agent_api_measurements"]
        if missing_api:
            wall_endpoints["agent_api_seconds"] = {
                "source_field": "agent_reported_api_seconds",
                "status": "not_evaluated_missing_measurements",
                "missing_measurements": missing_api,
                "required_measurements": len(cells),
                "reason": (
                    "agent-reported API duration is a separate nullable secondary endpoint; "
                    "the harness duration is not substituted"
                ),
            }
        else:
            api_means = _task_arm_means(cells, task_ids, "agent_api_seconds")
            wall_endpoints["agent_api_seconds"] = {
                "source_field": "agent_reported_api_seconds",
                **_ratio_family(
                    api_means,
                    task_ids,
                    samples,
                    estimand=(
                        "geometric_mean_across_tasks(arm_mean_agent_api_seconds_all_valid_attempts / "
                        "no_memory_mean_agent_api_seconds_all_valid_attempts)"
                    ),
                    multiplicity="holm_within_agent_api_seconds_endpoint_family",
                ),
            }
        wall_time: dict[str, Any] = {
            "status": (
                "evaluated_secondary"
                if not missing_api
                else "evaluated_secondary_with_unavailable_endpoint"
            ),
            "note": (
                "Harness wall time and agent-reported API time are separate endpoints; "
                "neither is substituted for the other."
            ),
            "endpoints": wall_endpoints,
        }
    else:
        withheld = {
            "status": "not_evaluated_correctness_gate",
            "reason": "retrieved_memory correctness did not clear the preregistered noninferiority gate",
        }
        tokens = dict(withheld)
        wall_time = dict(withheld)

    analyzer_path = pathlib.Path(__file__).resolve()
    current_identity_records = current_analyzer_records(analyzer_path.parent)
    current_identity_sha256 = analyzer_aggregate_sha256(current_identity_records)
    report = {
        "schema": REPORT_SCHEMA,
        "status": "complete",
        "analyzer_entrypoint_sha256": hashlib.sha256(analyzer_path.read_bytes()).hexdigest(),
        "analyzer_identity": {
            "algorithm": ANALYZER_AGGREGATE_ALGORITHM,
            "current_sha256": current_identity_sha256,
            "source_paths": list(ANALYZER_RUNTIME_SOURCE_PATHS),
            "matched": None,
            "note": "library-level record analysis is not bound to a suite or frozen expected hash",
        },
        "integrity": {
            "passed": True,
            **(suite_integrity or {}),
            **cell_integrity,
        },
        "design": {
            "task_clusters": len(task_ids),
            "runner_id": expected_runner_id,
            "primary_treatments": list(PRIMARY_ARMS),
            "repetitions_per_treatment": repetitions,
            "requested_primary_cells": len(task_ids) * len(PRIMARY_ARMS) * repetitions,
            "outlier_policy": "retain_all_valid_attempts",
        },
        "bootstrap": {
            "resamples": resamples,
            "seed": seed,
            "cluster_unit": "task",
            "index_generator": "sha256_counter_rejection_v1",
            "confidence": CONFIDENCE,
            "finite_resample_p_value_correction": "(exceedances + 1) / (resamples + 1)",
        },
        "arm_summary": arm_summary,
        "correctness": correctness,
        "tokens": tokens,
        "wall_time": wall_time,
    }
    return report


def _load_json(path: pathlib.Path) -> dict[str, Any]:
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise AnalysisInputError(f"cannot read {path}: {exc}") from exc
    if not isinstance(value, dict):
        raise AnalysisInputError(f"{path} must contain a JSON object")
    return value


def _assert_execution_order(
    schedule: dict[str, Any], schedule_state: dict[str, Any]
) -> None:
    cells = schedule.get("cells")
    if not isinstance(cells, list):
        raise AnalysisInputError("verified schedule has no cells array")
    planned = [cell.get("run_id") for cell in cells if isinstance(cell, dict)]
    if len(planned) != len(cells) or any(not isinstance(run_id, str) for run_id in planned):
        raise AnalysisInputError("verified schedule contains an invalid run ID")
    if schedule_state.get("schedule_sha256") != schedule.get("schedule_sha256"):
        raise AnalysisInputError("schedule-state hash does not match schedule")
    if schedule_state.get("planned_cell_count") != len(planned):
        raise AnalysisInputError("schedule-state planned cell count is inconsistent")
    if schedule_state.get("recorded_cell_count") != len(planned):
        raise AnalysisInputError("not all scheduled cells have records")
    deviations = schedule_state.get("deviations")
    if not isinstance(deviations, list) or deviations:
        raise AnalysisInputError(f"execution schedule contains deviations: {deviations!r}")
    if schedule_state.get("actual_started_order") != planned:
        raise AnalysisInputError("actual started order does not match the frozen schedule")
    if schedule_state.get("actual_finished_order") != planned:
        raise AnalysisInputError("actual finished order does not match the frozen schedule")


def analyze_verified_suite(
    suite_dir: pathlib.Path | str,
    *,
    expected_analyzer_sha256: str,
    expected_task_count: int = DEFAULT_TASKS,
    repetitions: int = DEFAULT_REPETITIONS,
    resamples: int = DEFAULT_RESAMPLES,
    seed: int = DEFAULT_SEED,
) -> dict[str, Any]:
    """Verify an evidence bundle and analyze it without loading holdout metadata."""
    root = pathlib.Path(suite_dir).resolve()
    verification = verify_bundle(root)
    if not verification.get("ok"):
        raise AnalysisInputError(
            f"evidence verification failed: {verification.get('errors', [])[:5]}"
        )
    manifest = _load_json(root / "evidence-manifest.json")
    if not isinstance(expected_analyzer_sha256, str) or not SHA256_RE.fullmatch(expected_analyzer_sha256):
        raise AnalysisInputError("a frozen 64-character lowercase analyzer SHA256 is required")
    analyzer = manifest.get("analyzer")
    if not isinstance(analyzer, dict):
        raise AnalysisInputError("suite manifest has no analyzer identity")
    if analyzer.get("algorithm") != ANALYZER_AGGREGATE_ALGORITHM:
        raise AnalysisInputError("suite analyzer aggregate algorithm does not match the frozen contract")
    suite_analyzer_sha256 = analyzer.get("aggregate_sha256")
    if suite_analyzer_sha256 != expected_analyzer_sha256:
        raise AnalysisInputError("suite analyzer aggregate does not match the frozen expected analyzer")
    current_records = current_analyzer_records(pathlib.Path(__file__).resolve().parent)
    current_analyzer_sha256 = analyzer_aggregate_sha256(current_records)
    if current_analyzer_sha256 != expected_analyzer_sha256:
        raise AnalysisInputError("current analyzer sources do not match the frozen expected analyzer")
    harness = manifest.get("harness")
    if not isinstance(harness, dict) or harness.get("confirmatory_eligible") is not True:
        raise AnalysisInputError("suite harness is not confirmatory eligible")
    schedule = _load_json(root / "schedule.json")
    schedule_state = _load_json(root / "schedule-state.json")
    expected_schedule_hash = _stable_json_sha256(
        {key: value for key, value in schedule.items() if key != "schedule_sha256"}
    )
    if schedule.get("schedule_sha256") != expected_schedule_hash:
        raise AnalysisInputError("schedule self-hash is invalid")
    _assert_execution_order(schedule, schedule_state)

    cells = schedule.get("cells")
    assert isinstance(cells, list)  # established by _assert_execution_order
    scheduled_tasks = sorted(
        {cell.get("task_id") for cell in cells if isinstance(cell, dict) and isinstance(cell.get("task_id"), str)}
    )
    scheduled_runners = sorted(
        {
            runner.get("id")
            for cell in cells
            if isinstance(cell, dict)
            for runner in [cell.get("runner")]
            if isinstance(runner, dict) and isinstance(runner.get("id"), str)
        }
    )
    if (
        isinstance(expected_task_count, bool)
        or not isinstance(expected_task_count, int)
        or expected_task_count < 1
    ):
        raise AnalysisInputError("expected_task_count must be a positive integer")
    if len(scheduled_tasks) != expected_task_count:
        raise AnalysisInputError(
            f"expected {expected_task_count} scheduled tasks, found {len(scheduled_tasks)}"
        )
    if scheduled_runners == [] or len(scheduled_runners) != 1:
        raise AnalysisInputError(
            f"confirmatory design requires exactly one runner, found {scheduled_runners}"
        )
    if schedule.get("repetitions") != repetitions:
        raise AnalysisInputError(
            f"schedule repetitions {schedule.get('repetitions')!r} do not match {repetitions}"
        )
    requested = manifest.get("requested_cells")
    expected_count = len(scheduled_tasks) * len(PRIMARY_ARMS) * repetitions
    if not isinstance(requested, dict) or requested.get("count") != expected_count:
        raise AnalysisInputError("manifest requested-cell count does not match the confirmatory design")
    requested_schedule = requested.get("schedule")
    if (
        not isinstance(requested_schedule, dict)
        or requested_schedule.get("schedule_sha256") != schedule.get("schedule_sha256")
        or requested_schedule.get("cell_count") != expected_count
    ):
        raise AnalysisInputError("manifest schedule identity does not match schedule.json")

    records = load_records(root)
    report = analyze_records(
        records,
        expected_task_ids=scheduled_tasks,
        expected_runner_id=scheduled_runners[0],
        repetitions=repetitions,
        resamples=resamples,
        seed=seed,
        expected_schedule_cells=cells,
        suite_integrity={
            "evidence_bundle_verified": True,
            "suite_id": manifest.get("suite_id"),
            "suite_identity_sha256": manifest.get("identity_sha256"),
            "schedule_sha256": schedule.get("schedule_sha256"),
            "schedule_deviations": 0,
            "holdout_metadata_loaded": False,
        },
    )
    report["analyzer_identity"] = {
        "algorithm": ANALYZER_AGGREGATE_ALGORITHM,
        "expected_sha256": expected_analyzer_sha256,
        "suite_sha256": suite_analyzer_sha256,
        "current_sha256": current_analyzer_sha256,
        "source_paths": list(ANALYZER_RUNTIME_SOURCE_PATHS),
        "matched": True,
    }
    return report


def load_analyzer_lock(path: pathlib.Path | str) -> str:
    """Load and structurally validate the frozen analyzer lock aggregate."""
    lock = _load_json(pathlib.Path(path))
    if lock.get("schema_version") != 1:
        raise AnalysisInputError("analyzer lock schema_version must be 1")
    if lock.get("algorithm") != ANALYZER_AGGREGATE_ALGORITHM:
        raise AnalysisInputError("analyzer lock algorithm is unsupported")
    files = lock.get("files")
    if not isinstance(files, list):
        raise AnalysisInputError("analyzer lock files must be an array")
    paths = [item.get("path") if isinstance(item, dict) else None for item in files]
    if paths != list(ANALYZER_RUNTIME_SOURCE_PATHS):
        raise AnalysisInputError("analyzer lock runtime source set changed or is out of order")
    records = [
        {"source_path": item.get("path"), "sha256": item.get("sha256")}
        for item in files
        if isinstance(item, dict)
    ]
    if len(records) != len(files) or any(
        not isinstance(item["sha256"], str) or not SHA256_RE.fullmatch(item["sha256"])
        for item in records
    ):
        raise AnalysisInputError("analyzer lock contains an invalid content hash")
    aggregate = analyzer_aggregate_sha256(records)
    if lock.get("aggregate_sha256") != aggregate:
        raise AnalysisInputError("analyzer lock aggregate hash mismatch")
    return aggregate


def _write_report(path: pathlib.Path | None, report: dict[str, Any]) -> None:
    rendered = json.dumps(report, indent=2, sort_keys=True) + "\n"
    if path is None:
        sys.stdout.write(rendered)
    else:
        path.write_text(rendered, encoding="utf-8")


def main(argv: Iterable[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description="Analyze a content-verified agent-brain confirmatory evidence suite."
    )
    parser.add_argument("suite", type=pathlib.Path, help="suite directory containing evidence-manifest.json")
    parser.add_argument("--output", type=pathlib.Path, help="write deterministic JSON here instead of stdout")
    parser.add_argument("--expected-tasks", type=int, default=DEFAULT_TASKS)
    parser.add_argument("--repetitions", type=int, default=DEFAULT_REPETITIONS)
    parser.add_argument("--resamples", type=int, default=DEFAULT_RESAMPLES)
    parser.add_argument("--seed", type=int, default=DEFAULT_SEED)
    analyzer_group = parser.add_mutually_exclusive_group()
    analyzer_group.add_argument(
        "--analyzer-sha256",
        help="frozen analyzer aggregate; bypasses the default analyzer-lock.json lookup",
    )
    analyzer_group.add_argument("--analyzer-lock", type=pathlib.Path)
    args = parser.parse_args(list(argv) if argv is not None else None)
    try:
        lock_path = (
            args.analyzer_lock
            or pathlib.Path(__file__).resolve().parents[1]
            / "confirmatory"
            / "analyzer-lock.json"
        )
        expected_analyzer_sha256 = (
            args.analyzer_sha256
            if args.analyzer_sha256 is not None
            else load_analyzer_lock(lock_path)
        )
        report = analyze_verified_suite(
            args.suite,
            expected_analyzer_sha256=expected_analyzer_sha256,
            expected_task_count=args.expected_tasks,
            repetitions=args.repetitions,
            resamples=args.resamples,
            seed=args.seed,
        )
        _write_report(args.output, report)
    except (AnalysisInputError, OSError, json.JSONDecodeError) as exc:
        parser.exit(2, f"confirmatory analysis refused: {exc}\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
