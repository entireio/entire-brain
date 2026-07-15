from __future__ import annotations

import contextlib
import hashlib
import importlib.util
import json
import pathlib
import socket
import struct
import subprocess
import sys
import tempfile
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
        self.facts = self.data / "repos/local/example/facts/main-0d6e4079/facts.ndjson"
        self.facts.parent.mkdir(parents=True)
        self.facts.write_text('{"id":"fact:a"}\n', encoding="utf-8")
        taxonomy = self.data / "repos/local/example/facts/taxonomy.json"
        taxonomy.write_text('{"categories":{},"paths":[]}\n', encoding="utf-8")
        manifest = self.data / "repos/local/example/manifest.json"
        manifest.write_text('{"schema_version":3}\n', encoding="utf-8")
        self.sessions = self.frozen / "session_dates.json"
        self.sessions.write_text('{"session-1":"2026-01-01T00:00:00Z"}\n', encoding="utf-8")
        self.binary = self.root / "input/entire-brain"
        self.binary.parent.mkdir(parents=True)
        self.binary.write_bytes(b"fake binary\n")
        self.binary.chmod(0o755)
        self.model = self.root / "input/embeddinggemma.gguf"
        self.model.write_bytes(b"fake embedding model\n")
        self.server_script = self.root / "input/embed-server.mjs"
        self.server_script.write_text("// fake\n", encoding="utf-8")
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
            query_id="entire-cli-c0701-6699ec40a:brain-query-1",
            query="Pi review cache tokens are double counted",
            branch="main",
            k=5,
            eligible_before="2026-07-01T19:32:18+02:00",
            exclude_session_ids=("019f1893-b810-7992-afb4-8c4bddc4ae3c",),
            expected_facts_sha256=digest(self.facts),
            expected_session_dates_sha256=digest(self.sessions),
            expected_prefilter_count=3,
            expected_eligible_count=2,
            embedding_model=self.model,
            expected_embedding_model_sha256=digest(self.model),
            embed_url="http://127.0.0.1:11500",
            node_binary="node",
            embed_server_script=self.server_script,
            server_start_timeout_seconds=1,
            output_dir=self.output,
            artifact_root=self.root,
        )

    def tearDown(self) -> None:
        self.temp.cleanup()

    def fake_run(self, command: list[str], **kwargs: object) -> subprocess.CompletedProcess[bytes]:
        env = kwargs["env"]
        assert isinstance(env, dict)
        lexical = "--no-semantic" in command
        embeddinggemma = env["ENTIRE_BRAIN_EMBEDDER"] == "ollama"
        arm = "lexical_handrolled" if lexical else "embeddinggemma_rrf" if embeddinggemma else "model2vec_rrf"
        semantic = not lexical
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
            model_id = VERIFY.EXPECTED_EMBEDDERS[arm]
            dimension = VERIFY.EXPECTED_DIMENSIONS[arm]
            data_dir = pathlib.Path(env["ENTIRE_PLUGIN_DATA_DIR"])
            derived_facts = next(data_dir.rglob("facts.ndjson"))
            vectors = derived_facts.parent / "embeddings/vectors.bin"
            write_vectors(vectors, model_id, dimension, ["fact:a", "fact:b"])
            engine.update(
                {
                    "embedder_id": model_id,
                    "embedding_dimension": dimension,
                    "vector_count": 2,
                    "resident_vector_count": 2,
                    "vector_cache_backend": "flat_file",
                    "vector_cache_path": str(vectors),
                    "vector_cache_read_only": False,
                }
            )
        payload = {
            "effective_engine": arm,
            "retrieval_engine": engine,
            "eligibility": {
                "prefilter_corpus_count": 3,
                "eligible_count": 2,
                "excluded_counts": {"at_or_after_cutoff": 1},
                "delivered_count": 1,
            },
            "facts": [{"id": "fact:a"}],
        }
        return subprocess.CompletedProcess(command, 0, json.dumps(payload).encode(), b"")

    @contextlib.contextmanager
    def fake_server(self, settings: object, retained_model: pathlib.Path):
        self.assertEqual(digest(retained_model), self.settings.expected_embedding_model_sha256)
        run_dir = self.output / "runs/embeddinggemma_rrf"
        run_dir.mkdir(parents=True, exist_ok=True)
        stdout = run_dir / "server.stdout.txt"
        stderr = run_dir / "server.stderr.txt"
        stdout.write_text("ready: dim = 768\n", encoding="utf-8")
        stderr.write_bytes(b"")
        yield VERIFY.ServerEvidence(
            ("node", str(self.server_script)),
            {"GGUF": str(retained_model), "HOST": "127.0.0.1", "PORT": "11500"},
            stdout,
            stderr,
        )

    def execute(self, run=None) -> pathlib.Path:
        with mock.patch.object(VERIFY, "assert_pinned_loopback_endpoint_available", return_value=("127.0.0.1", 11500)):
            return VERIFY.execute(
                self.settings,
                run_command=run or self.fake_run,
                server_context=self.fake_server,
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
        data_roots = [record["requested"]["environment"]["ENTIRE_PLUGIN_DATA_DIR"] for record in records]
        self.assertEqual(len(set(data_roots)), 3)
        vectors = [record["artifacts"]["vector_artifact_path"] for record in records[1:]]
        self.assertEqual(len(set(vectors)), 2)
        matrix = json.loads((HERE / "engine-matrix.json").read_text(encoding="utf-8"))
        relative = manifest_path.relative_to(self.root).as_posix()
        self.assertEqual(
            VERIFY.check_protocol.validate_engine_verification(
                matrix,
                {"status": "pass", "evidence": relative},
                here=self.root,
                repo=self.root,
            ),
            [],
        )

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

    def test_semantic_vector_path_must_stay_in_derived_data_root(self) -> None:
        outside = self.root / "outside.vectors.bin"

        def escaped_run(command: list[str], **kwargs: object) -> subprocess.CompletedProcess[bytes]:
            completed = self.fake_run(command, **kwargs)
            payload = json.loads(completed.stdout)
            if payload["effective_engine"] == "model2vec_rrf":
                write_vectors(outside, VERIFY.EXPECTED_EMBEDDERS["model2vec_rrf"], 512, ["fact:a", "fact:b"])
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

    def test_hash_pin_mismatch_refuses_before_output(self) -> None:
        changed = dataclass_replace(self.settings, expected_facts_sha256="0" * 64)
        with mock.patch.object(VERIFY, "assert_pinned_loopback_endpoint_available"):
            with self.assertRaisesRegex(VERIFY.VerificationError, "frozen facts SHA-256 mismatch"):
                VERIFY.execute(changed, run_command=self.fake_run, server_context=self.fake_server)
        self.assertFalse(self.output.exists())

    def test_non_exposed_query_is_rejected_before_output(self) -> None:
        changed = dataclass_replace(
            self.settings,
            query_id="fresh-holdout:query-1",
            query="a query from a sealed holdout",
        )
        with mock.patch.object(VERIFY, "assert_pinned_loopback_endpoint_available"):
            with self.assertRaisesRegex(VERIFY.VerificationError, "not from exposed development task"):
                VERIFY.execute(changed, run_command=self.fake_run, server_context=self.fake_server)
        self.assertFalse(self.output.exists())

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


def dataclass_replace(settings: object, **changes: object):
    return VERIFY.dataclasses.replace(settings, **changes)


if __name__ == "__main__":
    unittest.main()
