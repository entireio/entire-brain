package cli

import (
	"encoding/json"
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
	old := historyFTSFilteredScanCeiling
	historyFTSFilteredScanCeiling = 2
	defer func() { historyFTSFilteredScanCeiling = old }()
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
