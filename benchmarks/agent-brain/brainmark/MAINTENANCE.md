# MAINTENANCE — versioning, deprecation, leaderboard intake

Companion to `SEAL_PROTOCOL.md` (how a seal is produced) and
`release/` (how a seal becomes a public artifact). This document is about
what happens to the artifact *after* it is sealed and released: how new
versions are numbered, when an old one is retired, and how a third party's
submitted score gets in front of anyone.

## Versioning is by seal, not by date or by code tag

A BrainMark **version** is a `SEAL-MANIFEST.json`, full stop. Two runs against
the same seal are the same version even if the code that produced `report.py`
output changed cosmetically; two runs against different seals are different
versions even if nothing else changed. This follows directly from what
`seal.py` already guarantees: the manifest pins the sha256 of every sealed
task file, the miner, `prompts.py`, `mechmetrics.py`, the vendored estimator,
and `PREREGISTRATION.md` (`seal.py`'s `verify()`). Two manifests with
different `sha256`s for any of those are, by the tool's own definition, not
comparable — so "version" and "seal" have to be the same concept, or the
version number would claim a comparability the manifest itself refuses to
certify.

**Version string:** `v<seal_schema_version>.<sealed_count>-<first 12 hex of
config_sha256>`, e.g. `v2.150-3f9a0c112abc`. This is deterministic from the
manifest alone (no separate version registry to fall out of sync) and it
visibly encodes the two things that most often silently change between runs
(schema and config).

**A new version is required whenever any of the following changes:**

- the sealed/dev task set (any re-mine, any additional seal round)
- `mine_pairs.py`, `prompts.py`, `mechmetrics.py`, or the vendored estimator
- `config.json` (packet bounds, arm list, model tiers, seeds)
- `PREREGISTRATION.md`

None of these can change silently — `seal.py verify()` and `report.py` both
refuse to aggregate against a stale manifest (`report.py`'s "REFUSES TO
AGGREGATE" doctrine) — so in practice "a new version was required" and "the
old manifest stopped verifying" are the same event, discoverable by running
`seal.py verify` against the old manifest after the change.

**Results across versions are never pooled.** This is not new policy; it is
`PREREGISTRATION.md` §7's existing "cross-cell comparisons are confounded;
only paired-on-identical-instances comparisons will be made" rule, restated
at the artifact-lifecycle level: a v1.15-xxxx headline and a v2.150-yyyy
headline are two separate experiments that happen to share a name.

## Deprecation policy

A version is marked **deprecated** (not deleted) when:

1. a harness bug is found that affects its numbers (per
   `PREREGISTRATION.md` §8, "the harness is fixed and affected cells re-run;
   the task set is never edited" — deprecation is what happens to the OLD
   numbers once the re-run exists);
2. a newer sealed version supersedes it with a larger `sealed_count` at the
   same or newer schema version, and the plan's Phase-3 confirmatory run has
   moved to the newer version; or
3. the validity gate (`PREREGISTRATION.md` §12) fires against a version's
   primary metric and the required dated amendment changes the metric
   definition — every version sealed under the old definition is deprecated,
   not silently reinterpreted under the new one.

**Deprecation never deletes.** The deprecated `SEAL-MANIFEST.json`, its
`REPORT.json`/`REPORT.md`, and the Croissant manifest (`release/croissant_gen.py`)
stay published with a `"deprecated": {"reason": ..., "superseded_by": ...,
"dated": ...}` block added to the top of each. A number that was quoted from a
deprecated version should still resolve to the artifact that produced it —
retracting the page, rather than marking it, would make the retraction itself
unverifiable.

**A deprecation is itself dated and reasoned**, following the same discipline
as `PREREGISTRATION.md` §9's deviation log: no silent supersession, ever.

## Leaderboard submission intake

BrainMark's leaderboard (once hosted — see `paper/OUTLINE.md`'s compute/
artifact disclosure for hosting plans) accepts a submission as a **new
competitor arm's packet results against an EXISTING sealed version**, never
as a new task set. This mirrors the benchmark's own arm design: every
competitor (`mem0`, `graphify`, `cmm`, and any future submission) consumes
the *same pinned session-A bytes* and is scored by the *same* primary metric
and gates as the shipped arms — a leaderboard entry that used a different
task set, a different B prompt, or a relaxed gate would not be a BrainMark
result no matter what it reports.

**Intake requirements**, mirroring `PREREGISTRATION.md` §5's gates and
`report.py`'s integrity checks:

1. **Declare the sealed version** (the manifest version string above) the
   submission targets. A submission against a deprecated version is accepted
   but is labeled with that version's deprecation notice; it is not silently
   remapped to a newer version's task set.
2. **Submit a memory source, not a modified prompt.** The submission is a
   `memsources/<name>.py`-shaped module (pinned-A-bytes in, one bounded memory
   packet out — see `memsources/base.py`'s envelope contract); it must NOT
   see B's problem statement before producing its packet, must not alter
   `prompts.py`'s scaffold, and its packet must pass the prompt-symmetry gate
   (`prompts.py.assert_symmetric`) identically to the shipped arms.
3. **Same gates apply, gated the same way.** Per-session `usd>0 ∧ non-empty
   patch`; all-arms-clean pair gate (a pair the submission fails on drops
   from every arm's comparison for that pair, not just the submission's);
   packet sha256 pinned and re-verified at report time; no Entire-family
   tooling; no `.entire`/`.benchmark`/checkpoint-ref inspection.
4. **Prep cost is disclosed and reported separately from session cost** — a
   submission that spends heavily on retrieval-time compute is not penalized
   in the headline metric, but the number is never omitted (mirrors the
   shipped arms' `prep.seconds`/`prep` provenance already recorded by
   `memsources/base.py`).
5. **Independent reproduction before publication.** A submitted arm's numbers
   are re-run once, independently, before appearing on the leaderboard — the
   same "verify before believing" discipline this whole benchmark is built
   on (`PREREGISTRATION.md`'s entire design is a response to "how does a
   benchmark launder a null result into a headline"; a leaderboard is the
   same failure mode with a different author).
6. **Rejections are recorded, not silently dropped**, listing which gate the
   submission failed — a rejected submission is evidence about the gates too,
   and future maintainers should be able to see what got rejected and why.

**Out of scope for a submission:** proposing a new task pair, a new prompt
scaffold, or a new primary metric. Those are pre-registration-level changes
(`PREREGISTRATION.md` §9's deviation log) and go through the seal/prereg
process, not the leaderboard intake process — a leaderboard cannot be allowed
to become a backdoor for changing what is being measured.
