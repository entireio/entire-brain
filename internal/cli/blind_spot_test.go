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
	if !strings.Contains(note, "never been distilled") || !strings.Contains(note, "2") {
		t.Fatalf("undistilled brain should say so with the session count, got %q", note)
	}

	// Distilled, but sessions captured since: name the gap.
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	// Fixture sessions are created at now-2h and now-1h; a distill 90 minutes
	// ago leaves exactly the newer one undigested.
	manifest.Sources.Facts = &factSourceManifest{GeneratedAt: now.Add(-90 * time.Minute)}
	if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
		t.Fatal(err)
	}
	note = emptyResultBlindSpot(brainDir)
	if !strings.Contains(note, "1 session(s) captured since") {
		t.Fatalf("expected the undigested-session count, got %q", note)
	}

	// Fully digested: the empty is honest — say that too.
	manifest.Sources.Facts = &factSourceManifest{GeneratedAt: now}
	if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
		t.Fatal(err)
	}
	note = emptyResultBlindSpot(brainDir)
	if !strings.Contains(note, "may genuinely not be in the brain") {
		t.Fatalf("fully-digested brain should vouch for the empty, got %q", note)
	}

	// No manifest at all: nothing useful to say.
	if note := emptyResultBlindSpot(t.TempDir()); note != "" {
		t.Fatalf("missing manifest should yield no note, got %q", note)
	}
}
