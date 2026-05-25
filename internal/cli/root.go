package cli

import (
	"fmt"
	"time"

	"github.com/ashtom/entire-brain/internal/config"
	"github.com/spf13/cobra"
)

type Options struct {
	Version string
	Env     EntireEnv
	Runner  CommandRunner
	Now     func() time.Time
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
	if opts.Runner == nil {
		opts.Runner = ExecRunner{}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

	cmd := &cobra.Command{
		Use:           "entire-brain",
		Short:         "Export Entire session history for agent review",
		SilenceUsage:  true,
		SilenceErrors: true,
		Long: `entire-brain is an external-command plugin for the Entire CLI.

It exports checkpointed session transcripts and metadata into a directory an
agent can inspect to understand project history.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStatus(cmd, opts)
		},
	}

	cmd.AddCommand(newDoctorCommand(opts.Env))
	cmd.AddCommand(newConfigCommand(opts.Env))
	cmd.AddCommand(newExportCommand(opts))
	cmd.AddCommand(newVersionCommand(opts.Version))
	return cmd
}

func runStatus(cmd *cobra.Command, opts Options) error {
	out := cmd.OutOrStdout()
	fmt.Fprintln(out, "entire-brain")
	fmt.Fprintf(out, "version: %s\n", opts.Version)
	fmt.Fprintf(out, "entire cli: %s\n", valueOrUnset(opts.Env.CLIVersion))
	fmt.Fprintf(out, "repo root: %s\n", valueOrUnset(opts.Env.RepoRoot))
	dirs, err := resolvePluginDirs(opts.Env)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "plugin config: %s\n", dirs.Config)
	fmt.Fprintf(out, "plugin data: %s\n", dirs.Data)
	fmt.Fprintf(out, "plugin state: %s\n", dirs.State)
	fmt.Fprintf(out, "plugin cache: %s\n", dirs.Cache)

	cfg, err := config.Load(dirs.Config)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "greeting: %s\n", cfg.Greeting)
	return nil
}
