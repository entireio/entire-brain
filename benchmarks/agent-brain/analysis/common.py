"""Shared record semantics used by every analysis entry point."""

from __future__ import annotations

from typing import Any


EXECUTED_RUN_PREDICATE_VERSION = 2


def is_executed_run(record: dict[str, Any]) -> bool:
    """Return whether the causal treatment interval actually started.

    V2 records explicitly set ``treatment_started`` at the harness-owned
    retrieval/no-op boundary.  A retrieval or delivery failure after that
    boundary is therefore an executed product outcome even if no model request
    was sent.  ``agent_ran`` remains the conservative legacy fallback so
    retained pre-v2 suites can still be migrated.  Validation failures,
    non-zero return codes, and integrity failures after treatment start remain
    executed outcomes; pre-treatment setup failures do not.
    """
    if record.get("analysis_excluded"):
        return False
    if isinstance(record.get("treatment_started"), bool):
        return bool(record["treatment_started"])
    if isinstance(record.get("agent_ran"), bool):
        return bool(record["agent_ran"])
    agent_info = record.get("agent_info")
    if not isinstance(agent_info, dict) or not agent_info:
        return False
    usage = agent_info.get("usage")
    return (
        "returncode" in agent_info
        or isinstance(agent_info.get("seconds"), (int, float))
        or (isinstance(usage, dict) and any(isinstance(usage.get(k), (int, float)) for k in ("total_tokens", "input_tokens", "output_tokens")))
    )


def load_records(path: "Any") -> list[dict[str, Any]]:
    import json
    import pathlib

    source = pathlib.Path(path)
    if source.is_dir():
        source = source / "records.ndjson"
    records: list[dict[str, Any]] = []
    for number, line in enumerate(source.read_text().splitlines(), 1):
        if not line.strip():
            continue
        value = json.loads(line)
        if not isinstance(value, dict):
            raise ValueError(f"{source}:{number}: record must be an object")
        records.append(value)
    return records
