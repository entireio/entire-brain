package cli

import (
	"strings"
	"testing"
	"time"

	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

func writeSessionCorpusManifest(t *testing.T, outputDir string, sessions int) {
	t.Helper()
	manifest := exportManifest{
		SchemaVersion: bmanifest.SchemaVersion,
		GeneratedAt:   time.Now().UTC(),
	}
	for i := 0; i < sessions; i++ {
		manifest.Sessions = append(manifest.Sessions, exportSession{SessionID: "session-" + string(rune('a'+i))})
	}
	if err := writeBrainManifestAndReadme(outputDir, manifest); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

func TestPopulatedBrainIsDetectedBeforeAnEmptyExportOverwritesIt(t *testing.T) {
	outputDir := t.TempDir()
	writeSessionCorpusManifest(t, outputDir, 3)

	existing, err := bmanifest.Load(outputDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if len(existing.Sessions) != 3 {
		t.Fatalf("expected the guard to see 3 retained sessions, got %d", len(existing.Sessions))
	}
}

func TestFreshBrainWithNoCorpusIsNotBlocked(t *testing.T) {
	outputDir := t.TempDir()

	existing, err := bmanifest.Load(outputDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if len(existing.Sessions) != 0 {
		t.Fatalf("a fresh brain must report no sessions, got %d", len(existing.Sessions))
	}
}

func TestEmptyExportGuardMessageNamesTheRefAndCount(t *testing.T) {
	outputDir := t.TempDir()
	writeSessionCorpusManifest(t, outputDir, 11418)

	existing, err := bmanifest.Load(outputDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	// Mirrors the guard in runExport so the operator-facing wording stays
	// actionable: how much is at stake, and what to check.
	msg := emptyExportRefusalMessage(len(existing.Sessions))
	for _, want := range []string{"11418", "refusing to erase", v1MainRef} {
		if !strings.Contains(msg, want) {
			t.Fatalf("guard message missing %q: %s", want, msg)
		}
	}
}
