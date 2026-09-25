"""Tests for the framework adapters.

The core is tested unconditionally — it is where the behaviour lives. The
LangChain and LlamaIndex cases are skipped when those frameworks are absent,
because an adapter test that silently passes without the framework installed
would be a test that an integration works when nothing checked that it does.
"""

from __future__ import annotations

import os
import sys
import unittest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from integrations.core import Passage, passage_from_result, retrieve_passages  # noqa: E402


class FakeBrain:
    """Stands in for a running brain, in the shape /v1/search returns."""

    def __init__(self, results=None, facts=None):
        self.results = results if results is not None else [
            {
                "text": "Retries back off exponentially, capped at 30s.",
                "id": "fact:1",
                "source": "fact",
                "path": "docs/retry.md",
                "line": 12,
                "score": 0.91,
            },
            {"text": "   ", "id": "fact:blank", "source": "fact"},
            {"text": "The cache is invalidated on branch change.", "id": "fact:2", "source": "fact"},
        ]
        self._facts = facts if facts is not None else [{"text": "We deploy on Thursdays."}]
        self.calls = []

    def search(self, query, limit=8, **kwargs):
        self.calls.append({"query": query, "limit": limit, **kwargs})
        return self.results

    def facts(self, limit=20, **kwargs):
        return self._facts


class CoreTests(unittest.TestCase):
    def test_a_result_with_no_text_is_dropped(self):
        # A framework will embed an empty string and then rank it, and the
        # caller cannot tell that back apart from a real hit.
        passages = retrieve_passages(FakeBrain(), "retries")
        self.assertEqual(len(passages), 2)
        self.assertTrue(all(p.text.strip() for p in passages))

    def test_metadata_carries_only_the_keys_present(self):
        passages = retrieve_passages(FakeBrain(), "retries")
        self.assertEqual(passages[0].metadata["path"], "docs/retry.md")
        # fact:2 has no path, so the key is absent rather than None — a
        # consumer checking `"path" in metadata` must not be misled.
        self.assertNotIn("path", passages[1].metadata)

    def test_citation_prefers_path_and_line(self):
        self.assertEqual(
            Passage("t", {"path": "docs/retry.md", "line": 12}).citation, "docs/retry.md:12"
        )
        self.assertEqual(Passage("t", {"id": "fact:9"}).citation, "fact:9")

    def test_empty_query_is_refused_before_the_request(self):
        brain = FakeBrain()
        for bad in ("", "   "):
            with self.assertRaises(ValueError):
                retrieve_passages(brain, bad)
        self.assertEqual(brain.calls, [], "a refused query must not reach the brain")

    def test_limit_must_be_positive(self):
        with self.assertRaises(ValueError):
            retrieve_passages(FakeBrain(), "retries", limit=0)

    def test_passage_from_result_returns_none_for_blank(self):
        self.assertIsNone(passage_from_result({"text": "\n\t "}))


def _has(module):
    try:
        __import__(module)
        return True
    except ImportError:
        return False


@unittest.skipUnless(_has("langchain_core"), "langchain-core not installed")
class LangChainTests(unittest.TestCase):
    def test_retriever_returns_documents_with_metadata(self):
        from integrations.langchain import BrainRetriever

        docs = BrainRetriever(brain=FakeBrain()).invoke("how are retries handled")
        self.assertEqual(len(docs), 2)
        self.assertEqual(docs[0].page_content, "Retries back off exponentially, capped at 30s.")
        self.assertEqual(docs[0].metadata["path"], "docs/retry.md")

    def test_source_and_branch_reach_the_brain_only_when_set(self):
        from integrations.langchain import BrainRetriever

        brain = FakeBrain()
        BrainRetriever(brain=brain).invoke("q")
        self.assertNotIn("source", brain.calls[0])
        BrainRetriever(brain=brain, source="fact", branch="main").invoke("q")
        self.assertEqual(brain.calls[1]["source"], "fact")
        self.assertEqual(brain.calls[1]["branch"], "main")

    def test_tools_cite_their_source(self):
        from integrations.langchain import brain_tools

        tools = brain_tools(FakeBrain())
        self.assertEqual([t.name for t in tools], ["search_memory", "list_facts"])
        out = tools[0].invoke({"query": "retries"})
        self.assertIn("[docs/retry.md:12]", out)

    def test_tools_do_not_expose_a_write(self):
        # Recording durable memory should be granted deliberately, not arrive
        # as part of a convenience import.
        from integrations.langchain import brain_tools

        names = " ".join(t.name for t in brain_tools(FakeBrain()))
        for forbidden in ("remember", "write", "record", "delete", "retract"):
            self.assertNotIn(forbidden, names)

    def test_empty_memory_says_so_rather_than_returning_nothing(self):
        from integrations.langchain import brain_tools

        tools = brain_tools(FakeBrain(results=[], facts=[]))
        self.assertIn("No matching memory", tools[0].invoke({"query": "x"}))
        self.assertIn("no facts", tools[1].invoke({}))


@unittest.skipUnless(_has("llama_index.core"), "llama-index-core not installed")
class LlamaIndexTests(unittest.TestCase):
    def test_retriever_returns_scored_nodes(self):
        from integrations.llamaindex import BrainRetriever

        nodes = BrainRetriever(FakeBrain()).retrieve("how are retries handled")
        self.assertEqual(len(nodes), 2)
        self.assertEqual(nodes[0].node.text, "Retries back off exponentially, capped at 30s.")
        self.assertEqual(nodes[0].score, 0.91)

    def test_a_result_without_a_score_is_not_given_one(self):
        # The brain's score is a rank signal, not a similarity; inventing a
        # value would let a consumer threshold on something meaningless.
        from integrations.llamaindex import BrainRetriever

        brain = FakeBrain(results=[{"text": "No score here.", "id": "fact:3"}])
        nodes = BrainRetriever(brain).retrieve("q")
        self.assertIsNone(nodes[0].score)


if __name__ == "__main__":
    unittest.main()
