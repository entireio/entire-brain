from __future__ import annotations

import contextlib
import copy
import io
import json
import pathlib
import tempfile
import unittest
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

    def test_manifest_rejects_path_collisions_traversal_order_and_missing_repository(self) -> None:
        duplicate = copy.deepcopy(self.entries)
        duplicate[1]["path"] = duplicate[0]["path"]
        with self.assertRaisesRegex(gate.GatePrimitiveError, "paths collide globally"):
            self.build_manifest(entries=duplicate)

        for path in (
            "/gocache/host",
            "gocache/../escape",
            "gocache//double",
            "other/cache",
            "gocache\\windows",
            "gocache/e\u0301/non-nfc",
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
        with self.assertRaisesRegex(gate.GatePrimitiveError, "every repository"):
            self.build_manifest(entries=copy.deepcopy(self.entries[:2]))

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

        maximum = self.plan["resource_budget"]["max_cache_seed_bytes"]
        with self.assertRaisesRegex(gate.GatePrimitiveError, "archive exceeds"):
            self.build_manifest(archive_byte_count=maximum + 1)
        entries = copy.deepcopy(self.entries)
        entries[0]["byte_count"] = maximum + 1
        with self.assertRaisesRegex(gate.GatePrimitiveError, "unpacked cache seed exceeds"):
            self.build_manifest(entries=entries)

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
        with self.assertRaisesRegex(gate.GatePrimitiveError, "run-plan bytes are not canonical"):
            gate.validate_preflight_receipt(receipt, **{**kwargs, "plan_raw": self.plan_raw + b"\n"})
        with self.assertRaisesRegex(gate.GatePrimitiveError, "manifest bytes are not canonical"):
            gate.validate_preflight_receipt(receipt, **{**kwargs, "manifest_raw": manifest_raw + b"\n"})

        changed = copy.deepcopy(receipt)
        changed["execution_status"] = "authorized"
        self.reseal(changed, "receipt_sha256")
        with self.assertRaisesRegex(gate.GatePrimitiveError, "overclaims execution authority"):
            gate.validate_preflight_receipt(changed, **kwargs)

    def test_schema_validation_is_executed_not_only_hash_bound(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            manifest_schema = json.loads(self.manifest_schema.read_text(encoding="utf-8"))
            manifest_schema["properties"]["status"]["const"] = "impossible"
            changed_manifest_schema = root / self.manifest_schema.name
            changed_manifest_schema.write_text(json.dumps(manifest_schema), encoding="utf-8")
            with self.assertRaisesRegex(gate.GatePrimitiveError, "schema validation failed"):
                self.build_manifest(manifest_schema=changed_manifest_schema)

            manifest = self.build_manifest()
            receipt_schema = json.loads(self.receipt_schema.read_text(encoding="utf-8"))
            receipt_schema["properties"]["status"]["const"] = "impossible"
            changed_receipt_schema = root / self.receipt_schema.name
            changed_receipt_schema.write_text(json.dumps(receipt_schema), encoding="utf-8")
            with self.assertRaisesRegex(gate.GatePrimitiveError, "schema validation failed"):
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
