"""seal.verify() must recompute every pin, or say it could not.

report.py aggregates only when `seal.verify()` returns ok, and cmd_verify
prints "SEAL OK: every sealed task and pinned file matches the manifest". Both
statements were stronger than the code:

  * every code-hash check was guarded `if manifest.get(key) and actual != ...`,
    so a manifest carrying `"prompts_sha256": ""` -- or simply missing the key --
    skipped that check in silence and still reported SEAL OK;
  * `config_sha256` was written at seal time and NEVER recompared. config.json
    carries `arms`, `packet.max_bytes`, `packet.top_k`, `seeds`,
    `brain.distill` (the paid model) and `agent.backend`. All of it was
    post-hoc editable behind a green seal;
  * the pre-registration check was `if prereg.is_file() and ...`, so DELETING
    PREREGISTRATION.md removed the check rather than failing it -- and the path
    was hardcoded while promote() takes it from `config["seal"]["prereg_path"]`.

A seal that cannot be recomputed must not read as a seal that was.
"""

from __future__ import annotations

import json
import pathlib
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2]))

from brainmark import _harness, seal  # noqa: E402
from brainmark.tests.test_seal_and_report import SealHarness  # noqa: E402


class SealVerifyIsNotVacuousTest(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = pathlib.Path(tempfile.mkdtemp())
        self.harness = SealHarness(self.tmp)
        self.harness.promote()
        self.root = self.harness.root
        self.manifest_path = self.root / seal.MANIFEST_NAME

    def _manifest(self) -> dict:
        return json.loads(self.manifest_path.read_text(encoding="utf-8"))

    def _write(self, manifest: dict) -> None:
        self.manifest_path.write_text(_harness.pretty_json(manifest), encoding="utf-8")

    def test_the_round_trip_still_verifies(self):
        ok, problems = seal.verify(self.root)
        self.assertTrue(ok, problems)

    def test_a_blanked_pin_does_not_silently_skip_its_check(self):
        manifest = self._manifest()
        manifest["prompts_sha256"] = ""
        self._write(manifest)
        path = self.root / "prompts.py"
        path.write_text(path.read_text(encoding="utf-8") + "\n# post-seal edit\n",
                        encoding="utf-8")
        ok, problems = seal.verify(self.root)
        self.assertFalse(ok, "a blank pin let an edited prompts.py verify")
        self.assertTrue(any("prompts_sha256" in p for p in problems), problems)

    def test_a_deleted_pin_key_is_a_problem(self):
        for key in ("miner_sha256", "mechmetrics_sha256", "vendored_metrics_sha256"):
            with self.subTest(key=key):
                manifest = self._manifest()
                manifest.pop(key)
                self._write(manifest)
                ok, problems = seal.verify(self.root)
                self.assertFalse(ok)
                self.assertTrue(any(key in p for p in problems), problems)

    def test_editing_the_sealed_config_is_detected(self):
        config = json.loads(self.harness.config_path.read_text(encoding="utf-8"))
        config["packet"]["max_bytes"] = 999999
        self.harness.config_path.write_text(_harness.pretty_json(config), encoding="utf-8")
        ok, problems = seal.verify(self.root)
        self.assertFalse(ok, "the packet byte budget was changed after sealing")
        self.assertTrue(any("CHANGED since seal" in p for p in problems), problems)

    def test_deleting_the_prereg_is_a_failure_not_a_skip(self):
        (self.root / "PREREGISTRATION.md").unlink()
        ok, problems = seal.verify(self.root)
        self.assertFalse(ok, "deleting the pre-registration removed its check")
        self.assertTrue(any("PREREGISTRATION.md" in p for p in problems), problems)

    def test_editing_the_prereg_is_detected(self):
        path = self.root / "PREREGISTRATION.md"
        path.write_text(path.read_text(encoding="utf-8") + "\nnew endpoint\n",
                        encoding="utf-8")
        ok, problems = seal.verify(self.root)
        self.assertFalse(ok)
        self.assertTrue(any("PREREGISTRATION.md CHANGED" in p for p in problems), problems)

    def test_a_manifest_that_pins_a_hash_without_its_path_is_unverifiable(self):
        manifest = self._manifest()
        manifest.pop("config_path")
        self._write(manifest)
        ok, problems = seal.verify(self.root)
        self.assertFalse(ok)
        self.assertTrue(any("config_path" in p for p in problems), problems)

    def test_the_manifest_records_the_paths_its_hashes_are_of(self):
        manifest = self._manifest()
        self.assertTrue(manifest["config_path"])
        self.assertTrue(manifest["prereg_path"])
        self.assertEqual(
            _harness.sha256_file(self.root / manifest["config_path"]),
            manifest["config_sha256"],
        )


if __name__ == "__main__":
    unittest.main()
