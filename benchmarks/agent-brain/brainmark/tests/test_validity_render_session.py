"""validity/render_session.py: blinding, labeling-unit extraction, sampling."""

from __future__ import annotations

import json
import pathlib
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2]))

from brainmark import _harness  # noqa: E402
from brainmark.validity import render_session  # noqa: E402


PROMPT = (
    "You are working in a git checkout...\n\n"
    "A frozen memory packet is embedded...\n\n"
    "--- ISSUE ---\n"
    "The widget renderer drops the trailing newline.\n\n"
    "--- MEMORY ---\n"
    "<frozen-memory-packet>\n"
    '{"results":[{"source":"full_brain","text":"THIS MUST NEVER APPEAR IN A SHEET"}]}\n'
    "</frozen-memory-packet>\n"
)


def _stream_lines() -> list[str]:
    return [
        json.dumps({"type": "system", "subtype": "init", "model": "stub"}),
        json.dumps({"type": "assistant", "message": {"content": [
            {"type": "tool_use", "id": "t1", "name": "Grep", "input": {"pattern": "renderWidget", "path": "src"}},
        ]}}),
        json.dumps({"type": "user", "message": {"content": [
            {"type": "tool_result", "tool_use_id": "t1", "content": "src/widget.js:12: renderWidget()"},
        ]}}),
        json.dumps({"type": "assistant", "message": {"content": [
            {"type": "tool_use", "id": "t2", "name": "Read", "input": {"file_path": "src/widget.js"}},
        ]}}),
        json.dumps({"type": "assistant", "message": {"content": [
            {"type": "tool_use", "id": "t3", "name": "Edit",
             "input": {"file_path": "src/widget.js", "old_string": "a", "new_string": "b"}},
        ]}}),
        json.dumps({"type": "assistant", "message": {"content": [
            {"type": "tool_use", "id": "t4", "name": "Read", "input": {"file_path": "src/post-edit-should-not-count.js"}},
        ]}}),
        json.dumps({"type": "result", "subtype": "success", "is_error": False, "num_turns": 4,
                    "duration_ms": 100, "total_cost_usd": 0.02}),
    ]


class BlindingTest(unittest.TestCase):
    def test_extract_issue_text_never_includes_memory_section(self):
        issue = render_session.extract_issue_text(PROMPT)
        self.assertIn("trailing newline", issue)
        self.assertNotIn("NEVER APPEAR", issue)
        self.assertNotIn("full_brain", issue)
        self.assertNotIn("frozen-memory-packet", issue)

    def test_extract_issue_text_handles_missing_issue_header(self):
        self.assertEqual(render_session.extract_issue_text("no markers here"), "")

    def test_opaque_session_id_is_deterministic_and_does_not_contain_arm(self):
        sid = render_session.opaque_session_id(20260815, "pair-42", "full_brain")
        self.assertEqual(sid, render_session.opaque_session_id(20260815, "pair-42", "full_brain"))
        self.assertNotIn("full_brain", sid)
        self.assertNotIn("pair-42", sid)
        # a different arm on the SAME pair must yield a different id, or two
        # arms' sheets would collide on disk.
        self.assertNotEqual(sid, render_session.opaque_session_id(20260815, "pair-42", "no_brain"))

    def test_rendered_sheet_never_contains_arm_or_packet_bytes(self):
        with tempfile.TemporaryDirectory() as raw:
            root = pathlib.Path(raw)
            b_dir = root / "full_brain"
            b_dir.mkdir()
            (b_dir / "prompt.txt").write_text(PROMPT, encoding="utf-8")
            (b_dir / "stream.jsonl").write_text("\n".join(_stream_lines()) + "\n", encoding="utf-8")
            (b_dir / "packet.txt").write_text('{"arm":"full_brain","results":["secret"]}', encoding="utf-8")

            a_dir = root / "A"
            a_dir.mkdir()
            (a_dir / "meta.json").write_text(_harness.pretty_json(
                {"instance_id": "iid-a", "problem_statement": "A's own issue text"}), encoding="utf-8")

            pair = {"pair_id": "p1", "shared_files": ["src/widget.js"]}
            sheet = render_session.render_session(pair, a_dir, b_dir, "OPAQUE123")

            self.assertIn("OPAQUE123", sheet)
            self.assertIn("trailing newline", sheet)
            self.assertNotIn("full_brain", sheet)
            self.assertNotIn("secret", sheet)
            self.assertNotIn("frozen-memory-packet", sheet)


class LabelingUnitTest(unittest.TestCase):
    def test_pre_edit_locate_calls_matches_mechmetrics_horizon(self):
        with tempfile.TemporaryDirectory() as raw:
            stream = pathlib.Path(raw) / "stream.jsonl"
            stream.write_text("\n".join(_stream_lines()) + "\n", encoding="utf-8")
            calls = render_session.pre_edit_locate_calls(stream)
            # Grep (t1) and Read (t2) are pre-edit; the Read AFTER the Edit (t4)
            # must NOT appear -- this is the frozen mechmetrics horizon.
            self.assertEqual([c["name"] for c in calls], ["Grep", "Read"])
            self.assertEqual(calls[0]["target"], "'renderWidget' in src")
            self.assertIn("renderWidget", calls[0]["result_snippet"])
            self.assertEqual(calls[1]["target"], "src/widget.js")

    def test_pre_edit_locate_calls_agrees_with_mechmetrics_count(self):
        from brainmark.mechmetrics import session_metrics

        with tempfile.TemporaryDirectory() as raw:
            stream = pathlib.Path(raw) / "stream.jsonl"
            stream.write_text("\n".join(_stream_lines()) + "\n", encoding="utf-8")
            calls = render_session.pre_edit_locate_calls(stream)
            self.assertEqual(len(calls), session_metrics(stream)["locate_calls_pre_edit"])


class StratifiedSampleTest(unittest.TestCase):
    def _sessions(self):
        out = []
        for arm in ("no_brain", "full_brain"):
            for i in range(5):
                out.append((f"pair{i}", arm, pathlib.Path(f"/fake/{arm}/pair{i}")))
        return out

    def test_deterministic_for_fixed_seed(self):
        sessions = self._sessions()
        a = render_session.stratified_sample(sessions, 6, seed=42)
        b = render_session.stratified_sample(sessions, 6, seed=42)
        self.assertEqual(a, b)

    def test_balances_across_arms(self):
        sessions = self._sessions()
        sample = render_session.stratified_sample(sessions, 6, seed=42)
        arms = [item[1] for item in sample]
        self.assertEqual(arms.count("no_brain"), 3)
        self.assertEqual(arms.count("full_brain"), 3)

    def test_never_exceeds_requested_n_or_available_pool(self):
        sessions = self._sessions()
        self.assertEqual(len(render_session.stratified_sample(sessions, 3, seed=1)), 3)
        self.assertEqual(len(render_session.stratified_sample(sessions, 999, seed=1)), len(sessions))

    def test_different_seed_can_change_the_draw(self):
        sessions = self._sessions()
        a = render_session.stratified_sample(sessions, 4, seed=1)
        b = render_session.stratified_sample(sessions, 4, seed=2)
        self.assertNotEqual(a, b)


if __name__ == "__main__":
    unittest.main()
