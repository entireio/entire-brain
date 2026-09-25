"""The part of the adapters that is not about any framework.

Every framework wants the same thing from a memory service — text, and enough
provenance to cite it — and disagrees only about the class it arrives in. That
disagreement is the thin part; this is the rest, and it is testable without
installing anything.
"""

from __future__ import annotations

from typing import Any, Iterable, NamedTuple

__all__ = ["Passage", "retrieve_passages", "passage_from_result"]

# Fields worth carrying into a framework's metadata. A result's own keys vary by
# source — a fact has no line number, a history hit has no fact id — so the map
# is built from what is present rather than from a fixed schema, and an absent
# key is simply absent instead of None.
_METADATA_KEYS = (
    "id",
    "source",
    "path",
    "line",
    "branch",
    "score",
    "global",
    "verification_required",
)


class Passage(NamedTuple):
    """One retrieved passage: its text, and where it came from."""

    text: str
    metadata: dict[str, Any]

    @property
    def citation(self) -> str:
        """A short human-readable origin, for a prompt that must attribute."""
        path = self.metadata.get("path")
        line = self.metadata.get("line")
        ident = self.metadata.get("id") or self.metadata.get("source") or "brain"
        if path and line:
            return f"{path}:{line}"
        return str(path or ident)


def passage_from_result(result: dict[str, Any]) -> Passage | None:
    """Convert one `/v1/search` result. Returns None when there is no text.

    A result with nothing to read is dropped rather than passed along as an
    empty document: a framework will happily embed an empty string and then
    rank it, and the caller has no way to tell that back apart from a real hit.
    """
    text = (result.get("text") or "").strip()
    if not text:
        return None
    metadata = {key: result[key] for key in _METADATA_KEYS if key in result}
    return Passage(text=text, metadata=metadata)


def retrieve_passages(brain: Any, query: str, limit: int = 8, **kwargs: Any) -> list[Passage]:
    """Search a brain and return passages a framework can wrap.

    `brain` is anything with a `.search(query, limit=..., **kwargs)` returning
    the REST shape — the real client, or a stand-in in a test.
    """
    if not query or not query.strip():
        raise ValueError("query must not be empty")
    if limit <= 0:
        raise ValueError("limit must be greater than zero")
    results: Iterable[dict[str, Any]] = brain.search(query, limit=limit, **kwargs)
    passages = []
    for result in results:
        passage = passage_from_result(result)
        if passage is not None:
            passages.append(passage)
    return passages
