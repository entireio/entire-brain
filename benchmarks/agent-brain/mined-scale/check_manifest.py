#!/usr/bin/env python3
"""Validate the checked-in mined-scale task/patch corpus as one frozen unit."""

from __future__ import annotations

import argparse
import collections
import copy
import hashlib
import json
import pathlib
import re
from typing import Any


ROOT = pathlib.Path(__file__).resolve().parent
DEFAULT_MANIFEST = ROOT / "manifest.json"
SCHEMA = "agent-brain-mined-scale-corpus/v1"
EXPECTED_BY_REPO = {"entire-cli": 52, "entire-db": 21}
EXPECTED_TASK_COUNT = sum(EXPECTED_BY_REPO.values())
SHA40_RE = re.compile(r"^[0-9a-f]{40}$")
SHA256_RE = re.compile(r"^[0-9a-f]{64}$")


class ManifestError(RuntimeError):
    pass


def canonical_json_bytes(value: Any) -> bytes:
    return json.dumps(
        value,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
        allow_nan=False,
    ).encode("utf-8")


def sha256_bytes(value: bytes) -> str:
    return hashlib.sha256(value).hexdigest()


def attach_self_hash(value: dict[str, Any]) -> dict[str, Any]:
    result = copy.deepcopy(value)
    result.pop("manifest_sha256", None)
    result["manifest_sha256"] = sha256_bytes(canonical_json_bytes(result))
    return result


def _reject_duplicate_keys(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise ManifestError(f"duplicate JSON key: {key}")
        result[key] = value
    return result


def load_json_object(path: pathlib.Path, role: str) -> tuple[bytes, dict[str, Any]]:
    try:
        raw = path.read_bytes()
        value = json.loads(raw, object_pairs_hook=_reject_duplicate_keys)
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        raise ManifestError(f"cannot read valid {role}: {path}") from exc
    if not isinstance(value, dict):
        raise ManifestError(f"{role} must be a JSON object: {path}")
    return raw, value


def _safe_file(root: pathlib.Path, relative: str, role: str) -> pathlib.Path:
    pure = pathlib.PurePosixPath(relative)
    if not relative or pure.is_absolute() or ".." in pure.parts:
        raise ManifestError(f"unsafe {role} path: {relative!r}")
    root_resolved = root.resolve(strict=True)
    path = root.joinpath(*pure.parts)
    try:
        path.resolve(strict=True).relative_to(root_resolved)
    except (OSError, ValueError) as exc:
        raise ManifestError(f"{role} path escapes the corpus: {relative!r}") from exc
    if not path.is_file():
        raise ManifestError(f"{role} is not a regular file: {relative!r}")
    return path


def build_manifest_payload(root: pathlib.Path = ROOT) -> dict[str, Any]:
    config_paths = sorted(
        list(root.glob("entire-cli-scale-*.json"))
        + list(root.glob("entire-db-scale-*.json"))
    )
    patch_paths = sorted((root / "patches").glob("*.patch"))
    if len(config_paths) != EXPECTED_TASK_COUNT:
        raise ManifestError(
            f"mined-scale config count is {len(config_paths)}, expected {EXPECTED_TASK_COUNT}"
        )
    if len(patch_paths) != EXPECTED_TASK_COUNT:
        raise ManifestError(
            f"mined-scale patch count is {len(patch_paths)}, expected {EXPECTED_TASK_COUNT}"
        )

    patch_relatives = {path.relative_to(root).as_posix() for path in patch_paths}
    referenced_patches: set[str] = set()
    ids: set[str] = set()
    counts: collections.Counter[str] = collections.Counter()
    entries: list[dict[str, Any]] = []

    for config_path in config_paths:
        config_raw, task = load_json_object(config_path, "task config")
        task_id = task.get("id")
        repo = task.get("repo")
        if not isinstance(task_id, str) or task_id != config_path.stem:
            raise ManifestError(f"task id does not match config filename: {config_path.name}")
        if task_id in ids:
            raise ManifestError(f"duplicate task id: {task_id}")
        ids.add(task_id)
        expected_repo = "entire-cli" if task_id.startswith("entire-cli-scale-") else "entire-db"
        if repo != expected_repo:
            raise ManifestError(f"task repo disagrees with filename: {config_path.name}")
        if task.get("repo_path") != repo:
            raise ManifestError(f"task repo_path is not the portable logical repo name: {task_id}")
        if "post_brain_commands" in task:
            raise ManifestError(f"task retains shell-based post_brain_commands: {task_id}")
        patch_relative = task.get("post_brain_patch")
        expected_patch = f"patches/{task_id}.patch"
        if patch_relative != expected_patch:
            raise ManifestError(f"task patch path is not canonical: {task_id}")
        patch_path = _safe_file(root, expected_patch, "task patch")
        referenced_patches.add(expected_patch)
        source_commit = task.get("_mined_from_commit")
        base_commit = task.get("base_commit")
        if not isinstance(source_commit, str) or not SHA40_RE.fullmatch(source_commit):
            raise ManifestError(f"task source commit is not a full SHA-1: {task_id}")
        if not isinstance(base_commit, str) or not SHA40_RE.fullmatch(base_commit):
            raise ManifestError(f"task base commit is not a full SHA-1: {task_id}")
        counts[repo] += 1
        entries.append(
            {
                "id": task_id,
                "repo": repo,
                "source_commit": source_commit,
                "base_commit": base_commit,
                "config": {
                    "path": config_path.relative_to(root).as_posix(),
                    "sha256": sha256_bytes(config_raw),
                },
                "patch": {
                    "path": expected_patch,
                    "sha256": sha256_bytes(patch_path.read_bytes()),
                },
            }
        )

    if dict(sorted(counts.items())) != EXPECTED_BY_REPO:
        raise ManifestError(f"task repository counts are {dict(counts)}, expected {EXPECTED_BY_REPO}")
    if referenced_patches != patch_relatives:
        missing = sorted(patch_relatives - referenced_patches)
        extra = sorted(referenced_patches - patch_relatives)
        raise ManifestError(f"config/patch inventory mismatch: unreferenced={missing}, missing={extra}")

    body = {
        "schema": SCHEMA,
        "population": {
            "negative_control_valid_candidates": 75,
            "pre_treatment_materialization_attrition": 2,
            "runnable_task_count": EXPECTED_TASK_COUNT,
            "task_count_by_repo": EXPECTED_BY_REPO,
        },
        "inventory": {
            "config_globs": ["entire-cli-scale-*.json", "entire-db-scale-*.json"],
            "patch_glob": "patches/*.patch",
            "task_count": len(entries),
            "patch_count": len(patch_paths),
        },
        "tasks": entries,
    }
    return attach_self_hash(body)


def validate_manifest(
    root: pathlib.Path = ROOT,
    manifest_path: pathlib.Path = DEFAULT_MANIFEST,
) -> dict[str, Any]:
    _, actual = load_json_object(manifest_path, "mined-scale manifest")
    manifest_hash = actual.get("manifest_sha256")
    if not isinstance(manifest_hash, str) or not SHA256_RE.fullmatch(manifest_hash):
        raise ManifestError("manifest_sha256 is missing or malformed")
    unhashed = copy.deepcopy(actual)
    unhashed.pop("manifest_sha256", None)
    if manifest_hash != sha256_bytes(canonical_json_bytes(unhashed)):
        raise ManifestError("mined-scale manifest self-hash mismatch")
    expected = build_manifest_payload(root)
    if actual != expected:
        raise ManifestError("mined-scale manifest does not match the checked-in config/patch corpus")
    return actual


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--print-manifest",
        action="store_true",
        help="print the canonical manifest derived from the current corpus",
    )
    args = parser.parse_args()
    if args.print_manifest:
        print(json.dumps(build_manifest_payload(), indent=2, sort_keys=True))
        return 0
    manifest = validate_manifest()
    print(
        json.dumps(
            {
                "manifest_sha256": manifest["manifest_sha256"],
                "ok": True,
                "patches": manifest["inventory"]["patch_count"],
                "tasks": manifest["inventory"]["task_count"],
            },
            sort_keys=True,
        )
    )
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except ManifestError as exc:
        raise SystemExit(str(exc)) from exc
