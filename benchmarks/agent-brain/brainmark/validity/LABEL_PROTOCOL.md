# Construct validity — LABEL_PROTOCOL (plan 0.8)

`mechmetrics.py`'s `locate_calls_pre_edit` is the **pre-registered primary
metric** (`PREREGISTRATION.md` §3): it counts LOCATE tool calls, not whether
the agent actually *used* session A's knowledge. This document is the human
labeling protocol that tests whether the machine metric is a valid proxy for
the construct BrainMark claims to measure — "the agent re-derives what A's
session already established" — via Spearman's rho between the machine count
and a human-labeled re-derivation count (`analyze_validity.py`). If rho < 0.5,
`PREREGISTRATION.md`'s validity gate (dated amendment, 2026-08-19) requires a
dated amendment of the primary metric BEFORE any confirmatory run.

## Unit of labeling

**One label per pre-edit LOCATE call**, not one label per session. A session
with 8 pre-edit locate calls yields 8 labeling decisions. This is deliberately
finer-grained than "did the session succeed": the primary metric is a *count*
of calls, so construct validity has to be checked at the level the metric
counts at, or a session-level correlation could hide a metric that only
"happens" to correlate through session difficulty rather than through
individual calls actually re-deriving A's knowledge.

A call is in scope iff `mechmetrics.classify_tool()` would classify it
`"locate"` AND its index is strictly before the session's first `"edit"` call
(exactly `mechmetrics.session_metrics()`'s `pre_edit` slice) — this is not
independently reimplemented; `render_session.py` imports
`mechmetrics.extract_tool_calls` so the labeled unit and the scored unit are
provably the same calls, not a hand-rolled approximation that could drift.

## The label

For each in-scope call, the rater answers:

> **Does this call re-derive knowledge that session A's transcript already
> established** — the same file, function, contract, or decision A's session
> touched, changed, or reasoned about?

Values: `yes` / `no` / `unclear`.

- **`yes`** — the call's target (file path, grep pattern, matched region) or
  its result content overlaps with something visible in session A's reference
  panel (A's problem statement, A's patch's changed files/hunks). Overlap
  through the *pair's shared files* (`shared_files` in the task JSON) counts;
  overlap that is purely coincidental repo structure (e.g. both sessions
  `cat` the README because every session does) does not.
- **`no`** — the call explores something A's session never touched: novel
  files, unrelated functions, generic project orientation.
- **`unclear`** — the sheet does not show enough of the call's target/result
  to judge (e.g. a broad `grep` whose match content isn't rendered), or the
  overlap is genuinely ambiguous (same file, clearly different region, A's
  edit and B's call are both large). Use `unclear` rather than guessing —
  `analyze_validity.py` excludes `unclear` from both numerator and
  denominator of the re-derivation count rather than treating it as `no`.

## Blind to arm

The rater must **not** be able to tell whether the session ran `no_brain`,
`full_brain`, `mem0`, `graphify`, or `cmm`. This is the entire point of the
validity check: it tests whether the *metric* tracks re-derivation, and doing
that under an unblinded rater who already knows "this is the memory arm"
introduces exactly the expectation bias the check exists to rule out.

`render_session.py` enforces this mechanically:

- the sheet is written under an **opaque session id** (`sha256(seed:pair_id:
  arm)[:12]`), never the arm name or the results-directory path;
- the memory packet (`packet.txt`) and the prompt's `--- MEMORY ---` section
  are **never rendered** — only the `--- ISSUE ---` text is extracted from
  `prompt.txt`;
- `meta.json`'s `arm` field, `packet_provenance`, and any binary/tool path
  are never rendered;
- the id → `(pair_id, arm)` mapping is written to a **separate** file
  (`UNBLIND-MAP.json`) that must not be opened until every rater has
  submitted every label for a batch.

**Residual limitation (cannot be mechanically fixed, so it's a rater
instruction instead):** the agent's own free-text commentary inside a
`full_brain`/`mem0`/etc. session can sometimes narrate "recalling from a prior
session..." even though the harness never names the source to it (per
`prompts.py`'s arm-neutral policy, the agent is never told which arm it is,
but nothing stops it from noticing a packet is non-empty and saying so). If a
rater suspects a session is de-blinding itself this way: **label the call on
its own merits** (does *this specific call's target* overlap A's knowledge —
yes/no/unclear), do not let the suspicion change the verdict, and separately
flag the session id in your notes as "possible de-blinding" so it can be
audited without discarding the label.

## Sampling: ~30 pilot B sessions, stratified

`render_session.py batch` draws the sample deterministically (seeded, so the
sample is reproducible and cannot be silently redrawn after a bad-looking
batch):

- stratify by **arm**: pull an approximately equal share from every arm run
  in the pilot, so the validity check is not answerable only within one arm;
- stratify by **pair**: prefer covering distinct pairs over covering the same
  pair's five arms repeatedly, up to the target count;
- draw order is `sorted(pair_id, arm)` keyed and shuffled with
  `random.Random(seed)` (seed = `config.json`'s `seeds.master`, or an explicit
  `--seed`) — deterministic, auditable, and independent of directory
  iteration order or wall-clock.

Target: **~30 sessions** (plan 0.8). At full pilot scale (10 dev pairs × 5
arms × N reps) this is a genuine subsample, not the whole pilot — the sample
size is what two raters can label in the ~8h/rater budget in the plan's Phase
2, not a statement about how much of the pilot is "trustworthy".

## Process

1. `render_session.py batch --results <B-results-root> --arms ... --n 30
   --seed <seed> --out-dir validity/sheets/<batch>/` writes one Markdown sheet
   per sampled session (`sheets/<opaque-id>.md`) plus `UNBLIND-MAP.json` in a
   sibling location the sheets directory does not need read access to.
2. Both raters independently label every call in every sheet, offline,
   without discussing a session before both have submitted it — same
   independence discipline as `SEAL_PROTOCOL.md`'s dual review.
3. Each rater's labels are transcribed into a `labels.json`
   (`{"rater": "...", "sessions": {"<opaque-id>": {"calls": [{"call_index":
   int, "verdict": "yes"|"no"|"unclear"}]}}}`) — `analyze_validity.py`
   validates this shape and refuses to run on a malformed file rather than
   silently skipping entries.
4. Only after both raters' `labels.json` files exist does
   `analyze_validity.py` join in `UNBLIND-MAP.json` (arm, pair_id) and the
   machine `locate_calls_pre_edit` counts, and compute:
   - **Spearman's rho** between the machine count and the labeled
     re-derivation count (`yes` count among non-`unclear` calls), per session;
   - **per-rater agreement** (percent agreement + Cohen's kappa, reusing
     `seal.cohens_kappa`, which is already generic over the category set) on
     whichever calls both raters labeled.
5. Report rho, its sample size, and per-rater kappa in `DATASHEET.md` /
   `PREREGISTRATION.md`'s dated amendment, regardless of the value — a low
   rho is the validity gate firing, not a result to re-sample away.

## What this protocol is not

It is not a gate on individual *sessions* (no session is excluded from the
main report based on its labels) and it is not a second scoring pass on the
benchmark's outcome. It exists solely to answer one question: is the
pre-registered primary metric measuring what BrainMark claims it measures.
