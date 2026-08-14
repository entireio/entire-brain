package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func excludeSessionForIncludeAtomicityTest(t *testing.T, brainDir, sessionID string, now time.Time) {
	t.Helper()
	if err := withBrainPrivacySideEffectLock(brainDir, func() error {
		return withBrainWriteLock(brainDir, func() error {
			plan, err := buildSessionPurgePlan(brainDir, sessionID)
			if err != nil {
				return err
			}
			return executeSessionCleanup(brainDir, sessionID, plan, now, "atomic include test", true)
		})
	}); err != nil {
		t.Fatalf("exclude session: %v", err)
	}
}

func runIncludeSessionForAtomicityTest(brainDir, sessionID string, now time.Time) error {
	return withBrainPrivacySideEffectLock(brainDir, func() error {
		return withBrainWriteLock(brainDir, func() error {
			return includeSessionLocked(brainDir, sessionID, now)
		})
	})
}

func historyIndexContainsForIncludeTest(t *testing.T, brainDir, text string) bool {
	t.Helper()
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	index, err := loadBrainHistoryIndex(brainDir, manifest.Sources.History)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range index.Records {
		if strings.Contains(record.Summary, text) {
			return true
		}
	}
	return false
}

func TestIncludeSessionPublicationFailureRestoresPrivacyStateAndRetries(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	sessionID := "secret-sess"
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	excludeSessionForIncludeAtomicityTest(t, brainDir, sessionID, now)
	if historyIndexContainsForIncludeTest(t, brainDir, privacyCanary) {
		t.Fatal("precondition: excluded session remains in history")
	}

	tombstonePath := filepath.Join(brainDir, filepath.FromSlash(sessionTombstonesPath))
	transactionPath := filepath.Join(brainDir, filepath.FromSlash(privacyTransactionRel(sessionID)))
	tombstonesBefore, err := os.ReadFile(tombstonePath)
	if err != nil {
		t.Fatal(err)
	}
	transactionBefore, err := os.ReadFile(transactionPath)
	if err != nil {
		t.Fatalf("completed exclusion transaction must exist before include: %v", err)
	}
	manifestBefore, err := os.ReadFile(filepath.Join(brainDir, exportManifestFileName))
	if err != nil {
		t.Fatal(err)
	}

	originalRebuild := rebuildSessionHistoryForIncludeLocked
	t.Cleanup(func() { rebuildSessionHistoryForIncludeLocked = originalRebuild })
	injectedErr := errors.New("injected include publication failure")
	rebuildSessionHistoryForIncludeLocked = func(string, time.Time) error { return injectedErr }
	if err := runIncludeSessionForAtomicityTest(brainDir, sessionID, now.Add(time.Minute)); !errors.Is(err, injectedErr) {
		t.Fatalf("include error = %v, want injected publication failure", err)
	}

	tombstonesAfterFailure, err := os.ReadFile(tombstonePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(tombstonesAfterFailure, tombstonesBefore) {
		t.Fatalf("failed include did not restore exact tombstone bytes\n before: %q\n  after: %q", tombstonesBefore, tombstonesAfterFailure)
	}
	transactionAfterFailure, err := os.ReadFile(transactionPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(transactionAfterFailure, transactionBefore) {
		t.Fatalf("failed include did not restore exact transaction bytes\n before: %q\n  after: %q", transactionBefore, transactionAfterFailure)
	}
	manifestAfterFailure, err := os.ReadFile(filepath.Join(brainDir, exportManifestFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(manifestAfterFailure, manifestBefore) {
		t.Fatal("failed publication changed the active manifest")
	}
	stones, _, err := loadSessionTombstonesChecked(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, excluded := stones.Excluded[sessionID]; !excluded {
		t.Fatal("failed include made the session readable")
	}
	if historyIndexContainsForIncludeTest(t, brainDir, privacyCanary) {
		t.Fatal("failed include published the excluded session")
	}

	rebuildSessionHistoryForIncludeLocked = originalRebuild
	if err := runIncludeSessionForAtomicityTest(brainDir, sessionID, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("retry include: %v", err)
	}
	stones, _, err = loadSessionTombstonesChecked(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, excluded := stones.Excluded[sessionID]; excluded {
		t.Fatal("successful retry left the session excluded")
	}
	if _, err := os.Stat(transactionPath); !os.IsNotExist(err) {
		t.Fatalf("successful retry left privacy transaction: %v", err)
	}
	if !historyIndexContainsForIncludeTest(t, brainDir, privacyCanary) {
		t.Fatal("successful retry did not publish the re-included session")
	}
}

func TestIncludeSessionHoldsPrivacyAndBrainLocksThroughPublication(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	sessionID := "secret-sess"
	now := time.Date(2026, 8, 9, 13, 0, 0, 0, time.UTC)
	excludeSessionForIncludeAtomicityTest(t, brainDir, sessionID, now)

	originalRebuild := rebuildSessionHistoryForIncludeLocked
	t.Cleanup(func() { rebuildSessionHistoryForIncludeLocked = originalRebuild })
	publicationStarted := make(chan struct{})
	releasePublication := make(chan struct{})
	rebuildSessionHistoryForIncludeLocked = func(brainDir string, now time.Time) error {
		close(publicationStarted)
		<-releasePublication
		return originalRebuild(brainDir, now)
	}

	includeDone := make(chan error, 1)
	go func() {
		includeDone <- runIncludeSessionForAtomicityTest(brainDir, sessionID, now.Add(time.Minute))
	}()
	select {
	case <-publicationStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("include did not reach publication boundary")
	}

	privacyAttempted := make(chan struct{})
	privacyDone := make(chan error, 1)
	go func() {
		close(privacyAttempted)
		privacyDone <- withBrainPrivacySideEffectLock(brainDir, func() error { return nil })
	}()
	brainAttempted := make(chan struct{})
	brainDone := make(chan error, 1)
	go func() {
		close(brainAttempted)
		brainDone <- withBrainWriteLock(brainDir, func() error { return nil })
	}()
	<-privacyAttempted
	<-brainAttempted
	assertBlocked := func(name string, done <-chan error) {
		t.Helper()
		select {
		case err := <-done:
			t.Fatalf("%s crossed the include publication boundary early: %v", name, err)
		case <-time.After(100 * time.Millisecond):
		}
	}
	assertBlocked("privacy side effect", privacyDone)
	assertBlocked("Brain writer", brainDone)

	close(releasePublication)
	if err := <-includeDone; err != nil {
		t.Fatalf("include: %v", err)
	}
	if err := <-privacyDone; err != nil {
		t.Fatalf("privacy contender: %v", err)
	}
	if err := <-brainDone; err != nil {
		t.Fatalf("Brain contender: %v", err)
	}
	if !historyIndexContainsForIncludeTest(t, brainDir, privacyCanary) {
		t.Fatal("include did not publish the session after releasing the boundary")
	}
}

func TestIncludeSessionTransactionRemovalSyncFailureRestoresExactlyAndRetries(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	sessionID := "secret-sess"
	now := time.Date(2026, 8, 9, 15, 0, 0, 0, time.UTC)
	excludeSessionForIncludeAtomicityTest(t, brainDir, sessionID, now)

	tombstonePath := filepath.Join(brainDir, filepath.FromSlash(sessionTombstonesPath))
	transactionPath := filepath.Join(brainDir, filepath.FromSlash(privacyTransactionRel(sessionID)))
	tombstonesBefore, err := os.ReadFile(tombstonePath)
	if err != nil {
		t.Fatal(err)
	}
	transactionBefore, err := os.ReadFile(transactionPath)
	if err != nil {
		t.Fatal(err)
	}

	originalSync := beforeIncludePrivacyTransactionDirSync
	t.Cleanup(func() { beforeIncludePrivacyTransactionDirSync = originalSync })
	injectedErr := errors.New("injected privacy transaction directory sync failure")
	syncCalls := 0
	beforeIncludePrivacyTransactionDirSync = func(string) error {
		syncCalls++
		return injectedErr
	}
	if err := runIncludeSessionForAtomicityTest(brainDir, sessionID, now.Add(time.Minute)); !errors.Is(err, injectedErr) {
		t.Fatalf("include error = %v, want transaction sync failure", err)
	}
	if syncCalls != 1 {
		t.Fatalf("transaction directory sync calls = %d, want 1", syncCalls)
	}

	tombstonesAfter, err := os.ReadFile(tombstonePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(tombstonesAfter, tombstonesBefore) {
		t.Fatalf("transaction sync failure did not restore exact tombstones\n before: %q\n  after: %q", tombstonesBefore, tombstonesAfter)
	}
	transactionAfter, err := os.ReadFile(transactionPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(transactionAfter, transactionBefore) {
		t.Fatalf("transaction sync failure did not restore exact transaction\n before: %q\n  after: %q", transactionBefore, transactionAfter)
	}
	stones, _, err := loadSessionTombstonesChecked(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, excluded := stones.Excluded[sessionID]; !excluded {
		t.Fatal("transaction sync failure left the session readable")
	}

	beforeIncludePrivacyTransactionDirSync = originalSync
	if err := runIncludeSessionForAtomicityTest(brainDir, sessionID, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("retry include: %v", err)
	}
	if !historyIndexContainsForIncludeTest(t, brainDir, privacyCanary) {
		t.Fatal("successful retry did not publish the re-included session")
	}
}
