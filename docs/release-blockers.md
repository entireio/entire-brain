# Release Blockers

A register of release-gating findings from the PR #33 review (2026-06-11),
kept next to the audit so a release decision can see what is open, what was
fixed, and exactly why. The standard for this file is the eval ledger's:
explicit claims, exact evidence, no euphemism.

## OPEN — B1: Answer-bearing query hints confound every "citable proof" suite

**The claim under audit.** `docs/release_readiness_audit.md` graduates three
benchmark suites to citable release proof and asserts the panels work
"without handing over hidden validation or expected text". **That sentence is
false as written**, for all three suites and for at least one newer task.

**The mechanism.** `benchmarks/agent-brain/run.py` (`prompt_for`) injects
`Useful query terms: {brain_queries}` into the **brain/MCP-arm prompts only**;
the `no_brain` arm receives no such terms. Whatever appears in a task's
`brain_queries` is therefore handed to exactly the arm whose superiority the
suite is supposed to prove. The committed task files (as of the PR #33
review; since rewritten — see Progress below) put answer-bearing content
there:

| Suite (claimed result) | What `brain_queries` hands the brain arm | What the hidden validation requires |
|---|---|---|
| `entire-brain-history-codex-schema-contract` (score 66.0→91.5, pass 0/4→4/4) | The literal strings `"should rely on prompt plus local validation"` and ``"Checkpoint-context review reverted Codex `--output-schema` usage"`` | `rg 'should rely on prompt plus local validation, not --output-schema' internal/cli/seed_test.go` finds ≥1 match, and `--output-schema` is removed from `seed.go` — i.e. the prompt contains, near-verbatim, the exact string a hidden check greps for. An agent can pass by echoing its own prompt into a comment. |
| `entireio-cli-manual-commit-attribution-base` (pass 1/4→4/4, score 76.25→92.0) | `"RealignAttributionBase newHead manual commit hooks"`, `"BaseCommit AttributionBaseCommit realign"`, and the hidden-adjacent test name `"TestManualCommit_AttributionStaleBase"` | Hidden tests pass only when the fix re-adds the call `state.RealignAttributionBase(newHead)` — **the exact function name and argument are in the brain arm's prompt**. |
| `entireio-cli-radar-manual-attribution-deletions` (pass 2/4→4/4, score 73.0→92.5) | Identical `brain_queries` to the row above | Same hidden tests; same leak. |
| `entire-brain-semantic-tokenized-idf-ranking` (newer, not yet promoted) | `"tokenIDFWeight rare token ranking"` and the **hidden test's exact name** `"TestTokenizedSearchRanksRareTokenAboveCommonTokens"` | `rg 'weights\[i\] = tokenIDFWeight\(total, df\)'` finds exactly one match — the prompt names the function the hidden grep requires verbatim. |

**Why this is a confound and not a nitpick.** The suites measure
*(answer-bearing hint + brain)* versus *(no hint, no brain)*. Any observed
lift decomposes into (a) the hint telling the agent what the fix is and (b)
the brain retrieving context — and the committed artifacts cannot separate
them. In the schema-contract case the hint alone is sufficient to pass the
hidden exact-string gate with zero retrieval. The numbers themselves are
real (recomputed from the per-record artifacts during review; timestamps,
token counts, and logs are consistent and one genuine no-brain failure run is
retained) — what is unsupported is the *attribution* of the lift to the
brain.

**Why the PR's own hygiene machinery missed it.** The leak auditor checks
*transcripts* for harness markers and host paths; it never intersects
`brain_queries` with hidden-validation command strings, expected-text greps,
or hidden test names. The `Useful query terms` mechanism predates this PR
(present on `main` in 10 tasks), so prior comparisons carry the same
confound; this PR is gated on it because it is the one promoting these suites
to "citable proof".

**Required to clear B1 (all of):**

1. Strip answer-bearing terms from `brain_queries` in the affected tasks — no
   identifiers from the expected fix, no substrings of hidden-validation
   greps, no hidden test names — or give both arms identical hints so the
   delta isolates retrieval.
2. Extend the leak auditor: flag any `brain_queries` token (or ≥3-word
   substring) that appears in hidden validation commands, expected strings,
   or hidden test names. The four rows above must fail the auditor as it
   stands.
3. Re-run the three suites clean and re-promote only what survives.
4. Until then, correct the audit sentence and the two press-release "proof"
   bullets to disclose the confound explicitly.

**Progress (PR #34, branch claude/layer-b-scenarios).** Items 1 and 2 are done
in that PR; item 4 was completed on main during the PR #33 review follow-ups;
item 3 is NOT done, so B1 stays open:

- Item 2 (this PR): `brain_query_leak_audit` (run.py) flags identifier-shaped
  query tokens — matched case-insensitively, since retrieval and agents fold
  case — and verbatim 3+-word query phrases that appear in the hidden fix
  text or fix file path, hidden expected_files, hidden validation commands,
  expected-string greps, hidden test names, or validation fixture contents.
  It is wired into `panel_preflight` for every panel, and
  `run_test.BrainQueryLeakAuditTests` freezes the four rows above with their
  verbatim committed queries/replacements/validation and asserts each fails
  the auditor as committed.
  Operationalization note: the literal "any token" wording above is not
  implementable — plain English words from honest symptom queries ("prompt",
  "local", "commit") appear in the hidden strings of most tasks and would
  flag ~40 of 51 task files. The auditor therefore flags code-shaped tokens
  and 3+-word verbatim phrases. Known residual: a 1-2 word plain-English
  fragment shared with a hidden string is not flagged.
- Item 1 (this PR): every task the auditor flags had its `brain_queries`
  rewritten to symptom-level retrieval terms — 35 pre-existing task files
  (including the four named rows and the 10 pre-existing carrier tasks on
  main); the 5 new Layer-B tasks in the same PR were authored to the same
  standard. Reproducible check: run `brain_query_leak_audit` over every
  `tasks/*.json` — 0 of 51 files flagged.
- Item 4 (done on main, PR #33 follow-up): the audit sentence in
  `docs/release_readiness_audit.md` was corrected and the press release
  gained its Known Confound section gating the proof citations.
- Item 3 (open): the three promoted suites have NOT been re-run with the
  clean queries. Their retained evidence is unchanged and still carries
  the confound disclosed in `docs/release_readiness_audit.md` and the
  press release; the lift attribution remains unsupported until clean
  re-runs land and only survivors are re-promoted.

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
