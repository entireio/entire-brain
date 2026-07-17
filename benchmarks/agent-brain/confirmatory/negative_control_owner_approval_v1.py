#!/usr/bin/env python3
"""Build, check, and verify the non-authorizing owner-approval gate.

The checked-in trust-root contract is deliberately pending and empty, so the
production ``verify`` command currently fails closed before invoking SSHSIG.
This module has no signing, key-generation, authorization, consumption, run,
candidate, model/provider, or network surface.  Even a future valid signature
produces only a non-persistent verification report whose execution authority
is false until every remaining gate and atomic single-use consumption exist.
"""

from __future__ import annotations

import argparse
import base64
import binascii
import copy
import datetime as dt
import hashlib
import json
import os
import pathlib
import re
import signal
import stat
import struct
import subprocess
import sys
import tempfile
import types
from collections.abc import Mapping, Sequence
from typing import Any, cast


ROOT = pathlib.Path(__file__).parent
EXECUTION_CONTRACT_PATH = ROOT / "development-task-negative-control-execution-contract-v1.json"
TRUST_ROOTS_PATH = ROOT / "negative-control-owner-approval-trust-roots-v1.json"
VERIFIER_CONTRACT_PATH = ROOT / "negative-control-owner-approval-verifier-contract-v1.json"
APPROVAL_SCHEMA_PATH = ROOT / "schemas" / "negative-control-owner-approval-envelope-v1.schema.json"
TRUST_ROOTS_SCHEMA_PATH = ROOT / "schemas" / "negative-control-owner-approval-trust-roots-v1.schema.json"
REPORT_SCHEMA_PATH = ROOT / "schemas" / "negative-control-owner-approval-verification-report-v1.schema.json"
VERIFIER_CONTRACT_SCHEMA_PATH = ROOT / "schemas" / "negative-control-owner-approval-verifier-contract-v1.schema.json"
DRAFT202012_PATH = ROOT / "draft202012.py"
SSH_KEYGEN_PATH = pathlib.Path("/usr/bin/ssh-keygen")

SCHEMA_VERSION = 1
VERIFIER_PROFILE = "agent_brain_negative_control_owner_approval_verifier_contract_v1"
TRUST_PROFILE = "agent_brain_negative_control_owner_approval_trust_roots_v1"
APPROVAL_PROFILE = "agent_brain_negative_control_owner_approval_envelope_v1"
STATEMENT_PROFILE = "agent_brain_negative_control_owner_approval_statement_v1"
REPORT_PROFILE = "agent_brain_negative_control_owner_approval_verification_report_v1"
PENDING_STATUS = "verifier_compiled_trust_root_pending_execution_forbidden"
APPROVED_STATUS = "verifier_compiled_trust_root_approved_execution_forbidden"
EXECUTION_STATUS = "forbidden_missing_owner_approval_remaining_gates_and_atomic_consumption"
AFTER_VERIFICATION_STATUS = "forbidden_missing_remaining_gates_and_atomic_approval_consumption"
CANONICAL_ARTIFACT_PROFILE = "sorted_keys_indent_2_utf8_lf_v1"
CANONICAL_SIGNED_PROFILE = "sorted_keys_compact_utf8_no_float_v1"
SIGNATURE_SCHEME = "sshsig_ed25519_sha512_v1"
SIGNATURE_NAMESPACE = "entire-brain-negative-control-approval-v1"
PAYLOAD_DOMAIN = b"entire-brain/negative-control-execution-approval/v1\0"

CHECKED_EXECUTION_SOURCE_COMMIT = "f0552070921605cd9a0165ee29ca96e100675d58"
CHECKED_EXECUTION_ARTIFACT_SHA256 = "94422af8a5e24c6a855b66cf40868565ee3518d0c75eb5343b5d2450523b2c1e"
CHECKED_EXECUTION_CONTRACT_SHA256 = "1e290b91d5e0be42f750bf0e655edee48d297f86ad3e3867b8fe9177fd5125ef"
CHECKED_EXECUTION_BUILDER_SHA256 = "f3cfb82978b0ce98d17999ef4cb4f4089a425bb528ece4ff53b61a03a4344bcd"
CHECKED_EXECUTION_SCHEMA_SHA256 = "dbcb95119feeda4c436e90d8162ac0498577454ca9ccaab7a8254d55d90359f9"
CHECKED_CANDIDATE_BINDINGS_SHA256 = "4fbde9876d4c91e9cedcf994de9cecd5442beee928c1cadc505116a25c2bef60"
CHECKED_SCHEDULE_SHA256 = "c818e8ad87f4d6ff5fac8839f4296fd86da13eaf95dadf8b2c789c7254bfdbcd"
CHECKED_PROTOCOL_SHA256 = "3e5cef5c19836b61f5739347f4d0db25564b859cd26fc6c5761f81072b2f3f01"
CHECKED_STATE_MACHINE_SHA256 = "31015d9150afaf6c73cbea84dc647039449cc5bfc498ac0efedddfabad271207"
CHECKED_ENFORCEMENT_SHA256 = "c23f32b102077254ef4eae322faa12101e3e24d688808ebf6bca31cf128185e2"
CHECKED_RESIDUAL_GATES_SHA256 = "650f570c5fe096626381171dd6d9d2f0ca77c4a6c13e6994c783ee3a82d5cb91"
CHECKED_AUTHORITY_SHA256 = "422d6742432af499f9aec5e6b7ecc9d86ac0cf901e2accd0fedb611c580176de"
CHECKED_RUNTIME_BINDINGS_SHA256 = "938f7a76faf1d0a3490db06fbb5ac5b1100f2967a24b0bc846ce40c3c762b634"
CHECKED_TRUST_ROOTS_FILE_SHA256 = "07b515dddf74c53872c01f69cf2b1076820a10782dd9102cf235cf9691d89f1b"
CHECKED_TRUST_ROOTS_SHA256 = "b3374aff490518f3f1eb142428810bb7791aeaf5a6a05cf16f63455d02908b37"
CHECKED_SSH_KEYGEN_SHA256 = "bddae9c4ea46fd903574ec6ff61eda75e133f940fa538f2adca80af474767596"
CHECKED_SSH_KEYGEN_SIZE = 849_024
CHECKED_DRAFT202012_SHA256 = "8688b67468b096427f758163174432e221d8197c6b13870a6427939f6ea281eb"
CHECKED_DRAFT202012_SIZE = 18_097

# Updated only through reviewed source changes when any schema changes.
CHECKED_APPROVAL_SCHEMA_SHA256 = "1300806efed4cedfccbbd68291676f71eabfc1296dbbf054a721e18a20a4d675"
CHECKED_TRUST_ROOTS_SCHEMA_SHA256 = "54dd9ccd634863cfbc050221d2394b54326e03a6e465ebf807e358113f37158d"
CHECKED_REPORT_SCHEMA_SHA256 = "352ceeb16bc398c4241c0c3347b021a08c4ea16948f28d9d34e28965257e1594"
CHECKED_VERIFIER_CONTRACT_SCHEMA_SHA256 = "e3b8e95e62cda252434e11bf190e4f248526486ba3c1f63415ce78d78f2b5707"

MAX_APPROVAL_BYTES = 128 * 1024
MAX_TRUST_ROOTS_BYTES = 64 * 1024
MAX_ARTIFACT_BYTES = 8 * 1024 * 1024
MAX_COMPONENT_BYTES = 4 * 1024 * 1024
MAX_BINARY_BYTES = 2 * 1024 * 1024
MAX_SIGNATURE_BYTES = 16 * 1024
MAX_SIGNED_PAYLOAD_BYTES = 64 * 1024
MAX_JSON_DEPTH = 32
MAX_JSON_NODES = 20_000
MAX_INTEGER_DIGITS = 32
MAX_APPROVAL_LIFETIME_SECONDS = 900
MAX_ISSUANCE_TO_ACTIVATION_SECONDS = 60
MAX_ROOT_LIFETIME_SECONDS = 366 * 24 * 60 * 60
SSH_TIMEOUT_SECONDS = 10
SSHSIG_ARMOR_WIDTH = 70

SHA256_RE = re.compile(r"^(?!0{64}$)[0-9a-f]{64}$")
IDENTIFIER_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$")
PRINCIPAL_RE = re.compile(
    r"^entire-brain-negative-control-owner-[a-z0-9][a-z0-9._-]{0,63}$"
)
UTC_SECOND_RE = re.compile(r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$")
BASE64_RE = re.compile(r"^[A-Za-z0-9+/]+={0,2}$")

_CHECKED_DRAFT_VALIDATOR: types.ModuleType | None = None


class ApprovalVerificationError(RuntimeError):
    """Raised when an approval or a verifier dependency fails closed."""


def _require(condition: bool, message: str) -> None:
    if not condition:
        raise ApprovalVerificationError(message)


def _sha256(raw: bytes) -> str:
    return hashlib.sha256(raw).hexdigest()


def _canonical_bytes(value: Any) -> bytes:
    _validate_json_profile(value)
    try:
        return json.dumps(
            value,
            allow_nan=False,
            ensure_ascii=False,
            separators=(",", ":"),
            sort_keys=True,
        ).encode("utf-8")
    except (TypeError, ValueError, UnicodeError) as exc:
        raise ApprovalVerificationError("value is not canonical JSON") from exc


def _canonical_hash(value: Any) -> str:
    return _sha256(_canonical_bytes(value))


def _render(value: Any) -> bytes:
    _validate_json_profile(value)
    try:
        return (
            json.dumps(
                value,
                allow_nan=False,
                ensure_ascii=False,
                indent=2,
                sort_keys=True,
            )
            + "\n"
        ).encode("utf-8")
    except (TypeError, ValueError, UnicodeError) as exc:
        raise ApprovalVerificationError("value is not renderable JSON") from exc


def _field_self_hash(value: Mapping[str, Any], field: str) -> str:
    projected = copy.deepcopy(dict(value))
    _require(field in projected, f"{field} is missing")
    projected[field] = None
    return _canonical_hash(projected)


def _validate_json_profile(value: Any) -> None:
    stack: list[tuple[Any, int]] = [(value, 0)]
    seen_containers: set[int] = set()
    nodes = 0
    while stack:
        current, depth = stack.pop()
        nodes += 1
        _require(depth <= MAX_JSON_DEPTH, "JSON exceeds the nesting ceiling")
        _require(nodes <= MAX_JSON_NODES, "JSON exceeds the node ceiling")
        if current is None or type(current) in {bool, int, str}:
            if type(current) is int:
                _require(
                    len(str(abs(cast(int, current)))) <= MAX_INTEGER_DIGITS,
                    "JSON integer exceeds the digit ceiling",
                )
            if type(current) is str:
                _require(
                    not any(0xD800 <= ord(character) <= 0xDFFF for character in cast(str, current)),
                    "JSON string contains a surrogate code point",
                )
            continue
        _require(type(current) in {dict, list}, "JSON contains an unsupported value type")
        identity = id(current)
        _require(identity not in seen_containers, "JSON contains a cycle or aliased container")
        seen_containers.add(identity)
        if type(current) is dict:
            mapping = cast(dict[Any, Any], current)
            _require(all(type(key) is str for key in mapping), "JSON object key is not a string")
            stack.extend((child, depth + 1) for child in mapping.values())
        else:
            stack.extend((child, depth + 1) for child in cast(list[Any], current))


def _reject_duplicate_pairs(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        _require(key not in result, "JSON contains a duplicate object key")
        result[key] = value
    return result


def _parse_integer(text: str) -> int:
    _require(
        len(text.removeprefix("-")) <= MAX_INTEGER_DIGITS,
        "JSON integer exceeds the digit ceiling",
    )
    return int(text)


def _reject_float(_text: str) -> float:
    raise ApprovalVerificationError("floating-point JSON is forbidden")


def _reject_constant(_text: str) -> None:
    raise ApprovalVerificationError("non-finite JSON is forbidden")


def _parse_json(raw: bytes, *, label: str) -> dict[str, Any]:
    try:
        text = raw.decode("utf-8")
        value = json.loads(
            text,
            object_pairs_hook=_reject_duplicate_pairs,
            parse_constant=_reject_constant,
            parse_float=_reject_float,
            parse_int=_parse_integer,
        )
        _validate_json_profile(value)
    except ApprovalVerificationError:
        raise
    except (UnicodeError, json.JSONDecodeError, ValueError, RecursionError) as exc:
        raise ApprovalVerificationError(f"cannot parse {label}") from exc
    _require(type(value) is dict, f"{label} root is not an object")
    return cast(dict[str, Any], value)


def _read_bounded(
    path: pathlib.Path,
    *,
    maximum: int,
    label: str,
    require_owner_mode: int | None = None,
    require_single_link: bool = False,
) -> bytes:
    _require(type(maximum) is int and maximum >= 1, f"{label} byte ceiling is invalid")
    _require(path.anchor in ("", "/"), f"{label} path anchor is unsupported")
    components = path.parts[1:] if path.is_absolute() else path.parts
    _require(bool(components), f"{label} path has no file component")
    _require(
        all(
            component not in ("", ".", "..") and "\0" not in component
            for component in components
        ),
        f"{label} path contains a forbidden traversal component",
    )
    _require(os.open in os.supports_dir_fd, "descriptor-relative secure open is unavailable")

    file_flags = os.O_RDONLY
    directory_flags = os.O_RDONLY
    for flag_name in ("O_CLOEXEC", "O_NOFOLLOW", "O_NONBLOCK"):
        flag = getattr(os, flag_name, None)
        _require(type(flag) is int and flag != 0, f"secure open flag {flag_name} is unavailable")
        file_flags |= cast(int, flag)
        directory_flags |= cast(int, flag)
    directory_flag = getattr(os, "O_DIRECTORY", None)
    _require(type(directory_flag) is int and directory_flag != 0, "secure open flag O_DIRECTORY is unavailable")
    directory_flags |= cast(int, directory_flag)

    directory_descriptors: list[int] = []
    descriptor: int | None = None
    try:
        directory_descriptor = os.open("/" if path.is_absolute() else ".", directory_flags)
        directory_descriptors.append(directory_descriptor)
        for component in components[:-1]:
            directory_descriptor = os.open(
                component,
                directory_flags,
                dir_fd=directory_descriptor,
            )
            directory_descriptors.append(directory_descriptor)
            _require(stat.S_ISDIR(os.fstat(directory_descriptor).st_mode), f"{label} ancestor is not a directory")
        descriptor = os.open(components[-1], file_flags, dir_fd=directory_descriptor)
        before = os.fstat(descriptor)
        _require(stat.S_ISREG(before.st_mode), f"{label} is not a regular file")
        _require(before.st_size <= maximum, f"{label} exceeds the byte ceiling")
        if require_owner_mode is not None:
            _require(before.st_uid == os.getuid(), f"{label} owner differs")
            _require(stat.S_IMODE(before.st_mode) == require_owner_mode, f"{label} mode differs")
        if require_single_link:
            _require(before.st_nlink == 1, f"{label} link count differs")
        chunks: list[bytes] = []
        remaining = maximum + 1
        while remaining:
            chunk = os.read(descriptor, min(1024 * 1024, remaining))
            if not chunk:
                break
            chunks.append(chunk)
            remaining -= len(chunk)
        raw = b"".join(chunks)
        after = os.fstat(descriptor)
        identity_before = (
            before.st_dev,
            before.st_ino,
            before.st_mode,
            before.st_uid,
            before.st_gid,
            before.st_nlink,
            before.st_size,
            before.st_mtime_ns,
            before.st_ctime_ns,
        )
        identity_after = (
            after.st_dev,
            after.st_ino,
            after.st_mode,
            after.st_uid,
            after.st_gid,
            after.st_nlink,
            after.st_size,
            after.st_mtime_ns,
            after.st_ctime_ns,
        )
        _require(
            len(raw) <= maximum
            and len(raw) == before.st_size
            and identity_before == identity_after,
            f"{label} changed while being read",
        )
        return raw
    except ApprovalVerificationError:
        raise
    except OSError as exc:
        raise ApprovalVerificationError(f"cannot read {label}") from exc
    finally:
        if descriptor is not None:
            os.close(descriptor)
        for directory_descriptor in reversed(directory_descriptors):
            os.close(directory_descriptor)


def _read_json(
    path: pathlib.Path,
    *,
    maximum: int,
    label: str,
    canonical_profile: str | None = None,
    require_owner_mode: int | None = None,
    require_single_link: bool = False,
) -> tuple[dict[str, Any], bytes]:
    raw = _read_bounded(
        path,
        maximum=maximum,
        label=label,
        require_owner_mode=require_owner_mode,
        require_single_link=require_single_link,
    )
    value = _parse_json(raw, label=label)
    if canonical_profile == CANONICAL_ARTIFACT_PROFILE:
        _require(raw == _render(value), f"{label} bytes are not canonical")
    elif canonical_profile == CANONICAL_SIGNED_PROFILE:
        _require(raw == _canonical_bytes(value) + b"\n", f"{label} bytes are not canonical")
    return value, raw


def _artifact_file(path: pathlib.Path) -> str:
    _require(path.name == str(path.name) and "/" not in path.name and "\\" not in path.name, "artifact filename is invalid")
    return path.name


def _read_checked_draft_validator_source() -> tuple[bytes, dict[str, Any]]:
    raw = _read_bounded(
        DRAFT202012_PATH,
        maximum=MAX_COMPONENT_BYTES,
        label="Draft 2020-12 validator source",
    )
    identity = {
        "artifact_file": DRAFT202012_PATH.name,
        "artifact_sha256": _sha256(raw),
        "size_bytes": len(raw),
    }
    _require(identity["artifact_sha256"] == CHECKED_DRAFT202012_SHA256, "Draft 2020-12 validator source hash differs")
    _require(identity["size_bytes"] == CHECKED_DRAFT202012_SIZE, "Draft 2020-12 validator source size differs")
    return raw, identity


def _draft_validator_identity() -> dict[str, Any]:
    _, identity = _read_checked_draft_validator_source()
    return identity


def _checked_draft_validator() -> types.ModuleType:
    global _CHECKED_DRAFT_VALIDATOR

    raw, _ = _read_checked_draft_validator_source()
    if _CHECKED_DRAFT_VALIDATOR is not None:
        return _CHECKED_DRAFT_VALIDATOR
    module_name = "_entire_brain_checked_draft202012_v1"
    module = types.ModuleType(module_name)
    module.__file__ = str(DRAFT202012_PATH)
    previous = sys.modules.get(module_name)
    try:
        code = compile(raw, str(DRAFT202012_PATH), "exec", dont_inherit=True)
        sys.modules[module_name] = module
        exec(code, module.__dict__)
    except Exception as exc:
        raise ApprovalVerificationError("checked Draft 2020-12 validator source cannot be loaded") from exc
    finally:
        if previous is None:
            sys.modules.pop(module_name, None)
        else:
            sys.modules[module_name] = previous
    _require(
        getattr(module, "DIALECT", None)
        == "https://json-schema.org/draft/2020-12/schema"
        and type(getattr(module, "SchemaDocument", None)) is type
        and type(getattr(module, "Validator", None)) is type
        and type(getattr(module, "SchemaError", None)) is type,
        "checked Draft 2020-12 validator API differs",
    )
    _CHECKED_DRAFT_VALIDATOR = module
    return module


def _validate_schema(
    instance: Mapping[str, Any],
    schema: Mapping[str, Any],
    *,
    schema_name: str,
    label: str,
) -> None:
    validator_module = _checked_draft_validator()
    try:
        validator = validator_module.Validator(
            [validator_module.SchemaDocument(name=schema_name, schema=schema)]
        )
        validator.validate(instance, schema_name, label=label)
    except validator_module.SchemaError as exc:
        raise ApprovalVerificationError(f"{label} fails its checked schema") from exc


def _valid_sha(value: Any) -> bool:
    return type(value) is str and SHA256_RE.fullmatch(cast(str, value)) is not None


def _validate_identifier(value: Any, *, label: str) -> str:
    _require(type(value) is str and IDENTIFIER_RE.fullmatch(cast(str, value)) is not None, f"{label} is invalid")
    return cast(str, value)


def _validate_principal(value: Any, *, label: str) -> str:
    _require(type(value) is str and PRINCIPAL_RE.fullmatch(cast(str, value)) is not None, f"{label} is invalid")
    return cast(str, value)


def _parse_time(value: Any, *, label: str) -> dt.datetime:
    _require(type(value) is str and UTC_SECOND_RE.fullmatch(cast(str, value)) is not None, f"{label} is not canonical UTC")
    try:
        parsed = dt.datetime.strptime(cast(str, value), "%Y-%m-%dT%H:%M:%SZ")
    except ValueError as exc:
        raise ApprovalVerificationError(f"{label} is not canonical UTC") from exc
    return parsed.replace(tzinfo=dt.UTC)


def _format_time(value: dt.datetime) -> str:
    _require(value.tzinfo is not None, "verification clock is naive")
    return value.astimezone(dt.UTC).replace(microsecond=0).strftime("%Y-%m-%dT%H:%M:%SZ")


def _utc_now() -> dt.datetime:
    return dt.datetime.now(dt.UTC).replace(microsecond=0)


def _ssh_string(raw: bytes, offset: int, *, label: str) -> tuple[bytes, int]:
    _require(offset + 4 <= len(raw), f"{label} is truncated")
    size = struct.unpack(">I", raw[offset : offset + 4])[0]
    start = offset + 4
    end = start + size
    _require(end <= len(raw), f"{label} is truncated")
    return raw[start:end], end


def _public_key(value: Any, *, label: str) -> tuple[bytes, bytes, str]:
    _require(type(value) is str and BASE64_RE.fullmatch(cast(str, value)) is not None, f"{label} base64 is invalid")
    try:
        blob = base64.b64decode(cast(str, value), validate=True)
    except (ValueError, binascii.Error) as exc:
        raise ApprovalVerificationError(f"{label} base64 is invalid") from exc
    _require(base64.b64encode(blob).decode("ascii") == value, f"{label} base64 is not canonical")
    algorithm, offset = _ssh_string(blob, 0, label=label)
    key, offset = _ssh_string(blob, offset, label=label)
    _require(offset == len(blob), f"{label} has trailing bytes")
    _require(algorithm == b"ssh-ed25519", f"{label} is not Ed25519")
    _require(len(key) == 32, f"{label} Ed25519 key size differs")
    key_line = b"ssh-ed25519 " + cast(str, value).encode("ascii") + b"\n"
    return blob, key_line, _sha256(key_line)


def _decode_sshsig_armor(value: Any, *, expected_public_key_blob: bytes) -> bytes:
    _require(type(value) is str, "approval signature armor is not a string")
    try:
        armor = cast(str, value).encode("ascii")
    except UnicodeEncodeError as exc:
        raise ApprovalVerificationError("approval signature armor is not ASCII") from exc
    _require(0 < len(armor) <= MAX_SIGNATURE_BYTES, "approval signature armor size differs")
    _require("\r" not in cast(str, value) and not cast(str, value).endswith("\n"), "approval signature armor line endings differ")
    lines = cast(str, value).split("\n")
    _require(
        len(lines) >= 3
        and lines[0] == "-----BEGIN SSH SIGNATURE-----"
        and lines[-1] == "-----END SSH SIGNATURE-----",
        "approval signature armor markers differ",
    )
    interior = lines[1:-1]
    _require(
        all(
            bool(line)
            and len(line) <= SSHSIG_ARMOR_WIDTH
            and (index == len(interior) - 1 or len(line) == SSHSIG_ARMOR_WIDTH)
            and BASE64_RE.fullmatch(line) is not None
            for index, line in enumerate(interior)
        ),
        "approval signature armor payload differs",
    )
    encoded = "".join(interior)
    try:
        raw = base64.b64decode(encoded, validate=True)
    except (ValueError, binascii.Error) as exc:
        raise ApprovalVerificationError("approval signature armor payload is invalid") from exc
    canonical = base64.b64encode(raw).decode("ascii")
    canonical_lines = [
        canonical[index : index + SSHSIG_ARMOR_WIDTH]
        for index in range(0, len(canonical), SSHSIG_ARMOR_WIDTH)
    ]
    expected_armor = (
        "-----BEGIN SSH SIGNATURE-----\n"
        + "\n".join(canonical_lines)
        + "\n-----END SSH SIGNATURE-----"
    )
    _require(value == expected_armor, "approval signature armor is not canonical")
    _require(raw.startswith(b"SSHSIG"), "approval SSHSIG magic differs")
    _require(len(raw) >= 10, "approval SSHSIG is truncated")
    version = struct.unpack(">I", raw[6:10])[0]
    _require(version == 1, "approval SSHSIG version differs")
    offset = 10
    public_key_blob, offset = _ssh_string(raw, offset, label="approval SSHSIG public key")
    namespace, offset = _ssh_string(raw, offset, label="approval SSHSIG namespace")
    reserved, offset = _ssh_string(raw, offset, label="approval SSHSIG reserved field")
    hash_algorithm, offset = _ssh_string(raw, offset, label="approval SSHSIG hash algorithm")
    signature_blob, offset = _ssh_string(raw, offset, label="approval SSHSIG signature")
    _require(offset == len(raw), "approval SSHSIG has trailing bytes")
    _require(public_key_blob == expected_public_key_blob, "approval SSHSIG public key differs")
    _require(namespace == SIGNATURE_NAMESPACE.encode("ascii"), "approval SSHSIG namespace differs")
    _require(reserved == b"", "approval SSHSIG reserved field is not empty")
    _require(hash_algorithm == b"sha512", "approval SSHSIG hash algorithm differs")
    signature_algorithm, signature_offset = _ssh_string(signature_blob, 0, label="approval SSHSIG signature algorithm")
    signature, signature_offset = _ssh_string(signature_blob, signature_offset, label="approval SSHSIG Ed25519 signature")
    _require(signature_offset == len(signature_blob), "approval SSHSIG signature blob has trailing bytes")
    _require(signature_algorithm == b"ssh-ed25519", "approval SSHSIG signature algorithm differs")
    _require(len(signature) == 64, "approval SSHSIG Ed25519 signature size differs")
    return raw


def _validate_trust_roots(value: Mapping[str, Any]) -> dict[str, Any]:
    expected_fields = {
        "canonical_json_profile",
        "profile",
        "roots",
        "schema_version",
        "signature_namespace",
        "status",
        "trust_roots_sha256",
    }
    _require(type(value) is dict and set(value) == expected_fields, "trust-roots fields differ")
    _require(value["schema_version"] == 1, "trust-roots schema version differs")
    _require(value["profile"] == TRUST_PROFILE, "trust-roots profile differs")
    _require(value["canonical_json_profile"] == CANONICAL_ARTIFACT_PROFILE, "trust-roots canonical profile differs")
    _require(value["signature_namespace"] == SIGNATURE_NAMESPACE, "trust-roots namespace differs")
    _require(value["status"] in {"pending_owner_authorization", "approved"}, "trust-roots status differs")
    _require(_valid_sha(value["trust_roots_sha256"]), "trust-roots self hash is invalid")
    _require(value["trust_roots_sha256"] == _field_self_hash(value, "trust_roots_sha256"), "trust-roots self hash differs")
    roots = value["roots"]
    _require(type(roots) is list and len(cast(list[Any], roots)) <= 8, "trust-roots collection differs")
    if value["status"] == "pending_owner_authorization":
        _require(roots == [], "pending trust roots must be empty")
    else:
        _require(bool(roots), "approved trust roots must not be empty")
    seen_ids: set[str] = set()
    seen_principals: set[str] = set()
    seen_keys: set[str] = set()
    active_count = 0
    for index, row_value in enumerate(cast(list[Any], roots)):
        label = f"trust root {index}"
        fields = {
            "key_type",
            "not_after",
            "not_before",
            "principal",
            "public_key_base64",
            "public_key_sha256",
            "purpose",
            "revoked_at",
            "root_id",
            "status",
        }
        _require(type(row_value) is dict and set(cast(dict[str, Any], row_value)) == fields, f"{label} fields differ")
        row = cast(dict[str, Any], row_value)
        root_id = _validate_identifier(row["root_id"], label=f"{label} id")
        principal = _validate_principal(row["principal"], label=f"{label} principal")
        _require(row["key_type"] == "ssh-ed25519", f"{label} key type differs")
        _require(row["purpose"] == "negative_control_execution_approval_v1", f"{label} purpose differs")
        _, _, key_hash = _public_key(row["public_key_base64"], label=f"{label} public key")
        _require(row["public_key_sha256"] == key_hash, f"{label} public key hash differs")
        _require(root_id not in seen_ids, "trust roots duplicate an id")
        _require(principal not in seen_principals, "trust roots duplicate a principal")
        _require(key_hash not in seen_keys, "trust roots duplicate a public key")
        seen_ids.add(root_id)
        seen_principals.add(principal)
        seen_keys.add(key_hash)
        before = _parse_time(row["not_before"], label=f"{label} not_before")
        after = _parse_time(row["not_after"], label=f"{label} not_after")
        _require(before < after, f"{label} validity window is empty")
        _require((after - before).total_seconds() <= MAX_ROOT_LIFETIME_SECONDS, f"{label} validity window is too long")
        _require(row["status"] in {"active", "revoked"}, f"{label} status differs")
        if row["status"] == "active":
            active_count += 1
            _require(row["revoked_at"] is None, f"{label} active revocation time differs")
        else:
            revoked = _parse_time(row["revoked_at"], label=f"{label} revoked_at")
            _require(before <= revoked <= after, f"{label} revocation time is outside its window")
    if value["status"] == "approved":
        _require(active_count >= 1, "approved trust roots have no active root")
    return dict(value)


def _system_ssh_keygen_identity() -> dict[str, Any]:
    current = pathlib.Path("/")
    try:
        root_metadata = current.lstat()
    except OSError as exc:
        raise ApprovalVerificationError("trusted ssh-keygen root cannot be inspected") from exc
    _require(stat.S_ISDIR(root_metadata.st_mode), "trusted ssh-keygen root is not a directory")
    _require(not stat.S_ISLNK(root_metadata.st_mode), "trusted ssh-keygen root is redirected")
    _require(root_metadata.st_uid == 0 and root_metadata.st_gid == 0, "trusted ssh-keygen root owner differs")
    _require(stat.S_IMODE(root_metadata.st_mode) & 0o022 == 0, "trusted ssh-keygen root is writable")
    for index, component in enumerate(SSH_KEYGEN_PATH.parts[1:]):
        current = current / component
        try:
            metadata = current.lstat()
        except OSError as exc:
            raise ApprovalVerificationError("trusted ssh-keygen path cannot be inspected") from exc
        _require(not stat.S_ISLNK(metadata.st_mode), "trusted ssh-keygen path traverses a symlink")
        _require(metadata.st_uid == 0 and metadata.st_gid == 0, "trusted ssh-keygen path owner differs")
        _require(stat.S_IMODE(metadata.st_mode) & 0o022 == 0, "trusted ssh-keygen path is writable")
        if index == len(SSH_KEYGEN_PATH.parts[1:]) - 1:
            _require(stat.S_ISREG(metadata.st_mode), "trusted ssh-keygen is not a regular file")
            _require(stat.S_IMODE(metadata.st_mode) == 0o755, "trusted ssh-keygen mode differs")
        else:
            _require(stat.S_ISDIR(metadata.st_mode), "trusted ssh-keygen ancestor is not a directory")
    raw = _read_bounded(SSH_KEYGEN_PATH, maximum=MAX_BINARY_BYTES, label="trusted ssh-keygen")
    metadata = SSH_KEYGEN_PATH.lstat()
    identity = {
        "gid": metadata.st_gid,
        "mode": f"{stat.S_IMODE(metadata.st_mode):04o}",
        "path": str(SSH_KEYGEN_PATH),
        "root_adversary": "out_of_scope",
        "sha256": _sha256(raw),
        "size_bytes": len(raw),
        "uid": metadata.st_uid,
    }
    _require(identity["sha256"] == CHECKED_SSH_KEYGEN_SHA256, "trusted ssh-keygen hash differs")
    _require(identity["size_bytes"] == CHECKED_SSH_KEYGEN_SIZE, "trusted ssh-keygen size differs")
    return identity


EXPECTED_EXECUTION_CONTRACT_BINDING = {
    "artifact_file": EXECUTION_CONTRACT_PATH.name,
    "artifact_sha256": CHECKED_EXECUTION_ARTIFACT_SHA256,
    "attempt_count": 248,
    "authority_sha256": CHECKED_AUTHORITY_SHA256,
    "builder_sha256": CHECKED_EXECUTION_BUILDER_SHA256,
    "candidate_bindings_sha256": CHECKED_CANDIDATE_BINDINGS_SHA256,
    "candidate_count": 62,
    "contract_sha256": CHECKED_EXECUTION_CONTRACT_SHA256,
    "enforcement_requirements_sha256": CHECKED_ENFORCEMENT_SHA256,
    "execution_status": "forbidden_missing_all_residual_gates_and_audited_execution_adapter",
    "profile": "agent_brain_negative_control_execution_contract_v1",
    "protocol_sha256": CHECKED_PROTOCOL_SHA256,
    "repository_count": 3,
    "residual_gates_sha256": CHECKED_RESIDUAL_GATES_SHA256,
    "runtime_bindings_sha256": CHECKED_RUNTIME_BINDINGS_SHA256,
    "schedule_sha256": CHECKED_SCHEDULE_SHA256,
    "schema_sha256": CHECKED_EXECUTION_SCHEMA_SHA256,
    "schema_version": 1,
    "source_commit": CHECKED_EXECUTION_SOURCE_COMMIT,
    "state_machine_sha256": CHECKED_STATE_MACHINE_SHA256,
    "status": "contract_compiled_execution_forbidden",
}

PROTOCOL = {
    "arm_order": ["baseline", "first_parent_source_reversal"],
    "max_concurrency": 1,
    "max_total_wall_seconds": 28_800,
    "outer_timeout_seconds_per_arm": 600,
    "repetitions_per_arm": 2,
    "repository_order": ["entire-brain", "entire-db", "entire-graph"],
}

PROHIBITIONS = [
    "model_provider_execution",
    "paid_execution",
    "holdout_access",
    "population_assignment",
    "candidate_or_schedule_outside_exact_contract",
    "retry_replacement_or_schedule_mutation",
    "execution_before_all_residual_gates_and_atomic_consumption",
]

RESIDUAL_GATES = [
    "actual_approved_offline_cache_seed_archive_and_authorized_binding",
    "safe_archive_traversal_type_link_device_and_content_verifier",
    "trusted_apfs_observer_and_atomic_capacity_reservation",
    "clean_detached_worktree_executor_and_first_parent_reversal_proof",
    "attested_attempt_producer_and_classifier_integration",
    "private_log_executor_integration_retention_and_aggregate_accounting",
    "fail_closed_cleanup_interruption_attestation_and_no_receipt_guarantee",
    "atomic_single_use_approval_consumption_and_execution_binding",
]

APPROVAL_EFFECT = {
    "execution_status_after_verification": AFTER_VERIFICATION_STATUS,
    "remaining_gates": list(RESIDUAL_GATES),
    "satisfies_only_after_verification": [
        "owner_execution_approval_receipt_and_trust_mechanism"
    ],
    "signature_verification_alone": "not_execution_authority",
}

SINGLE_USE = {
    "consumption_key": "approval_sha256_and_signed_payload_sha256",
    "consumption_mode": "external_atomic_compare_and_set_required_not_implemented",
    "max_consumptions": 1,
    "receipt_binding": "approval_raw_sha256_signed_payload_sha256_run_identity_sha256_and_exact_contract_sha256",
    "reuse": "forbidden",
    "validity_check": "required_at_atomic_consumption_before_any_external_effect",
}

RUNTIME_BINDINGS = {
    "approval_envelope_sha256": None,
    "approval_id": None,
    "atomic_consumption_receipt_sha256": None,
    "run_id": None,
    "run_identity_sha256": None,
    "signature_verified_at": None,
}

VERIFICATION_POLICY = {
    "approval_json_max_bytes": MAX_APPROVAL_BYTES,
    "approval_lifetime_max_seconds": MAX_APPROVAL_LIFETIME_SECONDS,
    "approval_schema_profile": APPROVAL_PROFILE,
    "canonical_signed_json_profile": CANONICAL_SIGNED_PROFILE,
    "cli_commands": ["build", "check", "verify"],
    "current_time_source": "system_utc_sampled_before_and_after_sshsig_no_cli_override",
    "issuance_to_activation_max_seconds": MAX_ISSUANCE_TO_ACTIVATION_SECONDS,
    "json_depth_max": MAX_JSON_DEPTH,
    "json_nodes_max": MAX_JSON_NODES,
    "payload_domain_sha256": _sha256(PAYLOAD_DOMAIN),
    "production_path_overrides": "forbidden",
    "signature_armor_max_bytes": MAX_SIGNATURE_BYTES,
    "signature_hash_algorithm": "sha512",
    "signature_namespace": SIGNATURE_NAMESPACE,
    "signature_scheme": SIGNATURE_SCHEME,
    "signed_payload_max_bytes": MAX_SIGNED_PAYLOAD_BYTES,
    "ssh_timeout_seconds": SSH_TIMEOUT_SECONDS,
    "trust_roots_max_bytes": MAX_TRUST_ROOTS_BYTES,
    "verification_report": "non_persistent_non_authorizing_execution_forbidden",
}


def _load_schema(path: pathlib.Path, *, expected_sha256: str, label: str) -> tuple[dict[str, Any], bytes]:
    schema, raw = _read_json(path, maximum=MAX_COMPONENT_BYTES, label=label)
    _require(_sha256(raw) == expected_sha256, f"{label} hash differs")
    return schema, raw


def _execution_contract_binding(value: Mapping[str, Any], raw: bytes) -> dict[str, Any]:
    _require(_artifact_file(EXECUTION_CONTRACT_PATH) == EXPECTED_EXECUTION_CONTRACT_BINDING["artifact_file"], "execution-contract filename differs")
    binding = {
        "artifact_file": EXECUTION_CONTRACT_PATH.name,
        "artifact_sha256": _sha256(raw),
        "attempt_count": value.get("protocol", {}).get("total_attempt_count") if type(value.get("protocol")) is dict else None,
        "authority_sha256": _canonical_hash(value.get("authority")),
        "builder_sha256": value.get("implementation", {}).get("builder_sha256") if type(value.get("implementation")) is dict else None,
        "candidate_bindings_sha256": value.get("candidate_bindings_sha256"),
        "candidate_count": value.get("protocol", {}).get("total_candidate_count") if type(value.get("protocol")) is dict else None,
        "contract_sha256": value.get("contract_sha256"),
        "enforcement_requirements_sha256": _canonical_hash(value.get("enforcement_requirements")),
        "execution_status": value.get("execution_status"),
        "profile": value.get("profile"),
        "protocol_sha256": _canonical_hash(value.get("protocol")),
        "repository_count": len(cast(list[Any], value.get("repository_bindings"))) if type(value.get("repository_bindings")) is list else None,
        "residual_gates_sha256": _canonical_hash(value.get("residual_gates")),
        "runtime_bindings_sha256": _canonical_hash(value.get("runtime_bindings")),
        "schedule_sha256": value.get("schedule_sha256"),
        "schema_sha256": value.get("implementation", {}).get("schema_sha256") if type(value.get("implementation")) is dict else None,
        "schema_version": value.get("schema_version"),
        "source_commit": CHECKED_EXECUTION_SOURCE_COMMIT,
        "state_machine_sha256": _canonical_hash(value.get("state_machine")),
        "status": value.get("status"),
    }
    _require(binding == EXPECTED_EXECUTION_CONTRACT_BINDING, "execution contract differs from exact E0 binding")
    return binding


def _file_binding(path: pathlib.Path, raw: bytes) -> dict[str, str]:
    return {"artifact_file": _artifact_file(path), "artifact_sha256": _sha256(raw)}


def _load_dependencies() -> dict[str, Any]:
    draft_validator = _draft_validator_identity()
    approval_schema, approval_schema_raw = _load_schema(
        APPROVAL_SCHEMA_PATH,
        expected_sha256=CHECKED_APPROVAL_SCHEMA_SHA256,
        label="approval schema",
    )
    trust_schema, trust_schema_raw = _load_schema(
        TRUST_ROOTS_SCHEMA_PATH,
        expected_sha256=CHECKED_TRUST_ROOTS_SCHEMA_SHA256,
        label="trust-roots schema",
    )
    report_schema, report_schema_raw = _load_schema(
        REPORT_SCHEMA_PATH,
        expected_sha256=CHECKED_REPORT_SCHEMA_SHA256,
        label="verification-report schema",
    )
    verifier_schema, verifier_schema_raw = _load_schema(
        VERIFIER_CONTRACT_SCHEMA_PATH,
        expected_sha256=CHECKED_VERIFIER_CONTRACT_SCHEMA_SHA256,
        label="verifier-contract schema",
    )
    execution_contract, execution_raw = _read_json(
        EXECUTION_CONTRACT_PATH,
        maximum=MAX_ARTIFACT_BYTES,
        label="execution contract",
        canonical_profile=CANONICAL_ARTIFACT_PROFILE,
    )
    _require(_sha256(execution_raw) == CHECKED_EXECUTION_ARTIFACT_SHA256, "execution-contract raw hash differs")
    execution_binding = _execution_contract_binding(execution_contract, execution_raw)
    trust_roots, trust_raw = _read_json(
        TRUST_ROOTS_PATH,
        maximum=MAX_TRUST_ROOTS_BYTES,
        label="owner trust roots",
        canonical_profile=CANONICAL_ARTIFACT_PROFILE,
    )
    _require(_sha256(trust_raw) == CHECKED_TRUST_ROOTS_FILE_SHA256, "trust-roots raw hash differs")
    _validate_schema(trust_roots, trust_schema, schema_name=TRUST_ROOTS_SCHEMA_PATH.name, label="owner trust roots")
    validated_trust = _validate_trust_roots(trust_roots)
    _require(validated_trust["trust_roots_sha256"] == CHECKED_TRUST_ROOTS_SHA256, "checked trust-roots self hash differs")
    return {
        "approval_schema": approval_schema,
        "approval_schema_raw": approval_schema_raw,
        "draft_validator": draft_validator,
        "execution_binding": execution_binding,
        "execution_contract": execution_contract,
        "execution_raw": execution_raw,
        "report_schema": report_schema,
        "report_schema_raw": report_schema_raw,
        "ssh_keygen": _system_ssh_keygen_identity(),
        "trust_roots": validated_trust,
        "trust_raw": trust_raw,
        "trust_schema": trust_schema,
        "trust_schema_raw": trust_schema_raw,
        "verifier_schema": verifier_schema,
        "verifier_schema_raw": verifier_schema_raw,
    }


def build_verifier_contract() -> dict[str, Any]:
    dependencies = _load_dependencies()
    source_raw = _read_bounded(pathlib.Path(__file__), maximum=MAX_COMPONENT_BYTES, label="approval verifier source")
    trust_roots = dependencies["trust_roots"]
    trust_status = trust_roots["status"]
    status = APPROVED_STATUS if trust_status == "approved" else PENDING_STATUS
    contract: dict[str, Any] = {
        "authority": {
            "approval_envelope_sha256": None,
            "approval_signature_verified": False,
            "atomic_consumption": False,
            "benchmark_execution": "forbidden_verifier_contract_only",
            "candidate_execution": "forbidden_verifier_contract_only",
            "execution_authority": False,
            "model_provider_execution": "forbidden",
            "owner_key": "absent_not_fabricated",
            "paid_execution": "forbidden",
            "trust_root_status": trust_status,
        },
        "execution_contract_binding": copy.deepcopy(dependencies["execution_binding"]),
        "execution_status": EXECUTION_STATUS,
        "implementation": {
            "approval_schema": _file_binding(APPROVAL_SCHEMA_PATH, dependencies["approval_schema_raw"]),
            "builder": _file_binding(pathlib.Path(__file__), source_raw),
            "draft202012_validator": copy.deepcopy(dependencies["draft_validator"]),
            "report_schema": _file_binding(REPORT_SCHEMA_PATH, dependencies["report_schema_raw"]),
            "system_ssh_keygen": copy.deepcopy(dependencies["ssh_keygen"]),
            "trust_roots_schema": _file_binding(TRUST_ROOTS_SCHEMA_PATH, dependencies["trust_schema_raw"]),
            "verifier_contract_schema": _file_binding(VERIFIER_CONTRACT_SCHEMA_PATH, dependencies["verifier_schema_raw"]),
        },
        "profile": VERIFIER_PROFILE,
        "residual_gates_after_valid_signature": list(RESIDUAL_GATES),
        "runtime_bindings": copy.deepcopy(RUNTIME_BINDINGS),
        "schema_version": 1,
        "status": status,
        "trust_roots_binding": {
            "artifact_file": TRUST_ROOTS_PATH.name,
            "artifact_sha256": _sha256(dependencies["trust_raw"]),
            "canonical_json_profile": CANONICAL_ARTIFACT_PROFILE,
            "profile": TRUST_PROFILE,
            "root_count": len(trust_roots["roots"]),
            "schema_sha256": _sha256(dependencies["trust_schema_raw"]),
            "status": trust_status,
            "trust_roots_sha256": trust_roots["trust_roots_sha256"],
        },
        "verification_policy": copy.deepcopy(VERIFICATION_POLICY),
        "verifier_contract_sha256": None,
    }
    contract["verifier_contract_sha256"] = _field_self_hash(contract, "verifier_contract_sha256")
    validate_verifier_contract(contract, schema=dependencies["verifier_schema"])
    return contract


def _expected_verifier_implementation() -> dict[str, Any]:
    source_raw = _read_bounded(
        pathlib.Path(__file__),
        maximum=MAX_COMPONENT_BYTES,
        label="approval verifier source",
    )
    return {
        "approval_schema": {
            "artifact_file": APPROVAL_SCHEMA_PATH.name,
            "artifact_sha256": CHECKED_APPROVAL_SCHEMA_SHA256,
        },
        "builder": {
            "artifact_file": pathlib.Path(__file__).name,
            "artifact_sha256": _sha256(source_raw),
        },
        "draft202012_validator": _draft_validator_identity(),
        "report_schema": {
            "artifact_file": REPORT_SCHEMA_PATH.name,
            "artifact_sha256": CHECKED_REPORT_SCHEMA_SHA256,
        },
        "system_ssh_keygen": _system_ssh_keygen_identity(),
        "trust_roots_schema": {
            "artifact_file": TRUST_ROOTS_SCHEMA_PATH.name,
            "artifact_sha256": CHECKED_TRUST_ROOTS_SCHEMA_SHA256,
        },
        "verifier_contract_schema": {
            "artifact_file": VERIFIER_CONTRACT_SCHEMA_PATH.name,
            "artifact_sha256": CHECKED_VERIFIER_CONTRACT_SCHEMA_SHA256,
        },
    }


def validate_verifier_contract(value: Mapping[str, Any], *, schema: Mapping[str, Any]) -> None:
    _validate_schema(value, schema, schema_name=VERIFIER_CONTRACT_SCHEMA_PATH.name, label="approval verifier contract")
    expected_fields = {
        "authority",
        "execution_contract_binding",
        "execution_status",
        "implementation",
        "profile",
        "residual_gates_after_valid_signature",
        "runtime_bindings",
        "schema_version",
        "status",
        "trust_roots_binding",
        "verification_policy",
        "verifier_contract_sha256",
    }
    _require(type(value) is dict and set(value) == expected_fields, "verifier-contract fields differ")
    _require(value["profile"] == VERIFIER_PROFILE and value["schema_version"] == 1, "verifier-contract identity differs")
    _require(value["status"] in {PENDING_STATUS, APPROVED_STATUS}, "verifier-contract status differs")
    _require(value["execution_status"] == EXECUTION_STATUS, "verifier-contract execution status differs")
    _require(value["execution_contract_binding"] == EXPECTED_EXECUTION_CONTRACT_BINDING, "verifier-contract E0 binding differs")
    _require(value["runtime_bindings"] == RUNTIME_BINDINGS, "verifier-contract runtime bindings differ")
    _require(value["residual_gates_after_valid_signature"] == RESIDUAL_GATES, "verifier-contract residual gates differ")
    _require(value["verification_policy"] == VERIFICATION_POLICY, "verifier-contract policy differs")
    _require(value["implementation"] == _expected_verifier_implementation(), "verifier-contract implementation binding differs")
    _require(_valid_sha(value["verifier_contract_sha256"]), "verifier-contract self hash is invalid")
    _require(value["verifier_contract_sha256"] == _field_self_hash(value, "verifier_contract_sha256"), "verifier-contract self hash differs")
    authority = value["authority"]
    _require(type(authority) is dict, "verifier-contract authority differs")
    authority_map = cast(dict[str, Any], authority)
    trust_binding = value["trust_roots_binding"]
    _require(type(trust_binding) is dict, "verifier-contract trust binding differs")
    trust_binding_map = cast(dict[str, Any], trust_binding)
    expected_status = APPROVED_STATUS if trust_binding_map.get("status") == "approved" else PENDING_STATUS
    _require(value["status"] == expected_status, "verifier-contract trust status is inconsistent")
    expected_authority = {
        "approval_envelope_sha256": None,
        "approval_signature_verified": False,
        "atomic_consumption": False,
        "benchmark_execution": "forbidden_verifier_contract_only",
        "candidate_execution": "forbidden_verifier_contract_only",
        "execution_authority": False,
        "model_provider_execution": "forbidden",
        "owner_key": "absent_not_fabricated",
        "paid_execution": "forbidden",
        "trust_root_status": trust_binding_map.get("status"),
    }
    _require(authority_map == expected_authority, "verifier-contract authority differs")
    _require(
        trust_binding_map.get("artifact_file") == TRUST_ROOTS_PATH.name
        and trust_binding_map.get("canonical_json_profile") == CANONICAL_ARTIFACT_PROFILE
        and trust_binding_map.get("profile") == TRUST_PROFILE
        and trust_binding_map.get("schema_sha256") == CHECKED_TRUST_ROOTS_SCHEMA_SHA256,
        "verifier-contract trust binding identity differs",
    )


def check_verifier_contract(path: pathlib.Path = VERIFIER_CONTRACT_PATH) -> dict[str, Any]:
    value, raw = _read_json(
        path,
        maximum=MAX_ARTIFACT_BYTES,
        label="approval verifier contract",
        canonical_profile=CANONICAL_ARTIFACT_PROFILE,
    )
    dependencies = _load_dependencies()
    validate_verifier_contract(value, schema=dependencies["verifier_schema"])
    rebuilt = build_verifier_contract()
    _require(value == rebuilt and raw == _render(rebuilt), "approval verifier contract differs from exact rebuild")
    return value


def _expected_statement_execution_binding() -> dict[str, Any]:
    binding = dict(EXPECTED_EXECUTION_CONTRACT_BINDING)
    binding.pop("repository_count")
    return binding


def signed_payload(statement: Mapping[str, Any]) -> bytes:
    payload = PAYLOAD_DOMAIN + _canonical_bytes(statement)
    _require(len(payload) <= MAX_SIGNED_PAYLOAD_BYTES, "approval signed payload exceeds its ceiling")
    return payload


def validate_approval_envelope(
    value: Mapping[str, Any],
    *,
    schema: Mapping[str, Any],
    verifier_contract: Mapping[str, Any],
) -> dict[str, Any]:
    _validate_schema(value, schema, schema_name=APPROVAL_SCHEMA_PATH.name, label="owner approval envelope")
    _require(type(value) is dict, "owner approval envelope is not an object")
    _require(value.get("schema_version") == 1 and value.get("profile") == APPROVAL_PROFILE, "owner approval identity differs")
    _require(_valid_sha(value.get("approval_sha256")), "owner approval self hash is invalid")
    _require(value["approval_sha256"] == _field_self_hash(value, "approval_sha256"), "owner approval self hash differs")
    statement = value.get("statement")
    _require(type(statement) is dict, "owner approval statement is not an object")
    statement_map = cast(dict[str, Any], statement)
    _require(statement_map.get("schema_version") == 1 and statement_map.get("profile") == STATEMENT_PROFILE, "owner approval statement identity differs")
    _require(statement_map.get("action") == "approve_exact_development_negative_control_run_subject_to_all_residual_gates_and_atomic_consumption", "owner approval action differs")
    _require(statement_map.get("approval_scope") == "exact_e0_contract_one_development_run_v1", "owner approval scope differs")
    _require(statement_map.get("signature_namespace") == SIGNATURE_NAMESPACE, "owner approval namespace differs")
    _require(statement_map.get("signature_scheme") == SIGNATURE_SCHEME, "owner approval signature scheme differs")
    _validate_identifier(statement_map.get("trust_root_id"), label="owner approval trust root id")
    _validate_principal(statement_map.get("signer_principal"), label="owner approval signer principal")
    _require(_valid_sha(statement_map.get("signer_public_key_sha256")), "owner approval signer key hash is invalid")
    _require(_valid_sha(statement_map.get("approval_id")), "owner approval id is invalid")
    _require(statement_map.get("execution_contract") == _expected_statement_execution_binding(), "owner approval execution-contract binding differs")
    expected_verifier_binding = {
        "artifact_file": VERIFIER_CONTRACT_PATH.name,
        "artifact_sha256": _sha256(_render(verifier_contract)),
        "profile": VERIFIER_PROFILE,
        "status": APPROVED_STATUS,
        "verifier_contract_sha256": verifier_contract.get("verifier_contract_sha256"),
    }
    _require(statement_map.get("verifier_contract") == expected_verifier_binding, "owner approval verifier binding differs")
    _require(statement_map.get("protocol") == PROTOCOL, "owner approval protocol differs")
    _require(statement_map.get("prohibitions") == PROHIBITIONS, "owner approval prohibitions differ")
    _require(statement_map.get("approval_effect") == APPROVAL_EFFECT, "owner approval effect differs")
    _require(statement_map.get("single_use") == SINGLE_USE, "owner approval single-use policy differs")
    _require(statement_map.get("execution_status_after_verification") == AFTER_VERIFICATION_STATUS, "owner approval post-verification status differs")
    run_identity = statement_map.get("run_identity")
    _require(type(run_identity) is dict, "owner approval run identity is not an object")
    run_identity_map = cast(dict[str, Any], run_identity)
    expected_run_fields = {
        "attempt_count",
        "candidate_bindings_sha256",
        "candidate_count",
        "repository_count",
        "run_id",
        "run_nonce_sha256",
        "schedule_sha256",
    }
    _require(set(run_identity_map) == expected_run_fields, "owner approval run-identity fields differ")
    _require(run_identity_map.get("attempt_count") == 248 and run_identity_map.get("candidate_count") == 62 and run_identity_map.get("repository_count") == 3, "owner approval run counts differ")
    _require(run_identity_map.get("candidate_bindings_sha256") == CHECKED_CANDIDATE_BINDINGS_SHA256, "owner approval candidate binding differs")
    _require(run_identity_map.get("schedule_sha256") == CHECKED_SCHEDULE_SHA256, "owner approval schedule binding differs")
    _require(_valid_sha(run_identity_map.get("run_id")) and _valid_sha(run_identity_map.get("run_nonce_sha256")), "owner approval run identity hash is invalid")
    _require(statement_map.get("run_identity_sha256") == _canonical_hash(run_identity_map), "owner approval run identity self hash differs")
    _require(len({statement_map["approval_id"], run_identity_map["run_id"], run_identity_map["run_nonce_sha256"]}) == 3, "owner approval identifiers are not distinct")
    issued = _parse_time(statement_map.get("issued_at"), label="owner approval issued_at")
    not_before = _parse_time(statement_map.get("not_before"), label="owner approval not_before")
    expires = _parse_time(statement_map.get("expires_at"), label="owner approval expires_at")
    _require(issued <= not_before < expires, "owner approval time ordering differs")
    _require((not_before - issued).total_seconds() <= MAX_ISSUANCE_TO_ACTIVATION_SECONDS, "owner approval activation delay exceeds its ceiling")
    _require((expires - not_before).total_seconds() <= MAX_APPROVAL_LIFETIME_SECONDS, "owner approval lifetime exceeds its ceiling")
    proof = value.get("proof")
    _require(type(proof) is dict and set(cast(dict[str, Any], proof)) == {"scheme", "signature_armor", "signed_payload_sha256"}, "owner approval proof fields differ")
    proof_map = cast(dict[str, Any], proof)
    _require(proof_map.get("scheme") == SIGNATURE_SCHEME, "owner approval proof scheme differs")
    payload = signed_payload(statement_map)
    _require(proof_map.get("signed_payload_sha256") == _sha256(payload), "owner approval signed payload hash differs")
    return dict(value)


def _select_trust_root(
    approval: Mapping[str, Any],
    trust_roots: Mapping[str, Any],
    *,
    current: dt.datetime,
) -> tuple[dict[str, Any], bytes, bytes]:
    _require(trust_roots.get("status") == "approved", "owner trust roots are pending authorization")
    statement = cast(dict[str, Any], approval["statement"])
    root_id = statement["trust_root_id"]
    roots = cast(list[Any], trust_roots["roots"])
    matches = [row for row in roots if type(row) is dict and row.get("root_id") == root_id]
    _require(len(matches) == 1, "owner approval trust root is missing or ambiguous")
    root = cast(dict[str, Any], matches[0])
    _require(root["status"] == "active" and root["revoked_at"] is None, "owner approval trust root is not active")
    _require(root["principal"] == statement["signer_principal"], "owner approval signer principal differs from trust root")
    public_key_blob, key_line, key_hash = _public_key(root["public_key_base64"], label="owner trust-root public key")
    _require(root["public_key_sha256"] == key_hash == statement["signer_public_key_sha256"], "owner approval signer key differs from trust root")
    issued = _parse_time(statement["issued_at"], label="owner approval issued_at")
    not_before = _parse_time(statement["not_before"], label="owner approval not_before")
    expires = _parse_time(statement["expires_at"], label="owner approval expires_at")
    root_before = _parse_time(root["not_before"], label="owner trust-root not_before")
    root_after = _parse_time(root["not_after"], label="owner trust-root not_after")
    _require(root_before <= issued <= not_before and expires <= root_after, "owner approval window is outside trust-root validity")
    _require(not_before <= current < expires, "owner approval is not active at verification time")
    _require(root_before <= current < root_after, "owner trust root is not active at verification time")
    return root, public_key_blob, key_line


def _write_private_new(path: pathlib.Path, raw: bytes) -> None:
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
    nofollow = getattr(os, "O_NOFOLLOW", None)
    _require(type(nofollow) is int and nofollow != 0, "secure output flag O_NOFOLLOW is unavailable")
    flags |= cast(int, nofollow)
    descriptor = os.open(path, flags, 0o600)
    try:
        offset = 0
        while offset < len(raw):
            written = os.write(descriptor, raw[offset:])
            _require(written > 0, "private verifier file write failed")
            offset += written
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def _run_sshsig_verify(
    *,
    payload: bytes,
    signature_armor: str,
    principal: str,
    key_line: bytes,
) -> None:
    before = _system_ssh_keygen_identity()
    with tempfile.TemporaryDirectory(prefix="negative-control-owner-verify-") as temporary_name:
        temporary = pathlib.Path(temporary_name).resolve(strict=True)
        os.chmod(temporary, 0o700)
        allowed = temporary / "allowed_signers"
        signature_path = temporary / "signature"
        _write_private_new(allowed, principal.encode("ascii") + b" " + key_line)
        _write_private_new(signature_path, signature_armor.encode("ascii") + b"\n")
        command = [
            str(SSH_KEYGEN_PATH),
            "-Y",
            "verify",
            "-f",
            str(allowed),
            "-I",
            principal,
            "-n",
            SIGNATURE_NAMESPACE,
            "-s",
            str(signature_path),
        ]
        try:
            process = subprocess.Popen(
                command,
                cwd=temporary,
                env={"LANG": "C", "LC_ALL": "C", "PATH": "/usr/bin"},
                shell=False,
                stdin=subprocess.PIPE,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
                start_new_session=True,
            )
            try:
                process.communicate(input=payload, timeout=SSH_TIMEOUT_SECONDS)
            except subprocess.TimeoutExpired as exc:
                os.killpg(process.pid, signal.SIGKILL)
                process.communicate()
                raise ApprovalVerificationError("offline SSHSIG verification timed out") from exc
        except ApprovalVerificationError:
            raise
        except OSError as exc:
            raise ApprovalVerificationError("offline SSHSIG verification could not start") from exc
        _require(process.returncode == 0, "offline SSHSIG verification was rejected")
    after = _system_ssh_keygen_identity()
    _require(before == after, "trusted ssh-keygen identity changed during verification")


def _verify_approval_with_dependencies(
    approval: Mapping[str, Any],
    *,
    approval_raw: bytes,
    trust_roots: Mapping[str, Any],
    verifier_contract: Mapping[str, Any],
    approval_schema: Mapping[str, Any],
    report_schema: Mapping[str, Any],
    verifier_schema: Mapping[str, Any],
) -> dict[str, Any]:
    _require(
        len(approval_raw) <= MAX_APPROVAL_BYTES
        and approval_raw == _canonical_bytes(approval) + b"\n",
        "owner approval raw bytes differ from the canonical envelope",
    )
    validate_verifier_contract(verifier_contract, schema=verifier_schema)
    _require(verifier_contract.get("status") == APPROVED_STATUS, "approval verifier trust root is pending")
    _require(verifier_contract.get("execution_contract_binding") == EXPECTED_EXECUTION_CONTRACT_BINDING, "approval verifier E0 binding differs")
    _require(verifier_contract.get("runtime_bindings") == RUNTIME_BINDINGS, "approval verifier runtime bindings differ")
    _require(verifier_contract.get("execution_status") == EXECUTION_STATUS, "approval verifier execution status differs")
    _require(_valid_sha(verifier_contract.get("verifier_contract_sha256")), "approval verifier self hash is invalid")
    _require(
        verifier_contract.get("verifier_contract_sha256")
        == _field_self_hash(verifier_contract, "verifier_contract_sha256"),
        "approval verifier self hash differs",
    )
    authority = verifier_contract.get("authority")
    _require(
        type(authority) is dict
        and authority.get("execution_authority") is False
        and authority.get("trust_root_status") == "approved",
        "approval verifier authority differs",
    )
    validated_trust = _validate_trust_roots(trust_roots)
    trust_binding = verifier_contract.get("trust_roots_binding")
    _require(type(trust_binding) is dict, "approval verifier trust binding differs")
    trust_binding = cast(dict[str, Any], trust_binding)
    _require(
        trust_binding.get("artifact_sha256") == _sha256(_render(validated_trust))
        and trust_binding.get("trust_roots_sha256")
        == validated_trust["trust_roots_sha256"]
        and trust_binding.get("root_count") == len(validated_trust["roots"])
        and trust_binding.get("status") == "approved",
        "approval verifier trust binding differs",
    )
    validated = validate_approval_envelope(
        approval,
        schema=approval_schema,
        verifier_contract=verifier_contract,
    )
    before_time = _utc_now()
    root, public_key_blob, key_line = _select_trust_root(validated, validated_trust, current=before_time)
    statement = cast(dict[str, Any], validated["statement"])
    proof = cast(dict[str, Any], validated["proof"])
    _decode_sshsig_armor(proof["signature_armor"], expected_public_key_blob=public_key_blob)
    payload = signed_payload(statement)
    _run_sshsig_verify(
        payload=payload,
        signature_armor=proof["signature_armor"],
        principal=root["principal"],
        key_line=key_line,
    )
    after_time = _utc_now()
    _require(after_time >= before_time, "verification clock moved backward")
    _select_trust_root(validated, validated_trust, current=after_time)
    report: dict[str, Any] = {
        "approval_file_sha256": _sha256(approval_raw),
        "approval_id": statement["approval_id"],
        "approval_sha256": validated["approval_sha256"],
        "approval_valid_until": statement["expires_at"],
        "atomic_consumption": False,
        "candidate_execution": "forbidden",
        "execution_authority": False,
        "execution_status": AFTER_VERIFICATION_STATUS,
        "model_provider_execution": "forbidden",
        "paid_execution": "forbidden",
        "profile": REPORT_PROFILE,
        "replay_status": "unconsumed_no_atomic_replay_store",
        "report_sha256": None,
        "run_binding_status": "signed_statement_verified_not_bound_to_executor",
        "run_id": statement["run_identity"]["run_id"],
        "run_identity_sha256": statement["run_identity_sha256"],
        "schema_version": 1,
        "signature_verification": "valid",
        "signer_public_key_sha256": statement["signer_public_key_sha256"],
        "status": "owner_approval_signature_verified_execution_forbidden",
        "trust_root_id": statement["trust_root_id"],
        "verified_at": _format_time(after_time),
    }
    report["report_sha256"] = _field_self_hash(report, "report_sha256")
    _validate_schema(report, report_schema, schema_name=REPORT_SCHEMA_PATH.name, label="owner approval verification report")
    _require(report["report_sha256"] == _field_self_hash(report, "report_sha256"), "verification-report self hash differs")
    return report


def _approved_verification_dependencies() -> tuple[dict[str, Any], dict[str, Any]]:
    contract = check_verifier_contract()
    dependencies = _load_dependencies()
    _require(contract["status"] == APPROVED_STATUS, "approval verifier trust root is pending")
    _require(dependencies["trust_roots"]["status"] == "approved", "owner trust roots are pending authorization")
    return contract, dependencies


def verify_approval(
    approval: Mapping[str, Any],
    *,
    approval_raw: bytes,
) -> dict[str, Any]:
    """Verify against only the fixed, checked production dependency set."""

    contract, dependencies = _approved_verification_dependencies()
    return _verify_approval_with_dependencies(
        approval,
        approval_raw=approval_raw,
        trust_roots=dependencies["trust_roots"],
        verifier_contract=contract,
        approval_schema=dependencies["approval_schema"],
        report_schema=dependencies["report_schema"],
        verifier_schema=dependencies["verifier_schema"],
    )


def verify_approval_file(path: pathlib.Path) -> dict[str, Any]:
    """Fail on pending fixed dependencies before opening an approval path."""

    contract, dependencies = _approved_verification_dependencies()
    approval, approval_raw = _read_json(
        path,
        maximum=MAX_APPROVAL_BYTES,
        label="owner approval envelope",
        canonical_profile=CANONICAL_SIGNED_PROFILE,
        require_owner_mode=0o600,
        require_single_link=True,
    )
    return _verify_approval_with_dependencies(
        approval,
        approval_raw=approval_raw,
        trust_roots=dependencies["trust_roots"],
        verifier_contract=contract,
        approval_schema=dependencies["approval_schema"],
        report_schema=dependencies["report_schema"],
        verifier_schema=dependencies["verifier_schema"],
    )


def _write_atomic(path: pathlib.Path, value: Mapping[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temporary_name = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent)
    temporary = pathlib.Path(temporary_name)
    try:
        os.fchmod(descriptor, 0o644)
        with os.fdopen(descriptor, "wb") as handle:
            handle.write(_render(value))
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
    finally:
        temporary.unlink(missing_ok=True)


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    build = commands.add_parser("build", help="build the non-authorizing verifier contract")
    build.add_argument("--output", required=True, type=pathlib.Path)
    check = commands.add_parser("check", help="check the exact verifier contract and dependencies")
    check.add_argument("artifact", nargs="?", type=pathlib.Path, default=VERIFIER_CONTRACT_PATH)
    verify = commands.add_parser("verify", help="verify one owner signature without authorizing execution")
    verify.add_argument("approval", type=pathlib.Path)
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    parser = _parser()
    args = parser.parse_args(argv)
    try:
        if args.command == "build":
            _write_atomic(args.output, build_verifier_contract())
        elif args.command == "check":
            contract = check_verifier_contract(args.artifact)
            print(
                json.dumps(
                    {
                        "execution_authority": False,
                        "execution_status": contract["execution_status"],
                        "profile": contract["profile"],
                        "status": contract["status"],
                    },
                    separators=(",", ":"),
                    sort_keys=True,
                )
            )
        else:
            report = verify_approval_file(args.approval)
            print(_canonical_bytes(report).decode("utf-8"))
    except ApprovalVerificationError as exc:
        parser.error(str(exc))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
