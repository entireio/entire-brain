"""release/make_release.py: substitution pass, grep-gate, and end-to-end
anonymization on a controlled fixture tree -- plus a report of how many hits
the SAME gate finds against this repo's own current (un-anonymized) working
tree, which is expected and is the point (extends the parent directory's
`../test_publication_safety.py` / `../PUBLICATION-SANITIZATION.md` doctrine:
grep-gate the output, don't trust the tool that produced it)."""

from __future__ import annotations

import pathlib
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2]))

from brainmark import _harness  # noqa: E402
from brainmark.release import make_release  # noqa: E402


class AnonymizeTextTest(unittest.TestCase):
    def test_full_brain_becomes_system_x(self):
        text, n = make_release.anonymize_text("arm=full_brain, FULL_BRAIN, full-brain")
        self.assertEqual(text, "arm=system_x, SYSTEM_X, system-x")
        self.assertEqual(n, 3)

    def test_no_brain_becomes_baseline(self):
        text, _ = make_release.anonymize_text("no_brain vs NO_BRAIN vs no-brain")
        self.assertEqual(text, "baseline vs BASELINE vs no-memory-baseline")

    def test_entire_brain_product_name_becomes_system_x(self):
        text, _ = make_release.anonymize_text("run `entire-brain distill` and `Entire_Brain refresh`")
        self.assertIn("system_x distill", text)
        self.assertIn("system_x refresh", text)
        self.assertNotIn("entire", text.lower())

    def test_org_identifiers_are_replaced(self):
        text, _ = make_release.anonymize_text("hosted at entireHQ, see entire.io and entireio docs")
        self.assertNotIn("entirehq", text.lower())
        self.assertNotIn("entireio", text.lower())
        self.assertNotIn("entire.io", text.lower())

    def test_personal_identity_is_replaced(self):
        text, _ = make_release.anonymize_text("Reviewed by Suhaan Thayyil on 2026-08-19.")
        self.assertNotIn("Suhaan", text)
        self.assertNotIn("Thayyil", text)

    def test_absolute_home_paths_are_replaced(self):
        text, _ = make_release.anonymize_text(
            'graphmark_root: "/Users/suhaan/devenv/graphmark/agentic-swebench"')
        self.assertNotIn("/Users/", text)
        self.assertIn("<REPO_ROOT>", text)

    def test_competitor_names_are_left_alone(self):
        # mem0/graphify/cmm are third-party products, not Entire's IP -- the
        # substitution pass must not touch them.
        text, n = make_release.anonymize_text("arms: mem0, graphify, cmm, full_brain, no_brain")
        self.assertIn("mem0", text)
        self.assertIn("graphify", text)
        self.assertIn("cmm", text)
        self.assertGreaterEqual(n, 2)  # only full_brain/no_brain substituted


class ScanForbiddenTest(unittest.TestCase):
    def test_catches_entirehq_entireio_suhaan_devenv(self):
        findings = make_release.scan_forbidden(
            "hosted on entireHQ, config in devenv, contact suhaan, see entireio.example")
        words = {f["word"] for f in findings}
        self.assertIn("entirehq", words)
        self.assertIn("devenv", words)
        self.assertIn("suhaan", words)
        self.assertIn("entireio", words)

    def test_catches_bare_brain_substring(self):
        findings = make_release.scan_forbidden("the full_brain arm")
        self.assertTrue(any(f["word"] == "brain" for f in findings))

    def test_allowlist_permits_brainmark_and_entirely(self):
        findings = make_release.scan_forbidden("BrainMark measures this entirely fairly")
        self.assertEqual(findings, [])

    def test_case_insensitive(self):
        findings = make_release.scan_forbidden("DEVENV SUHAAN ENTIREHQ")
        # ENTIREHQ matches both "entire" and "entirehq" -- 4 findings, 3 tokens.
        self.assertEqual(len(findings), 4)
        self.assertEqual({f["context_word"] for f in findings}, {"DEVENV", "SUHAAN", "ENTIREHQ"})

    def test_clean_text_has_no_findings(self):
        self.assertEqual(make_release.scan_forbidden("system_x beat baseline on locate calls"), [])


class BuildReleaseFixtureTest(unittest.TestCase):
    """A small, fully controlled source tree -> anonymized copy -> gate MUST
    find zero hits. This is the one case where "clean output" is a fair bar:
    the fixture is small enough that every identifying string in it is one
    the substitution pass is designed to catch."""

    def _make_fixture(self, root: pathlib.Path) -> pathlib.Path:
        src = root / "brainmark"
        src.mkdir()
        (src / "config.json").write_text(_harness.pretty_json({
            "graphmark_root": "/Users/suhaan/devenv/graphmark/agentic-swebench",
            "arms": ["no_brain", "full_brain", "mem0", "graphify", "cmm"],
        }), encoding="utf-8")
        (src / "README.md").write_text(
            "# BrainMark\n\nBuilt by Suhaan Thayyil at entireHQ. See entire.io.\n"
            "Compares full_brain (entire-brain) against no_brain, mem0, graphify, cmm.\n",
            encoding="utf-8",
        )
        (src / "notes.py").write_text(
            '"""devenv-local notes, see /Users/suhaan/devenv for paths."""\n',
            encoding="utf-8",
        )
        results = src / "results"
        results.mkdir()
        (results / "raw.json").write_text('{"private": "suhaan artifact, not released"}',
                                          encoding="utf-8")
        (src / "REVIEW.json").write_text('{"reviewer": "Suhaan Thayyil"}', encoding="utf-8")
        return src

    def test_anonymized_output_passes_the_gate(self):
        with tempfile.TemporaryDirectory() as raw:
            root = pathlib.Path(raw)
            src = self._make_fixture(root)
            dst = root / "release"
            manifest = make_release.build_release(src, dst)

            self.assertGreater(manifest["substitutions_total"], 0)
            # results/ and REVIEW.json must not be released at all.
            released_names = {f["path"] for f in manifest["files"]}
            self.assertFalse(any(p.startswith("results/") for p in released_names))
            self.assertNotIn("REVIEW.json", released_names)

            hits = make_release.scan_tree(dst)
            self.assertEqual(hits, {}, hits)

    def test_refuses_to_write_into_a_nonempty_destination(self):
        with tempfile.TemporaryDirectory() as raw:
            root = pathlib.Path(raw)
            src = self._make_fixture(root)
            dst = root / "release"
            dst.mkdir()
            (dst / "existing.txt").write_text("already here", encoding="utf-8")
            with self.assertRaises(SystemExit):
                make_release.build_release(src, dst)


class CurrentTreeGateTest(unittest.TestCase):
    """The gate is SUPPOSED to flag this repo's own un-anonymized working
    tree -- that is the gate proving it is not a no-op. This test asserts
    the direction (non-empty), not an exact count (the tree is actively
    edited by other agents in parallel; an exact count would be flaky by
    construction)."""

    def test_gate_flags_the_current_unanonymized_brainmark_tree(self):
        hits = make_release.scan_tree(_harness.BRAINMARK_DIR)
        total = sum(len(v) for v in hits.values())
        self.assertGreater(total, 0, "grep-gate found nothing in the real tree -- "
                                     "either the tree is already anonymized, or the gate is broken")


if __name__ == "__main__":
    unittest.main()
