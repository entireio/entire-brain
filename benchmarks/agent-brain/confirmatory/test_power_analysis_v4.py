from __future__ import annotations

import copy
from decimal import Decimal
import importlib.util
import json
import math
import pathlib
import tempfile
import unittest


HERE = pathlib.Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location(
    "power_analysis_v4", HERE / "power_analysis_v4.py"
)
assert SPEC and SPEC.loader
V4 = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(V4)
PRODUCT = V4.PRODUCT


def digest(label: str) -> str:
    return PRODUCT.value_sha256({"fixture": label})


def decimal_text(value: float | int | Decimal) -> str:
    rendered = format(Decimal(str(value)), "f")
    if "." in rendered:
        rendered = rendered.rstrip("0").rstrip(".")
    return rendered or "0"


def product_identity(role: str, marker: str) -> dict:
    value = {
        "role": role,
        "commit_oid": marker * 40,
        "tree_oid": ("c" if marker == "a" else "d") * 40,
        "binary_sha256": digest(f"{marker}-binary"),
        "config_sha256": digest(f"{marker}-config"),
        "packet_format": {
            "id": f"agent-brain-packet/{marker}",
            "sha256": digest(f"{marker}-packet-format"),
        },
    }
    value["identity_sha256"] = PRODUCT.self_sha256(value)
    return value


def outcome(*, elapsed: float, cost: float, quality: float) -> dict:
    cost_text = decimal_text(cost)
    value = {
        "executed": True,
        "timing": {
            "primary": "end_to_end_user_visible_wall_seconds",
            "end_to_end_user_visible_wall_seconds": elapsed,
            "timeout_occurred": False,
            "timeout_stage": None,
            "agent_timeout_limit_seconds": 900,
            "timeout_component_limit_seconds": None,
            "pre_treatment_setup_included_in_primary": False,
            "treatment_retrieval_included_in_primary": True,
            "hidden_validation_included_in_primary": False,
        },
        "normalized_cost": {
            "schema": PRODUCT.NORMALIZED_COST_SCHEMA,
            "currency": "USD",
            "complete": True,
            "categories": {
                "uncached_input": cost_text,
                "cache_read_input": "0",
                "cache_write_input": "0",
                "visible_output": "0",
                "reasoning_output": "0",
            },
            "total_usd": cost_text,
        },
        "code_quality": {
            "schema": PRODUCT.QUALITY_SCHEMA,
            "rubric": PRODUCT.QUALITY_RUBRIC,
            "task_normalized_score": quality,
            "critical_failure": False,
            "critical_failure_reasons": [],
            "excluded_components": list(PRODUCT.QUALITY_EXCLUSIONS),
        },
        "failure": {
            "occurred": False,
            "kind": None,
            "return_code": 0,
            "reason_codes": [],
        },
        "validation": {"ok": True, "reason_codes": []},
    }
    value["identity_sha256"] = PRODUCT.self_sha256(value)
    return value


def metric_mean(
    task_index: int,
    arm: str,
    *,
    homogeneous: bool,
    structural_zero: bool,
) -> tuple[float, float, float]:
    if homogeneous:
        grid = {
            "no_memory": (20.0, 10.0, 0.30),
            "placebo_packet": (19.0, 9.0, 0.35),
            "preoptimization_memory": (14.0, 7.0, 0.55),
            "retrieved_memory": (8.0, 4.0, 0.85),
        }
        elapsed, cost, quality = grid[arm]
    elif task_index < 6:
        grid = {
            "no_memory": (20.0, 10.0, 0.30),
            "placebo_packet": (19.0, 9.0, 0.32),
            "preoptimization_memory": (14.0, 8.0, 0.50),
            "retrieved_memory": (8.0, 2.0, 0.70),
        }
        elapsed, cost, quality = grid[arm]
    else:
        grid = {
            "no_memory": (20.0, 100.0, 0.40),
            "placebo_packet": (19.0, 95.0, 0.42),
            "preoptimization_memory": (18.0, 90.0, 0.50),
            "retrieved_memory": (16.0, 80.0, 0.60),
        }
        elapsed, cost, quality = grid[arm]
    if structural_zero and arm == "retrieved_memory":
        cost = 0.0
    return elapsed, cost, quality


def product_cycle_fixture(
    *,
    evidence_class: str = "synthetic_fixture",
    homogeneous: bool = True,
    structural_zero: bool = False,
) -> dict:
    tasks = [
        {
            "task_id": f"task-{index:02d}",
            "task_sha256": digest(f"task-{index:02d}"),
            "prompt_parity_sha256": digest(f"task-{index:02d}-prompt-parity"),
        }
        for index in range(12)
    ]
    baseline = product_identity("preoptimization_memory", "a")
    candidate = product_identity("retrieved_memory_optimized_current", "b")

    schedule_cells = []
    position = 0
    for task in tasks:
        for repetition in (1, 2):
            for arm in PRODUCT.ARMS:
                position += 1
                schedule_cells.append(
                    {
                        "position": position,
                        "task_id": task["task_id"],
                        "repetition": repetition,
                        "arm": arm,
                    }
                )
    schedule = {
        "schema": PRODUCT.SCHEDULE_SCHEMA,
        "order_policy": "explicit_same_schedule_four_arm_v1",
        "cells": schedule_cells,
    }
    schedule["identity_sha256"] = PRODUCT.self_sha256(schedule)
    task_inventory_sha256 = PRODUCT.value_sha256(tasks)
    shared = {
        "task_inventory_sha256": task_inventory_sha256,
        "corpus_sha256": digest("corpus"),
        "engine_sha256": digest("engine"),
        "prompt_template_sha256": digest("prompt-template"),
        "prompt_parity_algorithm": PRODUCT.PROMPT_PARITY_ALGORITHM,
        "cache_policy_sha256": digest("cache-policy"),
        "runner_sha256": digest("runner"),
        "price_quote_sha256": digest("price-quote"),
        "pricing_policy_sha256": digest("pricing-policy"),
        "model_id": "synthetic-model",
        "effort": "synthetic-medium",
        "schedule_sha256": schedule["identity_sha256"],
    }
    shared["identity_sha256"] = PRODUCT.self_sha256(shared)

    task_by_id = {task["task_id"]: task for task in tasks}
    task_index = {task["task_id"]: index for index, task in enumerate(tasks)}
    product_hashes = {
        "baseline": baseline["identity_sha256"],
        "candidate": candidate["identity_sha256"],
    }
    cells = []
    for scheduled in schedule_cells:
        task_id = scheduled["task_id"]
        repetition = scheduled["repetition"]
        arm = scheduled["arm"]
        role = PRODUCT.ARM_PRODUCT_ROLES[arm]
        product_sha = product_hashes[role]
        execution = {
            "task_sha256": task_by_id[task_id]["task_sha256"],
            "prompt_parity_sha256": task_by_id[task_id]["prompt_parity_sha256"],
            "task_inventory_sha256": task_inventory_sha256,
            "corpus_sha256": shared["corpus_sha256"],
            "engine_sha256": shared["engine_sha256"],
            "prompt_template_sha256": shared["prompt_template_sha256"],
            "prompt_parity_algorithm": shared["prompt_parity_algorithm"],
            "cache_policy_sha256": shared["cache_policy_sha256"],
            "runner_sha256": shared["runner_sha256"],
            "price_quote_sha256": shared["price_quote_sha256"],
            "pricing_policy_sha256": shared["pricing_policy_sha256"],
            "model_id": shared["model_id"],
            "effort": shared["effort"],
            "schedule_sha256": shared["schedule_sha256"],
            "product_identity_sha256": product_sha,
        }
        execution["identity_sha256"] = PRODUCT.self_sha256(execution)
        elapsed_mean, cost_mean, quality_mean = metric_mean(
            task_index[task_id],
            arm,
            homogeneous=homogeneous,
            structural_zero=structural_zero,
        )
        elapsed = elapsed_mean * (0.8 if repetition == 1 else 1.2)
        cost = cost_mean * (0.5 if repetition == 1 else 1.5)
        quality = quality_mean + (-0.02 if repetition == 1 else 0.02)
        cell = {
            "run_id": f"{task_id}-r{repetition}-{arm}",
            "schedule_position": scheduled["position"],
            "task_id": task_id,
            "repetition": repetition,
            "arm": arm,
            "product_identity_sha256": product_sha,
            "execution_identity": execution,
            "outcome": outcome(elapsed=elapsed, cost=cost, quality=quality),
        }
        cell["identity_sha256"] = PRODUCT.self_sha256(cell)
        cells.append(cell)

    requested_cells = len(tasks) * 2 * len(PRODUCT.ARMS)
    manifest = {
        "schema": PRODUCT.MANIFEST_SCHEMA,
        "purpose": "development_product_cycle_only",
        "evidence_class": evidence_class,
        "claim_scope": "descriptive_not_confirmatory_no_budget_authorization",
        "product_identities": {
            "baseline": baseline,
            "candidate": candidate,
            "binding_sha256": PRODUCT.value_sha256(
                {
                    "baseline": baseline["identity_sha256"],
                    "candidate": candidate["identity_sha256"],
                }
            ),
        },
        "tasks": tasks,
        "task_inventory_sha256": task_inventory_sha256,
        "shared_execution": shared,
        "design": {
            "arms": list(PRODUCT.ARMS),
            "arm_product_roles": dict(PRODUCT.ARM_PRODUCT_ROLES),
            "primary_contrast": dict(PRODUCT.PRIMARY_CONTRAST),
            "diagnostic_contrast": dict(PRODUCT.DIAGNOSTIC_CONTRAST),
            "task_clusters": len(tasks),
            "repetitions_per_arm": 2,
            "requested_cells": requested_cells,
            "agent_retry_limit": 0,
            "replacement_cell_limit": 0,
            "reserve_cell_limit": 0,
            "maximum_agent_invocations": requested_cells,
            "budget": {"status": "not_frozen", "authorized_usd": None},
            "schedule": schedule,
        },
        "cells": cells,
    }
    manifest["manifest_sha256"] = PRODUCT.self_sha256(
        manifest, "manifest_sha256"
    )
    return manifest


def alternatives() -> dict:
    return {
        "primary": {
            "comparison": V4.PRIMARY_COMPARISON,
            "role": "primary_development_contrast",
            "elapsed_time": {
                "required_ratio_max": 0.90,
                "planning_alternative_ratio": 0.60,
            },
            "normalized_cost": {
                "required_ratio_max": 0.90,
                "planning_alternative_ratio": 0.55,
            },
            "code_quality": {
                "required_difference_min": 0.10,
                "planning_alternative_difference": 0.40,
            },
        },
        "product_diagnostic": {
            "comparison": V4.DIAGNOSTIC_COMPARISON,
            "role": "diagnostic_product_progress_only",
            "elapsed_time": {
                "required_ratio_max": 0.95,
                "planning_alternative_ratio": 0.75,
            },
            "normalized_cost": {
                "required_ratio_max": 0.95,
                "planning_alternative_ratio": 0.70,
            },
            "code_quality": {
                "required_difference_min": 0.05,
                "planning_alternative_difference": 0.20,
            },
        },
    }


def calibration_fixture(
    *,
    state: str = "candidate",
    evidence_class: str = "synthetic_fixture",
    homogeneous: bool = True,
    structural_zero: bool = False,
) -> dict:
    product = product_cycle_fixture(
        evidence_class=evidence_class,
        homogeneous=homogeneous,
        structural_zero=structural_zero,
    )
    members = []
    for task in product["tasks"]:
        task_id = task["task_id"]
        member = {
            "member_ref": digest(f"member-ref-{task_id}"),
            "membership": "development_calibration",
            "task_id": task_id,
            "task_id_sha256": V4._task_identity_sha256(task_id),
            "product_task_sha256": task["task_sha256"],
            "task_overlap_commitment_sha256": digest(f"overlap-{task_id}"),
            "contamination_state": "reviewed_clear",
            "review_commitment_sha256": digest(f"review-{task_id}"),
        }
        member["identity_sha256"] = PRODUCT.self_sha256(member)
        members.append(member)
    population = {
        "schema_version": 2,
        "profile": V4.TASK_POPULATION_PROFILE,
        "status": "frozen_unopened" if state == "frozen" else "candidate_unopened",
        "schema_sha256": V4.TASK_POPULATION_SCHEMA_SHA256,
        "population_sha256": digest("task-population-contract"),
        "source_file_sha256": digest("task-population-contract-file"),
        "review_ledger_sha256": digest("review-ledger"),
        "selection_verification_status": "verified",
        "selection_receipt_sha256": digest("selection-receipt"),
        "assignment_verification_status": "verified",
        "assignment_receipt_sha256": digest("assignment-receipt"),
        "overlap_commitment_verification_status": "verified",
        "overlap_commitment_receipt_sha256": digest("overlap-receipt"),
        "calibration_min_independent_tasks": 12,
        "holdout_plaintext": "forbidden",
        "calibration_members": members,
    }
    population["identity_sha256"] = PRODUCT.self_sha256(population)

    candidate_lock = {
        "status": "verified_locked_before_development_calibration",
        "candidate_product_identity_sha256": product["product_identities"]["candidate"][
            "identity_sha256"
        ],
        "precalibration_plan_sha256": V4._precalibration_plan_sha256(product),
        "task_population_contract_sha256": population["population_sha256"],
        "lock_receipt_sha256": digest("candidate-lock-receipt"),
    }
    candidate_lock["identity_sha256"] = PRODUCT.self_sha256(candidate_lock)

    source_bindings = {
        "product_cycle_schema_file_sha256": V4.PRODUCT_CYCLE_SCHEMA_SHA256,
        "product_cycle_evidence_canonical_bytes_sha256": PRODUCT.value_sha256(
            product
        ),
        "task_population_schema_file_sha256": V4.TASK_POPULATION_SCHEMA_SHA256,
        "task_population_contract_file_sha256": population["source_file_sha256"],
        "task_population_contract_sha256": population["population_sha256"],
        "task_population_binding_canonical_bytes_sha256": PRODUCT.value_sha256(
            population
        ),
    }
    source_bindings["identity_sha256"] = PRODUCT.self_sha256(source_bindings)

    candidate = product["product_identities"]["candidate"]
    shared = product["shared_execution"]
    locked = {
        "product_cycle_schema_sha256": V4.PRODUCT_CYCLE_SCHEMA_SHA256,
        "product_cycle_contract_sha256": product["manifest_sha256"],
        "product_cycle_evidence_canonical_bytes_sha256": PRODUCT.value_sha256(
            product
        ),
        "task_population_schema_sha256": V4.TASK_POPULATION_SCHEMA_SHA256,
        "task_population_sha256": population["population_sha256"],
        "task_population_file_sha256": population["source_file_sha256"],
        "candidate_product_identity_sha256": candidate["identity_sha256"],
        "candidate_packet_format_sha256": candidate["packet_format"]["sha256"],
        "corpus_sha256": shared["corpus_sha256"],
        "engine_sha256": shared["engine_sha256"],
        "prompt_template_sha256": shared["prompt_template_sha256"],
        "prompt_parity_algorithm": shared["prompt_parity_algorithm"],
        "cache_policy_sha256": shared["cache_policy_sha256"],
        "runner_sha256": shared["runner_sha256"],
        "model_id": shared["model_id"],
        "effort": shared["effort"],
        "schedule_sha256": shared["schedule_sha256"],
        "price_quote_sha256": shared["price_quote_sha256"],
        "pricing_policy_sha256": shared["pricing_policy_sha256"],
    }
    locked["identity_sha256"] = PRODUCT.self_sha256(locked)

    calibration = {
        "schema": V4.CALIBRATION_SCHEMA,
        "evidence_class": evidence_class,
        "state": state,
        "owner_approval_sha256": digest("owner-approval")
        if state == "frozen"
        else None,
        "candidate_lock": candidate_lock,
        "source_byte_bindings": source_bindings,
        "locked_identities": locked,
        "task_population_binding": population,
        "product_cycle_evidence": product,
        "planning": {
            "method": V4.RESAMPLING_METHOD,
            "target_power": V4.TARGET_POWER,
            "confidence": V4.CONFIDENCE,
            "resamples": 100,
            "seed": 2_026_071_600,
            "candidate_task_clusters": [12, 24],
            "arms": list(PRODUCT.ARMS),
            "repetitions_per_arm": 2,
            "agent_retry_limit": 0,
            "replacement_cell_limit": 0,
            "reserve_cell_limit": 0,
            "planning_alternatives": None if state == "pending" else alternatives(),
        },
    }
    calibration["identity_sha256"] = PRODUCT.self_sha256(calibration)
    return calibration


def reseal_population(calibration: dict) -> None:
    population = calibration["task_population_binding"]
    for member in population["calibration_members"]:
        member["identity_sha256"] = PRODUCT.self_sha256(member)
    population["identity_sha256"] = PRODUCT.self_sha256(population)
    calibration["identity_sha256"] = PRODUCT.self_sha256(calibration)


def reseal_nested(calibration: dict, key: str) -> None:
    calibration[key]["identity_sha256"] = PRODUCT.self_sha256(calibration[key])
    calibration["identity_sha256"] = PRODUCT.self_sha256(calibration)


def reseal_report(report: dict) -> None:
    report["report_sha256"] = PRODUCT.self_sha256(report, "report_sha256")


class PowerAnalysisV4Test(unittest.TestCase):
    def test_preflight_binds_twelve_active_tasks_and_exact_four_arm_cells(self) -> None:
        receipt = V4.preflight_calibration(calibration_fixture())
        self.assertEqual(receipt["status"], "valid")
        self.assertEqual(receipt["independent_active_task_clusters"], 12)
        self.assertEqual(receipt["calibration_repetitions_per_arm"], 2)
        self.assertEqual(receipt["calibration_requested_cells"], 96)
        self.assertEqual(receipt["calibration_validated_cells"], 96)
        self.assertFalse(receipt["budget_authorized"])
        self.assertTrue(receipt["planning_ready"])

    def test_pending_power_is_null_and_cannot_pass(self) -> None:
        report = V4.analyze_calibration(
            calibration_fixture(
                state="pending", evidence_class="development_measurement"
            )
        )
        self.assertEqual(report["status"], "pending")
        for row in report["power_candidates"]:
            self.assertIsNone(row["primary"])
            self.assertIsNone(row["product_diagnostic"])
        for key in ("benchmark_power_decision", "product_improvement_gate"):
            self.assertIsNone(report[key]["selected_task_clusters"])
            self.assertFalse(report[key]["statistical_gate_met"])
            self.assertFalse(report[key]["passed"])
            self.assertEqual(report[key]["reason"], "pending_calibration")

        tampered = copy.deepcopy(report)
        tampered["benchmark_power_decision"]["passed"] = True
        reseal_report(tampered)
        with self.assertRaisesRegex(V4.PowerV4Error, "pass drift"):
            V4.validate_power_report(tampered)

    def test_only_frozen_owner_approved_development_evidence_can_pass(self) -> None:
        candidate = V4.analyze_calibration(
            calibration_fixture(
                state="candidate", evidence_class="development_measurement"
            )
        )
        self.assertTrue(candidate["benchmark_power_decision"]["statistical_gate_met"])
        self.assertFalse(candidate["benchmark_power_decision"]["eligible_evidence"])
        self.assertFalse(candidate["benchmark_power_decision"]["passed"])

        frozen = V4.analyze_calibration(
            calibration_fixture(
                state="frozen", evidence_class="development_measurement"
            )
        )
        for key in ("benchmark_power_decision", "product_improvement_gate"):
            self.assertEqual(frozen[key]["selected_task_clusters"], 12)
            self.assertTrue(frozen[key]["statistical_gate_met"])
            self.assertTrue(frozen[key]["eligible_evidence"])
            self.assertTrue(frozen[key]["passed"])
            self.assertEqual(frozen[key]["reason"], "passed")

    def test_synthetic_fixture_can_clear_statistics_but_never_pass(self) -> None:
        report = V4.analyze_calibration(calibration_fixture())
        for key in ("benchmark_power_decision", "product_improvement_gate"):
            self.assertTrue(report[key]["statistical_gate_met"])
            self.assertFalse(report[key]["eligible_evidence"])
            self.assertFalse(report[key]["passed"])
            self.assertEqual(
                report[key]["reason"], "synthetic_fixture_not_decision_eligible"
            )

    def test_estimands_average_repetitions_then_use_frozen_task_estimands(self) -> None:
        report = V4.analyze_calibration(
            calibration_fixture(homogeneous=False)
        )
        primary = report["calibration_estimates"]["primary"]
        self.assertEqual(
            primary["repetition_aggregation"],
            "arithmetic_mean_within_task_arm_before_contrast",
        )
        self.assertAlmostEqual(
            primary["elapsed_time"]["paired_task_geometric_mean_ratio"],
            math.sqrt(0.4 * 0.8),
            places=11,
        )
        expected_ratio_of_means = 82.0 / 110.0
        geometric_mean_task_ratios = math.sqrt(0.2 * 0.8)
        self.assertAlmostEqual(
            primary["normalized_cost"]["equal_task_weighted_mean_ratio"],
            expected_ratio_of_means,
            places=11,
        )
        self.assertNotAlmostEqual(
            primary["normalized_cost"]["equal_task_weighted_mean_ratio"],
            geometric_mean_task_ratios,
            places=3,
        )
        self.assertFalse(
            primary["normalized_cost"]["geometric_mean_task_ratios_used"]
        )
        self.assertTrue(
            primary["normalized_cost"]["structural_zero_treatment_cost_supported"]
        )
        self.assertAlmostEqual(
            primary["code_quality"]["paired_task_mean_difference"], 0.30, places=11
        )

    def test_authenticated_structural_zero_treatment_costs_are_supported(self) -> None:
        report = V4.analyze_calibration(
            calibration_fixture(structural_zero=True)
        )
        for key in ("primary", "product_diagnostic"):
            cost = report["calibration_estimates"][key]["normalized_cost"]
            self.assertEqual(cost["equal_task_weighted_mean_ratio"], 0.0)
            self.assertTrue(cost["structural_zero_treatment_cost_supported"])
            self.assertFalse(cost["geometric_mean_task_ratios_used"])
            self.assertTrue(math.isfinite(cost["one_sided_upper_bound"]))

    def test_shared_resampling_is_deterministic_and_preserves_joint_draws(self) -> None:
        first_draws, first_sha = V4.shared_cluster_resamples(
            source_clusters=12, target_clusters=24, resamples=100, seed=41
        )
        second_draws, second_sha = V4.shared_cluster_resamples(
            source_clusters=12, target_clusters=24, resamples=100, seed=41
        )
        changed_draws, changed_sha = V4.shared_cluster_resamples(
            source_clusters=12, target_clusters=24, resamples=100, seed=42
        )
        self.assertEqual(first_draws, second_draws)
        self.assertEqual(first_sha, second_sha)
        self.assertNotEqual(first_draws, changed_draws)
        self.assertNotEqual(first_sha, changed_sha)
        self.assertTrue(all(len(draw) == 24 for draw in first_draws))

        fixture = calibration_fixture()
        first = V4.analyze_calibration(fixture)
        second = V4.analyze_calibration(copy.deepcopy(fixture))
        self.assertEqual(
            PRODUCT.canonical_json_bytes(first), PRODUCT.canonical_json_bytes(second)
        )
        for row in first["power_candidates"]:
            self.assertEqual(
                row["primary"]["shared_resampling_sha256"],
                row["product_diagnostic"]["shared_resampling_sha256"],
            )
            self.assertEqual(
                row["shared_resampling_sha256"],
                row["primary"]["shared_resampling_sha256"],
            )
            for result_key in ("primary", "product_diagnostic"):
                result = row[result_key]
                self.assertLessEqual(
                    result["joint_all_endpoint_power"],
                    min(result["marginal_power"].values()),
                )
                self.assertFalse(result["independence_shortcut_used"])

    def test_primary_and_product_diagnostic_are_separate_and_non_altering(self) -> None:
        report = V4.analyze_calibration(calibration_fixture())
        primary = report["benchmark_power_decision"]
        diagnostic = report["product_improvement_gate"]
        self.assertEqual(primary["comparison"], V4.PRIMARY_COMPARISON)
        self.assertEqual(primary["role"], "benchmark_primary_power_gate")
        self.assertEqual(diagnostic["comparison"], V4.DIAGNOSTIC_COMPARISON)
        self.assertEqual(diagnostic["role"], "separate_product_improvement_gate")
        self.assertFalse(diagnostic["alters_benchmark_primary_verdict"])
        self.assertNotIn("alters_benchmark_primary_verdict", primary)

        tampered = copy.deepcopy(report)
        tampered["product_improvement_gate"][
            "alters_benchmark_primary_verdict"
        ] = True
        reseal_report(tampered)
        with self.assertRaisesRegex(V4.PowerV4Error, "may not alter"):
            V4.validate_power_report(tampered)

    def test_all_marginal_and_direct_joint_power_must_clear_point_eight(self) -> None:
        report = V4.analyze_calibration(
            calibration_fixture(
                state="frozen", evidence_class="development_measurement"
            )
        )
        tampered = copy.deepcopy(report)
        result = tampered["power_candidates"][0]["primary"]
        result["marginal_power"]["elapsed_time"] = 0.79
        reseal_report(tampered)
        with self.assertRaisesRegex(V4.PowerV4Error, "marginal gate drift"):
            V4.validate_power_report(tampered)

        tampered = copy.deepcopy(report)
        result = tampered["power_candidates"][0]["primary"]
        result["joint_all_endpoint_power"] = 0.79
        reseal_report(tampered)
        with self.assertRaisesRegex(V4.PowerV4Error, "joint gate drift"):
            V4.validate_power_report(tampered)

    def test_fewer_than_twelve_or_non_calibration_members_fail_closed(self) -> None:
        too_small = calibration_fixture()
        too_small["task_population_binding"]["calibration_members"].pop()
        reseal_population(too_small)
        with self.assertRaisesRegex(V4.PowerV4Error, "at least 12"):
            V4.preflight_calibration(too_small)

        for membership in ("development_optimization", "confirmatory_holdout"):
            with self.subTest(membership=membership):
                invalid = calibration_fixture()
                invalid["task_population_binding"]["calibration_members"][0][
                    "membership"
                ] = membership
                reseal_population(invalid)
                with self.assertRaisesRegex(
                    V4.PowerV4Error, "optimization or holdout members"
                ):
                    V4.preflight_calibration(invalid)

    def test_duplicate_task_overlap_commitment_fails_closed(self) -> None:
        calibration = calibration_fixture()
        members = calibration["task_population_binding"]["calibration_members"]
        members[1]["task_overlap_commitment_sha256"] = members[0][
            "task_overlap_commitment_sha256"
        ]
        reseal_population(calibration)
        with self.assertRaisesRegex(V4.PowerV4Error, "overlap commitment is duplicated"):
            V4.preflight_calibration(calibration)

    def test_candidate_lock_and_source_byte_tampering_fail_closed(self) -> None:
        candidate_lock = calibration_fixture()
        candidate_lock["candidate_lock"]["candidate_product_identity_sha256"] = digest(
            "other-candidate"
        )
        reseal_nested(candidate_lock, "candidate_lock")
        with self.assertRaisesRegex(V4.PowerV4Error, "candidate lock product identity drift"):
            V4.preflight_calibration(candidate_lock)

        plan_lock = calibration_fixture()
        plan_lock["candidate_lock"]["precalibration_plan_sha256"] = digest(
            "post-outcome-plan"
        )
        reseal_nested(plan_lock, "candidate_lock")
        with self.assertRaisesRegex(V4.PowerV4Error, "pre-calibration plan drift"):
            V4.preflight_calibration(plan_lock)

        population_lock = calibration_fixture()
        population_lock["candidate_lock"][
            "task_population_contract_sha256"
        ] = digest("other-task-population")
        reseal_nested(population_lock, "candidate_lock")
        with self.assertRaisesRegex(
            V4.PowerV4Error, "candidate lock task-population contract drift"
        ):
            V4.preflight_calibration(population_lock)

        source = calibration_fixture()
        source["source_byte_bindings"]["task_population_schema_file_sha256"] = digest(
            "other-task-population-schema"
        )
        reseal_nested(source, "source_byte_bindings")
        with self.assertRaisesRegex(V4.PowerV4Error, "source byte binding drift"):
            V4.preflight_calibration(source)

    def test_execution_and_pricing_identity_tampering_fail_closed(self) -> None:
        fields = {
            "runner_sha256": digest("other-runner"),
            "model_id": "other-model",
            "effort": "other-effort",
            "engine_sha256": digest("other-engine"),
            "prompt_template_sha256": digest("other-prompt"),
            "prompt_parity_algorithm": "other-prompt-parity",
            "cache_policy_sha256": digest("other-cache"),
            "schedule_sha256": digest("other-schedule"),
            "price_quote_sha256": digest("other-price-quote"),
            "pricing_policy_sha256": digest("other-pricing-policy"),
            "candidate_packet_format_sha256": digest("other-packet-format"),
        }
        for field, replacement in fields.items():
            with self.subTest(field=field):
                calibration = calibration_fixture()
                calibration["locked_identities"][field] = replacement
                reseal_nested(calibration, "locked_identities")
                with self.assertRaisesRegex(V4.PowerV4Error, "locked identity drift"):
                    V4.preflight_calibration(calibration)

    def test_four_arm_grid_cell_and_invocation_arithmetic_fail_closed(self) -> None:
        mutations = (
            ("arms", lambda c: c["planning"].__setitem__("arms", list(PRODUCT.ARMS[:3]))),
            (
                "requested_cells",
                lambda c: c["product_cycle_evidence"]["design"].__setitem__(
                    "requested_cells", 95
                ),
            ),
            (
                "maximum_agent_invocations",
                lambda c: c["product_cycle_evidence"]["design"].__setitem__(
                    "maximum_agent_invocations", 95
                ),
            ),
            ("missing_cell", lambda c: c["product_cycle_evidence"]["cells"].pop()),
        )
        for label, mutate in mutations:
            with self.subTest(label=label):
                calibration = calibration_fixture()
                mutate(calibration)
                with self.assertRaises((V4.PowerV4Error, PRODUCT.ProductCycleError)):
                    V4.preflight_calibration(calibration)

    def test_no_budget_provider_private_or_holdout_evidence_is_used(self) -> None:
        calibration = calibration_fixture()
        receipt = V4.preflight_calibration(calibration)
        report = V4.analyze_calibration(calibration)
        self.assertFalse(receipt["budget_authorized"])
        self.assertEqual(
            calibration["product_cycle_evidence"]["design"]["budget"],
            {"status": "not_frozen", "authorized_usd": None},
        )
        self.assertEqual(
            {
                member["membership"]
                for member in calibration["task_population_binding"][
                    "calibration_members"
                ]
            },
            {"development_calibration"},
        )
        self.assertEqual(report["integrity"]["optimization_members_used"], 0)
        self.assertEqual(report["integrity"]["holdout_members_used"], 0)
        self.assertFalse(report["integrity"]["budget_authorized"])
        self.assertFalse(report["integrity"]["provider_or_model_calls_performed"])

    def test_strict_loader_rejects_duplicate_keys(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            path = pathlib.Path(temporary) / "duplicate.json"
            path.write_text('{"schema":"a","schema":"b"}', encoding="utf-8")
            with self.assertRaisesRegex(V4.PowerV4Error, "duplicate object key"):
                V4.load_calibration(path)
            with self.assertRaisesRegex(V4.PowerV4Error, "duplicate object key"):
                V4.load_power_report(path)

    def test_report_check_recomputes_every_value_from_calibration(self) -> None:
        calibration = calibration_fixture()
        report = V4.analyze_calibration(calibration)
        receipt = V4.check_power_report(calibration, report)
        self.assertEqual(receipt["status"], "valid")
        self.assertEqual(receipt["report_sha256"], report["report_sha256"])
        self.assertFalse(receipt["budget_authorized"])
        self.assertFalse(receipt["provider_or_model_calls_performed"])

        tampered = copy.deepcopy(report)
        tampered["power_candidates"][0]["primary"]["marginal_power"][
            "elapsed_time"
        ] = 0.99
        reseal_report(tampered)
        V4.validate_power_report(tampered)
        with self.assertRaisesRegex(V4.PowerV4Error, "deterministic recomputation"):
            V4.check_power_report(calibration, tampered)

        integrity_drift = copy.deepcopy(report)
        integrity_drift["integrity"]["optimization_members_used"] = 1
        reseal_report(integrity_drift)
        with self.assertRaisesRegex(V4.PowerV4Error, "optimization evidence"):
            V4.validate_power_report(integrity_drift)

    def test_checked_in_schemas_name_strict_calibration_and_report_contracts(self) -> None:
        calibration_schema = json.loads(
            (HERE / "schemas" / "final-calibration-v1.schema.json").read_text(
                encoding="utf-8"
            )
        )
        report_schema = json.loads(
            (HERE / "schemas" / "power-analysis-v4.schema.json").read_text(
                encoding="utf-8"
            )
        )
        self.assertEqual(
            calibration_schema["properties"]["schema"]["const"],
            V4.CALIBRATION_SCHEMA,
        )
        self.assertFalse(calibration_schema["additionalProperties"])
        self.assertEqual(
            calibration_schema["$defs"]["taskPopulationBinding"]["properties"][
                "calibration_members"
            ]["minItems"],
            12,
        )
        self.assertEqual(
            calibration_schema["$defs"]["planning"]["properties"]["arms"]["const"],
            list(PRODUCT.ARMS),
        )
        self.assertEqual(
            report_schema["properties"]["schema"]["const"], V4.REPORT_SCHEMA
        )
        self.assertFalse(report_schema["additionalProperties"])


if __name__ == "__main__":
    unittest.main()
