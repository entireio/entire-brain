package cli

import (
	"fmt"
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
	Env             map[string]string              `json:"env"`
	Dirs            []doctorCheckResult            `json:"dirs"`
	DirectoryHealth []memoryInstallDirectoryHealth `json:"directory_health"`
	Checks          []doctorCheckResult            `json:"checks,omitempty"`
	Memory          map[string]any                 `json:"memory,omitempty"`
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
	if env.RepoRoot != "" && opts.Runner != nil {
		report.Checks, report.Memory = brainDoctorReadOnlyReport(cmd.Context(), opts, env.RepoRoot)
		// Whatever the last `setup` could not build. setup keeps going on a
		// component failure and points here for the reason, so the reason has to
		// actually be here.
		if storage, serr := repoStoragePaths(cmd.Context(), opts.Runner, env, env.RepoRoot); serr == nil {
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
		if spots, spotErr := brainBlindSpotsForRepo(cmd.Context(), opts, env.RepoRoot); spotErr == nil {
			for _, group := range groupStatusBlindSpots(spots) {
				report.Checks = append(report.Checks, doctorCheckResult{
					Name:   "semantic blind spots",
					State:  "warn",
					Detail: group.Summary() + ": " + strings.Join(group.Paths, ", "),
				})
			}
		}
		if semReport, semErr := semanticStaleReport(cmd.Context(), opts, env.RepoRoot); semErr != nil {
			report.Checks = append(report.Checks, doctorCheckResult{Name: "semantic", State: "warn", Detail: "unavailable: " + semErr.Error()})
		} else {
			state := "ok"
			if semReport.Severity != "ok" {
				state = "warn"
			}
			report.Checks = append(report.Checks, doctorCheckResult{Name: "semantic", State: state, Detail: semReport.Severity})
		}
	}
	if jsonOut {
		return writeJSON(cmd, report)
	}
	out := cmd.OutOrStdout()
	for _, key := range []string{"ENTIRE_CLI_VERSION", "ENTIRE_REPO_ROOT", "ENTIRE_PLUGIN_CONFIG_DIR", "ENTIRE_PLUGIN_DATA_DIR", "ENTIRE_PLUGIN_STATE_DIR", "ENTIRE_PLUGIN_CACHE_DIR"} {
		fmt.Fprintf(out, "%s=%s\n", key, valueOrUnset(report.Env[key]))
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
