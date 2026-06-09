# Thomas Release Readiness Audit

This audit tracks the loose ends Thomas called out before `entire-brain`,
`entire-sem`, and `entire-replay-lab` can be shipped with honest claims.

## Distill Performance

Thomas observed `entire brain distill` running for more than 24 hours on a repo
with many Entire sessions. Current implementation distilled one transcript chunk
per agent call and then reconciled candidates with another agent step, so large
session stores can create thousands of serial calls.

Evidence added:

- `entire brain distill --dry-run --json` reports sessions, cached sessions,
  preprocessed bytes, chunks, branch spread, largest sessions, and an upper bound
  on extraction plus reconcile agent calls without calling an agent or writing
  facts.
- Distill summaries now include additive cache/timing fields:
  `cache_hits`, `failed_chunks`, `preprocessed_bytes`, `extraction_seconds`,
  `reconcile_seconds`, and `write_seconds`.
- `--jobs N` parallelizes extraction calls only. Candidate reconciliation and
  all writes still happen in deterministic session/chunk order.
- `--agent ollama --model <model>` can use local loopback Ollama through
  `ENTIRE_BRAIN_OLLAMA_URL` or `http://127.0.0.1:11434/api/generate`.

Claim policy: do not claim distill is "fast" until a real large-repo dry-run and
timed run show the call count and wall-time improvement.

## Facts Vs Raw Sessions

Thomas questioned whether facts recall is actually better than searching raw
session memory.

Evidence added:

- `entire brain facts eval --retriever facts|history|query|raw-sessions` compares
  retrieval arms over the same task file.
- Eval task JSON now accepts `source_session_id` and `source_transcript_path` so
  raw-session baselines can cite retained source; generated task files populate
  those fields.
- The eval `query` arm is read-only and lexical inside the harness, so it does
  not write embedding caches or call an embedder while measuring baselines.
- Eval summaries include `retriever` and `latency_ms`; `eval-compare` includes a
  per-metric winner and explicit claim text.

Claim policy: do not say facts are better than raw sessions unless paired evals
show a significant lift for the metric being claimed.

## Semantic / Tree-Sitter Proof

Thomas questioned whether tree-sitter and semantic indexing actually work.

Evidence added:

- `entire brain semantic-audit --json` reports provider/schema state, files,
  symbols, relations, warnings, partial failures, freshness axes, and blind
  spots from the local semantic manifest/store.
- The full benchmark panel now includes
  `entire-brain-semantic-completeness-tolerance`, a semantic freshness task whose
  prompt does not name the implementation file.

Claim policy: do not say semantic indexing works globally. Say which languages,
relations, and freshness states are covered, and show blind spots.

## QMD Alignment

Thomas said the API was aligned with Tobi's QMD. The current Entire Brain
retrieval surface keeps the qmd-inspired core verbs:

- `search`
- `vsearch`
- `query`
- `get`
- `multi-get`

Compatibility subset added:

- `--format json|cli` as an alias for JSON/CLI output selection.
- `-n` / `--number` as an alias for result count on search verbs.

Sources checked:

- [QMD README](https://github.com/tobi/qmd/blob/main/README.md) documents
  `search`, `vsearch`, `query`, `get`, `multi-get`, and `--json -n 10` for
  agent-oriented output.
- [QMD v2.5.3 release notes](https://github.com/tobi/qmd/releases/tag/v2.5.3)
  prefer `--format <kind>` while preserving legacy boolean output flags.

Intentional differences:

- Entire Brain sources are repo brain layers, not arbitrary QMD collections.
- `--branch` remains fact-branch-specific.
- `query` is hybrid over local brain layers; it is not a remote or hosted search.
- QMD formats beyond `json` and `cli` (`csv`, `md`, `xml`, `files`) are not
  implemented in this pass.

## Release Narrative

The launch story is tracked in `docs/release_press_release.md`.

Release claims must stay local-first and evidence-backed:

- `entire-brain`: local durable repo memory and qmd-inspired retrieval.
- `entire-sem`: local semantic code understanding with freshness and blind-spot
  reporting.
- `entire-replay-lab`: measured agent outcomes, not anecdotes.

## Audit Findings Closed In This Pass

- Distill no longer aborts a parallel run merely because the earliest ordered
  chunks failed when later chunks succeeded.
- Branch-limited distill preserves cache entries for untouched branches, so a
  main-only run does not force unchanged feature sessions to re-distill later.
- Ollama distill rejects redirects that leave loopback, preserving the local-only
  contract.
- Eval-generated tasks now retain `source_session_id` and
  `source_transcript_path`; raw/history retrievers can receive source-granularity
  relevance credit without claiming recall labels they do not have.
- Eval's `query` arm is read-only and lexical in the harness, avoiding accidental
  embedder calls and cache writes during baseline measurement.
- `eval-compare` rejects duplicate/mismatched task ids, skips recall where it is
  undefined, and populates `p_holm_threshold`.
- `--format json` now requests JSON error envelopes, matching successful output
  behavior.
- The semantic benchmark task uses a portable repo path and hides expected files
  and validation from the agent prompt.

## Commands Run For This Audit

Local implementation checks on 2026-06-09:

- `git diff --check`
- `go test ./...`
- `python3 benchmarks/agent-brain/run_test.py`
- `go vet ./...`
- `go test -race ./...`
- `GOOS=windows GOARCH=amd64 go build ./...`
- `mise run check`

Release evidence still to collect on the target repo(s):

- `entire brain distill --dry-run --json` on the large repo where distill ran for
  more than 24 hours.
- Timed `entire brain distill --agent <agent> --jobs N --json` runs, with the
  manifest timing fields retained.
- Paired `entire brain facts eval --retriever facts|history|query|raw-sessions`
  runs over the same labeled task set.
- `entire brain semantic-audit --json` from the release candidate brain.
- Benchmark panel or retained proof subset from `benchmarks/agent-brain`.
