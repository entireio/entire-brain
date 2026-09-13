package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// docsAvailabilityFixture builds a brain whose manifest declares a docs index
// AND whose docs index is really on disk, so the only variable in the tests
// below is whether that index survives.
func docsAvailabilityFixture(t *testing.T) (Options, string, string) {
	t.Helper()
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	env.RepoRoot = repoDir
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	now := time.Date(2026, 8, 10, 2, 0, 0, 0, time.UTC)
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return now }}
	storage, err := repoStoragePaths(context.Background(), runner, env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(storage.BrainDir, 0o700); err != nil {
		t.Fatal(err)
	}
	index := docIndex{GeneratedAt: now, Records: []docRecord{
		{ID: "doc-1", Path: "seed/docs/ARCH.md", Heading: "Arch", Line: 1, Text: "the alpha subsystem uses postgres for storage"},
	}}
	data, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeBrainRelativeFileAtomic(storage.BrainDir, docIndexPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		RepoKey:       storage.Key,
		GeneratedAt:   now,
		Sources: &brainSources{
			Seed: &seedSourceManifest{GeneratedAt: now, Commit: "aaa111", WorktreeMode: "head", SummaryPath: "seed/summary.md"},
			Docs: &docSourceManifest{GeneratedAt: now, IndexPath: docIndexPath, Records: 1, Files: 1},
		},
	}
	if err := writeBrainManifestAndReadme(storage.BrainDir, manifest); err != nil {
		t.Fatal(err)
	}
	return opts, repoDir, storage.BrainDir
}

// TestStatusReportsDeclaredDocsIndexMissing is the honesty contract: when the
// manifest declares a docs index that is no longer on disk, retrieval silently
// drops the entire docs layer (loadDocIndex's os.IsNotExist is skipped, not
// raised), so status/brain_status must not keep reporting "docs: ok" and a
// freshness severity of "ok". A caller cannot otherwise tell a dead docs layer
// apart from "nothing matched".
func TestStatusReportsDeclaredDocsIndexMissing(t *testing.T) {
	opts, repoDir, brainDir := docsAvailabilityFixture(t)

	healthy, err := buildAvailableBrainStatusReport(context.Background(), opts, repoDir)
	if err != nil {
		t.Fatalf("healthy status: %v", err)
	}
	if healthy.Retrieval == nil || healthy.Retrieval.Freshness == nil {
		t.Fatalf("healthy status has no retrieval freshness: %+v", healthy.Retrieval)
	}
	if got := healthy.Retrieval.Freshness.Axes["docs"].State; got != "ok" {
		t.Fatalf("healthy docs axis = %q, want ok (%+v)", got, healthy.Retrieval.Freshness.Axes["docs"])
	}
	if got := healthy.Retrieval.Freshness.Severity; got != "ok" {
		t.Fatalf("healthy retrieval severity = %q, want ok", got)
	}

	if err := os.Remove(filepath.Join(brainDir, filepath.FromSlash(docIndexPath))); err != nil {
		t.Fatal(err)
	}

	// The docs layer is now dead: prove retrieval really cannot answer from it
	// before asserting what status must say about it.
	//
	// This step used to assert that `search` SUCCEEDED and returned nothing,
	// which is what the docs arm did at the time: loadDocIndex's os.IsNotExist
	// was skipped rather than raised. That silent skip is the defect this test's
	// own comment describes, and it is now fixed — a doc index the manifest
	// DECLARES and the store does not hold is refused by name. The assertion
	// that matters here is unchanged (the removed record must not come back);
	// only the shape of "cannot answer" moved from an empty result to an error.
	out, err := execute(t, NewRootCommand(opts), "search", "postgres")
	if err == nil {
		t.Fatalf("search answered over a docs index that is gone:\n%s", out)
	}
	if strings.Contains(out, "doc:doc-1") {
		t.Fatalf("search still returned the doc record after the index was removed:\n%s", out)
	}
	if !strings.Contains(err.Error(), docIndexPath) {
		t.Fatalf("search does not name the absent index: %v", err)
	}

	degraded, err := buildAvailableBrainStatusReport(context.Background(), opts, repoDir)
	if err != nil {
		t.Fatalf("degraded status: %v", err)
	}
	if degraded.Retrieval == nil || degraded.Retrieval.Freshness == nil {
		t.Fatalf("degraded status has no retrieval freshness: %+v", degraded.Retrieval)
	}
	axis := degraded.Retrieval.Freshness.Axes["docs"]
	if axis.State == "ok" {
		t.Fatalf("docs axis still reports ok while %s is absent: %+v", docIndexPath, axis)
	}
	if axis.State != "missing" {
		t.Fatalf("docs axis state = %q, want missing: %+v", axis.State, axis)
	}
	if !strings.Contains(axis.Detail, docIndexPath) {
		t.Fatalf("docs axis detail does not name the absent index: %q", axis.Detail)
	}
	if got := degraded.Retrieval.Freshness.Severity; got == "ok" {
		t.Fatalf("retrieval freshness severity = %q while the docs layer is dead", got)
	}

	// The CLI surface must carry the same signal as the in-process report.
	statusOut, err := execute(t, NewRootCommand(opts), "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, statusOut)
	}
	if strings.Contains(statusOut, "docs: ok") {
		t.Fatalf("status still prints `docs: ok` with the index absent:\n%s", statusOut)
	}
}

// TestDoctorReportsDeclaredDocsIndexMissing pins the second required surface:
// doctor must not pass while a declared source is silently dead.
func TestDoctorReportsDeclaredDocsIndexMissing(t *testing.T) {
	opts, repoDir, brainDir := docsAvailabilityFixture(t)

	checks, _ := brainDoctorReadOnlyReport(context.Background(), opts, repoDir)
	if state, ok := doctorCheckState(checks, "docs_index"); !ok || state != "ok" {
		t.Fatalf("healthy doctor docs_index = %q present=%v, want ok", state, ok)
	}

	if err := os.Remove(filepath.Join(brainDir, filepath.FromSlash(docIndexPath))); err != nil {
		t.Fatal(err)
	}
	checks, _ = brainDoctorReadOnlyReport(context.Background(), opts, repoDir)
	state, ok := doctorCheckState(checks, "docs_index")
	if !ok {
		t.Fatalf("doctor has no docs_index check with the index absent: %+v", checks)
	}
	if state == "ok" {
		t.Fatalf("doctor docs_index still ok with %s absent", docIndexPath)
	}
}

func doctorCheckState(checks []doctorCheckResult, name string) (string, bool) {
	for _, check := range checks {
		if check.Name == name {
			return check.State, true
		}
	}
	return "", false
}
