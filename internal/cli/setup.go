package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/entireio/entire-brain/internal/cli/internal/cmdutil"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/entireio/entire-brain/internal/agentsetup"
	"github.com/entireio/entire-brain/internal/cli/command/watch"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/workspace"
	"github.com/entireio/entire-brain/internal/providerdetail"
	"github.com/entireio/entire-brain/internal/setupui"
	"github.com/spf13/cobra"
)

// setup.go is the one-command onboarding path. Before it there were three
// separate ways to stand a brain up — build it, build it with an agent, extract
// facts — and the only one that made the brain useful immediately was the one
// people skipped, while the one that took O(sessions) minutes was the one they
// ran in the foreground and then forgot to ever run again.
//
// `entire-brain setup` collapses that into three phases with honest cost labels:
//
//  1. INSTANT (blocking, seconds, $0): the deterministic core — session export,
//     semantic index, seed, docs, history. This is the SAME free path `watch`
//     runs every tick with seed agent "none". When it returns, the brain is
//     queryable.
//  2. BACKFILL (detached, spends tokens): distill over past sessions, NEWEST
//     FIRST so the most useful facts land first, on the cheap model/effort,
//     capped at --backfill-budget sessions per pass, with the persisted distill
//     cache so a re-run never re-spends. It is detached, so the prompt comes
//     back immediately.
//  3. DAEMON (installed once, machine-wide): the repo joins a workspace and a
//     single `workspace watch` service keeps every member fresh. Deterministic
//     refresh is free; the agent step stays gated by --distill-every and the
//     per-repo watch cursor.
//
// Every phase is idempotent. Running setup twice must produce the same machine,
// not two daemons and two workspace entries.
const (
	setupDefaultWorkspace = "default"
	// setupBackfillDefaultEffort keeps the background token spend cheap by
	// default. Model is deliberately NOT defaulted to a hard-coded name — model
	// ids age badly — so the agent's own default applies unless --model is given.
	setupBackfillDefaultEffort = "low"
	setupBackfillStateFile     = "backfill.json"
	setupBackfillLogFile       = "backfill.log"
	setupBackfillStateVersion  = 1
	// setupRecordFile remembers which workspace and daemon identity this repo
	// was set up with, so `status` reports on the daemon that actually exists
	// rather than on the default one it would have created.
	setupRecordFile = "setup.json"
	// setupDefaultBackfillBudget caps how many sessions a bare `entire-brain
	// setup` distills in its FIRST background pass. An uncapped default meant
	// one unadorned command could spend the whole corpus — years of sessions —
	// before anyone saw a cost line. 25 newest sessions is enough to make the
	// facts layer useful immediately; the rest arrive on later runs (or all at
	// once with the explicit --backfill-budget 0).
	setupDefaultBackfillBudget = 25
	// setupDaemonLogFile is machine-level ON PURPOSE. A per-repo path inside a
	// machine-wide unit made the rendered plist repo-dependent, so every new
	// repo's setup saw Contents != Current and unloaded/reloaded the running
	// daemon — restart ping-pong plus false "not current" status.
	setupDaemonLogFile = "watch.log"
	// setupInstantRecordFile remembers the last instant phase's per-component
	// outcome so `status` can say semantic=failed instead of semantic=missing
	// and `doctor` can print WHY without rebuilding anything.
	setupInstantRecordFile = "instant.json"
)

// brainComponent* are the instant phase's component ids. They are the same
// vocabulary `status` prints on its instant line, so a component reported as
// failed by setup is the component the reader then finds as failed there.
const (
	brainComponentSessions       = "sessions"
	brainComponentSeed           = string(brainstore.DeclaredIndexSeed)
	brainComponentDocs           = string(brainstore.DeclaredIndexDocs)
	brainComponentSemantic       = "semantic"
	brainComponentHistory        = string(brainstore.DeclaredIndexHistory)
	brainComponentHistoryVectors = "history-vectors"
	brainComponentBranches       = "branches"
	brainComponentFacts          = "facts"
	brainComponentPatterns       = "patterns"
	brainComponentMemory         = "memory"
	brainComponentEntities       = "entities"
)

// setupComponentLabels name each component the way the setup lines say it.
var setupComponentLabels = map[string]string{
	brainComponentSessions:       "session export",
	brainComponentSeed:           brainstore.DeclaredIndexSeed.Label(),
	brainComponentDocs:           brainstore.DeclaredIndexDocs.Label(),
	brainComponentSemantic:       "code index",
	brainComponentHistory:        brainstore.DeclaredIndexHistory.Label(),
	brainComponentHistoryVectors: "history vectors",
	brainComponentBranches:       "branch overlays",
	brainComponentFacts:          "fact reclassification",
	brainComponentPatterns:       "pattern layer",
	brainComponentMemory:         "memory coordinator",
	brainComponentEntities:       "entity index",
}

// setupRetrievalCore is the pair of components a QUERY reads. The semantic
// index is what `inspect code`, `query` and `brief` search; the seed baseline
// is the repository overview they fall back to. Every other component enriches
// an answer -- these two are what there is to answer FROM.
var setupRetrievalCore = []string{brainComponentSemantic, brainComponentSeed}

// setupUnqueryableCore returns the labels of the retrieval-core components when
// EVERY one of them failed, and nil otherwise.
//
// It exists because "Brain ready (degraded) ... the brain is queryable now" was
// printed, at exit 0, for a brain with no code index and no baseline: setup
// treats one surviving component as success, which is right for the exit code
// and wrong for that sentence. The exit code is deliberately NOT changed here
// -- a brain that captured sessions but could not index the tree is still worth
// keeping -- only the claim is.
//
// A component the phase never reported is NOT evidence of a failure: callers
// predating per-component reporting hand back no components at all, and reading
// that as "everything is gone" would cry wolf on every one of them.
// setupQueryabilityClause is the tail of the Build phase line, and it must
// never assert queryability this brain does not have.
//
// The phase line is `--verbose` output, so it is the one surface that still
// names the missing SOURCES rather than the lost capability. A brain with no
// index and no baseline answers nothing about this repository, and saying
// "the brain is queryable now" over it is the sentence this guard removes.
func setupQueryabilityClause(components []setupComponent) string {
	unavailable := setupUnqueryableCore(components)
	if len(unavailable) == 0 {
		return "the brain is queryable now"
	}
	return "no " + strings.Join(unavailable, " and no ") + ", so queries have nothing to read for this repo yet"
}

func setupUnqueryableCore(components []setupComponent) []string {
	states := make(map[string]string, len(components))
	for _, component := range components {
		states[component.Name] = component.State
	}
	labels := make([]string, 0, len(setupRetrievalCore))
	for _, name := range setupRetrievalCore {
		// Skipped counts as unavailable here: whoever caused the absence,
		// a skipped source answers queries exactly as well as a failed one.
		if state := states[name]; state != "failed" && state != "skipped" {
			return nil
		}
		labels = append(labels, setupComponentLabel(name))
	}
	return labels
}

// setupRepoKeyMismatchHint covers what is LEFT of the repo-key mismatch once
// the two tools' naming conventions no longer collide.
//
// It used to say "this repo has no git remote; add one, or upgrade the entire
// CLI" — advice that was wrong in both directions. It fired on repositories
// that DID have a remote (any origin that is not github.com: the provider names
// those `local/<basename>` while the brain names them `gl/…`, `bb/…`), it
// blamed the reader's repository for a disagreement between two of our own
// tools, and no upgrade of either tool would have made the two spellings meet.
// That whole class is now reconciled at ingest (semantic_repokey.go), so a
// mismatch that still reaches here means the snapshot genuinely describes some
// other repository — in practice a provider snapshot cached against an earlier
// remote — and rebuilding it is the thing to do.
func setupRepoKeyMismatchHint(brainCmd string) string {
	return "the snapshot names a different repository than this one, usually a provider snapshot cached before this repo's origin changed; rebuild it with `" +
		base.SetupBrainCommand(brainCmd) + " refresh index --force`"
}

// setupDirtyWorktreeHint is the ONE remedy resolver for an uncommitted working
// tree. Every surface that tells a reader what to do about a dirty tree calls
// it, and no other surface offers a remedy of its own.
//
// It is the second thing a first run hits, and it is caused by the step the
// docs put immediately before setup. `entire enable` writes
// .entire/settings.json and the agent's hook settings (.claude/settings.json)
// and does NOT commit them, so the very next `entire-brain setup` finds a dirty
// worktree and refuses to seed or index it. The error text names --worktree,
// which `setup` does not accept -- leaving the reader with a flag they cannot
// pass to the command they ran.
//
// It names BOTH ways out, because there are two and a reader handed one of them
// has no way to discover the other. That is also why it is a single resolver:
// `status` printed this remedy in its verdict while the freshness cause
// directly underneath said "refresh with --worktree to include current
// changes", so one screen gave two contradictory instructions. The cause now
// states the cause only (see buildBrainRetrievalStatus) and the remedy comes
// from here.
func setupDirtyWorktreeHint(brainCmd string) string {
	return "commit or stash the working tree -- or list the paths in .brainignore, which the repository context and code index checks both honour -- then re-run `" + base.SetupBrainCommand(brainCmd) +
		" setup`, or `" + base.SetupBrainCommand(brainCmd) +
		" refresh --worktree` to index the uncommitted changes as they are; `entire enable` writes .entire/ and .claude/ without committing them, which is what a first run usually trips over"
}

// setupGraphStoreUnsupportedHint is the ONE remedy for an installed Graph that
// predates the read-only store contract (graph_store.go). Refresh already
// forgives this state as degraded-optional — "Graph is encouraged, never
// required" — and docs/graph-owned-store.md names the remedy: upgrade Graph.
// No refresh, commit, or stash changes anything until the plugin is upgraded,
// so no other remedy may be offered for it.
func setupGraphStoreUnsupportedHint(brainCmd string) string {
	return "the installed Graph predates the read-only store interface, so code inspection is skipped and every other brain source still builds; upgrade Graph with `entire plugin install graph`, then run `" +
		base.SetupBrainCommand(brainCmd) + " " + base.SemanticIndexVerb + "`"
}

// setupIsGraphStoreUnsupported matches the two upgrade-Graph store sentinels in
// graph_store.go (errGraphStoreUnsupported, errGraphStoreContractOlder) from a
// PERSISTED component detail, where the error value is gone and only its text
// survives. Both sentinels, and only those two, say "store contract" and
// "upgrade Graph"; errGraphStoreContractNewer says "upgrade entire-brain" and
// deliberately does not match — that one is this build's fault, and treating
// it as an optional skip would hide a brain that needs upgrading.
func setupIsGraphStoreUnsupported(detail string) bool {
	return strings.Contains(detail, "store contract") && strings.Contains(detail, "upgrade Graph")
}

// setupNoSessionsHint covers the state EVERY repo is in between `entire enable`
// and its first finished agent session: Entire has no checkpoints to hand over,
// and when the checkpoint remote is also unreachable (offline, or not yet
// authenticated) the brain cannot prove the inventory is empty rather than
// merely unreadable, so it reports a failure. "no readable checkpoint IDs from
// an incomplete persistent-store inventory" is true and useless; this says what
// it means and what the reader does about it.
func setupNoSessionsHint(brainCmd string) string {
	return "no captured sessions are readable yet — if no agent session has finished in this repo since `entire enable`, that is expected: work in a session, then re-run `" +
		base.SetupBrainCommand(brainCmd) + " setup`; `entire checkpoint list` shows what Entire itself can see"
}

// setupBuildsHistoryProjection and watchTickBuildsHistoryProjection are the one
// place the two callers of the free deterministic refresh disagree.
//
// A watch TICK leaves the durable history projection to the memory coordinator:
// it runs every few minutes, the per-tick delta already makes new conversations
// recallable, and re-projecting on every tick would bypass the work ledger.
//
// `setup` is not a tick. It runs once, and it ends by telling the user to run
// `entire-brain brief` — which, with no projection, drops its transcript half
// and answers "history index missing; run `entire brain refresh`". Setup's own
// recommended next command reporting a gap in setup's own output is the first
// impression this whole path exists to get right.
const (
	setupBuildsHistoryProjection     = true
	watchTickBuildsHistoryProjection = false
)

// setupComponent is one instant-phase component and how it ended.
type setupComponent struct {
	Name   string `json:"name"`
	State  string `json:"state"` // ok | failed
	Detail string `json:"detail,omitempty"`
	Hint   string `json:"hint,omitempty"`
}

func (c setupComponent) failed() bool { return c.State == "failed" }

func setupComponentLabel(name string) string {
	if label, ok := setupComponentLabels[name]; ok {
		return label
	}
	return name
}

// brainSourceDisplayName translates the internal seed id only at display sites.
// JSON keys, component names, and freshness axis ids retain their stored names.
func brainSourceDisplayName(name string) string {
	if name == brainComponentSeed {
		return setupComponentLabel(name)
	}
	return name
}

// newSetupComponent turns one stage outcome into a reportable component,
// attaching the hint for the failure modes whose error text is not actionable
// on its own.
func newSetupComponent(name string, err error, brainCmd string) setupComponent {
	if err == nil {
		return setupComponent{Name: name, State: "ok"}
	}
	// An installed Graph without the read-only store contract is a designed
	// optional state, not a failure: refresh forgives it (semanticForgiven),
	// docs/graph-owned-store.md calls it "unsupported for code inspection",
	// and every other brain source builds without it. Recording it as "failed"
	// put a hard ✗ on the status row and counted "semantic failed" into a
	// verdict whose remedy (a dirty-tree commit, a refresh) cannot fix it.
	if errors.Is(err, errGraphStoreUnsupported) || errors.Is(err, errGraphStoreContractOlder) {
		return setupComponent{Name: name, State: "skipped", Detail: providerdetail.Flatten(err.Error()), Hint: setupGraphStoreUnsupportedHint(brainCmd)}
	}
	// Flatten is the CLASS guard, applied where EVERY component's detail is
	// born rather than per component: a stage that shells out to another
	// program hands us that program's whole stderr, and an unsummarised usage
	// dump printed here buries setup's real report under sixty lines of
	// someone else's help screen. A producer that already summarised passes
	// through untouched; see providerdetail.Flatten.
	component := setupComponent{Name: name, State: "failed", Detail: providerdetail.Flatten(err.Error())}
	switch {
	case setupIsRepoKeyMismatch(component.Detail):
		component.Hint = setupRepoKeyMismatchHint(brainCmd)
		component.Detail = "repo key mismatch: " + component.Detail
	case setupIsDirtyWorktree(component.Detail):
		component.Hint = setupDirtyWorktreeHint(brainCmd)
	case setupIsNoReadableSessions(component.Detail):
		component.Hint = setupNoSessionsHint(brainCmd)
	}
	return component
}

func setupIsRepoKeyMismatch(detail string) bool {
	return strings.Contains(detail, "repo_key") && strings.Contains(detail, "does not match current repo")
}

// setupIsDirtyWorktree matches the machine-readable prefix both refusals carry
// (seed.go and semantic.go), not their prose, so rewording either message
// cannot silently drop the hint.
func setupIsDirtyWorktree(detail string) bool {
	return strings.Contains(detail, brainstore.DirtyWorktreeCode+":")
}

// setupIsNoReadableSessions matches the export failure raised when nothing is
// enumerable AND the inventory could not be proven complete. The narrower
// "routed discovery failed" variants keep their own detail: those name a real
// fault to fix, while this one is usually just a repo with no sessions yet.
func setupIsNoReadableSessions(detail string) bool {
	return strings.Contains(detail, checkpointScopeIncompleteCode+":") &&
		strings.Contains(detail, "no readable checkpoint IDs")
}

// partitionSetupComponents splits the phase's outcome into what built, what was
// deliberately skipped, and what failed, in report order. Skipped is its own
// bucket because it is neither of the others: calling it built claims a source
// that does not exist, and calling it failed turns a designed optional state
// into an error every downstream surface then reports.
func partitionSetupComponents(components []setupComponent) (built, skipped, failed []setupComponent) {
	for _, component := range components {
		switch {
		case component.failed():
			failed = append(failed, component)
		case component.State == "skipped":
			skipped = append(skipped, component)
		default:
			built = append(built, component)
		}
	}
	return built, skipped, failed
}

// setupComponentFailureLine is the one line a degraded component gets: what
// failed, why, that setup is continuing anyway, and where the detail lives.
// dash is passed in rather than written inline because an em dash is a
// non-ASCII glyph and this line is printed to a terminal whose locale may not
// be able to draw one.
func setupComponentFailureLine(component setupComponent, dash, brainCmd string) string {
	detail := strings.TrimSpace(component.Detail)
	if detail == "" {
		detail = "no detail reported"
	}
	// A skipped component is a designed state, not a breakage: say "skipped",
	// and skip the doctor referral — the detail and the hint below it already
	// carry the whole remedy.
	if component.State == "skipped" {
		return fmt.Sprintf("%s skipped (%s)", setupComponentLabel(component.Name), detail)
	}
	// A hint that ends by naming `setup` closes a referral loop if this line
	// also names `doctor`: doctor re-renders this same component with this
	// same hint appended, so the two commands point at each other and neither
	// adds a step. Where the hint names a DIFFERENT remedy (the repo-key skew
	// names `refresh index --force`) there is no circle and doctor keeps its
	// referral: it really is where the rest of the detail lives.
	if setupHintReferralLoops(component.Hint, brainCmd) {
		return fmt.Sprintf("%s failed (%s) %s continuing",
			setupComponentLabel(component.Name), detail, dash)
	}
	return fmt.Sprintf("%s failed (%s) %s continuing; run '%s doctor' for detail",
		setupComponentLabel(component.Name), detail, dash, base.SetupBrainCommand(brainCmd))
}

// setupHintReferralLoops reports a hint whose own remedy is `setup` -- the
// command the reader is already running. Pointing them at `doctor` on top of
// that is the closed referral: doctor's only answer for this component is this
// same hint, which sends them straight back here.
func setupHintReferralLoops(hint, brainCmd string) bool {
	hint = strings.TrimSpace(hint)
	if hint == "" {
		return false
	}
	return strings.Contains(hint, base.SetupBrainCommand(brainCmd)+" setup")
}

// setupComponentCountSummary is the Build phase's clause, and it COUNTS where
// the old one listed.
//
// The nine component names used to be rendered three times in one run: as a
// cryptic tick run beside the spinner, again inside this sentence, and a third
// time in the summary block. Nobody acted on any of the three. What a reader
// acts on is "did everything build, and if not, which one" — so the healthy
// case is a count and a failure is named, because one name is a fact and nine
// are a list.
func setupComponentCountSummary(built, skipped, failed []setupComponent) string {
	total := len(built) + len(skipped) + len(failed)
	if total == 0 {
		// The pre-component contract: an injected step or an older build
		// reports nothing and built fine.
		return "deterministic core built"
	}
	if len(failed) == 0 && len(skipped) == 0 {
		return fmt.Sprintf("%d %s", total, setupComponentNoun(total))
	}
	// Labels, not raw ids: every other line setup prints calls a component
	// by setupComponentLabel, and "sessions" here beside "session export"
	// below is one component answering to two names in one run.
	clause := fmt.Sprintf("%d of %d %s built", len(built), total, setupComponentNoun(total))
	if len(skipped) > 0 {
		labels := make([]string, 0, len(skipped))
		for _, component := range skipped {
			labels = append(labels, setupComponentLabel(component.Name))
		}
		clause += ", skipped " + strings.Join(labels, ", ")
	}
	if len(failed) > 0 {
		labels := make([]string, 0, len(failed))
		for _, component := range failed {
			labels = append(labels, setupComponentLabel(component.Name))
		}
		clause += ", FAILED " + strings.Join(labels, ", ")
	}
	return clause
}

func setupComponentNoun(n int) string {
	if n == 1 {
		return "component"
	}
	return "components"
}

// setupComponentFailureDetails are the continuation rows a degraded Build
// phase is allowed: what failed, why, that setup is continuing anyway, and the
// hint for the failure modes whose error text is not actionable on its own.
//
// They hang off the phase instead of being printed by the phase, which is the
// whole difference: a degraded component must never abort the run — losing the
// workspace, the daemon and the status output because one source could not be
// built is the failure this path exists to prevent — and it must never get a
// top-level line either.
func setupComponentFailureDetails(failed []setupComponent, dash, brainCmd string) []string {
	details := make([]string, 0, 2*len(failed))
	// One remedy, one appearance: a dirty worktree stops both the seed
	// baseline and the semantic index, and the identical hint under each is
	// the repetition the short form exists to remove.
	seenHints := make(map[string]bool, len(failed))
	for _, component := range failed {
		// Flatten the DETAIL, not the composed line. A component's Detail is
		// another process's stderr: it can be three lines of git refusal, and
		// a newline inside it escapes the row budget entirely (that budget
		// governs width, not line count). Bounding the line as a whole looked
		// equivalent and was not — the clip landed before the actionable tail
		// and ate "run `… doctor` for detail" on exactly the long failures
		// that need it.
		component.Detail = setupui.FlattenText(component.Detail)
		details = append(details, setupComponentFailureLine(component, dash, brainCmd))
		if hint := strings.TrimSpace(component.Hint); hint != "" && !seenHints[hint] {
			seenHints[hint] = true
			details = append(details, setupui.FlattenText("hint: "+hint))
		}
	}
	return details
}

// setupInstantFailureReasons collects every reason the phase produced nothing
// usable, so the non-zero exit says WHAT failed rather than only that setup did.
func setupInstantFailureReasons(fatal error, failed []setupComponent) string {
	reasons := make([]string, 0, len(failed)+1)
	if fatal != nil {
		reasons = append(reasons, strings.TrimSpace(fatal.Error()))
	}
	for _, component := range failed {
		reason := setupComponentLabel(component.Name) + ": " + strings.TrimSpace(component.Detail)
		if hint := strings.TrimSpace(component.Hint); hint != "" {
			reason += " (" + hint + ")"
		}
		reasons = append(reasons, reason)
	}
	if len(reasons) == 0 {
		return "no component reported an outcome"
	}
	return strings.Join(reasons, "; ")
}

type setupCommandOptions struct {
	workspace  string
	daemonName string
	noDaemon   bool
	// verbose selects the per-phase report. The default is the short form:
	// the phase view names every component, path and provider error, which is
	// what you want when something is wrong and noise the rest of the time.
	verbose         bool
	uninstallDaemon bool
	noBackfill      bool
	noAgentHooks    bool
	// daemon/backfill/agentHooks are the AFFIRMATIVE overrides. A decline is
	// remembered (see setupRecord), so without an affirmative spelling the only
	// way back from one would be `--no-daemon=false`, which nothing documents
	// and nobody guesses. Their value is only ever read when cobra reports the
	// flag as typed.
	daemon     bool
	backfill   bool
	agentHooks bool
	// strict/normal save a guidance mode into the installed agent guide's
	// activation record. Unset, the agents phase inherits whatever mode the
	// repository already saved — a bare re-run never flips a strict repo back.
	strict bool
	normal bool
	// explicitPath is true when the reader typed the repository path, which is
	// consent to a nested target: the nested-repo warning stays quiet.
	explicitPath bool
	// yes answers the Facts phase's consent prompt without a human. It is the
	// ONLY way to spend tokens non-interactively: a run with no terminal and no
	// --yes distills nothing, because silence is not consent to spend.
	yes            bool
	agent          string
	model          string
	effort         string
	backfillBudget int
	interval       time.Duration
	distillEvery   time.Duration
	json           bool
}

func defaultSetupOptions() setupCommandOptions {
	watchDefaults := watch.DefaultFlags()
	return setupCommandOptions{
		workspace:      setupDefaultWorkspace,
		daemonName:     daemonDefaultName,
		agent:          "auto",
		effort:         setupBackfillDefaultEffort,
		backfillBudget: setupDefaultBackfillBudget,
		interval:       watchDefaults.Interval,
		distillEvery:   watchDefaults.DistillEvery,
	}
}

// setupSteps are the side effects of one setup run, injected so the
// orchestration (phase order, idempotence, skip reasons, reporting) is testable
// without running a real refresh, spawning a real process, or touching a real
// service manager.
type setupSteps struct {
	now func() time.Time
	// instant returns one entry per component it attempted. The error is
	// reserved for a phase that could not run AT ALL (no brain directory, no
	// resolvable output path); a component that failed on its own comes back as
	// a failed component with the rest still built.
	instant func(context.Context) ([]setupComponent, error)
	// observeComponent is notified as each instant-phase component lands, so the
	// progress line can tick components off live instead of revealing all of
	// them at once when the phase returns.
	observeComponent *setupComponentObserver
	// stepOutput receives the nested refresh reporter's whole-line output so
	// each stage feeds the Build phase's transient line instead of becoming a
	// permanent row of its own. Twenty-five lines of another command's phases
	// inside setup's one Build phase is the bulk of what the seven-phase model
	// deletes.
	stepOutput    *setupStepSink
	detectAgent   func(context.Context) string
	spawnBackfill func(context.Context, setupBackfillPlan) (int, error)
	stopBackfill  func(int) error
	// plan resolves the daemon artifact for a set of options. It is a step, not
	// a direct call, for the same reason planBrainWatchDaemon takes goos as an
	// argument: the install/idempotence/retirement logic above it is
	// OS-independent given a supported plan, so injecting the plan lets those
	// properties be tested from ANY host — including a windows runner, where the
	// real plan is deliberately unsupported and would otherwise make every one of
	// those tests vacuous (they failed instead: installs == 0).
	plan      func(setupCommandOptions) (daemonPlan, error)
	inspect   func(context.Context, daemonPlan) daemonState
	install   func(context.Context, daemonPlan) error
	uninstall func(context.Context, daemonPlan) error
}

// setupBackfillPlan is the detached distill invocation.
type setupBackfillPlan struct {
	Binary  string
	Args    []string
	Dir     string
	LogPath string
	Env     []string
}

type setupPhase struct {
	State   string  `json:"state"` // ok | degraded | skipped | failed
	Detail  string  `json:"detail,omitempty"`
	Seconds float64 `json:"seconds,omitempty"`
	// consentWhy is the typed reason the facts phase landed where it did.
	// Unexported on purpose: encoding/json never sees it, so the report's
	// shape is unchanged, and the short form gets to branch on a value
	// instead of matching English against Detail.
	consentWhy setupui.ConsentReason
	// Components is the instant phase's per-component outcome. "degraded" means
	// the brain is queryable but at least one of these failed.
	Components []setupComponent `json:"components,omitempty"`
}

type setupWorkspaceState struct {
	Name       string `json:"name"`
	Created    bool   `json:"created"`
	Registered bool   `json:"registered"`
	Already    bool   `json:"already_registered"`
	RepoKey    string `json:"repo_key,omitempty"`
}

type setupBackfillState struct {
	SchemaVersion int       `json:"schema_version"`
	StartedAt     time.Time `json:"started_at"`
	PID           int       `json:"pid"`
	Agent         string    `json:"agent,omitempty"`
	Model         string    `json:"model,omitempty"`
	Effort        string    `json:"effort,omitempty"`
	MaxSessions   int       `json:"max_sessions,omitempty"`
	LogPath       string    `json:"log_path,omitempty"`
	Sessions      int       `json:"sessions,omitempty"`
}

type setupReport struct {
	SchemaVersion int                 `json:"schema_version"`
	GeneratedAt   time.Time           `json:"generated_at"`
	Repo          string              `json:"repo"`
	RepoKey       string              `json:"repo_key"`
	BrainPath     string              `json:"brain_path"`
	Instant       setupPhase          `json:"instant"`
	Backfill      setupPhase          `json:"backfill"`
	Workspace     setupWorkspaceState `json:"workspace"`
	Daemon        daemonState         `json:"daemon"`
	Hook          setupHookState      `json:"session_end_hook"`
	Agents        setupAgentsState    `json:"agent_instructions"`
	Facts         factsBackfillStatus `json:"facts"`
	Warnings      []string            `json:"warnings,omitempty"`
}

func newSetupCommand(opts Options) *cobra.Command {
	setupOpts := defaultSetupOptions()
	cmd := &cobra.Command{
		Use:   "setup [path]",
		Short: "Build the brain and start background updates",
		Long: fmt.Sprintf(`setup is the one command that makes a repository's brain useful and keeps it
that way. It runs three phases:

  1. instant   build the deterministic core (code index, sessions,
               repository context, docs, history). Blocking, seconds, spends NO
               agent tokens. The brain is queryable the moment this returns.
  2. backfill  distill past sessions into durable facts, NEWEST FIRST, detached
               in the background on a cheap model. Spends tokens; bounded by
               --backfill-budget and the persisted distill cache, and skipped
               entirely when no agent CLI is available.
  3. daemon    register the repo into a workspace and install ONE machine-wide
               watcher service (launchd on macOS, a systemd user unit on Linux)
               so freshness never depends on remembering to run anything.
               ONE means one: the installed unit is identical for every repo and
               every workspace, and each workspace's interval, distill cadence,
               agent, model, effort and session cap live in a machine-level watch
               plan the running watcher re-reads. Setting up a second repo adds a
               row to that plan; it does not rewrite, restart or steal the
               watcher the first repo is relying on.

TOKEN SPEND: phases 2 and 3 call an agent. This is the first release in which
the watcher's --distill step and the session-end hook's distill actually run
(both were dead before), so setup is a real, recurring cost, bounded by five
gates: the fact phase ASKS before it spends (default no, and a run with no
terminal never spends unless --yes is passed), --backfill-budget sessions per
background pass, one gated agent run per --distill-every per repo, the
persisted distill cache (a session is never distilled twice), and the cheap
--model/--effort. Passing --no-backfill with --no-daemon spends nothing at all,
and "%[1]s status" reports what the backfill has done so far and whether the
watcher is alive.

EXIT CODE: setup exits 0 whenever the brain is QUERYABLE, and queryable means
it can answer: at least one of the code index, the repository context or the
history index built. Those are the sources "%[1]s query", "%[1]s brief" and
"%[1]s overview" read, so "%[1]s setup && %[1]s query ..." cannot proceed on a
brain that has nothing to answer from. A component that fails without costing
that (a doc index, an entity index, a stale code index beside repository context
that built) is reported as one line, skipped, and the remaining components, the
backfill, the workspace registration and the daemon install all still run;
"%[1]s status" then shows that component as failed and "%[1]s doctor" prints
the reason. setup exits non-zero when nothing usable exists: the brain
directory cannot be built, every component failed, the worktree is too dirty to
index, or the repository has no commits yet.

Re-running setup is safe: it detects the existing workspace membership and
daemon instead of duplicating them, resumes rather than restarts a backfill
that is still running, and keeps whatever --interval/--distill-every/--model/
--effort the previous run was given unless you pass the flag again.`, base.SetupCommandPrefix(os.LookupEnv)),
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			setupOpts.explicitPath = len(args) == 1
			return runSetup(cmd.Context(), cmd, opts, setupOpts, agentSurfaceTarget(opts, args), setupSteps{})
		},
	}
	cmd.Flags().StringVar(&setupOpts.workspace, "workspace", setupOpts.workspace, "Workspace the repo joins (created when absent)")
	cmd.Flags().BoolVar(&setupOpts.noDaemon, "no-daemon", false, "Run the instant phase and backfill but do not install the background watcher (remembered: later bare re-runs keep declining)")
	cmd.Flags().BoolVar(&setupOpts.daemon, "daemon", false, "Install the background watcher even if a previous run declined it")
	cmd.Flags().BoolVar(&setupOpts.uninstallDaemon, "uninstall-daemon", false, "Stop and remove the background watcher, then exit")
	cmd.Flags().BoolVar(&setupOpts.noBackfill, "no-backfill", false, "Do not start the background fact backfill, which SPENDS TOKENS (remembered: later bare re-runs keep declining)")
	cmd.Flags().BoolVar(&setupOpts.backfill, "backfill", false, "Start the background fact backfill even if a previous run declined it (SPENDS TOKENS)")
	cmd.Flags().BoolVar(&setupOpts.noAgentHooks, "no-agent-hooks", false, "Write the agent guide but do not merge the Claude Code PreToolUse entry (remembered)")
	cmd.Flags().BoolVar(&setupOpts.agentHooks, "agent-hooks", false, "Merge the Claude Code PreToolUse entry even if a previous run declined it")
	cmd.Flags().BoolVar(&setupOpts.strict, "strict", false, "Save strict (mandatory tool-use) guidance into the agent guide")
	cmd.Flags().BoolVar(&setupOpts.normal, "normal", false, "Reset the agent guide to normal guidance")
	cmd.Flags().BoolVar(&setupOpts.verbose, "verbose", false, "Report every phase, component and path instead of the short summary")
	cmd.Flags().BoolVar(&setupOpts.yes, "yes", false, "Answer the fact-backfill consent prompt with yes. Without it, a run with no terminal spends nothing")
	cmd.Flags().StringVar(&setupOpts.agent, "agent", setupOpts.agent, "Agent for the background backfill: auto, none, codex, claude-code")
	cmd.Flags().StringVar(&setupOpts.model, "model", "", "Cheap/fast model for the background backfill (recommended)")
	cmd.Flags().StringVar(&setupOpts.effort, "effort", setupOpts.effort, "Reasoning effort for the background backfill")
	cmd.Flags().IntVar(&setupOpts.backfillBudget, "backfill-budget", setupOpts.backfillBudget, "Cap sessions the background backfill distills in one pass, newest first (0 = explicitly unlimited: the WHOLE corpus in one spend)")
	cmd.Flags().DurationVar(&setupOpts.interval, "interval", setupOpts.interval, "Daemon poll interval")
	cmd.Flags().DurationVar(&setupOpts.distillEvery, "distill-every", setupOpts.distillEvery, "Minimum interval between the daemon's gated agent runs")
	cmd.Flags().StringVar(&setupOpts.daemonName, "daemon-name", setupOpts.daemonName, "RENAME the machine's single watcher (launchd label / systemd unit stem). It moves the watcher, it does not add a second one: the previous unit is retired first")
	cmd.Flags().BoolVar(&setupOpts.json, "json", false, "Emit the setup report as JSON")
	_ = cmd.Flags().MarkHidden("daemon-name")
	return cmd
}

func runSetup(ctx context.Context, cmd *cobra.Command, opts Options, setupOpts setupCommandOptions, target string, steps setupSteps) error {
	// How this run was reached, resolved ONCE so every command this run prints
	// is spelled the way the reader typed it -- the pre-flight lines, every
	// skip and failure line, the hints, and the summary's Next block all move
	// together and cannot drift apart. See setupCommandPrefix.
	brainCmd := base.SetupCommandPrefix(os.LookupEnv)
	repoDir, local, err := base.ResolveLocalTargetRepoDir(ctx, opts.Runner, target)
	if err != nil {
		return err
	}
	if !local {
		return fmt.Errorf("setup requires a local repository path: %s", target)
	}
	// Name both roots when this one sits inside another repository. Building
	// the brain and writing the guide into a nested clone is silent and looks
	// like success, and the instruction files land where the outer repository
	// cannot see them. Warn BEFORE any phase runs — the person may want to
	// abort — and only when no explicit path was given, which is consent to
	// the nested target. The path in the SUGGESTED COMMAND is shell-quoted;
	// the two in the prose are not, because they are prose. An unquoted path
	// with a space — ordinary on macOS — made the remedy silently do the wrong
	// thing when copied, and one with $ or a backtick would expand. See
	// agentsetup.OuterRepo.
	if outer := agentsetup.OuterRepo(repoDir); outer != "" && !setupOpts.explicitPath && !setupOpts.uninstallDaemon {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"warning: %s is a git repository inside %s.\nThe brain, the guide, AGENTS.md and CLAUDE.md go to the inner one. If you meant the project, run:\n    %s %s\n\n",
			repoDir, outer, base.BrainInvocation(os.LookupEnv, "setup"), shellQuotedRepoDir(outer))
	}
	// Refuse a non-repository UP FRONT. Everything below is a side effect --
	// a workspace registration, a machine watch-plan row, a per-repo state
	// directory, an installed KeepAlive service -- and every deterministic
	// source setup builds reads git. Without this check a plain directory got
	// all of those side effects, four components failing with a raw
	// `fatal: not a git repository`, and exit 0.
	//
	// --uninstall-daemon is exempt: it is machine-level maintenance that must
	// keep working from anywhere, including the directory whose failed setup is
	// the reason someone is uninstalling.
	if !setupOpts.uninstallDaemon {
		if _, ok := gitWorkTreeRoot(ctx, opts.Runner, repoDir); !ok {
			return notARepositoryError(ctx, opts.Runner, repoDir,
				base.SetupBrainCommand(brainCmd)+" setup builds every source it has -- sessions, repository context, docs, code index, entities -- from git history, so it needs git and a repository.",
				base.SetupBrainCommand(brainCmd)+" setup",
				base.SetupBrainCommand(brainCmd)+" setup <path>")
		}
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return err
	}
	perRepo := opts
	perRepo.Env.RepoRoot = repoDir
	stateDir := filepath.Dir(storage.HeadPath)
	// A repo already set up under a non-default daemon name must keep pointing
	// at THAT daemon, or `setup --uninstall-daemon` would look for one that was
	// never installed and leave the real one running. Same for the tuning flags:
	// a re-run that does not name them must not silently revert them.
	previous, setUpBefore, recordErr := setupOptionsFromRecord(stateDir)
	if recordErr != nil {
		// Refusing here is the whole point: every path below acts on a PREVIOUS
		// daemon identity, and without the record we would act on the default
		// one — retiring or uninstalling a watcher that belongs to another repo.
		return fmt.Errorf("%w\n%s", recordErr, setupRecordRemedy(recordErr, stateDir, brainCmd))
	}
	changed := setupFlagChanged(cmd)
	if err := rejectConflictingSetupToggles(changed); err != nil {
		return err
	}
	mode, err := agentsetup.SelectMode(setupOpts.strict, setupOpts.normal)
	if err != nil {
		return err
	}
	setupOpts = applySetupRecordDefaults(setupOpts, previous, changed)
	if steps.observeComponent == nil {
		steps.observeComponent = &setupComponentObserver{}
	}
	if steps.stepOutput == nil {
		steps.stepOutput = &setupStepSink{}
	}
	steps = resolveSetupSteps(cmd, perRepo, setupOpts, repoDir, brainCmd, steps)

	// --json must keep stdout parseable, so the friendly per-step lines are
	// dropped rather than interleaved with the report.
	progressOut := cmd.OutOrStdout()
	// The CONSENT PROMPT is not a progress row. It is the question that
	// decides whether this run spends the reader's money, so it is bound to
	// the real stream and silenced only by --json, which does not prompt.
	promptOut := cmd.OutOrStdout()
	if setupOpts.json {
		promptOut = io.Discard
	}
	// progressOut keeps the REAL writer unless --json, even though the default
	// short form drops the phase rows. Handing newProgress io.Discard to
	// silence them also destroys terminal detection -- io.Discard is not an
	// *os.File, so caps collapse to PlainCaps, the sink reports tty=false, and
	// setupui.Consent answers "no terminal to ask" instead of prompting. The
	// rows are suppressed at the SINK instead, which is the thing that should
	// be quiet.
	progressOut = setupProgressWriter(progressOut, setupOpts.json)
	progress := newProgress(progressOut, "setup")
	// The seven-phase UI. One live line per phase, one permanent row when it
	// settles, and sub-steps that structurally cannot reach a row of their own.
	ui := newSetupSink(progress, !setupOpts.verbose && !setupOpts.json)
	rend := ui.Renderer()
	run := setupui.NewRun(ui, rend)
	timings := &setupTimings{}

	report := setupReport{
		SchemaVersion: 1,
		GeneratedAt:   steps.now().UTC(),
		Repo:          repoDir,
		RepoKey:       storage.Key,
		BrainPath:     storage.BrainDir,
	}

	// The workspace name is the daemon's ONLY argument, and it is validated deep
	// inside registerRepoInWorkspace — a phase whose failure setup deliberately
	// downgrades to a warning so one bad source never costs the whole run. That
	// combination installed a KeepAlive / Restart=always unit running
	// `workspace watch <name the CLI rejects>`: it exits immediately, the
	// service manager restarts it every 60s forever, and setup exits 0. Reject
	// the name here, before anything is planned or installed, where the only
	// thing it can cost is this command.
	if err := workspace.ValidateName(setupOpts.workspace); err != nil {
		return fmt.Errorf("--workspace %q: %w", setupOpts.workspace, err)
	}
	shortOutput := !setupOpts.json && !setupOpts.verbose && !setupOpts.uninstallDaemon
	if shortOutput {
		setupui.RenderTerseIntro(cmd.OutOrStdout(), rend)
		ui.StartTask("Checking repository…")
		defer func() {
			ui.Suspend()
			fmt.Fprintln(cmd.OutOrStdout())
		}()
	}

	configuredAgent := setupOpts.agent

	// Resolve the agent ONCE, here, before the daemon plan is built. The plan
	// embeds the argv the watcher runs forever, so an agent resolved later (in
	// the backfill phase) reached the detached child but never the daemon, which
	// then fell back to watch's "codex" default no matter what setup detected,
	// reported, and paid the backfill with. Skipped for --uninstall-daemon,
	// which needs only the label and must not probe for CLIs it will not use.
	if !setupOpts.uninstallDaemon {
		if agent := strings.TrimSpace(setupOpts.agent); agent == "" || agent == "auto" {
			setupOpts.agent = steps.detectAgent(ctx)
		}
	}

	// The machine-level watch plan is the authority on which watcher is
	// installed. The per-repo setup record only knows what THIS repo installed.
	machinePlan, machinePlanErr := loadSetupWatchPlan(perRepo.Env)
	if machinePlanErr != nil {
		report.Warnings = append(report.Warnings, "machine watch plan unreadable: "+machinePlanErr.Error())
	}
	if !changed(setupFlagDaemonName) {
		if installedName, known := installedDaemonName(machinePlan, machinePlanErr, previous, setUpBefore); known {
			setupOpts.daemonName = installedName
		}
	}

	plan, planErr := steps.plan(setupOpts)
	if planErr != nil {
		report.Warnings = append(report.Warnings, "daemon plan unavailable: "+planErr.Error())
	}

	// --uninstall-daemon is a standalone maintenance verb: it must not build,
	// backfill, or register anything.
	if setupOpts.uninstallDaemon {
		return runSetupUninstall(ctx, cmd, opts, setupOpts, steps, plan, planErr, report)
	}

	// Renaming the daemon must MOVE it, not fork it: without this the old
	// launchd job / systemd unit keeps running under its old label forever,
	// invisible to every later `status` and `--uninstall-daemon`.
	previousName, previousKnown := installedDaemonName(machinePlan, machinePlanErr, previous, setUpBefore)
	// A repo that has never completed setup does not own the machine-wide
	// watcher merely because another repo recorded it in the shared plan.
	if !setUpBefore {
		previousKnown = false
	}

	// The one side effect of `setup` that OUTLIVES the command gets announced
	// before any of it happens. Everything else setup does lives under the
	// brain directory and dies with an `rm -rf`; the watcher is a service
	// registered with the machine's service manager that starts again at every
	// login. Reporting that in the middle of the closing summary — after it is
	// already installed and running — tells the reader what was done TO their
	// machine, not what is ABOUT to be. The daemon phase re-inspects for
	// itself rather than trusting this observation: it runs after the instant
	// and backfill phases, and its idempotence decision must be made on the
	// state as it is THEN, not as it was before any of that work.
	//
	// This does not change the default. Whether the watcher should install
	// unasked is a product decision; making it impossible to miss, and naming
	// both the flag that skips it and the command that removes it, is not.
	if !setupOpts.noDaemon && planErr == nil && plan.supported() {
		if line := setupDaemonPreflightLine(plan, steps.inspect(ctx, plan), brainCmd); line != "" {
			ui.Interject(setupPreflightRows(rend, line)...)
		}
	}

	// Phase 1 — DETECT. Spends nothing, decides nothing; it states what the
	// later phases will be able to offer, so the Facts phase's spend prompt is
	// an informed decision rather than a yes/no about an unknown.
	detectPhase := run.Begin(setupui.PhaseDetect)
	detected, commitsKnown := detectSetupFacts(ctx, perRepo, setupOpts, repoDir, storage.BrainDir, brainCmd, detectPhase)
	detected.Report(detectPhase, rend)
	// An empty repository is not a broken one. Three red component failures
	// for "you have not committed anything yet" was 47 lines describing a
	// repository where nothing is wrong; a Precondition is two.
	emptyRepo := commitsKnown && detected.Repo.Commits == 0

	// Phase 2 — BUILD. Per-component BEST-EFFORT: a component that fails is
	// reported as one line and the phase carries on, because first-run
	// onboarding must not die on one broken source. Losing the workspace, the
	// daemon and the status output because a stale semantic snapshot carried
	// another CLI's repo key is a worse outcome, by far, than a brain with four
	// sources instead of five.
	buildPhase := run.Begin(setupui.PhaseBuild)
	ui.StartTask("Building Brain…")
	// The nine components feed ONE line. The previous run's count sizes the
	// bar exactly; a first run uses the estimate, clamped so the gauge can
	// never read more than total.
	recordedComponents := len(readSetupInstantRecord(stateDir).Components)
	var observed int
	var observedMu sync.Mutex
	steps.observeComponent.bind(func(component setupComponent) {
		observedMu.Lock()
		observed++
		done := observed
		observedMu.Unlock()
		buildPhase.Progress(done, setupBuildTotal(recordedComponents, done))
		buildPhase.Step(setupComponentLabel(component.Name))
	})
	steps.stepOutput.bind(func(label string) {
		buildPhase.Step(label)
		ui.BuildStep(label)
	})
	instantStarted := time.Now()
	components, instantErr := steps.instant(ctx)
	elapsed := time.Since(instantStarted)
	timings.record("instant", elapsed)
	steps.observeComponent.bind(nil)
	steps.stepOutput.bind(nil)
	// A warning the nested refresh wrote is the one thing in that stream that
	// must survive being a repaint: it named something the reader has to know
	// and no phase line carries it.
	report.Warnings = append(report.Warnings, steps.stepOutput.Warnings()...)
	seconds := elapsed.Seconds()
	built, skipped, failed := partitionSetupComponents(components)
	// Persist the outcome BEFORE deciding whether to continue: `status` and
	// `doctor` must be able to name the failed component either way, and the
	// hard-fail path is exactly where the reason is needed most.
	if err := writeSetupInstantRecord(stateDir, steps.now().UTC(), components); err != nil {
		report.Warnings = append(report.Warnings, "instant component record not saved: "+err.Error())
	}
	report.Instant = setupPhase{Seconds: seconds, Components: components}
	// Hard-fail ONLY when nothing usable came out: the phase could not run at
	// all (unwritable brain directory, unresolvable repo) or every component it
	// attempted failed. Anything less is a degraded brain, which is still a
	// brain, and setup keeps going. A phase that reports no components at all
	// and no error built fine — that is the pre-component contract, and reading
	// it as "everything failed" would fail every such caller.
	if instantErr != nil || (len(failed) > 0 && len(built) == 0) {
		reasons := setupInstantFailureReasons(instantErr, failed)
		buildPhase.Fail("instant core failed "+rend.Dash()+" nothing usable was built", fmt.Errorf("%s", reasons))
		report.Instant.State = "failed"
		report.Instant.Detail = reasons
		if shortOutput {
			terse := terseReportFor(report, timings, false, setupOpts, brainCmd)
			setupui.RenderTerseBuild(cmd.OutOrStdout(), rend, terse)
			setupui.RenderTerseNext(cmd.OutOrStdout(), terse)
		}
		if instantErr != nil {
			return fmt.Errorf("instant phase failed, the brain is not usable: %w", instantErr)
		}
		return fmt.Errorf("instant phase failed, every component failed: %s", reasons)
	}
	report.Instant.State = "ok"
	if len(failed) > 0 {
		report.Instant.State = "degraded"
		report.Instant.Detail = setupInstantFailureReasons(nil, failed)
	}
	queryable := setupBrainQueryable(components) && !emptyRepo
	if emptyRepo {
		// The condition once, the action once, a skipped glyph. No component
		// list, no red, because nothing failed.
		buildPhase.Unmet(setupui.Precondition{
			Condition: "no commits yet, so there is nothing to index",
			Action:    "make one commit, then: " + base.SetupBrainCommand(brainCmd) + " setup",
		})
		renderSetupReady(run, rend, report, timings, queryable, brainCmd)
		if !setupOpts.json && !setupOpts.verbose {
			// The report stops here: the daemon was never inspected and no
			// source was built, so every field the normal short form reads
			// is unset rather than measured. Flagged, not rendered from, or
			// the output claims "Brain could not be built" and "Watcher
			// unavailable" about things nobody looked at.
			terse := terseReportFor(report, timings, queryable, setupOpts, brainCmd)
			terse.NoCommitsYet = true
			setupui.RenderTerse(cmd.OutOrStdout(), rend, terse)
		}
		if setupOpts.json {
			if err := cmdutil.WriteJSON(cmd, report); err != nil {
				return err
			}
		}
		return fmt.Errorf("the brain is not queryable: this repository has no commits yet, so there is nothing to index")
	}
	// Never assert queryability this brain does not have: see
	// setupUnqueryableCore. Exit 0 is unchanged -- a brain that built
	// something is still usable; it just must not claim to answer from
	// sources that are not there.
	queryability := setupQueryabilityClause(components)
	buildPhase.OK(fmt.Sprintf("instant core ready in %s %s %s, %s",
		roundedSeconds(seconds), rend.Dash(), setupComponentCountSummary(built, skipped, failed), queryability),
		setupComponentFailureDetails(append(append([]setupComponent{}, skipped...), failed...), rend.Dash(), brainCmd)...)
	if shortOutput {
		setupui.RenderTerseBuild(cmd.OutOrStdout(), rend,
			terseReportFor(report, timings, queryable, setupOpts, brainCmd))
	}

	// Make the brain VISIBLE. "Queryable" is only half of ready: until an agent
	// is told the brain exists it will never ask, and setup used to end without
	// writing a guide, a pointer or a hook — `setup.go` did not reference the
	// agentsetup package at all. A setup-only repository was invisible to every
	// agent that opened it.
	//
	// This runs here, immediately after the instant phase, rather than at the
	// end: the pointer is what makes the queryable brain worth anything, and it
	// must survive a later phase failing. It is best-effort for the same reason
	// the instant phase is — a repository whose AGENTS.md cannot be written
	// still has a working brain — so a failure is one warning, never an exit
	// code.

	// Phase 3 — AGENTS. The hooks that make every later session feed the brain
	// without anyone remembering to run anything.
	agentsPhase := run.Begin(setupui.PhaseAgents)
	ui.StartTask("Configuring agent integration…")
	agentsPhase.Step("session-end hook")
	report.Hook = inspectSessionEndHook(repoDir)
	hookLine, hookWired := setupHookSummary(report.Hook, rend.Dash())

	// The push hook rides this phase for the reason the phase exists: both
	// hooks are wiring that makes later work reach the brain without anyone
	// remembering to run anything. It arrived on main as a flat block after
	// the watcher; it depends on nothing the watcher does, so it belongs
	// beside the hook it is a sibling of rather than in a row of its own.
	agentsPhase.Step("push hook")
	pushDetails := refreshSetupPushHook(ctx, opts, repoDir, brainCmd)

	// Agent instructions ride this phase for the same reason: they are the step
	// that makes the brain discoverable at all. A failure here is one warning,
	// never an exit code -- a repository whose AGENTS.md cannot be written still
	// has a working brain.
	agentsPhase.Step("agent instructions")
	agentsStarted := time.Now()
	report.Agents = wireAgentInstructions(repoDir, !setupOpts.noAgentHooks, mode)
	timings.record("agents", time.Since(agentsStarted))
	agentDetails, agentWarning := setupAgentsPhaseDetails(report.Agents, setupOpts.noAgentHooks, rend.Dash(), brainCmd)
	if agentWarning != "" {
		report.Warnings = append(report.Warnings, agentWarning)
	}

	phaseDetails := append(append([]string{}, pushDetails...), agentDetails...)
	if setupAgentsPhaseDidWork(hookWired, report.Agents) {
		agentsPhase.OK(hookLine, phaseDetails...)
	} else {
		agentsPhase.Skip(hookLine, phaseDetails...)
	}
	if agentWarning != "" {
		ui.ShortWarning(agentWarning, brainCmd)
	}

	// Phase 4 — FACTS. The only phase that can spend money, and the only one
	// that asks first.
	backfillStarted := time.Now()
	factsPhase := run.Begin(setupui.PhaseFacts)
	ui.StartTask("Checking remaining sessions…")
	report.Facts = factsBackfillStatusForBrain(storage.BrainDir)
	report.Backfill = runSetupBackfill(ctx, factsPhase, ui, cmd.InOrStdin(), setupPromptWriter(promptOut),
		perRepo, setupOpts, steps, storage, repoDir, &report, brainCmd)
	timings.record("backfill", time.Since(backfillStarted))
	if shortOutput {
		setupui.RenderTerseDistill(cmd.OutOrStdout(), rend,
			terseReportFor(report, timings, queryable, setupOpts, brainCmd))
	}

	// Phase 5 — WORKSPACE.
	workspaceStarted := time.Now()
	workspacePhase := run.Begin(setupui.PhaseWorkspace)
	ui.StartTask("Registering repository in workspace…")
	workspacePhase.Step("registering repo")
	report.Workspace, err = registerRepoInWorkspace(ctx, cmd, perRepo, setupOpts.workspace, repoDir, storage.Key)
	switch {
	case err != nil:
		report.Warnings = append(report.Warnings, "workspace registration failed: "+err.Error())
		workspacePhase.Fail("workspace registration failed", err)
		ui.ShortWarning("Workspace registration failed: "+err.Error(), brainCmd)
	case report.Workspace.Already:
		workspacePhase.OK(fmt.Sprintf("workspace %q already has this repo", report.Workspace.Name))
	default:
		workspacePhase.OK(fmt.Sprintf("workspace %q now tracks %s", report.Workspace.Name, report.Workspace.RepoKey))
	}
	timings.record("workspace", time.Since(workspaceStarted))

	// Record this workspace's tuning in the machine-level plan BEFORE the daemon
	// is installed, so the watcher's very first tick reads a plan that already
	// contains this repo's workspace instead of reporting "nothing to watch".
	// A failure here is a warning, not a fatal: everything the deterministic
	// phases built stays usable, and the next `setup` re-attempts it.
	var watchPlanErr error
	if !setupOpts.noDaemon && planErr == nil && report.Workspace.Registered {
		if _, watchPlanErr = recordSetupWatchPlan(perRepo.Env, setupOpts.daemonName, setupWatchPlanEntryFor(setupOpts, steps.now())); watchPlanErr != nil {
			report.Warnings = append(report.Warnings, "machine watch plan not saved: "+watchPlanErr.Error())
		}
	}

	// Phase 6 — WATCHER. Nine reasons, one line. Whichever of them applies is
	// the line; none of them is worth a row of its own.
	daemonStarted := time.Now()
	watcherPhase := run.Begin(setupui.PhaseWatcher)
	if !setupOpts.noDaemon {
		ui.StartTask("Configuring background watcher…")
	}
	switch {
	case setupOpts.noDaemon:
		watcherPhase.Skip("background watcher (--no-daemon)")
		report.Daemon = daemonState{Manager: plan.Manager, Label: plan.Label, UnitPath: plan.UnitPath, Detail: "skipped: --no-daemon"}
	case !report.Workspace.Registered:
		watcherPhase.Skip("background watcher: workspace registration failed")
		report.Daemon = daemonState{Manager: plan.Manager, Label: plan.Label, UnitPath: plan.UnitPath, Detail: "skipped: workspace registration failed"}
	case watchPlanErr != nil:
		watcherPhase.Skip("background watcher: machine watch plan not saved " + rend.Dash() + " " + setupui.FlattenText(watchPlanErr.Error()))
		report.Daemon = daemonState{Manager: plan.Manager, Label: plan.Label, UnitPath: plan.UnitPath, Detail: "skipped: machine watch plan not saved"}
	case planErr != nil:
		watcherPhase.Skip("background watcher: " + setupui.FlattenText(planErr.Error()))
		report.Daemon = daemonState{Manager: daemonManagerUnsupported, Detail: planErr.Error()}
	default:
		outcome := runSetupDaemon(ctx, watcherPhase, opts.Env, steps, plan, &report, brainCmd)
		report.Daemon = outcome.state
		// Switch watcher names only after the replacement is confirmed on disk.
		// This preserves the old service if the instant phase or new install
		// fails, while still preventing two names from running indefinitely.
		if report.Daemon.Installed && report.Daemon.Label == plan.Label {
			if detail, retireErr := retireRenamedDaemon(ctx, setupOpts, previousName, previousKnown, steps, planErr); retireErr != nil {
				report.Warnings = append(report.Warnings, retireErr.Error())
				outcome.details = append(outcome.details, setupui.FlattenError(retireErr))
			} else if detail != "" {
				outcome.details = append(outcome.details, detail)
			}
		}
		if report.Daemon.Installed {
			if coverage := describeWatchPlanCoverage(watchPlanAfterInstall(perRepo.Env)); coverage != "" {
				outcome.details = append(outcome.details, coverage)
			}
		}
		outcome.settle(watcherPhase)
	}
	timings.record("daemon", time.Since(daemonStarted))
	if shortOutput {
		setupui.RenderTerseWatcher(cmd.OutOrStdout(), rend,
			terseReportFor(report, timings, queryable, setupOpts, brainCmd))
	}

	// Remember the identities AND the tuning this run used, so `status` inspects
	// the daemon that exists rather than the default one it would have created,
	// and so a later bare `setup` re-installs the same daemon rather than one
	// reverted to stock intervals and models.
	// A failed plan must not become the identity used by the next setup.
	ui.StartTask("Saving setup…")
	recordedDaemonName := setupOpts.daemonName
	if planErr != nil {
		recordedDaemonName, _ = installedDaemonName(machinePlan, machinePlanErr, previous, setUpBefore)
	}
	if err := writeSetupRecord(stateDir, setupRecord{
		SchemaVersion:  setupBackfillStateVersion,
		UpdatedAt:      steps.now().UTC(),
		Workspace:      setupOpts.workspace,
		DaemonName:     recordedDaemonName,
		Agent:          configuredAgent,
		BackfillBudget: &setupOpts.backfillBudget,
		Interval:       setupOpts.interval.String(),
		DistillEvery:   setupOpts.distillEvery.String(),
		Model:          strings.TrimSpace(setupOpts.model),
		Effort:         strings.TrimSpace(setupOpts.effort),
		// The RESOLVED values, not the raw flags: a run that inherited a
		// decline must re-record it, or the decision would survive exactly one
		// re-run and then evaporate.
		NoDaemon:     setupOpts.noDaemon,
		NoBackfill:   setupOpts.noBackfill,
		NoAgentHooks: setupOpts.noAgentHooks,
	}); err != nil {
		report.Warnings = append(report.Warnings, "setup record not saved: "+err.Error())
		ui.ShortWarning("Setup settings could not be saved: "+err.Error(), brainCmd)
	}

	// Phase 7 — READY. The verdict, the two paths nobody can infer, each
	// failure's one line, and what to run next. It does NOT re-list the
	// phases: they are on the screen directly above it.
	ui.Suspend()
	renderSetupReady(run, rend, report, timings, queryable, brainCmd)

	if shortOutput {
		setupui.RenderTerseNext(cmd.OutOrStdout(),
			terseReportFor(report, timings, queryable, setupOpts, brainCmd))
	}

	if setupOpts.json {
		if err := cmdutil.WriteJSON(cmd, report); err != nil {
			return err
		}
	}
	if !queryable {
		return fmt.Errorf("the brain is not queryable: %s", setupNotQueryableReason(failed))
	}
	return nil
}

// watchPlanAfterInstall re-reads the machine plan rather than reusing the
// value recordSetupWatchPlan returned: another repo's `setup` may have joined
// it while this one was building, and the coverage line is only worth printing
// if it is the truth right now.
func watchPlanAfterInstall(env EntireEnv) setupWatchPlan {
	plan, _ := loadSetupWatchPlan(env)
	return plan
}

// setupNotQueryableReason says which sources had to build and did not, so the
// non-zero exit names the problem instead of only reporting that there is one.
func setupNotQueryableReason(failed []setupComponent) string {
	reasons := make([]string, 0, len(failed))
	for _, component := range failed {
		if !setupContentComponents[component.Name] {
			continue
		}
		reasons = append(reasons, setupComponentLabel(component.Name)+": "+setupui.FlattenText(component.Detail))
	}
	if len(reasons) == 0 {
		return "no source this brain answers from was built"
	}
	return strings.Join(reasons, "; ")
}

// renderSetupReady settles the last phase and writes the closing block.
func renderSetupReady(run *setupui.Run, rend *setupui.Renderer, report setupReport, timings *setupTimings, queryable bool, brainCmd string) {
	phase := run.Begin(setupui.PhaseReady)
	summary := setupui.Summary{
		RepoPath:  report.Repo,
		BrainPath: report.BrainPath,
		Queryable: queryable,
		Failures:  run.Failures(),
	}
	if queryable {
		summary.Next = setupNextSteps(brainCmd, report.Daemon.Installed, report.Facts.Pending(), setupOffersConnect(report.BrainPath), report.Agents.Installed)
	}
	verdict := summary.Verdict(rend)
	if total := timings.total(); total > 0 {
		verdict += " " + rend.Bullet() + " " + roundedDuration(total)
	}
	if queryable {
		phase.OK(verdict)
	} else {
		phase.Skip(verdict)
	}
	run.EmitAll(summary.Lines(rend))
	run.EmitAll(setupWarningRows(report.Warnings))
}

func runSetupUninstall(ctx context.Context, cmd *cobra.Command, opts Options, setupOpts setupCommandOptions, steps setupSteps, plan daemonPlan, planErr error, report setupReport) error {
	out := cmd.OutOrStdout()
	if planErr != nil {
		return planErr
	}
	var before daemonState
	if err := withDaemonRegistrationLock(opts.Env, func() error {
		before = steps.inspect(ctx, plan)
		return steps.uninstall(ctx, plan)
	}); err != nil {
		return err
	}
	// Remove our pre-push hook (restoring any hook we had chained to).
	// Best-effort: a hook problem must not block the daemon uninstall report.
	if repoDir, local, repoErr := base.ResolveLocalTargetRepoDir(ctx, opts.Runner, agentSurfaceTarget(opts, nil)); repoErr == nil && local {
		if hookErr := uninstallPushHook(ctx, opts, repoDir); hookErr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "setup: push hook not removed: %v\n", hookErr)
		}
	}
	report.Daemon = daemonState{Manager: plan.Manager, Label: plan.Label, UnitPath: plan.UnitPath, Detail: "uninstalled"}
	if setupOpts.json {
		return cmdutil.WriteJSON(cmd, report)
	}
	if !before.Installed {
		fmt.Fprintf(out, "setup: no watcher was installed (%s)\n", plan.Label)
		return nil
	}
	fmt.Fprintf(out, "setup: removed the watcher %s (%s)\n", plan.Label, plan.UnitPath)
	return nil
}

// setupDaemonPreflightLine says, in one line and BEFORE the work starts, what
// setup is about to register with the machine's service manager, that it
// survives logout, which flag declines it, and which command removes it. It
// returns "" on a platform with no watcher to install, where there is nothing
// to warn about.
//
// The wording distinguishes the install from a re-run that finds the service
// already there: "will install" is a lie on the second `setup`, and a reader
// who is told a service will be installed every time stops reading the line.
func setupDaemonPreflightLine(plan daemonPlan, before daemonState, brainCmd string) string {
	if !plan.supported() {
		return ""
	}
	// Name the unit FILE, never the hashed service id. A label like
	// "io.entire.brain-watch.1a2b3c4d" is an internal uniqueness key: it is not
	// a path the reader can open, not a name they chose, and not an argument
	// any documented command accepts. The unit path is all three, and it is the
	// ONE place setup prints the watcher's identity.
	what := "a background watcher"
	if unit := strings.TrimSpace(plan.UnitPath); unit != "" {
		what = "a background watcher at " + unit
	}
	if before.Installed {
		return fmt.Sprintf(
			"background watcher: this run keeps %s installed — a persistent %s service that starts again at every login; remove it with `%s setup --uninstall-daemon`",
			what, plan.Manager, base.SetupBrainCommand(brainCmd))
	}
	return fmt.Sprintf(
		"background watcher: this run will install %s — a persistent %s service that starts again at every login; pass --no-daemon to skip it, or remove it later with `%s setup --uninstall-daemon`",
		what, plan.Manager, base.SetupBrainCommand(brainCmd))
}

// runSetupDaemon installs the one machine-wide watcher. On an OS with no
// service manager this file plans for — windows today — it is a DOCUMENTED
// no-op: setup says so in one line, keeps its exit code, and everything the
// deterministic phases built stays usable. Pretending to install a launchd
// agent on windows, or failing setup because the platform has no launchd, would
// both be worse than saying plainly that freshness there is manual for now.
// setupDaemonOutcome is what the watcher phase decided, held as DATA until
// every contributor has had its say. The install, the retirement of a renamed
// predecessor and the coverage line all belong to the same one line; building
// them as a value instead of printing each as it happens is what keeps the
// phase to one row plus its details.
type setupDaemonOutcome struct {
	state   daemonState
	summary string
	details []string
	err     error
	skipped bool
}

// settle writes the phase's single row.
func (o setupDaemonOutcome) settle(phase *setupui.PhaseRun) {
	switch {
	case o.err != nil:
		phase.Fail(o.summary, o.err)
	case o.skipped:
		phase.Skip(o.summary, o.details...)
	default:
		phase.OK(o.summary, o.details...)
	}
}

func runSetupDaemon(ctx context.Context, phase *setupui.PhaseRun, env EntireEnv, steps setupSteps, plan daemonPlan, report *setupReport, brainCmd string) setupDaemonOutcome {
	if !plan.supported() {
		return setupDaemonOutcome{
			state:   daemonState{Manager: daemonManagerUnsupported, Detail: "not supported on " + plan.OS + " yet"},
			summary: fmt.Sprintf("background watcher: not supported on %s yet (run `%s workspace watch %s` yourself to keep the brain fresh)", plan.OS, base.SetupBrainCommand(brainCmd), report.Workspace.Name),
			skipped: true,
		}
	}
	// Observe, decide and act as one step across processes: see
	// withDaemonRegistrationLock.
	var outcome setupDaemonOutcome
	_ = withDaemonRegistrationLock(env, func() error {
		outcome = runSetupDaemonLocked(ctx, phase, steps, plan, report)
		return nil
	})
	return outcome
}

func runSetupDaemonLocked(ctx context.Context, phase *setupui.PhaseRun, steps setupSteps, plan daemonPlan, report *setupReport) setupDaemonOutcome {
	phase.Step("inspecting the service manager")
	before := steps.inspect(ctx, plan)
	// Idempotence: an installed, byte-identical, running unit is left strictly
	// alone. Reloading it would kill an in-flight refresh for no reason.
	if before.Installed && before.Current && before.Running {
		// Left strictly alone at the service manager, but the file is brought up
		// to date so the inherited PATH is refreshed at the next natural restart
		// instead of being frozen at whatever the first install saw.
		_ = refreshDaemonUnitBytes(plan)
		// No hashed label. The daemon id is machine noise, a guard
		// elsewhere in the suite forbids it outside the unit path, and it
		// answers nothing a reader of `setup` came to ask.
		return setupDaemonOutcome{state: before, skipped: true, summary: "background watcher already running"}
	}
	// A no-register run can never observe "running", so byte-identical is the
	// whole of idempotence there; without this a second setup would report
	// "updated" forever and a harness could not demonstrate re-run safety.
	if plan.NoRegister && before.Installed && before.Current {
		return setupDaemonOutcome{state: before, skipped: true, summary: fmt.Sprintf("background watcher unit current, not registered (%s)", envDaemonNoRegister)}
	}
	phase.Step("installing the service")
	if err := steps.install(ctx, plan); err != nil {
		report.Warnings = append(report.Warnings, "daemon install failed: "+err.Error())
		state := before
		state.Detail = err.Error()
		return setupDaemonOutcome{state: state, summary: "background watcher not installed", err: err}
	}
	after := steps.inspect(ctx, plan)
	verb := "installed"
	if before.Installed {
		verb = "updated"
	}
	outcome := setupDaemonOutcome{state: after, summary: "background watcher " + verb}
	// Say plainly, at the moment of install, that this is a persistent service
	// and where its unit lives. "installed (io.entire.brain-watch.1a2b3c4d)"
	// does not tell a first-time reader that something now survives reboots.
	// Names the kind of thing and where it lives, WITHOUT repeating "starts
	// again at every login" -- that clause belongs to the pre-flight line,
	// which says it BEFORE the install, the half a reader can still act on.
	// Saying it again underneath is the UX-24 duplicate: a warning about
	// something that has already happened.
	//
	// Two guards meet here and both are right: one requires the output to
	// name a persistent service and its unit, the other requires the consent
	// clause to appear exactly once. "persistent service, unit: <path>"
	// satisfies both.
	if plan.UnitPath != "" {
		outcome.details = append(outcome.details, "persistent service, unit: "+plan.UnitPath)
	}
	return outcome
}

// runSetupBackfill starts the detached distill pass. Every skip reason is one
// clear line and never an error: a machine with no agent CLI, or a repo with no
// captured sessions yet, is a completely valid setup — it just has no facts to
// extract.
func runSetupBackfill(ctx context.Context, phase *setupui.PhaseRun, ui *setupSink, in io.Reader, promptOut io.Writer, opts Options, setupOpts setupCommandOptions, steps setupSteps, storage repoStorage, repoDir string, report *setupReport, brainCmd string) setupPhase {
	rend := ui.Renderer()
	dash := rend.Dash()
	resume := base.SetupBrainCommand(brainCmd) + " distill"
	counter := setupFactsCounter(rend, report.Facts)
	phase.Step("counting distillable sessions")

	// The conditions that make a spend IMPOSSIBLE come before the gate, so a
	// --yes in a paused brain reports the truth instead of claiming it spent.
	if base.BackgroundPaused(opts.Env) {
		return setupBackfillSkippedBecause(phase, "background work paused",
			"not distilled: background work is paused "+dash+" run `"+base.SetupBrainCommand(brainCmd)+" start`, then re-run setup"+counter,
			setupui.ConsentReasonPaused)
	}
	if report.Facts.Sessions > 0 && report.Facts.Pending() == 0 {
		return setupBackfillSkippedBecause(phase, "all sessions already distilled",
			fmt.Sprintf("already complete (%d/%d sessions distilled)", report.Facts.Distilled, report.Facts.Sessions),
			setupui.ConsentReasonAlreadyDistilled)
	}
	// Re-entrancy: `setup` is the command people re-run when they are impatient,
	// and the backfill is the phase that takes hours. Without this check a second
	// setup spawns a SECOND detached distill over the same pending sessions —
	// double token spend on the very corpus the first pass is working through.
	stateDir := filepath.Dir(storage.HeadPath)
	if previous, ok := readSetupBackfillState(stateDir); ok && backfillRunning(storage.BrainDir, previous.PID) {
		detail := fmt.Sprintf("already running (pid %d, started %s, %d/%d sessions distilled so far)",
			previous.PID, previous.StartedAt.Format(time.RFC3339), report.Facts.Distilled, report.Facts.Sessions)
		line := detail
		// The log path was its own line for no reason other than that it was
		// printed by its own call. It is a clause.
		if previous.LogPath != "" {
			line += "; log: " + previous.LogPath
		}
		return setupBackfillSkippedBecause(phase, detail, line, setupui.ConsentReasonAlreadyRunning)
	}
	agent := strings.TrimSpace(setupOpts.agent)
	if agent == "" || agent == "auto" {
		agent = steps.detectAgent(ctx)
	}
	if agent == "none" {
		agent = ""
	}
	budget := setupResolvedBackfillBudget(setupOpts)
	queued := report.Facts.Pending()
	if budget > 0 && queued > budget {
		queued = budget
	}

	// THE GATE. Distilling is the only part of setup that spends money, and
	// until this it was opt-OUT: a first run distilled unless the operator
	// happened to know about --no-backfill. Default no, never a prompt without
	// a terminal, and the reader is told what it would cost before answering.
	ui.Suspend()
	outcome := setupui.Consent(setupui.ConsentRequest{
		Compact:  ui.quiet,
		Sessions: queued,
		Total:    report.Facts.Pending(),
		Agent:    agent,
		Model:    strings.TrimSpace(setupOpts.model),
		Effort:   strings.TrimSpace(setupOpts.effort),
		Override: setupConsentOverride(setupOpts),
		// Both ends, not just the painted one: see setupCanAskAHuman.
		TTY:           setupCanAskAHuman(ui.tty, readerIsTerminal(in)),
		ResumeCommand: resume,
	}, in, promptOut, rend)
	if !outcome.Spend {
		return setupBackfillSkippedBecause(phase, setupui.FlattenText(outcome.Reason), outcome.Reason+counter, outcome.Why)
	}
	// What THIS run will actually distill, for the summary line. Without it the
	// short form said "Distilling 272 sessions" about a budget of 2.
	report.Facts.Queued = queued

	plan := setupBackfillPlanFor(opts, setupOpts, agent, repoDir, stateDir)
	if plan.Binary == "" {
		return setupBackfillSkippedBecause(phase, "executable path unavailable",
			"not distilled: could not resolve this executable's path"+counter,
			setupui.ConsentReasonNoExecutable)
	}
	// Interactive consent already discloses token use. Explicit --backfill or
	// --yes opts in without a prompt; keep its detailed announcement in the
	// verbose view so the default transcript contains only the result.
	if setupOpts.verbose && !outcome.Prompted {
		fmt.Fprintf(promptOut, "  fact backfill will SPEND TOKENS now: %s over %d session(s), newest first%s (--no-backfill skips it)\n",
			setupBackfillSpendLabel(agent, setupOpts), queued, setupBackfillOfLabel(queued, report.Facts.Pending()))
	}

	ui.StartTask("Starting background distillation…")
	pid, err := steps.spawnBackfill(ctx, plan)
	if err != nil {
		phase.Fail("fact backfill did not start", err)
		report.Warnings = append(report.Warnings, "backfill did not start: "+err.Error())
		return setupPhase{State: "failed", Detail: err.Error()}
	}
	state := setupBackfillState{
		SchemaVersion: setupBackfillStateVersion,
		StartedAt:     steps.now().UTC(),
		PID:           pid,
		Agent:         agent,
		Model:         strings.TrimSpace(setupOpts.model),
		Effort:        strings.TrimSpace(setupOpts.effort),
		MaxSessions:   budget,
		LogPath:       plan.LogPath,
		Sessions:      report.Facts.Pending(),
	}
	if err := writeSetupBackfillState(stateDir, state); err != nil {
		detail := "backfill state not recorded: " + err.Error()
		if stopErr := steps.stopBackfill(pid); stopErr != nil {
			detail += "; stopping untracked backfill: " + stopErr.Error()
		}
		phase.Fail("fact backfill stopped", fmt.Errorf("%s", detail))
		report.Warnings = append(report.Warnings, detail)
		return setupPhase{State: "failed", Detail: detail}
	}
	// Say what the cap did and how to lift it. A silent cap is as surprising as
	// a silent uncapped spend: the user is left believing the whole corpus was
	// distilled when only the newest slice was.
	var details []string
	if budget > 0 && state.Sessions > budget {
		details = append(details, fmt.Sprintf(
			"capped at %d of %d pending session(s) this pass (--backfill-budget); re-run `%s setup` for the next batch, or `--backfill-budget 0` to distill the whole corpus in one spend",
			budget, state.Sessions, base.SetupBrainCommand(brainCmd)))
	}
	phase.OK(fmt.Sprintf("fact backfill started in the background: %d session(s), newest first, %s (pid %d)%s",
		queued, setupBackfillSpendLabel(agent, setupOpts), pid, counter), details...)
	return setupPhase{
		State:      "ok",
		Detail:     fmt.Sprintf("pid %d, %d of %d pending session(s) queued", pid, queued, state.Sessions),
		consentWhy: outcome.Why,
	}
}

// setupBackfillSkipped settles the Facts phase with the ONE reason that
// applies. There are nine of them and none deserves a row of its own: the
// phase line IS the reason.
// setupProgressWriter picks the writer the progress reporter paints to.
//
// ONLY --json silences it. This looks like a trivial choice and is not: the
// reporter's writer is where terminal capabilities are detected, and the sink
// derives tty from it. Silencing the default short form by passing io.Discard
// here once made io.Discard the capability source -- not an *os.File, so
// PlainCaps, so tty=false, so setupui.Consent answered "no terminal to ask"
// and NEVER PROMPTED before spending. The short form suppresses rows at the
// sink instead, which is the thing that should be quiet.
func setupProgressWriter(out io.Writer, jsonMode bool) io.Writer {
	if jsonMode {
		return io.Discard
	}
	return out
}

// setupBackfillSkippedBecause records a skip and WHY, as a value.
//
// There is no untyped variant any more. The one that existed defaulted to
// ConsentReasonUnset, and every caller took it, so the short form fell
// through to a counts-based guess and reported a running backfill and a
// paused brain as "Distillation skipped -- run setup again anytime". Both
// are wrong, and the second is advice that cannot work.
func setupBackfillSkippedBecause(phase *setupui.PhaseRun, detail, line string, why setupui.ConsentReason) setupPhase {
	phase.Skip(line)
	return setupPhase{State: "skipped", Detail: detail, consentWhy: why}
}

// setupFactsCounter is the "3/12 sessions distilled" the old summary block
// carried on its own row.
//
// The block was deleted because it re-listed the phases; this number was the
// one thing in it that no phase line said, and dropping it with the rest
// would have been a collapse that lost something. It rides on the Facts line
// as a clause rather than coming back as a row, and it is omitted where the
// line already states the ratio or where there is nothing to count.
func setupFactsCounter(rend *setupui.Renderer, facts factsBackfillStatus) string {
	if facts.Sessions <= 0 {
		return ""
	}
	return fmt.Sprintf(" %s %d/%d sessions distilled", rend.Bullet(), facts.Distilled, facts.Sessions)
}

// setupBackfillSpendLabel names WHAT will be spent, in the terms the user chose:
// the agent, and the model and effort if they were set. "agent claude on
// haiku/low" is a cost a reader can price; "the backfill" is not.
func setupBackfillSpendLabel(agent string, setupOpts setupCommandOptions) string {
	label := "agent " + agent
	model := strings.TrimSpace(setupOpts.model)
	effort := strings.TrimSpace(setupOpts.effort)
	switch {
	case model != "" && effort != "":
		label += " on " + model + "/" + effort
	case model != "":
		label += " on " + model
	case effort != "":
		label += " at " + effort + " effort"
	}
	return label
}

// setupBackfillOfLabel spells out the cap when one is biting, so "25 sessions"
// is never mistaken for "all of them".
func setupBackfillOfLabel(queued, pending int) string {
	if queued >= pending {
		return ""
	}
	return fmt.Sprintf(" (capped at %d of %d pending; --backfill-budget 0 for all)", queued, pending)
}

// setupResolvedBackfillBudget normalizes --backfill-budget into the number the
// distill passes actually receive, so the detached backfill and the daemon it
// installs are capped by the SAME value. Only an EXPLICIT 0 means unlimited; a
// negative value is a typo, not a request to distill the whole corpus, and
// silently reading it as one is the most expensive way to misread a flag.
func setupResolvedBackfillBudget(setupOpts setupCommandOptions) int {
	if setupOpts.backfillBudget < 0 {
		return setupDefaultBackfillBudget
	}
	return setupOpts.backfillBudget
}

// setupBackfillPlanFor builds the detached distill argv. --newest-first is the
// whole point of the background pass: the run is O(sessions) and the user only
// ever sees its early output soon, so the most recent sessions must be first.
func setupBackfillPlanFor(opts Options, setupOpts setupCommandOptions, agent, repoDir, stateDir string) setupBackfillPlan {
	binary, err := os.Executable()
	if err != nil {
		return setupBackfillPlan{}
	}
	args := []string{"distill", repoDir, "--newest-first", "--agent", agent}
	if model := strings.TrimSpace(setupOpts.model); model != "" {
		args = append(args, "--model", model)
	}
	if effort := strings.TrimSpace(setupOpts.effort); effort != "" {
		args = append(args, "--effort", effort)
	}
	if budget := setupResolvedBackfillBudget(setupOpts); budget > 0 {
		args = append(args, "--max-sessions", fmt.Sprint(budget))
	}
	return setupBackfillPlan{
		Binary:  binary,
		Args:    args,
		Dir:     repoDir,
		LogPath: filepath.Join(stateDir, setupBackfillLogFile),
		Env:     setupChildEnv(opts.Env, repoDir),
	}
}

// setupChildEnv carries the plugin dirs into the detached child. Without them a
// background process started from a test or a non-default install would resolve
// a DIFFERENT brain store than the CLI that spawned it.
func setupChildEnv(env EntireEnv, repoDir string) []string {
	out := os.Environ()
	for key, value := range setupPluginEnv(env, repoDir) {
		out = append(out, key+"="+value)
	}
	return out
}

func setupPluginEnv(env EntireEnv, repoDir string) map[string]string {
	values := map[string]string{base.EnvRepoRoot: repoDir}
	for key, value := range map[string]string{
		base.EnvPluginConfigDir: env.PluginConfigDir,
		base.EnvPluginDataDir:   env.PluginDataDir,
		base.EnvPluginStateDir:  env.PluginStateDir,
		base.EnvPluginCacheDir:  env.PluginCacheDir,
	} {
		if strings.TrimSpace(value) != "" {
			values[key] = value
		}
	}
	return values
}

// brainWatchDaemonPlan describes the ONE machine-wide watcher. It watches the
// workspace, not this repo, so a second `setup` in a second repo adds a member
// rather than a second daemon.
//
// Nothing in the rendered unit may depend on which repo ran setup. The log path
// used to: it pointed inside the calling repo's state directory, so setting up a
// second repo produced different bytes for the same machine-wide job, which
// inspectDaemon correctly read as drift and "repaired" by unloading and
// reloading the running daemon — every single time.
func brainWatchDaemonPlan(opts Options, setupOpts setupCommandOptions) (daemonPlan, error) {
	return brainWatchDaemonPlanFor(runtime.GOOS, opts, setupOpts)
}

// brainWatchDaemonPlanFor is brainWatchDaemonPlan with the target OS as an
// argument. runtime.GOOS is read in exactly one place (above) so that the whole
// plan — spec, label, unit path, rendered contents — can be built for darwin or
// linux from any host, which is what makes the daemon behaviour testable on a
// windows runner instead of silently untested there.
func brainWatchDaemonPlanFor(goos string, opts Options, setupOpts setupCommandOptions) (daemonPlan, error) {
	binary, err := os.Executable()
	if err != nil {
		return daemonPlan{}, fmt.Errorf("resolve executable: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return daemonPlan{}, fmt.Errorf("resolve home directory: %w", err)
	}
	dirs, err := base.ResolvePluginDirs(opts.Env)
	if err != nil {
		return daemonPlan{}, fmt.Errorf("resolve plugin state dir: %w", err)
	}
	spec := daemonSpec{
		Name:       setupOpts.daemonName,
		Binary:     binary,
		Args:       brainWatchDaemonArgs(setupOpts),
		WorkingDir: filepath.Dir(binary),
		LogPath:    filepath.Join(dirs.State, "logs", setupDaemonLogFile),
		Env:        daemonEnv(opts.Env),
	}
	plan, err := planBrainWatchDaemon(goos, home, os.Getenv(base.XDGConfigHome), os.Getenv(envDaemonUnitDir), spec)
	if err != nil {
		return daemonPlan{}, err
	}
	// Read the no-register decision ONCE, here, so install, uninstall and the
	// running-probe all act on the same answer for the life of this command.
	plan.NoRegister = daemonRegistrationDisabled()
	return plan, nil
}

// brainWatchDaemonArgs are the argv of the ONE machine-wide watcher, and they
// are deliberately CONSTANT: `workspace watch --distill`, with no positional
// workspace and no tuning flags. Nothing about which repo ran `setup` reaches
// the unit file.
//
// That is the whole point. The launchd label and systemd unit name derive from
// the daemon NAME alone, so they are machine-wide — but the args used to be
// per-repo (`workspace watch <ws> --interval ... --model ...`). Two repos that
// asked for two different workspaces resolved to the SAME unit path and rendered
// DIFFERENT bytes: the second `setup` overwrote the first repo's watcher, the
// first repo was never watched again, and its `status` still said "running"
// because the drift was computed and then dropped. Even two repos on the SAME
// workspace clobbered each other's interval, model and effort.
//
// With a constant unit, everything that varies per workspace — interval,
// distill cadence, agent, model, effort, session cap — lives in the machine-level
// watch plan (setup_watchplan.go) that the running daemon re-reads on every
// outer pass. A second repo's `setup` adds a row to that plan; the unit's bytes
// do not change, so inspectDaemon sees no drift and the service is never
// unloaded and reloaded under an in-flight refresh.
//
// --distill stays here rather than in the plan because it is not tuning: it is
// the statement that this service is allowed to spend at all, and `setup` always
// installs a watcher that is. How MUCH and how OFTEN it may spend are both in
// the plan (--max-sessions, --distill-every) and both bind, per workspace.
//
// --budget is deliberately NOT passed. It counts gated agent runs for the life
// of the PROCESS and never resets, and this daemon is a KeepAlive service that
// is meant to run for months — so `--budget 1` did not mean "one run per window",
// it meant one run EVER, machine-wide, across every repo in the workspace. The
// durable, self-resetting guard is the pair watch.go documents: --distill-every
// plus each repo's persisted cursor, which survives restarts and bounds spend
// per repo per window without ever latching off.
func brainWatchDaemonArgs(setupCommandOptions) []string {
	return []string{"workspace", "watch", "--distill"}
}

func daemonEnv(env EntireEnv) map[string]string {
	values := map[string]string{}
	for key, value := range map[string]string{
		base.EnvPluginConfigDir: env.PluginConfigDir,
		base.EnvPluginDataDir:   env.PluginDataDir,
		base.EnvPluginStateDir:  env.PluginStateDir,
		base.EnvPluginCacheDir:  env.PluginCacheDir,
	} {
		if strings.TrimSpace(value) != "" {
			values[key] = value
		}
	}
	// launchd/systemd jobs get a minimal PATH; the deterministic refresh shells
	// out to `git` and `entire`, so an inherited PATH is not optional.
	if path := strings.TrimSpace(os.Getenv("PATH")); path != "" {
		values["PATH"] = path
	}
	// The unit-directory override is part of the daemon's identity: the daemon
	// re-derives its own plan when it re-registers itself after a binary
	// upgrade (daemon-reregister), and without this variable a watcher
	// installed into an overridden directory would resolve — and bootout — the
	// DEFAULT unit path, which may belong to a different, real watcher.
	if unitDir := strings.TrimSpace(os.Getenv(envDaemonUnitDir)); unitDir != "" {
		values[envDaemonUnitDir] = unitDir
	}
	return values
}

// registerRepoInWorkspace makes the repo a member of the workspace the daemon
// watches, creating the workspace on first use. Idempotent: an existing member
// is reported as already-registered and the manifest is not rewritten.
// The whole read-modify-write runs under the workspace manifest lock, because
// two `setup` runs in two repos at the same time is the ordinary case on a
// developer machine, and an unlocked last-writer-wins rewrite silently drops one
// of the two registrations.
func registerRepoInWorkspace(_ context.Context, _ *cobra.Command, opts Options, name, repoDir, repoKey string) (setupWorkspaceState, error) {
	state := setupWorkspaceState{Name: name, RepoKey: repoKey}
	result, err := workspace.DefaultStore(opts.Env).AddRepoLocked(name, workspace.Repo{RepoKey: repoKey, LocalPathHint: repoDir})
	if err != nil {
		return state, err
	}
	state.Created = result.Created
	state.Already = result.Already
	state.Registered = true
	return state, nil
}

// newSetupInstantCommand builds the cobra command the nested deterministic
// refresh runs under. Its output is ALWAYS discarded, in every mode.
//
// The nested refresh is a complete command with its own renderer: its own
// consent text, its own names for the components, its own daemon identity. Only
// --json discarded it, so a human first run printed setup's lines AND the inner
// refresh's on top of each other -- roughly two dozen extra lines in which the
// same consent appeared twice and one component answered to two different
// names in one run. The sub-command exists to be RUN, not to be read: every
// component it builds is reported by setup itself, live through
// observeComponent and again in the closing summary.
//
// Discarding unconditionally is also why the --json contract is untouched:
// that path already discarded, so its bytes cannot move.
func newSetupInstantCommand(ctx context.Context, stepOutput io.Writer) (*cobra.Command, *bytes.Buffer) {
	// stdout is discarded: it is the nested refresh's progress, which setup
	// reports itself. stderr is CAPTURED rather than discarded, because the
	// same stream carries genuine warnings -- a memory coordinator that could
	// not launch, an entity index that was skipped -- and discarding it lost
	// them silently. Raised as a finding on this change.
	var captured bytes.Buffer
	sub := &cobra.Command{}
	sub.SetContext(ctx)
	// stdout goes to the step sink rather than io.Discard: the nested reporter
	// has no idea it is nested, and left pointed at the real streams it writes
	// a permanent line per stage on top of the live row setup owns.
	sub.SetOut(stepOutput)
	// stderr is TEED, not chosen between. It has to reach the step sink so it
	// renders where the rest of the phase does, and it has to be captured so
	// setupWarningsFrom can lift the genuine warnings out. Sending it to only
	// one of the two is how either the rendering or the warnings go missing.
	sub.SetErr(io.MultiWriter(stepOutput, &captured))
	return sub, &captured
}

// setupWarningsFrom picks the warnings out of the nested refresh's stderr.
//
// Everything else on that stream is progress setup reports itself, so only
// lines the refresh explicitly marked as a warning are carried up.
func setupWarningsFrom(captured *bytes.Buffer) []string {
	if captured == nil {
		return nil
	}
	// A warning is one `warning: ` line plus any continuation lines under it.
	// Go errors are routinely multi-line -- errors.Join concatenates with a
	// newline, and the memory path joins a deterministic failure with a run
	// failure -- so keeping only prefixed lines dropped every cause after the
	// first, reproducing in miniature the information loss this re-emission
	// exists to repair.
	//
	// A continuation ends at a blank line or at the next warning, so unrelated
	// stderr written after a warning is not swept into it.
	var out []string
	inWarning := false
	for _, line := range strings.Split(captured.String(), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "warning: "):
			out = append(out, strings.TrimPrefix(line, "warning: "))
			inWarning = true
		case line == "":
			inWarning = false
		case inWarning:
			out[len(out)-1] += "; " + line
		}
	}
	return out
}

// resolveSetupSteps fills any injection point the caller left nil with the real
// implementation.
// The cobra command is used again: the nested refresh writes to io.Discard,
// but its stderr is captured and any WARNINGS on it are re-emitted here, on
// the command the user is actually reading.
func resolveSetupSteps(cmd *cobra.Command, opts Options, setupOpts setupCommandOptions, repoDir, brainCmd string, steps setupSteps) setupSteps {
	if steps.now == nil {
		steps.now = opts.Now
		if steps.now == nil {
			steps.now = time.Now
		}
	}
	if steps.stepOutput == nil {
		steps.stepOutput = &setupStepSink{}
	}
	if steps.instant == nil {
		steps.instant = func(ctx context.Context) ([]setupComponent, error) {
			// The free path `watch` runs — sessions + semantic + seed + docs +
			// entities + memory reconciliation with seed agent "none" — in
			// best-effort mode, so each component reports its own outcome and one
			// failure does not abort the build. History is the one difference from
			// a tick: onboarding builds the projection now rather than leaving the
			// first `brief` to answer without it.
			sub, capturedStderr := newSetupInstantCommand(ctx, steps.stepOutput)
			var components []setupComponent
			// User-invoked (`entire setup`): keeps the transcript cache sweep,
			// same as every other foreground command.
			err := watchDeterministicRefreshComponents(ctx, sub, opts, repoDir, setupBuildsHistoryProjection, true, func(name string, err error) {
				component := newSetupComponent(name, err, brainCmd)
				components = append(components, component)
				steps.observeComponent.notify(component)
			}, false)
			// The nested refresh's progress is discarded, but a warning on that
			// stream is not progress -- a memory coordinator that could not
			// launch, an entity index that was skipped -- and it reaches nobody
			// unless it is re-emitted on the command the user is reading.
			if cmd != nil && !setupOpts.json {
				for _, warning := range setupWarningsFrom(capturedStderr) {
					printAroundLiveLine(cmd.ErrOrStderr(), "warning: %s\n", warning)
				}
			}
			return components, err
		}
	}
	if steps.detectAgent == nil {
		steps.detectAgent = func(ctx context.Context) string {
			// No standalone announcement here: the agent is disclosed at the
			// moments it matters instead. The consent block names agent, model
			// and effort before a human answers, and the --yes path prints the
			// spend label before the detached child starts. A preamble before
			// any of that was the first line every reader saw, about a spend
			// that had not been agreed to yet (and often never happened).
			return defaultRefreshAgent(ctx, opts.Runner, repoDir)
		}
	}
	if steps.spawnBackfill == nil {
		steps.spawnBackfill = func(ctx context.Context, plan setupBackfillPlan) (int, error) {
			return spawnDetached(plan)
		}
	}
	if steps.stopBackfill == nil {
		steps.stopBackfill = stopDetachedProcess
	}
	if steps.plan == nil {
		steps.plan = func(so setupCommandOptions) (daemonPlan, error) {
			return brainWatchDaemonPlan(opts, so)
		}
	}
	if steps.inspect == nil {
		steps.inspect = func(ctx context.Context, plan daemonPlan) daemonState {
			return inspectDaemon(ctx, opts.Runner, plan)
		}
	}
	if steps.install == nil {
		steps.install = func(ctx context.Context, plan daemonPlan) error {
			return installDaemon(ctx, opts.Runner, plan)
		}
	}
	if steps.uninstall == nil {
		steps.uninstall = func(ctx context.Context, plan daemonPlan) error {
			return uninstallDaemon(ctx, opts.Runner, plan)
		}
	}
	return steps
}

// setupRecord is what a previous `setup` chose for this repo. It exists so
// later reads (status) inspect the real daemon identity instead of assuming the
// defaults, which would report a custom-named watcher as "not installed".
type setupRecord struct {
	SchemaVersion int       `json:"schema_version"`
	UpdatedAt     time.Time `json:"updated_at"`
	Workspace     string    `json:"workspace,omitempty"`
	DaemonName    string    `json:"daemon_name,omitempty"`
	// The daemon's tuning is part of its identity too: a re-run that does not
	// mention these must re-render the SAME unit, not one reverted to stock.
	Interval       string `json:"interval,omitempty"`
	DistillEvery   string `json:"distill_every,omitempty"`
	Model          string `json:"model,omitempty"`
	Effort         string `json:"effort,omitempty"`
	Agent          string `json:"agent,omitempty"`
	BackfillBudget *int   `json:"backfill_budget,omitempty"`
	// What the user DECLINED is part of the setup too, and leaving it out was
	// the sharpest edge on this command: our own output tells a reader to
	// re-run `setup`, and a bare re-run after `setup --no-daemon --no-backfill`
	// silently installed the persistent login service they had refused and
	// started the token spend they had refused. A decline is a decision; it is
	// remembered exactly like the daemon name and the tuning, and only an
	// explicit affirmative flag on a later run reverses it.
	//
	// A record written by an older build carries none of these, which decodes
	// to false — "nothing was declined" — and leaves that repo behaving exactly
	// as it does today. That is why the absent and the "no" case may share a
	// spelling here: both mean do it.
	NoDaemon     bool `json:"no_daemon,omitempty"`
	NoBackfill   bool `json:"no_backfill,omitempty"`
	NoAgentHooks bool `json:"no_agent_hooks,omitempty"`
}

// setupFlagNames maps each remembered option to its flag, so "was this passed?"
// is asked of cobra rather than inferred.
const (
	setupFlagAgent          = "agent"
	setupFlagBackfillBudget = "backfill-budget"
	setupFlagWorkspace      = "workspace"
	setupFlagDaemonName     = "daemon-name"
	setupFlagInterval       = "interval"
	setupFlagDistillEvery   = "distill-every"
	setupFlagModel          = "model"
	setupFlagEffort         = "effort"
	setupFlagNoDaemon       = "no-daemon"
	setupFlagDaemon         = "daemon"
	setupFlagNoBackfill     = "no-backfill"
	setupFlagBackfill       = "backfill"
	setupFlagNoAgentHooks   = "no-agent-hooks"
	setupFlagAgentHooks     = "agent-hooks"
)

// setupToggleConflicts pairs each decline with the affirmative that reverses it.
// Passing both in one run is a contradiction with no safe reading, and guessing
// one is how a reader ends up with the service they just declined.
var setupToggleConflicts = [][2]string{
	{setupFlagNoDaemon, setupFlagDaemon},
	{setupFlagNoBackfill, setupFlagBackfill},
	{setupFlagNoAgentHooks, setupFlagAgentHooks},
}

// rejectConflictingSetupToggles refuses a run that both declines and demands the
// same thing.
func rejectConflictingSetupToggles(changed func(string) bool) error {
	for _, pair := range setupToggleConflicts {
		if changed(pair[0]) && changed(pair[1]) {
			return fmt.Errorf("--%s and --%s contradict each other; pass one", pair[0], pair[1])
		}
	}
	return nil
}

// resolveSetupToggle answers one decline for this run.
//
// Precedence, strictest first: an explicit affirmative wins outright, then an
// explicit decline, then whatever the previous run recorded. Anything else
// would make the remembered decline unescapable or make it worthless.
func resolveSetupToggle(declined, affirmed, recorded bool, changed func(string) bool, declineFlag, affirmFlag string) bool {
	if changed(affirmFlag) {
		return !affirmed
	}
	if changed(declineFlag) {
		return declined
	}
	return recorded
}

// setupFlagChanged reports which flags the user actually typed. Comparing a
// value against its default cannot answer that: `--daemon-name entire-brain-watch`
// and `--interval 5m` are indistinguishable from not passing them, so a repo set
// up with a custom daemon could never be moved back to the default one, and
// explicitly re-affirming a default silently restored the recorded value
// instead. cmd may be nil in tests that drive runSetup directly; then nothing
// was typed.
// setupRecordRemedy answers an unreadable setup record with advice matched to
// WHY it could not be read.
//
// It used to say "delete it and re-run" for every failure. Deleting the record
// loses this repo's daemon identity and tuning, and the comment above the call
// site explains why that matters: every path below acts on a PREVIOUS daemon
// identity, so a lost record can retire a watcher belonging to another repo.
// Advising that for a transient read -- a permission problem, a file locked by
// another process, a disk hiccup -- is telling the reader to destroy recoverable
// state to clear an error that would have gone away.
//
// Only a record that cannot be PARSED is actually unrecoverable.
func setupRecordRemedy(err error, stateDir, brainCmd string) string {
	path := filepath.Join(stateDir, setupRecordFile)
	if setupRecordIsCorrupt(err) {
		return fmt.Sprintf("this repo's setup record cannot be parsed, so the watcher it names is unknown; delete %s and re-run `%s setup` to re-adopt the watcher", path, brainCmd)
	}
	return fmt.Sprintf("this repo's setup record could not be read, which may be temporary; check %s is readable and re-run `%s setup`. Do not delete it unless the re-run reports it cannot be parsed -- it records which watcher belongs to this repo", path, brainCmd)
}

// setupRecordIsCorrupt distinguishes "the bytes are not valid" from "the bytes
// could not be fetched". Typed errors first; the text check is the fallback for
// the wrapped forms this package produces.
func setupRecordIsCorrupt(err error) bool {
	if err == nil {
		return false
	}
	var syntax *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &syntax) || errors.As(err, &typeErr) {
		return true
	}
	if errors.Is(err, fs.ErrPermission) || errors.Is(err, fs.ErrNotExist) {
		return false
	}
	msg := err.Error()
	for _, mark := range []string{"unknown field", "cannot be parsed", "invalid character", "unexpected end of JSON", brainstore.StateCorruptCode} {
		if strings.Contains(msg, mark) {
			return true
		}
	}
	return false
}

func setupFlagChanged(cmd *cobra.Command) func(string) bool {
	if cmd == nil {
		return func(string) bool { return false }
	}
	flags := cmd.Flags()
	return func(name string) bool {
		if flags.Lookup(name) == nil {
			return false
		}
		return flags.Changed(name)
	}
}

// applySetupRecordDefaults carries a previous run's identities and tuning
// forward for every option the caller did NOT pass, so a repo set up with a
// custom workspace, daemon name, interval, model or effort keeps them on a bare
// re-run instead of silently reverting to stock and rewriting the unit.
func applySetupRecordDefaults(setupOpts setupCommandOptions, recorded setupCommandOptions, changed func(string) bool) setupCommandOptions {
	if changed == nil {
		changed = func(string) bool { return false }
	}
	if !changed(setupFlagDaemonName) {
		setupOpts.daemonName = recorded.daemonName
	}
	if !changed(setupFlagAgent) && recorded.agent != "" {
		setupOpts.agent = recorded.agent
	}
	if !changed(setupFlagBackfillBudget) {
		setupOpts.backfillBudget = recorded.backfillBudget
	}
	if !changed(setupFlagWorkspace) {
		setupOpts.workspace = recorded.workspace
	}
	if !changed(setupFlagInterval) && recorded.interval > 0 {
		setupOpts.interval = recorded.interval
	}
	if !changed(setupFlagDistillEvery) && recorded.distillEvery > 0 {
		setupOpts.distillEvery = recorded.distillEvery
	}
	if !changed(setupFlagModel) && strings.TrimSpace(recorded.model) != "" {
		setupOpts.model = recorded.model
	}
	if !changed(setupFlagEffort) && strings.TrimSpace(recorded.effort) != "" {
		setupOpts.effort = recorded.effort
	}
	setupOpts.noDaemon = resolveSetupToggle(setupOpts.noDaemon, setupOpts.daemon, recorded.noDaemon, changed, setupFlagNoDaemon, setupFlagDaemon)
	setupOpts.noBackfill = resolveSetupToggle(setupOpts.noBackfill, setupOpts.backfill, recorded.noBackfill, changed, setupFlagNoBackfill, setupFlagBackfill)
	setupOpts.noAgentHooks = resolveSetupToggle(setupOpts.noAgentHooks, setupOpts.agentHooks, recorded.noAgentHooks, changed, setupFlagNoAgentHooks, setupFlagAgentHooks)
	return setupOpts
}

// retireRenamedDaemon unloads the daemon a previous run installed when THIS run
// names a different one. Writing the new unit without removing the old one
// leaves two watchers running: the old label is not in any later plan, so
// `status` cannot see it and `--uninstall-daemon` cannot remove it. It would
// keep ticking (and spending) until the machine was rebuilt.
func retireRenamedDaemon(ctx context.Context, setupOpts setupCommandOptions, old string, known bool, steps setupSteps, planErr error) (string, error) {
	old = strings.TrimSpace(old)
	// Only a machine that actually installed a watcher before can have one to
	// retire. Without that check, a first-ever `setup --daemon-name mine` would
	// read the absent record as the default name and uninstall the machine-wide
	// default watcher that every other repo depends on.
	if !known || planErr != nil || old == "" || old == setupOpts.daemonName || setupOpts.noDaemon {
		return "", nil
	}
	retired := setupOpts
	retired.daemonName = old
	oldPlan, err := steps.plan(retired)
	if err != nil {
		return "", fmt.Errorf("previous watcher %q could not be planned for removal: %w", old, err)
	}
	// An OS with no service manager never installed the old daemon either, so
	// there is nothing to retire and nothing worth saying about it.
	if !oldPlan.supported() {
		return "", nil
	}
	// Uninstall is idempotent, so do not re-inspect here: the replacement has
	// already been installed and the old unit may no longer be representable by
	// a single-state test adapter. A known previous name is enough authority.
	if err := steps.uninstall(ctx, oldPlan); err != nil {
		return "", fmt.Errorf("previous watcher %s not removed: %w", oldPlan.Label, err)
	}
	return fmt.Sprintf("previous watcher %s removed (renamed to %s)", oldPlan.Label, setupOpts.daemonName), nil
}

// installedDaemonName answers "which watcher does this MACHINE have installed",
// preferring the machine-level watch plan over this repo's own setup record.
//
// The per-repo record is the fallback for a machine set up by an older build
// that never wrote a plan; it is correct for the single-repo case and wrong for
// exactly the case the plan exists to fix, so the plan wins whenever it has an
// answer. An unreadable plan falls back rather than guessing: retiring the
// wrong daemon is worse than retiring none.
func installedDaemonName(plan setupWatchPlan, planErr error, previous setupCommandOptions, setUpBefore bool) (string, bool) {
	if planErr == nil {
		if name := strings.TrimSpace(plan.DaemonName); name != "" {
			return name, true
		}
	}
	return strings.TrimSpace(previous.daemonName), setUpBefore
}

func writeSetupRecord(stateDir string, record setupRecord) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	return writeJSONFile(filepath.Join(stateDir, setupRecordFile), record)
}

// setupOptionsFromRecord returns the setup options a previous run used, falling
// back to the defaults when this repo has never been set up. The bool says
// which of the two it is — "never set up" and "set up with the defaults" are
// indistinguishable in the values but mean opposite things to anything that
// acts on a previous daemon.
//
// The error is the third state, and it is NOT the same as "never set up": a
// setup.json that exists but cannot be parsed means the recorded daemon name is
// unknown, and answering "never set up, here are the defaults" made
// `--uninstall-daemon` uninstall the DEFAULT watcher — which on a shared
// machine is the one every OTHER repo depends on — instead of this repo's. A
// caller that is about to act on a previous daemon must refuse; a caller that
// only wants defaults may ignore it.
func setupOptionsFromRecord(stateDir string) (setupCommandOptions, bool, error) {
	opts := defaultSetupOptions()
	data, err := os.ReadFile(filepath.Join(stateDir, setupRecordFile))
	if err != nil {
		if os.IsNotExist(err) {
			return opts, false, nil
		}
		return opts, false, fmt.Errorf("read %s: %w", filepath.Join(stateDir, setupRecordFile), err)
	}
	var record setupRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return opts, false, fmt.Errorf("parse %s: %w", filepath.Join(stateDir, setupRecordFile), err)
	}
	if record.Agent != "" {
		opts.agent = record.Agent
	}
	if record.BackfillBudget != nil {
		opts.backfillBudget = *record.BackfillBudget
	}
	if strings.TrimSpace(record.Workspace) != "" {
		opts.workspace = record.Workspace
	}
	if strings.TrimSpace(record.DaemonName) != "" {
		opts.daemonName = record.DaemonName
	}
	if d, err := time.ParseDuration(strings.TrimSpace(record.Interval)); err == nil && d > 0 {
		opts.interval = d
	}
	if d, err := time.ParseDuration(strings.TrimSpace(record.DistillEvery)); err == nil && d > 0 {
		opts.distillEvery = d
	}
	if strings.TrimSpace(record.Model) != "" {
		opts.model = record.Model
	}
	if strings.TrimSpace(record.Effort) != "" {
		opts.effort = record.Effort
	}
	opts.noDaemon = record.NoDaemon
	opts.noBackfill = record.NoBackfill
	opts.noAgentHooks = record.NoAgentHooks
	return opts, true, nil
}

// setupInstantRecord is the last instant phase's per-component outcome, kept
// beside the other per-repo setup state. Without it a failed component is
// indistinguishable from one that was never built: `status` would print
// semantic=missing for a semantic index that failed loudly ten seconds ago, and
// the reason would exist nowhere at all once the terminal scrolled.
type setupInstantRecord struct {
	SchemaVersion int              `json:"schema_version"`
	UpdatedAt     time.Time        `json:"updated_at"`
	Components    []setupComponent `json:"components,omitempty"`
}

// writeSetupInstantRecord persists the phase outcome. A phase that reported
// nothing (an injected step, an older build) leaves the previous record alone
// rather than erasing a real failure with an empty one.
func writeSetupInstantRecord(stateDir string, now time.Time, components []setupComponent) error {
	if len(components) == 0 {
		return nil
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	return writeJSONFile(filepath.Join(stateDir, setupInstantRecordFile), setupInstantRecord{
		SchemaVersion: setupBackfillStateVersion,
		UpdatedAt:     now,
		Components:    components,
	})
}

func readSetupInstantRecord(stateDir string) setupInstantRecord {
	data, err := os.ReadFile(filepath.Join(stateDir, setupInstantRecordFile))
	if err != nil {
		return setupInstantRecord{}
	}
	var record setupInstantRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return setupInstantRecord{}
	}
	return record
}

// failedSetupComponents is what `doctor` prints: the components the last setup
// could not build, with the reason and the hint.
// failedSetupComponents returns the components the last setup could not
// build, with failures that have since been fixed removed.
//
// It takes brainDir for the same reason instantPhaseComponents does: a
// recorded failure is only still a failure if the component's own subsystem
// has not produced a successful run since. Without that, `brain status`
// clears a stale failure and `brain doctor` keeps reporting it from the same
// record -- which is the two-surfaces-disagree bug this rule exists to end,
// reproduced between a different pair of surfaces.
func failedSetupComponents(stateDir, brainDir string) []setupComponent {
	record := readSetupInstantRecord(stateDir)
	_, _, failed := partitionSetupComponents(record.Components)
	return pruneRebuiltFailures(failed, componentRunTimes(brainDir), record.UpdatedAt)
}

// pruneRebuiltFailures drops failures whose component has produced a
// successful run since the failure was recorded.
//
// One implementation, shared by `status` and `doctor`. They used to each
// decide for themselves, which is how status came to report a component as
// built while doctor reported the same record as an error.
func pruneRebuiltFailures(failed []setupComponent, runs map[string]time.Time, recordedAt time.Time) []setupComponent {
	kept := make([]setupComponent, 0, len(failed))
	for _, component := range failed {
		if componentRebuiltSince(runs, component.Name, recordedAt) {
			continue
		}
		kept = append(kept, component)
	}
	return kept
}

func writeSetupBackfillState(stateDir string, state setupBackfillState) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	return writeJSONFile(filepath.Join(stateDir, setupBackfillStateFile), state)
}

func roundedSeconds(seconds float64) string {
	return time.Duration(seconds * float64(time.Second)).Round(100 * time.Millisecond).String()
}

// setupAgentsPhaseDetails says whether any agent can now find this brain, as
// rows that hang off the AGENTS phase rather than lines printed beside it --
// the same move main made for the push hook, and for the same reason: this is
// wiring that makes later work reach the brain, not a phase of its own.
//
// It returns the warning instead of printing it so the caller can put it on
// the report, where `--json` and the closing summary both read it. Silently
// skipping the one step that makes the brain discoverable is the defect this
// phase exists to fix, and a silent failure here would reintroduce it.
// setupAgentsPhaseDidWork reports whether the AGENTS phase actually did
// something, which decides its mark.
//
// Wiring is WORK, and a phase that did work must not report "skipped" -- that
// is the contradiction this PR exists to remove. The session-end hook is not
// the only wiring in the phase: writing the agent instructions, or merging the
// pre-edit hook, is the step that makes the brain discoverable at all. A run
// that did either on a repo whose session-end hook was already absent still
// did work, and saying "skipped" there denies it.
func setupAgentsPhaseDidWork(hookWired bool, agents setupAgentsState) bool {
	if hookWired {
		return true
	}
	return agents.Installed && (agents.HookChanged || len(agents.Changed) > 0)
}

func setupAgentsPhaseDetails(agents setupAgentsState, declinedHooks bool, dash, brainCmd string) (details []string, warning string) {
	if !agents.Installed {
		warning = "agent instructions not written: " + agents.Detail
		return []string{fmt.Sprintf("%s %s no agent will discover this brain; fix the cause and re-run `%s setup`",
			warning, dash, base.SetupBrainCommand(brainCmd))}, warning
	}
	what := strings.Join(agents.Files, ", ")
	if len(agents.Changed) == 0 && !agents.HookChanged {
		details = append(details, "agent instructions already current ("+what+")")
	} else {
		details = append(details, "agent instructions written ("+what+") "+dash+" agents are told this brain exists")
	}
	// All three pre-edit hook states stay distinct. "wired" is work that was
	// done, "already wired" is work that was not needed, and a decline is a
	// choice this run must not quietly undo.
	switch {
	case declinedHooks:
		details = append(details, "pre-edit hook not wired (--no-agent-hooks), remembered")
	case agents.Hook != "" && agents.HookChanged:
		details = append(details, fmt.Sprintf("pre-edit hook wired (%s) %s facts about a file arrive as it is edited", agents.Hook, dash))
	case agents.Hook != "":
		details = append(details, fmt.Sprintf("pre-edit hook already wired (%s)", agents.Hook))
	case agents.Detail != "":
		warning = agents.Detail
		details = append(details, agents.Detail)
	}
	return details, warning
}

func describeDaemonState(state daemonState, brainCmd string) string {
	if state.Detail != "" && state.Installed {
		detail := state.Detail
		state.Detail = ""
		return describeDaemonState(state, brainCmd) + "; " + detail
	}
	switch {
	case state.Detail != "" && !state.Installed:
		return state.Detail
	case state.Running && !state.Current:
		// A running watcher whose unit on disk is not what this repo would write
		// is the one state a reader most needs told. inspectDaemon computed
		// Current and this function dropped it, so a watcher installed by a
		// different build or a different name reported as plain "running" and
		// nothing in `status` said otherwise.
		//
		// The hashed label it used to carry is gone from all four branches: it
		// identified nothing a reader could act on. It stays in --json
		// (daemonState.Label) for anything that needs the service id.
		return fmt.Sprintf("running, but the installed unit is stale and not current; re-run `%s setup`", base.SetupBrainCommand(brainCmd))
	case state.Running:
		return "running"
	case state.Installed && !state.Current:
		return "installed but not running, and not current"
	case state.Installed:
		return "installed but not running"
	default:
		return "not installed"
	}
}

// refreshSetupPushHook refreshes an ALREADY-INSTALLED pre-push hook and
// returns the detail line describing what happened.
//
// Setup owns the upgrade beat because the baked plugin path goes stale when
// the binary moves and the hook's [ -x ] guard turns that into a silent
// no-op. Fresh installs stay with `connect`: a bare setup after a disconnect
// must not resurrect the hook, and setup on a never-connected repo must not
// rearrange another tool's hooks for a feature that is switched off.
//
// Every branch returns a line, including the ones that did nothing, because
// these are phase DETAILS -- `--verbose` only -- and the reader who asked for
// them is the one who wants to know why their push is not uploading.
func refreshSetupPushHook(ctx context.Context, opts Options, repoDir, brainCmd string) []string {
	state, path, err := refreshPushHook(ctx, opts, repoDir)
	if err != nil {
		return []string{"push hook not refreshed (uploads ride the daemon tick): " + setupui.FlattenText(err.Error())}
	}
	switch state {
	case pushHookSharedDir:
		return []string{"push hook: core.hooksPath is shared across repos (" + path + "); not managed - uploads ride the daemon tick"}
	case pushHookAbsent:
		return []string{"push hook not installed (run `" + base.SetupBrainCommand(brainCmd) + " connect` to upload memories on push)"}
	case pushHookUnchanged:
		return []string{"push hook already current (" + path + ")"}
	case pushHookChainedIn:
		return []string{"push hook runs via the host CLI's chain (" + path + ")"}
	default:
		return []string{"push hook refreshed (" + path + ")"}
	}
}
