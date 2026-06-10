# Facts Eval Evidence

This directory is reserved for retained `entire brain facts eval` proof
artifacts. It is intentionally empty until a real paired eval run exists.

To become release-citable, a future `manifest.json` in this directory must name:

- four `facts eval --json` summaries for `facts`, `history`, `query`, and
  `raw-sessions`;
- one or more `facts eval-compare --json` outputs comparing those summaries;
- required claims whose metrics are significant, `release_claimable: true`, and
  backed by `evidence_basis: "proof_labels"`.

Run `mise run facts:evidence` after committing artifacts. The validator fails if
task hashes or brain-manifest hashes differ, proxy comparisons are used, task
sets are missing rows, or the required facts-vs-raw claim is not release
claimable.
