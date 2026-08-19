"""Shared plumbing for the three competitor arms (mem0, graphify, cmm).

DUCK-TYPED CLIENT SURFACE -- the contract every competitor client must satisfy.
Taken from eg-memharness/bench/memory/benchmarks/common/entire_client.py (and
its graphify_client.py / cmm_client.py siblings), so a client written for that
harness drops in here unchanged:

    async add(messages: list[{"role","content"}], user_id: str,
              observation_date: str|None = None, timestamp: int|None = None,
              custom_instructions: str|None = None,
              metadata: dict|None = None) -> {"results": [...]}

    async search(query: str, user_id: str, top_k: int = 200,
                 rerank: bool = False,
                 score_debug: bool = False) -> list[{"memory","score","id",...}]

    async close() -> None            # plus __aenter__ / __aexit__

FAIRNESS: every competitor receives byte-identical session-A input, is queried
with the byte-identical query string, and is rendered into the byte-identical
envelope under the byte-identical byte budget. The only thing that varies is
what the competitor itself returns. Prep wall-clock is timed and reported.

NEVER degrade a failure into an empty packet -- see MemorySourceError in base.py.
"""

from __future__ import annotations

import asyncio
import json
import os
import pathlib
from typing import Any, Callable

from .base import MemoryPacket, MemorySourceError, Stopwatch, build_packet, normalize_results

# Where the eg-memharness checkout lives. Overridable per machine via
# EG_MEMHARNESS_ROOT; the default assumes the common devenv-worktree layout
# (eg-memharness cloned as a sibling under the user's own workspace root) but
# is never assumed to be correct -- load_memharness_client() below fails
# loudly at prep, never silently, if nothing is found there.
_DEFAULT_EG_MEMHARNESS_ROOT = pathlib.Path.home() / "devenv" / "eg-memharness"

# Transcript roles we surface. `system` is dropped: it is harness scaffolding,
# identical across arms, and feeding it in would just pad every store equally.
_ROLES = {"user", "assistant"}


def transcript_to_messages(transcript_bytes: bytes, max_chars: int = 12000) -> list[dict[str, str]]:
    """Claude Code stream-JSON (or plain JSONL) -> [{"role","content"}].

    Text blocks are joined; tool_use blocks are rendered as a compact one-line
    `[tool] Name {json args}` so a competitor that indexes prose still sees
    WHICH FILES the session touched -- that is the reusable signal in a coding
    session, and withholding it would handicap every competitor equally but
    pointlessly.
    """
    messages: list[dict[str, str]] = []
    for line in transcript_bytes.decode("utf-8", errors="replace").splitlines():
        line = line.strip()
        if not line:
            continue
        try:
            event = json.loads(line)
        except json.JSONDecodeError:
            continue
        if not isinstance(event, dict):
            continue
        role = event.get("type")
        message = event.get("message")
        if role not in _ROLES or not isinstance(message, dict):
            continue
        content = message.get("content")
        parts: list[str] = []
        if isinstance(content, str):
            parts.append(content)
        elif isinstance(content, list):
            for block in content:
                if not isinstance(block, dict):
                    continue
                btype = block.get("type")
                if btype == "text" and isinstance(block.get("text"), str):
                    parts.append(block["text"])
                elif btype == "tool_use":
                    args = json.dumps(block.get("input") or {}, ensure_ascii=False, sort_keys=True)
                    parts.append(f"[tool] {block.get('name')} {args[:600]}")
                elif btype == "tool_result":
                    inner = block.get("content")
                    if isinstance(inner, str):
                        parts.append(f"[result] {inner[:600]}")
                    elif isinstance(inner, list):
                        for sub in inner:
                            if isinstance(sub, dict) and isinstance(sub.get("text"), str):
                                parts.append(f"[result] {sub['text'][:600]}")
        text = "\n".join(p for p in parts if p).strip()
        if not text:
            continue
        messages.append({"role": role, "content": text[:max_chars]})
    return messages


def run_competitor(
    arm: str,
    client_factory: Callable[[], Any],
    query: str,
    messages: list[dict[str, str]],
    user_id: str,
    max_bytes: int,
    top_k: int,
    observation_date: str | None = None,
    pins: dict[str, Any] | None = None,
) -> MemoryPacket:
    """Ingest A, run ONE search, render the envelope. Synchronous wrapper."""
    if not messages:
        raise MemorySourceError(f"arm {arm}: session-A transcript produced no messages")

    async def _drive() -> tuple[list[dict], float, float]:
        client = client_factory()
        ingest = Stopwatch()
        retrieve = Stopwatch()
        try:
            async with client:
                with ingest:
                    for chunk in messages:
                        await client.add(
                            [chunk], user_id=user_id, observation_date=observation_date
                        )
                with retrieve:
                    hits = await client.search(query, user_id=user_id, top_k=top_k)
        finally:
            close = getattr(client, "close", None)
            if close is not None:
                try:
                    await close()
                except Exception:  # noqa: BLE001 - teardown must not mask the real error
                    pass
        return list(hits or []), ingest.elapsed_s, retrieve.elapsed_s

    try:
        hits, ingest_s, retrieve_s = asyncio.run(_drive())
    except MemorySourceError:
        raise
    except Exception as exc:  # noqa: BLE001
        raise MemorySourceError(f"arm {arm}: competitor prep failed: {type(exc).__name__}: {exc}") from exc

    for hit in hits:
        if isinstance(hit, dict):
            hit.setdefault("source", arm)

    return build_packet(
        arm=arm,
        query=query,
        results=normalize_results(hits, top_k),
        max_bytes=max_bytes,
        prep={
            "seconds": round(ingest_s + retrieve_s, 4),
            "ingest_seconds": ingest_s,
            "retrieve_seconds": retrieve_s,
            "messages_ingested": len(messages),
            "hits_returned": len(hits),
            "pins": pins or {},
        },
    )


def load_memharness_client(module_name: str, class_name: str) -> Any:
    """Import a client from the eg-memharness `common/` package by path.

    Those clients are the reference implementations for graphify and cmm; they
    are imported rather than copied so a fix there lands here. If the checkout
    is absent, the arm fails loudly at prep -- it never silently degrades.
    """
    import importlib.util

    memharness_root = pathlib.Path(
        os.environ.get("EG_MEMHARNESS_ROOT", str(_DEFAULT_EG_MEMHARNESS_ROOT))
    )
    root = memharness_root / "bench" / "memory" / "benchmarks" / "common"
    path = root / f"{module_name}.py"
    if not path.is_file():
        raise MemorySourceError(
            f"competitor client {module_name} not found at {path}; "
            "clone eg-memharness or set the arm aside -- do not run it degraded"
        )
    spec = importlib.util.spec_from_file_location(f"brainmark_{module_name}", path)
    if spec is None or spec.loader is None:
        raise MemorySourceError(f"cannot load {path}")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    client = getattr(module, class_name, None)
    if client is None:
        raise MemorySourceError(f"{path} has no {class_name}")
    return client
