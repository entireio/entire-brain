package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPatternRunHistoryErrorsAreNotStatusEmpty(t *testing.T) {
	brainDir := t.TempDir()
	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		Sources:       &brainSources{Patterns: &patternSourceManifest{GeneratedAt: time.Now().UTC()}},
	}
	if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(brainDir, "patterns"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brainDir, filepath.FromSlash(patternRunsRelPath)), []byte("{not-json}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPatternRunsChecked(brainDir); err == nil || !strings.Contains(err.Error(), memoryErrStateCorrupt) {
		t.Fatalf("malformed run history error = %v, want %s", err, memoryErrStateCorrupt)
	}
	if _, err := buildPatternsStatusReportChecked(brainDir); err == nil || !strings.Contains(err.Error(), memoryErrStateCorrupt) {
		t.Fatalf("status malformed run error = %v, want %s", err, memoryErrStateCorrupt)
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
	if err := os.Symlink(external, filepath.Join(brainDir, filepath.FromSlash(patternRunsRelPath))); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := loadPatternRunsChecked(brainDir); err == nil || !strings.Contains(err.Error(), memoryErrStateUnsafe) {
		t.Fatalf("symlinked run history error = %v, want %s", err, memoryErrStateUnsafe)
	}
	if err := appendPatternRun(brainDir, patternRun{At: time.Now().UTC().Format(time.RFC3339)}); err == nil || !strings.Contains(err.Error(), memoryErrStateUnsafe) {
		t.Fatalf("symlinked run append error = %v, want %s", err, memoryErrStateUnsafe)
	}
	got, err := os.ReadFile(external)
	if err != nil || !bytes.Equal(got, externalData) {
		t.Fatalf("external run history changed: err=%v got=%q", err, got)
	}
}
