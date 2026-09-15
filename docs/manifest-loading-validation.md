# Manifest loading validation

Implementation base: `origin/main` at `4623fab909ebefdab35087dd682fde387917ec62`.

## Implementation

Canonical Brain manifests no longer use the auxiliary 16 MiB read ceiling or
the corresponding writer rejection. CLI loading, strict replacement checks,
status/doctor health inspection, and forced-refresh validation share a dedicated
file decoder. Agent initialization's legacy/manual manifest fallback also decodes
from its checked file without the setup-record ceiling.

Existing file-type, path, descriptor-identity and alias checks remain in their
respective readers. Normal reads tolerate unknown fields; replacements reject
fields they cannot round-trip. Schema versions 1–3 remain supported, and malformed,
truncated, trailing, and unsupported-version documents are rejected. Atomic
publication remains in place. No storage migration is introduced.

The audit also covered refresh, status, doctor, MCP/agent surfaces, history
publication, legacy identity publication, export, and bundle import. Archive
validation and auxiliary state readers retain their existing independent rules.

## Synthetic regression evidence

All fixture values are synthetic. Only sizes and aggregate structural counts
came from the verification helper.

| Fixture | Bytes | Branches | Sessions | Warnings |
|---|---:|---:|---:|---:|
| Session-heavy | 18,569,433 | 870 | 5,305 | 111 |
| Warning-heavy | 17,220,291 | 389 | 2,633 | 6,238 |
| Larger synthetic | 83,886,080 | 1,800 | 20,000 | 12,000 |

Fixtures retain the production duplication between top-level session aliases and
`sources.sessions`. They use five or six source sections and distribute synthetic
metadata across session summaries to reach the target sizes. Round trips assert
all decoded fields, including all records, remain equal.

Tests cover:

- Production writer → normal reader, strict reader, and doctor health reader.
- Production writer → coordinated Graph/Brain preview and instruction installation
  through the no-setup-record fallback.
- A 50,000-record retained seed-coverage manifest written and read back intact.
- Failed serialization preserving the previous oversized manifest byte-for-byte.
- Unknown-field compatibility, strict refusal to erase unknown data, and checked
  forced-refresh validation on a synthetic oversized manifest.
- Normal versions 1–3, missing/negative/newer versions, malformed/truncated JSON,
  trailing JSON and trailing garbage.
- Directory, symlink, hardlink and regular-file-to-FIFO-swap rejection, plus the
  existing shared state-reader filesystem regression suite.
- Unchanged limits for unrelated bounded reads and agent setup records.

Executed successfully:

- `go test ./internal/cli ./internal/agentsetup -run 'Test.*(Manifest|Versioned|MemoryState|RepositoryBrain)' -count=1`
  (CLI 11.698s; agent setup 0.679s).
- `go test ./internal/cli -run 'TestBrainManifest' -count=1`
  after adding cross-package installation and large unknown-field coverage (6.783s).
- `go test ./...` passed (CLI 243.188s; all other packages passed or were cached).
  The initial full run caught the new dedicated decoder missing from the
  architecture-test registry; its strict-rewrite/tolerant-read contract is now
  explicitly registered, and the full rerun passed.
- Candidate binary build and `git diff --check`.

## Memory and limitations

Decoding from the file removes the arbitrary admission ceiling, not the memory
cost. Go's decoder buffers the JSON value and materializes the manifest. Schema
and body validation use multiple passes over the same descriptor; unknown fields
can add a tolerant retry. The writer still encodes the full document before atomic
replacement. Memory and CPU requirements grow with data size. No explicit resource
budget or recoverable out-of-memory guarantee is added. Other input ceilings and
existing seed projection policies are unchanged.

Retained manifest loading requires neither a source checkout nor Git. Operations
that rebuild source-derived indexes still require their corresponding sources.

## Sanitized manifest cohort validation

Validation used locally built candidate binaries; installed binaries were not updated. Entire tracking remained enabled. Only opaque repository IDs and aggregate findings are reported. No paid inference, watcher installation, checkpoint repair, publication, source reset, or retained-data deletion was performed.

### Existing manifests and normal readers

- Initial status opened all 20 existing manifests unchanged; both original oversized manifests were verified before refresh.
- Final status exit codes: {0: 20}. Doctor manifest checks: 20/20 OK. Final schemas: {3: 20}.
- Overall doctor exit codes: {1: 2, 0: 18}; these are separate from manifest loading.
- Coordinated candidate init-agents: {0: 20}; managed files changed: 0. The initial pass also succeeded on all 20 with zero changed files.

| ID | Original manifest bytes | Final manifest bytes | Schema | Deterministic worktree refresh | Seconds | Status | Doctor manifest | Overall doctor |
|---|---:|---:|---:|---|---:|---|---|---|
| R01 | 18,569,433 | 14,039,589 | 3 | blocked by independent input limit | 327.01 | pass | pass | exit 1 |
| R03 | 17,220,291 | 9,436,011 | 3 | completed | 1030.29 | pass | pass | exit 1 |
| R02 | 9,251,020 | 9,250,985 | 3 | completed | 385.88 | pass | pass | exit 0 |

### Refresh scope and remaining limitations

- Deterministic processing used --agent none and --worktree, local-only/no-egress settings, and an explicitly disabled external embedder.
- The first pass hit pre-existing dirty-worktree guards on 19 repositories. The two required large Brains and one already-running additional Brain were retried with --worktree. The other 17 full worktree refreshes were skipped once their regression read checks sufficed.
- R01 completed session export and seed generation but history rebuilding encountered the unchanged 268,435,456-byte canonical-transcript limit (memory_input_too_large). The full refresh therefore remains incomplete; this is not a manifest-loading failure. No workaround that weakens that unrelated limit or truncates retained data was applied.
- Post-refresh byte counts reflect normal production refresh writes. Support for the original larger inputs was established by opening their unchanged manifests first.

### Other doctor findings

- R01: memory_reconciliation.
- R03: five persisted failures from the earlier setup run (seed, history, docs, semantic, coordinator). Doctor replays those historical records, including their old size-cap reasons; the current full refresh succeeded and the current manifest check is OK. The setup records were preserved, not cleared.

### Checkpoint coverage warnings, separate from manifest failures

- Remaining top-level checkpoint-scope warning entries: 500. Prior cohort baseline: 2,403.
- Remaining top-level checkpoint-pointer warning entries: 5742. These counts classify top-level manifest warning entries by scope/pointer wording and do not count duplicated sources.sessions warnings again. They are not counts of unique missing checkpoints.

| ID | Scope warnings | Pointer warnings |
|---|---:|---:|
| R01 | 53 | 0 |
| R02 | 15 | 0 |
| R03 | 34 | 0 |
| R04 | 34 | 5361 |
| R05 | 1 | 0 |
| R09 | 1 | 0 |
| R13 | 360 | 381 |
| R14 | 1 | 0 |
| R20 | 1 | 0 |

### Retained-read probe

- Both oversized Brains passed explicit-directory status with no Git executable on PATH and an unavailable ambient source root, from outside a repository. Actual source directories were left untouched, so this checks absent ambient source/Git rather than physically hiding the recorded source directory.
- Real-cohort source code, transcript contents, identities, and raw diagnostics were kept out of the implementation report.
