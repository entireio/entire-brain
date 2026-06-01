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
    return run_cmd(["/bin/bash", "-lc", command], cwd=cwd, env=env, timeout=timeout)


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


def create_worktree(task: dict[str, Any], run_dir: pathlib.Path) -> pathlib.Path:
    source = pathlib.Path(task["repo_path"])
    base = task.get("base_commit") or git_head(source)
    worktree = run_dir / "worktree"
    run_cmd(["git", "worktree", "add", "--detach", str(worktree), base], cwd=source, check=True)
    ignore_benchmark_plugin(worktree)
    patch = task.get("setup_patch", "")
    if patch:
        run_cmd(["git", "apply", "-"], cwd=worktree, input_text=patch, check=True)
    apply_replacements(worktree, task.get("setup_replacements", []), "setup")
    for command in task.get("setup_commands", []):
        proc = shell_cmd(command, cwd=worktree, env=os.environ.copy(), timeout=120)
        if proc.returncode != 0:
            raise RuntimeError(
                f"setup command failed ({proc.returncode}): {command}\n"
                f"stdout:\n{proc.stdout}\nstderr:\n{proc.stderr}"
            )
    if patch or task.get("setup_replacements") or task.get("setup_commands"):
        run_cmd(["git", "add", "-A"], cwd=worktree, check=True)
        run_cmd(
            [
                "git",
                "-c",
                "user.name=Entire Brain Benchmark",
                "-c",
                "user.email=benchmark@example.invalid",
                "commit",
                "-m",
                f"Benchmark setup for {task['id']}",
            ],
            cwd=worktree,
            env=benchmark_git_env(),
            check=True,
        )
    return worktree


def ignore_benchmark_plugin(worktree: pathlib.Path) -> None:
    proc = run_cmd(["git", "rev-parse", "--git-path", "info/exclude"], cwd=worktree, check=True)
    raw = proc.stdout.strip()
    exclude_path = pathlib.Path(raw)
    if not exclude_path.is_absolute():
        exclude_path = worktree / exclude_path
    exclude_path.parent.mkdir(parents=True, exist_ok=True)
    existing = exclude_path.read_text() if exclude_path.exists() else ""
    entry = ".benchmark/plugin/"
    if entry not in existing.splitlines():
        suffix = "" if existing.endswith("\n") or not existing else "\n"
        exclude_path.write_text(existing + suffix + entry + "\n")


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
    if changed:
        run_cmd(["git", "add", "-A"], cwd=worktree, check=True)
        run_cmd(
            [
                "git",
                "-c",
                "user.name=Entire Brain Benchmark",
                "-c",
                "user.email=benchmark@example.invalid",
                "commit",
                "-m",
                f"Benchmark post-brain setup for {task['id']}",
            ],
            cwd=worktree,
            env=benchmark_git_env(),
            check=True,
        )
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


def remove_worktree(source: pathlib.Path, worktree: pathlib.Path) -> None:
    run_cmd(["git", "worktree", "remove", "--force", str(worktree)], cwd=source)


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


def brain_cache_payload(
    task: dict[str, Any],
    condition: str,
    worktree: pathlib.Path,
    tools: dict[str, pathlib.Path],
    checkpoint_limit: int,
) -> dict[str, Any]:
    return {
        "schema": 2,
        "repo": task.get("repo"),
        "repo_path": str(pathlib.Path(task["repo_path"]).resolve()),
        "base_ref": task.get("base_commit") or git_head(pathlib.Path(task["repo_path"])),
        "condition": condition,
        "prepare_semantic": bool(task.get("prepare_semantic", True)),
        "checkpoint_limit": checkpoint_limit if condition == "full_brain" else None,
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
    commands = [[str(tools["brain"]), "seed", str(worktree), "--agent", "none", "--force"]]
    if task.get("prepare_semantic", True):
        commands.append([str(tools["brain"]), "index", str(worktree), "--sem-binary", str(tools["entire"]), "--force"])
    if condition == "full_brain":
        commands.insert(0, [str(tools["brain"]), "export", "--checkpoint-limit", str(checkpoint_limit)])
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
    env = plugin_env(run_dir, worktree, tools)
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
        prep["cache"].update(
            {
                "hit": True,
                "source_worktree": meta.get("source_worktree"),
                "created_at": meta.get("created_at"),
            }
        )
        if condition == "full_brain" and task.get("history_excerpt", True):
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
    if condition == "full_brain" and task.get("history_excerpt", True):
        write_history_excerpt(task, worktree)
    return env, prep


def read_json_file(path: pathlib.Path) -> Any:
    with path.open() as f:
        return json.load(f)


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
    state["manifest"] = {
        "schema_version": manifest.get("schema_version"),
        "repo_key": manifest.get("repo_key"),
        "repo_root": manifest.get("repo_root"),
        "has_seed": bool(isinstance(sources, dict) and sources.get("seed")),
        "has_semantic": bool(semantic),
        "has_checkpoints": bool(isinstance(sources, dict) and sources.get("checkpoints")),
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
    for query in queries:
        proc = run_cmd(
            ["git", "grep", "-i", "-F", query, "entire/checkpoints/v1", "--", "*full.jsonl"],
            cwd=worktree,
        )
        if proc.returncode not in (0, 1):
            continue
        for raw in proc.stdout.splitlines():
            lower = raw.lower()
            idx = lower.find(query.lower())
            if idx == -1:
                excerpt = raw[:1200]
            else:
                start = max(0, idx - 450)
                end = min(len(raw), idx + len(query) + 900)
                excerpt = raw[start:end]
            key = excerpt.strip()
            if key and key not in seen:
                seen.add(key)
                score = 0
                for needle in ("Users/thomi/Projects/entire-brain", "internal/cli/", "docs/", "README.md", "templates/"):
                    if needle.lower() in lower:
                        score += 3
                if "apply_patch" in lower or "unified_diff" in lower:
                    score += 2
                if "agent_message" in lower:
                    score += 1
                if "react-split-flap" in lower or "agentviz" in lower:
                    score -= 4
                candidates.append((score, f"- query `{query}`: {excerpt}"))
    if not candidates:
        return
    snippets = [text for _, text in sorted(candidates, key=lambda item: item[0], reverse=True)[:limit]]
    out_dir = worktree / ".benchmark"
    out_dir.mkdir(exist_ok=True)
    (out_dir / "brain-history-excerpt.md").write_text(
        "# Retrieved Checkpoint History Excerpt\n\n"
        "This file is generated by the benchmark from Entire v1 checkpoints for the full-brain condition.\n\n"
        + "\n\n".join(snippets)
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


def prompt_for(task: dict[str, Any], condition: str) -> str:
    base = task["prompt"].strip()
    validation = "\n".join(f"- `{cmd}`" for cmd in task.get("validation", []))
    expected = ", ".join(task.get("expected_files", []))
    queries = ", ".join(task.get("brain_queries", []))
    semantic_available = task.get("prepare_semantic", True)
    if condition == "no_brain":
        policy = """Do not use Entire Brain for this run. Do not run `entire brain`, `entire-brain`, or any brain MCP tool. Inspect the repository normally."""
    elif condition == "semantic_brain" and semantic_available:
        policy = f"""Use Entire Brain semantic context before editing. Start with `entire brain stale --json`, then use `query`, `context`, `impact`, or `tests` for the task. Useful query terms: {queries}. Do not inspect checkpoint transcripts or session history."""
    elif condition == "semantic_brain":
        policy = "Use the prepared Entire Brain seed context before editing. Semantic indexing is disabled for this large-repo benchmark condition, so do not rely on semantic query commands."
    elif semantic_available:
        policy = f"""Use the full Entire Brain before editing. Start with `entire brain stale --json`, use semantic commands for code context, and inspect task-relevant checkpoint/session history if it can explain the behavior. Useful query terms: {queries}."""
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
    parts.append("Keep the fix minimal. Do not commit changes. Finish with a short summary of what changed and which validation commands passed.")
    return "\n\n".join(parts).strip()


def run_agent(
    runner: RunnerSpec,
    prompt: str,
    worktree: pathlib.Path,
    env: dict[str, str],
    run_dir: pathlib.Path,
    timeout: int,
    claude_budget: float,
    pricing: dict[str, Any],
) -> dict[str, Any]:
    start = time.time()
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
            '{"mcpServers":{}}',
            "--disable-slash-commands",
            "--permission-mode",
            "bypassPermissions",
            "--output-format",
            "json",
        ]
        if runner.model:
            cmd.extend(["--model", runner.model])
        if runner.effort:
            cmd.extend(["--effort", runner.effort])
        if claude_budget > 0:
            cmd[1:1] = ["--max-budget-usd", str(claude_budget)]
        cmd.append(prompt)
    else:
        raise ValueError(f"unknown agent: {runner.agent}")

    proc = run_cmd(cmd, cwd=worktree, env=env, timeout=timeout)
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
        "isolation": ISOLATION.get(runner.agent, {}),
        "cmd": cmd[:1] + ["..."],
        "returncode": proc.returncode,
        "seconds": time.time() - start,
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
    if turns:
        usage["turns"] = turns
    if usage["total_tokens"] is None:
        pieces = [usage["input_tokens"], usage["output_tokens"], usage["cache_read_tokens"], usage["cache_creation_tokens"]]
        if any(v is not None for v in pieces):
            usage["total_tokens"] = sum(int(v or 0) for v in pieces)
    return usage


def extract_agent_activity(stdout: str, stderr: str) -> dict[str, Any]:
    text = stdout + "\n" + stderr
    lower = text.lower()
    brain_commands = sorted(set(re.findall(r"\b(?:entire\s+brain|entire-brain)\s+([a-z][a-z-]*)", lower)))
    test_commands = sorted(
        set(
            re.findall(
                r"\b(?:go test|pytest|npm (?:test|run test)|yarn test|pnpm test|cargo test|mvn test|gradle test|make test)\b",
                lower,
            )
        )
    )
    return {
        "brain_commands": brain_commands,
        "used_brain": bool(brain_commands),
        "checked_stale": "stale" in brain_commands,
        "test_commands": test_commands,
        "ran_tests": bool(test_commands),
        "checked_diff": bool(re.search(r"\bgit\s+(?:diff|status)\b", lower)),
        "saw_index_locked": "index_locked" in lower or "semantic index lock" in lower,
    }


def changed_files(worktree: pathlib.Path) -> list[str]:
    out = run_cmd(["git", "diff", "--name-only"], cwd=worktree).stdout
    return sorted(x for x in out.splitlines() if x.strip())


def diff_stat(worktree: pathlib.Path) -> dict[str, Any]:
    stat = run_cmd(["git", "diff", "--shortstat"], cwd=worktree).stdout.strip()
    diff = run_cmd(["git", "diff", "--", "."], cwd=worktree).stdout
    return {"shortstat": stat, "bytes": len(diff.encode())}


def validate(task: dict[str, Any], worktree: pathlib.Path, env: dict[str, str]) -> dict[str, Any]:
    results = []
    ok = True
    for command in task.get("validation", []):
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
        if not semantic_available or activity.get("checked_stale"):
            brain_use += 2
        if not activity.get("saw_index_locked"):
            brain_use += 1

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
    run_id = f"{task['id']}__{runner.id}__{condition}__r{repetition}"
    run_dir = suite_dir / run_id
    run_dir.mkdir(parents=True, exist_ok=False)
    source = pathlib.Path(task["repo_path"])
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
    }
    try:
        worktree = create_worktree(task, run_dir)
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
        post_brain_changed = apply_post_brain_setup(task, worktree)
        prompt = prompt_for(task, condition)
        (run_dir / "prompt.txt").write_text(prompt)
        agent_info = run_agent(runner, prompt, worktree, env, run_dir, args.timeout, args.claude_budget, pricing)
        files = changed_files(worktree)
        validation = validate(task, worktree, env)
        diff = diff_stat(worktree)
        scoring = score(task, condition, agent_info, validation, files, diff)
        record.update(
            {
                "ok": validation["ok"] and agent_info["returncode"] == 0,
                "worktree": str(worktree),
                "brain_prep": prep,
                "post_brain_setup_applied": post_brain_changed,
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


def welch_p_value(a: list[float], b: list[float]) -> float | None:
    if len(a) < 2 or len(b) < 2:
        return None
    mean_a = sum(a) / len(a)
    mean_b = sum(b) / len(b)
    var_a = sum((x - mean_a) ** 2 for x in a) / (len(a) - 1)
    var_b = sum((x - mean_b) ** 2 for x in b) / (len(b) - 1)
    se = math.sqrt(var_a / len(a) + var_b / len(b))
    if se == 0:
        return 0.0 if mean_a != mean_b else 1.0
    z = abs(mean_a - mean_b) / se
    return math.erfc(z / math.sqrt(2))


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

        comparisons.append(
            {
                "task_id": task_id,
                "agent": agent,
                "runner": runner_id,
                "condition": condition,
                "baseline": "no_brain",
                "n_condition": len(values),
                "n_baseline": len(base),
                "mean_condition": sum(values) / len(values),
                "mean_baseline": sum(base) / len(base),
                "delta": sum(values) / len(values) - sum(base) / len(base),
                "p_value_approx": welch_p_value(values, base),
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
                "success_rate_condition": sum(1 for rec in condition_records if rec.get("ok")) / len(condition_records),
                "success_rate_baseline": sum(1 for rec in base_records if rec.get("ok")) / len(base_records),
            }
        )
    summary = {"records": len(records), "comparisons": comparisons}
    (suite_dir / "summary.json").write_text(json.dumps(summary, indent=2, sort_keys=True))
    return summary


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
            for condition in conditions:
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
    summary = summarize(records, suite_dir)
    print(json.dumps(summary, indent=2, sort_keys=True))
    return 0


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
            run_id = f"{task['id']}__prep__{condition}"
            run_dir = suite_dir / run_id
            run_dir.mkdir(parents=True, exist_ok=False)
            source = pathlib.Path(task["repo_path"])
            worktree: pathlib.Path | None = None
            record: dict[str, Any] = {
                "run_id": run_id,
                "task_id": task["id"],
                "repo": task["repo"],
                "condition": condition,
                "started_at": dt.datetime.now(dt.UTC).isoformat(),
            }
            try:
                worktree = create_worktree(task, run_dir)
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
                record.update(
                    {
                        "ok": True,
                        "worktree": str(worktree),
                        "brain_prep": prep,
                        "brain_state": collect_brain_state(worktree, env, tools),
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
        source = pathlib.Path(task["repo_path"])
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
    run_p.add_argument("--timeout", type=int, default=1800)
    run_p.add_argument("--claude-budget", type=float, default=0.0, help="Claude max budget in USD; 0 disables the cap")
    run_p.add_argument("--pricing-file", help="JSON price map for estimated cost when the agent does not report cost")
    run_p.add_argument("--pricing-json", help="Inline JSON price map for estimated cost when the agent does not report cost")
    run_p.add_argument("--checkpoint-limit", type=int, default=200)
    run_p.add_argument("--suite-name")
    run_p.add_argument("--keep-worktrees", action="store_true")
    run_p.add_argument("--no-brain-cache", action="store_true", help="Rebuild brain prep artifacts in every run")
    run_p.add_argument("--refresh-brain-cache", action="store_true", help="Overwrite cached brain prep artifacts")
    run_p.set_defaults(func=cmd_run)

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

    args = parser.parse_args()
    return args.func(args)


if __name__ == "__main__":
    raise SystemExit(main())
