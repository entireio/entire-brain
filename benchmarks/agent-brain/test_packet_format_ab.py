#!/usr/bin/env python3
"""Tests for the unpaid, privacy-safe brain_brief packet-format A/B runner."""

from __future__ import annotations

import collections
import copy
import hashlib
import importlib.util
import json
import os
import pathlib
import random
import stat
import sys
import tempfile
import unittest


HERE = pathlib.Path(__file__).resolve().parent
REPO_ROOT = HERE.parents[1]
if str(HERE) not in sys.path:
    sys.path.insert(0, str(HERE))
SPEC = importlib.util.spec_from_file_location("packet_format_ab", HERE / "packet_format_ab.py")
assert SPEC is not None and SPEC.loader is not None
packet_format_ab = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = packet_format_ab
SPEC.loader.exec_module(packet_format_ab)


def _sha(value: str) -> str:
    return hashlib.sha256(value.encode("utf-8")).hexdigest()


def _minimal_compact_packet(body_line: str) -> bytes:
    body = (packet_format_ab.COMPACT_MARKER + "\n" + body_line + "\n").encode("utf-8")
    end = (
        "end symbols=0 relations=0 neighbors=0 runtime_traces=0 test_roots=0 "
        "test_suggestions=0 history=0 facts=0 actions=0 patterns=0 consolidations=0 "
        "themes=0 guidance=0 warnings=0 body_records=1 body_sha256=\"sha256:"
        + hashlib.sha256(body).hexdigest()
        + "\"\n"
    ).encode("utf-8")
    return body + end


def _assert_int_leaves(test: unittest.TestCase, value: object) -> None:
    if isinstance(value, dict):
        for child in value.values():
            _assert_int_leaves(test, child)
        return
    test.assertIs(type(value), int)


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
        header = data[cursor:end]
        length = int(header.split(b": ", 1)[1])
        start = end + 4
        stop = start + length
        rows.append(json.loads(data[start:stop]))
        cursor = stop
    return rows


def compact(prompt):
    numeric_pattern = "LOOSE_NUMBER_PAIR" in prompt
    extra = (
        'pattern id="numeric" type="test" title="Numeric parity" strength=1 support=1\n'
        if numeric_pattern else ""
    )
    body = (
        "entire.brain_brief compact_v1\n"
        + "task value=" + json.dumps(prompt, ensure_ascii=False) + "\n"
        + 'status repo_key="fixture" brain_schema=1 dirty=false changed_files=0\n'
        + "sources seed=false sessions=false semantic=false history=false facts=false\n"
        + "live\n"
        + 'guidance text="PRIVATE_PACKET_GUIDANCE_SENTINEL"\n'
        + extra
    )
    digest = hashlib.sha256(body.encode("utf-8")).hexdigest()
    pattern_count = 1 if numeric_pattern else 0
    body_records = 6 if numeric_pattern else 5
    return body + (
        'end symbols=0 relations=0 neighbors=0 runtime_traces=0 test_roots=0 '
        'test_suggestions=0 history=0 facts=0 actions=0 patterns=' + str(pattern_count) + ' consolidations=0 '
        'themes=0 guidance=1 warnings=0 body_records=' + str(body_records) + ' body_sha256="sha256:' + digest + '"\n'
    )


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
        "private_padding_ignored_by_compact_projection": "P" * 512,
    }
    if "LOOSE_NUMBER_PAIR" in prompt:
        value["patterns"] = [
            {
                "id": "numeric",
                "type": "test",
                "title": "Numeric parity",
                "strength": 1.0,
                "support": 1,
            }
        ]
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


class PacketFormatABTests(unittest.TestCase):
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

    def test_checked_corpus_schedule_is_exact_same_114_query_order(self) -> None:
        corpus = packet_format_ab.profile_brief.load_verified_corpus(
            packet_format_ab.DEFAULT_CORPUS, REPO_ROOT
        )
        tasks = packet_format_ab.profile_brief.load_verified_prompts(corpus, REPO_ROOT)
        schedule = packet_format_ab.build_schedule(tasks)
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
            self.assertEqual(entry["format_order"], ["legacy_json", "compact_v1"])

    def test_frozen_token_proxy_and_definition_hash(self) -> None:
        self.assertEqual(packet_format_ab.offline_token_proxy_count(b""), 0)
        self.assertEqual(packet_format_ab.offline_token_proxy_count(b"a"), 1)
        self.assertEqual(packet_format_ab.offline_token_proxy_count(b"abcd"), 1)
        self.assertEqual(packet_format_ab.offline_token_proxy_count(b"abcde"), 2)
        self.assertEqual(
            packet_format_ab.TOKEN_PROXY_DEFINITION_SHA256,
            hashlib.sha256(packet_format_ab.TOKEN_PROXY_DEFINITION.encode("ascii")).hexdigest(),
        )

    def test_independent_legacy_projection_matches_exhaustive_go_golden(self) -> None:
        golden = (REPO_ROOT / "internal/cli/testdata/brain_brief_compact_v1.golden").read_bytes()
        compact = packet_format_ab.parse_compact_packet(golden)
        legacy_projection = packet_format_ab.canonical_projection_from_legacy(
            _rich_legacy_packet()
        )
        self.assertEqual(legacy_projection, compact["records"])
        self.assertEqual(compact["body_record_count"], 35)

    def test_structural_attribution_partitions_every_golden_byte(self) -> None:
        golden = (REPO_ROOT / "internal/cli/testdata/brain_brief_compact_v1.golden").read_bytes()
        attribution = packet_format_ab.parse_compact_packet(golden)[
            "structural_size_attribution"
        ]
        _assert_int_leaves(self, attribution)
        self.assertEqual(attribution["schema_version"], 2)
        partition = attribution["structural_partition"]
        self.assertEqual(sum(partition.values()), len(golden))
        invariants = attribution["accounting_invariants"]
        self.assertEqual(invariants["exact_inner_byte_count"], len(golden))
        self.assertEqual(invariants["structural_partition_delta_byte_count"], 0)
        self.assertEqual(invariants["per_tag_encoded_delta_byte_count"], 0)
        self.assertEqual(invariants["per_tag_record_count_delta"], 0)
        self.assertEqual(invariants["per_tag_field_count_delta"], 0)
        self.assertEqual(set(attribution["per_tag"]), set(packet_format_ab.COMPACT_BODY_TAG_ORDER))
        for metrics in attribution["per_tag"].values():
            self.assertEqual(
                metrics["encoded_byte_count"],
                sum(metrics[field] for field in packet_format_ab.RECORD_SIZE_PARTITION_FIELDS),
            )

    def test_go_quoted_source_byte_metrics_are_exact_for_hex_unicode_and_literals(self) -> None:
        cases = (
            (r'"\xff"', 1, 3),
            (r'"\xc3\xa9"', 2, 6),
            (r'"\x00"', 1, 3),
            (r'"\u0080"', 2, 4),
            (r'"\u20ac"', 3, 3),
            (r'"\U0001f680"', 4, 6),
            ('"é🚀"', 6, 0),
        )
        for raw, expected_payload, expected_expansion in cases:
            with self.subTest(raw=raw):
                self.assertEqual(
                    packet_format_ab._go_quoted_decoded_byte_count(raw), expected_payload
                )
                _, size = packet_format_ab._parse_compact_value_sized(raw)
                self.assertEqual(size["scalar_value_payload_byte_count"], expected_payload)
                self.assertEqual(size["escape_overhead_byte_count"], expected_expansion)
                self.assertEqual(
                    len(raw.encode("utf-8")),
                    expected_payload
                    + size["quoting_delimiter_byte_count"]
                    + expected_expansion,
                )

        array_raw = "[" + ",".join(raw for raw, _, _ in cases) + "]"
        _, array_size = packet_format_ab._parse_compact_value_sized(array_raw)
        self.assertEqual(
            array_size["array_value_payload_byte_count"],
            sum(payload for _, payload, _ in cases),
        )
        self.assertEqual(
            array_size["escape_overhead_byte_count"],
            sum(expansion for _, _, expansion in cases),
        )
        self.assertEqual(array_size["array_item_count"], len(cases))
        self.assertEqual(array_size["separator_byte_count"], len(cases) - 1)
        self.assertEqual(array_size["quoting_delimiter_byte_count"], 2 + 2 * len(cases))
        self.assertEqual(
            len(array_raw.encode("utf-8")),
            sum(
                array_size[field]
                for field in (
                    "array_value_payload_byte_count",
                    "separator_byte_count",
                    "quoting_delimiter_byte_count",
                    "escape_overhead_byte_count",
                )
            ),
        )

        for invalid in (r'"\ud800"', r'"\udfff"', r'"\U00110000"'):
            with self.subTest(invalid=invalid):
                with self.assertRaises(packet_format_ab.CompactPacketError):
                    packet_format_ab._parse_compact_value_sized(invalid)
                with self.assertRaises(packet_format_ab.CompactPacketError):
                    packet_format_ab._parse_compact_value_sized("[" + invalid + "]")

        for source_byte in range(256):
            raw = f'"\\x{source_byte:02x}"'
            _, size = packet_format_ab._parse_compact_value_sized(raw)
            self.assertEqual(size["scalar_value_payload_byte_count"], 1, source_byte)
            self.assertEqual(size["escape_overhead_byte_count"], 3, source_byte)

    def test_go_quoted_size_fuzz_conserves_scalar_and_array_bytes(self) -> None:
        rng = random.Random(0xC0DEC0DE)
        atoms = (
            ("a", 1),
            ("é", 2),
            ("€", 3),
            ("🚀", 4),
            (r"\a", 1),
            (r"\n", 1),
            (r"\t", 1),
            (r'\"', 1),
            (r"\\", 1),
            (r"\x00", 1),
            (r"\x80", 1),
            (r"\xff", 1),
            (r"\u0080", 2),
            (r"\u20ac", 3),
            (r"\U0001f680", 4),
        )

        for iteration in range(512):
            items: list[tuple[str, int]] = []
            for _ in range(rng.randrange(7)):
                selected = [rng.choice(atoms) for _ in range(rng.randrange(9))]
                raw = '"' + "".join(atom for atom, _ in selected) + '"'
                expected_payload = sum(width for _, width in selected)
                self.assertEqual(
                    packet_format_ab._go_quoted_decoded_byte_count(raw), expected_payload
                )
                _, scalar_size = packet_format_ab._parse_compact_value_sized(raw)
                self.assertEqual(
                    len(raw.encode("utf-8")),
                    scalar_size["scalar_value_payload_byte_count"]
                    + scalar_size["quoting_delimiter_byte_count"]
                    + scalar_size["escape_overhead_byte_count"],
                )
                items.append((raw, expected_payload))

            array_raw = "[" + ",".join(raw for raw, _ in items) + "]"
            _, array_size = packet_format_ab._parse_compact_value_sized(array_raw)
            self.assertEqual(
                array_size["array_value_payload_byte_count"],
                sum(payload for _, payload in items),
                iteration,
            )
            self.assertEqual(
                len(array_raw.encode("utf-8")),
                sum(
                    array_size[field]
                    for field in (
                        "array_value_payload_byte_count",
                        "separator_byte_count",
                        "quoting_delimiter_byte_count",
                        "escape_overhead_byte_count",
                    )
                ),
                iteration,
            )

    def test_structural_attribution_retains_no_adversarial_field_or_value_text(self) -> None:
        packet = _minimal_compact_packet(
            r'task private_field_sentinel="PRIVATE_SCALAR_SENTINEL\n\t\"é🚀" '
            r'aliases=["PRIVATE_ARRAY_SENTINEL","x\\y"] count=-12 ratio=1.25 enabled=true'
        )
        attribution = packet_format_ab.parse_compact_packet(packet)[
            "structural_size_attribution"
        ]
        _assert_int_leaves(self, attribution)
        retained = json.dumps(attribution, sort_keys=True)
        for forbidden in (
            "private_field_sentinel",
            "PRIVATE_SCALAR_SENTINEL",
            "PRIVATE_ARRAY_SENTINEL",
            "é",
            "🚀",
            "x\\\\y",
        ):
            self.assertNotIn(forbidden, retained)
        task = attribution["per_tag"]["task"]
        self.assertEqual(task["record_count"], 1)
        self.assertEqual(task["field_count"], 5)
        self.assertEqual(task["array_field_count"], 1)
        self.assertEqual(task["array_item_count"], 2)
        self.assertGreater(task["escape_overhead_byte_count"], 0)
        self.assertEqual(
            attribution["accounting_invariants"]["exact_inner_byte_count"], len(packet)
        )

    def test_unknown_body_tag_cannot_become_a_retained_attribution_key(self) -> None:
        packet = _minimal_compact_packet(
            'private_tag_sentinel value="PRIVATE_UNKNOWN_TAG_VALUE"'
        )
        with self.assertRaises(packet_format_ab.CompactPacketError) as raised:
            packet_format_ab.parse_compact_packet(packet)
        retained_error = str(raised.exception)
        self.assertNotIn("private_tag_sentinel", retained_error)
        self.assertNotIn("PRIVATE_UNKNOWN_TAG_VALUE", retained_error)

    def test_compact_integrity_rejects_tamper_and_truncation(self) -> None:
        golden = (REPO_ROOT / "internal/cli/testdata/brain_brief_compact_v1.golden").read_bytes()
        tampered = golden.replace(
            b"Validation runs before persistence.", b"Persistence runs before validation."
        )
        with self.assertRaises(packet_format_ab.CompactIntegrityError):
            packet_format_ab.parse_compact_packet(tampered)
        with self.assertRaises(packet_format_ab.CompactIntegrityError):
            packet_format_ab.parse_compact_packet(golden[: golden.rfind(b"\nend ") + 1])

    def test_compact_parser_enforces_go_quotes_arrays_and_typed_end_counts(self) -> None:
        valid = packet_format_ab._parse_compact_record(
            r'row value="a\t\u2028\U0001f600" values=["x","\x09"]'
        )
        self.assertEqual(
            valid["fields"],
            [["value", "a\t\u2028\U0001f600"], ["values", ["x", "\t"]]],
        )
        for invalid in (
            r"row values=['x']",
            r'row values=["x",]',
            r'row values=["x" "y"]',
            r'row value="\N{SNOWMAN}"',
            r'row value="\q"',
            'row value="raw\ttab"',
        ):
            with self.subTest(invalid=invalid):
                with self.assertRaises(packet_format_ab.CompactPacketError):
                    packet_format_ab._parse_compact_record(invalid)

        golden = (REPO_ROOT / "internal/cli/testdata/brain_brief_compact_v1.golden").read_bytes()
        for typed_bool in (
            golden.replace(b"symbols=1", b"symbols=true", 1),
            golden.replace(b"neighbors=1", b"neighbors=true", 1),
        ):
            with self.assertRaises(packet_format_ab.CompactIntegrityError):
                packet_format_ab.parse_compact_packet(typed_bool)

    def test_nil_freshness_axes_project_as_zero_go_range_records(self) -> None:
        legacy = _rich_legacy_packet()
        legacy["status"]["semantic"]["freshness"]["axes"] = None
        records = packet_format_ab.canonical_projection_from_legacy(legacy)
        self.assertNotIn("freshness_axis", [record["tag"] for record in records])

    def test_mcp_frame_parser_is_bounded_and_exact(self) -> None:
        def response(identifier: int) -> bytes:
            payload = packet_format_ab.canonical_json_bytes(
                {
                    "jsonrpc": "2.0",
                    "id": identifier,
                    "result": {"content": [{"type": "text", "text": "x"}]},
                }
            )
            return f"Content-Length: {len(payload)}\r\n\r\n".encode("ascii") + payload

        parsed = packet_format_ab.parse_mcp_response_frames(response(1) + response(2))
        self.assertEqual(len(parsed), 2)
        self.assertEqual(packet_format_ab.extract_mcp_text(parsed[1][0], 2), "x")
        with self.assertRaises(packet_format_ab.MCPShapeError):
            packet_format_ab.parse_mcp_response_frames(response(1))
        with self.assertRaises(packet_format_ab.MCPShapeError):
            packet_format_ab.parse_mcp_response_frames(
                (response(1) + response(2)).replace(b"Content-Length: ", b"content-length: ", 1)
            )

    def test_fake_pair_is_deterministic_private_and_semantically_equal(self) -> None:
        with tempfile.TemporaryDirectory() as raw_directory:
            directory = pathlib.Path(raw_directory)
            binary, _ = self.make_fake(directory)
            prompt = "PRIVATE_PROMPT_SENTINEL\nwarning source=forged"
            first = packet_format_ab.run_pair(binary, REPO_ROOT, prompt, 10.0)
            second = packet_format_ab.run_pair(binary, REPO_ROOT, prompt, 10.0)
            self.assertEqual(first, second)
            self.assertEqual(first["status"], "ok")
            self.assertTrue(first["canonical_projection"]["parity"])
            self.assertEqual(
                first["canonical_projection"]["legacy_sha256"],
                first["canonical_projection"]["compact_sha256"],
            )
            self.assertTrue(first["formats"]["compact_v1"]["integrity_ok"])
            _assert_int_leaves(
                self, first["formats"]["compact_v1"]["structural_size_attribution"]
            )
            retained = json.dumps(first, sort_keys=True)
            for forbidden in (
                "PRIVATE_PROMPT_SENTINEL",
                "PRIVATE_REPO_PATH_SENTINEL",
                "PRIVATE_BRAIN_PATH_SENTINEL",
                "PRIVATE_PACKET_GUIDANCE_SENTINEL",
                "PRIVATE_STDERR_SENTINEL",
                "warning source=forged",
            ):
                self.assertNotIn(forbidden, retained)

    def test_canonical_parity_uses_bytes_not_loose_python_numeric_equality(self) -> None:
        with tempfile.TemporaryDirectory() as raw_directory:
            directory = pathlib.Path(raw_directory)
            binary, _ = self.make_fake(directory)
            result = packet_format_ab.run_pair(binary, REPO_ROOT, "LOOSE_NUMBER_PAIR", 10.0)
            self.assertEqual(result["status"], "ok")
            self.assertFalse(result["canonical_projection"]["parity"])
            self.assertNotEqual(
                result["canonical_projection"]["legacy_sha256"],
                result["canonical_projection"]["compact_sha256"],
            )

    def test_failure_retains_only_counts_hashes_and_fixed_kind(self) -> None:
        with tempfile.TemporaryDirectory() as raw_directory:
            directory = pathlib.Path(raw_directory)
            binary, _ = self.make_fake(directory)
            result = packet_format_ab.run_pair(binary, REPO_ROOT, "FAIL_PAIR_SENTINEL", 10.0)
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

    def test_private_report_is_deterministic_self_hashed_mode_0600_and_incomplete(self) -> None:
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
            repos = {name: REPO_ROOT for name in packet_format_ab.profile_brief.LOGICAL_REPOS}
            first = packet_format_ab.build_report(corpus, [task], binary, repos, 10.0)
            second = packet_format_ab.build_report(corpus, [task], binary, repos, 10.0)
            self.assertEqual(first, second)
            self.assertEqual(first["schema_version"], 2)
            self.assertTrue(packet_format_ab.verify_self_hash(first, "report_sha256"))
            self.assertEqual(
                first["offline_token_proxy"]["definition"],
                packet_format_ab.TOKEN_PROXY_DEFINITION,
            )
            self.assertFalse(
                first["promotion_gate"]["eligible_for_separately_authorized_agent_quality_trial"]
            )
            aggregate_attribution = first["aggregate"]["compact_v1"][
                "structural_size_attribution"
            ]
            _assert_int_leaves(self, aggregate_attribution)
            self.assertEqual(aggregate_attribution["packet_count"], 1)
            self.assertEqual(
                aggregate_attribution["accounting_invariants"]["exact_inner_byte_count"],
                first["aggregate"]["compact_v1"]["inner_byte_count_sum"],
            )
            output = directory / "private-report.json"
            packet_format_ab.write_private_json(output, first)
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

    def test_promotion_thresholds_are_exact_and_do_not_authorize_paid_or_default(self) -> None:
        aggregate = {
            "scheduled_pair_count": 114,
            "successful_pair_count": 114,
            "legacy_json": {"parse_pass_count": 114},
            "compact_v1": {"parse_pass_count": 114},
            "compact_integrity_pass_count": 114,
            "canonical_parity_count": 114,
            "inner_byte_reduction_ppm": {
                "minimum": 0,
                "lower_median": packet_format_ab.INNER_BYTE_REDUCTION_GATE_PPM,
            },
            "token_proxy_reduction_ppm": {
                "lower_median": packet_format_ab.TOKEN_PROXY_REDUCTION_GATE_PPM,
            },
        }
        gate = packet_format_ab._promotion_gate(True, aggregate)
        self.assertTrue(gate["eligible_for_separately_authorized_agent_quality_trial"])
        self.assertFalse(gate["paid_agent_quality_trial_authorized"])
        self.assertFalse(gate["change_mcp_or_cli_default_authorized"])
        below = copy.deepcopy(aggregate)
        below["inner_byte_reduction_ppm"]["lower_median"] -= 1
        self.assertFalse(
            packet_format_ab._promotion_gate(True, below)[
                "eligible_for_separately_authorized_agent_quality_trial"
            ]
        )

    def test_report_rejects_nonprefix_task_selection(self) -> None:
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
        with self.assertRaises(packet_format_ab.PacketABError):
            packet_format_ab.build_report(corpus, [task], pathlib.Path("missing"), {}, 1.0)


if __name__ == "__main__":
    unittest.main()
