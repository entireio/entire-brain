package cli

import (
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

func TestReclassifyAllFactBranches(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	brainDir := t.TempDir()

	// Facts on two branches with empty Kind (as in a store predating kind-storage).
	main := []factRecord{
		{ID: "fact:inv", Paths: []string{"constraints.api.shape"}, Text: "The token must never be logged.",
			Branch: "main", Origin: "distilled", Status: "active",
			Provenance: []factAnchor{{SessionID: "s1"}}, UpdatedAt: now},
		{ID: "fact:cn", Paths: []string{"architecture.x.y"}, Text: "Approach X was rejected because it was too slow.",
			Branch: "main", Origin: "distilled", Status: "active",
			Provenance: []factAnchor{{SessionID: "s1"}}, UpdatedAt: now},
	}
	feature := []factRecord{
		{ID: "fact:pref", Paths: []string{"preferences.style.x"}, Text: "The user prefers table-driven tests.",
			Branch: "feature", Origin: "distilled", Status: "active",
			Provenance: []factAnchor{{SessionID: "s2"}}, UpdatedAt: now},
	}
	if err := factstore.Write(brainDir, "main", main); err != nil {
		t.Fatal(err)
	}
	if err := factstore.Write(brainDir, "feature", feature); err != nil {
		t.Fatal(err)
	}
	// A manifest must exist for updateFactSourceManifestLocked to write into.
	if err := writeBrainManifestAndReadme(brainDir, exportManifest{SchemaVersion: bmanifest.SchemaVersion, GeneratedAt: now}); err != nil {
		t.Fatal(err)
	}

	changed, err := reclassifyAllFactBranches(brainDir, now)
	if err != nil {
		t.Fatal(err)
	}
	if changed != 3 {
		t.Errorf("changed = %d, want 3", changed)
	}

	// Kinds are now persisted on disk.
	gotMain, err := factstore.Load(brainDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	wantKind := map[string]string{"fact:inv": factKindInvariant, "fact:cn": factKindClosedNegative}
	for _, f := range gotMain {
		if want := wantKind[f.ID]; want != "" && f.Kind != want {
			t.Errorf("%s kind = %q, want %q", f.ID, f.Kind, want)
		}
	}

	// The manifest by_kind histogram is rebuilt.
	manifest, err := bmanifest.Load(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Sources == nil || manifest.Sources.Facts == nil {
		t.Fatal("facts source not written to manifest")
	}
	bk := manifest.Sources.Facts.ByKind
	if bk[factKindInvariant] != 1 || bk[factKindClosedNegative] != 1 || bk[factKindPreference] != 1 {
		t.Errorf("by_kind = %v, want inv/closed-negative/preference each 1", bk)
	}

	// Idempotent: a second pass changes nothing.
	if again, err := reclassifyAllFactBranches(brainDir, now); err != nil || again != 0 {
		t.Errorf("second pass changed=%d err=%v, want 0/nil", again, err)
	}
}

func TestReclassifyAllFactBranchesNoFacts(t *testing.T) {
	// No fact store: a no-op, not an error.
	if changed, err := reclassifyAllFactBranches(t.TempDir(), time.Now().UTC()); err != nil || changed != 0 {
		t.Errorf("changed=%d err=%v, want 0/nil", changed, err)
	}
}

// TestReclassifyDoesNotFoldMirrorsIntoMainAndHealsDuplicates is the COR-2220
// regression. Setup reproduces the damaged shape a connected repo reaches: the
// hosted pull mirrored main's facts (branch stamp unchanged) into a feature
// branch's store, and an earlier reclassify pass — which wrote record-branch
// BUCKETS back into single files — had already doubled main's file. The pass
// must (1) heal the doubled file losslessly, (2) leave the mirror file alone,
// and (3) stay fixed-point: re-running must never grow any store again.
func TestReclassifyDoesNotFoldMirrorsIntoMainAndHealsDuplicates(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	brainDir := t.TempDir()

	fact := factRecord{ID: "fact:dup", Paths: []string{"constraints.api.shape"}, Text: "The token must never be logged.",
		Branch: "main", Origin: "distilled", Status: "active", Kind: factKindInvariant,
		Provenance: []factAnchor{{SessionID: "s1"}}, UpdatedAt: now}
	// Main's file holds the record twice (the doubling damage); the second copy
	// carries an anchor the first lacks, so the fold must union, not discard.
	second := fact
	second.Provenance = []factAnchor{{SessionID: "s2"}}
	if err := factstore.Write(brainDir, "main", []factRecord{fact, second}); err != nil {
		t.Fatal(err)
	}
	// The feature store mirrors the same main-stamped record (hosted pull shape).
	if err := factstore.Write(brainDir, "feature", []factRecord{fact}); err != nil {
		t.Fatal(err)
	}
	if err := writeBrainManifestAndReadme(brainDir, exportManifest{SchemaVersion: bmanifest.SchemaVersion, GeneratedAt: now}); err != nil {
		t.Fatal(err)
	}

	if _, err := reclassifyAllFactBranches(brainDir, now); err != nil {
		t.Fatal(err)
	}
	gotMain, err := factstore.Load(brainDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(gotMain) != 1 || len(gotMain[0].Provenance) != 2 {
		t.Fatalf("main store after heal = %+v, want one record with both anchors", gotMain)
	}
	gotFeature, err := factstore.Load(brainDir, "feature")
	if err != nil {
		t.Fatal(err)
	}
	if len(gotFeature) != 1 {
		t.Fatalf("mirror store = %+v, want untouched single record", gotFeature)
	}

	// Fixed point: another pass must not move records between files or grow any
	// store — the doubling bug was exactly a non-fixed-point rewrite.
	if again, err := reclassifyAllFactBranches(brainDir, now); err != nil || again != 0 {
		t.Fatalf("second pass changed=%d err=%v, want 0/nil", again, err)
	}
	if gotMain, _ = factstore.Load(brainDir, "main"); len(gotMain) != 1 {
		t.Fatalf("main store grew on second pass: %d records", len(gotMain))
	}

	// Integrity: the cross-file mirror is ordinary, not corruption.
	manifest, err := bmanifest.Load(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if report := factstore.Inspect(brainDir, manifest.Sources.Facts); report.Warning() != "" {
		t.Fatalf("mirror reported as corruption: %s", report.Warning())
	}
}
