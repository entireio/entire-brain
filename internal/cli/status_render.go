package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/tui"
)

// status_render.go owns the TEXT rendering of `entire-brain status`. Nothing
// here changes what the command computes, and in particular nothing here is
// reachable from --json: the machine contract is byte-identical to what it was
// before this file existed, and the JSON golden test enforces that.
//
// The problem it solves is that the default text report had become unreadable.
// On a real repository it printed forty-four "partial failure: E_MINIFIED …"
// lines, then the SAME forty-four files again as "blind spots", then a
// twenty-three-entry relation-type histogram — roughly 130 lines, of which
// about six carried information a person had asked for. The signal (is the
// backfill progressing? is the watcher alive? did a component fail?) was
// somewhere in the middle.
//
// So there are now two reports:
//
//   - DEFAULT: identity, the onboarding block, and a one-line health verdict.
//   - --verbose: everything the old report printed, except that repeated
//     warnings are collapsed to one line per group and no file is ever listed
//     twice across two sections.
//
// The grouping is not cosmetic. A repository that vendors JSON corpora is not
// forty-four separate problems; it is one fact ("data files are not source, and
// were not parsed as source") that the reader should be able to dismiss in one
// glance and go looking at in `doctor` if they care.

// statusDataFileExtensions are the file types whose "not parsed as source"
// warning is expected rather than actionable: a JSON corpus, a lockfile, a
// sourcemap and a minified bundle are all DATA, and reporting each one as an
// individual semantic blind spot is noise with a 44:1 ratio.
var statusDataFileExtensions = map[string]bool{
	".json": true, ".jsonl": true, ".ndjson": true, ".csv": true, ".tsv": true,
	".map": true, ".lock": true, ".snap": true, ".svg": true, ".min.js": true,
	".pb": true, ".bin": true, ".parquet": true,
}

// statusBlindSpotGroup is one collapsed reason plus the files it covers.
type statusBlindSpotGroup struct {
	Code string
	// Reason is the shared explanation, taken from the first member.
	Reason string
	Paths  []string
	// DataFiles reports whether every member looks like a data file rather than
	// unparsed source, which is what licenses the short "data files skipped"
	// phrasing instead of a warning the reader should act on.
	DataFiles bool
}

// Count reports how many files the group covers.
func (g statusBlindSpotGroup) Count() int { return len(g.Paths) }

// Summary is the single line a collapsed group prints.
func (g statusBlindSpotGroup) Summary() string {
	noun := "files"
	if g.DataFiles {
		noun = "data files"
	}
	if g.Count() == 1 {
		noun = strings.TrimSuffix(noun, "s")
	}
	reason := strings.TrimSpace(g.Reason)
	if reason == "" {
		reason = strings.TrimSpace(g.Code)
	}
	return fmt.Sprintf("%d %s skipped (%s)", g.Count(), noun, reason)
}

// isStatusDataFile reports whether a path is obviously data rather than source.
func isStatusDataFile(path string) bool {
	lowered := strings.ToLower(path)
	if strings.HasSuffix(lowered, ".min.js") || strings.HasSuffix(lowered, ".min.css") {
		return true
	}
	return statusDataFileExtensions[filepath.Ext(lowered)]
}

// statusBlindSpotReason shortens a provider's explanation to the clause that
// distinguishes it. The full sentence lives in doctor.
func statusBlindSpotReason(code, detail string) string {
	detail = strings.TrimSpace(detail)
	switch {
	case strings.Contains(detail, "minified"):
		return "minified/bundled"
	case detail == "":
		return strings.TrimSpace(code)
	}
	if idx := strings.Index(detail, ";"); idx > 0 {
		detail = detail[:idx]
	}
	return detail
}

// groupStatusBlindSpots collapses per-file records into one group per reason,
// in a stable order (most files first, then code).
func groupStatusBlindSpots(spots []brainBlindSpot) []statusBlindSpotGroup {
	byCode := map[string]*statusBlindSpotGroup{}
	order := []string{}
	for _, spot := range spots {
		key := spot.Code + "\x00" + statusBlindSpotReason(spot.Code, spot.Detail)
		group, ok := byCode[key]
		if !ok {
			group = &statusBlindSpotGroup{
				Code:      spot.Code,
				Reason:    statusBlindSpotReason(spot.Code, spot.Detail),
				DataFiles: true,
			}
			byCode[key] = group
			order = append(order, key)
		}
		group.Paths = append(group.Paths, spot.Path)
		if !isStatusDataFile(spot.Path) {
			group.DataFiles = false
		}
	}
	groups := make([]statusBlindSpotGroup, 0, len(order))
	for _, key := range order {
		groups = append(groups, *byCode[key])
	}
	sort.SliceStable(groups, func(i, j int) bool {
		if groups[i].Count() != groups[j].Count() {
			return groups[i].Count() > groups[j].Count()
		}
		return groups[i].Code < groups[j].Code
	})
	return groups
}

// blindSpotsFromWarnings projects semantic warnings onto the same shape as
// blind spots, so the two sections can be compared and de-duplicated instead of
// each printing the same forty-four files.
func blindSpotsFromWarnings(warnings []semanticWarning) []brainBlindSpot {
	out := make([]brainBlindSpot, 0, len(warnings))
	for _, warning := range warnings {
		out = append(out, brainBlindSpot{Path: warning.Path, Code: warning.Code, Detail: warning.Detail})
	}
	return out
}

// blindSpotKey identifies one (code, path) record across sections.
func blindSpotKey(spot brainBlindSpot) string { return spot.Code + "\x00" + spot.Path }

// subtractBlindSpots removes records already reported by another section.
func subtractBlindSpots(spots []brainBlindSpot, seen map[string]bool) []brainBlindSpot {
	out := make([]brainBlindSpot, 0, len(spots))
	for _, spot := range spots {
		if seen[blindSpotKey(spot)] {
			continue
		}
		out = append(out, spot)
	}
	return out
}

// statusCauseWidth bounds the one-line cause so the short report stays short
// even when a provider hands back a paragraph-length axis detail. It counts
// CHARACTERS, not bytes — see truncateStatusCause.
const statusCauseWidth = 140

// statusCauseEllipsis marks a cause that did not fit.
const statusCauseEllipsis = "..."

// truncateStatusCause bounds a cause at statusCauseWidth runes.
//
// The general-purpose truncateString measures its budget in BYTES. For the
// ASCII causes this file was written against the two are the same, but a
// provider detail carrying a non-ASCII path, identifier or quotation mark is
// charged two to four bytes per character, so a perfectly short cause was cut
// to a third of its length. Counting runes makes the advertised width mean
// what it says, and cutting on a rune index can never split a character.
func truncateStatusCause(cause string) string {
	if cause == "" {
		return ""
	}
	runes := []rune(cause)
	if len(runes) <= statusCauseWidth {
		return cause
	}
	// The RESERVE is counted in runes too.
	//
	// Measuring the input in runes and the ellipsis in bytes is the same
	// rune-versus-byte confusion this function exists to fix, just on the other
	// operand. It happens to agree for ASCII "...", and silently
	// over-reserves the moment the marker becomes "…" -- which is one
	// const edit away, in a function whose entire comment is about this class
	// of bug.
	ellipsisWidth := utf8.RuneCountInString(statusCauseEllipsis)
	limit := statusCauseWidth
	suffix := ""
	if statusCauseWidth > ellipsisWidth {
		limit = statusCauseWidth - ellipsisWidth
		suffix = statusCauseEllipsis
	}
	return string(runes[:limit]) + suffix
}

// statusHealth is the one-line verdict the short report ends on.
type statusHealth struct {
	Issues   int
	Warnings int
	Failed   []string
	Severity string
	// An old layout needs the migration bridge, even when Graph is absent.
	BrainLayoutRemedy string
	// BrainMissing reports that this repository has no brain at all: the
	// manifest is absent and every source is unbuilt. It is a STATE, not a
	// problem count, which is why it cannot be folded into Issues — and why
	// the old verdict got it exactly backwards. Nothing built means nothing
	// broken, so every counter read zero and the first command a new user runs
	// answered "healthy" about a brain that does not exist.
	BrainMissing bool
	// SemanticMissing reports that a brain exists but its manifest declares no
	// semantic index, so `inspect code`, `query` and `brief` have nothing to
	// read. Freshness cannot see this either: with no semantic source there is
	// no freshness report, and brainStatusFreshnessSeverity returns "" — which
	// Healthy() used to accept as "fine".
	SemanticMissing bool
	// Cause names WHICH axis is not current, in the words `overview` uses. See
	// statusFreshnessCause.
	Cause string
	// HostCLIMissing reports that the Entire CLI this plugin runs under is not
	// installed. Sessions, the semantic index and the entity index are all built
	// THROUGH it, so while it is absent no refresh brings any of them back and
	// the verdict must say so rather than naming a command that cannot repair
	// what it just listed.
	HostCLIMissing bool
	// SemanticProviderAbsent reports that the semantic index is missing AND
	// the host CLI cannot launch a graph provider, so the refresh remedy alone
	// cannot build it — the honest next step is the plugin install. Set only
	// when the host CLI itself is present.
	SemanticProviderAbsent bool
	// SemanticStoreUnsupported reports that a graph provider IS installed but
	// predates the read-only store contract (graph_store.go), so no refresh,
	// commit, or stash builds the semantic index — the only remedy is the
	// Graph upgrade. Without this flag the dirty-tree override below claimed
	// "commit or stash" as the fix for a failure committing cannot touch.
	SemanticStoreUnsupported bool
	// DirtyWorktree reports uncommitted changes in the working tree. Both stages
	// `refresh --agent none` would rebuild REFUSE that state by design — seed
	// with "dirty_worktree: refusing to seed uncommitted content without
	// --worktree", semantic with the same refusal to index it — so recommending
	// that command to a reader with a dirty tree is recommending an exit 1.
	DirtyWorktree bool
	// NoCapturedSessions reports the session export failing because Entire has
	// no readable checkpoints — the state every repository is in between
	// `entire enable` and its first finished agent session. `refresh --agent
	// none` re-runs that same export as its FIRST stage and aborts on the same
	// error, so naming it here hands the reader a remedy that reproduces the
	// condition it is supposed to clear. The remedy is a finished session, and
	// setupNoSessionsHint is where that sentence already lives.
	NoCapturedSessions bool
}

// statusRemedyRefresh is the deterministic rebuild. Named, because it is also
// the ONE remedy a dirty working tree invalidates, and the override has to be
// scoped to exactly it: a dirty tree does not change the right answer for a
// brain that was never built (`setup`) or for a corrupt store (`doctor`).
const statusRemedyRefresh = "refresh --agent none"

// buildStatusHealth counts everything a reader would call a problem.
func buildStatusHealth(report brainStatusReport) statusHealth {
	health := statusHealth{
		Issues:   len(report.Issues),
		Warnings: len(report.Warnings) + len(report.Live.Warnings),
		Severity: brainStatusFreshnessSeverity(report),
		// "absent" is inspectBrainManifestHealth's own word for a brain that
		// was never built (and for one just removed by `reset --force`).
		BrainMissing: report.Brain.ManifestState == "absent",
		Cause:        statusFreshnessCause(report),
	}
	for _, issue := range report.Issues {
		if issue.Kind == "brain_layout" {
			health.BrainLayoutRemedy = issue.Action
			break
		}
	}
	// Only one of the two: "no brain" already implies "no semantic index", and
	// a verdict that said both would be reporting one fact twice.
	health.SemanticMissing = !health.BrainMissing && report.Semantic == nil
	// Probe only in the state the answer changes: a missing semantic index is
	// repaired by refresh when a provider is installed and by the plugin
	// install when none is. With no `entire` on PATH the install hint would be
	// unfollowable, so HostCLIMissing keeps owning that message.
	health.SemanticProviderAbsent = health.SemanticMissing && hostCLIOnPath() && !semanticProviderOnHost()
	if report.Onboarding != nil {
		health.HostCLIMissing = report.Onboarding.HostCLIMissing
		for _, component := range report.Onboarding.Components {
			// Matched on the persisted detail, not the state: new records say
			// "skipped", but a record written by an older build still says
			// "failed" for the same condition, and both need the same remedy.
			if component.Name == brainComponentSemantic && setupIsGraphStoreUnsupported(component.Detail) {
				health.SemanticStoreUnsupported = true
			}
			if component.State == "failed" {
				health.Failed = append(health.Failed, component.Name)
				if setupIsNoReadableSessions(component.Detail) {
					health.NoCapturedSessions = true
				}
			}
		}
	}
	// The live worktree, not a freshness axis. The `dirty-unindexed` axis is one
	// SYMPTOM of a dirty tree and it was the obvious thing to key on, but it is
	// not the condition: after `refresh --worktree` the seed axis reads
	// `dirty-indexed` and freshness aggregates to "ok", while `refresh --agent
	// none` goes on refusing to reseed the same uncommitted content. Asking the
	// working tree directly covers both.
	health.DirtyWorktree = report.Live.Dirty
	return health
}

// statusFreshnessCause is the store-validity signal, and it is the SAME one
// `overview` prints: semanticStaleReport opens the semantic SQLite store and
// runs `PRAGMA integrity_check` on it, records the failure as
// store=unsafe (…), and freshnessSummary renders that axis. `overview` calls
// freshnessSummary; the short status report kept only the aggregate severity
// and threw the axes away, so one shredded SQLite file, a semantic index one
// commit behind, and a provider that was skipped all printed the same single
// word.
func statusFreshnessCause(report brainStatusReport) string {
	parts := make([]string, 0, 2)
	for _, freshness := range []*staleReport{
		semanticFreshnessOf(report),
		retrievalFreshnessOf(report),
	} {
		if freshness == nil || freshness.Severity == "ok" {
			continue
		}
		if summary := freshnessSummary(*freshness); summary != "" {
			parts = append(parts, summary)
		}
	}
	return truncateStatusCause(strings.Join(parts, "; "))
}

func semanticFreshnessOf(report brainStatusReport) *staleReport {
	if report.Semantic == nil {
		return nil
	}
	return report.Semantic.Freshness
}

func retrievalFreshnessOf(report brainStatusReport) *staleReport {
	if report.Retrieval == nil {
		return nil
	}
	return report.Retrieval.Freshness
}

// Total is how many distinct problems the verdict is reporting.
func (h statusHealth) Total() int { return h.Issues + h.Warnings + len(h.Failed) }

// Healthy reports whether the verdict is "nothing to do". A degraded or unsafe
// freshness severity counts as unhealthy even with no issues: a stale index is
// the failure most likely to make the brain quietly wrong. An absent brain and
// an absent semantic index are never healthy either — they are the two states
// in which every counter reads zero because nothing was ever built.
func (h statusHealth) Healthy() bool {
	if h.BrainMissing || h.SemanticMissing {
		return false
	}
	return h.Total() == 0 && (h.Severity == "" || h.Severity == "ok")
}

// repairCommand selects the subcommand that can repair or diagnose this state.
func (h statusHealth) repairCommand() string {
	switch {
	case h.BrainMissing:
		return "setup"
	case h.SemanticMissing, h.Severity != "" && h.Severity != "ok":
		// Deterministic and token-free: the index is rebuilt without asking an
		// agent for anything, which is what a stale, missing or corrupt index
		// needs.
		return statusRemedyRefresh
	default:
		return "doctor"
	}
}

// nextStepMessage gives one actionable next step. Missing brain and blocked
// repair take precedence; diagnosed faults outrank a missing-host advisory.
func (h statusHealth) nextStepMessage(brainCmd string) string {
	brainCmd = base.SetupBrainCommand(brainCmd)
	command := "run `" + brainCmd + " " + h.repairCommand() + "`"
	switch {
	case h.BrainLayoutRemedy != "":
		return h.BrainLayoutRemedy
	case h.BrainMissing:
		return command
	case h.SemanticMissing && h.SemanticProviderAbsent:
		// Refresh cannot build what no installed provider produces; naming it
		// anyway sends the reader through an exit that changes nothing.
		return "the graph provider is not installed, so code context queries are skipped; install it with `entire plugin install graph`, then run `" + brainCmd + " " + base.SemanticIndexVerb + "`"
	case h.SemanticMissing && h.SemanticStoreUnsupported:
		// Same shape as the provider-absent case above, and it must outrank
		// the dirty-tree override below: neither refresh nor a commit builds
		// a store the installed Graph cannot produce, and pairing "semantic
		// failed" with "commit or stash" was exactly the misattribution this
		// case removes. The dirty tree, when present, still states its cause
		// on the line underneath (statusFreshnessCause).
		return setupGraphStoreUnsupportedHint(brainCmd)
	case h.DirtyWorktree && h.repairCommand() == statusRemedyRefresh:
		// The ONE remedy resolver for a dirty tree. Line() prints h.Cause
		// directly underneath this, and the cause used to carry a SECOND,
		// contradictory remedy of its own ("refresh with --worktree to include
		// current changes"). Causes state causes; this resolver owns the
		// remedy, and it names both ways out so neither has to be discovered
		// elsewhere.
		return setupDirtyWorktreeHint(brainCmd)
	case h.NoCapturedSessions && h.repairCommand() == statusRemedyRefresh:
		// Same shape as the dirty-tree override and scoped the same way: the
		// ONE remedy a failed session export invalidates is the one whose first
		// stage IS that export.
		return setupNoSessionsHint(brainCmd)
	case h.Issues > 0 || h.Severity != "" && h.Severity != "ok":
		return command
	case h.HostCLIMissing && len(h.Failed) > 0:
		return "`" + entireBinaryName + "` is not on PATH; install the Entire CLI, then run `" + brainCmd + " setup`"
	default:
		return command
	}
}

// Line renders the verdict as one warning line: the problems, then the one
// next step, then the cause underneath. It carries its own two-space indent so
// the caller can print it beside the other warning lines unchanged.
func (h statusHealth) Line(render *tui.Renderer, brainCmd string) string {
	brainCmd = base.SetupBrainCommand(brainCmd)
	if h.Healthy() {
		return "  " + render.Mark(tui.MarkDone) + " " + render.PhasePaint(tui.PhaseDone, "healthy")
	}
	parts := make([]string, 0, 6)
	if h.BrainMissing {
		parts = append(parts, "no brain for this repo yet")
	}
	if h.SemanticMissing {
		parts = append(parts, "no code index")
	}
	if len(h.Failed) > 0 {
		parts = append(parts, fmt.Sprintf("%s failed", strings.Join(h.Failed, ", ")))
	}
	if h.Issues > 0 {
		parts = append(parts, fmt.Sprintf("%d %s", h.Issues, plural(h.Issues, "issue")))
	}
	if h.Warnings > 0 {
		parts = append(parts, fmt.Sprintf("%d %s", h.Warnings, plural(h.Warnings, "warning")))
	}
	if h.Severity != "" && h.Severity != "ok" {
		parts = append(parts, "freshness "+h.Severity)
	}
	color := render.Theme().Flag
	if len(h.Failed) > 0 || h.Severity == "unsafe" {
		color = render.Theme().Bad
	}
	line := "  " + render.Paint(color, "! "+strings.Join(parts, ", ")) +
		statusSep(render) + render.Dim(h.nextStepMessage(brainCmd))
	if h.Cause != "" {
		line += "\n    " + render.Dim(h.Cause)
	}
	return line
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// statusLabelWidth aligns the identity block's label column; "Code index" and
// "Brainstore" are the widest labels.
const statusLabelWidth = 10

// statusSep is the dim mid-dot that joins facts on one line — the same
// separator grammar the host `entire status` uses.
func statusSep(render *tui.Renderer) string {
	return render.Dim(" " + render.Bullet() + " ")
}

// statusDot is the verdict glyph on the header line, the signal `entire
// status` leads with: green when healthy, red when something failed, amber for
// everything between.
func statusDot(render *tui.Renderer, health statusHealth) string {
	glyph := "●"
	if !render.Caps().Unicode {
		glyph = "*"
	}
	theme := render.Theme()
	color := theme.Flag
	switch {
	case health.Healthy():
		color = theme.Good
	case len(health.Failed) > 0 || health.Severity == "unsafe":
		color = theme.Bad
	}
	return render.Paint(color, glyph)
}

// statusFactLine is one row of the identity block: a dim, aligned label, the
// dim separator, and the plain value.
func statusFactLine(render *tui.Renderer, label, value string) string {
	return "  " + render.Dim(fmt.Sprintf("%-*s %s ", statusLabelWidth, label, render.Bullet())) + value
}

// groupDigits writes n with comma thousands separators: a five-digit memory
// count reads as a count, not a typo.
func groupDigits(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// statusMemoriesValue merges the manifest's fact count with the backfill
// progress, which used to be two "memories" lines in two blocks.
func statusMemoriesValue(render *tui.Renderer, report brainStatusReport) string {
	parts := make([]string, 0, 2)
	if f := report.Facts; f != nil {
		switch {
		case report.Integrity == "not_checked":
			parts = append(parts, groupDigits(f.Facts)+" declared")
		case f.Integrity == nil || f.Integrity.OK():
			parts = append(parts, groupDigits(f.Facts))
		case f.Integrity.Mode == factStoreDefectUnreadable:
			// A store that would not parse yielded no count at all, so "0
			// readable" would be a measurement nobody took. Say what is true:
			// it could not be read.
			parts = append(parts, groupDigits(f.Facts)+" declared, store unreadable")
		default:
			parts = append(parts, fmt.Sprintf("%s declared, %s readable", groupDigits(f.Facts), groupDigits(f.Integrity.Readable)))
		}
	}
	if o := report.Onboarding; o != nil {
		facts := o.Facts
		progress := ""
		switch {
		case facts.Running:
			progress = render.PhasePaint(tui.PhaseBackfill,
				fmt.Sprintf("%d/%d distilled, backfill running, pid %d", facts.Distilled, facts.Sessions, facts.PID))
		case facts.Sessions == 0:
			progress = render.Dim("no captured sessions yet")
		case facts.Pending() > 0:
			progress = render.Dim(fmt.Sprintf("%d/%d distilled, %d pending", facts.Distilled, facts.Sessions, facts.Pending()))
		default:
			progress = render.Dim("all distilled")
		}
		if len(parts) > 0 {
			progress = "(" + progress + ")"
		}
		parts = append(parts, progress)
	}
	return strings.Join(parts, " ")
}

// statusDaemonValue is the watcher's whole story on one line: state, last
// tick, what it covers, and whether background work is paused. A hashed
// launchd label answers nothing a reader of `status` came to ask; the
// workspaces it is watching answer it exactly.
func statusDaemonValue(render *tui.Renderer, report brainStatusReport, brainCmd string) string {
	onboarding := report.Onboarding
	state := describeDaemonState(onboarding.Daemon, brainCmd)
	if !onboarding.Daemon.Running || !onboarding.Daemon.Current {
		state = render.Paint(render.Theme().Flag, state)
	}
	if !onboarding.LastTickAt.IsZero() {
		age := humanizeAge(report.GeneratedAt.Sub(onboarding.LastTickAt))
		if onboarding.Daemon.Running {
			state += " " + render.Dim("(last tick "+age+" ago)")
		} else {
			state += " " + render.Dim("(last watcher tick "+age+" ago)")
		}
	}
	if onboarding.Daemon.Installed {
		if coverage := describeWatchPlanCoverage(onboarding.WatchPlan); coverage != "" {
			state += statusSep(render) + render.Dim(coverage)
		}
	}
	if onboarding.Paused {
		state += statusSep(render) + render.Paint(render.Theme().Flag, "background work: paused")
	}
	return state
}

// statusSourcesValue lists the built sources by name and demotes the rest to
// labelled groups, replacing the nine-glyph instant row: on a healthy brain
// the line is just the names, and anything else stands out by being different.
func statusSourcesValue(render *tui.Renderer, components []brainStatusComponent) string {
	var built, missing, failed, skipped []string
	for _, component := range components {
		switch component.State {
		case "failed":
			failed = append(failed, component.Name)
		case "missing":
			missing = append(missing, component.Name)
		case "skipped":
			skipped = append(skipped, component.Name)
		default:
			built = append(built, component.Name)
		}
	}
	parts := make([]string, 0, 4)
	if len(built) > 0 {
		parts = append(parts, strings.Join(built, ", "))
	}
	if len(failed) > 0 {
		parts = append(parts, render.Paint(render.Theme().Bad, "failed: "+strings.Join(failed, ", ")))
	}
	if len(missing) > 0 {
		parts = append(parts, render.Dim("missing: "+strings.Join(missing, ", ")))
	}
	if len(skipped) > 0 {
		parts = append(parts, render.Dim("skipped: "+strings.Join(skipped, ", ")))
	}
	return strings.Join(parts, statusSep(render))
}

// statusWarningLines collects everything that deserves a "!" line: unwired
// agents first (a brain no agent consults reads healthy everywhere else and is
// never used), the memory-store integrity warning, the verdict with its one
// next step and its cause, then each failed component's reason.
func statusWarningLines(render *tui.Renderer, report brainStatusReport, brainCmd string, health statusHealth) []string {
	theme := render.Theme()
	lines := make([]string, 0, 4)
	onboarding := report.Onboarding
	if onboarding != nil && onboarding.AgentsUnwired {
		lines = append(lines, "  "+render.Paint(theme.Flag, "! agents not wired")+
			statusSep(render)+render.Dim(fmt.Sprintf("run `%s setup` so your agent uses this brain", brainCmd)))
		if len(onboarding.AgentsMissing) > 0 {
			lines = append(lines, "    "+render.Dim("missing: "+strings.Join(onboarding.AgentsMissing, ", ")))
		}
	}
	if f := report.Facts; f != nil {
		warning := ""
		if f.Integrity != nil {
			warning = f.Integrity.Warning()
		} else if len(f.MissingBranches) > 0 {
			warning = missingFactStoreWarning(f.MissingBranches)
		}
		if warning != "" {
			lines = append(lines, "  "+render.Paint(theme.Bad, "! "+warning))
		}
	}
	// One remedy, one appearance. Two components failing for the same reason
	// -- the ordinary case for a dirty worktree, which refuses both seed and
	// semantic -- carry the same hint the verdict already states.
	said := map[string]bool{}
	if !health.Healthy() {
		lines = append(lines, health.Line(render, brainCmd))
		said[strings.TrimSpace(health.nextStepMessage(brainCmd))] = true
	}
	if onboarding == nil {
		return lines
	}
	for _, component := range onboarding.Components {
		// A skipped component with a recorded reason reports too — the reason
		// is the one thing the sources line cannot say — but dim rather than
		// as a warning, because a designed skip rendered in failure red is the
		// contradiction this distinguishes.
		skippedWithReason := component.State == "skipped" && strings.TrimSpace(component.Detail) != ""
		if component.State != "failed" && !skippedWithReason {
			continue
		}
		detail := strings.TrimSpace(component.Detail)
		if detail == "" {
			// A failure with no recorded reason is itself worth saying:
			// silence here would read as "no detail needed".
			detail = "no reason recorded"
		}
		if skippedWithReason {
			lines = append(lines, "  "+render.Dim(render.Dash()+" "+component.Name+": "+detail))
		} else {
			lines = append(lines, "  "+render.Paint(theme.Bad, "! "+component.Name+": "+detail))
		}
		if hint := strings.TrimSpace(component.Hint); hint != "" && !said[hint] {
			said[hint] = true
			lines = append(lines, "    "+render.Dim("hint: "+hint))
		}
	}
	return lines
}

// renderBrainStatusShort is the DEFAULT report, in the host CLI's card
// grammar: a verdict-coloured dot on the identity line, one aligned fact per
// line, a blank-line-separated warning block, and the footer hints. Everything
// else is one flag away.
func renderBrainStatusShort(out io.Writer, render *tui.Renderer, report brainStatusReport, brainCmd string) {
	brainCmd = base.SetupBrainCommand(brainCmd)
	health := buildStatusHealth(report)

	// The blank lines are the layout: they separate the report from the shell
	// prompt on both sides and the header, facts, warnings and footer from one
	// another. The path stays in --verbose; it is machine detail.
	fmt.Fprintln(out)
	// The accent on "Brain" matches setup's terse block and the host CLI's
	// own header, not the instant-phase cyan.
	fmt.Fprintf(out, "%s %s%s%s\n", statusDot(render, health),
		render.Bold(render.Paint(render.Theme().Accent, "Brain")),
		statusSep(render),
		render.Bold(valueOrUnset(report.Repo.Key)))
	fmt.Fprintln(out)

	if value := statusMemoriesValue(render, report); value != "" {
		fmt.Fprintln(out, statusFactLine(render, "Memories", value))
	}
	if onboarding := report.Onboarding; onboarding != nil {
		fmt.Fprintln(out, statusFactLine(render, "Daemon", statusDaemonValue(render, report, brainCmd)))
		if len(onboarding.Components) > 0 {
			fmt.Fprintln(out, statusFactLine(render, "Sources", statusSourcesValue(render, onboarding.Components)))
		}
	}
	if s := report.Semantic; s != nil && s.Coverage != nil {
		fmt.Fprintln(out, statusFactLine(render, "Code index",
			fmt.Sprintf("%s files, %s symbols", groupDigits(s.Coverage.Files), groupDigits(s.Coverage.Symbols))))
	}
	fmt.Fprintln(out, statusFactLine(render, "Manifest", report.Brain.ManifestState))
	if h := report.Hosted; h != nil {
		fmt.Fprintln(out, statusFactLine(render, "Brainstore", hostedStateLabel(h.State)))
	}
	if s := report.IssueSource; s != nil {
		fmt.Fprintln(out, statusFactLine(render, "Issues",
			fmt.Sprintf("%s records (%d stale, %d incomplete)", groupDigits(s.Records), s.Stale, s.Incomplete)))
	}

	if warnings := statusWarningLines(render, report, brainCmd, health); len(warnings) > 0 {
		fmt.Fprintln(out)
		for _, line := range warnings {
			fmt.Fprintln(out, line)
		}
	}

	fmt.Fprintln(out)
	if health.Healthy() {
		verdict := "healthy"
		if report.Integrity == "not_checked" {
			// The lightweight path never cross-checked the store, so "healthy"
			// would claim an audit nobody ran; "up to date" claims only what
			// was measured.
			verdict = "up to date"
		}
		fmt.Fprintf(out, "  %s %s\n", render.Mark(tui.MarkDone), render.PhasePaint(tui.PhaseDone, verdict))
	}
	hints := make([]string, 0, 2)
	if report.Integrity == "not_checked" {
		hints = append(hints, "Integrity not checked", "`"+brainCmd+" doctor` for a deep audit")
	}
	hints = append(hints, "`"+brainCmd+" status --verbose` for the full report")
	fmt.Fprintf(out, "  %s\n", render.Dim(strings.Join(hints, " "+render.Bullet()+" ")))
	fmt.Fprintln(out)
}

// renderStatusBlindSpotGroups prints one line per collapsed group, and (in
// verbose mode) the member paths for the groups that are NOT obvious data.
// A data-file group never enumerates: that is the whole point of the collapse,
// and `doctor` is where the full list lives.
func renderStatusBlindSpotGroups(out io.Writer, render *tui.Renderer, indent string, groups []statusBlindSpotGroup, enumerate bool, brainCmd string) {
	for _, group := range groups {
		phase := tui.PhaseSkipped
		if !group.DataFiles {
			phase = tui.PhaseFailed
		}
		line := group.Summary()
		if group.DataFiles {
			line += " " + render.Dash() + " `" + base.SetupBrainCommand(brainCmd) + " doctor` lists them"
		}
		fmt.Fprintf(out, "%s%s %s\n", indent, render.Mark(tui.MarkSkipped), render.PhasePaint(phase, line))
		if !enumerate || group.DataFiles {
			continue
		}
		for _, path := range group.Paths {
			fmt.Fprintf(out, "%s  %s\n", indent, render.Dim(path))
		}
	}
}
