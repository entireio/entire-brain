package cli

import (
	"bytes"
	"github.com/entireio/entire-brain/internal/cli/command/patternscmd"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
	patterncore "github.com/entireio/entire-brain/internal/cli/patterns"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
)

func TestPatternRunHistoryErrorsAreNotStatusEmpty(t *testing.T) {
	brainDir := t.TempDir()
	manifest := exportManifest{
		SchemaVersion: bmanifest.SchemaVersion,
		Sources:       &brainSources{Patterns: &patternSourceManifest{GeneratedAt: time.Now().UTC()}},
	}
	if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(brainDir, "patterns"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brainDir, filepath.FromSlash(patterncore.RunsRelPath)), []byte("{not-json}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := patterncore.LoadRunsChecked(brainDir); err == nil || !strings.Contains(err.Error(), brainstore.StateCorruptCode) {
		t.Fatalf("malformed run history error = %v, want %s", err, brainstore.StateCorruptCode)
	}
	if _, err := patternscmd.BuildStatusReportChecked(brainDir, patternsCmdDeps()); err == nil || !strings.Contains(err.Error(), brainstore.StateCorruptCode) {
		t.Fatalf("status malformed run error = %v, want %s", err, brainstore.StateCorruptCode)
	}
}

func TestPatternRunHistorySymlinkCannotRedirectReadOrAppend(t *testing.T) {
	brainDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(brainDir, "patterns"), 0o700); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "external.ndjson")
	externalData := []byte(`{"at":"external"}` + "\n")
	if err := os.WriteFile(external, externalData, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(brainDir, filepath.FromSlash(patterncore.RunsRelPath))); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := patterncore.LoadRunsChecked(brainDir); err == nil || !strings.Contains(err.Error(), brainstore.StateUnsafeCode) {
		t.Fatalf("symlinked run history error = %v, want %s", err, brainstore.StateUnsafeCode)
	}
	if err := patterncore.AppendRun(brainDir, patterncore.Run{At: time.Now().UTC().Format(time.RFC3339)}); err == nil || !strings.Contains(err.Error(), brainstore.StateUnsafeCode) {
		t.Fatalf("symlinked run append error = %v, want %s", err, brainstore.StateUnsafeCode)
	}
	got, err := os.ReadFile(external)
	if err != nil || !bytes.Equal(got, externalData) {
		t.Fatalf("external run history changed: err=%v got=%q", err, got)
	}
}
