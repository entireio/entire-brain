"""Prescreen must not reset human judgments or replace their review materials."""

from __future__ import annotations

import argparse
import contextlib
import io
import json
import pathlib
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2]))

from brainmark import _harness, seal_prescreen
from brainmark.tests.test_prescreen_template import TemplateHarness


class PrescreenRerunTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.h = TemplateHarness(pathlib.Path(self.tmp.name))
        instances = []
        for candidate in self.h.candidates:
            for side in (candidate["a"], candidate["b"]):
                statement = f"Fix {side['instance_id']}"
                patch = "diff --git a/src/m.py b/src/m.py\n+fixed\n"
                side["problem_statement_sha256"] = _harness.sha256_text(statement)
                side["patch_sha256"] = _harness.sha256_text(patch)
                instances.append(dict(side, problem_statement=statement, patch=patch,
                                      repo=candidate["repo"]))
            (self.h.candidates_dir / f"{candidate['pair_id']}.json").write_text(
                _harness.pretty_json(candidate), encoding="utf-8")
        (self.h.root / "tasks.json").write_text(
            _harness.pretty_json({"instances": instances}), encoding="utf-8")
        self.h.config.update(graphmark_root=str(self.h.root), task_globs=["tasks.json"])
        self.h.config_path.write_text(_harness.pretty_json(self.h.config), encoding="utf-8")
        self.out = self.h.root / "review"
        self.args = argparse.Namespace(
            config=str(self.h.config_path), candidates=str(self.h.candidates_dir),
            out_dir=str(self.out), slate_size=6, dev_size=1, repo_cap_frac=1.0,
            dup_cap=2, stratum_floor=0, stratum_cap_frac=1.0, summary_only=False,
            no_template=False, no_sheets=False, raters="rater_1,rater_2")

    def run_prescreen(self):
        with contextlib.redirect_stdout(io.StringIO()):
            return seal_prescreen.run(self.args)

    def snapshot(self):
        return {str(p.relative_to(self.out)): p.read_bytes()
                for p in self.out.rglob("*") if p.is_file()}

    def test_fresh_run_creates_blank_independent_rater_files(self):
        result = self.run_prescreen()
        self.assertTrue(result["template_ok"])
        template = (self.out / "REVIEW-TEMPLATE.json").read_bytes()
        for rid in ("rater_1", "rater_2"):
            self.assertEqual((self.out / f"REVIEW-{rid}.json").read_bytes(), template)
        self.assertTrue((self.out / "sheets" / "INDEX.md").is_file())

    def test_rerun_refuses_before_writing_any_review_material(self):
        self.run_prescreen()
        for rid in ("rater_1", "rater_2"):
            path = self.out / f"REVIEW-{rid}.json"
            review = json.loads(path.read_text())
            for pair in review["pairs"].values():
                pair["reviews"][rid].update(verdict="accept", notes=f"Human notes: {rid}")
                pair["adjudication"].update(adjudicator="Human", note="Keep this resolution",
                                            resolved_verdict="accept")
            path.write_text(json.dumps(review, indent=3) + "\n", encoding="utf-8")
        before = self.snapshot()
        # Regeneration would change the slate, template and sheets as well as reviews.
        self.args.slate_size = 3
        for no_sheets, no_template in ((False, False), (True, False), (False, True)):
            with self.subTest(no_sheets=no_sheets, no_template=no_template):
                self.args.no_sheets, self.args.no_template = no_sheets, no_template
                with self.assertRaisesRegex(SystemExit, "REFUSING.*existing.*review"):
                    self.run_prescreen()
                self.assertEqual(self.snapshot(), before)

    def test_any_existing_rater_blocks_even_when_rater_list_changes(self):
        self.out.mkdir()
        for rid in ("rater_1", "rater_2", "previous_reviewer"):
            with self.subTest(rater=rid):
                path = self.out / f"REVIEW-{rid}.json"
                path.write_bytes(b'{"notes": "unfinished human work"}\n')
                before = self.snapshot()
                self.args.no_sheets = True
                with self.assertRaisesRegex(SystemExit, "REFUSING.*existing.*review"):
                    self.run_prescreen()
                self.assertEqual(self.snapshot(), before)
                path.unlink()

    def test_summary_only_can_read_existing_packet_without_changing_it(self):
        self.run_prescreen()
        before = self.snapshot()
        self.args.summary_only = True
        self.assertIsNone(self.run_prescreen()["template"])
        self.assertEqual(self.snapshot(), before)


if __name__ == "__main__":
    unittest.main()
