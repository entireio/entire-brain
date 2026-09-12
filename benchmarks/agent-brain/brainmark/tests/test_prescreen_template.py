"""seal_prescreen's REVIEW-TEMPLATE.json vs seal.py's actual v2 parser.

The template is what two humans spend an afternoon typing into. If its shape and
`seal.py promote`'s expectations disagree, that afternoon is discovered to be
unusable only at promotion time, after the verdicts exist and cannot honestly be
re-derived. So the contract is pinned from both directions:

  * the template is byte-identical to `seal.py review-template`'s own output once
    the prescreen's documented `_`-prefixed reader aids are removed -- and the
    stripping is by an explicit key list, because seal.py's template has its own
    `_instructions` key and a prefix rule would delete the raters' instructions
    and then "prove" a match,
  * `seal.py promote` actually consumes the template: three pairs are filled in
    with synthetic dual verdicts in a throwaway root, and promotion must produce
    a manifest with Cohen's kappa,
  * removing one rater's verdict from that same filled template must still
    REFUSE -- the guarantee is worthless if the template happens to satisfy the
    parser only because the parser stopped checking.

Offline, no network, no paid calls.
"""

from __future__ import annotations

import argparse
import io
import contextlib
import json
import pathlib
import shutil
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2]))

from brainmark import _harness, seal, seal_prescreen  # noqa: E402


def _candidate(pair_id: str, repo: str = "owner/repo", score: float = 0.5,
               flagged: bool = False) -> dict:
    a_id, b_id = pair_id.split("__then__")
    return {
        "schema_version": 1, "pair_id": pair_id, "repo": repo,
        "a": {"instance_id": a_id, "base_commit": "c1" * 20,
              "created_at": "2020-01-01 00:00:00", "files": ["src/m.py"],
              "problem_statement_sha256": "x", "patch_sha256": "y",
              "version": "1", "language": "Python"},
        "b": {"instance_id": b_id, "base_commit": "c2" * 20,
              "created_at": "2021-01-01 00:00:00", "files": ["src/m.py"],
              "problem_statement_sha256": "x", "patch_sha256": "z",
              "version": "1", "language": "Python"},
        "shared_files": ["src/m.py"], "score": score,
        "score_components": {"file_jaccard": score, "symbol_overlap": 0.0},
        "shared_symbols": [], "ancestry_verified": True,
        "leakage": {"patch_body_overlap": 0.1, "cap": 0.8, "borderline": flagged},
        "needs_human_review": flagged,
    }


def _slate_entry(candidate: dict, rank: int) -> dict:
    return {
        "pair_id": candidate["pair_id"], "repo": candidate["repo"],
        "score": candidate["score"], "rank": rank,
        "language": "Python", "pool": "swe_bench",
        "needs_human_review": candidate["needs_human_review"],
    }


class TemplateHarness:
    """A throwaway brainmark root with N candidates and nothing else moving."""

    def __init__(self, tmp: pathlib.Path, n: int = 6) -> None:
        self.root = tmp / "brainmark"
        self.root.mkdir(parents=True)
        real = _harness.BRAINMARK_DIR
        (self.root / "vendor").mkdir()
        for rel in ("mine_pairs.py", "prompts.py", "mechmetrics.py",
                    "PREREGISTRATION.md", "config.json"):
            src = real / rel
            if src.is_file():
                shutil.copyfile(src, self.root / rel)
            else:
                (self.root / rel).write_text(f"placeholder {rel}\n", encoding="utf-8")
        shutil.copyfile(real / "vendor" / "graphmark_metrics.py",
                        self.root / "vendor" / "graphmark_metrics.py")

        self.candidates_dir = self.root / "candidates"
        self.candidates_dir.mkdir()
        self.candidates = [
            _candidate(f"a{i}__then__b{i}", repo=f"owner/repo{i}",
                       score=1.0 - i * 0.01, flagged=(i == 1))
            for i in range(n)
        ]
        for cand in self.candidates:
            (self.candidates_dir / f"{cand['pair_id']}.json").write_text(
                _harness.pretty_json(cand), encoding="utf-8")
        (self.candidates_dir / "INDEX.json").write_text(
            _harness.pretty_json(
                {"candidate_pair_ids": [c["pair_id"] for c in self.candidates]}),
            encoding="utf-8")
        self.slate = [_slate_entry(c, i + 1) for i, c in enumerate(self.candidates)]
        self.sheet_names = {e["pair_id"]: f"{e['rank']:03d}_{e['pair_id']}.md"
                            for e in self.slate}

        self.config = json.loads((real / "config.json").read_text(encoding="utf-8"))
        self.config["seal"] = {"sealed_min": 2, "dev_split": 1,
                               "prereg_path": "PREREGISTRATION.md"}
        self.config_path = self.root / "test-config.json"
        self.config_path.write_text(_harness.pretty_json(self.config), encoding="utf-8")

    def build_template(self, dev_ids: set[str] | None = None,
                       raters: str = "rater_1,rater_2") -> dict:
        out = self.root / "review" / "REVIEW-TEMPLATE.json"
        with contextlib.redirect_stdout(io.StringIO()):
            return seal_prescreen.build_review_template(
                self.candidates_dir, self.slate, dev_ids or set(),
                self.sheet_names, out, raters)

    def promote(self, review_path: pathlib.Path, allow_short: bool = True) -> pathlib.Path:
        original = _harness.BRAINMARK_DIR
        _harness.BRAINMARK_DIR = self.root
        seal._harness.BRAINMARK_DIR = self.root
        try:
            with contextlib.redirect_stdout(io.StringIO()):
                seal.cmd_promote(argparse.Namespace(
                    candidates=str(self.candidates_dir), review=str(review_path),
                    config=str(self.config_path), allow_short=allow_short))
        finally:
            _harness.BRAINMARK_DIR = original
            seal._harness.BRAINMARK_DIR = original
        return self.root / seal.MANIFEST_NAME


def _fill(template: dict, pair_ids: list[str], raters: tuple[str, str],
          splits: dict[str, str]) -> dict:
    """Synthetic dual verdicts for exactly `pair_ids`; drop the rest.

    `seal.py promote` demands a verdict from both raters on EVERY pair present in
    REVIEW.json, so a partial review is submitted by shrinking `pairs`, never by
    leaving blanks -- which is also the instruction the raters get.
    """
    filled = json.loads(json.dumps(template))
    filled["pairs"] = {pid: filled["pairs"][pid] for pid in pair_ids}
    for pid in pair_ids:
        block = filled["pairs"][pid]
        for rid in raters:
            block["reviews"][rid] = {"verdict": "accept", "criteria": ["DEP"],
                                     "notes": "synthetic"}
        block["split"] = splits[pid]
    for rid, name in zip(raters, ("Rater One", "Rater Two")):
        filled["raters"][rid] = {"name": name, "affiliation": "project-affiliated"}
    return filled


class TemplateShapeTest(unittest.TestCase):
    def test_template_equals_seal_review_template_after_stripping_aids(self):
        with tempfile.TemporaryDirectory() as raw:
            h = TemplateHarness(pathlib.Path(raw))
            template = h.build_template()
            ok, problems = seal_prescreen.template_matches_seal_schema(
                template, h.candidates_dir, h.slate, "rater_1,rater_2")
            self.assertTrue(ok, problems)

    def test_stripping_is_by_explicit_key_not_by_underscore_prefix(self):
        # seal.py's own template carries `_instructions`. A prefix-based strip
        # would remove it and still report MATCH -- the raters would receive a
        # template with no instructions and nothing would have complained.
        with tempfile.TemporaryDirectory() as raw:
            h = TemplateHarness(pathlib.Path(raw))
            template = h.build_template()
            self.assertIn("_instructions", template)
            self.assertNotIn("_instructions", seal_prescreen.PRESCREEN_TEMPLATE_KEYS)
            broken = json.loads(json.dumps(template))
            del broken["_instructions"]
            ok, problems = seal_prescreen.template_matches_seal_schema(
                broken, h.candidates_dir, h.slate, "rater_1,rater_2")
            self.assertFalse(ok)
            self.assertTrue(problems)

    def test_every_pair_block_carries_the_fields_promote_v2_reads(self):
        with tempfile.TemporaryDirectory() as raw:
            h = TemplateHarness(pathlib.Path(raw))
            template = h.build_template()
            self.assertEqual(sorted(template["raters"]), ["rater_1", "rater_2"])
            self.assertEqual(template["criteria_codes"], dict(seal.CRITERIA_CODES))
            self.assertEqual(template["schema_version"], seal.SEAL_SCHEMA_VERSION_V2)
            for pid, block in template["pairs"].items():
                self.assertEqual(sorted(block["reviews"]), ["rater_1", "rater_2"])
                for rid in ("rater_1", "rater_2"):
                    self.assertEqual(sorted(block["reviews"][rid]),
                                     ["criteria", "notes", "verdict"])
                    self.assertEqual(block["reviews"][rid]["verdict"], "")
                self.assertEqual(sorted(block["adjudication"]),
                                 ["adjudicator", "note", "resolved_verdict"])
                self.assertEqual(block["split"], "")
                self.assertIn(block["_proposed_split"], ("sealed", "dev"))
                self.assertTrue(block["_sheet"].startswith("sheets/"))
                self.assertEqual(block["_sheet"], f"sheets/{h.sheet_names[pid]}")

    def test_extra_keys_are_exactly_the_documented_aids(self):
        with tempfile.TemporaryDirectory() as raw:
            h = TemplateHarness(pathlib.Path(raw))
            template = h.build_template(dev_ids={h.slate[0]["pair_id"]})
            for block in template["pairs"].values():
                extras = {k for k in block if k.startswith("_")}
                self.assertEqual(extras, set(seal_prescreen.PRESCREEN_PAIR_KEYS))
            self.assertEqual(template["_prescreen"]["dev_proposed"],
                             [h.slate[0]["pair_id"]])

    def test_dev_proposal_is_marked_in_the_template(self):
        with tempfile.TemporaryDirectory() as raw:
            h = TemplateHarness(pathlib.Path(raw))
            dev = {h.slate[1]["pair_id"]}
            template = h.build_template(dev_ids=dev)
            proposed = {pid for pid, b in template["pairs"].items()
                        if b["_proposed_split"] == "dev"}
            self.assertEqual(proposed, dev)

    def test_template_is_written_deterministically(self):
        with tempfile.TemporaryDirectory() as raw:
            h = TemplateHarness(pathlib.Path(raw))
            first = _harness.pretty_json(h.build_template())
            second = _harness.pretty_json(h.build_template())
            self.assertEqual(first, second)


class PromoteAcceptsTheTemplateTest(unittest.TestCase):
    RATERS = ("rater_1", "rater_2")

    def test_three_pairs_of_synthetic_dual_reviews_promote_with_kappa(self):
        with tempfile.TemporaryDirectory() as raw:
            h = TemplateHarness(pathlib.Path(raw))
            template = h.build_template()
            ids = [e["pair_id"] for e in h.slate[:3]]
            filled = _fill(template, ids, self.RATERS,
                           {ids[0]: "sealed", ids[1]: "sealed", ids[2]: "dev"})
            review_path = h.root / "REVIEW.json"
            review_path.write_text(_harness.pretty_json(filled), encoding="utf-8")

            manifest_path = h.promote(review_path)
            manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
            self.assertEqual(manifest["schema_version"], seal.SEAL_SCHEMA_VERSION_V2)
            self.assertEqual(manifest["sealed_count"], 2)
            self.assertEqual(manifest["dev_count"], 1)
            self.assertEqual(manifest["seal_v2"]["n_dually_reviewed_pairs"], 3)
            self.assertEqual(manifest["seal_v2"]["cohens_kappa"], 1.0)
            self.assertEqual((h.root / "tasks" / "dev" / f"{ids[2]}.json").exists(), True)
            ok, problems = seal.verify(h.root)
            self.assertTrue(ok, problems)

    def test_missing_second_review_is_still_refused_from_this_template(self):
        with tempfile.TemporaryDirectory() as raw:
            h = TemplateHarness(pathlib.Path(raw))
            template = h.build_template()
            ids = [e["pair_id"] for e in h.slate[:3]]
            filled = _fill(template, ids, self.RATERS,
                           {ids[0]: "sealed", ids[1]: "sealed", ids[2]: "dev"})
            filled["pairs"][ids[1]]["reviews"]["rater_2"]["verdict"] = ""
            review_path = h.root / "REVIEW.json"
            review_path.write_text(_harness.pretty_json(filled), encoding="utf-8")

            with self.assertRaises(SystemExit) as ctx:
                h.promote(review_path)
            self.assertIn("missing a review", str(ctx.exception))
            self.assertFalse((h.root / seal.MANIFEST_NAME).exists())

    def test_unfilled_template_refuses_on_every_pair(self):
        # The template as shipped must not promote: verdicts are empty strings,
        # so `promote` should refuse rather than treat blank as reject.
        with tempfile.TemporaryDirectory() as raw:
            h = TemplateHarness(pathlib.Path(raw))
            template = h.build_template()
            template["raters"] = {
                "rater_1": {"name": "A", "affiliation": "project-affiliated"},
                "rater_2": {"name": "B", "affiliation": "project-affiliated"},
            }
            review_path = h.root / "REVIEW.json"
            review_path.write_text(_harness.pretty_json(template), encoding="utf-8")
            with self.assertRaises(SystemExit) as ctx:
                h.promote(review_path)
            self.assertIn("missing a review", str(ctx.exception))

    def test_reject_without_a_criteria_code_is_refused(self):
        with tempfile.TemporaryDirectory() as raw:
            h = TemplateHarness(pathlib.Path(raw))
            template = h.build_template()
            ids = [e["pair_id"] for e in h.slate[:3]]
            filled = _fill(template, ids, self.RATERS,
                           {ids[0]: "sealed", ids[1]: "sealed", ids[2]: "dev"})
            for rid in self.RATERS:
                filled["pairs"][ids[2]]["reviews"][rid] = {
                    "verdict": "reject", "criteria": [], "notes": ""}
            filled["pairs"][ids[2]]["split"] = ""
            review_path = h.root / "REVIEW.json"
            review_path.write_text(_harness.pretty_json(filled), encoding="utf-8")
            with self.assertRaises(SystemExit) as ctx:
                h.promote(review_path)
            self.assertIn("cites no criteria code", str(ctx.exception))

    def test_custom_rater_ids_flow_through_to_promote(self):
        with tempfile.TemporaryDirectory() as raw:
            h = TemplateHarness(pathlib.Path(raw))
            template = h.build_template(raters="suhaan,teammate")
            self.assertEqual(sorted(template["raters"]), ["suhaan", "teammate"])
            ids = [e["pair_id"] for e in h.slate[:2]]
            filled = _fill(template, ids, ("suhaan", "teammate"),
                           {ids[0]: "sealed", ids[1]: "dev"})
            review_path = h.root / "REVIEW.json"
            review_path.write_text(_harness.pretty_json(filled), encoding="utf-8")
            manifest = json.loads(h.promote(review_path).read_text(encoding="utf-8"))
            self.assertEqual(sorted(manifest["seal_v2"]["raters"]), ["suhaan", "teammate"])
            self.assertIn("suhaan+teammate", manifest["reviewer"])


class LiveSlateTemplateTest(unittest.TestCase):
    """If review/ has been generated in this checkout, hold IT to the contract."""

    def setUp(self):
        self.review_dir = _harness.BRAINMARK_DIR / "review"
        self.template_path = self.review_dir / "REVIEW-TEMPLATE.json"
        self.slate_path = self.review_dir / "SLATE.json"
        if not (self.template_path.is_file() and self.slate_path.is_file()):
            self.skipTest("review/ not generated; run seal_prescreen.py first")

    def test_generated_template_matches_the_generated_slate(self):
        slate_doc = json.loads(self.slate_path.read_text(encoding="utf-8"))
        template = json.loads(self.template_path.read_text(encoding="utf-8"))
        self.assertEqual(sorted(template["pairs"]),
                         sorted(e["pair_id"] for e in slate_doc["slate"]))
        self.assertTrue(slate_doc["review_template"]["matches_seal_review_template"])
        self.assertEqual(slate_doc["review_template"]["problems"], [])

    def test_generated_template_has_no_prefilled_verdicts(self):
        template = json.loads(self.template_path.read_text(encoding="utf-8"))
        for pid, block in template["pairs"].items():
            for rid, rv in block["reviews"].items():
                self.assertEqual(rv["verdict"], "", f"{pid}/{rid} is prefilled")
                self.assertEqual(rv["criteria"], [])
            self.assertEqual(block["split"], "")
            self.assertIsNone(block["adjudication"]["resolved_verdict"])

    def test_generated_slate_respects_its_own_caps(self):
        slate_doc = json.loads(self.slate_path.read_text(encoding="utf-8"))
        comp = slate_doc["composition"]
        self.assertTrue(comp["repo_cap_respected"], comp.get("repo_cap_violations"))
        self.assertLessEqual(comp["top_repo_share"],
                             slate_doc["parameters"]["repo_cap_frac"] + 1e-9)
        self.assertTrue(slate_doc["sha256_recheck"]["ok"])
        self.assertTrue(slate_doc["pool_integrity_ok"])

    def test_every_slated_pair_has_a_sheet_on_disk(self):
        slate_doc = json.loads(self.slate_path.read_text(encoding="utf-8"))
        for e in slate_doc["slate"]:
            sheet = self.review_dir / e["sheet"]
            if not sheet.parent.is_dir():
                self.skipTest("review sheets are derived artifacts, absent in a fresh checkout; run seal_prescreen.py to regenerate")
            self.assertTrue(sheet.is_file(), e["sheet"])


if __name__ == "__main__":
    unittest.main()
