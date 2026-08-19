"""seal_prescreen --merge-reviews: two independent rater files -> one REVIEW.json.

The merge exists so neither rater has to read the other's verdict on the way to
their own row (SEAL_PROTOCOL.md rater instruction 2), and its output feeds
straight into `seal.py promote`. The failure modes it must catch are the ones
that would silently corrupt the kappa rather than break the run:

  * one rater filling in the other's column (the file has two owners),
  * a file nobody has started (no owner),
  * a pair only one rater reached being emitted with a blank verdict instead of
    being dropped and listed,
  * the two files disagreeing on a field only one of them should hold.

Offline, no network, no paid calls.
"""

from __future__ import annotations

import contextlib
import io
import json
import pathlib
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2]))

from brainmark import _harness, seal, seal_prescreen  # noqa: E402


def _template(pair_ids: list[str], raters: tuple[str, str] = ("r1", "r2")) -> dict:
    return {
        "schema_version": 2,
        "_instructions": "see SEAL_PROTOCOL.md",
        "criteria_codes": dict(seal.CRITERIA_CODES),
        "raters": {rid: {"name": "", "affiliation": ""} for rid in raters},
        "pairs": {
            pid: {
                "needs_review": False, "score": 0.5, "patch_body_overlap": 0.1,
                "shared_files": ["src/m.py"],
                "reviews": {rid: {"verdict": "", "criteria": [], "notes": ""}
                            for rid in raters},
                "adjudication": {"resolved_verdict": None, "adjudicator": "", "note": ""},
                "split": "",
            }
            for pid in pair_ids
        },
    }


def _rater_copy(pair_ids: list[str], owner: str, verdicts: dict[str, str],
                name: str = "Somebody", raters: tuple[str, str] = ("r1", "r2"),
                criteria: dict[str, list[str]] | None = None,
                splits: dict[str, str] | None = None) -> dict:
    doc = _template(pair_ids, raters)
    doc["raters"][owner] = {"name": name, "affiliation": "project-affiliated"}
    for pid, verdict in verdicts.items():
        doc["pairs"][pid]["reviews"][owner] = {
            "verdict": verdict,
            "criteria": (criteria or {}).get(pid, ["DEP"]),
            "notes": f"{owner} on {pid}",
        }
        if splits and pid in splits:
            doc["pairs"][pid]["split"] = splits[pid]
    return doc


def _write(tmp: pathlib.Path, name: str, doc: dict) -> pathlib.Path:
    path = tmp / name
    path.write_text(_harness.pretty_json(doc), encoding="utf-8")
    return path


class MergeHappyPathTest(unittest.TestCase):
    def test_merges_two_single_rater_files(self):
        ids = ["a__then__b", "c__then__d", "e__then__f"]
        with tempfile.TemporaryDirectory() as raw:
            tmp = pathlib.Path(raw)
            a = _write(tmp, "REVIEW-r1.json", _rater_copy(
                ids, "r1", {i: "accept" for i in ids}, name="One",
                splits={ids[0]: "sealed", ids[1]: "sealed", ids[2]: "dev"}))
            b = _write(tmp, "REVIEW-r2.json", _rater_copy(
                ids, "r2", {ids[0]: "accept", ids[1]: "reject", ids[2]: "accept"},
                name="Two"))
            merged = seal_prescreen.merge_reviews([a, b])

        self.assertEqual(sorted(merged["pairs"]), sorted(ids))
        self.assertEqual(merged["raters"]["r1"]["name"], "One")
        self.assertEqual(merged["raters"]["r2"]["name"], "Two")
        self.assertEqual(merged["pairs"][ids[1]]["reviews"]["r1"]["verdict"], "accept")
        self.assertEqual(merged["pairs"][ids[1]]["reviews"]["r2"]["verdict"], "reject")
        self.assertEqual(merged["pairs"][ids[2]]["split"], "dev")
        self.assertEqual(merged["_merge"]["pairs_dually_reviewed"], 3)
        self.assertEqual(merged["_merge"]["verdicts_per_rater"], {"r1": 3, "r2": 3})

    def test_pairs_only_one_rater_reached_are_dropped_and_listed(self):
        ids = ["a__then__b", "c__then__d", "e__then__f"]
        with tempfile.TemporaryDirectory() as raw:
            tmp = pathlib.Path(raw)
            a = _write(tmp, "REVIEW-r1.json", _rater_copy(
                ids, "r1", {ids[0]: "accept", ids[1]: "accept"},
                splits={ids[0]: "sealed", ids[1]: "dev"}))
            b = _write(tmp, "REVIEW-r2.json", _rater_copy(
                ids, "r2", {ids[0]: "accept", ids[1]: "accept"}))
            merged = seal_prescreen.merge_reviews([a, b])

        self.assertEqual(sorted(merged["pairs"]), sorted(ids[:2]))
        self.assertEqual(merged["_merge"]["pairs_dropped_unreviewed"], [ids[2]])
        # Nothing blank survives -- a blank is what seal.py refuses on.
        for block in merged["pairs"].values():
            for rv in block["reviews"].values():
                self.assertIn(rv["verdict"], ("accept", "reject"))

    def test_merge_output_promotes_through_seal_v2(self):
        """The whole point: the merged file is what seal.py actually consumes."""
        from brainmark.tests.test_prescreen_template import TemplateHarness

        with tempfile.TemporaryDirectory() as raw:
            h = TemplateHarness(pathlib.Path(raw))
            template = h.build_template()
            ids = [e["pair_id"] for e in h.slate[:3]]
            files = []
            for owner, name in (("rater_1", "One"), ("rater_2", "Two")):
                doc = json.loads(json.dumps(template))
                doc["raters"][owner] = {"name": name,
                                        "affiliation": "project-affiliated"}
                for pid in ids:
                    doc["pairs"][pid]["reviews"][owner] = {
                        "verdict": "accept", "criteria": ["DEP"], "notes": ""}
                    doc["pairs"][pid]["split"] = "dev" if pid == ids[2] else "sealed"
                files.append(_write(h.root, f"REVIEW-{owner}.json", doc))

            merged = seal_prescreen.merge_reviews(files)
            review_path = h.root / "REVIEW.json"
            review_path.write_text(_harness.pretty_json(merged), encoding="utf-8")
            manifest = json.loads(h.promote(review_path).read_text(encoding="utf-8"))

        self.assertEqual(manifest["schema_version"], seal.SEAL_SCHEMA_VERSION_V2)
        self.assertEqual(manifest["sealed_count"], 2)
        self.assertEqual(manifest["dev_count"], 1)
        self.assertEqual(manifest["seal_v2"]["n_dually_reviewed_pairs"], 3)


class MergeRefusalTest(unittest.TestCase):
    def _merge(self, docs: list[dict]) -> None:
        with tempfile.TemporaryDirectory() as raw:
            tmp = pathlib.Path(raw)
            paths = [_write(tmp, f"f{i}.json", d) for i, d in enumerate(docs)]
            seal_prescreen.merge_reviews(paths)

    def test_refuses_a_file_with_two_owners(self):
        ids = ["a__then__b", "c__then__d"]
        both = _rater_copy(ids, "r1", {i: "accept" for i in ids})
        for pid in ids:  # r1's file also fills r2's column
            both["pairs"][pid]["reviews"]["r2"] = {
                "verdict": "accept", "criteria": ["DEP"], "notes": ""}
        other = _rater_copy(ids, "r2", {i: "accept" for i in ids})
        with self.assertRaises(SystemExit) as ctx:
            self._merge([both, other])
        self.assertIn("exactly one rater id", str(ctx.exception))

    def test_refuses_an_unstarted_file(self):
        ids = ["a__then__b"]
        with self.assertRaises(SystemExit) as ctx:
            self._merge([_template(ids), _rater_copy(ids, "r2", {ids[0]: "accept"})])
        self.assertIn("no rater", str(ctx.exception))

    def test_refuses_two_files_from_the_same_rater(self):
        ids = ["a__then__b"]
        with self.assertRaises(SystemExit) as ctx:
            self._merge([_rater_copy(ids, "r1", {ids[0]: "accept"}),
                         _rater_copy(ids, "r1", {ids[0]: "reject"})])
        self.assertIn("one file per rater", str(ctx.exception))

    def test_refuses_mismatched_rater_blocks(self):
        ids = ["a__then__b"]
        with self.assertRaises(SystemExit) as ctx:
            self._merge([_rater_copy(ids, "r1", {ids[0]: "accept"}),
                         _rater_copy(ids, "x2", {ids[0]: "accept"},
                                     raters=("r1", "x2"))])
        self.assertIn("expected", str(ctx.exception))

    def test_refuses_conflicting_splits(self):
        ids = ["a__then__b", "c__then__d"]
        a = _rater_copy(ids, "r1", {i: "accept" for i in ids},
                        splits={ids[0]: "sealed", ids[1]: "sealed"})
        b = _rater_copy(ids, "r2", {i: "accept" for i in ids},
                        splits={ids[0]: "dev", ids[1]: "sealed"})
        with self.assertRaises(SystemExit) as ctx:
            self._merge([a, b])
        self.assertIn("disagree", str(ctx.exception))

    def test_refuses_more_or_fewer_than_two_files(self):
        ids = ["a__then__b"]
        with self.assertRaises(SystemExit):
            seal_prescreen.merge_reviews([pathlib.Path("only-one.json")])

    def test_refuses_a_missing_file(self):
        ids = ["a__then__b"]
        with tempfile.TemporaryDirectory() as raw:
            tmp = pathlib.Path(raw)
            good = _write(tmp, "good.json", _rater_copy(ids, "r1", {ids[0]: "accept"}))
            with self.assertRaises(SystemExit) as ctx:
                seal_prescreen.merge_reviews([good, tmp / "absent.json"])
            self.assertIn("not found", str(ctx.exception))


class MergeCliTest(unittest.TestCase):
    def test_cli_writes_the_merged_file_and_refuses_to_overwrite(self):
        ids = ["a__then__b", "c__then__d"]
        with tempfile.TemporaryDirectory() as raw:
            tmp = pathlib.Path(raw)
            a = _write(tmp, "REVIEW-r1.json", _rater_copy(
                ids, "r1", {i: "accept" for i in ids},
                splits={ids[0]: "sealed", ids[1]: "dev"}))
            b = _write(tmp, "REVIEW-r2.json", _rater_copy(
                ids, "r2", {i: "accept" for i in ids}))
            out = tmp / "REVIEW.json"
            argv = ["--merge-reviews", str(a), str(b), "--merge-out", str(out)]
            with contextlib.redirect_stdout(io.StringIO()) as buf:
                self.assertEqual(seal_prescreen.main(argv), 0)
            self.assertIn("dually reviewed", buf.getvalue())
            self.assertTrue(out.is_file())

            with self.assertRaises(SystemExit) as ctx:
                seal_prescreen.main(argv)
            self.assertIn("exists", str(ctx.exception))


if __name__ == "__main__":
    unittest.main()
