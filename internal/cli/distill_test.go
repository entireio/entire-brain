package cli

import (
	"strings"
	"testing"
	"time"
)

func TestRenderDistillPromptSubstitutesTaxonomy(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	prompt, err := renderDistillPrompt(defaultFactTaxonomy(now))
	if err != nil {
		t.Fatalf("renderDistillPrompt: %v", err)
	}
	if strings.Contains(prompt, distillTaxonomyMarker) {
		t.Fatalf("marker not substituted")
	}
	if !strings.Contains(prompt, "preferences") {
		t.Fatalf("rendered prompt missing taxonomy category")
	}
	if !strings.Contains(prompt, "architecture.boundaries.rationale") {
		t.Fatalf("rendered prompt missing known taxonomy path")
	}
	// The fixed-category instruction from the template must survive rendering.
	if !strings.Contains(prompt, "path<TAB>fact") {
		t.Fatalf("rendered prompt missing output-format contract")
	}
}

func TestFactTaxonomyBlockDeterministic(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	a := factTaxonomyBlock(defaultFactTaxonomy(now))
	b := factTaxonomyBlock(defaultFactTaxonomy(now))
	if a != b {
		t.Fatalf("taxonomy block not deterministic")
	}
}

func TestDistilledFactsFromOutput(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	taxonomy := defaultFactTaxonomy(now)
	anchor := factAnchor{SessionID: "s1", Commit: "abc123", Transcript: "sessions/main/x.jsonl", Line: 12}

	output := strings.Join([]string{
		"preferences.coding.style\tThe user prefers tabs over spaces.",
		"architecture.boundaries.rationale,project.tooling.stack\tThe MCP adapter is stdio-only to keep the local-only boundary.",
		"this line has no tab and should be ignored",
		"bogus.unknown.category\tShould be dropped: top-level not in taxonomy.",
		"   ", // blank
	}, "\n")

	records, warnings := distilledFactsFromOutput(output, taxonomy, anchor, "main", now)
	if len(records) != 2 {
		t.Fatalf("expected 2 records, got %d (%+v)", len(records), records)
	}

	first := records[0]
	if first.Origin != factOriginDistilled || first.Status != factStatusActive {
		t.Errorf("wrong origin/status: %+v", first)
	}
	if first.Branch != "main" {
		t.Errorf("wrong branch: %q", first.Branch)
	}
	if len(first.Provenance) != 1 || first.Provenance[0].SessionID != "s1" || first.Provenance[0].Line != 12 {
		t.Errorf("provenance anchor not attached: %+v", first.Provenance)
	}
	if first.ID != factRecordID(first.Text, first.Paths) {
		t.Errorf("id not content-derived")
	}

	// The multi-path line must be normalized (sorted, both kept).
	var multi *factRecord
	for i := range records {
		if len(records[i].Paths) == 2 {
			multi = &records[i]
		}
	}
	if multi == nil {
		t.Fatalf("expected a two-path record")
	}
	if multi.Paths[0] != "architecture.boundaries.rationale" || multi.Paths[1] != "project.tooling.stack" {
		t.Errorf("paths not normalized/sorted: %v", multi.Paths)
	}

	// The unknown-category line must have produced a warning and no record.
	if !containsWarning(warnings, "unknown category") {
		t.Errorf("expected unknown-category warning, got %v", warnings)
	}
}

func TestDistilledFactsFromOutputCapsPerChunk(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	taxonomy := defaultFactTaxonomy(now)
	var lines []string
	for i := 0; i < factsMaxPerChunk+4; i++ {
		lines = append(lines, "project.tooling.stack\tFact number "+string(rune('a'+i)))
	}
	records, warnings := distilledFactsFromOutput(strings.Join(lines, "\n"), taxonomy, factAnchor{SessionID: "s1"}, "main", now)
	if len(records) != factsMaxPerChunk {
		t.Fatalf("expected cap at %d, got %d", factsMaxPerChunk, len(records))
	}
	if !containsWarning(warnings, "extra lines dropped") {
		t.Errorf("expected cap warning, got %v", warnings)
	}
}

func TestDistillAgentCommandArgs(t *testing.T) {
	codex, err := distillAgentCommandArgs("codex", nil, "PROMPT")
	if err != nil || codex[0] != "codex" || codex[len(codex)-1] != "PROMPT" {
		t.Fatalf("codex args wrong: %v err=%v", codex, err)
	}
	claude, err := distillAgentCommandArgs("claude-code", nil, "PROMPT")
	if err != nil || claude[0] != "claude" || claude[len(claude)-1] != "PROMPT" {
		t.Fatalf("claude args wrong: %v err=%v", claude, err)
	}
	cmd, err := distillAgentCommandArgs("command", []string{"my-agent", "--flag"}, "PROMPT")
	if err != nil || len(cmd) != 2 || cmd[0] != "my-agent" {
		t.Fatalf("command args wrong: %v err=%v", cmd, err)
	}
	if _, err := distillAgentCommandArgs("command", nil, "PROMPT"); err == nil {
		t.Errorf("expected error when --agent command has no command")
	}
	if _, err := distillAgentCommandArgs("none", nil, "PROMPT"); err == nil {
		t.Errorf("expected error for --agent none (distillation is agent-required)")
	}
	if _, err := distillAgentCommandArgs("bogus", nil, "PROMPT"); err == nil {
		t.Errorf("expected error for unsupported agent")
	}
}

func containsWarning(warnings []string, substr string) bool {
	for _, w := range warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}
