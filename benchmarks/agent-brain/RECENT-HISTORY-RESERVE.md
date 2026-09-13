# Reserve recent history in the experimental memory packet

The optional `--recent-history-reserve-percent` setting protects recent,
independently retrieved passages from being crowded out by facts and their older
sources. It changes only the canary's `facts_with_sources` packet composition.
The default is **0**, preserving the baseline. The first evaluated candidate is
**50**, chosen before its live reader results.

```sh
python benchmarks/agent-brain/memory_integrity.py prepare \
  --brain-bin bin/entire-brain --out /tmp/reserved-history-run \
  --budget-bytes 512 --recent-history-reserve-percent 50
```

On Windows use `bin/entire-brain.exe` and a fresh output directory under `$env:TEMP`.

## Packing policy

1. Apply the task's branch filter to the independently retrieved top-k history hits.
2. Order hits with valid timezone-aware timestamps newest first. Equal timestamps
   preserve retrieval rank. Missing, naive or invalid timestamps receive ordinary
   backfill priority instead of recent-history priority.
3. Protect whole passages until their serialized JSON reaches the reserve target.
   The final passage may cross that target but never the total packet byte limit.
   Skip passages that do not fit the total remaining capacity. Never truncate them.
4. Pack facts together with their branch-matching source passages into the remaining
   space, then backfill raw hits in their original retrieval order. Deduplicate IDs
   across reserved history, supporting sources and backfill. Unused reserve space
   is immediately available to the other groups.

The setting uses no answer labels, correction labels, text heuristics or unseen
source scans. It cannot protect a correction that the independent retrieval path
did not find. Recency is a packing priority, not a claim that newer text is true.

The reserve is a target, not a strict partition: whole-passage rounding can consume
more than half the budget. For example, a passage larger than half but smaller than
the full budget may be selected. The total UTF-8 JSON byte ceiling remains strict.
Older evidence is preserved when it fits, but a recent irrelevant hit can displace
useful old evidence under a tight budget. A small synthetic regression improvement
does not justify enabling this by default or shipping it in `brain brief`.

## Verification

Focused tests cover the observed correction failure, older facts that still fit,
source deduplication, branch isolation, unknown dates, timezone ordering, equal-date
ranking, oversized passages, invalid options and unchanged baseline/control arms.
The retrieval comparison also verifies that reserve=0 reproduces the earlier
request packets byte-for-byte, and all three control arms are unchanged by reserve=50.

## Live comparison results — 2026-09-13

The baseline and candidate ran in one shuffled schedule using `gpt-5.6-sol`, low
effort, Codex CLI 0.154.0. Each request had a fresh ephemeral context with tools
disabled, and the exact same instruction, choices and output schema policy.
All model event logs were checked for tool use and errors. The eight development
cases and their labels were unchanged. Each budget/arm/condition ran three times.
Identical request hashes shared a response only within a repetition.

| Arm | Baseline 4096 | Reserved 4096 | Baseline 512 | Reserved 512 |
|---|---:|---:|---:|---:|
| no_memory | 9/24 | 9/24 | 9/24 | 9/24 |
| raw_history | 24/24 | 24/24 | 24/24 | 24/24 |
| facts_only | 9/24 | 9/24 | 9/24 | 9/24 |
| facts_with_sources | 24/24 | 24/24 | 21/24 | 24/24 |

The comparison used **114 actual model calls**, yielding 384 scored rows. There were 3 paired incorrect-to-correct changes and 0 paired correct-to-incorrect changes.

At 512 bytes, the original correction case's combined packet was 396 bytes and
contained the old policy and stale claim. The reserve packet was 500 bytes and
contained both the old history and its later correction; the stale claim no longer
fit. It therefore preserves the old source while replacing the misleading claim
with the newer evidence. Complete source coverage increased from 7/8 to 8/8 at
512 bytes and remained 8/8 at 4096 bytes.

The no-memory arm abstains, and three of eight gold answers require abstention.
Facts in this corpus are deliberately corrupted; its facts-only score does not
estimate ordinary Brain fact accuracy. Raw history remains the strongest baseline:
the reserve candidate has not shown an advantage over it. The paired observations
are repetitions of eight cases, not independent new tasks. New real-world cases
with misleading recent hits and cross-session changes remain necessary before
making the reserve a product default.

## Artifacts and validation

The ignored `bin/memory-integrity-recent-reserve-20260913/` directory contains
`prepared/`, `readers/`, `paired-results.json`, the exact runner/source snapshots,
and an artifact hash manifest. `recent-history-reserve.patch` isolates the source
and test changes relative to the preceding experiment, whose source hash is
recorded. These are local, uncommitted results; no installed Brain store changed.

Twenty focused tests pass. Twelve score files were recomputed, all memory byte
ceilings checked, and every provider call verified successful with no tool events.
The archived zero-reserve packets also match the prior pilot byte-for-byte.

Reported usage: 979,335 input tokens (99,840 cached, included in input), 8,836 output tokens. Summed call latency: 1005.7 seconds with up to three concurrent calls; this is not wall time. No dollar cost was reported by the subscription CLI.

Follow-up: [adverse cases and real-session results](RECENT-HISTORY-ADVERSE-RESULTS.md) show regressions from the fixed reserve. Keep it opt-in.
