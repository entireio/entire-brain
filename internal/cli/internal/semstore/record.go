// Package semstore is the semantic snapshot/store READ layer: the record
// schema streamed by the Graph provider, the provider schema-version policy,
// the Graph-owned store handle and its read-only SQLite projection, and the
// query/traversal functions every semantic read surface shares. The index
// BUILD pipeline, the semantic commands, and everything that writes the store
// stay in package cli.
package semstore

import "encoding/json"

// CompletenessLevelFromStats extracts completeness_level from the provider's
// trailing summary stats blob (json.RawMessage), or "" if absent.
func CompletenessLevelFromStats(stats json.RawMessage) string {
	if len(stats) == 0 {
		return ""
	}
	var s struct {
		CompletenessLevel string `json:"completeness_level"`
	}
	if err := json.Unmarshal(stats, &s); err != nil {
		return ""
	}
	return s.CompletenessLevel
}

// TrustForCompleteness maps a provider completeness level to a coarse
// retrieval-trust label agents can branch on.
func TrustForCompleteness(level string) string {
	switch level {
	case "ok":
		return "trusted"
	case "degraded":
		return "partial"
	case "unsafe":
		return "low"
	default:
		return "unknown"
	}
}

type Record struct {
	RecordType      string   `json:"record_type"`
	ID              string   `json:"id"`
	Kind            string   `json:"kind"`
	Name            string   `json:"name"`
	QualifiedName   string   `json:"qualified_name"`
	FilePath        string   `json:"file_path"`
	StartLine       int      `json:"start_line"`
	EndLine         int      `json:"end_line"`
	Path            string   `json:"path"`
	Signature       string   `json:"signature"`
	Language        string   `json:"language"`
	FromID          string   `json:"from_id"`
	ToID            string   `json:"to_id"`
	Type            string   `json:"type"`
	WarningCodes    []string `json:"warning_codes"`
	Confidence      float64  `json:"confidence"`
	Reason          string   `json:"reason"`
	StableIDVersion string   `json:"stable_id_version"`
	Blob            string   `json:"blob"`
	Score           int      `json:"score,omitempty"`

	// Schema 1.1 fields. These are omitempty so older (1.0) snapshots round-trip
	// unchanged, but they must be modeled so the streaming filter does not
	// silently drop them when re-marshaling file/symbol/relation records.
	Bytes           int        `json:"bytes,omitempty"`            // file record: source size
	ContainerID     string     `json:"container_id,omitempty"`     // symbol record: enclosing symbol
	BodyHash        string     `json:"body_hash,omitempty"`        // symbol record: content hash
	RelationScope   string     `json:"relation_scope,omitempty"`   // relation record: file|external|...
	Resolution      string     `json:"resolution,omitempty"`       // relation record: exact|type_inferred|name_only
	TargetKind      string     `json:"target_kind,omitempty"`      // relation record: symbol|external
	Evidence        []Evidence `json:"evidence,omitempty"`         // relation record: supporting spans
	EvidenceDropped int        `json:"evidence_dropped,omitempty"` // relation record: supporting spans omitted by provider limit

	// Schema 1.4 field. The provider derives it from Type and Resolution at
	// marshal time, so it adds no information — but it must be modeled so the
	// streaming filter round-trips the literal field.
	Provenance string `json:"provenance,omitempty"` // relation record: EXTRACTED|INFERRED|AMBIGUOUS
}

func (r Record) SemanticPath() string {
	if r.FilePath != "" {
		return r.FilePath
	}
	return r.Path
}

func (r *Record) SetSemanticPath(path string) {
	if r.RecordType == "symbol" {
		r.FilePath = path
		r.Path = ""
		return
	}
	if r.FilePath != "" {
		r.FilePath = path
		return
	}
	r.Path = path
}

// Evidence is a single supporting span for a relation (schema 1.1).
type Evidence struct {
	Kind      string `json:"kind,omitempty"`
	FilePath  string `json:"file_path,omitempty"`
	StartLine int    `json:"start_line,omitempty"`
	EndLine   int    `json:"end_line,omitempty"`
	Detail    string `json:"detail,omitempty"`
}
