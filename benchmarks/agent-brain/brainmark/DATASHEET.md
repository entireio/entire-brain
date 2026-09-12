# Datasheet — the BrainMark pair dataset

Following Gebru et al., *Datasheets for Datasets*. Describes the **pair** dataset
BrainMark constructs. It does not redistribute SWE-bench; it distributes
*references* to SWE-bench instances plus derived pairing metadata.

## Motivation

**What gap does it fill?** Existing agentic coding benchmarks score one task in
isolation. Nothing measures whether a *second* session on the same repository
benefits from the first. BrainMark pairs need `(A, B)` such that A is genuine
prior work on code that B must also touch — that pairing does not exist upstream
and is what this dataset provides.

**Who built it?** The Entire team, for internal evaluation of entire-brain and
for external publication of the harness.

## Composition

**Instances.** Each record is an ordered pair of SWE-bench instance IDs from the
same repository, plus derived metadata: shared files, hunk-header symbol overlap,
patch-body containment score, ancestry verification flag, and per-side sha256 of
the problem statement and gold patch. **No SWE-bench problem statement, patch, or
test is copied into the record** — only its sha256 and its instance ID. The task
text is read from the upstream dataset at run time.

**Source pool.** 303 unique instances loaded from
`graphmark/agentic-swebench/tasks/{multilingual_300,pilot_tasks,python_tasks}.json`
(dataset id `swe-bench/SWE-bench_Multilingual`, plus 3 Python instances). Each
source file's sha256 is recorded in `candidates/INDEX.json`.

**Current size.** 15 verified candidate pairs — `preactjs/preact` 12,
`prometheus/prometheus` 3. See "Known limitations" below; this is **short of the
n ≥ 30 the pre-registration requires**.

**Is anything missing?** Yes, and deliberately: a pair is emitted only when its
ancestry can be *verified against a local clone*. 162 pairs cleared the
shared-file gate but sit in repositories that are not in the local repo cache, so
`git merge-base --is-ancestor` could not be evaluated. They are counted and
reported, never emitted.

## Collection process

Mined by `mine_pairs.py`, deterministic and offline. For every ordered pair
`(A, B)` within a repository, sorted by `(created_at, instance_id)`:

1. **same repository**
2. **A strictly precedes B** in that order
3. **≥ 1 file touched by both gold patches** (paths parsed from `diff --git`)
4. **`git merge-base --is-ancestor base_A base_B`** verified in a local clone.
   Unverifiable ⇒ excluded, never a pass.
5. **leakage screen**: patch-body token containment (Szymkiewicz–Simpson over
   identifier tokens on `+`/`-` lines) **< 0.8**. If A's fix already *is* B's fix,
   B is a lookup, not a second task. Pairs in `[0.6, 0.8)` are emitted but flagged
   for individual human review.
6. **one best A per B**, scored `Jaccard(files) + Jaccard(hunk symbols)`, with a
   deterministic total-order tiebreak.

Determinism is a tested contract: two runs produce byte-identical output. No
wallclock, PID, hash-seed, or filesystem-iteration order reaches the output.

## Preprocessing / labeling

No human labels on content. Human review is a **gate, not an annotation**:
`seal.py` refuses to promote without an attributed `REVIEW.json` in which a named
reviewer accepts each pair and assigns it to `sealed` or `dev`. Flagged pairs must
be accepted individually.

## Uses

**Intended.** Measuring cross-session memory reuse for coding agents.

**Not appropriate.** (a) Training — the pairs point at public SWE-bench instances
whose gold patches are on the open internet; a model trained on them is
contaminated. (b) Claiming general coding ability — this measures *rediscovery
effort*, not capability. (c) Cross-repository generalization — the current pairs
come from two repositories.

## Distribution and licensing

- **SWE-bench / SWE-bench Multilingual** are MIT-licensed; the underlying
  repositories carry their own licenses (`preactjs/preact` MIT,
  `prometheus/prometheus` Apache-2.0). This dataset redistributes **no upstream
  content** — only instance IDs, commit SHAs, derived scores, and hashes.
- The BrainMark harness is released under this repository's license.
- Consumers must obtain SWE-bench itself from upstream.

## Maintenance

Versioned by `SEAL-MANIFEST.json`, which pins every task file's sha256 alongside
the miner, prompts, metric definition, and vendored estimator. Re-mining after a
repo-cache expansion produces a **new** seal; results across seals are not pooled.

## Known limitations (stated up front)

1. **n = 15 < 30.** The pre-registered confirmatory set cannot be filled from the
   current repo cache. The gates were **not** loosened to close the gap. Cloning
   the 22 uncached repositories would, on the same gates, yield roughly 83
   candidates — a planning estimate whose ancestry is unverified.
2. **Two repositories** (preact, prometheus). Repository-level confounds cannot be
   separated from arm effects at this n; no per-repo sub-group will be reported.
3. **`created_at` ordering is issue-creation order**, not merge order. Ancestry
   verification is what actually enforces "A's base precedes B's base".
4. **Containment is lexical.** A semantically leaking pair with disjoint
   identifiers would pass the screen; that is what human seal review is for.
5. **Multilingual pool.** Language is recorded per side; no language balance is
   guaranteed at this n.
