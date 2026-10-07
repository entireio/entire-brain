package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// `get fact:<id>` is usually the exact moment someone meets branch scoping, and
// it printed a bare "not found" -- making the same claim the empty-result note
// makes, that the item was never there, when the fact may be sitting on another
// branch. That is a different answer, and the only one with something the
// caller can act on.
//
// runGet checked ONLY the integrity path on a miss, so the branch choke point
// was unreachable from this surface.
func TestGetOnAMissNamesTheBranchHoldingFacts(t *testing.T) {
	f := newVerifyFixture(t)
	now := f.now
	// declareFacts edits an existing manifest, so the brain needs one.
	writeDistillFixtureAt(t, f.brainDir, now)

	// The fact lives on a feature branch; the query runs from the fixture's
	// branch and must miss.
	seedFactBranch(t, f.brainDir, "feature/deploy", "aaaa1111")
	declareFacts(t, f.brainDir, now, 1, "feature/deploy")

	var out bytes.Buffer
	cmd := NewRootCommand(f.opts)
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"get", "fact:doesnotexist"})
	_ = cmd.Execute() // a miss exits 1 by design; the OUTPUT is the subject

	got := out.String()
	if !strings.Contains(got, "not found") {
		t.Fatalf("a miss must still say so:\n%s", got)
	}
	if !strings.Contains(got, "feature/deploy") {
		t.Errorf("the branch holding facts must be named on a fact miss, so the caller has something to retype:\n%s", got)
	}
}

// Gate 1: this is a UNIFIED surface. A conversation id missing has nothing to
// do with which branch holds facts, and a note about facts there is noise
// attached to an unrelated answer.
func TestGetDoesNotTalkAboutFactBranchesForANonFactID(t *testing.T) {
	f := newVerifyFixture(t)
	writeDistillFixtureAt(t, f.brainDir, f.now)
	seedFactBranch(t, f.brainDir, "feature/deploy", "aaaa1111")
	declareFacts(t, f.brainDir, f.now, 1, "feature/deploy")

	var out bytes.Buffer
	cmd := NewRootCommand(f.opts)
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"get", "conversation:nope"})
	_ = cmd.Execute()

	if got := out.String(); strings.Contains(got, "feature/deploy") {
		t.Errorf("a conversation miss must not carry a note about fact branches:\n%s", got)
	}
}

// Gate 2: the id-shape test itself. A bare id with no prefix resolves as a fact
// everywhere else in this codebase, so it counts here too.
func TestMissingFactIDShapes(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		ids  []string
		want bool
	}{
		"prefixed fact":      {[]string{"fact:abc"}, true},
		"bare id":            {[]string{"abc123"}, true},
		"conversation":       {[]string{"conversation:abc"}, false},
		"history":            {[]string{"history:abc"}, false},
		"raw has a colon":    {[]string{"raw:abc"}, false},
		"mixed, one fact":    {[]string{"conversation:a", "fact:b"}, true},
		"mixed, no fact":     {[]string{"conversation:a", "doc:b"}, false},
		"empty slice":        {nil, false},
		"blank entries only": {[]string{"", "   "}, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := missingFactIDs(tc.ids); got != tc.want {
				t.Errorf("missingFactIDs(%v) = %v, want %v", tc.ids, got, tc.want)
			}
		})
	}
}

// Gate 3: integrity still wins. A store that lost facts must never be reported
// as merely the wrong branch -- the branch note would send the caller hunting
// for a fact that is actually gone.
func TestIntegrityWarningStillBeatsTheBranchNoteOnAMiss(t *testing.T) {
	f := newVerifyFixture(t)
	writeDistillFixtureAt(t, f.brainDir, f.now)

	// Declare more facts than exist anywhere: the store cannot produce what the
	// manifest claims.
	seedFactBranch(t, f.brainDir, "feature/deploy", "aaaa1111")
	declareFactsAt(t, f.brainDir, f.now, f.now, 99, "feature/deploy")

	var out bytes.Buffer
	cmd := NewRootCommand(f.opts)
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"get", "fact:doesnotexist"})
	_ = cmd.Execute()

	got := out.String()
	if !strings.Contains(got, "NOT evidence of absence") {
		t.Fatalf("a store that lost facts must say so on a miss:\n%s", got)
	}
	if strings.Contains(got, "feature/deploy") {
		t.Errorf("a lost-facts store must not be reported as merely the wrong branch:\n%s", got)
	}
	_ = time.Now
}

// THE JSON SURFACE. My first version of this fix put the note only in the text
// block, and the e2e test above used text output -- so it passed while
// `get --json` carried nothing. mcp.go:1323,1333 call runGet with
// jsonOut=true for brain_get and brain_multi_get, which makes the AGENT
// surface, the consumer most likely to act on a "--branch" hint, the one that
// could not see it.
//
// The integrity note had the same gap before any of this and inherits the fix,
// so both are asserted here.
func TestGetJSONCarriesTheMissNote(t *testing.T) {
	f := newVerifyFixture(t)
	writeDistillFixtureAt(t, f.brainDir, f.now)
	seedFactBranch(t, f.brainDir, "feature/deploy", "aaaa1111")
	declareFacts(t, f.brainDir, f.now, 1, "feature/deploy")

	var out bytes.Buffer
	cmd := NewRootCommand(f.opts)
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"get", "fact:doesnotexist", "--json"})
	_ = cmd.Execute()

	var got struct {
		Missing   []string `json:"missing"`
		BlindSpot string   `json:"blind_spot"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	if len(got.Missing) != 1 {
		t.Fatalf("the fixture must produce a miss for this guard to mean anything, got %v", got.Missing)
	}
	if !strings.Contains(got.BlindSpot, "feature/deploy") {
		t.Errorf("get --json must carry the branch note; it is the surface MCP uses. got %q", got.BlindSpot)
	}
}

// Integrity still wins on the JSON surface too, and a healthy store with no
// fact-shaped miss stays quiet rather than emitting an empty field.
func TestGetJSONMissNoteFollowsTheSameGates(t *testing.T) {
	f := newVerifyFixture(t)
	writeDistillFixtureAt(t, f.brainDir, f.now)
	seedFactBranch(t, f.brainDir, "feature/deploy", "aaaa1111")
	declareFactsAt(t, f.brainDir, f.now, f.now, 99, "feature/deploy") // store lost facts

	var out bytes.Buffer
	cmd := NewRootCommand(f.opts)
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"get", "fact:doesnotexist", "--json"})
	_ = cmd.Execute()

	var got struct {
		BlindSpot string `json:"blind_spot"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	if !strings.Contains(got.BlindSpot, "NOT evidence of absence") {
		t.Errorf("a store that lost facts must say so on the JSON surface, got %q", got.BlindSpot)
	}
	if strings.Contains(got.BlindSpot, "feature/deploy") {
		t.Errorf("a lost-facts store must not be reported as merely the wrong branch, got %q", got.BlindSpot)
	}
}

// loadAllFactBranches walked every branch directory and parsed its facts file
// with NO containment guards, while the single-branch read applies two
// (facts.go:146,150). So a symlinked facts.ndjson inside a real branch
// directory was read from wherever it pointed.
//
// Measured before the fix: loadFacts refused it with "path component must not
// be a symlink" while loadAllFactBranches returned the outside content. The
// two surfaces disagreed about the same file.
//
// A symlinked branch DIRECTORY was never the vector -- os.ReadDir reports
// types without following links, so IsDir() is false and the walk already
// skips it. Asserted below so the distinction does not get lost.
func TestFactInventoryRefusesASymlinkedFactsFile(t *testing.T) {
	t.Parallel()

	brainDir := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "elsewhere.ndjson")
	if err := os.WriteFile(secret,
		[]byte(`{"id":"fact:leak","branch":"evil","status":"active","text":"CONTENT FROM OUTSIDE"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rel := factsBranchRelDir("main")
	dir := filepath.Join(brainDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, factsFileName)
	if err := os.Symlink(secret, target); err != nil {
		t.Skipf("this filesystem cannot create symlinks: %v", err)
	}
	// Non-vacuity: the fixture must BE a symlink at the path the walk reads,
	// or a pass proves nothing.
	if info, err := os.Lstat(target); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("fixture is not a symlink at %s (err=%v)", rel, err)
	}

	byBranch, err := loadAllFactBranches(brainDir)
	if err == nil {
		t.Fatalf("a symlinked fact store must be refused, got %d branch(es) back", len(byBranch))
	}
	for branch, records := range byBranch {
		for _, record := range records {
			t.Errorf("content from outside the brain was returned: branch=%s text=%q", branch, record.Text)
		}
	}
	// And it must agree with the single-branch read, which already refused it.
	if _, single := loadFacts(brainDir, "main"); single == nil {
		t.Error("loadFacts should refuse this too; if it stopped, the guards have diverged again")
	}
}

// A symlinked branch DIRECTORY is skipped by the walk, not followed: os.ReadDir
// reports entry types without dereferencing. This pins that, because the
// finding that prompted the fix named this as the vector and it is not one.
func TestFactInventorySkipsASymlinkedBranchDirectory(t *testing.T) {
	t.Parallel()

	brainDir := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, factsFileName),
		[]byte(`{"id":"fact:leak","branch":"evil","status":"active","text":"CONTENT FROM OUTSIDE"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(brainDir, factsDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(brainDir, factsDirName, "escape-00000000")); err != nil {
		t.Skipf("this filesystem cannot create symlinks: %v", err)
	}

	byBranch, err := loadAllFactBranches(brainDir)
	if err != nil {
		t.Fatalf("a skipped symlink is not an error: %v", err)
	}
	if len(byBranch) != 0 {
		t.Errorf("a symlinked branch directory must be skipped, got %v", byBranch)
	}
}

// The two guards above are individually REDUNDANT for a symlinked facts file
// -- measured: removing either leaves the test green, removing both turns it
// red. So that test alone cannot justify keeping both. These two can: each
// guard is the only one that catches its own vector.
//
// COMPONENT GUARD ONLY. If the facts DIRECTORY is itself a symlink, os.ReadDir
// follows it (ReadDir dereferences the path it is given, unlike the entry types
// inside), so the whole walk reads from outside the brain while every inner
// file is a perfectly regular file the Lstat check waves through.
func TestFactInventoryRefusesASymlinkedFactsDirectory(t *testing.T) {
	t.Parallel()

	brainDir := t.TempDir()
	outside := t.TempDir()
	branchDir := filepath.Join(outside, "main-0d6e4079")
	if err := os.MkdirAll(branchDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(branchDir, factsFileName),
		[]byte(`{"id":"fact:leak","branch":"evil","status":"active","text":"CONTENT FROM OUTSIDE"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The facts directory itself is the symlink.
	if err := os.Symlink(outside, filepath.Join(brainDir, factsDirName)); err != nil {
		t.Skipf("this filesystem cannot create symlinks: %v", err)
	}
	// Non-vacuity: prove ReadDir really does reach through it, or this test is
	// asserting against a vector that does not exist.
	if entries, err := os.ReadDir(filepath.Join(brainDir, factsDirName)); err != nil || len(entries) == 0 {
		t.Fatalf("fixture does not reproduce the vector: ReadDir through the symlink gave %d entries (err=%v)", len(entries), err)
	}

	byBranch, err := loadAllFactBranches(brainDir)
	if err == nil {
		t.Fatalf("a symlinked facts directory must be refused, got %d branch(es)", len(byBranch))
	}
	for branch, records := range byBranch {
		for _, record := range records {
			t.Errorf("content from outside the brain was returned: branch=%s text=%q", branch, record.Text)
		}
	}
}

// LSTAT CHECK ONLY. A DIRECTORY at facts.ndjson is not a symlink, so the
// component guard passes it, and parseFactsFile would open it and fail with an
// opaque read error instead of naming the actual problem.
func TestFactInventoryRefusesANonRegularFactStore(t *testing.T) {
	t.Parallel()

	brainDir := t.TempDir()
	rel := factsBranchRelDir("main")
	dir := filepath.Join(brainDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The fact store path is a directory, not a file.
	store := filepath.Join(dir, factsFileName)
	if err := os.Mkdir(store, 0o755); err != nil {
		t.Fatal(err)
	}
	// Non-vacuity: it must be non-regular and NOT a symlink, or it is testing
	// the other guard.
	info, statErr := os.Lstat(store)
	if statErr != nil || info.Mode().IsRegular() {
		t.Fatalf("fixture is a regular file, so it tests nothing (err=%v)", statErr)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("fixture is a symlink, which the other guard already covers")
	}

	_, err := loadAllFactBranches(brainDir)
	if err == nil {
		t.Fatal("a non-regular fact store must be refused")
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("the error must name the actual problem, got %v", err)
	}
}
