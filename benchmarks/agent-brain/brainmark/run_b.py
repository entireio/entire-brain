#!/usr/bin/env python3
"""Session B: the measured session. One per (pair, arm, rep).

For each pair we build every packet FIRST, check prompt symmetry across the
whole arm set, and only then spawn anything. A symmetry violation aborts the
pair before a cent is spent -- catching it after the run would mean discarding
paid sessions.

Per-cell results layout (the contract report.py and grade.py read):

    results/B/<tier>/[rep<k>/]<pair_id>/<arm>/
        cc_out.json        terminal result object (usd, turns, duration)
        stream.jsonl       raw agent stream -- input to mechmetrics.py
        cc_err.log         stderr
        patch.diff         collect_patch.sh output = the SWE-bench prediction
        packet.txt         the exact bytes delivered to the agent
        packet.sha256      pin, re-checked at report time
        prompt_sym.sha256  symmetry sha, re-checked at report time
        prompt.txt         the exact prompt
        meta.json          everything else

REPS (plan 0.6). `--rep N` runs N independent replicates. The rep index is a
DIRECTORY LEVEL ABOVE the pair, not below the arm, for one reason: each
`rep<k>/` is then itself a complete, valid results root that report.py and
grade.py consume with no change at all (`--results .../pilot/rep0`). Putting
rep at the leaf would have forced every downstream reader to learn a new
layout. With no `--rep` the layout is exactly what it was, so existing result
trees stay readable.

Each rep records `rep_seed = master_seed*1000 + k` in meta.json. Stated plainly:
NEITHER backend exposes a sampling seed, so this seed does not make a rep
reproducible -- it identifies the rep and seeds harness-side choices only.
Replicates buy precision against run-level noise (devenv memory: run-level
variance is ~87% of paired variance), they do not buy determinism.

IDEMPOTENT RESUME, PER REP: a cell is skipped iff it recorded a completed
session AND a non-empty patch. "Completed" is backend-aware: claude reports a
provider cost, so usd>0 is the graphmark guard; codex reports NO cost at all, so
the guard there is a complete token report with output_tokens>0. Using usd>0 on
codex would re-run every cell forever (its usd is a harness-computed estimate
that would be 0 whenever prices are absent).

BACKENDS. The agent CLI lives behind brainmark/agents/ (plan 0.5): codex/Azure
is primary, claude is kept in-tree. The backend is chosen by, in order:
`--backend`, `config["agent"]["backend"]`, the CLI named in
`config["agent"]["cli"]`, then backends.json `default_backend`. Backends are
never pooled in a report -- they are separate cells.

ISOLATION per session: fresh worktree at base_commit_B, netjail first on PATH,
run.py env sanitization (run.py:5091), and -- on macOS -- run.py's deny-first
sandbox profile (run.py:5125) re-allowing only this cell's worktree, so sibling
results, task JSONs, and session-A artifacts are not reachable from B.

WORKTREE LOCATION IS A FAIRNESS PROPERTY, not a layout preference. The cell dir
is <results>/<pair_id>/<arm>, so a worktree at <cell>/worktree puts the ARM NAME
in the agent's own `pwd` -- and the agent's shell output contains its cwd. A
`no_brain` session can read the string "no_brain" out of its own working
directory and a `full_brain` session reads "full_brain". That is an
arm-asymmetric CUE delivered outside the prompt, which is exactly what the
symmetry gate exists to prevent; it fails the standard test ("would this
sentence help an arm with no memory?"). Secondarily, a worktree inside the
results tree leaves the SIBLING arms' packet.txt/prompt.txt two directories
above the agent's cwd -- on macOS run.py's deny-first sandbox profile hides
them, but that profile is macOS-only and is inert everywhere else
(read_isolation.backend == null), so the adjacency is real on Linux.

`worktree_root(...)` therefore places every worktree at an OPAQUE, arm-neutral
path -- sha256(pair|arm|rep)[:16] -- under BM_WORKTREE_ROOT (or the OS temp dir
when that is unset), outside the results tree. Nothing else changes:
worktree_add/remove and collect_patch all take the path as an argument. Set
BM_WORKTREE_IN_CELL=1 to restore the old in-cell layout for debugging; it is
recorded in meta.json as `worktree_arm_neutral: false` so a run made that way
can never be mistaken for a clean one.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import pathlib
import sys
import tempfile
from typing import Any

if __package__ in (None, ""):
    sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent.parent))
    from brainmark import _harness, _repo, agents, memsources, prompts  # type: ignore[no-redef]
    from brainmark.memsources import irrelevant as irrelevant_source  # type: ignore[no-redef]
    from brainmark.session_a import load_problem_statement  # type: ignore[no-redef]
else:
    from . import _harness, _repo, agents, memsources, prompts
    from .memsources import irrelevant as irrelevant_source
    from .session_a import load_problem_statement

__all__ = [
    "build_packets", "cell_is_complete", "resolve_backend", "resolve_model",
    "rep_root", "rep_seed", "run_cell", "run_pair", "main",
]


# --------------------------------------------------------------------------
# backend + model resolution
# --------------------------------------------------------------------------

#: config["agent"]["cli"] -> backend name, for configs written before the
#: agents/ layer existed. Keeps an old config.json meaning what it meant.
_CLI_TO_BACKEND = {"claude": "claude", "codex": "codex"}


def resolve_backend(config: dict, override: str | None = None,
                    backends: dict | None = None) -> tuple[Any, str, dict]:
    backends = backends if backends is not None else agents.load_backends()
    agent_cfg = config.get("agent") or {}
    name = (
        override
        or agent_cfg.get("backend")
        or _CLI_TO_BACKEND.get(str(agent_cfg.get("cli") or ""))
        or backends.get("default_backend")
    )
    adapter = agents.get_adapter(name, backends)
    return adapter, adapter.name, backends


def resolve_model(config: dict, backend: str, tier: str, backends: dict,
                  override: str | None = None, role: str = "primary") -> str:
    """Model for (backend, tier). config.json wins; backends.json is the fallback.

    Resolution, in order:
      1. an explicit `--model` override;
      2. a per-backend key, `session_b.<backend>_tier_<tier>`;
      3. the plain `session_b.tier_<tier>` key -- but ONLY when
         `config["agent"]["cli"]` names this backend, because that key holds the
         model written for THAT CLI. Setting `agent.backend = "codex"` on a
         config whose tier values are claude model names must not hand a claude
         model to codex; it falls through to (4) instead;
      4. backends.json `models[<role>]`.
    """
    if override:
        return override
    session_b = (config.get("agent") or {}).get("session_b") or {}
    scoped = session_b.get(f"{backend}_tier_{tier}")
    if scoped:
        return str(scoped)
    cli_backend = _CLI_TO_BACKEND.get(str((config.get("agent") or {}).get("cli") or ""))
    plain = session_b.get(f"tier_{tier}")
    if plain and cli_backend == backend:
        return str(plain)
    return agents.resolve_model(backend, role, backends)


# --------------------------------------------------------------------------
# reps
# --------------------------------------------------------------------------


def rep_root(out_root: pathlib.Path, rep: int | None) -> pathlib.Path:
    """`out_root` (legacy, no reps) or `out_root/rep<k>` -- itself a results root."""
    return pathlib.Path(out_root) if rep is None else pathlib.Path(out_root) / f"rep{rep}"


def rep_seed(config: dict, rep: int | None) -> int:
    master = int(((config.get("seeds") or {}).get("master")) or 0)
    return master * 1000 + int(rep or 0)


# --------------------------------------------------------------------------
# packets
# --------------------------------------------------------------------------


def find_donor_packet(out_root: pathlib.Path, donor_pair_id: str,
                      donor_arm: str = irrelevant_source.DONOR_ARM) -> str:
    """The donor pair's already-built packet bytes, from any rep of this tier.

    Read from disk rather than rebuilt so the placebo is BYTE-IDENTICAL to what
    the donor's treatment arm actually received -- rebuilding it would re-run a
    nondeterministic memory source and produce a packet nobody was ever given.
    Ordering consequence, stated rather than hidden: the placebo arm requires the
    donor's `{donor_arm}` cell to exist, so run that arm across all pairs first.
    """
    root = pathlib.Path(out_root)
    candidates = [root / donor_pair_id / donor_arm / "packet.txt"]
    candidates += sorted(root.glob(f"rep*/{donor_pair_id}/{donor_arm}/packet.txt"))
    if root.name.startswith("rep"):
        # This root IS a rep dir, so sibling reps of the same tier are valid
        # sources; the packet is pinned per pair, not per rep.
        candidates += sorted(root.parent.glob(
            f"rep*/{donor_pair_id}/{donor_arm}/packet.txt"))
    for candidate in candidates:
        if candidate.is_file() and candidate.stat().st_size > 0:
            return candidate.read_text(encoding="utf-8")
    raise memsources.MemorySourceError(
        f"arm {irrelevant_source.ARM}: no {donor_arm} packet found for donor "
        f"{donor_pair_id} under {root}. Run the {donor_arm} arm for every pair "
        "before the placebo arm; never substitute a freshly built donor packet."
    )


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
    sealed_pairs: list[dict] | None = None,
    donor_packet_lookup: Any = None,
) -> dict[str, memsources.MemoryPacket]:
    """Build one packet per arm from the SAME pinned session-A bytes.

    force_empty_packet drives the pre-registered NULL TEST: every arm runs its
    real machinery but delivers a sentinel packet, so any measured difference is
    machinery, not memory.

    The placebo arm is built LAST and out of the registry: it needs a donor pair
    (from the derangement over `sealed_pairs`) and, for size matching, the
    treatment packet this pair actually got.
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
        if arm == irrelevant_source.ARM:
            continue  # built below, once the treatment packet's size is known
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

    if irrelevant_source.ARM in arms and not force_empty_packet:
        if not sealed_pairs:
            raise memsources.MemorySourceError(
                f"arm {irrelevant_source.ARM}: the placebo needs the sealed pair "
                "list to compute its derangement (pass --pairs / sealed_pairs)"
            )
        donor_pair_id = irrelevant_source.donor_for(pair["pair_id"], sealed_pairs)
        lookup = donor_packet_lookup or find_donor_packet
        treatment = packets.get(irrelevant_source.DONOR_ARM)
        packets[irrelevant_source.ARM] = irrelevant_source.build(
            query=query, max_bytes=max_bytes, top_k=top_k,
            donor_packet_text=lookup(donor_pair_id),
            donor_pair_id=donor_pair_id,
            recipient_pair_id=pair["pair_id"],
            target_bytes=(len(treatment.text.encode("utf-8")) if treatment else None),
        )
    return packets


# --------------------------------------------------------------------------
# one cell
# --------------------------------------------------------------------------


def cell_is_complete(cell: pathlib.Path, backend: str | None = None) -> bool:
    """graphmark's resume guard, made backend-aware.

    claude: usd>0 AND non-empty patch (unchanged -- the provider reports cost).
    codex:  a COMPLETE token report with output_tokens>0 AND non-empty patch.
            Codex reports no cost, so its `total_cost_usd` is a harness estimate
            that is None/0 whenever the price table is absent; gating on it would
            re-run every finished cell forever.
    """
    cc_out = cell / "cc_out.json"
    patch = cell / "patch.diff"
    if not cc_out.is_file() or not patch.is_file() or patch.stat().st_size == 0:
        return False
    try:
        payload = json.loads(cc_out.read_text(encoding="utf-8"))
    except (json.JSONDecodeError, OSError):
        return False
    if backend is None:
        backend = "codex" if payload.get("_synthesized_by") else "claude"
    if backend == "codex":
        usage = payload.get("usage") or {}
        produced = usage.get("output_tokens")
        return (
            payload.get("subtype") == "success"
            and isinstance(produced, (int, float))
            and float(produced) > 0
        )
    return float(payload.get("total_cost_usd") or 0) > 0


def build_command(config: dict, prompt: str, tier: str,
                  adapter: Any = None, model: str | None = None,
                  worktree: pathlib.Path | None = None) -> list[str]:
    """Kept as the one-line entry point it always was; the argv now comes from
    the backend adapter (agents/claude_adapter.py reproduces the previous argv
    byte for byte, so a claude cell's command line did not change)."""
    if adapter is None:
        adapter, _name, backends = resolve_backend(config)
        model = model or resolve_model(config, adapter.name, tier, backends)
    return adapter.build_command(
        prompt, worktree or pathlib.Path("."), model or "",
        max_turns=int((config.get("agent") or {}).get("max_turns") or 60),
    )


def session_env(config: dict, cell: pathlib.Path) -> tuple[dict[str, str], dict]:
    """Backend-NEUTRAL env: run.py's sanitizer plus netjail. Backend-specific
    variables (CLAUDE_CONFIG_DIR / CODEX_HOME / the Azure key) are added by the
    adapter's prepare_env, so nothing here knows which agent is about to run."""
    env, provenance = _harness.sanitize_harness_agent_environment(dict(os.environ))
    env["PATH"] = _repo.netjail_path(pathlib.Path(config["graphmark_root"]), env.get("PATH"))
    return env, provenance


#: Explicit, recorded opt-out from read isolation. The ONLY way a macOS cell may
#: run unsandboxed; it is stamped into meta.json so such a run can never be
#: mistaken for an isolated one.
NO_READ_ISOLATION_ENV = "BM_NO_READ_ISOLATION"

#: Subtrees of the graphmark checkout that hold ANSWERS, relative to
#: graphmark_root. `tasks/*.json` records carry `patch` (the gold patch),
#: `test_patch`, `FAIL_TO_PASS`, `PASS_TO_PASS` and `hints_text` for every
#: instance; `results/` holds other runs' and other arms' staged predictions.
#:
#: These are NOT covered by run.py's denies, which name the entire-brain repo
#: root and benchmarks/agent-brain -- graphmark is a sibling checkout, so it
#: fell under `(allow default)`. And the agent does not have to guess where it
#: is: `_repo.netjail_path` puts `<graphmark_root>/tools/netjail` first on the
#: session's own PATH, so `echo $PATH` hands over the checkout root, and
#: `<root>/tasks/*.json` then hands over the gold patch for the very instance
#: the session is being scored on.
#:
#: The repo cache is deliberately NOT here: the worktree's `.git` points into
#: it and the session cannot run without it.
ANSWER_KEY_SUBDIRS: tuple[str, ...] = ("tasks", "results")


def answer_key_roots(graphmark_root: pathlib.Path) -> list[pathlib.Path]:
    """Existing answer-holding subtrees of the graphmark checkout, to deny."""
    root = pathlib.Path(graphmark_root)
    return [root / name for name in ANSWER_KEY_SUBDIRS if (root / name).exists()]


def read_isolation(config: dict, worktree: pathlib.Path, env: dict[str, str],
                   results_root: pathlib.Path | None = None):
    """run.py's deny-first sandbox profile (run.py:5125). macOS only.

    `tools["bin"]` IS REQUIRED by run.py:5152 -- it is one of the roots the
    profile re-allows. Passing `tools={}` raised KeyError inside profile
    construction, and a blanket `except Exception` turned that into
    `(None, ...)`: isolation silently OFF for every cell on every platform,
    while run_b's docstring went on justifying the arm-neutral worktree layout
    by pointing at a profile that was never built. The netjail directory is the
    right value here: it is this harness's frozen tool dir and it must stay
    readable, exactly as run.py's tool dir does.

    `results_root` is denied on top of run.py's own denies. run.py denies the
    repo root and `source`, which covers the DEFAULT results tree
    (brainmark/results) but not `--out /somewhere/else`; without this a B
    session run with an external `--out` can read every sibling arm's
    packet.txt, prompt.txt and meta.json, which is the exact side channel the
    profile exists to close.

    Failure is LOUD. The previous swallow was written to keep isolation setup
    from killing a run; the effect was that it killed the isolation instead.
    A cell that cannot be isolated must abort, or be opted out explicitly via
    BM_NO_READ_ISOLATION=1, which is recorded.
    """
    sandbox = _harness.sandbox_executable()
    if not sandbox.exists():
        return None, {"backend": None, "reason": f"{sandbox} not present"}
    if os.environ.get(NO_READ_ISOLATION_ENV) == "1":
        return None, {"backend": None, "reason": f"disabled by {NO_READ_ISOLATION_ENV}"}

    graphmark_root = pathlib.Path(config["graphmark_root"])
    tools_bin = _repo.netjail_dir(graphmark_root)
    profile, provenance = _harness.temporal_agent_read_isolation(
        worktree=worktree,
        source=_harness.AGENT_BENCH_DIR,
        tools={"bin": tools_bin},
        host_env=env,
    )
    provenance = dict(provenance)
    worktree_resolved = pathlib.Path(worktree).resolve()
    extra_denied: list[str] = []
    for candidate in [results_root, *answer_key_roots(graphmark_root)]:
        if candidate is None:
            continue
        resolved = pathlib.Path(candidate).resolve()
        if worktree_resolved.is_relative_to(resolved):
            continue
        if str(resolved) in extra_denied:
            continue
        # Appended AFTER run.py's re-allows: seatbelt takes the last matching
        # rule, so these denies stand while the worktree's own allow is
        # untouched.
        profile += f"(deny file-read* (subpath {json.dumps(str(resolved))}))\n"
        extra_denied.append(str(resolved))
    provenance["extra_denied_roots"] = extra_denied
    return profile, provenance


#: Set to 1 to put the worktree back inside the cell dir. DEBUG ONLY -- it
#: reintroduces the arm-name cue and is stamped into meta.json.
WORKTREE_IN_CELL_ENV = "BM_WORKTREE_IN_CELL"
#: Parent dir for the opaque worktrees. Unset -> the OS temp dir.
WORKTREE_ROOT_ENV = "BM_WORKTREE_ROOT"


def worktree_key(pair_id: str, arm: str, rep: int | None) -> str:
    """Opaque, stable, arm-neutral directory name for one cell's worktree.

    Stable so a resumed run reuses the same path; opaque so the agent's `pwd`
    carries no arm name, pair id, or instance id.
    """
    return hashlib.sha256(f"{pair_id}|{arm}|{rep}".encode("utf-8")).hexdigest()[:16]


def worktree_root() -> pathlib.Path:
    return pathlib.Path(os.environ.get(WORKTREE_ROOT_ENV) or tempfile.gettempdir())


def worktree_path(cell: pathlib.Path, pair_id: str, arm: str,
                  rep: int | None) -> tuple[pathlib.Path, bool]:
    """(path, arm_neutral). See the FAIRNESS note in the module docstring.

    The path must contain neither the arm name nor any part of the results
    tree, or the agent can read its own arm out of `pwd`.
    """
    if os.environ.get(WORKTREE_IN_CELL_ENV) == "1":
        return cell / "worktree", False
    return worktree_root() / "bm-wt" / worktree_key(pair_id, arm, rep), True


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
    adapter: Any = None,
    model: str | None = None,
    rep: int | None = None,
) -> dict:
    graphmark_root = pathlib.Path(config["graphmark_root"])
    cache = _repo.cache_dir_for(pathlib.Path(config["repo_cache"]), pair["repo"])
    cell.mkdir(parents=True, exist_ok=True)

    if adapter is None:
        adapter, _name, backends = resolve_backend(config)
        model = model or resolve_model(config, adapter.name, tier, backends)

    (cell / "packet.txt").write_text(packet.text, encoding="utf-8")
    (cell / "packet.sha256").write_text(packet.sha256 + "\n", encoding="utf-8")
    (cell / "prompt.txt").write_text(prompt, encoding="utf-8")
    (cell / "prompt_sym.sha256").write_text(symmetry_sha + "\n", encoding="utf-8")

    worktree, worktree_arm_neutral = worktree_path(cell, pair["pair_id"], arm, rep)
    env, env_prov = session_env(config, cell)
    # cell == <results_root>/<pair_id>/<arm>; the whole results tree is what the
    # sibling-artifact side channel lives in, so that is what gets denied.
    profile, iso_prov = read_isolation(config, worktree, env,
                                       results_root=cell.parent.parent)
    sandbox_wrapper = (
        [str(_harness.sandbox_executable()), "-p", profile] if profile is not None else None
    )

    meta: dict[str, Any] = {
        "pair_id": pair["pair_id"], "arm": arm, "tier": tier,
        "instance_id": pair["b"]["instance_id"], "repo": pair["repo"],
        "base_commit": pair["b"]["base_commit"],
        "backend": adapter.name,
        "model": model,
        "rep": rep,
        "rep_seed": rep_seed(config, rep),
        "packet_sha256": packet.sha256,
        "packet_provenance": packet.to_provenance(),
        "prompt_sym_sha256": symmetry_sha,
        "prompt_sha256": _harness.sha256_text(prompt),
        "env_sanitization": env_prov,
        "read_isolation": iso_prov,
        "stub_only": stub_only,
        "worktree": str(worktree),
        "worktree_arm_neutral": worktree_arm_neutral,
    }

    _repo.worktree_add(graphmark_root, cache, worktree, pair["b"]["base_commit"])
    try:
        result = adapter.run(
            prompt=prompt,
            worktree=worktree,
            model=model or "",
            timeout=config["agent"]["timeout_sec"],
            out_dir=cell,
            env=env,
            patch_collector=lambda wt, out: _repo.collect_patch(
                graphmark_root, cache, wt, out
            ),
            sandbox_wrapper=sandbox_wrapper,
            max_turns=int(config["agent"]["max_turns"]),
        )
    finally:
        _repo.worktree_remove(graphmark_root, cache, worktree)

    meta.update(result.to_meta())
    meta["cmd"] = result.cmd
    meta["returncode"] = result.returncode
    meta["result_event"] = json.loads(result.out_json_path.read_text(encoding="utf-8"))
    meta["patch_bytes"] = (result.patch or {}).get("bytes", 0)
    meta["session_id"] = result.session_id

    from .mechmetrics import session_metrics

    meta["mechmetrics"] = session_metrics(result.stream_path, backend=adapter.name)
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
    rep: int | None = None,
    backend: str | None = None,
    model: str | None = None,
    model_role: str = "primary",
    sealed_pairs: list[dict] | None = None,
) -> dict:
    arms = arms or list(config["arms"])
    adapter, backend_name, backends = resolve_backend(config, backend)
    resolved_model = resolve_model(config, backend_name, tier, backends, model, model_role)

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

    root = rep_root(out_root, rep)
    pair_root = root / pair["pair_id"]
    pair_root.mkdir(parents=True, exist_ok=True)

    packets = build_packets(
        config, pair, query, transcript_bytes, arms,
        a_created_at=a_meta.get("created_at") or "2026-01-01T00:00:00Z",
        brain_repo=pair_root / "brain-repo",
        brain_data_root=pair_root / "brain-data",
        force_empty_packet=force_empty_packet,
        sealed_pairs=sealed_pairs,
        donor_packet_lookup=(lambda donor: find_donor_packet(root, donor)),
    )

    built = {arm: prompts.build_prompt(query, packets[arm].text) for arm in arms}
    # THE SYMMETRY GATE -- before anything is spawned.
    symmetry_sha = prompts.assert_symmetric(built)

    summary: dict[str, Any] = {
        "pair_id": pair["pair_id"], "tier": tier, "arms": arms,
        "backend": backend_name, "model": resolved_model,
        "rep": rep, "rep_seed": rep_seed(config, rep),
        "results_root": str(root),
        "prompt_sym_sha256": symmetry_sha,
        "packet_sha256": {arm: packets[arm].sha256 for arm in arms},
        "packet_prep": {arm: packets[arm].meta.get("prep", {}) for arm in arms},
        "force_empty_packet": force_empty_packet,
        "cells": {},
    }

    for arm in arms:
        cell = pair_root / arm
        if resume and cell_is_complete(cell, backend_name):
            summary["cells"][arm] = {"skipped": "already complete"}
            continue
        summary["cells"][arm] = run_cell(
            config, pair, arm, packets[arm], built[arm], symmetry_sha, cell, tier,
            stub_only=stub_only, adapter=adapter, model=resolved_model, rep=rep,
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
    parser.add_argument("--backend", default=None, choices=sorted(agents.ADAPTERS),
                        help="agent backend; default comes from config/backends.json")
    parser.add_argument("--model", default=None, help="explicit model, overriding the tier")
    parser.add_argument("--model-role", default="primary",
                        choices=["primary", "generality", "alternate"],
                        help="backends.json model role used when config names no model")
    parser.add_argument("--rep", type=int, default=None,
                        help="number of replicates; writes rep0..rep<N-1> under --out")
    parser.add_argument("--rep-index", type=int, default=None,
                        help="run ONE replicate by index (for external parallelism)")
    parser.add_argument("--pairs", default=None,
                        help="sealed pair list (JSON array or dir of pair JSONs); "
                             "REQUIRED for the placebo arm's derangement")
    parser.add_argument("--null-test", action="store_true",
                        help="deliver a sentinel packet to EVERY arm (pre-registered null test)")
    parser.add_argument("--no-resume", action="store_true")
    args = parser.parse_args(argv)

    if args.rep is not None and args.rep_index is not None:
        parser.error("--rep and --rep-index are mutually exclusive")

    config = _harness.load_config(args.config)
    pair = json.loads(pathlib.Path(args.pair).read_text(encoding="utf-8"))
    out_root = pathlib.Path(args.out) if args.out else (
        _harness.BRAINMARK_DIR / "results" / "B" / args.tier
    )
    arms = args.arms.split(",") if args.arms else None
    sealed_pairs = load_sealed_pairs(args.pairs) if args.pairs else None

    if args.rep is not None:
        reps: list[int | None] = list(range(args.rep))
    elif args.rep_index is not None:
        reps = [args.rep_index]
    else:
        reps = [None]

    summaries = []
    for rep in reps:
        summaries.append(run_pair(
            config, pair, pathlib.Path(args.a_dir), out_root, args.tier,
            arms=arms, force_empty_packet=args.null_test, resume=not args.no_resume,
            rep=rep, backend=args.backend, model=args.model, model_role=args.model_role,
            sealed_pairs=sealed_pairs,
        ))

    print(_harness.pretty_json([
        {
            "pair_id": s["pair_id"], "rep": s["rep"], "backend": s["backend"],
            "model": s["model"], "prompt_sym_sha256": s["prompt_sym_sha256"],
            "packet_sha256": s["packet_sha256"],
        }
        for s in summaries
    ]), end="")
    return 0


def load_sealed_pairs(path: str | pathlib.Path) -> list[dict]:
    """A JSON array of pairs, or a directory of pair JSONs. Order-independent:
    the derangement sorts internally, so a directory listing cannot change it."""
    target = pathlib.Path(path)
    if target.is_dir():
        return [json.loads(p.read_text(encoding="utf-8"))
                for p in sorted(target.glob("*.json"))]
    payload = json.loads(target.read_text(encoding="utf-8"))
    if isinstance(payload, dict):
        for key in ("pairs", "sealed", "candidates"):
            if isinstance(payload.get(key), list):
                return list(payload[key])
        raise ValueError(f"{target}: no pair list under pairs/sealed/candidates")
    if not isinstance(payload, list):
        raise ValueError(f"{target}: expected a JSON array of pairs")
    return payload


if __name__ == "__main__":
    raise SystemExit(main())
