#!/usr/bin/env python3

from __future__ import annotations

import copy
import importlib.util
import json
import pathlib
import sys
import tempfile
import unittest


SCRIPT = pathlib.Path(__file__).with_name("mined-scale") / "check_manifest.py"
SPEC = importlib.util.spec_from_file_location("mined_scale_manifest", SCRIPT)
manifest = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
sys.modules[SPEC.name] = manifest
SPEC.loader.exec_module(manifest)


class MinedScaleManifestTests(unittest.TestCase):
    def test_checked_in_manifest_matches_corpus(self) -> None:
        value = manifest.validate_manifest()
        self.assertEqual(value["inventory"]["task_count"], 73)
        self.assertEqual(value["inventory"]["patch_count"], 73)
        self.assertEqual(
            value["population"]["task_count_by_repo"],
            {"entire-cli": 52, "entire-db": 21},
        )

    def test_rehashed_manifest_tamper_still_fails_against_corpus(self) -> None:
        value = manifest.build_manifest_payload()
        tampered = copy.deepcopy(value)
        tampered["tasks"][0]["config"]["sha256"] = "0" * 64
        tampered = manifest.attach_self_hash(tampered)
        with tempfile.TemporaryDirectory() as tmp:
            path = pathlib.Path(tmp) / "manifest.json"
            path.write_text(json.dumps(tampered))
            with self.assertRaisesRegex(manifest.ManifestError, "does not match"):
                manifest.validate_manifest(manifest.ROOT, path)

    def test_unrehashed_manifest_tamper_fails_self_hash(self) -> None:
        value = manifest.build_manifest_payload()
        value["inventory"]["task_count"] = 72
        with tempfile.TemporaryDirectory() as tmp:
            path = pathlib.Path(tmp) / "manifest.json"
            path.write_text(json.dumps(value))
            with self.assertRaisesRegex(manifest.ManifestError, "self-hash mismatch"):
                manifest.validate_manifest(manifest.ROOT, path)


if __name__ == "__main__":
    unittest.main()
