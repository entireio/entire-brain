package cli

import (
	"bytes"
	"context"
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

	if inARepository(context.Background(), opts) {
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

	if !inARepository(context.Background(), opts) {
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
