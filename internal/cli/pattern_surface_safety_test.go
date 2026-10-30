package cli

import (
	"bytes"
	"github.com/entireio/entire-brain/internal/cli/internal/patterntest"
	patterncore "github.com/entireio/entire-brain/internal/cli/patterns"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
)

func TestStrongestPatternsCheckedRejectsUnsafeLegacyState(t *testing.T) {
	brainDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(brainDir, "patterns"), 0o700); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "procedures.ndjson")
	externalData := []byte("external private pattern state\n")
	if err := os.WriteFile(external, externalData, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(brainDir, filepath.FromSlash(patterncore.ProceduresPath))); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := patterncore.StrongestPatternsChecked(brainDir, 3); err == nil || !strings.Contains(err.Error(), brainstore.StateUnsafeCode) {
		t.Fatalf("unsafe strongest-pattern state error = %v, want %s", err, brainstore.StateUnsafeCode)
	}
	got, err := os.ReadFile(external)
	if err != nil || !bytes.Equal(got, externalData) {
		t.Fatalf("external legacy pattern target changed: err=%v got=%q", err, got)
	}
}

func TestBriefConsolidationsCheckedRejectsMalformedDossier(t *testing.T) {
	brainDir := promotableCorpusDir(t, time.Now())
	db, err := patterntest.OpenCorpusMutableDB(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE dossiers SET json_redacted='{not-json}'`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := loadBriefConsolidationsChecked(brainDir, []string{"deploy"}, 3); err == nil || !strings.Contains(err.Error(), brainstore.StateCorruptCode) {
		t.Fatalf("malformed dossier error = %v, want %s", err, brainstore.StateCorruptCode)
	}
}
