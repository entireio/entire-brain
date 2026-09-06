# Large-repo distill performance evidence

Retained dry-run sizing and paired serial/parallel timed-run artifacts for
`entire-brain distill` on a **large** session corpus (the entireio/cli brain:
2375 sessions, 2435 chunks). This is the large-repo companion to the
current-repo evidence under `../distill-perf/`.

Run the gate with `mise run distill:large:evidence`; regenerate the committed
report with `mise run distill:large:evidence:update`.

## What this proves (and does not)

- **Proven:** the distill extraction scheduler parallelizes a large-repo
  backfill. `--jobs 1` vs `--jobs 4` on the same fresh corpus produces identical
  output (2432 facts, 2435 extraction calls, 1520 reconcile calls) with a
  measured wall-clock speedup of ~1.71x (auditor floor `min_speedup` = 1.5).
- **Not proven here:** hosted-model end-to-end latency or fact quality. The
  three runs use a deterministic local command agent
  (`large_command_agent.py`), not a hosted model, exactly like the current-repo
  evidence. It exercises scheduler/extraction/reconcile/write mechanics and
  timing only. The frontend/hosted-model distill latency and fact-quality claim
  still needs its own retained artifacts and remains pending in the release
  matrix and press release.

## Provenance and methodology

- `dry-run.json` — offline `distill --dry-run --json` sizing (no agent call),
  read from the live entireio/cli brain.
- `jobs-1.json` / `jobs-4.json` — paired timed runs (`--jobs 1`, `--jobs 4`),
  each on its own fresh copy of the brain store with durable facts cleared
  beforehand, so timing reflects a from-scratch large-repo backfill
  (`cache_state: force_recomputed_each_run`, every run `--force`).
- `large_command_agent.py` — the deterministic offline agent actually executed
  for the three runs. It differs from the current-repo `local-command-agent.py`
  only in path diversity: it spreads facts across 144 valid three-level
  taxonomy paths (`category.subcategory.<digest-leaf>`) so a large corpus
  exercises the per-chunk reconcile path while staying well under the
  50-facts-per-path reconcile scaling notice (which a fixed 9-path agent would
  trip at this scale). Facts are stored per branch, so the constraint is set by
  the busiest branch rather than the corpus total; the earlier 36-path spread
  put `main` over 50 facts on some paths as the corpus grew, and the runs came
  back with reconcile scaling warnings the auditor rejects. Its sha256 is pinned in `manifest.json` under
  `executed_agent` and is validated by `audit_distill_perf.py` (the auditor
  re-hashes the committed script and asserts every retained run command invokes
  exactly it), so the timing provenance is tamper-evident.
- `go-test-internal-cli-ollama-distill.jsonl` — the **shared** retained fake
  loopback Ollama go-test contract, reused verbatim from `../distill-perf/`. It
  certifies local-model wiring and no-egress safety, not model quality.

## Redactions

- `dry_run.brain_path` is redacted to a placeholder keeping the repo-key suffix.
- One private cross-repository branch name is replaced with
  `redacted-private-branch`, identically across all three artifacts and in the
  transcript paths derived from it. The remaining labels are ordinary
  entireio/cli branch names and are retained verbatim. Distill timing does not
  depend on branch identity.

## The shared Ollama contract

The reused `go-test-internal-cli-ollama-distill.jsonl` names distill-specific
loopback tests (`TestExecOllamaDistillAgent*`, `TestLoopbackOnlyDialContext*`,
etc.). An earlier note here claimed those test functions had been removed from
the tree; they had not — all nine still live in
`internal/cli/distill_cmd_test.go`, and the artifact is re-recorded from a real
`go test -count=1 -json` run at the manifest's `source_head`, 9/9 passing.
