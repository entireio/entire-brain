"""Shared deterministic fixtures for the power-v4 hardening tests."""

from __future__ import annotations

from decimal import Decimal
import pathlib
import sys
import unittest


HERE = pathlib.Path(__file__).resolve().parent
if str(HERE) not in sys.path:
    sys.path.insert(0, str(HERE))
import power_analysis_v4 as V4


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
    duplicate_task_hash: bool = False,
) -> dict:
    tasks = [
        {
            "task_id": f"task-{index:02d}",
            "task_sha256": digest(
                "task-00"
                if duplicate_task_hash and index == 1
                else f"task-{index:02d}"
            ),
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


class ProductCycleFixtureTest(unittest.TestCase):
    def test_shared_product_cycle_fixture_is_contract_valid(self) -> None:
        root, measurements, metadata = PRODUCT._validated_manifest(
            product_cycle_fixture()
        )
        self.assertEqual(root["design"]["task_clusters"], 12)
        self.assertEqual(len(measurements), 96)
        self.assertEqual(metadata["expected_cells"], 96)


if __name__ == "__main__":
    unittest.main()
