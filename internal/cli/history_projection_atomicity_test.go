package cli

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
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
		SchemaVersion: brainManifestSchemaVersion,
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
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Sources == nil || manifest.Sources.History == nil {
		t.Fatal("history source missing")
	}
	index, err := loadBrainHistoryIndex(brainDir, manifest.Sources.History)
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
		if rel == exportManifestFileName {
			return errors.New("injected manifest replacement failure")
		}
		return originalWrite(brainDir, rel, data, perm)
	}
	t.Cleanup(func() { historyProjectionWriteFile = originalWrite })
	err = withBrainWriteLock(brainDir, func() error {
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
	err = withBrainWriteLock(brainDir, func() error {
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
	oldInfo, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	oldBody, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	newBody := bytes.Replace(oldBody, []byte("commit the manifest last"), []byte("commit the content first"), 1)
	if bytes.Equal(newBody, oldBody) || len(newBody) != len(oldBody) {
		t.Fatalf("replacement fixture must change content without changing size: old=%d new=%d", len(oldBody), len(newBody))
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, newBody, oldInfo.Mode().Perm()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(target, oldInfo.ModTime(), oldInfo.ModTime()); err != nil {
		t.Fatal(err)
	}
	newInfo, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if newInfo.Size() != oldInfo.Size() || !newInfo.ModTime().Equal(oldInfo.ModTime()) || os.SameFile(oldInfo, newInfo) {
		t.Fatalf("replacement did not preserve only size+mtime while changing inode: old=%+v new=%+v", oldInfo, newInfo)
	}

	err = withBrainWriteLock(brainDir, func() error {
		_, publishErr := publishBrainHistoryProjectionLocked(brainDir, prepared, nil)
		return publishErr
	})
	if err == nil || !strings.Contains(err.Error(), memoryErrSourceStale) {
		t.Fatalf("publish error = %v, want %s", err, memoryErrSourceStale)
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
	err = withBrainWriteLock(brainDir, func() error {
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

func TestHistoryProjectionPrivacyOrOverlayChangeBeforeCommitPublishesNothing(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(t *testing.T, brainDir string, now time.Time)
	}{
		{name: "tombstone", mutate: func(t *testing.T, brainDir string, now time.Time) {
			stones := loadSessionTombstones(brainDir)
			stones.Excluded["session-1"] = sessionTombstone{At: now, Reason: "test"}
			if err := saveSessionTombstones(brainDir, stones); err != nil {
				t.Fatal(err)
			}
		}},
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
			err = withBrainWriteLock(brainDir, func() error {
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

func TestPrivacyInventoryIncludesEveryHistoryGenerationArtifact(t *testing.T) {
	brainDir, _, now := historyProjectionFixture(t)
	if _, err := writeBrainHistoryIndexAndSource(brainDir, now.Add(time.Minute), nil); err != nil {
		t.Fatal(err)
	}
	second, _ := activeHistoryProjection(t, brainDir)
	orphanGeneration := strings.Repeat("a", 40)
	orphanIndex := historyGenerationArtifactPath(orphanGeneration, historyIndexFileName)
	orphanReceipt := historyGenerationArtifactPath(orphanGeneration, projectionStateFileName)
	for source, target := range map[string]string{second.IndexPath: orphanIndex, second.ProjectionStatePath: orphanReceipt} {
		data, err := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(source)))
		if err != nil {
			t.Fatal(err)
		}
		if err := writeBrainRelativeFileAtomic(brainDir, target, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string]bool{
		orphanIndex:   true,
		orphanReceipt: true,
	}
	artifacts, err := privacyDerivedStoreArtifacts(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range artifacts {
		delete(want, rel)
	}
	if len(want) != 0 {
		t.Fatalf("privacy inventory omitted history generation artifacts: %v", want)
	}
	for _, rel := range artifacts {
		if rel == second.IndexPath || rel == second.ProjectionStatePath {
			t.Fatalf("privacy inventory must not delete the manifest-active generation: %s", rel)
		}
	}
}

func TestHistoryProjectionPrepareDoesNotAcquireBrainWriteLock(t *testing.T) {
	brainDir, _, now := historyProjectionFixture(t)
	unlock, err := acquireBrainWriteLock(brainDir)
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
	err = withBrainWriteLock(brainDir, func() error {
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

func TestPrivacyGenerationInventoryFailsClosedOnWalkError(t *testing.T) {
	brainDir, _, _ := historyProjectionFixture(t)
	originalWalk := privacyWalkDir
	privacyWalkDir = func(root string, fn fs.WalkDirFunc) error {
		if strings.HasSuffix(filepath.ToSlash(root), historyGenerationsDir) {
			return fs.ErrPermission
		}
		return originalWalk(root, fn)
	}
	t.Cleanup(func() { privacyWalkDir = originalWalk })
	if _, err := privacyDerivedStoreArtifacts(brainDir); err == nil || !strings.Contains(err.Error(), "memory_state_corrupt") {
		t.Fatalf("inventory error = %v, want fail-closed memory_state_corrupt", err)
	}
	if _, err := buildSessionPurgePlan(brainDir, "session-1"); err == nil {
		t.Fatal("privacy plan must fail when generation inventory is unreadable")
	}
}

func TestPrivacyCleanupRemovesInactiveLegacyProjectionAndEgressReceipt(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	now := time.Date(2026, 8, 9, 18, 0, 0, 0, time.UTC)
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate migration debris from the fixed-path projection layout. The
	// active manifest already references a generation, so these copies are
	// inactive and must be inventoried rather than silently retained.
	for source, target := range map[string]string{
		manifest.Sources.History.IndexPath:           historyIndexPath,
		manifest.Sources.History.ProjectionStatePath: projectionStateRel,
	} {
		data, err := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(source)))
		if err != nil {
			t.Fatal(err)
		}
		if err := writeBrainRelativeFileAtomic(brainDir, target, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	refs := sessionRefsForSessionID(brainDir, manifest, "secret-sess")
	var sessionRef string
	for ref := range refs {
		sessionRef = ref
		break
	}
	receipt := abstractEgressReceipt{
		SchemaVersion: 1, OperationID: strings.Repeat("b", 20), SessionRef: sessionRef,
		SessionDigest: "sha256:" + strings.Repeat("c", 64), Provider: "codex", Model: "test",
		StartedAt: now, FinishedAt: now, Status: "completed",
	}
	if err := writeAbstractEgressReceipt(brainDir, receipt); err != nil {
		t.Fatal(err)
	}
	plan, err := buildSessionPurgePlan(brainDir, "secret-sess")
	if err != nil {
		t.Fatal(err)
	}
	wantDerived := map[string]bool{historyIndexPath: true, projectionStateRel: true}
	for _, artifact := range plan.DerivedStores {
		delete(wantDerived, artifact.Path)
	}
	if len(wantDerived) != 0 {
		t.Fatalf("inactive legacy projection missing from privacy plan: %v", wantDerived)
	}
	egressRel := abstractEgressReceiptRel(receipt.SessionRef)
	foundEgress := false
	for _, artifact := range plan.WorkMetadata {
		foundEgress = foundEgress || artifact.Path == egressRel
	}
	if !foundEgress {
		t.Fatalf("egress receipt missing from privacy work inventory: %+v", plan.WorkMetadata)
	}
	if err := executeSessionCleanup(brainDir, "secret-sess", plan, now, "test", true); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{historyIndexPath, projectionStateRel, egressRel} {
		if _, err := os.Stat(filepath.Join(brainDir, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Fatalf("privacy cleanup retained %s: %v", rel, err)
		}
	}
}

func TestHistoryProjectionMigrationRemovesLegacyFilesOnlyAfterSwitch(t *testing.T) {
	brainDir, _, now := historyProjectionFixture(t)
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	old := manifest.Sources.History
	for source, target := range map[string]string{old.IndexPath: historyIndexPath, old.ProjectionStatePath: projectionStateRel} {
		data, err := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(source)))
		if err != nil {
			t.Fatal(err)
		}
		if err := writeBrainRelativeFileAtomic(brainDir, target, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	legacy := *old
	legacy.IndexPath = historyIndexPath
	legacy.ProjectionStatePath = projectionStateRel
	manifest.Sources.History = &legacy
	if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := writeBrainHistoryIndexAndSource(brainDir, now.Add(time.Minute), nil); err != nil {
		t.Fatal(err)
	}
	current, _ := activeHistoryProjection(t, brainDir)
	if current.IndexPath == historyIndexPath || current.ProjectionStatePath == projectionStateRel {
		t.Fatalf("migration did not switch to generation paths: %+v", current)
	}
	for _, rel := range []string{historyIndexPath, projectionStateRel} {
		if _, err := os.Stat(filepath.Join(brainDir, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Fatalf("inactive legacy artifact survived successful switch: %s err=%v", rel, err)
		}
	}
}

func TestPrivacyVerifyFailsClosedOnCorruptEgressReceipt(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	stones := loadSessionTombstones(brainDir)
	stones.Excluded["secret-sess"] = sessionTombstone{At: time.Now().UTC()}
	if err := saveSessionTombstones(brainDir, stones); err != nil {
		t.Fatal(err)
	}
	rel := filepath.ToSlash(filepath.Join(abstractEgressDirRel, "corrupt.json"))
	if err := writeBrainRelativeFileAtomic(brainDir, rel, []byte("{not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := verifySessionPrivacy(brainDir); err == nil || !strings.Contains(err.Error(), memoryErrStateCorrupt) {
		t.Fatalf("privacy verify error = %v, want %s", err, memoryErrStateCorrupt)
	}
}
