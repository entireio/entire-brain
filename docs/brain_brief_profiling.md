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
- history index load and indexed rank;
- raw-history fallback totals plus one numeric row per executed query (at most
  eight), including files and bytes scanned, matches, truncation, and errors;
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
