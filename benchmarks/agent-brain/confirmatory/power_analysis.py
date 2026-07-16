#!/usr/bin/env python3
"""Deterministic v3 power-readiness contract for the three co-primary endpoints.

The authoritative report stays uncalibrated until final-contract paired task
rows exist for elapsed time, normalized billed cost, and code quality. Legacy
token/pass-rate calculations remain below only as quarantined exploratory
compatibility helpers; they cannot enter ``build_report`` or pass a gate.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import math
import pathlib
import sys
from statistics import NormalDist, fmean, stdev
from typing import Any, Callable


HERE = pathlib.Path(__file__).resolve().parent
REPO = HERE.parents[2]
AGENT_BRAIN = REPO / "benchmarks" / "agent-brain"
if str(AGENT_BRAIN) not in sys.path:
    sys.path.insert(0, str(AGENT_BRAIN))

from analysis.common import EXECUTED_RUN_PREDICATE_VERSION, is_executed_run  # noqa: E402


TASKS = 24
REPETITIONS = 4
PRIMARY_TREATMENTS = 3
REQUESTED_CELLS = TASKS * REPETITIONS * PRIMARY_TREATMENTS
AGENT_RETRY_LIMIT = 0
REPLACEMENT_CELL_LIMIT = 0
MAXIMUM_AGENT_INVOCATIONS = REQUESTED_CELLS
TARGET_POWER = 0.80
TIME_REDUCTION_TARGET = 0.10
COST_REDUCTION_TARGET = 0.12
QUALITY_DIFFERENCE_TARGET = 0.05
TOKEN_REDUCTION_TARGET = 0.12
TOKEN_RATIO_TARGET = 1.0 - TOKEN_REDUCTION_TARGET
CORRECTNESS_MARGIN = -0.10
FAMILY_ALPHA = 0.05
FAMILY_COMPARISONS = 2
# Holm's first (worst-case) threshold for retrieved-vs-no-memory when the
# endpoint family also contains placebo-vs-no-memory.
PLANNING_ALPHA = FAMILY_ALPHA / FAMILY_COMPARISONS
NORMAL = NormalDist()
CALIBRATION_MANIFEST = HERE / "power-calibration-exploratory-v1.json"
REPETITION_TRADEOFFS = (1, 2, 3, 4, 6, 8, 12)
TASK_TRADEOFFS = (24, 48, 64, 80, 96, 118, 128)


SCENARIOS: tuple[dict[str, Any], ...] = (
    {
        "id": "favorable_high_pairing",
        "role": "sensitivity_only",
        "description": "Low heterogeneity and strong task pairing; deliberately favorable.",
        "token": {
            "between_task_log_ratio_sd": 0.10,
            "within_attempt_log_token_sd": 0.15,
            "cross_treatment_residual_correlation": 0.50,
        },
        "correctness": {
            "no_memory_pass_probability": 0.80,
            "retrieved_memory_pass_probability": 0.80,
            "within_arm_attempt_icc": 0.05,
            "cross_treatment_task_mean_correlation": 0.80,
        },
    },
    {
        "id": "conservative_planning",
        "role": "decision_scenario",
        "description": (
            "Material task heterogeneity, moderate repeated-attempt clustering, and no assumed "
            "cross-treatment pairing benefit."
        ),
        "token": {
            "between_task_log_ratio_sd": 0.25,
            "within_attempt_log_token_sd": 0.30,
            "cross_treatment_residual_correlation": 0.00,
        },
        "correctness": {
            "no_memory_pass_probability": 0.75,
            "retrieved_memory_pass_probability": 0.75,
            "within_arm_attempt_icc": 0.20,
            "cross_treatment_task_mean_correlation": 0.00,
        },
    },
    {
        "id": "stress_lower_correctness",
        "role": "sensitivity_only",
        "description": "Higher heterogeneity and a true correctness difference of -0.03, still inside the -0.10 margin.",
        "token": {
            "between_task_log_ratio_sd": 0.35,
            "within_attempt_log_token_sd": 0.40,
            "cross_treatment_residual_correlation": 0.00,
        },
        "correctness": {
            "no_memory_pass_probability": 0.60,
            "retrieved_memory_pass_probability": 0.57,
            "within_arm_attempt_icc": 0.30,
            "cross_treatment_task_mean_correlation": 0.00,
        },
    },
)


def _sha256(path: pathlib.Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def _repo_file(raw_path: Any) -> pathlib.Path:
    if not isinstance(raw_path, str) or not raw_path:
        raise ValueError("calibration path must be a non-empty repo-relative string")
    relative = pathlib.Path(raw_path)
    if relative.is_absolute() or ".." in relative.parts:
        raise ValueError(f"unsafe calibration path: {raw_path!r}")
    resolved = (REPO / relative).resolve()
    try:
        resolved.relative_to(REPO.resolve())
    except ValueError as exc:
        raise ValueError(f"calibration path escapes repository: {raw_path!r}") from exc
    if not resolved.is_file():
        raise ValueError(f"calibration file does not exist: {raw_path}")
    return resolved


def _verified_file(record: dict[str, Any]) -> pathlib.Path:
    path = _repo_file(record.get("path"))
    expected = record.get("sha256")
    actual = _sha256(path)
    if not isinstance(expected, str) or expected != actual:
        raise ValueError(f"calibration content hash mismatch: {record.get('path')}")
    return path


def _load_ndjson(path: pathlib.Path) -> list[dict[str, Any]]:
    records: list[dict[str, Any]] = []
    for line_number, line in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
        if not line.strip():
            continue
        value = json.loads(line)
        if not isinstance(value, dict):
            raise ValueError(f"{path}:{line_number}: calibration record must be an object")
        records.append(value)
    return records


def _positive_number(value: Any) -> float | None:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return None
    number = float(value)
    return number if math.isfinite(number) and number > 0.0 else None


def _record_tokens(record: dict[str, Any]) -> float | None:
    agent_info = record.get("agent_info")
    usage = agent_info.get("usage") if isinstance(agent_info, dict) else None
    return _positive_number(usage.get("total_tokens")) if isinstance(usage, dict) else None


def _record_correct(record: dict[str, Any]) -> float | None:
    validation = record.get("validation")
    value = validation.get("ok") if isinstance(validation, dict) else None
    return float(value) if isinstance(value, bool) else None


def _sample_sd(values: list[float]) -> float | None:
    return stdev(values) if len(values) >= 2 else None


def _correlation(left: list[float], right: list[float]) -> float | None:
    if len(left) != len(right) or len(left) < 2:
        return None
    left_mean, right_mean = fmean(left), fmean(right)
    left_delta = [value - left_mean for value in left]
    right_delta = [value - right_mean for value in right]
    denominator = math.sqrt(
        sum(value * value for value in left_delta) * sum(value * value for value in right_delta)
    )
    if denominator == 0.0:
        return None
    return sum(a * b for a, b in zip(left_delta, right_delta)) / denominator


def _dirty_state(record: dict[str, Any]) -> bool | None:
    provenance = record.get("provenance")
    harness = provenance.get("harness") if isinstance(provenance, dict) else None
    dirty = harness.get("dirty") if isinstance(harness, dict) else None
    value = dirty.get("dirty") if isinstance(dirty, dict) else None
    return value if isinstance(value, bool) else None


def _source_calibration(source: dict[str, Any]) -> dict[str, Any]:
    path = _verified_file(source)
    records = _load_ndjson(path)
    control = source.get("control_condition")
    memory = source.get("memory_condition")
    if not isinstance(control, str) or not isinstance(memory, str) or control == memory:
        raise ValueError(f"invalid calibration conditions for {source.get('id')}")
    contamination = source.get("contamination")
    if (
        not isinstance(contamination, list)
        or not contamination
        or any(not isinstance(reason, str) or not reason for reason in contamination)
    ):
        raise ValueError(f"calibration contamination reasons are missing for {source.get('id')}")
    executed = [record for record in records if is_executed_run(record)]
    selected = [record for record in executed if record.get("condition") in {control, memory}]
    tasks: dict[str, dict[str, list[dict[str, Any]]]] = {}
    seen_cells: set[tuple[str, str, Any]] = set()
    for record in selected:
        task_id = record.get("task_id")
        condition = record.get("condition")
        if not isinstance(task_id, str) or not task_id:
            raise ValueError(f"calibration record in {source.get('path')} has no task_id")
        repetition = record.get("repetition")
        try:
            cell = (task_id, condition, repetition)
            duplicate = cell in seen_cells
        except TypeError as exc:
            raise ValueError(f"calibration repetition is not hashable for {task_id}") from exc
        if duplicate:
            raise ValueError(
                f"duplicate calibration task/condition/repetition cell: {task_id}/{condition}/{repetition}"
            )
        seen_cells.add(cell)
        tasks.setdefault(task_id, {}).setdefault(condition, []).append(record)

    task_rows: list[dict[str, Any]] = []
    token_residuals: list[float] = []
    paired_residual_control: list[float] = []
    paired_residual_memory: list[float] = []
    arm_values: dict[str, dict[str, list[float]]] = {
        control: {"tokens": [], "correctness": []},
        memory: {"tokens": [], "correctness": []},
    }
    for task_id in sorted(tasks):
        by_condition = tasks[task_id]
        if control not in by_condition or memory not in by_condition:
            continue
        row: dict[str, Any] = {"task_id": task_id, "conditions": {}}
        condition_log_means: dict[str, float] = {}
        repetition_logs: dict[str, dict[Any, float]] = {}
        for condition in (control, memory):
            condition_records = by_condition[condition]
            tokens = [
                value
                for value in (_record_tokens(record) for record in condition_records)
                if value is not None
            ]
            correctness = [
                value
                for value in (_record_correct(record) for record in condition_records)
                if value is not None
            ]
            arm_values[condition]["tokens"].extend(tokens)
            arm_values[condition]["correctness"].extend(correctness)
            log_tokens = [math.log(value) for value in tokens]
            if log_tokens:
                condition_log_means[condition] = fmean(log_tokens)
                token_residuals.extend(value - condition_log_means[condition] for value in log_tokens)
            repetitions: dict[Any, float] = {}
            for record in condition_records:
                token = _record_tokens(record)
                repetition = record.get("repetition")
                if token is not None and repetition not in repetitions:
                    repetitions[repetition] = math.log(token)
            repetition_logs[condition] = repetitions
            row["conditions"][condition] = {
                "executed_attempts": len(condition_records),
                "token_measurements": len(tokens),
                "correctness_measurements": len(correctness),
                "mean_total_tokens": fmean(tokens) if tokens else None,
                "pass_rate": fmean(correctness) if correctness else None,
            }
        control_tokens = row["conditions"][control]["mean_total_tokens"]
        memory_tokens = row["conditions"][memory]["mean_total_tokens"]
        control_pass = row["conditions"][control]["pass_rate"]
        memory_pass = row["conditions"][memory]["pass_rate"]
        row["mean_token_log_ratio_memory_vs_control"] = (
            math.log(memory_tokens / control_tokens)
            if control_tokens is not None and memory_tokens is not None
            else None
        )
        row["pass_rate_difference_memory_minus_control"] = (
            memory_pass - control_pass
            if control_pass is not None and memory_pass is not None
            else None
        )
        task_rows.append(row)

        paired_repetitions = sorted(
            set(repetition_logs[control]) & set(repetition_logs[memory]), key=lambda value: str(value)
        )
        for repetition in paired_repetitions:
            if control in condition_log_means and memory in condition_log_means:
                paired_residual_control.append(
                    repetition_logs[control][repetition] - condition_log_means[control]
                )
                paired_residual_memory.append(
                    repetition_logs[memory][repetition] - condition_log_means[memory]
                )

    log_ratios = [
        row["mean_token_log_ratio_memory_vs_control"]
        for row in task_rows
        if row["mean_token_log_ratio_memory_vs_control"] is not None
    ]
    pass_differences = [
        row["pass_rate_difference_memory_minus_control"]
        for row in task_rows
        if row["pass_rate_difference_memory_minus_control"] is not None
    ]
    dirty_states = [_dirty_state(record) for record in selected]
    arms = {}
    for condition in (control, memory):
        tokens = arm_values[condition]["tokens"]
        correctness = arm_values[condition]["correctness"]
        arms[condition] = {
            "token_measurements": len(tokens),
            "correctness_measurements": len(correctness),
            "mean_total_tokens": fmean(tokens) if tokens else None,
            "pass_rate": fmean(correctness) if correctness else None,
        }
    return {
        "id": source.get("id"),
        "path": source.get("path"),
        "sha256": source.get("sha256"),
        "diagnostic_role": source.get("diagnostic_role"),
        "eligibility": "exploratory_only_excluded_from_confirmatory_decision",
        "control_condition": control,
        "memory_condition": memory,
        "contamination": contamination,
        "record_counts": {
            "raw": len(records),
            "executed": len(executed),
            "selected_conditions": len(selected),
            "dirty_harness_true": sum(value is True for value in dirty_states),
            "dirty_harness_false": sum(value is False for value in dirty_states),
            "dirty_harness_unknown": sum(value is None for value in dirty_states),
        },
        "paired_task_clusters": len(task_rows),
        "arms": arms,
        "observed_diagnostics": {
            "task_mean_log_token_ratio_count": len(log_ratios),
            "task_mean_log_token_ratio_sd": _sample_sd(log_ratios),
            "task_pass_rate_difference_count": len(pass_differences),
            "task_pass_rate_difference_sd": _sample_sd(pass_differences),
            "within_task_arm_log_token_residual_count": len(token_residuals),
            "within_task_arm_log_token_residual_sd": _sample_sd(token_residuals),
            "same_repetition_cross_condition_residual_pairs": len(paired_residual_control),
            "same_repetition_cross_condition_residual_correlation": _correlation(
                paired_residual_control, paired_residual_memory
            ),
        },
        "paired_task_ids": [row["task_id"] for row in task_rows],
    }


def build_calibration_diagnostics(
    manifest_path: pathlib.Path = CALIBRATION_MANIFEST,
) -> dict[str, Any]:
    manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    if manifest.get("schema_version") != 1:
        raise ValueError("calibration manifest schema_version must be 1")
    if manifest.get("eligibility") != "exploratory_only":
        raise ValueError("calibration manifest must remain exploratory_only")
    if manifest.get("confirmatory_assumption_source") is not False:
        raise ValueError("exploratory calibration cannot be a confirmatory assumption source")
    decision_use = manifest.get("decision_use")
    prohibited = (
        "may_select_confirmatory_assumptions",
        "may_reduce_task_count_or_repetitions",
        "may_pass_power_gate",
    )
    if not isinstance(decision_use, dict) or any(decision_use.get(key) is not False for key in prohibited):
        raise ValueError("calibration decision-use quarantine is missing")
    sources = manifest.get("sources")
    if not isinstance(sources, list) or not sources:
        raise ValueError("calibration manifest must contain sources")
    source_ids = [source.get("id") for source in sources if isinstance(source, dict)]
    if len(source_ids) != len(sources) or any(not isinstance(value, str) for value in source_ids):
        raise ValueError("calibration sources require string ids")
    if len(source_ids) != len(set(source_ids)):
        raise ValueError("calibration source ids must be unique")
    diagnostics = [_source_calibration(source) for source in sources]

    excluded = manifest.get("excluded_sources")
    if not isinstance(excluded, list):
        raise ValueError("calibration excluded_sources must be an array")
    verified_exclusions = []
    for record in excluded:
        if not isinstance(record, dict):
            raise ValueError("calibration excluded source must be an object")
        _verified_file(record)
        verified_exclusions.append(record)

    unique_tasks = {task_id for source in diagnostics for task_id in source["paired_task_ids"]}
    cluster_instances = sum(source["paired_task_clusters"] for source in diagnostics)
    return {
        "manifest_path": str(manifest_path.resolve().relative_to(REPO.resolve())),
        "manifest_sha256": _sha256(manifest_path),
        "executed_run_predicate_version": EXECUTED_RUN_PREDICATE_VERSION,
        "eligibility": "exploratory_only_excluded_from_confirmatory_decision",
        "confirmatory_assumption_source": False,
        "pooling_policy": manifest.get("pooling_policy"),
        "source_count": len(diagnostics),
        "paired_task_cluster_instances": cluster_instances,
        "unique_task_ids_across_sources": len(unique_tasks),
        "pooled_estimate_prohibited": True,
        "sources": diagnostics,
        "excluded_sources": verified_exclusions,
        "interpretation": (
            "The byte-verified legacy records show material heterogeneity and correctness saturation, "
            "but their selected tasks, legacy treatments, runners, and harness contracts are not "
            "exchangeable with the candidate confirmatory experiment. They cannot select assumptions "
            "or pass the power gate."
        ),
    }


def normal_two_sided_power(effect: float, standard_error: float, alpha: float) -> float:
    """Power for a two-sided normal test under a fixed nonzero effect."""
    if standard_error < 0:
        raise ValueError("standard_error must be non-negative")
    if standard_error == 0:
        return float(effect != 0)
    critical = NORMAL.inv_cdf(1.0 - alpha / 2.0)
    signal = abs(effect) / standard_error
    return NORMAL.cdf(-critical - signal) + 1.0 - NORMAL.cdf(critical - signal)


def normal_noninferiority_power(
    true_difference: float,
    margin: float,
    standard_error: float,
    alpha: float,
) -> float:
    """Power for lower CI > margin using a one-sided normal critical value."""
    if true_difference <= margin:
        return 0.0
    if standard_error < 0:
        raise ValueError("standard_error must be non-negative")
    if standard_error == 0:
        return 1.0
    critical = NORMAL.inv_cdf(1.0 - alpha)
    signal = (true_difference - margin) / standard_error
    return NORMAL.cdf(signal - critical)


def token_task_sd(token: dict[str, float], repetitions: int) -> float:
    """SD of a task's mean log token ratio under the stated components."""
    between = token["between_task_log_ratio_sd"]
    within = token["within_attempt_log_token_sd"]
    correlation = token["cross_treatment_residual_correlation"]
    if repetitions < 1 or not -1.0 <= correlation <= 1.0:
        raise ValueError("invalid token planning assumption")
    return math.sqrt(between**2 + 2.0 * within**2 * (1.0 - correlation) / repetitions)


def correctness_task_sd(correctness: dict[str, float], repetitions: int) -> float:
    """SD of a paired task-level pass-rate difference.

    Each arm uses a Bernoulli variance inflated by 1 + (r - 1) * ICC.  The
    cross-treatment correlation is applied to the two task-level arm means.
    """
    p0 = correctness["no_memory_pass_probability"]
    p1 = correctness["retrieved_memory_pass_probability"]
    icc = correctness["within_arm_attempt_icc"]
    correlation = correctness["cross_treatment_task_mean_correlation"]
    if repetitions < 1 or not (0.0 <= p0 <= 1.0 and 0.0 <= p1 <= 1.0):
        raise ValueError("invalid correctness probability")
    if not 0.0 <= icc <= 1.0 or not -1.0 <= correlation <= 1.0:
        raise ValueError("invalid correctness correlation")
    design_effect = 1.0 + (repetitions - 1.0) * icc
    variance0 = p0 * (1.0 - p0) * design_effect / repetitions
    variance1 = p1 * (1.0 - p1) * design_effect / repetitions
    covariance = correlation * math.sqrt(variance0 * variance1)
    return math.sqrt(max(0.0, variance0 + variance1 - 2.0 * covariance))


def _minimum_tasks(power_at_tasks: Callable[[int], float], target: float) -> int | None:
    for tasks in range(2, 10_001):
        if power_at_tasks(tasks) >= target:
            return tasks
    return None


def _maximum_sd(power_at_sd: Callable[[float], float], target: float) -> float:
    low, high = 0.0, 10.0
    for _ in range(200):
        midpoint = (low + high) / 2.0
        if power_at_sd(midpoint) >= target:
            low = midpoint
        else:
            high = midpoint
    return low


def _design_power(scenario: dict[str, Any], tasks: int, repetitions: int) -> dict[str, Any]:
    if tasks < 2 or repetitions < 1:
        raise ValueError("power design requires at least two tasks and one repetition")
    token_sd = token_task_sd(scenario["token"], repetitions)
    token_power = normal_two_sided_power(
        abs(math.log(TOKEN_RATIO_TARGET)), token_sd / math.sqrt(tasks), PLANNING_ALPHA
    )
    correctness = scenario["correctness"]
    true_difference = (
        correctness["retrieved_memory_pass_probability"]
        - correctness["no_memory_pass_probability"]
    )
    correctness_sd = correctness_task_sd(correctness, repetitions)
    correctness_power = normal_noninferiority_power(
        true_difference,
        CORRECTNESS_MARGIN,
        correctness_sd / math.sqrt(tasks),
        PLANNING_ALPHA,
    )
    return {
        "tasks": tasks,
        "repetitions_per_treatment": repetitions,
        "requested_cells": tasks * repetitions * PRIMARY_TREATMENTS,
        "maximum_agent_invocations_no_retries_or_replacements": (
            tasks * repetitions * PRIMARY_TREATMENTS
        ),
        "token_task_level_log_ratio_sd": token_sd,
        "token_marginal_power": token_power,
        "correctness_task_level_pass_rate_difference_sd": correctness_sd,
        "correctness_noninferiority_marginal_power": correctness_power,
        "both_marginal_targets_met": token_power >= TARGET_POWER
        and correctness_power >= TARGET_POWER,
    }


def _minimum_repetitions(
    power_at_repetitions: Callable[[int], float], target: float, maximum: int = 10_000
) -> int | None:
    for repetitions in range(1, maximum + 1):
        if power_at_repetitions(repetitions) >= target:
            return repetitions
    return None


def _design_tradeoffs(scenario: dict[str, Any]) -> dict[str, Any]:
    token_effect = abs(math.log(TOKEN_RATIO_TARGET))
    correctness = scenario["correctness"]
    true_difference = (
        correctness["retrieved_memory_pass_probability"]
        - correctness["no_memory_pass_probability"]
    )
    by_repetitions: list[dict[str, Any]] = []
    for repetitions in REPETITION_TRADEOFFS:
        token_sd = token_task_sd(scenario["token"], repetitions)
        correctness_sd = correctness_task_sd(correctness, repetitions)
        token_tasks = _minimum_tasks(
            lambda tasks: normal_two_sided_power(
                token_effect, token_sd / math.sqrt(tasks), PLANNING_ALPHA
            ),
            TARGET_POWER,
        )
        correctness_tasks = _minimum_tasks(
            lambda tasks: normal_noninferiority_power(
                true_difference,
                CORRECTNESS_MARGIN,
                correctness_sd / math.sqrt(tasks),
                PLANNING_ALPHA,
            ),
            TARGET_POWER,
        )
        tasks_for_both = (
            max(token_tasks, correctness_tasks)
            if token_tasks is not None and correctness_tasks is not None
            else None
        )
        row = {
            "repetitions_per_treatment": repetitions,
            "minimum_tasks_for_token_target": token_tasks,
            "minimum_tasks_for_correctness_noninferiority_target": correctness_tasks,
            "minimum_tasks_for_both_marginal_targets": tasks_for_both,
            "requested_cells_at_minimum_tasks": (
                tasks_for_both * repetitions * PRIMARY_TREATMENTS
                if tasks_for_both is not None
                else None
            ),
            "maximum_agent_invocations_no_retries_or_replacements": (
                tasks_for_both * repetitions * PRIMARY_TREATMENTS
                if tasks_for_both is not None
                else None
            ),
        }
        by_repetitions.append(row)

    by_tasks: list[dict[str, Any]] = []
    for tasks in TASK_TRADEOFFS:
        token_repetitions = _minimum_repetitions(
            lambda repetitions: normal_two_sided_power(
                token_effect,
                token_task_sd(scenario["token"], repetitions) / math.sqrt(tasks),
                PLANNING_ALPHA,
            ),
            TARGET_POWER,
        )
        correctness_repetitions = _minimum_repetitions(
            lambda repetitions: normal_noninferiority_power(
                true_difference,
                CORRECTNESS_MARGIN,
                correctness_task_sd(correctness, repetitions) / math.sqrt(tasks),
                PLANNING_ALPHA,
            ),
            TARGET_POWER,
        )
        repetitions_for_both = (
            max(token_repetitions, correctness_repetitions)
            if token_repetitions is not None and correctness_repetitions is not None
            else None
        )
        by_tasks.append(
            {
                "tasks": tasks,
                "minimum_repetitions_for_token_target": token_repetitions,
                "minimum_repetitions_for_correctness_noninferiority_target": correctness_repetitions,
                "minimum_repetitions_for_both_marginal_targets": repetitions_for_both,
                "requested_cells_at_minimum_repetitions": (
                    tasks * repetitions_for_both * PRIMARY_TREATMENTS
                    if repetitions_for_both is not None
                    else None
                ),
                "maximum_agent_invocations_no_retries_or_replacements": (
                    tasks * repetitions_for_both * PRIMARY_TREATMENTS
                    if repetitions_for_both is not None
                    else None
                ),
            }
        )

    feasible_by_repetitions = [
        row for row in by_repetitions if row["requested_cells_at_minimum_tasks"] is not None
    ]
    fewest_cells = min(
        feasible_by_repetitions,
        key=lambda row: (
            row["requested_cells_at_minimum_tasks"],
            row["minimum_tasks_for_both_marginal_targets"],
        ),
    )
    fewest_tasks = min(
        feasible_by_repetitions,
        key=lambda row: (
            row["minimum_tasks_for_both_marginal_targets"],
            row["requested_cells_at_minimum_tasks"],
        ),
    )
    return {
        "scenario": scenario["id"],
        "status": "arithmetic_sensitivity_not_an_approved_design",
        "repetition_options": list(REPETITION_TRADEOFFS),
        "task_options": list(TASK_TRADEOFFS),
        "minimum_tasks_by_repetitions": by_repetitions,
        "minimum_repetitions_by_tasks": by_tasks,
        "current_design": _design_power(scenario, TASKS, REPETITIONS),
        "arithmetic_extremes_in_listed_repetition_options": {
            "fewest_requested_cells": fewest_cells,
            "fewest_task_clusters": fewest_tasks,
        },
        "interpretation": (
            "More independent task clusters are substantially more cell-efficient than additional "
            "repetitions under the stated positive ICC and irreducible task heterogeneity. These rows "
            "are conditional on hypothetical assumptions and are not recommendations or budget approvals."
        ),
    }


def _scenario_result(scenario: dict[str, Any]) -> dict[str, Any]:
    token_sd = token_task_sd(scenario["token"], REPETITIONS)
    token_effect = abs(math.log(TOKEN_RATIO_TARGET))
    token_power = normal_two_sided_power(token_effect, token_sd / math.sqrt(TASKS), PLANNING_ALPHA)

    correctness = scenario["correctness"]
    true_difference = (
        correctness["retrieved_memory_pass_probability"] - correctness["no_memory_pass_probability"]
    )
    correctness_sd = correctness_task_sd(correctness, REPETITIONS)
    correctness_power = normal_noninferiority_power(
        true_difference,
        CORRECTNESS_MARGIN,
        correctness_sd / math.sqrt(TASKS),
        PLANNING_ALPHA,
    )

    min_token_tasks = _minimum_tasks(
        lambda tasks: normal_two_sided_power(
            token_effect, token_sd / math.sqrt(tasks), PLANNING_ALPHA
        ),
        TARGET_POWER,
    )
    min_correctness_tasks = _minimum_tasks(
        lambda tasks: normal_noninferiority_power(
            true_difference,
            CORRECTNESS_MARGIN,
            correctness_sd / math.sqrt(tasks),
            PLANNING_ALPHA,
        ),
        TARGET_POWER,
    )
    min_both = (
        max(min_token_tasks, min_correctness_tasks)
        if min_token_tasks is not None and min_correctness_tasks is not None
        else None
    )
    joint_lower = max(0.0, token_power + correctness_power - 1.0)
    joint_upper = min(token_power, correctness_power)
    return {
        **scenario,
        "assumption_basis": "hypothetical planning values; no benchmark outcomes or empirical variance used",
        "results": {
            "token": {
                "task_level_log_ratio_sd": token_sd,
                "standard_error": token_sd / math.sqrt(TASKS),
                "marginal_power": token_power,
                "target_met": token_power >= TARGET_POWER,
                "minimum_tasks_at_4_repetitions": min_token_tasks,
            },
            "correctness_noninferiority": {
                "true_difference": true_difference,
                "task_level_pass_rate_difference_sd": correctness_sd,
                "standard_error": correctness_sd / math.sqrt(TASKS),
                "marginal_power": correctness_power,
                "target_met": correctness_power >= TARGET_POWER,
                "minimum_tasks_at_4_repetitions": min_correctness_tasks,
            },
            "both_endpoints": {
                "both_marginal_targets_met": token_power >= TARGET_POWER
                and correctness_power >= TARGET_POWER,
                "joint_power_frechet_lower_bound": joint_lower,
                "joint_power_frechet_upper_bound": joint_upper,
                "joint_power_if_independent_sensitivity_only": token_power * correctness_power,
                "minimum_tasks_for_both_marginal_targets_at_4_repetitions": min_both,
                "requested_cells_at_that_task_count": (
                    min_both * REPETITIONS * PRIMARY_TREATMENTS if min_both is not None else None
                ),
                "maximum_agent_invocations_no_retries_or_replacements": (
                    min_both * REPETITIONS * PRIMARY_TREATMENTS
                    if min_both is not None
                    else None
                ),
            },
        },
    }


def _asymptotic_repetition_result(scenario: dict[str, Any]) -> dict[str, Any]:
    token = scenario["token"]
    correctness = scenario["correctness"]
    token_sd = token["between_task_log_ratio_sd"]
    p0 = correctness["no_memory_pass_probability"]
    p1 = correctness["retrieved_memory_pass_probability"]
    icc = correctness["within_arm_attempt_icc"]
    correlation = correctness["cross_treatment_task_mean_correlation"]
    variance0 = p0 * (1.0 - p0) * icc
    variance1 = p1 * (1.0 - p1) * icc
    correctness_sd = math.sqrt(
        max(0.0, variance0 + variance1 - 2.0 * correlation * math.sqrt(variance0 * variance1))
    )
    true_difference = p1 - p0
    token_power = normal_two_sided_power(
        abs(math.log(TOKEN_RATIO_TARGET)), token_sd / math.sqrt(TASKS), PLANNING_ALPHA
    )
    correctness_power = normal_noninferiority_power(
        true_difference,
        CORRECTNESS_MARGIN,
        correctness_sd / math.sqrt(TASKS),
        PLANNING_ALPHA,
    )
    return {
        "interpretation": "limit as repetitions per treatment increase; task count remains 24",
        "token_task_level_sd_floor": token_sd,
        "token_marginal_power_ceiling": token_power,
        "correctness_task_level_sd_floor": correctness_sd,
        "correctness_marginal_power_ceiling": correctness_power,
        "repetitions_only_can_meet_both_marginal_targets": token_power >= TARGET_POWER
        and correctness_power >= TARGET_POWER,
    }


def _round_floats(value: Any) -> Any:
    if isinstance(value, float):
        return round(value, 6)
    if isinstance(value, list):
        return [_round_floats(item) for item in value]
    if isinstance(value, tuple):
        return [_round_floats(item) for item in value]
    if isinstance(value, dict):
        return {key: _round_floats(item) for key, item in value.items()}
    return value


def _build_deprecated_v2_report_not_for_decision() -> dict[str, Any]:
    token_effect = abs(math.log(TOKEN_RATIO_TARGET))
    max_token_sd = _maximum_sd(
        lambda sd: normal_two_sided_power(token_effect, sd / math.sqrt(TASKS), PLANNING_ALPHA),
        TARGET_POWER,
    )
    max_correctness_sd = _maximum_sd(
        lambda sd: normal_noninferiority_power(
            0.0, CORRECTNESS_MARGIN, sd / math.sqrt(TASKS), PLANNING_ALPHA
        ),
        TARGET_POWER,
    )
    scenarios = [_scenario_result(scenario) for scenario in SCENARIOS]
    conservative = next(item for item in scenarios if item["role"] == "decision_scenario")
    conservative_inputs = next(item for item in SCENARIOS if item["role"] == "decision_scenario")
    passed = conservative["results"]["both_endpoints"]["both_marginal_targets_met"]
    calibration = build_calibration_diagnostics()
    tradeoffs = _design_tradeoffs(conservative_inputs)
    report = {
        "schema_version": 2,
        "artifact_id": "agent-brain-confirmatory-power-v2",
        "status": "pass" if passed else "fail",
        "analysis_kind": "deterministic_assumption_sensitivity_with_quarantined_exploratory_calibration",
        "paid_runs_performed": False,
        "empirical_variance_used_in_confirmatory_decision": False,
        "empirical_variance_computed_for_exploratory_diagnostics": True,
        "exploratory_outcomes_analyzed": True,
        "protocol_inputs": {
            "tasks": TASKS,
            "repetitions_per_treatment": REPETITIONS,
            "primary_treatments": PRIMARY_TREATMENTS,
            "requested_cells": REQUESTED_CELLS,
            "power_target": TARGET_POWER,
            "token_reduction_target": TOKEN_REDUCTION_TARGET,
            "token_ratio_alternative": TOKEN_RATIO_TARGET,
            "correctness_noninferiority_margin_absolute": CORRECTNESS_MARGIN,
            "endpoint_family_alpha": FAMILY_ALPHA,
            "endpoint_family_comparisons": FAMILY_COMPARISONS,
            "holm_worst_case_planning_alpha": PLANNING_ALPHA,
        },
        "method": {
            "token": (
                "Two-sided normal power for the paired task-level mean log token ratio; task SD is "
                "sqrt(between-task treatment-effect variance + averaged within-attempt variance)."
            ),
            "correctness": (
                "One-sided normal non-inferiority power for the paired task-level pass-rate "
                "difference; repeated Bernoulli variance is inflated by a stated ICC."
            ),
            "multiplicity": "Holm first-step alpha=0.05/2 is used for both retained-vs-control comparisons.",
            "small_sample_caveat": (
                "The normal approximation does not model discrete correctness outcomes or the exact "
                "small-sample bootstrap tails at 24 task clusters; required task counts are sensitivity "
                "estimates, not a final design recommendation."
            ),
            "joint_power_caveat": (
                "Endpoint dependence is unspecified. Frechet bounds and an independence sensitivity "
                "are reported, but the pass rule uses the two marginal targets."
            ),
        },
        "design_tolerance_at_24_tasks": {
            "maximum_task_level_log_ratio_sd_for_80_percent_token_power": max_token_sd,
            "maximum_task_level_pass_rate_difference_sd_for_80_percent_noninferiority_power_at_true_parity": max_correctness_sd,
            "interpretation": (
                "The design can meet the marginal targets only if actual task-level variability is no "
                "larger than these thresholds under the planning approximation."
            ),
        },
        "scenarios": scenarios,
        "conservative_repetition_limit": _asymptotic_repetition_result(conservative),
        "design_options": tradeoffs,
        "exploratory_calibration": calibration,
        "decision": {
            "scenario": conservative["id"],
            "rule": "pass only if token and correctness marginal power are each at least 0.80",
            "passed": passed,
            "conclusion": (
                "The proposed 24-task x 4-repetition design does not substantiate the declared power "
                "target under the conservative planning scenario. Quarantined legacy outcomes cannot "
                "repair that failure. Do not mark power_target_met pass."
            ),
        },
        "next_decision": {
            "status": "human_methodology_decision_required",
            "recommended_action": "choose_calibration_basis_before_resizing_or_sealing_holdout",
            "smallest_defensible_step": (
                "Keep the gate failed and the fresh holdout unopened. Decide whether to (a) explicitly "
                "accept the hypothetical conservative assumptions and their resulting task/cell envelope, "
                "or (b) preregister and budget a separate development-only calibration under the final "
                "runner and treatment contracts. Do not choose task count from the legacy point estimates."
            ),
            "why_no_automatic_design_is_selected": [
                "the decision scenario is hypothetical rather than empirically calibrated",
                "the retained outcomes use legacy treatments and non-exchangeable runners and harnesses",
                "the byte-verifiable sources contain only sparse, selected task clusters with saturated or unstable correctness",
                "the apparent cell minimum trades repetitions for hundreds of task clusters and is only arithmetic sensitivity",
            ],
            "prohibited_until_decision": [
                "mark power_target_met pass",
                "seal or open the fresh confirmatory holdout",
                "authorize paid confirmatory cells",
                "promote exploratory variance estimates into confirmatory assumptions",
            ],
        },
    }
    return _round_floats(report)


def _finite_numeric(value: Any, label: str) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise ValueError(f"{label} must be numeric")
    try:
        number = float(value)
    except OverflowError as exc:
        raise ValueError(f"{label} must be finite") from exc
    if not math.isfinite(number):
        raise ValueError(f"{label} must be finite")
    return number


def _probability(value: Any, label: str) -> float:
    number = _finite_numeric(value, label)
    if not 0.0 <= number <= 1.0:
        raise ValueError(f"{label} must be in [0,1]")
    return number


def _positive_integer(value: Any, label: str) -> int:
    if isinstance(value, bool) or not isinstance(value, int) or value <= 0:
        raise ValueError(f"{label} must be a positive integer")
    return value


def recompute_power_decision(report: dict[str, Any]) -> dict[str, Any]:
    """Derive the gate from calibrated powers; never trust ``decision.passed``."""
    inputs = report.get("protocol_inputs")
    if not isinstance(inputs, dict):
        raise ValueError("power protocol_inputs must be an object")
    target = _probability(inputs.get("power_target"), "power target")
    if target != TARGET_POWER:
        raise ValueError(f"power target must remain frozen at {TARGET_POWER:.2f}")
    tasks = _positive_integer(inputs.get("tasks"), "protocol task count")
    repetitions = _positive_integer(
        inputs.get("repetitions_per_treatment"), "protocol repetitions per treatment"
    )
    treatments = _positive_integer(
        inputs.get("primary_treatments"), "protocol primary treatment count"
    )
    expected_cells = tasks * repetitions * treatments
    requested_cells = inputs.get("requested_cells")
    if isinstance(requested_cells, bool) or requested_cells != expected_cells:
        raise ValueError("protocol requested-cell arithmetic is inconsistent")
    if (
        inputs.get("agent_retry_limit") != 0
        or isinstance(inputs.get("agent_retry_limit"), bool)
        or inputs.get("replacement_cell_limit") != 0
        or isinstance(inputs.get("replacement_cell_limit"), bool)
        or isinstance(inputs.get("maximum_agent_invocations"), bool)
        or inputs.get("maximum_agent_invocations") != expected_cells
    ):
        raise ValueError(
            "protocol invocation ceiling must equal requested cells with zero retries and replacements"
        )
    endpoints = report.get("co_primary_endpoints")
    if not isinstance(endpoints, dict) or set(endpoints) != {
        "elapsed_time",
        "normalized_cost",
        "code_quality",
    }:
        raise ValueError("power report must contain exactly three co-primary endpoints")
    readiness = report.get("design_readiness")
    joint = report.get("joint_iut_power")
    if not isinstance(readiness, dict) or not isinstance(joint, dict):
        raise ValueError("power readiness and joint IUT power records are required")
    readiness_arithmetic = {
        "provisional_tasks": tasks,
        "provisional_repetitions_per_treatment": repetitions,
        "provisional_requested_cells": expected_cells,
        "maximum_agent_invocations": expected_cells,
        "agent_retry_limit": 0,
        "replacement_cell_limit": 0,
    }
    for field, expected in readiness_arithmetic.items():
        if readiness.get(field) != expected or isinstance(readiness.get(field), bool):
            raise ValueError(f"power readiness arithmetic mismatch: {field}")

    alternatives_pending = True
    marginal_powers: list[float] = []
    for name, endpoint in endpoints.items():
        if not isinstance(endpoint, dict):
            raise ValueError(f"{name} endpoint must be an object")
        floor = endpoint.get("claim_floor")
        alternative = endpoint.get("planning_alternative")
        if not isinstance(floor, dict) or not isinstance(alternative, dict):
            raise ValueError(f"{name} must separate claim floor and planning alternative")
        floor_key = "difference_min" if name == "code_quality" else "ratio_max"
        floor_value = floor.get(floor_key)
        try:
            numeric_floor = _finite_numeric(floor_value, f"{name} claim floor")
        except ValueError as exc:
            raise ValueError(f"{name} claim floor is invalid") from exc
        if not 0.0 < numeric_floor < 1.0:
            raise ValueError(f"{name} claim floor is invalid")
        if floor.get("status") not in {"provisional", "frozen_approved"}:
            raise ValueError(f"{name} claim floor status is invalid")
        alt_status = alternative.get("status")
        alt_key = "difference_true" if name == "code_quality" else "ratio_true"
        alt_value = alternative.get(alt_key)
        if alt_status == "pending_owner_approval":
            if alt_value is not None:
                raise ValueError(f"{name} pending planning alternative must be null")
            if endpoint.get("paired_task_sd") is not None or endpoint.get("marginal_power") is not None:
                raise ValueError(f"{name} pending calibration must retain null SD and power")
            if endpoint.get("status") != "pending_final_contract_calibration_and_alternative":
                raise ValueError(f"{name} pending endpoint status is inconsistent")
            continue
        alternatives_pending = False
        if alt_status != "frozen_approved":
            raise ValueError(f"{name} planning alternative status is invalid")
        try:
            numeric_alternative = _finite_numeric(
                alt_value, f"{name} frozen planning alternative"
            )
        except ValueError:
            raise ValueError(f"{name} frozen planning alternative must be finite")
        if floor.get("status") != "frozen_approved":
            raise ValueError(
                f"{name} claim floor must be owner-approved and frozen before evaluation"
            )
        if name == "code_quality":
            if not -1.0 <= numeric_alternative <= 1.0:
                raise ValueError("quality planning alternative must be within [-1,1]")
            if not numeric_alternative > numeric_floor:
                raise ValueError("quality planning alternative must be strictly above its claim floor")
        else:
            if not 0.0 < numeric_alternative < numeric_floor:
                raise ValueError(f"{name} planning alternative must be strictly below its claim floor")
        sd = endpoint.get("paired_task_sd")
        try:
            numeric_sd = _finite_numeric(sd, f"{name} calibrated paired-task SD")
        except ValueError as exc:
            raise ValueError(f"{name} calibrated paired-task SD is invalid") from exc
        if numeric_sd < 0:
            raise ValueError(f"{name} calibrated paired-task SD is invalid")
        marginal_powers.append(_probability(endpoint.get("marginal_power"), f"{name} marginal power"))
        if endpoint.get("status") != "evaluated":
            raise ValueError(f"{name} calibrated endpoint status must be evaluated")

    count_fields = (
        "power_sized_development_task_count",
        "power_sized_confirmatory_task_count",
    )
    if alternatives_pending:
        if readiness.get("provisional_design_power_defensible") is not False:
            raise ValueError("pending power design cannot be marked power-defensible")
        if any(readiness.get(field) is not None for field in count_fields):
            raise ValueError("pending power design must retain null power-sized task counts")
        if joint.get("intersection_union_success_probability") is not None:
            raise ValueError("pending power design must retain null joint IUT power")
        if joint.get("status") != "pending_final_contract_calibration_and_alternative":
            raise ValueError("pending joint IUT power status is inconsistent")
        return {
            "passed": False,
            "status": "pending_uncalibrated",
            "reason": "planning alternatives, final-contract variance inputs, and endpoint dependence are pending",
        }
    if len(marginal_powers) != 3:
        raise ValueError("planning alternatives must be pending for all endpoints or frozen for all")
    calibration = report.get("calibration_requirements")
    if not isinstance(calibration, dict):
        raise ValueError("power calibration requirements must be an object")
    minimum_calibration_tasks = _positive_integer(
        calibration.get("minimum_independent_task_clusters"),
        "minimum calibration task clusters",
    )
    development_tasks = _positive_integer(
        readiness.get("power_sized_development_task_count"),
        "power_sized_development_task_count",
    )
    confirmatory_tasks = _positive_integer(
        readiness.get("power_sized_confirmatory_task_count"),
        "power_sized_confirmatory_task_count",
    )
    if development_tasks < minimum_calibration_tasks:
        raise ValueError(
            "power-sized development task count is below the calibration minimum"
        )
    if confirmatory_tasks != tasks:
        raise ValueError(
            "power-sized confirmatory task count must equal the protocol task count"
        )
    joint_power = _probability(
        joint.get("intersection_union_success_probability"), "joint IUT power"
    )
    if joint.get("status") != "evaluated":
        raise ValueError("calibrated joint IUT power status must be evaluated")
    passed = all(value >= target for value in marginal_powers) and joint_power >= target
    if readiness.get("provisional_design_power_defensible") is not passed:
        raise ValueError(
            "power-defensible readiness flag must equal the recomputed marginal/joint gate"
        )
    return {
        "passed": passed,
        "status": "pass" if passed else "fail_underpowered",
        "reason": (
            "every marginal and joint IUT power meets target"
            if passed
            else "one or more marginal or joint IUT powers is below target"
        ),
    }


def validate_power_report(report: dict[str, Any]) -> list[str]:
    """Return decision-consistency errors for a generated or retained report."""
    try:
        derived = recompute_power_decision(report)
    except (TypeError, ValueError) as exc:
        return [str(exc)]
    errors: list[str] = []
    decision = report.get("decision")
    if not isinstance(decision, dict) or decision.get("passed") is not derived["passed"]:
        errors.append("power decision.passed does not match recomputed marginal/joint gate")
    if report.get("status") != derived["status"]:
        errors.append("power status does not match recomputed marginal/joint gate")
    return errors


def build_report() -> dict[str, Any]:
    """Return the pending v3 three-endpoint power contract without invented alternatives."""
    calibration = build_calibration_diagnostics()
    endpoints = {
        "elapsed_time": {
            "estimand_scale": "paired_task_log_ratio",
            "claim_floor": {"ratio_max": 1.0 - TIME_REDUCTION_TARGET, "status": "provisional"},
            "planning_alternative": {"ratio_true": None, "status": "pending_owner_approval"},
            "paired_task_sd": None,
            "marginal_power": None,
            "status": "pending_final_contract_calibration_and_alternative",
        },
        "normalized_cost": {
            "estimand_scale": "ratio_of_equal_task_weighted_task_arm_mean_costs",
            "claim_floor": {"ratio_max": 1.0 - COST_REDUCTION_TARGET, "status": "provisional"},
            "planning_alternative": {"ratio_true": None, "status": "pending_owner_approval"},
            "paired_task_sd": None,
            "marginal_power": None,
            "status": "pending_final_contract_calibration_and_alternative",
        },
        "code_quality": {
            "estimand_scale": "paired_task_difference",
            "claim_floor": {"difference_min": QUALITY_DIFFERENCE_TARGET, "status": "provisional"},
            "planning_alternative": {"difference_true": None, "status": "pending_owner_approval"},
            "paired_task_sd": None,
            "marginal_power": None,
            "status": "pending_final_contract_calibration_and_alternative",
        },
    }
    report: dict[str, Any] = {
        "schema_version": 3,
        "artifact_id": "agent-brain-confirmatory-power-v3",
        "analysis_kind": "three_endpoint_intersection_union_power_calibration_pending",
        "paid_runs_performed": False,
        "protocol_inputs": {
            "tasks": TASKS,
            "repetitions_per_treatment": REPETITIONS,
            "primary_treatments": PRIMARY_TREATMENTS,
            "requested_cells": REQUESTED_CELLS,
            "agent_retry_limit": AGENT_RETRY_LIMIT,
            "replacement_cell_limit": REPLACEMENT_CELL_LIMIT,
            "maximum_agent_invocations": MAXIMUM_AGENT_INVOCATIONS,
            "power_target": TARGET_POWER,
            "intersection_union_alpha": FAMILY_ALPHA,
            "primary_contrast": "retrieved_memory_vs_no_memory",
            "cluster_unit": "task",
            "attempt_policy": "all_executed_attempts",
        },
        "co_primary_endpoints": endpoints,
        "joint_iut_power": {
            "intersection_union_success_probability": None,
            "status": "pending_final_contract_calibration_and_alternative",
            "method": "frozen_endpoint_dependence_simulation_or_conservative_bound",
        },
        "method": {
            "component_tests": "one-sided task-clustered superiority at each frozen claim floor",
            "joint_rule": (
                "intersection-union: all three component nulls must be rejected; no "
                "across-endpoint multiplicity adjustment is required"
            ),
            "planning_rule": (
                "owner-frozen true alternatives must be strictly better than claim floors; each "
                "marginal power and overall intersection-union joint success probability must "
                "each meet 0.80"
            ),
        },
        "calibration_requirements": {
            "status": "open",
            "must_match": [
                "full frozen provider, runner, agent CLI, requested/resolved model, effort, schedule, and quote identity",
                "v2 no_memory and retrieved_memory treatment contracts",
                "end-to-end timing boundary and timeout policy",
                "five-category frozen price quote, inclusion/absence semantics, authenticated structural zeros, and zero agent retries",
                "task-normalized quality rubric and critical-failure policy",
            ],
            "required_statistics": {
                "elapsed_time": "SD of paired task-level mean log ratios",
                "normalized_cost": "paired task-arm mean-cost rows for a task-clustered equal-weight ratio-of-means bootstrap",
                "code_quality": "SD of paired task-level mean differences",
                "dependence": "joint covariance or retained task-level calibration rows",
            },
            "minimum_independent_task_clusters": 12,
            "minimum_is_calibration_floor_not_power_sized_design": True,
            "selection_use": "development calibration only; cannot enter confirmatory outcomes",
        },
        "design_readiness": {
            "provisional_tasks": TASKS,
            "provisional_repetitions_per_treatment": REPETITIONS,
            "provisional_requested_cells": REQUESTED_CELLS,
            "maximum_agent_invocations": MAXIMUM_AGENT_INVOCATIONS,
            "agent_retry_limit": AGENT_RETRY_LIMIT,
            "replacement_cell_limit": REPLACEMENT_CELL_LIMIT,
            "provisional_design_power_defensible": False,
            "power_sized_development_task_count": None,
            "power_sized_confirmatory_task_count": None,
            "reason": "no owner-frozen alternatives or final-contract calibration support a numeric task count",
        },
        "exploratory_calibration": calibration,
        "empirical_variance_used_in_confirmatory_decision": False,
    }
    derived = recompute_power_decision(report)
    report["status"] = derived["status"]
    report["decision"] = {
        "passed": derived["passed"],
        "reason": derived["reason"],
        "prohibited_until_resolved": [
            "mark power_target_met pass",
            "freeze practical floors or planning alternatives without owner approval",
            "seal or open the fresh holdout",
            "authorize paid confirmatory agent invocations",
            "promote legacy two-endpoint sensitivity task counts into the v3 design",
        ],
    }
    return _round_floats(report)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "--check",
        type=pathlib.Path,
        help="fail if this JSON artifact differs semantically from the deterministic report",
    )
    parser.add_argument(
        "--output",
        type=pathlib.Path,
        help="write the deterministic JSON report to this path",
    )
    args = parser.parse_args()
    try:
        report = build_report()
    except (OSError, UnicodeDecodeError, json.JSONDecodeError, ValueError) as exc:
        print(f"ERROR: cannot build deterministic power report: {exc}", file=sys.stderr)
        return 2
    if args.check is not None:
        existing = json.loads(args.check.read_text(encoding="utf-8"))
        if existing != report:
            print(f"ERROR: stale power artifact: {args.check}", file=sys.stderr)
            return 1
        print(f"power artifact is current: {args.check}")
        return 0
    if args.output is not None:
        args.output.write_text(
            json.dumps(report, indent=2, sort_keys=True) + "\n", encoding="utf-8"
        )
        return 0
    print(json.dumps(report, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
