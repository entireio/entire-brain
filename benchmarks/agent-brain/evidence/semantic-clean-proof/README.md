# Semantic Clean-Proof Evidence (post-B1)

This lane retains the first **clean** (post-B1) brain-positive benchmark proof and its
honest negative counterparts. It is deliberately separate from `evidence/release/`,
whose manifest stays in `no_release_claim` mode: the three release proof scopes
(history / mcp / mcp_radar_location_only) remain unproven, and nothing here changes that.

All runs use symptom-level `brain_queries` that pass `brain_query_leak_audit`
(no answer-bearing identifiers, hidden test names, or verbatim hidden-validation
phrases reach the brain arm), hidden behavioral validation, pinned
`base_commit`, and the pinned runner `claude:claude-opus-4-8:high`.

## Retained: `layer-b-confirm-fw16-20260611` — brain_positive_stable, proof_ready

Task `github-cli-format-web-conflict` (Layer B: github-cli, seed+semantic only,
`history_available=false`), `no_brain` vs `semantic_brain`, **16 repetitions per side**
(confirmatory run; effect first observed in a 1-rep pilot, sized in an 8-rep suite,
then confirmed here — all prior suites' results are reported below, none discarded):

- tokens: 988,429 → 540,512 (−45%), Welch p=0.0124, **Holm p=0.0495**, token win
  survives dropping the single most favourable repetition
- turns: 19.4 → 8.3 (−57%), p=0.0005, **Holm p=0.0030**
- agent seconds: 111 → 62 (−44%), p=0.0083, **Holm p=0.0414**
- pass rate: 15/16 → 16/16; composite score 84.75 → 85.94 (parity-plus)
- mean cost: $0.88 → $0.74 per run

Claim scope (narrow, honest): on this one project-native GitHub-CLI task, with this
model/effort, the **semantic_brain condition** — a prepared seed+semantic brain plus its
compact delivery packet (symptom-level hint phrases and stop-rule policy text) — delivers
a stable efficiency win at equal-or-better quality versus the unhinted baseline. This is
a CONDITION-level claim, not a retrieval-attribution claim: per-run instrumentation shows
the agent did not invoke brain CLI commands in any semantic rep (brain_commands=0), so the
win is attributable to the condition's packet (hints + policy + prepared context), and the
hint phrases reach the brain arm only. Those phrases are auditor-verified symptom-level
(not answer-bearing — `brain_query_leak_audit` passes), which is the B1 remediation bar,
but isolating pure retrieval value would require identical hints in both arms or retained
brain-usage verification. It is ONE task — not a layer-wide or release-wide claim.

Prior suites for the same task (same conditions, kept for the full sequential picture):
- `layer-b-pilot-20260611` (1 rep screening): no_brain 87 / semantic 86, tokens 1.30M → 0.50M
- `layer-b-proof-20260611` (4 reps): all metrics positive, tag `noisy` (n too small)
- `layer-b-proof-fw8-20260611` (8 reps): all metrics positive, turns raw p=0.0227, Holm-blocked

## Retained: `layer-hist-proof-20260611` — honest negatives (full-history harms here)

Three hard history-advantaged tasks on `entire-cli` (cli-bench, 1,812 real exported
sessions), `no_brain` vs `full_cli_compact`, 4 repetitions per side:

- `entireio-cli-condense-drops-latest-turn`: no_brain 4/4 vs brain 2/4 — both brain
  failures patched the wrong file; the history context misled the agent.
- `entireio-cli-finalize-redaction-raw-live`: no_brain 4/4 vs brain 2/4 — one brain
  failure patched the expected file but still failed the hidden test; the other made
  no change at all.
- `entireio-cli-uncommitted-filter-exact-compare`: 0/4 vs 0/4 — too hard for both arms;
  history did not rescue the brain arm.

Conclusion recorded as found: with this model, full transcript-history delivery on this
repo is net harmful or neutral; none of these comparisons are brain-positive. This
independently corroborates (at 24 runs) the B1 claim-demotion finding: the previously
claimed history-condition gains do not reproduce without the answer-bearing hints.

## Provenance

Per suite: `records.ndjson` (full per-run records with provenance hashes) and
`report.json` (the `run.py report` output with Welch/Holm p-values, CV, stability
tags). Full run directories (worktrees, agent stdout/stderr) remain in the local
ignored `results/` directories per the standing retention convention.
