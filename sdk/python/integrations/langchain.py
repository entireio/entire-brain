"""LangChain adapters for Entire Brain.

Importing this module requires `langchain-core`; importing `entire_brain` does
not. That split is deliberate — the client stays dependency-free, and only a
caller who already has LangChain pays for it.

    from entire_brain import Brain
    from entire_brain.integrations.langchain import BrainRetriever, brain_tools

    brain = Brain("http://127.0.0.1:7777", token="…")
    retriever = BrainRetriever(brain=brain)
    chain = {"context": retriever} | prompt | model
"""

from __future__ import annotations

from typing import Any

from .core import retrieve_passages

try:  # pragma: no cover - exercised by having langchain installed
    from langchain_core.callbacks import CallbackManagerForRetrieverRun
    from langchain_core.documents import Document
    from langchain_core.retrievers import BaseRetriever
    from langchain_core.tools import StructuredTool
except ImportError as exc:  # pragma: no cover
    raise ImportError(
        "entire_brain.integrations.langchain needs langchain-core.\n"
        "  pip install langchain-core\n"
        "The Entire Brain client itself has no dependencies; only this adapter does."
    ) from exc

__all__ = ["BrainRetriever", "brain_tools"]


class BrainRetriever(BaseRetriever):
    """A LangChain retriever backed by a running brain.

    `source` narrows the corpus the way the REST API does ("fact", "history",
    "doc"); leaving it unset searches the default set.
    """

    brain: Any
    limit: int = 8
    source: str | None = None
    branch: str | None = None

    def _get_relevant_documents(
        self, query: str, *, run_manager: CallbackManagerForRetrieverRun | None = None
    ) -> list[Document]:
        kwargs: dict[str, Any] = {}
        if self.source is not None:
            kwargs["source"] = self.source
        if self.branch is not None:
            kwargs["branch"] = self.branch
        passages = retrieve_passages(self.brain, query, limit=self.limit, **kwargs)
        return [Document(page_content=p.text, metadata=p.metadata) for p in passages]


def brain_tools(brain: Any, limit: int = 8) -> list[StructuredTool]:
    """Tools an agent can call directly, rather than a retriever in a chain.

    Only reads. Recording a fact is deliberately not exposed here: a tool that
    writes durable memory should be granted on purpose, not arrive as part of a
    convenience import.
    """

    def search_memory(query: str) -> str:
        passages = retrieve_passages(brain, query, limit=limit)
        if not passages:
            return "No matching memory."
        return "\n\n".join(f"[{p.citation}] {p.text}" for p in passages)

    def list_facts(limit_override: int = limit) -> str:
        facts = brain.facts(limit=limit_override)
        if not facts:
            return "This brain holds no facts yet."
        return "\n".join(f"- {fact.get('text', '')}".rstrip() for fact in facts)

    return [
        StructuredTool.from_function(
            func=search_memory,
            name="search_memory",
            description=(
                "Search this repository's durable memory — facts, past sessions and "
                "documents. Returns passages with their origin in brackets."
            ),
        ),
        StructuredTool.from_function(
            func=list_facts,
            name="list_facts",
            description="List the durable facts recorded about this repository.",
        ),
    ]
