# Initial memory integrity canary — 2026-09-13

Development-only synthetic component results, using the current Brain checkout
based on `9f13d344` plus the opt-in `facts eval --include-context` patch.
The local output reports retain the fixture, runner, binary and request hashes.
No reader model, distiller or coding agent was run. These results measure
retrieved source availability, **not** answer accuracy or coding performance.

The fixture contains seven intentionally corrupted claims and one omitted
decision. This is a stress fixture, not an estimate of real memory error rates.
All rows used the same eight queries and product lexical retrieval with `k=4`.

| Memory arm | Complete source evidence, 4096-byte ceiling | Complete source evidence, 512-byte ceiling |
|---|---:|---:|
| No memory | 0/8 | 0/8 |
| Raw history | 8/8 | 8/8 |
| Facts only | 0/8 | 0/8 |
| Facts with source expansion and raw backfill | 8/8 | 7/8 |

Facts-only and no-memory contain no original sources by design, so their zero
source-coverage scores do not measure how often an agent would answer correctly.
At 4096 bytes, all seven misleading claims remain present in the combined arm;
retrieving their supporting/correcting evidence does not automatically remove
or repair those claims. A reader must still reconcile the conflict.

## The useful failure

At 512 bytes, the combined arm fails the later-correction case. Its first
group contains the stale RetryUpload claim and the original decision it cites.
That group consumes enough space to exclude the independently retrieved later
correction. Raw history alone fits both passages in the same ceiling.

This is a counterexample to assuming that adding provenance always improves
the context delivered to an agent. A facts-first packing policy can displace
newer evidence. Source expansion improved no source-coverage result over raw
history in this small fixture and regressed one tight-budget case.

The next candidate to evaluate is packing evidence for corrections and scope
before redundant claim text, with explicit temporal reasoning and a reserved
raw-retrieval budget. Do not silently change the current arm and overwrite this
baseline to erase the counterexample. Add a separate recorded composition and
evaluate it on independent cases before choosing it for the product.

Reproduction and the blinded reader-response contract are in
[MEMORY-INTEGRITY.md](MEMORY-INTEGRITY.md). The canary's scoring harness has
contract tests, including fabricated citations and missing-response rejection;
those tests use stubs and are not reader-accuracy evidence.
