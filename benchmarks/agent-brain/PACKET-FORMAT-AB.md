# Unpaid MCP packet-format A/B

`packet_format_ab.py` compares the committed MCP `brain_brief` packet formats
`legacy_json` and opt-in `compact_v1` over the frozen 114-query development
corpus. It launches only the selected local Entire Brain binary. It never
launches a coding agent, provider model, validator, or paid benchmark.

This is development evidence about packet size and semantic preservation. It is
not confirmatory benchmark evidence, a code-quality result, or authorization to
change the MCP/CLI default from `legacy_json`.

## Pairing and corpus

The runner reuses `brief-profile-corpus-v1.json` and its fail-closed inventory
verification. The exact corpus has 114 unique prompts in committed order: 25
`entire-brain`, 68 `entire-cli`, and 21 `entire-db`.

For each query, one local `entire mcp` process receives exactly two adjacent
Content-Length frames, without an initialize request:

1. `tools/call` for `brain_brief` with `packet_format: "legacy_json"`
2. `tools/call` for `brain_brief` with `packet_format: "compact_v1"`

The repo, task, prompt, binary, and live Brain state are therefore the same
within a pair. The runner never primes, clears, copies, or mutates Brain state.

Use `--max-tasks 1` for a non-promotable smoke test:

```sh
python3 benchmarks/agent-brain/packet_format_ab.py \
  --brain-bin /absolute/path/to/entire \
  --repo entire-brain=/absolute/path/to/entire-brain \
  --repo entire-cli=/absolute/path/to/entire-cli \
  --repo entire-db=/absolute/path/to/entire-db \
  --max-tasks 1 \
  --output /private/path/packet-format-ab-smoke.json
```

Omit `--max-tasks` only for a separately reviewed full-corpus run. A prefix run
sets `complete_corpus: false` and cannot pass the promotion gate.

## Measurements and semantic parity

For each format the report retains exact inner-packet and full MCP-response
frame byte counts and SHA-256 hashes. The frozen offline token proxy is:

```text
utf8_byte_quads_v1
count=0 if empty else ceil(len(exact_inner_utf8_bytes)/4)
```

The report retains that exact definition and its SHA-256. This transparent
integer proxy is not a provider tokenizer, a billable token count, or a claim
about any model's tokenizer.

The compact parser independently verifies the exact version marker, bounded
line grammar, fixed end-field order, record counts, final newline, body-record
count, body SHA-256, and warning/category counts. It fails closed on malformed,
truncated, tampered, non-UTF-8, duplicate-field, or non-finite input.

Report schema 2 and structural-attribution schema 2 attribute every compact
inner-packet byte in process.
The exact, disjoint partition is: marker bytes, newline bytes, the complete end
record, body record-tag bytes, body field-key-plus-`=` bytes, decoded scalar
payload bytes, decoded string-array payload bytes, separators, quote/bracket
delimiters, and escape expansion. Payload means decoded Go-string source bytes:
literal runes contribute their UTF-8 width, named escapes and `\xNN` each
contribute one source byte, and valid `\u`/`\U` escapes contribute their Unicode
scalar's UTF-8 width. Surrogate and out-of-range Unicode escapes fail closed.
Escape expansion is the encoded Go-quoted length less its delimiters and decoded
source-byte count. Each record and each string array is checked against its
lexical byte length before its numbers are retained; the packet partition must
sum exactly to the existing compact inner-byte count or the pair fails closed.

The report retains numeric body record/field/value-kind counts and byte totals
both per packet and summed over successful packets. It also retains the same
numeric totals for each member of the fixed `compact_v1` body-tag vocabulary.
Unknown body tags fail closed rather than becoming report keys. The attribution
subtrees contain integer leaves only: no tag value, field key, field value,
packet text, prompt, path, stdout, or stderr is retained. Per-tag names are
fixed format-schema keys, not values discovered from a packet.

Separately, Python code projects the parsed legacy JSON onto the allowlisted
semantic record stream defined for `compact_v1`. It does not call or reuse the
Go compact renderer. The two canonical projections are compared in memory and
only their SHA-256 hashes and equality boolean are retained. The projection
includes status/source/live trust signals; facts verification; semantic
freshness, coverage, blind spots, symbols, relations and evidence; runtime and
test records; history; facts and drift; actions; patterns; consolidations;
themes; guidance; and warnings. Volatile or host-identifying legacy metadata
that compact intentionally omits—timestamps, absolute repo/Brain/provider-store
paths, and private provenance identifiers—is outside the projection.

## Retained privacy-safe report

Prompts, packets, MCP JSON, repo paths, Brain paths, and stderr exist only in
process memory. A successful pair retains:

- sequence, logical repo, task hash, and prompt hash;
- inner and framed byte counts/hashes for both formats;
- frozen offline token-proxy counts;
- parse and compact-integrity results plus compact body count/hash;
- numeric-only exact structural-size attribution and accounting invariants;
- canonical-projection hashes/parity; and
- integer bytes/token savings and reductions in parts per million.

A failed pair retains only a fixed failure kind, return code, and stdout/stderr
byte counts and hashes. It retains no stdout or stderr content. Runtime identity
contains only the binary SHA-256 and, per logical repo, Git HEAD and Brain
manifest SHA-256. Reports contain no filesystem path, timestamp, environment
value, hostname, or user identity. The complete report is self-hashed and
atomically written with mode `0600`.

## Exact promotion gate

The report is eligible for a separately authorized agent-quality trial only if
all of these checks pass:

- the exact complete 114-query corpus ran;
- all 114 adjacent format pairs succeeded;
- every legacy and compact packet parsed;
- every compact body count, checksum, and category count passed;
- all 114 independent canonical projections matched;
- the lower median inner-byte reduction is at least 600,000 ppm (60%);
- the lower median offline-token-proxy reduction is at least 500,000 ppm (50%);
- no task has compact inner-byte growth.

Even a fully passing report sets `paid_agent_quality_trial_authorized: false` and
`change_mcp_or_cli_default_authorized: false`. It is only a gate for a later,
separately approved quality experiment; it is never itself a default-switch or
quality claim.
