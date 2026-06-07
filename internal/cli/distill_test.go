package cli

import (
	"slices"
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

func TestRenderDistillPromptStripsFrontmatter(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	prompt, err := renderDistillPrompt(defaultFactTaxonomy(now))
	if err != nil {
		t.Fatalf("renderDistillPrompt: %v", err)
	}
	// The agent runners pass the prompt as a positional CLI arg, so a leading
	// "---" (YAML frontmatter) would be parsed as an unknown flag and rejected.
	if strings.HasPrefix(prompt, "-") {
		t.Fatalf("prompt must not start with a dash (frontmatter not stripped): %q", prompt[:40])
	}
	if strings.Contains(prompt, "name: entire-brain-distill") {
		t.Fatalf("frontmatter leaked into prompt")
	}
	if !strings.HasPrefix(prompt, "You are an expert note-taker") {
		t.Fatalf("prompt body missing after frontmatter strip: %q", prompt[:40])
	}
}

func TestStripTemplateFrontmatter(t *testing.T) {
	in := "---\nname: x\ndescription: y\n---\n\nBody starts here.\n"
	if got := stripTemplateFrontmatter(in); got != "Body starts here.\n" {
		t.Fatalf("got %q", got)
	}
	// No frontmatter: unchanged.
	plain := "No frontmatter here.\n"
	if got := stripTemplateFrontmatter(plain); got != plain {
		t.Fatalf("plain content changed: %q", got)
	}
	// Unterminated frontmatter: left as-is rather than eating the whole file.
	open := "---\nname: x\nstill going\n"
	if got := stripTemplateFrontmatter(open); got != open {
		t.Fatalf("unterminated frontmatter changed: %q", got)
	}
	// CRLF line endings (Windows checkout): the body must not start with a stray
	// "\r" left over from splitting on "\n". Regression guard for the Windows CI
	// failure in TestRenderDistillPromptStripsFrontmatter.
	crlf := "---\r\nname: x\r\ndescription: y\r\n---\r\n\r\nBody starts here.\r\n"
	if got := stripTemplateFrontmatter(crlf); got != "Body starts here.\r\n" {
		t.Fatalf("crlf content not stripped cleanly: %q", got)
	}
}

func TestCapWarnings(t *testing.T) {
	var w []string
	for i := 0; i < 120; i++ {
		w = append(w, "warn")
	}
	capped := capWarnings(w, maxDistillWarnings)
	if len(capped) != maxDistillWarnings+1 {
		t.Fatalf("expected %d entries, got %d", maxDistillWarnings+1, len(capped))
	}
	if !strings.Contains(capped[len(capped)-1], "more warnings") {
		t.Fatalf("missing overflow summary: %q", capped[len(capped)-1])
	}
	// Under the cap: unchanged.
	small := []string{"a", "b"}
	if got := capWarnings(small, maxDistillWarnings); len(got) != 2 {
		t.Fatalf("small list changed: %v", got)
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

func TestDistilledFactsFromOutputTabSeparatedPaths(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	taxonomy := defaultFactTaxonomy(now)
	anchor := factAnchor{SessionID: "s1"}

	// The agent separated two paths with a TAB instead of a comma. The second
	// path must not leak into the fact text; both paths must be recovered.
	output := "architecture.data.flow\tconstraints.invariants.general\tHook-supporting agents stamp their config with cli_version."
	records, _ := distilledFactsFromOutput(output, taxonomy, anchor, "main", now)
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}
	r := records[0]
	if r.Text != "Hook-supporting agents stamp their config with cli_version." {
		t.Fatalf("path leaked into text: %q", r.Text)
	}
	if len(r.Paths) != 2 || r.Paths[0] != "architecture.data.flow" || r.Paths[1] != "constraints.invariants.general" {
		t.Fatalf("both tab-separated paths should be recovered: %v", r.Paths)
	}
}

func TestSplitFactLine(t *testing.T) {
	cases := []struct {
		line  string
		paths []string
		text  string
	}{
		{"a.b.c\tfact text", []string{"a.b.c"}, "fact text"},
		{"a.b.c,d.e.f\tfact text", []string{"a.b.c", "d.e.f"}, "fact text"},
		{"a.b.c\td.e.f\tfact text", []string{"a.b.c", "d.e.f"}, "fact text"},
		// A fact that itself contains a tab keeps everything after the paths.
		{"a.b.c\tprose with\ta tab", []string{"a.b.c"}, "prose with a tab"},
	}
	for _, tc := range cases {
		paths, text := splitFactLine(tc.line)
		if text != tc.text {
			t.Errorf("line %q: text = %q, want %q", tc.line, text, tc.text)
		}
		if len(paths) != len(tc.paths) {
			t.Errorf("line %q: paths = %v, want %v", tc.line, paths, tc.paths)
			continue
		}
		for i := range paths {
			if paths[i] != tc.paths[i] {
				t.Errorf("line %q: paths = %v, want %v", tc.line, paths, tc.paths)
			}
		}
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
	// The empty MCP config must be {"mcpServers":{}} — bare {} is rejected by the
	// current claude CLI ("mcpServers: expected record, received undefined").
	if !slices.Contains(claude, `{"mcpServers":{}}`) {
		t.Fatalf("claude args missing valid empty mcp-config: %v", claude)
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

func TestInjectAgentModel(t *testing.T) {
	codex, _ := distillAgentCommandArgs("codex", nil, "PROMPT")
	got := injectAgentModel(codex, "codex", "gpt-5.3-codex-spark")
	if got[0] != "codex" || got[1] != "exec" || got[2] != "--model" || got[3] != "gpt-5.3-codex-spark" {
		t.Fatalf("codex model not inserted after exec: %v", got)
	}
	if got[len(got)-1] != "PROMPT" {
		t.Fatalf("prompt must stay last: %v", got)
	}

	claude, _ := distillAgentCommandArgs("claude-code", nil, "PROMPT")
	gotc := injectAgentModel(claude, "claude-code", "sonnet")
	if gotc[0] != "claude" || gotc[1] != "--model" || gotc[2] != "sonnet" {
		t.Fatalf("claude model not inserted after claude: %v", gotc)
	}
	if gotc[len(gotc)-1] != "PROMPT" {
		t.Fatalf("prompt must stay last: %v", gotc)
	}

	// Empty model and the `command` agent are no-ops.
	if base, _ := distillAgentCommandArgs("codex", nil, "PROMPT"); !slices.Equal(injectAgentModel(base, "codex", ""), base) {
		t.Errorf("empty model should be a no-op")
	}
	cmd, _ := distillAgentCommandArgs("command", []string{"my-agent"}, "PROMPT")
	if !slices.Equal(injectAgentModel(cmd, "command", "x"), cmd) {
		t.Errorf("command agent should be a no-op for model injection")
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
