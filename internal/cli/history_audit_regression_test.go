package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestHistoryAuditDeclaredDocLossFailsExpansion(t *testing.T) {
	dir, m := declaredIndexBrain(t)
	if err := writeBrainManifestAndReadme(dir, *m); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, filepath.FromSlash(docIndexPath))); err != nil {
		t.Fatal(err)
	}
	if _, _, err := getUnifiedBatch("", dir, "main", []string{"doc:d1"}); err == nil {
		t.Fatal("declared doc index loss became an unknown ID")
	}
	if _, missing, err := getUnifiedBatch("", t.TempDir(), "main", []string{"doc:d1"}); err != nil || len(missing) != 1 {
		t.Fatalf("never indexed docs: %v %v", missing, err)
	}
}

func TestHistoryAuditFailedEmbeddingUnavailable(t *testing.T) {
	for _, v := range [][]float32{nil, {0, 0}, {1}, {float32(math.NaN()), 1}, {float32(math.Inf(1)), 0}} {
		query := "whitespace convention"
		e := &fixedEmbedder{dim: 2, vecs: map[string][]float32{query: {1, 0}, "unrelated": v}}
		idx := historyIndex{Records: []historyRecord{{ID: "noise", Kind: "decision", Summary: "unrelated"}}}
		f := newHistoryFusedRanker(t.TempDir(), idx, e, .3)
		got, err := f.rank(query, 1)
		if err == nil || len(got) != 0 {
			t.Errorf("invalid candidate %v became semantic evidence: %v %v", v, got, err)
		}
		if len(f.vecs) != 0 {
			t.Errorf("invalid vector cached: %v", f.vecs)
		}
		e.vecs[query] = v
		e.vecs["unrelated"] = []float32{1, 0}
		if got, err := f.rank(query, 1); err == nil || len(got) != 0 {
			t.Errorf("invalid query %v became semantic evidence: %v %v", v, got, err)
		}
	}
}

func TestHistoryAuditScannerPreservesClassifier(t *testing.T) {
	for _, s := range []struct{ role, text, kind string }{{"user", "How many buckets?", "request"}, {"assistant", "I prefer Redis.", "decision"}, {"assistant", "The bug is a null pointer.", "learning"}} {
		raw, _ := json.Marshal(map[string]any{"type": s.role, "message": map[string]any{"role": s.role, "content": s.text}})
		got, err := scanHistoryFileReader(context.Background(), strings.NewReader(string(raw)), "sessions/main/test.jsonl", ".jsonl")
		found := false
		for _, r := range got {
			found = found || r.Kind == s.kind
		}
		if err != nil || !found {
			t.Errorf("scanner lost %s %q: %v %v", s.kind, s.text, got, err)
		}
	}
	doc := historyDocumentRecords("sessions/main/document.json", []documentMessage{{Role: "user", Text: "How many buckets?", Line: 1}})
	if len(doc) != 1 || doc[0].Kind != "request" {
		t.Errorf("document request lost: %v", doc)
	}
}

func TestHistoryAuditRetiredGenerationRecovers(t *testing.T) {
	dir, _, now := historyProjectionFixture(t)
	old, _ := activeHistoryProjection(t, dir)
	if _, err := writeBrainHistoryIndexAndSource(dir, now.Add(time.Minute), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := loadBrainHistoryIndex(dir, old); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := rankHistoryLexicalFromSource(dir, old, "history", "manifest", 10); err != nil {
		t.Fatalf("retired generation did not recover: %v", err)
	}
	current, _ := activeHistoryProjection(t, dir)
	if err := os.Remove(filepath.Join(dir, filepath.FromSlash(current.IndexPath))); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := rankHistoryLexicalFromSource(dir, current, "history", "manifest", 10); err == nil {
		t.Fatal("missing current generation accepted")
	}
}

func TestHistoryAuditLongFirstLineStreams(t *testing.T) {
	d := t.TempDir()
	rel := "sessions/main/a.jsonl"
	p := filepath.Join(d, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"type":"user","message":{"role":"user","content":"` + strings.Repeat("a", 5000) + `"}}` + "\n" + `{"type":"assistant","message":{"role":"assistant","content":"response"}}` + "\n" + strings.Repeat("{}\n", 4000))
	if err := os.WriteFile(p, data, 0600); err != nil {
		t.Fatal(err)
	}
	old := maxDocumentTranscriptBytes
	maxDocumentTranscriptBytes = 8192
	t.Cleanup(func() { maxDocumentTranscriptBytes = old })
	got, err := expandConversationExchange(d, historyRecord{Path: rel, Line: 1, EndLine: 2, SourceDigest: conversationDigest(data)})
	if err != nil || got.Response != "response" {
		t.Fatalf("long JSONL first line: %+v %v", got, err)
	}
}

func TestHistoryAuditUnicodeExcerpt(t *testing.T) {
	for _, prefix := range []string{strings.Repeat("Ⱥ", 400), strings.Repeat("İ", 400), strings.Repeat("😀", 400)} {
		got := historyRawLineExcerpt(prefix+" target after", "TARGET")
		if !utf8.ValidString(got) || !strings.Contains(got, "target") {
			t.Fatalf("invalid Unicode excerpt: %q", got)
		}
	}
}

func TestHistoryAuditFTSGenerationCannotMisaddressRecords(t *testing.T) {
	for _, exhaustive := range []bool{false, true} {
		t.Run(map[bool]string{false: "filtered", true: "exhaustive"}[exhaustive], func(t *testing.T) {
			dir := t.TempDir()
			old := historyIndex{Records: []historyRecord{{ID: "old", Kind: "decision", Summary: "unrelated banana"}}}
			next := historyIndex{Records: []historyRecord{{ID: "new", Kind: "decision", Summary: "quasar"}}}
			original := openHistoryFTSForRanking
			openHistoryFTSForRanking = func(brainDir string, index historyIndex) (*sql.DB, error) {
				db, err := original(brainDir, index)
				if err != nil {
					return nil, err
				}
				newer, err := openHistoryFTSLocked(dir, next)
				if err != nil {
					db.Close()
					return nil, err
				}
				newer.Close()
				return db, nil
			}
			t.Cleanup(func() { openHistoryFTSForRanking = original })
			var got []scoredHistoryRecord
			if exhaustive {
				got, _, _, _ = rankHistoryViaFTSExhaustiveFiltered(dir, old, "history", "quasar", 10, nil)
			} else {
				got, _, _ = rankHistoryViaFTSFiltered(dir, old, "history", "quasar", 10, 0, nil)
			}
			if len(got) != 0 {
				t.Fatalf("new-generation ordinal resolved against old records: %+v", got)
			}
		})
	}
}

func TestHistoryAuditEmptyReplacementHidesAllOldViews(t *testing.T) {
	brainDir, changedRel, newRel := shortTermFixture(t)
	if err := os.Remove(filepath.Join(brainDir, filepath.FromSlash(newRel))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brainDir, filepath.FromSlash(changedRel)), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := buildShortTermResult(t, brainDir); err != nil {
		t.Fatal(err)
	}
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := loadFreshHistory(brainDir, manifest.Sources.History)
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh.replaced) != 1 || len(fresh.overlay) != 0 || len(fresh.index.Records) == 0 {
		t.Fatal("invalid empty replacement fixture")
	}
	if len(fresh.mergedRecords()) != 0 || len(fresh.longTermActive().Records) != 0 {
		t.Error("old history survived empty replacement")
	}
	hits, err := retrieveUnifiedWithOptions("", brainDir, "main", "lock timeout", 10, modeLexical, retrievalOptions{Source: retrievalSourceHistory})
	if err != nil || len(hits) != 0 {
		t.Fatalf("empty replacement retrieval: %v %v", hits, err)
	}
}

func TestHistoryAuditReplacementEligibilityBeforeLimit(t *testing.T) {
	rows := []historyRecord{
		{ID: "old1", Kind: "decision", Path: "changed", Summary: "cache coherence"},
		{ID: "old2", Kind: "decision", Path: "changed", Summary: "cache coherence"},
		{ID: "keep", Kind: "decision", Path: "kept", Summary: "cache coherence"},
	}
	fresh := freshHistory{index: historyIndex{Records: rows}, replaced: map[string]bool{"changed": true}, overlay: []historyRecord{{ID: "new", Kind: "decision", Path: "changed", Summary: "unrelated"}}}
	got := rankFreshHistory(fresh, "history", "cache coherence", 2, nil, func(historyIndex, func(historyRecord) bool) ([]scoredHistoryRecord, bool) { return nil, false })
	if len(got) != 1 || got[0].Record.ID != "keep" {
		t.Fatalf("eligible match displaced by stale rows: %+v", got)
	}
	dir := t.TempDir()
	got = rankFreshHistory(fresh, "history", "cache coherence", 2, nil, func(index historyIndex, pred func(historyRecord) bool) ([]scoredHistoryRecord, bool) {
		ranked, complete, ok := rankHistoryViaFTSFiltered(dir, index, "history", "cache coherence", 2, 0, pred)
		return ranked, complete && ok
	})
	if len(got) != 1 || got[0].Record.ID != "keep" {
		t.Fatalf("FTS eligibility lost match: %+v", got)
	}
	payloadDir, source := writeDirectHistoryFTSFixture(t, fresh.index)
	got, used, err := rankHistoryViaFreshFTSCutoffDetailed(payloadDir, source, "history", "cache coherence", 2, 0, func(r historyRecord) bool { return !fresh.replaced[r.Path] })
	if err != nil || !used || len(got) != 1 || got[0].Record.ID != "keep" {
		t.Fatalf("payload eligibility lost match: %+v %t %v", got, used, err)
	}

}

func TestHistoryAuditNoProposalsAllocatesNoReviewGraph(t *testing.T) {
	facts := make([]factRecord, 100)
	for i := range facts {
		facts[i] = factRecord{ID: fmt.Sprint(i), Status: factStatusActive}
	}
	allocations := testing.AllocsPerRun(10, func() {
		if len(buildFactReviewGroups(facts, nil)) != 0 {
			t.Fatal("unexpected groups")
		}
	})
	if allocations != 0 {
		t.Fatalf("empty proposal graph allocated %g times", allocations)
	}
}

func TestHistoryAuditProfilingMeasuresOnePhysicalScan(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions", "main", "s.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	data := []byte("unrelated payload\n")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	task := "ALPHA_ONE BETA_TWO GAMMA_THREE DELTA_FOUR EPSILON_FIVE ZETA_SIX ETA_SEVEN THETA_EIGHT"
	var profile brainBriefProfileRawHistory
	got, err := brainBriefRawHistoryMatchesObserved(dir, task, nil, 8, &profile)
	want, werr := brainBriefRawHistoryMatchesObserved(dir, task, nil, 8, nil)
	if err != nil || werr != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("profiling changed output: %v %v %v %v", got, want, err, werr)
	}
	if profile.ScannedFileCount != 1 || profile.ScannedByteCount != int64(len(data)) {
		t.Fatalf("profile measured repeated scans: %+v", profile)
	}
}

func TestHistoryAuditScanCacheCompressedBound(t *testing.T) {
	dir := t.TempDir()
	cache := historyScanCache{Version: historyScanCacheVersion, Files: map[string]historyScanCacheEntry{"kept": {}}}
	data, err := encodeHistoryScanCache(cache)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, historyScanCachePath)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ENTIRE_BRAIN_MAX_SNAPSHOT_BYTES", "2048")
	if len(loadHistoryScanCache(dir).Files) != 1 {
		t.Fatal("valid cache not reused")
	}
	data = append(data, make([]byte, 4096)...)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if len(loadHistoryScanCache(dir).Files) != 0 {
		t.Fatal("oversized compressed input accepted")
	}
}

func TestHistoryAuditEvidenceScoreIsTopicNeutral(t *testing.T) {
	for _, pair := range [][2]string{
		{"metadata value", "ordinary value"},
		{"self-contained browser", "self-contained worker"},
		{"openai docs", "ordinary docs"},
	} {
		a := historyRecordEvidenceScore(historyRecord{Summary: pair[0]}, 1, 0)
		b := historyRecordEvidenceScore(historyRecord{Summary: pair[1]}, 1, 0)
		if a != b {
			t.Errorf("topic alone changed evidence quality: %q=%d %q=%d", pair[0], a, pair[1], b)
		}
	}
}

func TestHistoryAuditOverlayRepairsCorruptPayload(t *testing.T) {
	dir, _, _ := shortTermFixture(t)
	buildShortTerm(t, dir)
	manifest, err := loadBrainManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	source := manifest.Sources.History
	_, _, inputs, err := rankFreshHistoryLexicalFromSource(dir, source, "history", "lock timeout", 10)
	if err != nil || inputs < source.Records {
		t.Fatalf("warm rank: inputs=%d err=%v", inputs, err)
	}
	db, err := sql.Open(sqliteDriverName, historyFTSDBPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE history_records SET terms_json = '{'`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	if _, _, _, err = rankFreshHistoryLexicalFromSource(dir, source, "history", "lock timeout", 10); err != nil {
		t.Fatalf("derived payload damage became fatal with overlay: %v", err)
	}
}
