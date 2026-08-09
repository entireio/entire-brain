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

// writeMultiConceptFixture builds three sessions: one covering three concepts
// in three different exchanges, one covering two concepts in a single
// exchange plus one more, and one missing two concepts entirely.
func writeMultiConceptFixture(t *testing.T) string {
	t.Helper()
	brainDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(brainDir, historyDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	mk := func(id, session, summary string, ordinal int) historyRecord {
		return historyRecord{
			ID: conversationIDPrefix + id, Kind: conversationKind,
			Path: "sessions/main/20260801T000000Z_" + session + ".jsonl", Line: ordinal*2 - 1, EndLine: ordinal * 2,
			TurnOrdinal: ordinal, SessionID: session, Agent: "Claude Code", Branch: "main",
			CreatedAt: "2026-08-01T10:00:00Z", Summary: summary, ContentRole: conversationContentRole,
		}
	}
	index := historyIndex{
		GeneratedAt: time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC),
		Records: []historyRecord{
			mk("one1", "sess-one", "alpha rollout plan for the ingest service", 1),
			mk("one2", "sess-one", "beta cache eviction bug in the ingest service", 2),
			mk("one3", "sess-one", "gamma flag cleanup after the ingest migration", 3),
			mk("two1", "sess-two", "alpha rollout hit the beta cache eviction path together", 1),
			mk("two2", "sess-two", "gamma flag rollout follow-up", 2),
			mk("three1", "sess-three", "alpha rollout notes without the other topics", 1),
		},
	}
	data, err := json.MarshalIndent(index, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brainDir, filepath.FromSlash(historyIndexPath)), data, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion, GeneratedAt: index.GeneratedAt, RepoKey: "test/mc",
		Sources: &brainSources{History: &historySourceManifest{
			GeneratedAt: index.GeneratedAt, IndexPath: historyIndexPath, Records: len(index.Records),
		}},
	}
	if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
		t.Fatal(err)
	}
	return brainDir
}

func TestMultiConceptSessionCoverage(t *testing.T) {
	brainDir := writeMultiConceptFixture(t)
	opts := retrievalOptions{Source: retrievalSourceConversation, Concepts: []string{"beta cache eviction", "gamma flag"}}
	results, err := retrieveConversationMultiConcept(brainDir, "alpha rollout", 10, modeLexical, opts)
	if err != nil {
		t.Fatalf("multi-concept: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2 (sess-three misses two concepts): %+v", len(results), results)
	}
	bySession := map[string]unifiedResult{}
	for _, result := range results {
		if result.Heading != "session_coverage" || result.SessionRef != result.ID || !result.VerificationRequired {
			t.Fatalf("coverage envelope wrong: %+v", result)
		}
		if len(result.Concepts) != 3 || len(result.ConceptMatches) != 3 {
			t.Fatalf("concept counts: %+v", result)
		}
		if result.WorstRank <= 0 || result.RankSum < result.WorstRank || result.Approximate {
			t.Fatalf("rank fields: %+v", result)
		}
		firstEvidence := result.ConceptMatches[0].ConversationID
		bySession[firstEvidence] = result
	}
	// sess-one: three concepts in three different exchanges.
	one, ok := bySession[conversationIDPrefix+"one1"]
	if !ok || len(one.EvidenceIDs) != 3 {
		t.Fatalf("sess-one coverage: %+v", one)
	}
	// sess-two: one exchange satisfies two concepts; evidence is deduplicated
	// but stays in query order.
	two, ok := bySession[conversationIDPrefix+"two1"]
	if !ok {
		t.Fatalf("sess-two missing: %+v", bySession)
	}
	if len(two.EvidenceIDs) != 2 || two.EvidenceIDs[0] != conversationIDPrefix+"two1" || two.EvidenceIDs[1] != conversationIDPrefix+"two2" {
		t.Fatalf("sess-two evidence: %+v", two.EvidenceIDs)
	}
	for _, match := range two.ConceptMatches {
		if match.Arm != "lexical" || match.Rank <= 0 {
			t.Fatalf("concept match fields: %+v", match)
		}
	}

	// Determinism: the same query yields the same order.
	again, err := retrieveConversationMultiConcept(brainDir, "alpha rollout", 10, modeLexical, opts)
	if err != nil || len(again) != len(results) {
		t.Fatalf("re-run: %v", err)
	}
	for i := range results {
		if results[i].ID != again[i].ID {
			t.Fatalf("order not deterministic: %s vs %s", results[i].ID, again[i].ID)
		}
	}

	// A structured filter reaches every concept list: filtering to an agent
	// nobody used matches nothing.
	filtered := opts
	filtered.Agent = "Codex"
	none, err := retrieveConversationMultiConcept(brainDir, "alpha rollout", 10, modeLexical, filtered)
	if err != nil || len(none) != 0 {
		t.Fatalf("agent-filtered coverage: %v (%d)", err, len(none))
	}
}

func TestMultiConceptInputValidation(t *testing.T) {
	if _, err := normalizeConversationConcepts("alpha", []string{"alpha"}); err == nil {
		t.Fatal("case-insensitive duplicate must error")
	}
	if _, err := normalizeConversationConcepts("alpha", []string{" "}); err == nil {
		t.Fatal("empty concept must error")
	}
	if _, err := normalizeConversationConcepts("alpha", []string{"b", "c", "d", "e", "f"}); err == nil {
		t.Fatal("more than five total concepts must error")
	}
	if _, err := normalizeConversationConcepts("alpha", []string{strings.Repeat("x", conversationConceptMaxBytes+1)}); err == nil {
		t.Fatal("over-cap concept must error")
	}
	if _, err := buildRetrievalOptions("history", "", "", "", "", "", []string{"beta"}); err == nil {
		t.Fatal("concepts with a non-conversation source must error")
	}
	if _, err := buildRetrievalOptions("conversation", "", "", "", "", "", []string{"a", "b", "c", "d", "e"}); err == nil {
		t.Fatal("more than four extra concepts must error")
	}
}

func TestMultiConceptTooBroadIsStructured(t *testing.T) {
	brainDir := writeMultiConceptFixture(t)
	old := conversationConceptScanCeiling
	conversationConceptScanCeiling = 2
	defer func() { conversationConceptScanCeiling = old }()
	opts := retrievalOptions{Source: retrievalSourceConversation, Concepts: []string{"ingest"}}
	_, err := retrieveConversationMultiConcept(brainDir, "rollout", 10, modeLexical, opts)
	if err == nil || !strings.Contains(err.Error(), "memory_query_too_broad") {
		t.Fatalf("ceiling overflow must be memory_query_too_broad: %v", err)
	}
}

func TestMultiConceptVectorModeUnavailableIsStructured(t *testing.T) {
	brainDir := writeMultiConceptFixture(t)
	opts := retrievalOptions{Source: retrievalSourceConversation, Concepts: []string{"gamma flag"}}
	if _, err := retrieveConversationMultiConcept(brainDir, "alpha rollout", 10, modeVector, opts); err == nil {
		t.Fatal("closed vector arm must return the structured vector-state error")
	}
}

func TestMultiConceptUsesShortTermEvidence(t *testing.T) {
	brainDir := writeMultiConceptFixture(t)
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	// Overlay adds the gamma exchange sess-three was missing: the session now
	// covers all concepts through mixed long-term and short-term evidence.
	overlayRecord := historyRecord{
		ID: conversationIDPrefix + "three2", Kind: conversationKind,
		Path: "sessions/main/20260807T000000Z_sess-three-delta.jsonl", Line: 1, EndLine: 2,
		TurnOrdinal: 2, SessionID: "sess-three", Agent: "Claude Code", Branch: "main",
		CreatedAt: "2026-08-07T10:00:00Z", ContentRole: conversationContentRole,
		Summary: "gamma flag cleanup and beta cache eviction follow-up for ingest",
	}
	overlay := shortTermIndex{
		Version:           historyShortTermVersion,
		ReconcilerVersion: historyShortTermReconcilerVersion,
		BaseGeneratedAt:   manifest.Sources.History.GeneratedAt,
		GeneratedAt:       manifest.Sources.History.GeneratedAt.Add(time.Hour),
		Files: map[string]shortTermFile{
			overlayRecord.Path: {Records: []historyRecord{overlayRecord}},
		},
	}
	data, err := json.MarshalIndent(overlay, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brainDir, filepath.FromSlash(historyShortTermPath)), data, 0o600); err != nil {
		t.Fatal(err)
	}
	opts := retrievalOptions{Source: retrievalSourceConversation, Concepts: []string{"beta cache eviction", "gamma flag"}}
	results, err := retrieveConversationMultiConcept(brainDir, "alpha rollout", 10, modeLexical, opts)
	if err != nil {
		t.Fatalf("multi-concept with overlay: %v", err)
	}
	foundThree := false
	for _, result := range results {
		for _, id := range result.EvidenceIDs {
			if id == conversationIDPrefix+"three2" {
				foundThree = true
			}
		}
	}
	if !foundThree {
		t.Fatalf("short-term evidence must complete the session coverage: %+v", results)
	}
}

func replaceMultiConceptLongTermRecords(t *testing.T, brainDir string, records []historyRecord) {
	t.Helper()
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	index := historyIndex{GeneratedAt: manifest.Sources.History.GeneratedAt, Records: records}
	data, err := json.MarshalIndent(index, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brainDir, filepath.FromSlash(historyIndexPath)), data, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest.Sources.History.Records = len(records)
	manifest.Sources.History.Exchanges = len(records)
	if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
		t.Fatal(err)
	}
}

func multiConceptOverflowRecord(id string, ordinal int) historyRecord {
	return historyRecord{
		ID: conversationIDPrefix + id, Kind: conversationKind,
		Path: fmt.Sprintf("sessions/main/20260808T120000Z_%s.jsonl", id), Line: 1, EndLine: 2,
		TurnOrdinal: ordinal, SessionID: id, Agent: "Claude Code", Branch: "main",
		CreatedAt: "2026-08-08T12:00:00Z", ContentRole: conversationContentRole,
		Summary: fmt.Sprintf("alpha deployment item %d with beta verification", ordinal),
	}
}

// The C2 lexical contract measures the ceiling after the in-scope winner
// rules, not at an arbitrary FTS SQL window. Every record in this fixture is
// in scope and distinct, so a complete scan must reject rather than return a
// partial session intersection.
func TestMultiConceptTooBroadForAllInScopeLongTermMatches(t *testing.T) {
	brainDir := writeMultiConceptFixture(t)
	oldCeiling := conversationConceptScanCeiling
	conversationConceptScanCeiling = 3
	defer func() { conversationConceptScanCeiling = oldCeiling }()
	records := make([]historyRecord, 0, conversationConceptScanCeiling+1)
	for i := 0; i <= conversationConceptScanCeiling; i++ {
		records = append(records, multiConceptOverflowRecord(fmt.Sprintf("long-%d", i), i+1))
	}
	replaceMultiConceptLongTermRecords(t, brainDir, records)

	_, err := retrieveConversationMultiConcept(brainDir, "alpha", 10, modeLexical, retrievalOptions{
		Source: retrievalSourceConversation, Agent: "Claude Code", Concepts: []string{"beta"},
	})
	if err == nil || !strings.Contains(err.Error(), "memory_query_too_broad") {
		t.Fatalf("all in-scope long-term overflow must reject, got %v", err)
	}
}

// A large global match set is not itself an overflow. The cap applies only
// after the caller's scope predicate, so unrelated sessions cannot force an
// in-scope query to fail or push its one valid result beyond a raw FTS window.
func TestMultiConceptCeilingCountsOnlyInScopeLongTermMatches(t *testing.T) {
	brainDir := writeMultiConceptFixture(t)
	oldCeiling := conversationConceptScanCeiling
	conversationConceptScanCeiling = 1
	defer func() { conversationConceptScanCeiling = oldCeiling }()
	oldRawCeiling := historyFTSExhaustiveRawScanCeiling
	historyFTSExhaustiveRawScanCeiling = 3
	defer func() { historyFTSExhaustiveRawScanCeiling = oldRawCeiling }()
	outsideOne := multiConceptOverflowRecord("outside-one", 1)
	outsideOne.Agent = "Codex"
	outsideTwo := multiConceptOverflowRecord("outside-two", 2)
	outsideTwo.Agent = "Codex"
	inside := multiConceptOverflowRecord("inside", 3)
	replaceMultiConceptLongTermRecords(t, brainDir, []historyRecord{outsideOne, outsideTwo, inside})

	results, err := retrieveConversationMultiConcept(brainDir, "alpha", 10, modeLexical, retrievalOptions{
		Source: retrievalSourceConversation, Agent: "Claude Code", Concepts: []string{"beta"},
	})
	if err != nil {
		t.Fatalf("out-of-scope candidates must not cause an overflow: %v", err)
	}
	if len(results) != 1 || len(results[0].EvidenceIDs) != 1 || results[0].EvidenceIDs[0] != conversationIDPrefix+"inside" {
		t.Fatalf("expected only the in-scope session, got %+v", results)
	}
}

// The exhaustive path has a separate, intentionally high raw-row guard. It
// is not a result window: the preceding test proves an in-scope hit can occur
// after out-of-scope rows within this allowance. Beyond it C2 refuses with a
// typed error instead of reading an unbounded FTS result set.
func TestMultiConceptRawFTSScanCeilingIsStructured(t *testing.T) {
	brainDir := writeMultiConceptFixture(t)
	oldRawCeiling := historyFTSExhaustiveRawScanCeiling
	historyFTSExhaustiveRawScanCeiling = 2
	defer func() { historyFTSExhaustiveRawScanCeiling = oldRawCeiling }()
	records := make([]historyRecord, 0, 3)
	for i := 0; i < 3; i++ {
		record := multiConceptOverflowRecord(fmt.Sprintf("raw-%d", i), i+1)
		record.Agent = "Codex"
		records = append(records, record)
	}
	replaceMultiConceptLongTermRecords(t, brainDir, records)

	_, err := retrieveConversationMultiConcept(brainDir, "alpha", 10, modeLexical, retrievalOptions{
		Source: retrievalSourceConversation, Agent: "Claude Code", Concepts: []string{"beta"},
	})
	if err == nil || !strings.Contains(err.Error(), "memory_query_too_broad") || !strings.Contains(err.Error(), "scanning more than 2") {
		t.Fatalf("raw scan guard must return a typed refusal, got %v", err)
	}
}

func unavailableExhaustiveFTS(historyIndex, func(historyRecord) bool) ([]scoredHistoryRecord, historyExhaustiveRankState, bool, int) {
	return nil, historyExhaustiveRankComplete, false, 0
}

func TestMultiConceptRawScanCeilingAppliesToPureGoFallback(t *testing.T) {
	oldRawCeiling := historyFTSExhaustiveRawScanCeiling
	historyFTSExhaustiveRawScanCeiling = 2
	defer func() { historyFTSExhaustiveRawScanCeiling = oldRawCeiling }()
	records := []historyRecord{
		multiConceptOverflowRecord("fallback-1", 1),
		multiConceptOverflowRecord("fallback-2", 2),
		multiConceptOverflowRecord("fallback-3", 3),
	}

	ranked, state := rankFreshHistoryExhaustive(
		freshHistory{index: historyIndex{Records: records}}, conversationKind, "alpha", 10,
		func(historyRecord) bool { return false }, unavailableExhaustiveFTS,
	)
	if state != historyExhaustiveRankRawScanOverflow || len(ranked) != 0 {
		t.Fatalf("pure-Go fallback must refuse before returning partial results: state=%v ranked=%+v", state, ranked)
	}
}

func TestMultiConceptRawScanCeilingAppliesToOverlay(t *testing.T) {
	oldRawCeiling := historyFTSExhaustiveRawScanCeiling
	historyFTSExhaustiveRawScanCeiling = 2
	defer func() { historyFTSExhaustiveRawScanCeiling = oldRawCeiling }()
	overlay := []historyRecord{
		multiConceptOverflowRecord("overlay-raw-1", 1),
		multiConceptOverflowRecord("overlay-raw-2", 2),
		multiConceptOverflowRecord("overlay-raw-3", 3),
	}

	ranked, state := rankFreshHistoryExhaustive(
		freshHistory{index: historyIndex{}, overlay: overlay}, conversationKind, "alpha", 10,
		nil, unavailableExhaustiveFTS,
	)
	if state != historyExhaustiveRankRawScanOverflow || len(ranked) != 0 {
		t.Fatalf("overlay scan must refuse before returning partial results: state=%v ranked=%+v", state, ranked)
	}
}

func TestMultiConceptRawScanBudgetIsSharedAcrossLongTermAndOverlay(t *testing.T) {
	oldRawCeiling := historyFTSExhaustiveRawScanCeiling
	historyFTSExhaustiveRawScanCeiling = 3
	defer func() { historyFTSExhaustiveRawScanCeiling = oldRawCeiling }()
	longTerm := []historyRecord{
		multiConceptOverflowRecord("union-long-1", 1),
		multiConceptOverflowRecord("union-long-2", 2),
	}
	overlay := []historyRecord{
		multiConceptOverflowRecord("union-overlay-1", 3),
		multiConceptOverflowRecord("union-overlay-2", 4),
	}

	ranked, state := rankFreshHistoryExhaustive(
		freshHistory{index: historyIndex{Records: longTerm}, overlay: overlay}, conversationKind, "alpha", 10,
		nil, unavailableExhaustiveFTS,
	)
	if state != historyExhaustiveRankRawScanOverflow || len(ranked) != 0 {
		t.Fatalf("two-tier scan must share one budget: state=%v ranked=%+v", state, ranked)
	}
}

func TestMultiConceptExhaustiveFTSAndFallbackUseSameMatchContract(t *testing.T) {
	brainDir := t.TempDir()
	index := historyIndex{Records: []historyRecord{
		{ID: conversationIDPrefix + "one-term", Kind: conversationKind, Path: "sessions/main/one.jsonl", Line: 1, Summary: "alpha only"},
		{ID: conversationIDPrefix + "both-terms", Kind: conversationKind, Path: "sessions/main/both.jsonl", Line: 1, Summary: "alpha and beta"},
	}}
	fresh := freshHistory{index: index}
	viaFTS, ftsState := rankFreshHistoryExhaustive(
		fresh, conversationKind, "alpha beta", 10, nil,
		func(longTerm historyIndex, pred func(historyRecord) bool) ([]scoredHistoryRecord, historyExhaustiveRankState, bool, int) {
			return rankHistoryViaFTSExhaustiveFiltered(brainDir, longTerm, conversationKind, "alpha beta", 10, pred)
		},
	)
	viaFallback, fallbackState := rankFreshHistoryExhaustive(
		fresh, conversationKind, "alpha beta", 10, nil, unavailableExhaustiveFTS,
	)
	if ftsState != historyExhaustiveRankComplete || fallbackState != historyExhaustiveRankComplete {
		t.Fatalf("exhaustive states differ: fts=%v fallback=%v", ftsState, fallbackState)
	}
	if len(viaFTS) != 1 || len(viaFallback) != 1 ||
		viaFTS[0].Record.ID != conversationIDPrefix+"both-terms" || viaFallback[0].Record.ID != viaFTS[0].Record.ID {
		t.Fatalf("FTS/fallback match-set mismatch: fts=%+v fallback=%+v", viaFTS, viaFallback)
	}
}

// Short-term records are not in the FTS store. They must nevertheless be
// counted before C2 produces coverage, otherwise a busy in-flight session can
// make an AND query silently partial even when the long-term index is empty.
func TestMultiConceptTooBroadForOverlayMatches(t *testing.T) {
	brainDir := writeMultiConceptFixture(t)
	oldCeiling := conversationConceptScanCeiling
	conversationConceptScanCeiling = 3
	defer func() { conversationConceptScanCeiling = oldCeiling }()
	replaceMultiConceptLongTermRecords(t, brainDir, nil)
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	files := make(map[string]shortTermFile, conversationConceptScanCeiling+1)
	for i := 0; i <= conversationConceptScanCeiling; i++ {
		record := multiConceptOverflowRecord(fmt.Sprintf("overlay-%d", i), i+1)
		files[record.Path] = shortTermFile{Records: []historyRecord{record}}
	}
	overlay := shortTermIndex{
		Version:             historyShortTermVersion,
		ReconcilerVersion:   historyShortTermReconcilerVersion,
		BaseGeneratedAt:     manifest.Sources.History.GeneratedAt,
		SessionsFingerprint: brainSessionsFingerprint(brainDir),
		Files:               files,
	}
	if err := saveHistoryShortTerm(brainDir, overlay); err != nil {
		t.Fatal(err)
	}

	_, err = retrieveConversationMultiConcept(brainDir, "alpha", 10, modeLexical, retrievalOptions{
		Source: retrievalSourceConversation, Agent: "Claude Code", Concepts: []string{"beta"},
	})
	if err == nil || !strings.Contains(err.Error(), "memory_query_too_broad") {
		t.Fatalf("overlay overflow must reject, got %v", err)
	}
}

// Equal prose is not duplicate session evidence. C2 must preserve both stable
// exchange identities in the FTS path and in its in-memory overlay/fallback
// path, otherwise one session silently loses AND coverage.
func TestMultiConceptPreservesIdenticalSummariesAcrossSessions(t *testing.T) {
	const sharedSummary = "alpha deployment completed with beta verification"
	makeRecords := func() []historyRecord {
		left := multiConceptOverflowRecord("same-left", 1)
		left.Summary = sharedSummary
		right := multiConceptOverflowRecord("same-right", 1)
		right.Summary = sharedSummary
		return []historyRecord{left, right}
	}
	assertBoth := func(t *testing.T, brainDir string) {
		t.Helper()
		results, err := retrieveConversationMultiConcept(brainDir, "alpha", 10, modeLexical, retrievalOptions{
			Source: retrievalSourceConversation, Concepts: []string{"beta"},
		})
		if err != nil {
			t.Fatalf("identical-summary coverage: %v", err)
		}
		seen := map[string]bool{}
		for _, result := range results {
			for _, id := range result.EvidenceIDs {
				seen[id] = true
			}
		}
		if len(results) != 2 || !seen[conversationIDPrefix+"same-left"] || !seen[conversationIDPrefix+"same-right"] {
			t.Fatalf("both sessions must retain independent evidence: %+v", results)
		}
	}

	t.Run("long-term FTS", func(t *testing.T) {
		brainDir := writeMultiConceptFixture(t)
		replaceMultiConceptLongTermRecords(t, brainDir, makeRecords())
		assertBoth(t, brainDir)
	})

	t.Run("long-term in-memory fallback", func(t *testing.T) {
		ranked, state := rankFreshHistoryExhaustive(
			freshHistory{index: historyIndex{Records: makeRecords()}},
			conversationKind, "alpha", 10, nil,
			func(historyIndex, func(historyRecord) bool) ([]scoredHistoryRecord, historyExhaustiveRankState, bool, int) {
				return nil, historyExhaustiveRankComplete, false, 0
			},
		)
		if state != historyExhaustiveRankComplete || len(ranked) != 2 {
			t.Fatalf("fallback collapsed independent equal summaries: state=%v ranked=%+v", state, ranked)
		}
	})

	t.Run("short-term in-memory", func(t *testing.T) {
		brainDir := writeMultiConceptFixture(t)
		replaceMultiConceptLongTermRecords(t, brainDir, nil)
		manifest, err := loadBrainManifest(brainDir)
		if err != nil {
			t.Fatal(err)
		}
		files := map[string]shortTermFile{}
		for _, record := range makeRecords() {
			files[record.Path] = shortTermFile{Records: []historyRecord{record}}
		}
		if err := saveHistoryShortTerm(brainDir, shortTermIndex{
			Version:             historyShortTermVersion,
			ReconcilerVersion:   historyShortTermReconcilerVersion,
			BaseGeneratedAt:     manifest.Sources.History.GeneratedAt,
			SessionsFingerprint: brainSessionsFingerprint(brainDir),
			Files:               files,
		}); err != nil {
			t.Fatal(err)
		}
		assertBoth(t, brainDir)
	})
}
