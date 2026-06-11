# Facts Eval Proof Fixture

This directory is a fixture-only retained evidence lane for the facts-vs-raw
auditor. It proves that proof-mode artifacts with the canonical retrievers
(`facts`, `history`, `query`, and `raw-sessions`), shared task/brain hashes,
proof labels, paired rows, and a facts-over-raw useful-per-1k win pass the
auditor end to end.

It is deliberately marked with:

```json
{
  "claim_policy": "proof_required",
  "claim_scope": "fixture_contract"
}
```

That scope means this is not production release evidence and must not be cited as
a real facts-beat-raw claim. The production release lane remains
`../facts-eval/`, which currently retains `claim_policy: "no_release_claim"`
because this checkout has no active durable facts. Real release proof must
replace that no-claim lane with target-corpus, proof-labeled paired eval runs.
