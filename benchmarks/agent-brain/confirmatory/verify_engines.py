#!/usr/bin/env python3
"""Create fail-closed, byte-verifiable evidence for the three retrieval engines.

Production expectations come only from ``engine-verification-pins.json``.  The
CLI accepts source locations, never caller-supplied expected hashes or counts.
It invokes no coding agent and opens only the named exposed development task.
"""

from __future__ import annotations

import argparse
import contextlib
import dataclasses
import datetime as dt
import hashlib
import json
import os
import pathlib
import platform
import secrets
import shutil
import socket
import struct
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from collections.abc import Callable, Iterator, Sequence
from typing import Any


HERE = pathlib.Path(__file__).resolve().parent
REPO = HERE.parents[2]
PINS_PATH = HERE / "engine-verification-pins.json"
MATRIX_PATH = HERE / "engine-matrix.json"
if str(HERE) not in sys.path:
    sys.path.insert(0, str(HERE))

import check_protocol  # noqa: E402


ARMS = ("lexical_handrolled", "model2vec_rrf", "embeddinggemma_rrf")
SAFE_PARENT_ENV = ("HOME", "LANG", "LC_ALL", "PATH", "TMPDIR")
DEPENDENCY_INVENTORY_ALGORITHM = "sha256_ordered_relative_path_nul_sha256_newline_v1"
PRODUCTION_AUTHORITY = "production"
TEST_AUTHORITY = "test_fixture"


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
    embedding_model: pathlib.Path
    embed_url: str
    node_runtime: pathlib.Path
    runtime_dependency_root: pathlib.Path
    server_start_timeout_seconds: float
    server_health_interval_seconds: float
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
class RuntimeBundle:
    binary: pathlib.Path
    binary_attestation: pathlib.Path
    facts_source: pathlib.Path
    session_dates_source: pathlib.Path
    embedding_model: pathlib.Path
    node_runtime: pathlib.Path
    server_script: pathlib.Path
    package_manifest: pathlib.Path
    package_lock: pathlib.Path
    dependency_root: pathlib.Path
    dependency_manifest: pathlib.Path


@dataclasses.dataclass(frozen=True)
class ServerEvidence:
    command: tuple[str, ...]
    environment: dict[str, str]
    stdout_path: pathlib.Path
    stderr_path: pathlib.Path
    attestation_path: pathlib.Path
    observe: Callable[[str], dict[str, Any]]
    record_recall_window: Callable[[str, str], None]


RunCommand = Callable[..., subprocess.CompletedProcess[bytes]]
ServerContext = Callable[
    [Settings, RuntimeBundle, dict[str, Any]],
    contextlib.AbstractContextManager[ServerEvidence],
]


def sha256_file(path: pathlib.Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def canonical_sha256(value: Any) -> str:
    encoded = json.dumps(value, ensure_ascii=False, allow_nan=False, sort_keys=True, separators=(",", ":")).encode()
    return hashlib.sha256(encoded).hexdigest()


def require(condition: bool, message: str) -> None:
    if not condition:
        raise VerificationError(message)


def require_sha256(actual: str, expected: str, label: str) -> None:
    require(actual == expected, f"{label} SHA-256 mismatch: got {actual}, want {expected}")


def require_pinned_file(path: pathlib.Path, pin: dict[str, Any], stem: str, label: str) -> None:
    require(path.is_file() and not path.is_symlink(), f"{label} is missing or not a regular file: {path}")
    require_sha256(sha256_file(path), pin[f"{stem}_sha256"], label)
    size_key = f"{stem}_size_bytes"
    if size_key in pin:
        require(path.stat().st_size == pin[size_key], f"{label} size does not match the canonical pin")


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


def dependency_rows(root: pathlib.Path) -> tuple[list[tuple[str, str, int]], str]:
    require(root.is_dir() and not root.is_symlink(), f"runtime dependency root is missing: {root}")
    rows: list[tuple[str, str, int]] = []
    for path in sorted(root.rglob("*")):
        if path.is_symlink():
            continue
        if path.is_file():
            rows.append((path.relative_to(root).as_posix(), sha256_file(path), path.stat().st_size))
    aggregate = hashlib.sha256()
    for relative, digest, _ in rows:
        aggregate.update(relative.encode("utf-8"))
        aggregate.update(b"\0")
        aggregate.update(digest.encode("ascii"))
        aggregate.update(b"\n")
    return rows, aggregate.hexdigest()


def copy_dependency_tree(source: pathlib.Path, target: pathlib.Path) -> None:
    require(not target.exists(), f"runtime dependency target already exists: {target}")
    for path in sorted(source.rglob("*")):
        if path.is_symlink():
            continue
        relative = path.relative_to(source)
        if path.is_dir():
            (target / relative).mkdir(parents=True, exist_ok=True)
        elif path.is_file():
            copy_file(path, target / relative)


def load_production_pins() -> dict[str, Any]:
    pins = json.loads(PINS_PATH.read_text(encoding="utf-8"))
    require(pins.get("schema_version") == 1, "production engine pin schema changed")
    require(pins.get("authority") == PRODUCTION_AUTHORITY, "production engine pins lost production authority")
    return pins


def pin_descriptor(pins: dict[str, Any], authority: str) -> dict[str, str]:
    digest = sha256_file(PINS_PATH) if authority == PRODUCTION_AUTHORITY else canonical_sha256(pins)
    return {"id": pins["pin_set_id"], "sha256": digest, "authority": authority}


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
    return pathlib.Path(*parts[:facts_index]), pathlib.Path(*parts[facts_index:]), relative_facts


def validate_development_query(settings: Settings, pins: dict[str, Any]) -> None:
    task = pins["development_task"]
    query = next((item for item in task["queries"] if item.get("query_id") == settings.query_id), None)
    require(isinstance(query, dict), "query id is not in the canonical development pin set")
    require(query.get("query_text") == settings.query, "query text does not match its canonical development pin")
    require(hashlib.sha256(settings.query.encode()).hexdigest() == query.get("query_sha256"), "query hash is stale")
    require(settings.branch == task.get("branch"), "development task branch changed")
    require(settings.k == task.get("k"), "development task k changed")
    require(settings.eligible_before == task.get("eligible_before"), "development task temporal cutoff changed")
    require(
        sorted(settings.exclude_session_ids) == sorted(task.get("exclude_session_ids", [])),
        "development task excluded-session set changed",
    )


def validate_source_inputs(settings: Settings, pins: dict[str, Any], authority: str) -> None:
    require(settings.repo_root.is_dir(), f"repository root is missing: {settings.repo_root}")
    require(settings.k > 0, "k must be positive")
    require(settings.query_id and settings.query, "query id and query text must be non-empty")
    validate_development_query(settings, pins)
    binary = pins["binary"]
    require_pinned_file(settings.binary, binary, "binary", "canonical entire-brain binary")
    corpus = pins["corpus"]
    model = pins["embedding_model"]
    require_pinned_file(settings.frozen_facts, corpus, "facts", "frozen facts")
    require_pinned_file(settings.session_dates, corpus, "session_dates", "session dates")
    require(settings.embedding_model.is_file() and not settings.embedding_model.is_symlink(), "EmbeddingGemma model is missing")
    require_sha256(sha256_file(settings.embedding_model), model["sha256"], "EmbeddingGemma model")
    require(settings.embedding_model.stat().st_size == model["size_bytes"], "EmbeddingGemma model size changed")

    runtime = pins["runtime"]
    require(
        settings.server_health_interval_seconds == runtime["server_health_interval_seconds"],
        "server health interval differs from the canonical pin",
    )
    server_script = REPO / runtime["server_script_repo_path"]
    package_manifest = REPO / runtime["package_manifest_repo_path"]
    package_lock = REPO / runtime["package_lock_repo_path"]
    for path, expected, label in (
        (server_script, runtime["server_script_sha256"], "server script"),
        (package_manifest, runtime["package_manifest_sha256"], "package manifest"),
        (package_lock, runtime["package_lock_sha256"], "package lock"),
        (settings.node_runtime, runtime["node_sha256"], "resolved Node runtime"),
    ):
        require(path.is_file() and not path.is_symlink(), f"{label} is missing or is a symlink: {path}")
        require_sha256(sha256_file(path), expected, label)

    rows, aggregate = dependency_rows(settings.runtime_dependency_root)
    require(DEPENDENCY_INVENTORY_ALGORITHM == runtime["dependency_inventory_algorithm"], "dependency algorithm pin changed")
    require(aggregate == runtime["dependency_inventory_sha256"], "runtime dependency inventory does not match canonical pin")
    require(len(rows) == runtime["dependency_file_count"], "runtime dependency file count does not match canonical pin")
    require(sum(size for _, _, size in rows) == runtime["dependency_total_bytes"], "runtime dependency byte count does not match canonical pin")
    if authority == PRODUCTION_AUTHORITY:
        require(pins is not None and pins.get("authority") == PRODUCTION_AUTHORITY, "production execution requires production pins")
        actual_platform = f"{platform.system().lower()}-{platform.machine().lower()}"
        require(runtime["platform"] == actual_platform, f"runtime platform mismatch: got {actual_platform}")


def build_dependency_manifest(
    dependency_root: pathlib.Path,
    path: pathlib.Path,
    settings: Settings,
    pins: dict[str, Any],
) -> None:
    rows, aggregate = dependency_rows(dependency_root)
    root_relative = artifact_path(dependency_root, settings.artifact_root)
    files = [
        {
            "relative_path": relative,
            "path": artifact_path(dependency_root / relative, settings.artifact_root),
            "sha256": digest,
            "size_bytes": size,
        }
        for relative, digest, size in rows
    ]
    runtime = pins["runtime"]
    manifest = {
        "schema_version": 1,
        "algorithm": DEPENDENCY_INVENTORY_ALGORITHM,
        "root_path": root_relative,
        "aggregate_sha256": aggregate,
        "file_count": len(files),
        "total_bytes": sum(item["size_bytes"] for item in files),
        "node_version": runtime["node_version"],
        "node_llama_cpp_version": runtime["node_llama_cpp_version"],
        "platform_package": runtime["platform_package"],
        "platform_package_version": runtime["platform_package_version"],
        "files": files,
    }
    path.write_text(json.dumps(manifest, indent=2, sort_keys=True) + "\n", encoding="utf-8")


def retain_runtime_bundle(
    settings: Settings,
    pins: dict[str, Any],
    *,
    version_command: RunCommand = subprocess.run,
) -> RuntimeBundle:
    artifacts = settings.output_dir / "artifacts"
    runtime_dir = artifacts / "runtime"
    bundle = RuntimeBundle(
        binary=artifacts / "bin" / "entire-brain",
        binary_attestation=artifacts / "bin" / "binary-attestation.json",
        facts_source=artifacts / "sources" / "facts.ndjson",
        session_dates_source=artifacts / "sources" / "session_dates.json",
        embedding_model=artifacts / "models" / settings.embedding_model.name,
        node_runtime=runtime_dir / "bin" / "node",
        server_script=runtime_dir / "embed-server.mjs",
        package_manifest=runtime_dir / "package.json",
        package_lock=runtime_dir / "package-lock.json",
        dependency_root=runtime_dir / "node_modules",
        dependency_manifest=runtime_dir / "dependency-inventory.json",
    )
    runtime = pins["runtime"]
    copy_file(settings.binary, bundle.binary)
    binary = pins["binary"]
    require_sha256(sha256_file(bundle.binary), binary["binary_sha256"], "retained canonical binary")
    require(bundle.binary.stat().st_size == binary["binary_size_bytes"], "retained canonical binary size changed")
    bundle.binary_attestation.write_text(
        json.dumps(
            {
                "schema_version": 1,
                "pin_set_id": pins["pin_set_id"],
                "provenance_mode": binary["provenance_mode"],
                "binary_path": artifact_path(bundle.binary, settings.artifact_root),
                "binary_sha256": binary["binary_sha256"],
                "binary_size_bytes": binary["binary_size_bytes"],
                "source_commit": binary["source_commit"],
                "source_tree": binary["source_tree"],
                "build_command": binary["build_command"],
                "go_version": binary["go_version"],
            },
            indent=2,
            sort_keys=True,
        )
        + "\n",
        encoding="utf-8",
    )
    copy_file(settings.frozen_facts, bundle.facts_source)
    copy_file(settings.session_dates, bundle.session_dates_source)
    copy_file(settings.embedding_model, bundle.embedding_model)
    copy_file(settings.node_runtime, bundle.node_runtime)
    copy_file(REPO / runtime["server_script_repo_path"], bundle.server_script)
    copy_file(REPO / runtime["package_manifest_repo_path"], bundle.package_manifest)
    copy_file(REPO / runtime["package_lock_repo_path"], bundle.package_lock)
    copy_dependency_tree(settings.runtime_dependency_root, bundle.dependency_root)
    build_dependency_manifest(bundle.dependency_root, bundle.dependency_manifest, settings, pins)

    corpus = pins["corpus"]
    model = pins["embedding_model"]
    require_sha256(sha256_file(bundle.facts_source), corpus["facts_sha256"], "retained facts source")
    require_sha256(sha256_file(bundle.session_dates_source), corpus["session_dates_sha256"], "retained session dates")
    require_sha256(sha256_file(bundle.embedding_model), model["sha256"], "retained EmbeddingGemma model")
    require_sha256(sha256_file(bundle.node_runtime), runtime["node_sha256"], "retained Node runtime")
    require_sha256(sha256_file(bundle.server_script), runtime["server_script_sha256"], "retained server script")
    require_sha256(sha256_file(bundle.package_manifest), runtime["package_manifest_sha256"], "retained package manifest")
    require_sha256(sha256_file(bundle.package_lock), runtime["package_lock_sha256"], "retained package lock")

    completed = version_command([str(bundle.node_runtime), "--version"], capture_output=True, check=False)
    require(completed.returncode == 0, "retained Node runtime could not report its version")
    version = completed.stdout.decode("utf-8", errors="strict").strip()
    require(version == runtime["node_version"], f"retained Node version mismatch: got {version}")
    return bundle


def prepare_arm(settings: Settings, bundle: RuntimeBundle, pins: dict[str, Any], arm: str) -> PreparedArm:
    require(arm in ARMS, f"unknown arm: {arm}")
    runtime = settings.output_dir / "runtime" / arm
    data_dir = runtime / "data"
    config_dir = runtime / "config"
    state_dir = runtime / "state"
    cache_dir = runtime / "cache"
    require(not runtime.exists(), f"arm runtime already exists: {runtime}")

    brain_relative, branch_facts_relative, relative_facts = frozen_layout(settings)
    target_facts = data_dir / relative_facts
    copy_file(bundle.facts_source, target_facts)
    frozen_brain = settings.frozen_data_dir / brain_relative
    target_brain = data_dir / brain_relative
    for relative in (pathlib.Path("manifest.json"), pathlib.Path("facts") / "taxonomy.json"):
        source = frozen_brain / relative
        if source.exists():
            copy_file(source, target_brain / relative)
    embeddings = target_brain / branch_facts_relative.parent / "embeddings"
    require(not embeddings.exists(), f"derived arm unexpectedly inherited vectors: {embeddings}")
    copy_small_tree(settings.frozen_config_dir, config_dir)
    copy_small_tree(settings.frozen_state_dir, state_dir)
    cache_dir.mkdir(parents=True)
    require_sha256(sha256_file(target_facts), pins["corpus"]["facts_sha256"], f"{arm} copied facts")
    return PreparedArm(arm, data_dir, config_dir, state_dir, cache_dir, target_facts)


def safe_runtime_environment(settings: Settings, prepared: PreparedArm, arm: str, pins: dict[str, Any]) -> dict[str, str]:
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
            "ENGINE_VERIFICATION_PIN_SET_ID": pins["pin_set_id"],
        }
    )
    return env


def recall_command(settings: Settings, bundle: RuntimeBundle, arm: str) -> list[str]:
    command = [
        str(bundle.binary.resolve()),
        "recall",
        settings.query,
        "--branch",
        settings.branch,
        "--k",
        str(settings.k),
        "--eligible-before",
        settings.eligible_before,
        "--session-dates",
        str(bundle.session_dates_source.resolve()),
    ]
    for session_id in settings.exclude_session_ids:
        command.extend(("--exclude-session-id", session_id))
    if arm == "lexical_handrolled":
        command.append("--no-semantic")
    command.append("--json")
    return command


def parse_vector_artifact(path: pathlib.Path) -> tuple[str, int, int, tuple[str, ...]]:
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
    return model_id, dimension, count, tuple(seen)


def parse_runtime_output(
    raw: bytes,
    arm: str,
    settings: Settings,
    prepared: PreparedArm,
    pins: dict[str, Any],
) -> dict[str, Any]:
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
    engine_pin = pins["engines"][arm]
    semantic = engine_pin["semantic"]
    require(engine.get("semantic_requested") is semantic, f"{arm} semantic request state mismatch")
    require(engine.get("semantic_available") is semantic, f"{arm} semantic availability mismatch")
    if semantic:
        require(engine.get("semantic_applied") is True, f"{arm} did not apply semantic ranking")
        require(engine.get("embedder_id") == engine_pin["embedder_id"], f"{arm} embedder identity mismatch")
        require(engine.get("embedding_dimension") == engine_pin["dimension"], f"{arm} dimension mismatch")
        require(engine.get("vector_cache_read_only") is False, f"{arm} vector cache was read-only")
        require(engine.get("vector_cache_backend") == "flat_file", f"{arm} did not use the retained flat-file cache")
        require(isinstance(engine.get("vector_count"), int) and engine["vector_count"] > 0, f"{arm} vector count is invalid")
        require(
            isinstance(engine.get("resident_vector_count"), int) and engine["resident_vector_count"] > 0,
            f"{arm} resident vector count is invalid",
        )
    else:
        require(not engine.get("embedder_id"), "lexical arm reported an embedder")
        require(engine.get("embedding_dimension") is None, "lexical arm reported an embedding dimension")

    corpus_pin = pins["corpus"]
    eligibility = output.get("eligibility")
    require(isinstance(eligibility, dict), f"{arm} output has no temporal eligibility audit")
    require(eligibility.get("prefilter_corpus_count") == corpus_pin["prefilter_count"], f"{arm} prefilter count mismatch")
    require(eligibility.get("eligible_count") == corpus_pin["eligible_count"], f"{arm} eligible count mismatch")
    excluded = eligibility.get("excluded_counts")
    require(isinstance(excluded, dict), f"{arm} excluded counts are invalid")
    require(
        all(isinstance(value, int) and not isinstance(value, bool) and value >= 0 for value in excluded.values()),
        f"{arm} excluded counts contain an invalid value",
    )
    require(sum(excluded.values()) == corpus_pin["prefilter_count"] - corpus_pin["eligible_count"], f"{arm} eligibility counts do not reconcile")
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
        model_id, dimension, resident_count, _ = parse_vector_artifact(cache_path)
        require(model_id == engine.get("embedder_id"), f"{arm} vector header model does not match runtime")
        require(dimension == engine.get("embedding_dimension"), f"{arm} vector header dimension does not match runtime")
        require(resident_count == engine.get("resident_vector_count"), f"{arm} vector header count does not match runtime")
    return output


def hashed_artifact(path: pathlib.Path, root: pathlib.Path) -> tuple[str, str]:
    return artifact_path(path, root), sha256_file(path)


def null_server_artifacts() -> dict[str, None]:
    stems = (
        "embedding_server_stdout",
        "embedding_server_stderr",
        "embedding_server_attestation",
        "server_script",
        "node_runtime",
        "package_manifest",
        "package_lock",
        "runtime_dependency_manifest",
    )
    return {f"{stem}_{suffix}": None for stem in stems for suffix in ("path", "sha256")}


def run_arm(
    settings: Settings,
    matrix_arm: dict[str, Any],
    prepared: PreparedArm,
    bundle: RuntimeBundle,
    pins: dict[str, Any],
    descriptor: dict[str, str],
    *,
    run_command: RunCommand = subprocess.run,
    during_observer: Callable[[str], dict[str, Any]] | None = None,
    recall_window_observer: Callable[[str, str], None] | None = None,
) -> dict[str, Any]:
    arm = prepared.arm
    run_dir = settings.output_dir / "runs" / arm
    run_dir.mkdir(parents=True, exist_ok=True)
    stdout_path = run_dir / "recall.stdout.json"
    stderr_path = run_dir / "recall.stderr.txt"
    command = recall_command(settings, bundle, arm)
    environment = safe_runtime_environment(settings, prepared, arm, pins)
    if during_observer is None:
        completed = run_command(command, cwd=settings.repo_root, env=environment, capture_output=True, check=False)
    else:
        require(recall_window_observer is not None, "recall-window observer is required with live server observation")
        result: list[subprocess.CompletedProcess[bytes]] = []
        failure: list[BaseException] = []
        recall_started = threading.Event()
        recall_finished = threading.Event()
        timing: dict[str, str] = {}

        def invoke_recall() -> None:
            timing["started_at"] = dt.datetime.now(dt.UTC).isoformat()
            recall_started.set()
            try:
                result.append(
                    run_command(command, cwd=settings.repo_root, env=environment, capture_output=True, check=False)
                )
            except BaseException as exc:  # propagate the runner's original failure after joining
                failure.append(exc)
            finally:
                timing["finished_at"] = dt.datetime.now(dt.UTC).isoformat()
                recall_finished.set()

        worker = threading.Thread(target=invoke_recall, name="embeddinggemma-recall")
        worker.start()
        try:
            require(
                recall_started.wait(timeout=settings.server_start_timeout_seconds),
                f"{arm} recall worker did not report startup",
            )
            completed_before_observation = recall_finished.is_set()
            if not completed_before_observation:
                during_observer("during_recall")
            completed_before_observation_returned = recall_finished.is_set()
        finally:
            worker.join()
        if failure:
            raise failure[0]
        require(not completed_before_observation, f"{arm} recall completed before the during-recall health request")
        require(
            not completed_before_observation_returned,
            f"{arm} recall did not remain active for the complete during-recall health request",
        )
        require(len(result) == 1, f"{arm} recall runner produced no completion result")
        recall_window_observer(timing["started_at"], timing["finished_at"])
        completed = result[0]
    require(isinstance(completed.stdout, bytes) and isinstance(completed.stderr, bytes), "runner must capture byte output")
    stdout_path.write_bytes(completed.stdout)
    stderr_path.write_bytes(completed.stderr)
    require(completed.returncode == 0, f"{arm} recall exited {completed.returncode}; see {stderr_path}")
    output = parse_runtime_output(completed.stdout, arm, settings, prepared, pins)
    runtime_engine = output["retrieval_engine"]
    corpus_hash = pins["corpus"]["facts_sha256"]
    require_sha256(sha256_file(prepared.facts), corpus_hash, f"{arm} derived facts after recall")

    vector_path: str | None = None
    vector_hash: str | None = None
    if pins["engines"][arm]["semantic"]:
        retained_vector = settings.output_dir / "artifacts" / "vectors" / f"{arm}.vectors.bin"
        copy_file(pathlib.Path(runtime_engine["vector_cache_path"]), retained_vector)
        vector_path, vector_hash = hashed_artifact(retained_vector, settings.artifact_root)

    binary_path, binary_hash = hashed_artifact(bundle.binary, settings.artifact_root)
    binary_attestation_path, binary_attestation_hash = hashed_artifact(
        bundle.binary_attestation,
        settings.artifact_root,
    )
    stdout_rel, stdout_hash = hashed_artifact(stdout_path, settings.artifact_root)
    stderr_rel, stderr_hash = hashed_artifact(stderr_path, settings.artifact_root)
    facts_source_path, facts_source_hash = hashed_artifact(bundle.facts_source, settings.artifact_root)
    sessions_path, sessions_hash = hashed_artifact(bundle.session_dates_source, settings.artifact_root)
    derived_path, derived_hash = hashed_artifact(prepared.facts, settings.artifact_root)
    model_path: str | None = None
    model_hash: str | None = None
    if arm == "embeddinggemma_rrf":
        model_path, model_hash = hashed_artifact(bundle.embedding_model, settings.artifact_root)

    eligibility = output["eligibility"]
    semantic = pins["engines"][arm]["semantic"]
    artifacts: dict[str, Any] = {
        "binary_path": binary_path,
        "binary_sha256": binary_hash,
        "binary_attestation_path": binary_attestation_path,
        "binary_attestation_sha256": binary_attestation_hash,
        "stdout_path": stdout_rel,
        "stdout_sha256": stdout_hash,
        "stderr_path": stderr_rel,
        "stderr_sha256": stderr_hash,
        "facts_source_path": facts_source_path,
        "facts_source_sha256": facts_source_hash,
        "session_dates_source_path": sessions_path,
        "session_dates_source_sha256": sessions_hash,
        "derived_facts_path": derived_path,
        "derived_facts_sha256": derived_hash,
        "vector_artifact_path": vector_path,
        "vector_artifact_sha256": vector_hash,
        "embedding_model_path": model_path,
        "embedding_model_sha256": model_hash,
    }
    artifacts.update(null_server_artifacts())
    return {
        "schema_version": 2,
        "pin_set": dict(descriptor),
        "arm": arm,
        "requested": {"command": command, "environment": environment, "namespace": matrix_arm["namespace"]},
        "effective": {
            "engine": runtime_engine["effective_engine"],
            "semantic_available": runtime_engine["semantic_available"],
            "bm25_enabled": runtime_engine["bm25_enabled"],
            "fallback_used": runtime_engine["fallback_used"],
            "embedder_id": runtime_engine.get("embedder_id") if semantic else None,
            "embedding_dimension": runtime_engine.get("embedding_dimension") if semantic else None,
            "vector_count": runtime_engine.get("vector_count", 0) if semantic else 0,
            "resident_vector_count": runtime_engine.get("resident_vector_count", 0) if semantic else 0,
            "vector_namespace": matrix_arm["namespace"],
        },
        "artifacts": artifacts,
        "corpus": {
            "facts_sha256": corpus_hash,
            "prefilter_count": eligibility["prefilter_corpus_count"],
            "eligible_count": eligibility["eligible_count"],
            "excluded_by_reason": eligibility["excluded_counts"],
            "delivered_count": eligibility["delivered_count"],
        },
        "result": {
            "query_id": settings.query_id,
            "fact_ids_in_order": [fact["id"] for fact in output["facts"]],
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


def health_url(embed_url: str) -> str:
    parsed = urllib.parse.urlparse(embed_url)
    return urllib.parse.urlunparse((parsed.scheme, parsed.netloc, "/health", "", "", ""))


def request_json(request: urllib.request.Request, timeout: float = 3) -> dict[str, Any]:
    with urllib.request.urlopen(request, timeout=timeout) as response:  # noqa: S310 - caller pins loopback
        value = json.load(response)
    require(isinstance(value, dict), "server response is not a JSON object")
    return value


def probe_embedding(url: str, dimension: int) -> None:
    body = json.dumps({"model": "embeddinggemma", "input": "title: none | text: verification probe"}).encode()
    result = request_json(urllib.request.Request(url, data=body, headers={"Content-Type": "application/json"}))
    embeddings = result.get("embeddings")
    require(
        isinstance(embeddings, list)
        and embeddings
        and isinstance(embeddings[0], list)
        and len(embeddings[0]) == dimension,
        "server returned an invalid embedding shape",
    )


class HealthMonitor:
    def __init__(
        self,
        process: subprocess.Popen[bytes],
        url: str,
        token: str,
        pins: dict[str, Any],
        model_path: pathlib.Path,
        interval_seconds: float,
    ) -> None:
        self.process = process
        self.url = health_url(url)
        self.token = token
        self.pins = pins
        self.model_path = model_path.resolve()
        self.interval_seconds = interval_seconds
        self.observations: list[dict[str, Any]] = []
        self.errors: list[str] = []
        self._lock = threading.Lock()
        self._observe_lock = threading.Lock()
        self._stop = threading.Event()
        self._thread: threading.Thread | None = None
        self._last_health_request_count = 0

    def observe(self, phase: str) -> dict[str, Any]:
        with self._observe_lock:
            require(self.process.poll() is None, f"owned embedding server exited before {phase}")
            nonce = secrets.token_hex(16)
            url = self.url + "?nonce=" + urllib.parse.quote(nonce, safe="")
            result = request_json(urllib.request.Request(url, method="GET"))
            model = self.pins["embedding_model"]
            runtime = self.pins["runtime"]
            require(result.get("pid") == self.process.pid, f"health PID mismatch during {phase}")
            require(result.get("verification_token") == self.token, f"health ownership token mismatch during {phase}")
            require(result.get("request_nonce") == nonce, f"health nonce mismatch during {phase}")
            request_count = result.get("health_request_count")
            require(
                isinstance(request_count, int)
                and not isinstance(request_count, bool)
                and request_count > self._last_health_request_count,
                f"health request counter did not advance during {phase}",
            )
            self._last_health_request_count = request_count
            require(pathlib.Path(result.get("model_path", "")).resolve() == self.model_path, f"health model path mismatch during {phase}")
            require(result.get("model_sha256") == model["sha256"], f"health model hash mismatch during {phase}")
            require(result.get("embedding_dimension") == model["dimension"], f"health dimension mismatch during {phase}")
            require(result.get("node_version") == runtime["node_version"], f"health Node version mismatch during {phase}")
            observation = {
                "sequence": 0,
                "phase": phase,
                "observed_at": dt.datetime.now(dt.UTC).isoformat(),
                "pid": result["pid"],
                "ownership_token_sha256": hashlib.sha256(self.token.encode()).hexdigest(),
                "request_nonce_sha256": hashlib.sha256(nonce.encode()).hexdigest(),
                "health_request_count": request_count,
                "model_path": str(self.model_path),
                "model_sha256": result["model_sha256"],
                "embedding_dimension": result["embedding_dimension"],
                "node_version": result["node_version"],
                "healthy": True,
            }
            with self._lock:
                observation["sequence"] = len(self.observations)
                self.observations.append(observation)
            return dict(observation)

    def _loop(self) -> None:
        while not self._stop.wait(self.interval_seconds):
            try:
                self.observe("heartbeat")
            except (OSError, VerificationError, urllib.error.URLError, json.JSONDecodeError) as exc:
                with self._lock:
                    self.errors.append(str(exc))
                self._stop.set()
                return

    def start(self) -> None:
        self.observe("pre_recall")
        self._thread = threading.Thread(target=self._loop, name="engine-server-health", daemon=True)
        self._thread.start()

    def finish(self) -> None:
        self._stop.set()
        if self._thread is not None:
            self._thread.join(timeout=max(2.0, self.interval_seconds * 2))
            require(not self._thread.is_alive(), "server health monitor did not stop")
        require(not self.errors, "server health monitor failed: " + "; ".join(self.errors))
        self.observe("post_recall")
        require(any(item["phase"] == "during_recall" for item in self.observations), "no during-recall health observation")
        require(self.observations[-1]["phase"] == "post_recall", "final health observation is not post-recall")
        require(self.process.poll() is None, "owned embedding server exited before attestation finalization")


def wait_for_owned_server(
    settings: Settings,
    process: subprocess.Popen[bytes],
    token: str,
    pins: dict[str, Any],
    model_path: pathlib.Path,
) -> None:
    deadline = time.monotonic() + settings.server_start_timeout_seconds
    last_error = "no response"
    while time.monotonic() < deadline:
        if process.poll() is not None:
            raise VerificationError(f"controlled embedding server exited early with code {process.returncode}")
        try:
            monitor = HealthMonitor(
                process,
                settings.embed_url,
                token,
                pins,
                model_path,
                settings.server_health_interval_seconds,
            )
            monitor.observe("startup")
            probe_embedding(settings.embed_url, pins["embedding_model"]["dimension"])
            return
        except (OSError, VerificationError, urllib.error.URLError, ValueError, json.JSONDecodeError) as exc:
            last_error = str(exc)
        time.sleep(0.2)
    raise VerificationError(f"controlled embedding server did not become ready: {last_error}")


@contextlib.contextmanager
def managed_embedding_server(
    settings: Settings,
    bundle: RuntimeBundle,
    pins: dict[str, Any],
) -> Iterator[ServerEvidence]:
    host, port = assert_pinned_loopback_endpoint_available(settings.embed_url)
    run_dir = settings.output_dir / "runs" / "embeddinggemma_rrf"
    run_dir.mkdir(parents=True, exist_ok=True)
    stdout_path = run_dir / "server.stdout.txt"
    stderr_path = run_dir / "server.stderr.txt"
    attestation_path = run_dir / "server-attestation.json"
    token = secrets.token_hex(32)
    command = (str(bundle.node_runtime.resolve()), str(bundle.server_script.resolve()))
    environment = {key: os.environ[key] for key in SAFE_PARENT_ENV if key in os.environ}
    environment.update(
        {
            "GGUF": str(bundle.embedding_model.resolve()),
            "HOST": host,
            "PORT": str(port),
            "ENGINE_VERIFICATION_TOKEN": token,
        }
    )
    monitor: HealthMonitor | None = None
    recall_window: dict[str, str] = {}

    def record_recall_window(started_at: str, finished_at: str) -> None:
        require(not recall_window, "embedding recall window was recorded more than once")
        recall_window.update({"started_at": started_at, "finished_at": finished_at})

    with stdout_path.open("wb") as stdout_handle, stderr_path.open("wb") as stderr_handle:
        process = subprocess.Popen(
            command,
            cwd=bundle.server_script.parent,
            env=environment,
            stdout=stdout_handle,
            stderr=stderr_handle,
        )
        try:
            wait_for_owned_server(settings, process, token, pins, bundle.embedding_model)
            monitor = HealthMonitor(
                process,
                settings.embed_url,
                token,
                pins,
                bundle.embedding_model,
                settings.server_health_interval_seconds,
            )
            monitor.start()
            yield ServerEvidence(
                command,
                environment,
                stdout_path,
                stderr_path,
                attestation_path,
                monitor.observe,
                record_recall_window,
            )
            monitor.finish()
            require(set(recall_window) == {"started_at", "finished_at"}, "embedding recall window was not recorded")
            attestation = {
                "schema_version": 2,
                "pin_set_id": pins["pin_set_id"],
                "process_pid": process.pid,
                "ownership_token_sha256": hashlib.sha256(token.encode()).hexdigest(),
                "model_path": str(bundle.embedding_model.resolve()),
                "model_sha256": pins["embedding_model"]["sha256"],
                "embedding_dimension": pins["embedding_model"]["dimension"],
                "node_version": pins["runtime"]["node_version"],
                "health_interval_seconds": settings.server_health_interval_seconds,
                "recall_window": recall_window,
                "observations": monitor.observations,
            }
            attestation_path.write_text(json.dumps(attestation, indent=2, sort_keys=True) + "\n", encoding="utf-8")
        finally:
            if monitor is not None:
                monitor._stop.set()
            if process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=10)


def add_server_evidence(
    record: dict[str, Any],
    evidence: ServerEvidence,
    bundle: RuntimeBundle,
    settings: Settings,
) -> None:
    artifacts = record["artifacts"]
    for key, path in (
        ("embedding_server_stdout", evidence.stdout_path),
        ("embedding_server_stderr", evidence.stderr_path),
        ("embedding_server_attestation", evidence.attestation_path),
        ("server_script", bundle.server_script),
        ("node_runtime", bundle.node_runtime),
        ("package_manifest", bundle.package_manifest),
        ("package_lock", bundle.package_lock),
        ("runtime_dependency_manifest", bundle.dependency_manifest),
    ):
        relative, digest = hashed_artifact(path, settings.artifact_root)
        artifacts[f"{key}_path"] = relative
        artifacts[f"{key}_sha256"] = digest
    record["requested"]["embedding_server_command"] = list(evidence.command)
    record["requested"]["embedding_server_environment"] = dict(evidence.environment)


def publish_manifest(
    settings: Settings,
    matrix: dict[str, Any],
    records: list[dict[str, Any]],
    pins: dict[str, Any],
    authority: str,
) -> pathlib.Path:
    arms = [record.get("arm") for record in records]
    require(arms == list(ARMS), f"records must be emitted once in canonical arm order: {arms}")
    manifest = {"schema_version": 2, "records": records}
    final_path = settings.output_dir / "engine-verification.json"
    require(not final_path.exists(), f"refusing to replace existing evidence manifest: {final_path}")
    temp_path = final_path.with_name(f".{final_path.name}.{secrets.token_hex(8)}.tmp")
    try:
        with temp_path.open("x", encoding="utf-8") as handle:
            json.dump(manifest, handle, indent=2, sort_keys=True)
            handle.write("\n")
            handle.flush()
            os.fsync(handle.fileno())
        evidence = artifact_path(temp_path, settings.artifact_root)
        errors = check_protocol.validate_engine_verification(
            matrix,
            {"status": "pass", "evidence": evidence},
            here=settings.artifact_root,
            repo=settings.artifact_root,
            pins=pins,
            require_production=authority == PRODUCTION_AUTHORITY,
            pin_repo=REPO,
        )
        if errors:
            raise VerificationError("generated evidence failed the protocol checker:\n- " + "\n- ".join(errors))
        os.replace(temp_path, final_path)
    finally:
        temp_path.unlink(missing_ok=True)
    return final_path


def _execute(
    settings: Settings,
    pins: dict[str, Any],
    authority: str,
    *,
    run_command: RunCommand = subprocess.run,
    version_command: RunCommand = subprocess.run,
    server_context: ServerContext = managed_embedding_server,
) -> pathlib.Path:
    require(not settings.output_dir.exists(), f"output directory already exists: {settings.output_dir}")
    try:
        settings.output_dir.resolve().relative_to(settings.artifact_root.resolve())
    except ValueError as exc:
        raise VerificationError("--output-dir must be below --artifact-root") from exc
    matrix = json.loads(MATRIX_PATH.read_text(encoding="utf-8"))
    matrix_by_id = {arm["id"]: arm for arm in matrix["arms"]}
    require(tuple(matrix_by_id) == ARMS, "engine matrix arm order changed")
    pinned_url = matrix_by_id["embeddinggemma_rrf"]["environment"]["ENTIRE_BRAIN_EMBED_URL"]
    require(settings.embed_url == pinned_url, f"embedding URL must match the production matrix pin: {pinned_url}")
    validate_source_inputs(settings, pins, authority)

    # No retained bytes are created until the wrapper proves it can own the
    # matrix-pinned endpoint. An earlier diagnostic process is never reused.
    assert_pinned_loopback_endpoint_available(settings.embed_url)
    settings.output_dir.mkdir(parents=True)
    bundle = retain_runtime_bundle(settings, pins, version_command=version_command)
    descriptor = pin_descriptor(pins, authority)
    prepared = {arm: prepare_arm(settings, bundle, pins, arm) for arm in ARMS}
    require(len({str(item.data_dir.resolve()) for item in prepared.values()}) == len(ARMS), "derived data roots overlap")
    records = [
        run_arm(settings, matrix_by_id[arm], prepared[arm], bundle, pins, descriptor, run_command=run_command)
        for arm in ARMS[:2]
    ]
    with server_context(settings, bundle, pins) as evidence:
        embedding_record = run_arm(
            settings,
            matrix_by_id["embeddinggemma_rrf"],
            prepared["embeddinggemma_rrf"],
            bundle,
            pins,
            descriptor,
            run_command=run_command,
            during_observer=evidence.observe,
            recall_window_observer=evidence.record_recall_window,
        )
    add_server_evidence(embedding_record, evidence, bundle, settings)
    records.append(embedding_record)

    corpus_hash = pins["corpus"]["facts_sha256"]
    require_sha256(sha256_file(bundle.facts_source), corpus_hash, "retained facts after all arms")
    require_sha256(
        sha256_file(bundle.session_dates_source),
        pins["corpus"]["session_dates_sha256"],
        "retained session dates after all arms",
    )
    for arm, item in prepared.items():
        require_sha256(sha256_file(item.facts), corpus_hash, f"{arm} derived facts final recheck")
    return publish_manifest(settings, matrix, records, pins, authority)


def execute(settings: Settings) -> pathlib.Path:
    """Gate-authoritative production execution; pins cannot be caller-injected."""
    return _execute(settings, load_production_pins(), PRODUCTION_AUTHORITY)


def execute_with_test_pins(
    settings: Settings,
    pins: dict[str, Any],
    *,
    run_command: RunCommand,
    version_command: RunCommand,
    server_context: ServerContext,
) -> pathlib.Path:
    """Hermetic-only seam. Records are explicitly non-production authority."""
    require(pins.get("authority") == TEST_AUTHORITY, "test pin injection must declare test_fixture authority")
    return _execute(
        settings,
        pins,
        TEST_AUTHORITY,
        run_command=run_command,
        version_command=version_command,
        server_context=server_context,
    )


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
    parser.add_argument("--embedding-model", type=pathlib.Path, required=True)
    parser.add_argument("--embed-url", default="http://127.0.0.1:11500")
    parser.add_argument("--node-runtime", type=pathlib.Path, required=True)
    parser.add_argument("--runtime-dependency-root", type=pathlib.Path, required=True)
    parser.add_argument("--server-start-timeout-seconds", type=float, default=120.0)
    parser.add_argument("--server-health-interval-seconds", type=float, default=2.0)
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
        embedding_model=args.embedding_model,
        embed_url=args.embed_url,
        node_runtime=args.node_runtime,
        runtime_dependency_root=args.runtime_dependency_root,
        server_start_timeout_seconds=args.server_start_timeout_seconds,
        server_health_interval_seconds=args.server_health_interval_seconds,
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
