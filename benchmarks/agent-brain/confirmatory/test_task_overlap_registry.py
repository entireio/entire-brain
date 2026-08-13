from __future__ import annotations

import copy
import json
import os
import pathlib
import shutil
import tempfile
import unittest

import relevance_dataset
import task_overlap_registry as registry


HERE = pathlib.Path(__file__).parent
REGISTRY_PATH = HERE / "development-task-overlap-registry-v1.json"
SCHEMA_PATH = HERE / "schemas" / "development-task-overlap-registry-v1.schema.json"
V2_PATHS = [
    HERE / "development-task-eligibility-entire-brain-v2.json",
    HERE / "development-task-eligibility-entire-db-v2.json",
    HERE / "development-task-eligibility-entire-graph-v2.json",
]


class TaskOverlapRegistryTest(unittest.TestCase):
    def load(self) -> dict:
        return json.loads(REGISTRY_PATH.read_text(encoding="utf-8"))

    def test_checked_registry_is_schema_valid_non_authoritative_and_exactly_disjoint(self) -> None:
        value = self.load()
        registry.validate_registry(value)
        schema = json.loads(SCHEMA_PATH.read_text(encoding="utf-8"))
        self.assertEqual(relevance_dataset.validate_schema_instance(value, schema, "registry"), [])
        self.assertEqual(value["summary"]["candidate_count"], 85)
        self.assertEqual(value["summary"]["exact_duplicate_group_count"], 0)
        self.assertEqual(value["exact_identity_profile"], registry.EXACT_IDENTITY_PROFILE)
        self.assertTrue(all(not groups for groups in value["duplicate_groups"].values()))
        self.assertEqual(value["authority"]["owner_hmac"], "absent_not_fabricated")
        self.assertTrue(all(item["identity_authority"] == "development_only_non_authoritative" for item in value["candidates"]))
        self.assertTrue(all(item["exact_identity_profile"] == registry.EXACT_IDENTITY_PROFILE for item in value["candidates"]))
        cli_records = [item for item in value["candidates"] if item["ledger_diff_profile"] == registry.CLI_LEDGER_DIFF_PROFILE]
        self.assertEqual(len(cli_records), 23)
        self.assertTrue(any(item["source_diff_sha256"] != item["source_ledger_diff_sha256"] for item in cli_records))

    def test_registry_rebuilds_from_exact_checked_inputs_when_cli_objects_are_available(self) -> None:
        cli_repo = pathlib.Path(os.environ.get("ENTIRE_CLI_AUDIT_REPO", pathlib.Path.home() / "Projects" / "entire-cli"))
        if not cli_repo.is_dir():
            self.skipTest("CLI object repository is unavailable")
        git_path = shutil.which("git")
        self.assertIsNotNone(git_path)
        registry.verify_registry_dependencies(
            self.load(),
            git_binary=pathlib.Path(git_path or "git").resolve(),
            cli_repo=cli_repo,
            cli_ledger_path=HERE / "development-task-eligibility-scan-v1.json",
            v2_ledger_paths=V2_PATHS,
            schema=SCHEMA_PATH,
        )

    def test_cli_repository_rebuild_rejects_resealed_parent_tampering(self) -> None:
        cli_repo = pathlib.Path(os.environ.get("ENTIRE_CLI_AUDIT_REPO", pathlib.Path.home() / "Projects" / "entire-cli"))
        if not cli_repo.is_dir():
            self.skipTest("CLI object repository is unavailable")
        git_path = shutil.which("git")
        self.assertIsNotNone(git_path)
        ledger = json.loads((HERE / "development-task-eligibility-scan-v1.json").read_text(encoding="utf-8"))
        ledger["candidates"][0]["parent_oid"] = ledger["head_oid"]
        ledger["ledger_sha256"] = registry.task_eligibility._self_hash(ledger)
        with self.assertRaisesRegex(registry.OverlapRegistryError, "actual first parent differs"):
            registry._cli_records(pathlib.Path(git_path or "git").resolve(), cli_repo, ledger)

    def test_resealed_duplicate_authority_and_summary_tampering_fail(self) -> None:
        cases = (
            (lambda value: value["authority"].__setitem__("owner_hmac", "invented"), "authority"),
            (lambda value: value["summary"].__setitem__("candidate_count", 1), "input counts|summary"),
            (lambda value: value["duplicate_groups"]["tree_oid"].append({"candidate_refs": ["1" * 64, "2" * 64], "value": "3" * 40}), "duplicate groups"),
            (lambda value: value["candidates"][0].__setitem__("identity_authority", "holdout"), "identity authority"),
            (lambda value: value["candidates"][0].__setitem__("extra", "schema bypass"), r"candidate\[0\] fields differ"),
            (lambda value: value["inputs"][0].__setitem__("extra", "schema bypass"), r"input\[0\] fields differ"),
        )
        for mutate, phrase in cases:
            with self.subTest(phrase=phrase):
                value = copy.deepcopy(self.load())
                mutate(value)
                value["registry_sha256"] = registry._self_hash(value)
                with self.assertRaisesRegex(registry.OverlapRegistryError, phrase):
                    registry.validate_registry(value)

    def test_duplicate_json_keys_are_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            path = pathlib.Path(temporary) / "duplicate.json"
            path.write_text('{"schema_version":1,"schema_version":2}', encoding="utf-8")
            with self.assertRaisesRegex(registry.OverlapRegistryError, "duplicate object key"):
                registry._load(path)


if __name__ == "__main__":
    unittest.main()
