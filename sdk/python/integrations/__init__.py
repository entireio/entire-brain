"""Framework adapters for Entire Brain.

A memory service that an agent framework cannot call is a memory service that
agent does not use. These are the adapters — LangChain and LlamaIndex — plus
the framework-neutral layer they both sit on.

Nothing here is imported by `entire_brain` itself, and importing a framework
adapter is the only thing that requires that framework to be installed:

    from entire_brain import Brain
    from entire_brain.integrations.langchain import BrainRetriever

    retriever = BrainRetriever(brain=Brain("http://127.0.0.1:7777", token="…"))
"""

from .core import Passage, retrieve_passages

__all__ = ["Passage", "retrieve_passages"]
