package cli

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// promotableCorpusDir builds a brain dir containing a corpus with one
// promotable, corroborated deploy:release consolidation, and returns the dir.
func promotableCorpusDir(t *testing.T, now time.Time) string {
	t.Helper()
	brainDir := t.TempDir()
	transcriptPath := filepath.Join(brainDir, "sessions", "main", "x.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcriptPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcriptPath, []byte("deploy release\nmise build\nmise deploy\nverify release\ndone\n"), 0o600); err != nil {
		t.Fatal(err)
	}
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
	db, err := openPatternCorpusMutableDB(brainDir)
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
	db, err := openPatternCorpusMutableDB(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	for i, confidence := range []float64{0.91, 0.73, 0.55, 0.42} {
		pid := fmt.Sprintf("pattern:overview-%d", i)
		cluster := fmt.Sprintf("overview:%d", i)
		rec := dossierRecord{SchemaVersion: dossierSchemaVersion, PatternID: pid, ClusterKey: cluster, Fingerprint: "sha256:test", Title: cluster, Confidence: confidence}
		blob, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO patterns (id,type,scope,repo_key,cluster_key,title,strength,strength_label,support,fingerprint,status,created_at,updated_at) VALUES (?, 'task','repo','gh/acme/cli',?,?,?,'strong',12,'sha256:test','active','t','t')`, pid, cluster, cluster, confidence); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO dossiers (pattern_id,cluster_key,fingerprint,json_redacted,status,created_at,updated_at) VALUES (?,?, 'sha256:test',?,'current','t','t')`, pid, cluster, string(blob)); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	got := strongestConsolidations(brainDir, 3)
	if len(got) != 3 {
		t.Fatalf("overview returned %d consolidations, want exact cap 3", len(got))
	}
	// Ordered by confidence, capped, all current and not rejected.
	for i := 1; i < len(got); i++ {
		if got[i-1].Confidence < got[i].Confidence {
			t.Errorf("consolidations not ordered by confidence: %v", got)
		}
	}
	// Stale dossiers are excluded from the overview (they no longer hold).
	db, err = openPatternCorpusMutableDB(brainDir)
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
	db, err := openPatternCorpusMutableDB(brainDir)
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
