"""`cmm` arm -- codebase-memory-mcp, over the same session-A bytes.

Client is the eg-memharness CmmClient (bench/memory/benchmarks/common/
cmm_client.py), imported by path rather than copied.

CMM_MEM_BUDGET_MB matters: cmm's RAM-first pipeline otherwise reserves half of
system RAM per process, which under concurrency=8 fails searches outright. That
is a resource setting, not a capability change -- the corpora here are small.
(memory: "load pressure is an arm confound".)
"""

from __future__ import annotations

import os
from typing import Any

from ._competitor import load_memharness_client, run_competitor, transcript_to_messages
from .base import MemoryPacket

ARM = "cmm"


def build(
    query: str,
    max_bytes: int,
    top_k: int,
    transcript_bytes: bytes,
    user_id: str,
    pins: dict[str, Any] | None = None,
    observation_date: str | None = None,
    **_ignored,
) -> MemoryPacket:
    pins = pins or {}
    if not _ignored.get("_isolated"):
        from . import _isolated
        mapping = {'binary': 'CMM_BIN', 'mem_budget_mb': 'CMM_MEM_BUDGET_MB', 'index_mode': 'CMM_INDEX_MODE'}
        environment = {env: str(pins[key]) for key, env in mapping.items() if pins.get(key) is not None}
        return _isolated.build(ARM, environment, {
            "query": query, "max_bytes": max_bytes, "top_k": top_k,
            "transcript_bytes": transcript_bytes, "user_id": user_id,
            "pins": pins, "observation_date": observation_date,
        })

    client_cls = load_memharness_client("cmm_client", "CmmClient")
    return run_competitor(
        arm=ARM,
        client_factory=lambda: client_cls(),
        query=query,
        messages=transcript_to_messages(transcript_bytes),
        user_id=user_id,
        max_bytes=max_bytes,
        top_k=top_k,
        observation_date=observation_date,
        pins=pins,
    )
