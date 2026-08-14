from __future__ import annotations

import copy
import json
import os
import pathlib
import subprocess
import tempfile
import unittest

import task_eligibility as eligibility
import relevance_dataset


class TaskEligibilityScanTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.repo = pathlib.Path(self.temporary.name) / "repo"
        self.repo.mkdir()
        self.git("init", "-q")
        self.git("config", "user.name", "Synthetic Reviewer")
        self.git("config", "user.email", "synthetic@example.invalid")
        package = self.repo / "pkg"
        package.mkdir()
        (package / "code.go").write_text("package pkg\n\nfunc Value() int { return 1 }\n", encoding="utf-8")
        (package / "code_test.go").write_text(
            "package pkg\n\nimport \"testing\"\n\nfunc TestValue(t *testing.T) {}\n",
            encoding="utf-8",
        )
        self.commit("base", "2026-07-16T00:00:00+00:00")
        self.base = self.rev("HEAD")

        (package / "code.go").write_text("package pkg\n\nfunc Value() int { return 2 }\n", encoding="utf-8")
        self.commit("source only", "2026-07-16T00:01:00+00:00")

        (package / "code.go").write_text("package pkg\n\nfunc Value() int { return 3 }\n", encoding="utf-8")
        (package / "code_test.go").write_text(
            "package pkg\n\nimport \"testing\"\n\nfunc TestValue(t *testing.T) { if Value() != 3 { t.Fail() } }\n",
            encoding="utf-8",
        )
        self.commit("source and test with answer-bearing subject", "2026-07-16T00:02:00+00:00")
        self.head = self.rev("HEAD")

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def git(self, *args: str, env: dict[str, str] | None = None) -> str:
        completed = subprocess.run(
            ["git", *args],
            cwd=self.repo,
            check=True,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=env,
        )
        return completed.stdout.strip()

    def commit(self, subject: str, timestamp: str) -> None:
        self.git("add", ".")
        env = dict(os.environ)
        env["GIT_AUTHOR_DATE"] = timestamp
        env["GIT_COMMITTER_DATE"] = timestamp
        self.git("commit", "-q", "-m", subject, env=env)

    def rev(self, revision: str) -> str:
        return self.git("rev-parse", revision)

    def scan(self) -> dict:
        return eligibility.scan_repository(
            self.repo,
            base=self.base,
            head=self.head,
            repository_id="github.com/example/repo",
        )

    def test_scan_is_deterministic_and_excludes_source_only_units(self) -> None:
        first = self.scan()
        second = self.scan()
        self.assertEqual(first, second)
        self.assertEqual(first["summary"]["first_parent_unit_count"], 2)
        self.assertEqual(first["summary"]["source_plus_test_candidate_count"], 1)
        self.assertEqual(first["summary"]["negative_control_pending_count"], 1)
        candidate = first["candidates"][0]
        self.assertEqual(candidate["commit_oid"], self.head)
        self.assertEqual(candidate["negative_control_status"], "pending")
        eligibility.validate_scan(first)

    def test_scan_retains_commitments_but_not_paths_or_subjects(self) -> None:
        rendered = json.dumps(self.scan(), sort_keys=True)
        self.assertNotIn("pkg/code.go", rendered)
        self.assertNotIn("answer-bearing subject", rendered)
        self.assertIn("source_paths_sha256", rendered)
        self.assertEqual(self.scan()["exposure"], "development_only_identity_inspected")

    def test_semantic_tampering_fails_after_resealing(self) -> None:
        scan = self.scan()
        scan["candidates"][0]["static_scope_score"] += 1
        scan["ledger_sha256"] = eligibility._self_hash(scan)
        with self.assertRaisesRegex(eligibility.EligibilityError, "static score drift"):
            eligibility.validate_scan(scan)

        scan = self.scan()
        scan["candidates"][0]["commit_oid"] = "0" * 40
        scan["ledger_sha256"] = eligibility._self_hash(scan)
        with self.assertRaisesRegex(eligibility.EligibilityError, "commit_oid is invalid"):
            eligibility.validate_scan(scan)

    def test_self_hash_and_schema_identity_are_strict(self) -> None:
        scan = self.scan()
        scan["summary"]["first_parent_unit_count"] = 99
        with self.assertRaisesRegex(eligibility.EligibilityError, "self hash mismatch"):
            eligibility.validate_scan(scan)
        schema = json.loads(
            (
                pathlib.Path(eligibility.__file__).parent
                / "schemas"
                / "development-task-eligibility-scan-v1.schema.json"
            ).read_text(encoding="utf-8")
        )
        self.assertEqual(schema["properties"]["profile"]["const"], eligibility.PROFILE)
        self.assertEqual(schema["properties"]["exposure"]["const"], eligibility.EXPOSURE)
        self.assertEqual(relevance_dataset.validate_schema_instance(self.scan(), schema, "scan"), [])

    def test_resealed_metadata_and_summary_tampering_fails_closed(self) -> None:
        cases = (
            (lambda scan: scan.__setitem__("repository_id", "bad\nrepo"), "repository_id is invalid"),
            (lambda scan: scan.__setitem__("generated_at", "2026-07-16T00:00:00"), "UTC offset"),
            (
                lambda scan: scan["candidates"][0].__setitem__("committed_at", "not-a-time"),
                "committed_at is invalid",
            ),
            (
                lambda scan: scan["candidates"][0].__setitem__("merge_parent_count", 0),
                "merge-parent count is invalid",
            ),
            (lambda scan: scan["summary"].__setitem__("extra", 1), "summary fields differ"),
            (
                lambda scan: scan["summary"].__setitem__("first_parent_unit_count", True),
                "first_parent_unit_count is invalid",
            ),
        )
        for mutate, phrase in cases:
            with self.subTest(phrase=phrase):
                scan = self.scan()
                mutate(scan)
                scan["ledger_sha256"] = eligibility._self_hash(scan)
                with self.assertRaisesRegex(eligibility.EligibilityError, phrase):
                    eligibility.validate_scan(scan)


if __name__ == "__main__":
    unittest.main()
