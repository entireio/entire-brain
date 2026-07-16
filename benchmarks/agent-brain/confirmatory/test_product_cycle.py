from __future__ import annotations

import copy
import importlib.util
import json
import pathlib
import tempfile
import unittest


HERE = pathlib.Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location("product_cycle", HERE / "product_cycle.py")
assert SPEC and SPEC.loader
PRODUCT = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(PRODUCT)


def digest(label: str) -> str:
    return PRODUCT.value_sha256({"fixture": label})


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


def outcome(*, elapsed: float, cost: str, quality: float) -> dict:
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
                "uncached_input": cost,
                "cache_read_input": "0",
                "cache_write_input": "0",
                "visible_output": "0",
                "reasoning_output": "0",
            },
            "total_usd": cost,
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


def finalize(manifest: dict) -> dict:
    """Rehash a mutated fixture without repairing its semantic fields."""
    for role in ("baseline", "candidate"):
        identity = manifest["product_identities"][role]
        identity["identity_sha256"] = PRODUCT.self_sha256(identity)
    manifest["product_identities"]["binding_sha256"] = PRODUCT.value_sha256(
        {
            "baseline": manifest["product_identities"]["baseline"]["identity_sha256"],
            "candidate": manifest["product_identities"]["candidate"]["identity_sha256"],
        }
    )
    manifest["task_inventory_sha256"] = PRODUCT.value_sha256(manifest["tasks"])
    manifest["design"]["schedule"]["identity_sha256"] = PRODUCT.self_sha256(
        manifest["design"]["schedule"]
    )
    manifest["shared_execution"]["identity_sha256"] = PRODUCT.self_sha256(
        manifest["shared_execution"]
    )
    for cell in manifest["cells"]:
        cell["execution_identity"]["identity_sha256"] = PRODUCT.self_sha256(
            cell["execution_identity"]
        )
        cell["outcome"]["identity_sha256"] = PRODUCT.self_sha256(cell["outcome"])
        cell["identity_sha256"] = PRODUCT.self_sha256(cell)
    manifest["manifest_sha256"] = PRODUCT.self_sha256(manifest, "manifest_sha256")
    return manifest


def fixture_manifest() -> dict:
    tasks = [
        {
            "task_id": "task-a",
            "task_sha256": digest("task-a"),
            "prompt_parity_sha256": digest("task-a-prompt-parity"),
        },
        {
            "task_id": "task-b",
            "task_sha256": digest("task-b"),
            "prompt_parity_sha256": digest("task-b-prompt-parity"),
        },
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
        "model_id": "synthetic-model",
        "effort": "synthetic-medium",
        "schedule_sha256": schedule["identity_sha256"],
    }
    shared["identity_sha256"] = PRODUCT.self_sha256(shared)

    metric_grid = {
        "task-a": {
            "no_memory": ((10, "4", 0.5), (14, "6", 0.7)),
            "placebo_packet": ((11, "5", 0.55), (13, "5", 0.65)),
            "preoptimization_memory": ((8, "2", 0.6), (12, "4", 0.7)),
            "retrieved_memory": ((6, "2", 0.8), (10, "2", 0.8)),
        },
        "task-b": {
            "no_memory": ((18, "8", 0.4), (22, "12", 0.6)),
            "placebo_packet": ((19, "9", 0.45), (21, "11", 0.55)),
            "preoptimization_memory": ((14, "5", 0.65), (18, "7", 0.75)),
            "retrieved_memory": ((8, "3", 0.7), (12, "5", 0.9)),
        },
    }
    task_by_id = {task["task_id"]: task for task in tasks}
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
        product_hash = product_hashes[role]
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
            "model_id": shared["model_id"],
            "effort": shared["effort"],
            "schedule_sha256": shared["schedule_sha256"],
            "product_identity_sha256": product_hash,
        }
        execution["identity_sha256"] = PRODUCT.self_sha256(execution)
        elapsed, cost, quality = metric_grid[task_id][arm][repetition - 1]
        cell = {
            "run_id": f"{task_id}-r{repetition}-{arm}",
            "schedule_position": scheduled["position"],
            "task_id": task_id,
            "repetition": repetition,
            "arm": arm,
            "product_identity_sha256": product_hash,
            "execution_identity": execution,
            "outcome": outcome(elapsed=elapsed, cost=cost, quality=quality),
        }
        cell["identity_sha256"] = PRODUCT.self_sha256(cell)
        cells.append(cell)

    manifest = {
        "schema": PRODUCT.MANIFEST_SCHEMA,
        "purpose": "development_product_cycle_only",
        "evidence_class": "synthetic_fixture",
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
            "task_clusters": 2,
            "repetitions_per_arm": 2,
            "requested_cells": 16,
            "agent_retry_limit": 0,
            "replacement_cell_limit": 0,
            "reserve_cell_limit": 0,
            "maximum_agent_invocations": 16,
            "budget": {"status": "not_frozen", "authorized_usd": None},
            "schedule": schedule,
        },
        "cells": cells,
    }
    manifest["manifest_sha256"] = PRODUCT.self_sha256(manifest, "manifest_sha256")
    return manifest


class ProductCycleV1Test(unittest.TestCase):
    def test_preflight_binds_exact_four_arm_arithmetic_without_budget(self) -> None:
        receipt = PRODUCT.preflight_manifest(fixture_manifest())
        self.assertEqual(receipt["status"], "valid")
        self.assertEqual(receipt["arms"], list(PRODUCT.ARMS))
        self.assertEqual(receipt["requested_cells"], 16)
        self.assertEqual(receipt["validated_cells"], 16)
        self.assertEqual(receipt["maximum_agent_invocations"], 16)
        self.assertEqual(receipt["budget_status"], "not_frozen")

    def test_analyzer_averages_repetitions_inside_task_for_all_endpoints(self) -> None:
        report = PRODUCT.analyze_manifest(fixture_manifest())
        primary = report["primary_result"]["endpoints"]
        self.assertAlmostEqual(
            primary["elapsed_time"]["paired_task_geometric_mean_ratio"],
            (8 / 12 * 10 / 20) ** 0.5,
            places=11,
        )
        self.assertAlmostEqual(
            primary["normalized_cost"]["equal_task_weighted_mean_ratio"], 0.4
        )
        self.assertAlmostEqual(primary["code_quality"]["paired_task_mean_difference"], 0.25)
        for endpoint in primary.values():
            self.assertEqual(
                endpoint["repetition_aggregation"],
                "arithmetic_mean_within_task_arm_before_contrast",
            )
            self.assertEqual(endpoint["cluster_unit"], "task")

    def test_diagnostic_baseline_contrast_cannot_become_primary_verdict(self) -> None:
        report = PRODUCT.analyze_manifest(fixture_manifest())
        self.assertEqual(report["primary_result"]["contrast"], "retrieved_memory_vs_no_memory")
        self.assertEqual(report["primary_result"]["verdict"], "descriptive_only_no_frozen_threshold")
        diagnostic = report["diagnostics"]["retrieved_memory_vs_preoptimization_memory"]
        self.assertFalse(diagnostic["eligible_for_primary_verdict"])
        self.assertEqual(set(diagnostic["endpoints"]), {"elapsed_time", "normalized_cost", "code_quality"})

    def test_analysis_is_byte_deterministic_for_identical_input(self) -> None:
        manifest = fixture_manifest()
        first = PRODUCT.canonical_json_bytes(PRODUCT.analyze_manifest(manifest))
        second = PRODUCT.canonical_json_bytes(PRODUCT.analyze_manifest(copy.deepcopy(manifest)))
        self.assertEqual(first, second)

    def test_missing_or_tampered_product_identity_fails_closed(self) -> None:
        missing = fixture_manifest()
        del missing["product_identities"]["baseline"]["binary_sha256"]
        with self.assertRaisesRegex(PRODUCT.ProductCycleError, "fields mismatch"):
            PRODUCT.preflight_manifest(missing)

        tampered = fixture_manifest()
        tampered["product_identities"]["candidate"]["commit_oid"] = "f" * 40
        with self.assertRaisesRegex(PRODUCT.ProductCycleError, "identity hash mismatch"):
            PRODUCT.preflight_manifest(tampered)

    def test_cell_product_identity_and_prompt_parity_fail_even_when_rehashed(self) -> None:
        product_attack = fixture_manifest()
        target = product_attack["cells"][0]
        target["product_identity_sha256"] = product_attack["product_identities"]["baseline"]["identity_sha256"]
        finalize(product_attack)
        with self.assertRaisesRegex(PRODUCT.ProductCycleError, "product identity does not match arm mapping"):
            PRODUCT.preflight_manifest(product_attack)

        prompt_attack = fixture_manifest()
        prompt_attack["cells"][0]["execution_identity"]["prompt_parity_sha256"] = digest(
            "different-prompt-parity"
        )
        finalize(prompt_attack)
        with self.assertRaisesRegex(
            PRODUCT.ProductCycleError, "prompt_parity_sha256 parity mismatch"
        ):
            PRODUCT.preflight_manifest(prompt_attack)

    def test_every_shared_execution_dimension_is_enforced_per_cell(self) -> None:
        for field, value in (
            ("task_sha256", digest("other-task")),
            ("corpus_sha256", digest("other-corpus")),
            ("engine_sha256", digest("other-engine")),
            ("prompt_template_sha256", digest("other-template")),
            ("prompt_parity_algorithm", "other-parity-algorithm"),
            ("cache_policy_sha256", digest("other-cache")),
            ("runner_sha256", digest("other-runner")),
            ("model_id", "other-model"),
            ("effort", "other-effort"),
            ("schedule_sha256", digest("other-schedule")),
        ):
            with self.subTest(field=field):
                manifest = fixture_manifest()
                manifest["cells"][0]["execution_identity"][field] = value
                finalize(manifest)
                with self.assertRaisesRegex(PRODUCT.ProductCycleError, f"{field} parity mismatch"):
                    PRODUCT.preflight_manifest(manifest)

    def test_duplicate_missing_and_schedule_mismatched_cells_fail_closed(self) -> None:
        missing = fixture_manifest()
        missing["cells"].pop()
        finalize(missing)
        with self.assertRaisesRegex(PRODUCT.ProductCycleError, "exactly 16"):
            PRODUCT.preflight_manifest(missing)

        duplicate = fixture_manifest()
        duplicate["cells"][1]["schedule_position"] = 1
        finalize(duplicate)
        with self.assertRaisesRegex(
            PRODUCT.ProductCycleError,
            "does not match its scheduled cell|duplicate measurement",
        ):
            PRODUCT.preflight_manifest(duplicate)

        mismatch = fixture_manifest()
        mismatch["cells"][0]["arm"] = "retrieved_memory"
        finalize(mismatch)
        with self.assertRaisesRegex(PRODUCT.ProductCycleError, "does not match its scheduled cell"):
            PRODUCT.preflight_manifest(mismatch)

    def test_incomplete_cost_categories_and_arithmetic_drift_fail_closed(self) -> None:
        incomplete = fixture_manifest()
        del incomplete["cells"][0]["outcome"]["normalized_cost"]["categories"]["reasoning_output"]
        finalize(incomplete)
        with self.assertRaisesRegex(PRODUCT.ProductCycleError, "fields mismatch"):
            PRODUCT.preflight_manifest(incomplete)

        stale_total = fixture_manifest()
        stale_total["cells"][0]["outcome"]["normalized_cost"]["total_usd"] = "999"
        finalize(stale_total)
        with self.assertRaisesRegex(PRODUCT.ProductCycleError, "total/category arithmetic drift"):
            PRODUCT.preflight_manifest(stale_total)

    def test_cell_and_invocation_arithmetic_drift_fail_closed(self) -> None:
        for field in ("requested_cells", "maximum_agent_invocations"):
            with self.subTest(field=field):
                manifest = fixture_manifest()
                manifest["design"][field] = 17
                finalize(manifest)
                with self.assertRaisesRegex(PRODUCT.ProductCycleError, "arithmetic drift"):
                    PRODUCT.preflight_manifest(manifest)

        retry = fixture_manifest()
        retry["design"]["agent_retry_limit"] = 1
        finalize(retry)
        with self.assertRaisesRegex(PRODUCT.ProductCycleError, "must remain zero"):
            PRODUCT.preflight_manifest(retry)

    def test_failure_validation_and_quality_v2_are_consistent_and_retained(self) -> None:
        manifest = fixture_manifest()
        target = manifest["cells"][0]["outcome"]
        target["failure"] = {
            "occurred": True,
            "kind": "agent_execution",
            "return_code": 1,
            "reason_codes": ["nonzero_return_code"],
        }
        target["validation"] = {"ok": False, "reason_codes": ["tests_failed"]}
        target["code_quality"].update(
            task_normalized_score=0,
            critical_failure=True,
            critical_failure_reasons=["tests_failed"],
        )
        finalize(manifest)
        report = PRODUCT.analyze_manifest(manifest)
        self.assertEqual(report["arm_summary"]["no_memory"]["failures"], 1)
        self.assertEqual(report["arm_summary"]["no_memory"]["validation_passes"], 3)

        inconsistent = fixture_manifest()
        inconsistent["cells"][0]["outcome"]["validation"] = {
            "ok": False,
            "reason_codes": ["tests_failed"],
        }
        finalize(inconsistent)
        with self.assertRaisesRegex(PRODUCT.ProductCycleError, "must force critical zero quality"):
            PRODUCT.preflight_manifest(inconsistent)

        unrecorded_timeout = fixture_manifest()
        timed_out = unrecorded_timeout["cells"][0]["outcome"]
        timed_out["timing"].update(
            timeout_occurred=True,
            timeout_stage="agent_execution",
            timeout_component_limit_seconds=900,
        )
        timed_out["code_quality"].update(
            task_normalized_score=0,
            critical_failure=True,
            critical_failure_reasons=["agent_timeout"],
        )
        finalize(unrecorded_timeout)
        with self.assertRaisesRegex(PRODUCT.ProductCycleError, "timeout must be recorded"):
            PRODUCT.preflight_manifest(unrecorded_timeout)

    def test_primary_and_diagnostic_contrast_roles_are_immutable(self) -> None:
        for key in ("primary_contrast", "diagnostic_contrast"):
            with self.subTest(key=key):
                manifest = fixture_manifest()
                manifest["design"][key]["role"] = "primary_development_contrast"
                if key == "primary_contrast":
                    manifest["design"][key]["denominator"] = "preoptimization_memory"
                finalize(manifest)
                with self.assertRaisesRegex(PRODUCT.ProductCycleError, "contrast changed"):
                    PRODUCT.preflight_manifest(manifest)

    def test_strict_loader_rejects_duplicate_keys(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            path = pathlib.Path(temporary) / "duplicate.json"
            path.write_text('{"schema":"a","schema":"b"}', encoding="utf-8")
            with self.assertRaisesRegex(PRODUCT.ProductCycleError, "duplicate key"):
                PRODUCT.load_manifest(path)

    def test_checked_in_schema_names_the_exact_contract(self) -> None:
        schema = json.loads(
            (HERE / "schemas" / "product-cycle-v1.schema.json").read_text(encoding="utf-8")
        )
        self.assertEqual(schema["properties"]["schema"]["const"], PRODUCT.MANIFEST_SCHEMA)
        self.assertEqual(schema["$defs"]["arm"]["enum"], list(PRODUCT.ARMS))
        self.assertEqual(
            set(schema["$defs"]["costCategories"]["required"]),
            set(PRODUCT.PRICE_CATEGORIES),
        )

if __name__ == "__main__":
    unittest.main()
