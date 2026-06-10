package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// hook_cmd.go is Phase 2 item 2 (agent-utility plan): moment-of-relevance
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

func newHookCommand(opts Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hook",
		Short: "Agent-harness hook surface: surface relevant facts at the moment of action",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newHookPreEditCommand(opts))
	cmd.AddCommand(newHookPostFailureCommand(opts))
	return cmd
}

func newHookPreEditCommand(opts Options) *cobra.Command {
	var (
		file    string
		branch  string
		budget  int
		jsonOut bool
	)
	cmd := &cobra.Command{
		Use:   "pre-edit --file <path>",
		Short: "Facts locus-anchored to a file about to be edited (silent when none)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(file) == "" {
				return fmt.Errorf("--file is required")
			}
			facts, ok := hookLoadFacts(cmd, opts, branch)
			if !ok {
				return nil // no brain / no facts: silence, never an error
			}
			rel := filepath.ToSlash(file)
			stem := strings.TrimSuffix(filepath.Base(rel), filepath.Ext(rel))
			// The stem ("distill_cmd") matches facts that name the file without
			// its extension or talk about its symbols' shared prefix.
			hits := factsRelevantToChange([]string{rel, stem}, nil, facts, 8)
			return hookEmit(cmd, hits, budget, jsonOut)
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "Path of the file about to be edited (repo-relative or absolute)")
	cmd.Flags().StringVar(&branch, "branch", "", "Branch whose facts to consult (default: current)")
	cmd.Flags().IntVar(&budget, "budget", hookDefaultBudgetTokens, "Token budget for the emission")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit JSON instead of text lines")
	return cmd
}

func newHookPostFailureCommand(opts Options) *cobra.Command {
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
			facts, ok := hookLoadFacts(cmd, opts, branch)
			if !ok {
				return nil
			}
			hits := hookMatchFailure(facts, query)
			return hookEmit(cmd, hits, budget, jsonOut)
		},
	}
	cmd.Flags().StringVar(&command, "command", "", "The command that failed (added to the match query)")
	cmd.Flags().StringVar(&branch, "branch", "", "Branch whose facts to consult (default: current)")
	cmd.Flags().IntVar(&budget, "budget", hookDefaultBudgetTokens, "Token budget for the emission")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit JSON instead of text lines")
	return cmd
}

// hookLoadFacts resolves the brain and loads the branch's facts, reporting
// ok=false (silence) on any problem — the hook contract.
func hookLoadFacts(cmd *cobra.Command, opts Options, branch string) ([]factRecord, bool) {
	_, brainDir, resolvedBranch, err := resolveFactsTarget(cmd.Context(), opts, agentSurfaceTarget(opts, nil), branch)
	if err != nil {
		return nil, false
	}
	facts, err := loadFacts(brainDir, resolvedBranch)
	if err != nil || len(facts) == 0 {
		return nil, false
	}
	return facts, true
}

// hookReadFailureTail reads at most the last hookFailureTailBytes of stdin —
// errors cluster at the end of output.
func hookReadFailureTail(r io.Reader) string {
	data, err := io.ReadAll(io.LimitReader(r, 1<<20)) // absolute cap: 1 MiB
	if err != nil {
		return ""
	}
	if len(data) > hookFailureTailBytes {
		data = data[len(data)-hookFailureTailBytes:]
	}
	return string(data)
}

// hookMatchFailure ranks gotcha and closed-negative facts against the failure
// text. Only those two kinds are considered — a failure moment wants traps and
// dead ends, not conventions — and only scores at or above the precision bar
// survive.
func hookMatchFailure(facts []factRecord, query string) []factRecord {
	queryLocus := factLocus(query)
	type scored struct {
		rec factRecord
		n   int
	}
	var hits []scored
	for _, f := range facts {
		if f.Status != factStatusActive {
			continue
		}
		switch factKindOrInferred(f) {
		case factKindGotcha, factKindClosedNegative:
		default:
			continue
		}
		if score := factLexicalScore(f, query, queryLocus); score >= hookPostFailureMinScore {
			hits = append(hits, scored{f, score})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].n != hits[j].n {
			return hits[i].n > hits[j].n
		}
		return hits[i].rec.UpdatedAt.After(hits[j].rec.UpdatedAt)
	})
	out := make([]factRecord, len(hits))
	for i, h := range hits {
		out[i] = h.rec
	}
	return out
}

// hookEmit writes the budget-capped emission: nothing at all when hits is
// empty (the silent default), else one compact line per fact (or a JSON
// object with --json). At least one fact is emitted when any matched, even if
// it alone exceeds the budget — an empty emission for a real match would be a
// false negative, the worse failure mode.
func hookEmit(cmd *cobra.Command, hits []factRecord, budget int, jsonOut bool) error {
	if len(hits) == 0 {
		return nil
	}
	if budget <= 0 {
		budget = hookDefaultBudgetTokens
	}
	kept := make([]factRecord, 0, len(hits))
	spent := 0
	for _, f := range hits {
		cost := estimateTokens([]factRecord{f})
		if len(kept) > 0 && spent+cost > budget {
			break
		}
		kept = append(kept, f)
		spent += cost
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
			out.Facts = append(out.Facts, hookFact{ID: f.ID, Kind: factKindOrInferred(f), Text: f.Text, Paths: f.Paths})
		}
		return writeJSON(cmd, out)
	}
	w := cmd.OutOrStdout()
	for _, f := range kept {
		fmt.Fprintf(w, "[%s] %s\n", factKindOrInferred(f), f.Text)
	}
	return nil
}
