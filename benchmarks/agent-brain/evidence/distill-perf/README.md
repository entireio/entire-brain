# Distill Performance Evidence

This directory contains retained `entire brain distill` performance artifacts.
The current retained manifest is scoped to this repository and a deterministic
local command agent. It proves the local extraction scheduler and evidence gate:
`--jobs 4` preserves the same output summary as `--jobs 1` while improving the
measured extraction-heavy wall time on a temporary copy of the current
`entire-brain` local brain.

Evidence must be collected on the repo whose performance claim it supports, or
the release copy must scope the claim to the measured repo. The committed
current-repo artifacts must not be reused to support a frontend/entire.io 24h
distill claim. That claim still needs retained dry-run and timed artifacts from
the target large session repo.

A release-citable `manifest.json` in this directory must name:

- `target.repo`, `target.repo_key`, `target.source_head`,
  `target.brain_manifest_sha256`, and `target.claim_scope`, so the measured repo
  and the release claim it supports are explicit and audit-visible;
- one `entire brain distill --dry-run --json` artifact captured before the timed
  runs;
- one serial timed `entire brain distill --json --jobs 1` artifact;
- one parallel timed `entire brain distill --json --jobs N` artifact from the
  same target repo/cache state, same agent/model/effort, same branch/force/
  max-chunk-bytes settings, and zero failed chunks;
- `artifact_sha256` entries for the three retained JSON artifacts;
- `commands` arrays with the exact retained command tokens for dry-run, serial,
  and parallel runs. Use an explicit `--agent`; do not rely on `auto`. Include
  the chosen `--max-chunk-bytes` so the chunking/call-count tradeoff is
  inspectable;
- `local_ollama_contract`, a retained non-cached `go test -json` artifact proving
  the fake loopback Ollama distill path sends the selected model to
  `/api/generate`, avoids the `ollama` PATH binary, and rejects non-loopback or
  redirected egress without requiring a real external model. This section must
  include the artifact hash, exact command, source head, source path list, and
  required test names;
- a `min_speedup` threshold greater than `1.0`.

For current retained evidence, `claim_scope` is intentionally narrow:
`current-repo local command-agent distill extraction scheduling speedup`. The
local command agent is committed as `local-command-agent.py`; it exercises
extraction, reconciliation, deterministic writes, and timing evidence without a
hosted model or network. It does not validate fact quality or hosted-model
latency. The retained `go-test-internal-cli-ollama-distill.jsonl` artifact is a
separate local-model contract proof: it exercises fake loopback Ollama with
`-count=1`, but it is not evidence that any specific Ollama model produces good
facts.

The retained timed pair is the MEDIAN of five back-to-back `--jobs 1`/`--jobs 4`
pairs collected on one machine, chosen by that rule before the runs were read.
The five spanned 2.5747x-2.6284x (median 2.5934x, stdev 0.026) against the
`min_speedup` floor of 1.25.

Run `mise run distill:evidence` after committing artifacts. The validator fails
if artifact hashes or command provenance do not match, the dry-run has missing
transcripts or warnings, branch totals do not add up, run configs differ,
cache-hit counts do not match, warnings or failed chunks are present,
serial/parallel output summaries differ, reconcile calls exceed the dry-run
upper bound, timing components exceed `total_seconds`, the parallel run uses
only one effective job, the retained command omits the artifact's
`max_chunk_bytes`, the retained fake-Ollama contract artifact is missing/stale or
lacks a required passing test, or the observed `total_seconds` speedup does not
meet the manifest threshold.
