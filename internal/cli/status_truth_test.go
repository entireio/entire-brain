package cli

import (
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// status_truth_test.go pins the one promise `status` exists to keep: the word
// "healthy" means the brain can be read. Two states broke it, and they are the
// two states in which nothing was ever built, so every counter the verdict
// summed read zero:
//
//   - no brain at all (a repo that was never onboarded, or one just reset), and
//   - a brain whose manifest declares no semantic index.
//
// A third state -- a semantic SQLite store shredded under a manifest that still
// describes it -- was reported as unhealthy but not usefully: the verdict was
// the single word "unsafe" plus a pointer to `doctor`, which rebuilds nothing
// and printed strictly less than `status --verbose` already did.

// statusTruthFixture wires an isolated brain over a fake git repo. Nothing is
// built: each test below decides how much of a brain to create.
//
// The live-state git calls are scripted to a clean worktree on purpose. The
// verdict under test is a SUM, so an unscripted command would add a warning,
// the sum would be nonzero, and the fixture would stop being able to reproduce
// the "+ healthy" it exists to pin.
func statusTruthFixture(t *testing.T) (Options, string, string) {
	t.Helper()
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	env.RepoRoot = repoDir
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	runner.responses[fakeCommandKey("git", "status", "--porcelain", "--untracked-files=all")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("git", "diff", "--shortstat", "HEAD")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("git", "diff", "--name-status", "-M", "-C", "HEAD")] = fakeCommandResponse{}
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return now }}
	storage, err := repoStoragePaths(context.Background(), runner, env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	return opts, repoDir, storage.BrainDir
}

// TestStatusOnAnUnbuiltBrainSaysSoAndNamesSetup is the first command a new user
// runs. It used to print "manifest absent", five unbuilt sources, and then
// `+ healthy` -- the one answer that is both wrong and terminal, because it
// names nothing to do next.
func TestStatusOnAnUnbuiltBrainSaysSoAndNamesSetup(t *testing.T) {
	opts, _, brainDir := statusTruthFixture(t)
	if _, err := os.Stat(filepath.Join(brainDir, exportManifestFileName)); !os.IsNotExist(err) {
		t.Fatalf("fixture is not an unbuilt brain: stat manifest = %v", err)
	}

	out, err := execute(t, NewRootCommand(opts), "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	if strings.Contains(out, "healthy") {
		t.Fatalf("status called a repository with no brain healthy:\n%s", out)
	}
	if !strings.Contains(out, "no brain for this repo yet") {
		t.Fatalf("status does not say plainly that there is no brain:\n%s", out)
	}
	if !strings.Contains(out, "setup`") {
		t.Fatalf("status does not name `setup`, the command that builds a brain:\n%s", out)
	}
}

// TestStatusAfterAManifestlessResetStillNamesSetup covers the same state
// reached the other way: `reset --force` removes the manifest, and status has to
// read that as "no brain" rather than as a clean bill of health.
func TestStatusAfterAManifestlessResetStillNamesSetup(t *testing.T) {
	opts, repoDir, brainDir := statusTruthFixture(t)
	if err := runSemanticIndex(context.Background(), &cobra.Command{Use: "index"}, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	built, err := execute(t, NewRootCommand(opts), "status")
	if err != nil {
		t.Fatalf("status on a built brain: %v\n%s", err, built)
	}
	if strings.Contains(built, "no brain for this repo yet") {
		t.Fatalf("a built brain must not be reported as absent:\n%s", built)
	}

	if err := os.Remove(filepath.Join(brainDir, exportManifestFileName)); err != nil {
		t.Fatal(err)
	}
	out, err := execute(t, NewRootCommand(opts), "status")
	if err != nil {
		t.Fatalf("status after manifest removal: %v\n%s", err, out)
	}
	if strings.Contains(out, "healthy") {
		t.Fatalf("status called a reset brain healthy:\n%s", out)
	}
	if !strings.Contains(out, "setup`") {
		t.Fatalf("status does not name `setup` after the manifest is gone:\n%s", out)
	}
}

// TestStatusWithNoSemanticIndexIsNotHealthy is the same hole one step along: a
// manifest exists, so "absent" no longer applies, but nothing declares a
// semantic index. `inspect code` fails outright in this state; status reported
// `+ healthy`, because with no semantic source there is no freshness report,
// and an empty severity was read as "ok".
func TestStatusWithNoSemanticIndexIsNotHealthy(t *testing.T) {
	opts, repoDir, brainDir := statusTruthFixture(t)
	now := opts.Now()
	if err := os.MkdirAll(brainDir, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		GeneratedAt:   now,
		Sources: &brainSources{
			Sessions: &sessionSourceManifest{GeneratedAt: now},
		},
	}
	if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
		t.Fatal(err)
	}

	report, err := buildAvailableBrainStatusReport(context.Background(), opts, repoDir)
	if err != nil {
		t.Fatalf("status report: %v", err)
	}
	if report.Semantic != nil {
		t.Fatalf("fixture unexpectedly has a semantic source: %+v", report.Semantic)
	}
	if buildStatusHealth(report).Healthy() {
		t.Fatal("a brain with no semantic index is not healthy: inspect/query/brief have nothing to read")
	}

	out, err := execute(t, NewRootCommand(opts), "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	if strings.Contains(out, "healthy") {
		t.Fatalf("status called a brain with no semantic index healthy:\n%s", out)
	}
	if !strings.Contains(out, "no semantic index") {
		t.Fatalf("status does not name the missing semantic index:\n%s", out)
	}
	if !strings.Contains(out, "refresh") {
		t.Fatalf("status does not name the command that builds the index:\n%s", out)
	}
}

// TestStatusNamesTheCorruptStoreAndTheCommandThatRebuildsIt is the third state.
// The signal already existed -- semanticStaleReport runs PRAGMA integrity_check
// on the semantic SQLite store and records store=unsafe with the sqlite error,
// which is exactly what `overview` prints -- and the short status report threw
// it away, leaving one word and a pointer to a command that re-reports rather
// than repairs.
func TestStatusNamesTheCorruptStoreAndTheCommandThatRebuildsIt(t *testing.T) {
	opts, repoDir, brainDir := statusTruthFixture(t)
	if err := runSemanticIndex(context.Background(), &cobra.Command{Use: "index"}, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Sources == nil || manifest.Sources.Semantic == nil || manifest.Sources.Semantic.StorePath == "" {
		t.Fatalf("fixture has no semantic store to corrupt: %+v", manifest.Sources)
	}
	shredSemanticStore(t, filepath.Join(brainDir, filepath.FromSlash(manifest.Sources.Semantic.StorePath)))

	// `overview` is the reference reader: it already reports the corruption in
	// words, and status must not be quieter about the same brain.
	overview, err := execute(t, NewRootCommand(opts), "overview")
	if err != nil {
		t.Fatalf("overview: %v\n%s", err, overview)
	}
	if !strings.Contains(overview, "store=unsafe") {
		t.Fatalf("the reference signal is missing from overview; the fixture did not corrupt the store:\n%s", overview)
	}

	out, err := execute(t, NewRootCommand(opts), "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	if strings.Contains(out, "healthy") {
		t.Fatalf("status called a brain with an unreadable semantic store healthy:\n%s", out)
	}
	if !strings.Contains(out, "store=unsafe") {
		t.Fatalf("status does not name the store as the failing axis, which `overview` already does:\n%s", out)
	}
	if !strings.Contains(out, "refresh --agent none`") {
		t.Fatalf("status does not name the command that rebuilds the store:\n%s", out)
	}
	if strings.Contains(out, "doctor`") {
		t.Fatalf("status sent the reader to `doctor`, which reports the problem again instead of fixing it:\n%s", out)
	}
}

// TestTheInstantLineDoesNotCallAnUnreadableSemanticIndexBuilt covers the last
// line of the default report that still called a shredded store fine. The
// verdict and its cause were fixed to name `store=unsafe`, but the onboarding
// block two lines above kept printing `+ semantic`, because the component marks
// answer from manifest PRESENCE and a shredded file is still present. One
// screen said the index built and the next said it cannot be opened.
//
// The control half matters as much as the failing half: the mark has to go back
// to `+` on a store that opens, or the fix is just a second constant.
func TestTheInstantLineDoesNotCallAnUnreadableSemanticIndexBuilt(t *testing.T) {
	opts, repoDir, brainDir := statusTruthFixture(t)
	if err := runSemanticIndex(context.Background(), &cobra.Command{Use: "index"}, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	storePath := filepath.Join(brainDir, filepath.FromSlash(manifest.Sources.Semantic.StorePath))

	healthy, err := execute(t, NewRootCommand(opts), "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, healthy)
	}
	healthyInstant := statusInstantLine(t, healthy)
	if !strings.Contains(healthyInstant, "+ semantic") {
		t.Fatalf("the fixture never reported a built semantic index, so the failing case proves nothing:\n%s", healthy)
	}

	shredSemanticStore(t, storePath)

	out, err := execute(t, NewRootCommand(opts), "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	instant := statusInstantLine(t, out)
	if strings.Contains(instant, "+ semantic") {
		t.Fatalf("the instant line still marks an unreadable semantic index as built:\n%s", out)
	}
	if !strings.Contains(instant, "x semantic") {
		t.Fatalf("the instant line does not mark the unreadable semantic index as failed:\n%s", out)
	}
	// Every other source keeps the mark it had. A report that fails everything
	// the moment one axis trips is no more usable than one that passes
	// everything, so the semantic mark must be the whole difference.
	if restored := strings.Replace(instant, "x semantic", "+ semantic", 1); restored != healthyInstant {
		t.Fatalf("a corrupt semantic store changed a mark other than semantic:\n before: %s\n  after: %s", healthyInstant, instant)
	}
}

// statusInstantLine returns the onboarding block's `instant` line, which is the
// only line in the default report carrying the per-source marks.
func statusInstantLine(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "instant") {
			return line
		}
	}
	t.Fatalf("status printed no instant line:\n%s", out)
	return ""
}

// TestDoctorNamesTheFailingSemanticAxis closes the other end of the dead end:
// whatever still sends a reader to doctor must find more there than it left
// behind. Doctor used to answer a shredded SQLite store with `semantic: warn
// (unsafe)` -- one word, and one word less than `status --verbose`.
func TestDoctorNamesTheFailingSemanticAxis(t *testing.T) {
	opts, repoDir, brainDir := statusTruthFixture(t)
	if err := runSemanticIndex(context.Background(), &cobra.Command{Use: "index"}, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	shredSemanticStore(t, filepath.Join(brainDir, filepath.FromSlash(manifest.Sources.Semantic.StorePath)))

	out, err := execute(t, NewRootCommand(opts), "doctor")
	if err != nil {
		t.Fatalf("doctor: %v\n%s", err, out)
	}
	if strings.Contains(out, "semantic: warn (unsafe)\n") {
		t.Fatalf("doctor still answers a corrupt store with one word:\n%s", out)
	}
	if !strings.Contains(out, "store=unsafe") {
		t.Fatalf("doctor does not name the failing axis:\n%s", out)
	}
}

// TestStatusHealthRemedyNamesTheFix pins the mapping from "what is wrong" to
// "what fixes it" directly, because the defect was never the detection: it was
// that every verdict ended on the same command, and that command repairs
// nothing.
func TestStatusHealthRemedyNamesTheFix(t *testing.T) {
	t.Parallel()
	for name, testCase := range map[string]struct {
		health statusHealth
		want   string
	}{
		"no brain":           {health: statusHealth{BrainMissing: true}, want: "setup"},
		"no semantic index":  {health: statusHealth{SemanticMissing: true}, want: "refresh --agent none"},
		"unsafe freshness":   {health: statusHealth{Severity: "unsafe"}, want: "refresh --agent none"},
		"degraded freshness": {health: statusHealth{Severity: "degraded"}, want: "refresh --agent none"},
		"a health issue":     {health: statusHealth{Issues: 1}, want: "doctor"},
	} {
		if got := testCase.health.Remedy(); got != testCase.want {
			t.Errorf("%s: remedy = %q, want %q", name, got, testCase.want)
		}
	}
}

// TestStatusHealthUnbuiltStatesAreNeverHealthy is the unit-level guard on the
// counting bug: zero issues, zero warnings, zero failed components and an empty
// severity is what "nothing was ever built" looks like, and it is not health.
func TestStatusHealthUnbuiltStatesAreNeverHealthy(t *testing.T) {
	t.Parallel()
	if (statusHealth{BrainMissing: true}).Healthy() {
		t.Error("an absent brain counted as healthy")
	}
	if (statusHealth{SemanticMissing: true}).Healthy() {
		t.Error("an absent semantic index counted as healthy")
	}
	if !(statusHealth{Severity: "ok"}).Healthy() {
		t.Error("a built, current brain must still be healthy")
	}
}

// TestStatusFreshnessCauseUsesTheOverviewSummary proves status reads the SAME
// signal `overview` does rather than a second mechanism of its own.
func TestStatusFreshnessCauseUsesTheOverviewSummary(t *testing.T) {
	t.Parallel()
	freshness := staleReport{Severity: "unsafe", Axes: map[string]staleAxis{
		"head":  {State: "ok"},
		"store": {State: "unsafe", Detail: "validate semantic sqlite integrity: file is not a database (26)"},
	}}
	report := brainStatusReport{Semantic: &brainStatusSemantic{Freshness: &freshness}}
	want := freshnessSummary(freshness)
	if got := statusFreshnessCause(report); got != want {
		t.Fatalf("status cause = %q, want the overview summary %q", got, want)
	}
	if !strings.Contains(want, "store=unsafe") {
		t.Fatalf("the overview summary no longer names the store axis: %q", want)
	}
}

// shredSemanticStore overwrites the head of a SQLite file with random bytes,
// which is what a torn write or a half-synced copy leaves behind: the file is
// still there, still the right size, and no longer a database.
func shredSemanticStore(t *testing.T, path string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open semantic store: %v", err)
	}
	defer file.Close()
	noise := make([]byte, 200)
	if _, err := rand.Read(noise); err != nil {
		t.Fatalf("random bytes: %v", err)
	}
	if _, err := file.WriteAt(noise, 0); err != nil {
		t.Fatalf("shred semantic store: %v", err)
	}
}
