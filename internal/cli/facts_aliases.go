package cli

import (
	"time"

	"github.com/ashtom/entire-brain/internal/factmerge"
)

// This file is the thin compatibility shim between internal/cli and the
// extracted, LLM-free durable-fact merge core in internal/factmerge. The core
// types, consts, and pure functions moved verbatim; the aliases and forwarding
// wrappers here keep the ~70 existing CLI call sites (and their golden tests)
// compiling and behaving identically against the old lowercase names.

// Type aliases (not new named types) so struct identity and JSON tags are the
// factmerge types exactly — a *factmerge.Record IS a factRecord.
type (
	factRecord   = factmerge.Record
	factAnchor   = factmerge.Anchor
	factAction   = factmerge.Action
	factProposal = factmerge.Proposal
	gcResult     = factmerge.GCResult
)

// Const aliases for the moved identifiers the CLI still references by their
// old lowercase names.
const (
	factStatusActive     = factmerge.StatusActive
	factStatusSuperseded = factmerge.StatusSuperseded
	factStatusRetracted  = factmerge.StatusRetracted

	factActionNew       = factmerge.ActionNew
	factActionMerge     = factmerge.ActionMerge
	factActionSupersede = factmerge.ActionSupersede

	defaultFactConfidenceThreshold = factmerge.DefaultConfidenceThreshold

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

func validFactPath(path string) bool { return factmerge.ValidPath(path) }

func normalizeFactPaths(paths []string) []string { return factmerge.NormalizePaths(paths) }

func validFactKind(kind string) bool { return factmerge.ValidFactKind(kind) }

func upsertFact(records []factRecord, incoming factRecord) []factRecord {
	return factmerge.Upsert(records, incoming)
}

func sortFactRecords(records []factRecord) { factmerge.Sort(records) }

func indexOfFact(records []factRecord, id string) int { return factmerge.IndexOf(records, id) }

func applyFactActions(active []factRecord, actions []factAction, threshold float64, now time.Time) ([]factRecord, []factProposal) {
	return factmerge.ApplyActions(active, actions, threshold, now)
}

func applyProposal(facts []factRecord, p factProposal, now time.Time) ([]factRecord, error) {
	return factmerge.ApplyProposal(facts, p, now)
}

func rejectProposal(facts []factRecord, p factProposal) ([]factRecord, error) {
	return factmerge.RejectProposal(facts, p)
}

func promoteFacts(source, target []factRecord, strategy, intoBranch string, now time.Time) ([]factRecord, []factProposal, int) {
	return factmerge.Promote(source, target, strategy, intoBranch, now)
}

func retractFact(facts []factRecord, id string, now time.Time) (found, changed bool) {
	return factmerge.Retract(facts, id, now)
}

// gcFacts injects the taxonomy's top-level membership into the pure GC core so
// factmerge stays free of the taxonomy loader.
func gcFacts(facts []factRecord, taxonomy factTaxonomy, now time.Time, retention time.Duration) gcResult {
	return factmerge.GC(facts, func(path string) bool {
		return factPathKnownTopLevel(taxonomy, path)
	}, now, retention)
}
