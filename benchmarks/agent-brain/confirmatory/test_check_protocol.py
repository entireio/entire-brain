from __future__ import annotations

import copy
from datetime import datetime, timezone
import hashlib
import importlib.util
import json
import pathlib
import tempfile
import unittest


HERE = pathlib.Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location("check_protocol", HERE / "check_protocol.py")
assert SPEC and SPEC.loader
CHECK = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(CHECK)


class ProtocolCheckTest(unittest.TestCase):
    @staticmethod
    def _write_json(path: pathlib.Path, value: object) -> None:
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n", encoding="utf-8")

    @staticmethod
    def _sha(path: pathlib.Path) -> str:
        return hashlib.sha256(path.read_bytes()).hexdigest()

    def _complete_pricing_budget(
        self,
        repo: pathlib.Path,
        here: pathlib.Path,
    ) -> tuple[dict[str, object], dict[str, dict[str, str]], dict[str, object]]:
        quote_source = here / "pricing-evidence" / "synthetic-quote.txt"
        quote_source.parent.mkdir(parents=True, exist_ok=True)
        quote_source.write_text("synthetic test quote; not a provider price\n", encoding="utf-8")
        runner = {
            "provider": "synthetic-provider",
            "runner_id": "synthetic-runner",
            "runner_version": "test-v1",
            "model_id": "synthetic-model",
            "effort": "synthetic-effort",
        }
        protocol: dict[str, object] = {
            "protocol_id": "synthetic-protocol",
            "agent_design": {
                "primary_treatments": ["a", "b", "c"],
                "tasks": 2,
                "repetitions_per_treatment": 1,
                "requested_cells": 6,
                "maximum_calls_with_reserve": 7,
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
            "schema_version": 1,
            "runner": runner,
            "pricing_quote": {
                "status": "pinned",
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
                    "cached_input": "0.5",
                    "output": "8",
                },
            },
            "design": {
                "protocol_id": "synthetic-protocol",
                "power_status_at_binding": "pass",
                "approved_for_budgeting": True,
                "tasks": 2,
                "treatments": ["a", "b", "c"],
                "repetitions_per_treatment": 1,
                "requested_calls": 6,
                "reserve_calls": 1,
                "maximum_calls_with_reserve": 7,
            },
            "token_assumptions": {
                "mode": "explicit_per_call_caps",
                "explicit_per_call_caps": {
                    "uncached_input": 1000,
                    "cached_input": 500,
                    "output": 100,
                    "rationale": "synthetic unit-test caps",
                },
                "empirical_bound": {
                    "runner": None,
                    "evidence_path": None,
                    "evidence_sha256": None,
                    "statistic": None,
                    "quantile": None,
                    "safety_multiplier": None,
                    "observed_tokens_per_call": {
                        "uncached_input": None,
                        "cached_input": None,
                        "output": None,
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
        calculation = CHECK.pricing_budget.calculate(contract)
        self.assertEqual(calculation["per_call_maximum_usd"], "0.00305")
        self.assertEqual(calculation["maximum_usd"], "0.02135")
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

    def test_preparation_artifacts_are_consistent(self) -> None:
        self.assertEqual(CHECK.validate(freeze=False), [])

    def test_freeze_is_fail_closed_after_dependencies_pass(self) -> None:
        errors = CHECK.validate(freeze=True)
        self.assertTrue(errors)
        self.assertNotIn("WS2-WS5 dependencies are pending", errors)
        self.assertIn("fresh holdout commitment is not frozen", errors)
        self.assertIn("paid-run checklist is not all pass", errors)

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

    def test_power_artifact_is_derived_and_failed_decision_is_completed(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            here = pathlib.Path(temp)
            artifact = CHECK.power_analysis.build_report()
            self._write_json(here / "power-analysis.json", artifact)
            protocol = {
                "agent_design": {
                    "power": {
                        "completed": True,
                        "status": "fail_calibration_insufficient",
                        "evidence": "power-analysis.json",
                        "analysis_kind": artifact["analysis_kind"],
                        "design_options_evidence": "power-analysis.json#design_options",
                        "exploratory_calibration": {
                            "manifest": "power-calibration-exploratory-v1.json",
                            "eligibility": "exploratory_only",
                            "confirmatory_assumption_source": False,
                            "unique_task_ids": 12,
                            "paired_task_cluster_instances": 14,
                            "pooled_estimate_prohibited": True,
                        },
                        "design_decision_required": True,
                    }
                }
            }
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

    def test_empirical_token_bound_is_hashed_and_bound_to_the_pinned_runner(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            repo = pathlib.Path(temp)
            here = repo / "benchmarks" / "agent-brain" / "confirmatory"
            protocol, checks, contract = self._complete_pricing_budget(repo, here)
            evidence = here / "pricing-evidence" / "synthetic-token-sample.json"
            evidence.write_text('{"synthetic": true}\n', encoding="utf-8")
            contract["token_assumptions"] = {
                "mode": "empirical_bound",
                "explicit_per_call_caps": {
                    "uncached_input": None,
                    "cached_input": None,
                    "output": None,
                    "rationale": None,
                },
                "empirical_bound": {
                    "runner": copy.deepcopy(contract["runner"]),
                    "evidence_path": evidence.relative_to(repo).as_posix(),
                    "evidence_sha256": self._sha(evidence),
                    "statistic": "synthetic upper quantile",
                    "quantile": 0.95,
                    "safety_multiplier": "1.25",
                    "observed_tokens_per_call": {
                        "uncached_input": 800,
                        "cached_input": 400,
                        "output": 80,
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

            contract["token_assumptions"]["empirical_bound"]["runner"]["model_id"] = "different-model"  # type: ignore[index]
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

    def test_engine_gate_verifies_exact_arms_runtime_state_and_artifact_bytes(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            repo = pathlib.Path(temp)
            here = repo / "benchmarks" / "agent-brain" / "confirmatory"
            matrix = json.loads((HERE / "engine-matrix.json").read_text(encoding="utf-8"))
            shared_files: dict[str, tuple[str, str]] = {}
            for name in (
                "binary",
                "stdout",
                "stderr",
                "vector-model2vec",
                "vector-embeddinggemma",
                "model",
            ):
                path = here / "engine-artifacts" / name
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_bytes((name + "\n").encode())
                relative = path.relative_to(repo).as_posix()
                shared_files[name] = (relative, self._sha(path))

            records = []
            for arm in matrix["arms"]:
                semantic = arm["semantic"]
                vector_path, vector_hash = (
                    shared_files[
                        "vector-embeddinggemma"
                        if arm["id"] == "embeddinggemma_rrf"
                        else "vector-model2vec"
                    ]
                    if semantic
                    else (None, None)
                )
                model_path, model_hash = (
                    shared_files["model"] if arm["id"] == "embeddinggemma_rrf" else (None, None)
                )
                records.append(
                    {
                        "schema_version": 1,
                        "arm": arm["id"],
                        "requested": {
                            "command": "entire-brain recall --json",
                            "environment": dict(arm["environment"]),
                            "namespace": arm["namespace"],
                        },
                        "effective": {
                            "engine": arm["id"],
                            "semantic_available": semantic,
                            "bm25_enabled": False,
                            "fallback_used": False,
                            "embedder_id": f"{arm['id']}-embedder" if semantic else None,
                            "embedding_dimension": 768 if semantic else None,
                            "vector_count": 8 if semantic else 0,
                            "vector_namespace": arm["namespace"],
                        },
                        "artifacts": {
                            "binary_path": shared_files["binary"][0],
                            "binary_sha256": shared_files["binary"][1],
                            "stdout_path": shared_files["stdout"][0],
                            "stdout_sha256": shared_files["stdout"][1],
                            "stderr_path": shared_files["stderr"][0],
                            "stderr_sha256": shared_files["stderr"][1],
                            "vector_artifact_path": vector_path,
                            "vector_artifact_sha256": vector_hash,
                            "embedding_model_path": model_path,
                            "embedding_model_sha256": model_hash,
                        },
                        "corpus": {
                            "facts_sha256": "f" * 64,
                            "prefilter_count": 10,
                            "eligible_count": 8,
                            "excluded_by_reason": {"future": 2},
                            "delivered_count": 3,
                        },
                        "result": {
                            "query_id": "query-1",
                            "fact_ids_in_order": ["fact-1", "fact-2"],
                            "output_valid": True,
                        },
                    }
                )
            self._write_json(here / "engine-verification.json", {"records": records})
            check = {"status": "pass", "evidence": "engine-verification.json"}
            self.assertEqual(
                CHECK.validate_engine_verification(matrix, check, here=here, repo=repo),
                [],
            )

            records[1]["effective"]["fallback_used"] = True
            self._write_json(here / "engine-verification.json", {"records": records})
            errors = CHECK.validate_engine_verification(matrix, check, here=here, repo=repo)
            self.assertTrue(any("fallback is prohibited" in error for error in errors))
            records[1]["effective"]["fallback_used"] = False
            (here / "engine-artifacts" / "binary").write_text("TAMPERED\n", encoding="utf-8")
            self._write_json(here / "engine-verification.json", {"records": records})
            errors = CHECK.validate_engine_verification(matrix, check, here=here, repo=repo)
            self.assertTrue(any("binary artifact: content hash mismatch" in error for error in errors))


if __name__ == "__main__":
    unittest.main()
