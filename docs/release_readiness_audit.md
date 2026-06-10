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
- The retained distill evidence now includes a non-cached fake-Ollama
  `go test -json` artifact for the local-model contract: the distill command sends
  the selected model to loopback `/api/generate`, requests non-streaming output,
  avoids the `ollama` PATH binary, ignores proxy/custom TLS hooks, and rejects
  non-loopback URLs or redirects without requiring a real external model.
- `benchmarks/agent-brain/audit_distill_perf.py` audits retained performance
  artifacts for release proof: one dry-run JSON, one timed `--jobs 1` summary,
  and one timed `--jobs N` summary. It requires matching agent/model/branch/force
  / max-chunk-bytes / confidence config, retained artifact hashes, retained command-token
  provenance with an explicit non-`auto` agent, target provenance
  (`repo`, `repo_key`, `source_head`, `brain_manifest_sha256`, and
  `claim_scope`), no missing transcripts, branch totals that add up, matching
  cache-hit counts, no warnings or failed chunks, bounded reconcile calls,
  timing components that fit under `total_seconds`, comparable chunk/call/output
  summaries, effective parallelism greater than 1, and observed `total_seconds`
  speedup above the manifest's `min_speedup`. It also requires the retained
  fake-Ollama contract artifact and all required loopback/no-egress tests to pass.
- `mise run distill:evidence` checks committed distill artifacts without
  rewriting them, while `distill:evidence:update` regenerates the audit report
  only after the validator passes. The current retained artifacts are scoped to
  this repository and a deterministic local command agent: they prove the
  extraction scheduler/evidence gate, not hosted-model quality or the
  frontend/entire.io 24h claim.

Claim policy: do not claim distill is "fast" until a real large-repo dry-run and
timed run show the call count and wall-time improvement.

Retained local scheduler evidence collected on this repo: the committed
`benchmarks/agent-brain/evidence/distill-perf/manifest.json` targets
`github.com/ashtom/entire-brain` at the retained measured head, uses a
deterministic local command agent, and passes `mise run distill:evidence`.
The retained dry-run reported 147 sessions, 7,419,024 preprocessed bytes, 159
scheduled extraction chunks, and 159 extraction calls. The paired timed summaries
showed `--jobs 1` and `--jobs 4` produced matching summaries with zero failed
chunks, and the observed wall-time speedup was about 2x. This validates the
parallel extraction scheduler and retained evidence gate, but it is not target
frontend performance proof. The same manifest also retains the fake loopback
Ollama contract test artifact, which supports local-model/Ollama wiring and
no-egress safety claims without supporting real-model fact quality.

Blocked evidence collection: the target large session repo and retained timed
run artifacts are still needed before any public frontend/large-repo performance
claim.

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
  artifacts for release proof: proof manifests require the four retriever
  summaries, matching non-empty task and brain-manifest hashes, no proxy/missing
  /mismatch overrides, empty missing-task sets, and required comparison metrics
  that are significant, `release_claimable: true`, and
  `evidence_basis: "proof_labels"`. The audit also requires top-level
  `release_pairing_ready: true`, a positive paired row count, and at least one
  required proof-label claim where the `facts` arm beats `raw-sessions`, so a
  proxy or unrelated comparison cannot be promoted into a facts-vs-raw claim by
  editing manifest text. It also supports an explicit
  `claim_policy: "no_release_claim"` manifest backed by `facts status --json`;
  that passes only when the facts arm is not ready and has zero active facts.
- The retained facts auditor now recomputes paired means, deltas, winners,
  two-sided paired t-test p-values, Holm thresholds, significance, and
  per-metric `release_claimable` from the retained summary rows before accepting
  a positive facts-vs-raw claim.
- `mise run facts:evidence` checks committed facts-eval artifacts without
  rewriting them, while `facts:evidence:update` regenerates the audit report
  only after the validator passes. The current retained artifact is no-claim
  evidence: it records that the current branch has zero active durable facts, so
  no facts-vs-raw win is being cited.

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
`query`, and `raw-sessions` over the same proof-labeled task set are still
required before any positive facts-vs-raw claim. Proxy-authorized/source-overlap
task sets are useful smoke evidence, but they are intentionally rejected by proof
manifests.

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

Historical clean semantic evidence collected on this repo after the provider
compatibility fix: `go run ./cmd/entire-brain refresh index --sem-binary entire --force`
indexed 134 files, 2,250 symbols, and 14,883 relations at `721aae0`; `go run
./cmd/entire-brain semantic-audit --json --fail-on release` passed with freshness
`ok`, worktree state `clean`, zero blind spots, and one retained warning:
`provider_ignore_file_unsupported`. After the release-readiness commits moved
HEAD forward, the same audit correctly failed as stale until the index was
refreshed. Re-running with the explicit local provider path
`go run ./cmd/entire-brain refresh index --sem-binary "$HOME/.local/bin/entire" --force`
indexed 137 files, 2,405 symbols, and 17,091 relations at `b430963`; the
subsequent `mise run semantic:evidence` passed with freshness `ok`, worktree
state `clean`, and zero blind spots. This proves local coverage and audit health
after refresh for those clean checkpoints, not agent usefulness. The final
release head must rerun `mise run semantic:evidence` on a clean checkout before
this is current release evidence.

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
resolved-branch reporting, and missing-id reporting.
`TestQMDBranchOverrideAcrossRetrievalVerbs` pins branch override behavior across
all five verbs, including wrong-branch misses. `TestQMDLimitAliasesAndPrecedence`
pins positive `--limit` handling plus mixed `--limit`/`-n` ordering.
`TestQMDHelpContractsForRetrievalVerbs` and
`TestQMDTopLevelHelpListsRetrievalVerbs` pin the help surface for supported
verbs and aliases, while
`TestQMDFormatCLIOverridesJSONAndReportsMissingIDs` pins human-readable output
and missing-id behavior. `TestQMDUnsupportedFormatRejectedAcrossRetrievalVerbs`
fails every supported retrieval verb on unsupported formats, so `csv`, `md`,
`xml`, `text`, and other QMD formats cannot quietly produce accidental partial
support. MCP coverage now also asserts the QMD-inspired retrieval tools are
advertised and callable as `brain_search`, `brain_vsearch`, `brain_query`,
`brain_get`, and `brain_multi_get`.

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
- `entire brain facts status --json` now reports active/durable fact counts,
  proposal counts, provenance anchor counts, and manifest facts source state
  without refreshing, exporting, or writing. This makes the facts-vs-raw
  blocker inspectable before an eval run; it is readiness telemetry, not proof
  that facts beat raw sessions.
- `--format json` now requests JSON error envelopes, matching successful output
  behavior.
- The supported QMD-inspired retrieval surface is now covered by local contract
  tests for happy-path aliases, help text, CLI/JSON output shapes, missing ids,
  and rejected unsupported formats.
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
- MCP replay-lab evidence and MCP/Radar tool-contract evidence are separate
  gates. Replay-lab release evidence keeps the still-valid agent A/B scopes
  `history`, generic `mcp`, and one retained `mcp_radar_location_only` Radar
  agent-lift suite. QMD-inspired MCP retrieval and Radar also have
  deterministic tool-contract evidence under
  `benchmarks/agent-brain/evidence/radar-tool`, checked by `mise run
  radar:evidence`.
- The Radar agent auditor remains strict for current retained proof and future
  panels: benchmark summaries carry additive `delivery_scope`/`env_flags`,
  Radar location-only delivery is counted as `mcp_radar_location_only` or
  `mcp_workspace_radar_location_only`, and proof-ready MCP-backed comparisons
  must be backed by their own MCP-verified condition records. The Radar agent
  auditor cross-checks `summary.json` pass rates and record counts against
  audited record rows, so a stale summary cannot become citable proof.
- MCP proof authenticity now has an explicit named-tool gate: the stdio server
  keeps the legacy `message: tools/call` count line and also emits redacted
  `tool: brain_*` plus `tool_result: brain_* ok|error` lines to the debug log
  after handled requests receive responses. New retained Radar agent runs must
  carry call counts, tool-name lines, safe boolean `tool_args` lines,
  `tool_result` lines, and structured safe-argument records. The independent
  auditor counts server-side named-tool records and completed named-tool records
  separately from basic MCP-verified records. Generic retained MCP-history proof
  still requires real `tools/call` lines before records count as MCP-verified,
  but older count-only logs remain legacy server-call proof rather than
  named-tool proof.
- MCP argument contracts now fail closed: schemas advertise
  `additionalProperties: false`, unknown keys are rejected, string fields and
  string-array fields must have the advertised type, and Radar MCP proof records
  preserve only safe boolean call details such as `location_only` and
  `include_deletions`. Deletion-shaped Radar tasks now carry
  `radar_include_deletions` in record provenance, and the independent auditor
  rejects retained Radar MCP records whose actual call omitted
  `include_deletions: true`.
- The committed `release-entire-cli-radar-mcp-review-base-scope` panel encodes
  `BENCH_RADAR_LOCATION_ONLY=1` in the panel manifest, so the fair radar run is
  reproducible and hashed with the panel config.
- The committed
  `release-entire-cli-workspace-radar-mcp-transcript-reresolve` panel exercises
  the workspace MCP lane (`no_brain` vs `mcp_workspace_radar`) with
  deletion-aware location-only radar. It is a calibration lane until a task with
  real no-brain headroom is identified.
- The committed `release-entire-cli-radar-mcp-attribution-realign` panel is a
  true-Radar calibration lane: it mutates a detector-shaped deleted state
  assignment in `RealignAttributionBase`, keeps Radar location-only, and validates
  the attribution-base state invariant without giving the agent expected/current
  values. A clean 1x pilot on 2026-06-10 saturated (`no_brain` 96/pass,
  `mcp_history` 87/pass, `mcp_radar_location_only`), so it is not release proof
  unless a future runner/task revision creates real baseline headroom.
- The committed `release-entire-cli-radar-mcp-review-file-count` panel is the
  next Radar proof candidate. It mutates the explicit-base review banner count
  range from `baseRef+"...HEAD"` to a hardcoded mainline range, uses
  location-only Radar, and hides a behavioral file-count validation that existing
  visible tests do not cover. Its first clean low-effort 1x pilot saturated when
  the prompt named the changed-file count directly, so the committed task now
  uses a symptom-level prompt while keeping precise Radar query terms hidden from
  the no-brain arm. A calibrated clean 1x pilot still saturated (`no_brain`
  94/pass, `mcp_history` 94/pass, `mcp_radar_location_only`), though Radar used
  fewer searches and roughly half the tokens; this remains directional
  efficiency calibration, not release proof.
- The committed
  `release-entire-cli-radar-mcp-manual-attribution-deletions` panel now backs
  the retained `release-candidate-cli-radar-mcp-del-20260610-r2` proof lane.
  Older clean runs without `tool_result` completion lines remain rejected, but
  the retained rerun is citable for focused location-only deletion Radar:
  no-brain passed 2/4, `mcp_history` passed 4/4, mean score improved
  `73.0 -> 92.5`, mean tokens dropped `811,466.5 -> 408,947.75`, mean search
  calls dropped `11.5 -> 6.0`, and the independent audit found 0 hard flags,
  8/8 provenance-backed records, 4 MCP-verified condition records, 4
  server-named `brain_regressions` records, and 4 completed `tool_result`
  records. It is not workspace Radar or broad multi-task proof.
- The committed `release-entire-cli-mcp-manual-attribution` panel targets a
  harder MCP-history proof lane on `cli-bench`: it regresses the manual-commit
  attribution-base invariant and requires `mcp_history` to surface the prior
  `RealignAttributionBase(newHead)` behavior without handing over hidden
  validation or expected text.
- Retained MCP evidence now includes
  `release-candidate-entire-cli-mcp-manual-attribution-20260610Tprogress` under
  `benchmarks/agent-brain/evidence/release`: no-brain passed 1/4, `mcp_history`
  passed 4/4, mean score improved `76.25 -> 92.0`, stability was
  `brain_positive_stable`, and the independent audit found 4 MCP-verified
  records with zero hard flags. This is citable as an MCP correctness/pass-rate
  proof, not named-tool MCP proof or efficiency proof, because its retained
  server logs are legacy count-only logs and the MCP arm used more mean time and
  tokens.
- Retained MCP/Radar tool-contract evidence now includes
  `benchmarks/agent-brain/evidence/radar-tool`: a hashed `go test -json`
  artifact over 49 focused `internal/cli` tests. It proves QMD-inspired MCP tool
  listing and local retrieval (`brain_search`, `brain_vsearch`, `brain_query`,
  `brain_get`, `brain_multi_get`), branch-scoped fact retrieval over MCP,
  changed-operand detection without per-file hint masking, location-only
  redaction, deletion opt-in, missing anchored call-site ranking, hinted
  assignment-deletion loci without suppressing missing files behind intact peers,
  same-name receiver methods, or adjacent same-function loci, strict argument
  validation, MCP framing, safe debug logging with `tool_result`,
  `brain_regressions`, `brain_workspace_regressions`,
  `brain_workspace_review`, workspace deletion redaction, and unsafe
  workspace-repo skipping. This is citable as deterministic local MCP/Radar tool
  behavior only, not as agent pass-rate lift.
- The query default-limit benchmark tasks now target the current unified
  retrieval implementation in `internal/cli/retrieve_cmd.go`, including the
  QMD-inspired `-n` alias, instead of the old semantic-query implementation.

## Commands And Evidence Status

Implementation checks reported for the release-readiness implementation pass on
2026-06-10:

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
`mise run check`, `mise run release:evidence`, `mise run radar:evidence`,
`mise run radar:agent-evidence`, `mise run distill:evidence`,
`mise run facts:evidence`, and `mise run semantic:evidence`, continuing after
individual failures and printing the complete failing-task summary at the end.
Distill and facts now have retained evidence, but their claim scopes are
deliberately narrow: distill proves current-repo local command-agent extraction
scheduling speedup plus fake loopback Ollama wiring/no-egress safety, and facts
records
`claim_policy: "no_release_claim"` because the current branch has no active
durable facts. These gates keep "distill is fast on the frontend repo"
and "facts beat raw sessions" blocked until target proof exists, instead of
hiding them behind a green implementation check. Semantic freshness is local
health evidence; semantic usefulness claims still need separate retained
benchmark proof before they should ship.

`mise run radar:screen` is the calibration check for local `pilot-radar-*`
suites plus promoted `release-candidate-*-radar-*` and
`release-candidate-*-workspace-radar-*` reruns. It first builds an independent
Codex audit and feeds that into the Radar report, so release-candidate rows show
whether the MCP server actually handled the named Radar tool and completed it.
This keeps a stale promotable pilot from hiding a failed promoted rerun. The
retained Radar release proof is
`release-candidate-cli-radar-mcp-del-20260610-r2`:
no-brain passed 2/4, `mcp_history` passed 4/4, mean score improved 73.0 ->
92.5, mean tokens dropped 811,466.5 -> 408,947.75, mean search calls dropped
11.5 -> 6.0, and the comparison is `proof_ready` with stability tag
`brain_positive_stable`. Independent audit found 0 hard flags, 8/8
provenance-backed records, 4 MCP-verified condition records, 4 server-named
Radar records, and 4 completed `tool_result` records. Earlier 2026-06-10
calibration runs that saturated, were noisy, or lacked named-tool/result proof
remain rejected. Suites that are stopped after an already-high no-brain score
are reported as `no-brain-too-easy`, and record-only suites without
`summary.json` are reported as `incomplete-suite`, instead of disappearing from
the screen report.
`mise run radar:evidence` now checks the retained deterministic MCP/Radar
tool-contract artifact instead of promoting those saturated agent panels. It
requires the retained `go test -json` artifact hash to match and all 49 required
MCP/Radar tests to pass, including branch-aware QMD-inspired retrieval tools and
Radar-specific MCP behavior, invariant-scoped related locations that distinguish
same-identifier assignment deletions with different RHS values, workspace
multi-locus deletion redaction, and safe success-path tool-result logging for
Radar and review tools.
`mise run radar:agent-evidence` checks the retained focused Radar agent-lift
suite and fails unless the release evidence contains a proof-ready Radar
comparison backed by MCP-verified, server-named, completed Radar tool calls.

`mise run release:evidence` regenerates the retained replay-lab Codex audit in a
temp directory before comparing it with committed
`codex-audit-report.{json,md}`. It now requires the retained
`mcp_radar_location_only` suite in addition to the history and generic MCP
scopes. `mise run radar:evidence` remains the deterministic tool-contract gate,
and `mise run radar:agent-evidence` remains the focused Radar agent-lift gate.

CI now includes race-enabled package tests, deterministic Phase 1 semantic tests,
the retained replay-lab release-evidence audit, focused Radar agent-lift audit,
deterministic Radar tool-contract audit, distill evidence audit, and facts
evidence audit in addition to lint/build checks. The facts evidence gate supports either
proof-required mode or explicit no-claim mode; the current no-claim manifest is
a guard against overclaiming, not a facts-quality win.

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
  agent/model/effort/max-chunk-bytes, with the dry-run JSON, manifest timing
  fields, and external wall-clock artifacts retained. The retained manifest must
  identify the measured repo and claim scope so the evidence cannot be reused
  for an unrelated frontend/large-repo claim. These artifacts must pass
  `mise run distill:evidence` before a speedup claim is release-citable.
- Paired `entire brain facts eval --retriever facts|history|query|raw-sessions`
  runs over the same proof-labeled task set, same task hash, and same brain
  manifest hash. Proxy-allowed runs remain smoke/calibration only. The committed
  no-claim facts manifest is a guard against overclaiming, not a substitute for
  this positive proof.
- Additional Regression Radar agent-lift evidence for other tasks/repos remains
  useful, especially workspace Radar. Future Radar agent claims still need 4
  repetitions per side, MCP-verified records, the appropriate proof scope
  (`mcp_radar_location_only` or `mcp_workspace_radar_location_only`), zero hard
  flags, a stable proof-ready comparison, deletion-aware MCP arguments when the
  task requests deletion signals, committed sanitized artifacts, and retained
  `mcp-server.log` files with server-backed named-tool plus `tool_result` proof.
- Semantic benchmark evidence from a retained proof suite. A clean local
  `semantic-audit --json --fail-on release` run after refreshing the semantic
  index is recorded above, but it is regenerated local coverage evidence, not a
  retained usefulness proof by itself.
- Additional benchmark panels or retained proof subsets from
  `benchmarks/agent-brain`, audited with `audit_codex.py --fail-on-flags` and
  committed under `benchmarks/agent-brain/evidence/release` as sanitized
  evidence. One focused history proof, one generic MCP-history proof, and one
  location-only Radar agent-lift proof are now retained; semantic/facts,
  workspace Radar, and broader multi-task replay evidence are still needed for
  broader claims.
- `mise run release:evidence` must pass before any replay-lab proof claim; it runs
  benchmark harness self-tests and then audits explicit `release-candidate-*`
  suites through `benchmarks/agent-brain/evidence/release/manifest.json`.
  Release mode rejects `release-local-*`, requires panel provenance, 4
  repetitions per side, zero hard flags, at least one proof-ready comparison
  overall, at least one proof-ready comparison per retained suite, and at least
  one proof-ready comparison for each manifest-required proof scope. Manifests
  can also require minimum counts for integrity-verified MCP datapoints and
  server-named MCP datapoints, and can require named-tool proof-ready comparisons
  for specific scopes. The committed release lane currently requires `history`,
  generic `mcp`, and one `mcp_radar_location_only` scope, so it cannot be cited
  as workspace Radar, semantic, or facts proof by aggregation. The check writes
  the Codex replay-lab report to
  a temp directory and compares it with the committed report; updating retained
  reports is explicit via
  `mise run release:evidence:update`.

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
