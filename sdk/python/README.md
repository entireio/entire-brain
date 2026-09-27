# Entire Brain — Python client and framework adapters

A brain runs on your machine. `entire_brain` is the client; the adapters under
`integrations/` are what let an agent framework use it without anyone writing
HTTP by hand.

```sh
entire brain mcp --http 7777      # binds loopback, prints a bearer token
```

## Client

No dependencies — standard library only. A memory client that drags in a
dependency tree is one people vendor rather than install.

```python
from entire_brain import Brain

brain = Brain("http://127.0.0.1:7777", token="…")
for hit in brain.search("retry policy"):
    print(hit["source"], hit["text"])
```

## LangChain

Needs `langchain-core`. Importing the adapter is the only thing that requires
it; importing `entire_brain` does not.

```python
from entire_brain import Brain
from entire_brain.integrations.langchain import BrainRetriever, brain_tools

brain = Brain("http://127.0.0.1:7777", token="…")

retriever = BrainRetriever(brain=brain, limit=8)          # a Retriever, for chains
chain = {"context": retriever} | prompt | model

tools = brain_tools(brain)                                 # or Tools, for agents
```

`source` narrows the corpus the way the REST API does — `"fact"`, `"history"`,
`"doc"` — and `branch` selects a branch.

`brain_tools` returns reads only. Recording a durable fact is deliberately not
among them: a tool that writes memory should be granted on purpose, not arrive
as part of a convenience import.

## LlamaIndex

Needs `llama-index-core`.

```python
from entire_brain.integrations.llamaindex import BrainRetriever

nodes = BrainRetriever(brain, limit=8).retrieve("how are retries handled")
```

Each node carries the brain's score when it has one. A result without a score
is not given one — the score is a rank signal rather than a similarity in
[0, 1], and inventing a value would let a consumer threshold on nothing.

## What the adapters guarantee

- **A result with no text never becomes a document.** A framework will embed an
  empty string and rank it, and the caller cannot tell that from a real hit.
- **Metadata carries only the keys a result actually had.** A fact has no line
  number; the key is absent rather than `None`, so `"path" in metadata` means
  what it says.
- **Provenance survives.** `Passage.citation` renders `path:line` when both are
  present, and the tool output puts it in brackets before the text.

## Tests

```sh
python3 -m pip install langchain-core llama-index-core
python3 -B -m unittest discover -s tests -p 'test_*.py'
```

The framework cases skip when the framework is absent. CI installs both, because
a suite that skips them would assert that an integration works while checking
nothing.
