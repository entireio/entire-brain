package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
)

// `patterns skills` — the real skill surface (Phase 2 redo). It lists recurring
// task candidates and synthesizes an actual SKILL.md from one via an agent,
// gated on non-obviousness. This supersedes the command-n-gram procedure listing
// as the source of skills.

func newPatternsSkillsCommand(opts Options) *cobra.Command {
	var (
		asJSON bool
		limit  int
	)
	cmd := &cobra.Command{
		Use:   "skills [path]",
		Short: "List recurring task candidates that can be synthesized into skills",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPatternsSkillsList(cmd.Context(), cmd, opts, targetFromArgs(opts, args), asJSON, limit)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit task candidates as JSON")
	cmd.Flags().IntVar(&limit, "limit", 20, "Maximum candidates to show")
	cmd.AddCommand(newPatternsSkillsFormCommand(opts))
	return cmd
}

func newPatternsSkillsFormCommand(opts Options) *cobra.Command {
	var s skillFormOptions
	cmd := &cobra.Command{
		Use:   "form <task-id> [path]",
		Short: "Synthesize a SKILL.md from a task candidate (previews until --yes)",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := targetFromArgs(opts, nil)
			if len(args) == 2 {
				target = args[1]
			}
			s.taskID = args[0]
			return runPatternsSkillsForm(cmd.Context(), cmd, opts, target, s)
		},
	}
	cmd.Flags().StringVar(&s.name, "name", "", "Override the skill name (default: from the synthesized frontmatter)")
	cmd.Flags().StringVar(&s.target, "target", "standard", "Install target: standard|claude-code|codex|factoryai-droid|all")
	cmd.Flags().StringVar(&s.scope, "scope", "global", "Install scope: global|repo")
	cmd.Flags().StringVar(&s.agent, "agent", "auto", "Synthesis agent: auto|codex|claude-code")
	cmd.Flags().StringVar(&s.model, "model", "", "Override the agent model")
	cmd.Flags().StringVar(&s.effort, "effort", "", "Override the agent reasoning effort")
	cmd.Flags().BoolVar(&s.yes, "yes", false, "Write the synthesized skill files")
	cmd.Flags().BoolVar(&s.force, "force", false, "Overwrite an existing skill file")
	cmd.Flags().BoolVar(&s.draftOnly, "draft-only", false, "Print the synthesized SKILL.md without writing")
	cmd.Flags().BoolVar(&s.asJSON, "json", false, "Emit the result as JSON")
	return cmd
}

type skillFormOptions struct {
	taskID    string
	name      string
	target    string
	scope     string
	agent     string
	model     string
	effort    string
	yes       bool
	force     bool
	draftOnly bool
	asJSON    bool
}

func runPatternsSkillsList(ctx context.Context, cmd *cobra.Command, opts Options, target string, asJSON bool, limit int) error {
	brainDir, err := resolvePatternsBrainDir(ctx, opts, target)
	if err != nil {
		return err
	}
	// V2 corpus is the primary source; legacy task JSON is the fallback.
	tasks, corpusBacked := loadCorpusTaskCandidates(brainDir)
	if !corpusBacked {
		if tasks, err = loadBrainTasks(brainDir); err != nil {
			return err
		}
	}
	// Suppress already-formed-and-current / declined-unchanged candidates.
	tasks = filterSkillCandidatesByMemory(brainDir, tasks)
	if limit > 0 && len(tasks) > limit {
		tasks = tasks[:limit]
	}
	if asJSON {
		redacted := make([]taskCandidate, len(tasks))
		for i, t := range tasks {
			redacted[i] = redactCandidate(t)
		}
		return writeJSON(cmd, redacted)
	}
	out := cmd.OutOrStdout()
	if len(tasks) == 0 {
		fmt.Fprintln(out, "no task candidates (run `entire brain patterns refresh`)")
		return nil
	}
	for _, t := range tasks {
		r := t.Reinforcement
		fmt.Fprintf(out, "[%s] %s  (strength %.2f)\n", t.StrengthLabel, redactText(t.Label), t.Strength)
		fmt.Fprintf(out, "    %d session(s), %d with commands; %d↑ %d↓ %d·; commands: %s\n",
			t.Support, t.WithCommands, r.Success, r.Corrected, r.Neutral, redactText(joinMax(t.Commands, 5)))
		fmt.Fprintf(out, "    id %s\n", t.ID)
	}
	return nil
}

func runPatternsSkillsForm(ctx context.Context, cmd *cobra.Command, opts Options, target string, s skillFormOptions) error {
	repoDir, local, err := resolveLocalTargetRepoDir(ctx, opts.Runner, target)
	if err != nil {
		return err
	}
	if !local {
		return fmt.Errorf("patterns skills form requires a local repository path: %s", target)
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return err
	}
	brainDir := storage.BrainDir

	var cand *taskCandidate
	if c, corpusBacked := corpusTaskCandidateByID(brainDir, s.taskID); corpusBacked {
		cand = c
	} else {
		tasks, err := loadBrainTasks(brainDir)
		if err != nil {
			return err
		}
		for i := range tasks {
			if tasks[i].ID == s.taskID {
				cand = &tasks[i]
				break
			}
		}
	}
	if cand == nil {
		return fmt.Errorf("task candidate not found: %s (run `entire brain patterns skills`)", s.taskID)
	}

	agent := s.agent
	if agent == "" || agent == "auto" {
		agent = defaultRefreshAgent(ctx, opts.Runner, repoDir)
	}
	if agent == "none" {
		return fmt.Errorf("skill synthesis requires an agent (codex or claude-code); none found on PATH")
	}
	return synthesizeAndForm(ctx, cmd, *cand, brainDir, repoDir, agent, defaultDistillAgentRunner(agent), s, opts.Now().UTC())
}

// synthesizeAndForm is the single skill-creation path (repo and workspace):
// synthesize via the agent, reject NOT_A_SKILL, then show evidence + draft and
// stop unless --yes. All evidence/draft egress is redacted. storeDir is where the
// skill-memory decision is recorded. run is injected so tests can stub the agent.
func synthesizeAndForm(ctx context.Context, cmd *cobra.Command, cand taskCandidate, storeDir, repoDir, agent string, run distillAgentRunner, s skillFormOptions, now time.Time) error {
	fmt.Fprintf(cmd.ErrOrStderr(), "synthesizing skill from %d sessions via %s…\n", cand.Support, agent)
	res, err := synthesizeSkill(ctx, repoDir, storeDir, cand, agent, s.model, s.effort, run)
	if err != nil {
		return err
	}
	if !res.IsSkill {
		if s.asJSON {
			return writeJSON(cmd, map[string]any{"task_id": cand.ID, "is_skill": false, "reason": res.Reason})
		}
		fmt.Fprintf(cmd.OutOrStdout(), "not a skill: %s\n", res.Reason)
		return nil
	}
	name := s.name
	if name == "" {
		name = res.Name
	}
	if name == "" {
		name = "skill"
	}
	skillText := redactText(res.SkillText)

	if s.draftOnly {
		if s.asJSON {
			return writeJSON(cmd, map[string]any{"task_id": cand.ID, "is_skill": true, "name": name, "skill": skillText})
		}
		fmt.Fprintln(cmd.OutOrStdout(), skillText)
		return nil
	}

	dests, err := skillDestinations(s.target, s.scope, name, repoDir)
	if err != nil {
		return err
	}

	// Evidence-first preview (Priority 6): show WHY this is a skill, then the
	// draft and the exact would-write destinations. Nothing is written.
	if !s.yes {
		if s.asJSON {
			return writeJSON(cmd, map[string]any{
				"task_id": cand.ID, "is_skill": true, "name": name,
				"evidence": redactCandidate(cand), "skill": skillText, "would_write": dests,
			})
		}
		out := cmd.OutOrStdout()
		renderCandidateEvidence(out, cand)
		fmt.Fprintln(out, "\n--- proposed SKILL.md ---")
		fmt.Fprintln(out, skillText)
		fmt.Fprintln(out, "\nwould write to:")
		for _, d := range dests {
			fmt.Fprintf(out, "  %s   (%s)\n", d.Path, joinMax(d.Agents, 8))
		}
		fmt.Fprintf(out, "\nre-run with --yes to write%s\n", overwriteHint(dests))
		return nil
	}

	// Write path.
	if !s.force {
		for _, d := range dests {
			if _, statErr := os.Stat(skillFilePath(d.Path)); statErr == nil {
				return fmt.Errorf("refusing to overwrite existing skill file: %s (use --force)", d.Path)
			}
		}
	}
	data := []byte(skillText)
	sha := "sha256:" + fmt.Sprintf("%x", sha256Sum(data))
	var installs []skillInstall
	for _, d := range dests {
		abs := skillFilePath(d.Path)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return fmt.Errorf("create skill dir: %w", err)
		}
		if err := os.WriteFile(abs, data, 0o644); err != nil {
			return fmt.Errorf("write skill: %w", err)
		}
		installs = append(installs, skillInstall{Agents: d.Agents, Path: d.Path, ContentSHA: sha})
	}
	if err := recordSkillDecision(storeDir, skillMemoryRecord{
		PatternID: cand.ID, Scope: candScope(cand), Status: skillStatusActive, SkillName: name,
		Installs: installs, Fingerprint: patternEvidenceFingerprint(taskView(cand)),
		Support: cand.Support, Reinforcement: cand.Reinforcement, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		return err
	}
	if s.asJSON {
		return writeJSON(cmd, map[string]any{"task_id": cand.ID, "name": name, "status": "active", "installs": installs})
	}
	fmt.Fprintf(cmd.OutOrStdout(), "formed skill %q from %s:\n", name, cand.ID)
	for _, in := range installs {
		fmt.Fprintf(cmd.OutOrStdout(), "  wrote %s   (%s)\n", in.Path, joinMax(in.Agents, 8))
	}
	return nil
}

// renderCandidateEvidence prints the evidence-first approval view: why this is a
// skill, before the draft. All fields are redacted.
func renderCandidateEvidence(out io.Writer, cand taskCandidate) {
	c := redactCandidate(cand)
	r := c.Reinforcement
	fmt.Fprintf(out, "Candidate: %s\n", c.Label)
	fmt.Fprintf(out, "id %s\n", cand.ID)
	if c.Repos > 0 {
		fmt.Fprintf(out, "Support: %d session(s) across %d repo(s); reinforcement %d↑ %d↓ %d·\n", c.Support, c.Repos, r.Success, r.Corrected, r.Neutral)
	} else {
		fmt.Fprintf(out, "Support: %d session(s) (%d with commands); reinforcement %d↑ %d↓ %d·\n", c.Support, c.WithCommands, r.Success, r.Corrected, r.Neutral)
	}
	if len(c.SampleIntents) > 0 {
		fmt.Fprintln(out, "Sample intents:")
		for _, s := range c.SampleIntents {
			fmt.Fprintf(out, "  - %s\n", s)
		}
	}
	if len(c.Procedures) > 0 {
		fmt.Fprintln(out, "Co-occurring procedure evidence:")
		for _, p := range c.Procedures {
			fmt.Fprintf(out, "  - %s  (in %d sessions, specificity %.2f)\n", joinArrow(p.Commands), p.Count, p.Specificity)
		}
	}
	if len(c.MatchingFacts) > 0 {
		fmt.Fprintln(out, "Relevant durable facts:")
		for _, f := range c.MatchingFacts {
			fmt.Fprintf(out, "  - %s\n", f)
		}
	}
	if len(cand.Examples) > 0 {
		fmt.Fprintln(out, "Source anchors:")
		for _, a := range cand.Examples {
			fmt.Fprintf(out, "  - %s:%d\n", redactText(a.Path), a.Line)
		}
	}
}

func joinArrow(cmds []string) string {
	out := ""
	for i, c := range cmds {
		if i > 0 {
			out += " → "
		}
		out += c
	}
	return out
}

// candScope reports the scope of a task candidate for skill-memory.
func candScope(c taskCandidate) string {
	if c.Workspace != "" {
		return "workspace"
	}
	return "repo"
}

// taskView adapts a task candidate to a patternView for fingerprinting/skill memory.
func taskView(c taskCandidate) patternView {
	r := c.Reinforcement
	return patternView{ID: c.ID, Type: "task", Scope: candScope(c), Title: c.Label,
		Strength: c.Strength, StrengthLabel: c.StrengthLabel, Support: c.Support, Reinforcement: &r}
}

func joinMax(items []string, n int) string {
	if len(items) > n {
		items = items[:n]
	}
	out := ""
	for i, s := range items {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}
