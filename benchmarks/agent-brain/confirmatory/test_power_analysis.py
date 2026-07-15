from __future__ import annotations

import importlib.util
import json
import math
import pathlib
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
