package factstore

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
)

func writeStoreBytes(t *testing.T, root string, data []byte) {
	t.Helper()
	if err := brainstore.WriteBrainRelativeFileAtomic(root, RepositoryPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeSchemaOneStore(t *testing.T, root string, s legacyV1Store) {
	t.Helper()
	s.SchemaVersion = 1
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	writeStoreBytes(t, root, data)
}

func storeVersionOnDisk(t *testing.T, root string) int {
	t.Helper()
	version, _, present, err := readStoreFile(root)
	if err != nil {
		t.Fatal(err)
	}
	if !present {
		return 0
	}
	return version
}

// (a) A schema-1 store: views flatten, overrides apply, record identity holds.
func TestMigrateSchemaOneStoreWithViewOverrides(t *testing.T) {
	root := t.TempDir()
	rec := repoStoreFact("schema one memory", "main")
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	overrideBranch, err := json.Marshal("feature")
	if err != nil {
		t.Fatal(err)
	}
	writeSchemaOneStore(t, root, legacyV1Store{
		Records: map[string]json.RawMessage{rec.ID: raw},
		Views: map[string]legacyV1View{
			"main-abc123":    {Entries: []legacyV1Entry{{ID: rec.ID}}},
			"feature-def456": {Entries: []legacyV1Entry{{ID: rec.ID, Override: map[string]json.RawMessage{"branch": overrideBranch}}}},
		},
	})
	summary, err := MigrateRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Folded != 1 || summary.StragglerFiles != 0 {
		t.Fatalf("summary wrong: %+v", summary)
	}
	if got := storeVersionOnDisk(t, root); got != repoFactsVersion {
		t.Fatalf("store not upgraded: schema %d", got)
	}
	byID := loadByID(t, root)
	if len(byID) != 1 {
		t.Fatalf("view copies must fold to one record: %+v", byID)
	}
	if _, ok := byID[rec.ID]; !ok {
		t.Fatalf("record identity lost: %+v", byID)
	}
}

// (b) The schema-2 marker world: payloads in legacy branch files fold in,
// dirs park only after the write verified, provenance unions across copies.
func TestMigrateMarkerAndLegacyFiles(t *testing.T) {
	root := t.TempDir()
	writeStoreBytes(t, root, []byte(`{"schema_version":2,"storage":"legacy_views"}`+"\n"))
	shared := repoStoreFact("shared memory", "main")
	mirrored := shared
	mirrored.Provenance = []factmerge.Anchor{{SessionID: "session-other", Commit: "commit-other"}}
	only := repoStoreFact("feature-only memory", "feature")
	writeLegacyFile(t, root, "main-abc123", []factmerge.Record{shared})
	writeLegacyFile(t, root, "feature-def456", []factmerge.Record{mirrored, only})

	summary, err := MigrateRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Folded != 2 || len(summary.MovedDirs) != 2 || summary.StragglerFiles != 0 {
		t.Fatalf("summary wrong: %+v", summary)
	}
	byID := loadByID(t, root)
	if len(byID) != 2 {
		t.Fatalf("expected 2 records after fold: %+v", byID)
	}
	if got := byID[shared.ID]; len(got.Provenance) != 2 {
		t.Fatalf("provenance not unioned: %+v", got.Provenance)
	}
	if dirs := legacyDirsOnDisk(t, root); len(dirs) != 0 {
		t.Fatalf("legacy dirs not parked: %v", dirs)
	}
	if parked := parkedDirs(t, root); len(parked) != 0 {
		t.Fatalf("verified redundant directories remain: %v", parked)
	}
	if summary.Cleanup.ReclaimedBytes == 0 {
		t.Fatal("cleanup did not report reclaimed bytes")
	}

	again, err := MigrateRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	if again.Changed() {
		t.Fatalf("second migration must be a no-op: %+v", again)
	}
}

// (c) A flat store plus a straggler file a released client wrote afterwards.
func TestMigrateFoldsStragglersIntoFlatStore(t *testing.T) {
	root := t.TempDir()
	existing := repoStoreFact("existing memory", "main")
	if err := Write(root, []factmerge.Record{existing}); err != nil {
		t.Fatal(err)
	}
	straggler := repoStoreFact("straggler memory", "main")
	writeLegacyFile(t, root, "main-abc123", []factmerge.Record{straggler})

	summary, err := MigrateRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	if summary.StragglerFiles != 1 || summary.Folded != 1 || len(summary.MovedDirs) != 1 {
		t.Fatalf("straggler summary wrong: %+v", summary)
	}
	byID := loadByID(t, root)
	if len(byID) != 2 {
		t.Fatalf("straggler not folded: %+v", byID)
	}
}

// (d) Mixed: a schema-1 store AND legacy branch files in one brain.
func TestMigrateMixedSchemaOneAndLegacyFiles(t *testing.T) {
	root := t.TempDir()
	inStore := repoStoreFact("store memory", "main")
	raw, err := json.Marshal(inStore)
	if err != nil {
		t.Fatal(err)
	}
	writeSchemaOneStore(t, root, legacyV1Store{
		Records: map[string]json.RawMessage{inStore.ID: raw},
		Views:   map[string]legacyV1View{"main-abc123": {Entries: []legacyV1Entry{{ID: inStore.ID}}}},
	})
	inFile := repoStoreFact("file memory", "feature")
	writeLegacyFile(t, root, "feature-def456", []factmerge.Record{inFile})

	summary, err := MigrateRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Folded != 2 {
		t.Fatalf("mixed fold summary wrong: %+v", summary)
	}
	byID := loadByID(t, root)
	if len(byID) != 2 {
		t.Fatalf("mixed sources not folded: %+v", byID)
	}
	if storeVersionOnDisk(t, root) != repoFactsVersion {
		t.Fatal("store not upgraded")
	}
}

// (e) Retirement wins: an active copy that is not strictly newer never revives
// a retraction; a strictly newer active copy does (a later explicit remember).
func TestMigrateRetirementWins(t *testing.T) {
	base := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name       string
		activeAt   time.Time
		wantStatus string
	}{
		{"stale active copy", base, factmerge.StatusRetracted},
		{"older active copy", base.Add(-time.Hour), factmerge.StatusRetracted},
		{"strictly newer active copy", base.Add(time.Hour), factmerge.StatusActive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, retiredFirst := range []bool{true, false} {
				root := t.TempDir()
				retired := repoStoreFact("contested memory", "main")
				retired.Status = factmerge.StatusRetracted
				retired.UpdatedAt = base
				active := retired
				active.Status = factmerge.StatusActive
				active.UpdatedAt = tc.activeAt
				first, second := retired, active
				if !retiredFirst {
					first, second = active, retired
				}
				writeLegacyFile(t, root, "dir-one", []factmerge.Record{first})
				writeLegacyFile(t, root, "dir-two", []factmerge.Record{second})
				if _, err := MigrateRepository(root); err != nil {
					t.Fatal(err)
				}
				byID := loadByID(t, root)
				if got := byID[retired.ID]; got.Status != tc.wantStatus {
					t.Fatalf("retiredFirst=%v: status %q, want %q", retiredFirst, got.Status, tc.wantStatus)
				}
			}
		})
	}
}

// Published beats staged: a candidate copy never shadows the active one.
func TestMigrateActiveBeatsCandidate(t *testing.T) {
	root := t.TempDir()
	active := repoStoreFact("published memory", "main")
	candidate := active
	candidate.Status = factmerge.StatusCandidate
	candidate.UpdatedAt = active.UpdatedAt.Add(time.Hour)
	writeLegacyFile(t, root, "dir-one", []factmerge.Record{candidate})
	writeLegacyFile(t, root, "dir-two", []factmerge.Record{active})
	if _, err := MigrateRepository(root); err != nil {
		t.Fatal(err)
	}
	if got := loadByID(t, root)[active.ID]; got.Status != factmerge.StatusActive {
		t.Fatalf("candidate shadowed the published record: %+v", got)
	}
}

// (f) A failing migration leaves the legacy sources untouched.
func TestMigrateFailureLeavesSourcesUntouched(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix directory mode test; Windows does not enforce these permission bits")
	}
	root := t.TempDir()
	rec := repoStoreFact("read-only memory", "main")
	writeLegacyFile(t, root, "main-abc123", []factmerge.Record{rec})
	legacyPath := filepath.Join(root, DirName, "main-abc123", FileName)
	before, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	factsDir := filepath.Join(root, DirName)
	if err := os.Chmod(factsDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(factsDir, 0o700) })
	probe := filepath.Join(factsDir, "permission-probe")
	if err := os.WriteFile(probe, nil, 0o600); err == nil {
		_ = os.Remove(probe)
		t.Skip("process can bypass directory permissions")
	}
	if _, err := MigrateRepository(root); err == nil {
		t.Fatal("migration must fail when it cannot write")
	}
	after, err := os.ReadFile(legacyPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("failed migration changed legacy data: %v", err)
	}
	if dirs := legacyDirsOnDisk(t, root); len(dirs) != 1 {
		t.Fatalf("failed migration moved legacy dirs: %v", dirs)
	}
	// Reads still work against the untouched sources.
	if got := loadByID(t, root); len(got) != 1 {
		t.Fatalf("read after failed migration lost data: %+v", got)
	}
}

// A no-op pass writes nothing: an empty brain stays storeless.
func TestMigrateEmptyBrainIsNoOp(t *testing.T) {
	root := t.TempDir()
	summary, err := MigrateRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Changed() {
		t.Fatalf("empty migration reported changes: %+v", summary)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(RepositoryPath))); !os.IsNotExist(err) {
		t.Fatalf("no-op migration created a store: %v", err)
	}
}

// A damaged legacy line fails the migration closed; the bytes stay in place.
func TestMigrateCorruptLegacyFileFailsClosed(t *testing.T) {
	root := t.TempDir()
	rel := filepath.ToSlash(filepath.Join(DirName, "main-abc123", FileName))
	if err := brainstore.WriteBrainRelativeFileAtomic(root, rel, []byte("{broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateRepository(root); err == nil || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("corrupt legacy file must fail migration closed, got %v", err)
	}
	if dirs := legacyDirsOnDisk(t, root); len(dirs) != 1 {
		t.Fatalf("corrupt legacy dir must stay in place: %v", dirs)
	}
}
