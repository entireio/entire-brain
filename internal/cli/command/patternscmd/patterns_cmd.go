package patternscmd

import (
	"context"
	"fmt"
	"os"
	"sort"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/cmdutil"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
	patterncore "github.com/entireio/entire-brain/internal/cli/patterns"
	"github.com/spf13/cobra"
)

// patterns command surface (Pattern Consolidation, Phase 1).
//
// Public, job-oriented commands per the plan: `patterns refresh` rebuilds the
// derived layer, `patterns status` reports freshness and counts. The bare
// `patterns` command is a read-only default — in Phase 1 it shows status; the
// strongest-patterns listing arrives with procedure detection (Phase 2).

func New(cfg Config) *cobra.Command {
	opts := cfg.Options
	var listOpts ListOptions
	cmd := &cobra.Command{
		Use:     "patterns [path]",
		Short:   "Inspect and rebuild recurring patterns from sessions",
		Long:    "Inspect and rebuild repeated-work patterns derived from session history",
		GroupID: "create",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPatternsList(cmd.Context(), cmd, cfg, cfg.Deps.AgentSurfaceTarget(opts, args), listOpts)
		},
	}
	cmd.Flags().BoolVar(&listOpts.AsJSON, "json", false, "Emit patterns as JSON")
	cmd.Flags().IntVar(&listOpts.Limit, "limit", 20, "Maximum number of patterns to show")
	cmd.Flags().StringVar(&listOpts.Type, "type", "", "Filter by type: task|procedure|risk|practice|theme")
	cmd.Flags().StringVar(&listOpts.Scope, "scope", "", "Filter by scope: repo|workspace")
	cmd.AddCommand(newPatternsRefreshCommand(cfg))
	cmd.AddCommand(newPatternsStatusCommand(cfg))
	cmd.AddCommand(cfg.Deps.VerifyCommand(opts))
	cmd.AddCommand(cfg.Deps.SkillsCommand(opts))
	return cmd
}

// ListOptions are the `patterns` listing flags, shared with the cli-side
// workspace patterns listing.
type ListOptions struct {
	AsJSON bool
	Limit  int
	Type   string
	Scope  string
}

func runPatternsList(ctx context.Context, cmd *cobra.Command, cfg Config, target string, listOpts ListOptions) error {
	_, brainDir, err := cfg.Deps.ResolveRepoAndBrain(ctx, cfg.Options, target)
	if err != nil {
		return err
	}
	privacyPolicy, err := brainstore.CaptureRetrievalPrivacyPolicy(brainDir)
	if err != nil {
		return err
	}
	privacyPolicy.RequireDerivedClean = true
	if err := cfg.Deps.RequireDerivedCleanRead(brainDir); err != nil {
		return err
	}
	// Primary analytical source is the V2 corpus; the legacy procedure/practice
	// JSON views remain only as a fallback when no corpus has been built yet.
	views, _, corpusBacked, err := patterncore.LoadCorpusPatternViewsChecked(brainDir)
	if err != nil {
		return err
	}
	if !corpusBacked {
		var verr error
		if views, _, verr = patterncore.LoadPatternViews(brainDir); verr != nil {
			return verr
		}
	} else {
		// Themes are a verifier-gated channel: only verified-accepted themes
		// surface (matching brief/overview and the stated contract). Unverified
		// candidates are processed by `patterns verify --themes`, not listed here.
		themes, err := patterncore.LoadThemeViewsChecked(brainDir, true)
		if err != nil {
			return err
		}
		for _, th := range themes {
			views = append(views, patterncore.ThemePatternView(th))
		}
	}
	memoryRecords, err := patterncore.LoadBrainSkillMemory(brainDir)
	if err != nil {
		return err
	}
	memIdx := patterncore.SkillMemoryByPatternID(memoryRecords)

	filtered := views[:0:0]
	for _, v := range views {
		if listOpts.Type != "" && v.Type != listOpts.Type {
			continue
		}
		if listOpts.Scope != "" && v.Scope != listOpts.Scope {
			continue
		}
		// Skill-memory recommendation: suppress already-formed-and-current and
		// declined-and-unchanged patterns; annotate the rest that have a decision.
		if rec, ok := memIdx[v.ID]; ok {
			eval := patterncore.EvaluateSkillMemory(rec, &v)
			if eval.SuppressInListing() {
				continue
			}
			v.SkillStatus = eval.Status + "/" + eval.Sub
			v.Note = eval.Annotation()
		}
		filtered = append(filtered, v)
	}
	sort.SliceStable(filtered, func(i, j int) bool { return filtered[i].Strength > filtered[j].Strength })
	if listOpts.Limit > 0 && len(filtered) > listOpts.Limit {
		filtered = filtered[:listOpts.Limit]
	}
	patternsPresent := true
	if len(filtered) == 0 {
		status, err := BuildStatusReportChecked(brainDir, cfg.Deps)
		if err != nil {
			return err
		}
		patternsPresent = status.Present
	}

	return cfg.Deps.BufferRetrievalCommandOutput(cmd, []brainstore.RetrievalPrivacyPolicy{privacyPolicy}, func() error {
		if listOpts.AsJSON {
			return cmdutil.WriteJSON(cmd, filtered)
		}
		out := cmd.OutOrStdout()
		if len(filtered) == 0 {
			if !patternsPresent {
				fmt.Fprintln(out, "patterns: not built (run `"+base.BrainInvocation(os.LookupEnv, "patterns refresh")+"`)")
			} else {
				fmt.Fprintln(out, "patterns: none detected yet")
			}
			return nil
		}
		for _, v := range filtered {
			patterncore.RenderPatternView(out, v)
		}
		return nil
	})
}

func newPatternsRefreshCommand(cfg Config) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "refresh [path]",
		Short: "Rebuild the pattern layer (episodes) from current session history",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPatternsRefresh(cmd.Context(), cmd, cfg, cfg.Deps.AgentSurfaceTarget(cfg.Options, args), asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit the patterns source summary as JSON")
	return cmd
}

func newPatternsStatusCommand(cfg Config) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status [path]",
		Short: "Show pattern layer freshness and counts",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPatternsStatus(cmd.Context(), cmd, cfg, cfg.Deps.AgentSurfaceTarget(cfg.Options, args), asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit the status report as JSON")
	return cmd
}

func runPatternsRefresh(ctx context.Context, cmd *cobra.Command, cfg Config, target string, asJSON bool) error {
	repoDir, brainDir, err := cfg.Deps.ResolveRepoAndBrain(ctx, cfg.Options, target)
	if err != nil {
		return err
	}
	source, err := cfg.Deps.RefreshPatternLayer(ctx, cfg.Options, repoDir, brainDir)
	if err != nil {
		return err
	}
	if asJSON {
		return cmdutil.WriteJSON(cmd, source)
	}
	r := source.Reinforcement
	fmt.Fprintf(cmd.OutOrStdout(), "patterns: rebuilt %d episode(s) (success %d, corrected %d, neutral %d)\n",
		source.Episodes, r.Success, r.Corrected, r.Neutral)
	for _, w := range source.Warnings {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", w)
	}
	return nil
}

// StatusReport is the JSON contract for `patterns status`.
type StatusReport struct {
	Present          bool                             `json:"present"`
	Freshness        string                           `json:"freshness"` // current | stale | missing
	Episodes         int                              `json:"episodes"`
	Reinforcement    *patterncore.ReinforcementCounts `json:"reinforcement,omitempty"`
	Procedures       int                              `json:"procedures"`
	Practices        int                              `json:"practices"`
	Patterns         int                              `json:"patterns"`
	AcceptedSkills   int                              `json:"accepted_skills"`
	DeclinedPatterns int                              `json:"declined_patterns"`
	UpdatesAvailable int                              `json:"updates_available"`
	LastRun          *patterncore.Run                 `json:"last_run,omitempty"` // what the last corpus build produced
}

func runPatternsStatus(ctx context.Context, cmd *cobra.Command, cfg Config, target string, asJSON bool) error {
	_, brainDir, err := cfg.Deps.ResolveRepoAndBrain(ctx, cfg.Options, target)
	if err != nil {
		return err
	}
	privacyPolicy, err := brainstore.CaptureRetrievalPrivacyPolicy(brainDir)
	if err != nil {
		return err
	}
	privacyPolicy.RequireDerivedClean = true
	if err := cfg.Deps.RequireDerivedCleanRead(brainDir); err != nil {
		return err
	}
	report, err := BuildStatusReportChecked(brainDir, cfg.Deps)
	if err != nil {
		return err
	}
	return cfg.Deps.BufferRetrievalCommandOutput(cmd, []brainstore.RetrievalPrivacyPolicy{privacyPolicy}, func() error {
		if asJSON {
			return cmdutil.WriteJSON(cmd, report)
		}
		if !report.Present {
			fmt.Fprintln(cmd.OutOrStdout(), "patterns: not built (run `"+base.BrainInvocation(os.LookupEnv, "patterns refresh")+"`)")
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
		fmt.Fprintf(out, "accepted skills: %d\n", report.AcceptedSkills)
		fmt.Fprintf(out, "declined patterns: %d\n", report.DeclinedPatterns)
		fmt.Fprintf(out, "updates available: %d\n", report.UpdatesAvailable)
		if lr := report.LastRun; lr != nil {
			fmt.Fprintf(out, "last run: %s — %d episode(s), %d pattern(s), %d dossier(s), %d symbol link(s), %d commit(s), %d synapse(s)\n",
				lr.At, lr.Episodes, lr.Patterns, lr.Dossiers, lr.SymbolLinks, lr.Commits, lr.Synapses)
		}
		return nil
	})
}

// BuildStatusReport reads the patterns source from the manifest and derives
// freshness from its recorded inputs — the sessions fingerprint and the
// tombstone epoch — via Deps.PatternSourceCurrent, the same predicate the
// refresh trigger uses, so no re-extraction is required and the two cannot
// disagree. Skill-memory counts are computed by evaluating each user decision
// against the current pattern set.
func BuildStatusReport(brainDir string, deps Deps) StatusReport {
	report, _ := BuildStatusReportChecked(brainDir, deps)
	return report
}

func BuildStatusReportChecked(brainDir string, deps Deps) (StatusReport, error) {
	manifest, err := bmanifest.Load(brainDir)
	if err != nil {
		return StatusReport{}, err
	}
	lastRun, hasLastRun, err := patterncore.LastRunChecked(brainDir)
	if err != nil {
		return StatusReport{}, err
	}
	_, byID, err := patterncore.LoadPatternViews(brainDir)
	if err != nil {
		return StatusReport{}, err
	}
	memoryRecords, err := patterncore.LoadBrainSkillMemory(brainDir)
	if err != nil {
		return StatusReport{}, err
	}
	if manifest.Sources == nil || manifest.Sources.Patterns == nil {
		return StatusReport{Present: false, Freshness: "missing"}, nil
	}
	src := manifest.Sources.Patterns
	freshness := "current"
	if !deps.PatternSourceCurrent(src, brainDir) {
		freshness = "stale"
	}
	rc := src.Reinforcement
	report := StatusReport{
		Present:       true,
		Freshness:     freshness,
		Episodes:      src.Episodes,
		Reinforcement: &rc,
		Procedures:    src.Procedures,
		Practices:     src.Practices,
		Patterns:      src.Patterns,
	}
	if hasLastRun {
		report.LastRun = &lastRun
	}

	for _, rec := range memoryRecords {
		var cur *patterncore.PatternView
		if v, ok := byID[rec.PatternID]; ok {
			cur = &v
		}
		eval := patterncore.EvaluateSkillMemory(rec, cur)
		if eval.Status == patterncore.SkillStatusDeclined {
			report.DeclinedPatterns++
		} else {
			report.AcceptedSkills++
		}
		if eval.NeedsAttention() {
			report.UpdatesAvailable++
		}
	}
	return report, nil
}
