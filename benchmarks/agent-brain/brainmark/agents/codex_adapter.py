"""Codex CLI backend against Azure OpenAI. PRIMARY backend for BrainMark v1.

Plan 0.5: v1 runs on the Azure GPT family (deployments `gpt-5`, `gpt-5.6-sol`,
`gpt-5.6-terra`); the claude adapter stays in-tree but is not used for
confirmatory runs. Generality in v1 = >=2 GPT models; cross-vendor is a
post-validation expansion and is stated as a limitation, not claimed.

WIRING PROVENANCE: argv mirrors run.py:6983-6999 (`codex exec --ephemeral
--ignore-user-config --ignore-rules --sandbox workspace-write --json --cd ...`,
`--model`, `--config model_reasoning_effort=...`). run.py's own MCP wiring
(`codex_mcp_config_args`, run.py:6835) is deliberately NOT used: BrainMark
delivers memory as a frozen prompt packet, so no arm gets a live MCP server and
the flags that would attach one must be absent for every arm equally.

AZURE CONFIG comes from ~/.azure_env (env names AZURE_AI_ENDPOINT /
AZURE_AI_API_KEY / AZURE_AI_API_VERSION; real process env wins over the file),
and is passed to codex as a custom model provider:

    model_provider="azure"
    model_providers.azure.base_url  = <AZURE_AI_ENDPOINT>/openai
    model_providers.azure.env_key   = "AZURE_AI_API_KEY"
    model_providers.azure.query_params = {"api-version"=<AZURE_AI_API_VERSION>}
    model_providers.azure.wire_api  = "responses"

`--ignore-user-config` means the operator's ~/.codex/config.toml can never
change what a measured session does; every knob that matters is on the command
line and therefore in meta.json. CODEX_HOME is repointed per cell so auth and
any state codex still writes cannot cross between cells.

USAGE MAPPING -- CODEX vs CLAUDE, STATED HONESTLY:

  | field        | claude                         | codex                        |
  |--------------|--------------------------------|------------------------------|
  | cost         | provider `total_cost_usd`      | NOT REPORTED -> computed by  |
  |              | (measured)                     | this harness, tokens x prices|
  |              |                                | from backends.json, flagged  |
  |              |                                | estimated=true + price_status|
  | input tokens | disjoint from cache counters   | INCLUDES cached_input_tokens |
  | cache write  | cache_creation_input_tokens    | no counter exists at all     |
  | turns        | result.num_turns               | count of turn.completed      |
  | session id   | result/system.session_id       | thread.started.thread_id     |

  Cumulative-vs-additive: codex `turn.completed.usage` is a CUMULATIVE snapshot
  for the invocation, so snapshots are never summed -- the last one is the
  total (run.py:7429). Summing them would silently inflate every codex cell.

A codex usd is therefore NEVER a measured cost. Any report that prints it must
print `price_status` beside it; backends.json currently ships
UNVERIFIED_PLACEHOLDER rates for the Azure deployments.
"""

from __future__ import annotations

import json
import os
import pathlib
from typing import Any

if __package__ in (None, ""):  # pragma: no cover
    import sys

    sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2]))
    from brainmark import _harness  # type: ignore[no-redef]
    from brainmark.agents.base import (  # type: ignore[no-redef]
        AgentAdapter, AgentBackendError, compute_usd, iter_ndjson, read_env_file, redact_env,
    )
else:
    from .. import _harness
    from .base import (
        AgentAdapter, AgentBackendError, compute_usd, iter_ndjson, read_env_file, redact_env,
    )

#: codex item kinds that represent the agent DOING something we can classify.
#: Exported so mechmetrics.py has one definition of the codex vocabulary.
COMMAND_ITEM_TYPES = frozenset({"command_execution", "local_shell_call"})
EDIT_ITEM_TYPES = frozenset({"file_change", "patch_apply", "apply_patch"})
MCP_ITEM_TYPES = frozenset({"mcp_tool_call"})
WEB_ITEM_TYPES = frozenset({"web_search"})


def toml_quote(value: str) -> str:
    """run.py:6831 -- a TOML basic string is a JSON string for our inputs."""
    return json.dumps(value)


class CodexAdapter(AgentAdapter):
    name = "codex"

    # -- azure ---------------------------------------------------------------

    def azure_settings(self, env: dict[str, str]) -> dict[str, str]:
        """Resolve endpoint/key/api-version. Process env wins over ~/.azure_env.

        Fails LOUDLY when the endpoint or key is missing: a codex session that
        silently falls back to the operator's ChatGPT auth would be a different
        model on a different account than the one the report names.
        """
        cfg = self.spec.get("azure") or {}
        file_values = read_env_file(cfg.get("env_file") or "~/.azure_env")
        merged = {**file_values, **{k: v for k, v in env.items() if k.startswith("AZURE_")}}

        endpoint = merged.get(cfg.get("endpoint_env") or "AZURE_AI_ENDPOINT", "").strip()
        api_key = merged.get(cfg.get("api_key_env") or "AZURE_AI_API_KEY", "").strip()
        api_version = (
            merged.get(cfg.get("api_version_env") or "AZURE_AI_API_VERSION", "").strip()
            or str(cfg.get("default_api_version") or "")
        )
        if not endpoint or not api_key:
            raise AgentBackendError(
                "codex/azure backend is not configured: need "
                f"{cfg.get('endpoint_env', 'AZURE_AI_ENDPOINT')} and "
                f"{cfg.get('api_key_env', 'AZURE_AI_API_KEY')} in the environment or "
                f"{cfg.get('env_file', '~/.azure_env')}"
            )
        base = endpoint.rstrip("/")
        suffix = str(cfg.get("base_url_suffix") or "")
        if suffix and not base.endswith(suffix):
            base += suffix
        return {"base_url": base, "api_key": api_key, "api_version": api_version,
                "api_key_env": cfg.get("api_key_env") or "AZURE_AI_API_KEY",
                "provider_key": cfg.get("provider_key") or "azure",
                "wire_api": cfg.get("wire_api") or "responses"}

    def azure_config_args(self, azure: dict[str, str]) -> list[str]:
        key = azure["provider_key"]
        args = [
            "--config", f"model_provider={toml_quote(key)}",
            "--config", f"model_providers.{key}.name={toml_quote('Azure OpenAI')}",
            "--config", f"model_providers.{key}.base_url={toml_quote(azure['base_url'])}",
            "--config", f"model_providers.{key}.env_key={toml_quote(azure['api_key_env'])}",
            "--config", f"model_providers.{key}.wire_api={toml_quote(azure['wire_api'])}",
        ]
        if azure.get("api_version"):
            # Inline TOML table with a QUOTED key: `api-version` contains a dash
            # and is not a bare key.
            args += ["--config",
                     f"model_providers.{key}.query_params="
                     f'{{"api-version"={toml_quote(azure["api_version"])}}}']
        return args

    # -- adapter surface -----------------------------------------------------

    def build_command(self, prompt: str, worktree: pathlib.Path, model: str,
                      max_turns: int | None = None, effort: str | None = None,
                      env: dict[str, str] | None = None, **_ignored: Any) -> list[str]:
        azure = self.azure_settings(dict(env or os.environ))
        cmd = [
            self.spec.get("cli", "codex"),
            "exec",
            "--ephemeral",
            "--ignore-user-config",
            "--ignore-rules",
            "--skip-git-repo-check",
            "--sandbox", str(self.spec.get("sandbox") or "workspace-write"),
            "--json",
            "--cd", str(worktree),
        ]
        cmd += self.azure_config_args(azure)
        if model:
            cmd += ["--model", model]
        effort = effort or self.spec.get("reasoning_effort")
        if effort:
            cmd += ["--config", f'model_reasoning_effort="{effort}"']
        # NOTE: codex exposes no --max-turns. `max_turns` is a claude-only
        # bound; the codex arm is bounded by the wall-clock timeout instead.
        # Recording that asymmetry is the point of this comment -- it must show
        # up in the limitations section, not be quietly equated.
        cmd += list(self.spec.get("extra_args") or [])
        cmd.append(prompt)
        return cmd

    def prepare_env(self, env: dict[str, str], out_dir: pathlib.Path) -> tuple[dict[str, str], dict]:
        env = dict(env)
        azure = self.azure_settings(env)
        codex_home = pathlib.Path(out_dir) / "codex-home"
        codex_home.mkdir(parents=True, exist_ok=True)
        env["CODEX_HOME"] = str(codex_home)
        # run.py's sanitizer runs BEFORE this and strips harness variables; the
        # provider credential is re-injected here and only here.
        env[azure["api_key_env"]] = azure["api_key"]
        env.setdefault("AZURE_AI_ENDPOINT", azure["base_url"])
        if azure.get("api_version"):
            env.setdefault("AZURE_AI_API_VERSION", azure["api_version"])
        provenance = {
            "backend": "codex",
            "CODEX_HOME": str(codex_home),
            "azure_base_url": azure["base_url"],
            "azure_api_version": azure.get("api_version"),
            "azure_wire_api": azure["wire_api"],
            **redact_env(env, [azure["api_key_env"]]),
        }
        return env, provenance

    def normalize_result(self, stream_path: pathlib.Path, model: str,
                         returncode: int, duration_s: float) -> tuple[dict, dict, str | None]:
        events = list(iter_ndjson(stream_path))
        session_id = _session_id(events)
        tokens, parser_meta = self._tokens(stream_path, events)
        priced = compute_usd(self.name, model, tokens, self.prices)

        completed = [e for e in events if e.get("type") == "turn.completed"]
        failed = [e for e in events if e.get("type") in ("turn.failed", "error")]
        subtype = "success" if (returncode == 0 and completed and not failed) else "error"

        # THE SYNTHESIZED TERMINAL OBJECT. Shaped like claude's `result` event so
        # every downstream reader (resume guard, report.py gate, grade.py) works
        # unchanged -- and marked as synthesized so nobody mistakes an estimated
        # cost for a provider-reported one.
        cc_out = {
            "type": "result",
            "subtype": subtype,
            "is_error": subtype != "success",
            "num_turns": tokens.get("turns"),
            "duration_ms": int(duration_s * 1000),
            "total_cost_usd": priced.get("usd"),
            "session_id": session_id,
            "usage": {
                "input_tokens": tokens.get("input_tokens"),
                "output_tokens": tokens.get("output_tokens"),
                "cache_read_input_tokens": tokens.get("cache_read_tokens"),
                "reasoning_output_tokens": tokens.get("reasoning_tokens"),
            },
            "_synthesized_by": "brainmark.agents.codex_adapter",
            "_usd_is_estimated": True,
            "_usd_price_status": priced.get("price_status"),
            "_usage_parser": parser_meta,
            "_returncode": returncode,
        }
        usage = {
            "backend": self.name,
            "model": model,
            "tokens": tokens,
            "usd": priced.get("usd"),
            "estimated": True,
            "usd_source": "computed_tokens_times_backends_json_prices",
            "price_status": priced.get("price_status"),
            "price_basis": priced.get("accounting_basis"),
            "prices_per_million": priced.get("prices_per_million"),
            "num_turns": tokens.get("turns"),
            "duration_ms": int(duration_s * 1000),
            "subtype": subtype,
            "is_error": subtype != "success",
            "complete": bool(parser_meta.get("complete")),
        }
        return cc_out, usage, session_id

    # -- tokens --------------------------------------------------------------

    def _tokens(self, stream_path: pathlib.Path, events: list[dict]) -> tuple[dict, dict]:
        """Prefer run.py's pinned parser (run.py:7379 _usage_from_codex).

        It enforces the invariants that matter -- cumulative snapshots must be
        monotone, optional counters must be consistently present, ambiguous
        aliases are refused rather than guessed -- and reusing it means a fix
        there lands here. The local fallback exists only so the adapter is
        testable without run.py on disk, and says which path produced a number.
        """
        try:
            stdout = stream_path.read_text(encoding="utf-8", errors="replace")
            raw = _harness.harness().extract_usage("codex", stdout, "")
        except Exception as exc:  # noqa: BLE001 - fall back, but never silently
            return _fallback_tokens(events, f"{type(exc).__name__}: {exc}")

        report = raw.get("usage_report") or {}
        tokens = {
            "input_tokens": raw.get("input_tokens"),
            "output_tokens": raw.get("output_tokens"),
            "cache_read_tokens": raw.get("cache_read_tokens"),
            # Codex exposes no cache-WRITE counter. None means "not reported",
            # not "zero" -- run.py:7420 freezes that distinction.
            "cache_creation_tokens": raw.get("cache_creation_tokens"),
            "reasoning_tokens": raw.get("reasoning_tokens"),
            "total_tokens": raw.get("total_tokens"),
            "turns": raw.get("turns"),
            "source": "codex_turn_completed_v1",
        }
        return tokens, {
            "parser": report.get("parser") or "codex_turn_completed_v1",
            "via": "run.py:extract_usage",
            "complete": bool(report.get("complete")),
            "source_events": report.get("source_events"),
            "error": report.get("error"),
        }


def _fallback_tokens(events: list[dict], why: str) -> tuple[dict, dict]:
    """Last cumulative turn.completed snapshot. Same rule as run.py, less strict."""
    snapshots = [e.get("usage") for e in events
                 if e.get("type") == "turn.completed" and isinstance(e.get("usage"), dict)]
    if not snapshots:
        return (
            {"input_tokens": None, "output_tokens": None, "cache_read_tokens": None,
             "cache_creation_tokens": None, "reasoning_tokens": None,
             "total_tokens": None, "turns": 0, "source": "codex_turn_completed_fallback"},
            {"parser": "codex_turn_completed_fallback", "via": "local", "complete": False,
             "source_events": 0, "error": f"no turn.completed usage ({why})"},
        )
    final = snapshots[-1]

    def get(*names: str) -> int | None:
        for name in names:
            value = final.get(name)
            if isinstance(value, (int, float)) and not isinstance(value, bool):
                return int(value)
        return None

    inp, out = get("input_tokens", "inputTokens"), get("output_tokens", "outputTokens")
    tokens = {
        "input_tokens": inp,
        "output_tokens": out,
        "cache_read_tokens": get("cached_input_tokens", "cache_read_input_tokens",
                                 "cache_read_tokens"),
        "cache_creation_tokens": None,
        "reasoning_tokens": get("reasoning_output_tokens", "reasoning_tokens"),
        "total_tokens": (inp + out) if (inp is not None and out is not None) else None,
        "turns": len(snapshots),
        "source": "codex_turn_completed_fallback",
    }
    return tokens, {"parser": "codex_turn_completed_fallback", "via": "local",
                    "complete": inp is not None and out is not None,
                    "source_events": len(snapshots), "error": why}


def _session_id(events: list[dict]) -> str | None:
    for event in events:
        if event.get("type") == "thread.started":
            value = event.get("thread_id") or event.get("threadId")
            if isinstance(value, str) and value:
                return value
        msg = event.get("msg")
        if isinstance(msg, dict) and msg.get("type") == "session_configured":
            value = msg.get("session_id")
            if isinstance(value, str) and value:
                return value
    return None
