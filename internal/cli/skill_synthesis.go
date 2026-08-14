package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// Skill synthesis (Pattern Consolidation, Phase 2 redo).
//
// Deterministic clustering FINDS recurring tasks; an agent AUTHORS the skill and
// REJECTS the generic. Given a task candidate's evidence (recurring intents, the
// commands actually run, reinforcement, session excerpts, and matching durable
// facts), the agent writes a yeet-shaped SKILL.md — or returns NOT_A_SKILL when
// the evidence holds nothing a competent model doesn't already know. That gate is
// the whole correction over the command-n-gram approach.

const skillSynthesisTimeout = 240 * time.Second

const skillSynthesisSystemPrompt = `You write Agent Skills: a SKILL.md that captures a RECURRING task in THIS repository so a future coding agent performs it correctly without rediscovering the specifics.

You are given evidence about one recurring task: the user requests that recur, the commands actually run, reinforcement outcomes, real session excerpts, and durable repository facts.

THE BAR (decisive): a skill must encode knowledge a competent model does NOT already have — this repo's exact commands/flags, conventions, gotchas, ordering, validation, and settled dead-ends. Generic knowledge (how to use git, how to write a loop, standard tool usage) is NOT a skill.

If the evidence contains nothing repo-specific or non-obvious worth encoding — including conversational fragments, plain generic tool use, OR an incoherent cluster where the sample requests are unrelated to one another — output EXACTLY one line and nothing else:
NOT_A_SKILL: <one-line reason>

Otherwise output ONLY a complete SKILL.md (no surrounding prose, no code fences):
- YAML frontmatter with name (lowercase-hyphenated) and description. The description MUST state what it does and "Use when ..." with concrete trigger conditions.
- A body in third-person imperative: Prerequisites (if any), a numbered Workflow using the ACTUAL commands from the evidence, Gotchas/dead-ends drawn from the facts, and a short Verification.
- Be concise. Include only what the evidence supports. Do not invent commands or facts not present in the evidence.`

type skillSynthesisResult struct {
	IsSkill   bool
	Reason    string
	SkillText string
	Name      string
}

// synthesizeSkill calls the agent to author (or reject) a skill for the task.
func synthesizeSkill(ctx context.Context, repoDir, brainDir string, c taskCandidate, agent, model, effort string, run distillAgentRunner) (skillSynthesisResult, error) {
	evidence, err := buildSkillEvidenceChecked(brainDir, c)
	if err != nil {
		return skillSynthesisResult{}, err
	}
	return synthesizeSkillWithEvidence(ctx, repoDir, evidence, agent, model, effort, run)
}

func synthesizeSkillWithEvidence(ctx context.Context, repoDir, evidence, agent, model, effort string, run distillAgentRunner) (skillSynthesisResult, error) {
	args, err := distillAgentCommandArgs(agent, nil, skillSynthesisSystemPrompt)
	if err != nil {
		return skillSynthesisResult{}, err
	}
	args = injectAgentModel(args, agent, model)
	args = injectAgentEffort(args, agent, effort)
	out, err := run(ctx, repoDir, args, []byte(evidence), skillSynthesisTimeout)
	if err != nil {
		return skillSynthesisResult{}, fmt.Errorf("synthesis agent: %w", err)
	}
	return parseSynthesisOutput(out), nil
}

func parseSynthesisOutput(out string) skillSynthesisResult {
	text := strings.TrimSpace(out)
	// Strip a wrapping code fence if the agent added one.
	if strings.HasPrefix(text, "```") {
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[i+1:]
		}
		text = strings.TrimSuffix(strings.TrimRight(text, "\n"), "```")
		text = strings.TrimSpace(text)
	}
	if strings.HasPrefix(strings.ToUpper(text), "NOT_A_SKILL") {
		reason := strings.TrimSpace(strings.TrimPrefix(text, "NOT_A_SKILL"))
		reason = strings.TrimPrefix(reason, ":")
		return skillSynthesisResult{IsSkill: false, Reason: strings.TrimSpace(reason)}
	}
	return skillSynthesisResult{IsSkill: true, SkillText: text, Name: skillNameFromFrontmatter(text)}
}

func skillNameFromFrontmatter(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "name:") {
			name := strings.TrimSpace(strings.TrimPrefix(line, "name:"))
			return strings.Trim(name, `"'`)
		}
		if line == "---" && strings.Contains(text, "name:") {
			continue
		}
	}
	return ""
}

// buildSkillEvidence assembles the deterministic evidence bundle handed to the
// synthesis agent: recurring intents, commands, reinforcement, matching durable
// facts, and real session excerpts.
func buildSkillEvidence(brainDir string, c taskCandidate) string {
	evidence, _ := buildSkillEvidenceChecked(brainDir, c)
	return evidence
}

func buildSkillEvidenceChecked(brainDir string, c taskCandidate) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "RECURRING TASK in repo %s\n", c.RepoKey)
	fmt.Fprintf(&b, "Intent signature: %s\n", c.IntentSignature)
	fmt.Fprintf(&b, "Observed in %d session(s); %d ran commands. Reinforcement: %d succeeded, %d corrected, %d neutral.\n\n",
		c.Support, c.WithCommands, c.Reinforcement.Success, c.Reinforcement.Corrected, c.Reinforcement.Neutral)

	if len(c.SampleIntents) > 0 {
		fmt.Fprintf(&b, "Sample user requests:\n")
		for _, s := range c.SampleIntents {
			fmt.Fprintf(&b, "- %s\n", s)
		}
		b.WriteByte('\n')
	}
	if len(c.Commands) > 0 {
		fmt.Fprintf(&b, "Most frequent commands run (normalized):\n")
		for _, cmd := range c.Commands {
			fmt.Fprintf(&b, "- %s\n", cmd)
		}
		b.WriteByte('\n')
	}

	if len(c.Procedures) > 0 {
		fmt.Fprintf(&b, "Co-occurring command shapes (specificity in [0,1]):\n")
		for _, p := range c.Procedures {
			fmt.Fprintf(&b, "- %s  (in %d sessions, specificity %.2f)\n", strings.Join(p.Commands, " → "), p.Count, p.Specificity)
		}
		b.WriteByte('\n')
	}

	if len(c.MatchingFacts) > 0 {
		fmt.Fprintf(&b, "Relevant durable repository facts:\n")
		for _, f := range c.MatchingFacts {
			fmt.Fprintf(&b, "- %s\n", f)
		}
		b.WriteByte('\n')
	}

	excerpts := 0
	for _, anchor := range c.Examples {
		if excerpts >= 2 {
			break
		}
		ex, err := transcriptExcerptChecked(brainDir, anchor, 80, 3000)
		if err != nil {
			return "", err
		}
		if ex != "" {
			fmt.Fprintf(&b, "Session excerpt (%s:%d):\n%s\n\n", anchor.Path, anchor.Line, ex)
			excerpts++
		}
	}
	// Redaction boundary: this bundle is the agent's input — strip secrets and
	// home-dir usernames before it leaves the brain.
	return redactText(b.String()), nil
}

// transcriptExcerpt returns up to maxLines of a transcript starting at the
// anchor line, capped at maxBytes — the raw evidence of what the agent did.
func transcriptExcerpt(brainDir string, anchor episodeAnchor, maxLines, maxBytes int) string {
	excerpt, _ := transcriptExcerptChecked(brainDir, anchor, maxLines, maxBytes)
	return excerpt
}

func transcriptExcerptChecked(brainDir string, anchor episodeAnchor, maxLines, maxBytes int) (string, error) {
	data, err := readCanonicalHistoryTranscript(context.Background(), brainDir, anchor.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", &historySessionInventoryError{
				Path:   anchor.Path,
				Reason: "pattern evidence transcript is missing; refusing partial skill synthesis",
				Err:    err,
			}
		}
		return "", err
	}
	content := string(data)
	lines := strings.Split(content, "\n")
	start := anchor.Line - 1
	if start < 0 {
		start = 0
	}
	if start >= len(lines) {
		return "", nil
	}
	end := start + maxLines
	if end > len(lines) {
		end = len(lines)
	}
	ex := strings.Join(lines[start:end], "\n")
	return truncateString(ex, maxBytes), nil
}
