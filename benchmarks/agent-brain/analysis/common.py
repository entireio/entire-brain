"""Shared record semantics used by every analysis entry point."""

from __future__ import annotations

from typing import Any


EXECUTED_RUN_PREDICATE_VERSION = 1


def is_executed_run(record: dict[str, Any]) -> bool:
    """Return whether an agent actually executed and produced an outcome.

    New records explicitly set ``agent_ran``.  The conservative legacy fallback
    accepts records with measured agent output so retained pre-schema suites can
    still be migrated.  Infrastructure-only synthetic records are never
    executed.  Validation failures, non-zero return codes, errors after agent
    start, and protocol/adherence failures remain executed outcomes.
    """
    if record.get("analysis_excluded"):
        return False
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
