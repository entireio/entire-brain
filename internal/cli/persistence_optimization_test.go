package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func persistenceOptimizationCorpusSessionIDs(t *testing.T, brainDir string) []string {
	t.Helper()
	db, err := openPatternCorpusMutableDB(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT session_id FROM indexed_sessions ORDER BY session_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

func persistenceOptimizationTombstoneBytes(t *testing.T, brainDir string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(sessionTombstonesPath)))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func persistenceOptimizationCorpusSidecars(t *testing.T, brainDir string) map[string][sha256.Size]byte {
	t.Helper()
	state := make(map[string][sha256.Size]byte)
	for _, name := range patternCorpusSidecarNames {
		path := filepath.Join(brainDir, "patterns", name)
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatalf("read pattern corpus sidecar %s: %v", name, err)
		}
		state[name] = sha256.Sum256(data)
	}
	return state
}

func persistenceOptimizationPrivateStages(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(os.TempDir(), "entire-brain-pattern-corpus-*"))
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

// This test mutates the process-global beforePatternCorpusPublish seam and
// therefore deliberately does not call t.Parallel.
func TestExcludedSessionSurvivesCancelledRefreshReopenAndRetry(t *testing.T) {
	isolatedTemp := t.TempDir()
	t.Setenv("TMPDIR", isolatedTemp)
	t.Setenv("TMP", isolatedTemp)
	t.Setenv("TEMP", isolatedTemp)
	now := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	brainDir := writePrivacyFixture(t)
	if err := buildPatternCorpus(brainDir, now); err != nil {
		t.Fatal(err)
	}
	if got, want := persistenceOptimizationCorpusSessionIDs(t, brainDir), []string{"clean-sess", "secret-sess"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("initial corpus sessions = %v, want %v", got, want)
	}

	stones, _, err := loadSessionTombstonesChecked(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	stones.Excluded["secret-sess"] = sessionTombstone{At: now.Add(time.Minute), Reason: "optimization regression"}
	if err := saveSessionTombstones(brainDir, stones); err != nil {
		t.Fatal(err)
	}
	tombstoneBytes := persistenceOptimizationTombstoneBytes(t, brainDir)
	if _, err := writeBrainHistoryIndexAndSource(brainDir, now.Add(2*time.Minute), nil); err != nil {
		t.Fatal(err)
	}
	if historyIndexContainsForIncludeTest(t, brainDir, privacyCanary) {
		t.Fatal("history refresh resurrected the excluded session")
	}

	runsBefore := len(loadPatternRuns(brainDir))
	sidecarsBefore := persistenceOptimizationCorpusSidecars(t, brainDir)
	privateStagesBefore := make(map[string]bool)
	for _, path := range persistenceOptimizationPrivateStages(t) {
		privateStagesBefore[path] = true
	}
	var cancelledPrivateStages []string
	originalPublishHook := beforePatternCorpusPublish
	beforePatternCorpusPublish = func() error {
		for _, path := range persistenceOptimizationPrivateStages(t) {
			if !privateStagesBefore[path] {
				cancelledPrivateStages = append(cancelledPrivateStages, path)
			}
		}
		return context.Canceled
	}
	t.Cleanup(func() { beforePatternCorpusPublish = originalPublishHook })
	if err := buildPatternCorpus(brainDir, now.Add(3*time.Minute)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled refresh error = %v, want context cancellation", err)
	}
	if len(cancelledPrivateStages) != 1 {
		t.Fatalf("private staging directories observed at cancellation = %v, want one", cancelledPrivateStages)
	}
	if _, err := os.Lstat(cancelledPrivateStages[0]); !os.IsNotExist(err) {
		t.Fatalf("cancelled refresh retained private staging directory %s: %v", cancelledPrivateStages[0], err)
	}
	marker := filepath.Join(brainDir, filepath.FromSlash(patternCorpusPublicationMarkerPath))
	if _, err := os.Lstat(marker); !os.IsNotExist(err) {
		t.Fatalf("cancelled pre-publication refresh retained publication marker: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(brainDir, "patterns"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		rel := filepath.ToSlash(filepath.Join("patterns", entry.Name()))
		if isPatternCorpusStagingArtifactRel(rel) {
			t.Fatalf("cancelled pre-publication refresh retained Brain staging artifact %s", rel)
		}
	}
	// Existing SQLite sidecars can be legitimate when another handle owns
	// them. Cancellation before publication must preserve their prior state;
	// the contract does not require an arbitrary sidecar set to be absent.
	if got := persistenceOptimizationCorpusSidecars(t, brainDir); !reflect.DeepEqual(got, sidecarsBefore) {
		t.Fatalf("cancelled pre-publication refresh changed live sidecars: before=%v after=%v", sidecarsBefore, got)
	}
	if got := persistenceOptimizationTombstoneBytes(t, brainDir); !bytes.Equal(got, tombstoneBytes) {
		t.Fatalf("cancelled refresh changed retained exclusion\n before: %q\n  after: %q", tombstoneBytes, got)
	}
	if got, want := persistenceOptimizationCorpusSessionIDs(t, brainDir), []string{"clean-sess", "secret-sess"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("cancelled refresh changed committed corpus: got %v, want %v", got, want)
	}
	if got := len(loadPatternRuns(brainDir)); got != runsBefore {
		t.Fatalf("cancelled refresh appended a run: before=%d after=%d", runsBefore, got)
	}
	if allowed, err := privacyDerivedReadGate(brainDir); err != nil || allowed {
		t.Fatalf("stale committed corpus must remain fail-closed after cancellation: allowed=%t err=%v", allowed, err)
	}

	beforePatternCorpusPublish = originalPublishHook
	for i, at := range []time.Time{now.Add(4 * time.Minute), now.Add(5 * time.Minute)} {
		if _, err := writeBrainHistoryIndexAndSource(brainDir, at, nil); err != nil {
			t.Fatalf("refresh %d history: %v", i+1, err)
		}
		if err := buildPatternCorpus(brainDir, at); err != nil {
			t.Fatalf("refresh %d corpus: %v", i+1, err)
		}
		// The helper opens and closes a fresh SQLite handle on every iteration,
		// exercising the persisted view rather than an in-memory connection.
		if got, want := persistenceOptimizationCorpusSessionIDs(t, brainDir), []string{"clean-sess"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("refresh %d corpus sessions = %v, want %v", i+1, got, want)
		}
		if got := persistenceOptimizationTombstoneBytes(t, brainDir); !bytes.Equal(got, tombstoneBytes) {
			t.Fatalf("refresh %d changed retained exclusion\n before: %q\n  after: %q", i+1, tombstoneBytes, got)
		}
		if historyIndexContainsForIncludeTest(t, brainDir, privacyCanary) {
			t.Fatalf("refresh %d resurrected excluded history", i+1)
		}
	}
	if allowed, err := privacyDerivedReadGate(brainDir); err != nil || !allowed {
		t.Fatalf("clean retried corpus gate: allowed=%t err=%v", allowed, err)
	}
	report, err := verifySessionPrivacy(brainDir)
	if err != nil || !report.Clean {
		t.Fatalf("post-retry privacy verification: clean=%t findings=%+v err=%v", report.Clean, report.Findings, err)
	}
}

// This test also mutates beforePatternCorpusPublish and must remain
// nonparallel with other tests that use that process-global seam.
func TestPatternPublicationLinearizesBeforeConcurrentExclusion(t *testing.T) {
	now := time.Date(2026, 8, 10, 10, 0, 0, 0, time.UTC)
	brainDir := writePrivacyFixture(t)
	if err := buildPatternCorpus(brainDir, now); err != nil {
		t.Fatal(err)
	}

	reachedPublish := make(chan struct{})
	releasePublish := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releasePublish) }) }
	originalPublishHook := beforePatternCorpusPublish
	beforePatternCorpusPublish = func() error {
		close(reachedPublish)
		<-releasePublish
		return nil
	}

	events := make(chan string, 2)
	buildDone := make(chan error, 1)
	buildExited := make(chan struct{})
	go func() {
		defer close(buildExited)
		buildDone <- withBrainWriteLock(brainDir, func() error {
			if err := buildPatternCorpusLocked(brainDir, now.Add(time.Minute)); err != nil {
				return err
			}
			events <- "publication_complete"
			return nil
		})
	}()
	exclusionExited := make(chan struct{})
	exclusionStarted := false
	t.Cleanup(func() {
		release()
		select {
		case <-buildExited:
		case <-time.After(5 * time.Second):
			t.Error("build goroutine did not exit during cleanup")
		}
		if exclusionStarted {
			select {
			case <-exclusionExited:
			case <-time.After(5 * time.Second):
				t.Error("exclusion goroutine did not exit during cleanup")
			}
		}
		beforePatternCorpusPublish = originalPublishHook
	})
	select {
	case <-reachedPublish:
	case <-time.After(5 * time.Second):
		t.Fatal("build did not reach the publication barrier")
	}

	exclusionAttempted := make(chan struct{})
	exclusionDone := make(chan error, 1)
	exclusionStarted = true
	go func() {
		defer close(exclusionExited)
		close(exclusionAttempted)
		exclusionDone <- withBrainWriteLock(brainDir, func() error {
			events <- "exclusion_acquired"
			stones, _, err := loadSessionTombstonesChecked(brainDir)
			if err != nil {
				return err
			}
			stones.Excluded["secret-sess"] = sessionTombstone{At: now.Add(2 * time.Minute), Reason: "ordered exclusion"}
			return saveSessionTombstones(brainDir, stones)
		})
	}()
	<-exclusionAttempted
	release()

	nextEvent := func() string {
		t.Helper()
		select {
		case event := <-events:
			return event
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for a linearization event")
			return ""
		}
	}
	if got := nextEvent(); got != "publication_complete" {
		t.Fatalf("first linearized event = %q, want publication_complete", got)
	}
	if got := nextEvent(); got != "exclusion_acquired" {
		t.Fatalf("second linearized event = %q, want exclusion_acquired", got)
	}
	if err := <-buildDone; err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := <-exclusionDone; err != nil {
		t.Fatalf("exclusion: %v", err)
	}

	beforePatternCorpusPublish = originalPublishHook
	if err := buildPatternCorpus(brainDir, now.Add(3*time.Minute)); err != nil {
		t.Fatalf("post-exclusion refresh: %v", err)
	}
	if got, want := persistenceOptimizationCorpusSessionIDs(t, brainDir), []string{"clean-sess"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("post-exclusion corpus sessions = %v, want %v", got, want)
	}
}
