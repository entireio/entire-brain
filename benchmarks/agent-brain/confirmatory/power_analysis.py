#!/usr/bin/env python3
"""Deterministic, data-free power sensitivity analysis for the WS6 design.

The calculation intentionally uses no benchmark outcomes.  Every variance and
correlation input is a labeled planning assumption.  The normal approximation
is a screening calculation rather than a substitute for the preregistered
task-clustered bootstrap, so it cannot justify a paid design that fails the
conservative scenario.
"""

from __future__ import annotations

import argparse
import json
import math
import pathlib
import sys
from statistics import NormalDist
from typing import Any, Callable


TASKS = 24
REPETITIONS = 4
PRIMARY_TREATMENTS = 3
REQUESTED_CELLS = TASKS * REPETITIONS * PRIMARY_TREATMENTS
TARGET_POWER = 0.80
TOKEN_REDUCTION_TARGET = 0.12
TOKEN_RATIO_TARGET = 1.0 - TOKEN_REDUCTION_TARGET
CORRECTNESS_MARGIN = -0.10
FAMILY_ALPHA = 0.05
FAMILY_COMPARISONS = 2
# Holm's first (worst-case) threshold for retrieved-vs-no-memory when the
# endpoint family also contains placebo-vs-no-memory.
PLANNING_ALPHA = FAMILY_ALPHA / FAMILY_COMPARISONS
NORMAL = NormalDist()


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
                "maximum_calls_with_10_percent_reserve": (
                    math.ceil(min_both * REPETITIONS * PRIMARY_TREATMENTS * 1.10)
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


def build_report() -> dict[str, Any]:
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
    passed = conservative["results"]["both_endpoints"]["both_marginal_targets_met"]
    report = {
        "schema_version": 1,
        "artifact_id": "agent-brain-confirmatory-power-v1",
        "status": "pass" if passed else "fail",
        "analysis_kind": "deterministic_data_free_sensitivity",
        "paid_runs_performed": False,
        "empirical_variance_used": False,
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
        "decision": {
            "scenario": conservative["id"],
            "rule": "pass only if token and correctness marginal power are each at least 0.80",
            "passed": passed,
            "conclusion": (
                "The proposed 24-task x 4-repetition design does not substantiate the declared power "
                "target under the conservative planning scenario. Do not mark power_target_met pass."
            ),
        },
    }
    return _round_floats(report)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "--check",
        type=pathlib.Path,
        help="fail if this JSON artifact differs semantically from the deterministic report",
    )
    args = parser.parse_args()
    report = build_report()
    if args.check is not None:
        existing = json.loads(args.check.read_text(encoding="utf-8"))
        if existing != report:
            print(f"ERROR: stale power artifact: {args.check}", file=sys.stderr)
            return 1
        print(f"power artifact is current: {args.check}")
        return 0
    print(json.dumps(report, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
