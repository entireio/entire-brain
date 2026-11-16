package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
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
	manifest, err := bmanifest.Load(brainDir)
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

// The precomputed-integrity variant exists so read surfaces that already
// loaded and inspected the store once do not read it again for the empty-result
// note. It must trust the caller's verdict, not recompute its own.
func TestEmptyResultBlindSpotWithIntegrityUsesCallerVerdict(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	brainDir := writeDistillFixture(t, now)
	manifest, err := bmanifest.Load(brainDir)
	if err != nil {
		t.Fatal(err)
	}

	// A caller that saw loss must get the loss note, even though re-inspecting
	// this brain's (empty, healthy) store would say nothing is wrong.
	lossy := factstore.Integrity{Declared: 5, Readable: 2, Distinct: 2, Mode: factstore.DefectLossy}
	note := emptyResultBlindSpotWithIntegrity(brainDir, "main", manifest, lossy)
	if !strings.Contains(note, "NOT evidence of absence") || !strings.Contains(note, lossy.Warning()) {
		t.Fatalf("lossy integrity must produce the loss note, got %q", note)
	}

	// A healthy verdict must fall through to the same coverage note the
	// re-inspecting surface produces.
	want := emptyResultBlindSpot(brainDir)
	got := emptyResultBlindSpotWithIntegrity(brainDir, "", manifest, factstore.Integrity{})
	if got != want {
		t.Fatalf("healthy integrity must match the re-inspecting note:\n got %q\nwant %q", got, want)
	}
}
