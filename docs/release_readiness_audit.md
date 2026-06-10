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
  `reconcile_seconds`, `write_seconds`, and `total_seconds`, plus the
  self-describing run config and actual call counts needed for timed-run
  evidence: `agent`, `model`, `effort`, `jobs`, `effective_extraction_jobs`,
  `max_chunk_bytes`, `confidence_threshold`, `extraction_agent_calls`,
  `reconcile_agent_calls`, and `total_agent_calls`.
- `--jobs N` parallelizes extraction calls only. Candidate reconciliation and
  all writes still happen in deterministic session/chunk order.
- `--agent ollama --model <model>` can use local loopback Ollama through
  `ENTIRE_BRAIN_OLLAMA_URL` or `http://127.0.0.1:11434/api/generate`; the HTTP
  transport bypasses proxies and resolves/dials only loopback IP targets.
- `benchmarks/agent-brain/audit_distill_perf.py` audits retained performance
  artifacts for release proof: one dry-run JSON, one timed `--jobs 1` summary,
  and one timed `--jobs N` summary. It requires matching agent/model/branch/force
  / chunk / confidence config, retained artifact hashes, retained command-token
  provenance with an explicit non-`auto` agent, no missing transcripts, branch
  totals that add up, matching cache-hit counts, no warnings or failed chunks,
  bounded reconcile calls, timing components that fit under `total_seconds`,
  comparable chunk/call/output summaries, effective parallelism greater than 1,
  and observed `total_seconds` speedup above the manifest's `min_speedup`.
- `mise run distill:evidence` checks committed large-repo distill artifacts
  without rewriting them, while `distill:evidence:update` regenerates the audit
  report only after the validator passes. Today the task is intentionally
  expected to fail because no target large-repo dry-run/timed artifacts have
  been collected yet.

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
  source types. The human table now prints each row's relevance/label source and
  warns when non-proof relevance rows are present.
- `eval-compare` includes a per-metric winner and explicit claim text, and now
  rejects non-proof or different relevance sources unless
  `--allow-proxy-comparison` is explicit. Same-source `source_match` proxy is
  still proxy evidence, not proof evidence. Its JSON metrics also include
  `evidence_basis` (`proof_labels`, `proxy_or_mixed`, `operational`, or
  `unavailable`) and `release_claimable`, so an allowed proxy comparison cannot
  silently become proof copy.
- `facts eval` validates `label_source`; task files with `relevant` ids must
  say whether labels are `human`, `judge_refined`, or `provenance_silver`.
- `eval-compare` also rejects differing non-empty `run_config.tasks_sha256` and
  `run_config.brain_manifest_sha256` values unless the matching override is
  explicit, so same-id eval runs with different task labels or brain state
  cannot masquerade as paired proof.
- `benchmarks/agent-brain/audit_facts_eval.py` audits retained facts-eval
  artifacts for release proof: it requires the four retriever summaries, matching
  non-empty task and brain-manifest hashes, no proxy/missing/mismatch overrides,
  empty missing-task sets, and required comparison metrics that are significant,
  `release_claimable: true`, and `evidence_basis: "proof_labels"`.
- `mise run facts:evidence` checks committed facts-eval artifacts without
  rewriting them, while `facts:evidence:update` regenerates the audit report
  only after the validator passes. Today the task is intentionally not part of
  `mise run release:evidence` because no citable facts-vs-raw artifact has been
  collected yet.

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

Local collection recipe:

1. Build a task file with `facts eval-gen --source sessions --out
   /tmp/entire-facts-eval-tasks.json` for smoke, or replace/augment its
   `relevant` ids with human or judge-refined labels and `label_source:
   "human"` or `"judge_refined"` for proof.
2. Run the same tasks and brain state through all four arms:
   `facts eval --tasks /tmp/entire-facts-eval-tasks.json --retriever facts
   --json > /tmp/facts.json`, then repeat for `history`, `query`, and
   `raw-sessions`.
3. Compare pairs with `facts eval-compare --a /tmp/raw.json --b /tmp/facts.json
   --json`. Use `--allow-proxy-comparison` only for smoke/source-overlap runs;
   release claims require `evidence_basis: "proof_labels"` and
   `release_claimable: true` for the relevance metric being claimed.
4. Retain the four summaries, comparison JSON, a `manifest.json`, and
   `facts-eval-audit-report.{json,md}` under
   `benchmarks/agent-brain/evidence/facts-eval`, then run
   `mise run facts:evidence`.

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
- `entire brain inspect context --json` and `brain brief --json` now include
  relation endpoint `neighbors` as full symbol records, so agents can see
  caller/callee names and file/line spans without parsing relation ids and
  issuing extra lookups.
- `semantic-audit --fail-on release` is the release-health gate for semantic
  coverage: it emits the full report, then fails unless freshness is `ok` and
  blind spots are empty.
- `refresh index` now tolerates local semantic providers that do not support the
  provider-side `--ignore-file` flag: it retries without the flag, records a
  `provider_ignore_file_unsupported` warning, and still applies `.brainignore`
  during Entire Brain's own snapshot filtering before persisting records.
- The benchmark task inventory includes
  `entire-brain-semantic-completeness-tolerance`, a semantic freshness task whose
  prompt does not name the implementation file. Retained benchmark outcomes are
  still needed before claiming agent improvement.
- The semantic release-candidate inventory now also includes
  `entire-brain-semantic-tokenized-idf-ranking` and the committed
  `release-entire-brain-semantic-tokenized-idf` panel. The task regresses the
  IDF-weighted tokenized code-search ranking line and hides the expected file,
  validation, and benchmark scaffold. `run.py check` confirms it fails as
  expected; it still needs pilot/release repetitions before any usefulness claim.

Claim policy: do not say semantic indexing works globally. Say which languages,
relations, and freshness states are covered, and show blind spots.

Local clean evidence collected on this repo after the provider compatibility
fix: `go run ./cmd/entire-brain refresh index --sem-binary entire --force`
indexed 134 files, 2,250 symbols, and 14,883 relations at `721aae0`; `go run
./cmd/entire-brain semantic-audit --json --fail-on release` passed with freshness
`ok`, worktree state `clean`, zero blind spots, and one retained warning:
`provider_ignore_file_unsupported`. After the release-readiness commits moved
HEAD forward, the same audit correctly failed as stale until the index was
refreshed. Re-running with the explicit local provider path
`go run ./cmd/entire-brain refresh index --sem-binary "$HOME/.local/bin/entire" --force`
indexed 135 files, 2,291 symbols, and 15,490 relations; the subsequent
`semantic-audit --json --fail-on release` passed with freshness `ok`, zero blind
spots, and the same provider ignore-file warning. This proves local coverage and
audit health after refresh, not agent usefulness.

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

Positive replay-lab history evidence retained: the committed
`release-entire-brain-schema-contract` panel generated
`release-candidate-entire-brain-schema-contract-20260610T0115Z` with one pinned
Codex runner, `no_brain` vs `full_brain`, and 4 repetitions per side. The
baseline failed all 4 reps while `full_brain` passed all 4 reps on the
schema-contract history task, with mean score `66.0 -> 91.5`, pass rate
`0.0 -> 1.0`, and summary `proof_ready=true` /
`brain_positive_stable`. The independent release audit retained under
`benchmarks/agent-brain/evidence/release` reports 1 suite, 8 records, 0 hard
flags, 8/8 provenance-backed records, and 1 proof-ready comparison. This is
citable evidence that the history/full-brain lane can improve one checkpointed
task; it is not evidence that semantic indexing or facts retrieval are broadly
better.

Semantic replay-lab pilot: the committed `release-entire-cli-semantic-xdg`
panel generated `release-candidate-entire-cli-semantic-xdg-20260610T0205Z` with
one pinned Codex runner, `no_brain` vs `semantic_brain`, and 4 repetitions per
side on a local `entire-cli` task. The audit was integrity-clean (0 hard flags,
8/8 provenance-backed records), but the task was saturated: both arms passed
4/4, semantic changed mean score only `96.0 -> 96.25`, added time/tokens, and
the summary reported `proof_ready=false` / `saturated/overhead_negative`. It was
not copied into `benchmarks/agent-brain/evidence/release`; a stronger semantic
task is still needed before semantic usefulness claims graduate.

Rejected semantic replay-lab candidate:
`release-candidate-entire-brain-semantic-audit-gate-20260610T0320Z` ran the
committed `release-entire-brain-semantic-audit-gate` panel with 4 repetitions
per side. The task breaks the semantic audit gate so it returns before emitting
JSON, then hides focused validation requiring JSON before a blind-spot gate
error. Both arms fixed the underlying focused tests; the initial no-brain
records failed only the harness output leak audit because generic benchmark
task-path text appeared in the agent transcript. The committed semantic release
tasks now hide `benchmarks/agent-brain` from agent worktrees, and release-panel
preflight rejects hidden-validation tasks without explicit canary
`leak_markers`; future semantic proof runs should therefore fail on actual
benchmark signal rather than harness-path exposure. Even without the leak
failure, this task should be treated as saturated rather than semantic-positive:
validation pass-rate was `1.0 -> 1.0`, mean score was `97.25 -> 96.5`, and
semantic added time/tokens (`71.7s -> 94.3s`, `281,062 -> 388,766.5`). It was
not copied into release evidence.

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

Fixture-backed contract coverage: `TestQMDAliasesAcrossRetrievalVerbs` exercises
the supported local surface across `search`, `query`, `vsearch`, `get`, and
`multi-get`, including `--format json`, `-n` / `--number`, JSON result shapes,
and missing-id reporting. `TestQMDUnsupportedFormatRejectedAcrossRetrievalVerbs`
now fails every supported retrieval verb on unsupported formats, so `csv`, `md`,
`xml`, and other QMD formats cannot quietly produce accidental partial support.

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
- This is a local supported-surface contract, not a pinned upstream QMD
  compatibility suite; release claims should say "QMD-inspired aliases" unless
  a fixture generated from upstream QMD behavior is added.

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
- Timed distill `--json` summaries are self-describing enough to compare
  `--jobs 1` and `--jobs N` artifacts without relying on external notes for
  agent/model/effort, job count, actual agent calls, or wall time.
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
  Missing task or brain-manifest hashes now keep comparisons smoke-only by
  forcing `release_claimable: false` even when a metric is significant.
- History/query eval arms now branch-filter session-derived history records,
  including legacy history records that need branch inference from their
  transcript path.
- Eval's `query` arm is read-only and lexical in the harness, avoiding accidental
  embedder calls and cache writes during baseline measurement.
- `eval-compare` rejects duplicate/mismatched task ids, skips recall where it is
  undefined, and populates `p_holm_threshold`.
- `facts eval --help` now shows `label_source` alongside `relevant` labels, so
  the copied example matches the loader's proof-label validation rules.
- Facts-vs-raw retained evidence now has a dedicated validator and `mise` task;
  the validator fails closed on proxy evidence, missing/mismatched hashes,
  missing retriever arms, and non-claimable comparison metrics.
- `--format json` now requests JSON error envelopes, matching successful output
  behavior.
- The supported QMD-inspired retrieval surface is now covered by local contract
  tests for happy-path aliases, JSON shapes, missing ids, and rejected
  unsupported formats.
- The semantic benchmark task uses a portable repo path and hides expected files
  and validation from the agent prompt.
- Benchmark validation now fails closed when a task defines zero validation
  commands, and the independent release auditor treats missing or empty
  validation results as hard flags. A proof-ready comparison cannot be backed by
  unvalidated records.
- Release-evidence audit mode now rejects retained records that leak local host
  paths from user homes or temp directories, and the retained proof lane has
  been redacted while preserving hashes, scores, validation, and verdicts.
- Release-panel preflight now requires candidate tasks to hide
  `benchmarks/agent-brain` from agent worktrees and to use explicit canary
  `leak_markers` whenever validation is hidden, preventing otherwise-good
  semantic runs from being disqualified by exposed benchmark scaffolding.
- Release-evidence audit mode now requires every retained release-candidate
  suite to carry at least one proof-ready comparison, so one passing history
  suite cannot mask a later weak semantic or facts suite. `mise run
  release:evidence` is also a pure check; report regeneration is explicit via
  `mise run release:evidence:update`.
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

`mise run check` is the implementation health gate: formatting, vetting,
race-enabled tests, deterministic Phase 1 semantic tests, and cross-builds. It
is intentionally not the full release-claim gate because release claims also
depend on retained evidence artifacts.

`mise run release:readiness` is the local release-claim gate. It runs
`mise run check`, `mise run release:evidence`, `mise run distill:evidence`,
`mise run facts:evidence`, and `mise run semantic:evidence`. It is expected to
fail until the distill performance and facts-vs-raw manifests plus retained
artifacts exist, which keeps "distill is fast" and "facts beat raw sessions"
claims blocked instead of hidden behind a green implementation check.

CI now includes the retained replay-lab release-evidence audit in addition to
lint/build/test/Phase 1 semantic checks. The facts evidence gate is not wired
into CI yet because the citable facts-vs-raw manifest is intentionally absent.

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
  external wall-clock artifacts retained. These artifacts must pass
  `mise run distill:evidence` before a speedup claim is release-citable.
- Paired `entire brain facts eval --retriever facts|history|query|raw-sessions`
  runs over the same labeled or explicitly proxy-allowed task set, same task
  hash, and same brain manifest hash.
- Semantic benchmark evidence from a retained proof suite. A clean local
  `semantic-audit --json --fail-on release` run after refreshing the semantic
  index is recorded above, but it is regenerated local coverage evidence, not a
  retained usefulness proof by itself.
- Additional benchmark panels or retained proof subsets from
  `benchmarks/agent-brain`, audited with `audit_codex.py --fail-on-flags` and
  committed under `benchmarks/agent-brain/evidence/release` as sanitized
  evidence. One focused history proof is now retained; semantic/facts and
  broader multi-task replay evidence are still needed for broader claims.
- `mise run release:evidence` must pass before any replay-lab proof claim; it runs
  benchmark harness self-tests and then audits explicit `release-candidate-*`
  suites through `benchmarks/agent-brain/evidence/release/manifest.json`.
  Release mode rejects `release-local-*`, requires panel provenance, 4
  repetitions per side, zero hard flags, at least one proof-ready comparison
  overall, at least one proof-ready comparison per retained suite, and at least
  one proof-ready comparison for each manifest-required proof scope. The
  committed release lane currently requires the `history` scope only, so it
  cannot be cited as semantic or facts proof by aggregation. The check writes
  reports to a temp directory and compares them with committed reports; updating
  retained reports is explicit via `mise run release:evidence:update`.

Historical quarantine caveat: `benchmarks/agent-brain/results/codex-audit-report.md`
is quarantine/reference evidence, not release proof. That old results report
has hard integrity flags and zero provenance-complete agent records. Release
claims must cite only retained suites under `benchmarks/agent-brain/evidence/release`
that pass `python3 benchmarks/agent-brain/audit_codex.py --results
benchmarks/agent-brain/evidence/release --release-manifest
benchmarks/agent-brain/evidence/release/manifest.json --out-dir
benchmarks/agent-brain/evidence/release --fail-on-flags`.
The local `release-local-semantic-proof-20260609T2305Z` pilot is also not release
proof because its audited comparison was not proof-ready and the suite had hard
leak-audit flags. `release-local-*` names are intentionally outside the citable
release gate.

The local `release-candidate-entire-brain-history-20260610T000415Z` panel was a
stricter release-lane attempt from the committed
`release-entire-brain-history` panel. It ran 2 history tasks, 1 pinned Codex
runner, `no_brain` vs `full_brain`, and 4 repetitions per side. The brain arm
improved mean scores on both tasks (`78.25 -> 91.25` and `90.5 -> 93.5`), but
the summary still marked both comparisons `proof_ready=false` / `brain_negative`
because validation was not clean and overhead exceeded the release gate. The
independent audit rejected the suite with 0 proof-ready comparisons and 11 hard
`E:agent_leak_audit_failed` flags, so it was not copied into
`benchmarks/agent-brain/evidence/release` and is not release evidence.

The follow-up `release-candidate-entire-brain-history-20260610T0045Z` panel was
rerun after benchmark sanitizer hardening. It reduced hard leak flags from 11 to
3 and produced some clean full-brain records, but still failed the release gate:
0 proof-ready comparisons, 3 hard flags, and unstable validation across tasks.
It remains diagnostic only.
