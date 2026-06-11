# Facts Eval Evidence Audit

- Status: **PASS**
- Release evidence: **false**
- Claim policy: **proof_required**
- Claim scope: **fixture_contract**
- Facts-vs-raw claimable: **false**
- Fixture facts-vs-raw claimable: **true**
- Tasks hash: `sha256:1111111111111111111111111111111111111111111111111111111111111111`
- Brain manifest hash: `sha256:2222222222222222222222222222222222222222222222222222222222222222`
- Required retrievers: `facts`, `history`, `query`, `raw-sessions`

## Required Claims

| Comparison | Metric | A | B | Winner | Evidence | Claimable |
|---|---|---|---|---|---|---|
| raw_vs_facts | useful_per_1k | raw-sessions | facts | b | proof_labels | True |

## Flags

None.

## Notes

- fixture_contract scope validates the proof-mode auditor mechanics only; it is not production facts-vs-raw release evidence
