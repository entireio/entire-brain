package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// Skill formation (Pattern Consolidation, Phase 5).
//
// `patterns form <id>` turns a selected pattern into a reusable SKILL.md. The
// safety contract: nothing is written until the user confirms a destination.
// Without --yes the command previews (card + draft + would-write paths) and
// stops; --yes (which requires --name) writes; --draft-only prints only; and
// --decline records a decline. An existing destination file is never overwritten
// without --force. Every write/decline records a skill-memory decision.

func newPatternsFormCommand(opts Options) *cobra.Command {
	var f formOptions
	cmd := &cobra.Command{
		Use:   "form <pattern-id> [path]",
		Short: "Form a pattern into a reusable skill (previews until --yes)",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := "."
			if len(args) == 2 {
				target = args[1]
			} else if opts.Env.RepoRoot != "" {
				target = opts.Env.RepoRoot
			}
			f.patternID = args[0]
			return runPatternsForm(cmd.Context(), cmd, opts, target, f)
		},
	}
	cmd.Flags().StringVar(&f.name, "name", "", "Skill name (required with --yes; derived for preview)")
	cmd.Flags().StringVar(&f.target, "target", "standard", "Install target: standard|claude-code|codex|factoryai-droid|all")
	cmd.Flags().StringVar(&f.scope, "scope", "global", "Install scope: global|repo")
	cmd.Flags().BoolVar(&f.yes, "yes", false, "Confirm and write the skill files")
	cmd.Flags().BoolVar(&f.force, "force", false, "Overwrite an existing skill file")
	cmd.Flags().BoolVar(&f.draftOnly, "draft-only", false, "Print the SKILL.md draft without writing")
	cmd.Flags().BoolVar(&f.decline, "decline", false, "Record a decline for this pattern (no file written)")
	cmd.Flags().BoolVar(&f.asJSON, "json", false, "Emit the result as JSON")
	return cmd
}

type formOptions struct {
	patternID string
	name      string
	target    string
	scope     string
	yes       bool
	force     bool
	draftOnly bool
	decline   bool
	asJSON    bool
}

type skillDestination struct {
	Agents []string `json:"agents"`
	Path   string   `json:"path"`
}

func runPatternsForm(ctx context.Context, cmd *cobra.Command, opts Options, target string, f formOptions) error {
	repoDir, local, err := resolveLocalTargetRepoDir(ctx, opts.Runner, target)
	if err != nil {
		return err
	}
	if !local {
		return fmt.Errorf("patterns form requires a local repository path: %s", target)
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return err
	}
	brainDir := storage.BrainDir

	_, byID, err := loadPatternViews(brainDir)
	if err != nil {
		return err
	}
	view, ok := byID[f.patternID]
	if !ok {
		return fmt.Errorf("pattern not found: %s (run `entire brain patterns` to list ids)", f.patternID)
	}
	now := opts.Now().UTC()

	// Decline path: record the decision, write no files.
	if f.decline {
		if err := recordSkillDecision(brainDir, skillMemoryRecord{
			PatternID: view.ID, Scope: view.Scope, Status: skillStatusDeclined,
			Fingerprint: patternEvidenceFingerprint(view), CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			return err
		}
		if f.asJSON {
			return writeJSON(cmd, map[string]any{"pattern_id": view.ID, "status": "declined"})
		}
		fmt.Fprintf(cmd.OutOrStdout(), "declined: %s\n", view.ID)
		return nil
	}

	name := f.name
	if name == "" {
		name = deriveSkillName(view)
	}
	draft := renderSkillDraft(view, name, now)

	if f.draftOnly {
		if f.asJSON {
			return writeJSON(cmd, map[string]any{"pattern_id": view.ID, "name": name, "draft": draft})
		}
		fmt.Fprintln(cmd.OutOrStdout(), draft)
		return nil
	}

	destinations, err := skillDestinations(f.target, f.scope, name, repoDir)
	if err != nil {
		return err
	}

	// Preview: render card + draft + would-write paths, then stop. No write.
	if !f.yes {
		out := cmd.OutOrStdout()
		if f.asJSON {
			return writeJSON(cmd, map[string]any{"pattern_id": view.ID, "name": name, "draft": draft, "would_write": destinations})
		}
		renderSkillCard(out, view)
		fmt.Fprintln(out, "\n--- proposed SKILL.md ---")
		fmt.Fprintln(out, draft)
		fmt.Fprintln(out, "would write to:")
		for _, d := range destinations {
			fmt.Fprintf(out, "  %s   (%s)\n", d.Path, strings.Join(d.Agents, ", "))
		}
		fmt.Fprintf(out, "\nre-run with --name <name> --yes to write%s\n", overwriteHint(destinations))
		return nil
	}

	// Write path.
	if f.name == "" {
		return fmt.Errorf("--name is required with --yes")
	}
	if !f.force {
		for _, d := range destinations {
			if _, err := os.Stat(expandHomePath(d.Path)); err == nil {
				return fmt.Errorf("refusing to overwrite existing skill file: %s (use --force)", d.Path)
			}
		}
	}
	data := []byte(draft)
	sha := "sha256:" + hex.EncodeToString(sha256Sum(data))
	var installs []skillInstall
	for _, d := range destinations {
		abs := expandHomePath(d.Path)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return fmt.Errorf("create skill dir: %w", err)
		}
		if err := os.WriteFile(abs, data, 0o644); err != nil {
			return fmt.Errorf("write skill: %w", err)
		}
		installs = append(installs, skillInstall{Agents: d.Agents, Path: d.Path, ContentSHA: sha})
	}

	rec := skillMemoryRecord{
		PatternID: view.ID, Scope: view.Scope, Status: skillStatusActive,
		SkillName: name, Installs: installs, Fingerprint: patternEvidenceFingerprint(view),
		Support: view.Support, CreatedAt: now, UpdatedAt: now,
	}
	if view.Reinforcement != nil {
		rec.Reinforcement = *view.Reinforcement
	}
	if err := recordSkillDecision(brainDir, rec); err != nil {
		return err
	}

	if f.asJSON {
		return writeJSON(cmd, map[string]any{"pattern_id": view.ID, "name": name, "status": "active", "installs": installs})
	}
	fmt.Fprintf(cmd.OutOrStdout(), "formed skill %q from %s:\n", name, view.ID)
	for _, in := range installs {
		fmt.Fprintf(cmd.OutOrStdout(), "  wrote %s   (%s)\n", in.Path, strings.Join(in.Agents, ", "))
	}
	return nil
}

// recordSkillDecision upserts a skill-memory record by pattern id, preserving the
// rest of the user-state file.
func recordSkillDecision(brainDir string, rec skillMemoryRecord) error {
	existing, err := loadBrainSkillMemory(brainDir)
	if err != nil {
		return err
	}
	out := make([]skillMemoryRecord, 0, len(existing)+1)
	replaced := false
	for _, r := range existing {
		if r.PatternID == rec.PatternID {
			if !r.CreatedAt.IsZero() {
				rec.CreatedAt = r.CreatedAt // preserve original creation time on update
			}
			out = append(out, rec)
			replaced = true
			continue
		}
		out = append(out, r)
	}
	if !replaced {
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PatternID < out[j].PatternID })
	return writeBrainSkillMemory(brainDir, out)
}

// skillDestinations resolves the install paths for a target+scope. "standard" is
// the cross-agent .agents/skills path (read by copilot-cli, cursor, gemini, pi);
// the holdouts get their own roots. "all" writes the minimal covering set.
func skillDestinations(target, scope, name, repoDir string) ([]skillDestination, error) {
	standardAgents := []string{"copilot-cli", "cursor", "gemini", "pi"}
	rel := filepath.Join("skills", name, "SKILL.md")

	mk := func(globalRoot, repoSub string, agents []string) (skillDestination, error) {
		var base string
		switch scope {
		case "global":
			base = globalRoot
		case "repo":
			if repoDir == "" {
				return skillDestination{}, fmt.Errorf("repo scope requires a local repository")
			}
			base = filepath.Join(repoDir, repoSub)
		default:
			return skillDestination{}, fmt.Errorf("invalid scope %q (want global|repo)", scope)
		}
		return skillDestination{Agents: agents, Path: filepath.Join(base, rel)}, nil
	}

	codexGlobal := os.Getenv("CODEX_HOME")
	if codexGlobal == "" {
		codexGlobal = "~/.codex"
	}
	specs := map[string]struct {
		globalRoot, repoSub string
		agents              []string
	}{
		"standard":        {"~/.agents", ".agents", standardAgents},
		"claude-code":     {"~/.claude", ".claude", []string{"claude-code", "opencode"}},
		"codex":           {codexGlobal, ".codex", []string{"codex"}},
		"factoryai-droid": {"~/.factory", ".factory", []string{"factoryai-droid"}},
	}

	var order []string
	switch target {
	case "all":
		order = []string{"standard", "claude-code", "codex", "factoryai-droid"}
	case "standard", "claude-code", "codex", "factoryai-droid":
		order = []string{target}
	default:
		return nil, fmt.Errorf("invalid target %q (want standard|claude-code|codex|factoryai-droid|all)", target)
	}

	var dests []skillDestination
	for _, key := range order {
		s := specs[key]
		d, err := mk(s.globalRoot, s.repoSub, s.agents)
		if err != nil {
			return nil, err
		}
		dests = append(dests, d)
	}
	return dests, nil
}

func overwriteHint(dests []skillDestination) string {
	for _, d := range dests {
		if _, err := os.Stat(expandHomePath(d.Path)); err == nil {
			return " (add --force to overwrite existing files)"
		}
	}
	return ""
}

var skillNameSanitize = regexp.MustCompile(`[^a-z0-9]+`)

func deriveSkillName(v patternView) string {
	base := v.Title
	if v.Type == "procedure" {
		base = strings.ReplaceAll(v.Title, " → ", " ")
	}
	slug := strings.Trim(skillNameSanitize.ReplaceAllString(strings.ToLower(base), "-"), "-")
	if slug == "" {
		slug = "skill"
	}
	// Keep names short and stable.
	parts := strings.Split(slug, "-")
	if len(parts) > 6 {
		parts = parts[:6]
	}
	prefix := "do"
	if v.Type == "practice" {
		prefix = v.Kind
		if prefix == "" {
			prefix = "practice"
		}
	}
	return strings.Trim(prefix+"-"+strings.Join(parts, "-"), "-")
}

// renderSkillCard prints the full approval card for a pattern (richer than the
// listing card).
func renderSkillCard(out io.Writer, v patternView) {
	label := v.Type
	if v.Kind != "" {
		label += "/" + v.Kind
	}
	fmt.Fprintf(out, "Pattern: %s\n", v.ID)
	fmt.Fprintf(out, "Type: %s    Scope: %s    Strength: %s (%.2f)\n", label, v.Scope, v.StrengthLabel, v.Strength)
	if v.Reinforcement != nil {
		r := v.Reinforcement
		fmt.Fprintf(out, "Reinforcement: %d success, %d corrected, %d neutral\n", r.Success, r.Corrected, r.Neutral)
	}
	fmt.Fprintf(out, "Seen in: %d\n", v.Support)
	fmt.Fprintf(out, "\nWhat repeats:\n  %s\n", v.Title)
	if v.Example != nil {
		fmt.Fprintf(out, "\nEvidence:\n  %s:%d\n", v.Example.Path, v.Example.Line)
	}
}

func renderSkillDraft(v patternView, name string, now time.Time) string {
	var b strings.Builder
	desc := skillDescription(v)
	fmt.Fprintf(&b, "---\nname: %s\ndescription: %s\n---\n\n", name, desc)
	title := name
	fmt.Fprintf(&b, "# %s\n\n", title)

	fmt.Fprintf(&b, "## Steps\n\n")
	if v.Type == "procedure" {
		for i, step := range strings.Split(v.Title, " → ") {
			fmt.Fprintf(&b, "%d. `%s`\n", i+1, step)
		}
	} else {
		fmt.Fprintf(&b, "%s\n", v.Title)
	}

	fmt.Fprintf(&b, "\n## Verification\n\n")
	if v.Type == "procedure" && strings.Contains(v.Title, "test") {
		fmt.Fprintf(&b, "Confirm the test step passes before treating the work as done.\n")
	} else {
		fmt.Fprintf(&b, "Confirm the change matches the user's stated intent before moving on.\n")
	}

	fmt.Fprintf(&b, "\n## Failure Modes\n\n")
	if v.Reinforcement != nil && v.Reinforcement.Corrected > 0 {
		fmt.Fprintf(&b, "This workflow was corrected in %d episode(s); double-check the result rather than assuming success.\n", v.Reinforcement.Corrected)
	} else {
		fmt.Fprintf(&b, "No correction signal recorded; still verify before relying on the outcome.\n")
	}

	fmt.Fprintf(&b, "\n## Evidence\n\n")
	fmt.Fprintf(&b, "Formed by Entire Brain from %d observation(s) with retained source anchors.\n", v.Support)
	if v.Example != nil {
		fmt.Fprintf(&b, "- %s:%d\n", v.Example.Path, v.Example.Line)
	}
	return strings.TrimRight(b.String(), "\n")
}

func skillDescription(v patternView) string {
	if v.Type == "procedure" {
		first := v.Title
		if i := strings.Index(v.Title, " → "); i > 0 {
			first = v.Title[:i]
		}
		return fmt.Sprintf("Use when you reach for the `%s` workflow.", first)
	}
	kind := v.Kind
	if kind == "" {
		kind = "practice"
	}
	return fmt.Sprintf("Use as a %s when working in this repository.", kind)
}

func sha256Sum(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}
