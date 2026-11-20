# Input limits & untrusted-input hardening

Entire Brain ingests data from sources it does not control: Entire session
transcripts, the `entire-graph` snapshot stream, repository files, checkpoint
history fetched over git, and on-disk derived artifacts. This
document records the size/time ceilings and the defensive policies that keep a
malformed or hostile input from crashing the process, exhausting memory, or
escaping its intended scope.

Inputs with a byte ceiling are rejected when they exceed it (or a derived
cache is rebuilt). Canonical Brain manifests have no byte ceiling; their memory
behavior is described below.

## Size ceilings

| Limit | Default | Where | Override |
|---|---|---|---|
| Document transcript (full read) | 256 MiB | `maxDocumentTranscriptBytes` (`safe_read.go`) | — |
| Conversation expansion (line transcript) | streamed; per-line 1 MiB, output 32 KiB | `expandConversationExchange` (`conversation.go`) | — |
| Short-term overlay (`history/short-term.json`) | 64 MiB | `defaultMaxReadBytes` via `loadHistoryShortTermState` | — |
| Filtered conversation candidate scan | 10,000 rows, then a structured degraded error | `historyFTSFilteredScanCeiling` (`history_fts.go`) | — |
| get / multi-get ids per request | 50 | `maxGetBatchIDs` (`retrieve.go`) | — |
| Seed / markdown doc (per file) | 32 MiB | `maxSeedDocBytes` | — |
| Auxiliary JSON state / index / cursor | 16 MiB | `maxManifestBytes` | — |
| Canonical Brain manifest | No byte ceiling | `readBrainManifestFile`; agent setup `readManifestRecord` | — |
| Full semantic snapshot (read) | 2 GiB | `defaultMaxSemanticSnapshotBytes` | `ENTIRE_BRAIN_MAX_SNAPSHOT_BYTES` |
| Gzip cache decompressed output | snapshot cap | `loadCheckpointMetadataCache`, `loadHistoryScanCache` | `ENTIRE_BRAIN_MAX_SNAPSHOT_BYTES` |
| Per NDJSON record (entire-graph) | 16 MiB | `semanticMaxRecordBytes` | `ENTIRE_BRAIN_MAX_RECORD_BYTES` |
| Brain-inspect history line | 4 MiB | `brainInspectHistoryMaxLine` | — |
| Facts NDJSON line | 64 KiB | `factsMaxLineBytes` | — |
| History record line | 1 MiB | `historyMaxLineBytes` | — |
| Ollama distill response | 256 KiB | `distillMaxOutputBytes` | — |
| Ollama embed response | 8 MiB | `maxOllamaEmbedResponseBytes` | — |
| Hook stdin | 1 MiB | `hook_cmd.go` | — |
| Default fallback read | 64 MiB | `defaultMaxReadBytes` (`safe_read.go`) | — |

`safeReadFile` / `safeReadAll` (`safe_read.go`) are the shared helpers: they cap a
read at `max+1` bytes via `io.LimitReader` and error if the source exceeds `max`.

## Canonical manifest loading

Brain's writer and normal readers accept existing manifests larger than 16 MiB.
No rebuild, deletion, JSON compaction, or record truncation is needed to open
these manifests. Refresh, status, doctor, and agent initialization use dedicated
manifest readers; auxiliary state, setup records, configuration, transcripts,
and snapshot limits are unchanged.

The readers decode directly from safely opened regular files and reject malformed
or trailing JSON. CLI reads preserve descriptor identity and alias checks.
Schemas 1–3 remain supported. Normal readers tolerate unknown fields; CLI writers
still refuse to replace a manifest containing fields they cannot round-trip.
Loading retained manifests does not require the source repository or Git.

This is **not bounded-memory streaming**. Go's JSON decoder buffers a complete
JSON value, and CLI readers materialize the manifest structure. Schema validation
and strict decoding use separate passes over the same descriptor, with another
pass when unknown fields require a tolerant retry. The writer also materializes
the encoded JSON before atomic replacement. Memory and processing time grow with
manifest size; there is no explicit manifest resource budget or recoverable
out-of-memory guarantee. Very large manifests remain limited by available process
memory and filesystem performance.

## Resilience policies

- **Panic isolation.** Distill worker goroutines convert a panic into a
  per-chunk error so one bad chunk does not crash the whole run.
- **Tolerant entire-graph ingest.** A stray malformed NDJSON record is skipped,
  counted, and surfaced as a `provider_malformed_record_dropped` warning rather
  than failing the whole index. More than `maxDroppedSemanticRecords` (1000)
  drops is treated as a broken stream and fails. Set
  `ENTIRE_BRAIN_STRICT_INGEST=1` to restore fail-on-first-bad-record.
- **Degrade-to-rebuild.** Index/cache loaders (gzip caches, doc index) return
  empty / rebuild on corrupt input.
- **Corrupt config quarantine.** A `brain.json` that fails to parse is moved
  aside to `brain.json.corrupt` and defaults are used, so a bad config cannot
  wedge repo-key resolution (`internal/config`).

## Path & filesystem safety

- Brain-relative reads and writes reject `..`, absolute paths, and symlinked
  components, and verify the resolved path stays inside the brain root
  (`rejectSymlinkPathComponents` / `rejectExistingSymlinkPathComponents`).
- Lock files (both `internal/cli` and `internal/config`) open with `O_NOFOLLOW`
  and reject symlinks, hardlinks, and open-file aliasing.
- The `checkpoint_remote` repo from `.entire/settings.json` must be a clean
  `owner/name` slug (`validateGitHubRepoSlug`); the fetch host is hardcoded to
  github.com.

## Egress threat model

- No-egress mode (`ENTIRE_BRAIN_NO_EGRESS`, deprecated alias
  `ENTIRE_BRAIN_LOCAL_ONLY`) is
  **fail-closed**: any unrecognized toggle value is treated as enabled (with a
  warning), and only `--agent ollama`/`none` are permitted — any other or
  unknown agent name is denied (`agent_policy.go`).
- Loopback-only HTTP clients (distill) disable proxies, restrict
  redirects to loopback, and re-resolve the dial target at connection time,
  rejecting any non-loopback IP. This defeats `localhost`-by-name trust, hostile
  `/etc/hosts`, and DNS rebinding.
## Fuzzing

`fuzz_test.go` provides panic-invariant fuzz harnesses for the untrusted parsers:
the entire-graph NDJSON stream, the `embed_store` binary loader, the
distilled fact-line parser, and the document-conversation parser. Run e.g.:

```sh
go test ./internal/cli -run '^$' -fuzz FuzzScanSemanticStream -fuzztime 30s
```
