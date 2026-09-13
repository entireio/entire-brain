package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

func newDoctorCommand(opts Options) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check the plugin environment and the capture-to-recall chain",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDoctor(cmd, opts, jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
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

func runDoctor(cmd *cobra.Command, opts Options, jsonOut bool) error {
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
		state := "ok"
		if health.State != "present_unproven" {
			state = "warn"
		}
		if health.State == "unsafe" || health.State == "unavailable" {
			state = "error"
		}
		detail := fmt.Sprintf("%s: %s", health.State, health.Path)
		if health.RecommendedAction != "" && health.RecommendedAction != "none" {
			detail += "; " + health.RecommendedAction
		}
		report.Dirs = append(report.Dirs, doctorCheckResult{Name: check.label, State: state, Detail: detail})
	}
	repoRoot, repoRootSource := doctorRepoRoot(cmd.Context(), opts)
	report.RepoRoot = repoRoot
	report.RepoRootSource = repoRootSource
	switch {
	case repoRoot == "":
		// Say so, and say what to do. Silence here is what made doctor look
		// broken rather than out of scope.
		report.Checks = append(report.Checks, doctorCheckResult{
			Name:   "repo",
			State:  "warn",
			Detail: "no repository in scope: " + envRepoRoot + " is unset and the working directory is not inside a git work tree; run doctor from inside the repository, or set " + envRepoRoot,
		})
	case opts.Runner == nil:
		report.Checks = append(report.Checks, doctorCheckResult{
			Name:   "repo",
			State:  "warn",
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
		report.Checks, report.Memory = brainDoctorReadOnlyReport(cmd.Context(), opts, repoRoot)
		// Whatever the last `setup` could not build. setup keeps going on a
		// component failure and points here for the reason, so the reason has to
		// actually be here.
		if storage, serr := repoStoragePaths(cmd.Context(), opts.Runner, env, repoRoot); serr == nil {
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
			state := "ok"
			detail := semReport.Severity
			if semReport.Severity != "ok" {
				state = "warn"
				// A bare severity was one word, and a strictly SMALLER one word
				// than `status --verbose` already prints: a reader sent here to
				// learn why the brain is unsafe learned nothing. freshnessSummary
				// is the same renderer `overview` uses, so a shredded store now
				// reads as "store=unsafe (validate semantic sqlite integrity: …)".
				if summary := freshnessSummary(semReport); summary != "" {
					detail += ": " + summary
				}
			}
			report.Checks = append(report.Checks, doctorCheckResult{Name: "semantic", State: state, Detail: detail})
		}
	}
	if jsonOut {
		return writeJSON(cmd, report)
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
	return nil
}
