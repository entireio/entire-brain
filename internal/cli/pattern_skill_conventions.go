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

// Capability-skill source (PR #44 follow-up): skills from durable conventions.
//
// The other non-obvious source (besides corrected-episode lessons) is durable
// knowledge the brain already distilled: gotchas, conventions, and invariants —
// repo-specific rules a capable agent would not derive from reading the code.
// This source samples those facts (preferring ones corroborated by real
// episodes), asks an agent to keep only the genuinely non-obvious ones and group
// related facts into capability skills, and stores accepted ones as deep dossiers
// (archetype "capability") under `convention:` ids — reusing the same
// deep_dossiers → accepted-dossier → convert-to-SKILL.md pipeline.

const conventionSampleCap = 60

// capabilityFactKinds are the durable-fact kinds that encode non-obvious,
// skill-worthy knowledge (as opposed to decisions/preferences, which are
// contextual rather than reusable capability).
var capabilityFactKinds = map[string]bool{
	"gotcha": true, "convention": true, "invariant": true,
}

const conventionProposeSystemPrompt = `You are given a SAMPLE of DURABLE FACTS distilled from a repository's history — gotchas, conventions, and invariants. Each fact has: a fact_id, its kind, the rule text, the code locus it concerns, and how many work episodes referenced it (corroboration).

A capability skill encodes NON-OBVIOUS, repo-specific knowledge a capable agent would NOT already know or derive from reading the code (a hidden coupling, a local convention, a constraint with a non-obvious reason). A fact that merely restates something obvious, or generic best practice, is NOT skill-worthy. Group facts that belong to ONE coherent capability — by MEANING — and drop the rest.

Return EXACTLY one JSON object and nothing else (no prose, no code fences):
{
  "conventions": [
    {
      "title": "<short title for the capability>",
      "trigger": "<when this knowledge applies — 'Use when ...'>",
      "not_when": "<when it does NOT apply — a scope limit>",
      "knowledge": ["<the non-obvious rule, stated actionably>", ...],
      "member_fact_ids": ["<fact_id from the sample>", ...],
      "verdict": "accepted" | "low_confidence" | "rejected"
    }
  ]
}

Use "accepted" only for genuinely non-obvious, actionable repo knowledge backed by >=1 corroborated fact. Use "rejected" for obvious or generic facts. Do not invent fact_ids not in the sample.`

type proposedConvention struct {
	Title         string   `json:"title"`
	Trigger       string   `json:"trigger"`
	NotWhen       string   `json:"not_when"`
	Knowledge     []string `json:"knowledge"`
	MemberFactIDs []string `json:"member_fact_ids"`
	Verdict       string   `json:"verdict"`
}

// proposeSkillConventions is the agent-only capability-skill proposal phase
// (reached via `patterns verify --conventions`): it samples durable gotcha/
// convention/invariant facts, asks an agent to keep only the non-obvious ones and
// group related facts into capability skills, and stores accepted ones as deep
// dossiers (archetype "capability") under `convention:` ids. Egress-gated; cached
// by the sample fingerprint; never from refresh.
func proposeSkillConventions(ctx context.Context, db *sql.DB, brainDir, repoDir, agent, model, effort string, run distillAgentRunner, now time.Time) (dossierVerifyStats, error) {
	var stats dossierVerifyStats
	if err := rejectAgentForNoEgress(agent); err != nil {
		return stats, err
	}
	sample, factText, sampleIDs := sampleCapabilityFacts(db, brainDir)
	count := func() int {
		return corpusScalar(db, `SELECT COUNT(*) FROM deep_dossiers WHERE pattern_id LIKE 'convention:%'`)
	}
	if len(sample) == 0 {
		_, _ = db.Exec(`DELETE FROM deep_dossiers WHERE pattern_id LIKE 'convention:%'`)
		return stats, nil
	}
	// Fingerprint the actual evidence payload (fact id + kind + text + locus +
	// corroboration count), so editing a fact's text or its episode corroboration
	// invalidates the cache even when the set of fact ids is unchanged.
	payload, _ := json.Marshal(map[string]any{"facts": sample})
	sampleFP := knowledgeSampleFingerprint(payload)
	if corpusMeta(db, "conventions_sample_fingerprint") == sampleFP && count() > 0 {
		stats.Cached = count()
		stats.Considered = stats.Cached
		return stats, nil // unchanged sample → reuse prior proposals
	}

	args, err := distillAgentCommandArgs(agent, nil, conventionProposeSystemPrompt)
	if err != nil {
		return stats, err
	}
	args = injectAgentModel(args, agent, model)
	args = injectAgentEffort(args, agent, effort)

	out, err := run(ctx, repoDir, args, []byte(redactText(string(payload))), dossierVerifyTimeout)
	if err != nil {
		return stats, fmt.Errorf("convention proposal agent: %w", err)
	}
	proposed, err := parseProposedConventions(out)
	if err != nil {
		return stats, err
	}

	repoKey := corpusMeta(db, "repo_key")
	ts := now.UTC().Format(time.RFC3339)
	if _, err := db.Exec(`DELETE FROM deep_dossiers WHERE pattern_id LIKE 'convention:%'`); err != nil {
		return stats, err
	}
	valid := map[string]bool{}
	for _, id := range sampleIDs {
		valid[id] = true
	}
	for _, pc := range proposed {
		stats.Considered++
		members := uniqueStrings(filterValidKeys(pc.MemberFactIDs, valid))
		verdict := strings.ToLower(strings.TrimSpace(pc.Verdict))
		if !allowedVerdicts[verdict] {
			verdict = "low_confidence"
		}
		if len(members) == 0 || verdict == "rejected" {
			stats.Failed++
			continue
		}
		rec := conventionRecord(pc, members, factText, repoKey)
		// Retain episode corroboration anchors (where available): the episodes that
		// referenced these facts, so the dossier carries real source provenance.
		rec.SourceAnchors = conventionSourceAnchors(db, brainDir, members)
		rec.Fingerprint = knowledgeDossierFingerprint(rec, sampleFP)
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
	_ = setCorpusMeta(db, map[string]string{"conventions_sample_fingerprint": sampleFP})
	return stats, nil
}

// conventionRecord renders an accepted convention proposal into a
// capability-archetype deep dossier. Facts carries the member rule texts so the
// writer converts real knowledge, not the agent's paraphrase alone.
func conventionRecord(pc proposedConvention, members []string, factText map[string]string, repoKey string) deepDossierRecord {
	id := "convention:" + hexSHA("convention\x00"+repoKey+"\x00"+strings.Join(sortedCopy(members), "|"))
	var knowledge []string
	for _, k := range pc.Knowledge {
		if s := strings.TrimSpace(k); s != "" {
			knowledge = append(knowledge, truncateString(s, 280))
		}
	}
	var facts []string
	for _, m := range members {
		if txt := strings.TrimSpace(factText[m]); txt != "" {
			facts = append(facts, truncateString(txt, 280))
		}
	}
	return deepDossierRecord{
		SchemaVersion:    deepSchemaVersion,
		PatternID:        id,
		Archetype:        "capability",
		Fingerprint:      "sha256:" + hexSHA("convention\x00"+strings.Join(sortedCopy(members), "|")),
		Title:            truncateString(strings.TrimSpace(firstNonEmpty(pc.Title, "convention")), 120),
		Trigger:          truncateString(strings.TrimSpace(pc.Trigger), 280),
		NotWhen:          truncateString(strings.TrimSpace(pc.NotWhen), 280),
		Knowledge:        knowledge,
		Facts:            facts,
		EvidenceEpisodes: len(members),
	}
}

// conventionSourceAnchors resolves the episodes that referenced the member facts
// (via episode_facts) into source anchors with redacted excerpts where available
// — the episode corroboration provenance for the convention.
func conventionSourceAnchors(db *sql.DB, brainDir string, factIDs []string) []dossierAnchor {
	if len(factIDs) == 0 {
		return nil
	}
	ph, args := inPlaceholders(factIDs)
	rows, err := db.Query(`SELECT DISTINCT e.session_id, e.source_path, e.start_line, e.end_line, e.outcome
		FROM episode_facts ef JOIN episodes e ON e.id = ef.episode_id
		WHERE ef.fact_id IN (`+ph+`) ORDER BY e.session_id LIMIT 12`, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var anchors []dossierAnchor
	for rows.Next() {
		var session, path, outcome string
		var start, end int
		if rows.Scan(&session, &path, &start, &end, &outcome) != nil {
			continue
		}
		a := dossierAnchor{SessionID: session, Transcript: redactText(path), StartLine: start, EndLine: end, Outcome: outcome}
		lines := end - start + 1
		if lines <= 0 || lines > themeMemberExcerptLines {
			lines = themeMemberExcerptLines
		}
		if t := transcriptExcerpt(brainDir, episodeAnchor{Path: path, Line: start}, lines, themeMemberExcerptBytes); t != "" {
			a.Excerpt = redactText(t)
		}
		anchors = append(anchors, a)
	}
	return anchors
}

func parseProposedConventions(out string) ([]proposedConvention, error) {
	raw := strings.TrimSpace(out)
	if i := strings.IndexByte(raw, '{'); i >= 0 {
		if j := strings.LastIndexByte(raw, '}'); j >= i {
			raw = raw[i : j+1]
		}
	}
	var wrap struct {
		Conventions []proposedConvention `json:"conventions"`
	}
	if err := json.Unmarshal([]byte(raw), &wrap); err != nil {
		return nil, fmt.Errorf("parse convention proposal: %w", err)
	}
	return wrap.Conventions, nil
}

// sampleCapabilityFacts returns up to conventionSampleCap active gotcha/
// convention/invariant facts as {fact_id, kind, text, locus, episodes}
// (redacted), the fact_id→text map, and the sampled ids. Facts corroborated by
// more episodes rank first. Branch-scoped facts never cross branches because each
// fact carries its own branch and is sampled as-is.
func sampleCapabilityFacts(db *sql.DB, brainDir string) ([]map[string]string, map[string]string, []string) {
	byBranch, err := loadAllFactBranches(brainDir)
	if err != nil {
		return nil, nil, nil
	}
	// Episode corroboration: how many episodes reference each fact_id.
	episodeCount := map[string]int{}
	if rows, err := db.Query(`SELECT fact_id, COUNT(*) FROM episode_facts GROUP BY fact_id`); err == nil {
		for rows.Next() {
			var id string
			var n int
			if rows.Scan(&id, &n) == nil {
				episodeCount[id] = n
			}
		}
		rows.Close()
	}

	type factEntry struct {
		f     factRecord
		count int
	}
	var entries []factEntry
	seen := map[string]bool{}
	for _, recs := range byBranch {
		for _, f := range recs {
			if f.Status != "active" || !capabilityFactKinds[f.Kind] || seen[f.ID] {
				continue
			}
			seen[f.ID] = true
			entries = append(entries, factEntry{f: f, count: episodeCount[f.ID]})
		}
	}
	// Most-corroborated first, then stable by id.
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].count != entries[j].count {
			return entries[i].count > entries[j].count
		}
		return entries[i].f.ID < entries[j].f.ID
	})

	var sample []map[string]string
	var ids []string
	factText := map[string]string{}
	for _, e := range entries {
		if len(sample) >= conventionSampleCap {
			break
		}
		// Retain fact provenance: the rule text plus its code locus.
		prov := redactText(e.f.Text)
		if locus := strings.Join(e.f.Locus, ", "); strings.TrimSpace(locus) != "" {
			prov += " @ " + redactText(locus)
		}
		factText[e.f.ID] = prov
		sample = append(sample, map[string]string{
			"fact_id":  e.f.ID,
			"kind":     e.f.Kind,
			"text":     redactText(truncateString(e.f.Text, 280)),
			"locus":    redactText(strings.Join(e.f.Locus, ", ")),
			"episodes": fmt.Sprintf("%d", e.count),
		})
		ids = append(ids, e.f.ID)
	}
	return sample, factText, ids
}
