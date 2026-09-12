"""power_analysis against synthetic data with HAND-COMPUTED answers.

Every expected number below was derived on paper from the formulas in
power_analysis.py's docstring, not by running the code. The two variance
designs are purely additive (y = pair + arm + rep, no noise) with values chosen
as integer multiples of ln 2, so the sums of squares are exact rationals times
(ln 2)^2 and the components can be written down.
"""

from __future__ import annotations

import math
import pathlib
import sys
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2]))

from brainmark import power_analysis as pa  # noqa: E402

L = math.log(2)
L2 = L * L


def counts_for(multiples):
    """value = k*ln2  <=>  count = 2^k - 1 (because the metric is log(count+1))."""
    return [2 ** k - 1 for k in multiples]


class RuleTest(unittest.TestCase):
    def test_delta_is_the_frozen_log_of_a_15_percent_reduction(self):
        self.assertAlmostEqual(pa.target_delta(), 0.16251892949777494, places=12)
        self.assertAlmostEqual(pa.target_delta(0.5), math.log(2), places=12)

    def test_required_n_hand_computed(self):
        # z_{0.975} + z_{0.80} = 2.801585218112968 ; delta = 0.16251892949777494
        # n_exact = (2.801585218112968 * 0.5 / 0.16251892949777494)^2 = 74.2916112...
        out = pa.required_n(0.5)
        self.assertAlmostEqual(out["z_sum"], 2.801585218112968, places=6)
        self.assertAlmostEqual(out["n_exact"], 74.2916, places=3)
        self.assertEqual(out["n"], 75)
        self.assertEqual(out["n_t_corrected"], 77)

    def test_required_n_scales_with_the_square_of_sd(self):
        """Doubling sd must quadruple n -- the sanity check the rule lives or dies by."""
        self.assertEqual(pa.required_n(0.25)["n"], 19)      # 74.2916/4 = 18.57 -> 19
        self.assertEqual(pa.required_n(1.0)["n"], 298)      # 74.2916*4 = 297.17 -> 298

    def test_power_at_the_required_n_is_at_least_eighty_percent(self):
        for sd in (0.2, 0.5, 0.9):
            with self.subTest(sd=sd):
                n = pa.required_n(sd)["n"]
                self.assertGreaterEqual(pa.achieved_power(sd, n), 0.80)
                self.assertLess(pa.achieved_power(sd, n - 1), 0.8001)

    def test_sealed_cap_flags_underpowered_rather_than_shrinking_the_claim(self):
        out = pa.required_n(0.5, sealed_n=30)
        self.assertEqual(out["n"], 75)
        self.assertEqual(out["n_capped"], 30)
        self.assertTrue(out["underpowered"])
        self.assertLess(out["power_at_sealed_n"], 0.80)

        ok = pa.required_n(0.5, sealed_n=200)
        self.assertFalse(ok["underpowered"])
        self.assertEqual(ok["n_capped"], 75)

    def test_mde_curve_is_monotone_and_crosses_the_target_at_required_n(self):
        sd = 0.5
        n = pa.required_n(sd)["n"]
        curve = pa.mde_curve(sd, [n - 1, n, n + 1, 4 * n])
        pcts = [row["mde_reduction_pct"] for row in curve]
        self.assertEqual(pcts, sorted(pcts, reverse=True), "MDE must fall as n grows")
        self.assertFalse(curve[0]["meets_target"])
        self.assertTrue(curve[1]["meets_target"])
        # At 4n the detectable reduction is roughly half of 15%.
        self.assertLess(curve[3]["mde_reduction_pct"], 9.0)

    def test_zero_variance_is_refused(self):
        with self.assertRaises(ValueError):
            pa.required_n(0.0)


class LogRatioTest(unittest.TestCase):
    def test_pair_ratio_uses_the_plus_one_offset(self):
        obs = [
            {"pair_id": "p1", "arm": "full_brain", "rep": 0, "locate_calls_pre_edit": 3},
            {"pair_id": "p1", "arm": "no_brain", "rep": 0, "locate_calls_pre_edit": 7},
        ]
        ratios = pa.pair_log_ratios(obs, "full_brain", "no_brain")
        self.assertAlmostEqual(ratios["p1"], math.log(4 / 8), places=12)

    def test_reps_are_averaged_within_a_pair_not_pooled_across(self):
        """Two reps of the same pair are ONE unit of analysis."""
        obs = []
        for rep, (t, c) in enumerate([(3, 7), (1, 3)]):
            obs += [
                {"pair_id": "p1", "arm": "t", "rep": rep, "locate_calls_pre_edit": t},
                {"pair_id": "p1", "arm": "c", "rep": rep, "locate_calls_pre_edit": c},
            ]
        ratios = pa.pair_log_ratios(obs, "t", "c")
        self.assertEqual(len(ratios), 1)
        self.assertAlmostEqual(
            ratios["p1"], (math.log(4 / 8) + math.log(2 / 4)) / 2, places=12)

    def test_zero_counts_are_finite(self):
        obs = [
            {"pair_id": "p", "arm": "t", "rep": 0, "locate_calls_pre_edit": 0},
            {"pair_id": "p", "arm": "c", "rep": 0, "locate_calls_pre_edit": 0},
        ]
        self.assertEqual(pa.pair_log_ratios(obs, "t", "c")["p"], 0.0)


class VarianceDecompositionTest(unittest.TestCase):
    """Purely additive designs -> residual is EXACTLY zero and every component
    is a hand-computed multiple of (ln 2)^2."""

    def test_two_by_two_single_rep(self):
        # values (in units of ln2):  p1a1=0 p1a2=1 p2a1=2 p2a2=3
        # ss_pair = 2*((-1)^2+(1)^2) = 4 ; ss_arm = 2*((-.5)^2+(.5)^2) = 1
        # ss_total = 2.25+.25+.25+2.25 = 5 ; ss_resid = 0 ; df_resid = 1
        # pair = (4-0)/(A*R=2) = 2 ; arm = (1-0)/(P*R=2) = 0.5
        design = {("p1", "a1"): 0, ("p1", "a2"): 1, ("p2", "a1"): 2, ("p2", "a2"): 3}
        obs = [
            {"pair_id": p, "arm": a, "rep": 0, "locate_calls_pre_edit": 2 ** k - 1}
            for (p, a), k in design.items()
        ]
        out = pa.variance_components(obs)
        self.assertEqual((out["n_pairs"], out["n_arms"], out["n_reps"]), (2, 2, 1))
        self.assertAlmostEqual(out["components"]["pair"], 2 * L2, places=7)
        self.assertAlmostEqual(out["components"]["arm"], 0.5 * L2, places=7)
        self.assertAlmostEqual(out["components"]["rep"], 0.0, places=12)
        self.assertAlmostEqual(out["components"]["residual"], 0.0, places=12)

    def test_two_by_two_by_two_recovers_a_rep_component(self):
        # y = p + a + r with p in {0,2}, a in {0,1}, r in {0,4} (units of ln2)
        # ss_pair=8, ss_arm=2, ss_rep=32, ss_total=42 -> ss_resid=0, df_resid=4
        # pair=8/4=2 ; arm=2/4=0.5 ; rep=32/4=8 ; residual=0
        obs = []
        for p, pv in (("p1", 0), ("p2", 2)):
            for a, av in (("a1", 0), ("a2", 1)):
                for r, rv in ((0, 0), (1, 4)):
                    obs.append({"pair_id": p, "arm": a, "rep": r,
                                "locate_calls_pre_edit": 2 ** (pv + av + rv) - 1})
        out = pa.variance_components(obs)
        self.assertAlmostEqual(out["components"]["pair"], 2 * L2, places=7)
        self.assertAlmostEqual(out["components"]["arm"], 0.5 * L2, places=7)
        self.assertAlmostEqual(out["components"]["rep"], 8 * L2, places=7)
        self.assertAlmostEqual(out["components"]["residual"], 0.0, places=10)
        self.assertAlmostEqual(sum(out["share"].values()), 1.0, places=9)

    def test_unbalanced_design_is_refused_not_approximated(self):
        obs = [
            {"pair_id": "p1", "arm": "a1", "rep": 0, "locate_calls_pre_edit": 1},
            {"pair_id": "p1", "arm": "a2", "rep": 0, "locate_calls_pre_edit": 3},
            {"pair_id": "p2", "arm": "a1", "rep": 0, "locate_calls_pre_edit": 7},
        ]
        with self.assertRaises(ValueError) as ctx:
            pa.variance_components(obs)
        self.assertIn("unbalanced", str(ctx.exception))

    def test_duplicate_observations_are_refused(self):
        row = {"pair_id": "p", "arm": "a", "rep": 0, "locate_calls_pre_edit": 1}
        with self.assertRaises(ValueError):
            pa.variance_components([row, dict(row)])

    def test_negative_components_are_clamped_and_flagged(self):
        """Pure noise across arms -> the arm component estimate can go negative."""
        design = {("p1", "a1"): 0, ("p1", "a2"): 3, ("p2", "a1"): 3, ("p2", "a2"): 0}
        obs = [
            {"pair_id": p, "arm": a, "rep": 0, "locate_calls_pre_edit": 2 ** k - 1}
            for (p, a), k in design.items()
        ]
        out = pa.variance_components(obs)
        self.assertIn("arm", out["clamped_to_zero"])
        self.assertLess(out["components_raw"]["arm"], 0)
        self.assertEqual(out["components"]["arm"], 0.0)

    def test_sd_with_reps_halves_the_variance_at_two_reps(self):
        components = {"components": {"residual": 0.32, "pair": 1.0, "arm": 0.0, "rep": 0.0}}
        one = pa.sd_with_reps(components, 1)
        two = pa.sd_with_reps(components, 2)
        self.assertAlmostEqual(one, math.sqrt(2 * 0.32), places=12)
        self.assertAlmostEqual(two, one / math.sqrt(2), places=12)


class IngestTest(unittest.TestCase):
    def test_observation_list_shape(self):
        rows = pa.load_observations(
            [{"pair_id": "p", "arm": "a", "rep": 1, "locate_calls_pre_edit": 4}])
        self.assertEqual(rows, [{"pair_id": "p", "arm": "a", "rep": 1,
                                 "locate_calls_pre_edit": 4}])

    def test_table_shape(self):
        rows = pa.load_observations({"table": {
            "p1": {"full_brain": {"mech": {"locate_calls_pre_edit": 2}},
                   "no_brain": {"mech": {"locate_calls_pre_edit": 5}}}}})
        self.assertEqual(len(rows), 2)
        self.assertEqual({r["arm"] for r in rows}, {"full_brain", "no_brain"})

    def test_unknown_shape_raises(self):
        with self.assertRaises(ValueError):
            pa.load_observations({"nope": 1})

    def test_results_tree_walk(self):
        import json
        import tempfile

        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            for rep in (0, 1):
                for arm, value in (("full_brain", 2), ("no_brain", 6)):
                    cell = root / f"rep{rep}" / "p1" / arm
                    cell.mkdir(parents=True)
                    (cell / "meta.json").write_text(json.dumps({
                        "pair_id": "p1", "arm": arm, "rep": rep,
                        "mechmetrics": {"locate_calls_pre_edit": value},
                    }), encoding="utf-8")
            rows = pa.observations_from_results(root)
        self.assertEqual(len(rows), 4)
        self.assertEqual({r["rep"] for r in rows}, {0, 1})


class AnalyzeTest(unittest.TestCase):
    def test_end_to_end_on_a_known_pilot(self):
        """10 pairs, treatment exactly one ln2 step below control on half of
        them and equal on the other half -> mean log-ratio = -ln2/2, and the sd
        is the sd of five 0s and five -ln2 values."""
        obs = []
        for i in range(10):
            drop = i % 2 == 0
            obs += [
                {"pair_id": f"p{i:02d}", "arm": "no_brain", "rep": 0,
                 "locate_calls_pre_edit": 7},
                {"pair_id": f"p{i:02d}", "arm": "full_brain", "rep": 0,
                 "locate_calls_pre_edit": 3 if drop else 7},
            ]
        out = pa.analyze(obs)
        self.assertEqual(out["n_pairs_with_both_arms"], 10)
        self.assertAlmostEqual(out["observed"]["mean_log_ratio"], -L / 2, places=5)
        expected_sd = math.sqrt(sum((v + L / 2) ** 2 for v in [0.0] * 5 + [-L] * 5) / 9)
        self.assertAlmostEqual(out["observed"]["sd_log_ratio"], expected_sd, places=5)
        self.assertEqual(out["required_n"]["n"], pa.required_n(expected_sd)["n"])
        self.assertIn("mde_curve", out)
        self.assertIn("variance_decomposition", out)

    def test_too_few_pairs_reports_an_error_not_a_number(self):
        obs = [
            {"pair_id": "p", "arm": "t", "rep": 0, "locate_calls_pre_edit": 1},
            {"pair_id": "p", "arm": "c", "rep": 0, "locate_calls_pre_edit": 2},
        ]
        out = pa.analyze(obs, "t", "c")
        self.assertIn("error", out)
        self.assertNotIn("required_n", out)


if __name__ == "__main__":
    unittest.main()
