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
    @staticmethod
    def frozen_report() -> dict:
        report = POWER.build_report()
        alternatives = {
            "elapsed_time": ("ratio_true", 0.85),
            "normalized_cost": ("ratio_true", 0.80),
            "code_quality": ("difference_true", 0.10),
        }
        for name, (key, value) in alternatives.items():
            endpoint = report["co_primary_endpoints"][name]
            endpoint["claim_floor"]["status"] = "frozen_approved"
            endpoint["planning_alternative"] = {
                key: value,
                "status": "frozen_approved",
            }
            endpoint["paired_task_sd"] = 0.20
            endpoint["marginal_power"] = 0.81
            endpoint["status"] = "evaluated"
        report["joint_iut_power"].update(
            {
                "intersection_union_success_probability": 0.81,
                "status": "evaluated",
            }
        )
        report["design_readiness"].update(
            {
                "power_sized_development_task_count": 24,
                "power_sized_confirmatory_task_count": 24,
                "provisional_design_power_defensible": True,
            }
        )
        derived = POWER.recompute_power_decision(report)
        report["status"] = derived["status"]
        report["decision"].update(
            {"passed": derived["passed"], "reason": derived["reason"]}
        )
        return report

    def test_design_inputs_match_candidate_preregistration(self) -> None:
        protocol = json.loads((HERE / "preregistration.json").read_text(encoding="utf-8"))
        design = protocol["agent_design"]
        self.assertEqual(POWER.TASKS, design["tasks"])
        self.assertEqual(POWER.REPETITIONS, design["repetitions_per_treatment"])
        self.assertEqual(POWER.PRIMARY_TREATMENTS, len(design["primary_treatments"]))
        self.assertEqual(POWER.REQUESTED_CELLS, design["requested_cells"])
        self.assertEqual(
            POWER.MAXIMUM_AGENT_INVOCATIONS, design["maximum_agent_invocations"]
        )
        self.assertEqual(POWER.TARGET_POWER, design["power"]["target"])
        inputs = POWER.build_report()["protocol_inputs"]
        self.assertEqual(inputs["maximum_agent_invocations"], inputs["requested_cells"])
        self.assertEqual(inputs["agent_retry_limit"], 0)
        self.assertEqual(inputs["replacement_cell_limit"], 0)

    def test_report_supports_all_three_co_primary_endpoints(self) -> None:
        report = POWER.build_report()
        self.assertEqual(report["schema_version"], 3)
        self.assertEqual(
            set(report["co_primary_endpoints"]),
            {"elapsed_time", "normalized_cost", "code_quality"},
        )
        self.assertEqual(report["co_primary_endpoints"]["elapsed_time"]["claim_floor"]["ratio_max"], 0.9)
        self.assertEqual(report["co_primary_endpoints"]["normalized_cost"]["claim_floor"]["ratio_max"], 0.88)
        self.assertEqual(report["co_primary_endpoints"]["code_quality"]["claim_floor"]["difference_min"], 0.05)
        self.assertTrue(
            all(
                endpoint["planning_alternative"]
                == {
                    ("difference_true" if name == "code_quality" else "ratio_true"): None,
                    "status": "pending_owner_approval",
                }
                for name, endpoint in report["co_primary_endpoints"].items()
            )
        )

    def test_uncalibrated_design_fails_closed_without_inventing_power(self) -> None:
        report = POWER.build_report()
        self.assertEqual(report["status"], "pending_uncalibrated")
        self.assertFalse(report["decision"]["passed"])
        for endpoint in report["co_primary_endpoints"].values():
            self.assertIsNone(endpoint["paired_task_sd"])
            self.assertIsNone(endpoint["marginal_power"])
            self.assertEqual(endpoint["claim_floor"]["status"], "provisional")
        self.assertIsNone(
            report["joint_iut_power"]["intersection_union_success_probability"]
        )
        self.assertFalse(report["empirical_variance_used_in_confirmatory_decision"])

    def test_joint_method_is_intersection_union_not_three_separate_claims(self) -> None:
        report = POWER.build_report()
        self.assertIn("intersection-union", report["method"]["joint_rule"])
        self.assertIn("all three", report["method"]["joint_rule"])
        self.assertIn("joint success probability must each meet 0.80", report["method"]["planning_rule"])
        self.assertEqual(report["protocol_inputs"]["cluster_unit"], "task")
        self.assertEqual(report["protocol_inputs"]["attempt_policy"], "all_executed_attempts")

    def test_no_numeric_power_sized_task_count_is_claimed_before_calibration(self) -> None:
        report = POWER.build_report()
        readiness = report["design_readiness"]
        self.assertFalse(readiness["provisional_design_power_defensible"])
        self.assertIsNone(readiness["power_sized_development_task_count"])
        self.assertIsNone(readiness["power_sized_confirmatory_task_count"])
        self.assertTrue(
            report["calibration_requirements"][
                "minimum_is_calibration_floor_not_power_sized_design"
            ]
        )

    def test_legacy_sensitivity_helper_cannot_reintroduce_call_reserve(self) -> None:
        legacy = POWER._design_power(POWER.SCENARIOS[1], POWER.TASKS, POWER.REPETITIONS)
        self.assertEqual(legacy["requested_cells"], 288)
        self.assertEqual(
            legacy["maximum_agent_invocations_no_retries_or_replacements"],
            legacy["requested_cells"],
        )
        self.assertNotIn("maximum_calls_with_10_percent_reserve", legacy)

    def test_pending_design_rejects_nonnull_power_sized_count(self) -> None:
        report = POWER.build_report()
        report["design_readiness"]["power_sized_confirmatory_task_count"] = 24
        with self.assertRaisesRegex(ValueError, "pending power design.*null"):
            POWER.recompute_power_decision(report)

    def test_recomputed_gate_rejects_point_79_and_point_01_despite_tampered_pass(self) -> None:
        report = self.frozen_report()
        report["co_primary_endpoints"]["normalized_cost"]["marginal_power"] = 0.79
        report["joint_iut_power"]["intersection_union_success_probability"] = 0.01
        report["design_readiness"]["provisional_design_power_defensible"] = False
        report["decision"]["passed"] = True
        report["status"] = "pass"
        derived = POWER.recompute_power_decision(report)
        self.assertFalse(derived["passed"])
        self.assertEqual(derived["status"], "fail_underpowered")
        self.assertEqual(
            POWER.validate_power_report(report),
            [
                "power decision.passed does not match recomputed marginal/joint gate",
                "power status does not match recomputed marginal/joint gate",
            ],
        )

    def test_planning_alternative_must_be_strictly_better_than_claim_floor(self) -> None:
        report = self.frozen_report()
        report["co_primary_endpoints"]["elapsed_time"]["planning_alternative"][
            "ratio_true"
        ] = 0.90
        with self.assertRaisesRegex(ValueError, "strictly below"):
            POWER.recompute_power_decision(report)

    def test_power_target_cannot_be_lowered_to_make_point_75_pass(self) -> None:
        report = self.frozen_report()
        report["protocol_inputs"]["power_target"] = 0.70
        for endpoint in report["co_primary_endpoints"].values():
            endpoint["marginal_power"] = 0.75
        report["joint_iut_power"]["intersection_union_success_probability"] = 0.75
        with self.assertRaisesRegex(ValueError, "must remain frozen at 0.80"):
            POWER.recompute_power_decision(report)

    def test_power_sized_confirmatory_count_must_equal_protocol_tasks(self) -> None:
        report = self.frozen_report()
        report["design_readiness"]["power_sized_confirmatory_task_count"] = 2
        with self.assertRaisesRegex(ValueError, "must equal the protocol task count"):
            POWER.recompute_power_decision(report)

    def test_power_sized_development_count_must_meet_calibration_minimum(self) -> None:
        report = self.frozen_report()
        report["design_readiness"]["power_sized_development_task_count"] = 11
        with self.assertRaisesRegex(ValueError, "below the calibration minimum"):
            POWER.recompute_power_decision(report)

    def test_powered_design_requires_frozen_claim_floors(self) -> None:
        report = self.frozen_report()
        report["co_primary_endpoints"]["elapsed_time"]["claim_floor"][
            "status"
        ] = "provisional"
        with self.assertRaisesRegex(
            ValueError, "claim floor must be owner-approved and frozen"
        ):
            POWER.recompute_power_decision(report)

    def test_quality_alternative_must_be_finite_and_within_score_domain(self) -> None:
        for value, message in (
            (math.inf, "must be finite"),
            (2.0, r"must be within \[-1,1\]"),
            (0.05, "must be strictly above its claim floor"),
        ):
            with self.subTest(value=value):
                report = self.frozen_report()
                report["co_primary_endpoints"]["code_quality"][
                    "planning_alternative"
                ]["difference_true"] = value
                with self.assertRaisesRegex(ValueError, message):
                    POWER.recompute_power_decision(report)

    def test_power_readiness_arithmetic_must_match_protocol_inputs(self) -> None:
        report = self.frozen_report()
        report["design_readiness"]["provisional_requested_cells"] = 1
        with self.assertRaisesRegex(
            ValueError, "power readiness arithmetic mismatch: provisional_requested_cells"
        ):
            POWER.recompute_power_decision(report)

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
