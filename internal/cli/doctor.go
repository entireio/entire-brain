package cli

import (
	"errors"
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
	fmt.Fprintf(out, "ENTIRE_PLUGIN_DATA_DIR=%s\n", valueOrUnset(env.PluginDataDir))

	if env.PluginDataDir == "" {
		return errors.New("ENTIRE_PLUGIN_DATA_DIR is unset; run through `entire plugin-template` or set it for local testing")
	}
	if err := os.MkdirAll(env.PluginDataDir, 0o700); err != nil {
		return fmt.Errorf("create plugin data dir: %w", err)
	}

	f, err := os.CreateTemp(env.PluginDataDir, ".write-test-*")
	if err != nil {
		return fmt.Errorf("write plugin data dir: %w", err)
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		return fmt.Errorf("close write probe: %w", err)
	}
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("remove write probe: %w", err)
	}

	fmt.Fprintln(out, "plugin data dir: writable")
	return nil
}
