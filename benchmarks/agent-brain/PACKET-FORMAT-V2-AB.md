# Compact v2 packet-format A/B

`packet_format_v2_ab.py` is a development-only, unpaid comparison of the MCP
`brain_brief` formats `legacy_json` and opt-in `compact_v2` over the verified
114-task brief-profile corpus. It does not launch an agent or call a model.
It cannot authorize a paid quality trial or a change to the MCP/CLI default.

## Independent checks

For every task, one MCP process receives adjacent `tools/call` requests in the
fixed order `legacy_json`, then `compact_v2`. The Python harness independently:

1. parses duplicate-key-free finite legacy JSON into the frozen compact-v1
   typed semantic projection;
2. requires the exact compact-v2 marker and the hashed in-band legend
   `legend ~=absent ^=previous_same_opcode_record_same_column`;
3. validates the complete frozen tag/opcode/field/type schema;
4. validates every keyed row against its exact schema, field order, and
   canonical Go value encoding;
5. validates declarations, positional widths, omitted `~` cells, and `^`
   references to the previous semantic record of the same opcode and column,
   including across interleaved physical rows;
6. permits `^` only for the frozen metadata allowlist, never natural-language
   task text, excerpts, guidance, reasons, evidence, actions, or workflow text;
7. independently recomputes the exact family budget: a family with more than
   one record is positional only when declaration plus positional-row UTF-8
   bytes (all including newlines) are strictly less than canonical keyed-row
   bytes; exact ties and all singletons are keyed;
8. verifies the footer record count, lowercase SHA-256 over the full body, and
   final newline; and
9. compares canonical projection bytes, not loose Python value equality.

Any grammar, integrity, reference, family-budget, or projection violation fails
the pair closed.

## Privacy and retained evidence

Prompts, legacy packets, compact packets, repository paths, stdout, and stderr
exist only in process memory. The mode-0600 report retains only logical repo
labels; task/prompt, packet/frame, runtime, and report hashes; byte counts; the
frozen UTF-8 byte-quad token proxy; parser/integrity/parity booleans; numeric
wire attribution; fixed gates; and fixed failure kinds with counts and hashes.

Report schema 2 adds a self-hashed, hash-only `runner_identity`. It binds the
exact `packet_format_v2_ab.py` bytes, imported `profile_brief.py` bytes, product
contract commit, exhaustive compact-v2 golden, and a canonical fingerprint of
the marker, legend, full positional schema, metadata-repeat allowlist, wire
selection rules, token proxy, and gates. The runner verifies the product
contract is an ancestor of `HEAD` and the checked-in golden has its frozen hash
before any task pair is launched. No source or repository path is retained.

The proxy is exactly:

```text
count=0 if empty else ceil(len(exact_inner_utf8_bytes)/4)
```

It is not a provider tokenizer and is not a billable token count.

## Commands

Run the exhaustive local tests first:

```bash
python3 -m unittest benchmarks/agent-brain/test_packet_format_v2_ab.py -v
```

One-task isolated smoke (incomplete and never promotion-eligible):

```bash
ENTIRE_PLUGIN_DATA_DIR="$ISOLATED_PLUGIN_DATA" \
python3 benchmarks/agent-brain/packet_format_v2_ab.py \
  --brain-bin "$BRAIN_BIN" \
  --repo "entire-brain=$ENTIRE_BRAIN_REPO" \
  --repo "entire-cli=$ENTIRE_CLI_REPO" \
  --repo "entire-db=$ENTIRE_DB_REPO" \
  --max-tasks 1 \
  --output "$PRIVATE_OUTPUT"
```

Full 114-task development run, only after the product contract and harness are
reviewed:

```bash
ENTIRE_PLUGIN_DATA_DIR="$ISOLATED_PLUGIN_DATA" \
python3 benchmarks/agent-brain/packet_format_v2_ab.py \
  --brain-bin "$BRAIN_BIN" \
  --repo "entire-brain=$ENTIRE_BRAIN_REPO" \
  --repo "entire-cli=$ENTIRE_CLI_REPO" \
  --repo "entire-db=$ENTIRE_DB_REPO" \
  --output "$PRIVATE_OUTPUT"
```

The frozen promotion gate requires exactly 114 successful pairs, every packet
to parse, every compact integrity check and canonical projection to pass, a
lower-median inner-byte reduction of at least 60%, a lower-median byte-quad
proxy reduction of at least 50%, and no task with compact inner-byte growth.
Passing only makes the candidate eligible for a separately authorized quality
trial; paid/model use and default changes remain false.

## Identity-bound smoke gate

The first one-task smoke exposed that report schema 1 did not bind the Python
parser/runner source. That report is superseded and is not promotion evidence.
After the schema-2 harness commit, all tests and the isolated no-egress smoke
must be rerun; only that regenerated identity-bound report can unlock the full
development run. The full report remains subject to every frozen 114-task gate
and never authorizes paid/model use or a default change.
