"""Memory sources: pinned session-A bytes in, one bounded memory packet out.

Five arms, one envelope (see base.py). `full_brain` is the only arm that touches
entire-brain; `no_brain` is the sentinel baseline; the rest are competitors over
byte-identical input.
"""

from __future__ import annotations

from typing import Any, Callable

from . import cmm_source, empty, full_brain, graphify_source, mem0_source
from .base import MemoryPacket, MemorySourceError, build_packet, normalize_results

ARMS: dict[str, Callable[..., MemoryPacket]] = {
    "no_brain": empty.build,
    "full_brain": full_brain.build,
    "mem0": mem0_source.build,
    "graphify": graphify_source.build,
    "cmm": cmm_source.build,
}

#: Arms permitted to touch Entire-family tooling. Everything else must fail the
#: leakage audit if an Entire binary appears in its session.
ENTIRE_TOOL_ARMS = frozenset({"full_brain"})

__all__ = [
    "ARMS",
    "ENTIRE_TOOL_ARMS",
    "MemoryPacket",
    "MemorySourceError",
    "build_packet",
    "normalize_results",
]


def build(arm: str, **kwargs: Any) -> MemoryPacket:
    if arm not in ARMS:
        raise MemorySourceError(f"unknown arm {arm!r}; known: {sorted(ARMS)}")
    return ARMS[arm](**kwargs)
