#!/usr/bin/env python3
"""Session B: the measured session. One per (pair, arm).

For each pair we build all five packets FIRST, check prompt symmetry across the
whole arm set, and only then spawn anything. A symmetry violation aborts the
pair before a cent is spent -- catching it after the run would mean discarding
paid sessions.

Per-cell results layout (the contract report.py and grade.py read):

    results/B/<tier>/<pair_id>/<arm>/
        cc_out.json        terminal result event (usd, turns, duration)
        stream.jsonl       full stream-JSON -- input to mechmetrics.py
        cc_err.log         stderr
        patch.diff         collect_patch.sh output = the SWE-bench prediction
        packet.txt         the exact bytes delivered to the agent
        packet.sha256      pin, re-checked at report time
        prompt_sym.sha256  symmetry sha, re-checked at report time
        prompt.txt         the exact prompt
        meta.json          everything else

IDEMPOTENT RESUME: a cell is skipped iff usd>0 AND patch.diff is non-empty --
the same guard graphmark uses. A cost>0 session with an empty patch is a real
failure and is re-run; a cost==0 session never happened.

ISOLATION per session: fresh worktree at base_commit_B, netjail first on PATH,
run.py env sanitization (run.py:5091), and -- on macOS -- run.py's deny-first
sandbox profile (run.py:5125) re-allowing only this cell's worktree, so sibling
results, task JSONs, and session-A artifacts are not reachable from B.
"""

from __future__ import annotations

import argparse
import json
import os
import pathlib
import subprocess
import sys
from typing import Any

if __package__ in (None, ""):
    sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent.parent))
    from brainmark import _harness, _repo, memsources, prompts  # type: ignore[no-redef]
    from brainmark.session_a import extract_result_event, load_problem_statement  # type: ignore[no-redef]
else:
    from . import _harness, _repo, memsources, prompts
    from .session_a import extract_result_event, load_problem_statement


# --------------------------------------------------------------------------
# packets
# --------------------------------------------------------------------------


def build_packets(
    config: dict,
    pair: dict,
    query: str,
    transcript_bytes: bytes,
    arms: list[str],
    a_created_at: str,
    brain_repo: pathlib.Path | None = None,
    brain_data_root: pathlib.Path | None = None,
    force_empty_packet: bool = False,
) -> dict[str, memsources.MemoryPacket]:
    """Build one packet per arm from the SAME pinned session-A bytes.

    force_empty_packet drives the pre-registered NULL TEST: every arm runs its
    real machinery but delivers a sentinel packet, so any measured difference is
    machinery, not memory.
    """
    packet_cfg = config["packet"]
    max_bytes = int(packet_cfg["max_bytes"])
    top_k = int(packet_cfg["top_k"])
    user_id = pair["pair_id"]
    packets: dict[str, memsources.MemoryPacket] = {}

    for arm in arms:
        if arm == "no_brain" or force_empty_packet:
            packets[arm] = memsources.ARMS["no_brain"](
                query=query, max_bytes=max_bytes, sentinel=packet_cfg["empty_sentinel"]
            )
            if force_empty_packet and arm != "no_brain":
                object.__setattr__(packets[arm], "arm", arm)
            continue
        if arm == "full_brain":
            brain_cfg = config["brain"]
            binary = pathlib.Path(brain_cfg["binary"])
            if not binary.is_absolute():
                binary = _harness.BRAINMARK_DIR / binary
            packets[arm] = memsources.ARMS[arm](
                query=query, max_bytes=max_bytes, top_k=top_k,
                binary=binary,
                repo=brain_repo,
                data_root=brain_data_root,
                transcript_bytes=transcript_bytes,
                session_id=f"a-{pair['a']['instance_id']}",
                created_at=a_created_at,
                distill_pin=brain_cfg["distill"],
            )
            continue
        packets[arm] = memsources.ARMS[arm](
            query=query, max_bytes=max_bytes, top_k=top_k,
            transcript_bytes=transcript_bytes, user_id=user_id,
            pins=config["competitors"].get(arm, {}),
        )
    return packets


# --------------------------------------------------------------------------
# one cell
# --------------------------------------------------------------------------


def cell_is_complete(cell: pathlib.Path) -> bool:
    """graphmark's resume guard: usd>0 AND non-empty patch."""
    cc_out = cell / "cc_out.json"
    patch = cell / "patch.diff"
    if not cc_out.is_file() or not patch.is_file() or patch.stat().st_size == 0:
        return False
    try:
        payload = json.loads(cc_out.read_text(encoding="utf-8"))
    except (json.JSONDecodeError, OSError):
        return False
    return float(payload.get("total_cost_usd") or 0) > 0


def build_command(config: dict, prompt: str, tier: str) -> list[str]:
    """Mirrors run.py:7006 for the claude runner, minus MCP (there is none)."""
    model = config["agent"]["session_b"][f"tier_{tier}"]
    return [
        config["agent"]["cli"],
        "--print",
        "--no-session-persistence",
        "--strict-mcp-config",
        "--mcp-config", '{"mcpServers":{}}',
        "--disable-slash-commands",
        "--permission-mode", "bypassPermissions",
        "--output-format", "stream-json",
        "--verbose",
        "--safe-mode",
        "--model", model,
        "--max-turns", str(config["agent"]["max_turns"]),
        prompt,
    ]


def session_env(config: dict, cell: pathlib.Path) -> tuple[dict[str, str], dict]:
    env, provenance = _harness.sanitize_harness_agent_environment(dict(os.environ))
    env["PATH"] = _repo.netjail_path(pathlib.Path(config["graphmark_root"]), env.get("PATH"))
    env["CLAUDE_CONFIG_DIR"] = str(cell / "claude-config")
    (cell / "claude-config").mkdir(parents=True, exist_ok=True)
    return env, provenance


def read_isolation(config: dict, worktree: pathlib.Path, env: dict[str, str]):
    """run.py's deny-first sandbox profile (run.py:5125). macOS only."""
    sandbox = _harness.sandbox_executable()
    if not sandbox.exists():
        return None, {"backend": None, "reason": f"{sandbox} not present"}
    try:
        profile, provenance = _harness.temporal_agent_read_isolation(
            worktree=worktree,
            source=_harness.AGENT_BENCH_DIR,
            tools={},
            host_env=env,
        )
        return profile, provenance
    except Exception as exc:  # noqa: BLE001 - never let isolation setup kill a run silently
        return None, {"backend": None, "reason": f"{type(exc).__name__}: {exc}"}


def run_cell(
    config: dict,
    pair: dict,
    arm: str,
    packet: memsources.MemoryPacket,
    prompt: str,
    symmetry_sha: str,
    cell: pathlib.Path,
    tier: str,
    stub_only: bool = False,
) -> dict:
    graphmark_root = pathlib.Path(config["graphmark_root"])
    cache = _repo.cache_dir_for(pathlib.Path(config["repo_cache"]), pair["repo"])
    cell.mkdir(parents=True, exist_ok=True)

    (cell / "packet.txt").write_text(packet.text, encoding="utf-8")
    (cell / "packet.sha256").write_text(packet.sha256 + "\n", encoding="utf-8")
    (cell / "prompt.txt").write_text(prompt, encoding="utf-8")
    (cell / "prompt_sym.sha256").write_text(symmetry_sha + "\n", encoding="utf-8")

    worktree = cell / "worktree"
    env, env_prov = session_env(config, cell)
    cmd = build_command(config, prompt, tier)
    profile, iso_prov = read_isolation(config, worktree, env)
    if profile is not None:
        cmd = [str(_harness.sandbox_executable()), "-p", profile, *cmd]

    meta: dict[str, Any] = {
        "pair_id": pair["pair_id"], "arm": arm, "tier": tier,
        "instance_id": pair["b"]["instance_id"], "repo": pair["repo"],
        "base_commit": pair["b"]["base_commit"],
        "model": config["agent"]["session_b"][f"tier_{tier}"],
        "packet_sha256": packet.sha256,
        "packet_provenance": packet.to_provenance(),
        "prompt_sym_sha256": symmetry_sha,
        "prompt_sha256": _harness.sha256_text(prompt),
        "env_sanitization": env_prov,
        "read_isolation": iso_prov,
        "cmd": cmd[:-1] + ["<PROMPT>"],
        "stub_only": stub_only,
    }

    _repo.worktree_add(graphmark_root, cache, worktree, pair["b"]["base_commit"])
    try:
        with (cell / "stream.jsonl").open("wb") as out_fh, (cell / "cc_err.log").open("wb") as err_fh:
            proc = subprocess.run(
                cmd, cwd=str(worktree), env=env, stdin=subprocess.DEVNULL,
                stdout=out_fh, stderr=err_fh,
                timeout=config["agent"]["timeout_sec"], check=False,
            )
        meta["returncode"] = proc.returncode
        meta["result_event"] = extract_result_event(cell / "stream.jsonl", cell / "cc_out.json")
        meta["patch_bytes"] = _repo.collect_patch(
            graphmark_root, cache, worktree, cell / "patch.diff"
        )
    finally:
        _repo.worktree_remove(graphmark_root, cache, worktree)

    from .mechmetrics import session_metrics

    meta["mechmetrics"] = session_metrics(cell / "stream.jsonl")
    (cell / "meta.json").write_text(_harness.pretty_json(meta), encoding="utf-8")
    return meta


# --------------------------------------------------------------------------
# one pair, all arms
# --------------------------------------------------------------------------


def run_pair(
    config: dict,
    pair: dict,
    a_dir: pathlib.Path,
    out_root: pathlib.Path,
    tier: str,
    arms: list[str] | None = None,
    force_empty_packet: bool = False,
    stub_only: bool = False,
    resume: bool = True,
) -> dict:
    arms = arms or list(config["arms"])
    a_meta = json.loads((a_dir / "meta.json").read_text(encoding="utf-8"))
    transcript = pathlib.Path(a_meta["transcript_path"])
    transcript_bytes = transcript.read_bytes()

    # THE PIN CHECK. If session A's bytes changed since it ran, every packet
    # derived from them is comparing something other than what was recorded.
    actual = _harness.sha256_bytes(transcript_bytes)
    if actual != a_meta["transcript_sha256"]:
        raise RuntimeError(
            f"session-A transcript pin mismatch for {pair['pair_id']}: "
            f"recorded {a_meta['transcript_sha256'][:12]}, found {actual[:12]}"
        )

    query = pair["b"].get("problem_statement") or load_problem_statement(
        config, pair["b"]["instance_id"]
    )

    pair_root = out_root / pair["pair_id"]
    pair_root.mkdir(parents=True, exist_ok=True)

    packets = build_packets(
        config, pair, query, transcript_bytes, arms,
        a_created_at=a_meta.get("created_at") or "2026-01-01T00:00:00Z",
        brain_repo=pair_root / "brain-repo",
        brain_data_root=pair_root / "brain-data",
        force_empty_packet=force_empty_packet,
    )

    built = {arm: prompts.build_prompt(query, packets[arm].text) for arm in arms}
    # THE SYMMETRY GATE -- before anything is spawned.
    symmetry_sha = prompts.assert_symmetric(built)

    summary: dict[str, Any] = {
        "pair_id": pair["pair_id"], "tier": tier, "arms": arms,
        "prompt_sym_sha256": symmetry_sha,
        "packet_sha256": {arm: packets[arm].sha256 for arm in arms},
        "packet_prep": {arm: packets[arm].meta.get("prep", {}) for arm in arms},
        "force_empty_packet": force_empty_packet,
        "cells": {},
    }

    for arm in arms:
        cell = pair_root / arm
        if resume and cell_is_complete(cell):
            summary["cells"][arm] = {"skipped": "already complete"}
            continue
        summary["cells"][arm] = run_cell(
            config, pair, arm, packets[arm], built[arm], symmetry_sha, cell, tier,
            stub_only=stub_only,
        )

    (pair_root / "pair_summary.json").write_text(
        _harness.pretty_json(summary), encoding="utf-8"
    )
    return summary


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Run BrainMark session B for one pair.")
    parser.add_argument("--pair", required=True)
    parser.add_argument("--a-dir", required=True, help="session A results dir for this pair")
    parser.add_argument("--config", default=None)
    parser.add_argument("--tier", default="pilot", choices=["pilot", "full"])
    parser.add_argument("--out", default=None)
    parser.add_argument("--arms", default=None, help="comma-separated subset of arms")
    parser.add_argument("--null-test", action="store_true",
                        help="deliver a sentinel packet to EVERY arm (pre-registered null test)")
    parser.add_argument("--no-resume", action="store_true")
    args = parser.parse_args(argv)

    config = _harness.load_config(args.config)
    pair = json.loads(pathlib.Path(args.pair).read_text(encoding="utf-8"))
    out_root = pathlib.Path(args.out) if args.out else (
        _harness.BRAINMARK_DIR / "results" / "B" / args.tier
    )
    arms = args.arms.split(",") if args.arms else None

    summary = run_pair(
        config, pair, pathlib.Path(args.a_dir), out_root, args.tier,
        arms=arms, force_empty_packet=args.null_test, resume=not args.no_resume,
    )
    print(_harness.pretty_json({
        "pair_id": summary["pair_id"],
        "prompt_sym_sha256": summary["prompt_sym_sha256"],
        "packet_sha256": summary["packet_sha256"],
    }), end="")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
