package cli

import (
	"testing"
	"time"
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
	if err := writeFacts(brainDir, "main", main); err != nil {
		t.Fatal(err)
	}
	if err := writeFacts(brainDir, "feature", feature); err != nil {
		t.Fatal(err)
	}
	// A manifest must exist for updateFactSourceManifestLocked to write into.
	if err := writeBrainManifestAndReadme(brainDir, exportManifest{SchemaVersion: brainManifestSchemaVersion, GeneratedAt: now}); err != nil {
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
	gotMain, err := loadFacts(brainDir, "main")
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
	manifest, err := loadBrainManifest(brainDir)
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
