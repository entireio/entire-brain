package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	cmd.Flags().StringVar(&list.typ, "type", "", "Filter by type: task|procedure|risk|practice")
	cmd.Flags().StringVar(&list.scope, "scope", "", "Filter by scope: workspace")

	refresh := &cobra.Command{
		Use:   "refresh <workspace>",
		Short: "Rebuild cross-repo patterns by merging member-repo patterns",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkspacePatternsRefresh(cmd.Context(), cmd, opts, args[0])
		},
	}
	status := &cobra.Command{
		Use:   "status <workspace>",
		Short: "Show workspace pattern counts and member coverage",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkspacePatternsStatus(cmd.Context(), cmd, opts, args[0])
		},
	}
	var v struct {
		agent, model, effort string
		asJSON               bool
	}
	verify := &cobra.Command{
		Use:   "verify <workspace>",
		Short: "Agent-judge cross-repo families: merge equivalent workflows across repos by meaning (explicit, egress-gated, cached)",
		Long: "Add the workspace judgment layer on top of exact aggregation: group member-repo task " +
			"candidates into cross-repo families by semantic trigger + procedure purpose (equivalent workflows " +
			"with different command names/order merge; generic git workflows are rejected). An accepted family " +
			"dossier is required before `workspace patterns skills form`. The only agent path for workspaces; " +
			"egress-gated and cached by the member-candidate fingerprint.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkspacePatternsVerify(cmd.Context(), cmd, opts, args[0], v.agent, v.model, v.effort, v.asJSON)
		},
	}
	verify.Flags().StringVar(&v.agent, "agent", "auto", "Judgment agent: auto, codex, claude-code, ollama, or command")
	verify.Flags().StringVar(&v.model, "model", "", "Override the agent model")
	verify.Flags().StringVar(&v.effort, "effort", "", "Agent reasoning effort")
	verify.Flags().BoolVar(&v.asJSON, "json", false, "Emit the verification summary as JSON")
	skills := newWorkspaceSkillsCommand(opts)

	cmd.AddCommand(refresh, status, verify, skills)
	return cmd
}

func runWorkspacePatternsVerify(ctx context.Context, cmd *cobra.Command, opts Options, name, agent, model, effort string, asJSON bool) error {
	manifest, err := loadWorkspaceManifest(opts.Env, name)
	if err != nil {
		return err
	}
	if err := rejectAgentForNoEgress(agent); err != nil {
		return err
	}
	resolved := agent
	if resolved == "" || resolved == "auto" {
		resolved = defaultRefreshAgent(ctx, opts.Runner, ".")
	}
	if resolved == "none" {
		return fmt.Errorf("workspace family judgment requires an agent (codex or claude-code); none found on PATH")
	}
	stats, err := proposeWorkspaceFamilies(ctx, opts.Env, manifest, ".", resolved, model, effort, defaultDistillAgentRunner(resolved), opts.Now().UTC())
	if err != nil {
		return err
	}
	if asJSON {
		return writeJSON(cmd, stats)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "workspace %s verify: %d family proposal(s) — %d accepted, %d cached, %d dropped\n",
		name, stats.Considered, stats.Verified, stats.Cached, stats.Failed)
	return nil
}

// newWorkspaceSkillsCommand mirrors `patterns skills`: list cross-repo task
// candidates and synthesize a skill from one. This is the only workspace
// skill-creation path; cross-repo procedures/practices are diagnostic only.
func newWorkspaceSkillsCommand(opts Options) *cobra.Command {
	var (
		asJSON bool
		limit  int
	)
	cmd := &cobra.Command{
		Use:   "skills <workspace>",
		Short: "List cross-repo task candidates for a workspace",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkspaceSkillsList(cmd.Context(), cmd, opts, args[0], asJSON, limit)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit task candidates as JSON")
	cmd.Flags().IntVar(&limit, "limit", 20, "Maximum candidates to show")

	var s skillFormOptions
	form := &cobra.Command{
		Use:   "form <workspace> <task-id>",
		Short: "Synthesize a SKILL.md from a cross-repo task candidate (previews until --yes)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			s.taskID = args[1]
			return runWorkspaceSkillsForm(cmd.Context(), cmd, opts, args[0], s)
		},
	}
	form.Flags().StringVar(&s.name, "name", "", "Override the skill name")
	form.Flags().StringVar(&s.target, "target", "standard", "Install target: standard|claude-code|codex|factoryai-droid|all")
	form.Flags().StringVar(&s.agent, "agent", "auto", "Synthesis agent: auto|codex|claude-code")
	form.Flags().StringVar(&s.model, "model", "", "Override the agent model")
	form.Flags().StringVar(&s.effort, "effort", "", "Override the agent reasoning effort")
	form.Flags().BoolVar(&s.yes, "yes", false, "Write the synthesized skill files")
	form.Flags().BoolVar(&s.force, "force", false, "Overwrite an existing skill file")
	form.Flags().BoolVar(&s.draftOnly, "draft-only", false, "Print the synthesized SKILL.md without writing")
	form.Flags().BoolVar(&s.asJSON, "json", false, "Emit the result as JSON")
	// Workspace skills always install global (a workspace pattern spans repos).
	s.scope = "global"
	cmd.AddCommand(form)
	return cmd
}

type memberPatterns struct {
	procsByRepo map[string][]procedureRecord
	pracsByRepo map[string][]practiceRecord
	tasksByRepo map[string][]taskCandidate
	repoOrder   []string
	warnings    []string
}

func loadMemberPatterns(opts Options, manifest workspaceManifest) (memberPatterns, error) {
	m := memberPatterns{procsByRepo: map[string][]procedureRecord{}, pracsByRepo: map[string][]practiceRecord{}, tasksByRepo: map[string][]taskCandidate{}}
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
		tasks, err := loadBrainTasks(brainDir)
		if err != nil {
			m.warnings = append(m.warnings, fmt.Sprintf("%s: tasks: %v", repo.RepoKey, err))
		}
		if len(procs) == 0 && len(pracs) == 0 && len(tasks) == 0 {
			m.warnings = append(m.warnings, fmt.Sprintf("%s: no patterns (run `entire brain refresh` in that repo)", repo.RepoKey))
		}
		m.procsByRepo[repo.RepoKey] = procs
		m.pracsByRepo[repo.RepoKey] = pracs
		m.tasksByRepo[repo.RepoKey] = tasks
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

func hexSHA(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func sortedSetKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedProcedures(m map[string]taskProcedure) []taskProcedure {
	out := make([]taskProcedure, 0, len(m))
	for _, p := range m {
		out = append(out, p)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Specificity != out[j].Specificity {
			return out[i].Specificity > out[j].Specificity
		}
		return strings.Join(out[i].Commands, " ") < strings.Join(out[j].Commands, " ")
	})
	if len(out) > taskMaxProcedures {
		out = out[:taskMaxProcedures]
	}
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
	// Primary V2 source: aggregate member corpora into the workspace corpus.
	counts, err := buildWorkspacePatternCorpus(opts.Env, manifest, opts.Now().UTC())
	if err != nil {
		return err
	}
	// Legacy JSON views remain only so the workspace skills path (loadBrainTasks)
	// keeps working; they are no longer the analytical source for the listing.
	procs := buildWorkspaceProcedures(members.procsByRepo, members.repoOrder, len(manifest.Repos), name)
	pracs := buildWorkspacePractices(members.pracsByRepo, members.repoOrder, len(manifest.Repos), name)
	tasks := buildWorkspaceTaskCandidates(members.tasksByRepo, members.repoOrder, len(manifest.Repos), name)
	if err := writeBrainProceduresFile(wsDir, procs); err != nil {
		return err
	}
	if err := writeBrainPracticesFile(wsDir, pracs); err != nil {
		return err
	}
	if err := writeBrainTasksFile(wsDir, tasks); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "workspace %s: %d cross-repo V2 pattern(s) from %d/%d member corpora\n",
		name, counts.Patterns, counts.WithCorpus, counts.Members)
	for _, w := range append(counts.Warnings, members.warnings...) {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", w)
	}
	return nil
}

func runWorkspacePatternsList(ctx context.Context, cmd *cobra.Command, opts Options, name string, list patternsListOptions) error {
	wsDir, err := workspaceDir(opts.Env, name)
	if err != nil {
		return err
	}
	views, _, corpusBacked := loadCorpusPatternViews(wsDir)
	if !corpusBacked {
		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "workspace %s: no cross-repo corpus (run `entire brain workspace patterns refresh %s`)\n", name, name)
		return nil
	}
	filtered := views[:0:0]
	for _, v := range views {
		if list.typ != "" && v.Type != list.typ {
			continue
		}
		if list.scope != "" && v.Scope != list.scope {
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
	if views, _, ok := loadCorpusPatternViews(wsDir); ok {
		fmt.Fprintf(out, "cross-repo V2 patterns: %d\n", len(views))
	}
	if run, ok := lastPatternRun(wsDir); ok {
		fmt.Fprintf(out, "last run: %s — %d pattern(s), %d synapse(s)\n", run.At, run.Patterns, run.Synapses)
	}
	for _, w := range members.warnings {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", w)
	}
	return nil
}

// buildWorkspaceTaskCandidates merges member-repo task candidates by intent
// signature, keeping tasks shared by ≥2 repos. Evidence (procedures, facts,
// commands) is unioned and a per-repo breakdown is attached; the candidate is
// workspace-scoped.
func buildWorkspaceTaskCandidates(byRepo map[string][]taskCandidate, repoOrder []string, memberCount int, workspace string) []taskCandidate {
	type agg struct {
		base      taskCandidate
		repos     map[string]bool
		support   int
		reinf     reinforcementCounts
		cmds      map[string]bool
		procKeys  map[string]taskProcedure
		facts     map[string]bool
		intents   map[string]bool
		breakdown map[string]*patternRepoStat
	}
	aggs := map[string]*agg{}
	for _, repoKey := range repoOrder {
		for _, t := range byRepo[repoKey] {
			a := aggs[t.IntentSignature]
			if a == nil {
				a = &agg{base: t, repos: map[string]bool{}, cmds: map[string]bool{}, procKeys: map[string]taskProcedure{}, facts: map[string]bool{}, intents: map[string]bool{}, breakdown: map[string]*patternRepoStat{}}
				aggs[t.IntentSignature] = a
			}
			a.repos[repoKey] = true
			a.support += t.Support
			a.reinf = addReinf(a.reinf, t.Reinforcement)
			for _, c := range t.Commands {
				a.cmds[c] = true
			}
			for _, p := range t.Procedures {
				key := strings.Join(p.Commands, "\x00")
				if cur, ok := a.procKeys[key]; !ok || p.Specificity > cur.Specificity {
					a.procKeys[key] = p
				}
			}
			for _, f := range t.MatchingFacts {
				a.facts[f] = true
			}
			for _, s := range t.SampleIntents {
				a.intents[s] = true
			}
			bd := a.breakdown[repoKey]
			if bd == nil {
				bd = &patternRepoStat{RepoKey: repoKey}
				a.breakdown[repoKey] = bd
			}
			bd.Support += t.Support
			bd.Reinforcement = addReinf(bd.Reinforcement, t.Reinforcement)
		}
	}

	var out []taskCandidate
	for sig, a := range aggs {
		repos := len(a.repos)
		if repos < workspaceMinRepos {
			continue
		}
		strength := workspacePracticeStrength("", repos, memberCount, a.support) // breadth+support blend
		out = append(out, taskCandidate{
			ID:              "task:ws:" + hexSHA(workspace+"\x00"+sig),
			Workspace:       workspace,
			IntentSignature: sig,
			Label:           a.base.Label,
			Support:         a.support,
			Reinforcement:   a.reinf,
			Commands:        sortedSetKeys(a.cmds),
			Procedures:      sortedProcedures(a.procKeys),
			MatchingFacts:   sortedSetKeys(a.facts),
			SampleIntents:   sortedSetKeys(a.intents),
			Repos:           repos,
			RepoBreakdown:   sortedBreakdown(a.breakdown),
			Strength:        math.Round(strength*1000) / 1000,
			StrengthLabel:   strengthLabel(strength),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Strength != out[j].Strength {
			return out[i].Strength > out[j].Strength
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// runWorkspaceSkillsList lists accepted cross-repo FAMILY proposals (SR5): the
// verified semantic groupings, not the raw exact-aggregated rows. Run
// `workspace patterns verify` to produce them.
func runWorkspaceSkillsList(ctx context.Context, cmd *cobra.Command, opts Options, name string, asJSON bool, limit int) error {
	wsDir, err := workspaceDir(opts.Env, name)
	if err != nil {
		return err
	}
	families := loadAcceptedWorkspaceFamilies(wsDir)
	if limit > 0 && len(families) > limit {
		families = families[:limit]
	}
	if asJSON {
		return writeJSON(cmd, families)
	}
	out := cmd.OutOrStdout()
	if len(families) == 0 {
		fmt.Fprintf(out, "workspace %s: no accepted cross-repo families (run `entire brain workspace patterns verify %s`)\n", name, name)
		return nil
	}
	for _, f := range families {
		repos := make([]string, 0, len(f.PerRepo))
		for _, v := range f.PerRepo {
			repos = append(repos, v.RepoKey)
		}
		fmt.Fprintf(out, "[family] %s  (%d repos: %s)\n", redactText(f.Title), f.repoCount(), strings.Join(repos, ", "))
		fmt.Fprintf(out, "    trigger: %s\n", redactText(f.Trigger))
		fmt.Fprintf(out, "    id %s\n", f.ID)
	}
	return nil
}

func runWorkspaceSkillsForm(ctx context.Context, cmd *cobra.Command, opts Options, name string, s skillFormOptions) error {
	wsDir, err := workspaceDir(opts.Env, name)
	if err != nil {
		return err
	}
	// Workspace skills MUST come from an accepted cross-repo family dossier.
	fam, ok := getAcceptedWorkspaceFamily(wsDir, s.taskID)
	if !ok {
		return fmt.Errorf("no accepted workspace family %q: run `entire brain workspace patterns verify %s` (explicit, egress-gated), then `entire brain workspace patterns skills %s`", s.taskID, name, name)
	}
	// A synthetic candidate carries id/name/scope + per-repo breakdown for the
	// skill-memory record; synthesis itself runs from the family.
	breakdown := make([]patternRepoStat, 0, len(fam.PerRepo))
	for _, v := range fam.PerRepo {
		breakdown = append(breakdown, patternRepoStat{RepoKey: v.RepoKey, Support: 1})
	}
	cand := taskCandidate{
		ID: fam.ID, Label: fam.Title, Workspace: name, Repos: fam.repoCount(),
		RepoBreakdown: breakdown, Commands: fam.CommonWorkflow, Support: fam.repoCount(),
		Strength: 0.7, StrengthLabel: "high",
	}
	agent := s.agent
	if agent == "" || agent == "auto" {
		agent = defaultRefreshAgent(ctx, opts.Runner, ".")
	}
	if agent == "none" {
		return fmt.Errorf("skill synthesis requires an agent (codex or claude-code); none found on PATH")
	}
	return synthesizeAndForm(ctx, cmd, cand, nil, &fam, wsDir, ".", agent, defaultDistillAgentRunner(agent), s, opts.Now().UTC())
}
