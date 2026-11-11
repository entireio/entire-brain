package cli

import (
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
		anchors = append(anchors, factmerge.Anchor{SessionID: s})
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
		if !covered[want] {
			t.Errorf("session %s must be covered by local fact provenance", want)
		}
	}
	if covered[""] {
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

	if !covered["sess-filtered"] {
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
	status := factsBackfillStatusForBrain(f.storage.BrainDir, map[string]bool{"s1": true, "s2": true})
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
	opts := distillCommandOptions{covered: map[string]bool{"mid": true}}
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
	opts := distillCommandOptions{covered: map[string]bool{"old": true}}
	plan, err := buildDistillPlan(brainDir, manifest, opts, "salt")
	if err != nil {
		t.Fatal(err)
	}
	if plan.SessionsToDistill != 1 || plan.CoveredSessions != 0 {
		t.Fatalf("a grown cached session must be re-planned, got distill=%d covered=%d", plan.SessionsToDistill, plan.CoveredSessions)
	}
}
