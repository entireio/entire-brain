package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// mcpWorkspaceToolText returns the payload of a successful workspace tool call,
// failing the test if the call was refused. Every test here is about a
// workspace that must still ANSWER, so a refusal is the failure mode.
func mcpWorkspaceToolText(t *testing.T, opts Options, tool string, args map[string]any) string {
	t.Helper()
	response := mcpScopeCall(t, opts, tool, args)
	if errObj, ok := response["error"]; ok {
		t.Fatalf("%s refused the workspace instead of answering: %+v", tool, errObj)
	}
	data, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestMCPWorkspaceToolsSurviveOneDeadMember: one broken member must not take
// down the workspace.
//
// Deleting ONE sibling checkout made all three workspace tools answer nothing
// at all -- for every other member, however healthy. The CLI has always
// degraded per member instead, so the agent-facing surface was the strictly
// worse one, and the refusal it produced recommended
// ENTIRE_BRAIN_MCP_ALLOW_CROSS_REPO -- the confused-deputy opt-out -- as the
// cure for a deleted directory.
func TestMCPWorkspaceToolsSurviveOneDeadMember(t *testing.T) {
	t.Setenv(mcpAllowCrossRepoEnv, "")
	for _, tool := range []string{"brain_workspace_graph", "brain_workspace_regressions", "brain_workspace_review"} {
		t.Run(tool, func(t *testing.T) {
			opts, boundDir, siblingKey := workspaceSiblingFixture(t, "dead")
			manifest, err := loadWorkspaceManifest(opts.Env, "dead")
			if err != nil {
				t.Fatal(err)
			}
			// In scope, registered, with a brain -- and no checkout on disk.
			// This is the state a workspace reaches the moment one sibling is
			// deleted, or the others are cloned onto a fresh machine first.
			deadDir := filepath.Join(filepath.Dir(boundDir), "deleted-sibling")
			deadKey := testLocalRepoStorageKey(t, deadDir)
			writeScopeTestBrain(t, opts.Env, deadKey)
			manifest.Repos = append(manifest.Repos, workspaceRepo{RepoKey: deadKey, Name: "deleted-sibling", LocalPathHint: deadDir})
			if err := writeWorkspaceManifest(opts.Env, manifest); err != nil {
				t.Fatal(err)
			}

			payload := mcpWorkspaceToolText(t, opts, tool, scopeToolArgs(tool, "dead"))

			if !strings.Contains(payload, siblingKey) {
				t.Fatalf("one dead member suppressed the healthy sibling %q: %s", siblingKey, payload)
			}
			// Reported AND skipped: naming the member is what keeps "two
			// results for a three-member workspace" from being something the
			// reader has to notice on their own.
			if !strings.Contains(payload, deadKey) {
				t.Fatalf("dead member %q was dropped silently: %s", deadKey, payload)
			}
			if !strings.Contains(payload, "skipped (unverified)") {
				t.Fatalf("dead member was not reported as skipped, with a reason: %s", payload)
			}
		})
	}
}

// TestMCPWorkspaceExcludesForgedMemberInsteadOfReadingItsBrain is the security
// half of the same change, and the reason an unverifiable member is DROPPED
// from the executed manifest rather than merely flagged.
//
// A member's repo_key addresses a brain, and brainDirForKey applies no
// containment -- so a forged key in workspace.json names any brain on the
// machine. The checkout is the only thing that grounds the key, and it is
// bounded by scopeRoot. A member whose key cannot be confirmed against an
// in-scope checkout must therefore never reach execution:
// buildWorkspaceGraphPayload opens that brain by key alone.
func TestMCPWorkspaceExcludesForgedMemberInsteadOfReadingItsBrain(t *testing.T) {
	t.Setenv(mcpAllowCrossRepoEnv, "")
	for _, tool := range []string{"brain_workspace_graph", "brain_workspace_regressions", "brain_workspace_review"} {
		t.Run(tool, func(t *testing.T) {
			opts, _, siblingKey := workspaceSiblingFixture(t, "forgery")
			manifest, err := loadWorkspaceManifest(opts.Env, "forgery")
			if err != nil {
				t.Fatal(err)
			}
			foreignKey := "gh/victim/other"
			victim := writeScopeTestBrain(t, opts.Env, foreignKey)
			// The sibling's entry keeps its real, in-scope checkout but claims
			// an unrelated repository's key.
			for i := range manifest.Repos {
				if manifest.Repos[i].RepoKey == siblingKey {
					manifest.Repos[i].RepoKey = foreignKey
				}
			}
			if err := writeWorkspaceManifest(opts.Env, manifest); err != nil {
				t.Fatal(err)
			}

			payload := mcpWorkspaceToolText(t, opts, tool, scopeToolArgs(tool, "forgery"))

			if !strings.Contains(payload, "skipped (unverified)") {
				t.Fatalf("forged member was not reported as skipped: %s", payload)
			}
			// The foreign brain is never opened.
			entries, err := os.ReadDir(victim)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.Contains(entry.Name(), "lock") {
					t.Fatalf("foreign brain was opened across the repo boundary: %s", entry.Name())
				}
			}
		})
	}
}

// TestMCPWorkspaceStillRefusesAMemberResolvingOutOfScope pins the line the
// prune must not cross. A member whose checkout RESOLVES outside the bound
// repository's parent is a confused-deputy request, not a degraded member, and
// stays a whole-call refusal: no per-member report makes it safe.
func TestMCPWorkspaceStillRefusesAMemberResolvingOutOfScope(t *testing.T) {
	t.Setenv(mcpAllowCrossRepoEnv, "")
	opts, boundDir, _ := workspaceSiblingFixture(t, "escape")
	outside := t.TempDir() // a different tree entirely, never under the parent
	escapeDir := filepath.Join(filepath.Dir(boundDir), "looks-local")
	if err := os.Symlink(outside, escapeDir); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	manifest, err := loadWorkspaceManifest(opts.Env, "escape")
	if err != nil {
		t.Fatal(err)
	}
	escapeKey := testLocalRepoStorageKey(t, outside)
	writeScopeTestBrain(t, opts.Env, escapeKey)
	manifest.Repos = append(manifest.Repos, workspaceRepo{RepoKey: escapeKey, LocalPathHint: escapeDir})
	if err := writeWorkspaceManifest(opts.Env, manifest); err != nil {
		t.Fatal(err)
	}
	message := mcpScopeErrorMessage(t, mcpScopeCall(t, opts, "brain_workspace_graph", scopeToolArgs("brain_workspace_graph", "escape")))
	if !strings.Contains(message, "outside") {
		t.Fatalf("a member resolving out of scope must refuse the whole call: %s", message)
	}
}

// workspaceCoverageFixture builds a two-member workspace in which exactly one
// member cannot be scanned: it is registered and has a brain, but its checkout
// is not on disk, so nothing ever compares that brain to a tree.
func workspaceCoverageFixture(t *testing.T, name string) (EntireEnv, *cobra.Command) {
	t.Helper()
	env := semanticTestEnv(t, t.TempDir())
	session := `{"text":"in pkg/review_context.go the scope diff uses scopeBaseRef+\"..HEAD\" for the range"}`
	clean := "package x\nfunc f() string {\n\treturn scopeBaseRef + \"..HEAD\"\n}\n"
	repoDir, key := writeLocalWorkspaceBrainRepo(t, env, session, "pkg/review_context.go", clean)

	deadDir := filepath.Join(t.TempDir(), "never-cloned")
	deadKey := testLocalRepoStorageKey(t, deadDir)
	writeWorkspaceBrainRepoAt(t, env, deadKey, repoDir, session, "pkg/review_context.go", clean)

	manifest := workspaceManifest{
		SchemaVersion: workspaceSchemaVersion,
		Name:          name,
		Repos: []workspaceRepo{
			{RepoKey: key, Name: "healthy", LocalPathHint: repoDir},
			{RepoKey: deadKey, Name: "never-cloned", LocalPathHint: deadDir},
		},
	}
	if err := writeWorkspaceManifest(env, manifest); err != nil {
		t.Fatal(err)
	}
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}
	return env, NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})
}

// TestWorkspaceRegressionsHeadlineCountsOnlyScannedRepos: the all-clear line is
// a claim about COVERAGE, and it used to be a claim about membership.
//
// "No suspected regressions across 2 repo(s)" was printed with len(results) --
// which counts the members that were skipped unsafe or whose checkout never
// resolved. A workspace with dead members read as every repo inspected and
// every one clean.
func TestWorkspaceRegressionsHeadlineCountsOnlyScannedRepos(t *testing.T) {
	_, cmd := workspaceCoverageFixture(t, "coverage")
	out, err := execute(t, cmd, "workspace", "inspect", "regressions", "coverage", "scopeBaseRef base scope")
	if err != nil {
		t.Fatalf("workspace regressions: %v", err)
	}
	if strings.Contains(out, "across 2 repo(s)") {
		t.Fatalf("headline counted an unscanned member as scanned: %s", out)
	}
	if !strings.Contains(out, "not scanned") {
		t.Fatalf("headline did not disclose the unscanned member: %s", out)
	}
}

// TestWorkspaceReviewHeadlineCountsOnlyReviewedRepos is the same claim for the
// review headline's "%d/%d repo(s)" denominator.
func TestWorkspaceReviewHeadlineCountsOnlyReviewedRepos(t *testing.T) {
	_, cmd := workspaceCoverageFixture(t, "coverage2")
	out, err := execute(t, cmd, "workspace", "review", "coverage2", "scopeBaseRef base scope")
	if err != nil {
		t.Fatalf("workspace review: %v", err)
	}
	if strings.Contains(out, "/2 repo(s)") {
		t.Fatalf("review headline counted an unreviewed member in its denominator: %s", out)
	}
	if !strings.Contains(out, "were not reviewed") {
		t.Fatalf("review headline did not disclose the unreviewed member: %s", out)
	}
}

// TestWorkspaceRegressionsWarningCarriesItsRepoHeader: an unattributed warning
// line in a multi-repo listing is read as belonging to the next repo printed.
//
// The header was emitted for anomalies, errors and a non-ok state -- but not
// for warnings. A repo that scanned cleanly and carried only a caveat ("no code
// identifiers found in query", "semantic index missing -- scanned raw sessions
// only") printed that caveat with nothing above it, so it landed under the NEXT
// repo's heading and qualified the wrong repo's findings. review's equivalent
// condition has always included the warnings term.
func TestWorkspaceRegressionsWarningCarriesItsRepoHeader(t *testing.T) {
	// The shape that lost its header: scanned, clean, ok -- and warned.
	if !workspaceRepoBlockNeedsHeader("ok", 0, 1, "") {
		t.Fatal("a clean repo carrying only a warning printed that warning with no repo header above it")
	}
	// The rest of the rule, so tightening one term cannot loosen another.
	for _, tc := range []struct {
		state     string
		anomalies int
		warnings  int
		err       string
		want      bool
	}{
		{"ok", 0, 0, "", false}, // nothing to attribute: no header
		{"ok", 1, 0, "", true},
		{"ok", 0, 0, "boom", true},
		{"degraded", 0, 0, "", true},
		{"unknown", 0, 0, "", true},
	} {
		if got := workspaceRepoBlockNeedsHeader(tc.state, tc.anomalies, tc.warnings, tc.err); got != tc.want {
			t.Fatalf("workspaceRepoBlockNeedsHeader(%q, %d, %d, %q) = %v, want %v",
				tc.state, tc.anomalies, tc.warnings, tc.err, got, tc.want)
		}
	}
}

// TestWorkspaceAddReportsAnExistingMemberRatherThanClaimingItAdded.
//
// Two paths reaching one repo key is routine -- a git worktree of a member
// carries the same origin and therefore the same key, and so does the same
// checkout named through a symlink. Every such `workspace add` printed
// "added <key>", implying a new member, while the member count did not move,
// the earlier --name was replaced, and the recorded path silently moved to the
// newly named tree, which is the tree every later scan runs against.
func TestWorkspaceAddReportsAnExistingMemberRatherThanClaimingItAdded(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	parent := t.TempDir()
	first := filepath.Join(parent, "checkout")
	second := filepath.Join(parent, "worktree")
	for _, dir := range []string{first, second} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Both directories answer with one shared origin, exactly as a worktree and
	// its main checkout do, so both resolve to a single repo key.
	runner := &workspaceSharedOriginRunner{}
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})

	out, err := execute(t, cmd, "workspace", "add", "dup", first, "--name", "main")
	if err != nil {
		t.Fatalf("first add: %v", err)
	}
	if !strings.Contains(out, "added ") {
		t.Fatalf("first add should report an addition: %s", out)
	}
	out, err = execute(t, cmd, "workspace", "add", "dup", second, "--name", "wt")
	if err != nil {
		t.Fatalf("second add: %v", err)
	}
	if strings.Contains(out, "added ") {
		t.Fatalf("re-adding an existing member reported a new addition: %s", out)
	}
	if !strings.Contains(out, "already a member") {
		t.Fatalf("re-adding an existing member did not say so: %s", out)
	}
	if !strings.Contains(out, second) {
		t.Fatalf("the rebound checkout, which every later scan uses, was not named: %s", out)
	}
	manifest, err := loadWorkspaceManifest(env, "dup")
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Repos) != 1 {
		t.Fatalf("expected one member after two adds of one repository, got %d", len(manifest.Repos))
	}
}

// TestWorkspaceImportTargetPrefersCodeOverDocumentation.
//
// A repository's symbol table carries its Markdown headings as `section`
// symbols. `section` fell into the same rank as every real code symbol, and the
// tie was then broken by file path -- where README.md sorts ahead of
// client/client.go. So a ROOT-package import (no subpath to match a symbol name
// against) resolved to the target repository's README heading in preference to
// its actual API. The edge exists to tell an agent where a cross-repo
// dependency lands, so a documentation target is worse than none: real,
// plausible, and never the code that would have to change.
func TestWorkspaceImportTargetPrefersCodeOverDocumentation(t *testing.T) {
	candidates := []workspaceGraphSymbolRef{
		{ID: "r:Markdown:README.md:section:api-client", Kind: "section", Name: "api-client", QualifiedName: "api-client", FilePath: "README.md"},
		{ID: "r:Go:client/client.go:type:Client", Kind: "type", Name: "Client", QualifiedName: "Client", FilePath: "client/client.go"},
	}
	// A root import -- `@owner/api-client` -- whose subpath is empty.
	got, ok := workspaceImportTargetCandidate(candidates, "")
	if !ok {
		t.Fatal("root import resolved to no target at all")
	}
	if got.FilePath == "README.md" {
		t.Fatalf("root import resolved to documentation instead of code: %+v", got)
	}
	if got.Name != "Client" {
		t.Fatalf("root import target = %+v, want the code symbol Client", got)
	}
	// Documentation is ranked last, not filtered: a docs-only repository has
	// nothing better to offer and must still resolve.
	got, ok = workspaceImportTargetCandidate(candidates[:1], "")
	if !ok || got.FilePath != "README.md" {
		t.Fatalf("a docs-only repository must still resolve: %+v ok=%v", got, ok)
	}
}

// TestWorkspaceReviewWillNotCallATreeCleanWithoutComparingIt.
//
// The per-repo verdict came from a default: branch that never asked whether
// anything had been compared:
//
//	result.Summary = "no suspected regressions (current tree matches the brain's memory)."
//
// detectRegressionAnomalies returns the number of files it read for exactly
// this purpose, and the caller discarded it with `_`. So a brain with no
// distilled assertions, and a query naming nothing the detector could anchor,
// both produced a sentence asserting a comparison that never ran -- printed
// directly above the warning saying it had not. `summary` is what an agent or
// script reads.
func TestWorkspaceReviewWillNotCallATreeCleanWithoutComparingIt(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})
	// A brain whose history holds no precise assertion about the query, so the
	// detector anchors nothing and opens no file.
	repoDir, key := writeLocalWorkspaceBrainRepo(t, env,
		`{"text":"we talked about the deployment schedule"}`,
		"pkg/review_context.go", "package x\n")
	manifest := workspaceManifest{
		SchemaVersion: workspaceSchemaVersion,
		Name:          "unscanned",
		Repos:         []workspaceRepo{{RepoKey: key, Name: "quiet", LocalPathHint: repoDir}},
	}
	if err := writeWorkspaceManifest(env, manifest); err != nil {
		t.Fatal(err)
	}
	out, err := execute(t, cmd, "workspace", "review", "unscanned", "scopeBaseRef base scope", "--json")
	if err != nil {
		t.Fatalf("workspace review: %v", err)
	}
	var payload struct {
		Summary string                  `json:"summary"`
		Results []workspaceReviewResult `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("parse review json: %v\n%s", err, out)
	}
	if len(payload.Results) != 1 {
		t.Fatalf("expected one member, got %d", len(payload.Results))
	}
	r := payload.Results[0]
	if r.FilesScanned != 0 {
		t.Fatalf("fixture compared %d files, want an unscanned member", r.FilesScanned)
	}
	if r.Checked {
		t.Fatalf("checked=true for a repo where no file was read: %+v", r)
	}
	if strings.Contains(r.Summary, "matches the brain's memory") {
		t.Fatalf("summary claimed a comparison that never ran: %q", r.Summary)
	}
	if !strings.Contains(r.Summary, "INCONCLUSIVE") {
		t.Fatalf("an unscanned repo must be reported INCONCLUSIVE: %q", r.Summary)
	}
	// And the workspace headline must not read as a clean sweep either.
	if !strings.Contains(payload.Summary, "INCONCLUSIVE") {
		t.Fatalf("workspace headline read as a clean sweep over an unscanned member: %q", payload.Summary)
	}
}

// TestWorkspaceReviewStillReportsAGenuinelyCleanTree is the other half: when
// files WERE compared and nothing was wrong, that is a real clean result and
// must keep saying so -- with the count, so the claim carries its own evidence.
func TestWorkspaceReviewStillReportsAGenuinelyCleanTree(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})
	session := `{"text":"in pkg/review_context.go the scope diff uses scopeBaseRef+\"..HEAD\" for the range"}`
	clean := "package x\nfunc f() string {\n\treturn scopeBaseRef + \"..HEAD\"\n}\n"
	repoDir, key := writeLocalWorkspaceBrainRepo(t, env, session, "pkg/review_context.go", clean)
	manifest := workspaceManifest{
		SchemaVersion: workspaceSchemaVersion,
		Name:          "scanned",
		Repos:         []workspaceRepo{{RepoKey: key, Name: "clean", LocalPathHint: repoDir}},
	}
	if err := writeWorkspaceManifest(env, manifest); err != nil {
		t.Fatal(err)
	}
	out, err := execute(t, cmd, "workspace", "review", "scanned", "fix scopeBaseRef base scope", "--json")
	if err != nil {
		t.Fatalf("workspace review: %v", err)
	}
	var payload struct {
		Summary string                  `json:"summary"`
		Results []workspaceReviewResult `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("parse review json: %v\n%s", err, out)
	}
	r := payload.Results[0]
	if !r.Checked || r.FilesScanned == 0 {
		t.Fatalf("a repo that was compared must report it: %+v", r)
	}
	if strings.Contains(r.Summary, "INCONCLUSIVE") {
		t.Fatalf("a genuinely compared clean tree must not be called inconclusive: %q", r.Summary)
	}
	if !strings.Contains(r.Summary, "no suspected regressions") {
		t.Fatalf("summary = %q", r.Summary)
	}
}

// TestWorkspaceReviewSummaryVerdict pins the rule both callers share.
func TestWorkspaceReviewSummaryVerdict(t *testing.T) {
	if got := workspaceReviewSummary(false, 0); !strings.Contains(got, "INCONCLUSIVE") {
		t.Fatalf("nothing compared must be inconclusive: %q", got)
	}
	// A zero file count is inconclusive even if a caller says it checked: the
	// count is the evidence, and the two must never disagree in the clean
	// direction.
	if got := workspaceReviewSummary(true, 0); !strings.Contains(got, "INCONCLUSIVE") {
		t.Fatalf("zero files compared must be inconclusive: %q", got)
	}
	got := workspaceReviewSummary(true, 3)
	if strings.Contains(got, "INCONCLUSIVE") || !strings.Contains(got, "3 file(s)") {
		t.Fatalf("a real comparison must report itself with its count: %q", got)
	}
}

// TestWorkspaceRegressionsHeadlineWillNotCallAWorkspaceCleanWithoutScanningIt:
// the regressions all-clear has the same duty as the review summary. A run in
// which no repo compared a single file is not "no suspected regressions" -- it
// is a run that never looked.
func TestWorkspaceRegressionsHeadlineWillNotCallAWorkspaceCleanWithoutScanningIt(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})
	// Two members, neither of whose history anchors the query, so neither scans.
	quiet := `{"text":"we talked about the deployment schedule"}`
	rA, kA := writeLocalWorkspaceBrainRepo(t, env, quiet, "pkg/a.go", "package a\n")
	rB, kB := writeLocalWorkspaceBrainRepo(t, env, quiet, "pkg/b.go", "package b\n")
	manifest := workspaceManifest{
		SchemaVersion: workspaceSchemaVersion,
		Name:          "allquiet",
		Repos: []workspaceRepo{
			{RepoKey: kA, Name: "a", LocalPathHint: rA},
			{RepoKey: kB, Name: "b", LocalPathHint: rB},
		},
	}
	if err := writeWorkspaceManifest(env, manifest); err != nil {
		t.Fatal(err)
	}
	out, err := execute(t, cmd, "workspace", "inspect", "regressions", "allquiet", "scopeBaseRef base scope")
	if err != nil {
		t.Fatalf("workspace regressions: %v", err)
	}
	if strings.Contains(out, "No suspected regressions across") {
		t.Fatalf("an all-clear was printed over a workspace where nothing was scanned:\n%s", out)
	}
	if !strings.Contains(out, "INCONCLUSIVE") {
		t.Fatalf("a workspace where nothing was scanned must be reported INCONCLUSIVE:\n%s", out)
	}
}

// workspaceSharedOriginRunner makes every directory resolve to one repository,
// the way a git worktree and its main checkout share an origin remote.
type workspaceSharedOriginRunner struct{}

func (r *workspaceSharedOriginRunner) Run(_ context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
	if name == "git" {
		switch strings.Join(args, " ") {
		case "rev-parse --show-toplevel":
			return []byte(dir + "\n"), nil, nil
		case "remote get-url origin":
			return []byte("https://github.com/fleet/shared.git\n"), nil, nil
		}
	}
	return nil, nil, errWorkspaceTestCommandUnsupported
}

var errWorkspaceTestCommandUnsupported = errorString("command not scripted for this test")

type errorString string

func (e errorString) Error() string { return string(e) }
