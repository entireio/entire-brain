package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	patterncore "github.com/entireio/entire-brain/internal/cli/patterns"
)

// Phase 6 quality gates (docs/pattern_v2.md). Tests-and-fixtures-first: each test
// encodes one invariant the pattern corpus must hold so the feature cannot
// silently regress.

// Gate: the rebuildable corpus can be deleted and regenerated without losing user
// decisions (skill-memory is the separate user-state store).
func TestQualityCorpusDeletablePreservesDecisions(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	brainDir := writeEpisodeFixture(t, now, "gh/acme/cli", []sessionFixture{
		{id: "s1", branch: "main", agent: "Claude", checkpoint: "cp1", relPath: "sessions/main/s1.jsonl", author: "Ada", transcript: claudeReviewTranscript},
	})
	if err := buildPatternCorpus(brainDir, now); err != nil {
		t.Fatal(err)
	}
	corpusPath := filepath.Join(brainDir, filepath.FromSlash(patterncore.CorpusPath))
	if _, err := os.Stat(corpusPath); err != nil {
		t.Fatalf("corpus should exist after build: %v", err)
	}
	// A durable user decision.
	decision := skillMemoryRecord{
		PatternID: "pattern:abc", Status: "declined", SkillName: "release-check",
		Note: "not worth a skill", CreatedAt: now, UpdatedAt: now,
	}
	if err := patterncore.WriteBrainSkillMemory(brainDir, []skillMemoryRecord{decision}); err != nil {
		t.Fatal(err)
	}
	// Delete the rebuildable corpus entirely (and its WAL/SHM siblings).
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(corpusPath + suffix); err != nil && !(suffix != "" && os.IsNotExist(err)) {
			t.Fatalf("remove corpus%s: %v", suffix, err)
		}
	}
	if err := buildPatternCorpus(brainDir, now); err != nil {
		t.Fatalf("corpus must regenerate after deletion: %v", err)
	}
	got, err := patterncore.LoadBrainSkillMemory(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].PatternID != "pattern:abc" || got[0].Status != "declined" {
		t.Errorf("user decision lost across corpus delete+rebuild: %+v", got)
	}
}

// Gate: branch-scoped evidence stays branch-correct — a fact on one branch never
// links to an episode on another branch.
func TestQualityBranchScopedFactLinks(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	transcript := `{"type":"event_msg","payload":{"type":"user_message","message":"fix the worktree fingerprint and run tests"}}
{"type":"response_item","payload":{"type":"custom_tool_call","call_id":"c2","name":"apply_patch","input":"*** Update File: internal/cli/semantic.go"}}
{"type":"response_item","payload":{"type":"function_call","call_id":"c1","name":"exec_command","arguments":"{\"cmd\":\"go test ./...\"}"}}
{"type":"event_msg","payload":{"type":"user_message","message":"thanks"}}`
	brainDir := writeEpisodeFixture(t, now, "gh/acme/cli", []sessionFixture{
		{id: "s-main", branch: "main", agent: "Codex", checkpoint: "cp1", relPath: "sessions/main/s1.jsonl", author: "Ada", transcript: transcript},
		{id: "s-feat", branch: "feature", agent: "Codex", checkpoint: "cp2", relPath: "sessions/feature/s2.jsonl", author: "Ada", transcript: transcript},
	})
	// The corroborating fact exists ONLY on main.
	if err := factstore.Write(brainDir, "main", []factRecord{
		{ID: "fact:wt", Paths: []string{"architecture.x.y"}, Text: "The worktree fingerprint hashes git status and the binary diff.", Locus: []string{"worktreefingerprint"}, Branch: "main", Origin: "distilled", Status: "active", UpdatedAt: now},
	}); err != nil {
		t.Fatal(err)
	}
	if err := buildPatternCorpus(brainDir, now); err != nil {
		t.Fatal(err)
	}
	db := openCorpus(t, brainDir)

	var mainLinks, featLinks int
	if err := db.QueryRow(`SELECT COUNT(*) FROM episode_facts ef JOIN episodes e ON e.id=ef.episode_id WHERE e.branch='main'`).Scan(&mainLinks); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM episode_facts ef JOIN episodes e ON e.id=ef.episode_id WHERE e.branch='feature'`).Scan(&featLinks); err != nil {
		t.Fatal(err)
	}
	if mainLinks == 0 {
		t.Error("expected the main-branch episode to link the main-branch fact")
	}
	if featLinks != 0 {
		t.Errorf("a main-branch fact leaked onto a feature-branch episode: %d links", featLinks)
	}
}
