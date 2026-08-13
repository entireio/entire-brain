# Temporal eligibility before ranking

## Decision

Rolling-cutoff frozen-brain runs use a task-scoped candidate constraint in
`entire-brain recall`. The harness supplies `--eligible-before`, the immutable
session-date map, and any `--exclude-session-id` values. Recall loads the full
branch corpus, proves temporal eligibility for every fact, and only then passes
the eligible facts to scope/kind/locus filtering and lexical or semantic ranking.

This is completeness-preserving: top-K is selected from the complete eligible
set. It does not depend on an over-fetch factor or assume how many ineligible
facts can rank above an eligible fact.

## Alternatives evaluated

- A filtered quarantine copy was rejected because hiding facts from the semantic
  store makes a normal cache flush indistinguishable from corpus deletion. It
  also duplicates a large frozen artifact for every task.
- Heuristic over-fetch/backfill was rejected because there is no finite bound on
  the number of highly ranked ineligible facts without first enumerating the
  corpus; an unbounded factor cannot prove completeness.
- A caller-generated eligible-ID allowlist was viable but would duplicate product
  fact parsing and provenance rules in Python. The chosen recall input computes
  that allowlist at the product boundary from the same loaded records immediately
  before ranking.

## Fail-closed contract

A fact is eligible only when it has non-empty provenance and every anchor has a
known session timestamp strictly before the cutoff. Missing anchor IDs, unknown
or malformed session dates, excluded task sessions, and timestamps equal to or
after the cutoff exclude the fact. Offset timestamps are compared as instants;
legacy naive timestamps are interpreted as UTC. Status is orthogonal: active and
superseded facts receive the same temporal decision, after which normal recall
status handling applies.

## Lexical, semantic, and cache behavior

The temporal filter runs before construction of the semantic reranker and before
`rankFactsFused`, so lexical-only, bundled Model2Vec RRF, and EmbeddingGemma RRF
receive the identical eligible candidate slice. Ranking algorithms and scores are
unchanged.

Frozen benchmark recall also passes `--read-only-semantic-cache`. Semantic vectors
may be read and missing vectors may be computed in memory for the query, but the
store is not flushed. Temporarily ineligible facts therefore cannot be interpreted
as deleted, no vector is pruned, and neither the source facts nor vector artifacts
are mutated. Ordinary non-benchmark recall retains its existing cache lifecycle.

## Audit and compatibility

JSON recall output includes `eligibility` only when a temporal constraint is
requested. It records `prefilter_corpus_count`, `eligible_count`, exclusions by
reason, and per-query `delivered_count`. The harness verifies corpus counts agree
across all queries and records the common counts, query count, and final deduped
packet `delivered_count` under `brain_prep.temporal_eligibility` in each run record.

Tasks without `rolling_cutoff_rfc3339` keep the prior recall command and JSON
shape. The harness retains its post-recall fail-closed filter as defense in depth,
but it no longer provides completeness; completeness comes from the product-side
candidate constraint.
