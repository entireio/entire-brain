# Facts Eval Evidence

This directory is reserved for retained `entire brain facts eval` proof
artifacts. It is intentionally empty until a real paired eval run exists.

To become release-citable, a future `manifest.json` in this directory must name:

- four `facts eval --json` summaries for `facts`, `history`, `query`, and
  `raw-sessions` (these canonical arms cannot be shortened in retained release
  evidence);
- one or more `facts eval-compare --json` outputs comparing those summaries;
- required claims whose metrics are significant, `release_claimable: true`, and
  backed by `evidence_basis: "proof_labels"`.

Minimum retained shape:

```json
{
  "schema": 1,
  "required_retrievers": ["facts", "history", "query", "raw-sessions"],
  "summaries": {
    "facts": "facts.json",
    "history": "history.json",
    "query": "query.json",
    "raw-sessions": "raw-sessions.json"
  },
  "comparisons": {
    "raw_vs_facts": "raw-vs-facts.compare.json"
  },
  "required_claims": [
    {
      "comparison": "raw_vs_facts",
      "metric": "useful_per_1k",
      "a_retriever": "raw-sessions",
      "b_retriever": "facts",
      "winner": "b",
      "evidence_basis": "proof_labels"
    }
  ]
}
```

Collection checklist:

- Before collecting, run `entire brain facts status --json` on the target repo.
  `facts_arm_ready` must be true for the facts retriever arm to be meaningful,
  but active facts alone are not proof; the paired eval still needs proof-grade
  labels and the retained artifacts below.
- Use one immutable task file for all four retrievers. Every summary must carry
  the same `run_config.tasks_sha256`.
- Use one immutable brain snapshot for all four retrievers. Every summary and
  comparison must carry the same `brain_manifest_sha256`.
- Use human-reviewed or otherwise proof-grade labels. Source-session proxy
  relevance is useful for calibration, but it must not be cited as a release
  proof that facts beat raw sessions. For required relevance claims, every
  retained result row on the compared sides must be `labeled: true` with
  `relevance_source: "explicit_label"` and `label_source: "human"` or
  `"judge_refined"`; the auditor cross-checks this instead of trusting the
  comparison JSON alone.
- Compare the same task ids on both sides. Missing tasks, task-hash overrides,
  brain-manifest overrides, and proxy-comparison overrides are rejected by the
  auditor.
- Keep the JSON artifacts inspectable and committed with the generated
  `facts-eval-audit-report.json` and `.md` after
  `mise run facts:evidence:update` passes.

Run `mise run facts:evidence` after committing artifacts. The validator fails if
any canonical retriever arm is omitted, task hashes or brain-manifest hashes
differ, proxy comparisons are used, task sets are missing rows, summary rows are
not proof-labeled, or the required facts-vs-raw claim is not release claimable.
