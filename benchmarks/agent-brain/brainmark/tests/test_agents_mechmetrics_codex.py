"""mechmetrics against HAND-LABELLED **codex** stream fixtures.

Same rule as tests/test_mechmetrics.py: the expected counts below were derived
by reading the fixtures by hand, not by running the code. If the codex mapping
changes, re-derive them by hand -- never paste in what the code now prints.

MUTATION TARGET (a): break the codex locate-call mapping and these must fail.
Two independent ways to break it are covered explicitly:
  * dropping the `bash -lc` unwrap -> every codex shell call becomes OTHER;
  * dropping the item.started/item.completed dedup -> shell calls double-count
    and the pre-edit cutoff moves.
"""

from __future__ import annotations

import json
import pathlib
import sys
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2]))

from brainmark import mechmetrics  # noqa: E402

FIXTURES = pathlib.Path(__file__).resolve().parent / "fixtures"

# (fixture, locate_calls_pre_edit, no_edit, first_edit_call_index,
#  tool_calls_total, locate_calls_total, edit_calls_total, extended_pre_edit)
HAND_LABELS = [
    # codex_stream_locate_heavy: after collapsing item.started/item.completed
    # the call sequence is
    #   0 Bash(ls src)                     LOCATE
    #   1 Bash(rg -n 'renderWidget' src)   LOCATE
    #   2 Bash(cat src/widget.js)          LOCATE
    #   3 WebSearch                        other
    #   4 Bash(sed -n '1,80p' ...)         other under the FROZEN allowlist,
    #                                      LOCATE only under the extended one
    #   5 Bash(npm test -- widget)         other
    #   6 file_change                      EDIT  <- the cutoff
    #   7 Bash(grep -n ...)                LOCATE but past the cutoff
    #   8 Bash(npm test)                   other
    # reasoning/agent_message items are not tool calls at all.
    ("codex_stream_locate_heavy.jsonl", 3, False, 6, 9, 4, 1, 4),
    # codex_stream_no_edit: git grep, find, head are LOCATE; `git log` is not;
    # an mcp_tool_call is not. No file_change ever -> the whole session counts.
    ("codex_stream_no_edit.jsonl", 3, True, None, 5, 3, 0, 3),
    # codex_stream_legacy_exec (legacy `msg` schema): rg LOCATE, python other,
    # patch_apply EDIT at call index 2, cat LOCATE after the cutoff. Each call
    # appears as a begin/end PAIR and must be counted once, and the `_end`
    # event -- which carries no command -- must not downgrade the label.
    ("codex_stream_legacy_exec.jsonl", 1, False, 2, 4, 2, 1, 1),
]


class CodexHandLabelTest(unittest.TestCase):
    def test_hand_labels(self):
        for (name, pre_edit, no_edit, first_edit, total, locate_total,
             edit_total, extended) in HAND_LABELS:
            with self.subTest(fixture=name):
                m = mechmetrics.session_metrics(FIXTURES / name)
                self.assertEqual(m["backend"], "codex", "backend auto-detection")
                self.assertEqual(m["locate_calls_pre_edit"], pre_edit,
                                 f"{name}: hand label says {pre_edit}")
                self.assertEqual(m["no_edit"], no_edit)
                self.assertEqual(m["first_edit_call_index"], first_edit)
                self.assertEqual(m["tool_calls_total"], total)
                self.assertEqual(m["locate_calls_total"], locate_total)
                self.assertEqual(m["edit_calls_total"], edit_total)
                self.assertEqual(m["locate_calls_pre_edit_extended"], extended)

    def test_started_and_completed_are_one_call(self):
        """item_1 and item_3 each appear twice in the fixture."""
        calls, _ = mechmetrics.extract_tool_calls(
            FIXTURES / "codex_stream_locate_heavy.jsonl")
        ids = [c["id"] for c in calls]
        self.assertEqual(len(ids), len(set(ids)), f"duplicated items: {ids}")
        self.assertEqual(ids.index("item_1"), 0, "dedup must keep the ORIGINAL position")

    def test_bash_wrapper_is_unwrapped(self):
        calls, _ = mechmetrics.extract_tool_calls(
            FIXTURES / "codex_stream_locate_heavy.jsonl")
        self.assertEqual(calls[0]["command"], "ls src")
        self.assertEqual(calls[1]["kind"], "locate")

    def test_extended_metric_is_secondary_not_primary(self):
        """The frozen allowlist must NOT have quietly grown a `sed` entry."""
        self.assertEqual(mechmetrics.classify_command("sed -n '1,5p' f"), "other")
        self.assertEqual(
            mechmetrics.classify_command("sed -n '1,5p' f", extended=True), "locate")
        # `sed -i` writes; it is not a read even under the extended allowlist.
        self.assertEqual(
            mechmetrics.classify_command("sed -i 's/a/b/' f", extended=True), "other")

    def test_codex_tokens_treat_cache_as_a_subset(self):
        m = mechmetrics.session_metrics(FIXTURES / "codex_stream_locate_heavy.jsonl")
        tokens = m["tokens"]
        self.assertTrue(tokens["cache_read_is_subset_of_input"])
        # 42000 already CONTAINS the 38000 cached tokens; adding them would
        # inflate every codex session by the cache.
        self.assertEqual(tokens["total_tokens"], 42000 + 900)
        self.assertIsNone(m["total_cost_usd"], "codex reports no provider cost")


class EffectiveShellCommandTest(unittest.TestCase):
    def test_shell_wrappers(self):
        for argv, expected in [
            (["bash", "-lc", "rg foo"], "rg foo"),
            (["/bin/bash", "-lc", "grep x"], "grep x"),
            (["sh", "-c", "ls"], "ls"),
            ("cat file", "cat file"),
        ]:
            with self.subTest(argv=argv):
                self.assertEqual(mechmetrics.effective_shell_command(argv), expected)

    def test_non_wrapper_argv_is_joined(self):
        self.assertEqual(
            mechmetrics.effective_shell_command(["rg", "-n", "foo bar", "src"]),
            "rg -n 'foo bar' src",
        )

    def test_unknown_shape_is_none(self):
        self.assertIsNone(mechmetrics.effective_shell_command(None))
        self.assertIsNone(mechmetrics.effective_shell_command({"command": "x"}))


class BackendDetectionTest(unittest.TestCase):
    def test_claude_fixtures_still_detect_as_claude(self):
        for name in ("stream_locate_heavy.jsonl", "stream_memory_assisted.jsonl",
                     "stream_no_edit.jsonl"):
            with self.subTest(fixture=name):
                m = mechmetrics.session_metrics(FIXTURES / name)
                self.assertEqual(m["backend"], "claude")

    def test_explicit_backend_overrides_detection(self):
        """A codex stream read as claude must yield zero calls, not garbage."""
        m = mechmetrics.session_metrics(
            FIXTURES / "codex_stream_locate_heavy.jsonl", backend="claude")
        self.assertEqual(m["tool_calls_total"], 0)

    def test_empty_stream_is_unknown_not_a_crash(self):
        m = mechmetrics.session_metrics([])
        self.assertEqual(m["backend"], "unknown")
        self.assertEqual(m["locate_calls_pre_edit"], 0)


class CodexRobustnessTest(unittest.TestCase):
    def test_truncated_tail_survives(self):
        lines = (FIXTURES / "codex_stream_locate_heavy.jsonl").read_text(
            encoding="utf-8").splitlines()
        truncated = lines[:-1] + ['{"type":"turn.completed","usage":{"input_to']
        m = mechmetrics.session_metrics(truncated)
        self.assertEqual(m["locate_calls_pre_edit"], 3)
        self.assertFalse(m["has_result_event"])

    def test_file_change_without_an_id_still_counts_once(self):
        events = [
            {"type": "item.completed",
             "item": {"item_type": "command_execution", "command": ["bash", "-lc", "rg x"]}},
            {"type": "item.completed",
             "item": {"item_type": "file_change", "changes": [{"path": "a", "kind": "update"}]}},
        ]
        m = mechmetrics.session_metrics([json.dumps(e) for e in events])
        self.assertEqual(m["locate_calls_pre_edit"], 1)
        self.assertEqual(m["edit_calls_total"], 1)


if __name__ == "__main__":
    unittest.main()
