from __future__ import annotations

import copy
import json
import math
import pathlib
import sys
import tempfile
import unittest
from typing import Any
from unittest import mock


HERE = pathlib.Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent))
from analysis import confirmatory  # noqa: E402
from analysis import evidence  # noqa: E402


CONDITIONS = {
    "no_memory": "control",
    "placebo_packet": "placebo",
    "retrieved_memory": "retrieved",
}


def with_self_hash(value: dict, field: str) -> dict:
    result = copy.deepcopy(value)
    result[field] = confirmatory._self_hash(result, field)
    return result


def success_contract(*, frozen: bool = True) -> dict:
    status = "frozen_approved" if frozen else "provisional"
    return with_self_hash(
        {
            "schema": confirmatory.SUCCESS_CONTRACT_SCHEMA,
            "status": status,
            "primary_contrast": "retrieved_memory_vs_no_memory",
            "intersection_union_alpha": 0.05,
            "endpoints": {
                "elapsed_time": {
                    "direction": "lower",
                    "estimand": "paired_task_geometric_mean_ratio",
                    "practical_ratio_max": 0.90,
                    "floor_status": status,
                },
                "normalized_cost": {
                    "direction": "lower",
                    "estimand": "ratio_of_equal_task_weighted_task_arm_mean_costs",
                    "practical_ratio_max": 0.88,
                    "floor_status": status,
                },
                "code_quality": {
                    "direction": "higher",
                    "estimand": "paired_task_mean_difference",
                    "practical_difference_min": 0.05,
                    "floor_status": status,
                },
            },
            "code_quality_measurement": {
                "schema": confirmatory.QUALITY_SCHEMA,
                "rubric": "task_relative_output_outcome_patch_focus_v2",
                "included_components": ["outcome", "patch_focus"],
                "normalization_denominator_points": 75,
                "excluded_components": [
                    "validation_discipline",
                    "runtime_efficiency",
                    "brain_use",
                ],
                "critical_failure_forces_zero": True,
            },
            "timeout_policy": {
                "policy": "retain_measured_end_to_end_elapsed_no_component_cap_substitution",
                "elapsed_field": "timing.end_to_end_user_visible_wall_seconds",
                "agent_timeout_limit_seconds": 100.0,
                "provider_retry_limit": 0,
                "substitute_component_timeout_limit": False,
                "status": status,
            },
        },
        "contract_sha256",
    )


def price_quote() -> dict:
    return with_self_hash(
        {
            "schema": "agent-brain-price-quote/v2",
            "status": "pinned",
            "currency": "USD",
            "tokens_per_price_unit": 1_000_000,
            "usage_semantics": {
                "input_tokens_includes": ["cache_read_input", "cache_write_input"],
                "output_tokens_includes": ["reasoning_output"],
                "counter_absence_means_zero": {
                    "cache_read_input": False,
                    "cache_write_input": False,
                    "reasoning_output": False,
                },
            },
            "prices_usd_per_unit": {
                "uncached_input": "2",
                "cache_read_input": "0.5",
                "cache_write_input": "2",
                "visible_output": "8",
                "reasoning_output": None,
            },
            "price_aliases": {
                "uncached_input": None,
                "cache_read_input": None,
                "cache_write_input": None,
                "visible_output": None,
                "reasoning_output": "visible_output",
            },
        },
        "quote_sha256",
    )


def frozen_runner_identity(
    *, schedule_sha256: str = "b" * 64, effort: str = "low"
) -> dict:
    return with_self_hash(
        {
            "schema": confirmatory.FROZEN_RUNNER_IDENTITY_SCHEMA,
            "provider": "synthetic-provider",
            "runner_id": "runner",
            "runner_version": "runner-v1",
            "agent_id": "codex",
            "agent_cli": "codex",
            "agent_cli_version": "codex-test-v1",
            "requested_model_id": "model",
            "resolved_model_id": "model",
            "effort": effort,
            "schedule_sha256": schedule_sha256,
        },
        "identity_sha256",
    )


def cell_execution_identity(
    runner: dict,
    quote: dict,
    *,
    structural_zero: bool = False,
) -> dict:
    return with_self_hash(
        {
            "schema": confirmatory.EXECUTION_IDENTITY_SCHEMA,
            "provider": runner["provider"],
            "runner_id": runner["runner_id"],
            "runner_version": runner["runner_version"],
            "agent_id": runner["agent_id"],
            "agent_cli": runner["agent_cli"],
            "agent_cli_version": runner["agent_cli_version"],
            "requested_model_id": runner["requested_model_id"],
            "resolved_model_id": runner["resolved_model_id"],
            "resolved_model_attestation": (
                "frozen_expected_no_agent_invocation"
                if structural_zero
                else "agent_cli_reported"
            ),
            "effort": runner["effort"],
            "schedule_sha256": runner["schedule_sha256"],
            "price_quote_sha256": quote["quote_sha256"],
            "frozen_runner_identity_sha256": runner["identity_sha256"],
        },
        "identity_sha256",
    )


def billing(quote: dict, multiplier: float) -> dict:
    uncached = int(10_000 * multiplier)
    cached = int(4_000 * multiplier)
    cache_write = int(2_000 * multiplier)
    output = int(1_000 * multiplier)
    reasoning = int(500 * multiplier)
    return {
        "schema": confirmatory.BILLING_SCHEMA,
        "price_quote_sha256": quote["quote_sha256"],
        "raw": {
            "input_tokens": uncached + cached + cache_write,
            "cache_read_input_tokens": cached,
            "cache_write_input_tokens": cache_write,
            "output_tokens": output + reasoning,
            "reasoning_tokens": reasoning,
        },
        "semantics": {
            "input_tokens_includes": ["cache_read_input", "cache_write_input"],
            "output_tokens_includes": ["reasoning_output"],
            "counter_absence_means_zero": {
                "cache_read_input": False,
                "cache_write_input": False,
                "reasoning_output": False,
            },
        },
        "exclusive": {
            "uncached_input": uncached,
            "cache_read_input": cached,
            "cache_write_input": cache_write,
            "visible_output": output,
            "reasoning_output": reasoning,
        },
    }


def attempt_usage(quote: dict, multiplier: float) -> dict:
    attempt_billing = billing(quote, multiplier)
    return {
        "input_tokens": attempt_billing["raw"]["input_tokens"],
        "cache_read_tokens": attempt_billing["raw"]["cache_read_input_tokens"],
        "cache_creation_tokens": attempt_billing["raw"]["cache_write_input_tokens"],
        "output_tokens": attempt_billing["raw"]["output_tokens"],
        "reasoning_tokens": attempt_billing["raw"]["reasoning_tokens"],
        "total_tokens": int(17_500 * multiplier),
        "usage_report": {
            "complete": True,
            "parser": "synthetic_fixture_v1",
            "accounting_basis": "attempt_total",
        },
        "billing_v2": attempt_billing,
    }


def make_records(
    *,
    tasks: tuple[str, ...] = ("task-a", "task-b", "task-c", "task-d"),
    repetitions: int = 2,
    time_ratio: float = 0.80,
    cost_ratio: float = 0.80,
    quality_lift: float = 0.10,
    runner_identity: dict | None = None,
) -> list[dict]:
    quote = price_quote()
    frozen_runner = runner_identity or frozen_runner_identity()
    records: list[dict] = []
    for task_index, task_id in enumerate(tasks, 1):
        baseline_time = 10.0 * task_index
        for repetition in range(1, repetitions + 1):
            for arm in confirmatory.PRIMARY_ARMS:
                condition = CONDITIONS[arm]
                time_multiplier = time_ratio if arm == "retrieved_memory" else 1.0
                cost_multiplier = cost_ratio if arm == "retrieved_memory" else 1.0
                quality = 0.70 + (quality_lift if arm == "retrieved_memory" else 0.0)
                per_attempt_usage = attempt_usage(quote, cost_multiplier)
                aggregate_usage = copy.deepcopy(per_attempt_usage)
                aggregate_usage["usage_report"] = {
                    "complete": True,
                    "parser": "provider_attempt_sum_v1",
                    "accounting_basis": "sum_per_isolated_provider_invocation_attempt_total",
                    "attempt_count": 1,
                    "complete_attempts": [1],
                    "incomplete_attempts": [],
                }
                records.append(
                    {
                        "run_id": f"{task_id}__runner__{condition}__r{repetition}",
                        "task_id": task_id,
                        "runner": {
                            "id": "runner",
                            "agent": frozen_runner["agent_id"],
                            "model": frozen_runner["requested_model_id"],
                            "effort": frozen_runner["effort"],
                        },
                        "condition": condition,
                        "repetition": repetition,
                        "treatment_started": True,
                        "agent_ran": True,
                        "treatment": {
                            "arm": arm,
                            "query_source": "user_query",
                            "confirmatory_eligible": True,
                        },
                        "retrieval_query_source": "user_query",
                        "validation": {"ok": True},
                        "agent_info": {
                            "returncode": 0,
                            "resolved_model": frozen_runner["resolved_model_id"],
                            "execution_identity": cell_execution_identity(
                                frozen_runner, quote
                            ),
                            "provider_invocation": {
                                "schema": confirmatory.PROVIDER_INVOCATION_SCHEMA,
                                "state": confirmatory.PROVIDER_INVOCATIONS_OBSERVED,
                                "invocation_count": 1,
                                "attestation": "retained_attempt_ledger",
                                "reason": None,
                            },
                            "attempts": [
                                {
                                    "attempt": 1,
                                    "returncode": 0,
                                    "resolved_model": frozen_runner["resolved_model_id"],
                                    "usage": per_attempt_usage,
                                }
                            ],
                            "billing_integrity": {
                                "schema": "agent-brain-attempt-billing-integrity/v1",
                                "required": True,
                                "attempt_count": 1,
                                "complete_attempts": 1,
                                "incomplete_attempts": [],
                                "aggregate_present": True,
                                "passed": True,
                                "aggregation": "sum_mutually_exclusive_categories_across_isolated_invocations",
                            },
                            "usage": aggregate_usage,
                        },
                        "timing": {
                            "primary": "end_to_end_user_visible_wall_seconds",
                            "end_to_end_user_visible_wall_seconds": baseline_time * time_multiplier,
                            "timeout_occurred": False,
                            "timeout_stage": None,
                            "agent_timeout_limit_seconds": 100.0,
                            "timeout_component_limit_seconds": None,
                            "harness_agent_interval_wall_seconds": baseline_time * 0.9 * time_multiplier,
                            "agent_reported_api_seconds": baseline_time * 0.8 * time_multiplier,
                            "pre_treatment_setup_included_in_primary": False,
                            "treatment_retrieval_included_in_primary": True,
                            "hidden_validation_included_in_primary": False,
                        },
                        "code_quality": {
                            "schema": confirmatory.QUALITY_SCHEMA,
                            "task_normalized_score": quality,
                            "critical_failure": False,
                            "critical_failure_reasons": [],
                        },
                    }
                )
    return records


def make_structural_zero(
    record: dict,
    *,
    reason: str = "treatment_retrieval_or_delivery_failure",
    runner_identity: dict | None = None,
) -> None:
    quote = price_quote()
    frozen_runner = runner_identity or frozen_runner_identity(
        effort=record["runner"]["effort"]
    )
    zero_billing = billing(quote, 0.0)
    record["agent_ran"] = False
    record["error"] = reason
    record["validation"] = {"ok": False}
    record["agent_info"] = {
        "returncode": None,
        "execution_identity": cell_execution_identity(
            frozen_runner, quote, structural_zero=True
        ),
        "provider_invocation": {
            "schema": confirmatory.PROVIDER_INVOCATION_SCHEMA,
            "state": confirmatory.STRUCTURAL_ZERO_NO_PROVIDER_INVOCATION,
            "invocation_count": 0,
            "attestation": "harness_control_flow_run_agent_not_entered",
            "reason": reason,
        },
        "attempts": [],
        "billing_integrity": {
            "schema": "agent-brain-attempt-billing-integrity/v1",
            "required": True,
            "attempt_count": 0,
            "complete_attempts": 0,
            "incomplete_attempts": [],
            "aggregate_present": True,
            "passed": True,
            "aggregation": confirmatory.STRUCTURAL_ZERO_NO_PROVIDER_INVOCATION,
        },
        "usage": {
            "total_tokens": 0,
            "billing_v2": zero_billing,
            "usage_report": {
                "complete": True,
                "parser": confirmatory.STRUCTURAL_ZERO_NO_PROVIDER_INVOCATION,
                "accounting_basis": confirmatory.STRUCTURAL_ZERO_NO_PROVIDER_INVOCATION,
                "source_events": 0,
                "attempt_count": 0,
                "complete_attempts": [],
                "incomplete_attempts": [],
                "error": None,
            },
        },
    }
    record["code_quality"].update(
        {
            "task_normalized_score": 0.0,
            "critical_failure": True,
            "critical_failure_reasons": [reason],
        }
    )


def schedule_cells(records: list[dict]) -> list[dict]:
    return [
        {
            "run_id": record["run_id"],
            "task_id": record["task_id"],
            "runner": record["runner"],
            "condition": record["condition"],
            "repetition": record["repetition"],
        }
        for record in records
    ]


class ConfirmatoryAnalysisV2Tests(unittest.TestCase):
    def analyze(
        self,
        records: list[dict],
        *,
        repetitions: int = 2,
        contract: dict | None = None,
        quote: dict | None = None,
        runner_identity: dict | None = None,
    ) -> dict:
        tasks = sorted({record["task_id"] for record in records})
        return confirmatory.analyze_records(
            records,
            expected_task_ids=tasks,
            expected_runner_id="runner",
            success_contract=contract or success_contract(),
            price_quote=quote or price_quote(),
            runner_identity=runner_identity or frozen_runner_identity(),
            repetitions=repetitions,
            resamples=199,
            seed=1234,
        )

    def test_all_three_practical_superiority_endpoints_pass_jointly(self) -> None:
        report = self.analyze(make_records())
        self.assertEqual(report["schema"], "agent-brain-confirmatory-analysis/v2")
        self.assertTrue(report["joint_superiority"]["passed"])
        self.assertEqual(report["joint_superiority"]["method"], "intersection_union_all_three_component_nulls_must_be_rejected")
        endpoints = report["primary_endpoints"]
        self.assertAlmostEqual(endpoints["elapsed_time"]["paired_task_geometric_mean_ratio"], 0.8)
        self.assertAlmostEqual(endpoints["normalized_cost"]["equal_task_weighted_mean_ratio"], 0.8)
        self.assertAlmostEqual(endpoints["code_quality"]["paired_task_mean_difference"], 0.1)
        self.assertAlmostEqual(report["arm_summary"]["no_memory"]["mean_normalized_cost_usd"], 0.038)
        self.assertTrue(all(item["clears_practical_floor"] for item in endpoints.values()))
        self.assertEqual(report["design"]["cluster_unit"], "task_not_repetition")
        self.assertFalse(report["diagnostics"]["successful_attempt_only_metrics_computed"])

    def test_one_endpoint_failure_fails_intersection_union_verdict(self) -> None:
        report = self.analyze(make_records(quality_lift=0.02))
        self.assertTrue(report["primary_endpoints"]["elapsed_time"]["clears_practical_floor"])
        self.assertTrue(report["primary_endpoints"]["normalized_cost"]["clears_practical_floor"])
        self.assertFalse(report["primary_endpoints"]["code_quality"]["clears_practical_floor"])
        self.assertFalse(report["joint_superiority"]["passed"])

    def test_merely_statistical_gain_does_not_clear_practical_floor(self) -> None:
        report = self.analyze(make_records(time_ratio=0.95))
        time = report["primary_endpoints"]["elapsed_time"]
        self.assertLess(time["paired_task_geometric_mean_ratio"], 1.0)
        self.assertGreater(time["one_sided_percentile_upper_bound"], time["practical_ratio_max"])
        self.assertFalse(time["clears_practical_floor"])
        self.assertFalse(report["joint_superiority"]["passed"])

    def test_provisional_floors_can_be_computed_but_cannot_pass(self) -> None:
        report = self.analyze(make_records(), contract=success_contract(frozen=False))
        self.assertTrue(report["joint_superiority"]["all_practical_floors_cleared"])
        self.assertFalse(report["joint_superiority"]["success_contract_frozen"])
        self.assertEqual(report["joint_superiority"]["status"], "not_eligible_contract_not_frozen")
        self.assertFalse(report["joint_superiority"]["passed"])

    def test_missing_cost_category_fails_closed(self) -> None:
        records = make_records()
        del records[0]["agent_info"]["usage"]["billing_v2"]["exclusive"]["reasoning_output"]
        with self.assertRaisesRegex(confirmatory.AnalysisInputError, "missing a cost category"):
            self.analyze(records)

    def test_inclusive_usage_is_normalized_without_double_counting(self) -> None:
        normalized = confirmatory.normalize_billing_usage(
            {
                "input_tokens": 100,
                "cache_read_input_tokens": 40,
                "cache_write_input_tokens": 10,
                "output_tokens": 30,
                "reasoning_tokens": 10,
            },
            {
                "input_tokens_includes": ["cache_read_input", "cache_write_input"],
                "output_tokens_includes": ["reasoning_output"],
                "counter_absence_means_zero": {
                    "cache_read_input": False,
                    "cache_write_input": False,
                    "reasoning_output": False,
                },
            },
        )
        self.assertEqual(
            normalized,
            {
                "uncached_input": 50,
                "cache_read_input": 40,
                "cache_write_input": 10,
                "visible_output": 20,
                "reasoning_output": 10,
            },
        )
        self.assertEqual(sum(normalized.values()), 130)

    def test_cache_write_tokens_cannot_be_omitted(self) -> None:
        records = make_records()
        billing_record = records[0]["agent_info"]["usage"]["billing_v2"]
        del billing_record["raw"]["cache_write_input_tokens"]
        with self.assertRaisesRegex(confirmatory.AnalysisInputError, "must contain exactly"):
            self.analyze(records)

    def test_confirmatory_provider_retries_are_rejected(self) -> None:
        records = make_records()
        target = records[0]
        second = copy.deepcopy(target["agent_info"]["attempts"][0])
        second["attempt"] = 2
        target["agent_info"]["attempts"].append(second)
        target["agent_info"]["billing_integrity"].update(
            {"attempt_count": 2, "complete_attempts": 2}
        )
        target["agent_info"]["provider_invocation"]["invocation_count"] = 2
        target["agent_info"]["usage"]["usage_report"].update(
            {"attempt_count": 2, "complete_attempts": [1, 2]}
        )
        aggregate = target["agent_info"]["usage"]["billing_v2"]
        for value in (aggregate["raw"], aggregate["exclusive"]):
            for key in value:
                value[key] *= 2
        with self.assertRaisesRegex(confirmatory.AnalysisInputError, "retry limit is zero"):
            self.analyze(records)

    def test_timeout_retains_measured_end_to_end_time_without_component_substitution(self) -> None:
        records = make_records()
        target = next(record for record in records if record["treatment"]["arm"] == "retrieved_memory")
        target["timing"].update(
            {
                "timeout_occurred": True,
                "timeout_stage": "agent_execution",
                "timeout_component_limit_seconds": 100.0,
                "end_to_end_user_visible_wall_seconds": 101.2,
            }
        )
        target["validation"]["ok"] = False
        target["code_quality"].update(
            {
                "task_normalized_score": 0.0,
                "critical_failure": True,
                "critical_failure_reasons": ["agent_execution_timeout"],
            }
        )
        report = self.analyze(records)
        self.assertEqual(report["integrity"]["timed_out_attempts"], 1)
        self.assertEqual(report["integrity"]["critical_quality_zeroes"], 1)
        self.assertAlmostEqual(
            report["arm_summary"]["retrieved_memory"][
                "mean_end_to_end_user_visible_wall_seconds"
            ],
            31.65,
        )
        self.assertFalse(report["primary_endpoints"]["elapsed_time"]["clears_practical_floor"])

        target["timing"]["agent_timeout_limit_seconds"] = 90.0
        with self.assertRaisesRegex(confirmatory.AnalysisInputError, "agent timeout limit differs"):
            self.analyze(records)

    def test_retrieval_timeout_before_provider_is_authenticated_structural_zero(self) -> None:
        records = make_records()
        target = records[0]
        make_structural_zero(
            target,
            reason="treatment_retrieval_or_delivery_timeout",
        )
        target["timing"].update(
            {
                "timeout_occurred": True,
                "timeout_stage": "treatment_retrieval_or_delivery",
                "timeout_component_limit_seconds": 30.0,
                "end_to_end_user_visible_wall_seconds": 30.4,
            }
        )
        report = self.analyze(records)
        self.assertEqual(report["integrity"]["structural_zero_no_provider_attempts"], 1)
        self.assertEqual(target["agent_info"]["usage"]["billing_v2"]["raw"]["input_tokens"], 0)

    def test_all_retrieved_structural_zero_costs_are_retained_without_log_crash(self) -> None:
        records = make_records()
        for record in records:
            if record["treatment"]["arm"] == "retrieved_memory":
                make_structural_zero(record)
        report = self.analyze(records)
        cost = report["primary_endpoints"]["normalized_cost"]
        self.assertEqual(cost["equal_task_weighted_mean_ratio"], 0.0)
        self.assertEqual(cost["bootstrap_zero_denominator_draws"], 0)
        self.assertTrue(cost["clears_practical_floor"])
        self.assertFalse(report["joint_superiority"]["passed"])

    def test_mixed_zero_baseline_tasks_force_conservative_finite_cost_result(self) -> None:
        records = make_records()
        for record in records:
            if record["task_id"] == "task-a" and record["treatment"]["arm"] == "no_memory":
                make_structural_zero(record)
        report = self.analyze(records)
        cost = report["primary_endpoints"]["normalized_cost"]
        self.assertGreater(cost["bootstrap_zero_denominator_draws"], 0)
        self.assertTrue(math.isfinite(cost["one_sided_percentile_upper_bound"]))
        self.assertFalse(cost["clears_practical_floor"])

    def test_all_zero_no_memory_denominator_fails_closed(self) -> None:
        records = make_records()
        for record in records:
            if record["treatment"]["arm"] == "no_memory":
                make_structural_zero(record)
        with self.assertRaisesRegex(
            confirmatory.AnalysisInputError,
            "full-sample no_memory denominator must be positive",
        ):
            self.analyze(records)

    def test_structural_zero_tampering_and_ambiguous_provider_entry_fail_closed(self) -> None:
        records = make_records()
        make_structural_zero(records[0])
        records[0]["agent_info"]["usage"]["billing_v2"]["raw"]["input_tokens"] = 1
        with self.assertRaisesRegex(confirmatory.AnalysisInputError, "exclusive categories do not match|only zeros"):
            self.analyze(records)

        records = make_records()
        make_structural_zero(records[0])
        records[0]["agent_info"]["provider_invocation"]["state"] = "provider_path_entered_usage_unknown"
        with self.assertRaisesRegex(confirmatory.AnalysisInputError, "ambiguous"):
            self.analyze(records)

    def test_quality_out_of_range_fails_and_critical_failure_is_forced_to_zero(self) -> None:
        records = make_records()
        records[0]["code_quality"]["task_normalized_score"] = 1.1
        with self.assertRaisesRegex(confirmatory.AnalysisInputError, r"must be in \[0,1\]"):
            self.analyze(records)

        records = make_records()
        for record in records:
            if record["treatment"]["arm"] == "retrieved_memory":
                record["code_quality"].update(
                    {
                        "task_normalized_score": 1.0,
                        "critical_failure": True,
                        "critical_failure_reasons": ["hidden_validation_failure"],
                    }
                )
                record["code_quality"]["task_normalized_score"] = 0.0
        report = self.analyze(records)
        self.assertEqual(report["arm_summary"]["retrieved_memory"]["mean_task_normalized_quality"], 0.0)
        self.assertFalse(report["primary_endpoints"]["code_quality"]["clears_practical_floor"])

    def test_quality_critical_flag_reasons_and_zero_are_fail_closed(self) -> None:
        records = make_records()
        records[0]["code_quality"]["critical_failure_reasons"] = ["integrity_failure"]
        with self.assertRaisesRegex(confirmatory.AnalysisInputError, "must equal whether"):
            self.analyze(records)

        records = make_records()
        records[0]["code_quality"].update(
            {
                "critical_failure": True,
                "critical_failure_reasons": ["integrity_failure"],
                "task_normalized_score": 0.5,
            }
        )
        with self.assertRaisesRegex(confirmatory.AnalysisInputError, "must record.*zero"):
            self.analyze(records)

        records = make_records()
        records[0]["validation"]["ok"] = False
        with self.assertRaisesRegex(confirmatory.AnalysisInputError, "requires a declared critical"):
            self.analyze(records)

    def test_repetitions_are_averaged_inside_task_clusters(self) -> None:
        records = make_records(repetitions=8)
        report = self.analyze(records, repetitions=8)
        for endpoint in report["primary_endpoints"].values():
            self.assertEqual(endpoint["n_task_clusters"], 4)
        self.assertEqual(report["design"]["requested_primary_cells"], 96)

    def test_record_reordering_is_invariant(self) -> None:
        records = make_records()
        self.assertEqual(self.analyze(records), self.analyze(list(reversed(copy.deepcopy(records)))))

    def test_tampered_price_quote_or_record_binding_fails_closed(self) -> None:
        quote = price_quote()
        tampered = copy.deepcopy(quote)
        tampered["prices_usd_per_unit"]["visible_output"] = "80"
        with self.assertRaisesRegex(confirmatory.AnalysisInputError, "self-hash mismatch"):
            self.analyze(make_records(), quote=tampered)

        tampered["quote_sha256"] = confirmatory._self_hash(tampered, "quote_sha256")
        with self.assertRaisesRegex(
            confirmatory.AnalysisInputError,
            "(?:price quote hash mismatch|price_quote_sha256)",
        ):
            self.analyze(make_records(), quote=tampered)

    def test_cell_identity_rejects_low_vs_high_effort_mismatch(self) -> None:
        runner_identity = frozen_runner_identity(effort="low")
        records = make_records(runner_identity=runner_identity)
        identity = records[0]["agent_info"]["execution_identity"]
        identity["effort"] = "high"
        identity["identity_sha256"] = confirmatory._self_hash(
            identity, "identity_sha256"
        )
        with self.assertRaisesRegex(
            confirmatory.AnalysisInputError, "cell execution identity mismatch: effort"
        ):
            self.analyze(records, runner_identity=runner_identity)

    def test_reasoning_price_alias_resolves_and_invalid_alias_fails(self) -> None:
        quote = price_quote()
        self.assertEqual(
            confirmatory._resolved_prices(quote)["reasoning_output"],
            confirmatory.Decimal("8"),
        )
        invalid = copy.deepcopy(quote)
        invalid["price_aliases"]["reasoning_output"] = "reasoning_output"
        invalid["quote_sha256"] = confirmatory._self_hash(invalid, "quote_sha256")
        with self.assertRaisesRegex(confirmatory.AnalysisInputError, "alias is invalid"):
            self.analyze(make_records(), quote=invalid)

    def test_duplicate_missing_and_nonexecuted_cells_fail_closed(self) -> None:
        records = make_records()
        records[-1] = copy.deepcopy(records[0])
        with self.assertRaisesRegex(confirmatory.AnalysisInputError, "duplicate run_id"):
            self.analyze(records)
        records = make_records()
        records.pop()
        with self.assertRaisesRegex(confirmatory.AnalysisInputError, "expected 24 primary cells"):
            confirmatory.analyze_records(
                records,
                expected_task_ids=("task-a", "task-b", "task-c", "task-d"),
                expected_runner_id="runner",
                success_contract=success_contract(),
                price_quote=price_quote(),
                runner_identity=frozen_runner_identity(),
                repetitions=2,
                resamples=19,
                seed=1,
            )
        records = make_records()
        records[0]["treatment_started"] = False
        with self.assertRaisesRegex(confirmatory.AnalysisInputError, "did not explicitly start"):
            self.analyze(records)

    def test_post_agent_error_is_retained_as_incorrect_and_quality_zero(self) -> None:
        records = make_records()
        target = records[0]
        target.pop("validation")
        target["error"] = "post-agent integrity audit failed"
        target["code_quality"].update(
            {
                "task_normalized_score": 0.0,
                "critical_failure": True,
                "critical_failure_reasons": ["post_agent_integrity_failure"],
            }
        )
        report = self.analyze(records)
        self.assertEqual(report["integrity"]["missing_validation_scored_incorrect"], 1)
        self.assertEqual(report["integrity"]["critical_quality_zeroes"], 1)

    def test_verified_schedule_identity_mismatch_fails_closed(self) -> None:
        records = make_records()
        expected = schedule_cells(records)
        expected[0]["condition"] = "tampered"
        with self.assertRaisesRegex(confirmatory.AnalysisInputError, "disagree with schedule"):
            confirmatory.analyze_records(
                records,
                expected_task_ids=("task-a", "task-b", "task-c", "task-d"),
                expected_runner_id="runner",
                success_contract=success_contract(),
                price_quote=price_quote(),
                runner_identity=frozen_runner_identity(),
                repetitions=2,
                resamples=19,
                seed=1,
                expected_schedule_cells=expected,
            )

    def test_bootstrap_stream_remains_frozen(self) -> None:
        self.assertEqual(
            confirmatory._bootstrap_indices(4, 3, 1234),
            [(0, 1, 1, 1), (1, 3, 0, 0), (2, 0, 0, 0)],
        )

    def test_v2_manifest_validator_enforces_quality_and_timeout_consistency(self) -> None:
        manifest = {
            "schema": evidence.RUN_SCHEMA,
            "run_id": "run",
            "identity_sha256": "a" * 64,
            "executed": True,
            "artifacts": [],
            "execution_gate": {
                "treatment_started": True,
                "agent_ran": True,
                "execution_identity": cell_execution_identity(
                    frozen_runner_identity(), price_quote()
                ),
                "provider_invocation": {
                    "schema": evidence.PROVIDER_INVOCATION_SCHEMA,
                    "state": evidence.PROVIDER_INVOCATIONS_OBSERVED,
                    "invocation_count": 1,
                    "attestation": "retained_attempt_ledger",
                    "reason": None,
                },
                "billing_integrity": {
                    "required": False,
                    "passed": True,
                    "attempt_count": 1,
                },
            },
            "raw_metrics": {
                "score": {},
                "code_quality": {
                    "schema": confirmatory.QUALITY_SCHEMA,
                    "rubric": "task_relative_output_outcome_patch_focus_v2",
                    "task_normalized_score": 0.75,
                    "critical_failure": False,
                    "critical_failure_reasons": [],
                    "excluded_components": [
                        "validation_discipline",
                        "runtime_efficiency",
                        "brain_use",
                    ],
                },
                "usage": {},
                "attempt_usage": [
                    {
                        "attempt": 1,
                        "returncode": 0,
                        "usage": {},
                        "stdout_artifact": {},
                        "stderr_artifact": {},
                    }
                ],
                "duration_seconds": 12.0,
                "timing": {
                    "primary": "end_to_end_user_visible_wall_seconds",
                    "end_to_end_user_visible_wall_seconds": 12.0,
                    "timeout_occurred": False,
                    "timeout_stage": None,
                    "agent_timeout_limit_seconds": 100.0,
                    "timeout_component_limit_seconds": None,
                    "harness_agent_interval_wall_seconds": 10.0,
                    "agent_reported_api_seconds": 9.0,
                    "cell_setup_wall_seconds": 3.0,
                    "cell_total_wall_seconds": 15.0,
                    "pre_treatment_setup_included_in_primary": False,
                    "treatment_retrieval_included_in_primary": True,
                    "hidden_validation_included_in_primary": False,
                },
            },
        }
        self.assertEqual(evidence.validate_run_manifest(manifest), [])
        manifest["raw_metrics"]["code_quality"]["critical_failure_reasons"] = ["failed"]
        self.assertIn(
            "v2 run manifest critical flag/reasons disagree",
            evidence.validate_run_manifest(manifest),
        )
        manifest["raw_metrics"]["code_quality"]["critical_failure_reasons"] = []
        manifest["raw_metrics"]["timing"].update(
            {
                "timeout_occurred": True,
                "timeout_stage": "agent_execution",
                "timeout_component_limit_seconds": None,
            }
        )
        self.assertIn(
            "v2 run manifest component timeout limit must be positive",
            evidence.validate_run_manifest(manifest),
        )

    def test_v2_manifest_execution_identity_tracks_pricing_binding(self) -> None:
        def manifest(identity: Any, required: bool) -> dict[str, Any]:
            base = {
                "schema": evidence.RUN_SCHEMA,
                "run_id": "run",
                "identity_sha256": "a" * 64,
                "executed": True,
                "artifacts": [],
                "execution_gate": {
                    "treatment_started": True,
                    "agent_ran": True,
                    "execution_identity": identity,
                    "provider_invocation": {
                        "schema": evidence.PROVIDER_INVOCATION_SCHEMA,
                        "state": evidence.PROVIDER_INVOCATIONS_OBSERVED,
                        "invocation_count": 1,
                        "attestation": "retained_attempt_ledger",
                        "reason": None,
                    },
                    "billing_integrity": {
                        "required": required,
                        "passed": True,
                        "attempt_count": 1,
                    },
                },
                "raw_metrics": {
                    "score": {},
                    "code_quality": {
                        "schema": confirmatory.QUALITY_SCHEMA,
                        "rubric": "task_relative_output_outcome_patch_focus_v2",
                        "task_normalized_score": 0.75,
                        "critical_failure": False,
                        "critical_failure_reasons": [],
                        "excluded_components": [
                            "validation_discipline",
                            "runtime_efficiency",
                            "brain_use",
                        ],
                    },
                    "usage": {},
                    "attempt_usage": [
                        {
                            "attempt": 1,
                            "returncode": 0,
                            "usage": {},
                            "stdout_artifact": {},
                            "stderr_artifact": {},
                        }
                    ],
                    "duration_seconds": 12.0,
                    "timing": {
                        "primary": "end_to_end_user_visible_wall_seconds",
                        "end_to_end_user_visible_wall_seconds": 12.0,
                        "timeout_occurred": False,
                        "timeout_stage": None,
                        "agent_timeout_limit_seconds": 100.0,
                        "timeout_component_limit_seconds": None,
                        "harness_agent_interval_wall_seconds": 10.0,
                        "agent_reported_api_seconds": 9.0,
                        "cell_setup_wall_seconds": 3.0,
                        "cell_total_wall_seconds": 15.0,
                        "pre_treatment_setup_included_in_primary": False,
                        "treatment_retrieval_included_in_primary": True,
                        "hidden_validation_included_in_primary": False,
                    },
                },
            }
            return base

        # No pricing quote bound at launch: an identity cannot exist and its
        # absence must not fail verification (the estimate-free evidence lane).
        self.assertEqual(evidence.validate_run_manifest(manifest(None, False)), [])
        # A bound quote makes the identity mandatory.
        self.assertIn(
            "v2 run manifest cell execution identity is required when a pricing quote is bound",
            evidence.validate_run_manifest(manifest(None, True)),
        )
        # A present identity must validate regardless of billing binding.
        self.assertIn(
            "v2 run manifest cell execution identity is invalid",
            evidence.validate_run_manifest(manifest({"schema": "bogus"}, False)),
        )
        valid = cell_execution_identity(frozen_runner_identity(), price_quote())
        self.assertEqual(evidence.validate_run_manifest(manifest(valid, True)), [])

    def test_v2_manifest_represents_pre_treatment_non_outcome_truthfully(self) -> None:
        record = {
            "treatment_started": False,
            "agent_ran": False,
            "run_id": "pre-treatment",
            "task_id": "task",
            "runner": {"id": "runner"},
            "condition": "control",
            "error": "preflight failed",
            "score": {"total": 0},
            "code_quality": {
                "schema": confirmatory.QUALITY_SCHEMA,
                "rubric": "task_relative_output_outcome_patch_focus_v2",
                "task_normalized_score": 0.0,
                "critical_failure": True,
                "critical_failure_reasons": ["pre_treatment_failure"],
                "excluded_components": [
                    "validation_discipline",
                    "runtime_efficiency",
                    "brain_use",
                ],
            },
            "timing": {
                "primary": "end_to_end_user_visible_wall_seconds",
                "end_to_end_user_visible_wall_seconds": None,
                "timeout_occurred": False,
                "timeout_stage": None,
                "agent_timeout_limit_seconds": 100.0,
                "timeout_component_limit_seconds": None,
                "harness_agent_interval_wall_seconds": None,
                "agent_reported_api_seconds": None,
                "cell_setup_wall_seconds": 1.0,
                "cell_total_wall_seconds": 1.0,
                "pre_treatment_setup_included_in_primary": False,
                "treatment_retrieval_included_in_primary": True,
                "hidden_validation_included_in_primary": False,
            },
        }
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            manifest = evidence.build_run_manifest(record, root, root)
        manifest["identity_sha256"] = "a" * 64
        self.assertFalse(manifest["executed"])
        self.assertEqual(
            manifest["execution_gate"]["provider_invocation"]["state"],
            evidence.PRE_TREATMENT_PROVIDER_PATH_NOT_ENTERED,
        )
        self.assertEqual(evidence.validate_run_manifest(manifest), [])

    def test_verified_suite_interface_uses_schedule_not_holdout_metadata(self) -> None:
        records = make_records(tasks=("task-a", "task-b"))
        cells = schedule_cells(records)
        schedule = {"schema": 1, "repetitions": 2, "cells": cells}
        schedule["schedule_sha256"] = confirmatory._stable_json_sha256(schedule)
        runner_identity = frozen_runner_identity(
            schedule_sha256=schedule["schedule_sha256"]
        )
        records = make_records(
            tasks=("task-a", "task-b"), runner_identity=runner_identity
        )
        cells = schedule_cells(records)
        state = {
            "schedule_sha256": schedule["schedule_sha256"],
            "planned_cell_count": len(cells),
            "recorded_cell_count": len(cells),
            "actual_started_order": [cell["run_id"] for cell in cells],
            "actual_finished_order": [cell["run_id"] for cell in cells],
            "deviations": [],
        }
        identity_records = evidence.current_analyzer_records(HERE)
        aggregate = evidence.analyzer_aggregate_sha256(identity_records)
        manifest = {
            "suite_id": "fixture-suite",
            "identity_sha256": "fixture-identity",
            "harness": {"confirmatory_eligible": True},
            "analyzer": {
                "algorithm": evidence.ANALYZER_AGGREGATE_ALGORITHM,
                "aggregate_sha256": aggregate,
            },
            "requested_cells": {
                "count": len(cells),
                "schedule": {"schedule_sha256": schedule["schedule_sha256"], "cell_count": len(cells)},
            },
        }
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            (root / "evidence-manifest.json").write_text(json.dumps(manifest))
            (root / "schedule.json").write_text(json.dumps(schedule))
            (root / "schedule-state.json").write_text(json.dumps(state))
            (root / "records.ndjson").write_text("".join(json.dumps(record) + "\n" for record in records))
            with mock.patch.object(confirmatory, "verify_bundle", return_value={"ok": True, "errors": []}):
                report = confirmatory.analyze_verified_suite(
                    root,
                    expected_analyzer_sha256=aggregate,
                    success_contract=success_contract(),
                    price_quote=price_quote(),
                    runner_identity=runner_identity,
                    expected_task_count=2,
                    repetitions=2,
                    resamples=199,
                    seed=1234,
                )
        self.assertTrue(report["integrity"]["evidence_bundle_verified"])
        self.assertFalse(report["integrity"]["holdout_metadata_loaded"])
        self.assertTrue(report["analyzer_identity"]["matched"])

    def test_analyzer_lock_loader_rejects_tampered_aggregate(self) -> None:
        records = evidence.current_analyzer_records(HERE)
        lock = {
            "schema_version": 1,
            "algorithm": evidence.ANALYZER_AGGREGATE_ALGORITHM,
            "files": [{"path": item["source_path"], "sha256": item["sha256"]} for item in records],
            "aggregate_sha256": evidence.analyzer_aggregate_sha256(records),
        }
        with tempfile.TemporaryDirectory() as temporary:
            path = pathlib.Path(temporary) / "analyzer-lock.json"
            path.write_text(json.dumps(lock))
            self.assertEqual(confirmatory.load_analyzer_lock(path), lock["aggregate_sha256"])
            lock["aggregate_sha256"] = "0" * 64
            path.write_text(json.dumps(lock))
            with self.assertRaisesRegex(confirmatory.AnalysisInputError, "aggregate hash mismatch"):
                confirmatory.load_analyzer_lock(path)


if __name__ == "__main__":
    unittest.main()
