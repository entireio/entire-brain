# Upstream PR review for the span-selector experiment

Review snapshot: 2026-09-13. Brain was fast-forwarded from `9f13d344` to merged
main `23101e06002d5e533a123de78b902d3b49217b53`. All 15 pre-existing modified or
untracked files were hash-checked before and after the fast-forward. The experiment
binary includes the existing opt-in retrieval-context export instrumentation;
its SHA-256 is recorded in the frozen protocol. PR bodies, head SHAs, merge status
and changed-file patches are archived locally. Status below is at this snapshot.

| PR | State | What matters here | Applied consequence |
|---|---|---|---|
| [Brain #251](https://github.com/entireio/entire-brain/pull/251) | Merged | A count-preserving duplicate can hide a missing fact. Fact identities are scoped within each branch. | Validate complete ID sets, reject duplicate assessments and conflicting source identities, and test identical IDs on different branches. Current upstream integrity tests pass. |
| [Brain #252](https://github.com/entireio/entire-brain/pull/252) | Merged | Repository identity must not depend on a symlinked spelling. | Rerun retrieval on the new binary. Span anchors bind branch, source identity, full source hash and byte offsets rather than a local absolute path. The new resolver required an unsandboxed run to access a Windows temporary repository. |
| [Brain #250](https://github.com/entireio/entire-brain/pull/250) | Merged | Missing host CLI and deleted indexes must be reported accurately. | Build and retrieve against current main; distinguish availability of supplied sources from completeness of retrieval. This experiment does not claim to validate the entire host/provider fallback path. |
| [Brain #246](https://github.com/entireio/entire-brain/pull/246) | Merged, already in prior baseline | Missing/corrupt stores and invalid history coverage must remain visible. | Preserve an explicit retrieval-state input; unavailable inputs cannot produce evidence. The fixture lane supplies available sources; a unit test covers the unavailable adapter state. Production warning parsing is not implemented by this component. |
| [Brain #253](https://github.com/entireio/entire-brain/pull/253) | Open | Trimming can leave pagination counts false, corrupt a bare-array record with metadata, and return non-monotonic coverage. | Derive packet counts and omitted IDs from the actual returned ID set. Keep packing metadata separate from source records. Tests check the byte ceiling and returned counts. This is a harness contract, not a claim that the open MCP patch is running. |
| [Brain #249](https://github.com/entireio/entire-brain/pull/249) | Open | Bundle export can lose layers; sessions and their projections need consistent fingerprints. | Retain original sources, source hashes, catalogues and outputs directly as local experiment artifacts. Do not use a semantic-only bundle as a backup or evaluate bundle restoration as though this proposal were merged. |
| [Graph #248](https://github.com/entireio/entire-graph/pull/248) | Merged | Re-ranking cannot recover candidates missed at retrieval; prose queries need recall fixes as well as ranking fixes. | Rerun actual Brain raw/fact retrieval, retain candidate pools, and add a deterministic block control using the same source universe. The selector operates after retrieval. This run does not invoke Graph search and cannot validate the PR's code-search improvement. |
| [Graph #233](https://github.com/entireio/entire-graph/pull/233) | Open | Proposed evidence state separates heuristic relationships and unresolved relationships from stronger results. | Keep model relationship judgments separate from exact anchor validation. Corroboration never becomes a mandatory dependency merely because the text is related. No production Graph evidence-state field is assumed. |
| [Graph #250](https://github.com/entireio/entire-graph/pull/250) | Open | Proposed source-file health accounting measures unique eligible files and exposes parser gaps. | Limit completeness claims to supplied source blocks; zero selected blocks is not proof that the repository contains no evidence. Snapshot health/schema 1.3 is not treated as a merged contract. |
| [CLI #2342](https://github.com/entireio/cli/pull/2342) | Merged | Checkpoint fetch must honor the selected remote. | New session sources are explicitly retained local transcript snapshots. This component does not silently substitute a checkpoint remote or claim an export-path test. |
| [CLI #2399](https://github.com/entireio/cli/pull/2399) | Open | Quoted/non-ASCII staged paths can break checkpoint attribution. | Preserve exact UTF-8 bytes and test Kannada, accented text, emoji and CRLF offsets. These tests concern evidence anchors, not a fix to the CLI's staged-path parser. |
| [CLI #2403](https://github.com/entireio/cli/pull/2403) | Open | A case-folded checkpoint shard directory can hide an intact checkpoint. | Do not infer evidence absence from a failed checkpoint lookup. The session pilot reads retained local transcripts and does not exercise ref discovery. Content identities remain independent of host path casing. |

The source review of Graph #233 also found that its proposed `RelationEvidenceState`
defaults an unknown resolution to `confirmed`, and an empty worst-state fold starts
at `confirmed`; individual consumers are expected to escalate gaps. Those defaults
are not a semantic truth check. This selector retains `unknown` judgments and treats
an exact anchor as proof of bytes only.

The new experiment is local Python component work, with fresh product retrieval
from the pinned Brain build. It does not integrate a new MCP schema, alter production
fact states, or constitute a review/approval of every open PR. Open PRs contributed
test cases and integration constraints; their implementation claims remain proposals.
