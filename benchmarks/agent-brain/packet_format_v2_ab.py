#!/usr/bin/env python3
"""Privacy-safe, unpaid MCP compact_v2 packet-format A/B for `brain_brief`.

Prompts and product packets exist only in process memory. The retained report
contains logical repo labels, task/prompt hashes, product/runtime hashes,
packet/frame byte counts and hashes, a frozen offline token proxy, parser and
integrity booleans, canonical-projection hashes/parity, aggregate counts, and
fixed promotion gates. It never launches an agent or provider model.
"""

from __future__ import annotations

import argparse
import copy
import hashlib
import json
import math
import os
import pathlib
import re
import statistics
import subprocess
import tempfile
from collections import Counter
from typing import Any, Mapping, Sequence

import profile_brief


REPO_ROOT = pathlib.Path(__file__).resolve().parents[2]
DEFAULT_CORPUS = pathlib.Path(__file__).with_name("brief-profile-corpus-v1.json")
REPORT_SCHEMA_VERSION = 2
SIZE_ATTRIBUTION_SCHEMA_VERSION = 1
RUNNER_IDENTITY_SCHEMA_VERSION = 1
EVIDENCE_ROLE = "development_only_unpaid_brain_brief_packet_format_v2_ab"
FORMAT_ORDER = ("legacy_json", "compact_v2")
COMPACT_MARKER = "entire.brain_brief compact_v2"
COMPACT_V2_LEGEND = "legend ~=absent ^=previous_same_opcode_record_same_column"
PRODUCT_CONTRACT_COMMIT_SHA1 = "172aaa3f361376cc00914e26d570f80b74442b5a"
COMPACT_V2_GOLDEN_SHA256 = "e0bc12b75059a7bba809ef1a66739c49d397852a30271da1b3e2ef4f9a6ca982"
MAX_MCP_FRAME_BYTES = 4 * 1024 * 1024
MAX_MCP_STDOUT_BYTES = 2 * (MAX_MCP_FRAME_BYTES + 128)
TOKEN_PROXY_NAME = "utf8_byte_quads_v1"
TOKEN_PROXY_DEFINITION = "count=0 if empty else ceil(len(exact_inner_utf8_bytes)/4)"
TOKEN_PROXY_DEFINITION_SHA256 = hashlib.sha256(TOKEN_PROXY_DEFINITION.encode("ascii")).hexdigest()
PPM = 1_000_000
INNER_BYTE_REDUCTION_GATE_PPM = 600_000
TOKEN_PROXY_REDUCTION_GATE_PPM = 500_000
SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
TAG_RE = re.compile(r"^[a-z][a-z0-9_]*$")
KEY_RE = re.compile(r"^[a-z][a-z0-9_]*$")
HEX_DIGITS = frozenset("0123456789abcdefABCDEF")
COMPACT_BODY_TAG_ORDER = (
    "task",
    "status",
    "sources",
    "live",
    "live_symbol",
    "facts_status",
    "semantic_status",
    "freshness_axis",
    "freshness_warning",
    "semantic_warning",
    "semantic_partial_failure",
    "blind_spot",
    "edit_file",
    "test_file",
    "likely_file",
    "symbol",
    "relation",
    "relation_evidence",
    "neighbor",
    "runtime_trace",
    "runtime_trace_evidence",
    "test_root",
    "test_suggestion",
    "history",
    "fact",
    "fact_drift",
    "action",
    "pattern",
    "consolidation",
    "theme",
    "guidance",
    "warning",
)
COMPACT_BODY_TAGS = frozenset(COMPACT_BODY_TAG_ORDER)
V2_STRING = "string"
V2_STRINGS = "strings"
V2_BOOL = "bool"
V2_INT = "int"
V2_FLOAT = "float"
V2_KINDS = frozenset((V2_STRING, V2_STRINGS, V2_BOOL, V2_INT, V2_FLOAT))
SAFE_ATOM_RE = re.compile(r"^[A-Za-z_./:@+\-][A-Za-z0-9_./:@+\-]*$")


def _v2_natural(name: str, kind: str) -> tuple[str, str, bool]:
    return (name, kind, False)


def _v2_metadata(name: str, kind: str) -> tuple[str, str, bool]:
    return (name, kind, True)


def _v2_schema(
    tag: str,
    opcode: str,
    *fields: tuple[str, str, bool],
) -> tuple[str, str, tuple[tuple[str, str, bool], ...]]:
    return (tag, opcode, tuple(fields))


def _v2_symbol_schema(tag: str, opcode: str, suggestion: bool = False) -> tuple[Any, ...]:
    fields = [
        _v2_metadata("id", V2_STRING),
        _v2_metadata("kind", V2_STRING),
        _v2_metadata("name", V2_STRING),
        _v2_metadata("short_name", V2_STRING),
        _v2_metadata("file", V2_STRING),
        _v2_metadata("start", V2_INT),
        _v2_metadata("end", V2_INT),
        _v2_metadata("path", V2_STRING),
        _v2_natural("signature", V2_STRING),
        _v2_metadata("language", V2_STRING),
        _v2_metadata("score", V2_INT),
        _v2_metadata("confidence", V2_FLOAT),
        _v2_natural("reason", V2_STRING),
        _v2_metadata("warning_codes", V2_STRINGS),
    ]
    if suggestion:
        fields.append(_v2_natural("suggestion_reason", V2_STRING))
    return _v2_schema(tag, opcode, *fields)


def _v2_relation_schema(tag: str, opcode: str) -> tuple[Any, ...]:
    return _v2_schema(
        tag,
        opcode,
        _v2_metadata("index", V2_INT),
        _v2_metadata("id", V2_STRING),
        _v2_metadata("type", V2_STRING),
        _v2_metadata("from", V2_STRING),
        _v2_metadata("to", V2_STRING),
        _v2_metadata("file", V2_STRING),
        _v2_metadata("start", V2_INT),
        _v2_metadata("end", V2_INT),
        _v2_metadata("path", V2_STRING),
        _v2_metadata("scope", V2_STRING),
        _v2_metadata("resolution", V2_STRING),
        _v2_metadata("target_kind", V2_STRING),
        _v2_metadata("confidence", V2_FLOAT),
        _v2_natural("reason", V2_STRING),
        _v2_metadata("warning_codes", V2_STRINGS),
    )


def _v2_evidence_schema(tag: str, opcode: str) -> tuple[Any, ...]:
    return _v2_schema(
        tag,
        opcode,
        _v2_metadata("owner_index", V2_INT),
        _v2_metadata("index", V2_INT),
        _v2_metadata("kind", V2_STRING),
        _v2_metadata("file", V2_STRING),
        _v2_metadata("start", V2_INT),
        _v2_metadata("end", V2_INT),
        _v2_natural("detail", V2_STRING),
    )


def _v2_warning_schema(tag: str, opcode: str) -> tuple[Any, ...]:
    return _v2_schema(
        tag,
        opcode,
        _v2_metadata("code", V2_STRING),
        _v2_metadata("severity", V2_STRING),
        _v2_metadata("path", V2_STRING),
        _v2_natural("effect", V2_STRING),
        _v2_natural("detail", V2_STRING),
    )


# Frozen independently in Python. This must not be imported from or generated
# by the Go encoder: parity evidence is only meaningful when the decoder has a
# separately maintained schema and metadata-repeat allowlist.
COMPACT_V2_SCHEMAS = (
    _v2_schema("task", "q", _v2_natural("value", V2_STRING)),
    _v2_schema(
        "status",
        "s",
        _v2_metadata("freshness", V2_STRING),
        _v2_metadata("repo_key", V2_STRING),
        _v2_metadata("brain_schema", V2_INT),
        _v2_metadata("branch", V2_STRING),
        _v2_metadata("head", V2_STRING),
        _v2_metadata("dirty", V2_BOOL),
        _v2_metadata("changed_files", V2_INT),
    ),
    _v2_schema(
        "sources",
        "u",
        *(_v2_metadata(name, V2_BOOL) for name in ("seed", "sessions", "semantic", "history", "facts")),
    ),
    _v2_schema(
        "live",
        "l",
        _v2_metadata("staged", V2_STRINGS),
        _v2_metadata("unstaged", V2_STRINGS),
        _v2_metadata("untracked", V2_STRINGS),
        _v2_metadata("changed", V2_STRINGS),
        _v2_natural("diff_stat", V2_STRING),
    ),
    _v2_symbol_schema("live_symbol", "L"),
    _v2_schema(
        "facts_status",
        "F",
        *(
            _v2_metadata(name, V2_INT)
            for name in (
                "facts",
                "distilled",
                "authored",
                "superseded",
                "branches",
                "proposals",
                "verified_facts",
                "verified",
                "stale",
                "orphaned",
                "unverifiable_here",
                "sampled_of",
            )
        ),
    ),
    _v2_schema(
        "semantic_status",
        "S",
        _v2_metadata("provider", V2_STRING),
        _v2_metadata("provider_version", V2_STRING),
        _v2_metadata("schema", V2_STRING),
        _v2_metadata("capabilities", V2_STRINGS),
        *(_v2_metadata(name, V2_INT) for name in ("files", "symbols", "relations", "warnings", "partial_failures")),
    ),
    _v2_schema(
        "freshness_axis",
        "a",
        _v2_metadata("name", V2_STRING),
        _v2_metadata("state", V2_STRING),
        _v2_natural("detail", V2_STRING),
    ),
    _v2_warning_schema("freshness_warning", "w"),
    _v2_warning_schema("semantic_warning", "W"),
    _v2_warning_schema("semantic_partial_failure", "E"),
    _v2_schema(
        "blind_spot",
        "b",
        _v2_metadata("path", V2_STRING),
        _v2_metadata("code", V2_STRING),
        _v2_natural("detail", V2_STRING),
    ),
    _v2_schema("edit_file", "e", _v2_metadata("path", V2_STRING)),
    _v2_schema("test_file", "t", _v2_metadata("path", V2_STRING)),
    _v2_schema("likely_file", "p", _v2_metadata("path", V2_STRING)),
    _v2_symbol_schema("symbol", "y"),
    _v2_relation_schema("relation", "r"),
    _v2_evidence_schema("relation_evidence", "R"),
    _v2_symbol_schema("neighbor", "n"),
    _v2_relation_schema("runtime_trace", "x"),
    _v2_evidence_schema("runtime_trace_evidence", "X"),
    _v2_symbol_schema("test_root", "o"),
    _v2_symbol_schema("test_suggestion", "O", suggestion=True),
    _v2_schema(
        "history",
        "h",
        _v2_metadata("path", V2_STRING),
        _v2_metadata("line", V2_INT),
        _v2_metadata("timestamp", V2_STRING),
        _v2_metadata("score", V2_INT),
        _v2_metadata("matched_terms", V2_STRINGS),
        _v2_natural("excerpt", V2_STRING),
    ),
    _v2_schema(
        "fact",
        "f",
        _v2_metadata("id", V2_STRING),
        _v2_metadata("paths", V2_STRINGS),
        _v2_metadata("kind", V2_STRING),
        _v2_metadata("locus", V2_STRINGS),
        _v2_natural("text", V2_STRING),
        _v2_metadata("branch", V2_STRING),
        _v2_metadata("origin", V2_STRING),
        _v2_metadata("status", V2_STRING),
        _v2_metadata("confidence", V2_STRING),
        _v2_metadata("related_ids", V2_STRINGS),
        _v2_metadata("superseded_by", V2_STRING),
        _v2_metadata("anchors", V2_INT),
        _v2_metadata("verified_anchors", V2_INT),
        _v2_metadata("stale_locus", V2_STRINGS),
    ),
    _v2_schema(
        "fact_drift",
        "d",
        _v2_metadata("id", V2_STRING),
        _v2_metadata("stale_locus", V2_STRINGS),
    ),
    _v2_schema(
        "action",
        "c",
        _v2_metadata("file", V2_STRING),
        _v2_metadata("symbol", V2_STRING),
        _v2_natural("action", V2_STRING),
        _v2_natural("evidence", V2_STRING),
    ),
    _v2_schema(
        "pattern",
        "P",
        _v2_metadata("id", V2_STRING),
        _v2_metadata("type", V2_STRING),
        _v2_metadata("scope", V2_STRING),
        _v2_metadata("kind", V2_STRING),
        _v2_natural("title", V2_STRING),
        _v2_metadata("strength", V2_FLOAT),
        _v2_metadata("strength_label", V2_STRING),
        _v2_metadata("support", V2_INT),
        _v2_metadata("repos", V2_INT),
        _v2_metadata("skill_status", V2_STRING),
        _v2_natural("note", V2_STRING),
        _v2_natural("intent_sig", V2_STRING),
        _v2_natural("gram", V2_STRING),
        _v2_metadata("dossier_status", V2_STRING),
        _v2_metadata("verdict", V2_STRING),
        _v2_metadata("workspace", V2_STRING),
        _v2_metadata("success", V2_INT),
        _v2_metadata("corrected", V2_INT),
        _v2_metadata("neutral", V2_INT),
        _v2_metadata("example_path", V2_STRING),
        _v2_metadata("example_line", V2_INT),
    ),
    _v2_schema(
        "consolidation",
        "C",
        _v2_metadata("pattern_id", V2_STRING),
        _v2_metadata("type", V2_STRING),
        _v2_natural("title", V2_STRING),
        _v2_natural("trigger", V2_STRING),
        _v2_metadata("confidence", V2_FLOAT),
        _v2_metadata("status", V2_STRING),
        _v2_metadata("verdict", V2_STRING),
        _v2_natural("workflow", V2_STRINGS),
        _v2_natural("verification", V2_STRINGS),
        _v2_natural("failure_modes", V2_STRINGS),
        _v2_metadata("anchor_transcript", V2_STRING),
        _v2_metadata("anchor_start", V2_INT),
        _v2_metadata("anchor_end", V2_INT),
        _v2_metadata("anchor_outcome", V2_STRING),
    ),
    _v2_schema(
        "theme",
        "T",
        _v2_metadata("id", V2_STRING),
        _v2_natural("title", V2_STRING),
        _v2_natural("description", V2_STRING),
        _v2_metadata("shape", V2_STRING),
        _v2_metadata("support", V2_INT),
        _v2_metadata("strength", V2_FLOAT),
        _v2_metadata("status", V2_STRING),
        _v2_metadata("verdict", V2_STRING),
    ),
    _v2_schema("guidance", "g", _v2_natural("text", V2_STRING)),
    _v2_schema(
        "warning",
        "!",
        _v2_metadata("source", V2_STRING),
        _v2_natural("text", V2_STRING),
    ),
)

COMPACT_V2_BY_TAG = {schema[0]: schema for schema in COMPACT_V2_SCHEMAS}
COMPACT_V2_BY_OPCODE = {schema[1]: schema for schema in COMPACT_V2_SCHEMAS}
if (
    len(COMPACT_V2_BY_TAG) != len(COMPACT_V2_SCHEMAS)
    or len(COMPACT_V2_BY_OPCODE) != len(COMPACT_V2_SCHEMAS)
    or set(COMPACT_V2_BY_TAG) != COMPACT_BODY_TAGS
):
    raise RuntimeError("invalid frozen compact_v2 schema vocabulary")
for _schema_tag, _schema_opcode, _schema_fields in COMPACT_V2_SCHEMAS:
    if (
        len(_schema_opcode) != 1
        or "\t" in _schema_opcode
        or "\n" in _schema_opcode
        or len({field[0] for field in _schema_fields}) != len(_schema_fields)
        or any(field[1] not in V2_KINDS for field in _schema_fields)
    ):
        raise RuntimeError(f"invalid frozen compact_v2 schema: {_schema_tag}")
class PacketABError(RuntimeError):
    """Fail-closed corpus, metadata, report, or runner error."""


class MCPShapeError(PacketABError):
    """MCP output was not the exact bounded response shape."""


class LegacyPacketError(PacketABError):
    """The legacy JSON packet was not valid for independent projection."""


class CompactPacketError(PacketABError):
    """The compact packet grammar or projection was invalid."""


class CompactIntegrityError(CompactPacketError):
    """The compact packet body count or checksum did not verify."""


OMIT = object()


def canonical_json_bytes(value: Any) -> bytes:
    return json.dumps(
        value,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
        allow_nan=False,
    ).encode("utf-8")


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def attach_self_hash(value: Mapping[str, Any], field: str) -> dict[str, Any]:
    result = copy.deepcopy(dict(value))
    result.pop(field, None)
    result[field] = sha256_bytes(canonical_json_bytes(result))
    return result


def verify_self_hash(value: Mapping[str, Any], field: str) -> bool:
    actual = value.get(field)
    if not isinstance(actual, str) or not SHA256_RE.fullmatch(actual):
        return False
    unhashed = copy.deepcopy(dict(value))
    unhashed.pop(field, None)
    return actual == sha256_bytes(canonical_json_bytes(unhashed))


def compact_v2_contract_fingerprint_sha256() -> str:
    contract = {
        "schema_version": 1,
        "format_order": list(FORMAT_ORDER),
        "marker": COMPACT_MARKER,
        "legend": COMPACT_V2_LEGEND,
        "safe_atom_pattern": SAFE_ATOM_RE.pattern,
        "schemas": [
            {
                "tag": tag,
                "opcode": opcode,
                "fields": [
                    {
                        "name": name,
                        "kind": kind,
                        "metadata_repeat_allowed": reference_allowed,
                    }
                    for name, kind, reference_allowed in fields
                ],
            }
            for tag, opcode, fields in COMPACT_V2_SCHEMAS
        ],
        "wire_rules": {
            "omission_cell": "~",
            "repeat_cell": "^",
            "repeat_scope": "previous_same_opcode_record_same_column",
            "natural_language_repeat_allowed": False,
            "singleton_form": "keyed",
            "multi_record_form": "positional_only_when_strictly_smaller_including_newlines",
            "equal_size_form": "keyed",
            "footer": "end_tab_nonnegative_record_count_tab_lowercase_sha256_body_final_newline",
        },
        "gates": {
            "full_task_count": 114,
            "median_inner_byte_reduction_ppm_minimum": INNER_BYTE_REDUCTION_GATE_PPM,
            "median_token_proxy_reduction_ppm_minimum": TOKEN_PROXY_REDUCTION_GATE_PPM,
            "minimum_per_task_inner_byte_reduction_ppm": 0,
            "all_pairs_parse_integrity_and_projection_parity_required": True,
        },
        "offline_token_proxy": {
            "name": TOKEN_PROXY_NAME,
            "definition_sha256": TOKEN_PROXY_DEFINITION_SHA256,
        },
    }
    return sha256_bytes(canonical_json_bytes(contract))


def _require_product_contract_ancestor() -> None:
    try:
        result = subprocess.run(
            ("git", "merge-base", "--is-ancestor", PRODUCT_CONTRACT_COMMIT_SHA1, "HEAD"),
            cwd=REPO_ROOT,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            check=False,
            timeout=10.0,
        )
    except (OSError, subprocess.TimeoutExpired) as exc:
        raise PacketABError("cannot verify compact_v2 product contract ancestry") from exc
    if result.returncode != 0:
        raise PacketABError("compact_v2 product contract commit is not an ancestor of HEAD")


def build_runner_identity() -> dict[str, Any]:
    _require_product_contract_ancestor()
    runner_source = pathlib.Path(__file__).resolve()
    profile_source_value = getattr(profile_brief, "__file__", None)
    if not isinstance(profile_source_value, str):
        raise PacketABError("profile_brief source identity is unavailable")
    profile_source = pathlib.Path(profile_source_value).resolve()
    golden = REPO_ROOT / "internal/cli/testdata/brain_brief_compact_v2.golden"
    try:
        runner_sha256 = sha256_bytes(runner_source.read_bytes())
        profile_sha256 = sha256_bytes(profile_source.read_bytes())
        golden_sha256 = sha256_bytes(golden.read_bytes())
    except OSError as exc:
        raise PacketABError("cannot hash runner identity inputs") from exc
    if golden_sha256 != COMPACT_V2_GOLDEN_SHA256:
        raise PacketABError("compact_v2 golden identity mismatch")
    body = {
        "schema_version": RUNNER_IDENTITY_SCHEMA_VERSION,
        "runner_source_sha256": runner_sha256,
        "profile_brief_source_sha256": profile_sha256,
        "product_contract_commit_sha1": PRODUCT_CONTRACT_COMMIT_SHA1,
        "compact_v2_golden_sha256": golden_sha256,
        "compact_v2_contract_fingerprint_sha256": compact_v2_contract_fingerprint_sha256(),
    }
    return attach_self_hash(body, "runner_identity_sha256")


def verify_runner_identity(value: Mapping[str, Any]) -> bool:
    if not verify_self_hash(value, "runner_identity_sha256"):
        return False
    try:
        expected = build_runner_identity()
    except PacketABError:
        return False
    return canonical_json_bytes(value) == canonical_json_bytes(expected)


def _strict_json(data: bytes, role: str) -> Any:
    def reject_duplicates(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
        result: dict[str, Any] = {}
        for key, value in pairs:
            if key in result:
                raise PacketABError(f"duplicate key in {role}")
            result[key] = value
        return result

    def reject_constant(_: str) -> Any:
        raise PacketABError(f"non-finite number in {role}")

    try:
        return json.loads(
            data,
            object_pairs_hook=reject_duplicates,
            parse_constant=reject_constant,
        )
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise PacketABError(f"invalid UTF-8 JSON in {role}") from exc


def offline_token_proxy_count(data: bytes) -> int:
    """Frozen transparent proxy; not a provider tokenizer or billable count."""

    return 0 if not data else (len(data) + 3) // 4


def reduction_ppm(baseline: int, candidate: int) -> int:
    if baseline <= 0:
        raise PacketABError("reduction baseline must be positive")
    return ((baseline - candidate) * PPM) // baseline


def _frame_request(message: Mapping[str, Any]) -> bytes:
    payload = canonical_json_bytes(message)
    return f"Content-Length: {len(payload)}\r\n\r\n".encode("ascii") + payload


def parse_mcp_response_frames(data: bytes) -> list[tuple[dict[str, Any], bytes]]:
    if not data or len(data) > MAX_MCP_STDOUT_BYTES:
        raise MCPShapeError("MCP stdout is empty or exceeds the two-frame ceiling")
    frames: list[tuple[dict[str, Any], bytes]] = []
    cursor = 0
    while cursor < len(data):
        frame_start = cursor
        header_end = data.find(b"\r\n\r\n", cursor)
        if header_end < 0:
            raise MCPShapeError("MCP response header is incomplete")
        header = data[cursor:header_end]
        match = re.fullmatch(rb"Content-Length: (0|[1-9][0-9]*)", header)
        if match is None:
            raise MCPShapeError("MCP response header is not canonical Content-Length")
        length = int(match.group(1))
        if length <= 0 or length > MAX_MCP_FRAME_BYTES:
            raise MCPShapeError("MCP response frame length is out of range")
        payload_start = header_end + 4
        payload_end = payload_start + length
        if payload_end > len(data):
            raise MCPShapeError("MCP response frame is truncated")
        payload = _strict_json(data[payload_start:payload_end], "MCP response")
        if not isinstance(payload, dict):
            raise MCPShapeError("MCP response must be an object")
        frames.append((payload, data[frame_start:payload_end]))
        cursor = payload_end
    if len(frames) != 2:
        raise MCPShapeError("MCP response must contain exactly two frames")
    return frames


def extract_mcp_text(response: Mapping[str, Any], expected_id: int) -> str:
    if set(response) != {"jsonrpc", "id", "result"}:
        raise MCPShapeError("MCP response fields are not exact")
    if response["jsonrpc"] != "2.0" or response["id"] != expected_id:
        raise MCPShapeError("MCP response identity mismatch")
    result = response["result"]
    if not isinstance(result, dict) or set(result) != {"content"}:
        raise MCPShapeError("MCP result fields are not exact")
    content = result["content"]
    if not isinstance(content, list) or len(content) != 1:
        raise MCPShapeError("MCP result must contain exactly one content row")
    row = content[0]
    if not isinstance(row, dict) or set(row) != {"type", "text"} or row["type"] != "text":
        raise MCPShapeError("MCP content row fields are not exact")
    text = row["text"]
    if not isinstance(text, str):
        raise MCPShapeError("MCP content text must be a string")
    return text


V2_INT_RE = re.compile(r"^-?(?:0|[1-9][0-9]*)$")
V2_FLOAT_RE = re.compile(
    r"^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:e[+-][0-9]{2,})?$"
)
V2_LOWER_SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
V2_INT_MIN = -(1 << 63)
V2_INT_MAX = (1 << 63) - 1
V2_NAMED_ESCAPES = {
    "a": "\a",
    "b": "\b",
    "f": "\f",
    "n": "\n",
    "r": "\r",
    "t": "\t",
    "v": "\v",
    "\\": "\\",
    '"': '"',
}
V2_QUOTE_ESCAPES = {
    "\a": r"\a",
    "\b": r"\b",
    "\f": r"\f",
    "\n": r"\n",
    "\r": r"\r",
    "\t": r"\t",
    "\v": r"\v",
    "\\": r"\\",
    '"': r'\"',
}


def _v2_safe_atom(value: str) -> bool:
    return value not in ("", "~", "^") and SAFE_ATOM_RE.fullmatch(value) is not None


def _v2_scan_quoted_end(text: str, start: int) -> int:
    if start >= len(text) or text[start] != '"':
        raise CompactPacketError("compact_v2 quoted value does not start with a quote")
    cursor = start + 1
    while cursor < len(text):
        character = text[cursor]
        if character == "\\":
            cursor += 2
            continue
        if character == '"':
            return cursor + 1
        if character in "\n\r" or ord(character) < 0x20:
            raise CompactPacketError("compact_v2 quoted value has an unescaped control")
        cursor += 1
    raise CompactPacketError("unterminated compact_v2 quoted value")


def _v2_unquote_go(raw: str) -> str:
    if len(raw) < 2 or raw[0] != '"' or raw[-1] != '"':
        raise CompactPacketError("invalid compact_v2 Go-quoted string")
    result: list[str] = []
    cursor = 1
    end = len(raw) - 1
    while cursor < end:
        character = raw[cursor]
        codepoint = ord(character)
        if character == '"' or codepoint < 0x20 or 0xD800 <= codepoint <= 0xDFFF:
            raise CompactPacketError("invalid unescaped character in compact_v2 string")
        if character != "\\":
            result.append(character)
            cursor += 1
            continue
        cursor += 1
        if cursor >= end:
            raise CompactPacketError("incomplete compact_v2 string escape")
        escape = raw[cursor]
        if escape in V2_NAMED_ESCAPES:
            result.append(V2_NAMED_ESCAPES[escape])
            cursor += 1
            continue
        digits = {"x": 2, "u": 4, "U": 8}.get(escape)
        if digits is None:
            raise CompactPacketError("unsupported compact_v2 string escape")
        start = cursor + 1
        stop = start + digits
        if stop > end or any(character not in HEX_DIGITS for character in raw[start:stop]):
            raise CompactPacketError("invalid hexadecimal compact_v2 string escape")
        value = int(raw[start:stop], 16)
        if (escape == "x" and value >= 0x80) or value > 0x10FFFF or 0xD800 <= value <= 0xDFFF:
            raise CompactPacketError("compact_v2 string escape is not a Unicode scalar")
        result.append(chr(value))
        cursor = stop
    return "".join(result)


def _v2_go_quote(value: str) -> str:
    parts = ['"']
    for character in value:
        if character in V2_QUOTE_ESCAPES:
            parts.append(V2_QUOTE_ESCAPES[character])
            continue
        codepoint = ord(character)
        if 0xD800 <= codepoint <= 0xDFFF:
            raise CompactPacketError("compact_v2 string contains a surrogate")
        if codepoint < 0x20 or codepoint == 0x7F:
            parts.append(f"\\x{codepoint:02x}")
        elif not character.isprintable():
            if codepoint <= 0xFFFF:
                parts.append(f"\\u{codepoint:04x}")
            else:
                parts.append(f"\\U{codepoint:08x}")
        else:
            parts.append(character)
    parts.append('"')
    return "".join(parts)


def _v2_parse_canonical_quote(raw: str) -> str:
    value = _v2_unquote_go(raw)
    if _v2_go_quote(value) != raw:
        raise CompactPacketError("noncanonical compact_v2 quoted string")
    return value


def _v2_parse_integer(encoded: str) -> int:
    if V2_INT_RE.fullmatch(encoded) is None:
        raise CompactPacketError("invalid compact_v2 integer")
    value = int(encoded)
    if value < V2_INT_MIN or value > V2_INT_MAX or str(value) != encoded:
        raise CompactPacketError("noncanonical or out-of-range compact_v2 integer")
    return value


def _v2_go_format_float(value: float) -> str:
    """Independently reproduce strconv.FormatFloat(value, 'g', -1, 64)."""

    if not math.isfinite(value):
        raise CompactPacketError("compact_v2 float is not finite")
    negative = math.copysign(1.0, value) < 0
    magnitude = abs(value)
    if magnitude == 0:
        return "-0" if negative else "0"
    shortest = repr(magnitude).lower()
    if "e" in shortest:
        mantissa, exponent_text = shortest.split("e", 1)
        exponent = int(exponent_text)
    else:
        mantissa = shortest
        exponent = 0
    if "." in mantissa:
        whole, fraction = mantissa.split(".", 1)
    else:
        whole, fraction = mantissa, ""
    combined = whole + fraction
    leading = len(combined) - len(combined.lstrip("0"))
    digits = combined[leading:].rstrip("0")
    if not digits:
        return "-0" if negative else "0"
    decimal_point = len(whole) - leading + exponent
    decimal_exponent = decimal_point - 1
    if decimal_exponent < -4 or decimal_exponent >= 6:
        mantissa_text = digits[0]
        if len(digits) > 1:
            mantissa_text += "." + digits[1:]
        sign = "+" if decimal_exponent >= 0 else "-"
        formatted = f"{mantissa_text}e{sign}{abs(decimal_exponent):02d}"
    elif decimal_point <= 0:
        formatted = "0." + "0" * (-decimal_point) + digits
    elif decimal_point >= len(digits):
        formatted = digits + "0" * (decimal_point - len(digits))
    else:
        formatted = digits[:decimal_point] + "." + digits[decimal_point:]
    return "-" + formatted if negative else formatted


def _v2_parse_float(encoded: str) -> int | float:
    if V2_FLOAT_RE.fullmatch(encoded) is None:
        raise CompactPacketError("invalid compact_v2 float")
    value = float(encoded)
    if _v2_go_format_float(value) != encoded:
        raise CompactPacketError("noncanonical compact_v2 float")
    # Match the independently parsed legacy/v1 projection's JSON number shape.
    return value if "." in encoded or "e" in encoded else int(encoded)


def _v2_parse_array(encoded: str, *, positional: bool) -> list[str]:
    if len(encoded) < 2 or encoded[0] != "[" or encoded[-1] != "]":
        raise CompactPacketError("invalid compact_v2 string array")
    if encoded == "[]":
        return []
    values: list[str] = []
    cursor = 1
    end = len(encoded) - 1
    while cursor < end:
        if encoded[cursor] == '"':
            stop = _v2_scan_quoted_end(encoded, cursor)
            if stop > end:
                raise CompactPacketError("compact_v2 array string exceeds its boundary")
            raw = encoded[cursor:stop]
            value = _v2_parse_canonical_quote(raw)
            if positional and _v2_safe_atom(value):
                raise CompactPacketError("compact_v2 safely atomic array item is quoted")
            values.append(value)
            cursor = stop
        else:
            if not positional:
                raise CompactPacketError("keyed compact_v2 arrays require quoted strings")
            stop = cursor
            while stop < end and encoded[stop] != ",":
                stop += 1
            value = encoded[cursor:stop]
            if not _v2_safe_atom(value):
                raise CompactPacketError("unsafe compact_v2 array atom")
            values.append(value)
            cursor = stop
        if cursor == end:
            break
        if encoded[cursor] != ",":
            raise CompactPacketError("compact_v2 array values are not comma separated")
        cursor += 1
        if cursor == end:
            raise CompactPacketError("compact_v2 array has a trailing comma")
    return values


def _v2_decode_cell(kind: str, encoded: str, *, positional: bool) -> Any:
    if kind == V2_STRING:
        if positional and not encoded.startswith('"'):
            if not _v2_safe_atom(encoded):
                raise CompactPacketError("unsafe compact_v2 string atom")
            return encoded
        value = _v2_parse_canonical_quote(encoded)
        if positional and _v2_safe_atom(value):
            raise CompactPacketError("compact_v2 safely atomic string is quoted")
        return value
    if kind == V2_STRINGS:
        return _v2_parse_array(encoded, positional=positional)
    if kind == V2_BOOL:
        if positional:
            if encoded not in ("0", "1"):
                raise CompactPacketError("invalid positional compact_v2 boolean")
            return encoded == "1"
        if encoded not in ("false", "true"):
            raise CompactPacketError("invalid keyed compact_v2 boolean")
        return encoded == "true"
    if kind == V2_INT:
        return _v2_parse_integer(encoded)
    if kind == V2_FLOAT:
        return _v2_parse_float(encoded)
    raise CompactPacketError("unknown compact_v2 field kind")


def _v2_cell_to_v1_raw(kind: str, encoded: str) -> tuple[Any, str]:
    value = _v2_decode_cell(kind, encoded, positional=True)
    if kind == V2_STRING:
        return value, _v2_go_quote(value)
    if kind == V2_STRINGS:
        return value, "[" + ",".join(_v2_go_quote(item) for item in value) + "]"
    if kind == V2_BOOL:
        return value, "true" if value else "false"
    return value, encoded


def _v2_encode_positional_cell(kind: str, raw: str) -> str:
    value = _v2_decode_cell(kind, raw, positional=False)
    if kind == V2_STRING:
        return value if _v2_safe_atom(value) else raw
    if kind == V2_STRINGS:
        return "[" + ",".join(
            item if _v2_safe_atom(item) else _v2_go_quote(item) for item in value
        ) + "]"
    if kind == V2_BOOL:
        return "1" if value else "0"
    return raw


def _v2_render_keyed_record(tag: str, raw_fields: Sequence[tuple[str, str]]) -> str:
    if not raw_fields:
        return tag
    return tag + " " + " ".join(f"{name}={raw}" for name, raw in raw_fields)


def _v2_encode_positional_row(
    schema: tuple[Any, ...],
    raw_fields: Sequence[tuple[str, str]],
    prior: Sequence[str] | None,
) -> tuple[str, list[str]]:
    positions = {field[0]: index for index, field in enumerate(schema[2])}
    cells = ["~"] * len(schema[2])
    last = -1
    for name, raw in raw_fields:
        position = positions.get(name)
        if position is None or position <= last:
            raise CompactPacketError("compact_v2 raw family fields are invalid")
        cells[position] = _v2_encode_positional_cell(schema[2][position][1], raw)
        last = position
    encoded = [schema[1]]
    for index in range(last + 1):
        cell = cells[index]
        if (
            schema[2][index][2]
            and cell != "~"
            and prior is not None
            and len(prior) == len(cells)
            and prior[index] == cell
            and len(cell) > 1
        ):
            encoded.append("^")
        else:
            encoded.append(cell)
    return "\t".join(encoded), cells


def _v2_expected_positional(
    schema: tuple[Any, ...],
    family: Sequence[Sequence[tuple[str, str]]],
) -> bool:
    if len(family) <= 1:
        return False
    keyed_bytes = sum(
        len((_v2_render_keyed_record(schema[0], fields) + "\n").encode("utf-8"))
        for fields in family
    )
    positional_bytes = len((_v2_declaration(schema) + "\n").encode("utf-8"))
    prior: Sequence[str] | None = None
    for fields in family:
        row, cells = _v2_encode_positional_row(schema, fields, prior)
        positional_bytes += len((row + "\n").encode("utf-8"))
        prior = cells
    # Strictly shorter wins; exact ties are keyed and no family may grow.
    return positional_bytes < keyed_bytes


def _v2_parse_keyed_raw_fields(line: str) -> tuple[str, list[tuple[str, str]]]:
    space = line.find(" ")
    if space < 0:
        tag = line
        remainder = ""
    else:
        tag = line[:space]
        remainder = line[space + 1 :]
        if remainder == "":
            raise CompactPacketError("compact_v2 keyed record has trailing space")
    if TAG_RE.fullmatch(tag) is None:
        raise CompactPacketError("invalid compact_v2 keyed tag")
    fields: list[tuple[str, str]] = []
    cursor = 0
    while cursor < len(remainder):
        equals = remainder.find("=", cursor)
        if equals <= cursor:
            raise CompactPacketError("invalid compact_v2 keyed field")
        key = remainder[cursor:equals]
        if KEY_RE.fullmatch(key) is None:
            raise CompactPacketError("invalid compact_v2 keyed field name")
        cursor = equals + 1
        if cursor >= len(remainder):
            raise CompactPacketError("compact_v2 keyed field has no value")
        start = cursor
        if remainder[cursor] == '"':
            cursor = _v2_scan_quoted_end(remainder, cursor)
        elif remainder[cursor] == "[":
            cursor += 1
            while cursor < len(remainder) and remainder[cursor] != "]":
                if remainder[cursor] == '"':
                    cursor = _v2_scan_quoted_end(remainder, cursor)
                else:
                    cursor += 1
            if cursor >= len(remainder):
                raise CompactPacketError("unterminated compact_v2 keyed array")
            cursor += 1
        else:
            next_space = remainder.find(" ", cursor)
            cursor = len(remainder) if next_space < 0 else next_space
        fields.append((key, remainder[start:cursor]))
        if cursor < len(remainder):
            if remainder[cursor] != " " or cursor + 1 == len(remainder) or remainder[cursor + 1] == " ":
                raise CompactPacketError("noncanonical compact_v2 keyed field separator")
            cursor += 1
    return tag, fields


def _v2_parse_keyed_record(
    line: str,
) -> tuple[dict[str, Any], list[tuple[str, str]], dict[str, int]]:
    tag, raw_fields = _v2_parse_keyed_raw_fields(line)
    schema = COMPACT_V2_BY_TAG.get(tag)
    if schema is None:
        raise CompactPacketError("unknown compact_v2 keyed tag")
    positions = {field[0]: index for index, field in enumerate(schema[2])}
    last = -1
    fields: list[list[Any]] = []
    for key, encoded in raw_fields:
        position = positions.get(key)
        if position is None or position <= last:
            raise CompactPacketError("compact_v2 keyed fields are unknown, duplicate, or out of order")
        name, kind, _ = schema[2][position]
        fields.append([name, _v2_decode_cell(kind, encoded, positional=False)])
        last = position
    return {"tag": tag, "fields": fields}, raw_fields, {
        "explicit_cell_count": len(fields),
        "explicit_null_cell_count": 0,
        "repeat_reference_cell_count": 0,
        "implicit_trailing_omission_count": len(schema[2]) - (last + 1),
    }


def _v2_declaration(schema: tuple[Any, ...]) -> str:
    return f"@{schema[1]}={schema[0]}({','.join(field[0] for field in schema[2])})"


def _v2_parse_positional_record(
    schema: tuple[Any, ...],
    line: str,
    prior: Sequence[str] | None,
) -> tuple[
    dict[str, Any],
    list[tuple[str, str]],
    list[str],
    dict[str, int],
]:
    parts = line.split("\t")
    if parts[0] != schema[1] or len(parts) - 1 > len(schema[2]):
        raise CompactPacketError("compact_v2 positional width is invalid")
    if len(parts) > 1 and parts[-1] == "~":
        raise CompactPacketError("compact_v2 positional row has redundant trailing omission")
    cells = ["~"] * len(schema[2])
    fields: list[list[Any]] = []
    raw_fields: list[tuple[str, str]] = []
    stats = {
        "explicit_cell_count": 0,
        "explicit_null_cell_count": 0,
        "repeat_reference_cell_count": 0,
        "implicit_trailing_omission_count": len(schema[2]) - (len(parts) - 1),
    }
    for index, encoded in enumerate(parts[1:]):
        name, kind, reference_allowed = schema[2][index]
        if encoded == "~":
            stats["explicit_null_cell_count"] += 1
            continue
        if encoded == "^":
            if (
                not reference_allowed
                or prior is None
                or len(prior) != len(cells)
                or prior[index] == "~"
            ):
                raise CompactPacketError("invalid compact_v2 repeat reference")
            resolved = prior[index]
            cells[index] = resolved
            value, raw = _v2_cell_to_v1_raw(kind, resolved)
            fields.append([name, value])
            raw_fields.append((name, raw))
            stats["repeat_reference_cell_count"] += 1
            continue
        value, raw = _v2_cell_to_v1_raw(kind, encoded)
        if (
            reference_allowed
            and prior is not None
            and len(prior) == len(cells)
            and prior[index] == encoded
            and len(encoded) > 1
        ):
            raise CompactPacketError("compact_v2 missed a required metadata repeat reference")
        cells[index] = encoded
        fields.append([name, value])
        raw_fields.append((name, raw))
        stats["explicit_cell_count"] += 1
    return {"tag": schema[0], "fields": fields}, raw_fields, cells, stats


def _v2_size_attribution(
    data: bytes,
    body_lines: Sequence[str],
    footer_line: str,
    line_roles: Sequence[tuple[str, str, Mapping[str, int]]],
) -> dict[str, Any]:
    marker_bytes = len(COMPACT_MARKER.encode("utf-8"))
    legend_bytes = len(COMPACT_V2_LEGEND.encode("utf-8"))
    footer_bytes = len(footer_line.encode("utf-8"))
    declaration_bytes = 0
    keyed_bytes = 0
    positional_bytes = 0
    counts = Counter()
    per_tag = {
        tag: {
            "record_count": 0,
            "keyed_record_count": 0,
            "positional_record_count": 0,
            "record_encoded_byte_count": 0,
            "declaration_count": 0,
            "declaration_encoded_byte_count": 0,
            "explicit_cell_count": 0,
            "explicit_null_cell_count": 0,
            "repeat_reference_cell_count": 0,
            "implicit_trailing_omission_count": 0,
        }
        for tag in COMPACT_BODY_TAG_ORDER
    }
    for line, (role, tag, stats) in zip(body_lines, line_roles):
        encoded_bytes = len(line.encode("utf-8"))
        if role == "declaration":
            declaration_bytes += encoded_bytes
            counts["declaration_count"] += 1
            per_tag[tag]["declaration_count"] += 1
            per_tag[tag]["declaration_encoded_byte_count"] += encoded_bytes
            continue
        counts["body_record_count"] += 1
        counts[role + "_record_count"] += 1
        if role == "keyed":
            keyed_bytes += encoded_bytes
        elif role == "positional":
            positional_bytes += encoded_bytes
        else:
            raise CompactIntegrityError("unknown compact_v2 attribution line role")
        target = per_tag[tag]
        target["record_count"] += 1
        target[role + "_record_count"] += 1
        target["record_encoded_byte_count"] += encoded_bytes
        for key, value in stats.items():
            target[key] += value
            counts[key] += value
    newline_bytes = data.count(b"\n")
    partition = {
        "marker_byte_count": marker_bytes,
        "legend_byte_count": legend_bytes,
        "newline_byte_count": newline_bytes,
        "footer_byte_count": footer_bytes,
        "declaration_byte_count": declaration_bytes,
        "keyed_record_byte_count": keyed_bytes,
        "positional_record_byte_count": positional_bytes,
    }
    partition_sum = sum(partition.values())
    per_tag_record_bytes = sum(row["record_encoded_byte_count"] for row in per_tag.values())
    per_tag_declaration_bytes = sum(row["declaration_encoded_byte_count"] for row in per_tag.values())
    invariants = {
        "exact_inner_byte_count": len(data),
        "partition_byte_count_sum": partition_sum,
        "partition_delta_byte_count": partition_sum - len(data),
        "record_encoded_byte_count_sum": keyed_bytes + positional_bytes,
        "per_tag_record_encoded_byte_count_sum": per_tag_record_bytes,
        "per_tag_record_encoded_delta_byte_count": per_tag_record_bytes - keyed_bytes - positional_bytes,
        "per_tag_declaration_encoded_byte_count_sum": per_tag_declaration_bytes,
        "per_tag_declaration_encoded_delta_byte_count": per_tag_declaration_bytes - declaration_bytes,
    }
    if any(value != 0 for key, value in invariants.items() if key.endswith("delta_byte_count")):
        raise CompactIntegrityError("compact_v2 numeric size attribution does not partition the packet")
    return {
        "schema_version": SIZE_ATTRIBUTION_SCHEMA_VERSION,
        "structural_partition": partition,
        "counts": dict(sorted(counts.items())),
        "per_tag": per_tag,
        "accounting_invariants": invariants,
    }


def parse_compact_v2_packet(data: bytes) -> dict[str, Any]:
    try:
        text = data.decode("utf-8")
    except UnicodeDecodeError as exc:
        raise CompactPacketError("compact_v2 packet is not UTF-8") from exc
    if not text.endswith("\n"):
        raise CompactIntegrityError("compact_v2 packet is missing its final newline")
    lines = text[:-1].split("\n")
    if len(lines) < 4 or lines[0] != COMPACT_MARKER:
        raise CompactPacketError("compact_v2 marker mismatch")
    if lines[1] != COMPACT_V2_LEGEND:
        raise CompactPacketError("compact_v2 legend mismatch")
    footer_parts = lines[-1].split("\t")
    if len(footer_parts) != 3 or footer_parts[0] != "end":
        raise CompactIntegrityError("compact_v2 footer shape is invalid")
    expected_count = _v2_parse_integer(footer_parts[1])
    if expected_count < 0:
        raise CompactIntegrityError("compact_v2 footer count is negative")
    if V2_LOWER_SHA256_RE.fullmatch(footer_parts[2]) is None:
        raise CompactIntegrityError("compact_v2 footer digest is not lowercase SHA-256")
    footer_start = data.rfind(b"\nend\t")
    if footer_start < 0:
        raise CompactIntegrityError("compact_v2 footer boundary is missing")
    body = data[: footer_start + 1]
    if sha256_bytes(body) != footer_parts[2]:
        raise CompactIntegrityError("compact_v2 body checksum mismatch")

    declared: set[str] = set()
    used: set[str] = set()
    pending_declaration: str | None = None
    previous: dict[str, list[str]] = {}
    records: list[dict[str, Any]] = []
    raw_families: dict[str, list[list[tuple[str, str]]]] = {}
    record_modes: list[tuple[str, str]] = []
    line_roles: list[tuple[str, str, Mapping[str, int]]] = []
    body_lines = lines[2:-1]
    for line in body_lines:
        if line == "":
            raise CompactPacketError("compact_v2 body contains an empty line")
        if line.startswith("@"):
            if pending_declaration is not None or len(line) < 2:
                raise CompactPacketError("compact_v2 declaration is empty or not immediately used")
            schema = COMPACT_V2_BY_OPCODE.get(line[1])
            if schema is None or line != _v2_declaration(schema):
                raise CompactPacketError("unknown or noncanonical compact_v2 declaration")
            opcode = schema[1]
            if opcode in declared or any(tag == schema[0] for tag, _ in record_modes):
                raise CompactPacketError("duplicate or late compact_v2 declaration")
            declared.add(opcode)
            pending_declaration = opcode
            line_roles.append(("declaration", schema[0], {}))
            continue

        schema = COMPACT_V2_BY_OPCODE.get(line[0])
        positional = schema is not None and (len(line) == 1 or line[1] == "\t")
        if positional:
            assert schema is not None
            opcode = schema[1]
            if opcode not in declared:
                raise CompactPacketError("compact_v2 positional opcode was not declared")
            if pending_declaration is not None and pending_declaration != opcode:
                raise CompactPacketError("compact_v2 declaration was not immediately followed by its opcode")
            record, raw_fields, cells, stats = _v2_parse_positional_record(
                schema,
                line,
                previous.get(opcode),
            )
            previous[opcode] = cells
            used.add(opcode)
            pending_declaration = None
            records.append(record)
            raw_families.setdefault(schema[0], []).append(raw_fields)
            record_modes.append((schema[0], "positional"))
            line_roles.append(("positional", schema[0], stats))
            continue

        if pending_declaration is not None:
            raise CompactPacketError("compact_v2 declaration was not immediately followed by a positional row")
        record, raw_fields, stats = _v2_parse_keyed_record(line)
        records.append(record)
        raw_families.setdefault(record["tag"], []).append(raw_fields)
        record_modes.append((record["tag"], "keyed"))
        line_roles.append(("keyed", record["tag"], stats))

    if pending_declaration is not None or declared != used:
        raise CompactPacketError("compact_v2 has an unused declaration")
    if len(records) != expected_count:
        raise CompactIntegrityError("compact_v2 body record count mismatch")
    family_modes: dict[str, str] = {}
    for tag, mode in record_modes:
        prior_mode = family_modes.setdefault(tag, mode)
        if prior_mode != mode:
            raise CompactPacketError("compact_v2 family mixes keyed and positional rows")
    for tag, family in raw_families.items():
        schema = COMPACT_V2_BY_TAG[tag]
        expected_mode = "positional" if _v2_expected_positional(schema, family) else "keyed"
        if family_modes[tag] != expected_mode:
            raise CompactPacketError("compact_v2 family violates the exact shorter-form rule")
        if (expected_mode == "positional") != (schema[1] in declared):
            raise CompactPacketError("compact_v2 declaration does not match the selected family form")
    attribution = _v2_size_attribution(data, body_lines, lines[-1], line_roles)
    return {
        "records": records,
        "body_record_count": len(records),
        "body_sha256": sha256_bytes(body),
        "structural_size_attribution": attribution,
    }


def _object(value: Any, role: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise LegacyPacketError(f"{role} must be an object")
    return value


def _map(value: Any, role: str) -> dict[str, Any]:
    # Go ranges a nil map as zero records. A non-omitempty map field is encoded
    # as JSON null, so the independent legacy projection must do the same.
    if value is None:
        return {}
    return _object(value, role)


def _array(value: Any, role: str) -> list[Any]:
    # Go's JSON encoder represents a nil slice as null when the field is not
    # omitempty; ranging that same slice in compact_v1 emits zero records.
    if value is None:
        return []
    if not isinstance(value, list):
        raise LegacyPacketError(f"{role} must be an array")
    return value


def _string(value: Any, role: str) -> str:
    if not isinstance(value, str):
        raise LegacyPacketError(f"{role} must be a string")
    return value


def _boolean(value: Any, role: str) -> bool:
    if not isinstance(value, bool):
        raise LegacyPacketError(f"{role} must be boolean")
    return value


def _integer(value: Any, role: str) -> int:
    if isinstance(value, bool) or not isinstance(value, int):
        raise LegacyPacketError(f"{role} must be an integer")
    return value


def _number(value: Any, role: str) -> int | float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise LegacyPacketError(f"{role} must be a finite number")
    if isinstance(value, float) and not math.isfinite(value):
        raise LegacyPacketError(f"{role} must be a finite number")
    return value


def _strings(value: Any, role: str) -> list[str]:
    values = _array(value, role)
    if any(not isinstance(item, str) for item in values):
        raise LegacyPacketError(f"{role} must contain only strings")
    return list(values)


def _opt_string(value: Any, role: str) -> Any:
    string = _string(value, role)
    return OMIT if string == "" else string


def _opt_strings(value: Any, role: str) -> Any:
    values = _strings(value, role)
    return OMIT if not values else values


def _opt_int(value: Any, role: str) -> Any:
    integer = _integer(value, role)
    return OMIT if integer == 0 else integer


def _opt_number(value: Any, role: str) -> Any:
    number = _number(value, role)
    return OMIT if number == 0 else number


def _get(obj: Mapping[str, Any], key: str, default: Any) -> Any:
    return obj[key] if key in obj else copy.deepcopy(default)


def _record(tag: str, fields: Sequence[tuple[str, Any]]) -> dict[str, Any]:
    return {"tag": tag, "fields": [[key, value] for key, value in fields if value is not OMIT]}


def _semantic_symbol_record(tag: str, value: Any, extra: Sequence[tuple[str, Any]] = ()) -> dict[str, Any]:
    obj = _object(value, tag)
    name = _string(_get(obj, "qualified_name", ""), tag + ".qualified_name")
    short_name = _string(_get(obj, "name", ""), tag + ".name")
    if not name:
        name = short_name
    fields: list[tuple[str, Any]] = [
        ("id", _opt_string(_get(obj, "id", ""), tag + ".id")),
        ("kind", _opt_string(_get(obj, "kind", ""), tag + ".kind")),
        ("name", _opt_string(name, tag + ".display_name")),
    ]
    if short_name and short_name != name:
        fields.append(("short_name", short_name))
    fields.extend(
        (
            ("file", _opt_string(_get(obj, "file_path", ""), tag + ".file_path")),
            ("start", _opt_int(_get(obj, "start_line", 0), tag + ".start_line")),
            ("end", _opt_int(_get(obj, "end_line", 0), tag + ".end_line")),
            ("path", _opt_string(_get(obj, "path", ""), tag + ".path")),
            ("signature", _opt_string(_get(obj, "signature", ""), tag + ".signature")),
            ("language", _opt_string(_get(obj, "language", ""), tag + ".language")),
            ("score", _opt_int(_get(obj, "score", 0), tag + ".score")),
            ("confidence", _opt_number(_get(obj, "confidence", 0), tag + ".confidence")),
            ("reason", _opt_string(_get(obj, "reason", ""), tag + ".reason")),
            ("warning_codes", _opt_strings(_get(obj, "warning_codes", []), tag + ".warning_codes")),
        )
    )
    fields.extend(extra)
    return _record(tag, fields)


def _semantic_relation_records(tag: str, index: int, value: Any) -> list[dict[str, Any]]:
    obj = _object(value, tag)
    records = [
        _record(
            tag,
            (
                ("index", index),
                ("id", _opt_string(_get(obj, "id", ""), tag + ".id")),
                ("type", _opt_string(_get(obj, "type", ""), tag + ".type")),
                ("from", _opt_string(_get(obj, "from_id", ""), tag + ".from_id")),
                ("to", _opt_string(_get(obj, "to_id", ""), tag + ".to_id")),
                ("file", _opt_string(_get(obj, "file_path", ""), tag + ".file_path")),
                ("start", _opt_int(_get(obj, "start_line", 0), tag + ".start_line")),
                ("end", _opt_int(_get(obj, "end_line", 0), tag + ".end_line")),
                ("path", _opt_string(_get(obj, "path", ""), tag + ".path")),
                ("scope", _opt_string(_get(obj, "relation_scope", ""), tag + ".relation_scope")),
                ("resolution", _opt_string(_get(obj, "resolution", ""), tag + ".resolution")),
                ("target_kind", _opt_string(_get(obj, "target_kind", ""), tag + ".target_kind")),
                ("confidence", _opt_number(_get(obj, "confidence", 0), tag + ".confidence")),
                ("reason", _opt_string(_get(obj, "reason", ""), tag + ".reason")),
                ("warning_codes", _opt_strings(_get(obj, "warning_codes", []), tag + ".warning_codes")),
            ),
        )
    ]
    for evidence_index, evidence_value in enumerate(_array(_get(obj, "evidence", []), tag + ".evidence")):
        evidence = _object(evidence_value, tag + ".evidence[]")
        records.append(
            _record(
                tag + "_evidence",
                (
                    ("owner_index", index),
                    ("index", evidence_index),
                    ("kind", _opt_string(_get(evidence, "kind", ""), "evidence.kind")),
                    ("file", _opt_string(_get(evidence, "file_path", ""), "evidence.file_path")),
                    ("start", _opt_int(_get(evidence, "start_line", 0), "evidence.start_line")),
                    ("end", _opt_int(_get(evidence, "end_line", 0), "evidence.end_line")),
                    ("detail", _opt_string(_get(evidence, "detail", ""), "evidence.detail")),
                ),
            )
        )
    return records


def _semantic_warning_record(tag: str, value: Any) -> dict[str, Any]:
    obj = _object(value, tag)
    return _record(
        tag,
        (
            ("code", _opt_string(_get(obj, "code", ""), tag + ".code")),
            ("severity", _opt_string(_get(obj, "severity", ""), tag + ".severity")),
            ("path", _opt_string(_get(obj, "path", ""), tag + ".path")),
            ("effect", _opt_string(_get(obj, "effect", ""), tag + ".effect")),
            ("detail", _opt_string(_get(obj, "detail", ""), tag + ".detail")),
        ),
    )


def canonical_projection_from_legacy(value: Any) -> list[dict[str, Any]]:
    """Independent allowlisted projection matching compact_v1's retained semantics."""

    report = _object(value, "legacy packet")
    status = _object(report.get("status"), "status")
    repo = _object(status.get("repo"), "status.repo")
    brain = _object(status.get("brain"), "status.brain")
    sources = _object(status.get("sources"), "status.sources")
    live = _object(status.get("live"), "status.live")
    semantic_status_value = status.get("semantic")
    semantic_status = None if semantic_status_value is None else _object(semantic_status_value, "status.semantic")
    freshness = None
    if semantic_status is not None and semantic_status.get("freshness") is not None:
        freshness = _object(semantic_status["freshness"], "status.semantic.freshness")

    records: list[dict[str, Any]] = []
    records.append(_record("task", (("value", _string(report.get("task"), "task")),)))
    severity: Any = OMIT
    if freshness is not None:
        severity = _opt_string(_get(freshness, "severity", ""), "freshness.severity")
    changed_files = _strings(_get(live, "changed_files", []), "live.changed_files")
    records.append(
        _record(
            "status",
            (
                ("freshness", severity),
                ("repo_key", _opt_string(_get(repo, "key", ""), "status.repo.key")),
                ("brain_schema", _opt_int(_get(brain, "schema_version", 0), "status.brain.schema_version")),
                ("branch", _opt_string(_get(live, "branch", ""), "status.live.branch")),
                ("head", _opt_string(_get(live, "head", ""), "status.live.head")),
                ("dirty", _boolean(_get(live, "dirty", False), "status.live.dirty")),
                ("changed_files", len(changed_files)),
            ),
        )
    )
    records.append(
        _record(
            "sources",
            tuple(
                (name, _boolean(_get(sources, name, False), "status.sources." + name))
                for name in ("seed", "sessions", "semantic", "history", "facts")
            ),
        )
    )
    diff_stat = _string(_get(live, "diff_stat", ""), "status.live.diff_stat").strip()
    records.append(
        _record(
            "live",
            (
                ("staged", _opt_strings(_get(live, "staged", []), "status.live.staged")),
                ("unstaged", _opt_strings(_get(live, "unstaged", []), "status.live.unstaged")),
                ("untracked", _opt_strings(_get(live, "untracked", []), "status.live.untracked")),
                ("changed", OMIT if not changed_files else changed_files),
                ("diff_stat", OMIT if not diff_stat else diff_stat),
            ),
        )
    )
    for symbol in _array(_get(live, "changed_symbol_hints", []), "status.live.changed_symbol_hints"):
        records.append(_semantic_symbol_record("live_symbol", symbol))

    if status.get("facts") is not None:
        facts_status = _object(status["facts"], "status.facts")
        fields: list[tuple[str, Any]] = [
            (name, _integer(_get(facts_status, name, 0), "status.facts." + name))
            for name in ("facts", "distilled", "authored", "superseded", "branches", "proposals")
        ]
        if facts_status.get("verification") is not None:
            verification = _object(facts_status["verification"], "status.facts.verification")
            fields.extend(
                (
                    ("verified_facts", _integer(_get(verification, "facts", 0), "verification.facts")),
                    ("verified", _integer(_get(verification, "verified", 0), "verification.verified")),
                    ("stale", _integer(_get(verification, "stale", 0), "verification.stale")),
                    ("orphaned", _integer(_get(verification, "orphaned", 0), "verification.orphaned")),
                    ("unverifiable_here", _integer(_get(verification, "unverifiable_here", 0), "verification.unverifiable_here")),
                    ("sampled_of", _opt_int(_get(verification, "sampled_of", 0), "verification.sampled_of")),
                )
            )
        records.append(_record("facts_status", fields))

    if semantic_status is not None:
        fields = []
        if semantic_status.get("provider") is not None:
            provider = _object(semantic_status["provider"], "status.semantic.provider")
            fields.extend(
                (
                    ("provider", _opt_string(_get(provider, "name", ""), "provider.name")),
                    ("provider_version", _opt_string(_get(provider, "version", ""), "provider.version")),
                    ("schema", _opt_string(_get(provider, "schema_version", ""), "provider.schema_version")),
                    ("capabilities", _opt_strings(_get(provider, "capabilities", []), "provider.capabilities")),
                )
            )
        coverage = None
        if semantic_status.get("coverage") is not None:
            coverage = _object(semantic_status["coverage"], "status.semantic.coverage")
            fields.extend(
                (name, _integer(_get(coverage, name, 0), "coverage." + name))
                for name in ("files", "symbols", "relations", "warnings", "partial_failures")
            )
        records.append(_record("semantic_status", fields))
        if freshness is not None:
            axes = _map(_get(freshness, "axes", {}), "freshness.axes")
            for name in sorted(axes):
                axis = _object(axes[name], "freshness.axes[]")
                records.append(
                    _record(
                        "freshness_axis",
                        (
                            ("name", name),
                            ("state", _string(_get(axis, "state", ""), "axis.state")),
                            ("detail", _opt_string(_get(axis, "detail", ""), "axis.detail")),
                        ),
                    )
                )
            for warning in _array(_get(freshness, "warnings", []), "freshness.warnings"):
                records.append(_semantic_warning_record("freshness_warning", warning))
        if coverage is not None:
            for warning in _array(_get(coverage, "warning_details", []), "coverage.warning_details"):
                records.append(_semantic_warning_record("semantic_warning", warning))
            for warning in _array(_get(coverage, "partial_failure_details", []), "coverage.partial_failure_details"):
                records.append(_semantic_warning_record("semantic_partial_failure", warning))
        for spot_value in _array(_get(semantic_status, "blind_spots", []), "semantic.blind_spots"):
            spot = _object(spot_value, "blind_spot")
            records.append(
                _record(
                    "blind_spot",
                    (
                        ("path", _string(_get(spot, "path", ""), "blind_spot.path")),
                        ("code", _opt_string(_get(spot, "code", ""), "blind_spot.code")),
                        ("detail", _opt_string(_get(spot, "detail", ""), "blind_spot.detail")),
                    ),
                )
            )

    edit_files = _strings(_get(report, "likely_edit_files", []), "likely_edit_files")
    test_files = _strings(_get(report, "likely_test_files", []), "likely_test_files")
    likely_files = _strings(_get(report, "likely_files", []), "likely_files")
    seen_likely = set()
    for path in edit_files:
        records.append(_record("edit_file", (("path", path),)))
        seen_likely.add(path)
    for path in test_files:
        records.append(_record("test_file", (("path", path),)))
        seen_likely.add(path)
    for path in likely_files:
        if path not in seen_likely:
            records.append(_record("likely_file", (("path", path),)))

    semantic = _object(report.get("semantic"), "semantic")
    context = _object(semantic.get("context"), "semantic.context")
    for symbol in _array(_get(context, "symbols", []), "semantic.context.symbols"):
        records.append(_semantic_symbol_record("symbol", symbol))
    for index, relation in enumerate(_array(_get(context, "relations", []), "semantic.context.relations")):
        records.extend(_semantic_relation_records("relation", index, relation))
    for neighbor in _array(_get(context, "neighbors", []), "semantic.context.neighbors"):
        records.append(_semantic_symbol_record("neighbor", neighbor))
    for index, trace in enumerate(_array(_get(semantic, "runtime_traces", []), "semantic.runtime_traces")):
        records.extend(_semantic_relation_records("runtime_trace", index, trace))
    tests = _object(semantic.get("tests"), "semantic.tests")
    for root in _array(_get(tests, "roots", []), "semantic.tests.roots"):
        records.append(_semantic_symbol_record("test_root", root))
    for suggestion_value in _array(_get(tests, "suggestions", []), "semantic.tests.suggestions"):
        suggestion = _object(suggestion_value, "semantic test suggestion")
        records.append(
            _semantic_symbol_record(
                "test_suggestion",
                suggestion.get("symbol"),
                (("suggestion_reason", _string(_get(suggestion, "reason", ""), "suggestion.reason")),),
            )
        )

    history = _object(report.get("history"), "history")
    for match_value in _array(_get(history, "matches", []), "history.matches"):
        match = _object(match_value, "history match")
        records.append(
            _record(
                "history",
                (
                    ("path", _string(_get(match, "path", ""), "history.path")),
                    ("line", _integer(_get(match, "line", 0), "history.line")),
                    ("timestamp", _opt_string(_get(match, "timestamp", ""), "history.timestamp")),
                    ("score", _opt_int(_get(match, "score", 0), "history.score")),
                    ("matched_terms", _opt_strings(_get(match, "matched_terms", []), "history.matched_terms")),
                    ("excerpt", _string(_get(match, "excerpt", ""), "history.excerpt")),
                ),
            )
        )

    drift = _object(_get(report, "facts_locus_drift", {}), "facts_locus_drift")
    seen_facts: set[str] = set()
    for fact_value in _array(_get(report, "facts", []), "facts"):
        fact = _object(fact_value, "fact")
        fact_id = _string(_get(fact, "id", ""), "fact.id")
        provenance = _array(_get(fact, "provenance", []), "fact.provenance")
        verified = 0
        for anchor_value in provenance:
            anchor = _object(anchor_value, "fact.provenance[]")
            if _boolean(_get(anchor, "verified", False), "fact.provenance.verified"):
                verified += 1
        stale_locus = _strings(_get(drift, fact_id, []), "facts_locus_drift[]")
        records.append(
            _record(
                "fact",
                (
                    ("id", fact_id),
                    ("paths", _opt_strings(_get(fact, "paths", []), "fact.paths")),
                    ("kind", _opt_string(_get(fact, "kind", ""), "fact.kind")),
                    ("locus", _opt_strings(_get(fact, "locus", []), "fact.locus")),
                    ("text", _string(_get(fact, "text", ""), "fact.text")),
                    ("branch", _opt_string(_get(fact, "branch", ""), "fact.branch")),
                    ("origin", _opt_string(_get(fact, "origin", ""), "fact.origin")),
                    ("status", _opt_string(_get(fact, "status", ""), "fact.status")),
                    ("confidence", _opt_string(_get(fact, "confidence", ""), "fact.confidence")),
                    ("related_ids", _opt_strings(_get(fact, "related_ids", []), "fact.related_ids")),
                    ("superseded_by", _opt_string(_get(fact, "superseded_by", ""), "fact.superseded_by")),
                    ("anchors", len(provenance)),
                    ("verified_anchors", verified),
                    ("stale_locus", OMIT if not stale_locus else stale_locus),
                ),
            )
        )
        seen_facts.add(fact_id)
    for fact_id in sorted(set(drift) - seen_facts):
        records.append(
            _record(
                "fact_drift",
                (
                    ("id", fact_id),
                    ("stale_locus", _opt_strings(drift[fact_id], "facts_locus_drift[]")),
                ),
            )
        )

    for action_value in _array(_get(report, "action_checklist", []), "action_checklist"):
        action = _object(action_value, "action")
        records.append(
            _record(
                "action",
                (
                    ("file", _opt_string(_get(action, "file", ""), "action.file")),
                    ("symbol", _opt_string(_get(action, "symbol", ""), "action.symbol")),
                    ("action", _string(_get(action, "action", ""), "action.action")),
                    ("evidence", _opt_string(_get(action, "evidence", ""), "action.evidence")),
                ),
            )
        )

    for pattern_value in _array(_get(report, "patterns", []), "patterns"):
        pattern = _object(pattern_value, "pattern")
        fields: list[tuple[str, Any]] = [
            ("id", _string(_get(pattern, "id", ""), "pattern.id")),
            ("type", _string(_get(pattern, "type", ""), "pattern.type")),
            ("scope", _opt_string(_get(pattern, "scope", ""), "pattern.scope")),
            ("kind", _opt_string(_get(pattern, "kind", ""), "pattern.kind")),
            ("title", _string(_get(pattern, "title", ""), "pattern.title")),
            ("strength", _number(_get(pattern, "strength", 0), "pattern.strength")),
            ("strength_label", _opt_string(_get(pattern, "strength_label", ""), "pattern.strength_label")),
            ("support", _integer(_get(pattern, "support", 0), "pattern.support")),
            ("repos", _opt_int(_get(pattern, "repos", 0), "pattern.repos")),
            ("skill_status", _opt_string(_get(pattern, "skill_status", ""), "pattern.skill_status")),
            ("note", _opt_string(_get(pattern, "note", ""), "pattern.note")),
            ("intent_sig", _opt_string(_get(pattern, "intent_sig", ""), "pattern.intent_sig")),
            ("gram", _opt_string(_get(pattern, "gram", ""), "pattern.gram")),
            ("dossier_status", _opt_string(_get(pattern, "dossier_status", ""), "pattern.dossier_status")),
            ("verdict", _opt_string(_get(pattern, "verdict", ""), "pattern.verdict")),
            ("workspace", _opt_string(_get(pattern, "workspace", ""), "pattern.workspace")),
        ]
        if pattern.get("reinforcement") is not None:
            reinforcement = _object(pattern["reinforcement"], "pattern.reinforcement")
            fields.extend(
                (name, _integer(_get(reinforcement, name, 0), "reinforcement." + name))
                for name in ("success", "corrected", "neutral")
            )
        if pattern.get("example") is not None:
            example = _object(pattern["example"], "pattern.example")
            fields.extend(
                (
                    ("example_path", _opt_string(_get(example, "path", ""), "example.path")),
                    ("example_line", _opt_int(_get(example, "line", 0), "example.line")),
                )
            )
        records.append(_record("pattern", fields))

    for consolidation_value in _array(_get(report, "consolidations", []), "consolidations"):
        consolidation = _object(consolidation_value, "consolidation")
        fields = [
            ("pattern_id", _string(_get(consolidation, "pattern_id", ""), "consolidation.pattern_id")),
            ("type", _string(_get(consolidation, "type", ""), "consolidation.type")),
            ("title", _string(_get(consolidation, "title", ""), "consolidation.title")),
            ("trigger", _string(_get(consolidation, "trigger", ""), "consolidation.trigger")),
            ("confidence", _number(_get(consolidation, "confidence", 0), "consolidation.confidence")),
            ("status", _string(_get(consolidation, "status", ""), "consolidation.status")),
            ("verdict", _opt_string(_get(consolidation, "verdict", ""), "consolidation.verdict")),
            ("workflow", _opt_strings(_get(consolidation, "workflow", []), "consolidation.workflow")),
            ("verification", _opt_strings(_get(consolidation, "verification", []), "consolidation.verification")),
            ("failure_modes", _opt_strings(_get(consolidation, "failure_modes", []), "consolidation.failure_modes")),
        ]
        if consolidation.get("anchor") is not None:
            anchor = _object(consolidation["anchor"], "consolidation.anchor")
            fields.extend(
                (
                    ("anchor_transcript", _opt_string(_get(anchor, "transcript", ""), "anchor.transcript")),
                    ("anchor_start", _opt_int(_get(anchor, "start_line", 0), "anchor.start_line")),
                    ("anchor_end", _opt_int(_get(anchor, "end_line", 0), "anchor.end_line")),
                    ("anchor_outcome", _opt_string(_get(anchor, "outcome", ""), "anchor.outcome")),
                )
            )
        records.append(_record("consolidation", fields))

    for theme_value in _array(_get(report, "themes", []), "themes"):
        theme = _object(theme_value, "theme")
        records.append(
            _record(
                "theme",
                (
                    ("id", _string(_get(theme, "id", ""), "theme.id")),
                    ("title", _string(_get(theme, "title", ""), "theme.title")),
                    ("description", _opt_string(_get(theme, "description", ""), "theme.description")),
                    ("shape", _string(_get(theme, "shape", ""), "theme.shape")),
                    ("support", _integer(_get(theme, "support", 0), "theme.support")),
                    ("strength", _number(_get(theme, "strength", 0), "theme.strength")),
                    ("status", _string(_get(theme, "status", ""), "theme.status")),
                    ("verdict", _opt_string(_get(theme, "verdict", ""), "theme.verdict")),
                ),
            )
        )
    for guidance in _strings(_get(report, "guidance", []), "guidance"):
        records.append(_record("guidance", (("text", guidance),)))
    for source, warnings in (
        ("status", _strings(_get(status, "warnings", []), "status.warnings")),
        ("live", _strings(_get(live, "warnings", []), "live.warnings")),
        ("brief", _strings(_get(report, "warnings", []), "warnings")),
    ):
        for warning in warnings:
            records.append(_record("warning", (("source", source), ("text", warning))))
    return records


def _failure(kind: str, stdout: bytes, stderr: bytes, return_code: int | None) -> dict[str, Any]:
    return {
        "kind": kind,
        "return_code": return_code,
        "stdout_byte_count": len(stdout),
        "stdout_sha256": sha256_bytes(stdout),
        "stderr_byte_count": len(stderr),
        "stderr_sha256": sha256_bytes(stderr),
    }


def _error_kind(exc: Exception) -> str:
    if isinstance(exc, MCPShapeError):
        return "invalid_mcp_response"
    if isinstance(exc, CompactIntegrityError):
        return "compact_integrity_failure"
    if isinstance(exc, CompactPacketError):
        return "invalid_compact_packet"
    if isinstance(exc, LegacyPacketError):
        return "invalid_legacy_packet"
    return "invalid_packet_projection"


def run_pair(
    brain_bin: pathlib.Path,
    repo: pathlib.Path,
    prompt: str,
    timeout_seconds: float,
) -> dict[str, Any]:
    messages = []
    for identifier, packet_format in enumerate(FORMAT_ORDER, start=1):
        messages.append(
            _frame_request(
                {
                    "jsonrpc": "2.0",
                    "id": identifier,
                    "method": "tools/call",
                    "params": {
                        "name": "brain_brief",
                        "arguments": {
                            "task": prompt,
                            "packet_format": packet_format,
                        },
                    },
                }
            )
        )
    request = b"".join(messages)
    try:
        result = subprocess.run(
            (str(brain_bin), "mcp"),
            cwd=repo,
            input=request,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
            timeout=timeout_seconds,
        )
    except subprocess.TimeoutExpired as exc:
        return {
            "status": "failed",
            "failure": _failure(
                "timeout",
                exc.stdout if isinstance(exc.stdout, bytes) else b"",
                exc.stderr if isinstance(exc.stderr, bytes) else b"",
                None,
            ),
        }
    except OSError:
        return {"status": "failed", "failure": _failure("launch_error", b"", b"", None)}
    if result.returncode != 0:
        return {
            "status": "failed",
            "failure": _failure("nonzero_exit", result.stdout, result.stderr, result.returncode),
        }
    try:
        frames = parse_mcp_response_frames(result.stdout)
        texts = [extract_mcp_text(frame[0], index) for index, frame in enumerate(frames, start=1)]
        inner = [text.encode("utf-8") for text in texts]
        legacy_value = _strict_json(inner[0], "legacy packet")
        legacy_projection = canonical_projection_from_legacy(legacy_value)
        compact = parse_compact_v2_packet(inner[1])
        compact_projection = compact["records"]
    except (PacketABError, KeyError, TypeError, ValueError) as exc:
        return {
            "status": "failed",
            "failure": _failure(_error_kind(exc), result.stdout, result.stderr, result.returncode),
        }

    format_metrics: dict[str, Any] = {}
    for index, packet_format in enumerate(FORMAT_ORDER):
        format_metrics[packet_format] = {
            "inner_byte_count": len(inner[index]),
            "inner_sha256": sha256_bytes(inner[index]),
            "framed_byte_count": len(frames[index][1]),
            "framed_sha256": sha256_bytes(frames[index][1]),
            "offline_token_proxy_count": offline_token_proxy_count(inner[index]),
            "parse_ok": True,
        }
    format_metrics["compact_v2"]["integrity_ok"] = True
    format_metrics["compact_v2"]["body_record_count"] = compact["body_record_count"]
    format_metrics["compact_v2"]["body_sha256"] = compact["body_sha256"]
    format_metrics["compact_v2"]["structural_size_attribution"] = compact[
        "structural_size_attribution"
    ]

    legacy_projection_bytes = canonical_json_bytes(legacy_projection)
    compact_projection_bytes = canonical_json_bytes(compact_projection)
    parity = legacy_projection_bytes == compact_projection_bytes
    legacy_bytes = format_metrics["legacy_json"]["inner_byte_count"]
    compact_bytes = format_metrics["compact_v2"]["inner_byte_count"]
    legacy_framed = format_metrics["legacy_json"]["framed_byte_count"]
    compact_framed = format_metrics["compact_v2"]["framed_byte_count"]
    legacy_tokens = format_metrics["legacy_json"]["offline_token_proxy_count"]
    compact_tokens = format_metrics["compact_v2"]["offline_token_proxy_count"]
    return {
        "status": "ok",
        "formats": format_metrics,
        "canonical_projection": {
            "legacy_sha256": sha256_bytes(legacy_projection_bytes),
            "compact_sha256": sha256_bytes(compact_projection_bytes),
            "parity": parity,
        },
        "reduction": {
            "inner_bytes_saved": legacy_bytes - compact_bytes,
            "inner_byte_reduction_ppm": reduction_ppm(legacy_bytes, compact_bytes),
            "framed_bytes_saved": legacy_framed - compact_framed,
            "framed_byte_reduction_ppm": reduction_ppm(legacy_framed, compact_framed),
            "token_proxy_saved": legacy_tokens - compact_tokens,
            "token_proxy_reduction_ppm": reduction_ppm(legacy_tokens, compact_tokens),
        },
    }


def build_schedule(tasks: Sequence[Mapping[str, str]]) -> dict[str, Any]:
    entries = [
        {
            "sequence": sequence,
            "logical_repo": task["logical_repo"],
            "task_sha256": task["task_sha256"],
            "format_order": list(FORMAT_ORDER),
        }
        for sequence, task in enumerate(tasks)
    ]
    return {
        "policy": "verified_brief_profile_corpus_order_with_adjacent_legacy_json_then_compact_v2",
        "task_count": len(entries),
        "format_order": list(FORMAT_ORDER),
        "entries_sha256": sha256_bytes(canonical_json_bytes(entries)),
        "entries": entries,
    }


def _lower_median(values: Sequence[int]) -> int | None:
    return None if not values else int(statistics.median_low(sorted(values)))


def _aggregate_size_attributions(values: Sequence[Mapping[str, Any]]) -> dict[str, Any]:
    partition_fields = (
        "marker_byte_count",
        "legend_byte_count",
        "newline_byte_count",
        "footer_byte_count",
        "declaration_byte_count",
        "keyed_record_byte_count",
        "positional_record_byte_count",
    )
    count_fields = (
        "body_record_count",
        "declaration_count",
        "keyed_record_count",
        "positional_record_count",
        "explicit_cell_count",
        "explicit_null_cell_count",
        "repeat_reference_cell_count",
        "implicit_trailing_omission_count",
    )
    per_tag_fields = (
        "record_count",
        "keyed_record_count",
        "positional_record_count",
        "record_encoded_byte_count",
        "declaration_count",
        "declaration_encoded_byte_count",
        "explicit_cell_count",
        "explicit_null_cell_count",
        "repeat_reference_cell_count",
        "implicit_trailing_omission_count",
    )
    for value in values:
        if (
            value.get("schema_version") != SIZE_ATTRIBUTION_SCHEMA_VERSION
            or set(value.get("per_tag", {})) != COMPACT_BODY_TAGS
        ):
            raise PacketABError("compact_v2 numeric attribution schema mismatch")
    partition = {
        field: sum(value["structural_partition"][field] for value in values)
        for field in partition_fields
    }
    counts = {
        field: sum(value["counts"].get(field, 0) for value in values)
        for field in count_fields
    }
    per_tag = {
        tag: {
            field: sum(value["per_tag"][tag][field] for value in values)
            for field in per_tag_fields
        }
        for tag in COMPACT_BODY_TAG_ORDER
    }
    exact_inner_sum = sum(
        value["accounting_invariants"]["exact_inner_byte_count"] for value in values
    )
    partition_sum = sum(partition.values())
    record_bytes = partition["keyed_record_byte_count"] + partition["positional_record_byte_count"]
    per_tag_record_bytes = sum(row["record_encoded_byte_count"] for row in per_tag.values())
    per_tag_declaration_bytes = sum(
        row["declaration_encoded_byte_count"] for row in per_tag.values()
    )
    per_tag_records = sum(row["record_count"] for row in per_tag.values())
    invariants = {
        "exact_inner_byte_count": exact_inner_sum,
        "partition_byte_count_sum": partition_sum,
        "partition_delta_byte_count": partition_sum - exact_inner_sum,
        "record_encoded_byte_count_sum": record_bytes,
        "per_tag_record_encoded_byte_count_sum": per_tag_record_bytes,
        "per_tag_record_encoded_delta_byte_count": per_tag_record_bytes - record_bytes,
        "per_tag_declaration_encoded_byte_count_sum": per_tag_declaration_bytes,
        "per_tag_declaration_encoded_delta_byte_count": (
            per_tag_declaration_bytes - partition["declaration_byte_count"]
        ),
        "per_tag_record_count_sum": per_tag_records,
        "per_tag_record_count_delta": per_tag_records - counts["body_record_count"],
    }
    if any(value != 0 for key, value in invariants.items() if "delta" in key):
        raise PacketABError("aggregate compact_v2 numeric attribution mismatch")
    return {
        "schema_version": SIZE_ATTRIBUTION_SCHEMA_VERSION,
        "packet_count": len(values),
        "structural_partition": partition,
        "summed_packet_counts": counts,
        "per_tag": per_tag,
        "accounting_invariants": invariants,
    }

def _aggregate(observations: Sequence[Mapping[str, Any]]) -> dict[str, Any]:
    successes = [row for row in observations if row["status"] == "ok"]
    failures = Counter(row["failure"]["kind"] for row in observations if row["status"] == "failed")
    result: dict[str, Any] = {
        "scheduled_pair_count": len(observations),
        "successful_pair_count": len(successes),
        "failure_count_by_kind": dict(sorted(failures.items())),
        "canonical_parity_count": sum(row["canonical_projection"]["parity"] for row in successes),
        "compact_integrity_pass_count": sum(row["formats"]["compact_v2"]["integrity_ok"] for row in successes),
    }
    for format_name in FORMAT_ORDER:
        result[format_name] = {
            "inner_byte_count_sum": sum(row["formats"][format_name]["inner_byte_count"] for row in successes),
            "framed_byte_count_sum": sum(row["formats"][format_name]["framed_byte_count"] for row in successes),
            "offline_token_proxy_count_sum": sum(
                row["formats"][format_name]["offline_token_proxy_count"] for row in successes
            ),
            "parse_pass_count": sum(row["formats"][format_name]["parse_ok"] for row in successes),
        }
    size_aggregate = _aggregate_size_attributions(
        [row["formats"]["compact_v2"]["structural_size_attribution"] for row in successes]
    )
    if (
        size_aggregate["accounting_invariants"]["exact_inner_byte_count"]
        != result["compact_v2"]["inner_byte_count_sum"]
    ):
        raise PacketABError("aggregate attribution does not match compact inner-byte total")
    result["compact_v2"]["structural_size_attribution"] = size_aggregate
    for key in (
        "inner_byte_reduction_ppm",
        "framed_byte_reduction_ppm",
        "token_proxy_reduction_ppm",
    ):
        values = [row["reduction"][key] for row in successes]
        result[key] = {
            "minimum": min(values) if values else None,
            "lower_median": _lower_median(values),
            "maximum": max(values) if values else None,
        }
    if successes:
        result["pooled_inner_byte_reduction_ppm"] = reduction_ppm(
            result["legacy_json"]["inner_byte_count_sum"],
            result["compact_v2"]["inner_byte_count_sum"],
        )
        result["pooled_framed_byte_reduction_ppm"] = reduction_ppm(
            result["legacy_json"]["framed_byte_count_sum"],
            result["compact_v2"]["framed_byte_count_sum"],
        )
        result["pooled_token_proxy_reduction_ppm"] = reduction_ppm(
            result["legacy_json"]["offline_token_proxy_count_sum"],
            result["compact_v2"]["offline_token_proxy_count_sum"],
        )
    else:
        result["pooled_inner_byte_reduction_ppm"] = None
        result["pooled_framed_byte_reduction_ppm"] = None
        result["pooled_token_proxy_reduction_ppm"] = None
    return result


def _promotion_gate(complete_corpus: bool, aggregate: Mapping[str, Any]) -> dict[str, Any]:
    scheduled = aggregate["scheduled_pair_count"]
    successful = aggregate["successful_pair_count"]
    median_bytes = aggregate["inner_byte_reduction_ppm"]["lower_median"]
    median_tokens = aggregate["token_proxy_reduction_ppm"]["lower_median"]
    minimum_bytes = aggregate["inner_byte_reduction_ppm"]["minimum"]
    checks = {
        "complete_114_query_corpus": complete_corpus and scheduled == 114,
        "all_pairs_successful": scheduled > 0 and successful == scheduled,
        "all_legacy_packets_parse": successful > 0 and aggregate["legacy_json"]["parse_pass_count"] == successful,
        "all_compact_packets_parse": successful > 0 and aggregate["compact_v2"]["parse_pass_count"] == successful,
        "all_compact_integrity_checks_pass": successful > 0 and aggregate["compact_integrity_pass_count"] == successful,
        "all_canonical_projections_match": successful > 0 and aggregate["canonical_parity_count"] == successful,
        "median_inner_byte_reduction_at_least_60_percent": median_bytes is not None and median_bytes >= INNER_BYTE_REDUCTION_GATE_PPM,
        "median_token_proxy_reduction_at_least_50_percent": median_tokens is not None and median_tokens >= TOKEN_PROXY_REDUCTION_GATE_PPM,
        "no_task_has_compact_inner_byte_growth": minimum_bytes is not None and minimum_bytes >= 0,
    }
    return {
        "thresholds": {
            "full_task_count": 114,
            "median_inner_byte_reduction_ppm_minimum": INNER_BYTE_REDUCTION_GATE_PPM,
            "median_token_proxy_reduction_ppm_minimum": TOKEN_PROXY_REDUCTION_GATE_PPM,
            "minimum_per_task_inner_byte_reduction_ppm": 0,
            "canonical_projection_parity_required": True,
            "compact_integrity_required": True,
        },
        "checks": checks,
        "eligible_for_separately_authorized_agent_quality_trial": all(checks.values()),
        "paid_agent_quality_trial_authorized": False,
        "change_mcp_or_cli_default_authorized": False,
    }


def build_report(
    corpus: Mapping[str, Any],
    tasks: Sequence[Mapping[str, str]],
    brain_bin: pathlib.Path,
    repo_mappings: Mapping[str, pathlib.Path],
    timeout_seconds: float,
) -> dict[str, Any]:
    if set(repo_mappings) != set(profile_brief.LOGICAL_REPOS):
        raise PacketABError("--repo mappings must cover all three logical repos exactly")
    if len(tasks) > len(corpus["tasks"]):
        raise PacketABError("selected tasks exceed the verified corpus")
    for selected, expected in zip(tasks, corpus["tasks"]):
        for field in ("logical_repo", "task_sha256", "prompt_sha256"):
            if selected.get(field) != expected.get(field):
                raise PacketABError("selected tasks are not the verified corpus prefix")
    runner_identity = build_runner_identity()
    metadata = profile_brief.collect_run_metadata(
        brain_bin,
        repo_mappings,
        profile_brief.LOGICAL_REPOS,
        timeout_seconds,
    )
    schedule = build_schedule(tasks)
    task_by_sha = {task["task_sha256"]: task for task in tasks}
    observations: list[dict[str, Any]] = []
    failures: list[dict[str, Any]] = []
    for scheduled in schedule["entries"]:
        task = task_by_sha[scheduled["task_sha256"]]
        result = run_pair(
            brain_bin,
            repo_mappings[task["logical_repo"]],
            task["prompt"],
            timeout_seconds,
        )
        row = {
            "sequence": scheduled["sequence"],
            "logical_repo": task["logical_repo"],
            "task_sha256": task["task_sha256"],
            "prompt_sha256": task["prompt_sha256"],
            **result,
        }
        observations.append(row)
        if row["status"] == "failed":
            failures.append(
                {
                    "sequence": row["sequence"],
                    "logical_repo": row["logical_repo"],
                    "task_sha256": row["task_sha256"],
                    **row["failure"],
                }
            )
    aggregate = _aggregate(observations)
    complete_corpus = len(tasks) == len(corpus["tasks"])
    body = {
        "schema_version": REPORT_SCHEMA_VERSION,
        "evidence_role": EVIDENCE_ROLE,
        "confirmatory_eligible": False,
        "quality_eligible": False,
        "paid_or_provider_calls": False,
        "runner_identity": runner_identity,
        "corpus": {
            "schema_version": corpus["schema_version"],
            "corpus_sha256": corpus["corpus_sha256"],
            "full_task_count": len(corpus["tasks"]),
            "selected_task_count": len(tasks),
            "complete_corpus": complete_corpus,
        },
        "runtime_identity": metadata,
        "offline_token_proxy": {
            "name": TOKEN_PROXY_NAME,
            "definition": TOKEN_PROXY_DEFINITION,
            "definition_sha256": TOKEN_PROXY_DEFINITION_SHA256,
            "provider_tokenizer": False,
            "billable_token_count": False,
        },
        "schedule": schedule,
        "observations": observations,
        "aggregate": aggregate,
        "promotion_gate": _promotion_gate(complete_corpus, aggregate),
        "failures": failures,
    }
    return attach_self_hash(body, "report_sha256")


def write_private_json(path: pathlib.Path, value: Mapping[str, Any]) -> None:
    path = path.expanduser().resolve()
    path.parent.mkdir(parents=True, exist_ok=True)
    data = json.dumps(value, indent=2, sort_keys=True, ensure_ascii=False, allow_nan=False).encode("utf-8") + b"\n"
    fd, temporary = tempfile.mkstemp(prefix=".packet-format-v2-ab-", dir=path.parent)
    try:
        os.fchmod(fd, 0o600)
        with os.fdopen(fd, "wb") as handle:
            fd = -1
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
    finally:
        if fd >= 0:
            os.close(fd)
        try:
            os.unlink(temporary)
        except FileNotFoundError:
            pass


def parse_args(argv: Sequence[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--corpus", type=pathlib.Path, default=DEFAULT_CORPUS)
    parser.add_argument("--brain-bin", type=pathlib.Path, required=True)
    parser.add_argument("--repo", action="append", default=[], metavar="LOGICAL=PATH", required=True)
    parser.add_argument("--output", type=pathlib.Path, required=True)
    parser.add_argument("--timeout-seconds", type=float, default=120.0)
    parser.add_argument(
        "--max-tasks",
        type=int,
        help="Run only the deterministic corpus prefix; incomplete reports cannot pass promotion gates",
    )
    return parser.parse_args(argv)


def main(argv: Sequence[str] | None = None) -> int:
    args = parse_args(argv)
    if args.timeout_seconds <= 0:
        raise SystemExit("--timeout-seconds must be positive")
    if args.max_tasks is not None and args.max_tasks <= 0:
        raise SystemExit("--max-tasks must be positive")
    try:
        corpus = profile_brief.load_verified_corpus(args.corpus, REPO_ROOT)
        tasks = profile_brief.load_verified_prompts(corpus, REPO_ROOT)
        if args.max_tasks is not None:
            tasks = tasks[: args.max_tasks]
        mappings = profile_brief.parse_repo_mappings(args.repo)
        report = build_report(
            corpus,
            tasks,
            args.brain_bin.expanduser().resolve(),
            mappings,
            args.timeout_seconds,
        )
        write_private_json(args.output, report)
    except (PacketABError, profile_brief.ProfileRunError) as exc:
        raise SystemExit(str(exc)) from exc
    print(
        json.dumps(
            {
                "report_sha256": report["report_sha256"],
                "pairs": len(report["observations"]),
                "failures": len(report["failures"]),
                "complete_corpus": report["corpus"]["complete_corpus"],
                "eligible_for_separately_authorized_agent_quality_trial": report["promotion_gate"]["eligible_for_separately_authorized_agent_quality_trial"],
            },
            sort_keys=True,
        )
    )
    return 0 if not report["failures"] else 2


if __name__ == "__main__":
    raise SystemExit(main())
