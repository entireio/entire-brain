package cli

import (
	"database/sql"
	"fmt"
	"testing"
	"time"
)

// promotableCorpusDir builds a brain dir containing a corpus with one
// promotable, corroborated deploy:release consolidation, and returns the dir.
func promotableCorpusDir(t *testing.T, now time.Time) string {
	t.Helper()
	brainDir := t.TempDir()
	path, err := prepareBrainRelativeSQLiteFile(brainDir, patternCorpusPath)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(sqliteDriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range patternCorpusSchema {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("episode:d%d", i)
		insertCorpusEpisode(t, db, id, "deploy:release", "success", 2, now)
		insertCorpusShape(t, db, id, "mise build", "mise deploy")
		if _, err := db.Exec(`INSERT INTO episode_facts (episode_id, fact_id, kind, paths, weight) VALUES (?,?,?,?,2)`,
			id, "fact:deploy", "architecture", "ops/deploy.go"); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 200; i++ {
		id := fmt.Sprintf("episode:u%d", i)
		insertCorpusEpisode(t, db, id, "other:thing", "neutral", 2, now)
		insertCorpusShape(t, db, id, "ls", "cat")
	}
	if err := buildPatternCandidates(db, "gh/acme/cli", now); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return brainDir
}

func TestBriefConsolidationsTaskGated(t *testing.T) {
	brainDir := promotableCorpusDir(t, time.Now())

	// Matching task → the deploy consolidation surfaces, anchored and actionable.
	match := loadBriefConsolidations(brainDir, brainBriefFileMatchTerms("deploy a release to production"), 5)
	if len(match) == 0 {
		t.Fatal("expected the deploy:release consolidation for a matching task")
	}
	got := match[0]
	if got.Type != "task" {
		t.Errorf("type = %q, want task", got.Type)
	}
	if len(got.Workflow) == 0 {
		t.Error("consolidation should carry the workflow")
	}
	if got.Anchor == nil {
		t.Error("consolidation should carry a source anchor")
	}

	// Unrelated task → no ambient pattern noise.
	none := loadBriefConsolidations(brainDir, brainBriefFileMatchTerms("rename a css variable in the theme"), 5)
	if len(none) != 0 {
		t.Errorf("unrelated task must return no consolidations, got %d: %+v", len(none), none)
	}
}

func TestBriefConsolidationsSuppressRejected(t *testing.T) {
	brainDir := promotableCorpusDir(t, time.Now())
	db, err := openPatternCorpusDB(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE dossiers SET verdict='rejected'`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	got := loadBriefConsolidations(brainDir, brainBriefFileMatchTerms("deploy a release"), 5)
	if len(got) != 0 {
		t.Errorf("a rejected consolidation must not surface, got %d", len(got))
	}
}

func TestBriefConsolidationsGracefulWithoutCorpus(t *testing.T) {
	// No corpus on disk → empty, not error/panic.
	if got := loadBriefConsolidations(t.TempDir(), brainBriefFileMatchTerms("anything"), 5); len(got) != 0 {
		t.Errorf("missing corpus should yield no consolidations, got %+v", got)
	}
}

func TestStrongestConsolidationsForOverview(t *testing.T) {
	brainDir := promotableCorpusDir(t, time.Now())
	got := strongestConsolidations(brainDir, 3)
	if len(got) == 0 {
		t.Fatal("expected at least one strong consolidation for the overview")
	}
	// Ordered by confidence, capped, all current and not rejected.
	for i := 1; i < len(got); i++ {
		if got[i-1].Confidence < got[i].Confidence {
			t.Errorf("consolidations not ordered by confidence: %v", got)
		}
	}
	if len(got) > 3 {
		t.Errorf("overview must cap consolidations, got %d", len(got))
	}

	// Stale dossiers are excluded from the overview (they no longer hold).
	db, err := openPatternCorpusDB(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE dossiers SET status='stale'`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if got := strongestConsolidations(brainDir, 3); len(got) != 0 {
		t.Errorf("stale consolidations must not surface in the overview, got %d", len(got))
	}
}

func TestGetConsolidationByPatternID(t *testing.T) {
	brainDir := promotableCorpusDir(t, time.Now())
	db, err := openPatternCorpusDB(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	var pid string
	if err := db.QueryRow(`SELECT pattern_id FROM dossiers LIMIT 1`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	db.Close()

	found, missing, err := getUnifiedBatch("", brainDir, "main", []string{pid, "pattern:does-not-exist"})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Source != "consolidation" || found[0].ID != pid {
		t.Fatalf("expected one consolidation result for %s, got %+v", pid, found)
	}
	if len(found[0].Text) == 0 {
		t.Error("consolidation get result should render text")
	}
	if len(missing) != 1 || missing[0] != "pattern:does-not-exist" {
		t.Errorf("expected the unknown pattern id reported missing, got %+v", missing)
	}
}
