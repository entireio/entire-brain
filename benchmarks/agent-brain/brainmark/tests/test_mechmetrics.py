"""mechmetrics against three HAND-LABELLED stream fixtures.

The expected counts below were derived by reading the fixtures by hand, not by
running the code. If the implementation changes the definition of a locate call,
these numbers must be re-derived by hand -- never by pasting in what the code
now prints. The fixtures live in tests/fixtures/ and are readable JSONL.
"""

from __future__ import annotations

import pathlib
import sys
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2]))

from brainmark import mechmetrics  # noqa: E402

FIXTURES = pathlib.Path(__file__).resolve().parent / "fixtures"

# pair (fixture, hand-labelled locate_calls_pre_edit, hand-labelled no_edit)
HAND_LABELS = [
    # Glob, Grep, Read, Bash(rg), Read before the first Edit. TodoWrite and
    # `npm test` are not locate; the Read after the Edit is past the cutoff.
    ("stream_locate_heavy.jsonl", 5, False),
    # One Read, then MultiEdit.
    ("stream_memory_assisted.jsonl", 1, False),
    # No edit ever happens, so the whole session counts: Grep, Read,
    # Bash(git grep), Bash(find). `git log` is NOT a locate call.
    ("stream_no_edit.jsonl", 4, True),
]


class HandLabelledFixtureTest(unittest.TestCase):
    def test_locate_calls_pre_edit_matches_hand_labels(self):
        for name, expected, expected_no_edit in HAND_LABELS:
            with self.subTest(fixture=name):
                metrics = mechmetrics.session_metrics(FIXTURES / name)
                self.assertEqual(
                    metrics["locate_calls_pre_edit"], expected,
                    f"{name}: hand label says {expected}",
                )
                self.assertEqual(metrics["no_edit"], expected_no_edit)

    def test_no_edit_session_counts_the_whole_session(self):
        metrics = mechmetrics.session_metrics(FIXTURES / "stream_no_edit.jsonl")
        self.assertTrue(metrics["no_edit"])
        self.assertIsNone(metrics["first_edit_call_index"])
        self.assertEqual(metrics["locate_calls_pre_edit"], metrics["locate_calls_total"])
        self.assertEqual(metrics["edit_calls_total"], 0)

    def test_cutoff_is_strict(self):
        metrics = mechmetrics.session_metrics(FIXTURES / "stream_locate_heavy.jsonl")
        # Index is over TOOL CALLS, not stream events:
        # 0 Glob, 1 Grep, 2 TodoWrite, 3 Read, 4 Bash(rg), 5 Bash(npm test),
        # 6 Read, 7 Edit  -> first edit at tool-call index 7.
        self.assertEqual(metrics["first_edit_call_index"], 7)
        self.assertEqual(metrics["locate_calls_pre_edit"], 5)
        # 6 locate calls exist overall; the 6th is after the edit.
        self.assertEqual(metrics["locate_calls_total"], 6)

    def test_secondary_metrics_are_read(self):
        metrics = mechmetrics.session_metrics(FIXTURES / "stream_locate_heavy.jsonl")
        self.assertEqual(metrics["num_turns"], 8)
        self.assertEqual(metrics["duration_ms"], 120000)
        self.assertAlmostEqual(metrics["total_cost_usd"], 0.42)
        self.assertEqual(metrics["subtype"], "success")
        self.assertEqual(metrics["tokens"]["source"], "modelUsage")
        self.assertEqual(metrics["tokens"]["total_tokens"], 1200 + 800 + 40000 + 5000)


class ClassificationTest(unittest.TestCase):
    def test_bash_allowlist(self):
        locate = ["rg foo", "grep -n x", "egrep x", "git grep x", "find . -name y",
                  "ls -la", "cat f", "head -5 f", "tail f", "ag pattern",
                  "  grep leading-space"]
        for command in locate:
            with self.subTest(command=command):
                self.assertEqual(mechmetrics.classify_tool("Bash", {"command": command}), "locate")

    def test_bash_non_locate(self):
        for command in ["git log", "git diff", "npm test", "python x.py", "make",
                        "cargo build", "grepfoo bar", "catalog"]:
            with self.subTest(command=command):
                self.assertEqual(mechmetrics.classify_tool("Bash", {"command": command}), "other")

    def test_tool_names(self):
        for name in ("Read", "Grep", "Glob"):
            self.assertEqual(mechmetrics.classify_tool(name, {}), "locate")
        for name in ("Edit", "Write", "MultiEdit", "NotebookEdit"):
            self.assertEqual(mechmetrics.classify_tool(name, {}), "edit")
        for name in ("TodoWrite", "WebFetch", "Task"):
            self.assertEqual(mechmetrics.classify_tool(name, {}), "other")

    def test_ratio_offset_makes_zero_finite(self):
        self.assertEqual(mechmetrics.pair_ratio(0, 0), 1.0)
        self.assertEqual(mechmetrics.pair_ratio(1, 0), 2.0)
        self.assertEqual(mechmetrics.pair_ratio(0, 1), 0.5)


class RobustnessTest(unittest.TestCase):
    def test_truncated_final_line_is_survivable(self):
        lines = (FIXTURES / "stream_locate_heavy.jsonl").read_text(
            encoding="utf-8").splitlines()
        truncated = lines[:-1] + ['{"type":"result","subtype":"suc']
        metrics = mechmetrics.session_metrics(truncated)
        self.assertEqual(metrics["locate_calls_pre_edit"], 5)
        self.assertFalse(metrics["has_result_event"])

    def test_tool_results_are_not_double_counted(self):
        """A user-side tool_result echo must not inflate the count."""
        import json

        events = [
            {"type": "assistant", "message": {"content": [
                {"type": "tool_use", "id": "a", "name": "Read", "input": {"file_path": "x"}}]}},
            {"type": "user", "message": {"content": [
                {"type": "tool_result", "tool_use_id": "a", "content": "..."}]}},
        ]
        metrics = mechmetrics.session_metrics([json.dumps(e) for e in events])
        self.assertEqual(metrics["locate_calls_pre_edit"], 1)
        self.assertEqual(metrics["tool_calls_total"], 1)


if __name__ == "__main__":
    unittest.main()
