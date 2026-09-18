package cli

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

// newCorpusAtDir creates an empty corpus (file + schema) at brainDir so
// openPatternCorpusMutableDB can reopen it; mirrors promotableCorpusDir's setup.
func newCorpusAtDir(t *testing.T, brainDir string) {
	t.Helper()
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
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

// seedCorrectedEpisodes inserts n corrected episodes sharing one recurring
// failing command — the raw material for a failure→recovery lesson.
func seedCorrectedEpisodes(t *testing.T, brainDir, intentSig, failingCmd string, n int, now time.Time) {
	t.Helper()
	newCorpusAtDir(t, brainDir)
	db, err := openPatternCorpusMutableDB(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("episode:%s%d", intentSig, i)
		insertCorpusEpisode(t, db, id, intentSig, "corrected", 3, now)
		if _, err := db.Exec(`INSERT INTO episode_commands (episode_id, ord, head, raw_redacted, line, exit_code, failed) VALUES (?,?,?,?,?,?,1)`,
			id, 0, failingCmd, failingCmd, 1, 1); err != nil {
			t.Fatal(err)
		}
	}
}

const lessonProposalJSON = `{"lessons":[{
  "title":"Regenerate radar evidence after touching pinned files",
  "trigger":"editing a hash-pinned radar-tool source file (mcp.go, retrieve.go, workspace.go)",
  "not_when":"editing unrelated files that are not hash-pinned",
  "failure":"CI release-evidence check fails because the committed manifest/reports drifted",
  "recovery":"re-run record_radar_tool_evidence.py and commit the regenerated manifest AND audit reports",
  "knowledge":["radar-tool source files are hash-pinned","the audit also diffs the committed report files, so both must be regenerated"],
  "member_keys":["episode:radar:evidence0","episode:radar:evidence1"],
  "verdict":"accepted"
}]}`

// A procedure skill is sourced from corrected episodes, stored as an accepted
// `lesson:` deep dossier (archetype "procedure"), and surfaces as a formable
// proposal — without any command-n-gram pattern row.
func TestProposeSkillLessonsRoundTrip(t *testing.T) {
	now := time.Now()
	brainDir := t.TempDir()
	seedCorrectedEpisodes(t, brainDir, "radar:evidence", "mise run release-evidence", 2, now)

	db, err := openPatternCorpusMutableDB(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := proposeSkillLessons(context.Background(), db, brainDir, t.TempDir(), "codex", "", "", stubRunner(lessonProposalJSON), now)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Verified != 1 {
		t.Fatalf("expected 1 accepted lesson, got %+v", stats)
	}

	// Stored as an accepted procedure deep dossier.
	props := loadKnowledgeSkillProposals(brainDir)
	if len(props) != 1 {
		t.Fatalf("expected 1 knowledge proposal, got %d", len(props))
	}
	id := props[0].ID
	if !strings.HasPrefix(id, "lesson:") {
		t.Errorf("proposal id should be a lesson, got %q", id)
	}
	in, ok := loadAcceptedDeepDossier(brainDir, id)
	if !ok {
		t.Fatal("accepted lesson dossier should load via loadAcceptedDeepDossier")
	}
	if in.rec.Archetype != "procedure" {
		t.Errorf("want procedure archetype, got %q", in.rec.Archetype)
	}
	if in.rec.NotWhen == "" || len(in.rec.Knowledge) == 0 || len(in.rec.FailureModes) == 0 {
		t.Errorf("procedure dossier missing not_when/knowledge/failure_modes: %+v", in.rec)
	}

	// The synthesis input leads with the non-obvious knowledge and the negative
	// trigger, and frames any commands as supporting evidence — not the spine.
	ev := buildDeepSkillEvidence(in)
	for _, want := range []string{
		"Archetype: procedure",
		"Do NOT use when:",
		"Non-obvious knowledge (the spine",
		"hash-pinned",
	} {
		if !strings.Contains(ev, want) {
			t.Errorf("procedure skill evidence missing %q:\n%s", want, ev)
		}
	}
}

// Item #4: a lesson dossier retains the selected episodes' source anchors, so
// buildDeepSkillEvidence carries real provenance — not only the proposal summary.
func TestLessonDossierCarriesSourceAnchors(t *testing.T) {
	now := time.Now()
	brainDir := t.TempDir()
	seedCorrectedEpisodes(t, brainDir, "radar:evidence", "mise run release-evidence", 2, now)
	db, err := openPatternCorpusMutableDB(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = proposeSkillLessons(context.Background(), db, brainDir, t.TempDir(), "codex", "", "", stubRunner(lessonProposalJSON), now)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	props := loadKnowledgeSkillProposals(brainDir)
	if len(props) != 1 {
		t.Fatalf("expected 1 lesson proposal, got %d", len(props))
	}
	in, ok := loadAcceptedDeepDossier(brainDir, props[0].ID)
	if !ok {
		t.Fatal("lesson dossier should load")
	}
	if len(in.rec.SourceAnchors) != 2 {
		t.Fatalf("lesson dossier must retain source anchors for its members, got %d", len(in.rec.SourceAnchors))
	}
	ev := buildDeepSkillEvidence(in)
	if !strings.Contains(ev, "Source anchors (provenance)") || !strings.Contains(ev, "sessions/main/x.jsonl") {
		t.Errorf("lesson evidence must carry real source provenance:\n%s", ev)
	}
}

// Item #2: editing episode CONTENT (failing command) while keeping the same
// episode keys invalidates the cached lesson proposal (the agent re-runs).
func TestProposeSkillLessonsCacheInvalidatesOnContentChange(t *testing.T) {
	now := time.Now()
	brainDir := t.TempDir()
	seedCorrectedEpisodes(t, brainDir, "radar:evidence", "mise run release-evidence", 2, now)
	db, err := openPatternCorpusMutableDB(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	calls := 0
	run := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		calls++
		return lessonProposalJSON, nil
	}
	if _, err := proposeSkillLessons(context.Background(), db, brainDir, t.TempDir(), "codex", "", "", run, now); err != nil {
		t.Fatal(err)
	}
	// Unchanged evidence → cache hit, no second agent call.
	if _, err := proposeSkillLessons(context.Background(), db, brainDir, t.TempDir(), "codex", "", "", run, now); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("unchanged evidence must reuse cache, calls=%d", calls)
	}
	// Mutate content of a member episode WITHOUT changing its key.
	if _, err := db.Exec(`UPDATE episode_commands SET head='different failing cmd', raw_redacted='different failing cmd' WHERE episode_id=?`, "episode:radar:evidence0"); err != nil {
		t.Fatal(err)
	}
	if _, err := proposeSkillLessons(context.Background(), db, brainDir, t.TempDir(), "codex", "", "", run, now); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("changed evidence content must invalidate the cache, calls=%d", calls)
	}
}

// A lesson the agent rejects (a generic mistake) is not stored as a proposal.
func TestProposeSkillLessonsRejectsGeneric(t *testing.T) {
	now := time.Now()
	brainDir := t.TempDir()
	seedCorrectedEpisodes(t, brainDir, "commit:flow", "git push", 2, now)
	db, err := openPatternCorpusMutableDB(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	run := stubRunner(`{"lessons":[{"title":"Remember to pull before push","trigger":"pushing","failure":"non-fast-forward","recovery":"git pull","member_keys":["episode:commit:flow0","episode:commit:flow1"],"verdict":"rejected"}]}`)
	stats, err := proposeSkillLessons(context.Background(), db, brainDir, t.TempDir(), "codex", "", "", run, now)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Verified != 0 {
		t.Errorf("a rejected generic lesson must not be stored, got %+v", stats)
	}
	if props := loadKnowledgeSkillProposals(brainDir); len(props) != 0 {
		t.Errorf("rejected lesson must not surface as a proposal, got %d", len(props))
	}
}

// The proposal phase is egress-gated: a no-egress agent is refused before any
// episode leaves the brain.
func TestProposeSkillLessonsEgressGated(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "1")
	now := time.Now()
	brainDir := t.TempDir()
	seedCorrectedEpisodes(t, brainDir, "radar:evidence", "mise run release-evidence", 2, now)
	db, err := openPatternCorpusMutableDB(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	called := false
	run := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		called = true
		return lessonProposalJSON, nil
	}
	_, err = proposeSkillLessons(context.Background(), db, brainDir, t.TempDir(), "codex", "", "", run, now)
	if err == nil || !strings.Contains(err.Error(), "no_egress") {
		t.Fatalf("expected no_egress refusal, got %v", err)
	}
	if called {
		t.Error("the agent must not be invoked under no-egress")
	}
}
