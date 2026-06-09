# Release Evidence

This directory is the nonignored retention lane for replay-lab evidence that is
safe to cite in release materials.

`mise run release:evidence` writes the independent Codex audit report here and
fails unless the selected release-candidate suites are provenance-complete,
audit-clean, and backed by at least one stable proof-ready comparison. Historical
or quarantine suites under `benchmarks/agent-brain/results/` do not become
release evidence just because they exist; they must be regenerated or copied into
an explicit `release-*` suite and pass the gate.

The committed manifest records the citable suite policy. Generated audit reports
should only be committed after the gate passes for a real release-candidate run.
