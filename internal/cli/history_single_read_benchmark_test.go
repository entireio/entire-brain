package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func BenchmarkHistoryTranscriptRefresh(b *testing.B) {
	for _, fileCount := range []int{48, 512} {
		b.Run(fmt.Sprintf("full-rebuild/%d-files", fileCount), func(b *testing.B) {
			brainDir, corpusBytes := historyRefreshBenchmarkFixture(b, fileCount)
			cachePath := filepath.Join(brainDir, filepath.FromSlash(historyScanCachePath))
			now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
			b.ReportAllocs()
			b.SetBytes(corpusBytes)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := os.Remove(cachePath); err != nil && !os.IsNotExist(err) {
					b.Fatal(err)
				}
				if _, _, err := buildBrainHistoryIndex(brainDir, now, nil); err != nil {
					b.Fatal(err)
				}
			}
		})

		b.Run(fmt.Sprintf("unchanged/%d-files", fileCount), func(b *testing.B) {
			brainDir, corpusBytes := historyRefreshBenchmarkFixture(b, fileCount)
			now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
			if _, err := writeBrainHistoryIndexAndSource(brainDir, now, nil); err != nil {
				b.Fatal(err)
			}
			manifest, err := loadBrainManifest(brainDir)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(corpusBytes)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if !historyIndexCurrent(brainDir, manifest) {
					b.Fatal("unchanged history unexpectedly stale")
				}
			}
		})
	}
}

func BenchmarkHistoryTranscriptRefreshPaired(b *testing.B) {
	for _, fileCount := range []int{48, 512} {
		b.Run(fmt.Sprintf("%d-files", fileCount), func(b *testing.B) {
			brainDir, corpusBytes := historyRefreshBenchmarkFixture(b, fileCount)
			cachePath := filepath.Join(brainDir, filepath.FromSlash(historyScanCachePath))
			now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
			var hashThenScan, singleRead time.Duration
			run := func(single bool) {
				if err := os.Remove(cachePath); err != nil && !os.IsNotExist(err) {
					b.Fatal(err)
				}
				start := time.Now()
				if single {
					if _, _, err := buildBrainHistoryIndex(brainDir, now, nil); err != nil {
						b.Fatal(err)
					}
					singleRead += time.Since(start)
					return
				}
				if _, _, err := buildBrainHistoryIndexHashThenScanReference(brainDir, now); err != nil {
					b.Fatal(err)
				}
				hashThenScan += time.Since(start)
			}

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				// Alternate order so either implementation sees each short-lived
				// machine-load and filesystem-cache advantage equally.
				if i%2 == 0 {
					run(false)
					run(true)
				} else {
					run(true)
					run(false)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(hashThenScan.Nanoseconds())/float64(b.N), "hash-scan-ns/op")
			b.ReportMetric(float64(singleRead.Nanoseconds())/float64(b.N), "single-read-ns/op")
			b.ReportMetric(float64(corpusBytes*2), "hash-scan-transcript-B/op")
			b.ReportMetric(float64(corpusBytes), "single-read-transcript-B/op")
		})
	}
}

func historyRefreshBenchmarkFixture(tb testing.TB, fileCount int) (string, int64) {
	tb.Helper()
	brainDir := tb.TempDir()
	sessionsDir := filepath.Join(brainDir, exportSessionsDirectory, "main")
	if err := os.MkdirAll(sessionsDir, 0o700); err != nil {
		tb.Fatal(err)
	}
	// Each transcript is about 64 KiB. Most lines are intentionally irrelevant
	// so the fixture exercises transcript I/O without turning this into a JSON
	// classification benchmark.
	padding := `{"type":"event_msg","payload":{"type":"usage","detail":"` + strings.Repeat("x", 4000) + `"}}` + "\n"
	var corpusBytes int64
	sessions := make([]exportSession, 0, fileCount)
	for i := 0; i < fileCount; i++ {
		content := strings.Repeat(padding, 16) + fmt.Sprintf(
			`{"type":"agent_message","message":"Decision: retain synthetic history fixture %04d."}`+"\n", i,
		)
		name := fmt.Sprintf("20260717T%06dZ-session-%04d.jsonl", i, i)
		path := filepath.Join(sessionsDir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			tb.Fatal(err)
		}
		corpusBytes += int64(len(content))
		sessions = append(sessions, exportSession{
			SessionID:      fmt.Sprintf("session-%04d", i),
			Branch:         "main",
			TranscriptPath: filepath.ToSlash(filepath.Join(exportSessionsDirectory, "main", name)),
		})
	}
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		GeneratedAt:   now,
		DefaultBranch: "main",
		Sources: &brainSources{Sessions: &sessionSourceManifest{
			GeneratedAt:   now,
			DefaultBranch: "main",
			Sessions:      sessions,
		}},
	}
	if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
		tb.Fatal(err)
	}
	return brainDir, corpusBytes
}
