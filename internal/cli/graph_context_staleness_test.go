package cli

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// Absence of the block means the answer is exact. This is the property that
// lets brain ship its half before any graph emits one: an older provider never
// sets it, and nothing about today's behaviour changes.
func TestExactGraphAnswerCarriesNoStaleness(t *testing.T) {
	t.Parallel()
	var none *graphContextStale
	if !none.usable() {
		t.Fatal("an absent stale block was treated as unusable, which would reject every current graph")
	}
	if got := semanticStalenessFor(none, []graphContextSymbol{{FilePath: "internal/auth/token.go"}}, nil); got != nil {
		t.Fatalf("an exact answer reported staleness: %+v", got)
	}
	if none.covers("internal/auth/token.go") {
		t.Fatal("an absent stale block claimed to cover a path")
	}
}

// The requirement brain insisted on: an ancestor answer must name the paths it
// cannot speak for, so a reader never reads "nothing found" there as "nothing
// exists". Those paths had their records dropped from the ancestor store.
func TestStalenessNamesThePathsItCannotSpeakFor(t *testing.T) {
	t.Parallel()
	stale := &graphContextStale{
		AncestorCommit: "abc1234", AncestorTree: "tree1", CommitsBehind: 3,
		ChangedFiles:    []string{"internal/auth/token.go", "internal/billing/"},
		ExcludedRecords: 12,
	}
	if !stale.usable() {
		t.Fatal("a complete stale block was refused")
	}
	symbols := []graphContextSymbol{
		{FilePath: "internal/auth/token.go"},   // changed: unreliable
		{FilePath: "internal/billing/plan.go"}, // under a changed directory
		{FilePath: "internal/server/route.go"}, // untouched
		{FilePath: "internal/auth/token.go"},   // duplicate, must not repeat
	}
	got := semanticStalenessFor(stale, symbols, []graphContextSymbol{{FilePath: "internal/billing/plan_test.go"}})
	if got == nil {
		t.Fatal("a stale answer reported no staleness")
	}
	if got.AncestorCommit != "abc1234" || got.CommitsBehind != 3 {
		t.Fatalf("ancestor identity lost: %+v", got)
	}
	want := []string{"internal/auth/token.go", "internal/billing/plan.go", "internal/billing/plan_test.go"}
	if !reflect.DeepEqual(got.Unreliable, want) {
		t.Fatalf("unreliable paths = %v, want %v", got.Unreliable, want)
	}
	if !reflect.DeepEqual(got.ChangedFiles, stale.ChangedFiles) {
		t.Fatalf("the full changed list was not carried through: %v", got.ChangedFiles)
	}
	if stale.covers("internal/server/route.go") {
		t.Fatal("an untouched file was marked unreliable, which would devalue the signal")
	}
}

// Fail closed. A block claiming records were excluded while naming no files is
// internally inconsistent: brain could not say which answers are incomplete,
// and an unqualified partial answer is worse than falling back. Same for one
// with no ancestor identity.
func TestInconsistentStaleBlockIsRefusedRatherThanTrusted(t *testing.T) {
	t.Parallel()
	for name, stale := range map[string]*graphContextStale{
		"records excluded but no files named": {AncestorCommit: "abc", CommitsBehind: 2, ExcludedRecords: 5},
		"no ancestor commit":                  {CommitsBehind: 2, ChangedFiles: []string{"a.go"}, ExcludedRecords: 1},
		"zero commits behind":                 {AncestorCommit: "abc", CommitsBehind: 0, ChangedFiles: []string{"a.go"}},
	} {
		if stale.usable() {
			t.Errorf("%s: an inconsistent stale block was accepted", name)
		}
	}
	// A block that excluded nothing needs no file list: there is nothing to
	// qualify, so it stays usable.
	harmless := &graphContextStale{AncestorCommit: "abc", CommitsBehind: 1}
	if !harmless.usable() {
		t.Fatal("an ancestor answer that dropped no records was refused")
	}
}

// Directory entries must suppress their children, or a renamed or deleted tree
// would leave every file under it looking reliable.
func TestChangedDirectoryCoversItsChildren(t *testing.T) {
	t.Parallel()
	stale := &graphContextStale{AncestorCommit: "abc", CommitsBehind: 1, ChangedFiles: []string{"internal/billing/"}, ExcludedRecords: 1}
	for _, path := range []string{"internal/billing/plan.go", "./internal/billing/nested/deep.go"} {
		if !stale.covers(path) {
			t.Errorf("%s is under a changed directory but was not covered", path)
		}
	}
	for _, path := range []string{"internal/billing-reports/x.go", "internal/auth/token.go", ""} {
		if stale.covers(path) {
			t.Errorf("%q was covered by a directory it is not under", path)
		}
	}
}

// The JSON contract: the field is omitted entirely for an exact answer, so the
// brief's output is byte-identical to today until a graph starts emitting it.
func TestStalenessIsOmittedFromJSONWhenExact(t *testing.T) {
	t.Parallel()
	exact := brainBriefSemantic{Source: "graph-context"}
	data, err := jsonMarshalForTest(exact)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "staleness") {
		t.Fatalf("an exact brief carried a staleness key: %s", data)
	}
	stale := brainBriefSemantic{Source: "graph-context", Staleness: &semanticContextStaleness{AncestorCommit: "abc", CommitsBehind: 2, ExcludedRecords: 12}}
	data, err = jsonMarshalForTest(stale)
	if err != nil {
		t.Fatal(err)
	}
	// excluded_records is asserted HERE, at the layer an agent actually reads,
	// and not only where the struct is built. It is the field that separates an
	// incomplete answer from a complete one, and this assertion is containment
	// based -- so without naming it, dropping it from the emitted JSON would
	// leave every test in this file green.
	for _, want := range []string{`"staleness"`, `"ancestor_commit":"abc"`, `"commits_behind":2`, `"excluded_records":12`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("stale brief JSON missing %s: %s", want, data)
		}
	}
}

func jsonMarshalForTest(v any) ([]byte, error) { return json.Marshal(v) }

// Brief-level, and the reason this exists: a weakening sweep showed that
// disabling the refusal in runBrainBriefWithRawHistoryMatcher broke no test,
// because the unit guards above cover usable() and not the brief's USE of it.
// A graph answer whose stale block cannot say what it excluded must not serve
// the semantic section at all — it falls through to the internal path, which is
// the same degradation as a missing plugin.
func TestBrainBriefRefusesAnAncestorAnswerThatCannotSayWhatItExcluded(t *testing.T) {
	fixture := newBrainBriefSemanticFreshnessFixture(t)
	runner := fixture.opts.Runner.(*fakeCommandRunner)
	// Records were dropped, but the provider named no files: brain could not
	// qualify a negative, so it must not present the answer.
	runner.responses[graphContextCommandKey(fixture.repoDir)] = fakeCommandResponse{
		stdout: `{"schema_version":"1.1","commit":"aaa111","tree":"tree111",` +
			`"symbols":[{"id":"g1","name":"Serve","kind":"function","file_path":"internal/server/route.go","start_line":1,"end_line":9}],` +
			`"relations":[],"tests":[],"latency_ms":3,` +
			`"stale":{"ancestor_commit":"abc1234","ancestor_tree":"t1","commits_behind":3,"changed_files":[],"excluded_records":12}}`,
	}

	report := runGraphContextBriefJSON(t, fixture)
	if report.Semantic.Source == "graph-context" {
		t.Fatal("the brief served an ancestor answer that could not say which files it excluded")
	}
	if report.Semantic.Staleness != nil {
		t.Fatalf("a refused answer still reported staleness: %+v", report.Semantic.Staleness)
	}
	assertInternalSemanticServed(t, report)
}

// The accepting case at the same level: a COMPLETE stale block is served, and
// the brief carries the staleness so a reader can see what it cannot trust.
func TestBrainBriefServesAnAncestorAnswerWithItsStaleness(t *testing.T) {
	fixture := newBrainBriefSemanticFreshnessFixture(t)
	runner := fixture.opts.Runner.(*fakeCommandRunner)
	runner.responses[graphContextCommandKey(fixture.repoDir)] = fakeCommandResponse{
		stdout: `{"schema_version":"1.1","commit":"aaa111","tree":"tree111",` +
			`"symbols":[{"id":"g1","name":"Serve","kind":"function","file_path":"internal/server/route.go","start_line":1,"end_line":9},` +
			`{"id":"g2","name":"Token","kind":"type","file_path":"internal/auth/token.go","start_line":1,"end_line":4}],` +
			`"relations":[],"tests":[],"latency_ms":3,` +
			`"stale":{"ancestor_commit":"abc1234","ancestor_tree":"t1","commits_behind":3,` +
			`"changed_files":["internal/auth/token.go"],"excluded_records":7}}`,
	}

	report := runGraphContextBriefJSON(t, fixture)
	if report.Semantic.Source != "graph-context" {
		t.Fatalf("a complete ancestor answer was not served: source = %q", report.Semantic.Source)
	}
	stale := report.Semantic.Staleness
	if stale == nil {
		t.Fatal("an ancestor answer was served with no staleness, so a reader cannot tell it is behind")
	}
	if stale.AncestorCommit != "abc1234" || stale.CommitsBehind != 3 {
		t.Fatalf("staleness identity lost: %+v", stale)
	}
	if !reflect.DeepEqual(stale.Unreliable, []string{"internal/auth/token.go"}) {
		t.Fatalf("unreliable = %v, want the one returned symbol in a changed file", stale.Unreliable)
	}
}

// GOLDEN: the exact `stale` block graph #380 emitted on a live run (fresh clone
// of entire-graph @d4a0c186, store built at HEAD~1, queried at HEAD with
// --allow-ancestor 3). Synthetic fixtures prove my own logic; this proves the
// contract against what the producer actually writes, which is the only version
// that matters. If the shape moves, this is what fails.
func TestProducerEmittedStaleBlockParsesAndIsUsable(t *testing.T) {
	t.Parallel()
	const live = `{"schema_version":"1.1","commit":"aaa111","tree":"tree111",` +
		`"symbols":[{"id":"g1","name":"Workflow","kind":"type","file_path":".github/workflows/test.yml","start_line":1,"end_line":3},` +
		`{"id":"g2","name":"Serve","kind":"function","file_path":"internal/server/route.go","start_line":1,"end_line":9}],` +
		`"relations":[],"tests":[],"latency_ms":350,` +
		`"stale":{"ancestor_commit":"8d91390a","ancestor_tree":"65801fbc","commits_behind":1,` +
		`"changed_files":[".github/workflows/test.yml"],"excluded_records":1}}`

	var got graphTaskContext
	if err := json.Unmarshal([]byte(live), &got); err != nil {
		t.Fatalf("the producer's own output does not parse: %v", err)
	}
	if got.Stale == nil {
		t.Fatal("the stale block did not bind; a field name must have drifted")
	}
	if got.Stale.AncestorCommit != "8d91390a" || got.Stale.AncestorTree != "65801fbc" ||
		got.Stale.CommitsBehind != 1 || got.Stale.ExcludedRecords != 1 ||
		len(got.Stale.ChangedFiles) != 1 || got.Stale.ChangedFiles[0] != ".github/workflows/test.yml" {
		t.Fatalf("a field bound to the wrong place: %+v", got.Stale)
	}
	if !got.Stale.usable() {
		t.Fatal("the fail-closed check REJECTED the producer's real output; one of us has to change")
	}

	// And the consumer-facing part: only the changed file is unreliable.
	stale := semanticStalenessFor(got.Stale, got.Symbols, got.Tests)
	if stale == nil {
		t.Fatal("no staleness derived from a real stale block")
	}
	if !reflect.DeepEqual(stale.Unreliable, []string{".github/workflows/test.yml"}) {
		t.Fatalf("unreliable = %v, want only the changed file", stale.Unreliable)
	}
	if stale.CommitsBehind != 1 || stale.AncestorCommit != "8d91390a" {
		t.Fatalf("staleness lost the ancestor identity: %+v", stale)
	}
}

// Unreliable can only name paths the answer RETURNED records for, and an
// ancestor store has already dropped the records for changed files -- so it is
// empty in the common case. Without the excluded count beside it, an
// incomplete answer is indistinguishable from a complete one.
func TestAnEmptyUnreliableListStillReportsThatRecordsWereDropped(t *testing.T) {
	t.Parallel()
	stale := &graphContextStale{
		AncestorCommit:  "abc1234",
		CommitsBehind:   3,
		ChangedFiles:    []string{"internal/billing/invoice.go"},
		ExcludedRecords: 12,
	}
	// The answer returned records, but none from the changed file -- exactly
	// the shape an ancestor store produces.
	got := semanticStalenessFor(stale,
		[]graphContextSymbol{{FilePath: "internal/auth/token.go"}},
		nil)
	if got == nil {
		t.Fatal("a stale block produced no staleness report")
	}
	if len(got.Unreliable) != 0 {
		t.Fatalf("Unreliable = %v, want empty: no returned record is from a changed file", got.Unreliable)
	}
	if got.ExcludedRecords != 12 {
		t.Fatalf("ExcludedRecords = %d, want 12: an empty Unreliable would otherwise read as a complete answer", got.ExcludedRecords)
	}
}
