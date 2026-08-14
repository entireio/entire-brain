//go:build !windows

package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIncludeSessionPostRenameFailureRestoresExactlyAndRetries(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	sessionID := "secret-sess"
	now := time.Date(2026, 8, 9, 14, 0, 0, 0, time.UTC)
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
	manifestBefore, err := os.ReadFile(filepath.Join(brainDir, exportManifestFileName))
	if err != nil {
		t.Fatal(err)
	}

	originalHook := atomicReplaceBeforeParentSync
	t.Cleanup(func() { atomicReplaceBeforeParentSync = originalHook })
	injectedErr := errors.New("injected failure after rename before directory sync")
	hookCalls := 0
	atomicReplaceBeforeParentSync = func(path string) error {
		if filepath.Clean(path) != filepath.Clean(tombstonePath) {
			return nil
		}
		hookCalls++
		// Fail both the include write and rollback write. The rollback path must
		// prove its exact bytes are live and complete the missing directory sync.
		if hookCalls <= 2 {
			return injectedErr
		}
		return nil
	}
	if err := runIncludeSessionForAtomicityTest(brainDir, sessionID, now.Add(time.Minute)); !errors.Is(err, injectedErr) {
		t.Fatalf("include error = %v, want post-rename failure", err)
	}
	if hookCalls != 2 {
		t.Fatalf("tombstone post-rename hook calls = %d, want include and rollback", hookCalls)
	}

	tombstonesAfterFailure, err := os.ReadFile(tombstonePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(tombstonesAfterFailure, tombstonesBefore) {
		t.Fatalf("post-rename failure did not restore exact tombstones\n before: %q\n  after: %q", tombstonesBefore, tombstonesAfterFailure)
	}
	transactionAfterFailure, err := os.ReadFile(transactionPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(transactionAfterFailure, transactionBefore) {
		t.Fatalf("post-rename failure changed privacy transaction\n before: %q\n  after: %q", transactionBefore, transactionAfterFailure)
	}
	manifestAfterFailure, err := os.ReadFile(filepath.Join(brainDir, exportManifestFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(manifestAfterFailure, manifestBefore) {
		t.Fatal("post-rename tombstone failure changed active history generation")
	}
	stones, _, err := loadSessionTombstonesChecked(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, excluded := stones.Excluded[sessionID]; !excluded {
		t.Fatal("post-rename failure left the session readable")
	}

	atomicReplaceBeforeParentSync = originalHook
	if err := runIncludeSessionForAtomicityTest(brainDir, sessionID, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("retry include: %v", err)
	}
	if !historyIndexContainsForIncludeTest(t, brainDir, privacyCanary) {
		t.Fatal("successful retry did not publish the re-included session")
	}
	if _, err := os.Stat(transactionPath); !os.IsNotExist(err) {
		t.Fatalf("successful retry left privacy transaction: %v", err)
	}
}
