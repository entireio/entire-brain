# Large-repo distill performance evidence

Retained dry-run sizing and paired serial/parallel timed-run artifacts for
`entire-brain distill` on a **large** session corpus (the entireio/cli brain:
2080 sessions, 2104 chunks). This is the large-repo companion to the
current-repo evidence under `../distill-perf/`.

Run the gate with `mise run distill:large:evidence`; regenerate the committed
report with `mise run distill:large:evidence:update`.

## What this proves (and does not)

- **Proven:** the distill extraction scheduler parallelizes a large-repo
  backfill. `--jobs 1` vs `--jobs 4` on the same fresh corpus produces identical
  output (2102 facts, 2104 extraction calls, 1456 reconcile calls) with a
  measured wall-clock speedup of ~1.67x (auditor floor `min_speedup` = 1.5).
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
  only in path diversity: it spreads facts across a few dozen valid three-level
  taxonomy paths (`category.subcategory.<digest-leaf>`) so a large corpus
  exercises the per-chunk reconcile path while staying well under the
  50-facts-per-path reconcile scaling notice (which a fixed 9-path agent would
  trip at this scale). Its sha256 is pinned in `manifest.json` under
  `executed_agent` and is validated by `audit_distill_perf.py` (the auditor
  re-hashes the committed script and asserts every retained run command invokes
  exactly it), so the timing provenance is tamper-evident.
- `go-test-internal-cli-ollama-distill.jsonl` — the **shared** retained fake
  loopback Ollama go-test contract, reused verbatim from `../distill-perf/`. It
  certifies local-model wiring and no-egress safety, not model quality.

## Redactions

- `dry_run.brain_path` is redacted to a placeholder keeping the repo-key suffix.
- Third-party contributor branch labels are replaced with content-free
  `branch-NNNN` identifiers across all artifacts; `largest_sessions` transcript
  paths and session ids are replaced with placeholders (only size/chunk counts
  retained). Distill timing does not depend on branch identity.

## Known limitation in the shared Ollama contract

The reused `go-test-internal-cli-ollama-distill.jsonl` was captured at its
recorded `source_head` and names distill-specific loopback tests
(`TestExecOllamaDistillAgent*`, `TestLoopbackOnlyDialContext*`, etc.). The
loopback/no-egress *functionality* it certifies still exists in
`internal/cli/distill_cmd.go` (`execOllamaDistillAgent`, `isLoopbackHTTPURL`,
`loopbackOnlyDialContext`), but those named test functions have since been
removed from the tree. The auditor validates the retained artifact, not live
code, so this does not affect the gate — but the shared contract (used by both
the current-repo and large-repo lanes) should be refreshed against current test
names as part of release hardening.
