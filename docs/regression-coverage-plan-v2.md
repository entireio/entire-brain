# Regression coverage completion plan

Status: executing residual coverage queue (2026-09-18). This supersedes treating the
previous safety matrix's implemented rows as completion of the broader goal.
The goal is to make subsequent optimizations reviewable against behavioral
contracts, while closing the identified coverage gaps.

## Baseline and definition of done

Baseline: commit `32d2229a`, passing CI run
[35273669719](https://github.com/entireio/entire-brain/actions/runs/35273669719).
Go statement coverage: Linux default 81.65%, macOS default 81.70%, Windows
81.43%, Linux CGO 81.75%, macOS CGO 81.79%. Python benchmark coverage is
75.85% lines and 65.12% branches. Compare each platform/build with its own
baseline, keeping the measurement scope fixed.

Completion requires all of the following, not merely one commit per workstream:

- Every required behavioral case below has an assertion and passing evidence.
- Linux default Go statement coverage reaches at least 85%; the current
  47,109/57,693 baseline needs 1,931 additional covered statements. Other Go
  lanes must not regress against their matching baseline. Coverage increases
  must come from useful tests, not denominator reductions.
- The targeted command files `memory_cmd.go`, `workspace_patterns.go` and
  `session_privacy_verify.go` reach 80%, and the selected live functions intended
  for optimization reach 90% aggregate statement coverage. Freeze the selected
  function list and uncovered-block inventory before implementation.
- Every residual uncovered block in those selected functions is classified:
  reachable and still missing a test, platform-specific, or unreachable with
  evidence. A missing reachable case keeps its task open. Unreachable code
  remains in reported coverage; any target exception needs explicit agreement.
- Critical state-preservation, privacy and retrieval invariants each have a
  deliberate mutation or equivalent fault-injection proof that the assertion
  detects the broken behavior. Compilation failures do not count.
- Default/CGO checks and native Windows shards pass on the final pushed commit;
  CI coverage artifacts, review findings and worktree status are verified.
- Python runner/report cases below pass with synthetic fixtures. Existing
  quarantines remain separately visible and cannot be counted as passing tests.

No claim of 100% source coverage or universal regression safety is implied.
"100% complete" means every required task and acceptance gate above is met.

## Work ledger

All rows start **not started**. Each row will record owner, exact functions,
test names, before/after covered statements, validation commands, review result
and remaining cases. Statuses: not started, implementing, validating, reviewed,
complete, blocked. Blocked rows retain their unmet acceptance criteria.

| ID | Responsibility and required cases | Worker | Test-file ownership |
|---|---|---|---|
| A | Freeze baseline, map uncovered blocks to live entry points and existing fixtures; distinguish unreachable wrappers from missing integration tests | Main, assisted by Luna | Main owns this ledger and coverage metadata |
| B | Memory/vector synchronization: initial and incremental sync agree with full sync; unchanged inputs are idempotent; edits/deletions remove stale vectors; model/digest/epoch changes reset correctly; batch budgets span history and conversation stores; failures persist pending health state; cancellation, ownership takeover and retry preserve committed data and progress; reopen retains correctness. Extend reconciliation/maintenance failure cases to meet the file target | Sol | New memory/vector regression tests only |
| C | Privacy verification/retention: detect deliberately planted excluded data across relevant stores; clean state passes; malformed/unreadable stores fail closed; retention honors its selection boundaries and preserves unrelated data; routed JSON/text/error contracts | Sol after B, or a separate Sol slot | New privacy verification/retention tests only |
| D | Workspace commands: real multi-repository fixtures; refresh/list/status/verification/skill formation through live root routes; filters/order/limits; missing or corrupt member state; partial failures; no-egress and preview/rejection without writes; deterministic provider fixture | Terra | New workspace command regression tests only |
| E | Semantic indexing/graph: incremental/full and snapshot-backed query parity; worktree changes, ignored paths and symlinks; failed generation preserves prior readable state; predicates/directions/limits/empty results; supported runtime-trace queries; boundary command text/JSON/errors | Sol | New semantic indexing/graph regression tests only |
| F | Brief/agent output: live metadata and previous-response paths; missing/stale/malformed inputs; provenance, ordering, deduplication and budget limits; no hidden data leakage; assert final public output rather than only helper results | Terra | New brief-output regression tests only |
| G | Python benchmark tooling: synthetic runner success/failure/timeout and result accounting; malformed/incomplete report inputs; report output contracts; relevance-dataset edge cases; diagnose quarantined modules separately without replacing authentic evidence | Terra for orchestration, Luna for bounded pure cases | Separate new Python test modules |
| H | Re-measure full suites, assess residual gaps, close remaining live cases, review mutations, inspect native CI and trail findings, publish evidence | Main; independent Sol review | Main owns shared tooling/docs and integration |

Files with substantial uncovered code, including `semantic.go`,
`agent_surface.go`, `workspace.go` and `export.go`, form the measured follow-up
queue if B–G leave the 85% gate unmet. Main selects reachable, valuable cases
from the fresh report; it does not close H or lower the target to fit the work
already completed. New production-code fixes require separate diagnosis and
review; this plan does not authorize performance refactoring as test scaffolding.

## Concrete starting fixtures and case inventory

For B, reuse `memoryContextTestEmbedder`, `memoryVectorTestStore`,
`shortTermFixture` and the CGO ownership fixtures. Exercise the real worker lane
as well as the injected-embedder synchronization core. Include unset embedder,
missing history, privacy-epoch mismatch, shared batch budget, reset, persisted
failure and successful completion. Keep default and CGO expectations distinct.

For C, seed temporary SQLite ID tables with retained and excluded canaries, then
exercise the actual privacy verifier. Include malformed databases and missing
tables. Use privacy fixtures with root command storage resolution for verify
and retention: clean/dirty text and JSON, nonzero errors on violations, dry run
without writes, age/branch boundary selection, actual exclusion/purge, empty
plans and preservation of unrelated data. Do not hand-reimplement the retention
algorithm inside a test instead of invoking it.

For D, start from `twoRepoWorkspace`, `seedPromotableMember` and
`releaseGroupingAgent`, adding root-command storage wiring. Test refresh through
skills list/form as one lifecycle: legacy views are still consumed here. Include
an unreadable member alongside a healthy member, member coverage in status,
accepted/cached verification and missing/unaccepted dossier refusal. The listed
workspace runners are live commands, not obsolete wrappers.

For F, drive the real brief with tiny metadata/previous-response fixture files
and task queries that activate those paths. Assert selected files, provenance
and actions; merely invoking the scoring helpers misses the routing contract.

The initial five vector/privacy functions contain 180 statements with 19 covered
in the Linux default profile. Bringing them near 90% adds only about 145 covered
statements, so B/C are high-value safety work rather than sufficient progress
toward the entire 1,931-statement deficit.

Potentially uncalled wrappers, long deadline branches, very large scan ceilings
and driver-only error paths need explicit reachability/testability assessment.
An empty graph result or a textual search alone does not prove dead code. Reuse
existing cancellation, clocks and fault fixtures where possible; do not invent
automatic exceptions or long sleep-based tests to meet a percentage.

## Execution and review strategy

Main orchestrates, chooses fixtures and acceptance criteria, reviews every diff,
integrates and judges completion. At most three workers run alongside Main.
Use Luna for bounded deterministic matrices and report plumbing, Terra for
command integration and fixtures, and Sol for persistence, privacy, concurrency
and semantic correctness. No worker marks its own work finally complete.

1. Complete A, then run B and D in parallel. A third slot can prepare bounded
   Python cases or review completed work. Avoid overlapping production files.
2. Move into C, E and F as their dependencies and ownership permit. Reuse agents
   and existing fixtures; workers must not revert another worker's changes.
3. Finish G and the measured residual queue, then H. Keep behavioral coverage
   and global percentage progress visible as separate measures.

Before each substantive task use Brain context and Graph discovery. Inspect
neighbors for relationships and impact before any shared/exported code change.
Prefer real temporary SQLite/Git state, fixed clocks, fake local providers and
barriers over sleep-based synchronization. Isolate home directories and provider
paths. Do not add production hooks solely to execute unreachable branches.

Each task produces a small reviewed commit. Run focused tests first, then the
relevant race/CGO checks. Run full CI at integrated checkpoints, keeping the
existing eight Windows shards and one coverage union; never add a duplicate
unsharded Windows suite. Inspect the actual final-commit results before reporting
completion. Do not merge without a request to merge.

## Progress and time control

Use 60–90 minute implementation waves as checkpoints, not completion promises.
At each checkpoint Main reports closed cases/total required cases, uncovered
statements removed, critical cases still open, CI status, and the next bounded
batch. Re-estimate duration from actual throughput after the first wave.

If a worker stalls on fixtures or infrastructure for roughly 20 minutes, Main
reassesses the approach or reassigns the task. Infrastructure work is separately
accounted for and must not consume a whole wave without explanation. If targets
prove disproportionate or require a scope change, report the remaining cases
and tradeoff explicitly; never silently declare a smaller scope complete.

## Execution ledger — 2026-09-18

| Task | Current status | Evidence / outstanding work |
|---|---|---|
| A | Baseline frozen; reachability audit continuing | CI 35273669719 profiles retained; selected wave-1 functions below |
| B | Reviewed locally; final CI pending | Default development union 1129/1411 (80.01%); core/lane CGO aggregate >90%; core filesystem/SQLite failures, takeover, retry and bounded deadline tested |
| C | Reviewed locally; final CI pending | Initial selected 72/80 statements (90%); full default package run passed at 505/629 file statements (80.3%); membership mutation detected; later-row failure and ceiling tests added |
| D | Reviewed locally; integration validation pending | Frozen handlers 186/205 (90.7%); root lifecycle, partial/corrupt members, egress, rejection, output failures and limits |
| E | Reviewed locally; final residual audit pending | Changed-input full/incremental and snapshot/store parity pass; root default/CGO race checks pass |
| F | Reviewing — Main and Sol | Root output, provenance, privacy, malformed inputs and fusion covered; residual audit remains open |
| G | Validating — Main, Sol review | Five report and three membership tests pass; existing 28 runner accounting/timeout tests pass; independent review passed; final CI pending |
| H | Implementing — Main | Measured residual batches in progress; global target, final mutation checks and final CI remain open |

Frozen wave-1 optimization function set (full statement denominators, no
post-hoc exclusions):

- B: `syncMemoryProjectionVectorsWithEmbedder`, `runMemoryVectorLane`.
- C: `inspectPrivacyVectorIDsSnapshot`, `newPrivacyVerifyCommand`, `runPrivacyRetention`.
- D: `runWorkspacePatternsVerify`, `runWorkspacePatternsRefresh`,
  `runWorkspacePatternsList`, `runWorkspacePatternsStatus`,
  `runWorkspaceSkillsList`, `runWorkspaceSkillsForm`.

E's frozen function set: `runSemanticBoundary`, `appendWorktreePathContent`,
`applyCypherPredicate`, `runtimeTraceWhereSQL`. F will freeze its additional
function set before implementation starts. Native vector execution is measured
in the applicable CGO profile; default results remain separately visible.
Additional lifecycle tests support the memory file gate and do not change B's
frozen optimization-function denominator.
The G report tests use real file loading, filtering, aggregation and table output
with synthetic records. Plotting imports are replaced with non-drawing placeholders;
these tests do not establish chart-rendering coverage or correctness.

C residual audit: a constant-size recursive SQLite view exercises the 2,000,001-row
ceiling in about 1.2 seconds locally without storing those rows. A separate view
raises an integer-overflow error after a readable row to prove scan failures
cannot report a clean partial result. Both focused tests pass. The JSON violation
contract now parses stdout strictly, separately from stderr.

F frozen set (before implementation): `runBrainBrief`,
`runBrainBriefWithRawHistoryMatcher`, `brainBriefJSONProjection`,
`brainBriefLikelyFileGroupsForRepoAndFilenameCounts`,
`brainBriefMetadataStringActions`, `brainBriefMetadataStringActionsForFile`,
`brainBriefPreviousResponseActions`, `brainBriefPreviousResponseActionsForFile`.

Reviewed privacy/boundary/report batch committed and pushed as `8e6313ef`.
Focused privacy and boundary tests pass with the race detector in default and
`brain_cgo sqlite_fts5` configurations. These are focused checks, not final full
suite evidence. Additional live fact-GC and session-purge command contracts
are being implemented for the global coverage queue; the 85% gate remains open.

Wave checkpoint: the macOS baseline plus completed focused default profiles
covers 47,706/57,693 statements (82.69%), an increase of 571 over the macOS
baseline. This development union predates the latest memory and audit additions
and is not a fresh full-suite result. Workspace's file union is 419/462 (90.7%);
privacy verification's is 512/629 (81.4%). Global coverage remains below target.

E now tests changed HEAD/input (old symbol deleted, new symbol added) against a
fresh full rebuild, plus snapshot/store query parity. Graph runtime limits use
two matching records so ignoring the limit breaks the assertion. Selected E
helpers cover 68/70 statements; the boundary handler separately covers 90.7%.
The remaining helper returns concern `filepath.Rel` and path validation for
WalkDir-emitted descendants, which cannot fail under those path invariants.

Residual discovery found no callers for `loadCheckpointSnapshotFromGitDirPolicy`
in Graph, and a focused Go-source search found only its declaration. It is not
being exercised solely to increase coverage; its denominator is unchanged.

Memory review checkpoint: sync core CGO 94.8%, vector lane 91.3%. Added real
filesystem progress-write failures, SQLite mutation failure, forward-version
takeover, conversation-only embedding failure, and repair/retry proofs. A short
parent deadline exercises continuation without a 20-second wait. Main tightened
post-failure vector assertions to compare complete expected ID sets and extended
the deadline allowance for loaded CI. Focused CGO race validation passes (25.7s).

Additional reviewed residual batches cover vector and FTS health, deletion
attribution, hook recovery and handoff output. Their development union with the
previous profiles reaches 48,042/57,693 statements (83.27%); 998 more statements
would reach 85% on this denominator. This is not final CI coverage. The first
pushed v2 batch (`8e6313ef`) passed CI run 35290044560; the second batch
(`86dcbb28`) is running in 35290882149. Final H gates remain open.

Integration checkpoint: commits `86dcbb28` and `c26bb973` passed CI runs
35290882149 and 35291595987. Later reviewed changes cover agent-state path
containment, explicit refresh output, workspace retrieval graph/partial failures,
abstract enqueue retry state, semantic repair/reset and retrieval privacy rechecks.
The CGO race check of the abstract/maintenance/privacy batch passes (6.8s).

Trail findings exposed missing failure evidence in the Windows harness when a
failed test produced no valid coverage profile. Commit `5738831b` preserves the
original exit code, event stream and invocation metadata while still rejecting
invalid profiles from successful runs. The Python runner also records report
failures. All 38 Windows harness and 10 coverage tooling tests pass. The three
related trail findings are resolved with evidence; native Windows still runs
its eight shards and one union.

Current completed-profile development union: 48,452/57,693 (83.98%), 588
statements below 85% on this denominator. This is a macOS baseline plus focused
profiles, not fresh full-suite or Linux CI evidence. The queue now covers public
evaluation, lifecycle reports and visualization contracts, with further measured
workspace resolution cases. The original global gate remains unchanged.
