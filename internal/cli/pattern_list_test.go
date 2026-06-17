package cli

import (
	"testing"
	"time"
)

// P1: the public `patterns` listing reads from the V2 corpus, not the legacy
// JSON views.
func TestPatternsListFromCorpus(t *testing.T) {
	brainDir := promotableCorpusDir(t, time.Now())

	views, idx, ok := loadCorpusPatternViews(brainDir)
	if !ok {
		t.Fatal("expected corpus-backed views")
	}
	if len(views) == 0 {
		t.Fatal("expected at least one corpus pattern view")
	}

	// Type variety is present (task + procedure at minimum) and carries V2 fields.
	var task *patternView
	byType := map[string]int{}
	for i := range views {
		byType[views[i].Type]++
		if views[i].Type == "task" && views[i].IntentSig == "deploy:release" {
			task = &views[i]
		}
	}
	if byType["task"] == 0 || byType["procedure"] == 0 {
		t.Errorf("expected task and procedure rows, got %v", byType)
	}
	if task == nil {
		t.Fatal("expected the deploy:release task in the corpus listing")
	}
	if task.ID == "" || task.Gram == "" || task.Reinforcement == nil {
		t.Errorf("task view missing V2 fields: %+v", task)
	}
	if task.DossierStatus != "current" {
		t.Errorf("promotable task should carry dossier status current, got %q", task.DossierStatus)
	}
	if task.Example == nil {
		t.Error("task view should carry a top anchor")
	}
	if _, ok := idx[task.ID]; !ok {
		t.Error("index should contain the task by id")
	}
}

func TestPatternsListSuppressesRejected(t *testing.T) {
	brainDir := promotableCorpusDir(t, time.Now())
	db, err := openPatternCorpusDB(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	// Reject the deploy task's dossier.
	if _, err := db.Exec(`UPDATE dossiers SET verdict='rejected'
		WHERE pattern_id IN (SELECT id FROM patterns WHERE type='task' AND intent_sig='deploy:release')`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	views, _, ok := loadCorpusPatternViews(brainDir)
	if !ok {
		t.Fatal("expected corpus-backed views")
	}
	for _, v := range views {
		if v.Type == "task" && v.IntentSig == "deploy:release" {
			t.Errorf("rejected dossier's pattern must be suppressed from the listing: %+v", v)
		}
	}
}

func TestPatternsListDegradesWithoutCorpus(t *testing.T) {
	if _, _, ok := loadCorpusPatternViews(t.TempDir()); ok {
		t.Error("no corpus on disk must report not-corpus-backed")
	}
}
