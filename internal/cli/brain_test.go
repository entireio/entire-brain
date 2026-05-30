package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBrainManifestMigratesFlatExportAndPreservesSeed(t *testing.T) {
	outputDir := t.TempDir()
	seed := &seedSourceManifest{
		GeneratedAt:     time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		WorktreeMode:    "tracked",
		FileFingerprint: "sha256:seed",
		SummaryPath:     "seed/repo-overview.md",
	}
	if err := writeBrainSeedSource(outputDir, "gh/example/repo", seed); err != nil {
		t.Fatalf("write seed source: %v", err)
	}

	session := exportSession{
		SessionID:        "session-one",
		LatestCheckpoint: "aaa111aaa111",
		CreatedAt:        time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		TranscriptPath:   "sessions/main/session.jsonl",
	}
	export := exportManifest{
		SchemaVersion:      1,
		GeneratedAt:        time.Date(2026, 1, 2, 1, 0, 0, 0, time.UTC),
		RepoRoot:           "/repo",
		TranscriptMode:     "compact",
		Scope:              exportScopeAll,
		CheckpointLimit:    10,
		CheckpointsScanned: 1,
		Sessions:           []exportSession{session},
		Branches:           []exportBranch{{Branch: "main", Directory: "sessions/main", SessionCount: 1, Default: true}},
	}
	if err := writeBrainSessionSource(outputDir, "gh/example/repo", export); err != nil {
		t.Fatalf("write session source: %v", err)
	}

	manifest, err := loadBrainManifest(outputDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if manifest.SchemaVersion != brainManifestSchemaVersion {
		t.Fatalf("schema = %d, want %d", manifest.SchemaVersion, brainManifestSchemaVersion)
	}
	if manifest.Sources == nil || manifest.Sources.Seed == nil || manifest.Sources.Sessions == nil {
		t.Fatalf("manifest did not preserve both sources: %+v", manifest.Sources)
	}
	if len(manifest.Sessions) != 1 {
		t.Fatalf("legacy sessions alias missing: %+v", manifest.Sessions)
	}
	readme, err := os.ReadFile(filepath.Join(outputDir, exportReadmeFileName))
	if err != nil {
		t.Fatalf("read readme: %v", err)
	}
	if !strings.Contains(string(readme), "Seeded Baseline") || !strings.Contains(string(readme), "Session History") {
		t.Fatalf("combined readme missing sections:\n%s", readme)
	}
}

func TestLoadBrainManifestMigratesLegacyFlatManifest(t *testing.T) {
	outputDir := t.TempDir()
	legacy := exportManifest{
		SchemaVersion:      1,
		GeneratedAt:        time.Date(2026, 1, 2, 1, 0, 0, 0, time.UTC),
		TranscriptMode:     "raw",
		Scope:              exportScopeBranch,
		CheckpointLimit:    5,
		CheckpointsScanned: 1,
		Sessions: []exportSession{{
			SessionID:        "session-one",
			LatestCheckpoint: "aaa111aaa111",
			CreatedAt:        time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
			TranscriptPath:   "sessions/main/session.jsonl",
		}},
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshal legacy: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outputDir, exportManifestFileName), data, 0o600); err != nil {
		t.Fatalf("write legacy: %v", err)
	}

	manifest, err := loadBrainManifest(outputDir)
	if err != nil {
		t.Fatalf("load legacy: %v", err)
	}
	if manifest.Sources == nil || manifest.Sources.Sessions == nil {
		t.Fatalf("legacy manifest did not get session source: %+v", manifest)
	}
	if manifest.Sources.Sessions.TranscriptMode != "raw" {
		t.Fatalf("transcript mode = %q, want raw", manifest.Sources.Sessions.TranscriptMode)
	}
}
