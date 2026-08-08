# Compact v3 local packet measurement

This is development-only evidence for the opt-in MCP `brain_brief`
`compact_v3` serializer. It compares v3 with `compact_v2` on 25 deterministic
public/synthetic Go fixtures. It does not use the private 114-task corpus,
launch an agent, call a provider, measure code quality, authorize a paid trial,
or change the MCP default.

## Contract under test

V3 preserves compact v2's record schemas, body grammar, typed solving
projection, and deterministic strictly-shorter family selection. Its deliberate
wire changes are:

1. the marker is `entire.brain_brief c3`;
2. the existing exact repeat reference `^` is allowed in every column, not only
   the v2 metadata allowlist; and
3. the SHA-256 footer uses canonical unpadded base64url instead of lowercase
   hexadecimal.

`legacy_json` remains the default. Tests independently expand each v3 packet
back to compact v1's full typed projection, verify the checksum and canonical
encoding, and require that no fixture grows relative to v2.

## Local exact-token result

Measured on 2026-07-17 with `o200k_base`, using `tiktoken-go` v0.1.8 and a
local-only 3,613,922-byte merge-rank asset with SHA-256
`446a9538cb6c348e3516120d7c08b09f57c36495e2acfffe59a5bf8b0cfb1a2d`.
The asset, measurement harness, and tokenizer dependency are not distributed
or added to this repository.

| Fixture | v2 bytes | v3 bytes | v2 o200k | v3 o200k |
| --- | ---: | ---: | ---: | ---: |
| minimal | 295 | 266 | 90 | 84 |
| comprehensive_v1 | 4,593 | 4,564 | 1,180 | 1,171 |
| exhaustive_v2 | 6,214 | 5,467 | 2,022 | 1,893 |
| sparse_1 | 474 | 445 | 135 | 126 |
| sparse_2 | 612 | 583 | 185 | 178 |
| sparse_3 | 739 | 710 | 230 | 216 |
| repeated_02 | 6,214 | 5,467 | 2,022 | 1,893 |
| repeated_03 | 7,444 | 5,951 | 2,629 | 2,379 |
| repeated_04 | 8,661 | 6,436 | 3,217 | 2,838 |
| repeated_05 | 9,877 | 6,920 | 3,809 | 3,293 |
| repeated_06 | 11,093 | 7,404 | 4,380 | 3,755 |
| repeated_07 | 12,309 | 7,888 | 4,961 | 4,210 |
| repeated_08 | 13,525 | 8,372 | 5,541 | 4,671 |
| repeated_09 | 14,741 | 8,856 | 6,126 | 5,128 |
| repeated_10 | 15,957 | 9,340 | 6,706 | 5,586 |
| repeated_11 | 17,178 | 9,829 | 7,292 | 6,042 |
| repeated_12 | 18,399 | 10,318 | 7,874 | 6,502 |
| repeated_13 | 19,620 | 10,807 | 8,461 | 6,960 |
| repeated_14 | 20,841 | 11,296 | 9,039 | 7,418 |
| repeated_15 | 22,062 | 11,785 | 9,621 | 7,873 |
| repeated_16 | 23,283 | 12,274 | 10,203 | 8,333 |
| repeated_17 | 24,504 | 12,763 | 10,785 | 8,790 |
| repeated_18 | 25,725 | 13,252 | 11,361 | 9,247 |
| repeated_19 | 26,946 | 13,741 | 11,946 | 9,706 |
| repeated_20 | 28,167 | 14,230 | 12,530 | 10,165 |
| **Aggregate** | **339,473** | **198,964** | **142,345** | **118,457** |

All 25 fixtures are strict wins in both bytes and exact local token count. The
aggregate reduction is 41.39% in bytes and 16.78% in `o200k_base` tokens. The
deterministic exhaustive-v2-derived v3 fixture is 5,467 bytes with SHA-256
`cf6f4084d89cb85a82a2e735df3b1fa7b4e41922892e1e5e4678b3c7494cfe4e`.

The token counts exclude provider framing, cached-input policy, billing, and
model behavior. They are packet-cost evidence only, not a code-quality result
or a promotion decision.

## Default-promotion gate

`compact_v3` remains opt-in. The measurements above establish deterministic
wire equivalence for the scoped fixtures and show packet-cost reductions; they
do not establish decoder compatibility across consumers, field parity over
representative production packets, or unchanged agent outcomes. Making v3 the
default requires those proofs plus an explicit migration and default-change
decision. Until then, `legacy_json` remains the product default. This gate is
also recorded in `docs/semantic_brain_plan.md` alongside the current retrieval
limitations.
