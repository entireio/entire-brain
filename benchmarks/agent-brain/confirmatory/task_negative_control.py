#!/usr/bin/env python3
"""Run development-only reverse-patch task negative controls.

For each candidate from ``task_eligibility.py``, this runner checks that the
candidate's changed test packages pass at the candidate tree, reverses only the
production Go patch relative to its first parent, and reruns the exact same
test command.  Receipts contain hashes, counts, and classifications, never
paths, test output, commit subjects, prompts, or patches.

This is a local development filter.  It does not approve symptom wording,
assign population splits, inspect retrieval, open a holdout, or authorize a
model/provider run.
"""

from __future__ import annotations

import argparse
import copy
import datetime as dt
import functools
import hashlib
import json
import os
import pathlib
import platform
import shutil
import signal
import subprocess
import sys
import tempfile
from collections import Counter
from typing import Any, Callable, Sequence

import task_eligibility
import task_population


PROFILE = "agent_brain_development_task_negative_control_v1"
STATUS = "development_filter_only"
EXPOSURE = task_eligibility.EXPOSURE
COMMAND_POLICY = "changed_go_test_packages_reverse_production_go_v1"
REVERSAL_POLICY = "first_parent_production_go_only_no_renames_v1"
TEST_FLAGS = ("-count=1", "-timeout=0")
GO_TOOLCHAIN = "go1.26.4"
GO_ENV_KEYS = (
    "GOARCH",
    "GOOS",
    "GOVERSION",
    "GOROOT",
    "GOTOOLDIR",
    "GOWORK",
    "GOFLAGS",
    "GOTOOLCHAIN",
    "GOENV",
    "GOPROXY",
    "GOSUMDB",
    "GOVCS",
    "CGO_ENABLED",
    "CC",
    "CXX",
    "GOEXPERIMENT",
    "GOCACHE",
    "GOMODCACHE",
    "GOPATH",
)
SHA256_RE = task_eligibility.SHA256_RE
OID_RE = task_eligibility.OID_RE
CLASSIFICATIONS = (
    "eligible_for_symptom_review",
    "negative_control_survived",
    "baseline_invalid_failure",
    "baseline_invalid_timeout",
    "reversed_invalid_timeout",
)


class NegativeControlError(ValueError):
    """Raised when a negative-control run cannot produce trustworthy evidence."""


def _require(condition: bool, message: str) -> None:
    if not condition:
        raise NegativeControlError(message)


def _sha256(value: bytes) -> str:
    return hashlib.sha256(value).hexdigest()


def _canonical_hash(value: Any) -> str:
    return _sha256(task_population.canonical_json_bytes(value))


def _self_hash(value: dict[str, Any]) -> str:
    projected = copy.deepcopy(value)
    _require("receipt_sha256" in projected, "negative-control receipt is missing receipt_sha256")
    projected["receipt_sha256"] = None
    return _canonical_hash(projected)


def _load_json(path: pathlib.Path) -> tuple[dict[str, Any], bytes]:
    try:
        raw = path.read_bytes()
        value = json.loads(raw, object_pairs_hook=_reject_duplicate_pairs)
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        raise NegativeControlError(f"cannot load JSON: {exc}") from exc
    _require(isinstance(value, dict), "JSON root must be an object")
    return value, raw


def _reject_duplicate_pairs(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise NegativeControlError("JSON contains a duplicate object key")
        result[key] = value
    return result


def _run(
    args: Sequence[str],
    *,
    cwd: pathlib.Path,
    timeout: float | None = None,
    input_bytes: bytes | None = None,
    combined_output: bool = False,
    env: dict[str, str] | None = None,
) -> subprocess.CompletedProcess[bytes]:
    try:
        return subprocess.run(
            list(args),
            cwd=cwd,
            input=input_bytes,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT if combined_output else subprocess.PIPE,
            check=False,
            timeout=timeout,
            env=env,
        )
    except (OSError, subprocess.TimeoutExpired) as exc:
        if isinstance(exc, subprocess.TimeoutExpired):
            raise
        raise NegativeControlError(f"cannot execute {args[0]}: {exc}") from exc


def _git(repo: pathlib.Path, args: Sequence[str], *, input_bytes: bytes | None = None) -> bytes:
    environment = dict(os.environ)
    environment["GIT_NO_REPLACE_OBJECTS"] = "1"
    completed = _run(["git", *args], cwd=repo, input_bytes=input_bytes, env=environment)
    if completed.returncode != 0:
        message = completed.stderr.decode("utf-8", errors="replace").strip()
        raise NegativeControlError(f"git command failed: {message or 'unknown error'}")
    return completed.stdout


def _git_text(repo: pathlib.Path, args: Sequence[str]) -> str:
    try:
        return _git(repo, args).decode("utf-8")
    except UnicodeDecodeError as exc:
        raise NegativeControlError("git emitted non-UTF-8 metadata") from exc


def _paths(repo: pathlib.Path, parent: str, commit: str) -> list[str]:
    raw = _git(repo, ["diff", "--name-only", "-z", "--no-renames", parent, commit, "--"])
    try:
        paths = [part.decode("utf-8") for part in raw.split(b"\0") if part]
    except UnicodeDecodeError as exc:
        raise NegativeControlError("changed path is not UTF-8") from exc
    _require(paths == sorted(set(paths)), "changed paths are not canonical")
    return paths


def _diff(repo: pathlib.Path, parent: str, commit: str, paths: list[str]) -> bytes:
    return _git(
        repo,
        ["diff", "--binary", "--no-ext-diff", "--no-renames", parent, commit, "--", *paths],
    )


def _test_targets(test_go_paths: list[str]) -> list[str]:
    directories = sorted({str(pathlib.PurePosixPath(path).parent) for path in test_go_paths})
    return ["." if directory == "." else f"./{directory}" for directory in directories]


@functools.lru_cache(maxsize=1)
def _go_runtime() -> dict[str, str]:
    discovery_environment = {
        "PATH": os.environ.get("PATH", os.defpath),
        "HOME": os.environ.get("HOME", "/var/empty"),
        "LANG": "C",
        "LC_ALL": "C",
        "GOENV": "off",
        "GOPROXY": "off",
        "GOSUMDB": "sum.golang.org",
        "GOTOOLCHAIN": GO_TOOLCHAIN,
    }
    completed = _run(
        ["go", "env", "GOROOT"],
        cwd=pathlib.Path.cwd(),
        env=discovery_environment,
    )
    _require(completed.returncode == 0, f"pinned {GO_TOOLCHAIN} runtime is not locally available")
    try:
        goroot = pathlib.Path(completed.stdout.decode("utf-8").strip()).resolve()
    except UnicodeDecodeError as exc:
        raise NegativeControlError("pinned Go root is not UTF-8") from exc
    binary = goroot / "bin" / "go"
    _require(binary.is_file(), "pinned Go binary is missing")
    runtime_environment = {
        "PATH": f"{binary.parent}{os.pathsep}{os.environ.get('PATH', os.defpath)}",
        "GOROOT": str(goroot),
        "GOTOOLCHAIN": "local",
        "GOENV": "off",
        "LANG": "C",
        "LC_ALL": "C",
    }
    version = _run([str(binary), "version"], cwd=pathlib.Path.cwd(), env=runtime_environment)
    _require(
        version.returncode == 0
        and f"go version {GO_TOOLCHAIN}" in version.stdout.decode("utf-8", errors="replace"),
        "pinned Go binary version differs",
    )
    return {
        "binary": str(binary),
        "binary_sha256": _sha256(binary.read_bytes()),
        "goroot": str(goroot),
    }


@functools.lru_cache(maxsize=1)
def _go_cache_paths() -> dict[str, str]:
    runtime = _go_runtime()
    discovery_environment = {
        "PATH": f"{pathlib.Path(runtime['binary']).parent}{os.pathsep}{os.environ.get('PATH', os.defpath)}",
        "HOME": os.environ.get("HOME", "/var/empty"),
        "GOROOT": runtime["goroot"],
        "LANG": "C",
        "LC_ALL": "C",
        "GOENV": "off",
        "GOTOOLCHAIN": "local",
    }
    completed = _run(
        [runtime["binary"], "env", "-json", "GOPATH", "GOMODCACHE", "GOCACHE"],
        cwd=pathlib.Path.cwd(),
        env=discovery_environment,
    )
    _require(completed.returncode == 0, "cannot discover local Go cache paths")
    try:
        value = json.loads(completed.stdout)
    except (UnicodeError, json.JSONDecodeError) as exc:
        raise NegativeControlError("Go cache path discovery returned invalid JSON") from exc
    expected = {"GOPATH", "GOMODCACHE", "GOCACHE"}
    _require(
        isinstance(value, dict)
        and set(value) == expected
        and all(isinstance(value[key], str) and bool(value[key]) for key in expected),
        "Go cache path discovery fields differ",
    )
    return value


def _telemetry_directories(state_root: pathlib.Path) -> tuple[pathlib.Path, ...]:
    """Every path the pinned toolchain may treat as its telemetry directory.

    The Go toolchain places it at ``os.UserConfigDir()/go/telemetry``, resolved
    from the environment ``_test_environment`` hands it: ``$XDG_CONFIG_HOME``
    on Linux, ``$HOME/.config`` when that is unset, and
    ``$HOME/Library/Application Support`` on macOS.  All of them are inside the
    isolated state root, and writing all of them keeps this independent of the
    host the control runs on.
    """
    home = state_root / "home"
    return (
        state_root / "xdg-config" / "go" / "telemetry",
        home / ".config" / "go" / "telemetry",
        home / "Library" / "Application Support" / "go" / "telemetry",
    )


def _reset_execution_state(state_root: pathlib.Path) -> None:
    shutil.rmtree(state_root, ignore_errors=True)
    _require(not state_root.exists(), "cannot reset isolated execution state")
    for relative in ("home", "tmp", "xdg-cache", "xdg-config", "xdg-data"):
        (state_root / relative).mkdir(parents=True, exist_ok=True)
    # Left at its default ("local"), every `go` invocation opens a telemetry
    # counter file under the isolated state root and fork+execs an upload
    # sidecar that is documented to outlive its parent, so work keeps landing
    # in that directory after the measured command has already exited.  That is
    # unmeasured background activity inside a run whose whole point is to be
    # hermetic -- the same reason GOPROXY, GOVCS and the proxy variables are
    # denied -- and it races whoever deletes the state root next, which is how
    # it first showed up (a caller's teardown failing with ENOTEMPTY).  "off"
    # is the one mode in which no counter file is opened and no sidecar starts.
    for telemetry in _telemetry_directories(state_root):
        telemetry.mkdir(parents=True, exist_ok=True)
        (telemetry / "mode").write_text("off\n", encoding="utf-8")


def _test_environment(state_root: pathlib.Path) -> dict[str, str]:
    caches = _go_cache_paths()
    runtime = _go_runtime()
    return {
        "ALL_PROXY": "",
        "GOCACHE": caches["GOCACHE"],
        "GOENV": "off",
        "GOFLAGS": "",
        "GOROOT": runtime["goroot"],
        "GOMODCACHE": caches["GOMODCACHE"],
        "GOPATH": caches["GOPATH"],
        "GOPROXY": "off",
        "GOSUMDB": "off",
        "GOTOOLCHAIN": "local",
        "GOVCS": "*:off",
        "GOWORK": "off",
        "HOME": str(state_root / "home"),
        "HTTP_PROXY": "",
        "HTTPS_PROXY": "",
        "LANG": "C",
        "LC_ALL": "C",
        "NO_PROXY": "127.0.0.1,localhost,::1",
        "PATH": f"{pathlib.Path(runtime['binary']).parent}{os.pathsep}{os.environ.get('PATH', os.defpath)}",
        "TMPDIR": str(state_root / "tmp"),
        "TZ": "UTC",
        "XDG_CACHE_HOME": str(state_root / "xdg-cache"),
        "XDG_CONFIG_HOME": str(state_root / "xdg-config"),
        "XDG_DATA_HOME": str(state_root / "xdg-data"),
    }


def _environment_manifest_hash(environment: dict[str, str]) -> str:
    projected = dict(environment)
    for field, replacement in (
        ("HOME", "<isolated-reset-home>"),
        ("TMPDIR", "<isolated-reset-tmp>"),
        ("XDG_CACHE_HOME", "<isolated-reset-xdg-cache>"),
        ("XDG_CONFIG_HOME", "<isolated-reset-xdg-config>"),
        ("XDG_DATA_HOME", "<isolated-reset-xdg-data>"),
    ):
        projected[field] = replacement
    return _canonical_hash(projected)


def _execution(
    command: list[str],
    worktree: pathlib.Path,
    timeout_seconds: float,
    environment: dict[str, str],
) -> dict[str, Any]:
    try:
        process = subprocess.Popen(
            command,
            cwd=worktree,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            env=environment,
            start_new_session=True,
        )
    except OSError as exc:
        raise NegativeControlError(f"cannot execute {command[0]}: {exc}") from exc
    try:
        output, _ = process.communicate(timeout=timeout_seconds)
    except subprocess.TimeoutExpired:
        if os.name == "posix":
            os.killpg(process.pid, signal.SIGKILL)
        else:
            process.kill()
        output, _ = process.communicate()
        return {
            "status": "timeout",
            "exit_code": None,
            "output_sha256": _sha256(output),
            "output_byte_count": len(output),
        }
    return {
        "status": "completed",
        "exit_code": process.returncode,
        "output_sha256": _sha256(output),
        "output_byte_count": len(output),
    }


def _classification(baseline: dict[str, Any], reversed_run: dict[str, Any]) -> str:
    if baseline["status"] == "timeout":
        return "baseline_invalid_timeout"
    if baseline["exit_code"] != 0:
        return "baseline_invalid_failure"
    if reversed_run["status"] == "timeout":
        return "reversed_invalid_timeout"
    if reversed_run["exit_code"] == 0:
        return "negative_control_survived"
    return "eligible_for_symptom_review"


def _environment_identity(repo: pathlib.Path, environment: dict[str, str]) -> dict[str, Any]:
    runtime = _go_runtime()
    go_version = _run([runtime["binary"], "version"], cwd=repo, env=environment)
    git_version = _run(["git", "--version"], cwd=repo)
    go_env = _run([runtime["binary"], "env", "-json", *GO_ENV_KEYS], cwd=repo, env=environment)
    for label, completed in (("go version", go_version), ("git version", git_version), ("go env", go_env)):
        _require(completed.returncode == 0, f"{label} failed")
    return {
        "go_version": go_version.stdout.decode("utf-8", errors="strict").strip(),
        "git_version": git_version.stdout.decode("utf-8", errors="strict").strip(),
        "go_binary_sha256": runtime["binary_sha256"],
        "go_env_keys": list(GO_ENV_KEYS),
        "go_env_sha256": _sha256(go_env.stdout),
        "platform": f"{platform.system().lower()}-{platform.machine().lower()}",
        "environment_policy": "deny_all_then_explicit_local_allowlist_v1",
        "environment_manifest_sha256": _environment_manifest_hash(environment),
        "inherited_variable_count": 0,
        "credential_like_variable_count": 0,
        "execution_overrides": {
            "GOENV": environment["GOENV"],
            "GOFLAGS": environment["GOFLAGS"],
            "GOPROXY": environment["GOPROXY"],
            "GOSUMDB": environment["GOSUMDB"],
            "GOTOOLCHAIN": environment["GOTOOLCHAIN"],
            "GOVCS": environment["GOVCS"],
            "GOWORK": environment["GOWORK"],
        },
    }


def _implementation_identity() -> dict[str, Any]:
    files = [
        {"role": "negative_control_runner", "sha256": _sha256(pathlib.Path(__file__).read_bytes())},
        {"role": "eligibility_scanner", "sha256": _sha256(pathlib.Path(task_eligibility.__file__).read_bytes())},
        {"role": "canonical_population_helpers", "sha256": _sha256(pathlib.Path(task_population.__file__).read_bytes())},
    ]
    return {
        "files": files,
        "aggregate_sha256": _canonical_hash(files),
        "python_implementation": platform.python_implementation(),
        "python_version": platform.python_version(),
    }


def _validate_repository_binding(repo: pathlib.Path, ledger: dict[str, Any]) -> list[str]:
    base = ledger["base_oid"]
    head = ledger["head_oid"]
    git_environment = dict(os.environ)
    git_environment["GIT_NO_REPLACE_OBJECTS"] = "1"
    ancestor = _run(
        ["git", "merge-base", "--is-ancestor", base, head],
        cwd=repo,
        env=git_environment,
    )
    _require(ancestor.returncode == 0, "eligibility base is not an ancestor of head")
    units = [line for line in _git_text(repo, ["rev-list", "--first-parent", "--reverse", f"{base}..{head}"]).splitlines() if line]
    _require(len(units) == ledger["summary"]["first_parent_unit_count"], "first-parent unit count differs from ledger")
    for index, candidate in enumerate(ledger["candidates"]):
        position = candidate["first_parent_position"]
        _require(position <= len(units) and units[position - 1] == candidate["commit_oid"], f"candidate[{index}] first-parent position differs")
    return units


def _candidate_inputs(repo: pathlib.Path, candidate: dict[str, Any], index: int) -> tuple[list[str], list[str], list[str]]:
    commit = candidate["commit_oid"]
    parent = candidate["parent_oid"]
    parents = _git_text(repo, ["show", "-s", "--format=%P", commit]).split()
    _require(bool(parents) and parents[0] == parent, f"candidate[{index}] first parent differs")
    _require(len(parents) == candidate["merge_parent_count"], f"candidate[{index}] parent count differs")
    tree = _git_text(repo, ["rev-parse", f"{commit}^{{tree}}"] ).strip()
    _require(tree == candidate["tree_oid"], f"candidate[{index}] tree differs")
    paths = _paths(repo, parent, commit)
    source_paths = sorted(path for path in paths if task_eligibility._is_source_path(path))
    test_paths = sorted(path for path in paths if task_eligibility._is_test_path(path))
    test_go_paths = sorted(path for path in test_paths if path.endswith("_test.go"))
    _require(bool(source_paths) and bool(test_go_paths), f"candidate[{index}] lacks production Go or Go tests")
    bindings = (
        ("changed paths", paths, candidate["changed_paths_sha256"]),
        ("source paths", source_paths, candidate["source_paths_sha256"]),
        ("test paths", test_paths, candidate["test_paths_sha256"]),
    )
    for label, values, expected in bindings:
        _require(_canonical_hash(values) == expected, f"candidate[{index}] {label} commitment differs")
    _require(_sha256(_diff(repo, parent, commit, source_paths)) == candidate["source_diff_sha256"], f"candidate[{index}] source diff differs")
    _require(_sha256(_diff(repo, parent, commit, test_paths)) == candidate["test_diff_sha256"], f"candidate[{index}] test diff differs")
    return source_paths, test_go_paths, _test_targets(test_go_paths)


def _verify_clean_candidate_tree(worktree: pathlib.Path, candidate: dict[str, Any], index: int) -> None:
    head = _git_text(worktree, ["rev-parse", "HEAD"]).strip()
    index_tree = _git_text(worktree, ["write-tree"]).strip()
    status = _git(worktree, ["status", "--porcelain=v1", "-z", "--untracked-files=all"])
    _require(head == candidate["commit_oid"], f"candidate[{index}] worktree HEAD differs")
    _require(index_tree == candidate["tree_oid"], f"candidate[{index}] worktree index tree differs")
    _require(status == b"", f"candidate[{index}] checkout is not clean")


def _prepare_worktree(
    repo: pathlib.Path,
    worktree: pathlib.Path,
    candidate: dict[str, Any],
    index: int,
    environment: dict[str, str],
) -> None:
    _git(
        repo,
        [
            "-c",
            "core.hooksPath=/dev/null",
            "worktree",
            "add",
            "--detach",
            "--quiet",
            str(worktree),
            candidate["commit_oid"],
        ],
    )
    _verify_clean_candidate_tree(worktree, candidate, index)
    _verify_go_module(worktree, environment, index)


def _verify_go_module(
    worktree: pathlib.Path,
    environment: dict[str, str],
    index: int,
) -> None:
    module = _run(
        [_go_runtime()["binary"], "list", "-m", "-json"],
        cwd=worktree,
        combined_output=True,
        env=environment,
    )
    _require(
        module.returncode == 0,
        f"candidate[{index}] cannot load its module with pinned Go toolchain",
    )


def _cleanup_worktree(repo: pathlib.Path, worktree: pathlib.Path) -> None:
    _run(["git", "worktree", "remove", "--force", str(worktree)], cwd=repo)
    shutil.rmtree(worktree, ignore_errors=True)
    _require(not worktree.exists(), "cannot delete temporary Git worktree")
    prune = _run(["git", "worktree", "prune"], cwd=repo)
    _require(prune.returncode == 0, "cannot prune temporary Git worktree metadata")
    listing = _git(repo, ["worktree", "list", "--porcelain", "-z"])
    try:
        listed_paths = [
            pathlib.Path(part.decode("utf-8").removeprefix("worktree ")).resolve()
            for part in listing.split(b"\0")
            if part.startswith(b"worktree ")
        ]
    except UnicodeDecodeError as exc:
        raise NegativeControlError("Git worktree path is not UTF-8") from exc
    _require(worktree.resolve() not in listed_paths, "temporary Git worktree metadata remains")


def _isolated_candidate_execution(
    repo: pathlib.Path,
    worktree: pathlib.Path,
    candidate: dict[str, Any],
    *,
    index: int,
    command: list[str],
    timeout_seconds: int,
    environment: dict[str, str],
    state_root: pathlib.Path,
    source_patch: bytes | None = None,
    source_paths: list[str] | None = None,
) -> dict[str, Any]:
    try:
        _reset_execution_state(state_root)
        _prepare_worktree(repo, worktree, candidate, index, environment)
        if source_patch is not None:
            _require(source_paths is not None, "source paths are required for a reversed run")
            _git(
                worktree,
                ["apply", "--reverse", "--index", "--whitespace=nowarn", "-"],
                input_bytes=source_patch,
            )
            reversed_paths_raw = _git(
                worktree,
                ["diff", "--cached", "--name-only", "-z", "--no-renames", "HEAD", "--"],
            )
            try:
                reversed_paths = sorted(
                    part.decode("utf-8") for part in reversed_paths_raw.split(b"\0") if part
                )
            except UnicodeDecodeError as exc:
                raise NegativeControlError("reversed source path is not UTF-8") from exc
            _require(reversed_paths == source_paths, f"candidate[{index}] reversed path set differs")
        return _execution(command, worktree, timeout_seconds, environment)
    finally:
        _cleanup_worktree(repo, worktree)


def _run_candidate(
    repo: pathlib.Path,
    candidate: dict[str, Any],
    *,
    index: int,
    timeout_seconds: int,
    environment: dict[str, str],
    state_root: pathlib.Path,
) -> dict[str, Any]:
    source_paths, test_go_paths, targets = _candidate_inputs(repo, candidate, index)
    command = [_go_runtime()["binary"], "test", *TEST_FLAGS, *targets]
    temporary_root = pathlib.Path(tempfile.mkdtemp(prefix="agent-brain-negative-control-"))
    patch = _diff(repo, candidate["parent_oid"], candidate["commit_oid"], source_paths)
    try:
        baseline = _isolated_candidate_execution(
            repo,
            temporary_root / "baseline",
            candidate,
            index=index,
            command=command,
            timeout_seconds=timeout_seconds,
            environment=environment,
            state_root=state_root,
        )
        reversed_run = _isolated_candidate_execution(
            repo,
            temporary_root / "reversed",
            candidate,
            index=index,
            command=command,
            timeout_seconds=timeout_seconds,
            environment=environment,
            state_root=state_root,
            source_patch=patch,
            source_paths=source_paths,
        )
    finally:
        shutil.rmtree(temporary_root, ignore_errors=True)
    return {
        "first_parent_position": candidate["first_parent_position"],
        "candidate_ref": candidate["candidate_ref"],
        "commit_oid": candidate["commit_oid"],
        "parent_oid": candidate["parent_oid"],
        "source_file_count": len(source_paths),
        "source_paths_sha256": candidate["source_paths_sha256"],
        "test_file_count": len(test_go_paths),
        "test_target_count": len(targets),
        "test_targets_sha256": _canonical_hash(targets),
        "test_command_sha256": _canonical_hash(command),
        "baseline": baseline,
        "reversed_source": reversed_run,
        "classification": _classification(baseline, reversed_run),
    }


def execute_receipt(
    repo: pathlib.Path,
    ledger: dict[str, Any],
    *,
    ledger_file_sha256: str,
    timeout_seconds: int,
    generated_at: str | None = None,
    progress: Callable[[str], None] | None = None,
) -> dict[str, Any]:
    repo = repo.resolve()
    _require(repo.is_dir(), "source repository is not a directory")
    _require(type(timeout_seconds) is int and 10 <= timeout_seconds <= 3600, "timeout_seconds must be an integer between 10 and 3600")
    try:
        task_eligibility.validate_scan(ledger)
    except task_eligibility.EligibilityError as exc:
        raise NegativeControlError(str(exc)) from exc
    _require(ledger["exposure"] == EXPOSURE, "only permanently development-only scans may run")
    _validate_repository_binding(repo, ledger)
    state_root = pathlib.Path(tempfile.mkdtemp(prefix="agent-brain-negative-control-state-"))
    _reset_execution_state(state_root)
    environment = _test_environment(state_root)
    environment_identity = _environment_identity(repo, environment)
    implementation_identity = _implementation_identity()
    results: list[dict[str, Any]] = []
    total = len(ledger["candidates"])
    try:
        for index, candidate in enumerate(ledger["candidates"]):
            if progress is not None:
                progress(f"negative-control {index + 1}/{total} position={candidate['first_parent_position']}: running")
            result = _run_candidate(
                repo,
                candidate,
                index=index,
                timeout_seconds=timeout_seconds,
                environment=environment,
                state_root=state_root,
            )
            results.append(result)
            if progress is not None:
                progress(
                    f"negative-control {index + 1}/{total} position={candidate['first_parent_position']}: "
                    f"{result['classification']}"
                )
    finally:
        shutil.rmtree(state_root, ignore_errors=True)
        _require(not state_root.exists(), "cannot delete isolated execution state")
    counts = Counter(result["classification"] for result in results)
    timestamp = generated_at or dt.datetime.now(dt.timezone.utc).isoformat().replace("+00:00", "Z")
    receipt = {
        "schema_version": 1,
        "profile": PROFILE,
        "status": STATUS,
        "exposure": EXPOSURE,
        "repository_id": ledger["repository_id"],
        "base_oid": ledger["base_oid"],
        "head_oid": ledger["head_oid"],
        "input_ledger_sha256": ledger["ledger_sha256"],
        "input_ledger_file_sha256": ledger_file_sha256,
        "generated_at": timestamp,
        "implementation": implementation_identity,
        "environment": environment_identity,
        "command_policy": {
            "name": COMMAND_POLICY,
            "reversal": REVERSAL_POLICY,
            "test_scope": "packages_containing_changed_go_test_files",
            "go_test_flags": list(TEST_FLAGS),
            "per_run_timeout_seconds": timeout_seconds,
        },
        "summary": {
            "candidate_count": len(results),
            "classification_counts": {name: counts[name] for name in CLASSIFICATIONS},
        },
        "results": results,
        "receipt_sha256": None,
    }
    receipt["receipt_sha256"] = _self_hash(receipt)
    validate_receipt(receipt)
    return receipt


def _validate_timestamp(value: Any, field: str) -> None:
    _require(isinstance(value, str) and bool(value), f"{field} is invalid")
    try:
        parsed = dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError as exc:
        raise NegativeControlError(f"{field} is invalid") from exc
    _require(parsed.tzinfo is not None and parsed.utcoffset() is not None, f"{field} must include a UTC offset")


def _valid_sha(value: Any) -> bool:
    return isinstance(value, str) and SHA256_RE.fullmatch(value) is not None and value != "0" * 64


def _valid_oid(value: Any) -> bool:
    return isinstance(value, str) and OID_RE.fullmatch(value) is not None and value != "0" * 40


def _validate_execution(value: Any, field: str) -> None:
    expected = {"status", "exit_code", "output_sha256", "output_byte_count"}
    _require(isinstance(value, dict) and set(value) == expected, f"{field} fields differ")
    _require(value["status"] in {"completed", "timeout"}, f"{field}.status is invalid")
    if value["status"] == "completed":
        _require(type(value["exit_code"]) is int and value["exit_code"] >= 0, f"{field}.exit_code is invalid")
    else:
        _require(value["exit_code"] is None, f"{field}.exit_code must be null on timeout")
    _require(_valid_sha(value["output_sha256"]), f"{field}.output_sha256 is invalid")
    _require(type(value["output_byte_count"]) is int and value["output_byte_count"] >= 0, f"{field}.output_byte_count is invalid")


def validate_receipt(value: dict[str, Any]) -> None:
    expected_root = {
        "schema_version", "profile", "status", "exposure", "repository_id", "base_oid", "head_oid",
        "input_ledger_sha256", "input_ledger_file_sha256", "generated_at", "implementation", "environment",
        "command_policy", "summary", "results", "receipt_sha256",
    }
    _require(set(value) == expected_root, "negative-control receipt root fields differ")
    _require(type(value["schema_version"]) is int and value["schema_version"] == 1 and value["profile"] == PROFILE, "negative-control profile changed")
    _require(value["status"] == STATUS and value["exposure"] == EXPOSURE, "negative-control scope changed")
    repository_id = value["repository_id"]
    _require(
        isinstance(repository_id, str)
        and bool(repository_id)
        and len(repository_id) <= 256
        and "\n" not in repository_id
        and "\r" not in repository_id,
        "repository_id is invalid",
    )
    for field in ("base_oid", "head_oid"):
        _require(_valid_oid(value[field]), f"{field} is invalid")
    for field in ("input_ledger_sha256", "input_ledger_file_sha256", "receipt_sha256"):
        _require(_valid_sha(value[field]), f"{field} is invalid")
    _validate_timestamp(value["generated_at"], "generated_at")
    _require(value["receipt_sha256"] == _self_hash(value), "negative-control receipt self hash mismatch")

    implementation = value["implementation"]
    expected_implementation = {
        "files", "aggregate_sha256", "python_implementation", "python_version",
    }
    _require(
        isinstance(implementation, dict) and set(implementation) == expected_implementation,
        "implementation fields differ",
    )
    files = implementation["files"]
    expected_roles = [
        "negative_control_runner",
        "eligibility_scanner",
        "canonical_population_helpers",
    ]
    _require(isinstance(files, list) and len(files) == len(expected_roles), "implementation files differ")
    for index, (item, role) in enumerate(zip(files, expected_roles, strict=True)):
        _require(
            isinstance(item, dict)
            and set(item) == {"role", "sha256"}
            and item["role"] == role
            and _valid_sha(item["sha256"]),
            f"implementation file[{index}] is invalid",
        )
    _require(
        _valid_sha(implementation["aggregate_sha256"])
        and implementation["aggregate_sha256"] == _canonical_hash(files),
        "implementation aggregate differs",
    )
    _require(
        all(
            isinstance(implementation[field], str) and bool(implementation[field])
            for field in ("python_implementation", "python_version")
        ),
        "Python implementation identity is invalid",
    )
    _require(implementation == _implementation_identity(), "implementation identity differs from current bytes")

    environment = value["environment"]
    expected_environment = {
        "go_version", "git_version", "go_binary_sha256", "go_env_keys", "go_env_sha256", "platform",
        "environment_policy", "environment_manifest_sha256", "inherited_variable_count",
        "credential_like_variable_count", "execution_overrides",
    }
    _require(isinstance(environment, dict) and set(environment) == expected_environment, "environment fields differ")
    _require(all(isinstance(environment[field], str) and bool(environment[field]) for field in ("go_version", "git_version", "platform")), "environment identity is invalid")
    _require(_valid_sha(environment["go_binary_sha256"]), "go_binary_sha256 is invalid")
    _require(environment["go_env_keys"] == list(GO_ENV_KEYS), "Go environment key set differs")
    _require(_valid_sha(environment["go_env_sha256"]), "go_env_sha256 is invalid")
    _require(
        environment["environment_policy"] == "deny_all_then_explicit_local_allowlist_v1",
        "execution environment policy differs",
    )
    _require(
        _valid_sha(environment["environment_manifest_sha256"]),
        "execution environment manifest is invalid",
    )
    for field in ("inherited_variable_count", "credential_like_variable_count"):
        _require(type(environment[field]) is int and environment[field] == 0, f"{field} must be zero")
    _require(
        environment["execution_overrides"]
        == {
            "GOENV": "off",
            "GOFLAGS": "",
            "GOPROXY": "off",
            "GOSUMDB": "off",
            "GOTOOLCHAIN": "local",
            "GOVCS": "*:off",
            "GOWORK": "off",
        },
        "execution environment overrides differ",
    )

    policy = value["command_policy"]
    expected_policy = {"name", "reversal", "test_scope", "go_test_flags", "per_run_timeout_seconds"}
    _require(isinstance(policy, dict) and set(policy) == expected_policy, "command policy fields differ")
    _require(policy["name"] == COMMAND_POLICY and policy["reversal"] == REVERSAL_POLICY, "command policy changed")
    _require(policy["test_scope"] == "packages_containing_changed_go_test_files", "test scope changed")
    _require(policy["go_test_flags"] == list(TEST_FLAGS), "Go test flags changed")
    _require(type(policy["per_run_timeout_seconds"]) is int and 10 <= policy["per_run_timeout_seconds"] <= 3600, "run timeout is invalid")

    results = value["results"]
    _require(isinstance(results, list), "results must be an array")
    expected_result = {
        "first_parent_position", "candidate_ref", "commit_oid", "parent_oid", "source_file_count",
        "source_paths_sha256", "test_file_count", "test_target_count", "test_targets_sha256",
        "test_command_sha256", "baseline", "reversed_source", "classification",
    }
    positions: list[int] = []
    refs: set[str] = set()
    counts: Counter[str] = Counter()
    for index, result in enumerate(results):
        _require(isinstance(result, dict) and set(result) == expected_result, f"result[{index}] fields differ")
        position = result["first_parent_position"]
        _require(type(position) is int and position > 0, f"result[{index}] position is invalid")
        positions.append(position)
        _require(_valid_sha(result["candidate_ref"]) and result["candidate_ref"] not in refs, f"result[{index}] candidate reference is invalid")
        refs.add(result["candidate_ref"])
        for field in ("commit_oid", "parent_oid"):
            _require(_valid_oid(result[field]), f"result[{index}].{field} is invalid")
        for field in ("source_paths_sha256", "test_targets_sha256", "test_command_sha256"):
            _require(_valid_sha(result[field]), f"result[{index}].{field} is invalid")
        for field in ("source_file_count", "test_file_count", "test_target_count"):
            _require(type(result[field]) is int and result[field] > 0, f"result[{index}].{field} is invalid")
        _validate_execution(result["baseline"], f"result[{index}].baseline")
        _validate_execution(result["reversed_source"], f"result[{index}].reversed_source")
        expected_classification = _classification(result["baseline"], result["reversed_source"])
        _require(result["classification"] == expected_classification, f"result[{index}] classification differs")
        counts[expected_classification] += 1
    _require(positions == sorted(set(positions)), "result positions are not unique first-parent order")

    summary = value["summary"]
    _require(isinstance(summary, dict) and set(summary) == {"candidate_count", "classification_counts"}, "summary fields differ")
    _require(type(summary["candidate_count"]) is int and summary["candidate_count"] >= 0, "candidate summary count is invalid")
    _require(summary["candidate_count"] == len(results), "candidate summary count differs")
    classification_counts = summary["classification_counts"]
    _require(
        isinstance(classification_counts, dict)
        and set(classification_counts) == set(CLASSIFICATIONS)
        and all(type(classification_counts[name]) is int and classification_counts[name] >= 0 for name in CLASSIFICATIONS),
        "classification summary values are invalid",
    )
    expected_counts = {name: counts[name] for name in CLASSIFICATIONS}
    _require(classification_counts == expected_counts, "classification summary differs")


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
    run_parser = subparsers.add_parser("run", help="run local development negative controls")
    run_parser.add_argument("--repo", required=True, type=pathlib.Path)
    run_parser.add_argument("--ledger", required=True, type=pathlib.Path)
    run_parser.add_argument("--output", required=True, type=pathlib.Path)
    run_parser.add_argument("--timeout-seconds", type=int, default=180)
    check_parser = subparsers.add_parser("check", help="validate a negative-control receipt")
    check_parser.add_argument("receipt", type=pathlib.Path)
    args = parser.parse_args(argv)
    try:
        if args.command == "run":
            ledger, raw = _load_json(args.ledger)
            receipt = execute_receipt(
                args.repo,
                ledger,
                ledger_file_sha256=_sha256(raw),
                timeout_seconds=args.timeout_seconds,
                progress=lambda message: print(message, file=sys.stderr, flush=True),
            )
            _write_atomic(args.output, receipt)
        else:
            receipt, _ = _load_json(args.receipt)
            validate_receipt(receipt)
    except NegativeControlError as exc:
        parser.error(str(exc))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
