# Release Evidence

This directory is the nonignored retention lane for replay-lab evidence that is
safe to cite in release materials.

`mise run release:evidence` writes the independent Codex audit report to a
temporary directory first. Only after the gate passes does it copy
`codex-audit-report.json` and `codex-audit-report.md` here. The task fails unless
the selected release-candidate suites are provenance-complete, audit-clean,
validated by non-empty validation command results, and backed by at least one
stable proof-ready comparison with at least 4 repetitions per side and retained
panel provenance. Historical, exploratory, quarantine, or `release-local-*`
suites under `benchmarks/agent-brain/results/` do not become release evidence
just because they exist; they must be regenerated from a committed panel into
an explicit `release-candidate-*` suite and pass the manifest-enforced gate.

The committed manifest records the citable suite policy. Generated audit reports
should only be committed after the gate passes for a real release-candidate run.
