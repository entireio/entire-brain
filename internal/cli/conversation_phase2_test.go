package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Phase 2 increment 1: index-time dedupe of re-exported sessions, hook/noise
// filtering, structured conversation filters, and the per-session diversity
// cap.

func TestHistoryIndexDedupesReexportedSessionCopies(t *testing.T) {
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	brainDir := t.TempDir()
	// The same session exported twice under different transcript paths (the
	// 17–31% duplication measured while dogfooding). Filenames carry sortable
	// timestamps; the newer export must win.
	older := "sessions/main/20260801T000000Z_first-export.jsonl"
	newer := "sessions/main/20260806T000000Z_second-export.jsonl"
	for _, rel := range []string{older, newer} {
		full := filepath.Join(brainDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(conversationClaudeFixture), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion, GeneratedAt: now, RepoKey: "test/repo", DefaultBranch: "main",
		Sources: &brainSources{Sessions: &sessionSourceManifest{GeneratedAt: now, DefaultBranch: "main", Sessions: []exportSession{
			{SessionID: "sess-dup", Branch: "main", Agent: "claude", LatestCheckpoint: "cp1", TranscriptPath: older, CreatedAt: now.Add(-2 * time.Hour)},
			{SessionID: "sess-dup", Branch: "main", Agent: "claude", LatestCheckpoint: "cp2", TranscriptPath: newer, CreatedAt: now.Add(-time.Hour)},
		}}},
	}
	if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
		t.Fatal(err)
	}
	source, err := writeBrainHistoryIndexAndSource(brainDir, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	if source.Exchanges != 2 {
		t.Fatalf("exchanges = %d, want 2 (each logical exchange indexed once)", source.Exchanges)
	}
	records := conversationRecordsFromIndex(t, brainDir, source)
	seen := map[string]bool{}
	for _, record := range records {
		if seen[record.ID] {
			t.Fatalf("duplicate exchange id survived dedupe: %s", record.ID)
		}
		seen[record.ID] = true
		if record.Path != newer {
			t.Fatalf("dedupe kept the older export: %s", record.Path)
		}
	}
}

func TestConversationHookInjectedRequestsDoNotOpenExchanges(t *testing.T) {
	fixture := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"Stop hook feedback: [keep testing until done]: Condition incomplete."}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Resuming the loop."}]}}
{"type":"user","message":{"role":"user","content":[{"type":"text","text":"A session-scoped Stop hook is now active with condition: finish the proof loop."}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Acknowledged."}]}}
{"type":"user","message":{"role":"user","content":[{"type":"text","text":"Fix the failing export test"}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"The cursor fixture was stale; regenerated it."}]}}
`
	scan, err := scanConversationTranscript(writeConversationFixture(t, fixture))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(scan.Exchanges) != 1 {
		t.Fatalf("exchanges = %d, want 1 (hook injections must not open exchanges): %+v", len(scan.Exchanges), scan.Exchanges)
	}
	if scan.Exchanges[0].Request != "Fix the failing export test" {
		t.Fatalf("request = %q", scan.Exchanges[0].Request)
	}
}

func TestConversationAPIErrorNarrativeIsNoise(t *testing.T) {
	fixture := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"finish everything fully end to end"}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"API Error: 529 Overloaded. This is a server-side issue, usually temporary."}]}}
{"type":"user","message":{"role":"user","content":[{"type":"text","text":"retry the run"}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"API Error: 529 Overloaded."},{"type":"text","text":"Recovered; the run passed on retry."}]}}
`
	scan, err := scanConversationTranscript(writeConversationFixture(t, fixture))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	// Exchange 1's only response is an API error envelope: skipped, incomplete.
	// Exchange 2 keeps the real narrative and drops the error chunk.
	if len(scan.Exchanges) != 1 || scan.Incomplete != 1 {
		t.Fatalf("exchanges=%d incomplete=%d, want 1/1: %+v", len(scan.Exchanges), scan.Incomplete, scan.Exchanges)
	}
	response := scan.Exchanges[0].Response
	if strings.Contains(response, "API Error") || !strings.Contains(response, "Recovered; the run passed on retry.") {
		t.Fatalf("noise filtering wrong: %q", response)
	}
}

// writeConversationFilterFixture builds a brain whose history index holds
// exchanges across two sessions/agents/branches/times, for filter and
// diversity tests.
func writeConversationFilterFixture(t *testing.T) string {
	t.Helper()
	brainDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(brainDir, historyDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	mk := func(id, session, agent, branch, created, summary string, line int) historyRecord {
		return historyRecord{
			ID: conversationIDPrefix + id, Kind: conversationKind,
			Path: "sessions/main/20260801T000000Z_" + session + ".jsonl", Line: line, EndLine: line + 1,
			TurnOrdinal: line, SessionID: session, Agent: agent, Branch: branch, CreatedAt: created,
			Summary: summary, ContentRole: conversationContentRole,
		}
	}
	index := historyIndex{
		GeneratedAt: time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC),
		Records: []historyRecord{
			mk("a1", "sess-a", "Claude Code", "main", "2026-08-01T10:00:00Z", "deploy pipeline alpha attempt one", 1),
			mk("a2", "sess-a", "Claude Code", "main", "2026-08-01T10:00:00Z", "deploy pipeline alpha attempt two", 3),
			mk("a3", "sess-a", "Claude Code", "main", "2026-08-01T10:00:00Z", "deploy pipeline alpha attempt three", 5),
			mk("b1", "sess-b", "Codex", "feature/x", "2026-08-05T10:00:00Z", "deploy pipeline beta conclusion", 1),
			mk("c1", "sess-c", "Claude Code", "main", "", "deploy pipeline gamma no timestamp", 1),
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
		SchemaVersion: brainManifestSchemaVersion,
		GeneratedAt:   index.GeneratedAt,
		Sources: &brainSources{History: &historySourceManifest{
			GeneratedAt: index.GeneratedAt, IndexPath: historyIndexPath, Records: len(index.Records),
		}},
	}
	if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
		t.Fatal(err)
	}
	return brainDir
}

func conversationResultIDs(results []unifiedResult) []string {
	out := make([]string, len(results))
	for i, r := range results {
		out[i] = strings.TrimPrefix(r.ID, conversationIDPrefix)
	}
	return out
}

func TestConversationStructuredFilters(t *testing.T) {
	brainDir := writeConversationFilterFixture(t)
	query := "deploy pipeline"
	get := func(opts retrievalOptions) []string {
		t.Helper()
		results, err := retrieveConversation(brainDir, query, 10, modeLexical, opts)
		if err != nil {
			t.Fatalf("retrieve: %v", err)
		}
		return conversationResultIDs(results)
	}
	has := func(ids []string, want ...string) bool {
		set := map[string]bool{}
		for _, id := range ids {
			set[id] = true
		}
		for _, w := range want {
			if !set[w] {
				return false
			}
		}
		return len(ids) == len(want)
	}

	if ids := get(retrievalOptions{SessionID: "sess-b"}); !has(ids, "b1") {
		t.Fatalf("session filter: %v", ids)
	}
	if ids := get(retrievalOptions{Agent: "codex"}); !has(ids, "b1") {
		t.Fatalf("agent filter (case-insensitive): %v", ids)
	}
	if ids := get(retrievalOptions{Branch: "feature/x"}); !has(ids, "b1") {
		t.Fatalf("branch filter: %v", ids)
	}
	after := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	if ids := get(retrievalOptions{After: after}); !has(ids, "b1") {
		t.Fatalf("after filter (timestampless records must not leak in): %v", ids)
	}
	before := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	if ids := get(retrievalOptions{Before: before}); !has(ids, "a1", "a2", "a3") {
		t.Fatalf("before filter: %v", ids)
	}

	// Filters on non-conversation sources are a structured error.
	if _, err := retrieveUnifiedWithOptions("", brainDir, "main", query, 10, modeLexical, retrievalOptions{Source: retrievalSourceHistory, Agent: "Codex"}); err == nil {
		t.Fatal("agent filter with source=history must error")
	}

	// Explainability: matched terms surface on every result.
	results, err := retrieveConversation(brainDir, "deploy pipeline beta", 10, modeLexical, retrievalOptions{})
	if err != nil || len(results) == 0 {
		t.Fatalf("retrieve: %v", err)
	}
	for _, r := range results {
		if len(r.MatchedTerms) == 0 {
			t.Fatalf("missing matched_terms: %+v", r)
		}
	}
}

func TestBuildRetrievalOptionsValidation(t *testing.T) {
	if _, err := buildRetrievalOptions("conversation", "2026-08-01", "2026-08-05", "s", "a", "b"); err != nil {
		t.Fatalf("valid options rejected: %v", err)
	}
	if _, err := buildRetrievalOptions("conversation", "not-a-date", "", "", "", ""); err == nil {
		t.Fatal("bad after must error")
	}
	if _, err := buildRetrievalOptions("conversation", "2026-08-05", "2026-08-01", "", "", ""); err == nil {
		t.Fatal("after >= before must error")
	}
	if _, err := buildRetrievalOptions("everything", "", "", "", "", ""); err == nil {
		t.Fatal("bad source must error")
	}
}

func TestConversationSessionDiversityCap(t *testing.T) {
	brainDir := writeConversationFilterFixture(t)
	// limit 4 → per-session cap 2. sess-a has three strong candidates; b and c
	// must still appear.
	results, err := retrieveConversation(brainDir, "deploy pipeline", 4, modeLexical, retrievalOptions{})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	counts := map[string]int{}
	for _, r := range results {
		counts[r.SessionID]++
	}
	if counts["sess-a"] > 2 {
		t.Fatalf("session sess-a exceeded the diversity cap: %v", counts)
	}
	if counts["sess-b"] == 0 || counts["sess-c"] == 0 {
		t.Fatalf("capped session crowded out others: %v", counts)
	}

	// Backfill: when only one session matches, the cap must not starve the
	// result list.
	only, err := retrieveConversation(brainDir, "alpha attempt", 3, modeLexical, retrievalOptions{})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(only) != 3 {
		t.Fatalf("backfill failed: got %d results, want 3", len(only))
	}

	// An explicit session filter disables the cap.
	scoped, err := retrieveConversation(brainDir, "deploy pipeline", 10, modeLexical, retrievalOptions{SessionID: "sess-a"})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(scoped) != 3 {
		t.Fatalf("session-scoped query must return all its exchanges: %d", len(scoped))
	}
}

// --- Increment 2: conversation semantic arm ---

func TestSyncConversationVectorsSelectsExchangesOnly(t *testing.T) {
	store := newMemHistoryVecStore()
	e := &fakeFusionEmbedder{vecs: map[string][]float32{}}
	index := historyIndex{Records: []historyRecord{
		{ID: "history:d1", Kind: "decision", Summary: "decision summary"},
		{ID: "history:r1", Kind: "request", Summary: "request summary"},
		{ID: conversationIDPrefix + "e1", Kind: conversationKind, Summary: "exchange one"},
		{ID: conversationIDPrefix + "e2", Kind: conversationKind, Summary: "exchange two"},
	}}
	added, dropped, total, err := syncConversationVectors(store, index, e, nil)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if added != 2 || dropped != 0 || total != 2 {
		t.Fatalf("added/dropped/total = %d/%d/%d, want 2/0/2", added, dropped, total)
	}
	ids, _ := store.ids()
	if _, ok := ids[conversationIDPrefix+"e1"]; !ok {
		t.Fatalf("exchange vector missing: %v", ids)
	}
	if _, ok := ids["history:d1"]; ok {
		t.Fatal("non-exchange record embedded into the conversation store")
	}
	// A departed exchange is pruned on the next sync.
	index.Records = index.Records[:3]
	_, dropped, total, err = syncConversationVectors(store, index, e, nil)
	if err != nil || dropped != 1 || total != 1 {
		t.Fatalf("prune: dropped=%d total=%d err=%v", dropped, total, err)
	}
}

func TestRankConversationSemanticRanksExchangesOnly(t *testing.T) {
	index := historyIndex{Records: []historyRecord{
		{ID: "history:d1", Kind: "decision", Summary: "decision about caching"},
		{ID: conversationIDPrefix + "e1", Kind: conversationKind, Summary: "cache rewrite rationale"},
		{ID: conversationIDPrefix + "e2", Kind: conversationKind, Summary: "unrelated deploy talk"},
	}}
	scores := map[string]float64{
		"history:d1":                0.99, // must be ignored: not an exchange
		conversationIDPrefix + "e1": 0.9,
		conversationIDPrefix + "e2": 0.2,
	}
	ranked := rankConversationSemantic(index, scores, 10)
	if len(ranked) != 2 {
		t.Fatalf("ranked = %d records, want 2 (exchanges only): %+v", len(ranked), ranked)
	}
	if ranked[0].Record.ID != conversationIDPrefix+"e1" {
		t.Fatalf("cosine order wrong: %+v", ranked)
	}
	for _, s := range ranked {
		if s.Record.Kind != conversationKind {
			t.Fatalf("non-exchange leaked into conversation semantic ranking: %+v", s.Record)
		}
	}
}

func TestRankConversationFusedDegradesToLexicalWhenGateClosed(t *testing.T) {
	brainDir := writeConversationFilterFixture(t)
	source := &historySourceManifest{IndexPath: historyIndexPath}
	index, err := loadBrainHistoryIndex(brainDir, source)
	if err != nil {
		t.Fatal(err)
	}
	// nil embedder = gate closed: fused ranking must equal the lexical ranking
	// exactly (same list, same ok contract).
	fused, fusedOK := rankConversationFused(brainDir, index, "deploy pipeline", 10, nil)
	lex, lexOK := rankHistoryViaFTS(brainDir, index, conversationKind, "deploy pipeline", 10)
	if fusedOK != lexOK || len(fused) != len(lex) {
		t.Fatalf("degraded fusion differs from lexical: ok %v/%v len %d/%d", fusedOK, lexOK, len(fused), len(lex))
	}
	for i := range fused {
		if fused[i].Record.ID != lex[i].Record.ID {
			t.Fatalf("degraded fusion order differs at %d: %s vs %s", i, fused[i].Record.ID, lex[i].Record.ID)
		}
	}
}

func TestConversationVectorModeExplicitUnavailable(t *testing.T) {
	brainDir := writeConversationFilterFixture(t)
	// On this build/environment the conversation semantic arm is closed:
	// vsearch must return the structured unavailable error, and hybrid must
	// produce exactly the lexical results.
	if _, err := retrieveConversation(brainDir, "deploy pipeline", 10, modeVector, retrievalOptions{}); err == nil {
		t.Fatal("vector mode with a closed semantic arm must error explicitly")
	}
	hybrid, err := retrieveConversation(brainDir, "deploy pipeline", 10, modeHybrid, retrievalOptions{})
	if err != nil {
		t.Fatalf("hybrid: %v", err)
	}
	lexical, err := retrieveConversation(brainDir, "deploy pipeline", 10, modeLexical, retrievalOptions{})
	if err != nil {
		t.Fatalf("lexical: %v", err)
	}
	if len(hybrid) != len(lexical) {
		t.Fatalf("hybrid (degraded) differs from lexical: %d vs %d", len(hybrid), len(lexical))
	}
	for i := range hybrid {
		if hybrid[i].ID != lexical[i].ID {
			t.Fatalf("hybrid order differs at %d: %s vs %s", i, hybrid[i].ID, lexical[i].ID)
		}
	}
}

func TestBuildConversationStatusDisabledWithoutEmbedderOptIn(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_EMBEDDER", "")
	manifest := &exportManifest{Sources: &brainSources{History: &historySourceManifest{
		Exchanges: 42, IncompleteExchanges: 3,
	}}}
	status := buildConversationStatus(t.TempDir(), manifest)
	if status == nil || status.Exchanges != 42 || status.IncompleteExchanges != 3 {
		t.Fatalf("status = %+v", status)
	}
	if status.VectorState != "disabled" {
		t.Fatalf("vector state = %q, want disabled", status.VectorState)
	}
	if buildConversationStatus(t.TempDir(), &exportManifest{Sources: &brainSources{}}) != nil {
		t.Fatal("no history source must yield no conversation status block")
	}
}
