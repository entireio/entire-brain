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

## Measured results

Apple M-series laptop, macOS, `syntax-only` profile, entire-graph
`v0.4.1-nightly.202609180615`, 8 fixed natural-language queries, 5 timed runs
each after one untimed warm-up. Every figure below came from running the command
above; nothing is estimated.

| Repository | kLOC | Files | Symbols | Index | KB / kLOC | Index vs source | Build | Throughput | Peak RSS |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| entire-api | 174 | 746 | 12.6k | 40 MB | 237 | 585% | 7.2s | 24k lines/s | 41 MB |
| entire.io | 485 | 1,835 | 29.1k | 103 MB | 218 | 655% | 22.7s | 21k lines/s | 48 MB |
| entiredb | 628 | 2,596 | 35.9k | 112 MB | 183 | 536% | 25.6s | 25k lines/s | 49 MB |
| entire-brain | 746 | 1,777 | 50.3k | 193 MB | 265 | 363% | 19.8s | 38k lines/s | 70 MB |
| **kubernetes** | **4,786** | **15,921** | **325.1k** | **1,171 MB** | **251** | **654%** | **219s** | **22k lines/s** | **162 MB** |

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

**Index size grows linearly with the codebase, at 183–265 KB per thousand
lines** — five to six times the size of the source it indexes. That ratio is
stable from 174k lines to 4.8M lines, which is the useful finding: there is no
sublinear behaviour to hope for.

**Query latency grows linearly too.** 299 ms at 174k lines, ~1.5 s at 746k,
**12.3 s at 4.8M**. Nothing flattens out.

Against the numbers competitors publish:

| | Corpus | Index | Search |
| --- | --- | --- | --- |
| Augment (published) | 100M lines | 250 MB | sub-200 ms |
| Zoekt (published) | ~2 GB, multi-repo | — | sub-50 ms |
| **Entire Brain (measured)** | **4.8M lines** | **1,171 MB** | **12,294 ms p50** |

At 2.5 KB per thousand lines, Augment's published index is roughly **100x
smaller per line** than ours, and its published search is roughly **60x faster
than ours on a corpus 20x smaller**.

Carrying our measured per-line cost to 100M lines gives an index around **24 GB**
and a search around **four minutes**. That is not a tuning problem.

**So: Brain does not scale to a 100M-line codebase today, it scales linearly in
both dimensions, and 4.8M lines is where we have actually measured it.** That is
a worse answer than "Unknown" reads, and a far more useful one — it is a number
to move rather than a question nobody had asked.

Those competitor figures are their published claims, not something we ran. They
are not measured here and should not be treated as verified.

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
There is no process-level cache between searches: repeated identical queries in
one process do not get faster, which is why the numbers track index size.

## Reproducing it

```sh
git clone <repo> && cd <repo>
entire brain bench scale --json > result.json
```

The worktree must be clean — the indexer refuses uncommitted content — and the
repository must be a git repository, since the line count comes from
`git ls-files`.

`--repeats` changes the timed runs per query, `--query` replaces the query set,
and `--keep` leaves the isolated store behind for inspection.

## Limits of this measurement

- **One machine, and one run per row except entire-brain, which was run twice
  and differed by 48%.** Treat differences below about 2x between adjacent rows
  as noise. The 40x spread from entire-api to kubernetes is not.
- **The largest repository measured is kubernetes at 4.8M lines.** Nothing
  beyond that is measured. The 100M-line figures above are arithmetic on our
  measured per-line cost, offered as a bound on what today's architecture could
  do — not a prediction, and not a run.
- **`syntax-only` profile.** Richer profiles produce more relations and a larger
  index.
- **Languages the provider cannot parse contribute lines but no symbols**, which
  is why symbol counts do not track kLOC evenly across rows.
- The kubernetes measurement required re-initialising a shallow clone, because
  the provider emits an empty commit for shallow repositories and the indexer
  rejects the snapshot. The file content is unchanged; only the git history is.
