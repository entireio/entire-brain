package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ashtom/entire-brain/internal/entityindex"
	"github.com/ashtom/entire-brain/internal/factgitmeta"
)

// entityIndexFixture wires a fake repository whose git and `entire graph diff`
// responses are scripted, so the whole CLI surface is exercised without a real
// checkout or a provider on PATH.
type entityIndexFixture struct {
	opts    Options
	runner  *fakeCommandRunner
	repoDir string
	storage repoStorage
}

// countEntityRunnerCalls counts how many times a command with the given
// name+leading args was invoked on a fake runner.
func countEntityRunnerCalls(runner *fakeCommandRunner, name string, leading ...string) int {
	count := 0
	for _, call := range runner.calls {
		if call.name != name || len(call.args) < len(leading) {
			continue
		}
		match := true
		for i, want := range leading {
			if call.args[i] != want {
				match = false
				break
			}
		}
		if match {
			count++
		}
	}
	return count
}

const (
	entityFixtureCommitA = "aaaa111111111111111111111111111111111111"
	entityFixtureCommitB = "bbbb222222222222222222222222222222222222"
	entityFixtureSession = "session-abc"
	entityFixtureCkptA   = "0123456789ab"
	entityFixtureCkptB   = "cba987654321"
)

// entityFixtureCommits is the fixture history, oldest first. Commit A adds two
// functions and commit B touches only one of them, so a locus can name work
// that happened at a MID-SESSION checkpoint rather than at the session's last.
var entityFixtureCommits = []struct {
	sha, parent, at, checkpoint, subject string
}{
	{entityFixtureCommitA, "", "2026-07-01T10:00:00Z", entityFixtureCkptA, "first turn"},
	{entityFixtureCommitB, entityFixtureCommitA, "2026-07-02T10:00:00Z", entityFixtureCkptB, "second turn"},
}

func entityFixtureMessage(i int) string {
	c := entityFixtureCommits[i]
	return c.subject + "\n\nEntire-Checkpoint: " + c.checkpoint + "\n"
}

func newEntityIndexFixture(t *testing.T) *entityIndexFixture {
	t.Helper()
	repoDir := t.TempDir()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "rev-parse", "--show-toplevel"):           {stdout: repoDir + "\n"},
		fakeCommandKey("git", "remote", "get-url", "origin"):            {stdout: "git@github.com:example/entities.git\n"},
		fakeCommandKey("git", "branch", "--show-current"):               {stdout: "main\n"},
		fakeCommandKey("git", "rev-parse", "--verify", "main^{commit}"): {stdout: entityFixtureCommitB + "\n"},
	}}
	// A real first-parent walker: the builder chooses its own ranges and bounds
	// from its cursors, so the fixture answers whatever range it asks for.
	runner.fallback = entityFixtureGitFallback

	runner.responses[fakeCommandKey("entire", "graph", "diff", "--repo", repoDir, "--base", entityindex.EmptyTreeSHA, "--head", entityFixtureCommitA, "--json")] = fakeCommandResponse{
		stdout: `{"base":"` + entityindex.EmptyTreeSHA + `","head":"` + entityFixtureCommitA + `","files":[{"path":"internal/pay/charge.go","status":"A","changes":[
		  {"type":"added","kind":"function","name":"ChargeCard","new_signature":"func ChargeCard(id string) error","after_start_line":10},
		  {"type":"added","kind":"function","name":"AuthorizeCard","new_signature":"func AuthorizeCard(id string) error","after_start_line":40}]}]}`,
	}
	runner.responses[fakeCommandKey("entire", "graph", "diff", "--repo", repoDir, "--base", entityFixtureCommitA, "--head", entityFixtureCommitB, "--json")] = fakeCommandResponse{
		stdout: `{"base":"` + entityFixtureCommitA + `","head":"` + entityFixtureCommitB + `","files":[{"path":"internal/pay/charge.go","status":"M","changes":[
		  {"type":"body_changed","kind":"function","name":"ChargeCard","before_start_line":10,"after_start_line":12},
		  {"type":"added","kind":"function","name":"RefundCard","new_signature":"func RefundCard(id string) error","after_start_line":30}]}]}`,
	}

	// Commit decoration for the derived cache (subject, committer date, trailers).
	runner.responses[fakeCommandKey("git", "log", "--no-walk", entityCommitLogFormat, entityFixtureCommitA, entityFixtureCommitB)] = fakeCommandResponse{
		stdout: entityFixtureLogRecord(0) + entityFixtureLogRecord(1),
	}

	opts := Options{
		Version: "test",
		Env: EntireEnv{
			RepoRoot:        repoDir,
			PluginConfigDir: t.TempDir(),
			PluginDataDir:   t.TempDir(),
			PluginStateDir:  t.TempDir(),
			PluginCacheDir:  t.TempDir(),
		},
		Runner: runner,
		Now:    func() time.Time { return now },
	}
	storage, err := repoStoragePaths(context.Background(), runner, opts.Env, repoDir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	if err := os.MkdirAll(storage.BrainDir, 0o755); err != nil {
		t.Fatalf("mkdir brain: %v", err)
	}
	return &entityIndexFixture{opts: opts, runner: runner, repoDir: repoDir, storage: storage}
}

// entityFixtureLogRecord renders one decoration record for `git log --no-walk`.
func entityFixtureLogRecord(i int) string {
	c := entityFixtureCommits[i]
	return c.sha + "\x00" + c.at + "\x00" + c.subject + "\x00" + entityFixtureMessage(i) + "\x1e\n"
}

// entityFixtureGitFallback answers the builder's first-parent walks and its
// ancestry probe over the fixture history.
func entityFixtureGitFallback(name string, args []string) (fakeCommandResponse, bool) {
	if name != "git" || len(args) == 0 {
		return fakeCommandResponse{}, false
	}
	index := map[string]int{}
	for i, c := range entityFixtureCommits {
		index[c.sha] = i
	}
	switch {
	case args[0] == "merge-base" && len(args) == 4 && args[1] == "--is-ancestor":
		ancestor, okA := index[args[2]]
		descendant, okB := index[args[3]]
		if !okA || !okB {
			return fakeCommandResponse{err: errors.New("unknown revision")}, true
		}
		if ancestor <= descendant {
			return fakeCommandResponse{}, true
		}
		return fakeCommandResponse{err: errors.New("not an ancestor")}, true
	case args[0] == "log" && len(args) == 5 && args[1] == "--first-parent":
		max, err := strconv.Atoi(strings.TrimPrefix(args[3], "--max-count="))
		if err != nil {
			return fakeCommandResponse{}, false
		}
		from, to := 0, 0
		if lo, hi, isRange := strings.Cut(args[4], ".."); isRange {
			loIdx, okLo := index[lo]
			hiIdx, okHi := index[hi]
			if !okLo || !okHi {
				return fakeCommandResponse{err: errors.New("unknown revision")}, true
			}
			from, to = loIdx+1, hiIdx
		} else {
			hiIdx, ok := index[args[4]]
			if !ok {
				return fakeCommandResponse{err: errors.New("unknown revision")}, true
			}
			from, to = 0, hiIdx
		}
		var b strings.Builder
		for i, n := to, 0; i >= from && n < max; i, n = i-1, n+1 {
			c := entityFixtureCommits[i]
			b.WriteString(c.sha + "\x00" + c.parent + "\x00" + c.at + "\x00" + entityFixtureMessage(i) + "\x1e\n")
		}
		return fakeCommandResponse{stdout: b.String()}, true
	}
	return fakeCommandResponse{}, false
}

// writeSessionManifest gives the derived cache a checkpoint -> session join.
func (f *entityIndexFixture) writeSessionManifest(t *testing.T) {
	t.Helper()
	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		GeneratedAt:   time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC),
		DefaultBranch: "main",
		Sessions: []exportSession{{
			SessionID:        entityFixtureSession,
			Branch:           "main",
			LatestCheckpoint: entityFixtureCkptB,
			CreatedAt:        time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC),
			TranscriptPath:   "sessions/s.jsonl",
		}},
	}
	if err := writeJSONFile(filepath.Join(f.storage.BrainDir, exportManifestFileName), manifest); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

// writeLocalCheckpoints scripts a local git-branch checkpoint store holding
// BOTH of the session's checkpoints. The manifest only ever names a session's
// latest one, so without this a mid-session checkpoint belongs to no session at
// all and provenance could never be sharpened off the last checkpoint.
func (f *entityIndexFixture) writeLocalCheckpoints(t *testing.T) {
	t.Helper()
	var paths []string
	for i, c := range entityFixtureCommits {
		dir := checkpointPath(c.checkpoint)
		root := dir + "/metadata.json"
		session := dir + "/0/metadata.json"
		transcript := dir + "/0/" + v1TranscriptFileName
		paths = append(paths, root, session, transcript)
		f.runner.responses[fakeCommandKey("git", "cat-file", "-p", v1MainRef+":"+root)] = fakeCommandResponse{
			stdout: `{"branch":"main","sessions":[{"metadata":"` + session + `","transcript":"` + transcript + `"}]}`,
		}
		f.runner.responses[fakeCommandKey("git", "cat-file", "-p", v1MainRef+":"+session)] = fakeCommandResponse{
			stdout: `{"checkpoint_id":"` + c.checkpoint + `","session_id":"` + entityFixtureSession + `","branch":"main","created_at":"` + c.at + `"}`,
		}
		f.runner.responses[fakeCommandKey("git", "cat-file", "-p", v1MainRef+":"+transcript)] = fakeCommandResponse{
			stdout: `{"type":"user_message","message":"turn ` + strconv.Itoa(i) + `"}` + "\n",
		}
	}
	f.runner.responses[fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1MainRef)] = fakeCommandResponse{
		stdout: strings.Join(paths, "\n") + "\n",
	}
}

func (f *entityIndexFixture) run(t *testing.T, args ...string) (string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := NewRootCommand(f.opts)
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("%v: %v\nstdout:\n%s\nstderr:\n%s", args, err, stdout.String(), stderr.String())
	}
	return stdout.String(), stderr.String()
}

// TestEntitiesBackfillThenHistoryAnswersFromTheIndex is the end-to-end
// contract: backfill persists the index, and history answers which checkpoints
// and sessions changed a symbol without re-invoking the provider.
func TestEntitiesBackfillThenHistoryAnswersFromTheIndex(t *testing.T) {
	fixture := newEntityIndexFixture(t)
	fixture.writeSessionManifest(t)

	stdout, _ := fixture.run(t, "entities", "backfill", "--json")
	var build entityindex.BuildResult
	if err := json.Unmarshal([]byte(stdout), &build); err != nil {
		t.Fatalf("decode backfill result: %v\n%s", err, stdout)
	}
	if build.Indexed != 2 || build.Entities != 4 {
		t.Fatalf("backfill indexed=%d entities=%d, want 2/4 (%+v)", build.Indexed, build.Entities, build)
	}

	diffCallsAfterBackfill := countEntityRunnerCalls(fixture.runner, "entire", "graph", "diff")

	stdout, _ = fixture.run(t, "entities", "history", "ChargeCard", "--json")
	var result entityHistoryResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("decode history: %v\n%s", err, stdout)
	}
	if len(result.Matches) != 1 {
		t.Fatalf("history matches = %d, want 1: %+v", len(result.Matches), result.Matches)
	}
	match := result.Matches[0]
	if match.EntityKey != "internal/pay/charge.go#function#ChargeCard" {
		t.Fatalf("entity key = %q", match.EntityKey)
	}
	if len(match.Occurrences) != 2 {
		t.Fatalf("occurrences = %+v, want both commits", match.Occurrences)
	}
	if match.Occurrences[0].Commit != entityFixtureCommitA || match.Occurrences[1].Commit != entityFixtureCommitB {
		t.Fatalf("occurrences are not oldest-first: %+v", match.Occurrences)
	}
	if got := match.Occurrences[0].CheckpointIDs; len(got) != 1 || got[0] != entityFixtureCkptA {
		t.Fatalf("first occurrence checkpoints = %v", got)
	}
	if got := match.Occurrences[1].SessionIDs; len(got) != 1 || got[0] != entityFixtureSession {
		t.Fatalf("second occurrence sessions = %v, want the manifest session", got)
	}
	if got := countEntityRunnerCalls(fixture.runner, "entire", "graph", "diff"); got != diffCallsAfterBackfill {
		t.Fatalf("history re-invoked the provider %d time(s); it must answer from the persisted index", got-diffCallsAfterBackfill)
	}

	// A second symbol changed only by the second commit resolves to just it.
	stdout, _ = fixture.run(t, "entities", "history", "RefundCard", "--json")
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("decode history: %v", err)
	}
	if len(result.Matches) != 1 || len(result.Matches[0].Occurrences) != 1 || result.Matches[0].Occurrences[0].Commit != entityFixtureCommitB {
		t.Fatalf("RefundCard history = %+v", result.Matches)
	}
}

// TestEntitiesHistoryAnswersOldestFirstAcrossBoundedPasses pins the answer's
// order against the way the index is actually built. Bounded passes converge
// BACKWARDS, and the durable reverse list preserves append order, so the newest
// commit is appended first — reporting the stored order would answer
// newest-first on every chunk-backfilled repository.
func TestEntitiesHistoryAnswersOldestFirstAcrossBoundedPasses(t *testing.T) {
	fixture := newEntityIndexFixture(t)
	fixture.writeSessionManifest(t)

	// One commit per pass: the newest lands in the index first.
	fixture.run(t, "entities", "backfill", "--limit", "1", "--json")
	fixture.run(t, "entities", "backfill", "--limit", "1", "--json")

	stdout, _ := fixture.run(t, "entities", "history", "ChargeCard", "--json")
	var result entityHistoryResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout)
	}
	if len(result.Matches) != 1 || len(result.Matches[0].Occurrences) != 2 {
		t.Fatalf("matches = %+v", result.Matches)
	}
	got := result.Matches[0].Occurrences
	if got[0].Commit != entityFixtureCommitA || got[1].Commit != entityFixtureCommitB {
		t.Fatalf("occurrences are not oldest-first after backwards convergence: %+v", got)
	}
}

// TestEntityIndexCacheRebuildsWhenTheGitMetaTipMoves locks the invalidation
// rule: the derived cache is pinned to the git-meta ref, so any index write
// forces a rebuild rather than serving a stale answer.
func TestEntityIndexCacheRebuildsWhenTheGitMetaTipMoves(t *testing.T) {
	fixture := newEntityIndexFixture(t)
	fixture.run(t, "entities", "backfill", "--json")

	cachePath := filepath.Join(fixture.storage.BrainDir, filepath.FromSlash(entitiesCachePath))
	cache, ok := loadEntityIndexCache(fixture.storage.BrainDir)
	if !ok {
		t.Fatal("backfill did not persist the derived cache")
	}
	if cache.MetaTip == "" {
		t.Fatal("cache is not pinned to a git-meta tip, so it can never be invalidated")
	}
	if _, present := cache.Entries["internal/pay/charge.go#function#ChargeCard"]; !present {
		t.Fatalf("cache entries = %v", cache.Entries)
	}

	// Poison the cache with a bogus entry pinned to a stale tip. A correct
	// implementation must throw it away and rebuild from git-meta.
	cache.MetaTip = "0000000000000000000000000000000000000000"
	cache.Entries = map[string][]entityIndexOccurrence{"phantom.go#function#Phantom": {{Commit: "deadbeef"}}}
	data, err := json.Marshal(cache)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(cachePath, data, 0o600); err != nil {
		t.Fatalf("write poisoned cache: %v", err)
	}

	stdout, _ := fixture.run(t, "entities", "history", "Phantom", "--json")
	var result entityHistoryResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(result.Matches) != 0 {
		t.Fatalf("stale cache was served: %+v", result.Matches)
	}
	stdout, _ = fixture.run(t, "entities", "history", "ChargeCard", "--json")
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(result.Matches) != 1 {
		t.Fatalf("rebuilt cache did not recover the real index: %+v", result)
	}
	rebuilt, ok := loadEntityIndexCache(fixture.storage.BrainDir)
	if !ok || rebuilt.MetaTip == "0000000000000000000000000000000000000000" {
		t.Fatalf("rebuilt cache was not re-pinned: ok=%v tip=%q", ok, rebuilt.MetaTip)
	}
}

// TestEntitiesShowPrintsTheStoredDeltaDocument locks the frozen wire contract:
// `show` prints exactly the bytes stored in git-meta.
func TestEntitiesShowPrintsTheStoredDeltaDocument(t *testing.T) {
	fixture := newEntityIndexFixture(t)
	fixture.run(t, "entities", "backfill", "--json")
	fixture.runner.responses[fakeCommandKey("git", "rev-parse", "--verify", entityFixtureCommitB+"^{commit}")] = fakeCommandResponse{stdout: entityFixtureCommitB + "\n"}

	stdout, _ := fixture.run(t, "entities", "show", entityFixtureCommitB)
	var delta entityindex.Delta
	if err := json.Unmarshal([]byte(stdout), &delta); err != nil {
		t.Fatalf("decode delta: %v\n%s", err, stdout)
	}
	if delta.SchemaVersion != "1.0" || delta.Producer != "entire-graph" {
		t.Fatalf("delta header = %+v", delta)
	}
	if delta.Base != entityFixtureCommitA || delta.Head != entityFixtureCommitB {
		t.Fatalf("delta base/head = %s/%s", delta.Base, delta.Head)
	}
	if len(delta.Entities) != 2 {
		t.Fatalf("delta entities = %+v", delta.Entities)
	}
	for _, entity := range delta.Entities {
		if entity.Path != "internal/pay/charge.go" || entity.Kind != "function" {
			t.Fatalf("entity = %+v", entity)
		}
	}
}

// TestEntitiesShowResolvesACheckpointID locks the second address `show`
// accepts: the checkpoint id an agent already has in hand, not just a sha.
func TestEntitiesShowResolvesACheckpointID(t *testing.T) {
	fixture := newEntityIndexFixture(t)
	fixture.writeSessionManifest(t)
	fixture.run(t, "entities", "backfill", "--json")

	stdout, _ := fixture.run(t, "entities", "show", entityFixtureCkptB)
	var delta entityindex.Delta
	if err := json.Unmarshal([]byte(stdout), &delta); err != nil {
		t.Fatalf("decode delta: %v\n%s", err, stdout)
	}
	if delta.Head != entityFixtureCommitB {
		t.Fatalf("checkpoint %s resolved to head %s, want %s", entityFixtureCkptB, delta.Head, entityFixtureCommitB)
	}
}

// TestEntitiesHistoryReportsAnEmptyIndex keeps the empty case actionable
// instead of silently indistinguishable from "no such symbol".
func TestEntitiesHistoryReportsAnEmptyIndex(t *testing.T) {
	fixture := newEntityIndexFixture(t)
	stdout, _ := fixture.run(t, "entities", "history", "ChargeCard", "--json")
	var result entityHistoryResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(result.Matches) != 0 || !strings.Contains(result.Note, "backfill") {
		t.Fatalf("empty-index answer = %+v", result)
	}
}

// TestEntitiesFreshnessStepIndexesNewCommits locks the deterministic,
// zero-token freshness path the watch tick and the session-end hook call.
func TestEntitiesFreshnessStepIndexesNewCommits(t *testing.T) {
	fixture := newEntityIndexFixture(t)
	if err := refreshEntityIndexQuietly(context.Background(), fixture.opts, fixture.repoDir); err != nil {
		t.Fatalf("freshness step: %v", err)
	}
	cache, ok := loadEntityIndexCache(fixture.storage.BrainDir)
	if !ok {
		t.Fatal("freshness step built no index")
	}
	if _, present := cache.Entries["internal/pay/charge.go#function#ChargeCard"]; !present {
		t.Fatalf("freshness step entries = %v", cache.Entries)
	}
	// Idempotent: a second pass with nothing new must not fail or re-diff.
	before := countEntityRunnerCalls(fixture.runner, "entire", "graph", "diff")
	if err := refreshEntityIndexQuietly(context.Background(), fixture.opts, fixture.repoDir); err != nil {
		t.Fatalf("second freshness step: %v", err)
	}
	if after := countEntityRunnerCalls(fixture.runner, "entire", "graph", "diff"); after != before {
		t.Fatalf("second freshness pass invoked the provider %d extra time(s)", after-before)
	}
}

// TestEntityProvenanceResolvesToAMidSessionCheckpoint is the provenance upgrade
// itself, and it only proves anything because the resolved checkpoint is NOT
// the session's latest: ckptA is the session's FIRST checkpoint and ckptB is
// the one distill would otherwise have anchored to. A fixture that resolved to
// the latest checkpoint would pass with the whole feature deleted.
func TestEntityProvenanceResolvesToAMidSessionCheckpoint(t *testing.T) {
	fixture := newEntityIndexFixture(t)
	fixture.writeSessionManifest(t)
	fixture.writeLocalCheckpoints(t)
	fixture.run(t, "entities", "backfill", "--json")

	resolver := newEntityProvenanceResolver(context.Background(), fixture.opts, fixture.repoDir)
	if resolver == nil {
		t.Fatal("resolver is nil despite a populated index")
	}
	// The session's LAST checkpoint, which is exactly what distill anchors to
	// without the index.
	fallback := factAnchor{SessionID: entityFixtureSession, CheckpointID: entityFixtureCkptB, Commit: entityFixtureCommitB}
	records := []factRecord{{
		Text:       "`AuthorizeCard` must run before a charge is captured.",
		Branch:     "main",
		Locus:      []string{"authorizecard"},
		Provenance: []factAnchor{fallback},
	}}
	got := applyEntityProvenance(records, resolver)
	anchor := got[0].Provenance[0]
	if anchor.CheckpointID == entityFixtureCkptB {
		t.Fatal("the anchor stayed on the session's LATEST checkpoint; nothing was sharpened")
	}
	if anchor.CheckpointID != entityFixtureCkptA || anchor.Commit != entityFixtureCommitA {
		t.Fatalf("anchor = %+v, want the checkpoint/commit that added AuthorizeCard (%s/%s)", anchor, entityFixtureCkptA, entityFixtureCommitA)
	}
	if anchor.SessionID != entityFixtureSession {
		t.Fatal("the session id must survive the anchor upgrade")
	}

	// And an entity the LAST checkpoint touched still resolves there.
	records = []factRecord{{
		Text:       "The `RefundCard` helper must never be called twice for one charge.",
		Branch:     "main",
		Locus:      []string{"refundcard"},
		Provenance: []factAnchor{fallback},
	}}
	if got := applyEntityProvenance(records, resolver)[0].Provenance[0]; got.CheckpointID != entityFixtureCkptB || got.Commit != entityFixtureCommitB {
		t.Fatalf("RefundCard anchor = %+v", got)
	}
}

// TestEntityProvenanceIsDeterministicOnTimestampTies pins the resolution
// ORDER. Committer dates resolve to one second, so two commits made in the same
// second are a routine tie — and the resolver reaches them by ranging over a Go
// map, whose iteration order is randomized per run. Keeping the first-seen
// candidate on a tie therefore made identical inputs produce different anchors
// from one distill to the next. The order has to be total.
func TestEntityProvenanceIsDeterministicOnTimestampTies(t *testing.T) {
	t.Parallel()
	const (
		session = "session-tie"
		lowSHA  = "1111111111111111111111111111111111111111"
		highSHA = "9999999999999999999999999999999999999999"
	)
	tie := time.Date(2026, 7, 4, 9, 30, 15, 0, time.UTC)
	// Two commits in the SAME SECOND, of the same session, each changing an
	// entity that the single locus token "chargecard" names.
	occurrences := map[string]entityIndexOccurrence{
		"internal/pay/a.go#function#ChargeCard": {Commit: lowSHA, CheckpointIDs: []string{"aaaaaaaaaaaa"}, SessionIDs: []string{session}, CommittedAt: &tie},
		"internal/pay/b.go#function#ChargeCard": {Commit: highSHA, CheckpointIDs: []string{"zzzzzzzzzzzz"}, SessionIDs: []string{session}, CommittedAt: &tie},
	}
	keys := []string{"internal/pay/a.go#function#ChargeCard", "internal/pay/b.go#function#ChargeCard"}
	record := factRecord{
		Text:       "`ChargeCard` must be idempotent.",
		Branch:     "main",
		Locus:      []string{"chargecard"},
		Provenance: []factAnchor{{SessionID: session, CheckpointID: "cccccccccccc", Commit: "ffffffffffffffffffffffffffffffffffffffff"}},
	}

	var first factAnchor
	for round := 0; round < 64; round++ {
		// A FRESH map every round, with the entries inserted in alternating
		// order, so neither insertion order nor Go's randomized range order is
		// held constant across rounds.
		cache := entityIndexCache{SchemaVersion: entityIndexCacheVersion, Entries: map[string][]entityIndexOccurrence{}}
		order := keys
		if round%2 == 1 {
			order = []string{keys[1], keys[0]}
		}
		for _, key := range order {
			cache.Entries[key] = []entityIndexOccurrence{occurrences[key]}
		}
		resolver := buildEntityProvenanceResolver(cache, map[string]string{session: "main"}, nil)
		if resolver == nil {
			t.Fatal("resolver is nil despite a populated cache")
		}
		got := applyEntityProvenance([]factRecord{record}, resolver)[0].Provenance[0]
		if round == 0 {
			first = got
			continue
		}
		if got != first {
			t.Fatalf("round %d resolved %+v, round 0 resolved %+v: the same input produced two different anchors", round, got, first)
		}
	}
	// And the tie-break is the documented one, not just "stable by accident".
	if first.Commit != highSHA || first.CheckpointID != "zzzzzzzzzzzz" {
		t.Fatalf("tie resolved to %+v, want the larger commit sha (%s)", first, highSHA)
	}
}

// TestEntityProvenanceRefusesAnotherSessionsCheckpoint pins the scoping rule.
// The index is repo-wide, so an unscoped "newest checkpoint that touched this
// entity" pairs the fact's session id with a DIFFERENT session's commit and
// checkpoint — a provenance triple that never existed anywhere.
func TestEntityProvenanceRefusesAnotherSessionsCheckpoint(t *testing.T) {
	fixture := newEntityIndexFixture(t)
	fixture.writeSessionManifest(t)
	fixture.writeLocalCheckpoints(t)
	fixture.run(t, "entities", "backfill", "--json")

	resolver := newEntityProvenanceResolver(context.Background(), fixture.opts, fixture.repoDir)
	if resolver == nil {
		t.Fatal("resolver is nil despite a populated index")
	}
	// Everything indexed belongs to entityFixtureSession; this fact does not.
	fallback := factAnchor{SessionID: "some-other-session", CheckpointID: "zzzzzzzzzzzz", Commit: "ffffffffffffffffffffffffffffffffffffffff"}
	records := []factRecord{{
		Text:       "`AuthorizeCard` must run before a charge is captured.",
		Branch:     "main",
		Locus:      []string{"authorizecard"},
		Provenance: []factAnchor{fallback},
	}}
	if got := applyEntityProvenance(records, resolver)[0].Provenance[0]; got != fallback {
		t.Fatalf("a fact was anchored to another session's checkpoint: %+v", got)
	}

	// Same session id, but the fact belongs to a branch the session was not
	// exported on: still no sharpening.
	crossBranch := factAnchor{SessionID: entityFixtureSession, CheckpointID: entityFixtureCkptB, Commit: entityFixtureCommitB}
	records = []factRecord{{
		Text:       "`AuthorizeCard` must run before a charge is captured.",
		Branch:     "release",
		Locus:      []string{"authorizecard"},
		Provenance: []factAnchor{crossBranch},
	}}
	if got := applyEntityProvenance(records, resolver)[0].Provenance[0]; got != crossBranch {
		t.Fatalf("a fact on another branch was sharpened: %+v", got)
	}
}

// TestEntityIndexCacheIsNotPersistedWhenGitFails pins the cache's failure mode.
// The join swallowed git errors, wrote the resulting EMPTY join pinned to the
// current git-meta tip, and then served it forever: one transient `git log`
// failure turned every "which checkpoints changed X" answer into "none", with
// no tip movement to ever invalidate it.
func TestEntityIndexCacheIsNotPersistedWhenGitFails(t *testing.T) {
	fixture := newEntityIndexFixture(t)
	fixture.writeSessionManifest(t)

	// Fail the decoration read exactly once; the fixture's normal response
	// serves every call after it.
	decorate := fakeCommandKey("git", "log", "--no-walk", entityCommitLogFormat, entityFixtureCommitA, entityFixtureCommitB)
	fixture.runner.sequences = map[string][]fakeCommandResponse{
		decorate: {{err: errors.New("git exploded")}},
	}

	var stdout, stderr bytes.Buffer
	cmd := NewRootCommand(fixture.opts)
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"entities", "backfill", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("backfill: %v\n%s", err, stderr.String())
	}
	if cache, ok := loadEntityIndexCache(fixture.storage.BrainDir); ok && len(cache.Entries) == 0 {
		t.Fatalf("an empty join was persisted and pinned to tip %q", cache.MetaTip)
	}

	// The next call must rebuild and answer for real.
	out, _ := fixture.run(t, "entities", "history", "ChargeCard", "--json")
	var result entityHistoryResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if len(result.Matches) != 1 || len(result.Matches[0].Occurrences) != 2 {
		t.Fatalf("the recovered answer is still poisoned: %+v", result.Matches)
	}
	if got := result.Matches[0].Occurrences[0]; len(got.CheckpointIDs) != 1 || got.CheckpointIDs[0] != entityFixtureCkptA {
		t.Fatalf("occurrence decoration was not recovered: %+v", got)
	}
	rebuilt, ok := loadEntityIndexCache(fixture.storage.BrainDir)
	if !ok || len(rebuilt.Entries) == 0 {
		t.Fatalf("the recovered join was not persisted: ok=%v entries=%d", ok, len(rebuilt.Entries))
	}
}

// TestEntityProvenanceFallsBackWhenUnresolvable pins the non-breaking half of
// the contract: no index, or no match, keeps distill's original anchor exactly.
func TestEntityProvenanceFallsBackWhenUnresolvable(t *testing.T) {
	fixture := newEntityIndexFixture(t)
	fixture.writeSessionManifest(t)

	// No index at all.
	if resolver := newEntityProvenanceResolver(context.Background(), fixture.opts, fixture.repoDir); resolver != nil {
		t.Fatal("resolver must be nil when the index is empty")
	}
	fallback := factAnchor{SessionID: entityFixtureSession, CheckpointID: "zzzzzzzzzzzz"}
	records := []factRecord{{Text: "unrelated", Locus: []string{"nothinghere"}, Provenance: []factAnchor{fallback}}}
	if got := applyEntityProvenance(records, nil); got[0].Provenance[0] != fallback {
		t.Fatalf("a nil resolver changed the anchor: %+v", got[0].Provenance[0])
	}

	// Index present, but the fact's locus names nothing indexed.
	fixture.run(t, "entities", "backfill", "--json")
	resolver := newEntityProvenanceResolver(context.Background(), fixture.opts, fixture.repoDir)
	if resolver == nil {
		t.Fatal("resolver is nil despite a populated index")
	}
	records = []factRecord{{Text: "unrelated", Locus: []string{"nothinghere"}, Provenance: []factAnchor{fallback}}}
	if got := applyEntityProvenance(records, resolver); got[0].Provenance[0] != fallback {
		t.Fatalf("an unresolvable locus changed the anchor: %+v", got[0].Provenance[0])
	}
}

// TestEntityIndexSharesTheFactSyncGitMetaStore proves the index and the
// fact-set head live on the same local ref under the same lock, rather than in
// two unsynchronized stores over one git dir.
func TestEntityIndexSharesTheFactSyncGitMetaStore(t *testing.T) {
	fixture := newEntityIndexFixture(t)
	fixture.run(t, "entities", "backfill", "--json")

	gitDir, err := gitmetaDirForKey(fixture.opts.Env, fixture.storage.Key)
	if err != nil {
		t.Fatalf("gitmeta dir: %v", err)
	}
	backend, err := factgitmeta.NewLocalBackend(gitDir, fixture.storage.Key, fixture.opts.Now)
	if err != nil {
		t.Fatalf("open fact backend: %v", err)
	}
	if _, err := backend.Advance(context.Background(), fixture.storage.Key, "main", "", []byte("{\"id\":\"x\"}\n")); err != nil {
		t.Fatalf("fact advance over the shared store: %v", err)
	}
	// The fact write moved the shared ref; the entity index must still read
	// back intact (a separate ref or a clobbering write would lose it).
	store, err := factgitmeta.OpenMetaStore(gitDir, fixture.opts.Now)
	if err != nil {
		t.Fatalf("open meta store: %v", err)
	}
	state, err := store.State()
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	snap := entityindex.Load(state)
	if got := snap.Commits("internal/pay/charge.go#function#ChargeCard"); len(got) != 2 {
		t.Fatalf("entity index lost records across a fact-set write: %v", got)
	}
	if _, _, found, err := backend.Current(context.Background(), fixture.storage.Key, "main"); err != nil || !found {
		t.Fatalf("fact head lost across the shared store: found=%v err=%v", found, err)
	}
}

// TestBrainEntityHistoryMCPToolIsExposedAndAnswers locks the agent surface:
// the tool is declared, its arguments validate, and it answers from the same
// query path as the CLI.
func TestBrainEntityHistoryMCPToolIsExposedAndAnswers(t *testing.T) {
	fixture := newEntityIndexFixture(t)
	fixture.writeSessionManifest(t)
	fixture.run(t, "entities", "backfill", "--json")

	declared := false
	for _, tool := range mcpToolDefinitions() {
		if tool["name"] == "brain_entity_history" {
			declared = true
			if _, ok := tool["inputSchema"].(map[string]any); !ok {
				t.Fatal("brain_entity_history has no input schema")
			}
		}
	}
	if !declared {
		t.Fatal("brain_entity_history is not in the MCP tool surface")
	}
	if err := validateMCPToolArguments("brain_entity_history", map[string]any{"query": "ChargeCard", "limit": 5, "branch": "main"}); err != nil {
		t.Fatalf("valid arguments rejected: %v", err)
	}
	if err := validateMCPToolArguments("brain_entity_history", map[string]any{"query": "x", "bogus": 1}); err == nil {
		t.Fatal("an unknown argument was accepted")
	}

	raw, err := json.Marshal(map[string]any{"name": "brain_entity_history", "arguments": map[string]any{"query": "ChargeCard"}})
	if err != nil {
		t.Fatalf("marshal call: %v", err)
	}
	response, err := handleMCPToolCall(context.Background(), fixture.opts, raw)
	if err != nil {
		t.Fatalf("tool call: %v", err)
	}
	content, _ := response["content"].([]map[string]any)
	if len(content) == 0 {
		t.Fatalf("empty tool response: %+v", response)
	}
	text, _ := content[0]["text"].(string)
	var result entityHistoryResult
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		t.Fatalf("tool output is not the history contract: %v\n%s", err, text)
	}
	if len(result.Matches) != 1 || result.Matches[0].Name != "ChargeCard" {
		t.Fatalf("tool matches = %+v", result.Matches)
	}
	if len(result.Matches[0].Occurrences) != 2 {
		t.Fatalf("tool occurrences = %+v", result.Matches[0].Occurrences)
	}
}
