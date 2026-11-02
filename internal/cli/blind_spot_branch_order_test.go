package cli

import (
	"testing"
	"time"

	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

// The branch note must win over the coverage notes when BOTH are true.
//
// Checked last, this feature was unreachable in the case it exists for.
// `undigested > 0` holds in every repository where a session was captured
// since the last distillation -- which is every actively worked repository,
// and you are on a feature branch BECAUSE you have been working. So the note
// that said "your facts are on branch X, retry with --branch X" was replaced
// by "coverage of older sessions is unknown", which the caller cannot act on.
//

// declareFactsAt is declareFacts with the distill timestamp under test control.
// The existing helper hardcodes LastDistilledAt=now, which is exactly what made
// the undigested>0 path untestable.
func declareFactsAt(t *testing.T, brainDir string, lastDistilled, generated time.Time, total int, branches ...string) {
	t.Helper()
	manifest, err := bmanifest.Load(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	// A brain with no manifest yet loads as an empty struct with nil Sources;
	// a remember-only brain is exactly that case.
	if manifest.Sources == nil {
		manifest.Sources = &brainSources{}
	}
	manifest.Sources.Facts = &factSourceManifest{
		LastDistilledAt: lastDistilled,
		GeneratedAt:     generated,
		Branches:        branches,
		Facts:           total,
		Distilled:       total,
	}
	if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
		t.Fatal(err)
	}
}
