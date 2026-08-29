"""Mechanism metrics over an agent stream. BACKEND-NEUTRAL (claude + codex).

PRIMARY PRE-REGISTERED METRIC: `locate_calls_pre_edit`.

    The number of LOCATE calls the agent makes STRICTLY BEFORE its first edit.

Why this and not tokens: memory (devenv) records that a score can move while the
mechanism does not, which is baseline drift rather than an effect. The claim
BrainMark tests is "a second session reuses what the first learned", and the
observable form of that claim is that the agent spends fewer calls REDISCOVERING
where the code lives. Pre-registering the mechanism as PRIMARY is what makes a
positive result interpretable and a negative result honest.

================================ THE TAXONOMY ================================

The metric is ONE definition expressed over TWO backend vocabularies. The
semantic classes (LOCATE / EDIT / OTHER) are frozen; what differs per backend is
only which stream events denote a tool call. Both mappings are stated exactly,
because a per-backend mapping that is merely "reasonable" is how a benchmark
ends up measuring its parser.

--- claude (stream-json) -----------------------------------------------------

  A tool call = a `tool_use` content block inside an `assistant` event.
  `user` `tool_result` echoes are NOT counted (they would double-count).

  LOCATE = tool_use named Read | Grep | Glob
         | tool_use named Bash whose command matches
           ^(rg|grep|egrep|git grep|find|ls|cat|head|tail|ag)\\b
  EDIT   = tool_use named Edit | Write | MultiEdit | NotebookEdit
  OTHER  = everything else (TodoWrite, Task, WebFetch, other Bash, ...)

--- codex (`codex exec --json`) ----------------------------------------------

  A tool call = one ITEM. Codex emits `item.started` and later `item.completed`
  for the SAME item id; they are ONE call. The completed payload replaces the
  started payload IN PLACE, so the call keeps its original position in the
  sequence -- the ordering is what the pre-edit cutoff is computed from, so a
  dedup that appended instead would silently move calls past the cutoff.
  (Same rule as run.py:7887 structured_tool_events.)

  ITEM KIND KEY: codex has emitted the kind under two different keys. Up to
  0.147 it was `item.item_type`; 0.148.0 renamed it to `item.type` (the OUTER
  event's "type" stays "item.completed"). Both are read. Reading only the old
  key made every call classify as "not a call", which is silent -- the metric
  reads 0 rather than raising -- so it is pinned by a 0.148-shaped fixture.

  item.item_type / item.type            -> class   synthesized name
    command_execution, local_shell_call -> LOCATE if the effective shell command
                                           matches the SAME frozen allowlist
                                           above, else OTHER;  name "Bash"
    file_change, patch_apply,
    apply_patch                         -> EDIT ;   name "FileChange"
    mcp_tool_call                       -> OTHER;   name "mcp__<server>__<tool>"
    web_search                          -> OTHER;   name "WebSearch"
    reasoning, agent_message, todo_list,
    error, <anything else>              -> NOT A TOOL CALL, skipped entirely

  EFFECTIVE SHELL COMMAND: codex reports `command` as either a string or an
  argv list, and normally wraps it: ["bash","-lc","rg foo"] on the older schema,
  the STRING "/usr/bin/bash -lc 'rg foo'" on 0.148.0. BOTH forms are unwrapped
  with the same <shell> -c/-lc rule, so `bash -lc 'rg foo'` classifies as LOCATE
  exactly as claude's Bash(rg foo) does. Without stripping, EVERY codex shell
  call would classify as OTHER and the codex primary metric would be
  identically zero -- which is why the string form is pinned by a fixture.

  Legacy codex schema (`{"id":..,"msg":{"type":"exec_command_begin",...}}`) is
  mapped the same way: exec_command_* -> shell, patch_apply_* -> EDIT.

--- what is deliberately NOT counted, on BOTH backends ------------------------

  * An edit performed THROUGH the shell (`sed -i`, a python heredoc) is not an
    EDIT event on either backend. Both are blind to it symmetrically.
  * `sed -n`, `awk`, `nl`, `less` are file READS that codex uses more than
    claude does, and they are NOT in the frozen allowlist. Adding them would
    change the pre-registered primary metric, which requires a dated prereg
    amendment -- so instead they are reported as the SECONDARY, EXPLORATORY
    metric `locate_calls_pre_edit_extended` (see EXTENDED_LOCATE_BASH). The
    primary stays frozen; the secondary shows what a wider allowlist would say.
  * Cross-backend ABSOLUTE levels are not comparable and are never pooled: the
    two vocabularies differ, and the plan reports per-backend cells anyway. What
    is compared is the WITHIN-PAIR, WITHIN-BACKEND arm delta.

  NO-EDIT SESSIONS: a session that never emits an EDIT event has no cutoff, so
  ALL of its locate calls count and `no_edit` is set True. THIS IS NOT A RARE
  CASE AND IT IS NOT DROPPED. An edit made through the shell -- `sed -i`, a
  heredoc, `cat >` -- is not an EDIT event on either backend (see the bullet
  above) and still produces a NON-EMPTY PATCH, so such a session passes the
  pre-registered gate (usd>0 + non-empty patch) and enters the primary metric
  with a WIDER horizon than a session that used the edit tool. Two streams with
  identical exploration measure 4 and 3 locate calls purely because of the edit
  modality. The inflation lands on whichever arm shell-edits more, so it is a
  real confound, not a rounding detail. report.py counts these cells per arm,
  warns, and publishes `headline_excluding_no_edit` beside the headline; the
  gate itself is pre-registered and is NOT changed here.

  RATIOS are formed on (count + 1) so that a 0-locate session is finite and a
  pair with a 0 denominator cannot explode the geometric mean.

Secondary metrics: total_cost_usd, tokens (modelUsage/usage on claude,
turn.completed on codex), duration_ms, num_turns, tool-call histogram.
"""

from __future__ import annotations

import json
import pathlib
import re
import shlex
from typing import Any, Iterable

LOCATE_TOOLS = frozenset({"Read", "Grep", "Glob"})
EDIT_TOOLS = frozenset({"Edit", "Write", "MultiEdit", "NotebookEdit"})

# Anchored at the start of the command. `git grep` is listed before bare `git`
# would ever match (there is no bare `git` alternative -- `git log` is NOT a
# locate call). Leading whitespace is stripped before matching.
LOCATE_BASH = re.compile(r"^(rg|grep|egrep|git grep|find|ls|cat|head|tail|ag)\b")

# SECONDARY / EXPLORATORY ONLY -- never the primary. Shell readers that codex
# reaches for and claude rarely does. `sed -i` is an in-place EDIT and is
# excluded below, not matched here.
EXTENDED_LOCATE_BASH = re.compile(
    r"^(rg|grep|egrep|git grep|git ls-files|git show|find|fd|ls|tree|cat|bat|head|tail"
    r"|ag|sed|awk|nl|wc|less|more|pcregrep)\b"
)
_SED_IN_PLACE = re.compile(r"^sed\b[^|;]*\s-\S*i")

RATIO_OFFSET = 1

# --- codex vocabulary --------------------------------------------------------
CODEX_SHELL_ITEMS = frozenset({"command_execution", "local_shell_call", "shell_call"})
CODEX_EDIT_ITEMS = frozenset({"file_change", "patch_apply", "apply_patch"})
CODEX_MCP_ITEMS = frozenset({"mcp_tool_call"})
CODEX_WEB_ITEMS = frozenset({"web_search"})
CODEX_ITEM_EVENTS = frozenset({"item.started", "item.completed", "item.updated"})
#: legacy `msg.type` prefixes -> class
CODEX_LEGACY_SHELL = ("exec_command", "shell_command")
CODEX_LEGACY_EDIT = ("patch_apply", "apply_patch")


def _iter_stream(path_or_lines: pathlib.Path | Iterable[str]) -> Iterable[dict]:
    if isinstance(path_or_lines, (str, pathlib.Path)):
        lines: Iterable[str] = pathlib.Path(path_or_lines).read_text(
            encoding="utf-8", errors="replace"
        ).splitlines()
    else:
        lines = path_or_lines
    for line in lines:
        # Already-parsed events pass straight through, so a caller that has
        # decoded the stream once (session_metrics) does not decode it twice.
        if isinstance(line, dict):
            yield line
            continue
        line = line.strip()
        if not line:
            continue
        try:
            payload = json.loads(line)
        except json.JSONDecodeError:
            # A truncated final line is normal when a session is killed on
            # timeout. Skip it; do not fail the whole session's metrics.
            continue
        if isinstance(payload, dict):
            yield payload


def _bash_command(inp: Any) -> str | None:
    if isinstance(inp, dict):
        for key in ("command", "cmd", "script"):
            value = inp.get(key)
            if isinstance(value, str):
                return value
    return None


def effective_shell_command(command: Any) -> str | None:
    """Unwrap a shell invocation to the command the agent actually ran.

    codex reports `["bash","-lc","rg foo"]`; the classifier must see `rg foo`.
    A string is returned as-is. An argv list that is NOT a `<shell> -c/-lc`
    wrapper is joined back into a command line.
    """
    if isinstance(command, str):
        # codex 0.148.0 reports the wrapper as a STRING --
        # "/usr/bin/bash -lc 'grep -n x a.txt'" -- not as the argv list the
        # older schema used. Returning it unchanged made LOCATE_BASH (anchored
        # at ^) miss every locate call, so the codex primary metric read 0.
        # Same <shell> -c/-lc rule as the argv-list branch below.
        try:
            parts = shlex.split(command)
        except ValueError:
            return command
        if len(parts) >= 3:
            head = pathlib.PurePath(parts[0]).name
            if head in {"bash", "sh", "zsh", "dash"} and parts[1].startswith("-") and "c" in parts[1]:
                return parts[2]
        return command
    if isinstance(command, list) and command:
        parts = [str(p) for p in command]
        if len(parts) >= 3:
            head = pathlib.PurePath(parts[0]).name
            if head in {"bash", "sh", "zsh", "dash"} and parts[1].startswith("-") and "c" in parts[1]:
                return parts[2]
        try:
            return shlex.join(parts)
        except (AttributeError, TypeError):  # pragma: no cover - py<3.8 shim
            return " ".join(parts)
    return None


def classify_command(command: str | None, extended: bool = False) -> str:
    """'locate' | 'other' for a shell command line. Frozen unless extended."""
    if not command:
        return "other"
    text = command.lstrip()
    if extended:
        if _SED_IN_PLACE.match(text):
            return "other"  # `sed -i` writes; it is not a read
        return "locate" if EXTENDED_LOCATE_BASH.match(text) else "other"
    return "locate" if LOCATE_BASH.match(text) else "other"


def classify_tool(name: str, tool_input: Any, extended: bool = False) -> str:
    """-> 'locate' | 'edit' | 'other' (claude vocabulary)."""
    if name in EDIT_TOOLS:
        return "edit"
    if name in LOCATE_TOOLS:
        return "locate"
    if name == "Bash":
        return classify_command(_bash_command(tool_input), extended=extended)
    return "other"


# --------------------------------------------------------------------------
# backend detection
# --------------------------------------------------------------------------


def detect_backend(events: list[dict]) -> str:
    """'codex' | 'claude' | 'unknown', from the stream's own event vocabulary.

    Detection is by event TYPE, never by a harness-written marker: the stream is
    the only artifact that cannot have been relabelled after the fact.
    """
    for event in events:
        etype = event.get("type")
        if etype in CODEX_ITEM_EVENTS or etype in {
            "thread.started", "turn.started", "turn.completed", "turn.failed"
        }:
            return "codex"
        if isinstance(event.get("msg"), dict) and isinstance(event.get("id"), (str, int)):
            return "codex"
        if etype in {"assistant", "user"} and isinstance(event.get("message"), dict):
            return "claude"
        if etype == "system" and event.get("subtype") == "init":
            return "claude"
    return "unknown"


# --------------------------------------------------------------------------
# per-backend tool-call extraction
# --------------------------------------------------------------------------


def _claude_tool_calls(events: list[dict]) -> tuple[list[dict], dict]:
    calls: list[dict] = []
    result: dict = {}
    for event in events:
        etype = event.get("type")
        if etype == "result":
            result = event
            continue
        if etype != "assistant":
            continue
        message = event.get("message")
        if not isinstance(message, dict):
            continue
        content = message.get("content")
        if not isinstance(content, list):
            continue
        for block in content:
            if not isinstance(block, dict) or block.get("type") != "tool_use":
                continue
            name = block.get("name")
            if not isinstance(name, str):
                continue
            tool_input = block.get("input")
            command = _bash_command(tool_input)
            calls.append({
                "name": name,
                "kind": classify_tool(name, tool_input),
                "kind_extended": classify_tool(name, tool_input, extended=True),
                "command": command,
                "id": block.get("id"),
            })
    return calls, result


def _codex_item(event: dict) -> dict | None:
    """The item payload of an item.* event, in either codex schema."""
    if event.get("type") in CODEX_ITEM_EVENTS:
        item = event.get("item")
        return item if isinstance(item, dict) else None
    msg = event.get("msg")
    if isinstance(msg, dict) and isinstance(msg.get("type"), str):
        # The OUTER id is the correlation key in the legacy schema, so it is
        # applied last and wins over any `id` inside the msg payload.
        return {**msg, "_legacy": True, "_legacy_type": msg["type"], "id": event.get("id")}
    return None


def _codex_classify_item(item: dict) -> tuple[str, str, str | None] | None:
    """(name, kind, command) for one codex item, or None if it is not a call."""
    item_type = item.get("item_type") or item.get("itemType")
    # codex 0.148.0 emits {"type":"item.completed","item":{"id":..,
    # "type":"command_execution",..}} -- the item kind moved from `item_type`
    # to `type`. Reading only `item_type` classified EVERY tool call as "not a
    # call": 0 locate calls, 0 edits, no_edit=True for every session in every
    # arm, i.e. the primary metric was identically 0 and the benchmark measured
    # nothing. Guarded on `_legacy` because in the legacy `msg` schema the
    # payload's own "type" IS the legacy type and must take the branch below.
    if not item_type and not item.get("_legacy"):
        candidate = item.get("type")
        if isinstance(candidate, str):
            item_type = candidate
    legacy_type = item.get("_legacy_type")

    if isinstance(legacy_type, str) and not item_type:
        if legacy_type.startswith(CODEX_LEGACY_EDIT):
            return "FileChange", "edit", None
        if legacy_type.startswith(CODEX_LEGACY_SHELL):
            command = effective_shell_command(item.get("command"))
            return "Bash", classify_command(command), command
        return None

    if item_type in CODEX_SHELL_ITEMS:
        command = effective_shell_command(
            item.get("command") if item.get("command") is not None else item.get("action")
        )
        return "Bash", classify_command(command), command
    if item_type in CODEX_EDIT_ITEMS:
        return "FileChange", "edit", None
    if item_type in CODEX_MCP_ITEMS:
        server = item.get("server") or item.get("server_name") or item.get("serverName")
        tool = item.get("tool") or item.get("tool_name") or item.get("toolName") or "unknown"
        name = f"mcp__{server}__{tool}" if server else str(tool)
        return name, "other", None
    if item_type in CODEX_WEB_ITEMS:
        return "WebSearch", "other", None
    return None


def _codex_tool_calls(events: list[dict]) -> tuple[list[dict], dict]:
    """Ordered codex tool calls, item.started/item.completed collapsed in place."""
    calls: list[dict] = []
    index_by_key: dict[tuple[str, str], int] = {}
    result: dict = {}
    for event in events:
        if event.get("type") == "turn.completed":
            result = event
        item = _codex_item(event)
        if item is None:
            continue
        classified = _codex_classify_item(item)
        if classified is None:
            continue
        name, kind, command = classified
        record = {
            "name": name,
            "kind": kind,
            "kind_extended": (
                classify_command(command, extended=True) if name == "Bash" else kind
            ),
            "command": command,
            "id": item.get("id"),
        }
        key = (str(item.get("id")), name) if item.get("id") is not None else None
        if key is not None and key in index_by_key:
            # SAME item, later event: replace IN PLACE so ordering is preserved.
            # A terminal event may carry less than its opener did (legacy
            # `exec_command_end` has no `command`), so the classification is
            # carried forward rather than downgraded to OTHER -- otherwise every
            # completed shell call would lose its LOCATE label.
            previous = calls[index_by_key[key]]
            if record["command"] is None and previous["command"] is not None:
                record["command"] = previous["command"]
                record["kind"] = previous["kind"]
                record["kind_extended"] = previous["kind_extended"]
            calls[index_by_key[key]] = record
            continue
        if key is not None:
            index_by_key[key] = len(calls)
        calls.append(record)
    return calls, result


def extract_tool_calls(stream: pathlib.Path | Iterable[str],
                       backend: str | None = None) -> tuple[list[dict], dict]:
    """Ordered tool calls plus the terminal result-ish event, for either backend.

    Returns (calls, result). `result` is claude's `result` event or codex's last
    `turn.completed`; `session_metrics` reads secondary fields off whichever it
    got, and adapters are what turn those into a normalized cc_out.json.
    """
    events = list(_iter_stream(stream))
    resolved = backend or detect_backend(events)
    if resolved == "codex":
        return _codex_tool_calls(events)
    return _claude_tool_calls(events)


def _usage(result: dict, backend: str) -> dict:
    """Token totals. Claude: modelUsage (run.py:7505 makes the same choice).
    Codex: the LAST cumulative `turn.completed.usage` snapshot -- codex snapshots
    are cumulative for the invocation and are never summed (run.py:7429)."""
    totals = {"input_tokens": 0, "output_tokens": 0,
              "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0}
    source = None

    if backend == "codex":
        usage = result.get("usage")
        if isinstance(usage, dict):
            source = "codex_turn_completed"
            aliases = {
                "input_tokens": ("input_tokens", "inputTokens"),
                "output_tokens": ("output_tokens", "outputTokens"),
                "cache_read_input_tokens": ("cached_input_tokens",
                                            "cache_read_input_tokens", "cache_read_tokens"),
            }
            for canonical, keys in aliases.items():
                for key in keys:
                    value = usage.get(key)
                    if isinstance(value, (int, float)) and not isinstance(value, bool):
                        totals[canonical] = int(value)
                        break
        # Codex `input_tokens` INCLUDES the cached part, so the total is
        # input+output; adding cache_read would double-count it.
        totals["total_tokens"] = totals["input_tokens"] + totals["output_tokens"]
        totals["source"] = source
        totals["cache_read_is_subset_of_input"] = True
        return totals

    model_usage = result.get("modelUsage")
    if isinstance(model_usage, dict) and model_usage:
        source = "modelUsage"
        for per_model in model_usage.values():
            if not isinstance(per_model, dict):
                continue
            for key in ("input_tokens", "output_tokens",
                        "cache_read_input_tokens", "cache_creation_input_tokens"):
                value = per_model.get(key)
                if isinstance(value, (int, float)):
                    totals[key] += int(value)
    elif isinstance(result.get("usage"), dict):
        source = "usage"
        usage = result["usage"]
        for key in ("input_tokens", "output_tokens",
                    "cache_read_input_tokens", "cache_creation_input_tokens"):
            value = usage.get(key)
            if isinstance(value, (int, float)):
                totals[key] += int(value)
    totals["total_tokens"] = (
        totals["input_tokens"] + totals["output_tokens"]
        + totals["cache_read_input_tokens"] + totals["cache_creation_input_tokens"]
    )
    totals["source"] = source
    totals["cache_read_is_subset_of_input"] = False
    return totals


def session_metrics(stream: pathlib.Path | Iterable[str],
                    backend: str | None = None) -> dict:
    """All mechanism + secondary metrics for one session, either backend."""
    events = list(_iter_stream(stream))
    resolved = backend or detect_backend(events)
    calls, result = extract_tool_calls(events, backend=resolved)

    def first_edit(key: str) -> int | None:
        for index, call in enumerate(calls):
            if call[key] == "edit":
                return index
        return None

    first_edit_index = first_edit("kind")
    no_edit = first_edit_index is None
    horizon = len(calls) if no_edit else first_edit_index
    pre_edit = calls[:horizon]
    locate_calls_pre_edit = sum(1 for c in pre_edit if c["kind"] == "locate")

    ext_first = first_edit("kind_extended")
    ext_horizon = len(calls) if ext_first is None else ext_first
    locate_pre_edit_extended = sum(
        1 for c in calls[:ext_horizon] if c["kind_extended"] == "locate"
    )

    histogram: dict[str, int] = {}
    for call in calls:
        histogram[call["name"]] = histogram.get(call["name"], 0) + 1

    if resolved == "codex":
        num_turns = None  # codex has no turn counter on a single item event
        duration_ms = result.get("duration_ms")
        total_cost_usd = None  # codex reports no cost; the adapter computes one
        subtype = "success" if result else None
        is_error = False
    else:
        num_turns = result.get("num_turns")
        duration_ms = result.get("duration_ms")
        total_cost_usd = result.get("total_cost_usd")
        subtype = result.get("subtype")
        is_error = bool(result.get("is_error"))

    return {
        "backend": resolved,
        "locate_calls_pre_edit": locate_calls_pre_edit,
        "locate_ratio_basis": locate_calls_pre_edit + RATIO_OFFSET,
        # SECONDARY / EXPLORATORY: wider shell-read allowlist. NOT the
        # pre-registered primary; see this module's docstring.
        "locate_calls_pre_edit_extended": locate_pre_edit_extended,
        "no_edit": no_edit,
        "first_edit_call_index": first_edit_index,
        "tool_calls_total": len(calls),
        "tool_calls_pre_edit": len(pre_edit),
        "locate_calls_total": sum(1 for c in calls if c["kind"] == "locate"),
        "edit_calls_total": sum(1 for c in calls if c["kind"] == "edit"),
        "tool_histogram": dict(sorted(histogram.items())),
        "num_turns": num_turns,
        "duration_ms": duration_ms,
        "total_cost_usd": total_cost_usd,
        "subtype": subtype,
        "is_error": is_error,
        "tokens": _usage(result, resolved),
        "has_result_event": bool(result),
    }


def pair_ratio(treatment: int, control: int) -> float:
    """(treatment+1)/(control+1) -- the frozen ratio form."""
    return (treatment + RATIO_OFFSET) / (control + RATIO_OFFSET)


def main(argv: list[str] | None = None) -> int:
    import argparse

    parser = argparse.ArgumentParser(description="Mechanism metrics for one stream.jsonl")
    parser.add_argument("stream")
    parser.add_argument("--backend", default=None, choices=["claude", "codex"],
                        help="override auto-detection")
    args = parser.parse_args(argv)
    print(json.dumps(session_metrics(pathlib.Path(args.stream), backend=args.backend),
                     indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
