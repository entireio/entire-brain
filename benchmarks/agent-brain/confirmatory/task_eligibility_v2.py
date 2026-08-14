#!/usr/bin/env python3
"""Build repository-bound, development-only task inventories.

Version 2 keeps production Go paths disjoint from all test evidence, supports
single-parent and two-parent first-parent integration units, binds module and
toolchain identities, and records stable patch IDs.  It never runs candidate
tests, assigns a split, creates an owner key, or authorizes a benchmark run.
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
from typing import Any, Iterable, Sequence


PROFILE = "agent_brain_development_task_eligibility_scan_v2"
SELECTION_RULE = "pinned_31_first_parent_production_go_plus_test_evidence_v2"
EXPOSURE = "permanent_development_only_identity_inspected"
AUTHORITY_STATUS = "non_authoritative_development_inventory_only"
SESSION_STATUS = "unresolved_no_owner_source_session_receipt"
EXECUTION_STATUS = "not_executed"
OID_RE = re.compile(r"^[0-9a-f]{40}$")
SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
GIT_CONFIG_OVERRIDES = (
    "color.ui=false",
    "core.abbrev=40",
    f"core.attributesFile={os.devnull}",
    "core.quotePath=true",
    "diff.algorithm=myers",
    "diff.color=false",
    "diff.compactionHeuristic=false",
    "diff.context=3",
    "diff.ignoreSubmodules=none",
    "diff.indentHeuristic=false",
    "diff.interHunkContext=0",
    "diff.mnemonicPrefix=false",
    "diff.noprefix=false",
    f"diff.orderFile={os.devnull}",
    "diff.relative=false",
    "diff.renames=false",
    "diff.submodule=short",
    "diff.suppressBlankEmpty=false",
    "log.showSignature=false",
    "patchid.stable=true",
    "patchid.verbatim=false",
    "submodule.recurse=false",
)


class EligibilityV2Error(ValueError):
    """Raised when a v2 inventory cannot be reproduced exactly."""


def _require(condition: bool, message: str) -> None:
    if not condition:
        raise EligibilityV2Error(message)


def _sha256(value: bytes) -> str:
    return hashlib.sha256(value).hexdigest()


def _canonical_hash(value: Any) -> str:
    return _sha256(_canonical_json_bytes(value))


def _reject_floats(value: Any) -> None:
    if isinstance(value, float):
        raise EligibilityV2Error("canonical v2 inventory JSON forbids floating-point values")
    if isinstance(value, dict):
        for child in value.values():
            _reject_floats(child)
    elif isinstance(value, list):
        for child in value:
            _reject_floats(child)


def _canonical_json_bytes(value: Any) -> bytes:
    _reject_floats(value)
    return json.dumps(
        value,
        ensure_ascii=False,
        allow_nan=False,
        sort_keys=True,
        separators=(",", ":"),
    ).encode("utf-8")


def _self_hash(value: dict[str, Any]) -> str:
    projected = copy.deepcopy(value)
    _require("ledger_sha256" in projected, "ledger_sha256 is missing")
    projected["ledger_sha256"] = None
    return _canonical_hash(projected)


def _reject_duplicate_pairs(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        _require(key not in result, "JSON contains a duplicate object key")
        result[key] = value
    return result


def _load_json(path: pathlib.Path) -> tuple[dict[str, Any], bytes]:
    try:
        raw = path.read_bytes()
        value = json.loads(raw, object_pairs_hook=_reject_duplicate_pairs)
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        raise EligibilityV2Error(f"cannot load JSON: {exc}") from exc
    _require(isinstance(value, dict), "JSON root must be an object")
    return value, raw


def _git(git_binary: pathlib.Path, repo: pathlib.Path, args: Sequence[str], *, input_bytes: bytes | None = None) -> bytes:
    _require(git_binary.is_absolute(), "Git binary path must be absolute")
    environment = dict(os.environ)
    for key in list(environment):
        if key in {
            "GIT_ALTERNATE_OBJECT_DIRECTORIES",
            "GIT_ATTR_SOURCE",
            "GIT_CEILING_DIRECTORIES",
            "GIT_COMMON_DIR",
            "GIT_CONFIG_COUNT",
            "GIT_CONFIG_PARAMETERS",
            "GIT_DIFF_OPTS",
            "GIT_DIR",
            "GIT_EXTERNAL_DIFF",
            "GIT_GLOB_PATHSPECS",
            "GIT_ICASE_PATHSPECS",
            "GIT_INDEX_FILE",
            "GIT_NAMESPACE",
            "GIT_NOGLOB_PATHSPECS",
            "GIT_OBJECT_DIRECTORY",
            "GIT_REPLACE_REF_BASE",
            "GIT_SHALLOW_FILE",
            "GIT_WORK_TREE",
        } or key.startswith("GIT_CONFIG_KEY_") or key.startswith("GIT_CONFIG_VALUE_"):
            environment.pop(key, None)
    environment["GIT_ATTR_NOSYSTEM"] = "1"
    environment["GIT_CONFIG_GLOBAL"] = os.devnull
    environment["GIT_CONFIG_NOSYSTEM"] = "1"
    environment["GIT_CONFIG_SYSTEM"] = os.devnull
    environment["GIT_LITERAL_PATHSPECS"] = "1"
    environment["GIT_NO_LAZY_FETCH"] = "1"
    environment["GIT_NO_REPLACE_OBJECTS"] = "1"
    environment["GIT_OPTIONAL_LOCKS"] = "0"
    environment["GIT_PAGER"] = "cat"
    environment["GIT_TERMINAL_PROMPT"] = "0"
    environment["LANG"] = "C"
    environment["LC_ALL"] = "C"
    command = [str(git_binary)]
    for config in GIT_CONFIG_OVERRIDES:
        command.extend(("-c", config))
    command.extend(args)
    try:
        completed = subprocess.run(
            command,
            cwd=repo,
            input=input_bytes,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
            env=environment,
        )
    except OSError as exc:
        raise EligibilityV2Error(f"cannot execute git: {exc}") from exc
    if completed.returncode != 0:
        message = completed.stderr.decode("utf-8", errors="replace").strip()
        raise EligibilityV2Error(f"git command failed: {message or 'unknown error'}")
    return completed.stdout


def _git_text(git_binary: pathlib.Path, repo: pathlib.Path, args: Sequence[str]) -> str:
    try:
        return _git(git_binary, repo, args).decode("utf-8")
    except UnicodeDecodeError as exc:
        raise EligibilityV2Error("git emitted non-UTF-8 metadata") from exc


def _oid(git_binary: pathlib.Path, repo: pathlib.Path, revision: str) -> str:
    value = _git_text(git_binary, repo, ["rev-parse", "--verify", f"{revision}^{{commit}}" ]).strip()
    _require(OID_RE.fullmatch(value) is not None and value != "0" * 40, "invalid Git object ID")
    return value


def _is_test_path(path: str) -> bool:
    parts = pathlib.PurePosixPath(path).parts
    return path.endswith("_test.go") or "testdata" in parts or (bool(parts) and parts[0] in {"test", "tests"})


def _is_production_go_path(path: str) -> bool:
    return path.endswith(".go") and not _is_test_path(path)


def static_scope_score(source_files: int, test_files: int, source_packages: int) -> int:
    _require(source_files > 0 and test_files > 0 and source_packages > 0, "static scope counts must be positive")
    return min(source_files, 4) + min(test_files, 3) + min(source_packages, 3) + (2 if source_packages > 1 else 0)


def static_scope_band(score: int) -> str:
    if score <= 5:
        return "low"
    if score <= 8:
        return "medium"
    return "high"


# The remaining scanner, validator, and CLI are intentionally in this file so
# the generated ledgers can bind one exact implementation hash.


def _candidate_ref(repository_id: str, commit_oid: str) -> str:
    payload = b"entire-brain/task-eligibility-v2/candidate\0" + repository_id.encode("utf-8") + b"\0" + commit_oid.encode("ascii")
    return _sha256(payload)


def _validate_timestamp(value: Any, field: str) -> None:
    _require(isinstance(value, str) and bool(value), f"{field} is invalid")
    try:
        parsed = dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError as exc:
        raise EligibilityV2Error(f"{field} is invalid") from exc
    _require(parsed.tzinfo is not None and parsed.utcoffset() is not None, f"{field} must include a UTC offset")


def _validate_repository_entry(value: Any) -> dict[str, Any]:
    _require(isinstance(value, dict), "repository binding must be an object")
    _require(set(value) == {"key", "remote", "repository_id", "toolchain", "window"}, "repository binding fields differ")
    _require(isinstance(value["key"], str) and bool(value["key"]), "repository key is invalid")
    repository_id = value["repository_id"]
    _require(
        isinstance(repository_id, str)
        and repository_id.startswith("github.com/")
        and "\n" not in repository_id
        and "\r" not in repository_id,
        "repository_id is invalid",
    )
    remote = value["remote"]
    _require(isinstance(remote, dict) and set(remote) == {"name", "ref", "url"}, "remote binding fields differ")
    _require(all(isinstance(remote[field], str) and bool(remote[field]) for field in remote), "remote binding is invalid")
    _require(remote["ref"] == f"refs/remotes/{remote['name']}/main", "remote ref is not the exact authoritative main ref")
    _require(remote["url"].startswith("https://github.com/") and remote["url"].endswith(".git"), "remote URL is invalid")
    window = value["window"]
    _require(isinstance(window, dict) and set(window) == {"base_oid", "first_parent_unit_count", "head_oid"}, "window binding fields differ")
    _require(window["first_parent_unit_count"] == 31, "v2 window must contain exactly 31 first-parent units")
    for field in ("base_oid", "head_oid"):
        item = window[field]
        _require(isinstance(item, str) and OID_RE.fullmatch(item) is not None and item != "0" * 40, f"window.{field} is invalid")
    toolchain = value["toolchain"]
    _require(isinstance(toolchain, dict) and set(toolchain) == {"git", "go", "native"}, "toolchain binding fields differ")
    git = toolchain["git"]
    go = toolchain["go"]
    native = toolchain["native"]
    _require(isinstance(git, dict) and set(git) == {"binary_sha256", "version_output"}, "Git binding fields differ")
    _require(isinstance(go, dict) and set(go) == {"binary_sha256", "cc", "cgo_enabled", "cxx", "goarch", "goos", "version_output"}, "Go binding fields differ")
    _require(isinstance(native, dict) and set(native) == {"cc_binary_sha256", "cxx_binary_sha256", "target", "version_first_line"}, "native binding fields differ")
    for container, fields in ((git, ("binary_sha256",)), (go, ("binary_sha256",)), (native, ("cc_binary_sha256", "cxx_binary_sha256"))):
        for field in fields:
            _require(isinstance(container[field], str) and SHA256_RE.fullmatch(container[field]) is not None, f"{field} is invalid")
    _require(isinstance(git["version_output"], str) and git["version_output"].startswith("git version "), "Git version output is invalid")
    _require(isinstance(go["version_output"], str) and go["version_output"].startswith("go version go"), "Go version output is invalid")
    _require(
        all(isinstance(go[field], str) and bool(go[field]) for field in ("cc", "cgo_enabled", "cxx", "goarch", "goos")),
        "Go environment binding is invalid",
    )
    _require(isinstance(native["version_first_line"], str) and bool(native["version_first_line"]), "native version is invalid")
    _require(isinstance(native["target"], str) and bool(native["target"]), "native target is invalid")
    return value


def load_repository_contract(path: pathlib.Path, key: str) -> tuple[dict[str, Any], str]:
    value, raw = _load_json(path)
    _require(
        set(value) == {"authority", "profile", "repositories", "schema_version", "window_policy"},
        "repository contract fields differ",
    )
    _require(value["schema_version"] == 1, "repository contract schema version differs")
    _require(value["profile"] == "agent_brain_development_repository_bindings_v2", "repository contract profile differs")
    _require(value["window_policy"] == "exact_31_first_parent_units_at_pinned_head_ancestor_of_authoritative_remote_ref", "window policy differs")
    expected_authority = {
        "benchmark_execution": "forbidden_not_executed",
        "calibration_membership": "forbidden_permanent_development_exposure",
        "confirmatory_holdout_membership": "forbidden_permanent_development_exposure",
        "owner_key": "absent_not_fabricated",
        "population_assignment": "absent_not_authorized",
        "source_session_identity": SESSION_STATUS,
    }
    _require(value["authority"] == expected_authority, "repository authority boundary differs")
    repositories = value["repositories"]
    _require(isinstance(repositories, list) and bool(repositories), "repository list is invalid")
    entries = [_validate_repository_entry(item) for item in repositories]
    keys = [item["key"] for item in entries]
    _require(len(keys) == len(set(keys)), "duplicate repository key")
    selected = [item for item in entries if item["key"] == key]
    _require(len(selected) == 1, "repository key is absent or ambiguous")
    return selected[0], _sha256(raw)


def _verify_git_binary(git_binary: pathlib.Path, expected: dict[str, Any]) -> dict[str, Any]:
    _require(git_binary.is_absolute() and git_binary.is_file(), "Git binary is missing or not absolute")
    _require(_sha256(git_binary.read_bytes()) == expected["binary_sha256"], "Git binary hash differs")
    environment = dict(os.environ)
    environment["LANG"] = "C"
    environment["LC_ALL"] = "C"
    version = subprocess.run(
        [str(git_binary), "--version"],
        cwd=pathlib.Path("/"),
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
        env=environment,
    )
    _require(version.returncode == 0, "Git version command failed")
    try:
        output = version.stdout.decode("utf-8").strip()
    except UnicodeDecodeError as exc:
        raise EligibilityV2Error("Git version output is not UTF-8") from exc
    _require(output == expected["version_output"], "Git version output differs")
    return dict(expected)


def _verify_toolchain(
    git_binary: pathlib.Path,
    go_binary: pathlib.Path,
    native_binary: pathlib.Path,
    native_cxx_binary: pathlib.Path,
    expected: dict[str, Any],
) -> dict[str, Any]:
    for path, label in ((go_binary, "Go"), (native_binary, "native C compiler"), (native_cxx_binary, "native C++ compiler")):
        _require(path.is_file(), f"{label} binary is missing")
    _require(_sha256(go_binary.read_bytes()) == expected["go"]["binary_sha256"], "Go binary hash differs")
    _require(_sha256(native_binary.read_bytes()) == expected["native"]["cc_binary_sha256"], "native C compiler hash differs")
    _require(_sha256(native_cxx_binary.read_bytes()) == expected["native"]["cxx_binary_sha256"], "native C++ compiler hash differs")
    verified_git = _verify_git_binary(git_binary, expected["git"])
    go_env = dict(os.environ)
    go_env["GOTOOLCHAIN"] = "local"
    go_env["GOENV"] = "off"
    go_version = subprocess.run(
        [str(go_binary), "version"],
        cwd=pathlib.Path("/"),
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
        env=go_env,
    )
    _require(go_version.returncode == 0, "Go version command failed")
    go_environment = subprocess.run(
        [str(go_binary), "env", "-json", "CGO_ENABLED", "CC", "CXX", "GOARCH", "GOOS"],
        cwd=pathlib.Path("/"),
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
        env=go_env,
    )
    _require(go_environment.returncode == 0, "Go environment command failed")
    native_versions = [
        subprocess.run(
            [str(binary), "--version"],
            cwd=pathlib.Path("/"),
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
        )
        for binary in (native_binary, native_cxx_binary)
    ]
    _require(all(item.returncode == 0 for item in native_versions), "native compiler version command failed")
    try:
        go_output = go_version.stdout.decode("utf-8").strip()
        go_environment_value = json.loads(go_environment.stdout)
        native_outputs = [item.stdout.decode("utf-8").splitlines() for item in native_versions]
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise EligibilityV2Error("toolchain version output is not UTF-8") from exc
    _require(go_output == expected["go"]["version_output"], "Go version output differs")
    expected_environment = {
        "CC": expected["go"]["cc"],
        "CGO_ENABLED": expected["go"]["cgo_enabled"],
        "CXX": expected["go"]["cxx"],
        "GOARCH": expected["go"]["goarch"],
        "GOOS": expected["go"]["goos"],
    }
    _require(go_environment_value == expected_environment, "Go environment binding differs")
    for native_lines in native_outputs:
        _require(bool(native_lines) and native_lines[0] == expected["native"]["version_first_line"], "native compiler version differs")
        targets = [line.removeprefix("Target: ") for line in native_lines if line.startswith("Target: ")]
        _require(targets == [expected["native"]["target"]], "native compiler target differs")
    return {
        "git": verified_git,
        "go": dict(expected["go"]),
        "native": dict(expected["native"]),
        "verification_status": "matched_local_binaries_without_candidate_execution",
    }


def _require_empty_git_metadata_file(git_binary: pathlib.Path, repo: pathlib.Path, relative: str) -> None:
    output = _git_text(git_binary, repo, ["rev-parse", "--path-format=absolute", "--git-path", relative])
    _require(output.endswith("\n"), f"cannot resolve repository {relative} path")
    path_value = output[:-1]
    _require(
        bool(path_value)
        and "\n" not in path_value
        and "\r" not in path_value
        and pathlib.Path(path_value).is_absolute(),
        f"repository {relative} path is invalid",
    )
    path = pathlib.Path(path_value)
    if os.path.lexists(path):
        try:
            raw = path.read_bytes()
        except OSError as exc:
            raise EligibilityV2Error(f"cannot inspect repository {relative}: {exc}") from exc
        _require(not raw, f"repository {relative} must be absent or empty")


def _verify_repository_binding(git_binary: pathlib.Path, repo: pathlib.Path, entry: dict[str, Any]) -> list[str]:
    replace_refs = [line for line in _git_text(git_binary, repo, ["for-each-ref", "--format=%(refname)", "refs/replace"]).splitlines() if line]
    _require(not replace_refs, "repository contains forbidden replace refs")
    _require_empty_git_metadata_file(git_binary, repo, "info/attributes")
    _require_empty_git_metadata_file(git_binary, repo, "info/grafts")
    shallow_status = _git_text(git_binary, repo, ["rev-parse", "--is-shallow-repository"]).strip()
    _require(shallow_status == "false", "repository must not be shallow")
    remote = entry["remote"]
    actual_urls = [line for line in _git_text(git_binary, repo, ["config", "--local", "--get-all", f"remote.{remote['name']}.url"]).splitlines() if line]
    _require(actual_urls == [remote["url"]], "authoritative remote URL differs")
    authoritative_tip_oid = _oid(git_binary, repo, remote["ref"])
    window = entry["window"]
    pinned_head_oid = _oid(git_binary, repo, window["head_oid"])
    merge_base = _git_text(git_binary, repo, ["merge-base", pinned_head_oid, authoritative_tip_oid]).strip()
    _require(merge_base == pinned_head_oid, "pinned head is not an ancestor of the authoritative remote ref")
    exact_base = _oid(git_binary, repo, f"{pinned_head_oid}~{window['first_parent_unit_count']}")
    _require(exact_base == window["base_oid"], "pinned base is not exact pinned-head~window")
    units = [line for line in _git_text(git_binary, repo, ["rev-list", "--first-parent", "--reverse", f"{window['base_oid']}..{pinned_head_oid}"]).splitlines() if line]
    _require(len(units) == window["first_parent_unit_count"], "first-parent window size differs")
    return units


def _changed_paths(git_binary: pathlib.Path, repo: pathlib.Path, parent: str, commit: str) -> list[str]:
    raw = _git(
        git_binary,
        repo,
        [
            f"--attr-source={commit}", "diff", "--name-only", "-z", "--no-relative", "--no-color", "--no-ext-diff", "--no-textconv",
            "--no-renames", "--submodule=short", "--ignore-submodules=none", f"-O{os.devnull}", parent, commit, "--",
        ],
    )
    try:
        paths = [part.decode("utf-8") for part in raw.split(b"\0") if part]
    except UnicodeDecodeError as exc:
        raise EligibilityV2Error("changed path is not UTF-8") from exc
    _require(paths == sorted(set(paths)), "changed paths are not canonical")
    return paths


def _require_unspecified_diff_attributes(git_binary: pathlib.Path, repo: pathlib.Path, commit: str, paths: Sequence[str]) -> None:
    _require(list(paths) == sorted(set(paths)), "diff paths are not canonical")
    input_bytes = b"\0".join(path.encode("utf-8") for path in paths) + b"\0"
    raw = _git(
        git_binary,
        repo,
        [f"--attr-source={commit}", "check-attr", "-z", "--stdin", "diff"],
        input_bytes=input_bytes,
    )
    fields = raw.split(b"\0")
    _require(fields[-1:] == [b""], "Git attribute output is not NUL terminated")
    fields = fields[:-1]
    _require(len(fields) == len(paths) * 3, "Git attribute output count differs")
    for index, path in enumerate(paths):
        path_field, attribute, value = fields[index * 3:index * 3 + 3]
        try:
            rendered_path = path_field.decode("utf-8")
        except UnicodeDecodeError as exc:
            raise EligibilityV2Error("Git attribute path is not UTF-8") from exc
        _require(rendered_path == path and attribute == b"diff", "Git attribute output differs")
        _require(value == b"unspecified", f"candidate diff attribute is not allowed for {path}")


def _diff(git_binary: pathlib.Path, repo: pathlib.Path, parent: str, commit: str, paths: Sequence[str]) -> bytes:
    _require(bool(paths), "cannot create an empty path-scoped diff")
    _require_unspecified_diff_attributes(git_binary, repo, commit, paths)
    return _git(
        git_binary,
        repo,
        [
            f"--attr-source={commit}", "diff", "--binary", "--full-index", "--unified=3", "--inter-hunk-context=0",
            "--src-prefix=a/", "--dst-prefix=b/", "--line-prefix=", "--output-indicator-new=+",
            "--output-indicator-old=-", "--output-indicator-context= ", "--no-relative", "--no-color",
            "--no-ext-diff", "--no-textconv", "--no-renames", "--no-indent-heuristic",
            "--diff-algorithm=myers", "--submodule=short", "--ignore-submodules=none",
            "--ws-error-highlight=none", f"-O{os.devnull}", parent, commit, "--", *paths,
        ],
    )


def _stable_patch_id(git_binary: pathlib.Path, repo: pathlib.Path, patch: bytes) -> str:
    output = _git(git_binary, repo, ["patch-id", "--stable"], input_bytes=patch).decode("ascii", errors="strict").splitlines()
    _require(len(output) == 1, "stable patch ID output differs")
    patch_id = output[0].split()[0]
    _require(OID_RE.fullmatch(patch_id) is not None and patch_id != "0" * 40, "stable patch ID is invalid")
    return patch_id


def _tree_paths(git_binary: pathlib.Path, repo: pathlib.Path, commit: str) -> list[str]:
    raw = _git(git_binary, repo, ["ls-tree", "-r", "-z", "--name-only", commit])
    try:
        paths = [part.decode("utf-8") for part in raw.split(b"\0") if part]
    except UnicodeDecodeError as exc:
        raise EligibilityV2Error("tree path is not UTF-8") from exc
    _require(paths == sorted(set(paths)), "tree paths are not canonical")
    return paths


def _manifest(git_binary: pathlib.Path, repo: pathlib.Path, commit: str, path: str) -> dict[str, Any]:
    raw = _git(git_binary, repo, ["show", f"{commit}:{path}"])
    try:
        text = raw.decode("utf-8")
    except UnicodeDecodeError as exc:
        raise EligibilityV2Error(f"manifest {path} is not UTF-8") from exc
    blob_oid = _git_text(git_binary, repo, ["rev-parse", f"{commit}:{path}"]).strip()
    _require(OID_RE.fullmatch(blob_oid) is not None and blob_oid != "0" * 40, "manifest blob OID is invalid")
    kind = pathlib.PurePosixPath(path).name
    root = str(pathlib.PurePosixPath(path).parent)
    module_match = re.search(r"(?m)^\s*module\s+([^\s]+)\s*$", text) if kind == "go.mod" else None
    go_match = re.search(r"(?m)^\s*go\s+([^\s]+)\s*$", text)
    toolchain_match = re.search(r"(?m)^\s*toolchain\s+([^\s]+)\s*$", text)
    if kind == "go.mod":
        _require(module_match is not None, f"{path} lacks a module directive")
    return {
        "blob_oid": blob_oid,
        "go_directive": go_match.group(1) if go_match else None,
        "kind": kind,
        "module_directive_sha256": _sha256(module_match.group(1).encode("utf-8")) if module_match else None,
        "path": path,
        "raw_sha256": _sha256(raw),
        "root": root,
        "toolchain_directive": toolchain_match.group(1) if toolchain_match else None,
    }


def _manifests(git_binary: pathlib.Path, repo: pathlib.Path, commit: str, tree_paths: Sequence[str]) -> tuple[list[dict[str, Any]], list[dict[str, Any]]]:
    modules = [_manifest(git_binary, repo, commit, path) for path in tree_paths if pathlib.PurePosixPath(path).name == "go.mod"]
    workspaces = [_manifest(git_binary, repo, commit, path) for path in tree_paths if pathlib.PurePosixPath(path).name == "go.work"]
    _require(bool(modules), "candidate tree contains no go.mod")
    return modules, workspaces


def _within(root: str, path: str) -> bool:
    return root == "." or path == root or path.startswith(f"{root}/")


def _module_for_path(path: str, modules: Sequence[dict[str, Any]]) -> dict[str, Any] | None:
    matches = [module for module in modules if _within(module["root"], path)]
    if not matches:
        return None
    return max(matches, key=lambda item: len(pathlib.PurePosixPath(item["root"]).parts) if item["root"] != "." else 0)


def _ancestors(directory: str, stop: str) -> Iterable[str]:
    current = pathlib.PurePosixPath(directory)
    stop_path = pathlib.PurePosixPath(stop)
    while True:
        rendered = str(current)
        if rendered == "":
            rendered = "."
        if not _within(stop, rendered):
            return
        yield rendered
        if current == stop_path or rendered == stop:
            return
        parent = current.parent
        if parent == current:
            return
        current = parent


def _command_target(module_root: str, package_dir: str) -> str:
    if module_root == package_dir:
        return "."
    relative = pathlib.PurePosixPath(package_dir).relative_to(pathlib.PurePosixPath(module_root)) if module_root != "." else pathlib.PurePosixPath(package_dir)
    return f"./{relative}"


def _test_bindings(
    tree_paths: Sequence[str],
    changed_go_tests: Sequence[str],
    fixtures: Sequence[str],
    modules: Sequence[dict[str, Any]],
) -> tuple[list[dict[str, Any]], list[dict[str, Any]], list[str]]:
    tree_go_tests = sorted(path for path in tree_paths if path.endswith("_test.go"))
    tests_by_directory: dict[str, list[str]] = {}
    for path in tree_go_tests:
        tests_by_directory.setdefault(str(pathlib.PurePosixPath(path).parent), []).append(path)
    targets: dict[tuple[str, str], dict[str, Any]] = {}

    def add_target(module: dict[str, Any], package_dir: str, evidence: str, evidence_path: str) -> None:
        key = (module["root"], package_dir)
        if key not in targets:
            owner_tests = tests_by_directory.get(package_dir, [])
            targets[key] = {
                "evidence_sources": [],
                "module_root": module["root"],
                "module_directive_sha256": module["module_directive_sha256"],
                "owner_go_test_file_count": len(owner_tests),
                "owner_go_test_paths_sha256": _canonical_hash(owner_tests),
                "package_dir_sha256": _sha256(package_dir.encode("utf-8")),
                "target_sha256": _sha256(_command_target(module["root"], package_dir).encode("utf-8")),
                "bound_evidence_paths": [],
            }
        record = targets[key]
        if evidence not in record["evidence_sources"]:
            record["evidence_sources"].append(evidence)
        record["bound_evidence_paths"].append(evidence_path)

    unresolved: list[str] = []
    fixture_owners: list[dict[str, Any]] = []
    for path in changed_go_tests:
        module = _module_for_path(path, modules)
        if module is None:
            unresolved.append(path)
            continue
        add_target(module, str(pathlib.PurePosixPath(path).parent), "changed_go_test", path)
    for path in fixtures:
        module = _module_for_path(path, modules)
        owner: str | None = None
        if module is not None:
            for directory in _ancestors(str(pathlib.PurePosixPath(path).parent), module["root"]):
                if directory in tests_by_directory:
                    owner = directory
                    break
        if module is None or owner is None:
            unresolved.append(path)
            continue
        add_target(module, owner, "fixture_or_helper_owner", path)
        fixture_owners.append({
            "fixture_or_helper_path_sha256": _sha256(path.encode("utf-8")),
            "module_root": module["root"],
            "owner_package_dir_sha256": _sha256(owner.encode("utf-8")),
            "owner_test_paths_sha256": _canonical_hash(tests_by_directory[owner]),
        })
    rendered_targets: list[dict[str, Any]] = []
    for key in sorted(targets):
        record = targets[key]
        record["evidence_sources"].sort()
        record["bound_evidence_paths"].sort()
        record["bound_evidence_paths_sha256"] = _canonical_hash(record.pop("bound_evidence_paths"))
        rendered_targets.append(record)
    rendered_targets.sort(key=lambda item: (item["module_root"], item["package_dir_sha256"]))
    return rendered_targets, sorted(fixture_owners, key=lambda item: item["fixture_or_helper_path_sha256"]), sorted(unresolved)


def _source_lineage(git_binary: pathlib.Path, repo: pathlib.Path, commit: str, parents: Sequence[str]) -> tuple[str, list[str]]:
    _require(len(parents) in {1, 2}, "integration unit must have one or two parents")
    if len(parents) == 1:
        return "single_parent_integration_unit", [commit]
    lineage = [line for line in _git_text(git_binary, repo, ["rev-list", "--reverse", f"{parents[0]}..{parents[1]}"]).splitlines() if line]
    _require(bool(lineage), "two-parent unit has empty feature lineage")
    _require(len(lineage) == len(set(lineage)), "feature lineage contains duplicate commits")
    return "two_parent_feature_branch", lineage


def scan_repository(
    repo: pathlib.Path,
    *,
    git_binary: pathlib.Path,
    repository: dict[str, Any],
    repository_contract_sha256: str,
    scanner_sha256: str,
    schema_sha256: str,
    verified_toolchain: dict[str, Any],
) -> dict[str, Any]:
    repo = repo.resolve()
    git_binary = git_binary.resolve()
    _require(repo.is_dir(), "repository path is not a directory")
    _validate_repository_entry(repository)
    verified_git = _verify_git_binary(git_binary, repository["toolchain"]["git"])
    for label, value in (
        ("repository contract", repository_contract_sha256),
        ("scanner", scanner_sha256),
        ("schema", schema_sha256),
    ):
        _require(SHA256_RE.fullmatch(value) is not None and value != "0" * 64, f"{label} SHA-256 is invalid")
    _require(
        verified_toolchain == {
            "git": verified_git,
            "go": repository["toolchain"]["go"],
            "native": repository["toolchain"]["native"],
            "verification_status": "matched_local_binaries_without_candidate_execution",
        },
        "verified toolchain binding differs",
    )
    units = _verify_repository_binding(git_binary, repo, repository)
    candidates: list[dict[str, Any]] = []
    bands: Counter[str] = Counter()
    kinds: Counter[str] = Counter()
    evidence_kinds: Counter[str] = Counter()
    ready_count = 0
    repository_id = repository["repository_id"]
    for position, commit in enumerate(units, 1):
        parents = _git_text(git_binary, repo, ["show", "-s", "--format=%P", commit]).split()
        _require(bool(parents) and parents[0] == (units[position - 2] if position > 1 else repository["window"]["base_oid"]), "first-parent chain differs")
        _require(len(parents) in {1, 2}, "integration unit must have one or two parents")
        parent = parents[0]
        changed_paths = _changed_paths(git_binary, repo, parent, commit)
        production_go = sorted(path for path in changed_paths if _is_production_go_path(path))
        test_evidence = sorted(path for path in changed_paths if _is_test_path(path))
        _require(not (set(production_go) & set(test_evidence)), "production and test evidence paths overlap")
        if not production_go or not test_evidence:
            continue
        changed_go_tests = sorted(path for path in test_evidence if path.endswith("_test.go"))
        fixtures = sorted(path for path in test_evidence if not path.endswith("_test.go"))
        if changed_go_tests and fixtures:
            evidence_kind = "changed_go_test_and_fixture_or_helper"
        elif changed_go_tests:
            evidence_kind = "changed_go_test_only"
        else:
            evidence_kind = "fixture_or_helper_only"
        tree_paths = _tree_paths(git_binary, repo, commit)
        modules, workspaces = _manifests(git_binary, repo, commit, tree_paths)
        targets, fixture_owners, unresolved = _test_bindings(tree_paths, changed_go_tests, fixtures, modules)
        relevant_modules: list[dict[str, Any]] = []
        relevant_roots = {
            module["root"]
            for path in production_go
            for module in [_module_for_path(path, modules)]
            if module is not None
        } | {target["module_root"] for target in targets}
        relevant_modules = [module for module in modules if module["root"] in relevant_roots]
        unresolved_source_modules = sorted(path for path in production_go if _module_for_path(path, modules) is None)
        runner_ready = bool(targets) and not unresolved and not unresolved_source_modules
        source_packages = sorted({str(pathlib.PurePosixPath(path).parent) for path in production_go})
        score = static_scope_score(len(production_go), len(test_evidence), len(source_packages))
        band = static_scope_band(score)
        unit_kind, lineage = _source_lineage(git_binary, repo, commit, parents)
        source_patch = _diff(git_binary, repo, parent, commit, production_go)
        test_patch = _diff(git_binary, repo, parent, commit, test_evidence)
        full_patch = _diff(git_binary, repo, parent, commit, changed_paths)
        candidate = {
            "candidate_ref": _candidate_ref(repository_id, commit),
            "changed_path_count": len(changed_paths),
            "changed_paths_sha256": _canonical_hash(changed_paths),
            "changed_go_test_file_count": len(changed_go_tests),
            "commit_oid": commit,
            "committed_at": _git_text(git_binary, repo, ["show", "-s", "--format=%cI", commit]).strip(),
            "evidence_kind": evidence_kind,
            "first_parent_position": position,
            "fixture_owner_bindings": fixture_owners,
            "full_diff_sha256": _sha256(full_patch),
            "full_stable_patch_id": _stable_patch_id(git_binary, repo, full_patch),
            "go_work_bindings": workspaces,
            "merge_parent_count": len(parents),
            "module_bindings": relevant_modules,
            "negative_control_status": EXECUTION_STATUS,
            "parent_oid": parent,
            "path_partition_status": "verified_disjoint_production_go_and_test_evidence",
            "production_go_paths_sha256": _canonical_hash(production_go),
            "runner_readiness": "structurally_ready_not_executed" if runner_ready else "blocked_unresolved_static_binding",
            "source_diff_sha256": _sha256(source_patch),
            "source_file_count": len(production_go),
            "source_lineage_commit_oids": lineage,
            "source_lineage_sha256": _canonical_hash(lineage),
            "source_package_count": len(source_packages),
            "source_session_status": SESSION_STATUS,
            "source_stable_patch_id": _stable_patch_id(git_binary, repo, source_patch),
            "static_scope_band": band,
            "static_scope_score": score,
            "test_diff_sha256": _sha256(test_patch),
            "test_evidence_paths_sha256": _canonical_hash(test_evidence),
            "test_file_count": len(test_evidence),
            "fixture_or_helper_file_count": len(fixtures),
            "test_stable_patch_id": _stable_patch_id(git_binary, repo, test_patch),
            "test_targets": targets,
            "tree_oid": _git_text(git_binary, repo, ["rev-parse", f"{commit}^{{tree}}" ]).strip(),
            "unit_kind": unit_kind,
            "unresolved_source_module_count": len(unresolved_source_modules),
            "unresolved_source_module_paths_sha256": _canonical_hash(unresolved_source_modules),
            "unresolved_test_binding_count": len(unresolved),
            "unresolved_test_binding_paths_sha256": _canonical_hash(unresolved),
        }
        candidates.append(candidate)
        bands[band] += 1
        kinds[unit_kind] += 1
        evidence_kinds[evidence_kind] += 1
        ready_count += int(runner_ready)
    authority = {
        "benchmark_execution": "forbidden_not_executed",
        "calibration_membership": "forbidden_permanent_development_exposure",
        "confirmatory_holdout_membership": "forbidden_permanent_development_exposure",
        "owner_key": "absent_not_fabricated",
        "population_assignment": "absent_not_authorized",
        "source_session_identity": SESSION_STATUS,
        "status": AUTHORITY_STATUS,
    }
    result = {
        "authority": authority,
        "candidates": candidates,
        "execution_status": EXECUTION_STATUS,
        "exposure": EXPOSURE,
        "generated_at": _git_text(git_binary, repo, ["show", "-s", "--format=%cI", repository["window"]["head_oid"]]).strip(),
        "implementation": {
            "repository_contract_sha256": repository_contract_sha256,
            "scanner_sha256": scanner_sha256,
            "schema_sha256": schema_sha256,
        },
        "ledger_sha256": None,
        "profile": PROFILE,
        "repository": copy.deepcopy(repository),
        "schema_version": 2,
        "selection_rule": SELECTION_RULE,
        "summary": {
            "candidate_count": len(candidates),
            "evidence_kind_counts": {
                "changed_go_test_and_fixture_or_helper": evidence_kinds["changed_go_test_and_fixture_or_helper"],
                "changed_go_test_only": evidence_kinds["changed_go_test_only"],
                "fixture_or_helper_only": evidence_kinds["fixture_or_helper_only"],
            },
            "first_parent_unit_count": len(units),
            "runner_ready_count": ready_count,
            "static_scope_band_counts": {"high": bands["high"], "low": bands["low"], "medium": bands["medium"]},
            "unit_kind_counts": {
                "single_parent_integration_unit": kinds["single_parent_integration_unit"],
                "two_parent_feature_branch": kinds["two_parent_feature_branch"],
            },
        },
        "toolchain_binding": copy.deepcopy(verified_toolchain),
    }
    result["ledger_sha256"] = _self_hash(result)
    validate_ledger(result)
    return result


def _validate_repo_path(value: Any, field: str) -> str:
    _require(isinstance(value, str) and bool(value) and "\n" not in value and "\r" not in value, f"{field} is invalid")
    path = pathlib.PurePosixPath(value)
    _require(not path.is_absolute() and ".." not in path.parts and str(path) == value, f"{field} is not a canonical repository path")
    return value


def _validate_manifest_binding(value: Any, field: str, *, expected_kind: str) -> None:
    expected_fields = {
        "blob_oid", "go_directive", "kind", "module_directive_sha256", "path", "raw_sha256", "root", "toolchain_directive",
    }
    _require(isinstance(value, dict) and set(value) == expected_fields, f"{field} fields differ")
    _require(value["kind"] == expected_kind, f"{field}.kind differs")
    path = _validate_repo_path(value["path"], f"{field}.path")
    _require(pathlib.PurePosixPath(path).name == expected_kind, f"{field}.path kind differs")
    expected_root = str(pathlib.PurePosixPath(path).parent)
    _require(value["root"] == expected_root, f"{field}.root differs from path")
    for hash_field, pattern, length in (("blob_oid", OID_RE, 40), ("raw_sha256", SHA256_RE, 64)):
        item = value[hash_field]
        _require(isinstance(item, str) and pattern.fullmatch(item) is not None and item != "0" * length, f"{field}.{hash_field} is invalid")
    for optional in ("go_directive", "toolchain_directive"):
        item = value[optional]
        _require(item is None or (isinstance(item, str) and bool(item)), f"{field}.{optional} is invalid")
    module_hash = value["module_directive_sha256"]
    if expected_kind == "go.mod":
        _require(isinstance(module_hash, str) and SHA256_RE.fullmatch(module_hash) is not None and module_hash != "0" * 64, f"{field}.module_directive_sha256 is invalid")
    else:
        _require(module_hash is None, f"{field}.module_directive_sha256 must be null")


def _validate_test_target(value: Any, field: str, modules_by_root: dict[str, dict[str, Any]]) -> None:
    expected_fields = {
        "bound_evidence_paths_sha256", "evidence_sources", "module_directive_sha256", "module_root",
        "owner_go_test_file_count", "owner_go_test_paths_sha256", "package_dir_sha256", "target_sha256",
    }
    _require(isinstance(value, dict) and set(value) == expected_fields, f"{field} fields differ")
    root = value["module_root"]
    _require(isinstance(root, str) and root in modules_by_root, f"{field}.module_root is unresolved")
    _require(value["module_directive_sha256"] == modules_by_root[root]["module_directive_sha256"], f"{field} module directive differs")
    sources = value["evidence_sources"]
    _require(
        isinstance(sources, list)
        and sources == sorted(set(sources))
        and bool(sources)
        and set(sources) <= {"changed_go_test", "fixture_or_helper_owner"},
        f"{field}.evidence_sources are invalid",
    )
    _require(type(value["owner_go_test_file_count"]) is int and value["owner_go_test_file_count"] >= 0, f"{field}.owner_go_test_file_count is invalid")
    if "fixture_or_helper_owner" in sources:
        _require(value["owner_go_test_file_count"] > 0, f"{field} fixture owner has no candidate-tree Go tests")
    for hash_field in ("bound_evidence_paths_sha256", "module_directive_sha256", "owner_go_test_paths_sha256", "package_dir_sha256", "target_sha256"):
        item = value[hash_field]
        _require(isinstance(item, str) and SHA256_RE.fullmatch(item) is not None and item != "0" * 64, f"{field}.{hash_field} is invalid")


def _validate_fixture_owner(value: Any, field: str, modules_by_root: dict[str, dict[str, Any]], target_keys: set[tuple[str, str]]) -> None:
    expected_fields = {"fixture_or_helper_path_sha256", "module_root", "owner_package_dir_sha256", "owner_test_paths_sha256"}
    _require(isinstance(value, dict) and set(value) == expected_fields, f"{field} fields differ")
    root = value["module_root"]
    _require(isinstance(root, str) and root in modules_by_root, f"{field}.module_root is unresolved")
    for hash_field in ("fixture_or_helper_path_sha256", "owner_package_dir_sha256", "owner_test_paths_sha256"):
        item = value[hash_field]
        _require(isinstance(item, str) and SHA256_RE.fullmatch(item) is not None and item != "0" * 64, f"{field}.{hash_field} is invalid")
    _require((root, value["owner_package_dir_sha256"]) in target_keys, f"{field} has no matching test target")


def validate_ledger(value: dict[str, Any]) -> None:
    expected_root = {
        "authority", "candidates", "execution_status", "exposure", "generated_at", "implementation",
        "ledger_sha256", "profile", "repository", "schema_version", "selection_rule", "summary", "toolchain_binding",
    }
    _require(isinstance(value, dict) and set(value) == expected_root, "v2 ledger root fields differ")
    _require(value["schema_version"] == 2 and value["profile"] == PROFILE, "v2 profile differs")
    _require(value["selection_rule"] == SELECTION_RULE and value["exposure"] == EXPOSURE, "v2 scope differs")
    _require(value["execution_status"] == EXECUTION_STATUS, "candidate execution must remain not_executed")
    _validate_timestamp(value["generated_at"], "generated_at")
    repository = _validate_repository_entry(value["repository"])
    expected_authority = {
        "benchmark_execution": "forbidden_not_executed",
        "calibration_membership": "forbidden_permanent_development_exposure",
        "confirmatory_holdout_membership": "forbidden_permanent_development_exposure",
        "owner_key": "absent_not_fabricated",
        "population_assignment": "absent_not_authorized",
        "source_session_identity": SESSION_STATUS,
        "status": AUTHORITY_STATUS,
    }
    _require(value["authority"] == expected_authority, "v2 authority boundary differs")
    implementation = value["implementation"]
    _require(isinstance(implementation, dict) and set(implementation) == {"repository_contract_sha256", "scanner_sha256", "schema_sha256"}, "implementation binding fields differ")
    for field, item in implementation.items():
        _require(isinstance(item, str) and SHA256_RE.fullmatch(item) is not None and item != "0" * 64, f"implementation.{field} is invalid")
    _require(value["toolchain_binding"] == {"git": repository["toolchain"]["git"], "go": repository["toolchain"]["go"], "native": repository["toolchain"]["native"], "verification_status": "matched_local_binaries_without_candidate_execution"}, "toolchain binding differs")
    recorded = value["ledger_sha256"]
    _require(isinstance(recorded, str) and SHA256_RE.fullmatch(recorded) is not None and recorded != "0" * 64, "ledger SHA-256 is invalid")
    _require(recorded == _self_hash(value), "v2 ledger self hash mismatch")
    candidates = value["candidates"]
    _require(isinstance(candidates, list), "candidates must be a list")
    positions: list[int] = []
    refs: set[str] = set()
    commits: set[str] = set()
    bands: Counter[str] = Counter()
    kinds: Counter[str] = Counter()
    evidence: Counter[str] = Counter()
    ready = 0
    expected_candidate_fields = {
        "candidate_ref", "changed_path_count", "changed_paths_sha256", "changed_go_test_file_count", "commit_oid", "committed_at", "evidence_kind",
        "first_parent_position", "fixture_owner_bindings", "full_diff_sha256", "full_stable_patch_id", "go_work_bindings",
        "merge_parent_count", "module_bindings", "negative_control_status", "parent_oid", "path_partition_status",
        "production_go_paths_sha256", "runner_readiness", "source_diff_sha256", "source_file_count",
        "source_lineage_commit_oids", "source_lineage_sha256", "source_package_count", "source_session_status",
        "source_stable_patch_id", "static_scope_band", "static_scope_score", "test_diff_sha256", "fixture_or_helper_file_count",
        "test_evidence_paths_sha256", "test_file_count", "test_stable_patch_id", "test_targets", "tree_oid", "unit_kind",
        "unresolved_source_module_count", "unresolved_source_module_paths_sha256", "unresolved_test_binding_count",
        "unresolved_test_binding_paths_sha256",
    }
    for index, candidate in enumerate(candidates):
        _require(isinstance(candidate, dict) and set(candidate) == expected_candidate_fields, f"candidate[{index}] fields differ")
        position = candidate["first_parent_position"]
        _require(type(position) is int and 1 <= position <= 31, f"candidate[{index}] position is invalid")
        positions.append(position)
        for field in ("commit_oid", "parent_oid", "tree_oid"):
            item = candidate[field]
            _require(isinstance(item, str) and OID_RE.fullmatch(item) is not None and item != "0" * 40, f"candidate[{index}].{field} is invalid")
        _require(candidate["commit_oid"] not in commits, "duplicate candidate commit")
        commits.add(candidate["commit_oid"])
        _require(candidate["candidate_ref"] == _candidate_ref(repository["repository_id"], candidate["commit_oid"]), f"candidate[{index}] reference differs")
        _require(candidate["candidate_ref"] not in refs, "duplicate candidate reference")
        refs.add(candidate["candidate_ref"])
        _validate_timestamp(candidate["committed_at"], f"candidate[{index}].committed_at")
        for field in ("changed_path_count", "changed_go_test_file_count", "source_file_count", "source_package_count", "test_file_count", "fixture_or_helper_file_count", "unresolved_source_module_count", "unresolved_test_binding_count"):
            _require(type(candidate[field]) is int and candidate[field] >= 0, f"candidate[{index}].{field} is invalid")
        _require(candidate["source_file_count"] > 0 and candidate["source_package_count"] > 0 and candidate["test_file_count"] > 0, f"candidate[{index}] lacks source or test evidence")
        _require(candidate["changed_go_test_file_count"] + candidate["fixture_or_helper_file_count"] == candidate["test_file_count"], f"candidate[{index}] evidence counts differ")
        _require(candidate["source_file_count"] + candidate["test_file_count"] <= candidate["changed_path_count"], f"candidate[{index}] changed-path count is too small")
        _require(candidate["path_partition_status"] == "verified_disjoint_production_go_and_test_evidence", f"candidate[{index}] production/test partition differs")
        for field in ("changed_paths_sha256", "production_go_paths_sha256", "test_evidence_paths_sha256", "unresolved_source_module_paths_sha256", "unresolved_test_binding_paths_sha256"):
            item = candidate[field]
            _require(isinstance(item, str) and SHA256_RE.fullmatch(item) is not None, f"candidate[{index}].{field} is invalid")
        go_test_count = candidate["changed_go_test_file_count"]
        fixture_count = candidate["fixture_or_helper_file_count"]
        expected_kind = "changed_go_test_and_fixture_or_helper" if go_test_count and fixture_count else "changed_go_test_only" if go_test_count else "fixture_or_helper_only"
        _require(candidate["evidence_kind"] == expected_kind, f"candidate[{index}] evidence kind differs")
        score = static_scope_score(candidate["source_file_count"], candidate["test_file_count"], candidate["source_package_count"])
        _require(candidate["static_scope_score"] == score and candidate["static_scope_band"] == static_scope_band(score), f"candidate[{index}] static scope differs")
        for field in ("source_diff_sha256", "test_diff_sha256", "full_diff_sha256"):
            item = candidate[field]
            _require(isinstance(item, str) and SHA256_RE.fullmatch(item) is not None and item != "0" * 64, f"candidate[{index}].{field} is invalid")
        for field in ("source_stable_patch_id", "test_stable_patch_id", "full_stable_patch_id"):
            item = candidate[field]
            _require(isinstance(item, str) and OID_RE.fullmatch(item) is not None and item != "0" * 40, f"candidate[{index}].{field} is invalid")
        lineage = candidate["source_lineage_commit_oids"]
        _require(isinstance(lineage, list) and bool(lineage) and len(lineage) == len(set(lineage)), f"candidate[{index}] lineage is invalid")
        _require(all(isinstance(item, str) and OID_RE.fullmatch(item) is not None for item in lineage), f"candidate[{index}] lineage OID is invalid")
        _require(candidate["source_lineage_sha256"] == _canonical_hash(lineage), f"candidate[{index}] lineage hash differs")
        expected_unit_kind = "single_parent_integration_unit" if candidate["merge_parent_count"] == 1 else "two_parent_feature_branch"
        _require(type(candidate["merge_parent_count"]) is int and candidate["merge_parent_count"] in {1, 2} and candidate["unit_kind"] == expected_unit_kind, f"candidate[{index}] unit kind differs")
        if candidate["merge_parent_count"] == 1:
            _require(lineage == [candidate["commit_oid"]], f"candidate[{index}] single-parent lineage differs")
        _require(candidate["negative_control_status"] == EXECUTION_STATUS, f"candidate[{index}] negative control was executed")
        _require(candidate["source_session_status"] == SESSION_STATUS, f"candidate[{index}] source session was overclaimed")
        modules = candidate["module_bindings"]
        workspaces = candidate["go_work_bindings"]
        _require(isinstance(modules, list) and modules == sorted(modules, key=lambda item: item["path"]), f"candidate[{index}] module bindings are not canonical")
        _require(isinstance(workspaces, list) and workspaces == sorted(workspaces, key=lambda item: item["path"]), f"candidate[{index}] workspace bindings are not canonical")
        for manifest_index, manifest in enumerate(modules):
            _validate_manifest_binding(manifest, f"candidate[{index}].module_bindings[{manifest_index}]", expected_kind="go.mod")
        for manifest_index, manifest in enumerate(workspaces):
            _validate_manifest_binding(manifest, f"candidate[{index}].go_work_bindings[{manifest_index}]", expected_kind="go.work")
        module_roots = [manifest["root"] for manifest in modules]
        _require(len(module_roots) == len(set(module_roots)), f"candidate[{index}] module roots are duplicated")
        workspace_paths = [manifest["path"] for manifest in workspaces]
        _require(len(workspace_paths) == len(set(workspace_paths)), f"candidate[{index}] workspace paths are duplicated")
        modules_by_root = {manifest["root"]: manifest for manifest in modules}
        targets = candidate["test_targets"]
        _require(isinstance(targets, list) and targets == sorted(targets, key=lambda item: (item["module_root"], item["package_dir_sha256"])), f"candidate[{index}] test targets are not canonical")
        for target_index, target in enumerate(targets):
            _validate_test_target(target, f"candidate[{index}].test_targets[{target_index}]", modules_by_root)
        target_keys = {(target["module_root"], target["package_dir_sha256"]) for target in targets}
        _require(len(target_keys) == len(targets), f"candidate[{index}] test targets are duplicated")
        fixtures = candidate["fixture_owner_bindings"]
        _require(isinstance(fixtures, list) and fixtures == sorted(fixtures, key=lambda item: item["fixture_or_helper_path_sha256"]), f"candidate[{index}] fixture owners are not canonical")
        for fixture_index, fixture in enumerate(fixtures):
            _validate_fixture_owner(fixture, f"candidate[{index}].fixture_owner_bindings[{fixture_index}]", modules_by_root, target_keys)
        fixture_hashes = [fixture["fixture_or_helper_path_sha256"] for fixture in fixtures]
        _require(len(fixture_hashes) == len(set(fixture_hashes)), f"candidate[{index}] fixture owners are duplicated")
        _require(len(fixtures) <= candidate["fixture_or_helper_file_count"], f"candidate[{index}] fixture owner count exceeds evidence count")
        if candidate["unresolved_test_binding_count"] == 0:
            _require(candidate["unresolved_test_binding_paths_sha256"] == _canonical_hash([]), f"candidate[{index}] unresolved test hash differs")
            _require(len(fixtures) == candidate["fixture_or_helper_file_count"], f"candidate[{index}] fixture owner count differs")
        if candidate["unresolved_source_module_count"] == 0:
            _require(candidate["unresolved_source_module_paths_sha256"] == _canonical_hash([]), f"candidate[{index}] unresolved source-module hash differs")
        if candidate["changed_go_test_file_count"] > 0 and candidate["unresolved_test_binding_count"] == 0:
            _require(any("changed_go_test" in target["evidence_sources"] for target in targets), f"candidate[{index}] lacks a changed-test target")
        if fixtures:
            _require(any("fixture_or_helper_owner" in target["evidence_sources"] for target in targets), f"candidate[{index}] lacks a fixture-owner target")
        expected_readiness = "structurally_ready_not_executed" if targets and candidate["unresolved_test_binding_count"] == 0 and candidate["unresolved_source_module_count"] == 0 else "blocked_unresolved_static_binding"
        _require(candidate["runner_readiness"] == expected_readiness, f"candidate[{index}] readiness differs")
        ready += int(expected_readiness == "structurally_ready_not_executed")
        bands[candidate["static_scope_band"]] += 1
        kinds[candidate["unit_kind"]] += 1
        evidence[candidate["evidence_kind"]] += 1
    _require(positions == sorted(set(positions)), "candidate positions are not canonical")
    summary = value["summary"]
    expected_summary = {
        "candidate_count": len(candidates),
        "evidence_kind_counts": {
            "changed_go_test_and_fixture_or_helper": evidence["changed_go_test_and_fixture_or_helper"],
            "changed_go_test_only": evidence["changed_go_test_only"],
            "fixture_or_helper_only": evidence["fixture_or_helper_only"],
        },
        "first_parent_unit_count": 31,
        "runner_ready_count": ready,
        "static_scope_band_counts": {"high": bands["high"], "low": bands["low"], "medium": bands["medium"]},
        "unit_kind_counts": {
            "single_parent_integration_unit": kinds["single_parent_integration_unit"],
            "two_parent_feature_branch": kinds["two_parent_feature_branch"],
        },
    }
    _require(summary == expected_summary, "v2 summary differs")


def verify_ledger_against_repository(repo: pathlib.Path, value: dict[str, Any], *, git_binary: pathlib.Path) -> None:
    rebuilt = scan_repository(
        repo.resolve(),
        git_binary=git_binary.resolve(),
        repository=value["repository"],
        repository_contract_sha256=value["implementation"]["repository_contract_sha256"],
        scanner_sha256=value["implementation"]["scanner_sha256"],
        schema_sha256=value["implementation"]["schema_sha256"],
        verified_toolchain=value["toolchain_binding"],
    )
    _require(rebuilt == value, "repository-backed full ledger rebuild differs")


def verify_ledger_dependencies(value: dict[str, Any], *, contract: pathlib.Path, schema: pathlib.Path) -> None:
    validate_ledger(value)
    try:
        scanner_hash = _sha256(pathlib.Path(__file__).read_bytes())
        schema_hash = _sha256(schema.read_bytes())
    except OSError as exc:
        raise EligibilityV2Error(f"cannot read implementation dependency: {exc}") from exc
    repository, contract_hash = load_repository_contract(contract, value["repository"]["key"])
    _require(scanner_hash == value["implementation"]["scanner_sha256"], "scanner dependency hash differs")
    _require(schema_hash == value["implementation"]["schema_sha256"], "schema dependency hash differs")
    _require(contract_hash == value["implementation"]["repository_contract_sha256"], "repository contract dependency hash differs")
    _require(repository == value["repository"], "repository contract entry differs")


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
    scan = subparsers.add_parser("scan", help="build one repository-bound v2 ledger")
    scan.add_argument("--repo", required=True, type=pathlib.Path)
    scan.add_argument("--contract", required=True, type=pathlib.Path)
    scan.add_argument("--repository-key", required=True)
    scan.add_argument("--schema", required=True, type=pathlib.Path)
    scan.add_argument("--git-binary", required=True, type=pathlib.Path)
    scan.add_argument("--go-binary", required=True, type=pathlib.Path)
    scan.add_argument("--native-binary", required=True, type=pathlib.Path)
    scan.add_argument("--native-cxx-binary", required=True, type=pathlib.Path)
    scan.add_argument("--output", required=True, type=pathlib.Path)
    check = subparsers.add_parser("check", help="validate a ledger and optionally its Git bindings")
    check.add_argument("ledger", type=pathlib.Path)
    check.add_argument("--repo", type=pathlib.Path)
    check.add_argument("--contract", type=pathlib.Path)
    check.add_argument("--schema", type=pathlib.Path)
    check.add_argument("--git-binary", type=pathlib.Path)
    args = parser.parse_args(argv)
    try:
        if args.command == "scan":
            repository, contract_hash = load_repository_contract(args.contract, args.repository_key)
            toolchain = _verify_toolchain(
                args.git_binary.resolve(),
                args.go_binary.resolve(),
                args.native_binary.resolve(),
                args.native_cxx_binary.resolve(),
                repository["toolchain"],
            )
            ledger = scan_repository(
                args.repo,
                git_binary=args.git_binary.resolve(),
                repository=repository,
                repository_contract_sha256=contract_hash,
                scanner_sha256=_sha256(pathlib.Path(__file__).read_bytes()),
                schema_sha256=_sha256(args.schema.read_bytes()),
                verified_toolchain=toolchain,
            )
            _write_atomic(args.output, ledger)
        else:
            ledger, _ = _load_json(args.ledger)
            validate_ledger(ledger)
            _require((args.contract is None) is (args.schema is None), "--contract and --schema must be supplied together")
            if args.contract is not None:
                verify_ledger_dependencies(ledger, contract=args.contract, schema=args.schema)
            _require((args.repo is None) is (args.git_binary is None), "--repo and --git-binary must be supplied together")
            if args.repo is not None:
                verify_ledger_against_repository(args.repo, ledger, git_binary=args.git_binary)
    except (EligibilityV2Error, OSError) as exc:
        parser.error(str(exc))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
