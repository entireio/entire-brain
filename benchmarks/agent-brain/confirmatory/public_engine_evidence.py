#!/usr/bin/env python3
"""Project validated private engine evidence into a privacy-safe public v4 bundle.

The projector consumes the exact-byte diagnostic evidence only after the legacy
checker accepts it.  Its output intentionally contains no source corpus, session
map, model/runtime payload, vector floats, raw process output, host path, command,
or environment value.  Public verification operates on typed invocations,
component commitments, a pseudonymous temporal projection, sanitized ranked IDs,
and digest/size commitments to restricted raw streams.
"""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import hmac
import json
import math
import os
import pathlib
import re
import secrets
import shutil
import stat
import struct
import sys
from typing import Any, Sequence


HERE = pathlib.Path(__file__).resolve().parent
REPO = HERE.parents[2]
ARMS = ("lexical_handrolled", "model2vec_rrf", "embeddinggemma_rrf")
MANIFEST_NAME = "engine-verification-public-v4.json"
PROFILE = "public_privacy_safe_v4"
PSEUDONYM_ALGORITHM = "hmac_sha256_domain_separated_128bit_v1"
INVENTORY_ALGORITHM = "sha256_ordered_relative_path_nul_sha256_nul_size_newline_v1"
VECTOR_INDEX_ALGORITHM = "sha256_chunked_ordered_candidate_refs_v1"
CHAIN_ALGORITHM = "sha256_canonical_previous_subject_v1"
REF_RE = re.compile(r"^(candidate|session):[0-9a-f]{32}$")
SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
RAW_SESSION_RE = re.compile(
    r"(?:\d{4}-\d{2}-\d{2}-)?[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}",
    re.IGNORECASE,
)
EMAIL_RE = re.compile(r"[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}", re.IGNORECASE)
JWT_RE = re.compile(r"eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}")
SECRET_RES = (
    re.compile(r"-----BEGIN [A-Z ]*PRIVATE KEY-----"),
    re.compile(r"AKIA[0-9A-Z]{16}"),
    re.compile(r"gh[opusr]_[A-Za-z0-9]{20,}"),
    re.compile(r"sk-[A-Za-z0-9_-]{20,}"),
    re.compile(r"xox[baprs]-[A-Za-z0-9-]{10,}"),
    JWT_RE,
)
FORBIDDEN_KEYS = {
    "author",
    "command",
    "email",
    "environment",
    "fact_id",
    "fact_ids",
    "fact_text",
    "home",
    "namespace",
    "query_text",
    "repo_root",
    "session_id",
    "session_ids",
    "source_path",
    "text",
    "user",
    "username",
}


class PublicEvidenceError(RuntimeError):
    """Raised when a safe projection cannot be established without inference."""


def _require(condition: bool, message: str) -> None:
    if not condition:
        raise PublicEvidenceError(message)


def canonical_json_bytes(value: Any) -> bytes:
    return json.dumps(
        value,
        ensure_ascii=False,
        allow_nan=False,
        sort_keys=True,
        separators=(",", ":"),
    ).encode("utf-8")


def canonical_sha256(value: Any) -> str:
    return hashlib.sha256(canonical_json_bytes(value)).hexdigest()


def sha256_bytes(value: bytes) -> str:
    return hashlib.sha256(value).hexdigest()


def sha256_file(path: pathlib.Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _parse_time(value: Any) -> dt.datetime:
    _require(isinstance(value, str) and bool(value), "temporal timestamp is missing")
    try:
        parsed = dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError as exc:
        raise PublicEvidenceError(f"invalid temporal timestamp: {value!r}") from exc
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=dt.UTC)
    return parsed.astimezone(dt.UTC)


def _public_time(value: Any) -> str:
    return _parse_time(value).isoformat().replace("+00:00", "Z")


def _safe_private_file(root: pathlib.Path, raw: Any, label: str) -> pathlib.Path:
    _require(isinstance(raw, str) and bool(raw), f"{label} path is missing")
    relative = pathlib.Path(raw)
    _require(not relative.is_absolute() and ".." not in relative.parts, f"{label} path is unsafe")
    root = root.resolve()
    target = (root / relative).resolve()
    try:
        target.relative_to(root)
    except ValueError as exc:
        raise PublicEvidenceError(f"{label} escapes the private artifact root") from exc
    _require(target.is_file() and not target.is_symlink(), f"{label} is not a real regular file")
    return target


def _write_json(path: pathlib.Path, value: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(canonical_json_bytes(value) + b"\n")


def _exact_object(value: Any, fields: set[str], label: str, errors: list[str]) -> bool:
    if not isinstance(value, dict):
        errors.append(f"{label} must be an object")
        return False
    if set(value) != fields:
        errors.append(f"{label} must contain exactly {', '.join(sorted(fields))}")
        return False
    return True


def _component_commitments(pins: dict[str, Any]) -> list[dict[str, Any]]:
    binary = pins.get("binary", {})
    corpus = pins.get("corpus", {})
    model = pins.get("embedding_model", {})
    runtime = pins.get("runtime", {})
    return [
        {
            "component": "entire_brain_binary",
            "sha256": binary.get("binary_sha256"),
            "size_bytes": binary.get("binary_size_bytes"),
            "source_commit": binary.get("source_commit"),
            "source_tree": binary.get("source_tree"),
        },
        {
            "component": "facts_source",
            "sha256": corpus.get("facts_sha256"),
            "size_bytes": corpus.get("facts_size_bytes"),
        },
        {
            "component": "session_dates_source",
            "sha256": corpus.get("session_dates_sha256"),
            "size_bytes": corpus.get("session_dates_size_bytes"),
        },
        {
            "component": "embedding_model",
            "sha256": model.get("sha256"),
            "size_bytes": model.get("size_bytes"),
            "model_id": model.get("model_id"),
        },
        {
            "component": "node_runtime",
            "sha256": runtime.get("node_sha256"),
            "version": runtime.get("node_version"),
        },
        {
            "component": "embedding_server_script",
            "sha256": runtime.get("server_script_sha256"),
        },
        {
            "component": "runtime_package_manifest",
            "sha256": runtime.get("package_manifest_sha256"),
        },
        {
            "component": "runtime_package_lock",
            "sha256": runtime.get("package_lock_sha256"),
        },
        {
            "component": "runtime_dependency_tree",
            "sha256": runtime.get("dependency_inventory_sha256"),
            "size_bytes": runtime.get("dependency_total_bytes"),
            "file_count": runtime.get("dependency_file_count"),
            "algorithm": runtime.get("dependency_inventory_algorithm"),
        },
    ]


class _Pseudonymizer:
    def __init__(self, key: bytes):
        _require(isinstance(key, bytes) and len(key) >= 32, "pseudonym key must contain at least 256 bits")
        self._key = key
        self._raw_by_ref: dict[str, str] = {}

    def ref(self, domain: str, raw: str) -> str:
        _require(domain in {"candidate", "session"}, "unknown pseudonym domain")
        _require(isinstance(raw, str) and bool(raw), f"empty raw {domain} identifier")
        message = b"entire-brain-engine-evidence-v4\0" + domain.encode() + b"\0" + raw.encode("utf-8")
        digest = hmac.new(self._key, message, hashlib.sha256).hexdigest()[:32]
        ref = f"{domain}:{digest}"
        previous = self._raw_by_ref.setdefault(ref, raw)
        _require(previous == raw, f"{domain} pseudonym collision")
        return ref


def _candidate_reason(
    candidate: dict[str, Any],
    sessions: dict[str, dict[str, Any]],
    cutoff: dt.datetime,
) -> str | None:
    if candidate["invalid_provenance"] or not candidate["session_refs"]:
        return "empty_provenance"
    states = [sessions[ref]["state"] for ref in candidate["session_refs"]]
    if "excluded" in states:
        return "excluded_session"
    if "unknown" in states:
        return "unknown_session"
    if any(_parse_time(sessions[ref]["created_at"]) >= cutoff for ref in candidate["session_refs"]):
        return "at_or_after_cutoff"
    return None


def _build_temporal_projection(
    facts_path: pathlib.Path,
    sessions_path: pathlib.Path,
    pins: dict[str, Any],
    pseudonyms: _Pseudonymizer,
) -> tuple[dict[str, Any], dict[str, str]]:
    corpus = pins["corpus"]
    _require(sha256_file(facts_path) == corpus["facts_sha256"], "facts source differs from pin")
    _require(facts_path.stat().st_size == corpus["facts_size_bytes"], "facts source size differs from pin")
    _require(sha256_file(sessions_path) == corpus["session_dates_sha256"], "session dates differ from pin")
    _require(sessions_path.stat().st_size == corpus["session_dates_size_bytes"], "session dates size differs from pin")
    try:
        session_dates = json.loads(sessions_path.read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise PublicEvidenceError(f"session dates are unreadable: {exc}") from exc
    _require(
        isinstance(session_dates, dict)
        and all(isinstance(key, str) and isinstance(value, str) for key, value in session_dates.items()),
        "session dates must be a string-to-string object",
    )
    task = pins["development_task"]
    cutoff = _parse_time(task["eligible_before"])
    excluded_raw = {
        item.strip() for item in task["exclude_session_ids"] if isinstance(item, str) and item.strip()
    }
    sessions: dict[str, dict[str, Any]] = {}
    candidates: list[dict[str, Any]] = []
    raw_by_candidate_ref: dict[str, str] = {}
    try:
        lines = facts_path.read_text(encoding="utf-8").splitlines()
        for line_number, line in enumerate(lines, 1):
            if not line.strip():
                continue
            fact = json.loads(line)
            _require(isinstance(fact, dict), f"facts line {line_number} is not an object")
            raw_id = fact.get("id")
            _require(isinstance(raw_id, str) and bool(raw_id), f"facts line {line_number} has no id")
            candidate_ref = pseudonyms.ref("candidate", raw_id)
            _require(candidate_ref not in raw_by_candidate_ref, f"facts line {line_number} duplicates an id")
            raw_by_candidate_ref[candidate_ref] = raw_id
            status = fact.get("status")
            _require(status in {"active", "superseded", "retracted"}, f"facts line {line_number} has invalid status")
            provenance = fact.get("provenance")
            invalid = not isinstance(provenance, list) or len(provenance) == 0
            session_refs: set[str] = set()
            if isinstance(provenance, list):
                for anchor in provenance:
                    if not isinstance(anchor, dict):
                        invalid = True
                        continue
                    raw_session = anchor.get("session_id")
                    if not isinstance(raw_session, str) or not raw_session.strip():
                        invalid = True
                        continue
                    raw_session = raw_session.strip()
                    session_ref = pseudonyms.ref("session", raw_session)
                    session_refs.add(session_ref)
                    if session_ref in sessions:
                        continue
                    if raw_session in excluded_raw:
                        sessions[session_ref] = {"ref": session_ref, "state": "excluded", "created_at": None}
                    elif raw_session not in session_dates:
                        sessions[session_ref] = {"ref": session_ref, "state": "unknown", "created_at": None}
                    else:
                        try:
                            created_at = _public_time(session_dates[raw_session])
                        except PublicEvidenceError:
                            sessions[session_ref] = {"ref": session_ref, "state": "unknown", "created_at": None}
                        else:
                            sessions[session_ref] = {"ref": session_ref, "state": "known", "created_at": created_at}
            candidates.append(
                {
                    "ref": candidate_ref,
                    "status": status,
                    "invalid_provenance": invalid,
                    "session_refs": sorted(session_refs),
                    "eligibility": None,
                    "exclusion_reason": None,
                }
            )
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise PublicEvidenceError(f"facts source is unreadable: {exc}") from exc

    candidates.sort(key=lambda item: item["ref"])
    eligible_refs: list[str] = []
    active_refs: list[str] = []
    eligible_raw: list[str] = []
    active_raw: list[str] = []
    excluded_counts = {
        "empty_provenance": 0,
        "excluded_session": 0,
        "unknown_session": 0,
        "at_or_after_cutoff": 0,
    }
    for candidate in candidates:
        reason = _candidate_reason(candidate, sessions, cutoff)
        if reason is None:
            candidate["eligibility"] = "eligible"
            eligible_refs.append(candidate["ref"])
            eligible_raw.append(raw_by_candidate_ref[candidate["ref"]])
            if candidate["status"] == "active":
                active_refs.append(candidate["ref"])
                active_raw.append(raw_by_candidate_ref[candidate["ref"]])
        else:
            candidate["eligibility"] = "excluded"
            candidate["exclusion_reason"] = reason
            excluded_counts[reason] += 1

    _require(len(candidates) == corpus["prefilter_count"], "projected prefilter count differs from pin")
    _require(len(eligible_refs) == corpus["eligible_count"], "projected eligible count differs from pin")
    _require(
        canonical_sha256(sorted(eligible_raw)) == corpus["eligible_ids_sha256"],
        "projected eligible source IDs differ from pin",
    )
    _require(len(active_refs) == corpus["semantic_candidate_count"], "projected semantic count differs from pin")
    _require(
        canonical_sha256(sorted(active_raw)) == corpus["semantic_candidate_ids_sha256"],
        "projected semantic source IDs differ from pin",
    )
    projection = {
        "schema_version": 4,
        "profile": PROFILE,
        "pseudonym_algorithm": PSEUDONYM_ALGORITHM,
        "cutoff": _public_time(task["eligible_before"]),
        "source_commitments": {
            "facts_sha256": corpus["facts_sha256"],
            "facts_size_bytes": corpus["facts_size_bytes"],
            "session_dates_sha256": corpus["session_dates_sha256"],
            "session_dates_size_bytes": corpus["session_dates_size_bytes"],
        },
        "sessions": sorted(sessions.values(), key=lambda item: item["ref"]),
        "candidates": candidates,
        "summary": {
            "prefilter_count": len(candidates),
            "eligible_count": len(eligible_refs),
            "active_eligible_count": len(active_refs),
            "excluded_by_reason": excluded_counts,
            "eligible_candidate_refs_sha256": canonical_sha256(sorted(eligible_refs)),
            "active_eligible_candidate_refs_sha256": canonical_sha256(sorted(active_refs)),
        },
    }
    raw_to_public = {raw: ref for ref, raw in raw_by_candidate_ref.items()}
    return projection, raw_to_public


def _parse_vector(path: pathlib.Path) -> dict[str, Any]:
    raw = path.read_bytes()
    try:
        _require(raw[:4] == b"EBV1", "vector artifact magic is not EBV1")
        offset = 4

        def take(fmt: str) -> tuple[int, ...]:
            nonlocal offset
            size = struct.calcsize(fmt)
            _require(offset + size <= len(raw), "vector artifact header is truncated")
            values = struct.unpack_from(fmt, raw, offset)
            offset += size
            return values

        (model_length,) = take("<H")
        _require(offset + model_length <= len(raw), "vector model id is truncated")
        model_id = raw[offset : offset + model_length].decode("utf-8")
        offset += model_length
        (dimension,) = take("<I")
        (count,) = take("<I")
        _require(bool(model_id) and dimension > 0, "vector identity is invalid")
        ids: list[str] = []
        seen: set[str] = set()
        for _ in range(count):
            (id_length,) = take("<H")
            _require(offset + id_length <= len(raw), "vector candidate id is truncated")
            candidate_id = raw[offset : offset + id_length].decode("utf-8")
            offset += id_length
            _require(bool(candidate_id) and candidate_id not in seen, "vector candidate id is empty or duplicate")
            seen.add(candidate_id)
            ids.append(candidate_id)
            vector_bytes = dimension * 4
            _require(offset + vector_bytes <= len(raw), "vector payload is truncated")
            for index in range(dimension):
                (value,) = struct.unpack_from("<f", raw, offset + index * 4)
                _require(math.isfinite(value), "vector payload contains a non-finite component")
            offset += vector_bytes
        _require(offset == len(raw), "vector artifact has trailing bytes")
    except (UnicodeDecodeError, struct.error) as exc:
        raise PublicEvidenceError(f"invalid vector artifact: {exc}") from exc
    return {
        "model_id": model_id,
        "dimension": dimension,
        "candidate_ids": ids,
        "raw_stream": {"sha256": sha256_bytes(raw), "size_bytes": len(raw)},
    }


def _vector_index(
    vector_path: pathlib.Path,
    raw_to_public: dict[str, str],
    expected_active_refs: set[str],
) -> dict[str, Any]:
    parsed = _parse_vector(vector_path)
    try:
        ordered_refs = [raw_to_public[raw_id] for raw_id in parsed["candidate_ids"]]
    except KeyError as exc:
        raise PublicEvidenceError("vector artifact contains a candidate outside the projected corpus") from exc
    _require(len(ordered_refs) == len(set(ordered_refs)), "vector projection contains duplicate candidates")
    _require(set(ordered_refs) == expected_active_refs, "vector candidates do not exactly cover active+eligible facts")
    chunks: list[dict[str, Any]] = []
    for index, start in enumerate(range(0, len(ordered_refs), 256)):
        refs = ordered_refs[start : start + 256]
        chunks.append({"index": index, "candidate_refs": refs, "sha256": canonical_sha256(refs)})
    return {
        "algorithm": VECTOR_INDEX_ALGORITHM,
        "model_id": parsed["model_id"],
        "dimension": parsed["dimension"],
        "count": len(ordered_refs),
        "candidate_set_sha256": canonical_sha256(sorted(ordered_refs)),
        "candidate_order_sha256": canonical_sha256(ordered_refs),
        "chunk_size": 256,
        "chunks": chunks,
        "root_sha256": canonical_sha256([chunk["sha256"] for chunk in chunks]),
        "raw_stream": parsed["raw_stream"],
    }


def _stream_commitment(path: pathlib.Path) -> dict[str, Any]:
    raw = path.read_bytes()
    return {"sha256": sha256_bytes(raw), "size_bytes": len(raw)}


def _logical_invocation(record: dict[str, Any], pins: dict[str, Any]) -> dict[str, Any]:
    task = pins["development_task"]
    query_id = record["result"]["query_id"]
    query = next((item for item in task["queries"] if item["query_id"] == query_id), None)
    _require(isinstance(query, dict), "diagnostic result query is not pinned")
    return {
        "executable_component": "entire_brain_binary",
        "operation": "recall",
        "query_ref": query_id,
        "query_sha256": query["query_sha256"],
        "branch_ref": "pinned_development_branch",
        "branch_sha256": sha256_bytes(task["branch"].encode("utf-8")),
        "limit": task["k"],
        "cutoff": _public_time(task["eligible_before"]),
        "exclusion_policy": "pseudonymous_temporal_projection",
        "semantic_mode": record["arm"],
        "output_format": "json",
    }


def _environment_policy(arm: str) -> dict[str, Any]:
    return {
        "profile": "hermetic_deny_parent_v1",
        "inherited_keys": [],
        "ephemeral_bindings": ["cache", "config", "data", "state"],
        "host_context_persisted": False,
        "network_policy": "pinned_loopback_only" if arm == "embeddinggemma_rrf" else "denied",
    }


def _sanitize_lifecycle(path: pathlib.Path, pins: dict[str, Any]) -> dict[str, Any]:
    try:
        raw = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise PublicEvidenceError(f"embedding server attestation is unreadable: {exc}") from exc
    observations = raw.get("observations")
    _require(isinstance(observations, list), "embedding server observations are missing")
    projected: list[dict[str, Any]] = []
    for item in observations:
        _require(isinstance(item, dict), "embedding server observation is not an object")
        projected.append(
            {
                "sequence": item.get("sequence"),
                "phase": item.get("phase"),
                "observed_at": _public_time(item.get("observed_at")),
                "healthy": item.get("healthy"),
                "health_request_count": item.get("health_request_count"),
                "request_nonce_sha256": item.get("request_nonce_sha256"),
            }
        )
    window = raw.get("recall_window", {})
    return {
        "ownership_token_sha256": raw.get("ownership_token_sha256"),
        "model_sha256": raw.get("model_sha256"),
        "embedding_dimension": raw.get("embedding_dimension"),
        "node_version": raw.get("node_version"),
        "recall_started_at": _public_time(window.get("started_at")),
        "recall_finished_at": _public_time(window.get("finished_at")),
        "observations": projected,
        "restricted_attestation_stream": _stream_commitment(path),
    }


def _build_arm_record(
    record: dict[str, Any],
    artifact_root: pathlib.Path,
    pins: dict[str, Any],
    raw_to_public: dict[str, str],
    active_refs: set[str],
) -> dict[str, Any]:
    arm = record.get("arm")
    _require(arm in ARMS, "diagnostic record has an unknown arm")
    artifacts = record.get("artifacts", {})
    stdout_path = _safe_private_file(artifact_root, artifacts.get("stdout_path"), f"{arm} stdout")
    stderr_path = _safe_private_file(artifact_root, artifacts.get("stderr_path"), f"{arm} stderr")
    try:
        stdout = json.loads(stdout_path.read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise PublicEvidenceError(f"{arm} stdout is not one JSON object: {exc}") from exc
    result = record.get("result", {})
    raw_result_ids = result.get("fact_ids_in_order")
    _require(
        isinstance(raw_result_ids, list) and all(isinstance(item, str) for item in raw_result_ids),
        f"{arm} result IDs are invalid",
    )
    stdout_facts = stdout.get("facts")
    stdout_ids = (
        [item.get("id") if isinstance(item, dict) else None for item in stdout_facts]
        if isinstance(stdout_facts, list)
        else None
    )
    _require(stdout_ids == raw_result_ids, f"{arm} sanitized results differ from restricted stdout")
    _require(stdout.get("effective_engine") == arm, f"{arm} restricted stdout engine differs")
    try:
        public_result_ids = [raw_to_public[item] for item in raw_result_ids]
    except KeyError as exc:
        raise PublicEvidenceError(f"{arm} returned an ID outside the temporal projection") from exc
    _require(set(public_result_ids).issubset(active_refs), f"{arm} returned an inactive or ineligible candidate")

    effective_raw = record.get("effective", {})
    effective_fields = (
        "engine",
        "semantic_available",
        "bm25_enabled",
        "fallback_used",
        "embedder_id",
        "embedding_dimension",
        "vector_count",
        "vector_candidate_count",
        "loaded_vector_count",
        "resident_vector_count",
    )
    effective = {field: effective_raw.get(field) for field in effective_fields}
    vector_index = None
    if arm != "lexical_handrolled":
        vector_path = _safe_private_file(artifact_root, artifacts.get("vector_artifact_path"), f"{arm} vector")
        vector_index = _vector_index(vector_path, raw_to_public, active_refs)

    lifecycle = None
    if arm == "embeddinggemma_rrf":
        lifecycle_path = _safe_private_file(
            artifact_root,
            artifacts.get("embedding_server_attestation_path"),
            "embedding server attestation",
        )
        lifecycle = _sanitize_lifecycle(lifecycle_path, pins)
    return {
        "schema_version": 4,
        "profile": PROFILE,
        "arm": arm,
        "invocation": _logical_invocation(record, pins),
        "environment_policy": _environment_policy(arm),
        "effective": effective,
        "streams": {
            "stdout": _stream_commitment(stdout_path),
            "stderr": _stream_commitment(stderr_path),
        },
        "result": {
            "query_ref": result.get("query_id"),
            "candidate_refs_in_order": public_result_ids,
            "candidate_order_sha256": canonical_sha256(public_result_ids),
            "delivered_count": len(public_result_ids),
            "output_valid": result.get("output_valid"),
        },
        "vector_index": vector_index,
        "managed_server_attestation": lifecycle,
    }


def _chain_entry(index: int, event: str, subject_sha256: str, previous: str) -> dict[str, Any]:
    body = {
        "index": index,
        "event": event,
        "subject_sha256": subject_sha256,
        "previous_entry_sha256": previous,
    }
    return {**body, "entry_sha256": canonical_sha256(body)}


def _inventory_rows(bundle: pathlib.Path, roles: dict[str, str]) -> list[dict[str, Any]]:
    rows: list[dict[str, Any]] = []
    for relative, role in sorted(roles.items()):
        path = bundle / relative
        rows.append(
            {
                "role": role,
                "relative_path": relative,
                "sha256": sha256_file(path),
                "size_bytes": path.stat().st_size,
            }
        )
    return rows


def _inventory_digest(rows: list[dict[str, Any]]) -> str:
    aggregate = hashlib.sha256()
    for row in rows:
        aggregate.update(row["relative_path"].encode("utf-8"))
        aggregate.update(b"\0")
        aggregate.update(row["sha256"].encode("ascii"))
        aggregate.update(b"\0")
        aggregate.update(str(row["size_bytes"]).encode("ascii"))
        aggregate.update(b"\n")
    return aggregate.hexdigest()


def _privacy_value_errors(value: Any, label: str = "public evidence") -> list[str]:
    errors: list[str] = []
    if isinstance(value, dict):
        for key, child in value.items():
            lowered = key.lower()
            if lowered in FORBIDDEN_KEYS:
                errors.append(f"{label}: forbidden privacy key {key}")
            if lowered.endswith("_path") and lowered != "relative_path":
                errors.append(f"{label}: persisted path key is forbidden: {key}")
            if "token" in lowered and not lowered.endswith("_sha256"):
                errors.append(f"{label}: plaintext token field is forbidden: {key}")
            if lowered == "environment_policy":
                pass
            elif lowered.startswith("environment"):
                errors.append(f"{label}: persisted environment field is forbidden: {key}")
            errors.extend(_privacy_value_errors(child, f"{label}.{key}"))
    elif isinstance(value, list):
        for index, child in enumerate(value):
            errors.extend(_privacy_value_errors(child, f"{label}[{index}]"))
    elif isinstance(value, str):
        if value.startswith(("/", "file://")) or re.match(r"^[A-Za-z]:[\\/]", value):
            errors.append(f"{label}: absolute host path is forbidden")
        if any(marker in value for marker in ("/Users/", "/home/", "/private/", "/tmp/", "\\Users\\")):
            errors.append(f"{label}: host-specific path fragment is forbidden")
        if EMAIL_RE.search(value):
            errors.append(f"{label}: email address is forbidden")
        if RAW_SESSION_RE.search(value):
            errors.append(f"{label}: raw session identifier is forbidden")
        if value.startswith("fact:") or value.startswith("session-") or "facts.ndjson" in value:
            errors.append(f"{label}: raw corpus/session identifier is forbidden")
        if any(pattern.search(value) for pattern in SECRET_RES):
            errors.append(f"{label}: secret-like value is forbidden")
        if any(marker in value for marker in ("ENTIRE_PLUGIN_", "ENGINE_VERIFICATION_TOKEN")):
            errors.append(f"{label}: host environment name is forbidden")
        if value in {"HOME", "PATH", "TMPDIR", "USER", "USERNAME"}:
            errors.append(f"{label}: host environment value is forbidden")
        if any(character.isspace() for character in value):
            errors.append(f"{label}: free-form text is forbidden")
    return errors


def scan_public_bundle(bundle: pathlib.Path) -> list[str]:
    """Recursively reject unsafe filesystem entries and privacy-bearing JSON."""
    errors: list[str] = []
    if not bundle.is_dir() or bundle.is_symlink():
        return ["public evidence bundle must be a real directory"]
    for current, dirnames, filenames in os.walk(bundle, followlinks=False):
        current_path = pathlib.Path(current)
        for name in [*dirnames, *filenames]:
            path = current_path / name
            mode = path.lstat().st_mode
            relative = path.relative_to(bundle).as_posix()
            if stat.S_ISLNK(mode):
                errors.append(f"public evidence contains a symlink: {relative}")
            elif not stat.S_ISDIR(mode) and not stat.S_ISREG(mode):
                errors.append(f"public evidence contains a special entry: {relative}")
        for name in filenames:
            path = current_path / name
            relative = path.relative_to(bundle).as_posix()
            if path.is_symlink() or not path.is_file():
                continue
            if path.suffix != ".json":
                errors.append(f"public evidence contains a non-JSON payload: {relative}")
                continue
            try:
                value = json.loads(path.read_text(encoding="utf-8"))
            except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
                errors.append(f"public evidence JSON is unreadable at {relative}: {exc}")
                continue
            errors.extend(_privacy_value_errors(value, relative))
    return errors


def build_public_bundle(
    diagnostic_manifest: pathlib.Path,
    artifact_root: pathlib.Path,
    output_dir: pathlib.Path,
    pins: dict[str, Any],
    matrix: dict[str, Any],
    pseudonym_key: bytes,
) -> pathlib.Path:
    """Project an already validated diagnostic manifest into an atomic v4 bundle."""
    _require(not output_dir.exists(), f"refusing to replace public evidence: {output_dir}")
    try:
        diagnostic = json.loads(diagnostic_manifest.read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise PublicEvidenceError(f"diagnostic manifest is unreadable: {exc}") from exc
    _require(
        isinstance(diagnostic, dict) and diagnostic.get("schema_version") == 2,
        "diagnostic manifest must use schema v2",
    )
    records = diagnostic.get("records")
    _require(
        isinstance(records, list)
        and [item.get("arm") for item in records if isinstance(item, dict)] == list(ARMS),
        "diagnostic manifest arms are incomplete or reordered",
    )
    descriptors = [item.get("pin_set") for item in records]
    _require(all(item == descriptors[0] for item in descriptors), "diagnostic pin descriptors differ")
    pin_descriptor = descriptors[0]
    _require(isinstance(pin_descriptor, dict), "diagnostic pin descriptor is missing")

    first_artifacts = records[0].get("artifacts", {})
    facts_path = _safe_private_file(artifact_root, first_artifacts.get("facts_source_path"), "facts source")
    sessions_path = _safe_private_file(
        artifact_root,
        first_artifacts.get("session_dates_source_path"),
        "session dates source",
    )
    pseudonyms = _Pseudonymizer(pseudonym_key)
    temporal, raw_to_public = _build_temporal_projection(facts_path, sessions_path, pins, pseudonyms)
    active_refs = {
        item["ref"]
        for item in temporal["candidates"]
        if item["eligibility"] == "eligible" and item["status"] == "active"
    }
    arm_records = [
        _build_arm_record(record, artifact_root, pins, raw_to_public, active_refs)
        for record in records
    ]
    components = _component_commitments(pins)

    temporary = output_dir.with_name(f".{output_dir.name}.tmp-{secrets.token_hex(8)}")
    _require(not temporary.exists(), "temporary public evidence path already exists")
    try:
        temporary.mkdir(parents=True)
        roles = {"temporal-projection.json": "temporal_projection"}
        _write_json(temporary / "temporal-projection.json", temporal)
        for record in arm_records:
            relative = f"arms/{record['arm']}.json"
            roles[relative] = f"arm_record_{record['arm']}"
            _write_json(temporary / relative, record)

        subjects = [
            ("inputs_authenticated", canonical_sha256({"pin_set": pin_descriptor, "components": components})),
            ("temporal_projection_derived", sha256_file(temporary / "temporal-projection.json")),
        ]
        for record in arm_records:
            path = temporary / f"arms/{record['arm']}.json"
            subjects.append((f"{record['arm']}_result_projected", sha256_file(path)))
        chain: list[dict[str, Any]] = []
        previous = "0" * 64
        for index, (event, subject) in enumerate(subjects):
            entry = _chain_entry(index, event, subject, previous)
            chain.append(entry)
            previous = entry["entry_sha256"]
        attestation = {
            "schema_version": 4,
            "profile": PROFILE,
            "algorithm": CHAIN_ALGORITHM,
            "entries": chain,
            "head_sha256": previous,
        }
        roles["attestation-sequence.json"] = "projection_attestation_sequence"
        _write_json(temporary / "attestation-sequence.json", attestation)

        rows = _inventory_rows(temporary, roles)
        manifest = {
            "schema_version": 4,
            "profile": PROFILE,
            "pin_set": pin_descriptor,
            "privacy_contract": {
                "pseudonym_algorithm": PSEUDONYM_ALGORITHM,
                "raw_payload_policy": "digest_size_only",
                "host_context_policy": "forbidden",
                "scanner": "recursive_fail_closed_v1",
            },
            "component_commitments": components,
            "artifact_inventory": {
                "algorithm": INVENTORY_ALGORITHM,
                "files": rows,
                "file_count": len(rows),
                "logical_bytes": sum(item["size_bytes"] for item in rows),
                "root_sha256": _inventory_digest(rows),
            },
        }
        _write_json(temporary / MANIFEST_NAME, manifest)
        privacy_errors = scan_public_bundle(temporary)
        _require(
            not privacy_errors,
            "generated public evidence failed privacy scan:\n- " + "\n- ".join(privacy_errors),
        )
        validation_errors = validate_public_bundle(
            temporary / MANIFEST_NAME,
            matrix,
            pins,
            pin_descriptor,
        )
        _require(
            not validation_errors,
            "generated public evidence failed the v4 checker:\n- " + "\n- ".join(validation_errors),
        )
        temporary.rename(output_dir)
    except BaseException:
        shutil.rmtree(temporary, ignore_errors=True)
        raise
    return output_dir / MANIFEST_NAME


def _load_json(path: pathlib.Path, errors: list[str], label: str) -> Any | None:
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        errors.append(f"{label} is unreadable JSON: {exc}")
        return None


def _safe_public_file(bundle: pathlib.Path, raw: Any, errors: list[str], label: str) -> pathlib.Path | None:
    if not isinstance(raw, str) or not raw:
        errors.append(f"{label} relative path is missing")
        return None
    relative = pathlib.Path(raw)
    if relative.is_absolute() or ".." in relative.parts:
        errors.append(f"{label} relative path is unsafe")
        return None
    target = (bundle / relative).resolve()
    try:
        target.relative_to(bundle.resolve())
    except ValueError:
        errors.append(f"{label} relative path escapes the bundle")
        return None
    if not target.is_file() or target.is_symlink():
        errors.append(f"{label} is not a real regular file")
        return None
    return target


def _validate_temporal(value: Any, pins: dict[str, Any], errors: list[str]) -> set[str]:
    fields = {
        "schema_version", "profile", "pseudonym_algorithm", "cutoff", "source_commitments",
        "sessions", "candidates", "summary",
    }
    if not _exact_object(value, fields, "temporal projection", errors):
        return set()
    if value["schema_version"] != 4 or value["profile"] != PROFILE:
        errors.append("temporal projection identity is invalid")
    if value["pseudonym_algorithm"] != PSEUDONYM_ALGORITHM:
        errors.append("temporal pseudonym algorithm changed")
    expected_cutoff = _public_time(pins["development_task"]["eligible_before"])
    if value["cutoff"] != expected_cutoff:
        errors.append("temporal cutoff differs from pin")
    corpus = pins["corpus"]
    expected_sources = {
        "facts_sha256": corpus["facts_sha256"],
        "facts_size_bytes": corpus["facts_size_bytes"],
        "session_dates_sha256": corpus["session_dates_sha256"],
        "session_dates_size_bytes": corpus["session_dates_size_bytes"],
    }
    if value["source_commitments"] != expected_sources:
        errors.append("temporal source commitments differ from pins")
    raw_sessions = value["sessions"]
    raw_candidates = value["candidates"]
    if not isinstance(raw_sessions, list) or not isinstance(raw_candidates, list):
        errors.append("temporal sessions and candidates must be arrays")
        return set()
    sessions: dict[str, dict[str, Any]] = {}
    for index, item in enumerate(raw_sessions):
        label = f"temporal sessions[{index}]"
        if not _exact_object(item, {"ref", "state", "created_at"}, label, errors):
            continue
        ref = item["ref"]
        if not isinstance(ref, str) or not re.fullmatch(r"session:[0-9a-f]{32}", ref):
            errors.append(f"{label}: invalid pseudonymous session ref")
            continue
        if ref in sessions:
            errors.append(f"{label}: duplicate session ref")
        sessions[ref] = item
        state = item["state"]
        if state not in {"known", "excluded", "unknown"}:
            errors.append(f"{label}: invalid session state")
        if state == "known":
            try:
                normalized = _public_time(item["created_at"])
            except PublicEvidenceError:
                errors.append(f"{label}: known session timestamp is invalid")
            else:
                if normalized != item["created_at"]:
                    errors.append(f"{label}: known session timestamp is not canonical")
        elif item["created_at"] is not None:
            errors.append(f"{label}: non-known session must not expose a timestamp")
    if [item.get("ref") for item in raw_sessions if isinstance(item, dict)] != sorted(sessions):
        errors.append("temporal sessions are not in canonical ref order")

    cutoff = _parse_time(expected_cutoff)
    eligible: set[str] = set()
    active: set[str] = set()
    excluded_counts = {
        key: 0
        for key in ("empty_provenance", "excluded_session", "unknown_session", "at_or_after_cutoff")
    }
    seen: set[str] = set()
    for index, item in enumerate(raw_candidates):
        label = f"temporal candidates[{index}]"
        if not _exact_object(
            item,
            {"ref", "status", "invalid_provenance", "session_refs", "eligibility", "exclusion_reason"},
            label,
            errors,
        ):
            continue
        ref = item["ref"]
        if not isinstance(ref, str) or not re.fullmatch(r"candidate:[0-9a-f]{32}", ref):
            errors.append(f"{label}: invalid pseudonymous candidate ref")
            continue
        if ref in seen:
            errors.append(f"{label}: duplicate candidate ref")
        seen.add(ref)
        if item["status"] not in {"active", "superseded", "retracted"}:
            errors.append(f"{label}: invalid status")
        if not isinstance(item["invalid_provenance"], bool):
            errors.append(f"{label}: invalid_provenance must be boolean")
        refs = item["session_refs"]
        if not isinstance(refs, list) or not all(isinstance(child, str) for child in refs):
            errors.append(f"{label}: session_refs must be a string array")
            continue
        if refs != sorted(set(refs)):
            errors.append(f"{label}: session_refs must be unique and sorted")
        if not set(refs).issubset(sessions):
            errors.append(f"{label}: session_refs contain an unknown ref")
            continue
        try:
            reason = _candidate_reason(item, sessions, cutoff)
        except (KeyError, PublicEvidenceError):
            errors.append(f"{label}: eligibility cannot be recomputed")
            continue
        expected_eligibility = "eligible" if reason is None else "excluded"
        if item["eligibility"] != expected_eligibility or item["exclusion_reason"] != reason:
            errors.append(f"{label}: recorded eligibility differs from independent recomputation")
        if reason is None:
            eligible.add(ref)
            if item["status"] == "active":
                active.add(ref)
        else:
            excluded_counts[reason] += 1
    if [item.get("ref") for item in raw_candidates if isinstance(item, dict)] != sorted(seen):
        errors.append("temporal candidates are not in canonical ref order")
    expected_summary = {
        "prefilter_count": len(seen),
        "eligible_count": len(eligible),
        "active_eligible_count": len(active),
        "excluded_by_reason": excluded_counts,
        "eligible_candidate_refs_sha256": canonical_sha256(sorted(eligible)),
        "active_eligible_candidate_refs_sha256": canonical_sha256(sorted(active)),
    }
    if value["summary"] != expected_summary:
        errors.append("temporal summary differs from independent recomputation")
    if len(seen) != corpus["prefilter_count"]:
        errors.append("temporal prefilter count differs from pin")
    if len(eligible) != corpus["eligible_count"]:
        errors.append("temporal eligible count differs from pin")
    if len(active) != corpus["semantic_candidate_count"]:
        errors.append("temporal semantic candidate count differs from pin")
    return active


def _validate_stream(value: Any, label: str, errors: list[str]) -> None:
    if not _exact_object(value, {"sha256", "size_bytes"}, label, errors):
        return
    if not isinstance(value["sha256"], str) or not SHA256_RE.fullmatch(value["sha256"]):
        errors.append(f"{label}: SHA-256 is invalid")
    if not isinstance(value["size_bytes"], int) or isinstance(value["size_bytes"], bool) or value["size_bytes"] < 0:
        errors.append(f"{label}: size is invalid")


def _validate_vector(
    value: Any,
    arm: str,
    pins: dict[str, Any],
    active_refs: set[str],
    errors: list[str],
) -> None:
    if arm == "lexical_handrolled":
        if value is not None:
            errors.append("lexical arm must not contain a public vector index")
        return
    fields = {
        "algorithm", "model_id", "dimension", "count", "candidate_set_sha256",
        "candidate_order_sha256", "chunk_size", "chunks", "root_sha256", "raw_stream",
    }
    label = f"{arm} vector index"
    if not _exact_object(value, fields, label, errors):
        return
    engine_pin = pins["engines"][arm]
    if value["algorithm"] != VECTOR_INDEX_ALGORITHM:
        errors.append(f"{label}: algorithm changed")
    if value["model_id"] != engine_pin["embedder_id"] or value["dimension"] != engine_pin["dimension"]:
        errors.append(f"{label}: engine identity differs from pin")
    if value["chunk_size"] != 256:
        errors.append(f"{label}: chunk size changed")
    chunks = value["chunks"]
    if not isinstance(chunks, list):
        errors.append(f"{label}: chunks must be an array")
        return
    ordered: list[str] = []
    chunk_hashes: list[str] = []
    for index, chunk in enumerate(chunks):
        chunk_label = f"{label} chunks[{index}]"
        if not _exact_object(chunk, {"index", "candidate_refs", "sha256"}, chunk_label, errors):
            continue
        refs = chunk["candidate_refs"]
        if chunk["index"] != index:
            errors.append(f"{chunk_label}: sequence index changed")
        if (
            not isinstance(refs, list)
            or not refs
            or len(refs) > 256
            or not all(isinstance(ref, str) and REF_RE.fullmatch(ref) for ref in refs)
        ):
            errors.append(f"{chunk_label}: candidate refs are invalid")
            continue
        expected_hash = canonical_sha256(refs)
        if chunk["sha256"] != expected_hash:
            errors.append(f"{chunk_label}: commitment differs from candidate refs")
        ordered.extend(refs)
        chunk_hashes.append(expected_hash)
    if len(ordered) != len(set(ordered)):
        errors.append(f"{label}: candidate refs are duplicated")
    if set(ordered) != active_refs:
        errors.append(f"{label}: candidates do not exactly cover active+eligible projection")
    expected = {
        "count": len(ordered),
        "candidate_set_sha256": canonical_sha256(sorted(ordered)),
        "candidate_order_sha256": canonical_sha256(ordered),
        "root_sha256": canonical_sha256(chunk_hashes),
    }
    for field, expected_value in expected.items():
        if value[field] != expected_value:
            errors.append(f"{label}: {field} differs from independent recomputation")
    _validate_stream(value["raw_stream"], f"{label} raw stream", errors)


def _validate_lifecycle(value: Any, arm: str, pins: dict[str, Any], errors: list[str]) -> None:
    if arm != "embeddinggemma_rrf":
        if value is not None:
            errors.append(f"{arm}: managed server attestation is prohibited")
        return
    fields = {
        "ownership_token_sha256", "model_sha256", "embedding_dimension", "node_version",
        "recall_started_at", "recall_finished_at", "observations", "restricted_attestation_stream",
    }
    label = "embeddinggemma managed server attestation"
    if not _exact_object(value, fields, label, errors):
        return
    model = pins["embedding_model"]
    runtime = pins["runtime"]
    if value["model_sha256"] != model["sha256"] or value["embedding_dimension"] != model["dimension"]:
        errors.append(f"{label}: model identity differs from pin")
    if value["node_version"] != runtime["node_version"]:
        errors.append(f"{label}: Node version differs from pin")
    if not isinstance(value["ownership_token_sha256"], str) or not SHA256_RE.fullmatch(
        value["ownership_token_sha256"]
    ):
        errors.append(f"{label}: ownership token commitment is invalid")
    _validate_stream(value["restricted_attestation_stream"], f"{label} restricted stream", errors)
    try:
        started = _parse_time(value["recall_started_at"])
        finished = _parse_time(value["recall_finished_at"])
    except PublicEvidenceError:
        errors.append(f"{label}: recall window is invalid")
        return
    observations = value["observations"]
    if not isinstance(observations, list) or len(observations) < 3:
        errors.append(f"{label}: at least pre/during/post observations are required")
        return
    times: list[dt.datetime] = []
    nonces: list[str] = []
    phases: list[Any] = []
    request_counts: list[Any] = []
    for index, item in enumerate(observations):
        item_label = f"{label} observations[{index}]"
        if not _exact_object(
            item,
            {"sequence", "phase", "observed_at", "healthy", "health_request_count", "request_nonce_sha256"},
            item_label,
            errors,
        ):
            continue
        if item["sequence"] != index:
            errors.append(f"{item_label}: sequence changed")
        try:
            times.append(_parse_time(item["observed_at"]))
        except PublicEvidenceError:
            errors.append(f"{item_label}: timestamp is invalid")
        if item["healthy"] is not True:
            errors.append(f"{item_label}: server was not healthy")
        phases.append(item["phase"])
        request_counts.append(item["health_request_count"])
        nonces.append(item["request_nonce_sha256"])
        if not isinstance(item["request_nonce_sha256"], str) or not SHA256_RE.fullmatch(item["request_nonce_sha256"]):
            errors.append(f"{item_label}: nonce commitment is invalid")
    if phases and (phases[0] != "pre_recall" or phases[1] != "during_recall" or phases[-1] != "post_recall"):
        errors.append(f"{label}: pre/during/post phase sequence changed")
    if any(phase != "heartbeat" for phase in phases[2:-1]):
        errors.append(f"{label}: intermediate attestation phase is invalid")
    if len(nonces) != len(set(nonces)):
        errors.append(f"{label}: request nonce commitments are not unique")
    if (
        not all(isinstance(item, int) and not isinstance(item, bool) for item in request_counts)
        or request_counts != sorted(set(request_counts))
    ):
        errors.append(f"{label}: health request counts are not strictly increasing")
    if len(times) == len(observations):
        if times != sorted(times) or len(times) != len(set(times)):
            errors.append(f"{label}: observation times are not strictly increasing")
        elif not (times[0] < started < times[1] < finished < times[-1]):
            errors.append(f"{label}: pre/during/post evidence does not strictly bound recall")


def _validate_arm(
    value: Any,
    expected_arm: str,
    matrix: dict[str, Any],
    pins: dict[str, Any],
    active_refs: set[str],
    errors: list[str],
) -> None:
    fields = {
        "schema_version", "profile", "arm", "invocation", "environment_policy", "effective",
        "streams", "result", "vector_index", "managed_server_attestation",
    }
    label = f"public arm {expected_arm}"
    if not _exact_object(value, fields, label, errors):
        return
    if value["schema_version"] != 4 or value["profile"] != PROFILE or value["arm"] != expected_arm:
        errors.append(f"{label}: identity is invalid")
    task = pins["development_task"]
    invocation = value["invocation"]
    invocation_fields = {
        "executable_component", "operation", "query_ref", "query_sha256", "branch_ref",
        "branch_sha256", "limit", "cutoff", "exclusion_policy", "semantic_mode", "output_format",
    }
    if _exact_object(invocation, invocation_fields, f"{label} invocation", errors):
        query = next((item for item in task["queries"] if item["query_id"] == invocation["query_ref"]), None)
        expected_invocation = {
            "executable_component": "entire_brain_binary",
            "operation": "recall",
            "query_ref": invocation["query_ref"],
            "query_sha256": query.get("query_sha256") if isinstance(query, dict) else None,
            "branch_ref": "pinned_development_branch",
            "branch_sha256": sha256_bytes(task["branch"].encode("utf-8")),
            "limit": task["k"],
            "cutoff": _public_time(task["eligible_before"]),
            "exclusion_policy": "pseudonymous_temporal_projection",
            "semantic_mode": expected_arm,
            "output_format": "json",
        }
        if query is None or invocation != expected_invocation:
            errors.append(f"{label}: typed invocation differs from pins")
    if value["environment_policy"] != _environment_policy(expected_arm):
        errors.append(f"{label}: hermetic environment policy changed")
    engine_pin = pins["engines"][expected_arm]
    effective = value["effective"]
    effective_fields = {
        "engine", "semantic_available", "bm25_enabled", "fallback_used", "embedder_id",
        "embedding_dimension", "vector_count", "vector_candidate_count", "loaded_vector_count",
        "resident_vector_count",
    }
    if _exact_object(effective, effective_fields, f"{label} effective engine", errors):
        if effective["engine"] != expected_arm:
            errors.append(f"{label}: effective engine differs")
        if effective["bm25_enabled"] is not False or effective["fallback_used"] is not False:
            errors.append(f"{label}: BM25/fallback must remain disabled")
        if (
            effective["embedder_id"] != engine_pin["embedder_id"]
            or effective["embedding_dimension"] != engine_pin["dimension"]
        ):
            errors.append(f"{label}: effective embedder identity differs from pin")
        expected_count = 0 if expected_arm == "lexical_handrolled" else len(active_refs)
        for field in ("vector_count", "vector_candidate_count", "resident_vector_count"):
            if effective[field] != expected_count:
                errors.append(f"{label}: {field} differs from active candidate coverage")
        if effective["loaded_vector_count"] != 0:
            errors.append(f"{label}: clean arm inherited vectors")
        expected_semantic = expected_arm != "lexical_handrolled"
        if effective["semantic_available"] is not expected_semantic:
            errors.append(f"{label}: semantic availability differs")
    streams = value["streams"]
    if _exact_object(streams, {"stdout", "stderr"}, f"{label} streams", errors):
        _validate_stream(streams["stdout"], f"{label} stdout stream", errors)
        _validate_stream(streams["stderr"], f"{label} stderr stream", errors)
    result = value["result"]
    if _exact_object(
        result,
        {"query_ref", "candidate_refs_in_order", "candidate_order_sha256", "delivered_count", "output_valid"},
        f"{label} result",
        errors,
    ):
        refs = result["candidate_refs_in_order"]
        if not isinstance(refs, list) or not all(
            isinstance(ref, str) and re.fullmatch(r"candidate:[0-9a-f]{32}", ref)
            for ref in refs
        ):
            errors.append(f"{label}: ranked candidate refs are invalid")
        else:
            if len(refs) != len(set(refs)):
                errors.append(f"{label}: ranked candidate refs are duplicated")
            if not set(refs).issubset(active_refs):
                errors.append(f"{label}: result contains an inactive or ineligible candidate")
            if len(refs) > task["k"]:
                errors.append(f"{label}: result exceeds pinned K")
            if result["delivered_count"] != len(refs):
                errors.append(f"{label}: delivered count does not reconcile")
            if result["candidate_order_sha256"] != canonical_sha256(refs):
                errors.append(f"{label}: ranked candidate order commitment differs")
        if result["query_ref"] != invocation.get("query_ref") or result["output_valid"] is not True:
            errors.append(f"{label}: result/query validity differs")
    _validate_vector(value["vector_index"], expected_arm, pins, active_refs, errors)
    _validate_lifecycle(value["managed_server_attestation"], expected_arm, pins, errors)


def _validate_attestation(
    value: Any,
    pin_set: dict[str, Any],
    components: list[dict[str, Any]],
    temporal_path: pathlib.Path,
    arm_paths: dict[str, pathlib.Path],
    errors: list[str],
) -> None:
    fields = {"schema_version", "profile", "algorithm", "entries", "head_sha256"}
    if not _exact_object(value, fields, "projection attestation", errors):
        return
    if value["schema_version"] != 4 or value["profile"] != PROFILE or value["algorithm"] != CHAIN_ALGORITHM:
        errors.append("projection attestation identity changed")
    expected_subjects = [
        ("inputs_authenticated", canonical_sha256({"pin_set": pin_set, "components": components})),
        ("temporal_projection_derived", sha256_file(temporal_path)),
        *[(f"{arm}_result_projected", sha256_file(arm_paths[arm])) for arm in ARMS],
    ]
    entries = value["entries"]
    if not isinstance(entries, list) or len(entries) != len(expected_subjects):
        errors.append("projection attestation sequence length differs")
        return
    previous = "0" * 64
    for index, (entry, (event, subject)) in enumerate(zip(entries, expected_subjects, strict=True)):
        label = f"projection attestation entries[{index}]"
        fields = {"index", "event", "subject_sha256", "previous_entry_sha256", "entry_sha256"}
        if not _exact_object(entry, fields, label, errors):
            continue
        body = {
            "index": index,
            "event": event,
            "subject_sha256": subject,
            "previous_entry_sha256": previous,
        }
        expected_entry = {**body, "entry_sha256": canonical_sha256(body)}
        if entry != expected_entry:
            errors.append(f"{label}: order, subject, or hash chain differs")
        previous = expected_entry["entry_sha256"]
    if value["head_sha256"] != previous:
        errors.append("projection attestation head differs from chain")


def validate_public_bundle(
    manifest_path: pathlib.Path,
    matrix: dict[str, Any],
    pins: dict[str, Any],
    expected_pin_descriptor: dict[str, Any],
) -> list[str]:
    """Fail-closed public v4 validation used by the protocol gate and tests."""
    errors: list[str] = []
    if matrix.get("verification_schema") != "schemas/engine-verification-public-v4.schema.json":
        errors.append("public engine matrix is not bound to the v4 schema")
    matrix_arms = matrix.get("arms")
    if (
        not isinstance(matrix_arms, list)
        or [item.get("id") for item in matrix_arms if isinstance(item, dict)] != list(ARMS)
    ):
        errors.append("public engine matrix arms are incomplete or reordered")
    else:
        for item in matrix_arms:
            arm = item["id"]
            expected_semantic = arm != "lexical_handrolled"
            if item.get("semantic") is not expected_semantic or item.get("effective_engine_required") != arm:
                errors.append(f"public engine matrix identity changed for {arm}")
            if item.get("environment", {}).get("ENTIRE_BRAIN_FACTS_BM25") != "0":
                errors.append(f"public engine matrix enabled BM25 for {arm}")
    bundle = manifest_path.parent
    manifest = _load_json(manifest_path, errors, "public engine manifest")
    if manifest is None:
        return errors
    manifest_fields = {
        "schema_version", "profile", "pin_set", "privacy_contract", "component_commitments",
        "artifact_inventory",
    }
    if not _exact_object(manifest, manifest_fields, "public engine manifest", errors):
        errors.extend(scan_public_bundle(bundle))
        return errors
    if manifest["schema_version"] != 4 or manifest["profile"] != PROFILE:
        errors.append("public engine manifest identity is invalid")
    if manifest["pin_set"] != expected_pin_descriptor:
        errors.append("public engine manifest pin descriptor is not authoritative")
    expected_privacy = {
        "pseudonym_algorithm": PSEUDONYM_ALGORITHM,
        "raw_payload_policy": "digest_size_only",
        "host_context_policy": "forbidden",
        "scanner": "recursive_fail_closed_v1",
    }
    if manifest["privacy_contract"] != expected_privacy:
        errors.append("public engine privacy contract changed")
    expected_components = _component_commitments(pins)
    if manifest["component_commitments"] != expected_components:
        errors.append("public engine component commitments differ from pins")

    inventory = manifest["artifact_inventory"]
    inventory_fields = {"algorithm", "files", "file_count", "logical_bytes", "root_sha256"}
    role_paths: dict[str, pathlib.Path] = {}
    if _exact_object(inventory, inventory_fields, "public artifact inventory", errors):
        rows = inventory["files"]
        if inventory["algorithm"] != INVENTORY_ALGORITHM:
            errors.append("public artifact inventory algorithm changed")
        if not isinstance(rows, list):
            errors.append("public artifact inventory files must be an array")
            rows = []
        normalized_rows: list[dict[str, Any]] = []
        seen_paths: set[str] = set()
        seen_roles: set[str] = set()
        for index, row in enumerate(rows):
            label = f"public artifact inventory files[{index}]"
            if not _exact_object(row, {"role", "relative_path", "sha256", "size_bytes"}, label, errors):
                continue
            relative = row["relative_path"]
            role = row["role"]
            path = _safe_public_file(bundle, relative, errors, label)
            if relative in seen_paths or role in seen_roles:
                errors.append(f"{label}: path or role is duplicated")
            if isinstance(relative, str):
                seen_paths.add(relative)
            if isinstance(role, str):
                seen_roles.add(role)
            if path is not None:
                if sha256_file(path) != row["sha256"]:
                    errors.append(f"{label}: content hash mismatch")
                if path.stat().st_size != row["size_bytes"]:
                    errors.append(f"{label}: byte size mismatch")
                role_paths[role] = path
            normalized_rows.append(row)
        if rows != sorted(normalized_rows, key=lambda item: item.get("relative_path", "")):
            errors.append("public artifact inventory is not in canonical path order")
        actual_files = {
            path.relative_to(bundle).as_posix()
            for path in bundle.rglob("*")
            if path.is_file() and not path.is_symlink()
        }
        expected_files = seen_paths | {MANIFEST_NAME}
        if actual_files != expected_files:
            errors.append("public artifact inventory does not exactly equal the bundle file set")
        if inventory["file_count"] != len(rows):
            errors.append("public artifact inventory file count differs")
        if inventory["logical_bytes"] != sum(row.get("size_bytes", 0) for row in rows if isinstance(row, dict)):
            errors.append("public artifact inventory logical bytes differ")
        try:
            expected_root = _inventory_digest(rows)
        except (KeyError, AttributeError, TypeError):
            errors.append("public artifact inventory cannot be hashed")
        else:
            if inventory["root_sha256"] != expected_root:
                errors.append("public artifact inventory root differs")

    expected_roles = {
        "temporal_projection",
        "arm_record_lexical_handrolled",
        "arm_record_model2vec_rrf",
        "arm_record_embeddinggemma_rrf",
        "projection_attestation_sequence",
    }
    if set(role_paths) != expected_roles:
        errors.append("public artifact inventory roles are incomplete or unexpected")
    privacy_errors = scan_public_bundle(bundle)
    errors.extend(privacy_errors)
    if set(role_paths) != expected_roles:
        return errors

    temporal_path = role_paths["temporal_projection"]
    temporal = _load_json(temporal_path, errors, "public temporal projection")
    active_refs = _validate_temporal(temporal, pins, errors) if temporal is not None else set()
    arm_paths = {arm: role_paths[f"arm_record_{arm}"] for arm in ARMS}
    query_refs: list[Any] = []
    for arm in ARMS:
        record = _load_json(arm_paths[arm], errors, f"public arm {arm}")
        if record is not None:
            _validate_arm(record, arm, matrix, pins, active_refs, errors)
            if isinstance(record, dict) and isinstance(record.get("result"), dict):
                query_refs.append(record["result"].get("query_ref"))
    if len(query_refs) == len(ARMS) and len(set(query_refs)) != 1:
        errors.append("public engine arms did not use one common query")
    attestation = _load_json(role_paths["projection_attestation_sequence"], errors, "projection attestation")
    if attestation is not None:
        _validate_attestation(
            attestation,
            manifest["pin_set"],
            manifest["component_commitments"],
            temporal_path,
            arm_paths,
            errors,
        )
    return errors


def _validate_private_diagnostic(
    manifest: pathlib.Path,
    artifact_root: pathlib.Path,
    pins: dict[str, Any],
    matrix: dict[str, Any],
    pin_repo: pathlib.Path,
) -> None:
    if str(HERE) not in sys.path:
        sys.path.insert(0, str(HERE))
    import check_protocol  # Imported lazily to keep the public checker acyclic.

    try:
        relative = manifest.resolve().relative_to(artifact_root.resolve()).as_posix()
    except ValueError as exc:
        raise PublicEvidenceError("diagnostic manifest is outside --artifact-root") from exc
    production = pins.get("authority") == "production"
    errors = check_protocol.validate_engine_verification(
        matrix,
        {"status": "pass", "evidence": relative},
        here=artifact_root,
        repo=artifact_root,
        pins=pins,
        require_production=production,
        pin_repo=pin_repo,
        require_storage_contract=False,
    )
    _require(not errors, "private diagnostic evidence failed exact-byte validation:\n- " + "\n- ".join(errors))


def parse_args(argv: Sequence[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--diagnostic-manifest", type=pathlib.Path, required=True)
    parser.add_argument("--artifact-root", type=pathlib.Path, required=True)
    parser.add_argument("--output-dir", type=pathlib.Path, required=True)
    parser.add_argument("--pins", type=pathlib.Path, default=HERE / "engine-verification-pins.json")
    parser.add_argument("--matrix", type=pathlib.Path, default=HERE / "engine-matrix.json")
    parser.add_argument("--pin-repo", type=pathlib.Path, default=REPO)
    parser.add_argument(
        "--pseudonym-key-file",
        type=pathlib.Path,
        help="optional restricted binary key; otherwise an ephemeral 256-bit key is generated and never persisted",
    )
    return parser.parse_args(argv)


def main(argv: Sequence[str] | None = None) -> int:
    args = parse_args(argv)
    try:
        pins = json.loads(args.pins.read_text(encoding="utf-8"))
        matrix = json.loads(args.matrix.read_text(encoding="utf-8"))
        _validate_private_diagnostic(
            args.diagnostic_manifest,
            args.artifact_root,
            pins,
            matrix,
            args.pin_repo,
        )
        key = args.pseudonym_key_file.read_bytes() if args.pseudonym_key_file else secrets.token_bytes(32)
        path = build_public_bundle(
            args.diagnostic_manifest,
            args.artifact_root,
            args.output_dir,
            pins,
            matrix,
            key,
        )
        print(path)
        return 0
    except (OSError, UnicodeDecodeError, json.JSONDecodeError, PublicEvidenceError) as exc:
        print(f"engine public evidence error: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
