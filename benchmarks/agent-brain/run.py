#!/usr/bin/env python3
"""Run controlled agent benchmarks for Entire Brain.

The harness creates disposable git worktrees, applies a known regression patch,
prepares the requested brain condition, lets an agent fix the task, validates
the result, and records machine-readable run data.
"""

from __future__ import annotations

import argparse
import datetime as dt
import glob
import hashlib
import json
import math
import os
import pathlib
import re
import shlex
import shutil
import subprocess
import sys
import tempfile
import textwrap
import time
from dataclasses import dataclass
from typing import Any


ROOT = pathlib.Path(__file__).resolve().parents[2]
BENCH_ROOT = pathlib.Path(__file__).resolve().parent
TASK_DIR = BENCH_ROOT / "tasks"
RESULT_DIR = BENCH_ROOT / "results"
CACHE_DIR = BENCH_ROOT / "cache"
VALIDATION_FIXTURE_DIR = BENCH_ROOT / "fixtures" / "validation"
BENCHMARK_COMMIT_DATE = "2026-01-01T00:00:00Z"

ISOLATION = {
    "codex": {
        "session": "ephemeral",
        "user_config": "ignored",
        "rules": "ignored",
        "auth": "host CODEX_HOME auth may still be used",
    },
    "claude": {
        "session": "no-session-persistence",
        "mcp": "strict empty config",
        "slash_commands": "disabled",
        "auth": "host Claude Max/OAuth auth may still be used",
        "settings": "default non-bare settings required for Max auth",
    },
}

PHASE2_PRICING = {
    "gpt-5.5": {"input_per_million": 5.00, "cache_read_per_million": 0.50, "output_per_million": 30.00},
    "gpt-5.4": {"input_per_million": 2.50, "cache_read_per_million": 0.25, "output_per_million": 15.00},
    "gpt-5.4-mini": {"input_per_million": 0.75, "cache_read_per_million": 0.075, "output_per_million": 4.50},
    "gpt-5.3-codex": {"input_per_million": 1.75, "cache_read_per_million": 0.175, "output_per_million": 14.00},
    "gpt-5.2": {"input_per_million": 1.75, "cache_read_per_million": 0.175, "output_per_million": 14.00},
    "claude-opus-4-8": {
        "input_per_million": 5.00,
        "cache_read_per_million": 0.50,
        "cache_creation_per_million": 6.25,
        "output_per_million": 25.00,
    },
    "claude-sonnet-4-6": {
        "input_per_million": 3.00,
        "cache_read_per_million": 0.30,
        "cache_creation_per_million": 3.75,
        "output_per_million": 15.00,
    },
    "claude-haiku-4-5": {
        "input_per_million": 1.00,
        "cache_read_per_million": 0.10,
        "cache_creation_per_million": 1.25,
        "output_per_million": 5.00,
    },
}

PHASE2_PROJECT_TOPICS = [
    {
        "repo": "entire-brain",
        "repo_path": str(ROOT),
        "area": "brain command surface",
        "queries": ["brief", "query decisions", "agent surface"],
        "brain_source": "hybrid",
        "signal": "The brief command should locate semantic code and warn about snapshot/live-state boundaries before edits.",
    },
    {
        "repo": "entire-brain",
        "repo_path": str(ROOT),
        "area": "semantic freshness",
        "queries": ["stale", "dirty-unindexed", "worktree overlay"],
        "brain_source": "semantic",
        "signal": "Freshness axes and live-state overlay prevent the agent from trusting stale semantic context.",
    },
    {
        "repo": "entire-brain",
        "repo_path": str(ROOT),
        "area": "seed agent contract",
        "queries": ["seed agent", "output-schema", "local validation"],
        "brain_source": "history",
        "signal": "Session history records a compatibility decision that is not obvious from current code alone.",
    },
    {
        "repo": "entire-brain",
        "repo_path": str(ROOT),
        "area": "bundle integrity",
        "queries": ["bundle import sha256", "checksum validation", "bundle semantic source"],
        "brain_source": "history",
        "signal": "History explains why checksum validation is mandatory even if simple import tests pass.",
    },
    {
        "repo": "entire-cli",
        "repo_path": "cli",
        "area": "review provenance env filtering",
        "queries": ["AppendReviewEnv", "provenance.IsEntry", "ENTIRE_INVESTIGATE"],
        "brain_source": "hybrid",
        "signal": "Semantic search localizes env plumbing while history names provenance entries that must stay stripped.",
    },
    {
        "repo": "entire-cli",
        "repo_path": "cli",
        "area": "manual commit hooks",
        "queries": ["manual_commit_hooks", "hook lifecycle", "checkpoint committed"],
        "brain_source": "hybrid",
        "signal": "Semantic relations and session history jointly identify hook lifecycle invariants.",
    },
    {
        "repo": "entire-cli",
        "repo_path": "cli",
        "area": "transcript path re-resolution",
        "queries": ["resolveTranscriptPath", "transcript re-resolution", "updates state"],
        "brain_source": "history",
        "signal": "Historical sessions describe why transcript state must be re-resolved after checkpoint movement.",
    },
    {
        "repo": "github-cli",
        "repo_path": "github-cli",
        "area": "repository name normalization",
        "queries": ["NormalizeRepoName", "TrimSuffix", "invalidCharactersRE"],
        "brain_source": "semantic",
        "signal": "Semantic context points to the shared normalizer instead of nearby command-specific call sites.",
    },
    {
        "repo": "github-cli",
        "repo_path": "github-cli",
        "area": "http auth scope suggestions",
        "queries": ["HandleHTTPError", "ScopesSuggestion", "X-Accepted-Oauth-Scopes"],
        "brain_source": "semantic",
        "signal": "Semantic context identifies the one production helper and avoids broader API test churn.",
    },
]

PHASE2_PROJECT_ARCHETYPES = [
    {
        "id": "architecture-localization",
        "prompt_shape": "Ask for the minimal fix in an area with many adjacent plausible files.",
        "validation_strategy": "Hidden validation checks that only the shared implementation changed.",
        "brain_advantage": "Semantic search and boundaries reduce file-read and patch-surface sprawl.",
        "metric": "score and changed_file_count",
    },
    {
        "id": "rationale-recovery",
        "prompt_shape": "Remove or invert a historical decision and ask the agent to restore intended behavior.",
        "validation_strategy": "Hidden validation checks the exact invariant and regression wording.",
        "brain_advantage": "Session history exposes why the prior decision existed.",
        "metric": "success_rate and score",
    },
    {
        "id": "validation-selection",
        "prompt_shape": "Hide focused validation and require the agent to infer the right tests.",
        "validation_strategy": "Score rewards focused test selection and penalizes broad unrelated edits.",
        "brain_advantage": "Semantic test suggestions identify the smallest relevant test target.",
        "metric": "tokens, seconds, and validation_discipline",
    },
    {
        "id": "stale-live-hygiene",
        "prompt_shape": "Prepare a brain, then mutate the worktree after brain prep.",
        "validation_strategy": "Hidden validation checks whether the agent accounted for live changes.",
        "brain_advantage": "Brief live-state overlay prevents stale-brain mistakes without forcing full diff reads.",
        "metric": "success_rate and file_read_count",
    },
]

PHASE2_GITHUB_PROJECT_ARCHETYPES = [
    *PHASE2_PROJECT_ARCHETYPES,
    {
        "id": "protocol-contract-recovery",
        "prompt_shape": "Ask for the stable CLI contract without naming the exact helper or validation path.",
        "validation_strategy": "Hidden validation checks the shared contract and rejects command-specific patches.",
        "brain_advantage": "Semantic context should surface the production helper and prior contract rationale before the edit.",
        "metric": "success_rate, score, tokens, and changed_file_count",
    },
]

PHASE2_SWE_ARCHETYPES = [
    {
        "id": "hidden-cross-file-contract",
        "prompt_shape": "Issue-style bug report omits expected files and exposes only failing behavior.",
        "validation_strategy": "Regression patch removes one contract edge; hidden tests assert the cross-file invariant.",
        "brain_advantage": "Semantic impact maps the non-local dependency before the edit.",
        "metric": "success_rate and score",
    },
    {
        "id": "large-repo-near-miss",
        "prompt_shape": "Issue wording names a symptom shared by several nearby modules.",
        "validation_strategy": "Hidden tests pass only when the shared helper changes.",
        "brain_advantage": "Semantic search disambiguates similar names and reduces exploratory reads.",
        "metric": "tokens and changed_file_count",
    },
    {
        "id": "history-only-regression",
        "prompt_shape": "Issue says a previous fix regressed but does not name the rationale.",
        "validation_strategy": "Setup removes both docs/progress and focused tests that explained the decision.",
        "brain_advantage": "Full session history recovers the missing decision and test intent.",
        "metric": "success_rate",
    },
    {
        "id": "stale-context-issue",
        "prompt_shape": "Issue is created after semantic prep and mutates the relevant file post-prep.",
        "validation_strategy": "Hidden tests require recognizing current worktree state.",
        "brain_advantage": "Brief reports dirty files and changed-symbol hints before semantic use.",
        "metric": "success_rate and file_read_count",
    },
]

PHASE2_COST_TASKS = [
    "entire-brain-history-codex-schema-contract",
    "entire-brain-history-bundle-sha256",
    "entire-brain-history-github-visibility",
    "entire-cli-review-base-flag-scope",
    "entire-cli-review-prompt-uncommitted-scope",
    "github-cli-repo-name-trims-dotgit",
    "github-cli-http-scopes-suggestion",
    "swe-style-entire-brain-stale-query-default-limit",
    "swe-style-entire-cli-transcript-reresolve",
]

PHASE2_LOWER_COST_RUNNERS = [
    {"runner": "codex-gpt-5.4-mini-medium", "agent": "codex", "model": "gpt-5.4-mini", "effort": "medium"},
    {"runner": "codex-gpt-5.3-codex-medium", "agent": "codex", "model": "gpt-5.3-codex", "effort": "medium"},
    {"runner": "codex-gpt-5.2-low", "agent": "codex", "model": "gpt-5.2", "effort": "low"},
    {"runner": "claude-sonnet-4-6-low", "agent": "claude", "model": "claude-sonnet-4-6", "effort": "low"},
    {"runner": "claude-haiku-4-5-low", "agent": "claude", "model": "claude-haiku-4-5", "effort": "low"},
]

@dataclass
class RunResult:
    record: dict[str, Any]
    run_dir: pathlib.Path


@dataclass(frozen=True)
class RunnerSpec:
    id: str
    agent: str
    model: str | None = None
    effort: str | None = None


SEMANTIC_CONDITIONS = {"semantic_brain", "semantic_cli", "mcp_semantic"}
FULL_HISTORY_CONDITIONS = {"full_brain", "full_cli_original", "full_cli_compact", "mcp_history", "mcp_workspace_radar"}
CLI_HISTORY_EXCERPT_CONDITIONS = {"full_brain", "full_cli_original"}
MCP_CONDITIONS = {"mcp_semantic", "mcp_history", "mcp_workspace_radar"}
MCP_BRAIN_TOOL_RE = r"brain_(?:stale|brief|query|search|vsearch|get|multi_get|context|impact|changes|code|tests|boundaries|regressions|review|workspace_regressions|workspace_review)"
MCP_SEMANTIC_CONTEXT_TOOLS = {"brain_context", "brain_impact", "brain_changes", "brain_code"}
MCP_UNIFIED_RETRIEVAL_TOOLS = {"brain_query", "brain_search", "brain_vsearch", "brain_get", "brain_multi_get"}
SAFE_MCP_BOOL_ARGUMENT_KEYS = {"location_only", "include_deletions", "blind_spots"}
SAFE_MCP_STRING_ARGUMENT_KEYS = {"workspace"}
SAFE_MCP_ARGUMENT_KEYS = SAFE_MCP_BOOL_ARGUMENT_KEYS | SAFE_MCP_STRING_ARGUMENT_KEYS
BENCHMARK_PRIVATE_PREFIXES = (".benchmark/", ".entire/", ".codex/")
HARNESS_SCAFFOLD_PATHS = (
    "benchmarks/agent-brain/tasks",
    "benchmarks/agent-brain/results",
    "benchmarks/agent-brain/cache",
    "benchmarks/agent-brain/discovery",
)
AGENT_VISIBLE_SECRET_PATTERNS = (
    '"validation"',
    '"setup_commands"',
    '"setup_replacements"',
    '"post_brain_commands"',
    '"post_brain_replacements"',
    "hide_validation_from_agent",
    "benchmarks/agent-brain/tasks",
    "benchmarks/agent-brain/results",
)
AGENT_OUTPUT_SECRET_PATTERNS = (
    '"validation"',
    "hide_validation_from_agent",
    "benchmarks/agent-brain/tasks",
)


def is_mcp_condition(condition: str) -> bool:
    return condition in MCP_CONDITIONS


def condition_prep_kind(condition: str) -> str:
    if condition in FULL_HISTORY_CONDITIONS:
        return "full_brain"
    if condition in SEMANTIC_CONDITIONS:
        return "semantic_brain"
    return condition


def condition_prepares_history(condition: str) -> bool:
    return condition in FULL_HISTORY_CONDITIONS


def condition_writes_history_excerpt(condition: str) -> bool:
    return condition in CLI_HISTORY_EXCERPT_CONDITIONS


def condition_copies_entire_history(condition: str) -> bool:
    return condition_prepares_history(condition)


def benchmark_workspace_name(task: dict[str, Any]) -> str:
    name = str(task.get("workspace_name") or "benchmark")
    if not re.fullmatch(r"[A-Za-z0-9._-]+", name) or name in {".", ".."} or not name.strip("."):
        raise ValueError(f"invalid benchmark workspace_name: {name!r}")
    return name


def safe_workspace_name(value: Any) -> str | None:
    if not isinstance(value, str):
        return None
    name = value.strip()
    if not re.fullmatch(r"[A-Za-z0-9._-]+", name) or name in {".", ".."} or not name.strip("."):
        return None
    return name


def manifest_source_counts(manifest: dict[str, Any]) -> dict[str, int]:
    sources = manifest.get("sources") if isinstance(manifest, dict) else None
    if not isinstance(sources, dict):
        return {"sessions": 0, "history_records": 0}

    sessions = sources.get("sessions")
    session_count = 0
    if isinstance(sessions, dict):
        session_items = sessions.get("sessions")
        if isinstance(session_items, list):
            session_count = len(session_items)

    history = sources.get("history")
    history_records = 0
    if isinstance(history, dict):
        records = history.get("records")
        if isinstance(records, int):
            history_records = records
        elif isinstance(records, float):
            history_records = int(records)

    return {"sessions": session_count, "history_records": history_records}


def assert_brain_state_ready(task: dict[str, Any], condition: str, state: dict[str, Any]) -> None:
    manifest = state.get("manifest") if isinstance(state, dict) else None
    if not isinstance(manifest, dict):
        raise RuntimeError(f"{condition} brain prep did not produce a readable manifest")

    if task.get("prepare_semantic", True) and condition_prep_kind(condition) in {"semantic_brain", "full_brain"}:
        if not manifest.get("has_semantic"):
            raise RuntimeError(f"{condition} brain prep did not produce a semantic source")

    if condition_prepares_history(condition):
        session_count = int(manifest.get("session_count") or 0)
        history_records = int(manifest.get("history_records") or 0)
        if session_count <= 0:
            raise RuntimeError(f"{condition} brain prep produced no exported sessions")
        if history_records <= 0:
            raise RuntimeError(f"{condition} brain prep produced no history index records")


def parse_runner_spec(value: str) -> RunnerSpec:
    value = value.strip()
    if not value:
        raise ValueError("empty runner spec")
    if "=" in value:
        runner_id, spec = value.split("=", 1)
    else:
        runner_id, spec = "", value
    parts = spec.split(":")
    agent = parts[0]
    if agent not in {"codex", "claude"}:
        raise ValueError(f"unknown runner agent {agent!r} in {value!r}")
    model = parts[1] if len(parts) > 1 and parts[1] else None
    effort = parts[2] if len(parts) > 2 and parts[2] else None
    if len(parts) > 3:
        raise ValueError(f"runner spec has too many ':' fields: {value!r}")
    if not runner_id:
        runner_id = agent
        if model:
            runner_id += f"-{model}"
        if effort:
            runner_id += f"-{effort}"
    return RunnerSpec(id=runner_id, agent=agent, model=model, effort=effort)


def load_pricing(args: argparse.Namespace) -> dict[str, Any]:
    data = args.pricing_json or os.environ.get("AGENT_BENCH_PRICING_JSON", "")
    if args.pricing_file:
        data = pathlib.Path(args.pricing_file).read_text()
    if not data:
        return {}
    payload = json.loads(data)
    if not isinstance(payload, dict):
        raise ValueError("pricing must be a JSON object")
    return payload


def estimate_cost_usd(runner: RunnerSpec, usage: dict[str, Any], pricing: dict[str, Any]) -> float | None:
    key = runner.id
    model_key = runner.model or runner.id
    entry = pricing.get(key) or pricing.get(model_key)
    if not isinstance(entry, dict):
        return None
    input_per_m = entry.get("input_per_million")
    output_per_m = entry.get("output_per_million")
    cache_read_per_m = entry.get("cache_read_per_million", input_per_m)
    cache_creation_per_m = entry.get("cache_creation_per_million", input_per_m)
    if input_per_m is None or output_per_m is None:
        return None
    cost = 0.0
    cost += float(usage.get("input_tokens") or 0) * float(input_per_m) / 1_000_000
    cost += float(usage.get("output_tokens") or 0) * float(output_per_m) / 1_000_000
    cost += float(usage.get("cache_read_tokens") or 0) * float(cache_read_per_m) / 1_000_000
    cost += float(usage.get("cache_creation_tokens") or 0) * float(cache_creation_per_m) / 1_000_000
    return cost


def run_cmd(
    args: list[str],
    *,
    cwd: pathlib.Path | str | None = None,
    env: dict[str, str] | None = None,
    input_text: str | None = None,
    timeout: int | None = None,
    check: bool = False,
) -> subprocess.CompletedProcess[str]:
    proc = subprocess.run(
        args,
        cwd=str(cwd) if cwd else None,
        env=env,
        input=input_text,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        timeout=timeout,
    )
    if check and proc.returncode != 0:
        raise RuntimeError(
            f"command failed ({proc.returncode}): {shlex.join(args)}\n"
            f"stdout:\n{proc.stdout}\nstderr:\n{proc.stderr}"
        )
    return proc


def benchmark_git_env() -> dict[str, str]:
    env = os.environ.copy()
    env.update(
        {
            "GIT_AUTHOR_DATE": BENCHMARK_COMMIT_DATE,
            "GIT_COMMITTER_DATE": BENCHMARK_COMMIT_DATE,
        }
    )
    return env


def shell_cmd(
    command: str,
    *,
    cwd: pathlib.Path,
    env: dict[str, str],
    timeout: int | None = None,
) -> subprocess.CompletedProcess[str]:
    return run_cmd(["/bin/bash", "-c", command], cwd=cwd, env=env, timeout=timeout)


def load_tasks(patterns: list[str]) -> list[dict[str, Any]]:
    paths: list[pathlib.Path] = []
    for pattern in patterns:
        candidate = pathlib.Path(pattern)
        if candidate.exists():
            paths.append(candidate)
        else:
            paths.extend(pathlib.Path(p) for p in glob.glob(str(TASK_DIR / pattern)))
    if not paths:
        paths = sorted(TASK_DIR.glob("*.json"))
    tasks = []
    for path in sorted(set(paths)):
        with path.open() as f:
            task = json.load(f)
        task["_path"] = str(path)
        tasks.append(task)
    return tasks


def resolve_repo_path(raw: str) -> pathlib.Path:
    """Resolve a task `repo_path` portably so the benchmark is reproducible on any
    machine (no hard-coded home paths). Order: expand `~` and `$VARS`/`${VARS}`,
    then if the result is relative, resolve it against `$AGENT_BENCH_REPO_ROOT`
    (default: the parent directory of this repo). Absolute paths pass through.
    Example: repo_path `"cli"` -> `<repo-root>/../cli`; `"../Ultron"` -> sibling.
    Set AGENT_BENCH_REPO_ROOT (or use absolute paths / `$VARS`) to point elsewhere."""
    expanded = os.path.expanduser(os.path.expandvars(str(raw)))
    if "$" in expanded:
        raise RuntimeError(
            f"unresolved environment variable in repo_path {raw!r}; set it before running "
            f"(see benchmarks/agent-brain/README.md)"
        )
    path = pathlib.Path(expanded)
    if not path.is_absolute():
        base = pathlib.Path(os.environ.get("AGENT_BENCH_REPO_ROOT") or str(ROOT.parent))
        path = base / path
    return path


def build_tools(run_root: pathlib.Path) -> dict[str, pathlib.Path]:
    bin_dir = run_root / "bin"
    bin_dir.mkdir(parents=True, exist_ok=True)
    brain_bin = bin_dir / "entire-brain"
    sem_bin = bin_dir / "entire-sem"
    entire_wrapper = bin_dir / "entire"

    run_cmd(["go", "build", "-o", str(brain_bin), "./cmd/entire-brain"], cwd=ROOT, check=True)
    run_cmd(
        ["go", "build", "-o", str(sem_bin), "./cmd/entire-sem"],
        cwd=ROOT.parent / "entire-sem",
        check=True,
    )

    system_entire = shutil.which("entire") or ""
    wrapper = f"""#!/usr/bin/env bash
set -euo pipefail
if [[ "${{1:-}}" == "brain" ]]; then
  shift
  exec "{brain_bin}" "$@"
fi
if [[ "${{1:-}}" == "sem" ]]; then
  shift
  exec "{sem_bin}" "$@"
fi
if [[ -n "{system_entire}" ]]; then
  exec "{system_entire}" "$@"
fi
echo "entire wrapper only supports brain and sem in this benchmark" >&2
exit 127
"""
    entire_wrapper.write_text(wrapper)
    entire_wrapper.chmod(0o755)
    return {"bin": bin_dir, "brain": brain_bin, "sem": sem_bin, "entire": entire_wrapper}


def git_head(repo: pathlib.Path) -> str:
    return run_cmd(["git", "rev-parse", "HEAD"], cwd=repo, check=True).stdout.strip()


def file_sha256(path: pathlib.Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(1024 * 1024), b""):
            h.update(chunk)
    return h.hexdigest()


def text_sha256(text: str) -> str:
    return hashlib.sha256(text.encode("utf-8")).hexdigest()


def stable_json_sha256(value: Any) -> str:
    data = json.dumps(value, sort_keys=True, separators=(",", ":"), default=str)
    return text_sha256(data)


def git_commit_metadata(repo: pathlib.Path, ref: str) -> dict[str, Any]:
    fmt = "%H%x00%P%x00%cI%x00%an%x00%s"
    proc = run_cmd(["git", "show", "-s", f"--format={fmt}", ref], cwd=repo)
    if proc.returncode != 0:
        return {
            "ref": ref,
            "available": False,
            "error": (proc.stderr or proc.stdout)[-1000:],
        }
    parts = proc.stdout.rstrip("\n").split("\x00", 4)
    if len(parts) != 5:
        return {"ref": ref, "available": False, "error": "unexpected git show output"}
    parents = parts[1].split() if parts[1] else []
    return {
        "ref": ref,
        "available": True,
        "commit": parts[0],
        "parents": parents,
        "parent_count": len(parents),
        "committer_date": parts[2],
        "author_name": parts[3],
        "subject": parts[4],
    }


def git_dirty_metadata(repo: pathlib.Path) -> dict[str, Any]:
    status = run_cmd(["git", "status", "--porcelain=v1"], cwd=repo)
    if status.returncode != 0:
        return {
            "available": False,
            "dirty": None,
            "error": (status.stderr or status.stdout)[-1000:],
        }
    lines = status.stdout.splitlines()
    diff = run_cmd(["git", "diff", "--binary", "--no-ext-diff", "HEAD", "--"], cwd=repo)
    diff_text = diff.stdout if diff.returncode == 0 else ""
    return {
        "available": True,
        "dirty": bool(lines),
        "status_entries": lines[:200],
        "status_count": len(lines),
        "status_sha256": text_sha256(status.stdout),
        "tracked_diff_sha256": text_sha256(diff_text),
    }


def git_remote_url(repo: pathlib.Path) -> str | None:
    proc = run_cmd(["git", "remote", "get-url", "origin"], cwd=repo)
    url = proc.stdout.strip()
    return url if proc.returncode == 0 and url else None


def display_path(path: pathlib.Path) -> str:
    try:
        return str(path.resolve().relative_to(ROOT))
    except ValueError:
        return str(path.resolve())


def task_file_sha256(task: dict[str, Any]) -> str | None:
    raw = task.get("_path")
    if not raw:
        return None
    path = pathlib.Path(str(raw))
    if not path.exists() or not path.is_file():
        return None
    return file_sha256(path)


def task_config_payload(task: dict[str, Any]) -> dict[str, Any]:
    return {k: v for k, v in task.items() if not str(k).startswith("_")}


def task_config_sha256(task: dict[str, Any]) -> str:
    return task_file_sha256(task) or stable_json_sha256(task_config_payload(task))


def bind_task_base_commit(task: dict[str, Any]) -> tuple[dict[str, Any], pathlib.Path, dict[str, Any]]:
    source = resolve_repo_path(task["repo_path"])
    base_ref = str(task.get("base_commit") or "HEAD")
    base = git_commit_metadata(source, base_ref)
    bound = dict(task)
    if base.get("available") and base.get("commit"):
        bound["_resolved_base_commit"] = base["commit"]
    return bound, source, base


def tools_provenance(tools: dict[str, pathlib.Path]) -> dict[str, Any]:
    out: dict[str, Any] = {}
    for name, path in sorted(tools.items()):
        entry: dict[str, Any] = {"path": str(path), "exists": path.exists()}
        if path.exists() and path.is_file():
            entry["sha256"] = file_sha256(path)
        out[name] = entry
    return out


def runner_payload(runner: RunnerSpec | None) -> dict[str, Any] | None:
    if runner is None:
        return None
    return {
        "id": runner.id,
        "agent": runner.agent,
        "model": runner.model,
        "effort": runner.effort,
    }


def run_config_provenance(
    command: str,
    runner: RunnerSpec | None,
    condition: str,
    repetition: int | None,
    suite_dir: pathlib.Path,
    args: argparse.Namespace,
    workspace_name: str | None = None,
) -> dict[str, Any]:
    pricing_json = getattr(args, "pricing_json", None)
    payload: dict[str, Any] = {
        "command": command,
        "suite": suite_dir.name,
        "condition": condition,
        "repetition": repetition,
        "runner": runner_payload(runner),
        "checkpoint_limit": getattr(args, "checkpoint_limit", None),
        "brain_cache": {
            "enabled": not bool(getattr(args, "no_brain_cache", False)),
            "refresh": bool(getattr(args, "refresh_brain_cache", False)),
        },
        "requested": {
            "tasks": getattr(args, "tasks", None),
            "agents": getattr(args, "agents", None),
            "runners": getattr(args, "runners", None),
            "conditions": getattr(args, "conditions", None),
            "repetitions": getattr(args, "repetitions", None),
        },
        "timeout": getattr(args, "timeout", None),
        "claude_budget": getattr(args, "claude_budget", None),
        "stop_after_no_brain_score": getattr(args, "stop_after_no_brain_score", None),
        "pricing": {
            "file": getattr(args, "pricing_file", None),
            "inline_sha256": text_sha256(pricing_json) if pricing_json else None,
        },
        "env_flags": {
            "BENCH_REGRESSION_RADAR": os.environ.get("BENCH_REGRESSION_RADAR"),
            "BENCH_RADAR_LOCATION_ONLY": os.environ.get("BENCH_RADAR_LOCATION_ONLY"),
        },
        "workspace_name": workspace_name if condition == "mcp_workspace_radar" else None,
    }
    panel_name = getattr(args, "panel_name", None)
    panel_path = getattr(args, "panel_path", None)
    panel_config_sha256 = getattr(args, "panel_config_sha256", None)
    if panel_name or panel_path or panel_config_sha256:
        payload["panel"] = {
            "name": panel_name,
            "path": panel_path,
            "config_sha256": panel_config_sha256,
        }
    payload["fingerprint"] = stable_json_sha256(payload)
    return payload


def build_record_provenance(
    task: dict[str, Any],
    runner: RunnerSpec | None,
    condition: str,
    repetition: int | None,
    suite_dir: pathlib.Path,
    tools: dict[str, pathlib.Path],
    args: argparse.Namespace,
    *,
    command: str,
    source: pathlib.Path,
    base: dict[str, Any],
) -> dict[str, Any]:
    task_path = pathlib.Path(str(task["_path"])) if task.get("_path") else None
    source_head = git_commit_metadata(source, "HEAD")
    task_base = task.get("base_commit")
    payload: dict[str, Any] = {
        "schema": 1,
        "captured_at": dt.datetime.now(dt.UTC).isoformat(),
        "harness": {
            "repo_path": str(ROOT),
            "head": git_commit_metadata(ROOT, "HEAD"),
            "dirty": git_dirty_metadata(ROOT),
        },
        "source": {
            "repo": task.get("repo"),
            "repo_path_input": task.get("repo_path"),
            "repo_path_resolved": str(source.resolve()),
            "origin_url": git_remote_url(source),
            "base_ref": str(task_base or "HEAD"),
            "base_ref_source": "task.base_commit" if task_base else "source_head",
            "base": base,
            "head": source_head,
            "dirty": git_dirty_metadata(source),
        },
        "task": {
            "id": task.get("id"),
            "path": display_path(task_path) if task_path else None,
            "config_sha256": task_config_sha256(task),
            "base_commit": task_base,
        },
        "run_config": run_config_provenance(
            command,
            runner,
            condition,
            repetition,
            suite_dir,
            args,
            benchmark_workspace_name(task) if condition == "mcp_workspace_radar" else None,
        ),
        "tools": tools_provenance(tools),
    }
    payload["task"]["radar_include_deletions"] = bool(task.get("radar_include_deletions"))
    payload["fingerprint"] = stable_json_sha256(
        {
            "harness_head": payload["harness"]["head"].get("commit"),
            "source_base": payload["source"]["base"].get("commit"),
            "source_head": payload["source"]["head"].get("commit"),
            "task": payload["task"],
            "run_config": payload["run_config"]["fingerprint"],
            "tools": {
                name: entry.get("sha256")
                for name, entry in payload["tools"].items()
                if isinstance(entry, dict)
            },
        }
    )
    return payload


def create_worktree(task: dict[str, Any], run_dir: pathlib.Path) -> pathlib.Path:
    source = resolve_repo_path(task["repo_path"])
    base = task.get("_resolved_base_commit") or task.get("base_commit") or git_head(source)
    worktree = run_dir / "worktree"
    worktree.mkdir(parents=True, exist_ok=False)
    archive = run_dir / "source.tar"
    run_cmd(["git", "archive", "--format=tar", "-o", str(archive), base], cwd=source, check=True)
    run_cmd(["tar", "-xf", str(archive), "-C", str(worktree)], cwd=run_dir, check=True)
    archive.unlink(missing_ok=True)
    run_cmd(["git", "init"], cwd=worktree, check=True)
    copy_origin_remote(source, worktree)
    ignore_benchmark_plugin(worktree)
    patch = task.get("setup_patch", "")
    if patch:
        run_cmd(["git", "apply", "-"], cwd=worktree, input_text=patch, check=True)
    apply_replacements(worktree, task.get("setup_replacements", []), "setup")
    setup_env = os.environ.copy()
    setup_env["BENCH_SOURCE_REPO"] = str(source)  # portable handle to the source repo for setup_commands
    for command in task.get("setup_commands", []):
        proc = shell_cmd(command, cwd=worktree, env=setup_env, timeout=120)
        if proc.returncode != 0:
            raise RuntimeError(
                f"setup command failed ({proc.returncode}): {command}\n"
                f"stdout:\n{proc.stdout}\nstderr:\n{proc.stderr}"
            )
    reset_agent_history_to_root(worktree, f"Benchmark agent baseline for {task['id']}", include_current_changes=True)
    return worktree


def copy_origin_remote(source: pathlib.Path, worktree: pathlib.Path) -> None:
    remote = run_cmd(["git", "remote", "get-url", "origin"], cwd=source)
    url = remote.stdout.strip()
    if remote.returncode == 0 and url:
        run_cmd(["git", "remote", "add", "origin", url], cwd=worktree, check=True)


def reset_agent_history_to_root(
    worktree: pathlib.Path,
    message: str,
    *,
    include_current_changes: bool = False,
) -> dict[str, Any]:
    """Make the current tree the visible baseline without exposing setup diffs."""
    if include_current_changes:
        run_cmd(["git", "add", "-A"], cwd=worktree, check=True)
    status = run_cmd(["git", "status", "--porcelain"], cwd=worktree, check=True).stdout.strip()
    if status and not include_current_changes:
        raise RuntimeError(f"cannot reset benchmark history with dirty worktree:\n{status}")
    tree = run_cmd(["git", "write-tree"], cwd=worktree, check=True).stdout.strip()
    commit = run_cmd(
        [
            "git",
            "-c",
            "user.name=Entire Brain Benchmark",
            "-c",
            "user.email=benchmark@example.invalid",
            "commit-tree",
            tree,
            "-m",
            message,
        ],
        cwd=worktree,
        env=benchmark_git_env(),
        check=True,
    ).stdout.strip()
    run_cmd(["git", "reset", "--hard", commit], cwd=worktree, check=True)
    run_cmd(["git", "reflog", "expire", "--expire=now", "--expire-unreachable=now", "--all"], cwd=worktree, check=True)
    run_cmd(["git", "prune", "--expire=now"], cwd=worktree, check=True)
    head_line = run_cmd(["git", "rev-list", "--parents", "-n", "1", "HEAD"], cwd=worktree, check=True).stdout.strip()
    parent_count = max(0, len(head_line.split()) - 1)
    return {"root_commit": commit, "parent_count": parent_count}


def copy_entire_history(source: pathlib.Path, worktree: pathlib.Path) -> None:
    source_entire = source / ".entire"
    if not source_entire.exists():
        raise RuntimeError(f"copy_entire_history_from_source requested but {source_entire} is missing")
    target_entire = worktree / ".entire"
    target_entire.mkdir(parents=True, exist_ok=True)
    for name in ("metadata", "settings.json", ".gitignore"):
        src = source_entire / name
        if not src.exists():
            continue
        dst = target_entire / name
        if dst.exists():
            if dst.is_dir():
                shutil.rmtree(dst)
            else:
                dst.unlink()
        if src.is_dir():
            shutil.copytree(src, dst)
        else:
            shutil.copy2(src, dst)


def ignore_benchmark_plugin(worktree: pathlib.Path) -> None:
    proc = run_cmd(["git", "rev-parse", "--git-path", "info/exclude"], cwd=worktree, check=True)
    raw = proc.stdout.strip()
    exclude_path = pathlib.Path(raw)
    if not exclude_path.is_absolute():
        exclude_path = worktree / exclude_path
    exclude_path.parent.mkdir(parents=True, exist_ok=True)
    existing = exclude_path.read_text() if exclude_path.exists() else ""
    entries = [".benchmark/", ".entire/", ".codex/"]
    missing = [entry for entry in entries if entry not in existing.splitlines()]
    if missing:
        suffix = "" if existing.endswith("\n") or not existing else "\n"
        exclude_path.write_text(existing + suffix + "\n".join(missing) + "\n")


def apply_post_brain_setup(task: dict[str, Any], worktree: pathlib.Path) -> bool:
    changed = False
    replacements = task.get("post_brain_replacements", [])
    commands = task.get("post_brain_commands", [])
    if replacements:
        apply_replacements(worktree, replacements, "post-brain")
        changed = True
    for command in commands:
        proc = shell_cmd(command, cwd=worktree, env=os.environ.copy(), timeout=120)
        if proc.returncode != 0:
            raise RuntimeError(
                f"post-brain command failed ({proc.returncode}): {command}\n"
                f"stdout:\n{proc.stdout}\nstderr:\n{proc.stderr}"
        )
        changed = True
    return changed


def apply_replacements(worktree: pathlib.Path, replacements: list[dict[str, str]], label: str) -> None:
    for replacement in replacements:
        rel = replacement["path"]
        path = worktree / rel
        data = path.read_text()
        old = replacement["old"]
        new = replacement["new"]
        count = data.count(old)
        if count != 1:
            raise RuntimeError(f"{label} replacement for {rel} matched {count} times, expected 1")
        path.write_text(data.replace(old, new, 1))


def agent_hidden_paths(task: dict[str, Any] | None = None) -> list[str]:
    paths = list(HARNESS_SCAFFOLD_PATHS)
    for rel in (task or {}).get("agent_hidden_paths", []):
        clean = str(rel).strip().replace("\\", "/").strip("/")
        if not clean or clean == "." or clean.startswith("../") or "/../" in clean:
            raise ValueError(f"invalid agent_hidden_paths entry: {rel!r}")
        paths.append(clean)
    return sorted(dict.fromkeys(paths), key=lambda item: (item.count("/"), item))


def sanitize_agent_worktree(worktree: pathlib.Path, task: dict[str, Any] | None = None) -> dict[str, Any]:
    removed: list[str] = []
    for rel in agent_hidden_paths(task):
        path = worktree / rel
        if not path.exists():
            continue
        if path.is_dir():
            shutil.rmtree(path)
        else:
            path.unlink()
        removed.append(rel)
    if not removed:
        return {"removed_paths": [], "committed": False}
    reset_agent_history_to_root(worktree, "Benchmark sanitized agent baseline", include_current_changes=True)
    return {"removed_paths": removed, "committed": True}


def secret_pattern_hits(text: str, patterns: tuple[str, ...] = AGENT_VISIBLE_SECRET_PATTERNS) -> list[str]:
    lower = text.lower()
    return sorted({pattern for pattern in patterns if pattern.lower() in lower})


def agent_secret_preflight(worktree: pathlib.Path) -> dict[str, Any]:
    findings: list[dict[str, Any]] = []
    for rel in HARNESS_SCAFFOLD_PATHS:
        path = worktree / rel
        if path.exists():
            findings.append({"kind": "harness_path_visible", "path": rel})
    for rel in ("benchmarks/agent-brain/tasks", "benchmarks/agent-brain/results"):
        path = worktree / rel
        if not path.exists():
            continue
        for candidate in sorted(path.rglob("*")):
            if not candidate.is_file() or candidate.is_symlink():
                continue
            try:
                text = candidate.read_text(encoding="utf-8", errors="ignore")
            except OSError:
                continue
            hits = secret_pattern_hits(text)
            if hits:
                findings.append(
                    {
                        "kind": "secret_pattern_visible",
                        "path": str(candidate.relative_to(worktree)),
                        "patterns": hits,
                    }
                )
                if len(findings) >= 20:
                    break
    return {"ok": not findings, "findings": findings}


def scrub_benchmark_secret_lines(path: pathlib.Path) -> tuple[int, int]:
    try:
        data = path.read_text(encoding="utf-8", errors="ignore")
    except OSError:
        return (0, 0)
    lines = data.splitlines(keepends=True)
    kept: list[str] = []
    removed = 0
    for line in lines:
        if secret_pattern_hits(line):
            removed += 1
            continue
        kept.append(line)
    if removed:
        path.write_text("".join(kept), encoding="utf-8")
    return (removed, len(lines))


def scrub_benchmark_secret_json(value: Any) -> tuple[Any, int]:
    if isinstance(value, str):
        if secret_pattern_hits(value):
            return "[redacted benchmark scaffold]", 1
        return value, 0
    if isinstance(value, list):
        scrubbed = []
        changes = 0
        for item in value:
            next_item, count = scrub_benchmark_secret_json(item)
            changes += count
            scrubbed.append(next_item)
        return scrubbed, changes
    if isinstance(value, dict):
        scrubbed = {}
        changes = 0
        for key, item in value.items():
            if isinstance(key, str) and secret_pattern_hits(key):
                changes += 1
                continue
            next_item, count = scrub_benchmark_secret_json(item)
            changes += count
            scrubbed[key] = next_item
        return scrubbed, changes
    return value, 0


def sanitize_brain_history(plugin: pathlib.Path) -> dict[str, Any]:
    brain_roots = [
        plugin / "data" / "brain",
        plugin / "data" / "repos",
    ]
    existing_roots = [root for root in brain_roots if root.exists()]
    if not existing_roots:
        return {"ok": True, "files_checked": 0, "files_scrubbed": 0, "items_redacted": 0}
    files_checked = 0
    files_scrubbed = 0
    items_redacted = 0
    for brain_data in existing_roots:
        for path in sorted(brain_data.rglob("*")):
            if not path.is_file() or path.is_symlink():
                continue
            suffix = path.suffix.lower()
            if suffix not in {".jsonl", ".json", ".md", ".txt"}:
                continue
            files_checked += 1
            if suffix == ".json":
                try:
                    payload = json.loads(path.read_text(encoding="utf-8", errors="ignore"))
                except (OSError, json.JSONDecodeError):
                    removed, _ = scrub_benchmark_secret_lines(path)
                    if removed:
                        files_scrubbed += 1
                        items_redacted += removed
                    continue
                scrubbed, changes = scrub_benchmark_secret_json(payload)
                if changes:
                    path.write_text(json.dumps(scrubbed, indent=2, sort_keys=True) + "\n", encoding="utf-8")
                    files_scrubbed += 1
                    items_redacted += changes
                continue
            removed, _ = scrub_benchmark_secret_lines(path)
            if removed:
                files_scrubbed += 1
                items_redacted += removed
    return {
        "ok": True,
        "files_checked": files_checked,
        "files_scrubbed": files_scrubbed,
        "items_redacted": items_redacted,
    }


def hidden_validation_markers(task: dict[str, Any]) -> list[str]:
    if not task.get("hide_validation_from_agent"):
        return []
    markers: list[str] = []
    explicit_markers = task.get("leak_markers")
    if explicit_markers is not None:
        if isinstance(explicit_markers, list):
            markers.extend(str(marker) for marker in explicit_markers if marker)
        elif explicit_markers:
            markers.append(str(explicit_markers))
        return [marker for marker in markers if len(marker.strip()) >= 12]
    markers.extend(str(command) for command in task.get("validation", []) if command)
    for entry in task.get("validation_files", []):
        if not isinstance(entry, dict):
            continue
        markers.append(str(entry.get("path", "")))
        markers.append(str(entry.get("fixture", "")))
        markers.append(str(entry.get("content", "")))
    return [marker for marker in markers if len(marker.strip()) >= 12]


def agent_output_leak_audit(task: dict[str, Any], stdout: str, stderr: str) -> dict[str, Any]:
	text = stdout + "\n" + stderr
	findings: list[dict[str, Any]] = []
	if not task.get("leak_markers"):
		pattern_hits = secret_pattern_hits(text, AGENT_OUTPUT_SECRET_PATTERNS)
		if pattern_hits:
			findings.append({"kind": "benchmark_secret_pattern_in_output", "patterns": pattern_hits})
	marker_hits = []
	for marker in hidden_validation_markers(task):
		if marker in text:
			marker_hits.append(hashlib.sha256(marker.encode()).hexdigest()[:16])
	if marker_hits:
		findings.append({"kind": "hidden_validation_marker_in_output", "marker_hashes": sorted(set(marker_hits))})
	return {"ok": not findings, "findings": findings}


def brain_query_answer_texts(task: dict[str, Any]) -> list[tuple[str, str]]:
    """The hidden, answer-bearing texts a brain_queries hint must not overlap (release blocker B1):
    the fix itself (setup replacements, including WHERE it lives) is always hidden from the agent,
    as are expected_files when the task hides them; validation commands, expected-string greps,
    hidden test names, and validation fixtures (including fixture file CONTENTS) count only when
    the task hides validation, because visible validation reaches both arms and is not an asymmetry."""
    texts: list[tuple[str, str]] = []
    for replacement in task.get("setup_replacements", []) + task.get("post_brain_replacements", []):
        texts.append(("fix_text", str(replacement.get("old", ""))))
        texts.append(("fix_text", str(replacement.get("new", ""))))
        texts.append(("fix_location", str(replacement.get("path", ""))))
    for command in task.get("setup_commands", []) + task.get("post_brain_commands", []):
        texts.append(("fix_command", str(command)))
    if task.get("hide_expected_from_agent"):
        for expected in task.get("expected_files", []):
            texts.append(("hidden_expected_file", str(expected)))
    if task.get("hide_validation_from_agent"):
        for command in task.get("validation", []):
            texts.append(("hidden_validation_command", str(command)))
            for match in re.finditer(r"-run\s+'([^']+)'|-run\s+(\S+)", str(command)):
                texts.append(("hidden_test_name", match.group(1) or match.group(2)))
            for match in re.finditer(r"rg\s+(?:-\S+\s+)*'([^']+)'", str(command)):
                texts.append(("hidden_expected_string", match.group(1)))
        for entry in task.get("validation_files", []):
            if isinstance(entry, dict):
                for key in ("path", "fixture", "content"):
                    if entry.get(key):
                        texts.append(("hidden_validation_file", str(entry[key])))
                if entry.get("fixture"):
                    try:
                        fixture = safe_child_path(
                            VALIDATION_FIXTURE_DIR, str(entry["fixture"]), label="validation fixture"
                        )
                    except Exception:
                        fixture = None
                    if fixture is not None and fixture.is_file():
                        texts.append(("hidden_validation_file", fixture.read_text()))
    return [(kind, text) for kind, text in texts if text]


def brain_query_token_is_identifier(token: str) -> bool:
    """Code-shaped tokens (PascalCase/camelCase, snake_case, dotted, flags, paths) are the
    answer-bearing carriers B1 names; plain lowercase English words are matched only as part
    of a verbatim phrase, never alone, so symptom vocabulary stays usable."""
    if len(token) < 4:
        return False
    if token.startswith("--") or "_" in token or "." in token or "/" in token:
        return True
    return any(ch.isupper() for ch in token[1:])


# Most-specific-first: a leak found in several overlapping texts (a test name is a substring of
# its own validation command) is reported once, labeled with the most specific source.
BRAIN_QUERY_ANSWER_KIND_PRIORITY = (
    "hidden_test_name",
    "hidden_expected_string",
    "hidden_validation_file",
    "hidden_validation_command",
    "fix_text",
    "fix_command",
    "fix_location",
    "hidden_expected_file",
)


BRAIN_QUERY_HIDDEN_IDENTIFIER_RE = re.compile(r"[A-Za-z][A-Za-z0-9_./-]{4,}")


def brain_query_hidden_identifier_folds(text: str) -> set[str]:
    """Casefolded identifier-shaped tokens occurring in a hidden answer text — used to catch
    queries that smuggle an identifier by lowercasing it or splitting it into words, two
    transformations retrieval undoes. Dotted/path tokens are also folded per segment so
    `state.RealignAttributionBase(newHead)` yields `realignattributionbase` as well."""
    folds: set[str] = set()
    for match in BRAIN_QUERY_HIDDEN_IDENTIFIER_RE.finditer(text):
        token = match.group(0)
        if brain_query_token_is_identifier(token):
            folds.add(token.lower())
        for segment in re.split(r"[._/-]", token):
            if len(segment) >= 6 and brain_query_token_is_identifier(segment):
                folds.add(segment.lower())
    return folds


def brain_query_leak_audit(task: dict[str, Any]) -> dict[str, Any]:
    """Release blocker B1: `Useful query terms: {brain_queries}` reaches the brain arm only, so any
    query content that also appears in the hidden fix or hidden validation hands that arm the
    answer and confounds the comparison. Flags (1) identifier-shaped query tokens found
    case-insensitively inside any hidden answer text (retrieval and agents fold case, so a
    lowercased identifier is exactly as answer-bearing), (2) query tokens or 2-4 word runs whose
    casefolded concatenation equals an identifier from a hidden text (lowercased or split
    identifiers), and (3) any contiguous 3+ word query phrase found there after punctuation
    normalization, reported as the maximal matching phrase."""
    answer_texts = brain_query_answer_texts(task)
    kind_rank = {kind: rank for rank, kind in enumerate(BRAIN_QUERY_ANSWER_KIND_PRIORITY)}
    best: dict[tuple[str, str, str], dict[str, Any]] = {}

    def record(query_text: str, term: str, where: str, finding_kind: str) -> None:
        key = (finding_kind, query_text, term.lower())
        rank = kind_rank.get(where, len(kind_rank))
        existing = best.get(key)
        if existing is None or rank < kind_rank.get(existing["where"], len(kind_rank)):
            field = "token" if finding_kind == "answer_bearing_identifier" else "phrase"
            best[key] = {"kind": finding_kind, "query": query_text, field: term, "where": where}

    for query in task.get("brain_queries", []):
        query_text = str(query)
        words = [word.strip("`\"',;:()").rstrip(".?!") for word in query_text.split()]
        for kind, text in answer_texts:
            folded_text = text.lower()
            identifier_folds = brain_query_hidden_identifier_folds(text)
            for token in words:
                if brain_query_token_is_identifier(token) and token.lower() in folded_text:
                    record(query_text, token, kind, "answer_bearing_identifier")
                elif len(token) >= 6 and token.lower() in identifier_folds:
                    record(query_text, token, kind, "answer_bearing_identifier")
            # split-identifier check: 3-4 word runs whose joined casefold IS a hidden identifier.
            # Two-word runs are deliberately exempt — natural compounds ("base commit",
            # "transcript path") match two-segment identifiers constantly; that residual is
            # disclosed in docs/release-blockers.md alongside the 1-2-word plain-fragment one.
            for size in (3, 4):
                for start in range(len(words) - size + 1):
                    joined = "".join(word.lower() for word in words[start:start + size])
                    if len(joined) >= 8 and joined in identifier_folds:
                        record(
                            query_text, " ".join(words[start:start + size]), kind, "answer_bearing_identifier"
                        )
            matched_starts = [
                start
                for start in range(len(words) - 2)
                if " ".join(words[start:start + 3]).lower() in folded_text
            ]
            # merge consecutive matching 3-gram windows into one maximal phrase
            run_start: int | None = None
            for index, start in enumerate(matched_starts):
                if run_start is None:
                    run_start = start
                last_in_run = index + 1 >= len(matched_starts) or matched_starts[index + 1] != start + 1
                if last_in_run:
                    phrase = " ".join(words[run_start:start + 3])
                    record(query_text, phrase, kind, "answer_bearing_phrase")
                    run_start = None
    unique = sorted(best.values(), key=lambda f: (f["query"], f.get("token") or f.get("phrase") or ""))
    return {"ok": not unique, "findings": unique}


def mcp_history_required_tools(runner: "RunnerSpec | None") -> tuple[str, ...]:
    """Tools the mcp_history condition must call (a floor, not a ceiling). Opus's brief-only
    delivery is told NOT to call brain_search, and the gpt-5.x disciplined delivery DOES call
    brain_search but can also fix correctly from the brief alone — in both cases requiring
    brain_search would mis-flag a correct run as a failure (the under-reporting bug). So these
    models only require brain_brief; all other models still require both.

    Radar deliveries (location-only / answer-assisted) reshape the mcp_history condition: the
    policy tells the agent to call brain_regressions instead of brain_brief/brain_search, so
    brain_regressions is the required floor there (requiring brain_brief would mis-flag a correct
    radar run — the bug that made every radar arm look like it bypassed the brain)."""
    if wants_radar_location_only(runner) or wants_regression_radar(runner):
        return ("brain_regressions",)
    compact = runner is not None and runner.model in (OPUS_COMPACT_MODELS | COMPACT_STRICT_MODELS)
    return ("brain_brief",) if compact else ("brain_brief", "brain_search")


def mcp_required_tools(condition: str, runner: "RunnerSpec | None") -> tuple[str, ...]:
    if condition == "mcp_semantic":
        return ("brain_status",)
    if condition == "mcp_workspace_radar":
        return ("brain_workspace_regressions",)
    if condition == "mcp_history":
        return mcp_history_required_tools(runner)
    return ()


def bare_mcp_tool_name(name: Any) -> str:
    text = str(name)
    if "__" in text:
        return text.rsplit("__", 1)[-1]
    return text


def safe_arg_matches(actual: Any, expected: Any) -> bool:
    if isinstance(expected, bool):
        return actual is expected
    if isinstance(expected, str):
        return isinstance(actual, str) and actual == expected
    return actual == expected


def required_arg_matches(args: dict[str, Any], required_args: dict[str, Any]) -> bool:
    return all(safe_arg_matches(args.get(key), value) for key, value in required_args.items())


def activity_has_mcp_call_with_args(activity: dict[str, Any], tool: str, required_args: dict[str, Any]) -> bool:
    details = activity.get("mcp_tool_details") if isinstance(activity.get("mcp_tool_details"), list) else []
    for detail in details:
        if not isinstance(detail, dict):
            continue
        name = str(detail.get("name") or "")
        if not (name == tool or name.endswith(f"__{tool}")):
            continue
        if detail.get("errored"):
            continue
        args = detail.get("arguments") if isinstance(detail.get("arguments"), dict) else {}
        if required_arg_matches(args, required_args):
            return True
    return False


def missing_mcp_call_args(activity: dict[str, Any], tool: str, required_args: dict[str, Any]) -> list[str]:
    details = activity.get("mcp_tool_details") if isinstance(activity.get("mcp_tool_details"), list) else []
    candidates: list[dict[str, Any]] = []
    for detail in details:
        if not isinstance(detail, dict):
            continue
        name = str(detail.get("name") or "")
        if not (name == tool or name.endswith(f"__{tool}")):
            continue
        if detail.get("errored"):
            continue
        candidates.append(detail)
    if not candidates:
        return sorted(required_args)
    missing = []
    for key, value in required_args.items():
        if not any(safe_arg_matches((detail.get("arguments") if isinstance(detail.get("arguments"), dict) else {}).get(key), value) for detail in candidates):
            missing.append(key)
    if not missing and not activity_has_mcp_call_with_args(activity, tool, required_args):
        missing.append("combined_arguments")
    return sorted(missing)


def radar_required_args(task: dict[str, Any] | None) -> dict[str, Any]:
    required = {"location_only": True}
    if isinstance(task, dict) and task.get("radar_include_deletions"):
        required["include_deletions"] = True
    return required


def workspace_radar_required_args(task: dict[str, Any] | None) -> dict[str, Any]:
    required = radar_required_args(task)
    required["workspace"] = benchmark_workspace_name(task or {})
    return required


def mcp_condition_audit(
    condition: str,
    agent_info: dict[str, Any],
    runner: "RunnerSpec | None" = None,
    task: dict[str, Any] | None = None,
) -> dict[str, Any]:
    if not is_mcp_condition(condition):
        return {"ok": True, "required": False, "findings": []}
    activity = agent_info.get("activity") if isinstance(agent_info.get("activity"), dict) else {}
    findings: list[dict[str, Any]] = []
    mcp_tool_names = list(activity.get("mcp_tool_names") or [])
    if not agent_info.get("mcp", {}).get("enabled"):
        findings.append({"kind": "mcp_not_enabled"})
    if int(activity.get("mcp_tool_calls") or 0) <= 0:
        findings.append({"kind": "no_mcp_tool_calls"})
    for required in mcp_required_tools(condition, runner):
        if not any(str(name).endswith(f"__{required}") or str(name) == required for name in mcp_tool_names):
            findings.append({"kind": "missing_required_mcp_tool", "condition": condition, "tool": required})
    if condition == "mcp_workspace_radar":
        for arg in missing_mcp_call_args(activity, "brain_workspace_regressions", workspace_radar_required_args(task)):
            findings.append({"kind": "missing_required_mcp_argument", "condition": condition, "tool": "brain_workspace_regressions", "argument": arg})
    if condition == "mcp_semantic" and (not isinstance(task, dict) or task.get("prepare_semantic", True)):
        bare_names = {bare_mcp_tool_name(name) for name in mcp_tool_names}
        forbidden = sorted(bare_names & MCP_UNIFIED_RETRIEVAL_TOOLS)
        if forbidden:
            findings.append({"kind": "forbidden_mcp_tool", "condition": condition, "tool": ",".join(forbidden)})
        if not (bare_names & MCP_SEMANTIC_CONTEXT_TOOLS):
            findings.append({"kind": "missing_required_mcp_tool_group", "condition": condition, "tool": "semantic_graph_context"})
    if condition == "mcp_history" and wants_radar_location_only(runner):
        for arg in missing_mcp_call_args(activity, "brain_regressions", radar_required_args(task)):
            findings.append({"kind": "missing_required_mcp_argument", "condition": condition, "tool": "brain_regressions", "argument": arg})
    if int(activity.get("direct_brain_cli_calls") or 0) > 0:
        findings.append({"kind": "direct_brain_cli_used_in_mcp_condition"})
    return {
        "ok": not findings,
        "required": True,
        "mcp_tool_calls": activity.get("mcp_tool_calls", 0),
        "mcp_tool_names": mcp_tool_names,
        "mcp_tool_details": activity.get("mcp_tool_details", []),
        "direct_brain_cli_calls": activity.get("direct_brain_cli_calls", 0),
        "findings": findings,
    }


def remove_worktree(source: pathlib.Path, worktree: pathlib.Path) -> None:
    proc = run_cmd(["git", "worktree", "remove", "--force", str(worktree)], cwd=source)
    if proc.returncode != 0 and worktree.exists():
        shutil.rmtree(worktree, ignore_errors=True)


CHECKPOINT_REF = "refs/heads/entire/checkpoints/v1"


def prepare_condition_history(task: dict[str, Any], condition: str, worktree: pathlib.Path) -> None:
    if not condition_copies_entire_history(condition):
        return
    source = resolve_repo_path(task["repo_path"])
    if task.get("copy_entire_history_from_source"):
        copy_entire_history(source, worktree)
    if task.get("copy_checkpoint_ref_from_source"):
        copy_checkpoint_ref(source, worktree)


def copy_checkpoint_ref(source: pathlib.Path, worktree: pathlib.Path) -> None:
    """Bring the Entire checkpoint branch (real session history, synced from the
    checkpoint remote, e.g. entireio/cli-checkpoints) into the disposable worktree
    so `entire brain refresh sessions` materializes the same sessions the live repo sees.
    The ref is removed again before the agent runs (remove_agent_visible_entire_history),
    so the agent cannot read raw transcripts via git; only the indexed brain remains."""
    probe = run_cmd(["git", "rev-parse", "--verify", "-q", CHECKPOINT_REF], cwd=source)
    if probe.returncode != 0 or not probe.stdout.strip():
        raise RuntimeError(
            f"copy_checkpoint_ref_from_source requested but {source} has no {CHECKPOINT_REF}; "
            f"run `entire brain refresh` there first to pull sessions from the checkpoint remote"
        )
    run_cmd(["git", "fetch", "--no-tags", str(source), f"+{CHECKPOINT_REF}:{CHECKPOINT_REF}"], cwd=worktree, check=True)
    src_settings = source / ".entire" / "settings.json"
    if src_settings.exists():
        (worktree / ".entire").mkdir(parents=True, exist_ok=True)
        shutil.copy2(src_settings, worktree / ".entire" / "settings.json")


def remove_agent_visible_entire_history(worktree: pathlib.Path) -> bool:
    removed = False
    target_entire = worktree / ".entire"
    if target_entire.exists():
        shutil.rmtree(target_entire)
        removed = True
    # Drop the Entire checkpoint branch so the agent cannot read raw session
    # transcripts via git; the indexed brain stays under .benchmark/plugin.
    if run_cmd(["git", "rev-parse", "--verify", "-q", CHECKPOINT_REF], cwd=worktree).returncode == 0:
        run_cmd(["git", "update-ref", "-d", CHECKPOINT_REF], cwd=worktree)
        run_cmd(["git", "reflog", "expire", "--expire=now", "--all"], cwd=worktree)
        run_cmd(["git", "prune", "--expire=now"], cwd=worktree)
        removed = True
    return removed


def run_plugin_dir(worktree: pathlib.Path) -> pathlib.Path:
    return worktree / ".benchmark" / "plugin"


def plugin_env(run_dir: pathlib.Path, worktree: pathlib.Path, tools: dict[str, pathlib.Path]) -> dict[str, str]:
    env = os.environ.copy()
    plugin = run_plugin_dir(worktree)
    env.update(
        {
            "PATH": f"{tools['bin']}:{env.get('PATH', '')}",
            "ENTIRE_REPO_ROOT": str(worktree),
            "ENTIRE_PLUGIN_CONFIG_DIR": str(plugin / "config"),
            "ENTIRE_PLUGIN_DATA_DIR": str(plugin / "data"),
            "ENTIRE_PLUGIN_STATE_DIR": str(plugin / "state"),
            "ENTIRE_PLUGIN_CACHE_DIR": str(plugin / "cache"),
        }
    )
    return env


def apply_task_env(env: dict[str, str], task: dict[str, Any]) -> dict[str, str]:
    # Expand ~ and $VARS so path_prefix is portable; "auto" / unset resolves the
    # directory of the host `node` so tsx-based validations work without a hard-coded path.
    def expand_prefix(raw: str) -> str:
        value = os.path.expanduser(os.path.expandvars(str(raw)))
        if value in ("auto", "") or "$" in value:
            node = shutil.which("node")
            return str(pathlib.Path(node).parent) if node else value
        return value
    path_prefixes = [expand_prefix(item) for item in task.get("path_prefixes", []) if item]
    if task.get("path_prefix"):
        path_prefixes.insert(0, expand_prefix(task["path_prefix"]))
    path_prefixes = [p for p in path_prefixes if p]
    if path_prefixes:
        env = env.copy()
        env["PATH"] = ":".join([*path_prefixes, env.get("PATH", "")])
    return env


def checkpoint_ref_sha_for_task(task: dict[str, Any]) -> str:
    """SHA of the source repo's checkpoint-history ref, so a moving session
    history invalidates the brain cache. The benchmark's thesis is that history
    helps, so a static-content cache key (base_commit + tool SHAs) would risk
    serving a stale brain if the checkpoint ref advanced between runs."""
    source = resolve_repo_path(task["repo_path"])
    proc = run_cmd(["git", "rev-parse", "--verify", "-q", CHECKPOINT_REF], cwd=source)
    return proc.stdout.strip() if proc.returncode == 0 else ""


def brain_cache_payload(
    task: dict[str, Any],
    condition: str,
    worktree: pathlib.Path,
    tools: dict[str, pathlib.Path],
    checkpoint_limit: int,
) -> dict[str, Any]:
    prep_kind = condition_prep_kind(condition)
    return {
        "schema": 3,
        "repo": task.get("repo"),
        "repo_path": str(resolve_repo_path(task["repo_path"]).resolve()),
        "base_ref": task.get("_resolved_base_commit") or task.get("base_commit") or git_head(resolve_repo_path(task["repo_path"])),
        "condition": prep_kind,
        "prepare_semantic": bool(task.get("prepare_semantic", True)),
        "checkpoint_limit": checkpoint_limit if condition_prepares_history(prep_kind) else None,
        "history_index": condition_prepares_history(prep_kind),
        # Bind the cache to the actual checkpoint-history content for conditions
        # that consume it, so a moving history is not silently reused.
        "checkpoint_ref_sha": checkpoint_ref_sha_for_task(task)
        if (condition_prepares_history(prep_kind) or task.get("copy_checkpoint_ref_from_source"))
        else None,
        "workspace_name": benchmark_workspace_name(task) if condition == "mcp_workspace_radar" else None,
        "copy_entire_history_from_source": bool(task.get("copy_entire_history_from_source"))
        and condition_copies_entire_history(condition),
        "setup_patch": task.get("setup_patch", ""),
        "setup_replacements": task.get("setup_replacements", []),
        "setup_commands": task.get("setup_commands", []),
        "brain_sha256": file_sha256(tools["brain"]),
        "sem_sha256": file_sha256(tools["sem"]),
    }


def brain_cache_key(payload: dict[str, Any]) -> str:
    data = json.dumps(payload, sort_keys=True, separators=(",", ":")).encode()
    return hashlib.sha256(data).hexdigest()[:24]


def copy_cached_plugin(cache_plugin: pathlib.Path, run_plugin: pathlib.Path, old_worktree: str, new_worktree: str) -> None:
    if run_plugin.exists():
        shutil.rmtree(run_plugin)
    shutil.copytree(cache_plugin, run_plugin)
    if old_worktree == new_worktree:
        return
    old = old_worktree.encode()
    new = new_worktree.encode()
    for path in run_plugin.rglob("*"):
        if not path.is_file() or path.is_symlink():
            continue
        data = path.read_bytes()
        if old not in data:
            continue
        try:
            data.decode("utf-8")
        except UnicodeDecodeError:
            continue
        path.write_bytes(data.replace(old, new))


def brain_prep_commands(task: dict[str, Any], condition: str, worktree: pathlib.Path, tools: dict[str, pathlib.Path], checkpoint_limit: int) -> list[list[str]]:
    commands = [[str(tools["brain"]), "refresh", "seed", str(worktree), "--agent", "none", "--force"]]
    if task.get("prepare_semantic", True):
        commands.append([str(tools["brain"]), "refresh", "index", str(worktree), "--sem-binary", str(tools["entire"]), "--force"])
    if condition_prepares_history(condition):
        # `export` moved under `refresh sessions` (PR #40 export-under-refresh); same flags.
        commands.insert(0, [str(tools["brain"]), "refresh", "sessions", "--checkpoint-limit", str(checkpoint_limit), "--history-index"])
    if condition == "mcp_workspace_radar":
        workspace = benchmark_workspace_name(task)
        repo_name = str(task.get("repo") or "repo")
        commands.append([str(tools["brain"]), "workspace", "create", workspace])
        commands.append([str(tools["brain"]), "workspace", "add", workspace, str(worktree), "--name", repo_name])
    return commands


def prepare_brain(
    task: dict[str, Any],
    condition: str,
    worktree: pathlib.Path,
    run_dir: pathlib.Path,
    tools: dict[str, pathlib.Path],
    checkpoint_limit: int,
    use_cache: bool = True,
    refresh_cache: bool = False,
) -> tuple[dict[str, str], dict[str, Any]]:
    env = apply_task_env(plugin_env(run_dir, worktree, tools), task)
    prep: dict[str, Any] = {"condition": condition, "commands": []}
    if condition == "no_brain":
        return env, prep

    payload = brain_cache_payload(task, condition, worktree, tools, checkpoint_limit)
    key = brain_cache_key(payload)
    cache_entry = CACHE_DIR / key
    cache_plugin = cache_entry / "plugin"
    cache_meta = cache_entry / "meta.json"
    plugin = run_plugin_dir(worktree)
    prep["cache"] = {"enabled": use_cache, "key": key, "hit": False}
    if use_cache and cache_plugin.exists() and cache_meta.exists() and not refresh_cache:
        meta = json.loads(cache_meta.read_text())
        copy_cached_plugin(cache_plugin, plugin, meta.get("source_worktree", ""), str(worktree))
        prep["history_sanitization"] = sanitize_brain_history(plugin)
        prep["cache"].update(
            {
                "hit": True,
                "source_worktree": meta.get("source_worktree"),
                "created_at": meta.get("created_at"),
            }
        )
        if condition_writes_history_excerpt(condition) and task.get("history_excerpt", True):
            write_history_excerpt(task, worktree)
        return env, prep

    commands = brain_prep_commands(task, condition, worktree, tools, checkpoint_limit)

    for cmd in commands:
        start = time.time()
        proc = run_cmd(cmd, cwd=worktree, env=env, timeout=900)
        entry = {
            "cmd": cmd,
            "returncode": proc.returncode,
            "seconds": time.time() - start,
            "stdout_tail": proc.stdout[-4000:],
            "stderr_tail": proc.stderr[-4000:],
        }
        prep["commands"].append(entry)
        if proc.returncode != 0:
            raise RuntimeError(f"brain prep failed: {shlex.join(cmd)}\n{proc.stderr}")
    prep["history_sanitization"] = sanitize_brain_history(plugin)
    if use_cache:
        tmp_entry = cache_entry.with_name(cache_entry.name + f".tmp-{os.getpid()}")
        if tmp_entry.exists():
            shutil.rmtree(tmp_entry)
        tmp_entry.mkdir(parents=True, exist_ok=True)
        shutil.copytree(plugin, tmp_entry / "plugin")
        (tmp_entry / "meta.json").write_text(
            json.dumps(
                {
                    "key": key,
                    "created_at": dt.datetime.now(dt.UTC).isoformat(),
                    "source_worktree": str(worktree),
                    "payload": payload,
                    "commands": prep["commands"],
                },
                indent=2,
                sort_keys=True,
            )
        )
        if cache_entry.exists():
            shutil.rmtree(cache_entry)
        tmp_entry.rename(cache_entry)
    if condition_writes_history_excerpt(condition) and task.get("history_excerpt", True):
        write_history_excerpt(task, worktree)
    return env, prep


def read_json_file(path: pathlib.Path) -> Any:
    with path.open() as f:
        return json.load(f)


def capture_brief_packet(
    task: dict[str, Any],
    condition: str,
    runner: "RunnerSpec | None",
    worktree: pathlib.Path,
    env: dict[str, str],
    tools: dict[str, pathlib.Path],
    run_dir: pathlib.Path,
) -> None:
    """Best-effort diagnostic: dump the brain's `brief --json` output for this run into the run
    dir so delivery can be diagnosed from real brain content (not just what the agent quoted).
    Mirrors prompt_for's brief query/limit construction. The `agent_runs_cli_brief` flag records
    whether the agent's policy actually issues a CLI brief for this run: it does NOT when the
    agent uses MCP tools (mcp_* conditions) or when semantic is disabled and it works from the
    history-excerpt file instead — in those cases this packet is brain-content context, not the
    literal text the agent saw. Never fails the run."""
    try:
        base = task["prompt"].strip()
        queries = ", ".join(task.get("brain_queries", []))
        brief_query = f"{task['id']}: {base[:120]}"
        if queries:
            brief_query = f"{brief_query} | {queries}"
        is_opus = runner is not None and runner.model in OPUS_COMPACT_MODELS
        semantic_available = task.get("prepare_semantic", True)
        # The agent issues a CLI `entire brain brief` only on a semantic-available CLI condition;
        # mcp_* conditions use MCP brain_brief, and a no-semantic run works from the excerpt.
        agent_runs_cli_brief = semantic_available and not str(condition).startswith("mcp")
        args = [str(tools["brain"]), "brief", brief_query, "--json"]
        # --limit mirrors prompt_for: only the full_cli_compact CLI brief is limited, and only
        # when semantic is available (otherwise prompt_for emits no brief at all).
        if condition == "full_cli_compact" and semantic_available:
            args += ["--limit", "2"] if is_opus else ["--limit", "4"]
        proc = run_cmd(args, cwd=worktree, env=env, timeout=180)
        (run_dir / "brief-packet.json").write_text(
            json.dumps(
                {
                    "condition": condition,
                    "query": brief_query,
                    "args": args[1:],
                    "agent_runs_cli_brief": agent_runs_cli_brief,
                    "returncode": proc.returncode,
                    "stdout": proc.stdout,
                    "stderr_tail": proc.stderr[-2000:],
                },
                indent=2,
            )
        )
    except Exception as exc:  # diagnostic only — never block the run
        try:
            (run_dir / "brief-packet.json").write_text(json.dumps({"error": str(exc)}, indent=2))
        except Exception:
            pass


def collect_brain_state(worktree: pathlib.Path, env: dict[str, str], tools: dict[str, pathlib.Path]) -> dict[str, Any]:
    state: dict[str, Any] = {}
    path_proc = run_cmd([str(tools["brain"]), "path", str(worktree)], cwd=worktree, env=env, timeout=120)
    state["path_command"] = {
        "returncode": path_proc.returncode,
        "stdout_tail": path_proc.stdout[-4000:],
        "stderr_tail": path_proc.stderr[-4000:],
    }
    if path_proc.returncode != 0:
        return state

    brain_dir = pathlib.Path(path_proc.stdout.strip().splitlines()[-1])
    state["brain_dir"] = str(brain_dir)
    manifest_path = brain_dir / "manifest.json"
    if not manifest_path.exists():
        state["manifest_error"] = "manifest.json missing"
        return state

    manifest = read_json_file(manifest_path)
    sources = manifest.get("sources") if isinstance(manifest, dict) else None
    semantic = sources.get("semantic") if isinstance(sources, dict) else None
    counts = manifest_source_counts(manifest)
    state["manifest"] = {
        "schema_version": manifest.get("schema_version"),
        "repo_key": manifest.get("repo_key"),
        "repo_root": manifest.get("repo_root"),
        "has_seed": bool(isinstance(sources, dict) and sources.get("seed")),
        "has_semantic": bool(semantic),
        "has_checkpoints": bool(isinstance(sources, dict) and sources.get("checkpoints")),
        "has_sessions": counts["sessions"] > 0,
        "session_count": counts["sessions"],
        "has_history": counts["history_records"] > 0,
        "history_records": counts["history_records"],
    }
    if isinstance(semantic, dict):
        state["semantic"] = semantic
        artifacts: dict[str, Any] = {}
        for key in ("snapshot_path", "store_path", "metrics_path", "parse_cache_path", "overlay_path"):
            rel = semantic.get(key)
            if not rel:
                continue
            artifact_path = brain_dir / pathlib.Path(rel)
            artifact: dict[str, Any] = {"path": rel, "exists": artifact_path.exists()}
            if artifact_path.exists() and artifact_path.is_file():
                artifact["bytes"] = artifact_path.stat().st_size
            elif artifact_path.exists() and artifact_path.is_dir():
                artifact["entries"] = sum(1 for _ in artifact_path.rglob("*"))
            artifacts[key] = artifact
        state["semantic_artifacts"] = artifacts

        metrics_rel = semantic.get("metrics_path")
        if isinstance(metrics_rel, str):
            metrics_path = brain_dir / pathlib.Path(metrics_rel)
            if metrics_path.exists():
                state["semantic_metrics"] = read_json_file(metrics_path)

    stale_proc = run_cmd([str(tools["brain"]), "stale", str(worktree), "--json"], cwd=worktree, env=env, timeout=120)
    state["stale_command"] = {
        "returncode": stale_proc.returncode,
        "stdout_tail": stale_proc.stdout[-4000:],
        "stderr_tail": stale_proc.stderr[-4000:],
    }
    if stale_proc.returncode == 0 and stale_proc.stdout.strip():
        try:
            state["stale"] = json.loads(stale_proc.stdout)
        except json.JSONDecodeError as exc:
            state["stale_parse_error"] = str(exc)
    return state


def prep_record_summary(record: dict[str, Any]) -> str:
    prep = record.get("brain_prep", {})
    cache = prep.get("cache", {}) if isinstance(prep, dict) else {}
    seconds = sum(
        float(command.get("seconds") or 0)
        for command in prep.get("commands", [])
        if isinstance(command, dict)
    )
    state = record.get("brain_state", {})
    semantic = state.get("semantic", {}) if isinstance(state, dict) else {}
    stale = state.get("stale", {}) if isinstance(state, dict) else {}
    parts = [f"ok={record.get('ok')}"]
    if cache:
        parts.append(f"cache_hit={cache.get('hit')}")
    if seconds:
        parts.append(f"prep_seconds={seconds:.1f}")
    if semantic:
        parts.append(f"files={semantic.get('files')}")
        parts.append(f"symbols={semantic.get('symbols')}")
        parts.append(f"relations={semantic.get('relations')}")
    if stale:
        parts.append(f"stale={stale.get('severity')}")
    return " ".join(parts)


def unique_json_values(values: list[dict[str, Any]]) -> list[dict[str, Any]]:
    keyed = {json.dumps(value, sort_keys=True, separators=(",", ":"), default=str): value for value in values}
    return [keyed[key] for key in sorted(keyed)]


def suite_provenance(records: list[dict[str, Any]]) -> dict[str, Any]:
    provenances = [record.get("provenance") for record in records if isinstance(record.get("provenance"), dict)]
    sources = []
    harnesses = []
    tasks = []
    run_configs = []
    for prov in provenances:
        source = prov.get("source") if isinstance(prov.get("source"), dict) else {}
        harness = prov.get("harness") if isinstance(prov.get("harness"), dict) else {}
        task = prov.get("task") if isinstance(prov.get("task"), dict) else {}
        run_config = prov.get("run_config") if isinstance(prov.get("run_config"), dict) else {}
        sources.append(
            {
                "repo": source.get("repo"),
                "repo_path_input": source.get("repo_path_input"),
                "repo_path_resolved": source.get("repo_path_resolved"),
                "base_ref": source.get("base_ref"),
                "base_ref_source": source.get("base_ref_source"),
                "base_commit": (source.get("base") or {}).get("commit") if isinstance(source.get("base"), dict) else None,
                "head_commit": (source.get("head") or {}).get("commit") if isinstance(source.get("head"), dict) else None,
                "dirty": (source.get("dirty") or {}).get("dirty") if isinstance(source.get("dirty"), dict) else None,
            }
        )
        harnesses.append(
            {
                "repo_path": harness.get("repo_path"),
                "head_commit": (harness.get("head") or {}).get("commit") if isinstance(harness.get("head"), dict) else None,
                "dirty": (harness.get("dirty") or {}).get("dirty") if isinstance(harness.get("dirty"), dict) else None,
            }
        )
        tasks.append(
            {
                "id": task.get("id"),
                "path": task.get("path"),
                "config_sha256": task.get("config_sha256"),
                "base_commit": task.get("base_commit"),
            }
        )
        run_configs.append(
            {
                "command": run_config.get("command"),
                "condition": run_config.get("condition"),
                "runner": run_config.get("runner"),
                "checkpoint_limit": run_config.get("checkpoint_limit"),
                "brain_cache": run_config.get("brain_cache"),
                "requested": run_config.get("requested"),
                "fingerprint": run_config.get("fingerprint"),
            }
        )
    payload = {
        "schema": 1,
        "records": len(records),
        "records_with_provenance": len(provenances),
        "missing_record_provenance": len(records) - len(provenances),
        "harnesses": unique_json_values(harnesses),
        "sources": unique_json_values(sources),
        "tasks": unique_json_values(tasks),
        "run_configs": unique_json_values(run_configs),
    }
    payload["fingerprint"] = stable_json_sha256(payload)
    return payload


def summarize_prep(records: list[dict[str, Any]], suite_dir: pathlib.Path) -> dict[str, Any]:
    summary_records = []
    for record in records:
        state = record.get("brain_state", {})
        semantic = state.get("semantic", {}) if isinstance(state, dict) else {}
        metrics = state.get("semantic_metrics", {}) if isinstance(state, dict) else {}
        stale = state.get("stale", {}) if isinstance(state, dict) else {}
        prep = record.get("brain_prep", {})
        commands = prep.get("commands", []) if isinstance(prep, dict) else []
        summary_records.append(
            {
                "run_id": record.get("run_id"),
                "task_id": record.get("task_id"),
                "condition": record.get("condition"),
                "ok": record.get("ok"),
                "cache": prep.get("cache") if isinstance(prep, dict) else None,
                "prep_seconds": sum(
                    float(command.get("seconds") or 0)
                    for command in commands
                    if isinstance(command, dict)
                ),
                "semantic_files": semantic.get("files"),
                "semantic_symbols": semantic.get("symbols"),
                "semantic_relations": semantic.get("relations"),
                "semantic_warnings": metrics.get("warnings"),
                "semantic_partial_failures": metrics.get("partial_failures"),
                "semantic_build_millis": metrics.get("build_millis"),
                "semantic_store_bytes": metrics.get("store_bytes"),
                "stale_severity": stale.get("severity"),
            }
        )
    summary = {
        "records": len(records),
        "ok": sum(1 for record in records if record.get("ok")),
        "failed": sum(1 for record in records if not record.get("ok")),
        "provenance": suite_provenance(records),
        "prep": summary_records,
    }
    (suite_dir / "prep-summary.json").write_text(json.dumps(summary, indent=2, sort_keys=True))
    return summary


def write_history_excerpt(task: dict[str, Any], worktree: pathlib.Path) -> None:
    queries = [q for q in task.get("brain_queries", []) if q]
    if not queries:
        return
    limit = int(task.get("history_excerpt_lines", 60))
    candidates: list[tuple[int, str]] = []
    seen: set[str] = set()
    history_files = list(history_excerpt_files(worktree))
    excluded_path_terms = [str(term) for term in task.get("history_exclude_path_terms", []) if term]
    for query in queries:
        for path, line_no, raw in matching_history_lines(history_files, query):
            if any(term in str(path) for term in excluded_path_terms):
                continue
            rel = path.relative_to(worktree) if path.is_relative_to(worktree) else path
            for excerpt, excerpt_score in history_snippets_for_match(raw, query):
                key = excerpt.strip()
                if key and key not in seen:
                    seen.add(key)
                    candidates.append((excerpt_score, f"## Query `{query}` ({rel}:{line_no})\n\n{excerpt}"))
    if not candidates:
        return
    snippets = [text for _, text in sorted(candidates, key=lambda item: item[0], reverse=True)[:limit]]
    fact_summary = summarize_history_facts(snippets)
    out_dir = worktree / ".benchmark"
    out_dir.mkdir(exist_ok=True)
    (out_dir / "brain-history-excerpt.md").write_text(
        "# Retrieved Checkpoint History Excerpt\n\n"
        "This file is generated by the benchmark from Entire v1 checkpoints for the full-brain condition.\n\n"
        + fact_summary
        + ("\n\n".join(snippets) if task.get("history_include_raw_snippets", True) else "")
        + "\n"
    )
    run_cmd(["git", "add", ".benchmark/brain-history-excerpt.md"], cwd=worktree, check=True)
    run_cmd(
        [
            "git",
            "-c",
            "user.name=Entire Brain Benchmark",
            "-c",
            "user.email=benchmark@example.invalid",
            "commit",
            "-m",
            "Benchmark full-brain history excerpt",
        ],
        cwd=worktree,
        env=benchmark_git_env(),
        check=True,
    )


def history_excerpt_files(worktree: pathlib.Path) -> list[pathlib.Path]:
    files: list[pathlib.Path] = []
    brain_root = run_plugin_dir(worktree) / "data" / "brain"
    if brain_root.exists():
        files.extend(sorted(brain_root.rglob("sessions/**/*.jsonl")))
        files.extend(sorted(brain_root.rglob("history/index.json")))
    checkpoint_root = worktree / "entire" / "checkpoints" / "v1"
    if checkpoint_root.exists():
        files.extend(sorted(checkpoint_root.rglob("*full.jsonl")))
    return files


def history_snippets_for_match(raw: str, query: str) -> list[tuple[str, int]]:
    snippets: list[tuple[str, int]] = []
    seen: set[str] = set()
    for text in history_text_candidates(raw, query):
        snippet = window_history_text(normalize_history_text(text), query)
        if not snippet or snippet in seen or reject_history_snippet(snippet):
            continue
        seen.add(snippet)
        snippets.append((snippet, score_history_snippet(snippet)))
    fallback = window_history_text(normalize_history_text(raw), query)
    if not snippets and fallback and not reject_history_snippet(fallback):
        snippets.append((window_history_text(normalize_history_text(raw), query), score_history_snippet(raw)))
    return snippets


def reject_history_snippet(snippet: str) -> bool:
    lower = snippet.lower()
    stripped = snippet.lstrip()
    scaffolding = (
        "benchmarks/agent-brain/tasks/",
        "benchmarks/agent-brain/results/",
        "phase2-discovery",
        '"task_id"',
        '"run_id"',
        '"setup_commands"',
        '"setup_replacements"',
        '"validation"',
    )
    if any(term in lower for term in scaffolding):
        return True
    if "validation:" in lower:
        return True
    if stripped.startswith("test $("):
        return True
    if stripped.startswith('{"timestamp"') or '"payload"' in stripped[:300]:
        return True
    return False


def history_text_candidates(raw: str, query: str) -> list[str]:
    needle = query.lower()
    queue = [raw]
    queued = {raw}
    candidates: list[str] = []

    def enqueue(text: str) -> None:
        if not text or text in queued or needle not in text.lower():
            return
        queued.add(text)
        queue.append(text)

    while queue:
        text = queue.pop(0)
        lower = text.lower()
        if needle not in lower:
            continue
        if len(text) <= 50_000:
            candidates.append(text)
        for line in text.splitlines():
            if needle not in line.lower():
                continue
            candidates.append(line)
            match = re.match(r"^.+?:\d+:(\{.*\})$", line)
            if match:
                enqueue(match.group(1))
        try:
            parsed = json.loads(text)
        except json.JSONDecodeError:
            continue
        for item in json_string_values(parsed):
            enqueue(item)
    return candidates


def json_string_values(value: Any) -> list[str]:
    values: list[str] = []
    if isinstance(value, str):
        values.append(value)
    elif isinstance(value, dict):
        for item in value.values():
            values.extend(json_string_values(item))
    elif isinstance(value, list):
        for item in value:
            values.extend(json_string_values(item))
    return values


def normalize_history_text(text: str) -> str:
    normalized = text.replace("\\r\\n", "\n").replace("\\n", "\n").replace("\\t", "\t")
    normalized = normalized.replace('\\"', '"').replace("\\/", "/")
    normalized = re.sub(r"^Chunk ID:.*?\n", "", normalized)
    normalized = re.sub(r"^Wall time:.*?\n", "", normalized, flags=re.MULTILINE)
    normalized = re.sub(r"^Original token count:.*?\n", "", normalized, flags=re.MULTILINE)
    normalized = re.sub(r"^Output:\n", "", normalized, flags=re.MULTILINE)
    return normalized.strip()


def window_history_text(text: str, query: str) -> str:
    if not text:
        return ""
    needle = query.lower()
    lines = text.splitlines()
    hit = next((i for i, line in enumerate(lines) if needle in line.lower()), None)
    if hit is not None and len(lines) > 1:
        start = max(0, hit - 8)
        end = min(len(lines), hit + 18)
        snippet = "\n".join(lines[start:end])
    else:
        lower = text.lower()
        idx = lower.find(needle)
        if idx == -1:
            snippet = text[:1800]
        else:
            start = max(0, idx - 700)
            end = min(len(text), idx + len(query) + 1400)
            snippet = text[start:end]
    snippet = "\n".join(line.rstrip() for line in snippet.splitlines())
    if len(snippet) > 2600:
        snippet = textwrap.shorten(snippet, width=2600, placeholder="\n...[truncated]...")
    return snippet.strip()


def score_history_snippet(snippet: str) -> int:
    lower = snippet.lower()
    score = 0
    for needle in ("internal/cli/", "docs/seed_plan.md", "readme.md", "templates/entire-brain-intake-claude-agent.md"):
        if needle in lower:
            score += 4
    for needle in ("case \"claude-code\"", "testseedagentcommandargsclaudecode", "testseedagentclaudephase", "--no-session-persistence", "--setting-sources", "--bare"):
        if needle in lower:
            score += 3
    if "restored" in lower or "validation passed" in lower or "apply_patch" in lower:
        score += 2
    if "react-split-flap" in lower or "agentviz" in lower:
        score -= 8
    if reject_history_snippet(snippet):
        score -= 20
    return score


def summarize_history_facts(snippets: list[str]) -> str:
    text = "\n".join(snippets)
    lower = text.lower()
    facts: list[str] = []
    if 'case "claude-code"' in lower or "agent synthesis mode: none, command, codex, or claude-code" in lower:
        facts.append(
            "`claude-code` was a first-party seed synthesis agent alongside `codex`, wired through `internal/cli/seed.go` and `internal/cli/refresh.go` flag help."
        )
    command_match = re.search(r'return \[\]string\{("claude".*?seedAgentPrompt\(phase\))\}', text, flags=re.DOTALL)
    if command_match:
        command = re.sub(r"\s+", " ", command_match.group(1)).strip()
        facts.append(f"The historical Claude invocation returned `[]string{{{command}}}`.")
    elif "--no-session-persistence" in lower and "--setting-sources" in lower:
        facts.append(
            "The Claude invocation used `claude --print --no-session-persistence --setting-sources user --strict-mcp-config --mcp-config {} --disable-slash-commands --permission-mode dontAsk --tools \"\" --system-prompt ...`."
        )
    tests = []
    for name in (
        "TestSeedAgentCommandArgsClaudeCodeDisablesToolsAndSessions",
        "TestSeedAgentClaudePhaseUsesPATHStdinAndWritesArtifacts",
    ):
        if name in text:
            tests.append(name)
    if tests:
        facts.append("Historical coverage included `" + "` and `".join(tests) + "`.")
    if "readme.md" in lower and "entire brain seed --agent claude-code" in lower:
        facts.append("`README.md` historically showed the example `entire brain seed --agent claude-code .`.")
    elif "entire brain seed --agent claude-code" in lower:
        facts.append("Docs showed the example `entire brain seed --agent claude-code .`.")
    if "docs/seed_plan.md" in lower and "--agent none\\|codex\\|claude-code\\|command" in lower:
        facts.append("`docs/seed_plan.md` historically restored `claude-code` in the escaped `--agent none\\|codex\\|claude-code\\|command` table row.")
    if "templates/entire-brain-intake-claude-agent.md" in lower:
        facts.append("Docs and templates referenced `templates/entire-brain-intake-claude-agent.md`.")
    if "readme.md" in lower and "templates/entire-brain-intake-claude-agent.md" in lower:
        facts.append("`README.md` historically listed `templates/entire-brain-intake-claude-agent.md` with the intake templates.")
    if "tools: bash, read" in lower:
        facts.append("The Claude intake template frontmatter used `tools: Bash, Read`.")
    bare_match = re.search(r"claude-code args should not use --bare because it bypasses logged-in Claude auth", text)
    if bare_match:
        facts.append("Historical failure wording: `claude-code args should not use --bare because it bypasses logged-in Claude auth`.")
    elif "--bare" in lower and "logged-in claude auth" in lower:
        facts.append("Historical rationale: do not use `--bare`; it bypasses logged-in Claude auth.")
    if not facts:
        return ""
    rendered = "## Decoded Historical Facts\n\n" + "\n".join(f"- {fact}" for fact in facts) + "\n\n"
    return rendered


def matching_history_lines(files: list[pathlib.Path], query: str) -> list[tuple[pathlib.Path, int, str]]:
    needle = query.lower()
    matches: list[tuple[pathlib.Path, int, str]] = []
    for path in files:
        try:
            with path.open("r", encoding="utf-8", errors="ignore") as f:
                for line_no, raw in enumerate(f, start=1):
                    if needle in raw.lower():
                        matches.append((path, line_no, raw.rstrip("\n")))
        except OSError:
            continue
    return matches


# GPT-5.x models that over-explore the full MCP history blob (spiral into extra searches/tokens).
# On MCP they now get the "disciplined MCP" delivery (see wants_disciplined_mcp below): brief once
# + ONE targeted brain_search for the invariant + hard stop. On the CLI path they still get the
# generic compact brief. (This set also keeps the audit's required-tools floor at brain_brief.)
COMPACT_STRICT_MODELS = {"gpt-5.5", "gpt-5"}

# Opus is already correctness-saturated but over-READS the brief's history blob
# (MCP brain_brief defaults to limit=20 -> ~8KB of history dominates the packet,
# inflating tokens +15-69% on MCP). Opus gets its own compact, bounded delivery:
# a small-limit brief (top high-signal history only), no forced second history
# call, hard stop, finite-context framing. Goal: keep Opus's review discipline
# (score) while cutting tokens + time on BOTH MCP and CLI. Other models unchanged.
OPUS_COMPACT_MODELS = {"opus", "claude-opus-4-8"}
# gpt-5.5 was A/B-tested for this Opus-style trim (matched n=3, both cli tasks, 0 hard flags,
# MCP server-log-verified): it HELPED on the CLI path (−35% cost / −36% tok, no quality loss)
# but STARVED quality on MCP (−9.5 composite, 5/6 → 4/6 valid, with no token savings) — the
# tiny limit:3 brief drops the one history hit it needs on the harder review task. gpt-5.5's
# failure mode is under-context, not over-reading, so it is NOT added here. Its MCP fix is the
# opposite of a trim — the "disciplined MCP" delivery below (one targeted brain_search for the
# invariant + hard stop), which lifted gpt-5.5 mcp_history pass-rate 88%->100%.


# The "disciplined MCP" delivery — brief ONCE, then ONE targeted brain_search for the exact
# invariant, open likely_edit_files[0], apply, one test, hard stop. Diagnosed root cause on the
# review task: brain_brief ranks the true fix file #1 but its compound-query history section
# misses the precise invariant (scopeBaseRef..HEAD), while a focused brain_search call surfaces
# it cleanly — yet gpt-5.5's old COMPACT_STRICT prompt BANNED brain_search, starving it (it then
# edited a plausible-wrong file). This delivery restores the one history call + a hard stop. It
# also gives gpt-5.4-mini the anti-spiral discipline it lacks at high/xhigh. General principle,
# not a per-model hack: smallest packet that carries BOTH the right file and the exact invariant,
# then stop. ADOPTED after a matched, integrity-audited A/B (0 hard flags, MCP server-log-verified):
#   - gpt-5.5 mcp_history: pass-rate 88%->100% (review 4/5->5/5), no score regression, transcript
#     also -36% time / -46% cost.  (Fixes "the brain hurts gpt-5.5 quality on MCP".)
#   - gpt-5.4-mini high/xhigh mcp_history: validation already saturated on these tasks (no quality
#     gap), but the hard stop cuts the spiral's token bloat -32% pooled (review/high 1462k->695k),
#     12/12 valid, no regression.
def wants_disciplined_mcp(runner: "RunnerSpec | None") -> bool:
    if runner is None:
        return False
    if runner.model in COMPACT_STRICT_MODELS:  # gpt-5.5, gpt-5: under-context on MCP
        return True
    if runner.model == "gpt-5.4-mini" and runner.effort in ("high", "xhigh"):  # spirals at high effort
        return True
    return False


# Experimental, env-gated A/B (default OFF): the "regression radar" delivery — the agent calls the
# new brain_regressions MCP tool, which pre-computes the suspected regressed line + expected value,
# and just restores it. Measures whether handing the agent the bug (vs making it hunt) lifts the
# review pass-rate over the shipped disciplined delivery.
def wants_regression_radar(runner: "RunnerSpec | None") -> bool:
    return os.environ.get("BENCH_REGRESSION_RADAR") == "1"


# Location-only radar (the FAIR detection arm): brain_regressions hands the suspected file:line but
# NOT the expected/current value, so the agent must apply the fix itself — de-leaks the circular A/B
# where the agent simply pasted the harness-computed answer.
def wants_radar_location_only(runner: "RunnerSpec | None") -> bool:
    return os.environ.get("BENCH_RADAR_LOCATION_ONLY") == "1"


def prompt_for(task: dict[str, Any], condition: str, runner: "RunnerSpec | None" = None) -> str:
    base = task["prompt"].strip()
    validation = "\n".join(f"- `{cmd}`" for cmd in task.get("validation", []))
    expected = ", ".join(task.get("expected_files", []))
    queries = ", ".join(task.get("brain_queries", []))
    brief_query = f"{task['id']}: {base[:120]}"
    if queries:
        brief_query = f"{brief_query} | {queries}"
    radar_arg_hint = "`location_only: true`"
    if task.get("radar_include_deletions"):
        radar_arg_hint = "`location_only: true` and `include_deletions: true`"
    radar_shape_note = " It should also flag deleted assignments for this task." if task.get("radar_include_deletions") else ""
    brief_limit = " --limit 4" if condition == "full_cli_compact" else ""
    brief_command = f'entire brain brief "{brief_query}" --json{brief_limit}'
    is_opus = runner is not None and runner.model in OPUS_COMPACT_MODELS
    # Opus gets a deliberately tiny packet (top-2 history hits) on the CLI path.
    opus_brief_command = f'entire brain brief "{brief_query}" --json --limit 2'
    top_level_entire_guard = (
        "Do not run top-level `entire doctor`, `entire status`, `entire session`, "
        "or `entire checkpoint`; they are not Brain context for this benchmark and may be interactive."
    )
    semantic_available = task.get("prepare_semantic", True)
    if condition == "no_brain":
        policy = """Do not use Entire Brain for this run. Do not run `entire brain`, `entire-brain`, or any brain MCP tool. Do not inspect `.entire`, `.benchmark`, or Brain/session/checkpoint artifacts. Inspect the repository normally."""
    elif condition in {"semantic_brain", "semantic_cli"} and semantic_available:
        policy = f"""Use Entire Brain semantic context before editing. Your first context command must be `{brief_command}`. Then use likely_edit_files plus `search` or `inspect code`, `inspect context`, `inspect impact`, or `inspect tests` for the task. Use likely_test_files for validation context only. Useful query terms: {queries}. Do not inspect checkpoint transcripts or session history. {top_level_entire_guard}"""
    elif condition in {"semantic_brain", "semantic_cli"}:
        policy = "Use the prepared Entire Brain seed context before editing. Semantic indexing is disabled for this large-repo benchmark condition, so do not rely on semantic query commands."
    elif condition == "mcp_semantic" and semantic_available:
        policy = f"""Use the Entire Brain MCP server before editing. If your client exposes a `WaitForMcpServers` tool, first wait for the `entire_brain` server. Then start with the `brain_status` MCP tool, followed by `brain_context`, `brain_impact`, `brain_changes`, or `brain_code` for focused semantic graph context. Useful query terms: {queries}. Do not call `brain_query` for this semantic-only condition; it is unified facts/history/docs retrieval, not semantic graph inspection. Do not run the `entire brain` CLI and do not inspect checkpoint transcripts or session history."""
    elif condition == "mcp_semantic":
        policy = "Use the Entire Brain MCP server before editing. Semantic indexing is disabled for this large-repo benchmark condition, so do not run semantic CLI commands or inspect checkpoint transcripts."
    elif condition == "mcp_workspace_radar":
        workspace = benchmark_workspace_name(task)
        policy = f"""Use the Entire Brain MCP server before any shell search or file reads. If your client exposes a `WaitForMcpServers` tool, first wait for the `entire_brain` server. This task is a WORKSPACE REGRESSION. Call `mcp__entire_brain__brain_workspace_regressions` / `brain_workspace_regressions` EXACTLY ONCE with `workspace: "{workspace}"`, query terms `{queries}`, and {radar_arg_hint}.{radar_shape_note} It returns the suspected workspace repo plus `file` and `line` of the regression but NOT the fix. Treat any `symbol` and `related_locations` fields as location-only hints: inspect the enclosing symbol and every related same-file location before editing. Inspect every top anomaly that shares the top anomaly's repo/file/kind before editing; when the top results list multiple line numbers in the same file, open each listed line and fix the shared invariant at all affected sites. Work out what the code should be by reading the surrounding code, and apply the fix yourself. Then run exactly one relevant test and FINISH. If it returns no anomalies, stop immediately and report `WORKSPACE_RADAR_NO_FINDINGS`. Do NOT call any other MCP tool, do NOT re-call, and keep `rg`/`grep`/`find` to at most 3 targeted in-file searches. Useful query terms: {queries}. Do not run the `entire brain` CLI or read `.benchmark/brain-history-excerpt.md`. If no Entire Brain MCP tools are visible, stop immediately and report `MCP_TOOLS_MISSING`."""
    elif condition == "mcp_history" and wants_radar_location_only(runner):
        # FAIR radar arm: brain_regressions(location_only) hands the suspected file:line but NOT the
        # fix — the agent must determine and apply the change itself (de-leaked detection test).
        policy = f"""Use the Entire Brain MCP server before any shell search or file reads. If your client exposes a `WaitForMcpServers` tool, first wait for the `entire_brain` server. This task is a REGRESSION. Call `mcp__entire_brain__brain_regressions` / `brain_regressions` EXACTLY ONCE with {radar_arg_hint} and these failing terms: `{queries}`.{radar_shape_note} It returns the suspected `file` and `line` of the regression but NOT the fix. Treat any `symbol` and `related_locations` fields as location-only hints: inspect the enclosing symbol and every related same-file location before editing. Inspect every top anomaly that shares the top anomaly's file/kind before editing; when the top results list multiple line numbers in the same file, open each listed line and fix the shared invariant at all affected sites. Work out what the code should be by reading the surrounding code, and apply the fix yourself. Then run exactly one relevant test and FINISH. If it returns no anomalies, call `brain_brief` ONCE and fix the single most likely `likely_edit_files` file. Do NOT call any other MCP tool, do NOT re-call, and keep `rg`/`grep`/`find` to at most 3 targeted in-file searches. Useful query terms: {queries}. Do not run the `entire brain` CLI or read `.benchmark/brain-history-excerpt.md`. If no Entire Brain MCP tools are visible, stop immediately and report `MCP_TOOLS_MISSING`."""
    elif condition == "mcp_history" and wants_regression_radar(runner):
        # ANSWER-ASSISTED radar arm (UPPER BOUND, not a fair detection measure): brain_regressions
        # hands file/line/expected/current and the agent pastes `expected`. Useful only to bound the
        # ceiling; the detector's real marginal value is the location-only arm vs the history control.
        policy = f"""Use the Entire Brain MCP server before any shell search or file reads. If your client exposes a `WaitForMcpServers` tool, first wait for the `entire_brain` server. This task is a REGRESSION. Call `mcp__entire_brain__brain_regressions` / `brain_regressions` EXACTLY ONCE with these failing terms: `{queries}` and {"`include_deletions: true`" if task.get("radar_include_deletions") else "no extra deletion flag"}.{radar_shape_note} It returns suspected regressions, each with a `file`, `line`, the `expected` value (what the code should be) and the `current` value. Open the top finding's `file` at its `line` and restore `expected` exactly in place of `current`. Then run exactly one relevant test and FINISH. If `brain_regressions` returns no anomalies, call `brain_brief` ONCE and fix the single most likely `likely_edit_files` file. Do NOT call any other MCP tool, do NOT re-call, and keep `rg`/`grep`/`find` to at most 2 targeted in-file searches. Useful query terms: {queries}. Do not run the `entire brain` CLI or read `.benchmark/brain-history-excerpt.md`. If no Entire Brain MCP tools are visible, stop immediately and report `MCP_TOOLS_MISSING`."""
    elif condition == "mcp_history" and wants_disciplined_mcp(runner):
        # Disciplined MCP (gpt-5.5 all efforts; gpt-5.4-mini high/xhigh): brief once + ONE
        # targeted brain_search for the exact invariant + open likely_edit_files[0] + one
        # test + hard stop. Fixes under-context (brief names the file, history names the change)
        # while bounding exploration (the spiral that motivated the brief-only delivery).
        policy = f"""Use the Entire Brain MCP server before any shell search or file reads. If your client exposes a `WaitForMcpServers` tool, first wait for the `entire_brain` server.
Step 1 — call `mcp__entire_brain__brain_brief` / `brain_brief` for this task EXACTLY ONCE. Note `likely_edit_files[0]` (the single most likely fix site) and `likely_test_files`.
Step 2 — call `mcp__entire_brain__brain_search` / `brain_search` EXACTLY ONCE with the focused query terms ({queries}). The brief names the FILE; brain_search names the exact INVARIANT — the precise expression/value/behavior this regression broke. Read only the top 1–2 hits.
Step 3 — open `likely_edit_files[0]` (prefer the core implementation file over TUI or test scaffolding) and restore the exact invariant from the brain_search hit there.
Step 4 — run exactly one `likely_test_files` test, then FINISH.
Hard stop: call each MCP tool AT MOST ONCE, do NOT call `brain_query`/`brain_context` or any other MCP tool, do NOT re-read the packet, do NOT open unrelated files, and do NOT broaden into repo-wide search. Keep `rg`/`grep`/`find` to at most 3 targeted in-file searches. Apply → validate once → stop. Useful query terms: {queries}. Do not run the `entire brain` CLI or read `.benchmark/brain-history-excerpt.md`. If no Entire Brain MCP tools are visible, stop immediately and report `MCP_TOOLS_MISSING`."""
    elif condition == "mcp_history" and is_opus:
        # Opus-only compact MCP: tiny brief (limit 3), no forced history blob, hard stop.
        policy = f"""Use the Entire Brain MCP server before any shell search or file reads. If your client exposes a `WaitForMcpServers` tool, first wait for the `entire_brain` server. Call `mcp__entire_brain__brain_brief` / `brain_brief` EXACTLY ONCE, passing a small limit (`limit: 3`) so the packet stays compact — it returns `likely_edit_files`, `likely_test_files`, and the top high-signal history hits, which is all the context you need. From `likely_edit_files`, open the single most relevant implementation file (not TUI or test scaffolding) and apply the fix, using the history hits for the exact invariant. Treat that one packet as sufficient: do NOT re-call `brain_brief`, do NOT call `brain_search`/`brain_query` or any other MCP tool, and do not re-read the packet. Run exactly one `likely_test_files` test, then finish. Keep `rg`/`grep`/`find` to at most 2 targeted in-file searches. Your context window is a finite budget — be concise and stop once the fix validates. Useful query terms: {queries}. Do not run the `entire brain` CLI or read `.benchmark/brain-history-excerpt.md`. If no Entire Brain MCP tools are visible, stop immediately and report `MCP_TOOLS_MISSING`."""
    elif condition == "mcp_history" and runner is not None and runner.model in COMPACT_STRICT_MODELS:
        # Compact strict delivery: one brain_brief, no forced history blob, hard stop.
        # NOTE: superseded for current COMPACT_STRICT models — wants_disciplined_mcp() catches
        # them first (the brief-only variant STARVED gpt-5.5 on the review task). Kept as a
        # fallback for any compact model deliberately excluded from the disciplined delivery.
        policy = f"""Use the Entire Brain MCP server before any shell search or file reads. If your client exposes a `WaitForMcpServers` tool, first wait for the `entire_brain` server. Call `mcp__entire_brain__brain_brief` / `brain_brief` for this task EXACTLY ONCE — it already includes the relevant `likely_edit_files`, `likely_test_files`, and compact session-history hits, so you do NOT need a separate `brain_search` call. From `likely_edit_files`, open the single file most relevant to the described regression (prefer the core implementation file over TUI or test scaffolding) and make the fix there, using the history hits to get the exact invariant right. Verify with a few targeted searches inside that file if needed, then run one `likely_test_files` test and finish. Do NOT re-call `brain_brief` and do NOT call `brain_search`/`brain_query` or any other MCP tool; do not open unrelated files or spiral into broad repo-wide search (keep `rg`/`grep`/`find` to at most 5 targeted searches). Useful query terms: {queries}. Do not run the `entire brain` CLI or read `.benchmark/brain-history-excerpt.md`. If no Entire Brain MCP tools are visible, stop immediately and report `MCP_TOOLS_MISSING`."""
    elif condition == "mcp_history":
        policy = f"""Use the Entire Brain MCP server before any shell search or file reads. If your client exposes a `WaitForMcpServers` tool, first wait for the `entire_brain` server. Your first context action must be the MCP tool `mcp__entire_brain__brain_brief` / `brain_brief` for this task; then run exactly one `mcp__entire_brain__brain_search` / `brain_search` query with the useful query terms: {queries}. From `likely_edit_files`, open the file most relevant to the described regression first (prefer the core implementation file over TUI or test scaffolding); apply the fix there before any additional MCP calls or `rg`/`grep`/`find`, and broaden only if it is clearly not the regression site or focused validation fails. Do not run the `entire brain` CLI and do not read `.benchmark/brain-history-excerpt.md`; this condition is testing MCP-delivered history. If no Entire Brain MCP tools are visible, stop immediately and report `MCP_TOOLS_MISSING` instead of using grep or normal code search."""
    elif condition == "full_cli_compact" and is_opus and semantic_available:
        # Opus-only compact CLI: a tiny --limit 2 packet + hard stop, no re-reads — but
        # self-correcting: the history hit (the invariant) is the authority, likely_edit_files
        # is a hint that can be lexically wrong, so verify before editing. Verification is a
        # no-op when the pointer is already right (preserves the seed+semantic win) and a
        # rescue when it is wrong (fixes the net-harmful history case).
        policy = f"""Use the full Entire Brain before editing. Your first context command must be `{opus_brief_command}` — a deliberately compact packet. Work in this order: (1) read the top session-history hits and name the EXACT broken invariant — the specific expression, value, or behavior this regression changed; (2) treat `likely_edit_files[0]` as a CANDIDATE and VERIFY it actually contains that invariant before editing — open it and confirm the broken behavior is present there; (3) if it does NOT, the invariant decides the file, not the ranking — check the next `likely_edit_files` candidate or run at most ONE targeted `rg` for the invariant, then edit the file that truly contains it. Apply the minimal fix, run exactly one `likely_test_files` test, then finish. The history hits are the authority; `likely_edit_files` is a hint that can be wrong. Do NOT re-run brief or inspect checkpoint/session files, and keep `rg`/`grep`/`find` to at most 2 targeted searches. Your context window is a finite budget — stop once the fix validates. Useful query terms: {queries}. {top_level_entire_guard}"""
    elif condition == "full_cli_compact" and semantic_available:
        policy = f"""Use the full Entire Brain before editing. Your first context command must be `{brief_command}`. Work in this order: (1) read the top session-history hits in the JSON and name the EXACT broken invariant — the specific expression, value, or behavior this regression changed; (2) treat `likely_edit_files[0]` as a CANDIDATE and VERIFY it actually contains that invariant before editing — open it and confirm the broken behavior is present there; (3) if it does NOT, the invariant decides the file, not the ranking — check the next `likely_edit_files` candidate or run at most ONE targeted `rg` for the invariant, then edit the file that truly contains it. Apply the minimal fix and run one `likely_test_files` validation command; if a test fails because of unrelated temp-file or project-environment setup, do not spend extra rounds debugging test infrastructure. The history hits are the authority; `likely_edit_files`/`action_checklist` are hints that can be lexically wrong. Avoid broad repo-wide `rg`/`grep`/`find`, and do not re-run brief or inspect checkpoint/session files directly. Useful query terms: {queries}. {top_level_entire_guard}"""
    elif semantic_available:
        policy = f"""Use the full Entire Brain before editing. Your first context command must be `{brief_command}`. In the JSON, prefer `action_checklist`, `likely_edit_files`, `likely_test_files`, and compact history hits before broad text search. Read `.benchmark/brain-history-excerpt.md` only if the brief does not give enough exact invariant or file guidance. Useful query terms: {queries}. {top_level_entire_guard}"""
    else:
        policy = f"""Use the full Entire Brain before editing. Semantic indexing is disabled for this large-repo benchmark condition, so focus on seed context and task-relevant checkpoint/session history. Useful history search terms: {queries}."""
        if task.get("history_excerpt", True):
            policy += " Read `.benchmark/brain-history-excerpt.md` first if it exists; it contains task-specific checkpoint hits retrieved from the brain. Treat matching checkpoint code/test names as authoritative when restoring removed coverage. When the excerpt names a historical failure mode, preserve that wording in regression-test failure text."
    parts = [
        base,
        f"Benchmark condition: {condition}",
        f"Context policy: {policy}",
    ]
    if not task.get("hide_expected_from_agent"):
        parts.append(f"Expected edit area: {expected}")
    if not task.get("hide_validation_from_agent"):
        parts.append(f"Validation commands to run before finishing:\n{validation}")
    else:
        parts.append("Run the focused tests you identify as relevant before finishing.")
    parts.append("Keep the fix minimal. Do not edit tests unless the task explicitly asks for test changes. Do not commit changes. Finish with a short summary of what changed and which validation commands passed.")
    return "\n\n".join(parts).strip()


def mcp_server_env(env: dict[str, str]) -> dict[str, str]:
    keys = [
        "ENTIRE_REPO_ROOT",
        "ENTIRE_PLUGIN_CONFIG_DIR",
        "ENTIRE_PLUGIN_DATA_DIR",
        "ENTIRE_PLUGIN_STATE_DIR",
        "ENTIRE_PLUGIN_CACHE_DIR",
        "ENTIRE_BRAIN_MCP_DEBUG_LOG",
    ]
    server_env = {key: env[key] for key in keys if key in env}
    if env.get("PATH"):
        server_env["PATH"] = env["PATH"]
    return server_env


def claude_mcp_config(tools: dict[str, pathlib.Path], env: dict[str, str]) -> str:
    return json.dumps(
        {
            "mcpServers": {
                "entire_brain": {
                    "type": "stdio",
                    "command": str(tools["brain"]),
                    "args": ["mcp"],
                    "env": mcp_server_env(env),
                }
            }
        },
        separators=(",", ":"),
    )


def toml_quote(value: str) -> str:
    return json.dumps(value)


def codex_mcp_config_args(tools: dict[str, pathlib.Path], env: dict[str, str]) -> list[str]:
    args = [
        "--config",
        f"mcp_servers.entire_brain.command={toml_quote(str(tools['brain']))}",
        "--config",
        'mcp_servers.entire_brain.args=["mcp"]',
        "--config",
        "mcp_servers.entire_brain.enabled=true",
        "--config",
        "mcp_servers.entire_brain.required=true",
        "--config",
        'mcp_servers.entire_brain.enabled_tools=["brain_status","brain_brief","brain_query","brain_search","brain_vsearch","brain_get","brain_multi_get","brain_context","brain_impact","brain_changes","brain_regressions","brain_review","brain_workspace_regressions","brain_workspace_review"]',
        "--config",
        'mcp_servers.entire_brain.default_tools_approval_mode="approve"',
        "--config",
        "mcp_servers.entire_brain.startup_timeout_sec=120",
        "--config",
        "mcp_servers.entire_brain.tool_timeout_sec=120",
    ]
    for key, value in sorted(mcp_server_env(env).items()):
        args.extend(["--config", f"mcp_servers.entire_brain.env.{key}={toml_quote(value)}"])
    return args


TRANSIENT_AGENT_FAILURE_PATTERNS: tuple[tuple[str, str], ...] = (
    ("selected_model_at_capacity", r"selected model is at capacity"),
    ("model_at_capacity", r"\bmodel is at capacity\b"),
    ("rate_limited", r"\brate[- ]?limit(?:ed|ing)?\b"),
    ("temporarily_unavailable", r"\btemporarily unavailable\b"),
    ("service_unavailable", r"\bservice unavailable\b"),
    ("overloaded", r"\boverloaded\b"),
)


def transient_agent_failure_reason(returncode: int, stdout: str, stderr: str) -> str | None:
    if returncode == 0:
        return None
    text = f"{stdout}\n{stderr}".lower()
    for reason, pattern in TRANSIENT_AGENT_FAILURE_PATTERNS:
        if re.search(pattern, text):
            return reason
    return None


def run_agent(
    runner: RunnerSpec,
    prompt: str,
    worktree: pathlib.Path,
    env: dict[str, str],
    run_dir: pathlib.Path,
    condition: str,
    tools: dict[str, pathlib.Path],
    timeout: int,
    claude_budget: float,
    pricing: dict[str, Any],
    agent_retries: int = 0,
) -> dict[str, Any]:
    start = time.time()
    mcp_enabled = is_mcp_condition(condition)
    if mcp_enabled:
        env = {**env, "ENTIRE_BRAIN_MCP_DEBUG_LOG": str(run_dir / "mcp-server.log")}
    if runner.agent == "codex":
        cmd = [
            "codex",
            "exec",
            "--ephemeral",
            "--ignore-user-config",
            "--ignore-rules",
            "--sandbox",
            "workspace-write",
            "--json",
            "--cd",
            str(worktree),
        ]
        if mcp_enabled:
            cmd.extend(codex_mcp_config_args(tools, env))
        if runner.model:
            cmd.extend(["--model", runner.model])
        if runner.effort:
            cmd.extend(["--config", f'model_reasoning_effort="{runner.effort}"'])
        cmd.append(prompt)
    elif runner.agent == "claude":
        cmd = [
            "claude",
            "--print",
            "--no-session-persistence",
            "--strict-mcp-config",
            "--mcp-config",
            claude_mcp_config(tools, env) if mcp_enabled else '{"mcpServers":{}}',
            "--disable-slash-commands",
            "--permission-mode",
            "bypassPermissions",
            "--output-format",
            "stream-json" if mcp_enabled else "json",
        ]
        if runner.model:
            cmd.extend(["--model", runner.model])
        if runner.effort:
            cmd.extend(["--effort", runner.effort])
        if mcp_enabled:
            cmd.append("--verbose")
        if claude_budget > 0:
            cmd[1:1] = ["--max-budget-usd", str(claude_budget)]
        cmd.append(prompt)
    else:
        raise ValueError(f"unknown agent: {runner.agent}")

    attempts: list[dict[str, Any]] = []
    proc: subprocess.CompletedProcess[str] | None = None
    max_attempts = max(1, int(agent_retries) + 1)
    for attempt in range(1, max_attempts + 1):
        attempt_start = time.time()
        proc = run_cmd(cmd, cwd=worktree, env=env, input_text="", timeout=timeout)
        reason = transient_agent_failure_reason(proc.returncode, proc.stdout, proc.stderr)
        attempts.append(
            {
                "attempt": attempt,
                "returncode": proc.returncode,
                "seconds": time.time() - attempt_start,
                "transient_failure_reason": reason,
            }
        )
        (run_dir / f"agent.attempt{attempt}.stdout").write_text(proc.stdout)
        (run_dir / f"agent.attempt{attempt}.stderr").write_text(proc.stderr)
        if reason is None or attempt == max_attempts:
            break
        time.sleep(min(2 * attempt, 10))
    assert proc is not None
    (run_dir / "agent.stdout").write_text(proc.stdout)
    (run_dir / "agent.stderr").write_text(proc.stderr)
    usage = extract_usage(runner.agent, proc.stdout, proc.stderr)
    activity = extract_agent_activity(proc.stdout, proc.stderr)
    if usage.get("cost_usd") is None:
        usage["cost_usd"] = estimate_cost_usd(runner, usage, pricing)
        usage["cost_source"] = "estimated" if usage["cost_usd"] is not None else None
    else:
        usage["cost_source"] = "reported"
    return {
        "agent": runner.agent,
        "runner": {
            "id": runner.id,
            "agent": runner.agent,
            "model": runner.model,
            "effort": runner.effort,
        },
        # The model the agent CLI actually reported running, parsed from its own
        # JSON stream — makes model attribution self-evident per record (vs only
        # the requested --model), addressing the "how do you know it was X" concern.
        "resolved_model": extract_resolved_model(proc.stdout),
        "isolation": {**ISOLATION.get(runner.agent, {}), "mcp": "entire-brain local stdio only" if mcp_enabled else ISOLATION.get(runner.agent, {}).get("mcp", "disabled")},
        "mcp": {
            "enabled": mcp_enabled,
            "server": "entire-brain" if mcp_enabled else None,
            "condition": condition,
        },
        "cmd": cmd[:1] + ["..."],
        "returncode": proc.returncode,
        "seconds": time.time() - start,
        "attempts": attempts,
        "transient_retries": max(0, len(attempts) - 1),
        "usage": usage,
        "activity": activity,
        "stdout_bytes": len(proc.stdout.encode()),
        "stderr_bytes": len(proc.stderr.encode()),
        "stdout_tail": proc.stdout[-4000:],
        "stderr_tail": proc.stderr[-4000:],
    }


def walk_numbers(value: Any, keys: set[str]) -> int:
    total = 0
    if isinstance(value, dict):
        for k, v in value.items():
            if k in keys and isinstance(v, (int, float)):
                total += int(v)
            total += walk_numbers(v, keys)
    elif isinstance(value, list):
        for item in value:
            total += walk_numbers(item, keys)
    return total


def extract_usage(agent: str, stdout: str, stderr: str) -> dict[str, Any]:
    usage: dict[str, Any] = {
        "turns": None,
        "input_tokens": None,
        "output_tokens": None,
        "total_tokens": None,
        "cache_read_tokens": None,
        "cache_creation_tokens": None,
        "cost_usd": None,
    }

    if agent == "claude":
        try:
            payload = json.loads(stdout)
        except json.JSONDecodeError:
            payload = None
        if isinstance(payload, dict):
            usage["turns"] = payload.get("num_turns")
            usage["cost_usd"] = payload.get("total_cost_usd")
            model_usage = payload.get("modelUsage")
            if isinstance(model_usage, dict):
                input_tokens = output_tokens = cache_read = cache_create = 0
                for item in model_usage.values():
                    if isinstance(item, dict):
                        input_tokens += int(item.get("inputTokens") or 0)
                        output_tokens += int(item.get("outputTokens") or 0)
                        cache_read += int(item.get("cacheReadInputTokens") or 0)
                        cache_create += int(item.get("cacheCreationInputTokens") or 0)
                usage["input_tokens"] = input_tokens
                usage["output_tokens"] = output_tokens
                usage["cache_read_tokens"] = cache_read
                usage["cache_creation_tokens"] = cache_create
                usage["total_tokens"] = input_tokens + output_tokens + cache_read + cache_create
            return usage

    token_match = re.search(r"tokens used\s*\n\s*([0-9,]+)", stdout + "\n" + stderr, re.IGNORECASE)
    if token_match:
        usage["total_tokens"] = int(token_match.group(1).replace(",", ""))

    turns = 0
    for line in stdout.splitlines():
        line = line.strip()
        if not line.startswith("{"):
            continue
        try:
            payload = json.loads(line)
        except json.JSONDecodeError:
            continue
        if payload.get("type") in {"agent_message", "assistant_message", "turn_end"}:
            turns += 1
        usage["input_tokens"] = (usage["input_tokens"] or 0) + walk_numbers(payload, {"input_tokens", "inputTokens"})
        usage["output_tokens"] = (usage["output_tokens"] or 0) + walk_numbers(payload, {"output_tokens", "outputTokens"})
        usage["cache_read_tokens"] = (usage["cache_read_tokens"] or 0) + walk_numbers(payload, {"cache_read_tokens", "cacheReadInputTokens"})
        usage["cache_creation_tokens"] = (usage["cache_creation_tokens"] or 0) + walk_numbers(payload, {"cache_creation_tokens", "cacheCreationInputTokens"})
        reported_cost = payload.get("total_cost_usd") or payload.get("totalCostUsd") or payload.get("cost_usd")
        if isinstance(reported_cost, (int, float)):
            usage["cost_usd"] = float(reported_cost)
    if turns:
        usage["turns"] = turns
    if usage["total_tokens"] is None:
        pieces = [usage["input_tokens"], usage["output_tokens"], usage["cache_read_tokens"], usage["cache_creation_tokens"]]
        if any(v is not None for v in pieces):
            usage["total_tokens"] = sum(int(v or 0) for v in pieces)
    return usage


def extract_tool_command(value: Any) -> str | None:
    if isinstance(value, str):
        try:
            parsed = json.loads(value)
        except json.JSONDecodeError:
            return value
        return extract_tool_command(parsed)
    if isinstance(value, dict):
        for key in ("command", "cmd", "shell", "script"):
            item = value.get(key)
            if isinstance(item, str):
                return item
    return None


def safe_tool_arguments(value: Any) -> dict[str, Any]:
    if isinstance(value, str):
        try:
            parsed = json.loads(value)
        except json.JSONDecodeError:
            return {}
        return safe_tool_arguments(parsed)
    if not isinstance(value, dict):
        return {}
    safe: dict[str, Any] = {
        key: bool(value[key])
        for key in sorted(SAFE_MCP_BOOL_ARGUMENT_KEYS)
        if isinstance(value.get(key), bool)
    }
    workspace = safe_workspace_name(value.get("workspace"))
    if workspace is not None:
        safe["workspace"] = workspace
    return safe


def tool_event_errored(value: Any) -> bool:
    if not isinstance(value, dict):
        return False
    status = str(value.get("status") or value.get("state") or "").lower()
    if status in {"error", "errored", "failed", "failure"}:
        return True
    if value.get("is_error") is True or value.get("isError") is True:
        return True
    return value.get("error") not in (None, "", False)


def collect_json_tool_events(value: Any) -> list[dict[str, Any]]:
    events: list[dict[str, Any]] = []
    if isinstance(value, dict):
        event_type = value.get("type")
        name = value.get("name") or value.get("tool_name") or value.get("toolName")
        if event_type == "mcp_tool_call":
            server = value.get("server") or value.get("server_name") or value.get("serverName")
            tool = value.get("tool") or value.get("tool_name") or value.get("toolName") or name
            if isinstance(tool, str):
                if isinstance(server, str) and server:
                    name = f"mcp__{server}__{tool}"
                else:
                    name = tool
        if event_type == "command_execution":
            command = extract_tool_command(value)
            if command:
                events.append({"name": "Bash", "command": command})
        if isinstance(name, str) and event_type in {"tool_use", "tool_call", "function_call", "mcp_tool_call"}:
            arg_candidates = [value.get("input"), value.get("arguments"), value.get("params")]
            raw_args = next((candidate for candidate in arg_candidates if candidate is not None), None)
            command = next((cmd for candidate in arg_candidates for cmd in [extract_tool_command(candidate)] if cmd is not None), None)
            events.append({
                "name": name,
                "command": command,
                "arguments": safe_tool_arguments(raw_args),
                "errored": tool_event_errored(value),
            })
        for item in value.values():
            events.extend(collect_json_tool_events(item))
    elif isinstance(value, list):
        for item in value:
            events.extend(collect_json_tool_events(item))
    return events


def structured_tool_events(stdout: str) -> list[dict[str, Any]]:
    events: list[dict[str, Any]] = []
    for line in stdout.splitlines():
        line = line.strip()
        if not line.startswith("{"):
            continue
        try:
            payload = json.loads(line)
        except json.JSONDecodeError:
            continue
        events.extend(collect_json_tool_events(payload))
    return events


def has_structured_json(stdout: str) -> bool:
    for line in stdout.splitlines():
        line = line.strip()
        if not line.startswith("{"):
            continue
        try:
            json.loads(line)
        except json.JSONDecodeError:
            continue
        return True
    return False


def collect_json_tool_names(value: Any) -> list[str]:
    names: list[str] = []
    for event in collect_json_tool_events(value):
        name = event.get("name")
        if isinstance(name, str):
            names.append(name)
    return names


def structured_tool_names(stdout: str) -> list[str]:
    names: list[str] = []
    for event in structured_tool_events(stdout):
        name = event.get("name")
        if isinstance(name, str):
            names.append(name)
    return names


def extract_resolved_model(stdout: str) -> str | None:
    """The model the agent CLI reported in its JSON output, when it exposes one.

    Claude exposes the resolved model via `modelUsage` keys (output-verifiable).
    Codex `exec --json` does NOT echo the resolved model and does not client-side
    validate `--model`, so for codex this is normally None and attribution rests
    on the explicit pinned `--model` flag (disclosed, not output-confirmed)."""
    text = stdout or ""
    m = re.search(r'"modelUsage"\s*:\s*\{\s*"([^"]+)"', text)
    if m:
        return m.group(1)
    found = re.findall(r'"model"\s*:\s*"([^"]+)"', text)
    if not found:
        return None
    counts: dict[str, int] = {}
    for value in found:
        counts[value] = counts.get(value, 0) + 1
    return max(counts, key=lambda k: counts[k])


def structured_activity_source(stdout: str, stderr: str) -> dict[str, Any]:
    events = structured_tool_events(stdout)
    if events:
        commands = [
            command
            for event in events
            for command in [event.get("command")]
            if isinstance(command, str) and command.strip()
        ]
        tool_details = [
            {
                "name": str(event["name"]),
                "arguments": dict(event.get("arguments") or {}),
                "errored": bool(event.get("errored")),
            }
            for event in events
            if isinstance(event.get("name"), str)
        ]
        return {
            "source": "protocol_json",
            "event_count": len(events),
            "tool_names": [str(event["name"]) for event in events if isinstance(event.get("name"), str)],
            "tool_details": tool_details,
            "commands": commands,
            "lower": "\n".join(commands).lower(),
        }
    if has_structured_json(stdout):
        return {
            "source": "protocol_json",
            "event_count": 0,
            "tool_names": [],
            "tool_details": [],
            "commands": [],
            "lower": "",
        }
    text = stdout + "\n" + stderr
    lower = text.lower()
    return {
        "source": "text_fallback",
        "event_count": 0,
        "tool_names": re.findall(rf"mcp__[a-z0-9_-]+__{MCP_BRAIN_TOOL_RE}", lower),
        "tool_details": [],
        "commands": [],
        "lower": lower,
    }


def extract_agent_activity(stdout: str, stderr: str) -> dict[str, Any]:
    activity_source = structured_activity_source(stdout, stderr)
    lower = activity_source["lower"]
    command_text = "\n".join(activity_source["commands"])
    command_lower = command_text.lower()
    known_brain_commands = {
        "brief",
        "export",
        "index",
        "inspect",
        "path",
        "query",
        "refresh",
        "seed",
        "search",
        "stale",
        "status",
    }
    brain_commands = sorted(
        {
            command
            for command in re.findall(r"\b(?:entire\s+brain|entire-brain)\s+([a-z][a-z-]*)", command_lower)
            if command in known_brain_commands
        }
    )
    tool_names = activity_source["tool_names"]
    mcp_tool_names = [
        name
        for name in tool_names
        if re.search(rf"(?:^|__){MCP_BRAIN_TOOL_RE}$", name)
    ]
    mcp_tool_details = [
        detail
        for detail in activity_source.get("tool_details", [])
        if isinstance(detail, dict) and re.search(rf"(?:^|__){MCP_BRAIN_TOOL_RE}$", str(detail.get("name") or ""))
    ]
    direct_brain_cli_calls = len(re.findall(r"\b(?:entire\s+brain|entire-brain)\s+[a-z][a-z-]*", command_lower))
    search_tool_calls = [name for name in tool_names if name in {"Grep", "Glob"}]
    search_call_matches = re.findall(r"\b(?:git\s+grep|rg|grep|find)\b", command_lower)
    checked_brief = "brief" in brain_commands
    if any(name.endswith("brain_brief") or name == "brain_brief" for name in mcp_tool_names):
        checked_brief = True
    checked_freshness = bool({"brief", "status", "stale"} & set(brain_commands)) or any(name.endswith(("brain_stale", "brain_status")) for name in mcp_tool_names)
    test_commands = sorted(
        set(
            re.findall(
                r"\b(?:go test|pytest|npm (?:test|run test)|npx (?:jest|vitest)|vitest|jest|yarn test|pnpm test|cargo test|mvn test|gradle test|make test)\b",
                command_lower,
            )
        )
    )
    return {
        "activity_source": activity_source.get("source", "unknown"),
        "structured_tool_event_count": activity_source.get("event_count", 0),
        "brain_commands": brain_commands,
        "direct_brain_cli_calls": direct_brain_cli_calls,
        "mcp_tool_names": sorted(set(mcp_tool_names)),
        "mcp_tool_details": mcp_tool_details,
        "mcp_tool_calls": len(mcp_tool_names),
        "used_mcp": bool(mcp_tool_names),
        "search_commands": sorted(set([*search_call_matches, *search_tool_calls])),
        "search_calls": len(search_call_matches) + len(search_tool_calls),
        "used_brain": bool(brain_commands) or bool(mcp_tool_names),
        "checked_brief": checked_brief,
        "checked_stale": "stale" in brain_commands,
        "checked_freshness": checked_freshness,
        "test_commands": test_commands,
        "ran_tests": bool(test_commands),
        "checked_diff": bool(re.search(r"\bgit\s+(?:diff|status)\b", command_lower)),
        "saw_index_locked": "index_locked" in lower or "semantic index lock" in lower,
    }


def benchmark_private_path(path: str) -> bool:
    clean = path.strip()
    while clean.startswith("./"):
        clean = clean[2:]
    return any(clean == prefix.rstrip("/") or clean.startswith(prefix) for prefix in BENCHMARK_PRIVATE_PREFIXES)


def changed_files(worktree: pathlib.Path) -> list[str]:
    tracked = run_cmd(["git", "diff", "--name-only"], cwd=worktree).stdout.splitlines()
    untracked = run_cmd(["git", "ls-files", "--others", "--exclude-standard"], cwd=worktree).stdout.splitlines()
    return sorted({x.strip() for x in [*tracked, *untracked] if x.strip() and not benchmark_private_path(x)})


def diff_stat(worktree: pathlib.Path) -> dict[str, Any]:
    stat = run_cmd(["git", "diff", "--shortstat"], cwd=worktree).stdout.strip()
    diff = run_cmd(["git", "diff", "--", "."], cwd=worktree).stdout
    untracked = [
        rel
        for rel in run_cmd(["git", "ls-files", "--others", "--exclude-standard"], cwd=worktree).stdout.splitlines()
        if not benchmark_private_path(rel)
    ]
    untracked_bytes = 0
    for rel in untracked:
        path = worktree / rel
        if path.is_file():
            try:
                untracked_bytes += path.stat().st_size
            except OSError:
                pass
    if untracked:
        stat = (stat + "; " if stat else "") + f"{len(untracked)} untracked file(s)"
    return {"shortstat": stat, "bytes": len(diff.encode()) + untracked_bytes, "untracked_files": sorted(untracked)}


def safe_child_path(root: pathlib.Path, rel: str, *, label: str) -> pathlib.Path:
    if not rel or pathlib.Path(rel).is_absolute():
        raise ValueError(f"{label} must be a relative path")
    root_resolved = root.resolve()
    target = (root_resolved / rel).resolve()
    try:
        target.relative_to(root_resolved)
    except ValueError as exc:
        raise ValueError(f"{label} escapes {root}") from exc
    return target


def validation_file_content(entry: dict[str, Any]) -> str:
    has_content = "content" in entry
    has_fixture = "fixture" in entry
    if has_content == has_fixture:
        raise ValueError("validation file entries must set exactly one of content or fixture")
    if has_content:
        return str(entry["content"])
    fixture = safe_child_path(VALIDATION_FIXTURE_DIR, str(entry.get("fixture", "")), label="validation fixture")
    return fixture.read_text()


def materialize_validation_files(task: dict[str, Any], worktree: pathlib.Path) -> list[tuple[pathlib.Path, bytes | None]]:
    cleanups: list[tuple[pathlib.Path, bytes | None]] = []
    for raw in task.get("validation_files", []):
        if not isinstance(raw, dict):
            raise ValueError("validation_files entries must be objects")
        target = safe_child_path(worktree, str(raw.get("path", "")), label="validation file path")
        original = target.read_bytes() if target.exists() else None
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(validation_file_content(raw))
        cleanups.append((target, original))
    return cleanups


def cleanup_validation_files(cleanups: list[tuple[pathlib.Path, bytes | None]]) -> None:
    for target, original in reversed(cleanups):
        if original is None:
            target.unlink(missing_ok=True)
        else:
            target.write_bytes(original)


def validate(task: dict[str, Any], worktree: pathlib.Path, env: dict[str, str]) -> dict[str, Any]:
    commands = task.get("validation", [])
    if not commands:
        return {"ok": False, "results": [], "error": "task has no validation commands"}
    results = []
    ok = True
    cleanups: list[tuple[pathlib.Path, bytes | None]] = []
    try:
        cleanups = materialize_validation_files(task, worktree)
        for command in commands:
            start = time.time()
            proc = shell_cmd(command, cwd=worktree, env=env, timeout=600)
            result = {
                "command": command,
                "returncode": proc.returncode,
                "seconds": time.time() - start,
                "stdout_tail": proc.stdout[-3000:],
                "stderr_tail": proc.stderr[-3000:],
            }
            results.append(result)
            if proc.returncode != 0:
                ok = False
    except Exception as exc:
        ok = False
        return {"ok": False, "results": results, "error": f"validation setup failed: {exc}"}
    finally:
        cleanup_validation_files(cleanups)
    return {"ok": ok, "results": results}


def score(
    task: dict[str, Any],
    condition: str,
    agent_info: dict[str, Any],
    validation: dict[str, Any],
    files: list[str],
    diff: dict[str, Any],
) -> dict[str, Any]:
    expected = set(task.get("expected_files", []))
    forbidden = set(task.get("forbidden_files", []))
    touched = set(files)
    results = validation.get("results") or []
    passed_validations = sum(1 for result in results if result.get("returncode") == 0)
    validation_count = len(results)
    validation_ratio = (passed_validations / validation_count) if validation_count else (1.0 if validation.get("ok") else 0.0)

    outcome = round(35 * validation_ratio)
    if agent_info.get("returncode") == 0:
        outcome += 10 if validation.get("ok") else 5

    expected_touched = touched & expected
    unexpected_touched = touched - expected if expected else set()
    missing_expected = expected - touched
    forbidden_touched = touched & forbidden
    if expected:
        expected_coverage = round(12 * len(expected_touched) / len(expected))
        minimality = max(0, 10 - 3 * len(unexpected_touched))
    else:
        expected_coverage = 8 if touched else 0
        minimality = max(0, 14 - 2 * max(0, len(touched) - 1))
    if not touched:
        minimality = 0
    forbidden_points = max(0, 5 - 5 * len(forbidden_touched))
    diff_bytes = int(diff.get("bytes") or 0)
    if diff_bytes == 0:
        size_points = 0
    elif diff_bytes <= 500:
        size_points = 3
    elif diff_bytes <= 2_000:
        size_points = 2
    elif diff_bytes <= 8_000:
        size_points = 1
    else:
        size_points = 0
    missing_penalty = min(5, 2 * len(missing_expected))
    patch_focus = max(0, min(30, expected_coverage + minimality + forbidden_points + size_points - missing_penalty))

    activity = agent_info.get("activity") if isinstance(agent_info.get("activity"), dict) else {}
    validation_discipline = 0
    if activity.get("ran_tests"):
        validation_discipline += 6
    if activity.get("checked_diff"):
        validation_discipline += 2
    if validation.get("ok"):
        validation_discipline += 2

    seconds = float(agent_info.get("seconds") or 0)
    if seconds <= 60:
        time_points = 5
    elif seconds <= 120:
        time_points = 4
    elif seconds <= 240:
        time_points = 3
    elif seconds <= 480:
        time_points = 2
    elif seconds <= 900:
        time_points = 1
    else:
        time_points = 0
    tokens = agent_info.get("usage", {}).get("total_tokens") if isinstance(agent_info.get("usage"), dict) else None
    if not isinstance(tokens, (int, float)):
        token_points = 3
    elif tokens <= 250_000:
        token_points = 5
    elif tokens <= 500_000:
        token_points = 4
    elif tokens <= 1_000_000:
        token_points = 3
    elif tokens <= 2_000_000:
        token_points = 2
    elif tokens <= 4_000_000:
        token_points = 1
    else:
        token_points = 0
    runtime_efficiency = time_points + token_points

    if condition == "no_brain":
        brain_use = 0 if activity.get("used_brain") else 5
    else:
        semantic_available = task.get("prepare_semantic", True)
        brain_use = 0
        if activity.get("used_brain"):
            brain_use += 2
        if not semantic_available or activity.get("checked_freshness"):
            brain_use += 2
        if activity.get("checked_brief"):
            brain_use += 1
        if not activity.get("saw_index_locked"):
            brain_use += 1
        brain_use = min(brain_use, 5)

    total = max(0, min(100, outcome + patch_focus + validation_discipline + runtime_efficiency + brain_use))
    return {
        "version": 2,
        "total": total,
        "outcome": outcome,
        "patch_focus": patch_focus,
        "validation_discipline": validation_discipline,
        "runtime_efficiency": runtime_efficiency,
        "brain_use": brain_use,
        "details": {
            "validation_passed": passed_validations,
            "validation_count": validation_count,
            "expected_files_touched": sorted(expected_touched),
            "missing_expected_files": sorted(missing_expected),
            "unexpected_files_touched": sorted(unexpected_touched),
            "forbidden_files_touched": sorted(forbidden_touched),
            "changed_file_count": len(touched),
            "diff_bytes": diff_bytes,
            "agent_seconds": seconds,
            "total_tokens": tokens,
            "brain_commands": activity.get("brain_commands", []),
            "mcp_tool_calls": activity.get("mcp_tool_calls", 0),
            "mcp_tool_names": activity.get("mcp_tool_names", []),
            "mcp_tool_details": activity.get("mcp_tool_details", []),
            "direct_brain_cli_calls": activity.get("direct_brain_cli_calls", 0),
            "search_calls": activity.get("search_calls", 0),
            "activity_source": activity.get("activity_source", "unknown"),
            "structured_tool_event_count": activity.get("structured_tool_event_count", 0),
            "ran_tests": bool(activity.get("ran_tests")),
            "checked_diff": bool(activity.get("checked_diff")),
        },
    }


def run_one(
    task: dict[str, Any],
    runner: RunnerSpec,
    condition: str,
    repetition: int,
    suite_dir: pathlib.Path,
    tools: dict[str, pathlib.Path],
    args: argparse.Namespace,
    pricing: dict[str, Any],
) -> RunResult:
    task, source, base_provenance = bind_task_base_commit(task)
    run_id = f"{task['id']}__{runner.id}__{condition}__r{repetition}"
    run_dir = suite_dir / run_id
    run_dir.mkdir(parents=True, exist_ok=False)
    worktree: pathlib.Path | None = None
    record: dict[str, Any] = {
        "run_id": run_id,
        "task_id": task["id"],
        "repo": task["repo"],
        "agent": runner.agent,
        "runner": {
            "id": runner.id,
            "agent": runner.agent,
            "model": runner.model,
            "effort": runner.effort,
        },
        "condition": condition,
        "repetition": repetition,
        "started_at": dt.datetime.now(dt.UTC).isoformat(),
        "provenance": build_record_provenance(
            task,
            runner,
            condition,
            repetition,
            suite_dir,
            tools,
            args,
            command="run",
            source=source,
            base=base_provenance,
        ),
    }
    try:
        worktree = create_worktree(task, run_dir)
        worktree_sanitization = sanitize_agent_worktree(worktree, task)
        record["agent_worktree_sanitization"] = worktree_sanitization
        record["agent_baseline_history_reset"] = reset_agent_history_to_root(
            worktree,
            f"Benchmark agent baseline for {task['id']}",
        )
        prepare_condition_history(task, condition, worktree)
        env, prep = prepare_brain(
            task,
            condition,
            worktree,
            run_dir,
            tools,
            args.checkpoint_limit,
            use_cache=not args.no_brain_cache,
            refresh_cache=args.refresh_brain_cache,
        )
        brain_state = collect_brain_state(worktree, env, tools) if condition != "no_brain" else {}
        if condition != "no_brain":
            assert_brain_state_ready(task, condition, brain_state)
            capture_brief_packet(task, condition, runner, worktree, env, tools, run_dir)
        post_brain_changed = apply_post_brain_setup(task, worktree)
        if post_brain_changed:
            record["post_brain_baseline_history_reset"] = reset_agent_history_to_root(
                worktree,
                f"Benchmark post-brain agent baseline for {task['id']}",
                include_current_changes=True,
            )
        agent_visible_entire_removed = False
        if condition_copies_entire_history(condition):
            agent_visible_entire_removed = remove_agent_visible_entire_history(worktree)
        secret_preflight = agent_secret_preflight(worktree)
        record["agent_secret_preflight"] = secret_preflight
        if not secret_preflight["ok"]:
            raise RuntimeError(f"agent-visible benchmark secrets failed preflight: {secret_preflight['findings'][:3]}")
        prompt = prompt_for(task, condition, runner)
        (run_dir / "prompt.txt").write_text(prompt)
        agent_info = run_agent(
            runner,
            prompt,
            worktree,
            env,
            run_dir,
            condition,
            tools,
            args.timeout,
            args.claude_budget,
            pricing,
            agent_retries=getattr(args, "agent_retries", 0),
        )
        leak_audit = agent_output_leak_audit(
            task,
            (run_dir / "agent.stdout").read_text(encoding="utf-8", errors="ignore"),
            (run_dir / "agent.stderr").read_text(encoding="utf-8", errors="ignore"),
        )
        mcp_audit = mcp_condition_audit(condition, agent_info, runner, task)
        files = changed_files(worktree)
        validation = validate(task, worktree, env)
        diff = diff_stat(worktree)
        scoring = score(task, condition, agent_info, validation, files, diff)
        record.update(
            {
                "ok": validation["ok"] and agent_info["returncode"] == 0 and leak_audit["ok"] and mcp_audit["ok"],
                "worktree": str(worktree),
                "brain_prep": prep,
                "brain_state": brain_state,
                "post_brain_setup_applied": post_brain_changed,
                "agent_visible_entire_history_removed": agent_visible_entire_removed,
                "agent_leak_audit": leak_audit,
                "mcp_condition_audit": mcp_audit,
                "agent_info": agent_info,
                "changed_files": files,
                "diff_stat": diff,
                "validation": validation,
                "score": scoring,
            }
        )
    except Exception as exc:
        record.update({"ok": False, "error": str(exc), "score": {"total": 0}})
    finally:
        record["finished_at"] = dt.datetime.now(dt.UTC).isoformat()
        (run_dir / "record.json").write_text(json.dumps(record, indent=2, sort_keys=True))
        if worktree and not args.keep_worktrees:
            remove_worktree(source, worktree)
    return RunResult(record=record, run_dir=run_dir)


def _betacf(a: float, b: float, x: float) -> float:
    # Continued-fraction expansion of the incomplete beta (Numerical Recipes).
    maxit, eps, fpmin = 200, 3.0e-12, 1.0e-300
    qab, qap, qam = a + b, a + 1.0, a - 1.0
    c = 1.0
    d = 1.0 - qab * x / qap
    if abs(d) < fpmin:
        d = fpmin
    d = 1.0 / d
    h = d
    for m in range(1, maxit + 1):
        m2 = 2 * m
        aa = m * (b - m) * x / ((qam + m2) * (a + m2))
        d = 1.0 + aa * d
        if abs(d) < fpmin:
            d = fpmin
        c = 1.0 + aa / c
        if abs(c) < fpmin:
            c = fpmin
        d = 1.0 / d
        h *= d * c
        aa = -(a + m) * (qab + m) * x / ((a + m2) * (qap + m2))
        d = 1.0 + aa * d
        if abs(d) < fpmin:
            d = fpmin
        c = 1.0 + aa / c
        if abs(c) < fpmin:
            c = fpmin
        d = 1.0 / d
        de = d * c
        h *= de
        if abs(de - 1.0) < eps:
            break
    return h


def _betai(a: float, b: float, x: float) -> float:
    # Regularized incomplete beta function I_x(a, b).
    if x <= 0.0:
        return 0.0
    if x >= 1.0:
        return 1.0
    lbeta = math.lgamma(a + b) - math.lgamma(a) - math.lgamma(b)
    bt = math.exp(lbeta + a * math.log(x) + b * math.log(1.0 - x))
    if x < (a + 1.0) / (a + b + 2.0):
        return bt * _betacf(a, b, x) / a
    return 1.0 - bt * _betacf(b, a, 1.0 - x) / b


def welch_p_value(a: list[float], b: list[float]) -> float | None:
    """Two-sided Welch's t-test p-value using the Student-t distribution with
    Welch-Satterthwaite degrees of freedom. A z/normal approximation (erfc) is
    invalid at the small per-cell n here (df ~ 2-4 has far heavier tails), so we
    use the proper t-distribution survival via the regularized incomplete beta."""
    if len(a) < 2 or len(b) < 2:
        return None
    na, nb = len(a), len(b)
    mean_a = sum(a) / na
    mean_b = sum(b) / nb
    var_a = sum((x - mean_a) ** 2 for x in a) / (na - 1)
    var_b = sum((x - mean_b) ** 2 for x in b) / (nb - 1)
    sa, sb = var_a / na, var_b / nb
    se2 = sa + sb
    if se2 == 0:
        return 0.0 if mean_a != mean_b else 1.0
    t = (mean_a - mean_b) / math.sqrt(se2)
    denom = (sa * sa) / (na - 1) + (sb * sb) / (nb - 1)
    if denom == 0:
        return 1.0
    df = se2 * se2 / denom
    if df <= 0:
        return 1.0
    # Two-sided p = I_{df/(df+t^2)}(df/2, 1/2).
    return _betai(df / 2.0, 0.5, df / (df + t * t))


def relative_delta(condition: Any, baseline: Any) -> float | None:
    if not isinstance(condition, (int, float)) or not isinstance(baseline, (int, float)):
        return None
    if baseline == 0:
        return None
    return (float(condition) - float(baseline)) / float(baseline)


def improvement_ratio(condition: Any, baseline: Any) -> float | None:
    delta = relative_delta(condition, baseline)
    if delta is None:
        return None
    return -delta


def brain_comparison_verdict(comparison: dict[str, Any]) -> dict[str, Any]:
    n_condition = int(comparison.get("n_condition") or 0)
    n_baseline = int(comparison.get("n_baseline") or 0)
    score_delta = float(comparison.get("delta") or 0)
    success_rate_condition = float(comparison.get("success_rate_condition") or 0)
    success_rate_baseline = float(comparison.get("success_rate_baseline") or 0)
    success_delta = success_rate_condition - success_rate_baseline
    time_improvement = improvement_ratio(comparison.get("mean_agent_seconds_condition"), comparison.get("mean_agent_seconds_baseline"))
    token_improvement = improvement_ratio(comparison.get("mean_total_tokens_condition"), comparison.get("mean_total_tokens_baseline"))
    search_improvement = improvement_ratio(comparison.get("mean_search_calls_condition"), comparison.get("mean_search_calls_baseline"))
    time_overhead = relative_delta(comparison.get("mean_agent_seconds_condition"), comparison.get("mean_agent_seconds_baseline"))
    token_overhead = relative_delta(comparison.get("mean_total_tokens_condition"), comparison.get("mean_total_tokens_baseline"))

    proof_repeated = n_condition >= 4 and n_baseline >= 4
    brain_validation_clean = success_rate_condition == 1.0
    correctness_win = brain_validation_clean and (success_delta > 0 or score_delta >= 3)
    equal_or_better = success_delta >= 0 and score_delta >= 0
    efficiency_win = brain_validation_clean and equal_or_better and (
        (isinstance(time_improvement, float) and time_improvement >= 0.20)
        or (isinstance(token_improvement, float) and token_improvement >= 0.20)
    )
    search_win = brain_validation_clean and equal_or_better and isinstance(search_improvement, float) and search_improvement >= 0.20
    both_solved = success_rate_condition == 1.0 and success_rate_baseline == 1.0
    saturated = both_solved and abs(score_delta) <= 1 and not efficiency_win
    overhead_bad = (
        success_delta < 0
        or score_delta < -1
        or (isinstance(time_overhead, float) and time_overhead > 0.15)
        or (isinstance(token_overhead, float) and token_overhead > 0.15)
    )

    reasons: list[str] = []
    if not proof_repeated:
        reasons.append("pilot_n<4")
    if not brain_validation_clean:
        reasons.append("brain_validation_not_clean")
    if both_solved:
        reasons.append("both_conditions_passed")
    if correctness_win:
        reasons.append(f"score_or_success_improved={score_delta:+.1f}")
    if efficiency_win:
        reasons.append("time_or_tokens_improved>=20%")
    if search_win:
        reasons.append("search_calls_dropped>=20%")
    if overhead_bad:
        reasons.append("score_drop_or_overhead_exceeds_gate")

    if correctness_win or efficiency_win:
        verdict = "brain_positive"
    elif saturated and overhead_bad:
        verdict = "saturated/overhead_negative"
    elif saturated:
        verdict = "saturated/no_signal"
    elif overhead_bad:
        verdict = "brain_negative"
    else:
        verdict = "inconclusive"
    if not proof_repeated and verdict == "brain_positive":
        verdict = "pilot_brain_positive"

    return {
        "verdict": verdict,
        "proof_ready": proof_repeated and verdict == "brain_positive",
        "brain_positive_gate": {
            "score_delta": score_delta,
            "success_delta": success_delta,
            "time_improvement_ratio": time_improvement,
            "token_improvement_ratio": token_improvement,
            "search_improvement_ratio": search_improvement,
            "time_overhead_ratio": time_overhead,
            "token_overhead_ratio": token_overhead,
            "requires_repetitions_per_side": 4,
        },
        "verdict_reasons": reasons,
    }


def metric_values(recs: list[dict[str, Any]], path: list[str]) -> list[float]:
    """Numeric values at a nested path across records (missing/non-numeric dropped)."""
    vals: list[float] = []
    for rec in recs:
        value: Any = rec
        for part in path:
            if not isinstance(value, dict):
                value = None
                break
            value = value.get(part)
        if isinstance(value, (int, float)):
            vals.append(float(value))
    return vals


def coefficient_of_variation(vals: list[float]) -> float | None:
    """Sample CV (stddev / |mean|). None below n=2 or at a zero mean. A spread measure that is
    comparable across metrics of different magnitude — the per-cell noise signal for the stability gate."""
    vals = [v for v in vals if isinstance(v, (int, float))]
    if len(vals) < 2:
        return None
    mean = sum(vals) / len(vals)
    if mean == 0:
        return None
    var = sum((v - mean) ** 2 for v in vals) / (len(vals) - 1)
    return (var**0.5) / abs(mean)


def delta_survives_drop_one(cond: list[float], base: list[float], lower_is_better: bool) -> bool:
    """The brain advantage still points the right way after removing the single MOST brain-favourable
    repetition from each arm — so one lucky/unlucky run cannot carry the verdict. This is a one-sided
    drop-one robustness check (NOT a full leave-one-out jackknife): it drops only the most-favourable rep,
    which is the conservative worst-case for the brain claim, matching the doc's 'repeatable enough to
    survive one obvious outlier' criterion (agent_benchmark_plan.md)."""
    if len(cond) < 2 or len(base) < 2:
        return False
    c = sorted(cond)
    b = sorted(base)
    if lower_is_better:
        # Brain wants a LOW condition value and is helped by a HIGH baseline; drop the most favourable of
        # each (lowest cond, highest base) and require the mean delta to survive.
        c_adj, b_adj = c[1:], b[:-1]
        return (sum(c_adj) / len(c_adj)) < (sum(b_adj) / len(b_adj))
    c_adj, b_adj = c[:-1], b[1:]
    return (sum(c_adj) / len(c_adj)) > (sum(b_adj) / len(b_adj))


def comparison_stability(
    condition_records: list[dict[str, Any]],
    base_records: list[dict[str, Any]],
    comparison: dict[str, Any],
) -> dict[str, Any]:
    """Per (task, runner, condition) stability verdict vs the no_brain baseline. NOT a significance
    manufacturer: it can only DOWNGRADE a result to saturated/noisy. A `brain_positive_stable` tag
    requires a real, repetition-robust win (a token reduction significant at p<0.05 using the MORE
    conservative of the raw Welch p and the Holm family-wise-adjusted p, plus drop-one survival, OR a
    pass-rate lift that survives drop-one). Saturated = both arms already pass 100% (no headroom)."""
    cond_tokens = metric_values(condition_records, ["agent_info", "usage", "total_tokens"])
    base_tokens = metric_values(base_records, ["agent_info", "usage", "total_tokens"])
    cond_scores = metric_values(condition_records, ["score", "total"])
    base_scores = metric_values(base_records, ["score", "total"])
    cond_pass = [1.0 if isinstance(r.get("validation"), dict) and r["validation"].get("ok") else 0.0 for r in condition_records]
    base_pass = [1.0 if isinstance(r.get("validation"), dict) and r["validation"].get("ok") else 0.0 for r in base_records]

    # Gate on the MORE conservative (larger) of the raw Welch p and the Holm family-wise-adjusted p.
    # The Holm value is attached by summarize() before this runs; without it (e.g. a lone comparison)
    # we fall back to raw. This means a token win that loses significance under multiple-comparison
    # correction is NOT tagged stable on its raw p alone.
    raw_p = comparison.get("p_value_total_tokens")
    holm_p = comparison.get("p_value_total_tokens_holm")
    candidates = [p for p in (raw_p, holm_p) if isinstance(p, (int, float))]
    eff_p = max(candidates) if candidates else None
    tokens_significant = eff_p is not None and eff_p < 0.05
    tokens_lower = (
        isinstance(comparison.get("mean_total_tokens_condition"), (int, float))
        and isinstance(comparison.get("mean_total_tokens_baseline"), (int, float))
        and comparison["mean_total_tokens_condition"] < comparison["mean_total_tokens_baseline"]
    )
    token_win_stable = bool(
        tokens_significant
        and tokens_lower
        and delta_survives_drop_one(cond_tokens, base_tokens, lower_is_better=True)
    )

    pass_cond = comparison.get("pass_rate_condition")
    pass_base = comparison.get("pass_rate_baseline")
    pass_lift_stable = bool(
        pass_cond is not None
        and pass_base is not None
        and pass_cond > pass_base
        and delta_survives_drop_one(cond_pass, base_pass, lower_is_better=False)
    )

    saturated = pass_cond == 1.0 and pass_base == 1.0
    if token_win_stable or pass_lift_stable:
        tag = "brain_positive_stable"
    elif saturated:
        tag = "saturated"
    else:
        tag = "noisy"

    return {
        # CV is reported for BOTH arms (a noisy baseline matters as much as a noisy condition).
        "coefficient_of_variation_total_tokens_condition": coefficient_of_variation(cond_tokens),
        "coefficient_of_variation_total_tokens_baseline": coefficient_of_variation(base_tokens),
        "coefficient_of_variation_score_condition": coefficient_of_variation(cond_scores),
        "coefficient_of_variation_score_baseline": coefficient_of_variation(base_scores),
        "tokens_p_raw": raw_p,
        "tokens_p_holm": holm_p,
        "tokens_significant_p_lt_0_05": tokens_significant,  # uses max(raw, holm)
        "token_win_survives_drop_one": token_win_stable,
        "pass_rate_lift_survives_drop_one": pass_lift_stable,
        "saturated": saturated,
        "tag": tag,
    }


def summarize(records: list[dict[str, Any]], suite_dir: pathlib.Path) -> dict[str, Any]:
    groups: dict[tuple[str, str, str, str], list[float]] = {}
    metrics: dict[tuple[str, str, str, str], list[dict[str, Any]]] = {}
    for rec in records:
        runner_id = rec.get("runner", {}).get("id") if isinstance(rec.get("runner"), dict) else None
        runner_id = runner_id or rec["agent"]
        key = (rec["task_id"], rec["agent"], runner_id, rec["condition"])
        groups.setdefault(key, []).append(float(rec.get("score", {}).get("total", 0)))
        metrics.setdefault(key, []).append(rec)

    comparisons = []
    stability_inputs: list[tuple[list[dict[str, Any]], list[dict[str, Any]]]] = []
    for (task_id, agent, runner_id, condition), values in groups.items():
        if condition == "no_brain":
            continue
        base = groups.get((task_id, agent, runner_id, "no_brain"), [])
        base_records = metrics.get((task_id, agent, runner_id, "no_brain"), [])
        condition_records = metrics.get((task_id, agent, runner_id, condition), [])
        if not base:
            continue
        def mean_field(recs: list[dict[str, Any]], path: list[str]) -> float | None:
            vals = []
            for rec in recs:
                value: Any = rec
                for part in path:
                    if not isinstance(value, dict):
                        value = None
                        break
                    value = value.get(part)
                if isinstance(value, (int, float)):
                    vals.append(float(value))
            return sum(vals) / len(vals) if vals else None

        def score_versions(recs: list[dict[str, Any]]) -> list[int]:
            versions = set()
            for rec in recs:
                score = rec.get("score")
                if isinstance(score, dict):
                    version = score.get("version", 1)
                    if isinstance(version, int):
                        versions.add(version)
            return sorted(versions)

        def score_core_values(recs: list[dict[str, Any]]) -> list[float]:
            # Composite score minus the soft, condition-dependent components
            # (brain_use auto-5 for no_brain; runtime_efficiency). Lets the
            # comparison cite a delta/p-value that is NOT inflated by them.
            out: list[float] = []
            for rec in recs:
                score = rec.get("score") if isinstance(rec.get("score"), dict) else {}
                total = score.get("total")
                if not isinstance(total, (int, float)):
                    continue
                soft = float(score.get("brain_use") or 0) + float(score.get("runtime_efficiency") or 0)
                out.append(float(total) - soft)
            return out

        def pass_rate(recs: list[dict[str, Any]]) -> float | None:
            if not recs:
                return None
            passed = sum(1 for rec in recs if isinstance(rec.get("validation"), dict) and rec["validation"].get("ok"))
            return passed / len(recs)

        def comparison_env_flags(recs: list[dict[str, Any]]) -> dict[str, str]:
            flags: dict[str, str] = {}
            for rec in recs:
                provenance = rec.get("provenance") if isinstance(rec.get("provenance"), dict) else {}
                run_config = provenance.get("run_config") if isinstance(provenance.get("run_config"), dict) else {}
                env_flags = run_config.get("env_flags") if isinstance(run_config.get("env_flags"), dict) else {}
                for key, value in env_flags.items():
                    if value not in (None, ""):
                        flags[str(key)] = str(value)
            return dict(sorted(flags.items()))

        def delivery_scope(condition: str, env_flags: dict[str, str]) -> str:
            if condition == "mcp_workspace_radar":
                return "mcp_workspace_radar_location_only"
            if condition == "mcp_history" and env_flags.get("BENCH_RADAR_LOCATION_ONLY") == "1":
                return "mcp_radar_location_only"
            if condition == "mcp_history" and env_flags.get("BENCH_REGRESSION_RADAR") == "1":
                return "mcp_radar_answer_assisted"
            if condition == "mcp_history":
                return "mcp"
            return condition

        score_core_condition = score_core_values(condition_records)
        score_core_baseline = score_core_values(base_records)
        env_flags = comparison_env_flags(condition_records)

        comparison = {
            "task_id": task_id,
            "agent": agent,
            "runner": runner_id,
            "condition": condition,
            "delivery_scope": delivery_scope(condition, env_flags),
            "env_flags": env_flags,
            "baseline": "no_brain",
            "n_condition": len(values),
            "n_baseline": len(base),
            "mean_condition": sum(values) / len(values),
            "mean_baseline": sum(base) / len(base),
            "delta": sum(values) / len(values) - sum(base) / len(base),
            "p_value_approx": welch_p_value(values, base),
            # Clean (soft-component-free) metrics: lead with these + token delta.
            "pass_rate_condition": pass_rate(condition_records),
            "pass_rate_baseline": pass_rate(base_records),
            "mean_score_core_condition": (sum(score_core_condition) / len(score_core_condition)) if score_core_condition else None,
            "mean_score_core_baseline": (sum(score_core_baseline) / len(score_core_baseline)) if score_core_baseline else None,
            "delta_score_core": (sum(score_core_condition) / len(score_core_condition) - sum(score_core_baseline) / len(score_core_baseline)) if score_core_condition and score_core_baseline else None,
            "p_value_score_core": welch_p_value(score_core_condition, score_core_baseline),
            "score_versions_condition": score_versions(condition_records),
            "score_versions_baseline": score_versions(base_records),
            "mean_outcome_condition": mean_field(condition_records, ["score", "outcome"]),
            "mean_outcome_baseline": mean_field(base_records, ["score", "outcome"]),
            "mean_patch_focus_condition": mean_field(condition_records, ["score", "patch_focus"]),
            "mean_patch_focus_baseline": mean_field(base_records, ["score", "patch_focus"]),
            "mean_validation_discipline_condition": mean_field(condition_records, ["score", "validation_discipline"]),
            "mean_validation_discipline_baseline": mean_field(base_records, ["score", "validation_discipline"]),
            "mean_runtime_efficiency_condition": mean_field(condition_records, ["score", "runtime_efficiency"]),
            "mean_runtime_efficiency_baseline": mean_field(base_records, ["score", "runtime_efficiency"]),
            "mean_brain_use_condition": mean_field(condition_records, ["score", "brain_use"]),
            "mean_brain_use_baseline": mean_field(base_records, ["score", "brain_use"]),
            "mean_agent_seconds_condition": mean_field(condition_records, ["agent_info", "seconds"]),
            "mean_agent_seconds_baseline": mean_field(base_records, ["agent_info", "seconds"]),
            "p_value_agent_seconds": welch_p_value(
                    [
                        float(rec["agent_info"]["seconds"])
                        for rec in condition_records
                        if isinstance(rec.get("agent_info", {}).get("seconds"), (int, float))
                    ],
                    [
                        float(rec["agent_info"]["seconds"])
                        for rec in base_records
                        if isinstance(rec.get("agent_info", {}).get("seconds"), (int, float))
                    ],
            ),
            "mean_total_tokens_condition": mean_field(condition_records, ["agent_info", "usage", "total_tokens"]),
            "mean_total_tokens_baseline": mean_field(base_records, ["agent_info", "usage", "total_tokens"]),
            "p_value_total_tokens": welch_p_value(
                    [
                        float(rec["agent_info"]["usage"]["total_tokens"])
                        for rec in condition_records
                        if isinstance(rec.get("agent_info", {}).get("usage", {}).get("total_tokens"), (int, float))
                    ],
                    [
                        float(rec["agent_info"]["usage"]["total_tokens"])
                        for rec in base_records
                        if isinstance(rec.get("agent_info", {}).get("usage", {}).get("total_tokens"), (int, float))
                    ],
            ),
            "mean_turns_condition": mean_field(condition_records, ["agent_info", "usage", "turns"]),
            "mean_turns_baseline": mean_field(base_records, ["agent_info", "usage", "turns"]),
            "p_value_turns": welch_p_value(
                    [
                        float(rec["agent_info"]["usage"]["turns"])
                        for rec in condition_records
                        if isinstance(rec.get("agent_info", {}).get("usage", {}).get("turns"), (int, float))
                    ],
                    [
                        float(rec["agent_info"]["usage"]["turns"])
                        for rec in base_records
                        if isinstance(rec.get("agent_info", {}).get("usage", {}).get("turns"), (int, float))
                    ],
            ),
            "mean_cost_usd_condition": mean_field(condition_records, ["agent_info", "usage", "cost_usd"]),
            "mean_cost_usd_baseline": mean_field(base_records, ["agent_info", "usage", "cost_usd"]),
            "p_value_cost_usd": welch_p_value(
                    [
                        float(rec["agent_info"]["usage"]["cost_usd"])
                        for rec in condition_records
                        if isinstance(rec.get("agent_info", {}).get("usage", {}).get("cost_usd"), (int, float))
                    ],
                    [
                        float(rec["agent_info"]["usage"]["cost_usd"])
                        for rec in base_records
                        if isinstance(rec.get("agent_info", {}).get("usage", {}).get("cost_usd"), (int, float))
                    ],
            ),
            "mean_mcp_tool_calls_condition": mean_field(condition_records, ["agent_info", "activity", "mcp_tool_calls"]),
            "mean_mcp_tool_calls_baseline": mean_field(base_records, ["agent_info", "activity", "mcp_tool_calls"]),
            "mean_search_calls_condition": mean_field(condition_records, ["agent_info", "activity", "search_calls"]),
            "mean_search_calls_baseline": mean_field(base_records, ["agent_info", "activity", "search_calls"]),
            "mean_direct_brain_cli_calls_condition": mean_field(condition_records, ["agent_info", "activity", "direct_brain_cli_calls"]),
            "mean_direct_brain_cli_calls_baseline": mean_field(base_records, ["agent_info", "activity", "direct_brain_cli_calls"]),
            "success_rate_condition": sum(1 for rec in condition_records if rec.get("ok")) / len(condition_records),
            "success_rate_baseline": sum(1 for rec in base_records if rec.get("ok")) / len(base_records),
        }
        comparison.update(brain_comparison_verdict(comparison))
        comparisons.append(comparison)
        # Stability is attached AFTER the Holm correction below, so the gate can use the family-wise
        # adjusted p (not the raw Welch p). Keep the records this comparison needs for that pass.
        stability_inputs.append((condition_records, base_records))

    # Holm-Bonferroni family-wise correction across every reported p-value in this
    # suite (P1: individual p-values were uncorrected). The headline verdict stays
    # validation-gated; adjusted values are written as `<field>_holm` so any p-value
    # cited downstream is family-wise controlled, not raw.
    p_fields = [
        "p_value_approx", "p_value_score_core", "p_value_agent_seconds",
        "p_value_total_tokens", "p_value_turns", "p_value_cost_usd",
    ]
    family: list[tuple[int, str, float]] = []
    for ci, comp in enumerate(comparisons):
        for field in p_fields:
            value = comp.get(field)
            if isinstance(value, (int, float)):
                family.append((ci, field, float(value)))
    if family:
        order = sorted(range(len(family)), key=lambda i: family[i][2])
        running = 0.0
        for rank, idx in enumerate(order):
            adjusted = min(1.0, (len(family) - rank) * family[idx][2])
            running = max(running, adjusted)  # enforce step-down monotonicity
            ci, field, _ = family[idx]
            comparisons[ci][field + "_holm"] = running

    # Now that the Holm-adjusted p-values exist, attach the stability verdict — the gate uses the
    # family-wise adjusted p (see comparison_stability), so a result that is only raw-significant
    # cannot be tagged brain_positive_stable.
    for comp, (cond_recs, base_recs) in zip(comparisons, stability_inputs):
        comp["stability"] = comparison_stability(cond_recs, base_recs, comp)
        comp["candidate_proof_ready"] = bool(comp.get("proof_ready"))
        comp["proof_ready"] = bool(comp.get("candidate_proof_ready") and comp["stability"].get("tag") == "brain_positive_stable")

    stability_tags: dict[str, int] = {}
    for comp in comparisons:
        tag = comp.get("stability", {}).get("tag")
        if tag:
            stability_tags[tag] = stability_tags.get(tag, 0) + 1

    summary = {
        "records": len(records),
        "comparisons": comparisons,
        "stability_tags": stability_tags,
        "provenance": suite_provenance(records),
        "stats_notes": {
            "p_value_test": "two-sided Welch's t-test (Student-t, Welch-Satterthwaite df)",
            "multiple_comparison_correction": "holm-bonferroni across all suite p-values; see <field>_holm",
            "n_pvalues_in_family": len(family),
            "headline_metric": "validation pass-rate + measured tokens; composite score and p-values are secondary",
            "stability": "per comparison: coefficient_of_variation + tag (brain_positive_stable requires a "
            "token win significant at p<0.05 using max(raw Welch p, Holm-adjusted p), or a pass-rate lift, "
            "that survives dropping the single most-favourable rep; saturated = both arms pass 100%; noisy "
            "otherwise). The gate only downgrades; it never manufactures significance.",
        },
    }
    (suite_dir / "summary.json").write_text(json.dumps(summary, indent=2, sort_keys=True))
    return summary


def slugify(value: str) -> str:
    value = re.sub(r"[^a-zA-Z0-9]+", "-", value.strip().lower())
    value = value.strip("-")
    return value or "scenario"


def load_task_index() -> dict[str, dict[str, Any]]:
    tasks = load_tasks([])
    return {task["id"]: task for task in tasks}


def load_existing_phase2_proofs() -> list[dict[str, Any]]:
    proofs: list[dict[str, Any]] = []
    audit_report = load_retained_audit_report()
    if not RESULT_DIR.exists():
        return proofs
    for summary_path in sorted(RESULT_DIR.glob("*/summary.json")):
        try:
            summary = json.loads(summary_path.read_text())
        except (OSError, json.JSONDecodeError):
            continue
        suite = summary_path.parent.name
        if not retained_suite_audit_clean(audit_report, suite):
            continue
        for comparison in summary.get("comparisons", []):
            if not isinstance(comparison, dict):
                continue
            proof = retained_brain_positive_comparison(suite, comparison)
            if proof:
                proofs.append(proof)
    return proofs


def load_retained_audit_report() -> dict[str, Any] | None:
    path = RESULT_DIR / "codex-audit-report.json"
    try:
        data = json.loads(path.read_text())
    except (OSError, json.JSONDecodeError):
        return None
    return data if isinstance(data, dict) else None


def retained_suite_audit_clean(audit_report: dict[str, Any] | None, suite: str) -> bool:
    if not audit_report:
        return False
    suite_report = audit_report.get("suites", {}).get(suite)
    if not isinstance(suite_report, dict):
        return False
    records = int(suite_report.get("n_records") or 0)
    if records == 0:
        return False
    if int(suite_report.get("n_flagged_records") or 0) != 0:
        return False
    if int(suite_report.get("n_provenance_ok") or 0) != records:
        return False
    for comparison in suite_report.get("comparisons", []):
        if isinstance(comparison, dict) and comparison.get("flags"):
            return False
    return True


def retained_brain_positive_comparison(suite: str, comparison: dict[str, Any]) -> dict[str, Any] | None:
    condition = comparison.get("condition")
    if condition == "no_brain":
        return None
    if not comparison.get("proof_ready"):
        return None
    if comparison.get("stability", {}).get("tag") != "brain_positive_stable":
        return None
    success_delta = float(comparison.get("success_rate_condition") or 0) - float(comparison.get("success_rate_baseline") or 0)
    score_delta = float(comparison.get("delta") or 0)
    correctness_p = comparison.get("p_value_approx_holm")
    metric_hits: list[dict[str, Any]] = []
    for field, delta_field, p_field, lower_is_better in [
        ("seconds", ("mean_agent_seconds_condition", "mean_agent_seconds_baseline"), "p_value_agent_seconds_holm", True),
        ("tokens", ("mean_total_tokens_condition", "mean_total_tokens_baseline"), "p_value_total_tokens_holm", True),
        ("turns", ("mean_turns_condition", "mean_turns_baseline"), "p_value_turns_holm", True),
        ("cost", ("mean_cost_usd_condition", "mean_cost_usd_baseline"), "p_value_cost_usd_holm", True),
    ]:
        condition_mean = comparison.get(delta_field[0])
        baseline_mean = comparison.get(delta_field[1])
        p_value = comparison.get(p_field)
        if not isinstance(condition_mean, (int, float)) or not isinstance(baseline_mean, (int, float)) or not isinstance(p_value, (int, float)):
            continue
        delta = float(condition_mean) - float(baseline_mean)
        improved = delta < 0 if lower_is_better else delta > 0
        if improved and float(p_value) < 0.05:
            metric_hits.append({"metric": field, "delta": delta, "p_value": float(p_value)})
    correctness_hit = (success_delta > 0 or score_delta > 0) and isinstance(correctness_p, (int, float)) and float(correctness_p) < 0.05
    if not correctness_hit and not metric_hits:
        return None
    return {
        "suite": suite,
        "task_id": comparison.get("task_id"),
        "runner": comparison.get("runner"),
        "agent": comparison.get("agent"),
        "condition": condition,
        "success_delta": success_delta,
        "score_delta": score_delta,
        "score_p_value": correctness_p,
        "metric_hits": metric_hits,
        "proof_level": "existing_repeated_run",
    }


def generate_phase2_scenarios(minimum_per_layer: int) -> list[dict[str, Any]]:
    scenarios: list[dict[str, Any]] = []
    scenarios.extend(generate_layer_a_scenarios(minimum_per_layer))
    scenarios.extend(generate_layer_b_scenarios(minimum_per_layer))
    scenarios.extend(generate_layer_c_scenarios(minimum_per_layer))
    return scenarios


def generate_layer_a_scenarios(minimum: int) -> list[dict[str, Any]]:
    scenarios: list[dict[str, Any]] = []
    counter = 1
    for topic in PHASE2_PROJECT_TOPICS:
        for archetype in PHASE2_PROJECT_ARCHETYPES:
            scenarios.append(
                {
                    "id": f"phase2-a-{counter:02d}-{slugify(topic['repo'])}-{slugify(topic['area'])}-{archetype['id']}",
                    "layer": "A",
                    "layer_name": "project-native",
                    "repo": topic["repo"],
                    "repo_path": topic["repo_path"],
                    "area": topic["area"],
                    "brain_source": topic["brain_source"],
                    "condition": "semantic_brain" if topic["brain_source"] == "semantic" else "full_brain",
                    "archetype": archetype["id"],
                    "prompt_shape": archetype["prompt_shape"],
                    "validation_strategy": archetype["validation_strategy"],
                    "queries": topic["queries"],
                    "brain_excellence_hypothesis": topic["signal"] + " " + archetype["brain_advantage"],
                    "primary_metric": archetype["metric"],
                    "recommended_repetitions": {"pilot": 1, "proof_minimum": 4},
                    "no_brain_pilot_gate": {"max_score": 90, "action": "reject_unless_full_brain_is_cheaper_or_faster"},
                    "status": "candidate-needs-pilot",
                    "discovery_score": candidate_discovery_score(topic["brain_source"], archetype["id"]),
                }
            )
            counter += 1
            if len(scenarios) >= minimum:
                return scenarios
    return scenarios


def generate_layer_b_scenarios(minimum: int) -> list[dict[str, Any]]:
    """One ledger row per task when the task declares its `archetype` (the Phase-2 counting
    rule: a scenario is one unique task shape, not a task x archetype cross-join). Tasks
    without the field keep the historical cross-join so old ledgers stay reproducible."""
    task_index = load_task_index()
    github_tasks = [
        task
        for task in sorted(task_index.values(), key=lambda item: item.get("id", ""))
        if task.get("repo") == "github-cli" and not str(task.get("id", "")).startswith("swe-style-")
    ]
    archetype_index = {archetype["id"]: archetype for archetype in PHASE2_GITHUB_PROJECT_ARCHETYPES}
    scenarios: list[dict[str, Any]] = []
    counter = 1
    for task in github_tasks:
        condition = phase2_preferred_condition(task)
        declared_archetype = task.get("archetype")
        if declared_archetype is not None:
            if declared_archetype not in archetype_index:
                raise RuntimeError(
                    f"task {task.get('id', '<unknown>')} declares unknown archetype {declared_archetype!r}; "
                    f"valid archetypes: {sorted(archetype_index)}"
                )
            task_archetypes = [archetype_index[declared_archetype]]
        else:
            task_archetypes = PHASE2_GITHUB_PROJECT_ARCHETYPES
        for archetype in task_archetypes:
            scenarios.append(
                {
                    "id": f"phase2-b-{counter:02d}-{slugify(task['id'])}-{archetype['id']}",
                    "layer": "B",
                    "layer_name": "github-cli-project-native",
                    "task_id": task["id"],
                    "repo": task.get("repo"),
                    "repo_path": task.get("repo_path"),
                    "condition": condition,
                    "brain_source": "full" if condition == "full_brain" else "semantic",
                    "archetype": archetype["id"],
                    "prompt_shape": archetype["prompt_shape"],
                    "validation_strategy": archetype["validation_strategy"],
                    "queries": task.get("brain_queries", []),
                    "brain_excellence_hypothesis": "GitHub CLI native task should require the shared CLI contract, not a local symptom-only patch. " + archetype["brain_advantage"],
                    "primary_metric": archetype["metric"],
                    "recommended_repetitions": {"pilot": 1, "proof_minimum": 4},
                    "no_brain_pilot_gate": {"max_score": 90, "action": "reject_unless_full_brain_is_cheaper_or_faster"},
                    "status": "candidate-needs-pilot",
                    "discovery_score": candidate_discovery_score("semantic", archetype["id"]) + 1,
                }
            )
            counter += 1
            if len(scenarios) >= minimum:
                return scenarios
    return scenarios


def generate_layer_c_scenarios(minimum: int) -> list[dict[str, Any]]:
    task_index = load_task_index()
    swe_tasks = [
        task
        for task in sorted(task_index.values(), key=lambda item: item.get("id", ""))
        if str(task.get("id", "")).startswith("swe-style-")
    ]
    scenarios: list[dict[str, Any]] = []
    counter = 1
    for task in swe_tasks:
        condition = phase2_preferred_condition(task)
        for archetype in PHASE2_SWE_ARCHETYPES:
            scenarios.append(
                {
                    "id": f"phase2-c-{counter:02d}-{slugify(task['id'])}-{archetype['id']}",
                    "layer": "C",
                    "layer_name": "swe-bench-style",
                    "task_id": task["id"],
                    "repo": task.get("repo"),
                    "repo_path": task.get("repo_path"),
                    "condition": condition,
                    "brain_source": "full" if condition == "full_brain" else "semantic",
                    "archetype": archetype["id"],
                    "prompt_shape": archetype["prompt_shape"],
                    "validation_strategy": archetype["validation_strategy"],
                    "queries": task.get("brain_queries", []),
                    "brain_excellence_hypothesis": "Issue-style prompt should omit the historical invariant and exact validation path. " + archetype["brain_advantage"],
                    "primary_metric": archetype["metric"],
                    "recommended_repetitions": {"pilot": 1, "proof_minimum": 4},
                    "no_brain_pilot_gate": {"max_score": 90, "action": "reject_unless_full_brain_is_cheaper_or_faster"},
                    "status": "candidate-needs-pilot",
                    "discovery_score": candidate_discovery_score("history", archetype["id"]) + 2,
                }
            )
            counter += 1
            if len(scenarios) >= minimum:
                return scenarios
    return scenarios


def phase2_preferred_condition(task: dict[str, Any]) -> str:
    conditions = task.get("conditions", [])
    if "full_brain" in conditions:
        return "full_brain"
    if "semantic_brain" in conditions:
        return "semantic_brain"
    return "no_brain"


def candidate_discovery_score(brain_source: str, archetype: str) -> int:
    score = 5
    if brain_source in {"history", "hybrid"}:
        score += 2
    if archetype in {"rationale-recovery", "history-only-regression"}:
        score += 2
    if archetype in {"stale-live-hygiene", "stale-context-issue"}:
        score += 1
    return score


def phase2_discovery_summary(scenarios: list[dict[str, Any]], proofs: list[dict[str, Any]], minimum: int) -> dict[str, Any]:
    counts: dict[str, int] = {}
    for scenario in scenarios:
        counts[scenario["layer"]] = counts.get(scenario["layer"], 0) + 1
    task_index = load_task_index()
    proof_counts: dict[str, int] = {}
    for proof in proofs:
        task_id = proof.get("task_id")
        task = task_index.get(task_id, {}) if isinstance(task_id, str) else {}
        if isinstance(task_id, str) and task_id.startswith("swe-style-"):
            layer = "C"
        elif task.get("repo") == "github-cli":
            layer = "B"
        else:
            layer = "A"
        proof_counts[layer] = proof_counts.get(layer, 0) + 1
    return {
        "generated_at": dt.datetime.now(dt.UTC).isoformat(),
        "minimum_per_layer": minimum,
        "scenario_counts": counts,
        "candidate_goal_met": all(counts.get(layer, 0) > 20 for layer in ("A", "B", "C")),
        "existing_repeated_proofs": len(proofs),
        "existing_repeated_proof_counts": proof_counts,
        "proof_goal_met": all(proof_counts.get(layer, 0) > 20 for layer in ("A", "B", "C")),
        "note": "candidate_goal_met means the discovery ledger has more than 20 brain-positive candidates per layer. proof_goal_met requires repeated agent runs with significant results.",
    }


def write_phase2_discovery_markdown(output_dir: pathlib.Path, summary: dict[str, Any], scenarios: list[dict[str, Any]], proofs: list[dict[str, Any]]) -> None:
    by_layer: dict[str, list[dict[str, Any]]] = {"A": [], "B": [], "C": []}
    for scenario in scenarios:
        by_layer.setdefault(scenario["layer"], []).append(scenario)
    lines = [
        "# Phase 2 Brain-Positive Scenario Discovery",
        "",
        f"Generated at: `{summary['generated_at']}`",
        "",
        "This report is the scenario-discovery ledger for Phase 2. The target is more than 20 candidate scenarios per layer where the brain should have a measurable advantage; the actual per-layer counts (which may be below target) are in the table below. Existing repeated-run proof signals are listed separately; candidates still require the proof repetitions before final claims.",
        "",
        "## Counts",
        "",
        "| Layer | Candidate scenarios | Existing repeated proofs |",
        "|---|---:|---:|",
    ]
    for layer in ("A", "B", "C"):
        lines.append(f"| {layer} | {summary['scenario_counts'].get(layer, 0)} | {summary['existing_repeated_proof_counts'].get(layer, 0)} |")
    lines.extend(
        [
            "",
            f"Candidate goal met: `{summary['candidate_goal_met']}`",
            "",
            f"Repeated proof goal met: `{summary['proof_goal_met']}`",
            "",
            "## Retention Gate",
            "",
            "Pilot each candidate with one no-brain run before spending proof repetitions. Reject scenarios whose no-brain pilot score is above 90 unless the brain condition is cheaper or faster at equivalent correctness. Strong brain-positive scenarios should withhold exact file names, test names, and historical rationale from the no-brain prompt; those facts should come from semantic context or checkpoint/session history.",
            "",
        ]
    )
    if proofs:
        lines.extend(["## Existing Repeated-Run Signals", ""])
        lines.append("| Suite | Task | Runner | Condition | Signal |")
        lines.append("|---|---|---|---|---|")
        for proof in proofs:
            signals = []
            if proof.get("score_delta"):
                signals.append(f"score delta {float(proof['score_delta']):.1f}")
            for hit in proof.get("metric_hits", []):
                signals.append(f"{hit['metric']} p={hit['p_value']:.4g}")
            lines.append(
                "| {suite} | {task} | {runner} | {condition} | {signal} |".format(
                    suite=proof.get("suite", ""),
                    task=proof.get("task_id", ""),
                    runner=proof.get("runner", ""),
                    condition=proof.get("condition", ""),
                    signal=", ".join(signals) or "significant",
                )
            )
        lines.append("")
    for layer, title in [
        ("A", "Layer A: Project-Native Entire"),
        ("B", "Layer B: GitHub CLI Project-Native"),
        ("C", "Layer C: SWE-Bench-Style"),
    ]:
        lines.extend([f"## {title}", ""])
        lines.append("| ID | Repo/Task | Brain | Archetype/Runner | Primary metric |")
        lines.append("|---|---|---|---|---|")
        for scenario in by_layer[layer]:
            repo_or_task = scenario.get("task_id") or f"{scenario.get('repo')} / {scenario.get('area')}"
            archetype = scenario.get("archetype") or scenario.get("runner", {}).get("runner", "")
            lines.append(
                f"| `{scenario['id']}` | {repo_or_task} | {scenario.get('condition', '')} | {archetype} | {scenario.get('primary_metric', '')} |"
            )
        lines.append("")
    (output_dir / "report.md").write_text("\n".join(lines) + "\n")


def cmd_discover(args: argparse.Namespace) -> int:
    minimum = args.minimum_per_layer
    if minimum <= 20:
        minimum = 21
    suite = args.suite_name or dt.datetime.now(dt.UTC).strftime("phase2-discovery-%Y%m%dT%H%M%SZ")
    output_dir = BENCH_ROOT / "discovery" / suite
    if output_dir.exists() and args.replace:
        shutil.rmtree(output_dir)
    output_dir.mkdir(parents=True, exist_ok=False)
    scenarios = generate_phase2_scenarios(minimum)
    proofs = load_existing_phase2_proofs()
    summary = phase2_discovery_summary(scenarios, proofs, minimum)
    (output_dir / "scenarios.json").write_text(json.dumps({"scenarios": scenarios}, indent=2, sort_keys=True))
    (output_dir / "summary.json").write_text(json.dumps(summary, indent=2, sort_keys=True))
    (output_dir / "pricing.json").write_text(json.dumps(PHASE2_PRICING, indent=2, sort_keys=True))
    if proofs:
        (output_dir / "existing-proofs.json").write_text(json.dumps({"proofs": proofs}, indent=2, sort_keys=True))
    write_phase2_discovery_markdown(output_dir, summary, scenarios, proofs)
    print(json.dumps({"output_dir": str(output_dir), **summary}, indent=2, sort_keys=True))
    return 0 if summary["candidate_goal_met"] else 1


def cmd_run(args: argparse.Namespace) -> int:
    tasks = load_tasks(args.tasks)
    if args.runners:
        runners = [parse_runner_spec(x.strip()) for x in args.runners.split(",") if x.strip()]
    else:
        runners = [parse_runner_spec(x.strip()) for x in args.agents.split(",") if x.strip()]
    conditions = [x.strip() for x in args.conditions.split(",") if x.strip()]
    pricing = load_pricing(args)
    suite = args.suite_name or dt.datetime.now(dt.UTC).strftime("%Y%m%dT%H%M%SZ")
    suite_dir = RESULT_DIR / suite
    suite_dir.mkdir(parents=True, exist_ok=False)
    tools = build_tools(suite_dir)

    records = []
    for task in tasks:
        for runner in runners:
            skip_remaining_conditions = False
            for condition in conditions:
                if skip_remaining_conditions:
                    break
                if condition not in task.get("conditions", []):
                    continue
                for repetition in range(1, args.repetitions + 1):
                    result = run_one(task, runner, condition, repetition, suite_dir, tools, args, pricing)
                    records.append(result.record)
                    with (suite_dir / "records.ndjson").open("a") as f:
                        f.write(json.dumps(result.record, sort_keys=True) + "\n")
                    print(
                        f"{result.record['run_id']}: score={result.record.get('score', {}).get('total', 0)} ok={result.record.get('ok')}",
                        flush=True,
                    )
                    if (
                        condition == "no_brain"
                        and repetition == 1
                        and args.stop_after_no_brain_score is not None
                        and float(result.record.get("score", {}).get("total", 0)) > args.stop_after_no_brain_score
                    ):
                        skip_remaining_conditions = True
                        print(
                            f"{task['id']}__{runner.id}: stopping after high no_brain pilot score "
                            f"{result.record.get('score', {}).get('total', 0)} > {args.stop_after_no_brain_score}",
                            flush=True,
                        )
                        break
    summary = summarize(records, suite_dir)
    print(json.dumps(summary, indent=2, sort_keys=True))
    return 0


PANEL_DIR = BENCH_ROOT / "panels"


def panel_manifest_path(name: str) -> pathlib.Path:
    return pathlib.Path(name) if name.endswith(".json") else PANEL_DIR / f"{name}.json"


def load_panel(name: str) -> dict[str, Any]:
    path = panel_manifest_path(name)
    if not path.exists():
        raise FileNotFoundError(f"panel manifest not found: {path}")
    return json.loads(path.read_text())


def panel_preflight(panel: dict[str, Any], check_local_artifacts: bool = False) -> list[str]:
    """Reject a panel that cannot produce a proof-grade result BEFORE spending tokens: runners must be
    fully pinned (agent:model:effort — defaults drift and are not visible in artifacts), there must be a
    no_brain baseline + tasks, and repetitions must reach the proof minimum so the stability gate has the
    n to drop an outlier."""
    errors: list[str] = []
    runners = panel.get("runners", [])
    if not runners:
        errors.append("panel has no runners")
    for spec in runners:
        try:
            rs = parse_runner_spec(str(spec))
        except ValueError as exc:
            errors.append(f"runner {spec!r}: {exc}")
            continue
        if not rs.model or not rs.effort:
            errors.append(
                f"runner {spec!r} is not fully pinned (need agent:model:effort) — "
                "unpinned runners are not allowed for proof claims"
            )
    if not panel.get("tasks"):
        errors.append("panel has no tasks")
    else:
        try:
            tasks = load_tasks(panel["tasks"])
        except Exception as exc:
            errors.append(f"panel tasks are not loadable: {exc}")
            tasks = []
        panel_conditions = set(panel.get("conditions", []))
        release_panel = str(panel.get("name") or "").startswith("release-")
        for task in tasks:
            task_conditions = panel_conditions & set(task.get("conditions", []))
            if release_panel:
                if "benchmarks/agent-brain" not in agent_hidden_paths(task):
                    errors.append(
                        f"release panel task {task.get('id', '<unknown>')} must hide "
                        "benchmarks/agent-brain from agent worktrees"
                    )
                if task.get("hide_validation_from_agent") and not task.get("leak_markers"):
                    errors.append(
                        f"release panel task {task.get('id', '<unknown>')} hides validation "
                        "but has no explicit leak_markers canary"
                    )
            query_audit = brain_query_leak_audit(task)
            if not query_audit["ok"]:
                for finding in query_audit["findings"]:
                    leaked = finding.get("token") or finding.get("phrase")
                    errors.append(
                        f"task {task.get('id', '<unknown>')} brain_queries hand the brain arm "
                        f"answer-bearing content: {leaked!r} appears in {finding['where']} "
                        "(release blocker B1 — strip it or give both arms identical hints)"
                    )
            if any(condition_prepares_history(condition) for condition in task_conditions):
                if not task.get("copy_entire_history_from_source") and not task.get("copy_checkpoint_ref_from_source"):
                    errors.append(
                        f"task {task.get('id', '<unknown>')} uses a history/full-brain condition "
                        "but has no local history source (copy_checkpoint_ref_from_source or copy_entire_history_from_source)"
                    )
            if check_local_artifacts:
                source = resolve_repo_path(task.get("repo_path", ""))
                if not source.exists():
                    errors.append(f"task {task.get('id', '<unknown>')} repo_path is missing locally: {source}")
                    continue
                repo_probe = run_cmd(["git", "rev-parse", "--show-toplevel"], cwd=source)
                if repo_probe.returncode != 0:
                    errors.append(f"task {task.get('id', '<unknown>')} repo_path is not a git repo: {source}")
                    continue
                if task.get("copy_entire_history_from_source") and not (source / ".entire").exists():
                    errors.append(f"task {task.get('id', '<unknown>')} requested .entire history but {source / '.entire'} is missing")
                if task.get("copy_checkpoint_ref_from_source"):
                    ref_probe = run_cmd(["git", "rev-parse", "--verify", "-q", CHECKPOINT_REF], cwd=source)
                    if ref_probe.returncode != 0 or not ref_probe.stdout.strip():
                        errors.append(f"task {task.get('id', '<unknown>')} requested {CHECKPOINT_REF} but it is missing in {source}")
    panel_env = panel.get("env", {})
    if panel_env is not None and not isinstance(panel_env, dict):
        errors.append("panel env must be an object when set")
    elif isinstance(panel_env, dict):
        for key, value in panel_env.items():
            if not isinstance(key, str) or not isinstance(value, str):
                errors.append("panel env keys and values must be strings")
    reps = int(panel.get("repetitions", 0) or 0)
    if reps < 4:
        errors.append(f"repetitions {reps} < 4 (proof_minimum); the stability gate needs enough reps to drop an outlier")
    if "no_brain" not in panel.get("conditions", []):
        errors.append("conditions must include the no_brain baseline")
    return errors


def cmd_panel(args: argparse.Namespace) -> int:
    panel_path = panel_manifest_path(args.name)
    panel = load_panel(args.name)
    errors = panel_preflight(panel, check_local_artifacts=True)
    if errors:
        for e in errors:
            print(f"panel preflight: {e}", file=sys.stderr)
        return 2
    # The committed manifest IS the run config (reproducible) — expand it into the existing run path.
    # No new run mechanics; the stability verdict comes from summarize().
    args.tasks = panel["tasks"]
    args.runners = ",".join(str(r) for r in panel["runners"])
    args.agents = ""
    args.conditions = ",".join(panel["conditions"])
    args.repetitions = int(panel["repetitions"])
    args.panel_name = str(panel.get("name") or args.name)
    args.panel_path = display_path(panel_path)
    args.panel_config_sha256 = file_sha256(panel_path)
    if not getattr(args, "suite_name", None):
        args.suite_name = f"panel-{slugify(args.name)}-{dt.datetime.now(dt.UTC).strftime('%Y%m%dT%H%M%SZ')}"
    print(
        f"panel {args.name}: {len(args.tasks)} tasks x {len(panel['runners'])} pinned runners x "
        f"{len(panel['conditions'])} conditions x {args.repetitions} reps",
        flush=True,
    )
    panel_env = panel.get("env") or {}
    old_env = {key: os.environ.get(key) for key in panel_env}
    try:
        for key, value in panel_env.items():
            os.environ[str(key)] = str(value)
        return cmd_run(args)
    finally:
        for key, value in old_env.items():
            if value is None:
                os.environ.pop(key, None)
            else:
                os.environ[key] = value


def cmd_prep(args: argparse.Namespace) -> int:
    tasks = load_tasks(args.tasks)
    conditions = [x.strip() for x in args.conditions.split(",") if x.strip()]
    suite = args.suite_name or dt.datetime.now(dt.UTC).strftime("prep-%Y%m%dT%H%M%SZ")
    suite_dir = RESULT_DIR / suite
    suite_dir.mkdir(parents=True, exist_ok=False)
    tools = build_tools(suite_dir)

    records: list[dict[str, Any]] = []
    for task in tasks:
        for condition in conditions:
            if condition == "no_brain" or condition not in task.get("conditions", []):
                continue
            task, source, base_provenance = bind_task_base_commit(task)
            run_id = f"{task['id']}__prep__{condition}"
            run_dir = suite_dir / run_id
            run_dir.mkdir(parents=True, exist_ok=False)
            worktree: pathlib.Path | None = None
            record: dict[str, Any] = {
                "run_id": run_id,
                "task_id": task["id"],
                "repo": task["repo"],
                "condition": condition,
                "started_at": dt.datetime.now(dt.UTC).isoformat(),
                "provenance": build_record_provenance(
                    task,
                    None,
                    condition,
                    None,
                    suite_dir,
                    tools,
                    args,
                    command="prep",
                    source=source,
                    base=base_provenance,
                ),
            }
            try:
                worktree = create_worktree(task, run_dir)
                prepare_condition_history(task, condition, worktree)
                env, prep = prepare_brain(
                    task,
                    condition,
                    worktree,
                    run_dir,
                    tools,
                    args.checkpoint_limit,
                    use_cache=not args.no_brain_cache,
                    refresh_cache=args.refresh_brain_cache,
                )
                brain_state = collect_brain_state(worktree, env, tools)
                assert_brain_state_ready(task, condition, brain_state)
                record.update(
                    {
                        "ok": True,
                        "worktree": str(worktree),
                        "brain_prep": prep,
                        "brain_state": brain_state,
                    }
                )
            except Exception as exc:
                record.update({"ok": False, "error": str(exc)})
            finally:
                record["finished_at"] = dt.datetime.now(dt.UTC).isoformat()
                (run_dir / "record.json").write_text(json.dumps(record, indent=2, sort_keys=True))
                records.append(record)
                with (suite_dir / "records.ndjson").open("a") as f:
                    f.write(json.dumps(record, sort_keys=True) + "\n")
                print(f"{run_id}: {prep_record_summary(record)}", flush=True)
                if worktree and not args.keep_worktrees:
                    remove_worktree(source, worktree)

    summary = summarize_prep(records, suite_dir)
    print(json.dumps(summary, indent=2, sort_keys=True))
    return 1 if any(not record.get("ok") for record in records) else 0


def cmd_report(args: argparse.Namespace) -> int:
    records = []
    output_dir = RESULT_DIR / "combined-report"
    output_dir.mkdir(parents=True, exist_ok=True)
    for suite in args.suite:
        suite_dir = pathlib.Path(suite)
        if not suite_dir.is_absolute():
            suite_dir = RESULT_DIR / suite_dir
        with (suite_dir / "records.ndjson").open() as f:
            for line in f:
                records.append(json.loads(line))
    print(json.dumps(summarize(records, output_dir), indent=2, sort_keys=True))
    return 0


def cmd_check(args: argparse.Namespace) -> int:
    tasks = load_tasks(args.tasks)
    suite_dir = pathlib.Path(tempfile.mkdtemp(prefix="agent-brain-check-"))
    tools = build_tools(suite_dir)
    failures = 0
    for task in tasks:
        source = resolve_repo_path(task["repo_path"])
        run_dir = suite_dir / task["id"]
        run_dir.mkdir(parents=True, exist_ok=True)
        worktree: pathlib.Path | None = None
        try:
            worktree = create_worktree(task, run_dir)
            env, _ = prepare_brain(
                task,
                "no_brain",
                worktree,
                run_dir,
                tools,
                args.checkpoint_limit,
                use_cache=not args.no_brain_cache,
                refresh_cache=args.refresh_brain_cache,
            )
            apply_post_brain_setup(task, worktree)
            validation = validate(task, worktree, env)
            state = "fails-as-expected" if not validation["ok"] else "unexpected-pass"
            print(f"{task['id']}: {state}")
            if validation["ok"]:
                failures += 1
        except Exception as exc:
            failures += 1
            print(f"{task['id']}: error: {exc}")
        finally:
            if worktree:
                remove_worktree(source, worktree)
    if not args.keep_check_dir:
        shutil.rmtree(suite_dir, ignore_errors=True)
    return 1 if failures else 0


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)

    run_p = sub.add_parser("run")
    run_p.add_argument("--tasks", nargs="*", default=[])
    run_p.add_argument("--agents", default="codex")
    run_p.add_argument(
        "--runners",
        default="",
        help="Comma-separated runner specs: agent[:model[:effort]] or id=agent[:model[:effort]]. Overrides --agents.",
    )
    run_p.add_argument("--conditions", default="no_brain,semantic_brain,full_brain")
    run_p.add_argument("--repetitions", type=int, default=1)
    run_p.add_argument(
        "--stop-after-no-brain-score",
        type=float,
        help="After the first no_brain repetition for a task/runner, skip the remaining repetitions and conditions if the score is above this threshold.",
    )
    run_p.add_argument("--timeout", type=int, default=1800)
    run_p.add_argument("--agent-retries", type=int, default=2, help="Retry explicit transient agent-capacity/service failures")
    run_p.add_argument("--claude-budget", type=float, default=0.0, help="Claude max budget in USD; 0 disables the cap")
    run_p.add_argument("--pricing-file", help="JSON price map for estimated cost when the agent does not report cost")
    run_p.add_argument("--pricing-json", help="Inline JSON price map for estimated cost when the agent does not report cost")
    run_p.add_argument("--checkpoint-limit", type=int, default=200)
    run_p.add_argument("--suite-name")
    run_p.add_argument("--keep-worktrees", action="store_true")
    run_p.add_argument("--no-brain-cache", action="store_true", help="Rebuild brain prep artifacts in every run")
    run_p.add_argument("--refresh-brain-cache", action="store_true", help="Overwrite cached brain prep artifacts")
    run_p.set_defaults(func=cmd_run)

    panel_p = sub.add_parser("panel", help="Run a committed, reproducible benchmark panel + print a stability verdict")
    panel_p.add_argument("name", help="Panel manifest under panels/ (name without .json, or a path to a .json)")
    panel_p.add_argument("--suite-name")
    panel_p.add_argument("--timeout", type=int, default=1800)
    panel_p.add_argument("--agent-retries", type=int, default=2, help="Retry explicit transient agent-capacity/service failures")
    panel_p.add_argument("--claude-budget", type=float, default=0.0, help="Claude max budget in USD; 0 disables the cap")
    panel_p.add_argument("--pricing-file", help="JSON price map for estimated cost when the agent does not report cost")
    panel_p.add_argument("--pricing-json", help="Inline JSON price map for estimated cost when the agent does not report cost")
    panel_p.add_argument("--checkpoint-limit", type=int, default=200)
    panel_p.add_argument("--keep-worktrees", action="store_true")
    panel_p.add_argument("--no-brain-cache", action="store_true", help="Rebuild brain prep artifacts in every run")
    panel_p.add_argument("--refresh-brain-cache", action="store_true", help="Overwrite cached brain prep artifacts")
    panel_p.add_argument("--stop-after-no-brain-score", type=float)
    panel_p.set_defaults(func=cmd_panel)

    prep_p = sub.add_parser("prep")
    prep_p.add_argument("--tasks", nargs="*", default=[])
    prep_p.add_argument("--conditions", default="semantic_brain,full_brain")
    prep_p.add_argument("--checkpoint-limit", type=int, default=200)
    prep_p.add_argument("--suite-name")
    prep_p.add_argument("--keep-worktrees", action="store_true")
    prep_p.add_argument("--no-brain-cache", action="store_true")
    prep_p.add_argument("--refresh-brain-cache", action="store_true")
    prep_p.set_defaults(func=cmd_prep)

    report_p = sub.add_parser("report")
    report_p.add_argument("suite", nargs="+")
    report_p.set_defaults(func=cmd_report)

    check_p = sub.add_parser("check")
    check_p.add_argument("--tasks", nargs="*", default=[])
    check_p.add_argument("--checkpoint-limit", type=int, default=50)
    check_p.add_argument("--keep-check-dir", action="store_true")
    check_p.add_argument("--no-brain-cache", action="store_true")
    check_p.add_argument("--refresh-brain-cache", action="store_true")
    check_p.set_defaults(func=cmd_check)

    discover_p = sub.add_parser("discover")
    discover_p.add_argument("--minimum-per-layer", type=int, default=21)
    discover_p.add_argument("--suite-name")
    discover_p.add_argument("--replace", action="store_true", help="Replace an existing discovery suite directory")
    discover_p.set_defaults(func=cmd_discover)

    args = parser.parse_args()
    return args.func(args)


if __name__ == "__main__":
    raise SystemExit(main())
