from __future__ import annotations

import copy
import json
import pathlib
import tempfile
import unittest
from unittest import mock
from typing import Any, Callable

import draft202012
import task_negative_control_plan_v2 as plan_v2


class TaskNegativeControlPlanV2Test(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.root = pathlib.Path(plan_v2.__file__).parent
        cls.contract = cls.root / "development-task-repositories-v2.json"
        cls.eligibility_schema = cls.root / "schemas" / "development-task-eligibility-scan-v2.schema.json"
        cls.ledgers = [
            cls.root / "development-task-eligibility-entire-brain-v2.json",
            cls.root / "development-task-eligibility-entire-db-v2.json",
            cls.root / "development-task-eligibility-entire-graph-v2.json",
        ]
        cls.plan_schema = cls.root / "schemas" / "development-task-negative-control-run-plan-v2.schema.json"
        cls.registry = cls.root / "development-task-overlap-registry-v1.json"
        cls.registry_schema = cls.root / "schemas" / "development-task-overlap-registry-v1.schema.json"
        cls.checked_plan_path = cls.root / "development-task-negative-control-run-plan-v2.json"
        cls.checked_plan, cls.checked_raw = plan_v2._load_json(cls.checked_plan_path)
        cls.kwargs = {
            "contract_path": cls.contract,
            "eligibility_schema_path": cls.eligibility_schema,
            "ledger_paths": cls.ledgers,
            "plan_schema_path": cls.plan_schema,
            "registry_path": cls.registry,
            "registry_schema_path": cls.registry_schema,
        }

    def reseal(self, value: dict[str, Any]) -> dict[str, Any]:
        value["plan_sha256"] = plan_v2._self_hash(value)
        return value

    def assert_plan_tamper_rejected(
        self,
        mutate: Callable[[dict[str, Any]], None],
        pattern: str,
        *,
        dependency_check: bool = False,
    ) -> None:
        value = copy.deepcopy(self.checked_plan)
        mutate(value)
        self.reseal(value)
        with self.assertRaisesRegex(plan_v2.RunPlanError, pattern):
            if dependency_check:
                plan_v2.verify_plan_dependencies(value, **self.kwargs)
            else:
                plan_v2.validate_plan(value)

    def test_checked_plan_rebuilds_and_renders_byte_for_byte(self) -> None:
        rebuilt = plan_v2.build_plan(**self.kwargs)
        self.assertEqual(rebuilt, self.checked_plan)
        self.assertEqual(plan_v2._render(rebuilt), self.checked_raw)
        plan_v2.verify_plan_dependencies(self.checked_plan, **self.kwargs)

        with tempfile.TemporaryDirectory() as temporary:
            first = pathlib.Path(temporary) / "first.json"
            second = pathlib.Path(temporary) / "second.json"
            plan_v2._write_atomic(first, rebuilt)
            plan_v2._write_atomic(second, plan_v2.build_plan(**self.kwargs))
            self.assertEqual(first.read_bytes(), second.read_bytes())
            self.assertEqual(first.read_bytes(), self.checked_raw)

    def test_schema_accepts_exact_checked_plan(self) -> None:
        schema = json.loads(self.plan_schema.read_text(encoding="utf-8"))
        validator = draft202012.Validator(
            [draft202012.SchemaDocument(self.plan_schema.name, schema)]
        )
        validator.validate(self.checked_plan, self.plan_schema.name, label="run plan")

    def test_schema_independently_rejects_exact_repository_order_tampering(self) -> None:
        schema = json.loads(self.plan_schema.read_text(encoding="utf-8"))
        validator = draft202012.Validator(
            [draft202012.SchemaDocument(self.plan_schema.name, schema)]
        )

        def swap_rows(value: dict[str, Any]) -> None:
            value["repositories"][0], value["repositories"][1] = (
                value["repositories"][1],
                value["repositories"][0],
            )

        def reverse_positions(value: dict[str, Any]) -> None:
            order = value["repositories"][0]["candidate_order"]
            order[0]["first_parent_position"], order[1]["first_parent_position"] = (
                order[1]["first_parent_position"],
                order[0]["first_parent_position"],
            )
            value["repositories"][0]["candidate_order_sha256"] = plan_v2._canonical_hash(order)

        def duplicate_reference(value: dict[str, Any]) -> None:
            order = value["repositories"][0]["candidate_order"]
            order[1]["candidate_ref"] = order[0]["candidate_ref"]
            value["repositories"][0]["candidate_order_sha256"] = plan_v2._canonical_hash(order)

        cases: list[tuple[str, Callable[[dict[str, Any]], None]]] = [
            (
                "candidate count",
                lambda value: value["repositories"][0].__setitem__("candidate_count", 99),
            ),
            ("swapped repository rows", swap_rows),
            (
                "wrong repository key",
                lambda value: value["repositories"][0].__setitem__("key", "entire-db"),
            ),
            ("out-of-order positions", reverse_positions),
            ("duplicate candidate refs", duplicate_reference),
        ]
        for label, mutate in cases:
            with self.subTest(label=label):
                value = copy.deepcopy(self.checked_plan)
                mutate(value)
                self.reseal(value)
                with self.assertRaisesRegex(draft202012.SchemaError, "repositories"):
                    validator.validate(value, self.plan_schema.name, label="tampered plan")

    def test_exact_62_candidate_order_and_pending_boundary(self) -> None:
        self.assertEqual(
            [item["key"] for item in self.checked_plan["repositories"]],
            list(plan_v2.REPOSITORY_ORDER),
        )
        self.assertEqual(
            [item["candidate_count"] for item in self.checked_plan["repositories"]],
            [18, 25, 19],
        )
        refs = [
            candidate["candidate_ref"]
            for repository in self.checked_plan["repositories"]
            for candidate in repository["candidate_order"]
        ]
        self.assertEqual(len(refs), 62)
        self.assertEqual(len(set(refs)), 62)
        self.assertEqual(self.checked_plan["status"], "pending_owner_authorization")
        self.assertEqual(self.checked_plan["authority"], plan_v2.AUTHORITY)
        self.assertIsNone(self.checked_plan["cache_seed"]["manifest_sha256"])
        self.assertEqual(self.checked_plan["isolation"]["network_os_enforcement"], "not_enforced")
        self.assertEqual(self.checked_plan["isolation"]["filesystem_os_enforcement"], "not_enforced")
        self.assertEqual(self.checked_plan["private_log_policy"]["private_root_mode"], "0700")
        self.assertEqual(self.checked_plan["private_log_policy"]["private_file_mode"], "0600")

    def test_authorized_or_overclaimed_authority_is_rejected_after_reseal(self) -> None:
        cases: list[tuple[str, Callable[[dict[str, Any]], None]]] = [
            ("status", lambda value: value.__setitem__("status", "authorized")),
            (
                "approval receipt",
                lambda value: value["authority"].__setitem__("approval_receipt", "verified"),
            ),
            (
                "approval hash",
                lambda value: value["authority"].__setitem__("approval_receipt_sha256", "a" * 64),
            ),
            (
                "trust mechanism",
                lambda value: value["authority"].__setitem__("approval_trust_mechanism", "local_signature"),
            ),
            (
                "benchmark",
                lambda value: value["authority"].__setitem__("benchmark_execution", "authorized"),
            ),
            (
                "candidate",
                lambda value: value["authority"].__setitem__("candidate_execution", "authorized"),
            ),
            (
                "provider",
                lambda value: value["authority"].__setitem__("model_provider_execution", "authorized"),
            ),
            (
                "paid",
                lambda value: value["authority"].__setitem__("paid_execution", "authorized"),
            ),
            (
                "calibration",
                lambda value: value["authority"].__setitem__("calibration_membership", "authorized"),
            ),
            (
                "holdout",
                lambda value: value["authority"].__setitem__("confirmatory_holdout_membership", "authorized"),
            ),
        ]
        for label, mutate in cases:
            with self.subTest(label=label):
                self.assert_plan_tamper_rejected(mutate, "authorized run plans|authority boundary")

    def test_protocol_and_every_resource_budget_field_are_frozen(self) -> None:
        protocol_fields = {
            "repetitions_per_arm": 3,
            "outer_timeout_seconds_per_arm": 601,
            "max_total_wall_seconds": 28_801,
            "max_concurrency": 2,
            "candidate_process_execution": "implemented",
            "classifier": "implemented",
            "private_log_writer": "implemented",
        }
        for field, replacement in protocol_fields.items():
            with self.subTest(protocol=field):
                self.assert_plan_tamper_rejected(
                    lambda value, field=field, replacement=replacement: value[
                        "execution_protocol"
                    ].__setitem__(field, replacement),
                    "execution protocol differs",
                )

        for field, original in plan_v2.RESOURCE_BUDGET.items():
            replacement: Any = (
                original + 1 if type(original) is int else f"{original}_tampered"
            )
            with self.subTest(resource=field):
                self.assert_plan_tamper_rejected(
                    lambda value, field=field, replacement=replacement: value[
                        "resource_budget"
                    ].__setitem__(field, replacement),
                    "resource budget differs",
                )

    def test_ledger_raw_self_count_order_and_candidate_bindings_are_exact(self) -> None:
        mutations: list[tuple[str, Callable[[dict[str, Any]], None], str]] = [
            (
                "raw hash",
                lambda value: value["repositories"][0].__setitem__("ledger_file_sha256", "a" * 64),
                "dependency rebuild",
            ),
            (
                "self hash",
                lambda value: value["repositories"][0].__setitem__("ledger_sha256", "b" * 64),
                "dependency rebuild",
            ),
            (
                "count",
                lambda value: value["repositories"][0].__setitem__("candidate_count", 19),
                "identity or count differs",
            ),
            (
                "order swap",
                lambda value: value["repositories"][0]["candidate_order"].__setitem__(
                    slice(0, 2),
                    list(reversed(value["repositories"][0]["candidate_order"][:2])),
                ),
                "positions are not canonical",
            ),
            (
                "order hash",
                lambda value: value["repositories"][0].__setitem__("candidate_order_sha256", "c" * 64),
                "candidate-order hash differs",
            ),
            (
                "candidate ref",
                lambda value: value["repositories"][0]["candidate_order"][0].__setitem__("candidate_ref", "d" * 64),
                "dependency rebuild",
            ),
        ]
        for label, mutate, pattern in mutations:
            with self.subTest(label=label):
                if label == "candidate ref":
                    def mutate_and_rehash(value: dict[str, Any]) -> None:
                        mutate(value)
                        value["repositories"][0]["candidate_order_sha256"] = plan_v2._canonical_hash(
                            value["repositories"][0]["candidate_order"]
                        )

                    self.assert_plan_tamper_rejected(
                        mutate_and_rehash, pattern, dependency_check=True
                    )
                else:
                    self.assert_plan_tamper_rejected(
                        mutate,
                        pattern,
                        dependency_check=label in {"raw hash", "self hash"},
                    )

    def test_every_bound_toolchain_dimension_is_dependency_checked(self) -> None:
        cases = [
            ("git", "binary_sha256", "a" * 64),
            ("git", "version_output", "git version 99.0.0"),
            ("go", "binary_sha256", "b" * 64),
            ("go", "version_output", "go version go99.0 test/test"),
            ("go", "goos", "testos"),
            ("go", "goarch", "testarch"),
            ("go", "cgo_enabled", "0"),
            ("go", "cc", "othercc"),
            ("go", "cxx", "othercxx"),
            ("native", "cc_binary_sha256", "c" * 64),
            ("native", "cxx_binary_sha256", "d" * 64),
            ("native", "target", "other-target"),
            ("native", "version_first_line", "Other compiler"),
        ]
        for section, field, replacement in cases:
            with self.subTest(section=section, field=field):
                self.assert_plan_tamper_rejected(
                    lambda value, section=section, field=field, replacement=replacement: value[
                        "repositories"
                    ][0]["toolchain"][section].__setitem__(field, replacement),
                    "dependency rebuild",
                    dependency_check=True,
                )

    def test_dependency_and_registry_hashes_are_exact(self) -> None:
        cases: list[tuple[str, Callable[[dict[str, Any]], None]]] = [
            (
                "scanner",
                lambda value: value["dependencies"]["eligibility_scanner"].__setitem__("artifact_sha256", "a" * 64),
            ),
            (
                "eligibility schema",
                lambda value: value["dependencies"]["eligibility_schema"].__setitem__("artifact_sha256", "b" * 64),
            ),
            (
                "contract",
                lambda value: value["dependencies"]["repository_contract"].__setitem__("artifact_sha256", "c" * 64),
            ),
            (
                "registry raw",
                lambda value: value["dependencies"]["overlap_registry"].__setitem__("artifact_sha256", "d" * 64),
            ),
            (
                "registry self",
                lambda value: value["dependencies"]["overlap_registry"].__setitem__("registry_sha256", "e" * 64),
            ),
            (
                "registry builder",
                lambda value: value["dependencies"]["overlap_registry"].__setitem__("builder_sha256", "f" * 64),
            ),
            (
                "registry CLI scanner",
                lambda value: value["dependencies"]["overlap_registry"].__setitem__("cli_scanner_sha256", "4" * 64),
            ),
            (
                "registry CLI canonical helper",
                lambda value: value["dependencies"]["overlap_registry"].__setitem__("cli_task_population_sha256", "5" * 64),
            ),
            (
                "registry Git binary",
                lambda value: value["dependencies"]["overlap_registry"].__setitem__("git_binary_sha256", "6" * 64),
            ),
            (
                "registry Git version",
                lambda value: value["dependencies"]["overlap_registry"].__setitem__("git_version_output", "git version 99.0.0"),
            ),
            (
                "registry schema",
                lambda value: value["dependencies"]["overlap_registry"].__setitem__("schema_sha256", "1" * 64),
            ),
            (
                "plan builder",
                lambda value: value["implementation"].__setitem__("builder_sha256", "2" * 64),
            ),
            (
                "plan schema",
                lambda value: value["implementation"].__setitem__("schema_sha256", "3" * 64),
            ),
        ]
        for label, mutate in cases:
            with self.subTest(label=label):
                self.assert_plan_tamper_rejected(
                    mutate, "dependency rebuild", dependency_check=True
                )

    def test_registry_dependency_binds_current_cli_helpers_and_contract_git(self) -> None:
        dependency = self.checked_plan["dependencies"]["overlap_registry"]
        self.assertIsInstance(dependency, dict)
        assert isinstance(dependency, dict)
        self.assertEqual(
            dependency["cli_scanner_sha256"],
            plan_v2._sha256(pathlib.Path(plan_v2.cli_eligibility.__file__).read_bytes()),
        )
        self.assertEqual(
            dependency["cli_task_population_sha256"],
            plan_v2._sha256(pathlib.Path(plan_v2.cli_population.__file__).read_bytes()),
        )
        git_bindings = [
            repository["toolchain"]["git"]
            for repository in self.checked_plan["repositories"]
        ]
        self.assertTrue(all(binding == git_bindings[0] for binding in git_bindings))
        self.assertEqual(dependency["git_binary_sha256"], git_bindings[0]["binary_sha256"])
        self.assertEqual(dependency["git_version_output"], git_bindings[0]["version_output"])

        with tempfile.TemporaryDirectory() as temporary:
            changed = pathlib.Path(temporary) / "changed-helper.py"
            changed.write_text("# changed helper bytes\n", encoding="utf-8")
            for module, pattern in (
                (plan_v2.cli_eligibility, "CLI scanner hash differs"),
                (plan_v2.cli_population, "CLI canonical-helper hash differs"),
            ):
                with self.subTest(module=module.__name__):
                    with (
                        mock.patch.object(module, "__file__", str(changed)),
                        self.assertRaisesRegex(plan_v2.RunPlanError, pattern),
                    ):
                        plan_v2.build_plan(**self.kwargs)

    def test_cache_log_isolation_and_pending_gate_overclaims_are_rejected(self) -> None:
        cases: list[tuple[str, Callable[[dict[str, Any]], None], str]] = [
            (
                "fabricated seed",
                lambda value: value["cache_seed"].__setitem__("manifest_sha256", "a" * 64),
                "cache-seed gate differs",
            ),
            (
                "host cache",
                lambda value: value["cache_seed"].__setitem__("host_shared_cache_reuse", "allowed"),
                "cache-seed gate differs",
            ),
            (
                "network enforced",
                lambda value: value["isolation"].__setitem__("network_os_enforcement", "enforced"),
                "isolation claims differ",
            ),
            (
                "filesystem enforced",
                lambda value: value["isolation"].__setitem__("filesystem_os_enforcement", "enforced"),
                "isolation claims differ",
            ),
            (
                "private root path",
                lambda value: value["private_log_policy"].__setitem__("private_root_locator", "/tmp/logs"),
                "private-log policy differs",
            ),
            (
                "private root mode",
                lambda value: value["private_log_policy"].__setitem__("private_root_mode", "0755"),
                "private-log policy differs",
            ),
            (
                "private file mode",
                lambda value: value["private_log_policy"].__setitem__("private_file_mode", "0644"),
                "private-log policy differs",
            ),
            (
                "retention",
                lambda value: value["private_log_policy"].__setitem__("raw_log_retention_days", 8),
                "private-log policy differs",
            ),
            (
                "drop gate",
                lambda value: value["pending_gates"].pop(),
                "pending gates differ",
            ),
        ]
        for label, mutate, pattern in cases:
            with self.subTest(label=label):
                self.assert_plan_tamper_rejected(mutate, pattern)

    def test_actual_input_byte_and_self_hash_tampering_fails_closed(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            raw_tampered = root / self.ledgers[0].name
            raw_tampered.write_bytes(self.ledgers[0].read_bytes() + b" ")
            paths = [raw_tampered, *self.ledgers[1:]]
            with self.assertRaisesRegex(
                plan_v2.RunPlanError,
                "schema repository projection differs|overlap-registry ledger raw hash differs",
            ):
                plan_v2.build_plan(**{**self.kwargs, "ledger_paths": paths})

            value = json.loads(self.ledgers[0].read_text(encoding="utf-8"))
            value["ledger_sha256"] = "a" * 64
            self_tampered = root / "self-tampered.json"
            self_tampered.write_text(json.dumps(value), encoding="utf-8")
            paths = [self_tampered, *self.ledgers[1:]]
            with self.assertRaisesRegex(plan_v2.RunPlanError, "self hash mismatch"):
                plan_v2.build_plan(**{**self.kwargs, "ledger_paths": paths})

    def test_duplicate_keys_floats_and_noncanonical_plan_bytes_are_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            duplicate = root / "duplicate.json"
            duplicate.write_text('{"profile":"a","profile":"b"}', encoding="utf-8")
            with self.assertRaisesRegex(plan_v2.RunPlanError, "duplicate object key"):
                plan_v2._load_json(duplicate)
            with self.assertRaisesRegex(plan_v2.RunPlanError, "floating-point"):
                plan_v2._canonical_json_bytes({"value": 1.0})
            noncanonical = root / "plan.json"
            noncanonical.write_bytes(self.checked_raw + b"\n")
            value, raw = plan_v2._load_json(noncanonical)
            self.assertNotEqual(raw, plan_v2._render(value))

    def test_public_plan_contains_no_host_path_or_secret_value(self) -> None:
        rendered = self.checked_raw.decode("utf-8")
        for forbidden in (
            "/Users/",
            "/home/",
            "/tmp/",
            "API_KEY",
            "PASSWORD",
            "SECRET=",
            "TOKEN=",
            "PRIVATE KEY",
        ):
            with self.subTest(forbidden=forbidden):
                self.assertNotIn(forbidden, rendered)


if __name__ == "__main__":
    unittest.main()
