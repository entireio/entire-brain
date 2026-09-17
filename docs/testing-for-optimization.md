# Regression safety before optimization

This is the execution record for the test-safety plan started from
`34fb3e04a66f020dc90619aaadd9d45cb59595ad`. Production Go behavior is unchanged.
Coverage identifies missing execution; behavioral assertions and mutation checks
establish what a later optimization must preserve.

## Safety matrix

| Plan area | Implemented protection | Evidence command |
|---|---|---|
| Reproducible baseline | Cross-package Go statement coverage, real instrumented CLI process, build/commit identity, full test outcomes, HTML and JSON reports | `python3 -B tools/coverage/run.py --output /tmp/brain-coverage-default` |
| Retrieval and indexing | Incremental history agrees with full rebuild; ordered conversation results preserve source identity and provenance; brief retains raw-only matches; direct FTS agrees with JSON-backed results for positive, negative, selective, limited and kind-filtered queries | `go test ./internal/cli -run 'TestIncrementalHistoryMatchesFullRebuild\|TestRawHistoryFallbackRetainsFallbackOnlyMatch\|TestHistoryRetrievalEquivalentPaths' -count=1` |
| Persistence and privacy | Cancellation preserves committed corpus, sidecars and exact tombstones, and removes staging artifacts; stale derived data stays blocked; retries and reopened SQLite stores retain exclusions; publication and exclusion obey write-lock ordering | `go test ./internal/cli -run 'TestExcludedSessionSurvivesCancelledRefreshReopenAndRetry\|TestPatternPublicationLinearizesBeforeConcurrentExclusion' -count=1` |
| Command integration | Root-routed patterns refresh/list/status and filters; skills preview, repository installation, duplicate refusal, forced replacement and cached/no-egress verification; deterministic loopback shutdown | `go test ./internal/cli -run '^TestRootCommand' -count=1` |
| Visualization integration | Snapshot-backed graph/node/replay responses, stable ordering and deduplication, escaped source links, path containment, history privacy and summary redaction | `go test ./internal/cli -run '^TestViz' -count=1` |
| Regression detection | Three deliberate production mutations must trigger the intended assertion; baseline must pass, compilation errors never count | `python3 -B tools/coverage/mutation_check.py` |
| Optimization measurements | JSON-load and direct-payload retrieval share correctness-checked fixtures at 12 and 6,000 records; ordinary tests also enforce parity | `go test ./internal/cli -run '^$' -bench '^BenchmarkHistoryRetrievalEquivalentPaths$' -benchmem -count=5` |

The tests reuse the repository's existing fixtures, real temporary SQLite/Git
state where applicable, fixed clocks, and deterministic provider responses.
They do not require paid models. Concurrency uses barriers and bounded waits,
not a sleep to guess when an operation has reached a particular state.
Global publication-hook tests deliberately remain nonparallel.

## Coverage collection

Use an empty output directory for every run. Reports combine coverage blocks by
source location, so a function reached by several packages or by the binary is
counted once. The real binary checks help, version, capabilities, and invalid
command behavior in isolated user directories, including the absence of state
creation by these read-only commands. This does not instrument every child
binary built by every existing integration test.

```sh
python3 -B tools/coverage/run.py --output /tmp/brain-default
CGO_ENABLED=1 python3 -B tools/coverage/run.py \
  --tags 'brain_cgo sqlite_fts5' --output /tmp/brain-cgo
python3 -B tools/coverage/run.py --race --output /tmp/brain-default-race
python3 -B tools/coverage/run.py --output /tmp/brain-next \
  --baseline /tmp/brain-default/summary.json
```

The optional baseline comparison rejects different Go versions, platforms,
tags, race settings or scopes, and requires both runs to have passed. It fails
on a drop greater than 0.01 percentage points. An incompatible baseline must
not be silently accepted as a regression comparison.

CI collects artifacts in the existing Linux/macOS default and CGO race jobs.
A separate Windows non-race coverage job measures Windows-only source without
replacing the existing sharded Windows race checks. New reporter unit tests run
in lint. CI currently publishes measurements; automatic percentage gating
should use validated baselines for each platform/build, rather than reusing a
local macOS number across every configuration.

A separate Linux job runs all three mutation proofs, so stale mutation anchors
or tests that stop detecting their intended regression cannot silently pass CI.

Each output contains `environment.json`, `tests.jsonl`, `test-results.json`,
`summary.json`, raw profiles and `coverage.html`. Failed suites still produce
diagnostics and return a failing exit code; a coverage percentage does not
override the test result.

## Python benchmark tooling

With Python 3.14 and `coverage==7.16.1` installed, run:

```sh
GOTOOLCHAIN=go1.26.4 go version
python3 -B tools/coverage/python_run.py --output /tmp/brain-python-coverage
```

This measures the existing discovery runner, including subprocesses and branch
coverage, rather than maintaining another list of test modules. Reports exclude
test files and vendored source. Attempted quarantined and failing modules can
contribute coverage; `test-results.json` preserves those distinctions. This
scope differs from the initial exploratory measurement that also included
vendored code, scripts, and Windows CI tooling.

The three existing evidence-dependent quarantines and the host-pinned
ssh-keygen check are unchanged. They are not converted to passing tests,
silently skipped on a failing host, or fixed by rewriting protocol attestations.
Tests for algorithmic behavior should use synthetic evidence; actual release
attestation still requires its authentic inputs. Benchmark-tool optimization
must validate the relevant excluded module before treating that area as safe.

## Optimization entry criteria

Before changing an algorithm, identify its public behavior and run the relevant
safety matrix rows. Check Graph impact for the actual symbols to be changed.
Extend the fixtures for any newly affected path, run default/CGO and relevant
platform checks, and demonstrate that the intended assertion detects a broken
invariant. Run before/after benchmarks with the same build and workload, retain
allocations and output equivalence, and use repeated measurements rather than
a wall-clock threshold on a shared CI runner.

The initial package-local Go result was about 80.5%; the new cross-package plus
binary metric is broader and must not be presented as a pure test-improvement
delta. The earlier 85% overall, 80% low-covered command-file, and 90% selected
component targets remain provisional, not claims achieved by this change.
Neither those targets nor this matrix prove the whole repository regression-free.
Semantic fusion, large multi-repository workloads, platform-specific runtime
behavior and real provider journeys need their own fixtures before optimization.
The visualization assertions protect rendered behavior; they do not assert cache
hit rates. Performance claims require the separate measurements described above.

## Verification record (2026-09-17)

Local validation used macOS arm64 and Go 1.27.1. The final focused race suite,
all three mutation proofs, `go vet ./...`, eight coverage-tool tests and 27
Windows CI tooling/contract tests passed. The CLI test package also
cross-compiled for Windows; Windows runtime and hosted CI results remain pending.
An existing MCP framing fuzz target completed 56,957 executions in a bounded
10-second run without finding a failure.

Final full-suite runs, including instrumented binary contracts, both passed:

| Build | Covered statements | Coverage | Top-level tests passed / skipped / failed |
|---|---:|---:|---:|
| Default | 47,088 / 57,693 | 81.62% | 2,897 / 23 / 0 |
| `brain_cgo sqlite_fts5` | 47,357 / 57,952 | 81.72% | 2,917 / 22 / 0 |

Local artifacts are in `/tmp/brain-go-default-verified` and
`/tmp/brain-go-cgo-verified`; each contains the complete report and test log.
These full runs were non-race; the focused new-test race run passed separately.

The final default report measures `patterns_cmd.go` at 73.45%, `skill_cmd.go`
at 70.68%, `viz.go` at 80.93%, and `memory_cmd.go` at 69.24%. Next prioritize
memory lifecycle/error paths and remaining pattern/skill generation branches,
then the exact retrieval component selected for optimization. The six work
areas above are implemented, but the provisional percentage targets are not
complete. Broad optimization should wait for tests of its specific affected
paths rather than treating this work as blanket approval.

The Python benchmark run measured 75.55% line coverage and 65.31% branch coverage
in the first-party scope described above. Of 77 discovered modules, 73 passed,
three retained their existing quarantine, and
`confirmatory/test_negative_control_owner_approval_v1.py` failed its host-pinned
`ssh-keygen` identity check. The runner correctly returned failure and retained
the report. This is not a green Python baseline.

The correctness-backed retrieval benchmark ran successfully. Its two existing
paths measured approximately 22.0 ms / 14.8 MB allocated for JSON loading and
11.2 ms / 59 KB for direct payload retrieval in a short local sample. These
numbers establish a workload, not a new optimization or a performance guarantee;
use repeated measurements before drawing performance conclusions.

The existing [profiling guidance](brain_brief_profiling.md) records why raw-only
matches must survive faster negative retrieval. The existing
[benchmark runner](../benchmarks/agent-brain/ci_tests.py) is the source of truth
for host exclusions and quarantines.
