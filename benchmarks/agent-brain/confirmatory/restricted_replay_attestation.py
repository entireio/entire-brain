#!/usr/bin/env python3
"""Issue and verify offline SSHSIG attestations for restricted engine replay.

The canonical repository contract intentionally contains no signing key or key
selection rule.  Owners select a key outside this tool and authorize its public
half in ``engine-replay-trust-roots.json``.  Production verification is offline:
the exact signed bytes, allowed-signers file, and signature are materialized in
a private temporary directory and passed to ``ssh-keygen -Y verify`` without a
shell or network access.

The ``issue`` command is fixture-only while the checked-in contract remains
``pending_owner_authorization``.  It exists so the complete byte contract can be
tested without creating or implying a production attestation.
"""

from __future__ import annotations

import argparse
import base64
import datetime as dt
import hashlib
import json
import os
import pathlib
import re
import stat
import struct
import subprocess
import tempfile
from typing import Any, Mapping, Sequence


HERE = pathlib.Path(__file__).resolve().parent
REPO = HERE.parents[2]
TRUST_ROOTS_PATH = HERE / "engine-replay-trust-roots.json"
TRUST_ROOTS_SHA256 = "b5fa8e8369663499d52464ddefb97c4ddc5fffc99f3c0c5b8627ce097753ff3b"
CHECKER_LOCK_PATH = HERE / "engine-replay-checker-lock.json"
ATTESTATION_SCHEMA_PATH = HERE / "schemas" / "engine-restricted-replay-attestation-v1.schema.json"
TRUST_ROOTS_SCHEMA_PATH = HERE / "schemas" / "engine-replay-trust-roots-v1.schema.json"
CHECKER_LOCK_SCHEMA_PATH = HERE / "schemas" / "engine-replay-checker-lock-v1.schema.json"

PROFILE = "restricted_engine_replay_attestation_v1"
TRUST_PROFILE = "engine_replay_sshsig_trust_roots_v1"
CHECKER_LOCK_PROFILE = "engine_replay_checker_lock_v1"
CANONICALIZATION = "utf8_json_sort_keys_compact_no_float_v1"
SIGNATURE_SCHEME = "sshsig_ed25519_v1"
SIGNATURE_NAMESPACE = "entire-brain-engine-replay-v1"
PAYLOAD_DOMAIN = b"entire-brain/restricted-engine-replay-attestation/v1\0"
CHECKER_LOCK_ALGORITHM = "sha256_ordered_path_nul_sha256_newline_v1"
TREE_INVENTORY_ALGORITHM = "sha256_ordered_type_nul_path_nul_mode_nul_sha256_nul_size_newline_v1"
PUBLIC_INVENTORY_ALGORITHM = "sha256_ordered_relative_path_nul_sha256_nul_size_newline_v1"
PIN_PATH = "benchmarks/agent-brain/confirmatory/engine-verification-pins.json"
MATRIX_PATH = "benchmarks/agent-brain/confirmatory/engine-matrix.json"
CHECKER_LOCK_REPO_PATH = "benchmarks/agent-brain/confirmatory/engine-replay-checker-lock.json"
ANALYZER_LOCK_REPO_PATH = "benchmarks/agent-brain/confirmatory/analyzer-lock.json"
REPOSITORY = "entireio/entire-brain"
PUBLIC_ROOT = "public-v4-v1"
PUBLIC_MANIFEST_PATH = f"{PUBLIC_ROOT}/engine-verification-public-v4.json"
PUBLIC_PAYLOAD_FILE_COUNT = 5
PUBLIC_REGULAR_FILE_COUNT = 6
PUBLIC_DIRECTORY_COUNT = 2
PRIVATE_MANIFEST_SHA256 = "b7c9baae2c3f0ed9b0773226ececf5bf129bd4dd487c02ffab9945cf0fc9c17d"
MAX_JSON_BYTES = 1024 * 1024
MAX_JSON_DEPTH = 64
MAX_JSON_NODES = 100_000
MAX_SIGNATURE_BYTES = 64 * 1024
SSH_TIMEOUT_SECONDS = 10
SYSTEM_SSH_KEYGEN_CANDIDATES = (
    pathlib.Path("/usr/bin/ssh-keygen"),
)
SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
SHA1_RE = re.compile(r"^[0-9a-f]{40}$")
RFC3339_RE = re.compile(
    r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,6})?(?:Z|\+\d{2}:\d{2}|-(?!00:00)\d{2}:\d{2})$"
)
BASE64_RE = re.compile(r"^[A-Za-z0-9+/]+={0,2}$")
PRINCIPAL_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._@+-]{0,127}$")
IDENTIFIER_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$")

ENVELOPE_FIELDS = {
    "schema_version",
    "profile",
    "canonicalization",
    "signature_scheme",
    "signature_namespace",
    "trust_root_id",
    "signer_principal",
    "public_key_sha256",
    "statement",
    "signed_payload_sha256",
    "signature",
}
STATEMENT_FIELDS = {
    "issued_at",
    "private_diagnostic",
    "public_projection",
    "production_inputs",
    "source_identity",
    "replay_result",
    "public_archive",
}
CHECKER_LOCK_PATHS = (
    "benchmarks/agent-brain/confirmatory/check_protocol.py",
    "benchmarks/agent-brain/confirmatory/hydrate_engine_evidence.py",
    "benchmarks/agent-brain/confirmatory/public_engine_evidence.py",
    "benchmarks/agent-brain/confirmatory/restricted_replay_attestation.py",
    "benchmarks/agent-brain/confirmatory/schemas/engine-evidence-storage-v1.schema.json",
    "benchmarks/agent-brain/confirmatory/schemas/engine-evidence-storage-v2.schema.json",
    "benchmarks/agent-brain/confirmatory/schemas/engine-replay-checker-lock-v1.schema.json",
    "benchmarks/agent-brain/confirmatory/schemas/engine-replay-trust-roots-v1.schema.json",
    "benchmarks/agent-brain/confirmatory/schemas/engine-restricted-replay-attestation-v1.schema.json",
    "benchmarks/agent-brain/confirmatory/schemas/engine-verification-manifest.schema.json",
    "benchmarks/agent-brain/confirmatory/schemas/engine-verification-public-v4.schema.json",
    "benchmarks/agent-brain/confirmatory/schemas/engine-verification.schema.json",
)


class AttestationError(RuntimeError):
    """Raised when an attestation or trust contract is not established."""


def _require(condition: bool, message: str) -> None:
    if not condition:
        raise AttestationError(message)


def _object_without_duplicate_keys(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise AttestationError(f"JSON contains duplicate key {key!r}")
        result[key] = value
    return result


def canonical_json_bytes(value: Any) -> bytes:
    """Return the only JSON encoding admitted by the signing contract."""
    _validate_json_types(value, "$canonical")
    try:
        return json.dumps(
            value,
            ensure_ascii=False,
            allow_nan=False,
            sort_keys=True,
            separators=(",", ":"),
        ).encode("utf-8")
    except (TypeError, ValueError) as exc:
        raise AttestationError(f"value is not canonicalizable JSON: {exc}") from exc


def _validate_json_types(value: Any, label: str) -> None:
    if value is None or type(value) in {bool, int, str}:
        return
    if isinstance(value, float):
        raise AttestationError(f"{label} contains a float prohibited by {CANONICALIZATION}")
    if isinstance(value, list):
        for index, item in enumerate(value):
            _validate_json_types(item, f"{label}[{index}]")
        return
    if isinstance(value, dict):
        for key, item in value.items():
            _require(type(key) is str, f"{label} contains a non-string object key")
            _validate_json_types(item, f"{label}.{key}")
        return
    raise AttestationError(f"{label} contains unsupported JSON type {type(value).__name__}")


def sha256_bytes(value: bytes) -> str:
    return hashlib.sha256(value).hexdigest()


def sha256_file(path: pathlib.Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _read_checker_source(path: pathlib.Path, label: str) -> bytes:
    try:
        metadata = path.lstat()
    except OSError as exc:
        raise AttestationError(f"{label} cannot be opened: {exc}") from exc
    _require(stat.S_ISREG(metadata.st_mode) and not path.is_symlink(), f"{label} must be one regular file")
    _require(metadata.st_size <= 16 * 1024 * 1024, f"{label} exceeds the checker-source size bound")
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
    except AttestationError:
        raise
    except OSError as exc:
        raise AttestationError(f"{label} cannot be read safely: {exc}") from exc
    return raw


def _read_regular_bytes(path: pathlib.Path, label: str, *, require_owner: bool = False) -> bytes:
    try:
        metadata = path.lstat()
    except OSError as exc:
        raise AttestationError(f"{label} cannot be opened: {exc}") from exc
    _require(stat.S_ISREG(metadata.st_mode) and not path.is_symlink(), f"{label} must be one regular file")
    if require_owner:
        _require(metadata.st_uid == os.getuid(), f"{label} must be owned by the current user")
        _require(stat.S_IMODE(metadata.st_mode) == 0o600, f"{label} must have mode 0600")
    _require(metadata.st_size <= MAX_JSON_BYTES, f"{label} exceeds the bounded JSON size")
    flags = os.O_RDONLY
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    try:
        descriptor = os.open(path, flags)
        try:
            current = os.fstat(descriptor)
            _require(stat.S_ISREG(current.st_mode), f"{label} must remain a regular file")
            _require(
                current.st_dev == metadata.st_dev
                and current.st_ino == metadata.st_ino
                and current.st_size == metadata.st_size,
                f"{label} changed while opening",
            )
            if require_owner:
                _require(current.st_uid == os.getuid(), f"{label} must remain owned by the current user")
                _require(stat.S_IMODE(current.st_mode) == 0o600, f"{label} must retain mode 0600")
            raw = os.read(descriptor, metadata.st_size + 1)
            _require(len(raw) == metadata.st_size, f"{label} changed while reading")
            _require(os.read(descriptor, 1) == b"", f"{label} changed while reading")
        finally:
            os.close(descriptor)
    except AttestationError:
        raise
    except OSError as exc:
        raise AttestationError(f"{label} cannot be read safely: {exc}") from exc
    return raw


def _decode_json_object(raw: bytes, label: str) -> dict[str, Any]:
    _require(len(raw) <= MAX_JSON_BYTES, f"{label} exceeds the bounded JSON size")
    try:
        value = json.loads(raw.decode("utf-8"), object_pairs_hook=_object_without_duplicate_keys)
    except AttestationError:
        raise
    except (UnicodeDecodeError, ValueError, RecursionError) as exc:
        raise AttestationError(f"{label} is not valid UTF-8 JSON: {exc}") from exc
    _require(isinstance(value, dict), f"{label} must be an object")
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


def _load_regular_json(path: pathlib.Path, label: str, *, require_owner: bool = False) -> dict[str, Any]:
    return _decode_json_object(_read_regular_bytes(path, label, require_owner=require_owner), label)


def _exact_object(value: Any, fields: set[str], label: str) -> Mapping[str, Any]:
    _require(isinstance(value, dict), f"{label} must be an object")
    _require(set(value) == fields, f"{label} must contain exactly {', '.join(sorted(fields))}")
    return value


def _sha256(value: Any, label: str) -> str:
    _require(isinstance(value, str) and SHA256_RE.fullmatch(value) is not None, f"{label} must be a lowercase SHA-256")
    return value


def _sha1(value: Any, label: str) -> str:
    _require(isinstance(value, str) and SHA1_RE.fullmatch(value) is not None, f"{label} must be a lowercase SHA-1")
    return value


def _positive_int(value: Any, label: str) -> int:
    _require(type(value) is int and value > 0, f"{label} must be a positive integer")
    return value


def _nonnegative_int(value: Any, label: str) -> int:
    _require(type(value) is int and value >= 0, f"{label} must be a non-negative integer")
    return value


def _exact_int(value: Any, expected: int, label: str) -> int:
    _require(type(value) is int and value == expected, f"{label} must equal integer {expected}")
    return value


def _nonempty(value: Any, label: str) -> str:
    _require(isinstance(value, str) and bool(value) and value == value.strip(), f"{label} must be a non-empty canonical string")
    _require("\x00" not in value and "\n" not in value and "\r" not in value, f"{label} contains a control separator")
    return value


def validate_principal(value: Any, label: str = "signer principal") -> str:
    """Return one literal allowed-signers principal token, never a pattern/options column."""
    _require(
        isinstance(value, str) and PRINCIPAL_RE.fullmatch(value) is not None,
        f"{label} must be one bounded literal ASCII principal token",
    )
    return value


def validate_identifier(value: Any, label: str = "identifier") -> str:
    """Return one bounded ASCII identifier with no SSH grammar metacharacters."""
    _require(
        isinstance(value, str) and IDENTIFIER_RE.fullmatch(value) is not None,
        f"{label} must be one bounded literal ASCII identifier",
    )
    return value


def _sshsig_armor(value: Any, label: str) -> str:
    _require(isinstance(value, str), f"{label} must be an ASCII-armored SSHSIG")
    try:
        raw = value.encode("ascii")
    except UnicodeEncodeError as exc:
        raise AttestationError(f"{label} must be ASCII") from exc
    _require(0 < len(raw) <= MAX_SIGNATURE_BYTES, f"{label} has an invalid size")
    _require("\r" not in value and not value.endswith("\n"), f"{label} must use canonical LF armor without a trailing newline")
    lines = value.split("\n")
    _require(
        len(lines) >= 3
        and lines[0] == "-----BEGIN SSH SIGNATURE-----"
        and lines[-1] == "-----END SSH SIGNATURE-----",
        f"{label} has invalid SSHSIG armor markers",
    )
    _require(
        all(bool(line) and BASE64_RE.fullmatch(line) is not None for line in lines[1:-1]),
        f"{label} has invalid SSHSIG armor payload",
    )
    return value


def _parse_time(value: Any, label: str) -> dt.datetime:
    _require(isinstance(value, str) and RFC3339_RE.fullmatch(value) is not None, f"{label} must be RFC3339")
    try:
        parsed = dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError as exc:
        raise AttestationError(f"{label} must be RFC3339") from exc
    _require(parsed.tzinfo is not None, f"{label} must include a timezone")
    return parsed.astimezone(dt.UTC)


def public_key_line(public_key_base64: str) -> bytes:
    _require(BASE64_RE.fullmatch(public_key_base64) is not None, "public key base64 is invalid")
    try:
        decoded = base64.b64decode(public_key_base64, validate=True)
    except ValueError as exc:
        raise AttestationError("public key base64 is invalid") from exc
    _require(len(decoded) >= 4, "public key SSH wire blob is truncated")
    algorithm_size = struct.unpack(">I", decoded[:4])[0]
    algorithm_end = 4 + algorithm_size
    _require(algorithm_end + 4 <= len(decoded), "public key SSH wire algorithm is truncated")
    _require(decoded[4:algorithm_end] == b"ssh-ed25519", "public key SSH wire algorithm is not ssh-ed25519")
    key_size = struct.unpack(">I", decoded[algorithm_end : algorithm_end + 4])[0]
    key_start = algorithm_end + 4
    _require(key_size == 32, "Ed25519 public key must contain exactly 32 key bytes")
    _require(key_start + key_size == len(decoded), "public key SSH wire blob has trailing or truncated bytes")
    return b"ssh-ed25519 " + public_key_base64.encode("ascii") + b"\n"


def _validate_trust_roots(value: dict[str, Any]) -> dict[str, Any]:
    fields = {"schema_version", "profile", "status", "signature_namespace", "roots"}
    _exact_object(value, fields, "engine replay trust roots")
    _exact_int(value["schema_version"], 1, "trust roots schema version")
    _require(value["profile"] == TRUST_PROFILE, "trust roots profile changed")
    _require(value["status"] in {"pending_owner_authorization", "approved"}, "trust roots status is invalid")
    _require(value["signature_namespace"] == SIGNATURE_NAMESPACE, "trust roots namespace changed")
    roots = value["roots"]
    _require(isinstance(roots, list), "trust roots must be an array")
    _require(value["status"] != "pending_owner_authorization" or roots == [], "pending trust roots must be empty")
    seen_ids: set[str] = set()
    seen_principals: set[str] = set()
    for index, row in enumerate(roots):
        label = f"trust roots[{index}]"
        root = _exact_object(
            row,
            {
                "root_id", "principal", "key_type", "public_key_base64", "public_key_sha256",
                "status", "not_before", "not_after", "revoked_at",
            },
            label,
        )
        root_id = validate_identifier(root["root_id"], f"{label}.root_id")
        principal = validate_principal(root["principal"], f"{label}.principal")
        _require(root_id not in seen_ids and principal not in seen_principals, f"{label} duplicates an identity")
        seen_ids.add(root_id)
        seen_principals.add(principal)
        _require(root["key_type"] == "ssh-ed25519", f"{label}.key_type must be ssh-ed25519")
        expected_key_hash = sha256_bytes(public_key_line(_nonempty(root["public_key_base64"], f"{label}.public_key_base64")))
        _require(root["public_key_sha256"] == expected_key_hash, f"{label}.public_key_sha256 differs")
        _require(root["status"] in {"active", "revoked"}, f"{label}.status is invalid")
        before = _parse_time(root["not_before"], f"{label}.not_before")
        after = _parse_time(root["not_after"], f"{label}.not_after")
        _require(before < after, f"{label} validity window is empty")
        revoked = root["revoked_at"]
        if root["status"] == "active":
            _require(revoked is None, f"{label}.revoked_at must be null while active")
        else:
            revoked_time = _parse_time(revoked, f"{label}.revoked_at")
            _require(before <= revoked_time <= after, f"{label}.revoked_at is outside the validity window")
    _require(value["status"] != "approved" or bool(roots), "approved trust roots must not be empty")
    return value


def load_trust_roots_bytes(raw: bytes) -> dict[str, Any]:
    return _validate_trust_roots(_decode_json_object(raw, "engine replay trust roots"))


def load_trust_roots(path: pathlib.Path = TRUST_ROOTS_PATH) -> dict[str, Any]:
    return load_trust_roots_bytes(_read_regular_bytes(path, "engine replay trust roots"))


def checker_lock_aggregate(rows: Sequence[Mapping[str, Any]]) -> str:
    digest = hashlib.sha256()
    for row in rows:
        path = _nonempty(row.get("path"), "checker lock path")
        sha = _sha256(row.get("sha256"), "checker lock sha256")
        digest.update(path.encode("utf-8"))
        digest.update(b"\0")
        digest.update(sha.encode("ascii"))
        digest.update(b"\n")
    return digest.hexdigest()


def _validate_checker_lock(value: dict[str, Any], *, repo: pathlib.Path) -> dict[str, Any]:
    _exact_object(value, {"schema_version", "profile", "algorithm", "files", "aggregate_sha256"}, "engine replay checker lock")
    _exact_int(value["schema_version"], 1, "checker lock schema version")
    _require(value["profile"] == CHECKER_LOCK_PROFILE, "checker lock profile changed")
    _require(value["algorithm"] == CHECKER_LOCK_ALGORITHM, "checker lock algorithm changed")
    rows = value["files"]
    _require(isinstance(rows, list) and len(rows) == 12, "checker lock must contain exactly 12 files")
    paths: list[str] = []
    for index, row in enumerate(rows):
        item = _exact_object(row, {"path", "sha256"}, f"checker lock files[{index}]")
        relative = _nonempty(item["path"], f"checker lock files[{index}].path")
        _sha256(item["sha256"], f"checker lock files[{index}].sha256")
        pure = pathlib.PurePosixPath(relative)
        _require(not pure.is_absolute() and all(part not in {"", ".", ".."} for part in pure.parts), f"checker lock files[{index}].path is unsafe")
        target = repo.joinpath(*pure.parts)
        _require(target.is_file() and not target.is_symlink(), f"checker lock file is missing or redirected: {relative}")
        _require(sha256_bytes(_read_checker_source(target, f"checker lock source {relative}")) == item["sha256"], f"checker lock file hash differs: {relative}")
        paths.append(relative)
    _require(tuple(paths) == CHECKER_LOCK_PATHS, "checker lock paths differ from the frozen 12-path set")
    _require(value["aggregate_sha256"] == checker_lock_aggregate(rows), "checker lock aggregate differs")
    return value


def load_checker_lock_bytes(raw: bytes, *, repo: pathlib.Path = REPO) -> dict[str, Any]:
    return _validate_checker_lock(_decode_json_object(raw, "engine replay checker lock"), repo=repo)


def load_checker_lock(path: pathlib.Path = CHECKER_LOCK_PATH, *, repo: pathlib.Path = REPO) -> dict[str, Any]:
    return load_checker_lock_bytes(_read_regular_bytes(path, "engine replay checker lock"), repo=repo)


def validate_statement(statement: Any) -> Mapping[str, Any]:
    _validate_json_types(statement, "attestation statement")
    value = _exact_object(statement, STATEMENT_FIELDS, "attestation statement")
    issued_at = _parse_time(value["issued_at"], "statement.issued_at")

    private = _exact_object(value["private_diagnostic"], {"manifest_schema_version", "manifest_sha256", "manifest_size_bytes"}, "statement.private_diagnostic")
    _exact_int(private["manifest_schema_version"], 2, "private diagnostic schema version")
    _require(private["manifest_sha256"] == PRIVATE_MANIFEST_SHA256, "private diagnostic manifest commitment changed")
    _positive_int(private["manifest_size_bytes"], "private diagnostic manifest size")

    projection = _exact_object(
        value["public_projection"],
        {
            "manifest_schema_version", "manifest_sha256", "manifest_size_bytes",
            "artifact_inventory_algorithm", "artifact_inventory_root_sha256",
            "payload_file_count", "payload_logical_bytes",
        },
        "statement.public_projection",
    )
    _exact_int(projection["manifest_schema_version"], 4, "public projection schema version")
    _sha256(projection["manifest_sha256"], "public projection manifest hash")
    _positive_int(projection["manifest_size_bytes"], "public projection manifest size")
    _require(projection["artifact_inventory_algorithm"] == PUBLIC_INVENTORY_ALGORITHM, "public projection inventory algorithm changed")
    _sha256(projection["artifact_inventory_root_sha256"], "public projection inventory root")
    _exact_int(projection["payload_file_count"], PUBLIC_PAYLOAD_FILE_COUNT, "public projection payload count")
    _positive_int(projection["payload_logical_bytes"], "public projection payload bytes")

    inputs = _exact_object(
        value["production_inputs"],
        {"pin_set_id", "pin_set_authority", "pin_set_path", "pin_set_sha256", "matrix_path", "matrix_sha256"},
        "statement.production_inputs",
    )
    _nonempty(inputs["pin_set_id"], "production pin set id")
    _require(inputs["pin_set_authority"] == "production", "production pin authority changed")
    _require(inputs["pin_set_path"] == PIN_PATH, "production pin path changed")
    _sha256(inputs["pin_set_sha256"], "production pin hash")
    _require(inputs["matrix_path"] == MATRIX_PATH, "engine matrix path changed")
    _sha256(inputs["matrix_sha256"], "engine matrix hash")

    source = _exact_object(
        value["source_identity"],
        {
            "repository", "git_object_format", "commit_oid", "tree_oid", "worktree_state",
            "checker_lock_path", "checker_lock_sha256", "checker_lock_algorithm",
            "checker_source_aggregate_sha256", "analyzer_lock_path", "analyzer_lock_sha256",
            "analyzer_lock_algorithm",
            "analyzer_source_aggregate_sha256",
        },
        "statement.source_identity",
    )
    _require(source["repository"] == REPOSITORY, "source repository changed")
    _require(source["git_object_format"] == "sha1", "source Git object format changed")
    source_commit = _sha1(source["commit_oid"], "source commit")
    _sha1(source["tree_oid"], "source tree")
    _require(source["worktree_state"] == "clean", "source worktree was not clean")
    _require(source["checker_lock_path"] == CHECKER_LOCK_REPO_PATH, "checker lock path changed")
    _sha256(source["checker_lock_sha256"], "checker lock hash")
    _require(source["checker_lock_algorithm"] == CHECKER_LOCK_ALGORITHM, "checker lock source algorithm changed")
    _sha256(source["checker_source_aggregate_sha256"], "checker source aggregate")
    _require(source["analyzer_lock_path"] == ANALYZER_LOCK_REPO_PATH, "analyzer lock path changed")
    _sha256(source["analyzer_lock_sha256"], "analyzer lock hash")
    _require(source["analyzer_lock_algorithm"] == CHECKER_LOCK_ALGORITHM, "analyzer source algorithm changed")
    _sha256(source["analyzer_source_aggregate_sha256"], "analyzer source aggregate")

    replay = _exact_object(
        value["replay_result"],
        {
            "operation", "private_validator", "private_error_count", "public_validator",
            "public_error_count", "checker_lock_valid", "analyzer_lock_valid", "decision",
        },
        "statement.replay_result",
    )
    _require(replay["operation"] == "offline_exact_byte_validation_v1", "replay operation changed")
    _require(replay["private_validator"] == "check_protocol.validate_engine_verification/v2", "private validator changed")
    _require(replay["public_validator"] == "public_engine_evidence.validate_public_bundle/v4", "public validator changed")
    _exact_int(replay["private_error_count"], 0, "private replay error count")
    _exact_int(replay["public_error_count"], 0, "public replay error count")
    _require(replay["checker_lock_valid"] is True, "replay checker lock was not valid")
    _require(replay["analyzer_lock_valid"] is True, "replay analyzer lock was not valid")
    _require(replay["decision"] == "pass", "replay decision did not pass")

    archive = _exact_object(
        value["public_archive"],
        {
            "format", "root", "sha256", "size_bytes", "regular_file_count", "directory_count",
            "logical_bytes", "tree_inventory_algorithm",
            "tree_inventory_sha256", "repository", "tag", "asset_name", "asset_url",
            "release_id", "asset_id", "release_immutable", "release_target_commitish",
            "asset_api_digest", "verified_at",
        },
        "statement.public_archive",
    )
    _require(archive["format"] == "tar_zstd", "public archive format changed")
    _require(archive["root"] == PUBLIC_ROOT, "public archive root changed")
    archive_sha = _sha256(archive["sha256"], "public archive hash")
    _positive_int(archive["size_bytes"], "public archive size")
    _exact_int(archive["regular_file_count"], PUBLIC_REGULAR_FILE_COUNT, "public archive regular-file count")
    _exact_int(archive["directory_count"], PUBLIC_DIRECTORY_COUNT, "public archive directory count")
    _positive_int(archive["logical_bytes"], "public archive logical bytes")
    _require(archive["tree_inventory_algorithm"] == TREE_INVENTORY_ALGORITHM, "public archive inventory algorithm changed")
    _sha256(archive["tree_inventory_sha256"], "public archive inventory hash")
    _require(archive["repository"] == REPOSITORY, "public archive repository changed")
    tag = _nonempty(archive["tag"], "public archive tag")
    asset_name = _nonempty(archive["asset_name"], "public archive asset name")
    _require("/" not in tag and "/" not in asset_name and asset_name.endswith(".tar.zst"), "public archive release names are unsafe")
    expected_url = f"https://github.com/{REPOSITORY}/releases/download/{tag}/{asset_name}"
    _require(archive["asset_url"] == expected_url, "public archive URL is not canonical")
    _positive_int(archive["release_id"], "public archive release id")
    _positive_int(archive["asset_id"], "public archive asset id")
    _require(archive["release_immutable"] is True, "public archive release is not immutable")
    _require(archive["release_target_commitish"] == source_commit, "public archive target differs from source commit")
    _require(archive["asset_api_digest"] == f"sha256:{archive_sha}", "public archive API digest differs from archive hash")
    verified_at = _parse_time(archive["verified_at"], "public archive verified_at")
    _require(issued_at >= verified_at, "attestation was issued before release verification")
    return value


def unsigned_envelope(envelope: Mapping[str, Any]) -> dict[str, Any]:
    _exact_object(envelope, ENVELOPE_FIELDS, "restricted replay attestation")
    return {key: envelope[key] for key in sorted(ENVELOPE_FIELDS - {"signed_payload_sha256", "signature"})}


def signed_payload(envelope: Mapping[str, Any]) -> bytes:
    return PAYLOAD_DOMAIN + canonical_json_bytes(unsigned_envelope(envelope))


def _validate_envelope_structure(envelope: Any) -> Mapping[str, Any]:
    value = _exact_object(envelope, ENVELOPE_FIELDS, "restricted replay attestation")
    _exact_int(value["schema_version"], 1, "attestation schema version")
    _require(value["profile"] == PROFILE, "attestation profile changed")
    _require(value["canonicalization"] == CANONICALIZATION, "attestation canonicalization changed")
    _require(value["signature_scheme"] == SIGNATURE_SCHEME, "attestation signature scheme changed")
    _require(value["signature_namespace"] == SIGNATURE_NAMESPACE, "attestation signature namespace changed")
    validate_identifier(value["trust_root_id"], "attestation trust root id")
    validate_principal(value["signer_principal"], "attestation signer principal")
    _sha256(value["public_key_sha256"], "attestation public key hash")
    validate_statement(value["statement"])
    _sha256(value["signed_payload_sha256"], "attestation signed payload hash")
    _sshsig_armor(value["signature"], "attestation signature")
    payload = signed_payload(value)
    _require(value["signed_payload_sha256"] == sha256_bytes(payload), "attestation signed payload hash differs")
    return value


def _validate_system_ssh_keygen_candidate(path: pathlib.Path) -> pathlib.Path:
    """Admit only an immutable-by-unprivileged-users system executable path."""
    _require(path.is_absolute(), "trusted ssh-keygen candidate is not absolute")
    _require(path == pathlib.Path(os.path.normpath(str(path))), "trusted ssh-keygen candidate is not normalized")
    current = pathlib.Path(path.anchor)
    parts = path.parts[1:]
    _require(bool(parts), "trusted ssh-keygen candidate has no path components")
    try:
        root_metadata = current.lstat()
    except OSError as exc:
        raise AttestationError(f"trusted ssh-keygen root cannot be inspected: {current}: {exc}") from exc
    _require(stat.S_ISDIR(root_metadata.st_mode) and not stat.S_ISLNK(root_metadata.st_mode), "trusted ssh-keygen filesystem root is redirected")
    _require(root_metadata.st_uid == 0, "trusted ssh-keygen filesystem root is not root-owned")
    _require(stat.S_IMODE(root_metadata.st_mode) & 0o022 == 0, "trusted ssh-keygen filesystem root is group/world-writable")
    for index, part in enumerate(parts):
        current = current / part
        try:
            metadata = current.lstat()
        except OSError as exc:
            raise AttestationError(f"trusted ssh-keygen path cannot be inspected: {current}: {exc}") from exc
        _require(not stat.S_ISLNK(metadata.st_mode), f"trusted ssh-keygen path traverses a symlink: {current}")
        _require(metadata.st_uid == 0, f"trusted ssh-keygen path is not root-owned: {current}")
        _require(
            stat.S_IMODE(metadata.st_mode) & 0o022 == 0,
            f"trusted ssh-keygen path is group/world-writable: {current}",
        )
        if index == len(parts) - 1:
            _require(stat.S_ISREG(metadata.st_mode), "trusted ssh-keygen is not one regular file")
            _require(stat.S_IMODE(metadata.st_mode) & 0o111 != 0, "trusted ssh-keygen is not executable")
        else:
            _require(stat.S_ISDIR(metadata.st_mode), f"trusted ssh-keygen ancestor is not a directory: {current}")
    return path


def _resolve_system_ssh_keygen() -> pathlib.Path:
    """Resolve production ssh-keygen without consulting ambient PATH."""
    failures: list[str] = []
    for candidate in SYSTEM_SSH_KEYGEN_CANDIDATES:
        try:
            return _validate_system_ssh_keygen_candidate(candidate)
        except AttestationError as exc:
            failures.append(str(exc))
    detail = "; ".join(failures) if failures else "no frozen candidates"
    raise AttestationError(f"trusted system ssh-keygen is unavailable: {detail}")


def _resolve_fixture_ssh_keygen(executable: str | None = None) -> pathlib.Path:
    """Allow an explicit executable only for synthetic fixture issuance."""
    if executable is None:
        return _resolve_system_ssh_keygen()
    path = pathlib.Path(executable).expanduser()
    _require(path.is_absolute(), "fixture ssh-keygen override must be absolute")
    try:
        metadata = path.lstat()
    except OSError as exc:
        raise AttestationError(f"fixture ssh-keygen override cannot be inspected: {exc}") from exc
    _require(stat.S_ISREG(metadata.st_mode) and not stat.S_ISLNK(metadata.st_mode), "fixture ssh-keygen override is not one regular executable")
    _require(stat.S_IMODE(metadata.st_mode) & 0o111 != 0, "fixture ssh-keygen override is not executable")
    return path


def _ssh_environment(executable: pathlib.Path) -> dict[str, str]:
    return {"LC_ALL": "C", "LANG": "C", "PATH": str(executable.parent)}


def _run_ssh(args: Sequence[str], *, executable: pathlib.Path, stdin: bytes | None = None) -> subprocess.CompletedProcess[bytes]:
    try:
        result = subprocess.run(
            [str(executable), *args],
            input=stdin,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            shell=False,
            check=False,
            timeout=SSH_TIMEOUT_SECONDS,
            env=_ssh_environment(executable),
        )
    except (OSError, subprocess.TimeoutExpired) as exc:
        raise AttestationError(f"offline SSHSIG operation failed: {exc}") from exc
    _require(result.returncode == 0, f"offline SSHSIG operation was rejected: {result.stderr.decode('utf-8', 'replace').strip()}")
    return result


def sign_envelope(
    statement: Mapping[str, Any],
    *,
    trust_root_id: str,
    signer_principal: str,
    public_key_base64: str,
    private_key: pathlib.Path,
    ssh_keygen: str | None = None,
) -> dict[str, Any]:
    """Create a fixture envelope; no key or principal is read from storage metadata."""
    validate_statement(statement)
    key_line = public_key_line(public_key_base64)
    try:
        metadata = private_key.lstat()
    except OSError as exc:
        raise AttestationError(f"private key cannot be opened: {exc}") from exc
    _require(stat.S_ISREG(metadata.st_mode) and not private_key.is_symlink(), "private key must be one regular file")
    _require(metadata.st_uid == os.getuid(), "private key must be owned by the current user")
    _require(stat.S_IMODE(metadata.st_mode) & 0o077 == 0, "private key permissions are too broad")
    executable = _resolve_fixture_ssh_keygen(ssh_keygen)
    envelope: dict[str, Any] = {
        "schema_version": 1,
        "profile": PROFILE,
        "canonicalization": CANONICALIZATION,
        "signature_scheme": SIGNATURE_SCHEME,
        "signature_namespace": SIGNATURE_NAMESPACE,
        "trust_root_id": validate_identifier(trust_root_id, "trust root id"),
        "signer_principal": validate_principal(signer_principal, "signer principal"),
        "public_key_sha256": sha256_bytes(key_line),
        "statement": dict(statement),
        "signed_payload_sha256": "0" * 64,
        "signature": "pending",
    }
    payload = signed_payload(envelope)
    envelope["signed_payload_sha256"] = sha256_bytes(payload)
    with tempfile.TemporaryDirectory(prefix="engine-replay-sign-") as raw_temp:
        temporary = pathlib.Path(raw_temp)
        temporary.chmod(0o700)
        payload_path = temporary / "payload"
        payload_path.write_bytes(payload)
        payload_path.chmod(0o600)
        _run_ssh(
            ["-Y", "sign", "-f", str(private_key.resolve()), "-n", SIGNATURE_NAMESPACE, str(payload_path)],
            executable=executable,
        )
        signature_path = temporary / "payload.sig"
        _require(signature_path.is_file() and not signature_path.is_symlink(), "ssh-keygen did not create one signature file")
        _require(signature_path.stat().st_size <= MAX_SIGNATURE_BYTES, "generated SSHSIG is too large")
        signature = signature_path.read_text(encoding="ascii")
    envelope["signature"] = signature.rstrip("\n")
    _validate_envelope_structure(envelope)
    return envelope


def verify_envelope(
    envelope: Mapping[str, Any],
    trust_roots: Mapping[str, Any],
    *,
    verification_time: dt.datetime | None = None,
) -> None:
    """Verify structure, trust-root authorization, validity, and SSHSIG bytes."""
    value = _validate_envelope_structure(envelope)
    _exact_int(trust_roots.get("schema_version"), 1, "trust roots schema version")
    _require(trust_roots.get("profile") == TRUST_PROFILE, "trust roots identity changed")
    _require(trust_roots.get("status") == "approved", "engine replay trust roots are pending owner authorization")
    _require(trust_roots.get("signature_namespace") == SIGNATURE_NAMESPACE, "trust roots namespace changed")
    rows = trust_roots.get("roots")
    _require(isinstance(rows, list), "trust roots are missing")
    matches = [row for row in rows if isinstance(row, dict) and row.get("root_id") == value["trust_root_id"]]
    _require(len(matches) == 1, "attestation trust root is missing or ambiguous")
    root = matches[0]
    validate_identifier(root.get("root_id"), "trust root id")
    validate_principal(root.get("principal"), "trust root principal")
    _require(root.get("principal") == value["signer_principal"], "attestation signer principal differs from trust root")
    _require(root.get("key_type") == "ssh-ed25519", "attestation trust root is not Ed25519")
    key_line = public_key_line(_nonempty(root.get("public_key_base64"), "trust root public key"))
    key_hash = sha256_bytes(key_line)
    _require(root.get("public_key_sha256") == key_hash == value["public_key_sha256"], "attestation public key differs from trust root")
    _require(root.get("status") == "active" and root.get("revoked_at") is None, "attestation trust root is not active")
    issued_at = _parse_time(value["statement"]["issued_at"], "statement.issued_at")
    _require(_parse_time(root.get("not_before"), "trust root not_before") <= issued_at <= _parse_time(root.get("not_after"), "trust root not_after"), "attestation issuance is outside the trust-root validity window")
    if verification_time is not None:
        current = verification_time.astimezone(dt.UTC)
        _require(current <= _parse_time(root.get("not_after"), "trust root not_after"), "trust root is expired at verification time")
    executable = _resolve_system_ssh_keygen()
    payload = signed_payload(value)
    with tempfile.TemporaryDirectory(prefix="engine-replay-verify-") as raw_temp:
        temporary = pathlib.Path(raw_temp)
        temporary.chmod(0o700)
        allowed = temporary / "allowed_signers"
        allowed.write_bytes(value["signer_principal"].encode("utf-8") + b" " + key_line)
        allowed.chmod(0o600)
        signature = temporary / "signature"
        signature.write_text(value["signature"] + "\n", encoding="ascii")
        signature.chmod(0o600)
        _run_ssh(
            ["-Y", "verify", "-f", str(allowed), "-I", value["signer_principal"], "-n", SIGNATURE_NAMESPACE, "-s", str(signature)],
            executable=executable,
            stdin=payload,
        )


def load_attestation(path: pathlib.Path, *, require_owner: bool = True) -> dict[str, Any]:
    raw = _read_regular_bytes(path, "restricted replay attestation", require_owner=require_owner)
    return load_attestation_bytes(raw)


def load_attestation_bytes(raw: bytes) -> dict[str, Any]:
    _require(isinstance(raw, bytes), "restricted replay attestation buffer must be bytes")
    value = _decode_json_object(raw, "restricted replay attestation")
    _validate_envelope_structure(value)
    _require(
        raw == canonical_json_bytes(value) + b"\n",
        "restricted replay attestation file is not canonical JSON with one LF",
    )
    return value


def _load_public_key(path: pathlib.Path) -> str:
    raw = path.read_text(encoding="ascii").strip().split()
    _require(len(raw) >= 2 and raw[0] == "ssh-ed25519", "public key file must contain an ssh-ed25519 key")
    public_key_line(raw[1])
    return raw[1]


def _write_new_json(path: pathlib.Path, value: Mapping[str, Any], *, mode: int = 0o600) -> None:
    _require(not os.path.lexists(path), f"refusing to replace existing output: {path}")
    path.parent.mkdir(parents=True, exist_ok=True)
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    descriptor = os.open(path, flags, mode)
    try:
        payload = canonical_json_bytes(value) + b"\n"
        with os.fdopen(descriptor, "wb", closefd=False) as handle:
            handle.write(payload)
            handle.flush()
            os.fsync(handle.fileno())
    finally:
        os.close(descriptor)
    path.chmod(mode)


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    inspect = commands.add_parser("inspect", help="validate attestation structure without asserting trust")
    inspect.add_argument("attestation", type=pathlib.Path)
    issue = commands.add_parser("issue", help="issue one explicitly synthetic fixture attestation")
    issue.add_argument("--synthetic-fixture", action="store_true", required=True)
    issue.add_argument("--statement", type=pathlib.Path, required=True)
    issue.add_argument("--private-key", type=pathlib.Path, required=True)
    issue.add_argument("--public-key", type=pathlib.Path, required=True)
    issue.add_argument("--trust-root-id", required=True)
    issue.add_argument("--principal", required=True)
    issue.add_argument("--output", type=pathlib.Path, required=True)
    verify = commands.add_parser("verify", help="verify one attestation using checked-in trust roots")
    verify.add_argument("attestation", type=pathlib.Path)
    verify.add_argument("--trust-roots", type=pathlib.Path, default=TRUST_ROOTS_PATH)
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    args = _parser().parse_args(argv)
    try:
        if args.command == "inspect":
            envelope = load_attestation(args.attestation)
            print(json.dumps({"profile": envelope["profile"], "trust_root_id": envelope["trust_root_id"], "structure": "valid"}, sort_keys=True))
        elif args.command == "issue":
            statement = _load_regular_json(args.statement, "synthetic fixture statement", require_owner=True)
            envelope = sign_envelope(
                statement,
                trust_root_id=args.trust_root_id,
                signer_principal=args.principal,
                public_key_base64=_load_public_key(args.public_key),
                private_key=args.private_key,
            )
            _write_new_json(args.output, envelope)
            print(args.output)
        else:
            envelope = load_attestation(args.attestation)
            trust_roots = load_trust_roots(args.trust_roots)
            verify_envelope(envelope, trust_roots)
            print(json.dumps({"profile": envelope["profile"], "signature": "valid"}, sort_keys=True))
    except AttestationError as exc:
        raise SystemExit(f"error: {exc}") from exc
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
