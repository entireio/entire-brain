package factstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestLegacyCleanupAlreadyFlat(t *testing.T) {
	root := t.TempDir()
	rec := repoStoreFact("already migrated memory", "main")
	writeLegacyFile(t, root, "main-abc123", []factmerge.Record{rec})
	if err := writeFlatStore(root, []factmerge.Record{rec}); err != nil {
		t.Fatal(err)
	}
	if _, err := parkLegacyDirs(root); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateRepository(root); err != nil {
		t.Fatal(err)
	}
	if dirs := parkedDirs(t, root); len(dirs) != 0 {
		t.Fatalf("verified legacy copies remain after ordinary reconciliation: %v", dirs)
	}
	got := loadByID(t, root)
	if got[rec.ID].Text != rec.Text {
		t.Fatal("memory changed")
	}
}

func cleanupFixture(t *testing.T) (string, string, factmerge.Record) {
	t.Helper()
	root := t.TempDir()
	rec := repoStoreFact("cleanup memory", "main")
	writeLegacyFile(t, root, "main-abc123", []factmerge.Record{rec})
	if err := writeFlatStore(root, []factmerge.Record{rec}); err != nil {
		t.Fatal(err)
	}
	if _, err := parkLegacyDirs(root); err != nil {
		t.Fatal(err)
	}
	return root, filepath.Join(root, DirName, parkedDirs(t, root)[0], "main-abc123"), rec
}

func runCleanup(t *testing.T, root string) CleanupStatus {
	t.Helper()
	report, err := CleanupLegacy(root)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func TestLegacyCleanupPreservesUnverifiedData(t *testing.T) {
	for _, tc := range []string{"missing", "changed-text", "changed-kind", "changed-provenance", "unknown-field", "unknown-anchor-field", "invalid-id", "corrupt", "duplicate-key", "duplicate-id", "invalid-receipt", "changed-after-receipt"} {
		t.Run(tc, func(t *testing.T) {
			root, branch, rec := cleanupFixture(t)
			path := filepath.Join(branch, FileName)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			switch tc {
			case "missing":
				if err := writeFlatStore(root, nil); err != nil {
					t.Fatal(err)
				}
			case "changed-text":
				rec.Text = "different later value"
				if err := writeFlatStore(root, []factmerge.Record{rec}); err != nil {
					t.Fatal(err)
				}
			case "changed-kind":
				rec.Kind = "decision"
				if err := writeFlatStore(root, []factmerge.Record{rec}); err != nil {
					t.Fatal(err)
				}
			case "changed-provenance":
				rec.Provenance = nil
				if err := writeFlatStore(root, []factmerge.Record{rec}); err != nil {
					t.Fatal(err)
				}
			case "unknown-field":
				raw = append([]byte(`{"future_value":1,`), bytes.TrimSpace(raw)[1:]...)
			case "unknown-anchor-field":
				raw = bytes.Replace(raw, []byte(`"session_id"`), []byte(`"future_anchor":true,"session_id"`), 1)
			case "invalid-id":
				raw = bytes.Replace(raw, []byte(rec.ID), []byte(""), 1)
			case "corrupt":
				raw = []byte("{broken\n")
			case "duplicate-key":
				raw = append([]byte(`{"text":"lost duplicate",`), bytes.TrimSpace(raw)[1:]...)
			case "duplicate-id":
				raw = append(raw, raw...)
			case "invalid-receipt":
				if err := os.WriteFile(filepath.Join(branch, cleanupReceiptName), []byte(`{"schema_version":99}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "changed-after-receipt":
				cleanupFault = func(stage string) error {
					if stage == "verified" {
						return errors.New("interrupted")
					}
					return nil
				}
				t.Cleanup(func() { cleanupFault = nil })
				runCleanup(t, root)
				cleanupFault = nil
				raw = append(raw, '\n')
			}
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			storeBefore, _ := os.ReadFile(filepath.Join(root, RepositoryPath))
			report := runCleanup(t, root)
			if report.State != "retained" {
				t.Fatalf("unsafe cleanup accepted: %+v", report)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(after, raw) {
				t.Fatalf("recovery bytes changed: %v", err)
			}
			storeAfter, _ := os.ReadFile(filepath.Join(root, RepositoryPath))
			if !bytes.Equal(storeBefore, storeAfter) {
				t.Fatal("cleanup modified active memories")
			}
		})
	}
}

func TestLegacyCleanupResumesAfterDeletionAndLaterMutation(t *testing.T) {
	for _, stage := range []string{"verified", "removed"} {
		t.Run(stage, func(t *testing.T) {
			root, _, rec := cleanupFixture(t)
			cleanupFault = func(at string) error {
				if at == stage {
					return errors.New("interrupted")
				}
				return nil
			}
			t.Cleanup(func() { cleanupFault = nil })
			report := runCleanup(t, root)
			if report.State != "retained" {
				t.Fatalf("fault missed: %+v", report)
			}
			cleanupFault = nil
			// This intentional deletion happens after the receipt proved preservation.
			if err := writeFlatStore(root, nil); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 3; i++ {
				runCleanup(t, root)
			}
			if dirs := parkedDirs(t, root); len(dirs) != 0 {
				t.Fatalf("retry left directories: %v", dirs)
			}
			if _, ok := loadByID(t, root)[rec.ID]; ok {
				t.Fatal("cleanup resurrected intentionally deleted memory")
			}
		})
	}
}

func TestLegacyCleanupRetractionDoesNotRevive(t *testing.T) {
	root, _, rec := cleanupFixture(t)
	rec.Status = factmerge.StatusRetracted
	rec.UpdatedAt = rec.UpdatedAt.Add(time.Hour)
	if err := writeFlatStore(root, []factmerge.Record{rec}); err != nil {
		t.Fatal(err)
	}
	runCleanup(t, root)
	if len(parkedDirs(t, root)) != 0 {
		t.Fatal("verified retirement retained redundant active copy")
	}
	if loadByID(t, root)[rec.ID].Status != factmerge.StatusRetracted {
		t.Fatal("memory revived")
	}
}

func TestLegacyCleanupPreservesOtherFiles(t *testing.T) {
	root, branch, _ := cleanupFixture(t)
	for _, name := range []string{"hosted.json", "customer-backup.txt"} {
		if err := os.WriteFile(filepath.Join(branch, name), []byte("preserve"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	runCleanup(t, root)
	if _, err := os.Stat(filepath.Join(branch, FileName)); !os.IsNotExist(err) {
		t.Fatal("redundant fact file remains")
	}
	for _, name := range []string{"hosted.json", "customer-backup.txt"} {
		data, err := os.ReadFile(filepath.Join(branch, name))
		if err != nil || string(data) != "preserve" {
			t.Fatalf("unrelated data changed: %s", name)
		}
	}
}

func TestLegacyCleanupDoesNotFollowSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires privileges on Windows")
	}
	for _, level := range []string{"facts", "park", "branch", "file", "receipt"} {
		t.Run(level, func(t *testing.T) {
			root, branch, _ := cleanupFixture(t)
			outside := t.TempDir()
			sentinel := filepath.Join(outside, "sentinel")
			os.WriteFile(sentinel, []byte("preserve"), 0600)
			path := map[string]string{"facts": filepath.Join(root, DirName), "park": filepath.Dir(branch), "branch": branch, "file": filepath.Join(branch, FileName), "receipt": filepath.Join(branch, cleanupReceiptName)}[level]
			if level != "receipt" {
				if err := os.Rename(path, path+"-saved"); err != nil {
					t.Fatal(err)
				}
			}
			dest := outside
			if level == "file" || level == "receipt" {
				dest = sentinel
			}
			if err := os.Symlink(dest, path); err != nil {
				t.Fatal(err)
			}
			_, _ = CleanupLegacy(root)
			data, err := os.ReadFile(sentinel)
			if err != nil || string(data) != "preserve" {
				t.Fatal("followed symlink")
			}
		})
	}
}

func TestLegacyCleanupSkipsBusyWriter(t *testing.T) {
	root, _, _ := cleanupFixture(t)
	unlock, err := brainstore.AcquireBrainWriteLock(root)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	start := time.Now()
	report, err := CleanupLegacy(root)
	if err != nil || report.State != "pending" || time.Since(start) > time.Second {
		t.Fatalf("maintenance waited for writer: %+v %v", report, err)
	}
	if len(parkedDirs(t, root)) != 1 {
		t.Fatal("busy cleanup changed files")
	}
}

func TestLegacyCleanupBatchMakesProgressPastRetainedFiles(t *testing.T) {
	root := t.TempDir()
	rec := repoStoreFact("valid later memory", "main")
	if err := writeFlatStore(root, []factmerge.Record{rec}); err != nil {
		t.Fatal(err)
	}
	group := filepath.Join(root, DirName, ".legacy-test")
	for i := 0; i < cleanupBatchFiles+5; i++ {
		dir := filepath.Join(group, fmt.Sprintf("branch-%04d", i))
		os.MkdirAll(dir, 0700)
		raw := []byte("{broken\n")
		if i >= cleanupBatchFiles {
			raw, _ = json.Marshal(rec)
		}
		os.WriteFile(filepath.Join(dir, FileName), raw, 0600)
	}
	for i := 0; i < 12; i++ {
		runCleanup(t, root)
	}
	for i := cleanupBatchFiles; i < cleanupBatchFiles+5; i++ {
		if _, err := os.Stat(filepath.Join(group, fmt.Sprintf("branch-%04d", i), FileName)); !os.IsNotExist(err) {
			t.Fatal("retained files starved later valid files")
		}
	}
	if _, err := os.Stat(filepath.Join(group, "branch-0000", FileName)); err != nil {
		t.Fatal("corrupt recovery file was removed")
	}
}

func TestLegacyCleanupWritePreservesIntentionalRemoval(t *testing.T) {
	root := t.TempDir()
	rec := repoStoreFact("removed by caller", "main")
	writeLegacyFile(t, root, "main-old", []factmerge.Record{rec})
	if err := Write(root, nil); err != nil {
		t.Fatal(err)
	}
	if len(loadByID(t, root)) != 0 {
		t.Fatal("pre-mutation migration resurrected removed record")
	}
	if len(parkedDirs(t, root)) != 0 {
		t.Fatal("pre-mutation cleanup did not reclaim verified copies")
	}
}

func TestLegacyCleanupDoesNotCreateUnusedStore(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	if _, err := CleanupLegacy(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("maintenance created unused store")
	}
}

func TestFlatMigrationVerifiesCompleteRecord(t *testing.T) {
	root := t.TempDir()
	rec := repoStoreFact("expected text", "main")
	changed := rec
	changed.Text = "wrong text"
	if err := writeFlatStore(root, []factmerge.Record{changed}); err != nil {
		t.Fatal(err)
	}
	if err := verifyFlatStore(root, []factmerge.Record{rec}); err == nil {
		t.Fatal("ID-only verification accepted changed payload")
	}
}

func TestLegacyCleanupRealProcessInterruption(t *testing.T) {
	if root := os.Getenv("BRAIN_CLEANUP_CRASH_ROOT"); root != "" {
		cleanupFault = func(stage string) error {
			if stage == os.Getenv("BRAIN_CLEANUP_CRASH_STAGE") {
				os.Exit(42)
			}
			return nil
		}
		if os.Getenv("BRAIN_CLEANUP_CRASH_MODE") == "migration" {
			_, _ = MigrateRepository(root)
		} else {
			_, _ = CleanupLegacy(root)
		}
		os.Exit(43)
	}
	for _, stage := range []string{"receipt-written", "verified", "removed"} {
		t.Run(stage, func(t *testing.T) {
			root, _, rec := cleanupFixture(t)
			cmd := exec.Command(os.Args[0], "-test.run=^TestLegacyCleanupRealProcessInterruption$")
			cmd.Env = append(os.Environ(), "BRAIN_CLEANUP_CRASH_ROOT="+root, "BRAIN_CLEANUP_CRASH_STAGE="+stage)
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 42 {
				t.Fatalf("child missed crash: %v %s", err, out)
			}
			for i := 0; i < 3; i++ {
				runCleanup(t, root)
			}
			if len(parkedDirs(t, root)) != 0 {
				t.Fatal("crash left cleanup incomplete")
			}
			if loadByID(t, root)[rec.ID].Text != rec.Text {
				t.Fatal("crash lost memory")
			}
		})
	}
}

func TestLegacyCleanupLargeFileUsesPreparedProof(t *testing.T) {
	root := t.TempDir()
	rec := repoStoreFact("large fact", "main")
	rec.Text = strings.Repeat("large memory ", 100000)
	writeLegacyFile(t, root, "main-large", []factmerge.Record{rec})
	summary, err := MigrateRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Cleanup.PendingDirs != 1 {
		t.Fatal("large verification ran under the migration lock")
	}
	report := runCleanup(t, root)
	if report.PendingDirs != 0 || len(parkedDirs(t, root)) != 0 {
		t.Fatalf("large cleanup did not finish: %+v", report)
	}
	if loadByID(t, root)[rec.ID].Text != rec.Text {
		t.Fatal("large record changed")
	}
}

func TestLegacyCleanupRejectsStalePreparedProof(t *testing.T) {
	root := t.TempDir()
	rec := repoStoreFact("large fact", "main")
	rec.Text = strings.Repeat("large memory ", 100000)
	writeLegacyFile(t, root, "main-large", []factmerge.Record{rec})
	if _, err := MigrateRepository(root); err != nil {
		t.Fatal(err)
	}
	cleanupFault = func(stage string) error {
		if stage == "prepared" {
			return writeFlatStore(root, nil)
		}
		return nil
	}
	t.Cleanup(func() { cleanupFault = nil })
	runCleanup(t, root)
	cleanupFault = nil
	if len(parkedDirs(t, root)) == 0 {
		t.Fatal("deleted recovery data using stale verification")
	}
	if len(loadByID(t, root)) != 0 {
		t.Fatal("restored intentionally removed memory")
	}
}

func TestLegacyCleanupRejectsChangedFileBeforeUnlink(t *testing.T) {
	root, branch, _ := cleanupFixture(t)
	path := filepath.Join(branch, FileName)
	cleanupFault = func(stage string) error {
		if stage == "verified" {
			return os.WriteFile(path, []byte("new recovery bytes"), 0600)
		}
		return nil
	}
	t.Cleanup(func() { cleanupFault = nil })
	report := runCleanup(t, root)
	if report.State != "retained" {
		t.Fatalf("changed data not retained: %+v", report)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "new recovery bytes" {
		t.Fatal("changed file was deleted")
	}
}

func TestLegacyMigrationRetainsConflictingMetadata(t *testing.T) {
	for _, field := range []string{"kind", "confidence", "author", "created-at", "verified-anchor"} {
		t.Run(field, func(t *testing.T) {
			root := t.TempDir()
			a := repoStoreFact("same content with conflicting metadata", "main")
			b := a
			switch field {
			case "kind":
				a.Kind, b.Kind = "decision", "constraint"
			case "confidence":
				a.Confidence, b.Confidence = "0.7", "0.9"
			case "author":
				a.Author, b.Author = "first", "second"
			case "created-at":
				b.CreatedAt = a.CreatedAt.Add(time.Hour)
			case "verified-anchor":
				a.Provenance = []factmerge.Anchor{{SessionID: "s", Verified: false}}
				b.Provenance = []factmerge.Anchor{{SessionID: "s", Verified: true}}
			}
			writeLegacyFile(t, root, "a", []factmerge.Record{a})
			writeLegacyFile(t, root, "b", []factmerge.Record{b})
			if _, err := MigrateRepository(root); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 3; i++ {
				runCleanup(t, root)
			}
			paths, _ := filepath.Glob(filepath.Join(root, DirName, ".legacy-*", "*", FileName))
			if len(paths) == 0 {
				t.Fatal("conflicting metadata lost its recovery copy")
			}
		})
	}
}

func TestLegacyMigrationLargeVerifiedCopySurvivesLaterDeletion(t *testing.T) {
	root := t.TempDir()
	rec := repoStoreFact(strings.Repeat("large verified memory ", 65000), "main")
	writeLegacyFile(t, root, "main-large", []factmerge.Record{rec})
	cleanupFault = func(stage string) error {
		if stage == "verified" {
			return errors.New("interrupt after durable receipt")
		}
		return nil
	}
	t.Cleanup(func() { cleanupFault = nil })
	if err := Write(root, nil); err != nil {
		t.Fatal(err)
	}
	if len(parkedDirs(t, root)) == 0 {
		t.Fatal("interruption did not retain recovery copy")
	}
	cleanupFault = nil
	runCleanup(t, root)
	if len(parkedDirs(t, root)) != 0 {
		t.Fatal("verified large copy did not resume cleanup")
	}
	if len(loadByID(t, root)) != 0 {
		t.Fatal("cleanup resurrected intentionally removed memory")
	}
}

func TestLegacyMigrationRealProcessInterruption(t *testing.T) {
	for _, stage := range []string{"receipt-written", "verified", "removed"} {
		t.Run(stage, func(t *testing.T) {
			root := t.TempDir()
			a := repoStoreFact("first migration source", "main")
			b := repoStoreFact("second migration source", "feature")
			writeLegacyFile(t, root, "a", []factmerge.Record{a})
			writeLegacyFile(t, root, "b", []factmerge.Record{b})
			cmd := exec.Command(os.Args[0], "-test.run=^TestLegacyCleanupRealProcessInterruption$")
			cmd.Env = append(os.Environ(), "BRAIN_CLEANUP_CRASH_ROOT="+root, "BRAIN_CLEANUP_CRASH_MODE=migration", "BRAIN_CLEANUP_CRASH_STAGE="+stage)
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 42 {
				t.Fatalf("child missed crash: %v %s", err, out)
			}
			if _, err := MigrateRepository(root); err != nil {
				t.Fatal(err)
			}
			runCleanup(t, root)
			if len(parkedDirs(t, root)) != 0 {
				t.Fatal("migration crash left verified copies")
			}
			got := loadByID(t, root)
			if len(got) != 2 || got[a.ID].Text != a.Text || got[b.ID].Text != b.Text {
				t.Fatal("migration crash lost memory")
			}
		})
	}
}

func TestLegacyCleanupReceiptFailurePreservesCommittedWrite(t *testing.T) {
	for _, failure := range []error{os.ErrPermission, syscall.ENOSPC} {
		t.Run(failure.Error(), func(t *testing.T) {
			root, branch, original := cleanupFixture(t)
			before, err := os.ReadFile(filepath.Join(branch, FileName))
			if err != nil {
				t.Fatal(err)
			}
			cleanupFault = func(stage string) error {
				if stage == "receipt-written" {
					return failure
				}
				return nil
			}
			t.Cleanup(func() { cleanupFault = nil })
			newRecord := repoStoreFact("committed despite cleanup failure", "main")
			if err := Write(root, []factmerge.Record{original, newRecord}); err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(filepath.Join(branch, FileName))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("cleanup failure removed recovery bytes")
			}
			if len(loadByID(t, root)) != 2 {
				t.Fatal("cleanup failure lost committed write")
			}
			cleanupFault = nil
			runCleanup(t, root)
			if len(parkedDirs(t, root)) != 0 {
				t.Fatal("cleanup did not recover after receipt error")
			}
		})
	}
}

func TestLegacyCleanupPinnedDirectoryReplacement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows directory handle prevents this replacement")
	}
	root, branch, _ := cleanupFixture(t)
	external := t.TempDir()
	outside := filepath.Join(external, FileName)
	os.WriteFile(outside, []byte("external data"), 0600)
	cleanupFault = func(stage string) error {
		if stage != "verified" {
			return nil
		}
		if err := os.Rename(branch, branch+"-moved"); err != nil {
			return err
		}
		return os.Symlink(external, branch)
	}
	t.Cleanup(func() { cleanupFault = nil })
	report := runCleanup(t, root)
	if report.State != "retained" {
		t.Fatalf("replacement not reported: %+v", report)
	}
	data, err := os.ReadFile(outside)
	if err != nil || string(data) != "external data" {
		t.Fatal("cleanup followed replacement path")
	}
}
