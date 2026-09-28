# Selected-function residual coverage ledger

Audit date: 2026-09-18. This ledger preserves the frozen B–F function set and
its full statement denominators. The reviewed default/native race profiles cover
all implementation cases below. B is measured with `brain_cgo sqlite_fts5`;
setting only `CGO_ENABLED=1` does not activate the native vector lane. The
function totals below are the final local audit, not a replacement for final
CI coverage. The Linux-only deleted-working-directory case needs Linux CI.

Final whole-suite results and same-platform coverage comparisons are published
on [PR #278](https://github.com/entireio/entire-brain/pull/278) and
[trail #198](https://entire.io/gh/entireio/entire-brain/trails/198). CI artifacts
retain the complete statement profiles and environment metadata. Use
`go tool cover -func <combined.out>` with the frozen function lists in the plan
to inspect each function; no residual is removed from the denominator.

| Task | Frozen covered / total | Aggregate | Threshold result |
|---|---:|---:|---|
| B | 98 / 100 | 98.0% | passes 90%; remaining branches follow native invariants |
| C | 79 / 80 | 98.8% | passes 90%; only deferred-driver open invariant remains |
| D | 193 / 205 | 94.1% | passes 90%; remaining blocks are preflight-dominated |
| E | 109 / 113 | 96.5% | passes 90%; typed/path invariants plus Linux-only subprocess proof |
| F | 519 / 531 | 97.7% | passes 90%; remaining blocks are dominated or postcondition invariants |

Classification meanings: **missing** is reachable with a test fixture and keeps
the responsible task open; **fault fixture** needs a filesystem, SQLite, writer,
or race fault at a real boundary; **invariant** cannot occur from the function's
validated inputs without replacing a standard-library operation or changing
production code.

| Task | Function and uncovered source block | Class | Specific evidence / useful next test |
|---|---|---|---|
| B | `syncMemoryProjectionVectorsWithEmbedder` `memory_cmd.go:1228` | native-build invariant | In the applicable `brain_cgo` build, `historyVectorStoreFor` returns a store for every validated embedder model/dimension used by this function. The unavailable-store return belongs to builds without the native vector backend, where this frozen function is not measured. |
| B | same, `1277` | budget invariant | `historyAdded` is produced by `syncVectorsForKindsContextBatch` with `batchRecords` as its hard addition budget, so `batchRecords - historyAdded` cannot be negative. Zero and positive remaining-budget paths are covered. |
| C | `inspectPrivacyVectorIDsSnapshot` `session_privacy_verify.go:286` | platform/driver invariant | `sql.Open` for the registered SQLite driver and a syntactically valid read-only DSN defers filesystem/open failures until `Ping`/the first query. The next `quick_check` branch owns those failures, so this immediate `sql.Open` error is not induced by a database fixture. |
| D | `runWorkspacePatternsRefresh` `workspace_patterns.go:428` | preflight/race invariant | `captureWorkspaceDerivedReadPolicies` already resolves the same workspace directory at `workspace.go:2587` and captures its privacy policy. The second pure `workspaceDir` call can fail only if environment/path state changes between the two calls; there is no mutation boundary there. |
| D | same, `441` | preflight-dominated invariant | A malformed member corpus is rejected by the earlier derived-read policy capture, as proved by `TestWorkspaceCommandHandlesMissingAndMalformedMembers/malformed_member_corpus_fails_closed`. Reaching the inner loader error would require corrupting a member after policy capture while the subsequent privacy lock is being acquired; no deterministic boundary exists in this handler. |
| D | `runWorkspacePatternsList` `workspace_patterns.go:488`, `491`, `495` | preflight-dominated invariant | Policy capture already resolves the workspace and runs `requirePrivacyDerivedRead`. `TestWorkspaceCommandUnsafePublicationArtifactFailsClosed` proves an unsafe marker is rejected on the public list route before `validatePatternCorpusReadSafety`; a malformed aggregate corpus is likewise rejected by the derived gate before the checked loader. These inner returns require a TOCTOU replacement after capture. |
| D | `runWorkspacePatternsStatus` `workspace_patterns.go:544`, `547`, `559` | preflight-dominated invariant | The duplicate directory/safety checks are preceded by workspace policy capture, and malformed member derived state is rejected there. Existing unsafe-publication and malformed-member route tables prove fail-closed behavior without reaching these duplicate returns. |
| D | `runWorkspaceSkillsList` `workspace_patterns.go:699`, `702` | preflight-dominated invariant | When the manifest exists, policy capture already resolves the same directory and rejects the unsafe marker. The public skills route is covered by `TestWorkspaceCommandUnsafePublicationArtifactFailsClosed`; these duplicate returns need a between-check mutation. The manifest-missing path still resolves a deterministic valid workspace path. |
| D | `runWorkspaceSkillsForm` `workspace_patterns.go:744`, `747` | preflight-dominated invariant | Policy capture has already resolved the directory and rejected unsafe derived state. The existing six-route unsafe-publication table covers public fail-closed behavior; the inner checks require a TOCTOU mutation before the privacy-linearized mutation begins. |
| E | `runSemanticBoundary` `semantic.go:3541` | platform-specific behavioral test | The only remaining handler return is an isolated `Getwd` failure. A Linux subprocess removes its current directory and proves the public command returns no boundary output; Darwin and Windows skip this Linux-specific deleted-directory fixture. Local macOS coverage therefore does not hit this statement; the Linux subprocess test must pass in final CI. |
| E | same, `3571` | invariant | `json.MarshalIndent` receives only concrete `staleReport` and `semanticBoundaryResult` values containing JSON-supported fields, with no custom marshalers, channels, functions, or cycles. Typed command input cannot make serialization fail. |
| E | `appendWorktreePathContent` `semantic.go:2726`, `2730` | invariant | Both branches are inside `filepath.WalkDir` for descendants of `path = Join(repoDir, validated-clean)`. `filepath.Rel(repoDir, child)` cannot fail for two already-clean local paths, and a WalkDir-emitted descendant normalizes to a provider-relative path accepted by `validateSemanticProviderPath`. Symlinks and ignored paths have separate covered branches. |
| F | `runBrainBriefWithRawHistoryMatcher` `agent_surface.go:1523` | post-rebuild invariant | `rebuildHistoryFTSFromTruth` atomically creates the required schema and payload from the same verified `index` immediately passed to `rankHistoryViaFTS`. An `ok=false` result after successful rebuild requires replacement/corruption between the adjacent calls; there is no mutation seam, and ordinary SQLite/schema failures make rebuild return an error instead. |
| F | same, `1580-1597` | preflight-dominated invariant | Live `runBrainBriefWithRawHistoryMatcher` first loads the tombstone guard and requires clean derived state. A tombstone capable of making this inner predicate nonempty makes the retrieval privacy preflight fail before history ranking, as asserted by `TestBriefOutputFailuresAreExplicitAndDoNotLeak/dirty_privacy_state_blocks_derived_output`. The default raw-history guard itself is separately proved by `TestDefaultBrainBriefRawHistoryRespectsExclusion`. Reaching this inner mixed keep/drop loop would require bypassing the public preflight or mutating tombstones between the preflight and this block; no deterministic mutation boundary exists there. |
| F | same, `1806` | invariant | The non-profile packet serializer writes supported concrete report fields to an in-memory `bytes.Buffer`; it has no failing writer and no unsupported dynamic value. Packet-format validation occurs before this branch. |
| F | same, `1823`, `1827`, `1831` | invariant | These three branches all require that same in-memory serialization to fail on the profiled path. The typed report/format invariant above makes the error count, zero-output count, and return unreachable without a production injection hook. |

## Reconciled existing evidence

- `TestPrivacyVectorSnapshotFailsClosedDuringIteration` executes the late
  `rows.Err` path, and `TestPrivacyVectorSnapshotEnforcesScanCeiling` executes
  the 2,000,001-row ceiling. The focused CGO profile raises
  `inspectPrivacyVectorIDsSnapshot` from 22/25 in the supplied reviewed union to
  24/25; only the immediate `sql.Open` return remains.
- `TestPrivacyVerifyRootCommandTextJSONAndViolationError` covers the verifier
  error route at `session_privacy_verify.go:495`; `newPrivacyVerifyCommand` is
  23/23 in the current merged profile.
- The reviewed retention closure covers unresolved repository storage, corrupt
  apply state with committed-data digest preservation, and shared-copy caveats
  in text and JSON. `runPrivacyRetention` is 32/32 in
  `/tmp/v2-root-final-reviewed-cgo.out`; C is 79/80.
- Memory progress faults already proved by
  `TestMemoryVectorSyncInitialProgressWriteFailurePreventsResetMutation` and
  `TestMemoryVectorSyncFinalProgressWriteFailureKeepsCommittedVectorsRepairable`
  are not residuals. The distinct reset-completion save between those two
  commit points is covered by the new native reset-failure file below.
- `memory_vector_reset_failure_regression_cgo_test.go` adds native SQLite
  `BEFORE DELETE` aborts for history and conversation reset clears, proves
  `reset_complete=false`/pending state and exact unaffected-store IDs, drops the
  trigger, and verifies a complete retry. It also fails the reset-completion
  progress commit and combines sync plus worker-log failures. The native race
  profile raises B from 95/100 to 98/100; `runMemoryVectorLane` is 23/23.
- Brief privacy evidence already proves public fail-closed behavior and the raw
  fallback guard. The inner `1580-1597` block is recorded as preflight-dominated,
  not as an unimplemented privacy assertion.
- `workspace_pattern_failure_regression_test.go` now covers procedure/practice
  refresh-write failures, procedure/practice/run status decode failures, the
  positive member coverage counter, and a valid SQLite corpus whose absent
  `deep_dossiers` table makes skill form fail before provider execution. Focused
  default and native `brain_cgo sqlite_fts5` profiles raise D from 186/205 to
  193/205; every remaining D block in this ledger is preflight-dominated.
- `brief_failure_residual_regression_test.go` covers the 12-file combined cap,
  keyword-safe action attribution, and malformed legacy-pattern/theme failures
  through the public root route. Both failures return the structured command
  error envelope without task/packet fields. It also corrupts a real FTS payload
  while retaining canonical history, then proves the public brief returns the
  canonical match and repairs the derived row. Default and native race checks
  plus the legacy fallback below raise F from 502/531 to 513/531; the three
  selector helpers are fully covered.
- The same file forces a legacy source to rank with an unavailable FTS database
  and proves the profiled packet remains byte-exact through the in-memory
  fallback. Root's cache-flush fault test proves an exact fact packet, flush
  `ErrorCount=1`, unchanged fact digest, and successful repair, removing the
  cache-flush residual.
- The final FTS matrix profiles a canonical index-load failure, then holds a
  real `BEGIN IMMEDIATE` transaction after committing a corrupt derived row.
  The first brief returns the exact canonical match through lexical fallback;
  after releasing the lock, a retry returns the same match and repairs that
  record's `terms_json` to the canonical `null`. The fusion/impact suite adds a
  real second-hop SQLite scan failure while retaining direct semantic context,
  a profiled fusion index-load error, and exact indexed lexical survival. The
  merged selected count is 519/531; only the invariant and preflight-dominated
  rows above remain.
- `semantic_boundary_failure_regression_test.go` covers invalid repository
  storage discovery and missing stale-report source failures with zero output
  and preserved committed manifest bytes. Its isolated Linux subprocess also
  provides behavioral evidence for the otherwise platform-dependent `Getwd`
  failure. E is 109/113.

## Retained uncovered invariants

1. Preserve the F post-rebuild, privacy-preflight, and typed serialization
   invariants in the denominator; do not add production hooks solely to hit
   them. The combined semantic/FTS fallback is covered by final integration.
2. Preserve E's JSON and WalkDir invariants in the denominator. The Linux-only
   deleted-current-directory subprocess is behavioral evidence rather than a
   portable parent-profile hit.

Final integration closes the outer fusion fallback too: a fusion-eligible fake
rejects the query while the FTS path is an unusable directory. The public brief
must return the exact indexed-only history match through its in-memory scorer.
Together with the healthy-FTS case this distinguishes both fallback boundaries;
`agent_surface.go:1562` is covered, not classified as an invariant.

No reachable implementation case remains unimplemented in the frozen inventory.
Final acceptance still requires the complete CI matrix, the Linux-only fixture,
the 85% global gate, and the unchanged-denominator/nonregression checks.
