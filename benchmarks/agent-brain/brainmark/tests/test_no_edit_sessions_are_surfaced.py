"""A session with no EDIT event measures the primary metric on a WIDER horizon.

`locate_calls_pre_edit` counts locate calls strictly before the first EDIT. A
session that never emits one has no cutoff, so mechmetrics counts locate calls
over the WHOLE session (`no_edit=True`). mechmetrics.py's docstring says such
sessions "are reported separately and are dropped by the standard pair gate
anyway (empty patch)". Both halves are false:

  * an edit made THROUGH the shell -- `sed -i`, a python heredoc, `cat >` --
    is not an EDIT event on either backend (the docstring says so itself, two
    bullets earlier), yet it produces a NON-EMPTY patch, so the pair gate
    (usd>0 + non-empty patch) passes it;
  * report.py reads `no_edit` nowhere at all.

Measured on two hand-built streams with identical exploration:

    shell-edit  locate_calls_pre_edit=4  no_edit=True   horizon=5 calls
    tool-edit   locate_calls_pre_edit=3  no_edit=False  horizon=3 calls

So the metric's definition changes with the edit MODALITY, and the inflation
lands on whichever arm shell-edits more. That is a plausible mechanism for a
headline: an arm that knows where to edit reaches for Edit, an arm that is
flailing explores and then `sed -i`s.

This is not a gate change -- the pre-registered gate stays exactly as written
(PREREGISTRATION.md section 5). It makes the exposure VISIBLE: the count, and a
sensitivity estimate on the pairs where every arm emitted a real edit.
"""

from __future__ import annotations

import json
import pathlib
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2]))

from brainmark import _harness, prompts, report  # noqa: E402

ARMS = ["no_brain", "full_brain"]


def _tool_use(name: str, tool_input: dict) -> str:
    return json.dumps({"type": "assistant", "message": {"content": [
        {"type": "tool_use", "id": name, "name": name, "input": tool_input}]}})


RESULT = json.dumps({"type": "result", "subtype": "success", "is_error": False,
                     "num_turns": 4, "duration_ms": 1000, "total_cost_usd": 0.1,
                     "modelUsage": {"m": {"input_tokens": 10, "output_tokens": 5}}})

SHELL_EDIT = [
    _tool_use("Grep", {"pattern": "x"}), _tool_use("Read", {"file_path": "a"}),
    _tool_use("Bash", {"command": "rg foo"}),
    _tool_use("Bash", {"command": "sed -i '' s/a/b/ f.py"}),
    _tool_use("Bash", {"command": "grep after"}), RESULT,
]
TOOL_EDIT = [
    _tool_use("Grep", {"pattern": "x"}), _tool_use("Read", {"file_path": "a"}),
    _tool_use("Bash", {"command": "rg foo"}),
    _tool_use("Edit", {"file_path": "f.py"}),
    _tool_use("Bash", {"command": "grep after"}), RESULT,
]


def _cell(cell: pathlib.Path, instance_id: str, stream: list[str]) -> None:
    from brainmark.mechmetrics import session_metrics

    cell.mkdir(parents=True, exist_ok=True)
    packet = '{"query":"q","results":[]}'
    (cell / "packet.txt").write_text(packet, encoding="utf-8")
    (cell / "packet.sha256").write_text(_harness.sha256_text(packet) + "\n", encoding="utf-8")
    (cell / "prompt.txt").write_text(
        f"T\n\n--- MEMORY ---\n<frozen-memory-packet>\n{packet}\n</frozen-memory-packet>\n",
        encoding="utf-8")
    (cell / "patch.diff").write_text("diff --git a/f.py b/f.py\n+x\n", encoding="utf-8")
    (cell / "stream.jsonl").write_text("\n".join(stream) + "\n", encoding="utf-8")
    prompt = (cell / "prompt.txt").read_text(encoding="utf-8")
    (cell / "prompt_sym.sha256").write_text(prompts.symmetry_sha(prompt) + "\n", encoding="utf-8")
    (cell / "meta.json").write_text(_harness.pretty_json({
        "instance_id": instance_id,
        "packet_sha256": _harness.sha256_text(packet),
        "prompt_sha256": _harness.sha256_text(prompt),
        "prompt_sym_sha256": prompts.symmetry_sha(prompt),
        "result_event": {"total_cost_usd": 0.1, "subtype": "success"},
        "mechmetrics": session_metrics(stream, backend="claude"),
    }), encoding="utf-8")


class NoEditSessionsAreSurfacedTest(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = pathlib.Path(tempfile.mkdtemp())
        self.results = self.tmp / "B"
        # p0/p1: every arm emitted a real edit. p2: the baseline shell-edited,
        # so its locate count is measured over the whole session.
        for index in (0, 1):
            for arm in ARMS:
                _cell(self.results / f"a{index}__then__b{index}" / arm, f"b{index}", TOOL_EDIT)
        _cell(self.results / "a2__then__b2" / "no_brain", "b2", SHELL_EDIT)
        _cell(self.results / "a2__then__b2" / "full_brain", "b2", TOOL_EDIT)
        self.config = {"_config_sha256": "x"}

    def test_the_shell_edited_cell_passes_the_gate(self):
        """The premise: mechmetrics claims these are dropped. They are not."""
        built = report.build_report(self.results, self.config, ARMS, require_seal=False)
        self.assertIn("a2__then__b2", built["clean_pairs"])

    def test_no_edit_cells_are_counted_per_arm(self):
        built = report.build_report(self.results, self.config, ARMS, require_seal=False)
        self.assertEqual(built["no_edit_cells"], {"no_brain": ["a2__then__b2"]})

    def test_the_exposure_is_warned_about(self):
        built = report.build_report(self.results, self.config, ARMS, require_seal=False)
        self.assertTrue(any("no_edit" in w for w in built["warnings"]), built["warnings"])

    def test_a_sensitivity_estimate_excludes_them(self):
        built = report.build_report(self.results, self.config, ARMS, require_seal=False)
        self.assertEqual(built["n_clean_all_arms_edited"], 2)
        sensitivity = built["headline_excluding_no_edit"]
        self.assertEqual(sensitivity["n"], 2)
        self.assertNotEqual(sensitivity["geomean_pct"], built["headline"]["geomean_pct"])

    def test_a_run_with_no_such_cell_carries_no_sensitivity_block(self):
        for arm in ARMS:
            _cell(self.results / "a2__then__b2" / arm, "b2", TOOL_EDIT)
        built = report.build_report(self.results, self.config, ARMS, require_seal=False)
        self.assertEqual(built["no_edit_cells"], {})
        self.assertNotIn("headline_excluding_no_edit", built)
        self.assertEqual(built["n_clean_all_arms_edited"], built["n_clean"])

    def test_the_markdown_shows_the_exposure(self):
        built = report.build_report(self.results, self.config, ARMS, require_seal=False)
        text = report.render_markdown(built)
        self.assertIn("no_edit", text)
        self.assertIn("Sensitivity", text)


if __name__ == "__main__":
    unittest.main()
