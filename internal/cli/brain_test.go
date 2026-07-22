package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestBrainManifestMigratesFlatExportAndPreservesSeed(t *testing.T) {
	outputDir := t.TempDir()
	seed := &seedSourceManifest{
		GeneratedAt:     time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		WorktreeMode:    "tracked",
		FileFingerprint: "sha256:seed",
		SummaryPath:     "seed/repo-overview.md",
	}
	if err := writeBrainSeedSource(outputDir, "gh/example/repo", seed); err != nil {
		t.Fatalf("write seed source: %v", err)
	}

	session := exportSession{
		SessionID:        "session-one",
		LatestCheckpoint: "aaa111aaa111",
		CreatedAt:        time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		TranscriptPath:   "sessions/main/session.jsonl",
	}
	export := exportManifest{
		SchemaVersion:      1,
		GeneratedAt:        time.Date(2026, 1, 2, 1, 0, 0, 0, time.UTC),
		RepoRoot:           "/repo",
		TranscriptMode:     "compact",
		Scope:              exportScopeAll,
		CheckpointLimit:    10,
		CheckpointsScanned: 1,
		Sessions:           []exportSession{session},
		Branches:           []exportBranch{{Branch: "main", Directory: "sessions/main", SessionCount: 1, Default: true}},
	}
	if err := writeBrainSessionSource(outputDir, "gh/example/repo", export); err != nil {
		t.Fatalf("write session source: %v", err)
	}

	manifest, err := loadBrainManifest(outputDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if manifest.SchemaVersion != brainManifestSchemaVersion {
		t.Fatalf("schema = %d, want %d", manifest.SchemaVersion, brainManifestSchemaVersion)
	}
	if manifest.Sources == nil || manifest.Sources.Seed == nil || manifest.Sources.Sessions == nil {
		t.Fatalf("manifest did not preserve both sources: %+v", manifest.Sources)
	}
	if len(manifest.Sessions) != 1 {
		t.Fatalf("legacy sessions alias missing: %+v", manifest.Sessions)
	}
	readme, err := os.ReadFile(filepath.Join(outputDir, exportReadmeFileName))
	if err != nil {
		t.Fatalf("read readme: %v", err)
	}
	if !strings.Contains(string(readme), "Seeded Baseline") || !strings.Contains(string(readme), "Session History") {
		t.Fatalf("combined readme missing sections:\n%s", readme)
	}
}

func TestBrainBriefJSONUsesSemanticContextAndLiveOverlay(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticBoundaryFixtureSnapshot())
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return now }}

	if err := runSemanticIndex((&cobra.Command{}).Context(), &cobra.Command{Use: "index"}, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	tracePath := filepath.Join(repoDir, "runtime-trace.ndjson")
	if err := os.WriteFile(tracePath, []byte(`{"from":"ValidateToken","to":"TestValidateToken","type":"OBSERVED_CALL"}`+"\n"), 0o600); err != nil {
		t.Fatalf("write runtime trace: %v", err)
	}
	if err := runSemanticIngestTraces(&cobra.Command{Use: "ingest"}, opts, semanticTraceIngestOptions{json: true}, tracePath); err != nil {
		t.Fatalf("ingest runtime trace: %v", err)
	}
	storage, err := repoStoragePaths((&cobra.Command{}).Context(), runner, env, repoDir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	sessionDir := filepath.Join(storage.BrainDir, exportSessionsDirectory, "main")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatalf("mkdir session dir: %v", err)
	}
	sessionLine := `{"type":"agent_message","message":"Decision: ValidateToken review default scope is mainline; --base <ref> is an explicit override and uncommitted changes are included in the prompt."}` + "\n"
	if err := os.WriteFile(filepath.Join(sessionDir, "session.jsonl"), []byte(sessionLine), 0o600); err != nil {
		t.Fatalf("write session: %v", err)
	}
	if _, err := writeBrainHistoryIndexAndSource(storage.BrainDir, now, nil); err != nil {
		t.Fatalf("write history index: %v", err)
	}
	runner.responses[fakeCommandKey("git", "status", "--porcelain")] = fakeCommandResponse{stdout: " M internal/auth/token.go\n?? notes.md\n"}
	runner.responses[fakeCommandKey("git", "status", "--porcelain", "--untracked-files=all")] = fakeCommandResponse{stdout: " M internal/auth/token.go\n?? notes.md\n"}
	runner.responses[fakeCommandKey("git", "diff", "--shortstat", "HEAD")] = fakeCommandResponse{stdout: " 1 file changed, 2 insertions(+)\n"}
	runner.responses[fakeCommandKey("git", "diff", "--name-status", "-M", "-C", "HEAD")] = fakeCommandResponse{stdout: "M\tinternal/auth/token.go\n"}

	cmd := NewRootCommand(opts)
	task := "ValidateToken"
	out, err := execute(t, cmd, "brief", task, "--json")
	if err != nil {
		t.Fatalf("brief: %v\n%s", err, out)
	}
	var report brainBriefReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse brief json: %v\n%s", err, out)
	}
	if report.Task != task {
		t.Fatalf("task = %q", report.Task)
	}
	if !report.Status.Sources.Semantic || !report.Status.Sources.History || !report.Status.Live.Dirty {
		t.Fatalf("brief did not report semantic source and dirty live state: %+v", report.Status)
	}
	if report.Status.Manifest != nil {
		t.Fatalf("brief JSON should not emit full manifest: %+v", report.Status.Manifest)
	}
	if got := report.Status.Live.ChangedFiles; len(got) != 2 || got[0] != "internal/auth/token.go" || got[1] != "notes.md" {
		t.Fatalf("changed files = %+v", got)
	}
	if len(report.Semantic.Context.Symbols) == 0 || report.Semantic.Context.Symbols[0].Name != "ValidateToken" {
		t.Fatalf("brief missing semantic context: %+v", report.Semantic.Context.Symbols)
	}
	if len(report.Semantic.RuntimeTraces) == 0 || report.Semantic.RuntimeTraces[0].Type != "RUNTIME_TRACE" {
		t.Fatalf("brief missing runtime trace context: %+v", report.Semantic.RuntimeTraces)
	}
	if report.Semantic.RuntimeTraces[0].FilePath != "internal/auth/token.go" {
		t.Fatalf("runtime trace did not carry source symbol file: %+v", report.Semantic.RuntimeTraces[0])
	}
	if len(report.History.Matches) == 0 || !strings.Contains(report.History.Matches[0].Excerpt, "mainline") {
		t.Fatalf("brief missing ranked history match: %+v", report.History.Matches)
	}
	foundLikelyFile := false
	for _, file := range report.LikelyEditFiles {
		if file == "internal/auth/token.go" {
			foundLikelyFile = true
			break
		}
	}
	if !foundLikelyFile {
		t.Fatalf("brief missing likely edit file hint: %+v", report.LikelyEditFiles)
	}
	if len(report.Guidance) == 0 || !strings.Contains(strings.Join(report.Guidance, "\n"), "indexed snapshot") {
		t.Fatalf("brief missing snapshot guidance: %+v", report.Guidance)
	}
}

func TestBrainBriefFactsCount(t *testing.T) {
	// Compact requests shrink the facts section; default/large requests cap it.
	cases := map[int]int{0: 1, 1: 1, 2: 2, 3: 3, 6: 6, 8: 6, 20: 6}
	for limit, want := range cases {
		if got := brainBriefFactsCount(limit); got != want {
			t.Errorf("brainBriefFactsCount(%d) = %d, want %d", limit, got, want)
		}
	}
}

func TestBrainBriefDefaultsToCompactPacketAndTargetsItsPublicSurface(t *testing.T) {
	if brainBriefDefaultLimit != 3 {
		t.Fatalf("brain brief default limit = %d, want 3", brainBriefDefaultLimit)
	}
	got := brainBriefSemanticQuery("Make brain brief compact for routine agent use")
	for _, want := range []string{"brain_brief", "brainBrief", "runBrainBrief"} {
		if !strings.Contains(got, want) {
			t.Fatalf("semantic query %q missing public-surface identifier %q", got, want)
		}
	}
	if !strings.HasPrefix(got, "brain_brief brainBrief runBrainBrief ") {
		t.Fatalf("public-surface identifiers must lead the bounded semantic query: %q", got)
	}
	plain := "ValidateToken behavior"
	if got := brainBriefSemanticQuery(plain); got != plain {
		t.Fatalf("unrelated semantic query changed: %q", got)
	}
}

func TestBrainBriefSemanticContextPrefersImplementationRootsAndReranksTests(t *testing.T) {
	noiseTest := semanticRecord{ID: "noise-test", Kind: "function", Name: "test_history_prompt", FilePath: "benchmarks/agent-brain/run_test.py"}
	field := semanticRecord{ID: "field", Kind: "field", Name: "details", FilePath: "internal/cli/semantic.go"}
	section := semanticRecord{ID: "section", Kind: "section", Name: "Compact semantic output", FilePath: "docs/semantic-memory.md"}
	noiseCommand := semanticRecord{ID: "dash", Kind: "function", Name: "runDash", FilePath: "internal/cli/dash.go"}
	implementation := semanticRecord{ID: "impl", Kind: "function", Name: "runBrainBrief", FilePath: "internal/cli/agent_surface.go"}
	projection := semanticRecord{ID: "projection", Kind: "function", Name: "compactSemanticRecords", FilePath: "internal/cli/agent_surface.go"}
	context := brainBriefSelectSemanticContext(
		[]semanticRecord{noiseTest, field, section, noiseCommand, implementation, projection}, nil, nil,
		"Improve brain brief semantic relevance", 2,
	)
	if len(context.Symbols) != 2 || context.Symbols[0].ID != "impl" || context.Symbols[1].ID != "projection" {
		t.Fatalf("implementation roots were displaced by test noise: %+v", context.Symbols)
	}
	tests := semanticTestsResult{Suggestions: []semanticTestSuggestion{
		{Symbol: noiseTest, Reason: "same directory"},
		{Symbol: semanticRecord{ID: "target-test", Kind: "function", Name: "TestBrainBriefJSONProjection", FilePath: "internal/cli/brain_test.go"}, Reason: "name terms"},
	}}
	selected := brainBriefSelectSemanticTests(tests, context, 2)
	if len(selected.Suggestions) != 1 || selected.Suggestions[0].Symbol.ID != "target-test" {
		t.Fatalf("test suggestions did not follow selected implementation roots: %+v", selected.Suggestions)
	}
}

func TestBrainBriefMergesImplementationImpactContext(t *testing.T) {
	root := semanticRecord{ID: "context-command", Kind: "function", Name: "newInspectContextCommand", FilePath: "internal/cli/agent_surface.go"}
	run := semanticRecord{ID: "run", Kind: "function", Name: "runSemanticContext", FilePath: "internal/cli/semantic.go"}
	field := semanticRecord{ID: "field", Kind: "field", Name: "details", FilePath: "internal/cli/semantic.go"}
	command := semanticRecord{ID: "command", Kind: "function", Name: "newBrainInspectCommand", FilePath: "internal/cli/agent_surface.go"}
	handler := semanticRecord{ID: "handler", Kind: "function", Name: "handleMCPToolCall", FilePath: "internal/cli/mcp.go"}
	projection := semanticRecord{ID: "projection", Kind: "function", Name: "compactSemanticRecords", FilePath: "internal/cli/agent_surface.go"}
	options := semanticRecord{ID: "options", Kind: "type", Name: "Options", FilePath: "internal/cli/root.go"}
	testSymbol := semanticRecord{ID: "test", Kind: "function", Name: "TestSemanticImpact", FilePath: "internal/cli/semantic_test.go"}
	relations := []semanticRecord{
		{RecordType: "relation", FromID: command.ID, ToID: root.ID, Type: "CALLS"},
		{RecordType: "relation", FromID: root.ID, ToID: run.ID, Type: "CALLS"},
		{RecordType: "relation", FromID: handler.ID, ToID: run.ID, Type: "CALLS"},
		{RecordType: "relation", FromID: handler.ID, ToID: run.ID, Type: "CALLS"},
		{RecordType: "relation", FromID: run.ID, ToID: projection.ID, Type: "CALLS"},
		{RecordType: "relation", FromID: root.ID, ToID: options.ID, Type: "USES_TYPE"},
		{RecordType: "relation", FromID: options.ID, ToID: field.ID, Type: "CONTAINS"},
	}

	context := brainBriefMergeImpactContext(
		semanticContextResult{Symbols: []semanticRecord{root}},
		[]semanticRecord{root, command, options, field, run, handler, projection, testSymbol},
		relations,
		"Make semantic context JSON compact with MCP plumbing", 3,
	)
	wantNeighbors := []string{"projection", "run", "handler"}
	if len(context.Neighbors) != len(wantNeighbors) {
		t.Fatalf("impact neighbors = %+v, want %v", context.Neighbors, wantNeighbors)
	}
	gotNeighbors := make([]string, 0, len(context.Neighbors))
	for _, neighbor := range context.Neighbors {
		gotNeighbors = append(gotNeighbors, neighbor.ID)
	}
	if !slices.Equal(gotNeighbors, wantNeighbors) {
		t.Fatalf("impact neighbors = %v, want %v", gotNeighbors, wantNeighbors)
	}
	if len(context.Relations) != 3 {
		t.Fatalf("impact relations = %+v, want the three selected implementation connections", context.Relations)
	}
	if context.Relations[0].Type != "CALLS" || context.Relations[1].Type != "CALLS" || context.Relations[2].Type != "CALLS" {
		t.Fatalf("call relations should lead lower-signal type relations: %+v", context.Relations)
	}
}

func TestBrainBriefLikelyFilesIncludeImpactNeighbors(t *testing.T) {
	report := brainBriefReport{Semantic: brainBriefSemantic{Context: semanticContextResult{
		Symbols: []semanticRecord{{ID: "root", Kind: "type", Name: "semanticImpactOptions", FilePath: "internal/cli/semantic.go"}},
		Neighbors: []semanticRecord{
			{ID: "command", Kind: "function", Name: "newInspectImpactCommand", FilePath: "internal/cli/agent_surface.go"},
			{ID: "handler", Kind: "function", Name: "handleMCPToolCall", FilePath: "internal/cli/mcp.go"},
		},
	}}}

	editFiles, _, _ := brainBriefLikelyFileGroups("", report, "semantic impact MCP details plumbing")
	for _, want := range []string{"internal/cli/semantic.go", "internal/cli/agent_surface.go", "internal/cli/mcp.go"} {
		if !slices.Contains(editFiles, want) {
			t.Fatalf("likely edit files %v missing graph-neighbor file %q", editFiles, want)
		}
	}
}

func TestBrainBriefTestIntentRequiresAnActualTestTask(t *testing.T) {
	for _, task := range []string{
		"Fix the failing integration test for brain status",
		"Which tests cover runSemanticImpact?",
		"Add a test fixture for semantic context",
	} {
		if !brainBriefTaskRequestsTests(task) {
			t.Errorf("test task was not recognized: %q", task)
		}
	}
	for _, task := range []string{
		"Review the compact semantic impact and test guidance change for code-level correctness",
		"Refactor the test suggestion implementation",
	} {
		if brainBriefTaskRequestsTests(task) {
			t.Errorf("implementation task was mistaken for test authoring: %q", task)
		}
	}
}

func TestBrainBriefJSONProjectionOmitsFollowUpDetail(t *testing.T) {
	status := brainBriefOutputStatus(brainStatusReport{
		Facts: &brainStatusFacts{Verification: &verifySummary{}},
		Semantic: &brainStatusSemantic{
			Coverage: &brainStatusSemanticCoverage{Files: 99},
		},
		Live: brainLiveState{
			ChangedFiles:       []string{"internal/cli/agent_surface.go"},
			ChangedSymbolHints: []semanticRecord{{ID: "symbol:huge", Blob: strings.Repeat("x", 1000)}},
		},
	})
	report := brainBriefReport{
		Task:   "compact",
		Status: status,
		Semantic: brainBriefSemantic{Context: semanticContextResult{
			Symbols: []semanticRecord{{ID: "symbol:brief", Name: "runBrainBrief", FilePath: "internal/cli/agent_surface.go", RecordType: "symbol", Blob: strings.Repeat("y", 1000)}},
		}},
		Facts: []factRecord{{ID: "fact:brief", Paths: []string{"product"}, Text: "Keep the packet compact.", Provenance: []factAnchor{{SessionID: "large-provenance"}}}},
	}
	data, err := json.Marshal(brainBriefJSONProjection(report))
	if err != nil {
		t.Fatal(err)
	}
	jsonText := string(data)
	for _, omitted := range []string{"changed_symbol_hints", "unstaged", "untracked", "coverage", "verification", "record_type", "provenance", "patterns", "consolidations", "themes", strings.Repeat("x", 100), strings.Repeat("y", 100)} {
		if strings.Contains(jsonText, omitted) {
			t.Fatalf("compact brief leaked follow-up detail %q: %s", omitted, jsonText)
		}
	}
	for _, kept := range []string{"changed_files", "internal/cli/agent_surface.go", "runBrainBrief", "Keep the packet compact."} {
		if !strings.Contains(jsonText, kept) {
			t.Fatalf("compact brief lost useful field %q: %s", kept, jsonText)
		}
	}
}

func TestBrainBriefIncludesMatchingFacts(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticBoundaryFixtureSnapshot())
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return now }}

	if err := runSemanticIndex((&cobra.Command{}).Context(), &cobra.Command{Use: "index"}, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	storage, err := repoStoragePaths((&cobra.Command{}).Context(), runner, env, repoDir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	// Seed a fact on the live branch (the fixture runner reports "feature")
	// and record it in the manifest. brief scopes facts to the current branch.
	paths := normalizeFactPaths([]string{"architecture.boundaries.rationale"})
	fact := factRecord{
		ID:        factRecordID("ValidateToken review default scope is mainline by design.", paths),
		Paths:     paths,
		Text:      "ValidateToken review default scope is mainline by design.",
		Branch:    "feature",
		Origin:    factOriginDistilled,
		Status:    factStatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := writeFacts(storage.BrainDir, "feature", []factRecord{fact}); err != nil {
		t.Fatalf("write facts: %v", err)
	}
	if err := updateFactSourceManifest(storage.BrainDir, now); err != nil {
		t.Fatalf("update manifest: %v", err)
	}
	runner.responses[fakeCommandKey("git", "diff", "--shortstat", "HEAD")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("git", "diff", "--name-status", "-M", "-C", "HEAD")] = fakeCommandResponse{}

	cmd := NewRootCommand(opts)
	out, err := execute(t, cmd, "brief", "ValidateToken", "--json")
	if err != nil {
		t.Fatalf("brief: %v\n%s", err, out)
	}
	var report brainBriefReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse brief json: %v\n%s", err, out)
	}
	if !report.Status.Sources.Facts {
		t.Fatalf("brief should report the facts source: %+v", report.Status.Sources)
	}
	if len(report.Facts) == 0 || report.Facts[0].ID != fact.ID {
		t.Fatalf("brief should include the matching fact, got %+v", report.Facts)
	}
}

func TestBrainBriefAnnotatesPendingFactReview(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticBoundaryFixtureSnapshot())
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return now }}

	if err := runSemanticIndex((&cobra.Command{}).Context(), &cobra.Command{Use: "index"}, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	storage, err := repoStoragePaths((&cobra.Command{}).Context(), runner, env, repoDir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	// Two facts on the live branch ("feature") linked by a pending supersede
	// proposal: the brief must carry the same verify-before-trust signal the
	// unified query/search/get path applies.
	facts := []factRecord{
		{ID: "fact:new", Text: "ValidateToken review default scope is mainline by design.", Paths: normalizeFactPaths([]string{"architecture.boundaries.rationale"}), Branch: "feature", Origin: factOriginDistilled, Status: factStatusActive, CreatedAt: now, UpdatedAt: now},
		{ID: "fact:old", Text: "ValidateToken review default scope is all branches.", Paths: normalizeFactPaths([]string{"architecture.boundaries.rationale"}), Branch: "feature", Origin: factOriginDistilled, Status: factStatusActive, CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour)},
	}
	if err := writeFacts(storage.BrainDir, "feature", facts); err != nil {
		t.Fatalf("write facts: %v", err)
	}
	if err := writeFactProposals(storage.BrainDir, "feature", []factProposal{{Action: factActionSupersede, CandidateID: "fact:new", TargetID: "fact:old", Confidence: 0.62, Branch: "feature"}}); err != nil {
		t.Fatalf("write proposals: %v", err)
	}
	if err := updateFactSourceManifest(storage.BrainDir, now); err != nil {
		t.Fatalf("update manifest: %v", err)
	}
	runner.responses[fakeCommandKey("git", "diff", "--shortstat", "HEAD")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("git", "diff", "--name-status", "-M", "-C", "HEAD")] = fakeCommandResponse{}

	out, err := execute(t, NewRootCommand(opts), "brief", "ValidateToken", "--json")
	if err != nil {
		t.Fatalf("brief: %v\n%s", err, out)
	}
	var report brainBriefReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse brief json: %v\n%s", err, out)
	}
	if len(report.Facts) == 0 {
		t.Fatalf("brief should include the matching facts, got %+v", report.Facts)
	}
	annotated := 0
	for _, fact := range report.Facts {
		notice, ok := report.FactsPendingReview[fact.ID]
		if !ok {
			continue
		}
		annotated++
		if !strings.HasPrefix(notice.ReviewID, "review:") || notice.Action != factActionSupersede {
			t.Fatalf("notice for %s lost proposal identity: %+v", fact.ID, notice)
		}
	}
	if annotated == 0 {
		t.Fatalf("brief omitted the pending-review trust annotation: facts=%+v pending=%+v", report.Facts, report.FactsPendingReview)
	}

	textOut, err := execute(t, NewRootCommand(opts), "brief", "ValidateToken")
	if err != nil {
		t.Fatalf("brief text: %v\n%s", err, textOut)
	}
	if !strings.Contains(textOut, "pending fact review review:") {
		t.Fatalf("text brief omitted the pending-review line:\n%s", textOut)
	}
}

func TestBrainBriefWarnsWhenProposalQueueUnreadable(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticBoundaryFixtureSnapshot())
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return now }}

	if err := runSemanticIndex((&cobra.Command{}).Context(), &cobra.Command{Use: "index"}, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	storage, err := repoStoragePaths((&cobra.Command{}).Context(), runner, env, repoDir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	fact := factRecord{ID: "fact:a", Text: "ValidateToken review default scope is mainline by design.", Paths: normalizeFactPaths([]string{"architecture.boundaries.rationale"}), Branch: "feature", Origin: factOriginDistilled, Status: factStatusActive, CreatedAt: now, UpdatedAt: now}
	if err := writeFacts(storage.BrainDir, "feature", []factRecord{fact}); err != nil {
		t.Fatalf("write facts: %v", err)
	}
	// A malformed proposal queue must degrade the brief with a verify warning,
	// not fail it and not drop the facts — the same contract recall enforces in
	// TestRecallWarnsWhenProposalQueueUnreadable.
	proposalPath := filepath.Join(storage.BrainDir, filepath.FromSlash(factsProposalsRelPath("feature")))
	if err := os.WriteFile(proposalPath, []byte("{not-json}\n"), 0o600); err != nil {
		t.Fatalf("write malformed proposals: %v", err)
	}
	if err := updateFactSourceManifest(storage.BrainDir, now); err != nil {
		t.Fatalf("update manifest: %v", err)
	}
	runner.responses[fakeCommandKey("git", "diff", "--shortstat", "HEAD")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("git", "diff", "--name-status", "-M", "-C", "HEAD")] = fakeCommandResponse{}

	out, err := execute(t, NewRootCommand(opts), "brief", "ValidateToken", "--json")
	if err != nil {
		t.Fatalf("brief must degrade with a warning, not fail: %v\n%s", err, out)
	}
	var report brainBriefReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse brief json: %v\n%s", err, out)
	}
	if len(report.Facts) == 0 {
		t.Fatalf("brief must still return facts when the queue is unreadable: %+v", report.Facts)
	}
	found := false
	for _, w := range report.Warnings {
		if w == factReviewQueueUnavailableWarning {
			found = true
		}
	}
	if !found {
		t.Fatalf("unreadable queue must surface the verify warning: %v", report.Warnings)
	}

	textOut, err := execute(t, NewRootCommand(opts), "brief", "ValidateToken")
	if err != nil {
		t.Fatalf("brief text: %v\n%s", err, textOut)
	}
	if !strings.Contains(textOut, factReviewQueueUnavailableWarning) {
		t.Fatalf("text brief omitted the queue-unreadable warning:\n%s", textOut)
	}
}

func TestBrainInspectCodeAndShow(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	opts := Options{Version: "test", Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex((&cobra.Command{}).Context(), &cobra.Command{Use: "index"}, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	runner.responses[fakeCommandKey("git", "diff", "--shortstat", "HEAD")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("git", "diff", "--name-status", "-M", "-C", "HEAD")] = fakeCommandResponse{}

	cmd := NewRootCommand(opts)
	out, err := execute(t, cmd, "inspect", "code", "ValidateToken", "--json")
	if err != nil {
		t.Fatalf("inspect code: %v\n%s", err, out)
	}
	if !strings.Contains(out, `"name": "ValidateToken"`) {
		t.Fatalf("inspect code output missing symbol:\n%s", out)
	}

	cmd = NewRootCommand(opts)
	out, err = execute(t, cmd, "show", "auth.ValidateToken", "--json")
	if err != nil {
		t.Fatalf("show: %v\n%s", err, out)
	}
	if !strings.Contains(out, `"qualified_name": "auth.ValidateToken"`) {
		t.Fatalf("show output missing record:\n%s", out)
	}

	cmd = NewRootCommand(opts)
	out, err = execute(t, cmd, "inspect", "code", "ValidateToken", "--json")
	if err != nil {
		t.Fatalf("inspect code: %v\n%s", err, out)
	}
	if !strings.Contains(out, `"name": "ValidateToken"`) {
		t.Fatalf("inspect code output missing symbol:\n%s", out)
	}
}

func TestBrainInspectDecisionsSearchesExportedText(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	storage, err := repoStoragePaths((&cobra.Command{}).Context(), runner, env, repoDir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(storage.BrainDir, exportSessionsDirectory, "main"), 0o700); err != nil {
		t.Fatalf("mkdir brain: %v", err)
	}
	if err := os.WriteFile(filepath.Join(storage.BrainDir, exportSessionsDirectory, "main", "session.jsonl"), []byte(`{"type":"agent_message","message":"Decision: keep prompt plus local validation instead of output schema."}`+"\n"), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	if _, err := writeBrainHistoryIndexAndSource(storage.BrainDir, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), nil); err != nil {
		t.Fatalf("write history index: %v", err)
	}
	manifest, err := loadBrainManifest(storage.BrainDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if manifest.Sources == nil || manifest.Sources.History == nil || manifest.Sources.History.Decisions != 1 {
		t.Fatalf("history source missing decision count: %+v", manifest.Sources)
	}

	// The indexed decision is retrievable by the shared history ranker (the engine
	// the unified search/query verbs use); the old `inspect decisions` command was
	// just a thin wrapper over this and was removed.
	index, err := loadBrainHistoryIndex(storage.BrainDir, manifest.Sources.History)
	if err != nil {
		t.Fatalf("load history index: %v", err)
	}
	scored, ok := rankHistoryViaFTS(storage.BrainDir, index, "decisions", "output schema", 25)
	if !ok || len(scored) != 1 || !strings.Contains(scored[0].Record.Summary, "Decision:") {
		t.Fatalf("unexpected matches (ok=%v): %+v", ok, scored)
	}
}

func TestBrainInspectParsesStructuredSessionHistory(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	storage, err := repoStoragePaths((&cobra.Command{}).Context(), runner, env, repoDir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	sessionDir := filepath.Join(storage.BrainDir, exportSessionsDirectory, "main")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatalf("mkdir brain: %v", err)
	}
	events := []map[string]any{
		{
			"type": "session_meta",
			"payload": map[string]any{
				"base_instructions": "Decision: fake metadata should not be indexed.",
			},
		},
		{
			"type": "response_item",
			"payload": map[string]any{
				"type": "message",
				"role": "assistant",
				"content": []map[string]any{{
					"type": "text",
					"text": "Implemented the manual_commit AttributionBaseCommit invariant because condensation must keep attribution stable.",
				}},
			},
		},
		{
			"type": "response_item",
			"payload": map[string]any{
				"type": "message",
				"role": "assistant",
				"content": []map[string]any{{
					"type": "text",
					"text": "Implemented the manual_commit AttributionBaseCommit invariant because condensation must keep attribution stable.",
				}},
			},
		},
		{
			"type": "response_item",
			"payload": map[string]any{
				"type": "message",
				"role": "assistant",
				"content": []map[string]any{{
					"type": "text",
					"text": "I'll treat the exported logs as the source of truth before reading current code.",
				}},
			},
		},
		{
			"type": "response_item",
			"payload": map[string]any{
				"type": "message",
				"role": "assistant",
				"content": []map[string]any{{
					"type": "text",
					"text": "Updated the parser because the old output was noisy.",
				}},
			},
		},
		{
			"type": "user",
			"message": map[string]any{
				"role": "user",
				"content": []map[string]any{{
					"type": "tool_result",
					"content": strings.Join([]string{
						"state.BaseCommit = newHead",
						"// Keep AttributionBaseCommit in sync to prevent stale base drift.",
						"// Without this, a subsequent condensation would diff from the old base,",
						"// inflating human_added with lines from unrelated prior commits.",
						"state.RealignAttributionBase(newHead)",
					}, "\n"),
				}},
			},
		},
		{
			"type": "user",
			"message": map[string]any{
				"role": "user",
				"content": []map[string]any{{
					"type": "tool_result",
					"content": strings.Join([]string{
						"func resolveTranscriptPath(state *SessionState) (string, error) {",
						"\t// Update state so subsequent reads use the correct path.",
						"\tstate.TranscriptPath = resolved",
						"\treturn resolved, nil",
						"}",
					}, "\n"),
				}},
			},
		},
		{
			"type": "response_item",
			"payload": map[string]any{
				"type":      "function_call",
				"name":      "exec_command",
				"arguments": map[string]any{"cmd": "go test ./cmd/entire/cli/strategy"},
			},
		},
		{
			"type": "response_item",
			"payload": map[string]any{
				"type":  "custom_tool_call",
				"name":  "apply_patch",
				"input": "*** Begin Patch\n*** Update File: README.md\n+go test ./...\n",
			},
		},
	}
	var transcript strings.Builder
	for _, event := range events {
		data, err := json.Marshal(event)
		if err != nil {
			t.Fatalf("marshal event: %v", err)
		}
		transcript.Write(data)
		transcript.WriteByte('\n')
	}
	transcript.WriteString(`  "output": "Decision: fake pretty JSON output should not be indexed."` + "\n")
	if err := os.WriteFile(filepath.Join(sessionDir, "session.jsonl"), []byte(transcript.String()), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	source, err := writeBrainHistoryIndexAndSource(storage.BrainDir, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("write history index: %v", err)
	}
	if source.Decisions != 1 || source.Validations != 1 || source.ToolCalls != 2 {
		t.Fatalf("unexpected history source counts: %+v", source)
	}
	if source.CodeFacts != 2 {
		t.Fatalf("unexpected code fact count: %+v", source)
	}

	// Kind-filtered retrieval over these parsed records is exercised against the
	// shared BM25 ranker in history_fts_test.go; the per-kind `inspect` commands
	// that used to drive it here were removed in favor of the unified verbs.
	index, err := loadBrainHistoryIndex(storage.BrainDir, source)
	if err != nil {
		t.Fatalf("load history index: %v", err)
	}
	if scored, ok := rankHistoryViaFTS(storage.BrainDir, index, "decisions", "manual commit attribution", 25); !ok || len(scored) == 0 || !strings.Contains(scored[0].Record.Summary, "AttributionBaseCommit") {
		t.Fatalf("decision record not retrievable (ok=%v): %+v", ok, scored)
	}
}

func TestRankHistoryRecordsUsesIdentifierTerms(t *testing.T) {
	index := historyIndex{Records: []historyRecord{
		{
			Kind:    "decision",
			Path:    "sessions/main/generic.jsonl",
			Line:    10,
			Summary: "Final state: drop stale Strip references in review scanner tests.",
		},
		{
			Kind:    "tool_call",
			Path:    "sessions/main/review-env.jsonl",
			Line:    20,
			Summary: "AppendReviewEnv strips stale ENTIRE_REVIEW_* and ENTIRE_INVESTIGATE_* entries before appending fresh values.",
		},
	}}

	records := rankHistoryRecords(index, "history", "Strip stale Entire review provenance before setting fresh ENTIRE_REVIEW_* values.", 2)
	if len(records) == 0 {
		t.Fatal("expected ranked history records")
	}
	if records[0].Path != "sessions/main/review-env.jsonl" {
		t.Fatalf("identifier-specific record did not rank first: %+v", records)
	}

	records = rankHistoryRecords(index, "history", "ENTIRE_REVIEW_*", 2)
	if len(records) == 0 || records[0].Path != "sessions/main/review-env.jsonl" {
		t.Fatalf("identifier-only query did not rank env record first: %+v", records)
	}
}

func TestRankHistoryRecordsPrefersPreciseCodeFactsOverSetupLogs(t *testing.T) {
	index := historyIndex{Records: []historyRecord{
		{
			Kind: "decision",
			Path: "sessions/main/setup.jsonl",
			Line: 10,
			Summary: "You are the main coding agent. Start by running: entire status --detailed, entire doctor, entire session current, entire checkpoint list. " +
				"Then inspect repository query limit normalization in the storage contract.",
		},
		{
			Kind: "code_fact",
			Path: "sessions/main/storage-fix.jsonl",
			Line: 20,
			Summary: "diff --git a/packages/storage/src/index.ts b/packages/storage/src/index.ts\n" +
				"+const MAX_QUERY_LIMIT = 1_000;\n" +
				"+const normalizeLimit = (value: number | undefined, fallback: number): number => Math.min(value, MAX_QUERY_LIMIT);\n" +
				"+    const limit = normalizeLimit(filter.limit, 250);",
		},
	}}

	records := rankHistoryRecords(index, "history", "repository query limit normalization normalizeLimit MAX_QUERY_LIMIT", 2)
	if len(records) == 0 {
		t.Fatal("expected ranked history records")
	}
	if records[0].Path != "sessions/main/storage-fix.jsonl" {
		t.Fatalf("precise code fact did not rank first: %+v", records)
	}

	identifiers := historyIdentifierQueryTerms("normalizeLimit MAX_QUERY_LIMIT")
	if !slices.Contains(identifiers, "NORMALIZELIMIT") || !slices.Contains(identifiers, "MAX_QUERY_LIMIT") {
		t.Fatalf("identifier extraction missed camelCase or constant: %+v", identifiers)
	}
}

func TestBrainBriefRawHistoryQueriesPrioritizeLimitIdentifiers(t *testing.T) {
	queries := brainBriefRawHistoryQueries("Restore ULTRON storage APIs. repository query limit normalization, normalizeLimit, MAX_QUERY_LIMIT, listNodes searchNodes listAutomationActions")
	if len(queries) < 2 {
		t.Fatalf("expected focused raw history queries: %+v", queries)
	}
	if queries[0] != "MAX_QUERY_LIMIT" || queries[1] != "NORMALIZELIMIT" {
		t.Fatalf("limit identifiers should rank first: %+v", queries)
	}
	if slices.Contains(queries, "ULTRON") || slices.Contains(queries, "APIS") {
		t.Fatalf("generic identifiers should be filtered: %+v", queries)
	}
}

func TestBrainBriefActionChecklistUsesHistoryAndCurrentLimitCode(t *testing.T) {
	repoDir := t.TempDir()
	sourcePath := filepath.Join(repoDir, "packages", "storage", "src", "index.ts")
	if err := os.MkdirAll(filepath.Dir(sourcePath), 0o700); err != nil {
		t.Fatalf("mkdir source dir: %v", err)
	}
	source := `
export class SqliteMemoryRepository {
  async listNodes(filter = {}) {
    const limit = filter.limit ?? 250;
    return this.db.prepare("SELECT * FROM memory_nodes LIMIT ?").all(limit);
  }

  async listChatMessages(chatId: string, limit = 200) {
    return this.db.prepare("SELECT * FROM chat_messages WHERE chat_id = ? LIMIT ?").all(chatId, limit);
  }

  async listAutomationActions(sessionId: string, limit = 500) {
    return this.pool.query("SELECT * FROM automation_actions WHERE session_id = $1 LIMIT $2", [sessionId, limit]);
  }
}

func TestBrainBriefActionChecklistDoesNotLetUnrelatedHistoryInventTaskIntent(t *testing.T) {
	repoDir := t.TempDir()
	sourcePath := filepath.Join(repoDir, "internal", "cli", "semantic.go")
	if err := os.MkdirAll(filepath.Dir(sourcePath), 0o700); err != nil {
		t.Fatalf("mkdir source dir: %v", err)
	}
	if err := os.WriteFile(sourcePath, []byte("func query(limit int) { _ = limit }\n"), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	report := brainBriefReport{
		History: brainBriefHistory{Matches: []brainTextMatch{{
			Excerpt: "Use normalizeLimit and MAX_QUERY_LIMIT before every SQL LIMIT.",
		}}},
		LikelyEditFiles: []string{"internal/cli/semantic.go"},
	}
	actions := brainBriefActionChecklist(repoDir, report, "Improve brain brief semantic relevance for its default three records")
	if len(actions) != 0 {
		t.Fatalf("unrelated limit history invented checklist actions: %+v", actions)
	}
}
`
	if err := os.WriteFile(sourcePath, []byte(source), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	report := brainBriefReport{
		History: brainBriefHistory{Matches: []brainTextMatch{{
			Excerpt: "diff --git a/packages/storage/src/index.ts b/packages/storage/src/index.ts +const MAX_QUERY_LIMIT = 1_000; +const normalizeLimit = (value, fallback) => Math.min(value, MAX_QUERY_LIMIT);",
		}}},
		LikelyEditFiles: []string{"packages/storage/src/index.ts"},
	}
	actions := brainBriefActionChecklist(repoDir, report, "repository query limit normalization")
	joined := strings.Join(func() []string {
		out := make([]string, 0, len(actions))
		for _, action := range actions {
			out = append(out, action.Symbol+" "+action.Action+" "+action.Evidence)
		}
		return out
	}(), "\n")
	if !strings.Contains(joined, "listNodes") || !strings.Contains(joined, "filter limit") {
		t.Fatalf("missing filter-limit action: %+v", actions)
	}
	if !strings.Contains(joined, "listChatMessages") || !strings.Contains(joined, "SQL LIMIT path") {
		t.Fatalf("missing sibling SQL LIMIT action: %+v", actions)
	}
	if !strings.Contains(joined, "listAutomationActions") || !strings.Contains(joined, "raw limit query argument") {
		t.Fatalf("missing raw array limit action: %+v", actions)
	}
}

func TestBrainBriefActionChecklistSkipsSymlinkedOutsideLikelyFile(t *testing.T) {
	repoDir := t.TempDir()
	outsidePath := filepath.Join(t.TempDir(), "index.ts")
	outsideSource := `
export class SqliteMemoryRepository {
  async listNodes(filter = {}) {
    const limit = filter.limit ?? 250;
    return this.db.prepare("SELECT * FROM memory_nodes LIMIT ?").all(limit);
  }
}
`
	if err := os.WriteFile(outsidePath, []byte(outsideSource), 0o600); err != nil {
		t.Fatalf("write outside source: %v", err)
	}
	linkPath := filepath.Join(repoDir, "packages", "storage", "src", "index.ts")
	if err := os.MkdirAll(filepath.Dir(linkPath), 0o700); err != nil {
		t.Fatalf("mkdir link dir: %v", err)
	}
	if err := os.Symlink(outsidePath, linkPath); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	report := brainBriefReport{LikelyEditFiles: []string{"packages/storage/src/index.ts"}}
	actions := brainBriefActionChecklist(repoDir, report, "repository query limit normalization")
	if len(actions) != 0 {
		t.Fatalf("outside symlink content should not be surfaced in actions: %+v", actions)
	}
}

func TestBrainBriefLikelyFilesExtractsDotSlashHistoryPaths(t *testing.T) {
	report := brainBriefReport{
		History: brainBriefHistory{Matches: []brainTextMatch{{
			Excerpt: "tool output: ./packages/storage/src/index.ts: listAgentMessages(channelId: string, limit?: number): Promise<AgentMessage[]>;",
		}}},
	}
	editFiles, _, _ := brainBriefLikelyFileGroups("", report, "")
	if !slices.Contains(editFiles, "packages/storage/src/index.ts") {
		t.Fatalf("expected packages/storage/src/index.ts from history excerpt, got %+v", editFiles)
	}
}

func TestBrainBriefTaskTermBonusFavorsBasenameMatch(t *testing.T) {
	// The task-term ranking (experimental, post-campaign) must score a file whose
	// BASENAME matches the task terms above one that only matches in the path, so the
	// true target outranks a large distractor. Generic stopwords must not match.
	terms := brainBriefFileMatchTerms("how does review checkpoint context choose the base ref")
	if slices.Contains(terms, "how") || slices.Contains(terms, "does") || slices.Contains(terms, "the") {
		t.Fatalf("generic stopwords leaked into match terms: %v", terms)
	}
	if !slices.Contains(terms, "review") || !slices.Contains(terms, "context") {
		t.Fatalf("meaningful terms missing: %v", terms)
	}
	target := brainBriefTaskTermBonus("cmd/entire/cli/review_context.go", terms) // basename: review+context
	distractor := brainBriefTaskTermBonus("cmd/entire/cli/review/tui_model.go", terms)
	if target <= distractor {
		t.Fatalf("expected basename match (review_context.go=%d) to outrank path-only match (tui_model.go=%d)", target, distractor)
	}
	if brainBriefTaskTermBonus("internal/unrelated/foo.go", terms) != 0 {
		t.Fatalf("unrelated file should get 0 task-term bonus")
	}
}

func TestBrainBriefCurrentCodeFileCountsFindsProviderMetadataContractFile(t *testing.T) {
	repoDir := t.TempDir()
	target := filepath.Join(repoDir, "apps", "desktop", "src", "main", "agentic-decider.ts")
	noise := filepath.Join(repoDir, "apps", "desktop", "src", "renderer", "components", "demoApi.ts")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(noise), 0o700); err != nil {
		t.Fatalf("mkdir noise: %v", err)
	}
	if err := os.WriteFile(target, []byte(`export function createAgenticDecider(llm) {
  const reasoningEffort = "low";
  return llm.generate([], {
    reasoningEffort,
    metadata: { purpose: "agentic_decision", step: perception.stepIndex },
    previousResponseId: perception.previousResponseId
  });
}
`), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.WriteFile(noise, []byte(`export const demo = { metadata: { preview: true } };
`), 0o600); err != nil {
		t.Fatalf("write noise: %v", err)
	}
	counts := brainBriefCurrentCodeFileCounts(repoDir, "Responses API metadata values must be strings for the agentic decider provider contract")
	if counts["apps/desktop/src/main/agentic-decider.ts"] <= counts["apps/desktop/src/renderer/components/demoApi.ts"] {
		t.Fatalf("expected decider to outrank demo metadata: %+v", counts)
	}
	if _, ok := counts["apps/desktop/src/renderer/components/demoApi.ts"]; ok {
		t.Fatalf("generic preview metadata should not be included: %+v", counts)
	}
	actions := brainBriefMetadataStringActions(repoDir, []string{
		"apps/desktop/src/main/agentic-decider.ts",
		"apps/desktop/src/renderer/components/demoApi.ts",
	})
	if len(actions) != 1 {
		t.Fatalf("expected only primary decider metadata action, got %+v", actions)
	}
	if actions[0].File != "apps/desktop/src/main/agentic-decider.ts" ||
		!strings.Contains(actions[0].Action, "Stringify the agentic_decision metadata step") {
		t.Fatalf("unexpected primary action: %+v", actions[0])
	}
}

func TestBrainBriefCurrentCodeFileCountsSkipsSymlinkedOutsideFile(t *testing.T) {
	repoDir := t.TempDir()
	outsidePath := filepath.Join(t.TempDir(), "agentic-decider.ts")
	outsideSource := `export function createAgenticDecider(llm) {
  const reasoningEffort = "low";
  return llm.generate([], {
    reasoningEffort,
    metadata: { purpose: "agentic_decision", step: perception.stepIndex },
    previousResponseId: perception.previousResponseId
  });
}
`
	if err := os.WriteFile(outsidePath, []byte(outsideSource), 0o600); err != nil {
		t.Fatalf("write outside source: %v", err)
	}
	linkPath := filepath.Join(repoDir, "apps", "desktop", "src", "main", "agentic-decider.ts")
	if err := os.MkdirAll(filepath.Dir(linkPath), 0o700); err != nil {
		t.Fatalf("mkdir link dir: %v", err)
	}
	if err := os.Symlink(outsidePath, linkPath); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	counts := brainBriefCurrentCodeFileCounts(repoDir, "Responses API metadata values must be strings for the agentic decider provider contract")
	if _, ok := counts["apps/desktop/src/main/agentic-decider.ts"]; ok {
		t.Fatalf("outside symlink content should not be counted: %+v", counts)
	}
}

func TestBrainBriefActionChecklistFindsSelfContainedAgenticPerception(t *testing.T) {
	repoDir := t.TempDir()
	target := filepath.Join(repoDir, "packages", "automation", "src", "index.ts")
	noise := filepath.Join(repoDir, "apps", "desktop", "src", "main", "agentic-decider.ts")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(noise), 0o700); err != nil {
		t.Fatalf("mkdir noise: %v", err)
	}
	if err := os.WriteFile(target, []byte(`export class AutomationRuntime {
  private async executeAgenticPlan() {
    let previousResponseId: string | null = null;
    const perception: AgenticPerception = {
      goal,
      history: [],
      previousResponseId
    };
    previousResponseId = decision.previousResponseId ?? previousResponseId;
  }
}
`), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.WriteFile(noise, []byte(`export const createAgenticDecider = () => ({
  decide(perception) {
    return { previousResponseId: perception.previousResponseId };
  }
});
`), 0o600); err != nil {
		t.Fatalf("write noise: %v", err)
	}
	report := brainBriefReport{
		LikelyEditFiles: []string{
			"apps/desktop/src/main/agentic-decider.ts",
			"packages/automation/src/index.ts",
		},
	}
	counts := brainBriefCurrentCodeFileCounts(repoDir, "browser decision turns must stay self-contained after stale model state")
	if counts["packages/automation/src/index.ts"] <= 0 {
		t.Fatalf("expected automation file current-code count, got %+v", counts)
	}
	if _, ok := counts["apps/desktop/src/main/agentic-decider.ts"]; ok {
		t.Fatalf("decider should not be a primary previous-response current-code target: %+v", counts)
	}
	actions := brainBriefActionChecklist(repoDir, report, "self-contained perception stopped chaining previous_response_id")
	if len(actions) != 3 {
		t.Fatalf("expected three primary automation actions, got %+v", actions)
	}
	var sawAccumulator, sawPerception, sawAssignment bool
	for _, action := range actions {
		if action.File != "packages/automation/src/index.ts" {
			t.Fatalf("unexpected previous-response action file: %+v", action)
		}
		sawAccumulator = sawAccumulator || strings.Contains(action.Action, "response-id accumulator")
		sawPerception = sawPerception ||
			(strings.Contains(action.Action, "previousResponseId: null") &&
				strings.Contains(action.Action, "Keep the `previousResponseId` key present") &&
				strings.Contains(action.Action, "do not delete it"))
		sawAssignment = sawAssignment || strings.Contains(action.Action, "Delete this browser-loop response-id carry-forward assignment")
	}
	if !sawAccumulator || !sawPerception || !sawAssignment {
		t.Fatalf("missing previous-response actions: %+v", actions)
	}
}

func TestBrainBriefAddsSiblingTestFiles(t *testing.T) {
	repoDir := t.TempDir()
	testPath := filepath.Join(repoDir, "packages", "storage", "src", "index.test.ts")
	if err := os.MkdirAll(filepath.Dir(testPath), 0o700); err != nil {
		t.Fatalf("mkdir test dir: %v", err)
	}
	if err := os.WriteFile(testPath, []byte("test('storage', () => {})\n"), 0o600); err != nil {
		t.Fatalf("write test: %v", err)
	}
	tests := brainBriefAddSiblingTestFiles(repoDir, []string{"packages/storage/src/index.ts"}, nil)
	if !slices.Contains(tests, "packages/storage/src/index.test.ts") {
		t.Fatalf("expected sibling test file, got %+v", tests)
	}
}

func TestBrainBriefAddsSiblingTestFilesSkipsSymlinkedOutsideFile(t *testing.T) {
	repoDir := t.TempDir()
	outsidePath := filepath.Join(t.TempDir(), "index.test.ts")
	if err := os.WriteFile(outsidePath, []byte("test('outside', () => {})\n"), 0o600); err != nil {
		t.Fatalf("write outside test: %v", err)
	}
	linkPath := filepath.Join(repoDir, "packages", "storage", "src", "index.test.ts")
	if err := os.MkdirAll(filepath.Dir(linkPath), 0o700); err != nil {
		t.Fatalf("mkdir link dir: %v", err)
	}
	if err := os.Symlink(outsidePath, linkPath); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	tests := brainBriefAddSiblingTestFiles(repoDir, []string{"packages/storage/src/index.ts"}, nil)
	if slices.Contains(tests, "packages/storage/src/index.test.ts") {
		t.Fatalf("outside symlink test file should not be surfaced: %+v", tests)
	}
}

func TestLoadBrainManifestMigratesLegacyFlatManifest(t *testing.T) {
	outputDir := t.TempDir()
	legacy := exportManifest{
		SchemaVersion:      1,
		GeneratedAt:        time.Date(2026, 1, 2, 1, 0, 0, 0, time.UTC),
		TranscriptMode:     "raw",
		Scope:              exportScopeBranch,
		CheckpointLimit:    5,
		CheckpointsScanned: 1,
		Sessions: []exportSession{{
			SessionID:        "session-one",
			LatestCheckpoint: "aaa111aaa111",
			CreatedAt:        time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
			TranscriptPath:   "sessions/main/session.jsonl",
		}},
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshal legacy: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outputDir, exportManifestFileName), data, 0o600); err != nil {
		t.Fatalf("write legacy: %v", err)
	}

	manifest, err := loadBrainManifest(outputDir)
	if err != nil {
		t.Fatalf("load legacy: %v", err)
	}
	if manifest.Sources == nil || manifest.Sources.Sessions == nil {
		t.Fatalf("legacy manifest did not get session source: %+v", manifest)
	}
	if manifest.Sources.Sessions.TranscriptMode != "raw" {
		t.Fatalf("transcript mode = %q, want raw", manifest.Sources.Sessions.TranscriptMode)
	}
}
