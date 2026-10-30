package cli

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/entireio/entire-brain/internal/cli/command/patternscmd"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/cmdutil"
	patterncore "github.com/entireio/entire-brain/internal/cli/patterns"
	"github.com/spf13/cobra"
)

// patternsCmdConfig binds the patterns command tree's cli-resident seams at the
// assembly boundary. Command packages never import cli, and no mutable hook is
// copied.
func patternsCmdConfig(opts Options) patternscmd.Config {
	return patternscmd.Config{Options: opts, Deps: patternsCmdDeps()}
}

// patternsCmdDeps is separate from patternsCmdConfig because
// patternscmd.BuildStatusReport takes only the seams, not the full command
// config (skill_memory_status_test.go builds reports directly).
func patternsCmdDeps() patternscmd.Deps {
	return patternscmd.Deps{
		AgentSurfaceTarget:           agentSurfaceTarget,
		ResolveRepoAndBrain:          resolvePatternsRepoAndBrain,
		RequireDerivedCleanRead:      requirePrivacyDerivedRead,
		BufferRetrievalCommandOutput: bufferRetrievalCommandOutput,
		RefreshPatternLayer:          refreshPatternLayerForCommand,
		PatternSourceCurrent:         patternSourceCurrent,
		VerifyCommand:                newPatternsVerifyCommand,
		SkillsCommand:                newPatternsSkillsCommand,
	}
}

func resolvePatternsBrainDir(ctx context.Context, opts Options, target string) (string, error) {
	_, brainDir, err := resolvePatternsRepoAndBrain(ctx, opts, target)
	return brainDir, err
}

// resolvePatternsRepoAndBrain resolves both the local repo directory (needed as
// the agent runner's working dir) and the brain storage dir for that repo.
func resolvePatternsRepoAndBrain(ctx context.Context, opts Options, target string) (repoDir, brainDir string, err error) {
	repoDir, local, err := base.ResolveLocalTargetRepoDir(ctx, opts.Runner, target)
	if err != nil {
		return "", "", err
	}
	if !local {
		return "", "", fmt.Errorf("patterns require a local repository path: %s", target)
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return "", "", err
	}
	return repoDir, storage.BrainDir, nil
}

// refreshPatternLayerForCommand rebuilds episodes and publishes the corpus for
// `patterns refresh`, binding the cli-resident episode writer and corpus
// builder behind the patternscmd seam.
func refreshPatternLayerForCommand(ctx context.Context, opts Options, repoDir, brainDir string) (*patternSourceManifest, error) {
	source, err := writeBrainEpisodesAndSource(brainDir, opts.Now().UTC())
	if err != nil {
		return nil, err
	}
	// Explicit refresh is the stricter surface: a corpus failure is fatal here.
	if err := buildPatternCorpus(brainDir, opts.Now().UTC(), patternCorpusGraphContext{Context: ctx, Options: opts, RepoDir: repoDir}); err != nil {
		return nil, fmt.Errorf("pattern corpus: %w", err)
	}
	return source, nil
}

func newPatternsVerifyCommand(opts Options) *cobra.Command {
	var (
		asJSON       bool
		deep         bool
		themes       bool
		lessons      bool
		conventions  bool
		agent, model string
		effort       string
	)
	cmd := &cobra.Command{
		Use:   "verify [path]",
		Short: "Agent-audit promotable consolidation dossiers (explicit, egress-gated, cached)",
		Long: "Run the optional consolidation verifier over promotable dossiers. This is the " +
			"only surface that may invoke an agent for patterns; refresh/watch/brief/query " +
			"never do. Verdicts are cached by evidence fingerprint and re-used until the evidence " +
			"changes. With --deep, each pattern is audited against a bounded evidence-deep export " +
			"of its backing episodes (corrected/failed first; up to 40 anchors, the top 12 carrying " +
			"redacted transcript excerpts, with parameters and recoveries) instead of the sampled " +
			"shallow dossier. With --themes, candidate latent-practice themes " +
			"(recurring read-only/conversational work) are verified before they surface. " +
			"With --lessons, corrected/failed episodes are grouped into non-obvious " +
			"failure→recovery procedure skills. With --conventions, durable gotcha/" +
			"convention/invariant facts are grouped into capability skills.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPatternsVerify(cmd.Context(), cmd, opts, agentSurfaceTarget(opts, args), agent, model, effort, asJSON, deep, themes, lessons, conventions)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit the verification summary as JSON")
	cmd.Flags().BoolVar(&deep, "deep", false, "Audit each pattern against a bounded evidence-deep export of its backing episodes (deep dossier)")
	cmd.Flags().BoolVar(&themes, "themes", false, "Verify candidate latent-practice themes before they surface")
	cmd.Flags().BoolVar(&lessons, "lessons", false, "Group corrected/failed episodes into non-obvious failure→recovery procedure skills")
	cmd.Flags().BoolVar(&conventions, "conventions", false, "Group durable gotcha/convention/invariant facts into capability skills")
	cmd.Flags().StringVar(&agent, "agent", "auto", "Verifier agent: auto, codex, claude-code, ollama, or command")
	cmd.Flags().StringVar(&model, "model", "", "Override the agent model, or select the local Ollama model")
	cmd.Flags().StringVar(&effort, "effort", "", "Agent reasoning effort (codex/claude-code)")
	return cmd
}

func runPatternsVerify(ctx context.Context, cmd *cobra.Command, opts Options, target, agent, model, effort string, asJSON, deep, themes, lessons, conventions bool) error {
	repoDir, brainDir, err := resolvePatternsRepoAndBrain(ctx, opts, target)
	if err != nil {
		return err
	}
	privacyPolicy, err := brainstore.CaptureRetrievalPrivacyPolicy(brainDir)
	if err != nil {
		return err
	}
	privacyPolicy.RequireDerivedClean = true
	if err := requirePrivacyDerivedRead(brainDir); err != nil {
		return err
	}
	if err := base.RejectAgentForNoEgress(agent); err != nil {
		return err
	}
	return runPrivacyLinearizedPatternMutation(cmd, []retrievalPrivacyPolicy{privacyPolicy}, func() error {
		run := defaultDistillAgentRunner(agent)
		verify := patterncore.VerifyDossiers
		mode := "dossier"
		switch {
		case themes:
			verify = verifyThemes
			mode = "theme"
		case lessons:
			verify = proposeSkillLessons
			mode = "lesson"
		case conventions:
			verify = proposeSkillConventions
			mode = "convention"
		case deep:
			verify = verifyDeepDossiers
			mode = "deep dossier"
		}
		var stats patterncore.VerifyStats
		if err := patterncore.WithCorpusMutationLocked(brainDir, func(db *sql.DB) error {
			var verifyErr error
			stats, verifyErr = verify(ctx, db, brainDir, repoDir, agent, model, effort, run, opts.Now().UTC())
			return verifyErr
		}, patterncore.DecodeCorpusPublication); err != nil {
			return err
		}
		if asJSON {
			return cmdutil.WriteJSON(cmd, stats)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "patterns verify: %d %s(s) — %d verified, %d cached, %d failed\n",
			stats.Considered, mode, stats.Verified, stats.Cached, stats.Failed)
		return nil
	})
}

// brainBriefPatternsCount caps how many task-relevant patterns the brief carries.
func brainBriefPatternsCount(limit int) int {
	if limit <= 0 || limit > 5 {
		return 5
	}
	return limit
}

// rankTaskRelevantPatterns returns the patterns whose title/kind share a term
// with the task, ranked by term overlap then strength. Patterns with no overlap
// are dropped — the brief should surface task-relevant patterns, not ambient
// noise — so an unrelated task yields none.
func rankTaskRelevantPatterns(views []patternView, terms []string, limit int) []patternView {
	if limit <= 0 || len(terms) == 0 {
		return nil
	}
	termSet := make(map[string]bool, len(terms))
	for _, t := range terms {
		termSet[t] = true
	}
	type scored struct {
		v       patternView
		overlap int
	}
	var matched []scored
	for _, v := range views {
		seen := map[string]bool{}
		overlap := 0
		for _, w := range brainBriefTaskWordPattern.FindAllString(strings.ToLower(v.Title+" "+v.Kind), -1) {
			if termSet[w] && !seen[w] {
				seen[w] = true
				overlap++
			}
		}
		if overlap > 0 {
			matched = append(matched, scored{v, overlap})
		}
	}
	sort.SliceStable(matched, func(i, j int) bool {
		if matched[i].overlap != matched[j].overlap {
			return matched[i].overlap > matched[j].overlap
		}
		return matched[i].v.Strength > matched[j].v.Strength
	})
	out := make([]patternView, 0, limit)
	for _, m := range matched {
		if len(out) >= limit {
			break
		}
		out = append(out, m.v)
	}
	return out
}

// mustLoadSkillMemory returns skill-memory records, treating a read/parse error
// as empty — the listing should degrade to "no recommendations", not fail.
func mustLoadSkillMemory(brainDir string) []skillMemoryRecord {
	records, err := patterncore.LoadBrainSkillMemory(brainDir)
	if err != nil {
		return nil
	}
	return records
}
