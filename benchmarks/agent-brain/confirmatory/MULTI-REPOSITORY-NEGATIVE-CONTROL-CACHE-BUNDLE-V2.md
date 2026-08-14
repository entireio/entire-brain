# Multi-repository negative-control cache bundle format v2

This slice freezes a deterministic, uncompressed, regular-file-only bundle format and a
verifier-only contract for the future three-repository offline Go cache seed. It closes the v1
archive's content-interpretation gap without creating, extracting, staging, consuming, or executing
any cache material. It does **not** authorize a benchmark, candidate, model/provider, network, or
paid operation.

The checked v1 material declaration remains `pending_source_review`, with every actual archive,
manifest, inventory, and count identity null. No actual v2 bundle or material is added. The checked
pending v2 verifier contract is added after the source and schemas are sealed, but its pending state
forbids access to either caller locator. The new code has no production `pack`, `extract`, or `stage`
command.

## Frozen identities

The wire contract has profile `agent_brain_negative_control_cache_bundle_format_v2`, schema version
2, and status `wire_format_frozen_no_material_no_authority`.

| Identity | SHA-256 |
| --- | --- |
| Format artifact raw bytes | `970669197065a563fa0a09c281f671f5a3cd63061d6ff8f52da64926fa5ac406` |
| Format artifact canonical self-hash | `e2231e7fae7dfdad054c770d928b549bb996b4c452169cb22abea7f8f6f0ee59` |
| Format schema | `95712ba76c36107f583a195167e6dea9d90ba81729fba10fa211096d277617d8` |
| Verifier-contract schema | `eca24ad3f0a50f4a9dcbd9e106715db8fee449d4735eaeee87912a95e7c564bd` |
| Verification-report schema | `3afd8fec9da54a8b7358b497031633bfeaf8c5df1f595347424769d4a30ec4ad` |
| Verifier source raw bytes | `1236f3244be5d25cb72b7937aab159903a20cb97c2ce057023e4c4694f248bfc` |
| Checked pending verifier contract raw bytes | `29d66a79f4c3083e006c823b44a5aeacb50538d426f0186e05fffa77226760ee` |
| Checked pending verifier contract canonical self-hash | `98de7291aceb5a84ff03d1862e66550fb4d67a55796634eacc4e610856829bdd` |

The format self-hash is SHA-256 over compact sorted-key UTF-8 JSON with `ensure_ascii=false`,
`allow_nan=false`, no trailing line feed, and only `format_contract_sha256` replaced by JSON null.
The verifier-contract self-hash uses the same canonical profile with only
`verifier_contract_sha256` replaced by JSON null. Each artifact's ordinary raw SHA-256 remains a
separate exact-byte identity.

The verifier contract also pins the existing v1 trust boundary:

| v1 dependency | Fixed value |
| --- | --- |
| Raw verifier source | `2f21da09c28cf2de62f808a6362f74fbc7aead07a04cfef96c15c93584540b94` |
| Pending material raw bytes | `7527cec7b2f1b47498845ae38710369997b7c82d2d0150761b23d2b594f68d93` |
| Pending material self-hash | `f03ae113a29585a9aba0c8ce57a41ab61eb0b2cd682474f1df2d214e299a4852` |
| Pending v1 verifier contract raw bytes | `2138adb640dee8ab3537afc1ccb69e846a6af5d360c02da9cfed30c33dded33c` |
| Pending v1 verifier contract self-hash | `56b5e019f65ef1afc0e4f265a705cdc1523585aad899d5c18ff7a5230463bb47` |

Changing a status string cannot approve material. A future source review must bind actual raw bytes,
update the v1 material declaration and v1 verifier contract together, and rebuild and review a v2
verifier contract against those new identities.

## One shared union seed

The existing manifest v1 is deliberately retained. It is one global, canonical union of files
owned by `entire-brain`, `entire-db`, and `entire-graph`, copied unchanged to every benchmark arm.
The manifest `repository_key` and bundle repository ID provide deterministic ownership,
provenance, counting, and ordering only. They are **not** destination-directory namespaces and do
not imply three independently staged cache trees. The exact machine-readable semantic is
`one_shared_union_seed_copied_unchanged_to_every_arm`.

Consequently:

- every repository must contribute at least one nonempty regular file;
- paths are globally unique, globally portable-unique, and globally ancestor-conflict-free across
  all three repository ownership groups;
- the manifest remains ordered by `(repository_key, path)` and bundle records by the equivalent
  `(repository_id, unsigned UTF-8 path bytes)` order; and
- staging, when separately implemented and reviewed, must copy the same verified union seed to
  each arm rather than partitioning records by repository ID.

There is no manifest v2 and no per-repository destination prefix in this format.

## Byte grammar

All integers are unsigned, fixed-width, and big-endian. Alternate integer encodings are invalid.
Every stored SHA-256 is the 32 raw bytes decoded from its lowercase hexadecimal representation.
The complete bundle is:

```text
HEADER[232] || RECORD[0] || ... || RECORD[N-1] || TRAILER[104]
```

There is no compression, member envelope, padding, alignment, or concatenated member.

### Header: exactly 232 bytes

| Offset | Bytes | Field |
| ---: | ---: | --- |
| 0 | 16 | ASCII magic `ENTIRECACHEBND2\0` (`454e544952454341434845424e443200`) |
| 16 | 2 | format version, exactly 2 |
| 18 | 2 | header byte count, exactly 232 |
| 20 | 4 | flags, exactly 0 |
| 24 | 8 | complete bundle byte count |
| 32 | 8 | file/record count |
| 40 | 8 | unpacked payload byte count |
| 48 | 8 | `entire-brain` file count |
| 56 | 8 | `entire-db` file count |
| 64 | 8 | `entire-graph` file count |
| 72 | 32 | format-contract self-hash |
| 104 | 32 | manifest-projection SHA-256 |
| 136 | 32 | manifest content-inventory SHA-256 |
| 168 | 32 | run-plan canonical self-hash |
| 200 | 32 | toolchain-bindings projection SHA-256 |

### Record: 52 fixed bytes, then path, then payload

| Offset | Bytes | Field |
| ---: | ---: | --- |
| 0 | 4 | ASCII marker `FILE` |
| 4 | 1 | repository ID: 0 `entire-brain`, 1 `entire-db`, 2 `entire-graph` |
| 5 | 1 | flags/reserved, exactly 0 |
| 6 | 2 | path UTF-8 byte count |
| 8 | 4 | ordinal, exactly the zero-based record position |
| 12 | 8 | content byte count |
| 20 | 32 | content SHA-256 |

The fixed record is followed immediately by the exact NFC UTF-8 path bytes and then by the exact
uncompressed payload bytes. There is no delimiter or padding. The manifest and record must agree
at the same ordinal on repository ownership, path, content byte count, and content SHA-256.

Only `gocache/` and `gomodcache/` paths are allowed. Paths are at most 1,024 UTF-8 bytes, each
component is at most 255 UTF-8 bytes, and NUL, backslash, dot traversal, non-NFC text, and Unicode
category-C control characters are rejected. Any component whose ASCII case-insensitive/case-folded
value is exactly `..namedfork` is also rejected. That closes the macOS/APFS `..namedfork/rsrc`
pseudo-path to resource forks and alternate named streams, preserving the regular-file-only claim.
Portable uniqueness is NFC case-folded per path component.

### Trailer: exactly 104 bytes

| Offset | Bytes | Field |
| ---: | ---: | --- |
| 0 | 16 | ASCII magic `ENTIRECACHEEND2\0` (`454e544952454341434845454e443200`) |
| 16 | 8 | repeated complete bundle byte count |
| 24 | 8 | repeated file/record count |
| 32 | 8 | repeated unpacked payload byte count |
| 40 | 32 | records digest |
| 72 | 32 | prefix digest |

Exact EOF must follow trailer byte 104. Let `B` be all bundle bytes, `L = len(B)`, and
`T = L - 104`.

- Records digest is SHA-256 of ASCII
  `entire-brain/cache-bundle-records/v2\0` followed by `B[232:T]`.
- Prefix digest is SHA-256 of ASCII `entire-brain/cache-bundle-prefix/v2\0` followed by
  `B[0:L-32]`. It therefore includes the trailer magic, repeated counts, and records digest and
  excludes only its own 32 bytes.
- The external raw identity remains plain SHA-256 of `B[0:L]`.

## Manifest projection

The header's projection digest cannot include the bundle identity fields that it helps bind. Start
with a deep copy of the fully schema-, canonical-, self-, scope-, inventory-, order-, and
path-validated final manifest v1. Set exactly these three fields to JSON null:

```text
archive.byte_count
archive.sha256
manifest_sha256
```

Delete or mutate nothing else. Serialize with sorted keys, compact `,` and `:` separators,
`ensure_ascii=false`, `allow_nan=false`, UTF-8, and no line feed. The projection digest is SHA-256
of ASCII `entire-brain/cache-bundle-manifest-projection/v2\0` followed by those canonical bytes.

## Exact ceilings

For `N` records, valid size must satisfy the exact equality:

```text
L = 232 + 104 + 52*N + sum(path_utf8_byte_count) + unpacked_byte_count
```

Both `L` and `unpacked_byte_count` are independently capped at 2,147,483,648 bytes. `N` is from 3
through 250,000 inclusive and each repository count is at least one. A parser must use
subtraction-based remaining-byte checks, reserve the exact trailer and minimum remaining-record
space before each read, stream payloads in bounded chunks, and never allocate from a payload-sized
declaration.

## Structurally unrepresentable input

The only record kind is a nonempty regular file. Parent directories are implicit. The grammar has
no representation for explicit directory entries, symbolic links, hard links or inode identity,
FIFO/socket/block/character/device nodes, whiteouts, sparse-file metadata, alternate streams,
resource forks, compression, encryption, signatures, padding, concatenated members, ACLs, extended
attributes, uid/gid, timestamps, modes, executable bits, or setuid/setgid/sticky bits.

This is a stronger content boundary than treating a tar or zip file as opaque. It is still not a
staging claim. A future stager must be a separately reviewed component, use freshly created private
directories and files (target policy 0700 directories and 0600 files), prove filesystem behavior,
and separately validate the Go cache's required mode and runtime semantics.

## Two-pass verifier-only boundary

The future approved production verifier must hold one descriptor for the entire operation. It
opens a stable, single-link regular file without following symlinks and never reopens the pathname.

1. Before any content interpretation, pass one checks exact `fstat` byte count and streams plain
   raw SHA-256 against the independently source-reviewed v1 material identity.
2. It compares pre/post stable descriptor identity and rewinds that same descriptor to byte zero.
3. Pass two validates the fixed header, streams every record and payload, checks every manifest
   declaration and payload SHA-256, checks canonical order and exact manifest bijection, checks both
   domain-separated trailer hashes, and requires exact EOF.
4. It compares descriptor identity again after pass two.
5. Only then may it emit one deterministic, non-persistent report on standard output.

No payload-sized allocation, extraction, filesystem materialization, staging, ownership change,
mode application, E0 binding, approval consumption, or execution occurs. Pending material fails
before the caller's manifest or bundle locator is opened.

The intended production CLI is limited to:

- `build --output CONTRACT` for a deterministic non-authorizing verifier contract;
- `check` for fixed dependency and checked-contract integrity; and
- `verify MANIFEST BUNDLE` for the approved two-pass verification path.

Synthetic fixture construction may exist only inside tests. It is not a material-production
surface and does not authorize supplying a real seed.

## Residual gates and next reviewed move

This format does not close actual cache-material source review, safe staging, Go-cache behavior
validation, APFS observation/reservation, owner approval and atomic consumption, clean-worktree
execution, first-parent reversal proof, attempt attestation/classification, private-log retention,
or fail-closed cleanup. All E0 runtime bindings remain null.

The next material-bearing change must be reviewed as one unit: actual external bundle and manifest
identities, the updated approved v1 material declaration and v1 verifier contract, the existing v2
verifier source and schemas, and a regenerated v2 verifier contract bound to those new identities.
It still may verify only; staging and execution remain separate successor gates.

## Unpaid local checks

From the repository root, these checks neither produce real material nor run a benchmark:

```bash
python3 benchmarks/agent-brain/confirmatory/negative_control_cache_bundle_v2.py build \
  --output /private/tmp/negative-control-cache-bundle-verifier-contract-v2.json
python3 benchmarks/agent-brain/confirmatory/negative_control_cache_bundle_v2.py check
cd benchmarks/agent-brain/confirmatory
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest test_negative_control_cache_bundle_v2.py
pyright negative_control_cache_bundle_v2.py test_negative_control_cache_bundle_v2.py
```

With the checked pending v1 declaration, production `verify MANIFEST BUNDLE` must exit nonzero
before reading either locator. Tests may use tiny temporary synthetic bundles only through private
test helpers and must not create or stage a real Go cache seed, run a candidate, contact a provider,
use the network, or incur paid work.
