package cli

import (
	"fmt"

	"github.com/entireio/entire-plugin-template/internal/config"
	"github.com/spf13/cobra"
)

type Options struct {
	Version string
	Env     EntireEnv
}

// Execute runs the plugin root command with the real process environment.
func Execute(version string) error {
	return NewRootCommand(Options{
		Version: version,
		Env:     EnvFromOS(),
	}).Execute()
}

func NewRootCommand(opts Options) *cobra.Command {
	if opts.Version == "" {
		opts.Version = "dev"
	}

	cmd := &cobra.Command{
		Use:           "entire-plugin-template",
		Short:         "Template external command plugin for the Entire CLI",
		SilenceUsage:  true,
		SilenceErrors: true,
		Long: `entire-plugin-template is a minimal, testable external-command
plugin for the Entire CLI.

It demonstrates the binary naming convention, parent-provided environment, and
per-plugin durable data directory used by Entire external commands.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStatus(cmd, opts)
		},
	}

	cmd.AddCommand(newDoctorCommand(opts.Env))
	cmd.AddCommand(newConfigCommand(opts.Env))
	cmd.AddCommand(newVersionCommand(opts.Version))
	return cmd
}

func runStatus(cmd *cobra.Command, opts Options) error {
	out := cmd.OutOrStdout()
	fmt.Fprintln(out, "entire-plugin-template")
	fmt.Fprintf(out, "version: %s\n", opts.Version)
	fmt.Fprintf(out, "entire cli: %s\n", valueOrUnset(opts.Env.CLIVersion))
	fmt.Fprintf(out, "repo root: %s\n", valueOrUnset(opts.Env.RepoRoot))
	fmt.Fprintf(out, "plugin data: %s\n", valueOrUnset(opts.Env.PluginDataDir))

	if opts.Env.PluginDataDir == "" {
		fmt.Fprintln(out, "greeting: <unavailable until ENTIRE_PLUGIN_DATA_DIR is set>")
		return nil
	}

	cfg, err := config.Load(opts.Env.PluginDataDir)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "greeting: %s\n", cfg.Greeting)
	return nil
}
