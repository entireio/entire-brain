package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Procedure-skill source (PR #44 follow-up): skills from corrected episodes.
//
// A recurring sequence of commands a capable agent already knows is not a skill.
// The non-obvious, skill-worthy knowledge in a session history is the RECOVERY
// from a mistake: what failed, why, and the move that fixed it. This source
// samples corrected/failed episodes, asks an agent to group recurring
// failure→recovery LESSONS by meaning, and stores accepted ones as deep dossiers
// (archetype "procedure") under `lesson:` ids — reusing the deep_dossiers →
// accepted-dossier → convert-to-SKILL.md pipeline that themes/families use.

const lessonSampleCap = 50
const lessonMinMembers = 2

const lessonProposeSystemPrompt = `You are given a SAMPLE of CORRECTED or FAILED work episodes from a coding agent's session history — moments where the agent did something wrong (a failing command, a wrong approach the user corrected) and then recovered. Each episode has: an episode_key, the user's intent, the outcome, the failing command if any, and a redacted transcript excerpt.

The skill-worthy knowledge here is the RECOVERY: a recurring mistake-and-fix that a capable agent would otherwise repeat. Group episodes that share ONE coherent, recurring failure→recovery lesson — by MEANING, not wording. A lesson must be NON-OBVIOUS and repo/domain-specific; a generic mistake any competent agent already avoids is NOT a lesson. Leave unrelated episodes ungrouped.

Return EXACTLY one JSON object and nothing else (no prose, no code fences):
{
  "lessons": [
    {
      "title": "<short imperative title>",
      "trigger": "<the situation where this lesson applies — 'Use when ...'>",
      "not_when": "<when this lesson does NOT apply — a scope limit>",
      "failure": "<the recurring mistake>",
      "recovery": "<the move that fixes it>",
      "knowledge": ["<non-obvious fact or rule learned>", ...],
      "member_keys": ["<episode_key from the sample>", ...],
      "verdict": "accepted" | "low_confidence" | "rejected"
    }
  ]
}

Use "accepted" only for a genuinely non-obvious, recurring lesson with >=2 members. Use "rejected" for generic mistakes any agent already avoids. Do not invent episode_keys not in the sample.`

type proposedLesson struct {
	Title      string   `json:"title"`
	Trigger    string   `json:"trigger"`
	NotWhen    string   `json:"not_when"`
	Failure    string   `json:"failure"`
	Recovery   string   `json:"recovery"`
	Knowledge  []string `json:"knowledge"`
	MemberKeys []string `json:"member_keys"`
	Verdict    string   `json:"verdict"`
}

// proposeSkillLessons is the agent-only procedure-skill proposal phase (reached
// via `patterns verify --lessons`): it samples corrected/failed episodes, asks an
// agent to group recurring failure→recovery lessons by meaning, and stores
// accepted lessons as deep dossiers (archetype "procedure") under `lesson:` ids.
// Egress-gated; cached by the sample fingerprint; never from refresh.
func proposeSkillLessons(ctx context.Context, db *sql.DB, brainDir, repoDir, agent, model, effort string, run distillAgentRunner, now time.Time) (dossierVerifyStats, error) {
	var stats dossierVerifyStats
	if err := rejectAgentForNoEgress(agent); err != nil {
		return stats, err
	}
	sample, sampleKeys := sampleCorrectedEpisodes(db, brainDir)
	if len(sample) < lessonMinMembers {
		_, _ = db.Exec(`DELETE FROM deep_dossiers WHERE pattern_id LIKE 'lesson:%'`)
		return stats, nil
	}
	sampleFP := "sha256:" + hexSHA(strings.Join(sortedCopy(sampleKeys), "|"))
	countLessons := func() int {
		return corpusScalar(db, `SELECT COUNT(*) FROM deep_dossiers WHERE pattern_id LIKE 'lesson:%'`)
	}
	if corpusMeta(db, "lessons_sample_fingerprint") == sampleFP && countLessons() > 0 {
		stats.Cached = countLessons()
		stats.Considered = stats.Cached
		return stats, nil // unchanged sample → reuse prior proposals
	}

	args, err := distillAgentCommandArgs(agent, nil, lessonProposeSystemPrompt)
	if err != nil {
		return stats, err
	}
	args = injectAgentModel(args, agent, model)
	args = injectAgentEffort(args, agent, effort)

	payload, _ := json.Marshal(map[string]any{"episodes": sample})
	out, err := run(ctx, repoDir, args, []byte(redactText(string(payload))), dossierVerifyTimeout)
	if err != nil {
		return stats, fmt.Errorf("lesson proposal agent: %w", err)
	}
	proposed, err := parseProposedLessons(out)
	if err != nil {
		return stats, err
	}

	repoKey := corpusMeta(db, "repo_key")
	ts := now.UTC().Format(time.RFC3339)
	if _, err := db.Exec(`DELETE FROM deep_dossiers WHERE pattern_id LIKE 'lesson:%'`); err != nil {
		return stats, err
	}
	valid := map[string]bool{}
	for _, k := range sampleKeys {
		valid[k] = true
	}
	for _, pl := range proposed {
		stats.Considered++
		members := uniqueStrings(filterValidKeys(pl.MemberKeys, valid))
		verdict := strings.ToLower(strings.TrimSpace(pl.Verdict))
		if !allowedVerdicts[verdict] {
			verdict = "low_confidence"
		}
		if len(members) < lessonMinMembers || verdict == "rejected" {
			stats.Failed++
			continue
		}
		rec := lessonRecord(pl, members, repoKey)
		blob, _ := json.Marshal(rec)
		if _, err := db.Exec(`INSERT OR REPLACE INTO deep_dossiers
			(pattern_id, fingerprint, json_redacted, verdict, status, created_at, updated_at)
			VALUES (?,?,?,?, 'current', ?, ?)`, rec.PatternID, rec.Fingerprint, redactText(string(blob)), verdict, ts, ts); err != nil {
			return stats, err
		}
		if verdict == "accepted" {
			stats.Verified++
		}
	}
	_ = setCorpusMeta(db, map[string]string{"lessons_sample_fingerprint": sampleFP})
	return stats, nil
}

// lessonRecord renders an accepted lesson proposal into a procedure-archetype
// deep dossier (already truncated; redacted by the caller before storage).
func lessonRecord(pl proposedLesson, members []string, repoKey string) deepDossierRecord {
	id := "lesson:" + hexSHA("lesson\x00"+repoKey+"\x00"+strings.Join(sortedCopy(members), "|"))
	var knowledge []string
	for _, k := range pl.Knowledge {
		if s := strings.TrimSpace(k); s != "" {
			knowledge = append(knowledge, truncateString(s, 280))
		}
	}
	return deepDossierRecord{
		SchemaVersion:    deepSchemaVersion,
		PatternID:        id,
		Archetype:        "procedure",
		Fingerprint:      "sha256:" + hexSHA("lesson\x00"+strings.Join(sortedCopy(members), "|")),
		Title:            truncateString(strings.TrimSpace(firstNonEmpty(pl.Title, "lesson")), 120),
		Trigger:          truncateString(strings.TrimSpace(pl.Trigger), 280),
		NotWhen:          truncateString(strings.TrimSpace(pl.NotWhen), 280),
		Knowledge:        knowledge,
		FailureModes:     []deepFailureMode{{Failure: truncateString(strings.TrimSpace(pl.Failure), 280), Recovery: truncateString(strings.TrimSpace(pl.Recovery), 280)}},
		EvidenceEpisodes: len(members),
	}
}

func parseProposedLessons(out string) ([]proposedLesson, error) {
	raw := strings.TrimSpace(out)
	if i := strings.IndexByte(raw, '{'); i >= 0 {
		if j := strings.LastIndexByte(raw, '}'); j >= i {
			raw = raw[i : j+1]
		}
	}
	var wrap struct {
		Lessons []proposedLesson `json:"lessons"`
	}
	if err := json.Unmarshal([]byte(raw), &wrap); err != nil {
		return nil, fmt.Errorf("parse lesson proposal: %w", err)
	}
	return wrap.Lessons, nil
}

// sampleCorrectedEpisodes returns up to lessonSampleCap recent corrected/failed
// episodes as {episode_key, intent, outcome, failing_command, excerpt}
// (redacted), plus their keys — the raw material for failure→recovery lessons.
func sampleCorrectedEpisodes(db *sql.DB, brainDir string) ([]map[string]string, []string) {
	rows, err := db.Query(`
		SELECT e.id, e.episode_key, COALESCE(e.intent_raw,''), e.outcome, e.source_path, e.start_line, e.end_line
		FROM episodes e
		WHERE e.outcome IN ('corrected','failed')
		ORDER BY e.created_at DESC LIMIT ?`, lessonSampleCap)
	if err != nil {
		return nil, nil
	}
	defer rows.Close()
	type epRow struct {
		id, key, intent, outcome, path string
		start, end                     int
	}
	var all []epRow
	for rows.Next() {
		var r epRow
		if rows.Scan(&r.id, &r.key, &r.intent, &r.outcome, &r.path, &r.start, &r.end) != nil {
			continue
		}
		all = append(all, r)
	}
	rows.Close()

	var sample []map[string]string
	var keys []string
	for _, r := range all {
		failing := ""
		forEachCommand(db, []string{r.id}, func(head, _ string, failed bool) {
			if failed && failing == "" {
				failing = head
			}
		})
		ex := ""
		lines := r.end - r.start + 1
		if lines <= 0 || lines > themeMemberExcerptLines {
			lines = themeMemberExcerptLines
		}
		if t := transcriptExcerpt(brainDir, episodeAnchor{Path: r.path, Line: r.start}, lines, themeMemberExcerptBytes); t != "" {
			ex = redactText(t)
		}
		sample = append(sample, map[string]string{
			"episode_key":     r.key,
			"intent":          redactText(truncateString(r.intent, 160)),
			"outcome":         r.outcome,
			"failing_command": redactText(failing),
			"excerpt":         ex,
		})
		keys = append(keys, r.key)
	}
	return sample, keys
}
