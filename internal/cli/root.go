package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/cmdutil"
	"io"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
	"github.com/entireio/entire-brain/internal/cli/command/add"
	"github.com/entireio/entire-brain/internal/cli/command/bench"
	"github.com/entireio/entire-brain/internal/cli/command/brainreview"
	"github.com/entireio/entire-brain/internal/cli/command/capabilities"
	"github.com/entireio/entire-brain/internal/cli/command/config"
	"github.com/entireio/entire-brain/internal/cli/command/connect"
	"github.com/entireio/entire-brain/internal/cli/command/daemonreregister"
	"github.com/entireio/entire-brain/internal/cli/command/dash"
	"github.com/entireio/entire-brain/internal/cli/command/disconnect"
	"github.com/entireio/entire-brain/internal/cli/command/docs"
	"github.com/entireio/entire-brain/internal/cli/command/facts"
	"github.com/entireio/entire-brain/internal/cli/command/guide"
	"github.com/entireio/entire-brain/internal/cli/command/hook"
	"github.com/entireio/entire-brain/internal/cli/command/patternscmd"
	"github.com/entireio/entire-brain/internal/cli/command/pause"
	"github.com/entireio/entire-brain/internal/cli/command/repoidentity"
	"github.com/entireio/entire-brain/internal/cli/command/retrieve"
	"github.com/entireio/entire-brain/internal/cli/command/stats"
	"github.com/entireio/entire-brain/internal/cli/command/version"
	"github.com/entireio/entire-brain/internal/cli/command/viz"
	"github.com/entireio/entire-brain/internal/cli/command/watch"
	"github.com/entireio/entire-brain/internal/cli/command/webhook"
	"github.com/entireio/entire-brain/internal/cli/entities"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Execute runs the plugin root command with the real process environment.
func Execute(version string) error {
	cmd := NewRootCommand(Options{
		Version: version,
		Env:     base.EnvFromOS(),
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
		// `version` subcommand already reports.
		Version:       opts.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		Long: `entire-brain is an external-command plugin for the Entire CLI.

It builds a local, inspectable repository brain from retained sessions, repository
context, docs, history, code-index records, and durable memories, then exposes
retrieval, verification, and evaluation surfaces for agents.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	// One binary must not answer "what version are you" two different ways.
	// cobra's default template prints "entire-brain version X"; `entire-brain
	// version` prints the bare string, and that is the form scripts already
	// parse.
	cmd.SetVersionTemplate("{{.Version}}\n")
	cmd.Flags().Bool("help-all", false, "List all commands, including hidden commands")

	// The shell-completion generator targets the standalone binary name and is
	// dead weight when dispatched as `entire brain`, so drop it entirely.
	cmd.CompletionOptions.DisableDefaultCmd = true

	cmd.AddGroup(
		&cobra.Group{ID: "setup", Title: "Set up the brain & agents:"},
		&cobra.Group{ID: "explore", Title: "Search & explore the brain:"},
		&cobra.Group{ID: "facts", Title: "Durable memories & patterns:"},
		&cobra.Group{ID: "code", Title: "Code index & context:"},
		&cobra.Group{ID: "maintain", Title: "Brain state & maintenance:"},
		&cobra.Group{ID: "share", Title: "Workspaces & sharing:"},
		&cobra.Group{ID: "operations", Title: "Troubleshooting & operations:"},
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
	addGrouped("setup", add.New(opts))
	addGrouped("setup", guide.New(opts))
	addGrouped("setup", webhook.New(opts))
	addGrouped("setup", config.New(opts.Env))

	addGrouped("explore", newBrainOverviewCommand(opts))
	addGrouped("explore", newBrainBriefCommand(opts))
	addGrouped("explore", retrieve.NewQuery(retrieveCommandConfig(opts)))
	addGrouped("explore", retrieve.NewSearch(retrieveCommandConfig(opts)))
	addGrouped("explore", retrieve.NewVsearch(retrieveCommandConfig(opts)))
	addGrouped("explore", retrieve.NewGet(retrieveCommandConfig(opts)))
	addGrouped("explore", retrieve.NewMultiGet(retrieveCommandConfig(opts)))
	addGrouped("explore", dash.New(dashConfig(opts)))

	addGrouped("facts", facts.NewRecall(factsCommandConfig(opts)))
	addGrouped("facts", facts.NewRemember(factsCommandConfig(opts)))
	addGrouped("facts", newDistillCommand(opts))
	addGrouped("facts", newFactsCommand(opts))
	addGrouped("facts", connect.New(connectConfig(opts)))
	addGrouped("facts", disconnect.New(disconnectConfig(opts)))
	addGrouped("facts", newVerifyCommand(opts))
	addGrouped("facts", patternscmd.New(patternsCmdConfig(opts)))

	addGrouped("operations", newMemoryCommand(opts))
	addGrouped("operations", newPrivacyCommand(opts))

	addGrouped("code", newBrainInspectCommand(opts))
	addGrouped("code", newBrainShowCommand(opts))
	addGrouped("operations", entities.NewCommand(entityConfig(opts)))
	addGrouped("code", viz.New(vizConfig(opts)))
	addGrouped("operations", newSemanticRepairCommand(opts))

	addGrouped("maintain", newAgentStatusCommand(opts))
	addGrouped("maintain", stats.New(statsConfig(opts)))
	addGrouped("operations", docs.New(opts))
	addGrouped("operations", newRefreshCommand(opts))
	addGrouped("operations", watch.New(watchConfig(opts)))
	addHidden(daemonreregister.New(daemonReregisterConfig(opts)))
	addGrouped("operations", pause.NewStop(opts))
	addGrouped("operations", pause.NewStart(opts))
	addGrouped("operations", newSemanticResetCommand(opts))

	addGrouped("share", newWorkspaceCommand(opts))
	addGrouped("share", newSemanticBundleCommand(opts))

	addGrouped("misc", newPathCommand(opts))
	addGrouped("operations", repoidentity.New(repoIdentityConfig(opts)))
	addGrouped("misc", capabilities.New(opts))
	addGrouped("misc", newDoctorCommand(opts))
	addHidden(bench.New(benchConfig(opts)))
	addGrouped("misc", version.New(opts.Version))

	// Hidden: `review` is the machine contract `entire review`'s diff-less mode shells
	// (`entire-brain review --json`), NOT a human-facing verb — the human
	// review surface is `entire review` in the cli, not a standalone brain command.
	addHidden(brainreview.New(brainReviewConfig(opts)))

	// Hidden measurement harness for the history retrieval layer (the facts
	// analog lives under `facts eval`/`eval-gen`; summaries are compatible with
	// `facts eval-compare`). Developer tooling, not agent surface.
	addHidden(newHistoryEvalGenCommand(opts))
	addHidden(newHistoryEvalCommand(opts))

	// Hidden agent-harness hook surface (Phase 2 item 2): wired into hooks by
	// the harness (Claude Code, Entire CLI), not invoked by humans or agents.
	addHidden(hook.New(hookCommandConfig(opts)))

	// Tombstone: `init-agents` was removed (folded into `setup`). Hidden so
	// scripts calling the old name fail with a pointer to the replacement
	// instead of cobra's generic "unknown command".
	addHidden(newInitAgentsTombstoneCommand())

	// Tombstone: `publish` was removed (the brain-bundle endpoint it posted
	// to was never deployed; memories travel the per-fact wire). Hidden so
	// old invocations fail with a pointer to `memories sync` instead of
	// cobra's generic "unknown command".
	addHidden(newPublishTombstoneCommand())

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
		type helpEntry struct {
			command *cobra.Command
			name    string
			group   string
		}
		var ordered []helpEntry
		for _, c := range commands {
			ordered = append(ordered, helpEntry{c, c.Name(), c.GroupID})
			if c.Name() == "memories" {
				// Surface operator paths without moving the everyday memories family
				// or changing any child's registration or visibility.
				for _, name := range []string{"import", "sync", "proposals", "gc", "vitality"} {
					for _, child := range c.Commands() {
						if child.Name() == name {
							ordered = append(ordered, helpEntry{child, "memories " + name, "operations"})
						}
					}
				}
			}
		}
		for _, c := range cmd.Commands() {
			if c.Name() == "help" {
				ordered = append(ordered, helpEntry{c, c.Name(), c.GroupID})
			}
		}
		var out strings.Builder
		fmt.Fprintf(&out, "Usage:\n  %s\n  %s [command]\n", cmd.UseLine(), cmd.CommandPath())
		width := 0
		for _, entry := range ordered {
			if !entry.command.Hidden && len(entry.name) > width {
				width = len(entry.name)
			}
		}
		terminalWidth := helpWidth
		if terminalWidth == 0 {
			terminalWidth = helpTerminalWidth(cmd.OutOrStderr())
		}
		for _, group := range cmd.Groups() {
			fmt.Fprintf(&out, "\n%s\n", group.Title)
			for _, entry := range ordered {
				c := entry.command
				if entry.group == group.ID && !c.Hidden && (c.IsAvailableCommand() || c.Name() == "help") {
					writeHelpCommand(&out, entry.name, c.Short, width, terminalWidth)
				}
			}
		}
		if all, _ := cmd.Flags().GetBool("help-all"); all {
			writeAllCommandHelp(&out, cmd, terminalWidth)
		}
		fmt.Fprintf(&out, "\nFlags:\n%s", cmd.LocalFlags().FlagUsages())
		fmt.Fprintf(&out, "\nUse %q for more information about a command.\n", cmd.CommandPath()+" [command] --help")
		_, err := io.WriteString(cmd.OutOrStderr(), out.String())
		return err
	})
}

// List full paths without changing Hidden: all-help is an inspection surface,
// not a visibility change to the command tree or subsequent help invocations.
func writeAllCommandHelp(out io.Writer, root *cobra.Command, terminalWidth int) {
	var commands []*cobra.Command
	var paths []string
	width := 0
	var collect func(*cobra.Command, string)
	collect = func(parent *cobra.Command, prefix string) {
		for _, child := range parent.Commands() {
			path := prefix + child.Name()
			commands = append(commands, child)
			paths = append(paths, path)
			width = max(width, len(path))
			collect(child, path+" ")
		}
	}
	collect(root, "")
	fmt.Fprintln(out, "\nAll commands (including developer and machine commands):")
	for i, cmd := range commands {
		writeHelpCommand(out, paths[i], cmd.Short, width, terminalWidth)
	}
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

// The rendered-error marker lives in cmdutil so extracted command packages can
// construct it; the alias keeps the package-local name for existing callers.
type renderedCommandError = cmdutil.RenderedCommandError

var errRenderedCommand = renderedCommandError{}

func RenderedError() error {
	return errRenderedCommand
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
			return renderedCommandError{Err: err}
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
			return renderedCommandError{Err: err}
		}
	}
	// CHAIN, do not replace. This walks every child, and a plain
	// SetFlagErrorFunc here silently discarded any handler a command had set
	// for itself -- which is how the `--symbol` hint on `inspect impact`
	// vanished without a trace: the command set it, this overwrote it, and the
	// user still saw cobra's bare "unknown flag".
	//
	// Any command may now improve its own flag error, and this still gets to
	// render the JSON form on top.
	prior := flagErrorFuncOf(cmd)
	cmd.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		if prior != nil {
			err = prior(cmd, err)
		}
		if !commandWantsJSONError(cmd) {
			return err
		}
		_ = writeCommandJSONError(cmd.ErrOrStderr(), err)
		return renderedCommandError{Err: err}
	})
	for _, child := range cmd.Commands() {
		wrapJSONErrorRendering(child)
	}
}

// flagErrorFuncOf returns a command's OWN flag-error handler, or nil when it
// has none.
//
// cobra's FlagErrorFunc() walks up to the root when a command has not set one,
// so calling it directly would return the parent's and chain it to itself.
// Comparing against the parent's is what distinguishes "this command set one"
// from "this command inherited one".
func flagErrorFuncOf(cmd *cobra.Command) func(*cobra.Command, error) error {
	own := cmd.FlagErrorFunc()
	if own == nil || cmd.Parent() == nil {
		return own
	}
	inherited := cmd.Parent().FlagErrorFunc()
	if inherited != nil && reflect.ValueOf(own).Pointer() == reflect.ValueOf(inherited).Pointer() {
		return nil
	}
	return own
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

func writeCommandJSONError(w io.Writer, err error) error {
	envelope := commandJSONError{
		Code:    cmdutil.CommandErrorCode(err),
		Message: err.Error(),
	}
	data, marshalErr := json.MarshalIndent(envelope, "", "  ")
	if marshalErr != nil {
		return marshalErr
	}
	_, writeErr := fmt.Fprintln(w, string(data))
	return writeErr
}

// legacyFlagAlias keeps a renamed flag's old spelling parseable by normalizing
// it to the new name. One per flag set: SetNormalizeFunc is single-slot, so a
// second call on the same FlagSet replaces the first.
func legacyFlagAlias(fs *pflag.FlagSet, oldName, newName string) {
	fs.SetNormalizeFunc(func(f *pflag.FlagSet, name string) pflag.NormalizedName {
		if name == oldName {
			name = newName
		}
		return pflag.NormalizedName(name)
	})
}
