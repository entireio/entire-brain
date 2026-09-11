# Recall threat model: prompt injection and secret retention

Scope: the conversational-memory and history recall surfaces (CLI retrieval
verbs, MCP tools, brief, bundles/publish, logs, and the derived stores), per
Phase 4 deliverable 7 of the conversational-memory plan. This documents what
the code enforces today, where the boundaries are, and the residual risks with
their tracking state. It is a living document: change the code, change this.

## Assets

- **Captured transcripts** (exported copies under the brain's `sessions/`):
  may contain secrets typed or pasted during sessions, hostile text copied
  from the web or from repo content, and instruction-like text of any origin.
- **Derived projections**: history index (classic records + conversation
  exchange projections), FTS stores, vector stores, scan caches, pattern
  episodes/corpus, durable facts, distill caches.
- **Published artifacts**: brain bundles (semantic snapshots, overlays, facts).
- **Receipts and logs**: serve receipts, watch output, MCP debug log.

## Adversary channels and enforced boundaries

### 1. Hostile content inside recalled history (prompt injection at recall)

Recalled conversation text is attacker-influencable by construction: anything
a past session read (web pages, repo files, tool output) can plant
instruction-like text that later surfaces in retrieval.

Enforced:

- Every conversation result carries `verification_required: true` plus a
  machine-readable `historical_conversation` caveat on every surface (CLI
  JSON, MCP JSON, text rendering) stating recalled content is quoted data,
  never instructions. The canary test proves an instruction-like string is
  returned only inside this wrapper.
- Tool and skill descriptions never instruct an agent to obey recalled
  content; the recall skill's safety contract is test-locked
  (`TestRecallSkillTemplatesEmbeddedAndSafe`).
- Hidden reasoning, raw tool output, and injected wrapper/pseudo-user
  messages are excluded from exchange text at extraction; harness noise (API
  error envelopes, hook injections) is filtered.
- `matched_terms` explainability exposes why weak matches surfaced, so
  low-signal bait is diagnosable.

Residual: the caveat is advisory; a consuming agent that ignores it can
still be injected. That boundary belongs to consuming harnesses; this repo's
obligation is that no recall surface ever presents historical text as
instructions, which is test-enforced.

### 2. Secret retention and deletion

Enforced:

- Local-first by default; publish bundles contain only semantic snapshots,
  overlays, and facts; never transcripts, history records, or conversation
  text (verified by construction: the bundle collector enumerates its
  artifact kinds).
- `privacy exclude` uses a content-free tombstone policy that fails closed:
  an absent policy means no exclusions, while an unreadable, malformed, unsafe,
  or unknown-newer policy blocks conversation reads and derivation until it is
  repaired. The policy is understood BEFORE derived indexing everywhere
  (history index, short-term overlay, episodes, pattern corpus, distill) AND at
  retrieval time: every
  conversation/history/fact retrieval, get, and brief boundary consults the
  tombstone set, so an excluded session is unreadable immediately, even while
  cleanup or a rebuild is still running. Exclude runs the same derived
  cleanup as purge (facts, episodes, pattern outputs, caches, FTS/vector
  stores) while keeping the exported transcript.
- `privacy purge --dry-run` predicts exact artifacts/bytes from the same
  shared inventory execution and verification use (including the short-term
  overlay); purge is tombstone-first (crash leaves the session excluded,
  never resurrected), idempotent, survives re-export, propagates every
  deletion failure as a non-zero, resumable error naming the artifact, and
  removes: the exported transcript, all index generations, FTS/scan-cache/
  vector-store files and known SQLite sidecars, single-source facts
  (multi-source facts lose the purged anchor; dangling proposals pruned),
  pattern episodes and derived pattern outputs including publication recovery
  state and the runs log, distill-cache entries, content-free lifecycle jobs and
  cancellation markers, optional abstracts, and their metadata-only egress
  receipts. The canary test walks EVERY file under the brain dir afterward.
  `privacy verify` also treats surviving work/generated state, incomplete
  pattern publication, or any derived store older than the newest tombstone as
  dirty rather than success.
- Vector stores hold embeddings of session text; embedding inversion is a
  known class of partial-content recovery, so purge deletes the store files
  rather than reasoning about per-row deletion.
- Serve receipts and diagnostics do not log query or conversation bodies; the
  MCP debug log records tool names and a safe-args allowlist only.

Residual (tracked):

- **Pattern/theme read surfaces**: pattern outputs are cross-session
  aggregates without per-row session identity, so they cannot be filtered by
  tombstone at row-read time. Exclusion closes those surfaces while the derived
  store is dirty, then republishes the complete corpus from tombstone-filtered
  truth under the shared privacy/publication boundary.

- **`facts sync` git-meta store**: keep-both merge retains synced copies of
  purged facts and can merge them back. The purge plan reports an explicit
  "NOT purged" caveat naming the store; real deletion semantics are a
  factsync protocol change (parking lot).
- **Canonical capture**: the checkpoints ref remains the capture layer's
  data; brain purge does not rewrite it, and a re-export restores the raw
  transcript copy (not the projections; the tombstone holds) until a
  capture-layer exclusion contract exists (parking lot, plan open decision).
- **Retention** (`privacy retention --max-age`, optionally per branch,
  exclude or purge) exists for age-based policy; anything finer stays
  per-session and explicit.

### 3. Malicious or compromised MCP client

Enforced:

- Tool arguments never choose an executable or a filesystem path: the graph
  binary comes from the trusted server environment; index paths are
  normalized against the bound repo root with containment re-checked on the
  resolved path; `brain_get` resolves transcript paths from the trusted index
  only, with canonical-path validation, a `sessions/` containment check, and
  symlink-component rejection.
- Tool arguments never reach another repository's brain: `brain_delete_project`
  refuses a `repo_key` other than the bound repo's (`confirm=true` is not a
  defence — the same injected agent supplies it), `brain_list_projects` returns
  only the bound project instead of enumerating every local repo's key, path,
  and counts, and the `brain_workspace_*` fan-out refuses a workspace the bound
  repository is not a member of (membership is what stops an agent in repo A
  reaching an unrelated repo B), or one whose repos live outside the bound
  repository's parent directory — sibling checkouts under a common parent stay
  in scope, since that is the layout every real workspace has. Each is opt-out
  via
  `ENTIRE_BRAIN_MCP_ALLOW_CROSS_REPO`, and each applies to the MCP surface only:
  the `entire brain ...` CLI stays cross-repo because the operator there is a
  human, not a deputy acting on injected instructions.
- Argument names are validated against each tool's declared schema (single
  source of truth); input frames, transcript reads (streamed for line
  transcripts, bounded for document form), parser lines, and per-exchange
  result bytes are bounded; one malformed frame answers with a parse error
  instead of killing the server; handler panics are recovered per request.

### 4. Hostile repository content

Seed/docs/semantic layers index repo content that may itself be hostile.
Historical documents carry their own `historical_document` caveat; the
conversation layer never treats repo text as instructions (it only quotes
what sessions said about it). No-egress mode (`ENTIRE_BRAIN_NO_EGRESS`)
enforces locality for the deterministic paths and loopback-pinned embedder
dials (URL validation + redirect checks + resolved-dial-target pinning), so
hostile content cannot exfiltrate via the embedder channel.

## Invariants a change must not break

1. No retrieval surface returns conversation content without
   `verification_required` + the historical-evidence caveat.
2. No MCP argument reaches a filesystem path or executable choice, and no MCP
   argument reaches another repository's brain (read or delete) unless the
   operator set `ENTIRE_BRAIN_MCP_ALLOW_CROSS_REPO`.
3. Publish/bundle output never contains transcripts, history records, or
   conversation text.
4. Tombstones are honored before every derived build AND consulted at every
   retrieval boundary; purge canary-absence holds across every file under
   the brain dir.
5. Deterministic paths make zero network calls; embedder calls are
   loopback-pinned.

Each invariant is enforced by at least one test today; extend the tests when
extending the surfaces.
