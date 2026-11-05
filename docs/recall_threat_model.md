# Recall threat model: prompt injection and secret retention

Scope: the conversational-memory and history recall surfaces (CLI retrieval
verbs, brief, bundles/publish, logs, and the derived stores), per
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
- **Receipts and logs**: serve receipts, watch output.

## Adversary channels and enforced boundaries

### 1. Hostile content inside recalled history (prompt injection at recall)

Recalled conversation text is attacker-influencable by construction: anything
a past session read (web pages, repo files, tool output) can plant
instruction-like text that later surfaces in retrieval.

Enforced:

- Every conversation result carries `verification_required: true` plus a
  machine-readable `historical_conversation` caveat on every surface (CLI
  JSON and text rendering) stating recalled content is quoted data,
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
- Deletion originates at the capture layer: `entire checkpoint delete`
  removes a checkpoint from the v1 tip (locally and on checkpoint remotes),
  and the brain mirrors the refs — the next `entire brain refresh` drops the
  session's transcript, history records, conversation exchanges, pattern
  episodes, caches and FTS entries, and the provenance anchors of its
  distilled facts (a distilled fact with no provenance left is deleted;
  authored memories are never touched). "Stop serving now" is
  `entire checkpoint delete <checkpoint> && entire brain refresh`; the watch
  daemon converges projections on its next tick (legacy cached transcript
  files are swept by a full refresh, which the daemon's tick skips). The mirror canary test plants a secret
  in one checkpoint session, deletes it from the tip, refreshes, and asserts
  the secret is absent from the sessions tree, history index, FTS,
  conversation retrieval, and facts while other sessions survive.
- Serve receipts and diagnostics do not log query or conversation bodies.

Residual (tracked):

- **Convergence window**: deletion takes effect at the next refresh, not at
  the next response. A response assembled between the checkpoint deletion and
  the refresh can still carry the deleted session's content (the
  instant-suppression guarantee was deliberately retired with the brain-local
  privacy layer; enforcing it required a per-response lock that failed every
  concurrent reader during long writes).
- **Hosted copies**: facts already synced over the per-fact wire are not
  retracted by a local refresh; the nightly hosted job handles retraction.
- **Session-level delete and anti-resurrection**: `entire checkpoint delete`
  is per-checkpoint and leaves sibling checkpoints of the same session; a
  session-level delete and a merge-surviving deletion record across clones
  belong to the capture layer (entire CLI).
- **Vector stores** hold embeddings of session text; embedding inversion is a
  known class of partial-content recovery. The mirror rebuild regenerates
  them from surviving sessions.

### 3. Hostile repository content

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
2. Retrieval arguments never choose an executable: the graph binary comes
   from the trusted environment, and transcript paths resolve from the trusted
   index only, with canonical-path validation and symlink-component rejection.
3. Publish/bundle output never contains transcripts, history records, or
   conversation text.
4. The brain mirrors the checkpoint refs: a deleted checkpoint's content is
   gone from every projection after the next refresh, held by the mirror
   canary test.
5. Deterministic paths make zero network calls; embedder calls are
   loopback-pinned.

Each invariant is enforced by at least one test today; extend the tests when
extending the surfaces.
