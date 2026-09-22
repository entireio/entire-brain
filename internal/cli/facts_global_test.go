package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// Global facts are the ones that follow you everywhere, which makes their
// failure modes the expensive kind. A global fact that cannot be retracted
// follows you everywhere. A global fact that is not labelled gets cited as a
// property of whatever repository you happen to be in. And a global fact that
// lands in a repository's semantic cache quietly corrupts a store that other
// repositories read.

func globalTestOptions(t *testing.T) (Options, string) {
	t.Helper()
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	opts := Options{
		Version: "test",
		Env:     env,
		Runner:  semanticFixtureRunner(repoDir, ""),
		Now:     func() time.Time { return time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC) },
	}
	return opts, repoDir
}

func rememberGlobal(t *testing.T, opts Options, text string) string {
	t.Helper()
	cmd := &cobra.Command{}
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	err := runRemember(context.Background(), cmd, opts,
		rememberCommandOptions{path: "preferences.coding.style", agent: "none", global: true}, text)
	if err != nil {
		t.Fatalf("remember --global: %v", err)
	}
	return out.String()
}

func TestGlobalFactIsWrittenOutsideEveryRepository(t *testing.T) {
	opts, _ := globalTestOptions(t)
	out := rememberGlobal(t, opts, "I prefer table-driven tests.")

	if !strings.Contains(out, "globally") {
		t.Fatalf("the command did not say where it filed the fact: %q", out)
	}
	// The store has to live beside the per-repo ones, not inside one of them,
	// or a fact meant for every repository would be lost with whichever
	// repository's brain happened to hold it.
	dir, err := globalBrainDir(opts.Env)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(dir, repoStoreDirName+string(filepath.Separator)) {
		t.Fatalf("the global store is inside the per-repo tree: %s", dir)
	}
	facts, err := loadGlobalFacts(opts.Env)
	if err != nil {
		t.Fatalf("loadGlobalFacts: %v", err)
	}
	if len(facts) != 1 || facts[0].Text != "I prefer table-driven tests." {
		t.Fatalf("the global store holds %+v", facts)
	}
}

func TestGlobalFactIsRecalledFromARepository(t *testing.T) {
	// The whole point: recorded once, available from a repository that has
	// never seen it.
	opts, _ := globalTestOptions(t)
	rememberGlobal(t, opts, "The staging cluster is in eu-west-1.")

	repoFacts := []factRecord{{ID: "local-1", Text: "Compaction runs above eight segments.", Status: factStatusActive}}
	global, err := loadGlobalFacts(opts.Env)
	if err != nil {
		t.Fatal(err)
	}
	merged, globalIDs := mergeGlobalFacts(repoFacts, global)

	if len(merged) != 2 {
		t.Fatalf("merge produced %d fact(s), want 2", len(merged))
	}
	found := false
	for _, fact := range merged {
		if strings.Contains(fact.Text, "eu-west-1") {
			found = true
			if !globalIDs[fact.ID] {
				t.Fatal("a global fact was merged in without being marked global")
			}
		}
	}
	if !found {
		t.Fatalf("the global fact did not reach the merged set: %+v", merged)
	}
	// And a repository fact must not be marked global, or every fact would be
	// rendered as a global convention.
	if globalIDs["local-1"] {
		t.Fatal("a repository fact was marked global")
	}
}

func TestRepositoryFactWinsOverAnIdenticalGlobalOne(t *testing.T) {
	// Fact ids are content-derived, so the same statement recorded in both
	// places collides. The repository's copy is the more specific claim and
	// carries that repo's provenance — the evidence somebody would check.
	shared := factRecord{ID: "same-id", Text: "We deploy on Thursdays.", Status: factStatusActive, Branch: "main"}
	global := factRecord{ID: "same-id", Text: "We deploy on Thursdays.", Status: factStatusActive, Branch: globalFactBranch}

	merged, globalIDs := mergeGlobalFacts([]factRecord{shared}, []factRecord{global})
	if len(merged) != 1 {
		t.Fatalf("the same fact was surfaced twice: %+v", merged)
	}
	if merged[0].Branch != "main" {
		t.Fatalf("the global copy won: %+v", merged[0])
	}
	if globalIDs["same-id"] {
		t.Fatal("a fact the repository also holds was labelled global")
	}
}

func TestGlobalFactsCanBeTurnedOff(t *testing.T) {
	// A machine where repository answers must not be influenced by anything
	// outside the repository. Both the flag and the environment variable have
	// to work, and the variable is fail-closed like every other guard here.
	if globalFactsEnabled(true) {
		t.Fatal("--no-global did not disable global facts")
	}
	t.Setenv("ENTIRE_BRAIN_NO_GLOBAL_FACTS", "1")
	if globalFactsEnabled(false) {
		t.Fatal("ENTIRE_BRAIN_NO_GLOBAL_FACTS did not disable global facts")
	}
	t.Setenv("ENTIRE_BRAIN_NO_GLOBAL_FACTS", "ture")
	if globalFactsEnabled(false) {
		t.Fatal("an unrecognized toggle value left global facts on; the toggle must fail closed")
	}
	t.Setenv("ENTIRE_BRAIN_NO_GLOBAL_FACTS", "")
	if !globalFactsEnabled(false) {
		t.Fatal("global facts are off by default; a fact recorded once is meant to apply")
	}
}

func TestGlobalFactCanBeRetracted(t *testing.T) {
	// A fact that can be written and not retracted is a fact you are stuck
	// with, in every repository at once.
	opts, _ := globalTestOptions(t)
	rememberGlobal(t, opts, "The staging cluster is in eu-west-1.")
	facts, err := loadGlobalFacts(opts.Env)
	if err != nil || len(facts) != 1 {
		t.Fatalf("setup: %v %+v", err, facts)
	}
	id := facts[0].ID

	cmd := newFactsRetractCommand(opts)
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetContext(context.Background())
	if err := cmd.Flags().Set("global", "true"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.RunE(cmd, []string{id}); err != nil {
		t.Fatalf("retract --global: %v", err)
	}
	if !strings.Contains(out.String(), "globally") {
		t.Fatalf("retract did not say where: %q", out.String())
	}

	after, err := loadGlobalFacts(opts.Env)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].Status != factStatusRetracted {
		t.Fatalf("the global fact was not retracted: %+v", after)
	}
	// A retracted global fact must stop being merged into repository reads.
	merged, _ := mergeGlobalFacts(nil, after)
	for _, fact := range filterFactsByScope(merged, "") {
		if fact.Status == factStatusActive {
			t.Fatalf("a retracted global fact is still active: %+v", fact)
		}
	}
}

func TestGlobalAndBranchCannotBeCombined(t *testing.T) {
	// Quietly ignoring --branch would file the fact somewhere the user did not
	// ask for, and they would look for it on the branch they named.
	opts, _ := globalTestOptions(t)
	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err := runRemember(context.Background(), cmd, opts,
		rememberCommandOptions{path: "preferences.coding.style", agent: "none", global: true, branch: "main"}, "x")
	if err == nil {
		t.Fatal("--global --branch was accepted")
	}
	if !strings.Contains(err.Error(), "branch") {
		t.Fatalf("the error should explain the conflict: %v", err)
	}
}

func TestGlobalFactHasNoCommitAnchor(t *testing.T) {
	// An anchor pointing at whatever happened to be checked out would be worse
	// than none: verification resolves it and reports the fact as evidenced by
	// a commit it has nothing to do with.
	opts, _ := globalTestOptions(t)
	rememberGlobal(t, opts, "I prefer table-driven tests.")
	facts, err := loadGlobalFacts(opts.Env)
	if err != nil || len(facts) != 1 {
		t.Fatalf("setup: %v", err)
	}
	for _, anchor := range facts[0].Provenance {
		if strings.TrimSpace(anchor.Commit) != "" {
			t.Fatalf("a global fact cites commit %q", anchor.Commit)
		}
	}
}

func TestRememberWhereThereIsNoRepositoryFilesGloballyRatherThanFailing(t *testing.T) {
	// The moment you most want to write something down is when you are not in a
	// checkout. Refusing there threw away what somebody had just typed — and the
	// fact they were recording is usually exactly the kind that belongs
	// globally anyway.
	opts, _ := globalTestOptions(t)
	// A repository root that does not resolve, which is what being outside a
	// repository looks like to resolveFactsTarget.
	opts.Env.RepoRoot = filepath.Join(t.TempDir(), "not-a-repository")

	cmd := &cobra.Command{}
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	err := runRemember(context.Background(), cmd, opts,
		rememberCommandOptions{path: "preferences.coding.style", agent: "none"}, "Deploys go out on Thursdays.")
	if err != nil {
		t.Fatalf("remember outside a repository failed: %v", err)
	}
	// And it must say so. Filing a fact somewhere the user did not choose and
	// saying nothing means they only find out by missing it later.
	if !strings.Contains(errOut.String(), "globally") {
		t.Fatalf("the fallback was silent: %q", errOut.String())
	}
	if !strings.Contains(out.String(), "globally") {
		t.Fatalf("the success line does not say where it went: %q", out.String())
	}
	facts, err := loadGlobalFacts(opts.Env)
	if err != nil || len(facts) != 1 {
		t.Fatalf("the fact was not stored globally: %v %+v", err, facts)
	}
}

// This is the case a faked git runner cannot express, and the one that was
// actually broken: a directory that EXISTS but is not a working tree.
// resolveFactsTarget happily succeeds there and then defaults the branch to
// "main", so a fact recorded in ~/Desktop was filed against a repository that
// does not exist, on a branch nobody is on — which looks like success and loses
// the fact. Real git is the only thing that can tell the difference, so this
// test uses the real runner.
func TestPlainDirectoryIsNotMistakenForARepository(t *testing.T) {
	plain := t.TempDir() // exists, has no .git, and never will
	opts, _ := globalTestOptions(t)
	opts.Runner = ExecRunner{}
	opts.Env.RepoRoot = plain

	here, err := inARepository(context.Background(), opts)
	if err != nil {
		t.Fatalf("a plain directory reported an error rather than \"no repository\": %v", err)
	}
	if here {
		t.Fatal("a plain directory was treated as a repository")
	}

	cmd := &cobra.Command{}
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	if err := runRemember(context.Background(), cmd, opts,
		rememberCommandOptions{path: "preferences.coding.style", agent: "none"}, "Deploys go out on Thursdays."); err != nil {
		t.Fatalf("remember in a plain directory: %v", err)
	}
	// "on main" here is the bug: it means the fact went to a per-repo store for
	// a repository that does not exist.
	if strings.Contains(out.String(), " on ") {
		t.Fatalf("the fact was filed against a branch: %q", out.String())
	}
	if !strings.Contains(out.String(), "globally") {
		t.Fatalf("the fact was not filed globally: %q", out.String())
	}
	facts, err := loadGlobalFacts(opts.Env)
	if err != nil || len(facts) != 1 {
		t.Fatalf("the global store holds %+v (%v)", facts, err)
	}
}

// The mirror: inside a real working tree, nothing may be diverted to the global
// store. A repository fact appearing globally would follow somebody into every
// other repository as though it were a general truth.
func TestRealRepositoryStillFilesFactsLocally(t *testing.T) {
	repoDir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "."},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "T"},
	} {
		if _, _, err := (ExecRunner{}).Run(context.Background(), repoDir, "git", args...); err != nil {
			t.Skipf("git unavailable: %v", err)
		}
	}
	opts, _ := globalTestOptions(t)
	opts.Runner = ExecRunner{}
	opts.Env.RepoRoot = repoDir

	here, err := inARepository(context.Background(), opts)
	if err != nil {
		t.Fatalf("a real working tree reported an error: %v", err)
	}
	if !here {
		t.Fatal("a real working tree was not recognised as a repository")
	}

	cmd := &cobra.Command{}
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	if err := runRemember(context.Background(), cmd, opts,
		rememberCommandOptions{path: "architecture.storage.invariant", agent: "none"}, "Compaction runs above eight segments."); err != nil {
		t.Fatalf("remember: %v", err)
	}
	if strings.Contains(out.String(), "globally") {
		t.Fatalf("a repository fact was filed globally: %q", out.String())
	}
	facts, err := loadGlobalFacts(opts.Env)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 0 {
		t.Fatalf("a repository fact leaked into the global store: %+v", facts)
	}
}

func TestExplicitBranchOutsideARepositoryStillFails(t *testing.T) {
	// The fallback is for somebody who did not name a target. Somebody who
	// typed --branch asked for a specific repository branch, and filing that
	// globally would be a different write than the one they asked for.
	opts, _ := globalTestOptions(t)
	opts.Env.RepoRoot = filepath.Join(t.TempDir(), "not-a-repository")

	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err := runRemember(context.Background(), cmd, opts,
		rememberCommandOptions{path: "preferences.coding.style", agent: "none", branch: "main"}, "x")
	if err == nil {
		t.Fatal("--branch outside a repository was silently filed globally")
	}
	// And the error has to be about the repository, not about --global. The
	// user never typed --global; telling them those two flags conflict sends
	// them looking for a flag they did not use.
	if strings.Contains(err.Error(), "--global") {
		t.Fatalf("the error blames a flag the user did not pass: %v", err)
	}
	if !strings.Contains(err.Error(), "repository") {
		t.Fatalf("the error should name the real problem: %v", err)
	}
}

func TestGlobalStoreIsNotCreatedJustByReading(t *testing.T) {
	// Somebody with no global facts should not find a directory they never
	// asked for, and a read must not fail because there is nothing there.
	opts, _ := globalTestOptions(t)
	facts, err := loadGlobalFacts(opts.Env)
	if err != nil {
		t.Fatalf("reading an empty global store failed: %v", err)
	}
	if len(facts) != 0 {
		t.Fatalf("got %d fact(s) from a store that does not exist", len(facts))
	}
	dir, err := globalBrainDir(opts.Env)
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(dir); statErr == nil {
		t.Fatalf("reading created %s", dir)
	}
}

func TestFactsGlobalListsTheStore(t *testing.T) {
	opts, _ := globalTestOptions(t)

	cmd := newFactsGlobalCommand(opts)
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("facts global: %v", err)
	}
	// The empty state is where somebody learns the feature exists.
	if !strings.Contains(out.String(), "--global") {
		t.Fatalf("the empty listing does not say how to record one: %q", out.String())
	}

	rememberGlobal(t, opts, "I prefer table-driven tests.")
	cmd = newFactsGlobalCommand(opts)
	out.Reset()
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("facts global: %v", err)
	}
	if !strings.Contains(out.String(), "I prefer table-driven tests.") {
		t.Fatalf("the fact is not listed: %q", out.String())
	}
}

func TestGlobalFactsAreLabelledWhenSurfaced(t *testing.T) {
	// Acting on a global convention as though this repository declared it is a
	// different thing, and an agent reading unlabelled output cannot tell.
	fact := factRecord{ID: "g1", Kind: "preference", Paths: []string{"preferences.coding.style"}, Text: "I prefer table-driven tests."}

	plain := &bytes.Buffer{}
	printFactLine(plain, fact)
	labelled := &bytes.Buffer{}
	printGlobalFactLine(labelled, fact)

	if !strings.Contains(labelled.String(), "(global)") {
		t.Fatalf("a global fact rendered without a label: %q", labelled.String())
	}
	if strings.Contains(plain.String(), "(global)") {
		t.Fatalf("a repository fact rendered as global: %q", plain.String())
	}
	// The fact's own text must survive the labelling.
	if !strings.Contains(labelled.String(), fact.Text) {
		t.Fatalf("labelling lost the text: %q", labelled.String())
	}
}

func TestGlobalFactIDsAreReportedInJSON(t *testing.T) {
	matches := []factRecord{{ID: "local"}, {ID: "g1"}, {ID: "g2"}}
	ids := sortedGlobalFactIDs(matches, map[string]bool{"g1": true, "g2": true})
	if len(ids) != 2 || ids[0] != "g1" || ids[1] != "g2" {
		t.Fatalf("global ids = %v", ids)
	}
	// Surfacing order, not map order, so JSON output is stable across runs.
	reversed := sortedGlobalFactIDs([]factRecord{{ID: "g2"}, {ID: "g1"}}, map[string]bool{"g1": true, "g2": true})
	if reversed[0] != "g2" {
		t.Fatalf("ids do not follow surfacing order: %v", reversed)
	}
	if got := sortedGlobalFactIDs(matches, map[string]bool{}); len(got) != 0 {
		t.Fatalf("ids reported with no global facts: %v", got)
	}
}

// "No repository here" and "I cannot tell which repository this is" call for
// opposite responses. resolveFactsTarget refuses to guess when state exists
// under two identity keys; treating that refusal as "not in a repository" would
// swallow the guard and file a repo-scoped fact globally — defeating the exact
// protection that was raised.
func TestUndecidableRepositoryIdentityIsSurfacedNotFiledGlobally(t *testing.T) {
	opts, _ := globalTestOptions(t)
	conflict := &localRepoIdentityConflictError{
		Canonical: repoStorage{Key: "local/repo-aaaa", BrainDir: "/tmp/a"},
		Legacy:    repoStorage{Key: "local/repo-bbbb", BrainDir: "/tmp/b"},
	}
	// The guard must be recognised through wrapping, the way it reaches a
	// caller in practice.
	if !errorIsRepoIdentityConflict(fmt.Errorf("resolve target: %w", conflict)) {
		t.Fatal("a wrapped identity conflict is not recognised; it would be treated as no repository")
	}
	if errorIsRepoIdentityConflict(errors.New("facts require a local repository path: /nowhere")) {
		t.Fatal("an ordinary not-a-repository error was mistaken for an identity conflict")
	}
	_ = opts
}

// errorIsRepoIdentityConflict mirrors the classification inARepository makes,
// so the distinction can be held without a repository in two states on disk.
func errorIsRepoIdentityConflict(err error) bool {
	var conflict *localRepoIdentityConflictError
	return errors.As(err, &conflict)
}

// A global fact was never scoped to this repository, so asking whether its
// locus still exists here is meaningless — and the answer is always alarming.
// Any path a global fact mentions would be flagged stale in every repository
// but the one it came from.
func TestLocusDriftSkipsGlobalFacts(t *testing.T) {
	repoDir := t.TempDir()
	local := factRecord{ID: "local-1", Text: "See internal/cli/missing_file.go for the parser.", Locus: []string{"internal/cli/missing_file.go"}}
	// A concrete file path, because the drift check only considers tokens that
	// look like files — a bare directory is never checked, so a fixture using
	// one would pass whether or not global facts are filtered.
	global := factRecord{ID: "global-1", Text: "Team convention: migrations start at db/migrations/0001_init.sql.", Locus: []string{"db/migrations/0001_init.sql"}}

	matches := []factRecord{local, global}
	globalIDs := map[string]bool{"global-1": true}

	// The production filter, not a copy of it in the test: a test that
	// reimplements the logic it is checking passes whatever the code does.
	drift := factsLocusDrift(repoDir, factsEligibleForLocusDrift(matches, globalIDs))
	if _, flagged := drift["global-1"]; flagged {
		t.Fatal("a global fact was flagged as having a stale locus in a repository it was never scoped to")
	}
	// The repository's own fact must still be checked, or the filter has
	// disabled drift detection rather than scoping it.
	if _, flagged := drift["local-1"]; !flagged {
		t.Fatalf("a repository fact with a missing locus was not flagged: %+v", drift)
	}
}

// A store that can be written and retracted but never pruned grows without
// bound, and the reference documents global facts as getting "the same
// retraction and garbage collection" as repository ones. `facts retract
// --global` shipped without its counterpart, so that promise was not kept.
func TestGlobalFactsCanBeGarbageCollected(t *testing.T) {
	opts, _ := globalTestOptions(t)
	rememberGlobal(t, opts, "The staging cluster is in eu-west-1.")

	facts, err := loadGlobalFacts(opts.Env)
	if err != nil || len(facts) != 1 {
		t.Fatalf("setup: %v %+v", err, facts)
	}
	id := facts[0].ID

	retract := newFactsRetractCommand(opts)
	retract.SetOut(&bytes.Buffer{})
	retract.SetErr(&bytes.Buffer{})
	retract.SetContext(context.Background())
	if err := retract.Flags().Set("global", "true"); err != nil {
		t.Fatal(err)
	}
	if err := retract.RunE(retract, []string{id}); err != nil {
		t.Fatalf("retract --global: %v", err)
	}

	gc := newFactsGCCommand(opts)
	out := &bytes.Buffer{}
	gc.SetOut(out)
	gc.SetErr(&bytes.Buffer{})
	gc.SetContext(context.Background())
	if err := gc.Flags().Set("global", "true"); err != nil {
		t.Fatalf("`facts gc` has no --global flag, so a retracted global fact can never be pruned: %v", err)
	}
	if err := gc.Flags().Set("force", "true"); err != nil {
		t.Fatal(err)
	}
	if err := gc.RunE(gc, nil); err != nil {
		t.Fatalf("gc --global: %v", err)
	}

	after, err := loadGlobalFacts(opts.Env)
	if err != nil {
		t.Fatal(err)
	}
	for _, fact := range after {
		if fact.ID == id {
			t.Fatalf("the retracted global fact survived gc: %+v", fact)
		}
	}
}

func TestGlobalGCRefusesABranch(t *testing.T) {
	// A global fact is not on a branch; accepting both would prune something
	// other than what was asked for.
	opts, _ := globalTestOptions(t)
	gc := newFactsGCCommand(opts)
	gc.SetOut(&bytes.Buffer{})
	gc.SetErr(&bytes.Buffer{})
	gc.SetContext(context.Background())
	for _, flag := range [][2]string{{"global", "true"}, {"branch", "main"}} {
		if err := gc.Flags().Set(flag[0], flag[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := gc.RunE(gc, nil); err == nil {
		t.Fatal("--global --branch was accepted")
	}
}

func TestARetractedRepositoryFactDoesNotHideAnActiveGlobalOne(t *testing.T) {
	// Shadowing is justified by the local copy being "the more specific claim
	// [that] carries that repo's provenance". A retracted fact makes no claim,
	// so there is nothing for it to shadow with — and because ids are
	// content-derived, retracting a local duplicate is exactly how a user would
	// expect to fall back to the global statement, not to lose it silently.
	retracted := factRecord{ID: "same-id", Text: "We deploy on Thursdays.", Status: factStatusRetracted, Branch: "main"}
	global := factRecord{ID: "same-id", Text: "We deploy on Thursdays.", Status: factStatusActive, Branch: globalFactBranch}

	merged, globalIDs := mergeGlobalFacts([]factRecord{retracted}, []factRecord{global})
	if len(merged) != 2 {
		t.Fatalf("the active global fact was suppressed by a retracted local one: %+v", merged)
	}
	if !globalIDs["same-id"] {
		t.Fatalf("the global fact must still be labelled global: %+v", globalIDs)
	}
	var sawGlobal bool
	for _, fact := range merged {
		if fact.Branch == globalFactBranch && fact.Status == factStatusActive {
			sawGlobal = true
		}
	}
	if !sawGlobal {
		t.Fatalf("the surviving global copy is not present: %+v", merged)
	}
}

func TestASupersededRepositoryFactDoesNotHideAnActiveGlobalOne(t *testing.T) {
	superseded := factRecord{ID: "same-id", Text: "We deploy on Thursdays.", Status: factStatusSuperseded, Branch: "main"}
	global := factRecord{ID: "same-id", Text: "We deploy on Thursdays.", Status: factStatusActive, Branch: globalFactBranch}

	merged, globalIDs := mergeGlobalFacts([]factRecord{superseded}, []factRecord{global})
	if len(merged) != 2 || !globalIDs["same-id"] {
		t.Fatalf("a superseded local fact suppressed the global one: %+v %+v", merged, globalIDs)
	}
}

// The three tests below exist because merging worked while no surface an agent
// actually uses called it. mergeGlobalFacts had exactly one caller — the recall
// command — so a fact recorded once reached the CLI and nothing else.

func TestMCPRememberCanRecordAGlobalFact(t *testing.T) {
	// brain_remember is how an MCP-connected agent writes memory. Without a
	// global argument it could only ever write repository-scoped facts, so the
	// primary consumer of the feature could not use it at all.
	opts, _ := globalTestOptions(t)
	params, err := json.Marshal(mcpToolCallParams{Name: "brain_remember", Arguments: map[string]any{
		"fact":   "The staging cluster is in eu-west-1.",
		"path":   "preferences.coding.style",
		"global": true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	response := handleMCPMessage(context.Background(), opts, mcpMessage{JSONRPC: "2.0", ID: 1, Method: "tools/call", Params: params})
	if response.Error != nil {
		t.Fatalf("brain_remember global: %+v", response.Error)
	}
	global, loadErr := loadGlobalFacts(opts.Env)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	for _, fact := range global {
		if strings.Contains(fact.Text, "eu-west-1") {
			return
		}
	}
	t.Fatalf("the fact was not written to the global store: %+v", global)
}

func TestMCPRememberStillDefaultsToThisRepository(t *testing.T) {
	// The new argument must not change what an omitted argument does.
	opts, _ := globalTestOptions(t)
	params, err := json.Marshal(mcpToolCallParams{Name: "brain_remember", Arguments: map[string]any{
		"fact": "Compaction runs above eight segments.",
		"path": "preferences.coding.style",
	}})
	if err != nil {
		t.Fatal(err)
	}
	response := handleMCPMessage(context.Background(), opts, mcpMessage{JSONRPC: "2.0", ID: 1, Method: "tools/call", Params: params})
	if response.Error != nil {
		t.Fatalf("brain_remember: %+v", response.Error)
	}
	global, loadErr := loadGlobalFacts(opts.Env)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	for _, fact := range global {
		if strings.Contains(fact.Text, "Compaction") {
			t.Fatalf("a repository fact was written to the global store: %+v", fact)
		}
	}
}

func TestBrainRememberAdvertisesGlobal(t *testing.T) {
	// An argument the schema does not advertise is one no agent will send.
	response := handleMCPMessage(context.Background(), Options{Version: "test"}, mcpMessage{
		JSONRPC: "2.0", ID: 1, Method: "tools/list",
	})
	if response.Error != nil {
		t.Fatalf("tools/list: %+v", response.Error)
	}
	encoded, err := json.Marshal(response.Result)
	if err != nil {
		t.Fatal(err)
	}
	var listing struct {
		Tools []struct {
			Name        string `json:"name"`
			InputSchema struct {
				Properties map[string]any `json:"properties"`
			} `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(encoded, &listing); err != nil {
		t.Fatal(err)
	}
	for _, tool := range listing.Tools {
		if tool.Name != "brain_remember" {
			continue
		}
		if _, ok := tool.InputSchema.Properties["global"]; !ok {
			t.Fatalf("brain_remember does not advertise global: %v", tool.InputSchema.Properties)
		}
		return
	}
	t.Fatal("brain_remember is not in tools/list")
}

// retrievalSurfaceFixture is a repository brain holding one local fact, with a
// global store holding one global fact. It is the shape every recall surface
// sees in practice: a repo that has never been told the global statement.
func retrievalSurfaceFixture(t *testing.T) (Options, string) {
	t.Helper()
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time {
		return time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	}}
	storage, err := repoStoragePaths((&cobra.Command{}).Context(), runner, env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	paths := normalizeFactPaths([]string{"architecture/retrieval.go"})
	local := factRecord{
		ID: factRecordID("orion local retrieval fact", paths), Text: "orion local retrieval fact",
		Paths: paths, Branch: "feature", Status: factStatusActive, UpdatedAt: opts.Now(),
	}
	if err := writeFacts(storage.BrainDir, "feature", []factRecord{local}); err != nil {
		t.Fatal(err)
	}
	rememberGlobal(t, opts, "orion global convention applies everywhere")
	return opts, storage.BrainDir
}

func TestQueryAndSearchSurfaceGlobalFacts(t *testing.T) {
	// brain_query, brain_search and brain_vsearch all run through runRetrieve.
	// Before this, a fact recorded once reached `recall` and no other surface.
	opts, _ := retrievalSurfaceFixture(t)
	cmd := &cobra.Command{}
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&strings.Builder{})
	if err := runRetrieve(context.Background(), cmd, opts, "orion", modeLexical, 10, "feature",
		retrievalOptions{Source: retrievalSourceFact}, true, false, "mcp:brain_query"); err != nil {
		t.Fatalf("runRetrieve: %v", err)
	}
	if !strings.Contains(out.String(), "global convention applies everywhere") {
		t.Fatalf("the global fact did not reach the query surface:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "local retrieval fact") {
		t.Fatalf("merging global facts dropped the repository's own:\n%s", out.String())
	}
}

func TestQueryRespectsTheGlobalOffSwitch(t *testing.T) {
	// An operator who turns global facts off must not get them anyway.
	opts, _ := retrievalSurfaceFixture(t)
	t.Setenv("ENTIRE_BRAIN_NO_GLOBAL_FACTS", "1")
	cmd := &cobra.Command{}
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&strings.Builder{})
	if err := runRetrieve(context.Background(), cmd, opts, "orion", modeLexical, 10, "feature",
		retrievalOptions{Source: retrievalSourceFact}, true, false, "mcp:brain_query"); err != nil {
		t.Fatalf("runRetrieve: %v", err)
	}
	if strings.Contains(out.String(), "global convention applies everywhere") {
		t.Fatalf("ENTIRE_BRAIN_NO_GLOBAL_FACTS did not suppress the global fact:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "local retrieval fact") {
		t.Fatalf("turning global facts off also hid the repository's own:\n%s", out.String())
	}
}

func TestBriefSurfacesGlobalFacts(t *testing.T) {
	// The agent guide mandates brief as the first call of every task, so a
	// preference recorded once has to reach it or the feature is invisible
	// exactly where it matters most.
	opts, _ := retrievalSurfaceFixture(t)
	cmd := &cobra.Command{}
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&strings.Builder{})
	if err := runBrainBrief(context.Background(), cmd, opts, brainBriefOptions{limit: 10, json: true}, "orion"); err != nil {
		t.Fatalf("runBrainBrief: %v", err)
	}
	if !strings.Contains(out.String(), "global convention applies everywhere") {
		t.Fatalf("the global fact did not reach the brief:\n%s", out.String())
	}
}

// A global fact belongs to every repository, so it must not be filed in any one
// repository's vector cache. The cache is keyed by (brainDir, branch) and flush
// prunes every id it did not see, so a global vector written under one repo's
// key is both a fact stored where it does not belong and one that vanishes the
// moment the same query runs with global facts turned off.
//
// Guarding only retain is not enough: factVector caches the vector and marks the
// id touched while ranking, long before retain is reached.
func TestGlobalFactsNeverEnterARepositorysVectorCache(t *testing.T) {
	embedder := defaultEmbedder()
	rr := newSemanticReranker(embedder)
	rr.touched = map[string]bool{}

	local := factRecord{ID: "local-1", Text: "local claim", Status: factStatusActive}
	global := factRecord{ID: "global-1", Text: "global claim", Status: factStatusActive}
	rr.markForeign(map[string]bool{global.ID: true})

	// Ranking embeds both — the global one must still be ranked.
	if v := rr.factVector(global); len(v) == 0 {
		t.Fatal("a global fact was not embedded, so it could not be ranked")
	}
	if v := rr.factVector(local); len(v) == 0 {
		t.Fatal("a repository fact was not embedded")
	}
	// ...but only the repository's fact may be persisted.
	if _, cached := rr.cache[global.ID]; cached {
		t.Fatal("a global fact's vector was cached under this repository's key")
	}
	if rr.touched[global.ID] {
		t.Fatal("a global fact was marked touched, so flush would persist it here")
	}
	if _, cached := rr.cache[local.ID]; !cached {
		t.Fatal("the repository's own fact stopped being cached")
	}
	if !rr.touched[local.ID] {
		t.Fatal("the repository's own fact stopped being retained")
	}

	// retain and markTouched must agree with factVector.
	rr.retain([]factRecord{local, global})
	rr.markTouched(global.ID)
	if rr.touched[global.ID] {
		t.Fatal("retain/markTouched still marked a global fact present in this store")
	}
	if !rr.touched[local.ID] {
		t.Fatal("retain dropped the repository's own fact")
	}
}

func TestVectorModeRanksGlobalFactsWithoutCachingThem(t *testing.T) {
	dir := t.TempDir()
	embedder := defaultEmbedder()
	local := factRecord{ID: "local-1", Text: "local claim", Status: factStatusActive}
	global := factRecord{ID: "global-1", Text: "global claim", Status: factStatusActive}

	got := factsVectorRanked(dir, "main", []factRecord{local, global}, "claim", embedder, 10,
		map[string]bool{global.ID: true})
	var sawGlobal bool
	for _, fact := range got {
		if fact.ID == global.ID {
			sawGlobal = true
		}
	}
	if !sawGlobal {
		t.Fatalf("vector mode dropped the global fact instead of ranking it: %+v", got)
	}

	store := newVectorStore(dir, "main", factEmbeddingModelID(embedder.ID()), embedder.Dim())
	cached := store.load()
	if _, ok := cached[global.ID]; ok {
		t.Fatal("vector mode wrote a global fact into this repository's store")
	}
	if _, ok := cached[local.ID]; !ok {
		t.Fatal("vector mode stopped caching the repository's own fact")
	}
}

func TestBriefLabelsGlobalFactsAndChecksOnlyLocalLoci(t *testing.T) {
	opts, brainDir := retrievalSurfaceFixture(t)
	rememberGlobal(t, opts, "orion convention uses db/migrations/global.sql")
	globalID := factRecordID("orion convention uses db/migrations/global.sql", normalizeFactPaths([]string{"preferences.coding.style"}))
	local, err := loadFacts(brainDir, "feature")
	if err != nil {
		t.Fatal(err)
	}
	local[0].Text = "orion local fact uses db/migrations/local.sql"
	if err := writeFacts(brainDir, "feature", local); err != nil {
		t.Fatal(err)
	}
	for _, jsonOutput := range []bool{true, false} {
		cmd := &cobra.Command{}
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&bytes.Buffer{})
		if err := runBrainBrief(context.Background(), cmd, opts, brainBriefOptions{limit: 10, json: jsonOutput}, "orion"); err != nil {
			t.Fatal(err)
		}
		if jsonOutput {
			var report brainBriefJSONReport
			if err := json.Unmarshal(out.Bytes(), &report); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, id := range report.GlobalFactIDs {
				if id == globalID {
					found = true
				}
			}
			if !found {
				t.Fatalf("missing global label: %s", out.String())
			}
			if len(report.FactsLocusDrift[globalID]) != 0 {
				t.Fatalf("global drift: %v", report.FactsLocusDrift)
			}
			if len(report.FactsLocusDrift[local[0].ID]) == 0 {
				t.Fatalf("local drift lost: %s", out.String())
			}
		} else if !strings.Contains(out.String(), "(global) orion convention") {
			t.Fatalf("missing text label: %s", out.String())
		}
	}
}

func TestCompactBriefLabelsGlobalFacts(t *testing.T) {
	opts, _ := retrievalSurfaceFixture(t)
	for _, format := range []brainBriefPacketFormat{brainBriefPacketCompactV1, brainBriefPacketCompactV2, brainBriefPacketCompactV3} {
		cmd := &cobra.Command{}
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&bytes.Buffer{})
		if err := runBrainBrief(context.Background(), cmd, opts, brainBriefOptions{limit: 10, packetFormat: format}, "orion"); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "Facts from the global store") {
			t.Fatalf("%s lost global scope: %s", format, out.String())
		}
	}
}

func TestUnifiedRetrievalChecksOnlyLocalLoci(t *testing.T) {
	opts, brainDir := retrievalSurfaceFixture(t)
	rememberGlobal(t, opts, "orion convention uses db/migrations/global.sql")
	globalID := factRecordID("orion convention uses db/migrations/global.sql", normalizeFactPaths([]string{"preferences.coding.style"}))
	globals, err := loadGlobalFacts(opts.Env)
	if err != nil {
		t.Fatal(err)
	}
	local, err := loadFacts(brainDir, "feature")
	if err != nil {
		t.Fatal(err)
	}
	local[0].Text = "orion local fact uses db/migrations/local.sql"
	if err := writeFacts(brainDir, "feature", local); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []retrievalMode{modeLexical, modeHybrid, modeVector} {
		results, err := retrieveUnifiedWithOptions(opts.Env.RepoRoot, brainDir, "feature", "orion", 10, mode,
			retrievalOptions{Source: retrievalSourceFact, GlobalFacts: globals})
		if err != nil {
			t.Fatal(err)
		}
		foundGlobal, foundLocal := false, false
		for _, result := range results {
			if result.ID == globalID {
				foundGlobal = true
				if result.VerificationRequired || len(result.Caveats) != 0 {
					t.Fatalf("mode %d: global fact falsely stale: %+v", mode, result)
				}
				if !strings.Contains(result.Heading, "(global)") {
					t.Fatalf("mode %d: global label missing: %+v", mode, result)
				}
			}
			if result.ID == local[0].ID {
				foundLocal = true
				if !result.VerificationRequired {
					t.Fatalf("mode %d: local drift lost: %+v", mode, result)
				}
			}
		}
		if !foundGlobal || !foundLocal {
			t.Fatalf("mode %d: missing facts: %+v", mode, results)
		}
	}
}

func TestRetrievalCommandsCanExcludeGlobalFactsPerRequest(t *testing.T) {
	opts, _ := retrievalSurfaceFixture(t)
	for _, use := range []string{"search", "query", "vsearch"} {
		for _, noGlobal := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/no-global=%v", use, noGlobal), func(t *testing.T) {
				mode := modeLexical
				if use == "query" {
					mode = modeHybrid
				}
				if use == "vsearch" {
					mode = modeVector
				}
				cmd := newRetrieveCommand(opts, use, mode, "test")
				var out bytes.Buffer
				cmd.SetOut(&out)
				cmd.SetErr(&bytes.Buffer{})
				cmd.SetArgs([]string{"orion", "--source=fact", "--branch=feature", "--json", fmt.Sprintf("--no-global=%v", noGlobal)})
				if err := cmd.Execute(); err != nil {
					t.Fatal(err)
				}
				hasGlobal := strings.Contains(out.String(), "global convention applies everywhere")
				if hasGlobal == noGlobal || !strings.Contains(out.String(), "local retrieval fact") {
					t.Fatalf("wrong scope: %s", out.String())
				}
			})
		}
	}
}

func TestMCPRetrievalCanExcludeGlobalFactsPerRequest(t *testing.T) {
	opts, _ := retrievalSurfaceFixture(t)
	for _, name := range []string{"brain_search", "brain_query", "brain_vsearch"} {
		for _, noGlobal := range []bool{false, true} {
			params, err := json.Marshal(mcpToolCallParams{Name: name, Arguments: map[string]any{
				"query": "orion", "source": "fact", "branch": "feature", "no_global": noGlobal,
			}})
			if err != nil {
				t.Fatal(err)
			}
			response := handleMCPMessage(context.Background(), opts, mcpMessage{JSONRPC: "2.0", ID: 1, Method: "tools/call", Params: params})
			if response.Error != nil {
				t.Fatalf("%s: %+v", name, response.Error)
			}
			encoded, err := json.Marshal(response.Result)
			if err != nil {
				t.Fatal(err)
			}
			hasGlobal := strings.Contains(string(encoded), "global convention applies everywhere")
			if hasGlobal == noGlobal || !strings.Contains(string(encoded), "local retrieval fact") {
				t.Fatalf("%s no_global=%v: wrong scope: %s", name, noGlobal, encoded)
			}
		}
	}
	if _, err := mcpRetrievalOptions(map[string]any{"no_global": "true"}, "feature"); err == nil {
		t.Fatal("no_global accepted a string instead of a boolean")
	}
}
