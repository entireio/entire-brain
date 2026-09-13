# Experimental evidence recall

`recall --evidence` returns original conversation text selected for a question.
Ordinary `recall` continues to return facts. This feature is opt-in and does not
write inferred replacements, requirements, or conflicts into Brain's fact store.

```powershell
entire-brain recall "What review is required before release?" --evidence --agent codex --model gpt-5.6-sol --evidence-bytes 8192 --json
entire-brain recall "What review is required before release?" --evidence --agent none --evidence-bytes 8192 --json
```

The first command invokes the selected provider once, with a 180-second deadline and a two-second subprocess pipe-drain backstop.
The second uses deterministic blocks in retrieval order and makes no model call.
`--agent auto`, `claude-code`, `ollama`, and a configured `command` use the existing
provider adapters. Hosted/unknown agents are rejected under Brain's no-egress
policy. No credentials or model are configured by enabling this feature.

For example, a session saying “release needs owner approval” and a later session
saying “security approval is also required” can produce a required evidence group.
Both original blocks fit together or are omitted together. A later implementation
report can be optional corroboration. A replacement judgment only affects this
question's packet; it does not establish that the stored policy has changed.

The locally built binary is retained at
`bin/memory-recall-compact-20260913/compact-diverse-final.exe`. To build from source:

```powershell
go build -o ./bin/entire-brain-evidence.exe ./cmd/entire-brain
```

## Retrieval and output contract

- Searches canonical sessions on the selected branch using positive lexical term
  coverage and simple plural matching, without history's code-specific score
  bonuses or long-record penalties.
  Retrieved facts contribute source-session handles; their claims are not placed
  in the evidence packet. `--k` limits candidate sessions (default 10, maximum 128).
- Limits scanning to 128 sessions and a bounded cumulative input; canonical reads
  use Brain's hardened transcript reader. Each matched session contributes from
  a ranked pool of at most 128 blocks, scanning at most 4,096 blocks per session.
  The global pool admits sessions in rounds and interleaves two user blocks with
  one assistant/unknown block where available. Query coverage, length and a soft
  user-role boost rank blocks; no benchmark labels or answers are consulted.
  The selector receives at most 128 blocks under a 24 KiB charged candidate
  budget (text plus a field allowance or actual metadata cost, whichever is larger), plus the question/prompt.
  Reaching a cap sets `input_truncated` and a warning. These heuristics can omit
  useful evidence; they do not establish exhaustive retrieval.
- Supports plain text, Claude/Pi message JSONL, Codex user/assistant event and
  response JSONL, simple role/content JSONL, and OpenCode message documents.
  JSON decoding preserves exact string contents. Hidden reasoning and raw tool
  payloads are excluded. Malformed/unsupported conversation sources are reported
  as partial or unavailable, never silently substituted with distilled claims.
- Each returned span includes its canonical Brain-relative `path`, session,
  branch, source-file SHA-256, decoded-content SHA-256, and UTF-8 byte interval.
  For JSONL, `line` locates the JSON record and `json_pointer` locates the decoded
  string within it. For a JSON document, line is 1 and the pointer addresses the
  document. For plain text, an empty pointer means offsets address file bytes.
  Ranges are half-open: `[start_byte, end_byte)`. They are not character indices.
- Stable IDs bind the source version and range. A changed source file yields new
  IDs. Checksums prove byte identity, not truth or a cryptographic speaker signature.
- `evidence_bytes` counts the compact Go JSON encoding of the `evidence` array,
  including its provenance fields. The surrounding metadata and pretty-printing
  are outside `--evidence-bytes`; this is not an entire-response transport cap.
  This flag is a CLI feature; it is not exposed in the MCP tool schemas.
- `returned_count` counts actual returned spans. `omitted_ids` are eligible blocks
  excluded by the output budget; `suppressed_ids` were excluded by model judgments.
  `truncated` reports output-budget omission, while `input_truncated` reports
  earlier candidate omission. Neither field establishes corpus completeness.

## Selection and failure behavior

The `sparse_v2` selector protocol groups source/field metadata once and sends
short request-bound aliases (`b0`, `b1`, ...). Full citation anchors stay local;
a catalogue digest binds every original anchor into the request digest. The
provider lists direct and optional supporting evidence, with a status for each
listed alias, and echoes the request digest. Unlisted aliases are treated as
unrelated/unknown for packing; omission does not prove semantic irrelevance.
The model is instructed to review all supplied sources, but sparse output does
not certify that it did so.

Unknown/duplicate aliases, unknown fields, duplicate JSON keys, missing arrays,
malformed tuples/links, changed request/catalogue bindings and inconsistent
required/replacement groups invalidate the response. Aliases expand back to
unchanged public `span:` IDs. No generated quotation or answer is accepted as
source evidence.

Required and conflicting blocks are kept as whole groups. A `joint_evidence`
link also keeps together distinct records needed for a complete answer, such as
a count across sessions; it does not assert a stored obligation. Corroboration is
optional. Applicable successors suppress explicitly replaced blocks only for
this recall. Future/proposed replacements are rejected. Relationships remain
fallible model judgments even when structural validation passes.

A failed, timed-out, or invalid model response returns deterministic source
blocks and `selection_mode: fallback`, with an explicit warning. Cancellation
and privacy-policy denial remain errors. Unavailable sources return no source
bytes. Source exclusions and privacy cleanup share the existing lock through
provider invocation and final buffered output; recall may therefore delay a
concurrent exclusion for up to the provider timeout.

This initial path does not support fact filters, temporal eligibility flags,
`--all`, or query expansion; combinations are rejected instead of silently
changing their meaning. It uses lexical session candidates, not Graph or the
semantic fact reranker. Retrieval misses remain possible.

`token_usage` reports provider-reported input/output/cache counts where available,
including failed attempts. `selector_protocol` identifies the model wire contract;
`selector_input_bytes` reports the serialized request sent (zero without a call).
`selector_seconds` and `total_seconds` expose latency.
No dollar cost is estimated when the provider supplies no billing information.

## Validation

The Go tests cover source reconstruction, Unicode/CRLF, code fences, branch
isolation, excluded/missing sources, ambiguous identities, provider isolation,
no-egress behavior, privacy locking, request validation, whole-group packing,
fallback, and unchanged default recall.

The original independently labeled pilot uses the published LongMemEval oracle subset:
four questions per published question type and four abstentions, selected in file
order before calls. Both arms get the same sessions, byte cap and reader. Gold
answers and `has_answer` labels never enter recall or reader requests. This tests
selection and reading over provided evidence sessions; it does not measure
full-corpus retrieval. The grading prompt follows the upstream task-specific
rubric, with a different configured judge model and JSON output, so results are
not official LongMemEval leaderboard scores.

Sources: [LongMemEval repository](https://github.com/xiaowu0162/LongMemEval),
[published cleaned dataset](https://huggingface.co/datasets/xiaowu0162/longmemeval-cleaned).

The [compact-selector follow-up](../benchmarks/agent-brain/RECALL-COMPACT-RESULTS.md)
reuses those 28 questions as a development/regression set after inspecting
failures. It preserves the original scores and labels, including an ambiguous
clothing-count case. It is not an unseen validation set.
