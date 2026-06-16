package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

// patterns command surface (Pattern Consolidation, Phase 1).
//
// Public, job-oriented commands per the plan: `patterns refresh` rebuilds the
// derived layer, `patterns status` reports freshness and counts. The bare
// `patterns` command is a read-only default — in Phase 1 it shows status; the
// strongest-patterns listing arrives with procedure detection (Phase 2).

func newPatternsCommand(opts Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "patterns [path]",
		Short:   "Inspect and rebuild repeated-work patterns derived from session history",
		GroupID: "create",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPatternsStatus(cmd.Context(), cmd, opts, targetFromArgs(opts, args), false)
		},
	}
	cmd.AddCommand(newPatternsRefreshCommand(opts))
	cmd.AddCommand(newPatternsStatusCommand(opts))
	return cmd
}

func newPatternsRefreshCommand(opts Options) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "refresh [path]",
		Short: "Rebuild the pattern layer (episodes) from current session history",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPatternsRefresh(cmd.Context(), cmd, opts, targetFromArgs(opts, args), asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit the patterns source summary as JSON")
	return cmd
}

func newPatternsStatusCommand(opts Options) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status [path]",
		Short: "Show pattern layer freshness and counts",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPatternsStatus(cmd.Context(), cmd, opts, targetFromArgs(opts, args), asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit the status report as JSON")
	return cmd
}

// targetFromArgs resolves the repo path argument, defaulting to the env repo root
// then the working directory — the same precedence the other brain commands use.
func targetFromArgs(opts Options, args []string) string {
	if len(args) == 1 {
		return args[0]
	}
	if opts.Env.RepoRoot != "" {
		return opts.Env.RepoRoot
	}
	return "."
}

func resolvePatternsBrainDir(ctx context.Context, opts Options, target string) (string, error) {
	repoDir, local, err := resolveLocalTargetRepoDir(ctx, opts.Runner, target)
	if err != nil {
		return "", err
	}
	if !local {
		return "", fmt.Errorf("patterns require a local repository path: %s", target)
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return "", err
	}
	return storage.BrainDir, nil
}

func runPatternsRefresh(ctx context.Context, cmd *cobra.Command, opts Options, target string, asJSON bool) error {
	brainDir, err := resolvePatternsBrainDir(ctx, opts, target)
	if err != nil {
		return err
	}
	source, err := writeBrainEpisodesAndSource(brainDir, opts.Now().UTC())
	if err != nil {
		return err
	}
	if asJSON {
		return writeJSON(cmd, source)
	}
	r := source.Reinforcement
	fmt.Fprintf(cmd.OutOrStdout(), "patterns: rebuilt %d episode(s) (success %d, corrected %d, neutral %d)\n",
		source.Episodes, r.Success, r.Corrected, r.Neutral)
	for _, w := range source.Warnings {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", w)
	}
	return nil
}

// patternsStatusReport is the JSON contract for `patterns status`.
type patternsStatusReport struct {
	Present       bool                 `json:"present"`
	Freshness     string               `json:"freshness"` // current | stale | missing
	Episodes      int                  `json:"episodes"`
	Reinforcement *reinforcementCounts `json:"reinforcement,omitempty"`
	Procedures    int                  `json:"procedures"`
	Practices     int                  `json:"practices"`
	Patterns      int                  `json:"patterns"`
	SkillMemory   int                  `json:"skill_memory"`
}

func runPatternsStatus(ctx context.Context, cmd *cobra.Command, opts Options, target string, asJSON bool) error {
	brainDir, err := resolvePatternsBrainDir(ctx, opts, target)
	if err != nil {
		return err
	}
	report := buildPatternsStatusReport(brainDir)
	if asJSON {
		return writeJSON(cmd, report)
	}
	if !report.Present {
		fmt.Fprintln(cmd.OutOrStdout(), "patterns: not built (run `entire brain patterns refresh`)")
		return nil
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "patterns: %s\n", report.Freshness)
	fmt.Fprintf(out, "episodes: %d\n", report.Episodes)
	if r := report.Reinforcement; r != nil {
		fmt.Fprintf(out, "reinforcement: %d success, %d corrected, %d neutral\n", r.Success, r.Corrected, r.Neutral)
	}
	fmt.Fprintf(out, "procedures: %d\n", report.Procedures)
	fmt.Fprintf(out, "practices: %d\n", report.Practices)
	return nil
}

// buildPatternsStatusReport reads the patterns source from the manifest and
// derives freshness from the sessions fingerprint, the same input-derived signal
// the history source uses — no re-extraction required.
func buildPatternsStatusReport(brainDir string) patternsStatusReport {
	manifest, err := loadBrainManifest(brainDir)
	if err != nil || manifest.Sources == nil || manifest.Sources.Patterns == nil {
		return patternsStatusReport{Present: false, Freshness: "missing"}
	}
	src := manifest.Sources.Patterns
	freshness := "current"
	if src.SessionsFingerprint != brainSessionsFingerprint(brainDir) {
		freshness = "stale"
	}
	rc := src.Reinforcement
	return patternsStatusReport{
		Present:       true,
		Freshness:     freshness,
		Episodes:      src.Episodes,
		Reinforcement: &rc,
		Procedures:    src.Procedures,
		Practices:     src.Practices,
		Patterns:      src.Patterns,
		SkillMemory:   src.SkillMemory,
	}
}
