from __future__ import annotations

import contextlib
import copy
import hashlib
import importlib.util
import io
import json
import pathlib
import shutil
import socket
import struct
import subprocess
import sys
import tempfile
import time
import unittest
from unittest import mock


HERE = pathlib.Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location("verify_engines", HERE / "verify_engines.py")
assert SPEC and SPEC.loader
VERIFY = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = VERIFY
SPEC.loader.exec_module(VERIFY)


def digest(path: pathlib.Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def write_vectors(path: pathlib.Path, model_id: str, dimension: int, fact_ids: list[str]) -> None:
    raw = bytearray(b"EBV1")
    model = model_id.encode()
    raw.extend(struct.pack("<H", len(model)))
    raw.extend(model)
    raw.extend(struct.pack("<II", dimension, len(fact_ids)))
    for fact_id in fact_ids:
        encoded = fact_id.encode()
        raw.extend(struct.pack("<H", len(encoded)))
        raw.extend(encoded)
        raw.extend(b"\0" * (dimension * 4))
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(raw)


class EngineVerificationRunnerTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self.temp.name)
        self.frozen = self.root / "frozen"
        self.data = self.frozen / "data"
        self.config = self.frozen / "config"
        self.state = self.frozen / "state"
        self.config.mkdir(parents=True)
        self.state.mkdir(parents=True)
        self.active_ids = ["fact:a", "fact:b", "fact:d", "fact:e", "fact:f", "fact:g"]
        self.facts = self.data / "repos/local/example/facts/main-0d6e4079/facts.ndjson"
        self.facts.parent.mkdir(parents=True)
        self.facts.write_text(
            '{"id":"fact:a","status":"active","provenance":[{"session_id":"session-1"}]}\n'
            '{"id":"fact:b","status":"active","provenance":[{"session_id":"session-1"}]}\n'
            '{"id":"fact:d","status":"active","provenance":[{"session_id":"session-1"}]}\n'
            '{"id":"fact:e","status":"active","provenance":[{"session_id":"session-1"}]}\n'
            '{"id":"fact:f","status":"active","provenance":[{"session_id":"session-1"}]}\n'
            '{"id":"fact:g","status":"active","provenance":[{"session_id":"session-1"}]}\n'
            '{"id":"fact:c","status":"active","provenance":[{"session_id":"late-session"}]}\n',
            encoding="utf-8",
        )
        taxonomy = self.data / "repos/local/example/facts/taxonomy.json"
        taxonomy.write_text('{"categories":{},"paths":[]}\n', encoding="utf-8")
        manifest = self.data / "repos/local/example/manifest.json"
        manifest.write_text('{"schema_version":3}\n', encoding="utf-8")
        self.sessions = self.frozen / "session_dates.json"
        self.sessions.write_text(
            '{"session-1":"2026-01-01T00:00:00Z","late-session":"2027-01-01T00:00:00Z"}\n',
            encoding="utf-8",
        )

        inputs = self.root / "input"
        inputs.mkdir()
        self.binary = inputs / "entire-brain"
        self.binary.write_bytes(b"fake binary\n")
        self.binary.chmod(0o755)
        self.model = inputs / "embeddinggemma.gguf"
        self.model.write_bytes(b"fake embedding model\n")
        self.node = inputs / "node"
        self.node.write_bytes(b"fake node runtime\n")
        self.node.chmod(0o755)
        self.dependencies = inputs / "node_modules"
        dependency = self.dependencies / "node-llama-cpp/index.js"
        dependency.parent.mkdir(parents=True)
        dependency.write_text("export const fixture = true;\n", encoding="utf-8")
        # These paths sort differently as pathlib components and serialized
        # relative strings; the evidence contract uses the latter.
        (self.dependencies / "a-b").write_text("flat\n", encoding="utf-8")
        nested = self.dependencies / "a/b"
        nested.parent.mkdir(parents=True)
        nested.write_text("nested\n", encoding="utf-8")
        rows, dependency_hash = VERIFY.dependency_rows(self.dependencies)
        self.assertEqual([row[0] for row in rows], sorted(row[0] for row in rows))

        runtime_sources = {
            "server_script": VERIFY.REPO / "scripts/bench/embed-server.mjs",
            "package_manifest": VERIFY.REPO / "scripts/bench/package.json",
            "package_lock": VERIFY.REPO / "scripts/bench/package-lock.json",
        }
        query = "Pi review cache tokens are double counted"
        self.pins = {
            "schema_version": 1,
            "pin_set_id": "engine-test-fixture-v1",
            "authority": "test_fixture",
            "development_task": {
                "task_id": "test-task",
                "branch": "main",
                "k": 5,
                "queries": [
                    {
                        "query_id": "test-task:query-1",
                        "query_text": query,
                        "query_sha256": hashlib.sha256(query.encode()).hexdigest(),
                    }
                ],
                "eligible_before": "2026-07-01T19:32:18+02:00",
                "exclude_session_ids": ["excluded-session"],
            },
            "binary": {
                "provenance_mode": "reproducible_build_v1",
                "binary_sha256": digest(self.binary),
                "binary_size_bytes": self.binary.stat().st_size,
                "source_commit": "a" * 40,
                "source_tree": "b" * 40,
                "build_command": [
                    "go",
                    "build",
                    "-trimpath",
                    "-buildvcs=false",
                    "-o",
                    "/tmp/fixture-entire-brain",
                    "./cmd/entire-brain",
                ],
                "go_version": "go version go-test fixture/arch",
            },
            "corpus": {
                "facts_sha256": digest(self.facts),
                "facts_size_bytes": self.facts.stat().st_size,
                "session_dates_sha256": digest(self.sessions),
                "session_dates_size_bytes": self.sessions.stat().st_size,
                "prefilter_count": 7,
                "eligible_count": 6,
                "candidate_ids_algorithm": VERIFY.CANDIDATE_IDS_ALGORITHM,
                "eligible_ids_sha256": VERIFY.canonical_sha256(self.active_ids),
                "semantic_candidate_count": 6,
                "semantic_candidate_ids_sha256": VERIFY.canonical_sha256(self.active_ids),
            },
            "embedding_model": {
                "model_id": "embeddinggemma-fixture",
                "embedder_id": "ollama:embeddinggemma",
                "dimension": 768,
                "sha256": digest(self.model),
                "size_bytes": self.model.stat().st_size,
            },
            "engines": {
                "lexical_handrolled": {"semantic": False, "embedder_id": None, "dimension": None},
                "model2vec_rrf": {
                    "semantic": True,
                    "embedder_id": "minishlab/potion-retrieval-32M",
                    "dimension": 512,
                },
                "embeddinggemma_rrf": {
                    "semantic": True,
                    "embedder_id": "ollama:embeddinggemma",
                    "dimension": 768,
                },
            },
            "runtime": {
                "platform": "fixture-platform",
                "node_version": "v-test",
                "node_sha256": digest(self.node),
                "server_script_repo_path": runtime_sources["server_script"].relative_to(VERIFY.REPO).as_posix(),
                "server_script_sha256": digest(runtime_sources["server_script"]),
                "package_manifest_repo_path": runtime_sources["package_manifest"].relative_to(VERIFY.REPO).as_posix(),
                "package_manifest_sha256": digest(runtime_sources["package_manifest"]),
                "package_lock_repo_path": runtime_sources["package_lock"].relative_to(VERIFY.REPO).as_posix(),
                "package_lock_sha256": digest(runtime_sources["package_lock"]),
                "dependency_inventory_algorithm": VERIFY.DEPENDENCY_INVENTORY_ALGORITHM,
                "dependency_inventory_sha256": dependency_hash,
                "dependency_file_count": len(rows),
                "dependency_total_bytes": sum(size for _, _, size in rows),
                "node_llama_cpp_version": "test-version",
                "platform_package": "fixture-platform-package",
                "platform_package_version": "test-version",
                "server_health_interval_seconds": 0.01,
            },
        }
        self.repo = self.root / "repo"
        self.repo.mkdir()
        self.output = self.root / "evidence/engine-run"
        self.settings = VERIFY.Settings(
            binary=self.binary,
            frozen_data_dir=self.data,
            frozen_config_dir=self.config,
            frozen_state_dir=self.state,
            frozen_facts=self.facts,
            repo_root=self.repo,
            session_dates=self.sessions,
            query_id="test-task:query-1",
            query=query,
            branch="main",
            k=5,
            eligible_before="2026-07-01T19:32:18+02:00",
            exclude_session_ids=("excluded-session",),
            embedding_model=self.model,
            embed_url="http://127.0.0.1:11500",
            node_runtime=self.node,
            runtime_dependency_root=self.dependencies,
            server_start_timeout_seconds=1,
            server_health_interval_seconds=0.01,
            output_dir=self.output,
            artifact_root=self.root,
        )
        self.attestation_mutator = None
        self.embedding_delay_seconds = 0.05
        self.during_observe_hook = None

    def tearDown(self) -> None:
        self.temp.cleanup()

    def fake_version(self, command: list[str], **_: object) -> subprocess.CompletedProcess[bytes]:
        return subprocess.CompletedProcess(command, 0, b"v-test\n", b"")

    def fake_run(self, command: list[str], **kwargs: object) -> subprocess.CompletedProcess[bytes]:
        env = kwargs["env"]
        assert isinstance(env, dict)
        lexical = "--no-semantic" in command
        embeddinggemma = env["ENTIRE_BRAIN_EMBEDDER"] == "ollama"
        arm = "lexical_handrolled" if lexical else "embeddinggemma_rrf" if embeddinggemma else "model2vec_rrf"
        engine_pin = self.pins["engines"][arm]
        semantic = engine_pin["semantic"]
        engine: dict[str, object] = {
            "schema_version": 1,
            "effective_engine": arm,
            "identity_verified": True,
            "semantic_requested": semantic,
            "semantic_applied": semantic,
            "semantic_available": semantic,
            "bm25_enabled": False,
            "bm25_applied": False,
            "fallback_used": False,
        }
        if semantic:
            data_dir = pathlib.Path(env["ENTIRE_PLUGIN_DATA_DIR"])
            derived_facts = next(data_dir.rglob("facts.ndjson"))
            vectors = derived_facts.parent / "embeddings/vectors.bin"
            write_vectors(vectors, engine_pin["embedder_id"], engine_pin["dimension"], self.active_ids)
            engine.update(
                {
                    "embedder_id": engine_pin["embedder_id"],
                    "embedding_dimension": engine_pin["dimension"],
                    "vector_count": 6,
                    "vector_candidate_count": 6,
                    "loaded_vector_count": 0,
                    "resident_vector_count": 6,
                    "vector_cache_backend": "flat_file",
                    "vector_cache_path": str(vectors),
                    "vector_cache_read_only": False,
                }
            )
        payload = {
            "effective_engine": arm,
            "retrieval_engine": engine,
            "eligibility": {
                "prefilter_corpus_count": 7,
                "eligible_count": 6,
                "excluded_counts": {
                    "empty_provenance": 0,
                    "excluded_session": 0,
                    "unknown_session": 0,
                    "at_or_after_cutoff": 1,
                },
                "delivered_count": 1,
            },
            "facts": [{"id": "fact:a"}],
        }
        if embeddinggemma and self.embedding_delay_seconds:
            time.sleep(self.embedding_delay_seconds)
        return subprocess.CompletedProcess(command, 0, json.dumps(payload).encode(), b"")

    @contextlib.contextmanager
    def fake_server(self, settings: VERIFY.Settings, bundle: VERIFY.RuntimeBundle, pins: dict[str, object]):
        self.assertEqual(digest(bundle.embedding_model), pins["embedding_model"]["sha256"])
        run_dir = settings.output_dir / "runs/embeddinggemma_rrf"
        run_dir.mkdir(parents=True, exist_ok=True)
        stdout = run_dir / "server.stdout.txt"
        stderr = run_dir / "server.stderr.txt"
        attestation_path = run_dir / "server-attestation.json"
        stdout.write_text("fixture server ready\n", encoding="utf-8")
        stderr.write_bytes(b"")
        token = "fixture-ownership-token"
        token_hash = hashlib.sha256(token.encode()).hexdigest()
        pid = 4242
        observations: list[dict[str, object]] = []
        recall_window: dict[str, str] = {}

        def observe(phase: str) -> dict[str, object]:
            if phase == "during_recall" and self.during_observe_hook is not None:
                self.during_observe_hook()
            sequence = len(observations)
            observation = {
                "sequence": sequence,
                "phase": phase,
                "observed_at": VERIFY.dt.datetime.now(VERIFY.dt.UTC).isoformat(),
                "pid": pid,
                "ownership_token_sha256": token_hash,
                "request_nonce_sha256": hashlib.sha256(f"fixture-{sequence}".encode()).hexdigest(),
                "health_request_count": sequence + 1,
                "model_path": str(bundle.embedding_model),
                "model_sha256": pins["embedding_model"]["sha256"],
                "embedding_dimension": pins["embedding_model"]["dimension"],
                "node_version": pins["runtime"]["node_version"],
                "healthy": True,
            }
            observations.append(observation)
            return observation

        def record_recall_window(started_at: str, finished_at: str) -> None:
            recall_window.update({"started_at": started_at, "finished_at": finished_at})

        observe("pre_recall")
        evidence = VERIFY.ServerEvidence(
            (str(bundle.node_runtime), str(bundle.server_script)),
            {
                "GGUF": str(bundle.embedding_model),
                "HOST": "127.0.0.1",
                "PORT": "11500",
                "ENGINE_VERIFICATION_TOKEN": token,
            },
            stdout,
            stderr,
            attestation_path,
            observe,
            record_recall_window,
        )
        yield evidence
        observe("post_recall")
        attestation = {
            "schema_version": 2,
            "pin_set_id": pins["pin_set_id"],
            "process_pid": pid,
            "ownership_token_sha256": token_hash,
            "model_path": str(bundle.embedding_model),
            "model_sha256": pins["embedding_model"]["sha256"],
            "embedding_dimension": pins["embedding_model"]["dimension"],
            "node_version": pins["runtime"]["node_version"],
            "health_interval_seconds": pins["runtime"]["server_health_interval_seconds"],
            "recall_window": recall_window,
            "observations": observations,
        }
        if self.attestation_mutator is not None:
            self.attestation_mutator(attestation)
        attestation_path.write_text(json.dumps(attestation, sort_keys=True) + "\n", encoding="utf-8")

    def execute(self, run=None, pins=None) -> pathlib.Path:
        with mock.patch.object(
            VERIFY,
            "assert_pinned_loopback_endpoint_available",
            return_value=("127.0.0.1", 11500),
        ):
            return VERIFY.execute_with_test_pins(
                self.settings,
                self.pins if pins is None else pins,
                run_command=run or self.fake_run,
                version_command=self.fake_version,
                server_context=self.fake_server,
            )

    def checker_errors(self, manifest_path: pathlib.Path) -> list[str]:
        matrix = json.loads((HERE / "engine-matrix.json").read_text(encoding="utf-8"))
        return VERIFY.check_protocol.validate_engine_verification(
            matrix,
            {"status": "pass", "evidence": manifest_path.relative_to(self.root).as_posix()},
            here=self.root,
            repo=self.root,
            pins=self.pins,
            require_production=False,
            pin_repo=VERIFY.REPO,
        )

    def storage_checker_fixture(self, manifest_path: pathlib.Path) -> tuple[pathlib.Path, pathlib.Path, dict[str, object]]:
        checkout = self.root / "storage-checkout"
        if checkout.exists() or checkout.is_symlink():
            if checkout.is_symlink():
                checkout.unlink()
            else:
                shutil.rmtree(checkout)
        here = checkout / "benchmarks/agent-brain/confirmatory"
        hydration_parent = checkout / VERIFY.check_protocol.ENGINE_EVIDENCE_HYDRATION_PARENT
        hydration_parent.parent.mkdir(parents=True, exist_ok=True)
        shutil.copytree(self.root / "evidence", hydration_parent / "evidence")
        archive_root = hydration_parent / "evidence"
        files = [path for path in archive_root.rglob("*") if path.is_file() and not path.is_symlink()]
        relocated_manifest = hydration_parent / manifest_path.relative_to(self.root)
        recorded_manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        contract: dict[str, object] = {
            "schema_version": 1,
            "storage": {
                "kind": "github_immutable_release_asset",
                "repository": "entireio/entire-brain",
                "tag": "engine-evidence-fixture-v1",
                "asset_name": "engine-evidence-fixture-v1.tar.zst",
                "asset_url": "https://github.com/entireio/entire-brain/releases/download/engine-evidence-fixture-v1/engine-evidence-fixture-v1.tar.zst",
                "asset_size_bytes": 123,
                "asset_sha256": "1" * 64,
                "publication_disposition": "approved",
                "privacy_review": "publishable",
                "published": True,
                "release_id": 101,
                "asset_id": 202,
                "release_immutable": True,
                "release_target_commitish": "a" * 40,
                "asset_api_digest": "sha256:" + "1" * 64,
                "verified_at": "2026-07-16T12:00:00Z",
            },
            "archive": {
                "format": "tar_zstd",
                "root": "evidence",
                "regular_file_count": len(files),
                "logical_bytes": sum(path.stat().st_size for path in files),
                "symlink_count": 0,
            },
            "evidence": {
                "manifest_path": manifest_path.relative_to(self.root).as_posix(),
                "manifest_sha256": digest(relocated_manifest),
                "recorded_artifact_root": str(self.root.resolve()),
            },
            "recorded_external_inputs": {
                "repo_root": recorded_manifest["records"][0]["requested"]["environment"]["ENTIRE_REPO_ROOT"],
                "repo_key": "local/example",
            },
            "hydration": {
                "repo_relative_parent": VERIFY.check_protocol.ENGINE_EVIDENCE_HYDRATION_PARENT,
            },
        }
        contract_path = checkout / VERIFY.check_protocol.ENGINE_EVIDENCE_STORAGE_REPO_PATH
        contract_path.parent.mkdir(parents=True, exist_ok=True)
        contract_path.write_text(json.dumps(contract, sort_keys=True) + "\n", encoding="utf-8")
        return checkout, contract_path, contract

    def storage_checker_errors(self, checkout: pathlib.Path) -> list[str]:
        matrix = json.loads((HERE / "engine-matrix.json").read_text(encoding="utf-8"))
        return VERIFY.check_protocol.validate_engine_verification(
            matrix,
            {
                "status": "pass",
                "evidence": VERIFY.check_protocol.ENGINE_EVIDENCE_STORAGE_REPO_PATH,
            },
            here=checkout / "benchmarks/agent-brain/confirmatory",
            repo=checkout,
            pins=self.pins,
            require_production=False,
            pin_repo=VERIFY.REPO,
            require_storage_contract=True,
        )

    def test_execute_publishes_exact_three_checker_valid_records(self) -> None:
        source_before = self.facts.read_bytes()
        manifest_path = self.execute()
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        records = manifest["records"]
        self.assertEqual([record["arm"] for record in records], list(VERIFY.ARMS))
        self.assertEqual(len(records), 3)
        self.assertEqual(self.facts.read_bytes(), source_before)
        self.assertFalse((self.facts.parent / "embeddings").exists())
        self.assertEqual(len({record["artifacts"]["derived_facts_path"] for record in records}), 3)
        self.assertEqual(len({record["artifacts"]["facts_source_path"] for record in records}), 1)
        self.assertEqual(self.checker_errors(manifest_path), [])

    def test_storage_contract_validates_relocated_hydrated_evidence_without_rewriting_manifest(self) -> None:
        manifest_path = self.execute()
        original_manifest = manifest_path.read_bytes()
        checkout, _, _ = self.storage_checker_fixture(manifest_path)
        self.assertEqual(self.storage_checker_errors(checkout), [])
        self.assertEqual(manifest_path.read_bytes(), original_manifest)

    def test_storage_contract_fails_closed_for_missing_and_mutated_hydrated_bytes(self) -> None:
        manifest_path = self.execute()
        checkout, _, _ = self.storage_checker_fixture(manifest_path)
        hydration = checkout / VERIFY.check_protocol.ENGINE_EVIDENCE_HYDRATION_PARENT
        missing = hydration / "evidence/engine-run/runs/lexical_handrolled/recall.stderr.txt"
        missing.unlink()
        errors = self.storage_checker_errors(checkout)
        self.assertTrue(any("regular-file count differs" in error for error in errors), errors)
        self.assertTrue(any("file does not exist" in error for error in errors), errors)

        checkout, _, _ = self.storage_checker_fixture(manifest_path)
        hydration = checkout / VERIFY.check_protocol.ENGINE_EVIDENCE_HYDRATION_PARENT
        mutated = hydration / "evidence/engine-run/artifacts/sources/facts.ndjson"
        mutated.write_bytes(b"X" + mutated.read_bytes()[1:])
        errors = self.storage_checker_errors(checkout)
        self.assertTrue(any("content hash mismatch" in error for error in errors), errors)

    def test_storage_contract_rejects_redirected_or_symlinked_hydration_root(self) -> None:
        manifest_path = self.execute()
        checkout, contract_path, contract = self.storage_checker_fixture(manifest_path)
        contract["hydration"]["repo_relative_parent"] = "benchmarks/agent-brain/confirmatory/.redirected"  # type: ignore[index]
        contract_path.write_text(json.dumps(contract, sort_keys=True) + "\n", encoding="utf-8")
        errors = self.storage_checker_errors(checkout)
        self.assertIn("engine evidence hydration parent was redirected", errors)

        checkout, _, _ = self.storage_checker_fixture(manifest_path)
        hydration = checkout / VERIFY.check_protocol.ENGINE_EVIDENCE_HYDRATION_PARENT
        moved = hydration.with_name(".engine-evidence-real")
        hydration.rename(moved)
        hydration.symlink_to(moved, target_is_directory=True)
        errors = self.storage_checker_errors(checkout)
        self.assertIn("engine evidence hydration parent must not traverse a symlink", errors)

    def test_storage_contract_rejects_unmapped_absolute_recorded_fields(self) -> None:
        manifest_path = self.execute()
        checkout, _, _ = self.storage_checker_fixture(manifest_path)
        relocated = (
            checkout
            / VERIFY.check_protocol.ENGINE_EVIDENCE_HYDRATION_PARENT
            / manifest_path.relative_to(self.root)
        )
        manifest = json.loads(relocated.read_text(encoding="utf-8"))
        manifest["records"][0]["requested"]["command"][0] = "/tmp/unmapped-entire-brain"
        manifest["records"][1]["requested"]["environment"]["UNMAPPED_ROOT"] = "/tmp/unmapped-root"
        relocated.write_text(json.dumps(manifest, sort_keys=True) + "\n", encoding="utf-8")
        errors = self.storage_checker_errors(checkout)
        self.assertTrue(any("outside recorded_artifact_root" in error for error in errors), errors)
        self.assertTrue(any("unmapped absolute field UNMAPPED_ROOT" in error for error in errors), errors)

    def test_storage_contract_refuses_pending_or_privacy_failed_release(self) -> None:
        manifest_path = self.execute()
        checkout, contract_path, contract = self.storage_checker_fixture(manifest_path)
        storage = contract["storage"]
        storage.update({  # type: ignore[union-attr]
            "publication_disposition": "regeneration_required",
            "privacy_review": "fail",
            "published": False,
            "release_id": None,
            "asset_id": None,
            "release_immutable": False,
            "release_target_commitish": None,
            "asset_api_digest": None,
            "verified_at": None,
        })
        contract_path.write_text(json.dumps(contract, sort_keys=True) + "\n", encoding="utf-8")
        errors = self.storage_checker_errors(checkout)
        self.assertIn("engine evidence publication disposition is not approved", errors)
        self.assertIn("engine evidence privacy review did not pass", errors)
        self.assertIn("engine evidence release asset is not published", errors)

    def test_checker_requires_exact_v2_manifest_wrapper_file(self) -> None:
        manifest_path = self.execute()
        canonical = json.loads(manifest_path.read_text(encoding="utf-8"))
        variants = (
            (canonical["records"], "manifest object"),
            ({"schema_version": 2, "records": canonical["records"], "extra": True}, "exactly schema_version and records"),
            ({"schema_version": 1, "records": canonical["records"]}, "manifest schema_version must be 2"),
            ({"record_paths": []}, "exactly schema_version and records"),
            (canonical["records"][0], "exactly schema_version and records"),
        )
        for value, expected in variants:
            with self.subTest(expected=expected):
                manifest_path.write_text(json.dumps(value) + "\n", encoding="utf-8")
                self.assertTrue(any(expected in error for error in self.checker_errors(manifest_path)))
        manifest_path.write_text(json.dumps(canonical) + "\n", encoding="utf-8")
        errors = self.checker_errors(self.output)
        self.assertTrue(any("exactly one regular JSON file" in error for error in errors))
        link = self.root / "manifest-link.json"
        link.symlink_to(manifest_path)
        errors = self.checker_errors(link)
        self.assertTrue(any("must not traverse a symlink" in error for error in errors))

    def test_checker_applies_recursive_record_schema_and_rejects_nested_extras(self) -> None:
        manifest_path = self.execute()
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        manifest["records"][0]["extra_top_level"] = True
        manifest["records"][0]["requested"]["extra_nested"] = "forbidden"
        manifest_path.write_text(json.dumps(manifest, sort_keys=True) + "\n", encoding="utf-8")
        errors = self.checker_errors(manifest_path)
        self.assertTrue(any("schema prohibits additional property extra_top_level" in error for error in errors))
        self.assertTrue(any("schema prohibits additional property extra_nested" in error for error in errors))

    def test_checker_rejects_self_consistent_delivered_fact_outside_retained_corpus(self) -> None:
        manifest_path = self.execute()
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        record = manifest["records"][0]
        stdout_path = self.root / record["artifacts"]["stdout_path"]
        stdout = json.loads(stdout_path.read_text(encoding="utf-8"))
        stdout["facts"] = [{"id": "fact:not-in-retained-corpus"}]
        stdout_path.write_text(json.dumps(stdout, sort_keys=True) + "\n", encoding="utf-8")
        record["artifacts"]["stdout_sha256"] = digest(stdout_path)
        record["result"]["fact_ids_in_order"] = ["fact:not-in-retained-corpus"]
        manifest_path.write_text(json.dumps(manifest, sort_keys=True) + "\n", encoding="utf-8")
        errors = self.checker_errors(manifest_path)
        self.assertTrue(any("delivered fact ids are inactive or temporally ineligible" in error for error in errors))

    def test_checker_reconciles_delivered_count_to_ranked_fact_count(self) -> None:
        manifest_path = self.execute()
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        record = manifest["records"][0]
        stdout_path = self.root / record["artifacts"]["stdout_path"]
        stdout = json.loads(stdout_path.read_text(encoding="utf-8"))
        stdout["eligibility"]["delivered_count"] = 2
        stdout_path.write_text(json.dumps(stdout, sort_keys=True) + "\n", encoding="utf-8")
        record["artifacts"]["stdout_sha256"] = digest(stdout_path)
        record["corpus"]["delivered_count"] = 2
        manifest_path.write_text(json.dumps(manifest, sort_keys=True) + "\n", encoding="utf-8")
        errors = self.checker_errors(manifest_path)
        self.assertTrue(any("delivered_count does not equal ranked fact count" in error for error in errors))

    def test_checker_rejects_exact_k_plus_one_self_consistent_delivery(self) -> None:
        manifest_path = self.execute()
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        record = manifest["records"][0]
        stdout_path = self.root / record["artifacts"]["stdout_path"]
        stdout = json.loads(stdout_path.read_text(encoding="utf-8"))
        stdout["facts"] = [{"id": fact_id} for fact_id in self.active_ids]
        stdout["eligibility"]["delivered_count"] = len(self.active_ids)
        stdout_path.write_text(json.dumps(stdout, sort_keys=True) + "\n", encoding="utf-8")
        record["artifacts"]["stdout_sha256"] = digest(stdout_path)
        record["corpus"]["delivered_count"] = len(self.active_ids)
        record["result"]["fact_ids_in_order"] = self.active_ids
        manifest_path.write_text(json.dumps(manifest, sort_keys=True) + "\n", encoding="utf-8")

        errors = self.checker_errors(manifest_path)
        self.assertEqual(errors, ["engine records[0]: ranked fact count exceeds pinned development task k"])

    def test_checker_rejects_binary_substitution_even_when_records_are_rehashed(self) -> None:
        manifest_path = self.execute()
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        replacement = self.root / "replacement-entire-brain"
        replacement.write_bytes(b"other executable bytes\n")
        replacement.chmod(0o755)
        replacement_rel = replacement.relative_to(self.root).as_posix()
        attestation_path = self.root / manifest["records"][0]["artifacts"]["binary_attestation_path"]
        attestation = json.loads(attestation_path.read_text(encoding="utf-8"))
        attestation["binary_path"] = replacement_rel
        attestation["binary_sha256"] = digest(replacement)
        attestation["binary_size_bytes"] = replacement.stat().st_size
        attestation_path.write_text(json.dumps(attestation, sort_keys=True) + "\n", encoding="utf-8")
        for record in manifest["records"]:
            record["artifacts"]["binary_path"] = replacement_rel
            record["artifacts"]["binary_sha256"] = digest(replacement)
            record["artifacts"]["binary_attestation_sha256"] = digest(attestation_path)
            record["requested"]["command"][0] = str(replacement.resolve())
        manifest_path.write_text(json.dumps(manifest, sort_keys=True) + "\n", encoding="utf-8")
        errors = self.checker_errors(manifest_path)
        self.assertTrue(any("retained binary differs from canonical pin" in error for error in errors))
        self.assertTrue(any("binary attestation differs" in error for error in errors))

    def test_checker_rejects_stdout_substitution_with_matching_artifact_hashes(self) -> None:
        manifest_path = self.execute()
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        left = manifest["records"][1]["artifacts"]
        right = manifest["records"][2]["artifacts"]
        left["stdout_path"], right["stdout_path"] = right["stdout_path"], left["stdout_path"]
        left["stdout_sha256"], right["stdout_sha256"] = right["stdout_sha256"], left["stdout_sha256"]
        manifest_path.write_text(json.dumps(manifest, sort_keys=True) + "\n", encoding="utf-8")
        errors = self.checker_errors(manifest_path)
        self.assertTrue(any("stdout top-level engine differs from record" in error for error in errors))

    def test_checker_rejects_vector_substitution_with_matching_artifact_hashes(self) -> None:
        manifest_path = self.execute()
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        left = manifest["records"][1]["artifacts"]
        right = manifest["records"][2]["artifacts"]
        left["vector_artifact_path"], right["vector_artifact_path"] = right["vector_artifact_path"], left["vector_artifact_path"]
        left["vector_artifact_sha256"], right["vector_artifact_sha256"] = right["vector_artifact_sha256"], left["vector_artifact_sha256"]
        manifest_path.write_text(json.dumps(manifest, sort_keys=True) + "\n", encoding="utf-8")
        errors = self.checker_errors(manifest_path)
        self.assertTrue(any("EBV1 model id differs from record/runtime" in error for error in errors))

    def test_checker_rejects_self_consistent_one_vector_coverage_shrink(self) -> None:
        manifest_path = self.execute()
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        record = manifest["records"][1]
        vector_path = self.root / record["artifacts"]["vector_artifact_path"]
        write_vectors(vector_path, "minishlab/potion-retrieval-32M", 512, ["fact:a"])
        record["artifacts"]["vector_artifact_sha256"] = digest(vector_path)
        for field in ("vector_count", "vector_candidate_count", "resident_vector_count"):
            record["effective"][field] = 1
        stdout_path = self.root / record["artifacts"]["stdout_path"]
        stdout = json.loads(stdout_path.read_text(encoding="utf-8"))
        for field in ("vector_count", "vector_candidate_count", "resident_vector_count"):
            stdout["retrieval_engine"][field] = 1
        stdout_path.write_text(json.dumps(stdout, sort_keys=True) + "\n", encoding="utf-8")
        record["artifacts"]["stdout_sha256"] = digest(stdout_path)
        manifest_path.write_text(json.dumps(manifest, sort_keys=True) + "\n", encoding="utf-8")
        errors = self.checker_errors(manifest_path)
        self.assertTrue(any("vector_count does not cover exact active+eligible candidates" in error for error in errors))
        self.assertTrue(any("EBV1 fact IDs do not exactly cover active+eligible candidates" in error for error in errors))

    def test_checker_independently_rejects_every_non_finite_ebv1_float_class(self) -> None:
        manifest_path = self.execute()
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        record = manifest["records"][1]
        vector_path = self.root / record["artifacts"]["vector_artifact_path"]
        canonical = vector_path.read_bytes()
        model_length = struct.unpack_from("<H", canonical, 4)[0]
        first_fact_length_offset = 4 + 2 + model_length + 4 + 4
        first_fact_length = struct.unpack_from("<H", canonical, first_fact_length_offset)[0]
        first_float_offset = first_fact_length_offset + 2 + first_fact_length
        for value in (float("nan"), float("inf"), float("-inf")):
            with self.subTest(value=value):
                raw = bytearray(canonical)
                struct.pack_into("<f", raw, first_float_offset, value)
                vector_path.write_bytes(raw)
                record["artifacts"]["vector_artifact_sha256"] = digest(vector_path)
                manifest_path.write_text(json.dumps(manifest, sort_keys=True) + "\n", encoding="utf-8")
                errors = self.checker_errors(manifest_path)
                self.assertTrue(any("non-finite float" in error for error in errors))

    def test_checker_independently_rejects_truncated_and_trailing_ebv1_payloads(self) -> None:
        manifest_path = self.execute()
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        record = manifest["records"][1]
        vector_path = self.root / record["artifacts"]["vector_artifact_path"]
        canonical = vector_path.read_bytes()
        for payload in (canonical[:-1], canonical + b"x"):
            with self.subTest(size=len(payload)):
                vector_path.write_bytes(payload)
                record["artifacts"]["vector_artifact_sha256"] = digest(vector_path)
                manifest_path.write_text(json.dumps(manifest, sort_keys=True) + "\n", encoding="utf-8")
                errors = self.checker_errors(manifest_path)
                self.assertTrue(any("invalid EBV1 vector artifact" in error for error in errors))

    def test_recall_must_remain_active_through_during_health_request(self) -> None:
        recall_entered = VERIFY.threading.Event()
        observation_started = VERIFY.threading.Event()
        recall_threads = []
        self.embedding_delay_seconds = 0

        def instant_run(command: list[str], **kwargs: object) -> subprocess.CompletedProcess[bytes]:
            completed = self.fake_run(command, **kwargs)
            env = kwargs["env"]
            if env["ENTIRE_BRAIN_EMBEDDER"] == "ollama":
                recall_threads.append(VERIFY.threading.current_thread())
                recall_entered.set()
                self.assertTrue(observation_started.wait(timeout=5))
            return completed

        def await_completion() -> None:
            self.assertTrue(recall_entered.wait(timeout=5))
            observation_started.set()
            recall_threads[0].join(timeout=5)
            self.assertFalse(recall_threads[0].is_alive())

        self.during_observe_hook = await_completion
        with self.assertRaisesRegex(VERIFY.VerificationError, "did not remain active"):
            self.execute(run=instant_run)
        self.assertFalse((self.output / "engine-verification.json").exists())

    def test_checker_rejects_invalid_and_out_of_window_liveness_timestamps(self) -> None:
        manifest_path = self.execute()
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        record = manifest["records"][2]
        attestation_path = self.root / record["artifacts"]["embedding_server_attestation_path"]
        attestation = json.loads(attestation_path.read_text(encoding="utf-8"))
        during = next(item for item in attestation["observations"] if item["phase"] == "during_recall")
        during["observed_at"] = attestation["observations"][-1]["observed_at"]
        attestation_path.write_text(json.dumps(attestation, sort_keys=True) + "\n", encoding="utf-8")
        record["artifacts"]["embedding_server_attestation_sha256"] = digest(attestation_path)
        manifest_path.write_text(json.dumps(manifest, sort_keys=True) + "\n", encoding="utf-8")
        errors = self.checker_errors(manifest_path)
        self.assertTrue(any("does not strictly bound active recall" in error for error in errors))

        attestation["recall_window"]["started_at"] = "not-rfc3339"
        attestation_path.write_text(json.dumps(attestation, sort_keys=True) + "\n", encoding="utf-8")
        record["artifacts"]["embedding_server_attestation_sha256"] = digest(attestation_path)
        manifest_path.write_text(json.dumps(manifest, sort_keys=True) + "\n", encoding="utf-8")
        errors = self.checker_errors(manifest_path)
        self.assertTrue(any("recall start is not timezone-aware RFC3339" in error for error in errors))

    def test_fixture_evidence_is_rejected_by_production_checker(self) -> None:
        manifest_path = self.execute()
        matrix = json.loads((HERE / "engine-matrix.json").read_text(encoding="utf-8"))
        errors = VERIFY.check_protocol.validate_engine_verification(
            matrix,
            {"status": "pass", "evidence": manifest_path.relative_to(self.root).as_posix()},
            here=self.root,
            repo=self.root,
            pin_repo=VERIFY.REPO,
        )
        self.assertTrue(any("pin descriptor is not authoritative" in error for error in errors))

    def test_fallback_refuses_manifest(self) -> None:
        def fallback_run(command: list[str], **kwargs: object) -> subprocess.CompletedProcess[bytes]:
            completed = self.fake_run(command, **kwargs)
            payload = json.loads(completed.stdout)
            if payload["effective_engine"] == "model2vec_rrf":
                payload["retrieval_engine"]["fallback_used"] = True
            return subprocess.CompletedProcess(command, 0, json.dumps(payload).encode(), b"")

        with self.assertRaisesRegex(VERIFY.VerificationError, "used a fallback"):
            self.execute(fallback_run)
        self.assertFalse((self.output / "engine-verification.json").exists())

    def test_derived_facts_mutation_after_recall_refuses_manifest(self) -> None:
        def mutating_run(command: list[str], **kwargs: object) -> subprocess.CompletedProcess[bytes]:
            completed = self.fake_run(command, **kwargs)
            env = kwargs["env"]
            if env["ENTIRE_BRAIN_EMBEDDER"] == "":
                next(pathlib.Path(env["ENTIRE_PLUGIN_DATA_DIR"]).rglob("facts.ndjson")).write_text(
                    "tampered\n", encoding="utf-8"
                )
            return completed

        with self.assertRaisesRegex(VERIFY.VerificationError, "derived facts after recall SHA-256 mismatch"):
            self.execute(mutating_run)
        self.assertFalse((self.output / "engine-verification.json").exists())

    def test_checker_rehashes_retained_sources_and_derived_facts(self) -> None:
        manifest_path = self.execute()
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        facts_source = self.root / manifest["records"][0]["artifacts"]["facts_source_path"]
        facts_source.write_text("tampered retained source\n", encoding="utf-8")
        errors = self.checker_errors(manifest_path)
        self.assertTrue(any("facts_source artifact: content hash mismatch" in error for error in errors))

        facts_source.write_bytes(self.facts.read_bytes())
        derived = self.root / manifest["records"][1]["artifacts"]["derived_facts_path"]
        derived.write_text("tampered derived facts\n", encoding="utf-8")
        errors = self.checker_errors(manifest_path)
        self.assertTrue(any("derived_facts artifact: content hash mismatch" in error for error in errors))

    def test_invalid_attestation_fails_atomic_publish_without_temp_manifest(self) -> None:
        self.attestation_mutator = lambda attestation: attestation["observations"].pop()
        with self.assertRaisesRegex(VERIFY.VerificationError, "generated evidence failed"):
            self.execute()
        self.assertFalse((self.output / "engine-verification.json").exists())
        self.assertEqual(list(self.output.glob(".engine-verification.json.*.tmp")), [])

    def test_checker_rejects_server_pid_and_phase_discontinuity(self) -> None:
        manifest_path = self.execute()
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        record = manifest["records"][2]
        attestation_path = self.root / record["artifacts"]["embedding_server_attestation_path"]
        attestation = json.loads(attestation_path.read_text(encoding="utf-8"))
        attestation["observations"][1]["pid"] += 1
        attestation["observations"][-1]["phase"] = "during_recall"
        attestation_path.write_text(json.dumps(attestation, sort_keys=True) + "\n", encoding="utf-8")
        record["artifacts"]["embedding_server_attestation_sha256"] = digest(attestation_path)
        manifest_path.write_text(json.dumps(manifest, sort_keys=True) + "\n", encoding="utf-8")
        errors = self.checker_errors(manifest_path)
        self.assertTrue(any("PID changed" in error for error in errors))
        self.assertTrue(any("final health attestation is not post-recall" in error for error in errors))

    def test_semantic_vector_path_must_stay_in_derived_data_root(self) -> None:
        outside = self.root / "outside.vectors.bin"

        def escaped_run(command: list[str], **kwargs: object) -> subprocess.CompletedProcess[bytes]:
            completed = self.fake_run(command, **kwargs)
            payload = json.loads(completed.stdout)
            if payload["effective_engine"] == "model2vec_rrf":
                write_vectors(outside, "minishlab/potion-retrieval-32M", 512, ["fact:a", "fact:b"])
                payload["retrieval_engine"]["vector_cache_path"] = str(outside)
            return subprocess.CompletedProcess(command, 0, json.dumps(payload).encode(), b"")

        with self.assertRaisesRegex(VERIFY.VerificationError, "escaped its derived data root"):
            self.execute(escaped_run)

    def test_vector_header_must_match_runtime_identity(self) -> None:
        def mismatched_run(command: list[str], **kwargs: object) -> subprocess.CompletedProcess[bytes]:
            completed = self.fake_run(command, **kwargs)
            payload = json.loads(completed.stdout)
            if payload["effective_engine"] == "model2vec_rrf":
                path = pathlib.Path(payload["retrieval_engine"]["vector_cache_path"])
                write_vectors(path, "wrong-model", 512, ["fact:a", "fact:b"])
            return subprocess.CompletedProcess(command, 0, json.dumps(payload).encode(), b"")

        with self.assertRaisesRegex(VERIFY.VerificationError, "vector header model"):
            self.execute(mismatched_run)

    def test_canonical_pin_mismatch_refuses_before_output(self) -> None:
        pins = copy.deepcopy(self.pins)
        pins["corpus"]["facts_sha256"] = "0" * 64
        with self.assertRaisesRegex(VERIFY.VerificationError, "frozen facts SHA-256 mismatch"):
            self.execute(pins=pins)
        self.assertFalse(self.output.exists())

    def test_binary_pin_mismatch_refuses_before_output(self) -> None:
        self.binary.write_bytes(b"substituted before execution\n")
        with self.assertRaisesRegex(VERIFY.VerificationError, "canonical entire-brain binary SHA-256 mismatch"):
            self.execute()
        self.assertFalse(self.output.exists())

    def test_non_exposed_query_is_rejected_before_output(self) -> None:
        self.settings = VERIFY.dataclasses.replace(
            self.settings,
            query_id="fresh-holdout:query-1",
            query="a query from a sealed holdout",
        )
        with self.assertRaisesRegex(VERIFY.VerificationError, "not in the canonical development pin set"):
            self.execute()
        self.assertFalse(self.output.exists())

    def test_cli_has_no_caller_controlled_expected_values(self) -> None:
        self.assertNotIn("expected_facts_sha256", VERIFY.Settings.__dataclass_fields__)
        args = [
            "--binary", str(self.binary),
            "--frozen-data-dir", str(self.data),
            "--frozen-config-dir", str(self.config),
            "--frozen-state-dir", str(self.state),
            "--frozen-facts", str(self.facts),
            "--repo-root", str(self.repo),
            "--session-dates", str(self.sessions),
            "--query-id", self.settings.query_id,
            "--query", self.settings.query,
            "--eligible-before", self.settings.eligible_before,
            "--embedding-model", str(self.model),
            "--node-runtime", str(self.node),
            "--runtime-dependency-root", str(self.dependencies),
            "--output-dir", str(self.output),
            "--expected-facts-sha256", "0" * 64,
        ]
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            VERIFY.parse_args(args)

    def test_occupied_endpoint_is_rejected_as_unowned(self) -> None:
        listener = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        listener.bind(("127.0.0.1", 0))
        port = listener.getsockname()[1]
        try:
            with self.assertRaisesRegex(VERIFY.VerificationError, "already occupied"):
                VERIFY.assert_pinned_loopback_endpoint_available(f"http://127.0.0.1:{port}")
        finally:
            listener.close()

    def test_vector_parser_rejects_trailing_bytes(self) -> None:
        path = self.root / "vectors.bin"
        write_vectors(path, "model", 2, ["fact:a"])
        path.write_bytes(path.read_bytes() + b"x")
        with self.assertRaisesRegex(VERIFY.VerificationError, "trailing bytes"):
            VERIFY.parse_vector_artifact(path)

    def test_health_monitor_binds_fresh_requests_to_a_real_owned_process(self) -> None:
        process = subprocess.Popen(
            [sys.executable, "-c", "import time; time.sleep(5)"],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        counter = 0

        def fake_request(request: object, timeout: float = 3) -> dict[str, object]:
            nonlocal counter
            del timeout
            counter += 1
            query = VERIFY.urllib.parse.parse_qs(VERIFY.urllib.parse.urlparse(request.full_url).query)
            return {
                "pid": process.pid,
                "verification_token": "owned-token",
                "request_nonce": query["nonce"][0],
                "health_request_count": counter,
                "model_path": str(self.model.resolve()),
                "model_sha256": self.pins["embedding_model"]["sha256"],
                "embedding_dimension": self.pins["embedding_model"]["dimension"],
                "node_version": self.pins["runtime"]["node_version"],
            }

        try:
            monitor = VERIFY.HealthMonitor(
                process,
                self.settings.embed_url,
                "owned-token",
                self.pins,
                self.model,
                1.0,
            )
            with mock.patch.object(VERIFY, "request_json", side_effect=fake_request):
                monitor.start()
                monitor.observe("during_recall")
                monitor.finish()
            self.assertEqual([item["phase"] for item in monitor.observations], ["pre_recall", "during_recall", "post_recall"])
            self.assertEqual([item["health_request_count"] for item in monitor.observations], [1, 2, 3])
            self.assertEqual(len({item["request_nonce_sha256"] for item in monitor.observations}), 3)
        finally:
            process.terminate()
            process.wait(timeout=2)


if __name__ == "__main__":
    unittest.main()
