package cli

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	clibase "github.com/entireio/entire-brain/internal/cli/internal/base"
	patterncore "github.com/entireio/entire-brain/internal/cli/patterns"
	"github.com/entireio/entire-brain/internal/cli/workspace"
)

// buildWorkspacePatternCorpus runs the shared workspace corpus build.
func buildWorkspacePatternCorpus(env EntireEnv, manifest workspace.Manifest, now time.Time) (patterncore.WorkspaceCorpusCounts, error) {
	return patterncore.BuildWorkspaceCorpus(env, manifest, now)
}

// getWorkspaceCorpusRecord resolves a bare workspace-aggregate id (pattern: or
// theme:) from the workspace corpus, so the ids `workspace patterns` prints are
// directly fetchable via `workspace get <ws> <id>`. Graceful: false when absent.
func getWorkspaceCorpusRecord(env EntireEnv, workspaceName, id string) (unifiedResult, bool) {
	result, ok, _ := getWorkspaceCorpusRecordChecked(env, workspaceName, id)
	return result, ok
}

func getWorkspaceCorpusRecordChecked(env EntireEnv, workspaceName, id string) (unifiedResult, bool, error) {
	wsBrainDir, err := workspace.DefaultStore(env).Dir(workspaceName)
	if err != nil {
		return unifiedResult{}, false, err
	}
	switch {
	case strings.HasPrefix(id, "pattern:"):
		return getWorkspacePatternChecked(wsBrainDir, id)
	case strings.HasPrefix(id, "theme:"):
		return getCorpusThemeChecked(wsBrainDir, id)
	default:
		return unifiedResult{}, false, nil
	}
}

// getWorkspacePattern renders a workspace-scope aggregate pattern (with its
// per-repo breakdown) as a unifiedResult.
func getWorkspacePattern(wsBrainDir, patternID string) (unifiedResult, bool) {
	result, ok, _ := getWorkspacePatternChecked(wsBrainDir, patternID)
	return result, ok
}

func getWorkspacePatternChecked(wsBrainDir, patternID string) (unifiedResult, bool, error) {
	db, present, err := patterncore.OpenCorpusReadDBIfPresent(wsBrainDir)
	if err != nil {
		return unifiedResult{}, false, err
	}
	if !present {
		return unifiedResult{}, false, nil
	}
	defer db.Close()
	var typ, title, intentSig, gram, metaID string
	var strength float64
	var nRepos int
	err = db.QueryRow(`SELECT type, title, COALESCE(intent_sig,''), COALESCE(gram,''), COALESCE(meta_id,''), strength, n_repos
		FROM patterns WHERE id=? AND scope='workspace'`, patternID).Scan(&typ, &title, &intentSig, &gram, &metaID, &strength, &nRepos)
	if err != nil {
		if err == sql.ErrNoRows {
			return unifiedResult{}, false, nil
		}
		return unifiedResult{}, false, err
	}
	var b strings.Builder
	b.WriteString(clibase.RedactText(title) + "\n")
	b.WriteString("workspace " + typ + " across " + strconv.Itoa(nRepos) + " repo(s); strength " +
		strconv.FormatFloat(strength, 'g', -1, 64) + "\n")
	rows, err := db.Query(`SELECT repo_key, support FROM workspace_pattern_repos WHERE pattern_id=? ORDER BY repo_key`, patternID)
	if err == nil {
		for rows.Next() {
			var rk string
			var support int
			if rows.Scan(&rk, &support) == nil {
				b.WriteString("  " + clibase.RedactText(rk) + " (" + strconv.Itoa(support) + ")\n")
			}
		}
		rows.Close()
	}
	return unifiedResult{Source: "workspace_pattern", ID: patternID, Text: clibase.RedactText(strings.TrimRight(b.String(), "\n"))}, true, nil
}

const workspaceFamilySkillSystemPrompt = `You convert a VERIFIED cross-repo workspace family into a SKILL.md for a multi-repo workspace. The family was assembled from real per-repo evidence and accepted by a verifier.

The family JSON gives: title, trigger, purpose, common_workflow (repo-neutral steps), and per_repo (each repo's ACTUAL commands). Render it faithfully — do not invent.

Rules:
- The skill MUST describe what is COMMON across repos AND what VARIES per repo (a per-repo variations section listing each repo's commands).
- If the family is plainly generic (git add/commit/push/status), output EXACTLY one line: NOT_A_SKILL: <reason>.

Otherwise output ONLY a complete SKILL.md (no fences):
- YAML frontmatter: name (lowercase-hyphenated) + description with a concrete "Use when ..." from the trigger.
- Body: a Workflow section (the common steps) and a "Per-repo variations" section naming each repo and its commands. Be concise; include only what the family supports.`

// synthesizeWorkspaceSkill converts an accepted family into a SKILL.md.
func synthesizeWorkspaceSkill(ctx context.Context, repoDir string, f patterncore.WorkspaceFamily, agent, model, effort string, run clibase.DistillAgentRunner) (skillSynthesisResult, error) {
	args, err := clibase.DistillAgentCommandArgs(agent, nil, workspaceFamilySkillSystemPrompt)
	if err != nil {
		return skillSynthesisResult{}, err
	}
	args = clibase.InjectAgentModel(args, agent, model)
	args = clibase.InjectAgentEffort(args, agent, effort)
	out, err := run(ctx, repoDir, args, []byte(patterncore.BuildWorkspaceFamilyEvidence(f)), skillSynthesisTimeout)
	if err != nil {
		return skillSynthesisResult{}, fmt.Errorf("workspace family synthesis agent: %w", err)
	}
	return parseSynthesisOutput(out), nil
}
