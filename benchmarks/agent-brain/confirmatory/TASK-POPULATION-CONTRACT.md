# Representative task-population v2 contract

This contract defines the population boundary needed before final development calibration or
confirmatory task selection. It does not mine tasks, reveal holdout membership, size the experiment,
or authorize a model run.

## Treatment-blind selection

The population binds a complete eligibility ledger for the preregistered
`post_cutoff_source_plus_test_reverse_patch_v1` rule before retrieval or treatment outcomes are
observed. The contract either retains every eligible task or binds a preregistered seeded sample;
the linter reconciles the eligible count with the represented population. A candidate cannot advance
until a separate receipt verifies both task selection and deterministic split assignment under their
committed owner-held keys. This prevents retrieval quality or product results from deciding which
tasks enter development, calibration, or holdout.

The companion `task_eligibility.py` scanner can inventory first-parent integration units that touch
both production Go and test files. Its output contains commit identities, structural counts, and
content commitments but no subjects, paths, prompts, or patches. It is only a static pre-screen:
reverse-patch negative controls and independent symptom-only review remain pending. Any scan whose
identities were inspected by a product optimizer is permanently labeled development-only and cannot
be reassigned to calibration or confirmatory holdout.

The checked-in development scan covers `github.com/entireio/cli` from
`3ebc57dbb923c0aa6eb53f17384109d189953c6c` through
`df765ab952185595d65f561f8ccb8036598980a8`. It records 31 first-parent integration units and 23
source-plus-test candidates: 7 low, 3 medium, and 13 high static-scope bands. All 23 negative-control
statuses are `pending`; this is an inventory milestone, not an eligible task population. Its
canonical self-hash is
`177a5f71bd9ac82d8b38e61251a07d0b073852ce5a527da0b05e64eabc8f4585`.

Reproduce it against an exact local Git object graph with:

```bash
python3 benchmarks/agent-brain/confirmatory/task_eligibility.py \
  --repo /path/to/entire-cli \
  --base 3ebc57dbb923c0aa6eb53f17384109d189953c6c \
  --head df765ab952185595d65f561f8ccb8036598980a8 \
  --repository-id github.com/entireio/cli \
  --output /tmp/development-task-eligibility-scan-v1.json
cmp /tmp/development-task-eligibility-scan-v1.json \
  benchmarks/agent-brain/confirmatory/development-task-eligibility-scan-v1.json
```

## Three disjoint populations

- `development_optimization` may be used for product and ranking iteration.
- `development_calibration` stays untouched until the candidate is locked and is used only to
  estimate final-contract variance and cross-endpoint dependence.
- `confirmatory_holdout` is represented only by opaque commitments until protocol freeze. Its task ID
  and representative attributes must remain null in the public contract.

`candidate_unopened` and `frozen_unopened` require at least one development-optimization member, at
least 12 active independent calibration tasks, and at least one commitment-only holdout member.
Twelve is a variance-estimation floor, not a powered sample size.

## Overlap and review rules

The owner computes prompt, patch, fix, source-session, related-family, and task-identity overlap
commitments with one random owner-held key using `hmac_sha256_owner_key_v1`. The public contract keeps
only the key receipt and blind commitments. Reusing one key across all three splits permits equality
checks without making low-entropy task material vulnerable to a public hash dictionary. A candidate
population cannot advance until a separately retained receipt confirms that the commitments were
recomputed under the committed key.

The linter rejects duplicate prompt or patch commitments globally and rejects active cross-split fix,
source-session, task-identity, or materially related-family overlap. Explicit related-task edges may
cross splits only after one member is excluded.

Every retained member has one exact review-ledger entry. Approval requires explicit positive checks
for symptom-only wording, absence of answer-bearing file/function/workflow hints, hidden-validation
isolation, and retrieval-query neutrality. Review rationale is retained by commitment rather than
free text so a holdout review cannot leak task content.

## Representative attributes

Development members declare repository, task family, memory-answerable versus corpus-closed-null
status, validation kind, critical-failure class, and cold/warm design. Structural difficulty uses the
frozen `bounded_components_v1` formula; the linter recomputes both score and band. Holdout attributes
remain hidden behind a commitment until opening is authorized.

The schemas are:

- `confirmatory/schemas/task-population-v2.schema.json`
- `confirmatory/schemas/development-task-eligibility-scan-v1.schema.json`
- `schemas/task-review-ledger-v2.schema.json`

Validate a populated pair with:

```bash
python3 benchmarks/agent-brain/confirmatory/task_population.py \
  path/to/task-population-v2.json path/to/task-review-ledger-v2.json
```

No production population is checked in by this slice. Protected queries, private task text, holdout
plaintext, provider calls, and paid execution remain outside its scope.
