package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ashtom/entire-brain/internal/tui"
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
// even when a provider hands back a paragraph-length axis detail.
const statusCauseWidth = 140

// statusHealth is the one-line verdict the short report ends on.
type statusHealth struct {
	Issues   int
	Warnings int
	Failed   []string
	Severity string
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
}

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
	// Only one of the two: "no brain" already implies "no semantic index", and
	// a verdict that said both would be reporting one fact twice.
	health.SemanticMissing = !health.BrainMissing && report.Semantic == nil
	if report.Onboarding != nil {
		for _, component := range report.Onboarding.Components {
			if component.State == "failed" {
				health.Failed = append(health.Failed, component.Name)
			}
		}
	}
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
	return truncateString(strings.Join(parts, "; "), statusCauseWidth)
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

// Remedy is the command that FIXES what the verdict just named, not a second
// command that reports it again. `doctor` diagnoses; it rebuilds nothing, so
// sending a reader with a corrupt store there was a dead end — doctor's own
// answer for that brain is one word shorter than the question.
func (h statusHealth) Remedy() string {
	switch {
	case h.BrainMissing:
		return "setup"
	case h.SemanticMissing, h.Severity != "" && h.Severity != "ok":
		// Deterministic and token-free: the index is rebuilt without asking an
		// agent for anything, which is what a stale, missing or corrupt index
		// needs.
		return "refresh --agent none"
	default:
		return "doctor"
	}
}

// Line renders the verdict.
func (h statusHealth) Line(render *tui.Renderer, brainCmd string) string {
	brainCmd = setupBrainCommand(brainCmd)
	if h.Healthy() {
		return render.Mark(tui.MarkDone) + " " + render.PhasePaint(tui.PhaseDone, "healthy")
	}
	parts := make([]string, 0, 6)
	if h.BrainMissing {
		parts = append(parts, "no brain for this repo yet")
	}
	if h.SemanticMissing {
		parts = append(parts, "no semantic index")
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
	phase := tui.PhaseSkipped
	mark := tui.MarkSkipped
	if len(h.Failed) > 0 || h.Severity == "unsafe" {
		phase = tui.PhaseFailed
		mark = tui.MarkFailed
	}
	line := render.Mark(mark) + " " + render.PhasePaint(phase, strings.Join(parts, ", ")) +
		render.Dim(" "+render.Dash()+" run `"+brainCmd+" "+h.Remedy()+"`")
	if h.Cause != "" {
		line += "\n  " + render.Dim(h.Cause)
	}
	return line
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// statusFactsIdentity renders the fact count on the identity line. A bare
// "5 facts" is the manifest's CLAIM, and printing it unqualified is how a
// brain that had lost a fact still said 5 while recall produced 4. When the
// cross-check fails, both numbers are shown so the claim can never be mistaken
// for an observation.
func statusFactsIdentity(f *brainStatusFacts) string {
	if f.Integrity == nil || f.Integrity.OK() {
		return fmt.Sprintf("%d facts", f.Facts)
	}
	// A store that would not parse yielded no count at all, so "0 readable"
	// would be a measurement nobody took. Say what is true: it could not be
	// read.
	if f.Integrity.Mode == factStoreDefectUnreadable {
		return fmt.Sprintf("%d facts declared, store unreadable", f.Facts)
	}
	return fmt.Sprintf("%d facts declared, %d readable", f.Facts, f.Integrity.Readable)
}

// statusLabelWidth aligns the short report's left column.
const statusLabelWidth = 9

// statusBarWidth is the mini bar in the onboarding block.
const statusBarWidth = 14

// renderBrainStatusShort is the DEFAULT report: who this brain is, how far
// onboarding has got, and one line of verdict. Everything else is one flag away.
func renderBrainStatusShort(out io.Writer, render *tui.Renderer, report brainStatusReport, brainCmd string) {
	brainCmd = setupBrainCommand(brainCmd)
	fmt.Fprintf(out, "%s  %s\n",
		render.Bold(render.PhasePaint(tui.PhaseInstant, "Brain")),
		render.Bold(valueOrUnset(report.Repo.Key)))
	identity := []string{report.Brain.Path, "manifest " + report.Brain.ManifestState}
	if s := report.Semantic; s != nil && s.Coverage != nil {
		identity = append(identity, fmt.Sprintf("%d files, %d symbols", s.Coverage.Files, s.Coverage.Symbols))
	}
	if f := report.Facts; f != nil {
		identity = append(identity, statusFactsIdentity(f))
	}
	fmt.Fprintf(out, "  %s\n", render.Dim(strings.Join(identity, " "+render.Bullet()+" ")))

	if f := report.Facts; f != nil && f.Integrity != nil {
		if warning := f.Integrity.Warning(); warning != "" {
			fmt.Fprintf(out, "  %s %s\n", render.Mark(tui.MarkFailed), render.PhasePaint(tui.PhaseFailed, warning))
		}
	}

	renderStatusOnboardingBlock(out, render, report, brainCmd)

	fmt.Fprintf(out, "\n%s\n", buildStatusHealth(report).Line(render, brainCmd))
	fmt.Fprintf(out, "%s\n", render.Dim("  `"+brainCmd+" status --verbose` for the full report"))
}

// renderStatusOnboardingBlock is the part a person actually came for: is the
// backfill moving, is the watcher alive, did every source build.
func renderStatusOnboardingBlock(out io.Writer, render *tui.Renderer, report brainStatusReport, brainCmd string) {
	brainCmd = setupBrainCommand(brainCmd)
	onboarding := report.Onboarding
	if onboarding == nil {
		return
	}
	fmt.Fprintf(out, "\n%s\n", render.Bold("Onboarding"))
	facts := onboarding.Facts
	factsLine := fmt.Sprintf("  %-*s %s %s distilled",
		statusLabelWidth, "facts",
		render.Bar(facts.Distilled, facts.Sessions, statusBarWidth),
		render.Ratio(facts.Distilled, facts.Sessions))
	switch {
	case facts.Running:
		factsLine += render.PhasePaint(tui.PhaseBackfill, fmt.Sprintf("  (backfill running, pid %d)", facts.PID))
	case facts.Sessions == 0:
		factsLine += render.Dim("  (no captured sessions yet)")
	case facts.Pending() > 0:
		factsLine += render.Dim(fmt.Sprintf("  (%d pending)", facts.Pending()))
	}
	fmt.Fprintln(out, factsLine)

	daemonPhase := tui.PhaseSkipped
	daemonMark := tui.MarkSkipped
	if onboarding.Daemon.Running {
		daemonPhase, daemonMark = tui.PhaseDaemon, tui.MarkDone
	}
	daemonLine := fmt.Sprintf("  %-*s %s %s", statusLabelWidth, "daemon",
		render.Mark(daemonMark), render.PhasePaint(daemonPhase, describeDaemonState(onboarding.Daemon, brainCmd)))
	if !onboarding.LastTickAt.IsZero() {
		age := humanizeAge(report.GeneratedAt.Sub(onboarding.LastTickAt))
		if onboarding.Daemon.Running {
			daemonLine += render.Dim(" " + render.Bullet() + " last tick " + age + " ago")
		} else {
			daemonLine += render.Dim(" (last watcher tick " + age + " ago)")
		}
	}
	fmt.Fprintln(out, daemonLine)
	// Say what the machine's ONE watcher covers. A hashed launchd label answers
	// nothing a reader of `status` came to ask; the workspaces it is watching
	// answer it exactly.
	if onboarding.Daemon.Installed {
		if coverage := describeWatchPlanCoverage(onboarding.WatchPlan); coverage != "" {
			fmt.Fprintf(out, "  %-*s   %s\n", statusLabelWidth, "", render.Dim(coverage))
		}
	}

	if len(onboarding.Components) > 0 {
		parts := make([]string, 0, len(onboarding.Components))
		for _, component := range onboarding.Components {
			mark := setupComponentMark(component.State)
			phase := tui.PhaseDone
			switch component.State {
			case "failed":
				phase = tui.PhaseFailed
			case "missing":
				phase = tui.PhaseSkipped
			}
			parts = append(parts, render.Mark(mark)+" "+render.PhasePaint(phase, component.Name))
		}
		fmt.Fprintf(out, "  %-*s %s\n", statusLabelWidth, "instant", strings.Join(parts, "  "))
	}
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
			line += " " + render.Dash() + " `" + setupBrainCommand(brainCmd) + " doctor` lists them"
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
