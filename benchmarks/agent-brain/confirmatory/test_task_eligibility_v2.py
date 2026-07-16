from __future__ import annotations

import copy
import json
import os
import pathlib
import shutil
import subprocess
import tempfile
import unittest
from unittest import mock

import relevance_dataset
import task_eligibility_v2 as eligibility


HERE = pathlib.Path(__file__).parent


class TaskEligibilityV2Test(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.repo = pathlib.Path(self.temporary.name) / "repo"
        self.repo.mkdir()
        self.git("init", "-q", "-b", "main")
        self.git("config", "user.name", "Synthetic Reviewer")
        self.git("config", "user.email", "synthetic@example.invalid")
        self.write("go.mod", "module example.invalid/root\n\ngo 1.26\n")
        self.write("pkg/code.go", "package pkg\n\nfunc Value() int { return 1 }\n")
        self.write("pkg/code_test.go", "package pkg\n\nimport \"testing\"\n\nfunc TestValue(t *testing.T) {}\n")
        self.write("nested/go.mod", "module example.invalid/nested\n\ngo 1.25\n")
        self.write("nested/part/code.go", "package part\n\nfunc Value() int { return 1 }\n")
        self.write("nested/part/code_test.go", "package part\n\nimport \"testing\"\n\nfunc TestValue(t *testing.T) {}\n")
        self.commit("base", 0)
        self.base = self.rev("HEAD")

        self.write("pkg/code.go", "package pkg\n\nfunc Value() int { return 2 }\n")
        self.write("pkg/code_test.go", "package pkg\n\nimport \"testing\"\n\nfunc TestValue(t *testing.T) { if Value() != 2 { t.Fail() } }\n")
        self.write("pkg/testdata/helper.go", "package fixture\n")
        self.commit("mixed evidence", 1)

        self.git("checkout", "-q", "-b", "feature")
        self.write("pkg/code.go", "package pkg\n\nfunc Value() int { return 3 }\n")
        self.write("pkg/testdata/case.txt", "fixture\n")
        self.commit("fixture-only evidence", 2)
        self.git("checkout", "-q", "main")
        self.git("merge", "-q", "--no-ff", "feature", "-m", "merge fixture work")

        self.write("nested/part/code.go", "package part\n\nfunc Value() int { return 2 }\n")
        self.write("nested/part/code_test.go", "package part\n\nimport \"testing\"\n\nfunc TestValue(t *testing.T) { if Value() != 2 { t.Fail() } }\n")
        self.commit("nested module", 3)
        for index in range(4, 32):
            self.write("notes.txt", f"unit {index}\n")
            self.commit(f"filler {index}", index)
        self.head = self.rev("HEAD")
        git_path = shutil.which("git")
        self.assertIsNotNone(git_path)
        self.git_binary = pathlib.Path(git_path or "git").resolve()
        self.git("remote", "add", "origin", "https://github.com/example/repo.git")
        self.git("update-ref", "refs/remotes/origin/main", self.head)
        self.repository = {
            "key": "synthetic",
            "remote": {"name": "origin", "ref": "refs/remotes/origin/main", "url": "https://github.com/example/repo.git"},
            "repository_id": "github.com/example/repo",
            "toolchain": {
                "git": {
                    "binary_sha256": eligibility._sha256(self.git_binary.read_bytes()),
                    "version_output": self.git("--version"),
                },
                "go": {
                    "binary_sha256": "1" * 64, "cc": "cc", "cgo_enabled": "1", "cxx": "c++",
                    "goarch": "arch", "goos": "synthetic", "version_output": "go version go1.26.2 synthetic/arch",
                },
                "native": {
                    "cc_binary_sha256": "2" * 64, "cxx_binary_sha256": "3" * 64,
                    "target": "synthetic-target", "version_first_line": "synthetic compiler",
                },
            },
            "window": {"base_oid": self.base, "first_parent_unit_count": 31, "head_oid": self.head},
        }
        self.toolchain = {
            "git": self.repository["toolchain"]["git"],
            "go": self.repository["toolchain"]["go"],
            "native": self.repository["toolchain"]["native"],
            "verification_status": "matched_local_binaries_without_candidate_execution",
        }

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def write(self, relative: str, text: str) -> None:
        path = self.repo / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text, encoding="utf-8")

    def git(self, *args: str, env: dict[str, str] | None = None) -> str:
        completed = subprocess.run(
            ["git", *args], cwd=self.repo, check=True, text=True,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=env,
        )
        return completed.stdout.strip()

    def commit(self, subject: str, minute: int) -> None:
        self.git("add", ".")
        environment = dict(os.environ)
        timestamp = f"2026-07-17T00:{minute:02d}:00+00:00"
        environment["GIT_AUTHOR_DATE"] = timestamp
        environment["GIT_COMMITTER_DATE"] = timestamp
        self.git("commit", "-q", "-m", subject, env=environment)

    def rev(self, revision: str) -> str:
        return self.git("rev-parse", revision)

    def scan(self) -> dict:
        return eligibility.scan_repository(
            self.repo,
            git_binary=self.git_binary,
            repository=self.repository,
            repository_contract_sha256="3" * 64,
            scanner_sha256="4" * 64,
            schema_sha256="5" * 64,
            verified_toolchain=self.toolchain,
        )

    def test_disjoint_evidence_lineage_modules_and_targets(self) -> None:
        first = self.scan()
        self.assertEqual(first, self.scan())
        self.assertEqual(first["summary"]["first_parent_unit_count"], 31)
        self.assertEqual(first["summary"]["candidate_count"], 3)
        self.assertEqual(first["summary"]["runner_ready_count"], 3)
        mixed, merged, nested = first["candidates"]
        self.assertEqual(mixed["evidence_kind"], "changed_go_test_and_fixture_or_helper")
        self.assertEqual(mixed["source_file_count"], 1)
        self.assertEqual(mixed["changed_go_test_file_count"], 1)
        self.assertEqual(mixed["fixture_or_helper_file_count"], 1)
        self.assertEqual(mixed["production_go_paths_sha256"], eligibility._canonical_hash(["pkg/code.go"]))
        self.assertEqual(mixed["test_evidence_paths_sha256"], eligibility._canonical_hash(["pkg/code_test.go", "pkg/testdata/helper.go"]))
        self.assertEqual(mixed["path_partition_status"], "verified_disjoint_production_go_and_test_evidence")
        self.assertEqual(merged["evidence_kind"], "fixture_or_helper_only")
        self.assertEqual(merged["unit_kind"], "two_parent_feature_branch")
        self.assertEqual(len(merged["source_lineage_commit_oids"]), 1)
        self.assertEqual(merged["fixture_owner_bindings"][0]["owner_package_dir_sha256"], eligibility._sha256(b"pkg"))
        self.assertEqual(merged["test_targets"][0]["target_sha256"], eligibility._sha256(b"./pkg"))
        self.assertEqual(nested["unit_kind"], "single_parent_integration_unit")
        self.assertEqual(nested["source_lineage_commit_oids"], [nested["commit_oid"]])
        self.assertEqual(nested["module_bindings"][0]["root"], "nested")
        self.assertEqual(nested["test_targets"][0]["target_sha256"], eligibility._sha256(b"./part"))

    def test_git_subprocess_forces_local_object_defenses_and_scanner_rejects_replace_refs(self) -> None:
        original = subprocess.run
        observed: list[tuple[str | None, str | None, str | None, str | None]] = []

        def recording_run(*args, **kwargs):
            environment = kwargs.get("env", {})
            observed.append((
                environment.get("GIT_NO_REPLACE_OBJECTS"),
                environment.get("GIT_NO_LAZY_FETCH"),
                environment.get("GIT_OPTIONAL_LOCKS"),
                environment.get("GIT_ATTR_SOURCE"),
            ))
            return original(*args, **kwargs)

        with mock.patch.object(eligibility.subprocess, "run", side_effect=recording_run):
            eligibility._git_text(self.git_binary, self.repo, ["rev-parse", "HEAD"])
        self.assertEqual(observed, [("1", "1", "0", None)])
        self.git("replace", self.base, self.head)
        with self.assertRaisesRegex(eligibility.EligibilityV2Error, "forbidden replace refs"):
            self.scan()

    def test_nonempty_grafts_and_shallow_repository_fail_closed(self) -> None:
        grafts = pathlib.Path(self.git("rev-parse", "--path-format=absolute", "--git-path", "info/grafts"))
        grafts.parent.mkdir(parents=True, exist_ok=True)
        grafts.write_text(f"{self.head} {self.base}\n", encoding="ascii")
        with self.assertRaisesRegex(eligibility.EligibilityV2Error, "info/grafts must be absent or empty"):
            self.scan()
        grafts.unlink()
        shallow = pathlib.Path(self.git("rev-parse", "--path-format=absolute", "--git-path", "shallow"))
        shallow.write_text(f"{self.head}\n", encoding="ascii")
        with self.assertRaisesRegex(eligibility.EligibilityV2Error, "must not be shallow"):
            self.scan()

    def test_hostile_git_config_and_diff_environment_do_not_change_ledger_bytes(self) -> None:
        baseline = self.scan()
        order_file = pathlib.Path(self.temporary.name) / "hostile-order"
        order_file.write_text("nested/*\npkg/*\n", encoding="utf-8")
        attributes_file = pathlib.Path(self.temporary.name) / "hostile-attributes"
        attributes_file.write_text("*.go -diff\n", encoding="utf-8")
        global_config = pathlib.Path(self.temporary.name) / "hostile-global-config"
        global_config.write_text("[diff]\n\tcontext = 17\n\tnoprefix = true\n[color]\n\tui = always\n", encoding="utf-8")
        hostile_config = {
            "color.ui": "always",
            "core.abbrev": "12",
            "core.attributesFile": str(attributes_file),
            "core.quotePath": "false",
            "diff.algorithm": "histogram",
            "diff.color": "always",
            "diff.compactionHeuristic": "true",
            "diff.context": "9",
            "diff.dstPrefix": "RIGHT/",
            "diff.external": "/bin/false",
            "diff.ignoreSubmodules": "all",
            "diff.indentHeuristic": "true",
            "diff.interHunkContext": "99",
            "diff.mnemonicPrefix": "true",
            "diff.noprefix": "true",
            "diff.orderFile": str(order_file),
            "diff.relative": "true",
            "diff.renames": "true",
            "diff.srcPrefix": "LEFT/",
            "diff.submodule": "log",
            "diff.suppressBlankEmpty": "true",
            "log.showSignature": "true",
            "patchid.stable": "false",
            "patchid.verbatim": "true",
            "submodule.recurse": "true",
            "url.https://invalid.example/.insteadOf": "https://github.com/",
        }
        for key, value in hostile_config.items():
            self.git("config", "--local", key, value)
        hostile_environment = {
            "GIT_CONFIG_COUNT": "2",
            "GIT_ATTR_SOURCE": self.base,
            "GIT_CONFIG_GLOBAL": str(global_config),
            "GIT_CONFIG_KEY_0": "diff.context",
            "GIT_CONFIG_KEY_1": "core.abbrev",
            "GIT_CONFIG_PARAMETERS": "'diff.noprefix=true' 'diff.context=29'",
            "GIT_CONFIG_SYSTEM": str(global_config),
            "GIT_CONFIG_VALUE_0": "23",
            "GIT_CONFIG_VALUE_1": "7",
            "GIT_DIFF_OPTS": "-U31",
            "GIT_EXTERNAL_DIFF": "/bin/false",
        }
        with mock.patch.dict(os.environ, hostile_environment, clear=False):
            hostile = self.scan()
        self.assertEqual(hostile, baseline)

    def test_worktree_attributes_are_ignored_and_info_attributes_fail_closed(self) -> None:
        baseline = self.scan()
        self.write(".gitattributes", "*.go -diff\n")
        self.assertEqual(self.scan(), baseline)
        info_attributes = pathlib.Path(
            self.git("rev-parse", "--path-format=absolute", "--git-path", "info/attributes")
        )
        info_attributes.parent.mkdir(parents=True, exist_ok=True)
        info_attributes.write_text("*.go -diff\n", encoding="utf-8")
        with self.assertRaisesRegex(eligibility.EligibilityV2Error, "info/attributes must be absent or empty"):
            self.scan()

    def test_committed_custom_diff_driver_attribute_fails_closed(self) -> None:
        parent = self.head
        self.write(".gitattributes", "*.go diff=custom\n")
        self.write("pkg/code.go", "package pkg\n\nfunc Value() int { return 99 }\n")
        self.commit("custom diff driver", 33)
        commit = self.rev("HEAD")
        self.git("config", "--local", "diff.custom.xfuncname", "^func.*$")
        with self.assertRaisesRegex(eligibility.EligibilityV2Error, "candidate diff attribute is not allowed"):
            eligibility._diff(self.git_binary, self.repo, parent, commit, ["pkg/code.go"])

    def test_remote_window_and_contract_drift_fail_closed(self) -> None:
        changed = copy.deepcopy(self.repository)
        changed["remote"]["url"] = "https://github.com/example/wrong.git"
        with self.assertRaisesRegex(eligibility.EligibilityV2Error, "remote URL differs"):
            eligibility.scan_repository(
                self.repo, git_binary=self.git_binary, repository=changed, repository_contract_sha256="3" * 64,
                scanner_sha256="4" * 64, schema_sha256="5" * 64,
                verified_toolchain={"git": changed["toolchain"]["git"], "go": changed["toolchain"]["go"], "native": changed["toolchain"]["native"], "verification_status": "matched_local_binaries_without_candidate_execution"},
            )
        changed = copy.deepcopy(self.repository)
        changed["window"]["base_oid"] = self.rev("HEAD~30")
        with self.assertRaisesRegex(eligibility.EligibilityV2Error, "exact pinned-head~window"):
            eligibility.scan_repository(
                self.repo, git_binary=self.git_binary, repository=changed, repository_contract_sha256="3" * 64,
                scanner_sha256="4" * 64, schema_sha256="5" * 64,
                verified_toolchain=self.toolchain,
            )

    def test_pinned_window_survives_authoritative_remote_advancement_but_not_rewind(self) -> None:
        baseline = self.scan()
        self.write("notes.txt", "authoritative ref advanced\n")
        self.commit("later authoritative work", 34)
        advanced = self.rev("HEAD")
        self.git("update-ref", "refs/remotes/origin/main", advanced)
        self.assertEqual(self.scan(), baseline)
        self.git("update-ref", "refs/remotes/origin/main", self.base)
        with self.assertRaisesRegex(eligibility.EligibilityV2Error, "pinned head is not an ancestor"):
            self.scan()

    def test_resealed_path_authority_and_summary_tampering_fail(self) -> None:
        cases = (
            (lambda ledger: ledger["candidates"][0].__setitem__("path_partition_status", "overlap"), "partition"),
            (lambda ledger: ledger["authority"].__setitem__("owner_key", "invented"), "authority"),
            (lambda ledger: ledger["summary"].__setitem__("runner_ready_count", 0), "summary"),
            (lambda ledger: ledger["candidates"][0].__setitem__("negative_control_status", "passed"), "executed"),
            (lambda ledger: ledger["candidates"][0]["module_bindings"][0].__setitem__("path", "other/go.mod"), "root differs from path"),
            (lambda ledger: ledger["candidates"][0]["test_targets"][0].__setitem__("module_directive_sha256", "8" * 64), "module directive differs"),
            (lambda ledger: ledger["candidates"][0]["fixture_owner_bindings"][0].__setitem__("module_root", "nested"), "module_root is unresolved"),
            (lambda ledger: ledger["candidates"][0]["test_targets"][0].__setitem__("owner_go_test_file_count", 0), "fixture owner has no candidate-tree Go tests"),
        )
        for mutate, phrase in cases:
            with self.subTest(phrase=phrase):
                ledger = self.scan()
                mutate(ledger)
                ledger["ledger_sha256"] = eligibility._self_hash(ledger)
                with self.assertRaisesRegex(eligibility.EligibilityV2Error, phrase):
                    eligibility.validate_ledger(ledger)

    def test_repository_rebuild_rejects_resealed_tree_lineage_module_and_target_tampering(self) -> None:
        def tree(ledger: dict) -> None:
            ledger["candidates"][0]["tree_oid"] = ledger["candidates"][0]["parent_oid"]

        def lineage(ledger: dict) -> None:
            candidate = ledger["candidates"][1]
            candidate["source_lineage_commit_oids"] = [self.base]
            candidate["source_lineage_sha256"] = eligibility._canonical_hash([self.base])

        def module(ledger: dict) -> None:
            ledger["candidates"][0]["module_bindings"][0]["raw_sha256"] = "6" * 64

        def target(ledger: dict) -> None:
            ledger["candidates"][0]["test_targets"][0]["target_sha256"] = "7" * 64

        for mutate in (tree, lineage, module, target):
            with self.subTest(mutation=mutate.__name__):
                ledger = self.scan()
                mutate(ledger)
                ledger["ledger_sha256"] = eligibility._self_hash(ledger)
                with self.assertRaisesRegex(eligibility.EligibilityV2Error, "full ledger rebuild differs"):
                    eligibility.verify_ledger_against_repository(self.repo, ledger, git_binary=self.git_binary)

    def test_schema_accepts_synthetic_ledger(self) -> None:
        schema = json.loads((HERE / "schemas" / "development-task-eligibility-scan-v2.schema.json").read_text(encoding="utf-8"))
        self.assertEqual(relevance_dataset.validate_schema_instance(self.scan(), schema, "v2"), [])

    def test_unresolved_candidate_allows_empty_modules_and_is_blocked(self) -> None:
        repo = pathlib.Path(self.temporary.name) / "blocked-repo"
        repo.mkdir()

        def git(*args: str, env: dict[str, str] | None = None) -> str:
            completed = subprocess.run(
                ["git", *args], cwd=repo, check=True, text=True,
                stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=env,
            )
            return completed.stdout.strip()

        def write(relative: str, text: str) -> None:
            path = repo / relative
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(text, encoding="utf-8")

        def commit(subject: str, minute: int) -> None:
            git("add", ".")
            environment = dict(os.environ)
            timestamp = f"2026-07-17T01:{minute:02d}:00+00:00"
            environment["GIT_AUTHOR_DATE"] = timestamp
            environment["GIT_COMMITTER_DATE"] = timestamp
            git("commit", "-q", "-m", subject, env=environment)

        git("init", "-q", "-b", "main")
        git("config", "user.name", "Synthetic Reviewer")
        git("config", "user.email", "synthetic@example.invalid")
        write("nested/go.mod", "module example.invalid/nested\n\ngo 1.26\n")
        write("pkg/code.go", "package pkg\n\nfunc Value() int { return 1 }\n")
        write("pkg/code_test.go", "package pkg\n\nfunc TestPlaceholder() {}\n")
        commit("base", 0)
        base = git("rev-parse", "HEAD")
        write("pkg/code.go", "package pkg\n\nfunc Value() int { return 2 }\n")
        write("pkg/code_test.go", "package pkg\n\nfunc TestPlaceholder() { _ = Value() }\n")
        commit("outside every module", 1)
        for index in range(2, 32):
            write("notes.txt", f"unit {index}\n")
            commit(f"filler {index}", index)
        head = git("rev-parse", "HEAD")
        git("remote", "add", "origin", "https://github.com/example/repo.git")
        git("update-ref", "refs/remotes/origin/main", head)
        repository = copy.deepcopy(self.repository)
        repository["window"] = {"base_oid": base, "first_parent_unit_count": 31, "head_oid": head}
        ledger = eligibility.scan_repository(
            repo,
            git_binary=self.git_binary,
            repository=repository,
            repository_contract_sha256="3" * 64,
            scanner_sha256="4" * 64,
            schema_sha256="5" * 64,
            verified_toolchain=self.toolchain,
        )
        self.assertEqual(ledger["summary"]["candidate_count"], 1)
        self.assertEqual(ledger["summary"]["runner_ready_count"], 0)
        candidate = ledger["candidates"][0]
        self.assertEqual(candidate["module_bindings"], [])
        self.assertEqual(candidate["test_targets"], [])
        self.assertEqual(candidate["unresolved_source_module_count"], 1)
        self.assertEqual(candidate["unresolved_test_binding_count"], 1)
        self.assertEqual(candidate["runner_readiness"], "blocked_unresolved_static_binding")
        schema = json.loads((HERE / "schemas" / "development-task-eligibility-scan-v2.schema.json").read_text(encoding="utf-8"))
        self.assertEqual(relevance_dataset.validate_schema_instance(ledger, schema, "blocked-v2"), [])

    def test_repository_contract_rejects_duplicate_keys(self) -> None:
        path = pathlib.Path(self.temporary.name) / "duplicate.json"
        path.write_text('{"profile":"x","profile":"y"}', encoding="utf-8")
        with self.assertRaisesRegex(eligibility.EligibilityV2Error, "duplicate object key"):
            eligibility.load_repository_contract(path, "synthetic")


class CheckedRepositoryBindingsTest(unittest.TestCase):
    def test_checked_ledgers_revalidate_against_available_repositories(self) -> None:
        git_path = shutil.which("git")
        self.assertIsNotNone(git_path)
        git_binary = pathlib.Path(git_path or "git").resolve()
        repositories = {
            "entire-brain": pathlib.Path(os.environ.get("ENTIRE_BRAIN_AUDIT_REPO", pathlib.Path.home() / "Projects" / "entire-brain")),
            "entire-db": pathlib.Path(os.environ.get("ENTIRE_DB_AUDIT_REPO", pathlib.Path.home() / "Projects" / "entire-db")),
            "entire-graph": pathlib.Path(os.environ.get("ENTIRE_GRAPH_AUDIT_REPO", pathlib.Path.home() / "Projects" / "entire-graph")),
        }
        if not all(path.is_dir() for path in repositories.values()):
            self.skipTest("authoritative local repositories are unavailable")
        for key, repo in repositories.items():
            with self.subTest(repository=key):
                ledger = json.loads((HERE / f"development-task-eligibility-{key}-v2.json").read_text(encoding="utf-8"))
                eligibility.verify_ledger_dependencies(
                    ledger,
                    contract=HERE / "development-task-repositories-v2.json",
                    schema=HERE / "schemas" / "development-task-eligibility-scan-v2.schema.json",
                )
                eligibility.verify_ledger_against_repository(repo, ledger, git_binary=git_binary)


if __name__ == "__main__":
    unittest.main()
