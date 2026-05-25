package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func newDoctorCommand(env EntireEnv) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check the parent Entire CLI plugin environment",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDoctor(cmd, env)
		},
	}
}

func runDoctor(cmd *cobra.Command, env EntireEnv) error {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "ENTIRE_CLI_VERSION=%s\n", valueOrUnset(env.CLIVersion))
	fmt.Fprintf(out, "ENTIRE_REPO_ROOT=%s\n", valueOrUnset(env.RepoRoot))
	fmt.Fprintf(out, "ENTIRE_PLUGIN_CONFIG_DIR=%s\n", valueOrUnset(env.PluginConfigDir))
	fmt.Fprintf(out, "ENTIRE_PLUGIN_DATA_DIR=%s\n", valueOrUnset(env.PluginDataDir))
	fmt.Fprintf(out, "ENTIRE_PLUGIN_STATE_DIR=%s\n", valueOrUnset(env.PluginStateDir))
	fmt.Fprintf(out, "ENTIRE_PLUGIN_CACHE_DIR=%s\n", valueOrUnset(env.PluginCacheDir))

	dirs, err := resolvePluginDirs(env)
	if err != nil {
		return err
	}
	checks := []struct {
		label string
		path  string
	}{
		{label: "plugin config dir", path: dirs.Config},
		{label: "plugin data dir", path: dirs.Data},
		{label: "plugin state dir", path: dirs.State},
		{label: "plugin cache dir", path: dirs.Cache},
	}
	for _, check := range checks {
		if err := probeWritableDir(check.path); err != nil {
			return fmt.Errorf("%s: %w", check.label, err)
		}
		fmt.Fprintf(out, "%s: writable (%s)\n", check.label, check.path)
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
