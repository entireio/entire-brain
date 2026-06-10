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
  preprocessed bytes, scheduled chunks, `chunks_if_uncached`, branch spread,
  largest sessions, `schema_version`, and an upper bound on extraction plus
  reconcile agent calls without calling an agent or writing facts. Cached-session
  rows keep `chunks` as scheduled work and use `chunks_if_uncached` for the warm
  cache capacity estimate.
- Distill summaries now include additive cache/timing fields:
  `cache_hits`, `failed_chunks`, `preprocessed_bytes`, `extraction_seconds`,
  `reconcile_seconds`, and `write_seconds`.
- `--jobs N` parallelizes extraction calls only. Candidate reconciliation and
  all writes still happen in deterministic session/chunk order.
- `--agent ollama --model <model>` can use local loopback Ollama through
  `ENTIRE_BRAIN_OLLAMA_URL` or `http://127.0.0.1:11434/api/generate`; the HTTP
  transport bypasses proxies and resolves/dials only loopback IP targets.

Claim policy: do not claim distill is "fast" until a real large-repo dry-run and
timed run show the call count and wall-time improvement.

Local smoke evidence collected on this repo: `go run ./cmd/entire-brain distill
--dry-run --json` completed without agent calls and reported 147 sessions,
250,321,142 raw bytes, 7,419,024 preprocessed bytes, 259 extraction chunks, and
an upper bound of 518 extraction plus reconcile agent calls. This validates the
sizing surface and explains why backfill scales painfully, but it is not the
target large-repo performance proof.

Blocked evidence collection: the target large session repo and retained timed
run artifacts are still needed before any public performance claim.

## Facts Vs Raw Sessions

Facts retrieval must be compared against history, unified-query, and preprocessed
session baselines before release claims say it is better.

Implemented eval surfaces:

- `entire brain facts eval --retriever facts|history|query|raw-sessions` compares
  facts, history, unified query, and preprocessed session chunk arms over the
  same task file.
- Eval task JSON now accepts `source_session_id`, `source_transcript_path`, and
  `source_lines` so preprocessed session baselines can cite retained source
  anchors; generated task files populate those fields, and line anchors are
  source-overlap proxies rather than proof labels.
- The eval `query` arm is read-only and lexical inside the harness, so it does
  not write embedding caches or call an embedder while measuring baselines.
- History and unified-query eval arms filter session-derived history by the
  task branch, matching the branch scope already used by facts and raw-session
  retrievers.
- Eval summaries include `retriever`, `latency_ms`, and `relevance_source`
  (`explicit_label`, `partial_explicit_label`, `mixed_explicit_source_match`,
  `source_match`, `judge`, or `none`), so charts can separate source-match proxy
  credit from labeled recall and spot query arms that mix fact labels with other
  source types.
- `eval-compare` includes a per-metric winner and explicit claim text, and now
  rejects non-proof or different relevance sources unless
  `--allow-proxy-comparison` is explicit. Same-source `source_match` proxy is
  still proxy evidence, not proof evidence.
- `facts eval` validates `label_source`; task files with `relevant` ids must
  say whether labels are `human`, `judge_refined`, or `provenance_silver`.
- `eval-compare` also rejects differing non-empty `run_config.tasks_sha256` and
  `run_config.brain_manifest_sha256` values unless the matching override is
  explicit, so same-id eval runs with different task labels or brain state
  cannot masquerade as paired proof.

Claim policy: do not say facts are better than raw/preprocessed sessions unless
paired evals show a significant lift for the metric being claimed. Do not compare
explicit-label recall, `source_match` proxy credit, provenance-silver labels, or
judge/proxy metrics unless `--allow-proxy-comparison` is called out.

Local baseline smoke collected on this repo: `go run ./cmd/entire-brain refresh
sessions` indexed 39,242 history records. A source-session task set generated
with `facts eval-gen --source sessions --limit 8` had
`tasks_sha256=sha256:8453817d5a9bb87e33e266e6b18e0fa832ea07934f9e2d1534e0c658f65321f7`.
Against that task file, `raw-sessions` returned mean 42,124.75 tokens and
source-match proxy useful/1k of 0.0148; `history` returned mean 921.25 tokens
and source-match proxy useful/1k of 1.0342; `query` had no relevance labels for
the surfaced mixed-source rows; `facts` surfaced zero rows because this local
brain has no durable facts. This validates the offline eval plumbing and the
token-cost contrast, but it is not facts-vs-raw release proof.

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
- `refresh index` now tolerates local semantic providers that do not support the
  provider-side `--ignore-file` flag: it retries without the flag, records a
  `provider_ignore_file_unsupported` warning, and still applies `.brainignore`
  during Entire Brain's own snapshot filtering before persisting records.
- The benchmark task inventory includes
  `entire-brain-semantic-completeness-tolerance`, a semantic freshness task whose
  prompt does not name the implementation file. Retained benchmark outcomes are
  still needed before claiming agent improvement.

Claim policy: do not say semantic indexing works globally. Say which languages,
relations, and freshness states are covered, and show blind spots.

Local clean evidence collected on this repo after the provider compatibility
fix: `go run ./cmd/entire-brain refresh index --sem-binary entire --force`
indexed 134 files, 2,250 symbols, and 14,883 relations at `721aae0`; `go run
./cmd/entire-brain semantic-audit --json --fail-on unsafe` passed with freshness
`ok`, worktree state `clean`, zero blind spots, and one retained warning:
`provider_ignore_file_unsupported`. This proves local coverage and audit health
for this checkout, not agent usefulness.

Blocked evidence collection: semantic benchmark/proof records must still be
retained and audited before usefulness claims graduate from draft language.

Negative replay-lab pilot: `python3 benchmarks/agent-brain/run.py run --tasks
entire-brain-semantic-completeness-tolerance.json --runners
codex:gpt-5.4-mini:low --conditions no_brain,semantic_brain --repetitions 3
--suite-name release-local-semantic-proof-20260609T2305Z` completed, but the
summary was `brain_negative` (`proof_ready=false`, stability `noisy`). The
independent audit rejected it with 0 proof-ready comparisons and 3 hard
`E:agent_leak_audit_failed` flags. This suite is a negative pilot, not release
evidence.

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

Local smoke evidence collected on this repo: `go run ./cmd/entire-brain search
"semantic provider" --format json -n 2`, `query "distill dry run" --format json
-n 2`, and `vsearch "semantic audit" --format json -n 2` all returned stable
JSON envelopes with two results. `search --help` lists both `--format` and
`-n, --number`.

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
- QMD flags for collections/context and file-only browsing are not implemented:
  `--files`, `--all`, `--min-score`, `--full`, `--full-path`, line-range `get`,
  line-number toggles, and collection filters remain outside this pass.
- QMD formats beyond `json` and `cli` (`csv`, `md`, `xml`, `files`) and legacy
  boolean format flags are not implemented in this pass.
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
- `eval-compare` now fails closed on non-proof/different relevance sources
  unless `--allow-proxy-comparison` is passed, so proxy-vs-label and
  proxy-vs-proxy comparisons are visible in command history and JSON output.
- `eval-compare` now fails closed on task-file and brain-manifest hash
  mismatches unless the corresponding override is passed, so same-id/different
  label files or changed brain state cannot produce quiet significance claims.
- History/query eval arms now branch-filter session-derived history records,
  including legacy history records that need branch inference from their
  transcript path.
- Eval's `query` arm is read-only and lexical in the harness, avoiding accidental
  embedder calls and cache writes during baseline measurement.
- `eval-compare` rejects duplicate/mismatched task ids, skips recall where it is
  undefined, and populates `p_holm_threshold`.
- `--format json` now requests JSON error envelopes, matching successful output
  behavior.
- The semantic benchmark task uses a portable repo path and hides expected files
  and validation from the agent prompt.
- Benchmark validation now fails closed when a task defines zero validation
  commands, and the independent release auditor treats missing or empty
  validation results as hard flags. A proof-ready comparison cannot be backed by
  unvalidated records.
- The query default-limit benchmark tasks now target the current unified
  retrieval implementation in `internal/cli/retrieve_cmd.go`, including the
  QMD-style `-n` alias, instead of the old semantic-query implementation.

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
- `python3 benchmarks/agent-brain/run.py check --tasks
  entire-brain-semantic-completeness-tolerance.json
  entire-brain-mcp-tool-name.json entire-brain-query-default-limit.json
  entire-brain-stale-query-default-limit.json`

This documentation claim-hygiene pass checked:

- `git diff --check`
- `git diff --no-index --check -- /dev/null docs/release_readiness_audit.md`
  during the first audit-doc pass; no whitespace warnings were emitted, and
  Git's nonzero no-index diff exit is expected for `/dev/null` comparisons

Release evidence still to collect on the target repo(s), blocked until the target
repo/access/artifacts are available:

- `entire brain distill --dry-run --json` on the large repo where distill ran for
  more than 24 hours.
- Paired timed `entire brain distill --agent <agent> --jobs 1 --json` and
  `--jobs N --json` runs on the same target repo/cache state, same
  agent/model/effort, with the dry-run JSON, manifest timing fields, and
  external wall-clock artifacts retained.
- Paired `entire brain facts eval --retriever facts|history|query|raw-sessions`
  runs over the same labeled or explicitly proxy-allowed task set, same task
  hash, and same brain manifest hash.
- Semantic benchmark evidence from a retained proof suite. A clean local
  `semantic-audit --json --fail-on unsafe` run on this checkout is recorded
  above, but it does not prove agent usefulness by itself.
- Benchmark panel or retained proof subset from `benchmarks/agent-brain`, audited
  with `audit_codex.py --fail-on-flags` and committed under
  `benchmarks/agent-brain/evidence/release` as sanitized evidence.
- `mise run release:evidence` must pass before any replay-lab proof claim; it runs
  benchmark harness self-tests and then audits explicit `release-candidate-*`
  suites through `benchmarks/agent-brain/evidence/release/manifest.json`.
  Release mode rejects `release-local-*`, requires panel provenance, 4
  repetitions per side, zero hard flags, and at least one proof-ready
  comparison. Failed audits write to a temp directory only; passing audits copy
  the report into the retained evidence lane.

Current retained benchmark caveat: `benchmarks/agent-brain/results/codex-audit-report.md`
is quarantine/reference evidence, not release proof. The retained panel reports hard
integrity flags and zero provenance-complete agent records, so public replay-lab
claims require a new provenance-complete `release-candidate-*` run generated
from a committed panel and passing `python3 benchmarks/agent-brain/audit_codex.py
--release-manifest benchmarks/agent-brain/evidence/release/manifest.json
--out-dir benchmarks/agent-brain/evidence/release --fail-on-flags`.
The local `release-local-semantic-proof-20260609T2305Z` pilot is also not release
proof because its audited comparison was not proof-ready and the suite had hard
leak-audit flags. `release-local-*` names are intentionally outside the citable
release gate.
