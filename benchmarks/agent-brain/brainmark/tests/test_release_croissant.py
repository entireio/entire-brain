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
        # distribution also carries the FileSet the RecordSet fields source
        # from (cg.TASK_FILE_SET_ID) -- it has no per-file sha256, only the
        # individual FileObjects do.
        shas = {f["sha256"] for f in doc["distribution"] if "sha256" in f}
        self.assertIn("a" * 64, shas)
        self.assertIn("b" * 64, shas)

    def test_distribution_includes_the_task_file_set_the_fields_source_from(self):
        doc = cg.build_croissant(_fixture_manifest(), FIXTURE_DATASHEET)
        file_sets = [f for f in doc["distribution"] if f.get("@type") == "cr:FileSet"]
        self.assertEqual(len(file_sets), 1)
        self.assertEqual(file_sets[0]["@id"], cg.TASK_FILE_SET_ID)
        self.assertTrue(file_sets[0].get("includes"))

    def test_record_set_fields_all_declare_a_source(self):
        doc = cg.build_croissant(_fixture_manifest(), FIXTURE_DATASHEET)
        fields = doc["recordSet"][0]["field"]
        self.assertTrue(fields)
        for field in fields:
            source = field.get("source")
            self.assertIsInstance(source, dict, field)
            self.assertTrue(source.get("fileObject") or source.get("fileSet") or source.get("field"),
                            field)

    def test_file_objects_content_size_is_a_string(self):
        doc = cg.build_croissant(_fixture_manifest(), FIXTURE_DATASHEET)
        file_objects = [f for f in doc["distribution"] if f.get("@type") == "cr:FileObject"]
        self.assertTrue(file_objects)
        for obj in file_objects:
            self.assertIsInstance(obj["contentSize"], str, obj)

    def test_provenance_omits_unknown_hashes_instead_of_recording_null(self):
        # An all-null (or empty) "_provenance" object sent real mlcroissant's
        # JSON-LD expansion into unbounded recursion during manual
        # verification -- omitting unknown hashes (and the key entirely if
        # none are known) is the fix, not something the fallback validator
        # alone can catch, so this is a direct regression test on the shape.
        manifest = _fixture_manifest()
        for key in ("miner_sha256", "config_sha256", "prereg_sha256",
                   "mechmetrics_sha256", "vendored_metrics_sha256"):
            manifest[key] = None
        doc = cg.build_croissant(manifest, FIXTURE_DATASHEET)
        self.assertNotIn("_provenance", doc)

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

    def test_structural_validator_catches_a_field_with_no_source_or_value(self):
        # Regression test for the exact defect real mlcroissant caught: a
        # RecordSet field defining neither `source` nor `value`.
        doc = cg.build_croissant(_fixture_manifest(), FIXTURE_DATASHEET)
        del doc["recordSet"][0]["field"][0]["source"]
        result = cg._structural_validate(doc)
        self.assertFalse(result["ok"])
        self.assertTrue(any("source" in e and "value" in e for e in result["errors"]), result["errors"])

    def test_structural_validator_catches_non_string_content_size(self):
        # Regression test for the exact defect real mlcroissant caught:
        # `contentSize` typed as an int instead of `sc:Text`.
        doc = cg.build_croissant(_fixture_manifest(), FIXTURE_DATASHEET)
        doc["distribution"][0]["contentSize"] = 512
        result = cg._structural_validate(doc)
        self.assertFalse(result["ok"])
        self.assertTrue(any("contentSize" in e for e in result["errors"]), result["errors"])

    def test_validate_dispatches_to_structural_when_mlcroissant_unavailable(self):
        # This environment may or may not have mlcroissant installed;
        # validate() must transparently pick whichever path applies, and
        # either way the well-formed fixture document must validate clean.
        doc = cg.build_croissant(_fixture_manifest(), FIXTURE_DATASHEET)
        result = cg.validate(doc)
        self.assertIn(result["validator"], ("structural", "mlcroissant"))
        self.assertTrue(result["ok"], result["errors"])

    def test_validate_an_early_manifest_with_no_sealed_tasks_yet(self):
        # A manifest before any pair is sealed (`tasks: {}`, every hash
        # unknown) must still produce a document real mlcroissant accepts --
        # an all-null "_provenance" object previously sent its JSON-LD
        # expansion into unbounded recursion (see
        # test_provenance_omits_unknown_hashes_instead_of_recording_null).
        doc = cg.build_croissant({}, "")
        result = cg.validate(doc)
        self.assertTrue(result["ok"], result["errors"])


if __name__ == "__main__":
    unittest.main()
