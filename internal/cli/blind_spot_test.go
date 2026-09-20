package cli

import (
	"strings"
	"testing"
	"time"
)

func TestEmptyResultBlindSpot(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)

	// Sessions captured, never distilled.
	brainDir := writeDistillFixture(t, now)
	note := emptyResultBlindSpot(brainDir)
	if !strings.Contains(note, "coverage is unknown") || !strings.Contains(note, "2") {
		t.Fatalf("undistilled brain should say so with the session count, got %q", note)
	}

	// Distilled, but sessions captured since: name the gap.
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	// Fixture sessions are created at now-2h and now-1h; a distill 90 minutes
	// ago leaves exactly the newer one undigested.
	manifest.Sources.Facts = &factSourceManifest{LastDistilledAt: now.Add(-90 * time.Minute)}
	if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
		t.Fatal(err)
	}
	note = emptyResultBlindSpot(brainDir)
	if !strings.Contains(note, "1 session(s) captured since") {
		t.Fatalf("expected the undigested-session count, got %q", note)
	}

	// A recent run does not establish complete coverage.
	manifest.Sources.Facts = &factSourceManifest{LastDistilledAt: now}
	if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
		t.Fatal(err)
	}
	note = emptyResultBlindSpot(brainDir)
	if !strings.Contains(note, "complete session coverage is unknown") {
		t.Fatalf("recent distillation should preserve coverage uncertainty, got %q", note)
	}

	// No manifest at all: the case that MOST needs saying, not the one to
	// stay silent about. See TestEmptyResultBlindSpotNamesAMissingBrain.
	if note := emptyResultBlindSpot(t.TempDir()); !strings.Contains(note, "no brain has been built") {
		t.Fatalf("missing manifest must name the missing brain, got %q", note)
	}
}
