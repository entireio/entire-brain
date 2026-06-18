package cli

import (
	"testing"
	"time"
)

func findPractice(ps []practiceRecord, statementPrefix string) *practiceRecord {
	for i := range ps {
		if len(ps[i].Statement) >= len(statementPrefix) && ps[i].Statement[:len(statementPrefix)] == statementPrefix {
			return &ps[i]
		}
	}
	return nil
}

func TestBuildBrainPractices(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	brainDir := t.TempDir()

	facts := []factRecord{
		{
			ID:     "fact:cn",
			Paths:  []string{"architecture.data.flow"},
			Text:   "Approach X was rejected because it made the benchmark slower.",
			Branch: "main", Origin: "distilled", Status: "active", Confidence: "1.00",
			Provenance: []factAnchor{
				{SessionID: "s1", Transcript: "sessions/main/s1.jsonl", Line: 10},
				{SessionID: "s2", Transcript: "sessions/main/s2.jsonl", Line: 20},
			},
			UpdatedAt: now.Add(-200 * 24 * time.Hour),
		},
		{
			ID:     "fact:dec",
			Paths:  []string{"project.layout.dirs"},
			Text:   "The CLI lives under cmd/entire-brain.",
			Branch: "main", Origin: "distilled", Status: "active", Confidence: "1.00",
			Provenance: []factAnchor{{SessionID: "s1", Transcript: "sessions/main/s1.jsonl", Line: 5}},
			UpdatedAt:  now.Add(-200 * 24 * time.Hour),
		},
		{
			ID:     "fact:dead",
			Paths:  []string{"project.layout.dirs"},
			Text:   "A superseded statement that must not appear.",
			Branch: "main", Origin: "distilled", Status: "superseded", Confidence: "1.00",
			Provenance: []factAnchor{{SessionID: "s1"}},
			UpdatedAt:  now,
		},
	}
	if err := writeFacts(brainDir, "main", facts); err != nil {
		t.Fatal(err)
	}

	manifest := &exportManifest{
		RepoKey: "gh/acme/cli",
		Sources: &brainSources{
			Sessions: &sessionSourceManifest{Sessions: []exportSession{
				{SessionID: "s1", CreatedAt: now.Add(-30 * 24 * time.Hour)},
				{SessionID: "s2", CreatedAt: now.Add(-5 * 24 * time.Hour)},
			}},
			Facts: &factSourceManifest{Branches: []string{"main"}},
		},
	}

	practices, err := buildBrainPractices(brainDir, manifest, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(practices) != 2 {
		t.Fatalf("got %d practices, want 2 (superseded excluded): %+v", len(practices), practices)
	}

	cn := findPractice(practices, "Approach X")
	dec := findPractice(practices, "The CLI lives")
	if cn == nil || dec == nil {
		t.Fatalf("missing expected practices: %+v", practices)
	}
	if cn.Kind != factKindClosedNegative {
		t.Errorf("rejected-approach kind = %q, want closed-negative", cn.Kind)
	}
	if dec.Kind != factKindDecision {
		t.Errorf("project fact kind = %q, want decision", dec.Kind)
	}
	// closed-negative outranks decision (kind weight dominates), and is recency-
	// boosted by its newer provenance session (s2 at -5d vs s1 at -30d).
	if cn.Strength <= dec.Strength {
		t.Errorf("closed-negative strength %.3f should exceed decision %.3f", cn.Strength, dec.Strength)
	}
	if cn.Support != 2 {
		t.Errorf("closed-negative support = %d, want 2 distinct sessions", cn.Support)
	}
	// Recency comes from the provenance session timestamp (-5d), not the uniform
	// fact UpdatedAt (-200d) which would otherwise score ~0.
	if cn.LastSeen == nil || !cn.LastSeen.Equal(now.Add(-5*24*time.Hour)) {
		t.Errorf("last_seen = %v, want newest provenance session time", cn.LastSeen)
	}
	if cn.ID != "pattern:practice:fact:cn" {
		t.Errorf("id = %q, want pattern:practice:fact:cn", cn.ID)
	}
	// Sorted by descending strength.
	if practices[0].Strength < practices[len(practices)-1].Strength {
		t.Error("practices not sorted by descending strength")
	}
}

func TestParsePracticeConfidence(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"1.00", 1.0}, {"0.78", 0.78}, {"", 0.5}, {"high", 1.0}, {"low", 0.3}, {"garbage", 0.5},
	}
	for _, c := range cases {
		if got := parsePracticeConfidence(c.in); got != c.want {
			t.Errorf("parsePracticeConfidence(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
