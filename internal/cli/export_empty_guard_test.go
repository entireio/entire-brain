package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// cleanupStaleSessionFiles treats every transcript that the current export did
// not produce as stale. When an export discovers no sessions at all, that makes
// the entire previously exported corpus stale, so the brain is emptied and the
// history index is republished with nothing in it. The source being briefly
// unreachable (ref not fetched into a fresh clone, checkpoint remote down,
// wrong branch checked out) looks exactly like a repository that genuinely has
// no checkpoints, which is why the empty case has to be refused rather than
// applied.
func TestCleanupStaleSessionFilesErasesCorpusWhenExportFindsNothing(t *testing.T) {
	outputDir := t.TempDir()
	transcript := filepath.Join(outputDir, exportSessionsDirectory, "main", "session-1.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o700); err != nil {
		t.Fatalf("create sessions dir: %v", err)
	}
	if err := os.WriteFile(transcript, []byte("{\"role\":\"user\"}\n"), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	if err := cleanupStaleSessionFiles(outputDir, nil); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	if _, err := os.Stat(transcript); !os.IsNotExist(err) {
		t.Fatalf("expected the documented destructive behaviour to be reproduced, stat err = %v", err)
	}
}

func writeSessionCorpusManifest(t *testing.T, outputDir string, sessions int) {
	t.Helper()
	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
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

	existing, err := loadBrainManifest(outputDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if len(existing.Sessions) != 3 {
		t.Fatalf("expected the guard to see 3 retained sessions, got %d", len(existing.Sessions))
	}
}

func TestFreshBrainWithNoCorpusIsNotBlocked(t *testing.T) {
	outputDir := t.TempDir()

	existing, err := loadBrainManifest(outputDir)
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

	existing, err := loadBrainManifest(outputDir)
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
