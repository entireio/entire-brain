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
	for _, mode := range []factStoreDefectMode{factStoreDefectLossy, factStoreDefectUnreadable, factStoreDefectMissing, factStoreDefectStale, factStoreDefectDuplicated} {
		got := factStoreIntegrity{Mode: mode, Declared: 5, Readable: 4, Distinct: 3, Missing: []string{"feature"}, DuplicateIDs: []string{"fact:aaaa"}}
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

// --- identity, not arithmetic ---------------------------------------------
//
// Everything above this line corrupts the store in a way that changes how MANY
// facts it yields. The cases below change WHICH facts it yields and leave the
// count alone, which is the shape a count comparison cannot see at all.

// overwriteFactLineWithLine replaces the record on line dst with a byte copy of
// the record on line src, the way a botched hand edit, a bad merge resolution
// or a partially-rewritten block does. The store stays valid NDJSON, every line
// still parses, and the line count never moves — only the fact that used to
// live on dst is gone. It returns the id that was destroyed and the id that now
// stands in its place.
func overwriteFactLineWithLine(t *testing.T, path string, src, dst int) (destroyed, standin string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if src < 0 || src >= len(lines) || dst < 0 || dst >= len(lines) || src == dst {
		t.Fatalf("src=%d dst=%d out of range for a %d-line store", src, dst, len(lines))
	}
	idOf := func(n int) string {
		var record factRecord
		if err := json.Unmarshal([]byte(lines[n]), &record); err != nil {
			t.Fatalf("line %d is not a fact to begin with: %v", n, err)
		}
		return record.ID
	}
	destroyed, standin = idOf(dst), idOf(src)
	lines[dst] = lines[src]
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return destroyed, standin
}

// TestFactStoreIntegrityDetectsCountPreservingOverwrite is the case the count
// comparison misses completely: five lines in, five lines out, five declared,
// five readable — and one fact replaced by a duplicate of another. `get` says
// "not found" for an id the caller is holding, so the loss is real; every
// health surface used to say the store was fine.
func TestFactStoreIntegrityDetectsCountPreservingOverwrite(t *testing.T) {
	opts, repoDir, brainDir, path := factLossFixture(t)

	destroyed, standin := overwriteFactLineWithLine(t, path, 0, 1)

	t.Run("cross-check", func(t *testing.T) {
		integrity := inspectBrainFactStore(brainDir)
		if integrity.OK() {
			t.Fatalf("integrity reports a healthy store after a fact was overwritten by its neighbour: %+v", integrity)
		}
		if integrity.Mode != factStoreDefectDuplicated {
			t.Fatalf("mode = %q, want %q", integrity.Mode, factStoreDefectDuplicated)
		}
		// The counts AGREE. That is the whole point: this defect is invisible
		// to them, and the check must not depend on them disagreeing.
		if integrity.Declared != 5 || integrity.Readable != 5 || integrity.Lost() != 0 {
			t.Fatalf("declared/readable/lost = %d/%d/%d, want 5/5/0: the corruption preserves the count",
				integrity.Declared, integrity.Readable, integrity.Lost())
		}
		if integrity.Distinct != 4 || integrity.Overwritten() != 1 {
			t.Fatalf("distinct/overwritten = %d/%d, want 4/1", integrity.Distinct, integrity.Overwritten())
		}
		if len(integrity.DuplicateIDs) != 1 || integrity.DuplicateIDs[0] != standin {
			t.Fatalf("duplicate ids = %v, want [%s]", integrity.DuplicateIDs, standin)
		}
		if !strings.Contains(integrity.Warning(), standin) {
			t.Fatalf("warning does not name the colliding id: %q", integrity.Warning())
		}
		if !strings.Contains(integrity.Warning(), "entire brain refresh") {
			t.Fatalf("warning does not name the repair command: %q", integrity.Warning())
		}
		// The stale wording is the one thing this must never say: `refresh`
		// alone would re-declare the collided store and silence the brain.
		if strings.Contains(integrity.Warning(), "the manifest is behind the store") {
			t.Fatalf("a substitution was reported as a stale manifest: %q", integrity.Warning())
		}
	})

	// The fact really is gone — without this the rest is a false alarm.
	t.Run("get", func(t *testing.T) {
		getOut, err := execute(t, NewRootCommand(opts), "get", destroyed, "--branch", "feature")
		if err == nil {
			t.Fatalf("get exited clean for an id it could not produce:\n%s", getOut)
		}
		if !strings.Contains(getOut, "not found") {
			t.Fatalf("the fixture did not actually lose the fact:\n%s", getOut)
		}
		if !strings.Contains(getOut, "NOT evidence of absence") {
			t.Fatalf("get reported \"not found\" for an overwritten fact with no qualification:\n%s", getOut)
		}
	})

	t.Run("status", func(t *testing.T) {
		report, err := buildAvailableBrainStatusReport(context.Background(), opts, repoDir)
		if err != nil {
			t.Fatalf("status report: %v", err)
		}
		if report.Facts == nil || report.Facts.Integrity == nil {
			t.Fatalf("status facts carries no integrity verdict: %+v", report.Facts)
		}
		if !warningsMention(report.Warnings, "not unique") {
			t.Fatalf("status warnings do not name the collision: %v", report.Warnings)
		}
	})

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

	t.Run("verify", func(t *testing.T) {
		verifyOut, verifyErr := execute(t, NewRootCommand(opts), "verify", "--branch", "feature")
		if verifyErr == nil {
			t.Fatalf("verify passed over a store that overwrote a fact:\n%s", verifyOut)
		}
		if !strings.Contains(verifyOut, "store integrity") {
			t.Fatalf("verify did not report the store defect:\n%s", verifyOut)
		}
	})

	t.Run("facts-map", func(t *testing.T) {
		mapOut, mapErr := execute(t, NewRootCommand(opts), "facts", "map", "--branch", "feature")
		if mapErr == nil {
			t.Fatalf("facts map exited clean over a collided store:\n%s", mapOut)
		}
		if !strings.Contains(mapOut, "not unique") {
			t.Fatalf("facts map did not qualify its counts:\n%s", mapOut)
		}
	})

	t.Run("facts-tree", func(t *testing.T) {
		treeOut, treeErr := execute(t, NewRootCommand(opts), "facts", "tree", "--branch", "feature")
		if treeErr == nil {
			t.Fatalf("facts tree exited clean over a collided store:\n%s", treeOut)
		}
		if !strings.Contains(treeOut, "not unique") {
			t.Fatalf("facts tree did not qualify its hierarchy:\n%s", treeOut)
		}
	})

	t.Run("facts-status", func(t *testing.T) {
		statusOut, err := execute(t, NewRootCommand(opts), "facts", "status", "--branch", "feature")
		if err != nil && !strings.Contains(statusOut, "not unique") {
			t.Fatalf("facts status: %v\n%s", err, statusOut)
		}
		if !strings.Contains(statusOut, "not unique") {
			t.Fatalf("facts status reported totals without naming the collision:\n%s", statusOut)
		}
	})

	// The retrieval note is the surface #242 exists for. An empty result over
	// a store with a hole in it must not be explained away.
	t.Run("retrieval-note", func(t *testing.T) {
		note := emptyResultBlindSpot(brainDir)
		if strings.Contains(note, "may genuinely not be in the brain") {
			t.Fatalf("the note still vouches for the empty over a collided store: %q", note)
		}
		if !strings.Contains(note, "NOT evidence of absence") {
			t.Fatalf("the note does not correct the empty result: %q", note)
		}
	})

	t.Run("search", func(t *testing.T) {
		searchOut, err := execute(t, NewRootCommand(opts), "search", "zzqqxxwwvv")
		if err != nil {
			t.Fatalf("search: %v\n%s", err, searchOut)
		}
		if strings.Contains(searchOut, "may genuinely not be in the brain") {
			t.Fatalf("search still explains the empty away:\n%s", searchOut)
		}
		if !strings.Contains(searchOut, "NOT evidence of absence") {
			t.Fatalf("search did not warn that the store overwrote a fact:\n%s", searchOut)
		}
	})

	t.Run("recall", func(t *testing.T) {
		recallOut, err := execute(t, NewRootCommand(opts), "recall", "alpha", "--branch", "feature")
		if err != nil && !strings.Contains(recallOut, "not unique") {
			t.Fatalf("recall: %v\n%s", err, recallOut)
		}
		if !strings.Contains(recallOut, "not unique") {
			t.Fatalf("recall served facts from a collided store without a warning:\n%s", recallOut)
		}
	})

	t.Run("query", func(t *testing.T) {
		queryOut, err := execute(t, NewRootCommand(opts), "query", "zzqqxxwwvv")
		if err != nil {
			t.Fatalf("query: %v\n%s", err, queryOut)
		}
		if strings.Contains(queryOut, "may genuinely not be in the brain") {
			t.Fatalf("query still explains the empty away:\n%s", queryOut)
		}
	})
}

// TestFactStoreIntegrityAppendedDuplicateIsNotAStaleManifest pins the OTHER
// duplicate shape and why it needed its own mode rather than the one it used to
// land in.
//
// Appending a copy of an existing line also makes the store longer than the
// manifest, so the count comparison already caught it — as "stale", whose
// advice is "the manifest is behind the store — run `entire brain refresh`".
// That advice is actively harmful here: refresh re-derives the count from the
// collided store, declares 6, and every surface goes quiet about a fact set
// that holds one id twice. Nothing was ever noticing the DUPLICATE; it was
// noticing a number that went up.
func TestFactStoreIntegrityAppendedDuplicateIsNotAStaleManifest(t *testing.T) {
	_, _, brainDir, path := factLossFixture(t)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if err := os.WriteFile(path, []byte(strings.Join(append(lines, lines[0]), "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := inspectBrainFactStore(brainDir)
	if got.Mode != factStoreDefectDuplicated {
		t.Fatalf("mode = %q, want %q: a repeated id is a collision, not a manifest that fell behind", got.Mode, factStoreDefectDuplicated)
	}
	if got.Readable != 6 || got.Distinct != 5 || got.Overwritten() != 1 {
		t.Fatalf("readable/distinct/overwritten = %d/%d/%d, want 6/5/1", got.Readable, got.Distinct, got.Overwritten())
	}
	if strings.Contains(got.Warning(), "the manifest is behind the store") {
		t.Fatalf("a colliding store was still told to just refresh: %q", got.Warning())
	}
	if !strings.Contains(got.Warning(), factsRepairHint) {
		t.Fatalf("warning does not name the repair: %q", got.Warning())
	}
}

// TestFactStoreIntegrityStaleStaysStale is the control for the reclassification
// above: a store that genuinely holds MORE distinct facts than the manifest
// declares is still a manifest that fell behind, and must keep its own words
// and its own repair (refresh, no re-distill).
func TestFactStoreIntegrityStaleStaysStale(t *testing.T) {
	_, _, brainDir, path := factLossFixture(t)

	extra := factRecord{
		ID:         factRecordID("a sixth fact nobody told the manifest about", []string{"project.tooling.stack"}),
		Paths:      []string{"project.tooling.stack"},
		Text:       "a sixth fact nobody told the manifest about",
		Branch:     "feature",
		Origin:     factOriginAuthored,
		Status:     factStatusActive,
		Provenance: []factAnchor{{SessionID: "session-1"}},
	}
	encoded, err := json.Marshal(extra)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(append(encoded, '\n')); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	got := inspectBrainFactStore(brainDir)
	if got.Mode != factStoreDefectStale {
		t.Fatalf("mode = %q, want stale", got.Mode)
	}
	if got.Distinct != 6 || got.Overwritten() != 0 {
		t.Fatalf("distinct/overwritten = %d/%d, want 6/0", got.Distinct, got.Overwritten())
	}
	if !strings.Contains(got.Warning(), "the manifest is behind the store") {
		t.Fatalf("stale lost its own words: %q", got.Warning())
	}
}

// TestFactStoreIntegrityShortAndCollidedReportsBoth covers a store that is both
// short AND collided. "lossy" stays the mode — a shortfall is the more urgent
// framing and keeps its wording — but the line must not stop at the arithmetic:
// the overwritten fact is gone too, and it is not one of the ones the count
// accounted for.
func TestFactStoreIntegrityShortAndCollidedReportsBoth(t *testing.T) {
	_, _, brainDir, path := factLossFixture(t)

	overwriteFactLineWithLine(t, path, 0, 1)
	replaceFactLines(t, path, `{"hello":"world"}`, 4)

	got := inspectBrainFactStore(brainDir)
	if got.Mode != factStoreDefectLossy {
		t.Fatalf("mode = %q, want lossy: a short store is still short", got.Mode)
	}
	if !strings.Contains(got.Warning(), "5 declared, 4 readable") {
		t.Fatalf("lossy lost its own words: %q", got.Warning())
	}
	if !strings.Contains(got.Warning(), "repeat a fact id") {
		t.Fatalf("a store that is short AND collided reported only the shortfall: %q", got.Warning())
	}
}

// TestFactStoreIntegrityAllowsOneIDOnTwoBranches is the false-positive control
// the id comparison exists under, and the reason it is scoped to a branch.
//
// A fact id is sha256(normalized text + sorted paths) — the branch is not in it
// — so the same fact on two branches is the same id by design, and `facts
// promote` produces exactly that on purpose. A brain-wide id set would call
// every promote data loss.
func TestFactStoreIntegrityAllowsOneIDOnTwoBranches(t *testing.T) {
	_, _, brainDir, _ := factLossFixture(t)
	now := time.Date(2026, 8, 10, 3, 0, 0, 0, time.UTC)

	feature, err := loadFacts(brainDir, "feature")
	if err != nil {
		t.Fatal(err)
	}
	carried := make([]factRecord, 0, len(feature))
	for _, fact := range feature {
		fact.Branch = "main"
		carried = append(carried, fact)
	}
	if err := writeFacts(brainDir, "main", carried); err != nil {
		t.Fatal(err)
	}
	if err := updateFactSourceManifest(brainDir, now); err != nil {
		t.Fatal(err)
	}

	got := inspectBrainFactStore(brainDir)
	if !got.OK() {
		t.Fatalf("carrying a branch's facts into another branch was reported as corruption: %+v\n%s", got, got.Warning())
	}
	if got.Readable != 10 || got.Distinct != 10 {
		t.Fatalf("readable/distinct = %d/%d, want 10/10: ids repeat ACROSS branches, never within one", got.Readable, got.Distinct)
	}
}

// TestFactStoreIntegrityHealthyStoreHasNoCollisions extends the healthy-brain
// control to identity: silence is also what an unrun check produces, so assert
// the checker actually resolved five distinct ids.
func TestFactStoreIntegrityHealthyStoreHasNoCollisions(t *testing.T) {
	_, _, brainDir, _ := factLossFixture(t)

	got := inspectBrainFactStore(brainDir)
	if !got.OK() {
		t.Fatalf("an intact five-fact store failed the identity check: %+v", got)
	}
	if got.Distinct != 5 || got.Overwritten() != 0 || len(got.DuplicateIDs) != 0 {
		t.Fatalf("distinct/overwritten/duplicates = %d/%d/%v, want 5/0/[]", got.Distinct, got.Overwritten(), got.DuplicateIDs)
	}
}
