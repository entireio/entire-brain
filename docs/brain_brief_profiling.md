# `brain brief` profiling sidecar

`entire brain brief <task> --profile-json <path>` writes a measurement-only JSON
sidecar without changing the task packet. Profiling is opt-in and is not
available for `brief --handoff`.

The sidecar is schema version 1. It contains only fixed contract strings,
monotonic durations in nanoseconds, booleans, and aggregate counts. It never
contains task/query text, fact or session identifiers, result text, paths,
repository/user identity, environment values, or wall-clock timestamps.

The profile covers:

- total brief construction and packet serialization;
- status/live-build-state construction;
- semantic context, runtime trace, and test lookup;
- history index load (when the verified JSON fallback or semantic arm needs it)
  and indexed rank;
- raw-history fallback totals from the same single filesystem scan used without
  profiling, plus one numeric row per evaluated query (at most eight);
- facts load, vector-cache load, embedding calls, rank, and cache flush;
- likely-file and action-checklist synthesis;
- patterns, consolidations, and themes; and
- final serialized packet byte size and section counts.

Durations are measured from Go monotonic clock readings. `facts.rank` is
exclusive of time spent inside measured embedder calls, which is reported under
`facts.embed`; `facts.vector_cache_load` covers construction and loading of the
branch vector cache. Raw-history byte counts are bytes actually read through the
scanner, including scanner read-ahead. Packet section counts follow the selected
serialization format, so JSON-only sections are zero in a text profile.
On a fresh BM25-only history query, schema-v2 FTS hydrates its bounded result
window directly and `history.index_load.invoked` is false; its database work is
reported by `history.indexed_rank`. Semantic history fusion, legacy manifests,
and stale/corrupt derived caches retain the full JSON load path and therefore
report `history.index_load.invoked=true`.

The requested sidecar is encoded before any packet bytes are released, written
through an atomic same-directory replacement, and committed with mode `0600`.
If encoding or writing fails, the brief command fails and does not emit the
successful task packet. The total duration intentionally excludes sidecar
encoding/write time and stdout delivery, so the measurement does not fold its
own persistence cost into the product path.

Two existing graceful loaders constrain error attribution. Consolidation and
theme loaders intentionally collapse “absent” and internal read/query failure to
an empty result. Their profile stages therefore report invocation, duration, and
output count, but keep `error_count` at zero. Distinguishing those cases would
require changing their product behavior, so this instrumentation does not do
so. Likewise, a fatal status/build error produces no sidecar because there is no
complete packet/profile to commit.

The deterministic, unpaid development runner for this sidecar is documented in
`benchmarks/agent-brain/BRIEF-PROFILE-BASELINE.md`. It uses 114 deduplicated
checked-in task prompts, executes adjacent `first_observation` and
`immediate_repeat` profiles, and retains packet hashes plus numeric profiles
only. Those order labels make no OS-cache cold/warm claim, and the resulting
reports are explicitly ineligible for confirmatory or quality conclusions.

## Known raw-history fallback cost

The August 2026 reconciliation observed a zero-match brief that scanned about
392.7 MB of raw history and spent about 4.37 seconds in that fallback out of a
5.07-second total. This is a local, workload-specific observation rather than a
portable performance baseline, but it demonstrates that proving absence may
still require reading the full available raw-history corpus.

The planned product fix is to make a trustworthy negative result cheap through
index-coverage metadata or an equivalent negative-result path. If raw scanning
is still necessary, it should have an explicit work budget and surface a
degraded or truncated result instead of silently implying a complete negative.
Any optimization must retain fixtures proving that valid fallback-only matches
are not lost. This open work is tracked with the other current limitations in
`docs/semantic_brain_plan.md`.

Raw fallback profiles set `shared_scan: true`. Aggregate duration, file, byte,
and error counts describe physical work once. Query rows report candidate matches
and truncation; their duration, file, byte, and error fields are zero because the
scan is shared and those costs cannot be attributed to one query. Aggregate I/O
counts therefore are not sums of query rows in this mode. The older multi-scan
implementation remains only as a test and benchmark reference.
