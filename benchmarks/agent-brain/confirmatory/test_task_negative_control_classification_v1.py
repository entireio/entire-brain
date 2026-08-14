from __future__ import annotations

import ast
import contextlib
import copy
import hashlib
import io
import json
import pathlib
import tempfile
import unittest
from itertools import product
from unittest import mock
from typing import Any

import negative_control_private_log as private_log
import task_negative_control as legacy
import task_negative_control_classification_v1 as classification
import task_negative_control_gate_v1 as gate
import task_negative_control_plan_v2 as run_plan


GIB = 1024**3


class NegativeControlClassificationV1Test(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.root = pathlib.Path(classification.__file__).parent
        cls.plan_path = cls.root / "development-task-negative-control-run-plan-v2.json"
        cls.plan, cls.plan_raw = run_plan._load_json(cls.plan_path)
        cls.gate_manifest_schema = (
            cls.root / "schemas" / "offline-go-cache-seed-manifest-v1.schema.json"
        )
        cls.gate_receipt_schema = (
            cls.root / "schemas" / "negative-control-gate-primitive-receipt-v1.schema.json"
        )
        cls.attempt_schema = (
            cls.root / "schemas" / "negative-control-attempt-observations-v1.schema.json"
        )
        cls.receipt_schema = (
            cls.root / "schemas" / "negative-control-classification-receipt-v1.schema.json"
        )
        cls.private_log_schema = (
            cls.root / "schemas" / "negative-control-private-log-receipt-v1.schema.json"
        )
        entries = [
            {
                "byte_count": 11,
                "path": "gocache/00/brain.a",
                "repository_key": "entire-brain",
                "sha256": gate._sha256(b"synthetic brain cache entry"),
            },
            {
                "byte_count": 13,
                "path": "gocache/01/db.a",
                "repository_key": "entire-db",
                "sha256": gate._sha256(b"synthetic db cache entry"),
            },
            {
                "byte_count": 17,
                "path": "gomodcache/example.invalid/graph@v1/mod.zip",
                "repository_key": "entire-graph",
                "sha256": gate._sha256(b"synthetic graph cache entry"),
            },
        ]
        cls.gate_manifest = gate.build_cache_seed_manifest(
            plan=cls.plan,
            manifest_schema_path=cls.gate_manifest_schema,
            archive_file="synthetic-offline-cache.tar",
            archive_sha256=gate._sha256(b"synthetic cache archive; never executed"),
            archive_byte_count=1024,
            entries=entries,
        )
        cls.gate_manifest_raw = gate._render(cls.gate_manifest)
        archive = cls.gate_manifest["archive"]
        contents = cls.gate_manifest["contents"]
        cls.gate_receipt = gate.build_preflight_receipt(
            plan=cls.plan,
            plan_raw=cls.plan_raw,
            manifest=cls.gate_manifest,
            manifest_raw=cls.gate_manifest_raw,
            manifest_schema_path=cls.gate_manifest_schema,
            receipt_schema_path=cls.gate_receipt_schema,
            archive_observation={
                "archive_byte_count": archive["byte_count"],
                "archive_sha256": archive["sha256"],
                "content_inventory_sha256": contents["inventory_sha256"],
                "file_count": contents["file_count"],
                "observation_kind": gate.CACHE_ARCHIVE_OBSERVATION_KIND,
                "unpacked_byte_count": contents["unpacked_byte_count"],
            },
            filesystem_stats={
                "available_blocks": 16 * GIB,
                "fragment_size_bytes": 1,
            },
        )
        cls.gate_receipt_raw = gate._render(cls.gate_receipt)
        cls.dependencies = {
            "plan": cls.plan,
            "plan_raw": cls.plan_raw,
            "gate_manifest": cls.gate_manifest,
            "gate_manifest_raw": cls.gate_manifest_raw,
            "gate_receipt": cls.gate_receipt,
            "gate_receipt_raw": cls.gate_receipt_raw,
            "gate_manifest_schema_path": cls.gate_manifest_schema,
            "gate_receipt_schema_path": cls.gate_receipt_schema,
            "attempt_schema_path": cls.attempt_schema,
            "receipt_schema_path": cls.receipt_schema,
            "private_log_schema_path": cls.private_log_schema,
        }
        cls.attempts = cls.make_attempts()
        cls.attempt_manifest = classification.build_attempt_manifest(
            cls.attempts,
            **cls.dependencies,
        )
        cls.attempt_manifest_raw = classification._render(cls.attempt_manifest)
        cls.receipt = classification.build_classification_receipt(
            cls.attempt_manifest,
            attempt_manifest_raw=cls.attempt_manifest_raw,
            **cls.dependencies,
        )

    @classmethod
    def make_attempts(
        cls,
        outcome_overrides: dict[int, tuple[list[str], list[str]]] | None = None,
        *,
        fixed_log_size: int | None = None,
    ) -> list[dict[str, Any]]:
        overrides = outcome_overrides or {}
        schedule = classification._expected_schedule(cls.plan)
        attempts: list[dict[str, Any]] = []
        for index, expected in enumerate(schedule):
            candidate_index = index // 4
            baseline, reversed_source = overrides.get(
                candidate_index,
                (["passed", "passed"], ["failed", "failed"]),
            )
            pair = baseline if expected["arm"] == "baseline" else reversed_source
            outcome = pair[expected["repetition"] - 1]
            payload = f"synthetic-output-{index + 1}-{outcome}".encode()
            raw_digest = hashlib.sha256(payload).hexdigest()
            raw_byte_count = len(payload) if fixed_log_size is None else fixed_log_size
            raw_receipt = private_log.RawLogReceipt(
                raw_log_sha256=raw_digest,
                raw_log_byte_count=raw_byte_count,
            ).canonical_bytes()
            attempts.append(
                {
                    **expected,
                    "exit_code": (
                        None if outcome == "timeout" else (0 if outcome == "passed" else 1)
                    ),
                    "observation_kind": classification.OBSERVATION_KIND,
                    "raw_log": {
                        "raw_log_byte_count": raw_byte_count,
                        "raw_log_sha256": raw_digest,
                        "receipt_file_sha256": hashlib.sha256(raw_receipt).hexdigest(),
                    },
                    "status": "timeout" if outcome == "timeout" else "completed",
                    "test_command_sha256": hashlib.sha256(
                        f"synthetic-command-{candidate_index}".encode()
                    ).hexdigest(),
                }
            )
        return attempts

    def build_manifest(self, attempts: list[dict[str, Any]]) -> dict[str, Any]:
        return classification.build_attempt_manifest(attempts, **self.dependencies)

    def build_receipt(self, manifest: dict[str, Any]) -> dict[str, Any]:
        return classification.build_classification_receipt(
            manifest,
            attempt_manifest_raw=classification._render(manifest),
            **self.dependencies,
        )

    def validate_manifest(self, manifest: dict[str, Any]) -> None:
        classification.validate_attempt_manifest(manifest, **self.dependencies)

    def validate_receipt(self, receipt: dict[str, Any]) -> None:
        classification.validate_classification_receipt(
            receipt,
            attempt_manifest=self.attempt_manifest,
            attempt_manifest_raw=self.attempt_manifest_raw,
            **self.dependencies,
        )

    @staticmethod
    def reseal(value: dict[str, Any], field: str) -> None:
        value[field] = classification._self_hash(value, field)

    def test_exact_schedule_bindings_and_default_receipt(self) -> None:
        self.assertEqual(len(self.attempt_manifest["attempts"]), 248)
        self.assertEqual(len(self.receipt["classifications"]), 62)
        self.assertEqual(
            self.receipt["summary"]["classification_counts"],
            {
                "eligible_for_symptom_review": 62,
                "negative_control_survived": 0,
                "baseline_invalid_failure": 0,
                "baseline_invalid_timeout": 0,
                "baseline_inconsistent": 0,
                "reversed_invalid_timeout": 0,
                "reversed_inconsistent": 0,
            },
        )
        self.assertEqual(self.receipt["authority"]["candidate_execution"], "forbidden_plan_unexecutable")
        self.assertEqual(self.receipt["execution_status"], classification.EXECUTION_STATUS)
        self.assertEqual(self.attempt_manifest["exposure"], self.plan["exposure"])
        self.assertEqual(self.receipt["exposure"], self.plan["exposure"])
        self.assertEqual(self.receipt["inputs"]["plan_sha256"], gate.CHECKED_PLAN_SHA256)
        self.assertEqual(
            self.receipt["inputs"]["attempt_manifest_file_sha256"],
            hashlib.sha256(self.attempt_manifest_raw).hexdigest(),
        )
        classification.validate_classification_receipt(
            self.receipt,
            attempt_manifest=self.attempt_manifest,
            attempt_manifest_raw=self.attempt_manifest_raw,
            **self.dependencies,
        )

    def test_all_81_outcome_combinations_follow_precedence(self) -> None:
        states = ("passed", "failed", "timeout")
        for values in product(states, repeat=4):
            baseline = list(values[:2])
            reversed_source = list(values[2:])
            if "timeout" in baseline:
                expected = "baseline_invalid_timeout"
            elif baseline.count("failed") == 2:
                expected = "baseline_invalid_failure"
            elif baseline.count("failed") == 1:
                expected = "baseline_inconsistent"
            elif "timeout" in reversed_source:
                expected = "reversed_invalid_timeout"
            elif reversed_source.count("failed") == 2:
                expected = "eligible_for_symptom_review"
            elif reversed_source.count("failed") == 1:
                expected = "reversed_inconsistent"
            else:
                expected = "negative_control_survived"
            with self.subTest(values=values):
                self.assertEqual(
                    classification.classify_candidate(baseline, reversed_source),
                    expected,
                )

    def test_one_repetition_homogeneous_cases_preserve_legacy_semantics(self) -> None:
        def old(status: str, exit_code: int | None) -> dict[str, Any]:
            return {
                "status": status,
                "exit_code": exit_code,
                "output_sha256": "a" * 64,
                "output_byte_count": 1,
            }

        cases = [
            (old("timeout", None), old("completed", 1), "baseline_invalid_timeout"),
            (old("completed", 1), old("completed", 1), "baseline_invalid_failure"),
            (old("completed", 0), old("timeout", None), "reversed_invalid_timeout"),
            (old("completed", 0), old("completed", 0), "negative_control_survived"),
            (old("completed", 0), old("completed", 1), "eligible_for_symptom_review"),
        ]
        outcome = lambda item: (
            "timeout"
            if item["status"] == "timeout"
            else ("passed" if item["exit_code"] == 0 else "failed")
        )
        for baseline, reversed_source, expected in cases:
            with self.subTest(expected=expected):
                self.assertEqual(legacy._classification(baseline, reversed_source), expected)
                self.assertEqual(
                    classification.classify_candidate(
                        [outcome(baseline)] * 2,
                        [outcome(reversed_source)] * 2,
                    ),
                    expected,
                )

    def test_receipt_can_list_every_classification(self) -> None:
        variants = {
            0: (["passed", "passed"], ["failed", "failed"]),
            1: (["passed", "passed"], ["passed", "passed"]),
            2: (["failed", "failed"], ["passed", "passed"]),
            3: (["timeout", "passed"], ["passed", "passed"]),
            4: (["passed", "failed"], ["failed", "failed"]),
            5: (["passed", "passed"], ["timeout", "failed"]),
            6: (["passed", "passed"], ["passed", "failed"]),
        }
        receipt = self.build_receipt(self.build_manifest(self.make_attempts(variants)))
        observed = {
            row["classification"] for row in receipt["classifications"][:7]
        }
        self.assertEqual(observed, set(classification.CLASSIFICATIONS))

    def test_missing_extra_duplicate_and_reordered_attempts_fail_closed(self) -> None:
        cases = []
        missing = copy.deepcopy(self.attempts[:-1])
        cases.append(missing)
        extra = copy.deepcopy(self.attempts) + [copy.deepcopy(self.attempts[-1])]
        cases.append(extra)
        duplicate = copy.deepcopy(self.attempts)
        duplicate[1] = copy.deepcopy(duplicate[0])
        cases.append(duplicate)
        reordered = copy.deepcopy(self.attempts)
        reordered[0], reordered[1] = reordered[1], reordered[0]
        cases.append(reordered)
        for attempts in cases:
            with self.subTest(length=len(attempts)), self.assertRaises(
                classification.ClassificationError
            ):
                self.build_manifest(attempts)

    def test_status_exit_command_and_hostile_values_fail_closed(self) -> None:
        mutations: list[tuple[str, Any]] = [
            ("completed-null", lambda value: value[0].__setitem__("exit_code", None)),
            ("timeout-exit", lambda value: (value[0].__setitem__("status", "timeout"), value[0].__setitem__("exit_code", 1))),
            ("negative-exit", lambda value: value[0].__setitem__("exit_code", -1)),
            ("large-exit", lambda value: value[0].__setitem__("exit_code", 256)),
            ("bool-exit", lambda value: value[0].__setitem__("exit_code", True)),
            ("interrupted", lambda value: (value[0].__setitem__("status", "interrupted"), value[0].__setitem__("exit_code", None))),
            ("command-drift", lambda value: value[1].__setitem__("test_command_sha256", "b" * 64)),
            ("zero-command", lambda value: value[0].__setitem__("test_command_sha256", "0" * 64)),
        ]
        for label, mutate in mutations:
            attempts = copy.deepcopy(self.attempts)
            mutate(attempts)
            with self.subTest(label=label), self.assertRaises(classification.ClassificationError):
                self.build_manifest(attempts)

        class HostileInt(int):
            pass

        attempts = copy.deepcopy(self.attempts)
        attempts[0]["exit_code"] = HostileInt(0)
        with self.assertRaisesRegex(classification.ClassificationError, "exact built-in"):
            self.build_manifest(attempts)

        class HostileList(list[Any]):
            pass

        with self.assertRaisesRegex(classification.ClassificationError, "exact built-in array"):
            classification.build_attempt_manifest(HostileList(self.attempts), **self.dependencies)

    def test_boolean_numeric_aliases_fail_closed(self) -> None:
        for field in ("attempt_ordinal", "first_parent_position", "repetition"):
            attempts = copy.deepcopy(self.attempts)
            attempts[0][field] = True
            with self.subTest(attempt_field=field), self.assertRaisesRegex(
                classification.ClassificationError, "schedule identity"
            ):
                self.build_manifest(attempts)

        manifest_mutations: list[tuple[str, Any]] = [
            ("schema-version", lambda value: value.__setitem__("schema_version", True)),
            (
                "protocol-concurrency",
                lambda value: value["protocol"].__setitem__("max_concurrency", True),
            ),
            (
                "summary-zero",
                lambda value: value["summary"]["outcome_counts"]["baseline"].__setitem__(
                    "failed", False
                ),
            ),
        ]
        for label, mutate in manifest_mutations:
            manifest = copy.deepcopy(self.attempt_manifest)
            mutate(manifest)
            self.reseal(manifest, "manifest_sha256")
            with self.subTest(manifest=label), self.assertRaises(
                classification.ClassificationError
            ):
                self.validate_manifest(manifest)

        receipt_mutations: list[tuple[str, Any]] = [
            ("schema-version", lambda value: value.__setitem__("schema_version", True)),
            (
                "candidate-ordinal",
                lambda value: value["classifications"][0].__setitem__(
                    "candidate_ordinal", True
                ),
            ),
            (
                "classification-zero",
                lambda value: value["summary"]["classification_counts"].__setitem__(
                    "negative_control_survived", False
                ),
            ),
        ]
        for label, mutate in receipt_mutations:
            receipt = copy.deepcopy(self.receipt)
            mutate(receipt)
            self.reseal(receipt, "receipt_sha256")
            with self.subTest(receipt=label), self.assertRaises(
                classification.ClassificationError
            ):
                self.validate_receipt(receipt)

    def test_shared_canonical_parser_rejects_hostile_json(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            cases = (
                (
                    "duplicate.json",
                    b'{"profile":"first","profile":"second"}',
                    "duplicate object key",
                ),
                (
                    "float.json",
                    b'{"profile":1.5}',
                    "floating-point",
                ),
            )
            for name, raw, message in cases:
                path = root / name
                path.write_bytes(raw)
                with self.subTest(name=name), self.assertRaisesRegex(
                    classification.ClassificationError, message
                ):
                    classification._read_json(path, label="hostile fixture")

            oversized = root / "oversized.json"
            oversized.write_bytes(b" " * (classification.MAX_ARTIFACT_RAW_BYTES + 1))
            with self.assertRaisesRegex(
                classification.ClassificationError, "raw-byte ceiling"
            ):
                classification._read_json(oversized, label="hostile fixture")

        nested: Any = 0
        for _ in range(gate.MAX_JSON_DEPTH + 1):
            nested = [nested]
        with self.assertRaisesRegex(
            classification.ClassificationError, "nesting-depth ceiling"
        ):
            classification._canonical_json_bytes(nested)

    def test_raw_log_receipt_hash_bounds_and_unique_total_fail_closed(self) -> None:
        attempts = copy.deepcopy(self.attempts)
        attempts[0]["raw_log"]["receipt_file_sha256"] = "a" * 64
        with self.assertRaisesRegex(classification.ClassificationError, "receipt-file hash"):
            self.build_manifest(attempts)

        attempts = copy.deepcopy(self.attempts)
        attempts[0]["raw_log"]["raw_log_byte_count"] = (
            private_log.MAX_PRIVATE_RAW_LOG_BYTES_PER_ARM + 1
        )
        with self.assertRaisesRegex(classification.ClassificationError, "byte count"):
            self.build_manifest(attempts)

        oversized = self.make_attempts(
            fixed_log_size=private_log.MAX_PRIVATE_RAW_LOG_BYTES_PER_ARM
        )
        with self.assertRaisesRegex(classification.ClassificationError, "unique private"):
            self.build_manifest(oversized)

        duplicate = self.make_attempts(
            fixed_log_size=private_log.MAX_PRIVATE_RAW_LOG_BYTES_PER_ARM
        )
        shared_digest = duplicate[0]["raw_log"]["raw_log_sha256"]
        shared_receipt = private_log.RawLogReceipt(
            raw_log_sha256=shared_digest,
            raw_log_byte_count=private_log.MAX_PRIVATE_RAW_LOG_BYTES_PER_ARM,
        ).canonical_bytes()
        for attempt in duplicate:
            attempt["raw_log"]["raw_log_sha256"] = shared_digest
            attempt["raw_log"]["receipt_file_sha256"] = hashlib.sha256(shared_receipt).hexdigest()
        manifest = self.build_manifest(duplicate)
        self.assertEqual(manifest["summary"]["raw_log_unique_count"], 1)

        mismatched_duplicate = copy.deepcopy(self.attempts)
        mismatched_duplicate[1]["raw_log"]["raw_log_sha256"] = mismatched_duplicate[0][
            "raw_log"
        ]["raw_log_sha256"]
        mismatched_duplicate[1]["raw_log"]["raw_log_byte_count"] += 1
        mismatched_receipt = private_log.RawLogReceipt(
            raw_log_sha256=mismatched_duplicate[1]["raw_log"]["raw_log_sha256"],
            raw_log_byte_count=mismatched_duplicate[1]["raw_log"]["raw_log_byte_count"],
        ).canonical_bytes()
        mismatched_duplicate[1]["raw_log"]["receipt_file_sha256"] = hashlib.sha256(
            mismatched_receipt
        ).hexdigest()
        with self.assertRaisesRegex(classification.ClassificationError, "different size"):
            self.build_manifest(mismatched_duplicate)

        zero_digest = copy.deepcopy(self.attempts)
        zero_digest[0]["raw_log"]["raw_log_sha256"] = "0" * 64
        zero_digest[0]["raw_log"]["raw_log_byte_count"] = 0
        zero_receipt = private_log.RawLogReceipt(
            raw_log_sha256="0" * 64,
            raw_log_byte_count=0,
        ).canonical_bytes()
        zero_digest[0]["raw_log"]["receipt_file_sha256"] = hashlib.sha256(
            zero_receipt
        ).hexdigest()
        manifest = self.build_manifest(zero_digest)
        self.assertEqual(manifest["attempts"][0]["raw_log"]["raw_log_sha256"], "0" * 64)

    def test_resealed_semantic_and_dependency_tampering_fails(self) -> None:
        changed = copy.deepcopy(self.receipt)
        changed["classifications"][0]["classification"] = "negative_control_survived"
        self.reseal(changed, "receipt_sha256")
        with self.assertRaisesRegex(classification.ClassificationError, "truth table"):
            classification.validate_classification_receipt(
                changed,
                attempt_manifest=self.attempt_manifest,
                attempt_manifest_raw=self.attempt_manifest_raw,
                **self.dependencies,
            )

        changed = copy.deepcopy(self.receipt)
        changed["authority"]["candidate_execution"] = "authorized"
        self.reseal(changed, "receipt_sha256")
        with self.assertRaisesRegex(classification.ClassificationError, "authority"):
            classification.validate_classification_receipt(
                changed,
                attempt_manifest=self.attempt_manifest,
                attempt_manifest_raw=self.attempt_manifest_raw,
                **self.dependencies,
            )

        changed_manifest = copy.deepcopy(self.attempt_manifest)
        changed_manifest["inputs"]["plan_sha256"] = "a" * 64
        self.reseal(changed_manifest, "manifest_sha256")
        with self.assertRaisesRegex(classification.ClassificationError, "input bindings"):
            classification.validate_attempt_manifest(changed_manifest, **self.dependencies)

    def test_public_artifacts_never_contain_raw_output_paths_or_commands(self) -> None:
        rendered = (
            classification._render(self.attempt_manifest)
            + classification._render(self.receipt)
        ).decode("utf-8")
        for forbidden in (
            "synthetic-output-",
            "synthetic-command-",
            "/Users/",
            "/home/",
            "/tmp/",
            "SECRET=",
            "TOKEN=",
            "PRIVATE KEY",
        ):
            with self.subTest(forbidden=forbidden):
                self.assertNotIn(forbidden, rendered)

    def test_schema_hashes_and_exact_prefix_projections(self) -> None:
        self.assertEqual(
            hashlib.sha256(self.attempt_schema.read_bytes()).hexdigest(),
            classification.CHECKED_ATTEMPT_SCHEMA_SHA256,
        )
        self.assertEqual(
            hashlib.sha256(self.receipt_schema.read_bytes()).hexdigest(),
            classification.CHECKED_RECEIPT_SCHEMA_SHA256,
        )
        classification._validate_schema_projections(
            plan=self.plan,
            attempt_schema_path=self.attempt_schema,
            receipt_schema_path=self.receipt_schema,
        )
        for path, attempt_count, classification_count in (
            (self.attempt_schema, 248, None),
            (self.receipt_schema, 248, 62),
        ):
            schema = json.loads(path.read_text(encoding="utf-8"))
            self.assertEqual(len(schema["$defs"]["attempts"]["prefixItems"]), attempt_count)
            self.assertEqual(
                schema["$defs"]["sha256"]["pattern"],
                "^(?!0{64}$)[0-9a-f]{64}$",
            )
            self.assertEqual(
                schema["$defs"]["raw_sha256"]["pattern"],
                "^[0-9a-f]{64}$",
            )
            self.assertEqual(
                schema["properties"]["exposure"]["const"],
                self.plan["exposure"],
            )
            if classification_count is not None:
                self.assertEqual(
                    len(schema["$defs"]["classifications"]["prefixItems"]),
                    classification_count,
                )

    def test_noncanonical_attempt_bytes_and_cli_round_trip(self) -> None:
        with self.assertRaisesRegex(classification.ClassificationError, "not canonical"):
            classification.build_classification_receipt(
                self.attempt_manifest,
                attempt_manifest_raw=self.attempt_manifest_raw + b"\n",
                **self.dependencies,
            )
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            paths = {
                "plan": root / "plan.json",
                "gate_manifest": root / "gate-manifest.json",
                "gate_receipt": root / "gate-receipt.json",
                "attempts": root / "attempts.json",
                "receipt": root / "receipt.json",
            }
            paths["plan"].write_bytes(self.plan_raw)
            paths["gate_manifest"].write_bytes(self.gate_manifest_raw)
            paths["gate_receipt"].write_bytes(self.gate_receipt_raw)
            paths["attempts"].write_bytes(self.attempt_manifest_raw)
            common = [
                "--plan", str(paths["plan"]),
                "--gate-manifest", str(paths["gate_manifest"]),
                "--gate-receipt", str(paths["gate_receipt"]),
                "--gate-manifest-schema", str(self.gate_manifest_schema),
                "--gate-receipt-schema", str(self.gate_receipt_schema),
                "--attempt-schema", str(self.attempt_schema),
                "--receipt-schema", str(self.receipt_schema),
                "--private-log-schema", str(self.private_log_schema),
                "--attempt-manifest", str(paths["attempts"]),
            ]
            self.assertEqual(
                classification.main(["classify", *common, "--output", str(paths["receipt"])]),
                0,
            )
            self.assertEqual(
                classification.main(["check-receipt", str(paths["receipt"]), *common]),
                0,
            )
            self.assertEqual(paths["receipt"].read_bytes(), classification._render(self.receipt))

    def test_incomplete_cli_input_creates_no_receipt(self) -> None:
        incomplete = copy.deepcopy(self.attempt_manifest)
        incomplete["attempts"].pop()
        incomplete["attempts_sha256"] = classification._canonical_hash(incomplete["attempts"])
        self.reseal(incomplete, "manifest_sha256")
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            plan_path = root / "plan.json"
            manifest_path = root / "gate-manifest.json"
            gate_receipt_path = root / "gate-receipt.json"
            attempts_path = root / "attempts.json"
            output_path = root / "must-not-exist.json"
            plan_path.write_bytes(self.plan_raw)
            manifest_path.write_bytes(self.gate_manifest_raw)
            gate_receipt_path.write_bytes(self.gate_receipt_raw)
            attempts_path.write_bytes(classification._render(incomplete))
            arguments = [
                "classify",
                "--plan", str(plan_path),
                "--gate-manifest", str(manifest_path),
                "--gate-receipt", str(gate_receipt_path),
                "--gate-manifest-schema", str(self.gate_manifest_schema),
                "--gate-receipt-schema", str(self.gate_receipt_schema),
                "--attempt-schema", str(self.attempt_schema),
                "--receipt-schema", str(self.receipt_schema),
                "--private-log-schema", str(self.private_log_schema),
                "--attempt-manifest", str(attempts_path),
                "--output", str(output_path),
            ]
            with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
                classification.main(arguments)
            self.assertFalse(output_path.exists())

    def test_cli_file_errors_use_a_fixed_nonleaking_message(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            secret = root / "SECRET_TOKEN_must_not_leak.json"
            output = root / "receipt.json"
            arguments = [
                "classify",
                "--plan", str(secret),
                "--gate-manifest", str(secret),
                "--gate-receipt", str(secret),
                "--gate-manifest-schema", str(self.gate_manifest_schema),
                "--gate-receipt-schema", str(self.gate_receipt_schema),
                "--attempt-schema", str(self.attempt_schema),
                "--receipt-schema", str(self.receipt_schema),
                "--private-log-schema", str(self.private_log_schema),
                "--attempt-manifest", str(secret),
                "--output", str(output),
            ]
            stderr = io.StringIO()
            with contextlib.redirect_stderr(stderr), self.assertRaises(SystemExit) as raised:
                classification.main(arguments)
            self.assertEqual(raised.exception.code, 2)
            rendered_error = stderr.getvalue()
            self.assertIn("negative-control classification validation failed", rendered_error)
            self.assertNotIn("SECRET_TOKEN", rendered_error)
            self.assertNotIn(str(root), rendered_error)
            self.assertFalse(output.exists())

    def test_module_has_no_candidate_network_or_provider_execution_surface(self) -> None:
        source = pathlib.Path(classification.__file__).read_text(encoding="utf-8")
        tree = ast.parse(source)
        imported = {
            alias.name.split(".")[0]
            for node in ast.walk(tree)
            if isinstance(node, ast.Import)
            for alias in node.names
        }
        imported.update(
            node.module.split(".")[0]
            for node in ast.walk(tree)
            if isinstance(node, ast.ImportFrom) and node.module
        )
        self.assertTrue(
            imported.isdisjoint(
                {"subprocess", "socket", "urllib", "requests", "http", "pty", "multiprocessing"}
            )
        )
        function_names = {
            node.name for node in ast.walk(tree) if isinstance(node, ast.FunctionDef)
        }
        self.assertTrue(function_names.isdisjoint({"run", "execute", "run_candidate"}))
        self.assertNotIn("os.system", source)
        self.assertNotIn("os.exec", source)

    def test_keyboard_interrupt_before_replace_is_not_converted_or_published(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            output = pathlib.Path(temporary) / "receipt.json"
            cancellation = KeyboardInterrupt("synthetic cancellation")
            with (
                mock.patch.object(classification.os, "replace", side_effect=cancellation),
                self.assertRaises(KeyboardInterrupt) as raised,
            ):
                classification._write_atomic(output, self.receipt)
            self.assertIs(raised.exception, cancellation)
            self.assertFalse(output.exists())

    def test_atomic_replace_is_the_documented_publication_commit_point(self) -> None:
        real_replace = classification.os.replace

        def replace_then_interrupt(source: pathlib.Path, destination: pathlib.Path) -> None:
            real_replace(source, destination)
            raise KeyboardInterrupt("synthetic post-commit cancellation")

        with tempfile.TemporaryDirectory() as temporary:
            output = pathlib.Path(temporary) / "receipt.json"
            with (
                mock.patch.object(
                    classification.os,
                    "replace",
                    side_effect=replace_then_interrupt,
                ),
                self.assertRaises(KeyboardInterrupt),
            ):
                classification._write_atomic(output, self.receipt)
            self.assertEqual(output.read_bytes(), classification._render(self.receipt))
            self.assertIn(
                "fail_closed_cleanup_and_no_receipt_on_interruption",
                self.receipt["residual_gates"],
            )

    def test_publication_os_errors_are_fixed_and_nonleaking(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            output = pathlib.Path(temporary) / "receipt.json"
            with (
                mock.patch.object(
                    classification.os,
                    "replace",
                    side_effect=OSError("SECRET_TOKEN_must_not_leak"),
                ),
                self.assertRaisesRegex(
                    classification.ClassificationError,
                    "^classification receipt publication failed$",
                ) as raised,
            ):
                classification._write_atomic(output, self.receipt)
            self.assertNotIn("SECRET_TOKEN", str(raised.exception))
            self.assertFalse(output.exists())


if __name__ == "__main__":
    unittest.main()
