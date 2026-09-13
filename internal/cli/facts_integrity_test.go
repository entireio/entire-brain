package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// facts_integrity_test.go pins the cross-check that was missing: the manifest
// declares N facts, the store yields M, and until now nothing compared them.
// Five facts go in; one, some or all of them are then replaced with content
// the store will read but that is not a fact. Every health surface must stop
// reporting the declared number as though it had been observed.

// factLossFixture builds a brain holding five authored facts on the branch the
// fixture runner reports as checked out, with sources.facts derived from disk.
// Returns the options, the repo dir, the brain dir and the facts.ndjson path.
func factLossFixture(t *testing.T) (Options, string, string, string) {
	t.Helper()
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	env.RepoRoot = repoDir
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	now := time.Date(2026, 8, 10, 2, 0, 0, 0, time.UTC)
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return now }}
	storage, err := repoStoragePaths(context.Background(), runner, env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(storage.BrainDir, 0o700); err != nil {
		t.Fatal(err)
	}
	texts := []string{
		"the alpha subsystem stores checkpoints in postgres",
		"beta must never run before alpha has initialized",
		"the project targets go 1.22 and formats with gofmt -s",
		"every commit runs the full go test suite first",
		"release branches are never rebased once cut",
	}
	facts := make([]factRecord, 0, len(texts))
	for i, text := range texts {
		facts = append(facts, factRecord{
			ID:         factRecordID(text, []string{"project.tooling.stack"}),
			Paths:      []string{"project.tooling.stack"},
			Text:       text,
			Branch:     "feature",
			Origin:     factOriginAuthored,
			Status:     factStatusActive,
			Provenance: []factAnchor{{SessionID: "session-1"}},
			CreatedAt:  now.Add(time.Duration(i) * time.Minute),
			UpdatedAt:  now.Add(time.Duration(i) * time.Minute),
		})
	}
	if err := writeFacts(storage.BrainDir, "feature", facts); err != nil {
		t.Fatal(err)
	}
	if err := updateFactSourceManifest(storage.BrainDir, now); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(storage.BrainDir, filepath.FromSlash(factsFileRelPath("feature")))
	return opts, repoDir, storage.BrainDir, path
}

// replaceFactLines overwrites the given 0-based line numbers of a facts.ndjson
// with the replacement text, leaving every other line untouched. The store
// stays readable; only its content stops being facts. It returns the ids the
// replacement destroyed, because writeFacts sorts the store and a caller
// cannot know from the line number alone which fact it just lost.
func replaceFactLines(t *testing.T, path, replacement string, lines ...int) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	existing := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	var lost []string
	for _, n := range lines {
		if n < 0 || n >= len(existing) {
			t.Fatalf("line %d out of range for a %d-line store", n, len(existing))
		}
		var record factRecord
		if err := json.Unmarshal([]byte(existing[n]), &record); err != nil {
			t.Fatalf("line %d is not a fact to begin with: %v", n, err)
		}
		lost = append(lost, record.ID)
		existing[n] = replacement
	}
	if err := os.WriteFile(path, []byte(strings.Join(existing, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return lost
}

// TestFactStoreIntegrityDetectsOneLostFactAmongFive is the headline case: one
// of five lines is replaced with valid JSON that is not a fact record. The
// store still reads, so every surface used to report the manifest's 5.
func TestFactStoreIntegrityDetectsOneLostFactAmongFive(t *testing.T) {
	opts, repoDir, brainDir, path := factLossFixture(t)

	lost := replaceFactLines(t, path, `{"hello":"world"}`, 0)

	// One subtest per surface, so a regression names the surface it broke.
	t.Run("cross-check", func(t *testing.T) {
		integrity := inspectBrainFactStore(brainDir)
		if integrity.OK() {
			t.Fatalf("integrity reports a healthy store after a fact was overwritten: %+v", integrity)
		}
		if integrity.Mode != factStoreDefectLossy {
			t.Fatalf("mode = %q, want %q", integrity.Mode, factStoreDefectLossy)
		}
		if integrity.Declared != 5 || integrity.Readable != 4 || integrity.Lost() != 1 {
			t.Fatalf("declared/readable/lost = %d/%d/%d, want 5/4/1", integrity.Declared, integrity.Readable, integrity.Lost())
		}
		if integrity.Unusable != 1 {
			t.Fatalf("unusable = %d, want 1", integrity.Unusable)
		}
		if !strings.Contains(integrity.Warning(), "5 declared, 4 readable") {
			t.Fatalf("warning does not carry both counts: %q", integrity.Warning())
		}
		if !strings.Contains(integrity.Warning(), "entire brain refresh") {
			t.Fatalf("warning does not name the repair command: %q", integrity.Warning())
		}
	})

	// status: the identity line must not print the declared number alone, and
	// the report must carry the defect for JSON/MCP readers.
	t.Run("status", func(t *testing.T) {
		report, err := buildAvailableBrainStatusReport(context.Background(), opts, repoDir)
		if err != nil {
			t.Fatalf("status report: %v", err)
		}
		if report.Facts == nil || report.Facts.Integrity == nil {
			t.Fatalf("status facts carries no integrity verdict: %+v", report.Facts)
		}
		if report.Facts.Readable() != 4 {
			t.Fatalf("status facts readable = %d, want 4", report.Facts.Readable())
		}
		if !warningsMention(report.Warnings, "4 readable") {
			t.Fatalf("status warnings do not name the loss: %v", report.Warnings)
		}
		statusOut, err := execute(t, NewRootCommand(opts), "status")
		if err != nil {
			t.Fatalf("status: %v\n%s", err, statusOut)
		}
		if !strings.Contains(statusOut, "5 facts declared, 4 readable") {
			t.Fatalf("status identity line still reports the declared count as observed:\n%s", statusOut)
		}
	})

	// doctor: an error-level finding (the exit gate is `doctor --fail-on`).
	t.Run("doctor", func(t *testing.T) {
		checks, _ := brainDoctorReadOnlyReport(context.Background(), opts, repoDir)
		state, ok := factsDoctorCheckState(checks)
		if !ok {
			t.Fatalf("doctor has no facts check: %+v", checks)
		}
		if state != "error" {
			t.Fatalf("doctor facts = %q, want error", state)
		}
	})

	// verify: its whole job. It must not answer "0 orphaned" and exit 0.
	t.Run("verify", func(t *testing.T) {
		verifyOut, verifyErr := execute(t, NewRootCommand(opts), "verify", "--branch", "feature")
		if verifyErr == nil {
			t.Fatalf("verify passed over a store that lost a fact:\n%s", verifyOut)
		}
		if !strings.Contains(verifyOut, "store integrity") {
			t.Fatalf("verify did not report the store defect:\n%s", verifyOut)
		}
		if !strings.Contains(verifyOut, "5 declared, 4 readable") {
			t.Fatalf("verify did not name the shortfall:\n%s", verifyOut)
		}
	})

	// facts map: the outline's "(N facts)" is a count of survivors.
	t.Run("facts-map", func(t *testing.T) {
		mapOut, mapErr := execute(t, NewRootCommand(opts), "facts", "map", "--branch", "feature")
		if mapErr == nil {
			t.Fatalf("facts map exited clean over a lossy store:\n%s", mapOut)
		}
		if !strings.Contains(mapOut, "5 declared, 4 readable") {
			t.Fatalf("facts map did not qualify its counts:\n%s", mapOut)
		}
	})

	// The retrieval note is the most damaging surface: it must not tell the
	// user their memory was never there.
	t.Run("retrieval-note", func(t *testing.T) {
		note := emptyResultBlindSpot(brainDir)
		if strings.Contains(note, "may genuinely not be in the brain") {
			t.Fatalf("the note still vouches for the empty after fact loss: %q", note)
		}
		if !strings.Contains(note, "NOT evidence of absence") {
			t.Fatalf("the note does not correct the empty result: %q", note)
		}
	})

	// get: "not found" for an id the caller is holding is the same lie.
	t.Run("get", func(t *testing.T) {
		// A miss exits non-zero (#243 made `get` agree with `show`), and the
		// payload is still emitted first -- so the qualification below has to
		// survive the failing exit, not depend on a clean one.
		getOut, err := execute(t, NewRootCommand(opts), "get", lost[0], "--branch", "feature")
		if err == nil {
			t.Fatalf("get exited clean for an id it could not produce:\n%s", getOut)
		}
		if !strings.Contains(getOut, "not found") {
			t.Fatalf("the fixture did not actually lose the fact:\n%s", getOut)
		}
		if !strings.Contains(getOut, "NOT evidence of absence") {
			t.Fatalf("get reported \"not found\" for a lost fact with no qualification:\n%s", getOut)
		}
	})
}

// TestFactStoreIntegrityDetectsTotalFactLoss covers the all-five case, where
// status said 5 and verify said 0 and neither noticed the other.
func TestFactStoreIntegrityDetectsTotalFactLoss(t *testing.T) {
	opts, repoDir, brainDir, path := factLossFixture(t)

	replaceFactLines(t, path, `{"hello":"world"}`, 0, 1, 2, 3, 4)

	integrity := inspectBrainFactStore(brainDir)
	if integrity.Mode != factStoreDefectLossy {
		t.Fatalf("mode = %q, want %q", integrity.Mode, factStoreDefectLossy)
	}
	if integrity.Declared != 5 || integrity.Readable != 0 || integrity.Lost() != 5 {
		t.Fatalf("declared/readable/lost = %d/%d/%d, want 5/0/5", integrity.Declared, integrity.Readable, integrity.Lost())
	}

	note := emptyResultBlindSpot(brainDir)
	if strings.Contains(note, "may genuinely not be in the brain") {
		t.Fatalf("the brain lost 100%% of its facts and still told the user they were never there: %q", note)
	}
	if !strings.Contains(note, "5 declared, 0 readable") {
		t.Fatalf("the note does not name the total loss: %q", note)
	}

	checks, _ := brainDoctorReadOnlyReport(context.Background(), opts, repoDir)
	if state, _ := factsDoctorCheckState(checks); state != "error" {
		t.Fatalf("doctor facts = %q, want error", state)
	}

	// search is where the note reaches a user. Force an empty result set.
	searchOut, err := execute(t, NewRootCommand(opts), "search", "zzqqxxwwvv")
	if err != nil {
		t.Fatalf("search: %v\n%s", err, searchOut)
	}
	if strings.Contains(searchOut, "may genuinely not be in the brain") {
		t.Fatalf("search still explains the empty away:\n%s", searchOut)
	}
	if !strings.Contains(searchOut, "NOT evidence of absence") {
		t.Fatalf("search did not warn that the store is lossy:\n%s", searchOut)
	}

	verifyOut, verifyErr := execute(t, NewRootCommand(opts), "verify", "--branch", "feature")
	if verifyErr == nil {
		t.Fatalf("verify reported success over a store with nothing left in it:\n%s", verifyOut)
	}
	if strings.Contains(verifyOut, "0 orphaned") && !strings.Contains(verifyOut, "store integrity") {
		t.Fatalf("verify answered \"0 orphaned\" about five lost facts:\n%s", verifyOut)
	}
}

// TestFactStoreIntegrityDetectsFactLineWithoutID covers a line that is a
// perfectly well-formed fact in every respect except identity. It parses, it
// has text and paths and a status, and it is still unreachable: `get fact:…`
// cannot address it, dedup cannot key it, supersede cannot find it.
func TestFactStoreIntegrityDetectsFactLineWithoutID(t *testing.T) {
	_, _, brainDir, path := factLossFixture(t)

	replaceFactLines(t, path,
		`{"id":"","paths":["project.tooling.stack"],"text":"an unaddressable fact","branch":"feature","origin":"authored","status":"active","provenance":[{"session_id":"session-1"}]}`,
		2)

	integrity := inspectBrainFactStore(brainDir)
	if integrity.OK() {
		t.Fatalf("a fact line with no id passed the integrity check: %+v", integrity)
	}
	if integrity.Readable != 4 || integrity.Unusable != 1 {
		t.Fatalf("readable/unusable = %d/%d, want 4/1", integrity.Readable, integrity.Unusable)
	}
	if !strings.Contains(integrity.Warning(), "carry no fact id") {
		t.Fatalf("warning does not name the missing id: %q", integrity.Warning())
	}

	// And the manifest producer must agree, so a later `remember` or `refresh`
	// cannot re-declare the damaged store as whole.
	byBranch, err := loadAllFactBranches(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := summarizeFactSource(time.Now().UTC(), byBranch, 0, 0, 0, nil).Facts; got != 4 {
		t.Fatalf("summarizeFactSource counted %d facts, want 4: an id-less line must never be declared as a fact", got)
	}
}

// TestFactStoreIntegrityKeepsFailureModesDistinct pins requirement 5: "declared
// 5, found 4" is a different user situation from "cannot parse line 3" and from
// "the file is gone", and each keeps its own words.
func TestFactStoreIntegrityKeepsFailureModesDistinct(t *testing.T) {
	t.Run("lossy", func(t *testing.T) {
		_, _, brainDir, path := factLossFixture(t)
		replaceFactLines(t, path, `{"hello":"world"}`, 0)
		got := inspectBrainFactStore(brainDir)
		if got.Mode != factStoreDefectLossy {
			t.Fatalf("mode = %q, want lossy", got.Mode)
		}
		if !strings.Contains(got.Warning(), "does not match the manifest") {
			t.Fatalf("lossy warning: %q", got.Warning())
		}
	})

	t.Run("corrupt", func(t *testing.T) {
		_, _, brainDir, path := factLossFixture(t)
		replaceFactLines(t, path, `{not json at all`, 2)
		got := inspectBrainFactStore(brainDir)
		if got.Mode != factStoreDefectUnreadable {
			t.Fatalf("mode = %q, want unreadable", got.Mode)
		}
		if !strings.Contains(got.Warning(), "corrupt") || !strings.Contains(got.Warning(), "line 3") {
			t.Fatalf("corrupt warning does not name the line: %q", got.Warning())
		}
		if strings.Contains(got.Warning(), "readable") {
			t.Fatalf("a store that would not parse must not report a readable count: %q", got.Warning())
		}
	})

	t.Run("missing", func(t *testing.T) {
		_, _, brainDir, path := factLossFixture(t)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		got := inspectBrainFactStore(brainDir)
		if got.Mode != factStoreDefectMissing {
			t.Fatalf("mode = %q, want missing", got.Mode)
		}
		if !strings.Contains(got.Warning(), factsFileName) {
			t.Fatalf("missing warning does not name the file: %q", got.Warning())
		}
	})

	// Every mode names a repair.
	for _, mode := range []factStoreDefectMode{factStoreDefectLossy, factStoreDefectUnreadable, factStoreDefectMissing, factStoreDefectStale} {
		got := factStoreIntegrity{Mode: mode, Declared: 5, Readable: 4, Missing: []string{"feature"}}
		if !strings.Contains(got.Warning(), "entire brain refresh") {
			t.Fatalf("mode %q does not name the repair command: %q", mode, got.Warning())
		}
	}
}

// TestFactStoreIntegrityHealthyBrainDoesNotRegress is the negative control for
// a store that is intact: no warning, no changed wording, no nonzero exit.
//
// The surface assertions come first and deliberately hold BOTH before and
// after the fix — that is what makes them a control. Only the closing
// assertion, that the checker actually measured the store, is new behaviour.
func TestFactStoreIntegrityHealthyBrainDoesNotRegress(t *testing.T) {
	opts, repoDir, brainDir, _ := factLossFixture(t)

	integrity := inspectBrainFactStore(brainDir)
	if !integrity.OK() {
		t.Fatalf("an intact five-fact store failed the check: %+v", integrity)
	}
	if integrity.Warning() != "" {
		t.Fatalf("intact store produced a warning: %q", integrity.Warning())
	}

	report, err := buildAvailableBrainStatusReport(context.Background(), opts, repoDir)
	if err != nil {
		t.Fatalf("status report: %v", err)
	}
	if report.Facts == nil || report.Facts.Integrity != nil {
		t.Fatalf("intact store carries an integrity defect: %+v", report.Facts)
	}
	statusOut, err := execute(t, NewRootCommand(opts), "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, statusOut)
	}
	if !strings.Contains(statusOut, "5 facts") || strings.Contains(statusOut, "declared") {
		t.Fatalf("intact store changed the status identity line:\n%s", statusOut)
	}

	checks, _ := brainDoctorReadOnlyReport(context.Background(), opts, repoDir)
	if state, _ := factsDoctorCheckState(checks); state != "ok" {
		t.Fatalf("doctor facts = %q on an intact store, want ok", state)
	}

	// The fixture's anchors cite a session this brain does not retain, so
	// verify legitimately reports orphaned facts here. What must NOT appear is
	// a store-integrity verdict: the store itself is whole.
	verifyOut, verifyErr := execute(t, NewRootCommand(opts), "verify", "--branch", "feature")
	if errors.Is(verifyErr, errVerifyStoreIncomplete) {
		t.Fatalf("verify reported a store defect on an intact store:\n%s", verifyOut)
	}
	if strings.Contains(verifyOut, "store integrity") {
		t.Fatalf("verify printed a store-integrity line for an intact store:\n%s", verifyOut)
	}
	mapOut, err := execute(t, NewRootCommand(opts), "facts", "map", "--branch", "feature")
	if err != nil {
		t.Fatalf("facts map failed on an intact store: %v\n%s", err, mapOut)
	}
	if strings.Contains(mapOut, "warning:") {
		t.Fatalf("facts map warned about an intact store:\n%s", mapOut)
	}

	// Everything above is silence, and silence is also what a check that never
	// ran produces. This is the assertion that separates "intact" from
	// "unmeasured": the cross-check must have counted all five facts.
	if integrity.Declared != 5 || integrity.Readable != 5 {
		t.Fatalf("declared/readable = %d/%d, want 5/5", integrity.Declared, integrity.Readable)
	}
}

// TestFactStoreIntegrityEmptyBrainReportsNoLoss is the other negative control,
// and the one a naive count check gets wrong: a brain that never held a fact
// has lost nothing, and must keep saying so in its own words.
func TestFactStoreIntegrityEmptyBrainReportsNoLoss(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	brainDir := writeDistillFixture(t, now)

	if got := inspectBrainFactStore(brainDir); !got.OK() {
		t.Fatalf("a brain with no fact source reported loss: %+v", got)
	}

	// A declared-but-genuinely-empty fact source is the sharper case: zero
	// declared and zero readable agree, and nothing is wrong.
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Sources.Facts = &factSourceManifest{GeneratedAt: now}
	if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
		t.Fatal(err)
	}
	got := inspectBrainFactStore(brainDir)
	if !got.OK() {
		t.Fatalf("an empty-but-declared fact source reported loss: %+v", got)
	}
	if got.Warning() != "" {
		t.Fatalf("empty brain produced a warning: %q", got.Warning())
	}
	if note := emptyResultBlindSpot(brainDir); !strings.Contains(note, "may genuinely not be in the brain") {
		t.Fatalf("an empty brain must still vouch for its empty result, got %q", note)
	}
}
