// Package factmerge holds the deterministic, LLM-free core of the durable-fact
// store: the content-derived identity of a fact, provenance dedup, the
// merge/supersede decision engine, promotion, retraction, garbage collection,
// and NDJSON (de)serialization. It depends on nothing from internal/cli, no
// cobra, no agent/network, and no filesystem path handling (callers pass
// io.Reader/io.Writer), so both sides of the sync wire — this CLI and the
// hosted brainstore service — run the identical merge semantics. It is
// exported (not internal/) because brainstore imports it.
package factmerge

import "time"

const (
	StatusActive     = "active"
	StatusSuperseded = "superseded"
	StatusRetracted  = "retracted"
	// StatusCandidate is a fact extracted by distill but not yet reconciled
	// against the active set, or awaiting explicit review. Candidates are durable (a crashed run resumes
	// them) but invisible to every consumer that selects StatusActive —
	// recall, sync, promote — so nothing unreconciled ever publishes. The
	// reconcile pass publishes explicit new decisions; destructive decisions
	// stay staged until review. GC retains candidates.
	StatusCandidate = "candidate"
)

// Record is one durable, self-contained statement. The id is content
// derived (sha256 of normalized text + sorted paths) so re-distilling a turn
// is idempotent and dedupe is a map lookup.
type Record struct {
	ID     string   `json:"id"`
	Paths  []string `json:"paths"`           // 1-2 taxonomy paths (topic label)
	Kind   string   `json:"kind,omitempty"`  // decision|invariant|gotcha|preference|convention|closed-negative
	Locus  []string `json:"locus,omitempty"` // code identifiers/paths the fact is about (WHERE)
	Text   string   `json:"text"`            // third person about the user
	Branch string   `json:"branch"`
	Origin string   `json:"origin"` // "distilled" | "authored"
	// Author is display attribution (an account handle), stamped by the hosted
	// pull path from server-side attribution. It is OUTSIDE the content id and
	// outside merge/sort semantics: two records differing only in Author are
	// the same fact.
	Author string `json:"author,omitempty"`
	// Agent/Model/Effort identify the extractor that generated a distilled
	// fact (empty for authored facts), so eval runs can compare fact quality
	// across extraction models. Like Author they are OUTSIDE the content id
	// and merge semantics: a reconcile merge keeps the surviving record's
	// extractor, dropping the removed candidate's. Eval runs distill each
	// model into its own store, so that loss stays out of comparisons.
	Agent      string   `json:"agent,omitempty"`
	Model      string   `json:"model,omitempty"`
	Effort     string   `json:"effort,omitempty"`
	Status     string   `json:"status"` // "active" | "candidate" | "superseded" | "retracted"
	Confidence string   `json:"confidence,omitempty"`
	Provenance []Anchor `json:"provenance"` // >=1; retained source/authored anchors
	// RelatedIDs is the pre-RC.M1 untyped edge bag. Every writer in this
	// package means "unresolved tension, needs review" by it, but consumers
	// read it as generic relatedness. It is retained, and kept in step with
	// Relations, because the sync wire and the hosted brainstore both read it;
	// new writers should add a typed Relation and let the mirror follow.
	RelatedIDs []string `json:"related_ids,omitempty"`
	// Relations are typed record-to-record edges (see Relation). Like
	// RelatedIDs and Author they are OUTSIDE the content id, so attaching one
	// never re-keys a stored record. Older readers ignore the field:
	// ParseNDJSON decodes with plain json.Unmarshal and no
	// DisallowUnknownFields, so a record carrying relations round-trips
	// through a build that predates them without error (it loses the edges,
	// which is why the RelatedIDs mirror exists).
	Relations    []Relation `json:"relations,omitempty"`
	SupersededBy string     `json:"superseded_by,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// Anchor cites the source a fact was derived from or authored against.
// TurnID is populated when the source provides turn-level anchors. Verified is
// retained signed-source metadata; `verify` is read-only and reports local
// verdicts without mutating this bit.
type Anchor struct {
	SessionID    string `json:"session_id"`
	Commit       string `json:"commit,omitempty"`
	CheckpointID string `json:"checkpoint_id,omitempty"`
	TurnID       string `json:"turn_id,omitempty"`    // Phase B
	Transcript   string `json:"transcript,omitempty"` // brain-relative path
	Line         int    `json:"line,omitempty"`       // turn offset in transcript
	Verified     bool   `json:"verified,omitempty"`   // retained signed-source metadata
}
