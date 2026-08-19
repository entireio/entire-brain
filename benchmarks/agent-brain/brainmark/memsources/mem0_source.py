"""`mem0` arm -- the named competitor, over the same session-A bytes.

Written against the mem0 OSS pip API (`from mem0 import Memory`), NOT the
managed platform: the managed service is a moving target that cannot be pinned,
and a benchmark that cannot be re-run is not evidence.

IMPORT-GUARDED. `mem0ai` is not a dependency of this repo and the offline test
suite must pass without it. The import happens inside `_make_client()`, so this
module always imports; only actually RUNNING the arm requires the package. A
missing package raises MemorySourceError at prep -- it never degrades to an
empty packet (that would score an infrastructure failure as "memory did not
help", which is the easiest way to fake this benchmark's headline).

mem0 is synchronous, so it is wrapped to satisfy the async duck-typed surface in
_competitor.py. The wrapper is the ONLY mem0-specific code here; retrieval,
budget, envelope, and pinning are the shared path every arm uses.
"""

from __future__ import annotations

import asyncio
from typing import Any

from ._competitor import run_competitor, transcript_to_messages
from .base import MemoryPacket, MemorySourceError

ARM = "mem0"


class Mem0Client:
    """Async adapter over mem0 OSS `Memory`, matching the harness client surface."""

    def __init__(self, pins: dict[str, Any] | None = None) -> None:
        self._pins = pins or {}
        self._memory: Any = None

    # -- context manager -------------------------------------------------
    async def __aenter__(self) -> "Mem0Client":
        self._memory = await asyncio.to_thread(self._make_client)
        return self

    async def __aexit__(self, *exc: Any) -> None:
        return None

    async def close(self) -> None:
        return None

    def _make_client(self) -> Any:
        try:
            from mem0 import Memory  # type: ignore[import-not-found]
        except ImportError as exc:
            raise MemorySourceError(
                "mem0 arm requires the `mem0ai` package "
                f"(pin {self._pins.get('version_pin', 'unpinned')}): pip install "
                "mem0ai==<pin>. Refusing to run the arm degraded."
            ) from exc

        llm = self._pins.get("llm") or {}
        embedder = self._pins.get("embedder") or {}
        config: dict[str, Any] = {}
        if llm:
            config["llm"] = {"provider": llm.get("provider", "openai"),
                             "config": {"model": llm.get("model")}}
        if embedder:
            config["embedder"] = {"provider": embedder.get("provider", "openai"),
                                  "config": {"model": embedder.get("model")}}
        return Memory.from_config(config) if config else Memory()

    # -- duck-typed surface ----------------------------------------------
    async def add(
        self,
        messages: list[dict[str, str]],
        user_id: str,
        observation_date: str | None = None,
        timestamp: int | None = None,
        custom_instructions: str | None = None,
        metadata: dict | None = None,
    ) -> dict | None:
        return await asyncio.to_thread(
            self._memory.add, messages, user_id=user_id, metadata=metadata or {}
        )

    async def search(
        self,
        query: str,
        user_id: str,
        top_k: int = 200,
        rerank: bool = False,
        score_debug: bool = False,
    ) -> list[dict]:
        raw = await asyncio.to_thread(
            self._memory.search, query, user_id=user_id, limit=int(top_k)
        )
        # mem0 >=0.1.x returns {"results": [...]}; older builds returned a list.
        hits = raw.get("results", []) if isinstance(raw, dict) else (raw or [])
        out: list[dict] = []
        for hit in hits:
            if not isinstance(hit, dict):
                continue
            text = str(hit.get("memory") or hit.get("text") or "").strip()
            if not text:
                continue
            out.append({
                "memory": text,
                "score": float(hit.get("score") or 0.0),
                "id": str(hit.get("id") or ""),
                "created_at": hit.get("created_at"),
                "source": ARM,
            })
        return out

    async def delete_user(self, user_id: str) -> bool:
        await asyncio.to_thread(self._memory.delete_all, user_id=user_id)
        return True

    async def get_user_profile(self, user_id: str) -> dict | None:
        return None


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
    return run_competitor(
        arm=ARM,
        client_factory=lambda: Mem0Client(pins),
        query=query,
        messages=transcript_to_messages(transcript_bytes),
        user_id=user_id,
        max_bytes=max_bytes,
        top_k=top_k,
        observation_date=observation_date,
        pins=pins,
    )
