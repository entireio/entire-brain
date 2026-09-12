"""`full_brain` -- the product arm. The ONLY arm that touches entire-brain.

Pipeline (each step pinned; the paid step is clearly marked):

  1. Point entire-brain at a scratch data root via ENTIRE_PLUGIN_DATA_DIR, then
     ASK the binary where its brain dir for this repo is. There is no
     --brain-dir flag and the path is derived from repo identity
     (internal/cli/env.go:249 repoStorageForKey), so `distill --dry-run --json`
     is used as the discovery call: it reports `brain_path` and is free.
  2. Synthesize the session corpus by hand: write the pinned session-A JSONL to
     <brainDir>/sessions/main/<session_id>.jsonl and write a manifest.json that
     points at it. This is the raw-JSONL ingest contract proven by
     internal/cli/distill_cmd_test.go:25-75 (writeSingleSessionFixture), so we
     reproduce that fixture's exact shape rather than shelling out to a capture
     flow that would need a live agent session.
  3. `entire-brain distill <repo>` -- PAID (invokes a model). Pinned agent/model/
     effort from config. Skipped when dry_run=True.
     When the pinned distill agent is `codex`, a PATH shim is installed first --
     see agents/codex_provider_shim.py. distill runs `codex exec
     --ignore-user-config` with no provider config of its own, so without the
     shim it would authenticate against public OpenAI instead of the Azure
     deployment this run is pinned to, silently and successfully. The shim
     injects the provider ONLY when the caller supplied none, so it never
     touches a measured BrainMark session, and its sha256 is recorded in the
     packet's prep provenance.
  4. `entire-brain refresh history <repo>` -- free, local.
  5. ONE frozen `entire-brain search "<B problem statement>" --json --limit N`.
     `search` is used rather than `brief` because only search emits the
     {"query", "branch", "results": [...]} shape the envelope's bounder requires
     (brief emits `facts`, not `results`).

Manifest shape is byte-compatible with internal/cli/export.go:3507 exportManifest
and brain.go:32 sessionSourceManifest; schema_version is brain.go:12 (=3).
"""

from __future__ import annotations

import json
import os
import pathlib
import subprocess
from typing import Any

from .. import _harness
from .base import MemoryPacket, MemorySourceError, Stopwatch, build_packet, normalize_results

ARM = "full_brain"

BRAIN_MANIFEST_SCHEMA_VERSION = 3  # internal/cli/brain.go:12
MANIFEST_FILENAME = "manifest.json"  # internal/cli/export.go:31


def _run(cmd: list[str], env: dict[str, str], cwd: pathlib.Path | None = None,
         timeout: int = 1800) -> subprocess.CompletedProcess:
    proc = subprocess.run(
        cmd, capture_output=True, text=True, env=env,
        cwd=str(cwd) if cwd else None, timeout=timeout, check=False,
    )
    if proc.returncode != 0:
        raise MemorySourceError(
            f"{' '.join(cmd[:3])} rc={proc.returncode}: "
            f"{(proc.stderr or '').strip()[:800]}"
        )
    return proc


def _json_stdout(proc: subprocess.CompletedProcess, what: str) -> dict:
    """entire-brain may log plain text before the JSON body."""
    out = proc.stdout or ""
    start = out.find("{")
    if start < 0:
        raise MemorySourceError(f"{what} produced no JSON: {out[:300]}")
    try:
        return json.loads(out[start:])
    except json.JSONDecodeError as exc:
        raise MemorySourceError(f"{what} produced non-JSON: {exc} :: {out[start:start+300]}") from exc


def install_distill_provider_shim(
    data_root: pathlib.Path,
    distill_pin: dict[str, Any],
    env: dict[str, str],
) -> tuple[dict[str, str], dict[str, Any] | None]:
    """Give distill's `codex` the pinned provider. See codex_provider_shim.

    Returns (env, provenance). provenance is None -- and env is untouched --
    when the pinned distill agent is not codex, so no other backend pays for
    this. Configuration comes from backends.json via the SAME
    `CodexAdapter.azure_settings()` the measured sessions use, so the shim
    cannot drift away from what the run is pinned to.
    """
    if str(distill_pin.get("agent") or "") != "codex":
        return env, None

    from .. import agents
    from ..agents import codex_provider_shim

    adapter = agents.get_adapter("codex")
    azure = adapter.azure_settings(dict(env))  # type: ignore[attr-defined]
    return codex_provider_shim.install(
        pathlib.Path(data_root) / "codex-shim", azure, env
    )


def brain_env(data_root: pathlib.Path, base_env: dict[str, str] | None = None) -> dict[str, str]:
    """Isolate all four plugin dirs under one scratch root.

    ENTIRE_PLUGIN_DATA_DIR alone is not enough: config/state/cache would still
    resolve to the operator's real home, which would let one pair's brain leak
    into the next pair's.
    """
    env = dict(base_env if base_env is not None else os.environ)
    root = data_root.resolve()
    env["ENTIRE_PLUGIN_DATA_DIR"] = str(root / "data")
    env["ENTIRE_PLUGIN_CONFIG_DIR"] = str(root / "config")
    env["ENTIRE_PLUGIN_STATE_DIR"] = str(root / "state")
    env["ENTIRE_PLUGIN_CACHE_DIR"] = str(root / "cache")
    for key in ("ENTIRE_PLUGIN_DATA_DIR", "ENTIRE_PLUGIN_CONFIG_DIR",
                "ENTIRE_PLUGIN_STATE_DIR", "ENTIRE_PLUGIN_CACHE_DIR"):
        pathlib.Path(env[key]).mkdir(parents=True, exist_ok=True)
    return env


def discover_brain_dir(binary: pathlib.Path, repo: pathlib.Path, env: dict[str, str]) -> pathlib.Path:
    """`status --json` reports brain.path, costs nothing, and works on an EMPTY brain.

    `distill --dry-run --json` also carries a `brain_path`, but it exits
    non-zero with "no exported sessions" before reporting one -- which is
    precisely the state we are in when we need the path, since we are about to
    create the very first session by hand. status is the only free call that
    answers on a cold brain.
    """
    proc = _run([str(binary), "status", "--json"], env, cwd=repo, timeout=300)
    report = _json_stdout(proc, "status --json")
    brain_path = (report.get("brain") or {}).get("path")
    if not brain_path:
        raise MemorySourceError(f"status --json reported no brain.path: {sorted(report)}")
    return pathlib.Path(str(brain_path))


def synthesize_session(
    brain_dir: pathlib.Path,
    session_id: str,
    transcript_bytes: bytes,
    created_at: str,
    latest_checkpoint: str = "cp1",
    default_branch: str = "main",
) -> dict[str, Any]:
    """Lay down sessions/main/<id>.jsonl + manifest.json.

    Mirrors internal/cli/distill_cmd_test.go:52 writeSingleSessionFixture: one
    main-branch session, transcript on disk, manifest pointing at it.
    """
    transcript_rel = f"sessions/{default_branch}/{session_id}.jsonl"
    transcript_path = brain_dir / pathlib.PurePosixPath(transcript_rel)
    transcript_path.parent.mkdir(parents=True, exist_ok=True)
    transcript_path.write_bytes(transcript_bytes)

    session = {
        "session_id": session_id,
        "branch": default_branch,
        "latest_checkpoint_id": latest_checkpoint,
        "session_index": 0,
        "created_at": created_at,
        "transcript_path": transcript_rel,
    }
    manifest = {
        "schema_version": BRAIN_MANIFEST_SCHEMA_VERSION,
        "generated_at": created_at,
        "default_branch": default_branch,
        "transcript_mode": "full",
        "scope": "branch",
        "checkpoint_limit": 0,
        "checkpoints_scanned": 1,
        "sessions": [session],
        "sources": {
            "sessions": {
                "generated_at": created_at,
                "default_branch": default_branch,
                "transcript_mode": "full",
                "scope": "branch",
                "checkpoint_limit": 0,
                "checkpoints_scanned": 1,
                "latest_checkpoint_id": latest_checkpoint,
                "sessions": [session],
            }
        },
    }
    brain_dir.mkdir(parents=True, exist_ok=True)
    manifest_path = brain_dir / MANIFEST_FILENAME
    manifest_path.write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")
    return {
        "brain_dir": str(brain_dir),
        "manifest_path": str(manifest_path),
        "transcript_path": str(transcript_path),
        "transcript_sha256": _harness.sha256_bytes(transcript_bytes),
        "transcript_bytes": len(transcript_bytes),
        "session_id": session_id,
    }


def build(
    query: str,
    max_bytes: int,
    top_k: int,
    binary: pathlib.Path,
    repo: pathlib.Path,
    data_root: pathlib.Path,
    transcript_bytes: bytes,
    session_id: str,
    created_at: str,
    distill_pin: dict[str, Any],
    dry_run: bool = False,
    agent_command: list[str] | None = None,
    base_env: dict[str, str] | None = None,
    **_ignored,
) -> MemoryPacket:
    """Full prep + one frozen retrieval.

    dry_run=True runs everything EXCEPT the paid distill, so the manifest
    synthesis contract and the retrieval path are exercised for $0.
    agent_command supplies `--agent command --agent-command ...`, which runs a
    local stub instead of a paid model (used by the offline tests).
    """
    binary = pathlib.Path(binary)
    if not binary.is_file():
        raise MemorySourceError(f"entire-brain binary not found at {binary}")
    env = brain_env(pathlib.Path(data_root), base_env)
    steps: list[dict[str, Any]] = []

    with Stopwatch() as watch:
        brain_dir = discover_brain_dir(binary, repo, env)
        synth = synthesize_session(brain_dir, session_id, transcript_bytes, created_at)
        steps.append({"step": "synthesize_session", **synth})

        # -- distill: the paid step -----------------------------------------
        distill_cmd = [str(binary), "distill", str(repo), "--json"]
        if agent_command:
            distill_cmd += ["--agent", "command"]
            for part in agent_command:
                distill_cmd += ["--agent-command", part]
        else:
            distill_cmd += [
                "--agent", str(distill_pin["agent"]),
                "--model", str(distill_pin["model"]),
                "--effort", str(distill_pin["effort"]),
            ]
        if dry_run and not agent_command:
            distill_cmd.append("--dry-run")

        # The shim is for the REAL agent path only: `--agent command` already
        # names the binary to run, so there is nothing to inject into and the
        # offline tests must not need a codex on PATH.
        shim_prov: dict[str, Any] | None = None
        distill_env = env
        if not agent_command:
            distill_env, shim_prov = install_distill_provider_shim(
                pathlib.Path(data_root), distill_pin, env
            )

        proc = _run(distill_cmd, distill_env, timeout=3600)
        steps.append({
            "step": "distill",
            "cmd": distill_cmd,
            "paid": not (dry_run or bool(agent_command)),
            "provider_shim": shim_prov,
            "report": _json_stdout(proc, "distill"),
        })

        # -- refresh history: free ------------------------------------------
        _run([str(binary), "refresh", "history", str(repo)], env, timeout=1800)
        steps.append({"step": "refresh_history"})

        # -- ONE frozen retrieval -------------------------------------------
        search = _run(
            [str(binary), "search", query, "--json", "--limit", str(top_k)],
            env, cwd=repo, timeout=600,
        )
        payload = _json_stdout(search, "search --json")

    raw = payload.get("results")
    if not isinstance(raw, list):
        raise MemorySourceError(f"search --json returned no results array: {sorted(payload)}")

    return build_packet(
        arm=ARM,
        query=query,
        results=normalize_results(raw, top_k),
        max_bytes=max_bytes,
        prep={
            "seconds": watch.elapsed_s,
            "dry_run": dry_run,
            "stub_agent": bool(agent_command),
            "brain_dir": str(brain_dir),
            "distill_pin": distill_pin,
            "binary_sha256": _harness.sha256_file(binary),
            "steps": steps,
        },
    )
