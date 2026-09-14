# Deterministic evidence recall (experimental)

`recall --evidence` returns original conversation blocks with source citations.
It uses lexical retrieval and makes no provider or embedding-model call. Ordinary
`recall` continues to return distilled facts.

```sh
entire brain recall "What approval does release require?" --evidence --json
entire brain recall "What approval does release require?" --evidence --branch main --k 20 --evidence-bytes 8192 --json
```

For example, separate sessions might say "Release requires owner approval" and
"Release also requires security approval." If both blocks are retrieved and fit
the output budget, both appear verbatim with independent citations. Brain does
not infer that either statement replaces the other, combine them into a new
fact, or establish that either statement is still current. The consuming agent
must interpret the evidence and acknowledge missing or conflicting context.

## Retrieval and limits

- Searches canonical sessions on the selected branch. Matching distilled facts
  supply source-session handles only; their claims never replace source bytes.
- `--k` limits candidate sessions (default 10, range 1–128). At most 128 eligible
  sessions are scanned in manifest order, admitting at most 32 MiB of transcript
  input through the hardened canonical reader. Larger corpora may be incomplete.
- Lexical matching uses positive term coverage and simple plural matching. Each
  matched session contributes from a ranked pool of up to 128 blocks, scanning
  at most 4,096 blocks. Admission across sessions proceeds in rounds and gives
  both user and assistant messages an opportunity. The global pool is at most
  128 blocks with a 24 KiB compact-text-and-metadata admission estimate.
- The heuristic candidate budget is distinct from the exact public output
  budget. `input_truncated` and a warning report candidate limits, including
  `--k`; they do not measure recall accuracy or guarantee corpus coverage.
- Supports plain text, Claude/Pi messages, Codex user/assistant events and
  response messages, simple role/content JSONL, and OpenCode message documents.
  Structured hidden reasoning, tool output, system instructions and metadata
  are not returned as conversation text. Plain text has no role classification.
- Malformed JSON, invalid UTF-8, unavailable files, and sources with no supported
  conversation fields yield warnings and `partial` or `unavailable` retrieval.
  Known non-conversation records are ignored. Ambiguous session identities fail
  closed rather than guessing which source supplied a fact anchor.

## Citation contract

Each evidence span includes `id`, `session_id`, `branch`, canonical Brain-relative
`path`, `line`, `source_sha256`, `content_sha256`, `start_byte`, `end_byte`, and
unchanged `text`. Structured sources also include a `json_pointer`; role and
timestamp are included when available.

To verify a citation:

1. Read the canonical file at `path` and verify `source_sha256`.
2. For JSONL, decode the record at one-based `line`. For a JSON document, decode
   the document (`line` is 1). Resolve `json_pointer` to a string. For plain text,
   the pointer is absent and the addressed content is the whole file.
3. Verify the addressed content's UTF-8 bytes against `content_sha256`.
4. Slice those bytes at `[start_byte, end_byte)` and compare them with `text`.

Offsets count UTF-8 bytes, not characters. Blank-line boundaries preserve code
fences, table rows, and original line endings. IDs bind the source version and
range; changing file bytes changes the ID. Hashes establish byte identity, not
the truth, authorship, or current applicability of a statement.

## Output and privacy

`--evidence-bytes` defaults to 8,192 (range 2–1,048,576). It bounds the compact Go
JSON encoding of the **evidence array**, including citations, escaping, commas,
and brackets. Enclosing metadata and pretty-print whitespace are outside this
budget, so it is not a full-response transport cap. Whole blocks that do not fit
are skipped; later smaller blocks can still fit. `omitted_ids` and `truncated`
report those omissions. A budget of 2 returns `[]`.

The response reports `schema_version: 1`, `selection_mode: deterministic`,
`effective_engine: canonical_sessions_lexical_with_fact_anchors`, counts, byte
usage, retrieval state, coverage scope, and warnings. Empty evidence does not
prove that no answer exists.

Session exclusions, source containment, and derived-state privacy guards apply.
The existing Brain privacy lock covers source inspection through buffered output
so cleanup cannot commit between checking a source and emitting its bytes.
This command does not write facts, inferred relationships, or semantic vectors.
It works under `ENTIRE_BRAIN_NO_EGRESS=1`.

Supported options are `--branch`, `--k`, `--evidence-bytes`, and `--json`.
`--no-semantic` is harmless because this mode is already lexical. Explicit
provider, expansion, fact-filter, temporal-eligibility, and semantic-cache flags
are rejected. `--evidence-bytes` requires `--evidence`.

This feature is CLI-only. It adds no MCP schema, Graph integration, model
selector, temporal inference, or benchmark accuracy guarantee.
