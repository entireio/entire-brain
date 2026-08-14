from __future__ import annotations

import ast
import contextlib
import copy
import hashlib
import io
import json
import os
import pathlib
import tempfile
import unittest
from typing import Any
from unittest import mock

import negative_control_execution_contract_v1 as contract


class NegativeControlExecutionContractV1Test(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.root = pathlib.Path(contract.__file__).parent
        cls.artifact_path = (
            cls.root / "development-task-negative-control-execution-contract-v1.json"
        )
        cls.paths = {
            "contract_path": cls.root / "development-task-repositories-v2.json",
            "contract_schema_path": (
                cls.root / "schemas/negative-control-execution-contract-v1.schema.json"
            ),
            "eligibility_schema_path": (
                cls.root / "schemas/development-task-eligibility-scan-v2.schema.json"
            ),
            "ledger_paths": [
                cls.root / "development-task-eligibility-entire-brain-v2.json",
                cls.root / "development-task-eligibility-entire-db-v2.json",
                cls.root / "development-task-eligibility-entire-graph-v2.json",
            ],
            "plan_path": cls.root / "development-task-negative-control-run-plan-v2.json",
            "plan_schema_path": (
                cls.root
                / "schemas/development-task-negative-control-run-plan-v2.schema.json"
            ),
            "registry_path": cls.root / "development-task-overlap-registry-v1.json",
            "registry_schema_path": (
                cls.root / "schemas/development-task-overlap-registry-v1.schema.json"
            ),
        }
        cls.artifact, cls.artifact_raw = contract._read_json(
            cls.artifact_path,
            label="checked execution contract",
        )

    @staticmethod
    def reseal(value: dict[str, Any]) -> None:
        value["contract_sha256"] = contract._self_hash(value)

    @classmethod
    def cli_arguments(cls) -> list[str]:
        return [
            "--contract",
            str(cls.paths["contract_path"]),
            "--contract-schema",
            str(cls.paths["contract_schema_path"]),
            "--eligibility-schema",
            str(cls.paths["eligibility_schema_path"]),
            "--ledger",
            str(cls.paths["ledger_paths"][0]),
            "--ledger",
            str(cls.paths["ledger_paths"][1]),
            "--ledger",
            str(cls.paths["ledger_paths"][2]),
            "--plan",
            str(cls.paths["plan_path"]),
            "--plan-schema",
            str(cls.paths["plan_schema_path"]),
            "--registry",
            str(cls.paths["registry_path"]),
            "--registry-schema",
            str(cls.paths["registry_schema_path"]),
        ]

    def test_checked_artifact_is_exact_dependency_rebuild(self) -> None:
        self.assertEqual(self.artifact_raw, contract._render(self.artifact))
        rebuilt = contract.build_contract(**self.paths)
        self.assertEqual(self.artifact, rebuilt)
        contract.verify_contract_dependencies(self.artifact, **self.paths)

    def test_exact_candidate_and_schedule_projection(self) -> None:
        candidates = self.artifact["candidates"]
        schedule = self.artifact["schedule"]
        self.assertEqual(len(candidates), 62)
        self.assertEqual(len(schedule), 248)
        self.assertEqual(
            [row["candidate_ordinal"] for row in candidates],
            list(range(1, 63)),
        )
        self.assertEqual(
            [row["attempt_ordinal"] for row in schedule],
            list(range(1, 249)),
        )
        self.assertEqual(
            [row["key"] for row in self.artifact["repository_bindings"]],
            ["entire-brain", "entire-db", "entire-graph"],
        )
        self.assertEqual(
            [row["candidate_count"] for row in self.artifact["repository_bindings"]],
            [18, 25, 19],
        )
        for candidate_index, candidate in enumerate(candidates):
            rows = schedule[candidate_index * 4 : candidate_index * 4 + 4]
            self.assertEqual(
                [(row["arm"], row["repetition"]) for row in rows],
                [
                    ("baseline", 1),
                    ("baseline", 2),
                    ("first_parent_source_reversal", 1),
                    ("first_parent_source_reversal", 2),
                ],
            )
            self.assertEqual(
                {row["candidate_ref"] for row in rows},
                {candidate["candidate_ref"]},
            )
        self.assertEqual(
            self.artifact["candidate_bindings_sha256"],
            contract._canonical_hash(candidates),
        )
        self.assertEqual(
            self.artifact["schedule_sha256"],
            contract._canonical_hash(schedule),
        )

    def test_all_runtime_and_candidate_execution_bindings_are_null(self) -> None:
        self.assertEqual(self.artifact["runtime_bindings"], contract.RUNTIME_BINDINGS)
        self.assertTrue(all(value is None for value in contract.RUNTIME_BINDINGS.values()))
        self.assertTrue(
            all(
                candidate["private_execution_projection_sha256"] is None
                and candidate["test_command_sha256"] is None
                for candidate in self.artifact["candidates"]
            )
        )
        self.assertEqual(self.artifact["authority"], contract.AUTHORITY)
        self.assertEqual(self.artifact["status"], contract.STATUS)
        self.assertEqual(self.artifact["execution_status"], contract.EXECUTION_STATUS)

    def test_candidate_runtime_authority_and_schedule_tampering_fail_closed(self) -> None:
        cases: list[tuple[str, Any]] = [
            (
                "test-command",
                lambda value: value["candidates"][0].__setitem__(
                    "test_command_sha256", "a" * 64
                ),
            ),
            (
                "private-projection",
                lambda value: value["candidates"][0].__setitem__(
                    "private_execution_projection_sha256", "a" * 64
                ),
            ),
            (
                "runtime-approval",
                lambda value: value["runtime_bindings"].__setitem__(
                    "approval_receipt_sha256", "a" * 64
                ),
            ),
            (
                "authority",
                lambda value: value["authority"].__setitem__(
                    "candidate_execution", "authorized"
                ),
            ),
            (
                "schedule-order",
                lambda value: value["schedule"].__setitem__(
                    slice(0, 2), [value["schedule"][1], value["schedule"][0]]
                ),
            ),
            (
                "state-machine",
                lambda value: value["state_machine"].__setitem__(
                    "publication_rule", "publish_before_cleanup"
                ),
            ),
        ]
        for label, mutate in cases:
            value = copy.deepcopy(self.artifact)
            mutate(value)
            value["candidate_bindings_sha256"] = contract._canonical_hash(
                value["candidates"]
            )
            value["schedule_sha256"] = contract._canonical_hash(value["schedule"])
            self.reseal(value)
            with self.subTest(label=label), self.assertRaises(
                contract.ExecutionContractError
            ):
                contract.validate_contract(
                    value,
                    contract_schema_path=self.paths["contract_schema_path"],
                )

    def test_dependency_rebuild_rejects_resealed_candidate_and_component_drift(self) -> None:
        candidate_drift = copy.deepcopy(self.artifact)
        candidate_drift["candidates"][0]["commit_oid"] = "a" * 40
        candidate_drift["candidate_bindings_sha256"] = contract._canonical_hash(
            candidate_drift["candidates"]
        )
        self.reseal(candidate_drift)
        with self.assertRaises(contract.ExecutionContractError):
            contract.verify_contract_dependencies(candidate_drift, **self.paths)

        components = copy.deepcopy(self.artifact["component_bindings"])
        components[0]["artifact_sha256"] = "a" * 64
        with mock.patch.object(contract, "_component_bindings", return_value=components):
            with self.assertRaises(contract.ExecutionContractError):
                contract.verify_contract_dependencies(self.artifact, **self.paths)

    def test_cross_binding_and_malformed_shape_tampering_fail_cleanly(self) -> None:
        cases: list[tuple[str, Any]] = [
            (
                "candidate-repository",
                lambda value: value["candidates"][0].__setitem__(
                    "repository_id", "github.com/evil/repo"
                ),
            ),
            (
                "candidate-parent",
                lambda value: value["candidates"][0].__setitem__(
                    "parent_oid", "a" * 40
                ),
            ),
            (
                "candidate-tree",
                lambda value: value["candidates"][0].__setitem__(
                    "tree_oid", "a" * 40
                ),
            ),
            (
                "candidate-source-diff",
                lambda value: value["candidates"][0].__setitem__(
                    "source_diff_sha256", "a" * 64
                ),
            ),
            (
                "candidate-test-diff",
                lambda value: value["candidates"][0].__setitem__(
                    "test_diff_sha256", "a" * 64
                ),
            ),
            (
                "candidate-test-targets",
                lambda value: value["candidates"][0].__setitem__(
                    "test_target_bindings_sha256", "a" * 64
                ),
            ),
            (
                "unit-kind",
                lambda value: value["candidates"][0].__setitem__(
                    "unit_kind", "single_parent_integration_unit"
                ),
            ),
            (
                "repository-counts",
                lambda value: (
                    value["repository_bindings"][0].__setitem__("candidate_count", 17),
                    value["repository_bindings"][1].__setitem__("candidate_count", 26),
                ),
            ),
            (
                "duplicate-ledger-key",
                lambda value: value["inputs"]["ledgers"][1].__setitem__(
                    "repository_key", "entire-brain"
                ),
            ),
            (
                "component-file",
                lambda value: value["component_bindings"][0].__setitem__(
                    "artifact_file", "arbitrary.py"
                ),
            ),
            (
                "schema-identity",
                lambda value: value["implementation"].__setitem__(
                    "schema_sha256", "a" * 64
                ),
            ),
            (
                "run-plan-builder-input",
                lambda value: value["inputs"]["run_plan"].__setitem__(
                    "builder_sha256", "a" * 64
                ),
            ),
            (
                "repository-contract-input",
                lambda value: value["inputs"]["repository_contract"].__setitem__(
                    "artifact_sha256", "a" * 64
                ),
            ),
            (
                "overlap-registry-input",
                lambda value: value["inputs"]["overlap_registry"].__setitem__(
                    "artifact_sha256", "a" * 64
                ),
            ),
            (
                "eligibility-schema-input",
                lambda value: value["inputs"]["eligibility_schema"].__setitem__(
                    "artifact_sha256", "a" * 64
                ),
            ),
            (
                "malformed-repository",
                lambda value: value["repository_bindings"][0].pop(
                    "candidate_count"
                ),
            ),
        ]
        for label, mutate in cases:
            value = copy.deepcopy(self.artifact)
            mutate(value)
            value["candidate_bindings_sha256"] = contract._canonical_hash(
                value["candidates"]
            )
            value["schedule_sha256"] = contract._canonical_hash(value["schedule"])
            self.reseal(value)
            with self.subTest(label=label), self.assertRaises(
                contract.ExecutionContractError
            ):
                contract.validate_contract(
                    value,
                    contract_schema_path=self.paths["contract_schema_path"],
                )

    def test_run_and_attempt_journals_cover_all_attempts_and_restart_cleanup(self) -> None:
        state = self.artifact["state_machine"]
        self.assertEqual(
            state["scope"],
            "run_journal_with_nested_per_schedule_row_attempt_journal_v1",
        )
        run = state["run_journal"]
        self.assertEqual(run["schedule_cursor_initial"], 0)
        self.assertIn("schedule_cursor_248", run["success_guard"])
        transitions = {
            row["from"]: set(row["to"]) for row in run["allowed_transitions"]
        }
        for source, target in run["interruption_targets"].items():
            with self.subTest(interruption_source=source):
                self.assertIn(target, transitions[source])

                reachable = {target}
                frontier = [target]
                while frontier:
                    current = frontier.pop()
                    for following in transitions.get(current, set()):
                        if following not in reachable:
                            reachable.add(following)
                            frontier.append(following)
                self.assertNotIn("aggregate_receipt_committed", reachable)
                self.assertTrue(
                    reachable
                    & {"cleaned_no_receipt", "cleanup_failed_latched"}
                )
        self.assertNotIn("aggregate_receipt_committed", transitions)
        for state_name in (
            "run_aborting",
            "run_cleaning_no_receipt",
            "reservation_released_no_receipt",
        ):
            self.assertNotIn(
                "aggregate_receipt_committed", transitions[state_name]
            )
        self.assertIn("captured_test_exit_or_timeout_is_data", run["abort_latch"])
        self.assertIn("atomic_durable", run["receipt_commit_boundary"])
        self.assertIn("persistent_monotonic", run["receipt_forbidden_latch"])
        self.assertIn("exclusive_run_lock", state["restart_recovery_rule"])
        self.assertIn("reservation_release", state["restart_recovery_rule"])
        self.assertIn("blocks_every_future_run", state["latched_cleanup_failure_rule"])
        self.assertIn("without_ever_permitting_failed_run_receipt", state["latched_cleanup_failure_rule"])
        attempt = state["attempt_journal"]
        self.assertEqual(
            attempt["binding_rule"],
            "bind_exact_schedule_row_at_run_cursor_plus_one",
        )
        self.assertEqual(
            attempt["success_terminal_state"], "attempt_cleanup_committed"
        )
        self.assertIn("post_cleanup_pre_cursor", attempt["interruption_rule"])

        for source, target in (
            ("run_cleaning", "run_cleaning_no_receipt"),
            ("reservation_released", "reservation_released_no_receipt"),
        ):
            value = copy.deepcopy(self.artifact)
            transition = next(
                row
                for row in value["state_machine"]["run_journal"]["allowed_transitions"]
                if row["from"] == source
            )
            transition["to"].remove(target)
            self.reseal(value)
            with self.subTest(missing_late_interruption_edge=source), self.assertRaises(
                contract.ExecutionContractError
            ):
                contract.validate_contract(
                    value,
                    contract_schema_path=self.paths["contract_schema_path"],
                )

    def test_mutated_ledger_and_schema_fail_before_contract_publication(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            temporary = pathlib.Path(directory).resolve(strict=True)
            ledger = json.loads(self.paths["ledger_paths"][0].read_bytes())
            ledger["candidates"][0]["commit_oid"] = "a" * 40
            ledger_path = temporary / "ledger.json"
            ledger_path.write_text(
                json.dumps(ledger, indent=2, sort_keys=True) + "\n",
                encoding="utf-8",
            )
            ledger_paths = [ledger_path, *self.paths["ledger_paths"][1:]]
            with self.assertRaises(contract.ExecutionContractError):
                contract.build_contract(**{**self.paths, "ledger_paths": ledger_paths})

            schema = json.loads(self.paths["contract_schema_path"].read_bytes())
            schema["properties"]["status"]["const"] = "authorized"
            schema_path = temporary / "schema.json"
            schema_path.write_text(
                json.dumps(schema, indent=2, sort_keys=True) + "\n",
                encoding="utf-8",
            )
            with self.assertRaises(contract.ExecutionContractError):
                contract.build_contract(
                    **{**self.paths, "contract_schema_path": schema_path}
                )

    def test_resealed_ledger_change_still_fails_exact_plan_binding(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            temporary = pathlib.Path(directory).resolve(strict=True)
            ledger = json.loads(self.paths["ledger_paths"][0].read_bytes())
            ledger["generated_at"] = "2026-07-14T17:34:42+02:00"
            ledger["ledger_sha256"] = contract._field_self_hash(
                ledger, "ledger_sha256"
            )
            ledger_path = temporary / "resealed-ledger.json"
            ledger_path.write_text(
                json.dumps(ledger, indent=2, sort_keys=True) + "\n",
                encoding="utf-8",
            )
            ledger_paths = [ledger_path, *self.paths["ledger_paths"][1:]]
            with self.assertRaisesRegex(
                contract.ExecutionContractError,
                "raw hash differs from plan",
            ):
                contract.build_contract(
                    **{**self.paths, "ledger_paths": ledger_paths}
                )

    def test_byte_identical_ledger_with_wrong_file_name_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            wrong = pathlib.Path(directory).resolve(strict=True) / "wrong-ledger-name.json"
            wrong.write_bytes(self.paths["ledger_paths"][0].read_bytes())
            ledgers = [wrong, *self.paths["ledger_paths"][1:]]
            with self.assertRaisesRegex(
                contract.ExecutionContractError,
                "file name",
            ):
                contract.build_contract(
                    **{**self.paths, "ledger_paths": ledgers}
                )

    def test_cli_build_is_canonical_and_check_rejects_noncanonical_bytes(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            temporary = pathlib.Path(directory).resolve(strict=True)
            output = temporary / "contract.json"
            self.assertEqual(
                contract.main(["build", *self.cli_arguments(), "--output", str(output)]),
                0,
            )
            self.assertEqual(output.read_bytes(), self.artifact_raw)
            self.assertEqual(os.stat(output).st_mode & 0o777, 0o644)
            self.assertEqual(
                contract.main(["check", str(output), *self.cli_arguments()]),
                0,
            )

            noncanonical = temporary / "noncanonical.json"
            noncanonical.write_text(
                json.dumps(self.artifact, sort_keys=True),
                encoding="utf-8",
            )
            error = io.StringIO()
            with contextlib.redirect_stderr(error), self.assertRaises(SystemExit) as raised:
                contract.main(["check", str(noncanonical), *self.cli_arguments()])
            self.assertEqual(raised.exception.code, 2)
            self.assertIn("bytes are not canonical", error.getvalue())

    def test_secure_reader_rejects_symlink_ancestor_traversal_and_non_object(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            temporary = pathlib.Path(directory).resolve(strict=True)
            link = temporary / "contract-link.json"
            link.symlink_to(self.artifact_path)
            with self.assertRaises(contract.ExecutionContractError):
                contract._read_json(link, label="symlink")
            array_path = temporary / "array.json"
            array_path.write_text("[]\n", encoding="utf-8")
            with self.assertRaises(contract.ExecutionContractError):
                contract._read_json(array_path, label="array")

            real = temporary / "real"
            real.mkdir()
            nested = real / "contract.json"
            nested.write_bytes(self.artifact_raw)
            ancestor_link = temporary / "ancestor-link"
            ancestor_link.symlink_to(real, target_is_directory=True)
            with self.assertRaises(contract.ExecutionContractError):
                contract._read_json(
                    ancestor_link / nested.name,
                    label="absolute symlink ancestor",
                )

            previous_cwd = pathlib.Path.cwd()
            os.chdir(temporary)
            try:
                relative_value, _ = contract._read_json(
                    pathlib.Path("real") / nested.name,
                    label="relative regular path",
                )
                self.assertEqual(relative_value, self.artifact)
                with self.assertRaises(contract.ExecutionContractError):
                    contract._read_json(
                        pathlib.Path("ancestor-link") / nested.name,
                        label="relative symlink ancestor",
                    )
                with self.assertRaises(contract.ExecutionContractError):
                    contract._read_json(
                        pathlib.Path("..") / temporary.name / "real" / nested.name,
                        label="parent traversal",
                    )
            finally:
                os.chdir(previous_cwd)

    def test_every_caller_supplied_dependency_rejects_symlinks(self) -> None:
        scalar_keys = [
            "contract_path",
            "contract_schema_path",
            "eligibility_schema_path",
            "plan_path",
            "plan_schema_path",
            "registry_path",
            "registry_schema_path",
        ]
        with tempfile.TemporaryDirectory() as directory:
            temporary = pathlib.Path(directory).resolve(strict=True)
            for key in scalar_keys:
                original = self.paths[key]
                link_root = temporary / key
                link_root.mkdir()
                link = link_root / original.name
                link.symlink_to(original)
                kwargs = {**self.paths, key: link}
                with self.subTest(key=key), self.assertRaises(
                    contract.ExecutionContractError
                ):
                    contract.build_contract(**kwargs)

                ancestor_link = temporary / f"{key}-ancestor"
                ancestor_link.symlink_to(original.parent, target_is_directory=True)
                kwargs = {**self.paths, key: ancestor_link / original.name}
                with self.subTest(key=key, symlink="ancestor"), self.assertRaises(
                    contract.ExecutionContractError
                ):
                    contract.build_contract(**kwargs)
            for index, original in enumerate(self.paths["ledger_paths"]):
                link_root = temporary / f"ledger-{index}"
                link_root.mkdir()
                link = link_root / original.name
                link.symlink_to(original)
                ledgers = list(self.paths["ledger_paths"])
                ledgers[index] = link
                with self.subTest(ledger=index), self.assertRaises(
                    contract.ExecutionContractError
                ):
                    contract.build_contract(
                        **{**self.paths, "ledger_paths": ledgers}
                    )

                ancestor_link = temporary / f"ledger-{index}-ancestor"
                ancestor_link.symlink_to(original.parent, target_is_directory=True)
                ledgers = list(self.paths["ledger_paths"])
                ledgers[index] = ancestor_link / original.name
                with self.subTest(ledger=index, symlink="ancestor"), self.assertRaises(
                    contract.ExecutionContractError
                ):
                    contract.build_contract(
                        **{**self.paths, "ledger_paths": ledgers}
                    )

    def test_module_has_build_check_only_and_no_execution_import_surface(self) -> None:
        source = pathlib.Path(contract.__file__).read_text(encoding="utf-8")
        tree = ast.parse(source)
        imports: set[str] = set()
        parser_commands: set[str] = set()
        dangerous_attributes: set[str] = set()
        for node in ast.walk(tree):
            if isinstance(node, ast.Import):
                imports.update(alias.name.split(".", 1)[0] for alias in node.names)
            elif isinstance(node, ast.ImportFrom) and node.module:
                imports.add(node.module.split(".", 1)[0])
            elif isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute):
                if node.func.attr == "add_parser" and node.args:
                    argument = node.args[0]
                    if isinstance(argument, ast.Constant) and isinstance(argument.value, str):
                        parser_commands.add(argument.value)
                if node.func.attr in {
                    "Popen",
                    "call",
                    "check_call",
                    "check_output",
                    "execv",
                    "execve",
                    "fork",
                    "popen",
                    "posix_spawn",
                    "run",
                    "spawnv",
                    "system",
                }:
                    dangerous_attributes.add(node.func.attr)
        self.assertEqual(parser_commands, {"build", "check"})
        self.assertFalse(
            imports
            & {
                "http",
                "requests",
                "shutil",
                "socket",
                "subprocess",
                "urllib",
            }
        )

    def test_transitive_schema_validator_has_no_process_or_network_surface(self) -> None:
        source_path = pathlib.Path(contract.draft202012.__file__)
        tree = ast.parse(source_path.read_text(encoding="utf-8"))
        imports: set[str] = set()
        dangerous_attributes: set[str] = set()
        for node in ast.walk(tree):
            if isinstance(node, ast.Import):
                imports.update(alias.name.split(".", 1)[0] for alias in node.names)
            elif isinstance(node, ast.ImportFrom) and node.module:
                imports.add(node.module.split(".", 1)[0])
            elif isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute):
                if node.func.attr in {
                    "Popen",
                    "call",
                    "check_call",
                    "check_output",
                    "execv",
                    "execve",
                    "fork",
                    "popen",
                    "posix_spawn",
                    "run",
                    "spawnv",
                    "system",
                    "urlopen",
                }:
                    dangerous_attributes.add(node.func.attr)
        self.assertFalse(imports & {"http", "requests", "socket", "subprocess"})
        self.assertFalse(dangerous_attributes)
        self.assertFalse(dangerous_attributes)
        self.assertFalse(
            imports
            & {
                "negative_control_private_log",
                "task_eligibility_v2",
                "task_negative_control_classification_v1",
                "task_negative_control_gate_v1",
                "task_negative_control_plan_v2",
            }
        )

    def test_checked_raw_hash_is_stable(self) -> None:
        self.assertEqual(
            hashlib.sha256(self.artifact_raw).hexdigest(),
            "94422af8a5e24c6a855b66cf40868565ee3518d0c75eb5343b5d2450523b2c1e",
        )
        self.assertEqual(
            self.artifact["contract_sha256"],
            "1e290b91d5e0be42f750bf0e655edee48d297f86ad3e3867b8fe9177fd5125ef",
        )


if __name__ == "__main__":
    unittest.main()
