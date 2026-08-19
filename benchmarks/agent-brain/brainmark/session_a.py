#!/usr/bin/env python3
"""Session A: the prior session whose learning every arm is asked to reuse.

A runs ONCE per pair per model tier and is SHARED BY ALL FIVE ARMS. That is the
core fairness property of BrainMark: no arm gets a session A tuned to it, and no
arm's memory is built from different bytes than another's. The harvested
transcript is sha256-pinned at harvest time and every downstream memory source
is handed those exact bytes.

A is an ordinary SWE-bench attempt at task A with NO memory of any kind. We do
not care whether A succeeds -- a failed-but-exploratory session is still a real
prior session, and filtering on A's success would select for pairs where the
answer was easy, biasing every arm at once.

Isolation:
  * dedicated CLAUDE_CONFIG_DIR per pair, so the native JSONL is harvestable and
    cannot mix with the operator's real sessions;
  * netjail first on PATH;
  * run.py's env sanitizer (run.py:5091) strips AGENT_BENCH_*/BENCH_*/ENTIRE_*
    so the session cannot discover the harness through the environment.

Note we deliberately do NOT pass --no-session-persistence (which run.py:7009 uses
for its B-style runs): persistence is what writes the native JSONL we harvest.

Usage:
    python3 session_a.py --pair PAIR.json --tier pilot [--dry-run]
"""

from __future__ import annotations

import argparse
import json
import os
import pathlib
import shutil
import sys

if __package__ in (None, ""):
    sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent.parent))
    from brainmark import _harness, _repo, agents  # type: ignore[no-redef]
    from brainmark.prompts import TASK_INSTRUCTIONS  # type: ignore[no-redef]
else:
    from . import _harness, _repo, agents
    from .prompts import TASK_INSTRUCTIONS


def a_prompt(problem_statement: str) -> str:
    """A's prompt: the task scaffold with NO memory block at all.

    A is not an arm and is never compared to one, so the packet-block symmetry
    rule does not apply; adding an empty packet here would only teach A about a
    channel that does not exist.
    """
    return f"{TASK_INSTRUCTIONS}\n\n--- ISSUE ---\n{problem_statement.strip()}\n"


def resolve_backend(config: dict, override: str | None = None):
    """Backend for session A. Same resolution order as run_b.resolve_backend --
    A and B MUST run on the same backend, or the memory every arm is built from
    was produced by a different agent than the one being measured."""
    from .run_b import resolve_backend as _resolve  # local: avoids an import cycle

    return _resolve(config, override)


def resolve_model(config: dict, backend: str, tier: str, backends: dict,
                  override: str | None = None, role: str = "primary") -> str:
    session_a = (config.get("agent") or {}).get("session_a") or {}
    if override:
        return override
    scoped = session_a.get(f"{backend}_tier_{tier}")
    if scoped:
        return str(scoped)
    # Same rule as run_b.resolve_model: the plain tier key belongs to the CLI
    # named in the config, not to whatever backend was selected at runtime.
    cli = str((config.get("agent") or {}).get("cli") or "")
    plain = session_a.get(f"tier_{tier}")
    if plain and cli == backend:
        return str(plain)
    return agents.resolve_model(backend, role, backends)


def build_command(config: dict, prompt: str, tier: str,
                  adapter=None, model: str | None = None,
                  worktree: pathlib.Path | None = None) -> list[str]:
    """A-session argv, now produced by the backend adapter.

    Session A keeps SESSION PERSISTENCE ON (unlike B): persistence is what
    writes the native JSONL this file harvests, and that JSONL is the pinned
    input every memory source is built from. agents/claude_adapter.py reproduces
    the previous argv byte for byte with `session_persistence=True`.
    """
    if adapter is None:
        adapter, _name, backends = resolve_backend(config)
        model = model or resolve_model(config, adapter.name, tier, backends)
    return adapter.build_command(
        prompt, worktree or pathlib.Path("."), model or "",
        max_turns=int(config["agent"]["max_turns"]),
        session_persistence=True,
    )


def session_env(config: dict) -> tuple[dict[str, str], dict]:
    """Backend-NEUTRAL env: run.py's sanitizer plus netjail.

    The per-session agent home (CLAUDE_CONFIG_DIR for claude, CODEX_HOME for
    codex) is added by the adapter's prepare_env, which also decides where it
    lives; harvest_native_jsonl() looks in the same place.
    """
    env, provenance = _harness.sanitize_harness_agent_environment(dict(os.environ))
    env["PATH"] = _repo.netjail_path(pathlib.Path(config["graphmark_root"]), env.get("PATH"))
    return env, provenance


def harvest_native_jsonl(claude_config_dir: pathlib.Path, dest: pathlib.Path) -> dict:
    """Copy Claude Code's own session JSONL out of the isolated config dir.

    Claude Code writes <CLAUDE_CONFIG_DIR>/projects/<munged-cwd>/<uuid>.jsonl.
    With a per-pair config dir there is normally exactly one; if a session was
    resumed there may be several, so the newest by mtime wins and the rest are
    recorded so the choice is auditable.
    """
    projects = claude_config_dir / "projects"
    found = sorted(projects.rglob("*.jsonl")) if projects.is_dir() else []
    if not found:
        return {"harvested": False, "candidates": [], "reason": "no native JSONL written"}
    chosen = max(found, key=lambda p: p.stat().st_mtime)
    dest.parent.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(chosen, dest)
    return {
        "harvested": True,
        "source": str(chosen),
        "candidates": [str(p) for p in found],
        "sha256": _harness.sha256_file(dest),
        "bytes": dest.stat().st_size,
    }


def run(pair: dict, config: dict, out_dir: pathlib.Path, tier: str,
        dry_run: bool = False, backend: str | None = None,
        model: str | None = None, model_role: str = "primary") -> dict:
    graphmark_root = pathlib.Path(config["graphmark_root"])
    repo = pair["repo"]
    cache = _repo.cache_dir_for(pathlib.Path(config["repo_cache"]), repo)
    out_dir.mkdir(parents=True, exist_ok=True)

    adapter, backend_name, backends = resolve_backend(config, backend)
    resolved_model = resolve_model(config, backend_name, tier, backends, model, model_role)

    worktree = out_dir / "worktree"
    claude_config_dir = out_dir / "claude-config"
    stream_path = out_dir / "stream.jsonl"
    transcript_path = out_dir / "session_transcript.jsonl"

    problem = pair["a"].get("problem_statement")
    if problem is None:
        problem = load_problem_statement(config, pair["a"]["instance_id"])
    prompt = a_prompt(problem)
    env, env_prov = session_env(config)

    meta: dict = {
        "pair_id": pair["pair_id"],
        "instance_id": pair["a"]["instance_id"],
        "repo": repo,
        "base_commit": pair["a"]["base_commit"],
        "tier": tier,
        "backend": backend_name,
        "model": resolved_model,
        "dry_run": dry_run,
        "prompt_sha256": _harness.sha256_text(prompt),
        "env_sanitization": env_prov,
    }

    _repo.worktree_add(graphmark_root, cache, worktree, pair["a"]["base_commit"])
    try:
        result = adapter.run(
            prompt=prompt, worktree=worktree, model=resolved_model,
            timeout=config["agent"]["timeout_sec"], out_dir=out_dir, env=env,
            patch_collector=lambda wt, out: _repo.collect_patch(
                graphmark_root, cache, wt, out
            ),
            max_turns=int(config["agent"]["max_turns"]),
            session_persistence=True,
        )
        meta.update(result.to_meta())
        meta["cmd"] = result.cmd
        meta["returncode"] = result.returncode
        meta["result_event"] = json.loads(result.out_json_path.read_text(encoding="utf-8"))
        meta["patch_bytes"] = (result.patch or {}).get("bytes", 0)
        meta["native_jsonl"] = harvest_native_jsonl(claude_config_dir, transcript_path)
    finally:
        _repo.worktree_remove(graphmark_root, cache, worktree)

    # THE PIN. Everything downstream consumes exactly these bytes.
    if meta["native_jsonl"].get("harvested"):
        meta["transcript_sha256"] = meta["native_jsonl"]["sha256"]
        meta["transcript_path"] = str(transcript_path)
        meta["transcript_source"] = "native_agent_jsonl"
    else:
        shutil.copyfile(stream_path, transcript_path)
        meta["transcript_sha256"] = _harness.sha256_file(transcript_path)
        meta["transcript_path"] = str(transcript_path)
        if backend_name == "codex":
            # NOT a degradation: `codex exec --ephemeral` writes no session file
            # by design, and its `--json` stdout IS the complete event record.
            # Named distinctly so a report never conflates the two formats.
            meta["transcript_source"] = "codex_event_stream"
        else:
            meta["transcript_source"] = "stream_fallback"
            meta["transcript_fallback"] = "stream.jsonl (native JSONL not found)"

    (out_dir / "meta.json").write_text(_harness.pretty_json(meta), encoding="utf-8")
    return meta


def extract_result_event(stream_path: pathlib.Path, cc_out_path: pathlib.Path) -> dict:
    """Write cc_out.json from the stream's terminal `result` event.

    graphmark's driver uses --output-format json and gets this object directly;
    we need stream-json for the mechanism metric, so the same object is recovered
    from the last `result` event. Downstream gates (usd>0) then work unchanged.
    """
    result: dict = {}
    if stream_path.exists():
        for line in stream_path.read_text(encoding="utf-8", errors="replace").splitlines():
            line = line.strip()
            if not line:
                continue
            try:
                event = json.loads(line)
            except json.JSONDecodeError:
                continue
            if isinstance(event, dict) and event.get("type") == "result":
                result = event
    cc_out_path.write_text(json.dumps(result, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    return result


def load_problem_statement(config: dict, instance_id: str) -> str:
    from .mine_pairs import load_instances  # local import: avoids a cycle

    pool, _ = load_instances(pathlib.Path(config["graphmark_root"]), config["task_globs"])
    inst = pool.get(instance_id)
    if inst is None:
        raise KeyError(f"instance {instance_id} not in the task pool")
    return str(inst.get("problem_statement") or "")


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Run BrainMark session A for one pair.")
    parser.add_argument("--pair", required=True, help="path to a candidate/sealed pair JSON")
    parser.add_argument("--config", default=None)
    parser.add_argument("--tier", default="pilot", choices=["pilot", "full"])
    parser.add_argument("--out", default=None)
    parser.add_argument("--backend", default=None, choices=sorted(agents.ADAPTERS),
                        help="agent backend; default comes from config/backends.json")
    parser.add_argument("--model", default=None, help="explicit model, overriding the tier")
    parser.add_argument("--model-role", default="primary",
                        choices=["primary", "generality", "alternate"])
    parser.add_argument("--dry-run", action="store_true",
                        help="print the plan and exit; spawns no agent, spends nothing")
    args = parser.parse_args(argv)

    config = _harness.load_config(args.config)
    pair = json.loads(pathlib.Path(args.pair).read_text(encoding="utf-8"))
    out_dir = pathlib.Path(args.out) if args.out else (
        _harness.BRAINMARK_DIR / "results" / "A" / args.tier / pair["pair_id"]
    )

    if args.dry_run:
        problem = load_problem_statement(config, pair["a"]["instance_id"])
        adapter, backend_name, backends = resolve_backend(config, args.backend)
        model = resolve_model(config, backend_name, args.tier, backends,
                              args.model, args.model_role)
        plan = {
            "pair_id": pair["pair_id"],
            "instance_id": pair["a"]["instance_id"],
            "tier": args.tier,
            "backend": backend_name,
            "model": model,
            "out_dir": str(out_dir),
            "cmd": build_command(config, "<PROMPT>", args.tier,
                                 adapter=adapter, model=model, worktree=out_dir / "worktree"),
            "prompt_sha256": _harness.sha256_text(a_prompt(problem)),
            "would_spend": True,
        }
        print(_harness.pretty_json(plan), end="")
        return 0

    meta = run(pair, config, out_dir, args.tier, backend=args.backend,
               model=args.model, model_role=args.model_role)
    print(_harness.pretty_json({
        "pair_id": meta["pair_id"],
        "transcript_sha256": meta["transcript_sha256"],
        "patch_bytes": meta["patch_bytes"],
        "usd": (meta.get("result_event") or {}).get("total_cost_usd"),
    }), end="")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
