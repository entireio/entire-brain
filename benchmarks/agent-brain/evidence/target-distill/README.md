# Target Distill Evidence

This directory tracks the specific frontend/large-session-repo distill
performance claim Thomas raised. It is intentionally separate from
`evidence/distill-perf`, which currently proves only the current-repo local
scheduler mechanics.

The committed manifest is `claim_policy: "no_release_claim"` because no retained
target dry-run or paired timed frontend artifacts exist in this checkout. Do not
reuse current-repo distill evidence to claim the frontend 24h backfill issue is
fixed.

To promote this lane later, replace the no-claim manifest with
`claim_policy: "proof_required"` and point `distill_report` at a passing
`audit_distill_perf.py` report collected from the target repo. The target repo
and claim scope in this manifest must exactly match that report.
