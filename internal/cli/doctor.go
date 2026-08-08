package cli

import (
	"fmt"
	"os"

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
// plugin-dir writability, and; when run inside a repository; the
// capture -> export -> index -> recall chain checks (brainDoctorChecks).
type doctorReport struct {
	Env    map[string]string   `json:"env"`
	Dirs   []doctorCheckResult `json:"dirs"`
	Checks []doctorCheckResult `json:"checks,omitempty"`
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
		result := doctorCheckResult{Name: check.label, State: "ok", Detail: check.path}
		if probeErr := probeWritableDir(check.path); probeErr != nil {
			result.State = "error"
			result.Detail = probeErr.Error()
		}
		report.Dirs = append(report.Dirs, result)
	}
	if env.RepoRoot != "" && opts.Runner != nil {
		report.Checks = brainDoctorChecks(cmd.Context(), opts, env.RepoRoot)
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
		if dir.State != "ok" {
			return fmt.Errorf("%s: %s", dir.Name, dir.Detail)
		}
		fmt.Fprintf(out, "%s: writable (%s)\n", dir.Name, dir.Detail)
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

func probeWritableDir(dir string) error {
	if err := ensureDir(dir); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}
	f, err := os.CreateTemp(dir, ".write-test-*")
	if err != nil {
		return fmt.Errorf("write probe: %w", err)
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		return fmt.Errorf("close write probe: %w", err)
	}
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("remove write probe: %w", err)
	}
	return nil
}
