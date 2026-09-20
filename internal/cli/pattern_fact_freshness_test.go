package cli

import (
	"testing"
	"time"
)

func TestPatternFactLinksRefreshWithoutTranscriptChanges(t *testing.T) {
	now := time.Now()
	d := writeEpisodeFixture(t, now, "gh/acme/cli", []sessionFixture{{id: "s1", branch: "main", relPath: "sessions/main/s1.jsonl", transcript: claudeReviewTranscript}})
	f := factRecord{ID: "fact:a", Text: "review release readiness requires staging", Paths: []string{"release.validation"}, Branch: "main", Status: "active", Origin: "distilled"}
	if err := writeFacts(d, "main", []factRecord{f}); err != nil {
		t.Fatal(err)
	}
	if err := buildPatternCorpus(d, now); err != nil {
		t.Fatal(err)
	}
	db := openCorpus(t, d)
	before := corpusCount(t, db, "episode_facts")
	db.Close()
	if before == 0 {
		t.Fatal("fixture has no links")
	}
	f.Status = "retracted"
	if err := writeFacts(d, "main", []factRecord{f}); err != nil {
		t.Fatal(err)
	}
	if err := buildPatternCorpus(d, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	db = openCorpus(t, d)
	defer db.Close()
	after := corpusCount(t, db, "episode_facts")
	if after != 0 {
		t.Fatalf("regression %d %d", before, after)
	}
	t.Logf("remaining retracted fact links: %d corroboration links after refresh", after)
	db.Close()
	f.ID = "fact:new"
	f.Status = "active"
	if err := writeFacts(d, "main", []factRecord{f}); err != nil {
		t.Fatal(err)
	}
	if err := buildPatternCorpus(d, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	next := openCorpus(t, d)
	defer next.Close()
	var added int
	if err := next.QueryRow(`SELECT COUNT(*) FROM episode_facts WHERE fact_id='fact:new'`).Scan(&added); err != nil {
		t.Fatal(err)
	}
	if added == 0 {
		t.Fatal("new fact was not linked to unchanged transcript")
	}

}
