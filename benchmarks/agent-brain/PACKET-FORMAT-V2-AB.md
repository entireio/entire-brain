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

## Identity-bound evidence

The first one-task smoke exposed that report schema 1 did not bind the Python
parser/runner source. That report is superseded and is not evidence. The
schema-2 harness was committed as
`7041f57fd60606e9e451b4fe8fb3a243c79a79d0` and binds:

- product contract commit
  `172aaa3f361376cc00914e26d570f80b74442b5a`;
- exact product binary SHA-256
  `099253bd8f19522cb3c2a3bb70fe27a0bce572bbae58b77f655a22069484d793`;
- exhaustive compact-v2 golden SHA-256
  `e0bc12b75059a7bba809ef1a66739c49d397852a30271da1b3e2ef4f9a6ca982`;
- runner source SHA-256
  `6d4fb8a3abf077fc98b6cc098db32b53d85610067ef8245b3e84fb8c7cef91b4`;
- imported profile source SHA-256
  `348bce25df0025ec03dfd782734996d942458999c52fa9f372374515c344a91f`;
- canonical contract fingerprint
  `df4b35287f95afc8773371006e83506e2f2b729e9a03ccd80f51b961de647d91`;
  and
- runner-identity self-hash
  `1f000aabed9d7b0841a87f29cc0f525b5871c40ac87fc42245dafbfd5f3ab0ad`.

The identity and every field-tamper test passed as part of the 24-test harness
suite. The post-commit, isolated, no-egress one-task smoke then passed with
canonical parity and compact integrity true:

| Evidence | Legacy | Compact v2 | Reduction |
| --- | ---: | ---: | ---: |
| Inner UTF-8 bytes | 154,877 | 59,174 | 61.7929% |
| Byte-quad proxy | 38,720 | 14,794 | 61.7923% |

The smoke report self-hash is
`b999a715def72afd6d845a9892cd697b96a729d533ee0cab7d166fda38ae2d48`
and its file SHA-256 is
`14a0353c0249066d89d7bf9602a385b09d7df659787743e5b5d000b1daeb9cbd`.
It is mode 0600, incomplete by construction, and not promotion-eligible.

## Full 114-pair result

After the identity-bound smoke passed, the same exact binary and harness ran
the full verified corpus with network-egress variables removed,
`ENTIRE_BRAIN_NO_EGRESS=1`, isolated plugin data, and a 300-second per-pair
timeout. All 114 adjacent pairs succeeded. Both formats parsed for every pair;
every compact integrity check and canonical projection matched; and no compact
packet grew.

| Metric | Legacy | Compact v2 | Pooled reduction | Per-task lower median | Per-task min / max |
| --- | ---: | ---: | ---: | ---: | ---: |
| Inner UTF-8 bytes | 14,567,681 | 5,750,255 | 60.5273% | 61.1118% | 14.4682% / 67.4029% |
| Byte-quad proxy | 3,641,962 | 1,437,601 | 60.5267% | 61.1103% | 14.4669% / 67.4033% |

The result is consistent across all three logical repositories:

| Logical repo | Pairs | Legacy inner bytes | Compact-v2 inner bytes | Pooled reduction | Lower median | Minimum |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| `entire-brain` | 25 | 3,055,714 | 1,234,486 | 59.6007% | 60.8946% | 19.3373% |
| `entire-cli` | 68 | 9,421,527 | 3,678,348 | 60.9580% | 61.1118% | 26.7103% |
| `entire-db` | 21 | 2,090,440 | 837,421 | 59.9404% | 61.6621% | 14.4682% |

The compact-v2 byte attribution exactly partitions all 5,750,255 inner bytes:

| Wire component | Bytes |
| --- | ---: |
| Markers | 3,306 |
| Legends | 6,498 |
| Newlines | 21,867 |
| Footers | 8,192 |
| Declarations | 106,687 |
| Keyed records | 111,758 |
| Positional records | 5,491,947 |

Across the corpus there are 20,312 body records: 735 keyed and 19,577
positional, with 1,213 declarations, 94,406 explicit cells, 83,500 explicit
null cells, 37,614 metadata repeat references, and 27,911 implicit trailing
omissions. Packet-level and aggregate attribution deltas are all zero.

Independent post-run verification checked mode 0600, report self-hash, exact
runner identity, complete frozen schedule, privacy exclusions, all 114
observation identities and arithmetic invariants, aggregate recomputation, and
every promotion gate. The exact binary hash and all three repository HEADs
still matched. Recollection under the exact isolated no-egress environment also
confirmed that all three manifest snapshots were unchanged after the run:

- `entire-brain`:
  `9464f3cca3c76e26d14068a7440727b8217072898bb13a53bd565faf376191c9`;
- `entire-cli`:
  `7348c72b6bc3d7dd7d8e963eaa88cc535e05313af546c5d10995de43e8ff4b9f`;
  and
- `entire-db`:
  `a5582c58b81aadc36eb63c50b2c61a2af45d70104b60d06c77f82c76ec84ef93`.

The full report self-hash is
`75ba1c4a4e25f873be151e6f67fa2cfe30ee4e0fbfe4d79b5a56c452e1fc5164`
and its file SHA-256 is
`95a458ff9f8e794cd48ca6b0dd2d07b6c03fb3d939ca70ad3584d76bf1687146`.
It makes the candidate eligible only for a separately authorized agent-quality
trial. It does not authorize paid/model use or changing the MCP/CLI default.
