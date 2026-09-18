# Full test-suite review

All **534 inventoried test files** were read in full: 444 Go files and 90 Python files/scripts. The inventory contains 174,954 lines, including platform-specific tests, fuzz and benchmark helpers, and the coordination test script. The earlier pattern-based screening did not satisfy this scope; this review replaces it.

The [per-file ledger](test-suite-review-20260918.json) records each file's review rationale, findings, final disposition and whether it changed. Root and Luna/Sol/Terra agents divided the files into disjoint assignments; the root reviewer reconciled all 534 entries, judged the findings and reviewed the edits. The ledger preserves original findings even when later inspection rejected them. Its original inventory line counts and later reread line counts are distinguished.

Compared with `38c32fcb`, this pass changes 59 test files, including removal of one obsolete file whose cases skipped after invalid repository IDs were rejected. There are 533 remaining test files. Existing hosted-client rejection tests retain that security contract.

The cleanup strengthens exact identities, error diagnostics, nonempty results and database setup checks; replaces assertions of assigned values; corrects names and comments that overstated coverage; and bounds process/channel waits with cleanup on failure. A rebuild test now invokes the command rather than its own stub, and bundle roundtrip verification performs a real query. Duplicate cases, redundant checks and unused helpers were removed where their behavior remained covered.

## Verification

Focused before/after profiles compare exact production blocks with unchanged denominators, not rounded percentages:

| Review group | Covered blocks before | After | Lost blocks |
|---|---:|---:|---:|
| Root retrieval/rendering and adjacent contracts | 5,530 | 5,530 | 0 |
| Procedure matching | 99 | 99 | 0 |
| Pattern contracts, default | 4,833 | 4,835 | 0 |
| Pattern contracts, native SQLite | 4,834 | 4,836 | 0 |
| Semantic contracts | 4,432 | 4,432 | 0 |
| Review/seed contracts | 1,130 | 1,186 | 0 |

These focused groups overlap and must not be added together. Default race checks passed for the root group; focused retrieval synchronization and validation checks also passed with `brain_cgo sqlite_fts5` and the race detector. The other changed packages' focused comparisons retained every production block. An initial native pattern run varied in `semantic_stream.go`; an identical rerun recovered the baseline blocks. This is recorded scheduling variance, not evidence that repeated runs should replace a failing coverage gate.

The authoritative full-platform coverage comparison and CI result are published on [PR 278](https://github.com/entireio/entire-brain/pull/278) and [trail 198](https://entire.io/gh/entireio/entire-brain/trails/198). The cleanup baseline is `35e524bd`, before both earlier cleanup passes; this exhaustive pass is also compared with `38c32fcb`. Production behavior, coverage exclusions, thresholds and Python quarantines are unchanged.

## Retained limitations

Full review does not mean every test needed editing or that all behavioral gaps disappeared. The ledger records why meaningful tests were retained. In particular:

- Optional shipped-model/provider integration tests still require those external capabilities; deterministic unit tests do not substitute for their integration contracts.
- Filesystem permission tests depend on actual host capabilities. The read-only directory test now verifies that writes are denied before claiming that scenario.
- Some lock-ordering tests retain observation windows, and the streaming-memory test retains a coarse heap bound. Bounded cleanup prevents indefinite waits but does not eliminate scheduling or runtime variance.
- Additional parse-error threshold boundary cases and arbitrary remote-name fixtures would extend coverage; the existing cases still test meaningful behavior.

This is complete file review and targeted cleanup, not a claim of 100% source coverage or a proof that every possible regression will be detected.
