from __future__ import annotations

import copy
from datetime import datetime, timezone
import hashlib
import importlib.util
import inspect
import json
import math
import pathlib
import shutil
import tempfile
import unittest
from unittest import mock


HERE = pathlib.Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location("check_protocol", HERE / "check_protocol.py")
assert SPEC and SPEC.loader
CHECK = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(CHECK)

import restricted_replay_attestation as ATTEST


class ProtocolCheckTest(unittest.TestCase):
    @staticmethod
    def _write_json(path: pathlib.Path, value: object) -> None:
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n", encoding="utf-8")

    @staticmethod
    def _sha(path: pathlib.Path) -> str:
        return hashlib.sha256(path.read_bytes()).hexdigest()

    @staticmethod
    def _copy_engine_storage_lock_bundle(repo: pathlib.Path) -> pathlib.Path:
        relative_paths = {
            CHECK.ENGINE_EVIDENCE_STORAGE_REPO_PATH,
            CHECK.ENGINE_PINS_REPO_PATH,
            "benchmarks/agent-brain/confirmatory/engine-matrix.json",
            "benchmarks/agent-brain/confirmatory/analyzer-lock.json",
            "benchmarks/agent-brain/confirmatory/engine-replay-checker-lock.json",
            "benchmarks/agent-brain/confirmatory/engine-replay-trust-roots.json",
            "benchmarks/agent-brain/confirmatory/engine-evidence-storage-legacy-v1.json",
            *ATTEST.CHECKER_LOCK_PATHS,
        }
        for relative in relative_paths:
            source = CHECK.REPO / relative
            destination = repo / relative
            destination.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(source, destination)
        return repo / CHECK.ENGINE_EVIDENCE_STORAGE_REPO_PATH

    @staticmethod
    def _pending_power_protocol(artifact: dict[str, object]) -> dict[str, object]:
        success_contract = copy.deepcopy(
            json.loads(
                (HERE / "preregistration.json").read_text(encoding="utf-8")
            )["agent_design"]["success_contract"]
        )
        inputs = artifact["protocol_inputs"]
        assert isinstance(inputs, dict)
        return {
            "agent_design": {
                "primary_treatments": [
                    "no_memory",
                    "placebo_packet",
                    "retrieved_memory",
                ],
                "success_contract": success_contract,
                "tasks": inputs["tasks"],
                "repetitions_per_treatment": inputs["repetitions_per_treatment"],
                "requested_cells": inputs["requested_cells"],
                "maximum_agent_invocations": inputs["maximum_agent_invocations"],
                "power": {
                    "target": inputs["power_target"],
                    "target_scope": "each_marginal_and_overall_intersection_union_joint_success",
                    "completed": False,
                    "status": "pending_uncalibrated",
                    "evidence": "power-analysis.json",
                    "analysis_kind": artifact["analysis_kind"],
                    "co_primary_claim_floors": {
                        "elapsed_time_ratio_max": 0.9,
                        "normalized_cost_ratio_max": 0.88,
                        "code_quality_difference_min": 0.05,
                        "status": "provisional",
                    },
                    "planning_alternatives": {
                        "elapsed_time_ratio_true": None,
                        "normalized_cost_ratio_true": None,
                        "code_quality_difference_true": None,
                        "status": "pending_owner_approval",
                    },
                    "exploratory_calibration": {
                        "manifest": "power-calibration-exploratory-v1.json",
                        "eligibility": "exploratory_only",
                        "confirmatory_assumption_source": False,
                        "unique_task_ids": 12,
                        "paired_task_cluster_instances": 14,
                        "pooled_estimate_prohibited": True,
                    },
                    "minimum_calibration_task_clusters": 12,
                    "minimum_calibration_is_power_sized_design": False,
                    "power_sized_development_task_count": None,
                    "power_sized_confirmatory_task_count": None,
                    "design_decision_required": True,
                },
            }
        }

    def _complete_pricing_budget(
        self,
        repo: pathlib.Path,
        here: pathlib.Path,
    ) -> tuple[dict[str, object], dict[str, dict[str, str]], dict[str, object]]:
        quote_source = here / "pricing-evidence" / "synthetic-quote.txt"
        quote_source.parent.mkdir(parents=True, exist_ok=True)
        quote_source.write_text("synthetic test quote; not a provider price\n", encoding="utf-8")
        runner = {
            "schema": "agent-brain-frozen-runner-identity/v1",
            "provider": "synthetic-provider",
            "runner_id": "synthetic-runner",
            "runner_version": "test-v1",
            "agent_id": "synthetic-cli",
            "agent_cli": "synthetic-cli",
            "agent_cli_version": "synthetic-cli-v1",
            "requested_model_id": "synthetic-model",
            "resolved_model_id": "synthetic-model",
            "effort": "synthetic-effort",
            "schedule_sha256": "b" * 64,
        }
        runner["identity_sha256"] = CHECK.canonical_json_sha256(runner)
        protocol: dict[str, object] = {
            "protocol_id": "synthetic-protocol",
            "agent_design": {
                "primary_treatments": ["a", "b", "c"],
                "tasks": 2,
                "repetitions_per_treatment": 1,
                "requested_cells": 6,
                "maximum_agent_invocations": 6,
                "power": {"status": "pass", "design_decision_required": False},
            },
            "paid_budget": {
                "contract": "pricing-budget.json",
                "status": "approved",
                "currency": "USD",
                "formula_id": CHECK.pricing_budget.FORMULA_ID,
                "maximum_usd": None,
            },
        }
        contract: dict[str, object] = {
            "schema_version": 3,
            "runner": runner,
            "pricing_quote": {
                "schema": "agent-brain-price-quote/v2",
                "status": "pinned",
                "quote_sha256": None,
                "source_uri": "synthetic://unit-test-quote",
                "source_artifact_path": quote_source.relative_to(repo).as_posix(),
                "source_artifact_sha256": self._sha(quote_source),
                "as_of": "2026-07-14T10:00:00+00:00",
                "retrieved_at": "2026-07-14T10:01:00+00:00",
                "expires_at": "2026-07-20T10:00:00+00:00",
                "maximum_age_days": 7,
                "currency": "USD",
                "tokens_per_price_unit": 1_000_000,
                "prices_usd_per_unit": {
                    "uncached_input": "2",
                    "cache_read_input": "0.5",
                    "cache_write_input": "2",
                    "visible_output": "8",
                    "reasoning_output": "8",
                },
                "price_aliases": {
                    "uncached_input": None,
                    "cache_read_input": None,
                    "cache_write_input": None,
                    "visible_output": None,
                    "reasoning_output": None,
                },
                "usage_semantics": {
                    "input_tokens_includes": ["cache_read_input", "cache_write_input"],
                    "output_tokens_includes": ["reasoning_output"],
                    "counter_absence_means_zero": {
                        "cache_read_input": False,
                        "cache_write_input": False,
                        "reasoning_output": False,
                    },
                },
            },
            "design": {
                "protocol_id": "synthetic-protocol",
                "power_status_at_binding": "pass",
                "approved_for_budgeting": True,
                "tasks": 2,
                "treatments": ["a", "b", "c"],
                "repetitions_per_treatment": 1,
                "requested_cells": 6,
                "retry_agent_invocations": 0,
                "replacement_cell_attempts": 0,
                "reserve_cell_attempts": 0,
                "maximum_agent_invocations": 6,
            },
            "token_assumptions": {
                "mode": "explicit_per_agent_invocation_caps",
                "explicit_per_agent_invocation_caps": {
                    "uncached_input": 1000,
                    "cache_read_input": 500,
                    "cache_write_input": 200,
                    "visible_output": 100,
                    "reasoning_output": 50,
                    "rationale": "synthetic unit-test caps",
                },
                "empirical_bound": {
                    "runner": None,
                    "evidence_path": None,
                    "evidence_sha256": None,
                    "statistic": None,
                    "quantile": None,
                    "safety_multiplier": None,
                    "observed_tokens_per_agent_invocation": {
                        "uncached_input": None,
                        "cache_read_input": None,
                        "cache_write_input": None,
                        "visible_output": None,
                        "reasoning_output": None,
                    },
                },
            },
            "calculation": None,
            "approval": {
                "status": "pending",
                "approved_cap_usd": None,
                "approved_by": None,
                "approver_role": None,
                "approved_at": None,
                "expires_at": None,
                "notes": None,
            },
        }
        quote_for_hash = copy.deepcopy(contract["pricing_quote"])
        quote_for_hash.pop("quote_sha256")
        contract["pricing_quote"]["quote_sha256"] = CHECK.canonical_json_sha256(quote_for_hash)  # type: ignore[index]
        calculation = CHECK.pricing_budget.calculate(contract)
        self.assertEqual(
            calculation["per_agent_invocation_maximum_usd"], "0.00385"
        )
        self.assertEqual(calculation["maximum_usd"], "0.0231")
        contract["calculation"] = calculation
        contract["approval"] = {
            "status": "approved",
            "approved_cap_usd": calculation["maximum_usd"],
            "approved_by": "synthetic-approver",
            "approver_role": "unit-test-owner",
            "approved_at": "2026-07-14T11:00:00+00:00",
            "expires_at": "2026-07-19T10:00:00+00:00",
            "notes": "synthetic test approval",
        }
        protocol["paid_budget"]["maximum_usd"] = calculation["maximum_usd"]  # type: ignore[index]
        checks = {
            "model_runner_price_pinned": {"status": "pass", "evidence": "pricing-budget.json"},
            "paid_budget_cap_approved": {"status": "pass", "evidence": "pricing-budget.json"},
        }
        self._write_json(here / "pricing-budget.json", contract)
        return protocol, checks, contract

    @staticmethod
    def _copy_relevance_bundle(target: pathlib.Path) -> None:
        for name in (
            "offline-relevance-development-labels.json",
            "offline-relevance-fact-snapshot.json",
            "offline-relevance-dataset.json",
            "offline-relevance-review-ledger.json",
            "offline-relevance-source-membership.json",
            "relevance-source-contract.json",
            "offline-relevance-null-review-ledger.json",
            "relevance-null-review-contract.json",
            "task-inventory.json",
        ):
            shutil.copy2(HERE / name, target / name)
        shutil.copytree(HERE / "schemas", target / "schemas")

    @staticmethod
    def _materialize_relevance_bundle(
        bundle: pathlib.Path,
        labels_path: pathlib.Path,
        snapshot_path: pathlib.Path,
    ) -> dict[str, object]:
        null_ledger_path = bundle / "offline-relevance-null-review-ledger.json"
        return CHECK.relevance_dataset.materialize(
            CHECK.REPO,
            labels_path,
            bundle / "task-inventory.json",
            snapshot_path,
            source_membership=json.loads(
                (bundle / "offline-relevance-source-membership.json").read_text(encoding="utf-8")
            ),
            null_review_ledger=json.loads(null_ledger_path.read_text(encoding="utf-8")),
            null_review_ledger_path=(
                "benchmarks/agent-brain/confirmatory/offline-relevance-null-review-ledger.json"
            ),
            null_review_ledger_sha256=hashlib.sha256(null_ledger_path.read_bytes()).hexdigest(),
        )

    def test_preparation_artifacts_are_consistent(self) -> None:
        # The integration verification is an immutable historical receipt.  This
        # workstream intentionally changes nine of its recorded source files, so
        # the live validator must fail closed on exactly those stale bindings
        # rather than silently rewriting the receipt.
        self.assertEqual(
            CHECK.validate(freeze=False),
            [
                "source_artifacts[0]: content hash mismatch: benchmarks/agent-brain/run.py",
                "source_artifacts[1]: content hash mismatch: benchmarks/agent-brain/run_test.py",
                "source_artifacts[8]: content hash mismatch: internal/cli/facts_eligibility_test.go",
                "source_artifacts[9]: content hash mismatch: internal/cli/facts_read_cmd.go",
                "source_artifacts[11]: content hash mismatch: internal/cli/embed_rank.go",
                "source_artifacts[12]: content hash mismatch: internal/cli/embed_store.go",
                "source_artifacts[13]: content hash mismatch: internal/cli/embed_vec_cgo.go",
                "source_artifacts[15]: content hash mismatch: benchmarks/agent-brain/analysis/evidence.py",
                "source_artifacts[17]: content hash mismatch: benchmarks/agent-brain/analysis/confirmatory.py",
            ],
        )

    def test_freeze_is_fail_closed_after_dependencies_pass(self) -> None:
        errors = CHECK.validate(freeze=True)
        self.assertTrue(errors)
        self.assertNotIn("WS2-WS5 dependencies are pending", errors)
        self.assertIn("fresh holdout commitment is not frozen", errors)
        self.assertIn("paid-run checklist is not all pass", errors)
        self.assertNotIn("too few answerable product-derived development relevance tasks", errors)
        self.assertNotIn("too few corpus-closed product-derived development null queries", errors)

    def test_inventory_is_unique_and_contamination_is_explicit(self) -> None:
        inventory = json.loads((HERE / "task-inventory.json").read_text())
        tasks = inventory["tasks"]
        self.assertEqual(len(tasks), 46)
        self.assertEqual(len({task["task_id"] for task in tasks}), 46)
        by_short = {task["fix_commit"][:9]: task for task in tasks}
        self.assertEqual(by_short["4dd458656"]["state"], "optimization_used")
        self.assertFalse(by_short["4dd458656"]["confirmatory_eligible"])
        self.assertEqual(by_short["d9df8fcca"]["ledger"]["present"], True)
        self.assertEqual(inventory["summary"]["confirmatory_eligible"], 0)

    def test_engine_names_and_namespaces_are_exact(self) -> None:
        matrix = json.loads((HERE / "engine-matrix.json").read_text())
        arms = matrix["arms"]
        self.assertEqual([arm["id"] for arm in arms], CHECK.ARMS)
        self.assertEqual(len({arm["namespace"] for arm in arms}), 3)
        self.assertIn("--no-semantic", arms[0]["cli_flags"])
        self.assertEqual(arms[2]["environment"]["ENTIRE_BRAIN_EMBEDDER"], "ollama")

    def test_power_artifact_is_derived_and_uncalibrated_decision_stays_pending(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            here = pathlib.Path(temp)
            artifact = CHECK.power_analysis.build_report()
            self._write_json(here / "power-analysis.json", artifact)
            protocol = self._pending_power_protocol(artifact)
            check = {"status": "fail", "evidence": "power-analysis.json"}
            self.assertEqual(
                CHECK.validate_power_analysis(protocol, check, here=here, repo=here),
                [],
            )

            drifted_protocol = copy.deepcopy(protocol)
            drifted_protocol["agent_design"]["power"]["exploratory_calibration"][
                "unique_task_ids"
            ] += 1
            errors = CHECK.validate_power_analysis(
                drifted_protocol, check, here=here, repo=here
            )
            self.assertIn(
                "protocol exploratory calibration summary does not match power artifact",
                errors,
            )

            stale = copy.deepcopy(artifact)
            stale["protocol_inputs"]["tasks"] += 1
            self._write_json(here / "power-analysis.json", stale)
            errors = CHECK.validate_power_analysis(protocol, check, here=here, repo=here)
            self.assertIn(
                "power-analysis.json is stale or does not match power_analysis.build_report()",
                errors,
            )

            pending_with_count = CHECK.power_analysis.build_report()
            pending_with_count["design_readiness"][
                "power_sized_confirmatory_task_count"
            ] = 24
            self._write_json(here / "power-analysis.json", pending_with_count)
            with mock.patch.object(
                CHECK.power_analysis, "build_report", return_value=pending_with_count
            ):
                errors = CHECK.validate_power_analysis(
                    protocol, check, here=here, repo=here
                )
            self.assertIn(
                "power artifact: pending power design must retain null power-sized task counts",
                errors,
            )

            underpowered = CHECK.power_analysis.build_report()
            alternatives = {
                "elapsed_time": ("ratio_true", 0.85, 0.81),
                "normalized_cost": ("ratio_true", 0.80, 0.79),
                "code_quality": ("difference_true", 0.10, 0.81),
            }
            for name, (key, alternative, marginal_power) in alternatives.items():
                endpoint = underpowered["co_primary_endpoints"][name]
                endpoint["claim_floor"]["status"] = "frozen_approved"
                endpoint["planning_alternative"] = {
                    key: alternative,
                    "status": "frozen_approved",
                }
                endpoint["paired_task_sd"] = 0.20
                endpoint["marginal_power"] = marginal_power
                endpoint["status"] = "evaluated"
            underpowered["joint_iut_power"].update(
                {
                    "intersection_union_success_probability": 0.01,
                    "status": "evaluated",
                }
            )
            underpowered["design_readiness"].update(
                {
                    "power_sized_development_task_count": 24,
                    "power_sized_confirmatory_task_count": 24,
                }
            )
            # Even internally consistent supplied SD/power literals cannot
            # promote pending-only v3 into an evaluated artifact.
            underpowered["decision"]["passed"] = True
            underpowered["status"] = "pass"
            underpowered_protocol = copy.deepcopy(protocol)
            underpowered_protocol["agent_design"]["power"].update(
                {
                    "planning_alternatives": {
                        "elapsed_time_ratio_true": 0.85,
                        "normalized_cost_ratio_true": 0.80,
                        "code_quality_difference_true": 0.10,
                        "status": "frozen_approved",
                    },
                    "status": "fail_underpowered",
                    "power_sized_development_task_count": 24,
                    "power_sized_confirmatory_task_count": 24,
                }
            )
            self._write_json(here / "power-analysis.json", underpowered)
            with mock.patch.object(
                CHECK.power_analysis, "build_report", return_value=underpowered
            ):
                errors = CHECK.validate_power_analysis(
                    underpowered_protocol, check, here=here, repo=here
                )
            self.assertTrue(
                any("schema v3 is pending-only" in error for error in errors),
                errors,
            )
            self.assertIn(
                "power artifact status does not match recomputed decision",
                errors,
            )

            synchronized_tamper = CHECK.power_analysis.build_report()
            synchronized_tamper["protocol_inputs"]["power_target"] = 0.70
            for name, (key, alternative) in {
                "elapsed_time": ("ratio_true", 0.85),
                "normalized_cost": ("ratio_true", 0.80),
                "code_quality": ("difference_true", 0.10),
            }.items():
                endpoint = synchronized_tamper["co_primary_endpoints"][name]
                endpoint["claim_floor"]["status"] = "frozen_approved"
                endpoint["planning_alternative"] = {
                    key: alternative,
                    "status": "frozen_approved",
                }
                endpoint["paired_task_sd"] = 0.20
                endpoint["marginal_power"] = 0.75
                endpoint["status"] = "evaluated"
            synchronized_tamper["joint_iut_power"].update(
                {
                    "intersection_union_success_probability": 0.75,
                    "status": "evaluated",
                }
            )
            synchronized_tamper["design_readiness"].update(
                {
                    "power_sized_development_task_count": 12,
                    "power_sized_confirmatory_task_count": 2,
                    "provisional_design_power_defensible": True,
                }
            )
            synchronized_tamper["decision"]["passed"] = True
            synchronized_tamper["status"] = "pass"
            synchronized_protocol = copy.deepcopy(protocol)
            synchronized_protocol["agent_design"]["power"].update(
                {
                    "target": 0.70,
                    "completed": True,
                    "status": "pass",
                    "co_primary_claim_floors": {
                        "elapsed_time_ratio_max": 0.9,
                        "normalized_cost_ratio_max": 0.88,
                        "code_quality_difference_min": 0.05,
                        "status": "frozen_approved",
                    },
                    "planning_alternatives": {
                        "elapsed_time_ratio_true": 0.85,
                        "normalized_cost_ratio_true": 0.80,
                        "code_quality_difference_true": 0.10,
                        "status": "frozen_approved",
                    },
                    "power_sized_development_task_count": 12,
                    "power_sized_confirmatory_task_count": 2,
                    "design_decision_required": False,
                }
            )
            synchronized_check = {
                "status": "pass",
                "evidence": "power-analysis.json",
            }
            self._write_json(here / "power-analysis.json", synchronized_tamper)
            with mock.patch.object(
                CHECK.power_analysis,
                "build_report",
                return_value=synchronized_tamper,
            ):
                errors = CHECK.validate_power_analysis(
                    synchronized_protocol,
                    synchronized_check,
                    here=here,
                    repo=here,
                )
            self.assertIn(
                "power target must remain frozen at 0.80 in artifact and preregistration",
                errors,
            )
            self.assertIn(
                "power-sized confirmatory task count must equal the protocol task count",
                errors,
            )
            self.assertIn(
                "cannot recompute power decision: power target must remain frozen at 0.80",
                errors,
            )

            valid_powered = copy.deepcopy(synchronized_tamper)
            valid_powered["protocol_inputs"]["power_target"] = 0.80
            for endpoint in valid_powered["co_primary_endpoints"].values():
                endpoint["marginal_power"] = 0.81
            valid_powered["joint_iut_power"][
                "intersection_union_success_probability"
            ] = 0.81
            valid_powered["design_readiness"].update(
                {
                    "power_sized_development_task_count": 24,
                    "power_sized_confirmatory_task_count": 24,
                }
            )
            powered_protocol = copy.deepcopy(synchronized_protocol)
            powered_protocol["agent_design"]["power"].update(
                {
                    "target": 0.80,
                    "power_sized_development_task_count": 24,
                    "power_sized_confirmatory_task_count": 24,
                }
            )
            self._write_json(here / "power-analysis.json", valid_powered)
            with mock.patch.object(
                CHECK.power_analysis, "build_report", return_value=valid_powered
            ):
                errors = CHECK.validate_power_analysis(
                    powered_protocol,
                    synchronized_check,
                    here=here,
                    repo=here,
                )
            self.assertTrue(
                any("schema v3 is pending-only" in error for error in errors),
                errors,
            )
            adversarial = (
                (
                    "provisional_floor",
                    lambda value: value["co_primary_endpoints"]["elapsed_time"][
                        "claim_floor"
                    ].update(status="provisional"),
                    "claim floor must be owner-approved and frozen",
                ),
                (
                    "infinite_quality_alternative",
                    lambda value: value["co_primary_endpoints"]["code_quality"][
                        "planning_alternative"
                    ].update(difference_true=math.inf),
                    "frozen planning alternative must be finite",
                ),
                (
                    "out_of_domain_quality_alternative",
                    lambda value: value["co_primary_endpoints"]["code_quality"][
                        "planning_alternative"
                    ].update(difference_true=2.0),
                    "quality planning alternative must be within [-1,1]",
                ),
            )
            for label, mutate, message in adversarial:
                with self.subTest(label=label):
                    invalid = copy.deepcopy(valid_powered)
                    mutate(invalid)
                    self._write_json(here / "power-analysis.json", invalid)
                    with mock.patch.object(
                        CHECK.power_analysis, "build_report", return_value=invalid
                    ):
                        errors = CHECK.validate_power_analysis(
                            powered_protocol,
                            synchronized_check,
                            here=here,
                            repo=here,
                        )
                    self.assertTrue(
                        any(message in error for error in errors),
                        errors,
                    )

    def test_synchronized_power_contract_tampering_fails_closed(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            here = pathlib.Path(temp)
            check = {"status": "fail", "evidence": "power-analysis.json"}

            def resize_tasks(artifact: dict, protocol: dict) -> None:
                cells = 1 * 4 * 3
                artifact["protocol_inputs"].update(
                    tasks=1,
                    requested_cells=cells,
                    maximum_agent_invocations=cells,
                )
                artifact["design_readiness"].update(
                    provisional_tasks=1,
                    provisional_requested_cells=cells,
                    maximum_agent_invocations=cells,
                )
                protocol["agent_design"].update(
                    tasks=1,
                    requested_cells=cells,
                    maximum_agent_invocations=cells,
                )

            def resize_repetitions(artifact: dict, protocol: dict) -> None:
                cells = 24 * 2 * 3
                artifact["protocol_inputs"].update(
                    repetitions_per_treatment=2,
                    requested_cells=cells,
                    maximum_agent_invocations=cells,
                )
                artifact["design_readiness"].update(
                    provisional_repetitions_per_treatment=2,
                    provisional_requested_cells=cells,
                    maximum_agent_invocations=cells,
                )
                protocol["agent_design"].update(
                    repetitions_per_treatment=2,
                    requested_cells=cells,
                    maximum_agent_invocations=cells,
                )

            def resize_treatments(artifact: dict, protocol: dict) -> None:
                cells = 24 * 4 * 2
                artifact["protocol_inputs"].update(
                    primary_treatments=2,
                    requested_cells=cells,
                    maximum_agent_invocations=cells,
                )
                artifact["design_readiness"].update(
                    provisional_requested_cells=cells,
                    maximum_agent_invocations=cells,
                )
                protocol["agent_design"].update(
                    primary_treatments=["no_memory", "retrieved_memory"],
                    requested_cells=cells,
                    maximum_agent_invocations=cells,
                )

            def lower_calibration(artifact: dict, protocol: dict) -> None:
                artifact["calibration_requirements"][
                    "minimum_independent_task_clusters"
                ] = 1
                protocol["agent_design"]["power"][
                    "minimum_calibration_task_clusters"
                ] = 1

            def drift_floors(artifact: dict, protocol: dict) -> None:
                artifact["co_primary_endpoints"]["elapsed_time"]["claim_floor"][
                    "ratio_max"
                ] = 0.95
                artifact["co_primary_endpoints"]["normalized_cost"]["claim_floor"][
                    "ratio_max"
                ] = 0.95
                artifact["co_primary_endpoints"]["code_quality"]["claim_floor"][
                    "difference_min"
                ] = 0.01
                protocol["agent_design"]["power"]["co_primary_claim_floors"].update(
                    elapsed_time_ratio_max=0.95,
                    normalized_cost_ratio_max=0.95,
                    code_quality_difference_min=0.01,
                )

            def drift_analysis_kind(artifact: dict, protocol: dict) -> None:
                artifact["analysis_kind"] = "arbitrary_power"
                protocol["agent_design"]["power"]["analysis_kind"] = "arbitrary_power"

            mutations = (
                ("one_task", resize_tasks, "provisional 24"),
                ("two_repetitions", resize_repetitions, "frozen at 4"),
                ("two_treatments", resize_treatments, "treatment identities changed"),
                ("one_calibration_cluster", lower_calibration, "frozen 12-cluster"),
                (
                    "floor_drift",
                    drift_floors,
                    "do not match the authoritative success contract",
                ),
                (
                    "alpha",
                    lambda artifact, protocol: artifact["protocol_inputs"].update(
                        intersection_union_alpha=0.5
                    ),
                    "alpha/contrast/cluster/attempt identity changed",
                ),
                (
                    "contrast",
                    lambda artifact, protocol: artifact["protocol_inputs"].update(
                        primary_contrast="placebo_vs_no_memory"
                    ),
                    "alpha/contrast/cluster/attempt identity changed",
                ),
                (
                    "cluster",
                    lambda artifact, protocol: artifact["protocol_inputs"].update(
                        cluster_unit="cell"
                    ),
                    "alpha/contrast/cluster/attempt identity changed",
                ),
                (
                    "attempt_policy",
                    lambda artifact, protocol: artifact["protocol_inputs"].update(
                        attempt_policy="successful_attempts_only"
                    ),
                    "alpha/contrast/cluster/attempt identity changed",
                ),
                (
                    "artifact_id",
                    lambda artifact, protocol: artifact.update(
                        artifact_id="easier-power-v3"
                    ),
                    "power artifact_id changed",
                ),
                ("analysis_kind", drift_analysis_kind, "power analysis_kind changed"),
                (
                    "method",
                    lambda artifact, protocol: artifact["method"].update(
                        planning_rule="accept supplied .81"
                    ),
                    "power method contract changed",
                ),
                (
                    "joint_method",
                    lambda artifact, protocol: artifact["joint_iut_power"].update(
                        method="independent"
                    ),
                    "joint IUT power method changed",
                ),
                (
                    "estimand",
                    lambda artifact, protocol: artifact["co_primary_endpoints"][
                        "elapsed_time"
                    ].update(estimand_scale="cell_mean"),
                    "power elapsed_time estimand scale changed",
                ),
                (
                    "calibration_status",
                    lambda artifact, protocol: artifact[
                        "calibration_requirements"
                    ].update(status="complete"),
                    "must remain open",
                ),
                (
                    "empirical_variance",
                    lambda artifact, protocol: artifact.update(
                        empirical_variance_used_in_confirmatory_decision=True
                    ),
                    "cannot claim empirical variance",
                ),
            )

            for label, mutate, message in mutations:
                with self.subTest(label=label):
                    artifact = CHECK.power_analysis.build_report()
                    protocol = self._pending_power_protocol(artifact)
                    mutate(artifact, protocol)
                    self._write_json(here / "power-analysis.json", artifact)
                    with mock.patch.object(
                        CHECK.power_analysis, "build_report", return_value=artifact
                    ):
                        errors = CHECK.validate_power_analysis(
                            protocol, check, here=here, repo=here
                        )
                    self.assertTrue(
                        any(message in error for error in errors),
                        errors,
                    )
                    self.assertEqual(
                        CHECK.validate_joint_success_contract(protocol, freeze=False),
                        [],
                    )

    def test_pricing_budget_draft_is_valid_and_both_gates_remain_pending(self) -> None:
        protocol = json.loads((HERE / "preregistration.json").read_text(encoding="utf-8"))
        gate = json.loads((HERE / "go-no-go.json").read_text(encoding="utf-8"))
        checks = {item["id"]: item for item in gate["checks"]}
        self.assertEqual(CHECK.validate_pricing_budget(protocol, checks), [])
        contract = json.loads((HERE / "pricing-budget.json").read_text(encoding="utf-8"))
        self.assertEqual(contract["pricing_quote"]["status"], "pending")
        self.assertIsNone(contract["token_assumptions"]["mode"])
        self.assertIsNone(contract["calculation"])
        self.assertEqual(contract["approval"]["status"], "pending")
        self.assertEqual(checks["model_runner_price_pinned"]["status"], "pending")
        self.assertEqual(checks["paid_budget_cap_approved"]["status"], "pending")

        claimed = copy.deepcopy(checks)
        claimed["model_runner_price_pinned"] = {
            "status": "pass",
            "evidence": "pricing-budget.json",
        }
        errors = CHECK.validate_pricing_budget(protocol, claimed)
        self.assertIn(
            "model/runner/price gate cannot pass until the quote is pinned",
            errors,
        )

    def test_pricing_budget_pass_requires_fresh_quote_exact_calculation_and_approval(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            repo = pathlib.Path(temp)
            here = repo / "benchmarks" / "agent-brain" / "confirmatory"
            protocol, checks, contract = self._complete_pricing_budget(repo, here)
            now = datetime(2026, 7, 15, 12, tzinfo=timezone.utc)
            self.assertEqual(
                CHECK.validate_pricing_budget(protocol, checks, here=here, repo=repo, now=now),
                [],
            )

            protocol["agent_design"]["maximum_agent_invocations"] = 7  # type: ignore[index]
            contract["design"]["reserve_cell_attempts"] = 1  # type: ignore[index]
            contract["design"]["maximum_agent_invocations"] = 7  # type: ignore[index]
            self._write_json(here / "pricing-budget.json", contract)
            errors = CHECK.validate_pricing_budget(
                protocol, checks, here=here, repo=repo, now=now
            )
            self.assertIn(
                "confirmatory retries, replacements, and reserves must remain zero and maximum agent invocations must equal requested cells",
                errors,
            )
            protocol["agent_design"]["maximum_agent_invocations"] = 6  # type: ignore[index]
            contract["design"]["reserve_cell_attempts"] = 0  # type: ignore[index]
            contract["design"]["maximum_agent_invocations"] = 6  # type: ignore[index]

            contract["calculation"]["maximum_usd"] = "999"  # type: ignore[index]
            self._write_json(here / "pricing-budget.json", contract)
            errors = CHECK.validate_pricing_budget(protocol, checks, here=here, repo=repo, now=now)
            self.assertIn("pricing budget calculation is stale or incorrect", errors)

            contract["calculation"] = CHECK.pricing_budget.calculate(contract)
            contract["approval"]["status"] = "pending"  # type: ignore[index]
            self._write_json(here / "pricing-budget.json", contract)
            errors = CHECK.validate_pricing_budget(protocol, checks, here=here, repo=repo, now=now)
            self.assertIn("paid budget gate cannot pass without explicit approval", errors)

            _, _, contract = self._complete_pricing_budget(repo, here)
            stale_now = datetime(2026, 7, 30, 12, tzinfo=timezone.utc)
            errors = CHECK.validate_pricing_budget(protocol, checks, here=here, repo=repo, now=stale_now)
            self.assertIn("pricing quote has expired", errors)
            self.assertIn("pricing quote exceeds its maximum age", errors)

    def test_pricing_calculator_rejects_any_retry_replacement_or_reserve(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            repo = pathlib.Path(temp)
            here = repo / "benchmarks" / "agent-brain" / "confirmatory"
            _, _, contract = self._complete_pricing_budget(repo, here)
            for field in (
                "retry_agent_invocations",
                "replacement_cell_attempts",
                "reserve_cell_attempts",
            ):
                invalid = copy.deepcopy(contract)
                invalid["design"][field] = 1
                with self.subTest(field=field), self.assertRaisesRegex(
                    ValueError, f"design\\.{field} must be exactly zero"
                ):
                    CHECK.pricing_budget.calculate(invalid)
            invalid = copy.deepcopy(contract)
            invalid["design"]["maximum_agent_invocations"] = 7
            with self.assertRaisesRegex(
                ValueError, "maximum_agent_invocations must equal requested_cells"
            ):
                CHECK.pricing_budget.calculate(invalid)

    def test_pricing_v3_rejects_every_retired_per_call_field(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            repo = pathlib.Path(temp)
            here = repo / "benchmarks" / "agent-brain" / "confirmatory"
            protocol, checks, contract = self._complete_pricing_budget(repo, here)
            now = datetime(2026, 7, 15, 12, tzinfo=timezone.utc)

            def rename(container: dict, current: str, retired: str) -> None:
                container[retired] = container.pop(current)

            mutations = (
                (
                    "schema_v2",
                    lambda value: value.update(schema_version=2),
                    "pricing budget schema_version must be 3",
                ),
                (
                    "old_mode",
                    lambda value: value["token_assumptions"].update(
                        mode="explicit_per_call_caps"
                    ),
                    "token assumption mode is invalid",
                ),
                (
                    "old_explicit_caps_key",
                    lambda value: rename(
                        value["token_assumptions"],
                        "explicit_per_agent_invocation_caps",
                        "explicit_per_call_caps",
                    ),
                    "token_assumptions fields changed",
                ),
                (
                    "old_observed_tokens_key",
                    lambda value: rename(
                        value["token_assumptions"]["empirical_bound"],
                        "observed_tokens_per_agent_invocation",
                        "observed_tokens_per_call",
                    ),
                    "empirical_bound fields changed",
                ),
                (
                    "old_effective_tokens_calculation_key",
                    lambda value: rename(
                        value["calculation"],
                        "effective_tokens_per_agent_invocation",
                        "effective_tokens_per_call",
                    ),
                    "pricing budget calculation is stale or incorrect",
                ),
                (
                    "old_category_cost_calculation_key",
                    lambda value: rename(
                        value["calculation"],
                        "per_agent_invocation_usd_by_category",
                        "per_call_usd_by_category",
                    ),
                    "pricing budget calculation is stale or incorrect",
                ),
                (
                    "old_maximum_cost_calculation_key",
                    lambda value: rename(
                        value["calculation"],
                        "per_agent_invocation_maximum_usd",
                        "per_call_maximum_usd",
                    ),
                    "pricing budget calculation is stale or incorrect",
                ),
            )
            for label, mutate, expected in mutations:
                with self.subTest(label=label):
                    invalid = copy.deepcopy(contract)
                    mutate(invalid)
                    self._write_json(here / "pricing-budget.json", invalid)
                    errors = CHECK.validate_pricing_budget(
                        protocol, checks, here=here, repo=repo, now=now
                    )
                    self.assertTrue(
                        any(expected in error for error in errors),
                        errors,
                    )

    def test_empirical_token_bound_is_hashed_and_bound_to_the_pinned_runner(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            repo = pathlib.Path(temp)
            here = repo / "benchmarks" / "agent-brain" / "confirmatory"
            protocol, checks, contract = self._complete_pricing_budget(repo, here)
            evidence = here / "pricing-evidence" / "synthetic-token-sample.json"
            evidence.write_text('{"synthetic": true}\n', encoding="utf-8")
            contract["token_assumptions"] = {
                "mode": "empirical_bound",
                "explicit_per_agent_invocation_caps": {
                    "uncached_input": None,
                    "cache_read_input": None,
                    "cache_write_input": None,
                    "visible_output": None,
                    "reasoning_output": None,
                    "rationale": None,
                },
                "empirical_bound": {
                    "runner": copy.deepcopy(contract["runner"]),
                    "evidence_path": evidence.relative_to(repo).as_posix(),
                    "evidence_sha256": self._sha(evidence),
                    "statistic": "synthetic upper quantile",
                    "quantile": 0.95,
                    "safety_multiplier": "1.25",
                    "observed_tokens_per_agent_invocation": {
                        "uncached_input": 800,
                        "cache_read_input": 400,
                        "cache_write_input": 100,
                        "visible_output": 80,
                        "reasoning_output": 20,
                    },
                },
            }
            contract["calculation"] = CHECK.pricing_budget.calculate(contract)
            contract["approval"]["approved_cap_usd"] = contract["calculation"]["maximum_usd"]  # type: ignore[index]
            protocol["paid_budget"]["maximum_usd"] = contract["calculation"]["maximum_usd"]  # type: ignore[index]
            self._write_json(here / "pricing-budget.json", contract)
            now = datetime(2026, 7, 15, 12, tzinfo=timezone.utc)
            self.assertEqual(
                CHECK.validate_pricing_budget(protocol, checks, here=here, repo=repo, now=now),
                [],
            )

            contract["token_assumptions"]["empirical_bound"]["runner"]["requested_model_id"] = "different-model"  # type: ignore[index]
            self._write_json(here / "pricing-budget.json", contract)
            errors = CHECK.validate_pricing_budget(protocol, checks, here=here, repo=repo, now=now)
            self.assertIn("empirical token evidence runner does not match the pinned runner", errors)

            contract["token_assumptions"]["empirical_bound"]["runner"] = copy.deepcopy(contract["runner"])  # type: ignore[index]
            evidence.write_text('{"synthetic": false}\n', encoding="utf-8")
            self._write_json(here / "pricing-budget.json", contract)
            errors = CHECK.validate_pricing_budget(protocol, checks, here=here, repo=repo, now=now)
            self.assertTrue(any("empirical token evidence: content hash mismatch" in error for error in errors))

    def test_integration_dependency_evidence_hashes_source_and_logs(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            repo = pathlib.Path(temp)
            here = repo / "benchmarks" / "agent-brain" / "confirmatory"
            source = repo / "internal" / "example.go"
            log = here / "logs" / "verification.log"
            source.parent.mkdir(parents=True)
            log.parent.mkdir(parents=True)
            source.write_text("package example\n", encoding="utf-8")
            log.write_text("PASS\n", encoding="utf-8")
            source_path = "internal/example.go"
            log_path = "benchmarks/agent-brain/confirmatory/logs/verification.log"
            artifact = {
                "schema_version": 1,
                "base_commit": CHECK.BASE_COMMIT,
                "verified_commit": "a" * 40,
                "source_commits": dict(CHECK.SOURCE_COMMITS),
                "source_artifacts": [{"path": source_path, "sha256": self._sha(source)}],
                "test_runs": [
                    {
                        "id": "combined-tests",
                        "command": "python3 -m unittest",
                        "status": "pass",
                        "log_path": log_path,
                        "log_sha256": self._sha(log),
                    }
                ],
                "dependency_evidence": {
                    dependency: {
                        "source_artifact_paths": [source_path],
                        "test_run_ids": ["combined-tests"],
                    }
                    for dependency in CHECK.DEPENDENCY_CHECKS
                },
                "entire_graph_modified": False,
                "paid_runs_performed": False,
            }
            self._write_json(here / "integration-verification.json", artifact)
            protocol = {"dependencies": {dependency: "pass" for dependency in CHECK.DEPENDENCY_CHECKS}}
            checks = {
                check_id: {"status": "pass", "evidence": "integration-verification.json"}
                for check_id in CHECK.DEPENDENCY_CHECKS.values()
            }
            self.assertEqual(
                CHECK.validate_integration_verification(protocol, checks, here=here, repo=repo),
                [],
            )

            source.write_text("package tampered\n", encoding="utf-8")
            errors = CHECK.validate_integration_verification(protocol, checks, here=here, repo=repo)
            self.assertTrue(any("content hash mismatch" in error for error in errors))
            source.write_text("package example\n", encoding="utf-8")
            log.write_text("TAMPERED\n", encoding="utf-8")
            errors = CHECK.validate_integration_verification(protocol, checks, here=here, repo=repo)
            self.assertTrue(any("test_runs[0] log: content hash mismatch" in error for error in errors))

    def test_analyzer_lock_hashes_ordered_path_records_and_file_bytes(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            repo = pathlib.Path(temp)
            here = repo / "benchmarks" / "agent-brain" / "confirmatory"
            analyzer = repo / "benchmarks" / "agent-brain" / "analysis" / "report.py"
            analyzer.parent.mkdir(parents=True)
            analyzer.write_text("VALUE = 1\n", encoding="utf-8")
            relative = "benchmarks/agent-brain/analysis/report.py"
            content_hash = self._sha(analyzer)
            aggregate = CHECK.analyzer_aggregate_sha256([(relative, content_hash)])
            lock = {
                "schema_version": 1,
                "algorithm": CHECK.ANALYZER_LOCK_ALGORITHM,
                "files": [{"path": relative, "sha256": content_hash}],
                "aggregate_sha256": aggregate,
            }
            self._write_json(here / "analyzer-lock.json", lock)
            protocol = {"analyzer_sha256": aggregate}
            check = {"status": "pass", "evidence": "analyzer-lock.json"}
            self.assertEqual(CHECK.validate_analyzer_lock(protocol, check, here=here, repo=repo), [])

            analyzer.write_text("VALUE = 2\n", encoding="utf-8")
            errors = CHECK.validate_analyzer_lock(protocol, check, here=here, repo=repo)
            self.assertTrue(any("content hash mismatch" in error for error in errors))

    def test_protocol_hash_nulls_only_its_self_reference(self) -> None:
        protocol = {"schema_version": 1, "freeze": {"frozen_at": "now", "protocol_sha256": None}}
        expected = CHECK.protocol_content_sha256(protocol)
        protocol["freeze"]["protocol_sha256"] = expected
        self.assertEqual(CHECK.protocol_content_sha256(protocol), expected)
        protocol["schema_version"] = 2
        self.assertNotEqual(CHECK.protocol_content_sha256(protocol), expected)

    def test_preregistration_binds_canonical_engine_pin_path_and_bytes(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            repo = pathlib.Path(temp)
            here = repo / "benchmarks" / "agent-brain" / "confirmatory"
            canonical = repo / CHECK.ENGINE_PINS_REPO_PATH
            canonical.parent.mkdir(parents=True)
            canonical.write_text('{"pin_set_id":"synthetic"}\n', encoding="utf-8")
            binding = {
                "path": CHECK.ENGINE_PINS_REPO_PATH,
                "sha256": self._sha(canonical),
            }
            protocol = {"engine_verification_pins": binding}

            self.assertEqual(
                CHECK.validate_engine_pin_binding(protocol, here=here, repo=repo),
                [],
            )

            frozen_hash = CHECK.protocol_content_sha256(
                {**protocol, "freeze": {"frozen_at": "now", "protocol_sha256": None}}
            )
            canonical.write_text('{"pin_set_id":"tampered"}\n', encoding="utf-8")
            errors = CHECK.validate_engine_pin_binding(protocol, here=here, repo=repo)
            self.assertTrue(any("content hash mismatch" in error for error in errors))

            alternate = repo / "alternate-pins.json"
            shutil.copy2(canonical, alternate)
            redirected = copy.deepcopy(protocol)
            redirected["engine_verification_pins"] = {
                "path": "alternate-pins.json",
                "sha256": self._sha(alternate),
            }
            errors = CHECK.validate_engine_pin_binding(redirected, here=here, repo=repo)
            self.assertTrue(any("path must be" in error for error in errors))

            rebound_hash = CHECK.protocol_content_sha256(
                {**redirected, "freeze": {"frozen_at": "now", "protocol_sha256": None}}
            )
            self.assertNotEqual(rebound_hash, frozen_hash)

            canonical.unlink()
            canonical.symlink_to(alternate)
            symlinked = {
                "engine_verification_pins": {
                    "path": CHECK.ENGINE_PINS_REPO_PATH,
                    "sha256": self._sha(canonical),
                }
            }
            errors = CHECK.validate_engine_pin_binding(symlinked, here=here, repo=repo)
            self.assertTrue(any("must not contain symlinks" in error for error in errors))

            errors = CHECK.validate_engine_pin_binding({}, here=here, repo=repo)
            self.assertIn("preregistration engine_verification_pins must be an object", errors)

    def test_missing_go_no_go_evidence_fails_closed(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            here = pathlib.Path(temp)
            checks = [
                {
                    "id": check_id,
                    "status": "pass" if check_id == "unique_inventory_reconciled" else "pending",
                    "evidence": "missing.json" if check_id == "unique_inventory_reconciled" else None,
                }
                for check_id in CHECK.GO_NO_GO_IDS
            ]
            errors, _ = CHECK.validate_gate_evidence(
                {"checks": checks, "decision": "no_go"},
                here=here,
                repo=here,
            )
            self.assertTrue(any("evidence path does not exist" in error for error in errors))

    def test_dataset_rejects_duplicate_query_hashes_and_unopened_plaintext(self) -> None:
        query_text = "find the relevant fact"
        item = {
            "query_id": "query-1",
            "task_id": "task-1",
            "query_source": "user_prompt_derived",
            "query_text": query_text,
            "query_sha256": hashlib.sha256(query_text.encode()).hexdigest(),
            "temporal_cutoff": "2026-07-15T00:00:00Z",
            "null_query": False,
            "judgments": [],
        }
        duplicate = copy.deepcopy(item)
        duplicate["query_id"] = "query-2"
        dataset = {
            "development": {"items": [item, duplicate]},
            "sealed_holdout": {
                "item_count": 1,
                "unique_task_count": 1,
                "opened_at": None,
                "items": [copy.deepcopy(item)],
            },
        }
        errors = CHECK.validate_dataset(dataset, {"offline_dataset": {}}, freeze=False)
        self.assertIn("development query hashes are not unique strings", errors)
        self.assertIn("plaintext sealed holdout labels are prohibited while unopened", errors)

    def test_oracle_only_task_and_null_cannot_close_product_floors(self) -> None:
        product = {
            "query_id": "product-1",
            "task_id": "task-1",
            "query_source": "user_prompt_derived",
            "query_text": "real product prompt",
            "query_sha256": hashlib.sha256(b"real product prompt").hexdigest(),
            "temporal_cutoff": "2026-07-15T00:00:00Z",
            "null_query": False,
            "judgments": [],
        }
        oracle = {
            "query_id": "oracle-2",
            "task_id": "task-2",
            "query_source": "oracle_upper_bound",
            "query_text": "hand tuned no-answer wording",
            "query_sha256": hashlib.sha256(b"hand tuned no-answer wording").hexdigest(),
            "temporal_cutoff": "2026-07-15T00:00:00Z",
            "null_query": True,
            "judgments": [],
        }
        dataset = {
            "development": {"items": [product, oracle]},
            "sealed_holdout": {
                "item_count": 0,
                "unique_task_count": 0,
                "commitment_sha256": "d" * 64,
                "opened_at": None,
                "items": [],
            },
        }
        protocol = {
            "offline_dataset": {
                "minimum_development_queries": 2,
                "minimum_development_tasks": 2,
                "minimum_development_null_queries": 1,
                "minimum_sealed_holdout_queries": 0,
                "minimum_sealed_holdout_tasks": 0,
            },
            "fresh_holdout": {"commitment_sha256": "d" * 64},
        }
        errors = CHECK.validate_dataset(dataset, protocol, freeze=True)
        self.assertTrue(any("oracle_upper_bound queries cannot be null" in error for error in errors))
        self.assertIn("too few answerable product-derived development relevance tasks", errors)
        self.assertIn("too few corpus-closed product-derived development null queries", errors)

    def test_relevance_contract_rejects_coordinated_snapshot_and_dataset_mutation(self) -> None:
        protocol = json.loads((HERE / "preregistration.json").read_text(encoding="utf-8"))
        with tempfile.TemporaryDirectory() as temp_raw:
            temp = pathlib.Path(temp_raw)
            for name in (
                "offline-relevance-development-labels.json",
                "offline-relevance-fact-snapshot.json",
                "offline-relevance-dataset.json",
                "offline-relevance-review-ledger.json",
                "offline-relevance-source-membership.json",
                "relevance-source-contract.json",
                "offline-relevance-null-review-ledger.json",
                "relevance-null-review-contract.json",
                "task-inventory.json",
            ):
                shutil.copy2(HERE / name, temp / name)
            shutil.copytree(HERE / "schemas", temp / "schemas")

            labels_path = temp / "offline-relevance-development-labels.json"
            snapshot_path = temp / "offline-relevance-fact-snapshot.json"
            dataset_path = temp / "offline-relevance-dataset.json"
            labels = json.loads(labels_path.read_text(encoding="utf-8"))
            snapshot = json.loads(snapshot_path.read_text(encoding="utf-8"))
            changed_fact = snapshot["facts"][0]
            changed_fact["text"] += " Coordinated tamper."
            changed_hash = CHECK.relevance_dataset.fact_sha256(changed_fact)
            for item in labels["items"]:
                for judgment in item["judgments"]:
                    if judgment["fact_id"] == changed_fact["id"]:
                        judgment["fact_sha256"] = changed_hash
            snapshot["facts_sha256"] = CHECK.relevance_dataset.sha256_bytes(
                CHECK.relevance_dataset.canonical_json(snapshot["facts"])
            )
            labels["snapshot_commitment"]["facts_sha256"] = snapshot["facts_sha256"]
            self._write_json(labels_path, labels)
            self._write_json(snapshot_path, snapshot)
            regenerated = self._materialize_relevance_bundle(temp, labels_path, snapshot_path)
            self._write_json(dataset_path, regenerated)

            errors = CHECK.validate_relevance_bundle(protocol, here=temp, repo=CHECK.REPO)
            self.assertIn("relevance labels artifact content hash mismatch", errors)
            self.assertIn("relevance snapshot artifact content hash mismatch", errors)
            self.assertIn("relevance dataset artifact content hash mismatch", errors)

    def test_relevance_contract_hashes_and_applies_schemas(self) -> None:
        protocol = json.loads((HERE / "preregistration.json").read_text(encoding="utf-8"))
        with tempfile.TemporaryDirectory() as temp_raw:
            temp = pathlib.Path(temp_raw)
            for name in (
                "offline-relevance-development-labels.json",
                "offline-relevance-fact-snapshot.json",
                "offline-relevance-dataset.json",
                "offline-relevance-review-ledger.json",
                "offline-relevance-source-membership.json",
                "relevance-source-contract.json",
                "offline-relevance-null-review-ledger.json",
                "relevance-null-review-contract.json",
                "task-inventory.json",
            ):
                shutil.copy2(HERE / name, temp / name)
            shutil.copytree(HERE / "schemas", temp / "schemas")
            schema_path = temp / "schemas" / "relevance-label-source.schema.json"
            schema = json.loads(schema_path.read_text(encoding="utf-8"))
            schema["properties"]["schema_version"]["const"] = 999
            self._write_json(schema_path, schema)

            errors = CHECK.validate_relevance_bundle(protocol, here=temp, repo=CHECK.REPO)
            self.assertIn("relevance labels artifact schema content hash mismatch", errors)
            self.assertTrue(any("labels$.schema_version: value differs from schema const" in error for error in errors))

    def test_hard_pinned_membership_rejects_fabricated_fact_or_date_after_hash_refresh(self) -> None:
        for attack in ("fact", "date"):
            with self.subTest(attack=attack), tempfile.TemporaryDirectory() as temp_raw:
                temp = pathlib.Path(temp_raw)
                self._copy_relevance_bundle(temp)
                protocol = json.loads((HERE / "preregistration.json").read_text(encoding="utf-8"))
                labels_path = temp / "offline-relevance-development-labels.json"
                snapshot_path = temp / "offline-relevance-fact-snapshot.json"
                dataset_path = temp / "offline-relevance-dataset.json"
                labels = json.loads(labels_path.read_text(encoding="utf-8"))
                snapshot = json.loads(snapshot_path.read_text(encoding="utf-8"))
                if attack == "fact":
                    changed_fact = snapshot["facts"][0]
                    changed_fact["text"] += " Fabricated but internally rehashed."
                    changed_hash = CHECK.relevance_dataset.fact_sha256(changed_fact)
                    for item in labels["items"]:
                        for judgment in item["judgments"]:
                            if judgment["fact_id"] == changed_fact["id"]:
                                judgment["fact_sha256"] = changed_hash
                else:
                    session_id = next(iter(snapshot["session_dates"]))
                    snapshot["session_dates"][session_id] = "2020-01-01T00:00:00Z"
                snapshot["facts_sha256"] = CHECK.relevance_dataset.sha256_bytes(
                    CHECK.relevance_dataset.canonical_json(snapshot["facts"])
                )
                snapshot["session_dates_sha256"] = CHECK.relevance_dataset.sha256_bytes(
                    CHECK.relevance_dataset.canonical_json(snapshot["session_dates"])
                )
                labels["snapshot_commitment"] = CHECK.relevance_dataset._snapshot_commitment(snapshot)
                self._write_json(labels_path, labels)
                self._write_json(snapshot_path, snapshot)
                regenerated = self._materialize_relevance_bundle(temp, labels_path, snapshot_path)
                self._write_json(dataset_path, regenerated)
                for role in ("labels", "snapshot", "dataset"):
                    artifact_path = temp / protocol["offline_dataset"]["development_artifact_contract"]["artifacts"][role]["path"]
                    protocol["offline_dataset"]["development_artifact_contract"]["artifacts"][role]["sha256"] = self._sha(artifact_path)

                errors = CHECK.validate_relevance_bundle(protocol, here=temp, repo=CHECK.REPO)
                self.assertFalse(any("artifact content hash mismatch" in error for error in errors))
                expected = (
                    "snapshot fact differs from reviewed full-source membership"
                    if attack == "fact"
                    else "snapshot session date differs from reviewed full-source membership"
                )
                self.assertTrue(any(expected in error for error in errors), errors)

    def test_source_contract_digest_is_not_refreshable_through_preregistration(self) -> None:
        protocol = json.loads((HERE / "preregistration.json").read_text(encoding="utf-8"))
        with tempfile.TemporaryDirectory() as temp_raw:
            temp = pathlib.Path(temp_raw)
            self._copy_relevance_bundle(temp)
            contract_path = temp / "relevance-source-contract.json"
            contract = json.loads(contract_path.read_text(encoding="utf-8"))
            contract["contract_id"] = "attacker-refreshed-contract"
            self._write_json(contract_path, contract)
            errors = CHECK.validate_relevance_bundle(protocol, here=temp, repo=CHECK.REPO)
            self.assertIn(
                "reviewed relevance source contract digest differs from hardcoded trust root",
                errors,
            )

    def test_review_ledger_cannot_be_missing_or_unhashed(self) -> None:
        protocol = json.loads((HERE / "preregistration.json").read_text(encoding="utf-8"))
        with tempfile.TemporaryDirectory() as temp_raw:
            temp = pathlib.Path(temp_raw)
            self._copy_relevance_bundle(temp)
            (temp / "offline-relevance-review-ledger.json").unlink()
            errors = CHECK.validate_relevance_bundle(protocol, here=temp, repo=CHECK.REPO)
            self.assertIn("relevance review_ledger artifact path is unsafe or missing", errors)
        with tempfile.TemporaryDirectory() as temp_raw:
            temp = pathlib.Path(temp_raw)
            self._copy_relevance_bundle(temp)
            labels_path = temp / "offline-relevance-development-labels.json"
            labels = json.loads(labels_path.read_text(encoding="utf-8"))
            del labels["items"][0]["label_evidence"]["source_sha256"]
            self._write_json(labels_path, labels)
            protocol["offline_dataset"]["development_artifact_contract"]["artifacts"]["labels"]["sha256"] = self._sha(labels_path)
            errors = CHECK.validate_relevance_bundle(protocol, here=temp, repo=CHECK.REPO)
            self.assertTrue(any("missing schema-required field source_sha256" in error for error in errors))
            self.assertTrue(any("evidence source hash differs" in error for error in errors))

    def test_b72_null_and_250_answerable_query_close_development_floors(self) -> None:
        labels = json.loads((HERE / "offline-relevance-development-labels.json").read_text())
        dataset = json.loads((HERE / "offline-relevance-dataset.json").read_text())
        query_ids = {item["query_id"] for item in labels["items"]}
        self.assertIn("dev-product-b72a6e621", query_ids)
        self.assertIn("dev-product-250538b1f", query_ids)
        self.assertEqual(dataset["development"]["item_count"], 14)
        self.assertEqual(dataset["development"]["unique_task_count"], 13)
        self.assertEqual(dataset["development"]["unique_answerable_product_task_count"], 12)
        self.assertEqual(dataset["development"]["product_null_query_count"], 1)
        null_item = next(
            item for item in dataset["development"]["items"]
            if item["query_id"] == "dev-product-b72a6e621"
        )
        self.assertEqual(null_item["null_closure"]["reviewed_fact_count"], 2531)
        self.assertEqual(null_item["null_closure"]["positive_fact_count"], 0)
        protocol = json.loads((HERE / "preregistration.json").read_text())
        errors = CHECK.validate_dataset(dataset, protocol, freeze=True)
        self.assertNotIn("too few answerable product-derived development relevance tasks", errors)
        self.assertNotIn("too few corpus-closed product-derived development null queries", errors)

    def test_engine_storage_v2_pending_is_valid_but_cannot_satisfy_production(self) -> None:
        contract_path = HERE / "engine-evidence-storage.json"
        diagnostic_errors: list[str] = []
        diagnostic = CHECK._validate_engine_storage_contract(
            contract_path,
            diagnostic_errors,
            repo=CHECK.REPO,
            require_published=False,
        )
        self.assertEqual(diagnostic_errors, [])
        self.assertIsNotNone(diagnostic)
        self.assertFalse(diagnostic["restricted_replay_verified"])

        production_errors: list[str] = []
        production = CHECK._validate_engine_storage_contract(
            contract_path,
            production_errors,
            repo=CHECK.REPO,
            require_published=True,
        )
        self.assertIsNone(production)
        self.assertEqual(
            production_errors,
            ["engine evidence storage v2 is pending owner authorization"],
        )

    def test_engine_storage_checker_and_trust_dependencies_fail_closed(self) -> None:
        cases = (
            ("engine-replay-checker-lock.json", "unlink", "checker lock cannot be opened"),
            ("engine-replay-checker-lock.json", "append", "checker lock raw hash differs"),
            ("engine-replay-trust-roots.json", "unlink", "trust roots cannot be opened"),
            ("engine-replay-trust-roots.json", "append", "trust roots raw hash changed"),
        )
        for name, operation, expected in cases:
            with self.subTest(name=name, operation=operation), tempfile.TemporaryDirectory() as raw:
                repo = pathlib.Path(raw)
                contract_path = self._copy_engine_storage_lock_bundle(repo)
                target = repo / "benchmarks/agent-brain/confirmatory" / name
                if operation == "unlink":
                    target.unlink()
                else:
                    target.write_bytes(target.read_bytes() + b" ")
                with self.assertRaisesRegex(CHECK.hydrate_engine_evidence.HydrationError, expected):
                    CHECK.hydrate_engine_evidence.load_storage_contract_v2(
                        contract_path,
                        repo=repo,
                    )

    def test_engine_production_interfaces_do_not_accept_verifier_injection(self) -> None:
        self.assertNotIn(
            "attestation_verifier",
            inspect.signature(CHECK.validate_engine_verification).parameters,
        )
        self.assertNotIn("ssh_keygen", inspect.signature(ATTEST.verify_envelope).parameters)

    def test_public_v4_cannot_satisfy_production_without_restricted_replay(self) -> None:
        with tempfile.TemporaryDirectory() as raw:
            here = pathlib.Path(raw)
            manifest = here / "public-v4.json"
            manifest.write_text('{"schema_version":4}\n', encoding="utf-8")
            with (
                mock.patch.object(CHECK, "_validate_engine_pins", return_value={}),
                mock.patch.object(CHECK, "_engine_pin_descriptor", return_value={}),
                mock.patch.object(CHECK, "_load_artifact", return_value={"schema_version": 4}),
                mock.patch.object(CHECK, "_validate_public_engine_manifest_schema"),
                mock.patch.object(CHECK.public_engine_evidence, "validate_public_bundle", return_value=[]),
            ):
                errors = CHECK.validate_engine_verification(
                    {},
                    {"status": "pass", "evidence": "public-v4.json"},
                    here=here,
                    repo=here,
                    pins={},
                    require_production=True,
                )
        self.assertEqual(
            errors,
            [
                "public v4 evidence lacks an authenticated restricted replay attestation bound to the v4 manifest, pin set, and checker"
            ],
        )

    def test_verified_two_root_snapshot_is_not_reopened_downstream(self) -> None:
        with tempfile.TemporaryDirectory() as raw:
            repo = pathlib.Path(raw)
            contract = repo / CHECK.ENGINE_EVIDENCE_STORAGE_REPO_PATH
            contract.parent.mkdir(parents=True)
            contract.write_text("{}\n", encoding="utf-8")
            relocation = {
                "manifest_path": contract.parent / ".engine-evidence/public-v4-v1/engine-verification-public-v4.json",
                "artifact_repo": contract.parent / ".engine-evidence",
                "restricted_replay_verified": True,
            }
            with (
                mock.patch.object(CHECK, "_validate_engine_pins", return_value={}),
                mock.patch.object(CHECK, "_engine_pin_descriptor", return_value={}),
                mock.patch.object(CHECK, "_validate_engine_storage_contract", return_value=relocation),
                mock.patch.object(CHECK, "_load_artifact", side_effect=AssertionError("mutable tree reopened")) as load_artifact,
                mock.patch.object(
                    CHECK.public_engine_evidence,
                    "validate_public_bundle",
                    side_effect=AssertionError("mutable tree revalidated"),
                ) as validate_public,
            ):
                errors = CHECK.validate_engine_verification(
                    {},
                    {
                        "status": "pass",
                        "evidence": CHECK.ENGINE_EVIDENCE_STORAGE_REPO_PATH,
                    },
                    here=contract.parent,
                    repo=repo,
                    pins={},
                    require_production=True,
                    require_storage_contract=True,
                )
            self.assertEqual(errors, [])
            load_artifact.assert_not_called()
            validate_public.assert_not_called()

    def test_json_loader_rejects_duplicate_schema_keys(self) -> None:
        with tempfile.TemporaryDirectory() as raw:
            path = pathlib.Path(raw) / "duplicate-schema.json"
            path.write_text('{"type":"object","type":"array"}\n', encoding="utf-8")
            with self.assertRaisesRegex(json.JSONDecodeError, "duplicate object key"):
                CHECK.load(path)
            for raw_json in (
                '{"value":' + "1" * 5000 + '}',
                '{"value":' + "[" * 2000 + "0" + "]" * 2000 + '}',
            ):
                path.write_text(raw_json, encoding="utf-8")
                with self.assertRaisesRegex(json.JSONDecodeError, "unsafe JSON structure"):
                    CHECK.load(path)

    def test_legacy_engine_storage_v1_is_diagnostic_only(self) -> None:
        errors: list[str] = []
        result = CHECK._validate_engine_storage_contract(
            HERE / "engine-evidence-storage-legacy-v1.json",
            errors,
            repo=CHECK.REPO,
            require_published=True,
            validate_hydrated=False,
            allow_legacy_diagnostic=False,
        )
        self.assertIsNone(result)
        self.assertIn(
            "legacy engine evidence storage v1 is diagnostic-only and cannot satisfy production",
            errors,
        )

    def test_engine_gate_rejects_legacy_unpinned_evidence(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            repo = pathlib.Path(temp)
            here = repo / "benchmarks" / "agent-brain" / "confirmatory"
            matrix = json.loads((HERE / "engine-matrix.json").read_text(encoding="utf-8"))
            legacy_records = [
                {
                    "schema_version": 1,
                    "arm": arm["id"],
                    "requested": {
                        "command": "entire-brain recall --json",
                        "environment": dict(arm["environment"]),
                        "namespace": arm["namespace"],
                    },
                    "effective": {},
                    "artifacts": {},
                    "corpus": {},
                    "result": {},
                }
                for arm in matrix["arms"]
            ]
            self._write_json(here / "engine-verification.json", {"records": legacy_records})
            errors = CHECK.validate_engine_verification(
                matrix,
                {"status": "pass", "evidence": "engine-verification.json"},
                here=here,
                repo=repo,
                pin_repo=CHECK.REPO,
            )
            self.assertEqual(sum("missing required field pin_set" in error for error in errors), 3)


if __name__ == "__main__":
    unittest.main()
