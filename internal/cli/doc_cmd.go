package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

func newInspectDocsCommand(opts Options) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "docs <query>",
		Short: "Search the brain's indexed markdown (seed summaries + copied repo docs)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDocInspect(cmd.Context(), cmd, opts, args[0], jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	return cmd
}

type docInspectMatch struct {
	Path    string `json:"path"`
	Heading string `json:"heading,omitempty"`
	Line    int    `json:"line"`
	Score   int    `json:"score"`
	Excerpt string `json:"excerpt"`
}

type docInspectReport struct {
	Query     string            `json:"query"`
	BrainPath string            `json:"brain_path"`
	Records   int               `json:"records"`
	Matches   []docInspectMatch `json:"matches"`
}

func runDocInspect(ctx context.Context, cmd *cobra.Command, opts Options, query string, jsonOut bool) error {
	target := agentSurfaceTarget(opts, nil)
	repoDir, local, err := resolveLocalTargetRepoDir(ctx, opts.Runner, target)
	if err != nil {
		return err
	}
	if !local {
		return fmt.Errorf("inspect docs requires a local repository path: %s", target)
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return err
	}
	index, _ := loadDocIndex(storage.BrainDir)
	report := docInspectReport{Query: query, BrainPath: storage.BrainDir, Records: len(index.Records), Matches: []docInspectMatch{}}
	if scored, ok := rankDocsViaFTS(storage.BrainDir, index, query, 25); ok {
		for _, s := range scored {
			ex := normalizeHistoryTextForExcerpt(s.Record.Text)
			if len(ex) > 300 {
				ex = ex[:300]
			}
			report.Matches = append(report.Matches, docInspectMatch{
				Path:    s.Record.Path,
				Heading: s.Record.Heading,
				Line:    s.Record.Line,
				Score:   s.Score,
				Excerpt: ex,
			})
		}
	}
	if jsonOut {
		return writeJSON(cmd, report)
	}
	for _, m := range report.Matches {
		fmt.Fprintf(cmd.OutOrStdout(), "%s:%d [%s]: %s\n", m.Path, m.Line, m.Heading, m.Excerpt)
	}
	if len(report.Matches) == 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "no doc matches for %q (%d doc chunks indexed)\n", query, report.Records)
	}
	return nil
}
