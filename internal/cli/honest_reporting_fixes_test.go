package cli

import (
	"bytes"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
	"github.com/entireio/entire-brain/internal/cli/internal/semstore"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// --- defect 2: privacy exclude reported work it never did ------------------

// privacyCommandFixture is factsAvailabilityFixture plus the one thing the
// privacy commands need that it does not build: a sessions source in the
// manifest, so the exclusion path can rebuild the (empty) history projection.
func privacyCommandFixture(t *testing.T) (Options, string) {
	t.Helper()
	opts, _, brainDir := factsAvailabilityFixture(t)
	manifest, err := bmanifest.Load(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Sources == nil {
		manifest.Sources = &brainSources{}
	}
	manifest.RepoKey = "gh/example/repo"
	manifest.DefaultBranch = "feature"
	manifest.Sources.Sessions = &sessionSourceManifest{
		GeneratedAt: opts.Now(), DefaultBranch: "feature", TranscriptMode: "full", Scope: "repo",
	}
	if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
		t.Fatal(err)
	}
	// The exported sessions root must exist for the history projection to be
	// buildable at all; it is legitimately empty here.
	if err := os.MkdirAll(filepath.Join(brainDir, brainstore.SessionsDirName, "feature"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := writeBrainHistoryIndexAndSource(brainDir, opts.Now(), nil); err != nil {
		t.Fatal(err)
	}
	return opts, brainDir
}

// --- defect 3: doctor called an unreadable store a warning -----------------

func doctorFindingsByName(findings []doctorCheckResult) map[string]doctorCheckResult {
	byName := map[string]doctorCheckResult{}
	for _, finding := range findings {
		byName[finding.Name] = finding
	}
	return byName
}

// TestDoctorDoesNotFailOnDegradedProvider is the constraint that made #239
// leave this check alone: the provider axis fires on any machine without the
// host CLI, and failing a gate on it would break CI everywhere.
func TestDoctorDoesNotFailOnDegradedProvider(t *testing.T) {
	report := staleReport{Severity: "degraded", Axes: map[string]staleAxis{
		"snapshot":              {State: "ok"},
		"store":                 {State: "ok"},
		"head":                  {State: "ok"},
		"worktree":              {State: "clean"},
		"provider":              {State: "degraded", Detail: `read graph identity revision: exec: "entire": executable file not found in $PATH`},
		"semantic_completeness": {State: "ok"},
	}}
	findings := semanticDoctorFindings(report)
	byName := doctorFindingsByName(findings)
	provider, ok := byName["semantic provider"]
	if !ok {
		t.Fatalf("doctor dropped the provider finding: %+v", findings)
	}
	if provider.scope() != doctorScopeEnvironment {
		t.Fatalf("semantic provider scope = %q, want %q", provider.scope(), doctorScopeEnvironment)
	}
	if provider.State != "warn" {
		t.Fatalf("semantic provider state = %q, want the finding kept at warn severity", provider.State)
	}
	if got := byName["semantic"]; got.State != "ok" {
		t.Fatalf("a degraded provider must not make the BRAIN finding %q: %+v", got.State, got)
	}
	for _, failOn := range []string{doctorFailOnError, doctorFailOnWarn} {
		if err := doctorGateFailure(doctorReport{Checks: findings}, failOn); err != nil {
			t.Fatalf("--fail-on %s must not fire on a host-environment finding: %v", failOn, err)
		}
	}
}

// TestDoctorKeepsSemanticStalenessAtWarn keeps the promotion narrow: an index
// that is merely behind HEAD is not corruption and must not fail the gate.
func TestDoctorKeepsSemanticStalenessAtWarn(t *testing.T) {
	report := staleReport{Severity: "stale", Axes: map[string]staleAxis{
		"snapshot": {State: "ok"},
		"store":    {State: "ok"},
		"head":     {State: "stale", Current: "bbb", Indexed: "aaa"},
		"worktree": {State: "clean"},
		"provider": {State: "ok", Detail: "entire-graph 1.2.3"},
	}}
	findings := semanticDoctorFindings(report)
	if got := doctorFindingsByName(findings)["semantic"]; got.State != "warn" {
		t.Fatalf("stale semantic index state = %q, want warn: %+v", got.State, got)
	}
	if err := doctorGateFailure(doctorReport{Checks: findings}, doctorFailOnError); err != nil {
		t.Fatalf("--fail-on error must not fire on a merely stale index: %v", err)
	}
}

// TestDoctorKeepsNeverBuiltSemanticIndexAtWarn is the other side of the
// narrowness constraint, and the one two existing tests caught: a brain that
// has simply not run `refresh index` reports semantic=missing at severity
// unsafe, and calling that an error would fail the default gate on every fresh
// brain.
func TestDoctorKeepsNeverBuiltSemanticIndexAtWarn(t *testing.T) {
	report := staleReport{Severity: "unsafe", Axes: map[string]staleAxis{
		"semantic": {State: "missing", Detail: "no semantic source in manifest"},
	}}
	findings := semanticDoctorFindings(report)
	if got := doctorFindingsByName(findings)["semantic"]; got.State != "warn" {
		t.Fatalf("never-built semantic index state = %q, want warn: %+v", got.State, got)
	}
	if err := doctorGateFailure(doctorReport{Checks: findings}, doctorFailOnError); err != nil {
		t.Fatalf("--fail-on error must not fire on a brain that has not been indexed yet: %v", err)
	}
}

// --- defect 4b: refresh index exited 1 when there was nothing to do --------

// TestRefreshIndexIsIdempotent is the scripted-loop defect: `refresh index`
// failed on every run after the first, so any refresh sequence containing the
// documented step could be green exactly once.
func TestRefreshIndexIsIdempotent(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot(semstore.SchemaVersion))
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	for attempt := 1; attempt <= 3; attempt++ {
		cmd := &cobra.Command{Use: "index"}
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
			t.Fatalf("refresh index attempt %d failed: %v\n%s", attempt, err, out.String())
		}
		if attempt > 1 && !strings.Contains(out.String(), "already current") {
			t.Fatalf("attempt %d did not report the no-op:\n%s", attempt, out.String())
		}
	}
}

// TestRefreshIndexRebuildsUnreadableStore keeps the no-op narrow: a recorded
// index whose SQLite store no longer opens matches HEAD but is not current in
// any useful sense, and refresh index is the command that repairs it.
func TestRefreshIndexRebuildsUnreadableStore(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot(semstore.SchemaVersion))
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	cmd := &cobra.Command{Use: "index"}
	cmd.SetOut(&bytes.Buffer{})
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	d := runner.graphStoreFixtures[repoDir+"\x00full"]
	if err := os.WriteFile(d.Path, []byte("this is not a database"), 0600); err != nil {
		t.Fatal(err)
	}
	if db, err := semstore.OpenReadDB(semstore.GraphStoreSource(d, "").StorePath); err == nil {
		db.Close()
		t.Fatal("corrupt Graph store was readable")
	}

	rebuild := &cobra.Command{Use: "index"}
	rebuild.SetOut(&bytes.Buffer{})
	if err := runSemanticIndex(rebuild.Context(), rebuild, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("refresh index over an unreadable store must rebuild: %v", err)
	}
	d = runner.graphStoreFixtures[repoDir+"\x00full"]
	db, err := semstore.OpenReadDB(semstore.GraphStoreSource(d, "").StorePath)
	if err != nil {
		t.Fatalf("Graph store was not rebuilt: %v", err)
	}
	db.Close()

}

// --- defect 4c: no --version flag ------------------------------------------

// TestRootCommandExposesVersionFlag pins the missing convention: Options.Version
// was set but the cobra.Command's own Version field never was, so
// InitDefaultVersionFlag no-opped and --version was "unknown flag".
func TestRootCommandExposesVersionFlag(t *testing.T) {
	for _, flag := range []string{"--version", "-v"} {
		stdout, stderr, err := executeSplit(t, NewRootCommand(Options{Version: "0.3.0"}), flag)
		if err != nil {
			t.Fatalf("%s: %v\n%s%s", flag, err, stdout, stderr)
		}
		if strings.TrimSpace(stdout) != "0.3.0" {
			t.Fatalf("%s printed %q, want the same string `entire-brain version` reports", flag, stdout)
		}
	}
	// One binary, one answer.
	subcommandOut, _, err := executeSplit(t, NewRootCommand(Options{Version: "0.3.0"}), "version")
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if strings.TrimSpace(subcommandOut) != "0.3.0" {
		t.Fatalf("version subcommand printed %q", subcommandOut)
	}
}
