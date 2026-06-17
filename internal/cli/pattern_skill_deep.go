package cli

import (
	"context"
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
	db, err := openPatternCorpusDB(brainDir)
	if err != nil {
		return deepSkillInput{}, false
	}
	defer db.Close()
	var jsonRedacted, verifierJSON, verdict string
	err = db.QueryRow(`SELECT json_redacted, COALESCE(verifier_json_redacted,''), COALESCE(verdict,'')
		FROM deep_dossiers WHERE pattern_id=?`, patternID).Scan(&jsonRedacted, &verifierJSON, &verdict)
	if err != nil || verdict != "accepted" {
		return deepSkillInput{}, false
	}
	var in deepSkillInput
	if json.Unmarshal([]byte(jsonRedacted), &in.rec) != nil {
		return deepSkillInput{}, false
	}
	_ = json.Unmarshal([]byte(verifierJSON), &in.verdict) // best-effort: required_edits etc.
	in.verdict.Verdict = verdict
	return in, true
}

const deepSkillSynthesisSystemPrompt = `You convert a VERIFIED skill dossier into a SKILL.md for THIS repository. The dossier was assembled from real session evidence and already PASSED an adversarial verifier (verdict: accepted). Your job is to render it faithfully — NOT to invent or infer a new procedure.

Rules:
- Preserve the dossier's trigger, canonical workflow (the ACTUAL commands), verification steps, and failure modes/recoveries. Do not add commands or claims the dossier does not contain.
- Apply the verifier's required edits/fixes if present.
- If, despite the accepted verdict, the dossier is plainly generic (nothing repo-specific or non-obvious — e.g. "git add then git push"), output EXACTLY one line: NOT_A_SKILL: <one-line reason>.

Otherwise output ONLY a complete SKILL.md (no surrounding prose, no code fences):
- YAML frontmatter with name (lowercase-hyphenated) and description. The description MUST state what it does and "Use when ..." with concrete trigger conditions from the dossier.
- Body in third-person imperative: Prerequisites (only if the dossier lists preconditions), a numbered Workflow using the dossier's exact commands, a Verification section from the dossier's verification moves, and a Gotchas/Failure modes section from the dossier's failure_modes (with recoveries).
- Be concise. Include only what the dossier supports.`

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
	fmt.Fprintf(&b, "Title: %s\n", rec.Title)
	fmt.Fprintf(&b, "Trigger / Use when: %s\n", rec.Trigger)
	if len(rec.Preconditions) > 0 {
		fmt.Fprintf(&b, "Preconditions:\n")
		for _, p := range rec.Preconditions {
			fmt.Fprintf(&b, "  - %s\n", p)
		}
	}
	if len(rec.Workflow) > 0 {
		fmt.Fprintf(&b, "Canonical workflow (exact commands):\n")
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
