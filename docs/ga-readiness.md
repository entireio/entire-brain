# P0 GA readiness — entire-sem + entire-brain + brain-bench

A single, cross-repo checklist for the P0 General Availability of the local brain:
the semantic provider (`entire-sem`), the brain (`entire-brain`), and the eval
harness (`brain-bench`). The standard here matches `docs/release-blockers.md` and
`docs/release_readiness_audit.md`: explicit claims, exact evidence, no euphemism.

## GA gate — everything below must hold on one aligned release SHA

| # | Gate | State | Evidence |
|---|------|-------|----------|
| 1 | `entire-sem` module path is the org namespace | ✅ done | `go.mod` = `github.com/entireio/entire-sem` (migrated) |
| 2 | `entire-sem` version stamped from a real tag | ✅ done | tag `v0.1.0`; `scripts/release.sh` injects `-X main.version=$(git describe --tags)` |
| 3 | Provider schema is a frozen, pinnable GA contract | ✅ done | ADR 0001 — schema `1.x` (minor `1.1`), additive-only, tolerant readers |
| 4 | Brain pins the provider at the live repo | ✅ done | `entire-plugin.yml` `repo_url` → `github.com/entireio/entire-sem` |
| 5 | No-egress + deterministic golden coverage | ✅ exists | `doctor` `no_egress:true`; golden/provider/quality tests green (GA acceptance bar) |
| 6 | `brain_*` MCP tool contract locked | ✅ exists | schemas `additionalProperties:false`, unknown-key rejection, named-tool auditability |
| 7 | brain-bench wired as a regression gate | ✅ done | `brain-bench` CI: self-tests + significance no-drift; floor = 28/30 languages, 1148 vs 802 |
| 8 | Phase-1 local-only boundary frozen as a guardrail | ✅ exists | stdio MCP only; `ENTIRE_BRAIN_NO_EGRESS` / `LOCAL_ONLY` documented |
| 9 | Release blockers B1 + R1–R4 confirmed closed on RC SHA | ⏳ verify | re-run `mise run release:readiness` on the tagged SHA; see `docs/release-blockers.md` |
| 10 | Tagged `main` on `entire-sem` **and** `entire-brain`, aligned | ⏳ partial | `entire-sem` `v0.1.0` cut; `entire-brain` tag pending (this SHA) |
| 11 | `no_release_claim` items resolved (proof or scoped-out) | ✅ resolved | see "Claim posture" below |

## Compatibility matrix

| Producer | Contract | Consumer | Rule |
|---|---|---|---|
| `entire-sem` schema | `1.x` (ADR 0001) | `entire-brain` ingestion | accept any `1.x`, ignore unknown fields, warn on newer minor |
| `entire-sem` provider | `provider_version` in manifest header | `entire-brain` refresh | recorded, not gated; used for provenance |
| `brain-bench` harness | committed `*.score.json` + `stats_significance.py` | CI regression gate | significance report must regenerate byte-identical; corpus/harness changes must re-commit it |

Pinning guidance: brain consumers may pin the provider `>=1.0 <2.0`; a `2.0` is a
breaking change with a migration note and is out of the P0 `1.x` line.

## Claim posture (honest — never overclaimed)

- **Semantic usefulness — CLAIMABLE.** Backed by `brain-bench`: Entire beats the
  comparison baseline on **28 of 30 top languages** at exact-McNemar p<0.05
  (C# p=0.07 and PHP p=0.18 lead on rate but sit just under significance — more
  scored repos, not a loss), overall **1,148 vs 802** correct (91% vs 64%,
  p≈0), with Benjamini-Hochberg multiple-comparisons control. This is the first
  broad, reproducible, in-repo-gated proof and supersedes the earlier single
  narrow condition-level result.
- **Distill wall-clock (>24h target-repo timing) — NO CLAIM.** Only ~2× local
  proof (`audit_distill_perf.py`) exists; the >24h target-repo evidence was never
  collected. GA makes **no distill-speed claim**; scoped out until measured.
- **Durable-facts-vs-raw — NO CLAIM.** The four-arm harness exists but there are
  **zero active durable facts**, so there is no paired proof. Facts ship as a
  capability with **no comparative-quality claim** until seeded facts + paired
  evidence land.

These match `docs/release_readiness_audit.md`; nothing here is asserted beyond its
evidence.

## Distribution

- **P0 channel: signed release archives.** `entire-sem/scripts/release.sh` emits
  per-OS/arch tarballs under `dist/` with `SHA256SUMS`; that is the GA artifact.
  Install is via the Entire CLI plugin flow pinned at the tagged release.
- **Marketplace / plugin-discovery listing: out of scope for P0**, tracked as a
  post-GA follow-up.

## Remaining to cut GA (owner action)

1. Re-run `mise run release:readiness` on the RC SHA and paste the 0-hard-flags
   result here (gate 9).
2. Tag `entire-brain` `v0.1.0` on the same aligned SHA (gate 10); `entire-sem` is
   already at `v0.1.0`.
3. Reconcile the brain stdio MCP server with the CLI's hidden `entire mcp` stdio
   server (composition currently referenced-not-verified) or document them as
   intentionally separate.
