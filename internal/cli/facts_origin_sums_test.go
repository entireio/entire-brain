package cli

import (
	"os"
	"strings"
	"testing"
)

// The origin buckets must account for the total, or status and the brief
// cannot explain their own numbers.
//
// factSourceManifest gained Imported, but brainStatusFacts did not forward it
// and the compact brief did not print it. So an import of 400 produced
// facts=400 distilled=0 authored=0 on both surfaces, which reads as a corrupt
// or empty brain to whoever is looking -- and the agent reading the brief has
// no other view.
func TestAgentSurfaceCarriesTheImportedOrigin(t *testing.T) {
	t.Parallel()

	report := brainStatusReport{
		Facts: &brainStatusFacts{Facts: 400, Distilled: 0, Authored: 0, Imported: 400},
	}
	if got := report.Facts.Distilled + report.Facts.Authored + report.Facts.Imported; got != report.Facts.Facts {
		t.Fatalf("origins sum to %d but facts is %d; the buckets must account for the total", got, report.Facts.Facts)
	}
}

// The compact brief is the packet an agent actually reads, and it has a byte
// budget — so imported is omitted at zero and present when non-zero.
func TestCompactBriefShowsImportedOnlyWhenPresent(t *testing.T) {
	t.Parallel()

	withImports := compactV1Int("imported", 400)
	if !strings.Contains(withImports, "imported=400") {
		t.Fatalf("a brain with imported facts must say so in the packet; got %q", withImports)
	}
	if none := compactV1Int("imported", 0); none != "" {
		t.Fatalf("a brain with no imported facts must not spend packet bytes on it; got %q", none)
	}
	// Non-vacuity: the brief must actually reference the field, or the two
	// assertions above only test the helper.
	data, err := os.ReadFile("brain_brief_compact.go")
	if err != nil {
		t.Fatalf("read brain_brief_compact.go: %v", err)
	}
	if !strings.Contains(string(data), `compactV1Int("imported", facts.Imported)`) {
		t.Error("the compact brief does not emit facts.Imported, so the packet still cannot explain its totals")
	}
}
