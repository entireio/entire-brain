# BrainMark

Does a second coding session reuse what the first one learned?

That is the product claim for entire-brain, and it has never been measured.
LoCoMo/LME measure the retrieval substrate over chat transcripts; they say
nothing about a *second coding session*. BrainMark measures it directly: an agent
does task A, memory is derived from A's session, then the agent does a related
task B — with memory, without it, and with each competitor — and we count how
much rediscovery B needed.

**Status: the $0 stage is built and tested. No paid session has been run.**

## The design in one paragraph

Mine ordered SWE-bench pairs `(A, B)` from the same repository where A's fix
touched code B must also touch. Run session A **once**, pin its transcript by
sha256, and hand those exact bytes to five memory sources. Each emits one bounded
packet through one identical envelope. Run B once per arm and grade it with its
own `FAIL_TO_PASS` tests in the official Docker harness. The pre-registered
primary metric is *locate calls before the first edit* — the mechanism, not the
score.

| arm | memory source | proves |
|---|---|---|
| `no_brain` | sentinel-empty packet | baseline |
| `full_brain` | `entire-brain distill` + `refresh history` over pinned A JSONL | the product (the only brain arm) |
| `mem0` | mem0 OSS over the same bytes | vs the named competitor |
| `graphify` | Graphify over the same bytes | vs the YC code-memory competitor |
| `cmm` | codebase-memory-mcp over the same bytes | vs the OSS competitor |

## Current mining result — read this before planning a run

```
instance pool                    303
VERIFIED CANDIDATE PAIRS          15    preact 12, prometheus 3
```

**15 < the 30 the pre-registration requires.** The gates were not loosened.

The binding constraint is the **repo cache, not the task pool**: 162 further
pairs cleared the shared-file gate but live in 22 repositories with no local
clone, so `git merge-base --is-ancestor` could not be evaluated and they were
excluded. Cloning those 22 repositories would, on the same gates, yield roughly
**83 candidates** — a planning estimate with unverified ancestry, printed by the
miner as a diagnostic and never emitted as candidates.

To close the gap, clone the missing repositories into
`graphmark/agentic-swebench/repo-cache/<owner>_<name>` and re-mine. Do not relax
the gates.

## Layout

```
config.json          every pin: roots, models, budgets, competitor versions, seeds
mine_pairs.py        deterministic $0 miner            -> candidates/
seal.py              human-reviewed promotion          -> tasks/{sealed,dev}/ + SEAL-MANIFEST.json
prompts.py           one scaffold for all 5 arms + the symmetry gate
mechmetrics.py       locate_calls_pre_edit (PRIMARY) + secondary metrics
memsources/          pinned A bytes -> one bounded packet, per arm
session_a.py         session A driver (once per pair per tier)
run_b.py             session B driver (once per pair per arm)
grade.py             wraps graphmark grade_tag.sh -> official SWE-bench Docker eval
report.py            REPORT.md / REPORT.json; refuses to aggregate on any breach
vendor/              pinned copy of graphmark metrics.py, with provenance
tests/               55 offline tests, no network, no paid calls
PREREGISTRATION.md   the frozen contract — read before quoting any number
DATASHEET.md         the pair dataset, its provenance and limits
REPRODUCIBILITY.md   exact pins: toolchain, commits, models, seeds
```

## Runbook

### Tier 0 — $0 (done)

```bash
cd benchmarks/agent-brain
python3 -m unittest discover -s brainmark/tests -t . -p "test_*.py"   # 55 tests
python3 brainmark/mine_pairs.py                                       # -> candidates/
python3 brainmark/seal.py review-template                             # -> REVIEW.json
#   ... a human reads every pair and fills in accept/split ...
python3 brainmark/seal.py promote
python3 brainmark/seal.py verify
```

Build the CLI binary once (its sha256 lands in every `full_brain` packet):

```bash
go build -o benchmarks/agent-brain/brainmark/bin/entire-brain ./cmd/entire-brain
```

### Tiers below this line SPEND MONEY. None has been run.

**Tier 1 — null test, ~$15, non-negotiable.** 3 dev pairs × 2 reps, every arm
running its real machinery on a sentinel packet:

```bash
python3 brainmark/run_b.py --pair tasks/dev/<pair>.json --a-dir <A> --null-test
```

Primary and token deltas **must straddle zero**. If they do not, the machinery is
not inert and no headline may be reported until that is fixed.

**Tier 2 — pilot, Sonnet, 10 dev pairs, ~$60–120.** 10 A + 50 B + prep. Fix the
*harness* only. Never the tasks.

**Tier 3 — full, n ≥ 30 sealed, main model, ~$700–1,500 on Opus.** 30 A + 150 B at
concurrency 8, resume-safe.

**Tier 4 — grading (free) and report.**

```bash
python3 brainmark/grade.py --results results/B/full --tag bm_full
python3 brainmark/report.py --results results/B/full
```

## What makes this fair

- **One envelope.** Every arm's memory is serialized to the same JSON shape,
  truncated by the same rank-preserving bounder, screened by the same
  delimiter guard, and wrapped in the same tags. Arms differ in packet *bytes*
  only.
- **Arm-neutral instructions.** The policy text never names the memory source.
  Tool *output* differences are legitimate; instruction differences are not. The
  test is: *would this sentence help an arm with no memory?*
- **The packet is pinned, not the store.** Competitor ingest is nondeterministic;
  pinning the delivered bytes means that nondeterminism cannot become an arm
  asymmetry between what was measured and what was reported.
- **Symmetric drops.** A pair counts only if every arm passes; a pair failing in
  one arm is dropped from all arms.
- **Failures are loud.** No memory source may degrade to an empty packet. A
  silent empty packet scores an infrastructure failure as "memory did not help",
  which is the easiest way to fake this benchmark's headline.
- **Fairness primitives are imported, not reimplemented.** `run.py` is loaded as a
  library and never modified.

## What the report refuses to do

`report.py` exits non-zero and prints `REFUSED` — rather than aggregating —
when the seal does not verify, a packet's sha256 does not match the bytes on
disk, a pair's arms disagree on the re-derived prompt-symmetry sha, or the
all-arms-clean gate is unmet. Each refusal is a real failure mode: post-hoc task
editing, reporting a packet that was never delivered, arms given different
instructions, and asymmetric dropping.

## Power — what will not be claimed

At n = 30 the MDE is roughly 25 points. A CI straddling zero is **inconclusive**,
never "no difference" and never a win read off the point estimate. No sub-group
smaller than ~30 pairs gets reported. Leave-one-out range is always shown; if
dropping one pair moves the headline across zero, that is one instance, not an
effect.

## Verification performed

55 offline tests, no network, no paid calls: miner determinism (byte-identical
double run), leakage screen, ancestry gate, seal round-trip and tamper detection,
report refusal paths, prompt symmetry across all five arms, delimiter-guard
rejection, packet re-derivation, mechanism metrics against three hand-labelled
stream fixtures (including a no-edit session), `full_brain` manifest synthesis
against the real binary, and a full `run_b` results-directory layout driven by a
PATH-stubbed `claude`.

Two load-bearing behaviors were **mutation-verified** — deliberately broken to
confirm the tests catch them:

| mutation | result |
|---|---|
| symmetry gate stops raising | `test_asymmetric_instructions_are_detected` FAILS |
| instructions name the memory source | `test_all_five_arms_share_one_symmetry_sha` FAILS (5 distinct shas) |
| leakage cap removed from the miner | `test_identical_patches_are_rejected_as_leakage` FAILS |

All mutations were reverted; the suite is green.
