package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSemanticTestSuggestionRankingFindsLateStrongCandidateAfterSaturation(t *testing.T) {
	root := semanticRecord{
		ID: "source:token", Name: "ValidateToken", QualifiedName: "auth.ValidateToken",
		FilePath: "internal/auth/token.go",
	}
	symbols := make(map[string]semanticRecord)
	// This large file and the lexicographically early distractors reproduce the
	// old first-N failure mode: the useful candidate sorts after a saturated
	// prefix of same-directory tests.
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("test:giant:%02d", i)
		symbols[id] = semanticTestSuggestionFixture(id, fmt.Sprintf("TestUnrelated%02d", i), "internal/auth/a_giant_test.go", i+1)
	}
	for i := 0; i < 6; i++ {
		id := fmt.Sprintf("test:distractor:%02d", i)
		path := fmt.Sprintf("internal/auth/b_distractor_%02d_test.go", i)
		symbols[id] = semanticTestSuggestionFixture(id, fmt.Sprintf("TestOther%02d", i), path, 1)
	}
	const strongID = "test:token-checksum"
	symbols[strongID] = semanticTestSuggestionFixture(
		strongID,
		"TestValidateTokenChecksum",
		"internal/auth/z_token_checksum_test.go",
		1,
	)

	got := rankSemanticTestSuggestions(symbols, []semanticRecord{root}, nil, "repair token checksum validation", 4)
	if len(got) != 4 {
		t.Fatalf("ranked suggestions = %d, want a full reservoir of 4: %+v", len(got), got)
	}
	if got[0].Symbol.ID != strongID || got[0].Reason != "task term match" || got[0].weak {
		t.Fatalf("late strong candidate rank = %+v, want strong task match at rank 1", got[0])
	}
	perFile := map[string]int{}
	for _, suggestion := range got {
		perFile[suggestion.Symbol.FilePath]++
		if perFile[suggestion.Symbol.FilePath] > semanticTestSuggestionPerFileLimit {
			t.Fatalf("file %q monopolized bounded reservoir: %+v", suggestion.Symbol.FilePath, got)
		}
	}

	visible := visibleSemanticTestSuggestions(got, 4)
	if len(visible) != 1 || visible[0].Symbol.ID != strongID {
		t.Fatalf("visible suggestions = %+v, want only the refilled strong candidate", visible)
	}
}

func TestSemanticTestSuggestionRankingPreservesStrongEvidenceClasses(t *testing.T) {
	root := semanticRecord{
		ID: "source:token", Name: "ValidateToken", QualifiedName: "auth.ValidateToken",
		FilePath: "internal/auth/token.go",
	}
	exact := semanticTestSuggestionFixture("test:exact", "TestChecksumRepair", "internal/auth/token_test.go", 10)
	related := semanticTestSuggestionFixture("test:related", "TestChecksumEdge", "integration/edge_test.go", 20)
	nameAndTask := semanticTestSuggestionFixture("test:name", "TestValidateTokenChecksum", "integration/token_cases_test.go", 30)
	alignedSameDir := semanticTestSuggestionFixture("test:aligned", "TestChecksum", "internal/auth/helpers_test.go", 40)
	weakSameDir := semanticTestSuggestionFixture("test:weak", "TestOther", "internal/auth/other_test.go", 50)
	symbols := map[string]semanticRecord{
		exact.ID:          exact,
		related.ID:        related,
		nameAndTask.ID:    nameAndTask,
		alignedSameDir.ID: alignedSameDir,
		weakSameDir.ID:    weakSameDir,
	}
	relations := []semanticRecord{{FromID: root.ID, ToID: related.ID}}

	got := rankSemanticTestSuggestions(symbols, []semanticRecord{root}, relations, "repair checksum", 8)
	assertSemanticSuggestion(t, got, exact.ID, "exact source/test companion", false)
	assertSemanticSuggestion(t, got, related.ID, "semantic relation", false)
	assertSemanticSuggestion(t, got, nameAndTask.ID, "name match", false)
	assertSemanticSuggestion(t, got, alignedSameDir.ID, "task term match", false)
	assertSemanticSuggestion(t, got, weakSameDir.ID, "same directory", true)

	visible := visibleSemanticTestSuggestions(got, 8)
	for _, suggestion := range visible {
		if suggestion.weak {
			t.Fatalf("weak same-directory suggestion escaped packet projection: %+v", visible)
		}
	}
	for _, id := range []string{exact.ID, related.ID, nameAndTask.ID, alignedSameDir.ID} {
		if semanticSuggestionByID(visible, id) == nil {
			t.Fatalf("strong suggestion %q did not survive projection: %+v", id, visible)
		}
	}
}

func TestSemanticTestSuggestionExactAndRelationInheritTaskAlignedRoot(t *testing.T) {
	root := semanticRecord{ID: "source:parser", Name: "ParseDocument", FilePath: "src/parser.go"}
	exact := semanticTestSuggestionFixture("test:exact-eof", "TestHandlesEOF", "src/parser_test.go", 10)
	related := semanticTestSuggestionFixture("test:related-sentinel", "TestHandlesSentinel", "integration/eof_test.go", 20)
	symbols := map[string]semanticRecord{exact.ID: exact, related.ID: related}
	relations := []semanticRecord{{FromID: root.ID, ToID: related.ID}}

	got := rankSemanticTestSuggestions(symbols, []semanticRecord{root}, relations, "repair parser", 4)
	assertSemanticSuggestion(t, got, exact.ID, "exact source/test companion", false)
	assertSemanticSuggestion(t, got, related.ID, "semantic relation", false)
	for _, id := range []string{exact.ID, related.ID} {
		if suggestion := semanticSuggestionByID(got, id); suggestion == nil || !suggestion.fallback {
			t.Fatalf("root-inherited suggestion %q was not marked fallback: %+v", id, suggestion)
		}
	}
	if visible := visibleSemanticTestSuggestions(got, 4); len(visible) != 2 {
		t.Fatalf("root-aligned exact/relation evidence disappeared from packet: %+v", visible)
	}
}

func TestSemanticTestSuggestionUnalignedRelationRemainsWeak(t *testing.T) {
	root := semanticRecord{ID: "source:engine", Name: "RunEngine", FilePath: "src/engine.go"}
	related := semanticTestSuggestionFixture("test:related-eof", "TestHandlesEOF", "integration/eof_test.go", 20)
	got := rankSemanticTestSuggestions(
		map[string]semanticRecord{related.ID: related},
		[]semanticRecord{root},
		[]semanticRecord{{FromID: root.ID, ToID: related.ID}},
		"prioritize Java layout",
		4,
	)
	assertSemanticSuggestion(t, got, related.ID, "semantic relation", true)
	if visible := visibleSemanticTestSuggestions(got, 4); len(visible) != 0 {
		t.Fatalf("unrelated relation escaped packet projection: %+v", visible)
	}
}

func TestSemanticTestSuggestionPrimaryEvidenceSuppressesInheritedFallback(t *testing.T) {
	root := semanticRecord{ID: "source:parser", Name: "ParseDocument", FilePath: "src/parser.go"}
	fallback := semanticTestSuggestionFixture("test:fallback-eof", "TestHandlesEOF", "src/parser_test.go", 10)
	primary := semanticTestSuggestionFixture("test:primary-recovery", "TestParserRecovery", "integration/recovery_test.go", 20)
	got := rankSemanticTestSuggestions(
		map[string]semanticRecord{fallback.ID: fallback, primary.ID: primary},
		[]semanticRecord{root},
		nil,
		"repair parser recovery",
		4,
	)
	if suggestion := semanticSuggestionByID(got, fallback.ID); suggestion == nil || !suggestion.fallback {
		t.Fatalf("exact root-inherited suggestion = %+v, want fallback", suggestion)
	}
	visible := visibleSemanticTestSuggestions(got, 4)
	if len(visible) != 1 || visible[0].Symbol.ID != primary.ID {
		t.Fatalf("visible suggestions = %+v, want only candidate-local primary", visible)
	}
}

func TestSemanticTestSuggestionFreshnessFilterRefillsBeforeCap(t *testing.T) {
	repoDir := t.TempDir()
	currentPath := "internal/auth/token_test.go"
	if err := os.MkdirAll(filepath.Join(repoDir, "internal", "auth"), 0o700); err != nil {
		t.Fatalf("mkdir current test parent: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, filepath.FromSlash(currentPath)), []byte("package auth\n"), 0o600); err != nil {
		t.Fatalf("write current test: %v", err)
	}
	stale := semanticTestSuggestion{Symbol: semanticTestSuggestionFixture("test:stale", "TestLegacyToken", "internal/auth/legacy_token_test.go", 1), Reason: "semantic relation"}
	current := semanticTestSuggestion{Symbol: semanticTestSuggestionFixture("test:current", "TestToken", currentPath, 1), Reason: "exact source/test companion", fallback: true}
	semantic := brainBriefSemantic{Tests: semanticTestsResult{
		Suggestions:          []semanticTestSuggestion{stale, current},
		synthesisSuggestions: []semanticTestSuggestion{stale, current},
	}}

	stats := brainBriefFilterDepartedSemantic(repoDir, brainLiveState{}, &semantic)
	brainBriefCapSemantic(&semantic, 1)
	semantic.Tests.Suggestions = visibleSemanticTestSuggestions(semantic.Tests.Suggestions, 1)
	if stats.RemovedRecords != 1 || stats.KeptRecords != 1 {
		t.Fatalf("freshness stats = %+v, want one stale removal and one retained candidate", stats)
	}
	if len(semantic.Tests.Suggestions) != 1 || semantic.Tests.Suggestions[0].Symbol.ID != current.Symbol.ID {
		t.Fatalf("public refill = %+v, want current candidate", semantic.Tests.Suggestions)
	}
	if got := semanticTestSuggestionsForSynthesis(semantic.Tests); len(got) != 1 || got[0].Symbol.ID != current.Symbol.ID {
		t.Fatalf("synthesis refill = %+v, want current candidate", got)
	}
}

func TestSemanticTestSuggestionLegacyStreamOwnsFreshnessGraphFiltering(t *testing.T) {
	repoDir := t.TempDir()
	currentPath := "internal/auth/token.go"
	if err := os.MkdirAll(filepath.Join(repoDir, "internal", "auth"), 0o700); err != nil {
		t.Fatalf("mkdir current source parent: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, filepath.FromSlash(currentPath)), []byte("package auth\n"), 0o600); err != nil {
		t.Fatalf("write current source: %v", err)
	}

	t.Run("ranked IDs do not perturb legacy graph", func(t *testing.T) {
		semantic := brainBriefSemantic{
			RuntimeTraces: []semanticRecord{{FromID: "ranked-stale", ToID: "external:trace", FilePath: currentPath}},
			Tests: semanticTestsResult{
				Suggestions: []semanticTestSuggestion{{Symbol: semanticRecord{ID: "ranked-stale", FilePath: "internal/auth/departed_test.go"}}},
				synthesisSuggestions: []semanticTestSuggestion{{
					Symbol: semanticRecord{ID: "legacy-current", FilePath: currentPath},
				}},
			},
		}
		brainBriefFilterDepartedSemantic(repoDir, brainLiveState{}, &semantic)
		if len(semantic.RuntimeTraces) != 1 {
			t.Fatalf("ranked-only stale ID removed a legacy-preserved graph edge: %+v", semantic.RuntimeTraces)
		}
	})

	t.Run("legacy IDs retain pre-ranking graph behavior", func(t *testing.T) {
		semantic := brainBriefSemantic{
			RuntimeTraces: []semanticRecord{{FromID: "legacy-stale", ToID: "external:trace", FilePath: currentPath}},
			Tests: semanticTestsResult{
				Suggestions: []semanticTestSuggestion{{Symbol: semanticRecord{ID: "ranked-current", FilePath: currentPath}}},
				synthesisSuggestions: []semanticTestSuggestion{{
					Symbol: semanticRecord{ID: "legacy-stale", FilePath: "internal/auth/departed_test.go"},
				}},
			},
		}
		brainBriefFilterDepartedSemantic(repoDir, brainLiveState{}, &semantic)
		if len(semantic.RuntimeTraces) != 0 {
			t.Fatalf("legacy stale ID no longer removed its graph edge: %+v", semantic.RuntimeTraces)
		}
	})
}

func TestSemanticTestSuggestionNoMatchAndNoOpProjectionPreservePacketBytes(t *testing.T) {
	report := brainBriefReport{Task: "update parser documentation"}
	before := renderBrainBriefCompactV3ForTest(t, report)
	root := semanticRecord{ID: "source:token", Name: "ValidateToken", FilePath: "internal/auth/token.go"}
	unrelated := semanticTestSuggestionFixture("test:other", "TestSession", "integration/session_test.go", 1)
	report.Semantic.Tests.Suggestions = visibleSemanticTestSuggestions(
		rankSemanticTestSuggestions(map[string]semanticRecord{unrelated.ID: unrelated}, []semanticRecord{root}, nil, report.Task, 8),
		8,
	)
	after := renderBrainBriefCompactV3ForTest(t, report)
	if before != after {
		t.Fatalf("no-match packet changed\n--- before ---\n%s--- after ---\n%s", before, after)
	}

	good := []semanticTestSuggestion{{Symbol: unrelated, Reason: "semantic relation"}}
	projected := visibleSemanticTestSuggestions(good, 8)
	if len(projected) != 1 || &projected[0] != &good[0] {
		t.Fatal("already-good projection copied or rewrote the suggestion slice")
	}
}

func TestSemanticTestSuggestionRankingIsDeterministicWithBoundedOutput(t *testing.T) {
	root := semanticRecord{ID: "source:token", Name: "ValidateToken", FilePath: "internal/auth/token.go"}
	symbols := make(map[string]semanticRecord, 2_000)
	for i := 0; i < 2_000; i++ {
		id := fmt.Sprintf("test:%04d", i)
		path := fmt.Sprintf("internal/auth/cases_%03d_test.go", i%200)
		name := fmt.Sprintf("TestCase%04d", i)
		if i%173 == 0 {
			name = fmt.Sprintf("TestTokenChecksum%04d", i)
		}
		symbols[id] = semanticTestSuggestionFixture(id, name, path, i+1)
	}
	want := rankSemanticTestSuggestions(symbols, []semanticRecord{root}, nil, "repair token checksum", 8)
	if len(want) != 8 {
		t.Fatalf("ranked suggestions = %d, want bounded output of 8", len(want))
	}
	for i := 0; i < 20; i++ {
		got := rankSemanticTestSuggestions(symbols, []semanticRecord{root}, nil, "repair token checksum", 8)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("map-order-dependent ranking on iteration %d\n got: %+v\nwant: %+v", i, got, want)
		}
	}
}

func BenchmarkSemanticTestSuggestionRankingBoundedReservoir(b *testing.B) {
	for _, count := range []int{1_000, 10_000} {
		b.Run(fmt.Sprintf("symbols=%d/limit=8", count), func(b *testing.B) {
			root := semanticRecord{ID: "source:token", Name: "ValidateToken", FilePath: "internal/auth/token.go"}
			symbols := make(map[string]semanticRecord, count)
			for i := 0; i < count; i++ {
				id := fmt.Sprintf("test:%06d", i)
				name := fmt.Sprintf("TestCase%06d", i)
				if i%997 == 0 {
					name = fmt.Sprintf("TestTokenChecksum%06d", i)
				}
				path := fmt.Sprintf("internal/auth/cases_%04d_test.go", i%500)
				symbols[id] = semanticTestSuggestionFixture(id, name, path, i+1)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				got := rankSemanticTestSuggestions(symbols, []semanticRecord{root}, nil, "repair token checksum", 8)
				if len(got) != 8 {
					b.Fatalf("ranked suggestions = %d, want 8", len(got))
				}
			}
		})
	}
}

func semanticTestSuggestionFixture(id, name, file string, line int) semanticRecord {
	return semanticRecord{
		ID: id, Kind: "test", Name: name, QualifiedName: "fixture." + name,
		FilePath: file, StartLine: line, EndLine: line + 1,
	}
}

func assertSemanticSuggestion(t *testing.T, suggestions []semanticTestSuggestion, id, reason string, weak bool) {
	t.Helper()
	suggestion := semanticSuggestionByID(suggestions, id)
	if suggestion == nil {
		t.Fatalf("suggestion %q missing from %+v", id, suggestions)
	}
	if suggestion.Reason != reason || suggestion.weak != weak {
		t.Fatalf("suggestion %q = %+v, want reason %q weak=%t", id, *suggestion, reason, weak)
	}
}

func semanticSuggestionByID(suggestions []semanticTestSuggestion, id string) *semanticTestSuggestion {
	for i := range suggestions {
		if suggestions[i].Symbol.ID == id {
			return &suggestions[i]
		}
	}
	return nil
}
