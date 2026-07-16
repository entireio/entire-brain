#!/usr/bin/env python3
"""Run the development-only, privacy-shaped `brain brief` profile corpus.

This runner deliberately retains no prompt, packet, stderr, sidecar path, brain
path, repository path, or wall-clock timestamp.  Source prompts and product
packets exist only in process memory; profile sidecars exist only in a private
temporary directory for the duration of one invocation.
"""

from __future__ import annotations

import argparse
import copy
import hashlib
import json
import os
import pathlib
import re
import statistics
import subprocess
import tempfile
from collections import Counter
from typing import Any, Iterable, Mapping, Sequence


REPO_ROOT = pathlib.Path(__file__).resolve().parents[2]
DEFAULT_CORPUS = pathlib.Path(__file__).with_name("brief-profile-corpus-v1.json")
CORPUS_SCHEMA_VERSION = 1
REPORT_SCHEMA_VERSION = 1
PROFILE_SCHEMA_VERSION = 1
OBSERVATION_LABELS = ("first_observation", "immediate_repeat")
LOGICAL_REPOS = ("entire-brain", "entire-cli", "entire-db")
PROFILE_ROLE = "development_only_brief_latency_and_packet_profile"
SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
GIT_SHA_RE = re.compile(r"^[0-9a-f]{40,64}$")
MAX_SIDECAR_BYTES = 1_000_000
MAX_PROFILE_INTEGER = (1 << 63) - 1

STAGE_PATHS: tuple[tuple[str, tuple[str, ...]], ...] = (
    ("total_brief", ("total_brief", "duration_ns")),
    ("status_build_state", ("status_build_state", "duration_ns")),
    ("semantic.context", ("semantic", "context", "duration_ns")),
    ("semantic.runtime_traces", ("semantic", "runtime_traces", "duration_ns")),
    ("semantic.tests", ("semantic", "tests", "duration_ns")),
    ("history.index_load", ("history", "index_load", "duration_ns")),
    ("history.indexed_rank", ("history", "indexed_rank", "duration_ns")),
    ("history.raw_fallback", ("history", "raw_fallback", "duration_ns")),
    ("facts.load", ("facts", "load", "duration_ns")),
    ("facts.vector_cache_load", ("facts", "vector_cache_load", "duration_ns")),
    ("facts.embed", ("facts", "embed", "duration_ns")),
    ("facts.rank", ("facts", "rank", "duration_ns")),
    ("facts.cache_flush", ("facts", "cache_flush", "duration_ns")),
    ("synthesis.likely_files", ("synthesis", "likely_files", "duration_ns")),
    ("synthesis.action_checklist", ("synthesis", "action_checklist", "duration_ns")),
    ("knowledge.patterns", ("knowledge", "patterns", "duration_ns")),
    ("knowledge.consolidations", ("knowledge", "consolidations", "duration_ns")),
    ("knowledge.themes", ("knowledge", "themes", "duration_ns")),
    ("packet.serialization", ("packet", "serialization", "duration_ns")),
)

VECTOR_CACHE_STATES = (
    "no_facts",
    "reranker_not_invoked",
    "empty_or_unused",
    "loaded_without_fact_embedding",
    "fact_embedding_from_empty_cache",
    "fact_embedding_with_existing_cache",
)


class ProfileRunError(RuntimeError):
    """A fail-closed corpus, metadata, or invocation error."""


class ProfileShapeError(ProfileRunError):
    """The sidecar was not the exact privacy-safe numeric contract."""


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


def _load_json_object(path: pathlib.Path, role: str) -> dict[str, Any]:
    try:
        value = json.loads(path.read_bytes())
    except (OSError, json.JSONDecodeError) as exc:
        raise ProfileRunError(f"cannot read valid {role} JSON") from exc
    if not isinstance(value, dict):
        raise ProfileRunError(f"{role} must be a JSON object")
    return value


def _source_config(path: pathlib.Path) -> tuple[bytes, dict[str, Any]]:
    try:
        raw = path.read_bytes()
        value = json.loads(raw)
    except (OSError, json.JSONDecodeError) as exc:
        raise ProfileRunError("selected task config is not readable valid JSON") from exc
    if not isinstance(value, dict):
        raise ProfileRunError("selected task config must be an object")
    if not isinstance(value.get("id"), str) or not value["id"]:
        raise ProfileRunError("selected task config has no nonempty id")
    if value.get("repo") not in LOGICAL_REPOS:
        raise ProfileRunError("selected task config has an unsupported logical repo")
    if not isinstance(value.get("prompt"), str) or not value["prompt"]:
        raise ProfileRunError("selected task config has no nonempty prompt")
    return raw, value


def selected_source_paths(repo_root: pathlib.Path = REPO_ROOT) -> list[pathlib.Path]:
    agent_brain = repo_root / "benchmarks" / "agent-brain"
    selected: list[pathlib.Path] = []
    for path in sorted((agent_brain / "tasks").glob("*.json")):
        try:
            candidate = json.loads(path.read_bytes())
        except (OSError, json.JSONDecodeError) as exc:
            raise ProfileRunError("task inventory contains unreadable invalid JSON") from exc
        if not isinstance(candidate, dict):
            raise ProfileRunError("task inventory entry must be a JSON object")
        if candidate.get("repo") in {"entire-brain", "entire-cli"}:
            _source_config(path)
            selected.append(path)
    for logical_repo in ("entire-cli", "entire-db"):
        paths = sorted((agent_brain / "mined-scale").glob(f"{logical_repo}-scale-*.json"))
        for path in paths:
            _, value = _source_config(path)
            if value["repo"] != logical_repo:
                raise ProfileRunError("mined-scale filename and logical repo disagree")
            selected.append(path)
    return sorted(selected, key=lambda path: path.relative_to(repo_root).as_posix())


def task_identity_sha256(
    logical_repo: str,
    source_path: str,
    config_sha256: str,
    prompt_sha256: str,
) -> str:
    identity = {
        "config_sha256": config_sha256,
        "logical_repo": logical_repo,
        "prompt_sha256": prompt_sha256,
        "source_path": source_path,
    }
    return sha256_bytes(canonical_json_bytes(identity))


def build_corpus_payload(repo_root: pathlib.Path = REPO_ROOT) -> dict[str, Any]:
    source_paths = selected_source_paths(repo_root)
    source_counts: Counter[str] = Counter()
    unique_counts: Counter[str] = Counter()
    seen: set[tuple[str, str]] = set()
    tasks: list[dict[str, str]] = []

    for path in source_paths:
        raw, value = _source_config(path)
        logical_repo = value["repo"]
        source_counts[logical_repo] += 1
        prompt_sha = sha256_bytes(value["prompt"].encode("utf-8"))
        dedupe_key = (logical_repo, prompt_sha)
        if dedupe_key in seen:
            continue
        seen.add(dedupe_key)
        relative = path.relative_to(repo_root).as_posix()
        config_sha = sha256_bytes(raw)
        tasks.append(
            {
                "logical_repo": logical_repo,
                "source_path": relative,
                "task_sha256": task_identity_sha256(logical_repo, relative, config_sha, prompt_sha),
                "config_sha256": config_sha,
                "prompt_sha256": prompt_sha,
            }
        )
        unique_counts[logical_repo] += 1

    tasks.sort(key=lambda task: (task["logical_repo"], task["source_path"]))
    body: dict[str, Any] = {
        "schema_version": CORPUS_SCHEMA_VERSION,
        "evidence_role": PROFILE_ROLE,
        "confirmatory_eligible": False,
        "quality_eligible": False,
        "selection": {
            "tasks_scope": "tasks/*.json where repo is entire-brain or entire-cli",
            "mined_scale_scope": ["entire-cli-scale-*.json", "entire-db-scale-*.json"],
            "dedupe_key": "logical_repo+prompt_sha256",
            "dedupe_resolution": "lexicographically_first_checked_in_relative_path",
            "source_file_count": len(source_paths),
            "deduplicated_task_count": len(tasks),
            "duplicates_removed": len(source_paths) - len(tasks),
            "source_count_by_repo": {name: source_counts[name] for name in LOGICAL_REPOS},
            "unique_count_by_repo": {name: unique_counts[name] for name in LOGICAL_REPOS},
        },
        "tasks": tasks,
    }
    return attach_self_hash(body, "corpus_sha256")


def load_verified_corpus(
    corpus_path: pathlib.Path = DEFAULT_CORPUS,
    repo_root: pathlib.Path = REPO_ROOT,
) -> dict[str, Any]:
    actual = _load_json_object(corpus_path, "brief profile corpus")
    if not verify_self_hash(actual, "corpus_sha256"):
        raise ProfileRunError("brief profile corpus self-hash mismatch")
    expected = build_corpus_payload(repo_root)
    if actual != expected:
        raise ProfileRunError("brief profile corpus does not match checked-in task inventory")
    return actual


def load_verified_prompts(
    corpus: Mapping[str, Any], repo_root: pathlib.Path = REPO_ROOT
) -> list[dict[str, str]]:
    prompts: list[dict[str, str]] = []
    for entry in corpus["tasks"]:
        relative = entry["source_path"]
        relative_path = pathlib.PurePosixPath(relative)
        if relative_path.is_absolute() or ".." in relative_path.parts:
            raise ProfileRunError("corpus source path is not a safe checked-in relative path")
        path = repo_root.joinpath(*relative_path.parts)
        try:
            path.resolve(strict=True).relative_to(repo_root.resolve(strict=True))
        except (OSError, ValueError) as exc:
            raise ProfileRunError("corpus source path escapes the source repository") from exc
        raw, value = _source_config(path)
        config_sha = sha256_bytes(raw)
        prompt_sha = sha256_bytes(value["prompt"].encode("utf-8"))
        task_sha = task_identity_sha256(value["repo"], relative, config_sha, prompt_sha)
        if (
            value["repo"] != entry["logical_repo"]
            or config_sha != entry["config_sha256"]
            or prompt_sha != entry["prompt_sha256"]
            or task_sha != entry["task_sha256"]
        ):
            raise ProfileRunError("task config changed after corpus verification")
        prompts.append(
            {
                "logical_repo": value["repo"],
                "task_sha256": task_sha,
                "prompt_sha256": prompt_sha,
                "prompt": value["prompt"],
            }
        )
    return prompts


def _exact_keys(value: Any, keys: Iterable[str], role: str) -> dict[str, Any]:
    if not isinstance(value, dict) or set(value) != set(keys):
        raise ProfileShapeError(f"unexpected fields in {role}")
    return value


def _nonnegative_int(value: Any, role: str) -> int:
    if (
        isinstance(value, bool)
        or not isinstance(value, int)
        or value < 0
        or value > MAX_PROFILE_INTEGER
    ):
        raise ProfileShapeError(f"{role} must be a nonnegative integer")
    return value


def _strict_sidecar_json(data: bytes) -> Any:
    def reject_duplicate_keys(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
        result: dict[str, Any] = {}
        for key, value in pairs:
            if key in result:
                raise ProfileShapeError("sidecar contains a duplicate JSON key")
            result[key] = value
        return result

    def reject_constant(_: str) -> Any:
        raise ProfileShapeError("sidecar contains a non-finite JSON number")

    try:
        return json.loads(
            data,
            object_pairs_hook=reject_duplicate_keys,
            parse_constant=reject_constant,
        )
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise ProfileShapeError("sidecar is not strict UTF-8 JSON") from exc


def _boolean(value: Any, role: str) -> bool:
    if not isinstance(value, bool):
        raise ProfileShapeError(f"{role} must be boolean")
    return value


def _validate_duration(value: Any, role: str) -> None:
    obj = _exact_keys(value, ("duration_ns",), role)
    _nonnegative_int(obj["duration_ns"], role + ".duration_ns")


def _validate_stage(value: Any, role: str) -> None:
    obj = _exact_keys(
        value,
        ("invoked", "duration_ns", "input_count", "output_count", "error_count"),
        role,
    )
    invoked = _boolean(obj["invoked"], role + ".invoked")
    numeric = [
        _nonnegative_int(obj[name], role + "." + name)
        for name in ("duration_ns", "input_count", "output_count", "error_count")
    ]
    if not invoked and any(numeric):
        raise ProfileShapeError(f"uninvoked {role} has nonzero measurements")


def _validate_embedding(value: Any, role: str) -> None:
    obj = _exact_keys(
        value,
        (
            "invoked",
            "duration_ns",
            "query_call_count",
            "fact_call_count",
            "valid_vector_count",
            "invalid_vector_count",
        ),
        role,
    )
    invoked = _boolean(obj["invoked"], role + ".invoked")
    numeric = {
        name: _nonnegative_int(obj[name], role + "." + name)
        for name in (
            "duration_ns",
            "query_call_count",
            "fact_call_count",
            "valid_vector_count",
            "invalid_vector_count",
        )
    }
    if not invoked and any(numeric.values()):
        raise ProfileShapeError("uninvoked facts.embed has nonzero measurements")
    calls = numeric["query_call_count"] + numeric["fact_call_count"]
    vectors = numeric["valid_vector_count"] + numeric["invalid_vector_count"]
    if calls != vectors:
        raise ProfileShapeError("facts.embed call/vector counts disagree")


def _validate_raw_history(value: Any) -> None:
    role = "history.raw_fallback"
    obj = _exact_keys(
        value,
        (
            "invoked",
            "duration_ns",
            "query_count",
            "scanned_file_count",
            "scanned_byte_count",
            "match_count",
            "truncation_count",
            "error_count",
            "queries",
        ),
        role,
    )
    invoked = _boolean(obj["invoked"], role + ".invoked")
    names = (
        "duration_ns",
        "query_count",
        "scanned_file_count",
        "scanned_byte_count",
        "match_count",
        "truncation_count",
        "error_count",
    )
    numeric = {name: _nonnegative_int(obj[name], role + "." + name) for name in names}
    queries = obj["queries"]
    if not isinstance(queries, list) or len(queries) > 8:
        raise ProfileShapeError("history.raw_fallback.queries must be a list of at most eight rows")
    sums = Counter()
    for index, value in enumerate(queries, start=1):
        query = _exact_keys(
            value,
            (
                "ordinal",
                "duration_ns",
                "scanned_file_count",
                "scanned_byte_count",
                "match_count",
                "truncated",
                "error_count",
            ),
            "history.raw_fallback.queries[]",
        )
        if _nonnegative_int(query["ordinal"], "raw query ordinal") != index:
            raise ProfileShapeError("raw query ordinals must be consecutive from one")
        for name in (
            "duration_ns",
            "scanned_file_count",
            "scanned_byte_count",
            "match_count",
            "error_count",
        ):
            sums[name] += _nonnegative_int(query[name], "raw query " + name)
        if _boolean(query["truncated"], "raw query truncated"):
            sums["truncation_count"] += 1
    if numeric["query_count"] != len(queries):
        raise ProfileShapeError("raw query count disagrees with rows")
    for aggregate, query_name in (
        ("scanned_file_count", "scanned_file_count"),
        ("scanned_byte_count", "scanned_byte_count"),
        ("match_count", "match_count"),
        ("truncation_count", "truncation_count"),
        ("error_count", "error_count"),
    ):
        if numeric[aggregate] != sums[query_name]:
            raise ProfileShapeError("raw history aggregate disagrees with query rows")
    if not invoked and (queries or any(numeric.values())):
        raise ProfileShapeError("uninvoked raw fallback has nonzero measurements")


def validate_numeric_profile(value: Any, packet_byte_count: int) -> dict[str, Any]:
    """Validate the exact sidecar contract and return a string-free copy."""

    obj = _exact_keys(
        value,
        (
            "schema_version",
            "duration_unit",
            "total_brief",
            "status_build_state",
            "semantic",
            "history",
            "facts",
            "synthesis",
            "knowledge",
            "packet",
        ),
        "profile",
    )
    if obj["schema_version"] != PROFILE_SCHEMA_VERSION:
        raise ProfileShapeError("unsupported sidecar schema version")
    if obj["duration_unit"] != "nanoseconds":
        raise ProfileShapeError("unsupported sidecar duration unit")
    _validate_duration(obj["total_brief"], "total_brief")
    _validate_stage(obj["status_build_state"], "status_build_state")

    semantic = _exact_keys(obj["semantic"], ("context", "runtime_traces", "tests"), "semantic")
    for name in ("context", "runtime_traces", "tests"):
        _validate_stage(semantic[name], "semantic." + name)

    history = _exact_keys(obj["history"], ("index_load", "indexed_rank", "raw_fallback"), "history")
    _validate_stage(history["index_load"], "history.index_load")
    _validate_stage(history["indexed_rank"], "history.indexed_rank")
    _validate_raw_history(history["raw_fallback"])

    facts = _exact_keys(
        obj["facts"],
        ("load", "vector_cache_load", "embed", "rank", "cache_flush"),
        "facts",
    )
    for name in ("load", "vector_cache_load", "rank", "cache_flush"):
        _validate_stage(facts[name], "facts." + name)
    _validate_embedding(facts["embed"], "facts.embed")

    synthesis = _exact_keys(obj["synthesis"], ("likely_files", "action_checklist"), "synthesis")
    for name in ("likely_files", "action_checklist"):
        _validate_stage(synthesis[name], "synthesis." + name)

    knowledge = _exact_keys(obj["knowledge"], ("patterns", "consolidations", "themes"), "knowledge")
    for name in ("patterns", "consolidations", "themes"):
        _validate_stage(knowledge[name], "knowledge." + name)

    packet = _exact_keys(obj["packet"], ("format", "serialization", "byte_count", "counts"), "packet")
    if packet["format"] != "json":
        raise ProfileShapeError("profile packet format must be json")
    _validate_stage(packet["serialization"], "packet.serialization")
    profiled_packet_bytes = _nonnegative_int(packet["byte_count"], "packet.byte_count")
    if profiled_packet_bytes != packet_byte_count:
        raise ProfileShapeError("profile packet byte count disagrees with captured stdout")
    count_names = (
        "semantic_symbols",
        "semantic_relations",
        "semantic_neighbors",
        "runtime_traces",
        "test_roots",
        "test_suggestions",
        "history_matches",
        "facts",
        "facts_with_locus_drift",
        "actions",
        "likely_edit_files",
        "likely_test_files",
        "likely_files",
        "patterns",
        "consolidations",
        "themes",
        "guidance_items",
        "warnings",
    )
    counts = _exact_keys(packet["counts"], count_names, "packet.counts")
    for name in count_names:
        _nonnegative_int(counts[name], "packet.counts." + name)

    numeric = copy.deepcopy(obj)
    numeric.pop("duration_unit")
    numeric["packet"].pop("format")
    return numeric


def observed_vector_cache_state(numeric_profile: Mapping[str, Any]) -> dict[str, Any]:
    facts = numeric_profile["facts"]
    fact_count = facts["load"]["output_count"]
    loaded_count = facts["vector_cache_load"]["output_count"]
    fact_embed_count = facts["embed"]["fact_call_count"]
    query_embed_count = facts["embed"]["query_call_count"]
    if fact_count == 0:
        state = "no_facts"
    elif not facts["vector_cache_load"]["invoked"]:
        state = "reranker_not_invoked"
    elif loaded_count == 0 and fact_embed_count == 0:
        state = "empty_or_unused"
    elif loaded_count > 0 and fact_embed_count == 0:
        state = "loaded_without_fact_embedding"
    elif loaded_count == 0 and fact_embed_count > 0:
        state = "fact_embedding_from_empty_cache"
    else:
        state = "fact_embedding_with_existing_cache"
    return {
        "state": state,
        "fact_count": fact_count,
        "loaded_vector_count": loaded_count,
        "fact_embed_call_count": fact_embed_count,
        "query_embed_call_count": query_embed_count,
    }


def parse_repo_mappings(values: Sequence[str]) -> dict[str, pathlib.Path]:
    mappings: dict[str, pathlib.Path] = {}
    for value in values:
        if "=" not in value:
            raise ProfileRunError("--repo must use logical=path")
        logical, raw_path = value.split("=", 1)
        if logical not in LOGICAL_REPOS or not raw_path or logical in mappings:
            raise ProfileRunError("--repo contains an unsupported or duplicate logical repo")
        path = pathlib.Path(raw_path).expanduser().resolve()
        if not path.is_dir():
            raise ProfileRunError("mapped repository is not a directory")
        mappings[logical] = path
    return mappings


def _run_private(
    argv: Sequence[str], cwd: pathlib.Path, timeout_seconds: float
) -> subprocess.CompletedProcess[bytes]:
    return subprocess.run(
        list(argv),
        cwd=cwd,
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
        timeout=timeout_seconds,
    )


def _repo_head(repo: pathlib.Path, timeout_seconds: float) -> str:
    try:
        result = _run_private(("git", "rev-parse", "--verify", "HEAD^{commit}"), repo, timeout_seconds)
    except (OSError, subprocess.TimeoutExpired) as exc:
        raise ProfileRunError("cannot resolve a mapped repository HEAD") from exc
    if result.returncode != 0:
        raise ProfileRunError("cannot resolve a mapped repository HEAD")
    try:
        head = result.stdout.decode("ascii").strip()
    except UnicodeDecodeError as exc:
        raise ProfileRunError("repository HEAD is not ASCII") from exc
    if not GIT_SHA_RE.fullmatch(head):
        raise ProfileRunError("repository HEAD is not a full object hash")
    return head


def _brain_manifest_hash(
    brain_bin: pathlib.Path, repo: pathlib.Path, timeout_seconds: float
) -> str:
    try:
        result = _run_private((str(brain_bin), "path", "."), repo, timeout_seconds)
    except (OSError, subprocess.TimeoutExpired) as exc:
        raise ProfileRunError("cannot locate a repository brain manifest") from exc
    if result.returncode != 0:
        raise ProfileRunError("cannot locate a repository brain manifest")
    try:
        brain_path = result.stdout.decode("utf-8").strip()
    except UnicodeDecodeError as exc:
        raise ProfileRunError("brain path was not valid UTF-8") from exc
    if not brain_path:
        raise ProfileRunError("brain path was empty")
    manifest = pathlib.Path(brain_path) / "manifest.json"
    try:
        if manifest.is_symlink() or not manifest.is_file():
            raise ProfileRunError("brain manifest is not a regular non-symlink file")
        data = manifest.read_bytes()
    except OSError as exc:
        raise ProfileRunError("brain manifest is not readable") from exc
    return sha256_bytes(data)


def collect_run_metadata(
    brain_bin: pathlib.Path,
    repo_mappings: Mapping[str, pathlib.Path],
    required_repos: Iterable[str],
    timeout_seconds: float,
) -> dict[str, Any]:
    if not brain_bin.is_file():
        raise ProfileRunError("--brain-bin is not a file")
    try:
        binary_sha = sha256_bytes(brain_bin.read_bytes())
    except OSError as exc:
        raise ProfileRunError("--brain-bin is not readable") from exc
    required = sorted(set(required_repos))
    if set(repo_mappings) != set(required):
        raise ProfileRunError("--repo mappings must exactly cover the corpus logical repos")
    repos: dict[str, Any] = {}
    for logical in required:
        repo = repo_mappings[logical]
        repos[logical] = {
            "head_sha": _repo_head(repo, timeout_seconds),
            "brain_manifest_sha256": _brain_manifest_hash(brain_bin, repo, timeout_seconds),
        }
    return {"brain_binary_sha256": binary_sha, "repos": repos}


def build_schedule(tasks: Sequence[Mapping[str, str]]) -> dict[str, Any]:
    entries: list[dict[str, Any]] = []
    sequence = 0
    for task in tasks:
        for label in OBSERVATION_LABELS:
            entries.append(
                {
                    "sequence": sequence,
                    "logical_repo": task["logical_repo"],
                    "task_sha256": task["task_sha256"],
                    "observation_label": label,
                }
            )
            sequence += 1
    return {
        "policy": "corpus_order_with_consecutive_two_observations_per_task",
        "task_count": len(tasks),
        "observation_count": len(entries),
        "entries_sha256": sha256_bytes(canonical_json_bytes(entries)),
        "entries": entries,
    }


def _stderr_bytes(value: Any) -> bytes:
    if value is None:
        return b""
    if isinstance(value, bytes):
        return value
    return str(value).encode("utf-8", errors="replace")


def _failure(
    kind: str,
    stderr: bytes,
    return_code: int | None,
) -> dict[str, Any]:
    return {
        "kind": kind,
        "return_code": return_code,
        "stderr_byte_count": len(stderr),
        "stderr_sha256": sha256_bytes(stderr),
    }


def run_observation(
    brain_bin: pathlib.Path,
    repo: pathlib.Path,
    prompt: str,
    timeout_seconds: float,
) -> dict[str, Any]:
    stderr = b""
    with tempfile.TemporaryDirectory(prefix="entire-brain-brief-profile-") as private_dir:
        sidecar_path = pathlib.Path(private_dir) / "profile.json"
        argv = (
            str(brain_bin),
            "brief",
            prompt,
            "--json",
            "--profile-json",
            str(sidecar_path),
        )
        try:
            result = _run_private(argv, repo, timeout_seconds)
        except subprocess.TimeoutExpired as exc:
            return {"status": "failed", "failure": _failure("timeout", _stderr_bytes(exc.stderr), None)}
        except OSError:
            return {"status": "failed", "failure": _failure("launch_error", b"", None)}
        stderr = result.stderr
        if result.returncode != 0:
            return {
                "status": "failed",
                "failure": _failure("nonzero_exit", stderr, result.returncode),
            }
        if not sidecar_path.is_file() or sidecar_path.is_symlink():
            return {
                "status": "failed",
                "failure": _failure("missing_or_unsafe_sidecar", stderr, result.returncode),
            }
        try:
            sidecar_stat = sidecar_path.stat()
            if sidecar_stat.st_mode & 0o077:
                raise ProfileShapeError("sidecar permissions are not private")
            if sidecar_stat.st_size <= 0 or sidecar_stat.st_size > MAX_SIDECAR_BYTES:
                raise ProfileShapeError("sidecar exceeds the fixed size ceiling")
            profile_value = _strict_sidecar_json(sidecar_path.read_bytes())
            numeric_profile = validate_numeric_profile(profile_value, len(result.stdout))
        except (OSError, json.JSONDecodeError, ProfileShapeError):
            return {
                "status": "failed",
                "failure": _failure("invalid_sidecar", stderr, result.returncode),
            }
        return {
            "status": "ok",
            "packet": {
                "byte_count": len(result.stdout),
                "sha256": sha256_bytes(result.stdout),
            },
            "numeric_profile": numeric_profile,
            "observed_fact_vector_cache": observed_vector_cache_state(numeric_profile),
        }


def _get_path(value: Mapping[str, Any], path: Sequence[str]) -> Any:
    current: Any = value
    for part in path:
        current = current[part]
    return current


def numeric_summary(values: Sequence[int]) -> dict[str, Any]:
    if not values:
        return {"count": 0, "sum": 0, "minimum": None, "maximum": None, "median": None}
    return {
        "count": len(values),
        "sum": sum(values),
        "minimum": min(values),
        "maximum": max(values),
        "median": statistics.median(values),
    }


def aggregate_observations(observations: Sequence[Mapping[str, Any]]) -> dict[str, Any]:
    aggregates: dict[str, Any] = {}
    labels = (*OBSERVATION_LABELS, "all_observations")
    for logical_repo in LOGICAL_REPOS:
        repo_rows = [row for row in observations if row["logical_repo"] == logical_repo]
        if not repo_rows:
            continue
        repo_aggregate: dict[str, Any] = {}
        for label in labels:
            rows = repo_rows if label == "all_observations" else [
                row for row in repo_rows if row["observation_label"] == label
            ]
            successes = [row for row in rows if row["status"] == "ok"]
            failures = Counter(
                row["failure"]["kind"] for row in rows if row["status"] == "failed"
            )
            stage_values: dict[str, Any] = {}
            for stage_name, stage_path in STAGE_PATHS:
                stage_values[stage_name] = numeric_summary(
                    [_get_path(row["numeric_profile"], stage_path) for row in successes]
                )
            cache_states = Counter(
                row["observed_fact_vector_cache"]["state"] for row in successes
            )
            repo_aggregate[label] = {
                "scheduled_observations": len(rows),
                "successful_observations": len(successes),
                "failure_count_by_kind": dict(sorted(failures.items())),
                "packet_byte_count": numeric_summary(
                    [row["packet"]["byte_count"] for row in successes]
                ),
                "stage_duration_ns": stage_values,
                "fact_vector_cache_state_count": {
                    state: cache_states[state] for state in VECTOR_CACHE_STATES
                },
            }
        aggregates[logical_repo] = repo_aggregate
    return aggregates


def build_profile_report(
    corpus: Mapping[str, Any],
    tasks: Sequence[Mapping[str, str]],
    brain_bin: pathlib.Path,
    repo_mappings: Mapping[str, pathlib.Path],
    timeout_seconds: float,
) -> dict[str, Any]:
    required_repos = {task["logical_repo"] for task in tasks}
    metadata = collect_run_metadata(brain_bin, repo_mappings, required_repos, timeout_seconds)
    schedule = build_schedule(tasks)
    observations: list[dict[str, Any]] = []
    failures: list[dict[str, Any]] = []

    task_by_sha = {task["task_sha256"]: task for task in tasks}
    for scheduled in schedule["entries"]:
        task = task_by_sha[scheduled["task_sha256"]]
        result = run_observation(
            brain_bin,
            repo_mappings[task["logical_repo"]],
            task["prompt"],
            timeout_seconds,
        )
        row = {
            "sequence": scheduled["sequence"],
            "logical_repo": scheduled["logical_repo"],
            "task_sha256": scheduled["task_sha256"],
            "prompt_sha256": task["prompt_sha256"],
            "observation_label": scheduled["observation_label"],
            **result,
        }
        observations.append(row)
        if result["status"] == "failed":
            failures.append(
                {
                    "sequence": row["sequence"],
                    "logical_repo": row["logical_repo"],
                    "task_sha256": row["task_sha256"],
                    "observation_label": row["observation_label"],
                    **result["failure"],
                }
            )

    body = {
        "schema_version": REPORT_SCHEMA_VERSION,
        "evidence_role": PROFILE_ROLE,
        "confirmatory_eligible": False,
        "quality_eligible": False,
        "corpus": {
            "schema_version": corpus["schema_version"],
            "corpus_sha256": corpus["corpus_sha256"],
            "full_task_count": len(corpus["tasks"]),
            "selected_task_count": len(tasks),
            "complete_corpus": len(tasks) == len(corpus["tasks"]),
        },
        "runtime_identity": metadata,
        "schedule": schedule,
        "observations": observations,
        "aggregates_by_repo": aggregate_observations(observations),
        "failures": failures,
    }
    return attach_self_hash(body, "report_sha256")


def write_private_json(path: pathlib.Path, value: Mapping[str, Any]) -> None:
    path = path.expanduser().resolve()
    path.parent.mkdir(parents=True, exist_ok=True)
    data = json.dumps(value, indent=2, sort_keys=True, ensure_ascii=False, allow_nan=False).encode("utf-8") + b"\n"
    fd, temporary = tempfile.mkstemp(prefix=".brief-profile-report-", dir=path.parent)
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
        help="Run only the deterministic corpus prefix for an explicitly incomplete smoke profile",
    )
    return parser.parse_args(argv)


def main(argv: Sequence[str] | None = None) -> int:
    args = parse_args(argv)
    if args.timeout_seconds <= 0:
        raise SystemExit("--timeout-seconds must be positive")
    if args.max_tasks is not None and args.max_tasks <= 0:
        raise SystemExit("--max-tasks must be positive")
    try:
        corpus = load_verified_corpus(args.corpus, REPO_ROOT)
        tasks = load_verified_prompts(corpus, REPO_ROOT)
        if args.max_tasks is not None:
            tasks = tasks[: args.max_tasks]
        mappings = parse_repo_mappings(args.repo)
        report = build_profile_report(
            corpus,
            tasks,
            args.brain_bin.expanduser().resolve(),
            mappings,
            args.timeout_seconds,
        )
        write_private_json(args.output, report)
    except ProfileRunError as exc:
        raise SystemExit(str(exc)) from exc
    print(
        json.dumps(
            {
                "report_sha256": report["report_sha256"],
                "observations": len(report["observations"]),
                "failures": len(report["failures"]),
                "complete_corpus": report["corpus"]["complete_corpus"],
            },
            sort_keys=True,
        )
    )
    return 0 if not report["failures"] else 2


if __name__ == "__main__":
    raise SystemExit(main())
