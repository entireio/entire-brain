# Memory integrity component canary

This synthetic, development-only experiment measures whether evidence survives
retrieval when distilled claims are wrong or incomplete. It is **not** an Agent
Brain coding benchmark, a held-out corpus, or evidence of better coding outcomes.
No real sessions, private benchmark artifacts, or installed Brain stores are used.

Eight cases cover lost scope, reversed negation, later corrections, branch
mis-scoping, hypotheses promoted to causes, omitted decisions, invalid negative
inferences, and invented values. Corruptions are injected explicitly; their
frequency says nothing about the actual distiller's error rate.

## Run the retrieval canary

From the repository root, build the current checkout and run:

```sh
go build -o bin/entire-brain ./cmd/entire-brain
python benchmarks/agent-brain/memory_integrity.py prepare \
  --brain-bin bin/entire-brain --out /tmp/brain-integrity-run
```

On Windows use `bin/entire-brain.exe` and a new directory under `$env:TEMP`.
The output directory must not already exist. Default `--k 4` and
`--budget-bytes 4096` are explicit, recorded controls; tune them only on this
development corpus. Smaller budgets are useful stress cases.

The harness seeds a temporary Brain in its documented on-disk format, with all
plugin directories isolated. It calls the built binary's existing `facts eval`
paths (`facts` and `raw-sessions`) with opt-in `--include-context --json`.
No model is invoked. `raw-sessions` uses the product's transcript preprocessing
and lexical ranking, not embeddings or the full `brief` retrieval pipeline.
The canary refuses source transformations or multi-chunk sources rather than
claiming full-source coverage from a partial excerpt.

| Arm | Delivered memory |
|---|---|
| `no_memory` | Empty packet |
| `raw_history` | Product-ranked raw source passages |
| `facts_only` | Product-ranked distilled claims |
| `facts_with_sources` | Same ranked claims, their branch-matching source passages, and independently ranked raw-history backfill |

Source expansion is an **experimental harness composition**, not a newly shipped
Brain retrieval command. The source lookup uses fact provenance, never answer
labels. Branch filtering precedes source expansion. This does not repair a
mis-scoped claim automatically: it remains visible as an uncorroborated claim.
Independent raw retrieval can supply the applicable branch's evidence.

Each arm uses the same ceiling on the exact UTF-8 JSON bytes of the memory array.
Sources and their claim are packed together without truncation, duplicate sources
are charged once, and omitted groups are reported. Byte parity is not token parity;
the report's bytes/4 token field is only an estimate. Candidate counts and retrieval
work also differ: the combined arm uses two retrieval paths plus source lookup.
This is a memory-content ablation, not an equal-compute comparison.

## Outputs and interpretation

- `facts.json` / `raw-sessions.json`: actual product retrieval outputs.
- `retrieval-report.json`: per-case, per-arm evidence recall, complete-evidence
  coverage, known misleading claims delivered, omitted groups, packet bytes,
  fixture hash, binary hash, and runner hash.
- `requests.jsonl`: reader inputs projected without answer keys, labels, case ids,
  or arm names. Request digests bind the exact task, choices, instruction and packet.

High evidence recall does not mean the reader used the evidence correctly.
A misleading claim delivered alongside its correction remains counted as exposure;
it is not automatically labeled an answer failure. No answer accuracy is emitted
by `prepare`. Even perfect results on this tiny corpus are component evidence only.

## Collect and score reader responses

Use one pinned reader model, effort, instruction and output limit across arms.
Give each distinct request a fresh, tool-free context with only that request;
do not give it this directory, the source fixture, reports, answer labels, other
requests, or prior reader outputs. Identical request digests can share one response
within a repetition. Run repetitions separately; randomize delivery order and
retain provider usage and latency in the external reader log.

The canary intentionally does not launch a provider or reuse a logged-in coding
agent that can read its answer key. A reader response is:

```json
{"request_sha256":"<digest from request>","choice":"<one offered choice>","citations":["source:<id from packet>"]}
```

Write one response per distinct request to a JSONL file, then:

```sh
python benchmarks/agent-brain/memory_integrity.py score \
  --run /tmp/brain-integrity-run --responses /tmp/reader-responses.jsonl \
  --reader-id '<model>/<effort>/<harness-version>/<repetition>' \
  --out /tmp/reader-score.json
```

Scoring rejects missing/duplicate responses, wrong fixture bindings, modified
requests and unknown choices. It separately reports decision correctness,
abstention, fabricated citation ids and coverage of required evidence citations.
Citation coverage is a labeled-id check, **not** semantic entailment or a count
of unsupported free-form claims. Reader provenance is declared by the external
runner and must be audited there; this scorer does not certify provider execution.

## Next experiment

Before a product change or an accuracy claim, add independently reviewed real
coding regressions to the existing Agent Brain harness under `CONDITIONS.md`.
Keep development cases separate from held-out task/source commits. Compare the
same model, source, task, validation and context budget; measure regression-test
success, wrong-memory adoption, answer support, provider tokens and latency.
Separately evaluate the actual distiller on source-grounded labels: this canary
tests recovery from injected errors, not how often extraction creates them.

Research motivation: [LongMemEval](https://arxiv.org/abs/2410.10813),
[HaluMem](https://arxiv.org/abs/2511.03506),
[LongMemEval-V2](https://arxiv.org/abs/2605.12493), and
[MemoryArena](https://arxiv.org/abs/2602.16313).

For the optional recent-history reserve and its live A/B results, see [RECENT-HISTORY-RESERVE.md](RECENT-HISTORY-RESERVE.md). The default remains zero (baseline packing).
