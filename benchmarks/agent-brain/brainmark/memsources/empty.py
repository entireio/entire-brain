"""`no_brain` -- the baseline arm.

It is NOT "no packet". It is a well-formed packet carrying a sentinel and zero
results, pushed through the exact same bounder, delimiter guard, envelope, and
prompt scaffold as every other arm.

That distinction is the whole design:
  * the prompt-symmetry sha is identical to the other arms by construction;
  * the null test (plan step 2) can run full_brain's machinery with an empty
    packet and compare it to this arm, and any difference it finds is machinery,
    not memory -- which is exactly what that test is for.
"""

from __future__ import annotations

from .base import MemoryPacket, Stopwatch, build_packet

ARM = "no_brain"


def build(query: str, max_bytes: int, sentinel: str, **_ignored) -> MemoryPacket:
    with Stopwatch() as watch:
        results: list[dict] = []
    return build_packet(
        arm=ARM,
        query=query,
        results=results,
        max_bytes=max_bytes,
        # META ONLY. Delivered as a `note` field it was a key no other arm
        # carried, whose value told the baseline in prose that it had no
        # memory. The empty `results` array is the honest tool output.
        extra={"empty_sentinel": sentinel},
        prep={"seconds": watch.elapsed_s, "llm_calls": 0, "usd": 0.0},
    )
