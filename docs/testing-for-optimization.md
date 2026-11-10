# Regression safety before optimization

Project tests and coverage tools use Go. The former Python tooling has been removed.

## Current checks

Run the full test suite:

```sh
go test ./... -count=1
```

Collect test coverage and check the compiled CLI:

```sh
go run ./tools/coverage -output /tmp/brain-coverage
go run ./tools/coverage -race -output /tmp/brain-race-coverage
```

Use an empty output directory for each run. The tool combines coverage by source location. It checks help, version, capabilities, and invalid commands in isolated directories.

Results include `environment.json`, `test-results.json`, `summary.json`, `tests.jsonl`, Go coverage profiles, CLI results, and `coverage.html`.
The environment records the commit, worktree status, Go version, build configuration, platform, race setting, and test scope.
The summary records statement coverage for each file and the complete profile, plus the profile's SHA-256 digest.
Test results count top-level tests and packages separately. Failed or incomplete runs retain diagnostic evidence and return a failing exit code.

Compare a new run with a previous `summary.json`:

```sh
go run ./tools/coverage -race -baseline /tmp/brain-baseline/summary.json -output /tmp/brain-comparison
```

Both runs must pass. Their toolchain, platform, build configuration, race setting, timeout, scope, and test group must match.
The command fails when coverage decreases by more than 0.01 percentage points. It retains the report and percentage-point difference.
Old Python reports need a new Go baseline because the report schema and measured source files changed.
CI publishes the reports for each platform. Baseline comparison remains an explicit option; CI does not select a baseline automatically.

CI runs the complete suite with race detection on Linux, macOS, and Windows. Windows CLI tests run in six independent groups. Other packages run once. Windows also runs `go vet -race ./...`. Each group has a 20-minute test timeout and a 45-minute job timeout. Linux and macOS retain 30-minute package timeouts and 60-minute job timeouts.

The Windows merger checks matching source revisions, build configurations, inventories, and exactly one result for every assigned test, example, and fuzz seed target.
Missing profiles, missing metadata, failed runs, or incomplete groups fail the required Windows check.
Merged reports retain the Windows execution environment, even when Linux performs the merge.
Executables remain outside temporary cleanup directories because detached Windows workers can retain them.
Package selection is not supported; use `go test` directly for a focused package run.

Before optimization, run the affected behavior tests. Compare benchmarks with the same Go version, platform, and workload. Coverage alone does not establish correct behavior.

## Mutation checks

Run `mise run test:mutations` or `go run ./tools/mutationcheck`. Use `-output /tmp/brain-mutations` to select an empty results directory.

The Go runner checks four deliberate defects: missing pattern publication, removed session exclusion, missing raw-history fallback, and disabled excluded-record detection.

Each named test must pass before mutation. A compiler overlay then substitutes a temporary source copy. The test must fail with its expected assertion. Build errors, skipped tests, unrelated failures, and timeouts do not count.

The runner leaves source files unchanged. Results include baseline logs, mutant logs, and `results.json`. CI retains these files. Required Linux and macOS checks depend on the mutation job. The local `mise run check` also includes mutation checks.

## Restored integration and publication checks

The required test jobs depend on real Brain–Graph integration. CI builds both binaries from fixed source revisions. Missing binaries fail that integration job.

Go tests cover Graph indexing, Brain code inspection, memory retrieval, combined activation, preserved user instructions, and repeated setup.
Guide comparisons cover normal and strict modes. The pinned Graph revision already carries the remember --path rule and no longer has the obsolete MCP sentences, but it still predates the COR-2308 memories-first wording (precedent anchors, history search as explicit escalation).
Only those exact differences are accepted; any other text difference fails integration.
Set `BRAIN_INTEGRATION_REQUIRE_GUIDE_PARITY=1` when validating a Graph build that also has that wording to require exact equality.
Removing this compatibility allowance requires the corresponding Graph correction and an updated CI revision pin.

Repository publication checks inspect tracked files and new, nonignored files. They reject the audited private identifiers and internal endpoints. The audited identifier rules match the former check.

Language layout tests use path strings in Go. They require no Python files or interpreter.

## Historical verification record (2026-09-17)

The following measurements describe the former tools and source revision. They are not current validation results.


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
matches must survive faster negative retrieval. The former benchmark runner has been removed. See the
[historical results archive](../benchmarks/agent-brain/README.md).

### Follow-up: pattern and skill regression gaps

Added deterministic ranking tests for relevance, strength, stable ties, repeated
words, limits and empty results. Root-command tests exercise accepted theme,
lesson and convention skill previews, missing verification and unavailable-agent
rejections without installations or decision writes. Provider fixtures are local
native executables, with temporary home directories and explicit repository scope.

Refresh tests cover text output matching the published source, corpus failures
without success output in either format, and missing transcripts preserving the
published pattern artifacts and manifest. The warning-output branch currently has
no producer: the episode builder never populates its warnings slice. No production
hook was added solely to cover that unreachable branch.

The combined focused suite passed with and without the race detector on macOS
arm64, Go 1.27.1; independent review also passed three consecutive focused runs.
Unioning the new non-race profile with the previous full default profile (unchanged
production code) gives the following cumulative statement coverage. This is not
a new full-suite run:

| Function | Before | After |
|---|---:|---:|
| `rankTaskRelevantPatterns` | 48.3% | 100.0% |
| `runPatternsSkillsForm` | 41.8% | 73.4% |
| `runPatternsRefresh` | 46.7% | 86.7% |
