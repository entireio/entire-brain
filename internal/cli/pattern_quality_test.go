package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Phase 6 quality gates (docs/pattern_v2.md). Tests-and-fixtures-first: each test
// encodes one invariant the pattern corpus must hold so the feature cannot
// silently regress.

// Gate: top (promotable) candidates must include at least two independent source
// anchors unless explicitly fact-backed.
func TestQualityPromotableHaveAnchorsOrFactBacked(t *testing.T) {
	db := promotableCorpus(t, time.Now())
	if err := buildPatternCandidates(db, "gh/acme/cli", time.Now()); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`SELECT json_redacted FROM dossiers`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var blob string
		if err := rows.Scan(&blob); err != nil {
			t.Fatal(err)
		}
		var rec dossierRecord
		if err := json.Unmarshal([]byte(blob), &rec); err != nil {
			t.Fatal(err)
		}
		n++
		if len(rec.SourceAnchors) < 2 && len(rec.Facts) == 0 {
			t.Errorf("promotable dossier %q has %d anchors and no facts — gate requires >=2 anchors or fact-backing",
				rec.Title, len(rec.SourceAnchors))
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("expected at least one promotable dossier to check")
	}
}

// Gate: redaction must hold over the consolidation record (the JSON used both at
// rest and as agent verifier input).
func TestQualityConsolidationRedactionBoundary(t *testing.T) {
	now := time.Now()
	db := freshCorpusDB(t)
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("episode:s%d", i)
		insertCorpusEpisode(t, db, id, "deploy:release", "success", 2, now)
		insertCorpusShape(t, db, id, "mise build", "mise deploy")
		// A fact whose paths carry a home-dir username AND a secret token.
		if _, err := db.Exec(`INSERT INTO episode_facts (episode_id, fact_id, kind, paths, weight) VALUES (?,?,?,?,2)`,
			id, "fact:deploy", "architecture", "/Users/example-user/secret ghp_SECRETTOKEN0123456789abcdef"); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 200; i++ {
		id := fmt.Sprintf("episode:n%d", i)
		insertCorpusEpisode(t, db, id, "other:thing", "neutral", 2, now)
		insertCorpusShape(t, db, id, "ls", "cat")
	}
	if err := buildPatternCandidates(db, "gh/acme/cli", now); err != nil {
		t.Fatal(err)
	}
	var blob string
	if err := db.QueryRow(`SELECT group_concat(json_redacted, '||') FROM dossiers`).Scan(&blob); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"example-user", "ghp_SECRETTOKEN0123456789abcdef"} {
		if strings.Contains(blob, secret) {
			t.Errorf("dossier JSON leaked %q:\n%s", secret, blob)
		}
	}
	// A second redaction pass over stored JSON must keep both sensitive values
	// absent. This does not exercise the verifier transport.
	if got := redactText(blob); strings.Contains(got, "example-user") || strings.Contains(got, "ghp_SECRET") {
		t.Errorf("second redaction pass leaked a secret: %s", got)
	}
}

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
	corpusPath := filepath.Join(brainDir, filepath.FromSlash(patternCorpusPath))
	if _, err := os.Stat(corpusPath); err != nil {
		t.Fatalf("corpus should exist after build: %v", err)
	}
	// A durable user decision.
	decision := skillMemoryRecord{
		PatternID: "pattern:abc", Status: "declined", SkillName: "release-check",
		Note: "not worth a skill", CreatedAt: now, UpdatedAt: now,
	}
	if err := writeBrainSkillMemory(brainDir, []skillMemoryRecord{decision}); err != nil {
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
	got, err := loadBrainSkillMemory(brainDir)
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
	if err := writeFacts(brainDir, "main", []factRecord{
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
