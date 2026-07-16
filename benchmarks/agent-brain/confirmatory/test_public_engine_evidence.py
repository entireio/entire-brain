#!/usr/bin/env python3
"""Privacy, mutation, and integration tests for public engine evidence v4."""

from __future__ import annotations

import copy
import json
import pathlib
import struct
import tempfile
import unittest
from unittest import mock

import check_protocol as CHECK
import public_engine_evidence as PUBLIC


HERE = pathlib.Path(__file__).resolve().parent


def write_json(path: pathlib.Path, value: object) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(PUBLIC.canonical_json_bytes(value) + b"\n")


def write_vectors(path: pathlib.Path, model_id: str, dimension: int, candidate_ids: list[str]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    payload = bytearray(b"EBV1")
    encoded_model = model_id.encode("utf-8")
    payload.extend(struct.pack("<H", len(encoded_model)))
    payload.extend(encoded_model)
    payload.extend(struct.pack("<I", dimension))
    payload.extend(struct.pack("<I", len(candidate_ids)))
    for candidate_id in candidate_ids:
        encoded_id = candidate_id.encode("utf-8")
        payload.extend(struct.pack("<H", len(encoded_id)))
        payload.extend(encoded_id)
        payload.extend(struct.pack(f"<{dimension}f", *([0.25] * dimension)))
    path.write_bytes(payload)


class PublicEngineEvidenceTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = pathlib.Path(self.temp.name)
        self.private = self.root / "restricted"
        self.private.mkdir()
        self.output = self.root / "public-v4"
        self.facts = self.private / "sources/facts.ndjson"
        self.sessions = self.private / "sources/session_dates.json"
        self.facts.parent.mkdir(parents=True)
        self.old_session = "2026-01-05-0bb9a2d9-a51e-4d5a-8275-566c766a0ff2"
        self.excluded_session = "019f1893-b810-7992-afb4-8c4bddc4ae3c"
        self.after_session = "2026-07-05-569aed7b-6ead-4790-a3fe-c7028466112c"
        self.unknown_session = "2026-01-08-ccc58457-00a5-4a2f-82a9-175e4473c60b"
        facts = [
            {
                "id": "fact:a", "status": "active", "text": "private answer alpha",
                "provenance": [{"session_id": self.old_session}],
            },
            {
                "id": "fact:h", "status": "active", "text": "private answer eta",
                "provenance": [{"session_id": self.old_session}],
            },
            {
                "id": "fact:b", "status": "superseded", "text": "private answer beta",
                "provenance": [{"session_id": self.old_session}],
            },
            {
                "id": "fact:g", "status": "retracted", "text": "private answer gamma",
                "provenance": [{"session_id": self.old_session}],
            },
            {"id": "fact:c", "status": "active", "text": "private empty", "provenance": []},
            {
                "id": "fact:d", "status": "active", "text": "private excluded",
                "provenance": [{"session_id": self.excluded_session}],
            },
            {
                "id": "fact:e", "status": "active", "text": "private unknown",
                "provenance": [{"session_id": self.unknown_session}],
            },
            {
                "id": "fact:f", "status": "active", "text": "private future",
                "provenance": [{"session_id": self.after_session}],
            },
        ]
        self.facts.write_text("".join(json.dumps(item, sort_keys=True) + "\n" for item in facts), encoding="utf-8")
        write_json(
            self.sessions,
            {
                self.old_session: "2026-06-01T10:00:00Z",
                self.excluded_session: "2026-06-01T11:00:00Z",
                self.after_session: "2026-07-05T10:00:00Z",
            },
        )
        eligible_ids = ["fact:a", "fact:b", "fact:g", "fact:h"]
        semantic_ids = ["fact:a", "fact:h"]
        self.pins = {
            "schema_version": 1,
            "pin_set_id": "test-public-engine-v4",
            "authority": "test_fixture",
            "development_task": {
                "task_id": "test-task",
                "branch": "main",
                "k": 5,
                "queries": [
                    {
                        "query_id": "test-task:query-1",
                        "query_text": "private query text",
                        "query_sha256": PUBLIC.sha256_bytes(b"private query text"),
                    }
                ],
                "eligible_before": "2026-07-01T00:00:00Z",
                "exclude_session_ids": [self.excluded_session],
            },
            "binary": {
                "provenance_mode": "reproducible_build_v1",
                "binary_sha256": "1" * 64,
                "binary_size_bytes": 100,
                "source_commit": "2" * 40,
                "source_tree": "3" * 40,
                "build_command": [
                    "go", "build", "-trimpath", "-buildvcs=false", "-o", "fixture-binary", "./cmd/entire-brain"
                ],
                "go_version": "go version go-test fixture/arch",
            },
            "corpus": {
                "facts_sha256": PUBLIC.sha256_file(self.facts),
                "facts_size_bytes": self.facts.stat().st_size,
                "session_dates_sha256": PUBLIC.sha256_file(self.sessions),
                "session_dates_size_bytes": self.sessions.stat().st_size,
                "prefilter_count": len(facts),
                "eligible_count": len(eligible_ids),
                "candidate_ids_algorithm": "sha256_canonical_sorted_id_array_v1",
                "eligible_ids_sha256": PUBLIC.canonical_sha256(sorted(eligible_ids)),
                "semantic_candidate_count": len(semantic_ids),
                "semantic_candidate_ids_sha256": PUBLIC.canonical_sha256(sorted(semantic_ids)),
            },
            "embedding_model": {
                "model_id": "embeddinggemma",
                "embedder_id": "ollama:embeddinggemma",
                "dimension": 3,
                "sha256": "4" * 64,
                "size_bytes": 200,
            },
            "engines": {
                "lexical_handrolled": {"semantic": False, "embedder_id": None, "dimension": None},
                "model2vec_rrf": {"semantic": True, "embedder_id": "fixture:model2vec", "dimension": 2},
                "embeddinggemma_rrf": {"semantic": True, "embedder_id": "ollama:embeddinggemma", "dimension": 3},
            },
            "runtime": {
                "platform": "fixture-platform",
                "node_version": "v-test",
                "node_sha256": "5" * 64,
                "server_script_repo_path": "scripts/bench/embed-server.mjs",
                "server_script_sha256": "d6893dc33b96d1d2d593cec25da67efe8bc8f21f7e0f3b25367662d8125b2971",
                "package_manifest_repo_path": "scripts/bench/package.json",
                "package_manifest_sha256": "8c8a9c0b6a6944c86799559d776862d0e679e7ba826823e5c96d5940f450ee75",
                "package_lock_repo_path": "scripts/bench/package-lock.json",
                "package_lock_sha256": "4a3eea2319165ee0253fa07c0bbffd07b8834ac552cce1947c99d5ea7d132e1a",
                "dependency_inventory_algorithm": "sha256_ordered_relative_path_nul_sha256_newline_v1",
                "dependency_inventory_sha256": "9" * 64,
                "dependency_file_count": 2,
                "dependency_total_bytes": 300,
                "node_llama_cpp_version": "fixture-version",
                "platform_package": "fixture-package",
                "platform_package_version": "fixture-version",
                "server_health_interval_seconds": 2.0,
            },
        }
        self.descriptor = {
            "id": self.pins["pin_set_id"],
            "sha256": PUBLIC.canonical_sha256(self.pins),
            "authority": "test_fixture",
        }
        self.matrix = json.loads((HERE / "engine-matrix.json").read_text(encoding="utf-8"))
        self.manifest = self._make_diagnostic()
        self.public_manifest = PUBLIC.build_public_bundle(
            self.manifest,
            self.private,
            self.output,
            self.pins,
            self.matrix,
            b"fixture-pseudonym-key-32-bytes!!",
        )

    def _make_diagnostic(self) -> pathlib.Path:
        records: list[dict[str, object]] = []
        for arm in PUBLIC.ARMS:
            run = self.private / f"runs/{arm}"
            run.mkdir(parents=True)
            stdout = run / "recall.stdout.json"
            stderr = run / "recall.stderr.txt"
            write_json(
                stdout,
                {
                    "effective_engine": arm,
                    "facts": [
                        {"id": "fact:a", "text": "PRIVATE FACT TEXT ALPHA"},
                        {"id": "fact:h", "text": "PRIVATE FACT TEXT ETA"},
                    ],
                },
            )
            stderr.write_text(
                "/Users/private/restricted ENGINE_VERIFICATION_TOKEN=plaintext-secret private@example.com\n",
                encoding="utf-8",
            )
            semantic = arm != "lexical_handrolled"
            dimension = self.pins["engines"][arm]["dimension"]
            vector_relative: str | None = None
            if semantic:
                vector = run / "vectors.bin"
                write_vectors(vector, self.pins["engines"][arm]["embedder_id"], dimension, ["fact:a", "fact:h"])
                vector_relative = vector.relative_to(self.private).as_posix()
            attestation_relative: str | None = None
            if arm == "embeddinggemma_rrf":
                attestation = run / "server-attestation.json"
                write_json(
                    attestation,
                    {
                        "schema_version": 2,
                        "model_path": "/Users/private/model.gguf",
                        "model_sha256": self.pins["embedding_model"]["sha256"],
                        "embedding_dimension": 3,
                        "node_version": "v-test",
                        "ownership_token_sha256": "a" * 64,
                        "process_pid": 1234,
                        "recall_window": {
                            "started_at": "2026-07-16T00:00:01Z",
                            "finished_at": "2026-07-16T00:00:04Z",
                        },
                        "observations": [
                            self._observation(0, "pre_recall", "2026-07-16T00:00:00Z", 1),
                            self._observation(1, "during_recall", "2026-07-16T00:00:02Z", 2),
                            self._observation(2, "heartbeat", "2026-07-16T00:00:03Z", 3),
                            self._observation(3, "post_recall", "2026-07-16T00:00:05Z", 4),
                        ],
                    },
                )
                attestation_relative = attestation.relative_to(self.private).as_posix()
            count = 2 if semantic else 0
            records.append(
                {
                    "schema_version": 2,
                    "pin_set": copy.deepcopy(self.descriptor),
                    "arm": arm,
                    "requested": {
                        "command": ["/Users/private/entire-brain", "recall", "private query text"],
                        "environment": {"HOME": "/Users/private", "ENGINE_VERIFICATION_TOKEN": "plaintext-secret"},
                        "namespace": f"private-{arm}",
                    },
                    "effective": {
                        "engine": arm,
                        "semantic_available": semantic,
                        "bm25_enabled": False,
                        "fallback_used": False,
                        "embedder_id": self.pins["engines"][arm]["embedder_id"],
                        "embedding_dimension": dimension,
                        "vector_count": count,
                        "vector_candidate_count": count,
                        "loaded_vector_count": 0,
                        "resident_vector_count": count,
                        "vector_namespace": f"private-{arm}",
                    },
                    "artifacts": {
                        "facts_source_path": self.facts.relative_to(self.private).as_posix(),
                        "session_dates_source_path": self.sessions.relative_to(self.private).as_posix(),
                        "stdout_path": stdout.relative_to(self.private).as_posix(),
                        "stderr_path": stderr.relative_to(self.private).as_posix(),
                        "vector_artifact_path": vector_relative,
                        "embedding_server_attestation_path": attestation_relative,
                    },
                    "corpus": {},
                    "result": {
                        "query_id": "test-task:query-1",
                        "fact_ids_in_order": ["fact:a", "fact:h"],
                        "output_valid": True,
                    },
                }
            )
        path = self.private / "engine-verification.json"
        write_json(path, {"schema_version": 2, "records": records})
        return path

    @staticmethod
    def _observation(sequence: int, phase: str, observed_at: str, count: int) -> dict[str, object]:
        return {
            "sequence": sequence,
            "phase": phase,
            "observed_at": observed_at,
            "healthy": True,
            "health_request_count": count,
            "request_nonce_sha256": f"{sequence + 10:064x}",
            "pid": 1234,
            "model_path": "/Users/private/model.gguf",
        }

    def errors(self) -> list[str]:
        return PUBLIC.validate_public_bundle(
            self.public_manifest,
            self.matrix,
            self.pins,
            self.descriptor,
        )

    def payload(self, role: str) -> tuple[pathlib.Path, dict[str, object]]:
        manifest = json.loads(self.public_manifest.read_text(encoding="utf-8"))
        row = next(item for item in manifest["artifact_inventory"]["files"] if item["role"] == role)
        path = self.output / row["relative_path"]
        return path, json.loads(path.read_text(encoding="utf-8"))

    def reseal(self, rebuild_projection_chain: bool = True) -> None:
        manifest = json.loads(self.public_manifest.read_text(encoding="utf-8"))
        rows = manifest["artifact_inventory"]["files"]
        if rebuild_projection_chain:
            by_role = {row["role"]: self.output / row["relative_path"] for row in rows}
            subjects = [
                (
                    "inputs_authenticated",
                    PUBLIC.canonical_sha256(
                        {"pin_set": manifest["pin_set"], "components": manifest["component_commitments"]}
                    ),
                ),
                ("temporal_projection_derived", PUBLIC.sha256_file(by_role["temporal_projection"])),
                *[
                    (f"{arm}_result_projected", PUBLIC.sha256_file(by_role[f"arm_record_{arm}"]))
                    for arm in PUBLIC.ARMS
                ],
            ]
            entries = []
            previous = "0" * 64
            for index, (event, subject) in enumerate(subjects):
                entry = PUBLIC._chain_entry(index, event, subject, previous)
                entries.append(entry)
                previous = entry["entry_sha256"]
            attestation = {
                "schema_version": 4,
                "profile": PUBLIC.PROFILE,
                "algorithm": PUBLIC.CHAIN_ALGORITHM,
                "entries": entries,
                "head_sha256": previous,
            }
            write_json(by_role["projection_attestation_sequence"], attestation)
        for row in rows:
            path = self.output / row["relative_path"]
            row["sha256"] = PUBLIC.sha256_file(path)
            row["size_bytes"] = path.stat().st_size
        rows.sort(key=lambda item: item["relative_path"])
        manifest["artifact_inventory"]["file_count"] = len(rows)
        manifest["artifact_inventory"]["logical_bytes"] = sum(row["size_bytes"] for row in rows)
        manifest["artifact_inventory"]["root_sha256"] = PUBLIC._inventory_digest(rows)
        write_json(self.public_manifest, manifest)

    def test_public_bundle_validates_and_is_protocol_checker_authoritative_v4(self) -> None:
        self.assertEqual([], self.errors())
        protocol_errors = CHECK.validate_engine_verification(
            self.matrix,
            {"status": "pass", "evidence": self.public_manifest.relative_to(self.root).as_posix()},
            here=self.root,
            repo=self.root,
            pins=self.pins,
            require_production=False,
            pin_repo=CHECK.REPO,
        )
        self.assertEqual([], protocol_errors)

    def test_stored_v4_cannot_close_gate_without_restricted_replay_attestation(self) -> None:
        contract_path = self.root / CHECK.ENGINE_EVIDENCE_STORAGE_REPO_PATH
        write_json(contract_path, {})
        relocation = {
            "manifest_path": self.public_manifest,
            "artifact_repo": self.root,
        }
        with (
            mock.patch.object(CHECK, "_validate_engine_pins", return_value=self.pins),
            mock.patch.object(CHECK, "_engine_pin_descriptor", return_value=self.descriptor),
            mock.patch.object(CHECK, "_validate_engine_storage_contract", return_value=relocation),
        ):
            errors = CHECK.validate_engine_verification(
                self.matrix,
                {"status": "pass", "evidence": CHECK.ENGINE_EVIDENCE_STORAGE_REPO_PATH},
                here=self.root / "benchmarks/agent-brain/confirmatory",
                repo=self.root,
                pins=self.pins,
                require_production=True,
                pin_repo=CHECK.REPO,
                require_storage_contract=True,
            )
        self.assertTrue(any("authenticated restricted replay attestation" in error for error in errors))

    def test_private_payloads_and_identifiers_are_not_copied(self) -> None:
        combined = b"\n".join(path.read_bytes() for path in self.output.rglob("*.json"))
        for forbidden in (
            b"PRIVATE FACT TEXT",
            b"private answer",
            b"private query text",
            self.old_session.encode(),
            self.excluded_session.encode(),
            b"/Users/private",
            b"private@example.com",
            b"plaintext-secret",
            b"ENGINE_VERIFICATION_TOKEN",
            b'"HOME"',
            b"fact:a",
        ):
            self.assertNotIn(forbidden, combined)
        self.assertFalse(any(path.suffix in {".bin", ".gguf", ".ndjson"} for path in self.output.rglob("*")))

    def test_refs_are_domain_separated_and_key_is_not_persisted(self) -> None:
        temporal_path, temporal = self.payload("temporal_projection")
        candidate_refs = {item["ref"] for item in temporal["candidates"]}
        session_refs = {item["ref"] for item in temporal["sessions"]}
        self.assertTrue(candidate_refs)
        self.assertTrue(session_refs)
        self.assertTrue(candidate_refs.isdisjoint(session_refs))
        self.assertNotIn(b"fixture-pseudonym-key", temporal_path.read_bytes())

    def test_pseudonymizer_fails_closed_on_128_bit_collision(self) -> None:
        class FixedDigest:
            @staticmethod
            def hexdigest() -> str:
                return "0" * 64

        pseudonyms = PUBLIC._Pseudonymizer(b"k" * 32)
        with mock.patch.object(PUBLIC.hmac, "new", return_value=FixedDigest()):
            pseudonyms.ref("candidate", "first-private-id")
            with self.assertRaisesRegex(PUBLIC.PublicEvidenceError, "pseudonym collision"):
                pseudonyms.ref("candidate", "second-private-id")

    def test_recursive_privacy_scanner_rejects_secret_and_plaintext_token(self) -> None:
        path, record = self.payload("arm_record_lexical_handrolled")
        record["api_token"] = "sk-abcdefghijklmnopqrstuvwxyz123456"
        write_json(path, record)
        self.reseal()
        errors = self.errors()
        self.assertTrue(any("plaintext token field" in error or "secret-like" in error for error in errors))

    def test_recursive_privacy_scanner_rejects_absolute_path(self) -> None:
        path, record = self.payload("arm_record_lexical_handrolled")
        record["invocation"]["branch_ref"] = "/Users/alice/private/repo"
        write_json(path, record)
        self.reseal()
        self.assertTrue(any("absolute host path" in error for error in self.errors()))

    def test_recursive_privacy_scanner_rejects_raw_session_and_fact_text(self) -> None:
        path, temporal = self.payload("temporal_projection")
        temporal["raw_session_id"] = self.old_session
        temporal["fact_text"] = "the private fact sentence"
        write_json(path, temporal)
        self.reseal()
        errors = self.errors()
        self.assertTrue(any("raw session identifier" in error for error in errors))
        self.assertTrue(any("forbidden privacy key fact_text" in error for error in errors))

    def test_checker_rejects_artifact_tampering_without_reseal(self) -> None:
        path, _ = self.payload("arm_record_model2vec_rrf")
        path.write_bytes(path.read_bytes() + b" ")
        self.assertTrue(any("content hash mismatch" in error for error in self.errors()))

    def test_checker_rejects_unlisted_file(self) -> None:
        write_json(self.output / "unlisted.json", {"schema_version": 4})
        self.assertTrue(any("does not exactly equal the bundle file set" in error for error in self.errors()))

    def test_checker_independently_recomputes_temporal_eligibility(self) -> None:
        path, temporal = self.payload("temporal_projection")
        known = next(item for item in temporal["sessions"] if item["state"] == "known")
        known["created_at"] = "2026-07-02T00:00:00Z"
        write_json(path, temporal)
        self.reseal()
        self.assertTrue(any("eligibility differs from independent recomputation" in error for error in self.errors()))

    def test_checker_rejects_ranked_result_order_mutation(self) -> None:
        path, record = self.payload("arm_record_lexical_handrolled")
        record["result"]["candidate_refs_in_order"].reverse()
        write_json(path, record)
        self.reseal()
        self.assertTrue(any("ranked candidate order commitment differs" in error for error in self.errors()))

    def test_checker_rejects_ranked_ineligible_candidate(self) -> None:
        temporal_path, temporal = self.payload("temporal_projection")
        ineligible = next(item["ref"] for item in temporal["candidates"] if item["eligibility"] == "excluded")
        path, record = self.payload("arm_record_lexical_handrolled")
        record["result"]["candidate_refs_in_order"][0] = ineligible
        record["result"]["candidate_order_sha256"] = PUBLIC.canonical_sha256(
            record["result"]["candidate_refs_in_order"]
        )
        write_json(path, record)
        self.reseal()
        self.assertTrue(any("inactive or ineligible" in error for error in self.errors()))

    def test_checker_rejects_self_consistent_vector_candidate_mismatch(self) -> None:
        temporal_path, temporal = self.payload("temporal_projection")
        ineligible = next(item["ref"] for item in temporal["candidates"] if item["eligibility"] == "excluded")
        path, record = self.payload("arm_record_model2vec_rrf")
        vector = record["vector_index"]
        vector["chunks"][0]["candidate_refs"][0] = ineligible
        refs = [ref for chunk in vector["chunks"] for ref in chunk["candidate_refs"]]
        for chunk in vector["chunks"]:
            chunk["sha256"] = PUBLIC.canonical_sha256(chunk["candidate_refs"])
        vector["candidate_set_sha256"] = PUBLIC.canonical_sha256(sorted(refs))
        vector["candidate_order_sha256"] = PUBLIC.canonical_sha256(refs)
        vector["root_sha256"] = PUBLIC.canonical_sha256([chunk["sha256"] for chunk in vector["chunks"]])
        write_json(path, record)
        self.reseal()
        self.assertTrue(any("do not exactly cover active+eligible" in error for error in self.errors()))

    def test_checker_rejects_projection_attestation_sequence_mutation(self) -> None:
        path, attestation = self.payload("projection_attestation_sequence")
        attestation["entries"][2]["event"] = "embeddinggemma_rrf_result_projected"
        write_json(path, attestation)
        self.reseal(rebuild_projection_chain=False)
        self.assertTrue(any("order, subject, or hash chain differs" in error for error in self.errors()))

    def test_checker_rejects_managed_server_phase_sequence_mutation(self) -> None:
        path, record = self.payload("arm_record_embeddinggemma_rrf")
        record["managed_server_attestation"]["observations"][1]["phase"] = "heartbeat"
        write_json(path, record)
        self.reseal()
        self.assertTrue(any("pre/during/post phase sequence changed" in error for error in self.errors()))

    def test_checker_rejects_component_commitment_mutation(self) -> None:
        manifest = json.loads(self.public_manifest.read_text(encoding="utf-8"))
        manifest["component_commitments"][0]["sha256"] = "f" * 64
        write_json(self.public_manifest, manifest)
        self.assertTrue(any("component commitments differ from pins" in error for error in self.errors()))


if __name__ == "__main__":
    unittest.main()
