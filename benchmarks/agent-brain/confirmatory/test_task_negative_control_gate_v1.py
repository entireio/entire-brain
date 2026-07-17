from __future__ import annotations

import contextlib
import copy
import io
import json
import pathlib
import tempfile
import unittest
from unittest import mock
from typing import Any, Callable

import draft202012
import task_negative_control_gate_v1 as gate
import task_negative_control_plan_v2 as run_plan


GIB = 1024**3


class TaskNegativeControlGateV1Test(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.root = pathlib.Path(gate.__file__).parent
        cls.plan_path = cls.root / "development-task-negative-control-run-plan-v2.json"
        cls.plan, cls.plan_raw = run_plan._load_json(cls.plan_path)
        cls.manifest_schema = cls.root / "schemas" / "offline-go-cache-seed-manifest-v1.schema.json"
        cls.receipt_schema = cls.root / "schemas" / "negative-control-gate-primitive-receipt-v1.schema.json"
        cls.archive_sha256 = gate._sha256(b"synthetic offline cache archive; not executable")
        cls.entries = [
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
                "path": "gomodcache/example.org/graph@v1.0.0/mod.zip",
                "repository_key": "entire-graph",
                "sha256": gate._sha256(b"synthetic graph cache entry"),
            },
        ]
        cls.maximum_seed_bytes = cls.plan["resource_budget"]["max_cache_seed_bytes"]

    def build_manifest(
        self,
        *,
        entries: list[dict[str, Any]] | None = None,
        archive_byte_count: int = 1024,
        manifest_schema: pathlib.Path | None = None,
    ) -> dict[str, Any]:
        return gate.build_cache_seed_manifest(
            plan=self.plan,
            manifest_schema_path=manifest_schema or self.manifest_schema,
            archive_file="offline-go-cache-seed-v1.tar",
            archive_sha256=self.archive_sha256,
            archive_byte_count=archive_byte_count,
            entries=copy.deepcopy(entries if entries is not None else self.entries),
        )

    @staticmethod
    def reseal(value: dict[str, Any], field: str) -> None:
        value[field] = gate._self_hash(value, field)

    def observation(self, manifest: dict[str, Any]) -> dict[str, Any]:
        return {
            "archive_byte_count": manifest["archive"]["byte_count"],
            "archive_sha256": manifest["archive"]["sha256"],
            "content_inventory_sha256": manifest["contents"]["inventory_sha256"],
            "file_count": manifest["contents"]["file_count"],
            "observation_kind": gate.CACHE_ARCHIVE_OBSERVATION_KIND,
            "unpacked_byte_count": manifest["contents"]["unpacked_byte_count"],
        }

    @staticmethod
    def filesystem_stats(free_bytes: int) -> dict[str, int]:
        return {"available_blocks": free_bytes, "fragment_size_bytes": 1}

    def build_receipt(
        self,
        manifest: dict[str, Any],
        *,
        plan_raw: bytes | None = None,
        manifest_raw: bytes | None = None,
        archive_observation: dict[str, Any] | None = None,
        free_bytes: int = 16 * GIB,
        receipt_schema: pathlib.Path | None = None,
    ) -> dict[str, Any]:
        return gate.build_preflight_receipt(
            plan=self.plan,
            plan_raw=self.plan_raw if plan_raw is None else plan_raw,
            manifest=manifest,
            manifest_raw=gate._render(manifest) if manifest_raw is None else manifest_raw,
            manifest_schema_path=self.manifest_schema,
            receipt_schema_path=receipt_schema or self.receipt_schema,
            archive_observation=(
                self.observation(manifest)
                if archive_observation is None
                else archive_observation
            ),
            filesystem_stats=self.filesystem_stats(free_bytes),
        )

    def assert_manifest_rejected(
        self,
        mutate: Callable[[dict[str, Any]], None],
        pattern: str,
    ) -> None:
        manifest = self.build_manifest()
        mutate(manifest)
        self.reseal(manifest, "manifest_sha256")
        with self.assertRaisesRegex(gate.GatePrimitiveError, pattern):
            gate.validate_cache_seed_manifest(
                manifest,
                plan=self.plan,
                manifest_schema_path=self.manifest_schema,
            )

    def assert_cli_exit_2(self, arguments: list[str], pattern: str) -> str:
        stderr = io.StringIO()
        with (
            contextlib.redirect_stderr(stderr),
            self.assertRaises(SystemExit) as raised,
        ):
            gate.main(arguments)
        self.assertEqual(raised.exception.code, 2)
        output = stderr.getvalue()
        self.assertIn(pattern, output)
        self.assertNotIn("Traceback", output)
        return output

    def test_manifest_and_receipt_are_deterministic_self_bound_and_schema_valid(self) -> None:
        first = self.build_manifest()
        second = self.build_manifest()
        self.assertEqual(first, second)
        self.assertEqual(first["manifest_sha256"], gate._self_hash(first, "manifest_sha256"))
        self.assertEqual(first["authority"]["candidate_execution"], "forbidden_plan_unexecutable")
        self.assertEqual(first["run_plan"]["plan_sha256"], self.plan["plan_sha256"])
        self.assertEqual(
            first["run_plan"]["toolchain_bindings_sha256"],
            gate._canonical_hash(gate._toolchain_projection(self.plan)),
        )

        receipt = self.build_receipt(first)
        self.assertEqual(receipt, self.build_receipt(second))
        self.assertEqual(receipt["receipt_sha256"], gate._self_hash(receipt, "receipt_sha256"))
        self.assertEqual(receipt["status"], "primitive_checks_passed_execution_forbidden")
        self.assertEqual(receipt["execution_status"], "forbidden_missing_remaining_gates")
        self.assertEqual(receipt["cache_seed"]["observation_kind"], gate.CACHE_ARCHIVE_OBSERVATION_KIND)
        self.assertEqual(receipt["resource"]["observation_kind"], gate.FILESYSTEM_OBSERVATION_KIND)
        self.assertIn("authorized_plan_binding_for_actual_cache_seed", receipt["residual_gates"])
        self.assertIn(
            "safe_archive_traversal_type_link_device_and_content_verifier",
            receipt["residual_gates"],
        )

        for path, value, label in (
            (self.manifest_schema, first, "manifest"),
            (self.receipt_schema, receipt, "receipt"),
        ):
            schema = json.loads(path.read_text(encoding="utf-8"))
            validator = draft202012.Validator([draft202012.SchemaDocument(path.name, schema)])
            validator.validate(value, path.name, label=label)

    def test_gate_anchors_exact_plan_dependencies_and_full_toolchains(self) -> None:
        self.assertEqual(self.plan["plan_sha256"], gate.CHECKED_PLAN_SHA256)
        self.assertEqual(gate._sha256(self.plan_raw), gate.CHECKED_PLAN_FILE_SHA256)
        gate._pending_authority(self.plan)

        projection = gate._toolchain_projection(self.plan)
        self.assertEqual(
            [item["repository_key"] for item in projection],
            list(run_plan.REPOSITORY_ORDER),
        )
        self.assertTrue(
            all(set(item["toolchain"]) == {"git", "go", "native"} for item in projection)
        )

        drift_cases: list[tuple[str, Callable[[dict[str, Any]], None]]] = [
            (
                "Go",
                lambda value: value["repositories"][0]["toolchain"]["go"].__setitem__(
                    "version_output", "go version drift"
                ),
            ),
            (
                "native",
                lambda value: value["repositories"][0]["toolchain"]["native"].__setitem__(
                    "target", "drift-target"
                ),
            ),
            (
                "Git",
                lambda value: value["repositories"][0]["toolchain"]["git"].__setitem__(
                    "version_output", "git version drift"
                ),
            ),
            (
                "builder",
                lambda value: value["implementation"].__setitem__("builder_sha256", "a" * 64),
            ),
            (
                "schema",
                lambda value: value["implementation"].__setitem__("schema_sha256", "b" * 64),
            ),
            (
                "eligibility",
                lambda value: value["dependencies"]["eligibility_schema"].__setitem__(
                    "artifact_sha256", "c" * 64
                ),
            ),
            (
                "registry",
                lambda value: value["dependencies"]["overlap_registry"].__setitem__(
                    "registry_sha256", "d" * 64
                ),
            ),
            (
                "ledger",
                lambda value: value["repositories"][0].__setitem__("ledger_file_sha256", "e" * 64),
            ),
            (
                "candidate",
                lambda value: value["repositories"][0]["candidate_order"][0].__setitem__(
                    "candidate_ref", "f" * 64
                ),
            ),
        ]
        for label, mutate in drift_cases:
            changed = copy.deepcopy(self.plan)
            mutate(changed)
            changed["plan_sha256"] = run_plan._self_hash(changed)
            with self.subTest(label=label), self.assertRaises(gate.GatePrimitiveError):
                gate._pending_authority(changed)

        with (
            mock.patch.object(
                run_plan,
                "verify_plan_dependencies",
                side_effect=run_plan.RunPlanError("synthetic exact dependency drift"),
            ),
            self.assertRaisesRegex(gate.GatePrimitiveError, "exact dependency verification failed"),
        ):
            gate._pending_authority(self.plan)

    def test_runtime_unicode_and_transitive_verifier_sources_are_exactly_bound(self) -> None:
        manifest = self.build_manifest()
        implementation = manifest["implementation"]
        self.assertEqual(implementation["python_implementation"], gate.platform.python_implementation())
        self.assertEqual(implementation["python_version"], gate.platform.python_version())
        self.assertEqual(implementation["unicode_data_version"], gate.unicodedata.unidata_version)
        self.assertEqual(implementation["verifier_sources"], gate._verifier_source_hashes())
        self.assertEqual(
            implementation["run_plan_builder_sha256"],
            implementation["verifier_sources"]["task_negative_control_plan_v2.py"],
        )
        self.assertEqual(
            implementation["schema_validator_sha256"],
            implementation["verifier_sources"]["draft202012.py"],
        )

        for field in ("python_version", "unicode_data_version", "python_executable_sha256"):
            changed = copy.deepcopy(manifest)
            changed["implementation"][field] = "a" * 64
            self.reseal(changed, "manifest_sha256")
            with self.subTest(field=field), self.assertRaisesRegex(
                gate.GatePrimitiveError,
                "implementation binding differs",
            ):
                gate.validate_cache_seed_manifest(
                    changed,
                    plan=self.plan,
                    manifest_schema_path=self.manifest_schema,
                )

        changed_sources = copy.deepcopy(implementation["verifier_sources"])
        changed_sources["task_population.py"] = "b" * 64
        with (
            mock.patch.object(gate, "_verifier_source_hashes", return_value=changed_sources),
            self.assertRaisesRegex(gate.GatePrimitiveError, "implementation binding differs"),
        ):
            gate.validate_cache_seed_manifest(
                manifest,
                plan=self.plan,
                manifest_schema_path=self.manifest_schema,
            )

    def test_manifest_rejects_path_collisions_traversal_order_and_missing_repository(self) -> None:
        duplicate = copy.deepcopy(self.entries)
        duplicate[1]["path"] = duplicate[0]["path"]
        with self.assertRaisesRegex(gate.GatePrimitiveError, "paths collide globally"):
            self.build_manifest(entries=duplicate)

        for path in (
            "/gocache/host",
            "gocache/../escape",
            "gocache//double",
            "gocache",
            "other/cache",
            "gocache\\windows",
            "gocache/e\u0301/non-nfc",
            "gocache/control\u0001/name",
            "gocache/\ud800",
        ):
            entries = copy.deepcopy(self.entries)
            entries[0]["path"] = path
            with self.subTest(path=path), self.assertRaisesRegex(
                gate.GatePrimitiveError,
                "canonical relative path|cache roots|encoding",
            ):
                self.build_manifest(entries=entries)

        with self.assertRaisesRegex(gate.GatePrimitiveError, "entries are not canonical"):
            self.build_manifest(entries=list(reversed(copy.deepcopy(self.entries))))
        missing_repository = copy.deepcopy(self.entries)
        missing_repository[2]["repository_key"] = "entire-db"
        missing_repository[2]["path"] = "gomodcache/example.org/db@v1.0.0/mod.zip"
        missing_repository.sort(key=lambda entry: (entry["repository_key"], entry["path"]))
        with self.assertRaisesRegex(gate.GatePrimitiveError, "every repository"):
            self.build_manifest(entries=missing_repository)

    def test_portable_path_key_rejects_casefold_unicode_and_ancestor_aliases(self) -> None:
        collision_pairs = (
            ("gocache/Case/item.a", "gocache/case/item.a"),
            ("gocache/straße/item.a", "gocache/strasse/item.a"),
            ("gocache/ΐ/item.a", "gocache/Ϊ\u0301/item.a"),
        )
        for first, second in collision_pairs:
            self.assertEqual(gate._portable_path_key(first), gate._portable_path_key(second))
            self.assertEqual(gate.unicodedata.normalize("NFC", first), first)
            self.assertEqual(gate.unicodedata.normalize("NFC", second), second)
            entries = copy.deepcopy(self.entries)
            entries[0]["path"] = first
            entries[1]["path"] = second
            with self.subTest(first=first, second=second), self.assertRaisesRegex(
                gate.GatePrimitiveError,
                "portable path key",
            ):
                self.build_manifest(entries=entries)

        self.assertEqual(
            gate._portable_path_key("gocache/é/item.a"),
            gate._portable_path_key("gocache/e\u0301/item.a"),
        )

        ancestor_assignments = (
            ("gocache/tree", "gocache/tree/item.a"),
            ("gocache/tree/item.a", "gocache/tree"),
        )
        for first, second in ancestor_assignments:
            entries = copy.deepcopy(self.entries)
            entries[0]["path"] = first
            entries[1]["path"] = second
            with self.subTest(first=first, second=second), self.assertRaisesRegex(
                gate.GatePrimitiveError,
                "portable ancestor conflict",
            ):
                self.build_manifest(entries=entries)

    def test_manifest_rejects_noncanonical_archive_file_and_frozen_byte_ceiling(self) -> None:
        for artifact_file in ("../seed.tar", "/tmp/seed.tar", ".", "..", "e\u0301.tar", "bad\nname.tar"):
            with self.subTest(artifact_file=artifact_file), self.assertRaisesRegex(
                gate.GatePrimitiveError,
                "canonical file name|encoding",
            ):
                gate.build_cache_seed_manifest(
                    plan=self.plan,
                    manifest_schema_path=self.manifest_schema,
                    archive_file=artifact_file,
                    archive_sha256=self.archive_sha256,
                    archive_byte_count=1024,
                    entries=copy.deepcopy(self.entries),
                )

        maximum = self.maximum_seed_bytes
        with self.assertRaisesRegex(gate.GatePrimitiveError, "archive exceeds"):
            self.build_manifest(archive_byte_count=maximum + 1)
        entries = copy.deepcopy(self.entries)
        entries[0]["byte_count"] = maximum + 1
        with self.assertRaisesRegex(gate.GatePrimitiveError, "byte_count exceeds"):
            self.build_manifest(entries=entries)

    def test_manifest_profile_limits_cover_boundaries_zeroes_aggregate_and_raw_bytes(self) -> None:
        gate._validate_manifest_profile_limits(
            file_count=gate.MAX_MANIFEST_FILE_COUNT,
            rendered_byte_count=gate.MAX_MANIFEST_RAW_BYTES,
        )
        for file_count, rendered_byte_count in (
            (gate.MAX_MANIFEST_FILE_COUNT + 1, gate.MAX_MANIFEST_RAW_BYTES),
            (gate.MAX_MANIFEST_FILE_COUNT, gate.MAX_MANIFEST_RAW_BYTES + 1),
        ):
            with self.subTest(
                file_count=file_count,
                rendered_byte_count=rendered_byte_count,
            ), self.assertRaisesRegex(gate.GatePrimitiveError, "profile ceiling"):
                gate._validate_manifest_profile_limits(
                    file_count=file_count,
                    rendered_byte_count=rendered_byte_count,
                )

        zero_entries = copy.deepcopy(self.entries)
        for entry in zero_entries:
            entry["byte_count"] = 0
        with self.assertRaisesRegex(gate.GatePrimitiveError, "byte_count is invalid"):
            self.build_manifest(entries=zero_entries)

        aggregate_entries = copy.deepcopy(self.entries)
        aggregate_entries[0]["byte_count"] = self.maximum_seed_bytes // 2
        aggregate_entries[1]["byte_count"] = self.maximum_seed_bytes // 2
        aggregate_entries[2]["byte_count"] = 1
        with self.assertRaisesRegex(gate.GatePrimitiveError, "unpacked cache seed exceeds"):
            self.build_manifest(entries=aggregate_entries)

        schema = json.loads(self.manifest_schema.read_text(encoding="utf-8"))
        entries_schema = schema["properties"]["contents"]["properties"]["entries"]
        self.assertEqual(entries_schema["maxItems"], gate.MAX_MANIFEST_FILE_COUNT)
        self.assertNotIn("uniqueItems", entries_schema)
        self.assertEqual(
            entries_schema["items"]["properties"]["byte_count"]["minimum"],
            1,
        )

        oversized_count = self.build_manifest()
        oversized_count["contents"]["entries"] = [
            oversized_count["contents"]["entries"][0]
        ] * (gate.MAX_MANIFEST_FILE_COUNT + 1)
        with (
            mock.patch.object(
                gate,
                "_render",
                side_effect=AssertionError("render must not run before count rejection"),
            ),
            self.assertRaisesRegex(gate.GatePrimitiveError, "file count"),
        ):
            gate.validate_cache_seed_manifest(
                oversized_count,
                plan=self.plan,
                manifest_schema_path=self.manifest_schema,
            )

        oversized_scalar = self.build_manifest()
        oversized_scalar["contents"]["entries"][0]["path"] = "gocache/" + "x" * 1_000_000
        with (
            mock.patch.object(
                gate,
                "_render",
                side_effect=AssertionError("render must not run before scalar rejection"),
            ),
            self.assertRaisesRegex(gate.GatePrimitiveError, "too long"),
        ):
            gate.validate_cache_seed_manifest(
                oversized_scalar,
                plan=self.plan,
                manifest_schema_path=self.manifest_schema,
            )

        manifest = self.build_manifest()
        manifest_raw = gate._render(manifest)
        with (
            mock.patch.object(gate, "MAX_MANIFEST_RAW_BYTES", len(manifest_raw) - 1),
            self.assertRaisesRegex(gate.GatePrimitiveError, "rendered bytes"),
        ):
            gate.validate_cache_seed_manifest(
                manifest,
                plan=self.plan,
                manifest_schema_path=self.manifest_schema,
            )

        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            path = root / "bounded.json"
            payload = b'{"value":1}'
            path.write_bytes(payload)
            self.assertEqual(
                gate._read_bounded(
                    path,
                    max_raw_bytes=len(payload),
                    label="boundary input",
                ),
                payload,
            )
            with self.assertRaisesRegex(gate.GatePrimitiveError, "raw-byte ceiling"):
                gate._read_bounded(
                    path,
                    max_raw_bytes=len(payload) - 1,
                    label="boundary input",
                )

            plan_path = root / "plan.json"
            manifest_path = root / "manifest.json"
            plan_path.write_bytes(self.plan_raw)
            manifest_path.write_bytes(manifest_raw)
            arguments = [
                "check-manifest",
                str(manifest_path),
                "--plan",
                str(plan_path),
                "--manifest-schema",
                str(self.manifest_schema),
            ]
            with mock.patch.object(gate, "MAX_MANIFEST_RAW_BYTES", len(manifest_raw) - 1):
                self.assert_cli_exit_2(arguments, "raw-byte ceiling")

    def test_manifest_self_content_toolchain_and_implementation_tampering_fail_closed(self) -> None:
        cases: list[tuple[str, Callable[[dict[str, Any]], None]]] = [
            (
                "self hash mismatch",
                lambda value: value["archive"].__setitem__("byte_count", 2048),
            ),
            (
                "inventory hash differs",
                lambda value: value["contents"].__setitem__("inventory_sha256", "a" * 64),
            ),
            (
                "run-plan binding differs",
                lambda value: value["run_plan"].__setitem__("plan_sha256", "b" * 64),
            ),
            (
                "implementation binding differs",
                lambda value: value["implementation"].__setitem__("builder_sha256", "c" * 64),
            ),
            (
                "authority boundary differs",
                lambda value: value["authority"].__setitem__("candidate_execution", "authorized"),
            ),
        ]
        for pattern, mutate in cases:
            with self.subTest(pattern=pattern):
                manifest = self.build_manifest()
                mutate(manifest)
                if pattern != "self hash mismatch":
                    self.reseal(manifest, "manifest_sha256")
                with self.assertRaisesRegex(gate.GatePrimitiveError, pattern):
                    gate.validate_cache_seed_manifest(
                        manifest,
                        plan=self.plan,
                        manifest_schema_path=self.manifest_schema,
                    )

    def test_injected_archive_observation_is_explicitly_unattested_and_exact(self) -> None:
        manifest = self.build_manifest()
        observation = self.observation(manifest)
        replacements: dict[str, Any] = {
            "archive_sha256": "d" * 64,
            "archive_byte_count": 1025,
            "content_inventory_sha256": "e" * 64,
            "file_count": 4,
            "unpacked_byte_count": 42,
            "observation_kind": "verified_archive",
        }
        for field, replacement in replacements.items():
            changed = copy.deepcopy(observation)
            changed[field] = replacement
            with self.subTest(field=field), self.assertRaisesRegex(
                gate.GatePrimitiveError,
                "observation differs",
            ):
                self.build_receipt(manifest, archive_observation=changed)

    def test_injected_resource_stats_reject_11_gib_and_accept_exact_16_gib(self) -> None:
        required = self.plan["resource_budget"]["minimum_free_disk_before_staging_bytes"]
        self.assertEqual(required, 16 * GIB)
        resource = gate.evaluate_injected_resource_stats(
            self.plan,
            self.filesystem_stats(required),
        )
        self.assertEqual(resource["free_bytes"], required)
        self.assertEqual(resource["headroom_bytes"], 0)
        self.assertEqual(resource["observation_kind"], gate.FILESYSTEM_OBSERVATION_KIND)

        for free_bytes in (11 * GIB, required - 1):
            with self.subTest(free_bytes=free_bytes), self.assertRaisesRegex(
                gate.GatePrimitiveError,
                "below the frozen 16 GiB",
            ):
                gate.evaluate_injected_resource_stats(
                    self.plan,
                    self.filesystem_stats(free_bytes),
                )
        for stats in (
            {"available_blocks": True, "fragment_size_bytes": 1},
            {"available_blocks": required, "fragment_size_bytes": 0},
            {"available_blocks": required},
        ):
            with self.subTest(stats=stats), self.assertRaises(gate.GatePrimitiveError):
                gate.evaluate_injected_resource_stats(self.plan, stats)

    def test_resource_profile_maxima_overflow_and_cli_round_trip(self) -> None:
        maximum = gate.MAX_RESOURCE_DERIVED_BYTES
        self.assertEqual(maximum, 10**gate.MAX_JSON_INTEGER_DIGITS - 1)
        self.assertEqual(len(str(maximum)), gate.MAX_JSON_INTEGER_DIGITS)
        self.assertEqual(gate.MAX_RESOURCE_AVAILABLE_BLOCKS, maximum)
        self.assertEqual(gate.MAX_RESOURCE_FRAGMENT_SIZE_BYTES, maximum)

        resource = gate.evaluate_injected_resource_stats(
            self.plan,
            {"available_blocks": maximum, "fragment_size_bytes": 1},
        )
        required = self.plan["resource_budget"]["minimum_free_disk_before_staging_bytes"]
        self.assertEqual(resource["free_bytes"], maximum)
        self.assertEqual(resource["headroom_bytes"], maximum - required)
        gate._validate_json_profile(resource)

        for stats in (
            {"available_blocks": maximum + 1, "fragment_size_bytes": 1},
            {"available_blocks": 1, "fragment_size_bytes": maximum + 1},
        ):
            with self.subTest(stats=stats), self.assertRaisesRegex(
                gate.GatePrimitiveError,
                "integer exceeds the digit ceiling",
            ):
                gate.evaluate_injected_resource_stats(self.plan, stats)

        for stats in (
            {"available_blocks": maximum, "fragment_size_bytes": maximum},
            {"available_blocks": maximum // 2 + 1, "fragment_size_bytes": 2},
        ):
            with self.subTest(stats=stats), self.assertRaisesRegex(
                gate.GatePrimitiveError,
                "multiplication exceeds the JSON integer ceiling",
            ):
                gate.evaluate_injected_resource_stats(self.plan, stats)

        schema = json.loads(self.receipt_schema.read_text(encoding="utf-8"))
        resource_schema = schema["properties"]["resource"]["properties"]
        for field in (
            "available_blocks",
            "fragment_size_bytes",
            "free_bytes",
            "headroom_bytes",
        ):
            with self.subTest(schema_field=field):
                self.assertEqual(resource_schema[field]["maximum"], maximum)

        manifest = self.build_manifest()
        receipt = self.build_receipt(manifest, free_bytes=maximum)
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            plan_path = root / "plan.json"
            manifest_path = root / "manifest.json"
            receipt_path = root / "receipt.json"
            plan_path.write_bytes(self.plan_raw)
            manifest_path.write_bytes(gate._render(manifest))
            receipt_path.write_bytes(gate._render(receipt))
            parsed_receipt, parsed_raw = gate._load_json(
                receipt_path,
                max_raw_bytes=gate.MAX_RECEIPT_RAW_BYTES,
                label="round-trip receipt",
            )
            self.assertEqual(parsed_receipt, receipt)
            self.assertEqual(parsed_raw, gate._render(receipt))
            self.assertEqual(
                gate.main(
                    [
                        "check-receipt",
                        str(receipt_path),
                        "--plan",
                        str(plan_path),
                        "--manifest",
                        str(manifest_path),
                        "--manifest-schema",
                        str(self.manifest_schema),
                        "--receipt-schema",
                        str(self.receipt_schema),
                    ]
                ),
                0,
            )

    def test_api_artifacts_share_parser_integer_depth_and_no_float_profile(self) -> None:
        maximum = gate.MAX_JSON_INTEGER_ABSOLUTE
        gate._canonical_json_bytes({"value": maximum})
        gate._canonical_json_bytes({"value": -maximum})
        with self.assertRaisesRegex(gate.GatePrimitiveError, "integer exceeds the digit ceiling"):
            gate._canonical_json_bytes({"value": maximum + 1})
        with self.assertRaisesRegex(gate.GatePrimitiveError, "floating-point"):
            gate._canonical_json_bytes({"value": (1.0,)})
        with self.assertRaisesRegex(gate.GatePrimitiveError, "string object keys"):
            gate._canonical_json_bytes({1: "value"})
        with self.assertRaisesRegex(gate.GatePrimitiveError, "not canonical JSON"):
            gate._canonical_json_bytes({"value": "\ud800"})
        with self.assertRaisesRegex(gate.GatePrimitiveError, "not renderable canonical JSON"):
            gate._render({"value": "\ud800"})

        nested: Any = 0
        for _ in range(gate.MAX_JSON_DEPTH):
            nested = [nested]

        manifest = self.build_manifest()
        manifest_cases: list[tuple[str, Callable[[dict[str, Any]], None]]] = [
            (
                "floating-point",
                lambda value: value["archive"].__setitem__("byte_count", 1.0),
            ),
            (
                "integer exceeds the digit ceiling",
                lambda value: value["archive"].__setitem__("byte_count", maximum + 1),
            ),
            (
                "nesting-depth ceiling",
                lambda value: value.__setitem__("implementation", copy.deepcopy(nested)),
            ),
        ]
        for pattern, mutate in manifest_cases:
            changed = copy.deepcopy(manifest)
            mutate(changed)
            with (
                self.subTest(artifact="manifest", pattern=pattern),
                mock.patch.object(
                    gate,
                    "_self_hash",
                    side_effect=AssertionError("self hash must follow profile validation"),
                ),
                mock.patch.object(
                    gate,
                    "_validate_against_schema",
                    side_effect=AssertionError("schema must follow profile validation"),
                ),
                self.assertRaisesRegex(gate.GatePrimitiveError, pattern),
            ):
                gate.validate_cache_seed_manifest(
                    changed,
                    plan=self.plan,
                    manifest_schema_path=self.manifest_schema,
                )

        receipt = self.build_receipt(manifest)
        receipt_kwargs = {
            "plan": self.plan,
            "plan_raw": self.plan_raw,
            "manifest": manifest,
            "manifest_raw": gate._render(manifest),
            "manifest_schema_path": self.manifest_schema,
            "receipt_schema_path": self.receipt_schema,
        }
        receipt_cases: list[tuple[str, Callable[[dict[str, Any]], None]]] = [
            (
                "floating-point",
                lambda value: value["resource"].__setitem__("available_blocks", 1.0),
            ),
            (
                "integer exceeds the digit ceiling",
                lambda value: value["resource"].__setitem__("available_blocks", maximum + 1),
            ),
            (
                "nesting-depth ceiling",
                lambda value: value.__setitem__("inputs", copy.deepcopy(nested)),
            ),
        ]
        for pattern, mutate in receipt_cases:
            changed = copy.deepcopy(receipt)
            mutate(changed)
            with (
                self.subTest(artifact="receipt", pattern=pattern),
                mock.patch.object(
                    gate,
                    "_self_hash",
                    side_effect=AssertionError("self hash must follow profile validation"),
                ),
                mock.patch.object(
                    gate,
                    "_validate_against_schema",
                    side_effect=AssertionError("schema must follow profile validation"),
                ),
                self.assertRaisesRegex(gate.GatePrimitiveError, pattern),
            ):
                gate.validate_preflight_receipt(changed, **receipt_kwargs)

    def test_json_tree_profile_rejects_cycles_alias_dags_and_bounds_nodes(self) -> None:
        self.assertEqual(gate.MAX_MANIFEST_ENTRY_JSON_NODES, 5)
        self.assertEqual(
            gate.MAX_JSON_VISITED_NODES,
            gate.MAX_MANIFEST_FILE_COUNT * gate.MAX_MANIFEST_ENTRY_JSON_NODES
            + gate.MAX_JSON_FIXED_ENVELOPE_NODES,
        )

        boundary_tree = {"items": [1, 2]}
        with mock.patch.object(gate, "MAX_JSON_VISITED_NODES", 4):
            self.assertEqual(gate._validate_json_profile(boundary_tree), 4)
        with (
            mock.patch.object(gate, "MAX_JSON_VISITED_NODES", 3),
            self.assertRaisesRegex(gate.GatePrimitiveError, "visited-node ceiling"),
        ):
            gate._validate_json_profile(boundary_tree)

        cycle: dict[str, Any] = {}
        cycle["self"] = cycle
        with self.assertRaisesRegex(
            gate.GatePrimitiveError,
            "cycle or repeated container alias",
        ):
            gate._validate_json_profile(cycle)

        shared_list: list[Any] = ["leaf"]
        shared_tuple = ("leaf",)
        for graph in ([shared_list, shared_list], [shared_tuple, shared_tuple]):
            with self.subTest(container=type(graph[0]).__name__), self.assertRaisesRegex(
                gate.GatePrimitiveError,
                "cycle or repeated container alias",
            ):
                gate._validate_json_profile(graph)

        alias_dag: Any = {"leaf": 1}
        for _ in range(40):
            alias_dag = [alias_dag, alias_dag]
        with (
            mock.patch.object(gate, "MAX_JSON_VISITED_NODES", 100),
            self.assertRaisesRegex(
                gate.GatePrimitiveError,
                "cycle or repeated container alias",
            ),
        ):
            gate._validate_json_profile(alias_dag)

        self.assertLessEqual(
            gate._validate_json_profile(self.plan),
            gate.MAX_JSON_FIXED_ENVELOPE_NODES,
        )
        cyclic_plan = copy.deepcopy(self.plan)
        cyclic_plan["authority"]["cycle"] = cyclic_plan
        with (
            mock.patch.object(
                run_plan,
                "validate_plan",
                side_effect=AssertionError("plan validation must follow graph validation"),
            ),
            self.assertRaisesRegex(
                gate.GatePrimitiveError,
                "cycle or repeated container alias",
            ),
        ):
            gate._pending_authority(cyclic_plan)
        for schema_path in (self.manifest_schema, self.receipt_schema):
            schema = json.loads(schema_path.read_text(encoding="utf-8"))
            with self.subTest(schema=schema_path.name):
                self.assertLessEqual(
                    gate._validate_json_profile(schema),
                    gate.MAX_JSON_FIXED_ENVELOPE_NODES,
                )

    def test_manifest_and_receipt_graph_failures_precede_hash_render_and_schema(self) -> None:
        manifest = self.build_manifest()
        manifest_nodes = gate._validate_json_profile(manifest)
        manifest_envelope_nodes = (
            manifest_nodes
            - len(manifest["contents"]["entries"])
            * gate.MAX_MANIFEST_ENTRY_JSON_NODES
        )
        self.assertLessEqual(
            manifest_envelope_nodes,
            gate.MAX_JSON_FIXED_ENVELOPE_NODES,
        )

        manifest_alias = copy.deepcopy(manifest)
        manifest_alias["implementation"]["verifier_sources"]["alias"] = manifest_alias[
            "authority"
        ]
        manifest_cycle = copy.deepcopy(manifest)
        manifest_cycle["implementation"]["cycle"] = manifest_cycle["implementation"]
        for label, changed in (
            ("alias", manifest_alias),
            ("cycle", manifest_cycle),
        ):
            with (
                self.subTest(artifact="manifest", failure=label),
                mock.patch.object(
                    gate,
                    "_self_hash",
                    side_effect=AssertionError("self hash must follow graph validation"),
                ),
                mock.patch.object(
                    gate,
                    "_render",
                    side_effect=AssertionError("render must follow graph validation"),
                ),
                mock.patch.object(
                    gate,
                    "_validate_against_schema",
                    side_effect=AssertionError("schema must follow graph validation"),
                ),
                self.assertRaisesRegex(
                    gate.GatePrimitiveError,
                    "cycle or repeated container alias",
                ),
            ):
                gate.validate_cache_seed_manifest(
                    changed,
                    plan=self.plan,
                    manifest_schema_path=self.manifest_schema,
                )

        with (
            mock.patch.object(gate, "MAX_JSON_VISITED_NODES", manifest_nodes - 1),
            mock.patch.object(
                gate,
                "_self_hash",
                side_effect=AssertionError("self hash must follow node validation"),
            ),
            mock.patch.object(
                gate,
                "_render",
                side_effect=AssertionError("render must follow node validation"),
            ),
            mock.patch.object(
                gate,
                "_validate_against_schema",
                side_effect=AssertionError("schema must follow node validation"),
            ),
            self.assertRaisesRegex(gate.GatePrimitiveError, "visited-node ceiling"),
        ):
            gate.validate_cache_seed_manifest(
                manifest,
                plan=self.plan,
                manifest_schema_path=self.manifest_schema,
            )

        receipt = self.build_receipt(manifest)
        receipt_nodes = gate._validate_json_profile(receipt)
        self.assertLessEqual(receipt_nodes, gate.MAX_JSON_FIXED_ENVELOPE_NODES)
        receipt_kwargs = {
            "plan": self.plan,
            "plan_raw": self.plan_raw,
            "manifest": manifest,
            "manifest_raw": gate._render(manifest),
            "manifest_schema_path": self.manifest_schema,
            "receipt_schema_path": self.receipt_schema,
        }
        receipt_alias = copy.deepcopy(receipt)
        receipt_alias["inputs"]["alias"] = receipt_alias["resource"]
        receipt_cycle = copy.deepcopy(receipt)
        receipt_cycle["inputs"]["cycle"] = receipt_cycle["inputs"]
        for label, changed in (
            ("alias", receipt_alias),
            ("cycle", receipt_cycle),
        ):
            with (
                self.subTest(artifact="receipt", failure=label),
                mock.patch.object(
                    gate,
                    "_self_hash",
                    side_effect=AssertionError("self hash must follow graph validation"),
                ),
                mock.patch.object(
                    gate,
                    "_render",
                    side_effect=AssertionError("render must follow graph validation"),
                ),
                mock.patch.object(
                    gate,
                    "_validate_against_schema",
                    side_effect=AssertionError("schema must follow graph validation"),
                ),
                self.assertRaisesRegex(
                    gate.GatePrimitiveError,
                    "cycle or repeated container alias",
                ),
            ):
                gate.validate_preflight_receipt(changed, **receipt_kwargs)

        with (
            mock.patch.object(gate, "MAX_JSON_VISITED_NODES", receipt_nodes - 1),
            mock.patch.object(
                gate,
                "_self_hash",
                side_effect=AssertionError("self hash must follow node validation"),
            ),
            mock.patch.object(
                gate,
                "_render",
                side_effect=AssertionError("render must follow node validation"),
            ),
            mock.patch.object(
                gate,
                "_validate_against_schema",
                side_effect=AssertionError("schema must follow node validation"),
            ),
            self.assertRaisesRegex(gate.GatePrimitiveError, "visited-node ceiling"),
        ):
            gate.validate_preflight_receipt(receipt, **receipt_kwargs)

    def test_receipt_validator_rejects_noncanonical_raw_inputs_and_authority_reseal(self) -> None:
        manifest = self.build_manifest()
        manifest_raw = gate._render(manifest)
        receipt = self.build_receipt(manifest)
        kwargs = {
            "plan": self.plan,
            "plan_raw": self.plan_raw,
            "manifest": manifest,
            "manifest_raw": manifest_raw,
            "manifest_schema_path": self.manifest_schema,
            "receipt_schema_path": self.receipt_schema,
        }
        gate.validate_preflight_receipt(receipt, **kwargs)
        with self.assertRaisesRegex(gate.GatePrimitiveError, "run-plan raw hash differs"):
            gate.validate_preflight_receipt(receipt, **{**kwargs, "plan_raw": self.plan_raw + b"\n"})
        with self.assertRaisesRegex(gate.GatePrimitiveError, "manifest bytes are not canonical"):
            gate.validate_preflight_receipt(receipt, **{**kwargs, "manifest_raw": manifest_raw + b"\n"})

        changed = copy.deepcopy(receipt)
        changed["execution_status"] = "authorized"
        self.reseal(changed, "receipt_sha256")
        with self.assertRaisesRegex(gate.GatePrimitiveError, "overclaims execution authority"):
            gate.validate_preflight_receipt(changed, **kwargs)

    def test_schema_bytes_are_pinned_and_schema_validation_is_executed(self) -> None:
        self.assertEqual(
            gate._sha256(self.manifest_schema.read_bytes()),
            gate.CHECKED_MANIFEST_SCHEMA_FILE_SHA256,
        )
        self.assertEqual(
            gate._sha256(self.receipt_schema.read_bytes()),
            gate.CHECKED_RECEIPT_SCHEMA_FILE_SHA256,
        )
        with mock.patch.object(
            gate,
            "_validate_against_schema",
            wraps=gate._validate_against_schema,
        ) as validate_schema:
            manifest = self.build_manifest()
            self.build_receipt(manifest)
        self.assertIn(
            "cache manifest",
            [call.args[2] for call in validate_schema.call_args_list],
        )
        self.assertIn(
            "preflight receipt",
            [call.args[2] for call in validate_schema.call_args_list],
        )

        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            manifest_schema = json.loads(self.manifest_schema.read_text(encoding="utf-8"))
            manifest_schema["properties"]["status"]["const"] = "impossible"
            changed_manifest_schema = root / self.manifest_schema.name
            changed_manifest_schema.write_text(json.dumps(manifest_schema), encoding="utf-8")
            with self.assertRaisesRegex(gate.GatePrimitiveError, "manifest schema raw hash differs"):
                self.build_manifest(manifest_schema=changed_manifest_schema)

            manifest = self.build_manifest()
            receipt_schema = json.loads(self.receipt_schema.read_text(encoding="utf-8"))
            receipt_schema["properties"]["status"]["const"] = "impossible"
            changed_receipt_schema = root / self.receipt_schema.name
            changed_receipt_schema.write_text(json.dumps(receipt_schema), encoding="utf-8")
            with self.assertRaisesRegex(gate.GatePrimitiveError, "receipt schema raw hash differs"):
                self.build_receipt(manifest, receipt_schema=changed_receipt_schema)

    def test_manifest_and_receipt_cli_check_canonical_bytes_with_no_execution_surface(self) -> None:
        manifest = self.build_manifest()
        receipt = self.build_receipt(manifest)
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            plan_path = root / "plan.json"
            manifest_path = root / "manifest.json"
            receipt_path = root / "receipt.json"
            plan_path.write_bytes(self.plan_raw)
            manifest_path.write_bytes(gate._render(manifest))
            manifest_arguments = [
                "check-manifest",
                str(manifest_path),
                "--plan",
                str(plan_path),
                "--manifest-schema",
                str(self.manifest_schema),
            ]
            self.assertEqual(gate.main(manifest_arguments), 0)
            manifest_path.write_bytes(gate._render(manifest) + b"\n")
            stderr = io.StringIO()
            with (
                contextlib.redirect_stderr(stderr),
                self.assertRaises(SystemExit) as raised,
            ):
                gate.main(manifest_arguments)
            self.assertEqual(raised.exception.code, 2)
            self.assertIn("cache manifest bytes are not canonical", stderr.getvalue())

            manifest_path.write_bytes(gate._render(manifest))
            receipt_path.write_bytes(gate._render(receipt))
            receipt_arguments = [
                "check-receipt",
                str(receipt_path),
                "--plan",
                str(plan_path),
                "--manifest",
                str(manifest_path),
                "--manifest-schema",
                str(self.manifest_schema),
                "--receipt-schema",
                str(self.receipt_schema),
            ]
            self.assertEqual(gate.main(receipt_arguments), 0)
            receipt_path.write_bytes(gate._render(receipt) + b"\n")
            stderr = io.StringIO()
            with (
                contextlib.redirect_stderr(stderr),
                self.assertRaises(SystemExit) as raised,
            ):
                gate.main(receipt_arguments)
            self.assertEqual(raised.exception.code, 2)
            self.assertIn("preflight receipt bytes are not canonical", stderr.getvalue())

            tampered_receipt = copy.deepcopy(receipt)
            tampered_receipt["execution_status"] = "authorized"
            self.reseal(tampered_receipt, "receipt_sha256")
            receipt_path.write_bytes(gate._render(tampered_receipt))
            stderr = io.StringIO()
            with (
                contextlib.redirect_stderr(stderr),
                self.assertRaises(SystemExit) as raised,
            ):
                gate.main(receipt_arguments)
            self.assertEqual(raised.exception.code, 2)
            self.assertIn("overclaims execution authority", stderr.getvalue())

        source = pathlib.Path(gate.__file__).read_text(encoding="utf-8")
        for prohibited_import in (
            "import subprocess",
            "import socket",
            "import urllib",
            "import requests",
            "os.statvfs",
            "os.system",
        ):
            with self.subTest(prohibited_import=prohibited_import):
                self.assertNotIn(prohibited_import, source)

    def test_both_cli_lanes_reject_a_self_resealed_plan_drift(self) -> None:
        manifest = self.build_manifest()
        receipt = self.build_receipt(manifest)
        changed_plan = copy.deepcopy(self.plan)
        changed_plan["implementation"]["builder_sha256"] = "a" * 64
        changed_plan["plan_sha256"] = run_plan._self_hash(changed_plan)
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            plan_path = root / "plan.json"
            manifest_path = root / "manifest.json"
            receipt_path = root / "receipt.json"
            plan_path.write_bytes(run_plan._render(changed_plan))
            manifest_path.write_bytes(gate._render(manifest))
            receipt_path.write_bytes(gate._render(receipt))
            common = [
                "--plan",
                str(plan_path),
                "--manifest-schema",
                str(self.manifest_schema),
            ]
            self.assert_cli_exit_2(
                ["check-manifest", str(manifest_path), *common],
                "run-plan raw hash differs",
            )
            self.assert_cli_exit_2(
                [
                    "check-receipt",
                    str(receipt_path),
                    "--manifest",
                    str(manifest_path),
                    *common,
                    "--receipt-schema",
                    str(self.receipt_schema),
                ],
                "run-plan raw hash differs",
            )

    def test_bounded_parser_rejects_digit_depth_and_same_size_read_races(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            plan_path = root / "plan.json"
            manifest_path = root / "manifest.json"
            plan_path.write_bytes(self.plan_raw)
            arguments = [
                "check-manifest",
                str(manifest_path),
                "--plan",
                str(plan_path),
                "--manifest-schema",
                str(self.manifest_schema),
            ]

            manifest_path.write_bytes(b'{"value":' + b"9" * 5000 + b"}")
            self.assert_cli_exit_2(arguments, "integer exceeds the digit ceiling")

            manifest_path.write_bytes(
                b'{"value":' + b"[" * 80 + b"0" + b"]" * 80 + b"}"
            )
            self.assert_cli_exit_2(arguments, "nesting-depth ceiling")

            parseable = root / "parseable.json"
            parseable.write_bytes(b"{}")
            for exception in (ValueError("synthetic"), RecursionError("synthetic")):
                with (
                    self.subTest(exception=type(exception).__name__),
                    mock.patch.object(gate.json, "loads", side_effect=exception),
                    self.assertRaisesRegex(gate.GatePrimitiveError, "cannot parse"),
                ):
                    gate._load_json(parseable)

            raced = root / "raced.json"
            before = b'{"value":1}'
            after = b'{"value":2}'
            self.assertEqual(len(before), len(after))
            raced.write_bytes(before)
            real_read = gate.os.read
            changed = False

            def mutate_after_read(descriptor: int, byte_count: int) -> bytes:
                nonlocal changed
                chunk = real_read(descriptor, byte_count)
                if chunk and not changed:
                    changed = True
                    raced.write_bytes(after)
                return chunk

            with (
                mock.patch.object(gate.os, "read", side_effect=mutate_after_read),
                self.assertRaisesRegex(gate.GatePrimitiveError, "changed while being read"),
            ):
                gate._read_bounded(
                    raced,
                    max_raw_bytes=len(before),
                    label="raced input",
                )

            symlink = root / "linked.json"
            symlink.symlink_to(parseable)
            with self.assertRaisesRegex(gate.GatePrimitiveError, "cannot read"):
                gate._read_bounded(
                    symlink,
                    max_raw_bytes=1024,
                    label="symlink input",
                )

            fifo = root / "stream.json"
            gate.os.mkfifo(fifo)
            with self.assertRaisesRegex(gate.GatePrimitiveError, "not a regular file"):
                gate._read_bounded(
                    fifo,
                    max_raw_bytes=1024,
                    label="FIFO input",
                )

            for flag_name in ("O_NOFOLLOW", "O_NONBLOCK"):
                with (
                    self.subTest(flag_name=flag_name),
                    mock.patch.object(gate.os, flag_name, 0),
                    self.assertRaisesRegex(
                        gate.GatePrimitiveError,
                        f"{flag_name} is unavailable",
                    ),
                ):
                    gate._read_bounded(
                        parseable,
                        max_raw_bytes=1024,
                        label="no-flag input",
                    )

    def test_duplicate_keys_floats_and_plan_authority_tampering_fail_closed(self) -> None:
        with self.assertRaisesRegex(gate.GatePrimitiveError, "floating-point"):
            gate._canonical_json_bytes({"value": 1.0})
        with tempfile.TemporaryDirectory() as temporary:
            duplicate = pathlib.Path(temporary) / "duplicate.json"
            duplicate.write_text('{"profile":"a","profile":"b"}', encoding="utf-8")
            with self.assertRaisesRegex(gate.GatePrimitiveError, "duplicate object key"):
                gate._load_json(duplicate)

        changed_plan = copy.deepcopy(self.plan)
        changed_plan["status"] = "authorized"
        changed_plan["plan_sha256"] = run_plan._self_hash(changed_plan)
        with self.assertRaisesRegex(gate.GatePrimitiveError, "run plan is invalid"):
            gate.build_cache_seed_manifest(
                plan=changed_plan,
                manifest_schema_path=self.manifest_schema,
                archive_file="offline-go-cache-seed-v1.tar",
                archive_sha256=self.archive_sha256,
                archive_byte_count=1024,
                entries=copy.deepcopy(self.entries),
            )

    def test_public_values_contain_no_host_path_or_secret(self) -> None:
        manifest = self.build_manifest()
        receipt = self.build_receipt(manifest)
        rendered = (gate._render(manifest) + gate._render(receipt)).decode("utf-8")
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
