# Distill Performance Evidence

This directory is reserved for retained `entire brain distill` performance
artifacts from a large session repo. It is intentionally empty until a real
large-repo run exists.

To become release-citable, a future `manifest.json` in this directory must name:

- one `entire brain distill --dry-run --json` artifact captured before the timed
  runs;
- one serial timed `entire brain distill --json --jobs 1` artifact;
- one parallel timed `entire brain distill --json --jobs N` artifact from the
  same target repo/cache state, same agent/model/effort, same branch/force/chunk
  settings, and zero failed chunks;
- a `min_speedup` threshold greater than `1.0`.

Run `mise run distill:evidence` after committing artifacts. The validator fails
if the run configs differ, the serial/parallel output summaries differ, the
parallel run uses only one effective job, or the observed `total_seconds`
speedup does not meet the manifest threshold.
