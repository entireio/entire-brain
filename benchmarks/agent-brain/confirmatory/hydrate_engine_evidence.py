#!/usr/bin/env python3
"""Safely hydrate the pinned three-engine evidence archive.

All expected identities and sizes come from ``engine-evidence-storage.json``.
Callers may choose where the bytes come from, but cannot weaken those checks.
The destination is published only after the complete archive and extracted tree
have been validated.
"""

from __future__ import annotations

import argparse
import contextlib
import ctypes
import datetime as dt
import errno
import hashlib
import json
import ntpath
import os
import pathlib
import re
import shutil
import stat
import sys
import tarfile
import tempfile
import unicodedata
import urllib.parse
import urllib.request
from dataclasses import dataclass
from typing import Any, BinaryIO, Callable, Iterator, Mapping, Sequence


HERE = pathlib.Path(__file__).resolve().parent
REPO = HERE.parents[2]
DEFAULT_CONTRACT_PATH = HERE / "engine-evidence-storage.json"
LEGACY_CONTRACT_PATH = HERE / "engine-evidence-storage-legacy-v1.json"
COPY_CHUNK_BYTES = 1024 * 1024
DOWNLOAD_TIMEOUT_SECONDS = 60
RENAME_EXCL = 0x00000004  # Darwin renamex_np(2)
RENAME_NOREPLACE = 0x00000001  # Linux renameat2(2)
AT_FDCWD = -100
PUBLIC_ARCHIVE_ROOT = "public-v4-v1"
PUBLIC_MANIFEST_NAME = "engine-verification-public-v4.json"
PUBLIC_MANIFEST_PATH = pathlib.PurePosixPath(PUBLIC_ARCHIVE_ROOT, PUBLIC_MANIFEST_NAME)
RESTRICTED_ATTESTATION_PATH = pathlib.PurePosixPath("restricted", "replay-attestation-v1.json")
PUBLIC_TREE_INVENTORY_ALGORITHM = "sha256_ordered_type_nul_path_nul_mode_nul_sha256_nul_size_newline_v1"
PUBLIC_SOURCE_PATHS = (
    "arms/embeddinggemma_rrf.json",
    "arms/lexical_handrolled.json",
    "arms/model2vec_rrf.json",
    "attestation-sequence.json",
    "engine-verification-public-v4.json",
    "temporal-projection.json",
)
PUBLIC_ARCHIVE_PATHS = (
    PUBLIC_ARCHIVE_ROOT,
    f"{PUBLIC_ARCHIVE_ROOT}/arms",
    *(f"{PUBLIC_ARCHIVE_ROOT}/{path}" for path in PUBLIC_SOURCE_PATHS),
)
MAX_PUBLIC_ARCHIVE_BYTES = 64 * 1024 * 1024
MAX_PUBLIC_LOGICAL_BYTES = 64 * 1024 * 1024
MAX_RESTRICTED_ATTESTATION_BYTES = 1024 * 1024
MAX_JSON_DEPTH = 64
MAX_JSON_NODES = 100_000
STORAGE_V2_PROFILE = "public_v4_restricted_replay_v1"
STORAGE_HYDRATION_PARENT = "benchmarks/agent-brain/confirmatory/.engine-evidence"
LEGACY_DESCRIPTOR_REPO_PATH = "benchmarks/agent-brain/confirmatory/engine-evidence-storage-legacy-v1.json"
PIN_REPO_PATH = "benchmarks/agent-brain/confirmatory/engine-verification-pins.json"
MATRIX_REPO_PATH = "benchmarks/agent-brain/confirmatory/engine-matrix.json"
CHECKER_LOCK_REPO_PATH = "benchmarks/agent-brain/confirmatory/engine-replay-checker-lock.json"
ANALYZER_LOCK_REPO_PATH = "benchmarks/agent-brain/confirmatory/analyzer-lock.json"
PIN_SHA256 = "c4e4989ffe22cefa2e71211ba2baa4027634daf07d19ff5e92a8b1289243db11"
MATRIX_SHA256 = "664093683bfb170b704958a3623445f46e20ed897681848f9f19b80504990cf7"
ANALYZER_LOCK_SHA256 = "dfe21a14334104041df4b91341082eb3d848be0dc19634a21a4ee57b06f0ab31"
ANALYZER_AGGREGATE_SHA256 = "7c4e37fac7f9431a28c8fafc2f226e13e54b0be574776ddeb73c3c0083887032"
PRIVATE_MANIFEST_SHA256 = "b7c9baae2c3f0ed9b0773226ececf5bf129bd4dd487c02ffab9945cf0fc9c17d"
LEGACY_ARCHIVE_SHA256 = "9b893028ebcbbb6271a0179327cefc97062e25dfda4a84b5b062f010bb69d17a"
LEGACY_DESCRIPTOR_SHA256 = "9515a3d97a97f6a17ea3c3821f3d59f672822e21079253d5ad1b8fb7052f2190"
PIN_SET_ID = "facts-retrieval-primary-2026-07-15-darwin-arm64-v1"
RFC3339_RE = re.compile(
    r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,6})?(?:Z|\+\d{2}:\d{2}|-(?!00:00)\d{2}:\d{2})$"
)


class HydrationError(RuntimeError):
    """Raised when evidence hydration cannot be established safely."""


@dataclass(frozen=True)
class StorageContract:
    asset_size_bytes: int
    asset_sha256: str
    archive_format: str
    archive_root: str
    regular_file_count: int
    logical_bytes: int
    manifest_path: pathlib.PurePosixPath
    manifest_sha256: str
    repository: str
    release_tag: str
    asset_name: str
    asset_url: str
    publication_disposition: str
    privacy_review: str
    published: bool
    release_id: int | None
    asset_id: int | None
    release_immutable: bool
    release_target_commitish: str | None
    asset_api_digest: str | None
    verified_at: str | None
    repo_relative_parent: pathlib.PurePosixPath

    @property
    def release_ready(self) -> bool:
        return (
            self.publication_disposition == "approved"
            and self.privacy_review == "publishable"
            and self.published
            and self.release_immutable
            and _is_int(self.release_id)
            and self.release_id > 0
            and _is_int(self.asset_id)
            and self.asset_id > 0
            and isinstance(self.release_target_commitish, str)
            and len(self.release_target_commitish) == 40
            and self.asset_api_digest == f"sha256:{self.asset_sha256}"
            and isinstance(self.verified_at, str)
            and bool(self.verified_at.strip())
        )


@dataclass(frozen=True)
class ArchiveMember:
    name: str
    parts: tuple[str, ...]
    is_file: bool
    size: int
    executable: bool


@dataclass(frozen=True)
class ArchiveInventory:
    members: tuple[ArchiveMember, ...]
    regular_file_count: int
    logical_bytes: int


@dataclass(frozen=True)
class PublicTreeEntry:
    entry_type: str
    path: str
    mode: int
    sha256: str
    size: int


@dataclass(frozen=True)
class PublicPackage:
    archive_path: pathlib.Path
    archive_sha256: str
    archive_size_bytes: int
    regular_file_count: int
    directory_count: int
    logical_bytes: int
    tree_inventory_algorithm: str
    tree_inventory_sha256: str
    manifest_sha256: str
    manifest_size_bytes: int
    payload_file_count: int
    payload_logical_bytes: int


def _require(condition: bool, message: str) -> None:
    if not condition:
        raise HydrationError(message)


def _is_int(value: Any) -> bool:
    return type(value) is int


def _sha256(value: Any, field: str) -> str:
    _require(
        isinstance(value, str)
        and len(value) == 64
        and value == value.lower()
        and all(character in "0123456789abcdef" for character in value),
        f"{field} must be a lowercase SHA-256 hex digest",
    )
    return value


def _mapping(value: Any, field: str) -> Mapping[str, Any]:
    _require(isinstance(value, dict), f"{field} must be an object")
    return value


def _positive_int(value: Any, field: str) -> int:
    _require(_is_int(value) and value > 0, f"{field} must be a positive integer")
    return value


def _nonempty_string(value: Any, field: str) -> str:
    _require(isinstance(value, str) and bool(value.strip()), f"{field} must be a non-empty string")
    return value.strip()


def _safe_relative_path(value: Any, field: str) -> pathlib.PurePosixPath:
    text = _nonempty_string(value, field)
    _require("\\" not in text and not ntpath.splitdrive(text)[0], f"{field} must use a relative POSIX path")
    path = pathlib.PurePosixPath(text)
    _require(not path.is_absolute(), f"{field} must be relative")
    _require(
        all(part not in {"", ".", ".."} for part in path.parts),
        f"{field} must not contain empty, dot, or parent components",
    )
    return path


def _object_without_duplicate_keys(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise HydrationError(f"storage contract contains duplicate key {key!r}")
        result[key] = value
    return result


def load_contract(path: pathlib.Path) -> StorageContract:
    """Load the fail-closed subset needed to authenticate and place the asset."""

    try:
        metadata = path.lstat()
    except OSError as exc:
        raise HydrationError(f"storage contract cannot be opened: {exc}") from exc
    _require(stat.S_ISREG(metadata.st_mode) and not path.is_symlink(), "storage contract must be one regular file")
    try:
        raw = json.loads(path.read_text(encoding="utf-8"), object_pairs_hook=_object_without_duplicate_keys)
    except HydrationError:
        raise
    except (OSError, UnicodeDecodeError, ValueError, RecursionError) as exc:
        raise HydrationError(f"storage contract is not valid UTF-8 JSON: {exc}") from exc

    contract = _mapping(raw, "storage contract")
    _require(contract.get("schema_version") == 1, "unsupported engine evidence storage contract schema")
    storage = _mapping(contract.get("storage"), "storage")
    archive = _mapping(contract.get("archive"), "archive")
    evidence = _mapping(contract.get("evidence"), "evidence")
    hydration = _mapping(contract.get("hydration"), "hydration")

    _require(
        storage.get("kind") == "github_immutable_release_asset",
        "storage.kind must be github_immutable_release_asset",
    )
    repository = _nonempty_string(storage.get("repository"), "storage.repository")
    repository_parts = repository.split("/")
    _require(
        len(repository_parts) == 2 and all(part not in {"", ".", ".."} for part in repository_parts),
        "storage.repository must be an owner/repository pair",
    )
    release_tag = _nonempty_string(storage.get("tag"), "storage.tag")
    asset_name = _nonempty_string(storage.get("asset_name"), "storage.asset_name")
    _require(
        asset_name == pathlib.PurePosixPath(asset_name).name and "\\" not in asset_name,
        "storage.asset_name must be a basename",
    )
    asset_url = _nonempty_string(storage.get("asset_url"), "storage.asset_url")
    _validate_download_url(asset_url)
    publication_disposition = storage.get("publication_disposition")
    privacy_review = storage.get("privacy_review")
    _require(
        publication_disposition in {"regeneration_required", "approved"},
        "storage.publication_disposition is invalid",
    )
    _require(privacy_review in {"fail", "publishable"}, "storage.privacy_review is invalid")
    published = storage.get("published")
    release_immutable = storage.get("release_immutable")
    _require(type(published) is bool, "storage.published must be a boolean")
    _require(type(release_immutable) is bool, "storage.release_immutable must be a boolean")
    release_id = storage.get("release_id")
    asset_id = storage.get("asset_id")
    _require(release_id is None or (_is_int(release_id) and release_id > 0), "storage.release_id is invalid")
    _require(asset_id is None or (_is_int(asset_id) and asset_id > 0), "storage.asset_id is invalid")
    release_target_commitish = storage.get("release_target_commitish")
    _require(
        release_target_commitish is None
        or (
            isinstance(release_target_commitish, str)
            and len(release_target_commitish) == 40
            and all(character in "0123456789abcdef" for character in release_target_commitish)
        ),
        "storage.release_target_commitish is invalid",
    )
    asset_api_digest = storage.get("asset_api_digest")
    _require(
        asset_api_digest is None
        or (
            isinstance(asset_api_digest, str)
            and asset_api_digest.startswith("sha256:")
            and len(asset_api_digest) == 71
            and all(character in "0123456789abcdef" for character in asset_api_digest[7:])
        ),
        "storage.asset_api_digest is invalid",
    )
    verified_at = storage.get("verified_at")
    _require(
        verified_at is None or (isinstance(verified_at, str) and bool(verified_at.strip())),
        "storage.verified_at is invalid",
    )

    asset_size_bytes = _positive_int(storage.get("asset_size_bytes"), "storage.asset_size_bytes")
    asset_sha256 = _sha256(storage.get("asset_sha256"), "storage.asset_sha256")
    _require(archive.get("format") == "tar_zstd", "archive.format must be tar_zstd")
    archive_root = _nonempty_string(archive.get("root"), "archive.root")
    _require(
        archive_root == pathlib.PurePosixPath(archive_root).name
        and archive_root not in {".", ".."}
        and "\\" not in archive_root,
        "archive.root must be one safe path component",
    )
    regular_file_count = _positive_int(archive.get("regular_file_count"), "archive.regular_file_count")
    logical_bytes = _positive_int(archive.get("logical_bytes"), "archive.logical_bytes")
    _require(archive.get("symlink_count") == 0, "archive.symlink_count must be zero")

    manifest_path = _safe_relative_path(evidence.get("manifest_path"), "evidence.manifest_path")
    _require(
        manifest_path.parts[0] == archive_root,
        "evidence.manifest_path must be inside archive.root",
    )
    manifest_sha256 = _sha256(evidence.get("manifest_sha256"), "evidence.manifest_sha256")
    _nonempty_string(evidence.get("recorded_artifact_root"), "evidence.recorded_artifact_root")
    repo_relative_parent = _safe_relative_path(
        hydration.get("repo_relative_parent"), "hydration.repo_relative_parent"
    )

    loaded = StorageContract(
        asset_size_bytes=asset_size_bytes,
        asset_sha256=asset_sha256,
        archive_format="tar_zstd",
        archive_root=archive_root,
        regular_file_count=regular_file_count,
        logical_bytes=logical_bytes,
        manifest_path=manifest_path,
        manifest_sha256=manifest_sha256,
        repository=repository,
        release_tag=release_tag,
        asset_name=asset_name,
        asset_url=asset_url,
        publication_disposition=publication_disposition,
        privacy_review=privacy_review,
        published=published,
        release_id=release_id,
        asset_id=asset_id,
        release_immutable=release_immutable,
        release_target_commitish=release_target_commitish,
        asset_api_digest=asset_api_digest,
        verified_at=verified_at,
        repo_relative_parent=repo_relative_parent,
    )
    _require(asset_url == _derived_release_asset_url(loaded), "storage.asset_url does not match repository, tag, and asset")
    return loaded


def _exact_keys(value: Any, fields: set[str], label: str) -> Mapping[str, Any]:
    _require(isinstance(value, dict), f"{label} must be an object")
    _require(set(value) == fields, f"{label} must contain exactly {', '.join(sorted(fields))}")
    return value


def _exact_integer(value: Any, expected: int, label: str) -> int:
    _require(type(value) is int and value == expected, f"{label} must equal integer {expected}")
    return value


def _sha1(value: Any, field: str) -> str:
    _require(
        isinstance(value, str)
        and len(value) == 40
        and value == value.lower()
        and all(character in "0123456789abcdef" for character in value),
        f"{field} must be a lowercase SHA-1 hex digest",
    )
    return value


def _rfc3339(value: Any, field: str) -> dt.datetime:
    _require(isinstance(value, str) and RFC3339_RE.fullmatch(value) is not None, f"{field} must be RFC3339")
    try:
        parsed = dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError as exc:
        raise HydrationError(f"{field} must be RFC3339") from exc
    _require(parsed.tzinfo is not None, f"{field} must include a timezone")
    return parsed.astimezone(dt.UTC)


def _no_nulls(value: Any, label: str) -> None:
    if value is None:
        raise HydrationError(f"approved storage contract contains null at {label}")
    if isinstance(value, dict):
        for key, child in value.items():
            _no_nulls(child, f"{label}.{key}")
    elif isinstance(value, list):
        for index, child in enumerate(value):
            _no_nulls(child, f"{label}[{index}]")


def _approved_identifier(value: Any, field: str) -> str:
    text = _nonempty_string(value, field)
    lowered = text.casefold()
    _require(
        not any(
            marker in lowered
            for marker in (
                "://", "credential", "password", "secret", "private_key", "public_key",
                "ssh-ed25519", "begin ssh", "query_text", "query=", "prompt=", "token=",
            )
        )
        and not any(character in text for character in ("\x00", "\r", "\n")),
        f"{field} contains prohibited URL, credential, query, or key material",
    )
    return text


def _read_dependency_bytes(path: pathlib.Path, label: str, *, maximum: int = 16 * 1024 * 1024) -> bytes:
    try:
        metadata = path.lstat()
    except OSError as exc:
        raise HydrationError(f"{label} cannot be opened: {exc}") from exc
    _require(stat.S_ISREG(metadata.st_mode) and not path.is_symlink(), f"{label} must be one regular file")
    _require(metadata.st_size <= maximum, f"{label} exceeds the size bound")
    flags = os.O_RDONLY
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    try:
        descriptor = os.open(path, flags)
        try:
            opened = os.fstat(descriptor)
            _require(
                opened.st_dev == metadata.st_dev
                and opened.st_ino == metadata.st_ino
                and stat.S_ISREG(opened.st_mode)
                and opened.st_size == metadata.st_size,
                f"{label} changed while opening",
            )
            raw = os.read(descriptor, metadata.st_size + 1)
            _require(len(raw) == metadata.st_size and os.read(descriptor, 1) == b"", f"{label} changed while reading")
        finally:
            os.close(descriptor)
    except HydrationError:
        raise
    except OSError as exc:
        raise HydrationError(f"{label} cannot be read safely: {exc}") from exc
    return raw


def _decode_storage_json(raw: bytes, label: str) -> dict[str, Any]:
    try:
        value = json.loads(raw.decode("utf-8"), object_pairs_hook=_object_without_duplicate_keys)
    except HydrationError:
        raise
    except (UnicodeDecodeError, ValueError, RecursionError) as exc:
        raise HydrationError(f"{label} is not valid UTF-8 JSON: {exc}") from exc
    _require(isinstance(value, dict), "storage v2 contract must be an object")
    stack: list[tuple[Any, int]] = [(value, 0)]
    nodes = 0
    while stack:
        current, depth = stack.pop()
        nodes += 1
        _require(depth <= MAX_JSON_DEPTH, f"{label} exceeds the JSON nesting bound")
        _require(nodes <= MAX_JSON_NODES, f"{label} exceeds the JSON node bound")
        if isinstance(current, dict):
            stack.extend((child, depth + 1) for child in current.values())
        elif isinstance(current, list):
            stack.extend((child, depth + 1) for child in current)
    return value


def _load_storage_json(path: pathlib.Path) -> dict[str, Any]:
    return _decode_storage_json(_read_dependency_bytes(path, "JSON dependency"), "JSON dependency")


def load_storage_contract_v2(
    path: pathlib.Path = DEFAULT_CONTRACT_PATH,
    *,
    repo: pathlib.Path = REPO,
    verify_external_locks: bool = True,
) -> dict[str, Any]:
    """Load and semantically close the pending or approved canonical v2 contract."""
    contract = _decode_storage_json(
        _read_dependency_bytes(path, "storage v2 contract", maximum=1024 * 1024),
        "storage v2 contract",
    )
    _exact_keys(
        contract,
        {"schema_version", "profile", "contract_status", "public", "restricted", "expected_bindings", "hydration", "legacy_v1"},
        "storage v2 contract",
    )
    _exact_integer(contract["schema_version"], 2, "storage v2 schema version")
    _require(contract["profile"] == STORAGE_V2_PROFILE, "storage v2 profile changed")
    status = contract["contract_status"]
    _require(status in {"pending_owner_authorization", "approved"}, "storage v2 status is invalid")

    public = _exact_keys(contract["public"], {"storage", "archive", "evidence"}, "storage v2 public")
    storage = _exact_keys(
        public["storage"],
        {
            "kind", "repository", "tag", "asset_name", "asset_url", "asset_size_bytes", "asset_sha256",
            "publication_disposition", "privacy_review", "published", "release_id", "asset_id",
            "release_immutable", "release_target_commitish", "asset_api_digest", "verified_at",
        },
        "storage v2 public.storage",
    )
    archive = _exact_keys(
        public["archive"],
        {
            "format", "root", "regular_file_count", "directory_count", "logical_bytes", "symlink_count",
            "hardlink_count", "special_file_count", "tree_inventory_algorithm", "tree_inventory_sha256",
        },
        "storage v2 public.archive",
    )
    evidence = _exact_keys(
        public["evidence"],
        {
            "manifest_path", "manifest_schema_version", "manifest_sha256", "manifest_size_bytes",
            "artifact_inventory_algorithm", "artifact_inventory_root_sha256", "payload_file_count",
            "payload_logical_bytes",
        },
        "storage v2 public.evidence",
    )
    _require(storage["kind"] == "github_immutable_release_asset" and storage["repository"] == "entireio/entire-brain", "public storage identity changed")
    _require(archive["format"] == "tar_zstd" and archive["root"] == PUBLIC_ARCHIVE_ROOT, "public archive identity changed")
    _exact_integer(archive["regular_file_count"], 6, "public archive regular-file count")
    _exact_integer(archive["directory_count"], 2, "public archive directory count")
    for field in ("symlink_count", "hardlink_count", "special_file_count"):
        _exact_integer(archive[field], 0, f"public archive {field}")
    _require(archive["tree_inventory_algorithm"] == PUBLIC_TREE_INVENTORY_ALGORITHM, "public tree inventory algorithm changed")
    _require(evidence["manifest_path"] == str(PUBLIC_MANIFEST_PATH), "public manifest path changed")
    _exact_integer(evidence["manifest_schema_version"], 4, "public manifest schema version")
    _require(evidence["artifact_inventory_algorithm"] == "sha256_ordered_relative_path_nul_sha256_nul_size_newline_v1", "public artifact inventory algorithm changed")
    _exact_integer(evidence["payload_file_count"], 5, "public payload file count")

    restricted = _exact_keys(contract["restricted"], {"storage", "attestation"}, "storage v2 restricted")
    restricted_storage = _exact_keys(
        restricted["storage"],
        {"kind", "provider_id", "object_id", "version_id", "retention_status", "access_control_status", "immutable", "verified_at"},
        "storage v2 restricted.storage",
    )
    attestation = _exact_keys(
        restricted["attestation"],
        {
            "relative_path", "schema_version", "profile", "signature_scheme", "signature_namespace",
            "sha256", "size_bytes", "signed_payload_sha256", "trust_root_id", "signer_principal",
            "public_key_sha256",
        },
        "storage v2 restricted.attestation",
    )
    _require(restricted_storage["kind"] == "owner_managed_restricted_object", "restricted storage kind changed")
    _require(attestation["relative_path"] == str(RESTRICTED_ATTESTATION_PATH), "restricted attestation path changed")
    _exact_integer(attestation["schema_version"], 1, "restricted attestation schema version")
    _require(attestation["profile"] == "restricted_engine_replay_attestation_v1", "restricted attestation profile changed")
    _require(attestation["signature_scheme"] == "sshsig_ed25519_v1", "restricted attestation signature scheme changed")
    _require(attestation["signature_namespace"] == "entire-brain-engine-replay-v1", "restricted attestation namespace changed")

    bindings = _exact_keys(
        contract["expected_bindings"],
        {"issued_at", "private_diagnostic", "public_projection", "production_inputs", "source_identity", "replay_result", "public_archive"},
        "storage v2 expected_bindings",
    )
    private = _exact_keys(bindings["private_diagnostic"], {"manifest_schema_version", "manifest_sha256", "manifest_size_bytes"}, "expected private diagnostic")
    _exact_integer(private["manifest_schema_version"], 2, "expected private manifest schema")
    _require(private["manifest_sha256"] == PRIVATE_MANIFEST_SHA256, "private manifest commitment changed")
    projection = _exact_keys(
        bindings["public_projection"],
        {"manifest_schema_version", "manifest_sha256", "manifest_size_bytes", "artifact_inventory_algorithm", "artifact_inventory_root_sha256", "payload_file_count", "payload_logical_bytes"},
        "expected public projection",
    )
    _exact_integer(projection["manifest_schema_version"], 4, "expected public projection schema")
    _require(projection["artifact_inventory_algorithm"] == evidence["artifact_inventory_algorithm"], "expected public inventory algorithm differs")
    _exact_integer(projection["payload_file_count"], 5, "expected public payload count")
    inputs = _exact_keys(
        bindings["production_inputs"],
        {"pin_set_id", "pin_set_authority", "pin_set_path", "pin_set_sha256", "matrix_path", "matrix_sha256"},
        "expected production inputs",
    )
    _require(
        inputs == {
            "pin_set_id": PIN_SET_ID,
            "pin_set_authority": "production",
            "pin_set_path": PIN_REPO_PATH,
            "pin_set_sha256": PIN_SHA256,
            "matrix_path": MATRIX_REPO_PATH,
            "matrix_sha256": MATRIX_SHA256,
        },
        "production input bindings changed",
    )
    source = _exact_keys(
        bindings["source_identity"],
        {
            "repository", "git_object_format", "commit_oid", "tree_oid", "worktree_state", "checker_lock_path",
            "checker_lock_sha256", "checker_lock_algorithm", "checker_source_aggregate_sha256",
            "analyzer_lock_path", "analyzer_lock_sha256", "analyzer_lock_algorithm",
            "analyzer_source_aggregate_sha256",
        },
        "expected source identity",
    )
    _require(source["repository"] == "entireio/entire-brain" and source["git_object_format"] == "sha1", "source repository identity changed")
    _require(source["checker_lock_path"] == CHECKER_LOCK_REPO_PATH, "checker lock path changed")
    _sha256(source["checker_lock_sha256"], "checker lock SHA-256")
    _require(source["checker_lock_algorithm"] == "sha256_ordered_path_nul_sha256_newline_v1", "checker lock algorithm changed")
    _sha256(source["checker_source_aggregate_sha256"], "checker source aggregate")
    _require(source["analyzer_lock_path"] == ANALYZER_LOCK_REPO_PATH, "analyzer lock path changed")
    _require(source["analyzer_lock_sha256"] == ANALYZER_LOCK_SHA256, "analyzer lock raw hash changed")
    _require(source["analyzer_lock_algorithm"] == "sha256_ordered_path_nul_sha256_newline_v1", "analyzer lock algorithm changed")
    _require(source["analyzer_source_aggregate_sha256"] == ANALYZER_AGGREGATE_SHA256, "analyzer source aggregate changed")
    replay = _exact_keys(
        bindings["replay_result"],
        {"operation", "private_validator", "private_error_count", "public_validator", "public_error_count", "checker_lock_valid", "analyzer_lock_valid", "decision"},
        "expected replay result",
    )
    _require(replay["operation"] == "offline_exact_byte_validation_v1", "replay operation changed")
    _require(replay["private_validator"] == "check_protocol.validate_engine_verification/v2", "private replay validator changed")
    _require(replay["public_validator"] == "public_engine_evidence.validate_public_bundle/v4", "public replay validator changed")
    expected_archive = _exact_keys(
        bindings["public_archive"],
        {
            "format", "root", "sha256", "size_bytes", "regular_file_count", "directory_count", "logical_bytes",
            "tree_inventory_algorithm", "tree_inventory_sha256", "repository", "tag", "asset_name", "asset_url",
            "release_id", "asset_id", "release_immutable", "release_target_commitish", "asset_api_digest", "verified_at",
        },
        "expected public archive",
    )
    _require(expected_archive["format"] == "tar_zstd" and expected_archive["root"] == PUBLIC_ARCHIVE_ROOT, "expected public archive identity changed")
    _exact_integer(expected_archive["regular_file_count"], 6, "expected public regular-file count")
    _exact_integer(expected_archive["directory_count"], 2, "expected public directory count")
    _require(expected_archive["tree_inventory_algorithm"] == PUBLIC_TREE_INVENTORY_ALGORITHM, "expected tree inventory algorithm changed")
    _require(expected_archive["repository"] == "entireio/entire-brain", "expected public archive repository changed")

    hydration = _exact_keys(
        contract["hydration"],
        {"repo_relative_parent", "public_archive_root", "public_manifest_path", "restricted_attestation_path"},
        "storage v2 hydration",
    )
    _require(
        hydration == {
            "repo_relative_parent": STORAGE_HYDRATION_PARENT,
            "public_archive_root": PUBLIC_ARCHIVE_ROOT,
            "public_manifest_path": str(PUBLIC_MANIFEST_PATH),
            "restricted_attestation_path": str(RESTRICTED_ATTESTATION_PATH),
        },
        "storage v2 hydration paths changed",
    )
    legacy = _exact_keys(contract["legacy_v1"], {"descriptor_path", "disposition", "archive_sha256", "private_manifest_sha256"}, "storage v2 legacy_v1")
    _require(
        legacy == {
            "descriptor_path": LEGACY_DESCRIPTOR_REPO_PATH,
            "disposition": "diagnostic_privacy_failed_not_publishable",
            "archive_sha256": LEGACY_ARCHIVE_SHA256,
            "private_manifest_sha256": PRIVATE_MANIFEST_SHA256,
        },
        "legacy diagnostic binding changed",
    )

    if verify_external_locks:
        import restricted_replay_attestation

        try:
            pin_raw = _read_dependency_bytes(repo / PIN_REPO_PATH, "canonical engine pins")
            matrix_raw = _read_dependency_bytes(repo / MATRIX_REPO_PATH, "canonical engine matrix")
            analyzer_raw = _read_dependency_bytes(repo / ANALYZER_LOCK_REPO_PATH, "canonical analyzer lock")
            checker_raw = _read_dependency_bytes(repo / CHECKER_LOCK_REPO_PATH, "canonical checker lock")
            trust_path = repo / "benchmarks/agent-brain/confirmatory/engine-replay-trust-roots.json"
            trust_raw = _read_dependency_bytes(trust_path, "canonical engine replay trust roots")
            legacy_raw = _read_dependency_bytes(repo / LEGACY_DESCRIPTOR_REPO_PATH, "legacy diagnostic descriptor")
            _require(hashlib.sha256(pin_raw).hexdigest() == PIN_SHA256, "canonical engine pins raw hash changed")
            _require(hashlib.sha256(matrix_raw).hexdigest() == MATRIX_SHA256, "canonical engine matrix raw hash changed")
            _require(hashlib.sha256(analyzer_raw).hexdigest() == ANALYZER_LOCK_SHA256, "canonical analyzer lock raw hash changed")
            analyzer = _decode_storage_json(analyzer_raw, "canonical analyzer lock")
            _require(analyzer.get("algorithm") == source["analyzer_lock_algorithm"], "analyzer lock algorithm differs from binding")
            _require(analyzer.get("aggregate_sha256") == source["analyzer_source_aggregate_sha256"], "analyzer lock aggregate differs from binding")
            checker = restricted_replay_attestation.load_checker_lock_bytes(checker_raw, repo=repo)
            _require(hashlib.sha256(checker_raw).hexdigest() == source["checker_lock_sha256"], "checker lock raw hash differs from binding")
            _require(checker["aggregate_sha256"] == source["checker_source_aggregate_sha256"], "checker lock aggregate differs from binding")
            _require(hashlib.sha256(trust_raw).hexdigest() == restricted_replay_attestation.TRUST_ROOTS_SHA256, "engine replay trust roots raw hash changed")
            restricted_replay_attestation.load_trust_roots_bytes(trust_raw)
            _require(hashlib.sha256(legacy_raw).hexdigest() == LEGACY_DESCRIPTOR_SHA256, "legacy diagnostic descriptor raw hash changed")
        except restricted_replay_attestation.AttestationError as exc:
            raise HydrationError(f"engine replay trust/lock validation failed: {exc}") from exc
        except OSError as exc:
            raise HydrationError(f"canonical storage dependency cannot be read: {exc}") from exc

    if status == "pending_owner_authorization":
        _require(
            all(storage[field] is None for field in ("tag", "asset_name", "asset_url", "asset_size_bytes", "asset_sha256", "release_id", "asset_id", "release_target_commitish", "asset_api_digest", "verified_at")),
            "pending public storage owner fields must be null",
        )
        _require(storage["publication_disposition"] == storage["privacy_review"] == "pending_owner_authorization", "pending public storage dispositions changed")
        _require(storage["published"] is False and storage["release_immutable"] is False, "pending public release booleans must be false")
        _require(archive["logical_bytes"] is None and archive["tree_inventory_sha256"] is None, "pending public archive fields must be null")
        _require(all(evidence[field] is None for field in ("manifest_sha256", "manifest_size_bytes", "artifact_inventory_root_sha256", "payload_logical_bytes")), "pending public evidence fields must be null")
        _require(all(restricted_storage[field] is None for field in ("provider_id", "object_id", "version_id", "verified_at")), "pending restricted owner fields must be null")
        _require(restricted_storage["retention_status"] == restricted_storage["access_control_status"] == "pending_owner_authorization", "pending restricted statuses changed")
        _require(restricted_storage["immutable"] is False, "pending restricted immutable must be false")
        _require(all(attestation[field] is None for field in ("sha256", "size_bytes", "signed_payload_sha256", "trust_root_id", "signer_principal", "public_key_sha256")), "pending attestation fields must be null")
        _require(bindings["issued_at"] is None and private["manifest_size_bytes"] is None, "pending replay issuance fields must be null")
        _require(all(projection[field] is None for field in ("manifest_sha256", "manifest_size_bytes", "artifact_inventory_root_sha256", "payload_logical_bytes")), "pending projection fields must be null")
        _require(source["commit_oid"] is None and source["tree_oid"] is None and source["worktree_state"] is None, "pending source identity owner fields must be null")
        _require(all(replay[field] is None for field in ("private_error_count", "public_error_count", "checker_lock_valid", "analyzer_lock_valid")) and replay["decision"] == "pending", "pending replay result is not exact")
        _require(all(expected_archive[field] is None for field in ("sha256", "size_bytes", "logical_bytes", "tree_inventory_sha256", "tag", "asset_name", "asset_url", "release_id", "asset_id", "release_immutable", "release_target_commitish", "asset_api_digest", "verified_at")), "pending expected public archive fields must be null")
        return contract

    _no_nulls(public, "public")
    _no_nulls(restricted, "restricted")
    _no_nulls(bindings, "expected_bindings")
    _require(storage["publication_disposition"] == "approved" and storage["privacy_review"] == "publishable", "approved public release dispositions are invalid")
    _require(storage["published"] is True and storage["release_immutable"] is True, "approved public release booleans are invalid")
    for field in ("asset_size_bytes", "release_id", "asset_id"):
        _positive_int(storage[field], f"approved public storage {field}")
    asset_sha = _sha256(storage["asset_sha256"], "approved public archive hash")
    tag = _nonempty_string(storage["tag"], "approved release tag")
    asset_name = _nonempty_string(storage["asset_name"], "approved asset name")
    _require("/" not in tag and "/" not in asset_name and asset_name.endswith(".tar.zst"), "approved release names are unsafe")
    canonical_url = f"https://github.com/entireio/entire-brain/releases/download/{tag}/{asset_name}"
    _require(storage["asset_url"] == canonical_url, "approved release URL is not canonical")
    target_commit = _sha1(storage["release_target_commitish"], "approved release target")
    _require(storage["asset_api_digest"] == f"sha256:{asset_sha}", "approved release API digest differs")
    public_verified_at = _rfc3339(storage["verified_at"], "approved public release verified_at")
    _positive_int(archive["logical_bytes"], "approved public archive logical bytes")
    _sha256(archive["tree_inventory_sha256"], "approved tree inventory hash")
    for field in ("manifest_size_bytes", "payload_logical_bytes"):
        _positive_int(evidence[field], f"approved public evidence {field}")
    for field in ("manifest_sha256", "artifact_inventory_root_sha256"):
        _sha256(evidence[field], f"approved public evidence {field}")
    _require(archive["logical_bytes"] == evidence["manifest_size_bytes"] + evidence["payload_logical_bytes"], "public archive logical bytes do not equal manifest plus payload bytes")
    for field in ("provider_id", "object_id", "version_id"):
        _approved_identifier(restricted_storage[field], f"approved restricted storage {field}")
    _require(restricted_storage["retention_status"] == restricted_storage["access_control_status"] == "approved", "approved restricted statuses are invalid")
    _require(restricted_storage["immutable"] is True, "approved restricted storage is not immutable")
    restricted_verified_at = _rfc3339(restricted_storage["verified_at"], "approved restricted verified_at")
    import restricted_replay_attestation

    try:
        restricted_replay_attestation.validate_statement(bindings)
    except restricted_replay_attestation.AttestationError as exc:
        raise HydrationError(f"approved expected bindings are invalid: {exc}") from exc
    issued_at = _rfc3339(bindings["issued_at"], "approved attestation issued_at")
    _require(issued_at >= public_verified_at and restricted_verified_at >= issued_at, "approved verification timestamps are misordered")
    _require(source["commit_oid"] == target_commit, "approved source commit differs from release target")
    _require(
        storage["asset_sha256"] == expected_archive["sha256"]
        and storage["asset_size_bytes"] == expected_archive["size_bytes"]
        and all(storage[field] == expected_archive[field] for field in ("tag", "asset_name", "asset_url", "release_id", "asset_id", "release_immutable", "release_target_commitish", "asset_api_digest", "verified_at")),
        "approved public release fields differ from signed bindings",
    )
    _require(
        archive["regular_file_count"] == expected_archive["regular_file_count"]
        and archive["directory_count"] == expected_archive["directory_count"]
        and archive["logical_bytes"] == expected_archive["logical_bytes"]
        and archive["tree_inventory_sha256"] == expected_archive["tree_inventory_sha256"],
        "approved public archive contract differs from signed bindings",
    )
    _require(
        evidence["manifest_schema_version"] == projection["manifest_schema_version"]
        and evidence["manifest_sha256"] == projection["manifest_sha256"]
        and evidence["manifest_size_bytes"] == projection["manifest_size_bytes"]
        and evidence["artifact_inventory_algorithm"] == projection["artifact_inventory_algorithm"]
        and evidence["artifact_inventory_root_sha256"] == projection["artifact_inventory_root_sha256"]
        and evidence["payload_file_count"] == projection["payload_file_count"]
        and evidence["payload_logical_bytes"] == projection["payload_logical_bytes"],
        "approved public evidence differs from signed projection bindings",
    )
    for field in ("sha256", "signed_payload_sha256", "public_key_sha256"):
        _sha256(attestation[field], f"approved restricted attestation {field}")
    _positive_int(attestation["size_bytes"], "approved restricted attestation size")
    restricted_replay_attestation.validate_identifier(
        attestation["trust_root_id"],
        "approved restricted attestation trust_root_id",
    )
    restricted_replay_attestation.validate_principal(
        attestation["signer_principal"],
        "approved restricted attestation signer_principal",
    )
    return contract


def sha256_file(path: pathlib.Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(COPY_CHUNK_BYTES), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _copy_authenticated_stream(
    source: BinaryIO,
    destination: pathlib.Path,
    *,
    expected_size: int,
    expected_sha256: str,
) -> None:
    digest = hashlib.sha256()
    written = 0
    try:
        with destination.open("xb") as output:
            while True:
                chunk = source.read(COPY_CHUNK_BYTES)
                if not chunk:
                    break
                _require(isinstance(chunk, bytes), "archive source returned non-byte data")
                written += len(chunk)
                _require(written <= expected_size, "archive exceeds storage.asset_size_bytes")
                output.write(chunk)
                digest.update(chunk)
            output.flush()
            os.fsync(output.fileno())
    except HydrationError:
        raise
    except OSError as exc:
        raise HydrationError(f"archive could not be staged: {exc}") from exc
    _require(written == expected_size, f"archive size is {written}, expected {expected_size}")
    _require(digest.hexdigest() == expected_sha256, "archive SHA-256 does not match storage contract")


@contextlib.contextmanager
def _open_local_archive(
    path: pathlib.Path,
    *,
    expected_metadata: os.stat_result | None = None,
) -> Iterator[BinaryIO]:
    flags = os.O_RDONLY
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    try:
        descriptor = os.open(path, flags)
    except OSError as exc:
        raise HydrationError(f"local archive cannot be opened safely: {exc}") from exc
    try:
        metadata = os.fstat(descriptor)
        _require(stat.S_ISREG(metadata.st_mode), "local archive must be a regular file")
        if expected_metadata is not None:
            _require(
                metadata.st_dev == expected_metadata.st_dev
                and metadata.st_ino == expected_metadata.st_ino
                and metadata.st_uid == expected_metadata.st_uid
                and stat.S_IMODE(metadata.st_mode) == stat.S_IMODE(expected_metadata.st_mode)
                and metadata.st_size == expected_metadata.st_size,
                "local archive identity or custody changed while opening",
            )
        with os.fdopen(descriptor, "rb", closefd=False) as handle:
            yield handle
    finally:
        os.close(descriptor)


def _derived_release_asset_url(contract: StorageContract) -> str:
    owner, repository = contract.repository.split("/", 1)
    return (
        f"https://github.com/{urllib.parse.quote(owner, safe='')}/"
        f"{urllib.parse.quote(repository, safe='')}/releases/download/"
        f"{urllib.parse.quote(contract.release_tag, safe='')}/"
        f"{urllib.parse.quote(contract.asset_name, safe='')}"
    )


def release_asset_url(contract: StorageContract) -> str:
    _require(contract.release_ready, "contracted release asset is not published and immutably attested")
    return contract.asset_url


def _validate_download_url(url: str) -> None:
    parsed = urllib.parse.urlparse(url)
    _require(
        parsed.scheme == "https" and bool(parsed.netloc) and parsed.username is None and parsed.password is None,
        "download URL must be HTTPS and must not contain credentials",
    )


def _download_archive(
    url: str,
    destination: pathlib.Path,
    contract: StorageContract,
    *,
    urlopen: Callable[..., Any] = urllib.request.urlopen,
) -> None:
    _validate_download_url(url)
    request = urllib.request.Request(
        url,
        headers={"Accept": "application/octet-stream", "User-Agent": "entire-brain-evidence-hydrator/1"},
    )
    try:
        response_context = urlopen(request, timeout=DOWNLOAD_TIMEOUT_SECONDS)
        with response_context as response:
            final_url = response.geturl()
            _validate_download_url(final_url)
            content_length = response.headers.get("Content-Length")
            if content_length is not None:
                try:
                    parsed_length = int(content_length)
                except ValueError as exc:
                    raise HydrationError("download Content-Length is not an integer") from exc
                _require(
                    parsed_length == contract.asset_size_bytes,
                    "download Content-Length does not match storage.asset_size_bytes",
                )
            _copy_authenticated_stream(
                response,
                destination,
                expected_size=contract.asset_size_bytes,
                expected_sha256=contract.asset_sha256,
            )
    except HydrationError:
        raise
    except Exception as exc:
        raise HydrationError(f"archive download failed: {exc}") from exc


def _member_parts(name: str) -> tuple[str, ...]:
    _require(isinstance(name, str) and bool(name), "archive member has an empty name")
    _require("\\" not in name and not ntpath.splitdrive(name)[0], f"unsafe archive member path: {name!r}")
    _require(not name.startswith("/"), f"absolute archive member path: {name!r}")
    stripped = name[:-1] if name.endswith("/") else name
    _require(bool(stripped), f"unsafe archive member path: {name!r}")
    parts = tuple(stripped.split("/"))
    _require(
        all(part not in {"", ".", ".."} for part in parts),
        f"archive member contains traversal or ambiguous components: {name!r}",
    )
    return parts


def _open_tar(path: pathlib.Path) -> tarfile.TarFile:
    try:
        return tarfile.open(path, mode="r:zst")
    except (tarfile.TarError, OSError, ValueError) as exc:
        raise HydrationError(f"archive is not a readable tar_zstd file: {exc}") from exc


@contextlib.contextmanager
def _open_public_tar(
    path: pathlib.Path,
    *,
    expected_metadata: os.stat_result | None = None,
) -> Iterator[tarfile.TarFile]:
    """Open a bounded public archive through one no-follow regular-file fd."""
    flags = os.O_RDONLY
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    try:
        descriptor = os.open(path, flags)
    except OSError as exc:
        raise HydrationError(f"public archive cannot be opened safely: {exc}") from exc
    try:
        metadata = os.fstat(descriptor)
        _require(stat.S_ISREG(metadata.st_mode), "public archive must be one regular file")
        _require(0 < metadata.st_size <= MAX_PUBLIC_ARCHIVE_BYTES, "public archive compressed size is outside the safety bound")
        if expected_metadata is not None:
            _require(
                metadata.st_dev == expected_metadata.st_dev
                and metadata.st_ino == expected_metadata.st_ino
                and metadata.st_size == expected_metadata.st_size,
                "public archive identity changed between validation passes",
            )
        with os.fdopen(descriptor, "rb", closefd=False) as handle:
            try:
                with tarfile.open(fileobj=handle, mode="r:zst") as archive:
                    yield archive
            except (tarfile.TarError, OSError, ValueError) as exc:
                raise HydrationError(f"public archive is not readable tar_zstd: {exc}") from exc
    finally:
        os.close(descriptor)


def inspect_archive(path: pathlib.Path, contract: StorageContract) -> ArchiveInventory:
    members: list[ArchiveMember] = []
    names: set[str] = set()
    regular_paths: set[tuple[str, ...]] = set()
    logical_bytes = 0
    max_members = contract.regular_file_count * 4 + 128
    with _open_tar(path) as archive:
        try:
            for index, member in enumerate(archive, 1):
                _require(index <= max_members, "archive contains implausibly many metadata entries")
                parts = _member_parts(member.name)
                normalized = "/".join(parts)
                _require(normalized not in names, f"duplicate archive member path: {normalized}")
                names.add(normalized)
                _require(parts[0] == contract.archive_root, "archive contains multiple or unexpected roots")
                _require(not member.issym(), f"archive contains a symbolic link: {normalized}")
                _require(not member.islnk(), f"archive contains a hard link: {normalized}")
                _require(not member.isdev() and not member.isfifo(), f"archive contains a device or FIFO: {normalized}")
                _require(member.isfile() or member.isdir(), f"archive contains an unsupported entry: {normalized}")
                _require(not getattr(member, "sparse", None), f"archive contains a sparse file: {normalized}")
                if len(parts) == 1:
                    _require(member.isdir(), "archive root entry must be a directory")
                if member.isdir():
                    _require(member.size == 0, f"archive directory has a non-zero size: {normalized}")
                else:
                    _require(member.size >= 0, f"archive file has a negative size: {normalized}")
                    logical_bytes += member.size
                    _require(
                        logical_bytes <= contract.logical_bytes,
                        "archive logical bytes exceed storage contract",
                    )
                    regular_paths.add(parts)
                members.append(
                    ArchiveMember(
                        name=normalized,
                        parts=parts,
                        is_file=member.isfile(),
                        size=member.size,
                        executable=bool(member.mode & 0o111),
                    )
                )
        except (tarfile.TarError, OSError) as exc:
            raise HydrationError(f"archive metadata cannot be read safely: {exc}") from exc

    for path_parts in regular_paths:
        for length in range(1, len(path_parts)):
            _require(
                path_parts[:length] not in regular_paths,
                f"archive file is also a parent path: {'/'.join(path_parts[:length])}",
            )
    regular_file_count = len(regular_paths)
    _require(
        regular_file_count == contract.regular_file_count,
        f"archive has {regular_file_count} regular files, expected {contract.regular_file_count}",
    )
    _require(
        logical_bytes == contract.logical_bytes,
        f"archive has {logical_bytes} logical bytes, expected {contract.logical_bytes}",
    )
    _require(
        contract.manifest_path.parts in regular_paths,
        "archive does not contain the contracted evidence manifest as a regular file",
    )
    return ArchiveInventory(tuple(members), regular_file_count, logical_bytes)


def _write_member(archive: tarfile.TarFile, member: tarfile.TarInfo, destination: pathlib.Path) -> None:
    source = archive.extractfile(member)
    _require(source is not None, f"archive file cannot be read: {member.name}")
    remaining = member.size
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    try:
        descriptor = os.open(destination, flags, 0o600)
        with os.fdopen(descriptor, "wb") as output:
            while remaining:
                chunk = source.read(min(COPY_CHUNK_BYTES, remaining))
                _require(bool(chunk), f"archive file ended early: {member.name}")
                output.write(chunk)
                remaining -= len(chunk)
            _require(source.read(1) == b"", f"archive file exceeds declared size: {member.name}")
            output.flush()
            os.fsync(output.fileno())
        destination.chmod(0o755 if member.mode & 0o111 else 0o644)
    except HydrationError:
        raise
    except OSError as exc:
        raise HydrationError(f"archive member could not be written safely: {member.name}: {exc}") from exc
    finally:
        source.close()


def extract_archive(path: pathlib.Path, destination: pathlib.Path, contract: StorageContract) -> None:
    """Extract manually into a private staging directory after a full scan."""

    destination.mkdir(mode=0o700)
    with _open_tar(path) as archive:
        try:
            for member in archive:
                parts = _member_parts(member.name)
                target = destination.joinpath(*parts)
                if member.isdir():
                    target.mkdir(mode=0o700, parents=True, exist_ok=True)
                else:
                    target.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
                    _write_member(archive, member, target)
        except HydrationError:
            raise
        except (tarfile.TarError, OSError) as exc:
            raise HydrationError(f"archive extraction failed: {exc}") from exc

    for current, directories, _files in os.walk(destination, topdown=False, followlinks=False):
        for directory in directories:
            pathlib.Path(current, directory).chmod(0o755)
    destination.chmod(0o755)


def verify_extracted_tree(destination: pathlib.Path, contract: StorageContract) -> None:
    roots = list(destination.iterdir())
    _require(
        len(roots) == 1 and roots[0].name == contract.archive_root and roots[0].is_dir() and not roots[0].is_symlink(),
        "hydrated evidence does not contain exactly the contracted root directory",
    )
    regular_file_count = 0
    logical_bytes = 0
    for current, directories, files in os.walk(destination, followlinks=False):
        current_path = pathlib.Path(current)
        for name in directories:
            path = current_path / name
            metadata = path.lstat()
            _require(stat.S_ISDIR(metadata.st_mode), f"hydrated tree contains a non-directory: {path}")
        for name in files:
            path = current_path / name
            metadata = path.lstat()
            _require(stat.S_ISREG(metadata.st_mode), f"hydrated tree contains a non-regular file: {path}")
            regular_file_count += 1
            logical_bytes += metadata.st_size
    _require(
        regular_file_count == contract.regular_file_count,
        "hydrated regular-file count does not match storage contract",
    )
    _require(logical_bytes == contract.logical_bytes, "hydrated logical byte count does not match storage contract")
    manifest = destination.joinpath(*contract.manifest_path.parts)
    try:
        metadata = manifest.lstat()
    except OSError as exc:
        raise HydrationError(f"hydrated evidence manifest is missing: {exc}") from exc
    _require(stat.S_ISREG(metadata.st_mode), "hydrated evidence manifest must be a regular file")
    _require(sha256_file(manifest) == contract.manifest_sha256, "hydrated evidence manifest SHA-256 mismatch")


def _public_path_key(value: str) -> str:
    """Return normalization keys used to reject cross-platform collisions."""
    return unicodedata.normalize("NFC", value).casefold()


def _validate_public_relative_path(value: str, label: str) -> pathlib.PurePosixPath:
    _require(isinstance(value, str) and bool(value), f"{label} is empty")
    _require("\\" not in value and not ntpath.splitdrive(value)[0], f"{label} contains a backslash or drive")
    _require(not any(ord(character) < 32 or ord(character) == 127 for character in value), f"{label} contains a control character")
    _require(unicodedata.normalize("NFC", value) == value, f"{label} is not NFC normalized")
    path = pathlib.PurePosixPath(value)
    _require(not path.is_absolute(), f"{label} is absolute")
    _require(all(part not in {"", ".", ".."} for part in path.parts), f"{label} contains an ambiguous component")
    return path


def _safe_public_source(
    bundle: pathlib.Path,
    *,
    require_canonical_modes: bool = False,
) -> tuple[dict[str, bytes], tuple[PublicTreeEntry, ...]]:
    """Read the exact public-v4 tree once after a no-link filesystem scan."""
    try:
        root_metadata = bundle.lstat()
    except OSError as exc:
        raise HydrationError(f"public v4 bundle cannot be opened: {exc}") from exc
    _require(stat.S_ISDIR(root_metadata.st_mode) and not bundle.is_symlink(), "public v4 bundle must be one real directory")
    if require_canonical_modes:
        _require(stat.S_IMODE(root_metadata.st_mode) == 0o755, "hydrated public root mode differs")
    expected_files = set(PUBLIC_SOURCE_PATHS)
    expected_directories = {"arms"}
    actual_files: set[str] = set()
    actual_directories: set[str] = set()
    collision_keys: dict[str, str] = {}
    scanned_metadata: dict[str, os.stat_result] = {}
    for current, dirnames, filenames in os.walk(bundle, topdown=True, followlinks=False):
        current_path = pathlib.Path(current)
        for name in [*dirnames, *filenames]:
            target = current_path / name
            relative = target.relative_to(bundle).as_posix()
            _validate_public_relative_path(relative, f"public v4 path {relative!r}")
            key = _public_path_key(relative)
            previous = collision_keys.get(key)
            _require(previous is None or previous == relative, f"public v4 paths collide after normalization: {previous!r}, {relative!r}")
            collision_keys[key] = relative
            try:
                metadata = target.lstat()
            except OSError as exc:
                raise HydrationError(f"public v4 entry cannot be inspected: {relative}: {exc}") from exc
            if stat.S_ISLNK(metadata.st_mode):
                raise HydrationError(f"public v4 bundle contains a symbolic link: {relative}")
            if stat.S_ISDIR(metadata.st_mode):
                actual_directories.add(relative)
                if require_canonical_modes:
                    _require(stat.S_IMODE(metadata.st_mode) == 0o755, f"hydrated public directory mode differs: {relative}")
            elif stat.S_ISREG(metadata.st_mode):
                actual_files.add(relative)
                if require_canonical_modes:
                    _require(stat.S_IMODE(metadata.st_mode) == 0o644, f"hydrated public file mode differs: {relative}")
                _require(metadata.st_size <= MAX_PUBLIC_LOGICAL_BYTES, f"public v4 file is too large: {relative}")
            else:
                raise HydrationError(f"public v4 bundle contains a special entry: {relative}")
            scanned_metadata[relative] = metadata
    _require(actual_directories == expected_directories, "public v4 bundle directory set is not canonical")
    _require(actual_files == expected_files, "public v4 bundle file set is not canonical")

    payloads: dict[str, bytes] = {}
    entries: list[PublicTreeEntry] = [
        PublicTreeEntry("directory", PUBLIC_ARCHIVE_ROOT, 0o755, "0" * 64, 0),
        PublicTreeEntry("directory", f"{PUBLIC_ARCHIVE_ROOT}/arms", 0o755, "0" * 64, 0),
    ]
    logical_bytes = 0
    for relative in PUBLIC_SOURCE_PATHS:
        source = bundle.joinpath(*pathlib.PurePosixPath(relative).parts)
        flags = os.O_RDONLY
        if hasattr(os, "O_NOFOLLOW"):
            flags |= os.O_NOFOLLOW
        try:
            descriptor = os.open(source, flags)
            try:
                metadata = os.fstat(descriptor)
                _require(stat.S_ISREG(metadata.st_mode), f"public v4 source changed type: {relative}")
                scanned = scanned_metadata[relative]
                _require(
                    metadata.st_dev == scanned.st_dev and metadata.st_ino == scanned.st_ino,
                    f"public v4 source changed identity while opening: {relative}",
                )
                _require(metadata.st_size <= MAX_PUBLIC_LOGICAL_BYTES, f"public v4 file is too large: {relative}")
                raw = bytearray()
                while len(raw) <= metadata.st_size:
                    chunk = os.read(descriptor, min(COPY_CHUNK_BYTES, metadata.st_size + 1 - len(raw)))
                    if not chunk:
                        break
                    raw.extend(chunk)
                _require(len(raw) == metadata.st_size and os.read(descriptor, 1) == b"", f"public v4 source changed while reading: {relative}")
            finally:
                os.close(descriptor)
        except HydrationError:
            raise
        except OSError as exc:
            raise HydrationError(f"public v4 source cannot be read safely: {relative}: {exc}") from exc
        logical_bytes += len(raw)
        _require(logical_bytes <= MAX_PUBLIC_LOGICAL_BYTES, "public v4 logical bytes exceed the safety bound")
        archive_relative = f"{PUBLIC_ARCHIVE_ROOT}/{relative}"
        payloads[archive_relative] = bytes(raw)
        entries.append(PublicTreeEntry("file", archive_relative, 0o644, hashlib.sha256(raw).hexdigest(), len(raw)))
    final_root = bundle.lstat()
    final_arms = (bundle / "arms").lstat()
    _require(
        final_root.st_dev == root_metadata.st_dev and final_root.st_ino == root_metadata.st_ino,
        "public v4 bundle root changed during packaging",
    )
    scanned_arms = scanned_metadata["arms"]
    _require(
        final_arms.st_dev == scanned_arms.st_dev and final_arms.st_ino == scanned_arms.st_ino,
        "public v4 arms directory changed during packaging",
    )
    return payloads, tuple(sorted(entries, key=lambda item: item.path))


def public_tree_inventory_sha256(entries: Sequence[PublicTreeEntry]) -> str:
    digest = hashlib.sha256()
    previous = ""
    seen_keys: set[str] = set()
    for entry in entries:
        _require(entry.path > previous, "public tree inventory is not strict lexicographic order")
        previous = entry.path
        _validate_public_relative_path(entry.path, "public tree inventory path")
        key = _public_path_key(entry.path)
        _require(key not in seen_keys, "public tree inventory contains a normalized path collision")
        seen_keys.add(key)
        _require(entry.entry_type in {"directory", "file"}, "public tree inventory type is invalid")
        expected_mode = 0o755 if entry.entry_type == "directory" else 0o644
        _require(entry.mode == expected_mode, "public tree inventory mode is noncanonical")
        _require(entry.size >= 0, "public tree inventory size is negative")
        _sha256(entry.sha256, "public tree inventory SHA-256")
        if entry.entry_type == "directory":
            _require(entry.size == 0 and entry.sha256 == "0" * 64, "public tree directory commitment is invalid")
        digest.update(entry.entry_type.encode("ascii"))
        digest.update(b"\0")
        digest.update(entry.path.encode("utf-8"))
        digest.update(b"\0")
        digest.update(f"{entry.mode:04o}".encode("ascii"))
        digest.update(b"\0")
        digest.update(entry.sha256.encode("ascii"))
        digest.update(b"\0")
        digest.update(str(entry.size).encode("ascii"))
        digest.update(b"\n")
    return digest.hexdigest()


def _canonical_tar_info(name: str, *, directory: bool, size: int = 0) -> tarfile.TarInfo:
    member = tarfile.TarInfo(name)
    member.type = tarfile.DIRTYPE if directory else tarfile.REGTYPE
    member.mode = 0o755 if directory else 0o644
    member.uid = 0
    member.gid = 0
    member.uname = ""
    member.gname = ""
    member.mtime = 0
    member.size = 0 if directory else size
    member.pax_headers = {}
    return member


def package_public_v4(bundle: pathlib.Path, archive_path: pathlib.Path) -> PublicPackage:
    """Create a deterministic, exact-tree tar_zstd archive from a public-v4 bundle."""
    import io

    _require(not os.path.lexists(archive_path), f"refusing to replace public archive: {archive_path}")
    payloads, entries = _safe_public_source(bundle)
    archive_path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, raw_temporary = tempfile.mkstemp(
        prefix=f".{archive_path.name}.tmp-",
        suffix=".tar.zst",
        dir=archive_path.parent,
    )
    temporary = pathlib.Path(raw_temporary)
    try:
        with os.fdopen(descriptor, "w+b") as output:
            with tarfile.open(fileobj=output, mode="w:zst") as archive:
                for entry in entries:
                    if entry.entry_type == "directory":
                        archive.addfile(_canonical_tar_info(entry.path, directory=True))
                    else:
                        raw = payloads[entry.path]
                        archive.addfile(_canonical_tar_info(entry.path, directory=False, size=len(raw)), io.BytesIO(raw))
            output.flush()
            os.fsync(output.fileno())
        size = temporary.stat().st_size
        _require(0 < size <= MAX_PUBLIC_ARCHIVE_BYTES, "public archive size exceeds the safety bound")
        _rename_noreplace(temporary, archive_path)
        _fsync_directory(archive_path.parent)
    except BaseException:
        try:
            temporary.unlink()
        except FileNotFoundError:
            pass
        raise
    manifest = payloads[str(PUBLIC_MANIFEST_PATH)]
    payload_entries = [entry for entry in entries if entry.entry_type == "file" and entry.path != str(PUBLIC_MANIFEST_PATH)]
    return PublicPackage(
        archive_path=archive_path,
        archive_sha256=sha256_file(archive_path),
        archive_size_bytes=archive_path.stat().st_size,
        regular_file_count=sum(entry.entry_type == "file" for entry in entries),
        directory_count=sum(entry.entry_type == "directory" for entry in entries),
        logical_bytes=sum(entry.size for entry in entries if entry.entry_type == "file"),
        tree_inventory_algorithm=PUBLIC_TREE_INVENTORY_ALGORITHM,
        tree_inventory_sha256=public_tree_inventory_sha256(entries),
        manifest_sha256=hashlib.sha256(manifest).hexdigest(),
        manifest_size_bytes=len(manifest),
        payload_file_count=len(payload_entries),
        payload_logical_bytes=sum(entry.size for entry in payload_entries),
    )


def inspect_public_archive(
    path: pathlib.Path,
    *,
    expected_metadata: os.stat_result | None = None,
) -> tuple[PublicTreeEntry, ...]:
    """Scan the complete archive before extraction and require canonical metadata."""
    entries: list[PublicTreeEntry] = []
    seen_paths: set[str] = set()
    seen_keys: set[tuple[str, str]] = set()
    logical_bytes = 0
    with _open_public_tar(path, expected_metadata=expected_metadata) as archive:
        try:
            _require(not archive.pax_headers, "public archive contains noncanonical global PAX metadata")
            for index, member in enumerate(archive, 1):
                _require(index <= len(PUBLIC_ARCHIVE_PATHS), "public archive contains too many metadata entries")
                normalized = "/".join(_validate_public_relative_path(member.name.rstrip("/"), "public archive member").parts)
                _require(member.name == normalized, f"public archive member spelling is noncanonical: {member.name!r}")
                _require(normalized not in seen_paths, f"duplicate public archive member: {normalized}")
                seen_paths.add(normalized)
                key = _public_path_key(normalized)
                _require(key not in seen_keys, f"public archive members collide after normalization: {normalized}")
                seen_keys.add(key)
                _require(member.uid == 0 and member.gid == 0, f"public archive ownership metadata is noncanonical: {normalized}")
                _require(member.uname == "" and member.gname == "", f"public archive owner names are noncanonical: {normalized}")
                _require(member.mtime == 0, f"public archive timestamp is noncanonical: {normalized}")
                _require(not member.pax_headers, f"public archive contains noncanonical PAX metadata: {normalized}")
                _require(not member.issym() and not member.islnk(), f"public archive contains a link: {normalized}")
                _require(not member.isdev() and not member.isfifo(), f"public archive contains a device or FIFO: {normalized}")
                _require(not getattr(member, "sparse", None), f"public archive contains a sparse file: {normalized}")
                _require(member.isdir() or member.isfile(), f"public archive contains an unsupported entry: {normalized}")
                if member.isdir():
                    _require(member.size == 0 and stat.S_IMODE(member.mode) == 0o755, f"public archive directory metadata is noncanonical: {normalized}")
                    entries.append(PublicTreeEntry("directory", normalized, 0o755, "0" * 64, 0))
                    continue
                _require(stat.S_IMODE(member.mode) == 0o644, f"public archive file mode is noncanonical: {normalized}")
                _require(member.size >= 0, f"public archive file has a negative size: {normalized}")
                logical_bytes += member.size
                _require(logical_bytes <= MAX_PUBLIC_LOGICAL_BYTES, "public archive logical bytes exceed the safety bound")
                source = archive.extractfile(member)
                _require(source is not None, f"public archive file cannot be opened: {normalized}")
                digest = hashlib.sha256()
                remaining = member.size
                while remaining:
                    chunk = source.read(min(COPY_CHUNK_BYTES, remaining))
                    _require(bool(chunk), f"public archive file ended early: {normalized}")
                    digest.update(chunk)
                    remaining -= len(chunk)
                _require(source.read(1) == b"", f"public archive file exceeds its declared size: {normalized}")
                source.close()
                entries.append(PublicTreeEntry("file", normalized, 0o644, digest.hexdigest(), member.size))
        except HydrationError:
            raise
        except (OSError, tarfile.TarError) as exc:
            raise HydrationError(f"public archive cannot be scanned safely: {exc}") from exc
    _require(tuple(entry.path for entry in entries) == PUBLIC_ARCHIVE_PATHS, "public archive tree or ordering is not canonical")
    _require(len(entries) == 8, "public archive entry count changed")
    return tuple(entries)


def verify_public_tree(root: pathlib.Path, expected_tree_sha256: str) -> tuple[PublicTreeEntry, ...]:
    """Verify a hydrated public root has the exact six-file/two-directory tree."""
    _payloads, entries = capture_public_tree(root, expected_tree_sha256)
    return entries


def capture_public_tree(
    root: pathlib.Path,
    expected_tree_sha256: str,
) -> tuple[dict[str, bytes], tuple[PublicTreeEntry, ...]]:
    """Capture one inode-bound public tree snapshot for all downstream checks."""
    _require(root.name == PUBLIC_ARCHIVE_ROOT, "hydrated public root name changed")
    payloads, entries = _safe_public_source(root, require_canonical_modes=True)
    _require(public_tree_inventory_sha256(entries) == expected_tree_sha256, "hydrated public tree inventory differs")
    return payloads, entries


def _materialize_public_snapshot(payloads: Mapping[str, bytes], destination: pathlib.Path) -> pathlib.Path:
    """Write a captured public snapshot into one private verifier-owned tree."""
    bundle = destination / PUBLIC_ARCHIVE_ROOT
    bundle.mkdir(mode=0o755)
    (bundle / "arms").mkdir(mode=0o755)
    bundle.chmod(0o755)
    (bundle / "arms").chmod(0o755)
    _require(set(payloads) == {f"{PUBLIC_ARCHIVE_ROOT}/{item}" for item in PUBLIC_SOURCE_PATHS}, "captured public snapshot file set changed")
    for archive_relative, raw in payloads.items():
        relative = pathlib.PurePosixPath(archive_relative).relative_to(PUBLIC_ARCHIVE_ROOT)
        target = bundle.joinpath(*relative.parts)
        flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
        if hasattr(os, "O_NOFOLLOW"):
            flags |= os.O_NOFOLLOW
        descriptor = os.open(target, flags, 0o600)
        try:
            with os.fdopen(descriptor, "wb", closefd=False) as handle:
                handle.write(raw)
                handle.flush()
                os.fsync(handle.fileno())
        finally:
            os.close(descriptor)
        target.chmod(0o644)
    return bundle / PUBLIC_MANIFEST_NAME


def _copy_bounded_public_archive(source: pathlib.Path, destination: pathlib.Path) -> tuple[int, str]:
    digest = hashlib.sha256()
    written = 0
    with _open_local_archive(source) as handle:
        flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
        if hasattr(os, "O_NOFOLLOW"):
            flags |= os.O_NOFOLLOW
        descriptor = os.open(destination, flags, 0o600)
        try:
            with os.fdopen(descriptor, "wb", closefd=False) as output:
                while True:
                    chunk = handle.read(COPY_CHUNK_BYTES)
                    if not chunk:
                        break
                    written += len(chunk)
                    _require(written <= MAX_PUBLIC_ARCHIVE_BYTES, "public archive exceeds the safety bound")
                    output.write(chunk)
                    digest.update(chunk)
                output.flush()
                os.fsync(output.fileno())
        finally:
            os.close(descriptor)
    _require(written > 0, "public archive is empty")
    return written, digest.hexdigest()


def extract_public_archive(
    path: pathlib.Path,
    destination_parent: pathlib.Path,
    *,
    expected_size: int | None = None,
    expected_sha256: str | None = None,
    expected_tree_sha256: str | None = None,
) -> pathlib.Path:
    """Extract a pre-scanned public archive and atomically publish only its root."""
    destination = destination_parent / PUBLIC_ARCHIVE_ROOT
    _require(not os.path.lexists(destination), f"destination already exists: {destination}")
    destination_parent.mkdir(parents=True, exist_ok=True)
    stage = pathlib.Path(tempfile.mkdtemp(prefix=".public-v4.hydrate-", dir=destination_parent))
    try:
        staged_archive = stage / "public-v4.tar.zst"
        copied_size, copied_sha256 = _copy_bounded_public_archive(path, staged_archive)
        if expected_size is not None:
            _require(copied_size == expected_size, "public archive size differs from storage contract")
        if expected_sha256 is not None:
            _sha256(expected_sha256, "public archive SHA-256")
            _require(copied_sha256 == expected_sha256, "public archive SHA-256 differs from storage contract")
        staged_metadata = staged_archive.lstat()
        expected_entries = inspect_public_archive(staged_archive, expected_metadata=staged_metadata)
        tree_sha256 = public_tree_inventory_sha256(expected_entries)
        if expected_tree_sha256 is not None:
            _sha256(expected_tree_sha256, "public tree inventory SHA-256")
            _require(tree_sha256 == expected_tree_sha256, "public archive tree inventory differs from storage contract")
        staged_root = stage / PUBLIC_ARCHIVE_ROOT
        staged_root.mkdir(mode=0o755)
        (staged_root / "arms").mkdir(mode=0o755)
        staged_root.chmod(0o755)
        (staged_root / "arms").chmod(0o755)
        with _open_public_tar(staged_archive, expected_metadata=staged_metadata) as archive:
            members = {"/".join(_member_parts(member.name)): member for member in archive}
            for entry in expected_entries:
                if entry.entry_type == "directory":
                    continue
                member = members[entry.path]
                target = stage.joinpath(*pathlib.PurePosixPath(entry.path).parts)
                _write_member(archive, member, target)
                target.chmod(0o644)
        verify_public_tree(staged_root, tree_sha256)
        _fsync_directory(staged_root)
        _rename_noreplace(staged_root, destination)
        _fsync_directory(destination_parent)
    finally:
        shutil.rmtree(stage, ignore_errors=True)
    return destination / PUBLIC_MANIFEST_NAME


def hydrate_restricted_attestation(
    source: pathlib.Path,
    destination_parent: pathlib.Path,
    *,
    expected_size: int,
    expected_sha256: str,
) -> pathlib.Path:
    """Copy one owner-only local attestation into the fixed restricted root."""
    _require(0 < expected_size <= MAX_RESTRICTED_ATTESTATION_BYTES, "restricted attestation size is outside the safety bound")
    _sha256(expected_sha256, "restricted attestation SHA-256")
    try:
        metadata = source.lstat()
    except OSError as exc:
        raise HydrationError(f"restricted attestation cannot be opened: {exc}") from exc
    _require(stat.S_ISREG(metadata.st_mode) and not source.is_symlink(), "restricted attestation source must be one regular file")
    _require(metadata.st_uid == os.getuid(), "restricted attestation source must be owned by the current user")
    _require(stat.S_IMODE(metadata.st_mode) == 0o600, "restricted attestation source must have mode 0600")
    _require(metadata.st_size == expected_size, "restricted attestation size differs")
    restricted_root = destination_parent / RESTRICTED_ATTESTATION_PATH.parts[0]
    destination = destination_parent.joinpath(*RESTRICTED_ATTESTATION_PATH.parts)
    _require(not os.path.lexists(restricted_root), f"destination already exists: {restricted_root}")
    destination_parent.mkdir(parents=True, exist_ok=True)
    stage = pathlib.Path(tempfile.mkdtemp(prefix=".restricted.hydrate-", dir=destination_parent))
    stage.chmod(0o700)
    try:
        staged_root = stage / "restricted"
        staged_root.mkdir(mode=0o700)
        staged = staged_root / "replay-attestation-v1.json"
        with _open_local_archive(source, expected_metadata=metadata) as handle:
            _copy_authenticated_stream(handle, staged, expected_size=expected_size, expected_sha256=expected_sha256)
        staged.chmod(0o600)
        # Import lazily so the legacy diagnostic hydrator remains standalone.
        import restricted_replay_attestation

        restricted_replay_attestation.load_attestation(staged)
        _fsync_directory(staged_root)
        _rename_noreplace(staged_root, restricted_root)
        _fsync_directory(destination_parent)
    finally:
        shutil.rmtree(stage, ignore_errors=True)
    return destination


def _read_hydrated_attestation_buffer(
    path: pathlib.Path,
    *,
    expected_size: int,
    expected_sha256: str,
) -> bytes:
    """Bind custody, raw hash, parsing, and signature verification to one buffer."""
    _positive_int(expected_size, "restricted attestation expected size")
    _require(expected_size <= MAX_RESTRICTED_ATTESTATION_BYTES, "restricted attestation exceeds the safety bound")
    _sha256(expected_sha256, "restricted attestation expected SHA-256")
    try:
        scanned = path.lstat()
    except OSError as exc:
        raise HydrationError(f"hydrated restricted attestation cannot be opened: {exc}") from exc
    _require(stat.S_ISREG(scanned.st_mode) and not path.is_symlink(), "hydrated restricted attestation must be one regular file")
    _require(scanned.st_uid == os.getuid() and stat.S_IMODE(scanned.st_mode) == 0o600, "hydrated restricted attestation custody differs")
    _require(scanned.st_size == expected_size, "hydrated restricted attestation size differs")
    flags = os.O_RDONLY
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    try:
        descriptor = os.open(path, flags)
        try:
            opened = os.fstat(descriptor)
            _require(
                opened.st_dev == scanned.st_dev
                and opened.st_ino == scanned.st_ino
                and opened.st_uid == scanned.st_uid
                and stat.S_IMODE(opened.st_mode) == 0o600
                and opened.st_size == expected_size,
                "hydrated restricted attestation changed while opening",
            )
            raw = os.read(descriptor, expected_size + 1)
            _require(len(raw) == expected_size and os.read(descriptor, 1) == b"", "hydrated restricted attestation changed while reading")
        finally:
            os.close(descriptor)
    except HydrationError:
        raise
    except OSError as exc:
        raise HydrationError(f"hydrated restricted attestation cannot be read safely: {exc}") from exc
    _require(hashlib.sha256(raw).hexdigest() == expected_sha256, "hydrated restricted attestation hash differs")
    return raw


def _repo_path_without_symlinks(repo: pathlib.Path, relative: str, label: str) -> pathlib.Path:
    pure = _safe_relative_path(relative, label)
    current = repo
    for part in pure.parts:
        current = current / part
        if os.path.lexists(current):
            _require(not current.is_symlink(), f"{label} must not traverse a symlink")
    return current


def verify_hydrated_storage_v2(
    contract_path: pathlib.Path = DEFAULT_CONTRACT_PATH,
    *,
    repo: pathlib.Path = REPO,
    matrix: Mapping[str, Any] | None = None,
    pins: Mapping[str, Any] | None = None,
    attestation_verifier: Callable[[Mapping[str, Any], Mapping[str, Any]], None] | None = None,
    hydration_parent: pathlib.Path | None = None,
    verify_external_locks: bool = True,
) -> pathlib.Path:
    """Verify both hydrated roots and the complete approved binding chain offline."""
    if verify_external_locks:
        _require(
            matrix is None and pins is None and attestation_verifier is None,
            "production hydrated verification does not accept injected pins, matrix, or attestation verifier",
        )
    contract = load_storage_contract_v2(contract_path, repo=repo, verify_external_locks=verify_external_locks)
    _require(contract["contract_status"] == "approved", "engine evidence storage contract is pending owner authorization")
    public = contract["public"]
    archive = public["archive"]
    evidence = public["evidence"]
    restricted = contract["restricted"]
    attestation_contract = restricted["attestation"]
    bindings = contract["expected_bindings"]
    hydration = contract["hydration"]

    parent = (
        _repo_path_without_symlinks(repo, hydration["repo_relative_parent"], "engine evidence hydration parent")
        if hydration_parent is None
        else hydration_parent
    )
    _require(parent.is_dir() and not parent.is_symlink(), "engine evidence hydration parent is missing")
    public_root = _repo_path_without_symlinks(parent, hydration["public_archive_root"], "hydrated public archive root")
    public_payloads, entries = capture_public_tree(public_root, archive["tree_inventory_sha256"])
    file_entries = [entry for entry in entries if entry.entry_type == "file"]
    _require(len(file_entries) == archive["regular_file_count"], "hydrated public regular-file count differs")
    _require(sum(entry.size for entry in file_entries) == archive["logical_bytes"], "hydrated public logical bytes differ")
    manifest = parent / hydration["public_manifest_path"]
    manifest_raw = public_payloads[str(PUBLIC_MANIFEST_PATH)]
    _require(len(manifest_raw) == evidence["manifest_size_bytes"], "hydrated public manifest size differs")
    _require(hashlib.sha256(manifest_raw).hexdigest() == evidence["manifest_sha256"], "hydrated public manifest hash differs")
    try:
        public_manifest = json.loads(manifest_raw.decode("utf-8"), object_pairs_hook=_object_without_duplicate_keys)
    except (UnicodeDecodeError, ValueError, RecursionError) as exc:
        raise HydrationError(f"hydrated public manifest is not valid UTF-8 JSON: {exc}") from exc
    _require(isinstance(public_manifest, dict) and type(public_manifest.get("schema_version")) is int and public_manifest["schema_version"] == 4, "hydrated public manifest is not schema v4")
    inventory = public_manifest.get("artifact_inventory")
    _require(isinstance(inventory, dict), "hydrated public manifest inventory is missing")
    _require(inventory.get("algorithm") == evidence["artifact_inventory_algorithm"], "hydrated public artifact inventory algorithm differs")
    _require(inventory.get("root_sha256") == evidence["artifact_inventory_root_sha256"], "hydrated public artifact inventory root differs")
    _require(type(inventory.get("file_count")) is int and inventory["file_count"] == evidence["payload_file_count"], "hydrated public payload count differs")
    _require(type(inventory.get("logical_bytes")) is int and inventory["logical_bytes"] == evidence["payload_logical_bytes"], "hydrated public payload bytes differ")

    if verify_external_locks:
        matrix_raw = _read_dependency_bytes(repo / MATRIX_REPO_PATH, "canonical engine matrix")
        pin_raw = _read_dependency_bytes(repo / PIN_REPO_PATH, "canonical engine pins")
        _require(hashlib.sha256(matrix_raw).hexdigest() == MATRIX_SHA256, "canonical engine matrix raw hash changed before replay")
        _require(hashlib.sha256(pin_raw).hexdigest() == PIN_SHA256, "canonical engine pins raw hash changed before replay")
        matrix = _decode_storage_json(matrix_raw, "canonical engine matrix")
        pins = _decode_storage_json(pin_raw, "canonical engine pins")
    else:
        if matrix is None:
            matrix = _load_storage_json(repo / MATRIX_REPO_PATH)
        if pins is None:
            pins = _load_storage_json(repo / PIN_REPO_PATH)
    expected_pin_descriptor = {"id": PIN_SET_ID, "sha256": PIN_SHA256, "authority": "production"}
    import public_engine_evidence

    with tempfile.TemporaryDirectory(prefix="engine-public-snapshot-") as raw_snapshot:
        snapshot_root = pathlib.Path(raw_snapshot)
        snapshot_root.chmod(0o700)
        snapshot_manifest = _materialize_public_snapshot(public_payloads, snapshot_root)
        public_errors = public_engine_evidence.validate_public_bundle(
            snapshot_manifest,
            dict(matrix),
            dict(pins),
            expected_pin_descriptor,
        )
    _require(not public_errors, "hydrated public v4 validation failed:\n- " + "\n- ".join(public_errors))

    attestation_path = _repo_path_without_symlinks(parent, hydration["restricted_attestation_path"], "hydrated restricted attestation")
    import restricted_replay_attestation
    raw_attestation = _read_hydrated_attestation_buffer(
        attestation_path,
        expected_size=attestation_contract["size_bytes"],
        expected_sha256=attestation_contract["sha256"],
    )
    try:
        envelope = restricted_replay_attestation.load_attestation_bytes(raw_attestation)
    except restricted_replay_attestation.AttestationError as exc:
        raise HydrationError(f"hydrated restricted attestation is invalid: {exc}") from exc
    _require(envelope["statement"] == bindings, "restricted replay statement differs from expected bindings")
    for contract_field, envelope_field in (
        ("schema_version", "schema_version"),
        ("profile", "profile"),
        ("signature_scheme", "signature_scheme"),
        ("signature_namespace", "signature_namespace"),
        ("signed_payload_sha256", "signed_payload_sha256"),
        ("trust_root_id", "trust_root_id"),
        ("signer_principal", "signer_principal"),
        ("public_key_sha256", "public_key_sha256"),
    ):
        _require(attestation_contract[contract_field] == envelope[envelope_field], f"restricted attestation {contract_field} differs from envelope")
    _require(
        envelope["signed_payload_sha256"] == restricted_replay_attestation.sha256_bytes(restricted_replay_attestation.signed_payload(envelope)),
        "restricted replay signed payload commitment differs",
    )
    try:
        trust_path = repo / "benchmarks/agent-brain/confirmatory/engine-replay-trust-roots.json"
        trust_raw = _read_dependency_bytes(trust_path, "canonical engine replay trust roots")
        if verify_external_locks:
            _require(hashlib.sha256(trust_raw).hexdigest() == restricted_replay_attestation.TRUST_ROOTS_SHA256, "engine replay trust roots raw hash changed before signature verification")
        trust_roots = restricted_replay_attestation.load_trust_roots_bytes(trust_raw)
        verifier = restricted_replay_attestation.verify_envelope if attestation_verifier is None else attestation_verifier
        verifier(envelope, trust_roots)
    except restricted_replay_attestation.AttestationError as exc:
        raise HydrationError(f"restricted replay signature verification failed: {exc}") from exc
    return manifest


def hydrate_storage_v2(
    contract_path: pathlib.Path,
    archive_path: pathlib.Path,
    attestation_path: pathlib.Path,
    *,
    repo: pathlib.Path = REPO,
    matrix: Mapping[str, Any] | None = None,
    pins: Mapping[str, Any] | None = None,
    attestation_verifier: Callable[[Mapping[str, Any], Mapping[str, Any]], None] | None = None,
    verify_external_locks: bool = True,
) -> pathlib.Path:
    """Hydrate and verify both roots before one atomic no-replace publication."""
    contract = load_storage_contract_v2(contract_path, repo=repo, verify_external_locks=verify_external_locks)
    _require(contract["contract_status"] == "approved", "engine evidence storage contract is pending owner authorization")
    storage = contract["public"]["storage"]
    archive = contract["public"]["archive"]
    attestation = contract["restricted"]["attestation"]
    destination = repo / contract["hydration"]["repo_relative_parent"]
    _require(not os.path.lexists(destination), f"destination already exists: {destination}")
    destination.parent.mkdir(parents=True, exist_ok=True)
    stage = pathlib.Path(tempfile.mkdtemp(prefix=".engine-evidence.hydrate-", dir=destination.parent))
    staged_parent = stage / destination.name
    try:
        extract_public_archive(
            archive_path,
            staged_parent,
            expected_size=storage["asset_size_bytes"],
            expected_sha256=storage["asset_sha256"],
            expected_tree_sha256=archive["tree_inventory_sha256"],
        )
        hydrate_restricted_attestation(
            attestation_path,
            staged_parent,
            expected_size=attestation["size_bytes"],
            expected_sha256=attestation["sha256"],
        )
        verify_hydrated_storage_v2(
            contract_path,
            repo=repo,
            matrix=matrix,
            pins=pins,
            attestation_verifier=attestation_verifier,
            hydration_parent=staged_parent,
            verify_external_locks=verify_external_locks,
        )
        _fsync_directory(staged_parent)
        _rename_noreplace(staged_parent, destination)
        _fsync_directory(destination.parent)
    finally:
        shutil.rmtree(stage, ignore_errors=True)
    return destination / contract["hydration"]["public_manifest_path"]


def _fsync_directory(path: pathlib.Path) -> None:
    try:
        descriptor = os.open(path, os.O_RDONLY)
        try:
            os.fsync(descriptor)
        finally:
            os.close(descriptor)
    except OSError as exc:
        if exc.errno not in {errno.EINVAL, errno.ENOTSUP}:
            raise HydrationError(f"cannot synchronize directory {path}: {exc}") from exc


def _rename_noreplace(source: pathlib.Path, destination: pathlib.Path) -> None:
    """Publish a directory atomically without an overwrite race where supported."""

    libc = ctypes.CDLL(None, use_errno=True)
    source_bytes = os.fsencode(source)
    destination_bytes = os.fsencode(destination)
    result: int | None = None
    if sys.platform == "darwin" and hasattr(libc, "renamex_np"):
        function = libc.renamex_np
        function.argtypes = [ctypes.c_char_p, ctypes.c_char_p, ctypes.c_uint]
        function.restype = ctypes.c_int
        result = function(source_bytes, destination_bytes, RENAME_EXCL)
    elif sys.platform.startswith("linux") and hasattr(libc, "renameat2"):
        function = libc.renameat2
        function.argtypes = [ctypes.c_int, ctypes.c_char_p, ctypes.c_int, ctypes.c_char_p, ctypes.c_uint]
        function.restype = ctypes.c_int
        result = function(AT_FDCWD, source_bytes, AT_FDCWD, destination_bytes, RENAME_NOREPLACE)
    if result is not None:
        if result == 0:
            return
        error = ctypes.get_errno()
        if error in {errno.EEXIST, errno.ENOTEMPTY}:
            raise HydrationError(f"destination already exists: {destination}")
        raise HydrationError(f"atomic evidence publication failed: {os.strerror(error)}")

    # Python has no portable no-replace directory rename. The destination is
    # checked immediately before rename on unsupported platforms; production
    # Darwin and Linux paths use the atomic primitives above.
    _require(not os.path.lexists(destination), f"destination already exists: {destination}")
    try:
        os.rename(source, destination)
    except OSError as exc:
        raise HydrationError(f"atomic evidence publication failed: {exc}") from exc


def hydrate(
    contract_path: pathlib.Path,
    destination: pathlib.Path,
    *,
    archive_path: pathlib.Path | None = None,
    download_url: str | None = None,
    release_asset: bool = False,
    urlopen: Callable[..., Any] = urllib.request.urlopen,
) -> pathlib.Path:
    """Authenticate, safely extract, validate, and atomically publish evidence."""

    selected_sources = sum((archive_path is not None, download_url is not None, release_asset))
    _require(selected_sources == 1, "select exactly one archive source")
    contract = load_contract(contract_path)
    destination = destination.expanduser().absolute()
    _require(not os.path.lexists(destination), f"destination already exists: {destination}")
    try:
        destination.parent.mkdir(parents=True, exist_ok=True)
    except OSError as exc:
        raise HydrationError(f"destination parent cannot be created: {exc}") from exc
    _require(destination.parent.is_dir(), "destination parent is not a directory")

    stage = pathlib.Path(tempfile.mkdtemp(prefix=f".{destination.name}.hydrate-", dir=destination.parent))
    staged_archive = stage / contract.asset_name
    staged_tree = stage / "tree"
    try:
        if archive_path is not None:
            with _open_local_archive(archive_path.expanduser()) as source:
                _copy_authenticated_stream(
                    source,
                    staged_archive,
                    expected_size=contract.asset_size_bytes,
                    expected_sha256=contract.asset_sha256,
                )
        else:
            url = release_asset_url(contract) if release_asset else download_url
            assert url is not None
            _download_archive(url, staged_archive, contract, urlopen=urlopen)

        inspect_archive(staged_archive, contract)
        extract_archive(staged_archive, staged_tree, contract)
        verify_extracted_tree(staged_tree, contract)
        _fsync_directory(staged_tree)
        _rename_noreplace(staged_tree, destination)
        _fsync_directory(destination.parent)
    finally:
        shutil.rmtree(stage, ignore_errors=True)
    return destination.joinpath(*contract.manifest_path.parts)


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    inspect = commands.add_parser("inspect-contract", help="validate canonical v2 structure and lock bindings")
    inspect.add_argument("--contract", type=pathlib.Path, default=DEFAULT_CONTRACT_PATH)
    package = commands.add_parser("package-public", help="build one deterministic public-v4 tar_zstd archive")
    package.add_argument("--bundle", type=pathlib.Path, required=True)
    package.add_argument("--output", type=pathlib.Path, required=True)
    hydrate_public = commands.add_parser("hydrate-public", help="hydrate one approved local public archive")
    hydrate_public.add_argument("--contract", type=pathlib.Path, default=DEFAULT_CONTRACT_PATH)
    hydrate_public.add_argument("--archive", type=pathlib.Path, required=True)
    hydrate_public.add_argument("--destination", type=pathlib.Path)
    hydrate_attestation = commands.add_parser("hydrate-attestation", help="hydrate one approved local restricted attestation")
    hydrate_attestation.add_argument("--contract", type=pathlib.Path, default=DEFAULT_CONTRACT_PATH)
    hydrate_attestation.add_argument("--attestation", type=pathlib.Path, required=True)
    hydrate_attestation.add_argument("--destination", type=pathlib.Path)
    hydrate_all = commands.add_parser("hydrate-all", help="atomically hydrate and verify both approved local roots")
    hydrate_all.add_argument("--contract", type=pathlib.Path, default=DEFAULT_CONTRACT_PATH)
    hydrate_all.add_argument("--archive", type=pathlib.Path, required=True)
    hydrate_all.add_argument("--attestation", type=pathlib.Path, required=True)
    verify = commands.add_parser("verify-hydrated", help="verify both approved hydrated roots offline")
    verify.add_argument("--contract", type=pathlib.Path, default=DEFAULT_CONTRACT_PATH)
    legacy = commands.add_parser("legacy-hydrate", help="explicitly hydrate diagnostic-only legacy v1")
    legacy.add_argument("--allow-legacy-diagnostic", action="store_true", required=True)
    legacy.add_argument("--contract", type=pathlib.Path, default=LEGACY_CONTRACT_PATH)
    legacy.add_argument("--destination", type=pathlib.Path, required=True)
    legacy_sources = legacy.add_mutually_exclusive_group(required=True)
    legacy_sources.add_argument("--archive", type=pathlib.Path)
    legacy_sources.add_argument("--url")
    legacy_sources.add_argument("--release", action="store_true")
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    parser = _parser()
    args = parser.parse_args(argv)
    try:
        if args.command == "inspect-contract":
            contract = load_storage_contract_v2(args.contract)
            print(json.dumps({"schema_version": 2, "profile": contract["profile"], "contract_status": contract["contract_status"]}, sort_keys=True))
            return 0
        if args.command == "package-public":
            package = package_public_v4(args.bundle, args.output)
            print(
                json.dumps(
                    {
                        "archive_sha256": package.archive_sha256,
                        "archive_size_bytes": package.archive_size_bytes,
                        "regular_file_count": package.regular_file_count,
                        "directory_count": package.directory_count,
                        "logical_bytes": package.logical_bytes,
                        "tree_inventory_algorithm": package.tree_inventory_algorithm,
                        "tree_inventory_sha256": package.tree_inventory_sha256,
                        "manifest_sha256": package.manifest_sha256,
                        "manifest_size_bytes": package.manifest_size_bytes,
                        "payload_file_count": package.payload_file_count,
                        "payload_logical_bytes": package.payload_logical_bytes,
                    },
                    sort_keys=True,
                )
            )
            return 0
        if args.command == "verify-hydrated":
            manifest = verify_hydrated_storage_v2(args.contract)
        elif args.command == "hydrate-all":
            manifest = hydrate_storage_v2(args.contract, args.archive, args.attestation)
        elif args.command == "hydrate-public":
            contract = load_storage_contract_v2(args.contract)
            _require(contract["contract_status"] == "approved", "engine evidence storage contract is pending owner authorization")
            destination = args.destination or REPO / contract["hydration"]["repo_relative_parent"]
            manifest = extract_public_archive(
                args.archive,
                destination,
                expected_size=contract["public"]["storage"]["asset_size_bytes"],
                expected_sha256=contract["public"]["storage"]["asset_sha256"],
                expected_tree_sha256=contract["public"]["archive"]["tree_inventory_sha256"],
            )
        elif args.command == "hydrate-attestation":
            contract = load_storage_contract_v2(args.contract)
            _require(contract["contract_status"] == "approved", "engine evidence storage contract is pending owner authorization")
            destination = args.destination or REPO / contract["hydration"]["repo_relative_parent"]
            restricted = contract["restricted"]["attestation"]
            manifest = hydrate_restricted_attestation(
                args.attestation,
                destination,
                expected_size=restricted["size_bytes"],
                expected_sha256=restricted["sha256"],
            )
        else:
            manifest = hydrate(
                args.contract,
                args.destination,
                archive_path=args.archive,
                download_url=args.url,
                release_asset=args.release,
            )
    except HydrationError as exc:
        parser.error(str(exc))
    print(manifest)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
