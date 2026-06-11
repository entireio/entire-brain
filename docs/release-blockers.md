# Release Blockers

A register of release-gating findings from the PR #33 review (2026-06-11),
kept next to the audit so a release decision can see what is open, what was
fixed, and exactly why. The standard for this file is the eval ledger's:
explicit claims, exact evidence, no euphemism.

## RESOLVED AS CLAIM DEMOTION — B1: Answer-bearing query hints invalidated the old replay-lab proof

**The claim under audit.** `docs/release_readiness_audit.md` previously treated
three replay-lab suites as citable proof that history/MCP/Radar context improved
agent outcomes without handing over hidden validation or expected text. That
attribution was unsupported.

**The confound.** `benchmarks/agent-brain/run.py` injects `brain_queries` into
brain/MCP-arm prompts only; the `no_brain` arm receives none. In the retained
pre-B1 suites, several `brain_queries` contained answer-bearing identifiers,
hidden-test names, or near-verbatim hidden-validation strings. The old
comparison therefore measured *(answer-bearing hint + brain)* versus *(no hint,
no brain)*, not brain value alone.

**Fixes landed.**

- `brain_query_leak_audit` now rejects answer-bearing `brain_queries` before a
  panel can run. It checks code-shaped identifiers and verbatim 3+-word phrases
  against hidden fix text and paths, expected files, hidden validation commands,
  expected-string greps, hidden test names, and validation fixture contents.
- All committed benchmark task queries that tripped the auditor were rewritten
  to symptom-level retrieval terms. The all-task sweep now reports zero flagged
  task files.
- The retained evidence manifest is stricter again: even in `no_release_claim`
  mode it requires zero hard integrity flags.
- The old confounded retained suites were removed from
  `benchmarks/agent-brain/evidence/release` and replaced with clean reruns.

**Clean rerun result (2026-06-11).** Item 3 is now complete: the three promoted
suites were rerun from a clean checkout with the sanitized task configs, four
repetitions per side, retained panel provenance, and host paths redacted before
commit. The independent audit now reports 3 suites, 24 records, 24/24 provenance
complete, 0 hard integrity flags, 8 MCP-verified datapoints, 4 completed
named-tool Radar datapoints, and 0 proof-ready comparisons.

| Clean retained suite | Outcome |
|---|---|
| `release-candidate-entire-brain-schema-contract-clean-20260611T0412Z` | Audit-clean but noisy: pass rate 50% -> 75%, mean score 86.5 -> 87.75, `proof_ready=false`. |
| `release-candidate-entire-cli-mcp-manual-attribution-clean-20260611T0412Z` | Audit-clean but saturated/negative: both arms passed 100%, mean score 94.25 -> 91.25, `proof_ready=false`. |
| `release-candidate-cli-radar-mcp-del-clean-20260611T0412Z` | Audit-clean but saturated/negative: both arms passed 100%, mean score 94.5 -> 90.5, `proof_ready=false`. |

**Current release implication.** B1 is resolved as an integrity/confound blocker:
the retained release lane no longer contains answer-bearing query-hint artifacts.
It is still `no_release_claim`, because the clean reruns did not produce a
proof-ready replay-lab lift. Any future agent-lift claim needs harder clean
retained tasks that survive the proof-ready gate; the old numbers stay removed
from the release story.

Operationalization note: the literal "any token" wording from the review is not
implemented because it would false-flag ordinary symptom text. The enforced rule
is code-shaped identifiers plus verbatim 3+-word phrases, with the known residual
that 1-2 word plain-English overlaps are not flagged.

## RESOLVED — fixed during review on this branch

**R1 — Eval label discipline orphaned the history-eval pipeline** (fixed in
`15e7aaa`). `validateEvalTask` rejected `history-eval-gen` output and every
retained task file; `eval-compare` hard-errored on its own default eval-gen
output. Fix separates ground-*truth* (same explicit labels, same source →
comparable) from evidence *grade* (human/judge-refined → claimable);
history-eval now declares `provenance_silver`, legacy files default to it,
and side-dependent truths (`source_match`, runtime judge) stay gated behind
`--allow-proxy-comparison`. Covered by a new gen→load→run→compare seam test.

**R2 — Retrieval panic on the loopback guard's own rejection path** (fixed in
`15e7aaa`). `configuredEmbedder` dereferenced the nil returned for a
non-loopback `ENTIRE_BRAIN_EMBED_URL`; every `search`/`vsearch`/`query`
crashed in the exact scenario the guard hardens. Now degrades to the bundled
Model2Vec embedder with a warning; regression test added.

**R3 — Brain write-lock scoping** (fixed in `15e7aaa`). Export held the
exclusive lock across the minutes-long transcript phase while every other
user times out at 10s — failing a concurrent distill's FINAL flush (an
hours-long run killed at its last step) — and `openHistoryFTS` took the write
lock on the query READ path, serializing all searches. Export now locks only
the shared-artifact phases; FTS opens fresh indexes lock-free and
double-checks freshness under the lock only for rebuilds.

**R4 — Silent full re-distill on upgrade** (fixed alongside this file). The
fingerprint salt plus the stable-branch-dir rename invalidated every existing
cache entry, and the shipped "legacy key" fallback could never match (a
legacy-format fingerprint never equals a salted one) — the first post-upgrade
run would re-distill the entire corpus (observed scale: ~14h of agent calls
for a 2,640-session brain). Fixed with lazy grandfathering: the legacy
formula is kept genuinely computable (including pre-rename transcript paths),
a matching legacy entry counts as cached and is rewritten under the new
key/format on first touch, and the transcript path is no longer hashed into
the new fingerprint at all — storage layout is not identity, and hashing it
re-invalidates the world on every future layout change. The filtered-session
carry-through, which copied legacy-format fingerprints under new-format keys
(making them permanently unmatchable), now preserves entries under their own
keys. Covered by `TestDistillCacheGrandfathersLegacyEntries`, including the
renamed-branch-dir case and the changed-content negative.
