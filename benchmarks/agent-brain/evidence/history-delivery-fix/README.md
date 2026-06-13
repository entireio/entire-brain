# History-delivery fix — evidence

## The problem (measured, tonight)

The brain's `full_cli_compact` history delivery was **net-harmful** on cli-bench: the
compact policy made the agent blindly trust the brief's `likely_edit_files` pointer and
"do not broaden," so when that pointer was a lexical false positive (e.g. `pickLatestVersion`
matching the word "latest"), the agent confidently edited the wrong file. Prior result:
condense 2/4 vs 4/4, redaction 2/4 vs 4/4 (brain worse than no-brain).

Two bugs were behind it:
1. **Delivery shape** — blind-trust packet with no step to verify the pointer.
2. **Prep drift** — `brain_prep_commands` called `entire-brain export`, but main moved that
   under `refresh sessions` (PR #40), so *every* history-condition run had been failing at
   prep with `unknown command export`. (This silently broke the history benchmark entirely.)

## The fix

- Prep: `export` → `refresh sessions` (same flags).
- Delivery: both compact CLI paths (Opus `--limit 2`, generic `--limit 4`) rewritten into a
  self-correcting two-step (the CLI analogue of the disciplined-MCP shape that already won):
  name the broken invariant from the brief's history hits, treat `likely_edit_files[0]` as a
  **candidate to verify**, and let the invariant — not the lexical ranking — decide the file.
  Verification is cheap when the pointer is already right (the candidate file is the one the
  agent must open to edit anyway) and a rescue when it is wrong. Anti-spiral discipline kept
  (one brief, ≤2 searches, one test, stop). `capture_brief_packet` retains the brief packet
  per run for diagnosis.

  Note on scope: this edit only touches the `full_cli_compact` branches. The previously
  shipped seed+semantic win (`github-cli-format-web-conflict`, opus-4-8) ran on the
  **`semantic_brain`** condition, which is a different, **untouched** policy branch — so that
  win is insulated by code separation, not by anything in this change. `full_cli_compact`
  itself was never a shipped win; it appears only in the net-harmful "before" proof and this
  "after" proof.

## Result — `hist-fix-proof-v2-20260613` (no_brain vs full_cli_compact, opus-4-8 high, 4 reps/side)

| task | before | after (brain vs no_brain) | tokens |
|---|---|---|---|
| condense-drops-latest-turn | 2/4 vs 4/4 (harmful) | **3/4 vs 3/4 parity** | 6.79M vs 9.65M (**−30%**, raw p=0.036) |
| finalize-redaction-raw-live | 2/4 vs 4/4 (harmful) | **4/4 vs 4/4 parity** | 0.97M vs 2.27M (**−57%**, raw p=0.053; secs p=0.050) |
| uncommitted-filter-exact-compare | 0/4 vs 0/4 | 0/4 vs 0/4 (non-discriminating) | 0.93M vs 2.23M (−58%) |

**Honest reading:** the net-harm is **eliminated** on both discriminating tasks — history
delivery is now **parity quality at 30–57% lower tokens**. Precise on the statistics:

- **condense** token win raw p=0.036 (`<0.05`); **redaction** token raw p=0.053 (just over
  0.05) but its agent-seconds win is raw p=0.0499 (`<0.05`). So "raw-significant" holds for
  condense's tokens and redaction's seconds — not a blanket claim across every metric.
- None survive Holm correction across the 3-task family at n=4, so the stability gate
  correctly tags them `noisy`/`saturated`, **not** `brain_positive_stable` — the same pattern
  format-web-conflict followed before n=16 made it stable.
- condense reaching parity reflects **both** a brain-arm improvement (2/4→3/4 vs the "before"
  run) **and** run-to-run movement in the no_brain baseline (4/4→3/4); on this hard, noisy
  task the honest statement is "the brain arm is no longer worse," not "the brain arm lifted
  it." redaction is the cleaner case (brain 2/4→4/4, baseline steady at 4/4).
- **Instrumentation caveat:** for Claude/opus runs the harness's tool-activity counters
  (`search_calls`, `used_brain`, `direct_brain_cli_calls`) read 0 — they parse Codex's
  protocol-json, not Claude's stream-json — so the efficiency claim rests on **measured
  `total_tokens` + pass-rate**, which ARE recorded, not on a measured search-count reduction.
  "Verification is cheap" is a code-path argument plus the token outcome, not an instrumented
  search count.

This is an efficiency-at-parity result, not a correctness lift — stated as found. The
uncommitted-filter task is 0/4 on both arms and does not discriminate (retire or redesign). A
focused n=16 confirmation of the cleanest task (redaction) is the path to a
`brain_positive_stable` tag (see `hist-fix-redaction-n16-*`; run alone, Holm family = 1).

## Files
- `hist-fix-proof-v2-20260613/records.ndjson` — 24 per-run records (provenance-backed).
- `hist-fix-proof-v2-20260613/report.json` — stability gate output (Welch + Holm p-values, CV, tags).
