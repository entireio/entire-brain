package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Workspace semantic merge (SR5).
//
// Exact (type,intent_sig,gram,meta_id) aggregation stays as a cheap signal, but
// equivalent cross-repo workflows that differ in command spelling/order never
// merge that way, and generic exact matches can survive. SR5 adds an agent
// judgment layer: it groups each member repo's promotable task candidates into
// cross-repo FAMILIES by semantic trigger + procedure purpose, records what is
// common and what varies per repo, and requires an accepted workspace family
// dossier before a workspace skill can be formed. Explicit (`workspace patterns
// verify`), egress-gated, cached by the member-candidate fingerprint.

const workspaceFamilyMaxPerRepo = 15 // cap member candidates handed to the agent per repo

type familyRepoVariant struct {
	RepoKey  string   `json:"repo_key"`
	Commands []string `json:"commands"`
}

type workspaceFamily struct {
	ID             string              `json:"id"`
	Title          string              `json:"title"`
	Trigger        string              `json:"trigger"`
	Purpose        string              `json:"purpose"`
	CommonWorkflow []string            `json:"common_workflow"`
	PerRepo        []familyRepoVariant `json:"per_repo"`
	Verdict        string              `json:"verdict"`
}

func (f workspaceFamily) repoCount() int {
	seen := map[string]bool{}
	for _, v := range f.PerRepo {
		if v.RepoKey != "" {
			seen[v.RepoKey] = true
		}
	}
	return len(seen)
}

const workspaceFamilyProposeSystemPrompt = `You are given task workflows from MULTIPLE repositories in a workspace, grouped per repo (each with its trigger/intent and the actual commands run).

Group workflows that share the same TRIGGER and PROCEDURE PURPOSE into cross-repo families — even when the exact commands differ in spelling or order across repos (e.g. "mise deploy" in one repo and "make release" in another are the same release workflow). Judge by MEANING, not by command string.

Rules:
- A family must span at least 2 distinct repos.
- Reject GENERIC workflows that are not repo-specific practice (plain git add/commit/push/status/diff, ls, cat) — those are not families.
- Capture what is COMMON (a repo-neutral workflow) and what VARIES per repo (each repo's actual commands).

Return EXACTLY one JSON object and nothing else (no prose, no code fences):
{
  "families": [
    {
      "title": "<short title>",
      "trigger": "<when this applies / Use when>",
      "purpose": "<the procedure purpose>",
      "common_workflow": ["<repo-neutral step>", ...],
      "per_repo": [ {"repo_key": "<repo>", "commands": ["<actual command>", ...]}, ... ],
      "verdict": "accepted" | "rejected" | "needs_split" | "low_confidence"
    }
  ]
}
Use "accepted" only for a genuine cross-repo family spanning >=2 repos. Do not invent repo keys not provided.`

// proposeWorkspaceFamilies runs the agent judgment layer over member-repo task
// candidates and stores accepted cross-repo families as workspace dossiers.
func proposeWorkspaceFamilies(ctx context.Context, env EntireEnv, manifest workspaceManifest, repoDir, agent, model, effort string, run distillAgentRunner, now time.Time) (dossierVerifyStats, error) {
	var stats dossierVerifyStats
	if err := rejectAgentForNoEgress(agent); err != nil {
		return stats, err
	}
	wsBrainDir, err := workspaceDir(env, manifest.Name)
	if err != nil {
		return stats, err
	}
	err = withPatternCorpusMutationLocked(wsBrainDir, func(db *sql.DB) error {
		var mutationErr error
		stats, mutationErr = proposeWorkspaceFamiliesInDB(ctx, db, env, manifest, repoDir, agent, model, effort, run, now)
		return mutationErr
	})
	if err != nil {
		return stats, fmt.Errorf("workspace corpus not built (run `entire brain workspace patterns refresh %s`): %w", manifest.Name, err)
	}
	return stats, nil
}

func proposeWorkspaceFamiliesInDB(ctx context.Context, db *sql.DB, env EntireEnv, manifest workspaceManifest, repoDir, agent, model, effort string, run distillAgentRunner, now time.Time) (dossierVerifyStats, error) {
	var stats dossierVerifyStats
	// Gather each member's promotable task candidates (the cheap exact signal feeds
	// the agent, which merges across repos by meaning).
	perRepo := map[string][]map[string]any{}
	contributing := 0
	repos := append([]workspaceRepo(nil), manifest.Repos...)
	sort.Slice(repos, func(i, j int) bool { return repos[i].RepoKey < repos[j].RepoKey })
	for _, repo := range repos {
		memberDir, err := brainDirForKey(env, repo.RepoKey)
		if err != nil {
			continue
		}
		cands, ok, err := loadCorpusTaskCandidatesChecked(memberDir)
		if err != nil {
			return stats, err
		}
		if !ok || len(cands) == 0 {
			continue
		}
		var list []map[string]any
		for i, c := range cands {
			if i >= workspaceFamilyMaxPerRepo {
				break
			}
			r := c.Reinforcement
			list = append(list, map[string]any{
				"intent": c.IntentSignature, "title": redactText(c.Label), "commands": redactStrings(c.Commands),
				"support": c.Support, "reinforcement": []int{r.Success, r.Corrected, r.Neutral},
			})
		}
		if len(list) > 0 {
			perRepo[repo.RepoKey] = list
			contributing++
		}
	}
	if contributing < workspaceMinRepos {
		return stats, nil // nothing can be cross-repo
	}

	// Fingerprint the actual evidence payload (per-repo intent + title + commands +
	// support + reinforcement), so changed member evidence invalidates the cached
	// families even when the set of repos/intents is unchanged.
	payload, _ := json.Marshal(map[string]any{"workspace": manifest.Name, "repos": perRepo})
	fp := proposalSampleFingerprint(payload)
	if corpusMeta(db, "workspace_families_fingerprint") == fp &&
		corpusScalar(db, `SELECT COUNT(*) FROM deep_dossiers WHERE pattern_id LIKE 'family:%'`) > 0 {
		stats.Cached = corpusScalar(db, `SELECT COUNT(*) FROM deep_dossiers WHERE pattern_id LIKE 'family:%'`)
		stats.Considered = stats.Cached
		return stats, nil
	}

	args, err := distillAgentCommandArgs(agent, nil, workspaceFamilyProposeSystemPrompt)
	if err != nil {
		return stats, err
	}
	args = injectAgentModel(args, agent, model)
	args = injectAgentEffort(args, agent, effort)
	out, err := run(ctx, repoDir, args, []byte(redactText(string(payload))), dossierVerifyTimeout)
	if err != nil {
		return stats, fmt.Errorf("workspace family proposal agent: %w", err)
	}
	families, err := parseWorkspaceFamilies(out)
	if err != nil {
		return stats, err
	}

	members := map[string]bool{}
	for k := range perRepo {
		members[k] = true
	}
	ts := now.UTC().Format(time.RFC3339)
	prepared := make([]workspaceFamily, 0, len(families))
	for _, f := range families {
		stats.Considered++
		// Curate per-repo variants to real members; require >=2 distinct repos.
		var variants []familyRepoVariant
		for _, v := range f.PerRepo {
			if members[v.RepoKey] {
				variants = append(variants, familyRepoVariant{RepoKey: v.RepoKey, Commands: redactStrings(v.Commands)})
			}
		}
		f.PerRepo = variants
		if f.repoCount() < workspaceMinRepos {
			stats.Failed++
			continue
		}
		f.Verdict = strings.ToLower(strings.TrimSpace(f.Verdict))
		if !allowedVerdicts[f.Verdict] {
			// A missing/unknown verdict must never be treated as accepted — it would
			// surface an unverified family. Default to the softest non-accepting verdict.
			f.Verdict = "low_confidence"
		}
		f.ID = "family:" + hexSHA(manifest.Name+"\x00"+f.Title+"\x00"+familyRepoKeysJoined(f))
		f.Title = redactText(f.Title)
		prepared = append(prepared, f)
		if f.Verdict == "accepted" {
			stats.Verified++
		}
	}
	tx, err := db.Begin()
	if err != nil {
		return stats, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM deep_dossiers WHERE pattern_id LIKE 'family:%'`); err != nil {
		return stats, err
	}
	for _, f := range prepared {
		blob, err := json.Marshal(f)
		if err != nil {
			return stats, err
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO deep_dossiers
			(pattern_id, fingerprint, json_redacted, verdict, status, created_at, updated_at)
			VALUES (?,?,?,?, 'current', ?, ?)`, f.ID, fp, redactText(string(blob)), f.Verdict, ts, ts); err != nil {
			return stats, err
		}
	}
	if _, err := tx.Exec(`INSERT INTO meta(key, value) VALUES ('workspace_families_fingerprint', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, fp); err != nil {
		return stats, err
	}
	return stats, tx.Commit()
}

func parseWorkspaceFamilies(out string) ([]workspaceFamily, error) {
	raw := strings.TrimSpace(out)
	if i := strings.IndexByte(raw, '{'); i >= 0 {
		if j := strings.LastIndexByte(raw, '}'); j >= i {
			raw = raw[i : j+1]
		}
	}
	var wrap struct {
		Families []workspaceFamily `json:"families"`
	}
	if err := json.Unmarshal([]byte(raw), &wrap); err != nil {
		return nil, fmt.Errorf("parse workspace families: %w", err)
	}
	return wrap.Families, nil
}

func familyRepoKeysJoined(f workspaceFamily) string {
	keys := make([]string, 0, len(f.PerRepo))
	for _, v := range f.PerRepo {
		keys = append(keys, v.RepoKey)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// loadAcceptedWorkspaceFamilies returns the accepted cross-repo families.
func loadAcceptedWorkspaceFamilies(wsBrainDir string) []workspaceFamily {
	families, _ := loadAcceptedWorkspaceFamiliesChecked(wsBrainDir)
	return families
}

func loadAcceptedWorkspaceFamiliesChecked(wsBrainDir string) ([]workspaceFamily, error) {
	db, present, err := openPatternCorpusReadDBIfPresent(wsBrainDir)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, nil
	}
	defer db.Close()
	rows, err := db.Query(`SELECT json_redacted FROM deep_dossiers WHERE pattern_id LIKE 'family:%' AND verdict='accepted' ORDER BY pattern_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []workspaceFamily
	for rows.Next() {
		var blob string
		if err := rows.Scan(&blob); err != nil {
			return out, err
		}
		var f workspaceFamily
		if json.Unmarshal([]byte(blob), &f) == nil {
			out = append(out, f)
		}
	}
	return out, rows.Err()
}

// getAcceptedWorkspaceFamily returns one accepted family by id.
func getAcceptedWorkspaceFamily(wsBrainDir, id string) (workspaceFamily, bool) {
	family, ok, _ := getAcceptedWorkspaceFamilyChecked(wsBrainDir, id)
	return family, ok
}

func getAcceptedWorkspaceFamilyChecked(wsBrainDir, id string) (workspaceFamily, bool, error) {
	families, err := loadAcceptedWorkspaceFamiliesChecked(wsBrainDir)
	if err != nil {
		return workspaceFamily{}, false, err
	}
	for _, f := range families {
		if f.ID == id {
			return f, true, nil
		}
	}
	return workspaceFamily{}, false, nil
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
func synthesizeWorkspaceSkill(ctx context.Context, repoDir string, f workspaceFamily, agent, model, effort string, run distillAgentRunner) (skillSynthesisResult, error) {
	args, err := distillAgentCommandArgs(agent, nil, workspaceFamilySkillSystemPrompt)
	if err != nil {
		return skillSynthesisResult{}, err
	}
	args = injectAgentModel(args, agent, model)
	args = injectAgentEffort(args, agent, effort)
	out, err := run(ctx, repoDir, args, []byte(buildWorkspaceFamilyEvidence(f)), skillSynthesisTimeout)
	if err != nil {
		return skillSynthesisResult{}, fmt.Errorf("workspace family synthesis agent: %w", err)
	}
	return parseSynthesisOutput(out), nil
}

func buildWorkspaceFamilyEvidence(f workspaceFamily) string {
	var b strings.Builder
	fmt.Fprintf(&b, "VERIFIED CROSS-REPO WORKSPACE FAMILY (verdict: %s)\n", nonEmptyOr(f.Verdict, "accepted"))
	fmt.Fprintf(&b, "Title: %s\n", f.Title)
	fmt.Fprintf(&b, "Trigger / Use when: %s\n", f.Trigger)
	if f.Purpose != "" {
		fmt.Fprintf(&b, "Purpose: %s\n", f.Purpose)
	}
	if len(f.CommonWorkflow) > 0 {
		fmt.Fprintf(&b, "Common workflow (repo-neutral):\n")
		for i, s := range f.CommonWorkflow {
			fmt.Fprintf(&b, "  %d. %s\n", i+1, s)
		}
	}
	fmt.Fprintf(&b, "Per-repo variations (what differs):\n")
	for _, v := range f.PerRepo {
		fmt.Fprintf(&b, "  - %s: %s\n", v.RepoKey, strings.Join(v.Commands, " → "))
	}
	return redactText(b.String())
}
