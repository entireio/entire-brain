package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/entireio/entire-brain/internal/setupui"
	"github.com/entireio/entire-brain/internal/tui"
)

// setup_ui.go is the seam between setup's orchestration and the seven-phase
// presentation in internal/setupui. It owns three things:
//
//   - setupSink, the terminal implementation of setupui.Sink. It is the only
//     place in this package that knows a phase line is painted by the shared
//     live-line owner rather than written to a file descriptor.
//   - setupStepSink, which routes the NESTED refresh reporter's chatter into
//     the open phase's transient line. That reporter emits one permanent line
//     per stage, which is twenty-five lines of someone else's phases inside
//     setup's one Build phase; as a transient they are repaints on a terminal
//     and nothing at all in a pipe, which is exactly the contract setupui
//     states ("identical under a pipe, minus the repaints").
//   - the Detect phase's probes, so the renderer keeps taking a filled struct
//     and never shells out.

// setupSink paints setupui's model through this package's progress reporter.
//
// Begin/Live are TTY-only on purpose. The plain path announces a task the
// moment it starts (so a log reads in event order), which for a composed phase
// row would print the phase twice: once as "setup: Detect starting" and again
// as the settled row. A pipe therefore gets the settled rows alone, which is
// the same model with the repaints removed rather than a second code path.
type setupSink struct {
	progress *refreshProgress
	rend     *setupui.Renderer
	tty      bool
	mu       sync.Mutex
	task     *refreshProgressTask
}

// newSetupSink builds the sink and the renderer that composes its rows. The
// renderer is derived from the PROGRESS reporter's capabilities rather than
// detected again from the writer: asking the environment a second time is how
// the glyph set a row is composed with drifts from the one the spinner beside
// it is painted with.
func newSetupSink(progress *refreshProgress) *setupSink {
	var base *tui.Renderer
	if progress != nil {
		base = progress.render
	}
	caps := tui.PlainCaps()
	if base != nil {
		caps = base.Caps()
	}
	return &setupSink{
		progress: progress,
		rend:     setupui.NewRendererWidth(base, setupRenderWidth(caps, os.LookupEnv)),
		tty:      progress.TTY(),
	}
}

// setupPipeWidth is the budget for a destination that has no physical row.
//
// setupui truncates every row to its width so a line can never wrap, and 80 is
// the right assumption for a terminal that did not report one. A PIPE is not
// that: it cannot wrap, so clipping there is not protection, it is deleting
// the end of a line nobody can get back — and the two rows this block exists
// to carry are the repo path and the brain path, which are routinely longer
// than 67 cells once the gutter is paid for. An explicit COLUMNS still wins,
// because `setup | head` at a stated width is how the budget gets tested.
const setupPipeWidth = 500

// setupRenderWidth answers "how many cells may a row use" for this
// destination: what the terminal reported, then COLUMNS, then 80 for a
// terminal that reported nothing and setupPipeWidth for something that is not
// a terminal at all.
func setupRenderWidth(caps tui.Caps, lookup tui.EnvLookup) int {
	if caps.Width > 0 || caps.TTY {
		return setupui.ResolveWidth(caps, lookup)
	}
	if lookup != nil {
		if _, ok := lookup("COLUMNS"); ok {
			return setupui.ResolveWidth(caps, lookup)
		}
	}
	return setupPipeWidth
}

// Renderer is the renderer every row in this run is composed with.
func (s *setupSink) Renderer() *setupui.Renderer { return s.rend }

// Begin opens the phase's transient line.
func (s *setupSink) Begin(accent tui.Phase, label string) {
	if s == nil || !s.tty {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.task = s.progress.BeginPhase(label, accent)
}

// Live updates the transient line. It can never produce a permanent row, and
// it never touches the live terminal's lock: a sub-step can reach here from
// inside printAroundLiveLine, which already holds that lock, and taking it
// again deadlocks the process. The spinner repaints the new label within
// 100ms. See refreshProgressTask.SetLabel.
func (s *setupSink) Live(label string) {
	if s == nil || !s.tty {
		return
	}
	s.mu.Lock()
	task := s.task
	s.mu.Unlock()
	task.SetLabel(label)
}

// Settle closes the transient line and writes the composed row.
func (s *setupSink) Settle(line setupui.Line) {
	if s == nil {
		return
	}
	s.Suspend()
	s.progress.EmitLine(s.rend.Line(line))
}

// Interject writes rows while a phase is still OPEN, without ending it. The
// daemon pre-flight warning needs this: it has to be on screen before the work
// it is warning about starts, and routing it through the run's Emit would
// abandon the live line it is standing in front of.
func (s *setupSink) Interject(lines ...setupui.Line) {
	if s == nil {
		return
	}
	for _, line := range lines {
		s.progress.EmitLine(s.rend.Line(line))
	}
}

// Suspend ends the open transient line and writes nothing. A caller that is
// about to write to the terminal itself — the consent prompt — calls this
// first, because a prompt painted onto the row the repaints own is erased by
// the next repaint, mid-question.
func (s *setupSink) Suspend() {
	if s == nil {
		return
	}
	s.mu.Lock()
	task := s.task
	s.task = nil
	s.mu.Unlock()
	task.Abandon()
}

// setupStepSink turns a byte stream into sub-step labels.
//
// The nested refresh reporter writes whole lines when its destination is not a
// terminal, which is what this is. Each one names a stage inside setup's Build
// phase ("semantic index: parsing sources"), so each one is a SUB-STEP: it
// feeds the phase's transient line and never becomes a row. A line that
// announces a warning is kept instead, because a warning that only ever
// existed as a repaint has not been reported.
type setupStepSink struct {
	mu  sync.Mutex
	buf []byte
	fn  func(string)
	// inWarning is carried across Write calls: a multi-line warning can be
	// split across them, and a continuation that arrives in the next write is
	// the same warning.
	inWarning bool
	warnings  []string
}

// bind attaches the sub-step consumer. A nil fn drops everything, which is the
// state before the phase opens and after it settles.
func (s *setupStepSink) bind(fn func(string)) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.fn = fn
	s.mu.Unlock()
}

// Warnings are the "warning: ..." lines seen so far, in order.
func (s *setupStepSink) Warnings() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.warnings...)
}

// maxStepLineBytes bounds the partial-line buffer. A producer that never
// writes a newline must not be able to grow this without limit.
const maxStepLineBytes = 8 << 10

func (s *setupStepSink) Write(p []byte) (int, error) {
	if s == nil {
		return len(p), nil
	}
	s.mu.Lock()
	s.buf = append(s.buf, p...)
	var lines []string
	for {
		index := strings.IndexByte(string(s.buf), '\n')
		if index < 0 {
			break
		}
		lines = append(lines, string(s.buf[:index]))
		s.buf = s.buf[index+1:]
	}
	if len(s.buf) > maxStepLineBytes {
		lines = append(lines, string(s.buf))
		s.buf = s.buf[:0]
	}
	fn := s.fn
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		label, prefixed := setupStepLine(line)
		if label == "" {
			// A blank line closes a warning's continuation, so unrelated
			// output written after it is not swept into the warning.
			s.inWarning = false
			continue
		}
		switch after, isWarning := strings.CutPrefix(label, "warning: "); {
		case isWarning:
			s.warnings = append(s.warnings, strings.TrimSpace(after))
			s.inWarning = true
		case s.inWarning && !prefixed && len(s.warnings) > 0:
			// A Go error is routinely multi-line -- errors.Join concatenates
			// its causes with a newline, and the memory path joins a
			// deterministic failure with a run failure before it reaches a
			// `warning: %v` line. Keeping only the prefixed line dropped every
			// cause after the first, which is the same information loss this
			// carries the warning up to avoid. A line the nested REPORTER
			// wrote carries its own prefix and is progress, never a cause, so
			// it ends the continuation instead of joining it.
			s.warnings[len(s.warnings)-1] += "; " + label
		case prefixed:
			s.inWarning = false
		}
		kept = append(kept, label)
	}
	s.mu.Unlock()

	if fn != nil {
		for _, label := range kept {
			fn(label)
		}
	}
	return len(p), nil
}

// setupStepLabel strips the reporter prefix a nested command writes, so the
// sub-step reads as part of setup's phase rather than as another command's
// output leaking through.
func setupStepLabel(line string) string {
	label, _ := setupStepLine(line)
	return label
}

// setupStepLine is setupStepLabel plus whether a reporter prefix was actually
// there. The flag is what separates another command's PROGRESS from the
// continuation lines of a multi-line warning, which carry no prefix at all.
func setupStepLine(line string) (string, bool) {
	line = strings.TrimRight(line, "\r")
	line = strings.TrimSpace(line)
	for _, prefix := range []string{"refresh: ", "setup: "} {
		if after, ok := strings.CutPrefix(line, prefix); ok {
			return strings.TrimSpace(after), true
		}
	}
	return line, false
}

// setupEstimatedInstantComponents is the Build bar's denominator before a
// previous run has recorded the real one. The free deterministic path reports
// nine components on an ordinary repository; a repo where one cannot even be
// attempted reports fewer, and one with extra sources reports more, so this is
// an ESTIMATE and the bar is clamped to never exceed itself (see
// setupBuildTotal). It is only ever the label of a transient line.
const setupEstimatedInstantComponents = 9

// setupBuildTotal sizes the Build bar: the previous run's component count when
// there is one, the estimate otherwise, and never fewer than the components
// already seen.
func setupBuildTotal(recorded, done int) int {
	total := recorded
	if total <= 0 {
		total = setupEstimatedInstantComponents
	}
	if done > total {
		total = done
	}
	return total
}

// setupContentComponents are the sources that make a brain able to ANSWER.
//
// This is the whole of the exit-code contract. `setup --help` used to promise
// exit 0 "whenever the brain is queryable -- that is, whenever at least one
// instant-phase component built", and those two clauses are not the same
// claim: on a dirty worktree the seed, semantic and history components all
// refuse while docs, entities and memory build, so the proxy said exit 0 for a
// brain that cannot answer a question. A script doing `setup && query` then
// proceeded on it.
//
// Entity and doc indexes are deliberately NOT here. They build over a tree
// whose searchable content was refused, so their presence is not evidence that
// anything can be retrieved; these three are the sources `query`, `brief` and
// `overview` actually read.
var setupContentComponents = map[string]bool{
	brainComponentSemantic: true,
	brainComponentSeed:     true,
	brainComponentHistory:  true,
}

// setupBrainQueryable reports whether the instant phase left a brain that can
// answer. A phase that reported NO components at all is the pre-component
// contract — an injected step, an older build — and is read as built, because
// reading silence as failure would fail every such caller.
func setupBrainQueryable(components []setupComponent) bool {
	if len(components) == 0 {
		return true
	}
	for _, component := range components {
		if component.failed() {
			continue
		}
		if setupContentComponents[component.Name] {
			return true
		}
	}
	return false
}

// setupRepoFacts probes the repository for the Detect phase.
//
// known is false when git could not answer, and nothing downstream may read
// "no commits" out of that: a fixture with no scripted git, a repository on a
// filesystem that just went away, and an empty repository are three different
// states, and only the third one is allowed to change what setup does.
func setupRepoFacts(ctx context.Context, runner CommandRunner, repoDir string) (facts setupui.RepoFacts, known bool) {
	facts.Name = filepath.Base(repoDir)
	facts.Path = repoDir
	if dirty, err := worktreeDirty(ctx, runner, repoDir); err == nil {
		facts.Dirty = dirty
	}
	branch, branchErr := gitScalar(ctx, runner, repoDir, "symbolic-ref", "--quiet", "HEAD")
	branch = strings.TrimSpace(branch)

	count, err := gitScalar(ctx, runner, repoDir, "rev-list", "--count", "HEAD")
	if err == nil {
		if parsed, convErr := strconv.Atoi(strings.TrimSpace(count)); convErr == nil && parsed >= 0 {
			facts.Commits = parsed
			facts.Detached = branchErr != nil && parsed > 0
			return facts, true
		}
		return facts, false
	}

	// rev-list failed, and "no commits" must be SHOWN rather than inferred
	// from that: it also fails when git is unavailable, and answering that
	// with "no commits yet" is a confident wrong diagnosis that would skip
	// every later phase. symbolic-ref SUCCEEDING is the positive evidence —
	// git ran, the repository is readable, and it named the branch HEAD points
	// at even though nothing has been committed to it. A detached HEAD has no
	// symbolic ref and falls through as unknown, which is right: it is not
	// this condition. (Same probe, same reasoning, as nameDegenerateRepoFailure.)
	if branchErr != nil || branch == "" {
		return facts, false
	}
	if _, resolved := gitScalar(ctx, runner, repoDir, "rev-parse", "--verify", "--quiet", branch); resolved != nil {
		facts.Commits = 0
		return facts, true
	}
	return facts, false
}

// setupGraphProviderFacts asks the provider what it is. One subprocess, a hard
// timeout, and every failure is an ABSENT provider rather than a setup
// failure: the Detect phase spends nothing and decides nothing, it only says
// what the later phases will be able to offer.
func setupGraphProviderFacts(ctx context.Context, runner CommandRunner, repoDir, brainCmd string) setupui.ProviderFacts {
	facts := setupui.ProviderFacts{Remedy: setupProviderInstallRemedy}
	if runner == nil {
		return facts
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	stdout, _, err := runner.Run(probeCtx, repoDir, "entire", "graph", "version", "--json")
	if err != nil {
		return facts
	}
	var info struct {
		Provider         string `json:"provider"`
		Version          string `json:"version"`
		IdentityRevision string `json:"identity_revision"`
	}
	if err := json.Unmarshal(stdout, &info); err != nil {
		return facts
	}
	facts.Name = strings.TrimSpace(info.Provider)
	if facts.Name == "" {
		return facts
	}
	facts.Version = strings.TrimSpace(info.Version)
	facts.Semantic = setupui.Capability{OK: true}
	revision := strings.TrimSpace(info.IdentityRevision)
	facts.Entities = setupui.Capability{
		OK:     revision != "",
		Detail: entityRevisionDetail(revision),
		Remedy: setupProviderInstallRemedy,
	}
	_ = brainCmd
	return facts
}

// setupProviderInstallRemedy is the runnable form. `scripts/install.sh from the
// entire-brain checkout` assumes the reader has the source tree; this is what
// `entire plugin search graph` itself prints.
const setupProviderInstallRemedy = "entire plugin install graph"

func entityRevisionDetail(revision string) string {
	if revision == "" {
		return ""
	}
	return "revision " + revision
}

// setupHostedFacts reads the repo-level hosted binding. An unreadable binding
// is reported as unbound with the read error as its note: claiming a binding
// this command could not read is the one answer that could cost someone data.
func setupHostedFacts(brainDir, brainCmd string) setupui.HostedFacts {
	facts := setupui.HostedFacts{Remedy: setupBrainCommand(brainCmd) + " connect"}
	binding, present, err := readHostedRepoBinding(brainDir)
	switch {
	case err != nil:
		facts.Note = "binding unreadable: " + setupui.FlattenError(err)
	case !present:
		// setupui's own default names the ticket that owns the gap.
	case binding.OptedOut:
		facts.Note = "disconnected by `" + setupBrainCommand(brainCmd) + " disconnect`"
	default:
		facts.Bound = true
		facts.Target = binding.RepoID
		if binding.BaseURL != "" {
			facts.Target += " at " + binding.BaseURL
		}
	}
	return facts
}

// detectSetupFacts fills the Detect phase's struct. Every probe is read-only
// and none of them can fail the run.
func detectSetupFacts(ctx context.Context, opts Options, setupOpts setupCommandOptions, repoDir, brainDir, brainCmd string, phase *setupui.PhaseRun) (setupui.Detected, bool) {
	var detected setupui.Detected

	phase.Step("git state")
	repo, commitsKnown := setupRepoFacts(ctx, opts.Runner, repoDir)
	detected.Repo = repo

	phase.Step("agent CLI")
	detected.Agent = setupui.AgentFacts{
		Model:  strings.TrimSpace(setupOpts.model),
		Effort: strings.TrimSpace(setupOpts.effort),
		Remedy: "install codex or claude, then re-run `" + setupBrainCommand(brainCmd) + " setup`",
	}
	if agent := strings.TrimSpace(setupOpts.agent); agent != "" && agent != "none" && agent != "auto" {
		detected.Agent.Name = agent
	} else {
		// No agent means nothing can distill, so the model and effort that
		// would have been used are not capabilities — printing them would
		// describe a run that cannot happen.
		detected.Agent.Model, detected.Agent.Effort = "", ""
	}

	phase.Step("graph provider")
	detected.Provider = setupGraphProviderFacts(ctx, opts.Runner, repoDir, brainCmd)

	phase.Step("hosted binding")
	detected.Hosted = setupHostedFacts(brainDir, brainCmd)

	return detected, commitsKnown
}

// setupHookSummary is the Agents phase's one line: what the session-end hook
// is, and when it is not what it should be, the one thing to do about it. The
// four cases and their wording are unchanged; only their destination is.
func setupHookSummary(hook setupHookState, dash string) (string, bool) {
	switch {
	case hook.Installed && !hook.EntireCLI:
		return "session-end hook wired (" + hook.Source + ") but `" + entireBinaryName +
			"` is not on PATH " + dash + " sessions will NOT distill until the Entire CLI is installed", false
	case hook.Installed:
		return "session-end hook already wired (" + hook.Source + ")", true
	case !hook.EntireCLI:
		return "session-end hook not wired and `" + entireBinaryName + "` is not on PATH " + dash +
			" install the Entire CLI, then run `entire enable` in this repo", false
	default:
		return "session-end hook not wired " + dash +
			" run `entire enable` in this repo so each session distills as it ends (SPENDS TOKENS per session)", false
	}
}

// setupPreflightRows wraps the daemon pre-flight warning to the terminal.
//
// The text is unchanged — it is the best line in the command: it names the
// artifact, says it starts again at every login, and gives both escape
// hatches. What changes is that it is now re-flowed instead of emitted at its
// natural 250 characters, which wrapped into mush in any pane narrower than
// that. It is rendered FLUSH, at column zero: a warning that precedes every
// phase has no phase column to sit in.
func setupPreflightRows(rend *setupui.Renderer, line string) []setupui.Line {
	if strings.TrimSpace(line) == "" {
		return nil
	}
	rows := make([]setupui.Line, 0, 4)
	for _, text := range rend.WrapFlush(line) {
		rows = append(rows, setupui.Line{Text: text, Accent: tui.PhaseDaemon, Flush: true})
	}
	return rows
}

// setupWarningRows restate the report's warnings at the end of the run.
//
// A warning is not a phase outcome — most of them already appeared as the skip
// reason on the phase they came from — but it is the only record of the ones
// that did not, and a warning that scrolled away during a twenty-second build
// has not been reported.
func setupWarningRows(warnings []string) []setupui.Line {
	rows := make([]setupui.Line, 0, len(warnings))
	for _, warning := range warnings {
		text := strings.TrimSpace(setupui.FlattenText(warning))
		if text == "" {
			continue
		}
		rows = append(rows, setupui.Line{
			Text:   "warning  " + text,
			Accent: tui.PhaseSkipped,
			Glyph:  setupui.GlyphSkipped,
		})
	}
	return rows
}

// setupNextSteps are the runnable follow-ups the Ready block prints. Every one
// of them is spelled the way the reader reached this binary.
func setupNextSteps(brainCmd string, daemonInstalled, agentsInstalled bool, pendingSessions int) []setupui.NextStep {
	cmd := setupBrainCommand(brainCmd)
	next := []setupui.NextStep{
		{Command: cmd + " overview", Why: "what this project is"},
		{Command: cmd + ` brief "<task>"`, Why: "task-shaped context"},
		{Command: cmd + " status", Why: "backfill progress and daemon health"},
	}
	if pendingSessions > 0 {
		next = append(next, setupui.NextStep{
			Command: cmd + " distill",
			Why:     "turn " + strconv.Itoa(pendingSessions) + " pending session(s) into facts (spends tokens)",
		})
	}
	if daemonInstalled {
		next = append(next, setupui.NextStep{
			Command: cmd + " setup --uninstall-daemon",
			Why:     "stop and remove the background watcher",
		})
	}
	// Only when the wiring did NOT land. Naming init-agents on a run that
	// already wrote the guide would send the reader to re-do work setup just
	// did; naming it on a run that failed is the only way they learn the brain
	// is invisible.
	if !agentsInstalled {
		next = append(next, setupui.NextStep{
			Command: cmd + " init-agents",
			Why:     "tell agents this brain exists (nothing will use it until this runs)",
		})
	}
	return next
}

// setupConsentOverride maps the flags onto the consent gate. --no-backfill,
// --yes and --backfill all suppress the prompt; they are the only ways to
// answer it without a terminal, and --yes/--backfill are the only ones that
// can answer it YES.
func setupConsentOverride(setupOpts setupCommandOptions) setupui.Override {
	switch {
	case setupOpts.noBackfill:
		return setupui.OverrideNo
	// --backfill is an explicit, typed demand to start the token spend, and
	// after a remembered decline it is the ONLY way back to it. Making the
	// reader then answer a prompt they have already answered on the command
	// line would make the affirmative flag worthless in exactly the
	// non-interactive runs it exists for.
	case setupOpts.yes, setupOpts.backfill:
		return setupui.OverrideYes
	default:
		return setupui.OverrideNone
	}
}

// setupPromptWriter is where the consent prompt is painted. It is the progress
// destination, not stdout, so `--json` (whose progress goes to io.Discard)
// cannot have a question printed into its report.
func setupPromptWriter(out io.Writer) io.Writer {
	if out == nil {
		return io.Discard
	}
	return out
}
