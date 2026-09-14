package cli

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const emptyHistoryTranscriptsFingerprint = "sha256:eadd0dee7e9012b93e3fae44cd9c3e48e03e3c35f121ccacc069a3c837ffacb1"

func TestHistoryScanCacheReappliesManifestBranch(t *testing.T) {
	brainDir := t.TempDir()
	now := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
	rel := "sessions/main/session.jsonl"
	path := filepath.Join(brainDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	content := `{"type":"agent_message","message":"Decision: keep branch inference fresh."}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	writeManifest := func(branch string) {
		t.Helper()
		manifest := exportManifest{
			SchemaVersion: brainManifestSchemaVersion,
			GeneratedAt:   now,
			DefaultBranch: "main",
			Sources: &brainSources{Sessions: &sessionSourceManifest{
				GeneratedAt:   now,
				DefaultBranch: "main",
				Sessions: []exportSession{{
					SessionID: "session", Branch: branch, TranscriptPath: rel,
				}},
			}},
		}
		if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
			t.Fatal(err)
		}
	}

	writeManifest("main")
	index, _, err := buildBrainHistoryIndex(brainDir, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertOnlyHistoryBranch(t, index, "main")
	cache := loadHistoryScanCache(brainDir)
	assertCachedRawBranch(t, cache, rel, "")

	writeManifest("feature")
	index, _, err = buildBrainHistoryIndex(brainDir, now.Add(time.Second), nil)
	if err != nil {
		t.Fatal(err)
	}
	assertOnlyHistoryBranch(t, index, "feature")
	cache = loadHistoryScanCache(brainDir)
	assertCachedRawBranch(t, cache, rel, "")

	// A scanner-provided branch is raw record data, not manifest inference, and
	// must survive annotation. Seed that future-compatible v6 shape directly.
	entry := cache.Files[rel]
	entry.Records[0].Branch = "transcript-explicit"
	cache.Files[rel] = entry
	saveHistoryScanCache(brainDir, cache)
	writeManifest("hotfix")
	index, _, err = buildBrainHistoryIndex(brainDir, now.Add(2*time.Second), nil)
	if err != nil {
		t.Fatal(err)
	}
	assertOnlyHistoryBranch(t, index, "transcript-explicit")
	assertCachedRawBranch(t, loadHistoryScanCache(brainDir), rel, "transcript-explicit")
}

func TestAnnotateHistoryRecordBranchesCopiesRawRecords(t *testing.T) {
	raw := []historyRecord{{ID: "inferred"}, {ID: "explicit", Branch: "transcript-explicit"}}
	annotated := annotateHistoryRecordBranches(raw, "sessions/main/session.jsonl", map[string]string{
		"sessions/main/session.jsonl": "feature",
	})
	if raw[0].Branch != "" || raw[1].Branch != "transcript-explicit" {
		t.Fatalf("raw cache records mutated in place: %+v", raw)
	}
	if annotated[0].Branch != "feature" || annotated[1].Branch != "transcript-explicit" {
		t.Fatalf("annotated branches = %+v", annotated)
	}
	if &raw[0] == &annotated[0] {
		t.Fatal("branch annotation reused the raw cache backing slice")
	}
}

// TestHistoryIndexRejectsMutationBetweenFingerprintAndScan locks the
// fail-closed ingestion contract: a transcript rewritten between identity
// capture and the scan aborts the build. It used to be recorded as a warning
// and the index published anyway, but a partial or mixed-generation inventory
// must never be published as a complete projection.
func TestHistoryIndexRejectsMutationBetweenFingerprintAndScan(t *testing.T) {
	brainDir, targetPath, info, now := historyMutationRaceFixture(t)
	after := "{\"type\":\"agent_message\",\"message\":\"Decision: mutated after fingerprint.\"}\n"
	mutated := false
	_, _, err := buildBrainHistoryIndex(brainDir, now.Add(time.Second), func(done, total int) {
		if done != 0 || mutated {
			return
		}
		mutated = true
		if err := os.WriteFile(targetPath, []byte(after), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(targetPath, info.ModTime(), info.ModTime()); err != nil {
			t.Fatal(err)
		}
	})
	if !mutated {
		t.Fatal("progress hook did not mutate target after identity capture")
	}
	if err == nil {
		t.Fatal("a transcript mutated mid-build must abort the index, not warn")
	}
	if !errors.Is(err, errHistorySessionInventoryDegraded) && !strings.Contains(err.Error(), memoryErrSourceStale) {
		t.Fatalf("mutation error = %v, want a typed degraded/stale refusal", err)
	}
}

// TestHistoryCacheEmptyRejectsOpenAndIdentityRaces locks that a transcript
// deleted, inode-replaced, or symlink-swapped mid-build aborts the index and
// never lets external bytes in. #83 made these typed refusals rather than
// per-file warnings.
func TestHistoryCacheEmptyRejectsOpenAndIdentityRaces(t *testing.T) {
	for _, mode := range []string{"deleted", "inode replacement", "symlink swap"} {
		t.Run(mode, func(t *testing.T) {
			brainDir := t.TempDir()
			sessionDir := filepath.Join(brainDir, exportSessionsDirectory, "main")
			if err := os.MkdirAll(sessionDir, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(sessionDir, "20260717T010000Z-session.jsonl")
			if err := os.WriteFile(path, []byte(`{"type":"agent_message","message":"Decision: safe in-repo record."}`+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			externalPath := filepath.Join(t.TempDir(), "external.jsonl")
			secret := "Decision: external secret must never be indexed."
			if err := os.WriteFile(externalPath, []byte(`{"type":"agent_message","message":"`+secret+`"}`+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if mode == "symlink swap" {
				probe := filepath.Join(t.TempDir(), "probe")
				if err := os.Symlink(externalPath, probe); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			}
			mutated := false
			index, _, err := buildBrainHistoryIndex(brainDir, time.Date(2026, 7, 18, 11, 0, 0, 0, time.UTC), func(done, total int) {
				if done != 0 || mutated {
					return
				}
				mutated = true
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				switch mode {
				case "inode replacement":
					if err := os.Rename(externalPath, path); err != nil {
						t.Fatal(err)
					}
				case "symlink swap":
					if err := os.Symlink(externalPath, path); err != nil {
						t.Fatal(err)
					}
				}
			})
			if err == nil {
				t.Fatal("an open/identity race must abort the index, not warn")
			}
			if !errors.Is(err, errHistorySessionInventoryDegraded) {
				t.Fatalf("race error = %v, want the typed degraded refusal", err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("refusal exposed external content: %v", err)
			}
			if len(index.Records) != 0 {
				t.Fatalf("external or unstable bytes entered the index: %+v", index.Records)
			}
			if cache := loadHistoryScanCache(brainDir); len(cache.Files) != 0 {
				t.Fatalf("unstable file entered cache: %+v", cache.Files)
			}
		})
	}
}

// TestHistoryCacheEmptyFailuresUseSortedScanOrder locks that the build refuses
// on the FIRST failure in recency-sorted scan order, so the refusal is
// deterministic regardless of directory enumeration order.
func TestHistoryCacheEmptyFailuresUseSortedScanOrder(t *testing.T) {
	brainDir := t.TempDir()
	sessionDir := filepath.Join(brainDir, exportSessionsDirectory, "main")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(sessionDir, "20260717T010000Z-old.jsonl")
	newPath := filepath.Join(sessionDir, "20260717T020000Z-new.jsonl")
	for _, path := range []string{oldPath, newPath} {
		if err := os.WriteFile(path, []byte("Decision: candidate.\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_, _, err := buildBrainHistoryIndex(brainDir, time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC), func(done, total int) {
		if done != 0 {
			return
		}
		for _, path := range []string{oldPath, newPath} {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}
	})
	if err == nil {
		t.Fatal("removed transcripts must abort the index")
	}
	// Newest-first scan order: the newer transcript is the one reported.
	if !strings.Contains(err.Error(), filepath.Base(newPath)) {
		t.Fatalf("refusal = %v, want the newest-first candidate %q", err, filepath.Base(newPath))
	}
	if cache := loadHistoryScanCache(brainDir); len(cache.Files) != 0 {
		t.Fatalf("failed files entered cache: %+v", cache.Files)
	}
}

// historyMutationRaceFixture writes a two-transcript brain and returns the
// target transcript plus its pre-mutation metadata.
func historyMutationRaceFixture(t *testing.T) (string, string, os.FileInfo, time.Time) {
	t.Helper()
	brainDir := t.TempDir()
	sessionDir := filepath.Join(brainDir, exportSessionsDirectory, "main")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	stablePath := filepath.Join(sessionDir, "20260717T010000Z-stable.jsonl")
	targetPath := filepath.Join(sessionDir, "20260717T020000Z-target.jsonl")
	if err := os.WriteFile(stablePath, []byte(`{"type":"agent_message","message":"Decision: retain stable record."}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targetPath, []byte(`{"type":"agent_message","message":"Decision: retain target record."}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	if _, _, err := buildBrainHistoryIndex(brainDir, now, nil); err != nil {
		t.Fatalf("prime cache: %v", err)
	}
	info, err := os.Stat(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	return brainDir, targetPath, info, now
}

func TestHistoryCacheEmptyFingerprintsButDoesNotCacheParseLimitFailure(t *testing.T) {
	brainDir := t.TempDir()
	path := filepath.Join(brainDir, exportSessionsDirectory, "main", "20260717T010000Z-oversized.txt")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	content := "Decision: " + strings.Repeat("x", historyMaxLineBytes+1) + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	var progress [][2]int
	index, source, err := buildBrainHistoryIndex(brainDir, time.Date(2026, 7, 18, 13, 0, 0, 0, time.UTC), func(done, total int) {
		progress = append(progress, [2]int{done, total})
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := [][2]int{{0, 1}, {1, 1}}; !reflect.DeepEqual(progress, want) {
		t.Fatalf("progress = %v, want %v", progress, want)
	}
	// The warning names the transcript. A whole session drops out of the
	// projection here, and an unattributed scanner error left no way to tell
	// which one had gone missing.
	if want := []string{"history records skipped for sessions/main/20260717T010000Z-oversized.txt: bufio.Scanner: token too long"}; !reflect.DeepEqual(source.Warnings, want) {
		t.Fatalf("warnings = %v, want %v", source.Warnings, want)
	}
	const wantFingerprint = "sha256:999bbc4efb10f491343b79dee94e03b966545c254838a9f00d681e3f3bc04aea"
	if source.TranscriptsFingerprint != wantFingerprint {
		t.Fatalf("fingerprint = %q, want %q", source.TranscriptsFingerprint, wantFingerprint)
	}
	if len(index.Records) != 0 || source.Records != 0 {
		t.Fatalf("parse-failed records entered index: %+v source=%+v", index.Records, source)
	}
	if cache := loadHistoryScanCache(brainDir); len(cache.Files) != 0 {
		t.Fatalf("parse-failed transcript entered cache: %+v", cache.Files)
	}
}

func TestHistoryScanFingerprintErrorDefendsNilError(t *testing.T) {
	path := "sessions/main/missing.jsonl"
	want := "history transcript scan returned no content fingerprint: " + path
	if got := historyScanFingerprintError(path, nil).Error(); got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

func assertOnlyHistoryBranch(t *testing.T, index historyIndex, want string) {
	t.Helper()
	if len(index.Records) != 1 || index.Records[0].Branch != want {
		t.Fatalf("history branches = %+v, want one %q record", index.Records, want)
	}
}

func assertCachedRawBranch(t *testing.T, cache historyScanCache, rel, want string) {
	t.Helper()
	entry, ok := cache.Files[rel]
	if !ok || len(entry.Records) != 1 || entry.Records[0].Branch != want {
		t.Fatalf("cached raw branch for %s = %+v, want %q", rel, entry, want)
	}
}

func assertHistorySummaries(t *testing.T, index historyIndex, want []string) {
	t.Helper()
	got := make([]string, 0, len(index.Records))
	for _, record := range index.Records {
		got = append(got, record.Summary)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("history summaries = %v, want %v", got, want)
	}
}
