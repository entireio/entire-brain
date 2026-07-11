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

func TestDistilledFactsFromOutputKindColumn(t *testing.T) {
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	taxonomy := defaultFactTaxonomy(now)
	anchor := factAnchor{SessionID: "s1"}

	output := strings.Join([]string{
		// New 3-field form: explicit kind column.
		"gotcha\tconstraints.invariants.general\tThe advisory lock must be released before the rename.",
		// Legacy 2-field form: kind inferred from taxonomy (preferences → preference).
		"preferences.coding.style\tThe user prefers tabs over spaces.",
		// Kind column with two tab-separated paths after it.
		"decision\tarchitecture.data.flow\tconstraints.invariants.general\tThe index is derived from the ndjson truth.",
	}, "\n")

	records, _ := distilledFactsFromOutput(output, taxonomy, anchor, "main", now)
	if len(records) != 3 {
		t.Fatalf("expected 3 records, got %d (%+v)", len(records), records)
	}
	byText := map[string]factRecord{}
	for _, r := range records {
		byText[r.Text] = r
	}
	if got := byText["The advisory lock must be released before the rename."]; got.Kind != factKindGotcha {
		t.Errorf("explicit kind column not honored: %q", got.Kind)
	}
	if got := byText["The user prefers tabs over spaces."]; got.Kind != factKindPreference {
		t.Errorf("legacy line should infer preference, got %q", got.Kind)
	}
	idx := byText["The index is derived from the ndjson truth."]
	if idx.Kind != factKindDecision {
		t.Errorf("kind column before two paths not honored: %q", idx.Kind)
	}
	if len(idx.Paths) != 2 {
		t.Errorf("both paths after the kind column should survive: %v", idx.Paths)
	}
}

func TestDistilledFactsFromOutputUnrecognizedKindRecovered(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	taxonomy := defaultFactTaxonomy(now)
	anchor := factAnchor{SessionID: "s1"}

	// The agent prepends a leading word that is NOT in the closed kind set (a
	// synonym). The valid path must still be recovered — the fact is kept and the
	// kind inferred — rather than dropped because the leading token isn't a kind.
	output := "rule\tconstraints.invariants.general\tThe lock is released before the rename."
	records, _ := distilledFactsFromOutput(output, taxonomy, anchor, "main", now)
	if len(records) != 1 {
		t.Fatalf("synonym-led line should be recovered, got %d records", len(records))
	}
	if records[0].Paths[0] != "constraints.invariants.general" {
		t.Fatalf("path not recovered: %v", records[0].Paths)
	}
	if records[0].Kind != factKindInvariant { // inferred from constraints.*
		t.Fatalf("kind should be inferred for an unrecognized leading word, got %q", records[0].Kind)
	}
	if records[0].Text != "The lock is released before the rename." {
		t.Fatalf("the synonym word leaked into text: %q", records[0].Text)
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

func TestDistilledFactsFromOutputLiteralTabEscape(t *testing.T) {
	now := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	taxonomy := defaultFactTaxonomy(now)
	anchor := factAnchor{SessionID: "s1"}

	// gpt-5.3-codex-spark sometimes writes the separator as a literal backslash-t
	// escape instead of a real tab. The line must still be recovered, not dropped.
	output := `constraints.invariants.general\tThe CLI stores version_check.json under ~/.config/entire.`
	records, warnings := distilledFactsFromOutput(output, taxonomy, anchor, "main", now)
	if len(records) != 1 {
		t.Fatalf("literal \\t line should be recovered: got %d records, warnings=%v", len(records), warnings)
	}
	r := records[0]
	if r.Text != "The CLI stores version_check.json under ~/.config/entire." {
		t.Fatalf("text wrong after \\t recovery: %q", r.Text)
	}
	if len(r.Paths) != 1 || r.Paths[0] != "constraints.invariants.general" {
		t.Fatalf("path wrong after \\t recovery: %v", r.Paths)
	}
}

// Only the first literal `\t` is the path/fact separator; a later `\t` belongs
// to the fact text and must be left untouched (not converted to a tab, which
// would split the text into a spurious extra field).
func TestDistilledFactsFromOutputLiteralTabEscapeFirstOnly(t *testing.T) {
	now := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	taxonomy := defaultFactTaxonomy(now)
	anchor := factAnchor{SessionID: "s1"}

	output := `constraints.invariants.general\tColumns are separated by\ta tab character.`
	records, warnings := distilledFactsFromOutput(output, taxonomy, anchor, "main", now)
	if len(records) != 1 {
		t.Fatalf("line with a \\t inside the fact text should be recovered: got %d records, warnings=%v", len(records), warnings)
	}
	// The second `\t` stays literal in the text — it is not the separator.
	if got := records[0].Text; got != `Columns are separated by\ta tab character.` {
		t.Fatalf("only the first \\t should be the separator; text wrong: %q", got)
	}
	if len(records[0].Paths) != 1 || records[0].Paths[0] != "constraints.invariants.general" {
		t.Fatalf("path wrong: %v", records[0].Paths)
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
	if !slices.Contains(codex, "--json") {
		t.Fatalf("codex args must request provider usage JSONL: %v", codex)
	}
	claude, err := distillAgentCommandArgs("claude-code", nil, "PROMPT")
	if err != nil || claude[0] != "claude" || claude[len(claude)-1] != "PROMPT" {
		t.Fatalf("claude args wrong: %v err=%v", claude, err)
	}
	if !slices.Contains(claude, "--output-format") || !slices.Contains(claude, "json") {
		t.Fatalf("claude args must request provider usage JSON: %v", claude)
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

func TestDistillAgentCommandArgsRejectsRemoteAgentsInNoEgressMode(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "1")
	for _, agent := range []string{"codex", "claude-code", "command"} {
		_, err := distillAgentCommandArgs(agent, []string{"local-agent"}, "PROMPT")
		if err == nil || !strings.Contains(err.Error(), "no_egress") {
			t.Fatalf("agent %s should be rejected in no-egress mode, got %v", agent, err)
		}
	}
	if _, err := distillAgentCommandArgs("ollama", nil, "PROMPT"); err != nil {
		t.Fatalf("ollama should remain available in no-egress mode: %v", err)
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

	ollama, _ := distillAgentCommandArgs("ollama", nil, "PROMPT")
	gotOllama := injectAgentModel(ollama, "ollama", "llama3.2")
	if !slices.Equal(gotOllama, []string{"ollama", "llama3.2", "PROMPT"}) {
		t.Fatalf("ollama model not inserted into runner args: %v", gotOllama)
	}
	if !slices.Equal(ollama, []string{"ollama", "", "PROMPT"}) {
		t.Fatalf("ollama model injection should not mutate original args: %v", ollama)
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

func TestInjectAgentEffort(t *testing.T) {
	codex, _ := distillAgentCommandArgs("codex", nil, "PROMPT")
	got := injectAgentEffort(codex, "codex", "low")
	if got[0] != "codex" || got[1] != "exec" || got[2] != "--config" || got[3] != "model_reasoning_effort=low" {
		t.Fatalf("codex effort not inserted as --config after exec: %v", got)
	}
	if got[len(got)-1] != "PROMPT" {
		t.Fatalf("prompt must stay last: %v", got)
	}

	claude, _ := distillAgentCommandArgs("claude-code", nil, "PROMPT")
	gotc := injectAgentEffort(claude, "claude-code", "low")
	if gotc[0] != "claude" || gotc[1] != "--effort" || gotc[2] != "low" {
		t.Fatalf("claude effort not inserted after claude: %v", gotc)
	}
	if gotc[len(gotc)-1] != "PROMPT" {
		t.Fatalf("prompt must stay last: %v", gotc)
	}

	// Empty effort and the `command` agent are no-ops.
	if base, _ := distillAgentCommandArgs("codex", nil, "PROMPT"); !slices.Equal(injectAgentEffort(base, "codex", ""), base) {
		t.Errorf("empty effort should be a no-op")
	}
	cmd, _ := distillAgentCommandArgs("command", []string{"my-agent"}, "PROMPT")
	if !slices.Equal(injectAgentEffort(cmd, "command", "x"), cmd) {
		t.Errorf("command agent should be a no-op for effort injection")
	}

	// model + effort compose (the distill/seed path applies both): both flags present, prompt last.
	both := injectAgentEffort(injectAgentModel(codex, "codex", "gpt-5.4-mini"), "codex", "low")
	if !slices.Contains(both, "--model") || !slices.Contains(both, "gpt-5.4-mini") || !slices.Contains(both, "model_reasoning_effort=low") || both[len(both)-1] != "PROMPT" {
		t.Fatalf("model+effort should compose with prompt last: %v", both)
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

// TestDistilledFactsFromOutputClosedNegativeKind covers the sixth kind end to
// end: the agent emits it as a kind column, and the rendered prompt teaches it
// (list entry + the always-capture trigger for questions settled negatively).
func TestDistilledFactsFromOutputClosedNegativeKind(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	taxonomy := defaultFactTaxonomy(now)
	anchor := factAnchor{SessionID: "s1"}

	output := "closed-negative\tarchitecture.data.flow\tQuery expansion was tried for fact recall and rejected: the lift collapsed at larger n; revisit only with a new expansion model."
	records, warnings := distilledFactsFromOutput(output, taxonomy, anchor, "main", now)
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d (warnings: %v)", len(records), warnings)
	}
	if records[0].Kind != factKindClosedNegative {
		t.Fatalf("closed-negative kind column not honored: %q", records[0].Kind)
	}

	prompt, err := renderDistillPrompt(taxonomy)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"closed-negative", "QUESTIONS SETTLED NEGATIVELY", "six words"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("distill prompt should contain %q", want)
		}
	}
}
