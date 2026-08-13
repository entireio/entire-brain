#!/usr/bin/env python3
"""Synthetic, offline tests for the cache-archive raw-identity verifier."""

from __future__ import annotations

import argparse
import ast
import contextlib
import copy
import importlib.util
import inspect
import io
import marshal
import os
import pathlib
import struct
import subprocess
import sys
import tempfile
import unittest
from collections.abc import Callable
from typing import Any
from unittest import mock

import negative_control_cache_archive_v1 as cache
import task_negative_control_gate_v1 as gate
import task_negative_control_plan_v2 as run_plan


HERE = pathlib.Path(__file__).parent


def _reseal_material(value: dict[str, Any]) -> None:
    value["material_declaration_sha256"] = None
    value["material_declaration_sha256"] = cache._field_self_hash(
        value, "material_declaration_sha256"
    )


def _reseal_contract(value: dict[str, Any]) -> None:
    value["verifier_contract_sha256"] = None
    value["verifier_contract_sha256"] = cache._field_self_hash(
        value, "verifier_contract_sha256"
    )


def _checked_plan() -> dict[str, Any]:
    value, _raw = run_plan._load_json(cache.RUN_PLAN_PATH)
    run_plan.validate_plan(value)
    return value


def _synthetic_manifest(archive_raw: bytes) -> dict[str, Any]:
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
            "path": "gomodcache/example.org/graph@v1.0.0/mod.zip",
            "repository_key": "entire-graph",
            "sha256": gate._sha256(b"synthetic graph cache entry"),
        },
    ]
    return gate.build_cache_seed_manifest(
        plan=_checked_plan(),
        manifest_schema_path=cache.MANIFEST_SCHEMA_PATH,
        archive_file="synthetic-offline-go-cache.opaque",
        archive_sha256=cache._sha256(archive_raw),
        archive_byte_count=len(archive_raw),
        entries=entries,
    )


def _approved_material(
    pending: dict[str, Any],
    *,
    archive_raw: bytes,
    manifest: dict[str, Any],
    manifest_raw: bytes,
) -> dict[str, Any]:
    value = copy.deepcopy(pending)
    contents = manifest["contents"]
    value["status"] = cache.APPROVED_MATERIAL_STATUS
    value["material"].update(
        {
            "archive_byte_count": len(archive_raw),
            "archive_file": manifest["archive"]["artifact_file"],
            "archive_sha256": cache._sha256(archive_raw),
            "content_inventory_sha256": contents["inventory_sha256"],
            "file_count": contents["file_count"],
            "manifest_file": "synthetic-offline-go-cache-manifest-v1.json",
            "manifest_file_sha256": cache._sha256(manifest_raw),
            "manifest_sha256": manifest["manifest_sha256"],
            "repository_file_counts": copy.deepcopy(
                contents["repository_file_counts"]
            ),
            "unpacked_byte_count": contents["unpacked_byte_count"],
        }
    )
    value["review"].update(
        {
            "disposition": "approved_source_review_identity_only",
            "source_review_effect": "raw_identity_only_not_e0_runtime_authority",
        }
    )
    _reseal_material(value)
    return value


def _approved_contract(
    pending: dict[str, Any],
    *,
    material: dict[str, Any],
) -> dict[str, Any]:
    value = copy.deepcopy(pending)
    declared = material["material"]
    value["status"] = cache.APPROVED_VERIFIER_STATUS
    value["material_binding"].update(
        {
            "archive_byte_count": declared["archive_byte_count"],
            "archive_file": declared["archive_file"],
            "archive_sha256": declared["archive_sha256"],
            "artifact_sha256": cache._sha256(cache._render(material)),
            "content_inventory_sha256": declared["content_inventory_sha256"],
            "file_count": declared["file_count"],
            "manifest_file": declared["manifest_file"],
            "manifest_file_sha256": declared["manifest_file_sha256"],
            "manifest_sha256": declared["manifest_sha256"],
            "material_declaration_sha256": material[
                "material_declaration_sha256"
            ],
            "repository_file_counts": copy.deepcopy(
                declared["repository_file_counts"]
            ),
            "status": material["status"],
            "unpacked_byte_count": declared["unpacked_byte_count"],
        }
    )
    _reseal_contract(value)
    return value


def _mutate_manifest_without_resealing(value: dict[str, Any]) -> None:
    value["archive"]["artifact_file"] = "other-cache.opaque"


def _mutate_manifest_inventory(value: dict[str, Any]) -> None:
    value["contents"]["entries"][0]["sha256"] = "a" * 64
    value["manifest_sha256"] = gate._self_hash(value, "manifest_sha256")


def _mutate_manifest_count(value: dict[str, Any]) -> None:
    value["contents"]["file_count"] = 4
    value["manifest_sha256"] = gate._self_hash(value, "manifest_sha256")


def _mutate_manifest_plan_binding(value: dict[str, Any]) -> None:
    value["run_plan"]["plan_sha256"] = "a" * 64
    value["manifest_sha256"] = gate._self_hash(value, "manifest_sha256")


class CheckedPendingContractTest(unittest.TestCase):
    def test_checked_pending_material_is_canonical_and_exactly_validated(self) -> None:
        dependencies = cache._load_fixed_dependencies()
        material, raw = cache._read_json(
            cache.MATERIAL_PATH,
            maximum=cache.MAX_FIXED_JSON_BYTES,
            label="checked cache archive material declaration",
            canonical=True,
        )
        self.assertEqual(raw, cache._render(material))
        self.assertEqual(
            cache.validate_material_declaration(
                material,
                schema=dependencies["material_schema"],
            ),
            dependencies["material"],
        )
        self.assertEqual(material["status"], cache.PENDING_MATERIAL_STATUS)
        self.assertEqual(
            material["material_declaration_sha256"],
            cache._field_self_hash(material, "material_declaration_sha256"),
        )
        self.assertTrue(
            all(
                value is None
                for key, value in material["material"].items()
                if key != "archive_interpretation"
            )
        )
        self.assertFalse(material["review"]["execution_authority"])
        self.assertFalse(material["review"]["content_safety_verified"])
        self.assertFalse(material["review"]["e0_runtime_binding"])

    def test_checked_contract_is_a_deterministic_exact_pending_rebuild(self) -> None:
        first = cache.build_verifier_contract()
        second = cache.build_verifier_contract()
        self.assertEqual(first, second)
        self.assertEqual(first["status"], cache.PENDING_VERIFIER_STATUS)
        self.assertEqual(first["authority"], cache.AUTHORITY)
        self.assertEqual(first["runtime_bindings"], cache.RUNTIME_BINDINGS)
        self.assertEqual(
            first["verifier_contract_sha256"],
            cache._field_self_hash(first, "verifier_contract_sha256"),
        )

        checked, checked_raw = cache._read_json(
            cache.VERIFIER_CONTRACT_PATH,
            maximum=cache.MAX_VERIFIER_CONTRACT_BYTES,
            label="checked cache archive verifier contract",
            canonical=True,
        )
        self.assertEqual(checked, first)
        self.assertEqual(checked_raw, cache._render(first))
        self.assertEqual(cache.check_verifier_contract(), first)

    def test_fixed_dependency_identity_tampering_fails_closed(self) -> None:
        constants = (
            "CHECKED_DRAFT202012_SHA256",
            "CHECKED_EXECUTION_ARTIFACT_SHA256",
            "CHECKED_EXECUTION_BUILDER_SHA256",
            "CHECKED_EXECUTION_SCHEMA_SHA256",
            "CHECKED_GATE_PRIMITIVE_SHA256",
            "CHECKED_MANIFEST_SCHEMA_SHA256",
            "CHECKED_MATERIAL_ARTIFACT_SHA256",
            "CHECKED_MATERIAL_SHA256",
            "CHECKED_MATERIAL_SCHEMA_SHA256",
            "CHECKED_REPORT_SCHEMA_SHA256",
            "CHECKED_RUN_PLAN_ARTIFACT_SHA256",
            "CHECKED_RUN_PLAN_BUILDER_SHA256",
            "CHECKED_RUN_PLAN_SCHEMA_SHA256",
            "CHECKED_TOOLCHAIN_BINDINGS_SHA256",
            "CHECKED_VERIFIER_CONTRACT_SCHEMA_SHA256",
        )
        for constant in constants:
            with (
                self.subTest(constant=constant),
                mock.patch.object(cache, constant, "a" * 64),
                self.assertRaisesRegex(
                    cache.CacheArchiveVerificationError,
                    "differs",
                ),
            ):
                cache._load_fixed_dependencies()


class PublicPendingSurfaceTest(unittest.TestCase):
    def test_cli_surface_is_exactly_build_check_and_verify(self) -> None:
        parser = cache._parser()
        subparsers = [
            action
            for action in parser._actions
            if isinstance(action, argparse._SubParsersAction)
        ]
        self.assertEqual(len(subparsers), 1)
        self.assertEqual(set(subparsers[0].choices), {"build", "check", "verify"})
        self.assertEqual(
            vars(parser.parse_args(["build", "--output", "out.json"])),
            {"command": "build", "output": pathlib.Path("out.json")},
        )
        self.assertEqual(
            vars(parser.parse_args(["check"])),
            {"command": "check"},
        )
        self.assertEqual(
            vars(parser.parse_args(["verify", "manifest.json", "archive.opaque"])),
            {
                "archive": pathlib.Path("archive.opaque"),
                "command": "verify",
                "manifest": pathlib.Path("manifest.json"),
            },
        )
        for argv in (
            [],
            ["pack"],
            ["extract"],
            ["stage"],
            ["authorize"],
            ["run"],
            ["verify"],
            ["verify", "manifest.json"],
            ["verify", "manifest.json", "archive", "extra"],
            ["verify", "manifest.json", "archive", "--material", "other.json"],
            ["verify", "manifest.json", "archive", "--contract", "other.json"],
            ["verify", "manifest.json", "archive", "--schema", "other.json"],
            ["check", "one.json"],
            ["check", "one.json", "two.json"],
        ):
            with self.subTest(argv=argv), contextlib.redirect_stderr(io.StringIO()):
                with self.assertRaises(SystemExit):
                    parser.parse_args(argv)

    def test_public_verify_has_no_dependency_or_contract_injection(self) -> None:
        self.assertEqual(
            tuple(inspect.signature(cache.verify_material_files).parameters),
            ("manifest_path", "archive_path"),
        )
        with self.assertRaises(TypeError):
            cache.verify_material_files(
                pathlib.Path("manifest.json"),
                pathlib.Path("archive.opaque"),
                dependencies={},  # pyright: ignore[reportCallIssue]
            )

    def test_pending_verify_fails_before_opening_caller_paths(self) -> None:
        manifest_path = pathlib.Path("must-not-open-manifest.json")
        archive_path = pathlib.Path("must-not-open-archive.opaque")
        original_open = cache._open_regular_descriptor
        attempted: list[pathlib.Path] = []

        def guarded_open(path: pathlib.Path, *, label: str) -> Any:
            if path in {manifest_path, archive_path}:
                attempted.append(path)
                raise AssertionError(f"pending verifier opened {label}")
            return original_open(path, label=label)

        with (
            mock.patch.object(
                cache,
                "_open_regular_descriptor",
                side_effect=guarded_open,
            ),
            self.assertRaisesRegex(
                cache.CacheArchiveVerificationError,
                "pending",
            ),
        ):
            cache.verify_material_files(manifest_path, archive_path)
        self.assertEqual(attempted, [])


class SyntheticApprovedFixtureTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory(
            prefix="negative-control-cache-archive-tests-"
        )
        self.root = pathlib.Path(self.temporary.name).resolve(strict=True)
        self.archive_raw = b"synthetic opaque cache archive bytes; never interpreted"
        self.archive_path = self.root / "synthetic-offline-go-cache.opaque"
        self.archive_path.write_bytes(self.archive_raw)
        self.manifest = _synthetic_manifest(self.archive_raw)
        self.manifest_raw = gate._render(self.manifest)
        self.manifest_path = (
            self.root / "synthetic-offline-go-cache-manifest-v1.json"
        )
        self.manifest_path.write_bytes(self.manifest_raw)

        self.pending_dependencies = cache._load_fixed_dependencies()
        self.pending_contract = cache.build_verifier_contract()
        self.material = _approved_material(
            self.pending_dependencies["material"],
            archive_raw=self.archive_raw,
            manifest=self.manifest,
            manifest_raw=self.manifest_raw,
        )
        self.dependencies, self.contract = self.bind_material(self.material)

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def bind_material(
        self, material: dict[str, Any]
    ) -> tuple[dict[str, Any], dict[str, Any]]:
        dependencies = copy.deepcopy(self.pending_dependencies)
        dependencies["material"] = copy.deepcopy(material)
        dependencies["material_raw"] = cache._render(material)
        contract = _approved_contract(
            self.pending_contract,
            material=material,
        )
        cache.validate_material_declaration(
            material,
            schema=dependencies["material_schema"],
        )
        cache.validate_verifier_contract(
            contract,
            schema=dependencies["verifier_schema"],
            dependencies=dependencies,
        )
        return dependencies, contract

    def rebind_manifest(
        self,
        manifest: dict[str, Any],
        manifest_raw: bytes,
        *,
        copy_content_fields: bool = True,
    ) -> tuple[dict[str, Any], dict[str, Any]]:
        material = copy.deepcopy(self.material)
        material["material"]["manifest_file_sha256"] = cache._sha256(
            manifest_raw
        )
        material["material"]["manifest_sha256"] = manifest["manifest_sha256"]
        if copy_content_fields:
            contents = manifest["contents"]
            material["material"].update(
                {
                    "content_inventory_sha256": contents["inventory_sha256"],
                    "file_count": contents["file_count"],
                    "repository_file_counts": copy.deepcopy(
                        contents["repository_file_counts"]
                    ),
                    "unpacked_byte_count": contents["unpacked_byte_count"],
                }
            )
        _reseal_material(material)
        return self.bind_material(material)

    def verify(
        self,
        *,
        dependencies: dict[str, Any] | None = None,
        contract: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        return cache._verify_with_dependencies(
            self.manifest_path,
            self.archive_path,
            dependencies=(self.dependencies if dependencies is None else dependencies),
            verifier_contract=(self.contract if contract is None else contract),
        )

    def test_approved_opaque_identity_happy_path_is_non_authorizing(self) -> None:
        first = self.verify()
        second = self.verify()
        self.assertEqual(first, second)
        self.assertEqual(first["status"], cache.REPORT_STATUS)
        self.assertEqual(
            first["identity_binding_status"],
            cache.IDENTITY_BINDING_STATUS,
        )
        self.assertEqual(first["authority"], cache.REPORT_AUTHORITY)
        self.assertEqual(first["safety"], cache.REPORT_SAFETY)
        self.assertFalse(first["authority"]["execution_authority"])
        self.assertFalse(first["authority"]["owner_approval"])
        self.assertFalse(first["authority"]["atomic_consumption"])
        self.assertFalse(first["safety"]["archive_content_parsed"])
        self.assertFalse(first["safety"]["content_safety_verified"])
        self.assertFalse(first["safety"]["extraction"])
        self.assertFalse(first["safety"]["staging"])
        self.assertFalse(first["safety"]["e0_runtime_binding"])
        self.assertEqual(
            first["archive"],
            {
                "artifact_file": self.archive_path.name,
                "byte_count": len(self.archive_raw),
                "sha256": cache._sha256(self.archive_raw),
                "verification": "exact_bounded_regular_file_raw_identity",
            },
        )
        self.assertEqual(
            first["report_sha256"],
            cache._field_self_hash(first, "report_sha256"),
        )
        cache._validate_schema(
            first,
            self.dependencies["report_schema"],
            schema_name=cache.REPORT_SCHEMA_PATH.name,
            label="synthetic cache archive verification report",
        )

    def test_archive_raw_hash_and_size_mismatches_fail_closed(self) -> None:
        for label, raw in (
            ("hash", b"x" * len(self.archive_raw)),
            ("size", self.archive_raw[:-1]),
        ):
            self.archive_path.write_bytes(raw)
            with self.subTest(label=label), self.assertRaises(
                cache.CacheArchiveVerificationError
            ):
                self.verify()
            self.archive_path.write_bytes(self.archive_raw)

    def test_archive_path_rejects_symlink_hardlink_fifo_and_race(self) -> None:
        target = self.root / "alternate-archive-source.opaque"
        target.write_bytes(self.archive_raw)

        self.archive_path.unlink()
        self.archive_path.symlink_to(target)
        with self.assertRaises(cache.CacheArchiveVerificationError):
            self.verify()
        self.archive_path.unlink()

        os.link(target, self.archive_path)
        with self.assertRaisesRegex(
            cache.CacheArchiveVerificationError,
            "link",
        ):
            self.verify()
        self.archive_path.unlink()
        target.unlink()

        os.mkfifo(self.archive_path)
        with self.assertRaisesRegex(
            cache.CacheArchiveVerificationError,
            "regular file",
        ):
            self.verify()
        self.archive_path.unlink()
        self.archive_path.write_bytes(self.archive_raw)

        original_read = cache.os.read
        archive_identity = self.archive_path.stat()
        replacement = b"x" * len(self.archive_raw)
        changed = False

        def mutate_archive_after_read(descriptor: int, byte_count: int) -> bytes:
            nonlocal changed
            chunk = original_read(descriptor, byte_count)
            metadata = os.fstat(descriptor)
            if (
                chunk
                and not changed
                and metadata.st_dev == archive_identity.st_dev
                and metadata.st_ino == archive_identity.st_ino
            ):
                changed = True
                self.archive_path.write_bytes(replacement)
            return chunk

        with (
            mock.patch.object(
                cache.os,
                "read",
                side_effect=mutate_archive_after_read,
            ),
            self.assertRaisesRegex(
                cache.CacheArchiveVerificationError,
                "changed while being read",
            ),
        ):
            self.verify()

    def test_noncanonical_manifest_bytes_fail_even_when_material_binds_them(self) -> None:
        noncanonical = cache._canonical_bytes(self.manifest)
        self.manifest_path.write_bytes(noncanonical)
        dependencies, contract = self.rebind_manifest(
            self.manifest,
            noncanonical,
        )
        with self.assertRaisesRegex(
            cache.CacheArchiveVerificationError,
            "canonical",
        ):
            self.verify(dependencies=dependencies, contract=contract)

    def test_manifest_self_inventory_count_and_plan_bindings_fail_closed(self) -> None:
        cases: list[
            tuple[
                str,
                Callable[[dict[str, Any]], None],
                bool,
            ]
        ] = [
            (
                "self-hash",
                _mutate_manifest_without_resealing,
                True,
            ),
            (
                "inventory",
                _mutate_manifest_inventory,
                True,
            ),
            (
                "count",
                _mutate_manifest_count,
                False,
            ),
            (
                "plan",
                _mutate_manifest_plan_binding,
                True,
            ),
        ]
        for label, mutate, copy_content_fields in cases:
            changed = copy.deepcopy(self.manifest)
            mutate(changed)
            raw = gate._render(changed)
            self.manifest_path.write_bytes(raw)
            dependencies, contract = self.rebind_manifest(
                changed,
                raw,
                copy_content_fields=copy_content_fields,
            )
            with self.subTest(label=label), self.assertRaises(
                cache.CacheArchiveVerificationError
            ):
                self.verify(dependencies=dependencies, contract=contract)
            self.manifest_path.write_bytes(self.manifest_raw)


class RawFileIdentityPrimitiveTest(unittest.TestCase):
    def test_stream_identity_rejects_size_symlink_fifo_and_in_place_race(self) -> None:
        with tempfile.TemporaryDirectory() as temporary_name:
            root = pathlib.Path(temporary_name).resolve(strict=True)
            regular = root / "archive.opaque"
            before = b"before-opaque-cache-bytes"
            after = b"after--opaque-cache-bytes"
            self.assertEqual(len(before), len(after))
            regular.write_bytes(before)

            self.assertEqual(
                cache._stream_identity(
                    regular, maximum=len(before), label="fixture archive"
                ),
                {"byte_count": len(before), "sha256": cache._sha256(before)},
            )
            with self.assertRaisesRegex(
                cache.CacheArchiveVerificationError, "outside the byte ceiling"
            ):
                cache._stream_identity(
                    regular, maximum=len(before) - 1, label="fixture archive"
                )

            symlink = root / "archive-link.opaque"
            symlink.symlink_to(regular)
            with self.assertRaisesRegex(
                cache.CacheArchiveVerificationError, "cannot open"
            ):
                cache._stream_identity(
                    symlink, maximum=len(before), label="fixture symlink"
                )

            hardlink = root / "archive-hardlink.opaque"
            os.link(regular, hardlink)
            with self.assertRaisesRegex(
                cache.CacheArchiveVerificationError, "link"
            ):
                cache._stream_identity(
                    hardlink, maximum=len(before), label="fixture hardlink"
                )
            hardlink.unlink()

            fifo = root / "archive-fifo.opaque"
            os.mkfifo(fifo)
            with self.assertRaisesRegex(
                cache.CacheArchiveVerificationError, "not a regular file"
            ):
                cache._stream_identity(
                    fifo, maximum=len(before), label="fixture FIFO"
                )

            original_read = cache.os.read
            changed = False

            def mutate_after_read(descriptor: int, byte_count: int) -> bytes:
                nonlocal changed
                chunk = original_read(descriptor, byte_count)
                if chunk and not changed:
                    changed = True
                    regular.write_bytes(after)
                return chunk

            regular.write_bytes(before)
            with (
                mock.patch.object(cache.os, "read", side_effect=mutate_after_read),
                self.assertRaisesRegex(
                    cache.CacheArchiveVerificationError,
                    "changed while being read",
                ),
            ):
                cache._stream_identity(
                    regular, maximum=len(before), label="raced fixture archive"
                )

    def test_descriptor_relative_open_rejects_or_outlives_ancestor_symlinks(self) -> None:
        with tempfile.TemporaryDirectory() as temporary_name:
            root = pathlib.Path(temporary_name).resolve(strict=True)
            outside = root / "outside"
            outside.mkdir()
            (outside / "archive.opaque").write_bytes(b"outside bytes")
            (outside / "nested").mkdir()
            (outside / "nested" / "archive.opaque").write_bytes(
                b"outside swapped bytes"
            )

            linked_parent = root / "linked-parent"
            linked_parent.symlink_to(outside, target_is_directory=True)
            with self.assertRaisesRegex(
                cache.CacheArchiveVerificationError,
                "cannot open",
            ):
                cache._stream_identity(
                    linked_parent / "archive.opaque",
                    maximum=1024,
                    label="ancestor symlink fixture",
                )

            victim_parent = root / "victim-parent"
            nested = victim_parent / "nested"
            nested.mkdir(parents=True)
            inside_raw = b"descriptor-bound inside bytes"
            (nested / "archive.opaque").write_bytes(inside_raw)
            attacked_path = victim_parent / "nested" / "archive.opaque"
            held_parent = root / "held-original-parent"
            original_open = cache.os.open
            swapped = False

            def swap_after_parent_open(
                path: Any,
                flags: int,
                mode: int = 0o777,
                *,
                dir_fd: int | None = None,
            ) -> int:
                nonlocal swapped
                descriptor = original_open(path, flags, mode, dir_fd=dir_fd)
                if path == "victim-parent" and not swapped:
                    swapped = True
                    victim_parent.rename(held_parent)
                    victim_parent.symlink_to(outside, target_is_directory=True)
                return descriptor

            supported_dir_fd = set(cache.os.supports_dir_fd)
            supported_dir_fd.add(swap_after_parent_open)
            with (
                mock.patch.object(cache.os, "open", new=swap_after_parent_open),
                mock.patch.object(
                    cache.os,
                    "supports_dir_fd",
                    supported_dir_fd,
                ),
            ):
                observed = cache._stream_identity(
                    attacked_path,
                    maximum=1024,
                    label="ancestor swap fixture",
                )
            self.assertTrue(swapped)
            self.assertEqual(
                observed,
                {
                    "byte_count": len(inside_raw),
                    "sha256": cache._sha256(inside_raw),
                },
            )

    def test_bounded_json_rejects_oversize_duplicate_keys_and_floats(self) -> None:
        with tempfile.TemporaryDirectory() as temporary_name:
            root = pathlib.Path(temporary_name).resolve(strict=True)
            oversized = root / "oversized.json"
            oversized.write_bytes(b"{}")
            with self.assertRaisesRegex(
                cache.CacheArchiveVerificationError,
                "exceeds the byte ceiling",
            ):
                cache._read_bounded(
                    oversized,
                    maximum=1,
                    label="oversized JSON fixture",
                )

            duplicate = root / "duplicate.json"
            duplicate.write_bytes(b'{"profile":"a","profile":"b"}')
            with self.assertRaisesRegex(
                cache.CacheArchiveVerificationError,
                "duplicate object key",
            ):
                cache._read_json(
                    duplicate,
                    maximum=1024,
                    label="duplicate JSON fixture",
                )

            floating = root / "floating.json"
            floating.write_bytes(b'{"value":1.5}')
            with self.assertRaisesRegex(
                cache.CacheArchiveVerificationError,
                "floating-point",
            ):
                cache._read_json(
                    floating,
                    maximum=1024,
                    label="floating JSON fixture",
                )


class SourceOnlyDependencyTest(unittest.TestCase):
    def test_forged_timestamp_valid_validator_bytecode_is_ignored(self) -> None:
        with tempfile.TemporaryDirectory() as temporary_name:
            root = pathlib.Path(temporary_name).resolve(strict=True)
            source = root / "draft202012.py"
            raw = cache.DRAFT202012_PATH.read_bytes()
            source.write_bytes(raw)
            fixed_mtime = 1_700_000_000
            os.utime(source, (fixed_mtime, fixed_mtime))
            metadata = source.stat()

            forged_code = compile(
                "MARKER = 'forged-timestamp-cache-loaded'\n",
                str(source),
                "exec",
            )
            bytecode = pathlib.Path(importlib.util.cache_from_source(str(source)))
            bytecode.parent.mkdir()
            bytecode.write_bytes(
                importlib.util.MAGIC_NUMBER
                + struct.pack("<III", 0, int(metadata.st_mtime), metadata.st_size)
                + marshal.dumps(forged_code)
            )

            control = subprocess.run(
                [
                    sys.executable,
                    "-c",
                    "import draft202012; print(draft202012.MARKER)",
                ],
                check=True,
                cwd=root,
                env={
                    "LANG": "C",
                    "LC_ALL": "C",
                    "PATH": "/usr/bin:/bin",
                    "PYTHONDONTWRITEBYTECODE": "1",
                    "PYTHONPATH": str(root),
                },
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                text=True,
            )
            self.assertEqual(
                control.stdout.strip(),
                "forged-timestamp-cache-loaded",
            )

            with (
                mock.patch.object(cache, "DRAFT202012_PATH", source),
                mock.patch.object(
                    cache,
                    "CHECKED_DRAFT202012_SHA256",
                    cache._sha256(raw),
                ),
                mock.patch.object(cache, "CHECKED_DRAFT202012_SIZE", len(raw)),
                mock.patch.object(cache, "_CHECKED_DRAFT_VALIDATOR", None),
            ):
                checked = cache._checked_draft_validator()
            self.assertFalse(hasattr(checked, "MARKER"))
            self.assertEqual(
                checked.DIALECT,
                "https://json-schema.org/draft/2020-12/schema",
            )


class ProhibitedSurfaceTest(unittest.TestCase):
    def test_source_has_no_archive_parser_extractor_network_or_process_surface(self) -> None:
        source = pathlib.Path(cache.__file__).read_text(encoding="utf-8")
        tree = ast.parse(source)
        imported_roots: set[str] = set()
        called_names: set[str] = set()
        for node in ast.walk(tree):
            if isinstance(node, ast.Import):
                imported_roots.update(alias.name.split(".", 1)[0] for alias in node.names)
            elif isinstance(node, ast.ImportFrom) and node.module:
                imported_roots.add(node.module.split(".", 1)[0])
            elif isinstance(node, ast.Call):
                if isinstance(node.func, ast.Name):
                    called_names.add(node.func.id)
                elif isinstance(node.func, ast.Attribute):
                    called_names.add(node.func.attr)

        self.assertTrue(
            imported_roots.isdisjoint(
                {
                    "compression",
                    "http",
                    "requests",
                    "shutil",
                    "socket",
                    "subprocess",
                    "tarfile",
                    "urllib",
                    "zstandard",
                }
            ),
            imported_roots,
        )
        self.assertTrue(
            called_names.isdisjoint(
                {
                    "Popen",
                    "create_connection",
                    "extract",
                    "extractall",
                    "posix_spawn",
                    "run",
                    "system",
                    "unpack_archive",
                    "urlopen",
                }
            ),
            called_names,
        )


if __name__ == "__main__":
    unittest.main()
