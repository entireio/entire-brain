package cli

import (
	"fmt"
	"testing"
	"time"
)

// p6Brain builds a brain dir with a promotable deploy task whose evidence
// episodes touched ops/deploy.go (so review --patterns can match by file).
func p6Brain(t *testing.T) string {
	t.Helper()
	brainDir := promotableCorpusDir(t, time.Now())
	db, err := openPatternCorpusMutableDB(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for i := 0; i < 12; i++ {
		if _, err := db.Exec(`INSERT OR IGNORE INTO episode_files (episode_id, path, action, line) VALUES (?,?,?,?)`,
			fmt.Sprintf("episode:d%d", i), "ops/deploy.go", "edit", 1); err != nil {
			t.Fatal(err)
		}
	}
	return brainDir
}

func TestReviewPatternContext(t *testing.T) {
	brainDir := p6Brain(t)
	// A finding on ops/deploy.go gets the deploy task as diff-less context.
	ctx := loadReviewPatternContext(brainDir, []string{"ops/deploy.go"}, 10)
	if len(ctx) == 0 {
		t.Fatal("expected pattern context for ops/deploy.go")
	}
	if ctx[0].File != "ops/deploy.go" || ctx[0].ID == "" {
		t.Errorf("context = %+v", ctx[0])
	}
	// Unrelated file → none.
	if c := loadReviewPatternContext(brainDir, []string{"unrelated/file.go"}, 10); len(c) != 0 {
		t.Errorf("unrelated file must yield no context, got %d", len(c))
	}
	// No corpus → nil (graceful).
	if c := loadReviewPatternContext(t.TempDir(), []string{"ops/deploy.go"}, 10); c != nil {
		t.Errorf("missing corpus must yield nil, got %+v", c)
	}
}

func TestRelatedPatternPointers(t *testing.T) {
	brainDir := p6Brain(t)
	// A query overlapping the deploy task surfaces pointers.
	ptrs := relatedPatternPointers(brainDir, "deploy a release", patternPointerCap)
	if len(ptrs) == 0 {
		t.Fatal("expected related pattern pointers for a deploy query")
	}
	for _, p := range ptrs {
		if p.ID == "" || p.Type == "" {
			t.Fatalf("pointer missing id/type: %+v", p)
		}
	}
	// Unrelated query → no pointers (no drowning of facts/history/docs).
	if p := relatedPatternPointers(brainDir, "rename a css color variable", patternPointerCap); len(p) != 0 {
		t.Errorf("unrelated query must yield no pointers, got %d", len(p))
	}
	// No corpus → nil.
	if p := relatedPatternPointers(t.TempDir(), "deploy", patternPointerCap); p != nil {
		t.Errorf("missing corpus must yield nil, got %+v", p)
	}
}

func TestHandoffConsolidationsIncludesStaleNotRejected(t *testing.T) {
	brainDir := promotableCorpusDir(t, time.Now())
	db, err := openPatternCorpusMutableDB(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	// One stale (kept), then reject it in a second scenario.
	if _, err := db.Exec(`UPDATE dossiers SET status='stale'`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	got := handoffConsolidations(brainDir, 5)
	if len(got) == 0 {
		t.Fatal("handoff should include stale consolidations")
	}

	db2, err := openPatternCorpusMutableDB(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db2.Exec(`UPDATE dossiers SET verdict='rejected'`); err != nil {
		t.Fatal(err)
	}
	db2.Close()
	if c := handoffConsolidations(brainDir, 5); len(c) != 0 {
		t.Errorf("handoff must exclude rejected consolidations, got %d", len(c))
	}
}
