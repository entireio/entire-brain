package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writePatternPublicationPrivacyFixture(t *testing.T, brainDir, candidate string, candidateData []byte) ([]byte, string) {
	t.Helper()
	patternsDir := filepath.Join(brainDir, "patterns")
	if err := os.MkdirAll(patternsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	candidatePath := filepath.Join(patternsDir, candidate)
	if err := os.WriteFile(candidatePath, candidateData, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(candidateData)
	publication := patternCorpusPublication{
		SchemaVersion: patternCorpusPublicationVersion,
		Candidate:     candidate,
		SHA256:        hex.EncodeToString(sum[:]),
		Size:          int64(len(candidateData)),
	}
	marker, err := json.Marshal(publication)
	if err != nil {
		t.Fatal(err)
	}
	marker = append(marker, '\n')
	markerPath := filepath.Join(brainDir, filepath.FromSlash(patternCorpusPublicationMarkerPath))
	if err := os.WriteFile(markerPath, marker, 0o600); err != nil {
		t.Fatal(err)
	}
	return marker, candidatePath
}

func privacyPlanContainsDerived(plan sessionPurgePlan, rel string) bool {
	for _, artifact := range plan.DerivedStores {
		if artifact.Path == rel {
			return true
		}
	}
	return false
}

func TestPatternCorpusPublicationPrivacyInventoryAndCleanup(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	candidate := patternCorpusStagingPrefix + "privacy-current"
	marker, candidatePath := writePatternPublicationPrivacyFixture(t, brainDir, candidate, []byte(privacyCanary))
	markerPath := filepath.Join(brainDir, filepath.FromSlash(patternCorpusPublicationMarkerPath))
	candidateRel := filepath.ToSlash(filepath.Join("patterns", candidate))

	plan, err := buildSessionPurgePlan(brainDir, "secret-sess")
	if err != nil {
		t.Fatal(err)
	}
	if !privacyPlanContainsDerived(plan, patternCorpusPublicationMarkerPath) || !privacyPlanContainsDerived(plan, candidateRel) {
		t.Fatalf("publication marker/candidate absent from privacy plan: %+v", plan.DerivedStores)
	}
	requireForwardStateBytes(t, markerPath, marker)
	tombstonePrivacyFixture(t, brainDir)
	future := time.Now().Add(2 * time.Hour)
	for _, path := range []string{markerPath, candidatePath} {
		if err := os.Chtimes(path, future, future); err != nil {
			t.Fatal(err)
		}
	}
	report, err := verifySessionPrivacy(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, finding := range report.Findings {
		found[finding.Artifact] = true
	}
	if report.Clean || !found["pattern_corpus_publication"] || !found["pattern_corpus_staging"] {
		t.Fatalf("future-mtime marker/candidate passed verification: %+v", report.Findings)
	}
	if err := executeSessionCleanup(brainDir, "secret-sess", plan, time.Now().UTC(), "test", true); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{markerPath, candidatePath} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("publication artifact survived cleanup: %s err=%v", path, err)
		}
	}
}

func TestPatternCorpusPublicationPrivacyPreservesForwardCorruptAndUnsafeState(t *testing.T) {
	now := time.Date(2026, 8, 9, 23, 0, 0, 0, time.UTC)
	for _, action := range []struct {
		name string
		run  func(string) error
	}{
		{name: "purge-plan", run: func(brainDir string) error {
			_, err := buildSessionPurgePlan(brainDir, "secret-sess")
			return err
		}},
		{name: "cleanup", run: func(brainDir string) error {
			return executeSessionCleanup(brainDir, "secret-sess", sessionPurgePlan{}, now, "test", true)
		}},
	} {
		t.Run("unknown-newer-"+action.name, func(t *testing.T) {
			brainDir := writePrivacyFixture(t)
			candidate := patternCorpusStagingPrefix + "privacy-future"
			candidateData := []byte(privacyCanary + "-candidate")
			if err := os.MkdirAll(filepath.Join(brainDir, "patterns"), 0o700); err != nil {
				t.Fatal(err)
			}
			candidatePath := filepath.Join(brainDir, "patterns", candidate)
			if err := os.WriteFile(candidatePath, candidateData, 0o600); err != nil {
				t.Fatal(err)
			}
			future := []byte(`{"schema_version":2,"candidate":"` + candidate + `","future":true}` + "\n")
			markerPath := writeForwardStateFixture(t, brainDir, patternCorpusPublicationMarkerPath, future)

			err := action.run(brainDir)
			var typed *patternCorpusPublicationLoadError
			if !errors.As(err, &typed) || typed.Code != memoryErrUnsupportedVersion || typed.State != patternCorpusPublicationUnsupported || typed.Version != patternCorpusPublicationVersion+1 {
				t.Fatalf("error = %v typed=%+v, want typed unsupported vNext", err, typed)
			}
			requireForwardStateBytes(t, markerPath, future)
			requireForwardStateBytes(t, candidatePath, candidateData)
		})
	}

	t.Run("corrupt-current-cleanup", func(t *testing.T) {
		brainDir := writePrivacyFixture(t)
		candidate := patternCorpusStagingPrefix + "privacy-corrupt"
		candidateData := []byte(privacyCanary + "-corrupt-candidate")
		if err := os.MkdirAll(filepath.Join(brainDir, "patterns"), 0o700); err != nil {
			t.Fatal(err)
		}
		candidatePath := filepath.Join(brainDir, "patterns", candidate)
		if err := os.WriteFile(candidatePath, candidateData, 0o600); err != nil {
			t.Fatal(err)
		}
		corrupt := []byte(`{"schema_version":1,"candidate":"` + candidate + `","future":true}` + "\n")
		markerPath := writeForwardStateFixture(t, brainDir, patternCorpusPublicationMarkerPath, corrupt)
		err := executeSessionCleanup(brainDir, "secret-sess", sessionPurgePlan{}, now, "test", true)
		var typed *patternCorpusPublicationLoadError
		if !errors.As(err, &typed) || typed.Code != memoryErrStateCorrupt || typed.State != patternCorpusPublicationCorrupt {
			t.Fatalf("error = %v typed=%+v, want typed corrupt", err, typed)
		}
		requireForwardStateBytes(t, markerPath, corrupt)
		requireForwardStateBytes(t, candidatePath, candidateData)
	})

	t.Run("unsafe-cleanup", func(t *testing.T) {
		brainDir := writePrivacyFixture(t)
		candidate := patternCorpusStagingPrefix + "privacy-unsafe"
		candidateData := []byte(privacyCanary + "-unsafe-candidate")
		if err := os.MkdirAll(filepath.Join(brainDir, "patterns"), 0o700); err != nil {
			t.Fatal(err)
		}
		candidatePath := filepath.Join(brainDir, "patterns", candidate)
		if err := os.WriteFile(candidatePath, candidateData, 0o600); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(t.TempDir(), "publication-canary")
		outsideData := []byte("outside-" + privacyCanary)
		if err := os.WriteFile(outside, outsideData, 0o600); err != nil {
			t.Fatal(err)
		}
		markerPath := filepath.Join(brainDir, filepath.FromSlash(patternCorpusPublicationMarkerPath))
		if err := os.Symlink(outside, markerPath); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		err := executeSessionCleanup(brainDir, "secret-sess", sessionPurgePlan{}, now, "test", true)
		var typed *patternCorpusPublicationLoadError
		if !errors.As(err, &typed) || typed.Code != memoryErrStateUnsafe || typed.State != patternCorpusPublicationUnsafe {
			t.Fatalf("error = %v typed=%+v, want typed unsafe", err, typed)
		}
		if info, statErr := os.Lstat(markerPath); statErr != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("unsafe marker changed: info=%v err=%v", info, statErr)
		}
		requireForwardStateBytes(t, outside, outsideData)
		requireForwardStateBytes(t, candidatePath, candidateData)
	})
}

func TestPatternCorpusPublicationPrivacyRemovalRejectsIdentityTakeover(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	candidate := patternCorpusStagingPrefix + "privacy-race"
	candidateData := []byte(privacyCanary + "-raced-candidate")
	_, candidatePath := writePatternPublicationPrivacyFixture(t, brainDir, candidate, candidateData)
	markerPath := filepath.Join(brainDir, filepath.FromSlash(patternCorpusPublicationMarkerPath))
	replacementPath := filepath.Join(brainDir, "patterns", ".replacement-publication")
	replacement := []byte(`{"schema_version":2,"future":true}` + "\n")
	if err := os.WriteFile(replacementPath, replacement, 0o600); err != nil {
		t.Fatal(err)
	}

	originalLstat := memoryStateLstat
	markerStats := 0
	var swapErr error
	memoryStateLstat = func(path string) (os.FileInfo, error) {
		if path == markerPath {
			markerStats++
			// The first two observations belong to the descriptor-bound read.
			// Replace the pathname immediately before exact-identity removal.
			if markerStats == 3 {
				swapErr = os.Rename(replacementPath, markerPath)
			}
		}
		return originalLstat(path)
	}
	t.Cleanup(func() { memoryStateLstat = originalLstat })
	err := removePatternCorpusPublicationForPrivacy(brainDir)
	if swapErr != nil {
		t.Fatal(swapErr)
	}
	var typed *patternCorpusPublicationLoadError
	if !errors.As(err, &typed) || typed.Code != memoryErrStateUnsafe || typed.State != patternCorpusPublicationUnsafe {
		t.Fatalf("identity takeover error = %v typed=%+v, want typed unsafe", err, typed)
	}
	requireForwardStateBytes(t, markerPath, replacement)
	requireForwardStateBytes(t, candidatePath, candidateData)
}

func TestPatternCorpusRollbackJournalPrivacyInventoryVerifyAndRemoval(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	journalRel := ""
	for _, rel := range patternCorpusPrivacyArtifactRels() {
		if isPatternCorpusRollbackJournalRel(rel) {
			journalRel = rel
			break
		}
	}
	if journalRel == "" {
		t.Fatal("publication sidecar set has no rollback journal")
	}
	journalPath := filepath.Join(brainDir, filepath.FromSlash(journalRel))
	if err := os.MkdirAll(filepath.Dir(journalPath), 0o700); err != nil {
		t.Fatal(err)
	}
	journalData := []byte("sqlite rollback pages: " + privacyCanary)
	if err := os.WriteFile(journalPath, journalData, 0o600); err != nil {
		t.Fatal(err)
	}

	plan, err := buildSessionPurgePlan(brainDir, "secret-sess")
	if err != nil {
		t.Fatal(err)
	}
	if !privacyPlanContainsDerived(plan, journalRel) {
		t.Fatalf("rollback journal absent from privacy plan: %+v", plan.DerivedStores)
	}
	tombstonePrivacyFixture(t, brainDir)
	future := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(journalPath, future, future); err != nil {
		t.Fatal(err)
	}
	report, err := verifySessionPrivacy(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	foundJournal := false
	for _, finding := range report.Findings {
		if finding.Artifact == "pattern_corpus_rollback_journal" && strings.Contains(finding.Detail, journalRel) {
			foundJournal = true
		}
	}
	if report.Clean || !foundJournal {
		t.Fatalf("future-mtime rollback journal passed privacy verification: %+v", report.Findings)
	}

	if err := executeSessionCleanup(brainDir, "secret-sess", plan, time.Now().UTC(), "test", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(journalPath); !os.IsNotExist(err) {
		t.Fatalf("rollback journal survived cleanup: %v", err)
	}
	report, err = verifySessionPrivacy(brainDir)
	if err != nil || !report.Clean {
		t.Fatalf("post-journal cleanup report=%+v err=%v", report, err)
	}
}

func TestPatternCorpusRollbackJournalRemovalRejectsUnsafeAlias(t *testing.T) {
	brainDir := t.TempDir()
	journalRel := "patterns/corpus.sqlite-journal"
	outside := filepath.Join(t.TempDir(), "journal-canary")
	want := []byte(privacyCanary)
	if err := os.WriteFile(outside, want, 0o600); err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(brainDir, filepath.FromSlash(journalRel))
	if err := os.MkdirAll(filepath.Dir(journalPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, journalPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := removePrivacyDerivedStoreArtifact(brainDir, journalRel); err == nil || !strings.Contains(err.Error(), memoryErrStateUnsafe) {
		t.Fatalf("unsafe journal removal error = %v, want %s", err, memoryErrStateUnsafe)
	}
	requireForwardStateBytes(t, outside, want)
}
