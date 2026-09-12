"""SEAL v2: dual independent review + Cohen's kappa (plan 0.7).

Round trip, refusal-without-second-review, and kappa correctness on a fixture
with a hand-computed answer. Mirrors the throwaway-root pattern in
test_seal_and_report.py but builds its own v2-shaped REVIEW.json -- the v1
harness there is untouched and out of this task's scope.
"""

from __future__ import annotations

import json
import pathlib
import shutil
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2]))

from brainmark import _harness, seal  # noqa: E402


def _pair(pair_id: str) -> dict:
    a_id, b_id = pair_id.split("__then__")
    return {
        "schema_version": 1, "pair_id": pair_id, "repo": "o/r",
        "a": {"instance_id": a_id, "base_commit": "c1", "created_at": "2020-01-01 00:00:00",
              "files": ["src/m.py"], "problem_statement_sha256": "x", "patch_sha256": "y",
              "version": "1", "language": "Python"},
        "b": {"instance_id": b_id, "base_commit": "c2", "created_at": "2021-01-01 00:00:00",
              "files": ["src/m.py"], "problem_statement_sha256": "x", "patch_sha256": "z",
              "version": "1", "language": "Python"},
        "shared_files": ["src/m.py"], "score": 0.5,
        "score_components": {"file_jaccard": 0.5, "symbol_overlap": 0.0},
        "shared_symbols": [], "ancestry_verified": True,
        "leakage": {"patch_body_overlap": 0.1, "cap": 0.8, "borderline": False},
        "needs_human_review": False,
    }


def _review_entry(v1: str, v2: str, split: str = "sealed", adjudication: dict | None = None,
                  criteria: tuple[str, ...] = ("DEP",)) -> dict:
    return {
        "needs_review": False, "score": 0.5, "patch_body_overlap": 0.1, "shared_files": ["src/m.py"],
        "reviews": {
            "r1": {"verdict": v1, "criteria": list(criteria) if v1 == "reject" or criteria else [], "notes": ""},
            "r2": {"verdict": v2, "criteria": list(criteria) if v2 == "reject" or criteria else [], "notes": ""},
        },
        "adjudication": adjudication or {"resolved_verdict": None, "adjudicator": "", "note": ""},
        "split": split,
    }


class SealV2Harness:
    """Throwaway brainmark root with the files seal.py hashes, v2 REVIEW.json."""

    def __init__(self, tmp: pathlib.Path, pairs_review: dict[str, dict],
                sealed_min: int = 1, dev_split: int = 1,
                raters: dict | None = None) -> None:
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

        self.candidates = self.root / "candidates"
        self.candidates.mkdir()
        pair_ids = list(pairs_review)
        for pair_id in pair_ids:
            (self.candidates / f"{pair_id}.json").write_text(
                _harness.pretty_json(_pair(pair_id)), encoding="utf-8")
        (self.candidates / "INDEX.json").write_text(
            _harness.pretty_json({"candidate_pair_ids": pair_ids}), encoding="utf-8")

        self.review = self.root / "REVIEW.json"
        self.review.write_text(_harness.pretty_json({
            "schema_version": 2,
            "raters": raters or {
                "r1": {"name": "Rater One", "affiliation": "project-affiliated (user)"},
                "r2": {"name": "Rater Two", "affiliation": "project-affiliated (teammate)"},
            },
            "pairs": pairs_review,
        }), encoding="utf-8")

        self.config = json.loads((real / "config.json").read_text(encoding="utf-8"))
        self.config["seal"] = {"sealed_min": sealed_min, "dev_split": dev_split,
                               "prereg_path": "PREREGISTRATION.md"}
        self.config_path = self.root / "test-config.json"
        self.config_path.write_text(_harness.pretty_json(self.config), encoding="utf-8")

    def promote(self, allow_short: bool = False) -> None:
        original = _harness.BRAINMARK_DIR
        _harness.BRAINMARK_DIR = self.root
        seal._harness.BRAINMARK_DIR = self.root
        try:
            args = type("A", (), {
                "candidates": str(self.candidates), "review": str(self.review),
                "config": str(self.config_path), "allow_short": allow_short,
            })()
            seal.cmd_promote(args)
        finally:
            _harness.BRAINMARK_DIR = original
            seal._harness.BRAINMARK_DIR = original


class DualReviewRoundTripTest(unittest.TestCase):
    def test_promote_v2_round_trip_and_kappa_present(self):
        pairs_review = {
            "a0__then__b0": _review_entry("accept", "accept", split="sealed"),
            "d0__then__e0": _review_entry("accept", "accept", split="dev"),
        }
        with tempfile.TemporaryDirectory() as raw:
            harness = SealV2Harness(pathlib.Path(raw), pairs_review)
            harness.promote()
            manifest = json.loads((harness.root / seal.MANIFEST_NAME).read_text(encoding="utf-8"))
            self.assertEqual(manifest["schema_version"], seal.SEAL_SCHEMA_VERSION_V2)
            self.assertEqual(manifest["sealed_count"], 1)
            self.assertEqual(manifest["dev_count"], 1)
            self.assertIn("seal_v2", manifest)
            self.assertEqual(manifest["seal_v2"]["n_dually_reviewed_pairs"], 2)
            self.assertAlmostEqual(manifest["seal_v2"]["cohens_kappa"], 1.0)
            ok, problems = seal.verify(harness.root)
            self.assertTrue(ok, problems)

    def test_disagreement_with_adjudication_accepts(self):
        pairs_review = {
            "a0__then__b0": _review_entry(
                "accept", "reject", split="sealed",
                adjudication={"resolved_verdict": "accept", "adjudicator": "Lead", "note": "sided with r1"},
                criteria=("DEP",),
            ),
            "d0__then__e0": _review_entry("reject", "reject", split=""),
        }
        with tempfile.TemporaryDirectory() as raw:
            harness = SealV2Harness(pathlib.Path(raw), pairs_review, sealed_min=1, dev_split=0)
            harness.promote()
            manifest = json.loads((harness.root / seal.MANIFEST_NAME).read_text(encoding="utf-8"))
            self.assertEqual(manifest["sealed_count"], 1)
            self.assertEqual(manifest["dev_count"], 0)
            self.assertEqual(manifest["seal_v2"]["disagreements_adjudicated"], 1)


class RefusalTest(unittest.TestCase):
    def test_refuses_without_second_review(self):
        pairs_review = {
            "a0__then__b0": _review_entry("accept", "accept", split="sealed"),
        }
        # Strip r2's verdict -- only ONE of the two independent reviews present.
        pairs_review["a0__then__b0"]["reviews"]["r2"]["verdict"] = ""
        with tempfile.TemporaryDirectory() as raw:
            harness = SealV2Harness(pathlib.Path(raw), pairs_review, sealed_min=1, dev_split=0)
            with self.assertRaises(SystemExit) as ctx:
                harness.promote()
            self.assertIn("missing a review", str(ctx.exception))
            self.assertFalse((harness.root / "SEAL-MANIFEST.json").exists())

    def test_refuses_on_disagreement_without_adjudication(self):
        pairs_review = {
            "a0__then__b0": _review_entry("accept", "reject", split="sealed"),
        }
        with tempfile.TemporaryDirectory() as raw:
            harness = SealV2Harness(pathlib.Path(raw), pairs_review, sealed_min=1, dev_split=0)
            with self.assertRaises(SystemExit) as ctx:
                harness.promote()
            self.assertIn("no named adjudication", str(ctx.exception))

    def test_refuses_reject_without_criteria_code(self):
        pairs_review = {
            "a0__then__b0": _review_entry("accept", "accept", split="sealed"),
            "d0__then__e0": _review_entry("reject", "reject", split="", criteria=()),
        }
        with tempfile.TemporaryDirectory() as raw:
            harness = SealV2Harness(pathlib.Path(raw), pairs_review, sealed_min=1, dev_split=0)
            with self.assertRaises(SystemExit) as ctx:
                harness.promote()
            self.assertIn("cites no criteria code", str(ctx.exception))

    def test_refuses_kappa_missing_with_fewer_than_two_reviewed_pairs(self):
        pairs_review = {
            "a0__then__b0": _review_entry("accept", "accept", split="sealed"),
        }
        with tempfile.TemporaryDirectory() as raw:
            harness = SealV2Harness(pathlib.Path(raw), pairs_review, sealed_min=1, dev_split=0)
            with self.assertRaises(SystemExit) as ctx:
                harness.promote()
            self.assertIn("kappa is missing", str(ctx.exception))

    def test_refuses_unattributed_rater(self):
        pairs_review = {
            "a0__then__b0": _review_entry("accept", "accept", split="sealed"),
            "d0__then__e0": _review_entry("accept", "accept", split="dev"),
        }
        with tempfile.TemporaryDirectory() as raw:
            harness = SealV2Harness(
                pathlib.Path(raw), pairs_review, sealed_min=1, dev_split=1,
                raters={"r1": {"name": "", "affiliation": "project-affiliated"},
                       "r2": {"name": "Rater Two", "affiliation": "project-affiliated"}},
            )
            with self.assertRaises(SystemExit) as ctx:
                harness.promote()
            self.assertIn("no name", str(ctx.exception))

    def test_refuses_unknown_criteria_code(self):
        entry = _review_entry("reject", "reject", split="", criteria=("NOT_A_REAL_CODE",))
        pairs_review = {
            "a0__then__b0": _review_entry("accept", "accept", split="sealed"),
            "d0__then__e0": entry,
        }
        with tempfile.TemporaryDirectory() as raw:
            harness = SealV2Harness(pathlib.Path(raw), pairs_review, sealed_min=1, dev_split=0)
            with self.assertRaises(SystemExit) as ctx:
                harness.promote()
            self.assertIn("unknown criteria code", str(ctx.exception))


class KappaFixtureTest(unittest.TestCase):
    """Cohen's kappa on a hand-computed fixture (textbook 2x2 contingency table).

    10 dually-reviewed items: 5 accept/accept, 3 reject/reject, 1 accept/reject,
    1 reject/accept.
        po = 8/10 = 0.8
        r1: accept=6, reject=4  |  r2: accept=6, reject=4
        pe = (6/10)(6/10) + (4/10)(4/10) = 0.36 + 0.16 = 0.52
        kappa = (0.8 - 0.52) / (1 - 0.52) = 0.28 / 0.48 = 0.58333...
    """

    def test_kappa_matches_hand_computation(self):
        rated = (
            [("accept", "accept")] * 5
            + [("reject", "reject")] * 3
            + [("accept", "reject")]
            + [("reject", "accept")]
        )
        result = seal.cohens_kappa(rated)
        self.assertEqual(result["n"], 10)
        self.assertAlmostEqual(result["observed_agreement"], 0.8)
        self.assertAlmostEqual(result["expected_agreement"], 0.52)
        self.assertAlmostEqual(result["kappa"], 0.28 / 0.48, places=9)

    def test_kappa_perfect_agreement_single_category_is_one(self):
        # po == pe == 1 (0/0 in the raw formula) is conventionally kappa=1.0.
        result = seal.cohens_kappa([("accept", "accept")] * 4)
        self.assertEqual(result["kappa"], 1.0)

    def test_kappa_zero_items_raises(self):
        with self.assertRaises(ValueError):
            seal.cohens_kappa([])

    def test_kappa_is_reusable_for_three_category_labels(self):
        # validity/analyze_validity.py reuses this same function for yes/no/unclear.
        rated = [("yes", "yes"), ("no", "no"), ("unclear", "no"), ("yes", "unclear")]
        result = seal.cohens_kappa(rated)
        self.assertEqual(sorted(result["categories"]), ["no", "unclear", "yes"])


if __name__ == "__main__":
    unittest.main()
