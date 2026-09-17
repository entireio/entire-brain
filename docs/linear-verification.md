# Linear issue workflow verification

Implementation branch: `feat/linear-issue-memory`, based on mainline `34fb3e04`.
Verified on 2026-09-17 with Go 1.27.1 on Windows arm64.

## Live acceptance

The user selected Core (COR) → **Unified, Pluggable Agent Memory**. Linear's MCP
workspace/project UUIDs were resolved before importing. No Brain-owned token,
native Linear client, or background service was used.

- Imported the complete accessible project listing with archived access enabled:
  five existing active issues, their full descriptions and fields, the project
  description, and all comment pages (empty on those five issues). No closed
  issues were returned. The import receipt fixes the requested 90-day window.
- An ordinary hybrid Brain query returned issue evidence with source URL,
  observation time, content hash, and immutable snapshot reference.
- Created the explicitly authorized sample
  [COR-1825](https://linear.app/entirehq/issue/COR-1825/sample-brain-acceptance-offline-linear-evidence-round-trip)
  in that project, after recording a pending local creation operation.
- Refetched/imported the sample issue and generated a cited `brief --issue COR-1825`.
- Recorded a pending comment operation, posted one test comment, confirmed it by
  re-fetching, recorded success, and imported the comment. Its UUID is
  `439ce604-a892-4469-8c8f-478a50a54fbc`. The MCP response omitted a comment
  permalink; evidence explicitly records that limitation and retains the parent
  issue's canonical URL. No transcript was posted.
- With no-egress enabled, an ordinary Brain query returned the confirmed comment.
  Brain's stdio MCP also passed an eight-request protocol check: initialize,
  tools/list, import, idempotent import replay, ordinary query, pinned brief,
  exact-snapshot get, and operation-status retrieval.

The local store contains eight current records: one project, six issues, and one
comment. Creation and comment receipts are both `succeeded`. The sample issue is
clearly labeled and remains available as a test fixture. The local acceptance
inputs/results are outside Git under the system temporary directory; remote
source text and Brain data are not committed to the repository.

## Automated checks

- `internal/issues` tests pass: replay and revision ordering, canonical CLI/MCP
  JSON identity, snapshot integrity, batch rejection without partial writes,
  interrupted pagination/resume, omitted comments, explicit older references,
  failed refresh coverage, removal/inaccessibility/moves, disconnect/purge,
  repository/workspace isolation, stale observations, concurrent writers, links,
  and pending/ambiguous/terminal operation histories.
- Issue integration tests pass for default retrieval, offline exact citations,
  FTS fallback, long-thread diversity, bounded excerpts, semantic capability
  errors/cache invalidation, CLI stdin/MCP parity, all brief packet formats,
  untrusted issue text, workspace citation qualification, and export exclusion.
- Existing affected retrieval, brief, workspace, MCP, documentation-contract,
  and doctor-clock tests pass. MCP schema fixtures were deliberately updated.
- The full Windows CLI test run exposed four introduced contract regressions;
  all were fixed and rechecked. The remaining **37 failing tests reproduced on
  an unchanged mainline checkout**. Most require Windows symlink privileges that
  this machine lacks; others exercise Unix absolute-path assumptions or Git's
  `NUL` handling. This is not a claim that the full Windows suite is green.
- Windows arm64, Linux amd64, and macOS arm64 builds pass. Linux/macOS runtime
  tests were not run here; WSL is not installed. The repository's existing CI
  runs platform builds and Linux/macOS race tests and discovers the new package.
- Graph impact analysis of `unifiedResult` completed on the implementation
  checkout before shared-contract edits, including workspace/transport consumers.
  The earlier `E_NO_GIT_HEAD` warning did not recur.

## Practical limits

Review follow-up verification (2026-09-17): regression tests now cover scope
changes during ranking, purge immediately before CLI/MCP/brief/workspace output,
and holding the write lock through the outer MCP frame. Same-revision refresh
tests cover completing truncated fields, preserving known-field conflicts and
old citations, and moving comments with their parent between selected projects.
Split-batch imports cannot complete before comments for every run member finish.
Large Unicode evidence is reconstructed through bounded MCP pages while a newer
revision is imported; the same continuation works through CLI multi-get and
workspace get. Storage tests and the affected retrieval, MCP, brief, workspace,
and documentation suites pass (excluding the known platform-dependent cases
described above).

The first release loads the issue manifest into memory and rewrites it atomically
on mutation. It does not provide background sync, hosted shared storage, automatic
fact extraction, or unbounded-scale indexing. Remote authorization, refreshing,
pagination, filter application, and conflict detection belong to the documented
host workflow. Receipts attest to observations rather than independent enforcement
of authorization. Issue source data stays outside the existing bundle/publish
allowlists; durable-fact anchors are unchanged.
