package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

func TestFactRecordIDStableForCanonicalPaths(t *testing.T) {
	a := factRecordID("Use tabs, not spaces", []string{"preferences.coding.style", "project.tooling.stack"})
	b := factRecordID("Use tabs, not spaces", []string{"preferences.coding.style", "project.tooling.stack"})
	if a != b {
		t.Fatalf("id not deterministic: %s != %s", a, b)
	}
	// Casing and surrounding whitespace must not change the id.
	c := factRecordID("  use TABS, not spaces  ", []string{"preferences.coding.style", "project.tooling.stack"})
	if a != c {
		t.Fatalf("id should ignore casing/whitespace: %s != %s", a, c)
	}
	// A different statement must produce a different id.
	d := factRecordID("Use spaces, not tabs", []string{"preferences.coding.style", "project.tooling.stack"})
	if a == d {
		t.Fatalf("distinct statements collided: %s", a)
	}
}

func TestNormalizeFactPaths(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"sorts and dedupes", []string{"project.tooling.stack", "architecture.data.flow", "project.tooling.stack"}, []string{"architecture.data.flow", "project.tooling.stack"}},
		{"drops malformed, lowercases the rest", []string{"too.few", "Architecture.Data.Flow", "ok.one.two"}, []string{"architecture.data.flow", "ok.one.two"}},
		{"caps at two", []string{"a.b.c", "d.e.f", "g.h.i"}, []string{"a.b.c", "d.e.f"}},
		{"lowercases and trims", []string{"  PREFERENCES.coding.STYLE "}, []string{"preferences.coding.style"}},
		{"hyphens invalid", []string{"ci-cd.build.step"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeFactPaths(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("got %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestValidFactPath(t *testing.T) {
	valid := []string{"preferences.coding.style", "a.b.c", "ci_cd.build.step", "x1.y2.z3"}
	invalid := []string{"", "a.b", "a.b.c.d", "A.b.c", "ci-cd.build.step", "1a.b.c", "a..c", "a.b.c "}
	for _, p := range valid {
		if !validFactPath(p) {
			t.Errorf("expected valid: %q", p)
		}
	}
	for _, p := range invalid {
		if validFactPath(p) {
			t.Errorf("expected invalid: %q", p)
		}
	}
}

func TestFactWritesRejectSymlinkedFactsBranch(t *testing.T) {
	brainDir := t.TempDir()
	outside := t.TempDir()
	branchDir := filepath.Join(brainDir, filepath.FromSlash(factstore.BranchRelDir("main")))
	if err := os.MkdirAll(filepath.Dir(branchDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, branchDir); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	paths := normalizeFactPaths([]string{"preferences.coding.style"})
	record := factRecord{
		ID:         factRecordID("Reject symlink facts branch", paths),
		Paths:      paths,
		Text:       "Reject symlink facts branch",
		Branch:     "main",
		Origin:     factstore.OriginAuthored,
		Status:     factStatusActive,
		Provenance: []factAnchor{{SessionID: "s1"}},
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := factstore.Write(brainDir, "main", []factRecord{record}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("factstore.Write should reject symlinked branch dir, got %v", err)
	}
	if err := writeFactProposals(brainDir, "main", []factProposal{{Action: "merge", CandidateID: "fact:a", TargetID: "fact:b"}}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("writeFactProposals should reject symlinked branch dir, got %v", err)
	}
	if err := writeFactProposals(brainDir, "main", nil); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("empty writeFactProposals should reject symlinked branch dir before remove, got %v", err)
	}
	saveDistillCache(brainDir, distillCache{Version: distillCacheVersion, Sessions: map[string]string{"main/s1": "fp"}})
	if _, err := os.Stat(filepath.Join(outside, distillCacheFileName)); !os.IsNotExist(err) {
		t.Fatalf("distill cache should not be written through symlink, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, factstore.FileName)); !os.IsNotExist(err) {
		t.Fatalf("facts file should not be written through symlink, stat err=%v", err)
	}
}

func TestUpsertFactDedupesAndUnionsProvenance(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	later := now.Add(time.Hour)
	paths := normalizeFactPaths([]string{"project.tooling.stack"})
	id := factRecordID("Uses Go 1.26", paths)
	base := factRecord{
		ID:         id,
		Paths:      paths,
		Text:       "Uses Go 1.26",
		Origin:     factstore.OriginDistilled,
		Status:     factStatusActive,
		Provenance: []factAnchor{{SessionID: "s1", Line: 10}},
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	set := upsertFact(nil, base)

	// Re-distilling the same statement from a new turn unions provenance,
	// does not duplicate the record, and advances UpdatedAt.
	dup := base
	dup.Provenance = []factAnchor{{SessionID: "s2", Line: 20}}
	dup.UpdatedAt = later
	set = upsertFact(set, dup)

	if len(set) != 1 {
		t.Fatalf("expected dedupe to one record, got %d", len(set))
	}
	if len(set[0].Provenance) != 2 {
		t.Fatalf("expected unioned provenance of 2, got %d", len(set[0].Provenance))
	}
	if !set[0].UpdatedAt.Equal(later) {
		t.Fatalf("expected UpdatedAt to advance to %v, got %v", later, set[0].UpdatedAt)
	}

	// A re-submission with an already-seen anchor must not grow provenance.
	set = upsertFact(set, dup)
	if len(set[0].Provenance) != 2 {
		t.Fatalf("anchor union not idempotent: got %d", len(set[0].Provenance))
	}

	// A genuinely different fact is appended.
	other := base
	other.Text = "Uses Rust"
	other.ID = factRecordID(other.Text, paths)
	set = upsertFact(set, other)
	if len(set) != 2 {
		t.Fatalf("expected distinct fact appended, got %d", len(set))
	}
}

func TestUpdateFactSourceManifestPreservesDistillEvidence(t *testing.T) {
	brainDir := t.TempDir()
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	paths := normalizeFactPaths([]string{"preferences.coding.style"})
	if err := factstore.Write(brainDir, "main", []factRecord{{
		ID:         factRecordID("Keep distill evidence", paths),
		Paths:      paths,
		Text:       "Keep distill evidence",
		Branch:     "main",
		Origin:     factstore.OriginAuthored,
		Status:     factStatusActive,
		Provenance: []factAnchor{{SessionID: "s1"}},
		CreatedAt:  now,
		UpdatedAt:  now,
	}}); err != nil {
		t.Fatalf("write facts: %v", err)
	}
	prior := &factSourceManifest{
		GeneratedAt:           now.Add(-time.Hour),
		TaxonomyPath:          factstore.TaxonomyPath,
		ChunksScanned:         42,
		ChunksDistilled:       17,
		CacheHits:             9,
		FailedChunks:          3,
		PreprocessedBytes:     123456,
		ExtractionWaitSeconds: 1.25,
		ReconcileSeconds:      0.5,
		WriteSeconds:          0.125,
		Warnings:              []string{"agent timeout"},
	}
	if err := writeBrainManifestAndReadme(brainDir, exportManifest{SchemaVersion: bmanifest.SchemaVersion, GeneratedAt: now, Sources: &brainSources{Facts: prior}}); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	if err := updateFactSourceManifestLocked(brainDir, now.Add(time.Minute)); err != nil {
		t.Fatalf("update fact source: %v", err)
	}
	manifest, err := bmanifest.Load(brainDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	source := manifest.Sources.Facts
	if source.Facts != 1 || source.Authored != 1 || source.Unsigned != 1 {
		t.Fatalf("live fact counts were not rebuilt: %+v", source)
	}
	if source.ChunksScanned != prior.ChunksScanned || source.ChunksDistilled != prior.ChunksDistilled ||
		source.CacheHits != prior.CacheHits || source.FailedChunks != prior.FailedChunks ||
		source.PreprocessedBytes != prior.PreprocessedBytes ||
		source.ExtractionWaitSeconds != prior.ExtractionWaitSeconds ||
		source.ReconcileSeconds != prior.ReconcileSeconds ||
		source.WriteSeconds != prior.WriteSeconds {
		t.Fatalf("distill evidence was not preserved: %+v", source)
	}
	if len(source.Warnings) != 1 || source.Warnings[0] != "agent timeout" {
		t.Fatalf("distill warnings were not preserved: %+v", source.Warnings)
	}
}
