# Input limits & untrusted-input hardening

Entire Brain ingests data from sources it does not control: Entire session
transcripts, the `entire-graph` snapshot stream, repository files, checkpoint
history fetched over git, MCP requests, and on-disk derived artifacts. This
document records the size/time ceilings and the defensive policies that keep a
malformed or hostile input from crashing the process, exhausting memory, or
escaping its intended scope.

All limits degrade gracefully: a file/record over its ceiling is rejected (or a
derived cache is rebuilt) rather than silently truncated or allowed to OOM.

## Size ceilings

| Limit | Default | Where | Override |
|---|---|---|---|
| MCP frame | 4 MiB | `maxMCPFrameBytes` (`mcp.go`) | — |
| Document transcript (full read) | 256 MiB | `maxDocumentTranscriptBytes` (`safe_read.go`) | — |
| Conversation expansion (line transcript) | streamed; per-line 1 MiB, output 32 KiB | `expandConversationExchange` (`conversation.go`) | — |
| Short-term overlay (`history/short-term.json`) | 64 MiB | `defaultMaxReadBytes` via `loadHistoryShortTermState` | — |
| Filtered conversation candidate scan | 10,000 rows, then a structured degraded error | `historyFTSFilteredScanCeiling` (`history_fts.go`) | — |
| Vector KNN neighborhood (brain_cgo) | 4,096 | `vec0KnnMaxK` (`embed_vec_cgo.go`); filtered vector search is approximate past it | — |
| get / multi-get ids per request | 50 | `maxGetBatchIDs` (`retrieve.go`) | — |
| Seed / markdown doc (per file) | 32 MiB | `maxSeedDocBytes` | — |
| JSON manifest / index / cursor | 16 MiB | `maxManifestBytes` | — |
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

## Resilience policies

- **Panic isolation.** The long-lived MCP server recovers from any handler panic
  and returns JSON-RPC `-32603` instead of tearing down the stdio session
  (`handleMCPMessage`). Distill worker goroutines convert a panic into a
  per-chunk error so one bad chunk does not crash the whole run.
- **Tolerant entire-graph ingest.** A stray malformed NDJSON record is skipped,
  counted, and surfaced as a `provider_malformed_record_dropped` warning rather
  than failing the whole index. More than `maxDroppedSemanticRecords` (1000)
  drops is treated as a broken stream and fails. Set
  `ENTIRE_BRAIN_STRICT_INGEST=1` to restore fail-on-first-bad-record.
- **Degrade-to-rebuild.** Vector/index/cache loaders (`embed_store`, vec store,
  gzip caches, doc index) return empty / rebuild on corrupt input. The
  `embed_store` binary loader preflights its length prefix against the remaining
  file bytes before trusting it.
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

- No-egress mode (`ENTIRE_BRAIN_NO_EGRESS` / `ENTIRE_BRAIN_LOCAL_ONLY`) is
  **fail-closed**: any unrecognized toggle value is treated as enabled (with a
  warning), and only `--agent ollama`/`none` are permitted — any other or
  unknown agent name is denied (`agent_policy.go`).
- Loopback-only HTTP clients (distill + embedder) disable proxies, restrict
  redirects to loopback, and re-resolve the dial target at connection time,
  rejecting any non-loopback IP. This defeats `localhost`-by-name trust, hostile
  `/etc/hosts`, and DNS rebinding.
- The MCP `brain_index_repository` tool resolves its indexer binary from the
  trusted server environment (`ENTIRE_BRAIN_GRAPH_BINARY`, default `entire`), never
  from a client argument, and constrains the index path to the bound repo root
  unless `ENTIRE_BRAIN_MCP_ALLOW_ANY_PATH` is set.
- The MCP surface is scoped to the repository the server was bound to
  (`ENTIRE_REPO_ROOT`): `brain_delete_project` refuses a foreign `repo_key`,
  `brain_list_projects` returns only the bound project, and
  `brain_workspace_graph`/`_regressions`/`_review` refuse a workspace containing
  repos outside that root — each unless the operator sets
  `ENTIRE_BRAIN_MCP_ALLOW_CROSS_REPO`. This closes the confused-deputy path where
  an agent working in repo A irreversibly erases repo B's brain, or enumerates
  every other project on the machine. The plain `entire brain ...` CLI keeps its
  cross-repo verbs: a human at a terminal is not the confused deputy.

## Fuzzing

`fuzz_test.go` provides panic-invariant fuzz harnesses for the untrusted parsers:
MCP framing, the entire-graph NDJSON stream, the `embed_store` binary loader, the
distilled fact-line parser, and the document-conversation parser. Run e.g.:

```sh
go test ./internal/cli -run '^$' -fuzz FuzzScanSemanticStream -fuzztime 30s
```
