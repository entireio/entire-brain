"""The one memory-packet envelope every BrainMark arm is delivered through.

THE INVARIANT THIS FILE EXISTS TO ENFORCE:
every arm's memory reaches the agent as the SAME JSON shape, bounded by the SAME
bounder, screened by the SAME delimiter guard, wrapped in the SAME tags. Arms
differ in packet CONTENT only. Anything an arm wants to say that the envelope
cannot express is, by construction, an unfair advantage and is dropped.

    {"query": <B problem statement>, "results": [
        {"source": str, "id": str, "text": str, "score": float, ...}, ...]}

THE ENVELOPE IS ARM-BLIND. The arm name is NOT in the delivered bytes -- it
lives on MemoryPacket.arm and in `meta`, which is what the artifacts and the
report read. A packet whose first field said `"arm":"no_brain"` handed the
model its own experimental condition inside the memory block; that is
unblinding of the subject, not tool output, and it is the one thing an
envelope shared by six arms must never do. What legitimately differs between
arms is what the source RETURNED: how many results, what they say, and what a
backend calls its own `source`.

`results` is mandatory and is what run.py's bounder (run.py:4731) truncates
rank-first, so a packet that overflows loses its WORST results, never its best,
and never becomes malformed JSON.

THE PACKET IS THE PINNED ARTIFACT, NOT THE STORE. Every source returns a
MemoryPacket whose `sha256` is recorded at prep time and re-checked at launch and
at report time. Competitor ingest is nondeterministic (LLM extraction, ANN
indexes, wallclock ids); pinning the delivered bytes means that nondeterminism
can never silently become an arm asymmetry between what was measured and what was
reported.
"""

from __future__ import annotations

import dataclasses
import time
from typing import Any

from .. import _harness


@dataclasses.dataclass(frozen=True)
class MemoryPacket:
    arm: str
    text: str
    sha256: str
    meta: dict[str, Any]

    def to_provenance(self) -> dict[str, Any]:
        return {"arm": self.arm, "packet_sha256": self.sha256, **self.meta}


class MemorySourceError(RuntimeError):
    """Prep failed. NEVER degrade to an empty packet: a silent empty packet
    turns an infrastructure failure into a measured 'memory did not help',
    which is the single easiest way to fake this benchmark's headline.
    (Same invariant the eg-memharness clients enforce with BUFFER_MISSING.)"""


def normalize_results(raw: list[dict], top_k: int) -> list[dict]:
    """Coerce any client's hits into the envelope's result shape.

    Sort is (score desc, id asc) -- the id tiebreak makes a packet reproducible
    when a backend returns ties in arbitrary order.
    """
    out: list[dict] = []
    for item in raw:
        if not isinstance(item, dict):
            continue
        text = str(item.get("memory") or item.get("text") or "").strip()
        if not text:
            continue
        try:
            score = float(item.get("score") or 0.0)
        except (TypeError, ValueError):
            score = 0.0
        entry = {
            "source": str(item.get("source") or ""),
            "id": str(item.get("id") or ""),
            "text": text,
            "score": round(score, 6),
        }
        for optional in ("path", "line", "heading"):
            value = item.get(optional)
            if value not in (None, "", 0):
                entry[optional] = value
        out.append(entry)
    out.sort(key=lambda e: (-e["score"], e["id"]))
    return out[:top_k]


def build_packet(
    arm: str,
    query: str,
    results: list[dict],
    max_bytes: int,
    extra: dict[str, Any] | None = None,
    prep: dict[str, Any] | None = None,
) -> MemoryPacket:
    """Serialize -> bound (run.py:4731) -> delimiter-guard (run.py:4840) -> pin.

    `extra` goes to `meta`, NOT into the packet: it exists so a source can
    record something about its own prep, and anything it could add to the
    delivered JSON would be a per-arm shape difference.
    """
    # NO `arm` KEY. The condition name is recorded on the returned MemoryPacket
    # and in `meta`, never in the bytes the agent reads. `extra` is likewise
    # meta-only: a key that only one arm carries is a tell even when its value
    # is bland, so nothing here may vary in SHAPE between arms.
    payload: dict[str, Any] = {"query": query, "results": results}

    text, bound_meta = _harness.bound_memory_packet(
        _harness.canonical_json(payload), max_bytes
    )

    if _harness.packet_contains_reserved_delimiter(text):
        # Fail closed. A packet that can forge its own closing tag can inject
        # instructions into the prompt, and no arm may do that.
        raise MemorySourceError(
            f"arm {arm}: memory packet contains the reserved packet delimiter"
        )

    meta: dict[str, Any] = {
        "result_count_offered": len(results),
        "bounding": bound_meta,
        "max_bytes": max_bytes,
        "bytes": len(text.encode("utf-8")),
    }
    if extra:
        meta.update(extra)
    if prep:
        meta["prep"] = prep
    return MemoryPacket(arm=arm, text=text, sha256=_harness.sha256_text(text), meta=meta)


class Stopwatch:
    """Prep cost is recorded separately per arm and REPORTED, never hidden.

    An arm that needs 20 minutes and an LLM to ingest is not free; the headline
    table shows prep wall-clock beside the result so a reader can price it.
    """

    def __init__(self) -> None:
        self.elapsed_s = 0.0
        self._start = 0.0

    def __enter__(self) -> "Stopwatch":
        self._start = time.monotonic()
        return self

    def __exit__(self, *exc: Any) -> None:
        self.elapsed_s = round(time.monotonic() - self._start, 4)
