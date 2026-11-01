package cli

import (
	"fmt"
	"strings"

	"github.com/entireio/entire-brain/internal/cli/internal/cmdutil"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	"github.com/spf13/cobra"
)

func newFactsProvenanceCommand(opts Options) *cobra.Command {
	var (
		branch     string
		jsonOut    bool
		depth      int
		session    string
		commit     string
		checkpoint string
	)
	cmd := &cobra.Command{
		Use:   "provenance [memory-id]",
		Short: "Walk a memory's lineage: what it was derived from, what disagrees with it, what replaced it",
		Long: "Walk the provenance graph around a memory, or around a point in the checkpoint stream.\n\n" +
			"Unlike recall, this traverses superseded and retracted memories: \"what replaced this\" " +
			"cannot be answered from the active set alone. It records no serve receipt, so inspecting " +
			"lineage never moves the reuse metrics.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var id string
			if len(args) == 1 {
				id = strings.TrimSpace(args[0])
			}
			return runFactsProvenance(cmd, opts, factsProvenanceOptions{
				branch: branch, jsonOut: jsonOut, depth: depth,
				id: id, session: session, commit: commit, checkpoint: checkpoint,
			})
		},
	}
	cmd.Flags().StringVar(&branch, "branch", "", "Branch to read (default: current branch)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit the graph as JSON")
	cmd.Flags().IntVar(&depth, "depth", 2, "How many memory-to-memory hops to walk")
	cmd.Flags().StringVar(&session, "session", "", "Start from every memory anchored in this session id")
	cmd.Flags().StringVar(&commit, "commit", "", "Start from every memory anchored at this commit (prefix ok)")
	cmd.Flags().StringVar(&checkpoint, "checkpoint", "", "Start from every memory anchored at this checkpoint id")
	return cmd
}

type factsProvenanceOptions struct {
	branch                      string
	jsonOut                     bool
	depth                       int
	id                          string
	session, commit, checkpoint string
}

// selectors returns the reverse-query selectors that were set. More than one
// is refused rather than intersected or unioned: either guess would silently
// answer a different question than the one asked.
func (o factsProvenanceOptions) selectors() []string {
	var set []string
	for _, pair := range []struct{ flag, value string }{
		{"--session", o.session},
		{"--commit", o.commit},
		{"--checkpoint", o.checkpoint},
	} {
		if strings.TrimSpace(pair.value) != "" {
			set = append(set, pair.flag)
		}
	}
	return set
}

func (o factsProvenanceOptions) validate() error {
	selectors := o.selectors()
	switch {
	case o.id != "" && len(selectors) > 0:
		return fmt.Errorf("a memory id and %s cannot both be given: pass the id to walk one memory's lineage, or the selector to start from everything anchored there", selectors[0])
	case o.id == "" && len(selectors) == 0:
		return fmt.Errorf("pass a memory id, or one of --session/--commit/--checkpoint")
	case len(selectors) > 1:
		return fmt.Errorf("%s cannot be combined: each one names a different starting set", strings.Join(selectors, " and "))
	case o.depth < 0:
		return fmt.Errorf("--depth cannot be negative")
	}
	return nil
}

func runFactsProvenance(cmd *cobra.Command, opts Options, provOpts factsProvenanceOptions) error {
	if err := provOpts.validate(); err != nil {
		return err
	}

	_, brainDir, branch, err := resolveFactsTarget(cmd.Context(), opts, agentSurfaceTarget(opts, nil), provOpts.branch)
	if err != nil {
		return err
	}

	records, err := factstore.Load(brainDir, branch)
	if err != nil {
		return err
	}

	var roots []string
	if provOpts.id != "" {
		if indexOfFact(records, provOpts.id) < 0 {
			byBranch, loadErr := factstore.LoadAllBranches(brainDir)
			if loadErr == nil {
				if hint := provenanceBranchHint(byBranch, branch, provOpts.id); hint != "" {
					return fmt.Errorf("%s", hint)
				}
			}
			return fmt.Errorf("no memory %s on %s", provOpts.id, branch)
		}
		roots = []string{provOpts.id}
	} else {
		nodeType, value := provOpts.selector()
		roots = memoriesAnchoredAt(records, nodeType, value)
		if len(roots) == 0 {
			return fmt.Errorf("no memory on %s is anchored at %s %s", branch, nodeType, value)
		}
	}

	graph := buildProvenanceGraph(records, branch, roots, provOpts.depth)

	if provOpts.jsonOut {
		return cmdutil.WriteJSON(cmd, graph)
	}
	var out strings.Builder
	renderProvenanceGraph(&out, graph)
	fmt.Fprint(cmd.OutOrStdout(), out.String())
	return nil
}

// selector returns the node type and value for whichever reverse-query flag
// was set. validate has already established that exactly one is.
func (o factsProvenanceOptions) selector() (string, string) {
	switch {
	case strings.TrimSpace(o.session) != "":
		return provNodeSession, strings.TrimSpace(o.session)
	case strings.TrimSpace(o.commit) != "":
		return provNodeCommit, strings.TrimSpace(o.commit)
	default:
		return provNodeCheckpoint, strings.TrimSpace(o.checkpoint)
	}
}
