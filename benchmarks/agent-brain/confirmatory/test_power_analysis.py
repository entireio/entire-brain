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


class PowerAnalysisV3Test(unittest.TestCase):
    def test_design_inputs_match_candidate_preregistration(self) -> None:
        protocol = json.loads((HERE / "preregistration.json").read_text(encoding="utf-8"))
        design = protocol["agent_design"]
        self.assertEqual(POWER.TASKS, design["tasks"])
        self.assertEqual(POWER.REPETITIONS, design["repetitions_per_treatment"])
        self.assertEqual(POWER.PRIMARY_TREATMENTS, len(design["primary_treatments"]))
        self.assertEqual(POWER.REQUESTED_CELLS, design["requested_cells"])
        self.assertEqual(POWER.TARGET_POWER, design["power"]["target"])

    def test_report_supports_all_three_co_primary_endpoints(self) -> None:
        report = POWER.build_report()
        self.assertEqual(report["schema_version"], 3)
        self.assertEqual(
            set(report["co_primary_endpoints"]),
            {"elapsed_time", "normalized_cost", "code_quality"},
        )
        self.assertEqual(report["co_primary_endpoints"]["elapsed_time"]["practical_floor"]["ratio_max"], 0.9)
        self.assertEqual(report["co_primary_endpoints"]["normalized_cost"]["practical_floor"]["ratio_max"], 0.88)
        self.assertEqual(report["co_primary_endpoints"]["code_quality"]["practical_floor"]["difference_min"], 0.05)

    def test_uncalibrated_design_fails_closed_without_inventing_power(self) -> None:
        report = POWER.build_report()
        self.assertEqual(report["status"], "pending_uncalibrated")
        self.assertFalse(report["decision"]["passed"])
        for endpoint in report["co_primary_endpoints"].values():
            self.assertIsNone(endpoint["paired_task_sd"])
            self.assertIsNone(endpoint["marginal_power"])
            self.assertEqual(endpoint["practical_floor"]["status"], "provisional")
        self.assertFalse(report["empirical_variance_used_in_confirmatory_decision"])

    def test_joint_method_is_intersection_union_not_three_separate_claims(self) -> None:
        report = POWER.build_report()
        self.assertIn("intersection-union", report["method"]["joint_rule"])
        self.assertIn("all three", report["method"]["joint_rule"])
        self.assertEqual(report["protocol_inputs"]["cluster_unit"], "task")
        self.assertEqual(report["protocol_inputs"]["attempt_policy"], "all_executed_attempts")

    def test_legacy_exploratory_calibration_remains_hashed_and_quarantined(self) -> None:
        report = POWER.build_report()
        calibration = report["exploratory_calibration"]
        self.assertEqual(calibration["eligibility"], "exploratory_only_excluded_from_confirmatory_decision")
        self.assertFalse(calibration["confirmatory_assumption_source"])
        self.assertTrue(calibration["pooled_estimate_prohibited"])
        self.assertEqual(calibration["paired_task_cluster_instances"], 14)
        self.assertEqual(calibration["unique_task_ids_across_sources"], 12)

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

    def test_existing_screening_helpers_still_behave_monotonically(self) -> None:
        effect = abs(math.log(0.88))
        low = POWER.normal_two_sided_power(effect, 0.30 / math.sqrt(24), 0.05)
        high = POWER.normal_two_sided_power(effect, 0.30 / math.sqrt(96), 0.05)
        self.assertGreater(high, low)

    def test_checked_in_result_is_current(self) -> None:
        checked_in = json.loads((HERE / "power-analysis.json").read_text(encoding="utf-8"))
        self.assertEqual(checked_in, POWER.build_report())


if __name__ == "__main__":
    unittest.main()
