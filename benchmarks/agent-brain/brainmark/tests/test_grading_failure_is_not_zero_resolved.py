"""An arm that could not be GRADED must not be scored as zero-resolved.

Two coupled defects, both live on this checkout.

1. grade.py stages `results/<tag>_<arm>/<arm>/<iid>/patch.diff` and shells out to
   graphmark's tools/grade_tag.sh, whose docstring in stage_arm says "we pass the
   arm through explicitly via the directory layout". It does not: grade_tag.sh
   re-derives the arm from the TAG with

       case "$TAG" in
         *baseline*) ARM=baseline ;;
         *cmm*)      ARM=cmm ;;
         *eg*)       ARM=eg ;;
         *) echo "cannot infer arm from tag $TAG"; exit 1 ;;

   BrainMark's arms are no_brain, full_brain, mem0, graphify, cmm, irrelevant.
   Only `cmm` matches. Probed against the real script:

       bm_pilot_no_brain    -> cannot infer arm from tag bm_pilot_no_brain
       bm_pilot_full_brain  -> cannot infer arm from tag bm_pilot_full_brain
       bm_pilot_mem0        -> cannot infer arm from tag bm_pilot_mem0
       bm_pilot_graphify    -> cannot infer arm from tag bm_pilot_graphify
       bm_pilot_cmm         -> (proceeds)
       bm_pilot_irrelevant  -> cannot infer arm from tag bm_pilot_irrelevant

   The headline arm and the baseline are both in the failing set.

2. When that happens grade.py records `entry["error"]` and no `resolved_ids`,
   and report.collect() does `resolved = set(entry.get("resolved_ids") or [])`,
   so EVERY instance of the failed arm is marked `resolved=False`. compare()
   then sees the key present and runs McNemar on a fabricated 0-of-n, with no
   warning -- while `cmm`, the one arm that grades, reports real numbers. An
   infrastructure failure becomes "memory did not help", asymmetrically.
"""

from __future__ import annotations

import json
import pathlib
import sys
import tempfile
import textwrap
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2]))

from brainmark import _harness, grade, report  # noqa: E402

ARMS = ["no_brain", "full_brain"]

FAKE_GRADE_TAG = textwrap.dedent("""\
    #!/usr/bin/env bash
    # A stand-in for graphmark's tools/grade_tag.sh, with ITS arm inference.
    set -euo pipefail
    TAG="${1:?tag}"
    case "$TAG" in
      *baseline*) ARM=baseline ;;
      *cmm*)      ARM=cmm ;;
      *eg*)       ARM=eg ;;
      *) echo "cannot infer arm from tag $TAG"; exit 1 ;;
    esac
    echo "would grade $ARM"
""")


def _write_cell(cell: pathlib.Path, instance_id: str, locate: int) -> None:
    cell.mkdir(parents=True, exist_ok=True)
    packet = '{"query":"q","results":[]}'
    (cell / "packet.txt").write_text(packet, encoding="utf-8")
    (cell / "packet.sha256").write_text(_harness.sha256_text(packet) + "\n", encoding="utf-8")
    (cell / "prompt.txt").write_text(
        f"TASK\n\n--- MEMORY ---\n<frozen-memory-packet>\n{packet}\n</frozen-memory-packet>\n",
        encoding="utf-8")
    (cell / "patch.diff").write_text("diff --git a/x b/x\n", encoding="utf-8")
    (cell / "meta.json").write_text(_harness.pretty_json({
        "instance_id": instance_id,
        "result_event": {"total_cost_usd": 0.1, "subtype": "success"},
        "mechmetrics": {"locate_calls_pre_edit": locate, "tokens": {"total_tokens": 100}},
    }), encoding="utf-8")


def _results_tree(root: pathlib.Path, n: int = 3) -> pathlib.Path:
    results = root / "B"
    for index in range(n):
        pair = results / f"a{index}__then__b{index}"
        for offset, arm in enumerate(ARMS):
            _write_cell(pair / arm, f"b{index}", locate=2 + offset)
    return results


class GradeTagArmInferenceTest(unittest.TestCase):
    """grade.py must refuse up front, not discover this after Docker starts."""

    def setUp(self) -> None:
        self.tmp = pathlib.Path(tempfile.mkdtemp())
        self.graphmark = self.tmp / "graphmark"
        (self.graphmark / "tools").mkdir(parents=True)
        self.script = self.graphmark / "tools" / "grade_tag.sh"
        self.script.write_text(FAKE_GRADE_TAG, encoding="utf-8")
        self.results = _results_tree(self.tmp)
        self.config = {
            "graphmark_root": str(self.graphmark),
            "grading": {"grade_tag_script": "tools/grade_tag.sh"},
        }

    def test_an_arm_the_script_cannot_infer_is_refused_before_any_grading(self):
        with self.assertRaises(SystemExit) as ctx:
            grade.run(self.results, self.config, "bm_pilot", ARMS, dry_run=True)
        message = str(ctx.exception)
        self.assertIn("no_brain", message)
        self.assertIn("full_brain", message)
        self.assertIn("cannot infer", message.lower())

    def test_an_arm_the_script_would_misattribute_is_refused(self):
        """`*cmm*` on a tag ending in a DIFFERENT arm resolves to the wrong dir."""
        problems = grade.unresolvable_arms(self.script, "bm_cmm_pilot", ["no_brain"])
        self.assertEqual([p[0] for p in problems], ["no_brain"])
        self.assertIn("cmm", problems[0][1])

    def test_an_arm_the_script_does_infer_is_accepted(self):
        self.assertEqual(grade.unresolvable_arms(self.script, "bm_pilot", ["cmm"]), [])


class UngradedArmIsNotZeroResolvedTest(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = pathlib.Path(tempfile.mkdtemp())
        self.results = _results_tree(self.tmp)
        self.config = {"_config_sha256": "x"}

    def _grading(self, failed_arm: str) -> None:
        per_arm = {}
        for arm in ARMS:
            if arm == failed_arm:
                per_arm[arm] = {"error": "grade_tag.sh produced no official report",
                                "returncode": 1}
            else:
                per_arm[arm] = {"resolved_ids": ["b0"], "resolved": 1, "total": 3}
        (self.results / "grading.json").write_text(
            _harness.pretty_json({"tag": "t", "arms": ARMS, "per_arm": per_arm}),
            encoding="utf-8")

    def test_a_failed_arm_is_not_marked_all_unresolved(self):
        self._grading(failed_arm="full_brain")
        built = report.build_report(self.results, self.config, ARMS, require_seal=False)
        self.assertFalse(built["refused"], built["problems"])
        table_arm = built["headline"]
        self.assertNotIn(
            "resolved", table_arm,
            "an arm that was never graded was scored 0-resolved and pushed "
            "through McNemar as if that were a measurement",
        )

    def test_the_failure_is_reported_not_silent(self):
        self._grading(failed_arm="full_brain")
        built = report.build_report(self.results, self.config, ARMS, require_seal=False)
        self.assertTrue(
            any("full_brain" in w and "grad" in w.lower() for w in built["warnings"]),
            built["warnings"],
        )

    def test_a_fully_graded_run_still_reports_resolved(self):
        (self.results / "grading.json").write_text(_harness.pretty_json({
            "tag": "t", "arms": ARMS,
            "per_arm": {arm: {"resolved_ids": ["b0"], "resolved": 1, "total": 3}
                        for arm in ARMS},
        }), encoding="utf-8")
        built = report.build_report(self.results, self.config, ARMS, require_seal=False)
        self.assertIn("resolved", built["headline"])
        self.assertEqual(built["headline"]["resolved"]["treatment"], 1)


if __name__ == "__main__":
    unittest.main()
