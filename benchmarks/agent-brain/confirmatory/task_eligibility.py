#!/usr/bin/env python3
"""Build a deterministic, development-only static task-eligibility scan.

The scan walks first-parent integration units and retains only content
commitments and structural counts.  It deliberately does not create prompts,
inspect retrieval, run negative controls, assign splits, or authorize model
execution.  Any scan whose identities were exposed to the optimizer is
permanently development-only.
"""

from __future__ import annotations

import argparse
import copy
import datetime as dt
import hashlib
import json
import os
import pathlib
import re
import subprocess
import sys
import tempfile
from collections import Counter
from typing import Any, Sequence

import task_population


PROFILE = "agent_brain_development_task_eligibility_scan_v1"
SELECTION_RULE = "first_parent_source_plus_test_static_screen_v1"
EXPOSURE = "development_only_identity_inspected"
OID_RE = re.compile(r"^[0-9a-f]{40}$")
SHA256_RE = re.compile(r"^[0-9a-f]{64}$")


class EligibilityError(ValueError):
    """Raised when a development eligibility scan is not reproducible."""


def _require(condition: bool, message: str) -> None:
    if not condition:
        raise EligibilityError(message)


def _run_git(repo: pathlib.Path, args: Sequence[str], *, binary: bool = False) -> str | bytes:
    try:
        completed = subprocess.run(
            ["git", *args],
            cwd=repo,
            check=False,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )
    except OSError as exc:
        raise EligibilityError(f"cannot execute git: {exc}") from exc
    if completed.returncode != 0:
        message = completed.stderr.decode("utf-8", errors="replace").strip()
        raise EligibilityError(f"git command failed: {message or 'unknown error'}")
    if binary:
        return completed.stdout
    try:
        return completed.stdout.decode("utf-8")
    except UnicodeDecodeError as exc:
        raise EligibilityError("git emitted a non-UTF-8 metadata path") from exc


def _oid(repo: pathlib.Path, revision: str) -> str:
    value = str(_run_git(repo, ["rev-parse", "--verify", f"{revision}^{{commit}}"])).strip()
    _require(OID_RE.fullmatch(value) is not None and value != "0" * 40, "invalid Git object ID")
    return value


def _sha256(value: bytes) -> str:
    return hashlib.sha256(value).hexdigest()


def _canonical_hash(value: Any) -> str:
    return _sha256(task_population.canonical_json_bytes(value))


def _self_hash(value: dict[str, Any]) -> str:
    projected = copy.deepcopy(value)
    _require("ledger_sha256" in projected, "eligibility scan is missing ledger_sha256")
    projected["ledger_sha256"] = None
    return _canonical_hash(projected)


def _validate_timestamp(value: Any, field: str) -> None:
    _require(isinstance(value, str) and bool(value), f"{field} is invalid")
    try:
        parsed = dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError as exc:
        raise EligibilityError(f"{field} is invalid") from exc
    _require(parsed.tzinfo is not None and parsed.utcoffset() is not None, f"{field} must include a UTC offset")


def _candidate_ref(commit_oid: str) -> str:
    return _sha256(b"entire-brain/task-eligibility-v1/candidate\0" + commit_oid.encode("ascii"))


def _is_test_path(path: str) -> bool:
    return (
        path.endswith("_test.go")
        or "/testdata/" in path
        or path.startswith("test/")
        or path.startswith("tests/")
    )


def _is_source_path(path: str) -> bool:
    return path.endswith(".go") and not path.endswith("_test.go")


def static_scope_score(source_files: int, test_files: int, source_packages: int) -> int:
    _require(source_files > 0 and test_files > 0 and source_packages > 0, "static scope counts must be positive")
    return (
        min(source_files, 4)
        + min(test_files, 3)
        + min(source_packages, 3)
        + (2 if source_packages > 1 else 0)
    )


def static_scope_band(score: int) -> str:
    if score <= 5:
        return "low"
    if score <= 8:
        return "medium"
    return "high"


def _paths(repo: pathlib.Path, parent: str, commit: str) -> list[str]:
    raw = _run_git(
        repo,
        ["diff", "--name-only", "-z", "--no-renames", parent, commit, "--"],
        binary=True,
    )
    assert isinstance(raw, bytes)
    try:
        values = [part.decode("utf-8") for part in raw.split(b"\0") if part]
    except UnicodeDecodeError as exc:
        raise EligibilityError("changed path is not valid UTF-8") from exc
    _require(values == sorted(set(values)), "Git changed-path output is not canonical")
    return values


def _diff_hash(repo: pathlib.Path, parent: str, commit: str, paths: list[str]) -> str:
    raw = _run_git(
        repo,
        ["diff", "--binary", "--no-ext-diff", "--no-renames", parent, commit, "--", *paths],
        binary=True,
    )
    assert isinstance(raw, bytes)
    return _sha256(raw)


def scan_repository(
    repo: pathlib.Path,
    *,
    base: str,
    head: str,
    repository_id: str,
) -> dict[str, Any]:
    repo = repo.resolve()
    _require(repo.is_dir(), "repository path is not a directory")
    _require(
        bool(repository_id)
        and len(repository_id) <= 256
        and "\n" not in repository_id
        and "\r" not in repository_id,
        "repository_id must be one line of at most 256 characters",
    )
    base_oid = _oid(repo, base)
    head_oid = _oid(repo, head)
    ancestor = subprocess.run(
        ["git", "merge-base", "--is-ancestor", base_oid, head_oid],
        cwd=repo,
        check=False,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    _require(ancestor.returncode == 0, "base revision is not an ancestor of head")
    units_text = str(
        _run_git(repo, ["rev-list", "--first-parent", "--reverse", f"{base_oid}..{head_oid}"])
    )
    units = [line for line in units_text.splitlines() if line]
    candidates: list[dict[str, Any]] = []
    band_counts: Counter[str] = Counter()
    for position, commit in enumerate(units, 1):
        parents = str(_run_git(repo, ["show", "-s", "--format=%P", commit])).split()
        _require(bool(parents), "first-parent unit has no parent")
        parent = parents[0]
        paths = _paths(repo, parent, commit)
        source_paths = sorted(path for path in paths if _is_source_path(path))
        test_paths = sorted(path for path in paths if _is_test_path(path))
        if not source_paths or not test_paths:
            continue
        packages = sorted({str(pathlib.PurePosixPath(path).parent) for path in source_paths})
        score = static_scope_score(len(source_paths), len(test_paths), len(packages))
        band = static_scope_band(score)
        band_counts[band] += 1
        committed_at = str(_run_git(repo, ["show", "-s", "--format=%cI", commit])).strip()
        tree_oid = str(_run_git(repo, ["rev-parse", f"{commit}^{{tree}}"])).strip()
        candidates.append(
            {
                "first_parent_position": position,
                "candidate_ref": _candidate_ref(commit),
                "commit_oid": commit,
                "parent_oid": parent,
                "tree_oid": tree_oid,
                "committed_at": committed_at,
                "merge_parent_count": len(parents),
                "source_file_count": len(source_paths),
                "test_file_count": len(test_paths),
                "source_package_count": len(packages),
                "cross_package": len(packages) > 1,
                "changed_paths_sha256": _canonical_hash(paths),
                "source_paths_sha256": _canonical_hash(source_paths),
                "test_paths_sha256": _canonical_hash(test_paths),
                "source_diff_sha256": _diff_hash(repo, parent, commit, source_paths),
                "test_diff_sha256": _diff_hash(repo, parent, commit, test_paths),
                "static_scope_score": score,
                "static_scope_band": band,
                "negative_control_status": "pending",
            }
        )
    generated_at = str(_run_git(repo, ["show", "-s", "--format=%cI", head_oid])).strip()
    result = {
        "schema_version": 1,
        "profile": PROFILE,
        "status": "static_screen_only",
        "exposure": EXPOSURE,
        "repository_id": repository_id,
        "selection_rule": SELECTION_RULE,
        "base_oid": base_oid,
        "head_oid": head_oid,
        "generated_at": generated_at,
        "ledger_sha256": None,
        "summary": {
            "first_parent_unit_count": len(units),
            "source_plus_test_candidate_count": len(candidates),
            "negative_control_pending_count": len(candidates),
            "static_scope_band_counts": {
                "low": band_counts["low"],
                "medium": band_counts["medium"],
                "high": band_counts["high"],
            },
        },
        "candidates": candidates,
    }
    result["ledger_sha256"] = _self_hash(result)
    validate_scan(result)
    return result


def validate_scan(value: dict[str, Any]) -> None:
    expected_root = {
        "schema_version",
        "profile",
        "status",
        "exposure",
        "repository_id",
        "selection_rule",
        "base_oid",
        "head_oid",
        "generated_at",
        "ledger_sha256",
        "summary",
        "candidates",
    }
    _require(set(value) == expected_root, "eligibility scan root fields differ")
    _require(value["schema_version"] == 1 and value["profile"] == PROFILE, "eligibility profile changed")
    _require(value["status"] == "static_screen_only" and value["exposure"] == EXPOSURE, "eligibility scope changed")
    _require(value["selection_rule"] == SELECTION_RULE, "selection rule changed")
    repository_id = value["repository_id"]
    _require(
        isinstance(repository_id, str)
        and bool(repository_id)
        and len(repository_id) <= 256
        and "\n" not in repository_id
        and "\r" not in repository_id,
        "repository_id is invalid",
    )
    _validate_timestamp(value["generated_at"], "generated_at")
    for field in ("base_oid", "head_oid"):
        item = value[field]
        _require(isinstance(item, str) and OID_RE.fullmatch(item) is not None and item != "0" * 40, f"{field} is invalid")
    recorded = value["ledger_sha256"]
    _require(isinstance(recorded, str) and SHA256_RE.fullmatch(recorded) is not None and recorded != "0" * 64, "ledger SHA-256 is invalid")
    _require(recorded == _self_hash(value), "eligibility ledger self hash mismatch")
    candidates = value["candidates"]
    _require(isinstance(candidates, list), "candidates must be a list")
    expected_fields = {
        "first_parent_position",
        "candidate_ref",
        "commit_oid",
        "parent_oid",
        "tree_oid",
        "committed_at",
        "merge_parent_count",
        "source_file_count",
        "test_file_count",
        "source_package_count",
        "cross_package",
        "changed_paths_sha256",
        "source_paths_sha256",
        "test_paths_sha256",
        "source_diff_sha256",
        "test_diff_sha256",
        "static_scope_score",
        "static_scope_band",
        "negative_control_status",
    }
    positions: list[int] = []
    refs: set[str] = set()
    commits: set[str] = set()
    bands: Counter[str] = Counter()
    for index, candidate in enumerate(candidates):
        _require(isinstance(candidate, dict) and set(candidate) == expected_fields, f"candidate[{index}] fields differ")
        position = candidate["first_parent_position"]
        _require(type(position) is int and position > 0, f"candidate[{index}] position is invalid")
        positions.append(position)
        for field in ("commit_oid", "parent_oid", "tree_oid"):
            item = candidate[field]
            _require(isinstance(item, str) and OID_RE.fullmatch(item) is not None and item != "0" * 40, f"candidate[{index}].{field} is invalid")
        _require(candidate["commit_oid"] not in commits, "duplicate candidate commit")
        commits.add(candidate["commit_oid"])
        _validate_timestamp(candidate["committed_at"], f"candidate[{index}].committed_at")
        _require(
            type(candidate["merge_parent_count"]) is int and candidate["merge_parent_count"] >= 1,
            f"candidate[{index}] merge-parent count is invalid",
        )
        for field in (
            "candidate_ref",
            "changed_paths_sha256",
            "source_paths_sha256",
            "test_paths_sha256",
            "source_diff_sha256",
            "test_diff_sha256",
        ):
            item = candidate[field]
            _require(isinstance(item, str) and SHA256_RE.fullmatch(item) is not None and item != "0" * 64, f"candidate[{index}].{field} is invalid")
        _require(candidate["candidate_ref"] == _candidate_ref(candidate["commit_oid"]), f"candidate[{index}] reference mismatch")
        _require(candidate["candidate_ref"] not in refs, "duplicate candidate reference")
        refs.add(candidate["candidate_ref"])
        counts = [candidate[field] for field in ("source_file_count", "test_file_count", "source_package_count")]
        _require(all(type(item) is int and item > 0 for item in counts), f"candidate[{index}] counts are invalid")
        _require(candidate["cross_package"] is (candidate["source_package_count"] > 1), f"candidate[{index}] cross-package flag drift")
        score = static_scope_score(*counts)
        _require(candidate["static_scope_score"] == score, f"candidate[{index}] static score drift")
        band = static_scope_band(score)
        _require(candidate["static_scope_band"] == band, f"candidate[{index}] static band drift")
        _require(candidate["negative_control_status"] == "pending", f"candidate[{index}] negative control must remain pending")
        bands[band] += 1
    _require(positions == sorted(set(positions)), "candidate positions are not unique first-parent order")
    summary = value["summary"]
    expected_summary_fields = {
        "first_parent_unit_count",
        "source_plus_test_candidate_count",
        "negative_control_pending_count",
        "static_scope_band_counts",
    }
    _require(isinstance(summary, dict) and set(summary) == expected_summary_fields, "summary fields differ")
    for field in (
        "first_parent_unit_count",
        "source_plus_test_candidate_count",
        "negative_control_pending_count",
    ):
        _require(type(summary[field]) is int and summary[field] >= 0, f"summary.{field} is invalid")
    _require(summary["source_plus_test_candidate_count"] == len(candidates), "candidate summary count drift")
    _require(summary["negative_control_pending_count"] == len(candidates), "negative-control summary drift")
    _require(summary["first_parent_unit_count"] >= len(candidates), "first-parent summary count drift")
    _require(all(position <= summary["first_parent_unit_count"] for position in positions), "candidate position exceeds first-parent count")
    _require(summary["static_scope_band_counts"] == {"low": bands["low"], "medium": bands["medium"], "high": bands["high"]}, "static-band summary drift")


def _write_atomic(path: pathlib.Path, value: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    rendered = json.dumps(value, ensure_ascii=False, allow_nan=False, indent=2, sort_keys=True) + "\n"
    descriptor, temporary_name = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent)
    temporary = pathlib.Path(temporary_name)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
            handle.write(rendered)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
    finally:
        temporary.unlink(missing_ok=True)


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo", required=True, type=pathlib.Path)
    parser.add_argument("--base", required=True)
    parser.add_argument("--head", required=True)
    parser.add_argument("--repository-id", required=True)
    parser.add_argument("--output", type=pathlib.Path)
    args = parser.parse_args(argv)
    try:
        scan = scan_repository(
            args.repo,
            base=args.base,
            head=args.head,
            repository_id=args.repository_id,
        )
    except EligibilityError as exc:
        parser.error(str(exc))
    if args.output is None:
        sys.stdout.write(json.dumps(scan, ensure_ascii=False, indent=2, sort_keys=True) + "\n")
    else:
        _write_atomic(args.output, scan)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
