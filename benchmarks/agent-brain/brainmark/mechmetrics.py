"""Mechanism metrics over a Claude Code stream-JSON transcript.

PRIMARY PRE-REGISTERED METRIC: `locate_calls_pre_edit`.

    The number of LOCATE calls the agent makes STRICTLY BEFORE its first edit.

Why this and not tokens: memory (devenv) records that a score can move while the
mechanism does not, which is baseline drift rather than an effect. The claim
BrainMark tests is "a second session reuses what the first learned", and the
observable form of that claim is that the agent spends fewer calls REDISCOVERING
where the code lives. Pre-registering the mechanism as PRIMARY is what makes a
positive result interpretable and a negative result honest.

Definitions (frozen -- changing any of these changes the metric):

  LOCATE = tool_use whose name is in {Read, Grep, Glob}
         | tool_use named Bash whose command matches
           ^(rg|grep|egrep|git grep|find|ls|cat|head|tail|ag)\\b
  EDIT   = tool_use whose name is in {Edit, Write, MultiEdit, NotebookEdit}

  locate_calls_pre_edit = |{LOCATE calls with index < index of first EDIT}|

  NO-EDIT SESSIONS: a session that never edits has no cutoff, so ALL of its
  locate calls count and `no_edit` is set True. Such sessions are reported
  separately and are dropped by the standard pair gate anyway (empty patch), but
  the metric is still defined so the drop is auditable rather than a crash.

  RATIOS are formed on (count + 1) so that a 0-locate session is finite and a
  pair with a 0 denominator cannot explode the geometric mean.

Secondary metrics: total_cost_usd, tokens (modelUsage/usage), duration_ms,
num_turns, tool-call histogram.
"""

from __future__ import annotations

import json
import pathlib
import re
from typing import Any, Iterable

LOCATE_TOOLS = frozenset({"Read", "Grep", "Glob"})
EDIT_TOOLS = frozenset({"Edit", "Write", "MultiEdit", "NotebookEdit"})

# Anchored at the start of the command. `git grep` is listed before bare `git`
# would ever match (there is no bare `git` alternative -- `git log` is NOT a
# locate call). Leading whitespace is stripped before matching.
LOCATE_BASH = re.compile(r"^(rg|grep|egrep|git grep|find|ls|cat|head|tail|ag)\b")

RATIO_OFFSET = 1


def _iter_stream(path_or_lines: pathlib.Path | Iterable[str]) -> Iterable[dict]:
    if isinstance(path_or_lines, (str, pathlib.Path)):
        lines: Iterable[str] = pathlib.Path(path_or_lines).read_text(
            encoding="utf-8", errors="replace"
        ).splitlines()
    else:
        lines = path_or_lines
    for line in lines:
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


def classify_tool(name: str, tool_input: Any) -> str:
    """-> 'locate' | 'edit' | 'other'."""
    if name in EDIT_TOOLS:
        return "edit"
    if name in LOCATE_TOOLS:
        return "locate"
    if name == "Bash":
        command = _bash_command(tool_input)
        if command and LOCATE_BASH.match(command.lstrip()):
            return "locate"
    return "other"


def extract_tool_calls(stream: pathlib.Path | Iterable[str]) -> tuple[list[dict], dict]:
    """Ordered tool_use calls plus the terminal `result` event.

    Only `assistant` message content blocks of type `tool_use` are counted: those
    are calls the agent ISSUED. `user` tool_result blocks are echoes of the same
    call and would double-count.
    """
    calls: list[dict] = []
    result: dict = {}
    for event in _iter_stream(stream):
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
            calls.append({
                "name": name,
                "kind": classify_tool(name, tool_input),
                "command": _bash_command(tool_input),
                "id": block.get("id"),
            })
    return calls, result


def _usage(result: dict) -> dict:
    """Token totals, preferring modelUsage (run.py:7505 makes the same choice)."""
    totals = {"input_tokens": 0, "output_tokens": 0,
              "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0}
    model_usage = result.get("modelUsage")
    source = None
    if isinstance(model_usage, dict) and model_usage:
        source = "modelUsage"
        for per_model in model_usage.values():
            if not isinstance(per_model, dict):
                continue
            for key in totals:
                value = per_model.get(key)
                if isinstance(value, (int, float)):
                    totals[key] += int(value)
    elif isinstance(result.get("usage"), dict):
        source = "usage"
        usage = result["usage"]
        for key in totals:
            value = usage.get(key)
            if isinstance(value, (int, float)):
                totals[key] += int(value)
    totals["total_tokens"] = (
        totals["input_tokens"] + totals["output_tokens"]
        + totals["cache_read_input_tokens"] + totals["cache_creation_input_tokens"]
    )
    totals["source"] = source
    return totals


def session_metrics(stream: pathlib.Path | Iterable[str]) -> dict:
    """All mechanism + secondary metrics for one session."""
    calls, result = extract_tool_calls(stream)

    first_edit_index: int | None = None
    for index, call in enumerate(calls):
        if call["kind"] == "edit":
            first_edit_index = index
            break

    no_edit = first_edit_index is None
    horizon = len(calls) if no_edit else first_edit_index
    pre_edit = calls[:horizon]
    locate_calls_pre_edit = sum(1 for c in pre_edit if c["kind"] == "locate")

    histogram: dict[str, int] = {}
    for call in calls:
        histogram[call["name"]] = histogram.get(call["name"], 0) + 1

    return {
        "locate_calls_pre_edit": locate_calls_pre_edit,
        "locate_ratio_basis": locate_calls_pre_edit + RATIO_OFFSET,
        "no_edit": no_edit,
        "first_edit_call_index": first_edit_index,
        "tool_calls_total": len(calls),
        "tool_calls_pre_edit": len(pre_edit),
        "locate_calls_total": sum(1 for c in calls if c["kind"] == "locate"),
        "edit_calls_total": sum(1 for c in calls if c["kind"] == "edit"),
        "tool_histogram": dict(sorted(histogram.items())),
        "num_turns": result.get("num_turns"),
        "duration_ms": result.get("duration_ms"),
        "total_cost_usd": result.get("total_cost_usd"),
        "subtype": result.get("subtype"),
        "is_error": bool(result.get("is_error")),
        "tokens": _usage(result),
        "has_result_event": bool(result),
    }


def pair_ratio(treatment: int, control: int) -> float:
    """(treatment+1)/(control+1) -- the frozen ratio form."""
    return (treatment + RATIO_OFFSET) / (control + RATIO_OFFSET)


def main(argv: list[str] | None = None) -> int:
    import argparse

    parser = argparse.ArgumentParser(description="Mechanism metrics for one stream.jsonl")
    parser.add_argument("stream")
    args = parser.parse_args(argv)
    print(json.dumps(session_metrics(pathlib.Path(args.stream)), indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
