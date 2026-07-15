from __future__ import annotations

import importlib.util
import json
import math
import pathlib
import tempfile
import unittest


HERE = pathlib.Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location("power_analysis", HERE / "power_analysis.py")
assert SPEC and SPEC.loader
POWER = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(POWER)


class PowerAnalysisTest(unittest.TestCase):
    def test_design_inputs_match_candidate_preregistration(self) -> None:
        protocol = json.loads((HERE / "preregistration.json").read_text(encoding="utf-8"))
        design = protocol["agent_design"]
        self.assertEqual(POWER.TASKS, design["tasks"])
        self.assertEqual(POWER.REPETITIONS, design["repetitions_per_treatment"])
        self.assertEqual(POWER.PRIMARY_TREATMENTS, len(design["primary_treatments"]))
        self.assertEqual(POWER.REQUESTED_CELLS, design["requested_cells"])
        self.assertEqual(POWER.TARGET_POWER, design["power"]["target"])
        self.assertEqual(POWER.TOKEN_REDUCTION_TARGET, design["power"]["token_reduction_target"])
        self.assertEqual(POWER.CORRECTNESS_MARGIN, design["correctness"]["margin_absolute"])

    def test_token_task_sd_keeps_irreducible_task_heterogeneity(self) -> None:
        assumptions = {
            "between_task_log_ratio_sd": 0.25,
            "within_attempt_log_token_sd": 0.30,
            "cross_treatment_residual_correlation": 0.0,
        }
        at_four = POWER.token_task_sd(assumptions, 4)
        at_many = POWER.token_task_sd(assumptions, 1_000_000)
        self.assertGreater(at_four, at_many)
        self.assertAlmostEqual(at_many, 0.25, places=5)

    def test_power_increases_with_tasks(self) -> None:
        effect = abs(math.log(POWER.TOKEN_RATIO_TARGET))
        low = POWER.normal_two_sided_power(effect, 0.30 / math.sqrt(24), POWER.PLANNING_ALPHA)
        high = POWER.normal_two_sided_power(effect, 0.30 / math.sqrt(96), POWER.PLANNING_ALPHA)
        self.assertGreater(high, low)

    def test_conservative_design_fails_and_repetitions_alone_do_not_rescue_it(self) -> None:
        report = POWER.build_report()
        scenarios = {item["id"]: item for item in report["scenarios"]}
        conservative = scenarios["conservative_planning"]["results"]
        self.assertFalse(conservative["token"]["target_met"])
        self.assertFalse(conservative["correctness_noninferiority"]["target_met"])
        self.assertFalse(conservative["both_endpoints"]["both_marginal_targets_met"])
        self.assertFalse(report["conservative_repetition_limit"]["repetitions_only_can_meet_both_marginal_targets"])
        self.assertEqual(report["status"], "fail")
        self.assertFalse(report["decision"]["passed"])

    def test_tradeoff_table_compares_tasks_repetitions_and_cells(self) -> None:
        report = POWER.build_report()
        tradeoffs = report["design_options"]
        by_repetitions = {
            row["repetitions_per_treatment"]: row
            for row in tradeoffs["minimum_tasks_by_repetitions"]
        }
        self.assertEqual(by_repetitions[1]["minimum_tasks_for_both_marginal_targets"], 295)
        self.assertEqual(by_repetitions[1]["requested_cells_at_minimum_tasks"], 885)
        self.assertEqual(by_repetitions[4]["minimum_tasks_for_token_target"], 63)
        self.assertEqual(
            by_repetitions[4]["minimum_tasks_for_correctness_noninferiority_target"], 118
        )
        self.assertEqual(by_repetitions[4]["requested_cells_at_minimum_tasks"], 1416)
        by_tasks = {row["tasks"]: row for row in tradeoffs["minimum_repetitions_by_tasks"]}
        self.assertIsNone(by_tasks[24]["minimum_repetitions_for_both_marginal_targets"])
        self.assertEqual(by_tasks[118]["minimum_repetitions_for_both_marginal_targets"], 4)
        self.assertEqual(tradeoffs["status"], "arithmetic_sensitivity_not_an_approved_design")

    def test_exploratory_calibration_is_hashed_sparse_and_quarantined(self) -> None:
        report = POWER.build_report()
        calibration = report["exploratory_calibration"]
        self.assertTrue(report["empirical_variance_computed_for_exploratory_diagnostics"])
        self.assertFalse(report["empirical_variance_used_in_confirmatory_decision"])
        self.assertEqual(
            calibration["eligibility"],
            "exploratory_only_excluded_from_confirmatory_decision",
        )
        self.assertFalse(calibration["confirmatory_assumption_source"])
        self.assertTrue(calibration["pooled_estimate_prohibited"])
        self.assertEqual(calibration["paired_task_cluster_instances"], 14)
        self.assertEqual(calibration["unique_task_ids_across_sources"], 12)
        self.assertEqual(
            [source["id"] for source in calibration["sources"]],
            [
                "semantic_layer_b_pilot",
                "semantic_layer_b_proof",
                "legacy_full_cli_compact",
                "legacy_full_brain_replay",
            ],
        )
        self.assertTrue(
            all(
                source["eligibility"] == "exploratory_only_excluded_from_confirmatory_decision"
                for source in calibration["sources"]
            )
        )

    def test_calibration_cannot_change_confirmatory_power_decision(self) -> None:
        baseline = POWER.build_report()
        original = POWER.build_calibration_diagnostics
        try:
            POWER.build_calibration_diagnostics = lambda: {"synthetic": "not a decision input"}
            modified = POWER.build_report()
        finally:
            POWER.build_calibration_diagnostics = original
        self.assertEqual(modified["scenarios"], baseline["scenarios"])
        self.assertEqual(modified["design_options"], baseline["design_options"])
        self.assertEqual(modified["decision"], baseline["decision"])
        self.assertEqual(modified["status"], "fail")

    def test_calibration_manifest_fails_closed_on_decision_use_or_hash_change(self) -> None:
        manifest = json.loads(POWER.CALIBRATION_MANIFEST.read_text(encoding="utf-8"))
        with tempfile.TemporaryDirectory() as temp:
            path = pathlib.Path(temp) / "manifest.json"
            promoted = json.loads(json.dumps(manifest))
            promoted["decision_use"]["may_pass_power_gate"] = True
            path.write_text(json.dumps(promoted), encoding="utf-8")
            with self.assertRaisesRegex(ValueError, "decision-use quarantine"):
                POWER.build_calibration_diagnostics(path)

            bad_hash = json.loads(json.dumps(manifest))
            bad_hash["sources"][0]["sha256"] = "0" * 64
            path.write_text(json.dumps(bad_hash), encoding="utf-8")
            with self.assertRaisesRegex(ValueError, "content hash mismatch"):
                POWER.build_calibration_diagnostics(path)

    def test_favorable_sensitivity_is_distinct_from_decision_scenario(self) -> None:
        report = POWER.build_report()
        scenarios = {item["id"]: item for item in report["scenarios"]}
        favorable = scenarios["favorable_high_pairing"]
        self.assertEqual(favorable["role"], "sensitivity_only")
        self.assertTrue(favorable["results"]["both_endpoints"]["both_marginal_targets_met"])

    def test_checked_in_result_is_current(self) -> None:
        checked_in = json.loads((HERE / "power-analysis.json").read_text(encoding="utf-8"))
        self.assertEqual(checked_in, POWER.build_report())


if __name__ == "__main__":
    unittest.main()
