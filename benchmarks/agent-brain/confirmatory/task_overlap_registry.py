#!/usr/bin/env python3
"""Build the global, development-only exact-overlap registry.

The registry combines the existing 23-entry CLI v1 inventory with v2 ledgers
for the three pinned repositories.  Exact Git/diff/patch identities are
compared globally.  Semantic, family, and source-session overlap deliberately
remain unresolved without owner-held authority.
"""

from __future__ import annotations

import argparse
import copy
import datetime as dt
import json
import os
import pathlib
import tempfile
from collections import defaultdict
from typing import Any, Sequence

import task_eligibility
import task_eligibility_v2 as v2
import task_population as cli_task_population


PROFILE = "agent_brain_global_development_task_overlap_registry_v1"
EXPOSURE = v2.EXPOSURE
STATUS = v2.AUTHORITY_STATUS
EXACT_IDENTITY_PROFILE = "canonical_git_object_diff_full_index_v1"
CLI_LEDGER_DIFF_PROFILE = "legacy_cli_v1_git_diff_bytes_abbrev_9"
DIMENSIONS = (
    "commit_oid",
    "tree_oid",
    "source_diff_sha256",
    "test_diff_sha256",
    "full_diff_sha256",
    "source_stable_patch_id",
    "test_stable_patch_id",
    "full_stable_patch_id",
)


class OverlapRegistryError(ValueError):
    """Raised when the global registry cannot be reproduced exactly."""


def _require(condition: bool, message: str) -> None:
    if not condition:
        raise OverlapRegistryError(message)


def _load(path: pathlib.Path) -> tuple[dict[str, Any], bytes]:
    try:
        raw = path.read_bytes()
        value = json.loads(raw, object_pairs_hook=v2._reject_duplicate_pairs)
    except (OSError, UnicodeError, json.JSONDecodeError, v2.EligibilityV2Error) as exc:
        raise OverlapRegistryError(f"cannot load JSON: {exc}") from exc
    _require(isinstance(value, dict), "JSON root must be an object")
    return value, raw


def _self_hash(value: dict[str, Any]) -> str:
    projected = copy.deepcopy(value)
    _require("registry_sha256" in projected, "registry_sha256 is missing")
    projected["registry_sha256"] = None
    return v2._canonical_hash(projected)


def _legacy_cli_diff(
    git_binary: pathlib.Path,
    repo: pathlib.Path,
    parent: str,
    commit: str,
    paths: Sequence[str],
) -> bytes:
    _require(bool(paths), "cannot create an empty legacy CLI diff")
    v2._require_unspecified_diff_attributes(git_binary, repo, commit, paths)
    return v2._git(
        git_binary,
        repo,
        [
            f"--attr-source={commit}", "diff", "--binary", "--no-full-index", "--abbrev=9",
            "--unified=3", "--inter-hunk-context=0", "--src-prefix=a/", "--dst-prefix=b/",
            "--line-prefix=", "--output-indicator-new=+", "--output-indicator-old=-",
            "--output-indicator-context= ", "--no-relative", "--no-color", "--no-ext-diff",
            "--no-textconv", "--no-renames", "--indent-heuristic", "--diff-algorithm=myers",
            "--submodule=short", "--ignore-submodules=none", "--ws-error-highlight=none",
            f"-O{os.devnull}", parent, commit, "--", *paths,
        ],
    )


def _cli_records(git_binary: pathlib.Path, repo: pathlib.Path, ledger: dict[str, Any]) -> list[dict[str, Any]]:
    try:
        task_eligibility.validate_scan(ledger)
    except task_eligibility.EligibilityError as exc:
        raise OverlapRegistryError(f"CLI v1 ledger is invalid: {exc}") from exc
    replace_refs = [line for line in v2._git_text(git_binary, repo, ["for-each-ref", "--format=%(refname)", "refs/replace"]).splitlines() if line]
    _require(not replace_refs, "CLI repository contains forbidden replace refs")
    v2._require_empty_git_metadata_file(git_binary, repo, "info/attributes")
    v2._require_empty_git_metadata_file(git_binary, repo, "info/grafts")
    _require(v2._git_text(git_binary, repo, ["rev-parse", "--is-shallow-repository"]).strip() == "false", "CLI repository must not be shallow")
    base_oid = v2._oid(git_binary, repo, ledger["base_oid"])
    head_oid = v2._oid(git_binary, repo, ledger["head_oid"])
    exact_base_oid = v2._oid(git_binary, repo, f"{head_oid}~{ledger['summary']['first_parent_unit_count']}")
    _require(exact_base_oid == base_oid, "CLI base is not exact pinned-head~window")
    units = [line for line in v2._git_text(git_binary, repo, ["rev-list", "--first-parent", "--reverse", f"{base_oid}..{head_oid}"]).splitlines() if line]
    _require(len(units) == ledger["summary"]["first_parent_unit_count"], "CLI first-parent window differs")
    records: list[dict[str, Any]] = []
    for index, candidate in enumerate(ledger["candidates"]):
        position = candidate["first_parent_position"]
        _require(units[position - 1] == candidate["commit_oid"], f"CLI candidate[{index}] first-parent position differs")
        parent = candidate["parent_oid"]
        commit = candidate["commit_oid"]
        parents = v2._git_text(git_binary, repo, ["show", "-s", "--format=%P", commit]).split()
        _require(bool(parents) and parents[0] == parent, f"CLI candidate[{index}] actual first parent differs")
        _require(len(parents) == candidate["merge_parent_count"], f"CLI candidate[{index}] merge parent count differs")
        expected_parent = units[position - 2] if position > 1 else base_oid
        _require(parent == expected_parent, f"CLI candidate[{index}] first-parent chain differs")
        paths = v2._changed_paths(git_binary, repo, parent, commit)
        source = sorted(path for path in paths if task_eligibility._is_source_path(path))
        tests = sorted(path for path in paths if task_eligibility._is_test_path(path))
        _require(not (set(source) & set(tests)), f"CLI candidate[{index}] source/test paths overlap")
        _require(task_eligibility._canonical_hash(paths) == candidate["changed_paths_sha256"], f"CLI candidate[{index}] changed paths differ")
        _require(task_eligibility._canonical_hash(source) == candidate["source_paths_sha256"], f"CLI candidate[{index}] source paths differ")
        _require(task_eligibility._canonical_hash(tests) == candidate["test_paths_sha256"], f"CLI candidate[{index}] test paths differ")
        legacy_source_patch = _legacy_cli_diff(git_binary, repo, parent, commit, source)
        legacy_test_patch = _legacy_cli_diff(git_binary, repo, parent, commit, tests)
        _require(v2._sha256(legacy_source_patch) == candidate["source_diff_sha256"], f"CLI candidate[{index}] legacy source diff differs")
        _require(v2._sha256(legacy_test_patch) == candidate["test_diff_sha256"], f"CLI candidate[{index}] legacy test diff differs")
        source_patch = v2._diff(git_binary, repo, parent, commit, source)
        test_patch = v2._diff(git_binary, repo, parent, commit, tests)
        full_patch = v2._diff(git_binary, repo, parent, commit, paths)
        tree_oid = v2._git_text(git_binary, repo, ["rev-parse", f"{commit}^{{tree}}" ]).strip()
        _require(tree_oid == candidate["tree_oid"], f"CLI candidate[{index}] tree differs")
        records.append({
            "candidate_ref": candidate["candidate_ref"],
            "changed_paths_sha256": candidate["changed_paths_sha256"],
            "commit_oid": commit,
            "exact_identity_profile": EXACT_IDENTITY_PROFILE,
            "full_diff_sha256": v2._sha256(full_patch),
            "full_ledger_diff_sha256": None,
            "full_stable_patch_id": v2._stable_patch_id(git_binary, repo, full_patch),
            "identity_authority": "development_only_non_authoritative",
            "ledger_diff_profile": CLI_LEDGER_DIFF_PROFILE,
            "repository_id": ledger["repository_id"],
            "source_diff_sha256": v2._sha256(source_patch),
            "source_ledger_diff_sha256": candidate["source_diff_sha256"],
            "source_paths_sha256": candidate["source_paths_sha256"],
            "source_profile": ledger["profile"],
            "source_stable_patch_id": v2._stable_patch_id(git_binary, repo, source_patch),
            "static_scope_band": candidate["static_scope_band"],
            "test_diff_sha256": v2._sha256(test_patch),
            "test_ledger_diff_sha256": candidate["test_diff_sha256"],
            "test_paths_sha256": candidate["test_paths_sha256"],
            "test_stable_patch_id": v2._stable_patch_id(git_binary, repo, test_patch),
            "tree_oid": tree_oid,
        })
    return records


def _v2_records(ledger: dict[str, Any]) -> list[dict[str, Any]]:
    try:
        v2.validate_ledger(ledger)
    except v2.EligibilityV2Error as exc:
        raise OverlapRegistryError(f"v2 ledger is invalid: {exc}") from exc
    repository_id = ledger["repository"]["repository_id"]
    return [
        {
            "candidate_ref": item["candidate_ref"],
            "changed_paths_sha256": item["changed_paths_sha256"],
            "commit_oid": item["commit_oid"],
            "exact_identity_profile": EXACT_IDENTITY_PROFILE,
            "full_diff_sha256": item["full_diff_sha256"],
            "full_ledger_diff_sha256": item["full_diff_sha256"],
            "full_stable_patch_id": item["full_stable_patch_id"],
            "identity_authority": "development_only_non_authoritative",
            "ledger_diff_profile": EXACT_IDENTITY_PROFILE,
            "repository_id": repository_id,
            "source_diff_sha256": item["source_diff_sha256"],
            "source_ledger_diff_sha256": item["source_diff_sha256"],
            "source_paths_sha256": item["production_go_paths_sha256"],
            "source_profile": ledger["profile"],
            "source_stable_patch_id": item["source_stable_patch_id"],
            "static_scope_band": item["static_scope_band"],
            "test_diff_sha256": item["test_diff_sha256"],
            "test_ledger_diff_sha256": item["test_diff_sha256"],
            "test_paths_sha256": item["test_evidence_paths_sha256"],
            "test_stable_patch_id": item["test_stable_patch_id"],
            "tree_oid": item["tree_oid"],
        }
        for item in ledger["candidates"]
    ]


def _groups(records: Sequence[dict[str, Any]], field: str) -> list[dict[str, Any]]:
    values: dict[str, list[str]] = defaultdict(list)
    for item in records:
        values[item[field]].append(item["candidate_ref"])
    return [
        {"candidate_refs": sorted(refs), "value": value}
        for value, refs in sorted(values.items())
        if len(refs) > 1
    ]


def build_registry(
    *,
    git_binary: pathlib.Path,
    cli_repo: pathlib.Path,
    cli_ledger_path: pathlib.Path,
    v2_ledger_paths: Sequence[pathlib.Path],
    implementation_sha256: str,
    schema_sha256: str,
) -> dict[str, Any]:
    _require(len(v2_ledger_paths) == 3, "registry requires exactly three v2 ledgers")
    for label, item in (("implementation", implementation_sha256), ("schema", schema_sha256)):
        _require(v2.SHA256_RE.fullmatch(item) is not None and item != "0" * 64, f"{label} SHA-256 is invalid")
    cli_ledger, cli_raw = _load(cli_ledger_path)
    inputs = [{
        "artifact_file": cli_ledger_path.name,
        "artifact_sha256": v2._sha256(cli_raw),
        "candidate_count": len(cli_ledger["candidates"]),
        "ledger_sha256": cli_ledger["ledger_sha256"],
        "profile": cli_ledger["profile"],
        "repository_id": cli_ledger["repository_id"],
    }]
    v2_ledgers: list[dict[str, Any]] = []
    v2_raw_inputs: list[tuple[pathlib.Path, dict[str, Any], bytes]] = []
    for path in v2_ledger_paths:
        ledger, raw = _load(path)
        v2.validate_ledger(ledger)
        v2_ledgers.append(ledger)
        v2_raw_inputs.append((path, ledger, raw))
        inputs.append({
            "artifact_file": path.name,
            "artifact_sha256": v2._sha256(raw),
            "candidate_count": len(ledger["candidates"]),
            "ledger_sha256": ledger["ledger_sha256"],
            "profile": ledger["profile"],
            "repository_id": ledger["repository"]["repository_id"],
        })
    git_bindings = [ledger["toolchain_binding"]["git"] for ledger in v2_ledgers]
    _require(all(binding == git_bindings[0] for binding in git_bindings), "v2 Git tool bindings differ")
    git_binary = git_binary.resolve()
    verified_git = v2._verify_git_binary(git_binary, git_bindings[0])
    v2_scanner_sha256 = v2._sha256(pathlib.Path(v2.__file__).resolve().read_bytes())
    _require(
        all(ledger["implementation"]["scanner_sha256"] == v2_scanner_sha256 for ledger in v2_ledgers),
        "v2 input scanner dependency differs from current helper",
    )
    cli_scanner_sha256 = v2._sha256(pathlib.Path(task_eligibility.__file__).resolve().read_bytes())
    cli_task_population_sha256 = v2._sha256(pathlib.Path(cli_task_population.__file__).resolve().read_bytes())
    records = _cli_records(git_binary, cli_repo.resolve(), cli_ledger)
    for _, ledger, _ in v2_raw_inputs:
        records.extend(_v2_records(ledger))
    records.sort(key=lambda item: (item["repository_id"], item["commit_oid"]))
    refs = [item["candidate_ref"] for item in records]
    _require(len(refs) == len(set(refs)), "candidate reference collides globally")
    duplicate_groups = {field: _groups(records, field) for field in DIMENSIONS}
    path_repeats = {field: _groups(records, field) for field in ("changed_paths_sha256", "source_paths_sha256", "test_paths_sha256")}
    bands: defaultdict[str, int] = defaultdict(int)
    repository_counts: defaultdict[str, int] = defaultdict(int)
    for item in records:
        bands[item["static_scope_band"]] += 1
        repository_counts[item["repository_id"]] += 1
    timestamps = [dt.datetime.fromisoformat(ledger["generated_at"].replace("Z", "+00:00")) for ledger in v2_ledgers]
    generated_at = max(timestamps).astimezone(dt.timezone.utc).isoformat().replace("+00:00", "Z")
    result = {
        "authority": {
            "benchmark_execution": "forbidden_not_executed",
            "calibration_membership": "forbidden_permanent_development_exposure",
            "confirmatory_holdout_membership": "forbidden_permanent_development_exposure",
            "owner_hmac": "absent_not_fabricated",
            "population_assignment": "absent_not_authorized",
            "status": STATUS,
        },
        "candidates": records,
        "duplicate_groups": duplicate_groups,
        "exact_identity_profile": EXACT_IDENTITY_PROFILE,
        "exposure": EXPOSURE,
        "generated_at": generated_at,
        "implementation": {
            "cli_scanner_sha256": cli_scanner_sha256,
            "cli_task_population_sha256": cli_task_population_sha256,
            "git_binary_sha256": verified_git["binary_sha256"],
            "git_version_output": verified_git["version_output"],
            "registry_builder_sha256": implementation_sha256,
            "schema_sha256": schema_sha256,
            "v2_scanner_sha256": v2_scanner_sha256,
        },
        "inputs": sorted(inputs, key=lambda item: item["repository_id"]),
        "path_set_repeat_groups": path_repeats,
        "profile": PROFILE,
        "registry_sha256": None,
        "schema_version": 1,
        "summary": {
            "candidate_count": len(records),
            "exact_duplicate_group_count": sum(len(groups) for groups in duplicate_groups.values()),
            "repository_candidate_counts": dict(sorted(repository_counts.items())),
            "static_scope_band_counts": {"high": bands["high"], "low": bands["low"], "medium": bands["medium"]},
        },
        "unresolved_overlap": {
            "related_family": "unresolved_no_owner_family_commitment",
            "semantic_identity": "unresolved_no_authoritative_semantic_review",
            "source_session": v2.SESSION_STATUS,
        },
    }
    result["registry_sha256"] = _self_hash(result)
    validate_registry(result)
    return result


def validate_registry(value: dict[str, Any]) -> None:
    expected_root = {
        "authority", "candidates", "duplicate_groups", "exact_identity_profile", "exposure", "generated_at", "implementation", "inputs",
        "path_set_repeat_groups", "profile", "registry_sha256", "schema_version", "summary", "unresolved_overlap",
    }
    _require(isinstance(value, dict) and set(value) == expected_root, "registry root fields differ")
    _require(value["schema_version"] == 1 and value["profile"] == PROFILE, "registry profile differs")
    _require(value["exact_identity_profile"] == EXACT_IDENTITY_PROFILE, "registry exact-identity profile differs")
    _require(value["exposure"] == EXPOSURE, "registry exposure differs")
    _require(value["authority"] == {
        "benchmark_execution": "forbidden_not_executed",
        "calibration_membership": "forbidden_permanent_development_exposure",
        "confirmatory_holdout_membership": "forbidden_permanent_development_exposure",
        "owner_hmac": "absent_not_fabricated",
        "population_assignment": "absent_not_authorized",
        "status": STATUS,
    }, "registry authority differs")
    _require(value["unresolved_overlap"] == {
        "related_family": "unresolved_no_owner_family_commitment",
        "semantic_identity": "unresolved_no_authoritative_semantic_review",
        "source_session": v2.SESSION_STATUS,
    }, "registry unresolved-overlap boundary differs")
    v2._validate_timestamp(value["generated_at"], "generated_at")
    recorded = value["registry_sha256"]
    _require(isinstance(recorded, str) and v2.SHA256_RE.fullmatch(recorded) is not None and recorded != "0" * 64, "registry SHA-256 is invalid")
    _require(recorded == _self_hash(value), "registry self hash mismatch")
    implementation = value["implementation"]
    expected_implementation_fields = {
        "cli_scanner_sha256", "cli_task_population_sha256", "git_binary_sha256", "git_version_output",
        "registry_builder_sha256", "schema_sha256", "v2_scanner_sha256",
    }
    _require(isinstance(implementation, dict) and set(implementation) == expected_implementation_fields, "registry implementation fields differ")
    for field in expected_implementation_fields - {"git_version_output"}:
        item = implementation[field]
        _require(isinstance(item, str) and v2.SHA256_RE.fullmatch(item) is not None and item != "0" * 64, f"registry implementation {field} is invalid")
    _require(isinstance(implementation["git_version_output"], str) and implementation["git_version_output"].startswith("git version "), "registry Git version is invalid")
    inputs = value["inputs"]
    _require(isinstance(inputs, list) and len(inputs) == 4, "registry inputs differ")
    expected_input_fields = {"artifact_file", "artifact_sha256", "candidate_count", "ledger_sha256", "profile", "repository_id"}
    allowed_profiles = {task_eligibility.PROFILE, v2.PROFILE}
    for index, item in enumerate(inputs):
        _require(isinstance(item, dict) and set(item) == expected_input_fields, f"input[{index}] fields differ")
        _require(
            isinstance(item["artifact_file"], str)
            and bool(item["artifact_file"])
            and pathlib.PurePosixPath(item["artifact_file"]).name == item["artifact_file"],
            f"input[{index}].artifact_file is invalid",
        )
        for field in ("artifact_sha256", "ledger_sha256"):
            digest = item[field]
            _require(isinstance(digest, str) and v2.SHA256_RE.fullmatch(digest) is not None and digest != "0" * 64, f"input[{index}].{field} is invalid")
        _require(type(item["candidate_count"]) is int and item["candidate_count"] >= 0, f"input[{index}].candidate_count is invalid")
        _require(item["profile"] in allowed_profiles, f"input[{index}].profile is invalid")
        _require(isinstance(item["repository_id"], str) and item["repository_id"].startswith("github.com/"), f"input[{index}].repository_id is invalid")
    _require(inputs == sorted(inputs, key=lambda item: item["repository_id"]), "registry inputs are not canonical")
    _require(len({item["artifact_file"] for item in inputs}) == len(inputs), "registry input artifact files are duplicated")
    _require(len({item["repository_id"] for item in inputs}) == len(inputs), "registry input repositories are duplicated")
    _require(sum(item["profile"] == task_eligibility.PROFILE for item in inputs) == 1, "registry must contain exactly one CLI v1 input")
    _require(sum(item["profile"] == v2.PROFILE for item in inputs) == 3, "registry must contain exactly three v2 inputs")
    _require(sum(item["candidate_count"] for item in inputs) == len(value["candidates"]), "registry input counts differ")
    records = value["candidates"]
    _require(isinstance(records, list), "registry candidates must be a list")
    expected_candidate_fields = {
        "candidate_ref", "changed_paths_sha256", "commit_oid", "exact_identity_profile", "full_diff_sha256",
        "full_ledger_diff_sha256", "full_stable_patch_id", "identity_authority", "ledger_diff_profile",
        "repository_id", "source_diff_sha256", "source_ledger_diff_sha256", "source_paths_sha256",
        "source_profile", "source_stable_patch_id", "static_scope_band", "test_diff_sha256",
        "test_ledger_diff_sha256", "test_paths_sha256", "test_stable_patch_id", "tree_oid",
    }
    input_profiles = {item["repository_id"]: item["profile"] for item in inputs}
    observed_repository_counts: defaultdict[str, int] = defaultdict(int)
    for index, item in enumerate(records):
        _require(isinstance(item, dict) and set(item) == expected_candidate_fields, f"candidate[{index}] fields differ")
        _require(item["exact_identity_profile"] == EXACT_IDENTITY_PROFILE, f"candidate[{index}] exact-identity profile differs")
        _require(item["identity_authority"] == "development_only_non_authoritative", f"candidate[{index}] identity authority differs")
        _require(item["repository_id"] in input_profiles, f"candidate[{index}] repository is absent from inputs")
        _require(item["source_profile"] == input_profiles[item["repository_id"]], f"candidate[{index}] source profile differs from input")
        _require(item["static_scope_band"] in {"low", "medium", "high"}, f"candidate[{index}] static scope band is invalid")
        for field in ("candidate_ref", "changed_paths_sha256", "source_paths_sha256", "test_paths_sha256", "source_diff_sha256", "test_diff_sha256", "full_diff_sha256", "source_ledger_diff_sha256", "test_ledger_diff_sha256"):
            _require(isinstance(item[field], str) and v2.SHA256_RE.fullmatch(item[field]) is not None and item[field] != "0" * 64, f"candidate[{index}].{field} is invalid")
        if item["source_profile"] == task_eligibility.PROFILE:
            _require(item["ledger_diff_profile"] == CLI_LEDGER_DIFF_PROFILE, f"candidate[{index}] CLI ledger-diff profile differs")
            _require(item["full_ledger_diff_sha256"] is None, f"candidate[{index}] CLI full ledger diff must be absent")
        else:
            _require(item["ledger_diff_profile"] == EXACT_IDENTITY_PROFILE, f"candidate[{index}] v2 ledger-diff profile differs")
            _require(item["full_ledger_diff_sha256"] == item["full_diff_sha256"], f"candidate[{index}] v2 full ledger diff differs")
            _require(item["source_ledger_diff_sha256"] == item["source_diff_sha256"], f"candidate[{index}] v2 source ledger diff differs")
            _require(item["test_ledger_diff_sha256"] == item["test_diff_sha256"], f"candidate[{index}] v2 test ledger diff differs")
        for field in ("commit_oid", "tree_oid", "source_stable_patch_id", "test_stable_patch_id", "full_stable_patch_id"):
            _require(isinstance(item[field], str) and v2.OID_RE.fullmatch(item[field]) is not None and item[field] != "0" * 40, f"candidate[{index}].{field} is invalid")
        observed_repository_counts[item["repository_id"]] += 1
    _require(records == sorted(records, key=lambda item: (item["repository_id"], item["commit_oid"])), "registry candidates are not canonical")
    refs = [item["candidate_ref"] for item in records]
    _require(len(refs) == len(set(refs)), "registry candidate reference collides")
    expected_input_counts = {item["repository_id"]: item["candidate_count"] for item in inputs}
    _require(dict(sorted(observed_repository_counts.items())) == dict(sorted(expected_input_counts.items())), "registry per-input candidate counts differ")
    expected_duplicates = {field: _groups(records, field) for field in DIMENSIONS}
    expected_paths = {field: _groups(records, field) for field in ("changed_paths_sha256", "source_paths_sha256", "test_paths_sha256")}
    _require(value["duplicate_groups"] == expected_duplicates, "exact duplicate groups differ")
    _require(value["path_set_repeat_groups"] == expected_paths, "path-set repeat groups differ")
    bands: defaultdict[str, int] = defaultdict(int)
    repositories: defaultdict[str, int] = defaultdict(int)
    for item in records:
        bands[item["static_scope_band"]] += 1
        repositories[item["repository_id"]] += 1
    expected_summary = {
        "candidate_count": len(records),
        "exact_duplicate_group_count": sum(len(groups) for groups in expected_duplicates.values()),
        "repository_candidate_counts": dict(sorted(repositories.items())),
        "static_scope_band_counts": {"high": bands["high"], "low": bands["low"], "medium": bands["medium"]},
    }
    _require(value["summary"] == expected_summary, "registry summary differs")


def verify_registry_dependencies(
    value: dict[str, Any],
    *,
    git_binary: pathlib.Path,
    cli_repo: pathlib.Path,
    cli_ledger_path: pathlib.Path,
    v2_ledger_paths: Sequence[pathlib.Path],
    schema: pathlib.Path,
) -> None:
    validate_registry(value)
    try:
        implementation_hash = v2._sha256(pathlib.Path(__file__).read_bytes())
        schema_hash = v2._sha256(schema.read_bytes())
    except OSError as exc:
        raise OverlapRegistryError(f"cannot read registry dependency: {exc}") from exc
    rebuilt = build_registry(
        git_binary=git_binary,
        cli_repo=cli_repo,
        cli_ledger_path=cli_ledger_path,
        v2_ledger_paths=v2_ledger_paths,
        implementation_sha256=implementation_hash,
        schema_sha256=schema_hash,
    )
    _require(rebuilt == value, "dependency-backed full registry rebuild differs")


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
    subparsers = parser.add_subparsers(dest="command", required=True)
    build = subparsers.add_parser("build")
    build.add_argument("--git-binary", required=True, type=pathlib.Path)
    build.add_argument("--cli-repo", required=True, type=pathlib.Path)
    build.add_argument("--cli-ledger", required=True, type=pathlib.Path)
    build.add_argument("--v2-ledger", required=True, action="append", type=pathlib.Path)
    build.add_argument("--schema", required=True, type=pathlib.Path)
    build.add_argument("--output", required=True, type=pathlib.Path)
    check = subparsers.add_parser("check")
    check.add_argument("registry", type=pathlib.Path)
    check.add_argument("--cli-repo", type=pathlib.Path)
    check.add_argument("--cli-ledger", type=pathlib.Path)
    check.add_argument("--v2-ledger", action="append", type=pathlib.Path)
    check.add_argument("--schema", type=pathlib.Path)
    check.add_argument("--git-binary", type=pathlib.Path)
    args = parser.parse_args(argv)
    try:
        if args.command == "build":
            result = build_registry(
                git_binary=args.git_binary,
                cli_repo=args.cli_repo,
                cli_ledger_path=args.cli_ledger,
                v2_ledger_paths=args.v2_ledger,
                implementation_sha256=v2._sha256(pathlib.Path(__file__).read_bytes()),
                schema_sha256=v2._sha256(args.schema.read_bytes()),
            )
            _write_atomic(args.output, result)
        else:
            value, _ = _load(args.registry)
            validate_registry(value)
            dependency_args = (args.git_binary, args.cli_repo, args.cli_ledger, args.v2_ledger, args.schema)
            _require(all(item is None for item in dependency_args) or all(item is not None for item in dependency_args), "all registry dependency arguments must be supplied together")
            if args.cli_repo is not None:
                verify_registry_dependencies(
                    value,
                    git_binary=args.git_binary,
                    cli_repo=args.cli_repo,
                    cli_ledger_path=args.cli_ledger,
                    v2_ledger_paths=args.v2_ledger,
                    schema=args.schema,
                )
    except (OverlapRegistryError, v2.EligibilityV2Error, OSError) as exc:
        parser.error(str(exc))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
