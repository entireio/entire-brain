package entityindex

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/factgitmeta"
	"github.com/entireio/entire-brain/internal/factgitmeta/gitmeta"
)

// fakeRunner scripts `git` and `entire graph diff` responses by command key, so
// every builder behavior is exercised with no repository and no provider on
// PATH. It is the package's runner seam.
type fakeRunner struct {
	responses map[string]fakeResponse
	calls     []string
	missing   func(key string) (fakeResponse, bool)
}

type fakeResponse struct {
	stdout string
	err    error
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{responses: map[string]fakeResponse{}}
}

func (r *fakeRunner) Run(_ context.Context, _ string, name string, args ...string) ([]byte, []byte, error) {
	key := name + " " + strings.Join(args, " ")
	r.calls = append(r.calls, key)
	if resp, ok := r.responses[key]; ok {
		return []byte(resp.stdout), nil, resp.err
	}
	if r.missing != nil {
		if resp, ok := r.missing(key); ok {
			return []byte(resp.stdout), nil, resp.err
		}
	}
	return nil, nil, fmt.Errorf("fakeRunner: unscripted command %q", key)
}

func (r *fakeRunner) set(key, stdout string) { r.responses[key] = fakeResponse{stdout: stdout} }

func (r *fakeRunner) fail(key string, err error) { r.responses[key] = fakeResponse{err: err} }

func (r *fakeRunner) called(substr string) int {
	count := 0
	for _, call := range r.calls {
		if strings.Contains(call, substr) {
			count++
		}
	}
	return count
}

const (
	logFormat   = "--format=%H%x00%P%x00%cI%x00%B%x1e"
	testRepoDir = "/repo"
)

// scriptRepo wires branch resolution and a REAL first-parent walker over a
// linear history given oldest-first commits, so a pass can ask for whichever
// range its cursors imply instead of the one range a fixture pre-baked.
// Explicit responses still win, so a test can script a specific failure.
func scriptRepo(r *fakeRunner, branch string, commits []commitInfo) {
	r.set("git branch --show-current", branch+"\n")
	tip := commits[len(commits)-1].SHA
	r.set("git rev-parse --verify "+branch+"^{commit}", tip+"\n")
	r.missing = gitHistory(commits)
}

// gitHistory answers `git log --first-parent <fmt> --max-count=N [--skip=N]
// <spec>`, `git rev-list --count --first-parent <spec>` and `git merge-base
// --is-ancestor A B` over a linear, oldest-first history.
func gitHistory(commits []commitInfo) func(string) (fakeResponse, bool) {
	index := map[string]int{}
	for i, commit := range commits {
		index[commit.SHA] = i
	}
	// bounds resolves a revision spec to the inclusive [from, to] index range it
	// names, newest at `to`.
	bounds := func(spec string) (int, int, bool) {
		if from, to, isRange := strings.Cut(spec, ".."); isRange {
			lo, okLo := index[from]
			hi, okHi := index[to]
			if !okLo || !okHi {
				return 0, 0, false
			}
			return lo + 1, hi, true
		}
		hi, ok := index[spec]
		if !ok {
			return 0, 0, false
		}
		return 0, hi, true
	}
	render := func(from, to, max, skip int) string {
		var b strings.Builder
		for i, n := to-skip, 0; i >= from && n < max; i, n = i-1, n+1 {
			at := ""
			if !commits[i].CommittedAt.IsZero() {
				at = commits[i].CommittedAt.UTC().Format(time.RFC3339)
			}
			fmt.Fprintf(&b, "%s\x00%s\x00%s\x00%s\x1e\n", commits[i].SHA, commits[i].Parent, at, commits[i].Message)
		}
		return b.String()
	}
	return func(key string) (fakeResponse, bool) {
		fields := strings.Fields(key)
		if len(fields) == 5 && fields[0] == "git" && fields[1] == "merge-base" && fields[2] == "--is-ancestor" {
			ancestor, okA := index[fields[3]]
			descendant, okB := index[fields[4]]
			if !okA || !okB {
				return fakeResponse{err: fmt.Errorf("unknown revision")}, true
			}
			if ancestor <= descendant {
				return fakeResponse{}, true
			}
			return fakeResponse{err: fmt.Errorf("not an ancestor")}, true
		}
		if len(fields) == 5 && fields[0] == "git" && fields[1] == "rev-list" && fields[2] == "--count" && fields[3] == "--first-parent" {
			from, to, ok := bounds(fields[4])
			if !ok {
				return fakeResponse{err: fmt.Errorf("unknown revision %s", fields[4])}, true
			}
			return fakeResponse{stdout: strconv.Itoa(to-from+1) + "\n"}, true
		}
		if len(fields) < 6 || len(fields) > 7 || fields[0] != "git" || fields[1] != "log" || fields[2] != "--first-parent" {
			return fakeResponse{}, false
		}
		max, err := strconv.Atoi(strings.TrimPrefix(fields[4], "--max-count="))
		if err != nil {
			return fakeResponse{}, false
		}
		skip := 0
		if len(fields) == 7 {
			if skip, err = strconv.Atoi(strings.TrimPrefix(fields[5], "--skip=")); err != nil {
				return fakeResponse{}, false
			}
		}
		spec := fields[len(fields)-1]
		from, to, ok := bounds(spec)
		if !ok {
			return fakeResponse{err: fmt.Errorf("unknown revision %s", spec)}, true
		}
		return fakeResponse{stdout: render(from, to, max, skip)}, true
	}
}

func graphDiffKey(base, head string) string {
	return "entire graph diff --repo " + testRepoDir + " --base " + base + " --head " + head + " --json"
}

// windowFor reads a branch's stored indexed window straight out of git-meta.
func windowFor(t *testing.T, store *factgitmeta.MetaStore, branch string) (IndexWindow, bool) {
	t.Helper()
	raw, ok, err := store.ReadString(projectTarget, WindowKey(branch))
	if err != nil {
		t.Fatalf("read window: %v", err)
	}
	if !ok {
		return IndexWindow{}, false
	}
	return DecodeWindow(raw)
}

// graphOutput renders a provider response with the given file/entity changes.
func graphOutput(base, head string, files ...graphFileChange) string {
	data, err := json.Marshal(graphResult{Base: base, Head: head, Files: files})
	if err != nil {
		panic(err)
	}
	return string(data)
}

func newTestStore(t *testing.T) *factgitmeta.MetaStore {
	t.Helper()
	at := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	store, err := factgitmeta.OpenMetaStore(t.TempDir(), func() time.Time { at = at.Add(time.Second); return at })
	if err != nil {
		t.Fatalf("open meta store: %v", err)
	}
	return store
}

// commitAt stamps a deterministic, increasing committer date so reverse-index
// ordering is a property of the history rather than of the fixture.
func commitAt(n int) time.Time {
	return time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(n) * time.Hour)
}

func fixedNow() func() time.Time {
	at := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	return func() time.Time { at = at.Add(time.Minute); return at }
}

func loadSnapshot(t *testing.T, store *factgitmeta.MetaStore) *Snapshot {
	t.Helper()
	state, err := store.State()
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	return Load(state)
}

// --- build -----------------------------------------------------------------

func TestBuildWritesForwardReverseAndWindow(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner()
	commits := []commitInfo{
		{SHA: "aaaa111111111111111111111111111111111111", Parent: "", CommittedAt: commitAt(0), Message: "root\n"},
		{SHA: "bbbb222222222222222222222222222222222222", Parent: "aaaa111111111111111111111111111111111111", CommittedAt: commitAt(1), Message: "work\n\nEntire-Checkpoint: 0123456789ab\n"},
	}
	scriptRepo(runner, "main", commits)
	runner.set(graphDiffKey(EmptyTreeSHA, commits[0].SHA), graphOutput(EmptyTreeSHA, commits[0].SHA, graphFileChange{
		Path:    "internal/cli/export.go",
		Changes: []graphEntityChange{{Type: "added", Kind: "function", Name: "runExport", NewSignature: "func runExport()", AfterStartLine: 12}},
	}))
	runner.set(graphDiffKey(commits[0].SHA, commits[1].SHA), graphOutput(commits[0].SHA, commits[1].SHA, graphFileChange{
		Path:    "internal/cli/export.go",
		Changes: []graphEntityChange{{Type: "body_changed", Kind: "function", Name: "runExport", BeforeStartLine: 12, AfterStartLine: 14}},
	}))

	store := newTestStore(t)
	result, err := Build(context.Background(), runner, store, BuildOptions{RepoDir: testRepoDir, Limit: 100, Now: fixedNow()})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if result.Indexed != 2 || result.Entities != 2 {
		t.Fatalf("indexed=%d entities=%d, want 2/2 (%+v)", result.Indexed, result.Entities, result)
	}
	wantWindow := IndexWindow{Floor: commits[0].SHA, Tip: commits[1].SHA}
	if result.Window != wantWindow {
		t.Fatalf("window = %+v, want %+v", result.Window, wantWindow)
	}

	snap := loadSnapshot(t, store)
	key := EntityKey("internal/cli/export.go", "function", "runExport")
	got := snap.Commits(key)
	want := []string{commits[0].SHA, commits[1].SHA}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reverse index = %v, want %v", got, want)
	}
	if stored, ok := snap.Window("main"); !ok || stored != wantWindow {
		t.Fatalf("stored window = %+v (ok=%v), want %+v", stored, ok, wantWindow)
	}

	delta, ok := snap.Delta(commits[1].SHA)
	if !ok {
		t.Fatal("forward delta document missing for the second commit")
	}
	if delta.SchemaVersion != SchemaVersion || delta.Producer != Producer {
		t.Fatalf("delta header = %+v", delta)
	}
	if delta.Base != commits[0].SHA || delta.Head != commits[1].SHA {
		t.Fatalf("delta base/head = %s/%s", delta.Base, delta.Head)
	}
	if len(delta.Entities) != 1 || delta.Entities[0].Change != ChangeModified {
		t.Fatalf("delta entities = %+v", delta.Entities)
	}
	if _, err := time.Parse(time.RFC3339, delta.ComputedAt); err != nil {
		t.Fatalf("computed_at %q is not RFC3339: %v", delta.ComputedAt, err)
	}
}

func TestBuildIsIdempotentAndSkipsIndexedCommits(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner()
	commits := []commitInfo{
		{SHA: "aaaa111111111111111111111111111111111111", Parent: "", CommittedAt: commitAt(0), Message: "root\n"},
	}
	scriptRepo(runner, "main", commits)
	runner.set(graphDiffKey(EmptyTreeSHA, commits[0].SHA), graphOutput(EmptyTreeSHA, commits[0].SHA, graphFileChange{
		Path:    "a.go",
		Changes: []graphEntityChange{{Type: "added", Kind: "function", Name: "Alpha", AfterStartLine: 3}},
	}))
	store := newTestStore(t)
	now := fixedNow()
	first, err := Build(context.Background(), runner, store, BuildOptions{RepoDir: testRepoDir, Limit: 100, Now: now})
	if err != nil {
		t.Fatalf("first build: %v", err)
	}
	if first.Indexed != 1 {
		t.Fatalf("first pass indexed %d, want 1", first.Indexed)
	}
	diffCallsAfterFirst := runner.called("graph diff")

	second, err := Build(context.Background(), runner, store, BuildOptions{RepoDir: testRepoDir, Limit: 100, Now: now})
	if err != nil {
		t.Fatalf("second build: %v", err)
	}
	if second.Indexed != 0 {
		t.Fatalf("second pass indexed %d, want 0", second.Indexed)
	}
	if got := runner.called("graph diff"); got != diffCallsAfterFirst {
		t.Fatalf("second pass invoked the provider %d extra time(s)", got-diffCallsAfterFirst)
	}

	// A --full re-walk must still skip the already-indexed commit rather than
	// re-diffing or double-appending to the reverse list.
	full, err := Build(context.Background(), runner, store, BuildOptions{RepoDir: testRepoDir, Limit: 100, Full: true, Now: now})
	if err != nil {
		t.Fatalf("full build: %v", err)
	}
	if full.Indexed != 0 || full.Skipped != 1 {
		t.Fatalf("full pass indexed=%d skipped=%d, want 0/1", full.Indexed, full.Skipped)
	}
	snap := loadSnapshot(t, store)
	if got := snap.Commits(EntityKey("a.go", "function", "Alpha")); len(got) != 1 {
		t.Fatalf("reverse index has %d entries after re-runs, want 1: %v", len(got), got)
	}
}

// manyEntityChanges synthesizes n distinct "added function" changes in one
// file, named so their sort order (by Path/Kind/Name) is Fn0, Fn1, ... Fn(n-1)
// only for n<=10; callers past that only need a count, not a fixed order.
func manyEntityChanges(n int) []graphEntityChange {
	out := make([]graphEntityChange, n)
	for i := range out {
		out[i] = graphEntityChange{Type: "added", Kind: "function", Name: fmt.Sprintf("Fn%05d", i), AfterStartLine: i + 1}
	}
	return out
}

// TestBuildTruncatesAndMarksTheDeltaDocument pins entity-index audit fix (a): a
// commit whose entity count exceeds maxDeltaEntities must be recorded as
// truncated, with the TRUE count preserved, rather than silently stored as if
// it were complete. Before this fix a 2001-entity commit was marked fully
// indexed with no marker at all, so it stayed silently under-indexed forever.
func TestBuildTruncatesAndMarksTheDeltaDocument(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner()
	const trueCount = maxDeltaEntities + 5
	commits := []commitInfo{
		{SHA: "aaaa111111111111111111111111111111111111", Parent: "", CommittedAt: commitAt(0), Message: "root\n"},
	}
	scriptRepo(runner, "main", commits)
	runner.set(graphDiffKey(EmptyTreeSHA, commits[0].SHA), graphOutput(EmptyTreeSHA, commits[0].SHA, graphFileChange{
		Path:    "a.go",
		Changes: manyEntityChanges(trueCount),
	}))
	store := newTestStore(t)
	result, err := Build(context.Background(), runner, store, BuildOptions{RepoDir: testRepoDir, Limit: 100, Now: fixedNow()})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if result.Indexed != 1 {
		t.Fatalf("indexed=%d, want 1", result.Indexed)
	}
	if len(result.Warnings) == 0 {
		t.Fatalf("expected a truncation warning, got none")
	}

	snap := loadSnapshot(t, store)
	delta, ok := snap.Delta(commits[0].SHA)
	if !ok {
		t.Fatal("forward delta document missing")
	}
	if !delta.Truncated {
		t.Fatalf("delta.Truncated = false, want true for a %d-entity commit capped at %d", trueCount, maxDeltaEntities)
	}
	if delta.EntityCount != trueCount {
		t.Fatalf("delta.EntityCount = %d, want the true count %d", delta.EntityCount, trueCount)
	}
	if len(delta.Entities) != maxDeltaEntities {
		t.Fatalf("stored %d entities, want exactly the cap %d", len(delta.Entities), maxDeltaEntities)
	}

	// A commit truncated at the cap that is STILL the current cap must not be
	// re-diffed by a --full repair pass: nothing changed that would let it
	// recover more of itself, so re-indexing would only reproduce the same
	// truncation at provider-invocation cost.
	full, err := Build(context.Background(), runner, store, BuildOptions{RepoDir: testRepoDir, Limit: 100, Full: true, Now: fixedNow()})
	if err != nil {
		t.Fatalf("full build: %v", err)
	}
	if full.Indexed != 0 || full.Skipped != 1 {
		t.Fatalf("full pass indexed=%d skipped=%d, want 0/1 (an unchanged cap must not trigger a re-diff)", full.Indexed, full.Skipped)
	}
}

// TestBuildFullPassRepairsATruncatedCommitWhenTheCapHasRisen pins the other
// half of fix (a): the window/coverage logic must treat a commit that was
// truncated under a LOWER cap than the one in effect now as not fully
// indexed, so a --full repair pass re-diffs it and recovers the entities the
// old, lower cap had dropped. The lower-cap document is seeded directly (the
// production cap is a frozen constant), simulating history written before a
// cap increase shipped.
func TestBuildFullPassRepairsATruncatedCommitWhenTheCapHasRisen(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner()
	commits := []commitInfo{
		{SHA: "aaaa111111111111111111111111111111111111", Parent: "", CommittedAt: commitAt(0), Message: "root\n"},
	}
	scriptRepo(runner, "main", commits)
	const trueCount = 12
	const oldCap = 5 // stands in for a maxDeltaEntities value lower than today's
	fullDelta := graphOutput(EmptyTreeSHA, commits[0].SHA, graphFileChange{
		Path:    "a.go",
		Changes: manyEntityChanges(trueCount),
	})
	runner.set(graphDiffKey(EmptyTreeSHA, commits[0].SHA), fullDelta)
	fullyDiffed, err := ParseDiff([]byte(fullDelta), EmptyTreeSHA, commits[0].SHA, fixedNow()())
	if err != nil {
		t.Fatalf("parse full diff: %v", err)
	}
	if len(fullyDiffed.Entities) != trueCount {
		t.Fatalf("fixture entity count = %d, want %d", len(fullyDiffed.Entities), trueCount)
	}

	store := newTestStore(t)
	// Seed a commit doc truncated at oldCap (< maxDeltaEntities) plus the
	// window that already claims it covered, bypassing Build entirely — this
	// is the state a real repo would be in the moment after a cap increase
	// shipped, before any repair pass ran.
	stale := fullyDiffed
	stale.Truncated = true
	stale.EntityCount = trueCount
	stale.Entities = append([]EntityDelta(nil), fullyDiffed.Entities[:oldCap]...)
	encoded, err := json.Marshal(stale)
	if err != nil {
		t.Fatalf("encode stale delta: %v", err)
	}
	windowKey := WindowKey("main")
	seedMuts := []gitmeta.Mutation{
		{Op: gitmeta.OpSetString, Target: CommitTarget(commits[0].SHA), Key: ForwardKey, Value: string(encoded)},
		{Op: gitmeta.OpSetString, Target: projectTarget, Key: windowKey, Value: EncodeWindow(IndexWindow{Floor: commits[0].SHA, Tip: commits[0].SHA})},
	}
	for _, entity := range stale.Entities {
		seedMuts = append(seedMuts, gitmeta.Mutation{Op: gitmeta.OpListPush, Target: projectTarget, Key: EntityRecordKey(entity.Key()), Value: commits[0].SHA, NowMS: 123})
	}
	if _, err := store.Update(len(seedMuts), func(current gitmeta.State) (gitmeta.State, error) {
		return applyBatch(current, seedMuts), nil
	}); err != nil {
		t.Fatalf("seed stale state: %v", err)
	}

	// Sanity: before repair, the stored document is still the stale, truncated
	// one.
	before := loadSnapshot(t, store)
	staleDelta, ok := before.Delta(commits[0].SHA)
	if !ok || !staleDelta.Truncated || len(staleDelta.Entities) != oldCap {
		t.Fatalf("seed did not land as expected: %+v (ok=%v)", staleDelta, ok)
	}

	full, err := Build(context.Background(), runner, store, BuildOptions{RepoDir: testRepoDir, Limit: 100, Full: true, Now: fixedNow()})
	if err != nil {
		t.Fatalf("full build: %v", err)
	}
	if full.Indexed != 1 {
		t.Fatalf("full repair pass indexed=%d, want 1 (the stale-cap commit must be re-diffed)", full.Indexed)
	}

	after := loadSnapshot(t, store)
	repaired, ok := after.Delta(commits[0].SHA)
	if !ok {
		t.Fatal("repaired delta document missing")
	}
	if repaired.Truncated {
		t.Fatalf("repaired delta still truncated after the cap covers the whole commit: %+v", repaired)
	}
	if len(repaired.Entities) != trueCount {
		t.Fatalf("repaired delta has %d entities, want the full %d", len(repaired.Entities), trueCount)
	}
	for _, entity := range stale.Entities {
		if got := after.Commits(entity.Key()); len(got) != 1 {
			t.Fatalf("repair duplicated reverse history: %v", got)
		}
	}

	state, err := store.State()
	if err != nil {
		t.Fatal(err)
	}
	for _, list := range state.Lists {
		for _, entity := range stale.Entities {
			if list.Key == EntityRecordKey(entity.Key()) && (len(list.Entries) != 1 || list.Entries[0].Timestamp != 123) {
				t.Fatalf("repair changed existing timestamps: %+v", list)
			}
		}
	}

}

// TestBuildLimitedPassesConvergeBackwards pins the --limit contract: a bounded
// pass covers the NEWEST unindexed commits, claims only the range it actually
// covered, and the next pass pulls the floor further back — without re-walking
// or re-checking the part already inside the window.
func TestBuildLimitedPassesConvergeBackwards(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner()
	commits := []commitInfo{
		{SHA: "1111111111111111111111111111111111111111", Parent: "", CommittedAt: commitAt(0), Message: "one\n"},
		{SHA: "2222222222222222222222222222222222222222", Parent: "1111111111111111111111111111111111111111", CommittedAt: commitAt(1), Message: "two\n"},
		{SHA: "3333333333333333333333333333333333333333", Parent: "2222222222222222222222222222222222222222", CommittedAt: commitAt(2), Message: "three\n"},
	}
	scriptRepo(runner, "main", commits)
	bases := []string{EmptyTreeSHA, commits[0].SHA, commits[1].SHA}
	names := []string{"One", "Two", "Three"}
	for i, commit := range commits {
		runner.set(graphDiffKey(bases[i], commit.SHA), graphOutput(bases[i], commit.SHA, graphFileChange{
			Path:    "a.go",
			Changes: []graphEntityChange{{Type: "added", Kind: "function", Name: names[i], AfterStartLine: 3}},
		}))
	}

	store := newTestStore(t)
	now := fixedNow()
	first, err := Build(context.Background(), runner, store, BuildOptions{RepoDir: testRepoDir, Limit: 1, Now: now})
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if first.Indexed != 1 || first.NewCommits[0] != commits[2].SHA {
		t.Fatalf("a limited pass must index the NEWEST commit first, got %+v", first)
	}
	if want := (IndexWindow{Floor: commits[2].SHA, Tip: commits[2].SHA}); first.Window != want {
		t.Fatalf("a bounded pass claimed %+v, want exactly the one commit it covered (%+v)", first.Window, want)
	}

	second, err := Build(context.Background(), runner, store, BuildOptions{RepoDir: testRepoDir, Limit: 1, Now: now})
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if second.Indexed != 1 || second.NewCommits[0] != commits[1].SHA {
		t.Fatalf("the second bounded pass must move BEHIND the first, got %+v", second)
	}
	if want := (IndexWindow{Floor: commits[1].SHA, Tip: commits[2].SHA}); second.Window != want {
		t.Fatalf("second window = %+v, want %+v", second.Window, want)
	}
	// The tip is INSIDE the window, so the second pass never even looks at it:
	// a mark-based index had to re-walk and re-check it every time.
	if second.Skipped != 0 || second.Scanned != 1 {
		t.Fatalf("a bounded pass re-examined covered history: %+v", second)
	}

	third, err := Build(context.Background(), runner, store, BuildOptions{RepoDir: testRepoDir, Limit: 1, Now: now})
	if err != nil {
		t.Fatalf("third pass: %v", err)
	}
	if want := (IndexWindow{Floor: commits[0].SHA, Tip: commits[2].SHA}); third.Indexed != 1 || third.Window != want {
		t.Fatalf("the completing pass must claim the whole branch, got %+v", third)
	}
	snap := loadSnapshot(t, store)
	for i, name := range names {
		if got := snap.Commits(EntityKey("a.go", "function", name)); len(got) != 1 || got[0] != commits[i].SHA {
			t.Fatalf("%s indexed as %v", name, got)
		}
	}
}

func TestBuildRecordsRenameAndMoveAliases(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner()
	commits := []commitInfo{
		{SHA: "1111111111111111111111111111111111111111", Parent: "", CommittedAt: commitAt(0), Message: "add\n"},
		{SHA: "2222222222222222222222222222222222222222", Parent: "1111111111111111111111111111111111111111", CommittedAt: commitAt(1), Message: "rename\n"},
		{SHA: "3333333333333333333333333333333333333333", Parent: "2222222222222222222222222222222222222222", CommittedAt: commitAt(2), Message: "move\n"},
	}
	scriptRepo(runner, "main", commits)
	runner.set(graphDiffKey(EmptyTreeSHA, commits[0].SHA), graphOutput(EmptyTreeSHA, commits[0].SHA, graphFileChange{
		Path:    "old/pkg.go",
		Changes: []graphEntityChange{{Type: "added", Kind: "function", Name: "OldName", AfterStartLine: 3}},
	}))
	runner.set(graphDiffKey(commits[0].SHA, commits[1].SHA), graphOutput(commits[0].SHA, commits[1].SHA, graphFileChange{
		Path:    "old/pkg.go",
		Changes: []graphEntityChange{{Type: "renamed", Kind: "function", Name: "MidName", OldName: "OldName", NewName: "MidName", AfterStartLine: 3}},
	}))
	runner.set(graphDiffKey(commits[1].SHA, commits[2].SHA), graphOutput(commits[1].SHA, commits[2].SHA, graphFileChange{
		Path:    "new/pkg.go",
		OldPath: "old/pkg.go",
		Changes: []graphEntityChange{{Type: "moved", Kind: "function", Name: "NewName", OldName: "MidName", NewName: "NewName", OldPath: "old/pkg.go", NewPath: "new/pkg.go", AfterStartLine: 5}},
	}))

	store := newTestStore(t)
	if _, err := Build(context.Background(), runner, store, BuildOptions{RepoDir: testRepoDir, Limit: 100, Now: fixedNow()}); err != nil {
		t.Fatalf("build: %v", err)
	}
	snap := loadSnapshot(t, store)

	oldest := EntityKey("old/pkg.go", "function", "OldName")
	newest := EntityKey("new/pkg.go", "function", "NewName")
	if got := snap.Resolve(oldest); got != newest {
		t.Fatalf("alias chain resolved %q -> %q, want %q", oldest, got, newest)
	}
	// Asking by the ORIGINAL key must surface the entity's whole history,
	// including the commit recorded under the intermediate name.
	got := snap.Commits(oldest)
	want := []string{commits[0].SHA, commits[1].SHA, commits[2].SHA}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("commits via the old key = %v, want %v", got, want)
	}
	// And the search surface reports the current key with the old one recorded.
	matches := snap.Search("OldName", 10)
	if len(matches) != 1 || matches[0].EntityKey != newest || matches[0].AliasOf != oldest {
		t.Fatalf("search by the old name = %+v", matches)
	}
}

// TestResolveAliasSurvivesARenameBackCycle pins entity-index audit fix (b): an
// entity renamed A -> B and later renamed BACK B -> A must resolve to A, the
// spelling that actually exists in the tree at HEAD — not B, a name nothing is
// called any more. AliasRecordKey(A) ("A -> B", from the first rename) and
// AliasRecordKey(B) ("B -> A", from the second) BOTH persist forever — nothing
// ever retracts the first edge, because the second rename touches a different
// key — so the alias graph genuinely contains a 2-cycle; the fix is
// resolveAlias breaking it with the reverse index's per-key recency instead of
// returning whichever node it happened to visit right before detecting the
// repeat.
func TestResolveAliasSurvivesARenameBackCycle(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner()
	commits := []commitInfo{
		{SHA: "1111111111111111111111111111111111111111", Parent: "", CommittedAt: commitAt(0), Message: "add\n"},
		{SHA: "2222222222222222222222222222222222222222", Parent: "1111111111111111111111111111111111111111", CommittedAt: commitAt(1), Message: "rename A to B\n"},
		{SHA: "3333333333333333333333333333333333333333", Parent: "2222222222222222222222222222222222222222", CommittedAt: commitAt(2), Message: "rename B back to A\n"},
	}
	scriptRepo(runner, "main", commits)
	runner.set(graphDiffKey(EmptyTreeSHA, commits[0].SHA), graphOutput(EmptyTreeSHA, commits[0].SHA, graphFileChange{
		Path:    "pkg.go",
		Changes: []graphEntityChange{{Type: "added", Kind: "function", Name: "A", AfterStartLine: 3}},
	}))
	runner.set(graphDiffKey(commits[0].SHA, commits[1].SHA), graphOutput(commits[0].SHA, commits[1].SHA, graphFileChange{
		Path:    "pkg.go",
		Changes: []graphEntityChange{{Type: "renamed", Kind: "function", Name: "B", OldName: "A", NewName: "B", AfterStartLine: 3}},
	}))
	runner.set(graphDiffKey(commits[1].SHA, commits[2].SHA), graphOutput(commits[1].SHA, commits[2].SHA, graphFileChange{
		Path:    "pkg.go",
		Changes: []graphEntityChange{{Type: "renamed", Kind: "function", Name: "A", OldName: "B", NewName: "A", AfterStartLine: 3}},
	}))

	store := newTestStore(t)
	if _, err := Build(context.Background(), runner, store, BuildOptions{RepoDir: testRepoDir, Limit: 100, Now: fixedNow()}); err != nil {
		t.Fatalf("build: %v", err)
	}
	snap := loadSnapshot(t, store)

	a := EntityKey("pkg.go", "function", "A")
	b := EntityKey("pkg.go", "function", "B")
	if got := snap.Resolve(a); got != a {
		t.Fatalf("Resolve(%q) = %q, want %q (the live spelling, back where it started)", a, got, a)
	}
	if got := snap.Resolve(b); got != a {
		t.Fatalf("Resolve(%q) = %q, want %q (an old spelling must resolve to the live one)", b, got, a)
	}
	// Confirm the premise: the alias graph genuinely holds BOTH edges (a real
	// 2-cycle), so the assertions above are exercising the tie-break, not some
	// other mechanism that happens to avoid the cycle entirely.
	aliases := snap.Aliases()
	if aliases[a] != b || aliases[b] != a {
		t.Fatalf("alias map = %+v, want a real A<->B cycle (both edges present)", aliases)
	}
	// A query by the old name must still find the entity, reported under its
	// current (and, here, original) key.
	matches := snap.Search("B", 10)
	if len(matches) != 1 || matches[0].EntityKey != a || matches[0].AliasOf != b {
		t.Fatalf("search by the intermediate name = %+v", matches)
	}
	// The whole history is still reachable from any spelling it was ever
	// known by — order is not chronological across keys (Commits' own
	// contract: sort by commit date if you need that), so compare as a set.
	want := []string{commits[0].SHA, commits[1].SHA, commits[2].SHA}
	if got := snap.Commits(a); !sameSet(got, want) {
		t.Fatalf("commits via the current key = %v, want the set %v", got, want)
	}
	if got := snap.Commits(b); !sameSet(got, want) {
		t.Fatalf("commits via the intermediate key = %v, want the set %v", got, want)
	}
}

// sameSet reports whether got and want hold the same elements, ignoring order
// and duplicate count.
func sameSet(got, want []string) bool {
	g := map[string]struct{}{}
	for _, v := range got {
		g[v] = struct{}{}
	}
	w := map[string]struct{}{}
	for _, v := range want {
		w[v] = struct{}{}
	}
	if len(g) != len(w) {
		return false
	}
	for v := range w {
		if _, ok := g[v]; !ok {
			return false
		}
	}
	return true
}

func TestBuildCheckpointsOnlySkipsPlainCommits(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner()
	commits := []commitInfo{
		{SHA: "aaaa111111111111111111111111111111111111", Parent: "", CommittedAt: commitAt(0), Message: "plain commit\n"},
		{SHA: "bbbb222222222222222222222222222222222222", Parent: "aaaa111111111111111111111111111111111111", CommittedAt: commitAt(1), Message: "agent work\n\nEntire-Checkpoint: 01J0ABCDEFGHJKMNPQRSTVWXYZ\n"},
	}
	scriptRepo(runner, "main", commits)
	runner.set(graphDiffKey(commits[0].SHA, commits[1].SHA), graphOutput(commits[0].SHA, commits[1].SHA, graphFileChange{
		Path:    "a.go",
		Changes: []graphEntityChange{{Type: "added", Kind: "function", Name: "Beta", AfterStartLine: 7}},
	}))

	store := newTestStore(t)
	result, err := Build(context.Background(), runner, store, BuildOptions{RepoDir: testRepoDir, Limit: 100, CheckpointsOnly: true, Now: fixedNow()})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if result.Indexed != 1 || result.Skipped != 1 {
		t.Fatalf("indexed=%d skipped=%d, want 1/1", result.Indexed, result.Skipped)
	}
	if runner.called("graph diff --repo "+testRepoDir+" --base "+EmptyTreeSHA) != 0 {
		t.Fatal("the provider was invoked for a non-checkpoint commit")
	}
}

// TestBuildCheckpointsOnlyLeavesTheWindowUntouched is the exact defect a single
// high-water mark had: a filtered pass reached the tip, so the mark jumped to
// it, and every plain commit it deliberately skipped was then behind the mark —
// permanently unindexed, because no later pass would ever look below it again.
// A filtered pass may index; it may not claim coverage.
func TestBuildCheckpointsOnlyLeavesTheWindowUntouched(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner()
	commits := []commitInfo{
		{SHA: "aaaa111111111111111111111111111111111111", Parent: "", CommittedAt: commitAt(0), Message: "plain commit\n"},
		{SHA: "bbbb222222222222222222222222222222222222", Parent: "aaaa111111111111111111111111111111111111", CommittedAt: commitAt(1), Message: "agent work\n\nEntire-Checkpoint: 01J0ABCDEFGHJKMNPQRSTVWXYZ\n"},
	}
	scriptRepo(runner, "main", commits)
	runner.set(graphDiffKey(EmptyTreeSHA, commits[0].SHA), graphOutput(EmptyTreeSHA, commits[0].SHA, graphFileChange{
		Path:    "a.go",
		Changes: []graphEntityChange{{Type: "added", Kind: "function", Name: "Alpha", AfterStartLine: 3}},
	}))
	runner.set(graphDiffKey(commits[0].SHA, commits[1].SHA), graphOutput(commits[0].SHA, commits[1].SHA, graphFileChange{
		Path:    "a.go",
		Changes: []graphEntityChange{{Type: "body_changed", Kind: "function", Name: "Alpha", AfterStartLine: 3}},
	}))

	store := newTestStore(t)
	now := fixedNow()
	filtered, err := Build(context.Background(), runner, store, BuildOptions{RepoDir: testRepoDir, Limit: 100, CheckpointsOnly: true, Now: now})
	if err != nil {
		t.Fatalf("filtered pass: %v", err)
	}
	if filtered.Indexed != 1 {
		t.Fatalf("filtered pass indexed %d, want the one checkpoint commit", filtered.Indexed)
	}
	if !filtered.Window.Empty() {
		t.Fatalf("a filtered pass claimed coverage: %+v", filtered.Window)
	}
	if _, stored := windowFor(t, store, "main"); stored {
		t.Fatal("a filtered pass persisted a window record")
	}

	// The plain commit the filtered pass skipped must still be reachable.
	full, err := Build(context.Background(), runner, store, BuildOptions{RepoDir: testRepoDir, Limit: 100, Now: now})
	if err != nil {
		t.Fatalf("default pass: %v", err)
	}
	if full.Scanned == 0 {
		t.Fatalf("the default pass scanned nothing; the filtered pass had claimed the branch: %+v", full)
	}
	if full.Indexed != 1 || full.NewCommits[0] != commits[0].SHA {
		t.Fatalf("the default pass did not index the skipped plain commit: %+v", full)
	}
	if want := (IndexWindow{Floor: commits[0].SHA, Tip: commits[1].SHA}); full.Window != want {
		t.Fatalf("window = %+v, want %+v", full.Window, want)
	}
	snap := loadSnapshot(t, store)
	if _, ok := snap.Delta(commits[0].SHA); !ok {
		t.Fatalf("commit %s is still unindexed after a default pass", commits[0].SHA)
	}
}

// TestBuildTickWalksOnlyTheNewCommits pins the freshness tick's cost model: a
// tick lists tip..head, not the whole branch. Reading the branch's history on
// every tick is what made a bounded tick re-walk twenty thousand commits, find
// itself truncated, and never advance.
func TestBuildTickWalksOnlyTheNewCommits(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner()
	commits := []commitInfo{
		{SHA: "1111111111111111111111111111111111111111", Parent: "", CommittedAt: commitAt(0), Message: "one\n"},
		{SHA: "2222222222222222222222222222222222222222", Parent: "1111111111111111111111111111111111111111", CommittedAt: commitAt(1), Message: "two\n"},
	}
	scriptRepo(runner, "main", commits)
	for i, base := range []string{EmptyTreeSHA, commits[0].SHA} {
		runner.set(graphDiffKey(base, commits[i].SHA), graphOutput(base, commits[i].SHA, graphFileChange{
			Path:    "a.go",
			Changes: []graphEntityChange{{Type: "added", Kind: "function", Name: fmt.Sprintf("F%d", i), AfterStartLine: 3}},
		}))
	}
	store := newTestStore(t)
	now := fixedNow()
	if _, err := Build(context.Background(), runner, store, BuildOptions{RepoDir: testRepoDir, Limit: 100, Now: now}); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	// A third commit lands; the tick must reach it by walking only the delta.
	third := commitInfo{SHA: "3333333333333333333333333333333333333333", Parent: commits[1].SHA, CommittedAt: commitAt(2), Message: "three\n"}
	commits = append(commits, third)
	scriptRepo(runner, "main", commits)
	runner.set(graphDiffKey(commits[1].SHA, third.SHA), graphOutput(commits[1].SHA, third.SHA, graphFileChange{
		Path:    "a.go",
		Changes: []graphEntityChange{{Type: "added", Kind: "function", Name: "F2", AfterStartLine: 9}},
	}))
	runner.calls = nil

	tick, err := Build(context.Background(), runner, store, BuildOptions{RepoDir: testRepoDir, Limit: 10, Now: now})
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if tick.Indexed != 1 || tick.NewCommits[0] != third.SHA {
		t.Fatalf("tick = %+v, want exactly the new commit", tick)
	}
	if want := (IndexWindow{Floor: commits[0].SHA, Tip: third.SHA}); tick.Window != want {
		t.Fatalf("tick window = %+v, want %+v", tick.Window, want)
	}
	// Exactly one walk, of exactly tip..head, bounded by the pass's own Limit —
	// not by maxWalkCommits, which would materialize a whole fast-forward's
	// commit bodies for a ten-commit budget.
	if got := runner.called("git log --first-parent " + logFormat + " --max-count=10 " + commits[1].SHA + ".." + third.SHA); got != 1 {
		t.Fatalf("the tick did not walk exactly tip..head once within its budget (%d): %v", got, runner.calls)
	}
	// The branch's whole history was never listed: no walk names the head alone.
	for _, call := range runner.calls {
		if strings.HasSuffix(call, " "+third.SHA) && strings.Contains(call, "git log") {
			t.Fatalf("the tick walked the branch from the head: %q", call)
		}
	}
}

// TestBuildNonBlockingSkipsWhenTheStoreIsLocked pins the freshness tick's other
// promise: it never waits on the git-meta lock. flock(2) is not interruptible
// by a context, so a blocking write would outlive the tick's own deadline and
// hang the session-end hook behind whichever writer holds the store.
func TestBuildNonBlockingSkipsWhenTheStoreIsLocked(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner()
	commits := []commitInfo{{SHA: "aaaa111111111111111111111111111111111111", Parent: "", CommittedAt: commitAt(0), Message: "one\n"}}
	scriptRepo(runner, "main", commits)
	runner.set(graphDiffKey(EmptyTreeSHA, commits[0].SHA), graphOutput(EmptyTreeSHA, commits[0].SHA, graphFileChange{
		Path:    "a.go",
		Changes: []graphEntityChange{{Type: "added", Kind: "function", Name: "Alpha", AfterStartLine: 3}},
	}))
	store := newTestStore(t)

	release, err := store.TryLock()
	if err != nil {
		t.Fatalf("hold the store lock: %v", err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan BuildResult, 1)
	go func() {
		result, buildErr := Build(ctx, runner, store, BuildOptions{RepoDir: testRepoDir, Limit: 10, NonBlocking: true, Now: fixedNow()})
		if buildErr != nil {
			t.Errorf("non-blocking build: %v", buildErr)
		}
		done <- result
	}()
	select {
	case result := <-done:
		if !result.LockBusy {
			t.Fatalf("a contended non-blocking pass did not report the contention: %+v", result)
		}
		if result.Indexed != 0 || len(result.NewCommits) != 0 || !result.Window.Empty() {
			t.Fatalf("a pass that wrote nothing reported work done: %+v", result)
		}
	case <-ctx.Done():
		t.Fatal("the non-blocking pass blocked on the git-meta lock")
	}

	// And nothing was written.
	if tip, tipErr := store.Tip(); tipErr != nil || tip != "" {
		t.Fatalf("the contended pass wrote to the store: tip=%q err=%v", tip, tipErr)
	}
	if snap := loadSnapshot(t, store); !snap.Empty() {
		t.Fatal("the contended pass left index records behind")
	}
}

// TestBuildNonBlockingDoesNoWorkWhenTheStoreIsLocked pins the ORDER of the
// lock against the work. The lock used to be taken at the write, after the
// walk and after one `entire graph diff` subprocess per new commit: a
// contended tick burned its whole budget (up to ten provider invocations
// inside a 90s deadline) and then threw every byte of it away. A contended
// pass must cost one syscall, not one subprocess per commit.
func TestBuildNonBlockingDoesNoWorkWhenTheStoreIsLocked(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner()
	commits := []commitInfo{
		{SHA: "aaaa111111111111111111111111111111111111", Parent: "", CommittedAt: commitAt(0), Message: "one\n"},
		{SHA: "bbbb222222222222222222222222222222222222", Parent: "aaaa111111111111111111111111111111111111", CommittedAt: commitAt(1), Message: "two\n"},
	}
	scriptRepo(runner, "main", commits)
	for i, base := range []string{EmptyTreeSHA, commits[0].SHA} {
		runner.set(graphDiffKey(base, commits[i].SHA), graphOutput(base, commits[i].SHA, graphFileChange{
			Path:    "a.go",
			Changes: []graphEntityChange{{Type: "added", Kind: "function", Name: fmt.Sprintf("F%d", i), AfterStartLine: 3}},
		}))
	}
	store := newTestStore(t)

	release, err := store.TryLock()
	if err != nil {
		t.Fatalf("hold the store lock: %v", err)
	}
	defer release()

	runner.calls = nil
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan BuildResult, 1)
	go func() {
		result, buildErr := Build(ctx, runner, store, BuildOptions{RepoDir: testRepoDir, Limit: 10, NonBlocking: true, Now: fixedNow()})
		if buildErr != nil {
			t.Errorf("non-blocking build: %v", buildErr)
		}
		done <- result
	}()
	select {
	case result := <-done:
		if !result.LockBusy {
			t.Fatalf("a contended non-blocking pass did not report the contention: %+v", result)
		}
	case <-ctx.Done():
		t.Fatal("the non-blocking pass blocked on the git-meta lock")
	}

	// The producer is the expensive part and it must not have run at all.
	if got := runner.called("entire graph diff"); got != 0 {
		t.Fatalf("a contended pass invoked the provider %d time(s): %v", got, runner.calls)
	}
	// Nor may it have walked the history to decide what to feed the producer.
	if got := runner.called("git log"); got != 0 {
		t.Fatalf("a contended pass walked the history %d time(s): %v", got, runner.calls)
	}
	if tip, tipErr := store.Tip(); tipErr != nil || tip != "" {
		t.Fatalf("the contended pass wrote to the store: tip=%q err=%v", tip, tipErr)
	}
}

// TestBuildForwardWalkTakesTheOldestEndOfItsBudget pins the budgeted forward
// walk. `git log --max-count=N` keeps the NEWEST N, so a ten-commit tick after
// a large fast-forward would either materialize the whole range or advance the
// tip over commits it never listed. It must take the range's OLDEST end and
// advance contiguously, one budget at a time.
func TestBuildForwardWalkTakesTheOldestEndOfItsBudget(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner()
	commits := []commitInfo{{SHA: strings.Repeat("0", 40), Parent: "", CommittedAt: commitAt(0), Message: "root\n"}}
	scriptRepo(runner, "main", commits)
	runner.set(graphDiffKey(EmptyTreeSHA, commits[0].SHA), graphOutput(EmptyTreeSHA, commits[0].SHA, graphFileChange{
		Path:    "a.go",
		Changes: []graphEntityChange{{Type: "added", Kind: "function", Name: "F0", AfterStartLine: 3}},
	}))
	store := newTestStore(t)
	now := fixedNow()
	if _, err := Build(context.Background(), runner, store, BuildOptions{RepoDir: testRepoDir, Limit: 10, Now: now}); err != nil {
		t.Fatalf("establish: %v", err)
	}

	// Twenty commits land at once; the tick's budget is two.
	for i := 1; i <= 20; i++ {
		sha := fmt.Sprintf("%02d", i) + strings.Repeat("f", 38)
		commits = append(commits, commitInfo{SHA: sha, Parent: commits[i-1].SHA, CommittedAt: commitAt(i), Message: fmt.Sprintf("c%d\n", i)})
		runner.set(graphDiffKey(commits[i-1].SHA, sha), graphOutput(commits[i-1].SHA, sha, graphFileChange{
			Path:    "a.go",
			Changes: []graphEntityChange{{Type: "body_changed", Kind: "function", Name: "F0", AfterStartLine: 3}},
		}))
	}
	scriptRepo(runner, "main", commits)

	runner.calls = nil
	tick, err := Build(context.Background(), runner, store, BuildOptions{RepoDir: testRepoDir, Limit: 2, Now: now})
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	// The two commits ADJACENT to the old tip, not the two newest.
	if got := tick.NewCommits; len(got) != 2 || got[0] != commits[1].SHA || got[1] != commits[2].SHA {
		t.Fatalf("tick indexed %v, want the oldest two of the range (%s, %s)", got, commits[1].SHA, commits[2].SHA)
	}
	if tick.Window.Tip != commits[2].SHA {
		t.Fatalf("tip = %s, want a contiguous advance to %s", tick.Window.Tip, commits[2].SHA)
	}
	// And it materialized two commit bodies, not twenty: the walk is bounded by
	// the budget and skips past the range's newest end.
	want := "git log --first-parent " + logFormat + " --max-count=2 --skip=18 " + commits[0].SHA + ".." + commits[20].SHA
	if got := runner.called(want); got != 1 {
		t.Fatalf("the forward walk was not budget-bounded to the range's oldest end (%d): %v", got, runner.calls)
	}
	for _, call := range runner.calls {
		if strings.Contains(call, "git log") && strings.Contains(call, fmt.Sprintf("--max-count=%d", maxWalkCommits)) {
			t.Fatalf("a budgeted pass materialized the whole walk ceiling: %q", call)
		}
	}

	// Repeated ticks converge on the rest, still two at a time.
	for round := 0; round < 9; round++ {
		if _, err := Build(context.Background(), runner, store, BuildOptions{RepoDir: testRepoDir, Limit: 2, Now: now}); err != nil {
			t.Fatalf("tick %d: %v", round, err)
		}
	}
	window, ok := windowFor(t, store, "main")
	if !ok || window.Tip != commits[20].SHA || window.Floor != commits[0].SHA {
		t.Fatalf("window = %+v ok=%v, want the whole history covered", window, ok)
	}
}

// TestBuildConvergesWhenTheHeadCannotBeDiffed pins the anti-stall invariant.
// The window used to be HEAD-anchored: if the head itself failed to diff,
// covered[head] stayed false, the window was never established, and every
// later tick re-walked the entire budget forever. Anchoring at the newest
// DIFFABLE commit demotes an undiffable head to an ordinary forward-path hole,
// so repeated ticks cost O(new commits).
func TestBuildConvergesWhenTheHeadCannotBeDiffed(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner()
	var commits []commitInfo
	for i := 0; i < 6; i++ {
		sha := fmt.Sprintf("%d", i) + strings.Repeat("a", 39)
		parent := ""
		base := EmptyTreeSHA
		if i > 0 {
			parent = commits[i-1].SHA
			base = parent
		}
		commits = append(commits, commitInfo{SHA: sha, Parent: parent, CommittedAt: commitAt(i), Message: fmt.Sprintf("c%d\n", i)})
		runner.set(graphDiffKey(base, sha), graphOutput(base, sha, graphFileChange{
			Path:    "a.go",
			Changes: []graphEntityChange{{Type: "added", Kind: "function", Name: fmt.Sprintf("F%d", i), AfterStartLine: 3}},
		}))
	}
	head := commits[len(commits)-1]
	scriptRepo(runner, "main", commits)
	// The head, and only the head, is undiffable — forever.
	runner.fail(graphDiffKey(commits[len(commits)-2].SHA, head.SHA), fmt.Errorf("provider exploded on the head"))

	store := newTestStore(t)
	now := fixedNow()
	first, err := Build(context.Background(), runner, store, BuildOptions{RepoDir: testRepoDir, Limit: 10, Now: now})
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if first.Window.Empty() {
		t.Fatalf("an undiffable head left the window unestablished: %+v", first)
	}
	if first.Window.Tip != commits[len(commits)-2].SHA {
		t.Fatalf("window tip = %s, want the newest DIFFABLE commit %s", first.Window.Tip, commits[len(commits)-2].SHA)
	}
	if first.Window.Floor != commits[0].SHA {
		t.Fatalf("window floor = %s, want the root %s", first.Window.Floor, commits[0].SHA)
	}

	// Every later tick must re-attempt only the head: one diff, and a walk of
	// the one-commit range — never the whole budget again.
	for round := 0; round < 3; round++ {
		runner.calls = nil
		tick, tickErr := Build(context.Background(), runner, store, BuildOptions{RepoDir: testRepoDir, Limit: 10, Now: now})
		if tickErr != nil {
			t.Fatalf("tick %d: %v", round, tickErr)
		}
		if tick.Indexed != 0 || tick.Failed != 1 {
			t.Fatalf("tick %d = %+v, want exactly one retried failure", round, tick)
		}
		if got := runner.called("entire graph diff"); got != 1 {
			t.Fatalf("tick %d invoked the provider %d time(s), want 1: %v", round, got, runner.calls)
		}
		if got := runner.called("entire graph diff --repo " + testRepoDir + " --base " + commits[len(commits)-2].SHA); got != 1 {
			t.Fatalf("tick %d re-diffed something other than the head: %v", round, runner.calls)
		}
		if tick.Window != first.Window {
			t.Fatalf("tick %d moved the window over an undiffable head: %+v", round, tick.Window)
		}
	}
}

// TestBuildKeepsFailedCommitOutsideTheWindow pins the window's honesty: a
// commit the provider could not diff is a hole, and neither cursor may step
// over it.
func TestBuildKeepsFailedCommitOutsideTheWindow(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner()
	commits := []commitInfo{
		{SHA: "aaaa111111111111111111111111111111111111", Parent: "", CommittedAt: commitAt(0), Message: "one\n"},
		{SHA: "bbbb222222222222222222222222222222222222", Parent: "aaaa111111111111111111111111111111111111", CommittedAt: commitAt(1), Message: "two\n"},
	}
	scriptRepo(runner, "main", commits)
	runner.fail(graphDiffKey(EmptyTreeSHA, commits[0].SHA), fmt.Errorf("provider exploded"))
	runner.set(graphDiffKey(commits[0].SHA, commits[1].SHA), graphOutput(commits[0].SHA, commits[1].SHA, graphFileChange{
		Path:    "a.go",
		Changes: []graphEntityChange{{Type: "added", Kind: "function", Name: "Gamma", AfterStartLine: 2}},
	}))

	store := newTestStore(t)
	result, err := Build(context.Background(), runner, store, BuildOptions{RepoDir: testRepoDir, Limit: 100, Now: fixedNow()})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if result.Failed != 1 || result.Indexed != 1 {
		t.Fatalf("failed=%d indexed=%d, want 1/1", result.Failed, result.Indexed)
	}
	if want := (IndexWindow{Floor: commits[1].SHA, Tip: commits[1].SHA}); result.Window != want {
		t.Fatalf("window = %+v; it must stop above the failed commit (%+v)", result.Window, want)
	}
	if len(result.Warnings) == 0 {
		t.Fatal("a provider failure was not reported as a warning")
	}
}

func TestBuildEmptyProviderOutputStillIndexesTheCommit(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner()
	commits := []commitInfo{{SHA: "aaaa111111111111111111111111111111111111", Parent: "", CommittedAt: commitAt(0), Message: "docs only\n"}}
	scriptRepo(runner, "main", commits)
	runner.set(graphDiffKey(EmptyTreeSHA, commits[0].SHA), "")

	store := newTestStore(t)
	result, err := Build(context.Background(), runner, store, BuildOptions{RepoDir: testRepoDir, Limit: 100, Now: fixedNow()})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if result.Indexed != 1 || result.Entities != 0 {
		t.Fatalf("indexed=%d entities=%d, want 1/0", result.Indexed, result.Entities)
	}
	snap := loadSnapshot(t, store)
	if _, ok := snap.Delta(commits[0].SHA); !ok {
		t.Fatal("a semantically-empty commit was not recorded, so it would be re-diffed forever")
	}
}

// --- cache rebuild ---------------------------------------------------------

func TestSnapshotRebuildsFromGitMetaAlone(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner()
	commits := []commitInfo{{SHA: "aaaa111111111111111111111111111111111111", Parent: "", CommittedAt: commitAt(0), Message: "one\n"}}
	scriptRepo(runner, "main", commits)
	runner.set(graphDiffKey(EmptyTreeSHA, commits[0].SHA), graphOutput(EmptyTreeSHA, commits[0].SHA, graphFileChange{
		Path:    "pkg/a.go",
		Changes: []graphEntityChange{{Type: "added", Kind: "function", Name: "Delta", AfterStartLine: 4}},
	}))
	store := newTestStore(t)
	if _, err := Build(context.Background(), runner, store, BuildOptions{RepoDir: testRepoDir, Limit: 100, Now: fixedNow()}); err != nil {
		t.Fatalf("build: %v", err)
	}
	tip, err := store.Tip()
	if err != nil || tip == "" {
		t.Fatalf("tip = %q, err = %v", tip, err)
	}

	// A completely fresh in-memory view of the SAME git dir (nothing cached)
	// must reproduce the index: that is what makes the derived cache safe to
	// throw away.
	rebuilt := loadSnapshot(t, store)
	if got := rebuilt.Commits(EntityKey("pkg/a.go", "function", "Delta")); !reflect.DeepEqual(got, []string{commits[0].SHA}) {
		t.Fatalf("rebuilt reverse index = %v", got)
	}
	if rebuilt.Empty() {
		t.Fatal("rebuilt snapshot reports empty")
	}
}

// --- pure mapping ----------------------------------------------------------

func TestParseDiffMapsProviderVocabulary(t *testing.T) {
	t.Parallel()
	raw := `{"base":"b","head":"h","files":[{"path":"a.go","status":"M","changes":[
	  {"type":"signature_changed","kind":"function","name":"Sig","old_signature":"func Sig()","new_signature":"func Sig(x int)","before_start_line":3,"after_start_line":3},
	  {"type":"removed","kind":"method","name":"Gone","old_signature":"func (t T) Gone()","before_start_line":40},
	  {"type":"weird_new_thing","kind":"function","name":"Future","after_start_line":9}]}]}`
	delta, err := ParseDiff([]byte(raw), "b", "h", time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	changes := map[string]EntityDelta{}
	for _, e := range delta.Entities {
		changes[e.Name] = e
	}
	if changes["Sig"].Change != ChangeModified || changes["Sig"].Signature != "func Sig(x int)" {
		t.Fatalf("signature_changed mapped to %+v", changes["Sig"])
	}
	if changes["Gone"].Change != ChangeRemoved || changes["Gone"].StartLine != 40 {
		t.Fatalf("removed mapped to %+v (a removal must keep its pre-change line)", changes["Gone"])
	}
	if changes["Future"].Change != ChangeModified {
		t.Fatalf("an unknown provider change type must fold to modified, got %+v", changes["Future"])
	}
	if delta.ProducerVersion != "" {
		t.Fatalf("absent schema_version must stay absent, got %q", delta.ProducerVersion)
	}
}

// TestParseDiffCarriesProviderVersionWhenPresent pins WHICH key becomes the
// stored producer_version. The provider's own schema_version, where other graph
// subcommands emit one, versions that command's envelope — copying it here
// recorded a number that describes something else entirely.
func TestParseDiffCarriesProviderVersionWhenPresent(t *testing.T) {
	t.Parallel()
	raw := `{"producer_version":"2.4.0","base":"b","head":"h","files":[]}`
	delta, err := ParseDiff([]byte(raw), "b", "h", time.Now())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if delta.ProducerVersion != "2.4.0" {
		t.Fatalf("producer_version = %q, want 2.4.0", delta.ProducerVersion)
	}

	// The producer emits no producer_version today, and an envelope
	// schema_version must NOT be borrowed as one.
	delta, err = ParseDiff([]byte(`{"schema_version":"1.1","base":"b","head":"h","files":[]}`), "b", "h", time.Now())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if delta.ProducerVersion != "" {
		t.Fatalf("schema_version leaked into producer_version as %q", delta.ProducerVersion)
	}
}

// TestParseDiffRecordsNoFingerprint pins the other half of the contract audit:
// the producer emits no per-change fingerprint, so no document may carry one.
func TestParseDiffRecordsNoFingerprint(t *testing.T) {
	t.Parallel()
	raw := `{"base":"b","head":"h","files":[{"path":"a.go","status":"M","changes":[
	  {"type":"added","kind":"function","name":"F","after_start_line":1,"fingerprint":"deadbeef"}]}]}`
	delta, err := ParseDiff([]byte(raw), "b", "h", time.Now())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(delta.Entities) != 1 {
		t.Fatalf("entities = %+v", delta.Entities)
	}
	if delta.Entities[0].Fingerprint != "" {
		t.Fatalf("a phantom fingerprint was decoded: %q", delta.Entities[0].Fingerprint)
	}
	encoded, err := json.Marshal(delta)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), "fingerprint") {
		t.Fatalf("stored document carries a fingerprint field: %s", encoded)
	}
}

// TestDiffCommitNamesTheRepositoryExplicitly pins the provider invocation: the
// repository is passed, never inherited. A provider that resolved a different
// repository from the ambient environment would index another tree's entities
// under these keys.
func TestDiffCommitNamesTheRepositoryExplicitly(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner()
	runner.set(graphDiffKey("base", "head"), graphOutput("base", "head"))
	if _, err := DiffCommit(context.Background(), runner, testRepoDir, "entire", "base", "head", time.Now()); err != nil {
		t.Fatalf("diff: %v", err)
	}
	if runner.called("--repo "+testRepoDir) != 1 {
		t.Fatalf("provider was not told which repository to read: %v", runner.calls)
	}
}

// TestWindowRecordRoundTrips locks the stored cursor encoding.
func TestWindowRecordRoundTrips(t *testing.T) {
	t.Parallel()
	window := IndexWindow{Floor: "aaaa1111", Tip: "bbbb2222"}
	got, ok := DecodeWindow(EncodeWindow(window))
	if !ok || got != window {
		t.Fatalf("round trip = %+v (ok=%v)", got, ok)
	}
	if err := gitmeta.ValidateKey(WindowKey("feature/x")); err != nil {
		t.Fatalf("window key is not a valid git-meta key: %v", err)
	}
	if branch, ok := DecodeWindowBranch(WindowKey("feature/x")); !ok || branch != "feature/x" {
		t.Fatalf("decode window branch = %q (ok=%v)", branch, ok)
	}
	for _, bad := range []string{"", "   ", "not json", `{"floor":"a"}`, `{"tip":"b"}`} {
		if _, ok := DecodeWindow(bad); ok {
			t.Fatalf("an incomplete window record %q was accepted as coverage", bad)
		}
	}
}

func TestEntityKeyRoundTrip(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ path, kind, name string }{
		{"internal/cli/export.go", "function", "runExport"},
		{"weird#dir/file.go", "method", "Do"},
		{"a.go", "type", "T"},
	} {
		key := EntityKey(tc.path, tc.kind, tc.name)
		gotPath, gotKind, gotName, ok := SplitEntityKey(key)
		if !ok || gotPath != tc.path || gotKind != tc.kind || gotName != tc.name {
			t.Fatalf("round trip of %q gave (%q,%q,%q,%v)", key, gotPath, gotKind, gotName, ok)
		}
		record := EntityRecordKey(key)
		if err := gitmeta.ValidateKey(record); err != nil {
			t.Fatalf("record key %q is not a valid git-meta key: %v", record, err)
		}
		decoded, alias, ok := DecodeEntityKey(record)
		if !ok || alias || decoded != key {
			t.Fatalf("decode(%q) = (%q,%v,%v)", record, decoded, alias, ok)
		}
		aliasRecord := AliasRecordKey(key)
		if err := gitmeta.ValidateKey(aliasRecord); err != nil {
			t.Fatalf("alias key %q is not a valid git-meta key: %v", aliasRecord, err)
		}
		if decoded, alias, ok := DecodeEntityKey(aliasRecord); !ok || !alias || decoded != key {
			t.Fatalf("decode alias(%q) = (%q,%v,%v)", aliasRecord, decoded, alias, ok)
		}
	}
}

func TestCheckpointIDsFromMessage(t *testing.T) {
	t.Parallel()
	msg := "work\n\nEntire-Checkpoint: 0123456789ab\nEntire-Checkpoint: 0123456789ab\nEntire-Checkpoint: 01J0ABCDEFGHJKMNPQRSTVWXYZ\n"
	got := CheckpointIDs(msg)
	want := []string{"0123456789ab", "01J0ABCDEFGHJKMNPQRSTVWXYZ"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("checkpoint ids = %v, want %v", got, want)
	}
	if len(CheckpointIDs("no trailers here")) != 0 {
		t.Fatal("a trailer-free message reported checkpoints")
	}
}

// TestApplyBatchMatchesReferenceApply pins the batched applier to
// gitmeta.State.Apply, the reference semantics it replaces for speed.
//
// It starts from a NON-EMPTY state on purpose. Every interesting divergence is
// in the update paths — an in-place string overwrite, a push onto a list that
// already has entries — and a batch folded onto State{} exercises none of them:
// every key takes the create path exactly once. The seed also puts a whole-key
// tombstone and a live string on the SAME key, which is the state a set must
// clean up and where the in-place path used to skip the tombstone drop.
func TestApplyBatchMatchesReferenceApply(t *testing.T) {
	t.Parallel()
	commit := CommitTarget("aaaa111111111111111111111111111111111111")
	other := CommitTarget("cccc333333333333333333333333333333333333")
	aKey := EntityRecordKey("a.go#function#A")
	bKey := EntityRecordKey("b.go#type#B")
	windowKey := WindowKey("main")

	seed := gitmeta.State{
		Strings: []gitmeta.StringVal{
			{Target: commit, Key: ForwardKey, Value: `{"stale":true}`},
			{Target: other, Key: ForwardKey, Value: `{"untouched":true}`},
			{Target: projectTarget, Key: windowKey, Value: `{"floor":"sha0","tip":"sha0"}`},
		},
		Lists: []gitmeta.ListVal{
			{Target: projectTarget, Key: aKey, Entries: []gitmeta.ListEntry{{Value: "sha0", Timestamp: 7}}},
			{Target: projectTarget, Key: EntityRecordKey("c.go#function#C"), Entries: []gitmeta.ListEntry{{Value: "sha0", Timestamp: 3}}},
		},
		Tombstones: []gitmeta.Tombstone{
			// Coexists with the live string above: a set must drop it.
			{Target: commit, Key: ForwardKey},
			// On a key nothing in the batch touches: it must survive.
			{Target: projectTarget, Key: AliasRecordKey("z.go#function#Z")},
		},
	}

	muts := []gitmeta.Mutation{
		{Op: gitmeta.OpSetString, Target: commit, Key: ForwardKey, Value: "{}"},
		{Op: gitmeta.OpListPush, Target: projectTarget, Key: aKey, Value: "sha1", NowMS: 10},
		{Op: gitmeta.OpListPush, Target: projectTarget, Key: aKey, Value: "sha2", NowMS: 5},
		{Op: gitmeta.OpListPush, Target: projectTarget, Key: bKey, Value: "sha1", NowMS: 11},
		{Op: gitmeta.OpSetString, Target: projectTarget, Key: AliasRecordKey("a.go#function#Old"), Value: "a.go#function#A"},
		{Op: gitmeta.OpSetString, Target: projectTarget, Key: windowKey, Value: EncodeWindow(IndexWindow{Floor: "sha1", Tip: "sha2"})},
		{Op: gitmeta.OpSetString, Target: projectTarget, Key: windowKey, Value: EncodeWindow(IndexWindow{Floor: "sha1", Tip: "sha3"})},
	}

	reference := seed
	for _, m := range muts {
		reference = reference.Apply(m)
	}
	batched := applyBatch(seed, muts)
	if !reflect.DeepEqual(normalizeState(reference), normalizeState(batched)) {
		t.Fatalf("batched applier diverged from the reference:\nreference=%+v\nbatched=%+v", normalizeState(reference), normalizeState(batched))
	}
	// Guard the guard: the seed must actually reach the update paths.
	if len(batched.Strings) == 0 || len(batched.Lists) == 0 {
		t.Fatalf("degenerate result state: %+v", batched)
	}
}

// normalizeState orders a State's records so two semantically identical states
// compare equal regardless of insertion order.
func normalizeState(st gitmeta.State) gitmeta.State {
	out := gitmeta.State{
		Strings:    append([]gitmeta.StringVal(nil), st.Strings...),
		Lists:      append([]gitmeta.ListVal(nil), st.Lists...),
		Sets:       append([]gitmeta.SetVal(nil), st.Sets...),
		Tombstones: append([]gitmeta.Tombstone(nil), st.Tombstones...),
	}
	sortBy(out.Strings, func(v gitmeta.StringVal) string { return v.Target.String() + "|" + v.Key })
	sortBy(out.Lists, func(v gitmeta.ListVal) string { return v.Target.String() + "|" + v.Key })
	sortBy(out.Sets, func(v gitmeta.SetVal) string { return v.Target.String() + "|" + v.Key })
	return out
}

func sortBy[T any](values []T, key func(T) string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && key(values[j]) < key(values[j-1]); j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
