# Release Readiness Audit

This audit tracks the loose ends that need to close before `entire-brain`,
`entire-sem`, and `entire-replay-lab` can ship with honest claims.

## Distill Performance

Large repos can make `entire brain distill` run for many hours because each
transcript chunk may become one extraction agent call and each candidate set can
also require reconcile work. Without a local preflight, users cannot tell whether
a run means tens, hundreds, or thousands of calls.

Implemented claim-control surfaces:

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

Blocked evidence collection: the target large session repo and retained timed
run artifacts are still needed before any public performance claim.

## Facts Vs Raw Sessions

Facts retrieval must be compared against history, unified-query, and preprocessed
session baselines before release claims say it is better.

Implemented eval surfaces:

- `entire brain facts eval --retriever facts|history|query|raw-sessions` compares
  facts, history, unified query, and preprocessed session chunk arms over the
  same task file.
- Eval task JSON now accepts `source_session_id` and `source_transcript_path` so
  preprocessed session baselines can cite retained source; generated task files
  populate those fields.
- The eval `query` arm is read-only and lexical inside the harness, so it does
  not write embedding caches or call an embedder while measuring baselines.
- Eval summaries include `retriever`, `latency_ms`, and `relevance_source`
  (`explicit_label`, `partial_explicit_label`, `mixed_explicit_source_match`,
  `source_match`, `judge`, or `none`), so charts can separate source-match proxy
  credit from labeled recall and spot query arms that mix fact labels with other
  source types.
- `eval-compare` includes a per-metric winner and explicit claim text, and now
  rejects mixed relevance sources unless `--allow-proxy-comparison` is explicit.

Claim policy: do not say facts are better than raw/preprocessed sessions unless
paired evals show a significant lift for the metric being claimed. Do not compare
explicit-label recall with `source_match` proxy credit unless
`--allow-proxy-comparison` is called out.

Blocked evidence collection: paired `facts eval` runs for `facts`, `history`,
`query`, and `raw-sessions` over the same labeled or explicitly proxy-authorized
task set are still required.

## Semantic / Tree-Sitter Proof

Semantic indexing must prove which provider records work and where they degrade
before release claims say it improves agent work. If the provider is
tree-sitter-backed, claims still need to be scoped to audited languages,
relations, freshness state, and blind spots.

Implemented audit surfaces:

- `entire brain semantic-audit --json` reports provider/schema state, files,
  symbols, relations, file-language counts, symbol-language counts, symbol-kind
  counts, relation-type counts, warning/failure details, freshness axes, and
  blind spots from the local semantic manifest/store.
- The benchmark task inventory includes
  `entire-brain-semantic-completeness-tolerance`, a semantic freshness task whose
  prompt does not name the implementation file. Retained benchmark outcomes are
  still needed before claiming agent improvement.

Claim policy: do not say semantic indexing works globally. Say which languages,
relations, and freshness states are covered, and show blind spots.

Blocked evidence collection: `entire brain semantic-audit --json` must be run on
the release-candidate brain, and semantic benchmark/proof records must be
retained and audited before usefulness claims graduate from draft language.

## QMD Alignment

The current Entire Brain retrieval surface keeps the qmd-inspired core verbs:

- `search`
- `vsearch`
- `query`
- `get`
- `multi-get`

QMD-inspired aliases added:

- `--format json|cli` as an alias for JSON/CLI output selection.
- `-n` / `--number` as an alias for result count on search verbs.

Sources checked during the local audit:

- [QMD README](https://github.com/tobi/qmd/blob/main/README.md) documents
  `search`, `vsearch`, `query`, `get`, `multi-get`, and `--json -n 10` for
  agent-oriented output.
- [QMD v2.5.3 release notes](https://github.com/tobi/qmd/releases/tag/v2.5.3)
  prefer `--format <kind>` while preserving legacy boolean output flags.

Intentional differences and unpinned areas:

- Entire Brain sources are repo brain layers, not arbitrary QMD collections.
- `--branch` remains fact-branch-specific.
- `query` is hybrid over local brain layers; it is not a remote or hosted search.
- QMD formats beyond `json` and `cli` (`csv`, `md`, `xml`, `files`) are not
  implemented in this pass.
- This is not a pinned upstream compatibility suite; release claims should say
  "QMD-inspired aliases" unless a fixture-backed QMD contract test is added.

## Release Narrative

The launch story is tracked in `docs/release_press_release.md`.

Release claims must stay local-first and evidence-backed:

- `entire-brain`: local durable repo memory and qmd-inspired retrieval.
- `entire-sem`: audit-reported local semantic code context with freshness and
  blind-spot reporting.
- `entire-replay-lab`: measured agent outcomes, not anecdotes.

## Audit Findings Closed In The Implementation Pass

- Distill no longer aborts a parallel run merely because the earliest ordered
  chunks failed when later chunks succeeded.
- Branch-limited distill preserves cache entries for untouched branches, so a
  main-only run does not force unchanged feature sessions to re-distill later.
- Ollama distill rejects redirects that leave loopback, preserving the local-only
  contract.
- Eval-generated tasks now retain `source_session_id` and
  `source_transcript_path`; raw/history retrievers can receive source-granularity
  relevance credit without claiming recall labels they do not have.
- Eval result JSON now includes `relevance_source`, making explicit labels,
  source-match proxy credit, judge credit, and no-label tasks distinguishable.
- `eval-compare` now fails closed on mixed relevance sources unless
  `--allow-proxy-comparison` is passed, so proxy-vs-label comparisons are visible
  in command history and JSON output.
- Eval's `query` arm is read-only and lexical in the harness, avoiding accidental
  embedder calls and cache writes during baseline measurement.
- `eval-compare` rejects duplicate/mismatched task ids, skips recall where it is
  undefined, and populates `p_holm_threshold`.
- `--format json` now requests JSON error envelopes, matching successful output
  behavior.
- The semantic benchmark task uses a portable repo path and hides expected files
  and validation from the agent prompt.

## Commands And Evidence Status

Implementation checks reported for the release-readiness implementation pass on
2026-06-09:

- `git diff --check`
- `go test ./...`
- `python3 benchmarks/agent-brain/run_test.py`
- `go vet ./...`
- `go test -race ./...`
- `GOOS=windows GOARCH=amd64 go build ./...`
- `mise run check`

This documentation claim-hygiene pass checked:

- `git diff --check`
- `git diff --no-index --check -- /dev/null docs/release_readiness_audit.md`
  for the untracked audit file; no whitespace warnings were emitted, and Git's
  nonzero no-index diff exit is expected for `/dev/null` comparisons

Release evidence still to collect on the target repo(s), blocked until the target
repo/access/artifacts are available:

- `entire brain distill --dry-run --json` on the large repo where distill ran for
  more than 24 hours.
- Timed `entire brain distill --agent <agent> --jobs N --json` runs, with the
  manifest timing fields retained.
- Paired `entire brain facts eval --retriever facts|history|query|raw-sessions`
  runs over the same labeled or explicitly proxy-allowed task set.
- `entire brain semantic-audit --json` from the release candidate brain.
- Benchmark panel or retained proof subset from `benchmarks/agent-brain`, audited
  with `audit_codex.py --fail-on-flags` and committed as sanitized evidence.

Current retained benchmark caveat: `benchmarks/agent-brain/results/codex-audit-report.md`
is quarantine/reference evidence, not release proof. The retained panel reports hard
integrity flags and zero provenance-complete agent records, so public replay-lab
claims require a new provenance-complete run (or a filtered suite subset) that passes
`python3 benchmarks/agent-brain/audit_codex.py --fail-on-flags --min-suites <n>
--min-records <n>`.
