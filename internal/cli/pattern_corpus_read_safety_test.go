package cli

import (
	"bytes"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/sqlitedriver"
	patterncore "github.com/entireio/entire-brain/internal/cli/patterns"
	"github.com/spf13/cobra"
)

func TestPatternCorpusReadRejectsExternalSymlinkWithoutTouchingTarget(t *testing.T) {
	brainDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(brainDir, "patterns"), 0o700); err != nil {
		t.Fatal(err)
	}
	externalDir := t.TempDir()
	externalPath := filepath.Join(externalDir, "external.sqlite")
	external, err := sql.Open(sqlitedriver.Name, externalPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := external.Exec(`CREATE TABLE sentinel (value TEXT NOT NULL)`); err != nil {
		external.Close()
		t.Fatal(err)
	}
	if _, err := external.Exec(`INSERT INTO sentinel(value) VALUES ('outside-secret')`); err != nil {
		external.Close()
		t.Fatal(err)
	}
	if err := external.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(externalPath)
	if err != nil {
		t.Fatal(err)
	}

	corpusPath := filepath.Join(brainDir, filepath.FromSlash(patterncore.CorpusPath))
	if err := os.Symlink(externalPath, corpusPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	reader, err := patterncore.OpenCorpusReadDB(brainDir)
	if reader != nil {
		_ = reader.Close()
	}
	if err == nil || !strings.Contains(err.Error(), brainstore.StateUnsafeCode) {
		t.Fatalf("external corpus alias error = %v, want %s", err, brainstore.StateUnsafeCode)
	}
	if err := patterncore.ValidateCorpusReadSafety(brainDir); err == nil || !strings.Contains(err.Error(), brainstore.StateUnsafeCode) {
		t.Fatalf("surface safety gate error = %v, want %s", err, brainstore.StateUnsafeCode)
	}
	if _, _, _, err := patterncore.LoadCorpusPatternViewsChecked(brainDir); err == nil || !strings.Contains(err.Error(), brainstore.StateUnsafeCode) {
		t.Fatalf("checked pattern loader error = %v, want %s", err, brainstore.StateUnsafeCode)
	}
	if allowed, err := privacyDerivedReadGate(brainDir); err == nil || allowed || !strings.Contains(err.Error(), brainstore.StateUnsafeCode) {
		t.Fatalf("derived-read gate = allowed %v error %v, want fail-closed %s", allowed, err, brainstore.StateUnsafeCode)
	}
	after, err := os.ReadFile(externalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("external SQLite target changed through corpus symlink")
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Stat(externalPath + suffix); !os.IsNotExist(err) {
			t.Fatalf("external SQLite sidecar %q was created: %v", suffix, err)
		}
	}

	check, err := sql.Open(sqlitedriver.Name, brainstore.SQLiteReadOnlyDSN(externalPath))
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var sentinel string
	if err := check.QueryRow(`SELECT value FROM sentinel`).Scan(&sentinel); err != nil {
		t.Fatal(err)
	}
	if sentinel != "outside-secret" {
		t.Fatalf("external sentinel = %q", sentinel)
	}
	var patternTables int
	if err := check.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='patterns'`).Scan(&patternTables); err != nil {
		t.Fatal(err)
	}
	if patternTables != 0 {
		t.Fatal("reader executed pattern corpus DDL against external target")
	}
}

func installLatePatternCorpusAlias(t *testing.T, brainDir string) {
	t.Helper()
	corpusPath := filepath.Join(brainDir, filepath.FromSlash(patterncore.CorpusPath))
	backupPath := corpusPath + ".before-alias"
	externalPath := filepath.Join(t.TempDir(), "outside.sqlite")
	want := []byte("outside target must remain untouched")
	if err := os.WriteFile(externalPath, want, 0o600); err != nil {
		t.Fatal(err)
	}
	originalHook := beforeRetrievalResponsePrivacyEmissionCheck
	landed := false
	beforeRetrievalResponsePrivacyEmissionCheck = func() {
		if landed {
			return
		}
		landed = true
		if err := os.Rename(corpusPath, backupPath); err != nil {
			t.Fatalf("move corpus before alias: %v", err)
		}
		if err := os.Symlink(externalPath, corpusPath); err != nil {
			_ = os.Rename(backupPath, corpusPath)
			t.Skipf("symlinks unavailable: %v", err)
		}
	}
	t.Cleanup(func() { beforeRetrievalResponsePrivacyEmissionCheck = originalHook })
	t.Cleanup(func() {
		got, err := os.ReadFile(externalPath)
		if err != nil {
			t.Errorf("read external target: %v", err)
			return
		}
		if !bytes.Equal(got, want) {
			t.Errorf("external target changed: got %q want %q", got, want)
		}
	})
}

func TestPatternDerivedOutputRejectsLateCorpusAliasWithoutBytes(t *testing.T) {
	brainDir := promotableCorpusDir(t, time.Now().UTC())
	policy, err := brainstore.CaptureRetrievalPrivacyPolicy(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	policy.RequireDerivedClean = true
	installLatePatternCorpusAlias(t, brainDir)

	var external bytes.Buffer
	cmd := &cobra.Command{Use: "pattern-output-test"}
	cmd.SetOut(&external)
	err = bufferRetrievalCommandOutput(cmd, []retrievalPrivacyPolicy{policy}, func() error {
		_, err := fmt.Fprint(cmd.OutOrStdout(), "private pattern\n")
		return err
	})
	if err == nil || !strings.Contains(err.Error(), brainstore.StateUnsafeCode) {
		t.Fatalf("late corpus alias error = %v, want %s", err, brainstore.StateUnsafeCode)
	}
	if external.Len() != 0 {
		t.Fatalf("late corpus alias leaked %d bytes: %q", external.Len(), external.String())
	}
}
