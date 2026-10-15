package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// firstrun_no_sessions_test.go covers one defect with three faults, observed on
// a brand-new repository with a clean working tree and no finished agent
// session yet -- the state EVERY repo is in between `entire enable` and its
// first captured session:
//
//	$ entire-brain status
//	  instant   x sessions  + seed  + docs  ...
//	            x sessions: no_egress: a complete readable local checkpoint
//	            catalog could not be established; refusing routed checkpoint
//	            discovery
//	x sessions failed, 1 issue, freshness degraded -- run `entire-brain refresh --agent none`
//
//	$ entire-brain refresh --agent none          # the advice, run verbatim
//	refresh: export sessions: reading local checkpoint stores failed
//	no_egress: a complete readable local checkpoint catalog could not be established
//
//	$ entire-brain status                        # byte-identical advice again
//	x sessions failed, 1 issue, freshness degraded -- run `entire-brain refresh --agent none`
//
//  1. a normal state was classified as a failure;
//  2. the remedy re-ran the stage that produced the failure, so it could never
//     clear it, and the pair looped;
//  3. setupNoSessionsHint -- the sentence that explains exactly this state --
//     already existed and was selected on the `setup` path only.

// emptyCatalogRunner answers the two ref enumerations with a clean, empty
// result: both namespaces listed, neither holds anything. That is a repository
// with no captured sessions, NOT an unreadable one.
func emptyCatalogRunner(t *testing.T) *fakeCommandRunner {
	t.Helper()
	return &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "for-each-ref", "--format=%(refname)", v1MainRef, v1OriginRef): {},
		fakeCommandKey("git", "for-each-ref", "--format=%(refname)", checkpointRefPrefix):    {},
	}}
}

// TestCheckpointCatalogSeparatesEmptyFromUnreadable is fault 1 at its source.
// Zero checkpoints and unreadable checkpoints arrived at one `len(ids) == 0`
// and left as one refusal, so the brain could not tell a first run from a
// broken store -- and reported the first as the second.
func TestCheckpointCatalogSeparatesEmptyFromUnreadable(t *testing.T) {
	t.Parallel()

	refsUnlistable := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "for-each-ref", "--format=%(refname)", v1MainRef, v1OriginRef): {},
		fakeCommandKey("git", "for-each-ref", "--format=%(refname)", checkpointRefPrefix):    {err: errors.New("fatal: not a git repository")},
	}}
	refPresentButBroken := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "for-each-ref", "--format=%(refname)", v1MainRef, v1OriginRef): {stdout: v1MainRef + "\n"},
		fakeCommandKey("git", "for-each-ref", "--format=%(refname)", checkpointRefPrefix):    {},
		fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1MainRef):                     {err: errors.New("fatal: bad object")},
	}}

	for name, testCase := range map[string]struct {
		runner    *fakeCommandRunner
		wantEmpty bool
	}{
		"nothing has ever been captured":     {runner: emptyCatalogRunner(t), wantEmpty: true},
		"the ref namespace will not list":    {runner: refsUnlistable},
		"a ref is present and will not open": {runner: refPresentButBroken},
	} {
		_, _, err := loadLocalCheckpointUnionSnapshot(context.Background(), testCase.runner, t.TempDir(),
			checkpointBackendGitBranch, false, 0, checkpointBranchDestinations{}, nil, nil, true)
		if err == nil {
			t.Fatalf("%s: a snapshot was returned from an empty catalog", name)
		}
		if !errors.Is(err, errCheckpointSnapshotUnavailable) {
			t.Fatalf("%s: every one of these states is still 'no snapshot': %v", name, err)
		}
		if got := errors.Is(err, errCheckpointCatalogEmpty); got != testCase.wantEmpty {
			t.Errorf("%s: proven-empty = %v, want %v (%v)", name, got, testCase.wantEmpty, err)
		}
	}
}

// TestRefreshSucceedsOnARepoWithNoCapturedSessions is fault 1 end to end, in
// no-egress mode, which is where the refusal fires: the local catalog is the
// whole catalog there, so a clean empty enumeration PROVES emptiness and there
// is nothing for routed discovery to add.
func TestRefreshSucceedsOnARepoWithNoCapturedSessions(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_LOCAL_ONLY", "1")
	opts := noCapturedSessionsOptions(t)
	// Twice, because that is the shape of the live report: the FIRST refresh
	// into an empty brain degrades rather than aborting (there is no manifest
	// to protect), and only the second -- the one a reader runs after `setup`,
	// which is when the loop was observed -- is fatal.
	for _, pass := range []string{"first", "second"} {
		out, err := execute(t, NewRootCommand(opts), "refresh", "--agent", "none", "--semantic=false", "--entire-binary", "entire-test")
		if err != nil {
			t.Fatalf("%s refresh: a repository with no captured sessions is not a broken one:\n%v\n%s", pass, err, out)
		}
		for _, unwanted := range []string{"could not be established", "session export degraded"} {
			if strings.Contains(out, unwanted) {
				t.Fatalf("%s refresh reported an empty catalog as a fault (%q):\n%s", pass, unwanted, out)
			}
		}
	}
}

// TestRefreshStillFailsOnAnUnreadableCatalog is the other side, and it is the
// one that stops this fix from being a blindfold: a catalog that cannot be
// READ is a real fault and must stay loud.
func TestRefreshStillFailsOnAnUnreadableCatalog(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_LOCAL_ONLY", "1")
	opts := noCapturedSessionsOptions(t)
	runner := opts.Runner.(*fakeCommandRunner)
	runner.responses[fakeCommandKey("git", "for-each-ref", "--format=%(refname)", checkpointRefPrefix)] = fakeCommandResponse{err: errors.New("fatal: unable to read refs")}
	first, err := execute(t, NewRootCommand(opts), "refresh", "--agent", "none", "--semantic=false", "--entire-binary", "entire-test")
	if !strings.Contains(first, "session export degraded") {
		t.Fatalf("the first refresh must still report the fault even while it degrades:\n%s", first)
	}
	out, err := execute(t, NewRootCommand(opts), "refresh", "--agent", "none", "--semantic=false", "--entire-binary", "entire-test")
	if err == nil {
		t.Fatalf("an unreadable checkpoint catalog must not be reported as an empty one:\n%s", out)
	}
	if !strings.Contains(err.Error(), "no_egress") {
		t.Fatalf("the real fault lost its own message: %v\n%s", err, out)
	}
}

func noCapturedSessionsOptions(t *testing.T) Options {
	t.Helper()
	repoDir := seedFixtureRepo(t)
	runner := seedFixtureRunner(repoDir)
	addRefreshSemanticFixture(runner, repoDir)
	for key, response := range emptyCatalogRunner(t).responses {
		runner.responses[key] = response
	}
	return Options{
		Version: "test-version",
		Env: EntireEnv{
			RepoRoot:        repoDir,
			PluginConfigDir: filepath.Join(t.TempDir(), "config"),
			PluginDataDir:   filepath.Join(t.TempDir(), "data"),
			PluginStateDir:  filepath.Join(t.TempDir(), "state"),
			PluginCacheDir:  filepath.Join(t.TempDir(), "cache"),
		},
		Runner: runner,
		Now:    func() time.Time { return time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC) },
	}
}

// noCapturedSessionsHealth is the verdict input for the ambiguous half of this
// state: Entire's routed inventory could not be proven complete, so the
// sessions component DID fail. Fault 1 removes the provable case; this is the
// case that legitimately remains, and faults 2 and 3 are about what the reader
// is told to do about it.
func noCapturedSessionsHealth() statusHealth {
	return buildStatusHealth(brainStatusReport{
		Semantic: &brainStatusSemantic{},
		Onboarding: &brainStatusOnboarding{Components: []brainStatusComponent{{
			Name:   brainComponentSessions,
			State:  "failed",
			Detail: checkpointScopeIncompleteCode + ": Entire returned no readable checkpoint IDs from an incomplete persistent-store inventory",
			Hint:   setupNoSessionsHint(setupBrainBinaryName),
		}}},
	})
}

// TestNoCapturedSessionsGetsTheHintThatAlreadyExists is fault 3. The sentence
// was written for exactly this state and only `setup` ever reached it.
func TestNoCapturedSessionsGetsTheHintThatAlreadyExists(t *testing.T) {
	t.Parallel()
	health := noCapturedSessionsHealth()
	if !health.NoCapturedSessions {
		t.Fatal("a failed session export with the no-sessions signature must be recognised as that state")
	}
	health.Severity = "degraded"
	got := health.nextStepMessage(setupBrainBinaryName)
	if got != setupNoSessionsHint(setupBrainBinaryName) {
		t.Fatalf("next step is\n  %s\nwant the no-sessions guidance\n  %s", got, setupNoSessionsHint(setupBrainBinaryName))
	}
	if strings.Contains(got, statusRemedyRefresh) {
		t.Fatalf("the next step still names the command whose first stage is the failure: %s", got)
	}
}

// TestDirtyWorktreeHintStillFires is the regression guard. The dirty-tree
// override was already correct and is gated on the same remedy, so a careless
// second override could displace it.
func TestDirtyWorktreeHintStillFires(t *testing.T) {
	t.Parallel()
	dirty := statusHealth{Severity: "degraded", DirtyWorktree: true}
	if got := dirty.nextStepMessage(setupBrainBinaryName); got != setupDirtyWorktreeHint(setupBrainBinaryName) {
		t.Fatalf("dirty worktree next step = %s", got)
	}
	// And a clean tree in that same state keeps the refresh it can run.
	clean := statusHealth{Severity: "degraded"}
	if got := clean.nextStepMessage(setupBrainBinaryName); got != "run `"+setupBrainBinaryName+" "+statusRemedyRefresh+"`" {
		t.Fatalf("clean worktree, stale index next step = %s", got)
	}
}

// TestNoNextStepReEmitsItself is the loop guard, written as the property
// rather than as a string: follow the advice, and if the state the reader is
// in is unchanged by it, the advice is a loop.
//
// Exactly one transition is modelled, and the live transcript above
// establishes it: `refresh` runs the session export as its FIRST stage, so in
// a state where that export fails, refresh exits 1 having changed nothing.
// Every other remedy either repairs something or diagnoses (doctor prints the
// detail and stops), so the chain terminates.
func TestNoNextStepReEmitsItself(t *testing.T) {
	t.Parallel()
	for name, health := range map[string]statusHealth{
		"no captured sessions":                     noCapturedSessionsHealth(),
		"no captured sessions, stale index":        withSeverity(noCapturedSessionsHealth(), "degraded"),
		"no captured sessions, no semantic index":  withSemanticMissing(noCapturedSessionsHealth()),
		"no captured sessions, corrupt store":      withSeverity(noCapturedSessionsHealth(), "unsafe"),
		"no captured sessions on a dirty worktree": withDirty(noCapturedSessionsHealth()),
	} {
		first := health.nextStepMessage(setupBrainBinaryName)
		if !namesACommandThatRerunsTheSessionExport(first) {
			continue
		}
		// The named command cannot change this state, so the reader lands back
		// on the same verdict and is handed the same sentence. That is the bug.
		t.Errorf("%s: next step loops -- %q re-runs the stage that failed and status then re-emits:\n  %s", name, statusRemedyRefresh, first)
	}
}

// namesACommandThatRerunsTheSessionExport reports advice whose first action is
// the stage that failed.
func namesACommandThatRerunsTheSessionExport(nextStep string) bool {
	return strings.Contains(nextStep, "`"+setupBrainBinaryName+" "+statusRemedyRefresh+"`")
}

func withSeverity(health statusHealth, severity string) statusHealth {
	health.Severity = severity
	return health
}

func withSemanticMissing(health statusHealth) statusHealth {
	health.SemanticMissing = true
	return health
}

func withDirty(health statusHealth) statusHealth {
	health.DirtyWorktree = true
	return health
}

// TestSetupAndDoctorDoNotReferToEachOther is fault 2's other half, and needs
// no model at all: `setup` ended a diagnosed component's line with "run
// 'entire-brain doctor' for detail", and doctor re-rendered that same
// component with its hint appended -- a hint that ends by naming `setup`. Two
// commands, each sending the reader to the other, neither adding a step.
func TestSetupAndDoctorDoNotReferToEachOther(t *testing.T) {
	t.Parallel()
	component := newSetupComponent(brainComponentSessions,
		errors.New(checkpointScopeIncompleteCode+": Entire returned no readable checkpoint IDs from an incomplete persistent-store inventory"),
		setupBrainBinaryName)
	if component.Hint == "" {
		t.Fatal("precondition: this component is the one that carries a hint")
	}
	// What doctor prints for this component (doctor.go appends the hint).
	doctorLine := component.Detail + "; " + component.Hint
	if !strings.Contains(doctorLine, setupBrainBinaryName+" setup") {
		t.Fatalf("precondition: doctor's line for this component names setup:\n%s", doctorLine)
	}
	setupLine := setupComponentFailureLine(component, "--", setupBrainBinaryName)
	if strings.Contains(setupLine, "doctor") {
		t.Fatalf("setup sends the reader to doctor, which sends them back to setup:\n  setup:  %s\n  doctor: %s", setupLine, doctorLine)
	}
	// An UNdiagnosed component has nothing else to offer, so the referral to
	// doctor stays: that one really is where the detail lives.
	opaque := newSetupComponent(brainComponentSessions, errors.New("some unclassified failure"), setupBrainBinaryName)
	if opaque.Hint != "" {
		t.Fatalf("precondition: this component carries no hint: %+v", opaque)
	}
	if !strings.Contains(setupComponentFailureLine(opaque, "--", setupBrainBinaryName), "doctor") {
		t.Fatal("a component with no hint still has to point somewhere")
	}
	// ...and so does a component whose hint names a DIFFERENT remedy. The
	// repo-key skew sends the reader to `refresh index --force`, so nothing
	// about going to doctor first sends them back here: no circle, no change.
	skew := newSetupComponent(brainComponentSemantic, errors.New(`semantic snapshot repo_key "local/a" does not match current repo "local/b"`), setupBrainBinaryName)
	if skew.Hint == "" {
		t.Fatalf("precondition: the repo-key skew carries a hint: %+v", skew)
	}
	if !strings.Contains(setupComponentFailureLine(skew, "--", setupBrainBinaryName), "doctor") {
		t.Fatal("only a hint that points back at `setup` closes the loop; the others keep their referral")
	}
}

// TestDoctorCaptureCheckDoesNotRecommendANoOp is the third site of the same
// self-referential-remedy class. doctor's `capture` check answered BOTH "the
// sessions stage never ran" and "it ran and Entire has captured nothing" with
// "run `entire brain refresh`". For the second, refresh succeeds, exports zero
// sessions again, and doctor re-prints the identical line forever.
func TestDoctorCaptureCheckDoesNotRecommendANoOp(t *testing.T) {
	ran := captureCheckFor(t, true)
	if ran.State != "ok" {
		t.Errorf("a repo that has captured nothing yet is not broken: %+v", ran)
	}
	// Rendered, not hardcoded: this stack prints every brain command in the
	// spelling the reader reached the binary by (brainInvocation), so an
	// assertion pinned to one spelling would pass vacuously in the other.
	refreshCommand := brainInvocation(os.LookupEnv, "refresh")
	if strings.Contains(ran.Detail, refreshCommand) {
		t.Errorf("the remedy for 'nothing captured yet' cannot be a refresh that captures nothing: %s", ran.Detail)
	}
	for _, want := range []string{"no captured sessions", "entire checkpoint list"} {
		if !strings.Contains(ran.Detail, want) {
			t.Errorf("the capture line must say what the state is and how to check it, missing %q: %s", want, ran.Detail)
		}
	}
	// The stage genuinely not having run still points at the stage.
	never := captureCheckFor(t, false)
	if never.State != "warn" || !strings.Contains(never.Detail, refreshCommand) {
		t.Errorf("a sessions stage that never ran is still a refresh away: %+v", never)
	}
}

// captureCheckFor drives doctor's read-only report against a brain whose
// manifest either declares an empty sessions source (the stage ran and found
// nothing) or declares none at all (the stage never ran), and returns the
// capture check.
func captureCheckFor(t *testing.T, sessionsStageRan bool) doctorCheckResult {
	t.Helper()
	opts := noCapturedSessionsOptions(t)
	storage, err := repoStoragePaths(context.Background(), opts.Runner, opts.Env, opts.Env.RepoRoot)
	if err != nil {
		t.Fatalf("resolve brain storage: %v", err)
	}
	if err := os.MkdirAll(storage.BrainDir, 0o700); err != nil {
		t.Fatalf("create brain dir: %v", err)
	}
	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		GeneratedAt:   opts.Now().UTC(),
		RepoRoot:      opts.Env.RepoRoot,
		RepoKey:       storage.Key,
		Sources:       &brainSources{},
	}
	if sessionsStageRan {
		if err := withBrainWriteLock(storage.BrainDir, func() error {
			return writeBrainSessionSourceLocked(storage.BrainDir, storage.Key, manifest)
		}); err != nil {
			t.Fatalf("write sessions source: %v", err)
		}
	} else if err := writeBrainManifestAndReadme(storage.BrainDir, manifest); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	checks, _ := brainDoctorReadOnlyReport(context.Background(), opts, opts.Env.RepoRoot)
	for _, check := range checks {
		if check.Name == "capture" {
			return check
		}
	}
	t.Fatalf("doctor reported no capture check: %+v", checks)
	return doctorCheckResult{}
}

// --- egress ALLOWED ------------------------------------------------------
//
// The no-egress rail above is what makes the session export REFUSE, and that
// rail is opt-in. Everything below runs with neither ENTIRE_BRAIN_NO_EGRESS
// nor ENTIRE_BRAIN_LOCAL_ONLY set, because that is the configuration real
// users run and it is where the remaining half of this defect lives: Entire's
// own routed inventory can report itself INCOMPLETE, and then a repository
// with nothing captured still fails its sessions component -- honestly, since
// nothing can prove the inventory empty -- and still got sent to a command
// whose first stage is that same export.

// routedScopeIncompleteStderr is the stable stderr record Entire emits when it
// could not enumerate every checkpoint store.
const routedScopeIncompleteStderr = entireCheckpointScopePrefix +
	`{"schema_version":1,"code":"checkpoint_scope_incomplete","complete":false,"issues":[{"code":"store_unreadable","count":1}]}`

// TestEgressAllowedEmptyCatalogStillAsksEntire is the guard on the gate added
// to the empty-catalog short circuit. In no-egress mode the local catalog IS
// the catalog, so proving it empty is proving the inventory empty. With egress
// ALLOWED that inference is false -- the routed `--search-all` list is the only
// complete inventory and it sees checkpoints the local refs do not -- so the
// short circuit must not fire and a locally-empty clone must not export empty.
func TestEgressAllowedEmptyCatalogStillAsksEntire(t *testing.T) {
	assertNoEgressRailsUnset(t)
	repoDir := seedFixtureRepo(t)
	runner := seedFixtureRunner(repoDir)
	addRefreshSemanticFixture(runner, repoDir)
	// Local refs enumerate cleanly and hold nothing...
	for key, response := range emptyCatalogRunner(t).responses {
		runner.responses[key] = response
	}
	// ...while Entire can see a checkpoint that only exists remotely.
	runner.responses[fakeCommandKey("entire-test", "checkpoint", "explain", "--json", "--search-all")] = fakeCommandResponse{
		stdout: `[{"checkpoint_id":"cccccccccccc"}]`,
	}
	opts := noCapturedSessionsOptions(t)
	opts.Env.RepoRoot = repoDir
	opts.Runner = runner
	out, _ := execute(t, NewRootCommand(opts), "refresh", "--agent", "none", "--semantic=false", "--entire-binary", "entire-test")
	if !runnerCalled(runner, "entire-test", "checkpoint", "explain", "--json", "--search-all") {
		t.Fatalf("with egress allowed, an empty LOCAL catalog does not prove the inventory empty -- Entire must still be asked:\n%s", out)
	}
}

// TestEgressAllowedNoSessionsIsNotSentBackToRefresh is the loop, in the only
// configuration a real user can reach it: Entire reports an incomplete scope
// and no readable IDs, so the component fails honestly -- and the reader must
// still be handed a step that is not the command that just failed.
func TestEgressAllowedNoSessionsIsNotSentBackToRefresh(t *testing.T) {
	assertNoEgressRailsUnset(t)
	f, runner := newSetupRefreshFixture(t)
	runner.responses[fakeCommandKey("entire", "checkpoint", "explain", "--json", "--search-all")] = fakeCommandResponse{
		stdout: "[]\n",
		stderr: routedScopeIncompleteStderr,
	}
	rec := &recordedSetup{}
	steps := rec.steps(f)
	steps.instant = nil // the real, best-effort instant phase
	setupOpts := defaultSetupOptions()
	setupOpts.noBackfill = true
	setupOpts.noDaemon = true
	out := &bytes.Buffer{}
	if err := runSetup(context.Background(), setupTestCommand(t, out, setupOpts), f.opts, setupOpts, f.repoDir, steps); err != nil {
		t.Fatalf("one failed component must not fail setup: %v\n%s", err, out)
	}
	if !strings.Contains(out.String(), "no captured sessions") {
		t.Fatalf("setup must explain the state rather than only report it:\n%s", out)
	}
	// Fault 2, setup's half: the hint's own remedy is `setup`, so adding a
	// referral to `doctor` would close the circle.
	if strings.Contains(out.String(), "run 'entire-brain doctor' for detail") {
		t.Fatalf("setup refers to doctor, whose answer for this component refers back to setup:\n%s", out)
	}

	// Fault 2, status's half: read the verdict the same surface builds.
	report := brainStatusReport{
		Semantic:   &brainStatusSemantic{},
		Onboarding: &brainStatusOnboarding{Components: instantPhaseComponents(nil, readSetupInstantRecord(filepath.Dir(f.storage.HeadPath)))},
	}
	health := buildStatusHealth(report)
	if !health.NoCapturedSessions {
		t.Fatalf("status did not recognise the state setup just recorded: %+v", health.Failed)
	}
	health.Severity = "degraded"
	if got := health.nextStepMessage(setupBrainBinaryName); got != setupNoSessionsHint(setupBrainBinaryName) {
		t.Fatalf("egress-allowed next step is\n  %s\nwant the no-sessions guidance", got)
	}
	if namesACommandThatRerunsTheSessionExport(health.nextStepMessage(setupBrainBinaryName)) {
		t.Fatal("egress-allowed next step still re-runs the stage that failed")
	}
}

// TestEgressAllowedCompleteEmptyInventoryIsNotAFailure pins the ordinary case:
// Entire answers "complete, and empty", which is a repository with nothing
// captured and nothing wrong. This passed before the change and must keep
// passing -- the whole point of the no-egress gate above.
func TestEgressAllowedCompleteEmptyInventoryIsNotAFailure(t *testing.T) {
	assertNoEgressRailsUnset(t)
	f, _ := newSetupRefreshFixture(t) // scripts `--search-all` -> "[]" with no scope warning
	rec := &recordedSetup{}
	steps := rec.steps(f)
	steps.instant = nil
	setupOpts := defaultSetupOptions()
	setupOpts.noBackfill = true
	setupOpts.noDaemon = true
	out := &bytes.Buffer{}
	if err := runSetup(context.Background(), setupTestCommand(t, out, setupOpts), f.opts, setupOpts, f.repoDir, steps); err != nil {
		t.Fatalf("runSetup: %v\n%s", err, out)
	}
	if strings.Contains(out.String(), "session export failed") {
		t.Fatalf("a complete, empty inventory is not a failed sessions component:\n%s", out)
	}
	health := buildStatusHealth(brainStatusReport{
		Semantic:   &brainStatusSemantic{},
		Onboarding: &brainStatusOnboarding{Components: instantPhaseComponents(nil, readSetupInstantRecord(filepath.Dir(f.storage.HeadPath)))},
	})
	for _, failed := range health.Failed {
		if failed == brainComponentSessions {
			t.Fatalf("sessions reported as failed on a complete, empty inventory: %v", health.Failed)
		}
	}
	if health.NoCapturedSessions {
		t.Fatal("a complete, empty inventory needs no no-sessions hint: nothing failed")
	}
}

// assertNoEgressRailsUnset fails loudly rather than letting an inherited rail
// turn an egress-allowed test into a second copy of the no-egress ones.
func assertNoEgressRailsUnset(t *testing.T) {
	t.Helper()
	for _, name := range []string{"ENTIRE_BRAIN_NO_EGRESS", "ENTIRE_BRAIN_LOCAL_ONLY"} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			t.Fatalf("%s=%q: this test must run with egress allowed", name, value)
		}
	}
	if brainNoEgressMode() {
		t.Fatal("no-egress mode is on; this test must run with egress allowed")
	}
}

func runnerCalled(runner *fakeCommandRunner, name string, args ...string) bool {
	want := fakeCommandKey(name, args...)
	for _, call := range runner.calls {
		if fakeCommandKey(call.name, call.args...) == want {
			return true
		}
	}
	return false
}
