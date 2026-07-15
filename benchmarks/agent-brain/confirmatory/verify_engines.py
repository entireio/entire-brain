#!/usr/bin/env python3
"""Create fail-closed, byte-verifiable evidence for the three retrieval engines.

This runner never invokes a coding agent.  It copies the frozen facts into one
derived plugin data root per arm, forces a clean semantic cache build, and only
publishes the three-record manifest after every runtime observation and retained
artifact has passed the protocol checker's machine contract.
"""

from __future__ import annotations

import argparse
import contextlib
import dataclasses
import hashlib
import json
import os
import pathlib
import shutil
import socket
import struct
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from collections.abc import Callable, Iterator, Sequence
from typing import Any


HERE = pathlib.Path(__file__).resolve().parent
REPO = HERE.parents[2]
if str(HERE) not in sys.path:
    sys.path.insert(0, str(HERE))

import check_protocol  # noqa: E402


ARMS = ("lexical_handrolled", "model2vec_rrf", "embeddinggemma_rrf")
EXPECTED_EMBEDDERS = {
    "model2vec_rrf": "minishlab/potion-retrieval-32M",
    "embeddinggemma_rrf": "ollama:embeddinggemma",
}
EXPECTED_DIMENSIONS = {"model2vec_rrf": 512, "embeddinggemma_rrf": 768}
SAFE_PARENT_ENV = ("HOME", "LANG", "LC_ALL", "PATH", "TMPDIR")
EXPOSED_DEVELOPMENT_TASK_ID = "entire-cli-c0701-6699ec40a"
EXPOSED_DEVELOPMENT_TASK = REPO / "benchmarks" / "agent-brain" / "mined-c0701" / "dev" / f"{EXPOSED_DEVELOPMENT_TASK_ID}.json"


class VerificationError(RuntimeError):
    """Raised when evidence cannot be established without inference."""


@dataclasses.dataclass(frozen=True)
class Settings:
    binary: pathlib.Path
    frozen_data_dir: pathlib.Path
    frozen_config_dir: pathlib.Path
    frozen_state_dir: pathlib.Path
    frozen_facts: pathlib.Path
    repo_root: pathlib.Path
    session_dates: pathlib.Path
    query_id: str
    query: str
    branch: str
    k: int
    eligible_before: str
    exclude_session_ids: tuple[str, ...]
    expected_facts_sha256: str
    expected_session_dates_sha256: str
    expected_prefilter_count: int
    expected_eligible_count: int
    embedding_model: pathlib.Path
    expected_embedding_model_sha256: str
    embed_url: str
    node_binary: str
    embed_server_script: pathlib.Path
    server_start_timeout_seconds: float
    output_dir: pathlib.Path
    artifact_root: pathlib.Path


@dataclasses.dataclass(frozen=True)
class PreparedArm:
    arm: str
    data_dir: pathlib.Path
    config_dir: pathlib.Path
    state_dir: pathlib.Path
    cache_dir: pathlib.Path
    facts: pathlib.Path


@dataclasses.dataclass(frozen=True)
class ServerEvidence:
    command: tuple[str, ...]
    environment: dict[str, str]
    stdout_path: pathlib.Path
    stderr_path: pathlib.Path


RunCommand = Callable[..., subprocess.CompletedProcess[bytes]]


def sha256_file(path: pathlib.Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def require(condition: bool, message: str) -> None:
    if not condition:
        raise VerificationError(message)


def require_sha256(actual: str, expected: str, label: str) -> None:
    require(actual == expected, f"{label} SHA-256 mismatch: got {actual}, want {expected}")


def artifact_path(path: pathlib.Path, root: pathlib.Path) -> str:
    resolved = path.resolve()
    try:
        return resolved.relative_to(root.resolve()).as_posix()
    except ValueError as exc:
        raise VerificationError(f"artifact is outside the declared artifact root: {path}") from exc


def copy_file(source: pathlib.Path, target: pathlib.Path) -> None:
    require(source.is_file() and not source.is_symlink(), f"required regular file is missing: {source}")
    target.parent.mkdir(parents=True, exist_ok=True)
    shutil.copy2(source, target)


def copy_small_tree(source: pathlib.Path, target: pathlib.Path) -> None:
    require(source.is_dir() and not source.is_symlink(), f"required directory is missing: {source}")
    for path in source.rglob("*"):
        require(not path.is_symlink(), f"frozen seed directory contains a symlink: {path}")
    shutil.copytree(source, target)


def frozen_layout(settings: Settings) -> tuple[pathlib.Path, pathlib.Path, pathlib.Path]:
    data = settings.frozen_data_dir.resolve()
    facts = settings.frozen_facts.resolve()
    try:
        relative_facts = facts.relative_to(data)
    except ValueError as exc:
        raise VerificationError("frozen facts must be below --frozen-data-dir") from exc
    parts = relative_facts.parts
    require("facts" in parts, "frozen facts path has no facts/ directory")
    facts_index = parts.index("facts")
    require(parts[-1] == "facts.ndjson", "frozen fact source must be a facts.ndjson file")
    require(facts_index > 0, "frozen facts path does not identify a brain directory")
    brain_relative = pathlib.Path(*parts[:facts_index])
    branch_facts_relative = pathlib.Path(*parts[facts_index:])
    return brain_relative, branch_facts_relative, relative_facts


def validate_exposed_development_query(settings: Settings) -> None:
    task = json.loads(EXPOSED_DEVELOPMENT_TASK.read_text(encoding="utf-8"))
    require(task.get("id") == EXPOSED_DEVELOPMENT_TASK_ID, "exposed development task identity changed")
    queries = task.get("brain_queries")
    require(isinstance(queries, list) and settings.query in queries, "query is not from exposed development task 6699ec40a")
    query_index = queries.index(settings.query) + 1
    expected_query_id = f"{EXPOSED_DEVELOPMENT_TASK_ID}:brain-query-{query_index}"
    require(settings.query_id == expected_query_id, f"query id mismatch: got {settings.query_id}, want {expected_query_id}")
    require(settings.eligible_before == task.get("rolling_cutoff_rfc3339"), "development task temporal cutoff changed")
    expected_exclusions = task.get("exclude_session_ids")
    require(
        isinstance(expected_exclusions, list)
        and sorted(settings.exclude_session_ids) == sorted(expected_exclusions),
        "development task excluded-session set changed",
    )


def prepare_arm(settings: Settings, arm: str) -> PreparedArm:
    require(arm in ARMS, f"unknown arm: {arm}")
    runtime = settings.output_dir / "runtime" / arm
    data_dir = runtime / "data"
    config_dir = runtime / "config"
    state_dir = runtime / "state"
    cache_dir = runtime / "cache"
    require(not runtime.exists(), f"arm runtime already exists: {runtime}")

    brain_relative, branch_facts_relative, relative_facts = frozen_layout(settings)
    target_facts = data_dir / relative_facts
    copy_file(settings.frozen_facts, target_facts)

    frozen_brain = settings.frozen_data_dir / brain_relative
    target_brain = data_dir / brain_relative
    for relative in (pathlib.Path("manifest.json"), pathlib.Path("facts") / "taxonomy.json"):
        source = frozen_brain / relative
        if source.exists():
            copy_file(source, target_brain / relative)

    # No arm inherits a semantic artifact.  Both semantic arms must create and
    # retain their own vectors from the same copied fact bytes.
    embeddings = target_brain / branch_facts_relative.parent / "embeddings"
    require(not embeddings.exists(), f"derived arm unexpectedly inherited vectors: {embeddings}")

    copy_small_tree(settings.frozen_config_dir, config_dir)
    copy_small_tree(settings.frozen_state_dir, state_dir)
    cache_dir.mkdir(parents=True)
    require_sha256(sha256_file(target_facts), settings.expected_facts_sha256, f"{arm} copied facts")
    return PreparedArm(arm, data_dir, config_dir, state_dir, cache_dir, target_facts)


def safe_runtime_environment(settings: Settings, prepared: PreparedArm, arm: str) -> dict[str, str]:
    env = {key: os.environ[key] for key in SAFE_PARENT_ENV if key in os.environ}
    env.update(
        {
            "ENTIRE_BRAIN_FACTS_BM25": "0",
            "ENTIRE_BRAIN_EMBEDDER": "ollama" if arm == "embeddinggemma_rrf" else "",
            "ENTIRE_BRAIN_EMBED_URL": settings.embed_url if arm == "embeddinggemma_rrf" else "",
            "ENTIRE_PLUGIN_CONFIG_DIR": str(prepared.config_dir.resolve()),
            "ENTIRE_PLUGIN_DATA_DIR": str(prepared.data_dir.resolve()),
            "ENTIRE_PLUGIN_STATE_DIR": str(prepared.state_dir.resolve()),
            "ENTIRE_PLUGIN_CACHE_DIR": str(prepared.cache_dir.resolve()),
            "ENTIRE_REPO_ROOT": str(settings.repo_root.resolve()),
            "ENGINE_VERIFICATION_FACTS_SHA256": settings.expected_facts_sha256,
            "ENGINE_VERIFICATION_SESSION_DATES_SHA256": settings.expected_session_dates_sha256,
        }
    )
    return env


def recall_command(settings: Settings, retained_binary: pathlib.Path, arm: str) -> list[str]:
    command = [
        str(retained_binary.resolve()),
        "recall",
        settings.query,
        "--branch",
        settings.branch,
        "--k",
        str(settings.k),
        "--eligible-before",
        settings.eligible_before,
        "--session-dates",
        str(settings.session_dates.resolve()),
    ]
    for session_id in settings.exclude_session_ids:
        command.extend(("--exclude-session-id", session_id))
    if arm == "lexical_handrolled":
        command.append("--no-semantic")
    command.append("--json")
    return command


def parse_vector_artifact(path: pathlib.Path) -> tuple[str, int, int]:
    raw = path.read_bytes()
    require(raw[:4] == b"EBV1", f"unsupported or corrupt vector artifact magic: {path}")
    offset = 4

    def take(fmt: str) -> tuple[int, ...]:
        nonlocal offset
        size = struct.calcsize(fmt)
        require(offset + size <= len(raw), f"truncated vector artifact header: {path}")
        values = struct.unpack_from(fmt, raw, offset)
        offset += size
        return values

    (model_len,) = take("<H")
    require(offset + model_len <= len(raw), f"truncated vector model id: {path}")
    model_id = raw[offset : offset + model_len].decode("utf-8")
    offset += model_len
    (dimension,) = take("<I")
    (count,) = take("<I")
    require(dimension > 0, f"vector artifact has a zero dimension: {path}")
    seen: set[str] = set()
    for _ in range(count):
        (fact_id_len,) = take("<H")
        require(offset + fact_id_len <= len(raw), f"truncated vector fact id: {path}")
        fact_id = raw[offset : offset + fact_id_len].decode("utf-8")
        offset += fact_id_len
        require(fact_id not in seen, f"duplicate vector fact id {fact_id!r}: {path}")
        seen.add(fact_id)
        vector_bytes = dimension * 4
        require(offset + vector_bytes <= len(raw), f"truncated vector values for {fact_id!r}: {path}")
        offset += vector_bytes
    require(offset == len(raw), f"vector artifact has trailing bytes: {path}")
    return model_id, dimension, count


def parse_runtime_output(raw: bytes, arm: str, settings: Settings, prepared: PreparedArm) -> dict[str, Any]:
    try:
        output = json.loads(raw)
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise VerificationError(f"{arm} stdout is not one JSON object") from exc
    require(isinstance(output, dict), f"{arm} stdout must be a JSON object")
    engine = output.get("retrieval_engine")
    require(isinstance(engine, dict), f"{arm} output has no retrieval_engine object")
    require(output.get("effective_engine") == arm, f"{arm} top-level effective engine mismatch")
    require(engine.get("effective_engine") == arm, f"{arm} runtime effective engine mismatch")
    require(engine.get("identity_verified") is True, f"{arm} runtime identity is not verified")
    require(engine.get("bm25_enabled") is False, f"{arm} unexpectedly enabled BM25")
    require(engine.get("fallback_used") is False, f"{arm} used a fallback")
    semantic = arm != "lexical_handrolled"
    require(engine.get("semantic_requested") is semantic, f"{arm} semantic request state mismatch")
    require(engine.get("semantic_available") is semantic, f"{arm} semantic availability mismatch")
    if semantic:
        require(engine.get("semantic_applied") is True, f"{arm} did not apply semantic ranking")
        require(engine.get("embedder_id") == EXPECTED_EMBEDDERS[arm], f"{arm} embedder identity mismatch")
        require(engine.get("embedding_dimension") == EXPECTED_DIMENSIONS[arm], f"{arm} dimension mismatch")
        require(engine.get("vector_cache_read_only") is False, f"{arm} vector cache was read-only")
        require(engine.get("vector_cache_backend") == "flat_file", f"{arm} did not use the retained flat-file cache")
        require(isinstance(engine.get("vector_count"), int) and engine["vector_count"] > 0, f"{arm} vector count is invalid")
    else:
        require(not engine.get("embedder_id"), "lexical arm reported an embedder")
        require(engine.get("embedding_dimension") is None, "lexical arm reported an embedding dimension")

    eligibility = output.get("eligibility")
    require(isinstance(eligibility, dict), f"{arm} output has no temporal eligibility audit")
    require(
        eligibility.get("prefilter_corpus_count") == settings.expected_prefilter_count,
        f"{arm} prefilter count mismatch",
    )
    require(eligibility.get("eligible_count") == settings.expected_eligible_count, f"{arm} eligible count mismatch")
    excluded = eligibility.get("excluded_counts")
    require(isinstance(excluded, dict), f"{arm} excluded counts are invalid")
    require(
        all(isinstance(value, int) and not isinstance(value, bool) and value >= 0 for value in excluded.values()),
        f"{arm} excluded counts contain an invalid value",
    )
    require(sum(excluded.values()) == settings.expected_prefilter_count - settings.expected_eligible_count, f"{arm} eligibility counts do not reconcile")
    facts = output.get("facts")
    require(isinstance(facts, list), f"{arm} facts result is not an array")
    fact_ids = [fact.get("id") if isinstance(fact, dict) else None for fact in facts]
    require(all(isinstance(fact_id, str) and fact_id for fact_id in fact_ids), f"{arm} result contains an invalid fact id")
    require(len(fact_ids) == len(set(fact_ids)), f"{arm} result contains duplicate fact ids")
    require(eligibility.get("delivered_count") == len(fact_ids), f"{arm} delivered count mismatch")
    require(len(fact_ids) <= settings.k, f"{arm} delivered more than k facts")

    if semantic:
        cache_path_raw = engine.get("vector_cache_path")
        require(isinstance(cache_path_raw, str) and cache_path_raw, f"{arm} vector cache path is missing")
        cache_path = pathlib.Path(cache_path_raw).resolve()
        try:
            cache_path.relative_to(prepared.data_dir.resolve())
        except ValueError as exc:
            raise VerificationError(f"{arm} vector cache escaped its derived data root: {cache_path}") from exc
        require(cache_path.is_file() and not cache_path.is_symlink(), f"{arm} vector artifact was not persisted")
        model_id, dimension, resident_count = parse_vector_artifact(cache_path)
        require(model_id == engine.get("embedder_id"), f"{arm} vector header model does not match runtime")
        require(dimension == engine.get("embedding_dimension"), f"{arm} vector header dimension does not match runtime")
        require(resident_count == engine.get("resident_vector_count"), f"{arm} vector header count does not match runtime")
    return output


def hashed_artifact(path: pathlib.Path, root: pathlib.Path) -> tuple[str, str]:
    return artifact_path(path, root), sha256_file(path)


def run_arm(
    settings: Settings,
    matrix_arm: dict[str, Any],
    prepared: PreparedArm,
    retained_binary: pathlib.Path,
    retained_model: pathlib.Path,
    *,
    run_command: RunCommand = subprocess.run,
) -> dict[str, Any]:
    arm = prepared.arm
    run_dir = settings.output_dir / "runs" / arm
    run_dir.mkdir(parents=True, exist_ok=True)
    stdout_path = run_dir / "recall.stdout.json"
    stderr_path = run_dir / "recall.stderr.txt"
    command = recall_command(settings, retained_binary, arm)
    environment = safe_runtime_environment(settings, prepared, arm)
    completed = run_command(command, cwd=settings.repo_root, env=environment, capture_output=True, check=False)
    require(isinstance(completed.stdout, bytes) and isinstance(completed.stderr, bytes), "runner must capture byte output")
    stdout_path.write_bytes(completed.stdout)
    stderr_path.write_bytes(completed.stderr)
    require(completed.returncode == 0, f"{arm} recall exited {completed.returncode}; see {stderr_path}")
    output = parse_runtime_output(completed.stdout, arm, settings, prepared)
    runtime_engine = output["retrieval_engine"]

    vector_path: str | None = None
    vector_hash: str | None = None
    if arm != "lexical_handrolled":
        source_vector = pathlib.Path(runtime_engine["vector_cache_path"])
        retained_vector = settings.output_dir / "artifacts" / "vectors" / f"{arm}.vectors.bin"
        copy_file(source_vector, retained_vector)
        vector_path, vector_hash = hashed_artifact(retained_vector, settings.artifact_root)

    binary_path, binary_hash = hashed_artifact(retained_binary, settings.artifact_root)
    stdout_rel, stdout_hash = hashed_artifact(stdout_path, settings.artifact_root)
    stderr_rel, stderr_hash = hashed_artifact(stderr_path, settings.artifact_root)
    model_path: str | None = None
    model_hash: str | None = None
    if arm == "embeddinggemma_rrf":
        model_path, model_hash = hashed_artifact(retained_model, settings.artifact_root)

    eligibility = output["eligibility"]
    facts = output["facts"]
    semantic = arm != "lexical_handrolled"
    return {
        "schema_version": 1,
        "arm": arm,
        "requested": {
            "command": command,
            "environment": environment,
            "namespace": matrix_arm["namespace"],
        },
        "effective": {
            "engine": runtime_engine["effective_engine"],
            "semantic_available": runtime_engine["semantic_available"],
            "bm25_enabled": runtime_engine["bm25_enabled"],
            "fallback_used": runtime_engine["fallback_used"],
            "embedder_id": runtime_engine.get("embedder_id") if semantic else None,
            "embedding_dimension": runtime_engine.get("embedding_dimension") if semantic else None,
            "vector_count": runtime_engine.get("vector_count", 0) if semantic else 0,
            "vector_namespace": matrix_arm["namespace"],
        },
        "artifacts": {
            "binary_path": binary_path,
            "binary_sha256": binary_hash,
            "stdout_path": stdout_rel,
            "stdout_sha256": stdout_hash,
            "stderr_path": stderr_rel,
            "stderr_sha256": stderr_hash,
            "vector_artifact_path": vector_path,
            "vector_artifact_sha256": vector_hash,
            "embedding_model_path": model_path,
            "embedding_model_sha256": model_hash,
        },
        "corpus": {
            "facts_sha256": settings.expected_facts_sha256,
            "prefilter_count": eligibility["prefilter_corpus_count"],
            "eligible_count": eligibility["eligible_count"],
            "excluded_by_reason": eligibility["excluded_counts"],
            "delivered_count": eligibility["delivered_count"],
        },
        "result": {
            "query_id": settings.query_id,
            "fact_ids_in_order": [fact["id"] for fact in facts],
            "output_valid": True,
        },
    }


def assert_pinned_loopback_endpoint_available(embed_url: str) -> tuple[str, int]:
    parsed = urllib.parse.urlparse(embed_url)
    require(parsed.scheme == "http", "embedding endpoint must use loopback HTTP")
    require(parsed.hostname == "127.0.0.1", "embedding endpoint must be pinned to 127.0.0.1")
    require(parsed.port is not None, "embedding endpoint must include a port")
    probe = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    try:
        probe.bind((parsed.hostname, parsed.port))
    except OSError as exc:
        raise VerificationError(
            f"pinned embedding endpoint {parsed.hostname}:{parsed.port} is already occupied; "
            "refusing to reuse an unowned server as attributable evidence"
        ) from exc
    finally:
        probe.close()
    return parsed.hostname, parsed.port


def wait_for_server(url: str, process: subprocess.Popen[bytes], timeout_seconds: float) -> None:
    deadline = time.monotonic() + timeout_seconds
    body = json.dumps({"model": "embeddinggemma", "input": "title: none | text: verification probe"}).encode()
    last_error = "no response"
    while time.monotonic() < deadline:
        if process.poll() is not None:
            raise VerificationError(f"controlled embedding server exited early with code {process.returncode}")
        try:
            request = urllib.request.Request(url, data=body, headers={"Content-Type": "application/json"})
            with urllib.request.urlopen(request, timeout=2) as response:  # noqa: S310 - URL is loopback-pinned above
                result = json.load(response)
            embeddings = result.get("embeddings") if isinstance(result, dict) else None
            if (
                isinstance(embeddings, list)
                and embeddings
                and isinstance(embeddings[0], list)
                and len(embeddings[0]) == EXPECTED_DIMENSIONS["embeddinggemma_rrf"]
            ):
                return
            last_error = "server returned an invalid embedding shape"
        except (OSError, urllib.error.URLError, ValueError, json.JSONDecodeError) as exc:
            last_error = str(exc)
        time.sleep(0.2)
    raise VerificationError(f"controlled embedding server did not become ready: {last_error}")


@contextlib.contextmanager
def managed_embedding_server(
    settings: Settings,
    retained_model: pathlib.Path,
) -> Iterator[ServerEvidence]:
    host, port = assert_pinned_loopback_endpoint_available(settings.embed_url)
    run_dir = settings.output_dir / "runs" / "embeddinggemma_rrf"
    run_dir.mkdir(parents=True, exist_ok=True)
    stdout_path = run_dir / "server.stdout.txt"
    stderr_path = run_dir / "server.stderr.txt"
    command = (settings.node_binary, str(settings.embed_server_script.resolve()))
    environment = {key: os.environ[key] for key in SAFE_PARENT_ENV if key in os.environ}
    environment.update({"GGUF": str(retained_model.resolve()), "HOST": host, "PORT": str(port)})
    with stdout_path.open("wb") as stdout_handle, stderr_path.open("wb") as stderr_handle:
        process = subprocess.Popen(
            command,
            cwd=settings.embed_server_script.parent,
            env=environment,
            stdout=stdout_handle,
            stderr=stderr_handle,
        )
        try:
            wait_for_server(settings.embed_url, process, settings.server_start_timeout_seconds)
            yield ServerEvidence(command, environment, stdout_path, stderr_path)
        finally:
            if process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=10)


def add_server_evidence(record: dict[str, Any], evidence: ServerEvidence, settings: Settings) -> None:
    stdout_path, stdout_hash = hashed_artifact(evidence.stdout_path, settings.artifact_root)
    stderr_path, stderr_hash = hashed_artifact(evidence.stderr_path, settings.artifact_root)
    artifacts = record["artifacts"]
    artifacts.update(
        {
            "embedding_server_stdout_path": stdout_path,
            "embedding_server_stdout_sha256": stdout_hash,
            "embedding_server_stderr_path": stderr_path,
            "embedding_server_stderr_sha256": stderr_hash,
        }
    )
    requested = record["requested"]
    requested["embedding_server_command"] = list(evidence.command)
    requested["embedding_server_environment"] = {
        "GGUF": evidence.environment["GGUF"],
        "HOST": evidence.environment["HOST"],
        "PORT": evidence.environment["PORT"],
    }


def validate_server_evidence(record: dict[str, Any], root: pathlib.Path) -> None:
    artifacts = record["artifacts"]
    for stem in ("embedding_server_stdout", "embedding_server_stderr"):
        path = root / artifacts[f"{stem}_path"]
        require(path.is_file(), f"missing {stem} artifact")
        require_sha256(sha256_file(path), artifacts[f"{stem}_sha256"], stem)


def publish_manifest(settings: Settings, matrix: dict[str, Any], records: list[dict[str, Any]]) -> pathlib.Path:
    arms = [record.get("arm") for record in records]
    require(arms == list(ARMS), f"records must be emitted once in canonical arm order: {arms}")
    manifest = {"schema_version": 1, "records": records}
    path = settings.output_dir / "engine-verification.json"
    require(not path.exists(), f"refusing to replace existing evidence manifest: {path}")
    path.write_text(json.dumps(manifest, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    evidence = artifact_path(path, settings.artifact_root)
    errors = check_protocol.validate_engine_verification(
        matrix,
        {"status": "pass", "evidence": evidence},
        here=settings.artifact_root,
        repo=settings.artifact_root,
    )
    if errors:
        path.unlink(missing_ok=True)
        raise VerificationError("generated evidence failed the protocol checker:\n- " + "\n- ".join(errors))
    validate_server_evidence(records[2], settings.artifact_root)
    return path


def execute(
    settings: Settings,
    *,
    run_command: RunCommand = subprocess.run,
    server_context: Callable[[Settings, pathlib.Path], contextlib.AbstractContextManager[ServerEvidence]] = managed_embedding_server,
) -> pathlib.Path:
    require(not settings.output_dir.exists(), f"output directory already exists: {settings.output_dir}")
    try:
        settings.output_dir.resolve().relative_to(settings.artifact_root.resolve())
    except ValueError as exc:
        raise VerificationError("--output-dir must be below --artifact-root") from exc

    matrix = json.loads((HERE / "engine-matrix.json").read_text(encoding="utf-8"))
    matrix_by_id = {arm["id"]: arm for arm in matrix["arms"]}
    require(tuple(matrix_by_id) == ARMS, "engine matrix arm order changed")
    pinned_url = matrix_by_id["embeddinggemma_rrf"]["environment"]["ENTIRE_BRAIN_EMBED_URL"]
    require(settings.embed_url == pinned_url, f"embedding URL must match the production matrix pin: {pinned_url}")

    for path, label in (
        (settings.binary, "binary"),
        (settings.frozen_facts, "frozen facts"),
        (settings.session_dates, "session dates"),
        (settings.embedding_model, "EmbeddingGemma model"),
        (settings.embed_server_script, "embedding server script"),
    ):
        require(path.is_file() and not path.is_symlink(), f"{label} is missing or not a regular file: {path}")
    require(settings.repo_root.is_dir(), f"repository root is missing: {settings.repo_root}")
    require(settings.k > 0, "k must be positive")
    require(settings.query_id and settings.query, "query id and query text must be non-empty")
    validate_exposed_development_query(settings)
    require(settings.expected_eligible_count <= settings.expected_prefilter_count, "expected eligibility counts are impossible")
    require_sha256(sha256_file(settings.frozen_facts), settings.expected_facts_sha256, "frozen facts")
    require_sha256(sha256_file(settings.session_dates), settings.expected_session_dates_sha256, "session dates")
    require_sha256(sha256_file(settings.embedding_model), settings.expected_embedding_model_sha256, "EmbeddingGemma model")

    # Port ownership is checked before any evidence bytes are generated.  A
    # process left by an earlier smoke run is not silently reused.
    assert_pinned_loopback_endpoint_available(settings.embed_url)
    settings.output_dir.mkdir(parents=True)
    retained_binary = settings.output_dir / "artifacts" / "bin" / "entire-brain"
    retained_model = settings.output_dir / "artifacts" / "models" / settings.embedding_model.name
    copy_file(settings.binary, retained_binary)
    copy_file(settings.embedding_model, retained_model)
    require_sha256(sha256_file(retained_model), settings.expected_embedding_model_sha256, "retained EmbeddingGemma model")

    prepared = {arm: prepare_arm(settings, arm) for arm in ARMS}
    require(len({str(value.data_dir.resolve()) for value in prepared.values()}) == len(ARMS), "derived data roots overlap")
    records = [
        run_arm(settings, matrix_by_id[arm], prepared[arm], retained_binary, retained_model, run_command=run_command)
        for arm in ARMS[:2]
    ]
    with server_context(settings, retained_model) as evidence:
        embedding_record = run_arm(
            settings,
            matrix_by_id["embeddinggemma_rrf"],
            prepared["embeddinggemma_rrf"],
            retained_binary,
            retained_model,
            run_command=run_command,
        )
    add_server_evidence(embedding_record, evidence, settings)
    records.append(embedding_record)

    require_sha256(sha256_file(settings.frozen_facts), settings.expected_facts_sha256, "frozen facts after run")
    require_sha256(sha256_file(settings.session_dates), settings.expected_session_dates_sha256, "session dates after run")
    return publish_manifest(settings, matrix, records)


def parse_args(argv: Sequence[str] | None = None) -> Settings:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=pathlib.Path, required=True)
    parser.add_argument("--frozen-data-dir", type=pathlib.Path, required=True)
    parser.add_argument("--frozen-config-dir", type=pathlib.Path, required=True)
    parser.add_argument("--frozen-state-dir", type=pathlib.Path, required=True)
    parser.add_argument("--frozen-facts", type=pathlib.Path, required=True)
    parser.add_argument("--repo-root", type=pathlib.Path, required=True)
    parser.add_argument("--session-dates", type=pathlib.Path, required=True)
    parser.add_argument("--query-id", required=True)
    parser.add_argument("--query", required=True)
    parser.add_argument("--branch", default="main")
    parser.add_argument("--k", type=int, default=5)
    parser.add_argument("--eligible-before", required=True)
    parser.add_argument("--exclude-session-id", action="append", default=[])
    parser.add_argument("--expected-facts-sha256", required=True)
    parser.add_argument("--expected-session-dates-sha256", required=True)
    parser.add_argument("--expected-prefilter-count", type=int, required=True)
    parser.add_argument("--expected-eligible-count", type=int, required=True)
    parser.add_argument("--embedding-model", type=pathlib.Path, required=True)
    parser.add_argument("--expected-embedding-model-sha256", required=True)
    parser.add_argument("--embed-url", default="http://127.0.0.1:11500")
    parser.add_argument("--node-binary", default="node")
    parser.add_argument("--embed-server-script", type=pathlib.Path, default=REPO / "scripts" / "bench" / "embed-server.mjs")
    parser.add_argument("--server-start-timeout-seconds", type=float, default=120.0)
    parser.add_argument("--output-dir", type=pathlib.Path, required=True)
    parser.add_argument("--artifact-root", type=pathlib.Path, default=REPO)
    args = parser.parse_args(argv)
    return Settings(
        binary=args.binary,
        frozen_data_dir=args.frozen_data_dir,
        frozen_config_dir=args.frozen_config_dir,
        frozen_state_dir=args.frozen_state_dir,
        frozen_facts=args.frozen_facts,
        repo_root=args.repo_root,
        session_dates=args.session_dates,
        query_id=args.query_id,
        query=args.query,
        branch=args.branch,
        k=args.k,
        eligible_before=args.eligible_before,
        exclude_session_ids=tuple(args.exclude_session_id),
        expected_facts_sha256=args.expected_facts_sha256,
        expected_session_dates_sha256=args.expected_session_dates_sha256,
        expected_prefilter_count=args.expected_prefilter_count,
        expected_eligible_count=args.expected_eligible_count,
        embedding_model=args.embedding_model,
        expected_embedding_model_sha256=args.expected_embedding_model_sha256,
        embed_url=args.embed_url,
        node_binary=args.node_binary,
        embed_server_script=args.embed_server_script,
        server_start_timeout_seconds=args.server_start_timeout_seconds,
        output_dir=args.output_dir,
        artifact_root=args.artifact_root,
    )


def main(argv: Sequence[str] | None = None) -> int:
    try:
        manifest = execute(parse_args(argv))
    except (OSError, VerificationError) as exc:
        print(f"engine verification refused: {exc}", file=sys.stderr)
        return 2
    print(manifest)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
