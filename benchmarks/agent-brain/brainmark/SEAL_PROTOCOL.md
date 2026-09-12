# SEAL_PROTOCOL — dual independent review (v2)

Companion to `seal.py` and `PREREGISTRATION.md`. Read this before running
`seal.py review-template` or filling in a `REVIEW.json`. This is the plan-0.7
hardening of sealing: BrainMark v1 sealed a task set behind one named
reviewer; v2 requires **two independent reviewers per candidate pair**, an
adjudication path for disagreements, and Cohen's kappa across raters recorded
in `SEAL-MANIFEST.json`. `seal.py promote` mechanically refuses to run without
all three.

## Why dual review, why enforced in code

A single reviewer's accept/reject calls are unfalsifiable — nothing checks
whether the criteria were applied consistently, and a lenient or tired
reviewer can quietly inflate the sealed set. Two independent reviewers who
score without seeing each other's verdicts give an inter-rater reliability
statistic (Cohen's kappa) that is itself evidence about whether the accept/
reject criteria are well-specified. `seal.py` enforces this the same way it
enforces sha256 pinning: not because reviewers are assumed dishonest, but
because "we did the review carefully" is not falsifiable and "kappa = 0.81
across two raters who did not discuss pairs before submitting" is.

## Disclosure template

> Task pairs in this dataset were reviewed by two raters: the user (project
> lead) and one teammate, both project-affiliated with the BrainMark benchmark
> and with an interest in entire-brain's outcome. Raters reviewed
> independently — verdicts were recorded before any discussion — and
> disagreements were resolved by a named adjudicator. Cohen's kappa across the
> two raters is reported in `SEAL-MANIFEST.json` (`seal_v2.cohens_kappa`) and
> in the paper's dataset section. Reviewer affiliation is a limitation, not a
> hidden fact: rater independence at the point of judgment, and a machine-
> checked reliability statistic, are the mitigations, not blind unaffiliated
> reviewers we do not have.

Copy this paragraph (updating rater identities/roles if they change) into
`DATASHEET.md`'s composition section and the paper's ethics/reproducibility
section (`paper/OUTLINE.md` §Ethics) verbatim or near-verbatim — do not
paraphrase away the "project-affiliated" disclosure.

## Accept / reject criteria (the fixed vocabulary)

Every reject verdict MUST cite at least one of these codes (`seal.py` refuses
promotion on a reject with an empty `criteria` list, and on any code outside
this list — the vocabulary is fixed so verdicts can be audited and counted,
not free text a rater invents per pair). Accept verdicts may also cite codes
(e.g. "DEP" to record *why* it's a strong pair), but are not required to.

| code | name | question the rater is answering |
|---|---|---|
| `DEP` | genuine A→B dependence | Does resolving B actually require understanding what A's session changed — not merely touching the same file, but the same mechanism, contract, or decision? |
| `LEAK` | leakage | Does A's patch (or B's problem statement/tests) already contain B's fix? If A already IS B's answer, B is a lookup, not a second task, and the pair does not test rediscovery. |
| `TRIV` | triviality | Is B's fix trivial (rename, typo, one-line, no navigation required) such that no memory source — including a perfect one — could show a mechanism effect? |
| `REVEAL` | answer-revealing | Does B's problem statement, or something visible in A's artifacts, name the exact file/symbol/line B must edit? If so the task is solved by reading the prompt, not by memory or by search, and the primary metric (`locate_calls_pre_edit`) cannot be interpreted. |

A pair is **accepted** only when the rater's honest answer is: genuine
dependence present (`DEP` reasonably applies), and none of `LEAK`, `TRIV`,
`REVEAL` apply strongly enough to null the comparison. A pair flagged
`needs_human_review: true` by the miner (borderline patch-body overlap, see
`DATASHEET.md` §5) must be judged individually against `LEAK` specifically —
never accepted by default because the miner's automatic screen already passed
it at a wider tolerance.

## Rater instructions

1. **Read `SEAL_PROTOCOL.md` (this file) and the pair's full context** — A's
   problem statement, A's gold patch, B's problem statement, B's gold patch,
   and the miner's `shared_files` / `score_components` / `leakage` fields
   (all present in the candidate JSON) — before judging.
2. **Judge independently.** Do not discuss a pair with the other rater, or
   look at their verdict, before you have recorded your own verdict for that
   pair. `seal.py` does not enforce this procedurally (it cannot see a
   conversation), so it is a discipline the raters owe the statistic —
   kappa computed after raters have seen each other's answers is not
   inter-rater reliability, it is agreement-after-anchoring.
3. **Record verdict, criteria codes, and a short note** for every candidate,
   not only ones you plan to accept. `seal.py promote` requires a verdict
   from *both* raters on every pair present in `REVIEW.json["pairs"]` — an
   omitted pair (not "reject" — literally absent or blank) refuses the whole
   promotion, on purpose: a pair one rater silently skipped is a pair that
   was never actually dual-reviewed.
4. **Split assignment** (`sealed` vs `dev`, 30+/10 per `config.json`) happens
   only after both verdicts are in and, if needed, adjudication is resolved.
   Do not pre-assign a split before the pair is accepted.
5. **When you disagree**, the pair goes to adjudication (below) — do not
   quietly change your verdict to match the other rater; a resolved
   adjudication with a named adjudicator is the record of the disagreement,
   whereas a changed verdict destroys it.

## Adjudication

When the two raters' verdicts differ (`accept` vs `reject`), fill in the
pair's `adjudication` object:

```json
"adjudication": {
  "resolved_verdict": "accept",
  "adjudicator": "<name — may be one of the two raters, acting a second time>",
  "note": "<why the disagreement was resolved this way>"
}
```

`seal.py promote` refuses any disagreement whose `adjudication.resolved_verdict`
is not `"accept"`/`"reject"`, or whose `adjudicator` is blank. A pair both
raters reject is **not** sent to adjudication (unambiguous exclusion); a pair
both raters accept is not either (unambiguous inclusion, subject to the split/
count checks `seal.py` already runs).

## Cohen's kappa

Computed by `seal.cohens_kappa()` across every pair that received a verdict
from both raters (not only accepted ones — a low kappa on rejects is exactly
as informative as a low kappa on accepts):

```
kappa = (po - pe) / (1 - pe)
po = observed agreement            = P(rater_a's verdict == rater_b's verdict)
pe = agreement expected by chance  = sum_c P(rater_a = c) * P(rater_b = c)
```

Stored in `SEAL-MANIFEST.json.seal_v2`: `cohens_kappa`, `observed_agreement`,
`expected_agreement`, `n_dually_reviewed_pairs`, `disagreements_adjudicated`,
and the `raters` block (names + disclosed affiliations). `seal.py promote`
**refuses** if kappa cannot be computed at all (fewer than two dually-reviewed
pairs) — it does NOT refuse on a *low* kappa, because a low kappa is a real,
reportable finding about the criteria's specificity, not a bug to suppress by
re-sealing until the number looks better. Report it in `DATASHEET.md` and the
paper regardless of its value.

Landis & Koch (1977) rule of thumb for interpreting the value (report the
number either way; this is guidance for the paper's discussion, not a gate):

| kappa | interpretation |
|---|---|
| < 0.00 | worse than chance |
| 0.00–0.20 | slight |
| 0.21–0.40 | fair |
| 0.41–0.60 | moderate |
| 0.61–0.80 | substantial |
| 0.81–1.00 | almost perfect |

If kappa lands below "substantial" (~0.61), the honest response is to revisit
the criteria wording in this document (they may be under-specified) and
re-run review on a fresh sample to check whether reliability improves — never
to unilaterally override the lower-scoring rater's verdicts.

## Refusal summary (what `seal.py promote` mechanically checks)

`_promote_v2` in `seal.py` refuses, in this order, before writing any file:

1. `REVIEW.json["raters"]` does not name exactly two raters, or either lacks
   a non-empty `name`/`affiliation`.
2. Any pair lacks a valid `accept`/`reject` verdict from **either** rater.
3. Any `reject` verdict cites zero criteria codes, or cites a code outside
   the fixed vocabulary above.
4. Any disagreement lacks a resolved, named adjudication.
5. Fewer than two pairs were dually reviewed (kappa cannot be estimated).
6. (Unchanged from v1) accepted pairs overlap between `sealed`/`dev`, an
   accepted pair has no valid split, or the sealed/dev counts miss the
   `config.json` targets without `--allow-short`.

A `REVIEW.json` without a `raters` key falls back to the original v1
single-reviewer path (`_promote_v1`) unchanged, for backward compatibility
with harness tests written against v1 — but `seal.py review-template` only
ever emits the v2 shape, so a *fresh* seal always goes through dual review.
