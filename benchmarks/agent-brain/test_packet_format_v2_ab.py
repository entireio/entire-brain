#!/usr/bin/env python3
"""Tests for the unpaid, privacy-safe brain_brief compact_v2 packet-format A/B runner."""

from __future__ import annotations

import collections
import copy
import hashlib
import importlib.util
import json
import pathlib
import stat
import sys
import tempfile
import unittest
from unittest import mock


HERE = pathlib.Path(__file__).resolve().parent
REPO_ROOT = HERE.parents[1]
if str(HERE) not in sys.path:
    sys.path.insert(0, str(HERE))
SPEC = importlib.util.spec_from_file_location("packet_format_v2_ab", HERE / "packet_format_v2_ab.py")
assert SPEC is not None and SPEC.loader is not None
packet_format_v2_ab = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = packet_format_v2_ab
SPEC.loader.exec_module(packet_format_v2_ab)


def _sha(value: str) -> str:
    return hashlib.sha256(value.encode("utf-8")).hexdigest()


def _assert_int_leaves(test: unittest.TestCase, value: object) -> None:
    if isinstance(value, dict):
        for child in value.values():
            _assert_int_leaves(test, child)
        return
    test.assertIs(type(value), int)


def _resign(packet: bytes) -> bytes:
    footer_at = packet.rfind(b"\nend\t")
    if footer_at < 0:
        return packet
    body = packet[: footer_at + 1]
    footer_end = packet.find(b"\n", footer_at + 1)
    if footer_end < 0:
        return packet
    parts = packet[footer_at + 1 : footer_end].split(b"\t")
    if len(parts) != 3:
        return packet
    parts[2] = hashlib.sha256(body).hexdigest().encode("ascii")
    return body + b"\t".join(parts) + b"\n"


def _replace_line(packet: bytes, prefix: bytes, replacement: bytes) -> bytes:
    lines = packet.splitlines(keepends=True)
    for index, line in enumerate(lines):
        if line.startswith(prefix):
            newline = b"\n" if line.endswith(b"\n") else b""
            lines[index] = replacement + newline
            return _resign(b"".join(lines))
    raise AssertionError(f"missing public golden prefix: {prefix!r}")


def _insert_before_line(packet: bytes, prefix: bytes, inserted: bytes) -> bytes:
    lines = packet.splitlines(keepends=True)
    for index, line in enumerate(lines):
        if line.startswith(prefix):
            lines.insert(index, inserted + b"\n")
            return _resign(b"".join(lines))
    raise AssertionError(f"missing public golden prefix: {prefix!r}")


def _replace_nth_line(
    packet: bytes, prefix: bytes, occurrence: int, replacement: bytes
) -> bytes:
    lines = packet.splitlines(keepends=True)
    seen = 0
    for index, line in enumerate(lines):
        if not line.startswith(prefix):
            continue
        if seen == occurrence:
            lines[index] = replacement + b"\n"
            return _resign(b"".join(lines))
        seen += 1
    raise AssertionError(f"missing public golden occurrence: {prefix!r} #{occurrence}")


def _remove_line(packet: bytes, prefix: bytes) -> bytes:
    lines = packet.splitlines(keepends=True)
    for index, line in enumerate(lines):
        if line.startswith(prefix):
            del lines[index]
            return _resign(b"".join(lines))
    raise AssertionError(f"missing public golden prefix: {prefix!r}")


def _rich_legacy_packet() -> dict[str, object]:
    symbol = {
        "id": "sym:validate",
        "kind": "function",
        "name": "Validate",
        "qualified_name": "pkg.Validate",
        "file_path": "internal/a.go",
        "start_line": 10,
        "end_line": 20,
        "signature": "func Validate(token string) error",
        "language": "go",
        "score": 9,
        "confidence": 0.95,
        "reason": "task term match",
        "warning_codes": ["partial"],
    }
    relation = {
        "id": "rel:1",
        "type": "CALLS",
        "from_id": "sym:validate",
        "to_id": "sym:persist",
        "file_path": "internal/a.go",
        "start_line": 16,
        "end_line": 16,
        "relation_scope": "file",
        "resolution": "exact",
        "target_kind": "symbol",
        "confidence": 0.9,
        "evidence": [
            {
                "kind": "call",
                "file_path": "internal/a.go",
                "start_line": 16,
                "end_line": 16,
                "detail": "direct call",
            }
        ],
    }
    runtime_trace = {
        "id": "runtime:1",
        "type": "RUNTIME_TRACE",
        "from_id": "sym:validate",
        "to_id": "sym:persist",
        "reason": "observed edge",
        "evidence": [
            {
                "kind": "trace",
                "file_path": "trace.ndjson",
                "start_line": 2,
                "end_line": 2,
                "detail": "captured locally",
            }
        ],
    }
    return {
        "generated_at": "PRIVATE_GENERATED_AT",
        "task": "repair validation ordering",
        "status": {
            "repo": {"root": "/PRIVATE/REPO", "key": "repo-key"},
            "brain": {
                "path": "/PRIVATE/BRAIN",
                "schema_version": 2,
                "generated_at": "PRIVATE_BRAIN_TIME",
            },
            "sources": {
                "seed": True,
                "sessions": True,
                "semantic": True,
                "history": True,
                "facts": True,
            },
            "facts": {
                "facts": 4,
                "distilled": 3,
                "authored": 1,
                "superseded": 1,
                "branches": 2,
                "proposals": 1,
                "verification": {
                    "facts": 3,
                    "verified": 2,
                    "stale": 1,
                    "orphaned": 0,
                    "unverifiable_here": 0,
                    "sampled_of": 4,
                },
            },
            "semantic": {
                "provider": {
                    "name": "entire-graph",
                    "version": "1.2",
                    "schema_version": "1.1",
                    "snapshot_path": "/PRIVATE/SNAPSHOT",
                    "store_path": "/PRIVATE/STORE",
                    "capabilities": ["relations", "tests"],
                },
                "coverage": {
                    "files": 12,
                    "symbols": 30,
                    "relations": 22,
                    "warnings": 1,
                    "partial_failures": 1,
                    "warning_details": [
                        {
                            "code": "WARN",
                            "severity": "warning",
                            "path": "warn.go",
                            "effect": "partial",
                            "detail": "unsupported syntax",
                        }
                    ],
                    "partial_failure_details": [
                        {
                            "code": "FAIL",
                            "severity": "error",
                            "path": "broken.go",
                            "effect": "missing symbols",
                            "detail": "parse failed",
                        }
                    ],
                },
                "freshness": {
                    "severity": "degraded",
                    "axes": {
                        "worktree": {"state": "dirty", "detail": "live edits"},
                        "head": {"state": "ok"},
                    },
                    "warnings": [
                        {
                            "code": "STALE",
                            "severity": "warning",
                            "detail": "worktree differs",
                        }
                    ],
                },
                "blind_spots": [
                    {
                        "path": "generated.go",
                        "code": "ignored",
                        "detail": "excluded by policy",
                    }
                ],
            },
            "live": {
                "branch": "feature",
                "head": "abc123",
                "dirty": True,
                "staged": ["internal/a.go"],
                "unstaged": ["internal/a_test.go"],
                "untracked": ["notes.txt"],
                "changed_files": ["internal/a.go", "internal/a_test.go", "notes.txt"],
                "diff_stat": "2 files changed",
                "changed_symbol_hints": [copy.deepcopy(symbol)],
                "warnings": ["live warning"],
            },
            "warnings": ["status warning"],
        },
        "semantic": {
            "context": {
                "symbols": [copy.deepcopy(symbol)],
                "relations": [relation],
                "neighbors": [
                    {
                        "id": "sym:helper",
                        "kind": "function",
                        "name": "Helper",
                        "qualified_name": "pkg.Helper",
                        "file_path": "internal/helper.go",
                        "start_line": 4,
                        "end_line": 8,
                    }
                ],
            },
            "runtime_traces": [runtime_trace],
            "tests": {
                "roots": [
                    {
                        "id": "test:root",
                        "kind": "function",
                        "name": "TestValidate",
                        "file_path": "internal/a_test.go",
                        "start_line": 10,
                        "end_line": 30,
                    }
                ],
                "suggestions": [
                    {
                        "symbol": {
                            "id": "test:suggestion",
                            "kind": "function",
                            "name": "TestValidateOrder",
                            "file_path": "internal/a_test.go",
                            "start_line": 32,
                            "end_line": 44,
                        },
                        "reason": "covers the boundary",
                    }
                ],
            },
        },
        "history": {
            "matches": [
                {
                    "path": "history/main.jsonl",
                    "line": 8,
                    "timestamp": "2026-07-01",
                    "score": 12,
                    "matched_terms": ["validation", "order"],
                    "excerpt": "Decision: preserve validation order.",
                }
            ]
        },
        "facts": [
            {
                "id": "fact:1",
                "paths": ["architecture.validation.order"],
                "kind": "invariant",
                "locus": ["pkg.Validate"],
                "text": "Validation runs before persistence.",
                "branch": "feature",
                "origin": "distilled",
                "status": "active",
                "confidence": "high",
                "provenance": [
                    {
                        "session_id": "PRIVATE_SESSION",
                        "checkpoint_id": "PRIVATE_CHECKPOINT",
                        "verified": True,
                    }
                ],
                "related_ids": ["fact:2"],
            }
        ],
        "facts_locus_drift": {
            "fact:orphan": ["GoneSymbol"],
            "fact:1": ["OldValidate"],
        },
        "action_checklist": [
            {
                "file": "internal/a.go",
                "symbol": "pkg.Validate",
                "action": "inspect validation before persistence",
                "evidence": "history and current code",
            }
        ],
        "likely_edit_files": ["internal/a.go"],
        "likely_test_files": ["internal/a_test.go"],
        "likely_files": ["internal/a.go", "internal/a_test.go", "docs/order.md"],
        "patterns": [
            {
                "id": "pattern:1",
                "type": "procedure",
                "scope": "repo",
                "kind": "workflow",
                "title": "Validate then persist",
                "strength": 0.88,
                "strength_label": "strong",
                "support": 5,
                "reinforcement": {"success": 4, "corrected": 1, "neutral": 0},
                "example": {"path": "patterns/episodes.ndjson", "line": 12},
                "note": "Reuse the validated sequence.",
                "intent_sig": "validation ordering",
                "gram": "validate>persist",
                "dossier_status": "current",
                "verdict": "accepted",
            }
        ],
        "consolidations": [
            {
                "pattern_id": "pattern:1",
                "type": "procedure",
                "title": "Validate then persist",
                "trigger": "when validation changes",
                "confidence": 0.88,
                "status": "current",
                "verdict": "accepted",
                "workflow": ["validate input", "persist state"],
                "verification": ["run validation tests"],
                "failure_modes": ["persisting first"],
                "anchor": {
                    "session_id": "PRIVATE_SESSION",
                    "checkpoint_id": "PRIVATE_CHECKPOINT",
                    "transcript": "sessions/redacted.jsonl",
                    "start_line": 10,
                    "end_line": 14,
                    "outcome": "success",
                },
            }
        ],
        "themes": [
            {
                "id": "theme:1",
                "title": "Read before edit",
                "description": "Inspect before editing.",
                "shape": "read_only",
                "support": 4,
                "strength": 0.75,
                "status": "current",
                "verdict": "accepted",
            }
        ],
        "guidance": ["Inspect dirty files before trusting the snapshot."],
        "warnings": ["brief warning"],
    }


def _repeat_rich_legacy_packet() -> dict[str, object]:
    """Mirror the public two-copy Go golden fixture at the JSON layer."""

    value = _rich_legacy_packet()
    status = value["status"]
    live = status["live"]
    semantic_status = status["semantic"]
    freshness = semantic_status["freshness"]
    coverage = semantic_status["coverage"]
    semantic = value["semantic"]
    context = semantic["context"]
    tests = semantic["tests"]

    def duplicate(items: list[object]) -> list[object]:
        return copy.deepcopy(items) + copy.deepcopy(items)

    live["changed_symbol_hints"] = duplicate(live["changed_symbol_hints"])
    freshness["warnings"] = duplicate(freshness["warnings"])
    coverage["warning_details"] = duplicate(coverage["warning_details"])
    coverage["partial_failure_details"] = duplicate(coverage["partial_failure_details"])
    semantic_status["blind_spots"] = duplicate(semantic_status["blind_spots"])
    for key in ("symbols", "relations", "neighbors"):
        context[key] = duplicate(context[key])
    semantic["runtime_traces"] = duplicate(semantic["runtime_traces"])
    tests["roots"] = duplicate(tests["roots"])
    tests["suggestions"] = duplicate(tests["suggestions"])
    value["likely_edit_files"] = duplicate(value["likely_edit_files"])
    value["likely_test_files"] = duplicate(value["likely_test_files"])
    value["likely_files"] = duplicate(value["likely_files"] + ["docs/other.md"])
    for key in (
        "history",
        "facts",
        "action_checklist",
        "patterns",
        "consolidations",
        "themes",
        "guidance",
        "warnings",
    ):
        if key == "history":
            value[key]["matches"] = duplicate(value[key]["matches"])
        else:
            value[key] = duplicate(value[key])
    status["warnings"] = duplicate(status["warnings"])
    live["warnings"] = duplicate(live["warnings"])
    value["facts_locus_drift"]["fact:orphan-0"] = ["GoneSymbol"]
    value["facts_locus_drift"]["fact:orphan-1"] = ["GoneSymbol"]
    return value


FAKE_MCP = r'''#!/usr/bin/env python3
import hashlib
import json
import pathlib
import sys


def frame(value):
    payload = json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode("utf-8")
    return f"Content-Length: {len(payload)}\r\n\r\n".encode("ascii") + payload


def requests(data):
    rows = []
    cursor = 0
    while cursor < len(data):
        end = data.index(b"\r\n\r\n", cursor)
        length = int(data[cursor:end].split(b": ", 1)[1])
        start = end + 4
        stop = start + length
        rows.append(json.loads(data[start:stop]))
        cursor = stop
    return rows


def compact(prompt):
    body = (
        "entire.brain_brief compact_v2\n"
        "legend ~=absent ^=previous_same_opcode_record_same_column\n"
        + "task value=" + json.dumps(prompt, ensure_ascii=False) + "\n"
        + 'status repo_key="fixture" brain_schema=1 dirty=false changed_files=0\n'
        + "sources seed=false sessions=false semantic=false history=false facts=false\n"
        + "live\n"
        + 'guidance text="PRIVATE_PACKET_GUIDANCE_SENTINEL"\n'
    )
    digest = hashlib.sha256(body.encode("utf-8")).hexdigest()
    return body + "end\t5\t" + digest + "\n"


def legacy(prompt):
    value = {
        "generated_at": "PRIVATE_PACKET_TIME_SENTINEL",
        "task": prompt,
        "status": {
            "repo": {"root": "PRIVATE_REPO_PATH_SENTINEL", "key": "fixture"},
            "brain": {"path": "PRIVATE_BRAIN_PATH_SENTINEL", "schema_version": 1},
            "sources": {"seed": False, "sessions": False, "semantic": False, "history": False, "facts": False},
            "live": {"dirty": False, "changed_files": []},
        },
        "semantic": {
            "context": {"symbols": [], "relations": [], "neighbors": None},
            "runtime_traces": None,
            "tests": {"roots": None, "suggestions": None},
        },
        "history": {"matches": None},
        "guidance": ["PRIVATE_PACKET_GUIDANCE_SENTINEL"],
        "private_padding_ignored_by_projection": "P" * 512,
    }
    return json.dumps(value, ensure_ascii=False, sort_keys=True, indent=2) + "\n"


brain = pathlib.Path(__file__).with_name("private-brain")
if sys.argv[1:] == ["path", "."]:
    print(brain)
    raise SystemExit(0)
if sys.argv[1:] != ["mcp"]:
    raise SystemExit(64)
raw = sys.stdin.buffer.read()
if b"FAIL_PAIR_SENTINEL" in raw:
    sys.stdout.buffer.write(b"PRIVATE_STDOUT_PACKET_SENTINEL")
    sys.stderr.buffer.write(b"PRIVATE_STDERR_SENTINEL")
    raise SystemExit(9)
responses = []
for request in requests(raw):
    prompt = request["params"]["arguments"]["task"]
    packet_format = request["params"]["arguments"]["packet_format"]
    text = legacy(prompt) if packet_format == "legacy_json" else compact(prompt)
    responses.append(frame({"jsonrpc": "2.0", "id": request["id"], "result": {"content": [{"type": "text", "text": text}]}}))
sys.stdout.buffer.write(b"".join(responses))
sys.stderr.buffer.write(b"PRIVATE_STDERR_SENTINEL")
'''


class PacketFormatV2ABFoundationTests(unittest.TestCase):
    def test_runner_identity_recomputes_exact_hash_only_inputs(self) -> None:
        identity = packet_format_v2_ab.build_runner_identity()
        self.assertEqual(
            set(identity),
            {
                "schema_version",
                "runner_source_sha256",
                "profile_brief_source_sha256",
                "product_contract_commit_sha1",
                "compact_v2_golden_sha256",
                "compact_v2_contract_fingerprint_sha256",
                "runner_identity_sha256",
            },
        )
        self.assertEqual(identity["schema_version"], 1)
        self.assertTrue(packet_format_v2_ab.verify_runner_identity(identity))
        self.assertEqual(
            identity["runner_source_sha256"],
            hashlib.sha256(pathlib.Path(packet_format_v2_ab.__file__).read_bytes()).hexdigest(),
        )
        self.assertEqual(
            identity["profile_brief_source_sha256"],
            hashlib.sha256(
                pathlib.Path(packet_format_v2_ab.profile_brief.__file__).read_bytes()
            ).hexdigest(),
        )
        self.assertEqual(
            identity["product_contract_commit_sha1"],
            "172aaa3f361376cc00914e26d570f80b74442b5a",
        )
        self.assertEqual(
            identity["compact_v2_golden_sha256"],
            "e0bc12b75059a7bba809ef1a66739c49d397852a30271da1b3e2ef4f9a6ca982",
        )
        self.assertEqual(
            identity["compact_v2_contract_fingerprint_sha256"],
            "df4b35287f95afc8773371006e83506e2f2b729e9a03ccd80f51b961de647d91",
        )
        self.assertEqual(
            identity["compact_v2_contract_fingerprint_sha256"],
            packet_format_v2_ab.compact_v2_contract_fingerprint_sha256(),
        )
        retained = json.dumps(identity, sort_keys=True)
        self.assertNotIn("/", retained)
        for key, value in identity.items():
            if key == "schema_version":
                continue
            expected_length = 40 if key == "product_contract_commit_sha1" else 64
            self.assertRegex(value, rf"^[0-9a-f]{{{expected_length}}}$")

    def test_runner_identity_rejects_field_contract_and_golden_tampering(self) -> None:
        identity = packet_format_v2_ab.build_runner_identity()
        for field in identity:
            tampered = copy.deepcopy(identity)
            if field == "schema_version":
                tampered[field] += 1
            else:
                tampered[field] = ("0" if tampered[field][0] != "0" else "1") + tampered[field][1:]
            with self.subTest(field=field):
                self.assertFalse(packet_format_v2_ab.verify_runner_identity(tampered))
        with mock.patch.object(
            packet_format_v2_ab,
            "COMPACT_V2_GOLDEN_SHA256",
            "0" * 64,
        ):
            with self.assertRaises(packet_format_v2_ab.PacketABError):
                packet_format_v2_ab.build_runner_identity()
        with mock.patch.object(
            packet_format_v2_ab,
            "COMPACT_V2_LEGEND",
            "legend tampered",
        ):
            self.assertFalse(packet_format_v2_ab.verify_runner_identity(identity))

    def test_checked_corpus_schedule_is_exact_same_114_query_order(self) -> None:
        corpus = packet_format_v2_ab.profile_brief.load_verified_corpus(
            packet_format_v2_ab.DEFAULT_CORPUS, REPO_ROOT
        )
        tasks = packet_format_v2_ab.profile_brief.load_verified_prompts(corpus, REPO_ROOT)
        schedule = packet_format_v2_ab.build_schedule(tasks)
        self.assertEqual(len(tasks), 114)
        self.assertEqual(
            collections.Counter(task["logical_repo"] for task in tasks),
            {"entire-brain": 25, "entire-cli": 68, "entire-db": 21},
        )
        self.assertEqual(schedule["task_count"], 114)
        for sequence, (task, entry) in enumerate(zip(tasks, schedule["entries"])):
            self.assertEqual(entry["sequence"], sequence)
            self.assertEqual(entry["logical_repo"], task["logical_repo"])
            self.assertEqual(entry["task_sha256"], task["task_sha256"])
            self.assertEqual(entry["format_order"], ["legacy_json", "compact_v2"])

    def test_frozen_token_proxy_and_definition_hash(self) -> None:
        self.assertEqual(packet_format_v2_ab.offline_token_proxy_count(b""), 0)
        self.assertEqual(packet_format_v2_ab.offline_token_proxy_count(b"a"), 1)
        self.assertEqual(packet_format_v2_ab.offline_token_proxy_count(b"abcd"), 1)
        self.assertEqual(packet_format_v2_ab.offline_token_proxy_count(b"abcde"), 2)
        self.assertEqual(
            packet_format_v2_ab.TOKEN_PROXY_DEFINITION_SHA256,
            hashlib.sha256(packet_format_v2_ab.TOKEN_PROXY_DEFINITION.encode("ascii")).hexdigest(),
        )

    def test_frozen_schema_is_exact_and_natural_language_cannot_reference(self) -> None:
        self.assertEqual(
            tuple(schema[0] for schema in packet_format_v2_ab.COMPACT_V2_SCHEMAS),
            packet_format_v2_ab.COMPACT_BODY_TAG_ORDER,
        )
        self.assertEqual(len(packet_format_v2_ab.COMPACT_V2_BY_OPCODE), 32)
        denied = {
            "value",
            "diff_stat",
            "effect",
            "detail",
            "signature",
            "reason",
            "suggestion_reason",
            "excerpt",
            "text",
            "action",
            "evidence",
            "title",
            "note",
            "intent_sig",
            "gram",
            "trigger",
            "workflow",
            "verification",
            "failure_modes",
            "description",
        }
        for tag, _, fields in packet_format_v2_ab.COMPACT_V2_SCHEMAS:
            for name, _, reference_allowed in fields:
                with self.subTest(tag=tag, name=name):
                    if name in denied:
                        self.assertFalse(reference_allowed)

    def test_independent_legacy_projection_omits_private_host_metadata(self) -> None:
        projection = packet_format_v2_ab.canonical_projection_from_legacy(
            _repeat_rich_legacy_packet()
        )
        self.assertEqual(len(projection), 65)
        retained = json.dumps(projection, sort_keys=True)
        for forbidden in (
            "PRIVATE_GENERATED_AT",
            "/PRIVATE/REPO",
            "/PRIVATE/BRAIN",
            "PRIVATE_BRAIN_TIME",
            "/PRIVATE/SNAPSHOT",
            "/PRIVATE/STORE",
            "PRIVATE_SESSION",
            "PRIVATE_CHECKPOINT",
        ):
            self.assertNotIn(forbidden, retained)

    def test_mcp_frame_parser_is_bounded_and_exact(self) -> None:
        def response(identifier: int) -> bytes:
            payload = packet_format_v2_ab.canonical_json_bytes(
                {
                    "jsonrpc": "2.0",
                    "id": identifier,
                    "result": {"content": [{"type": "text", "text": "x"}]},
                }
            )
            return f"Content-Length: {len(payload)}\r\n\r\n".encode("ascii") + payload

        parsed = packet_format_v2_ab.parse_mcp_response_frames(response(1) + response(2))
        self.assertEqual(len(parsed), 2)
        self.assertEqual(packet_format_v2_ab.extract_mcp_text(parsed[1][0], 2), "x")
        with self.assertRaises(packet_format_v2_ab.MCPShapeError):
            packet_format_v2_ab.parse_mcp_response_frames(response(1))
        with self.assertRaises(packet_format_v2_ab.MCPShapeError):
            packet_format_v2_ab.parse_mcp_response_frames(
                (response(1) + response(2)).replace(
                    b"Content-Length: ", b"content-length: ", 1
                )
            )

    def test_promotion_thresholds_are_exact_and_never_authorize_paid_or_default(self) -> None:
        aggregate = {
            "scheduled_pair_count": 114,
            "successful_pair_count": 114,
            "legacy_json": {"parse_pass_count": 114},
            "compact_v2": {"parse_pass_count": 114},
            "compact_integrity_pass_count": 114,
            "canonical_parity_count": 114,
            "inner_byte_reduction_ppm": {
                "minimum": 0,
                "lower_median": packet_format_v2_ab.INNER_BYTE_REDUCTION_GATE_PPM,
            },
            "token_proxy_reduction_ppm": {
                "lower_median": packet_format_v2_ab.TOKEN_PROXY_REDUCTION_GATE_PPM,
            },
        }
        gate = packet_format_v2_ab._promotion_gate(True, aggregate)
        self.assertTrue(gate["eligible_for_separately_authorized_agent_quality_trial"])
        self.assertFalse(gate["paid_agent_quality_trial_authorized"])
        self.assertFalse(gate["change_mcp_or_cli_default_authorized"])
        below = copy.deepcopy(aggregate)
        below["inner_byte_reduction_ppm"]["lower_median"] -= 1
        self.assertFalse(
            packet_format_v2_ab._promotion_gate(True, below)[
                "eligible_for_separately_authorized_agent_quality_trial"
            ]
        )

    def test_report_rejects_nonprefix_task_selection_without_launching(self) -> None:
        corpus = {
            "tasks": [
                {
                    "logical_repo": "entire-brain",
                    "task_sha256": "a" * 64,
                    "prompt_sha256": "b" * 64,
                }
            ]
        }
        task = {
            "logical_repo": "entire-brain",
            "task_sha256": "c" * 64,
            "prompt_sha256": "b" * 64,
            "prompt": "private",
        }
        with self.assertRaises(packet_format_v2_ab.PacketABError):
            packet_format_v2_ab.build_report(
                corpus, [task], pathlib.Path("missing"), {}, 1.0
            )


class PacketFormatV2PrimitiveTests(unittest.TestCase):
    def test_go_quote_is_canonical_and_rejects_alternate_or_invalid_escapes(self) -> None:
        values = (
            "",
            "plain",
            "space here",
            "line\nnext\tcolumn",
            'quote"slash\\',
            "é🚀",
            "\u2028",
            "\x00\x7f",
        )
        for value in values:
            with self.subTest(value=value):
                raw = packet_format_v2_ab._v2_go_quote(value)
                self.assertEqual(packet_format_v2_ab._v2_parse_canonical_quote(raw), value)
        for raw in (
            r'"\u0061"',
            r'"\x61"',
            r'"\q"',
            r'"\ud800"',
            r'"\U00110000"',
            '"raw\ttab"',
        ):
            with self.subTest(raw=raw):
                with self.assertRaises(packet_format_v2_ab.CompactPacketError):
                    packet_format_v2_ab._v2_parse_canonical_quote(raw)

    def test_go_shortest_float_format_and_canonical_rejections(self) -> None:
        cases = (
            (0.0, "0"),
            (-0.0, "-0"),
            (1.0, "1"),
            (0.95, "0.95"),
            (100000.0, "100000"),
            (1000000.0, "1e+06"),
            (0.0001, "0.0001"),
            (0.00001, "1e-05"),
            (1.2345678901234567, "1.2345678901234567"),
            (1234567.8, "1.2345678e+06"),
            (108678236358137.625, "1.0867823635813762e+14"),
            (5e-324, "5e-324"),
            (2.2250738585072012e-308, "2.2250738585072014e-308"),
            (383260575764816448.0, "3.8326057576481645e+17"),
        )
        for value, expected in cases:
            with self.subTest(value=value):
                self.assertEqual(packet_format_v2_ab._v2_go_format_float(value), expected)
                packet_format_v2_ab._v2_parse_float(expected)
        for raw in (
            "1.0",
            "0.950",
            "1e+02",
            "1E+06",
            "1e6",
            "+1",
            "01",
            "NaN",
            "+Inf",
        ):
            with self.subTest(raw=raw):
                with self.assertRaises(packet_format_v2_ab.CompactPacketError):
                    packet_format_v2_ab._v2_parse_float(raw)

    def test_keyed_records_enforce_exact_schema_order_and_types(self) -> None:
        record, raw, _ = packet_format_v2_ab._v2_parse_keyed_record(
            'status freshness="ok" brain_schema=2 dirty=false changed_files=0'
        )
        self.assertEqual(record["tag"], "status")
        self.assertEqual(
            record["fields"],
            [["freshness", "ok"], ["brain_schema", 2], ["dirty", False], ["changed_files", 0]],
        )
        self.assertEqual(raw[-1], ("changed_files", "0"))
        invalid = (
            'status private="x"',
            'status dirty=false freshness="ok"',
            'status dirty=false dirty=true',
            "status dirty=0",
            'status brain_schema="2"',
            "status brain_schema=02",
            "status freshness=ok",
            r'status freshness="\u006f\u006b"',
        )
        for line in invalid:
            with self.subTest(line=line):
                with self.assertRaises(packet_format_v2_ab.CompactPacketError):
                    packet_format_v2_ab._v2_parse_keyed_record(line)

    def test_repeat_references_are_same_opcode_only_and_natural_text_is_never_hidden(self) -> None:
        schema = packet_format_v2_ab.COMPACT_V2_BY_TAG["history"]
        first, _, cells, _ = packet_format_v2_ab._v2_parse_positional_record(
            schema,
            'h\thistory/main.jsonl\t8\t"2026-07-01"\t12\t[validation,order]\t"first text"',
            None,
        )
        self.assertEqual(first["fields"][0], ["path", "history/main.jsonl"])
        second, _, _, _ = packet_format_v2_ab._v2_parse_positional_record(
            schema,
            'h\t^\t8\t^\t^\t^\t"second text"',
            cells,
        )
        self.assertEqual(second["fields"][0], ["path", "history/main.jsonl"])
        for line, prior in (
            ('h\t^\t8\t^\t^\t^\t"text"', None),
            ('h\thistory/main.jsonl\t8\t^\t^\t^\t"text"', cells),
            ('h\t^\t8\t^\t^\t^\t^', cells),
            ('h\t^\t8\t^\t^\t^\t"text"\t~', cells),
        ):
            with self.subTest(line=line):
                with self.assertRaises(packet_format_v2_ab.CompactPacketError):
                    packet_format_v2_ab._v2_parse_positional_record(schema, line, prior)

    def test_exact_family_budget_uses_strict_shorter_and_keyed_ties(self) -> None:
        schema = packet_format_v2_ab.COMPACT_V2_BY_TAG["freshness_axis"]
        tie = [[("name", '""')], [("name", '""')]]
        self.assertFalse(packet_format_v2_ab._v2_expected_positional(schema, tie))
        repeated = [[("name", '"worktree"'), ("state", '"dirty"')]] * 4
        self.assertTrue(packet_format_v2_ab._v2_expected_positional(schema, repeated))
        self.assertFalse(packet_format_v2_ab._v2_expected_positional(schema, repeated[:1]))


class PacketFormatV2GoldenAndAdversarialTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.golden = (
            REPO_ROOT / "internal/cli/testdata/brain_brief_compact_v2.golden"
        ).read_bytes()

    def test_exhaustive_golden_identity_projection_and_numeric_attribution(self) -> None:
        self.assertEqual(
            hashlib.sha256(self.golden).hexdigest(),
            "e0bc12b75059a7bba809ef1a66739c49d397852a30271da1b3e2ef4f9a6ca982",
        )
        parsed = packet_format_v2_ab.parse_compact_v2_packet(self.golden)
        legacy = packet_format_v2_ab.canonical_projection_from_legacy(
            _repeat_rich_legacy_packet()
        )
        self.assertEqual(parsed["records"], legacy)
        self.assertEqual(parsed["body_record_count"], 65)
        self.assertEqual(
            packet_format_v2_ab.sha256_bytes(
                packet_format_v2_ab.canonical_json_bytes(parsed["records"])
            ),
            "a934c2c5c12c96cff93f1c6e500a2cf61af1a985c40f7e57aad998961aba3a1c",
        )
        attribution = parsed["structural_size_attribution"]
        _assert_int_leaves(self, attribution)
        self.assertEqual(attribution["schema_version"], 1)
        self.assertEqual(sum(attribution["structural_partition"].values()), len(self.golden))
        self.assertEqual(
            attribution["structural_partition"]["legend_byte_count"],
            len(packet_format_v2_ab.COMPACT_V2_LEGEND.encode("utf-8")),
        )
        self.assertEqual(
            attribution["accounting_invariants"]["partition_delta_byte_count"], 0
        )
        self.assertEqual(
            attribution["per_tag"]["runtime_trace"]["keyed_record_count"], 2
        )
        self.assertEqual(
            attribution["per_tag"]["runtime_trace"]["positional_record_count"], 0
        )
        self.assertEqual(
            attribution["per_tag"]["runtime_trace_evidence"][
                "positional_record_count"
            ],
            2,
        )

    def test_hashed_exact_legend_and_footer_integrity_fail_closed(self) -> None:
        changed_legend = _replace_line(
            self.golden,
            b"legend ",
            b"legend ~=missing ^=previous_physical_row",
        )
        changed_marker = _replace_line(
            self.golden,
            b"entire.brain_brief ",
            b"entire.brain_brief compact_v3",
        )
        wrong_count = self.golden.replace(b"end\t65\t", b"end\t64\t", 1)
        uppercase_hash = self.golden[:-65] + self.golden[-65:].upper()
        tampered_body = self.golden.replace(
            b"Decision: preserve validation order.",
            b"Decision: reverse validation order.",
            1,
        )
        cases = (
            changed_legend,
            changed_marker,
            wrong_count,
            uppercase_hash,
            tampered_body,
            self.golden[:-1],
            self.golden[: self.golden.rfind(b"\nend\t") + 1],
            b"\xff" + self.golden[1:],
        )
        for candidate in cases:
            with self.subTest(candidate_sha256=hashlib.sha256(candidate).hexdigest()):
                with self.assertRaises(packet_format_v2_ab.CompactPacketError):
                    packet_format_v2_ab.parse_compact_v2_packet(candidate)

    def test_declarations_are_exact_unique_and_immediately_used(self) -> None:
        history_declaration = next(
            line.rstrip(b"\n")
            for line in self.golden.splitlines(keepends=True)
            if line.startswith(b"@h=")
        )
        cases = (
            _insert_before_line(self.golden, b"@h=", history_declaration),
            _replace_line(
                self.golden,
                b"@h=",
                b"@h=history(line,path,timestamp,score,matched_terms,excerpt)",
            ),
            _replace_line(
                self.golden,
                b"@h=",
                b"@?=history(path,line,timestamp,score,matched_terms,excerpt)",
            ),
            _remove_line(self.golden, b"@h="),
            _insert_before_line(self.golden, b"h\t", b"task value=\"interposed\""),
        )
        for candidate in cases:
            with self.subTest(candidate_sha256=hashlib.sha256(candidate).hexdigest()):
                with self.assertRaises(packet_format_v2_ab.CompactPacketError):
                    packet_format_v2_ab.parse_compact_v2_packet(candidate)

    def test_keyed_rows_reject_unknown_reordered_duplicate_and_wrong_typed_fields(self) -> None:
        line = next(
            row.rstrip(b"\n")
            for row in self.golden.splitlines(keepends=True)
            if row.startswith(b"runtime_trace ")
        )
        cases = (
            line + b' private="PRIVATE_KEYED_SENTINEL"',
            line.replace(b"index=0 id=", b"id=", 1) + b" index=0",
            line.replace(b' id="runtime:1"', b' id="runtime:1" id="runtime:1"', 1),
            line.replace(b"index=0", b'index="0"', 1),
            line.replace(b'id="runtime:1"', b"id=runtime:1", 1),
        )
        for replacement in cases:
            candidate = _replace_nth_line(
                self.golden, b"runtime_trace ", 0, replacement
            )
            with self.subTest(candidate_sha256=hashlib.sha256(candidate).hexdigest()):
                with self.assertRaises(packet_format_v2_ab.CompactPacketError) as raised:
                    packet_format_v2_ab.parse_compact_v2_packet(candidate)
                self.assertNotIn("PRIVATE_KEYED_SENTINEL", str(raised.exception))

    def test_positional_rows_reject_bad_references_values_width_and_trailing_omission(self) -> None:
        first = next(
            row.rstrip(b"\n")
            for row in self.golden.splitlines(keepends=True)
            if row.startswith(b"h\t")
        )
        second = [
            row.rstrip(b"\n")
            for row in self.golden.splitlines(keepends=True)
            if row.startswith(b"h\t")
        ][1]
        mutations = (
            (0, first.replace(b"h\thistory/main.jsonl", b"h\t^", 1)),
            (1, second.replace(b"h\t^", b"h\thistory/main.jsonl", 1)),
            (1, second.rsplit(b"\t", 1)[0] + b"\t^"),
            (0, first.replace(b"h\thistory/main.jsonl", b'h\t"history/main.jsonl"', 1)),
            (0, first.replace(b"\t8\t", b"\t08\t", 1)),
            (0, first.replace(b"[validation,order]", b"[validation,]", 1)),
            (0, first + b"\textra"),
            (0, first.rsplit(b"\t", 1)[0] + b"\t~"),
        )
        for occurrence, replacement in mutations:
            candidate = _replace_nth_line(self.golden, b"h\t", occurrence, replacement)
            with self.subTest(candidate_sha256=hashlib.sha256(candidate).hexdigest()):
                with self.assertRaises(packet_format_v2_ab.CompactPacketError):
                    packet_format_v2_ab.parse_compact_v2_packet(candidate)
        loose_float = _resign(self.golden.replace(b"\t0.88\t", b"\t0.880\t", 1))
        with self.assertRaises(packet_format_v2_ab.CompactPacketError):
            packet_format_v2_ab.parse_compact_v2_packet(loose_float)

    def test_family_budget_rejects_valid_but_nonminimal_keyed_and_positional_forms(self) -> None:
        # History is cheaper positionally in the frozen golden. Replace its
        # declaration/rows with individually valid keyed rows; semantic parity
        # is unchanged, but the exact family budget must reject the packet.
        lines = self.golden.splitlines(keepends=True)
        keyed_history = (
            b'history path="history/main.jsonl" line=8 timestamp="2026-07-01" '
            b'score=12 matched_terms=["validation","order"] '
            b'excerpt="Decision: preserve validation order."\n'
        )
        rewritten: list[bytes] = []
        for row in lines:
            if row.startswith(b"@h="):
                continue
            if row.startswith(b"h\t"):
                rewritten.append(keyed_history)
            else:
                rewritten.append(row)
        nonminimal_keyed = _resign(b"".join(rewritten))
        with self.assertRaises(packet_format_v2_ab.CompactPacketError):
            packet_format_v2_ab.parse_compact_v2_packet(nonminimal_keyed)

        # Sparse runtime traces are cheaper keyed. The old valid positional
        # representation is rejected because declaration plus rows is larger.
        positional = _insert_before_line(
            self.golden,
            b"runtime_trace ",
            b"@x=runtime_trace(index,id,type,from,to,file,start,end,path,scope,resolution,target_kind,confidence,reason,warning_codes)",
        )
        first_row = (
            b'x\t0\truntime:1\tRUNTIME_TRACE\tsym:validate\tsym:persist'
            b'\t~\t~\t~\t~\t~\t~\t~\t~\t"observed edge"'
        )
        second_row = (
            b'x\t1\t^\t^\t^\t^\t~\t~\t~\t~\t~\t~\t~\t~\t"observed edge"'
        )
        positional = _replace_nth_line(positional, b"runtime_trace ", 0, first_row)
        positional = _replace_nth_line(positional, b"runtime_trace ", 0, second_row)
        with self.assertRaises(packet_format_v2_ab.CompactPacketError):
            packet_format_v2_ab.parse_compact_v2_packet(positional)

    def test_same_opcode_reference_survives_interleaved_physical_evidence_row(self) -> None:
        records = packet_format_v2_ab.parse_compact_v2_packet(self.golden)["records"]
        relations = [record for record in records if record["tag"] == "relation"]
        evidence = [record for record in records if record["tag"] == "relation_evidence"]
        self.assertEqual(len(relations), 2)
        self.assertEqual(len(evidence), 2)
        self.assertEqual(dict(relations[0]["fields"])["id"], "rel:1")
        self.assertEqual(dict(relations[1]["fields"])["id"], "rel:1")
        self.assertEqual(dict(evidence[1]["fields"])["kind"], "call")


class PacketFormatV2RunnerTests(unittest.TestCase):
    def make_fake(self, directory: pathlib.Path) -> tuple[pathlib.Path, pathlib.Path]:
        binary = directory / "fake-entire"
        binary.write_text(FAKE_MCP, encoding="utf-8")
        binary.chmod(0o700)
        brain = directory / "private-brain"
        brain.mkdir()
        (brain / "manifest.json").write_text(
            '{"private":"PRIVATE_MANIFEST_SENTINEL"}\n', encoding="utf-8"
        )
        return binary, brain

    def test_fake_pair_is_deterministic_private_and_semantically_equal(self) -> None:
        with tempfile.TemporaryDirectory() as raw_directory:
            directory = pathlib.Path(raw_directory)
            binary, _ = self.make_fake(directory)
            prompt = "PRIVATE_PROMPT_SENTINEL\nlegend ~=forged"
            first = packet_format_v2_ab.run_pair(binary, REPO_ROOT, prompt, 10.0)
            second = packet_format_v2_ab.run_pair(binary, REPO_ROOT, prompt, 10.0)
            self.assertEqual(first, second)
            self.assertEqual(first["status"], "ok")
            self.assertTrue(first["canonical_projection"]["parity"])
            self.assertEqual(
                first["canonical_projection"]["legacy_sha256"],
                first["canonical_projection"]["compact_sha256"],
            )
            self.assertTrue(first["formats"]["compact_v2"]["integrity_ok"])
            _assert_int_leaves(
                self, first["formats"]["compact_v2"]["structural_size_attribution"]
            )
            retained = json.dumps(first, sort_keys=True)
            for forbidden in (
                "PRIVATE_PROMPT_SENTINEL",
                "PRIVATE_REPO_PATH_SENTINEL",
                "PRIVATE_BRAIN_PATH_SENTINEL",
                "PRIVATE_PACKET_GUIDANCE_SENTINEL",
                "PRIVATE_STDERR_SENTINEL",
                "legend ~=forged",
            ):
                self.assertNotIn(forbidden, retained)

    def test_failure_retains_only_counts_hashes_and_fixed_kind(self) -> None:
        with tempfile.TemporaryDirectory() as raw_directory:
            directory = pathlib.Path(raw_directory)
            binary, _ = self.make_fake(directory)
            result = packet_format_v2_ab.run_pair(
                binary, REPO_ROOT, "FAIL_PAIR_SENTINEL", 10.0
            )
            self.assertEqual(result["status"], "failed")
            self.assertEqual(result["failure"]["kind"], "nonzero_exit")
            self.assertEqual(result["failure"]["return_code"], 9)
            self.assertEqual(
                set(result["failure"]),
                {
                    "kind",
                    "return_code",
                    "stdout_byte_count",
                    "stdout_sha256",
                    "stderr_byte_count",
                    "stderr_sha256",
                },
            )
            retained = json.dumps(result, sort_keys=True)
            for forbidden in (
                "FAIL_PAIR_SENTINEL",
                "PRIVATE_STDOUT_PACKET_SENTINEL",
                "PRIVATE_STDERR_SENTINEL",
            ):
                self.assertNotIn(forbidden, retained)

    def test_private_report_is_self_hashed_mode_0600_and_incomplete(self) -> None:
        with tempfile.TemporaryDirectory() as raw_directory:
            directory = pathlib.Path(raw_directory)
            binary, brain = self.make_fake(directory)
            prompt = "PRIVATE_REPORT_PROMPT_SENTINEL"
            task = {
                "logical_repo": "entire-brain",
                "task_sha256": _sha("PRIVATE_TASK_IDENTITY_SENTINEL"),
                "prompt_sha256": _sha(prompt),
                "prompt": prompt,
            }
            corpus = {
                "schema_version": 1,
                "corpus_sha256": _sha("PRIVATE_CORPUS_SENTINEL"),
                "tasks": [
                    {
                        "logical_repo": task["logical_repo"],
                        "task_sha256": task["task_sha256"],
                        "prompt_sha256": task["prompt_sha256"],
                    }
                ],
            }
            repos = {
                name: REPO_ROOT
                for name in packet_format_v2_ab.profile_brief.LOGICAL_REPOS
            }
            first = packet_format_v2_ab.build_report(corpus, [task], binary, repos, 10.0)
            second = packet_format_v2_ab.build_report(corpus, [task], binary, repos, 10.0)
            self.assertEqual(first, second)
            self.assertEqual(first["schema_version"], 2)
            self.assertTrue(packet_format_v2_ab.verify_self_hash(first, "report_sha256"))
            self.assertTrue(
                packet_format_v2_ab.verify_runner_identity(first["runner_identity"])
            )
            self.assertFalse(first["confirmatory_eligible"])
            self.assertFalse(first["quality_eligible"])
            self.assertFalse(first["paid_or_provider_calls"])
            self.assertFalse(
                first["promotion_gate"][
                    "eligible_for_separately_authorized_agent_quality_trial"
                ]
            )
            attribution = first["aggregate"]["compact_v2"][
                "structural_size_attribution"
            ]
            _assert_int_leaves(self, attribution)
            self.assertEqual(attribution["packet_count"], 1)
            self.assertEqual(
                attribution["accounting_invariants"]["exact_inner_byte_count"],
                first["aggregate"]["compact_v2"]["inner_byte_count_sum"],
            )
            output = directory / "private-report.json"
            packet_format_v2_ab.write_private_json(output, first)
            self.assertEqual(stat.S_IMODE(output.stat().st_mode), 0o600)
            retained = output.read_text(encoding="utf-8")
            for forbidden in (
                prompt,
                str(REPO_ROOT),
                str(brain),
                "PRIVATE_REPO_PATH_SENTINEL",
                "PRIVATE_BRAIN_PATH_SENTINEL",
                "PRIVATE_PACKET_GUIDANCE_SENTINEL",
                "PRIVATE_PACKET_TIME_SENTINEL",
                "PRIVATE_MANIFEST_SENTINEL",
                "PRIVATE_STDOUT_PACKET_SENTINEL",
                "PRIVATE_STDERR_SENTINEL",
            ):
                self.assertNotIn(forbidden, retained)


if __name__ == "__main__":
    unittest.main()
