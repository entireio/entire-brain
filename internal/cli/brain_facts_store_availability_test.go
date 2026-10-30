package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
)

// factsAvailabilityFixture builds a brain that really holds one authored fact
// on the checked-out branch, with the fact source manifest derived from disk.
func factsAvailabilityFixture(t *testing.T) (Options, string, string) {
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
	// "feature" is the branch semanticFixtureRunner reports as checked out, so
	// brief and recall both resolve to this store.
	facts := []factRecord{{
		ID:         "fact-1",
		Paths:      []string{"project.tooling.stack"},
		Text:       "the alpha subsystem stores checkpoints in postgres",
		Branch:     "feature",
		Origin:     factstore.OriginAuthored,
		Status:     factStatusActive,
		Provenance: []factAnchor{{SessionID: "session-1"}},
		CreatedAt:  now,
		UpdatedAt:  now,
	}}
	if err := factstore.Write(storage.BrainDir, "feature", facts); err != nil {
		t.Fatal(err)
	}
	if err := updateFactSourceManifest(storage.BrainDir, now); err != nil {
		t.Fatal(err)
	}
	return opts, repoDir, storage.BrainDir
}

func removeFactStore(t *testing.T, brainDir, branch string) {
	t.Helper()
	// Remove the authoritative snapshot as well as the compatibility view.
	// Removing only a projection no longer destroys the memory collection.
	if err := os.Remove(filepath.Join(brainDir, factstore.RepositoryPath)); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(brainDir, filepath.FromSlash(factstore.FileRelPath(branch)))); err != nil {
		t.Fatal(err)
	}
}

// TestStatusReportsDeclaredFactBranchWithMissingStore is the honesty contract:
// the manifest's fact counts are a claim about branches, not about files.
// factstore.Load reads an absent store as an empty branch, so when the store is gone
// status/brain_status keep reporting "facts: 1 across 1 branch(es)" while every
// recall surface returns nothing. The caller must be told.
func TestStatusReportsDeclaredFactBranchWithMissingStore(t *testing.T) {
	opts, repoDir, brainDir := factsAvailabilityFixture(t)

	healthy, err := buildAvailableBrainStatusReport(context.Background(), opts, repoDir)
	if err != nil {
		t.Fatalf("healthy status: %v", err)
	}
	if healthy.Facts == nil || healthy.Facts.Facts != 1 {
		t.Fatalf("healthy status facts = %+v, want 1 fact", healthy.Facts)
	}
	if got := statusFactsMissingBranches(t, healthy); len(got) != 0 {
		t.Fatalf("healthy status reports missing fact branches: %v", got)
	}

	removeFactStore(t, brainDir, "feature")

	// Prove the facts layer is really dead before asserting what status says.
	recallOut, err := execute(t, NewRootCommand(opts), "recall", "postgres", "--branch", "feature")
	if err != nil {
		t.Fatalf("recall after store removal: %v\n%s", err, recallOut)
	}
	if strings.Contains(recallOut, "fact-1") {
		t.Fatalf("recall still returned the fact after its store was removed:\n%s", recallOut)
	}

	degraded, err := buildAvailableBrainStatusReport(context.Background(), opts, repoDir)
	if err != nil {
		t.Fatalf("degraded status: %v", err)
	}
	if degraded.Facts == nil {
		t.Fatalf("degraded status dropped the facts section")
	}
	if got := statusFactsMissingBranches(t, degraded); len(got) != 1 || got[0] != "feature" {
		t.Fatalf("status facts.missing_branches = %v, want [feature]", got)
	}
	if !warningsMention(degraded.Warnings, "feature") {
		t.Fatalf("status warnings do not name the unreadable fact branch: %v", degraded.Warnings)
	}

	statusOut, err := execute(t, NewRootCommand(opts), "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, statusOut)
	}
	if !strings.Contains(statusOut, factstore.FileName) {
		t.Fatalf("status output does not report the missing fact store:\n%s", statusOut)
	}
}

// TestBrainBriefWarnsWhenFactStoreIsMissing pins the packet surface: a brief
// that contributes zero facts from a source it reports as present must say why.
func TestBrainBriefWarnsWhenFactStoreIsMissing(t *testing.T) {
	opts, _, brainDir := factsAvailabilityFixture(t)

	out, err := execute(t, NewRootCommand(opts), "brief", "postgres checkpoint storage", "--json")
	if err != nil {
		t.Fatalf("healthy brief: %v\n%s", err, out)
	}
	if !strings.Contains(out, "fact-1") {
		t.Fatalf("healthy brief did not carry the fact:\n%s", out)
	}

	removeFactStore(t, brainDir, "feature")

	degraded, err := execute(t, NewRootCommand(opts), "brief", "postgres checkpoint storage", "--json")
	if err != nil {
		t.Fatalf("degraded brief: %v\n%s", err, degraded)
	}
	if strings.Contains(degraded, "fact-1") {
		t.Fatalf("degraded brief still carried the fact:\n%s", degraded)
	}
	if !strings.Contains(degraded, factstore.FileName) {
		t.Fatalf("brief reported a present facts source that contributed nothing, with no warning:\n%s", degraded)
	}
}

// TestDoctorReportsDeclaredFactBranchWithMissingStore pins the second required
// surface: doctor must not pass while a declared source is silently dead.
func TestDoctorReportsDeclaredFactBranchWithMissingStore(t *testing.T) {
	opts, repoDir, brainDir := factsAvailabilityFixture(t)

	checks, _ := brainDoctorReadOnlyReport(context.Background(), opts, repoDir)
	state, ok := factsDoctorCheckState(checks)
	if !ok || state != "ok" {
		t.Fatalf("healthy doctor facts = %q present=%v, want ok", state, ok)
	}

	removeFactStore(t, brainDir, "feature")
	checks, _ = brainDoctorReadOnlyReport(context.Background(), opts, repoDir)
	state, ok = factsDoctorCheckState(checks)
	if !ok {
		t.Fatalf("doctor has no facts check with the store absent: %+v", checks)
	}
	if state == "ok" {
		t.Fatalf("doctor facts still ok with the declared branch store absent")
	}
}

// statusFactsMissingBranches reads the facts section straight out of the
// serialized status contract, which is what every JSON/MCP caller sees.
func statusFactsMissingBranches(t *testing.T, report brainStatusReport) []string {
	t.Helper()
	data, err := json.Marshal(report.Facts)
	if err != nil {
		t.Fatalf("marshal status facts: %v", err)
	}
	var payload struct {
		MissingBranches []string `json:"missing_branches"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("decode status facts: %v", err)
	}
	return payload.MissingBranches
}

func factsDoctorCheckState(checks []doctorCheckResult) (string, bool) {
	for _, check := range checks {
		if check.Name == "facts" {
			return check.State, true
		}
	}
	return "", false
}

func warningsMention(warnings []string, needle string) bool {
	for _, warning := range warnings {
		if strings.Contains(warning, needle) {
			return true
		}
	}
	return false
}
