package cli

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/historystore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

func historyProjectionFixture(t *testing.T) (string, string, time.Time) {
	t.Helper()
	brainDir := t.TempDir()
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	rel := "sessions/main/session.jsonl"
	full := filepath.Join(brainDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"Fix projection publication"}]}}` + "\n" +
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Decision: commit the manifest last."}]}}` + "\n"
	if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := exportManifest{
		SchemaVersion: bmanifest.SchemaVersion,
		GeneratedAt:   now,
		RepoKey:       "test/projection-atomicity",
		Sources: &brainSources{Sessions: &sessionSourceManifest{
			GeneratedAt: now, DefaultBranch: "main",
			Sessions: []exportSession{{SessionID: "session-1", Branch: "main", LatestCheckpoint: "cp1", TranscriptPath: rel}},
		}},
	}
	if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := writeBrainHistoryIndexAndSource(brainDir, now, nil); err != nil {
		t.Fatal(err)
	}
	return brainDir, rel, now
}

func activeHistoryProjection(t *testing.T, brainDir string) (*historySourceManifest, historyIndex) {
	t.Helper()
	manifest, err := bmanifest.Load(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Sources == nil || manifest.Sources.History == nil {
		t.Fatal("history source missing")
	}
	index, err := historystore.LoadIndex(brainDir, manifest.Sources.History)
	if err != nil {
		t.Fatal(err)
	}
	return manifest.Sources.History, index
}

func TestHistoryProjectionPublishFailurePreservesPreviousGeneration(t *testing.T) {
	brainDir, _, now := historyProjectionFixture(t)
	previous, previousIndex := activeHistoryProjection(t, brainDir)
	prepared, err := prepareBrainHistoryProjection(brainDir, now.Add(time.Minute), nil)
	if err != nil {
		t.Fatal(err)
	}

	originalWrite := historyProjectionWriteFile
	historyProjectionWriteFile = func(brainDir, rel string, data []byte, perm os.FileMode) error {
		if rel == bmanifest.FileName {
			return errors.New("injected manifest replacement failure")
		}
		return originalWrite(brainDir, rel, data, perm)
	}
	t.Cleanup(func() { historyProjectionWriteFile = originalWrite })
	err = brainstore.WithBrainWriteLock(brainDir, func() error {
		_, publishErr := publishBrainHistoryProjectionLocked(brainDir, prepared, nil)
		return publishErr
	})
	if err == nil || !strings.Contains(err.Error(), "injected manifest") {
		t.Fatalf("publish error = %v", err)
	}
	current, currentIndex := activeHistoryProjection(t, brainDir)
	if current.IndexPath != previous.IndexPath || current.ProjectionStatePath != previous.ProjectionStatePath {
		t.Fatalf("failed publish switched generation: before=%+v after=%+v", previous, current)
	}
	if len(currentIndex.Records) != len(previousIndex.Records) {
		t.Fatalf("previous projection changed: before=%d after=%d", len(previousIndex.Records), len(currentIndex.Records))
	}
	if _, ok := loadProjectionState(brainDir, current); !ok {
		t.Fatal("previous receipt generation became unreadable")
	}
}

func TestHistoryProjectionSourceChangeBeforeCommitPublishesNothing(t *testing.T) {
	brainDir, rel, now := historyProjectionFixture(t)
	previous, _ := activeHistoryProjection(t, brainDir)
	prepared, err := prepareBrainHistoryProjection(brainDir, now.Add(time.Minute), nil)
	if err != nil {
		t.Fatal(err)
	}
	full := filepath.Join(brainDir, filepath.FromSlash(rel))
	f, err := os.OpenFile(full, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := f.WriteString(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Learning: source changed."}]}}` + "\n")
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatalf("append=%v close=%v", writeErr, closeErr)
	}
	changed := now.Add(2 * time.Minute)
	if err := os.Chtimes(full, changed, changed); err != nil {
		t.Fatal(err)
	}
	err = brainstore.WithBrainWriteLock(brainDir, func() error {
		_, publishErr := publishBrainHistoryProjectionLocked(brainDir, prepared, nil)
		return publishErr
	})
	if err == nil || !strings.Contains(err.Error(), "memory_source_stale") {
		t.Fatalf("publish error = %v, want memory_source_stale", err)
	}
	current, _ := activeHistoryProjection(t, brainDir)
	if current.IndexPath != previous.IndexPath || current.ProjectionStatePath != previous.ProjectionStatePath {
		t.Fatalf("stale publish switched generation: before=%+v after=%+v", previous, current)
	}
}

func TestHistoryProjectionSameMetadataReplacementBeforeCommitPublishesNothing(t *testing.T) {
	brainDir, rel, now := historyProjectionFixture(t)
	previous, previousIndex := activeHistoryProjection(t, brainDir)
	prepared, err := prepareBrainHistoryProjection(brainDir, now.Add(time.Minute), nil)
	if err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(brainDir, filepath.FromSlash(rel))
	replaceDeltaTranscriptSameMetadata(t, target, "commit the manifest last", "commit the content first")

	err = brainstore.WithBrainWriteLock(brainDir, func() error {
		_, publishErr := publishBrainHistoryProjectionLocked(brainDir, prepared, nil)
		return publishErr
	})
	if err == nil || !strings.Contains(err.Error(), brainstore.SourceStaleCode) {
		t.Fatalf("publish error = %v, want %s", err, brainstore.SourceStaleCode)
	}
	current, currentIndex := activeHistoryProjection(t, brainDir)
	if current.IndexPath != previous.IndexPath || current.ProjectionStatePath != previous.ProjectionStatePath {
		t.Fatalf("same-metadata replacement switched generation: before=%+v after=%+v", previous, current)
	}
	if !reflect.DeepEqual(currentIndex.Records, previousIndex.Records) {
		t.Fatal("same-metadata replacement changed the active searchable records")
	}
	for _, record := range currentIndex.Records {
		if strings.Contains(record.Summary, "content first") {
			t.Fatalf("replacement summary was published: %+v", record)
		}
	}
}

func TestHistoryProjectionCancellationAtCommitPublishesNothing(t *testing.T) {
	brainDir, _, now := historyProjectionFixture(t)
	previous, _ := activeHistoryProjection(t, brainDir)
	prepared, err := prepareBrainHistoryProjection(brainDir, now.Add(time.Minute), nil)
	if err != nil {
		t.Fatal(err)
	}
	checks := 0
	guard := func() error {
		checks++
		if checks == 2 {
			return errMemoryCancellationRequested
		}
		return nil
	}
	err = brainstore.WithBrainWriteLock(brainDir, func() error {
		_, publishErr := publishBrainHistoryProjectionLocked(brainDir, prepared, guard)
		return publishErr
	})
	if !errors.Is(err, errMemoryCancellationRequested) {
		t.Fatalf("publish error = %v, want cancellation", err)
	}
	current, _ := activeHistoryProjection(t, brainDir)
	if current.IndexPath != previous.IndexPath || current.ProjectionStatePath != previous.ProjectionStatePath {
		t.Fatalf("cancelled publish switched generation: before=%+v after=%+v", previous, current)
	}
}

func TestHistoryProjectionOverlayChangeBeforeCommitPublishesNothing(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(t *testing.T, brainDir string, now time.Time)
	}{
		{name: "overlay", mutate: func(t *testing.T, brainDir string, now time.Time) {
			source, _ := activeHistoryProjection(t, brainDir)
			overlay := shortTermIndex{
				Version: historyShortTermVersion, BaseGeneratedAt: source.GeneratedAt,
				SessionsFingerprint: source.SessionsFingerprint, GeneratedAt: now,
				ReconcilerVersion: historyShortTermReconcilerVersion, Files: map[string]shortTermFile{},
			}
			if err := saveHistoryShortTerm(brainDir, overlay); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			brainDir, _, now := historyProjectionFixture(t)
			previous, _ := activeHistoryProjection(t, brainDir)
			prepared, err := prepareBrainHistoryProjection(brainDir, now.Add(time.Minute), nil)
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(t, brainDir, now.Add(2*time.Minute))
			err = brainstore.WithBrainWriteLock(brainDir, func() error {
				_, publishErr := publishBrainHistoryProjectionLocked(brainDir, prepared, nil)
				return publishErr
			})
			if err == nil || !strings.Contains(err.Error(), "memory_source_stale") {
				t.Fatalf("publish error = %v, want memory_source_stale", err)
			}
			current, _ := activeHistoryProjection(t, brainDir)
			if current.IndexPath != previous.IndexPath {
				t.Fatalf("identity-changing publish switched generation: before=%s after=%s", previous.IndexPath, current.IndexPath)
			}
		})
	}
}

func TestHistoryProjectionChangeDuringPrepareStagesNothing(t *testing.T) {
	brainDir, rel, now := historyProjectionFixture(t)
	previous, _ := activeHistoryProjection(t, brainDir)
	changed := false
	_, err := prepareBrainHistoryProjection(brainDir, now.Add(time.Minute), func(done, total int) {
		if changed || done != 0 || total == 0 {
			return
		}
		changed = true
		full := filepath.Join(brainDir, filepath.FromSlash(rel))
		f, openErr := os.OpenFile(full, os.O_APPEND|os.O_WRONLY, 0)
		if openErr != nil {
			t.Fatal(openErr)
		}
		if _, writeErr := f.WriteString("\n"); writeErr != nil {
			t.Fatal(writeErr)
		}
		if closeErr := f.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	})
	if err == nil || !strings.Contains(err.Error(), "memory_source_stale") {
		t.Fatalf("prepare error = %v, want memory_source_stale", err)
	}
	current, _ := activeHistoryProjection(t, brainDir)
	if current.IndexPath != previous.IndexPath {
		t.Fatalf("failed preparation switched generation: before=%s after=%s", previous.IndexPath, current.IndexPath)
	}
}

func TestHistoryProjectionPrepareDoesNotAcquireBrainWriteLock(t *testing.T) {
	brainDir, _, now := historyProjectionFixture(t)
	unlock, err := brainstore.AcquireBrainWriteLock(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	prepared, err := prepareBrainHistoryProjection(brainDir, now.Add(time.Minute), nil)
	if err != nil {
		t.Fatalf("prepare while another owner holds the commit lock: %v", err)
	}
	discardPreparedHistoryProjection(brainDir, prepared)
}

func TestHistoryProjectionCommitRevalidationIsMetadataOnly(t *testing.T) {
	brainDir, _, now := historyProjectionFixture(t)
	prepared, err := prepareBrainHistoryProjection(brainDir, now.Add(time.Minute), nil)
	if err != nil {
		t.Fatal(err)
	}
	digestCalls := 0
	originalHook := beforeHistoryProjectionContentDigest
	beforeHistoryProjectionContentDigest = func(string) { digestCalls++ }
	t.Cleanup(func() { beforeHistoryProjectionContentDigest = originalHook })
	err = brainstore.WithBrainWriteLock(brainDir, func() error {
		_, publishErr := publishBrainHistoryProjectionLocked(brainDir, prepared, nil)
		return publishErr
	})
	if err != nil {
		t.Fatal(err)
	}
	if digestCalls != 0 {
		t.Fatalf("commit revalidation hashed %d transcript(s) while holding the Brain lock", digestCalls)
	}
}

func TestHistoryProjectionMigrationRemovesLegacyFilesOnlyAfterSwitch(t *testing.T) {
	brainDir, _, now := historyProjectionFixture(t)
	manifest, err := bmanifest.Load(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	old := manifest.Sources.History
	for source, target := range map[string]string{old.IndexPath: historystore.IndexPath, old.ProjectionStatePath: projectionStateRel} {
		data, err := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(source)))
		if err != nil {
			t.Fatal(err)
		}
		if err := brainstore.WriteBrainRelativeFileAtomic(brainDir, target, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	legacy := *old
	legacy.IndexPath = historystore.IndexPath
	legacy.ProjectionStatePath = projectionStateRel
	manifest.Sources.History = &legacy
	if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := writeBrainHistoryIndexAndSource(brainDir, now.Add(time.Minute), nil); err != nil {
		t.Fatal(err)
	}
	current, _ := activeHistoryProjection(t, brainDir)
	if current.IndexPath == historystore.IndexPath || current.ProjectionStatePath == projectionStateRel {
		t.Fatalf("migration did not switch to generation paths: %+v", current)
	}
	for _, rel := range []string{historystore.IndexPath, projectionStateRel} {
		if _, err := os.Stat(filepath.Join(brainDir, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Fatalf("inactive legacy artifact survived successful switch: %s err=%v", rel, err)
		}
	}
}
