# Confirmatory benchmark preparation

This directory contains the unpaid, preparatory Workstream 6 artifacts. It does **not** authorize a
paid agent run and it does not contain or open a fresh confirmatory holdout.

Current state: `integrated_methodology_draft_power_redesign_required`. The WS2 treatment contract,
WS3 pre-ranking temporal filter, WS4 scheduling/cache controls, and WS5 evidence controls have been
integrated and verified by content-addressed source and test evidence. The exposed-only offline
development relevance set now contains 14 queries over 13 tasks, meets the 12-answerable-product-task
floor, and includes one corpus-closed product null. A controlled three-engine diagnostic run has
produced a checker-valid manifest, but its 602 MB byte-complete v3 bundle contains private
corpus/session/host material and is explicitly non-publishable. A privacy-safe public v4 projector
and fail-closed checker lane are implemented; the retained real v3 run projects locally to a
checker-valid six-file, sub-megabyte public bundle. External publication, restricted exact-byte
attestation storage, trust-root approval, and clean hydration are still pending. The v2 two-root
storage/attestation profile, deterministic public packager, offline verifier, and atomic hydrator are
implemented but checked in as `pending_owner_authorization`; production-mode v4 validation
deliberately fails without authenticated restricted replay. No development-threshold metrics have
been selected. `offline_relevance_eval.py` can compute deterministic development diagnostics from
text-free ranked IDs, but evaluator v1 is deliberately `diagnostic_unattested`: its strict report
schema forces every authoritative threshold pass to `false` and every selection to
`non_authoritative` until a later contract independently verifies retained producer/policy bytes,
an authenticated all-top-K fact-metadata catalog, authoritative engine evidence, and the reviewed
source contract. The relevance holdout remains unopened and empty. Final freeze remains blocked on
fresh-task validity review, approved retention of public v4 plus restricted attestation and
development-threshold selection,
relevance holdout sealing, a defensible powered design, and model pricing/budget approval. The
confirmatory analyzer is implemented and path-locked.

## Artifacts

- `PREREGISTRATION.md` and `preregistration.json`: human- and machine-readable candidate protocol.
- `task-inventory.json`: content-addressed reconciliation of all 46 unique C0701 tasks.
- `schemas/task-inventory.schema.json`: task exposure/contamination contract.
- `development-task-eligibility-scan-v1.json`, `development-task-negative-control-v1.json`, and
  `development-task-symptom-review-v1.json`: permanently development-only local task inventory,
  reverse-patch classifications, and 17 exact independently audited symptom prompts. The last
  artifact binds source/test/negative-control commitments and records fix/family/session-overlap
  decisions, but explicitly has no owner HMAC receipt, population assignment, calibration
  membership, holdout membership, or run authority. `development_task_symptom_review.py` and its
  schema/test provide the fail-closed build and exact-source verification workflow.
- `MULTI-REPOSITORY-DEVELOPMENT-INVENTORY-V2.md`, `task_eligibility_v2.py`, the three
  `development-task-eligibility-*-v2.json` ledgers, and `development-task-overlap-registry-v1.json`:
  repository/ref/window/module/Git-and-build-toolchain-bound structural inventories for 62 additional permanently
  development-only candidates plus a global 85-identity exact-overlap registry. Production Go and
  test evidence are disjoint, single-parent and two-parent units are supported, fixture-only evidence
  binds its owning test package, and source/test/full stable patch IDs are recorded under one
  canonical identity profile shared with the 23 CLI v1 registry entries. All 62 are only
  `structurally_ready_not_executed`; there is no negative-control result or run authority.
- `MULTI-REPOSITORY-NEGATIVE-CONTROL-RUN-PLAN-V2.md`,
  `development-task-negative-control-run-plan-v2.json`, `task_negative_control_plan_v2.py`, and
  `schemas/development-task-negative-control-run-plan-v2.schema.json`: deterministic exact-input
  plan/check lane for those 62 candidates. The checked plan is unexecutable
  `pending_owner_authorization`. It froze the approval trust mechanism, offline cache seed, executor,
  classifier, private-log writer, and resource/cleanup attestation as absent when authored. Pure
  gate, private-log, and classification primitives now exist, but none changes the checked plan's
  authority or supplies executor attestation. Building or checking the plan invokes no candidate
  process.
- `MULTI-REPOSITORY-NEGATIVE-CONTROL-GATE-PRIMITIVES-V1.md`,
  `task_negative_control_gate_v1.py`, and the adjacent manifest/receipt schemas: pure fail-closed
  cache-identity and injected resource-arithmetic primitives. They pin the exact plan/dependency
  rebuild, complete Go/native/Git toolchains, runtime/verifier sources, checked schema bytes, and a
  bounded 250,000-file/128 MiB portable-path-safe manifest profile, but explicitly do not verify an
  actual archive, observe/reserve host disk, authorize execution, or expose an executor.
- `MULTI-REPOSITORY-NEGATIVE-CONTROL-DARWIN-CAPACITY-V1.md`,
  `negative_control_darwin_capacity_v1.swift`, and its schema/test: a live but explicitly unattested
  Foundation observation of immediately free, important-use, and opportunistic volume capacity. It
  corrects the APFS `df` interpretation without recording the supplied path or adding reservation,
  cleanup, authority, or execution.
- `NEGATIVE-CONTROL-PRIVATE-RAW-LOG-V1.md`, `negative_control_private_log.py`, and
  `schemas/negative-control-private-log-receipt-v1.schema.json`: content-addressed private raw-log
  storage/check primitive with bounded streaming, exact public two-field receipts, private modes,
  safe descriptor-relative operations, aggregate limits, and atomic publication. It is not wired to
  an executor and provides no retention, cleanup, or execution attestation by itself.
- `MULTI-REPOSITORY-NEGATIVE-CONTROL-CLASSIFICATION-V1.md`,
  `task_negative_control_classification_v1.py`, and the attempt/receipt schemas: pure fail-closed
  validation and seven-class truth-table derivation over exactly 248 injected, unattested attempts
  for all 62 candidates. The schemas pin all attempt, result, and repository positions; public rows
  expose only status/exit and content-addressed log-receipt metadata. The receipt remains explicitly
  unexecutable and does not claim the attempts ran.
- `MULTI-REPOSITORY-NEGATIVE-CONTROL-EXECUTION-CONTRACT-V1.md`,
  `negative_control_execution_contract_v1.py`, its checked artifact, and schema/test: a deterministic
  build/check-only projection of the exact 62 candidates and 248-attempt order into a future-executor
  contract. It binds the current gate/log/classifier/APFS-observer components plus first-parent
  reversal and cleanup-state requirements, while every approval, cache, host, reservation, producer,
  private-root, and cleanup runtime binding remains null. It has no candidate or subprocess surface
  and remains `contract_compiled_execution_forbidden`.
- `MULTI-REPOSITORY-NEGATIVE-CONTROL-OWNER-APPROVAL-V1.md`,
  `negative_control_owner_approval_v1.py`, the checked verifier/trust-root artifacts, and their
  schemas/test: a deterministic build/check/verify-only SSHSIG approval gate bound to the exact
  execution contract. The checked trust-root set is pending and empty, so production verification
  fails before reading an approval or invoking `/usr/bin/ssh-keygen`. A future valid owner signature
  can produce only a non-persistent, non-authorizing report: execution and atomic consumption remain
  forbidden. The module has no signing, key-generation, trust-root installation, approval issuance,
  candidate execution, provider, or network lane.
- `MULTI-REPOSITORY-NEGATIVE-CONTROL-CACHE-ARCHIVE-V1.md`,
  `negative_control_cache_archive_v1.py`, the pending material declaration, verifier contract, and
  three adjacent schemas/test: a deterministic build/check/verify-only raw-identity gate for the
  future offline Go cache seed. The checked declaration has no archive, manifest, inventory, or
  count identities, so production verification fails before reading caller locators. Even a future
  source-reviewed match treats the archive as opaque bytes and leaves content safety, extraction,
  staging, E0 runtime binding, owner approval, atomic consumption, candidate execution, network,
  model/provider work, and paid work forbidden.
- `schemas/relevance-dataset.schema.json`: generated offline relevance dataset and sealed-holdout
  contract; the adjacent relevance schemas cover reviewed labels, retained review evidence, the fact
  excerpt, and the complete source-membership contract.
- `OFFLINE-RELEVANCE-DEVELOPMENT-2026-07-15.md`: counts, contamination boundary, retained evidence
  binding, reproducible commands, and exact remaining count/threshold/holdout blockers.
- `offline-relevance-development-labels.json`: reviewed exposed-task label source.
- `offline-relevance-review-ledger.json`: retained per-query/per-judgment manual review evidence for
  all 14 queries; no label claims unavailable historical packet bytes as evidence.
- `offline-relevance-source-membership.json`: complete authenticated catalog of 2,621 facts (2,531
  active), their canonical provenance-session IDs, and 2,248 session dates; it derives exact
  active+eligible catalogs for each task policy.
- `relevance-source-contract.json`: small reviewed contract for the complete membership catalog; its
  raw digest is pinned independently in `check_protocol.py`, outside routine preregistration hashes.
- `offline-relevance-null-review-ledger.json` and `relevance-null-review-contract.json`: retain the
  exhaustive 2,531-decision review for `dev-product-b72a6e621` and its separately
  hard-pinned contract: four hard topical distractors, 2,527 irrelevant facts, and zero positives.
- `offline-relevance-fact-snapshot.json`: content-addressed 44-fact/39-date excerpt of the pinned
  full quarantine, sufficient for offline rebuild and validation.
- `relevance_dataset.py` and `test_relevance_dataset.py`: exposed-only proposal, snapshot,
  materialization, and fail-closed validation workflow.
- `offline-relevance-dataset.json`: generated 14-query/13-task development set with 46 judgments and
  one corpus-closed null; its sealed holdout intentionally contains no plaintext labels.
- `offline_relevance_eval.py`, `test_offline_relevance_eval.py`, and the three
  `schemas/offline-relevance-*-v1.schema.json` files: deterministic, text-free offline scoring with
  an explicit fail-closed `diagnostic_unattested` authority state. Diagnostic threshold conditions
  and best candidates remain visible, while v1 cannot emit an authoritative pass or selected
  winner from declared producer, metadata, or engine identities.
- `DEVELOPMENT-RELEVANCE-QUERY-FIXTURE.md`, `development_relevance_queries.py`, and
  the two `schemas/development-relevance-*-v1.schema.json` files: fixed-path private query and
  temporal-policy receipt contracts, machine-pinned source trust, symlink-safe fail-closed loader,
  and synthetic tests. Both ignored private artifacts and the temporal receipt's reviewed raw-hash
  pin are intentionally absent pending an authorized safe export.
- `engine-matrix.json` and `ENGINE-VERIFICATION-RUNBOOK.md`: exact named arms, public v4 authority,
  legacy diagnostic schema binding, and verification/projection steps.
- `engine-verification-pins.json`: gate-authoritative corpus, query, reproducible binary build,
  GGUF, resolved Node, server script, package/lockfile, health interval, and dependency-inventory
  expectations, including exact eligible and active+eligible semantic-candidate set commitments;
  the runner does not accept these values from its caller. `preregistration.json` binds this exact
  canonical repo-relative path and its raw SHA-256, so the protocol content hash necessarily freezes
  the selected production pin bytes rather than whichever pin file happens to be present later.
- `schemas/engine-verification.schema.json` and
  `schemas/engine-verification-manifest.schema.json`: restricted diagnostic per-arm output and exact
  three-record legacy wrapper.
- `schemas/engine-verification-public-v4.schema.json`, `public_engine_evidence.py`, and
  `test_public_engine_evidence.py`: authoritative public manifest, atomic privacy-safe projector,
  recursive scanner, exact file inventory, independent temporal/result/vector/lifecycle checker,
  and adversarial mutation coverage. The public projection keeps cutoff-relative session states and
  ordinal lifecycle evidence, not exact session/run times; names actual safe inherited keys but no
  environment values; and states that network isolation was not enforced. No encryption,
  publication, or key-management action is performed by this lane.
- `ENGINE-REPLAY-ATTESTATION-CONTRACT.md`, `engine-evidence-storage.json`,
  `restricted_replay_attestation.py`, and `hydrate_engine_evidence.py`: pending-only v2 two-root
  public/restricted storage contract, exact 12-source checker lock, empty pending trust-root set,
  canonical Ed25519 SSHSIG envelope, deterministic public tar-zstd packaging, and atomic offline
  hydration/verification. No production key, signer, restricted object, or publication is selected.
  `engine-evidence-storage-legacy-v1.json` is preserved as privacy-failed diagnostic-only input and
  cannot satisfy production.
- `verify_engines.py`: fail-closed retained runner with isolated arms, owned-server continuity
  attestation overlapping live recall, byte-complete artifacts, and
  checker-before-atomic-publication semantics.
- `ENGINE-PROBE-READINESS-2026-07-15.md`: unpaid controlled three-engine verification result and
  the remaining durable repository-retention blocker; the external local bundle is not freeze
  evidence until its exact bytes are retained through the approved artifact strategy.
- `power_analysis.py`, `power-analysis.json`, and `POWER-DESIGN-OPTIONS-2026-07-15.md`: deterministic
  unpaid v3 power contract for time, normalized cost, and code quality. All three remain
  uncalibrated under the final runner/treatment contract, so numeric power and both development and
  confirmatory power-sized task counts are intentionally null. Twelve tasks is only the minimum
  calibration floor; the current 24-task x 4-repetition design is not approved. Its placeholder
  ceiling is 288 agent-CLI invocations/cell attempts because retries, replacements, and reserves
  are frozen at zero. Claim floors and strictly better owner-frozen planning alternatives are
  separate; the alternatives and all numeric powers remain null while approval/calibration is open.
- `power_analysis_v4.py`, `schemas/final-calibration-v1.schema.json`,
  `schemas/power-analysis-v4.schema.json`, and `FINAL-CALIBRATION-POWER-V4.md`: standalone four-arm
  development-calibration and power-sizing lane. It validates raw product-cycle, task-population,
  review-ledger, and receipt files under pinned Draft 2020-12 schemas; derives full
  family/fix/session/material-edge closure; rejects related or duplicate calibration tasks; and
  binds a complete pre-open plan, per-cell execution attestations, and a dedicated v4
  analyzer/schema lock. The production plan fixes 10,000 resamples, seed, exhaustive grid, and both
  contrasts' alternatives before opening. Shared cluster draws preserve dependence and require
  every marginal plus direct same-draw joint power to reach 0.80. No authentication trust root is
  configured, so pending, candidate, synthetic, and frozen development evidence all remain
  nonpassing. This lane does not modify the locked v3 artifact, authorize budget, call a
  provider/model, or open holdout plaintext.
- `power-calibration-exploratory-v1.json`: content-addressed manifest for sparse retained legacy
  outcomes. The derived diagnostics are quarantined from confirmatory assumptions and cannot pass
  the power gate.
- `pricing-budget.json`, `pricing_budget.py`, and `schemas/pricing-budget.schema.json`: provider-neutral
  pricing quote, five-category token-envelope arithmetic (including cache-write and reasoning),
  staleness, and explicit budget-approval contract. All
  human/model/price fields remain pending. `PRICING-BUDGET-READINESS-2026-07-15.md` lists the exact
  decisions and evidence needed to close the two budget gates.
- `go-no-go.json`: paid-run gate. Every item must be `pass`; `pending` is a hard no-go.
- `integration-verification.json` and `integration-logs/`: exact WS2-WS5 source commits, content
  hashes, unpaid commands, and captured test outputs for the locked integration commit.
- `analyzer-lock.json`: path-bound hash of the exact confirmatory analyzer sources and schemas.
- `check_protocol.py`: offline integrity checker. It validates the independently pinned complete
  fact/date membership and per-query ledger bytes, validates the versioned relevance artifact and
  schema hashes, applies every checked-in relevance schema, and rematerializes the dataset. `--freeze`
  additionally enforces final-freeze gates, including the development task and null-query floors.

Run the non-paid checks with:

```sh
python3 benchmarks/agent-brain/confirmatory/check_protocol.py
python3 -m unittest benchmarks/agent-brain/confirmatory/test_check_protocol.py
python3 benchmarks/agent-brain/confirmatory/relevance_dataset.py validate \
  --labels benchmarks/agent-brain/confirmatory/offline-relevance-development-labels.json \
  --inventory benchmarks/agent-brain/confirmatory/task-inventory.json \
  --snapshot benchmarks/agent-brain/confirmatory/offline-relevance-fact-snapshot.json \
  --dataset benchmarks/agent-brain/confirmatory/offline-relevance-dataset.json \
  --source-contract benchmarks/agent-brain/confirmatory/relevance-source-contract.json \
  --source-membership benchmarks/agent-brain/confirmatory/offline-relevance-source-membership.json \
  --review-ledger benchmarks/agent-brain/confirmatory/offline-relevance-review-ledger.json \
  --null-review-ledger benchmarks/agent-brain/confirmatory/offline-relevance-null-review-ledger.json \
  --null-review-contract benchmarks/agent-brain/confirmatory/relevance-null-review-contract.json
python3 -m unittest benchmarks/agent-brain/confirmatory/test_relevance_dataset.py
```

The preparation and relevance validation commands must pass now. The following command must fail
until fresh-task review and three-engine development evaluation/threshold selection are complete, the
relevance holdout is sealed, the powered design is repaired, final pricing/budget is approved, and
durable engine verification exists:

```sh
python3 benchmarks/agent-brain/confirmatory/check_protocol.py --freeze
```
