package entityindex

import (
	"encoding/json"
	"strings"
	"testing"
)

// realDiffEnvelope is the shape `entire graph diff --json` actually emitted on
// this repository, captured rather than imagined: an E_FILE_TOO_LARGE for a
// tracked 16.8 MB JSON file the commit never touched, two info-level
// unsupported-file notes for files it also did not touch, and one range-wide
// W_INDEX_POLICY_CHANGED naming a file that IS in the diff.
const realDiffEnvelope = `{
  "base": "aaa", "head": "bbb", "identity_revision": "4", "schema_version": "1.4",
  "files": [
    {"path": "benchmarks/memory-ladder/.gitignore", "status": "modified", "changes": []},
    {"path": "internal/cli/distill_cmd.go", "status": "modified", "changes": []}
  ],
  "warnings": [
    {"code": "W_INDEX_POLICY_CHANGED", "severity": "warning",
     "file_path": "benchmarks/memory-ladder/.gitignore",
     "effect_on_semantic_completeness": "files that did not change may have entered or left the graph in this range"},
    {"code": "W_UNSUPPORTED_FILE", "severity": "info",
     "file_path": "benchmarks/memory-ladder/questions/global-activation.txt",
     "effect_on_semantic_completeness": "file skipped; no parser for this file type"},
    {"code": "E_FILE_TOO_LARGE", "severity": "warning",
     "file_path": "benchmarks/agent-brain/brainmark/candidates/fresh/SNAPSHOT.json",
     "effect_on_semantic_completeness": "dependent references in this file were not counted"}
  ]
}`

func envelopeWarnings(t *testing.T, raw string) []json.RawMessage {
	t.Helper()
	var envelope struct {
		Warnings []json.RawMessage `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatalf("fixture does not decode: %v", err)
	}
	return envelope.Warnings
}

// TestBlockingWarningsExcusesWhatCannotReachThisDelta.
//
// The unreachable ones go: an oversized file the range never touched cannot
// change whether a function in a changed file was added, removed or modified.
// The range-wide one stays: "files that did not change may have entered or
// left the graph in this range" is precisely the incompleteness that
// fail-closed exists for.
func TestBlockingWarningsExcusesWhatCannotReachThisDelta(t *testing.T) {
	t.Parallel()

	blocking := blockingWarnings([]byte(realDiffEnvelope), envelopeWarnings(t, realDiffEnvelope))
	// Two block, and only the oversize one is excused. The unsupported-file
	// notice is NOT reachability-excused even though its .txt file is absent
	// from the diff: "no parser for this file" is also a reason a touched file
	// would be missing from files[], so that code is left to the metadata
	// rule, which does not cover a questions/*.txt path.
	if len(blocking) != 2 {
		t.Fatalf("expected the range-wide and unsupported-file warnings to block, got %d:\n%s", len(blocking), strings.Join(blocking, "\n"))
	}
	if !strings.Contains(blocking[0], "W_INDEX_POLICY_CHANGED") {
		t.Errorf("the range-wide warning must block: %s", blocking[0])
	}
	for _, got := range blocking {
		if strings.Contains(got, "E_FILE_TOO_LARGE") {
			t.Errorf("the oversize untouched file must be excused: %s", got)
		}
	}
}

// TestBlockingWarningsLetsTheWholeHistoryThroughWhenOnlyUntouchedFilesFailed
// is the case that was actually broken. The oversize warning is a property of
// the snapshot, so it appeared on EVERY commit; five consecutive refusals then
// stopped the run and migration was impossible on this repository.
func TestBlockingWarningsLetsTheWholeHistoryThroughWhenOnlyUntouchedFilesFailed(t *testing.T) {
	t.Parallel()

	const typicalCommit = `{
	  "files": [{"path": "internal/cli/distill_cmd.go", "status": "modified", "changes": []}],
	  "warnings": [
	    {"code": "E_FILE_TOO_LARGE", "severity": "warning",
	     "file_path": "benchmarks/agent-brain/brainmark/candidates/fresh/SNAPSHOT.json"}
	  ]
	}`
	if blocking := blockingWarnings([]byte(typicalCommit), envelopeWarnings(t, typicalCommit)); len(blocking) != 0 {
		t.Fatalf("a typical commit must migrate: %s", strings.Join(blocking, "\n"))
	}
}

// TestBlockingWarningsStillRefusesWhatCouldHaveCostAnEntity. Fail-closed is
// the right instinct and this must not erode it.
func TestBlockingWarningsStillRefusesWhatCouldHaveCostAnEntity(t *testing.T) {
	t.Parallel()

	const files = `"files": [{"path": "a.go", "status": "modified", "changes": []}]`
	for _, tc := range []struct {
		name    string
		warning string
		why     string
	}{
		{"file-local, and the file IS in the diff",
			`{"code": "E_PARSE_ERROR", "severity": "warning", "file_path": "a.go"}`,
			"a parse failure in a file the range changed is exactly a lost entity"},
		{"unknown code with no scope published",
			`{"code": "W_SOMETHING_NEW", "severity": "warning", "file_path": "untouched.go"}`,
			"an unrecognised code's reach is unknown, and unknown reach must not be excused"},
		{"claims file scope but names no file",
			`{"code": "E_FILE_TOO_LARGE", "severity": "warning", "file_path": ""}`,
			"without a file its irrelevance cannot be shown"},
		{"an effect_scope this build does not know",
			`{"code": "E_FILE_TOO_LARGE", "severity": "warning", "file_path": "untouched.go", "effect_scope": "repository"}`,
			"a new scope name is far likelier to mean wider than narrower"},
		{"snapshot scope, even on an untouched file",
			`{"code": "E_FILE_TOO_LARGE", "severity": "warning", "file_path": "untouched.go", "effect_scope": "snapshot"}`,
			"the provider itself says the effect is not confined to that file"},
		{"undecodable",
			`"a bare string, not an object"`,
			"a warning whose reach cannot be established must fail closed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			envelope := `{` + files + `, "warnings": [` + tc.warning + `]}`
			if blocking := blockingWarnings([]byte(envelope), envelopeWarnings(t, envelope)); len(blocking) != 1 {
				t.Fatalf("must refuse — %s", tc.why)
			}
		})
	}
}

// TestBlockingWarningsSeverityAloneExcusesNothing.
//
// This replaces an earlier test of mine that trusted info severity on its own.
// TestMigrationDiffWarnings pins the opposite and is right: an info-level
// "no parser for this file type" naming main.go is a lost function, and the
// provider's severity is not a licence to write incomplete history. Only two
// things excuse a warning now -- the metadata rule, and reachability for the
// snapshot-property codes.
func TestBlockingWarningsSeverityAloneExcusesNothing(t *testing.T) {
	t.Parallel()

	const files = `"files": [{"path": "main.go", "status": "modified", "changes": []}]`
	for _, tc := range []struct {
		name    string
		warning string
	}{
		{"unsupported source file, info",
			`{"code": "W_UNSUPPORTED_FILE", "severity": "info", "file_path": "main.go"}`},
		{"unsupported file absent from the diff, info",
			`{"code": "W_UNSUPPORTED_FILE", "severity": "info", "file_path": "source.unknown"}`},
		{"unknown code, info, on a touched file",
			`{"code": "W_SOME_FUTURE_NOTE", "severity": "info", "file_path": "main.go"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			envelope := `{` + files + `, "warnings": [` + tc.warning + `]}`
			if blocking := blockingWarnings([]byte(envelope), envelopeWarnings(t, envelope)); len(blocking) != 1 {
				t.Fatalf("info severity must not excuse a file that could hold entities: %v", blocking)
			}
		})
	}
}

// TestBlockingWarningsExcusesAnInfoNoteOnAMetadataFile is the other half: the
// metadata rule still lets a go.mod or .gitignore notice through, so a commit
// that touches one still migrates.
func TestBlockingWarningsExcusesAnInfoNoteOnAMetadataFile(t *testing.T) {
	t.Parallel()

	const envelope = `{
	  "files": [{"path": "go.mod", "status": "modified", "changes": []}],
	  "warnings": [{"code": "W_UNSUPPORTED_FILE", "severity": "info", "file_path": "go.mod"}]
	}`
	if blocking := blockingWarnings([]byte(envelope), envelopeWarnings(t, envelope)); len(blocking) != 0 {
		t.Fatalf("a metadata file holds no entities to lose: %s", strings.Join(blocking, "\n"))
	}
}

// TestBlockingWarningsPrefersTheProvidersOwnScope: once entire-graph publishes
// effect_scope (its PR #389), brain must read the provider's answer instead of
// its own code table. A consumer maintaining a private copy of another tool's
// taxonomy is a copy that goes stale silently — so the fallback must lose to
// the published value, in BOTH directions.
func TestBlockingWarningsPrefersTheProvidersOwnScope(t *testing.T) {
	t.Parallel()

	const files = `"files": [{"path": "a.go", "status": "modified", "changes": []}]`

	// A code the table calls file-local, published as snapshot: must refuse,
	// even though the named file is untouched.
	tableSaysFileLocal := `{` + files + `, "warnings": [{"code": "E_FILE_TOO_LARGE", "severity": "warning", "file_path": "untouched.go", "effect_scope": "snapshot"}]}`
	if got := blockingWarnings([]byte(tableSaysFileLocal), envelopeWarnings(t, tableSaysFileLocal)); len(got) != 1 {
		t.Error("the provider's snapshot scope must override this build's file-local guess")
	}

	// A code the table does not know, published as file-local on an untouched
	// file: must be excused, so a new graph release needs no brain change.
	tableDoesNotKnow := `{` + files + `, "warnings": [{"code": "W_JULIA_OVERLOAD_SOMETHING", "severity": "warning", "file_path": "untouched.go", "effect_scope": "file"}]}`
	if got := blockingWarnings([]byte(tableDoesNotKnow), envelopeWarnings(t, tableDoesNotKnow)); len(got) != 0 {
		t.Errorf("a published file scope must be trusted for a code this build has never seen: %s", strings.Join(got, "\n"))
	}
}

// TestBlockingWarningsReadsTheFileListFromTheSameBytes: a rename names the
// file on both sides, and missing old_path would excuse a warning about the
// pre-rename path that the range certainly did touch.
func TestBlockingWarningsCountsBothSidesOfARename(t *testing.T) {
	t.Parallel()

	const renamed = `{
	  "files": [{"path": "internal/cli/command/hook/hook.go", "old_path": "internal/cli/hook_cmd.go", "status": "renamed", "changes": []}],
	  "warnings": [{"code": "E_FILE_TOO_LARGE", "severity": "warning", "file_path": "internal/cli/hook_cmd.go"}]
	}`
	if blocking := blockingWarnings([]byte(renamed), envelopeWarnings(t, renamed)); len(blocking) != 1 {
		t.Fatal("a warning about the pre-rename path must block: the range did touch that file")
	}
}

// TestBlockingWarningsOnNoWarningsAndNoFiles keeps the degenerate paths honest.
func TestBlockingWarningsDegenerateInputs(t *testing.T) {
	t.Parallel()

	if got := blockingWarnings([]byte(`{"files":[],"warnings":[]}`), nil); got != nil {
		t.Errorf("no warnings must mean nothing blocks: %v", got)
	}
	// An envelope whose files list cannot be read yields no touched set, so
	// every file-local warning is excused for naming an untouched file. That
	// is safe only because diffCommit already refuses an envelope with no
	// files key before reaching here; assert the companion guard exists rather
	// than assume it.
	unreadable := `{"warnings":[{"code":"E_FILE_TOO_LARGE","severity":"warning","file_path":"x.json"}]}`
	if got := blockingWarnings([]byte(unreadable), envelopeWarnings(t, unreadable)); len(got) != 0 {
		t.Errorf("unexpected: %v", got)
	}
}
