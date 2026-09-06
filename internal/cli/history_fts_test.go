package cli

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func ftsTestIndex() historyIndex {
	return historyIndex{
		GeneratedAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		Records: []historyRecord{
			{ID: "d1", Kind: "decision", Path: "sessions/main/20260601T000000Z_a.jsonl", Line: 1,
				Summary: "We chose the static embedding model over a transformer bi-encoder to keep the build pure Go."},
			{ID: "d2", Kind: "decision", Path: "sessions/main/20260601T000001Z_b.jsonl", Line: 2,
				Summary: "Reconcile decides to supersede a fact when a new claim contradicts an active one."},
			{ID: "t1", Kind: "tool_call", Path: "sessions/main/20260601T000002Z_c.jsonl", Line: 3,
				Summary: "apply_patch updated README.md with go test instructions."},
		},
	}
}

func ftsExcerpts(scored []scoredHistoryRecord) []string {
	out := make([]string, len(scored))
	for i, s := range scored {
		out[i] = s.Record.ID
	}
	return out
}

func writeDirectHistoryFTSFixture(t testing.TB, index historyIndex) (string, *historySourceManifest) {
	t.Helper()
	brainDir := t.TempDir()
	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	fingerprint := historyRecordsFingerprint(index.Records)
	source := &historySourceManifest{
		GeneratedAt:        index.GeneratedAt,
		IndexPath:          historyIndexPath,
		IndexBytes:         int64(len(data)),
		IndexSHA256:        historyIndexBytesFingerprint(data),
		RecordsFingerprint: fingerprint,
		Records:            len(index.Records),
	}
	if err := writeBrainRelativeFileAtomic(brainDir, historyIndexPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	index.recordsFingerprint = fingerprint
	db, err := openHistoryFTSLocked(brainDir, index)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writeBrainManifestAndReadme(brainDir, exportManifest{SchemaVersion: brainManifestSchemaVersion, Sources: &brainSources{History: source}}); err != nil {
		t.Fatal(err)
	}
	return brainDir, source
}

func TestHistoryFTSDirectHydrationMatchesIndexBackedRanking(t *testing.T) {
	index := ftsTestIndex()
	index.Records = append(index.Records,
		historyRecord{ID: "d3", Kind: "decision", Branch: "feature", Path: "sessions/feature/20260601T000003Z_d.jsonl", Line: 4, Summary: "Embedding cache invalidation uses a model fingerprint.", Terms: []string{"EmbeddingCache", "model_fingerprint"}},
		historyRecord{ID: "d4", Kind: "decision", Path: "sessions/main/20260601T000004Z_e.jsonl", Line: 5, Summary: "Embedding cache invalidation uses a model fingerprint."}, // dedup tail
		historyRecord{ID: "r1", Kind: "request", Path: "sessions/main/20260601T000005Z_f.jsonl", Line: 6, Summary: "Please explain embedding cache invalidation."},
	)
	brainDir, source := writeDirectHistoryFTSFixture(t, index)

	for _, tc := range []struct {
		kind  string
		query string
	}{
		{kind: "history", query: "embedding cache invalidation fingerprint"},
		{kind: "decisions", query: "EmbeddingCache model_fingerprint"},
		{kind: "tool-paths", query: "go test README apply_patch"},
		{kind: "requests", query: "embedding cache invalidation"},
		{kind: "history", query: "xylophone zebra quokka"},
	} {
		want, ok := rankHistoryViaFTS(brainDir, index, tc.kind, tc.query, 25)
		if !ok {
			t.Fatalf("index-backed %s/%q unavailable", tc.kind, tc.query)
		}
		got, used, err := rankHistoryViaFreshFTS(brainDir, source, tc.kind, tc.query, 25)
		if err != nil || !used {
			t.Fatalf("direct %s/%q: used=%v err=%v", tc.kind, tc.query, used, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("direct parity %s/%q\n got: %#v\nwant: %#v", tc.kind, tc.query, got, want)
		}
	}
}

func TestHistoryFTSDirectPathDoesNotLoadJSON(t *testing.T) {
	index := ftsTestIndex()
	brainDir, source := writeDirectHistoryFTSFixture(t, index)
	loads := 0
	loader := func(string, *historySourceManifest) (historyIndex, error) {
		loads++
		return historyIndex{}, errors.New("JSON loader must not run")
	}
	got, access, inputs, err := rankHistoryLexicalFromSourceWithLoader(brainDir, source, "decisions", "embedding model", 25, loader)
	if err != nil || loads != 0 || access != historyIndexAccessFTSPayload || inputs != len(index.Records) || len(got) == 0 {
		t.Fatalf("direct path: loads=%d access=%q inputs=%d matches=%v err=%v", loads, access, inputs, ftsExcerpts(got), err)
	}
}

func TestHistoryFTSPreservesReplacementScopedDuplicateIDs(t *testing.T) {
	sharedID := conversationIDPrefix + "shared-turn"
	index := historyIndex{
		GeneratedAt: time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC),
		Records: []historyRecord{
			{ID: sharedID, Kind: conversationKind, Branch: "main", SessionID: "session-1", Path: "sessions/main/first.jsonl", Line: 10, Summary: "Alpha conversation covers the shared retrieval marker."},
			{ID: sharedID, Kind: conversationKind, Branch: "feature", SessionID: "session-1", Path: "sessions/feature/second.jsonl", Line: 20, Summary: "Beta conversation covers the shared retrieval marker."},
		},
	}
	brainDir, source := writeDirectHistoryFTSFixture(t, index)

	got, used, err := rankHistoryViaFreshFTSCutoff(brainDir, source, conversationKind, "shared retrieval marker", 10, 0)
	if err != nil || !used {
		t.Fatalf("direct duplicate-ID ranking: used=%v err=%v", used, err)
	}
	if len(got) != 2 {
		t.Fatalf("replacement-scoped duplicate IDs were collapsed or rejected: %+v", got)
	}
	orders := map[int]bool{}
	branches := map[string]bool{}
	for _, match := range got {
		if match.Record.ID != sharedID {
			t.Fatalf("unexpected hydrated ID: %+v", match)
		}
		orders[match.Order] = true
		branches[match.Record.Branch] = true
	}
	if !orders[0] || !orders[1] || !branches["main"] || !branches["feature"] {
		t.Fatalf("physical row identity was not preserved: orders=%v branches=%v", orders, branches)
	}

	db, err := openHistoryFTSIfFresh(brainDir, index)
	if err != nil || db == nil {
		t.Fatalf("open duplicate-ID store: db=%v err=%v", db, err)
	}
	defer db.Close()
	var rows, distinctIDs int
	if err := db.QueryRow(`SELECT count(*), count(DISTINCT id) FROM history_records`).Scan(&rows, &distinctIDs); err != nil {
		t.Fatal(err)
	}
	if rows != 2 || distinctIDs != 1 {
		t.Fatalf("payload rows=%d distinct IDs=%d, want 2 physical rows sharing 1 ID", rows, distinctIDs)
	}
}

func TestHistoryFTSExhaustiveQueryUsesPayloadMetadata(t *testing.T) {
	brainDir := t.TempDir()
	index := historyIndex{Records: []historyRecord{
		{ID: conversationIDPrefix + "one-term", Kind: conversationKind, Path: "sessions/main/one.jsonl", Line: 1, Summary: "alpha only"},
		{ID: conversationIDPrefix + "both-terms", Kind: conversationKind, Path: "sessions/main/both.jsonl", Line: 1, Summary: "alpha and beta"},
	}}

	got, state, ok, scanned := rankHistoryViaFTSExhaustiveFiltered(brainDir, index, conversationKind, "alpha beta", 10, nil)
	if !ok || state != historyExhaustiveRankComplete || scanned == 0 {
		t.Fatalf("direct exhaustive FTS unavailable: ok=%v state=%v scanned=%d", ok, state, scanned)
	}
	if len(got) != 1 || got[0].Record.ID != conversationIDPrefix+"both-terms" {
		t.Fatalf("direct exhaustive FTS matches = %+v", got)
	}
}

func TestHistoryFTSDirectStaleAndMalformedPayloadFallBack(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mutate     func(*testing.T, string)
		wholeDBBad bool
	}{
		{
			name: "stale metadata",
			mutate: func(t *testing.T, brainDir string) {
				db, err := sql.Open(sqliteDriverName, historyFTSDBPath(brainDir))
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				if _, err := db.Exec(`UPDATE history_fts_meta SET value = ? WHERE key = 'records_fingerprint'`, "sha256:"+strings.Repeat("0", 64)); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "malformed payload",
			mutate: func(t *testing.T, brainDir string) {
				db, err := sql.Open(sqliteDriverName, historyFTSDBPath(brainDir))
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				if _, err := db.Exec(`UPDATE history_records SET terms_json = '{' WHERE id = 'd1'`); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:       "corrupt sqlite",
			wholeDBBad: true,
			mutate: func(t *testing.T, brainDir string) {
				if err := os.WriteFile(historyFTSDBPath(brainDir), []byte("not a sqlite database"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "aliased record order",
			mutate: func(t *testing.T, brainDir string) {
				db, err := sql.Open(sqliteDriverName, historyFTSDBPath(brainDir))
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				if _, err := db.Exec(`PRAGMA ignore_check_constraints=ON`); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`UPDATE history_records SET rec_order = 99 WHERE id = 'd1'`); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			index := ftsTestIndex()
			brainDir, source := writeDirectHistoryFTSFixture(t, index)
			want, ok := rankHistoryViaFTS(brainDir, index, "decisions", "embedding model", 25)
			if !ok {
				t.Fatal("precondition: index-backed BM25 unavailable")
			}
			tc.mutate(t, brainDir)
			if tc.wholeDBBad {
				want = rankHistoryRecordsScored(index, "decisions", "embedding model", 25, 0)
			}
			if _, used, directErr := rankHistoryViaFreshFTS(brainDir, source, "decisions", "embedding model", 25); directErr != nil || used {
				t.Fatalf("derived corruption must be a soft direct miss: used=%v err=%v", used, directErr)
			}
			loads := 0
			loader := func(string, *historySourceManifest) (historyIndex, error) {
				loads++
				return index, nil
			}
			got, access, _, err := rankHistoryLexicalFromSourceWithLoader(brainDir, source, "decisions", "embedding model", 25, loader)
			if err != nil || loads != 1 || access != historyIndexAccessJSON || !reflect.DeepEqual(got, want) {
				t.Fatalf("fallback: loads=%d access=%q matches=%v err=%v", loads, access, ftsExcerpts(got), err)
			}
		})
	}
}

func TestHistoryFTSReadOnlyMissDoesNotCreateCache(t *testing.T) {
	index := ftsTestIndex()
	brainDir := t.TempDir()
	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	source := &historySourceManifest{
		GeneratedAt:        index.GeneratedAt,
		IndexPath:          historyIndexPath,
		IndexBytes:         int64(len(data)),
		IndexSHA256:        historyIndexBytesFingerprint(data),
		RecordsFingerprint: historyRecordsFingerprint(index.Records),
		Records:            len(index.Records),
	}
	if err := writeBrainRelativeFileAtomic(brainDir, historyIndexPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(historyFTSDBPath(brainDir)); !os.IsNotExist(err) {
		t.Fatalf("precondition: FTS cache exists: %v", err)
	}
	if _, used, err := rankHistoryViaFreshFTS(brainDir, source, "decisions", "embedding", 25); err != nil || used {
		t.Fatalf("missing cache: used=%v err=%v", used, err)
	}
	if _, err := os.Stat(historyFTSDBPath(brainDir)); !os.IsNotExist(err) {
		t.Fatalf("read-only miss created FTS cache: %v", err)
	}
}

func TestHistoryFTSDirectSizeMismatchIsHardWithoutWriter(t *testing.T) {
	index := ftsTestIndex()
	brainDir, source := writeDirectHistoryFTSFixture(t, index)
	manifest := exportManifest{SchemaVersion: brainManifestSchemaVersion, Sources: &brainSources{History: source}}
	if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(brainDir, filepath.FromSlash(historyIndexPath))
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(" "); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, used, err := rankHistoryViaFreshFTS(brainDir, source, "decisions", "embedding", 25); err == nil || used {
		t.Fatalf("out-of-band size change: used=%v err=%v", used, err)
	}
}

func TestHistoryFTSDirectServesMatchingSnapshotDuringRefresh(t *testing.T) {
	index := ftsTestIndex()
	brainDir, source := writeDirectHistoryFTSFixture(t, index)
	unlock, err := acquireBrainWriteLock(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	path := filepath.Join(brainDir, filepath.FromSlash(historyIndexPath))
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(" next-generation-in-progress"); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	got, used, err := rankHistoryViaFreshFTS(brainDir, source, "decisions", "embedding model", 25)
	if err != nil || !used || len(got) == 0 {
		t.Fatalf("old published snapshot unavailable during refresh: used=%v matches=%v err=%v", used, ftsExcerpts(got), err)
	}
}

func TestHistoryIndexSourceIdentityDetectsTamper(t *testing.T) {
	index := ftsTestIndex()
	brainDir, source := writeDirectHistoryFTSFixture(t, index)
	if _, err := loadBrainHistoryIndex(brainDir, source); err != nil {
		t.Fatalf("valid source rejected: %v", err)
	}
	path := filepath.Join(brainDir, filepath.FromSlash(historyIndexPath))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := range data {
		if data[i] == 's' {
			data[i] = 'S' // preserve byte length while changing exact content
			break
		}
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadBrainHistoryIndex(brainDir, source); err == nil {
		t.Fatal("same-size history index tamper passed source checksum validation")
	}
}

func TestHistoryFTSDirectSecurityAndAuthoritativeFailures(t *testing.T) {
	index := ftsTestIndex()

	t.Run("missing authoritative index is hard error", func(t *testing.T) {
		brainDir, source := writeDirectHistoryFTSFixture(t, index)
		if err := os.Remove(filepath.Join(brainDir, filepath.FromSlash(historyIndexPath))); err != nil {
			t.Fatal(err)
		}
		if _, used, err := rankHistoryViaFreshFTS(brainDir, source, "decisions", "embedding", 25); err == nil || used {
			t.Fatalf("missing truth: used=%v err=%v", used, err)
		}
	})

	t.Run("unsafe source path is hard error", func(t *testing.T) {
		brainDir, source := writeDirectHistoryFTSFixture(t, index)
		bad := *source
		bad.IndexPath = "../history/index.json"
		if _, used, err := rankHistoryViaFreshFTS(brainDir, &bad, "decisions", "embedding", 25); err == nil || used {
			t.Fatalf("unsafe truth: used=%v err=%v", used, err)
		}
	})

	t.Run("symlinked authoritative index is hard error", func(t *testing.T) {
		brainDir, source := writeDirectHistoryFTSFixture(t, index)
		path := filepath.Join(brainDir, filepath.FromSlash(historyIndexPath))
		outside := filepath.Join(t.TempDir(), "index.json")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(outside, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, path); err != nil {
			t.Fatal(err)
		}
		if _, used, err := rankHistoryViaFreshFTS(brainDir, source, "decisions", "embedding", 25); err == nil || used {
			t.Fatalf("symlink truth: used=%v err=%v", used, err)
		}
	})

	t.Run("symlinked derived cache falls back", func(t *testing.T) {
		brainDir, source := writeDirectHistoryFTSFixture(t, index)
		path := historyFTSDBPath(brainDir)
		outside := filepath.Join(t.TempDir(), "cache.sqlite")
		if err := os.WriteFile(outside, []byte("not sqlite"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, path); err != nil {
			t.Fatal(err)
		}
		loads := 0
		loader := func(string, *historySourceManifest) (historyIndex, error) {
			loads++
			return index, nil
		}
		got, access, _, err := rankHistoryLexicalFromSourceWithLoader(brainDir, source, "decisions", "embedding model", 25, loader)
		if err != nil || loads != 1 || access != historyIndexAccessJSON || len(got) == 0 {
			t.Fatalf("derived symlink fallback: loads=%d access=%q matches=%v err=%v", loads, access, ftsExcerpts(got), err)
		}
	})
}

// TestHistoryFTSSurfacesLowCoverageParaphrase locks in the Stage 1a fix as a
// contrast: a multi-term natural-language query whose relevant record shares only
// a single distinctive term ("embedding") is dropped entirely by the substring
// scorer's 3-of-N coverage gate (the matches:null defect), but surfaced by BM25,
// which scores the rare term instead of gating on coverage. (Rank-order quality
// needs a realistic corpus and is validated by eval, not a 3-doc unit fixture.)
func TestHistoryFTSSurfacesLowCoverageParaphrase(t *testing.T) {
	brainDir := t.TempDir()
	index := ftsTestIndex()
	query := "reasons we picked one embedding approach over another"

	// Precondition: the coverage gate returns nothing for this low-overlap query.
	if gated := rankHistoryRecordsScored(index, "decisions", query, 25, 0); len(gated) != 0 {
		t.Fatalf("precondition: expected coverage gate to drop the query, got %v", ftsExcerpts(gated))
	}

	scored, ok := rankHistoryViaFTS(brainDir, index, "decisions", query, 25)
	if !ok {
		t.Fatal("rankHistoryViaFTS returned ok=false; expected BM25 results")
	}
	found := false
	for _, s := range scored {
		if s.Record.ID == "d1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected BM25 to surface d1, got %v", ftsExcerpts(scored))
	}
}

// TestHistoryFTSHonestEmpty verifies a query whose terms appear in no record
// returns nothing — the strict-contract property the precision surface depends on.
func TestHistoryFTSHonestEmpty(t *testing.T) {
	brainDir := t.TempDir()
	index := ftsTestIndex()

	scored, ok := rankHistoryViaFTS(brainDir, index, "decisions", "xylophone zebra quokka", 25)
	if !ok {
		t.Fatal("expected ok=true for a searchable (if unmatched) query")
	}
	if len(scored) != 0 {
		t.Fatalf("expected no matches for absent terms, got %v", ftsExcerpts(scored))
	}
}

// TestHistoryFTSKindFilter confirms the kind filter excludes other-kind records:
// a tool_call must not surface under kind=decisions even when it matches terms.
func TestHistoryFTSKindFilter(t *testing.T) {
	brainDir := t.TempDir()
	index := ftsTestIndex()

	scored, ok := rankHistoryViaFTS(brainDir, index, "decisions", "go test README apply_patch", 25)
	if !ok {
		t.Fatal("expected ok=true")
	}
	for _, s := range scored {
		if s.Record.Kind != "decision" {
			t.Fatalf("kind=decisions returned a %s record: %v", s.Record.Kind, ftsExcerpts(scored))
		}
	}

	scored, ok = rankHistoryViaFTS(brainDir, index, "tool-paths", "go test README apply_patch", 25)
	if !ok || len(scored) == 0 || scored[0].Record.ID != "t1" {
		t.Fatalf("expected t1 under tool-paths, got ok=%v %v", ok, ftsExcerpts(scored))
	}
}

// TestHistoryFTSRequestGating verifies user-prompt ("request") records are kept
// out of general history ranking (they add noise) but surface via the explicit
// `requests` kind.
func TestHistoryFTSRequestGating(t *testing.T) {
	brainDir := t.TempDir()
	index := historyIndex{
		GeneratedAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		Records: []historyRecord{
			{ID: "d1", Kind: "decision", Path: "sessions/main/20260601T000000Z_a.jsonl", Line: 1,
				Summary: "We chose FTS5 BM25 for history ranking."},
			{ID: "r1", Kind: "request", Path: "sessions/main/20260601T000000Z_a.jsonl", Line: 2,
				Summary: "How does history ranking work, should we use BM25?"},
		},
	}
	general, ok := rankHistoryViaFTS(brainDir, index, "history", "history ranking bm25", 25)
	if !ok {
		t.Fatal("general ok=false")
	}
	for _, s := range general {
		if s.Record.Kind == "request" {
			t.Fatalf("request record leaked into general history ranking: %v", ftsExcerpts(general))
		}
	}
	reqs, ok := rankHistoryViaFTS(brainDir, index, "requests", "history ranking bm25", 25)
	if !ok || len(reqs) == 0 || reqs[0].Record.ID != "r1" {
		t.Fatalf("requests kind should surface r1, got ok=%v %v", ok, ftsExcerpts(reqs))
	}
}

// TestHistoryFTSRebuildDeterministic verifies the derived index is a rebuildable
// artifact: a second open over the same truth returns identical ranking.
func TestHistoryFTSRebuildDeterministic(t *testing.T) {
	brainDir := t.TempDir()
	index := ftsTestIndex()
	query := "supersede a fact during reconcile"

	first, ok := rankHistoryViaFTS(brainDir, index, "decisions", query, 25)
	if !ok {
		t.Fatal("first query ok=false")
	}
	second, ok := rankHistoryViaFTS(brainDir, index, "decisions", query, 25)
	if !ok {
		t.Fatal("second query ok=false")
	}
	if len(first) != len(second) {
		t.Fatalf("nondeterministic length: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].Record.ID != second[i].Record.ID {
			t.Fatalf("nondeterministic order at %d: %v vs %v", i, ftsExcerpts(first), ftsExcerpts(second))
		}
	}
}

func TestHistoryFTSRebuildsForEqualTimeAndCountGeneration(t *testing.T) {
	brainDir := t.TempDir()
	generatedAt := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	first := historyIndex{
		GeneratedAt: generatedAt,
		Records: []historyRecord{{
			ID: "old", Kind: "decision", Path: "sessions/main/old.jsonl", Line: 1,
			Summary: "PRIVATE-OLD-GENERATION-CANARY alpha",
		}},
		storageIdentity: "history/generations/v1/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/index.json",
		contentIdentity: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	if scored, ok := rankHistoryViaFTS(brainDir, first, "decisions", "alpha", 10); !ok || len(scored) != 1 || scored[0].Record.ID != "old" {
		t.Fatalf("seed generation ranking: ok=%v scored=%+v", ok, scored)
	}

	second := historyIndex{
		GeneratedAt: generatedAt,
		Records: []historyRecord{{
			ID: "new", Kind: "decision", Path: "sessions/main/new.jsonl", Line: 1,
			Summary: "replacement generation beta",
		}},
		storageIdentity: "history/generations/v1/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb/index.json",
		contentIdentity: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}
	if scored, ok := rankHistoryViaFTS(brainDir, second, "decisions", "beta", 10); !ok || len(scored) != 1 || scored[0].Record.ID != "new" {
		t.Fatalf("replacement generation did not rebuild FTS: ok=%v scored=%+v", ok, scored)
	}
	if stale, ok := rankHistoryViaFTS(brainDir, second, "decisions", "alpha", 10); !ok || len(stale) != 0 {
		t.Fatalf("old generation canary survived replacement: ok=%v scored=%+v", ok, stale)
	}
}

// TestBriefFocusedHistoryNeverRebuildsFTSFromMergedRecords locks the contract
// behind Bugbot PR #77: the on-disk BM25 store must only ever rank the
// LONG-TERM tier.
//
// openHistoryFTS keys freshness on historyFTSIdentityFromIndex (records
// fingerprint + count). Handing it a merged (short-term-inclusive) index
// therefore does not just read the wrong rows: the identity mismatches, so the
// store is REBUILT from the merged set, and every later long-term reader finds
// it stale and rebuilds it back. This test drives the real
// brainBriefFocusedHistoryMatches against a two-tier view and proves the store
// is still fresh for the long-term index afterwards. Passing the merged set
// (the pre-fix behavior) fails it.
func TestBriefFocusedHistoryNeverRebuildsFTSFromMergedRecords(t *testing.T) {
	brainDir, _, _ := shortTermFixture(t)
	buildShortTerm(t, brainDir)
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := loadFreshHistory(brainDir, manifest.Sources.History)
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh.overlay) == 0 {
		t.Fatal("fixture invalid: the two-tier view needs a non-empty overlay")
	}
	longTerm := fresh.index
	merged := historyIndex{GeneratedAt: longTerm.GeneratedAt, Records: fresh.mergedRecords()}
	if historyFTSIdentityFromIndex(longTerm) == historyFTSIdentityFromIndex(merged) {
		t.Fatal("fixture invalid: the merged and long-term identities must differ for this to prove anything")
	}

	// Prime the store from the long-term tier. Skip where SQLite/FTS5 is not
	// compiled in: there is no store to corrupt.
	primed, err := openHistoryFTS(brainDir, longTerm)
	if err != nil || primed == nil {
		t.Skipf("history FTS unavailable in this build: %v", err)
	}
	primed.Close()

	matches := brainBriefFocusedHistoryMatches(
		brainDir,
		fresh,
		semanticRecord{Name: "gzip", QualifiedName: "gzip"},
		5,
		sessionReadGuard{},
	)
	_ = matches // ranking output is not the contract under test; the store identity is.

	after, err := openHistoryFTSIfFresh(brainDir, longTerm)
	if err != nil {
		t.Fatalf("history FTS unreadable after the focused lookup: %v", err)
	}
	if after == nil {
		t.Fatal("focused history left the BM25 store stale for the long-term index: it was rebuilt from a merged record set")
	}
	after.Close()
}

// Removing UNIQUE(id) is read-compatible with payload v1. Existing caches with
// unique IDs can remain fresh; changing the source rebuilds the whole table,
// rather than inserting new records into the legacy schema.
func TestHistoryFTSLegacyUniquePayloadRebuildsForDuplicateIDs(t *testing.T) {
	index := historyIndex{Records: []historyRecord{
		{ID: conversationIDPrefix + "shared", Kind: conversationKind, Branch: "main", Path: "sessions/main/one.jsonl", Line: 1, Summary: "alpha shared retrieval marker"},
	}}
	brainDir, source := writeDirectHistoryFTSFixture(t, index)
	db, err := openHistoryFTSIfFresh(brainDir, index)
	if err != nil || db == nil {
		t.Fatalf("open fixture: db=%v err=%v", db, err)
	}
	defer db.Close()
	var schema string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE name = 'history_records'`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	legacySchema := strings.Replace(schema, "id TEXT NOT NULL,", "id TEXT NOT NULL UNIQUE,", 1)
	if legacySchema == schema {
		t.Fatal("fixture could not restore the old UNIQUE(id) table definition")
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, stmt := range []string{
		`ALTER TABLE history_records RENAME TO saved_records`,
		legacySchema,
		`INSERT INTO history_records SELECT * FROM saved_records`,
		`DROP TABLE saved_records`,
		`UPDATE history_fts_meta SET value = '1' WHERE key = 'payload_schema'`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			t.Fatalf("restore legacy schema: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if !historyFTSFresh(db, index) {
		t.Fatal("unchanged history should reuse the read-compatible legacy cache")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	got, used, err := rankHistoryViaFreshFTSCutoff(brainDir, source, conversationKind, "shared retrieval marker", 10, 0)
	if err != nil || !used || len(got) != 1 {
		t.Fatalf("legacy direct read: used=%v matches=%d err=%v", used, len(got), err)
	}

	index.Records = append(index.Records, historyRecord{
		ID: index.Records[0].ID, Kind: conversationKind, Branch: "feature", Path: "sessions/feature/two.jsonl", Line: 1, Summary: "beta shared retrieval marker",
	})
	fresh, err := openHistoryFTSIfFresh(brainDir, index)
	if fresh != nil {
		fresh.Close()
		t.Fatal("changed source incorrectly reused the legacy cache")
	}
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err := openHistoryFTS(brainDir, index)
	if err != nil {
		t.Fatalf("rebuild for duplicate IDs: %v", err)
	}
	defer rebuilt.Close()
	var rows, ids int
	if err := rebuilt.QueryRow(`SELECT count(*), count(DISTINCT id) FROM history_records`).Scan(&rows, &ids); err != nil {
		t.Fatal(err)
	}
	if rows != 2 || ids != 1 || !historyFTSFresh(rebuilt, index) {
		t.Fatalf("rebuilt cache: rows=%d distinct IDs=%d; want two fresh rows sharing an ID", rows, ids)
	}
}
