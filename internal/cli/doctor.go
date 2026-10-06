package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

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
		failOn  string
	)
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose environment and brain pipeline problems",
		Long:  "Check the plugin environment and the capture-to-recall chain",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDoctor(cmd, opts, jsonOut, failOn)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
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
	RepoRoot        string                         `json:"repo_root,omitempty"`
	RepoRootSource  string                         `json:"repo_root_source,omitempty"`
	Dirs            []doctorCheckResult            `json:"dirs"`
	DirectoryHealth []memoryInstallDirectoryHealth `json:"directory_health"`
	Checks          []doctorCheckResult            `json:"checks,omitempty"`
	Memory          map[string]any                 `json:"memory,omitempty"`
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

// doctorSemanticArtifactAxes are the freshness axes that describe persisted
// semantic artifacts this brain CLAIMS to have: the provider snapshot on disk
// and the derived SQLite store. An `unsafe` reading on one of these is not a
// staleness observation and not a host-environment observation -- it is this
// brain's own recorded data failing to load.
//
// The `semantic` axis is deliberately NOT here. Its only state is "missing"
// ("no semantic source in manifest"), which is the ordinary condition of a
// brain that has not run `refresh index` yet. Promoting never-built to `error`
// would fail the gate on every fresh brain, which is the same mistake in the
// opposite direction.
var doctorSemanticArtifactAxes = map[string]bool{
	"snapshot": true,
	"store":    true,
}

// semanticAxisBroken reports an axis state that means "what the manifest points
// at could not be read", as opposed to "this is behind" (stale, dirty-*),
// "this is incomplete" (degraded), or "this was never built" (missing).
func semanticAxisBroken(state string) bool {
	return state == "unsafe"
}

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
		if doctorSemanticArtifactAxes[name] && semanticAxisBroken(axis.State) {
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

func runDoctor(cmd *cobra.Command, opts Options, jsonOut bool, failOn string) error {
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
			for _, component := range failedSetupComponents(filepath.Dir(storage.HeadPath)) {
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
		}
	}
	if jsonOut {
		if err := writeJSON(cmd, report); err != nil {
			return err
		}
		return doctorGateResult(report, failOn)
	}
	out := cmd.OutOrStdout()
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
	return doctorGateResult(report, failOn)
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
