package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/spf13/cobra"
)

// doctor's exit gate. A diagnostic that prints `error` and exits 0 cannot be
// used from CI or a script, which is the whole point of a diagnostic. The
// shape deliberately mirrors `status --fail-on`: normalize the value, emit the
// WHOLE report first, then return a rendered error so the exit code is nonzero
// and nothing is printed twice.
//
// The default differs from status's, on purpose. status's gate is opt-in
// because its gates are policy ("is this release-ready?"). doctor's are not:
// an `error` finding means a check that doctor itself performed came back
// broken, and there is no reading of that under which exit 0 is honest.
// `warn` stays opt-in (`--fail-on warn`) because warnings include ordinary
// not-built-yet states, and `--fail-on none` restores the old always-zero
// behaviour for callers that only want the report.
//
// The gate is scoped to findings about the BRAIN. See doctorScope.
const (
	doctorFailOnNone  = "none"
	doctorFailOnError = "error"
	doctorFailOnWarn  = "warn"
)

var errDoctorGate = errors.New("doctor found problems")

func doctorFailOnValues() string {
	return strings.Join([]string{doctorFailOnError, doctorFailOnWarn, doctorFailOnNone}, ", ")
}

func normalizeDoctorFailOn(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", doctorFailOnError:
		return doctorFailOnError, nil
	case doctorFailOnWarn:
		return doctorFailOnWarn, nil
	case doctorFailOnNone:
		return doctorFailOnNone, nil
	default:
		return "", fmt.Errorf("--fail-on must be one of: %s", doctorFailOnValues())
	}
}

// doctorGateFailure evaluates the gate over the findings that are claims about
// THIS BRAIN -- directory health and repo-scoped checks alike, since a caller
// gating on "is this brain healthy" does not care which section noticed.
//
// Environment-scoped findings are excluded, and that is a statement about what
// doctor's exit code means rather than a softening of the finding. The line is
// still printed, at its real severity, in both the text and JSON reports; it
// simply does not decide the exit status of a diagnostic about a different
// subsystem. The concrete case: `memory_install: error (the entire executable
// is not on PATH)` is true and worth printing, but the brain it was asked about
// is perfectly readable, so failing on it would make `doctor` unusable from CI,
// a container, or a fresh checkout -- the exact environments the gate was added
// to serve. Callers that really do want to gate on the host environment have
// `scope` and `state` in `doctor --json`.
func doctorGateFailure(report doctorReport, failOn string) error {
	if failOn == doctorFailOnNone {
		return nil
	}
	var failing []string
	errorCount, warnCount := 0, 0
	for _, finding := range append(append([]doctorCheckResult{}, report.Dirs...), report.Checks...) {
		if finding.environmental() {
			continue
		}
		switch finding.State {
		case "error":
			errorCount++
			failing = append(failing, finding.Name)
		case "warn":
			warnCount++
			if failOn == doctorFailOnWarn {
				failing = append(failing, finding.Name)
			}
		}
	}
	if len(failing) == 0 {
		return nil
	}
	summary := fmt.Sprintf("%d error(s)", errorCount)
	if failOn == doctorFailOnWarn {
		summary += fmt.Sprintf(", %d warning(s)", warnCount)
	}
	return fmt.Errorf("%w: %s: %s", errDoctorGate, summary, strings.Join(failing, ", "))
}

func newDoctorCommand(opts Options) *cobra.Command {
	var (
		jsonOut bool
		verbose bool
		failOn  string
	)
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose environment and brain pipeline problems",
		Long:  "Check the plugin environment and the capture-to-recall chain",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDoctor(cmd, opts, jsonOut, verbose, failOn)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "Print every check, including the ones that passed, and the environment doctor read")
	cmd.Flags().StringVar(&failOn, "fail-on", doctorFailOnError, "Return nonzero after printing the report when a brain finding at this severity exists: error, warn, none (host-environment findings are reported, never gated)")
	return cmd
}

// doctorReport is the machine-readable doctor contract: environment values,
// read-only plugin-directory capability hints and, when run inside a
// repository, the capture -> export -> index -> recall chain plus maintenance health.
type doctorReport struct {
	Env map[string]string `json:"env"`
	// RepoRoot is the repository the repo-scoped checks below actually ran
	// against, and RepoRootSource says how it was found. They are separate from
	// Env["ENTIRE_REPO_ROOT"], which stays a faithful report of the variable:
	// an unset variable and an inferred root are different facts and a reader
	// debugging a plugin dispatch needs both.
	RepoRoot         string                         `json:"repo_root,omitempty"`
	RepoRootSource   string                         `json:"repo_root_source,omitempty"`
	Dirs             []doctorCheckResult            `json:"dirs"`
	DirectoryHealth  []memoryInstallDirectoryHealth `json:"directory_health"`
	Checks           []doctorCheckResult            `json:"checks,omitempty"`
	Memory           map[string]any                 `json:"memory,omitempty"`
	SemanticCoverage *brainStatusSemanticCoverage   `json:"semantic_coverage,omitempty"`
}

// doctorRepoRoot resolves the repository doctor reports on.
//
// ENTIRE_REPO_ROOT wins, because the host CLI sets it when it dispatches this
// plugin and an explicit target must never be second-guessed. It is simply not
// set when the binary is run straight from a terminal -- and gating the whole
// repo-scoped section on it made `doctor` a dead end exactly when it matters:
// `setup` prints "run 'entire-brain doctor' for detail" on a failed component,
// and doctor then printed plugin-directory health and nothing about the
// failure. Inferring the root from the working directory the way every other
// repo-scoped command does closes that loop.
func doctorRepoRoot(ctx context.Context, opts Options) (root, source string) {
	if configured := strings.TrimSpace(opts.Env.RepoRoot); configured != "" {
		return configured, envRepoRoot
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", ""
	}
	if inferred, ok := gitWorkTreeRoot(ctx, opts.Runner, wd); ok {
		return inferred, "cwd"
	}
	return "", ""
}

// doctorDirectoryFinding turns read-only directory health into a finding.
//
// It reports the EVIDENCE inspectAbsoluteInstallDirectory already collected
// instead of its epistemic state name. The previous mapping called anything
// other than "present_unproven" a warning, which made every not-yet-created
// plugin directory -- the normal state of a fresh install, and of any XDG
// directory the user has not caused a write to yet -- a permanent warning that
// no action could ever clear, because doctor performs no probe write and so
// can never observe "proven". A check that can never pass teaches the reader
// to ignore all of them.
//
// What is actually checkable without a probe write is the permission mode of
// the directory (or, when it does not exist yet, of its nearest existing
// ancestor). That is a real signal in one direction: mode bits that deny
// writes mean the next refresh WILL fail, and saying so is worth a warning.
// Mode bits that permit writes are not a proof of writability -- ACLs and
// platform policy can still override them -- but "not proven" is not the same
// as "suspect", and only the JSON contract (directory_health[].state,
// .evidence, .writability_proven) needs to carry that distinction.
func doctorDirectoryFinding(label string, health memoryInstallDirectoryHealth) doctorCheckResult {
	switch health.State {
	case "present_unproven":
		if !health.ModeWriteHint {
			return doctorCheckResult{Name: label, State: "warn", Detail: fmt.Sprintf("%s: mode bits currently deny writes (mode %s); grant write access before the next refresh", health.Path, health.Mode)}
		}
		return doctorCheckResult{Name: label, State: "ok", Detail: health.Path}
	case "creatable_unproven":
		if !health.ModeWriteHint {
			return doctorCheckResult{Name: label, State: "warn", Detail: fmt.Sprintf("%s: does not exist and cannot be created; its nearest existing parent denies writes (mode %s)", health.Path, health.Mode)}
		}
		return doctorCheckResult{Name: label, State: "ok", Detail: health.Path + ": not created yet; the first write creates it"}
	default:
		detail := fmt.Sprintf("%s: %s", health.Path, health.State)
		if health.RecommendedAction != "" && health.RecommendedAction != "none" {
			detail += "; " + health.RecommendedAction
		}
		return doctorCheckResult{Name: label, State: "error", Detail: detail}
	}
}

// doctorSemanticProviderAxis is the freshness axis that describes the SEMANTIC
// PROVIDER -- the `entire graph` surface of a different product this binary
// neither installs nor repairs -- rather than any artifact in this brain.
const doctorSemanticProviderAxis = "provider"

// semanticAxisCurrent mirrors freshnessSummary's notion of an axis with nothing
// to say.
func semanticAxisCurrent(state string) bool {
	return state == "ok" || state == "clean"
}

// semanticDoctorFindings splits the semantic freshness report along the line
// doctor's exit gate already draws: what is wrong with THIS BRAIN, and what is
// wrong with the host environment around it.
//
// It was one mixed finding, and the mix was load-bearing in the wrong
// direction. Its axes include `store=unsafe (validate semantic sqlite
// integrity: file is not a database)` -- the strongest evidence this tool can
// produce that a brain is broken -- and `provider=degraded (exec: "entire":
// executable file not found)`, which says nothing about the brain and fires on
// any machine without the host CLI. Reporting the pair as a single `warn` meant
// the default `--fail-on error` gate exited 0 on a brain whose semantic store
// no longer opens. Promoting the whole check to `error` instead would fail the
// gate on every runner without `entire`, which is exactly why #239 left this
// check alone when it scoped the others.
//
// So the axes are separated rather than the severity being guessed:
//
//   - the artifact axes become a brain-scoped finding, and an `unsafe` reading
//     on one of them is an `error` -- gated, because a store that is not a
//     database is a fact about this brain and no environment can excuse it.
//   - the provider axis becomes its own environment-scoped finding, reported at
//     full severity and never gated, for the same reason memory_install is not.
//   - head, branch_tip, worktree and semantic_completeness are staleness and
//     completeness, not corruption. They stay warnings on the brain finding.
func semanticDoctorFindings(report staleReport) []doctorCheckResult {
	names := make([]string, 0, len(report.Axes))
	for name := range report.Axes {
		names = append(names, name)
	}
	sort.Strings(names)

	var brainParts, providerParts []string
	brainState := "ok"
	providerState := "ok"
	providerDetail := ""
	for _, name := range names {
		axis := report.Axes[name]
		if name == doctorSemanticProviderAxis {
			if semanticAxisCurrent(axis.State) {
				providerDetail = axis.Detail
				continue
			}
			providerState = "warn"
			providerParts = append(providerParts, semanticAxisPart(name, axis))
			continue
		}
		if semanticAxisCurrent(axis.State) {
			continue
		}
		brainParts = append(brainParts, semanticAxisPart(name, axis))
		if name == "brain_layout" && axis.State == "unsafe" {
			brainState = "error"
		} else if brainState != "error" {
			brainState = "warn"
		}
	}

	// The detail keeps the shape a reader already knows from `status
	// --verbose`: the aggregate severity, then the axes that explain it.
	brainDetail := "ok"
	if len(brainParts) > 0 {
		brainDetail = report.Severity + ": " + strings.Join(brainParts, "; ")
	}
	if providerDetail == "" {
		providerDetail = "ok"
	}
	if len(providerParts) > 0 {
		providerDetail = strings.Join(providerParts, "; ")
	}
	return []doctorCheckResult{
		{Name: "semantic", State: brainState, Detail: brainDetail},
		{Name: "semantic provider", State: providerState, Detail: providerDetail, Scope: doctorScopeEnvironment},
	}
}

func semanticAxisPart(name string, axis staleAxis) string {
	if axis.Detail != "" {
		return fmt.Sprintf("%s=%s (%s)", name, axis.State, axis.Detail)
	}
	return fmt.Sprintf("%s=%s", name, axis.State)
}

func doctorSemanticCoverage(ctx context.Context, opts Options, repoDir string, freshness staleReport) (*brainStatusSemanticCoverage, error) {
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return nil, err
	}
	manifest, err := loadGraphManifest(ctx, opts, repoDir, storage.BrainDir)
	if err != nil {
		return nil, err
	}
	if manifest.Sources == nil || manifest.Sources.Semantic == nil {
		return nil, nil
	}
	source := manifest.Sources.Semantic
	coverage, err := semanticAuditStoreCoverage(storage.BrainDir, source, freshness)
	if err != nil {
		return nil, err
	}
	return &brainStatusSemanticCoverage{
		Files: source.Files, Symbols: source.Symbols, Relations: source.Relations,
		Warnings: len(source.Warnings), PartialFailures: len(source.PartialFailures),
		FileLanguages: coverage.FileLanguages, Languages: coverage.Languages,
		SymbolKinds: coverage.SymbolKinds, RelationTypes: coverage.RelationTypes,
	}, nil
}

func runDoctor(cmd *cobra.Command, opts Options, jsonOut, verbose bool, failOn string) error {
	failOn, err := normalizeDoctorFailOn(failOn)
	if err != nil {
		return err
	}
	env := opts.Env
	report := doctorReport{Env: map[string]string{
		"ENTIRE_CLI_VERSION":       env.CLIVersion,
		"ENTIRE_REPO_ROOT":         env.RepoRoot,
		"ENTIRE_PLUGIN_CONFIG_DIR": env.PluginConfigDir,
		"ENTIRE_PLUGIN_DATA_DIR":   env.PluginDataDir,
		"ENTIRE_PLUGIN_STATE_DIR":  env.PluginStateDir,
		"ENTIRE_PLUGIN_CACHE_DIR":  env.PluginCacheDir,
	}}
	dirs, err := resolvePluginDirs(env)
	if err != nil {
		return err
	}
	for _, check := range []struct {
		label string
		path  string
	}{
		{label: "plugin config dir", path: dirs.Config},
		{label: "plugin data dir", path: dirs.Data},
		{label: "plugin state dir", path: dirs.State},
		{label: "plugin cache dir", path: dirs.Cache},
	} {
		health := inspectAbsoluteInstallDirectory(check.path)
		report.DirectoryHealth = append(report.DirectoryHealth, health)
		report.Dirs = append(report.Dirs, doctorDirectoryFinding(check.label, health))
	}
	repoRoot, repoRootSource := doctorRepoRoot(cmd.Context(), opts)
	report.RepoRoot = repoRoot
	report.RepoRootSource = repoRootSource
	switch {
	case repoRoot == "":
		// Say so, and say what to do. Silence here is what made doctor look
		// broken rather than out of scope.
		// Environment-scoped: "you ran me somewhere without a repository" is a
		// statement about the invocation, not a finding about any brain's
		// health, and it must not fail a gate on one.
		report.Checks = append(report.Checks, doctorCheckResult{
			Name:   "repo",
			State:  "warn",
			Scope:  doctorScopeEnvironment,
			Detail: "no repository in scope: " + envRepoRoot + " is unset and the working directory is not inside a git work tree; run doctor from inside the repository, or set " + envRepoRoot,
		})
	case opts.Runner == nil:
		report.Checks = append(report.Checks, doctorCheckResult{
			Name:   "repo",
			State:  "warn",
			Scope:  doctorScopeEnvironment,
			Detail: "no command runner: repo-scoped checks skipped for " + repoRoot,
		})
	}
	if repoRoot != "" && opts.Runner != nil {
		// The repo-scoped helpers below read opts.Env for storage identity, so
		// the inferred root has to travel in the copy they get -- otherwise an
		// inferred run would report on the repo it found but resolve storage as
		// if no repo were selected at all.
		scoped := opts
		scoped.Env.RepoRoot = repoRoot
		opts = scoped
		env = scoped.Env
		sampledNow := opts.Now()
		opts.Now = func() time.Time { return sampledNow }
		report.Checks, report.Memory = brainDoctorReadOnlyReport(cmd.Context(), opts, repoRoot)
		// Whatever the last `setup` could not build. setup keeps going on a
		// component failure and points here for the reason, so the reason has to
		// actually be here.
		if storage, serr := repoStoragePaths(cmd.Context(), opts.Runner, env, repoRoot); serr == nil {
			is, ierr := issueStore(storage.BrainDir).Status(opts.Now())
			if ierr != nil {
				report.Checks = append(report.Checks, doctorCheckResult{Name: "issue source", State: "error", Detail: ierr.Error()})
			} else if is.Binding.Workspace != "" {
				state := "ok"
				if is.Stale > 0 || is.Incomplete > 0 || is.PendingRuns > 0 || is.LimitedRuns > 0 {
					state = "warn"
				}
				report.Checks = append(report.Checks, doctorCheckResult{Name: "issue source", State: state, Detail: fmt.Sprintf("%d records, %d stale, %d incomplete; %d pending and %d limited runs; inspect issues status for import coverage", is.Records, is.Stale, is.Incomplete, is.PendingRuns, is.LimitedRuns)})
			}
			for _, component := range failedSetupComponents(filepath.Dir(storage.HeadPath), storage.BrainDir) {
				detail := component.Detail
				if hint := component.Hint; hint != "" {
					detail += "; " + hint
				}
				report.Checks = append(report.Checks, doctorCheckResult{
					Name:   "setup " + setupComponentLabel(component.Name),
					State:  "error",
					Detail: detail,
				})
			}
		}
		// The full blind-spot list lives HERE. `status` collapses forty-four
		// per-file warnings into one line and tells the reader to run doctor for
		// the detail, so the detail has to actually exist somewhere — and doctor
		// is the right somewhere: it is the command you run when you have decided
		// to care.
		if spots, spotErr := brainBlindSpotsForRepo(cmd.Context(), opts, repoRoot); spotErr == nil {
			for _, group := range groupStatusBlindSpots(spots) {
				report.Checks = append(report.Checks, doctorCheckResult{
					Name:   "semantic blind spots",
					State:  "warn",
					Detail: group.Summary() + ": " + strings.Join(group.Paths, ", "),
				})
			}
		}
		if semReport, semErr := semanticStaleReport(cmd.Context(), opts, repoRoot); semErr != nil {
			report.Checks = append(report.Checks, doctorCheckResult{Name: "semantic", State: "warn", Detail: "unavailable: " + semErr.Error()})
		} else {
			report.Checks = append(report.Checks, semanticDoctorFindings(semReport)...)
			if coverage, err := doctorSemanticCoverage(cmd.Context(), opts, repoRoot, semReport); err != nil {
				report.Checks = append(report.Checks, doctorCheckResult{Name: "semantic coverage", State: "warn", Detail: err.Error()})
			} else {
				report.SemanticCoverage = coverage
			}
		}
	}
	if jsonOut {
		if err := writeJSON(cmd, report); err != nil {
			return err
		}
		return doctorGateResult(report, failOn)
	}
	out := cmd.OutOrStdout()
	if verbose {
		renderDoctorVerbose(out, report)
	} else {
		renderDoctorActionable(out, report)
	}
	return doctorGateResult(report, failOn)
}

// doctorVerboseInvocation is the one command that turns the short report back
// into the long one. It is named on every short report, so the detail a reader
// does not see is always one documented command away.
// A func, not a const: the spelling is only knowable at call time.
func doctorVerboseInvocation() string {
	return base.BrainInvocation(os.LookupEnv, "doctor --verbose")
}

// doctorActionableDetailBudget caps one short line's detail. The blind-spot
// groups carry every skipped path -- 3.5KB on this repository alone -- which is
// a list to read at leisure under --verbose, not a wall to scroll past when you
// are looking for what to fix.
const doctorActionableDetailBudget = 160

// renderDoctorVerbose is the whole report: the environment doctor read, the
// resolved repository, and every finding at full detail, passing or not.
func renderDoctorVerbose(out io.Writer, report doctorReport) {
	for _, key := range []string{"ENTIRE_CLI_VERSION", "ENTIRE_REPO_ROOT", "ENTIRE_PLUGIN_CONFIG_DIR", "ENTIRE_PLUGIN_DATA_DIR", "ENTIRE_PLUGIN_STATE_DIR", "ENTIRE_PLUGIN_CACHE_DIR"} {
		fmt.Fprintf(out, "%s=%s\n", key, valueOrUnset(report.Env[key]))
	}
	if report.RepoRoot != "" && report.RepoRootSource != envRepoRoot {
		fmt.Fprintf(out, "repo root: %s (inferred from %s)\n", report.RepoRoot, report.RepoRootSource)
	}
	for _, dir := range report.Dirs {
		fmt.Fprintf(out, "%s: %s (%s)\n", dir.Name, dir.State, dir.Detail)
	}
	for _, check := range report.Checks {
		fmt.Fprintf(out, "%s: %s", check.Name, check.State)
		if check.Detail != "" {
			fmt.Fprintf(out, " (%s)", check.Detail)
		}
		fmt.Fprintln(out)
	}
	// Coverage is counts, never a finding: it names nothing to fix, so it
	// belongs with the rest of the passing detail under --verbose. The one
	// coverage condition a reader can act on -- the audit failing -- is a
	// "semantic coverage" warn check above, and the short report prints it.
	if c := report.SemanticCoverage; c != nil {
		fmt.Fprintf(out, "semantic coverage: %d files, %d symbols, %d relations (manifest counts)\n", c.Files, c.Symbols, c.Relations)
		for _, section := range []struct {
			name   string
			counts []semanticAuditCount
		}{
			{"file languages", c.FileLanguages}, {"symbol languages", c.Languages},
			{"symbol kinds", c.SymbolKinds}, {"relation types", c.RelationTypes},
		} {
			if len(section.counts) > 0 {
				fmt.Fprintf(out, "  %s: %s\n", section.name, semanticAuditCountSummary(section.counts))
			}
		}
	}
}

// renderDoctorActionable is the default report, and it prints exactly the
// findings a reader can do something about.
//
// The old default printed six environment lines, four passing directory lines
// and every passing check -- forty lines and 6.4KB on a HEALTHY brain, of which
// six said anything and exactly one named a remedy. A diagnostic that cries
// wolf on a healthy system teaches people to stop reading it, and then the one
// line that mattered is lost in the thirty-four that did not.
//
// So: nothing that passed, and every line that did not ends with a command.
// The counts stay, because "I printed nothing" and "I checked nothing" must not
// look the same, and the summary names the flag that restores the long form.
//
// Non-ok findings are NEVER dropped or capped. A brain with twelve problems
// prints twelve lines; the saving comes from silence about what is fine, not
// from hiding what is broken.
func renderDoctorActionable(out io.Writer, report doctorReport) {
	okCount, warnCount, errorCount := 0, 0, 0
	var lines []doctorActionableLine
	for i, dir := range report.Dirs {
		switch dir.State {
		case "ok":
			okCount++
			continue
		case "warn":
			warnCount++
		default:
			errorCount++
		}
		// Dirs and DirectoryHealth are appended in lockstep, so the health that
		// produced this finding -- and the path to chmod -- is at the same index.
		var health memoryInstallDirectoryHealth
		if i < len(report.DirectoryHealth) {
			health = report.DirectoryHealth[i]
		}
		lines = append(lines, doctorActionableLine{finding: dir, remedy: doctorDirectoryRemedy(health)})
	}
	for _, check := range report.Checks {
		switch check.State {
		case "ok":
			okCount++
			continue
		case "warn":
			warnCount++
		default:
			errorCount++
		}
		lines = append(lines, doctorActionableLine{finding: check, remedy: doctorRemedy(check)})
	}
	if len(lines) == 0 {
		fmt.Fprintf(out, "healthy: %d check(s) passed, nothing to fix; run `%s` for the full report\n", okCount, doctorVerboseInvocation())
		return
	}
	for _, line := range lines {
		fmt.Fprintf(out, "%s: %s (%s)\n", line.finding.Name, line.finding.State, line.detail())
	}
	fmt.Fprintf(out, "%d ok, %d warn, %d error; run `%s` for the full report\n", okCount, warnCount, errorCount, doctorVerboseInvocation())
}

// doctorActionableLine is one short-report line: the finding, and the command
// that addresses it when the finding's own detail does not already name one.
type doctorActionableLine struct {
	finding doctorCheckResult
	remedy  string
}

func (l doctorActionableLine) detail() string {
	detail := truncateDoctorDetail(l.finding.Detail)
	// Whether the detail names its own command is decided on the text that is
	// actually PRINTED. Deciding it on the untruncated detail meant a line
	// whose command had been cut in half still suppressed the remedy, and the
	// reader got "...; run `entire brain ref…" and nothing else.
	// Truncation never cuts inside a span, so a backtick here means a whole one.
	if strings.Contains(detail, "`") {
		return detail
	}
	if l.remedy == "" {
		return detail
	}
	if detail == "" {
		return "run `" + l.remedy + "`"
	}
	return strings.TrimRight(detail, "; ") + "; run `" + l.remedy + "`"
}

func truncateDoctorDetail(detail string) string {
	runes := []rune(detail)
	if len(runes) <= doctorActionableDetailBudget {
		return detail
	}
	cut := doctorActionableDetailBudget
	// Never cut inside a `command` span. Half a command is worse than none: it
	// cannot be run, and it still looks like the line answered the question.
	// Backticks are balanced, so an odd count before the cut means it lands
	// inside one; back up to that span's opening backtick and drop it whole.
	if strings.Count(string(runes[:cut]), "`")%2 == 1 {
		for cut > 0 && runes[cut-1] != '`' {
			cut--
		}
		cut--
	}
	if cut < 0 {
		cut = 0
	}
	kept := strings.TrimRight(string(runes[:cut]), " ,;`")
	// Dropping the command can leave the word that introduced it, so the line
	// read "...; run\u2026; run `entire brain refresh`". Trim the dangling lead-in.
	for _, lead := range []string{"; run", " run", "; try", " try"} {
		kept = strings.TrimSuffix(kept, lead)
	}
	return strings.TrimRight(kept, " ,;") + "\u2026"
}

// doctorDirectoryRemedy: every non-ok plugin-directory finding is a permission
// or path problem on a named directory, and the mode bits are the part doctor
// can actually see.
func doctorDirectoryRemedy(health memoryInstallDirectoryHealth) string {
	// The remedy is rendered as "run `<remedy>`", so it must be something a
	// reader can actually run.
	//
	// This used to return health.RecommendedAction whenever it was set, which
	// a previous finding asked for. Every constructor seeds that field and then
	// overwrites it on each path, so it is NEVER empty for a real doctor run:
	// the branches below were unreachable outside hand-built fixtures, and the
	// line rendered as
	//
	//   run `grant write access before running a mutation; mode bits currently deny writes`
	//
	// presenting a sentence as a command, which is the defect this whole
	// report set out to remove.
	//
	// Nothing is lost by not repeating it here: doctorDirectoryFinding already
	// puts that guidance in the Detail this remedy is appended to, either in
	// its own words for the two warn states or verbatim for the error state.
	path := strings.TrimSpace(health.Path)
	if path == "" {
		return doctorVerboseInvocation()
	}
	if strings.Contains(health.State, "unsafe") {
		// A symlink, a non-directory, or a relative path. No mode change fixes
		// any of them, and what the reader needs first is to see what is there.
		return "ls -ld " + shellQuote(path)
	}
	if !health.Exists {
		// The detail names the nearest EXISTING parent as the thing denying
		// writes, so that is what the reader has to change. filepath.Dir climbs
		// exactly one level, which was wrong whenever more than one component
		// was missing -- the default data dir is three levels deep, so the
		// suggested chmod named a directory that did not exist either.
		return "chmod u+w " + shellQuote(nearestExistingAncestor(path))
	}
	return "chmod u+w " + shellQuote(path)
}

// shellQuote makes a path safe to paste into a shell.
//
// Raised as a shell-injection finding. As stated the threat model is weak: the
// path comes from the reader's OWN ENTIRE_PLUGIN_* variables or home directory,
// and anyone who can set those can already run code. The quoting is right for a
// duller reason that bites far more often — a path containing a SPACE produces a
// command that silently chmods the wrong thing, and "/Users/me/My Brain" is an
// ordinary path on a Mac.
//
// Single quotes, because inside them a shell expands nothing at all; an embedded
// single quote is closed, escaped and reopened.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// doctorRemedies maps a finding to the command that addresses it, for the
// findings whose detail does not already quote one.
//
// This is a render-time table on purpose: doctorCheckResult is the --json
// contract and gains no field, so adding a remedy to the human report cannot
// change a byte of the machine one.
//
// TestDoctorRemediesAreRealCommands resolves every `entire brain ...` value
// here against the live command tree, so a renamed verb fails the build rather
// than sending a user to a command that no longer exists.
const (
	// doctorSemanticProviderFinding is the one finding whose remedy is chosen
	// by condition rather than by name; see doctorSemanticProviderRemedy.
	doctorSemanticProviderFinding = "semantic provider"
	// doctorGraphInstallRemedy and doctorBrainUpgradeRemedy are the two sides
	// of that pair, as the README documents installing them. Neither is an
	// `entire brain ...` verb, so neither is resolvable against the command
	// tree: entire-brain installs and upgrades neither itself nor Graph.
	doctorGraphInstallRemedy = "entire plugin install graph"
	doctorBrainUpgradeRemedy = "curl -fsSL https://raw.githubusercontent.com/entireio/entire-brain/main/scripts/get-brain.sh | bash"
)

// doctorRemedies holds VERBS, not commands. The reader's dispatch spelling
// -- `entire brain` or `entire-brain` -- is applied at lookup, because a
// remedy printed in the other spelling is one the reader cannot paste.
var doctorRemedies = map[string]string{
	"brain":                  "setup",
	"manifest":               "refresh",
	"capture":                "refresh sessions",
	"docs_index":             "refresh",
	"facts":                  "refresh",
	"history_index":          "refresh history",
	"history_integrity":      "refresh history",
	"history_freshness":      "refresh",
	"short_term_memory":      "refresh delta",
	"conversation":           "refresh",
	"history_fts":            "refresh history",
	"semantic":               "refresh index",
	"semantic blind spots":   "refresh index",
	"memory_coordinator":     "memory status",
	"write_lock":             "memory status",
	"memory_provider_egress": "config show",
	"memory_schemas":         "memory migrate",
	"memory_overlay":         "memory rebuild",
	"memory_fts":             "memory rebuild",
	"memory_jobs":            "memory jobs",
	"memory_vectors":         "memory rebuild",
	"memory_migrations":      "memory migrate --dry-run",
	"memory_worker_log":      "memory status",
	"memory_sessions":        "refresh sessions",
	"memory_reconciliation":  "memory reconcile",
	// Host-environment findings. entire-brain neither installs nor repairs the
	// Entire CLI or the Graph provider, so the command is the one the README
	// gives for installing them -- not an entire-brain verb that would quietly
	// do nothing.
	"memory_install": "curl -fsSL https://entire.io/install.sh | bash",
	// "semantic provider" is deliberately ABSENT: its remedy depends on which
	// side of the pair is behind, which a name-keyed table cannot express.
	// See doctorSemanticProviderRemedy.
	// Quoted: unquoted, `<your` is an input redirection and the line fails with
	// a redirection error instead of saying anything. A placeholder still has
	// to survive being pasted.
	"repo": "cd '<your repository>' && entire brain doctor",
}

func doctorRemedy(check doctorCheckResult) string {
	// The "detail already names a command" case is NOT decided here: it depends
	// on the truncated text, which only doctorActionableLine.detail sees.
	// `setup <component>` findings are one per failed setup component.
	if strings.HasPrefix(check.Name, "setup ") {
		return base.BrainInvocation(os.LookupEnv, "setup")
	}
	if check.Name == doctorSemanticProviderFinding {
		return doctorSemanticProviderRemedy(check.Detail)
	}
	if verb, ok := doctorRemedies[check.Name]; ok {
		return base.BrainInvocation(os.LookupEnv, verb)
	}
	// A check added later without a table entry still gets a command, because
	// "no remedy" is the one answer a diagnostic must never give.
	return doctorVerboseInvocation()
}

// doctorSemanticProviderRemedy picks the remedy for the ONE finding whose fix
// depends on the condition rather than on the finding's name.
//
// "semantic provider" reports a pair that has to agree: this build of
// entire-brain and the entire-graph provider it reads. Either side can be the
// one that is behind, and the table above is keyed by finding NAME, so it
// could only ever name one of them. It named Graph. That is right when the
// provider is absent and exactly backwards on a version skew, where the
// provider is the side that is AHEAD:
//
//	semantic provider: warn (provider=degraded (semantic provider emitted
//	schema_version "1.3" but this build reads 1.1; ...; but upgrade
//	entire-b...; run `entire plugin install graph`)
//
// The message says upgrade Brain and the remedy says install Graph. Installing
// Graph cannot help, and a newer Graph widens the skew it was offered for.
//
// Like every other classification of a provider failure here, this one reads
// the FULL detail, never the truncated line: truncation cuts the skew message
// mid-word ("upgrade entire-b..."), so a remedy chosen from what is printed
// would flip back to the wrong side exactly when the line is long enough to
// need the help.
func doctorSemanticProviderRemedy(detail string) string {
	if doctorSemanticProviderAhead(detail) {
		return doctorBrainUpgradeRemedy
	}
	return doctorGraphInstallRemedy
}

// doctorSemanticProviderAhead reports whether the detail describes a provider
// that is NEWER than this build, i.e. the skew where entire-brain is the side
// to upgrade. It matches the remedy the message itself states, so the two can
// only disagree if someone rewords the message without rewording this.
func doctorSemanticProviderAhead(detail string) bool {
	text := strings.ToLower(detail)
	return strings.Contains(text, "upgrade entire-brain")
}

// doctorGateResult wraps a tripped gate the way `status --fail-on` does: the
// report is already on stdout and names every failing check, so the error is
// marked rendered and the process exits nonzero without printing a second,
// less informative copy of the same news to stderr.
func doctorGateResult(report doctorReport, failOn string) error {
	if err := doctorGateFailure(report, failOn); err != nil {
		return renderedCommandError{err: err}
	}
	return nil
}

// nearestExistingAncestor walks up until it finds a directory that exists.
// inspectAbsoluteInstallDirectory's creatable_unproven state can sit under
// arbitrarily many missing components, and a chmod against one of those fails
// with "No such file or directory" rather than fixing anything.
//
// Returns the last path examined if it runs out of ancestors, so the caller
// always has something to name.
func nearestExistingAncestor(path string) string {
	dir := filepath.Dir(path)
	for {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return dir
		}
		dir = parent
	}
}
