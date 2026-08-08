# Development-only `brain brief` profile baseline

`profile_brief.py` measures the instrumented product retrieval path without
launching a coding agent or model. It is an unpaid product-development profile,
not confirmatory benchmark evidence and not a code-quality evaluation. Its
checked-in corpus and every report set both `confirmatory_eligible` and
`quality_eligible` to `false`.

## Frozen development corpus

`brief-profile-corpus-v1.json` selects checked-in task configs only:

- every `tasks/*.json` entry whose logical `repo` is `entire-brain` or
  `entire-cli`;
- every `mined-scale/entire-cli-scale-*.json` entry; and
- every `mined-scale/entire-db-scale-*.json` entry.

The 119 selected source configs are sorted by checked-in relative path and
deduplicated by `(logical repo, SHA-256 of the exact prompt UTF-8 bytes)`. The
lexicographically first checked-in path owns a duplicate. This produces 114
profile tasks: 25 `entire-brain`, 68 `entire-cli`, and 21 `entire-db`; five
duplicate source prompts are removed.

The corpus stores only the logical repo, checked-in relative config path, exact
config-file SHA-256, prompt SHA-256, and a derived task-identity SHA-256. It does
not store prompt text. Its self-hash covers the selection contract and all 114
entries. At startup the runner rebuilds the inventory from the checkout and
requires byte-for-byte equality with the committed corpus, then verifies the
config and prompt hashes again before retaining prompts in process memory. A
task edit therefore fails closed until the corpus is deliberately regenerated
and reviewed.

## Running

Build or select the exact `entire` binary to profile, and explicitly map all
three logical repos:

```sh
python3 benchmarks/agent-brain/profile_brief.py \
  --brain-bin /absolute/path/to/entire-brain \
  --repo entire-brain=/absolute/path/to/entire-brain \
  --repo entire-cli=/absolute/path/to/entire-cli \
  --repo entire-db=/absolute/path/to/entire-db \
  --output /private/path/brief-profile-report.json
```

`--brain-bin` is the standalone external-command binary that the host exposes as
`entire brain`. Each mapped checkout must have a readable current Brain
manifest. The runner captures the binary SHA-256, each repository HEAD, and each
Brain manifest SHA-256. To locate a manifest it captures
`entire-brain status --json` in memory, extracts the Brain directory, hashes
`manifest.json`, and discards both the status packet and path.

The full schedule is corpus order with exactly two adjacent invocations per
task:

1. `first_observation`
2. `immediate_repeat`

These labels describe order only. The runner never clears, copies, primes, or
otherwise edits private Brain state and makes no claim that the first
observation has a cold OS cache or that the repeat has a warm OS cache. For a
small runner smoke test, `--max-tasks N` executes the deterministic corpus
prefix and marks the report `complete_corpus: false`.

For every scheduled observation the command is:

```text
entire-brain brief <prompt> --json --profile-json <private-temporary-path>
```

The prompt is loaded in memory from its verified checked-in config. Stdout is
captured in memory and reduced to packet byte count and SHA-256. The sidecar is
read from a mode-private temporary directory, validated, reduced to its fixed
numeric/boolean shape, and deleted. The runner does not invoke agents, models,
validation commands, or holdout evaluation.

## Retained report and privacy boundary

A successful observation retains only:

- logical repo, task and prompt hashes, schedule label, and sequence;
- packet byte count and packet SHA-256;
- the exact schema-v1 sidecar numeric/boolean tree, with the fixed
  `nanoseconds` and `json` strings removed; and
- an observed fact-vector state derived from fact load count, vector-cache load
  count, fact embed calls, and query embed calls.

The vector state names are observations, not cache-completeness claims:
`no_facts`, `reranker_not_invoked`, `empty_or_unused`,
`loaded_without_fact_embedding`, `fact_embedding_from_empty_cache`, and
`fact_embedding_with_existing_cache`.

The sidecar validator accepts the exact schema-v1 key set only. It accepts only
nonnegative integers and booleans after checking the two fixed enum strings,
rejects duplicate keys, non-finite values, and integers outside signed 64-bit
range, recomputes raw-history query aggregates, checks embed-call/vector totals,
caps raw-history rows at eight, requires mode-private sidecar permissions, and
requires the sidecar packet byte count to match captured stdout. Any extra
field, arbitrary string, malformed count, unsafe sidecar file, or size mismatch
fails the observation without retaining the sidecar or packet.

A failed observation retains a fixed failure kind, return code when available,
stderr byte count, and stderr SHA-256. It never retains stderr or failed stdout.
Success stderr is discarded without retention. Reports contain no prompt,
packet, stderr, sidecar path, Brain path, repository path, environment value,
wall-clock timestamp, hostname, or user identity.

The report includes the full hashed schedule, per-repo aggregates split by
`first_observation`, `immediate_repeat`, and all observations, and a self-hash
over the complete report. Aggregate stages cover total brief, status, semantic,
history index/rank/raw fallback, facts load/cache/embed/rank/flush, synthesis,
knowledge, and packet serialization. The output is atomically replaced with
mode `0600`. The runner exits `2` after writing a report when any scheduled
observation failed; corpus or metadata preflight errors fail before execution.
