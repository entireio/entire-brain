package entityindex

import (
	"encoding/json"
	"strings"
)

// Which provider warnings may refuse a migration, and which may not.
//
// `entities migrate` re-derives stored deltas under a new parser identity, so
// it fails closed: a delta computed from an incomplete diff would be written
// as authoritative history. That instinct is right. Refusing on the PRESENCE
// of any warning is not, because the provider reports a warning per
// unparseable file in the whole-repository snapshot it builds at each end of
// the range -- not only for files the range touched.
//
// Measured on entire-brain: a tracked 16.8 MB SNAPSHOT.json trips
// E_FILE_TOO_LARGE on EVERY commit's diff, because the limit is a property of
// the snapshot, not of the commit. Three unrelated commits sampled across the
// history each carried exactly one non-info warning and it was that one. So
// every commit refused, five consecutive refusals stopped the run, and
// migration was impossible on this repository for a reason that cannot cost
// the delta a single entity: dependent references missed inside an untouched
// JSON data file cannot change whether a function in a changed file was added,
// removed or modified.
//
// The test is reachability, and the diff envelope already carries what it
// needs. A warning may be excused only when it is file-local AND names a file
// absent from this diff's own file list. Anything whose effect reaches beyond
// one file still refuses -- W_INDEX_POLICY_CHANGED, for instance, reports that
// "files that did not change may have entered or left the graph in this
// range", which is exactly the incompleteness fail-closed exists for.
//
// TWO CODES ARE DELIBERATELY NOT REACHABILITY-EXCUSED, and this is the merge
// with the metadata rule that landed separately on main. W_UNSUPPORTED_FILE
// and E_PARSE_ERROR both mean "I could not read this file", which is also a
// reason the file may be MISSING from files[] even though the range touched
// it. For those two, absence from the file list is not evidence of
// irrelevance, so they are left to isUnsupportedMetadataWarning, which
// excuses only an info-level unsupported-file notice naming a known metadata
// file (.gitignore, go.mod, ...) -- a file that holds no entities to lose.
//
// That is why severity alone excuses nothing here. An info-level notice about
// a SOURCE file is pinned as refusing by TestMigrationDiffWarnings, and it is
// right: "no parser for main.go" is a lost function, whatever severity the
// provider attaches. Reachability keeps E_FILE_TOO_LARGE and E_MINIFIED,
// where the provider has identified the file by path and the limit is a
// property of the snapshot rather than of the range -- which is the case that
// actually made migration impossible on this repository.

// providerWarning is the subset of a provider warning this needs. Everything
// is optional: an older provider emits no effect_scope, and some warnings
// carry no file at all.
type providerWarning struct {
	Code        string `json:"code"`
	Severity    string `json:"severity"`
	FilePath    string `json:"file_path"`
	EffectScope string `json:"effect_scope"`
}

// fileLocalWarningCodes are the codes known to affect exactly the file they
// name, for providers that do not publish effect_scope yet.
//
// This list is a FALLBACK with a shelf life, not the contract. entire-graph
// PR #389 publishes effect_scope ("file" | "snapshot") on every warning, keyed
// in capabilities as optional_local_only_features.partial_failure_effect_scope.
// Once that is the installed provider, scopeOf below reads the provider's own
// answer and this map stops being consulted -- which is the right outcome,
// because a consumer maintaining its own copy of another tool's severity
// taxonomy is a copy that goes stale silently.
var fileLocalWarningCodes = map[string]bool{
	"E_FILE_TOO_LARGE": true,
	"E_MINIFIED":       true,
}

// blockingWarnings returns the warnings that could have cost this delta an
// entity, in the order the provider reported them.
//
// stdout is the raw envelope, because the files list has to be read from the
// SAME bytes the warnings came from. Reading it from a separately decoded
// value would make the two halves of this judgement independently
// substitutable, and the whole point is that they agree.
func blockingWarnings(stdout []byte, warnings []json.RawMessage) []string {
	if len(warnings) == 0 {
		return nil
	}
	touched := diffFilePaths(stdout)
	var blocking []string
	for _, raw := range warnings {
		// An unsupported-file notice about a known metadata file is excused by
		// its own rule (isUnsupportedMetadataWarning), which owns every
		// W_UNSUPPORTED_FILE decision. Reachability deliberately does not
		// second-guess it: see the note above on why that rule is stricter.
		if isUnsupportedMetadataWarning(raw) {
			continue
		}
		var warning providerWarning
		if err := json.Unmarshal(raw, &warning); err != nil {
			// An undecodable warning is a warning whose reach cannot be
			// established. Fail closed on it: this is the one place where
			// guessing would write incomplete history as authoritative.
			blocking = append(blocking, string(raw))
			continue
		}
		if warningIsBlocking(warning, touched) {
			blocking = append(blocking, string(raw))
		}
	}
	return blocking
}

// warningIsBlocking decides one warning.
func warningIsBlocking(warning providerWarning, touched map[string]bool) bool {
	if scopeOf(warning) != warningScopeFile {
		// Snapshot-wide, range-wide, or unknown reach: refuse.
		return true
	}
	file := strings.TrimSpace(warning.FilePath)
	if file == "" {
		// Claims to be file-local but names no file, so its irrelevance
		// cannot be shown.
		return true
	}
	return touched[file]
}

type warningScope int

const (
	warningScopeWider warningScope = iota
	warningScopeFile
)

// scopeOf prefers the provider's own effect_scope and falls back to the code
// table only when the provider does not publish one. An effect_scope this
// build does not recognise counts as wider-than-file: a new scope name is far
// more likely to mean "reaches further" than "reaches less".
func scopeOf(warning providerWarning) warningScope {
	switch strings.ToLower(strings.TrimSpace(warning.EffectScope)) {
	case "file":
		return warningScopeFile
	case "":
		if fileLocalWarningCodes[strings.ToUpper(strings.TrimSpace(warning.Code))] {
			return warningScopeFile
		}
		return warningScopeWider
	default:
		return warningScopeWider
	}
}

// diffFilePaths is every path this diff claims to cover, both sides of a
// rename. A path missing from it is a path the range did not touch.
func diffFilePaths(stdout []byte) map[string]bool {
	var envelope struct {
		Files []struct {
			Path    string `json:"path"`
			OldPath string `json:"old_path"`
		} `json:"files"`
	}
	if err := json.Unmarshal(stdout, &envelope); err != nil {
		return nil
	}
	touched := make(map[string]bool, len(envelope.Files)*2)
	for _, file := range envelope.Files {
		for _, path := range []string{file.Path, file.OldPath} {
			if trimmed := strings.TrimSpace(path); trimmed != "" {
				touched[trimmed] = true
			}
		}
	}
	return touched
}
