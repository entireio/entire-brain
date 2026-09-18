package cli

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func prepareDistinctPatternCorpusPublication(t *testing.T, brainDir string) (string, func()) {
	t.Helper()
	path, cleanup, err := preparePatternCorpusStaging(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(sqliteDriverName, path)
	if err != nil {
		cleanup()
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT OR REPLACE INTO meta(key,value) VALUES('publication_test_generation','new')`); err != nil {
		_ = db.Close()
		cleanup()
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA journal_mode=DELETE`); err != nil {
		_ = db.Close()
		cleanup()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		cleanup()
		t.Fatal(err)
	}
	return path, cleanup
}

func patternCorpusPublicationTestGeneration(t *testing.T, brainDir string) string {
	t.Helper()
	db, err := openPatternCorpusReadDB(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var generation string
	err = db.QueryRow(`SELECT value FROM meta WHERE key='publication_test_generation'`).Scan(&generation)
	if errors.Is(err, sql.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return generation
}

func TestPrivacyLinearizedPatternMutationHoldsPolicyThroughProviderAndPersistence(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	policy, err := captureRetrievalPrivacyPolicy(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	providerStarted := make(chan struct{})
	releaseProvider := make(chan struct{})
	operationDone := make(chan error, 1)
	var stdout, stderr bytes.Buffer
	cmd := &cobra.Command{Use: "test"}
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	persisted := filepath.Join(brainDir, "patterns", "provider-result")
	if err := os.MkdirAll(filepath.Dir(persisted), 0o700); err != nil {
		t.Fatal(err)
	}
	go func() {
		operationDone <- runPrivacyLinearizedPatternMutation(cmd, []retrievalPrivacyPolicy{policy}, func() error {
			close(providerStarted)
			<-releaseProvider
			if err := os.WriteFile(persisted, []byte("derived"), 0o600); err != nil {
				return err
			}
			_, _ = cmd.OutOrStdout().Write([]byte("complete\n"))
			_, _ = cmd.ErrOrStderr().Write([]byte("provider complete\n"))
			return nil
		})
	}()
	<-providerStarted

	tombstoneAttempted := make(chan struct{})
	tombstoneDone := make(chan error, 1)
	go func() {
		close(tombstoneAttempted)
		tombstoneDone <- withBrainWriteLock(brainDir, func() error {
			stones, _, err := loadSessionTombstonesChecked(brainDir)
			if err != nil {
				return err
			}
			stones.Excluded["secret-sess"] = sessionTombstone{At: time.Now().UTC()}
			return saveSessionTombstones(brainDir, stones)
		})
	}()
	<-tombstoneAttempted
	select {
	case err := <-tombstoneDone:
		t.Fatalf("tombstone linearized during provider invocation: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("private output escaped before provider/persistence commit: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}

	close(releaseProvider)
	if err := <-operationDone; err != nil {
		t.Fatal(err)
	}
	if err := <-tombstoneDone; err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "complete\n" || stderr.String() != "provider complete\n" {
		t.Fatalf("buffered output mismatch: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if _, err := os.Stat(persisted); err != nil {
		t.Fatalf("provider result was not persisted before the tombstone linearized: %v", err)
	}
}

func TestPrivacyLinearizedPatternMutationRejectsChangedPolicyWithoutOutput(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	policy, err := captureRetrievalPrivacyPolicy(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := withBrainWriteLock(brainDir, func() error {
		stones, _, err := loadSessionTombstonesChecked(brainDir)
		if err != nil {
			return err
		}
		stones.Excluded["secret-sess"] = sessionTombstone{At: time.Now().UTC()}
		return saveSessionTombstones(brainDir, stones)
	}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	cmd := &cobra.Command{Use: "test"}
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	called := false
	err = runPrivacyLinearizedPatternMutation(cmd, []retrievalPrivacyPolicy{policy}, func() error {
		called = true
		_, _ = cmd.OutOrStdout().Write([]byte("private"))
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), memoryErrPrivacyDirty) {
		t.Fatalf("changed policy error = %v, want %s", err, memoryErrPrivacyDirty)
	}
	if called || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("rejected mutation leaked work/output: called=%v stdout=%q stderr=%q", called, stdout.String(), stderr.String())
	}
}

func TestPatternCorpusFailedStagingPublishPreservesLiveCorpus(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	brainDir := writeEpisodeFixture(t, now, "gh/acme/privacy", []sessionFixture{{
		id: "s1", branch: "main", agent: "Codex", checkpoint: "c1", relPath: "sessions/main/s1.jsonl", author: "Ada", transcript: claudeReviewTranscript,
	}})
	if err := buildPatternCorpus(brainDir, now); err != nil {
		t.Fatal(err)
	}
	beforeDB, err := openPatternCorpusMutableDB(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	var beforeSHA string
	if err := beforeDB.QueryRow(`SELECT content_sha FROM indexed_sessions WHERE session_id='s1'`).Scan(&beforeSHA); err != nil {
		t.Fatal(err)
	}
	_ = beforeDB.Close()
	beforeRuns := len(loadPatternRuns(brainDir))

	transcript := filepath.Join(brainDir, "sessions", "main", "s1.jsonl")
	f, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\n" + themeProposalTranscript); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("injected publication failure")
	original := beforePatternCorpusPublish
	beforePatternCorpusPublish = func() error { return wantErr }
	t.Cleanup(func() { beforePatternCorpusPublish = original })
	if err := buildPatternCorpus(brainDir, now.Add(time.Minute)); !errors.Is(err, wantErr) {
		t.Fatalf("build error = %v, want injected failure", err)
	}

	afterDB, err := openPatternCorpusMutableDB(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	defer afterDB.Close()
	var afterSHA string
	if err := afterDB.QueryRow(`SELECT content_sha FROM indexed_sessions WHERE session_id='s1'`).Scan(&afterSHA); err != nil {
		t.Fatal(err)
	}
	if afterSHA != beforeSHA {
		t.Fatalf("failed staged publication changed live corpus: before=%s after=%s", beforeSHA, afterSHA)
	}
	if got := len(loadPatternRuns(brainDir)); got != beforeRuns {
		t.Fatalf("failed publication appended a run record: before=%d after=%d", beforeRuns, got)
	}
	entries, err := os.ReadDir(filepath.Join(brainDir, "patterns"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), patternCorpusStagingPrefix) {
			t.Fatalf("failed build left staging artifact %s", entry.Name())
		}
	}
}

func TestPatternCorpusPublicationCrashBoundariesAreOldNewOrFailClosed(t *testing.T) {
	boundaries := []struct {
		step             string
		wantState        string
		wantSidecarsLeft int
	}{
		{step: "before_marker_create", wantState: "old", wantSidecarsLeft: 3},
		{step: "after_marker_create", wantState: "marked", wantSidecarsLeft: 3},
		{step: "before_remove_corpus.sqlite-wal", wantState: "marked", wantSidecarsLeft: 3},
		{step: "after_remove_corpus.sqlite-wal", wantState: "marked", wantSidecarsLeft: 2},
		{step: "before_remove_corpus.sqlite-shm", wantState: "marked", wantSidecarsLeft: 2},
		{step: "after_remove_corpus.sqlite-shm", wantState: "marked", wantSidecarsLeft: 1},
		{step: "before_remove_corpus.sqlite-journal", wantState: "marked", wantSidecarsLeft: 1},
		{step: "after_remove_corpus.sqlite-journal", wantState: "marked", wantSidecarsLeft: 0},
		{step: "before_main_rename", wantState: "marked", wantSidecarsLeft: 0},
		{step: "after_main_rename", wantState: "marked", wantSidecarsLeft: 0},
		{step: "after_main_sync", wantState: "marked", wantSidecarsLeft: 0},
		{step: "before_marker_remove", wantState: "marked", wantSidecarsLeft: 0},
		{step: "after_marker_remove", wantState: "new", wantSidecarsLeft: 0},
		{step: "after_marker_remove_sync", wantState: "new", wantSidecarsLeft: 0},
	}
	for _, tc := range boundaries {
		t.Run(tc.step, func(t *testing.T) {
			now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
			brainDir := writeEpisodeFixture(t, now, "gh/acme/privacy", []sessionFixture{{
				id: "s1", branch: "main", agent: "Codex", checkpoint: "c1", relPath: "sessions/main/s1.jsonl", author: "Ada", transcript: claudeReviewTranscript,
			}})
			if err := buildPatternCorpus(brainDir, now); err != nil {
				t.Fatal(err)
			}
			if got := patternCorpusPublicationTestGeneration(t, brainDir); got != "" {
				t.Fatalf("initial corpus generation = %q", got)
			}
			stage, cleanup := prepareDistinctPatternCorpusPublication(t, brainDir)
			defer cleanup()
			// Regular placeholder sidecars let the test prove each unlink
			// boundary. They are never opened by SQLite: the durable marker is
			// already present before the first one can be removed.
			patternsDir := filepath.Join(brainDir, "patterns")
			for _, sidecar := range patternCorpusSidecarNames {
				if err := os.WriteFile(filepath.Join(patternsDir, sidecar), []byte("old sidecar\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			injected := errors.New("injected crash boundary")
			original := patternCorpusPublishBoundary
			patternCorpusPublishBoundary = func(step string) error {
				if step == tc.step {
					return injected
				}
				return nil
			}
			err := publishPatternCorpusLocked(brainDir, stage)
			patternCorpusPublishBoundary = original
			if !errors.Is(err, injected) {
				t.Fatalf("publish error = %v, want injected boundary %s", err, tc.step)
			}
			left := 0
			for _, sidecar := range patternCorpusSidecarNames {
				if _, err := os.Lstat(filepath.Join(patternsDir, sidecar)); err == nil {
					left++
				} else if !os.IsNotExist(err) {
					t.Fatal(err)
				}
			}
			if left != tc.wantSidecarsLeft {
				t.Fatalf("sidecars remaining at %s = %d, want %d", tc.step, left, tc.wantSidecarsLeft)
			}

			marker := filepath.Join(brainDir, filepath.FromSlash(patternCorpusPublicationMarkerPath))
			switch tc.wantState {
			case "old":
				if _, err := os.Lstat(marker); !os.IsNotExist(err) {
					t.Fatalf("pre-transaction failure left marker: %v", err)
				}
				// Remove test-only invalid sidecars before asking SQLite to read
				// the prior, otherwise-complete logical database.
				for _, sidecar := range patternCorpusSidecarNames {
					if err := os.Remove(filepath.Join(patternsDir, sidecar)); err != nil {
						t.Fatal(err)
					}
				}
				if got := patternCorpusPublicationTestGeneration(t, brainDir); got != "" {
					t.Fatalf("pre-marker failure selected generation %q, want old", got)
				}
			case "marked":
				if _, err := os.Lstat(marker); err != nil {
					t.Fatalf("transactional failure did not retain marker: %v", err)
				}
				if _, err := openPatternCorpusReadDB(brainDir); err == nil || !strings.Contains(err.Error(), memoryErrStateUnsafe) {
					t.Fatalf("marked corpus read error = %v, want %s", err, memoryErrStateUnsafe)
				}
				if err := recoverPatternCorpusPublicationLocked(brainDir); err != nil {
					t.Fatalf("recover %s: %v", tc.step, err)
				}
				if got := patternCorpusPublicationTestGeneration(t, brainDir); got != "new" {
					t.Fatalf("recovered generation = %q, want new", got)
				}
			case "new":
				if _, err := os.Lstat(marker); !os.IsNotExist(err) {
					t.Fatalf("committed publication retained marker: %v", err)
				}
				if got := patternCorpusPublicationTestGeneration(t, brainDir); got != "new" {
					t.Fatalf("committed generation = %q, want new", got)
				}
			}
		})
	}
}

func TestPatternCorpusForwardPublicationMarkerPreservesRecoveryAndBuildState(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	for _, action := range []struct {
		name string
		run  func(string) error
	}{
		{name: "recover", run: recoverPatternCorpusPublicationLocked},
		{name: "build", run: func(brainDir string) error {
			return buildPatternCorpus(brainDir, now.Add(time.Minute))
		}},
	} {
		t.Run(action.name, func(t *testing.T) {
			brainDir := writeEpisodeFixture(t, now, "gh/acme/privacy", []sessionFixture{{
				id: "s1", branch: "main", agent: "Codex", checkpoint: "c1", relPath: "sessions/main/s1.jsonl", author: "Ada", transcript: claudeReviewTranscript,
			}})
			if err := buildPatternCorpus(brainDir, now); err != nil {
				t.Fatal(err)
			}
			patternsDir := filepath.Join(brainDir, "patterns")
			corpusPath := filepath.Join(patternsDir, "corpus.sqlite")
			runsPath := filepath.Join(brainDir, filepath.FromSlash(patternRunsRelPath))
			corpusBefore, err := os.ReadFile(corpusPath)
			if err != nil {
				t.Fatal(err)
			}
			runsBefore, err := os.ReadFile(runsPath)
			if err != nil {
				t.Fatal(err)
			}

			candidate := patternCorpusStagingPrefix + "future"
			candidatePath := filepath.Join(patternsDir, candidate)
			candidateData := []byte("opaque future pattern corpus candidate\n")
			if err := os.WriteFile(candidatePath, candidateData, 0o600); err != nil {
				t.Fatal(err)
			}
			markerData := []byte(fmt.Sprintf(
				`{"schema_version":%d,"candidate":%q,"future_layout":{"digest":"opaque"}}`+"\n",
				patternCorpusPublicationVersion+1, candidate,
			))
			markerPath := filepath.Join(patternsDir, patternCorpusPublicationMarkerName)
			if err := os.WriteFile(markerPath, markerData, 0o600); err != nil {
				t.Fatal(err)
			}

			err = action.run(brainDir)
			var typed *patternCorpusPublicationLoadError
			if !errors.As(err, &typed) || typed.Code != memoryErrUnsupportedVersion ||
				typed.State != patternCorpusPublicationUnsupported || typed.Version != patternCorpusPublicationVersion+1 {
				t.Fatalf("error = %v typed=%+v, want typed unsupported vNext", err, typed)
			}
			for path, want := range map[string][]byte{
				markerPath:    markerData,
				candidatePath: candidateData,
				corpusPath:    corpusBefore,
				runsPath:      runsBefore,
			} {
				got, readErr := os.ReadFile(path)
				if readErr != nil || !bytes.Equal(got, want) {
					t.Fatalf("%s changed: err=%v got=%q want=%q", path, readErr, got, want)
				}
			}
		})
	}
}

func TestPatternCorpusPublishDirectorySwapCannotTouchExternalTarget(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	brainDir := writeEpisodeFixture(t, now, "gh/acme/privacy", []sessionFixture{{
		id: "s1", branch: "main", agent: "Codex", checkpoint: "c1", relPath: "sessions/main/s1.jsonl", author: "Ada", transcript: claudeReviewTranscript,
	}})
	if err := buildPatternCorpus(brainDir, now); err != nil {
		t.Fatal(err)
	}
	external := t.TempDir()
	externalFiles := map[string][]byte{
		"corpus.sqlite":         []byte("external corpus sentinel\n"),
		"corpus.sqlite-wal":     []byte("external wal sentinel\n"),
		"corpus.sqlite-shm":     []byte("external shm sentinel\n"),
		"corpus.sqlite-journal": []byte("external journal sentinel\n"),
	}
	for name, data := range externalFiles {
		if err := os.WriteFile(filepath.Join(external, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	patternsDir := filepath.Join(brainDir, "patterns")
	detached := filepath.Join(brainDir, "patterns-detached")
	original := patternCorpusPublishBoundary
	patternCorpusPublishBoundary = func(step string) error {
		if step != "after_marker_create" {
			return nil
		}
		if err := os.Rename(patternsDir, detached); err != nil {
			return err
		}
		return os.Symlink(external, patternsDir)
	}
	t.Cleanup(func() { patternCorpusPublishBoundary = original })
	err := buildPatternCorpus(brainDir, now.Add(time.Minute))
	if err == nil || !strings.Contains(err.Error(), memoryErrStateUnsafe) {
		t.Fatalf("directory swap publish error = %v, want %s", err, memoryErrStateUnsafe)
	}
	for name, want := range externalFiles {
		got, readErr := os.ReadFile(filepath.Join(external, name))
		if readErr != nil || !bytes.Equal(got, want) {
			t.Fatalf("external target %s changed: err=%v got=%q want=%q", name, readErr, got, want)
		}
	}
	entries, err := os.ReadDir(external)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(externalFiles) {
		t.Fatalf("publication created files in external target: %v", entries)
	}
}

func TestPatternCorpusTranscriptReadIsBoundedAndFailClosed(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	brainDir := writeEpisodeFixture(t, now, "gh/acme/privacy", []sessionFixture{{
		id: "s1", branch: "main", agent: "Codex", checkpoint: "c1", relPath: "sessions/main/s1.jsonl", author: "Ada", transcript: claudeReviewTranscript,
	}})
	originalMax := maxDocumentTranscriptBytes
	maxDocumentTranscriptBytes = 8
	t.Cleanup(func() { maxDocumentTranscriptBytes = originalMax })
	err := buildPatternCorpus(brainDir, now)
	if err == nil || !strings.Contains(err.Error(), "exceeds maximum size") {
		t.Fatalf("oversized pattern transcript error = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(brainDir, filepath.FromSlash(patternCorpusPath))); !os.IsNotExist(statErr) {
		t.Fatalf("failed bounded read published a corpus: %v", statErr)
	}
	entries, readErr := os.ReadDir(filepath.Join(brainDir, "patterns"))
	if os.IsNotExist(readErr) {
		// Private staging intentionally leaves no live directory behind when a
		// first build fails before publication begins.
		return
	}
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), patternCorpusStagingPrefix) {
			t.Fatalf("failed bounded read left staging artifact %s", entry.Name())
		}
	}
}

func TestPatternCorpusTranscriptReadRejectsSymlink(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	brainDir := writeEpisodeFixture(t, now, "gh/acme/privacy", []sessionFixture{{
		id: "s1", branch: "main", agent: "Codex", checkpoint: "c1", relPath: "sessions/main/s1.jsonl", author: "Ada", transcript: claudeReviewTranscript,
	}})
	transcript := filepath.Join(brainDir, "sessions", "main", "s1.jsonl")
	realTranscript := filepath.Join(brainDir, "sessions", "main", "real.jsonl")
	if err := os.Rename(transcript, realTranscript); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realTranscript, transcript); err != nil {
		t.Skipf("symlink fixture unavailable: %v", err)
	}
	err := buildPatternCorpus(brainDir, now)
	if err == nil || !strings.Contains(err.Error(), memoryErrSessionInventory) {
		t.Fatalf("symlinked pattern transcript error = %v, want %s", err, memoryErrSessionInventory)
	}
	if _, statErr := os.Stat(filepath.Join(brainDir, filepath.FromSlash(patternCorpusPath))); !os.IsNotExist(statErr) {
		t.Fatalf("unsafe transcript published a corpus: %v", statErr)
	}
}

func TestPatternCorpusMissingManifestTranscriptRefusesPartialPublication(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	const rel = "sessions/main/s1.jsonl"
	brainDir := writeEpisodeFixture(t, now, "gh/acme/privacy", []sessionFixture{{
		id: "s1", branch: "main", agent: "Codex", checkpoint: "c1", relPath: rel, author: "Ada", transcript: claudeReviewTranscript,
	}})
	if err := buildPatternCorpus(brainDir, now); err != nil {
		t.Fatal(err)
	}
	corpusPath := filepath.Join(brainDir, filepath.FromSlash(patternCorpusPath))
	beforeCorpus, err := os.ReadFile(corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeRuns := len(loadPatternRuns(brainDir))
	if err := os.Remove(filepath.Join(brainDir, filepath.FromSlash(rel))); err != nil {
		t.Fatal(err)
	}
	err = buildPatternCorpus(brainDir, now.Add(time.Minute))
	if err == nil || !strings.Contains(err.Error(), memoryErrSessionInventory) {
		t.Fatalf("missing manifest transcript error = %v, want %s", err, memoryErrSessionInventory)
	}
	afterCorpus, err := os.ReadFile(corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterCorpus, beforeCorpus) {
		t.Fatal("missing source published a partial replacement corpus")
	}
	if got := len(loadPatternRuns(brainDir)); got != beforeRuns {
		t.Fatalf("refused partial build appended run history: before=%d after=%d", beforeRuns, got)
	}
}

func TestPatternCorpusBuildRejectsNewerManifestWithoutMutation(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	brainDir := writeEpisodeFixture(t, now, "gh/acme/privacy", []sessionFixture{{
		id: "s1", branch: "main", agent: "Codex", checkpoint: "c1", relPath: "sessions/main/s1.jsonl", author: "Ada", transcript: claudeReviewTranscript,
	}})
	if err := buildPatternCorpus(brainDir, now); err != nil {
		t.Fatal(err)
	}
	corpusPath := filepath.Join(brainDir, filepath.FromSlash(patternCorpusPath))
	beforeCorpus, err := os.ReadFile(corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(brainDir, exportManifestFileName)
	vNext := []byte(fmt.Sprintf("{\"schema_version\":%d,\"future\":{\"must_survive\":true}}\n", brainManifestSchemaVersion+1))
	if err := os.WriteFile(manifestPath, vNext, 0o600); err != nil {
		t.Fatal(err)
	}
	err = buildPatternCorpus(brainDir, now.Add(time.Minute))
	if err == nil || !strings.Contains(err.Error(), memoryErrUnsupportedVersion) {
		t.Fatalf("vNext manifest build error = %v, want %s", err, memoryErrUnsupportedVersion)
	}
	gotManifest, err := os.ReadFile(manifestPath)
	if err != nil || !bytes.Equal(gotManifest, vNext) {
		t.Fatalf("vNext manifest was changed: err=%v got=%q", err, gotManifest)
	}
	afterCorpus, err := os.ReadFile(corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterCorpus, beforeCorpus) {
		t.Fatal("unsupported manifest changed the published corpus")
	}
}

func TestPatternCorpusStagingArtifactsAreInPrivacyInventory(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	if err := os.MkdirAll(filepath.Join(brainDir, "patterns"), 0o700); err != nil {
		t.Fatal(err)
	}
	base := patternCorpusStagingPrefix + "orphan"
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.WriteFile(filepath.Join(brainDir, "patterns", base+suffix), []byte("staged private data"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	artifacts, err := privacyDerivedStoreArtifacts(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		filepath.ToSlash(filepath.Join("patterns", base)):          false,
		filepath.ToSlash(filepath.Join("patterns", base)) + "-wal": false,
		filepath.ToSlash(filepath.Join("patterns", base)) + "-shm": false,
	}
	for _, artifact := range artifacts {
		if _, ok := want[artifact]; ok {
			want[artifact] = true
		}
	}
	for artifact, found := range want {
		if !found {
			t.Errorf("privacy inventory missed staging artifact %s", artifact)
		}
	}
}

func TestPatternCorpusBuildCannotPublishAfterTombstoneLinearizes(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	brainDir := writeEpisodeFixture(t, now, "gh/acme/privacy", []sessionFixture{{
		id: "s1", branch: "main", agent: "Codex", checkpoint: "c1", relPath: "sessions/main/s1.jsonl", author: "Ada", transcript: claudeReviewTranscript,
	}})
	if err := buildPatternCorpus(brainDir, now); err != nil {
		t.Fatal(err)
	}
	reachedPublish := make(chan struct{})
	releasePublish := make(chan struct{})
	original := beforePatternCorpusPublish
	beforePatternCorpusPublish = func() error {
		close(reachedPublish)
		<-releasePublish
		return nil
	}
	t.Cleanup(func() { beforePatternCorpusPublish = original })
	buildDone := make(chan error, 1)
	go func() { buildDone <- buildPatternCorpus(brainDir, now.Add(time.Minute)) }()
	<-reachedPublish

	tombstoneDone := make(chan error, 1)
	go func() {
		tombstoneDone <- withBrainWriteLock(brainDir, func() error {
			stones, _, err := loadSessionTombstonesChecked(brainDir)
			if err != nil {
				return err
			}
			stones.Excluded["s1"] = sessionTombstone{At: now.Add(2 * time.Minute)}
			if err := saveSessionTombstones(brainDir, stones); err != nil {
				return err
			}
			return removeBrainRelativeFile(brainDir, patternCorpusPath)
		})
	}()
	select {
	case err := <-tombstoneDone:
		t.Fatalf("tombstone linearized before staged publication completed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(releasePublish)
	if err := <-buildDone; err != nil {
		t.Fatal(err)
	}
	if err := <-tombstoneDone; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(brainDir, filepath.FromSlash(patternCorpusPath))); !os.IsNotExist(err) {
		t.Fatalf("stale corpus was published after tombstone commit: %v", err)
	}
}
