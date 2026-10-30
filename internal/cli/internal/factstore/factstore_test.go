package factstore_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
)

func TestFactsRoundTrip(t *testing.T) {
	brainDir := t.TempDir()
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	paths := factmerge.NormalizePaths([]string{"preferences.coding.style"})
	record := factmerge.Record{
		ID:         factmerge.RecordID("Prefers table-driven tests", paths),
		Paths:      paths,
		Text:       "Prefers table-driven tests",
		Branch:     "main",
		Origin:     factstore.OriginAuthored,
		Status:     factmerge.StatusActive,
		Provenance: []factmerge.Anchor{{SessionID: "s1", Commit: "abc123", Line: 42}},
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := factstore.Write(brainDir, "main", []factmerge.Record{record}); err != nil {
		t.Fatalf("factstore.Write: %v", err)
	}
	got, err := factstore.Load(brainDir, "main")
	if err != nil {
		t.Fatalf("factstore.Load: %v", err)
	}
	if len(got) != 1 || got[0].ID != record.ID || got[0].Text != record.Text {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if len(got[0].Provenance) != 1 || got[0].Provenance[0].Line != 42 {
		t.Fatalf("provenance not preserved: %+v", got[0].Provenance)
	}
}

func TestFactTaxonomyRejectsSymlinkedFactsRoot(t *testing.T) {
	brainDir := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(brainDir, factstore.DirName)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	err := factstore.WriteTaxonomy(brainDir, factstore.DefaultTaxonomy(time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)))
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("factstore.WriteTaxonomy should reject symlinked facts root, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, factstore.TaxonomyFileName)); !os.IsNotExist(err) {
		t.Fatalf("taxonomy should not be written through symlink, stat err=%v", err)
	}
}

func TestLoadFactsMissingFileIsEmpty(t *testing.T) {
	records, err := factstore.Load(t.TempDir(), "main")
	if err != nil {
		t.Fatalf("expected nil error for missing file, got %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("expected no records, got %d", len(records))
	}
}

func TestFactTaxonomyDefaultAndRoundTrip(t *testing.T) {
	brainDir := t.TempDir()
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)

	// Missing taxonomy falls back to the shipped default.
	taxonomy, err := factstore.LoadTaxonomy(brainDir, now)
	if err != nil {
		t.Fatalf("factstore.LoadTaxonomy: %v", err)
	}
	if len(taxonomy.Categories) == 0 || len(taxonomy.Paths) == 0 {
		t.Fatalf("default taxonomy is empty: %+v", taxonomy)
	}
	for _, def := range taxonomy.Paths {
		if !factmerge.ValidPath(def.Path) {
			t.Fatalf("default taxonomy ships invalid path: %q", def.Path)
		}
		if !factstore.PathKnownTopLevel(taxonomy, def.Path) {
			t.Fatalf("default taxonomy path %q has unknown top-level", def.Path)
		}
	}

	// A path under an unknown top-level is an orphan.
	if factstore.PathKnownTopLevel(taxonomy, "nonsense.foo.bar") {
		t.Fatalf("unknown top-level should not be recognized")
	}

	if err := factstore.WriteTaxonomy(brainDir, taxonomy); err != nil {
		t.Fatalf("factstore.WriteTaxonomy: %v", err)
	}
	if _, err := os.Stat(filepath.Join(brainDir, filepath.FromSlash(factstore.TaxonomyPath))); err != nil {
		t.Fatalf("taxonomy not written: %v", err)
	}
	reloaded, err := factstore.LoadTaxonomy(brainDir, now)
	if err != nil {
		t.Fatalf("reload taxonomy: %v", err)
	}
	if len(reloaded.Paths) != len(taxonomy.Paths) {
		t.Fatalf("taxonomy round trip changed path count: %d != %d", len(reloaded.Paths), len(taxonomy.Paths))
	}
}

func TestSummarizeFactSource(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	// Every record carries an id, because that is what makes it a fact.
	// factstore.SummarizeSource counts only records that have one (see factstore.IsFactRecord),
	// so the fixture has to be a store of real facts rather than a bag of
	// half-populated structs.
	byBranch := map[string][]factmerge.Record{
		"main": {
			{ID: "fact:main-1", Origin: factstore.OriginDistilled, Status: factmerge.StatusActive, Provenance: []factmerge.Anchor{{SessionID: "s1", Verified: true}}},
			{ID: "fact:main-2", Origin: factstore.OriginAuthored, Status: factmerge.StatusActive, Provenance: []factmerge.Anchor{{SessionID: "s2"}}},
		},
		"feature": {
			{ID: "fact:feature-1", Origin: factstore.OriginDistilled, Status: factmerge.StatusSuperseded, Provenance: []factmerge.Anchor{{SessionID: "s3"}}},
		},
	}
	source := factstore.SummarizeSource(now, byBranch, 100, 7, 4, []string{"w"})
	if source.Facts != 3 {
		t.Errorf("Facts = %d, want 3", source.Facts)
	}
	if source.Proposals != 4 {
		t.Errorf("Proposals = %d, want 4", source.Proposals)
	}
	if source.Distilled != 2 || source.Authored != 1 {
		t.Errorf("origin counts wrong: distilled=%d authored=%d", source.Distilled, source.Authored)
	}
	if source.Superseded != 1 {
		t.Errorf("Superseded = %d, want 1", source.Superseded)
	}
	if source.Verified != 1 || source.Unsigned != 2 {
		t.Errorf("anchor counts wrong: verified=%d unsigned=%d", source.Verified, source.Unsigned)
	}
	if source.ChunksScanned != 100 || source.ChunksDistilled != 7 {
		t.Errorf("chunk counts wrong: scanned=%d distilled=%d", source.ChunksScanned, source.ChunksDistilled)
	}
	if len(source.Branches) != 2 || source.Branches[0] != "feature" || source.Branches[1] != "main" {
		t.Errorf("branches not sorted: %v", source.Branches)
	}
	if source.TaxonomyPath != factstore.TaxonomyPath {
		t.Errorf("TaxonomyPath = %q, want %q", source.TaxonomyPath, factstore.TaxonomyPath)
	}

	// A stored line that parses but carries no id is not a fact and must never
	// be declared as one: counting it here is how a damaged store re-declared
	// its own damage as whole on the next `remember` or `refresh`, hiding the
	// loss from the integrity cross-check that reads this number back.
	byBranch["main"] = append(byBranch["main"], factmerge.Record{Status: factmerge.StatusActive, Origin: factstore.OriginAuthored})
	if got := factstore.SummarizeSource(now, byBranch, 100, 7, 4, nil).Facts; got != 3 {
		t.Errorf("Facts = %d after adding an id-less line, want 3", got)
	}
}
