"""The backend-neutral agent-adapter contract.

WHY THIS LAYER EXISTS (plan 0.5 / NeurIPS generality gap): BrainMark v1 runs on
the Azure GPT family through the codex CLI, but a benchmark that can only be run
with one vendor's agent cannot claim generality. Everything above this layer --
packet building, symmetry gate, worktrees, netjail, sandbox, results layout,
resume guard, mechanism metric -- is backend-agnostic. Everything that knows how
to spawn an agent CLI, parse its stream and price its tokens lives here.

THE CONTRACT (one method, one result shape):

    adapter.run(prompt=..., worktree=..., model=..., timeout=...,
                out_dir=..., env=..., patch_collector=...) -> AgentResult

    AgentResult.stream_path    the raw agent stdout stream (NDJSON), verbatim
    AgentResult.out_json_path  ONE normalized terminal object, `cc_out.json`
    AgentResult.patch          {"path": ..., "bytes": N} or None
    AgentResult.usage          {"tokens": {...}, "usd": float|None, ...}
    AgentResult.session_id     the backend's own session/thread id, or None

`stream_path` is never rewritten: the mechanism metric and any later audit must
be able to re-derive their numbers from bytes the agent actually emitted. Only
`out_json_path` is synthesized, and it records how it was synthesized.

USAGE-FIELD MAPPING IS NOT SYMMETRIC BETWEEN BACKENDS AND WE DO NOT PRETEND IT
IS. See compute_usd() below and each adapter's `usage()` docstring: Claude
reports a provider-computed `total_cost_usd` and cache tokens DISJOINT from
input tokens; codex reports no cost at all and its `cached_input_tokens` is a
SUBSET of `input_tokens`. A codex usd figure is therefore COMPUTED by this
harness from tokens x a price table, is flagged `estimated: true`, and carries
the price table's `status` so a placeholder rate card can never be quoted as a
measured cost.
"""

from __future__ import annotations

import abc
import dataclasses
import json
import os
import pathlib
import re
import signal
import subprocess
import time
from typing import Any, Callable, Iterable

if __package__ in (None, ""):  # pragma: no cover - direct-script execution
    import sys

    sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2]))
    from brainmark import _harness  # type: ignore[no-redef]
else:
    from .. import _harness

AGENTS_DIR = pathlib.Path(__file__).resolve().parent
BACKENDS_JSON = AGENTS_DIR / "backends.json"

#: Environment variables an adapter is allowed to re-inject AFTER run.py's
#: sanitizer has run. The sanitizer strips harness-identifying variables; a
#: provider credential is not one of those, but it must never reach meta.json.
SECRET_ENV_PATTERN = re.compile(r"(KEY|TOKEN|SECRET|PASSWORD)$", re.IGNORECASE)


class AgentBackendError(RuntimeError):
    """Backend setup failed. Never degrade into a run: a session spawned with a
    half-configured backend produces a real-looking transcript with unreal
    provenance, which is worse than no session at all."""


@dataclasses.dataclass(frozen=True)
class AgentResult:
    backend: str
    model: str
    session_id: str | None
    stream_path: pathlib.Path
    out_json_path: pathlib.Path
    patch: dict[str, Any] | None
    usage: dict[str, Any]
    returncode: int
    duration_s: float
    cmd: list[str]
    meta: dict[str, Any]

    def to_meta(self) -> dict[str, Any]:
        return {
            "backend": self.backend,
            "model": self.model,
            "session_id": self.session_id,
            "stream_path": str(self.stream_path),
            "out_json_path": str(self.out_json_path),
            "patch": self.patch,
            "usage": self.usage,
            "returncode": self.returncode,
            "duration_s": self.duration_s,
            "cmd": self.cmd,
            **self.meta,
        }


# --------------------------------------------------------------------------
# backends.json
# --------------------------------------------------------------------------


def load_backends(path: str | pathlib.Path | None = None) -> dict:
    """Read agents/backends.json (the backend pin file).

    Deliberately NOT ../config.json: config.json pins the experiment (arms,
    packet budget, seal), backends.json pins the machine. They are owned by
    different parts of this harness and are hashed separately.
    """
    cfg_path = pathlib.Path(path) if path else BACKENDS_JSON
    if not cfg_path.is_file():
        raise AgentBackendError(f"backend pin file not found at {cfg_path}")
    text = cfg_path.read_text(encoding="utf-8")
    cfg = json.loads(text)
    cfg["_backends_path"] = str(cfg_path)
    cfg["_backends_sha256"] = _harness.sha256_text(text)
    return cfg


def redact_env(env: dict[str, str], keys: Iterable[str]) -> dict[str, str]:
    """Values for secret-looking keys become a sha256 prefix, never the secret."""
    out: dict[str, str] = {}
    for key in keys:
        value = env.get(key)
        if value is None:
            continue
        if SECRET_ENV_PATTERN.search(key):
            out[key] = "sha256:" + _harness.sha256_text(value)[:12]
        else:
            out[key] = value
    return out


def read_env_file(path: str | pathlib.Path) -> dict[str, str]:
    """Parse a `KEY=value` / `export KEY=value` shell env file.

    Used for ~/.azure_env. Intentionally does NOT execute the file: sourcing an
    operator's shell file into a benchmark process is how unrelated state leaks
    into a measurement.
    """
    resolved = pathlib.Path(path).expanduser()
    if not resolved.is_file():
        return {}
    values: dict[str, str] = {}
    for raw in resolved.read_text(encoding="utf-8", errors="replace").splitlines():
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        if line.startswith("export "):
            line = line[len("export "):].strip()
        if "=" not in line:
            continue
        key, _, value = line.partition("=")
        key = key.strip()
        value = value.strip()
        if len(value) >= 2 and value[0] == value[-1] and value[0] in "\"'":
            value = value[1:-1]
        if key:
            values[key] = value
    return values


# --------------------------------------------------------------------------
# pricing
# --------------------------------------------------------------------------


def compute_usd(backend: str, model: str, tokens: dict[str, Any],
                prices: dict[str, Any]) -> dict[str, Any]:
    """Price a token vector. THE TWO PROVIDER CONVENTIONS ARE DIFFERENT.

    Anthropic (claude): `input_tokens` EXCLUDES cache reads and cache writes;
    the three counters are disjoint and are each billed at their own rate.

        usd = in*Pin + cache_read*Pcr + cache_creation*Pcc + out*Pout

    OpenAI/codex: `cached_input_tokens` is a SUBSET of `input_tokens`; the
    cached part bills at the discounted rate and only the remainder bills at the
    full input rate. Applying the Anthropic formula to a codex vector would
    double-count every cached token.

        usd = (in - cached)*Pin + cached*Pcr + out*Pout

    Codex exposes no cache-WRITE counter at all (run.py:7420 records this as a
    frozen provider-quote decision, not a parser inference), so no cache-write
    term exists on that path -- it is omitted, not assumed zero-cost.

    Returns a dict, never a bare float, because the number is only meaningful
    beside `estimated` and `price_status`.
    """
    row = (prices or {}).get(model) or {}
    if not row:
        return {
            "usd": None,
            "estimated": True,
            "price_status": "no_price_row",
            "error": f"no price row for model {model!r} in backends.json",
        }

    def per_m(key: str) -> float | None:
        value = row.get(key)
        return float(value) if isinstance(value, (int, float)) else None

    p_in, p_out = per_m("input_per_million"), per_m("output_per_million")
    p_cr, p_cc = per_m("cache_read_per_million"), per_m("cache_creation_per_million")
    if p_in is None or p_out is None:
        return {"usd": None, "estimated": True, "price_status": row.get("status"),
                "error": "price row lacks input/output rates"}

    def count(key: str) -> int:
        value = tokens.get(key)
        return int(value) if isinstance(value, (int, float)) and not isinstance(value, bool) else 0

    inp, out = count("input_tokens"), count("output_tokens")
    cache_read, cache_creation = count("cache_read_tokens"), count("cache_creation_tokens")

    if backend == "codex":
        uncached = max(inp - cache_read, 0)
        usd = (uncached * p_in + cache_read * (p_cr if p_cr is not None else p_in)
               + out * p_out) / 1_000_000
        basis = "openai_cached_input_is_subset_of_input"
    else:
        usd = (inp * p_in
               + cache_read * (p_cr if p_cr is not None else p_in)
               + cache_creation * (p_cc if p_cc is not None else p_in)
               + out * p_out) / 1_000_000
        basis = "anthropic_disjoint_input_cacheread_cachewrite"

    return {
        "usd": round(usd, 8),
        "estimated": True,
        "price_status": row.get("status"),
        "accounting_basis": basis,
        "prices_per_million": {k: row.get(k) for k in sorted(row) if k.endswith("_per_million")},
    }


# --------------------------------------------------------------------------
# stream helpers shared by every adapter
# --------------------------------------------------------------------------


def iter_ndjson(path: pathlib.Path) -> Iterable[dict]:
    """Yield JSON objects from an NDJSON stream, tolerating a truncated tail.

    A killed-on-timeout session ends mid-line. Losing that one line is correct;
    losing the whole session's metrics because of it is not.
    """
    if not path.is_file():
        return
    for line in path.read_text(encoding="utf-8", errors="replace").splitlines():
        line = line.strip()
        if not line.startswith("{"):
            continue
        try:
            payload = json.loads(line)
        except json.JSONDecodeError:
            continue
        if isinstance(payload, dict):
            yield payload


class AgentAdapter(abc.ABC):
    """One agent CLI. Subclasses own command construction and stream parsing."""

    #: registry key, e.g. "codex"
    name: str = ""

    def __init__(self, spec: dict[str, Any], prices: dict[str, Any] | None = None) -> None:
        self.spec = dict(spec or {})
        self.prices = dict(prices or {})

    # -- subclass surface ---------------------------------------------------

    @abc.abstractmethod
    def build_command(self, prompt: str, worktree: pathlib.Path, model: str,
                      **kwargs: Any) -> list[str]:
        """Argv for one session. The prompt MUST be the last element so callers
        can log `cmd[:-1] + ["<PROMPT>"]` without leaking the packet."""

    @abc.abstractmethod
    def prepare_env(self, env: dict[str, str], out_dir: pathlib.Path,
                    state_dir: pathlib.Path | None = None) -> tuple[dict[str, str], dict]:
        """Backend-specific env on top of the caller's sanitized env.

        Returns (env, provenance). The provenance must be safe to write to
        meta.json: run it through redact_env() for anything credential-shaped.

        `state_dir` is where the backend puts its own home (CODEX_HOME /
        CLAUDE_CONFIG_DIR). The CALLER chooses it because the choice is a
        fairness property, not a backend detail: those variables reach the
        model's shell, so a session-B caller must hand over an arm-neutral,
        opaque path (run_b.agent_state_path) rather than let it default into
        `<results>/<pair_id>/<arm>/`. `None` keeps the per-out_dir default,
        which is what session A -- one directory per PAIR, no arms -- wants.
        """

    @abc.abstractmethod
    def normalize_result(self, stream_path: pathlib.Path, model: str,
                         returncode: int, duration_s: float) -> tuple[dict, dict, str | None]:
        """(cc_out_object, usage, session_id) from the raw stream."""

    # -- the contract -------------------------------------------------------

    def run(
        self,
        *,
        prompt: str,
        worktree: pathlib.Path,
        model: str,
        timeout: int,
        out_dir: pathlib.Path,
        env: dict[str, str] | None = None,
        patch_collector: Callable[[pathlib.Path, pathlib.Path], int] | None = None,
        sandbox_wrapper: list[str] | None = None,
        state_dir: pathlib.Path | None = None,
        **kwargs: Any,
    ) -> AgentResult:
        """Spawn one session and return the normalized result.

        `patch_collector(worktree, out_patch) -> bytes` is invoked BEFORE the
        caller tears the worktree down, because patch collection is the one
        backend-neutral step that must happen while the worktree still exists.
        Patch collection stays in _repo.py (it holds the graphmark cache lock);
        this layer only decides WHEN it runs.
        """
        out_dir = pathlib.Path(out_dir)
        out_dir.mkdir(parents=True, exist_ok=True)
        stream_path = out_dir / "stream.jsonl"
        out_json_path = out_dir / "cc_out.json"
        err_path = out_dir / "cc_err.log"

        child_env, env_prov = self.prepare_env(dict(env or os.environ), out_dir, state_dir)
        # The command is built from the CHILD env, not the parent's: the codex
        # adapter reads its Azure endpoint from there, and reading it from
        # os.environ instead would let an unsanitized variable pick the provider.
        cmd = self.build_command(prompt, worktree, model, env=child_env, **kwargs)
        if sandbox_wrapper:
            cmd = [*sandbox_wrapper, *cmd]

        started = time.monotonic()
        timed_out = False
        try:
            with stream_path.open("wb") as out_fh, err_path.open("wb") as err_fh:
                proc = subprocess.Popen(
                    cmd, cwd=str(worktree), env=child_env, stdin=subprocess.DEVNULL,
                    stdout=out_fh, stderr=err_fh, start_new_session=(os.name != "nt"),
                )
                try:
                    proc.wait(timeout=timeout)
                except subprocess.TimeoutExpired:
                    if os.name == "nt":
                        subprocess.run(["taskkill", "/PID", str(proc.pid), "/T", "/F"],
                                       capture_output=True, check=True)
                    else:
                        try:
                            os.killpg(proc.pid, signal.SIGKILL)
                        except ProcessLookupError:
                            pass
                    proc.wait()
                    raise
            returncode = proc.returncode
        except subprocess.TimeoutExpired:
            # 124 is the shell convention and what run.py records for a killed
            # agent. The partial stream on disk is kept and still parsed.
            returncode = 124
            timed_out = True
        except FileNotFoundError as exc:
            raise AgentBackendError(
                f"backend {self.name}: CLI not found on PATH ({cmd[0]!r}): {exc}"
            ) from exc
        duration_s = round(time.monotonic() - started, 4)

        cc_out, usage, session_id = self.normalize_result(
            stream_path, model, returncode, duration_s
        )
        out_json_path.write_text(
            json.dumps(cc_out, indent=2, sort_keys=True) + "\n", encoding="utf-8"
        )

        patch: dict[str, Any] | None = None
        if patch_collector is not None:
            patch_path = out_dir / "patch.diff"
            patch = {"path": str(patch_path), "bytes": int(patch_collector(worktree, patch_path))}

        return AgentResult(
            backend=self.name, model=model, session_id=session_id,
            stream_path=stream_path, out_json_path=out_json_path,
            patch=patch, usage=usage, returncode=returncode, duration_s=duration_s,
            cmd=[*cmd[:-1], "<PROMPT>"],
            meta={"env_backend": env_prov, "timed_out": timed_out,
                  "backends_sha256": self.spec.get("_backends_sha256")},
        )
