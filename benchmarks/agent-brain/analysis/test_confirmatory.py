from __future__ import annotations

import copy
import json
import pathlib
import sys
import tempfile
import unittest
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


def make_records(
    *,
    tasks: tuple[str, ...] = ("task-a", "task-b", "task-c", "task-d"),
    repetitions: int = 2,
    retrieved_correct: bool = True,
) -> list[dict]:
    records: list[dict] = []
    token_ratios = {"no_memory": 1.0, "placebo_packet": 1.1, "retrieved_memory": 0.8}
    time_ratios = {"no_memory": 1.0, "placebo_packet": 1.05, "retrieved_memory": 0.75}
    for task_index, task_id in enumerate(tasks, 1):
        baseline_tokens = 100.0 * task_index
        baseline_time = 10.0 * task_index
        for repetition in range(1, repetitions + 1):
            for arm in confirmatory.PRIMARY_ARMS:
                condition = CONDITIONS[arm]
                correct = retrieved_correct if arm == "retrieved_memory" else True
                run_id = f"{task_id}__runner__{condition}__r{repetition}"
                records.append(
                    {
                        "run_id": run_id,
                        "task_id": task_id,
                        "runner": {"id": "runner"},
                        "condition": condition,
                        "repetition": repetition,
                        "agent_ran": True,
                        "treatment": {
                            "arm": arm,
                            "query_source": "user_query",
                            "confirmatory_eligible": True,
                        },
                        "retrieval_query_source": "user_query",
                        "validation": {"ok": correct},
                        "agent_info": {
                            "returncode": 0,
                            "usage": {"total_tokens": baseline_tokens * token_ratios[arm]},
                        },
                        "timing": {
                            "harness_agent_interval_wall_seconds": baseline_time * time_ratios[arm],
                            "agent_reported_api_seconds": baseline_time * 0.8 * time_ratios[arm],
                        },
                    }
                )
    return records


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


class ConfirmatoryAnalysisTests(unittest.TestCase):
    def analyze(self, records: list[dict], *, repetitions: int = 2) -> dict:
        tasks = sorted({record["task_id"] for record in records})
        return confirmatory.analyze_records(
            records,
            expected_task_ids=tasks,
            expected_runner_id="runner",
            repetitions=repetitions,
            resamples=199,
            seed=1234,
        )

    def test_paired_estimands_gate_then_analyze_tokens_and_both_times(self) -> None:
        report = self.analyze(make_records())
        correctness = report["correctness"]["comparisons"]["retrieved_memory"]
        self.assertEqual(correctness["task_level_paired_pass_rate_difference"], 0.0)
        self.assertEqual(correctness["one_sided_percentile_lower_bound"], 0.0)
        self.assertTrue(report["correctness"]["primary_gate"]["passed"])

        tokens = report["tokens"]["comparisons"]["retrieved_memory"]
        self.assertAlmostEqual(tokens["paired_task_geometric_mean_ratio"], 0.8)
        self.assertAlmostEqual(tokens["two_sided_percentile_ci"][0], 0.8)
        self.assertAlmostEqual(tokens["two_sided_percentile_ci"][1], 0.8)
        self.assertTrue(tokens["ratio_below_one_supported"])
        self.assertEqual(report["tokens"]["multiplicity"], "holm_within_total_token_endpoint_family")

        wall = report["wall_time"]
        self.assertEqual(wall["status"], "evaluated_secondary")
        self.assertEqual(
            wall["endpoints"]["harness_wall_seconds"]["source_field"],
            "harness_agent_interval_wall_seconds",
        )
        self.assertEqual(
            wall["endpoints"]["agent_api_seconds"]["source_field"],
            "agent_reported_api_seconds",
        )
        for endpoint in wall["endpoints"].values():
            ratio = endpoint["comparisons"]["retrieved_memory"]["paired_task_geometric_mean_ratio"]
            self.assertAlmostEqual(ratio, 0.75)

    def test_token_and_wall_endpoints_are_withheld_when_correctness_fails(self) -> None:
        report = self.analyze(make_records(retrieved_correct=False))
        self.assertFalse(report["correctness"]["primary_gate"]["passed"])
        self.assertEqual(report["tokens"]["status"], "not_evaluated_correctness_gate")
        self.assertEqual(report["wall_time"]["status"], "not_evaluated_correctness_gate")

    def test_post_agent_error_without_validation_is_retained_as_incorrect(self) -> None:
        records = make_records()
        target = next(record for record in records if record["treatment"]["arm"] == "retrieved_memory")
        target.pop("validation")
        target["error"] = "post-agent integrity audit failed"
        report = self.analyze(records)
        self.assertEqual(report["integrity"]["missing_validation_scored_incorrect"], 1)
        self.assertEqual(report["integrity"]["valid_executed_attempts"], len(records))

    def test_holm_is_step_down_and_deterministic(self) -> None:
        adjusted = confirmatory.holm_adjust({"retrieved": 0.01, "placebo": 0.03})
        self.assertAlmostEqual(adjusted["retrieved"]["holm_adjusted_p_value"], 0.02)
        self.assertAlmostEqual(adjusted["placebo"]["holm_adjusted_p_value"], 0.03)
        self.assertTrue(adjusted["retrieved"]["holm_rejected_at_family_alpha"])
        self.assertTrue(adjusted["placebo"]["holm_rejected_at_family_alpha"])

    def test_analysis_is_deterministic_for_seed_and_resample_count(self) -> None:
        records = make_records()
        first = self.analyze(records)
        second = self.analyze(copy.deepcopy(records))
        self.assertEqual(first, second)
        self.assertEqual(first["bootstrap"]["resamples"], 199)
        self.assertEqual(first["bootstrap"]["seed"], 1234)
        self.assertEqual(first["bootstrap"]["index_generator"], "sha256_counter_rejection_v1")

    def test_bootstrap_index_stream_is_frozen(self) -> None:
        self.assertEqual(
            confirmatory._bootstrap_indices(4, 3, 1234),
            [(0, 1, 1, 1), (1, 3, 0, 0), (2, 0, 0, 0)],
        )

    def test_duplicate_cell_fails_closed(self) -> None:
        records = make_records()
        records[-1] = copy.deepcopy(records[0])
        with self.assertRaisesRegex(confirmatory.AnalysisInputError, "duplicate run_id"):
            self.analyze(records)

    def test_missing_cell_fails_closed_against_expected_task_set(self) -> None:
        records = make_records()
        expected_tasks = sorted({record["task_id"] for record in records})
        records.pop()
        with self.assertRaisesRegex(confirmatory.AnalysisInputError, "expected 24 primary cells"):
            confirmatory.analyze_records(
                records,
                expected_task_ids=expected_tasks,
                expected_runner_id="runner",
                repetitions=2,
                resamples=19,
                seed=1,
            )

    def test_nonpositive_tokens_fail_closed(self) -> None:
        records = make_records()
        records[0]["agent_info"]["usage"]["total_tokens"] = 0
        with self.assertRaisesRegex(confirmatory.AnalysisInputError, "finite positive"):
            self.analyze(records)

    def test_missing_api_time_withholds_only_that_secondary_endpoint(self) -> None:
        records = make_records()
        records[0]["timing"]["agent_reported_api_seconds"] = None
        report = self.analyze(records)
        self.assertEqual(report["tokens"]["status"], "evaluated")
        self.assertEqual(
            report["wall_time"]["status"],
            "evaluated_secondary_with_unavailable_endpoint",
        )
        self.assertEqual(
            report["wall_time"]["endpoints"]["harness_wall_seconds"]["status"],
            "evaluated",
        )
        api = report["wall_time"]["endpoints"]["agent_api_seconds"]
        self.assertEqual(api["status"], "not_evaluated_missing_measurements")
        self.assertEqual(api["missing_measurements"], 1)

    def test_present_nonpositive_api_time_still_fails_closed(self) -> None:
        records = make_records()
        records[0]["timing"]["agent_reported_api_seconds"] = 0
        with self.assertRaisesRegex(confirmatory.AnalysisInputError, "agent_reported_api_seconds"):
            self.analyze(records)

    def test_nonexecuted_cell_fails_closed(self) -> None:
        records = make_records()
        records[0]["agent_ran"] = False
        with self.assertRaisesRegex(confirmatory.AnalysisInputError, "not an explicitly executed"):
            self.analyze(records)

    def test_verified_schedule_identity_mismatch_fails_closed(self) -> None:
        records = make_records()
        expected = schedule_cells(records)
        expected[0]["condition"] = "tampered"
        with self.assertRaisesRegex(confirmatory.AnalysisInputError, "disagree with schedule"):
            confirmatory.analyze_records(
                records,
                expected_task_ids=("task-a", "task-b", "task-c", "task-d"),
                expected_runner_id="runner",
                repetitions=2,
                resamples=19,
                seed=1,
                expected_schedule_cells=expected,
            )

    def test_verified_suite_interface_uses_schedule_not_holdout_metadata(self) -> None:
        records = make_records(tasks=("task-a", "task-b"))
        cells = schedule_cells(records)
        schedule = {
            "schema": 1,
            "repetitions": 2,
            "cells": cells,
        }
        schedule["schedule_sha256"] = confirmatory._stable_json_sha256(schedule)
        state = {
            "schedule_sha256": schedule["schedule_sha256"],
            "planned_cell_count": len(cells),
            "recorded_cell_count": len(cells),
            "actual_started_order": [cell["run_id"] for cell in cells],
            "actual_finished_order": [cell["run_id"] for cell in cells],
            "deviations": [],
        }
        current_records = evidence.current_analyzer_records(HERE)
        current_aggregate = evidence.analyzer_aggregate_sha256(current_records)
        manifest = {
            "suite_id": "fixture-suite",
            "identity_sha256": "fixture-identity",
            "harness": {"confirmatory_eligible": True},
            "analyzer": {
                "algorithm": evidence.ANALYZER_AGGREGATE_ALGORITHM,
                "aggregate_sha256": current_aggregate,
            },
            "requested_cells": {
                "count": len(cells),
                "schedule": {
                    "schedule_sha256": schedule["schedule_sha256"],
                    "cell_count": len(cells),
                },
            },
        }
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            (root / "evidence-manifest.json").write_text(json.dumps(manifest))
            (root / "schedule.json").write_text(json.dumps(schedule))
            (root / "schedule-state.json").write_text(json.dumps(state))
            (root / "records.ndjson").write_text(
                "".join(json.dumps(record) + "\n" for record in records)
            )
            with mock.patch.object(
                confirmatory, "verify_bundle", return_value={"ok": True, "errors": []}
            ):
                report = confirmatory.analyze_verified_suite(
                    root,
                    expected_analyzer_sha256=current_aggregate,
                    expected_task_count=2,
                    repetitions=2,
                    resamples=199,
                    seed=1234,
                )
        self.assertTrue(report["integrity"]["evidence_bundle_verified"])
        self.assertFalse(report["integrity"]["holdout_metadata_loaded"])
        self.assertTrue(report["analyzer_identity"]["matched"])
        self.assertEqual(report["analyzer_identity"]["suite_sha256"], current_aggregate)

    def test_analyzer_aggregate_binds_paths_to_content_hashes(self) -> None:
        original = [
            {"source_path": "a.py", "sha256": "1" * 64},
            {"source_path": "b.py", "sha256": "2" * 64},
        ]
        swapped = [
            {"source_path": "a.py", "sha256": "2" * 64},
            {"source_path": "b.py", "sha256": "1" * 64},
        ]
        self.assertNotEqual(
            evidence.analyzer_aggregate_sha256(original),
            evidence.analyzer_aggregate_sha256(swapped),
        )

    def test_analyzer_manifest_rejects_source_path_and_aggregate_tampering(self) -> None:
        records = evidence.current_analyzer_records(HERE)
        value = {
            "algorithm": evidence.ANALYZER_AGGREGATE_ALGORITHM,
            "artifacts": records,
            "aggregate_sha256": evidence.analyzer_aggregate_sha256(records),
        }
        self.assertEqual(evidence.validate_analyzer_manifest(value), [])
        path_tampered = copy.deepcopy(value)
        path_tampered["artifacts"][0]["source_path"] = "benchmarks/agent-brain/analysis/reports/not-runtime.md"
        self.assertTrue(evidence.validate_analyzer_manifest(path_tampered))
        aggregate_tampered = copy.deepcopy(value)
        aggregate_tampered["aggregate_sha256"] = "0" * 64
        self.assertIn(
            "suite manifest analyzer aggregate hash mismatch",
            evidence.validate_analyzer_manifest(aggregate_tampered),
        )

    def test_analyzer_lock_loader_rejects_tampered_aggregate(self) -> None:
        records = evidence.current_analyzer_records(HERE)
        lock = {
            "schema_version": 1,
            "algorithm": evidence.ANALYZER_AGGREGATE_ALGORITHM,
            "files": [
                {"path": item["source_path"], "sha256": item["sha256"]}
                for item in records
            ],
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
