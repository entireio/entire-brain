package cli

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func benchmarkHistoryFTSIndex(b *testing.B) historyIndex {
	b.Helper()
	if path := os.Getenv("ENTIRE_BRAIN_HISTORY_BENCH_INDEX"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			b.Fatal(err)
		}
		var index historyIndex
		if err := json.Unmarshal(data, &index); err != nil {
			b.Fatal(err)
		}
		return index
	}
	n := 60000
	if raw := os.Getenv("ENTIRE_BRAIN_HISTORY_BENCH_RECORDS"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			b.Fatalf("ENTIRE_BRAIN_HISTORY_BENCH_RECORDS=%q", raw)
		}
		n = parsed
	}
	index := historyIndex{GeneratedAt: time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC), Records: make([]historyRecord, 0, n)}
	for i := 0; i < n; i++ {
		kind := "decision"
		if i%7 == 0 {
			kind = "tool_call"
		}
		index.Records = append(index.Records, historyRecord{
			ID:      fmt.Sprintf("history:%024x", i+1),
			Kind:    kind,
			Branch:  "main",
			Path:    fmt.Sprintf("sessions/main/20260716T000000Z_%08d.jsonl", i/20),
			Line:    i%20 + 1,
			Summary: fmt.Sprintf("Decision %d validates embedding cache invalidation and model fingerprint behavior for component_%d with deterministic regression coverage.", i, i%997),
			Terms:   []string{"EmbeddingCache", "model_fingerprint", fmt.Sprintf("component_%d", i%997)},
		})
	}
	return index
}

func BenchmarkHistoryFTSQueryPaths(b *testing.B) {
	index := benchmarkHistoryFTSIndex(b)
	brainDir, source := writeDirectHistoryFTSFixture(b, index)
	query := os.Getenv("ENTIRE_BRAIN_HISTORY_BENCH_QUERY")
	if query == "" {
		query = "embedding cache invalidation fingerprint"
	}

	b.Run("json_load_then_bm25", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			loaded, err := loadBrainHistoryIndex(brainDir, source)
			if err != nil {
				b.Fatal(err)
			}
			if _, ok := rankHistoryViaFTS(brainDir, loaded, "history", query, 10); !ok {
				b.Fatal("BM25 unavailable")
			}
		}
	})
	b.Run("direct_v2_payload", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, used, err := rankHistoryViaFreshFTS(brainDir, source, "history", query, 10); err != nil || !used {
				b.Fatalf("direct BM25: used=%v err=%v", used, err)
			}
		}
	})
}

// BenchmarkHistoryLegacyIdentityUpgrade measures the one-time legacy fallback
// against the next invocation's direct payload path. Manifest reset is outside
// the timed region; the first arm includes full truth load, identity derivation,
// schema-v2 cache validation and ranking, locking, revalidation, and atomic
// migration.
func BenchmarkHistoryLegacyIdentityUpgrade(b *testing.B) {
	index := benchmarkHistoryFTSIndex(b)
	brainDir, strong := writeDirectHistoryFTSFixture(b, index)
	legacy := *strong
	legacy.IndexBytes = 0
	legacy.IndexSHA256 = ""
	legacy.RecordsFingerprint = ""
	manifest := exportManifest{SchemaVersion: brainManifestSchemaVersion, Sources: &brainSources{History: &legacy}}
	query := "embedding cache invalidation fingerprint"

	b.Run("first_legacy_upgrade", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			b.StopTimer()
			if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
				b.Fatal(err)
			}
			b.StartTimer()
			_, identity, err := loadBrainHistoryIndexWithLegacyIdentity(brainDir, &legacy)
			if err != nil || identity == nil {
				b.Fatalf("legacy load: identity=%+v err=%v", identity, err)
			}
			if _, used := rankHistoryViaLegacyDirectPayload(brainDir, &legacy, identity, "history", query, 10, historyFTSRelevanceCutoff); !used {
				b.Fatal("legacy direct payload unavailable")
			}
		}
	})

	upgraded, err := loadBrainManifest(brainDir)
	if err != nil {
		b.Fatal(err)
	}
	b.Run("second_direct", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, used, err := rankHistoryViaFreshFTS(brainDir, upgraded.Sources.History, "history", query, 10); err != nil || !used {
				b.Fatalf("direct payload: used=%v err=%v", used, err)
			}
		}
	})
}

func BenchmarkHistoryLegacyIdentityLockRecheck(b *testing.B) {
	index := benchmarkHistoryFTSIndex(b)
	brainDir, source := writeDirectHistoryFTSFixture(b, index)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fingerprint, err := historyIndexFingerprintForIdentityRecheck(brainDir, source.IndexPath, source.IndexBytes)
		if err != nil || fingerprint != source.IndexSHA256 {
			b.Fatalf("stream identity: fingerprint=%q err=%v", fingerprint, err)
		}
	}
}

func BenchmarkHistoryFTSStorageDelta(b *testing.B) {
	b.StopTimer()
	index := benchmarkHistoryFTSIndex(b)
	v2Brain, _ := writeDirectHistoryFTSFixture(b, index)
	v2Bytes := sqliteArtifactBytes(historyFTSDBPath(v2Brain))

	v1Path := filepath.Join(b.TempDir(), "history-v1.sqlite")
	if err := buildHistoryFTSV1Fixture(v1Path, index); err != nil {
		b.Fatal(err)
	}
	v1Bytes := sqliteArtifactBytes(v1Path)
	b.ReportMetric(float64(v1Bytes), "v1_bytes")
	b.ReportMetric(float64(v2Bytes), "v2_bytes")
	b.ReportMetric(100*(float64(v2Bytes)-float64(v1Bytes))/float64(v1Bytes), "v2_delta_pct")
	b.StartTimer()
	for i := 0; i < b.N; i++ {
		// Artifacts are built outside the timed region; this benchmark reports
		// their exact closed-database footprint through custom metrics.
	}
}

func sqliteArtifactBytes(path string) int64 {
	var total int64
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		if info, err := os.Stat(candidate); err == nil {
			total += info.Size()
		}
	}
	return total
}

func buildHistoryFTSV1Fixture(path string, index historyIndex) error {
	db, err := sql.Open(sqliteDriverName, path)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE VIRTUAL TABLE history_fts USING fts5(content, kind UNINDEXED, rec_order UNINDEXED, tokenize='porter unicode61')`); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO history_fts(content, kind, rec_order) VALUES (?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for i, record := range index.Records {
		if _, err := stmt.Exec(historyFTSContent(record), record.Kind, i); err != nil {
			return err
		}
	}
	return tx.Commit()
}
