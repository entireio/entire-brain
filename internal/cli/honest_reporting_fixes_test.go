package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// --- defect 2: privacy exclude reported work it never did ------------------

// TestExcludeDoesNotClaimCleanupItNeverDid pins the sentence. Excluding an id
// no session answers to is a legitimate pre-emptive privacy operation and stays
// permitted and zero-exit; what was wrong was reporting it as "removed or
// rebuilt its derived projections" when nothing had been derived.
func TestExcludeDoesNotClaimCleanupItNeverDid(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	phantom := "session:totally-made-up-12345"
	plan, err := buildSessionPurgePlan(brainDir, phantom)
	if err != nil {
		t.Fatal(err)
	}
	if sessionExclusionMatchedSomething(manifest, plan, phantom) {
		t.Fatalf("an id no captured session answers to must not report as matched; plan=%+v", plan)
	}
	// A session this brain really captured is matched, so the honest report
	// does not become a blanket "nothing happened" either.
	realPlan, err := buildSessionPurgePlan(brainDir, "secret-sess")
	if err != nil {
		t.Fatal(err)
	}
	if !sessionExclusionMatchedSomething(manifest, realPlan, "secret-sess") {
		t.Fatalf("a captured session must report as matched; plan=%+v", realPlan)
	}
}

// privacyCommandFixture is factsAvailabilityFixture plus the one thing the
// privacy commands need that it does not build: a sessions source in the
// manifest, so the exclusion path can rebuild the (empty) history projection.
func privacyCommandFixture(t *testing.T) (Options, string) {
	t.Helper()
	opts, _, brainDir := factsAvailabilityFixture(t)
	manifest, err := loadBrainManifest(brainDir)
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
	if err := os.MkdirAll(filepath.Join(brainDir, exportSessionsDirectory, "feature"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := writeBrainHistoryIndexAndSource(brainDir, opts.Now(), nil); err != nil {
		t.Fatal(err)
	}
	return opts, brainDir
}

// TestExcludePhantomReportsTombstoneOnly drives the command surface: the exit
// code, the human sentence, and the machine field a caller can branch on.
func TestExcludePhantomReportsTombstoneOnly(t *testing.T) {
	opts, _ := privacyCommandFixture(t)
	phantom := "session:totally-made-up-12345"

	cmd := &cobra.Command{Use: "exclude"}
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := runSessionsExclude(cmd.Context(), cmd, opts, phantom, "", false); err != nil {
		t.Fatalf("pre-emptive exclusion must stay permitted: %v", err)
	}
	text := out.String()
	if strings.Contains(text, "removed or rebuilt its derived projections") {
		t.Fatalf("exclude claimed cleanup for a session that produced nothing:\n%s", text)
	}
	if !strings.Contains(text, "nothing to remove or rebuild") || !strings.Contains(text, "no captured session in this brain matches this id") {
		t.Fatalf("exclude did not say the tombstone matched nothing:\n%s", text)
	}

	jsonCmd := &cobra.Command{Use: "exclude"}
	var jsonOut bytes.Buffer
	jsonCmd.SetOut(&jsonOut)
	if err := runSessionsExclude(jsonCmd.Context(), jsonCmd, opts, "session:another-phantom", "", true); err != nil {
		t.Fatalf("exclude --json: %v", err)
	}
	var payload struct {
		Excluded string `json:"excluded"`
		Matched  bool   `json:"matched"`
	}
	if err := json.Unmarshal(jsonOut.Bytes(), &payload); err != nil {
		t.Fatalf("parse exclude --json: %v\n%s", err, jsonOut.String())
	}
	if payload.Excluded != "session:another-phantom" || payload.Matched {
		t.Fatalf("exclude --json must report matched=false for a phantom: %+v", payload)
	}
}

// TestPrivacyListMarksUnmatchedTombstones pins the other half: the list is the
// only place a user can notice that the id they typed matched nothing, and it
// rendered a phantom exactly like a completed exclusion, forever.
func TestPrivacyListMarksUnmatchedTombstones(t *testing.T) {
	opts, _ := privacyCommandFixture(t)
	excludeCmd := &cobra.Command{Use: "exclude"}
	excludeCmd.SetOut(&bytes.Buffer{})
	if err := runSessionsExclude(excludeCmd.Context(), excludeCmd, opts, "session:totally-made-up-12345", "", false); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := executeSplit(t, newSessionsListCommand(opts))
	if err != nil {
		t.Fatalf("privacy list: %v\n%s", err, stdout)
	}
	if !strings.Contains(stdout, "UNMATCHED") {
		t.Fatalf("privacy list rendered a phantom tombstone as a completed exclusion:\n%s", stdout)
	}

	jsonOut, _, err := executeSplit(t, newSessionsListCommand(opts), "--json")
	if err != nil {
		t.Fatalf("privacy list --json: %v\n%s", err, jsonOut)
	}
	var payload struct {
		Sessions []sessionListEntry `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &payload); err != nil {
		t.Fatalf("parse privacy list --json: %v\n%s", err, jsonOut)
	}
	found := false
	for _, entry := range payload.Sessions {
		if entry.SessionID == "session:totally-made-up-12345" {
			found = true
			if !entry.Unmatched {
				t.Fatalf("privacy list --json did not mark the phantom tombstone: %+v", entry)
			}
		}
	}
	if !found {
		t.Fatalf("privacy list --json dropped the tombstone entirely: %+v", payload.Sessions)
	}
}

// --- defect 3: doctor called an unreadable store a warning -----------------

func doctorFindingsByName(findings []doctorCheckResult) map[string]doctorCheckResult {
	byName := map[string]doctorCheckResult{}
	for _, finding := range findings {
		byName[finding.Name] = finding
	}
	return byName
}

// TestDoctorFailsOnUnreadableSemanticStore is the gate defect. "file is not a
// database" is the strongest broken signal this tool has, and it was reported
// as a warning, so the default `--fail-on error` gate exited 0 on a corrupt
// brain.
func TestDoctorFailsOnUnreadableSemanticStore(t *testing.T) {
	report := staleReport{Severity: "unsafe", Axes: map[string]staleAxis{
		"snapshot":              {State: "ok", Detail: "semantic/snapshots/a/snapshot.ndjson"},
		"store":                 {State: "unsafe", Detail: "validate semantic sqlite integrity: file is not a database (26)"},
		"head":                  {State: "ok"},
		"branch_tip":            {State: "ok"},
		"worktree":              {State: "clean"},
		"provider":              {State: "ok", Detail: "entire-graph 1.2.3"},
		"semantic_completeness": {State: "ok"},
	}}
	findings := semanticDoctorFindings(report)
	byName := doctorFindingsByName(findings)
	semantic, ok := byName["semantic"]
	if !ok {
		t.Fatalf("doctor dropped the semantic finding: %+v", findings)
	}
	if semantic.State != "error" {
		t.Fatalf("semantic state = %q for an unreadable store, want error: %+v", semantic.State, semantic)
	}
	if semantic.scope() != doctorScopeBrain {
		t.Fatalf("semantic scope = %q, want %q", semantic.scope(), doctorScopeBrain)
	}
	if !strings.Contains(semantic.Detail, "file is not a database") {
		t.Fatalf("semantic detail lost the reason: %q", semantic.Detail)
	}
	if err := doctorGateFailure(doctorReport{Checks: findings}, doctorFailOnError); err == nil {
		t.Fatalf("the default --fail-on error gate must fire on an unreadable semantic store")
	}
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

// --- defect 4a: facts proposals --json printed help text -------------------

// TestFactsProposalsJSONDoesNotPrintHelp pins the one --json violation out of
// the 92 nodes that advertise the flag: the group node printed human help to
// stdout and exited 0, so a machine caller got prose labelled as success.
func TestFactsProposalsJSONDoesNotPrintHelp(t *testing.T) {
	opts := Options{Version: "test"}
	stdout, stderr, err := executeSplit(t, NewRootCommand(opts), "facts", "proposals", "--json")
	if err == nil {
		t.Fatalf("facts proposals --json must not exit 0 with no result\nstdout:\n%s", stdout)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Fatalf("facts proposals --json wrote non-JSON to stdout:\n%s", stdout)
	}
	var envelope commandJSONError
	if jsonErr := json.Unmarshal([]byte(stderr), &envelope); jsonErr != nil {
		t.Fatalf("facts proposals --json did not emit a JSON error envelope: %v\n%s", jsonErr, stderr)
	}
	if !strings.Contains(envelope.Message, "requires a subcommand") {
		t.Fatalf("JSON error does not say what is missing: %+v", envelope)
	}
	for _, verb := range []string{"list", "show", "apply", "reject", "repair"} {
		if !strings.Contains(envelope.Message, verb) {
			t.Fatalf("JSON error does not name the %q verb: %+v", verb, envelope)
		}
	}

	// Without --json the human help is unchanged and still exits 0.
	humanOut, _, humanErr := executeSplit(t, NewRootCommand(opts), "facts", "proposals")
	if humanErr != nil {
		t.Fatalf("facts proposals (human) must still exit 0: %v", humanErr)
	}
	if !strings.Contains(humanOut, "Usage:") {
		t.Fatalf("facts proposals (human) stopped printing help:\n%s", humanOut)
	}
}

// --- defect 4b: refresh index exited 1 when there was nothing to do --------

// TestRefreshIndexIsIdempotent is the scripted-loop defect: `refresh index`
// failed on every run after the first, so any refresh sequence containing the
// documented step could be green exactly once.
func TestRefreshIndexIsIdempotent(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot(semanticSchemaVersion))
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
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot(semanticSchemaVersion))
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	cmd := &cobra.Command{Use: "index"}
	cmd.SetOut(&bytes.Buffer{})
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := bundleTestBrainDir(env)
	source := mustSemanticSource(t, env)
	storePath := filepath.Join(brainDir, filepath.FromSlash(source.StorePath))
	if err := os.WriteFile(storePath, []byte("this is not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := semanticIndexAlreadyCurrent(brainDir, source, semanticIndexOptions{graphBinary: "entire"}, source.Commit, source.Tree, ""); ok {
		t.Fatalf("an index whose store is not a database must not be reported as current")
	}

	rebuild := &cobra.Command{Use: "index"}
	rebuild.SetOut(&bytes.Buffer{})
	if err := runSemanticIndex(rebuild.Context(), rebuild, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("refresh index over an unreadable store must rebuild: %v", err)
	}
	rebuilt := mustSemanticSource(t, env)
	if err := validateSemanticSQLiteStore(filepath.Join(brainDir, filepath.FromSlash(rebuilt.StorePath)), rebuilt.Symbols, rebuilt.Relations); err != nil {
		t.Fatalf("store was not rebuilt: %v", err)
	}
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
