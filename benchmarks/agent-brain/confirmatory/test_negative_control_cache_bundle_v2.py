#!/usr/bin/env python3
"""Synthetic, offline tests for the regular-file-only cache bundle verifier."""

from __future__ import annotations

import argparse
import ast
import contextlib
import copy
import hashlib
import importlib.util
import inspect
import io
import json
import marshal
import os
import pathlib
import struct
import subprocess
import sys
import tempfile
import unittest
from collections.abc import Callable, Mapping
from typing import Any, cast
from unittest import mock

import negative_control_cache_archive_v1 as archive
import negative_control_cache_bundle_v2 as bundle
import task_negative_control_gate_v1 as gate
import task_negative_control_plan_v2 as run_plan


HERE = pathlib.Path(__file__).parent


def _sha256(raw: bytes) -> str:
    return hashlib.sha256(raw).hexdigest()


def _checked_plan() -> dict[str, Any]:
    value, _raw = run_plan._load_json(archive.RUN_PLAN_PATH)
    run_plan.validate_plan(value)
    return value


def _format_contract() -> dict[str, Any]:
    value = json.loads(bundle.FORMAT_PATH.read_text(encoding="utf-8"))
    assert type(value) is dict
    return value


def _manifest_for(
    payloads: Mapping[tuple[str, str], bytes],
    *,
    archive_raw: bytes,
) -> dict[str, Any]:
    entries = [
        {
            "byte_count": len(raw),
            "path": path,
            "repository_key": repository_key,
            "sha256": _sha256(raw),
        }
        for (repository_key, path), raw in sorted(payloads.items())
    ]
    return gate.build_cache_seed_manifest(
        plan=_checked_plan(),
        manifest_schema_path=archive.MANIFEST_SCHEMA_PATH,
        archive_file="synthetic-offline-go-cache.bundle-v2",
        archive_sha256=_sha256(archive_raw),
        archive_byte_count=len(archive_raw),
        entries=entries,
    )


def _bundle_size(
    manifest: Mapping[str, Any], payloads: Mapping[tuple[str, str], bytes]
) -> int:
    entries = cast(list[dict[str, Any]], manifest["contents"]["entries"])
    return (
        bundle.HEADER_BYTES
        + bundle.TRAILER_BYTES
        + sum(
            bundle.RECORD_FIXED_BYTES
            + len(cast(str, entry["path"]).encode("utf-8"))
            + len(payloads[(entry["repository_key"], entry["path"])])
            for entry in entries
        )
    )


def _encode_bundle(
    manifest: Mapping[str, Any], payloads: Mapping[tuple[str, str], bytes]
) -> bytes:
    """Test-only encoder for the frozen uncompressed byte grammar."""

    contents = cast(dict[str, Any], manifest["contents"])
    entries = cast(list[dict[str, Any]], contents["entries"])
    repository_counts = cast(dict[str, int], contents["repository_file_counts"])
    format_contract = _format_contract()
    bundle_bytes = _bundle_size(manifest, payloads)
    header = struct.pack(
        ">16sHHIQQQQQQ32s32s32s32s32s",
        bundle.HEADER_MAGIC,
        bundle.FORMAT_VERSION,
        bundle.HEADER_BYTES,
        0,
        bundle_bytes,
        contents["file_count"],
        contents["unpacked_byte_count"],
        repository_counts["entire-brain"],
        repository_counts["entire-db"],
        repository_counts["entire-graph"],
        bytes.fromhex(format_contract["format_contract_sha256"]),
        bytes.fromhex(bundle._manifest_projection_sha256(manifest)),
        bytes.fromhex(contents["inventory_sha256"]),
        bytes.fromhex(manifest["run_plan"]["plan_sha256"]),
        bytes.fromhex(manifest["run_plan"]["toolchain_bindings_sha256"]),
    )
    if len(header) != 232:
        raise AssertionError(f"test header has {len(header)} bytes")

    record_parts: list[bytes] = []
    for ordinal, entry in enumerate(entries):
        repository_key = cast(str, entry["repository_key"])
        path = cast(str, entry["path"])
        path_raw = path.encode("utf-8")
        content = payloads[(repository_key, path)]
        fixed = struct.pack(
            ">4sBBHIQ32s",
            bundle.RECORD_MAGIC,
            bundle.REPOSITORY_IDS[repository_key],
            0,
            len(path_raw),
            ordinal,
            len(content),
            bytes.fromhex(_sha256(content)),
        )
        if len(fixed) != 52:
            raise AssertionError(f"test record header has {len(fixed)} bytes")
        record_parts.extend((fixed, path_raw, content))
    records = b"".join(record_parts)
    records_sha = hashlib.sha256(bundle.RECORDS_DOMAIN + records).digest()
    trailer_prefix = struct.pack(
        ">16sQQQ32s",
        bundle.TRAILER_MAGIC,
        bundle_bytes,
        contents["file_count"],
        contents["unpacked_byte_count"],
        records_sha,
    )
    prefix_sha = hashlib.sha256(
        bundle.PREFIX_DOMAIN + header + records + trailer_prefix
    ).digest()
    trailer = trailer_prefix + prefix_sha
    if len(trailer) != 104:
        raise AssertionError(f"test trailer has {len(trailer)} bytes")
    raw = header + records + trailer
    if len(raw) != bundle_bytes:
        raise AssertionError("test bundle byte formula differs")
    return raw


def _valid_manifest_and_bundle() -> tuple[
    dict[str, Any], dict[tuple[str, str], bytes], bytes
]:
    payloads = {
        ("entire-brain", "gocache/00/brain.a"): b"brain-cache-payload",
        ("entire-db", "gocache/01/db.a"): b"database-cache-payload",
        (
            "entire-graph",
            "gomodcache/example.org/graph@v1.0.0/mod.zip",
        ): b"graph-cache-payload",
    }
    # The manifest projection deliberately nulls archive identity and its own
    # self-hash, so the final manifest can be sealed after the bundle is built.
    provisional = _manifest_for(payloads, archive_raw=b"provisional")
    raw = _encode_bundle(provisional, payloads)
    manifest = _manifest_for(payloads, archive_raw=raw)
    final_raw = _encode_bundle(manifest, payloads)
    if final_raw != raw:
        raise AssertionError("manifest projection unexpectedly depends on raw identity")
    return manifest, payloads, raw


def _namedfork_manifest_and_bundle(
    component: str,
) -> tuple[dict[str, Any], dict[tuple[str, str], bytes], bytes]:
    payloads = {
        ("entire-brain", f"gocache/{component}/rsrc"): b"named-fork-payload",
        ("entire-db", "gocache/01/db.a"): b"database-cache-payload",
        (
            "entire-graph",
            "gomodcache/example.org/graph@v1.0.0/mod.zip",
        ): b"graph-cache-payload",
    }
    provisional = _manifest_for(payloads, archive_raw=b"provisional")
    raw = _encode_bundle(provisional, payloads)
    manifest = _manifest_for(payloads, archive_raw=raw)
    final_raw = _encode_bundle(manifest, payloads)
    if final_raw != raw:
        raise AssertionError("named-fork manifest projection depends on raw identity")
    return manifest, payloads, raw


def _reseal_v1_material(v1: Any, value: dict[str, Any]) -> None:
    value["material_declaration_sha256"] = None
    value["material_declaration_sha256"] = v1._field_self_hash(
        value, "material_declaration_sha256"
    )


def _reseal_v1_contract(v1: Any, value: dict[str, Any]) -> None:
    value["verifier_contract_sha256"] = None
    value["verifier_contract_sha256"] = v1._field_self_hash(
        value, "verifier_contract_sha256"
    )


def _approved_dependencies(
    manifest: dict[str, Any], bundle_raw: bytes
) -> tuple[dict[str, Any], dict[str, Any], bytes]:
    """Bind synthetic bytes through the real v1 identity-review contracts."""

    dependencies = bundle._load_fixed_dependencies()
    v1 = dependencies["v1"]
    manifest_raw = v1._render(manifest)
    v1_dependencies = copy.deepcopy(dependencies["v1_dependencies"])
    material = copy.deepcopy(v1_dependencies["material"])
    contents = manifest["contents"]
    material["status"] = bundle.V1_APPROVED_MATERIAL_STATUS
    material["material"].update(
        {
            "archive_byte_count": len(bundle_raw),
            "archive_file": manifest["archive"]["artifact_file"],
            "archive_sha256": _sha256(bundle_raw),
            "content_inventory_sha256": contents["inventory_sha256"],
            "file_count": contents["file_count"],
            "manifest_file": "synthetic-offline-go-cache-manifest-v1.json",
            "manifest_file_sha256": _sha256(manifest_raw),
            "manifest_sha256": manifest["manifest_sha256"],
            "repository_file_counts": copy.deepcopy(
                contents["repository_file_counts"]
            ),
            "unpacked_byte_count": contents["unpacked_byte_count"],
        }
    )
    material["review"].update(
        {
            "disposition": "approved_source_review_identity_only",
            "source_review_effect": "raw_identity_only_not_e0_runtime_authority",
        }
    )
    _reseal_v1_material(v1, material)
    v1_dependencies["material"] = material
    v1_dependencies["material_raw"] = v1._render(material)
    v1.validate_material_declaration(
        material,
        schema=v1_dependencies["material_schema"],
    )

    v1_contract = copy.deepcopy(dependencies["v1_verifier_contract"])
    declared = material["material"]
    v1_contract["status"] = bundle.V1_APPROVED_VERIFIER_STATUS
    v1_contract["material_binding"].update(
        {
            "archive_byte_count": declared["archive_byte_count"],
            "archive_file": declared["archive_file"],
            "archive_sha256": declared["archive_sha256"],
            "artifact_sha256": _sha256(v1_dependencies["material_raw"]),
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
    _reseal_v1_contract(v1, v1_contract)
    v1.validate_verifier_contract(
        v1_contract,
        schema=v1_dependencies["verifier_schema"],
        dependencies=v1_dependencies,
    )
    dependencies["v1_dependencies"] = v1_dependencies
    dependencies["v1_verifier_contract"] = v1_contract
    dependencies["v1_verifier_contract_raw"] = v1._render(v1_contract)
    verifier_contract = bundle._build_verifier_contract(dependencies)
    return dependencies, verifier_contract, manifest_raw


def _record_offsets(
    manifest: Mapping[str, Any], payloads: Mapping[tuple[str, str], bytes]
) -> list[tuple[int, int, int]]:
    """Return (fixed, path, content) offsets for each canonical record."""

    result: list[tuple[int, int, int]] = []
    offset = bundle.HEADER_BYTES
    for entry in manifest["contents"]["entries"]:
        path = cast(str, entry["path"])
        path_offset = offset + bundle.RECORD_FIXED_BYTES
        content_offset = path_offset + len(path.encode("utf-8"))
        result.append((offset, path_offset, content_offset))
        offset = content_offset + len(
            payloads[(entry["repository_key"], entry["path"])]
        )
    return result


def _parse_raw(
    root: pathlib.Path,
    raw: bytes,
    manifest: Mapping[str, Any],
) -> dict[str, Any]:
    path = root / "fixture.bundle-v2"
    path.write_bytes(raw)
    descriptor, directories = bundle._open_source_descriptor(
        path, label="synthetic cache bundle"
    )
    try:
        metadata = os.fstat(descriptor)
        return bundle._parse_content_pass(
            descriptor,
            bundle_stat=metadata,
            manifest=manifest,
            format_contract=_format_contract(),
        )
    finally:
        os.close(descriptor)
        for current in reversed(directories):
            os.close(current)


class ExactWireGrammarTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory(
            prefix="negative-control-cache-bundle-wire-tests-"
        )
        self.root = pathlib.Path(self.temporary.name).resolve(strict=True)
        self.manifest, self.payloads, self.raw = _valid_manifest_and_bundle()
        self.offsets = _record_offsets(self.manifest, self.payloads)

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def assert_rejected(self, raw: bytes, pattern: str | None = None) -> None:
        context = (
            self.assertRaisesRegex(bundle.CacheBundleVerificationError, pattern)
            if pattern is not None
            else self.assertRaises(bundle.CacheBundleVerificationError)
        )
        with context:
            _parse_raw(self.root, raw, self.manifest)

    def test_exact_232_52_104_grammar_and_happy_parse(self) -> None:
        self.assertEqual(bundle.HEADER_BYTES, 232)
        self.assertEqual(bundle.RECORD_FIXED_BYTES, 52)
        self.assertEqual(bundle.TRAILER_BYTES, 104)
        self.assertEqual(struct.calcsize(">16sHHIQQQQQQ32s32s32s32s32s"), 232)
        self.assertEqual(struct.calcsize(">4sBBHIQ32s"), 52)
        self.assertEqual(struct.calcsize(">16sQQQ32s32s"), 104)
        report = _parse_raw(self.root, self.raw, self.manifest)
        self.assertEqual(report["bundle_byte_count"], len(self.raw))
        self.assertEqual(report["file_count"], 3)
        self.assertEqual(
            report["unpacked_byte_count"],
            sum(len(value) for value in self.payloads.values()),
        )

    def test_truncation_at_every_fixed_field_and_payload_boundary_fails(self) -> None:
        field_ends = {
            1,
            15,
            16,
            17,
            18,
            19,
            20,
            23,
            24,
            31,
            32,
            39,
            40,
            47,
            48,
            55,
            56,
            63,
            64,
            71,
            72,
            103,
            104,
            135,
            136,
            167,
            168,
            199,
            200,
            231,
            232,
        }
        for fixed, path_offset, content_offset in self.offsets:
            field_ends.update(
                fixed + end
                for end in (1, 3, 4, 5, 6, 7, 8, 11, 12, 19, 20, 51, 52)
            )
            field_ends.update(
                {
                    path_offset + 1,
                    content_offset,
                    content_offset + 1,
                }
            )
        trailer = len(self.raw) - bundle.TRAILER_BYTES
        field_ends.update(
            trailer + end
            for end in (1, 15, 16, 23, 24, 31, 32, 39, 40, 71, 72, 103)
        )
        for cut in sorted(value for value in field_ends if value < len(self.raw)):
            with self.subTest(cut=cut):
                self.assert_rejected(self.raw[:cut])

    def test_trailing_bytes_and_concatenated_member_fail(self) -> None:
        for suffix in (b"\0", self.raw, self.raw[-bundle.TRAILER_BYTES :]):
            with self.subTest(suffix_bytes=len(suffix)):
                self.assert_rejected(self.raw + suffix)

    def test_header_magic_version_length_flags_counts_and_bindings_fail(self) -> None:
        mutations: list[tuple[str, int, bytes]] = [
            ("magic", 0, b"X"),
            ("version", 16, struct.pack(">H", 1)),
            ("header length", 18, struct.pack(">H", 231)),
            ("flags", 20, struct.pack(">I", 1)),
            ("bundle bytes", 24, struct.pack(">Q", len(self.raw) + 1)),
            ("files", 32, struct.pack(">Q", 4)),
            ("unpacked", 40, struct.pack(">Q", 1)),
            ("brain count", 48, struct.pack(">Q", 2)),
            ("db count", 56, struct.pack(">Q", 2)),
            ("graph count", 64, struct.pack(">Q", 2)),
        ]
        mutations.extend(
            (label, offset, b"\0" * 32)
            for label, offset in (
                ("format", 72),
                ("projection", 104),
                ("inventory", 136),
                ("plan", 168),
                ("toolchain", 200),
            )
        )
        for label, offset, replacement in mutations:
            changed = bytearray(self.raw)
            changed[offset : offset + len(replacement)] = replacement
            with self.subTest(label=label):
                self.assert_rejected(bytes(changed))

    def test_record_magic_repository_flags_ordinal_lengths_hashes_and_order_fail(self) -> None:
        first, first_path, first_content = self.offsets[0]
        second, _second_path, _second_content = self.offsets[1]
        mutations: list[tuple[str, int, bytes]] = [
            ("marker", first, b"FAIL"),
            ("repository id", first + 4, b"\x02"),
            ("unknown repository id", first + 4, b"\xff"),
            ("flags", first + 5, b"\x01"),
            ("empty path", first + 6, struct.pack(">H", 0)),
            ("oversize path", first + 6, struct.pack(">H", 1025)),
            ("ordinal", first + 8, struct.pack(">I", 1)),
            ("zero content", first + 12, struct.pack(">Q", 0)),
            ("huge content", first + 12, struct.pack(">Q", 2**64 - 1)),
            ("content declaration", first + 20, b"\0" * 32),
            ("later ordinal", second + 8, struct.pack(">I", 0)),
            ("content", first_content, bytes([self.raw[first_content] ^ 1])),
        ]
        for label, offset, replacement in mutations:
            changed = bytearray(self.raw)
            changed[offset : offset + len(replacement)] = replacement
            with self.subTest(label=label):
                self.assert_rejected(bytes(changed))

        # A byte-valid record reordering still violates manifest bijection and
        # ordinal/repository order; no alternate member ordering is accepted.
        first_end = second
        third = self.offsets[2][0]
        changed = self.raw[:first] + self.raw[second:third] + self.raw[first:first_end] + self.raw[third:]
        self.assert_rejected(changed)

    def test_utf8_nfc_path_root_separator_control_and_collision_bytes_fail(self) -> None:
        fixed, path_offset, _content_offset = self.offsets[0]
        original_path = cast(
            str, self.manifest["contents"]["entries"][0]["path"]
        ).encode("utf-8")
        replacements = (
            b"\xff" + original_path[1:],
            b"GOCACHE/00/brain.a",
            b"gocache//0/brain.a",
            b"gocache/../brain.a",
            b"gocache\\00\\brain.a",
            b"gocache/00/brain\x00a",
            "gocache/00/brai\u006e\u0301".encode("utf-8"),
        )
        for replacement in replacements:
            changed = bytearray(self.raw)
            if len(replacement) == len(original_path):
                changed[path_offset : path_offset + len(original_path)] = replacement
            else:
                changed[fixed + 6 : fixed + 8] = struct.pack(">H", len(replacement))
                changed[path_offset : path_offset + len(original_path)] = replacement
            with self.subTest(replacement=replacement):
                self.assert_rejected(bytes(changed))

    def test_trailer_magic_counts_record_digest_and_prefix_digest_fail(self) -> None:
        trailer = len(self.raw) - bundle.TRAILER_BYTES
        mutations = (
            ("magic", trailer, b"X"),
            ("bundle bytes", trailer + 16, struct.pack(">Q", len(self.raw) + 1)),
            ("files", trailer + 24, struct.pack(">Q", 4)),
            ("unpacked", trailer + 32, struct.pack(">Q", 1)),
            ("records hash", trailer + 40, b"\0" * 32),
            ("prefix hash", trailer + 72, b"\0" * 32),
        )
        for label, offset, replacement in mutations:
            changed = bytearray(self.raw)
            changed[offset : offset + len(replacement)] = replacement
            with self.subTest(label=label):
                self.assert_rejected(bytes(changed))

    def test_every_payload_hash_is_checked_before_the_trailer_digests(self) -> None:
        for ordinal, (_fixed, _path, content) in enumerate(self.offsets):
            changed = bytearray(self.raw)
            changed[content] ^= 1
            with self.subTest(ordinal=ordinal), self.assertRaisesRegex(
                bundle.CacheBundleVerificationError,
                rf"record\[{ordinal}\] content SHA-256 differs",
            ):
                _parse_raw(self.root, bytes(changed), self.manifest)

    def test_huge_lengths_fail_before_any_large_read_or_allocation(self) -> None:
        first, _path, _content = self.offsets[0]
        changed = bytearray(self.raw)
        changed[first + 12 : first + 20] = struct.pack(">Q", 2**64 - 1)
        requested: list[int] = []
        original_read = bundle.os.read
        fixture = self.root / "fixture.bundle-v2"

        def bounded_read(descriptor: int, count: int) -> bytes:
            self.assertLessEqual(count, bundle.READ_CHUNK_BYTES)
            metadata = os.fstat(descriptor)
            path_metadata = fixture.stat()
            if (metadata.st_dev, metadata.st_ino) == (
                path_metadata.st_dev,
                path_metadata.st_ino,
            ):
                requested.append(count)
            return original_read(descriptor, count)

        with (
            mock.patch.object(bundle.os, "read", side_effect=bounded_read),
            self.assertRaises(bundle.CacheBundleVerificationError),
        ):
            _parse_raw(self.root, bytes(changed), self.manifest)
        self.assertEqual(requested, [bundle.HEADER_BYTES, bundle.RECORD_FIXED_BYTES])


class ManifestProjectionTest(unittest.TestCase):
    def setUp(self) -> None:
        self.manifest, self.payloads, self.raw = _valid_manifest_and_bundle()

    def test_projection_is_exactly_domain_plus_three_nulled_fields(self) -> None:
        original = copy.deepcopy(self.manifest)
        projected = copy.deepcopy(self.manifest)
        projected["archive"]["byte_count"] = None
        projected["archive"]["sha256"] = None
        projected["manifest_sha256"] = None
        expected = hashlib.sha256(
            bundle.MANIFEST_PROJECTION_DOMAIN
            + bundle._checked_v1()._canonical_bytes(projected)
        ).hexdigest()
        self.assertEqual(bundle._manifest_projection_sha256(self.manifest), expected)
        self.assertEqual(self.manifest, original)

    def test_only_archive_identity_and_manifest_self_hash_are_projected_out(self) -> None:
        baseline = bundle._manifest_projection_sha256(self.manifest)
        for label, mutate in (
            (
                "archive bytes",
                lambda value: value["archive"].__setitem__("byte_count", 1),
            ),
            (
                "archive hash",
                lambda value: value["archive"].__setitem__("sha256", "a" * 64),
            ),
            (
                "manifest hash",
                lambda value: value.__setitem__("manifest_sha256", "b" * 64),
            ),
        ):
            changed = copy.deepcopy(self.manifest)
            mutate(changed)
            with self.subTest(label=label):
                self.assertEqual(bundle._manifest_projection_sha256(changed), baseline)

        material_changes = (
            lambda value: value["archive"].__setitem__(
                "artifact_file", "different.bundle-v2"
            ),
            lambda value: value["contents"]["entries"][0].__setitem__(
                "sha256", "c" * 64
            ),
            lambda value: value["run_plan"].__setitem__("plan_sha256", "d" * 64),
            lambda value: value["authority"].__setitem__(
                "paid_execution", "forbidden_changed"
            ),
        )
        for index, mutate in enumerate(material_changes):
            changed = copy.deepcopy(self.manifest)
            mutate(changed)
            with self.subTest(index=index):
                self.assertNotEqual(bundle._manifest_projection_sha256(changed), baseline)


class CheckedFormatAndSchemaTest(unittest.TestCase):
    def test_fixed_format_and_schemas_are_canonical_pinned_and_meta_audited(self) -> None:
        dependencies = bundle._load_fixed_dependencies()
        v1 = dependencies["v1"]
        self.assertEqual(
            dependencies["format_raw"], v1._render(dependencies["format"])
        )
        self.assertEqual(
            _sha256(dependencies["format_raw"]), bundle.CHECKED_FORMAT_FILE_SHA256
        )
        self.assertEqual(
            dependencies["format"]["format_contract_sha256"],
            bundle.CHECKED_FORMAT_SHA256,
        )
        self.assertEqual(
            dependencies["format"]["format_contract_sha256"],
            v1._field_self_hash(dependencies["format"], "format_contract_sha256"),
        )
        for raw, expected in (
            (dependencies["format_schema_raw"], bundle.CHECKED_FORMAT_SCHEMA_SHA256),
            (dependencies["report_schema_raw"], bundle.CHECKED_REPORT_SCHEMA_SHA256),
            (dependencies["verifier_schema_raw"], bundle.CHECKED_VERIFIER_SCHEMA_SHA256),
        ):
            self.assertEqual(_sha256(raw), expected)
        for schema, path, label in (
            (dependencies["format_schema"], bundle.FORMAT_SCHEMA_PATH, "format"),
            (dependencies["report_schema"], bundle.REPORT_SCHEMA_PATH, "report"),
            (dependencies["verifier_schema"], bundle.VERIFIER_SCHEMA_PATH, "verifier"),
        ):
            v1._audit_schema(schema, schema_name=path.name, label=label)

    def test_each_fixed_format_or_schema_pin_fails_closed_when_changed(self) -> None:
        for constant in (
            "CHECKED_FORMAT_FILE_SHA256",
            "CHECKED_FORMAT_SHA256",
            "CHECKED_FORMAT_SCHEMA_SHA256",
            "CHECKED_REPORT_SCHEMA_SHA256",
            "CHECKED_VERIFIER_SCHEMA_SHA256",
        ):
            with (
                self.subTest(constant=constant),
                mock.patch.object(bundle, constant, "a" * 64),
                self.assertRaises(bundle.CacheBundleVerificationError),
            ):
                bundle._load_fixed_dependencies()

    def test_schema_meta_audit_rejects_an_invalid_schema_before_use(self) -> None:
        dependencies = bundle._load_fixed_dependencies()
        with self.assertRaises(dependencies["v1"].CacheArchiveVerificationError):
            dependencies["v1"]._audit_schema(
                {
                    "$schema": "https://json-schema.org/draft/2020-12/schema",
                    "type": "definitely-not-a-json-schema-type",
                },
                schema_name="invalid.schema.json",
                label="invalid synthetic schema",
            )


class RawDescriptorPassTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory(
            prefix="negative-control-cache-bundle-descriptor-tests-"
        )
        self.root = pathlib.Path(self.temporary.name).resolve(strict=True)
        self.manifest, self.payloads, self.raw = _valid_manifest_and_bundle()
        self.path = self.root / "fixture.bundle-v2"
        self.path.write_bytes(self.raw)

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def _open(self) -> tuple[int, list[int]]:
        return bundle._open_source_descriptor(self.path, label="synthetic cache bundle")

    def test_raw_identity_then_content_parse_reuses_one_held_descriptor(self) -> None:
        descriptor, directories = self._open()
        try:
            identity, metadata = bundle._raw_identity_pass(
                descriptor,
                expected_byte_count=len(self.raw),
                expected_sha256=_sha256(self.raw),
            )
            self.assertEqual(
                identity,
                {"byte_count": len(self.raw), "sha256": _sha256(self.raw)},
            )
            self.assertEqual(os.lseek(descriptor, 0, os.SEEK_CUR), 0)
            parsed = bundle._parse_content_pass(
                descriptor,
                bundle_stat=metadata,
                manifest=self.manifest,
                format_contract=_format_contract(),
            )
            self.assertEqual(parsed["file_count"], 3)
        finally:
            os.close(descriptor)
            for current in reversed(directories):
                os.close(current)

    def test_path_replacement_between_passes_cannot_substitute_the_held_inode(self) -> None:
        descriptor, directories = self._open()
        held = self.root / "held-original.bundle-v2"
        try:
            _identity, metadata = bundle._raw_identity_pass(
                descriptor,
                expected_byte_count=len(self.raw),
                expected_sha256=_sha256(self.raw),
            )
            self.path.rename(held)
            self.path.write_bytes(b"x" * len(self.raw))
            with self.assertRaisesRegex(
                bundle.CacheBundleVerificationError,
                "changed during content verification",
            ):
                bundle._parse_content_pass(
                    descriptor,
                    bundle_stat=metadata,
                    manifest=self.manifest,
                    format_contract=_format_contract(),
                )
            self.assertEqual(self.path.read_bytes(), b"x" * len(self.raw))
        finally:
            os.close(descriptor)
            for current in reversed(directories):
                os.close(current)

    def test_raw_hash_size_and_u64_ceiling_mismatches_fail_closed(self) -> None:
        for label, byte_count, digest in (
            ("size", len(self.raw) + 1, _sha256(self.raw)),
            ("hash", len(self.raw), "a" * 64),
            ("too small", bundle.HEADER_BYTES + bundle.TRAILER_BYTES - 1, _sha256(self.raw)),
            ("u64 ceiling", 2**64 - 1, _sha256(self.raw)),
        ):
            descriptor, directories = self._open()
            try:
                with self.subTest(label=label), self.assertRaises(
                    bundle.CacheBundleVerificationError
                ):
                    bundle._raw_identity_pass(
                        descriptor,
                        expected_byte_count=byte_count,
                        expected_sha256=digest,
                    )
            finally:
                os.close(descriptor)
                for current in reversed(directories):
                    os.close(current)

    def test_symlink_hardlink_fifo_and_ancestor_symlink_are_rejected(self) -> None:
        target = self.root / "target.bundle-v2"
        self.path.rename(target)

        self.path.symlink_to(target)
        with self.assertRaises(bundle.CacheBundleVerificationError):
            self._open()
        self.path.unlink()

        os.link(target, self.path)
        descriptor, directories = self._open()
        try:
            with self.assertRaisesRegex(bundle.CacheBundleVerificationError, "link"):
                bundle._raw_identity_pass(
                    descriptor,
                    expected_byte_count=len(self.raw),
                    expected_sha256=_sha256(self.raw),
                )
        finally:
            os.close(descriptor)
            for current in reversed(directories):
                os.close(current)
        self.path.unlink()

        os.mkfifo(self.path)
        with self.assertRaisesRegex(bundle.CacheBundleVerificationError, "regular file"):
            self._open()
        self.path.unlink()

        linked_parent = self.root / "linked-parent"
        linked_parent.symlink_to(self.root, target_is_directory=True)
        with self.assertRaises(bundle.CacheBundleVerificationError):
            bundle._open_source_descriptor(
                linked_parent / target.name,
                label="bundle beneath a symlink ancestor",
            )

    def test_ancestor_swap_after_open_cannot_redirect_the_descriptor_walk(self) -> None:
        victim = self.root / "victim"
        nested = victim / "nested"
        nested.mkdir(parents=True)
        attacked = nested / "fixture.bundle-v2"
        attacked.write_bytes(self.raw)
        outside = self.root / "outside"
        (outside / "nested").mkdir(parents=True)
        (outside / "nested" / "fixture.bundle-v2").write_bytes(b"x" * len(self.raw))
        held = self.root / "held-victim"
        original_open = bundle.os.open
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
            if path == "victim" and not swapped:
                swapped = True
                victim.rename(held)
                victim.symlink_to(outside, target_is_directory=True)
            return descriptor

        supported = set(bundle.os.supports_dir_fd)
        supported.add(swap_after_parent_open)
        with (
            mock.patch.object(bundle.os, "open", new=swap_after_parent_open),
            mock.patch.object(bundle.os, "supports_dir_fd", supported),
        ):
            descriptor, directories = bundle._open_source_descriptor(
                attacked, label="ancestor-swap bundle"
            )
        try:
            identity, _metadata = bundle._raw_identity_pass(
                descriptor,
                expected_byte_count=len(self.raw),
                expected_sha256=_sha256(self.raw),
            )
        finally:
            os.close(descriptor)
            for current in reversed(directories):
                os.close(current)
        self.assertTrue(swapped)
        self.assertEqual(identity["sha256"], _sha256(self.raw))

    def test_in_place_data_or_metadata_race_fails_each_pass(self) -> None:
        original_read = bundle.os.read
        for label, mutate in (
            ("data", lambda: self.path.write_bytes(b"x" * len(self.raw))),
            (
                "metadata",
                lambda: os.utime(
                    self.path,
                    ns=(self.path.stat().st_atime_ns, self.path.stat().st_mtime_ns + 1),
                ),
            ),
        ):
            self.path.write_bytes(self.raw)
            descriptor, directories = self._open()
            changed = False

            def race(descriptor_under_test: int, count: int) -> bytes:
                nonlocal changed
                chunk = original_read(descriptor_under_test, count)
                if chunk and not changed:
                    changed = True
                    mutate()
                return chunk

            try:
                with (
                    self.subTest(label=label),
                    mock.patch.object(bundle.os, "read", side_effect=race),
                    self.assertRaisesRegex(
                        bundle.CacheBundleVerificationError,
                        "changed during raw identity verification",
                    ),
                ):
                    bundle._raw_identity_pass(
                        descriptor,
                        expected_byte_count=len(self.raw),
                        expected_sha256=_sha256(self.raw),
                    )
            finally:
                os.close(descriptor)
                for current in reversed(directories):
                    os.close(current)


class ManifestPathPolicyTest(unittest.TestCase):
    def setUp(self) -> None:
        self.manifest, self.payloads, self.raw = _valid_manifest_and_bundle()
        self.plan = _checked_plan()

    def assert_manifest_rejected(
        self, mutate: Callable[[dict[str, Any]], Any]
    ) -> None:
        changed = copy.deepcopy(self.manifest)
        mutate(changed)
        entries = changed["contents"]["entries"]
        changed["contents"]["file_count"] = len(entries)
        changed["contents"]["unpacked_byte_count"] = sum(
            entry["byte_count"] for entry in entries
        )
        changed["contents"]["repository_file_counts"] = {
            key: sum(entry["repository_key"] == key for entry in entries)
            for key in bundle.REPOSITORY_ORDER
        }
        changed["contents"]["inventory_sha256"] = gate._canonical_hash(entries)
        changed["manifest_sha256"] = gate._self_hash(changed, "manifest_sha256")
        with self.assertRaises(gate.GatePrimitiveError):
            gate.validate_cache_seed_manifest(
                changed,
                plan=self.plan,
                manifest_schema_path=archive.MANIFEST_SCHEMA_PATH,
            )

    def test_utf8_nfc_roots_components_separators_and_controls_fail(self) -> None:
        bad_paths = (
            "other/00/brain.a",
            "/gocache/00/brain.a",
            "gocache//brain.a",
            "gocache/./brain.a",
            "gocache/../brain.a",
            "gocache\\00\\brain.a",
            "gocache/00/brain\0a",
            "gocache/00/brain\x01a",
            "gocache/00/cafe\u0301.a",
            "gocache/" + "a" * 256,
            "gocache/" + "/".join("a" * 250 for _ in range(5)),
        )
        for path in bad_paths:
            with self.subTest(path=path):
                self.assert_manifest_rejected(
                    lambda value, path=path: value["contents"]["entries"][0].__setitem__(
                        "path", path
                    )
                )

    def test_global_exact_portable_and_ancestor_collisions_fail(self) -> None:
        cases = (
            (
                "exact",
                lambda value: value["contents"]["entries"][1].__setitem__(
                    "path", value["contents"]["entries"][0]["path"]
                ),
            ),
            (
                "casefold",
                lambda value: value["contents"]["entries"][1].__setitem__(
                    "path", "GOCACHE/00/BRAIN.A"
                ),
            ),
            (
                "ancestor",
                lambda value: (
                    value["contents"]["entries"][0].__setitem__(
                        "path", "gocache/00"
                    ),
                    value["contents"]["entries"][1].__setitem__(
                        "path", "gocache/00/child"
                    ),
                ),
            ),
            (
                "order",
                lambda value: value["contents"]["entries"].reverse(),
            ),
        )
        for label, mutate in cases:
            with self.subTest(label=label):
                self.assert_manifest_rejected(mutate)


class APFSNamedForkPolicyTest(unittest.TestCase):
    COMPONENTS = ("..namedfork", "..NaMeDfOrK", "..NAMEDFORK")

    def test_manifest_v1_accepts_the_formerly_valid_named_fork_paths(self) -> None:
        for component in self.COMPONENTS:
            manifest, _payloads, raw = _namedfork_manifest_and_bundle(component)
            with self.subTest(component=component):
                gate.validate_cache_seed_manifest(
                    manifest,
                    plan=_checked_plan(),
                    manifest_schema_path=archive.MANIFEST_SCHEMA_PATH,
                )
                dependencies, _contract, _manifest_raw = _approved_dependencies(
                    manifest, raw
                )
                v1 = dependencies["v1"]
                self.assertEqual(
                    v1._validate_manifest_identity(
                        manifest,
                        schema=dependencies["v1_dependencies"]["manifest_schema"],
                        material=dependencies["v1_dependencies"]["material"],
                    ),
                    manifest,
                )

    def test_parse_rejects_exact_and_casefold_variants_before_bundle_read(self) -> None:
        with tempfile.TemporaryDirectory(
            prefix="negative-control-cache-bundle-namedfork-parse-"
        ) as temporary_name:
            root = pathlib.Path(temporary_name).resolve(strict=True)
            for component in self.COMPONENTS:
                manifest, _payloads, raw = _namedfork_manifest_and_bundle(component)
                path = root / f"{component.replace('.', 'd')}.bundle-v2"
                path.write_bytes(raw)
                descriptor = os.open(path, os.O_RDONLY)
                try:
                    metadata = os.fstat(descriptor)
                    with (
                        self.subTest(component=component),
                        mock.patch.object(
                            bundle.os,
                            "read",
                            side_effect=AssertionError(
                                "named-fork policy read bundle bytes"
                            ),
                        ) as reader,
                        self.assertRaisesRegex(
                            bundle.CacheBundleVerificationError,
                            r"APFS \.\.namedfork component is forbidden",
                        ),
                    ):
                        bundle._parse_content_pass(
                            descriptor,
                            bundle_stat=metadata,
                            manifest=manifest,
                            format_contract=_format_contract(),
                        )
                    reader.assert_not_called()
                    self.assertEqual(os.lseek(descriptor, 0, os.SEEK_CUR), 0)
                finally:
                    os.close(descriptor)

    def test_approved_seam_rejects_after_v1_validation_before_bundle_open_or_raw_pass(self) -> None:
        manifest, _payloads, raw = _namedfork_manifest_and_bundle("..NaMeDfOrK")
        dependencies, contract, manifest_raw = _approved_dependencies(manifest, raw)
        v1 = dependencies["v1"]
        with tempfile.TemporaryDirectory(
            prefix="negative-control-cache-bundle-namedfork-approved-"
        ) as temporary_name:
            root = pathlib.Path(temporary_name).resolve(strict=True)
            manifest_path = root / "synthetic-offline-go-cache-manifest-v1.json"
            bundle_path = root / manifest["archive"]["artifact_file"]
            manifest_path.write_bytes(manifest_raw)
            bundle_path.write_bytes(raw)
            original_validate = v1._validate_manifest_identity
            original_open = v1._open_regular_descriptor
            events: list[str] = []

            def tracked_validate(*args: Any, **kwargs: Any) -> Any:
                validated = original_validate(*args, **kwargs)
                events.append("manifest-v1-validated")
                return validated

            def guarded_open(path: pathlib.Path, *, label: str) -> Any:
                if path == bundle_path:
                    events.append("bundle-opened")
                    raise AssertionError("named-fork policy opened bundle locator")
                return original_open(path, label=label)

            with (
                mock.patch.object(
                    v1,
                    "_validate_manifest_identity",
                    side_effect=tracked_validate,
                ),
                mock.patch.object(
                    v1,
                    "_open_regular_descriptor",
                    side_effect=guarded_open,
                ),
                mock.patch.object(
                    bundle,
                    "_raw_identity_pass",
                    side_effect=AssertionError(
                        "named-fork policy reached bundle raw pass"
                    ),
                ) as raw_pass,
                self.assertRaisesRegex(
                    bundle.CacheBundleVerificationError,
                    r"APFS \.\.namedfork component is forbidden",
                ),
            ):
                bundle._verify_with_dependencies(
                    manifest_path,
                    bundle_path,
                    dependencies=dependencies,
                    verifier_contract=contract,
                )
            self.assertEqual(events, ["manifest-v1-validated"])
            raw_pass.assert_not_called()


class CheckedPendingContractAndPublicSurfaceTest(unittest.TestCase):
    def test_pending_contract_build_is_deterministic_non_authorizing_and_exact(self) -> None:
        first = bundle.build_verifier_contract()
        second = bundle.build_verifier_contract()
        self.assertEqual(first, second)
        self.assertEqual(first["status"], bundle.PENDING_STATUS)
        self.assertEqual(first["authority"], bundle.AUTHORITY)
        self.assertFalse(first["authority"]["execution_authority"])
        self.assertFalse(first["authority"]["owner_approval"])
        self.assertFalse(first["authority"]["atomic_consumption"])
        self.assertEqual(
            first["verification_policy"]["production_commands_absent"],
            ["pack", "extract", "stage"],
        )
        self.assertEqual(
            first["verifier_contract_sha256"],
            bundle._checked_v1()._field_self_hash(
                first, "verifier_contract_sha256"
            ),
        )
        self.assertEqual(
            first["verifier_contract_sha256"],
            "98de7291aceb5a84ff03d1862e66550fb4d67a55796634eacc4e610856829bdd",
        )
        checked, raw = bundle._checked_v1()._read_json(
            bundle.VERIFIER_CONTRACT_PATH,
            maximum=bundle.MAX_VERIFIER_CONTRACT_BYTES,
            label="checked synthetic bundle verifier contract",
            canonical=True,
            require_single_link=True,
        )
        self.assertEqual(
            _sha256(raw),
            "29d66a79f4c3083e006c823b44a5aeacb50538d426f0186e05fffa77226760ee",
        )
        self.assertEqual(checked, first)
        self.assertEqual(raw, bundle._checked_v1()._render(first))
        self.assertEqual(bundle.check_verifier_contract(), first)

    def test_cli_is_exactly_build_check_verify_with_no_injection_or_stage_surface(self) -> None:
        parser = bundle._parser()
        subparsers = [
            action
            for action in parser._actions
            if isinstance(action, argparse._SubParsersAction)
        ]
        self.assertEqual(len(subparsers), 1)
        self.assertEqual(set(subparsers[0].choices), {"build", "check", "verify"})
        self.assertEqual(
            vars(parser.parse_args(["build", "--output", "contract.json"])),
            {"command": "build", "output": pathlib.Path("contract.json")},
        )
        self.assertEqual(vars(parser.parse_args(["check"])), {"command": "check"})
        self.assertEqual(
            vars(parser.parse_args(["verify", "manifest.json", "bundle.v2"])),
            {
                "bundle": pathlib.Path("bundle.v2"),
                "command": "verify",
                "manifest": pathlib.Path("manifest.json"),
            },
        )
        for argv in (
            [],
            ["pack"],
            ["extract"],
            ["stage"],
            ["reserve"],
            ["authorize"],
            ["execute"],
            ["verify"],
            ["verify", "manifest.json"],
            ["verify", "manifest.json", "bundle.v2", "extra"],
            ["verify", "manifest.json", "bundle.v2", "--material", "x.json"],
            ["verify", "manifest.json", "bundle.v2", "--contract", "x.json"],
            ["check", "x.json"],
        ):
            with self.subTest(argv=argv), contextlib.redirect_stderr(io.StringIO()):
                with self.assertRaises(SystemExit):
                    parser.parse_args(argv)

        self.assertEqual(
            tuple(inspect.signature(bundle.verify_bundle_files).parameters),
            ("manifest_path", "bundle_path"),
        )
        with self.assertRaises(TypeError):
            bundle.verify_bundle_files(
                pathlib.Path("manifest.json"),
                pathlib.Path("bundle.v2"),
                dependencies={},  # pyright: ignore[reportCallIssue]
            )

    def test_pending_public_verify_fails_before_either_caller_locator_is_opened(self) -> None:
        manifest_path = pathlib.Path("must-not-open-manifest.json")
        bundle_path = pathlib.Path("must-not-open-bundle.v2")
        dependencies = bundle._load_fixed_dependencies()
        pending_contract = bundle._build_verifier_contract(dependencies)
        v1 = dependencies["v1"]
        original_open = v1._open_regular_descriptor
        attempted: list[pathlib.Path] = []

        def guarded_open(path: pathlib.Path, *, label: str) -> Any:
            if path in {manifest_path, bundle_path}:
                attempted.append(path)
                raise AssertionError(f"pending verifier opened {label}")
            return original_open(path, label=label)

        with (
            mock.patch.object(bundle, "check_verifier_contract", return_value=pending_contract),
            mock.patch.object(bundle, "_load_fixed_dependencies", return_value=dependencies),
            mock.patch.object(v1, "_open_regular_descriptor", side_effect=guarded_open),
            self.assertRaisesRegex(bundle.CacheBundleVerificationError, "pending"),
        ):
            bundle.verify_bundle_files(manifest_path, bundle_path)
        self.assertEqual(attempted, [])

    def test_bounded_json_rejects_oversize_duplicate_keys_and_floats(self) -> None:
        v1 = bundle._checked_v1()
        with tempfile.TemporaryDirectory() as temporary_name:
            root = pathlib.Path(temporary_name).resolve(strict=True)
            oversized = root / "oversized.json"
            oversized.write_bytes(b"{}")
            with self.assertRaises(v1.CacheArchiveVerificationError):
                v1._read_json(oversized, maximum=1, label="oversized JSON")
            duplicate = root / "duplicate.json"
            duplicate.write_bytes(b'{"value":1,"value":2}')
            with self.assertRaises(v1.CacheArchiveVerificationError):
                v1._read_json(duplicate, maximum=1024, label="duplicate JSON")
            floating = root / "floating.json"
            floating.write_bytes(b'{"value":1.5}')
            with self.assertRaises(v1.CacheArchiveVerificationError):
                v1._read_json(floating, maximum=1024, label="floating JSON")


class SyntheticApprovedVerificationTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory(
            prefix="negative-control-cache-bundle-approved-tests-"
        )
        self.root = pathlib.Path(self.temporary.name).resolve(strict=True)
        self.manifest, self.payloads, self.raw = _valid_manifest_and_bundle()
        (
            self.dependencies,
            self.contract,
            self.manifest_raw,
        ) = _approved_dependencies(self.manifest, self.raw)
        self.manifest_path = (
            self.root / "synthetic-offline-go-cache-manifest-v1.json"
        )
        self.bundle_path = self.root / self.manifest["archive"]["artifact_file"]
        self.manifest_path.write_bytes(self.manifest_raw)
        self.bundle_path.write_bytes(self.raw)

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def verify(
        self,
        *,
        dependencies: dict[str, Any] | None = None,
        contract: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        return bundle._verify_with_dependencies(
            self.manifest_path,
            self.bundle_path,
            dependencies=(self.dependencies if dependencies is None else dependencies),
            verifier_contract=(self.contract if contract is None else contract),
        )

    def test_deterministic_report_is_future_approved_schema_valid_and_nonauthorizing(self) -> None:
        first = self.verify()
        second = self.verify()
        self.assertEqual(first, second)
        self.assertEqual(first["status"], bundle.REPORT_STATUS)
        self.assertEqual(first["authority"], bundle.REPORT_AUTHORITY)
        self.assertEqual(first["safety"], bundle.REPORT_SAFETY)
        self.assertEqual(first["structure"], bundle.REPORT_STRUCTURE)
        self.assertFalse(first["authority"]["execution_authority"])
        self.assertFalse(first["authority"]["owner_approval"])
        self.assertFalse(first["authority"]["atomic_consumption"])
        self.assertFalse(first["safety"]["bundle_creation"])
        self.assertFalse(first["safety"]["extraction"])
        self.assertFalse(first["safety"]["staging"])
        self.assertFalse(first["safety"]["staged_tree_created"])
        self.assertFalse(first["safety"]["e0_runtime_binding"])
        self.assertTrue(first["contents"]["all_payload_sha256_verified"])
        self.assertTrue(first["contents"]["manifest_bijection"])
        self.assertEqual(
            first["contents"]["shared_seed_semantics"],
            "one_shared_union_seed_copied_unchanged_to_every_arm",
        )
        self.assertTrue(first["structure"]["exact_eof"])
        self.assertEqual(
            first["report_sha256"],
            self.dependencies["v1"]._field_self_hash(first, "report_sha256"),
        )
        self.dependencies["v1"]._validate_schema(
            first,
            self.dependencies["report_schema"],
            schema_name=bundle.REPORT_SCHEMA_PATH.name,
            label="future-approved synthetic bundle report",
        )

        pending = bundle._load_fixed_dependencies()
        bindings = first["bindings"]
        self.assertNotEqual(
            bindings["v1_material_artifact_sha256"],
            _sha256(pending["v1_dependencies"]["material_raw"]),
        )
        self.assertNotEqual(
            bindings["v1_material_declaration_sha256"],
            pending["v1_dependencies"]["material"]["material_declaration_sha256"],
        )
        self.assertNotEqual(
            bindings["v1_verifier_contract_artifact_sha256"],
            _sha256(pending["v1_verifier_contract_raw"]),
        )
        self.assertNotEqual(
            bindings["v1_verifier_contract_sha256"],
            pending["v1_verifier_contract"]["verifier_contract_sha256"],
        )

    def test_real_private_seam_runs_raw_pass_before_parse_on_one_descriptor(self) -> None:
        v1 = self.dependencies["v1"]
        original_open = v1._open_regular_descriptor
        original_raw = bundle._raw_identity_pass
        original_parse = bundle._parse_content_pass
        events: list[tuple[str, int]] = []
        bundle_opens = 0

        def tracked_open(path: pathlib.Path, *, label: str) -> Any:
            nonlocal bundle_opens
            opened = original_open(path, label=label)
            if path == self.bundle_path:
                bundle_opens += 1
            return opened

        def tracked_raw(descriptor: int, **kwargs: Any) -> Any:
            events.append(("raw", descriptor))
            return original_raw(descriptor, **kwargs)

        def tracked_parse(descriptor: int, **kwargs: Any) -> Any:
            events.append(("parse", descriptor))
            return original_parse(descriptor, **kwargs)

        with (
            mock.patch.object(v1, "_open_regular_descriptor", side_effect=tracked_open),
            mock.patch.object(bundle, "_raw_identity_pass", side_effect=tracked_raw),
            mock.patch.object(bundle, "_parse_content_pass", side_effect=tracked_parse),
        ):
            self.verify()
        self.assertEqual([name for name, _descriptor in events], ["raw", "parse"])
        self.assertEqual(events[0][1], events[1][1])
        self.assertEqual(bundle_opens, 1)

    def test_raw_identity_failure_prevents_any_parse(self) -> None:
        self.bundle_path.write_bytes(b"x" * len(self.raw))
        with (
            mock.patch.object(
                bundle,
                "_parse_content_pass",
                side_effect=AssertionError("parse ran before raw identity passed"),
            ) as parser,
            self.assertRaisesRegex(
                bundle.CacheBundleVerificationError, "raw identity differs"
            ),
        ):
            self.verify()
        parser.assert_not_called()

    def test_between_pass_bundle_mutation_is_rejected(self) -> None:
        original_parse = bundle._parse_content_pass

        def mutate_then_parse(descriptor: int, **kwargs: Any) -> Any:
            self.bundle_path.write_bytes(b"x" * len(self.raw))
            return original_parse(descriptor, **kwargs)

        with (
            mock.patch.object(bundle, "_parse_content_pass", side_effect=mutate_then_parse),
            self.assertRaises(bundle.CacheBundleVerificationError),
        ):
            self.verify()

    def test_manifest_self_inventory_count_and_plan_mismatches_fail_closed(self) -> None:
        cases: tuple[tuple[str, Callable[[dict[str, Any]], Any]], ...] = (
            (
                "self hash",
                lambda value: value.__setitem__(
                    "host_shared_cache_reuse", "not-forbidden"
                ),
            ),
            (
                "inventory",
                lambda value: (
                    value["contents"]["entries"][0].__setitem__("sha256", "a" * 64),
                    value.__setitem__(
                        "manifest_sha256", gate._self_hash(value, "manifest_sha256")
                    ),
                ),
            ),
            (
                "count",
                lambda value: (
                    value["contents"].__setitem__("file_count", 4),
                    value.__setitem__(
                        "manifest_sha256", gate._self_hash(value, "manifest_sha256")
                    ),
                ),
            ),
            (
                "plan",
                lambda value: (
                    value["run_plan"].__setitem__("plan_sha256", "a" * 64),
                    value.__setitem__(
                        "manifest_sha256", gate._self_hash(value, "manifest_sha256")
                    ),
                ),
            ),
        )
        for label, mutate in cases:
            changed = copy.deepcopy(self.manifest)
            mutate(changed)
            changed_raw = self.dependencies["v1"]._render(changed)
            self.manifest_path.write_bytes(changed_raw)
            with self.subTest(label=label), self.assertRaises(
                (
                    bundle.CacheBundleVerificationError,
                    self.dependencies["v1"].CacheArchiveVerificationError,
                )
            ):
                dependencies, contract, _canonical_raw = _approved_dependencies(
                    changed, self.raw
                )
                self.verify(dependencies=dependencies, contract=contract)
            self.manifest_path.write_bytes(self.manifest_raw)

    def test_both_locators_reject_symlink_hardlink_and_fifo(self) -> None:
        for label, path, raw in (
            ("manifest", self.manifest_path, self.manifest_raw),
            ("bundle", self.bundle_path, self.raw),
        ):
            target = self.root / f"{label}-target"
            path.rename(target)
            path.symlink_to(target)
            with self.subTest(label=label, kind="symlink"), self.assertRaises(
                bundle.CacheBundleVerificationError
            ):
                self.verify()
            path.unlink()

            os.link(target, path)
            with self.subTest(label=label, kind="hardlink"), self.assertRaises(
                bundle.CacheBundleVerificationError
            ):
                self.verify()
            path.unlink()
            target.unlink()

            os.mkfifo(path)
            with self.subTest(label=label, kind="fifo"), self.assertRaises(
                bundle.CacheBundleVerificationError
            ):
                self.verify()
            path.unlink()
            path.write_bytes(raw)


class SourceOnlyAndSurfaceTest(unittest.TestCase):
    def test_forged_timestamp_valid_v1_bytecode_is_ignored(self) -> None:
        with tempfile.TemporaryDirectory() as temporary_name:
            root = pathlib.Path(temporary_name).resolve(strict=True)
            source = root / "negative_control_cache_archive_v1.py"
            raw = bundle.V1_SOURCE_PATH.read_bytes()
            source.write_bytes(raw)
            fixed_mtime = 1_700_000_000
            os.utime(source, (fixed_mtime, fixed_mtime))
            metadata = source.stat()
            forged = compile(
                "MARKER = 'forged-timestamp-cache-loaded'\n",
                str(source),
                "exec",
            )
            bytecode = pathlib.Path(importlib.util.cache_from_source(str(source)))
            bytecode.parent.mkdir()
            bytecode.write_bytes(
                importlib.util.MAGIC_NUMBER
                + struct.pack("<III", 0, int(metadata.st_mtime), metadata.st_size)
                + marshal.dumps(forged)
            )
            control = subprocess.run(
                [
                    sys.executable,
                    "-c",
                    "import negative_control_cache_archive_v1 as m; print(m.MARKER)",
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
            self.assertEqual(control.stdout.strip(), "forged-timestamp-cache-loaded")
            with (
                mock.patch.object(bundle, "V1_SOURCE_PATH", source),
                mock.patch.object(bundle, "CHECKED_V1_SOURCE_SHA256", _sha256(raw)),
                mock.patch.object(bundle, "CHECKED_V1_SOURCE_SIZE", len(raw)),
                mock.patch.object(bundle, "_V1_MODULE", None),
            ):
                checked = bundle._checked_v1()
            self.assertFalse(hasattr(checked, "MARKER"))
            self.assertTrue(hasattr(checked, "_validate_manifest_identity"))

    def test_source_has_no_pack_extract_stage_network_or_process_surface(self) -> None:
        source = pathlib.Path(bundle.__file__).read_text(encoding="utf-8")
        tree = ast.parse(source)
        imported_roots: set[str] = set()
        called_names: set[str] = set()
        function_names: set[str] = set()
        for node in ast.walk(tree):
            if isinstance(node, ast.Import):
                imported_roots.update(alias.name.split(".", 1)[0] for alias in node.names)
            elif isinstance(node, ast.ImportFrom) and node.module:
                imported_roots.add(node.module.split(".", 1)[0])
            elif isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
                function_names.add(node.name)
            elif isinstance(node, ast.Call):
                if isinstance(node.func, ast.Name):
                    called_names.add(node.func.id)
                elif isinstance(node.func, ast.Attribute):
                    called_names.add(node.func.attr)
        self.assertTrue(
            imported_roots.isdisjoint(
                {
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
        self.assertTrue(
            all(
                token not in name.lower()
                for name in function_names
                for token in ("pack", "extract", "stage", "reserve", "execute")
            ),
            function_names,
        )


if __name__ == "__main__":
    unittest.main()
