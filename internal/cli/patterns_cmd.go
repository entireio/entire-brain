package cli

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// patterns command surface (Pattern Consolidation, Phase 1).
//
// Public, job-oriented commands per the plan: `patterns refresh` rebuilds the
// derived layer, `patterns status` reports freshness and counts. The bare
// `patterns` command is a read-only default — in Phase 1 it shows status; the
// strongest-patterns listing arrives with procedure detection (Phase 2).

func newPatternsCommand(opts Options) *cobra.Command {
	var listOpts patternsListOptions
	cmd := &cobra.Command{
		Use:     "patterns [path]",
		Short:   "Inspect and rebuild repeated-work patterns derived from session history",
		GroupID: "create",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPatternsList(cmd.Context(), cmd, opts, targetFromArgs(opts, args), listOpts)
		},
	}
	cmd.Flags().BoolVar(&listOpts.asJSON, "json", false, "Emit patterns as JSON")
	cmd.Flags().IntVar(&listOpts.limit, "limit", 20, "Maximum number of patterns to show")
	cmd.Flags().StringVar(&listOpts.typ, "type", "", "Filter by type: task|procedure|risk|practice|theme")
	cmd.Flags().StringVar(&listOpts.scope, "scope", "", "Filter by scope: repo|workspace")
	cmd.AddCommand(newPatternsRefreshCommand(opts))
	cmd.AddCommand(newPatternsStatusCommand(opts))
	cmd.AddCommand(newPatternsVerifyCommand(opts))
	cmd.AddCommand(newPatternsSkillsCommand(opts))
	return cmd
}

type patternsListOptions struct {
	asJSON bool
	limit  int
	typ    string
	scope  string
}

func runPatternsList(ctx context.Context, cmd *cobra.Command, opts Options, target string, listOpts patternsListOptions) error {
	brainDir, err := resolvePatternsBrainDir(ctx, opts, target)
	if err != nil {
		return err
	}
	// Primary analytical source is the V2 corpus; the legacy procedure/practice
	// JSON views remain only as a fallback when no corpus has been built yet.
	views, _, corpusBacked := loadCorpusPatternViews(brainDir)
	if !corpusBacked {
		var verr error
		if views, _, verr = loadPatternViews(brainDir); verr != nil {
			return verr
		}
	} else {
		// Themes (latent practices) join the corpus listing as their own type.
		for _, th := range loadThemeViews(brainDir, false) {
			views = append(views, themePatternView(th))
		}
	}
	memIdx := skillMemoryByPatternID(mustLoadSkillMemory(brainDir))

	filtered := views[:0:0]
	for _, v := range views {
		if listOpts.typ != "" && v.Type != listOpts.typ {
			continue
		}
		if listOpts.scope != "" && v.Scope != listOpts.scope {
			continue
		}
		// Skill-memory recommendation: suppress already-formed-and-current and
		// declined-and-unchanged patterns; annotate the rest that have a decision.
		if rec, ok := memIdx[v.ID]; ok {
			eval := evaluateSkillMemory(rec, &v)
			if eval.suppressInListing() {
				continue
			}
			v.SkillStatus = eval.Status + "/" + eval.Sub
			v.Note = eval.annotation()
		}
		filtered = append(filtered, v)
	}
	sort.SliceStable(filtered, func(i, j int) bool { return filtered[i].Strength > filtered[j].Strength })
	if listOpts.limit > 0 && len(filtered) > listOpts.limit {
		filtered = filtered[:listOpts.limit]
	}

	if listOpts.asJSON {
		return writeJSON(cmd, filtered)
	}
	out := cmd.OutOrStdout()
	if len(filtered) == 0 {
		if !buildPatternsStatusReport(brainDir).Present {
			fmt.Fprintln(out, "patterns: not built (run `entire brain patterns refresh`)")
		} else {
			fmt.Fprintln(out, "patterns: none detected yet")
		}
		return nil
	}
	for _, v := range filtered {
		renderPatternView(out, v)
	}
	return nil
}

// patternView is the unified listing shape for both pattern families, so
// procedures and practices sort and render through one path.
type patternView struct {
	ID            string               `json:"id"`
	Type          string               `json:"type"`
	Scope         string               `json:"scope"`
	Kind          string               `json:"kind,omitempty"`
	Title         string               `json:"title"`
	Strength      float64              `json:"strength"`
	StrengthLabel string               `json:"strength_label"`
	Support       int                  `json:"support"`
	Repos         int                  `json:"repos,omitempty"`          // workspace scope
	RepoBreakdown []patternRepoStat    `json:"repo_breakdown,omitempty"` // workspace scope
	Reinforcement *reinforcementCounts `json:"reinforcement,omitempty"`
	Example       *episodeAnchor       `json:"example,omitempty"`
	SkillStatus   string               `json:"skill_status,omitempty"` // e.g. "active/update", "declined/reconsider"
	Note          string               `json:"note,omitempty"`         // human-readable recommendation
	// V2 corpus-backed fields (empty for legacy views).
	IntentSig     string `json:"intent_sig,omitempty"`
	Gram          string `json:"gram,omitempty"`
	DossierStatus string `json:"dossier_status,omitempty"` // current | stale (promotable patterns only)
	Verdict       string `json:"verdict,omitempty"`        // accepted | needs_split | low_confidence (rejected are suppressed)
	Workspace     string `json:"workspace,omitempty"`
}

// loadPatternViews loads procedures + practices as unified views and an index by
// pattern id.
func loadPatternViews(brainDir string) ([]patternView, map[string]patternView, error) {
	procedures, err := loadBrainProcedures(brainDir)
	if err != nil {
		return nil, nil, err
	}
	practices, err := loadBrainPractices(brainDir)
	if err != nil {
		return nil, nil, err
	}
	views := make([]patternView, 0, len(procedures)+len(practices))
	for _, p := range procedures {
		views = append(views, procedureView(p))
	}
	for _, p := range practices {
		views = append(views, practiceView(p))
	}
	idx := make(map[string]patternView, len(views))
	for _, v := range views {
		idx[v.ID] = v
	}
	return views, idx, nil
}

// strongestPatterns loads the top-N patterns by strength for the overview.
func strongestPatterns(brainDir string, limit int) []patternView {
	views, _, err := loadPatternViews(brainDir)
	if err != nil || len(views) == 0 {
		return nil
	}
	sort.SliceStable(views, func(i, j int) bool { return views[i].Strength > views[j].Strength })
	if limit > 0 && len(views) > limit {
		views = views[:limit]
	}
	return views
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
	records, err := loadBrainSkillMemory(brainDir)
	if err != nil {
		return nil
	}
	return records
}

func procedureView(p procedureRecord) patternView {
	r := p.Reinforcement
	v := patternView{
		ID: p.ID, Type: p.Type, Scope: p.Scope,
		Title:    strings.Join(p.Commands, " → "),
		Strength: p.Strength, StrengthLabel: p.StrengthLabel,
		Support: p.Support, Reinforcement: &r,
		Repos: p.Repos, RepoBreakdown: p.RepoBreakdown,
	}
	if len(p.Examples) > 0 {
		v.Example = &p.Examples[0]
	}
	return v
}

func practiceView(p practiceRecord) patternView {
	v := patternView{
		ID: p.ID, Type: p.Type, Scope: p.Scope, Kind: p.Kind,
		Title:    p.Statement,
		Strength: p.Strength, StrengthLabel: p.StrengthLabel,
		Support: p.Support,
		Repos:   p.Repos, RepoBreakdown: p.RepoBreakdown,
	}
	if len(p.Examples) > 0 {
		v.Example = &p.Examples[0]
	}
	return v
}

// renderPatternView prints a concise read-only pattern card. Patterns are
// diagnostic; the evidence-first approval card lives in `patterns skills form`.
func renderPatternView(out io.Writer, v patternView) {
	label := v.Type
	if v.Kind != "" {
		label += "/" + v.Kind
	}
	scope := v.Scope
	if v.Scope == "workspace" && v.Workspace != "" {
		scope = "workspace:" + v.Workspace
	}
	state := v.DossierStatus
	if v.Verdict != "" {
		state = strings.TrimLeft(state+"/"+v.Verdict, "/")
	}
	header := fmt.Sprintf("[%s] %s  (%s", strings.ToUpper(v.StrengthLabel), redactText(v.Title), label)
	if scope != "" {
		header += ", " + scope
	}
	header += fmt.Sprintf(", strength %.2f", v.Strength)
	if state != "" {
		header += ", " + state
	}
	header += ")"
	fmt.Fprintln(out, header)
	if v.Reinforcement != nil {
		r := v.Reinforcement
		fmt.Fprintf(out, "    seen in %d episode(s); reinforcement %d↑ %d↓ %d·\n", v.Support, r.Success, r.Corrected, r.Neutral)
	} else {
		fmt.Fprintf(out, "    seen in %d session(s)\n", v.Support)
	}
	if v.Scope == "workspace" && v.Repos > 0 {
		repos := make([]string, 0, len(v.RepoBreakdown))
		for _, b := range v.RepoBreakdown {
			repos = append(repos, fmt.Sprintf("%s(%d)", b.RepoKey, b.Support))
		}
		fmt.Fprintf(out, "    across %d repo(s): %s\n", v.Repos, strings.Join(repos, ", "))
	}
	if v.Example != nil {
		fmt.Fprintf(out, "    e.g. %s:%d   id %s\n", redactText(v.Example.Path), v.Example.Line, v.ID)
	} else {
		fmt.Fprintf(out, "    id %s\n", v.ID)
	}
	if v.Note != "" {
		fmt.Fprintf(out, "    ! %s\n", v.Note)
	}
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

func newPatternsVerifyCommand(opts Options) *cobra.Command {
	var (
		asJSON       bool
		deep         bool
		themes       bool
		agent, model string
		effort       string
	)
	cmd := &cobra.Command{
		Use:   "verify [path]",
		Short: "Agent-audit promotable consolidation dossiers (explicit, egress-gated, cached)",
		Long: "Run the optional consolidation verifier over promotable dossiers. This is the " +
			"only surface that may invoke an agent for patterns; refresh/watch/brief/query/MCP " +
			"never do. Verdicts are cached by evidence fingerprint and re-used until the evidence " +
			"changes. With --deep, each pattern is audited against a bounded evidence-deep export " +
			"of its backing episodes (corrected/failed first; up to 40 anchors, the top 12 carrying " +
			"redacted transcript excerpts, with parameters and recoveries) instead of the sampled " +
			"shallow dossier. With --themes, candidate latent-practice themes " +
			"(recurring read-only/conversational work) are verified before they surface.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPatternsVerify(cmd.Context(), cmd, opts, targetFromArgs(opts, args), agent, model, effort, asJSON, deep, themes)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit the verification summary as JSON")
	cmd.Flags().BoolVar(&deep, "deep", false, "Audit each pattern against a bounded evidence-deep export of its backing episodes (deep dossier)")
	cmd.Flags().BoolVar(&themes, "themes", false, "Verify candidate latent-practice themes before they surface")
	cmd.Flags().StringVar(&agent, "agent", "auto", "Verifier agent: auto, codex, claude-code, ollama, or command")
	cmd.Flags().StringVar(&model, "model", "", "Override the agent model, or select the local Ollama model")
	cmd.Flags().StringVar(&effort, "effort", "", "Agent reasoning effort (codex/claude-code)")
	return cmd
}

func runPatternsVerify(ctx context.Context, cmd *cobra.Command, opts Options, target, agent, model, effort string, asJSON, deep, themes bool) error {
	repoDir, brainDir, err := resolvePatternsRepoAndBrain(ctx, opts, target)
	if err != nil {
		return err
	}
	if err := rejectAgentForNoEgress(agent); err != nil {
		return err
	}
	db, err := openPatternCorpusDB(brainDir)
	if err != nil {
		return err
	}
	defer db.Close()
	run := defaultDistillAgentRunner(agent)
	verify := verifyDossiers
	mode := "dossier"
	switch {
	case themes:
		verify = verifyThemes
		mode = "theme"
	case deep:
		verify = verifyDeepDossiers
		mode = "deep dossier"
	}
	stats, err := verify(ctx, db, brainDir, repoDir, agent, model, effort, run, opts.Now().UTC())
	if err != nil {
		return err
	}
	if asJSON {
		return writeJSON(cmd, stats)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "patterns verify: %d %s(s) — %d verified, %d cached, %d failed\n",
		stats.Considered, mode, stats.Verified, stats.Cached, stats.Failed)
	return nil
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
	_, brainDir, err := resolvePatternsRepoAndBrain(ctx, opts, target)
	return brainDir, err
}

// resolvePatternsRepoAndBrain resolves both the local repo directory (needed as
// the agent runner's working dir) and the brain storage dir for that repo.
func resolvePatternsRepoAndBrain(ctx context.Context, opts Options, target string) (repoDir, brainDir string, err error) {
	repoDir, local, err := resolveLocalTargetRepoDir(ctx, opts.Runner, target)
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

func runPatternsRefresh(ctx context.Context, cmd *cobra.Command, opts Options, target string, asJSON bool) error {
	brainDir, err := resolvePatternsBrainDir(ctx, opts, target)
	if err != nil {
		return err
	}
	source, err := writeBrainEpisodesAndSource(brainDir, opts.Now().UTC())
	if err != nil {
		return err
	}
	// Explicit refresh is the stricter surface: a corpus failure is fatal here.
	if err := buildPatternCorpus(brainDir, opts.Now().UTC()); err != nil {
		return fmt.Errorf("pattern corpus: %w", err)
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
	Present          bool                 `json:"present"`
	Freshness        string               `json:"freshness"` // current | stale | missing
	Episodes         int                  `json:"episodes"`
	Reinforcement    *reinforcementCounts `json:"reinforcement,omitempty"`
	Procedures       int                  `json:"procedures"`
	Practices        int                  `json:"practices"`
	Patterns         int                  `json:"patterns"`
	AcceptedSkills   int                  `json:"accepted_skills"`
	DeclinedPatterns int                  `json:"declined_patterns"`
	UpdatesAvailable int                  `json:"updates_available"`
	LastRun          *patternRun          `json:"last_run,omitempty"` // what the last corpus build produced
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
	fmt.Fprintf(out, "accepted skills: %d\n", report.AcceptedSkills)
	fmt.Fprintf(out, "declined patterns: %d\n", report.DeclinedPatterns)
	fmt.Fprintf(out, "updates available: %d\n", report.UpdatesAvailable)
	if lr := report.LastRun; lr != nil {
		fmt.Fprintf(out, "last run: %s — %d episode(s), %d pattern(s), %d dossier(s), %d symbol link(s), %d commit(s), %d synapse(s)\n",
			lr.At, lr.Episodes, lr.Patterns, lr.Dossiers, lr.SymbolLinks, lr.Commits, lr.Synapses)
	}
	return nil
}

// buildPatternsStatusReport reads the patterns source from the manifest and
// derives freshness from the sessions fingerprint, the same input-derived signal
// the history source uses — no re-extraction required. Skill-memory counts are
// computed by evaluating each user decision against the current pattern set.
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
	report := patternsStatusReport{
		Present:       true,
		Freshness:     freshness,
		Episodes:      src.Episodes,
		Reinforcement: &rc,
		Procedures:    src.Procedures,
		Practices:     src.Practices,
		Patterns:      src.Patterns,
	}
	if run, ok := lastPatternRun(brainDir); ok {
		report.LastRun = &run
	}

	_, byID, _ := loadPatternViews(brainDir)
	for _, rec := range mustLoadSkillMemory(brainDir) {
		var cur *patternView
		if v, ok := byID[rec.PatternID]; ok {
			cur = &v
		}
		eval := evaluateSkillMemory(rec, cur)
		if eval.Status == skillStatusDeclined {
			report.DeclinedPatterns++
		} else {
			report.AcceptedSkills++
		}
		if eval.needsAttention() {
			report.UpdatesAvailable++
		}
	}
	return report
}
