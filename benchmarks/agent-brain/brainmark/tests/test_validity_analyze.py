"""validity/analyze_validity.py: Spearman rho, label validation, agreement,
and the end-to-end join, all on synthetic labels (no real sessions needed)."""

from __future__ import annotations

import json
import pathlib
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2]))

from brainmark import _harness  # noqa: E402
from brainmark.validity import analyze_validity as av  # noqa: E402


class SpearmanRhoTest(unittest.TestCase):
    def test_perfect_monotonic_increasing_is_one(self):
        result = av.spearman_rho([1, 2, 3, 4, 5], [10, 20, 30, 40, 50])
        self.assertAlmostEqual(result["rho"], 1.0)

    def test_perfect_monotonic_decreasing_is_minus_one(self):
        result = av.spearman_rho([1, 2, 3, 4, 5], [50, 40, 30, 20, 10])
        self.assertAlmostEqual(result["rho"], -1.0)

    def test_hand_computed_value_with_ties(self):
        # x has a tie (2,2); ranks_x = [1, 2.5, 2.5, 4], ranks_y = [1,2,3,4].
        # cov=4.5, var_x=4.5, var_y=5.0 -> rho = 4.5/sqrt(22.5) = sqrt(0.9).
        result = av.spearman_rho([1, 2, 2, 3], [1, 2, 3, 4])
        self.assertAlmostEqual(result["rho"], 0.9 ** 0.5, places=9)
        self.assertEqual(result["n"], 4)

    def test_zero_variance_series_is_undefined_not_zero(self):
        result = av.spearman_rho([1, 1, 1], [1, 2, 3])
        self.assertIsNone(result["rho"])

    def test_mismatched_length_raises(self):
        with self.assertRaises(ValueError):
            av.spearman_rho([1, 2], [1])

    def test_too_few_points_raises(self):
        with self.assertRaises(ValueError):
            av.spearman_rho([1], [1])


class LabelValidationTest(unittest.TestCase):
    def _valid(self) -> dict:
        return {
            "rater": "alice",
            "sessions": {
                "s1": {"calls": [{"call_index": 0, "verdict": "yes"},
                                 {"call_index": 1, "verdict": "no"}]},
            },
        }

    def test_valid_payload_passes(self):
        av.validate_labels(self._valid())  # must not raise

    def test_missing_rater_raises(self):
        payload = self._valid()
        payload["rater"] = ""
        with self.assertRaises(ValueError):
            av.validate_labels(payload)

    def test_bad_verdict_raises(self):
        payload = self._valid()
        payload["sessions"]["s1"]["calls"][0]["verdict"] = "maybe"
        with self.assertRaises(ValueError):
            av.validate_labels(payload)

    def test_non_int_call_index_raises(self):
        payload = self._valid()
        payload["sessions"]["s1"]["calls"][0]["call_index"] = "0"
        with self.assertRaises(ValueError):
            av.validate_labels(payload)

    def test_empty_sessions_raises(self):
        payload = self._valid()
        payload["sessions"] = {}
        with self.assertRaises(ValueError):
            av.validate_labels(payload)


class YesCountTest(unittest.TestCase):
    def test_yes_count_ignores_no_and_unclear(self):
        entry = {"calls": [{"call_index": 0, "verdict": "yes"},
                           {"call_index": 1, "verdict": "no"},
                           {"call_index": 2, "verdict": "unclear"},
                           {"call_index": 3, "verdict": "yes"}]}
        self.assertEqual(av.session_yes_count(entry), 2)

    def test_yes_ratio_excludes_unclear_from_denominator(self):
        entry = {"calls": [{"call_index": 0, "verdict": "yes"},
                           {"call_index": 1, "verdict": "unclear"},
                           {"call_index": 2, "verdict": "unclear"}]}
        # denominator is yes+no = 1, not 3.
        self.assertEqual(av.session_yes_ratio(entry), 1.0)

    def test_yes_ratio_none_when_no_countable_calls(self):
        entry = {"calls": [{"call_index": 0, "verdict": "unclear"}]}
        self.assertIsNone(av.session_yes_ratio(entry))


class AgreementTest(unittest.TestCase):
    def test_percent_agreement_and_kappa_on_dually_labeled_calls(self):
        labels_a = {"rater": "alice", "sessions": {
            "s1": {"calls": [{"call_index": 0, "verdict": "yes"},
                             {"call_index": 1, "verdict": "no"}]},
        }}
        labels_b = {"rater": "bob", "sessions": {
            "s1": {"calls": [{"call_index": 0, "verdict": "yes"},
                             {"call_index": 1, "verdict": "yes"}]},  # disagreement
        }}
        result = av.per_rater_agreement(labels_a, labels_b)
        self.assertEqual(result["n"], 2)
        self.assertAlmostEqual(result["percent_agreement"], 0.5)
        self.assertIsInstance(result["kappa"], float)

    def test_no_overlap_reports_zero_n(self):
        labels_a = {"rater": "alice", "sessions": {"s1": {"calls": [{"call_index": 0, "verdict": "yes"}]}}}
        labels_b = {"rater": "bob", "sessions": {"s2": {"calls": [{"call_index": 0, "verdict": "no"}]}}}
        result = av.per_rater_agreement(labels_a, labels_b)
        self.assertEqual(result["n"], 0)
        self.assertIsNone(result["kappa"])


class EndToEndReportTest(unittest.TestCase):
    """Synthetic sessions with a machine stream + synthetic labels chosen so
    the joined rho is a known, verifiable value: yes_count == machine count
    exactly for every session (perfect construct validity by construction)."""

    def _make_session(self, root: pathlib.Path, pair_id: str, arm: str, n_locate: int) -> None:
        cell = root / pair_id / arm
        cell.mkdir(parents=True)
        events = [{"type": "system", "subtype": "init", "model": "stub"}]
        for i in range(n_locate):
            events.append({"type": "assistant", "message": {"content": [
                {"type": "tool_use", "id": f"t{i}", "name": "Read", "input": {"file_path": f"f{i}.py"}},
            ]}})
        events.append({"type": "assistant", "message": {"content": [
            {"type": "tool_use", "id": "edit", "name": "Edit",
             "input": {"file_path": "f0.py", "old_string": "a", "new_string": "b"}},
        ]}})
        events.append({"type": "result", "subtype": "success", "is_error": False,
                       "num_turns": n_locate + 1, "duration_ms": 1, "total_cost_usd": 0.01})
        (cell / "stream.jsonl").write_text(
            "\n".join(json.dumps(e) for e in events) + "\n", encoding="utf-8")

    def test_perfect_rho_when_labels_track_machine_count_exactly(self):
        with tempfile.TemporaryDirectory() as raw:
            results = pathlib.Path(raw) / "B"
            counts = {"p1": ("full_brain", 1), "p2": ("full_brain", 2), "p3": ("no_brain", 4)}
            unblind_sessions = {}
            labels_sessions = {}
            for pair_id, (arm, n) in counts.items():
                self._make_session(results, pair_id, arm, n)
                sid = f"sid-{pair_id}"
                unblind_sessions[sid] = {"pair_id": pair_id, "arm": arm}
                labels_sessions[sid] = {"calls": [
                    {"call_index": i, "verdict": "yes"} for i in range(n)
                ]}

            unblind_map = {"seed": 1, "sessions": unblind_sessions}
            labels = {"rater": "alice", "sessions": labels_sessions}

            report = av.build_report({"alice": labels}, unblind_map, results)
            self.assertEqual(report["n_sessions_by_rater"]["alice"], 3)
            self.assertAlmostEqual(report["primary_rho"], 1.0)
            self.assertFalse(report["validity_gate_fires"])

    def test_gate_fires_when_labels_are_uncorrelated_with_machine_count(self):
        with tempfile.TemporaryDirectory() as raw:
            results = pathlib.Path(raw) / "B"
            # machine counts increase 1,2,3,4,5 while intended yes-counts decrease
            # 5,4,3,2,1; capped by min(yes, n) per session this yields the
            # non-monotonic sequence [1,2,3,2,1] -> rho = 0.0, still < 0.5.
            unblind_sessions = {}
            labels_sessions = {}
            for i, pair_id in enumerate(["p1", "p2", "p3", "p4", "p5"]):
                n = i + 1
                self._make_session(results, pair_id, "full_brain", n)
                sid = f"sid-{pair_id}"
                unblind_sessions[sid] = {"pair_id": pair_id, "arm": "full_brain"}
                yes = 5 - i
                labels_sessions[sid] = {"calls": (
                    [{"call_index": j, "verdict": "yes"} for j in range(min(yes, n))]
                    + [{"call_index": j, "verdict": "no"} for j in range(min(yes, n), n)]
                )}
            unblind_map = {"seed": 1, "sessions": unblind_sessions}
            labels = {"rater": "alice", "sessions": labels_sessions}
            report = av.build_report({"alice": labels}, unblind_map, results)
            self.assertLess(report["primary_rho"], av.VALIDITY_GATE_RHO)
            self.assertTrue(report["validity_gate_fires"])

    def test_cli_end_to_end_writes_report_and_exits_nonzero_on_gate_fire(self):
        with tempfile.TemporaryDirectory() as raw:
            root = pathlib.Path(raw)
            results = root / "B"
            unblind_sessions, labels_sessions = {}, {}
            for i, pair_id in enumerate(["p1", "p2", "p3", "p4", "p5"]):
                n = i + 1
                self._make_session(results, pair_id, "full_brain", n)
                sid = f"sid-{pair_id}"
                unblind_sessions[sid] = {"pair_id": pair_id, "arm": "full_brain"}
                yes = 5 - i
                labels_sessions[sid] = {"calls": (
                    [{"call_index": j, "verdict": "yes"} for j in range(min(yes, n))]
                    + [{"call_index": j, "verdict": "no"} for j in range(min(yes, n), n)]
                )}
            unblind_path = root / "UNBLIND-MAP.json"
            unblind_path.write_text(_harness.pretty_json({"seed": 1, "sessions": unblind_sessions}),
                                    encoding="utf-8")
            labels_path = root / "alice.json"
            labels_path.write_text(_harness.pretty_json({"rater": "alice", "sessions": labels_sessions}),
                                   encoding="utf-8")
            out_path = root / "REPORT.json"

            rc = av.main(["--labels", str(labels_path), "--unblind-map", str(unblind_path),
                         "--results", str(results), "--out", str(out_path)])
            self.assertEqual(rc, 1)
            report = json.loads(out_path.read_text(encoding="utf-8"))
            self.assertTrue(report["validity_gate_fires"])


if __name__ == "__main__":
    unittest.main()
