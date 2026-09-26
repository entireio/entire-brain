"""LlamaIndex adapter for Entire Brain.

Importing this module requires `llama-index-core`; importing `entire_brain`
does not.

    from entire_brain import Brain
    from entire_brain.integrations.llamaindex import BrainRetriever

    retriever = BrainRetriever(Brain("http://127.0.0.1:7777", token="…"))
    nodes = retriever.retrieve("how are retries handled")
"""

from __future__ import annotations

from typing import Any

from .core import retrieve_passages

try:  # pragma: no cover - exercised by having llama-index installed
    from llama_index.core.retrievers import BaseRetriever
    from llama_index.core.schema import NodeWithScore, QueryBundle, TextNode
except ImportError as exc:  # pragma: no cover
    raise ImportError(
        "entire_brain.integrations.llamaindex needs llama-index-core.\n"
        "  pip install llama-index-core\n"
        "The Entire Brain client itself has no dependencies; only this adapter does."
    ) from exc

__all__ = ["BrainRetriever"]


class BrainRetriever(BaseRetriever):
    """A LlamaIndex retriever backed by a running brain."""

    def __init__(
        self,
        brain: Any,
        limit: int = 8,
        source: str | None = None,
        branch: str | None = None,
    ) -> None:
        self._brain = brain
        self._limit = limit
        self._source = source
        self._branch = branch
        super().__init__()

    def _retrieve(self, query_bundle: QueryBundle) -> list[NodeWithScore]:
        kwargs: dict[str, Any] = {}
        if self._source is not None:
            kwargs["source"] = self._source
        if self._branch is not None:
            kwargs["branch"] = self._branch
        passages = retrieve_passages(
            self._brain, query_bundle.query_str, limit=self._limit, **kwargs
        )
        nodes = []
        for passage in passages:
            node = TextNode(text=passage.text, metadata=dict(passage.metadata))
            # The brain's score is a rank signal, not a similarity in [0,1];
            # it is carried through rather than invented when absent.
            score = passage.metadata.get("score")
            nodes.append(NodeWithScore(node=node, score=score))
        return nodes
