# Scale: how big a brain gets, and how fast it answers

Brain's row in every large-codebase comparison read **Unknown**, in both
columns, because nothing had measured it. This is that measurement, and the
command that produces it.

```sh
entire brain bench scale [path] [--json]
```

It reports index size on disk and query latency against a repository's size in
lines — the terms those comparisons are stated in — building the index in an
isolated store so it neither reads nor disturbs the brain you actually use.

**The headline is not flattering, and that is the point of publishing it.**

## Historical measured results

Apple M-series laptop, macOS, `syntax-only` profile, entire-graph
`v0.4.1-nightly.202609180615`, 8 fixed natural-language queries, 5 timed runs
each after one untimed warm-up. Every figure below came from running the command
above in September 2026; nothing in the tables is extrapolated. These are
retained summaries from the original benchmark, not measurements of the current
branch. Raw per-query samples and exact corpus revisions were not retained here,
so these rows cannot be independently reproduced byte-for-byte. Re-run the
command to evaluate a current build.

| Repository | kLOC | Files | Symbols | Index | KB / kLOC | Index vs source | Build | Throughput |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| entire-api | 174 | 746 | 12.6k | 40 MB | 237 | 585% | 7.2s | 24k lines/s |
| entire.io | 485 | 1,835 | 29.1k | 103 MB | 218 | 655% | 22.7s | 21k lines/s |
| entiredb | 628 | 2,596 | 35.9k | 112 MB | 183 | 536% | 25.6s | 25k lines/s |
| entire-brain | 746 | 1,777 | 50.3k | 193 MB | 265 | 363% | 19.8s | 38k lines/s |
| **kubernetes** | **4,786** | **15,921** | **325.1k** | **1,171 MB** | **251** | **654%** | **219s** | **22k lines/s** |

**Peak memory is withdrawn from this table rather than restated.** The figures
published here were read from `RUSAGE_SELF`, which never includes a child
process — and indexing runs in an external provider, so they measured the
harness rather than the work. Re-measured on entire-brain with the corrected
reading, the peak was **203 MB against the 70 MB this table used to report**,
2.8x higher. The other rows are wrong in the same direction by an unknown
factor, and restating them would mean re-running every repository, so the column
is gone until someone does. `brain bench scale --json` reports the corrected
`max_rss_bytes` today.

Code-search latency, against the semantic index:

| Repository | kLOC | p50 | p95 | p99 |
| --- | ---: | ---: | ---: | ---: |
| entire-api | 174 | 299 ms | 359 ms | 569 ms |
| entire.io | 485 | 1,329 ms | 1,701 ms | 1,777 ms |
| entiredb | 628 | 1,407 ms | 1,982 ms | 2,102 ms |
| entire-brain | 746 | 1,315 / 1,945 ms | 1,433 / 2,122 ms | 1,443 / 2,160 ms |
| **kubernetes** | **4,786** | **12,294 ms** | **13,031 ms** | **13,043 ms** |

The two figures for entire-brain are two separate runs of the same command on
the same commit, 48% apart. **Run-to-run variance at this scale is large**, so
read the order of magnitude and not the digits; the gap between 174k lines and
4.8M lines is far outside it.

## What this says

In these five runs, index size per thousand counted lines ranged from
183–265 KB. The largest tested repository had a 1,171 MB index and a
12.3-second median code search. These observations motivate work on index
size and query cost; they do not establish a scaling law or a capacity limit.

**100M lines has not been measured.** Multiplying a per-line ratio from a
smaller repository is neither a measured result nor a bound on performance at
that size. Different languages, profiles, hardware, and index implementations
can change both size and latency. No competitor implementation was run under
this workload, so these results do not establish a comparative speed or size
advantage.

## What is and is not being measured

**`code_search` is the number that matters for this question.** It runs against
the semantic index, which is the store that grows with the codebase.

**`knowledge_*` is a different corpus.** Facts, history and docs grow with how
much a team records, not with how much code there is, so their latency says
nothing about codebase scale. The benchmark reports how many records that corpus
holds beside the timing, because a sub-millisecond number over an empty store is
not a latency for anything. The first draft of this benchmark measured only that
corpus, found 0.4 ms, and would have published it as a large-codebase search
figure.

**The corpus denominator excludes what is not the codebase**: `vendor/`,
`node_modules/`, `third_party/`, `testdata/`, and binary assets. Counting them
would inflate the denominator and make every ratio here look better.

**Latency excludes process start.** One untimed warm-up runs per query, so these
are steady-state figures for a live process, not `time entire brain search`.
Any caches populated by indexing or warm-up remain available to timed calls.
These figures must not be used as cold-process latency estimates.

**It does not exclude the subprocesses a search spawns.** Every search resolves
its repository and checks index freshness through `git`, so each timed sample
pays for several process spawns — around 30-50 ms here, measured with
`rev-parse` at 10 ms, `branch --show-current` at 7 ms and the worktree-dirty
check at 14 ms. That is a real cost a caller pays and it belongs in the figure,
but it is not retrieval, and at the small end of this table it is a visible
share of the total rather than a rounding error.

## Reproducing it

```sh
git clone <repo> && cd <repo>
entire brain bench scale --json > result.json
```

The worktree must be clean — the indexer refuses uncommitted content — and the
repository must be a git repository, since the line count comes from
`git ls-files`. `bench scale <path>` measures a repository other than the one
you are standing in; the numbers in this document were taken with the form
above, from inside each repository.

`--repeats` changes the timed runs per query, `--query` replaces the query set,
and `--keep` leaves the isolated store behind for inspection.

## Limits of this measurement

- **One machine, and one run per row except entire-brain, which was run twice
  and differed by 48%.** Treat differences below about 2x between adjacent rows
  as noise. The 40x spread from entire-api to kubernetes is not.
- **The largest repository measured is kubernetes at 4.8M lines.** Nothing
  beyond that is measured; no 100M-line performance conclusion follows.
- **`syntax-only` profile.** Richer profiles produce more relations and a larger
  index.
- **Languages the provider cannot parse contribute lines but no symbols**, which
  is why symbol counts do not track kLOC evenly across rows.
- The kubernetes measurement required re-initialising a shallow clone, because
  the provider emits an empty commit for shallow repositories and the indexer
  rejects the snapshot. The file content is unchanged; only the git history is.
