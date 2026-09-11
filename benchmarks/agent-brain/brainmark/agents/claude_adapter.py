"""Claude Code backend. EXTRACTED VERBATIM from session_a.py / run_b.py.

This adapter is the previous inline driver, moved behind the AgentAdapter
contract with no behavioural change:

  * argv is byte-identical to run_b.build_command / session_a.build_command
    (which themselves mirror run.py:7006), including `--safe-mode` and the
    empty `--mcp-config`, so nothing about a claude cell moved when the agents/
    layer landed;
  * `cc_out.json` is still the stream's terminal `result` event, so the
    resume guard (`total_cost_usd > 0`) and every downstream reader are
    unchanged;
  * session A still runs WITH session persistence (that is what writes the
    native JSONL we harvest) and session B still runs with
    `--no-session-persistence`; the difference is now one keyword argument
    (`session_persistence`) rather than two copies of the argv.

STATUS: kept in-tree, NOT used for v1 confirmatory runs (plan 0.5 makes the
codex/Azure adapter primary). It stays because a second backend is what makes
the mechanism taxonomy testable as backend-neutral rather than merely asserted.

USAGE MAPPING (claude): the provider reports cost, so `usage.usd` is the
provider's own `total_cost_usd` and `estimated` is False. Token counters come
from `modelUsage` when present, else `usage`, and Anthropic's three input
counters (input / cache_read / cache_creation) are DISJOINT.
"""

from __future__ import annotations

import pathlib
from typing import Any

if __package__ in (None, ""):  # pragma: no cover
    import sys

    sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2]))
    from brainmark.agents.base import AgentAdapter, iter_ndjson  # type: ignore[no-redef]
else:
    from .base import AgentAdapter, iter_ndjson

#: run.py:7006 argv, minus MCP (BrainMark delivers memory through the prompt
#: packet, never through a live tool).
STATIC_ARGS = (
    "--print",
    "--strict-mcp-config",
    "--mcp-config", '{"mcpServers":{}}',
    "--disable-slash-commands",
    "--permission-mode", "bypassPermissions",
    "--output-format", "stream-json",
    "--verbose",
    "--safe-mode",
)


class ClaudeAdapter(AgentAdapter):
    name = "claude"

    def build_command(self, prompt: str, worktree: pathlib.Path, model: str,
                      max_turns: int = 60, session_persistence: bool = False,
                      **_ignored: Any) -> list[str]:
        cli = self.spec.get("cli", "claude")
        cmd = [cli, *STATIC_ARGS]
        if not session_persistence:
            # B-style: run.py:7009. A-style keeps persistence so the native
            # JSONL that every memory source is built from actually exists.
            cmd.insert(1, "--no-session-persistence")
        if model:
            cmd.extend(["--model", model])
        cmd.extend(["--max-turns", str(max_turns)])
        cmd.extend(self.spec.get("extra_args") or [])
        cmd.append(prompt)
        return cmd

    def prepare_env(self, env: dict[str, str], out_dir: pathlib.Path,
                    state_dir: pathlib.Path | None = None) -> tuple[dict[str, str], dict]:
        """A per-cell CLAUDE_CONFIG_DIR: isolation, and the only way session A's
        native JSONL is harvestable without touching the operator's own sessions.

        CLAUDE_CONFIG_DIR reaches the model's shell, so its VALUE is an
        arm-visible cue; `state_dir` lets a session-B caller supply an
        arm-neutral path. Defaulting to `<out_dir>/claude-config` keeps session
        A -- one directory per pair, no arms -- exactly where
        harvest_native_jsonl() looks for it."""
        env = dict(env)
        config_dir = pathlib.Path(state_dir) if state_dir else pathlib.Path(out_dir) / "claude-config"
        config_dir.mkdir(parents=True, exist_ok=True)
        env["CLAUDE_CONFIG_DIR"] = str(config_dir)
        return env, {"backend": "claude", "CLAUDE_CONFIG_DIR": str(config_dir)}

    def normalize_result(self, stream_path: pathlib.Path, model: str,
                         returncode: int, duration_s: float) -> tuple[dict, dict, str | None]:
        result: dict = {}
        session_id: str | None = None
        for event in iter_ndjson(stream_path):
            etype = event.get("type")
            if etype == "result":
                result = event  # last one wins, as in session_a.extract_result_event
            elif etype == "system" and event.get("subtype") == "init":
                candidate = event.get("session_id")
                if isinstance(candidate, str) and candidate:
                    session_id = candidate
            if session_id is None:
                candidate = event.get("session_id")
                if isinstance(candidate, str) and candidate:
                    session_id = candidate

        usage = {
            "backend": "claude",
            "model": model,
            "tokens": _tokens(result),
            "usd": result.get("total_cost_usd"),
            "estimated": False,
            "usd_source": "provider_total_cost_usd",
            "num_turns": result.get("num_turns"),
            "duration_ms": result.get("duration_ms"),
            "subtype": result.get("subtype"),
            "is_error": bool(result.get("is_error")),
            "complete": bool(result),
        }
        return result, usage, session_id


def _tokens(result: dict) -> dict[str, Any]:
    """modelUsage first (run.py:7505 makes the same choice), else `usage`."""
    totals = {"input_tokens": 0, "output_tokens": 0,
              "cache_read_tokens": 0, "cache_creation_tokens": 0}
    aliases = {
        "input_tokens": ("input_tokens",),
        "output_tokens": ("output_tokens",),
        "cache_read_tokens": ("cache_read_input_tokens", "cache_read_tokens"),
        "cache_creation_tokens": ("cache_creation_input_tokens", "cache_creation_tokens"),
    }
    model_usage = result.get("modelUsage")
    source = None
    rows: list[dict] = []
    if isinstance(model_usage, dict) and model_usage:
        source = "modelUsage"
        rows = [row for row in model_usage.values() if isinstance(row, dict)]
    elif isinstance(result.get("usage"), dict):
        source = "usage"
        rows = [result["usage"]]
    for row in rows:
        for canonical, keys in aliases.items():
            for key in keys:
                value = row.get(key)
                if isinstance(value, (int, float)) and not isinstance(value, bool):
                    totals[canonical] += int(value)
                    break
    totals["total_tokens"] = sum(
        totals[k] for k in ("input_tokens", "output_tokens",
                            "cache_read_tokens", "cache_creation_tokens")
    )
    totals["source"] = source
    return totals
