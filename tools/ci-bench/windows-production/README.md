# Windows race-test sharding

Adapted from entire-graph PR #189, source revision
`fbec112800199d918a6e9f9a2c0e98ffc591dfa7`. This port retains Brain's race
detector and default pure-Go SQLite backend; it does not enable the `brain_cgo`
or `sqlite_fts5` build tags. `CGO_ENABLED=1` supports Go's race runtime.

The `test` workflow compiles `internal/cli` once with `go test -race -c`,
checks the executable's Go build information for `-race=true`, inventories
its runnable Test, Example, and Fuzz roots, and partitions them across eight
standard ephemeral Windows runners. Benchmarks are excluded. All remaining
Windows packages run once with `go test -race`; native Windows vet runs once
separately because direct test binaries bypass go test's automatic vet.
The existing cross-build matrix remains in place.

The final `test (windows-latest)` check retains the previous required-check
name and fails if any prepare, shard, non-heavy, vet, or verification job
fails or is skipped. The verifier checks exact root coverage, package/process
multiplicity, exit codes, race evidence, binary hashes, toolchain and commit
identity, and command-line limits. It requires all planned roots to run and
terminate successfully (including legitimate skips), and rejects duplicates
and omissions. The compiled inventory is authoritative for newly added tests.

`settings.json` is the tuning surface. The timeout remains 20 minutes per
package, shuffle remains off, and Go's default parallelism is preserved.
`historical-weights.json` intentionally starts empty: graph timings are not
Brain measurements. The deterministic planner initially balances root counts;
future weights should come from top-level terminal Elapsed values in retained,
successfully verified Brain Windows shard JSONL, recording the source commit
and run. No Brain speedup is claimed until native CI has been measured.

Every runner restores setup-go's module/build caches. The test-result cache
is cleared before execution to ensure tests actually run. Direct launches
set PWD to the package directory, prepend GOTOOLDIR to PATH, and preserve
`-test.paniconexit0`. The harness inventories target-selected TestMain
functions and rejects lifecycle drift until the settings are reviewed.
The CLI's pinned `TestMain` isolates XDG storage in a fresh temporary directory
for each process, runs `m.Run()` once, and propagates its exit code. It does not
filter tests or share state between shards; inventory-only launches also clean
up their temporary roots.
Before and after execution it verifies the tracked worktree and commit.

The separate `phase1-semantic` matrix retains its original race-test command,
`ENTIRE_BRAIN_PHASE1_CI=1`, and offline GOPROXY/GOSUMDB environment after module
priming. The Linux/macOS race and specialized CGO lanes are unchanged.

Validate locally:

```sh
python3 -B -m unittest discover -s tools/ci-bench/windows-production/tests -p 'test_*.py' -v
```

`prepare.py`, `run_shard.py`, and `run_other.py` require native Windows;
the planner, verifier, and harness regression tests run on any host with Go
and Python 3.12+. GitHub Actions retains the compiled bundle and all execution
and verification evidence for three days. Hosted Actions execution and
queue-inclusive latency still require a workflow run.

## Native Windows validation

The September 6, 2026 Azure VM run passed with Go 1.26.2 and Python 3.12.10:
27 harness tests, all eight shards, the remaining package suite, Windows vet,
and the coverage/integrity verifier. The CLI inventory accounted for all
1,718 roots: 1,700 passed and 18 skipped under existing platform, capability,
or opt-in guards. The verifier covered 12 packages in total.

The run took 813.527 seconds excluding bootstrap on a four-core
`Standard_D4s_v5` VM, with two shards running concurrently in isolated
worktrees. This validates native execution; it does not measure the latency
of eight separate hosted runners. The separate Phase 1 matrix was not rerun.

See [the evidence manifest](evidence/2026-09-06/manifest.json),
[execution results](evidence/2026-09-06/execution.json), and
[coverage verification](evidence/2026-09-06/verification.json). The manifest
binds the tested workflow and harness files to the immutable source snapshot.
