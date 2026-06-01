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


@dataclass
class RunResult:
    record: dict[str, Any]
    run_dir: pathlib.Path


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


def create_worktree(task: dict[str, Any], run_dir: pathlib.Path) -> pathlib.Path:
    source = pathlib.Path(task["repo_path"])
    base = task.get("base_commit") or git_head(source)
    worktree = run_dir / "worktree"
    run_cmd(["git", "worktree", "add", "--detach", str(worktree), base], cwd=source, check=True)
    patch = task.get("setup_patch", "")
    if patch:
        run_cmd(["git", "apply", "-"], cwd=worktree, input_text=patch, check=True)
    for replacement in task.get("setup_replacements", []):
        rel = replacement["path"]
        path = worktree / rel
        data = path.read_text()
        old = replacement["old"]
        new = replacement["new"]
        count = data.count(old)
        if count != 1:
            raise RuntimeError(f"setup replacement for {rel} matched {count} times, expected 1")
        path.write_text(data.replace(old, new, 1))
    if patch or task.get("setup_replacements"):
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
            check=True,
        )
    return worktree


def remove_worktree(source: pathlib.Path, worktree: pathlib.Path) -> None:
    run_cmd(["git", "worktree", "remove", "--force", str(worktree)], cwd=source)


def plugin_env(run_dir: pathlib.Path, worktree: pathlib.Path, tools: dict[str, pathlib.Path]) -> dict[str, str]:
    env = os.environ.copy()
    env.update(
        {
            "PATH": f"{tools['bin']}:{env.get('PATH', '')}",
            "ENTIRE_REPO_ROOT": str(worktree),
            "ENTIRE_PLUGIN_CONFIG_DIR": str(run_dir / "plugin" / "config"),
            "ENTIRE_PLUGIN_DATA_DIR": str(run_dir / "plugin" / "data"),
            "ENTIRE_PLUGIN_STATE_DIR": str(run_dir / "plugin" / "state"),
            "ENTIRE_PLUGIN_CACHE_DIR": str(run_dir / "plugin" / "cache"),
        }
    )
    return env


def prepare_brain(
    task: dict[str, Any],
    condition: str,
    worktree: pathlib.Path,
    run_dir: pathlib.Path,
    tools: dict[str, pathlib.Path],
    checkpoint_limit: int,
) -> tuple[dict[str, str], dict[str, Any]]:
    env = plugin_env(run_dir, worktree, tools)
    prep: dict[str, Any] = {"condition": condition, "commands": []}
    if condition == "no_brain":
        return env, prep

    commands = [[str(tools["brain"]), "seed", str(worktree), "--agent", "none", "--force"]]
    if task.get("prepare_semantic", True):
        commands.append([str(tools["brain"]), "index", str(worktree), "--sem-binary", str(tools["entire"]), "--force"])
    if condition == "full_brain":
        commands.insert(0, [str(tools["brain"]), "export", "--checkpoint-limit", str(checkpoint_limit)])

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
    return env, prep


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
        policy = "Use the full Entire Brain before editing. Semantic indexing is disabled for this large-repo benchmark condition, so focus on seed context and task-relevant checkpoint/session history."
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
    agent: str,
    prompt: str,
    worktree: pathlib.Path,
    env: dict[str, str],
    run_dir: pathlib.Path,
    timeout: int,
    claude_budget: float,
) -> dict[str, Any]:
    start = time.time()
    if agent == "codex":
        cmd = [
            "codex",
            "exec",
            "--ephemeral",
            "--sandbox",
            "workspace-write",
            "--json",
            "--cd",
            str(worktree),
            prompt,
        ]
    elif agent == "claude":
        cmd = [
            "claude",
            "--print",
            "--permission-mode",
            "bypassPermissions",
            "--output-format",
            "json",
            prompt,
        ]
        if claude_budget > 0:
            cmd[1:1] = ["--max-budget-usd", str(claude_budget)]
    else:
        raise ValueError(f"unknown agent: {agent}")

    proc = run_cmd(cmd, cwd=worktree, env=env, timeout=timeout)
    (run_dir / "agent.stdout").write_text(proc.stdout)
    (run_dir / "agent.stderr").write_text(proc.stderr)
    usage = extract_usage(agent, proc.stdout, proc.stderr)
    return {
        "agent": agent,
        "cmd": cmd[:1] + ["..."],
        "returncode": proc.returncode,
        "seconds": time.time() - start,
        "usage": usage,
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


def score(task: dict[str, Any], condition: str, agent_info: dict[str, Any], validation: dict[str, Any], files: list[str]) -> dict[str, Any]:
    expected = set(task.get("expected_files", []))
    forbidden = set(task.get("forbidden_files", []))
    touched = set(files)

    correctness = 50 if validation["ok"] and agent_info["returncode"] == 0 else 0
    locality = 0
    if touched:
        if touched <= expected:
            locality = 20
        elif touched & expected:
            locality = 10
    if touched & forbidden:
        locality = max(0, locality - 20)
    validation_quality = 15 if validation["ok"] else 0
    efficiency = max(0, 10 - int(agent_info["seconds"] // 300))
    brain_hygiene = 0
    if condition == "no_brain":
        brain_hygiene = 5
    else:
        text = (agent_info.get("stdout_tail") or "") + "\n" + (agent_info.get("stderr_tail") or "")
        if "brain" in text.lower() or "entire" in text.lower():
            brain_hygiene = 5
    total = correctness + locality + validation_quality + efficiency + brain_hygiene
    return {
        "total": total,
        "correctness": correctness,
        "locality": locality,
        "validation_quality": validation_quality,
        "efficiency": efficiency,
        "brain_hygiene": brain_hygiene,
    }


def run_one(
    task: dict[str, Any],
    agent: str,
    condition: str,
    repetition: int,
    suite_dir: pathlib.Path,
    tools: dict[str, pathlib.Path],
    args: argparse.Namespace,
) -> RunResult:
    run_id = f"{task['id']}__{agent}__{condition}__r{repetition}"
    run_dir = suite_dir / run_id
    run_dir.mkdir(parents=True, exist_ok=False)
    source = pathlib.Path(task["repo_path"])
    worktree: pathlib.Path | None = None
    record: dict[str, Any] = {
        "run_id": run_id,
        "task_id": task["id"],
        "repo": task["repo"],
        "agent": agent,
        "condition": condition,
        "repetition": repetition,
        "started_at": dt.datetime.now(dt.UTC).isoformat(),
    }
    try:
        worktree = create_worktree(task, run_dir)
        env, prep = prepare_brain(task, condition, worktree, run_dir, tools, args.checkpoint_limit)
        prompt = prompt_for(task, condition)
        (run_dir / "prompt.txt").write_text(prompt)
        agent_info = run_agent(agent, prompt, worktree, env, run_dir, args.timeout, args.claude_budget)
        files = changed_files(worktree)
        validation = validate(task, worktree, env)
        scoring = score(task, condition, agent_info, validation, files)
        record.update(
            {
                "ok": validation["ok"] and agent_info["returncode"] == 0,
                "worktree": str(worktree),
                "brain_prep": prep,
                "agent_info": agent_info,
                "changed_files": files,
                "diff_stat": diff_stat(worktree),
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
    groups: dict[tuple[str, str, str], list[float]] = {}
    metrics: dict[tuple[str, str, str], list[dict[str, Any]]] = {}
    for rec in records:
        key = (rec["task_id"], rec["agent"], rec["condition"])
        groups.setdefault(key, []).append(float(rec.get("score", {}).get("total", 0)))
        metrics.setdefault(key, []).append(rec)

    comparisons = []
    for (task_id, agent, condition), values in groups.items():
        if condition == "no_brain":
            continue
        base = groups.get((task_id, agent, "no_brain"), [])
        base_records = metrics.get((task_id, agent, "no_brain"), [])
        condition_records = metrics.get((task_id, agent, condition), [])
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

        comparisons.append(
            {
                "task_id": task_id,
                "agent": agent,
                "condition": condition,
                "baseline": "no_brain",
                "n_condition": len(values),
                "n_baseline": len(base),
                "mean_condition": sum(values) / len(values),
                "mean_baseline": sum(base) / len(base),
                "delta": sum(values) / len(values) - sum(base) / len(base),
                "p_value_approx": welch_p_value(values, base),
                "mean_agent_seconds_condition": mean_field(condition_records, ["agent_info", "seconds"]),
                "mean_agent_seconds_baseline": mean_field(base_records, ["agent_info", "seconds"]),
                "mean_total_tokens_condition": mean_field(condition_records, ["agent_info", "usage", "total_tokens"]),
                "mean_total_tokens_baseline": mean_field(base_records, ["agent_info", "usage", "total_tokens"]),
                "mean_turns_condition": mean_field(condition_records, ["agent_info", "usage", "turns"]),
                "mean_turns_baseline": mean_field(base_records, ["agent_info", "usage", "turns"]),
            }
        )
    summary = {"records": len(records), "comparisons": comparisons}
    (suite_dir / "summary.json").write_text(json.dumps(summary, indent=2, sort_keys=True))
    return summary


def cmd_run(args: argparse.Namespace) -> int:
    tasks = load_tasks(args.tasks)
    agents = [x.strip() for x in args.agents.split(",") if x.strip()]
    conditions = [x.strip() for x in args.conditions.split(",") if x.strip()]
    suite = args.suite_name or dt.datetime.now(dt.UTC).strftime("%Y%m%dT%H%M%SZ")
    suite_dir = RESULT_DIR / suite
    suite_dir.mkdir(parents=True, exist_ok=False)
    tools = build_tools(suite_dir)

    records = []
    for task in tasks:
        for agent in agents:
            for condition in conditions:
                if condition not in task.get("conditions", []):
                    continue
                for repetition in range(1, args.repetitions + 1):
                    result = run_one(task, agent, condition, repetition, suite_dir, tools, args)
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
            env, _ = prepare_brain(task, "no_brain", worktree, run_dir, tools, args.checkpoint_limit)
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
    run_p.add_argument("--conditions", default="no_brain,semantic_brain,full_brain")
    run_p.add_argument("--repetitions", type=int, default=1)
    run_p.add_argument("--timeout", type=int, default=1800)
    run_p.add_argument("--claude-budget", type=float, default=0.0, help="Claude max budget in USD; 0 disables the cap")
    run_p.add_argument("--checkpoint-limit", type=int, default=200)
    run_p.add_argument("--suite-name")
    run_p.add_argument("--keep-worktrees", action="store_true")
    run_p.set_defaults(func=cmd_run)

    report_p = sub.add_parser("report")
    report_p.add_argument("suite", nargs="+")
    report_p.set_defaults(func=cmd_report)

    check_p = sub.add_parser("check")
    check_p.add_argument("--tasks", nargs="*", default=[])
    check_p.add_argument("--checkpoint-limit", type=int, default=50)
    check_p.add_argument("--keep-check-dir", action="store_true")
    check_p.set_defaults(func=cmd_check)

    args = parser.parse_args()
    return args.func(args)


if __name__ == "__main__":
    raise SystemExit(main())
