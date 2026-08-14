package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// Dossier-driven skill synthesis (PR #44 follow-up).
//
// Skills are formed from VERIFIED deep dossiers, not raw corpus rows + shallow
// snippets. A skill proposal requires an accepted deep dossier; the synthesis
// agent CONVERTS that verified dossier into SKILL.md (preserving trigger,
// workflow, verification, failure modes, and applying the verifier's required
// edits) rather than re-inferring a procedure from scratch.

// deepSkillInput bundles the accepted deep dossier with its verifier verdict so
// the writer has the full verified source material (including required fixes).
type deepSkillInput struct {
	rec     deepDossierRecord
	verdict dossierVerdict
}

// loadAcceptedDeepDossier returns the accepted deep dossier for a pattern (with
// its verifier verdict), or ok=false when none exists / it is not accepted.
func loadAcceptedDeepDossier(brainDir, patternID string) (deepSkillInput, bool) {
	input, ok, _ := loadAcceptedDeepDossierChecked(brainDir, patternID)
	return input, ok
}

func loadAcceptedDeepDossierChecked(brainDir, patternID string) (deepSkillInput, bool, error) {
	db, present, err := openPatternCorpusReadDBIfPresent(brainDir)
	if err != nil {
		return deepSkillInput{}, false, err
	}
	if !present {
		return deepSkillInput{}, false, nil
	}
	defer db.Close()
	if ok, err := patternCorpusHasTable(db.DB, "deep_dossiers"); err != nil || !ok {
		return deepSkillInput{}, false, err
	}
	var jsonRedacted, verifierJSON, verdict string
	err = db.QueryRow(`SELECT json_redacted, COALESCE(verifier_json_redacted,''), COALESCE(verdict,'')
		FROM deep_dossiers WHERE pattern_id=?`, patternID).Scan(&jsonRedacted, &verifierJSON, &verdict)
	if err != nil || verdict != "accepted" {
		if err != nil && err != sql.ErrNoRows {
			return deepSkillInput{}, false, err
		}
		return deepSkillInput{}, false, nil
	}
	var in deepSkillInput
	if json.Unmarshal([]byte(jsonRedacted), &in.rec) != nil {
		return deepSkillInput{}, false, nil
	}
	_ = json.Unmarshal([]byte(verifierJSON), &in.verdict) // best-effort: required_edits etc.
	in.verdict.Verdict = verdict
	return in, true, nil
}

const deepSkillSynthesisSystemPrompt = `You convert a VERIFIED skill dossier into a SKILL.md for THIS repository. The dossier was assembled from real session evidence and already PASSED an adversarial verifier (verdict: accepted). Your job is to render it faithfully — NOT to invent or infer new content.

A real skill encodes NON-OBVIOUS, repo-specific knowledge a capable agent would not already know: a hard-won recovery from a mistake, a local convention or gotcha, a constraint that is not derivable from reading the code. A skill is NOT a list of generic commands the agent already knows (e.g. "git add then git push", "go test then commit"). If, despite the accepted verdict, the dossier reduces to generic commands plus discipline with no encoded knowledge, output EXACTLY one line: NOT_A_SKILL: <one-line reason>.

The dossier has an "archetype" field that selects the shape:
- "procedure": a recurring failure→recovery lesson. Lead with the gotcha and the recovery; the commands are secondary context.
- "capability": a non-obvious convention / domain knowledge. Lead with the knowledge and when it applies.
- "" (legacy): a command workflow; treat the command sequence as SUPPORTING EVIDENCE, and only emit a skill if there is genuine non-obvious knowledge around it — otherwise NOT_A_SKILL.

Otherwise output ONLY a complete SKILL.md (no surrounding prose, no code fences):
- YAML frontmatter with name (lowercase-hyphenated) and a description that MUST contain BOTH a "Use when ..." clause (concrete trigger conditions from the dossier) AND a "Do NOT use when ..." clause (the negative trigger from the dossier's not_when, or a sensible scope limit). Claude under-triggers skills, so the description must be specific and pushy.
- Body in third-person imperative, drawn ONLY from the dossier: the non-obvious knowledge / lesson first; then any concrete commands as a Quick reference (not the spine); a Verification section if the dossier has verification moves; and a "Gotchas / Red flags" section from the dossier's failure_modes (with recoveries) and knowledge. Apply the verifier's required edits if present.
- Be concise. Include only what the dossier supports. Do not pad with generic advice.`

// synthesizeSkillFromDossier converts an accepted deep dossier into a SKILL.md
// via the agent. The agent renders the verified dossier; it does not re-discover.
func synthesizeSkillFromDossier(ctx context.Context, repoDir string, in deepSkillInput, agent, model, effort string, run distillAgentRunner) (skillSynthesisResult, error) {
	args, err := distillAgentCommandArgs(agent, nil, deepSkillSynthesisSystemPrompt)
	if err != nil {
		return skillSynthesisResult{}, err
	}
	args = injectAgentModel(args, agent, model)
	args = injectAgentEffort(args, agent, effort)
	out, err := run(ctx, repoDir, args, []byte(buildDeepSkillEvidence(in)), skillSynthesisTimeout)
	if err != nil {
		return skillSynthesisResult{}, fmt.Errorf("dossier synthesis agent: %w", err)
	}
	return parseSynthesisOutput(out), nil
}

// buildDeepSkillEvidence renders the accepted deep dossier as the synthesis
// input: trigger, preconditions, canonical workflow, variations, verification,
// failure modes + recoveries, parameters, facts, verifier required edits, and
// redacted evidence excerpts. Already redacted at rest; re-redacted on egress.
func buildDeepSkillEvidence(in deepSkillInput) string {
	rec := in.rec
	var b strings.Builder
	fmt.Fprintf(&b, "VERIFIED SKILL DOSSIER (verifier verdict: %s)\n", nonEmptyOr(in.verdict.Verdict, "accepted"))
	if rec.Archetype != "" {
		fmt.Fprintf(&b, "Archetype: %s\n", rec.Archetype)
	}
	fmt.Fprintf(&b, "Title: %s\n", rec.Title)
	fmt.Fprintf(&b, "Trigger / Use when: %s\n", rec.Trigger)
	if rec.NotWhen != "" {
		fmt.Fprintf(&b, "Do NOT use when: %s\n", rec.NotWhen)
	}
	if len(rec.Knowledge) > 0 {
		fmt.Fprintf(&b, "Non-obvious knowledge (the spine of this skill):\n")
		for _, k := range rec.Knowledge {
			fmt.Fprintf(&b, "  - %s\n", k)
		}
	}
	if len(rec.Preconditions) > 0 {
		fmt.Fprintf(&b, "Preconditions:\n")
		for _, p := range rec.Preconditions {
			fmt.Fprintf(&b, "  - %s\n", p)
		}
	}
	if len(rec.Workflow) > 0 {
		// Command sequences are SUPPORTING EVIDENCE, not the skill's spine — a
		// recurring sequence of commands the agent already knows is not, by
		// itself, a skill (see archetype guidance in the synthesis prompt).
		label := "Observed commands (supporting evidence / quick reference — NOT the skill by themselves)"
		if rec.Archetype == "" {
			label = "Observed command workflow (supporting evidence; emit a skill only if non-obvious knowledge surrounds it)"
		}
		fmt.Fprintf(&b, "%s:\n", label)
		for i, w := range rec.Workflow {
			fmt.Fprintf(&b, "  %d. %s\n", i+1, w)
		}
	}
	if len(rec.Variations) > 0 {
		fmt.Fprintf(&b, "Observed variations:\n")
		for _, v := range rec.Variations {
			fmt.Fprintf(&b, "  - %s\n", v)
		}
	}
	if len(rec.Verification) > 0 {
		fmt.Fprintf(&b, "Verification moves: %s\n", strings.Join(rec.Verification, ", "))
	}
	if len(rec.FailureModes) > 0 {
		fmt.Fprintf(&b, "Failure modes & recoveries:\n")
		for _, fm := range rec.FailureModes {
			line := "  - " + fm.Failure
			if fm.Recovery != "" {
				line += " → recovery: " + fm.Recovery
			}
			fmt.Fprintln(&b, line)
		}
	}
	if len(rec.Parameters) > 0 {
		fmt.Fprintf(&b, "Parameters / what varies:\n")
		for _, p := range rec.Parameters {
			fmt.Fprintf(&b, "  - %s\n", p)
		}
	}
	if len(rec.Facts) > 0 {
		fmt.Fprintf(&b, "Relevant durable facts: %s\n", strings.Join(rec.Facts, "; "))
	}
	if len(in.verdict.RequiredEdits) > 0 {
		fmt.Fprintf(&b, "Verifier-required edits (apply these):\n")
		for _, e := range in.verdict.RequiredEdits {
			fmt.Fprintf(&b, "  - %s\n", e)
		}
	}
	// Provenance: list the source anchors (transcript:line, outcome) even when a
	// transcript excerpt is unavailable, so the dossier always carries where the
	// evidence came from — not just the agent's proposal summary.
	if len(rec.SourceAnchors) > 0 {
		fmt.Fprintf(&b, "Source anchors (provenance):\n")
		for i, a := range rec.SourceAnchors {
			if i >= 12 {
				break
			}
			loc := a.Transcript
			if loc == "" {
				loc = a.SessionID
			}
			fmt.Fprintf(&b, "  - %s:%d (%s)\n", loc, a.StartLine, a.Outcome)
		}
	}
	excerpts := 0
	for _, a := range rec.SourceAnchors {
		if a.Excerpt == "" || excerpts >= 3 {
			continue
		}
		fmt.Fprintf(&b, "Evidence (%s, %s):\n%s\n", a.SessionID, a.Outcome, truncateString(a.Excerpt, 1200))
		excerpts++
	}
	return redactText(b.String())
}
