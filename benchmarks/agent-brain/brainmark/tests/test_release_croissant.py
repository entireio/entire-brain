"""release/croissant_gen.py: markdown section extraction, Croissant document
shape, Responsible-AI fields from a fixture SEAL-MANIFEST, and the
import-guarded validator (mlcroissant if importable, structural fallback
otherwise -- both return the same {"validator","ok","errors"} shape)."""

from __future__ import annotations

import pathlib
import sys
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2]))

from brainmark.release import croissant_gen as cg  # noqa: E402


FIXTURE_DATASHEET = """# Datasheet -- the BrainMark pair dataset

## Motivation

Measures whether a second coding session reuses what the first learned.

## Collection process

Mined deterministically and offline by mine_pairs.py.

## Preprocessing / labeling

Human review is a gate, not an annotation.

## Uses

Intended: measuring cross-session memory reuse.
Not appropriate: training, claiming general coding ability.

## Distribution and licensing

SWE-bench is MIT-licensed; no upstream content is redistributed.

## Known limitations (stated up front)

1. n may be short of the pre-registered floor.
2. Two repositories only.
"""


def _fixture_manifest() -> dict:
    return {
        "schema_version": 2,
        "sealed_count": 30,
        "dev_count": 10,
        "tasks": {
            "sealed/a0__then__b0.json": {"sha256": "a" * 64, "split": "sealed", "bytes": 512},
            "dev/d0__then__e0.json": {"sha256": "b" * 64, "split": "dev", "bytes": 480},
        },
        "miner_sha256": "c" * 64,
        "config_sha256": "d" * 64,
        "prereg_sha256": "e" * 64,
        "mechmetrics_sha256": "f" * 64,
        "vendored_metrics_sha256": "0" * 64,
        "seal_v2": {
            "raters": {"rater_1": {"name": "A", "affiliation": "project-affiliated"},
                      "rater_2": {"name": "B", "affiliation": "project-affiliated"}},
            "n_dually_reviewed_pairs": 42,
            "cohens_kappa": 0.7333,
            "observed_agreement": 0.9,
            "expected_agreement": 0.625,
            "disagreements_adjudicated": 3,
        },
    }


class MarkdownSectionsTest(unittest.TestCase):
    def test_extracts_known_headings(self):
        sections = cg.parse_markdown_sections(FIXTURE_DATASHEET)
        self.assertIn("Measures whether a second coding session", sections["Motivation"])
        self.assertIn("Human review is a gate", sections["Preprocessing / labeling"])
        self.assertIn("n may be short", sections["Known limitations (stated up front)"])

    def test_missing_heading_is_simply_absent(self):
        sections = cg.parse_markdown_sections("# Title\n\nno subheadings here\n")
        self.assertNotIn("Motivation", sections)


class BuildCroissantTest(unittest.TestCase):
    def test_document_has_required_top_level_fields(self):
        doc = cg.build_croissant(_fixture_manifest(), FIXTURE_DATASHEET)
        for key in cg.REQUIRED_TOP_LEVEL:
            self.assertIn(key, doc, f"missing {key}")
        self.assertEqual(doc["@type"], "sc:Dataset")
        self.assertIn("second coding session", doc["description"])

    def test_distribution_carries_the_seal_manifest_sha256s(self):
        doc = cg.build_croissant(_fixture_manifest(), FIXTURE_DATASHEET)
        shas = {f["sha256"] for f in doc["distribution"]}
        self.assertIn("a" * 64, shas)
        self.assertIn("b" * 64, shas)

    def test_responsible_ai_carries_kappa_and_disclosure(self):
        doc = cg.build_croissant(_fixture_manifest(), FIXTURE_DATASHEET)
        rai = doc["rai"]
        self.assertEqual(
            rai["rai:dataAnnotationPlatform"]["interRaterReliability"]["value"], 0.7333)
        self.assertIn("teammate", rai["rai:dataAnnotationPlatform"]["raterDisclosure"])
        self.assertIn("training", rai["rai:dataUseCases"])
        self.assertIn("n may be short", rai["rai:dataLimitations"])
        self.assertIn("No", rai["rai:personalSensitiveInformation"])

    def test_v1_manifest_without_seal_v2_still_builds(self):
        manifest = _fixture_manifest()
        del manifest["seal_v2"]
        doc = cg.build_croissant(manifest, FIXTURE_DATASHEET)
        self.assertEqual(doc["rai"]["rai:dataAnnotationPlatform"]["reviewerCount"], 1)
        self.assertNotIn("interRaterReliability", doc["rai"]["rai:dataAnnotationPlatform"])


class ValidateTest(unittest.TestCase):
    def test_structural_validator_passes_a_well_formed_document(self):
        doc = cg.build_croissant(_fixture_manifest(), FIXTURE_DATASHEET)
        result = cg._structural_validate(doc)
        self.assertTrue(result["ok"], result["errors"])

    def test_structural_validator_catches_missing_required_field(self):
        doc = cg.build_croissant(_fixture_manifest(), FIXTURE_DATASHEET)
        del doc["license"]
        result = cg._structural_validate(doc)
        self.assertFalse(result["ok"])
        self.assertTrue(any("license" in e for e in result["errors"]))

    def test_structural_validator_catches_missing_content_hash(self):
        doc = cg.build_croissant(_fixture_manifest(), FIXTURE_DATASHEET)
        del doc["distribution"][0]["sha256"]
        result = cg._structural_validate(doc)
        self.assertFalse(result["ok"])
        self.assertTrue(any("sha256" in e for e in result["errors"]))

    def test_structural_validator_catches_empty_record_set(self):
        doc = cg.build_croissant(_fixture_manifest(), FIXTURE_DATASHEET)
        doc["recordSet"] = []
        result = cg._structural_validate(doc)
        self.assertFalse(result["ok"])

    def test_validate_dispatches_to_structural_when_mlcroissant_unavailable(self):
        # This environment does not have mlcroissant installed (checked at
        # test-authoring time); validate() must transparently fall back.
        doc = cg.build_croissant(_fixture_manifest(), FIXTURE_DATASHEET)
        result = cg.validate(doc)
        self.assertIn(result["validator"], ("structural", "mlcroissant"))
        self.assertTrue(result["ok"], result["errors"])


if __name__ == "__main__":
    unittest.main()
