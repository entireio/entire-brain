package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
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

func TestHistoryIndexRejectsMutationBetweenFingerprintAndScan(t *testing.T) {
	brainDir := t.TempDir()
	sessionDir := filepath.Join(brainDir, exportSessionsDirectory, "main")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	stablePath := filepath.Join(sessionDir, "20260717T010000Z-stable.jsonl")
	targetPath := filepath.Join(sessionDir, "20260717T020000Z-target.jsonl")
	stable := `{"type":"agent_message","message":"Decision: retain stable record."}` + "\n"
	initial := `{"type":"agent_message","message":"Decision: retain prior cache."}` + "\n"
	before := `{"type":"agent_message","message":"Decision: retain alpha cache."}` + "\n"
	after := `{"type":"agent_message","message":"Decision: retain omega cache."}` + "\n"
	if len(before) != len(after) {
		t.Fatalf("race fixture sizes differ: %d != %d", len(before), len(after))
	}
	for path, content := range map[string]string{stablePath: stable, targetPath: initial} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	if _, _, err := buildBrainHistoryIndex(brainDir, now, nil); err != nil {
		t.Fatalf("prime v6 cache: %v", err)
	}
	if err := os.WriteFile(targetPath, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	var expectedWarnings []string
	expectedFiles, err := collectHistorySessionFiles(sessionDir, &expectedWarnings)
	if err != nil || len(expectedWarnings) != 0 {
		t.Fatalf("expected fingerprint: warnings=%v err=%v", expectedWarnings, err)
	}
	expectedFingerprint := historyTranscriptFilesFingerprint(brainDir, expectedFiles)

	var progress [][2]int
	mutated := false
	index, source, err := buildBrainHistoryIndex(brainDir, now.Add(time.Second), func(done, total int) {
		progress = append(progress, [2]int{done, total})
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
	if err != nil {
		t.Fatal(err)
	}
	if !mutated {
		t.Fatal("progress hook did not mutate target after fingerprint collection")
	}
	if want := [][2]int{{0, 2}, {1, 2}, {2, 2}}; !reflect.DeepEqual(progress, want) {
		t.Fatalf("progress = %v, want %v", progress, want)
	}
	wantWarning := "history transcript changed between fingerprint and scan: " + targetPath
	if !reflect.DeepEqual(source.Warnings, []string{wantWarning}) {
		t.Fatalf("warnings = %v, want [%q]", source.Warnings, wantWarning)
	}
	if source.TranscriptsFingerprint != expectedFingerprint {
		t.Fatalf("fingerprint = %q, want pre-mutation %q", source.TranscriptsFingerprint, expectedFingerprint)
	}
	assertHistorySummaries(t, index, []string{"Decision: retain stable record."})
	cache := loadHistoryScanCache(brainDir)
	stableRel, _ := filepath.Rel(brainDir, stablePath)
	targetRel, _ := filepath.Rel(brainDir, targetPath)
	if len(cache.Files) != 1 {
		t.Fatalf("cache files = %+v, want only stable transcript", cache.Files)
	}
	if _, ok := cache.Files[filepath.ToSlash(stableRel)]; !ok {
		t.Fatalf("stable transcript missing from cache: %+v", cache.Files)
	}
	if _, ok := cache.Files[filepath.ToSlash(targetRel)]; ok {
		t.Fatalf("raced transcript entered cache: %+v", cache.Files)
	}
	var currentWarnings []string
	currentFiles, err := collectHistorySessionFiles(sessionDir, &currentWarnings)
	if err != nil || len(currentWarnings) != 0 {
		t.Fatalf("current fingerprint: warnings=%v err=%v", currentWarnings, err)
	}
	if current := historyTranscriptFilesFingerprint(brainDir, currentFiles); current == source.TranscriptsFingerprint {
		t.Fatal("raced source fingerprint incorrectly described post-mutation bytes")
	}

	// The skipped file is retried on the next build and becomes cacheable once
	// its hash and parsed bytes are the same stable version.
	recovered, recoveredSource, err := buildBrainHistoryIndex(brainDir, now.Add(2*time.Second), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(recoveredSource.Warnings) != 0 {
		t.Fatalf("recovery warnings = %v", recoveredSource.Warnings)
	}
	assertHistorySummaries(t, recovered, []string{"Decision: retain stable record.", "Decision: retain omega cache."})
	if got := loadHistoryScanCache(brainDir); len(got.Files) != 2 {
		t.Fatalf("recovery cache = %+v", got.Files)
	}
}

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

			var progress [][2]int
			mutated := false
			index, source, err := buildBrainHistoryIndex(brainDir, time.Date(2026, 7, 18, 11, 0, 0, 0, time.UTC), func(done, total int) {
				progress = append(progress, [2]int{done, total})
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
			if err != nil {
				t.Fatal(err)
			}
			if want := [][2]int{{0, 1}, {1, 1}}; !reflect.DeepEqual(progress, want) {
				t.Fatalf("progress = %v, want %v", progress, want)
			}
			var wantWarning string
			if mode == "deleted" {
				wantWarning = (&os.PathError{Op: "open", Path: path, Err: syscall.ENOENT}).Error()
			} else {
				wantWarning = "history transcript changed while opening: " + path
			}
			if !reflect.DeepEqual(source.Warnings, []string{wantWarning}) {
				t.Fatalf("warnings = %v, want [%q]", source.Warnings, wantWarning)
			}
			if strings.Contains(strings.Join(source.Warnings, "\n"), externalPath) || strings.Contains(strings.Join(source.Warnings, "\n"), secret) {
				t.Fatalf("warning exposed external source: %v", source.Warnings)
			}
			if len(index.Records) != 0 || source.Records != 0 {
				t.Fatalf("external or unstable bytes entered index: %+v source=%+v", index.Records, source)
			}
			if source.TranscriptsFingerprint != emptyHistoryTranscriptsFingerprint {
				t.Fatalf("fingerprint = %q, want empty %q", source.TranscriptsFingerprint, emptyHistoryTranscriptsFingerprint)
			}
			if cache := loadHistoryScanCache(brainDir); len(cache.Files) != 0 {
				t.Fatalf("unstable file entered cache: %+v", cache.Files)
			}
		})
	}
}

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
	var progress [][2]int
	_, source, err := buildBrainHistoryIndex(brainDir, time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC), func(done, total int) {
		progress = append(progress, [2]int{done, total})
		if done == 0 {
			for _, path := range []string{oldPath, newPath} {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	wantWarnings := []string{
		(&os.PathError{Op: "open", Path: newPath, Err: syscall.ENOENT}).Error(),
		(&os.PathError{Op: "open", Path: oldPath, Err: syscall.ENOENT}).Error(),
	}
	if !reflect.DeepEqual(source.Warnings, wantWarnings) {
		t.Fatalf("warnings = %v, want sorted scan order %v", source.Warnings, wantWarnings)
	}
	if want := [][2]int{{0, 2}, {1, 2}, {2, 2}}; !reflect.DeepEqual(progress, want) {
		t.Fatalf("progress = %v, want %v", progress, want)
	}
	if source.TranscriptsFingerprint != emptyHistoryTranscriptsFingerprint {
		t.Fatalf("fingerprint = %q, want empty", source.TranscriptsFingerprint)
	}
	if cache := loadHistoryScanCache(brainDir); len(cache.Files) != 0 {
		t.Fatalf("failed files entered cache: %+v", cache.Files)
	}
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
	if want := []string{"bufio.Scanner: token too long"}; !reflect.DeepEqual(source.Warnings, want) {
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
