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
content commitments but no subjects, paths, prompts, or patches. It is only a static pre-screen. The
checked-in development receipt closes the reverse-patch negative-control filter. The companion
development symptom ledger records the subsequent independent wording review, but remains
non-authoritative because owner-key and complete source-session receipts are absent. Any scan whose
identities, patches, tests, or prompt text were inspected is permanently labeled development-only
and cannot be reassigned to calibration or confirmatory holdout.

The checked-in development scan covers `github.com/entireio/cli` from
`3ebc57dbb923c0aa6eb53f17384109d189953c6c` through
`df765ab952185595d65f561f8ccb8036598980a8`. It records 31 first-parent integration units and 23
source-plus-test candidates: 7 low, 3 medium, and 13 high static-scope bands. This remains an
inventory milestone, not an eligible task population. Its canonical self-hash is
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

The separate v2 development inventory extends structural scanning to pinned 31-unit windows for
`github.com/entireio/entire-brain`, `github.com/entirehq/entiredb`, and
`github.com/entireio/entire-graph`. It records 62 additional candidates: 20 low, 14 medium, and 28
high. Production Go is `.go AND NOT test_evidence`, preventing fixture/helper Go files from entering
the reversal set. Single-parent units bind the integration commit as their source lineage;
two-parent units bind the feature-branch lineage. Changed Go tests bind their package targets, while
fixture/helper-only evidence binds the nearest ancestor package with `_test.go` files. All Git reads
disable replacement objects and lazy fetch internally; any replace ref, graft file, or shallow
boundary fails the scan. Object diffs pin
attribute lookup to the candidate commit, while any nonempty repository `info/attributes` file
or effective `diff` attribute on a diffed path fails closed.

The v2 ledgers bind exact remote/ref/pinned-head/base/window, require the pinned head to remain an
ancestor of the authoritative remote tip, and bind candidate-tree module/workspace manifests,
exact Git and repository-specific Go/native toolchains, test-evidence kinds, target commitments,
diff hashes, and stable patch IDs. They contain no prompt or patch bytes. Their 62 identities are permanently
development-only, all source sessions remain unresolved, and no owner key, split, population, or run
authority exists. The global registry combines them with the 23 CLI v1 identities and reports zero
exact duplicate groups across commit, tree, source/test/full diff, and source/test/full stable patch
ID under one canonical full-index profile. CLI v1 legacy diff hashes remain labeled separately and
are not used as cross-repository identity dimensions. Repeated path sets are diagnostic only and do
not resolve semantic or family overlap. See
`MULTI-REPOSITORY-DEVELOPMENT-INVENTORY-V2.md` for exact counts and reproduction commands. The
separate checked v2 negative-control plan binds all 62 candidates but is
`pending_owner_authorization`: no approval trust mechanism, cache-seed manifest, executor,
classifier, or private-log writer exists, and no v2 candidate has been executed. See
`MULTI-REPOSITORY-NEGATIVE-CONTROL-RUN-PLAN-V2.md`.

`task_negative_control.py` is the next development-only filter. For each statically screened
candidate, it runs the packages containing changed Go tests from a clean detached candidate worktree,
then creates a second clean detached worktree, reverses only the production Go patch against the first
parent, and reruns the identical command. Hooks are disabled, and HEAD, index-tree identity, and clean
status are checked before either run. A candidate advances only to independent symptom review when the
baseline passes and the reversed-source run fails. A passing reversal, baseline failure, or a
harness-owned process-group timeout cannot advance. Receipts retain only hashes, counts, exit
classifications, and exact runner/dependency/environment/policy identities; raw paths, subjects,
patches, and test output are not checked in. The Go test timeout is disabled so it cannot masquerade as
a causal failure; one outer deadline kills the whole test process group. The receipt pins Go 1.26.4,
and each clean worktree must pass a module preflight under that exact toolchain before either run.

The checked-in development-only receipt classifies all 23 candidates: 17 advance to independent
symptom review, one is rejected because the negative control survived, four are rejected because the
baseline failed, and one is rejected because the reversed-source run hit the harness deadline. This
receipt does not approve prompts or assign a population split. Its canonical self-hash is
`c71423e9cb8c88d6749cd75ecf76651d1d0b59b72ef24fc9bd8071676a81032c`; the checked-in file SHA-256 is
`bed85eebcf71f7b256646e2bbbcf6da2b843d7aad7d9869a48f19f262bcd922d`.

Run and validate this local filter with:

```bash
python3 benchmarks/agent-brain/confirmatory/task_negative_control.py run \
  --repo /path/to/entire-cli \
  --ledger benchmarks/agent-brain/confirmatory/development-task-eligibility-scan-v1.json \
  --output /tmp/development-task-negative-control-v1.json
python3 benchmarks/agent-brain/confirmatory/task_negative_control.py check \
  /tmp/development-task-negative-control-v1.json
```

This filter is deliberately narrower than a full task validation: it does not detect dependent
packages outside the changed-test package set, judge whether a prompt is symptom-only, or approve a
population split. Those remain separate fail-closed review gates.

### Development-only symptom review

`development_task_symptom_review.py` consumes exactly the 17
`eligible_for_symptom_review` results in the checked-in negative-control receipt. It fails if the
four baseline-invalid results, the surviving negative control, or the reversed-source timeout enter
the review. Each retained row binds the eligibility and negative-control file/self hashes, exact
commit/parent/tree identities, source and test diff commitments, test-command and negative-control
output commitments, and the domain-separated SHA-256 of the exact prompt text.

An accepted development prompt requires two distinct review records: the prompt author and a
separate read-only auditor who compares that prompt with the local patch and changed tests. Both
must positively record symptom-only scope and the absence of fix terms, file/function hints,
command or workflow hints, hidden-test details, patch leakage, and other answer-bearing context.
Static checks reject path, identifier, command, and high-signal implementation syntax, but do not
replace the semantic audit. Review-record hashes and the ledger self-hash bind the decisions; they
are integrity commitments, not signatures or owner receipts.

The ledger also records development fix-lineage sets and cleartext semantic family labels so exact
prompt, patch, fix, and family overlap is visible during development. These are deliberately not
substitutes for the owner-held HMAC commitments required by task-population v2. Local commit trailers
do not establish complete source-session identity, so every source-session and authoritative overlap
decision remains explicitly unresolved. The validator rejects any claim of population membership,
calibration membership, holdout opening, owner-key verification, or benchmark-run authorization.

Build a reviewed draft and then validate its exact local Git bindings with:

```bash
python3 benchmarks/agent-brain/confirmatory/development_task_symptom_review.py build \
  --repo /path/to/entire-cli \
  --ledger benchmarks/agent-brain/confirmatory/development-task-eligibility-scan-v1.json \
  --negative-control benchmarks/agent-brain/confirmatory/development-task-negative-control-v1.json \
  --draft /path/to/independently-audited-development-draft.json \
  --output benchmarks/agent-brain/confirmatory/development-task-symptom-review-v1.json
python3 benchmarks/agent-brain/confirmatory/development_task_symptom_review.py check \
  --repo /path/to/entire-cli \
  --ledger benchmarks/agent-brain/confirmatory/development-task-eligibility-scan-v1.json \
  --negative-control benchmarks/agent-brain/confirmatory/development-task-negative-control-v1.json \
  benchmarks/agent-brain/confirmatory/development-task-symptom-review-v1.json
```

The build command is not an approval mechanism: it only seals already recorded reviewer decisions.
It runs no model, task test, paid benchmark, provider call, calibration procedure, or holdout action.
The checked-in review contains exactly 17 accepted development-only prompts and no rejected rows;
all 17 source-session decisions remain unresolved and authoritative population membership remains
zero. Its canonical self-hash is
`69fb0e5c68fb62cdc4b313c8d709c32f4a68aaba1234584ba1a5978fba89ae15`; the checked-in file SHA-256 is
`e3d2d07d71bed97926c871441f59e725a358715aab449e0e6806b5a9007c6dcc`.

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
- `confirmatory/schemas/development-task-eligibility-scan-v2.schema.json`
- `confirmatory/schemas/development-task-overlap-registry-v1.schema.json`
- `confirmatory/schemas/development-task-negative-control-v1.schema.json`
- `confirmatory/schemas/development-task-negative-control-run-plan-v2.schema.json`
- `confirmatory/schemas/development-task-symptom-review-v1.schema.json`
- `schemas/task-review-ledger-v2.schema.json`

Validate a populated pair with:

```bash
python3 benchmarks/agent-brain/confirmatory/task_population.py \
  path/to/task-population-v2.json path/to/task-review-ledger-v2.json
```

No production population is checked in by this slice. Protected queries, private task text, holdout
plaintext, provider calls, and paid execution remain outside its scope.
