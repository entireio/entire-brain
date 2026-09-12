"""Seal round-trip, and report.py refusing to aggregate on a hash mismatch."""

from __future__ import annotations

import json
import pathlib
import shutil
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2]))

from brainmark import _harness, report, seal  # noqa: E402


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


class SealHarness:
    """A throwaway brainmark root with the files seal.py hashes."""

    def __init__(self, tmp: pathlib.Path, n_sealed: int = 2, n_dev: int = 1) -> None:
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
        pair_ids = ([f"a{i}__then__b{i}" for i in range(n_sealed)]
                    + [f"d{i}__then__e{i}" for i in range(n_dev)])
        for pair_id in pair_ids:
            (self.candidates / f"{pair_id}.json").write_text(
                _harness.pretty_json(_pair(pair_id)), encoding="utf-8")
        (self.candidates / "INDEX.json").write_text(
            _harness.pretty_json({"candidate_pair_ids": pair_ids}), encoding="utf-8")

        self.review = self.root / "REVIEW.json"
        self.review.write_text(_harness.pretty_json({
            "reviewer": "test-reviewer", "reviewed_at": "2026-08-18",
            "pairs": {
                pid: {"accept": True,
                      "split": "dev" if pid.startswith("d") else "sealed",
                      "note": ""}
                for pid in pair_ids
            },
        }), encoding="utf-8")

        self.config = json.loads((real / "config.json").read_text(encoding="utf-8"))
        self.config["seal"] = {"sealed_min": n_sealed, "dev_split": n_dev,
                               "prereg_path": "PREREGISTRATION.md"}
        self.config_path = self.root / "test-config.json"
        self.config_path.write_text(_harness.pretty_json(self.config), encoding="utf-8")

    def promote(self) -> None:
        original = _harness.BRAINMARK_DIR
        _harness.BRAINMARK_DIR = self.root
        seal._harness.BRAINMARK_DIR = self.root
        try:
            args = type("A", (), {
                "candidates": str(self.candidates), "review": str(self.review),
                "config": str(self.config_path), "allow_short": False,
            })()
            seal.cmd_promote(args)
        finally:
            _harness.BRAINMARK_DIR = original
            seal._harness.BRAINMARK_DIR = original


class SealRoundTripTest(unittest.TestCase):
    def test_promote_then_verify_passes(self):
        with tempfile.TemporaryDirectory() as raw:
            harness = SealHarness(pathlib.Path(raw))
            harness.promote()
            manifest = json.loads((harness.root / seal.MANIFEST_NAME).read_text(encoding="utf-8"))
            self.assertEqual(manifest["sealed_count"], 2)
            self.assertEqual(manifest["dev_count"], 1)
            self.assertEqual(manifest["reviewer"], "test-reviewer")
            ok, problems = seal.verify(harness.root)
            self.assertTrue(ok, problems)

    def test_verify_detects_an_edited_task(self):
        with tempfile.TemporaryDirectory() as raw:
            harness = SealHarness(pathlib.Path(raw))
            harness.promote()
            victim = next((harness.root / "tasks" / "sealed").glob("*.json"))
            payload = json.loads(victim.read_text(encoding="utf-8"))
            payload["score"] = 0.99
            victim.write_text(_harness.pretty_json(payload), encoding="utf-8")

            ok, problems = seal.verify(harness.root)
            self.assertFalse(ok)
            self.assertTrue(any("MODIFIED" in p for p in problems), problems)

    def test_verify_detects_an_unsealed_task_slipped_in(self):
        with tempfile.TemporaryDirectory() as raw:
            harness = SealHarness(pathlib.Path(raw))
            harness.promote()
            (harness.root / "tasks" / "sealed" / "sneaky__then__extra.json").write_text(
                _harness.pretty_json(_pair("sneaky__then__extra")), encoding="utf-8")
            ok, problems = seal.verify(harness.root)
            self.assertFalse(ok)
            self.assertTrue(any("UNSEALED" in p for p in problems), problems)

    def test_verify_detects_a_changed_metric_definition(self):
        with tempfile.TemporaryDirectory() as raw:
            harness = SealHarness(pathlib.Path(raw))
            harness.promote()
            path = harness.root / "mechmetrics.py"
            path.write_text(path.read_text(encoding="utf-8") + "\n# post-seal edit\n",
                            encoding="utf-8")
            ok, problems = seal.verify(harness.root)
            self.assertFalse(ok)
            self.assertTrue(any("mechmetrics.py CHANGED" in p for p in problems), problems)

    def test_promote_refuses_without_a_reviewer(self):
        with tempfile.TemporaryDirectory() as raw:
            harness = SealHarness(pathlib.Path(raw))
            payload = json.loads(harness.review.read_text(encoding="utf-8"))
            payload["reviewer"] = ""
            harness.review.write_text(_harness.pretty_json(payload), encoding="utf-8")
            with self.assertRaises(SystemExit):
                harness.promote()

    def test_verify_reports_an_unsealed_tree(self):
        with tempfile.TemporaryDirectory() as raw:
            root = pathlib.Path(raw) / "empty"
            root.mkdir()
            ok, problems = seal.verify(root)
            self.assertFalse(ok)
            self.assertTrue(any("never sealed" in p for p in problems), problems)


class ReportRefusalTest(unittest.TestCase):
    """report.py must refuse rather than quietly aggregate a broken run."""

    def _cell(self, cell: pathlib.Path, arm: str, packet_text: str, prompt: str) -> None:
        cell.mkdir(parents=True, exist_ok=True)
        (cell / "packet.txt").write_text(packet_text, encoding="utf-8")
        (cell / "packet.sha256").write_text(_harness.sha256_text(packet_text) + "\n",
                                            encoding="utf-8")
        (cell / "prompt.txt").write_text(prompt, encoding="utf-8")
        (cell / "patch.diff").write_text("diff --git a/x b/x\n", encoding="utf-8")
        (cell / "meta.json").write_text(_harness.pretty_json({
            "instance_id": f"iid-{cell.parent.name}", "arm": arm,
            "result_event": {"total_cost_usd": 0.1},
            "mechmetrics": {"locate_calls_pre_edit": 3, "tokens": {"total_tokens": 100}},
        }), encoding="utf-8")

    def test_packet_sha_mismatch_is_blocking(self):
        from brainmark import prompts

        with tempfile.TemporaryDirectory() as raw:
            results = pathlib.Path(raw) / "B"
            pair = results / "p1"
            arms = ["no_brain", "full_brain"]
            for arm in arms:
                packet = f'{{"results":[],"arm":"{arm}"}}'
                self._cell(pair / arm, arm, packet, prompts.build_prompt("issue", packet))
            # Tamper: rewrite the delivered bytes after the pin was taken.
            (pair / "full_brain" / "packet.txt").write_text('{"results":["TAMPERED"]}',
                                                            encoding="utf-8")
            problems, _ = report.check_integrity(results, arms, require_seal=False)
            self.assertTrue(any("packet.sha256 mismatch" in p for p in problems), problems)

    def test_symmetry_violation_is_blocking_and_re_derived(self):
        from brainmark import prompts

        with tempfile.TemporaryDirectory() as raw:
            results = pathlib.Path(raw) / "B"
            pair = results / "p1"
            arms = ["no_brain", "full_brain"]
            for arm in arms:
                packet = f'{{"results":[],"arm":"{arm}"}}'
                self._cell(pair / arm, arm, packet, prompts.build_prompt("issue", packet))
            # Give one arm different INSTRUCTIONS after the fact. The recorded
            # prompt_sym.sha256 is deliberately NOT updated -- report.py must
            # re-derive from prompt.txt and catch it anyway.
            victim = pair / "full_brain" / "prompt.txt"
            victim.write_text(victim.read_text(encoding="utf-8").replace(
                "--- ISSUE ---", "--- ISSUE (hint: check src/widget.py) ---"), encoding="utf-8")

            problems, _ = report.check_integrity(results, arms, require_seal=False)
            self.assertTrue(any("SYMMETRY VIOLATION" in p for p in problems), problems)

    def test_clean_run_has_no_blocking_problems(self):
        from brainmark import prompts

        with tempfile.TemporaryDirectory() as raw:
            results = pathlib.Path(raw) / "B"
            arms = ["no_brain", "full_brain"]
            for pair_name in ("p1", "p2"):
                for arm in arms:
                    packet = f'{{"results":[],"arm":"{arm}"}}'
                    self._cell(results / pair_name / arm, arm, packet,
                               prompts.build_prompt("issue", packet))
            problems, _ = report.check_integrity(results, arms, require_seal=False)
            self.assertEqual(problems, [])

    def test_refused_report_renders_and_exits_nonzero_shape(self):
        from brainmark import prompts

        with tempfile.TemporaryDirectory() as raw:
            results = pathlib.Path(raw) / "B"
            pair = results / "p1"
            arms = ["no_brain", "full_brain"]
            for arm in arms:
                packet = f'{{"results":[],"arm":"{arm}"}}'
                self._cell(pair / arm, arm, packet, prompts.build_prompt("issue", packet))
            (pair / "full_brain" / "packet.txt").write_text("TAMPERED", encoding="utf-8")

            config = _harness.load_config()
            built = report.build_report(results, config, arms, require_seal=False)
            self.assertTrue(built["refused"])
            markdown = report.render_markdown(built)
            self.assertIn("REFUSED", markdown)
            self.assertNotIn("Headline", markdown)


if __name__ == "__main__":
    unittest.main()
