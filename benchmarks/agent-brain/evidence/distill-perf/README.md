# Distill Performance Evidence

This directory is reserved for retained `entire brain distill` performance
artifacts from a large session repo. It is intentionally empty until a real
large-repo run exists.

Evidence must be collected on the repo whose performance claim it supports, or
the release copy must scope the claim to the measured repo. Do not use fixture,
current-repo, or unrelated `cli-bench` artifacts to support a frontend 24h
distill claim.

To become release-citable, a future `manifest.json` in this directory must name:

- one `entire brain distill --dry-run --json` artifact captured before the timed
  runs;
- one serial timed `entire brain distill --json --jobs 1` artifact;
- one parallel timed `entire brain distill --json --jobs N` artifact from the
  same target repo/cache state, same agent/model/effort, same branch/force/chunk
  settings, and zero failed chunks;
- `artifact_sha256` entries for the three retained JSON artifacts;
- `commands` arrays with the exact retained command tokens for dry-run, serial,
  and parallel runs. Use an explicit `--agent`; do not rely on `auto`;
- a `min_speedup` threshold greater than `1.0`.

Run `mise run distill:evidence` after committing artifacts. The validator fails
if artifact hashes or command provenance do not match, the dry-run has missing
transcripts or warnings, branch totals do not add up, run configs differ,
cache-hit counts do not match, warnings or failed chunks are present,
serial/parallel output summaries differ, reconcile calls exceed the dry-run
upper bound, timing components exceed `total_seconds`, the parallel run uses
only one effective job, or the observed `total_seconds` speedup does not meet
the manifest threshold.
