package cli

import (
	"testing"
	"time"

	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
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
