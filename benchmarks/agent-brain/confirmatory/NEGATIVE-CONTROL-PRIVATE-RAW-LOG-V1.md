# Negative-control private raw-log primitive v1

Status: synthetic-tested storage primitive only. It does not authorize or perform candidate, Git,
Go, worktree, network, model/provider, private-data, holdout, benchmark, or paid execution.

`negative_control_private_log.py` implements the private content-addressed raw-log writer and checker
required by the pending multi-repository negative-control plan. It deliberately has no CLI, executor,
retention-deletion function, or public-receipt writer. The library returns canonical receipt bytes
only after a raw content file is fully published and reverified, so an interrupted call cannot emit a
partial public receipt.

## Private storage contract

The caller supplies an already-existing dedicated private root. The root must both be and resolve to
the same current-user, non-symlink directory with exact mode `0700`. The primitive creates exactly:

- `.negative-control-private-log.lock`, a current-user regular, non-symlink, non-hardlink file with
  exact mode `0600`; and
- `sha256/`, a current-user non-symlink directory with exact mode `0700`.

The lock is acquired exclusively and nonblocking. All store enumeration, size/count accounting,
writing, publication, checking, and future cleanup by cooperating same-UID processes **must** occur
while it is held. A busy or malformed lock fails immediately. This owner-only advisory lock is the
required coordination boundary; it cannot stop a noncooperating process with the same UID from
mutating a pathname after the final check. Such a process can always race an advisory-lock protocol,
so no stronger OS-isolation claim is made.

Each final raw log is `sha256/<sha256-of-exact-raw-bytes>`. Final files must be current-user regular,
non-symlink, non-hardlink files with exact mode `0600`. Untrusted regular-file opens use
`O_NOFOLLOW`, `O_NONBLOCK`, and descriptor-relative paths. Before a write, the pinned `sha256`
directory is incrementally enumerated through its open descriptor and every entry's type, owner,
mode, link count, and size contributes to the actual total. Enumeration stops at the final-file
limit plus one without first materializing an unbounded name list. Caller-reported accounting is not
accepted.

Input is streamed into an exclusive `0600` temporary file while SHA-256 and byte count are computed.
Only after the file is complete and synchronized does descriptor-relative `link(2)` publish the digest
name without replacement. The temporary name is then removed, the final link count must be one, and
the final bytes are streamed and hashed again. After hashing, a descriptor-relative, non-following
pathname stat must prove the digest name still resolves to the opened inode with the complete expected
metadata. A pre-existing exact digest is deduplicated and never overwritten; a pre-existing digest
name with different bytes fails closed.

The exact frozen ceilings are 16 MiB per arm, 2 GiB total stored raw-log content, and 4,096 final
content files. The frozen plan has 62 candidates × 2 arms × 2 repetitions = 248 planned arm logs;
4,096 provides more than 16× headroom while keeping enumeration and in-memory inventory bounded.
Tests may inject only smaller positive ceilings. The total is the secure sum of actual final files
under the exclusive lock. Admission for a new digest happens after hashing; therefore an exact
duplicate adds zero stored bytes and remains valid when the byte or file-count store is at its
ceiling, while a new digest is rejected.

## Public receipt contract

The public receipt contains exactly two fields and one trailing LF:

```json
{"raw_log_byte_count":9,"raw_log_sha256":"<64 lowercase hex>"}
```

The schema is `schemas/negative-control-private-log-receipt-v1.schema.json`. The runtime checker
rejects duplicate keys, extra/missing fields, floats and booleans as byte counts, invalid UTF-8 or
JSON, noncanonical key order/spacing, extra newlines, out-of-range counts, missing content addresses,
and byte/hash drift.

No raw content, filesystem path, host identity, secret, command, or redaction status is included.
Raw logs may contain secrets. This primitive makes **no redaction claim**; privacy comes from the
private root and exact file controls, not from content transformation. `PrivateLogError` failures
created by the primitive use fixed descriptions and never interpolate raw input or a private path.
Caller-originated `BaseException` instances, including cancellation such as `KeyboardInterrupt`, are
propagated unchanged to preserve cancellation semantics and are outside that non-leaking claim.

## Interruption and residual gates

Directory synchronization fails closed on every `OSError`; unsupported/error returns are not treated
as durable success. Before publication, normal exception cleanup removes the temporary file and leaves
no final digest or returned receipt. After the atomic link, an exception can leave a complete final
content file without a receipt. If both temporary cleanup and digest rollback unlink fail—or a process
is killed in the brief publication interval—the complete temporary and digest names may remain as a
two-link inode. No receipt is returned, and later exact enumeration rejects the temporary name/link
count until a separately audited cleanup mechanism repairs it.

This slice intentionally does not implement:

- retention deletion or enforcement of the plan's seven-day expiry;
- orphan cleanup or interruption attestation;
- a global byte-hash scrub of unrelated pre-existing digest names (every bounded entry is metadata-checked,
  and the receipt target is exact-byte checked);
- receipt-file publication or aggregate receipt accounting;
- classification, executor, reversal, worktree, cache-seed, resource-preflight, owner-approval, or
  trust mechanisms; or
- any update that makes the frozen pending plan executable.

Those remain residual gates. The primitive alone cannot authorize a negative-control run.

## Synthetic verification

From `benchmarks/agent-brain/confirmatory`:

```bash
python3 -m unittest test_negative_control_private_log.py -v
```

The focused tests use only temporary roots and in-memory synthetic byte streams. Tightened limits
exercise the 16 MiB/2 GiB arithmetic without allocating large fixtures. They also cover symlinks,
hardlinks, wrong modes, FIFOs and nonregular entries, a busy lock, strict directory synchronization,
bounded file-count admission, no-overwrite behavior, exact deduplication at a full store, deterministic
pathname replacement, mutation, canonical receipts, fixed `PrivateLogError` messages, cancellation,
and interruption before and after atomic publication—including the rejected two-link residual state.
