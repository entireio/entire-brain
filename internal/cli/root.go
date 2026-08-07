package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"regexp"
	"strings"
	"time"

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
		Short:         "Build and query a local repository brain for agents",
		SilenceUsage:  true,
		SilenceErrors: true,
		Long: `entire-brain is an external-command plugin for the Entire CLI.

It builds a local, inspectable repository brain from retained sessions, seed
context, docs, history, semantic records, and durable facts, then exposes
retrieval, verification, evaluation, and MCP surfaces for agents.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}

	// The shell-completion generator targets the standalone binary name and is
	// dead weight when dispatched as `entire brain`, so drop it entirely.
	cmd.CompletionOptions.DisableDefaultCmd = true

	cmd.AddGroup(
		&cobra.Group{ID: "create", Title: "Create the brain:"},
		&cobra.Group{ID: "explore", Title: "Explore the brain:"},
		&cobra.Group{ID: "maintain", Title: "Maintain & share:"},
	)

	addGrouped := func(group string, c *cobra.Command) {
		c.GroupID = group
		cmd.AddCommand(c)
	}
	// addHidden registers commands kept out of help but still invocable: the
	// Entire CLI host / MCP clients drive some programmatically, and the rest
	// are redundant aliases fully covered by `search` and `inspect <sub>`.
	addHidden := func(c *cobra.Command) {
		c.Hidden = true
		cmd.AddCommand(c)
	}

	// Create the brain — locate it, build/refresh it, manage workspaces.
	addGrouped("create", newPathCommand(opts))
	addGrouped("create", newAddCommand(opts))
	addGrouped("create", newRefreshCommand(opts))
	addGrouped("create", newWatchCommand(opts))
	addGrouped("create", newDistillCommand(opts))
	addGrouped("create", newRememberCommand(opts))
	addGrouped("create", newPatternsCommand(opts))
	addGrouped("create", newAgentStatusCommand(opts))
	addGrouped("create", newWorkspaceCommand(opts))

	// Explore the brain — task packets, search, records, specialist inspection.
	addGrouped("explore", newBrainOverviewCommand(opts))
	addGrouped("explore", newBrainBriefCommand(opts))
	addGrouped("explore", newBrainGuideCommand())
	addGrouped("explore", newBrainInspectCommand(opts))
	addGrouped("explore", newMCPCommand(opts))
	addGrouped("explore", newRecallCommand(opts))
	addGrouped("explore", newVerifyCommand(opts))
	// qmd-inspired retrieval over the unified text index (facts + history + docs).
	// Symbol/code search stays at `inspect code`.
	addGrouped("explore", newQueryCommand(opts))
	addGrouped("explore", newSearchCommand(opts))
	addGrouped("explore", newVsearchCommand(opts))
	addGrouped("explore", newGetCommand(opts))
	addGrouped("explore", newMultiGetCommand(opts))
	addGrouped("explore", newBrainShowCommand(opts))
	addGrouped("explore", newDashCommand(opts))
	addGrouped("explore", newVizCommand(opts))

	// Maintain & share — freshness, cleanup, portability, version.
	// (Build stages live under `refresh`: sessions, index, seed.)
	addGrouped("maintain", newSemanticBundleCommand(opts))
	addGrouped("maintain", newPublishCommand(opts))
	addGrouped("maintain", newBenchmarkCommand(opts))
	addGrouped("maintain", newFactsCommand(opts))
	addGrouped("maintain", newStatsCommand(opts))
	addGrouped("maintain", newSemanticGCCommand(opts))
	addGrouped("maintain", newSemanticRepairCommand(opts))
	addGrouped("maintain", newSemanticResetCommand(opts))
	addGrouped("maintain", newVersionCommand(opts.Version))

	// Hidden: `review` is the machine contract `entire review`'s diff-less mode shells
	// (`entire-brain review --json`), NOT a human-facing verb — the human
	// review surface is `entire review` in the cli, not a standalone brain command.
	addHidden(newBrainReviewCommand(opts))

	// Hidden plugin/config commands.
	addHidden(newDoctorCommand(opts))
	addHidden(newConfigCommand(opts.Env))

	// Hidden measurement harness for the history retrieval layer (the facts
	// analog lives under `facts eval`/`eval-gen`; summaries are compatible with
	// `facts eval-compare`). Developer tooling, not agent surface.
	addHidden(newHistoryEvalGenCommand(opts))
	addHidden(newHistoryEvalCommand(opts))

	// Hidden agent-harness hook surface (Phase 2 item 2): wired into hooks by
	// the harness (Claude Code, Entire CLI), not invoked by humans or agents.
	addHidden(newHookCommand(opts))

	cmd.SetHelpCommandGroupID("maintain")

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
	var rendered renderedCommandError
	return errors.As(err, &rendered)
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
			if commandErrorWasRendered(err) {
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
	if flag := cmd.Flags().Lookup("format"); flag != nil && flag.Changed {
		if wants, ok := commandFormatFlagWantsJSON(flag.Value.String()); ok {
			return wants
		}
	}
	if flag := cmd.InheritedFlags().Lookup("format"); flag != nil && flag.Changed {
		if wants, ok := commandFormatFlagWantsJSON(flag.Value.String()); ok {
			return wants
		}
	}
	if wants, ok := commandRawFormatWantsJSON(append(cmd.Flags().Args(), commandRawArgs(cmd)...)); ok {
		return wants
	}
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

func commandFormatFlagWantsJSON(value string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "json":
		return true, true
	case "cli":
		return false, true
	default:
		return false, false
	}
}

func commandRawFormatWantsJSON(args []string) (bool, bool) {
	for i, arg := range args {
		if arg == "--format" {
			if i+1 >= len(args) {
				return false, false
			}
			return commandFormatFlagWantsJSON(args[i+1])
		}
		if strings.HasPrefix(arg, "--format=") {
			return commandFormatFlagWantsJSON(strings.TrimPrefix(arg, "--format="))
		}
	}
	return false, false
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
