#!/usr/bin/env python3
"""Privacy-safe, unpaid MCP packet-format A/B for `brain_brief`.

Prompts and product packets exist only in process memory. The retained report
contains logical repo labels, task/prompt hashes, product/runtime hashes,
packet/frame byte counts and hashes, a frozen offline token proxy, parser and
integrity booleans, canonical-projection hashes/parity, aggregate counts, and
fixed promotion gates. It never launches an agent or provider model.
"""

from __future__ import annotations

import argparse
import ast
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
SIZE_ATTRIBUTION_SCHEMA_VERSION = 2
EVIDENCE_ROLE = "development_only_unpaid_brain_brief_packet_format_ab"
FORMAT_ORDER = ("legacy_json", "compact_v1")
COMPACT_MARKER = "entire.brain_brief compact_v1"
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
INT_RE = re.compile(r"^-?(?:0|[1-9][0-9]*)$")
FLOAT_RE = re.compile(
    r"^-?(?:(?:0|[1-9][0-9]*)\.[0-9]+|(?:0|[1-9][0-9]*)(?:\.[0-9]+)?[eE][+-]?[0-9]+)$"
)
END_FIELD_ORDER = (
    "symbols",
    "relations",
    "neighbors",
    "runtime_traces",
    "test_roots",
    "test_suggestions",
    "history",
    "facts",
    "actions",
    "patterns",
    "consolidations",
    "themes",
    "guidance",
    "warnings",
    "body_records",
    "body_sha256",
)
SIMPLE_GO_ESCAPES = frozenset('abfnrtv\\"')
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
RECORD_SIZE_COUNT_FIELDS = (
    "record_count",
    "field_count",
    "string_scalar_field_count",
    "integer_scalar_field_count",
    "float_scalar_field_count",
    "boolean_scalar_field_count",
    "array_field_count",
    "array_item_count",
)
RECORD_SIZE_BYTE_FIELDS = (
    "encoded_byte_count",
    "tag_byte_count",
    "field_key_equals_byte_count",
    "scalar_value_payload_byte_count",
    "array_value_payload_byte_count",
    "separator_byte_count",
    "quoting_delimiter_byte_count",
    "escape_overhead_byte_count",
)
RECORD_SIZE_FIELDS = RECORD_SIZE_COUNT_FIELDS + RECORD_SIZE_BYTE_FIELDS
RECORD_SIZE_PARTITION_FIELDS = RECORD_SIZE_BYTE_FIELDS[1:]


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


def _scan_quoted(text: str, cursor: int) -> int:
    cursor += 1
    while cursor < len(text):
        if text[cursor] == "\\":
            cursor += 2
            continue
        if text[cursor] == '"':
            return cursor + 1
        cursor += 1
    raise CompactPacketError("unterminated compact quoted value")


def _scan_array(text: str, cursor: int) -> int:
    cursor += 1
    while cursor < len(text):
        if text[cursor] == '"':
            cursor = _scan_quoted(text, cursor)
            continue
        if text[cursor] == "]":
            return cursor + 1
        cursor += 1
    raise CompactPacketError("unterminated compact array value")


def _parse_go_quoted(raw: str) -> str:
    if len(raw) < 2 or raw[0] != '"' or raw[-1] != '"':
        raise CompactPacketError("invalid Go-quoted compact string")
    cursor = 1
    end = len(raw) - 1
    while cursor < end:
        character = raw[cursor]
        if character == '"' or ord(character) < 0x20:
            raise CompactPacketError("invalid unescaped character in compact string")
        if character != "\\":
            cursor += 1
            continue
        cursor += 1
        if cursor >= end:
            raise CompactPacketError("incomplete compact string escape")
        escape = raw[cursor]
        if escape in SIMPLE_GO_ESCAPES:
            cursor += 1
            continue
        digits = {"x": 2, "u": 4, "U": 8}.get(escape)
        if digits is None:
            raise CompactPacketError("unsupported compact string escape")
        start = cursor + 1
        stop = start + digits
        if stop > end or any(character not in HEX_DIGITS for character in raw[start:stop]):
            raise CompactPacketError("invalid hexadecimal compact string escape")
        cursor = stop
    try:
        value = ast.literal_eval(raw)
    except (SyntaxError, ValueError) as exc:
        raise CompactPacketError("invalid Go-quoted compact string") from exc
    if not isinstance(value, str):
        raise CompactPacketError("compact quoted value is not a string")
    return value


def _utf8_byte_count(value: str, role: str) -> int:
    try:
        return len(value.encode("utf-8"))
    except UnicodeEncodeError as exc:
        raise CompactPacketError(f"{role} is not valid Unicode") from exc


def _go_quoted_decoded_byte_count(raw: str) -> int:
    """Count decoded Go-string source bytes without interpreting them as Unicode.

    Go's ``\\xNN`` escape contributes one source byte even when NN is at least
    0x80. Named escapes do the same. Unicode escapes and literal runes instead
    contribute the UTF-8 width of their valid Unicode scalar value.
    """

    if len(raw) < 2 or raw[0] != '"' or raw[-1] != '"':
        raise CompactPacketError("invalid Go-quoted compact string")
    decoded_bytes = 0
    cursor = 1
    end = len(raw) - 1
    while cursor < end:
        character = raw[cursor]
        codepoint = ord(character)
        if character == '"' or codepoint < 0x20:
            raise CompactPacketError("invalid unescaped character in compact string")
        if character != "\\":
            if 0xD800 <= codepoint <= 0xDFFF:
                raise CompactPacketError("invalid Unicode scalar in compact string")
            decoded_bytes += _utf8_byte_count(character, "compact string literal rune")
            cursor += 1
            continue

        cursor += 1
        if cursor >= end:
            raise CompactPacketError("incomplete compact string escape")
        escape = raw[cursor]
        if escape in SIMPLE_GO_ESCAPES:
            decoded_bytes += 1
            cursor += 1
            continue
        digits = {"x": 2, "u": 4, "U": 8}.get(escape)
        if digits is None:
            raise CompactPacketError("unsupported compact string escape")
        start = cursor + 1
        stop = start + digits
        if stop > end or any(character not in HEX_DIGITS for character in raw[start:stop]):
            raise CompactPacketError("invalid hexadecimal compact string escape")
        escaped_value = int(raw[start:stop], 16)
        if escape == "x":
            decoded_bytes += 1
        else:
            if escaped_value > 0x10FFFF or 0xD800 <= escaped_value <= 0xDFFF:
                raise CompactPacketError("invalid Unicode scalar in compact string escape")
            decoded_bytes += len(chr(escaped_value).encode("utf-8"))
        cursor = stop
    return decoded_bytes


def _empty_record_size() -> dict[str, int]:
    return {field: 0 for field in RECORD_SIZE_FIELDS}


def _add_record_size(target: dict[str, int], source: Mapping[str, int]) -> None:
    for field in RECORD_SIZE_FIELDS:
        target[field] += source[field]


def _parse_compact_array_sized(raw: str) -> tuple[list[str], dict[str, int]]:
    if len(raw) < 2 or raw[0] != "[" or raw[-1] != "]":
        raise CompactPacketError("invalid compact string array")
    size = {
        "string_scalar_field_count": 0,
        "integer_scalar_field_count": 0,
        "float_scalar_field_count": 0,
        "boolean_scalar_field_count": 0,
        "array_field_count": 1,
        "array_item_count": 0,
        "scalar_value_payload_byte_count": 0,
        "array_value_payload_byte_count": 0,
        "separator_byte_count": 0,
        "quoting_delimiter_byte_count": 2,
        "escape_overhead_byte_count": 0,
    }
    if raw == "[]":
        return [], size
    values: list[str] = []
    cursor = 1
    end = len(raw) - 1
    while cursor < end:
        if raw[cursor] != '"':
            raise CompactPacketError("compact arrays require double-quoted strings")
        stop = _scan_quoted(raw, cursor)
        if stop > end:
            raise CompactPacketError("compact array string exceeds its boundary")
        item_raw = raw[cursor:stop]
        payload_bytes = _go_quoted_decoded_byte_count(item_raw)
        value = _parse_go_quoted(item_raw)
        raw_bytes = _utf8_byte_count(item_raw, "compact array item encoding")
        escape_overhead = raw_bytes - 2 - payload_bytes
        if escape_overhead < 0:
            raise CompactPacketError("compact array item has negative escape overhead")
        values.append(value)
        size["array_item_count"] += 1
        size["array_value_payload_byte_count"] += payload_bytes
        size["quoting_delimiter_byte_count"] += 2
        size["escape_overhead_byte_count"] += escape_overhead
        cursor = stop
        if cursor == end:
            break
        if raw[cursor] != ",":
            raise CompactPacketError("compact array values are not comma separated")
        size["separator_byte_count"] += 1
        cursor += 1
        if cursor == end:
            raise CompactPacketError("compact array has a trailing comma")
    if cursor != end:
        raise CompactPacketError("invalid compact string array")
    accounted = sum(size[field] for field in (
        "array_value_payload_byte_count",
        "separator_byte_count",
        "quoting_delimiter_byte_count",
        "escape_overhead_byte_count",
    ))
    if accounted != _utf8_byte_count(raw, "compact array encoding"):
        raise CompactPacketError("compact array size accounting mismatch")
    return values, size


def _parse_compact_array(raw: str) -> list[str]:
    return _parse_compact_array_sized(raw)[0]


def _parse_compact_value_sized(raw: str) -> tuple[Any, dict[str, int]]:
    size = {
        "string_scalar_field_count": 0,
        "integer_scalar_field_count": 0,
        "float_scalar_field_count": 0,
        "boolean_scalar_field_count": 0,
        "array_field_count": 0,
        "array_item_count": 0,
        "scalar_value_payload_byte_count": 0,
        "array_value_payload_byte_count": 0,
        "separator_byte_count": 0,
        "quoting_delimiter_byte_count": 0,
        "escape_overhead_byte_count": 0,
    }
    if raw.startswith('"'):
        payload_bytes = _go_quoted_decoded_byte_count(raw)
        value = _parse_go_quoted(raw)
        raw_bytes = _utf8_byte_count(raw, "compact scalar string encoding")
        escape_overhead = raw_bytes - 2 - payload_bytes
        if escape_overhead < 0:
            raise CompactPacketError("compact scalar string has negative escape overhead")
        size["string_scalar_field_count"] = 1
        size["scalar_value_payload_byte_count"] = payload_bytes
        size["quoting_delimiter_byte_count"] = 2
        size["escape_overhead_byte_count"] = escape_overhead
        return value, size
    if raw.startswith("["):
        return _parse_compact_array_sized(raw)
    if raw == "true":
        size["boolean_scalar_field_count"] = 1
        size["scalar_value_payload_byte_count"] = len(raw)
        return True, size
    if raw == "false":
        size["boolean_scalar_field_count"] = 1
        size["scalar_value_payload_byte_count"] = len(raw)
        return False, size
    if INT_RE.fullmatch(raw):
        size["integer_scalar_field_count"] = 1
        size["scalar_value_payload_byte_count"] = len(raw)
        return int(raw), size
    if FLOAT_RE.fullmatch(raw):
        value = float(raw)
        if not math.isfinite(value):
            raise CompactPacketError("compact number is not finite")
        size["float_scalar_field_count"] = 1
        size["scalar_value_payload_byte_count"] = len(raw)
        return value, size
    raise CompactPacketError("invalid compact scalar")


def _parse_compact_value(raw: str) -> Any:
    return _parse_compact_value_sized(raw)[0]


def _parse_compact_record_sized(line: str) -> tuple[dict[str, Any], dict[str, int]]:
    space = line.find(" ")
    if space < 0:
        tag = line
        remainder = ""
    else:
        tag = line[:space]
        remainder = line[space + 1 :]
    if TAG_RE.fullmatch(tag) is None:
        raise CompactPacketError("invalid compact record tag")
    size = _empty_record_size()
    size["record_count"] = 1
    size["tag_byte_count"] = _utf8_byte_count(tag, "compact record tag")
    fields: list[list[Any]] = []
    seen: set[str] = set()
    cursor = 0
    while cursor < len(remainder):
        equals = remainder.find("=", cursor)
        if equals <= cursor:
            raise CompactPacketError("invalid compact field")
        key = remainder[cursor:equals]
        if KEY_RE.fullmatch(key) is None or key in seen:
            raise CompactPacketError("invalid or duplicate compact field key")
        seen.add(key)
        size["field_key_equals_byte_count"] += _utf8_byte_count(key, "compact field key") + 1
        cursor = equals + 1
        if cursor >= len(remainder):
            raise CompactPacketError("compact field has no value")
        value_start = cursor
        if remainder[cursor] == '"':
            cursor = _scan_quoted(remainder, cursor)
        elif remainder[cursor] == "[":
            cursor = _scan_array(remainder, cursor)
        else:
            next_space = remainder.find(" ", cursor)
            cursor = len(remainder) if next_space < 0 else next_space
        raw = remainder[value_start:cursor]
        value, value_size = _parse_compact_value_sized(raw)
        fields.append([key, value])
        for field, amount in value_size.items():
            size[field] += amount
        if cursor < len(remainder):
            if remainder[cursor] != " ":
                raise CompactPacketError("compact fields are not space separated")
            cursor += 1
            if cursor == len(remainder):
                raise CompactPacketError("compact record has trailing space")
    size["field_count"] = len(fields)
    # One ASCII space introduces every field; array commas were counted while
    # parsing their values.
    size["separator_byte_count"] += len(fields)
    size["encoded_byte_count"] = _utf8_byte_count(line, "compact record")
    if sum(size[field] for field in RECORD_SIZE_PARTITION_FIELDS) != size["encoded_byte_count"]:
        raise CompactPacketError("compact record size accounting mismatch")
    return {"tag": tag, "fields": fields}, size


def _parse_compact_record(line: str) -> dict[str, Any]:
    return _parse_compact_record_sized(line)[0]


def _field_map(record: Mapping[str, Any]) -> dict[str, Any]:
    return {key: value for key, value in record["fields"]}


def _sum_record_sizes(values: Sequence[Mapping[str, int]]) -> dict[str, int]:
    total = _empty_record_size()
    for value in values:
        _add_record_size(total, value)
    return total


def _build_compact_size_attribution(
    data: bytes,
    records: Sequence[Mapping[str, Any]],
    record_sizes: Sequence[Mapping[str, int]],
) -> dict[str, Any]:
    if len(records) != len(record_sizes) or not records or records[-1]["tag"] != "end":
        raise CompactIntegrityError("compact attribution record shape mismatch")
    body_records = records[:-1]
    body_sizes = record_sizes[:-1]
    end_size = record_sizes[-1]
    per_tag = {tag: _empty_record_size() for tag in COMPACT_BODY_TAG_ORDER}
    for record, size in zip(body_records, body_sizes):
        tag = record["tag"]
        if tag not in COMPACT_BODY_TAGS:
            raise CompactPacketError("compact packet contains an unsupported body record tag")
        _add_record_size(per_tag[tag], size)

    body_totals = _sum_record_sizes(body_sizes)
    marker_bytes = len(COMPACT_MARKER.encode("ascii"))
    newline_bytes = data.count(b"\n")
    structural_partition = {
        "marker_byte_count": marker_bytes,
        "newline_byte_count": newline_bytes,
        "end_record_byte_count": end_size["encoded_byte_count"],
        "body_record_tag_byte_count": body_totals["tag_byte_count"],
        "body_field_key_equals_byte_count": body_totals["field_key_equals_byte_count"],
        "body_scalar_value_payload_byte_count": body_totals["scalar_value_payload_byte_count"],
        "body_array_value_payload_byte_count": body_totals["array_value_payload_byte_count"],
        "body_separator_byte_count": body_totals["separator_byte_count"],
        "body_quoting_delimiter_byte_count": body_totals["quoting_delimiter_byte_count"],
        "body_escape_overhead_byte_count": body_totals["escape_overhead_byte_count"],
    }
    partition_sum = sum(structural_partition.values())
    per_tag_encoded_sum = sum(value["encoded_byte_count"] for value in per_tag.values())
    per_tag_record_sum = sum(value["record_count"] for value in per_tag.values())
    per_tag_field_sum = sum(value["field_count"] for value in per_tag.values())
    invariants = {
        "exact_inner_byte_count": len(data),
        "structural_partition_byte_count_sum": partition_sum,
        "structural_partition_delta_byte_count": partition_sum - len(data),
        "body_encoded_byte_count": body_totals["encoded_byte_count"],
        "per_tag_encoded_byte_count_sum": per_tag_encoded_sum,
        "per_tag_encoded_delta_byte_count": per_tag_encoded_sum - body_totals["encoded_byte_count"],
        "body_record_count": body_totals["record_count"],
        "per_tag_record_count_sum": per_tag_record_sum,
        "per_tag_record_count_delta": per_tag_record_sum - body_totals["record_count"],
        "body_field_count": body_totals["field_count"],
        "per_tag_field_count_sum": per_tag_field_sum,
        "per_tag_field_count_delta": per_tag_field_sum - body_totals["field_count"],
    }
    if any(
        invariants[field] != 0
        for field in (
            "structural_partition_delta_byte_count",
            "per_tag_encoded_delta_byte_count",
            "per_tag_record_count_delta",
            "per_tag_field_count_delta",
        )
    ):
        raise CompactIntegrityError("compact structural size accounting mismatch")
    counts = {
        "body_record_count": body_totals["record_count"],
        "end_record_count": 1,
        "total_record_count": body_totals["record_count"] + 1,
        "body_field_count": body_totals["field_count"],
        "end_field_count": end_size["field_count"],
        "total_field_count": body_totals["field_count"] + end_size["field_count"],
        "body_string_scalar_field_count": body_totals["string_scalar_field_count"],
        "body_integer_scalar_field_count": body_totals["integer_scalar_field_count"],
        "body_float_scalar_field_count": body_totals["float_scalar_field_count"],
        "body_boolean_scalar_field_count": body_totals["boolean_scalar_field_count"],
        "body_array_field_count": body_totals["array_field_count"],
        "body_array_item_count": body_totals["array_item_count"],
        "distinct_body_tag_count": sum(value["record_count"] > 0 for value in per_tag.values()),
    }
    return {
        "schema_version": SIZE_ATTRIBUTION_SCHEMA_VERSION,
        "structural_partition": structural_partition,
        "counts": counts,
        "body_totals": body_totals,
        "per_tag": per_tag,
        "accounting_invariants": invariants,
    }


def parse_compact_packet(data: bytes) -> dict[str, Any]:
    try:
        text = data.decode("utf-8")
    except UnicodeDecodeError as exc:
        raise CompactPacketError("compact packet is not UTF-8") from exc
    if not text.endswith("\n"):
        raise CompactIntegrityError("compact packet is missing its final newline")
    lines = text[:-1].split("\n")
    if len(lines) < 3 or lines[0] != COMPACT_MARKER:
        raise CompactPacketError("compact packet version marker mismatch")
    parsed_and_sizes = [_parse_compact_record_sized(line) for line in lines[1:]]
    parsed = [record for record, _ in parsed_and_sizes]
    record_sizes = [size for _, size in parsed_and_sizes]
    end = parsed[-1]
    if end["tag"] != "end":
        raise CompactIntegrityError("compact end record is missing")
    if tuple(key for key, _ in end["fields"]) != END_FIELD_ORDER:
        raise CompactIntegrityError("compact end fields are not exact")
    end_fields = _field_map(end)
    for key in END_FIELD_ORDER[:-1]:
        value = end_fields[key]
        if isinstance(value, bool) or not isinstance(value, int) or value < 0:
            raise CompactIntegrityError(f"compact end count is not a nonnegative integer: {key}")
    body_sha256 = end_fields["body_sha256"]
    if not isinstance(body_sha256, str) or re.fullmatch(r"sha256:[0-9a-f]{64}", body_sha256) is None:
        raise CompactIntegrityError("compact body checksum field is invalid")

    end_start = data.rfind(b"\nend ")
    if end_start < 0:
        raise CompactIntegrityError("compact end record boundary is missing")
    body = data[: end_start + 1]
    records = parsed[:-1]
    if end_fields["body_records"] != len(records):
        raise CompactIntegrityError("compact body record count mismatch")
    expected_body_hash = "sha256:" + sha256_bytes(body)
    if end_fields["body_sha256"] != expected_body_hash:
        raise CompactIntegrityError("compact body checksum mismatch")

    tag_counts = Counter(record["tag"] for record in records)
    expected_counts = {
        "symbols": tag_counts["symbol"],
        "relations": tag_counts["relation"],
        "neighbors": tag_counts["neighbor"],
        "runtime_traces": tag_counts["runtime_trace"],
        "test_roots": tag_counts["test_root"],
        "test_suggestions": tag_counts["test_suggestion"],
        "history": tag_counts["history"],
        "facts": tag_counts["fact"],
        "actions": tag_counts["action"],
        "patterns": tag_counts["pattern"],
        "consolidations": tag_counts["consolidation"],
        "themes": tag_counts["theme"],
        "guidance": tag_counts["guidance"],
        "warnings": sum(
            tag_counts[tag]
            for tag in (
                "freshness_warning",
                "semantic_warning",
                "semantic_partial_failure",
                "blind_spot",
                "warning",
            )
        ),
    }
    for key, expected in expected_counts.items():
        if end_fields[key] != expected:
            raise CompactIntegrityError(f"compact end count mismatch for {key}")
    size_attribution = _build_compact_size_attribution(data, parsed, record_sizes)
    return {
        "records": records,
        "body_record_count": len(records),
        "body_sha256": sha256_bytes(body),
        "structural_size_attribution": size_attribution,
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
        compact = parse_compact_packet(inner[1])
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
    format_metrics["compact_v1"]["integrity_ok"] = True
    format_metrics["compact_v1"]["body_record_count"] = compact["body_record_count"]
    format_metrics["compact_v1"]["body_sha256"] = compact["body_sha256"]
    format_metrics["compact_v1"]["structural_size_attribution"] = compact[
        "structural_size_attribution"
    ]

    legacy_projection_bytes = canonical_json_bytes(legacy_projection)
    compact_projection_bytes = canonical_json_bytes(compact_projection)
    parity = legacy_projection_bytes == compact_projection_bytes
    legacy_bytes = format_metrics["legacy_json"]["inner_byte_count"]
    compact_bytes = format_metrics["compact_v1"]["inner_byte_count"]
    legacy_framed = format_metrics["legacy_json"]["framed_byte_count"]
    compact_framed = format_metrics["compact_v1"]["framed_byte_count"]
    legacy_tokens = format_metrics["legacy_json"]["offline_token_proxy_count"]
    compact_tokens = format_metrics["compact_v1"]["offline_token_proxy_count"]
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
        "policy": "verified_brief_profile_corpus_order_with_adjacent_legacy_json_then_compact_v1",
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
        "newline_byte_count",
        "end_record_byte_count",
        "body_record_tag_byte_count",
        "body_field_key_equals_byte_count",
        "body_scalar_value_payload_byte_count",
        "body_array_value_payload_byte_count",
        "body_separator_byte_count",
        "body_quoting_delimiter_byte_count",
        "body_escape_overhead_byte_count",
    )
    count_fields = (
        "body_record_count",
        "end_record_count",
        "total_record_count",
        "body_field_count",
        "end_field_count",
        "total_field_count",
        "body_string_scalar_field_count",
        "body_integer_scalar_field_count",
        "body_float_scalar_field_count",
        "body_boolean_scalar_field_count",
        "body_array_field_count",
        "body_array_item_count",
        "distinct_body_tag_count",
    )
    for value in values:
        if value.get("schema_version") != SIZE_ATTRIBUTION_SCHEMA_VERSION:
            raise PacketABError("compact structural size attribution schema mismatch")
    partition = {
        field: sum(value["structural_partition"][field] for value in values)
        for field in partition_fields
    }
    counts = {
        field: sum(value["counts"][field] for value in values)
        for field in count_fields
    }
    body_totals = {
        field: sum(value["body_totals"][field] for value in values)
        for field in RECORD_SIZE_FIELDS
    }
    per_tag = {
        tag: {
            field: sum(value["per_tag"][tag][field] for value in values)
            for field in RECORD_SIZE_FIELDS
        }
        for tag in COMPACT_BODY_TAG_ORDER
    }
    exact_inner_sum = sum(
        value["accounting_invariants"]["exact_inner_byte_count"] for value in values
    )
    partition_sum = sum(partition.values())
    per_tag_encoded_sum = sum(value["encoded_byte_count"] for value in per_tag.values())
    per_tag_record_sum = sum(value["record_count"] for value in per_tag.values())
    per_tag_field_sum = sum(value["field_count"] for value in per_tag.values())
    invariants = {
        "exact_inner_byte_count": exact_inner_sum,
        "structural_partition_byte_count_sum": partition_sum,
        "structural_partition_delta_byte_count": partition_sum - exact_inner_sum,
        "body_encoded_byte_count": body_totals["encoded_byte_count"],
        "per_tag_encoded_byte_count_sum": per_tag_encoded_sum,
        "per_tag_encoded_delta_byte_count": per_tag_encoded_sum - body_totals["encoded_byte_count"],
        "body_record_count": body_totals["record_count"],
        "per_tag_record_count_sum": per_tag_record_sum,
        "per_tag_record_count_delta": per_tag_record_sum - body_totals["record_count"],
        "body_field_count": body_totals["field_count"],
        "per_tag_field_count_sum": per_tag_field_sum,
        "per_tag_field_count_delta": per_tag_field_sum - body_totals["field_count"],
    }
    if any(
        invariants[field] != 0
        for field in (
            "structural_partition_delta_byte_count",
            "per_tag_encoded_delta_byte_count",
            "per_tag_record_count_delta",
            "per_tag_field_count_delta",
        )
    ):
        raise PacketABError("aggregate compact structural size accounting mismatch")
    return {
        "schema_version": SIZE_ATTRIBUTION_SCHEMA_VERSION,
        "packet_count": len(values),
        "structural_partition": partition,
        "summed_packet_counts": counts,
        "body_totals": body_totals,
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
        "compact_integrity_pass_count": sum(row["formats"]["compact_v1"]["integrity_ok"] for row in successes),
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
        [row["formats"]["compact_v1"]["structural_size_attribution"] for row in successes]
    )
    if (
        size_aggregate["accounting_invariants"]["exact_inner_byte_count"]
        != result["compact_v1"]["inner_byte_count_sum"]
    ):
        raise PacketABError("aggregate attribution does not match compact inner-byte total")
    result["compact_v1"]["structural_size_attribution"] = size_aggregate
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
            result["compact_v1"]["inner_byte_count_sum"],
        )
        result["pooled_framed_byte_reduction_ppm"] = reduction_ppm(
            result["legacy_json"]["framed_byte_count_sum"],
            result["compact_v1"]["framed_byte_count_sum"],
        )
        result["pooled_token_proxy_reduction_ppm"] = reduction_ppm(
            result["legacy_json"]["offline_token_proxy_count_sum"],
            result["compact_v1"]["offline_token_proxy_count_sum"],
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
        "all_compact_packets_parse": successful > 0 and aggregate["compact_v1"]["parse_pass_count"] == successful,
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
    fd, temporary = tempfile.mkstemp(prefix=".packet-format-ab-", dir=path.parent)
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
