package cli

import (
	"time"

	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
)

// This file is the thin compatibility shim between internal/cli and the
// extracted, LLM-free durable-fact merge core in factmerge. The core
// types, consts, and pure functions moved verbatim; the aliases and forwarding
// wrappers here keep the ~70 existing CLI call sites (and their golden tests)
// compiling and behaving identically against the old lowercase names.

// Type aliases (not new named types) so struct identity and JSON tags are the
// factmerge types exactly — a *factmerge.Record IS a factRecord.
type (
	factRecord = factmerge.Record
	factAnchor = factmerge.Anchor
	factAction = factmerge.Action
	gcResult   = factmerge.GCResult
)

// Const aliases for the moved identifiers the CLI still references by their
// old lowercase names.
const (
	factStatusActive     = factmerge.StatusActive
	factStatusSuperseded = factmerge.StatusSuperseded
	factStatusRetracted  = factmerge.StatusRetracted
	factStatusCandidate  = factmerge.StatusCandidate

	factActionNew       = factmerge.ActionNew
	factActionMerge     = factmerge.ActionMerge
	factActionSupersede = factmerge.ActionSupersede

	factKindDecision       = factmerge.KindDecision
	factKindInvariant      = factmerge.KindInvariant
	factKindGotcha         = factmerge.KindGotcha
	factKindPreference     = factmerge.KindPreference
	factKindConvention     = factmerge.KindConvention
	factKindClosedNegative = factmerge.KindClosedNegative
)

// Forwarding wrappers preserve the old lowercase call sites verbatim.

func factRecordID(text string, sortedPaths []string) string {
	return factmerge.RecordID(text, sortedPaths)
}

func factTextOverlap(a, b string) float64 { return factmerge.TextOverlap(a, b) }

func validFactPath(path string) bool { return factmerge.ValidPath(path) }

func normalizeFactPaths(paths []string) []string { return factmerge.NormalizePaths(paths) }

func validFactKind(kind string) bool { return factmerge.ValidFactKind(kind) }

func upsertFact(records []factRecord, incoming factRecord) []factRecord {
	return factmerge.Upsert(records, incoming)
}

func dedupFactRecords(records []factRecord) ([]factRecord, int) {
	return factmerge.DedupByID(records)
}

func sortFactRecords(records []factRecord) { factmerge.Sort(records) }

func indexOfFact(records []factRecord, id string) int { return factmerge.IndexOf(records, id) }

func retractFact(facts []factRecord, id string, now time.Time) (found, changed bool) {
	return factmerge.Retract(facts, id, now)
}

// gcFacts injects the taxonomy's top-level membership into the pure GC core so
// factmerge stays free of the taxonomy loader.
func gcFacts(facts []factRecord, taxonomy factstore.Taxonomy, now time.Time, retention time.Duration) gcResult {
	return factmerge.GC(facts, func(path string) bool {
		return factstore.PathKnownTopLevel(taxonomy, path)
	}, now, retention)
}
