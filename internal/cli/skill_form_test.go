package cli

import (
	"strings"
	"testing"
	"time"
)

func TestSkillDestinations(t *testing.T) {
	// standard global -> the cross-agent path read by 4 agents.
	d, err := skillDestinations("standard", "global", "my-skill", "/repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(d) != 1 || !strings.HasSuffix(d[0].Path, "/.agents/skills/my-skill/SKILL.md") {
		t.Errorf("standard global path = %+v", d)
	}
	if len(d[0].Agents) != 4 {
		t.Errorf("standard agents = %v, want 4", d[0].Agents)
	}

	// claude-code repo -> in-repo .claude path, also serving opencode.
	d, err = skillDestinations("claude-code", "repo", "my-skill", "/repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(d) != 1 || d[0].Path != "/repo/.claude/skills/my-skill/SKILL.md" {
		t.Errorf("claude repo path = %+v", d)
	}

	// all -> the minimal covering set of 4 roots.
	d, err = skillDestinations("all", "global", "my-skill", "/repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(d) != 4 {
		t.Errorf("all targets = %d, want 4", len(d))
	}

	if _, err := skillDestinations("bogus", "global", "x", "/repo"); err == nil {
		t.Error("invalid target should error")
	}
	if _, err := skillDestinations("standard", "repo", "x", ""); err == nil {
		t.Error("repo scope without repoDir should error")
	}
}

func TestRecordSkillDecisionUpsert(t *testing.T) {
	brainDir := t.TempDir()
	t0 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	t1 := time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC)

	if err := recordSkillDecision(brainDir, skillMemoryRecord{PatternID: "P1", Status: skillStatusActive, SkillName: "v1", CreatedAt: t0, UpdatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	if err := recordSkillDecision(brainDir, skillMemoryRecord{PatternID: "P2", Status: skillStatusDeclined, CreatedAt: t0, UpdatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	// Update P1: same id replaces, preserves original CreatedAt.
	if err := recordSkillDecision(brainDir, skillMemoryRecord{PatternID: "P1", Status: skillStatusActive, SkillName: "v2", CreatedAt: t1, UpdatedAt: t1}); err != nil {
		t.Fatal(err)
	}

	recs, err := loadBrainSkillMemory(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2 (P1 upserted, P2 kept)", len(recs))
	}
	byID := skillMemoryByPatternID(recs)
	if byID["P1"].SkillName != "v2" {
		t.Errorf("P1 not updated: %q", byID["P1"].SkillName)
	}
	if !byID["P1"].CreatedAt.Equal(t0) {
		t.Errorf("P1 CreatedAt = %v, want preserved %v", byID["P1"].CreatedAt, t0)
	}
	if byID["P2"].Status != skillStatusDeclined {
		t.Errorf("P2 lost: %+v", byID["P2"])
	}
}
