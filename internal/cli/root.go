package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
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
		Use:   "entire-brain",
		Short: "Build and query a local repository brain for agents",
		// Version is what makes cobra install `--version` at all:
		// InitDefaultVersionFlag is a no-op on a command whose Version is
		// empty, so leaving it unset while carrying the value on Options meant
		// `--version`, `-v` and `-V` all failed with "unknown flag" on a binary
		// that knows perfectly well what it is. It is the same string the
		// `version` subcommand and the MCP serverInfo already report.
		Version:       opts.Version,
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
	// One binary must not answer "what version are you" two different ways.
	// cobra's default template prints "entire-brain version X"; `entire-brain
	// version` prints the bare string, and that is the form scripts already
	// parse.
	cmd.SetVersionTemplate("{{.Version}}\n")

	// The shell-completion generator targets the standalone binary name and is
	// dead weight when dispatched as `entire brain`, so drop it entirely.
	cmd.CompletionOptions.DisableDefaultCmd = true

	cmd.AddGroup(
		&cobra.Group{ID: "setup", Title: "Set up the brain & agents:"},
		&cobra.Group{ID: "explore", Title: "Search & explore the brain:"},
		&cobra.Group{ID: "facts", Title: "Durable facts & patterns:"},
		&cobra.Group{ID: "memory", Title: "Sessions & conversation memory:"},
		&cobra.Group{ID: "code", Title: "Code & semantic index:"},
		&cobra.Group{ID: "maintain", Title: "Brain state & maintenance:"},
		&cobra.Group{ID: "share", Title: "Workspaces & sharing:"},
		&cobra.Group{ID: "misc", Title: "Miscellaneous:"},
	)

	var helpCommands []*cobra.Command
	addGrouped := func(group string, c *cobra.Command) {
		c.GroupID = group
		cmd.AddCommand(c)
		helpCommands = append(helpCommands, c)
	}
	addHidden := func(c *cobra.Command) {
		c.Hidden = true
		cmd.AddCommand(c)
	}

	addGrouped("setup", newSetupCommand(opts))
	addGrouped("setup", newAddCommand(opts))
	addGrouped("setup", newInitAgentsCommand(opts))
	addGrouped("setup", newBrainGuideCommand(opts))
	addGrouped("setup", newMCPCommand(opts))
	addGrouped("setup", newConfigCommand(opts.Env))

	addGrouped("explore", newBrainOverviewCommand(opts))
	addGrouped("explore", newBrainBriefCommand(opts))
	addGrouped("explore", newQueryCommand(opts))
	addGrouped("explore", newSearchCommand(opts))
	addGrouped("explore", newVsearchCommand(opts))
	addGrouped("explore", newGetCommand(opts))
	addGrouped("explore", newMultiGetCommand(opts))
	addGrouped("explore", newDashCommand(opts))

	addGrouped("facts", newRecallCommand(opts))
	addGrouped("facts", newRememberCommand(opts))
	addGrouped("facts", newDistillCommand(opts))
	addGrouped("facts", newFactsCommand(opts))
	addGrouped("facts", newVerifyCommand(opts))
	addGrouped("facts", newPatternsCommand(opts))

	addGrouped("memory", newMemoryCommand(opts))
	addGrouped("memory", newPrivacyCommand(opts))

	addGrouped("code", newBrainInspectCommand(opts))
	addGrouped("code", newBrainShowCommand(opts))
	addGrouped("code", newEntitiesCommand(opts))
	addGrouped("code", newVizCommand(opts))
	addGrouped("code", newSemanticRepairCommand(opts))

	addGrouped("maintain", newAgentStatusCommand(opts))
	addGrouped("maintain", newStatsCommand(opts))
	addGrouped("maintain", newRefreshCommand(opts))
	addGrouped("maintain", newWatchCommand(opts))
	addGrouped("maintain", newSemanticGCCommand(opts))
	addGrouped("maintain", newSemanticResetCommand(opts))

	addGrouped("share", newWorkspaceCommand(opts))
	addGrouped("share", newSemanticBundleCommand(opts))
	addGrouped("share", newPublishCommand(opts))

	addGrouped("misc", newPathCommand(opts))
	addGrouped("misc", newRepoIdentityCommand(opts))
	addGrouped("misc", newCapabilitiesCommand(opts))
	addGrouped("misc", newDoctorCommand(opts))
	addGrouped("misc", newBenchmarkCommand(opts))
	addGrouped("misc", newVersionCommand(opts.Version))

	// Hidden: `review` is the machine contract `entire review`'s diff-less mode shells
	// (`entire-brain review --json`), NOT a human-facing verb — the human
	// review surface is `entire review` in the cli, not a standalone brain command.
	addHidden(newBrainReviewCommand(opts))

	// Hidden measurement harness for the history retrieval layer (the facts
	// analog lives under `facts eval`/`eval-gen`; summaries are compatible with
	// `facts eval-compare`). Developer tooling, not agent surface.
	addHidden(newHistoryEvalGenCommand(opts))
	addHidden(newHistoryEvalCommand(opts))

	// Hidden agent-harness hook surface (Phase 2 item 2): wired into hooks by
	// the harness (Claude Code, Entire CLI), not invoked by humans or agents.
	addHidden(newHookCommand(opts))

	cmd.SetHelpCommandGroupID("misc")
	setRootUsage(cmd, helpCommands)

	wrapJSONErrorRendering(cmd)
	return cmd
}

// setRootUsage preserves the workflow order without changing Cobra's global
// sorting setting or the default help for subcommands.
func setRootUsage(root *cobra.Command, commands []*cobra.Command) {
	defaultUsage := root.UsageFunc()
	defaultHelp := root.HelpFunc()
	helpWidth := 0
	root.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		// Cobra renders UsageString into a buffer; measure the real output first.
		if cmd == root {
			helpWidth = helpTerminalWidth(cmd.OutOrStdout())
			defer func() { helpWidth = 0 }()
		}
		defaultHelp(cmd, args)
	})
	root.SetUsageFunc(func(cmd *cobra.Command) error {
		if cmd != root {
			return defaultUsage(cmd)
		}
		ordered := append([]*cobra.Command(nil), commands...)
		for _, c := range cmd.Commands() {
			if c.Name() == "help" {
				ordered = append(ordered, c)
			}
		}
		var out strings.Builder
		fmt.Fprintf(&out, "Usage:\n  %s\n  %s [command]\n", cmd.UseLine(), cmd.CommandPath())
		width := 0
		for _, c := range ordered {
			if !c.Hidden && len(c.Name()) > width {
				width = len(c.Name())
			}
		}
		terminalWidth := helpWidth
		if terminalWidth == 0 {
			terminalWidth = helpTerminalWidth(cmd.OutOrStderr())
		}
		for _, group := range cmd.Groups() {
			fmt.Fprintf(&out, "\n%s\n", group.Title)
			for _, c := range ordered {
				if c.GroupID == group.ID && !c.Hidden && (c.IsAvailableCommand() || c.Name() == "help") {
					writeHelpCommand(&out, c.Name(), c.Short, width, terminalWidth)
				}
			}
		}
		fmt.Fprintf(&out, "\nFlags:\n%s", cmd.LocalFlags().FlagUsages())
		fmt.Fprintf(&out, "\nUse %q for more information about a command.\n", cmd.CommandPath()+" [command] --help")
		_, err := io.WriteString(cmd.OutOrStderr(), out.String())
		return err
	})
}

// Prefer the output terminal. Entire may pipe plugin output, so also consult
// the controlling terminal for file-backed output. Unknown width means no wrap.
func helpTerminalWidth(out io.Writer) int {
	if f, ok := out.(interface{ Fd() uintptr }); ok {
		if width, _, err := term.GetSize(f.Fd()); err == nil && width > 0 {
			return width
		}
		if tty, err := os.Open("/dev/tty"); err == nil {
			defer tty.Close()
			if width, _, err := term.GetSize(tty.Fd()); err == nil && width > 0 {
				return width
			}
		}
	}
	if width, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && width > 0 {
		return width
	}
	return 0
}

func writeHelpCommand(out io.Writer, name, description string, nameWidth, terminalWidth int) {
	indent := nameWidth + 4
	prefix := fmt.Sprintf("  %-*s  ", nameWidth, name)
	if terminalWidth <= 0 || ansi.StringWidth(prefix+description) <= terminalWidth {
		fmt.Fprintln(out, prefix+description)
		return
	}
	// On very narrow terminals, put the description below the command name.
	if terminalWidth <= indent {
		fmt.Fprintf(out, "  %s\n", name)
		indent = min(2, max(0, terminalWidth-1))
		prefix = strings.Repeat(" ", indent)
	}
	wrapped := ansi.Wrap(description, max(1, terminalWidth-indent), "")
	fmt.Fprintln(out, prefix+strings.ReplaceAll(wrapped, "\n", "\n"+strings.Repeat(" ", indent)))
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
