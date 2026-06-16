package cli

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// Workspace patterns (Pattern Consolidation, Phase 6).
//
// Workspace patterns are cross-repo: a procedure command-shape or a durable-fact
// practice that recurs across MULTIPLE member repos. Rather than re-derive from
// raw episodes/facts, this merges each member's already-built per-repo patterns
// (procedures.ndjson / practices.ndjson) — no generated brain data is copied
// between repos, repo-specific variants are preserved as a per-repo breakdown,
// and a pattern surfaces at the workspace level only when ≥2 repos share it.
//
// Strength rewards breadth (fraction of members sharing the pattern) on top of
// total support and reinforcement; practices also keep the kind weighting.
// Workspace pattern files live under the workspace dir, a sibling of repos/.

const workspaceMinRepos = 2 // a workspace pattern must be shared across ≥2 repos

func newWorkspacePatternsCommand(opts Options) *cobra.Command {
	var list patternsListOptions
	cmd := &cobra.Command{
		Use:   "patterns <workspace>",
		Short: "Inspect and rebuild cross-repo patterns for a workspace",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return fmt.Errorf("workspace name required")
			}
			return runWorkspacePatternsList(cmd.Context(), cmd, opts, args[0], list)
		},
	}
	cmd.Flags().BoolVar(&list.asJSON, "json", false, "Emit patterns as JSON")
	cmd.Flags().IntVar(&list.limit, "limit", 20, "Maximum number of patterns to show")
	cmd.Flags().StringVar(&list.typ, "type", "", "Filter by type: procedure|practice")

	refresh := &cobra.Command{
		Use:   "refresh <workspace>",
		Short: "Rebuild cross-repo patterns by merging member-repo patterns",
		Args:  cobra.ExactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return runWorkspacePatternsRefresh(cmd.Context(), cmd, opts, args[0]) },
	}
	status := &cobra.Command{
		Use:   "status <workspace>",
		Short: "Show workspace pattern counts and member coverage",
		Args:  cobra.ExactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return runWorkspacePatternsStatus(cmd.Context(), cmd, opts, args[0]) },
	}
	var f formOptions
	form := &cobra.Command{
		Use:   "form <workspace> <pattern-id>",
		Short: "Form a cross-repo pattern into a reusable skill (previews until --yes)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			f.patternID = args[1]
			return runWorkspacePatternsForm(cmd.Context(), cmd, opts, args[0], f)
		},
	}
	form.Flags().StringVar(&f.name, "name", "", "Skill name (required with --yes)")
	form.Flags().StringVar(&f.target, "target", "standard", "Install target: standard|claude-code|codex|factoryai-droid|all")
	form.Flags().StringVar(&f.scope, "scope", "global", "Install scope (workspace skills install global)")
	form.Flags().BoolVar(&f.yes, "yes", false, "Confirm and write the skill files")
	form.Flags().BoolVar(&f.force, "force", false, "Overwrite an existing skill file")
	form.Flags().BoolVar(&f.draftOnly, "draft-only", false, "Print the SKILL.md draft without writing")
	form.Flags().BoolVar(&f.decline, "decline", false, "Record a decline for this pattern")
	form.Flags().BoolVar(&f.asJSON, "json", false, "Emit the result as JSON")

	cmd.AddCommand(refresh, status, form)
	return cmd
}

type memberPatterns struct {
	procsByRepo map[string][]procedureRecord
	pracsByRepo map[string][]practiceRecord
	repoOrder   []string
	warnings    []string
}

func loadMemberPatterns(opts Options, manifest workspaceManifest) (memberPatterns, error) {
	m := memberPatterns{procsByRepo: map[string][]procedureRecord{}, pracsByRepo: map[string][]practiceRecord{}}
	repos := append([]workspaceRepo(nil), manifest.Repos...)
	sort.Slice(repos, func(i, j int) bool { return repos[i].RepoKey < repos[j].RepoKey })
	for _, repo := range repos {
		brainDir, err := brainDirForKey(opts.Env, repo.RepoKey)
		if err != nil {
			m.warnings = append(m.warnings, fmt.Sprintf("%s: %v", repo.RepoKey, err))
			continue
		}
		procs, err := loadBrainProcedures(brainDir)
		if err != nil {
			m.warnings = append(m.warnings, fmt.Sprintf("%s: procedures: %v", repo.RepoKey, err))
		}
		pracs, err := loadBrainPractices(brainDir)
		if err != nil {
			m.warnings = append(m.warnings, fmt.Sprintf("%s: practices: %v", repo.RepoKey, err))
		}
		if len(procs) == 0 && len(pracs) == 0 {
			m.warnings = append(m.warnings, fmt.Sprintf("%s: no patterns (run `entire brain patterns refresh` in that repo)", repo.RepoKey))
		}
		m.procsByRepo[repo.RepoKey] = procs
		m.pracsByRepo[repo.RepoKey] = pracs
		m.repoOrder = append(m.repoOrder, repo.RepoKey)
	}
	return m, nil
}

// buildWorkspaceProcedures merges per-repo procedures by command shape, keeping
// shapes shared by ≥2 repos.
func buildWorkspaceProcedures(byRepo map[string][]procedureRecord, repoOrder []string, memberCount int, workspace string) []procedureRecord {
	type agg struct {
		commands  []string
		repos     map[string]bool
		support   int
		reinf     reinforcementCounts
		breakdown map[string]*patternRepoStat
		examples  []episodeAnchor
	}
	aggs := map[string]*agg{}
	for _, repoKey := range repoOrder {
		for _, p := range byRepo[repoKey] {
			key := strings.Join(p.Commands, "\x00")
			a := aggs[key]
			if a == nil {
				a = &agg{commands: p.Commands, repos: map[string]bool{}, breakdown: map[string]*patternRepoStat{}}
				aggs[key] = a
			}
			a.repos[repoKey] = true
			a.support += p.Support
			a.reinf = addReinf(a.reinf, p.Reinforcement)
			bd := a.breakdown[repoKey]
			if bd == nil {
				bd = &patternRepoStat{RepoKey: repoKey}
				a.breakdown[repoKey] = bd
			}
			bd.Support += p.Support
			bd.Reinforcement = addReinf(bd.Reinforcement, p.Reinforcement)
			if len(a.examples) < 3 && len(p.Examples) > 0 {
				a.examples = append(a.examples, p.Examples[0])
			}
		}
	}

	var out []procedureRecord
	for _, a := range aggs {
		repos := len(a.repos)
		if repos < workspaceMinRepos {
			continue
		}
		strength := workspaceProcedureStrength(repos, memberCount, a.support, a.reinf)
		out = append(out, procedureRecord{
			ID:            procedureID("workspace", workspace, a.commands),
			Type:          "procedure",
			Scope:         "workspace",
			Workspace:     workspace,
			Commands:      a.commands,
			Support:       a.support,
			Repos:         repos,
			RepoBreakdown: sortedBreakdown(a.breakdown),
			Reinforcement: a.reinf,
			Strength:      math.Round(strength*1000) / 1000,
			StrengthLabel: strengthLabel(strength),
			Examples:      a.examples,
		})
	}
	sortPatternsByStrength(out, func(i int) (float64, int, string) {
		return out[i].Strength, out[i].Repos, out[i].ID
	})
	return out
}

// buildWorkspacePractices merges per-repo practices by their (content-stable)
// id, keeping practices shared by ≥2 repos.
func buildWorkspacePractices(byRepo map[string][]practiceRecord, repoOrder []string, memberCount int, workspace string) []practiceRecord {
	type agg struct {
		base      practiceRecord
		repos     map[string]bool
		support   int
		breakdown map[string]*patternRepoStat
	}
	aggs := map[string]*agg{}
	for _, repoKey := range repoOrder {
		for _, p := range byRepo[repoKey] {
			a := aggs[p.ID]
			if a == nil {
				a = &agg{base: p, repos: map[string]bool{}, breakdown: map[string]*patternRepoStat{}}
				aggs[p.ID] = a
			}
			a.repos[repoKey] = true
			a.support += p.Support
			bd := a.breakdown[repoKey]
			if bd == nil {
				bd = &patternRepoStat{RepoKey: repoKey}
				a.breakdown[repoKey] = bd
			}
			bd.Support += p.Support
		}
	}

	var out []practiceRecord
	for _, a := range aggs {
		repos := len(a.repos)
		if repos < workspaceMinRepos {
			continue
		}
		strength := workspacePracticeStrength(a.base.Kind, repos, memberCount, a.support)
		rec := a.base
		rec.Scope = "workspace"
		rec.Workspace = workspace
		rec.RepoKey = ""
		rec.Support = a.support
		rec.Repos = repos
		rec.RepoBreakdown = sortedBreakdown(a.breakdown)
		rec.Strength = math.Round(strength*1000) / 1000
		rec.StrengthLabel = strengthLabel(strength)
		out = append(out, rec)
	}
	sortPatternsByStrength(out, func(i int) (float64, int, string) {
		return out[i].Strength, out[i].Repos, out[i].ID
	})
	return out
}

func workspaceProcedureStrength(repos, memberCount, support int, reinf reinforcementCounts) float64 {
	breadth := repoBreadthScore(repos, memberCount)
	supportScore := clamp01(math.Log2(1+float64(support)) / math.Log2(1+20))
	reinfScore := 0.5
	if support > 0 {
		reinfScore = clamp01(0.5 + 0.5*float64(reinf.Success-reinf.Corrected)/float64(support))
	}
	return 0.45*breadth + 0.30*supportScore + 0.25*reinfScore
}

func workspacePracticeStrength(kind string, repos, memberCount, support int) float64 {
	kindScore := clamp01(float64(factKindPriority[kind]) / 6.0)
	breadth := repoBreadthScore(repos, memberCount)
	supportScore := clamp01(math.Log2(1+float64(support)) / math.Log2(1+10))
	return 0.40*kindScore + 0.35*breadth + 0.25*supportScore
}

// repoBreadthScore is the fraction of OTHER members that also share the pattern:
// shared by all members -> 1.0, shared by 2 of many -> low.
func repoBreadthScore(repos, memberCount int) float64 {
	denom := memberCount - 1
	if denom < 1 {
		denom = 1
	}
	return clamp01(float64(repos-1) / float64(denom))
}

func addReinf(a, b reinforcementCounts) reinforcementCounts {
	return reinforcementCounts{Success: a.Success + b.Success, Corrected: a.Corrected + b.Corrected, Neutral: a.Neutral + b.Neutral}
}

func sortedBreakdown(m map[string]*patternRepoStat) []patternRepoStat {
	out := make([]patternRepoStat, 0, len(m))
	for _, v := range m {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RepoKey < out[j].RepoKey })
	return out
}

func sortPatternsByStrength[T any](items []T, key func(i int) (float64, int, string)) {
	sort.SliceStable(items, func(i, j int) bool {
		si, ri, idi := key(i)
		sj, rj, idj := key(j)
		if si != sj {
			return si > sj
		}
		if ri != rj {
			return ri > rj
		}
		return idi < idj
	})
}

func runWorkspacePatternsRefresh(ctx context.Context, cmd *cobra.Command, opts Options, name string) error {
	manifest, err := loadWorkspaceManifest(opts.Env, name)
	if err != nil {
		return err
	}
	members, err := loadMemberPatterns(opts, manifest)
	if err != nil {
		return err
	}
	wsDir, err := workspaceDir(opts.Env, name)
	if err != nil {
		return err
	}
	procs := buildWorkspaceProcedures(members.procsByRepo, members.repoOrder, len(manifest.Repos), name)
	pracs := buildWorkspacePractices(members.pracsByRepo, members.repoOrder, len(manifest.Repos), name)
	if err := writeBrainProceduresFile(wsDir, procs); err != nil {
		return err
	}
	if err := writeBrainPracticesFile(wsDir, pracs); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "workspace %s: %d cross-repo procedure(s), %d cross-repo practice(s) across %d member repo(s)\n",
		name, len(procs), len(pracs), len(manifest.Repos))
	for _, w := range members.warnings {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", w)
	}
	return nil
}

func runWorkspacePatternsList(ctx context.Context, cmd *cobra.Command, opts Options, name string, list patternsListOptions) error {
	wsDir, err := workspaceDir(opts.Env, name)
	if err != nil {
		return err
	}
	views, _, err := loadPatternViews(wsDir)
	if err != nil {
		return err
	}
	filtered := views[:0:0]
	for _, v := range views {
		if list.typ != "" && v.Type != list.typ {
			continue
		}
		filtered = append(filtered, v)
	}
	sort.SliceStable(filtered, func(i, j int) bool { return filtered[i].Strength > filtered[j].Strength })
	if list.limit > 0 && len(filtered) > list.limit {
		filtered = filtered[:list.limit]
	}
	if list.asJSON {
		return writeJSON(cmd, filtered)
	}
	out := cmd.OutOrStdout()
	if len(filtered) == 0 {
		fmt.Fprintf(out, "workspace %s: no cross-repo patterns (run `entire brain workspace patterns refresh %s`)\n", name, name)
		return nil
	}
	for _, v := range filtered {
		renderPatternView(out, v)
	}
	return nil
}

func runWorkspacePatternsStatus(ctx context.Context, cmd *cobra.Command, opts Options, name string) error {
	manifest, err := loadWorkspaceManifest(opts.Env, name)
	if err != nil {
		return err
	}
	wsDir, err := workspaceDir(opts.Env, name)
	if err != nil {
		return err
	}
	procs, _ := loadBrainProcedures(wsDir)
	pracs, _ := loadBrainPractices(wsDir)
	members, _ := loadMemberPatterns(opts, manifest)
	covered := 0
	for _, repoKey := range members.repoOrder {
		if len(members.procsByRepo[repoKey]) > 0 || len(members.pracsByRepo[repoKey]) > 0 {
			covered++
		}
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "workspace: %s\n", name)
	fmt.Fprintf(out, "member repos: %d (%d with patterns)\n", len(manifest.Repos), covered)
	fmt.Fprintf(out, "cross-repo procedures: %d\n", len(procs))
	fmt.Fprintf(out, "cross-repo practices: %d\n", len(pracs))
	for _, w := range members.warnings {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", w)
	}
	return nil
}

func runWorkspacePatternsForm(ctx context.Context, cmd *cobra.Command, opts Options, name string, f formOptions) error {
	if f.scope == "repo" {
		return fmt.Errorf("workspace skills install with --scope global (a workspace pattern spans repos)")
	}
	wsDir, err := workspaceDir(opts.Env, name)
	if err != nil {
		return err
	}
	_, byID, err := loadPatternViews(wsDir)
	if err != nil {
		return err
	}
	view, ok := byID[f.patternID]
	if !ok {
		return fmt.Errorf("workspace pattern not found: %s (run `entire brain workspace patterns %s`)", f.patternID, name)
	}
	// storeDir = workspace dir (skill-memory lives with the workspace); repoDir
	// empty (global install only).
	return formFromView(cmd, view, wsDir, "", f, opts.Now().UTC())
}
