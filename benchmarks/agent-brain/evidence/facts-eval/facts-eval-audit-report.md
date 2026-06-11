# Facts Eval Evidence Audit

- Status: **PASS**
- Release evidence: **true**
- Claim policy: **no_release_claim**
- Facts-vs-raw claimable: **false**
- Tasks hash: `unset`
- Brain manifest hash: `unset`
- Required retrievers: none

## Proof Contract

- Comparison: `raw_vs_facts`
- Arms: `raw-sessions` vs `facts`
- Metric: `useful_per_1k`
- Required winner: `b`
- Evidence basis: `proof_labels`
- Required retrievers: `facts`, `history`, `query`, `raw-sessions`
- Include retrieved ids: **true**
- Retain task artifact: **true**

## Flags

None.

## Notes

- no facts-vs-raw release claim is retained; paired proof artifacts are still required before claiming facts beat raw sessions
