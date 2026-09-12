"""Shared plumbing: load `benchmarks/agent-brain/run.py` AS A LIBRARY.

run.py is the normative fairness implementation for this repo's agent benchmarks
(CONDITIONS.md is its contract). BrainMark reuses its primitives instead of
re-implementing them, so a fairness fix in run.py propagates here for free.

run.py guards its CLI behind `if __name__ == "__main__"` (run.py:11130), so
importing it by path is side-effect free -- verified: import completes in ~0.1s
and touches no filesystem state.

DO NOT MODIFY run.py. If a primitive is genuinely unusable from here, vendor
that ONE function into this file with a provenance comment naming run.py and the
line number -- never fork the file.
"""

from __future__ import annotations

import importlib.util
import json
import os
import pathlib
import re
import sys
import types
from typing import Any

BRAINMARK_DIR = pathlib.Path(__file__).resolve().parent
AGENT_BENCH_DIR = BRAINMARK_DIR.parent
RUN_PY = AGENT_BENCH_DIR / "run.py"

#: `${NAME}` in a config.json string value is expanded from the environment
#: at load time -- see `_expand_config_env` below. Same convention (and same
#: deliberate `${...}` not `$NAME`) as memsources/mem0_source.py's expand_env.
_CONFIG_ENV_REF = re.compile(r"\$\{([A-Za-z_][A-Za-z0-9_]*)\}")

#: Defaults for the machine-local OPERATIONAL paths config.json references by
#: `${VAR}` -- WHERE this checkout's sibling repos happen to live on disk,
#: never anything that changes a measured result (those stay literal pins).
#: Home-relative so no contributor's username is ever committed; override any
#: of these per machine by setting the env var instead of editing config.json.
_CONFIG_ENV_DEFAULTS: dict[str, str] = {
    "BRAINMARK_GRAPHMARK_ROOT": str(pathlib.Path.home() / "devenv" / "graphmark" / "agentic-swebench"),
    "BRAINMARK_REPO_CACHE": str(
        pathlib.Path.home() / "devenv" / "graphmark" / "agentic-swebench" / "repo-cache"
    ),
    "BRAINMARK_CMM_PATCH_PATH": str(
        pathlib.Path.home() / "devenv" / "eg-memharness" / "bench" / "memory" / "patches"
        / "0005-cmm-v0.9.0-markdown-sections.patch"
    ),
}


def _expand_config_env_string(value: str) -> str:
    def sub(match: "re.Match[str]") -> str:
        name = match.group(1)
        resolved = os.environ.get(name)
        if resolved:
            return resolved
        default = _CONFIG_ENV_DEFAULTS.get(name)
        if default is not None:
            return default
        raise RuntimeError(
            f"config.json references ${{{name}}} but it is unset and has no built-in default"
        )
    return _CONFIG_ENV_REF.sub(sub, value)


#: Dotted paths of the ONLY config.json keys eagerly `${VAR}`-expanded at load
#: time -- machine-local operational paths every caller of load_config()
#: needs resolved immediately. Deliberately NOT a blanket recursive walk over
#: the whole config: competitors.mem0.llm.config_extra also carries `${VAR}`
#: refs (e.g. `${AZURE_AI_ENDPOINT}`), but those are tenant secrets expanded
#: LAZILY and separately by memsources/mem0_source.py:expand_env, only when
#: the mem0 arm actually runs -- eagerly expanding them here would make
#: merely loading config.json (every test, every script) require that
#: variable to be set even when nothing touches the mem0 arm.
_CONFIG_ENV_KEYS: tuple[tuple[str, ...], ...] = (
    ("graphmark_root",),
    ("repo_cache",),
    ("competitors", "cmm", "patch", "path"),
)


def _expand_config_env(cfg: dict) -> dict:
    for key_path in _CONFIG_ENV_KEYS:
        node: Any = cfg
        for key in key_path[:-1]:
            if not isinstance(node, dict) or key not in node:
                node = None
                break
            node = node[key]
        if not isinstance(node, dict):
            continue
        leaf = key_path[-1]
        if isinstance(node.get(leaf), str):
            node[leaf] = _expand_config_env_string(node[leaf])
    return cfg


_CACHED: types.ModuleType | None = None


def harness() -> types.ModuleType:
    """Import benchmarks/agent-brain/run.py as the module `brainmark_run`."""
    global _CACHED
    if _CACHED is not None:
        return _CACHED
    if not RUN_PY.is_file():
        raise RuntimeError(f"cannot find the agent-brain harness at {RUN_PY}")
    spec = importlib.util.spec_from_file_location("brainmark_run", RUN_PY)
    if spec is None or spec.loader is None:
        raise RuntimeError(f"cannot load {RUN_PY} as a module")
    module = importlib.util.module_from_spec(spec)
    # Register before exec so any internal self-reference resolves.
    sys.modules["brainmark_run"] = module
    spec.loader.exec_module(module)
    _CACHED = module
    return module


# --- primitives re-exported by name, so callers never reach into run.py ------
#
# run.py:4731 bound_memory_packet(stdout, max_bytes) -> (text, meta)
# run.py:4840 packet_contains_reserved_delimiter(text) -> bool
# run.py:5091 sanitize_harness_agent_environment(env) -> (env, provenance)
# run.py:5125 temporal_agent_read_isolation(...) -> (sandbox_profile, provenance)
# run.py:1391 stable_json_sha256(value) -> str
# run.py:464  FROZEN_MEMORY_PACKET_END_TAG
# run.py:463  TEMPORAL_AGENT_SANDBOX_EXECUTABLE
# run.py:7843 collect_json_tool_events(value, worktree) -> list[dict]


def bound_memory_packet(stdout: str, max_bytes: int) -> tuple[str, dict]:
    return harness().bound_memory_packet(stdout, max_bytes)


def packet_contains_reserved_delimiter(packet_text: str) -> bool:
    return harness().packet_contains_reserved_delimiter(packet_text)


def sanitize_harness_agent_environment(env: dict[str, str]) -> tuple[dict[str, str], dict]:
    return harness().sanitize_harness_agent_environment(env)


def temporal_agent_read_isolation(*args, **kwargs) -> tuple[str, dict]:
    return harness().temporal_agent_read_isolation(*args, **kwargs)


def stable_json_sha256(value) -> str:
    return harness().stable_json_sha256(value)


def collect_json_tool_events(value, worktree: str | None = None) -> list[dict]:
    return harness().collect_json_tool_events(value, worktree)


def packet_end_tag() -> str:
    return harness().FROZEN_MEMORY_PACKET_END_TAG


def packet_begin_tag() -> str:
    # run.py:6786 emits the opening tag inline; it is the end tag that is a
    # module constant (it is the one an injected packet could forge).
    return "<frozen-memory-packet>"


def sandbox_executable() -> pathlib.Path:
    return harness().TEMPORAL_AGENT_SANDBOX_EXECUTABLE


# --- brainmark-local helpers -------------------------------------------------


def canonical_json(value) -> str:
    """Byte-stable JSON. Same convention as run.py:4733 _canonical_packet_json."""
    return json.dumps(value, ensure_ascii=False, separators=(",", ":"), sort_keys=True)


def pretty_json(value) -> str:
    """Deterministic human-readable JSON for artifacts committed to git."""
    return json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True) + "\n"


def load_config(path: str | pathlib.Path | None = None) -> dict:
    cfg_path = pathlib.Path(path) if path else BRAINMARK_DIR / "config.json"
    raw_text = cfg_path.read_text(encoding="utf-8")
    # The pin's sha256 is over the COMMITTED (unexpanded) bytes: which sibling
    # repo a machine points graphmark_root/repo_cache at is not part of what
    # is being measured, so it must not perturb the config pin.
    cfg = _expand_config_env(json.loads(raw_text))
    cfg["_config_path"] = str(cfg_path)
    cfg["_config_sha256"] = sha256_text(raw_text)
    return cfg


def sha256_text(text: str) -> str:
    import hashlib

    return hashlib.sha256(text.encode("utf-8")).hexdigest()


def sha256_bytes(data: bytes) -> str:
    import hashlib

    return hashlib.sha256(data).hexdigest()


def sha256_file(path: str | pathlib.Path) -> str:
    return sha256_bytes(pathlib.Path(path).read_bytes())


def display_path(path: str | pathlib.Path) -> str:
    """A path safe to write into a TRACKED artifact (ledger, provenance, log).

    Absolute machine-local paths bake a contributor's username into a
    committed file the moment a script runs once and writes its output.
    Collapse anything under the current user's home directory to a `~/...`
    form -- publication-safe (no username survives) and still legible/useful
    for a human reading the artifact. Paths outside the home directory (e.g.
    already-relative paths) are returned unchanged.
    """
    p = pathlib.Path(path)
    home = pathlib.Path.home()
    try:
        return "~/" + p.relative_to(home).as_posix()
    except ValueError:
        return str(path)
