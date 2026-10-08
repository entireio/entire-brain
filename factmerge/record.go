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
	Author       string    `json:"author,omitempty"`
	Status       string    `json:"status"` // "active" | "superseded" | "retracted"
	Confidence   string    `json:"confidence,omitempty"`
	Provenance   []Anchor  `json:"provenance"` // >=1; retained source/authored anchors
	RelatedIDs   []string  `json:"related_ids,omitempty"`
	SupersededBy string    `json:"superseded_by,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
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
