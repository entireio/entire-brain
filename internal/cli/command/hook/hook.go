package hook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/cmdutil"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	"github.com/spf13/cobra"
)

// hook.go is Phase 2 item 2 (agent-utility plan): moment-of-relevance
// delivery. Everything else in the brain is pull — the agent must know to
// ask. These verbs are designed to be wired into agent-harness hooks (Claude
// Code PreToolUse/PostToolUse, Entire CLI hooks) so the right fact arrives
// unprompted at the moment it changes a decision:
//
//	entire-brain hook pre-edit --file <path>     # facts anchored to the file
//	entire-brain hook post-failure < stderr.txt  # dead ends matching a failure
//
// Contract (the harness-safety rules every emission obeys):
//   - SILENT, HONEST EMPTY: nothing crosses the bar -> no output, exit 0. A
//     hook that emits noise gets removed from the harness within a day.
//   - NEVER FAILS: a missing/empty brain, an unresolvable branch, or any data
//     problem is silence, not an error — a hook must never break the harness.
//     Only flag misuse (e.g. pre-edit without --file) errors, so hand-testing
//     stays debuggable.
//   - TOKEN-BUDGETED: emissions are capped (default 200 estimated tokens);
//     highest-relevance facts first.
//   - WARM PATH ONLY: lexical matching over the branch's loaded facts. No
//     embedder, no FTS build, no semantic-store open. (Symbol-level pre-edit
//     enrichment arrives with the worktree overlay seam.)
const hookDefaultBudgetTokens = 200

// hookPostFailureMinScore is the relevance bar for matching a failure against
// gotcha/closed-negative facts: 40 requires several strong term hits or a
// locus/substring-grade signal (term hit = 10, locus overlap = 60, whole-query
// substring = 200), keeping the post-failure hook high-precision.
const hookPostFailureMinScore = 40

// hookFailureTailBytes caps how much failure text is considered. Errors
// cluster at the end of output; an unbounded query over a huge build log
// would dilute term matching and slow the hook.
const hookFailureTailBytes = 4096

func New(cfg Config) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hook",
		Short: "Agent-harness hook surface: surface relevant memories at the moment of action",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newPreEditCommand(cfg))
	cmd.AddCommand(NewPostFailure(cfg))
	cmd.AddCommand(NewSessionEnd(cfg))
	return cmd
}

// NewSessionEnd is Phase 2 item 5: per-session incremental distill,
// closing the write loop. Batch distill is expensive and run occasionally, so
// today's insights are invisible to tomorrow's session unless someone
// remembers to run it. Wired to a session-lifecycle hook, this distills the
// just-ended session — incremental export/overlay first (free), then a durable
// lifecycle hint, then distill bounded to that one session (--session).
// Long-term projection work is owned by the memory coordinator; the hook only
// reduces latency and correctness never depends on it firing. The incremental
// cache makes re-firing for the same session free.
//
// Unlike pre-edit/post-failure this verb may legitimately take minutes and
// spend tokens — the session is over, nobody is waiting — but it keeps the
// rest of the hook contract: stdout stays empty (a one-line summary goes to
// stderr), and environment problems (no repo, no brain, no agent on PATH)
// are silence, never an error.
func NewSessionEnd(cfg Config) *cobra.Command {
	var (
		sessionID string
		branch    string
		agent     string
		model     string
		effort    string
	)
	cmd := &cobra.Command{
		Use:   "session-end --session <id>",
		Short: "Refresh and distill the just-ended session (spends tokens; silent on env problems)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(sessionID) == "" {
				return fmt.Errorf("--session <id> is required")
			}
			return cfg.Deps.SessionEnd(cmd, cfg.Options, sessionID, branch, agent, model, effort)
		},
	}
	cmd.Flags().StringVar(&sessionID, "session", "", "The just-ended session's id")
	cmd.Flags().StringVar(&branch, "branch", "", "Captured branch when the host knows it")
	cmd.Flags().StringVar(&agent, "agent", "auto", "Distill agent: auto, codex, claude-code, or command")
	cmd.Flags().StringVar(&model, "model", "", "Cheap/fast model override for the distill agent (recommended)")
	cmd.Flags().StringVar(&effort, "effort", "", "Reasoning-effort override (e.g. low) — pairs with --model for a cheap run")
	return cmd
}

func newPreEditCommand(cfg Config) *cobra.Command {
	var (
		file       string
		branch     string
		budget     int
		jsonOut    bool
		claudeHook bool
	)
	cmd := &cobra.Command{
		Use:   "pre-edit [--file <path> | --claude-hook]",
		Short: "Memories locus-anchored to a file about to be edited (silent when none)",
		Long: `pre-edit surfaces the memories anchored to a file an agent is about to change.

--file names the file directly, for hand-testing and for harnesses that pass
arguments. --claude-hook is the wired spelling: Claude Code delivers a
PreToolUse event as JSON on stdin and only reads a JSON envelope back, so the
flag reads the path from the event and replies in the shape the host parses.
Plain stdout is NOT added to the agent's context for PreToolUse, which is why
the text emission alone could never have reached an agent. ` + base.SetupBrainBinaryName + ` setup
wires it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Read the event when asked to, and also when no --file was given
			// and stdin is a pipe: a harness that invokes the bare verb then
			// still works instead of failing on a missing flag.
			//
			// A terminal stdin is never read, on EITHER path. --claude-hook used
			// to bypass this guard, so hand-running the exact command the docs
			// suggest -- the flag's own documented testing route -- blocked for
			// ever on a read that would never return. Say so instead.
			if claudeHook && !StdinIsPipe(cmd.InOrStdin()) && strings.TrimSpace(file) == "" {
				return fmt.Errorf("--claude-hook reads the host's PreToolUse event from stdin, and stdin is a terminal here; pipe an event in, or pass --file <path> to check one file by hand")
			}
			// Only read when the answer is still missing. Keying this on
			// claudeHook as well meant `--file X --claude-hook` read the event
			// anyway and then discarded it, because the inner guard refuses to
			// overwrite a --file that was given. On a pipe that does not reach
			// EOF -- one inherited from a parent that has not closed it -- that
			// read blocks for up to maxClaudeHookEventBytes, which is the
			// hand-run hang the check above this one exists to prevent.
			if strings.TrimSpace(file) == "" && StdinIsPipe(cmd.InOrStdin()) {
				if path, ok := DecodeClaudeHookEventPath(cmd.InOrStdin()); ok {
					file = path
				}
			}
			if strings.TrimSpace(file) == "" {
				// An event that names no path at all (a Bash call reaching a
				// loose matcher, say) is the hook contract's silent-honest-empty
				// case, not misuse. Misuse is still an error, so hand-testing
				// stays debuggable.
				if claudeHook {
					return nil
				}
				return fmt.Errorf("--file is required")
			}
			facts, target, ok := hookLoadFacts(cmd, cfg, branch)
			if !ok {
				return nil // no brain / no facts: silence, never an error
			}
			if filepath.IsAbs(file) {
				repoRel, err := filepath.Rel(target.repoDir, file)
				if err != nil || repoRel == ".." || strings.HasPrefix(repoRel, ".."+string(filepath.Separator)) {
					return nil
				}
				file = repoRel
			}
			rel := filepath.ToSlash(file)
			stem := strings.TrimSuffix(filepath.Base(rel), filepath.Ext(rel))
			// The stem ("distill_cmd") matches facts that name the file without
			// its extension or talk about its symbols' shared prefix.
			hits := cfg.Deps.FactsRelevantToChange([]string{rel, stem}, facts, 8)
			var kept []factmerge.Record
			var err error
			if claudeHook {
				kept, err = hookEmitClaudeEnvelope(cmd, hits, budget)
			} else {
				kept, err = Emit(cmd, hits, budget, jsonOut)
			}
			if err == nil {
				hookRecordServed(cmd, cfg, target, "hook-pre-edit", rel, kept)
			}
			return err
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "Path of the file about to be edited (repo-relative or absolute)")
	cmd.Flags().StringVar(&branch, "branch", "", "Branch whose memories to consult (default: current)")
	cmd.Flags().IntVar(&budget, "budget", hookDefaultBudgetTokens, "Token budget for the emission")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit JSON instead of text lines")
	cmd.Flags().BoolVar(&claudeHook, "claude-hook", false, "Read a Claude Code PreToolUse event on stdin and reply with its hook envelope")
	return cmd
}

// claudeHookEvent is the subset of Claude Code's PreToolUse payload this hook
// needs. Every other field is ignored rather than rejected: the host owns the
// shape and adds to it, and a hook that failed on an unknown key would break
// the harness on the next release.
type claudeHookEvent struct {
	ToolInput struct {
		FilePath string `json:"file_path"`
		// NotebookEdit names its target differently; a hook wired to a matcher
		// that includes it would otherwise see every notebook edit as pathless.
		NotebookPath string `json:"notebook_path"`
	} `json:"tool_input"`
}

// DecodeClaudeHookEventPath reads one PreToolUse event and returns the path the
// tool is about to write. Malformed input is "no path", never an error — the
// hook contract is that a data problem is silence.
func DecodeClaudeHookEventPath(r io.Reader) (string, bool) {
	if r == nil {
		return "", false
	}
	data, err := io.ReadAll(io.LimitReader(r, maxClaudeHookEventBytes))
	if err != nil || len(bytes.TrimSpace(data)) == 0 {
		return "", false
	}
	var event claudeHookEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return "", false
	}
	for _, candidate := range []string{event.ToolInput.FilePath, event.ToolInput.NotebookPath} {
		if path := strings.TrimSpace(candidate); path != "" {
			return path, true
		}
	}
	return "", false
}

// maxClaudeHookEventBytes bounds the event read. The payload is small, but a
// hook must not be the thing that runs the machine out of memory.
const maxClaudeHookEventBytes = 1 << 20

// StdinIsPipe reports whether stdin carries piped input. A character device is
// a terminal, and reading one blocks forever — the difference between a hook
// that no-ops when hand-run and one that hangs the shell.
func StdinIsPipe(r io.Reader) bool {
	file, ok := r.(*os.File)
	if !ok {
		// A test buffer or any other reader: readable without blocking.
		return true
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice == 0
}

// hookEmitClaudeEnvelope writes the ONE stdout shape Claude Code feeds back into
// the agent's context for PreToolUse. Plain lines on stdout are discarded by the
// host at exit 0, so the text emission — the only one this verb had — could
// never reach an agent no matter how it was wired.
//
// It keeps the rest of the hook contract exactly: nothing at all when no fact
// cleared the bar (an empty envelope is noise), the same budget, and no
// permissionDecision, because surfacing a fact must never block an edit.
func hookEmitClaudeEnvelope(cmd *cobra.Command, hits []factmerge.Record, budget int) ([]factmerge.Record, error) {
	kept := hookKeepWithinBudget(hits, budget)
	if len(kept) == 0 {
		return nil, nil
	}
	lines := make([]string, 0, len(kept)+1)
	lines = append(lines, "Memories this project recorded about this file:")
	for _, f := range kept {
		lines = append(lines, fmt.Sprintf("- [%s] %s", factstore.KindOrInferred(f), f.Text))
	}
	type specific struct {
		HookEventName     string `json:"hookEventName"`
		AdditionalContext string `json:"additionalContext"`
	}
	envelope := struct {
		HookSpecificOutput specific `json:"hookSpecificOutput"`
	}{specific{HookEventName: "PreToolUse", AdditionalContext: strings.Join(lines, "\n")}}
	return kept, cmdutil.WriteJSON(cmd, envelope)
}

func NewPostFailure(cfg Config) *cobra.Command {
	var (
		command string
		branch  string
		budget  int
		jsonOut bool
	)
	cmd := &cobra.Command{
		Use:   "post-failure [--command <cmd>]",
		Short: "Gotchas and closed-negatives matching a failure (text on stdin; silent when none)",
		Long: `post-failure matches a failed command and its output against the brain's
gotcha and closed-negative facts — the "this was tried and failed" and "this
breaks if" knowledge — so an agent learns about a known dead end the moment it
steps into one, not after re-deriving it. Pipe the failure output on stdin:

    go test ./... 2>&1 | tail -20 | entire-brain hook post-failure --command "go test"`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			failure := hookReadFailureTail(cmd.InOrStdin())
			query := strings.TrimSpace(command + " " + failure)
			if query == "" {
				return nil // nothing to match against: silence
			}
			facts, target, ok := hookLoadFacts(cmd, cfg, branch)
			if !ok {
				return nil
			}
			hits := MatchFailure(facts, query, cfg.Deps.FactLexicalScore)
			kept, err := Emit(cmd, hits, budget, jsonOut)
			if err == nil {
				hookRecordServed(cmd, cfg, target, "hook-post-failure", query, kept)
			}
			return err
		},
	}
	cmd.Flags().StringVar(&command, "command", "", "The command that failed (added to the match query)")
	cmd.Flags().StringVar(&branch, "branch", "", "Branch whose memories to consult (default: current)")
	cmd.Flags().IntVar(&budget, "budget", hookDefaultBudgetTokens, "Token budget for the emission")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit JSON instead of text lines")
	return cmd
}

// hookTarget carries the resolved brain location so an emission can leave a
// serve receipt.
type hookTarget struct {
	repoDir  string
	brainDir string
	branch   string
}

// hookLoadFacts resolves the brain and loads the branch's facts, reporting
// ok=false (silence) on any problem — the hook contract.
func hookLoadFacts(cmd *cobra.Command, cfg Config, branch string) ([]factmerge.Record, hookTarget, bool) {
	repoDir, brainDir, resolvedBranch, err := cfg.Deps.ResolveTarget(cmd.Context(), cfg.Options, branch)
	if err != nil {
		return nil, hookTarget{}, false
	}
	facts, err := factstore.Load(brainDir, resolvedBranch)
	if err != nil || len(facts) == 0 {
		return nil, hookTarget{}, false
	}
	return facts, hookTarget{repoDir: repoDir, brainDir: brainDir, branch: resolvedBranch}, true
}

// hookRecordServed leaves serve receipts (vitality Phase 1) for the facts an
// emission actually carried. Best-effort like everything else here: recording
// problems go to stderr (bounded to one line), never to stdout, and never
// fail the hook. Only a hash of the trigger text is persisted.
func hookRecordServed(cmd *cobra.Command, cfg Config, target hookTarget, surface, triggerText string, kept []factmerge.Record) {
	if len(kept) == 0 {
		return
	}
	cfg.Deps.RecordServed(cmd.ErrOrStderr(), cmd.Context(), cfg.Options,
		target.brainDir, target.branch, surface, target.repoDir, triggerText, kept)
}

// hookReadFailureTail reads at most the last hookFailureTailBytes of stdin —
// errors cluster at the end of output.
func hookReadFailureTail(r io.Reader) string {
	var tail []byte
	buf := make([]byte, hookFailureTailBytes)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if len(tail)+n > hookFailureTailBytes {
				tail = tail[len(tail)+n-hookFailureTailBytes:]
			}
			tail = append(tail, buf[:n]...)
		}
		if err == io.EOF {
			return string(tail)
		}
		if err != nil {
			return ""
		}
	}
}

// MatchFailure ranks gotcha and closed-negative facts against the failure
// text. Only those two kinds are considered — a failure moment wants traps and
// dead ends, not conventions — and only scores at or above the precision bar
// survive. score is the legacy lexical scorer (Deps.FactLexicalScore);
// hookPostFailureMinScore is calibrated to its scale.
func MatchFailure(facts []factmerge.Record, query string, score func(f factmerge.Record, query string, queryLocus []string) int) []factmerge.Record {
	queryLocus := base.FactLocus(query)
	type scored struct {
		rec factmerge.Record
		n   int
	}
	var hits []scored
	for _, f := range facts {
		if f.Status != factmerge.StatusActive {
			continue
		}
		switch factstore.KindOrInferred(f) {
		case factmerge.KindGotcha, factmerge.KindClosedNegative:
		default:
			continue
		}
		if score := score(f, query, queryLocus); score >= hookPostFailureMinScore {
			hits = append(hits, scored{f, score})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].n != hits[j].n {
			return hits[i].n > hits[j].n
		}
		return hits[i].rec.UpdatedAt.After(hits[j].rec.UpdatedAt)
	})
	out := make([]factmerge.Record, len(hits))
	for i, h := range hits {
		out[i] = h.rec
	}
	return out
}

// Emit writes the budget-capped emission: nothing at all when hits is
// empty (the silent default), else one compact line per fact (or a JSON
// object with --json). At least one fact is emitted when any matched, even if
// it alone exceeds the budget — an empty emission for a real match would be a
// false negative, the worse failure mode. It returns the facts actually
// emitted so the caller can leave serve receipts for exactly those.
// hookKeepWithinBudget is the budget rule itself, shared by every emission
// shape so a new one cannot quietly get a different cap. At least one fact
// survives when any matched, even if it alone exceeds the budget — an empty
// emission for a real match would be a false negative, the worse failure mode.
func hookKeepWithinBudget(hits []factmerge.Record, budget int) []factmerge.Record {
	if len(hits) == 0 {
		return nil
	}
	if budget <= 0 {
		budget = hookDefaultBudgetTokens
	}
	kept := make([]factmerge.Record, 0, len(hits))
	spent := 0
	for _, f := range hits {
		cost := estimateTokens([]factmerge.Record{f})
		if len(kept) > 0 && spent+cost > budget {
			break
		}
		kept = append(kept, f)
		spent += cost
	}
	return kept
}

// estimateTokens is a deterministic ~4-chars-per-token estimate over the text
// an agent would actually load (fact text plus its paths). It is a relative
// measure for comparing retrieval configurations, not an exact tokenizer.
func estimateTokens(facts []factmerge.Record) int {
	chars := 0
	for _, f := range facts {
		chars += len(f.Text) + len(strings.Join(f.Paths, ",")) + 4 // small per-fact framing
	}
	return (chars + 3) / 4
}

func Emit(cmd *cobra.Command, hits []factmerge.Record, budget int, jsonOut bool) ([]factmerge.Record, error) {
	kept := hookKeepWithinBudget(hits, budget)
	if len(kept) == 0 {
		return nil, nil
	}
	if jsonOut {
		type hookFact struct {
			ID    string   `json:"id"`
			Kind  string   `json:"kind"`
			Text  string   `json:"text"`
			Paths []string `json:"paths,omitempty"`
		}
		out := struct {
			Facts []hookFact `json:"facts"`
		}{Facts: make([]hookFact, 0, len(kept))}
		for _, f := range kept {
			out.Facts = append(out.Facts, hookFact{ID: f.ID, Kind: factstore.KindOrInferred(f), Text: f.Text, Paths: f.Paths})
		}
		return kept, cmdutil.WriteJSON(cmd, out)
	}
	return kept, cmdutil.WriteText(cmd, func(out io.Writer) {
		for _, f := range kept {
			fmt.Fprintf(out, "[%s] %s\n", factstore.KindOrInferred(f), f.Text)
		}
	})
}

// A Claude Code PreToolUse hook receives its payload as JSON on STDIN. It does
// NOT receive a CLAUDE_FILE_PATH environment variable; no such variable exists.
// Measured against Claude Code 2.1.288 by wiring a hook that dumped its stdin
// and environment:
//
//	stdin: {"hook_event_name":"PreToolUse","tool_name":"Write",
//	        "tool_input":{"file_path":"/abs/path","content":"…"},
//	        "cwd":"…","session_id":"…","tool_use_id":"…"}
//	env:   CLAUDE_PROJECT_DIR, CLAUDE_PID, CLAUDE_EFFORT, CLAUDE_TRANSCRIPT_PATH,
//	       CLAUDE_CODE_ENTRYPOINT, … and no CLAUDE_FILE_PATH
//
// The first wiring of this hook passed `--file "$CLAUDE_FILE_PATH"`, which
// expanded to the empty string on every edit, so the hook errored out every
// time and served nothing. It was a no-op that looked installed.
type agentHookPayload struct {
	HookEventName string `json:"hook_event_name"`
	ToolName      string `json:"tool_name"`
	ToolInput     struct {
		FilePath string `json:"file_path"`
		// NotebookEdit uses notebook_path rather than file_path.
		NotebookPath string `json:"notebook_path"`
	} `json:"tool_input"`
}

// hookStdinWouldBlock reports whether reading this reader would wait on a
// person: it is the process's real stdin, and that stdin is a terminal.
//
// A harness always redirects stdin, so this is false for every real hook
// invocation and true only for someone running the verb by hand.
func hookStdinWouldBlock(in io.Reader) bool {
	return in == os.Stdin && base.StdinIsTerminal()
}

// hookFileFromStdin reads a harness hook payload and returns the path of the
// file about to be edited.
//
// Every failure is a silent empty string, never an error. The hook's contract
// is that a data problem is silence: a hook that fails breaks the harness it is
// wired into, and an edit must never be blocked because the brain could not
// parse something. The caller decides whether an empty result is fatal, which
// keeps `--file` hand-testing debuggable.
func hookFileFromStdin(in io.Reader) (path string, sawInput bool) {
	if in == nil {
		return "", false
	}
	// A hook payload is small; cap the read so a non-hook stdin (a piped file,
	// a terminal left open) cannot hang or balloon.
	const maxPayloadBytes = 1 << 20
	data, err := io.ReadAll(io.LimitReader(in, maxPayloadBytes+1))
	if len(data) > maxPayloadBytes {
		return "", true
	}
	if err != nil || len(strings.TrimSpace(string(data))) == 0 {
		return "", false
	}
	// sawInput is true from here on, and that distinction is the whole point.
	// Something INVOKED us with a payload. If we cannot make sense of it, the
	// answer is silence and exit 0, never an error: this runs as a PreToolUse
	// hook on every edit, and a hook that reports an error on every edit is a
	// hook that gets deleted from the settings within a day -- the same
	// doctrine that makes an empty fact set silent rather than loud.
	//
	// An error is reserved for the case where NOTHING supplied a path: no
	// stdin and no --file. That is a human hand-testing the verb wrong, and
	// they need to be told.
	var payload agentHookPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return "", true
	}
	if p := strings.TrimSpace(payload.ToolInput.FilePath); p != "" {
		return p, true
	}
	return strings.TrimSpace(payload.ToolInput.NotebookPath), true
}
