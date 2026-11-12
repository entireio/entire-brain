package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
	"github.com/entireio/entire-brain/internal/factsync"
)

func coverageRecord(id string, sessionIDs ...string) factmerge.Record {
	anchors := make([]factmerge.Anchor, 0, len(sessionIDs))
	for _, s := range sessionIDs {
		anchors = append(anchors, factmerge.Anchor{SessionID: s, CheckpointID: "cp-" + s})
	}
	return factmerge.Record{
		ID:         id,
		Paths:      []string{"project.decisions.cli"},
		Text:       "fact " + id,
		Branch:     "main",
		Origin:     "distilled",
		Status:     "active",
		Provenance: anchors,
		CreatedAt:  time.Unix(1700000000, 0).UTC(),
		UpdatedAt:  time.Unix(1700000000, 0).UTC(),
	}
}

func TestCoveredSessionIDsReadsLocalFactProvenance(t *testing.T) {
	t.Parallel()
	brainDir := t.TempDir()
	records := []factmerge.Record{
		coverageRecord("fact:aaaaaaaaaaaaaaaaaaaaaaaa", "sess-1", "sess-2"),
		coverageRecord("fact:bbbbbbbbbbbbbbbbbbbbbbbb", "sess-3"),
	}
	if err := factstore.Write(brainDir, records); err != nil {
		t.Fatal(err)
	}

	covered := coveredSessionIDs(brainDir, "")

	for _, want := range []string{"sess-1", "sess-2", "sess-3"} {
		if !covered[coveredSessionKey(want, "cp-"+want)] {
			t.Errorf("session %s must be covered at its cited checkpoint", want)
		}
		if covered[coveredSessionKey(want, "cp-later")] {
			t.Errorf("session %s must NOT be covered at a checkpoint no fact cites", want)
		}
	}
	if covered[coveredSessionKey("", "")] {
		t.Error("the empty session id must never be covered")
	}
}

// The pull filter can drop a teammate's fact from the local store (unmerged
// branch, squash merge) while the feed cache still holds it. That session was
// still distilled by the team and must count as covered.
func TestCoveredSessionIDsIncludesFeedCacheOnlySessions(t *testing.T) {
	t.Parallel()
	brainDir := t.TempDir()
	feedDir := t.TempDir()
	items := map[string]factsync.FeedItem{
		"fact:cccccccccccccccccccccccc": {Record: coverageRecord("fact:cccccccccccccccccccccccc", "sess-filtered")},
	}
	if err := writeHostedFeedCache(feedDir, hostedFeedState{RepoID: "r", BaseURL: "u"}, items); err != nil {
		t.Fatal(err)
	}

	covered := coveredSessionIDs(brainDir, feedDir)

	if !covered[coveredSessionKey("sess-filtered", "cp-sess-filtered")] {
		t.Error("a session cited only by the hosted feed cache must count as covered")
	}
}

func TestCoveredSessionIDsToleratesMissingStores(t *testing.T) {
	t.Parallel()
	covered := coveredSessionIDs(t.TempDir(), "")
	if len(covered) != 0 {
		t.Errorf("empty brain and no feed dir must cover nothing, got %v", covered)
	}
}

func TestBackfillStatusCountsTeammateCoveredSessions(t *testing.T) {
	f := newSetupTestFixture(t, "s1", "s2", "s3")
	f.markDistilled(t, "s1")
	// s1 is distilled AND covered: the distill cache wins, so Covered counts
	// only sessions whose skip comes from teammate facts.
	status := factsBackfillStatusForBrain(f.storage.BrainDir, map[string]bool{
		coveredSessionKey("s1", "cp-s1"): true,
		coveredSessionKey("s2", "cp-s2"): true,
	})
	if status.Sessions != 3 || status.Distilled != 1 || status.Covered != 1 || status.Pending() != 1 {
		t.Fatalf("expected 3 sessions / 1 distilled / 1 covered / 1 pending, got %+v", status)
	}
}

func TestDistillPlanSkipsCoveredUncachedSessions(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	brainDir := writeOrderedDistillFixture(t, now, "old", "mid", "new")
	manifest, err := bmanifest.Load(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	opts := distillCommandOptions{covered: map[string]bool{coveredSessionKey("mid", "cp-mid"): true}}
	plan, err := buildDistillPlan(brainDir, manifest, opts, "salt")
	if err != nil {
		t.Fatal(err)
	}
	if plan.SessionsToDistill != 2 || plan.CoveredSessions != 1 {
		t.Fatalf("expected 2 to distill + 1 covered, got %d + %d", plan.SessionsToDistill, plan.CoveredSessions)
	}
	for _, s := range plan.Sessions {
		if s.Session.SessionID == "mid" && (!s.Covered || len(s.ChunkIndexes) != 0) {
			t.Fatalf("mid must be marked covered with no work, got %+v", s)
		}
	}
}

// Coverage must never suppress incremental re-distillation: a session WITH a
// cache entry whose fingerprint changed (the transcript grew) is re-planned
// even when its facts make it look covered.
func TestDistillPlanStillRedispatchesGrownCachedSessions(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	brainDir := writeOrderedDistillFixture(t, now, "old")
	cache := distillCache{Version: distillCacheVersion, Sessions: map[string]string{
		distillSessionCacheKey("main", "old"): "stale-fingerprint",
	}}
	if err := saveDistillCache(brainDir, cache); err != nil {
		t.Fatal(err)
	}
	manifest, err := bmanifest.Load(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	opts := distillCommandOptions{covered: map[string]bool{coveredSessionKey("old", "cp-old"): true}}
	plan, err := buildDistillPlan(brainDir, manifest, opts, "salt")
	if err != nil {
		t.Fatal(err)
	}
	if plan.SessionsToDistill != 1 || plan.CoveredSessions != 0 {
		t.Fatalf("a grown cached session must be re-planned, got distill=%d covered=%d", plan.SessionsToDistill, plan.CoveredSessions)
	}
}

// The REAL distill run must honor coverage, not just the --dry-run plan: the
// consent prompt's "distill the remaining K?" is priced on it.
func TestRunDistillSkipsCoveredSessions(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	brainDir := writeOrderedDistillFixture(t, now, "old", "new")
	var visited []string
	opts := distillCommandOptions{
		agent:         "command",
		agentCommand:  []string{"fake"},
		run:           recordingDistillRunner(&visited),
		maxChunkBytes: defaultDistillChunkSize,
		timeout:       time.Minute,
		concurrency:   1,
		covered:       map[string]bool{coveredSessionKey("old", "cp-old"): true},
	}
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now); err != nil {
		t.Fatal(err)
	}
	for _, marker := range visited {
		if strings.Contains(marker, "old") {
			t.Fatalf("the covered session was distilled anyway: %v", visited)
		}
	}
	if len(visited) == 0 {
		t.Fatalf("the uncovered session must still be distilled, visited %v", visited)
	}
	// Covered sessions get no cache entry: they stay covered, never cached.
	cache := loadDistillCache(brainDir)
	if _, ok := cachedDistillSessionFingerprint(cache, "main", "old"); ok {
		t.Fatal("a covered session must not gain a distill-cache entry")
	}
}

// A candidate fact is a mid-run flush of THIS machine's unfinished distill
// (facts land before the cache entry does), and an authored fact
// (`brain remember --session X`) is not a distillation. Neither proves the
// session was distilled; counting them silently drops the session forever.
func TestCoveredSessionIDsIgnoresCandidatesAndAuthoredFacts(t *testing.T) {
	t.Parallel()
	brainDir := t.TempDir()
	candidate := coverageRecord("fact:dddddddddddddddddddddddd", "sess-unfinished")
	candidate.Status = "candidate"
	authored := coverageRecord("fact:eeeeeeeeeeeeeeeeeeeeeeee", "sess-remembered")
	authored.Origin = "authored"
	if err := factstore.Write(brainDir, []factmerge.Record{candidate, authored}); err != nil {
		t.Fatal(err)
	}

	covered := coveredSessionIDs(brainDir, "")

	if covered["sess-unfinished"] {
		t.Error("a candidate fact must not mark its session covered")
	}
	if covered["sess-remembered"] {
		t.Error("an authored fact must not mark its session covered")
	}
}

func TestDryRunReportAccountsForCoveredSessions(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	brainDir := writeOrderedDistillFixture(t, now, "old", "mid", "new")
	opts := distillCommandOptions{covered: map[string]bool{coveredSessionKey("mid", "cp-mid"): true}, maxChunkBytes: defaultDistillChunkSize}
	report, err := buildDistillDryRunReport(brainDir, opts, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.SessionsToDistill != 2 || report.CoveredSessions != 1 {
		t.Fatalf("report must say 2 to distill + 1 covered, got %d + %d", report.SessionsToDistill, report.CoveredSessions)
	}
	var toDistill int
	for _, branch := range report.Branches {
		toDistill += branch.SessionsToDistill
	}
	if toDistill != report.SessionsToDistill {
		t.Fatalf("branch stats contradict the top-level count: %d vs %d", toDistill, report.SessionsToDistill)
	}
}

// nil covered must mean the SAME thing to the plan and to the run: no
// coverage. runDistill is the one place the set is computed (local store plus
// hosted feed cache); a plan-side fallback that read only the local store made
// the dry-run plan skip sessions a nil-covered run would distill.
func TestDistillPlanWithNilCoveredSkipsNothing(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	brainDir := writeOrderedDistillFixture(t, now, "old")
	if err := factstore.Write(brainDir, []factmerge.Record{coverageRecord("fact:ffffffffffffffffffffffff", "old")}); err != nil {
		t.Fatal(err)
	}
	manifest, err := bmanifest.Load(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := buildDistillPlan(brainDir, manifest, distillCommandOptions{}, "salt")
	if err != nil {
		t.Fatal(err)
	}
	if plan.CoveredSessions != 0 || plan.SessionsToDistill != 1 {
		t.Fatalf("nil covered must skip nothing, got covered=%d distill=%d", plan.CoveredSessions, plan.SessionsToDistill)
	}
}

// Coverage must track the session's CURRENT latest checkpoint, not the bare
// session id: a teammate distilled early, the transcript keeps growing in git,
// and a clone with no cache entry would otherwise skip the new content forever
// (trail 2851 finding 01M768ADMYKZFGZXWX9FEGRN4Z).
func TestRunDistillRedistillsAGrownCoveredSession(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	brainDir := writeOrderedDistillFixture(t, now, "old")
	// The fixture's session has LatestCheckpoint "cp-old"; the covering fact
	// cites an EARLIER checkpoint of the same session.
	var visited []string
	opts := distillCommandOptions{
		agent:         "command",
		agentCommand:  []string{"fake"},
		run:           recordingDistillRunner(&visited),
		maxChunkBytes: defaultDistillChunkSize,
		timeout:       time.Minute,
		concurrency:   1,
		covered:       map[string]bool{coveredSessionKey("old", "cp-old-stale"): true},
	}
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now); err != nil {
		t.Fatal(err)
	}
	if len(visited) == 0 {
		t.Fatal("a grown covered session must be re-distilled")
	}
}
