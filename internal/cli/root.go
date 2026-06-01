package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"regexp"
	"strings"
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
	cmd := NewRootCommand(Options{
		Version: version,
		Env:     EnvFromOS(),
	})
	err := cmd.Execute()
	if err != nil && commandErrorWasRendered(err) {
		return err
	}
	return err
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

	cmd.AddCommand(newDoctorCommand(opts))
	cmd.AddCommand(newAgentStatusCommand(opts))
	cmd.AddCommand(newBrainBriefCommand(opts))
	cmd.AddCommand(newConfigCommand(opts.Env))
	cmd.AddCommand(newExportCommand(opts))
	cmd.AddCommand(newBrainGuideCommand())
	cmd.AddCommand(newBrainInspectCommand(opts))
	cmd.AddCommand(newMCPCommand(opts))
	cmd.AddCommand(newSemanticBundleCommand(opts))
	cmd.AddCommand(newSemanticChangesCommand(opts))
	cmd.AddCommand(newSemanticContextCommand(opts))
	cmd.AddCommand(newSemanticGCCommand(opts))
	cmd.AddCommand(newSemanticImpactCommand(opts))
	cmd.AddCommand(newSemanticIndexCommand(opts))
	cmd.AddCommand(newSemanticQueryCommand(opts))
	cmd.AddCommand(newSemanticRoutesCommand(opts))
	cmd.AddCommand(newPathCommand(opts))
	cmd.AddCommand(newRefreshCommand(opts))
	cmd.AddCommand(newSemanticRepairCommand(opts))
	cmd.AddCommand(newSemanticResetCommand(opts))
	cmd.AddCommand(newBrainSearchCommand(opts))
	cmd.AddCommand(newSeedCommand(opts))
	cmd.AddCommand(newBrainShowCommand(opts))
	cmd.AddCommand(newSemanticStaleCommand(opts))
	cmd.AddCommand(newSemanticTestsCommand(opts))
	cmd.AddCommand(newSemanticToolsCommand(opts))
	cmd.AddCommand(newSemanticWorkflowsCommand(opts))
	cmd.AddCommand(newWorkspaceCommand(opts))
	cmd.AddCommand(newVersionCommand(opts.Version))
	wrapJSONErrorRendering(cmd)
	return cmd
}

type commandJSONError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type renderedCommandError struct {
	err error
}

var errRenderedCommand = renderedCommandError{}

func RenderedError() error {
	return errRenderedCommand
}

func (e renderedCommandError) Error() string {
	if e.err == nil {
		return "command error rendered"
	}
	return e.err.Error()
}

func (e renderedCommandError) Unwrap() error {
	return e.err
}

func (e renderedCommandError) Is(target error) bool {
	_, ok := target.(renderedCommandError)
	return ok
}

func commandErrorWasRendered(err error) bool {
	_, ok := err.(renderedCommandError)
	return ok
}

func wrapJSONErrorRendering(cmd *cobra.Command) {
	if cmd.Args != nil {
		argsFunc := cmd.Args
		cmd.Args = func(cmd *cobra.Command, args []string) error {
			err := argsFunc(cmd, args)
			if err == nil || !commandWantsJSONError(cmd) {
				return err
			}
			_ = writeCommandJSONError(cmd.ErrOrStderr(), err)
			return renderedCommandError{err: err}
		}
	}
	if cmd.RunE != nil {
		run := cmd.RunE
		cmd.RunE = func(cmd *cobra.Command, args []string) error {
			err := run(cmd, args)
			if err == nil || !commandWantsJSONError(cmd) {
				return err
			}
			_ = writeCommandJSONError(cmd.ErrOrStderr(), err)
			return renderedCommandError{err: err}
		}
	}
	cmd.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		if !commandWantsJSONError(cmd) {
			return err
		}
		_ = writeCommandJSONError(cmd.ErrOrStderr(), err)
		return renderedCommandError{err: err}
	})
	for _, child := range cmd.Commands() {
		wrapJSONErrorRendering(child)
	}
}

func commandWantsJSONError(cmd *cobra.Command) bool {
	if flag := cmd.Flags().Lookup("json"); flag != nil && flag.Changed {
		return commandJSONFlagEnabled(flag.Value.String())
	}
	if flag := cmd.InheritedFlags().Lookup("json"); flag != nil && flag.Changed {
		return commandJSONFlagEnabled(flag.Value.String())
	}
	for _, arg := range append(cmd.Flags().Args(), commandRawArgs(cmd)...) {
		if arg == "--json" {
			return true
		}
		if strings.HasPrefix(arg, "--json=") {
			value := strings.TrimPrefix(arg, "--json=")
			return value == "" || value == "true" || value == "1"
		}
	}
	return false
}

func commandJSONFlagEnabled(value string) bool {
	value = strings.ToLower(value)
	return value != "false" && value != "0"
}

func commandRawArgs(cmd *cobra.Command) []string {
	root := cmd.Root()
	if root == nil {
		return os.Args[1:]
	}
	value := reflect.ValueOf(root)
	if value.Kind() != reflect.Pointer || value.IsNil() {
		return os.Args[1:]
	}
	args := value.Elem().FieldByName("args")
	if !args.IsValid() || args.Kind() != reflect.Slice || args.IsNil() {
		return os.Args[1:]
	}
	out := make([]string, 0, args.Len())
	for i := 0; i < args.Len(); i++ {
		out = append(out, args.Index(i).String())
	}
	return out
}

var commandErrorCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

func writeCommandJSONError(w io.Writer, err error) error {
	envelope := commandJSONError{
		Code:    commandErrorCode(err),
		Message: err.Error(),
	}
	data, marshalErr := json.MarshalIndent(envelope, "", "  ")
	if marshalErr != nil {
		return marshalErr
	}
	_, writeErr := fmt.Fprintln(w, string(data))
	return writeErr
}

func commandErrorCode(err error) string {
	message := err.Error()
	code, _, ok := strings.Cut(message, ":")
	if ok {
		code = strings.TrimSpace(code)
		if commandErrorCodePattern.MatchString(code) {
			return code
		}
	}
	return "command_failed"
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
